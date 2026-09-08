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
  test("C1: agy working -> idle with empty assistant message suppresses all events and leaves plan pending in WAITING state", async () => {
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
      sleep: () => Promise.resolve(),
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());

    const result = await index.handleHerdrEvent({
      event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });

    // agent.idle and agent.status.changed events are suppressed in returned events
    expect(result.events).toEqual([]);

    // Zero events in DB (neither status.changed nor idle)
    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents).toEqual([]);

    // Plan row in DB is pending with PLAN_WAITING_HISTORY
    const plans = harness.statusEventPlans.listUnfinished();
    expect(plans).toHaveLength(1);
    expect(plans[0]).toMatchObject({
      attempts: 1,
      lastError: "PLAN_WAITING_HISTORY",
      status: "pending",
    });

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("C2: agy working -> idle with non-empty assistant message writes paired status.changed and idle events", async () => {
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
              lastAssistantMessage: { ref: "ref-1", text: "agy finished task", timestamp: null },
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

    expect(result.events).toHaveLength(1);
    expect(result.events[0]?.type).toBe("agent.idle");

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents).toHaveLength(2);
    expect(allEvents[0]?.type).toBe("agent.status.changed");
    expect(allEvents[1]?.type).toBe("agent.idle");

    harness.sqlite.close();
  });

  test("C3: consecutive two rounds of working -> done with different event ids write both rounds, replaying same id does not write", async () => {
    const harness = openObservabilityDbHarness();
    let currentAssistantRef = "ref-1";
    let currentAgentStatus = "working";
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent(currentAgentStatus, 10, "agy");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("antigravity-sqlite"),
              lastAssistantMessage: {
                ref: currentAssistantRef,
                text: `done text for ${currentAssistantRef}`,
                timestamp: null,
              },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());

    // Round 1
    const round1 = await index.handleHerdrEvent({
      event: {
        agent_status: "done",
        event_id: "ev1",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...sessionInput(),
    });
    expect(round1.events.map((e) => e.type)).toEqual(["agent.done"]);

    // Replay round 1 with same event_id
    const replay1 = await index.handleHerdrEvent({
      event: {
        agent_status: "done",
        event_id: "ev1",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...sessionInput(),
    });
    expect(replay1.events).toEqual([]);

    // Round 2: Agent starts working again, then transitions to done with new assistant ref and new event_id
    currentAgentStatus = "working";
    currentAssistantRef = "ref-2";
    await index.handleHerdrEvent({
      event: {
        agent_status: "working",
        event_id: "ev2-work",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...sessionInput(),
    });

    const round2 = await index.handleHerdrEvent({
      event: {
        agent_status: "done",
        event_id: "ev2-done",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...sessionInput(),
    });
    expect(round2.events.map((e) => e.type)).toEqual(["agent.done"]);

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(2);

    harness.sqlite.close();
  });

  test("C4: pane returned to working still emits agy idle event pair (agy idle mismatch guaranteed)", async () => {
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
              lastAssistantMessage: { ref: "ref-1", text: "completed task", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());

    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const plan: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: { ref: "ref-1", text: "completed task", timestamp: null },
      },
      from: "working",
      to: "idle",
    };

    // Agent in store is currently "working", but plan is to "idle"
    // For agy idle, mismatch is exempt and emits paired events
    const event = await index.executeStatusEventPlan(plan);
    expect(event?.type).toBe("agent.idle");

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.map((e) => e.type)).toEqual(["agent.status.changed", "agent.idle"]);

    harness.sqlite.close();
  });

  test("C5: fake timers 8x4s + 7x10s = 102s exhausts retry budget to failed with attempts=8, one agent.failed event, no drainPendingPlans called", async () => {
    vi.useFakeTimers();
    try {
      const harness = openObservabilityDbHarness();
      const drainSpy = vi.spyOn(AgentIndexService.prototype, "drainPendingPlans");

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

      const handlePromise = index.handleHerdrEvent({
        event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInput(),
      });

      // Advance initial attempt (8 x 500ms = 4000ms)
      await vi.advanceTimersByTimeAsync(4000);
      const initialResult = await handlePromise;
      expect(initialResult.events).toEqual([]);

      const planRecord = harness.statusEventPlans.listUnfinished()[0];
      if (!planRecord) throw new Error("expected pending plan");
      expect(harness.statusEventPlans.get(planRecord.id)).toMatchObject({
        attempts: 1,
        lastError: "PLAN_WAITING_HISTORY",
        status: "pending",
      });

      // Advance remaining 7 retry intervals (10s timer + 4s wait each = 14s each, total 7 x 14s = 98s)
      for (let retry = 2; retry <= 8; retry += 1) {
        await vi.advanceTimersByTimeAsync(10_000);
        await vi.advanceTimersByTimeAsync(4_000);
      }

      const finalPlan = harness.statusEventPlans.get(planRecord.id);
      expect(finalPlan.attempts).toBe(8);
      expect(finalPlan.status).toBe("failed");
      expect(finalPlan.lastError).toBe("PLAN_WAITING_HISTORY");

      const allEvents = harness.agentEvents.listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      });
      const failedEvents = allEvents.filter((event) => event.type === "agent.failed");
      expect(failedEvents).toHaveLength(1);
      expect(failedEvents[0]?.payload).toMatchObject({
        attempts: 8,
        from: "working",
        paneId: "wJ:p2",
        reason: "PLAN_WAITING_HISTORY",
        to: "idle",
      });
      expect(allEvents.filter((event) => event.type !== "agent.failed")).toEqual([]);

      // Proves drainPendingPlans was never called during retry loop
      expect(drainSpy).not.toHaveBeenCalled();

      index.stopWaitingHistoryRetries();
      harness.sqlite.close();
    } finally {
      vi.useRealTimers();
    }
  });

  test("C6: claude working -> idle mismatch is skipped and marked completed without event generation", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "claude");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("claude-jsonl"),
              lastAssistantMessage: { ref: "ref-claude", text: "claude message", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());

    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const plan: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-claude", text: "claude message", timestamp: null },
      },
      from: "working",
      to: "idle",
    };

    // Agent status in store is "working", target is "idle".
    // For non-agy idle, mismatch is NOT exempt -> returns undefined, plan completed
    const result = await index.executeStatusEventPlan(plan);
    expect(result).toBeUndefined();

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents).toEqual([]);

    harness.sqlite.close();
  });

  test("claude working -> done with non-empty assistant message in snapshot writes paired events without delay (0 sleep/retry calls)", async () => {
    const harness = openObservabilityDbHarness();
    const sleepSpy = vi.fn();
    const scheduleRetrySpy = vi.fn();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "claude");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("claude-jsonl"),
              lastAssistantMessage: {
                ref: "ref-claude",
                text: "claude completed",
                timestamp: null,
              },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      scheduleRetry: scheduleRetrySpy,
      sleep: sleepSpy,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());

    const result = await index.handleHerdrEvent({
      event: { agent_status: "done", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });

    // Non-agy non-pi is ready immediately without deadlock or waiting
    expect(result.events.map((e) => e.type)).toEqual(["agent.done"]);

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.map((e) => e.type)).toEqual(["agent.status.changed", "agent.done"]);

    // Locked: non-agy does not sleep or schedule retries
    expect(sleepSpy).toHaveBeenCalledTimes(0);
    expect(scheduleRetrySpy).toHaveBeenCalledTimes(0);

    harness.sqlite.close();
  });

  test("blocked: non-pi to=blocked bypasses ready gate and writes paired events immediately without assistant message", async () => {
    const harness = openObservabilityDbHarness();
    const sleepSpy = vi.fn();
    const scheduleRetrySpy = vi.fn();
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
      scheduleRetry: scheduleRetrySpy,
      sleep: sleepSpy,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());

    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const plan: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: null,
      },
      from: "working",
      to: "blocked",
    };

    const result = await index.executeStatusEventPlan(plan);
    expect(result).toBeDefined();
    expect(result?.type).toBe("agent.blocked");

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.map((e) => e.type)).toEqual(["agent.status.changed", "agent.blocked"]);

    expect(sleepSpy).toHaveBeenCalledTimes(0);
    expect(scheduleRetrySpy).toHaveBeenCalledTimes(0);

    harness.sqlite.close();
  });

  test("retry does not trust insert-time compact and refreshes from disk, avoiding duplicate done when terminal event already emitted", async () => {
    const harness = openObservabilityDbHarness();
    let historyRef = "R1";
    let historyText = "outdated R1 content";

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
              lastAssistantMessage: { ref: historyRef, text: historyText, timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      scheduleRetry: () => 1,
      sleep: async () => {},
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    // 1. P1 executes and completes with R3
    historyRef = "R3";
    historyText = "terminal R3 content";
    const p1: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: { ref: "R3", text: "terminal R3 content", timestamp: null },
      },
      from: "working",
      to: "done",
    };
    const p1Result = await index.executeStatusEventPlan(p1);
    expect(p1Result?.type).toBe("agent.done");

    // 2. Insert P2 which was enqueued with old compact ref=R1
    const p2Row = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: { ref: "R1", text: "outdated R1 content", timestamp: null },
      },
      from: "working",
      to: "done",
    });
    // Set to WAITING_HISTORY (attempts = 1)
    harness.statusEventPlans.markRetry(p2Row.id, new Error("PLAN_WAITING_HISTORY"));

    // 3. Now simulate P2 retry entering #retryWaitingPlanRow
    // Disk still has R3 (matching latest terminal event in agent_events).
    // Because retry refreshes agent with forceRefresh: true, it reads R3 (matching prevRef R3),
    // sees currentRef === prevRef (not ready), and throws PlanWaitingHistoryError instead of
    // treating insert-time R1 as new content!
    // Execute drainPendingPlans to run the retry row
    await index.drainPendingPlans();

    // Verify agent_events still only has 1 done event with R3 (no duplicate second done with R1)
    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(1);
    expect(doneEvents[0]?.compactHistory?.lastAssistantMessage?.ref).toBe("R3");

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("deduplication: keyed plan after terminal event (hasTerminalEventAfter=true) writes paired events even with matching from/to", async () => {
    const harness = openObservabilityDbHarness();
    let currentRef = "ref-1";
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
              lastAssistantMessage: { ref: currentRef, text: "output text", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    // Plan 1: keyed event "key-1", working -> done
    const p1: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: { ref: "ref-1", text: "output text 1", timestamp: null },
      },
      from: "working",
      herdrEventKey: "key-1",
      to: "done",
    };
    const res1 = await index.executeStatusEventPlan(p1);
    expect(res1?.type).toBe("agent.done");

    // Plan 2: keyed event "key-2", working -> done (same from/to, but key-2 hasTerminalEventAfter=true and ref-2)
    currentRef = "ref-2";
    const p2: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: { ref: "ref-2", text: "output text 2", timestamp: null },
      },
      from: "working",
      herdrEventKey: "key-2",
      to: "done",
    };
    const res2 = await index.executeStatusEventPlan(p2);
    expect(res2?.type).toBe("agent.done");

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(2);

    harness.sqlite.close();
  });

  test("deduplication: legacy plan without herdrEventKey skips duplicate if ref matches latest terminal, emits if ref differs", async () => {
    const harness = openObservabilityDbHarness();
    let currentRef = "ref-1";
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
              lastAssistantMessage: { ref: currentRef, text: "output text", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    // 1. Initial terminal event (working -> done) with ref-1
    const p1: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: { ref: "ref-1", text: "output text 1", timestamp: null },
      },
      from: "working",
      to: "done",
    };
    const res1 = await index.executeStatusEventPlan(p1);
    expect(res1?.type).toBe("agent.done");

    // 2. Legacy plan with SAME ref-1 -> duplicate, skipped without emitting
    const p2Duplicate: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: { ref: "ref-1", text: "output text 1 duplicate", timestamp: null },
      },
      from: "working",
      to: "done",
    };
    const res2 = await index.executeStatusEventPlan(p2Duplicate);
    expect(res2).toBeUndefined();

    // 3. Legacy plan with DIFFERENT ref-2 -> emitted
    currentRef = "ref-2";
    const p3Different: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: { ref: "ref-2", text: "output text 2 new", timestamp: null },
      },
      from: "working",
      to: "done",
    };
    const res3 = await index.executeStatusEventPlan(p3Different);
    expect(res3?.type).toBe("agent.done");

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(2);

    harness.sqlite.close();
  });

  test("plan reaching max attempts (8) transitions to failed and logs structured console.warn", async () => {
    const harness = openObservabilityDbHarness();
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});

    try {
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
        sleep: async () => {},
        stores: harness,
      });

      await index.refreshHerdrSession(sessionInput());
      const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
      if (!agent) throw new Error("expected agent");

      const planRow = harness.statusEventPlans.insertPending({
        agent,
        from: "working",
        to: "done",
      });

      // Set attempts to 7 so the next failure becomes attempt 8 (failed)
      for (let i = 1; i <= 7; i += 1) {
        harness.statusEventPlans.markRetry(planRow.id, new Error("PLAN_WAITING_HISTORY"));
      }

      await index.drainPendingPlans();

      const updated = harness.statusEventPlans.get(planRow.id);
      expect(updated.attempts).toBe(8);
      expect(updated.status).toBe("failed");

      expect(warnSpy).toHaveBeenCalledWith(
        "Herdsman status event plan failed after max attempts",
        expect.objectContaining({
          agentId: agent.id,
          attempts: 8,
          from: "working",
          herdrSessionName: "default",
          planId: planRow.id,
          to: "done",
        }),
      );

      const failedEvents = harness.agentEvents
        .listAfter({
          herdrSessionName: "default",
          workspaceId: "wJ",
        })
        .filter((event) => event.type === "agent.failed");
      expect(failedEvents).toHaveLength(1);
      expect(failedEvents[0]?.payload).toMatchObject({
        agent: "agy",
        attempts: 8,
        from: "working",
        paneId: "wJ:p2",
        reason: "PLAN_WAITING_HISTORY",
        to: "done",
      });

      await index.drainPendingPlans();
      expect(
        harness.agentEvents
          .listAfter({
            herdrSessionName: "default",
            workspaceId: "wJ",
          })
          .filter((event) => event.type === "agent.failed"),
      ).toHaveLength(1);
      const replayed = harness.agentEvents.append({
        agentId: agent.id,
        herdrSessionName: "default",
        idempotencyKey: `agent.failed:plan:${planRow.id}`,
        paneId: agent.paneId,
        payload: { duplicate: true },
        terminalId: agent.terminalId,
        type: "agent.failed",
        workspaceId: agent.workspaceId,
      });
      expect(replayed.id).toBe(failedEvents[0]?.id);
    } finally {
      warnSpy.mockRestore();
      harness.sqlite.close();
    }
  });

  test("#drainPlanRow 8th refresh failure marks plan failed, logs plan marked failed, writes one agent.failed", async () => {
    const harness = openObservabilityDbHarness();
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});

    try {
      let failRefresh = false;
      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return oneAgent("working", 10, "claude");
          },
        }),
        history: {
          async resolveCompactHistory() {
            if (failRefresh) {
              throw new Error("simulated disk IO refresh failure");
            }
            return {
              compactHistory: {
                ...emptyCompactHistory("claude-jsonl"),
                lastAssistantMessage: { ref: "initial", text: "initial output", timestamp: null },
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        scheduleRetry: () => 1,
        sleep: async () => {},
        stores: harness,
      });

      await index.refreshHerdrSession(sessionInput());
      const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
      if (!agent) throw new Error("expected agent");

      const planRow = harness.statusEventPlans.insertPending({
        agent,
        compactHistory: {
          ...emptyCompactHistory("claude-jsonl"),
          lastAssistantMessage: { ref: "initial", text: "initial output", timestamp: null },
        },
        from: "working",
        to: "done",
      });
      for (let i = 1; i <= 7; i += 1) {
        harness.statusEventPlans.markRetry(planRow.id, new Error("PLAN_WAITING_HISTORY"));
      }

      failRefresh = true;
      await index.drainPendingPlans();

      const updated = harness.statusEventPlans.get(planRow.id);
      expect(updated.status).toBe("failed");
      expect(updated.attempts).toBe(8);

      expect(warnSpy).toHaveBeenCalledWith(
        "Herdsman plan marked failed",
        expect.objectContaining({
          agentId: agent.id,
          from: "working",
          planId: planRow.id,
          reason: "PLAN_WAITING_HISTORY",
          to: "done",
        }),
      );
      expect(
        warnSpy.mock.calls.some(
          (call) => typeof call[0] === "string" && call[0].includes("keeping waiting"),
        ),
      ).toBe(false);

      const failedEvents = harness.agentEvents
        .listAfter({
          herdrSessionName: "default",
          workspaceId: "wJ",
        })
        .filter((event) => event.type === "agent.failed");
      expect(failedEvents).toHaveLength(1);
      expect(failedEvents[0]?.payload).toMatchObject({
        attempts: 8,
        from: "working",
        paneId: "wJ:p2",
        reason: "PLAN_WAITING_HISTORY",
        to: "done",
      });

      await index.drainPendingPlans();
      expect(
        harness.agentEvents
          .listAfter({
            herdrSessionName: "default",
            workspaceId: "wJ",
          })
          .filter((event) => event.type === "agent.failed"),
      ).toHaveLength(1);

      index.stopWaitingHistoryRetries();
    } finally {
      warnSpy.mockRestore();
      harness.sqlite.close();
    }
  });

  test("T1: legacy non-agy retry: ref/text equals already-emitted terminal -> no emit and stays in waiting; ref/text differs -> emits paired events", async () => {
    const harness = openObservabilityDbHarness();
    let historyText = "Claude turn 1";
    let historyRef: string | null = "ref-1";
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "claude");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("claude-jsonl"),
              lastAssistantMessage: { ref: historyRef, text: historyText, timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      scheduleRetry: () => 1,
      sleep: async () => {},
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    // 1. Initial terminal event for Claude (working -> done) with "Claude turn 1"
    const p1: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "Claude turn 1", timestamp: null },
      },
      from: "working",
      to: "done",
    };
    const res1 = await index.executeStatusEventPlan(p1);
    expect(res1?.type).toBe("agent.done");

    // 2. Retry row with SAME text/ref -> stays in waiting (no second done event)
    const p2Row = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "Claude turn 1", timestamp: null },
      },
      from: "working",
      to: "done",
    });
    harness.statusEventPlans.markRetry(p2Row.id, new Error("PLAN_WAITING_HISTORY"));

    await index.drainPendingPlans();

    let allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    let doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(1);
    expect(harness.statusEventPlans.get(p2Row.id).status).toBe("pending");

    // 3. Disk updates with new text -> retry row now drains successfully and emits second done!
    historyText = "Claude turn 2 new output";
    historyRef = "ref-2";
    await index.drainPendingPlans();

    allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(2);
    expect(doneEvents[1]?.compactHistory?.lastAssistantMessage?.text).toBe(
      "Claude turn 2 new output",
    );
    expect(harness.statusEventPlans.get(p2Row.id).status).toBe("completed");

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("keyed retry: same assistant content in retry -> stays in waiting; different assistant content -> emits paired events", async () => {
    const harness = openObservabilityDbHarness();
    let historyText = "Codex turn 1";
    let historyRef: string | null = "ref-1";
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "codex");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("codex-jsonl"),
              lastAssistantMessage: { ref: historyRef, text: historyText, timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      scheduleRetry: () => 1,
      sleep: async () => {},
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    // 1. Initial terminal event (working -> done) with key "key-1"
    const p1: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("codex-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "Codex turn 1", timestamp: null },
      },
      from: "working",
      herdrEventKey: "key-1",
      to: "done",
    };
    const res1 = await index.executeStatusEventPlan(p1);
    expect(res1?.type).toBe("agent.done");

    // 2. Keyed retry row with new key "key-2", but SAME text/ref -> stays in waiting
    const p2Row = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("codex-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "Codex turn 1", timestamp: null },
      },
      from: "working",
      herdrEventKey: "key-2",
      to: "done",
    });
    harness.statusEventPlans.markRetry(p2Row.id, new Error("PLAN_WAITING_HISTORY"));

    await index.drainPendingPlans();

    let allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    let doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(1);
    expect(harness.statusEventPlans.get(p2Row.id).status).toBe("pending");

    // 3. Disk updates with new text -> retry row now drains successfully and emits second done!
    historyText = "Codex turn 2 new output";
    historyRef = "ref-2";
    await index.drainPendingPlans();

    allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(2);
    expect(doneEvents[1]?.compactHistory?.lastAssistantMessage?.text).toBe(
      "Codex turn 2 new output",
    );
    expect(harness.statusEventPlans.get(p2Row.id).status).toBe("completed");

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("T2: pi retry without history advance throws PlanWaitingHistoryError and does not emit with warning", async () => {
    const harness = openObservabilityDbHarness();
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
    const turnRegistry = new TurnCompletionRegistry();

    try {
      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return oneAgent("working", 10, "pi");
          },
        }),
        history: {
          async resolveCompactHistory() {
            return {
              compactHistory: {
                ...emptyCompactHistory("pi-jsonl"),
                lastAssistantMessage: null,
                messageCount: 0,
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        scheduleRetry: () => 1,
        sleep: async () => {},
        stores: harness,
        turnCompletions: turnRegistry,
      });

      await index.refreshHerdrSession(sessionInput());
      const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
      if (!agent) throw new Error("expected agent");

      // Insert retry plan row (attempts = 1)
      const pRow = harness.statusEventPlans.insertPending({
        agent,
        compactHistory: {
          ...emptyCompactHistory("pi-jsonl"),
          lastAssistantMessage: null,
          messageCount: 0,
        },
        from: "working",
        to: "done",
      });
      harness.statusEventPlans.markRetry(pRow.id, new Error("PLAN_WAITING_HISTORY"));
      // Record turn completion signal so waitForSignal returns immediately without 5000ms delay
      turnRegistry.record({
        confirmed: true,
        herdrSessionName: "default",
        paneId: "wJ:p2",
        terminalId: "term_claude",
        workspaceId: "wJ",
      });

      await index.drainPendingPlans();

      const updated = harness.statusEventPlans.get(pRow.id);
      expect(updated.status).toBe("pending");
      expect(updated.attempts).toBe(2);

      const events = harness.agentEvents.listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      });
      expect(events.filter((e) => e.type === "agent.done")).toHaveLength(0);

      // Verify no lenient emission warning
      expect(warnSpy).not.toHaveBeenCalledWith(
        expect.stringContaining("Herdsman emitted pi agent.done without a turn completion signal"),
        expect.anything(),
      );

      index.stopWaitingHistoryRetries();
    } finally {
      warnSpy.mockRestore();
      harness.sqlite.close();
    }
  });

  test("S2: pi retry window receives user prompt (messageCount increments without assistant change) -> stays in waiting; assistant ref changes -> emits", async () => {
    const harness = openObservabilityDbHarness();
    const turnRegistry = new TurnCompletionRegistry();
    let currentRef = "pi-ref-1";
    let currentText = "pi answer turn 1";
    let currentMsgCount = 2;

    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "pi");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: { ref: currentRef, text: currentText, timestamp: null },
              messageCount: currentMsgCount,
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      scheduleRetry: () => 1,
      sleep: async () => {},
      stores: harness,
      turnCompletions: turnRegistry,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    // 1. Initial terminal event for Pi (working -> done) with ref-1, msgCount=2
    turnRegistry.record({
      confirmed: true,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });
    const p1: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: { ref: "pi-ref-1", text: "pi answer turn 1", timestamp: null },
        messageCount: 2,
      },
      from: "working",
      to: "done",
    };
    const res1 = await index.executeStatusEventPlan(p1);
    expect(res1?.type).toBe("agent.done");

    // 2. Retry row entered (attempts = 1).
    // In retry window, user sends new prompt: messageCount becomes 3, but lastAssistantMessage is still ref-1!
    currentMsgCount = 3;
    const p2Row = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: { ref: "pi-ref-1", text: "pi answer turn 1", timestamp: null },
        messageCount: 2,
      },
      from: "working",
      to: "done",
    });
    harness.statusEventPlans.markRetry(p2Row.id, new Error("PLAN_WAITING_HISTORY"));

    turnRegistry.record({
      confirmed: true,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    await index.drainPendingPlans();

    // Must NOT emit second done (fake advance suppressed), stays pending waiting!
    let allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    let doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(1);
    expect(harness.statusEventPlans.get(p2Row.id).status).toBe("pending");

    // 3. Assistant responds for real: ref becomes "pi-ref-2", text becomes "pi answer turn 2", msgCount=4
    currentRef = "pi-ref-2";
    currentText = "pi answer turn 2";
    currentMsgCount = 4;

    turnRegistry.record({
      confirmed: true,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    await index.drainPendingPlans();

    // Now emits second done!
    allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(2);
    expect(doneEvents[1]?.compactHistory?.lastAssistantMessage?.ref).toBe("pi-ref-2");
    expect(harness.statusEventPlans.get(p2Row.id).status).toBe("completed");

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("T3: refresh failure during retry/drain maintains waiting (markRetry) without emitting events", async () => {
    const harness = openObservabilityDbHarness();
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});

    try {
      let failRefresh = false;
      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return oneAgent("working", 10, "claude");
          },
        }),
        history: {
          async resolveCompactHistory() {
            if (failRefresh) {
              throw new Error("simulated disk IO refresh failure");
            }
            return {
              compactHistory: {
                ...emptyCompactHistory("claude-jsonl"),
                lastAssistantMessage: { ref: "initial", text: "initial output", timestamp: null },
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        scheduleRetry: () => 1,
        sleep: async () => {},
        stores: harness,
      });

      await index.refreshHerdrSession(sessionInput());
      const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
      if (!agent) throw new Error("expected agent");

      // Insert pending row with attempts = 1
      const pRow = harness.statusEventPlans.insertPending({
        agent,
        compactHistory: {
          ...emptyCompactHistory("claude-jsonl"),
          lastAssistantMessage: { ref: "initial", text: "initial output", timestamp: null },
        },
        from: "working",
        to: "done",
      });
      harness.statusEventPlans.markRetry(pRow.id, new Error("PLAN_WAITING_HISTORY"));

      // Now set failRefresh = true
      failRefresh = true;

      await index.drainPendingPlans();

      const updated = harness.statusEventPlans.get(pRow.id);
      expect(updated.status).toBe("pending");
      expect(updated.attempts).toBe(2);
      expect(updated.lastError).toBe("PLAN_WAITING_HISTORY");

      const events = harness.agentEvents.listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      });
      expect(events).toHaveLength(0);

      index.stopWaitingHistoryRetries();
    } finally {
      warnSpy.mockRestore();
      harness.sqlite.close();
    }
  });

  test("S1: refresh failure maintains PLAN_WAITING_HISTORY so 10s timer can retry instead of no-op delete", async () => {
    const harness = openObservabilityDbHarness();
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});

    try {
      let failRefresh = false;
      let capturedTimerCallback: (() => Promise<void>) | undefined;

      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return oneAgent("working", 10, "claude");
          },
        }),
        history: {
          async resolveCompactHistory() {
            if (failRefresh) {
              throw new Error("simulated disk IO refresh failure");
            }
            return {
              compactHistory: {
                ...emptyCompactHistory("claude-jsonl"),
                lastAssistantMessage: { ref: "ref-ok", text: "recovered output", timestamp: null },
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        scheduleRetry: (cb) => {
          capturedTimerCallback = cb as () => Promise<void>;
          return 1;
        },
        sleep: async () => {},
        stores: harness,
      });

      await index.refreshHerdrSession(sessionInput());
      const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
      if (!agent) throw new Error("expected agent");

      const pRow = harness.statusEventPlans.insertPending({
        agent,
        compactHistory: {
          ...emptyCompactHistory("claude-jsonl"),
          lastAssistantMessage: { ref: "initial", text: "initial output", timestamp: null },
        },
        from: "working",
        to: "done",
      });
      harness.statusEventPlans.markRetry(pRow.id, new Error("PLAN_WAITING_HISTORY"));

      // 1. Refresh fails -> lastError must be PLAN_WAITING_HISTORY and timer scheduled
      failRefresh = true;
      await index.drainPendingPlans();

      const afterFail = harness.statusEventPlans.get(pRow.id);
      expect(afterFail.status).toBe("pending");
      expect(afterFail.lastError).toBe("PLAN_WAITING_HISTORY");
      expect(afterFail.attempts).toBe(2);
      expect(capturedTimerCallback).toBeDefined();

      // 2. IO recovers, 10s timer fires -> callback successfully executes plan
      failRefresh = false;
      await capturedTimerCallback?.();

      const afterTimer = harness.statusEventPlans.get(pRow.id);
      expect(afterTimer.status).toBe("completed");

      const events = harness.agentEvents.listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      });
      expect(events.filter((e) => e.type === "agent.done")).toHaveLength(1);

      index.stopWaitingHistoryRetries();
    } finally {
      warnSpy.mockRestore();
      harness.sqlite.close();
    }
  });

  test("T4: crash recovery drain with attempts=0 uses refreshed compact history from disk instead of insert-time stale compact", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "claude");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("claude-jsonl"),
              lastAssistantMessage: {
                ref: "fresh-disk-ref",
                text: "fresh disk text",
                timestamp: null,
              },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    // Insert pending plan with stale insert-time compact and attempts = 0 (as if inserted right before crash)
    const pRow = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: {
          ref: "stale-insert-ref",
          text: "stale insert text",
          timestamp: null,
        },
      },
      from: "working",
      to: "done",
    });
    expect(pRow.attempts).toBe(0);

    // Drain runs after reboot
    await index.drainPendingPlans();

    const updated = harness.statusEventPlans.get(pRow.id);
    expect(updated.status).toBe("completed");

    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const doneEvent = events.find((e) => e.type === "agent.done");
    expect(doneEvent).toBeDefined();
    expect(doneEvent?.compactHistory?.lastAssistantMessage?.ref).toBe("fresh-disk-ref");
    expect(doneEvent?.compactHistory?.lastAssistantMessage?.text).toBe("fresh disk text");

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
