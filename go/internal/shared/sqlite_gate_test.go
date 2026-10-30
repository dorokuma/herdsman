package shared

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSqliteGateAcquireWarnsOnTimeoutThenStillAcquires is the regression for
// the daemon lockout the timed wait exists to diagnose: when a gate owner
// never released, every queued goroutine slept on cond.Wait with no trace of
// which goroutine it was stuck behind.
//
// The assertions pin the two halves of the contract: an expired timed round
// reports the owner gid and the hold duration through warnf, and the waiter
// still acquires the gate once the owner releases — the timeout is a
// diagnostic, not a failure, so Acquire's no-error contract is unchanged.
func TestSqliteGateAcquireWarnsOnTimeoutThenStillAcquires(t *testing.T) {
	gate := NewSqliteGate()
	// The injected timeout keeps the regression at milliseconds; the warnf
	// swap captures what production writes to stderr.
	gate.waitTimeout = 20 * time.Millisecond
	warns := make(chan string, 8)
	gate.warnf = func(format string, args ...any) {
		warns <- fmt.Sprintf(format, args...)
	}

	holderReady := make(chan int64, 1)
	holderRelease := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		gid := goroutineID()
		gate.Acquire(gid)
		defer gate.Release(gid)
		holderReady <- gid
		<-holderRelease
	}()
	holderGid := <-holderReady

	waiterGid := make(chan int64, 1)
	acquired := make(chan struct{})
	go func() {
		gid := goroutineID()
		waiterGid <- gid
		gate.Acquire(gid)
		close(acquired)
		gate.Release(gid)
	}()
	gid := <-waiterGid

	// The waiter must surface the owner it is parked behind. The report names
	// the holder by gid — the production failure's missing piece — so matching
	// the gid, not just the prefix, is the assertion.
	var report string
	select {
	case report = <-warns:
	case <-time.After(5 * time.Second):
		t.Fatal("no timeout report within 5s, want one per waitTimeout round")
	}
	if !strings.Contains(report, fmt.Sprintf("goroutine %d", holderGid)) {
		t.Fatalf("timeout report %q does not name the holder gid %d", report, holderGid)
	}
	if !strings.Contains(report, fmt.Sprintf("goroutine %d", gid)) {
		t.Fatalf("timeout report %q does not name the waiter gid %d", report, gid)
	}

	// Acquiring while the owner still holds must not have succeeded on the
	// timeout: the report is observability, not a grant.
	select {
	case <-acquired:
		t.Fatal("Acquire returned while the gate was still owned")
	default:
	}

	close(holderRelease)
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not return after the owner released")
	}
	<-holderDone
}

// TestSqliteGateWarnReportsHoldFromFirstAcquire pins the heldFor field of the
// timeout report: a reentrant Acquire must not restart the hold clock. The
// owner here holds the gate for over a second, re-enters and releases one
// level right before the waiter's round expires — if acquiredAt were stamped
// on every acquire instead of only the first, the report would round the hold
// down to the milliseconds since that re-entry and the stuck-owner diagnostic
// would understate a long hold by exactly the pre-expiry window.
func TestSqliteGateWarnReportsHoldFromFirstAcquire(t *testing.T) {
	gate := NewSqliteGate()
	gate.waitTimeout = 10 * time.Millisecond
	warns := make(chan string, 8)
	gate.warnf = func(format string, args ...any) {
		warns <- fmt.Sprintf(format, args...)
	}

	holderReady := make(chan int64, 1)
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		gid := goroutineID()
		gate.Acquire(gid)
		defer gate.Release(gid)
		// Hold past one second so the correct heldFor rounds to at least "1s",
		// then re-enter and release: the buggy stamp-every-acquire version would
		// report only the few milliseconds since this re-entry.
		time.Sleep(time.Second)
		gate.Acquire(gid)
		gate.Release(gid)
		holderReady <- gid
		// Keep the gate long enough for the waiter's first timed round to
		// expire and produce its report.
		time.Sleep(100 * time.Millisecond)
	}()
	holderGid := <-holderReady

	acquired := make(chan struct{})
	go func() {
		gid := goroutineID()
		gate.Acquire(gid)
		close(acquired)
		gate.Release(gid)
	}()

	var report string
	select {
	case report = <-warns:
	case <-time.After(5 * time.Second):
		t.Fatal("no timeout report within 5s, want one per waitTimeout round")
	}
	if !strings.Contains(report, fmt.Sprintf("goroutine %d for ", holderGid)) {
		t.Fatalf("timeout report %q does not name the holder gid %d with a hold duration", report, holderGid)
	}
	if strings.Contains(report, "for 0s") {
		t.Fatalf("timeout report %q rounds the hold to 0s — acquiredAt was restamped by the reentrant acquire", report)
	}

	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire did not return after the owner released")
	}
	<-holderDone
}

// TestSqliteGateWaitTimeoutEnvOverride pins the HERDSMAN_SQLITE_GATE_WARN_MS
// override: a positive millisecond value becomes the report interval, and an
// unset, non-positive, or unparsable value falls back to the 30s default so a
// deployment cannot accidentally silence or flood the diagnostic.
func TestSqliteGateWaitTimeoutEnvOverride(t *testing.T) {
	t.Setenv("HERDSMAN_SQLITE_GATE_WARN_MS", "7")
	if got := NewSqliteGate().waitTimeoutOrDefault(); got != 7*time.Millisecond {
		t.Fatalf("waitTimeout with HERDSMAN_SQLITE_GATE_WARN_MS=7 = %v, want 7ms", got)
	}
	t.Setenv("HERDSMAN_SQLITE_GATE_WARN_MS", "0")
	if got := NewSqliteGate().waitTimeoutOrDefault(); got != sqliteGateWaitTimeout {
		t.Fatalf("waitTimeout with HERDSMAN_SQLITE_GATE_WARN_MS=0 = %v, want the %v default", got, sqliteGateWaitTimeout)
	}
	t.Setenv("HERDSMAN_SQLITE_GATE_WARN_MS", "not-a-number")
	if got := NewSqliteGate().waitTimeoutOrDefault(); got != sqliteGateWaitTimeout {
		t.Fatalf("waitTimeout with HERDSMAN_SQLITE_GATE_WARN_MS=not-a-number = %v, want the %v default", got, sqliteGateWaitTimeout)
	}
}
