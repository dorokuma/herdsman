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
		Tag     string `json:"tag"`
		Version string `json:"version"`
		When    int64  `json:"when"`
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
	return applyMigrationsThrough(db, "")
}

// ApplyMigrationsThrough mirrors ApplyMigrations up to and including the given
// tag, leaving the database in the state that migration produced. An empty tag
// means "every migration", which is what ApplyMigrations does. The
// data-backfill test migrates to an older schema first and then replays one
// migration by hand, exactly like the TS harness does with a pruned drizzle
// folder.
func ApplyMigrationsThrough(db *sql.DB, throughTag string) error {
	return applyMigrationsThrough(db, throughTag)
}

func applyMigrationsThrough(db *sql.DB, throughTag string) error {
	files, err := readMigrationFiles()
	if err != nil {
		return err
	}
	if throughTag != "" {
		upto := -1
		for index, file := range files {
			if file.tag == throughTag {
				upto = index
				break
			}
		}
		if upto < 0 {
			return fmt.Errorf("unknown migration tag: %s", throughTag)
		}
		files = files[:upto+1]
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

// MigrationJournalEntry mirrors one entry of drizzle's meta/_journal.json.
type MigrationJournalEntry struct {
	Tag     string
	Version string
	When    int64
}

// MigrationJournal reads the embedded journal: the ordered migration entries the
// replay follows.
func MigrationJournal() ([]MigrationJournalEntry, error) {
	journalBytes, err := migrationsFS.ReadFile("migrations/meta/_journal.json")
	if err != nil {
		return nil, err
	}
	var journal migrationJournal
	if err := json.Unmarshal(journalBytes, &journal); err != nil {
		return nil, err
	}
	entries := make([]MigrationJournalEntry, 0, len(journal.Entries))
	for _, entry := range journal.Entries {
		entries = append(entries, MigrationJournalEntry{
			Tag:     entry.Tag,
			Version: entry.Version,
			When:    entry.When,
		})
	}
	return entries, nil
}

// MigrationStatements returns the statements of one embedded migration file,
// split on drizzle's statement breakpoint. A data-backfill migration is replayed
// by hand by the tests that cover it in isolation.
func MigrationStatements(tag string) ([]string, error) {
	files, err := readMigrationFiles()
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if file.tag == tag {
			return file.statements, nil
		}
	}
	return nil, fmt.Errorf("unknown migration tag: %s", tag)
}

// applyMigration replays one migration file inside a single transaction and
// records it, mirroring the TS begin/commit with rollback on failure.
func applyMigration(db *sql.DB, file migrationFile) error {
	if _, err := db.Exec("begin"); err != nil {
		return err
	}
	// Same panic contract as RunSqliteTransaction: a panic unwinding past this
	// point would leave the open transaction attached to the one connection
	// every goroutine shares, so it is rolled back inside the unwind before
	// the panic continues. The statements above are db.Exec calls — the gate
	// levels they take are released per call — but the leftover-transaction
	// window is identical, so the repair is identical: a rollback failure must
	// not mask the panic, hence the ignored error.
	defer func() {
		if p := recover(); p != nil {
			_, _ = db.Exec("rollback")
			panic(p)
		}
	}()
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
