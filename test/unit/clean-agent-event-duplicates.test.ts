import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import { acquireDaemonLock, daemonInstanceLockPath } from "@/daemon/process-manager.js";
import {
  copyDatabaseBackup,
  ensureDaemonNotRunning,
  liveDaemonOwnerPid,
  prepareDuplicateTempTable,
  readLockOwnerPid,
  runDuplicateCleanup,
} from "../../scripts/clean-agent-event-duplicates-lib.js";
import {
  cleanupTempDirs,
  openObservabilityDbHarness,
} from "../integration/observability-db-harness.js";

const tempDirs: string[] = [];

afterEach(() => {
  cleanupTempDirs();
  for (const dir of tempDirs.splice(0)) rmSync(dir, { force: true, recursive: true });
});

function tempDir(): string {
  const dir = mkdtempSync(join(tmpdir(), "herdsman-clean-duplicates-"));
  tempDirs.push(dir);
  return dir;
}

describe("clean agent event duplicates helpers", () => {
  test("refuses write mode when a live daemon holds the instance flock lock", () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    const release = acquireDaemonLock(daemonInstanceLockPath(pidPath));
    try {
      expect(liveDaemonOwnerPid({ pidPath })).toEqual({ pid: process.pid, source: "lock" });
      expect(() => ensureDaemonNotRunning({ dryRun: false, pidPath })).toThrow(
        /Herdsman daemon is running .*refusing to clean agent event duplicates/,
      );
    } finally {
      release();
    }
  });

  test("refuses write mode when a lock owner is alive and warns on dry-run only", () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    for (const lockPath of [`${pidPath}.lock`, `${pidPath}.instance.lock`]) {
      const release = acquireDaemonLock(lockPath);
      try {
        expect(liveDaemonOwnerPid({ pidPath })).toEqual({ pid: process.pid, source: "lock" });
        expect(() => ensureDaemonNotRunning({ dryRun: false, pidPath })).toThrow(/refusing/);
      } finally {
        release();
      }
    }
    // Warns on dry-run when lock is held
    const release = acquireDaemonLock(daemonInstanceLockPath(pidPath));
    try {
      const warning = vi.spyOn(console, "warn").mockImplementation(() => {});
      expect(() => ensureDaemonNotRunning({ dryRun: true, pidPath })).not.toThrow();
      expect(warning).toHaveBeenCalledWith(expect.stringContaining("dry-run proceeds read-only"));
      warning.mockRestore();
    } finally {
      release();
    }
  });

  test("refuses write mode even when lock is held but owner.json is missing", () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    const lockPath = daemonInstanceLockPath(pidPath);
    const release = acquireDaemonLock(lockPath);
    try {
      // Intentionally remove owner.json while kernel lock is still held
      rmSync(`${lockPath}.owner.json`, { force: true });
      expect(liveDaemonOwnerPid({ pidPath })).toEqual({ pid: undefined, source: "lock" });
      expect(() => ensureDaemonNotRunning({ dryRun: false, pidPath })).toThrow(
        /Herdsman daemon is running \(unknown pid, lock\); refusing to clean agent event duplicates/,
      );
    } finally {
      release();
    }
  });

  test("ignores stale pids, dead lock files, and unheld locks even with live PIDs in metadata", () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    // Even if pid file or owner.json points to current live PID, without flock held it is stale
    writeFileSync(pidPath, `${process.pid}\n`);
    writeFileSync(`${pidPath}.lock`, "");
    writeFileSync(`${pidPath}.lock.owner.json`, JSON.stringify({ pid: process.pid }));
    expect(liveDaemonOwnerPid({ pidPath })).toEqual({ pid: undefined, source: undefined });
    expect(() => ensureDaemonNotRunning({ dryRun: false, pidPath })).not.toThrow();
    // A corrupted owner.json is not treated as a live daemon.
    writeFileSync(`${pidPath}.lock.owner.json`, "not-json");
    expect(readLockOwnerPid(`${pidPath}.lock`)).toBeUndefined();
  });

  test("backs up the database together with its -wal and -shm sidecars", () => {
    const dir = tempDir();
    const databasePath = join(dir, "state.db");
    writeFileSync(databasePath, "main-db-content");
    writeFileSync(`${databasePath}-wal`, "wal-content");
    writeFileSync(`${databasePath}-shm`, "shm-content");

    const backup = copyDatabaseBackup(databasePath);
    expect(backup.backupSizeBytes).toBe("main-db-content".length);
    expect(existsSync(backup.backupPath)).toBe(true);
    expect(existsSync(`${backup.backupPath}-wal`)).toBe(true);
    expect(existsSync(`${backup.backupPath}-shm`)).toBe(true);
  });

  test("rewrites scope cursors pointing at a deleted id to the kept id", () => {
    const harness = openObservabilityDbHarness();
    harness.herdrSessions.upsertRunning({
      name: "default",
      sessionDir: "/tmp/herdr",
      socketPath: "/tmp/herdr.sock",
    });
    const agent = harness.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [{ agent: "codex", pane_id: "wA:p1", terminal_id: "term-a", workspace_id: "wA" }],
    })[0];
    if (!agent) throw new Error("expected indexed agent");
    const ids: number[] = [];
    for (let index = 0; index < 3; index += 1) {
      const event = harness.agentEvents.append({
        agentId: agent.id,
        herdrSessionName: "default",
        paneId: "wA:p1",
        payload: { duplicate: true },
        terminalId: "term-a",
        type: "agent.done",
        workspaceId: "wA",
      });
      ids.push(event.id);
    }
    const [kept, duplicate, duplicate2] = ids;
    if (!kept || !duplicate || !duplicate2) throw new Error("expected three events");
    harness.agentOrchestratorScopes.claim({
      herdrSessionName: "default",
      workspaceId: "wA",
      ackedEventId: duplicate,
      paneId: "wA:owner",
      terminalId: "term-owner",
    });

    expect(prepareDuplicateTempTable(harness.sqlite)).toBe(2);
    const { cursorsRewritten, deletedRows } = runDuplicateCleanup(harness.sqlite);
    expect(cursorsRewritten).toBe(1);
    expect(deletedRows).toBe(2);
    expect(
      harness.agentOrchestratorScopes.get({ herdrSessionName: "default", workspaceId: "wA" }),
    ).toMatchObject({ ackedEventId: kept });
    expect(harness.agentEvents.get(kept).status).toBe("pending");
    expect(harness.sqlite.prepare("select count(*) as count from agent_events").get()).toEqual({
      count: 1,
    });
    harness.sqlite.close();
  });
});
