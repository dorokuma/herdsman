CREATE TABLE `agent_pane_tombstones` (
	`closed_at` integer NOT NULL,
	`herdr_session_name` text NOT NULL,
	`pane_generation` text,
	`pane_id` text NOT NULL,
	FOREIGN KEY (`herdr_session_name`) REFERENCES `herdr_sessions`(`name`) ON UPDATE no action ON DELETE cascade
);
--> statement-breakpoint
CREATE INDEX `agent_pane_tombstones_pane_idx` ON `agent_pane_tombstones` (`herdr_session_name`,`pane_id`);