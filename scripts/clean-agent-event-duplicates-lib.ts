/**
 * Pure helpers for the agent_events duplicate cleanup script
 * (scripts/clean-agent-event-duplicates.ts). Kept separate from the CLI
 * argument parsing so each step can be unit-tested.
 */
import { copyFileSync, existsSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import type { DatabaseSync } from "node:sqlite";
import { isProcessRunning } from "@/daemon/process-manager.js";

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
  const ownerPath = join(lockPath, "owner.json");
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
 * Detects a live daemon for a HERDSMAN_HOME: the pid file's pid is alive, or a
 * lock directory's owner pid is alive. Returns the live pid with the source
 * that proved it, or undefined when nothing points at a running process.
 */
export function liveDaemonOwnerPid(input: { pidPath: string }): DaemonLiveness {
  if (existsSync(input.pidPath)) {
    try {
      const pid = Number(readFileSync(input.pidPath, "utf8").trim());
      if (Number.isInteger(pid) && pid > 0 && isProcessRunning(pid)) {
        return { pid, source: "pid-file" };
      }
    } catch {
      // Unreadable pid file is treated as no pid-file evidence
    }
  }
  for (const lockPath of [`${input.pidPath}.lock`, `${input.pidPath}.instance.lock`]) {
    const ownerPid = readLockOwnerPid(lockPath);
    if (ownerPid !== undefined && isProcessRunning(ownerPid)) {
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
  if (live.pid === undefined) return;
  if (input.dryRun) {
    console.warn(
      `Herdsman daemon is running (pid ${live.pid}, ${live.source ?? "unknown"}); dry-run proceeds read-only`,
    );
    return;
  }
  throw new Error(
    `Herdsman daemon is running (pid ${live.pid}, ${live.source ?? "unknown"}); refusing to clean agent event duplicates`,
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