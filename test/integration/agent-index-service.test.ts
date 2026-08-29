import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import type { AgentHistoryService } from "@/agent-history/service.js";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { AgentIndexService } from "@/observability/agent-index-service.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

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
