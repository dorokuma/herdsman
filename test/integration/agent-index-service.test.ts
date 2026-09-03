import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AgentHistoryService } from "@/agent-history/service.js";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { AgentIndexService, type StatusEventPlan } from "@/observability/agent-index-service.js";
import { TurnCompletionRegistry } from "@/observability/turn-completion.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

const doneEvent = {
  event: { agent_status: "done", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
  herdrSessionName: "default",
  sessionDir: "/tmp/herdr",
  socketPath: "/tmp/herdr.sock",
};

afterEach(cleanupTempDirs);

describe("AgentIndexService", () => {
  test("refreshes only missing, revised, or identity-changed agents and overlays pane revisions", async () => {
    const harness = openObservabilityDbHarness();
    const calls: string[] = [];
    let current = twoAgents();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return current;
        },
      }),
      history: history((agent) => calls.push(agent.agent ?? "unknown")),
      stores: harness,
    });
    const refresh = () => index.refreshHerdrSession(sessionInput());

    const first = await refresh();
    expect(calls).toEqual(["claude", "codex"]);
    expect(first.contextChangedScopes).toEqual([
      { herdrSessionName: "default", workspaceId: "wJ" },
    ]);

    calls.length = 0;
    await refresh();
    expect(calls).toEqual([]);

    current = twoAgents({ claudeRevision: 11 });
    await refresh();
    expect(calls).toEqual(["claude"]);

    calls.length = 0;
    current = twoAgents({ codexCwd: "/other", claudeRevision: 11 });
    await refresh();
    expect(calls).toEqual(["codex"]);

    calls.length = 0;
    current = twoAgents({
      claudePane: "wK:p2",
      claudeWorkspace: "wK",
      claudeRevision: 11,
      codexCwd: "/other",
    });
    const moved = await refresh();
    expect(calls).toEqual([]);
    expect(moved.contextChangedScopes).toEqual([
      { herdrSessionName: "default", workspaceId: "wJ" },
      { herdrSessionName: "default", workspaceId: "wK" },
    ]);
    expect(
      harness.agents.findByPane({ herdrSessionName: "default", paneId: "wK:p2" })?.paneRevision,
    ).toBe(11);

    calls.length = 0;
    current = twoAgents({
      claudePane: "wK:p2",
      claudeRevision: 11,
      claudeTerminal: null,
      claudeWorkspace: "wK",
      codexCwd: "/other",
    });
    await refresh();
    expect(calls).toEqual(["claude"]);

    calls.length = 0;
    current = twoAgents({
      claudePane: "wK:p2",
      claudeRevision: 11,
      claudeTerminal: "term_claude",
      claudeWorkspace: "wJ",
      codexCwd: "/other",
    });
    const restoredTerminal = await refresh();
    expect(restoredTerminal.contextChangedScopes).toEqual([
      { herdrSessionName: "default", workspaceId: "wJ" },
      { herdrSessionName: "default", workspaceId: "wK" },
    ]);
    harness.sqlite.close();
  });

  test("publishes name-only changes without reparsing history and snapshots names in events", async () => {
    const harness = openObservabilityDbHarness();
    const calls: string[] = [];
    let current = oneAgent("working", 10, "codex", "reviewer");
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return current;
        },
      }),
      history: history((agent) => calls.push(agent.agent ?? "unknown")),
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    calls.length = 0;
    current = oneAgent("working", 10, "codex", "implementer");
    const renamed = await index.refreshHerdrSession(sessionInput());

    expect(calls).toEqual([]);
    expect(renamed.contextChangedScopes).toEqual([
      { herdrSessionName: "default", workspaceId: "wJ" },
    ]);
    expect(renamed.agents[0]).toMatchObject({
      agent: "codex",
      name: "implementer",
      terminalId: "term_claude",
    });

    const status = await index.handleHerdrEvent({
      event: { agent_status: "done", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });
    expect(status.events).toContainEqual(
      expect.objectContaining({
        payload: expect.objectContaining({
          agent: "codex",
          name: "implementer",
          to: "done",
        }),
        type: "agent.done",
      }),
    );
    harness.sqlite.close();
  });

  test("refreshes status immediately and synthesizes an unknown-pane transition exactly once", async () => {
    const harness = openObservabilityDbHarness();
    const calls: string[] = [];
    const current = oneAgent("working", 10);
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return current;
        },
      }),
      history: history((agent) => calls.push(agent.agent ?? "unknown"), "final result"),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    calls.length = 0;

    const status = await index.handleHerdrEvent({
      event: { agent_status: "done", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });
    expect(calls).toEqual(["claude"]);
    expect(status).toMatchObject({
      contextChangedScopes: [{ herdrSessionName: "default", workspaceId: "wJ" }],
      events: [
        { compactHistory: { lastAssistantMessage: { text: "final result" } }, type: "agent.done" },
      ],
    });

    calls.length = 0;
    const duplicate = await index.handleHerdrEvent({
      event: { agent_status: "done", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });
    expect(calls).toEqual(["claude"]);
    expect(duplicate).toEqual({ contextChangedScopes: [], events: [] });

    const unknownHarness = openObservabilityDbHarness();
    const unknown = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("idle", 10);
        },
      }),
      history: history(() => undefined),
      stores: unknownHarness,
    });
    const recovered = await unknown.handleHerdrEvent({
      event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });
    expect(recovered.events).toEqual([
      expect.objectContaining({
        payload: expect.objectContaining({ from: "unknown", name: null, to: "idle" }),
        type: "agent.idle",
      }),
    ]);
    expect(
      unknownHarness.agentEvents.listAfter({ herdrSessionName: "default", workspaceId: "wJ" }),
    ).toHaveLength(2);
    harness.sqlite.close();
    unknownHarness.sqlite.close();
  });

  test("S3: waits for a new history ref instead of advancing on a repeated old ref", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 50 });
    const oldHistory = {
      ...emptyCompactHistory("pi-jsonl"),
      lastAssistantMessage: { ref: "x#entry=1", text: "old", timestamp: null },
      messageCount: 2,
    };
    let calls = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "pi");
        },
      }),
      history: {
        async resolveCompactHistory() {
          calls += 1;
          // The first two resolutions return the baseline value again: the
          // same ref must NOT count as history advancing. Only the third
          // resolution reports a new ref (text may stay identical).
          if (calls <= 2) {
            return { compactHistory: oldHistory, historyRef: null, sourceFingerprint: null };
          }
          return {
            compactHistory: {
              ...oldHistory,
              lastAssistantMessage: { ref: "x#entry=2", text: "old", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
      turnCompletions: registry,
    });

    await index.refreshHerdrSession(sessionInput());
    calls = 0;
    const result = await index.handleHerdrEvent(doneEvent);
    expect(calls).toBeGreaterThanOrEqual(3);
    expect(result.events).toContainEqual(
      expect.objectContaining({
        compactHistory: expect.objectContaining({
          lastAssistantMessage: expect.objectContaining({ ref: "x#entry=2" }),
        }),
        type: "agent.done",
      }),
    );
    harness.sqlite.close();
  });

  test("recovered path emits exactly one agent.done when refresh and event plans overlap", async () => {
    const harness = openObservabilityDbHarness();
    let current = oneAgent("working", 10);
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return current;
        },
      }),
      history: history(() => undefined, "final result"),
      stores: harness,
    });
    // Index the agent without a generation first.
    await index.refreshHerdrSession(sessionInput());

    // The pane is re-created with generation g1 and already terminal (done);
    // the event arrives with the generation, so the DB row (generation-less,
    // working) does not match and the daemon recovers via an internal refresh.
    // The refresh-side plan (working -> done) and the event-side plan
    // (unknown -> done) overlap; the equivalent guard must keep exactly one.
    current = snapshot(
      [
        agent({
          agent_status: "done",
          pane_generation: "g1",
          pane_id: "wJ:p2",
          revision: 11,
          terminal_id: "term_claude",
          workspace_id: "wJ",
        }),
      ],
      [{ pane_id: "wJ:p2", revision: 11 }],
    );
    const result = await index.handleHerdrEvent({
      event: {
        agent_status: "done",
        pane_generation: "g1",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...sessionInput(),
    });
    expect(result.events.filter((event) => event.type === "agent.done")).toHaveLength(1);
    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.filter((event) => event.type === "agent.done")).toHaveLength(1);
    harness.sqlite.close();
  });

  test("deduplicates a refresh transition repeated by a realtime event in the same session", async () => {
    const harness = openObservabilityDbHarness();
    let current = oneAgent("idle", 10);
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return current;
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    current = oneAgent("working", 11);
    await index.refreshHerdrSession(sessionInput());
    await index.handleHerdrEvent({
      event: { agent_status: "working", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });

    const events = harness.agentEvents
      .listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      })
      .filter((event) => event.type === "agent.status.changed");
    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({
      payload: expect.objectContaining({ from: "idle", to: "working" }),
      type: "agent.status.changed",
    });
    harness.sqlite.close();
  });

  test("preserves opposite status transitions repeated within the same second", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("idle", 10);
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    for (const status of ["working", "idle", "working"] as const) {
      await index.handleHerdrEvent({
        event: { agent_status: status, pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInput(),
      });
    }

    const events = harness.agentEvents
      .listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      })
      .filter((event) => event.type === "agent.status.changed");
    expect(events).toHaveLength(3);
    expect(
      events.map((event) => {
        const payload = event.payload as { from: string; to: string };
        return { from: payload.from, to: payload.to };
      }),
    ).toEqual([
      { from: "idle", to: "working" },
      { from: "working", to: "idle" },
      { from: "idle", to: "working" },
    ]);
    harness.sqlite.close();
  });

  test("coalesces same-epoch refreshes and queues a later refresh after a status mutation", async () => {
    const harness = openObservabilityDbHarness();
    let snapshots = 0;
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          snapshots += 1;
          await gate;
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    const first = index.refreshHerdrSession(sessionInput());
    const same = index.refreshHerdrSession(sessionInput());
    expect(same).toBe(first);
    const status = index.handleHerdrEvent({
      event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });
    const later = index.refreshHerdrSession(sessionInput());
    expect(later).not.toBe(first);
    release();
    await Promise.all([first, same, status, later]);
    expect(snapshots).toBe(2);
    expect(
      harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" })?.agentStatus,
    ).toBe("working");
    harness.sqlite.close();
  });

  test("applies a Pi session hint registered before the agent is indexed", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("idle", 10, "pi");
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    const allowedPath = join("/tmp/pi-role-sessions", `early-${Date.now()}.jsonl`);
    mkdirSync("/tmp/pi-role-sessions", { recursive: true });
    writeFileSync(allowedPath, JSON.stringify({ cwd: "/tmp" }));
    const sessionRef = {
      agent: "pi" as const,
      kind: "path" as const,
      source: "herdr:pi",
      value: allowedPath,
    };

    await expect(
      index.registerPiSessionRef({
        herdrSessionName: "default",
        sessionRef,
        terminalId: "term_claude",
      }),
    ).resolves.toEqual({ agent: undefined, contextChangedScopes: [] });
    const refreshed = await index.refreshHerdrSession(sessionInput());

    expect(refreshed.agents[0]?.agentSession).toEqual(sessionRef);
    expect(
      harness.sqlite
        .prepare("select agent_session_hint_json from agents where terminal_id = ?")
        .get("term_claude"),
    ).toEqual({ agent_session_hint_json: JSON.stringify(sessionRef) });
    harness.sqlite.close();
  });

  test("serializes Pi session hints with refreshes and preserves the effective ref", async () => {
    const harness = openObservabilityDbHarness();
    let snapshots = 0;
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          snapshots += 1;
          await gate;
          return oneAgent("idle", 10, "pi");
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    const sessionRef = {
      agent: "pi" as const,
      kind: "path" as const,
      source: "herdr:pi",
      value: "/tmp/pi-role-sessions/serialized-pi-session.jsonl",
    };
    mkdirSync("/tmp/pi-role-sessions", { recursive: true });
    writeFileSync(sessionRef.value, JSON.stringify({ cwd: "/tmp" }));

    const first = index.refreshHerdrSession(sessionInput());
    const registration = index.registerPiSessionRef({
      herdrSessionName: "default",
      sessionRef,
      terminalId: "term_claude",
    });
    const later = index.refreshHerdrSession(sessionInput());
    expect(later).not.toBe(first);

    release();
    const [, registered] = await Promise.all([first, registration, later]);
    expect(snapshots).toBe(2);
    expect(registered.agent?.agentSession).toEqual(sessionRef);
    expect(
      harness.agents.findByTerminal({
        herdrSessionName: "default",
        terminalId: "term_claude",
      })?.agentSession,
    ).toEqual(sessionRef);
    harness.sqlite.close();
  });
});

