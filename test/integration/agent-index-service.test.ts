import { chmodSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AgentHistoryService } from "@/agent-history/service.js";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { AgentEventStore } from "@/db/agent-events.js";
import { STATUS_PLAN_MAX_ATTEMPTS } from "@/db/status-event-plans.js";
import { AgentIndexService, type StatusEventPlan } from "@/observability/agent-index-service.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import type { AgentEventRecord } from "@/observability/contracts.js";
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
    const registry = new TurnCompletionRegistry({ timeoutMs: 50, sleep: async () => {} });
    const oldHistory = {
      ...emptyCompactHistory("pi-jsonl"),
      lastAssistantMessage: { ref: "x#entry=1", text: "old", timestamp: null, stopReason: "stop" },
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
              lastAssistantMessage: {
                ref: "x#entry=2",
                text: "old",
                timestamp: null,
                stopReason: "stop",
              },
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
    const allowedPath = join("/tmp/herdr-role-sessions", `early-${Date.now()}.jsonl`);
    mkdirSync("/tmp/herdr-role-sessions", { recursive: true });
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
      value: "/tmp/herdr-role-sessions/serialized-pi-session.jsonl",
    };
    mkdirSync("/tmp/herdr-role-sessions", { recursive: true });
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

  test("stores a session hint when the registered file is missing and promotes it after the file appears", async () => {
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
    await index.refreshHerdrSession(sessionInput());
    const allowedPath = join("/tmp/herdr-role-sessions", `hint-missing-${Date.now()}.jsonl`);
    const sessionRef = {
      agent: "pi" as const,
      kind: "path" as const,
      source: "herdr:pi",
      value: allowedPath,
    };

    const registered = await index.registerPiSessionRef({
      herdrSessionName: "default",
      sessionRef,
      terminalId: "term_claude",
    });
    expect(registered.agent?.agentSession).toEqual(sessionRef);
    expect(
      harness.sqlite
        .prepare("select agent_session_hint_json from agents where terminal_id = ?")
        .get("term_claude"),
    ).toEqual({ agent_session_hint_json: JSON.stringify(sessionRef) });
    const registeredId = registered.agent?.id;
    if (!registeredId) throw new Error("expected registered agent");
    expect(harness.agentContextSnapshots.get(registeredId)?.historyRef?.path).not.toBe(allowedPath);

    mkdirSync("/tmp/herdr-role-sessions", { recursive: true });
    writeFileSync(allowedPath, JSON.stringify({ cwd: "/tmp" }));
    chmodSync(allowedPath, 0o600);
    const refreshed = await index.refreshHerdrSession(sessionInput());
    expect(refreshed.agents[0]?.agentSession).toEqual(sessionRef);
    expect(harness.agentContextSnapshots.get(registeredId)?.historyRef?.path).toBe(allowedPath);
    harness.sqlite.close();
  });

  test("re-evaluates discovered_file and null history snapshots on every index refresh", async () => {
    const harness = openObservabilityDbHarness();
    const calls: string[] = [];
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("idle", 10, "pi");
        },
      }),
      history: {
        async resolveCompactHistory(agent: { agent: string | null }) {
          calls.push(agent.agent ?? "unknown");
          const path = "/tmp/herdr-role-sessions/default/role-spec/session.jsonl";
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: { ref: "history", text: "result", timestamp: null },
            },
            historyRef: {
              kind: "discovered_file",
              path,
              source: "pi-jsonl",
              value: path,
            },
            sourceFingerprint: { mtimeMs: 1, path, size: 1 },
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    expect(calls).toEqual(["pi"]);
    calls.length = 0;
    await index.refreshHerdrSession(sessionInput());
    expect(calls).toEqual(["pi"]);

    const nullHarness = openObservabilityDbHarness();
    const nullCalls: string[] = [];
    const nullIndex = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("idle", 10, "pi");
        },
      }),
      history: {
        async resolveCompactHistory(agent: { agent: string | null }) {
          nullCalls.push(agent.agent ?? "unknown");
          return {
            compactHistory: emptyCompactHistory("pi-jsonl"),
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: nullHarness,
    });
    await nullIndex.refreshHerdrSession(sessionInput());
    expect(nullCalls).toEqual(["pi"]);
    nullCalls.length = 0;
    await nullIndex.refreshHerdrSession(sessionInput());
    expect(nullCalls).toEqual(["pi"]);
    harness.sqlite.close();
    nullHarness.sqlite.close();
  });

  test("re-discovers when another agent occupies the cached history path", async () => {
    const harness = openObservabilityDbHarness();
    const calls: string[] = [];
    const sharedPath = "/tmp/herdr-role-sessions/default/role-shared/session.jsonl";
    let includeOccupant = false;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return snapshot(
            [
              agent({
                agent: "pi",
                pane_id: "wJ:p2",
                revision: undefined,
                terminal_id: "term_claude",
                workspace_id: "wJ",
              }),
              ...(includeOccupant
                ? [
                    agent({
                      agent: "pi",
                      agent_session: {
                        agent: "pi",
                        kind: "path",
                        source: "herdr:pi",
                        value: sharedPath,
                      },
                      pane_id: "wJ:p3",
                      revision: undefined,
                      terminal_id: "term_other",
                      workspace_id: "wJ",
                    }),
                  ]
                : []),
            ],
            includeOccupant
              ? [
                  { pane_id: "wJ:p2", revision: 10 },
                  { pane_id: "wJ:p3", revision: 10 },
                ]
              : [{ pane_id: "wJ:p2", revision: 10 }],
          );
        },
      }),
      history: {
        async resolveCompactHistory(agent: {
          agent: string | null;
          agentSession?: { value?: string };
        }) {
          calls.push(`${agent.agent}:${agent.agentSession?.value ?? "none"}`);
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: { ref: "history", text: "result", timestamp: null },
            },
            historyRef: {
              kind: "agent_session",
              path: sharedPath,
              source: "pi-jsonl",
              value: sharedPath,
            },
            sourceFingerprint: { mtimeMs: 1, path: sharedPath, size: 1 },
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    expect(calls).toEqual(["pi:none"]);
    calls.length = 0;
    await index.refreshHerdrSession(sessionInput());
    expect(calls).toEqual([]);
    includeOccupant = true;
    await index.refreshHerdrSession(sessionInput());
    expect(calls).toContain("pi:none");
    harness.sqlite.close();
  });

  test("forwards terminalTitle from the live snapshot into history lookup", async () => {
    const harness = openObservabilityDbHarness();
    const titles: Array<string | null | undefined> = [];
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return snapshot(
            [
              agent({
                agent: "pi",
                pane_id: "wJ:p2",
                revision: undefined,
                terminal_id: "term_claude",
                workspace_id: "wJ",
              }),
            ],
            [
              {
                pane_id: "wJ:p2",
                revision: 10,
                terminal_title: "π - role-worker-53c500b2 - root",
              },
            ],
          );
        },
      }),
      history: {
        async resolveCompactHistory(input: { terminalTitle?: string | null }) {
          titles.push(input.terminalTitle);
          return {
            compactHistory: emptyCompactHistory("pi-jsonl"),
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    expect(titles).toEqual(["π - role-worker-53c500b2 - root"]);
    harness.sqlite.close();
  });
});

