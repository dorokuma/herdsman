import { cpSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import { REDELIVERY_FRESHNESS_MS } from "@/db/agent-events.js";
import { applyMigrations } from "@/db/apply-migrations.js";
import { openSqlite } from "@/db/client.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import {
  cleanupTempDirs,
  openObservabilityDbHarness,
  openObservabilityDbHarnessAt,
} from "./observability-db-harness.js";

const tempDirs: string[] = [];
afterEach(() => {
  cleanupTempDirs();
  for (const dir of tempDirs.splice(0)) rmSync(dir, { force: true, recursive: true });
});

function appendEvent(
  harness: ReturnType<typeof openObservabilityDbHarness>,
  terminalId = "term-agent",
) {
  const agent = harness.agents.listForHerdrSession("default")[0];
  if (!agent) throw new Error("Expected indexed agent");
  return harness.agentEvents.append({
    agentId: agent.id,
    herdrSessionName: "default",
    paneId: "wA:p1",
    payload: { ok: true },
    terminalId,
    type: "agent.done",
    workspaceId: "wA",
  });
}
function prepareHarness() {
  const harness = openObservabilityDbHarness();
  harness.herdrSessions.upsertRunning({
    name: "default",
    sessionDir: "/tmp/herdr",
    socketPath: "/tmp/herdr.sock",
  });
  harness.agents.replaceForSession({
    herdrSessionName: "default",
    agents: [
      {
        agent: "codex",
        agent_status: "working",
        pane_id: "wA:p1",
        terminal_id: "term-agent",
        workspace_id: "wA",
      },
    ],
  });
  return harness;
}

