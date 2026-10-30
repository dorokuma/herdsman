package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/shared"
)

// The port of test/integration/sqlite-migrations.test.ts, plus the checks this
// port adds on top: replaying the embedded migrations is idempotent (an
// already-migrated database is reused untouched), and the consolidated
// CreateSchema of schema.go is structurally identical to the replayed one.
//
// This file also carries the database-gate regressions that used to live in
// sqlite_transaction_test.go: the TS side shares one DatabaseSync connection
// with an event loop, so a `begin immediate ... commit` block is atomic against
// every other statement. Go has no event loop, so without the gate two
// goroutines interleave `BEGIN` ("cannot start a transaction within a
// transaction") and a plain write can join somebody else's open transaction and
// be discarded by its rollback.

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenSqlite(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("openSqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openMigratedTestDB mirrors the TS observability harness: a fresh database
// with the migrations applied, ready for the stores.
func openMigratedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openTestDB(t)
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	return db
}

func TestSqliteMigrations(t *testing.T) {
	t.Run("create the agent index schema", func(t *testing.T) {
		db := openTestDB(t)
		if err := ApplyMigrations(db); err != nil {
			t.Fatalf("applyMigrations: %v", err)
		}

		tables := tableNames(t, db)
		wantTables := []string{
			"agent_context_snapshots",
			"agent_events",
			"agent_history_cache",
			"agent_orchestrator_scopes",
			"agent_pane_tombstones",
			"agents",
			"daemon_meta",
			"herdr_sessions",
			"herdr_workspaces",
			"status_event_plans",
		}
		if !slices.Equal(tables, wantTables) {
			t.Fatalf("tables = %v, want %v", tables, wantTables)
		}
		if slices.Contains(tables, "observed_workspaces") {
			t.Fatal("tables must not contain observed_workspaces")
		}

		// drizzle/0011 adds the partial index behind the stale-baseline
		// duplicate guard's "newest delivered/acked terminal rows for this
		// agent" window.
		var guardSQL string
		if err := db.QueryRow(
			"select sql from sqlite_master where name = 'agent_events_guard_scan_idx'",
		).Scan(&guardSQL); err != nil {
			t.Fatalf("read agent_events_guard_scan_idx: %v", err)
		}
		wantGuard := "(`agent_id`,`herdr_session_name`,`id`) WHERE status in ('delivered', 'acked')"
		if !strings.Contains(guardSQL, wantGuard) {
			t.Fatalf("agent_events_guard_scan_idx sql = %q, want it to contain %q", guardSQL, wantGuard)
		}

		scopeColumns := columnNames(t, db, "agent_orchestrator_scopes")
		for _, want := range []string{
			"acked_event_id", "herdr_session_name", "owner_pane_id", "owner_terminal_id", "workspace_id",
		} {
			if !slices.Contains(scopeColumns, want) {
				t.Fatalf("agent_orchestrator_scopes columns = %v, want it to contain %q", scopeColumns, want)
			}
		}

		eventColumns := tableInfo(t, db, "agent_events")
		if got := requireColumn(t, eventColumns, "terminal_id").notnull; got != 0 {
			t.Fatalf("agent_events.terminal_id notnull = %d, want 0", got)
		}

		agentColumns := tableInfo(t, db, "agents")
		for _, name := range []string{"pane_revision", "agent_session_hint_json", "name", "grok_home"} {
			column := requireColumn(t, agentColumns, name)
			if column.notnull != 0 || column.dflt != "" {
				t.Fatalf("agents.%s = {notnull:%d, dflt:%q}, want {notnull:0, dflt:null}",
					name, column.notnull, column.dflt)
			}
		}

		now := time.Now().UnixMilli()
		if _, err := db.Exec(
			"insert into herdr_sessions (name, running, session_dir, socket_path, updated_at) values (?, ?, ?, ?, ?)",
			"s", 1, "/tmp", "/tmp/s.sock", now,
		); err != nil {
			t.Fatalf("insert herdr_sessions: %v", err)
		}
		if _, err := db.Exec(
			"insert into agents (id, herdr_session_name, pane_id, workspace_id, agent_status, focused, first_seen_at, last_seen_at, grok_home) values (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			"a", "s", "p", "w", "idle", 0, now, now, "/tmp/grok",
		); err != nil {
			t.Fatalf("insert agents: %v", err)
		}
		var grokHome string
		if err := db.QueryRow("select grok_home from agents where id = ?", "a").Scan(&grokHome); err != nil {
			t.Fatalf("read grok_home: %v", err)
		}
		if grokHome != "/tmp/grok" {
			t.Fatalf("grok_home = %q, want %q", grokHome, "/tmp/grok")
		}

		contextColumns := tableInfo(t, db, "agent_context_snapshots")
		wantContextColumns := []string{
			"agent_id",
			"compact_history_json",
			"history_ref_json",
			"pane_revision",
			"source_path",
			"source_mtime_ms",
			"source_size",
			"updated_at",
		}
		if got := columnNamesFrom(contextColumns); !slices.Equal(got, wantContextColumns) {
			t.Fatalf("agent_context_snapshots columns = %v, want %v", got, wantContextColumns)
		}
		if got := requireColumn(t, contextColumns, "agent_id").pk; got != 1 {
			t.Fatalf("agent_context_snapshots.agent_id pk = %d, want 1", got)
		}
		if got := requireColumn(t, contextColumns, "history_ref_json").notnull; got != 0 {
			t.Fatalf("agent_context_snapshots.history_ref_json notnull = %d, want 0", got)
		}
		foreignKeys := foreignKeys(t, db, "agent_context_snapshots")
		if !slices.ContainsFunc(foreignKeys, func(fk foreignKeyInfo) bool {
			return fk.table == "agents" && fk.onDelete == "CASCADE"
		}) {
			t.Fatalf("agent_context_snapshots foreign keys = %v, want one to agents ON DELETE CASCADE", foreignKeys)
		}

		tombstoneColumns := tableInfo(t, db, "agent_pane_tombstones")
		wantTombstoneColumns := []string{"closed_at", "herdr_session_name", "pane_generation", "pane_id"}
		if got := columnNamesFrom(tombstoneColumns); !slices.Equal(got, wantTombstoneColumns) {
			t.Fatalf("agent_pane_tombstones columns = %v, want %v", got, wantTombstoneColumns)
		}
		if got := requireColumn(t, tombstoneColumns, "pane_generation").notnull; got != 0 {
			t.Fatalf("agent_pane_tombstones.pane_generation notnull = %d, want 0", got)
		}
	})

	t.Run("all drizzle migration sql files are registered in the journal and when timestamps are monotonically increasing", func(t *testing.T) {
		entries, err := migrationsFS.ReadDir("migrations")
		if err != nil {
			t.Fatalf("read embedded migrations: %v", err)
		}
		var sqlTags []string
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
				continue
			}
			sqlTags = append(sqlTags, strings.TrimSuffix(entry.Name(), ".sql"))
		}
		sort.Strings(sqlTags)

		journalBytes, err := migrationsFS.ReadFile("migrations/meta/_journal.json")
		if err != nil {
			t.Fatalf("read embedded journal: %v", err)
		}
		var journal migrationJournal
		if err := json.Unmarshal(journalBytes, &journal); err != nil {
			t.Fatalf("parse embedded journal: %v", err)
		}
		journalTags := make([]string, 0, len(journal.Entries))
		for _, entry := range journal.Entries {
			journalTags = append(journalTags, entry.Tag)
		}
		sort.Strings(journalTags)

		if !slices.Equal(sqlTags, journalTags) {
			t.Fatalf("migration sql files = %v, journal entries = %v", sqlTags, journalTags)
		}

		// Journal when timestamps must be strictly monotonically increasing to
		// prevent the high-water skip in ApplyMigrations from permanently
		// skipping migrations on existing databases.
		for i := 1; i < len(journal.Entries); i++ {
			if journal.Entries[i].When <= journal.Entries[i-1].When {
				t.Fatalf("journal when[%d] = %d, want > when[%d] = %d",
					i, journal.Entries[i].When, i-1, journal.Entries[i-1].When)
			}
		}
	})

	t.Run("replaying on an already migrated database is a no-op", func(t *testing.T) {
		db := openTestDB(t)
		if err := ApplyMigrations(db); err != nil {
			t.Fatalf("first applyMigrations: %v", err)
		}
		files, err := readMigrationFiles()
		if err != nil {
			t.Fatalf("readMigrationFiles: %v", err)
		}
		var applied int
		if err := db.QueryRow("select count(*) from " + migrationsTable).Scan(&applied); err != nil {
			t.Fatalf("count migrations: %v", err)
		}
		if applied != len(files) {
			t.Fatalf("applied migrations = %d, want %d", applied, len(files))
		}
		fingerprint := schemaFingerprint(t, db)

		// An existing (production) database keeps its tables and its recorded
		// timestamps: nothing is replayed, nothing is rewritten.
		if err := ApplyMigrations(db); err != nil {
			t.Fatalf("second applyMigrations: %v", err)
		}
		var reapplied int
		if err := db.QueryRow("select count(*) from " + migrationsTable).Scan(&reapplied); err != nil {
			t.Fatalf("count migrations after replay: %v", err)
		}
		if reapplied != applied {
			t.Fatalf("applied migrations after replay = %d, want %d", reapplied, applied)
		}
		if after := schemaFingerprint(t, db); after != fingerprint {
			t.Fatalf("schema changed on replay:\n%s\nwant:\n%s", after, fingerprint)
		}
	})

	t.Run("CreateSchema matches the migrated schema", func(t *testing.T) {
		migrated := openTestDB(t)
		if err := ApplyMigrations(migrated); err != nil {
			t.Fatalf("applyMigrations: %v", err)
		}
		created := openTestDB(t)
		if err := CreateSchema(created); err != nil {
			t.Fatalf("createSchema: %v", err)
		}
		migratedFingerprint := schemaFingerprint(t, migrated)
		createdFingerprint := schemaFingerprint(t, created)
		if migratedFingerprint != createdFingerprint {
			t.Fatalf("CreateSchema schema differs from the migrated one:\n%s\nwant:\n%s",
				createdFingerprint, migratedFingerprint)
		}
	})
}

