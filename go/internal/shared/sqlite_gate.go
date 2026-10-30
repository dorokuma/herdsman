package shared

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sqliteGateWaitTimeout is the default ceiling for one round of Acquire's timed
// wait: a waiter wakes at least this often to recheck the gate and, on every
// expiry, reports the owner it is still stuck behind to stderr (journald on the
// production daemon). The wait is observability, not a real timeout: Acquire's
// contract — queue until the gate is free, with no error to return — must not
// change for the serialized connection and transaction callers, so expiring
// logs and goes back to waiting instead of failing.
//
// Production hit exactly the failure this warns about: a gate owner never
// released, every RPC handler queued on cond.Wait forever, and 77+ minutes
// passed with no diagnostic that named the culprit. A naked sync.Cond cannot
// express even that bound — Wait has no timeout — so the gate wakes waiters
// through a one-slot channel instead, which a Timer can interrupt.
const sqliteGateWaitTimeout = 30 * time.Second

// sqliteGateWaitTimeoutEnv tunes the report interval above: the value is a
// number of milliseconds between "still waiting" reports, so a deployment
// whose transactions legitimately run long can raise it instead of letting
// the gate fill the journal. An unset, unparsable, or non-positive value
// falls back to sqliteGateWaitTimeout. It only changes how often the gate
// reports — the wait never turns into a failure either way.
const sqliteGateWaitTimeoutEnv = "HERDSMAN_SQLITE_GATE_WARN_MS"

// SqliteGate is the database gate: it serialises access to the one SQLite
// connection every statement of a process shares, and it is reentrant per
// goroutine so a transaction can run its own statements while it holds the gate.
//
// Why a gate exists at all: the TS original holds a single node:sqlite
// DatabaseSync connection and its event loop makes every statement and every
// `begin immediate ... commit` block atomic. Go has no such loop, so
// `database/sql` alone cannot express the invariant that matters for SQLite:
// a transaction is not just a serialised statement, it is a *window* during
// which no other goroutine may start its own transaction (a second
// `begin immediate` fails with "cannot start a transaction within a
// transaction") and no other goroutine's write may join the open transaction
// (so a rollback cannot discard somebody else's committed-looking work).
//
// The gate closes that window: a goroutine acquires it, and every other
// goroutine — transaction or plain statement — queues until it releases. Depth
// counting makes it reentrant, so the statements a transaction callback runs
// through the same handle re-acquire without deadlocking.
type SqliteGate struct {
	mu    sync.Mutex
	owner int64
	depth int
	// acquiredAt is when the current owner first took the gate; waiters put it
	// in their timeout report so the log ties a stuck queue to the whole hold,
	// not just to the owner's latest reentrant acquire.
	acquiredAt time.Time

	// wake is the single channel the queue waits on. Release drops one signal
	// into its one-slot buffer, the fastest waiter to consume it wins the gate,
	// and every other waiter re-queues for the next Release. A stale signal is
	// harmless: consuming it only buys an extra ownership recheck, never a
	// grant — the mutex alone decides who owns the gate.
	wake chan struct{}

	// warnf is where the timeout reports go; tests swap it to capture them.
	warnf func(format string, args ...any)
	// waitTimeout overrides the timed-wait ceiling; <= 0 falls back to
	// sqliteGateWaitTimeout. Tests inject a short value so the warn path runs
	// without a 30-second pause.
	waitTimeout time.Duration
}

// NewSqliteGate returns an unlocked gate.
func NewSqliteGate() *SqliteGate {
	return &SqliteGate{
		// The env override lands in the same field tests inject; it must apply
		// to every gate a process opens, so it is resolved per gate here rather
		// than memoised once at package load.
		waitTimeout: sqliteGateWaitTimeoutFromEnv(),
		warnf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "SqliteGate: "+format+"\n", args...)
		},
	}
}