describe("agent event delivery lifecycle", () => {
  test("migration backfills legacy deliverable values into status and reason", () => {
    const dir = mkdtempSync(join(tmpdir(), "herdsman-migrations-"));
    tempDirs.push(dir);
    const migrations = join(dir, "drizzle");
    cpSync("drizzle", migrations, { recursive: true });
    rmSync(join(migrations, "0007_moaning_guardian.sql"));
    rmSync(join(migrations, "meta", "0007_snapshot.json"));
    const journalPath = join(migrations, "meta", "_journal.json");
    const journal = JSON.parse(readFileSync(journalPath, "utf8")) as { entries: unknown[] };
    journal.entries = journal.entries.slice(0, 7);
    writeFileSync(journalPath, JSON.stringify(journal));
    const { sqlite } = openSqlite(join(dir, "state.db"));
    applyMigrations(sqlite, { migrationsFolder: migrations });
    sqlite
      .prepare(
        "insert into herdr_sessions (name, running, session_dir, socket_path, updated_at) values (?, 1, ?, ?, ?)",
      )
      .run("default", "/tmp", "/tmp/sock", Date.now());
    sqlite
      .prepare(
        "insert into agent_events (herdr_session_name, payload_json, deliverable, terminal_id, type, created_at) values (?, ?, ?, ?, ?, ?)",
      )
      .run("default", "{}", 1, "term-a", "agent.done", Date.now());
    sqlite
      .prepare(
        "insert into agent_events (herdr_session_name, payload_json, deliverable, terminal_id, type, created_at) values (?, ?, ?, ?, ?, ?)",
      )
      .run("default", "{}", 0, "term-b", "agent.done", Date.now());
    for (const statement of readFileSync("drizzle/0007_moaning_guardian.sql", "utf8").split(
      "--> statement-breakpoint",
    )) {
      sqlite.exec(statement);
    }
    expect(
      sqlite
        .prepare("select deliverable, status, invalidated_reason from agent_events order by id")
        .all(),
    ).toEqual([
      { deliverable: 1, status: "pending", invalidated_reason: null },
      { deliverable: 0, status: "invalidated", invalidated_reason: "LEGACY_DELIVERABLE_FALSE" },
    ]);
    sqlite.close();
  });

  test("acking a cursor marks older pending rows in the scope as acked", () => {
    const harness = prepareHarness();
    const low = appendEvent(harness);
    const cursor = appendEvent(harness);
    const high = appendEvent(harness);
    harness.agentEvents.markAcked(cursor.id, { herdrSessionName: "default", workspaceId: "wA" });
    expect(harness.sqlite.prepare("select id, status from agent_events order by id").all()).toEqual(
      [
        { id: low.id, status: "acked" },
        { id: cursor.id, status: "acked" },
        { id: high.id, status: "pending" },
      ],
    );
  });
  test("two connections reserve the same pending batch without duplicate delivery", () => {
    const harness = prepareHarness();
    const event = appendEvent(harness);
    expect(harness.agentEvents.reservePending("term-owner-a").map((row) => row.id)).toEqual([
      event.id,
    ]);
    expect(harness.agentEvents.reservePending("term-owner-b").map((row) => row.id)).toEqual([]);
    expect(
      harness.sqlite
        .prepare("select status, delivery_attempts from agent_events where id = ?")
        .get(event.id),
    ).toEqual({ status: "delivered", delivery_attempts: 1 });
  });

  test("two independent SQLite connections concurrently reserve each pending event once", async () => {
    const first = prepareHarness();
    const second = openObservabilityDbHarnessAt(first.dbPath);
    try {
      for (let index = 0; index < 12; index += 1) appendEvent(first, `term-${index}`);
      const [a, b] = await Promise.all([
        Promise.resolve().then(() => first.agentEvents.reservePending("owner-a", 100)),
        Promise.resolve().then(() => second.agentEvents.reservePending("owner-b", 100)),
      ]);
      expect(new Set([...a, ...b].map((event) => event.id)).size).toBe(12);
      expect(a.length + b.length).toBe(12);
      expect(
        first.sqlite
          .prepare(
            "select count(*) as count, max(delivery_attempts) as attempts from agent_events where status = 'delivered'",
          )
          .get(),
      ).toEqual({ count: 12, attempts: 1 });
    } finally {
      second.sqlite.close();
      first.sqlite.close();
    }
  });
  test("listAfter excludes delivered rows before the cursor and filters scope", () => {
    const harness = prepareHarness();
    const delivered = appendEvent(harness);
    const pending = appendEvent(harness, "term-agent-2");
    harness.sqlite
      .prepare(
        "update agent_events set status = 'delivered', delivered_to_terminal_id = ? where id = ?",
      )
      .run("term-owner", delivered.id);
    expect(
      harness.agentEvents
        .listAfter({
          afterEventId: delivered.id,
          ownerTerminalId: "term-owner",
          herdrSessionName: "default",
          workspaceId: "wA",
        })
        .map((row) => row.id),
    ).toEqual([pending.id]);
    expect(
      harness.agentEvents.listAfter({
        afterEventId: 0,
        ownerTerminalId: "term-owner",
        herdrSessionName: "other",
        workspaceId: "wA",
      }),
    ).toEqual([]);
    expect(
      harness.agentEvents.listAfter({
        afterEventId: 0,
        ownerTerminalId: "term-owner",
        herdrSessionName: "default",
        workspaceId: "other",
      }),
    ).toEqual([]);
  });
  test("repeated reserve for the same delivered event is idempotent", () => {
    const harness = prepareHarness();
    const event = appendEvent(harness);
    expect(harness.agentEvents.reservePending("term-owner")).toHaveLength(1);
    const before = harness.agentEvents.get(event.id);
    expect(harness.agentEvents.reservePending("term-owner")).toHaveLength(1);
    expect(harness.agentEvents.get(event.id)).toEqual(before);
  });

  test("does not re-list a delivered event after the redelivery freshness window", () => {
    const harness = prepareHarness();
    const event = appendEvent(harness);
    expect(harness.agentEvents.reservePending("term-owner")).toHaveLength(1);
    // Age the delivery beyond the redelivery freshness window.
    harness.sqlite
      .prepare("update agent_events set last_attempt_at = ? where id = ?")
      .run(Date.now() - REDELIVERY_FRESHNESS_MS - 1, event.id);
    // The same terminal is no longer re-served the stale delivered event.
    expect(harness.agentEvents.reservePending("term-owner")).toEqual([]);
    expect(
      harness.agentEvents.listAfter({
        afterEventId: 0,
        ownerTerminalId: "term-owner",
        herdrSessionName: "default",
        workspaceId: "wA",
      }),
    ).toEqual([]);
    // A crash-redelivery path still works: an event reset to pending is
    // reserved again (the freshness window only bounds the delivered re-list).
    harness.sqlite
      .prepare(
        "update agent_events set status = 'pending', deliverable = 1, delivered_to_terminal_id = null, last_attempt_at = null where id = ?",
      )
      .run(event.id);
    expect(harness.agentEvents.reservePending("term-owner").map((row) => row.id)).toEqual([
      event.id,
    ]);
  });

  test("reclaims timed-out deliveries and fails events at the attempt limit", () => {
    const harness = prepareHarness();
    const retry = appendEvent(harness);
    const exhausted = appendEvent(harness, "term-agent-2");
    harness.sqlite
      .prepare(
        "update agent_events set status = 'delivered', deliverable = 1, delivery_attempts = 1, last_attempt_at = ? where id = ?",
      )
      .run(Date.now() - 100_000, retry.id);
    harness.sqlite
      .prepare(
        "update agent_events set status = 'delivered', deliverable = 1, delivery_attempts = 10, last_attempt_at = ? where id = ?",
      )
      .run(Date.now() - 100_000, exhausted.id);
    expect(harness.agentEvents.reclaimDelivered(60_000)).toBe(2);
    expect(harness.agentEvents.get(retry.id)).toMatchObject({
      status: "pending",
      deliverable: 1,
      deliveryAttempts: 1,
    });
    expect(harness.agentEvents.get(exhausted.id)).toMatchObject({
      status: "failed",
      deliverable: 0,
      deliveryAttempts: 10,
      lastFailureCode: "DELIVERY_ATTEMPTS_EXCEEDED",
    });
    expect(harness.agentEvents.reclaimDelivered(60_000)).toBe(0);
  });

  test("keeps a delivered event with the owner while the pane is open and the scope is still held", () => {
    const harness = prepareHarness();
    harness.agentOrchestratorScopes.claim({
      herdrSessionName: "default",
      workspaceId: "wA",
      ackedEventId: 0,
      paneId: "wA:owner",
      terminalId: "term-owner",
    });
    const event = appendEvent(harness);
    harness.agentEvents.reservePending("term-owner");
    harness.sqlite
      .prepare("update agent_events set last_attempt_at = ? where id = ?")
      .run(Date.now() - 100_000, event.id);
    expect(harness.agentEvents.reclaimDelivered(60_000)).toBe(0);
    expect(harness.agentEvents.get(event.id)).toMatchObject({
      status: "delivered",
      deliverable: 1,
      deliveryAttempts: 1,
      deliveredToTerminalId: "term-owner",
      invalidatedReason: null,
    });
  });

  test("invalidates a delivered event whose agent pane is closed instead of redelivering it", () => {
    const harness = prepareHarness();
    const event = appendEvent(harness);
    harness.agentEvents.reservePending("term-owner");
    harness.sqlite
      .prepare("update agent_events set last_attempt_at = ? where id = ?")
      .run(Date.now() - 100_000, event.id);
    const agent = harness.agents.listForHerdrSession("default")[0];
    if (!agent) throw new Error("Expected indexed agent");
    // Simulate the pane being closed/retired: the agents row disappears.
    harness.sqlite.prepare("delete from agents where id = ?").run(agent.id);
    expect(harness.agentEvents.reclaimDelivered(60_000)).toBe(1);
    expect(harness.agentEvents.get(event.id)).toMatchObject({
      status: "invalidated",
      deliverable: 0,
      invalidatedReason: "PANE_CLOSED_RECLAIM",
    });
    expect(harness.agentEvents.reservePending("term-owner")).toEqual([]);
    expect(harness.agentEvents.reclaimDelivered(60_000)).toBe(0);
  });

  test("reclaims a delivered event when the scope moved to another terminal", () => {
    const harness = prepareHarness();
    harness.agentOrchestratorScopes.claim({
      herdrSessionName: "default",
      workspaceId: "wA",
      ackedEventId: 0,
      paneId: "wA:owner",
      terminalId: "term-new-owner",
    });
    const event = appendEvent(harness);
    harness.agentEvents.reservePending("term-old-owner");
    harness.sqlite
      .prepare("update agent_events set last_attempt_at = ? where id = ?")
      .run(Date.now() - 100_000, event.id);
    expect(harness.agentEvents.reclaimDelivered(60_000)).toBe(1);
    expect(harness.agentEvents.get(event.id)).toMatchObject({
      status: "pending",
      deliverable: 1,
      deliveredToTerminalId: null,
    });
  });

  test("reclaims a delivered event immediately when the delivery terminal has no active connection", () => {
    const harness = prepareHarness();
    harness.agentOrchestratorScopes.claim({
      herdrSessionName: "default",
      workspaceId: "wA",
      ackedEventId: 0,
      paneId: "wA:owner",
      terminalId: "term-owner",
    });
    const event = appendEvent(harness);
    harness.agentEvents.reservePending("term-owner");
    // last_attempt_at is very recent: without the connection check the event
    // would linger as delivered until the timeout.
    harness.sqlite
      .prepare("update agent_events set last_attempt_at = ? where id = ?")
      .run(Date.now(), event.id);
    expect(harness.agentEvents.reclaimDelivered(60_000, { isTerminalConnected: () => false })).toBe(
      1,
    );
    expect(harness.agentEvents.get(event.id)).toMatchObject({
      status: "pending",
      deliverable: 1,
      deliveryAttempts: 1,
      deliveredToTerminalId: null,
      nextAttemptAt: null,
    });
    // Control: with the terminal connected and a fresh last_attempt_at, the
    // delivered event is kept even when the timeout has not elapsed.
    harness.agentEvents.reservePending("term-owner");
    expect(harness.agentEvents.reclaimDelivered(60_000, { isTerminalConnected: () => true })).toBe(
      0,
    );
    expect(harness.agentEvents.get(event.id)).toMatchObject({
      status: "delivered",
      deliveredToTerminalId: "term-owner",
    });
  });

  test("isTerminalConnected is judged against the delivered event's own session, not the caller's", () => {
    const harness = prepareHarness();
    const orchestrator = new AgentOrchestratorService({
      agentEvents: harness.agentEvents,
      agents: harness.agents,
      scopes: harness.agentOrchestratorScopes,
    });
    harness.agentOrchestratorScopes.claim({
      herdrSessionName: "default",
      workspaceId: "wA",
      ackedEventId: 0,
      paneId: "wA:owner",
      terminalId: "term-owner",
    });
    const event = appendEvent(harness);
    harness.agentEvents.reservePending("term-owner");
    // last_attempt_at is fresh: only the connection-check shortcut can touch
    // this delivered row.
    harness.sqlite
      .prepare("update agent_events set last_attempt_at = ? where id = ?")
      .run(Date.now(), event.id);

    // Session B calls pending(); its isTerminalConnected only recognizes
    // "session A (default) + term-owner" as connected. The delivered row
    // belongs to session A, so it must NOT be reclaimed: the old closure over
    // the caller's session misjudged it as offline and reclaimed it.
    orchestrator.pending({
      herdrSessionName: "other",
      workspaceId: "wB",
      terminalId: "term-b",
      isTerminalConnected: (input) =>
        input.herdrSessionName === "default" && input.terminalId === "term-owner",
    });
    expect(harness.agentEvents.get(event.id)).toMatchObject({
      status: "delivered",
      deliveredToTerminalId: "term-owner",
    });

    // Control: the terminal is offline in the event's OWN session -> the row
    // is reclaimed immediately.
    orchestrator.pending({
      herdrSessionName: "other",
      workspaceId: "wB",
      terminalId: "term-b",
      isTerminalConnected: () => false,
    });
    expect(harness.agentEvents.get(event.id)).toMatchObject({
      status: "pending",
      deliverable: 1,
      deliveredToTerminalId: null,
    });
  });

  test("monotonic logical clock survives a wall-clock rollback and a store restart", () => {
    const harness = prepareHarness();
    const realNow = Date.now();
    const event = appendEvent(harness);
    harness.agentEvents.reservePending("term-owner");
    expect(harness.agentEvents.get(event.id)).toMatchObject({ status: "delivered" });

    // Roll the wall clock back by one hour: the logical clock must keep the
    // delivered event inside its freshness window instead of aging it out.
    vi.useFakeTimers();
    vi.setSystemTime(realNow - 60 * 60 * 1_000);
    try {
      expect(
        harness.agentEvents.listAfter({
          afterEventId: 0,
          herdrSessionName: "default",
          ownerTerminalId: "term-owner",
          workspaceId: "wA",
        }),
      ).toHaveLength(1);
      // A brand-new store over the same database reads the persisted logical
      // clock, so a daemon restart cannot roll the clock back either.
      const restarted = openObservabilityDbHarnessAt(harness.dbPath);
      try {
        expect(
          restarted.agentEvents.listAfter({
            afterEventId: 0,
            herdrSessionName: "default",
            ownerTerminalId: "term-owner",
            workspaceId: "wA",
          }),
        ).toHaveLength(1);
        expect(
          restarted.sqlite
            .prepare("select value from daemon_meta where key = 'logical_now_ms'")
            .get(),
        ).toMatchObject({ value: String(realNow) });
      } finally {
        restarted.sqlite.close();
      }
    } finally {
      vi.useRealTimers();
    }
  });

  test("keeps deliverable and status equivalent after every transition", () => {
    const harness = prepareHarness();
    const event = appendEvent(harness);
    const assertMirror = () => {
      const row = harness.sqlite
        .prepare("select status, deliverable from agent_events where id = ?")
        .get(event.id) as { status: string; deliverable: number };
      expect(row.deliverable).toBe(["pending", "delivered"].includes(row.status) ? 1 : 0);
    };
    assertMirror();
    harness.agentEvents.reservePending("term-owner");
    assertMirror();
    harness.agentEvents.markAcked(event.id);
    assertMirror();
    for (const status of ["invalidated", "failed"]) {
      harness.sqlite
        .prepare("update agent_events set status = ?, deliverable = 0 where id = ?")
        .run(status, event.id);
      assertMirror();
    }
  });

  test("terminal states are excluded from pending and nextDeliverableAfter candidates", () => {
    const harness = prepareHarness();
    const ids = [
      appendEvent(harness),
      appendEvent(harness, "term-agent-2"),
      appendEvent(harness, "term-agent-3"),
      appendEvent(harness, "term-agent-4"),
    ];
    if (ids.some((id) => !id)) throw new Error("Expected four events");
    const [pending, acked, invalidated, failed] = ids;
    if (!pending || !acked || !invalidated || !failed) throw new Error("Expected four events");
    harness.sqlite
      .prepare("update agent_events set status = 'acked', deliverable = 0 where id = ?")
      .run(acked.id);
    harness.sqlite
      .prepare("update agent_events set status = 'invalidated', deliverable = 0 where id = ?")
      .run(invalidated.id);
    harness.sqlite
      .prepare("update agent_events set status = 'failed', deliverable = 0 where id = ?")
      .run(failed.id);
    expect(
      harness.agentEvents
        .listAfter({ herdrSessionName: "default", workspaceId: "wA", afterEventId: 0 })
        .map((row) => row.id),
    ).toEqual([pending.id]);
    expect(
      harness.agentEvents.nextDeliverableAfter({
        afterEventId: 0,
        herdrSessionName: "default",
        ownerTerminalId: "term-owner",
        workspaceId: "wA",
        getAgent: () => harness.agents.listForHerdrSession("default")[0],
      }),
    ).toMatchObject({ id: pending.id });
  });
});

