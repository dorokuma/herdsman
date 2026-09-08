/**
 * Pure helpers for the agent_events duplicate cleanup script
 * (scripts/clean-agent-event-duplicates.ts). Kept separate from the CLI
 * argument parsing so each step can be unit-tested.
 */
import { copyFileSync, existsSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import type { DatabaseSync } from "node:sqlite";
import { isFlockHeld } from "@/daemon/process-manager.js";

export type DaemonLiveness = {
  /** PID of the live daemon process, or undefined when no daemon is running. */
  pid: number | undefined;
  source: "pid-file" | "lock" | undefined;
};

/**
 * Reads the owner PID of a daemon lock directory (${pidPath}.lock /
 * ${pidPath}.instance.lock). Returns undefined when the lock does not exist or
 * has no valid owner.json.
 */
export function readLockOwnerPid(lockPath: string): number | undefined {
  const ownerPath = `${lockPath}.owner.json`;
  if (!existsSync(ownerPath)) return undefined;
  try {
    const data = JSON.parse(readFileSync(ownerPath, "utf8")) as { pid?: unknown };
    const pid = data.pid;
    return typeof pid === "number" && Number.isInteger(pid) && pid > 0 ? pid : undefined;
  } catch {
    return undefined;
  }
}

/**
 * Detects a live daemon for a HERDSMAN_HOME: probes non-blocking kernel flock on
 * lock paths (${pidPath}.instance.lock / ${pidPath}.lock). Returns the live pid
 * from owner metadata with the source that proved it, or undefined when no live daemon holds flock.
 */
export function liveDaemonOwnerPid(input: { pidPath: string }): DaemonLiveness {
  for (const lockPath of [`${input.pidPath}.instance.lock`, `${input.pidPath}.lock`]) {
    if (isFlockHeld(lockPath)) {
      const ownerPid = readLockOwnerPid(lockPath);
      return { pid: ownerPid, source: "lock" };
    }
  }
  return { pid: undefined, source: undefined };
}

/**
 * Refuses (write mode) or warns (dry-run) when a daemon is live for the given
 * pidPath. Write mode throws so the CLI exits non-zero; dry-run proceeds
 * read-only.
 */
export function ensureDaemonNotRunning(input: { dryRun: boolean; pidPath: string }): void {
  const live = liveDaemonOwnerPid({ pidPath: input.pidPath });
  if (live.source === undefined && live.pid === undefined) return;
  const pidDesc = live.pid !== undefined ? `pid ${live.pid}` : "unknown pid";
  if (input.dryRun) {
    console.warn(
      `Herdsman daemon is running (${pidDesc}, ${live.source ?? "unknown"}); dry-run proceeds read-only`,
    );
    return;
  }
  throw new Error(
    `Herdsman daemon is running (${pidDesc}, ${live.source ?? "unknown"}); refusing to clean agent event duplicates`,
  );
}

export type DuplicateBackup = {
  backupPath: string;
  backupSizeBytes: number;
};

/**
 * Copies the database plus its -wal/-shm sidecars (when present) to a
 * timestamped backup and verifies the main file's size. Throws when the main
 * file copy fails the size check.
 */
export function copyDatabaseBackup(databasePath: string): DuplicateBackup {
  const timestamp = new Date().toISOString().replace(/[-:.TZ]/g, "");
  const backupPath = `${databasePath}.bak-${timestamp}`;
  copyFileSync(databasePath, backupPath);
  const backupSize = statSync(backupPath).size;
  if (backupSize !== statSync(databasePath).size) {
    throw new Error("Backup size verification failed");
  }
  for (const suffix of ["-wal", "-shm"]) {
    const source = `${databasePath}${suffix}`;
    if (existsSync(source)) copyFileSync(source, `${backupPath}${suffix}`);
  }
  return { backupPath, backupSizeBytes: backupSize };
}

/**
 * Creates the temporary duplicate-id table. Each duplicate row carries the
 * id of the first row of its group (kept_id) so agent_orchestrator_scopes
 * cursors pointing at a deleted id can be rewritten before the delete.
 * Returns the number of rows scheduled for deletion.
 */
export function prepareDuplicateTempTable(sqlite: DatabaseSync): number {
  sqlite.exec(`
    drop table if exists temp.agent_event_duplicate_ids;
    create temp table agent_event_duplicate_ids as
    select id, kept_id from (
      select id,
        first_value(id) over (
          partition by herdr_session_name, agent_id, type, payload_json
          order by id
        ) as kept_id,
        row_number() over (
          partition by herdr_session_name, agent_id, type, payload_json
          order by id
        ) as duplicate_rank
      from agent_events
    )
    where duplicate_rank > 1;
  `);
  return Number(
    (
      sqlite
        .prepare("select count(*) as count from temp.agent_event_duplicate_ids")
        .get() as { count: number }
    ).count,
  );
}

/**
 * Rewrites agent_orchestrator_scopes.acked_event_id values that point at a
 * duplicate id to the group's kept id. Must run inside the same transaction
 * as the delete. Returns the number of cursors rewritten.
 */
export function rewriteDuplicateCursors(sqlite: DatabaseSync): number {
  return Number(
    sqlite
      .prepare(
        `update agent_orchestrator_scopes
         set acked_event_id = (
           select kept_id from temp.agent_event_duplicate_ids
           where temp.agent_event_duplicate_ids.id = agent_orchestrator_scopes.acked_event_id
         )
         where acked_event_id in (select id from temp.agent_event_duplicate_ids)`,
      )
      .run().changes,
  );
}

/**
 * Deletes the duplicate rows. Returns the number of deleted rows.
 */
export function deleteDuplicateRows(sqlite: DatabaseSync): number {
  return Number(
    sqlite
      .prepare(
        "delete from agent_events where id in (select id from temp.agent_event_duplicate_ids)",
      )
      .run().changes,
  );
}

/**
 * Runs cursor rewrite + delete in one transaction. Returns the number of
 * cursors rewritten and rows deleted.
 */
export function runDuplicateCleanup(sqlite: DatabaseSync): {
  cursorsRewritten: number;
  deletedRows: number;
} {
  sqlite.exec("begin immediate");
  try {
    const cursorsRewritten = rewriteDuplicateCursors(sqlite);
    const deletedRows = deleteDuplicateRows(sqlite);
    sqlite.exec("commit");
    return { cursorsRewritten, deletedRows };
  } catch (error) {
    sqlite.exec("rollback");
    throw error;
  }
}