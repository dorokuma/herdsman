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
// DatabaseSync connection. That only holds because the transaction holds the
// database gate for its whole window: every other goroutine's statement and
// transaction queues on the same gate, so nobody can open a second
// `begin immediate` inside this one, and nobody's write can land in this
// transaction and be discarded by its rollback.
func RunSqliteTransaction(db *sql.DB, fn func() error) error {
	ctx := context.Background()
	gate := SqliteGateOf(db)
	if gate != nil {
		gid := goroutineID()
		gate.Acquire(gid)
		defer gate.Release(gid)
	}

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
		// A failed commit leaves the transaction open — SQLite keeps it for a
		// retry, a deferred foreign-key violation is the usual cause. Without an
		// explicit rollback this goroutine releases the gate with a transaction
		// still attached to the connection, and the next caller's `begin
		// immediate` hits "cannot start a transaction within a transaction".
		_, _ = db.ExecContext(ctx, "rollback")
		return err
	}
	return nil
}