// --- schema introspection helpers -------------------------------------------

type columnInfo struct {
	name    string
	typ     string
	notnull int
	dflt    string
	pk      int
}

type foreignKeyInfo struct {
	table    string
	from     string
	to       string
	onUpdate string
	onDelete string
}

type indexInfo struct {
	name    string
	unique  int
	origin  string
	partial int
	columns []string
}

// tableNames lists the application tables, mirroring the TS test's filter of
// the bookkeeping and sqlite internal tables.
func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("select name from sqlite_master where type = 'table' order by name")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		if name == migrationsTable || name == "sqlite_sequence" {
			continue
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	return names
}

func columnNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	return columnNamesFrom(tableInfo(t, db, table))
}

func columnNamesFrom(columns []columnInfo) []string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, column.name)
	}
	return names
}

func tableInfo(t *testing.T, db *sql.DB, table string) []columnInfo {
	t.Helper()
	rows, err := db.Query("pragma table_info(" + table + ")")
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	columns := []columnInfo{}
	for rows.Next() {
		var cid int
		var dflt sql.NullString
		column := columnInfo{}
		if err := rows.Scan(&cid, &column.name, &column.typ, &column.notnull, &dflt, &column.pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		column.dflt = dflt.String
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	return columns
}

// requireColumn returns the named column or fails: an assertion about a missing
// column must not pass on a zero value.
func requireColumn(t *testing.T, columns []columnInfo, name string) columnInfo {
	t.Helper()
	for _, column := range columns {
		if column.name == name {
			return column
		}
	}
	t.Fatalf("columns %v, want it to contain %q", columnNamesFrom(columns), name)
	return columnInfo{}
}

func foreignKeys(t *testing.T, db *sql.DB, table string) []foreignKeyInfo {
	t.Helper()
	rows, err := db.Query("pragma foreign_key_list(" + table + ")")
	if err != nil {
		t.Fatalf("pragma foreign_key_list(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	keys := []foreignKeyInfo{}
	for rows.Next() {
		var id, seq int
		var to, match sql.NullString
		key := foreignKeyInfo{}
		if err := rows.Scan(&id, &seq, &key.table, &key.from, &to, &key.onUpdate, &key.onDelete, &match); err != nil {
			t.Fatalf("scan foreign_key_list(%s): %v", table, err)
		}
		key.to = to.String
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pragma foreign_key_list(%s): %v", table, err)
	}
	return keys
}

func indexInfos(t *testing.T, db *sql.DB, table string) []indexInfo {
	t.Helper()
	rows, err := db.Query("pragma index_list(" + table + ")")
	if err != nil {
		t.Fatalf("pragma index_list(%s): %v", table, err)
	}
	indexes := []indexInfo{}
	for rows.Next() {
		var seq int
		index := indexInfo{}
		if err := rows.Scan(&seq, &index.name, &index.unique, &index.origin, &index.partial); err != nil {
			t.Fatalf("scan index_list(%s): %v", table, err)
		}
		indexes = append(indexes, index)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pragma index_list(%s): %v", table, err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("pragma index_list(%s): %v", table, err)
	}
	// The index columns are read after index_list is fully consumed: the shared
	// single connection cannot serve a nested query while rows are open.
	for i := range indexes {
		indexes[i].columns = indexColumns(t, db, indexes[i].name)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i].name < indexes[j].name })
	return indexes
}

func indexColumns(t *testing.T, db *sql.DB, index string) []string {
	t.Helper()
	rows, err := db.Query("pragma index_info(" + index + ")")
	if err != nil {
		t.Fatalf("pragma index_info(%s): %v", index, err)
	}
	defer func() { _ = rows.Close() }()
	columns := []string{}
	for rows.Next() {
		var seqno, cid int
		var name sql.NullString
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			t.Fatalf("scan index_info(%s): %v", index, err)
		}
		columns = append(columns, name.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pragma index_info(%s): %v", index, err)
	}
	return columns
}

// schemaFingerprint renders the structural shape of a database: tables, their
// columns (with type, notnull, default and primary-key position), foreign keys
// and indexes (with uniqueness, origin, partiality and columns). Two databases
// with the same fingerprint have the same schema.
func schemaFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	var out strings.Builder
	for _, table := range tableNames(t, db) {
		fmt.Fprintf(&out, "table %s\n", table)
		for _, column := range tableInfo(t, db, table) {
			fmt.Fprintf(&out, "  column %s %s notnull=%d dflt=%q pk=%d\n",
				column.name, column.typ, column.notnull, column.dflt, column.pk)
		}
		keys := foreignKeys(t, db, table)
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].table != keys[j].table {
				return keys[i].table < keys[j].table
			}
			return keys[i].from < keys[j].from
		})
		for _, key := range keys {
			fmt.Fprintf(&out, "  fk %s.%s references %s.%s on update %s on delete %s\n",
				table, key.from, key.table, key.to, key.onUpdate, key.onDelete)
		}
		for _, index := range indexInfos(t, db, table) {
			fmt.Fprintf(&out, "  index %s unique=%d origin=%s partial=%d columns=%s\n",
				index.name, index.unique, index.origin, index.partial, strings.Join(index.columns, ","))
		}
	}
	return out.String()
}