function history(onResolve: (agent: { agent: string | null }) => void, assistantText = "result") {
  return {
    async resolveCompactHistory(agent: { agent: string | null }) {
      onResolve(agent);
      return {
        compactHistory: {
          ...emptyCompactHistory("claude-jsonl"),
          lastAssistantMessage: { ref: "history", text: assistantText, timestamp: null },
        },
        historyRef: null,
        sourceFingerprint: null,
      };
    },
  } as unknown as AgentHistoryService;
}

function sessionInput() {
  return { herdrSessionName: "default", sessionDir: "/tmp/herdr", socketPath: "/tmp/herdr.sock" };
}

function oneAgent(
  status: string,
  revision: number,
  agentKind = "claude",
  name: string | null = null,
) {
  return snapshot(
    [
      agent({
        agent: agentKind,
        agent_status: status,
        name,
        pane_id: "wJ:p2",
        revision,
        terminal_id: "term_claude",
        workspace_id: "wJ",
      }),
    ],
    [{ pane_id: "wJ:p2", revision }],
  );
}

function twoAgents(
  input: {
    claudePane?: string;
    claudeRevision?: number;
    claudeTerminal?: string | null;
    claudeWorkspace?: string;
    codexCwd?: string;
  } = {},
) {
  const claudePane = input.claudePane ?? "wJ:p2";
  const claudeRevision = input.claudeRevision ?? 10;
  const claudeWorkspace = input.claudeWorkspace ?? "wJ";
  const claudeTerminal = Object.hasOwn(input, "claudeTerminal")
    ? input.claudeTerminal
    : "term_claude";
  return snapshot(
    [
      agent({
        pane_id: claudePane,
        revision: undefined,
        terminal_id: claudeTerminal,
        workspace_id: claudeWorkspace,
      }),
      agent({
        agent: "codex",
        cwd: input.codexCwd ?? "/repo",
        pane_id: "wJ:p3",
        revision: 20,
        terminal_id: "term_codex",
        workspace_id: "wJ",
      }),
    ],
    [
      { pane_id: claudePane, revision: claudeRevision },
      { pane_id: "wJ:p3", revision: 20 },
    ],
  );
}

