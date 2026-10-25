// Package shared holds the small helpers more than one internal package needs.
// It deliberately imports nothing from the rest of the module, so every layer
// (db, observability, daemon) can depend on it without creating a cycle.
package shared

import (
	"context"
	"database/sql"
)

// RunSqliteTransaction mirrors runSqliteTransaction of src/db/client.ts: fn runs
// inside a `begin immediate` transaction on the shared connection, and a failing
// fn rolls the transaction back and its error is returned to the caller.
//
// The statements fn executes go through the same *sql.DB, so they join the
// open transaction exactly like the TS callback, which reuses the one
// DatabaseSync connection.
func RunSqliteTransaction(db *sql.DB, fn func() error) error {
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "begin immediate"); err != nil {
		return err
	}
	if err := fn(); err != nil {
		// A rollback failure must not mask the original error, so it is ignored
		// here — matching the TS control flow, where the caught error is rethrown
		// after the rollback attempt.
		_, _ = db.ExecContext(ctx, "rollback")
		return err
	}
	if _, err := db.ExecContext(ctx, "commit"); err != nil {
		return err
	}
	return nil
}