// Acquire takes the gate for the calling goroutine. A goroutine that already
// holds it only bumps the depth, so a statement run from inside a transaction
// callback re-enters instead of deadlocking against itself.
//
// The queue waits on the gate's single wake channel instead of sync.Cond so a
// stuck owner cannot leave a waiter asleep forever with no trace: when the
// timed round expires the waiter logs which gid owns the gate and how long it
// has held it, then starts the next round. Nothing is returned and nothing
// gives up — the call contract is unchanged.
func (g *SqliteGate) Acquire(gid int64) {
	g.mu.Lock()
	// gid 0 is the unparsable fallback of goroutineID, and it is also the free
	// gate's owner value — comparing a 0 gid against the owner would therefore
	// let every unparsable goroutine share one hold. A gid of 0 never re-enters:
	// it degrades to a plain, non-reentrant mutex — stricter, never looser.
	if g.depth == 0 || (gid != 0 && g.owner == gid) {
		// acquiredAt stamps only the first level of a hold: a reentrant
		// re-acquire must not restart the clock, or a timeout report would
		// understate how long the gate has actually been owned.
		if g.depth == 0 {
			g.acquiredAt = time.Now()
		}
		g.owner = gid
		g.depth++
		g.mu.Unlock()
		return
	}
	// Grab the wake channel before unlocking so a Release cannot broadcast
	// between the check above and the wait: the channel exists and this
	// goroutine's select will listen on it before the lock is dropped, exactly
	// like sync.Cond requires Wait to run under the lock.
	if g.wake == nil {
		g.wake = make(chan struct{}, 1)
	}
	wake := g.wake
	g.mu.Unlock()

	for {
		timer := time.NewTimer(g.waitTimeoutOrDefault())
		woke := true
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
			woke = false
		}
		g.mu.Lock()
		if g.depth == 0 || (gid != 0 && g.owner == gid) {
			if g.depth == 0 {
				g.acquiredAt = time.Now()
			}
			g.owner = gid
			g.depth++
			g.mu.Unlock()
			return
		}
		if !woke {
			// The round timed out: report the owner it is stuck behind, then
			// queue again. One report per round keeps a permanently stuck gate
			// visible at one line per waitTimeout instead of flooding.
			heldFor := time.Since(g.acquiredAt).Round(time.Second)
			warnf := g.warnf
			owner := g.owner
			waitTimeout := g.waitTimeoutOrDefault()
			g.mu.Unlock()
			if warnf != nil {
				warnf("acquire still waiting after %v: gate held by goroutine %d for %v (waiter goroutine %d)",
					waitTimeout, owner, heldFor, gid)
			}
			continue
		}
		// A signal woke this waiter but the gate is still owned — either the
		// owner re-entered or a faster waiter won it. The wake is spent; queue
		// for the next Release.
		g.mu.Unlock()
	}
}

