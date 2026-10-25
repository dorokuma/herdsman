// Package db ports the SQLite layer of src/db: connection management, the table
// schema and its migrations, and the per-table stores. The driver is the pure
// Go modernc.org/sqlite, so the port stays CGO_ENABLED=0 clean.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: the port stays CGO_ENABLED=0 clean
)

// OpenSqlite mirrors openSqlite of src/db/client.ts: it opens (creating the
// file when missing) the database at path and applies the connection pragmas
// the TS side runs — WAL journalling, a 5s busy timeout, and foreign key
// enforcement (node:sqlite's DatabaseSync defaults enableForeignKeyConstraints
// to true, so the TS daemon effectively runs with foreign keys on).
//
// The handle is a *sql.DB pinned to a single connection, mirroring the one
// DatabaseSync connection the TS side shares: every statement and transaction
// serializes on it and the pragmas stay in effect for all of them.
func OpenSqlite(path string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	ctx := context.Background()
	// journal_mode is best effort, exactly like the TS try/catch: a database
	// that cannot switch to WAL (an in-memory database, for instance) still
	// opens and runs, so the failure is reported but not fatal.
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to enable SQLite WAL mode; continuing startup: %v\n", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// RunSqliteTransaction mirrors runSqliteTransaction: fn runs inside a
// `begin immediate` transaction on the shared connection; a failing fn rolls
// the transaction back and its error is returned to the caller.
func RunSqliteTransaction(db *sql.DB, fn func() error) error {
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "begin immediate"); err != nil {
		return err
	}
	if err := fn(); err != nil {
		// A rollback failure must not mask the original error, so it is
		// ignored here — matching the TS control flow, where the caught error
		// is rethrown after the rollback attempt.
		_, _ = db.ExecContext(ctx, "rollback")
		return err
	}
	if _, err := db.ExecContext(ctx, "commit"); err != nil {
		return err
	}
	return nil
}

// isoTimestampLayout is JavaScript's Date#toISOString millisecond layout, which
// the TS contracts' Date fields serialize to.
const isoTimestampLayout = "2006-01-02T15:04:05.000Z"

// formatTimestamp mirrors `new Date(ms).toISOString()` for the record fields
// the TS contracts type as Date.
func formatTimestamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(isoTimestampLayout)
}

// nullableTimestamp mirrors `value === null ? null : new Date(value)`: an absent
// column stays absent instead of collapsing to the epoch.
func nullableTimestamp(value sql.NullInt64) *string {
	if !value.Valid {
		return nil
	}
	formatted := formatTimestamp(value.Int64)
	return &formatted
}