function history(onResolve: (agent: { agent: string | null }) => void, assistantText = "result") {
  return {
    async resolveCompactHistory(
      agent: { agent: string | null },
      options: { preferredRef?: { path?: string; value?: string } | null } = {},
    ) {
      onResolve(agent);
      const path =
        options.preferredRef?.path ??
        options.preferredRef?.value ??
        `/tmp/herdr-role-sessions/default/${agent.agent ?? "unknown"}-history.jsonl`;
      const historyRef = {
        kind: "agent_session" as const,
        path,
        source: "claude-jsonl" as const,
        value: path,
      };
      return {
        compactHistory: {
          ...emptyCompactHistory("claude-jsonl"),
          historyRef,
          lastAssistantMessage: { ref: "history", text: assistantText, timestamp: null },
        },
        historyRef,
        sourceFingerprint: { mtimeMs: 1, path, size: 1 },
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

  test("S11: shutdown abort releases an in-flight ready wait and keeps the plan retryable", async () => {
    const harness = openObservabilityDbHarness();
    const shutdown = new AbortController();
    let waiting = false;
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
      // Stands in for the real readiness window (500ms x 8): only an abort can
      // end this wait, so the plan stays in flight until the daemon aborts it.
      shutdownSignal: shutdown.signal,
      sleep: (_ms, signal) =>
        new Promise<void>((resolve) => {
          waiting = true;
          signal?.addEventListener("abort", () => resolve(), { once: true });
        }),
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const handling = index.handleHerdrEvent({
      event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });
    await vi.waitFor(() => expect(waiting).toBe(true));

    let drained = false;
    const draining = index.drainInFlightPlans().then(() => {
      drained = true;
    });
    await new Promise((resolve) => setTimeout(resolve, 20));
    // Parked in the readiness wait: draining it would have to wait the window out,
    // which is what pushed the daemon past the unit's TimeoutStopSec.
    expect(drained).toBe(false);

    shutdown.abort();
    await draining;
    expect(drained).toBe(true);
    await expect(handling).resolves.toMatchObject({ events: [] });

    // Nothing was emitted for the half-observed transition, and the plan row stays
    // retryable for the next daemon start instead of being completed silently.
    expect(
      harness.agentEvents.listAfter({ herdrSessionName: "default", workspaceId: "wJ" }),
    ).toEqual([]);
    expect(harness.statusEventPlans.listUnfinished()).toMatchObject([
      { attempts: 1, lastError: "PLAN_WAITING_HISTORY", status: "pending" },
    ]);

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

  test("C5: fake timers 8x4s + 7x10s = 102s exhausts retry budget to discarded with attempts=8, zero agent.failed events, no drainPendingPlans called", async () => {
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
      expect(finalPlan.status).toBe("discarded");
      expect(finalPlan.lastError).toBe("PLAN_WAITING_HISTORY");

      const allEvents = harness.agentEvents.listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      });
      const failedEvents = allEvents.filter((event) => event.type === "agent.failed");
      expect(failedEvents).toHaveLength(0);
      const discardedEvents = allEvents.filter((event) => event.type === "agent.discarded");
      expect(discardedEvents).toHaveLength(1);
      expect(discardedEvents[0]?.payload).toMatchObject({
        attempts: 8,
        from: "working",
        paneId: "wJ:p2",
        reason: "PLAN_WAITING_HISTORY",
        to: "idle",
      });

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

  test("PLAN_WAITING_HISTORY plan reaching max attempts (8) transitions to discarded with zero agent.failed events", async () => {
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

      // Set attempts to 7 so the next failure becomes attempt 8 (discarded)
      for (let i = 1; i <= 7; i += 1) {
        harness.statusEventPlans.markRetry(planRow.id, new Error("PLAN_WAITING_HISTORY"));
      }

      await index.drainPendingPlans();

      const updated = harness.statusEventPlans.get(planRow.id);
      expect(updated.attempts).toBe(8);
      expect(updated.status).toBe("discarded");

      const failedEvents = harness.agentEvents
        .listAfter({
          herdrSessionName: "default",
          workspaceId: "wJ",
        })
        .filter((event) => event.type === "agent.failed");
      expect(failedEvents).toHaveLength(0);

      const discardedEvents = harness.agentEvents
        .listAfter({
          herdrSessionName: "default",
          workspaceId: "wJ",
        })
        .filter((event) => event.type === "agent.discarded");
      expect(discardedEvents).toHaveLength(1);
      expect(discardedEvents[0]?.payload).toMatchObject({
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
          .filter((event) => event.type === "agent.discarded"),
      ).toHaveLength(1);
    } finally {
      warnSpy.mockRestore();
      harness.sqlite.close();
    }
  });

  test("#drainPlanRow 8th refresh failure marks plan discarded, logs plan marked discarded, writes one agent.discarded", async () => {
    const harness = openObservabilityDbHarness();
    const infoSpy = vi.spyOn(console, "info").mockImplementation(() => {});

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
      expect(updated.status).toBe("discarded");
      expect(updated.attempts).toBe(8);

      expect(infoSpy).toHaveBeenCalledWith(
        "Herdsman plan marked discarded during drain",
        expect.objectContaining({
          agentId: agent.id,
          from: "working",
          planId: planRow.id,
          reason: "PLAN_WAITING_HISTORY",
          to: "done",
        }),
      );

      const failedEvents = harness.agentEvents
        .listAfter({
          herdrSessionName: "default",
          workspaceId: "wJ",
        })
        .filter((event) => event.type === "agent.failed");
      expect(failedEvents).toHaveLength(0);

      const discardedEvents = harness.agentEvents
        .listAfter({
          herdrSessionName: "default",
          workspaceId: "wJ",
        })
        .filter((event) => event.type === "agent.discarded");
      expect(discardedEvents).toHaveLength(1);
      expect(discardedEvents[0]?.payload).toMatchObject({
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
          .filter((event) => event.type === "agent.discarded"),
      ).toHaveLength(1);

      index.stopWaitingHistoryRetries();
    } finally {
      infoSpy.mockRestore();
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

  test("keyed retry: same assistant content in retry -> explicit skip (completed, zero extra events); different assistant content -> emits paired events", async () => {
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

    // 2. Keyed retry row with new key "key-2", but SAME text/ref -> explicit skip
    // (oracle b: completed, zero extra events; do not spin into waiting/failed).
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
    expect(harness.statusEventPlans.get(p2Row.id).status).toBe("completed");
    expect(harness.statusEventPlans.get(p2Row.id).status).not.toBe("pending");
    expect(harness.statusEventPlans.get(p2Row.id).status).not.toBe("failed");

    // 3. Disk updates with new text; a new keyed plan emits the second done.
    historyText = "Codex turn 2 new output";
    historyRef = "ref-2";
    const p3: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("codex-jsonl"),
        lastAssistantMessage: { ref: "ref-2", text: "Codex turn 2 new output", timestamp: null },
      },
      from: "working",
      herdrEventKey: "key-3",
      to: "done",
    };
    const res3 = await index.executeStatusEventPlan(p3);
    expect(res3?.type).toBe("agent.done");

    allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    doneEvents = allEvents.filter((e) => e.type === "agent.done");
    expect(doneEvents).toHaveLength(2);
    expect(doneEvents[1]?.compactHistory?.lastAssistantMessage?.text).toBe(
      "Codex turn 2 new output",
    );

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("T2: pi retry without history advance throws PlanWaitingHistoryError and does not emit with warning", async () => {
    const harness = openObservabilityDbHarness();
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
    const turnRegistry = new TurnCompletionRegistry({ sleep: async () => {} });

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
    const turnRegistry = new TurnCompletionRegistry({ sleep: async () => {} });
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
              lastAssistantMessage: {
                ref: currentRef,
                text: currentText,
                timestamp: null,
                stopReason: "stop",
              },
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
        lastAssistantMessage: {
          ref: "pi-ref-1",
          text: "pi answer turn 1",
          timestamp: null,
          stopReason: "stop",
        },
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
        lastAssistantMessage: {
          ref: "pi-ref-1",
          text: "pi answer turn 1",
          timestamp: null,
          stopReason: "stop",
        },
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

  test("S4: terminated pi agent recovers after 77s with stop message after error", async () => {
    const harness = openObservabilityDbHarness();
    const turnRegistry = new TurnCompletionRegistry({ sleep: async () => {} });
    let attempt = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "pi");
        },
      }),
      history: {
        async resolveCompactHistory() {
          attempt += 1;
          // First two attempts simulate the agent being terminated (error).
          // Third attempt simulates recovery with a proper stop message.
          if (attempt <= 2) {
            return {
              compactHistory: {
                ...emptyCompactHistory("pi-jsonl"),
                lastAssistantMessage: {
                  ref: "error-ref",
                  text: "error: terminated",
                  timestamp: null,
                  stopReason: "error",
                },
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          }
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "stop-ref",
                text: "final answer after recovery",
                timestamp: null,
                stopReason: "stop",
              },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      sleep: async () => {},
      stores: harness,
      turnCompletions: turnRegistry,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const plan = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: {
          ref: "error-ref",
          text: "error: terminated",
          timestamp: null,
          stopReason: "error",
        },
        messageCount: 1,
      },
      from: "working",
      to: "done",
    });

    turnRegistry.record({
      confirmed: true,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    await index.drainPendingPlans();

    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const doneEvents = events.filter((e) => e.type === "agent.done");
    // The plan should eventually emit with the recovered stop message, not the error.
    expect(doneEvents).toHaveLength(1);
    expect(doneEvents[0]?.compactHistory?.lastAssistantMessage?.ref).toBe("stop-ref");
    expect(doneEvents[0]?.compactHistory?.lastAssistantMessage?.text).toBe(
      "final answer after recovery",
    );
    expect(harness.statusEventPlans.get(plan.id).status).toBe("completed");

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

describe("batch2 window regressions", () => {
  test("W1: insertPending throw rolls back status update; agent stays working and listUnfinished is empty", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const before = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    expect(before?.agentStatus).toBe("working");

    vi.spyOn(harness.statusEventPlans, "insertPending").mockImplementation(() => {
      throw new Error("insertPending boom");
    });

    await expect(
      index.handleHerdrEventFast({
        event: { agent_status: "done", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInput(),
      }),
    ).rejects.toThrow("insertPending boom");

    const after = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    expect(after?.agentStatus).toBe("working");
    expect(harness.statusEventPlans.listUnfinished()).toEqual([]);
    harness.sqlite.close();
  });

  test("W2: matching status.changed without done still emits agent.done and does not duplicate status.changed", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "claude");
        },
      }),
      history: history(() => undefined),
      sleep: async () => {},
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    harness.agentEvents.append({
      agentId: agent.id,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "history", text: "result", timestamp: null },
      },
      herdrSessionName: "default",
      paneId: agent.paneId,
      payload: { from: "working", to: "done" },
      terminalId: agent.terminalId,
      type: "agent.status.changed",
      workspaceId: agent.workspaceId,
    });

    const plan: StatusEventPlan = {
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "history", text: "result", timestamp: null },
      },
      from: "working",
      to: "done",
    };
    const event = await index.executeStatusEventPlan(plan);
    expect(event?.type).toBe("agent.done");

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.filter((row) => row.type === "agent.done")).toHaveLength(1);
    expect(allEvents.filter((row) => row.type === "agent.status.changed")).toHaveLength(1);
    harness.sqlite.close();
  });

  test("W3: drainPendingPlans backfills agent.failed for failed rows without events", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const row = harness.statusEventPlans.insertPending({
      agent,
      from: "working",
      to: "done",
    });
    for (let i = 0; i < 8; i += 1) {
      harness.statusEventPlans.markRetry(row.id, new Error("RETRY_EXHAUSTED"));
    }
    expect(harness.statusEventPlans.get(row.id).status).toBe("failed");

    // Also insert a legacy failed row with PLAN_WAITING_HISTORY; it must NOT be backfilled
    const legacyRow = harness.statusEventPlans.insertPending({
      agent,
      from: "working",
      to: "done",
    });
    harness.sqlite
      .prepare(
        "update status_event_plans set status = 'failed', last_error = 'PLAN_WAITING_HISTORY' where id = ?",
      )
      .run(legacyRow.id);

    const infoSpy = vi.spyOn(console, "info").mockImplementation(() => {});
    try {
      await index.drainPendingPlans();

      // 静默入库：写入后立即确认，只可查、不投递、不打扰当前 owner。
      const failed = harness.sqlite
        .prepare("select * from agent_events where type = 'agent.failed'")
        .all() as Array<Record<string, unknown>>;
      expect(failed).toHaveLength(1);
      expect(failed[0]).toMatchObject({ agent_id: agent.id, deliverable: 0, status: "acked" });
      expect(JSON.parse(String(failed[0]?.payload_json))).toMatchObject({
        agentId: agent.id,
        from: "working",
        reason: "RETRY_EXHAUSTED",
        to: "done",
      });
      expect(
        harness.agentEvents.listAfter({ herdrSessionName: "default", workspaceId: "wJ" }),
      ).toEqual([]);

      // 存量补录只在日志里报数：能补 N 条 / 跳过 M 条。
      expect(infoSpy).toHaveBeenCalledWith("Herdsman backfilled agent.failed rows", {
        backfilled: 1,
        skipped: 1,
        total: 2,
      });
    } finally {
      infoSpy.mockRestore();
    }
    harness.sqlite.close();
  });

  test("W3: drainPendingPlans does not duplicate agent.failed when event already exists", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const row = harness.statusEventPlans.insertPending({
      agent,
      from: "working",
      to: "done",
    });
    for (let i = 0; i < 8; i += 1) {
      harness.statusEventPlans.markRetry(row.id, new Error("RETRY_EXHAUSTED"));
    }
    harness.agentEvents.append({
      agentId: agent.id,
      herdrSessionName: "default",
      idempotencyKey: `agent.failed:plan:${row.id}`,
      paneId: agent.paneId,
      payload: { from: "working", reason: "RETRY_EXHAUSTED", to: "done" },
      terminalId: agent.terminalId,
      type: "agent.failed",
      workspaceId: agent.workspaceId,
    });

    await index.drainPendingPlans();

    const failed = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((event) => event.type === "agent.failed");
    expect(failed).toHaveLength(1);
    harness.sqlite.close();
  });

  test("drainPendingPlans backfills agent.discarded for discarded rows without events", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const row = harness.statusEventPlans.insertPending({
      agent,
      from: "working",
      to: "done",
    });
    for (let i = 0; i < 8; i += 1) {
      harness.statusEventPlans.markRetry(row.id, new Error("PLAN_WAITING_HISTORY"));
    }
    expect(harness.statusEventPlans.get(row.id).status).toBe("discarded");

    const infoSpy = vi.spyOn(console, "info").mockImplementation(() => {});
    try {
      await index.drainPendingPlans();

      // 静默入库：写入后立即确认，只可查、不投递、不打扰当前 owner。
      const discarded = harness.sqlite
        .prepare("select * from agent_events where type = 'agent.discarded'")
        .all() as Array<Record<string, unknown>>;
      expect(discarded).toHaveLength(1);
      expect(discarded[0]).toMatchObject({ agent_id: agent.id, deliverable: 0, status: "acked" });
      expect(JSON.parse(String(discarded[0]?.payload_json))).toMatchObject({
        agentId: agent.id,
        attempts: 8,
        from: "working",
        planId: row.id,
        reason: "PLAN_WAITING_HISTORY",
        to: "done",
      });
      expect(
        harness.agentEvents.listAfter({ herdrSessionName: "default", workspaceId: "wJ" }),
      ).toEqual([]);

      expect(infoSpy).toHaveBeenCalledWith("Herdsman backfilled agent.discarded rows", {
        backfilled: 1,
        skipped: 0,
        total: 1,
      });
    } finally {
      infoSpy.mockRestore();
    }
    harness.sqlite.close();
  });

  test("drainPendingPlans does not duplicate agent.discarded when event already exists", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const row = harness.statusEventPlans.insertPending({
      agent,
      from: "working",
      to: "done",
    });
    for (let i = 0; i < 8; i += 1) {
      harness.statusEventPlans.markRetry(row.id, new Error("PLAN_WAITING_HISTORY"));
    }
    expect(harness.statusEventPlans.get(row.id).status).toBe("discarded");

    harness.agentEvents.append({
      agentId: agent.id,
      herdrSessionName: "default",
      idempotencyKey: `agent.discarded:plan:${row.id}`,
      paneId: agent.paneId,
      payload: {
        agentId: agent.id,
        attempts: 8,
        from: "working",
        planId: row.id,
        reason: "PLAN_WAITING_HISTORY",
        to: "done",
      },
      terminalId: agent.terminalId,
      type: "agent.discarded",
      workspaceId: agent.workspaceId,
    });

    await index.drainPendingPlans();

    const discarded = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((event) => event.type === "agent.discarded");
    expect(discarded).toHaveLength(1);
    harness.sqlite.close();
  });

  test("W6: plan failed after pane generation change emits with current generation", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return snapshot(
            [
              agent({
                pane_id: "wJ:p2",
                pane_generation: "gen-1",
                revision: 10,
                terminal_id: "term_claude",
                workspace_id: "wJ",
              }),
            ],
            [{ pane_id: "wJ:p2", revision: 10 }],
          );
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const original = harness.agents.findByPane({
      herdrSessionName: "default",
      paneId: "wJ:p2",
      paneGeneration: "gen-1",
    });
    if (!original) throw new Error("expected agent");

    harness.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [
        {
          agent: "claude",
          agent_status: "working",
          pane_id: "wJ:p2",
          pane_generation: "gen-2",
          terminal_id: "term_claude",
          workspace_id: "wJ",
        },
      ],
    });
    const current = harness.agents.findByPane({
      herdrSessionName: "default",
      paneId: "wJ:p2",
      paneGeneration: "gen-2",
    });
    expect(current?.id).toBe(original.id);
    expect(current?.paneGeneration).toBe("gen-2");

    const orchestrator = new AgentOrchestratorService({
      agentEvents: harness.agentEvents,
      agents: harness.agents,
      scopes: harness.agentOrchestratorScopes,
    });
    orchestrator.claim({
      herdrSessionName: "default",
      workspaceId: "wJ",
      paneId: "wJ:owner",
      terminalId: "term_owner",
    });

    const row = harness.statusEventPlans.insertPending({
      agent: original,
      from: "working",
      to: "done",
    });
    for (let i = 0; i < 8; i += 1) {
      harness.statusEventPlans.markRetry(row.id, new Error("RETRY_EXHAUSTED"));
    }
    await index.drainPendingPlans();

    const failed = harness.sqlite
      .prepare("select * from agent_events where type = 'agent.failed'")
      .all() as Array<Record<string, unknown>>;
    expect(failed).toHaveLength(1);
    expect(failed[0]).toMatchObject({
      agent_id: original.id,
      deliverable: 0,
      pane_generation: "gen-2",
      status: "acked",
    });
    expect(JSON.parse(String(failed[0]?.payload_json))).toMatchObject({
      agentId: original.id,
      paneId: "wJ:p2",
    });

    // 存量补录是静默入库：不投递给当前 owner。
    expect(
      orchestrator.pending({
        herdrSessionName: "default",
        workspaceId: "wJ",
        terminalId: "term_owner",
      }),
    ).toEqual([]);
    harness.sqlite.close();
  });

  test("W6: agent.failed is still delivered to owner after agent row is deleted", async () => {
    const harness = openObservabilityDbHarness();
    let failRefresh = false;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: {
        async resolveCompactHistory() {
          if (failRefresh) throw new Error("simulated disk error");
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: { ref: "ref-1", text: "old", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
      turnCompletions: new TurnCompletionRegistry({ timeoutMs: 0 }),
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.listForHerdrSession("default")[0];
    if (!agent) throw new Error("expected agent");

    const orchestrator = new AgentOrchestratorService({
      agentEvents: harness.agentEvents,
      agents: harness.agents,
      scopes: harness.agentOrchestratorScopes,
    });
    orchestrator.claim({
      herdrSessionName: "default",
      workspaceId: "wJ",
      paneId: "wJ:owner",
      terminalId: "term_owner",
    });

    // The plan is one round away from exhaustion when the pane is retired and the
    // agents row is physically deleted: this is the H1 write path (the retry
    // refresh fails and the terminal event is appended with the agent row gone).
    const row = harness.statusEventPlans.insertPending({ agent, from: "working", to: "done" });
    for (let i = 0; i < STATUS_PLAN_MAX_ATTEMPTS - 1; i += 1) {
      expect(harness.statusEventPlans.markRetry(row.id, new Error("degraded"))?.status).toBe(
        "pending",
      );
    }
    harness.sqlite.prepare("delete from agents where id = ?").run(agent.id);
    failRefresh = true;

    const failed = await index.executeStatusEventPlan({
      agent,
      compactHistory: undefined,
      from: "working",
      planId: row.id,
      to: "done",
    });

    expect(failed?.type).toBe("agent.failed");
    // 修复前：agents 行不存在导致 FOREIGN KEY constraint failed，事件从未落库。
    // 修复后：降级为孤儿行写入，原始 agent id 保留在 payload 里。
    expect(failed?.agentId).toBeNull();
    expect((failed?.payload as Record<string, unknown> | undefined)?.agentId).toBe(agent.id);
    expect(harness.agentEvents.get(failed?.id ?? 0)).toMatchObject({
      agentId: null,
      deliverable: 1,
      status: "pending",
      type: "agent.failed",
    });

    const pending = orchestrator.pending({
      herdrSessionName: "default",
      workspaceId: "wJ",
      terminalId: "term_owner",
    });
    expect(pending.map((event) => event.id)).toContain(failed?.id);
    harness.sqlite.close();
  });

  test("H1: an undelivered orphan failed row is not swept by a later cursor ack", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.listForHerdrSession("default")[0];
    if (!agent) throw new Error("expected agent");

    const orchestrator = new AgentOrchestratorService({
      agentEvents: harness.agentEvents,
      agents: harness.agents,
      scopes: harness.agentOrchestratorScopes,
    });
    orchestrator.claim({
      herdrSessionName: "default",
      workspaceId: "wJ",
      paneId: "wJ:owner",
      terminalId: "term_owner",
    });

    // 孤儿终态行：agents 行已物理删除后补写，agent_id 为 null。
    const orphan = harness.agentEvents.append({
      agentId: null,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      payload: { agentId: agent.id, reason: "RETRY_EXHAUSTED" },
      terminalId: "term_gone",
      type: "agent.failed",
      workspaceId: "wJ",
    });
    const later = harness.agentEvents.append({
      agentId: agent.id,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      payload: { from: "working", to: "done" },
      terminalId: "term_claude",
      type: "agent.done",
      workspaceId: "wJ",
    });
    expect(orphan.id).toBeLessThan(later.id);

    // 顺序守卫必须能看到孤儿行，否则 ack 路径不会拦下后到的 cursor ack。
    expect(
      harness.agentEvents.nextDeliverableAfter({
        afterEventId: 0,
        herdrSessionName: "default",
        ownerTerminalId: "term_owner",
        workspaceId: "wJ",
      }),
    ).toMatchObject({ id: orphan.id });

    // 只投递后一条，再尝试 ack：孤儿行未投递且位于 cursor 之前，ack 必须被拒绝，
    // 而不是被 markAcked(id <= cursor) 静默扫成已确认。
    expect(
      harness.agentEvents.reservePending("term_owner", 100, [later.id]).map((event) => event.id),
    ).toEqual([later.id]);
    expect(() =>
      orchestrator.ack({
        herdrSessionName: "default",
        workspaceId: "wJ",
        terminalId: "term_owner",
        eventId: later.id,
      }),
    ).toThrowError(/Only the next pending orchestrator event/);
    expect(harness.agentEvents.get(orphan.id)).toMatchObject({
      deliverable: 1,
      status: "pending",
    });
    harness.sqlite.close();
  });

  test("H1: backfill stitches pane metadata by generation and never borrows a newer instance", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return snapshot(
            [
              agent({
                pane_generation: "gen-1",
                pane_id: "wJ:p2",
                revision: 10,
                terminal_id: "term_old",
                workspace_id: "wJ",
              }),
            ],
            [{ pane_id: "wJ:p2", revision: 10 }],
          );
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const oldInstance = harness.agents.findByPane({
      herdrSessionName: "default",
      paneId: "wJ:p2",
      paneGeneration: "gen-1",
    });
    if (!oldInstance) throw new Error("expected agent");

    // The old instance's own event carries the metadata the stitch has to find.
    harness.agentEvents.append({
      agentId: oldInstance.id,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      paneGeneration: "gen-1",
      payload: { from: "working", to: "done" },
      terminalId: "term_old",
      type: "agent.status.changed",
      workspaceId: "wJ",
    });

    const row = harness.statusEventPlans.insertPending({
      agent: oldInstance,
      from: "working",
      to: "done",
    });
    for (let i = 0; i < STATUS_PLAN_MAX_ATTEMPTS; i += 1) {
      harness.statusEventPlans.markRetry(row.id, new Error("RETRY_EXHAUSTED"));
    }
    expect(harness.statusEventPlans.get(row.id).status).toBe("failed");

    // A newer instance reuses the same paneId. Its event is back-dated so only the
    // generation filter can keep it away from the old plan.
    const newcomer = harness.agentEvents.append({
      agentId: null,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      paneGeneration: "gen-2",
      payload: { from: "working", to: "done" },
      terminalId: "term_new",
      type: "agent.status.changed",
      workspaceId: "wJ",
    });
    harness.sqlite
      .prepare("update agent_events set created_at = ? where id = ?")
      .run(row.createdAt.getTime() - 1, newcomer.id);

    // Pane retired: the agents row is physically deleted and ON DELETE SET NULL
    // clears agent_id on every event, so the legacy agent_id stitch finds nothing.
    harness.sqlite.prepare("delete from agents where id = ?").run(oldInstance.id);

    const infoSpy = vi.spyOn(console, "info").mockImplementation(() => {});
    try {
      await index.drainPendingPlans();

      const failed = harness.sqlite
        .prepare("select * from agent_events where type = 'agent.failed'")
        .all() as Array<Record<string, unknown>>;
      expect(failed).toHaveLength(1);
      // 旧 plan 的补录必须取旧实例的 metadata，绝不能挂到同 pane 的新实例上。
      expect(failed[0]).toMatchObject({
        agent_id: null,
        pane_generation: "gen-1",
        terminal_id: "term_old",
        workspace_id: "wJ",
      });
      expect(failed[0]?.terminal_id).not.toBe("term_new");

      expect(infoSpy).toHaveBeenCalledWith("Herdsman backfilled agent.failed rows", {
        backfilled: 1,
        skipped: 0,
        total: 1,
      });
    } finally {
      infoSpy.mockRestore();
    }
    harness.sqlite.close();
  });

  test("W14: claude first done succeeds; keyed same content skips; keyed new ref emits", async () => {
    const harness = openObservabilityDbHarness();
    let historyRef: string | null = "ref-1";
    let historyText = "turn-1";
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
      sleep: async () => {},
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const first = await index.executeStatusEventPlan({
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "turn-1", timestamp: null },
      },
      from: "working",
      to: "done",
    });
    expect(first?.type).toBe("agent.done");
    expect(first?.compactHistory?.lastAssistantMessage?.ref).toBe("ref-1");

    const k2 = await index.executeStatusEventPlan({
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "turn-1", timestamp: null },
      },
      from: "working",
      herdrEventKey: "k2",
      to: "done",
    });
    expect(k2).toBeUndefined();
    const afterK2 = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((event) => event.type === "agent.done");
    expect(afterK2).toHaveLength(1);
    const k2Plans = harness.sqlite
      .prepare("select status from status_event_plans where herdr_event_key = ?")
      .all("k2") as Array<{ status: string }>;
    expect(k2Plans).toEqual([{ status: "completed" }]);

    historyRef = "ref-2";
    historyText = "turn-2";
    const k3 = await index.executeStatusEventPlan({
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-2", text: "turn-2", timestamp: null },
      },
      from: "working",
      herdrEventKey: "k3",
      to: "done",
    });
    expect(k3?.type).toBe("agent.done");
    expect(k3?.compactHistory?.lastAssistantMessage?.ref).toBe("ref-2");
    const doneEvents = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((event) => event.type === "agent.done");
    expect(doneEvents).toHaveLength(2);
    harness.sqlite.close();
  });

  test("drain short-circuits when queued plan has already reached terminal status (completed/cancelled/failed/discarded) and does not revive it", async () => {
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
              lastAssistantMessage: { ref: "ref-1", text: "turn-1", timestamp: null },
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

    const terminalStatuses = ["completed", "cancelled", "failed", "discarded"] as const;
    const planRows: Array<{ id: number; status: string; attempts: number }> = [];

    for (const status of terminalStatuses) {
      const row = harness.statusEventPlans.insertPending({
        agent,
        compactHistory: {
          ...emptyCompactHistory("claude-jsonl"),
          lastAssistantMessage: { ref: "ref-1", text: "turn-1", timestamp: null },
        },
        from: "working",
        to: "done",
      });
      if (status === "completed") {
        harness.statusEventPlans.markCompleted(row.id);
      } else if (status === "cancelled") {
        harness.statusEventPlans.markCancelled(row.id);
      } else if (status === "discarded") {
        harness.statusEventPlans.markDiscarded(row.id, "TEST_DISCARD");
      } else if (status === "failed") {
        for (let i = 0; i < 8; i += 1) {
          harness.statusEventPlans.markRetry(row.id, new Error("PLAN_WAITING_HISTORY"));
        }
        // Force status to failed for testing terminal failed row
        harness.sqlite
          .prepare(
            "update status_event_plans set status = 'failed', last_error = 'PLAN_WAITING_HISTORY' where id = ?",
          )
          .run(row.id);
      }
      const record = harness.statusEventPlans.get(row.id);
      planRows.push({ attempts: record.attempts, id: row.id, status });
    }

    await index.drainPendingPlans();

    // Verify none of the terminal rows were revived or modified
    for (const { attempts, id, status } of planRows) {
      const current = harness.statusEventPlans.get(id);
      expect(current.status).toBe(status);
      expect(current.attempts).toBe(attempts);
    }

    // No agent.done events should have been created by draining terminal rows
    const doneEvents = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((e) => e.type === "agent.done");
    expect(doneEvents).toEqual([]);

    harness.sqlite.close();
  });

  test("#retryWaitingPlanRow clears waiting timer on discarded and failed transitions without residue in timer map", async () => {
    const harness = openObservabilityDbHarness();
    const scheduledCallbacks = new Map<unknown, () => Promise<void> | void>();
    const clearedTimers: unknown[] = [];

    let timerSeq = 1;
    const scheduleRetryMock = vi.fn((callback: () => Promise<void> | void) => {
      const id = timerSeq++;
      scheduledCallbacks.set(id, callback);
      return id;
    });
    const clearRetryMock = vi.fn((timer: unknown) => {
      clearedTimers.push(timer);
      scheduledCallbacks.delete(timer);
    });

    let failRefresh = false;
    const index = new AgentIndexService({
      clearRetry: clearRetryMock,
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "claude");
        },
      }),
      history: {
        async resolveCompactHistory() {
          if (failRefresh) {
            throw new Error("simulated disk error");
          }
          return {
            compactHistory: {
              ...emptyCompactHistory("claude-jsonl"),
              lastAssistantMessage: { ref: "ref-1", text: "turn-1", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      scheduleRetry: scheduleRetryMock,
      sleep: async () => {},
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    // 1. Test discarded branch of #retryWaitingPlanRow:
    // Create a plan with 6 attempts.
    const discardedRow = harness.statusEventPlans.insertPending({
      agent,
      from: "working",
      to: "done",
    });
    for (let i = 1; i <= 6; i += 1) {
      harness.statusEventPlans.markRetry(discardedRow.id, new Error("PLAN_WAITING_HISTORY"));
    }

    // executeStatusEventPlan with failRefresh makes attempt 7 (pending) and schedules retry timer
    failRefresh = true;
    await index.executeStatusEventPlan({
      agent,
      compactHistory: undefined,
      from: "working",
      planId: discardedRow.id,
      to: "done",
    });

    expect(harness.statusEventPlans.get(discardedRow.id).status).toBe("pending");
    expect(harness.statusEventPlans.get(discardedRow.id).attempts).toBe(7);
    expect(scheduleRetryMock).toHaveBeenCalledTimes(1);
    const retryTimerId = scheduleRetryMock.mock.results[0]?.value;
    const retryCallback = scheduledCallbacks.get(retryTimerId);
    if (!retryCallback) throw new Error("expected scheduled retry callback");

    // Clear record of clearRetry calls
    clearRetryMock.mockClear();

    // Now execute the scheduled retry callback (this runs #retryWaitingPlanRow for 8th attempt -> discarded)
    await retryCallback();

    expect(harness.statusEventPlans.get(discardedRow.id).status).toBe("discarded");
    expect(harness.statusEventPlans.get(discardedRow.id).attempts).toBe(8);

    // clearRetry must have been called during #retryWaitingPlanRow
    expect(clearRetryMock).toHaveBeenCalledWith(retryTimerId);

    // Calling stopWaitingHistoryRetries() must find no residue in the timer map
    clearRetryMock.mockClear();
    index.stopWaitingHistoryRetries();
    expect(clearRetryMock).not.toHaveBeenCalled();

    // Verify agent.discarded event was emitted
    const discardedEvents = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((e) => e.type === "agent.discarded");
    expect(discardedEvents).toHaveLength(1);
    expect(discardedEvents[0]?.payload).toMatchObject({
      attempts: 8,
      from: "working",
      planId: discardedRow.id,
      reason: "PLAN_WAITING_HISTORY",
      to: "done",
    });

    harness.sqlite.close();
  });

  test("#runPlanRow general error catch path emits agent.discarded when retry exhausted to discarded", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => ({
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "initial", timestamp: null },
      })),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const row = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "initial", timestamp: null },
      },
      from: "working",
      to: "done",
    });
    // Set attempts to 7 so the next retry exhausts attempts to 8
    for (let i = 0; i < 7; i += 1) {
      harness.statusEventPlans.markRetry(row.id, new Error("PLAN_WAITING_HISTORY"));
    }
    expect(harness.statusEventPlans.get(row.id).attempts).toBe(7);

    // Mock agentEvents.append so the first append inside #appendStatusEvents throws Error("PLAN_WAITING_HISTORY")
    let appendCallCount = 0;
    const originalAppend = harness.agentEvents.append.bind(harness.agentEvents);
    harness.agentEvents.append = (input: Parameters<typeof originalAppend>[0]) => {
      appendCallCount += 1;
      if (appendCallCount === 1) {
        throw new Error("PLAN_WAITING_HISTORY");
      }
      return originalAppend(input);
    };

    // executeStatusEventPlan with planId will invoke #runPlanRow on the existing row
    const result = await index.executeStatusEventPlan({
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "initial", timestamp: null },
      },
      from: "working",
      planId: row.id,
      to: "done",
    });

    expect(result).toBeDefined();
    expect(result?.type).toBe("agent.discarded");
    expect(harness.statusEventPlans.get(row.id).status).toBe("discarded");
    expect(harness.statusEventPlans.get(row.id).attempts).toBe(8);

    const discardedEvents = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((e) => e.type === "agent.discarded");
    expect(discardedEvents).toHaveLength(1);
    expect(discardedEvents[0]?.payload).toMatchObject({
      attempts: 8,
      from: "working",
      planId: row.id,
      reason: "PLAN_WAITING_HISTORY",
      to: "done",
    });

    harness.sqlite.close();
  });

  test("#runPlanRow gracefully handles markRetry returning null when changes === 0", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => ({
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "initial", timestamp: null },
      })),
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const row = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "initial", timestamp: null },
      },
      from: "working",
      to: "done",
    });

    // Mock agentEvents.append so it concurrently marks row completed in DB and throws
    let appendCallCount = 0;
    const originalAppend = harness.agentEvents.append.bind(harness.agentEvents);
    harness.agentEvents.append = (input: Parameters<typeof originalAppend>[0]) => {
      appendCallCount += 1;
      if (appendCallCount === 1) {
        harness.sqlite
          .prepare("update status_event_plans set status = 'completed' where id = ?")
          .run(row.id);
        throw new Error("UNEXPECTED_ERROR");
      }
      return originalAppend(input);
    };

    // executeStatusEventPlan enters #runPlanRow, which catches the error, calls markRetry.
    // Since row was modified to 'completed', markRetry returns null (changes === 0).
    // #runPlanRow should return undefined gracefully without crashing.
    const result = await index.executeStatusEventPlan({
      agent,
      compactHistory: {
        ...emptyCompactHistory("claude-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "initial", timestamp: null },
      },
      from: "working",
      planId: row.id,
      to: "done",
    });

    expect(result).toBeUndefined();
    expect(harness.statusEventPlans.get(row.id).status).toBe("completed");

    harness.sqlite.close();
  });
});