function agent(input: Record<string, unknown>) {
  return {
    agent: "claude",
    agent_status: "working",
    cwd: "/repo",
    foreground_cwd: "/repo",
    tab_id: "wJ:t1",
    ...input,
  };
}

function snapshot(agents: Record<string, unknown>[], panes: Record<string, unknown>[]) {
  return {
    snapshot: {
      agents,
      panes,
      tabs: [],
      workspaces: [
        { agent_status: "working", focused: true, label: "repo", workspace_id: "wJ" },
        { agent_status: "working", focused: false, label: "other", workspace_id: "wK" },
      ],
    },
  };
}

describe("AgentIndexService identity regressions (independent coverage)", () => {
  test("agentSession 由空变为存在时 identityChanged 为真并强制 discovery", async () => {
    const harness = openObservabilityDbHarness();
    const calls: Array<{ forceDiscovery?: boolean }> = [];
    let current = oneAgent("working", 10, "pi");
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return current;
        },
      }),
      history: {
        async resolveCompactHistory(
          _agent: Parameters<AgentHistoryService["resolveCompactHistory"]>[0],
          options: Parameters<AgentHistoryService["resolveCompactHistory"]>[1],
        ) {
          calls.push(options ?? {});
          return {
            compactHistory: { ...emptyCompactHistory("pi-jsonl"), lastAssistantMessage: null },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    calls.length = 0;
    current = snapshot(
      [
        agent({
          agent: "pi",
          agent_session: { agent: "pi", kind: "id", source: "herdr:pi", value: "role-session-1" },
          pane_id: "wJ:p2",
          revision: undefined,
          terminal_id: "term_claude",
          workspace_id: "wJ",
        }),
      ],
      [{ pane_id: "wJ:p2", revision: 11 }],
    );
    await index.refreshHerdrSession(sessionInput());
    expect(calls).toEqual([{ forceDiscovery: true }]);
    harness.sqlite.close();
  });
});

