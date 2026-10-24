import { chmodSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AgentHistoryService } from "@/agent-history/service.js";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { AgentEventStore, isDeliverableAgentEvent } from "@/db/agent-events.js";
import { STATUS_PLAN_MAX_ATTEMPTS } from "@/db/status-event-plans.js";
import {
  AgentIndexService,
  STALE_DUPLICATE_GUARD_SCAN_LIMIT,
  type StatusEventPlan,
  WORKING_PLAN_MAX_STALL_ATTEMPTS,
  WORKING_PLAN_MAX_WAIT_MS,
} from "@/observability/agent-index-service.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import {
  type AgentEventRecord,
  type AgentStatus,
  type CompactAgentHistory,
  PLAN_STALLED,
  PLAN_WAITING_HISTORY,
} from "@/observability/contracts.js";
import { TurnCompletionRegistry } from "@/observability/turn-completion.js";
import type { AgentEventWireRecord } from "../../packages/herdsman-pi/src/daemon-client.js";
import { projectAgentOutcomes } from "../../packages/herdsman-pi/src/wake.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

const toWireRecord = (e: AgentEventRecord): AgentEventWireRecord => ({
  agentId: e.agentId,
  compactHistory: e.compactHistory,
  id: e.id,
  paneGeneration: e.paneGeneration ?? null,
  paneId: e.paneId,
  payload: e.payload,
  terminalId: e.terminalId,
  type: e.type,
});

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

  test("falls back to the local index when the official agent.list snapshot fails", async () => {
    const harness = openObservabilityDbHarness();
    let fail = false;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          if (fail) throw new Error("Herdr session.snapshot request timed out after 2000ms");
          return oneAgent("working", 10, "codex", "reviewer");
        },
      }),
      history: history(() => undefined),
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const indexed = harness.agents.list({ herdrSessionName: "default" });
    expect(indexed).toHaveLength(1);

    fail = true;
    await expect(index.refreshHerdrSession(sessionInput())).rejects.toThrow(
      "Herdr session.snapshot request timed out after 2000ms",
    );
    // The failed official call must not empty the base list: the last indexed
    // snapshot keeps serving `agent.list` until Herdr answers again.
    expect(harness.agents.list({ herdrSessionName: "default" })).toEqual(indexed);
    expect(harness.herdrSessions.list().filter((session) => session.running)).toHaveLength(1);
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
        source: "pi-jsonl" as const,
        value: path,
      };
      return {
        compactHistory: {
          ...emptyCompactHistory("pi-jsonl"),
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
  test("agentSession 由空变为存在时 identityChanged 为真并重新解析（不强制 discovery）", async () => {
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
    // The pane is looked up again (the fake was called), because the new official
    // value is a new lookup key. It is not a *forced* round: an unresolved pane
    // must let the failure memory in `discovery.ts` age out normally.
    expect(calls).toEqual([{ forceDiscovery: false }]);
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
      lastError: PLAN_WAITING_HISTORY,
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
      { attempts: 1, lastError: PLAN_WAITING_HISTORY, status: "pending" },
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

  test("C5: fake timers 8x4s + 255s exponential backoff = 287s exhausts non-agy retry budget to discarded with attempts=8, zero agent.failed events, no drainPendingPlans called", async () => {
    vi.useFakeTimers();
    try {
      const harness = openObservabilityDbHarness();
      const drainSpy = vi.spyOn(AgentIndexService.prototype, "drainPendingPlans");

      let currentAssistantMsg: { ref: string; text: string; timestamp: string | null } | null =
        null;
      let paneStatus = "working";
      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return oneAgent(paneStatus, 10, "agy");
          },
        }),
        history: {
          async resolveCompactHistory() {
            return {
              compactHistory: {
                ...emptyCompactHistory("antigravity-sqlite"),
                lastAssistantMessage: currentAssistantMsg,
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        statusPlansConfig: {
          initial_backoff_ms: 5_000,
          max_backoff_ms: 60_000,
          backoff_factor: 2,
          total_window_ms: 250_000,
        },
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
        lastError: PLAN_WAITING_HISTORY,
        status: "pending",
      });

      // Advance remaining 7 retry intervals with exponential backoff (5s, 10s, 20s, 40s, 60s, 60s, 60s + 4s wait each)
      const backoffDelays = [5_000, 10_000, 20_000, 40_000, 60_000, 60_000, 60_000];
      for (const delay of backoffDelays) {
        await vi.advanceTimersByTimeAsync(delay);
        await vi.advanceTimersByTimeAsync(4_000);
      }

      const finalPlan = harness.statusEventPlans.get(planRecord.id);
      expect(finalPlan.attempts).toBe(8);
      expect(finalPlan.status).toBe("discarded");
      expect(finalPlan.lastError).toBe(PLAN_WAITING_HISTORY);

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
        fallbackOutcome: true,
        from: "working",
        paneId: "wJ:p2",
        reason: PLAN_WAITING_HISTORY,
        to: "idle",
      });

      // M2: agent.discarded is inserted with status pending and deliverable 1, and can be reserved
      const firstDiscardedInC5 = discardedEvents[0];
      expect(firstDiscardedInC5).toBeDefined();
      if (!firstDiscardedInC5) {
        throw new Error("expected discardedEvents[0] in C5 to be defined");
      }
      expect(firstDiscardedInC5.status).toBe("pending");
      expect(firstDiscardedInC5.deliverable).toBe(1);
      const reserved = harness.agentEvents.reservePending("term_orchestrator");
      expect(reserved.map((e) => e.id)).toContain(firstDiscardedInC5.id);

      // 新需求 1: stalled 之后迟到回复的补发（走正常 completed 语义，按 ref 去重只发一次）
      // 此时 pane 已结算回 idle（M1：只有 idle/done 才允许补发）。
      paneStatus = "idle";
      currentAssistantMsg = { ref: "late-ref-1", text: "Finished late work", timestamp: null };
      const lateRefreshResult = await index.refreshHerdrSession(sessionInput());
      const lateEvents = lateRefreshResult.events.filter(
        (e) => e.type === "agent.idle" || e.type === "agent.done",
      );
      expect(lateEvents).toHaveLength(1);
      const lateEvent = lateEvents[0];
      expect(lateEvent).toBeDefined();
      if (!lateEvent) {
        throw new Error("expected lateEvents[0] to be defined");
      }
      expect(lateEvent.type).toBe("agent.idle");
      expect(lateEvent.payload).toMatchObject({
        from: "working",
        lateCompletion: true,
        to: "idle",
      });
      expect(lateEvent.compactHistory?.lastAssistantMessage?.ref).toBe("late-ref-1");

      // 幂等：同一 ref 再次刷新历史不会重复产生完成事件
      const lateRefreshResult2 = await index.refreshHerdrSession(sessionInput());
      const secondLateEvents = lateRefreshResult2.events.filter(
        (e) => e.type === "agent.idle" || e.type === "agent.done",
      );
      expect(secondLateEvents).toHaveLength(0);

      // Proves drainPendingPlans was never called during retry loop
      expect(drainSpy).not.toHaveBeenCalled();

      index.stopWaitingHistoryRetries();
      harness.sqlite.close();
    } finally {
      vi.useRealTimers();
    }
  });

  test("M3: suspected false idle stays pending throughout the total window and is discarded only upon window expiration, with working time deducted", async () => {
    vi.useFakeTimers();
    try {
      const harness = openObservabilityDbHarness();
      let liveAgentStatus: AgentStatus = "working";
      let historyReads = 0;

      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return oneAgent(liveAgentStatus, 10, "agy");
          },
        }),
        history: {
          async resolveCompactHistory() {
            // The pane keeps making progress on disk while it works, so the
            // progress fingerprint advances and the stall fallback
            // (`WORKING_PLAN_MAX_STALL_ATTEMPTS`) never fires. What is under test
            // here is the *window* accounting, not the stall accounting.
            historyReads += 1;
            return {
              compactHistory: {
                ...emptyCompactHistory("antigravity-sqlite"),
                lastAssistantMessage: null,
                lastToolResult: {
                  compaction: { mode: "unknown", originalChars: 10, returnedChars: 10 },
                  isError: false,
                  ref: `progress-${historyReads}`,
                  text: `progress step ${historyReads}`,
                  toolName: "bash",
                },
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        statusPlansConfig: {
          backoff_factor: 2,
          initial_backoff_ms: 5_000,
          max_backoff_ms: 60_000,
          total_window_ms: 900_000, // 生产默认 15 分钟窗口
        },
        stores: harness,
      });

      await index.refreshHerdrSession(sessionInput());
      // The waiting plan is the only plan under test; a real `working` phase also
      // inserts an `idle -> working` row through the event path, so plans are
      // selected by transition instead of by position.
      const waitingPlan = () =>
        harness.statusEventPlans
          .listUnfinished()
          .find((row) => row.fromStatus === "working" && row.toStatus === "idle");

      // 1. Herdr 报 agent 变为 idle，但历史上没有新 assistant 消息（疑似假 idle）
      liveAgentStatus = "idle";
      const handlePromise = index.handleHerdrEvent({
        event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInput(),
      });

      // 推进首次 ready wait (8 x 500ms = 4000ms)
      await vi.advanceTimersByTimeAsync(4000);
      await handlePromise;

      const planRecord = waitingPlan();
      if (!planRecord) throw new Error("expected pending plan");

      // 2. 推进前 7 次重试 (5s, 10s, 20s, 40s, 60s, 60s, 60s)
      const backoffDelays = [5_000, 10_000, 20_000, 40_000, 60_000, 60_000, 60_000];
      for (const delay of backoffDelays) {
        await vi.advanceTimersByTimeAsync(delay);
        await vi.advanceTimersByTimeAsync(4000);
      }

      // 此时已经达到了 8 次 attempts，但时间只过了 4s + 255s + 28s = 287s，远小于 900s 窗口！
      // 核心断言：疑似假 idle 在窗口期内严禁被 discard，必须保持 pending！
      const planAtAttempt8 = harness.statusEventPlans.get(planRecord.id);
      expect(planAtAttempt8.attempts).toBe(8);
      expect(planAtAttempt8.status).toBe("pending");

      // 3. 模拟 agent 重新转入 working：走真实事件路径把 working 写进 agents 表，
      //    这样 `#markPlanRetry` 的 `#findLiveAgent` 才能真的看到 live working
      //    （旧版用例只改 sessionSnapshot 返回值，agents 表从未变 working，
      //    `isLiveWorking` 全程为假，working 豁免根本没被行使）。
      liveAgentStatus = "working";
      await index.handleHerdrEventFast({
        event: { agent_status: "working", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInput(),
      });
      const liveWorkingAgent = harness.agents.findByPane({
        herdrSessionName: "default",
        paneId: "wJ:p2",
      });
      expect(liveWorkingAgent?.agentStatus).toBe("working");

      // working 期间推进 5 个 tick（5 x (60s 退避上限 + 4s ready wait) = 320s），
      // 这段时间不应计入窗口：每个 tick 都会把 idle 锚点重置为当前时刻。
      for (let i = 0; i < 5; i += 1) {
        await vi.advanceTimersByTimeAsync(60_000);
        await vi.advanceTimersByTimeAsync(4000);
      }

      // 4. agent 再次回到 idle（同样走真实事件路径）
      liveAgentStatus = "idle";
      await index.handleHerdrEventFast({
        event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInput(),
      });
      expect(
        harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" })?.agentStatus,
      ).toBe("idle");

      // 4a. 先推进 8 个 idle tick（8 x 64s = 512s）。此时总墙钟已达
      //     287 + 320 + 512 = 1119s > 900s，但扣除 working 的 320s 后有效 idle
      //     只有 512s < 900s：计划必须仍然 pending。
      for (let i = 0; i < 8; i += 1) {
        await vi.advanceTimersByTimeAsync(60_000);
        await vi.advanceTimersByTimeAsync(4000);
      }
      const planAfterWorking = harness.statusEventPlans.get(planRecord.id);
      expect(planAfterWorking.status).toBe("pending");
      expect(planAfterWorking.lastError).toBe(PLAN_WAITING_HISTORY);

      // 5. 再推进 8 个 idle tick（512s），有效 idle 达到 1024s > 900s，
      //    plan 真正转为 discarded。
      for (let i = 0; i < 8; i += 1) {
        await vi.advanceTimersByTimeAsync(60_000);
        await vi.advanceTimersByTimeAsync(4000);
      }

      // 此时有效 idle 时间耗尽，plan 真正转为 discarded！
      const expiredPlan = harness.statusEventPlans.get(planRecord.id);
      expect(expiredPlan.status).toBe("discarded");
      expect(expiredPlan.lastError).toBe(PLAN_WAITING_HISTORY);
      expect(expiredPlan.attempts).toBeGreaterThan(8);

      const discardedEvents = harness.agentEvents
        .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
        .filter((e) => e.type === "agent.discarded");
      expect(discardedEvents).toHaveLength(1);
      const firstDiscarded = discardedEvents[0];
      expect(firstDiscarded).toBeDefined();
      if (!firstDiscarded) {
        throw new Error("expected discardedEvents[0] to be defined");
      }
      expect(firstDiscarded.payload).toMatchObject({
        fallbackOutcome: true,
        reason: PLAN_WAITING_HISTORY,
      });

      index.stopWaitingHistoryRetries();
      harness.sqlite.close();
    } finally {
      vi.useRealTimers();
    }
  });

  test("F1: one late completion is published exactly once (returned events carry it, no extra onAgentEvent push)", async () => {
    // F1: `#reconcileLateCompletion` used to push through `#onAgentEvent` itself
    // while all three of its callers also put the event into the `events` array
    // they return, and `HerdrSessionWatchManager#publishResult` ->
    // `publishAgentEvent` pushes that array with no id-level dedupe. The same
    // `agent.event` therefore reached the socket twice.
    vi.useFakeTimers();
    try {
      const harness = openObservabilityDbHarness();
      const pushed: AgentEventRecord[] = [];
      let currentAssistantMsg: { ref: string; text: string; timestamp: string | null } | null =
        null;
      let paneStatus = "working";

      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return oneAgent(paneStatus, 10, "agy");
          },
        }),
        history: {
          async resolveCompactHistory() {
            return {
              compactHistory: {
                ...emptyCompactHistory("antigravity-sqlite"),
                lastAssistantMessage: currentAssistantMsg,
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        onAgentEvent: (event) => {
          pushed.push(event);
        },
        statusPlansConfig: {
          backoff_factor: 2,
          initial_backoff_ms: 5_000,
          max_backoff_ms: 60_000,
          total_window_ms: 60_000,
        },
        stores: harness,
      });

      await index.refreshHerdrSession(sessionInput());
      const handlePromise = index.handleHerdrEvent({
        event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInput(),
      });
      await vi.advanceTimersByTimeAsync(4000);
      await handlePromise;

      // 5s + 10s + 20s + 40s = 75s > 60s 窗口：计划结算为 discarded。
      for (const delay of [5_000, 10_000, 20_000, 40_000]) {
        await vi.advanceTimersByTimeAsync(delay);
        await vi.advanceTimersByTimeAsync(4000);
      }

      const stallDiscard = harness.agentEvents
        .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
        .filter((e) => e.type === "agent.discarded");
      expect(stallDiscard).toHaveLength(1);
      expect(stallDiscard[0]?.payload).toMatchObject({
        fallbackOutcome: true,
        reason: PLAN_WAITING_HISTORY,
      });
      // The stall discard itself is pushed once by the retry ring (its only
      // caller) — the reference behaviour the replay has to match.
      expect(pushed).toHaveLength(1);

      // The orchestrator-side projector turns that discard into the
      // self-readable `stalled` outcome (C4: merged from the former
      // `exercise:` case instead of keeping a near-duplicate scenario).
      expect(
        projectAgentOutcomes([toWireRecord(stallDiscard[0] as AgentEventRecord)]).outcomes,
      ).toEqual([expect.objectContaining({ kind: "stalled", reason: PLAN_WAITING_HISTORY })]);

      // The late answer lands; the next refresh must replay it exactly once. The
      // pane has settled back to `idle` by then — a replay is only allowed for a
      // settled pane (M1), never mid-round.
      await vi.advanceTimersByTimeAsync(100_000);
      paneStatus = "idle";
      currentAssistantMsg = {
        ref: "late-f1-turn",
        text: "late answer for the stalled round",
        timestamp: null,
      };
      const replay = await index.refreshHerdrSession(sessionInput());
      const replayed = replay.events.filter(
        (e) => e.type === "agent.idle" || e.type === "agent.done",
      );
      expect(replayed).toHaveLength(1);
      expect(replayed[0]?.payload).toMatchObject({ lateCompletion: true });
      expect(
        projectAgentOutcomes([toWireRecord(replayed[0] as AgentEventRecord)]).outcomes,
      ).toEqual([
        expect.objectContaining({
          kind: "completed",
          text: "late answer for the stalled round",
        }),
      ]);

      // Model the real publication path: `HerdrSessionWatchManager#publishResult`
      // forwards every event the refresh returned through the same
      // `publishAgentEvent` channel the service pushes on internally. The late
      // completion must appear exactly once across both.
      const published = [...pushed, ...replay.events];
      const latePublications = published.filter(
        (e) => (e.payload as Record<string, unknown>).lateCompletion === true,
      );
      expect(latePublications).toHaveLength(1);
      expect(latePublications[0]?.id).toBe(replayed[0]?.id);
      // …and the service itself must not have pushed it a second time.
      expect(
        pushed.filter((e) => (e.payload as Record<string, unknown>).lateCompletion === true),
      ).toHaveLength(0);
      expect(pushed).toHaveLength(1);

      // 数据库里同样只有一行（idempotencyKey 生效）。
      const dbLateRows = harness.agentEvents
        .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
        .filter((e) => (e.payload as Record<string, unknown>).lateCompletion === true);
      expect(dbLateRows).toHaveLength(1);

      // 第二轮刷新静默，且不产生任何额外推送。
      const second = await index.refreshHerdrSession(sessionInput());
      expect(
        second.events.filter((e) => e.type === "agent.idle" || e.type === "agent.done"),
      ).toHaveLength(0);
      expect(pushed).toHaveLength(1);

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
              ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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
              ...emptyCompactHistory("pi-jsonl"),
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
    harness.statusEventPlans.markRetry(p2Row.id, new Error(PLAN_WAITING_HISTORY));

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
        harness.statusEventPlans.markRetry(planRow.id, new Error(PLAN_WAITING_HISTORY));
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
        reason: PLAN_WAITING_HISTORY,
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
                ...emptyCompactHistory("pi-jsonl"),
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
          ...emptyCompactHistory("pi-jsonl"),
          lastAssistantMessage: { ref: "initial", text: "initial output", timestamp: null },
        },
        from: "working",
        to: "done",
      });
      for (let i = 1; i <= 7; i += 1) {
        harness.statusEventPlans.markRetry(planRow.id, new Error(PLAN_WAITING_HISTORY));
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
          reason: PLAN_WAITING_HISTORY,
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
        reason: PLAN_WAITING_HISTORY,
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
              ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "Claude turn 1", timestamp: null },
      },
      from: "working",
      to: "done",
    });
    harness.statusEventPlans.markRetry(p2Row.id, new Error(PLAN_WAITING_HISTORY));

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
              ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "Codex turn 1", timestamp: null },
      },
      from: "working",
      herdrEventKey: "key-2",
      to: "done",
    });
    harness.statusEventPlans.markRetry(p2Row.id, new Error(PLAN_WAITING_HISTORY));

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
        ...emptyCompactHistory("pi-jsonl"),
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
      harness.statusEventPlans.markRetry(pRow.id, new Error(PLAN_WAITING_HISTORY));
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
    harness.statusEventPlans.markRetry(p2Row.id, new Error(PLAN_WAITING_HISTORY));

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
                ...emptyCompactHistory("pi-jsonl"),
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
          ...emptyCompactHistory("pi-jsonl"),
          lastAssistantMessage: { ref: "initial", text: "initial output", timestamp: null },
        },
        from: "working",
        to: "done",
      });
      harness.statusEventPlans.markRetry(pRow.id, new Error(PLAN_WAITING_HISTORY));

      // Now set failRefresh = true
      failRefresh = true;

      await index.drainPendingPlans();

      const updated = harness.statusEventPlans.get(pRow.id);
      expect(updated.status).toBe("pending");
      expect(updated.attempts).toBe(2);
      expect(updated.lastError).toBe(PLAN_WAITING_HISTORY);

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
                ...emptyCompactHistory("pi-jsonl"),
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
          ...emptyCompactHistory("pi-jsonl"),
          lastAssistantMessage: { ref: "initial", text: "initial output", timestamp: null },
        },
        from: "working",
        to: "done",
      });
      harness.statusEventPlans.markRetry(pRow.id, new Error(PLAN_WAITING_HISTORY));

      // 1. Refresh fails -> lastError must be PLAN_WAITING_HISTORY and timer scheduled
      failRefresh = true;
      await index.drainPendingPlans();

      const afterFail = harness.statusEventPlans.get(pRow.id);
      expect(afterFail.status).toBe("pending");
      expect(afterFail.lastError).toBe(PLAN_WAITING_HISTORY);
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
              ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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

  test("retains and delivers orphan terminal event when pane is closed while plan is pending", async () => {
    const harness = openObservabilityDbHarness();
    const pushed: AgentEventRecord[] = [];
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      onAgentEvent: (event) => {
        pushed.push(event);
      },
      stores: harness,
    });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      await index.refreshHerdrSession(sessionInput());
      const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
      if (!agent) throw new Error("expected indexed agent");

      // The pane's own event history is what keeps an orphan terminal row
      // routable once the `agents` row is gone (pane id, terminal, workspace).
      harness.agentEvents.append({
        agentId: agent.id,
        herdrSessionName: "default",
        paneId: "wJ:p2",
        payload: { agent: "pi", from: "working", to: "done" },
        terminalId: "term_claude",
        type: "agent.status.changed",
        workspaceId: "wJ",
      });

      // A pending plan whose frozen baseline holds this round's final answer.
      const row = harness.statusEventPlans.insertPending({
        agentId: agent.id,
        compactHistory: {
          ...emptyCompactHistory("pi-jsonl"),
          lastAssistantMessage: {
            ref: "m2",
            stopReason: "stop",
            text: "final answer",
            timestamp: null,
          },
        },
        fromStatus: "working",
        herdrSessionName: "default",
        paneId: "wJ:p2",
        toStatus: "done",
      });

      // The orchestrator closes the pane: the `agents` row is physically
      // deleted (ON DELETE SET NULL then clears the event's agent_id too).
      harness.sqlite.prepare("delete from agents where id = ?").run(agent.id);

      await expect(index.drainPendingPlans()).resolves.toBeUndefined();

      // The plan is settled, not cancelled: its content survives as an orphan
      // terminal event with the full body.
      expect(harness.statusEventPlans.get(row.id).status).toBe("completed");
      const events = harness.agentEvents.listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      });
      const orphan = events.find((event) => event.type === "agent.failed");
      if (!orphan) throw new Error("expected the salvaged orphan terminal event");
      expect(orphan.agentId).toBeNull();
      expect(orphan).toMatchObject({ deliverable: 1, status: "pending" });
      expect(orphan.compactHistory?.lastAssistantMessage?.text).toBe("final answer");
      expect(orphan.payload).toMatchObject({
        agentId: agent.id,
        herdrSessionName: "default",
        paneId: "wJ:p2",
        reason: "pane_closed_before_delivery",
      });
      expect(events.filter((event) => event.type === "agent.done")).toHaveLength(0);

      // The salvage is published, not just written: the orchestrator's wake is
      // `agent.event`-push driven, and a pending-only row would sit untouched
      // while the pane is closed and the orchestrator is idle.
      expect(pushed).toHaveLength(1);
      expect(pushed[0]).toMatchObject({
        id: orphan.id,
        payload: expect.objectContaining({ reason: "pane_closed_before_delivery" }),
        type: "agent.failed",
      });

      const warnCalls = warn.mock.calls as unknown as Array<[unknown, Record<string, unknown>]>;
      expect(
        warnCalls.filter(
          (call) => call[0] === "Herdsman cancelling status event plan for missing agent",
        ),
      ).toHaveLength(0);
      expect(
        warnCalls.filter(
          (call) =>
            call[0] ===
              "Herdsman salvaged an orphan terminal event for a plan whose agent row is gone" &&
            call[1]?.planId === row.id,
        ),
      ).toHaveLength(1);
    } finally {
      warn.mockRestore();
    }
    harness.sqlite.close();
  });

  test("salvage window: a saturated scan warns once per pane and throttles repeats", async () => {
    const harness = openObservabilityDbHarness();
    const pushed: AgentEventRecord[] = [];
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      onAgentEvent: (event) => {
        pushed.push(event);
      },
      stores: harness,
    });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      await index.refreshHerdrSession(sessionInput());
      const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
      if (!agent) throw new Error("expected indexed agent");

      // Saturate the salvage window: exactly the scan limit in delivered/acked
      // terminal rows for this (herdr_session_name, pane_id) that predate the
      // plans, so the newest-`limit` window comes back full and any older row
      // would be invisible to the `alreadyDelivered` comparison.
      const seed = harness.sqlite.prepare(
        `insert into agent_events
           (herdr_session_name, agent_id, pane_id, type, payload_json, deliverable, status, delivery_attempts, created_at)
         values ('default', ?, 'wJ:p2', 'agent.done', '{}', 1, 'delivered', 1, ?)`,
      );
      const seededAt = Date.now() - 60_000;
      for (let i = 0; i < STALE_DUPLICATE_GUARD_SCAN_LIMIT; i += 1) {
        seed.run(agent.id, seededAt + i);
      }

      // The pane's own event history is what keeps an orphan terminal row
      // routable once the `agents` row is gone (pane id, terminal, workspace).
      // Appended after the saturated rows so it stays the newest pane event that
      // predates the plans and therefore the one the orphan scope stitches from
      // (the seeded rows carry no terminal id of their own).
      harness.agentEvents.append({
        agentId: agent.id,
        herdrSessionName: "default",
        paneId: "wJ:p2",
        payload: { agent: "pi", from: "working", to: "done" },
        terminalId: "term_claude",
        type: "agent.status.changed",
        workspaceId: "wJ",
      });

      // Two pending plans whose frozen baselines hold bodies the saturated
      // window does not mention, so each one runs the window query and salvages.
      const planIds: number[] = [];
      for (const [i, text] of ["salvage me", "salvage me too"].entries()) {
        planIds.push(
          harness.statusEventPlans.insertPending({
            agentId: agent.id,
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: `m${i + 1}`,
                stopReason: "stop",
                text,
                timestamp: null,
              },
            },
            fromStatus: "working",
            herdrSessionName: "default",
            paneId: "wJ:p2",
            toStatus: "done",
          }).id,
        );
      }

      // The orchestrator closes the pane: the `agents` row is physically deleted.
      harness.sqlite.prepare("delete from agents where id = ?").run(agent.id);

      await expect(index.drainPendingPlans()).resolves.toBeUndefined();

      // Both rounds are salvaged (the arm that runs the window query), yet the
      // saturation is reported exactly once: the in-memory Set throttles per
      // (herdr_session_name, pane_id), so the second round adds no warning.
      expect(planIds.map((id) => harness.statusEventPlans.get(id).status)).toEqual([
        "completed",
        "completed",
      ]);
      expect(pushed.filter((event) => event.type === "agent.failed")).toHaveLength(2);
      const warnCalls = warn.mock.calls as unknown as Array<[unknown, Record<string, unknown>]>;
      const saturated = warnCalls.filter(
        (call) => call[0] === "Herdsman salvage window is saturated",
      );
      expect(saturated).toHaveLength(1);
      expect(saturated[0]?.[1]).toMatchObject({
        limit: STALE_DUPLICATE_GUARD_SCAN_LIMIT,
        paneId: "wJ:p2",
        scanned: STALE_DUPLICATE_GUARD_SCAN_LIMIT,
      });
    } finally {
      warn.mockRestore();
    }
    harness.sqlite.close();
  }, 20_000);

  test("cancels and appends nothing when a pending plan's baseline repeats an already-delivered body", async () => {
    const harness = openObservabilityDbHarness();
    const index = openIndex(harness);
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      await index.refreshHerdrSession(sessionInput());
      const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
      if (!agent) throw new Error("expected indexed agent");

      // The orchestrator already holds this body: a terminal row for the pane
      // that was delivered (and consumed). It also doubles as the stitch source
      // for the pane's terminal/workspace, so the only thing that can stop a
      // salvage below is the already-delivered comparison.
      const deliveredBody = {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: {
          ref: "m1",
          stopReason: "stop",
          text: "already delivered answer",
          timestamp: null,
        },
      };
      const delivered = harness.agentEvents.append({
        agentId: agent.id,
        compactHistory: deliveredBody,
        herdrSessionName: "default",
        paneId: "wJ:p2",
        payload: { agent: "pi", from: "working", to: "done" },
        terminalId: "term_claude",
        type: "agent.done",
        workspaceId: "wJ",
      });
      harness.agentEvents.reservePending("term_owner", 100, [delivered.id]);

      // The plan's frozen baseline is exactly that delivered body — the pane
      // closes right after a finished round, so the baseline frequently *is*
      // the previous round's answer. Sending it again would be a false failure
      // with a stale body; no same-pane completed row can suppress it.
      const row = harness.statusEventPlans.insertPending({
        agentId: agent.id,
        compactHistory: deliveredBody,
        fromStatus: "working",
        herdrSessionName: "default",
        paneId: "wJ:p2",
        toStatus: "done",
      });

      harness.sqlite.prepare("delete from agents where id = ?").run(agent.id);
      await expect(index.drainPendingPlans()).resolves.toBeUndefined();

      expect(harness.statusEventPlans.get(row.id).status).toBe("cancelled");
      const events = harness.agentEvents.listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      });
      expect(events.filter((event) => event.type === "agent.failed")).toHaveLength(0);
      const warnCalls = warn.mock.calls as unknown as Array<[unknown, Record<string, unknown>]>;
      expect(
        warnCalls.filter(
          (call) => call[0] === "Herdsman cancelling status event plan for missing agent",
        ),
      ).toHaveLength(1);
      expect(
        warnCalls.filter(
          (call) =>
            call[0] ===
            "Herdsman salvaged an orphan terminal event for a plan whose agent row is gone",
        ),
      ).toHaveLength(0);
    } finally {
      warn.mockRestore();
    }
    harness.sqlite.close();
  });

  test("cancels and appends nothing when the pane scope cannot be stitched for an orphan salvage", async () => {
    const harness = openObservabilityDbHarness();
    const index = openIndex(harness);
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      await index.refreshHerdrSession(sessionInput());
      const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
      if (!agent) throw new Error("expected indexed agent");

      // A pending plan whose baseline holds this round's answer, but whose pane
      // has NO event history at all: nothing on the pane predates the plan, so
      // the orphan row's scope cannot be stitched and writing it would dead-
      // letter (`terminal_id = null`, `workspace_id = ""`) and poison the
      // plan's idempotency key. The plan must stay cancelled instead.
      const row = harness.statusEventPlans.insertPending({
        agentId: agent.id,
        compactHistory: {
          ...emptyCompactHistory("pi-jsonl"),
          lastAssistantMessage: {
            ref: "m2",
            stopReason: "stop",
            text: "final answer",
            timestamp: null,
          },
        },
        fromStatus: "working",
        herdrSessionName: "default",
        paneId: "wJ:p2",
        toStatus: "done",
      });

      harness.sqlite.prepare("delete from agents where id = ?").run(agent.id);
      await expect(index.drainPendingPlans()).resolves.toBeUndefined();

      // Nothing appended (no dead row, no idempotency key written), plan
      // cancelled, and the reason is a warning rather than a silent cancel.
      expect(harness.statusEventPlans.get(row.id).status).toBe("cancelled");
      const events = harness.agentEvents.listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      });
      expect(events.filter((event) => event.type === "agent.failed")).toHaveLength(0);
      const warnCalls = warn.mock.calls as unknown as Array<[unknown, Record<string, unknown>]>;
      expect(
        warnCalls.filter(
          (call) =>
            call[0] ===
            "Herdsman left a status event plan cancellable because the pane scope could not be stitched",
        ),
      ).toHaveLength(1);
      expect(
        warnCalls.filter(
          (call) => call[0] === "Herdsman cancelling status event plan for missing agent",
        ),
      ).toHaveLength(1);
    } finally {
      warn.mockRestore();
    }
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
        ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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

  test("a terminal plan whose agent row is gone mid-execution still cancels when nothing is salvageable", async () => {
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

    // The agent row disappears before the plan executes (the orchestrator's
    // "finish and close" lands inside the execution window). The fast-path plan
    // carries no terminal body and the pane recorded none, so the mid-execution
    // loss — which now settles through the same salvage path as a drain-time
    // missing agent — still cancels: nothing to salvage means a content-free
    // round stays cancelled, never an appended dangling event and never a
    // completed plan.
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
    expect(events.filter((e) => e.type === "agent.done" || e.type === "agent.failed")).toHaveLength(
      0,
    );
    harness.sqlite.close();
  });

  test("salvages deliverable terminal content when the agent row is gone mid-execution", async () => {
    const harness = openObservabilityDbHarness();
    const pushed: AgentEventRecord[] = [];
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10);
        },
      }),
      history: history(() => undefined),
      onAgentEvent: (event) => {
        pushed.push(event);
      },
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected indexed agent");

    // The pane's own event history keeps the orphan row routable (terminal +
    // workspace) once the `agents` row is gone.
    harness.agentEvents.append({
      agentId: agent.id,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      payload: { agent: "pi", from: "working", to: "done" },
      terminalId: "term_claude",
      type: "agent.status.changed",
      workspaceId: "wJ",
    });

    // The orchestrator closes the pane while the plan is pending: the row is
    // physically deleted and only then the plan reaches its append.
    harness.sqlite.prepare("delete from agents where id = ?").run(agent.id);

    await expect(
      index.executeStatusEventPlan({
        agent,
        compactHistory: {
          ...emptyCompactHistory("pi-jsonl"),
          lastAssistantMessage: {
            ref: "m2",
            stopReason: "stop",
            text: "final answer",
            timestamp: null,
          },
        },
        from: "working",
        herdrEventKey: "evt-exec-1",
        to: "done",
      }),
    ).resolves.toBeUndefined();

    // The mid-execution loss settles like the drain-time one: the plan holds
    // this round's answer, so it is salvaged as an orphan terminal event and
    // published — not cancelled, not left pending for a tick that may never
    // see the pane again.
    const rows = harness.sqlite
      .prepare("select id, status from status_event_plans order by id")
      .all() as Array<{ id: number; status: string }>;
    expect(rows).toHaveLength(1);
    expect(rows[0]?.status).toBe("completed");
    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const orphan = events.find((event) => event.type === "agent.failed");
    if (!orphan) throw new Error("expected the salvaged orphan terminal event");
    expect(orphan).toMatchObject({ agentId: null, deliverable: 1, status: "pending" });
    expect(orphan.compactHistory?.lastAssistantMessage?.text).toBe("final answer");
    expect(orphan.payload).toMatchObject({
      agentId: agent.id,
      paneId: "wJ:p2",
      reason: "pane_closed_before_delivery",
    });
    expect(events.filter((event) => event.type === "agent.done")).toHaveLength(0);
    expect(pushed).toHaveLength(1);
    expect(pushed[0]?.id).toBe(orphan.id);
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
        ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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
      .prepare("update status_event_plans set status = 'failed', last_error = ? where id = ?")
      .run(PLAN_WAITING_HISTORY, legacyRow.id);

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
      harness.statusEventPlans.markRetry(row.id, new Error(PLAN_WAITING_HISTORY));
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
        reason: PLAN_WAITING_HISTORY,
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
      harness.statusEventPlans.markRetry(row.id, new Error(PLAN_WAITING_HISTORY));
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
        reason: PLAN_WAITING_HISTORY,
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
    // The baseline carries a body on purpose: since Phase 2 item 1 no longer
    // records `agent.failed` for a degraded round with no assistant content
    // (.agents/notes/20260930-phase1-delivery-latency.md:193,204-205), this case
    // keeps pinning the orphan write it exists for.
    const retainedBaseline: CompactAgentHistory = {
      ...emptyCompactHistory("pi-jsonl"),
      lastAssistantMessage: { ref: "ref-1", text: "old", timestamp: null },
    };
    const row = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: retainedBaseline,
      from: "working",
      to: "done",
    });
    for (let i = 0; i < STATUS_PLAN_MAX_ATTEMPTS - 1; i += 1) {
      expect(harness.statusEventPlans.markRetry(row.id, new Error("degraded"))?.status).toBe(
        "pending",
      );
    }
    harness.sqlite.prepare("delete from agents where id = ?").run(agent.id);
    failRefresh = true;

    const failed = await index.executeStatusEventPlan({
      agent,
      compactHistory: retainedBaseline,
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
              ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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
              ...emptyCompactHistory("pi-jsonl"),
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
          ...emptyCompactHistory("pi-jsonl"),
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
          harness.statusEventPlans.markRetry(row.id, new Error(PLAN_WAITING_HISTORY));
        }
        // Force status to failed for testing terminal failed row
        harness.sqlite
          .prepare("update status_event_plans set status = 'failed', last_error = ? where id = ?")
          .run(PLAN_WAITING_HISTORY, row.id);
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
              ...emptyCompactHistory("pi-jsonl"),
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
      harness.statusEventPlans.markRetry(discardedRow.id, new Error(PLAN_WAITING_HISTORY));
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
      reason: PLAN_WAITING_HISTORY,
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
        ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: { ref: "ref-1", text: "initial", timestamp: null },
      },
      from: "working",
      to: "done",
    });
    // Set attempts to 7 so the next retry exhausts attempts to 8
    for (let i = 0; i < 7; i += 1) {
      harness.statusEventPlans.markRetry(row.id, new Error(PLAN_WAITING_HISTORY));
    }
    expect(harness.statusEventPlans.get(row.id).attempts).toBe(7);

    // Mock agentEvents.append so the first append inside #appendStatusEvents throws Error("PLAN_WAITING_HISTORY")
    let appendCallCount = 0;
    const originalAppend = harness.agentEvents.append.bind(harness.agentEvents);
    harness.agentEvents.append = (input: Parameters<typeof originalAppend>[0]) => {
      appendCallCount += 1;
      if (appendCallCount === 1) {
        throw new Error(PLAN_WAITING_HISTORY);
      }
      return originalAppend(input);
    };

    // executeStatusEventPlan with planId will invoke #runPlanRow on the existing row
    const result = await index.executeStatusEventPlan({
      agent,
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
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
      reason: PLAN_WAITING_HISTORY,
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
        ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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
        ...emptyCompactHistory("pi-jsonl"),
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
      lastError: PLAN_WAITING_HISTORY,
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
    harness.statusEventPlans.markRetry(retryRow.id, new Error(PLAN_WAITING_HISTORY));
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
      lastError: PLAN_WAITING_HISTORY,
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

  test("S5: waitForHistoryAdvance spins at a fixed 200ms and respects the 1500ms budget", async () => {
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
    // Verify the fixed interval: every step is 200ms and nothing doubles.
    expect(delays).toHaveLength(8);
    expect(new Set(delays.slice(0, -1))).toEqual(new Set([200]));
    // The last step is clamped to the remaining budget, so the whole wait stays
    // inside maxTotalMs (1500ms) instead of overshooting by one step.
    expect(delays.at(-1)).toBeLessThanOrEqual(200);
    expect(delays.reduce((sum, d) => sum + d, 0)).toBeLessThanOrEqual(1500);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("S9: a synthetic degraded retry reason never re-arms the waiting-history ring", async () => {
    const harness = openObservabilityDbHarness();
    const scheduledDelays: number[] = [];
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
      scheduleRetry: (_callback, delayMs) => {
        scheduledDelays.push(delayMs);
        return undefined;
      },
      sleep: async () => {},
      stores: harness,
      // No turn signal: the round takes the degraded path (no history advance).
      turnCompletions: new TurnCompletionRegistry({ sleep: async () => {}, timeoutMs: 0 }),
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const baseline = {
      ...emptyCompactHistory("pi-jsonl"),
      lastAssistantMessage: { ref: "r1", text: "old", timestamp: null, stopReason: "stop" },
    };
    const degradedPlan = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: baseline,
      from: "working",
      to: "done",
    });

    await index.executeStatusEventPlan({
      agent,
      compactHistory: baseline,
      from: "working",
      to: "done",
      planId: degradedPlan.id,
    });

    // The degraded round is retried (pending + lastError=degraded) but the 10s
    // waiting-history ring is not armed by that synthetic reason.
    expect(harness.statusEventPlans.get(degradedPlan.id)).toMatchObject({
      lastError: "degraded",
      status: "pending",
    });
    expect(scheduledDelays).toEqual([]);

    // Control: a genuine PlanWaitingHistoryError still re-arms the ring.
    const waitingPlan = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: baseline,
      from: "working",
      to: "done",
    });
    harness.statusEventPlans.markRetry(waitingPlan.id, new Error(PLAN_WAITING_HISTORY));
    await index.drainPendingPlans();
    expect(scheduledDelays).toContain(10_000);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("S10: a degraded row cannot re-arm the ring through the PlanWaitingHistoryError catch", async () => {
    const harness = openObservabilityDbHarness();
    const scheduledDelays: number[] = [];
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
      scheduleRetry: (_callback, delayMs) => {
        scheduledDelays.push(delayMs);
        return undefined;
      },
      sleep: async () => {},
      stores: harness,
      turnCompletions: new TurnCompletionRegistry({ sleep: async () => {}, timeoutMs: 0 }),
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");

    const baseline = {
      ...emptyCompactHistory("pi-jsonl"),
      lastAssistantMessage: { ref: "r1", text: "old", timestamp: null, stopReason: "stop" },
    };
    const degradedPlan = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: baseline,
      from: "working",
      to: "done",
    });

    // The row already carries the synthetic `degraded` reason (the assistant
    // content was missing, not the history). The retry drain then hits a genuine
    // PlanWaitingHistoryError; before this fix that alone re-armed the 10s ring,
    // which is the last way a degraded round could stay in it.
    harness.statusEventPlans.markRetry(degradedPlan.id, new Error("degraded"));
    await index.drainPendingPlans();
    expect(harness.statusEventPlans.get(degradedPlan.id)).toMatchObject({
      lastError: "degraded",
      status: "pending",
    });
    expect(scheduledDelays).toEqual([]);

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
    harness.statusEventPlans.markRetry(plan.id, new Error(PLAN_WAITING_HISTORY));

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

  test("S7: an unconfirmed expectedText mismatch with a terminal body on disk is delivered", async () => {
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
    // Pre-record signal so drain picks it up synchronously. Unconfirmed: only an
    // unconfirmed turn keeps the degraded / invalidation retry path.
    registry.record({
      confirmed: false,
      expectedText: "final answer",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });
    const pending = index.handleHerdrEvent(doneEvent);

    const _result = await pending;
    // M2: the disk tail the wait settled on ("intermediate text") is a non-empty
    // terminal assistant message and the turn signal arrived, so the disk body is
    // authoritative and delivered as-is. `confirmed: false` no longer blanks it.
    const dones = _result.events.filter((e) => e.type === "agent.done");
    expect(dones).toHaveLength(1);
    expect(dones[0]?.compactHistory?.lastAssistantMessage?.text).toBe("intermediate text");
    expect(dones[0]?.payload).not.toEqual(expect.objectContaining({ degraded: true }));
    const invalidatedRows = harness.sqlite
      .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
      .all();
    expect(invalidatedRows).toHaveLength(0);
    harness.sqlite.close();
  }, 10_000);

  test("S8: a degraded event retries until a terminal body arrives then completes", async () => {
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
          // call 1/2 = the baseline reads; call 3 = the first drain's reread, still
          // a non-terminal (tool-use) turn so there is no deliverable body (M2 keeps
          // degrading it); call 4+ = the terminal answer that matches expectedText.
          if (callCount <= 2) {
            return {
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
          }
          if (callCount === 3) {
            return {
              compactHistory: {
                ...emptyCompactHistory("pi-jsonl"),
                lastAssistantMessage: {
                  ref: "mismatch-ref",
                  text: "still not final",
                  timestamp: null,
                  stopReason: null,
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
      confirmed: false,
      expectedText: "final answer",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    await index.drainPendingPlans();
    // First drain emitted a degraded event and called markRetry, which invalidated
    // the row in DB and set the plan to pending. The
    // `expected_text_mismatch` label is an arm-order artefact — the mismatch arm is
    // checked before the non-terminal one — NOT a consequence of the tail being
    // non-terminal. The round is degraded at all because M2 finds no deliverable
    // terminal body on disk: an unconfirmed turn is blanked only then.
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
      confirmed: false,
      expectedText: "final answer",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    // Second drain: reset running→pending and drain again; the terminal body has
    // landed and matches expectedText, so the complete event is emitted and the
    // plan completes.
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

      // Step 3: simulate a pre-0010 database: drop the 0010 and later migration records
      // and revert the schema change they carry (0011 creates the guard index), so the
      // runner sees a true 0009-shape database with the legacy rows still present.
      sqlite.exec("drop index if exists agent_events_guard_scan_idx");
      sqlite.prepare("delete from __drizzle_migrations where created_at >= ?").run(1790610020312);

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

  test("degraded done exhausting retries records no agent.failed for an empty round", async () => {
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
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const result = await index.executeStatusEventPlan({
      agent,
      compactHistory: undefined,
      from: "working",
      planId: plan.id,
      to: "done",
    });

    // Phase 2 item 1: this round carries no assistant content at all (the plan
    // exhausted on the synthetic `degraded` marker), so the daemon records no
    // `agent.failed` row and pushes nothing: "the agent failed" is not a verdict
    // this evidence supports, and the row would reach the orchestrator as a bare
    // fail reason with an empty body. The empty round is observable through the
    // once-per-pane `upstream_empty_turn` alert instead.
    expect(result).toBeUndefined();
    const failedRows = harness.sqlite
      .prepare("select * from agent_events where type = 'agent.failed'")
      .all();
    expect(failedRows).toEqual([]);
    // 发布路径唯一：`executeStatusEventPlan` 只返回事件，不再自行推流；推流由调用方
    // 转发该返回值完成（herdr-session-watch-manager.ts:360 #submitPlan -> service.ts:206
    // -> observability-server.ts:244 publishAgentEvent）。本用例中计划落到 `failed`
    // 但无内容可报，因此返回 undefined、内部也不推任何事件（否则 socket 上会出现重复
    // 的 `agent.event`，破坏合同 §4 的「恰好一次」）。
    expect(pushedEvents).toEqual([]);
    expect(
      (warn.mock.calls as unknown as Array<[unknown, Record<string, unknown>]>).filter(
        (call) => call[0] === "Herdsman observed an empty upstream turn",
      ),
    ).toEqual([
      [
        "Herdsman observed an empty upstream turn",
        expect.objectContaining({ paneId: "wJ:p2", reason: "upstream_empty_turn" }),
      ],
    ]);
    warn.mockRestore();

    harness.sqlite.close();
    // ~80% of the default 5s budget even in an isolated single run (4014ms
    // measured), i.e. above the >50% bar for raising only this case's budget:
    // D16 in .agents/notes/20260930-terminal-event-delivery-open-items.md.
    // ~4s is intrinsic: retries at src/observability/agent-index-service.ts:2123-2124
    // sleep 500ms x8, and this case's history has no assistant message, so all 8 burn.
  }, 30_000);

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

  test("plan waiting history stays pending and never transitions to discarded or failed while agy agent remains working, settling only after leaving working or on real stall", async () => {
    const harness = openObservabilityDbHarness();
    const scheduledCallbacks = new Map<unknown, () => Promise<void> | void>();
    let nextTimerId = 1;
    const scheduleRetryMock = vi.fn((callback: () => void, _delayMs: number) => {
      const id = nextTimerId++;
      scheduledCallbacks.set(id, callback);
      return id;
    });
    const clearRetryMock = vi.fn((id: unknown) => {
      scheduledCallbacks.delete(id);
    });

    let currentToolCount = 0;
    let nowMs = Date.now();
    const index = new AgentIndexService({
      clearRetry: clearRetryMock,
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
              messageCount: currentToolCount,
              lastToolResult:
                currentToolCount > 0
                  ? {
                      compaction: {
                        mode: "truncated_passthrough",
                        originalChars: 10,
                        returnedChars: 10,
                      },
                      isError: false,
                      ref: `tool_${currentToolCount}`,
                      text: `result_${currentToolCount}`,
                      toolName: "ctxmode",
                    }
                  : null,
              updatedAt: new Date(1700000000000 + currentToolCount * 1000).toISOString(),
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      now: () => nowMs,
      scheduleRetry: scheduleRetryMock,
      sleep: async () => {},
      statusPlansConfig: {
        total_window_ms: 100_000,
      },
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");
    expect(agent.agentStatus).toBe("working");

    const planRow = harness.statusEventPlans.insertPending({
      agent,
      from: "working",
      to: "done",
    });

    // Initial plan execution: fails history check and schedules waiting retry
    await index.executeStatusEventPlan({
      agent,
      compactHistory: undefined,
      from: "working",
      planId: planRow.id,
      to: "done",
    });

    // Verify initial retry scheduled
    expect(harness.statusEventPlans.get(planRow.id).status).toBe("pending");
    expect(scheduleRetryMock).toHaveBeenCalledTimes(1);

    // Simulate 12 retries (exceeding STATUS_PLAN_MAX_ATTEMPTS = 8) while agent is actively working with tools
    for (let attempt = 1; attempt <= 12; attempt++) {
      currentToolCount = attempt; // Activity: history advances with each MCP tool call
      const latestTimerId =
        scheduleRetryMock.mock.results[scheduleRetryMock.mock.results.length - 1]?.value;
      const callback = scheduledCallbacks.get(latestTimerId);
      if (!callback) throw new Error(`expected callback for timer ${latestTimerId}`);
      await callback();

      const plan = harness.statusEventPlans.get(planRow.id);
      // Invariant: plan must NEVER drop to discarded or failed while agent is working
      expect(plan.status).toBe("pending");
      expect(plan.lastError).toBe(PLAN_WAITING_HISTORY);
    }

    // Zero agent.discarded and zero agent.failed events must have been appended for this plan
    const allEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(allEvents.filter((e) => e.type === "agent.discarded")).toHaveLength(0);
    expect(allEvents.filter((e) => e.type === "agent.failed")).toHaveLength(0);

    // Now simulate agent leaving working state (becomes idle)
    harness.agents.updateStatus({
      agentStatus: "idle",
      herdrSessionName: "default",
      paneId: "wJ:p2",
    });
    expect(
      harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" })?.agentStatus,
    ).toBe("idle");

    // Next retry after leaving working and window expiring should now settle to discarded
    nowMs += 500_000;
    const latestTimerId =
      scheduleRetryMock.mock.results[scheduleRetryMock.mock.results.length - 1]?.value;
    const finalCallback = scheduledCallbacks.get(latestTimerId);
    if (!finalCallback) throw new Error("expected callback after leaving working");
    await finalCallback();

    const settledPlan = harness.statusEventPlans.get(planRow.id);
    expect(settledPlan.status).toBe("discarded");
    expect(settledPlan.lastError).toBe(PLAN_WAITING_HISTORY);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("stall branch: plan keeps pending while working until stallAttempts reaches WORKING_PLAN_MAX_STALL_ATTEMPTS, then settles to discarded", async () => {
    const harness = openObservabilityDbHarness();
    let timerSequence = 0;
    const scheduledCallbacks = new Map<unknown, () => Promise<void> | void>();
    const scheduleRetryMock = vi.fn((callback: () => void, _delayMs: number) => {
      timerSequence += 1;
      scheduledCallbacks.set(timerSequence, callback);
      return timerSequence;
    });

    const frozenHistory: CompactAgentHistory = {
      ...emptyCompactHistory("antigravity-sqlite"),
      lastAssistantMessage: null,
      messageCount: 1,
      lastToolResult: null,
      source: "antigravity-sqlite",
      updatedAt: new Date(1700000000000).toISOString(),
    };

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
            compactHistory: frozenHistory,
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
    expect(agent.agentStatus).toBe("working");

    const planRow = harness.statusEventPlans.insertPending({
      agent,
      from: "working",
      to: "done",
    });

    await index.executeStatusEventPlan({
      agent,
      compactHistory: frozenHistory,
      from: "working",
      planId: planRow.id,
      to: "done",
    });

    expect(harness.statusEventPlans.get(planRow.id).status).toBe("pending");
    expect(scheduleRetryMock).toHaveBeenCalledTimes(1);

    for (let attempt = 1; attempt < WORKING_PLAN_MAX_STALL_ATTEMPTS; attempt++) {
      const latestTimerId =
        scheduleRetryMock.mock.results[scheduleRetryMock.mock.results.length - 1]?.value;
      const callback = scheduledCallbacks.get(latestTimerId);
      if (!callback) throw new Error(`expected callback for timer ${latestTimerId}`);
      await callback();

      const plan = harness.statusEventPlans.get(planRow.id);
      expect(plan.status).toBe("pending");
      expect(plan.lastError).toBe(PLAN_WAITING_HISTORY);

      const allEvents = harness.agentEvents.listAfter({
        herdrSessionName: "default",
        workspaceId: "wJ",
      });
      expect(allEvents.filter((e) => e.type === "agent.discarded")).toHaveLength(0);
      expect(allEvents.filter((e) => e.type === "agent.failed")).toHaveLength(0);
    }

    const stallTimerId =
      scheduleRetryMock.mock.results[scheduleRetryMock.mock.results.length - 1]?.value;
    const stallCallback = scheduledCallbacks.get(stallTimerId);
    if (!stallCallback) throw new Error("expected stall callback");
    await stallCallback();

    const settledPlan = harness.statusEventPlans.get(planRow.id);
    expect(settledPlan.status).toBe("discarded");
    expect(settledPlan.lastError).toBe(PLAN_STALLED);

    const eventsAfterStall = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const discardedEvents = eventsAfterStall.filter((e) => e.type === "agent.discarded");
    expect(discardedEvents).toHaveLength(1);
    expect(discardedEvents[0]?.payload).toMatchObject({
      reason: PLAN_STALLED,
    });
    expect(eventsAfterStall.filter((e) => e.type === "agent.failed")).toHaveLength(0);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("drain branch: attempts-exhausted plan keeps pending during periodic drain while working, but settles during startup or shutdown drain", async () => {
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
              source: "antigravity-sqlite",
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
    expect(agent.agentStatus).toBe("working");

    const planRow = harness.statusEventPlans.insertPending({
      agent,
      from: "working",
      to: "done",
    });
    harness.sqlite
      .prepare("update status_event_plans set attempts = ?, last_error = ? where id = ?")
      .run(STATUS_PLAN_MAX_ATTEMPTS, PLAN_WAITING_HISTORY, planRow.id);
    expect(harness.statusEventPlans.get(planRow.id).attempts).toBe(STATUS_PLAN_MAX_ATTEMPTS);

    // 1. 周期 drain：运行在正常运行时且 live agent 持续 working，保留 working 豁免，应保持 pending
    await index.drainPendingPlans({ reason: "periodic" });
    const planAfterPeriodic = harness.statusEventPlans.get(planRow.id);
    expect(planAfterPeriodic.status).toBe("pending");
    expect(planAfterPeriodic.lastError).toBe(PLAN_WAITING_HISTORY);

    // 2. 启动/关停 drain：agents 表可能陈旧不可信，不享有 working 豁免，应收口为 discarded
    await index.drainPendingPlans({ reason: "startup" });
    const planAfterStartup = harness.statusEventPlans.get(planRow.id);
    expect(planAfterStartup.status).toBe("discarded");
    expect(planAfterStartup.lastError).toBe(PLAN_WAITING_HISTORY);

    const discardedEvents = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((e) => e.type === "agent.discarded");
    expect(discardedEvents).toHaveLength(1);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("interactive suppression applies to orphan agent.idle events", async () => {
    const harness = openObservabilityDbHarness();

    const interactiveSnapshot = snapshot(
      [
        agent({
          agent: "pi",
          agent_session: {
            agent: "pi",
            kind: "path",
            source: "pi-jsonl",
            value: "/root/.pi/agent/sessions/interactive.jsonl",
          },
          agent_status: "working",
          cwd: "/tmp/project",
          pane_id: "wJ:p2",
          revision: 10,
          terminal_id: "term_2",
          workspace_id: "wJ",
        }),
      ],
      [{ pane_id: "wJ:p2", revision: 10 }],
    );

    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return interactiveSnapshot;
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: emptyCompactHistory("pi-jsonl"),
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      sleep: async () => {},
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const agentRecord = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agentRecord) throw new Error("expected agent");

    harness.agents.updateStatus({
      agentStatus: "idle",
      herdrSessionName: "default",
      paneId: "wJ:p2",
    });

    await index.executeStatusEventPlan({
      agent: agentRecord,
      compactHistory: emptyCompactHistory("pi-jsonl"),
      from: "working",
      to: "idle",
    });

    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const idleEvent = events.find((e) => e.type === "agent.idle");
    expect(idleEvent).toBeDefined();
    expect((idleEvent?.payload as Record<string, unknown>).interactive).toBe(true);

    harness.agents.retirePane({
      herdrSessionName: "default",
      paneId: "wJ:p2",
    });
    expect(
      harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" }),
    ).toBeUndefined();

    const scope = { herdrSessionName: "default", workspaceId: "wJ" };
    if (!idleEvent) throw new Error("expected idleEvent");
    const deliverable = isDeliverableAgentEvent(idleEvent, undefined, scope, "owner_term");
    expect(deliverable).toBe(false);

    const nextDeliverable = harness.agentEvents.nextDeliverableAfter({
      afterEventId: 0,
      herdrSessionName: "default",
      ownerTerminalId: "owner_term",
      workspaceId: "wJ",
    });
    expect(nextDeliverable).toBeUndefined();

    harness.sqlite.close();
  });

  test("Fix C: working agy encountering generic runtime error enjoys working exemption instead of early failure", async () => {
    const harness = openObservabilityDbHarness();
    const readyHistory = {
      ...emptyCompactHistory("antigravity-sqlite"),
      lastAssistantMessage: { ref: "ref-new", text: "completed output", timestamp: null },
      source: "antigravity-sqlite",
    };
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
            compactHistory: readyHistory,
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
    expect(agent.agentStatus).toBe("working");

    const row = harness.statusEventPlans.insertPending({
      agent,
      compactHistory: readyHistory,
      from: "working",
      to: "done",
    });

    // Advance attempts to 7 so next attempt hits STATUS_PLAN_MAX_ATTEMPTS (8)
    for (let i = 0; i < 7; i += 1) {
      harness.statusEventPlans.markRetry(row.id, new Error("generic disk failure"), {
        isTargetWorking: true,
      });
    }
    expect(harness.statusEventPlans.get(row.id).attempts).toBe(7);

    // Mock agentEvents.append to throw a generic Error (not PlanWaitingHistoryError)
    harness.agentEvents.append = () => {
      throw new Error("simulated generic db write error");
    };

    // When executeStatusEventPlan runs #runPlanRow, generic error catch routes to #markPlanRetry
    // Because agy is working and not stalled, attempts reach 8 but row is EXEMPTED (status stays pending)
    // and #runPlanRow rethrows the error preserving throw timing
    await expect(
      index.executeStatusEventPlan({
        agent,
        compactHistory: readyHistory,
        from: "working",
        planId: row.id,
        to: "done",
      }),
    ).rejects.toThrow("simulated generic db write error");

    const planAfter = harness.statusEventPlans.get(row.id);
    expect(planAfter.attempts).toBe(8);
    expect(planAfter.status).toBe("pending"); // Exempted! Not failed!
    expect(planAfter.lastError).toBe("simulated generic db write error");

    harness.sqlite.close();
  });

  test("Fix F: plan waiting history times out and settles to discarded with PLAN_STALLED when total wait exceeds maxWorkingPlanWaitMs even with continuous progress", async () => {
    const harness = openObservabilityDbHarness();
    let nowMs = 1_000_000;
    let step = 0;

    // Use a short maxWorkingPlanWaitMs: 300ms (production default is 30m)
    expect(WORKING_PLAN_MAX_WAIT_MS).toBe(30 * 60 * 1000);
    const shortMaxWaitMs = 300;

    let timerSequence = 0;
    const scheduledCallbacks = new Map<unknown, () => Promise<void> | void>();
    const scheduleRetryMock = vi.fn((callback: () => void, _delayMs: number) => {
      timerSequence += 1;
      scheduledCallbacks.set(timerSequence, callback);
      return timerSequence;
    });

    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10, "agy");
        },
      }),
      history: {
        async resolveCompactHistory() {
          step += 1;
          // Progress fingerprint continuously advances every round (different tool result)
          return {
            compactHistory: {
              ...emptyCompactHistory("antigravity-sqlite"),
              lastAssistantMessage: null,
              lastToolResult: {
                compaction: { mode: "unknown", originalChars: 10, returnedChars: 10 },
                isError: false,
                ref: `tool-${step}`,
                text: `progress step ${step}`,
                toolName: "bash",
              },
              source: "antigravity-sqlite",
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      maxWorkingPlanWaitMs: shortMaxWaitMs,
      now: () => nowMs,
      scheduleRetry: scheduleRetryMock,
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

    // Initial plan run (waiting history, has no assistant message yet)
    await index.executeStatusEventPlan({
      agent,
      compactHistory: {
        ...emptyCompactHistory("antigravity-sqlite"),
        lastAssistantMessage: null,
        lastToolResult: {
          compaction: { mode: "unknown", originalChars: 10, returnedChars: 10 },
          isError: false,
          ref: "tool-0",
          text: "progress step 0",
          toolName: "bash",
        },
        source: "antigravity-sqlite",
      },
      from: "working",
      planId: planRow.id,
      to: "done",
    });

    expect(harness.statusEventPlans.get(planRow.id).status).toBe("pending");

    // Advance time within limit: progress changes, stallAttempts resets, plan stays pending
    nowMs += 100;
    const timer1 = scheduleRetryMock.mock.results[scheduleRetryMock.mock.results.length - 1]?.value;
    const cb1 = scheduledCallbacks.get(timer1);
    if (!cb1) throw new Error("expected callback 1");
    await cb1();
    expect(harness.statusEventPlans.get(planRow.id).status).toBe("pending");

    nowMs += 100; // total 200ms < 300ms
    const timer2 = scheduleRetryMock.mock.results[scheduleRetryMock.mock.results.length - 1]?.value;
    const cb2 = scheduledCallbacks.get(timer2);
    if (!cb2) throw new Error("expected callback 2");
    await cb2();
    expect(harness.statusEventPlans.get(planRow.id).status).toBe("pending");

    // Now advance time past maxWorkingPlanWaitMs: total elapsed = 350ms >= 300ms
    nowMs += 150;
    const timer3 = scheduleRetryMock.mock.results[scheduleRetryMock.mock.results.length - 1]?.value;
    const cb3 = scheduledCallbacks.get(timer3);
    if (!cb3) throw new Error("expected callback 3");
    await cb3();

    const settledPlan = harness.statusEventPlans.get(planRow.id);
    expect(settledPlan.status).toBe("discarded");
    expect(settledPlan.lastError).toBe(PLAN_STALLED);

    const discardedEvents = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((e) => e.type === "agent.discarded");
    expect(discardedEvents).toHaveLength(1);
    expect(discardedEvents[0]?.payload).toMatchObject({
      reason: PLAN_STALLED,
    });

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("Fix E: salvage or cancel missing-agent plan clears #planProgressTrackers", async () => {
    const harness = openObservabilityDbHarness();
    const waitingHistory = {
      ...emptyCompactHistory("antigravity-sqlite"),
      lastAssistantMessage: null,
      source: "antigravity-sqlite",
    };
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
            compactHistory: waitingHistory,
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
      compactHistory: waitingHistory,
      from: "working",
      to: "done",
    });

    // Execute plan: since history has no assistant message, it enters waiting history retry and initializes tracker
    await index.executeStatusEventPlan({
      agent,
      compactHistory: waitingHistory,
      from: "working",
      planId: planRow.id,
      to: "done",
    });

    // Verify tracker exists in index
    expect(index.hasPlanProgressTracker(planRow.id)).toBe(true);

    // Now delete agent row simulating missing agent before next drain
    harness.agents.retirePane({
      herdrSessionName: "default",
      paneId: "wJ:p2",
    });

    // Next drain will invoke #salvageOrCancelMissingAgentPlan
    await index.drainPendingPlans();

    // Verify tracker has been cleaned up
    expect(index.hasPlanProgressTracker(planRow.id)).toBe(false);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("generic catch preserves progress tracker across retries so time-based stall closes plan to failed with PLAN_STALLED", async () => {
    const harness = openObservabilityDbHarness();
    let nowMs = 1_000_000;
    let step = 0;
    const shortMaxWaitMs = 300;

    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return oneAgent("working", 10 + step, "agy");
        },
      }),
      history: {
        async resolveCompactHistory() {
          step += 1;
          return {
            compactHistory: {
              ...emptyCompactHistory("antigravity-sqlite"),
              lastAssistantMessage: {
                ref: `asst-${step}`,
                text: `assistant message step ${step}`,
                timestamp: null,
              },
              lastToolResult: {
                compaction: { mode: "unknown", originalChars: 10, returnedChars: 10 },
                isError: false,
                ref: `tool-${step}`,
                text: `progress step ${step}`,
                toolName: "bash",
              },
              source: "antigravity-sqlite",
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      maxWorkingPlanWaitMs: shortMaxWaitMs,
      now: () => nowMs,
      scheduleRetry: () => 1,
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

    // Mock harness.agentEvents.append to throw a generic error for standard status events,
    // but allow agent.failed (the stall outcome event) to succeed.
    const realAppend = harness.agentEvents.append.bind(harness.agentEvents);
    vi.spyOn(harness.agentEvents, "append").mockImplementation((input) => {
      if (input.type !== "agent.failed") {
        throw new Error("simulated generic db write boom");
      }
      return realAppend(input);
    });

    // Round 1: first drain throws generic error in appendStatusEvents. Plan stays pending.
    await index.drainPendingPlans({ allowWorkingExemption: true });
    expect(harness.statusEventPlans.get(planRow.id).status).toBe("pending");
    expect(harness.statusEventPlans.get(planRow.id).attempts).toBe(1);

    // Round 2: time advances 100ms (< 300ms). Progress changes, plan stays pending.
    nowMs += 100;
    await index.drainPendingPlans({ allowWorkingExemption: true });
    expect(harness.statusEventPlans.get(planRow.id).status).toBe("pending");
    expect(harness.statusEventPlans.get(planRow.id).attempts).toBe(2);

    // Round 3: time advances another 100ms (total 200ms < 300ms). Progress changes, plan stays pending.
    nowMs += 100;
    await index.drainPendingPlans({ allowWorkingExemption: true });
    expect(harness.statusEventPlans.get(planRow.id).status).toBe("pending");
    expect(harness.statusEventPlans.get(planRow.id).attempts).toBe(3);

    // Round 4: time advances 150ms (total 350ms >= 300ms).
    // Because tracker was preserved on the generic catch path, total elapsed exceeds maxWorkingPlanWaitMs,
    // so stallDetected triggers and plan closes as failed with PLAN_STALLED.
    nowMs += 150;
    await index.drainPendingPlans({ allowWorkingExemption: true });

    const settledPlan = harness.statusEventPlans.get(planRow.id);
    expect(settledPlan.status).toBe("failed");
    expect(settledPlan.lastError).toBe(PLAN_STALLED);

    const failedEvents = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((e) => e.type === "agent.failed");
    expect(failedEvents).toHaveLength(1);
    expect(failedEvents[0]?.payload).toMatchObject({
      reason: PLAN_STALLED,
    });

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  test("drain refresh failure pushes discarded events unconditionally: agy PLAN_WAITING_HISTORY is suppressed by projector while pi wakes, and agy PLAN_STALLED wakes as stalled", async () => {
    const harness = openObservabilityDbHarness();
    const pushedEvents: AgentEventRecord[] = [];
    let failRefresh = false;
    let nowMs = 1_000;
    const shortMaxWaitMs = 300;

    const initialSnapshot = snapshot(
      [
        agent({
          agent: "agy",
          agent_status: "working",
          pane_id: "wJ:p2",
          revision: 10,
          terminal_id: "term_agy",
          workspace_id: "wJ",
        }),
        agent({
          agent: "pi",
          agent_status: "working",
          pane_id: "wJ:p3",
          revision: 10,
          terminal_id: "term_pi",
          workspace_id: "wJ",
        }),
      ],
      [
        { pane_id: "wJ:p2", revision: 10 },
        { pane_id: "wJ:p3", revision: 10 },
      ],
    );

    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return initialSnapshot;
        },
      }),
      history: {
        async resolveCompactHistory() {
          if (failRefresh) {
            throw new Error("simulated refresh failure during drain");
          }
          return {
            compactHistory: {
              ...emptyCompactHistory("antigravity-sqlite"),
              source: "antigravity-sqlite",
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      maxWorkingPlanWaitMs: shortMaxWaitMs,
      now: () => nowMs,
      onAgentEvent: (event) => {
        pushedEvents.push(event);
      },
      sleep: async () => {},
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInput());
    const agyAgent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    const piAgent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p3" });
    if (!agyAgent || !piAgent) throw new Error("expected agents");
    expect(agyAgent.agentStatus).toBe("working");
    expect(agyAgent.agent).toBe("agy");
    expect(piAgent.agentStatus).toBe("working");
    expect(piAgent.agent).toBe("pi");

    // Enable simulated refresh failure so #drainPlanRow goes into the catch branch
    failRefresh = true;

    // 1. Positive case: periodic drain with working agy stalling closes as discarded with PLAN_STALLED and pushes
    const planRowAgyStall = harness.statusEventPlans.insertPending({
      agent: agyAgent,
      from: "working",
      to: "done",
    });

    // Round 1: tracker initialized, progress not yet stalled, stays pending
    await index.drainPendingPlans({ reason: "periodic" });
    const plan1 = harness.statusEventPlans.get(planRowAgyStall.id);
    expect(plan1.status).toBe("pending");
    expect(pushedEvents).toHaveLength(0);

    // Advance time beyond shortMaxWaitMs so timeStalled becomes true
    nowMs += 350;
    await index.drainPendingPlans({ reason: "periodic" });

    const settledStallPlan = harness.statusEventPlans.get(planRowAgyStall.id);
    expect(settledStallPlan.status).toBe("discarded");
    expect(settledStallPlan.lastError).toBe(PLAN_STALLED);

    // Assert that the PLAN_STALLED discarded event was written to db
    const discardedInDb = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((e) => e.type === "agent.discarded");
    expect(discardedInDb).toHaveLength(1);
    expect(discardedInDb[0]?.payload).toMatchObject({
      reason: PLAN_STALLED,
    });

    // Positive assertion: verify that the PLAN_STALLED discarded event was indeed pushed to onAgentEvent
    const discardedPushed = pushedEvents.filter((e) => e.type === "agent.discarded");
    expect(discardedPushed).toHaveLength(1);
    expect(discardedPushed[0]?.payload).toMatchObject({
      reason: PLAN_STALLED,
    });
    expect(discardedPushed[0]?.id).toBe(discardedInDb[0]?.id);

    // Assert downstream projector behavior for PLAN_STALLED: agy stall wakes as stalled
    // (F6: the jitter-shaped false idle must stay self-readable, not read as a plain failure)
    const firstDiscardedPushed = discardedPushed[0];
    if (!firstDiscardedPushed) throw new Error("expected pushed stall event");
    // The stall discard carries the fallback marker, which is what arms the replay.
    expect(firstDiscardedPushed.payload).toMatchObject({
      agent: "agy",
      fallbackOutcome: true,
      reason: PLAN_STALLED,
    });
    const stallProjection = projectAgentOutcomes([toWireRecord(firstDiscardedPushed)]);
    expect(stallProjection.outcomes).toEqual([
      expect.objectContaining({
        kind: "stalled",
        reason: PLAN_STALLED,
      }),
    ]);

    // 2. agy conventional PLAN_WAITING_HISTORY discarded on this path is pushed unconditionally,
    // and downstream projector suppresses it (returns undefined / 0 outcomes, does not wake)
    const planRowAgyWaiting = harness.statusEventPlans.insertPending({
      agent: agyAgent,
      from: "working",
      to: "done",
    });
    // Set attempts to STATUS_PLAN_MAX_ATTEMPTS and lastError to PLAN_WAITING_HISTORY
    harness.sqlite
      .prepare("update status_event_plans set attempts = ?, last_error = ? where id = ?")
      .run(STATUS_PLAN_MAX_ATTEMPTS, PLAN_WAITING_HISTORY, planRowAgyWaiting.id);

    const pushedCountBeforeAgyWaiting = pushedEvents.length;
    // Startup drain has allowWorkingExemption = false, so attempts >= MAX closes as discarded with PLAN_WAITING_HISTORY
    await index.drainPendingPlans({ reason: "startup" });

    const planAgyWaitingSettled = harness.statusEventPlans.get(planRowAgyWaiting.id);
    expect(planAgyWaitingSettled.status).toBe("discarded");
    expect(planAgyWaitingSettled.lastError).toBe(PLAN_WAITING_HISTORY);

    // Verify DB received the second discarded event
    const allDiscardedInDb = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((e) => e.type === "agent.discarded");
    expect(allDiscardedInDb).toHaveLength(2);

    // Unconditional push: verify the discarded event was pushed to onAgentEvent (exactly once)
    expect(pushedEvents.length).toBe(pushedCountBeforeAgyWaiting + 1);
    const agyWaitingDiscarded = pushedEvents[pushedEvents.length - 1];
    if (!agyWaitingDiscarded) throw new Error("expected pushed agy waiting event");
    expect(agyWaitingDiscarded.payload).toMatchObject({
      agent: "agy",
      reason: PLAN_WAITING_HISTORY,
    });

    // Downstream projector assertion: agy PLAN_WAITING_HISTORY discarded with fallbackOutcome projects as stalled
    const agyProjection = projectAgentOutcomes([toWireRecord(agyWaitingDiscarded)]);
    expect(agyProjection.outcomes).toEqual([
      expect.objectContaining({
        kind: "stalled",
        reason: PLAN_WAITING_HISTORY,
      }),
    ]);

    // 3. pi conventional PLAN_WAITING_HISTORY discarded on this path is pushed unconditionally,
    // and pi projector projects it as failed (wakes orchestrator)
    const planRowPiWaiting = harness.statusEventPlans.insertPending({
      agent: piAgent,
      from: "working",
      to: "done",
    });
    harness.sqlite
      .prepare("update status_event_plans set attempts = ?, last_error = ? where id = ?")
      .run(STATUS_PLAN_MAX_ATTEMPTS, PLAN_WAITING_HISTORY, planRowPiWaiting.id);

    const pushedCountBeforePiWaiting = pushedEvents.length;
    await index.drainPendingPlans({ reason: "startup" });

    const planPiWaitingSettled = harness.statusEventPlans.get(planRowPiWaiting.id);
    expect(planPiWaitingSettled.status).toBe("discarded");
    expect(planPiWaitingSettled.lastError).toBe(PLAN_WAITING_HISTORY);

    // Verify DB received the third discarded event
    const allDiscardedInDbAfterPi = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((e) => e.type === "agent.discarded");
    expect(allDiscardedInDbAfterPi).toHaveLength(3);

    // Unconditional push: verify pi discarded event was pushed to onAgentEvent (exactly once)
    expect(pushedEvents.length).toBe(pushedCountBeforePiWaiting + 1);
    const piWaitingDiscarded = pushedEvents[pushedEvents.length - 1];
    if (!piWaitingDiscarded) throw new Error("expected pushed pi waiting event");
    expect(piWaitingDiscarded.payload).toMatchObject({
      agent: "pi",
      reason: PLAN_WAITING_HISTORY,
    });

    // Downstream projector assertion: pi PLAN_WAITING_HISTORY discarded wakes as failed
    const piProjection = projectAgentOutcomes([toWireRecord(piWaitingDiscarded)]);
    expect(piProjection.outcomes).toEqual([
      expect.objectContaining({
        agent: "pi",
        kind: "failed",
        reason: PLAN_WAITING_HISTORY,
      }),
    ]);

    index.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });
});

describe("agy late completion replay: third-round & M1 regressions (F4/F6/F7/M1)", () => {
  const frozenAgyHistory: CompactAgentHistory = {
    ...emptyCompactHistory("antigravity-sqlite"),
    lastAssistantMessage: null,
    lastToolResult: {
      compaction: { mode: "unknown", originalChars: 10, returnedChars: 10 },
      isError: false,
      ref: "frozen-fingerprint",
      text: "frozen progress fingerprint",
      toolName: "bash",
    },
    messageCount: 1,
    source: "antigravity-sqlite",
    updatedAt: new Date(1700000000000).toISOString(),
  };

  const sessionInputF4 = () => ({
    herdrSessionName: "default",
    sessionDir: "/tmp/herdr",
    socketPath: "/tmp/herdr.sock",
  });

  const agyPane = (status: string) =>
    snapshot(
      [
        agent({
          agent: "agy",
          agent_status: status,
          name: "worker",
          pane_generation: "gen-f4",
          pane_id: "wJ:p2",
          revision: 10,
          terminal_id: "term_agy",
          workspace_id: "wJ",
        }),
      ],
      [{ pane_id: "wJ:p2", revision: 10 }],
    );

  /**
   * F4: the `pane.closed` salvage path must hand the orchestrator the round that
   * stalled and only landed after the window expired, and the row must survive
   * the pane-close invalidation as a retained outcome.
   */
  test("F4: pane.closed after a stall replays the late completion and retains it for delivery", async () => {
    vi.useFakeTimers();
    try {
      const harness = openObservabilityDbHarness();
      let currentAssistantMsg: { ref: string; text: string; timestamp: string | null } | null =
        null;

      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return agyPane("working");
          },
        }),
        history: {
          async resolveCompactHistory() {
            return {
              compactHistory: {
                ...emptyCompactHistory("antigravity-sqlite"),
                lastAssistantMessage: currentAssistantMsg,
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        statusPlansConfig: {
          backoff_factor: 2,
          initial_backoff_ms: 5_000,
          max_backoff_ms: 60_000,
          total_window_ms: 60_000,
        },
        stores: harness,
      });

      await index.refreshHerdrSession(sessionInputF4());
      const handlePromise = index.handleHerdrEvent({
        event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInputF4(),
      });
      await vi.advanceTimersByTimeAsync(4000);
      await handlePromise;
      for (const delay of [5_000, 10_000, 20_000, 40_000]) {
        await vi.advanceTimersByTimeAsync(delay);
        await vi.advanceTimersByTimeAsync(4000);
      }

      const stalledDiscard = harness.agentEvents
        .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
        .filter((e) => e.type === "agent.discarded");
      expect(stalledDiscard).toHaveLength(1);
      expect(stalledDiscard[0]?.payload).toMatchObject({
        fallbackOutcome: true,
        reason: PLAN_WAITING_HISTORY,
      });

      // The answer only lands now, and the orchestrator closes the pane right
      // after: `pane.closed` has to salvage it before retiring the pane.
      currentAssistantMsg = {
        ref: "late-f4-turn",
        text: "answer that arrived after the pane was closed",
        timestamp: null,
      };
      const closed = await index.handleHerdrEvent({
        event: { pane_generation: "gen-f4", pane_id: "wJ:p2", type: "pane.closed" },
        ...sessionInputF4(),
      });
      const lateEvents = closed.events.filter(
        (e) => e.type === "agent.idle" || e.type === "agent.done",
      );
      expect(lateEvents).toHaveLength(1);
      expect(lateEvents[0]?.payload).toMatchObject({
        from: "working",
        lateCompletion: true,
        to: "idle",
      });

      // The row survives the close as a retained outcome: invalidated, but still
      // deliverable, with the pane-close retention reason.
      const retained = harness.agentEvents
        .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
        .find((e) => e.id === lateEvents[0]?.id);
      expect(retained?.status).toBe("invalidated");
      expect(retained?.deliverable).toBe(1);
      expect(retained?.invalidatedReason).toBe("RETAINED_OUTCOME_PANE_CLOSED");

      // …and the shared delivery predicate lets it through for the pane's owner,
      // even though the `agents` row is gone by now.
      expect(
        isDeliverableAgentEvent(
          retained as AgentEventRecord,
          undefined,
          { herdrSessionName: "default", workspaceId: "wJ" },
          "term_owner",
        ),
      ).toBe(true);

      index.stopWaitingHistoryRetries();
      harness.sqlite.close();
    } finally {
      vi.useRealTimers();
    }
  });

  /**
   * F6: Herdr jitters `idle <-> working`, so every retry tick sees a live
   * `working` pane and resets the idle anchor — the 15 minute window never
   * accumulates and the pre-existing stall fallback settles the plan as
   * `PLAN_STALLED`. That discard must arm the late replay and must project as
   * the self-readable `stalled`, not as a plain `failed`.
   */
  test("F6: an idle/working jitter settles through the stall branch as a fallback-armed, stalled-projected discard", async () => {
    const harness = openObservabilityDbHarness();
    let timerSequence = 0;
    const scheduledCallbacks = new Map<unknown, () => Promise<void> | void>();
    const scheduleRetryMock = vi.fn((callback: () => void) => {
      timerSequence += 1;
      scheduledCallbacks.set(timerSequence, callback);
      return timerSequence;
    });

    // Live Herdr status the fake client reports. The replay is only allowed for a
    // settled pane (M1), so the tests pin it to `idle` once the answer lands.
    let paneStatus = "working";

    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return agyPane(paneStatus);
        },
      }),
      history: {
        async resolveCompactHistory() {
          // Frozen fingerprint: no progress is ever observed.
          return {
            compactHistory: frozenAgyHistory,
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      scheduleRetry: scheduleRetryMock,
      sleep: async () => {},
      stores: harness,
    });

    await index.refreshHerdrSession(sessionInputF4());
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected agent");
    expect(agent.agentStatus).toBe("working");

    const planRow = harness.statusEventPlans.insertPending({
      agent,
      from: "working",
      to: "done",
    });
    await index.executeStatusEventPlan({
      agent,
      compactHistory: frozenAgyHistory,
      from: "working",
      planId: planRow.id,
      to: "done",
    });
    expect(harness.statusEventPlans.get(planRow.id).status).toBe("pending");

    const waitingPlan = () =>
      harness.statusEventPlans
        .listUnfinished()
        .find((row) => row.fromStatus === "working" && row.toStatus === "done");
    const latestCallback = (): (() => Promise<void> | void) => {
      const id = scheduleRetryMock.mock.results[scheduleRetryMock.mock.results.length - 1]?.value;
      const callback = scheduledCallbacks.get(id);
      if (!callback) throw new Error(`expected callback for timer ${String(id)}`);
      return callback;
    };
    /** Jitter: every tick flips the agents row, as Herdr does in the field. */
    const jitterFlip = async (attempt: number): Promise<void> => {
      await index.handleHerdrEventFast({
        event: {
          agent_status: attempt % 2 === 1 ? "working" : "idle",
          pane_id: "wJ:p2",
          type: "pane.agent_status_changed",
        },
        ...sessionInputF4(),
      });
    };

    for (let attempt = 1; attempt < WORKING_PLAN_MAX_STALL_ATTEMPTS; attempt += 1) {
      await jitterFlip(attempt);
      await latestCallback()();
      const plan = waitingPlan();
      expect(plan?.status).toBe("pending");
      expect(plan?.lastError).toBe(PLAN_WAITING_HISTORY);
    }

    // 第 18 次 tick：stallAttempts 达到 WORKING_PLAN_MAX_STALL_ATTEMPTS，结算为 stall。
    await jitterFlip(WORKING_PLAN_MAX_STALL_ATTEMPTS);
    await latestCallback()();

    const settled = harness.statusEventPlans.get(planRow.id);
    expect(settled.status).toBe("discarded");
    expect(settled.lastError).toBe(PLAN_STALLED);

    const discardEvents = harness.agentEvents
      .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
      .filter((e) => e.type === "agent.discarded");
    expect(discardEvents).toHaveLength(1);
    const stallDiscard = discardEvents[0];
    if (!stallDiscard) throw new Error("expected the stall discard event");
    // ① fallbackOutcome 武装补发；② 编排侧投影为 stalled，而不是 failed。
    expect(stallDiscard.payload).toMatchObject({
      agent: "agy",
      fallbackOutcome: true,
      reason: PLAN_STALLED,
    });
    expect(projectAgentOutcomes([toWireRecord(stallDiscard)]).outcomes).toEqual([
      expect.objectContaining({ kind: "stalled", reason: PLAN_STALLED }),
    ]);

    // 抖动结束后迟到的 assistant 仍能补发一次。
    // The jitter has settled: the pane is idle again when the answer lands.
    paneStatus = "idle";
    const jitterAssistant = {
      ref: "late-f6-turn",
      text: "answer that landed after the jitter stalled the plan",
      timestamp: null,
    };
    const lateIndex = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return agyPane(paneStatus);
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("antigravity-sqlite"),
              lastAssistantMessage: jitterAssistant,
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
    const replay = await lateIndex.refreshHerdrSession(sessionInputF4());
    const replayed = replay.events.filter(
      (e) => e.type === "agent.idle" || e.type === "agent.done",
    );
    expect(replayed).toHaveLength(1);
    expect(replayed[0]?.payload).toMatchObject({
      from: "working",
      lateCompletion: true,
      to: "idle",
    });
    const secondReplay = await lateIndex.refreshHerdrSession(sessionInputF4());
    expect(
      secondReplay.events.filter((e) => e.type === "agent.idle" || e.type === "agent.done"),
    ).toHaveLength(0);

    index.stopWaitingHistoryRetries();
    lateIndex.stopWaitingHistoryRetries();
    harness.sqlite.close();
  });

  /**
   * F7: the arming judgement reads the plan record, so an `agent.failed` (or a
   * status change, or a discard without the fallback marker) landing between the
   * stall and the late answer must not disarm the replay.
   */
  test("F7: an intervening agent.failed does not disarm the late completion replay", async () => {
    vi.useFakeTimers();
    try {
      const harness = openObservabilityDbHarness();
      let currentAssistantMsg: { ref: string; text: string; timestamp: string | null } | null =
        null;
      // Live Herdr status the fake client reports; the pane is settled (`idle`) by
      // the time the late answer lands, which is what M1 requires for a replay.
      let paneStatus = "working";

      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return agyPane(paneStatus);
          },
        }),
        history: {
          async resolveCompactHistory() {
            return {
              compactHistory: {
                ...emptyCompactHistory("antigravity-sqlite"),
                lastAssistantMessage: currentAssistantMsg,
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        statusPlansConfig: {
          backoff_factor: 2,
          initial_backoff_ms: 5_000,
          max_backoff_ms: 60_000,
          total_window_ms: 60_000,
        },
        stores: harness,
      });

      await index.refreshHerdrSession(sessionInputF4());
      const stalledAgent = harness.agents.findByPane({
        herdrSessionName: "default",
        paneId: "wJ:p2",
      });
      if (!stalledAgent) throw new Error("expected agent");

      const handlePromise = index.handleHerdrEvent({
        event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInputF4(),
      });
      await vi.advanceTimersByTimeAsync(4000);
      await handlePromise;
      for (const delay of [5_000, 10_000, 20_000, 40_000]) {
        await vi.advanceTimersByTimeAsync(delay);
        await vi.advanceTimersByTimeAsync(4000);
      }
      expect(
        harness.agentEvents
          .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
          .filter((e) => e.type === "agent.discarded"),
      ).toHaveLength(1);

      // An unrelated round of the same pane fails in between, and a plain status
      // change is recorded too: neither may look like "the pane moved on".
      harness.agentEvents.append({
        agentId: stalledAgent.id,
        herdrSessionName: "default",
        idempotencyKey: "agent.failed:f7-intervening",
        paneGeneration: "gen-f4",
        paneId: "wJ:p2",
        payload: {
          agent: "agy",
          attempts: 8,
          from: "working",
          reason: "unrelated runtime failure",
          to: "done",
        },
        terminalId: "term_agy",
        type: "agent.failed",
        workspaceId: "wJ",
      });
      harness.agentEvents.append({
        agentId: stalledAgent.id,
        herdrSessionName: "default",
        idempotencyKey: "agent.status.changed:f7-intervening",
        paneGeneration: "gen-f4",
        paneId: "wJ:p2",
        payload: { agent: "agy", from: "idle", to: "working" },
        terminalId: "term_agy",
        type: "agent.status.changed",
        workspaceId: "wJ",
      });

      paneStatus = "idle";
      currentAssistantMsg = {
        ref: "late-f7-turn",
        text: "answer that landed after an unrelated failure",
        timestamp: null,
      };
      const replay = await index.refreshHerdrSession(sessionInputF4());
      const replayed = replay.events.filter(
        (e) => e.type === "agent.idle" || e.type === "agent.done",
      );
      expect(replayed).toHaveLength(1);
      expect(replayed[0]?.payload).toMatchObject({
        from: "working",
        lateCompletion: true,
        to: "idle",
      });
      expect(
        projectAgentOutcomes([toWireRecord(replayed[0] as AgentEventRecord)]).outcomes,
      ).toEqual([expect.objectContaining({ kind: "completed" })]);

      const second = await index.refreshHerdrSession(sessionInputF4());
      expect(
        second.events.filter((e) => e.type === "agent.idle" || e.type === "agent.done"),
      ).toHaveLength(0);

      index.stopWaitingHistoryRetries();
      harness.sqlite.close();
    } finally {
      vi.useRealTimers();
    }
  });

  /**
   * M1 (disarm): once the replay fired, the stale `discarded` plan row must stop
   * arming it. Before the disarm conditions every 10s inspection treated the pane
   * as still stalled and re-read sqlite with `forceRefresh: true` for the whole
   * 7-day settled-event TTL.
   */
  test("M1: a landed replay disarms the stalled plan instead of forcing a re-read on every inspection", async () => {
    vi.useFakeTimers();
    try {
      const harness = openObservabilityDbHarness();
      let currentAssistantMsg: { ref: string; text: string; timestamp: string | null } | null =
        null;
      let paneStatus = "working";
      // `forceRefresh: true` is how the stalled judgement reaches the history
      // reader (the retry ring uses the same flag), so the counter is reset after
      // the stall to isolate the inspections.
      const forceRefreshReads: Array<boolean | undefined> = [];

      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return agyPane(paneStatus);
          },
        }),
        history: {
          async resolveCompactHistory(_lookup: unknown, options?: { forceRefresh?: boolean }) {
            forceRefreshReads.push(options?.forceRefresh);
            return {
              compactHistory: {
                ...emptyCompactHistory("antigravity-sqlite"),
                lastAssistantMessage: currentAssistantMsg,
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        statusPlansConfig: {
          backoff_factor: 2,
          initial_backoff_ms: 5_000,
          max_backoff_ms: 60_000,
          total_window_ms: 60_000,
        },
        stores: harness,
      });

      // 1. The round stalls: the plan exhausts the elastic window and is discarded
      //    with the fallback marker, which arms the replay.
      await index.refreshHerdrSession(sessionInputF4());
      const handlePromise = index.handleHerdrEvent({
        event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInputF4(),
      });
      await vi.advanceTimersByTimeAsync(4000);
      await handlePromise;
      for (const delay of [5_000, 10_000, 20_000, 40_000]) {
        await vi.advanceTimersByTimeAsync(delay);
        await vi.advanceTimersByTimeAsync(4000);
      }
      const discarded = harness.agentEvents
        .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
        .filter((e) => e.type === "agent.discarded");
      expect(discarded).toHaveLength(1);
      expect(discarded[0]?.payload).toMatchObject({
        fallbackOutcome: true,
        reason: PLAN_WAITING_HISTORY,
      });
      forceRefreshReads.length = 0;

      // 2. The answer lands while the pane is settled (`idle`): replayed exactly
      //    once, and the still-armed replay forces the read.
      paneStatus = "idle";
      currentAssistantMsg = { ref: "ref-1", text: "first round answer", timestamp: null };
      const replay = await index.refreshHerdrSession(sessionInputF4());
      const replayed = replay.events.filter(
        (e) => e.type === "agent.idle" || e.type === "agent.done",
      );
      expect(replayed).toHaveLength(1);
      expect(replayed[0]?.payload).toMatchObject({ from: "working", lateCompletion: true });
      expect(forceRefreshReads.filter(Boolean)).toHaveLength(1);

      // 3. Disarmed: the next inspection neither replays again nor forces another
      //    history read for the stalled pane.
      const second = await index.refreshHerdrSession(sessionInputF4());
      expect(
        second.events.filter((e) => e.type === "agent.idle" || e.type === "agent.done"),
      ).toHaveLength(0);
      expect(forceRefreshReads.filter(Boolean)).toHaveLength(1);

      index.stopWaitingHistoryRetries();
      harness.sqlite.close();
    } finally {
      vi.useRealTimers();
    }
  });

  /**
   * M1 (round boundary): a replay is only allowed for a settled (`idle`/`done`)
   * pane. While the pane reports `working` — the next round of the same pane — the
   * intermediate assistant body on disk must not be replayed as a fake
   * `working -> idle` completion, and the stalled branch must not force a re-read
   * either. The round's real end is then settled by the ordinary plan path.
   */
  test("M1: a working round is never replayed as a late completion", async () => {
    vi.useFakeTimers();
    try {
      const harness = openObservabilityDbHarness();
      let currentAssistantMsg: { ref: string; text: string; timestamp: string | null } | null =
        null;
      let paneStatus = "working";
      const forceRefreshReads: Array<boolean | undefined> = [];

      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return agyPane(paneStatus);
          },
        }),
        history: {
          async resolveCompactHistory(_lookup: unknown, options?: { forceRefresh?: boolean }) {
            forceRefreshReads.push(options?.forceRefresh);
            return {
              compactHistory: {
                ...emptyCompactHistory("antigravity-sqlite"),
                lastAssistantMessage: currentAssistantMsg,
              },
              historyRef: null,
              sourceFingerprint: null,
            };
          },
        } as unknown as AgentHistoryService,
        statusPlansConfig: {
          backoff_factor: 2,
          initial_backoff_ms: 5_000,
          max_backoff_ms: 60_000,
          total_window_ms: 60_000,
        },
        stores: harness,
      });

      // 1. The round stalls and arms the replay.
      await index.refreshHerdrSession(sessionInputF4());
      const handlePromise = index.handleHerdrEvent({
        event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
        ...sessionInputF4(),
      });
      await vi.advanceTimersByTimeAsync(4000);
      await handlePromise;
      for (const delay of [5_000, 10_000, 20_000, 40_000]) {
        await vi.advanceTimersByTimeAsync(delay);
        await vi.advanceTimersByTimeAsync(4000);
      }
      expect(
        harness.agentEvents
          .listAfter({ herdrSessionName: "default", workspaceId: "wJ" })
          .filter((e) => e.type === "agent.discarded"),
      ).toHaveLength(1);
      forceRefreshReads.length = 0;

      // 2. The pane is working again (the round resumed) and the body on disk is an
      //    intermediate one: no completion may be emitted, and the stalled branch
      //    must not force a re-read mid-round.
      paneStatus = "working";
      currentAssistantMsg = {
        ref: "ref-2",
        text: "intermediate body of the resumed round",
        timestamp: null,
      };
      const working = await index.refreshHerdrSession(sessionInputF4());
      expect(
        working.events.filter((e) => e.type === "agent.idle" || e.type === "agent.done"),
      ).toHaveLength(0);
      expect(
        working.events.filter(
          (e) => (e.payload as Record<string, unknown>).lateCompletion === true,
        ),
      ).toHaveLength(0);
      expect(forceRefreshReads.filter(Boolean)).toHaveLength(0);

      // 3. The round really ends: `idle` with the final body `ref-3` settles as an
      //    ordinary completed round, not as a late completion.
      paneStatus = "idle";
      currentAssistantMsg = { ref: "ref-3", text: "resumed round final answer", timestamp: null };
      const settled = await index.refreshHerdrSession(sessionInputF4());
      const completions = settled.events.filter(
        (e) => e.type === "agent.idle" || e.type === "agent.done",
      );
      expect(completions).toHaveLength(1);
      expect(completions[0]?.payload).toMatchObject({ from: "working", to: "idle" });
      expect((completions[0]?.payload as Record<string, unknown>).lateCompletion).toBeUndefined();

      index.stopWaitingHistoryRetries();
      harness.sqlite.close();
    } finally {
      vi.useRealTimers();
    }
  });
});
