// Package db ports the SQLite layer of src/db: connection management, the table
// schema and its migrations, and the per-table stores. The driver is the pure
// Go modernc.org/sqlite, so the port stays CGO_ENABLED=0 clean.
package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"modernc.org/sqlite" // pure-Go driver: the port stays CGO_ENABLED=0 clean

	"github.com/dorokuma/herdsman/go/internal/shared"
)

// OpenSqlite mirrors openSqlite of src/db/client.ts: it opens (creating the
// file when missing) the database at path and applies the connection pragmas
// the TS side runs — WAL journalling, a 5s busy timeout, and foreign key
// enforcement (node:sqlite's DatabaseSync defaults enableForeignKeyConstraints
// to true, so the TS daemon effectively runs with foreign keys on).
//
// One real driver connection backs every handle, mirroring the single
// DatabaseSync connection the TS side shares, but the serialisation is the
// database gate rather than a one-slot pool: `database/sql` holding a
// transaction on its only connection would deadlock the statements that
// transaction runs (they would wait for the connection the transaction owns).
// Instead every Connect returns a lightweight serializedConn over the one
// underlying driver.Conn, and each of its Exec/Query/Prepare takes the gate
// first. See SqliteGate for why a transaction has to hold the gate across its
// whole window.
func OpenSqlite(path string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	underlying, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	conn, err := underlying.Connect(ctx)
	if err != nil {
		return nil, err
	}
	// The pragmas live on the one connection, exactly as they did on the single
	// pooled connection before, so they stay in effect for every statement.
	//
	// journal_mode is best effort, exactly like the TS try/catch: a database
	// that cannot switch to WAL (an in-memory database, for instance) still
	// opens and runs, so the failure is reported but not fatal.
	if err := execOnConn(ctx, conn, "PRAGMA journal_mode=WAL"); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to enable SQLite WAL mode; continuing startup: %v\n", err)
	}
	if err := execOnConn(ctx, conn, "PRAGMA busy_timeout=5000"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := execOnConn(ctx, conn, "PRAGMA foreign_keys=ON"); err != nil {
		_ = conn.Close()
		return nil, err
	}

	gate := shared.NewSqliteGate()
	connector := &serializedConnector{
		conn: conn,
		gate: gate,
	}
	db := sql.OpenDB(connector)
	// The pool no longer serialises anything, so it must not cap itself at one
	// connection: the gate hands out the one driver connection to one goroutine
	// at a time, and the extra handles are only wrappers waiting on it.
	connector.db = db
	shared.RegisterSqliteGate(db, gate)
	return db, nil
}

// execOnConn runs a pragma on the raw driver connection during startup, before
// any statement can be in flight.
func execOnConn(ctx context.Context, conn driver.Conn, statement string) error {
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		return errors.New("sqlite driver connection does not support ExecContext")
	}
	_, err := execer.ExecContext(ctx, statement, nil)
	return err
}

// serializedConnector hands database/sql a serializedConn for every Connect. The
// underlying driver connection is shared, never opened twice.
//
// It implements io.Closer, which database/sql calls automatically when the
// *sql.DB handle is closed: that is the one place the handle's lifecycle reaches
// the resources it owns, because the driver.Conn it hands out (serializedConn)
// must stay closable without tearing down the shared connection. Closing the
// handle therefore releases the single driver connection and drops the gate
// registration here, not in serializedConn.Close.
type serializedConnector struct {
	conn driver.Conn
	gate *shared.SqliteGate
	db   *sql.DB

	// mu guards closed: Connect reads it while Close writes it.
	mu     sync.Mutex
	closed bool
	once   sync.Once
}

func (c *serializedConnector) Connect(context.Context) (driver.Conn, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		// A Connect after database/sql already closed the handle cannot be
		// served: the shared driver connection is gone.
		return nil, sql.ErrConnDone
	}
	return &serializedConn{conn: c.conn, gate: c.gate}, nil
}

func (c *serializedConnector) Driver() driver.Driver { return serializedDriver{} }

// Close releases everything the open handle owned: the gate registration and the
// one driver connection every wrapper shares. database/sql calls it from
// (*sql.DB).Close, which is why a closed handle must not be used afterwards —
// Connect reports sql.ErrConnDone from then on.
//
// It is idempotent: (*sql.DB).Close may be called more than once (and a caller
// that never closed may still race a deferred cleanup), and closing a driver
// connection twice is an error the caller must not see.
func (c *serializedConnector) Close() error {
	var err error
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		shared.UnregisterSqliteGate(c.db)
		err = c.conn.Close()
	})
	return err
}

type serializedDriver struct{}

func (serializedDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("serialized sqlite driver is connector-only")
}

// serializedConn is the gate-aware wrapper around the one shared driver
// connection. Every operation that touches SQLite takes the gate for its whole
// duration; a query keeps it until its rows are closed, because the connection
// stays busy while rows are streaming.
type serializedConn struct {
	conn driver.Conn
	gate *shared.SqliteGate
}

func (c *serializedConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("serialized sqlite connection requires PrepareContext")
}

func (c *serializedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	gid := shared.CurrentGoroutineID()
	c.gate.Acquire(gid)
	stmt, err := c.prepareOnConn(ctx, query)
	if err != nil {
		c.gate.Release(gid)
		return nil, err
	}
	// The statement keeps its own gate level so a caller that prepares and then
	// closes (normalize/column metadata) does not race a reentrant Exec.
	return &serializedStmt{stmt: stmt, gate: c.gate, gid: gid, held: true}, nil
}

func (c *serializedConn) prepareOnConn(ctx context.Context, query string) (driver.Stmt, error) {
	if preparer, ok := c.conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.conn.Prepare(query)
}

