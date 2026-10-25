CREATE TABLE `daemon_meta` (
	`key` text PRIMARY KEY NOT NULL,
	`value` text NOT NULL
);
--> statement-breakpoint
CREATE TABLE `status_event_plans` (
	`agent_id` text NOT NULL,
	`attempts` integer DEFAULT 0 NOT NULL,
	`compact_history_json` text,
	`created_at` integer NOT NULL,
	`from_status` text NOT NULL,
	`herdr_event_key` text,
	`herdr_session_name` text NOT NULL,
	`id` integer PRIMARY KEY AUTOINCREMENT NOT NULL,
	`last_error` text,
	`pane_generation` text,
	`pane_id` text NOT NULL,
	`status` text DEFAULT 'pending' NOT NULL,
	`to_status` text NOT NULL,
	`updated_at` integer NOT NULL
);
--> statement-breakpoint
CREATE UNIQUE INDEX `status_event_plans_session_key_idx` ON `status_event_plans` (`herdr_session_name`,`herdr_event_key`) WHERE herdr_event_key IS NOT NULL;--> statement-breakpoint
CREATE INDEX `status_event_plans_status_id_idx` ON `status_event_plans` (`status`,`id`);