describe("empty database migration chain", () => {
  test("applies 0000 through 0008 and matches schema columns", () => {
    const dir = mkdtempSync(join(tmpdir(), "herdsman-empty-chain-"));
    tempDirs.push(dir);
    const { sqlite } = openSqlite(join(dir, "state.db"));
    applyMigrations(sqlite, { migrationsFolder: "drizzle" });
    const columns = sqlite
      .prepare("pragma table_info(agent_events)")
      .all()
      .map((row) => (row as { name: string }).name);
    expect(columns).toEqual(
      expect.arrayContaining([
        "deliverable",
        "status",
        "delivery_attempts",
        "last_attempt_at",
        "next_attempt_at",
        "last_failure_code",
        "invalidated_reason",
        "delivered_to_terminal_id",
      ]),
    );
    expect(sqlite.prepare("select count(*) as count from __drizzle_migrations").get()).toEqual({
      count: 10,
    });
    const journal = JSON.parse(readFileSync("drizzle/meta/_journal.json", "utf8")) as {
      version: string;
      entries: Array<{ tag: string; version: string }>;
    };
    expect(
      journal.entries.every(
        (entry, index) => entry.version === (index < 6 || index === 8 || index === 9 ? "6" : "7"),
      ),
    ).toBe(true);
    expect(journal.entries.find((entry) => entry.tag === "0007_moaning_guardian")?.version).toBe(
      "7",
    );
    expect(journal.entries.find((entry) => entry.tag === "0008_pane_tombstones")?.version).toBe(
      "6",
    );
    expect(journal.entries.find((entry) => entry.tag === "0009_high_lake")?.version).toBe("6");
    sqlite.close();
  });
});
