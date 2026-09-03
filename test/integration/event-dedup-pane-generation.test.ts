import { afterEach, describe, expect, test, vi } from "vitest";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { AgentIndexService } from "@/observability/agent-index-service.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

vi.setConfig({ testTimeout: 30_000 });

afterEach(cleanupTempDirs);
const dbSession = { name: "default", sessionDir: "/tmp/herdr", socketPath: "/tmp/herdr.sock" };
const session = {
  herdrSessionName: "default",
  sessionDir: "/tmp/herdr",
  socketPath: "/tmp/herdr.sock",
};
const scope = { herdrSessionName: "default", workspaceId: "wJ" };
function agent(name: string, generation: string) {
  return {
    agent: name,
    agent_status: "working",
    cwd: "/repo",
    pane_id: "wJ:p2",
    pane_generation: generation,
    revision: 1,
    terminal_id: `term-${name}`,
    workspace_id: "wJ",
  };
}

function historyStub() {
  return {
    async resolveCompactHistory() {
      return {
        compactHistory: {
          ...emptyCompactHistory("claude-jsonl"),
          lastAssistantMessage: { ref: "x", text: "result", timestamp: null },
        },
        historyRef: null,
        sourceFingerprint: null,
      };
    },
  } as never;
}

describe("event deduplication and pane generations", () => {
  test("does not persist the same Herdr input event twice", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return {
            snapshot: {
              agents: [agent("claude", "gen-1")],
              panes: [{ pane_id: "wJ:p2", revision: 1 }],
              tabs: [],
              workspaces: [],
            },
          };
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("claude-jsonl"),
              lastAssistantMessage: { ref: "x", text: "result", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as never,
      stores: h,
    });
    await index.refreshHerdrSession(session);
    const input = {
      event: {
        agent_status: "done",
        event_id: "herdr-42",
        pane_id: "wJ:p2",
        type: "pane.agent_status_changed",
      },
      ...session,
    };
    await index.handleHerdrEvent(input);
    await index.handleHerdrEvent(input);
    expect(h.agentEvents.listAfter(scope).filter((e) => e.type === "agent.done")).toHaveLength(1);
    h.sqlite.close();
  });
  test("invalidates closed-pane events and isolates a new generation identity", () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const old = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [agent("old", "gen-1")],
    })[0];
    if (!old) throw new Error("Expected old agent");
    const e = h.agentEvents.append({
      agentId: old.id,
      ...scope,
      paneId: "wJ:p2",
      paneGeneration: "gen-1",
      payload: {},
      terminalId: "term-old",
      type: "agent.done",
    });
    h.agentEvents.invalidatePane({
      herdrSessionName: "default",
      paneId: "wJ:p2",
      paneGeneration: "gen-1",
    });
    expect(
      h.agentEvents.nextDeliverableAfter({
        ...scope,
        afterEventId: 0,
        ownerTerminalId: "term-owner",
      }),
    ).toBeUndefined();
    const fresh = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [agent("new", "gen-2")],
    })[0];
    if (!fresh) throw new Error("Expected fresh agent");
    expect(fresh.id).not.toBe(old.id);
    expect(h.agentEvents.get(e.id).paneGeneration).toBe("gen-1");
    h.sqlite.close();
  });
  test("retires only the closed pane generation when a pane id is reused", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const index = new AgentIndexService({ stores: h });
    const old = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [agent("old", "gen-1")],
    })[0];
    if (!old) throw new Error("Expected old agent");
    const oldEvent = h.agentEvents.append({
      agentId: old.id,
      ...scope,
      paneId: old.paneId,
      paneGeneration: old.paneGeneration ?? null,
      payload: {},
      terminalId: old.terminalId,
      type: "agent.done",
    });

    await index.handleHerdrEvent({
      event: { pane_id: old.paneId, pane_generation: old.paneGeneration, type: "pane.closed" },
      ...session,
    });
    const fresh = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [agent("fresh", "gen-2")],
    })[0];
    if (!fresh) throw new Error("Expected fresh agent");
    const freshEvent = h.agentEvents.append({
      agentId: fresh.id,
      ...scope,
      paneId: fresh.paneId,
      paneGeneration: fresh.paneGeneration ?? null,
      payload: {},
      terminalId: fresh.terminalId,
      type: "agent.done",
    });

    await index.handleHerdrEvent({
      event: { pane_id: fresh.paneId, pane_generation: "gen-1", type: "pane.closed" },
      ...session,
    });
    expect(
      h.agents.findByPane({ ...scope, paneId: "wJ:p2", paneGeneration: "gen-1" }),
    ).toBeUndefined();
    expect(
      h.agents.findByPane({ ...scope, paneId: "wJ:p2", paneGeneration: "gen-2" }),
    ).toMatchObject({
      id: fresh.id,
      agent: "fresh",
      agentStatus: "working",
    });
    expect(h.agentEvents.get(oldEvent.id)).toMatchObject({
      status: "invalidated",
      invalidatedReason: "PANE_CLOSED",
    });
    expect(h.agentEvents.get(freshEvent.id)).toMatchObject({
      status: "pending",
      paneGeneration: "gen-2",
    });
    h.sqlite.close();
  });

  test("pane.closed retires and invalidates the matching generation", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const indexed = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [agent("claude", "gen-7")],
    })[0];
    if (!indexed) throw new Error("Expected agent");
    const event = h.agentEvents.append({
      agentId: indexed.id,
      ...scope,
      paneId: indexed.paneId,
      paneGeneration: indexed.paneGeneration ?? null,
      payload: {},
      terminalId: indexed.terminalId,
      type: "agent.done",
    });
    await new AgentIndexService({ stores: h }).handleHerdrEvent({
      event: { pane_id: "wJ:p2", pane_generation: "gen-7", type: "pane.closed" },
      ...session,
    });
    expect(
      h.agents.findByPane({ ...scope, paneId: "wJ:p2", paneGeneration: "gen-7" }),
    ).toBeUndefined();
    expect(h.agentEvents.get(event.id)).toMatchObject({
      status: "invalidated",
      invalidatedReason: "PANE_CLOSED",
    });
    h.sqlite.close();
  });

  test("legacy pane.closed invalidates legacy events with an explicit reason", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const indexed = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [{ ...agent("claude", "legacy-gen"), pane_generation: undefined }],
    })[0];
    if (!indexed) throw new Error("Expected agent");
    const event = h.agentEvents.append({
      agentId: indexed.id,
      ...scope,
      paneId: indexed.paneId,
      paneGeneration: null,
      payload: {},
      terminalId: indexed.terminalId,
      type: "agent.done",
    });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    await new AgentIndexService({ stores: h }).handleHerdrEvent({
      event: { pane_id: "wJ:p2", type: "pane.closed" },
      ...session,
    });
    expect(h.agentEvents.get(event.id)).toMatchObject({
      status: "invalidated",
      invalidatedReason: "LEGACY_CLOSE_WITHOUT_GENERATION",
    });
    expect(warn).toHaveBeenCalledWith("LEGACY_CLOSE_WITHOUT_GENERATION", expect.any(Object));
    warn.mockRestore();
    h.sqlite.close();
  });

  test("legacy pane.closed invalidates pending events from every pane generation", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const indexed = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [agent("claude", "gen-1")],
    })[0];
    if (!indexed) throw new Error("Expected agent");
    const event = h.agentEvents.append({
      agentId: indexed.id,
      ...scope,
      paneId: indexed.paneId,
      paneGeneration: "gen-1",
      payload: {},
      terminalId: indexed.terminalId,
      type: "agent.done",
    });
    const result = await new AgentIndexService({ stores: h }).handleHerdrEvent({
      event: { pane_id: indexed.paneId, type: "pane.closed" },
      ...session,
    });
    expect(h.agentEvents.get(event.id)).toMatchObject({
      status: "invalidated",
      invalidatedReason: "LEGACY_CLOSE_WITHOUT_GENERATION",
    });
    // A legacy close invalidates pending done events but never emits done events.
    expect(result.events).toEqual([]);
    h.sqlite.close();
  });
  test("pane.closed tombstone: late done does not refresh or append new events", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    let snapshots = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          snapshots += 1;
          return {
            snapshot: {
              agents: [agent("claude", "gen-1")],
              panes: [{ pane_id: "wJ:p2", revision: 1 }],
              tabs: [],
              workspaces: [],
            },
          };
        },
      }),
      history: historyStub(),
      stores: h,
    });
    await index.refreshHerdrSession(session);
    expect(
      h.agents.findByPane({ ...scope, paneId: "wJ:p2", paneGeneration: "gen-1" }),
    ).toBeDefined();
    await index.handleHerdrEvent({
      event: { pane_id: "wJ:p2", pane_generation: "gen-1", type: "pane.closed" },
      ...session,
    });
    const snapshotsAfterClose = snapshots;
    const late = await index.handleHerdrEvent({
      event: {
        agent_status: "done",
        pane_id: "wJ:p2",
        pane_generation: "gen-1",
        type: "pane.agent_status_changed",
      },
      ...session,
    });
    expect(snapshots).toBe(snapshotsAfterClose);
    expect(late).toEqual({ contextChangedScopes: [], events: [] });
    expect(
      h.agents.findByPane({ ...scope, paneId: "wJ:p2", paneGeneration: "gen-1" }),
    ).toBeUndefined();
    expect(h.agentEvents.listAfter(scope).filter((e) => e.type === "agent.done")).toHaveLength(0);
    h.sqlite.close();
  });
  test("pane.closed tombstone: refresh with a stale snapshot does not restore the retired agent", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return {
            snapshot: {
              agents: [agent("claude", "gen-1")],
              panes: [{ pane_id: "wJ:p2", revision: 1 }],
              tabs: [],
              workspaces: [],
            },
          };
        },
      }),
      history: historyStub(),
      stores: h,
    });
    await index.refreshHerdrSession(session);
    await index.handleHerdrEvent({
      event: { pane_id: "wJ:p2", pane_generation: "gen-1", type: "pane.closed" },
      ...session,
    });
    const refreshed = await index.refreshHerdrSession(session);
    expect(refreshed.agents).toHaveLength(0);
    expect(
      h.agents.findByPane({ ...scope, paneId: "wJ:p2", paneGeneration: "gen-1" }),
    ).toBeUndefined();
    expect(h.agents.listForHerdrSession("default")).toHaveLength(0);
    h.sqlite.close();
  });
  test("legacy pane.closed releases an owner matching the closed pane and terminal", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    h.agentOrchestratorScopes.claim({
      ...scope,
      ackedEventId: 0,
      paneId: "wJ:p2",
      terminalId: "term-owner",
    });
    h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [agent("claude", "gen-1")],
    });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    await new AgentIndexService({ stores: h }).handleHerdrEvent({
      event: { pane_id: "wJ:p2", terminal_id: "term-owner", type: "pane.closed" },
      ...session,
    });
    expect(h.agentOrchestratorScopes.get(scope)?.owner).toBeNull();
    warn.mockRestore();
    h.sqlite.close();
  });
  test("isPaneClosed: a legacy tombstone matches every pane generation", () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    // Fix the semantics: markPaneClosed(paneGeneration: "gen-1") tombstones only
    // gen-1, while markPaneClosed(paneGeneration: null) (a legacy close) matches
    // any generation — including a later gen-2 — mirroring
    // AgentEventStore.invalidatePane legacy semantics.
    h.agents.markPaneClosed({
      herdrSessionName: "default",
      paneId: "wJ:p2",
      paneGeneration: "gen-1",
    });
    h.agents.markPaneClosed({
      herdrSessionName: "default",
      paneId: "wJ:p2",
      paneGeneration: null,
    });
    expect(
      h.agents.isPaneClosed({
        herdrSessionName: "default",
        paneId: "wJ:p2",
        paneGeneration: "gen-1",
      }),
    ).toBe(true);
    expect(
      h.agents.isPaneClosed({
        herdrSessionName: "default",
        paneId: "wJ:p2",
        paneGeneration: "gen-2",
      }),
    ).toBe(true);
    // Dropping the legacy tombstone unblocks other generations while the
    // generation-scoped tombstone still closes only its own generation.
    h.agents.clearLegacyPaneTombstone({ herdrSessionName: "default", paneId: "wJ:p2" });
    expect(
      h.agents.isPaneClosed({
        herdrSessionName: "default",
        paneId: "wJ:p2",
        paneGeneration: "gen-2",
      }),
    ).toBe(false);
    expect(
      h.agents.isPaneClosed({
        herdrSessionName: "default",
        paneId: "wJ:p2",
        paneGeneration: "gen-1",
      }),
    ).toBe(true);
    h.sqlite.close();
  });
  test("legacy pane.closed tombstone is cleared when a new generation pane is indexed", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return {
            snapshot: {
              agents: [agent("fresh", "gen-2")],
              panes: [{ pane_id: "wJ:p2", revision: 1 }],
              tabs: [],
              workspaces: [],
            },
          };
        },
      }),
      history: historyStub(),
      stores: h,
    });
    // A legacy (generation-less) close tombstones the pane for every generation.
    await index.handleHerdrEvent({
      event: { pane_id: "wJ:p2", type: "pane.closed" },
      ...session,
    });
    expect(
      h.agents.isPaneClosed({
        herdrSessionName: "default",
        paneId: "wJ:p2",
        paneGeneration: "gen-2",
      }),
    ).toBe(true);
    // Indexing a live gen-2 agent clears the legacy tombstone and restores it.
    const refreshed = await index.refreshHerdrSession(session);
    expect(refreshed.agents).toHaveLength(1);
    expect(
      h.agents.findByPane({ ...scope, paneId: "wJ:p2", paneGeneration: "gen-2" }),
    ).toMatchObject({ agent: "fresh", agentStatus: "working" });
    expect(
      h.agents.isPaneClosed({
        herdrSessionName: "default",
        paneId: "wJ:p2",
        paneGeneration: "gen-2",
      }),
    ).toBe(false);
    h.sqlite.close();
  });
  test("legacy pane.closed does not block a live generation-carrying late status", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return {
            snapshot: {
              agents: [agent("fresh", "gen-2")],
              panes: [{ pane_id: "wJ:p2", revision: 1 }],
              tabs: [],
              workspaces: [],
            },
          };
        },
      }),
      history: historyStub(),
      stores: h,
    });
    await index.handleHerdrEvent({
      event: { pane_id: "wJ:p2", type: "pane.closed" },
      ...session,
    });
    const late = await index.handleHerdrEvent({
      event: {
        agent_status: "done",
        pane_id: "wJ:p2",
        pane_generation: "gen-2",
        type: "pane.agent_status_changed",
      },
      ...session,
    });
    expect(late.events.filter((event) => event.type === "agent.done")).toHaveLength(1);
    expect(
      h.agents.findByPane({ ...scope, paneId: "wJ:p2", paneGeneration: "gen-2" }),
    ).toMatchObject({ agent: "fresh", agentStatus: "done" });
    expect(
      h.agents.isPaneClosed({
        herdrSessionName: "default",
        paneId: "wJ:p2",
        paneGeneration: "gen-2",
      }),
    ).toBe(false);
    h.sqlite.close();
  });
  test("status-derived events persist the source agent pane generation", async () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const index = new AgentIndexService({
      stores: h,
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("claude-jsonl"),
              lastAssistantMessage: { ref: "x", text: "result", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as never,
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return {
            snapshot: {
              agents: [agent("claude", "gen-status")],
              panes: [],
              tabs: [],
              workspaces: [],
            },
          };
        },
      }),
    });
    await index.refreshHerdrSession(session);
    await index.handleHerdrEvent({
      event: {
        agent_status: "done",
        pane_id: "wJ:p2",
        pane_generation: "gen-status",
        type: "pane.agent_status_changed",
      },
      ...session,
    });
    const generated = h.agentEvents.listAfter(scope).filter((event) => event.type === "agent.done");
    expect(generated.length).toBeGreaterThan(0);
    expect(generated.every((event) => event.paneGeneration === "gen-status")).toBe(true);
    h.sqlite.close();
  });
  test("advances cursor past an undeliverable old event", () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning(dbSession);
    const service = new AgentOrchestratorService({
      agentEvents: h.agentEvents,
      agents: h.agents,
      scopes: h.agentOrchestratorScopes,
    });
    service.claim({ ...scope, paneId: "owner", terminalId: "term-owner" });
    const a = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [agent("a", "gen-1")],
    })[0];
    if (!a) throw new Error("Expected agent");
    const e = h.agentEvents.append({
      agentId: a.id,
      ...scope,
      paneId: "wJ:p2",
      paneGeneration: "gen-1",
      payload: {},
      terminalId: "term-agent",
      type: "agent.done",
    });
    h.agentEvents.invalidatePane({
      herdrSessionName: "default",
      paneId: "wJ:p2",
      paneGeneration: "gen-1",
    });
    expect(() => service.ack({ ...scope, eventId: e.id, terminalId: "term-owner" })).toThrow(
      "orchestrator event is no longer pending (invalidated)",
    );
  });
});

