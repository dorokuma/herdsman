import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { AgentEventReconciler, HISTORY_CACHE_TTL_MS } from "@/daemon/agent-event-reconciler.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

const session = {
  name: "default",
  running: true,
  sessionDir: "/tmp/herdr",
  socketPath: "/tmp/herdr.sock",
};

const harnesses: Array<ReturnType<typeof openObservabilityDbHarness>> = [];

afterEach(() => {
  vi.restoreAllMocks();
  for (const harness of harnesses.splice(0)) harness.sqlite.close();
  cleanupTempDirs();
});

function setup() {
  const harness = openObservabilityDbHarness();
  harnesses.push(harness);
  return harness;
}

function putCache(
  harness: ReturnType<typeof openObservabilityDbHarness>,
  input: { sourcePath: string; formatterVersion?: string },
) {
  return harness.agentHistoryCache.put({
    compactHistory: emptyCompactHistory("pi-jsonl"),
    formatterVersion: input.formatterVersion ?? "agent-history-v3",
    historyRef: {
      kind: "discovered_file",
      path: input.sourcePath,
      source: "pi-jsonl",
      value: input.sourcePath,
    },
    sourceMtimeMs: 1,
    sourcePath: input.sourcePath,
    sourceSize: 1,
  });
}

function sourceFile(harness: ReturnType<typeof openObservabilityDbHarness>, name: string): string {
  const path = join(dirname(harness.dbPath), name);
  writeFileSync(path, "history\n");
  return path;
}

function reconciler(harness: ReturnType<typeof openObservabilityDbHarness>) {
  return new AgentEventReconciler({
    agentHistoryCache: harness.agentHistoryCache,
    events: harness.agentEvents,
    scopes: harness.agentOrchestratorScopes,
    sessionList: async () => [session],
    clientFactory: () =>
      ({
        close: vi.fn(),
        sessionSnapshot: async () => ({ snapshot: { panes: [] } }),
      }) as never,
  });
}

describe("agent history cache TTL reconcile", () => {
  test("deletes expired rows and missing sources while keeping fresh existing rows", async () => {
    const harness = setup();
    const freshPath = sourceFile(harness, "fresh.jsonl");
    const expiredPath = sourceFile(harness, "expired.jsonl");
    const missingPath = join(dirname(harness.dbPath), "missing.jsonl");
    putCache(harness, { sourcePath: freshPath });
    putCache(harness, { sourcePath: expiredPath });
    putCache(harness, { sourcePath: missingPath });
    harness.sqlite
      .prepare("update agent_history_cache set updated_at = ? where source_path = ?")
      .run(Date.now() - HISTORY_CACHE_TTL_MS - 1_000, expiredPath);

    const result = await reconciler(harness).reconcile();
    expect(result.historyCacheExpired).toBe(1);
    expect(result.historyCacheMissing).toBe(1);
    expect(
      harness.sqlite
        .prepare("select source_path from agent_history_cache order by source_path")
        .all(),
    ).toEqual([{ source_path: freshPath }]);
  });

  test("keeps an opencode-sqlite cache key whose backing file still exists", async () => {
    const harness = setup();
    const backing = sourceFile(harness, "opencode.db");
    const cacheKey = `${backing}#session=sess-1`;
    putCache(harness, { sourcePath: cacheKey });
    const result = await reconciler(harness).reconcile();
    expect(result.historyCacheExpired).toBe(0);
    expect(result.historyCacheMissing).toBe(0);
    expect(harness.sqlite.prepare("select source_path from agent_history_cache").all()).toEqual([
      { source_path: cacheKey },
    ]);
  });
});

describe("AgentHistoryCacheStore cleanup", () => {
  test("deleteOlderThan removes only stale updated_at rows", () => {
    const harness = setup();
    const stale = putCache(harness, { sourcePath: "/tmp/stale.jsonl" });
    const fresh = putCache(harness, { sourcePath: "/tmp/fresh.jsonl" });
    harness.sqlite
      .prepare("update agent_history_cache set updated_at = ? where id = ?")
      .run(Date.now() - 2_000, stale.id);
    expect(harness.agentHistoryCache.deleteOlderThan(1_000)).toBe(1);
    expect(harness.sqlite.prepare("select id from agent_history_cache").all()).toEqual([
      { id: fresh.id },
    ]);
  });

  test("listSourcePaths and deleteBySourcePaths operate on the given set", () => {
    const harness = setup();
    putCache(harness, { sourcePath: "/tmp/a.jsonl" });
    putCache(harness, { sourcePath: "/tmp/b.jsonl", formatterVersion: "v1" });
    putCache(harness, { sourcePath: "/tmp/b.jsonl", formatterVersion: "v2" });
    putCache(harness, { sourcePath: "/tmp/c.jsonl" });
    expect(harness.agentHistoryCache.listSourcePaths()).toEqual([
      "/tmp/a.jsonl",
      "/tmp/b.jsonl",
      "/tmp/c.jsonl",
    ]);
    expect(harness.agentHistoryCache.deleteBySourcePaths([])).toBe(0);
    expect(
      harness.agentHistoryCache.deleteBySourcePaths(["/tmp/b.jsonl", "/tmp/missing.jsonl"]),
    ).toBe(2);
    expect(harness.agentHistoryCache.listSourcePaths()).toEqual(["/tmp/a.jsonl", "/tmp/c.jsonl"]);
  });
});