describe("AgentIndexService non-pi completed event generation", () => {
  test("agy working -> idle with empty assistant message suppresses agent.idle event generation", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "agy");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("antigravity-sqlite"),
              lastAssistantMessage: null,
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());

    const result = await index.handleHerdrEvent({
      event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });

    // agent.idle event is suppressed in returned events
    expect(result.events).toEqual([]);

    // agent.status.changed is still recorded in DB for history
    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.filter((e) => e.type === "agent.idle")).toHaveLength(0);
    expect(allEvents.filter((e) => e.type === "agent.status.changed")).toHaveLength(1);

    harness.sqlite.close();
  });

  test("agy working -> idle with non-empty assistant message generates agent.idle event", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "agy");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("antigravity-sqlite"),
              lastAssistantMessage: { ref: "history", text: "agy finished task", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());

    const result = await index.handleHerdrEvent({
      event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });

    expect(result.events).toEqual([
      expect.objectContaining({
        compactHistory: expect.objectContaining({
          lastAssistantMessage: expect.objectContaining({ text: "agy finished task" }),
        }),
        type: "agent.idle",
      }),
    ]);

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.filter((e) => e.type === "agent.idle")).toHaveLength(1);

    harness.sqlite.close();
  });

  test("agy working -> done with empty assistant message generates agent.done event", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "agy");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("antigravity-sqlite"),
              lastAssistantMessage: null,
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());

    const result = await index.handleHerdrEvent({
      event: { agent_status: "done", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });

    expect(result.events).toEqual([
      expect.objectContaining({
        type: "agent.done",
      }),
    ]);

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.filter((e) => e.type === "agent.done")).toHaveLength(1);

    harness.sqlite.close();
  });
});

