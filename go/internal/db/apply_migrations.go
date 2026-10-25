package db

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The migration replay, ported from src/db/apply-migrations.ts. The drizzle
// migrations of the TS repository are embedded verbatim (migrations/*.sql plus
// migrations/meta/_journal.json are byte-identical copies of drizzle/ at port
// time), so an empty database built here is indistinguishable from one the TS
// daemon migrated, and an already-migrated production database is reused
// untouched.

//go:embed all:migrations
var migrationsFS embed.FS

// migrationsTable mirrors the `__drizzle_migrations` bookkeeping table drizzle
// keeps: the applied migrations' content hashes and folder timestamps.
const migrationsTable = "__drizzle_migrations"

// createMigrationsTableSQL mirrors the DDL apply-migrations.ts runs before
// replaying.
const createMigrationsTableSQL = "create table if not exists \"" + migrationsTable + "\" (\n" +
	"  id integer primary key autoincrement not null,\n" +
	"  hash text not null,\n" +
	"  created_at integer not null\n" +
	")"

// statementBreakpoint is drizzle's statement separator inside a .sql file.
const statementBreakpoint = "--> statement-breakpoint"

// migrationJournal mirrors drizzle/meta/_journal.json.
type migrationJournal struct {
	Entries []struct {
		Tag  string `json:"tag"`
		When int64  `json:"when"`
	} `json:"entries"`
}

// migrationFile mirrors one entry of drizzle's readMigrationFiles result.
type migrationFile struct {
	tag          string
	folderMillis int64
	hash         string
	statements   []string
}

// readMigrationFiles mirrors drizzle-orm's readMigrationFiles: the journal
// decides the order and timestamps, each tag's .sql file is split on the
// statement breakpoint, and the hash is the sha256 of the file's bytes (the
// value drizzle records; nothing reads it back, the timestamps drive the
// high-water skip).
func readMigrationFiles() ([]migrationFile, error) {
	journalBytes, err := migrationsFS.ReadFile("migrations/meta/_journal.json")
	if err != nil {
		return nil, err
	}
	var journal migrationJournal
	if err := json.Unmarshal(journalBytes, &journal); err != nil {
		return nil, err
	}
	files := make([]migrationFile, 0, len(journal.Entries))
	for _, entry := range journal.Entries {
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + entry.Tag + ".sql")
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(sqlBytes)
		statements := []string{}
		for _, statement := range strings.Split(string(sqlBytes), statementBreakpoint) {
			if trimmed := strings.TrimSpace(statement); trimmed != "" {
				statements = append(statements, trimmed)
			}
		}
		files = append(files, migrationFile{
			tag:          entry.Tag,
			folderMillis: entry.When,
			hash:         hex.EncodeToString(sum[:]),
			statements:   statements,
		})
	}
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].folderMillis < files[j].folderMillis
	})
	return files, nil
}

// ApplyMigrations mirrors applyMigrations: it creates the bookkeeping table,
// then replays every embedded migration whose folder timestamp is newer than
// the newest recorded one, each in its own transaction. A database the TS
// daemon already migrated has all timestamps recorded, so this is a no-op on
// it — the existing tables are reused as they are.
func ApplyMigrations(db *sql.DB) error {
	files, err := readMigrationFiles()
	if err != nil {
		return err
	}
	if _, err := db.Exec(createMigrationsTableSQL); err != nil {
		return err
	}
	var lastCreatedAt sql.NullInt64
	if err := db.QueryRow("select created_at from \"" + migrationsTable +
		"\" order by created_at desc limit 1").Scan(&lastCreatedAt); err != nil && err != sql.ErrNoRows {
		return err
	}
	for _, file := range files {
		if lastCreatedAt.Valid && lastCreatedAt.Int64 >= file.folderMillis {
			continue
		}
		if err := applyMigration(db, file); err != nil {
			return fmt.Errorf("apply migration %s: %w", file.tag, err)
		}
	}
	return nil
}

// applyMigration replays one migration file inside a single transaction and
// records it, mirroring the TS begin/commit with rollback on failure.
func applyMigration(db *sql.DB, file migrationFile) error {
	if _, err := db.Exec("begin"); err != nil {
		return err
	}
	for _, statement := range file.statements {
		if _, err := db.Exec(statement); err != nil {
			_, _ = db.Exec("rollback")
			return err
		}
	}
	if _, err := db.Exec("insert into \""+migrationsTable+"\" (hash, created_at) values (?, ?)",
		file.hash, file.folderMillis); err != nil {
		_, _ = db.Exec("rollback")
		return err
	}
	if _, err := db.Exec("commit"); err != nil {
		return err
	}
	return nil
}
