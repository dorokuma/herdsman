package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// The regression tests for the database gate: the TS side shares one
// DatabaseSync connection with an event loop, so a `begin immediate ... commit`
// block is atomic against every other statement. Go has no event loop, so
// without the gate two goroutines interleave `BEGIN` ("cannot start a
// transaction within a transaction") and a plain write can join somebody else's
// open transaction and be discarded by its rollback.

// insertTestSession registers the Herdr session the workspace rows hang off.
func insertTestSession(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(
		"insert into herdr_sessions (name, running, session_dir, socket_path, updated_at) " +
			"values ('session', 1, '/tmp/session', '/tmp/session.sock', 1)",
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

// insertTestWorkspace writes one workspace row of the test session.
func insertTestWorkspace(db *sql.DB, workspace string) error {
	_, err := db.Exec(
		"insert into herdr_workspaces "+
			"(herdr_session_name, workspace_id, focused, agent_status, last_seen_at) "+
			"values ('session', ?, 0, 'active', 1)",
		workspace,
	)
	return err
}

func TestRunSqliteTransactionConcurrentDoesNotInterleave(t *testing.T) {
	db := openMigratedTestDB(t)
	insertTestSession(t, db)

	const goroutines = 8
	var wait sync.WaitGroup
	errs := make([]error, goroutines)
	wait.Add(goroutines)
	for index := 0; index < goroutines; index++ {
		go func(index int) {
			defer wait.Done()
			errs[index] = RunSqliteTransaction(db, func() error {
				// Two statements plus a yield: a transaction that does not hold the
				// gate across fn would let the next goroutine's BEGIN in here.
				if err := insertTestWorkspace(db, fmt.Sprintf("ws-%d", index)); err != nil {
					return err
				}
				time.Sleep(5 * time.Millisecond)
				return insertTestWorkspace(db, fmt.Sprintf("ws-%d-b", index))
			})
		}(index)
	}
	wait.Wait()

	for index, err := range errs {
		if err == nil {
			continue
		}
		t.Fatalf("goroutine %d transaction failed: %v", index, err)
	}

	var workspaces int
	if err := db.QueryRow("select count(*) from herdr_workspaces").Scan(&workspaces); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	if want := goroutines * 2; workspaces != want {
		t.Fatalf("committed workspaces = %d, want %d", workspaces, want)
	}
}

func TestRunSqliteTransactionRollbackDoesNotDiscardOtherGoroutineWrite(t *testing.T) {
	db := openMigratedTestDB(t)
	insertTestSession(t, db)

	var wait sync.WaitGroup
	started := make(chan struct{})
	otherDone := make(chan error, 1)

	wait.Add(1)
	go func() {
		defer wait.Done()
		// The transaction writes, then fails once the other goroutine has had
		// time to try its own write, so its rollback covers the window the
		// plain statement would otherwise slip into.
		_ = RunSqliteTransaction(db, func() error {
			if err := insertTestWorkspace(db, "rolled-back"); err != nil {
				return err
			}
			close(started)
			time.Sleep(20 * time.Millisecond)
			return errors.New("rollback on purpose")
		})
	}()

	<-started
	go func() {
		otherDone <- insertTestWorkspace(db, "survivor")
	}()

	wait.Wait()
	if err := <-otherDone; err != nil {
		t.Fatalf("concurrent plain write failed: %v", err)
	}

	var survivor, rolledBack int
	if err := db.QueryRow(
		"select count(*) from herdr_workspaces where workspace_id = 'survivor'",
	).Scan(&survivor); err != nil {
		t.Fatalf("count survivor: %v", err)
	}
	if err := db.QueryRow(
		"select count(*) from herdr_workspaces where workspace_id = 'rolled-back'",
	).Scan(&rolledBack); err != nil {
		t.Fatalf("count rolled-back: %v", err)
	}
	if survivor != 1 {
		t.Fatalf("survivor workspace count = %d, want 1 (the rollback discarded it)", survivor)
	}
	if rolledBack != 0 {
		t.Fatalf("rolled-back workspace count = %d, want 0", rolledBack)
	}
}
