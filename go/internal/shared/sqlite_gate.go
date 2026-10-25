package shared

import (
	"bytes"
	"database/sql"
	"runtime"
	"strconv"
	"sync"
)

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
	cond  *sync.Cond
	owner int64
	depth int
}

// NewSqliteGate returns an unlocked gate.
func NewSqliteGate() *SqliteGate {
	gate := &SqliteGate{}
	gate.cond = sync.NewCond(&gate.mu)
	return gate
}

// Acquire takes the gate for the calling goroutine. A goroutine that already
// holds it only bumps the depth, so a statement run from inside a transaction
// callback re-enters instead of deadlocking against itself.
func (g *SqliteGate) Acquire(gid int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.depth > 0 && g.owner != gid {
		g.cond.Wait()
	}
	g.owner = gid
	g.depth++
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
		g.cond.Broadcast()
	}
}

// HeldBy reports whether gid currently holds the gate. It exists for the tests
// that assert the release path actually ran.
func (g *SqliteGate) HeldBy(gid int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.depth > 0 && g.owner == gid
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
