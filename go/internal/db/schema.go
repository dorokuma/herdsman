package db

import "database/sql"

// The consolidated schema of src/db/schema.ts: the 10 tables the daemon owns,
// with their primary keys, foreign keys, unique indexes and partial indexes.
//
// This is the shape the drizzle migrations in migrations/ build up (the ALTER
// TABLE columns included, in the order the migrations append them), and
// CreateSchema below must stay equivalent to replaying them —
// sqlite_migrations_test.go enforces exactly that. The structure is frozen:
// new columns or indexes belong in a new drizzle migration, not here.

// Table names, mirroring the sqliteTable names of src/db/schema.ts.
const (
	TableAgentContextSnapshots   = "agent_context_snapshots"
	TableAgentEvents             = "agent_events"
	TableAgentHistoryCache       = "agent_history_cache"
	TableAgentOrchestratorScopes = "agent_orchestrator_scopes"
	TableAgentPaneTombstones     = "agent_pane_tombstones"
	TableAgents                  = "agents"
	TableDaemonMeta              = "daemon_meta"
	TableHerdrSessions           = "herdr_sessions"
	TableHerdrWorkspaces         = "herdr_workspaces"
	TableStatusEventPlans        = "status_event_plans"
)

// SchemaStatements is the consolidated DDL of the final schema, in dependency
// order (parents before children). It is what the migrations/ replay produces.
var SchemaStatements = []string{
	// herdr_sessions: drizzle/0000.
	"CREATE TABLE `herdr_sessions` (\n" +
		"\t`last_scanned_at` integer,\n" +
		"\t`name` text PRIMARY KEY NOT NULL,\n" +
		"\t`running` integer NOT NULL,\n" +
		"\t`session_dir` text NOT NULL,\n" +
		"\t`socket_path` text NOT NULL,\n" +
		"\t`updated_at` integer NOT NULL\n" +
		")",

	// agents: drizzle/0000 plus the agent_session_hint_json and pane_revision
	// (0003), name (0004), pane_generation (0005) and grok_home (0006) columns
	// the ALTER TABLE migrations append, in that append order.
	"CREATE TABLE `agents` (\n" +
		"\t`agent` text,\n" +
		"\t`agent_session_json` text,\n" +
		"\t`agent_status` text NOT NULL,\n" +
		"\t`cwd` text,\n" +
		"\t`first_seen_at` integer NOT NULL,\n" +
		"\t`focused` integer NOT NULL,\n" +
		"\t`foreground_cwd` text,\n" +
		"\t`herdr_session_name` text NOT NULL,\n" +
		"\t`id` text PRIMARY KEY NOT NULL,\n" +
		"\t`last_seen_at` integer NOT NULL,\n" +
		"\t`pane_id` text NOT NULL,\n" +
		"\t`tab_id` text,\n" +
		"\t`terminal_id` text,\n" +
		"\t`workspace_id` text NOT NULL,\n" +
		"\t`agent_session_hint_json` text,\n" +
		"\t`pane_revision` integer,\n" +
		"\t`name` text,\n" +
		"\t`pane_generation` text,\n" +
		"\t`grok_home` text,\n" +
		"\tFOREIGN KEY (`herdr_session_name`) REFERENCES `herdr_sessions`(`name`) ON UPDATE no action ON DELETE cascade\n" +
		")",

	// agent_events: drizzle/0000 plus terminal_id (0001), pane_generation and
	// deliverable (0005) and the delivery columns (0007), in append order.
	"CREATE TABLE `agent_events` (\n" +
		"\t`agent_id` text,\n" +
		"\t`compact_history_json` text,\n" +
		"\t`created_at` integer NOT NULL,\n" +
		"\t`herdr_session_name` text NOT NULL,\n" +
		"\t`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,\n" +
		"\t`idempotency_key` text,\n" +
		"\t`pane_id` text,\n" +
		"\t`payload_json` text NOT NULL,\n" +
		"\t`type` text NOT NULL,\n" +
		"\t`workspace_id` text,\n" +
		"\t`terminal_id` text,\n" +
		"\t`pane_generation` text,\n" +
		"\t`deliverable` integer NOT NULL DEFAULT 1,\n" +
		"\t`status` text DEFAULT 'pending' NOT NULL,\n" +
		"\t`delivery_attempts` integer DEFAULT 0 NOT NULL,\n" +
		"\t`last_attempt_at` integer,\n" +
		"\t`next_attempt_at` integer,\n" +
		"\t`last_failure_code` text,\n" +
		"\t`invalidated_reason` text,\n" +
		"\t`delivered_to_terminal_id` text,\n" +
		"\tFOREIGN KEY (`agent_id`) REFERENCES `agents`(`id`) ON UPDATE no action ON DELETE set null,\n" +
		"\tFOREIGN KEY (`herdr_session_name`) REFERENCES `herdr_sessions`(`name`) ON UPDATE no action ON DELETE cascade\n" +
		")",

	// agent_history_cache: drizzle/0000.
	"CREATE TABLE `agent_history_cache` (\n" +
		"\t`compact_history_json` text NOT NULL,\n" +
		"\t`formatter_version` text NOT NULL,\n" +
		"\t`history_ref_json` text NOT NULL,\n" +
		"\t`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,\n" +
		"\t`source_mtime_ms` integer NOT NULL,\n" +
		"\t`source_path` text NOT NULL,\n" +
		"\t`source_size` integer NOT NULL,\n" +
		"\t`updated_at` integer NOT NULL\n" +
		")",

	// agent_orchestrator_scopes: drizzle/0001, composite primary key.
	"CREATE TABLE `agent_orchestrator_scopes` (\n" +
		"\t`acked_event_id` integer NOT NULL,\n" +
		"\t`created_at` integer NOT NULL,\n" +
		"\t`herdr_session_name` text NOT NULL,\n" +
		"\t`owner_pane_id` text,\n" +
		"\t`owner_terminal_id` text,\n" +
		"\t`updated_at` integer NOT NULL,\n" +
		"\t`workspace_id` text NOT NULL,\n" +
		"\tPRIMARY KEY(`herdr_session_name`, `workspace_id`),\n" +
		"\tFOREIGN KEY (`herdr_session_name`) REFERENCES `herdr_sessions`(`name`) ON UPDATE no action ON DELETE cascade\n" +
		")",

	// agent_context_snapshots: drizzle/0003.
	"CREATE TABLE `agent_context_snapshots` (\n" +
		"\t`agent_id` text PRIMARY KEY NOT NULL,\n" +
		"\t`compact_history_json` text NOT NULL,\n" +
		"\t`history_ref_json` text,\n" +
		"\t`pane_revision` integer,\n" +
		"\t`source_path` text,\n" +
		"\t`source_mtime_ms` integer,\n" +
		"\t`source_size` integer,\n" +
		"\t`updated_at` integer NOT NULL,\n" +
		"\tFOREIGN KEY (`agent_id`) REFERENCES `agents`(`id`) ON UPDATE no action ON DELETE cascade\n" +
		")",

	// agent_pane_tombstones: drizzle/0008.
	"CREATE TABLE `agent_pane_tombstones` (\n" +
		"\t`closed_at` integer NOT NULL,\n" +
		"\t`herdr_session_name` text NOT NULL,\n" +
		"\t`pane_generation` text,\n" +
		"\t`pane_id` text NOT NULL,\n" +
		"\tFOREIGN KEY (`herdr_session_name`) REFERENCES `herdr_sessions`(`name`) ON UPDATE no action ON DELETE cascade\n" +
		")",

	// daemon_meta: drizzle/0009.
	"CREATE TABLE `daemon_meta` (\n" +
		"\t`key` text PRIMARY KEY NOT NULL,\n" +
		"\t`value` text NOT NULL\n" +
		")",

	// status_event_plans: drizzle/0009.
	"CREATE TABLE `status_event_plans` (\n" +
		"\t`agent_id` text NOT NULL,\n" +
		"\t`attempts` integer DEFAULT 0 NOT NULL,\n" +
		"\t`compact_history_json` text,\n" +
		"\t`created_at` integer NOT NULL,\n" +
		"\t`from_status` text NOT NULL,\n" +
		"\t`herdr_event_key` text,\n" +
		"\t`herdr_session_name` text NOT NULL,\n" +
		"\t`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,\n" +
		"\t`last_error` text,\n" +
		"\t`pane_generation` text,\n" +
		"\t`pane_id` text NOT NULL,\n" +
		"\t`status` text DEFAULT 'pending' NOT NULL,\n" +
		"\t`to_status` text NOT NULL,\n" +
		"\t`updated_at` integer NOT NULL\n" +
		")",

	// herdr_workspaces: drizzle/0000.
	"CREATE TABLE `herdr_workspaces` (\n" +
		"\t`agent_status` text NOT NULL,\n" +
		"\t`focused` integer NOT NULL,\n" +
		"\t`herdr_session_name` text NOT NULL,\n" +
		"\t`label` text,\n" +
		"\t`last_seen_at` integer NOT NULL,\n" +
		"\t`workspace_id` text NOT NULL,\n" +
		"\tFOREIGN KEY (`herdr_session_name`) REFERENCES `herdr_sessions`(`name`) ON UPDATE no action ON DELETE cascade\n" +
		")",

	// Indexes, in the order the migrations create them.
	"CREATE UNIQUE INDEX `agents_session_pane_idx` ON `agents` (`herdr_session_name`,`pane_id`)",
	"CREATE UNIQUE INDEX `agents_session_terminal_idx` ON `agents` (`herdr_session_name`,`terminal_id`)",
	"CREATE UNIQUE INDEX `herdr_workspaces_session_workspace_idx` ON `herdr_workspaces` (`herdr_session_name`,`workspace_id`)",
	"CREATE UNIQUE INDEX `agent_events_session_idempotency_idx` ON `agent_events` (`herdr_session_name`,`idempotency_key`)",
	"CREATE INDEX `agent_events_session_pane_generation_idx` ON `agent_events` (`herdr_session_name`,`pane_id`,`pane_generation`)",
	"CREATE INDEX `agent_events_delivery_scope_idx` ON `agent_events` (`herdr_session_name`,`workspace_id`,`status`,`id`)",
	"CREATE INDEX `agent_events_delivery_retry_idx` ON `agent_events` (`status`,`next_attempt_at`)",
	"CREATE UNIQUE INDEX `agent_history_cache_source_formatter_idx` ON `agent_history_cache` (`source_path`,`formatter_version`)",
	"CREATE UNIQUE INDEX `status_event_plans_session_key_idx` ON `status_event_plans` (`herdr_session_name`,`herdr_event_key`) WHERE herdr_event_key IS NOT NULL",
	"CREATE INDEX `status_event_plans_status_id_idx` ON `status_event_plans` (`status`,`id`)",
	"CREATE INDEX `agent_pane_tombstones_pane_idx` ON `agent_pane_tombstones` (`herdr_session_name`,`pane_id`)",
	// The stale-baseline duplicate guard's window over one agent's newest
	// delivered/acked rows (drizzle/0011).
	"CREATE INDEX `agent_events_guard_scan_idx` ON `agent_events` (`agent_id`,`herdr_session_name`,`id`) WHERE status in ('delivered', 'acked')",
}

// CreateSchema builds the consolidated schema on an empty database. It is the
// from-scratch twin of ApplyMigrations and exists for tests and tooling that
// need the final shape without replaying history.
func CreateSchema(db *sql.DB) error {
	for _, statement := range SchemaStatements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}
