#!/usr/bin/env tsx
/**
 * Remove historical semantic duplicates from the daemon's agent_events table.
 *
 * Usage: pnpm exec tsx scripts/clean-agent-event-duplicates.ts [--dry-run]
 *        ... [--db /absolute/path/to/state.db]
 */
import { existsSync } from "node:fs";
import { DatabaseSync } from "node:sqlite";
import { resolve } from "node:path";
import { argv } from "node:process";
import { resolveRuntime, runtimePathsFromRecordOrDefault } from "@/config/runtime.js";
import {
  copyDatabaseBackup,
  ensureDaemonNotRunning,
  prepareDuplicateTempTable,
  runDuplicateCleanup,
} from "./clean-agent-event-duplicates-lib.js";

const dryRun = argv.includes("--dry-run");
const dbOptionIndex = argv.indexOf("--db");
const dbPath = dbOptionIndex >= 0 ? argv[dbOptionIndex + 1] : undefined;
if (dbOptionIndex >= 0 && (!dbPath || dbPath.startsWith("--"))) {
  throw new Error("--db requires a database path");
}

const runtime = resolveRuntime();
const runtimePaths = runtimePathsFromRecordOrDefault({
  environment: runtime.environment,
  recordPath: runtime.paths.runtimeRecordPath,
});
// Refuse to touch a database whose daemon is live: a running daemon writes
// agent_events concurrently and would resurrect or re-deliver the rows this
// script deletes. Write mode hard-refuses (exit 1); dry-run warns and proceeds
// read-only.
ensureDaemonNotRunning({ dryRun, pidPath: runtimePaths.pidPath });
const databasePath = resolve(dbPath ?? runtimePaths.dbPath);
if (!existsSync(databasePath)) throw new Error(`SQLite database does not exist: ${databasePath}`);

const sqlite = new DatabaseSync(databasePath);
try {
  const before = Number(
    (sqlite.prepare("select count(*) as count from agent_events").get() as { count: number }).count,
  );
  const rowsToDelete = prepareDuplicateTempTable(sqlite);
  const duplicateGroups = Number(
    (
      sqlite
        .prepare(
          `select count(*) as count from (
             select herdr_session_name, agent_id, type, payload_json
             from agent_events
             group by herdr_session_name, agent_id, type, payload_json
             having count(*) > 1
           )`,
        )
        .get() as { count: number }
    ).count,
  );
  const cursorRows = Number(
    (
      sqlite
        .prepare(
          `select count(*) as count
           from agent_orchestrator_scopes s
           join temp.agent_event_duplicate_ids d on d.id = s.acked_event_id`,
        )
        .get() as { count: number }
    ).count,
  );
  const samples = sqlite
    .prepare(
      `select herdr_session_name, agent_id, type, substr(payload_json, 1, 160) as payload_sample,
              min(id) as kept_id, count(*) - 1 as delete_count
       from agent_events
       group by herdr_session_name, agent_id, type, payload_json
       having count(*) > 1
       order by delete_count desc, kept_id
       limit 5`,
    )
    .all() as Array<Record<string, unknown>>;

  console.log(`database=${databasePath}`);
  console.log(`mode=${dryRun ? "dry-run" : "write"}`);
  console.log(`before_total_rows=${before}`);
  console.log(`duplicate_groups=${duplicateGroups}`);
  console.log(`rows_to_delete=${rowsToDelete}`);
  console.log(`acked_cursor_rows_pointing_to_deleted_ids=${cursorRows}`);
  console.log("sample_groups=" + JSON.stringify(samples));

  if (dryRun) process.exitCode = 0;
  else {
    const backup = copyDatabaseBackup(databasePath);
    const { cursorsRewritten, deletedRows } = runDuplicateCleanup(sqlite);
    const after = Number(
      (sqlite.prepare("select count(*) as count from agent_events").get() as { count: number })
        .count,
    );
    console.log(`backup=${backup.backupPath}`);
    console.log(`backup_size_bytes=${backup.backupSizeBytes}`);
    console.log(`cursors_rewritten=${cursorsRewritten}`);
    console.log(`after_total_rows=${after}`);
    console.log(`deleted_rows=${deletedRows}`);
    if (after !== before - rowsToDelete) {
      throw new Error("Post-delete count does not match planned count");
    }
  }
} finally {
  sqlite.close();
}
