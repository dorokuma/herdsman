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
	var gid int64
	if gate != nil {
		gid = goroutineID()
		gate.Acquire(gid)
		defer gate.Release(gid)
	}

	if _, err := db.ExecContext(ctx, "begin immediate"); err != nil {
		return err
	}
	// The deferred recover is what makes a panic inside fn survivable for the
	// shared connection: without it a panicking callback would unwind past the
	// rollback, release the gate (its own defer still runs), and leave the open
	// transaction attached to the one connection every goroutine shares — the
	// next caller's `begin immediate` then fails with "cannot start a
	// transaction within a transaction", and a later rollback could discard
	// work a different transaction committed-looking. Rolling back first, then
	// re-panicking, restores the connection before the panic propagates.
	//
	// The recover runs inside the deferred function itself rather than naming
	// the panic: `defer func(){ recover() }()` keeps the panic propagating
	// while still letting the rollback run inside the unwind, which is exactly
	// the window where the connection state can still be repaired.
	defer func() {
		if p := recover(); p != nil {
			// This goroutine still owns the gate — the deferred Release above has
			// not run yet — so the rollback re-enters on its own hold and the
			// transaction window stays closed for its whole duration: no waiter
			// can slip its `begin immediate` in while the shared connection is
			// still inside this transaction, and no waiter's write can join it
			// and be discarded by this rollback. Same rule as the error path
			// below: a rollback failure must not mask the panic that is already
			// propagating, so it is ignored.
			_, _ = db.ExecContext(ctx, "rollback")
			// Only once the rollback has run are the gate levels the panic left
			// dangling settled: a Query or Prepare that returned its wrapped
			// rows/stmt to fn holds a level until that wrapper's Close runs, and a
			// panic unwinds past it. Releasing those orphans first would broadcast
			// the waiters awake while the transaction is still open — the window
			// this ordering exists to keep shut. The resources themselves stay
			// where the panic left them; only their gate levels are settled, and
			// whatever database/sql does with an abandoned *sql.Rows next cannot
			// push this goroutine's depth below zero — Release of a non-owner is a
			// no-op. Afterwards the deferred Release above is itself a no-op:
			// depth is already zero and the owner cleared.
			if gate != nil {
				gate.ReleaseAll(gid)
			}
			panic(p)
		}
	}()
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
