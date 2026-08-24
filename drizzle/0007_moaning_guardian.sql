ALTER TABLE `agent_events` ADD `status` text DEFAULT 'pending' NOT NULL;--> statement-breakpoint
ALTER TABLE `agent_events` ADD `delivery_attempts` integer DEFAULT 0 NOT NULL;--> statement-breakpoint
ALTER TABLE `agent_events` ADD `last_attempt_at` integer;--> statement-breakpoint
ALTER TABLE `agent_events` ADD `next_attempt_at` integer;--> statement-breakpoint
ALTER TABLE `agent_events` ADD `last_failure_code` text;--> statement-breakpoint
ALTER TABLE `agent_events` ADD `invalidated_reason` text;--> statement-breakpoint
ALTER TABLE `agent_events` ADD `delivered_to_terminal_id` text;--> statement-breakpoint
UPDATE `agent_events` SET `status` = CASE WHEN `deliverable` = 1 THEN 'pending' ELSE 'invalidated' END, `invalidated_reason` = CASE WHEN `deliverable` = 1 THEN NULL ELSE 'LEGACY_DELIVERABLE_FALSE' END;--> statement-breakpoint
CREATE INDEX `agent_events_delivery_scope_idx` ON `agent_events` (`herdr_session_name`,`workspace_id`,`status`,`id`);--> statement-breakpoint
CREATE INDEX `agent_events_delivery_retry_idx` ON `agent_events` (`status`,`next_attempt_at`);--> statement-breakpoint