describe("agent event pagination regressions (independent coverage)", () => {
  test("nextDeliverableAfter crosses more than 1000 filtered noise events", () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning({
      name: "default",
      sessionDir: "/tmp/herdr",
      socketPath: "/tmp/herdr.sock",
    });
    const agent = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [
        {
          agent: "claude",
          agent_status: "working",
          cwd: "/repo",
          pane_id: "wB:p2",
          pane_generation: "g1",
          revision: 1,
          terminal_id: "term-agent",
          workspace_id: "wB",
        },
      ],
    })[0];
    if (!agent) throw new Error("Expected agent");
    h.sqlite.exec("begin immediate");
    try {
      for (let i = 0; i < 1_001; i += 1)
        h.agentEvents.append({
          agentId: agent.id,
          herdrSessionName: "default",
          workspaceId: "wB",
          paneId: "wB:noise",
          payload: {},
          terminalId: "term-agent",
          type: "agent.done",
        });
      h.sqlite.exec("commit");
    } catch (error) {
      h.sqlite.exec("rollback");
      throw error;
    }
    const pending = h.agentEvents.append({
      agentId: agent.id,
      herdrSessionName: "default",
      workspaceId: "wB",
      paneId: "wB:p2",
      payload: {},
      terminalId: "term-agent",
      type: "agent.done",
    });
    expect(
      h.agentEvents.nextDeliverableAfter({
        afterEventId: 0,
        herdrSessionName: "default",
        workspaceId: "wB",
        ownerTerminalId: "term-owner",
        getAgent: () => agent,
      })?.id,
    ).toBe(pending.id);
    h.sqlite.close();
  }, 30_000);

  test("returns empty and warns when every event is noise", () => {
    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning({
      name: "default",
      sessionDir: "/tmp/herdr",
      socketPath: "/tmp/herdr.sock",
    });
    const agent = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [
        {
          agent: "claude",
          agent_status: "working",
          cwd: "/repo",
          pane_id: "wB:p2",
          pane_generation: "g1",
          revision: 1,
          terminal_id: "term-agent",
          workspace_id: "wB",
        },
      ],
    })[0];
    if (!agent) throw new Error("Expected agent");
    h.sqlite.exec("begin immediate");
    try {
      for (let i = 0; i < 50_001; i += 1)
        h.agentEvents.append({
          agentId: agent.id,
          herdrSessionName: "default",
          workspaceId: "wB",
          paneId: "wB:noise",
          payload: {},
          terminalId: "term-agent",
          type: "agent.done",
        });
      h.sqlite.exec("commit");
    } catch (error) {
      h.sqlite.exec("rollback");
      throw error;
    }
    const warning = vi.spyOn(console, "warn").mockImplementation(() => {});
    expect(
      h.agentEvents.nextDeliverableAfter({
        afterEventId: 0,
        herdrSessionName: "default",
        workspaceId: "wB",
        ownerTerminalId: "term-owner",
        getAgent: () => agent,
      }),
    ).toBeUndefined();
    expect(warning).toHaveBeenCalledWith(
      expect.stringContaining("stopped scanning"),
      expect.anything(),
    );
    warning.mockRestore();
    h.sqlite.close();
  }, 120_000);
});
