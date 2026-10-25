package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// The port of test/integration/sqlite-migrations.test.ts, plus the checks this
// port adds on top: replaying the embedded migrations is idempotent (an
// already-migrated database is reused untouched), and the consolidated
// CreateSchema of schema.go is structurally identical to the replayed one.

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