// sqliteGateWaitTimeoutFromEnv resolves the HERDSMAN_SQLITE_GATE_WARN_MS
// override; a non-positive or unparsable value returns 0 so the gate falls
// back to its default.
func sqliteGateWaitTimeoutFromEnv() time.Duration {
	ms, err := strconv.Atoi(strings.TrimSpace(os.Getenv(sqliteGateWaitTimeoutEnv)))
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// waitTimeoutOrDefault resolves the ceiling a timed wait round runs on.
func (g *SqliteGate) waitTimeoutOrDefault() time.Duration {
	if g.waitTimeout > 0 {
		return g.waitTimeout
	}
	return sqliteGateWaitTimeout
}

// Release drops one level of the gate and hands it to the next waiter when the
// calling goroutine's last level goes away.
func (g *SqliteGate) Release(gid int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.depth == 0 || g.owner != gid {
		return
	}
	g.depth--
	if g.depth == 0 {
		g.owner = 0
		g.broadcastLocked()
	}
}

// ReleaseAll drops every level the calling goroutine owns in one shot. The
// normal lifecycle never needs it — every serializedConn holder releases its
// own level — but a panic unwinding through RunSqliteTransaction can skip the
// Close of a rows or statement that is still holding a level, and without a
// way to settle those leftovers the defer that releases the transaction's own
// level pops only one: depth never reaches zero, the owner stays pinned, and
// the gate deadlocks exactly like the never-released owner this type exists to
// diagnose. RunSqliteTransaction's recover calls it so the unwinding goroutine
// hands the gate back whole.
//
// Like Release, a call from a goroutine that owns nothing — or a gid of 0,
// which never re-enters — is a no-op, and it only ever clears the caller's own
// levels, so it cannot tear down another goroutine's hold.
func (g *SqliteGate) ReleaseAll(gid int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.depth == 0 || g.owner != gid {
		return
	}
	g.depth = 0
	g.owner = 0
	g.broadcastLocked()
}

// broadcastLocked wakes the queue. Exactly one waiter may proceed — the gate
// hands out one hold — so the wake is a single buffered signal, and every
// waiter that observes it rechecks ownership under the mutex: the fastest one
// wins the owner slot and the rest re-queue for the next Release. A signal
// already parked in the buffer is just as good as a fresh one, so a full slot
// skips the send. Callers hold g.mu.
func (g *SqliteGate) broadcastLocked() {
	if g.wake == nil {
		return
	}
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// HeldBy reports whether gid currently holds the gate. It exists for the tests
// that assert the release path actually ran. A gid of 0 identifies no
// goroutine (it is goroutineID's unparsable fallback), so it is never reported
// as a holder even when an unparsable goroutine owns the gate.
func (g *SqliteGate) HeldBy(gid int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.depth > 0 && gid != 0 && g.owner == gid
}

// gates is the registry mapping an open *sql.DB to the gate that serialises it.
// The gate lives outside the handle because internal/observability cannot
// import internal/db yet runs the same multi-statement transactions, so both
// layers have to find one gate per database.
var gates = struct {
	mu   sync.Mutex
	byDB map[*sql.DB]*SqliteGate
}{byDB: map[*sql.DB]*SqliteGate{}}

// RegisterSqliteGate associates db with gate so RunSqliteTransaction and the
// driver wrapper find the same lock. Called by OpenSqlite.
func RegisterSqliteGate(db *sql.DB, gate *SqliteGate) {
	if db == nil || gate == nil {
		return
	}
	gates.mu.Lock()
	gates.byDB[db] = gate
	gates.mu.Unlock()
}

// UnregisterSqliteGate drops db from the registry. Called when the handle is
// closed, so a long-running process whose daemons open and close databases does
// not keep a gate (and the *sql.DB key itself) alive for every database it has
// ever opened.
func UnregisterSqliteGate(db *sql.DB) {
	if db == nil {
		return
	}
	gates.mu.Lock()
	delete(gates.byDB, db)
	gates.mu.Unlock()
}

// SqliteGateOf returns the gate registered for db, or nil when the handle was
// not opened through OpenSqlite (a caller-supplied *sql.DB in a test, say).
func SqliteGateOf(db *sql.DB) *SqliteGate {
	if db == nil {
		return nil
	}
	gates.mu.Lock()
	defer gates.mu.Unlock()
	return gates.byDB[db]
}

// goroutineID parses the current goroutine's id out of runtime.Stack. Go has no
// supported API for this, and the gate needs it: reentrancy must be keyed on the
// goroutine, not on the call stack, because a transaction callback runs
// statements through a different stack than the one that opened it.
//
// The format is stable across supported releases ("goroutine 123 [running]:");
// an unparsable header falls back to 0, which still serialises correctly — it
// just loses reentrancy for that one goroutine instead of corrupting the gate.
func goroutineID() int64 {
	var buf [64]byte
	stack := buf[:runtime.Stack(buf[:], false)]
	stack = bytes.TrimPrefix(stack, []byte("goroutine "))
	if index := bytes.IndexByte(stack, ' '); index >= 0 {
		stack = stack[:index]
	}
	if id, err := strconv.ParseInt(string(stack), 10, 64); err == nil {
		return id
	}
	return 0
}

// CurrentGoroutineID exposes the parsed id to the packages that wrap the gate.
func CurrentGoroutineID() int64 { return goroutineID() }
