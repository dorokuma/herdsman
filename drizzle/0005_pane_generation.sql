ALTER TABLE `agents` ADD `pane_generation` text;
--> statement-breakpoint
ALTER TABLE `agent_events` ADD `pane_generation` text;
--> statement-breakpoint
ALTER TABLE `agent_events` ADD `deliverable` integer NOT NULL DEFAULT 1;
--> statement-breakpoint
CREATE INDEX `agent_events_session_pane_generation_idx` ON `agent_events` (`herdr_session_name`,`pane_id`,`pane_generation`);