describe("agy late startup idle supersession (p76 regression)", () => {
  test("P76: the late unknown->idle plan is superseded and the following working->done still emits agent.done", async () => {
    const harness = openObservabilityDbHarness();
    let agentStatus = "idle";
    let assistantRef: string | null = null;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent(agentStatus, 10, "agy");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("antigravity-sqlite"),
              lastAssistantMessage: assistantRef
                ? { ref: assistantRef, text: `output ${assistantRef}`, timestamp: null }
                : null,
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

    // 1. The pane announces idle before it is indexed: an unknown -> idle plan is
    //    created while history is still empty, so it stays pending in WAITING.
    const startup = await index.handleHerdrEvent({
      event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });
    expect(startup.events).toEqual([]);
    const staleRow = harness.statusEventPlans.listUnfinished()[0];
    if (!staleRow) throw new Error("expected pending startup idle plan");
    expect(staleRow).toMatchObject({
      attempts: 1,
      fromStatus: "unknown",
      lastError: "PLAN_WAITING_HISTORY",
      status: "pending",
      toStatus: "idle",
    });

    // 2. The pane starts working: the newer transition invalidates the stale
    //    startup idle plan before it can complete late.
    agentStatus = "working";
    const working = await index.handleHerdrEvent({
      event: {
        agent_status: "working",
        event_id: "ev-work",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...sessionInput(),
    });
    expect(working.events.map((event) => event.type)).toEqual(["agent.status.changed"]);
    expect(harness.statusEventPlans.get(staleRow.id)).toMatchObject({
      lastError: "PLAN_SUPERSEDED",
      status: "cancelled",
    });

    // 3. The final assistant ref appears. Draining plans must not resurrect the
    //    cancelled startup idle plan and must not let it register that ref.
    assistantRef = "#entry=23";
    await index.drainPendingPlans();
    const afterDrain = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(afterDrain.map((event) => event.type)).toEqual(["agent.status.changed"]);

    // 4. The round finishes with the same ref the stale plan captured: the
    //    working -> done plan must still emit agent.done (and never discarded).
    agentStatus = "done";
    const done = await index.handleHerdrEvent({
      event: {
        agent_status: "done",
        event_id: "ev-done",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...sessionInput(),
    });
    expect(done.events.map((event) => event.type)).toEqual(["agent.done"]);
    expect(done.events[0]?.payload).toMatchObject({ from: "working", to: "done" });
    expect(done.events[0]?.compactHistory?.lastAssistantMessage?.ref).toBe("#entry=23");

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.filter((event) => event.type === "agent.done")).toHaveLength(1);
    expect(allEvents.filter((event) => event.type === "agent.idle")).toHaveLength(0);
    expect(allEvents.filter((event) => event.type === "agent.discarded")).toHaveLength(0);
    expect(harness.statusEventPlans.listUnfinished()).toEqual([]);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("agy idle delivered from unknown does not consume the final ref; a repeated working->done with the same ref skips instead of discarding", async () => {
    const harness = openObservabilityDbHarness();
    let agentStatus = "idle";
    const assistantRef = "#entry=23";
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent(agentStatus, 10, "agy");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("antigravity-sqlite"),
              lastAssistantMessage: {
                ref: assistantRef,
                text: `output ${assistantRef}`,
                timestamp: null,
              },
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

    // 1. Recovered pane idle from unknown: the idle event is emitted (it is not
    //    deliverable, but it is recorded).
    const startup = await index.handleHerdrEvent({
      event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });
    expect(startup.events.map((event) => event.type)).toEqual(["agent.idle"]);
    expect(startup.events[0]?.payload).toMatchObject({ from: "unknown", to: "idle" });

    // 2. The pane works and then finishes with the same assistant ref: the
    //    unknown -> idle row must not make this round look like a duplicate.
    agentStatus = "working";
    await index.handleHerdrEvent({
      event: {
        agent_status: "working",
        event_id: "ev-work",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...sessionInput(),
    });
    agentStatus = "done";
    const done = await index.handleHerdrEvent({
      event: {
        agent_status: "done",
        event_id: "ev-done",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...sessionInput(),
    });
    expect(done.events.map((event) => event.type)).toEqual(["agent.done"]);
    expect(done.events[0]?.compactHistory?.lastAssistantMessage?.ref).toBe(assistantRef);

    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected indexed agent");

    // 3. A repeated working -> done legacy plan with the already delivered ref is
    //    a genuine skip: completed, no extra event, never discarded.
    const duplicate = await index.executeStatusEventPlan({
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: {
          ref: assistantRef,
          text: `output ${assistantRef}`,
          timestamp: null,
        },
      },
      from: "working",
      to: "done",
    });
    expect(duplicate).toBeUndefined();
    const duplicateRow = harness.statusEventPlans.listUnfinished();
    expect(duplicateRow).toEqual([]);

    // 4. The same already delivered ref on a retry row skips as well instead of
    //    exhausting the budget into discarded.
    const retryRow = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: {
          ref: assistantRef,
          text: `output ${assistantRef}`,
          timestamp: null,
        },
      },
      from: "working",
      to: "done",
    });
    harness.statusEventPlans.markRetry(retryRow.id, new Error("PLAN_WAITING_HISTORY"));
    await index.drainPendingPlans();
    expect(harness.statusEventPlans.get(retryRow.id).status).toBe("completed");

    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.filter((event) => event.type === "agent.done")).toHaveLength(1);
    expect(allEvents.filter((event) => event.type === "agent.discarded")).toHaveLength(0);
    expect(allEvents.filter((event) => event.type === "agent.idle")).toHaveLength(1);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("agy with history that never becomes non-empty still ends in PLAN_WAITING_HISTORY (pending, then discarded budget)", async () => {
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
      scheduleRetry: () => 1,
      sleep: async () => {},
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const result = await index.handleHerdrEvent({
      event: {
        agent_status: "done",
        event_id: "ev-done",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...sessionInput(),
    });
    expect(result.events).toEqual([]);
    const pending = harness.statusEventPlans.listUnfinished();
    expect(pending).toHaveLength(1);
    expect(pending[0]).toMatchObject({
      attempts: 1,
      lastError: "PLAN_WAITING_HISTORY",
      status: "pending",
      toStatus: "done",
    });
    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.filter((event) => event.type === "agent.discarded")).toHaveLength(0);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("S5: waitForHistoryAdvance uses exponential backoff up to 16s and respects 30s total budget", async () => {
    const harness = openObservabilityDbHarness();
    const delays: number[] = [];
    let clock = 0;
    const now = () => clock;
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
              lastAssistantMessage: { ref: "r1", text: "old", timestamp: null, stopReason: "stop" },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      now,
      sleep: async (ms: number) => {
        delays.push(ms);
        clock += ms;
      },
      stores: harness,
      turnCompletions: new TurnCompletionRegistry({ sleep: async () => {}, timeoutMs: 100 }),
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const plan = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: { ref: "r1", text: "old", timestamp: null, stopReason: "stop" },
      },
      from: "working",
      to: "done",
    });

    const _result = await index.executeStatusEventPlan({
      agent,
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: { ref: "r1", text: "old", timestamp: null, stopReason: "stop" },
      },
      from: "working",
      to: "done",
      planId: plan.id,
    });
    // The drain emits a degraded event (no history advance) and calls markRetry,
    // leaving the plan pending. result is undefined because the plan was retried.
    // Verify the degraded record in DB instead.
    const dbEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const _degradedInDb = dbEvents.filter(
      (e) =>
        e.type === "agent.done" &&
        e.payload &&
        typeof e.payload === "object" &&
        (e.payload as Record<string, unknown>).degraded &&
        (e.payload as Record<string, unknown>).degradedReason === "no_advance_from_input",
    );
    // Verify the plan is pending (retry was scheduled).
    const updatedPlan = harness.statusEventPlans.get(plan.id);
    expect(updatedPlan.status).toBe("pending");
    // Verify backoff sequence: first delay 500ms, final delay capped at 16000ms.
    expect(delays[0]).toBe(500);
    if (delays.length > 0) {
      expect(delays[delays.length - 1]).toBe(16000);
    }
    // Verify exponential growth (each delay >= previous, except cap).
    for (let i = 1; i < delays.length; i++) {
      expect(delays[i]).toBeGreaterThanOrEqual(delays.at(i - 1) as number);
    }
    // The loop may let one final sleep exceed 30s by up to one maxDelay step (16s).
    expect(delays.reduce((sum, d) => sum + d, 0)).toBeLessThanOrEqual(31500);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("S6: plan retry after 30s budget catches recovered stop message", async () => {
    const harness = openObservabilityDbHarness();
    const turnRegistry = new TurnCompletionRegistry({ sleep: async () => {} });
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
              lastAssistantMessage: {
                ref: "e1",
                text: "error: terminated",
                timestamp: null,
                stopReason: "error",
              },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      sleep: async () => {},
      stores: harness,
      turnCompletions: turnRegistry,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const plan = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: {
          ref: "e1",
          text: "error: terminated",
          timestamp: null,
          stopReason: "error",
        },
      },
      from: "working",
      to: "done",
    });
    // Promote to retry so the first drain enters the retry path.
    harness.statusEventPlans.markRetry(plan.id, new Error("PLAN_WAITING_HISTORY"));

    turnRegistry.record({
      confirmed: true,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    await index.drainPendingPlans();
    const updated = harness.statusEventPlans.get(plan.id);
    expect(updated.status).toBe("pending");
    expect(updated.attempts).toBe(2);

    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(events.filter((e) => e.type === "agent.done")).toHaveLength(0);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("S7: degraded emission on expectedText mismatch after wait", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ sleep: async () => {}, timeoutMs: 3_000 });
    let callCount = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "pi");
        },
      }),
      history: {
        async resolveCompactHistory() {
          callCount += 1;
          const old = {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "old-ref",
                text: "old",
                timestamp: null,
                stopReason: "stop",
              },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
          const intermediate = {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "m1",
                text: "intermediate text",
                timestamp: null,
                stopReason: "stop",
              },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
          const result = callCount <= 2 ? old : intermediate;
          return result;
        },
      } as unknown as AgentHistoryService,
      sleep: async () => {},
      stores: harness,
      turnCompletions: registry,
    });

    await index.refreshHerdrSession(sessionInput());
    // Pre-record signal so drain picks it up synchronously.
    registry.record({
      confirmed: true,
      expectedText: "final answer",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });
    const pending = index.handleHerdrEvent(doneEvent);

    const _result = await pending;
    // Degraded event is invalidated in-DB and not delivered to callers.
    expect(_result.events.filter((e) => e.type === "agent.done")).toHaveLength(0);
    const listAfterEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(listAfterEvents.filter((e) => e.type === "agent.done")).toHaveLength(0);
    // Verify the degraded record in DB.
    const invalidatedRows = harness.sqlite
      .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
      .all();
    expect(invalidatedRows).toHaveLength(1);
    const degraded = invalidatedRows[0];
    if (!degraded) throw new Error("expected degraded event row");
    const payload = JSON.parse(degraded.payload_json as string);
    expect(payload.degradedReason).toBe("expected_text_mismatch");
    harness.sqlite.close();
  }, 10_000);

  test("S8: degraded event retries until history matches expectedText then completes", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ sleep: async () => {}, timeoutMs: 3_000 });
    let callCount = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "pi");
        },
      }),
      history: {
        async resolveCompactHistory() {
          callCount += 1;
          if (callCount <= 2) {
            return {
              compactHistory: {
                ...emptyCompactHistory("pi-jsonl"),
                lastAssistantMessage: {
                  ref: callCount === 1 ? "old-ref" : "intermediate-ref",
                  text: callCount === 1 ? "old" : "intermediate text",
                  timestamp: null,
                  stopReason: "stop",
                },
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          }
          if (callCount === 3) {
            return {
              compactHistory: {
                ...emptyCompactHistory("pi-jsonl"),
                lastAssistantMessage: {
                  ref: "mismatch-ref",
                  text: "still not final",
                  timestamp: null,
                  stopReason: "stop",
                },
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          }
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "final-ref",
                text: "final answer",
                timestamp: null,
                stopReason: "stop",
              },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      sleep: async () => {},
      stores: harness,
      turnCompletions: registry,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const plan = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: {
          ref: "old-ref",
          text: "old",
          timestamp: null,
          stopReason: "stop",
        },
      },
      from: "working",
      to: "done",
    });

    registry.record({
      confirmed: true,
      expectedText: "final answer",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    await index.drainPendingPlans();
    // First drain emitted a degraded event (expected_text_mismatch) and called
    // markRetry, which invalidated the event in DB and set plan to pending.
    const firstInvalidatedRows = harness.sqlite
      .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
      .all();
    expect(firstInvalidatedRows).toHaveLength(1);
    const firstDegraded = firstInvalidatedRows[0];
    if (!firstDegraded) throw new Error("expected degraded event row");
    const firstPayload = JSON.parse(firstDegraded.payload_json as string);
    expect(firstPayload.degradedReason).toBe("expected_text_mismatch");

    const firstUpdated = harness.statusEventPlans.get(plan.id);
    expect(firstUpdated.status).toBe("pending");
    expect(firstUpdated.attempts).toBeGreaterThanOrEqual(1);
    expect(firstUpdated.lastError).toBe("degraded");

    // Re-record the turn signal for the retry drain (the first drain consumed it).
    registry.record({
      confirmed: true,
      expectedText: "final answer",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    // Second drain: reset running→pending and drain again; history now matches
    // expectedText, so the complete event is emitted and plan is completed.
    await index.drainPendingPlans();
    const completed = harness.statusEventPlans.get(plan.id);
    expect(completed.status).toBe("completed");

    const finalEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const validDones = finalEvents.filter(
      (e) => e.type === "agent.done" && e.status !== "invalidated",
    );
    expect(validDones).toHaveLength(1);
    expect(validDones[0]?.compactHistory?.lastAssistantMessage?.text).toBe("final answer");

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("legacy degraded rows are invalidated by real upgrade migration and hidden from delivery", async () => {
    const { mkdtempSync, rmSync } = await import("node:fs");
    const { tmpdir } = await import("node:os");
    const { join } = await import("node:path");
    const { openSqlite } = await import("@/db/client.js");
    const { applyMigrations } = await import("@/db/apply-migrations.js");

    // Use a fresh DB so the migration journal is clean (no prior __drizzle_migrations rows).
    const dir = mkdtempSync(join(tmpdir(), "herdsman-legacy-migrate-"));
    const dbPath = join(dir, "test.sqlite");
    const { sqlite } = openSqlite(dbPath);
    try {
      // Apply all migrations including 0010_legacy_degraded_cleanup.
      applyMigrations(sqlite, { migrationsFolder: "drizzle" });

      // Insert required parent rows so FK constraints are satisfied.
      sqlite
        .prepare(
          `insert into herdr_sessions (name, running, session_dir, socket_path, updated_at)
           values (?, 1, '/tmp', '/tmp/s.sock', ?)`,
        )
        .run("default", Date.now());
      sqlite
        .prepare(
          `insert into agents (id, herdr_session_name, pane_id, workspace_id, agent_status, focused, first_seen_at, last_seen_at)
           values (?, ?, ?, ?, 'working', 0, ?, ?)`,
        )
        .run("agent-legacy", "default", "wJ:p2", "wJ", Date.now(), Date.now());

      // Simulate legacy degraded rows written before the fix: status='pending',
      // payload_json.degraded=true, empty lastAssistantMessage.
      const legacyPayload = JSON.stringify({
        degraded: true,
        degradedReason: "non_terminal_assistant",
      });
      const emptyHistory = JSON.stringify({ lastAssistantMessage: null });
      sqlite
        .prepare(
          `insert into agent_events
           (herdr_session_name, agent_id, pane_id, workspace_id, terminal_id, type, payload_json, compact_history_json, deliverable, status, delivery_attempts, created_at)
           values (?, ?, ?, ?, ?, 'agent.done', ?, ?, 1, 'pending', 0, ?)`,
        )
        .run(
          "default",
          "agent-legacy",
          "wJ:p2",
          "wJ",
          "term_claude",
          legacyPayload,
          emptyHistory,
          Date.now(),
        );

      // Step 3: simulate a pre-0010 database by removing only the 0010 migration record.
      // Schema stays at 0009 shape; legacy rows are present in agent_events.
      sqlite
        .prepare("delete from __drizzle_migrations where hash = ?")
        .run("52f3d8bfd24cc5b9e380d8e03cbf0d479034b75c16469a15683939111c82a26e");

      // Step 4: run the real migration runner — 0010 must execute against existing data.
      applyMigrations(sqlite, { migrationsFolder: "drizzle" });

      // Assert 0010 was recorded by THIS call (created_at must equal the 0010 folderMillis).
      const appliedRows = sqlite
        .prepare("select id, hash, created_at from __drizzle_migrations where hash = ?")
        .all("52f3d8bfd24cc5b9e380d8e03cbf0d479034b75c16469a15683939111c82a26e");
      expect(appliedRows).toHaveLength(1);
      expect(appliedRows[0]?.created_at).toBe(1790610020312);

      // Assert legacy row is invalidated by the 0010 migration SQL (not by hand-written exec).
      const invalidatedRows = sqlite
        .prepare(
          "select * from agent_events where status = 'invalidated' and type = 'agent.done' and invalidated_reason = 'legacy_degraded'",
        )
        .all();
      expect(invalidatedRows).toHaveLength(1);

      // listAfter must not return it.
      const agentEvents = new AgentEventStore(sqlite);
      const listAfterEvents = agentEvents.listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      });
      expect(listAfterEvents.filter((e) => e.type === "agent.done")).toHaveLength(0);

      // nextDeliverableAfter must not return it.
      const nextDeliverable = agentEvents.nextDeliverableAfter({
        afterEventId: 0,
        herdrSessionName: "default",
        ownerTerminalId: "term_claude",
        workspaceId: "wJ",
      });
      expect(nextDeliverable).toBeUndefined();

      // latestTerminalEvent must not return it.
      const latestTerminal = agentEvents.latestTerminalEvent("agent-legacy", "default");
      expect(latestTerminal).toBeUndefined();
    } finally {
      sqlite.close();
      rmSync(dir, { force: true, recursive: true });
    }
  });

  test("degraded done exhausting retries emits deliverable fallback failed event", async () => {
    const harness = openObservabilityDbHarness();
    const pushedEvents: AgentEventRecord[] = [];
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: null,
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      onAgentEvent: (event) => {
        pushedEvents.push(event);
      },
      stores: harness,
      turnCompletions: new TurnCompletionRegistry({ timeoutMs: 0 }),
    });

    await index.refreshHerdrSession(sessionInput());

    const initialAgent = harness.agents.listForHerdrSession("default")[0];
    if (!initialAgent) throw new Error("expected agent");

    // Insert a plan at max attempts - 1 so the next #runPlanRow execution hits the failed branch.
    const plan = harness.statusEventPlans.insertPending({
      agent: initialAgent,
      from: "working",
      to: "done",
    });
    for (let i = 0; i < STATUS_PLAN_MAX_ATTEMPTS - 1; i += 1) {
      const retried = harness.statusEventPlans.markRetry(plan.id, new Error("degraded"));
      expect(retried?.status).toBe("pending");
    }

    const agent = harness.agents.listForHerdrSession("default")[0];
    if (!agent) throw new Error("expected agent");

    // executeStatusEventPlan will run #runPlanRow with the near-exhausted plan.
    const result = await index.executeStatusEventPlan({
      agent,
      compactHistory: undefined,
      from: "working",
      planId: plan.id,
      to: "done",
    });

    // #runPlanRow transitions the plan to failed and returns the fallback event.
    // The single publication path forwards this return value (see below), so
    // there is no onAgentEvent capture to fall back on here.
    const failedEvent = result;
    expect(failedEvent).toBeDefined();
    expect(failedEvent?.type).toBe("agent.failed");
    expect(failedEvent?.deliverable).toBe(1);
    const payload = failedEvent?.payload as Record<string, unknown> | undefined;
    expect(payload?.fallbackOutcome).toBe(true);

    // 发布路径唯一：`executeStatusEventPlan` 只返回事件，不再自行推流；推流由调用方
    // 转发该返回值完成（herdr-session-watch-manager.ts:360 #submitPlan -> service.ts:206
    // -> observability-server.ts:244 publishAgentEvent）。index 内部若再推一次，
    // socket 上会出现两条相同的 `agent.event`，破坏合同 §4 的“恰好一次”。
    expect(result?.type).toBe("agent.failed");
    expect(pushedEvents.filter((e) => e.type === "agent.failed")).toHaveLength(0);

    harness.sqlite.close();
  });

  test("degraded retry row failing its retry-round refresh exhausts as wakeable failed", async () => {
    const harness = openObservabilityDbHarness();
    const pushedEvents: AgentEventRecord[] = [];
    let historyCalls = 0;
    let failFromCall = Number.POSITIVE_INFINITY;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: {
        async resolveCompactHistory() {
          historyCalls += 1;
          if (historyCalls >= failFromCall) throw new Error("simulated disk error");
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: { ref: "ref-1", text: "old", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      onAgentEvent: (event) => pushedEvents.push(event),
      stores: harness,
      turnCompletions: new TurnCompletionRegistry({ timeoutMs: 0 }),
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.listForHerdrSession("default")[0];
    if (!agent) throw new Error("expected agent");

    // A plan already degraded by earlier retry rounds (last_error = "degraded").
    const plan = harness.statusEventPlans.insertPending({ agent, from: "working", to: "done" });
    for (let i = 0; i < STATUS_PLAN_MAX_ATTEMPTS - 1; i += 1) {
      const retried = harness.statusEventPlans.markRetry(plan.id, new Error("degraded"));
      expect(retried?.status).toBe("pending");
    }
    expect(harness.statusEventPlans.get(plan.id).lastError).toBe("degraded");

    // Only the second refresh of this drain round fails: #drainPlanRow's own
    // pre-refresh succeeds, then #runPlanRow's retry pre-refresh throws.
    failFromCall = historyCalls + 2;
    await index.drainPendingPlans();

    // Exhaustion must surface as failed (wakeable fallback), never as a silent discard.
    const row = harness.statusEventPlans.get(plan.id);
    expect(row.status).toBe("failed");
    expect(row.lastError).toBe("degraded");
    expect(pushedEvents.filter((e) => e.type === "agent.discarded")).toHaveLength(0);
    const failedEvent = pushedEvents.find((e) => e.type === "agent.failed");
    expect(failedEvent).toBeDefined();
    expect(failedEvent?.status).toBe("pending");
    expect(failedEvent?.deliverable).toBe(1);
    const payload = failedEvent?.payload as Record<string, unknown> | undefined;
    expect(payload?.fallbackOutcome).toBe(true);

    harness.sqlite.close();
  }, 20_000);
});