describe("AgentIndexService status event plan drain resilience", () => {
  function openIndex(harness: ReturnType<typeof openObservabilityDbHarness>) {
    return new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
  }

  test("drain cancels a plan whose agent row is missing and still executes healthy rows", async () => {
    const harness = openObservabilityDbHarness();
    const index = openIndex(harness);
    await index.refreshHerdrSession(sessionInput());

    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected indexed agent");

    // Healthy row: the agent row exists, the plan drains to completion.
    const healthy = harness.statusEventPlans.insertPending({
      agentId: agent.id,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "history", text: "final answer", timestamp: null },
      },
      fromStatus: "working",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      toStatus: "done",
    });
    // Ghost row: neither findByPane nor agents.get can resolve it (get throws).
    const ghost = harness.statusEventPlans.insertPending({
      agentId: "ag_ghost",
      fromStatus: "working",
      herdrSessionName: "default",
      paneId: "ghost:p1",
      toStatus: "done",
    });

    // drainPendingPlans must never reject: a missing-agent row is cancelled
    // without poisoning the healthy rows.
    await expect(index.drainPendingPlans()).resolves.toBeUndefined();

    expect(harness.statusEventPlans.get(ghost.id).status).toBe("cancelled");
    expect(harness.statusEventPlans.get(healthy.id).status).toBe("completed");
    expect(harness.statusEventPlans.listUnfinished()).toEqual([]);
    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(events.filter((e) => e.type === "agent.done")).toHaveLength(1);
    harness.sqlite.close();
  });

  test("a runtime-failed plan is retried by the next drain and completes", async () => {
    const harness = openObservabilityDbHarness();
    const index = openIndex(harness);
    await index.refreshHerdrSession(sessionInput());

    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected indexed agent");
    const row = harness.statusEventPlans.insertPending({
      agentId: agent.id,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "history", text: "final answer", timestamp: null },
      },
      fromStatus: "working",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      toStatus: "done",
    });

    // First drain: the append blows up once; the row goes back to pending with
    // attempts=1 (markRetry path) and the drain still resolves.
    const append = vi.spyOn(harness.agentEvents, "append").mockImplementationOnce(() => {
      throw new Error("append boom");
    });
    await expect(index.drainPendingPlans()).resolves.toBeUndefined();
    expect(append).toHaveBeenCalledTimes(1);
    expect(harness.statusEventPlans.get(row.id)).toMatchObject({
      attempts: 1,
      lastError: "append boom",
      status: "pending",
    });

    // Second drain retries the same row and completes it: the retry pathway.
    await expect(index.drainPendingPlans()).resolves.toBeUndefined();
    expect(harness.statusEventPlans.get(row.id).status).toBe("completed");
    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(events.filter((e) => e.type === "agent.done")).toHaveLength(1);
    harness.sqlite.close();
  });

  test("executeStatusEventPlan skips inserting a plan when from equals to", async () => {
    const harness = openObservabilityDbHarness();
    const index = openIndex(harness);
    await index.refreshHerdrSession(sessionInput());

    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected indexed agent");
    const plan: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "history", text: "final answer", timestamp: null },
      },
      from: "working",
      to: "working",
    };

    await expect(index.executeStatusEventPlan(plan)).resolves.toBeUndefined();
    const count = harness.sqlite
      .prepare("select count(*) as count from status_event_plans")
      .get() as { count: number };
    expect(count.count).toBe(0);
    harness.sqlite.close();
  });

  test("appending a terminal plan whose agent row is gone cancels the plan and appends nothing", async () => {
    const harness = openObservabilityDbHarness();
    const index = openIndex(harness);
    await index.refreshHerdrSession(sessionInput());

    const fast = await index.handleHerdrEventFast({
      event: { agent_status: "done", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });
    expect(fast.statusEventPlans).toHaveLength(1);
    const plan = fast.statusEventPlans[0];
    if (!plan) throw new Error("expected status event plan");

    // The agent row disappears before the plan executes: the append-time
    // mismatch guard must cancel (not append a dangling event, not mark
    // completed).
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected indexed agent");
    harness.sqlite.prepare("delete from agents where id = ?").run(agent.id);

    await expect(index.executeStatusEventPlan(plan)).resolves.toBeUndefined();

    const rows = harness.sqlite
      .prepare("select id, status from status_event_plans order by id")
      .all() as Array<{ id: number; status: string }>;
    expect(rows).toHaveLength(1);
    expect(rows[0]?.status).toBe("cancelled");
    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(events.filter((e) => e.type === "agent.done")).toHaveLength(0);
    harness.sqlite.close();
  });
});