// ---------------------------------------------------------------------------
// Handle lifecycle and the database gate
// ---------------------------------------------------------------------------

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

// TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate is the regression test
// for the handle lifecycle. database/sql calls connector.Close from
// (*sql.DB).Close, so the connector is the one place that can release the single
// shared driver connection and drop the gate registration.
//
// Two things are asserted against the driver connection itself, not against
// database/sql's own bookkeeping: a closed *sql.DB refuses statements on its own
// (it never reaches the driver), so an error from db.Exec would prove nothing
// about whether the connection was released. DriverConnClosed and
// WaitDriverConnClosed report the connector's own record of the driver Close
// returning, which only the production code can set.
//
// The last subtest is the concurrency regression: DriverConnClosed polled while
// (*sql.DB).Close tears the driver connection down must be synchronised
// against that teardown, not just the closed flag.
// pollEnteredContext closes entered the first time Done() is called.
// WaitDriverConnClosed's loop only reaches its select — and with it Done() —
// after a DriverConnClosed(db) pass has observed the connection still open, so
// the first Done() call is a 100% deterministic "the waiter is inside its own
// 1ms poll cycle" barrier: no pre-check in the waiter goroutine, no sleep to
// guess when the waiter reached the select.
type pollEnteredContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *pollEnteredContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestOpenSqliteCloseReleasesDriverConnAndUnregistersGate(t *testing.T) {
	t.Run("the handle closes the driver connection and drops the gate", func(t *testing.T) {
		db, err := OpenSqlite(filepath.Join(t.TempDir(), "close.sqlite"))
		if err != nil {
			t.Fatalf("openSqlite: %v", err)
		}
		gate := shared.SqliteGateOf(db)
		if gate == nil {
			t.Fatal("SqliteGateOf = nil after OpenSqlite, want the registered gate")
		}
		// The connection is open before the close, so the observation below really
		// does track a transition rather than a constant true.
		if DriverConnClosed(db) {
			t.Fatal("DriverConnClosed = true before Close, want an open driver connection")
		}

		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		// The direct observation: the driver connection itself was closed. Comment
		// out c.conn.Close() in serializedConnector.Close and this line fails while
		// every db.Exec-based assertion below still passes.
		if !DriverConnClosed(db) {
			t.Fatal("DriverConnClosed = false after Close, want the driver connection closed")
		}

		// The gate registration goes with the handle, so a later lookup by the same
		// (now dead) handle finds nothing. SqliteGateOf reads the registry map, and
		// the handle pointer is still a valid map key, so this really does ask about
		// the closed handle rather than about a fresh one.
		if lingering := shared.SqliteGateOf(db); lingering != nil {
			t.Fatal("SqliteGateOf after Close returned the gate, want the registration to be dropped")
		}

		// The handle is unusable afterwards (database/sql enforces this itself, so
		// it is a usability assertion, not the release evidence above).
		if _, err := db.Exec("select 1"); err == nil {
			t.Fatal("exec on a closed handle succeeded, want an error")
		}

		// Close is idempotent: (*sql.DB).Close may run more than once and a second
		// call must not resurface an error from closing the driver connection twice.
		if err := db.Close(); err != nil {
			t.Fatalf("second close: %v", err)
		}
		if !DriverConnClosed(db) {
			t.Fatal("DriverConnClosed = false after the second Close, want it to stay closed")
		}
	})

	// The gate is what makes the teardown safe: a Close that closes the driver
	// connection without taking the gate races whatever statement is inside the
	// driver right now. Holding the gate from another goroutine must delay the
	// driver Close until it is released.
	t.Run("Close waits for a concurrently held gate", func(t *testing.T) {
		db, err := OpenSqlite(filepath.Join(t.TempDir(), "close-gated.sqlite"))
		if err != nil {
			t.Fatalf("openSqlite: %v", err)
		}
		gate := shared.SqliteGateOf(db)
		if gate == nil {
			t.Fatal("SqliteGateOf = nil after OpenSqlite, want the registered gate")
		}

		// A different goroutine holds the gate, exactly as a statement or a
		// transaction would while it is inside the driver.
		holderDone := make(chan struct{})
		release := make(chan struct{})
		holderReady := make(chan struct{})
		go func() {
			defer close(holderDone)
			gid := shared.CurrentGoroutineID()
			gate.Acquire(gid)
			defer gate.Release(gid)
			close(holderReady)
			<-release
		}()
		<-holderReady

		closed := make(chan error, 1)
		go func() { closed <- db.Close() }()

		// While the gate is held the driver connection must stay open. This is the
		// observation that fails when Close closes it without acquiring the gate.
		select {
		case err := <-closed:
			t.Fatalf("db.Close returned (%v) while another goroutine held the gate", err)
		case <-time.After(200 * time.Millisecond):
		}
		if DriverConnClosed(db) {
			t.Fatal("DriverConnClosed = true while another goroutine held the gate, " +
				"want the close to wait for it")
		}

		// Releasing the gate lets the close proceed; the driver connection is then
		// really closed and the handle reports no error.
		close(release)
		<-holderDone
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("close after the gate was released: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("db.Close did not finish after the gate was released")
		}
		if !DriverConnClosed(db) {
			t.Fatal("DriverConnClosed = false after the gate was released, want the connection closed")
		}
		if lingering := shared.SqliteGateOf(db); lingering != nil {
			t.Fatal("SqliteGateOf after Close returned the gate, want the registration to be dropped")
		}
	})

	// A Close racing a live Rows cursor is the same hazard in the shape the
	// daemon actually hits: the cursor holds the gate for as long as it streams.
	t.Run("Close waits for an open Rows cursor", func(t *testing.T) {
		db := openMigratedTestDB(t)

		rows, err := db.Query("select name from sqlite_master")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		// The cursor holds the gate now, exactly like a caller still draining it.
		if rows.Next() {
			// Reading one row is enough: the gate stays held until Close.
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan: %v", err)
			}
		}

		closed := make(chan error, 1)
		go func() { closed <- db.Close() }()
		select {
		case err := <-closed:
			t.Fatalf("db.Close returned (%v) with a Rows cursor still open", err)
		case <-time.After(200 * time.Millisecond):
		}
		if DriverConnClosed(db) {
			t.Fatal("DriverConnClosed = true with a Rows cursor still open, " +
				"want the close to wait for it")
		}

		if err := rows.Close(); err != nil {
			t.Fatalf("close rows: %v", err)
		}
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("close after the rows were closed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("db.Close did not finish after the Rows cursor was closed")
		}
		if !DriverConnClosed(db) {
			t.Fatal("DriverConnClosed = false after the rows were closed, want the connection closed")
		}
	})

	// The teardown itself must also be synchronised against the observation
	// points. modernc.org/sqlite mutates its own connection state inside Close
	// (the sqlite3 handle is released, TLS state dropped), and DriverConnClosed
	// reads exactly that state through driver.Validator. A waiter polling
	// DriverConnClosed while (*sql.DB).Close runs — what WaitDriverConnClosed
	// does, and what every subtest above does while a Close is in flight — used
	// to read those fields with no synchronisation at all, a data race -race
	// flags. The poller and the WaitDriverConnClosed waiter below are both
	// parked against a held gate and held at a readiness barrier before the
	// gate is released: both are already observing a live connection (the
	// waiter parked inside WaitDriverConnClosed's own 1ms poll select) when
	// the driver Close actually starts, so the overlap window is the lifetime
	// of the driver's own Close — which an observer starting only after
	// (*sql.DB).Close returned could never observe.

	t.Run("DriverConnClosed does not race the driver Close", func(t *testing.T) {
		db := openMigratedTestDB(t)
		gate := shared.SqliteGateOf(db)
		if gate == nil {
			t.Fatal("SqliteGateOf = nil after OpenSqlite, want the registered gate")
		}

		// Hold the gate first, then start the close: it parks on gate.Acquire
		// while the poller below starts spinning.
		holderDone := make(chan struct{})
		release := make(chan struct{})
		holderReady := make(chan struct{})
		go func() {
			defer close(holderDone)
			gid := shared.CurrentGoroutineID()
			gate.Acquire(gid)
			defer gate.Release(gid)
			close(holderReady)
			<-release
		}()
		<-holderReady

		closed := make(chan error, 1)
		go func() { closed <- db.Close() }()

		// Both observers must be started (and already observing a live
		// connection) before the gate is released: a goroutine that is only
		// scheduled after db.Close() has already run can never overlap the
		// driver Close, so the overlap window below would be empty on a slow
		// scheduler. pollerReady proves the tight poller read the still-open
		// connection; pollEnteredContext.Done() proves the WaitDriverConnClosed
		// goroutine checked !DriverConnClosed and is parked inside its own 1ms
		// poll select.
		pollerDone := make(chan struct{})
		pollerReady := make(chan struct{})
		go func() {
			defer close(pollerDone)
			if DriverConnClosed(db) {
				return
			}
			close(pollerReady)
			// No sleep between the reads: the point is to overlap the poller's
			// driver.Validator reads with the driver Close running under the
			// gate, not to step over it a millisecond at a time.
			for !DriverConnClosed(db) {
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		waitCtx := &pollEnteredContext{Context: ctx, entered: make(chan struct{})}
		waiterResult := make(chan bool, 1)
		go func() { waiterResult <- WaitDriverConnClosed(waitCtx, db) }()

		<-pollerReady
		<-waitCtx.entered
		// entered is closed only once WaitDriverConnClosed's own loop has
		// evaluated Done() — which happens right after DriverConnClosed(db) saw
		// the connection still open — so the waiter is parked inside its 1ms
		// select while the gate still holds the close back. Assert it has not
		// returned yet, then release the gate.
		select {
		case result := <-waiterResult:
			t.Fatalf("WaitDriverConnClosed = %v while the gate still held the close back, want it still polling", result)
		default:
		}

		close(release)
		<-holderDone
		<-pollerDone

		select {
		case result := <-waiterResult:
			if !result {
				t.Fatal("WaitDriverConnClosed = false after Close, want the driver connection closed")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("WaitDriverConnClosed did not return after Close finished")
		}

		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("close with a concurrent DriverConnClosed poller: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("db.Close did not finish while DriverConnClosed was polling")
		}
	})
}

// TestRunSqliteTransactionConcurrentDoesNotInterleave is the gate regression:
// eight goroutines each run a two-statement transaction, so a transaction that
// did not hold the gate across fn would let the next goroutine's BEGIN in.
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

// TestRunSqliteTransactionRollbackDoesNotDiscardOtherGoroutineWrite asserts the
// other half of the window: a plain write issued while a transaction is open
// waits for the gate instead of joining the transaction and being rolled back.
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

// TestRunSqliteTransactionRollsBackAfterFailedCommit is the regression test for
// the commit-failure branch of RunSqliteTransaction.
//
// A deferred foreign-key check makes the INSERT succeed inside fn and the COMMIT
// statement itself fail, which is the only way to reach a failed commit with a
// transaction still open. Without the explicit rollback on that path the
// transaction stays attached to the shared connection, the goroutine releases
// the gate, and the next caller's `begin immediate` fails with "cannot start a
// transaction within a transaction".
func TestRunSqliteTransactionRollsBackAfterFailedCommit(t *testing.T) {
	db := openMigratedTestDB(t)
	insertTestSession(t, db)

	commitErr := RunSqliteTransaction(db, func() error {
		// defer_foreign_keys postpones the foreign-key check to COMMIT, so the
		// insert below is accepted here and rejected by the COMMIT statement.
		// It is a connection-scoped pragma on the one shared connection; the
		// deferred check is per-transaction, so the next transaction starts
		// clean.
		if _, err := db.Exec("pragma defer_foreign_keys = on"); err != nil {
			return err
		}
		_, err := db.Exec(
			"insert into herdr_workspaces " +
				"(herdr_session_name, workspace_id, focused, agent_status, last_seen_at) " +
				"values ('no-such-session', 'orphan', 0, 'active', 1)",
		)
		return err
	})
	if commitErr == nil {
		t.Fatal("RunSqliteTransaction = nil, want the COMMIT failure to surface")
	}

	// The rollback already ran, so the connection is free again: this second
	// transaction must be able to BEGIN IMMEDIATE and commit. Without the
	// rollback it fails with "cannot start a transaction within a transaction".
	if err := RunSqliteTransaction(db, func() error {
		return insertTestWorkspace(db, "after-failed-commit")
	}); err != nil {
		t.Fatalf("transaction after a failed commit: %v", err)
	}

	var committed int
	if err := db.QueryRow(
		"select count(*) from herdr_workspaces where workspace_id = 'after-failed-commit'",
	).Scan(&committed); err != nil {
		t.Fatalf("count committed: %v", err)
	}
	if committed != 1 {
		t.Fatalf("committed workspaces = %d, want 1", committed)
	}

	// The failed transaction left nothing behind, and the deferred check was
	// undone with it: the orphan row is gone and a valid row still inserts.
	var orphans int
	if err := db.QueryRow(
		"select count(*) from herdr_workspaces where workspace_id = 'orphan'",
	).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("orphan workspace count = %d, want 0 (the failed commit was rolled back)", orphans)
	}
}

// TestRunSqliteTransactionRollsBackAfterFnPanic is the regression for the
// panic path of RunSqliteTransaction: a panicking callback must still roll its
// transaction back before the panic propagates.
//
// Without the recover inside the function the defer chain still releases the
// gate, but the open transaction stays attached to the one connection every
// goroutine shares — the next caller's `begin immediate` then fails with
// "cannot start a transaction within a transaction", which is the same stuck
// window a failed commit leaves.
func TestRunSqliteTransactionRollsBackAfterFnPanic(t *testing.T) {
	db := openMigratedTestDB(t)
	insertTestSession(t, db)

	func() {
		defer func() {
			if p := recover(); p == nil {
				t.Fatal("RunSqliteTransaction returned without panicking")
			} else if p != "fn panicked on purpose" {
				t.Fatalf("panic = %v, want the callback's own panic to propagate", p)
			}
		}()
		_ = RunSqliteTransaction(db, func() error {
			if err := insertTestWorkspace(db, "panic-rolled-back"); err != nil {
				return err
			}
			panic("fn panicked on purpose")
		})
	}()

	// The rollback ran before the panic propagated, so the shared connection is
	// clean again: a new transaction must be able to BEGIN IMMEDIATE and commit
	// instead of hitting the leftover open transaction.
	if err := RunSqliteTransaction(db, func() error {
		return insertTestWorkspace(db, "after-fn-panic")
	}); err != nil {
		t.Fatalf("transaction after a panicking fn: %v", err)
	}

	var rolledBack, committed int
	if err := db.QueryRow(
		"select count(*) from herdr_workspaces where workspace_id = 'panic-rolled-back'",
	).Scan(&rolledBack); err != nil {
		t.Fatalf("count rolled-back: %v", err)
	}
	if err := db.QueryRow(
		"select count(*) from herdr_workspaces where workspace_id = 'after-fn-panic'",
	).Scan(&committed); err != nil {
		t.Fatalf("count committed: %v", err)
	}
	if rolledBack != 0 {
		t.Fatalf("panic-rolled-back workspace count = %d, want 0 (the panic discarded it)", rolledBack)
	}
	if committed != 1 {
		t.Fatalf("after-fn-panic workspace count = %d, want 1", committed)
	}
}

// TestRunSqliteTransactionPanicSettlesOrphanedGateLevels is the regression for
// the owner-never-release shape the rollback alone cannot fix: when fn panics
// while a *sql.Rows (or a prepared statement) it opened is still unclosed,
// that wrapper's gate level is orphaned — the panic unwinds past its Close.
// Before the fix the deferred Release popped only the transaction's own
// level, so depth never reached zero, the owner stayed pinned, and every
// later statement queued forever — the exact deadlock the watchdog reports.
func TestRunSqliteTransactionPanicSettlesOrphanedGateLevels(t *testing.T) {
	db := openMigratedTestDB(t)
	insertTestSession(t, db)

	gid := make(chan int64, 1)
	func() {
		defer func() {
			if p := recover(); p == nil {
				t.Fatal("RunSqliteTransaction returned without panicking")
			}
		}()
		_ = RunSqliteTransaction(db, func() error {
			// Query keeps its gate level until rows.Close, which the panic
			// below unwinds past — the orphaned level of the production shape.
			rows, err := db.Query("select workspace_id from herdr_workspaces")
			if err != nil {
				return err
			}
			_ = rows // deliberately unclosed
			gid <- shared.CurrentGoroutineID()
			panic("fn panicked with open rows")
		})
	}()

	// The unwind settled every level this goroutine owned, so its leftover
	// gate hold is gone entirely.
	if shared.SqliteGateOf(db).HeldBy(<-gid) {
		t.Fatal("gate still held by the panicking goroutine: orphaned rows level kept depth above zero")
	}

	// The gate is free again: a new transaction from another goroutine
	// acquires, begins, and commits instead of queueing forever.
	done := make(chan error, 1)
	go func() {
		done <- RunSqliteTransaction(db, func() error {
			return insertTestWorkspace(db, "after-orphaned-rows-panic")
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("transaction after the panic: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: the orphaned rows level kept the gate owned forever")
	}

	var committed int
	if err := db.QueryRow(
		"select count(*) from herdr_workspaces where workspace_id = 'after-orphaned-rows-panic'",
	).Scan(&committed); err != nil {
		t.Fatalf("count committed: %v", err)
	}
	if committed != 1 {
		t.Fatalf("after-orphaned-rows-panic workspace count = %d, want 1", committed)
	}
}

// TestRunSqliteTransactionPanicRollsBackBeforeReleasingGate is the regression
// for the lock order of the panic path: the rollback must run while the
// panicking goroutine still owns the gate, and the orphaned gate levels
// settle only after it. Settling them first would broadcast the queue awake
// while the shared connection is still inside the open transaction — a
// waiter that wins the free gate then either fails its `begin immediate`
// with "cannot start a transaction within a transaction" or writes into a
// transaction the rollback is about to discard, losing a write it believes
// committed.
//
// A wrong order opens only a single race — the woken waiter must reach the
// gate before the rollback re-acquires it — so the assertion runs parked
// waiters against the panic over many attempts. Under the fixed order no
// waiter can observe the gate free while the transaction is open, so every
// attempt must commit every waiter row; under the old order one steal is
// enough to lose the waiter's write to the rollback.
func TestRunSqliteTransactionPanicRollsBackBeforeReleasingGate(t *testing.T) {
	db := openMigratedTestDB(t)
	insertTestSession(t, db)

	const attempts = 64
	const waiters = 8
	for attempt := 0; attempt < attempts; attempt++ {
		var wait sync.WaitGroup
		errs := make([]error, waiters)
		wait.Add(waiters)

		func() {
			defer func() {
				if p := recover(); p == nil {
					t.Fatal("RunSqliteTransaction returned without panicking")
				}
			}()
			_ = RunSqliteTransaction(db, func() error {
				rolledBack := fmt.Sprintf("window-rolled-back-%d", attempt)
				if err := insertTestWorkspace(db, rolledBack); err != nil {
					return err
				}
				// The waiters park on the gate while fn still holds it, so a
				// ReleaseAll ahead of the rollback wakes them into the still-open
				// transaction — their plain inserts would silently join it and be
				// discarded by the rollback that follows.
				for index := 0; index < waiters; index++ {
					go func(index int) {
						defer wait.Done()
						errs[index] = insertTestWorkspace(db, fmt.Sprintf("window-waiter-%d-%d", attempt, index))
					}(index)
				}
				time.Sleep(5 * time.Millisecond)
				panic("fn panicked with waiters queued on the gate")
			})
		}()

		wait.Wait()
		for index, err := range errs {
			if err != nil {
				t.Fatalf("attempt %d waiter %d failed: %v", attempt, index, err)
			}
		}

		var survived, rolledBack int
		if err := db.QueryRow(
			"select count(*) from herdr_workspaces where workspace_id like ?",
			fmt.Sprintf("window-waiter-%d-%%", attempt),
		).Scan(&survived); err != nil {
			t.Fatalf("count waiter rows: %v", err)
		}
		if survived != waiters {
			t.Fatalf("attempt %d: committed waiter rows = %d, want %d — a waiter wrote into the not-yet-rolled-back transaction and the rollback discarded it",
				attempt, survived, waiters)
		}
		if err := db.QueryRow(
			"select count(*) from herdr_workspaces where workspace_id = ?",
			fmt.Sprintf("window-rolled-back-%d", attempt),
		).Scan(&rolledBack); err != nil {
			t.Fatalf("count rolled-back row: %v", err)
		}
		if rolledBack != 0 {
			t.Fatalf("attempt %d: rolled-back row count = %d, want 0", attempt, rolledBack)
		}
	}
}