func (c *serializedConn) Close() error {
	// The underlying connection is shared by every wrapper and is released by
	// serializedConnector.Close when the handle itself is closed, so closing one
	// pooled wrapper must not close it: sql.DB closing its last wrapper leaves
	// the driver connection alive for the handle.
	return nil
}

func (c *serializedConn) Begin() (driver.Tx, error) {
	return nil, errors.New("serialized sqlite connection requires BeginTx")
}

func (c *serializedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	gid := shared.CurrentGoroutineID()
	c.gate.Acquire(gid)
	beginnable, ok := c.conn.(driver.ConnBeginTx)
	if !ok {
		c.gate.Release(gid)
		return nil, errors.New("sqlite driver connection does not support BeginTx")
	}
	tx, err := beginnable.BeginTx(ctx, opts)
	if err != nil {
		c.gate.Release(gid)
		return nil, err
	}
	return &serializedTx{tx: tx, gate: c.gate, gid: gid}, nil
}

func (c *serializedConn) ExecContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Result, error) {
	gid := shared.CurrentGoroutineID()
	c.gate.Acquire(gid)
	defer c.gate.Release(gid)
	if execer, ok := c.conn.(driver.ExecerContext); ok {
		return execer.ExecContext(ctx, query, args)
	}
	stmt, err := c.prepareOnConn(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	return runStmt(ctx, stmt, args)
}

func (c *serializedConn) QueryContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	gid := shared.CurrentGoroutineID()
	c.gate.Acquire(gid)
	queryer, ok := c.conn.(driver.QueryerContext)
	if !ok {
		c.gate.Release(gid)
		return nil, errors.New("sqlite driver connection does not support QueryContext")
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil {
		c.gate.Release(gid)
		return nil, err
	}
	if rows == nil {
		c.gate.Release(gid)
		return nil, nil
	}
	// The gate is held until the rows are drained and closed: the connection is
	// busy while rows stream, so releasing earlier would let another goroutine
	// step on the open cursor.
	return &serializedRows{rows: rows, gate: c.gate, gid: gid}, nil
}

// runStmt executes a prepared statement with the arguments database/sql passes.
func runStmt(ctx context.Context, stmt driver.Stmt, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := stmt.(driver.StmtExecContext)
	if !ok {
		return nil, errors.New("sqlite statement does not support StmtExecContext")
	}
	return execer.ExecContext(ctx, args)
}

// serializedStmt is a prepared statement holding one gate level from Prepare
// until Close, so the prepare/exec pair cannot interleave with another
// goroutine between them.
type serializedStmt struct {
	stmt driver.Stmt
	gate *shared.SqliteGate
	gid  int64
	held bool
}

func (s *serializedStmt) Close() error {
	defer s.release()
	return s.stmt.Close()
}

func (s *serializedStmt) release() {
	if s.held {
		s.held = false
		s.gate.Release(s.gid)
	}
}

func (s *serializedStmt) NumInput() int { return s.stmt.NumInput() }

func (s *serializedStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.stmt.Exec(args)
}

func (s *serializedStmt) Query(args []driver.Value) (driver.Rows, error) {
	rows, err := s.stmt.Query(args)
	if err != nil || rows == nil {
		return rows, err
	}
	// The gate level moves to the rows so it survives until they are closed.
	s.held = false
	return &serializedRows{rows: rows, gate: s.gate, gid: s.gid}, nil
}

func (s *serializedStmt) ExecContext(
	ctx context.Context,
	args []driver.NamedValue,
) (driver.Result, error) {
	execer, ok := s.stmt.(driver.StmtExecContext)
	if !ok {
		return runStmt(ctx, s.stmt, args)
	}
	return execer.ExecContext(ctx, args)
}

func (s *serializedStmt) QueryContext(
	ctx context.Context,
	args []driver.NamedValue,
) (driver.Rows, error) {
	queryer, ok := s.stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, errors.New("sqlite statement does not support StmtQueryContext")
	}
	rows, err := queryer.QueryContext(ctx, args)
	if err != nil || rows == nil {
		return rows, err
	}
	s.held = false
	return &serializedRows{rows: rows, gate: s.gate, gid: s.gid}, nil
}

// serializedRows releases the gate exactly once, when the rows are closed.
type serializedRows struct {
	rows driver.Rows
	gate *shared.SqliteGate
	gid  int64
	once sync.Once
}

func (r *serializedRows) Columns() []string { return r.rows.Columns() }

func (r *serializedRows) Close() error {
	defer r.once.Do(func() { r.gate.Release(r.gid) })
	return r.rows.Close()
}

func (r *serializedRows) Next(dest []driver.Value) error { return r.rows.Next(dest) }

// serializedTx releases the gate when the transaction ends. The BEGIN it wraps
// was issued while the gate was held, so no other goroutine can be inside one.
type serializedTx struct {
	tx   driver.Tx
	gate *shared.SqliteGate
	gid  int64
	once sync.Once
}

func (t *serializedTx) Commit() error {
	defer t.once.Do(func() { t.gate.Release(t.gid) })
	return t.tx.Commit()
}

func (t *serializedTx) Rollback() error {
	defer t.once.Do(func() { t.gate.Release(t.gid) })
	return t.tx.Rollback()
}

// RunSqliteTransaction mirrors runSqliteTransaction. The implementation lives
// in internal/shared (aliased here, and used by internal/observability) because
// internal/observability cannot import internal/db yet its AgentIndexService runs
// the same multi-statement transactions the TS does.
var RunSqliteTransaction = shared.RunSqliteTransaction

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
