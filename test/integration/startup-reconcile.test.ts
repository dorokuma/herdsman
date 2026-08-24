import { afterEach, describe, expect, test, vi } from "vitest";
import { AgentEventReconciler } from "@/daemon/agent-event-reconciler.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

const scope = { herdrSessionName: "default", workspaceId: "wA" };
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
  harness.herdrSessions.upsertRunning(session);
  const agent = harness.agents.replaceForSession({
    herdrSessionName: "default",
    agents: [{ agent: "codex", pane_id: "wA:live", terminal_id: "term-live", workspace_id: "wA" }],
  })[0];
  if (!agent) throw new Error("Expected indexed agent");
  const append = (input: { paneId: string; terminalId: string; generation?: string }) =>
    harness.agentEvents.append({
      agentId: agent.id,
      herdrSessionName: "default",
      paneId: input.paneId,
      ...(input.generation === undefined ? {} : { paneGeneration: input.generation }),
      payload: {},
      terminalId: input.terminalId,
      type: "agent.done",
      workspaceId: "wA",
    });
  return { harness, append };
}

function reconciler(
  harness: ReturnType<typeof openObservabilityDbHarness>,
  panes: unknown[] | (() => Promise<unknown[]>),
) {
  return new AgentEventReconciler({
    events: harness.agentEvents,
    scopes: harness.agentOrchestratorScopes,
    sessionList: async () => [session],
    clientFactory: () =>
      ({
        close: vi.fn(),
        sessionSnapshot: async () => ({
          snapshot: { panes: typeof panes === "function" ? await panes() : panes },
        }),
      }) as never,
    connectedTerminal: () => false,
  });
}

describe("startup reconcile dedicated coverage", () => {
  test("a: invalidates absent pending/delivered panes and releases stale owner, preserving ack", async () => {
    const { harness, append } = setup();
    const zombiePending = append({ paneId: "wA:gone", terminalId: "term-gone" });
    const zombieDelivered = append({ paneId: "wA:gone-2", terminalId: "term-gone-2" });
    harness.agentEvents.reservePending("term-delivery");
    harness.agentOrchestratorScopes.claim({
      ...scope,
      ackedEventId: 77,
      paneId: "wA:gone",
      terminalId: "term-owner",
    });
    const liveEvent = append({ paneId: "wA:live", terminalId: "term-live" });
    const result = await reconciler(harness, [
      { pane_id: "wA:live", terminal_id: "term-live" },
    ]).reconcile();
    expect(result).toEqual({ invalidated: 2, released: 1 });
    expect(
      harness.sqlite
        .prepare("select count(*) as count from agent_events where id in (?, ?)")
        .get(zombiePending.id, zombieDelivered.id),
    ).toEqual({ count: 0 });
    expect(harness.agentEvents.get(liveEvent.id)).toMatchObject({ status: "pending" });
    expect(harness.agentOrchestratorScopes.get(scope)).toMatchObject({
      ackedEventId: 77,
      owner: null,
    });
  });

  test("reconcile physically removes existing invalidated rows but preserves acked rows", async () => {
    const { harness, append } = setup();
    const invalidatedOne = append({ paneId: "wA:gone-1", terminalId: "term-1" });
    const invalidatedTwo = append({ paneId: "wA:gone-2", terminalId: "term-2" });
    const acked = append({ paneId: "wA:gone-acked", terminalId: "term-acked" });
    harness.sqlite
      .prepare("update agent_events set status = 'invalidated' where id in (?, ?)")
      .run(invalidatedOne.id, invalidatedTwo.id);
    harness.sqlite.prepare("update agent_events set status = 'acked' where id = ?").run(acked.id);
    await reconciler(harness, []).reconcile();
    expect(
      harness.sqlite
        .prepare("select count(*) as count from agent_events where status = 'invalidated'")
        .get(),
    ).toEqual({ count: 0 });
    expect(harness.agentEvents.get(acked.id)).toMatchObject({ status: "acked" });
  });
  test("b: snapshot failure makes no database changes and logs the skip", async () => {
    const { harness, append } = setup();
    const event = append({ paneId: "wA:gone", terminalId: "term-gone" });
    harness.agentOrchestratorScopes.claim({
      ...scope,
      ackedEventId: event.id,
      paneId: "wA:gone",
      terminalId: "term-owner",
    });
    const before = harness.agentOrchestratorScopes.get(scope);
    const warning = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const result = await reconciler(harness, async () => {
      throw new Error("snapshot unavailable");
    }).reconcile();
    expect(result).toEqual({ invalidated: 0, released: 0 });
    expect(harness.agentEvents.get(event.id)).toMatchObject({ status: "pending" });
    expect(harness.agentOrchestratorScopes.get(scope)).toEqual(before);
    expect(warning).toHaveBeenCalledWith(
      "Herdsman reconcile skipped: incomplete Herdr pane snapshot",
      expect.any(Error),
    );
  });

  test("skips reconciliation for every session when a later session snapshot fails", async () => {
    const { harness, append } = setup();
    const event = append({ paneId: "wA:gone", terminalId: "term-gone" });
    const secondSession = { ...session, name: "second", socketPath: "/tmp/herdr-second.sock" };
    const warning = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const result = await new AgentEventReconciler({
      events: harness.agentEvents,
      scopes: harness.agentOrchestratorScopes,
      sessionList: async () => [session, secondSession],
      clientFactory: (entry) =>
        ({
          close: vi.fn(),
          sessionSnapshot: async () => {
            if (entry.name === "second") throw new Error("second snapshot unavailable");
            return { snapshot: { panes: [{ pane_id: "wA:gone", terminal_id: "term-gone" }] } };
          },
        }) as never,
    }).reconcile();
    expect(result).toEqual({ invalidated: 0, released: 0 });
    expect(harness.agentEvents.get(event.id)).toMatchObject({ status: "pending" });
    expect(warning).toHaveBeenCalledWith(
      "Herdsman reconcile skipped: incomplete Herdr pane snapshot",
      expect.any(Error),
    );
  });
  test("b: session list failure makes no database changes and logs the skip", async () => {
    const { harness, append } = setup();
    const event = append({ paneId: "wA:gone", terminalId: "term-gone" });
    const warning = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const result = await new AgentEventReconciler({
      events: harness.agentEvents,
      scopes: harness.agentOrchestratorScopes,
      sessionList: async () => {
        throw new Error("list unavailable");
      },
    }).reconcile();
    expect(result).toEqual({ invalidated: 0, released: 0 });
    expect(harness.agentEvents.get(event.id)).toMatchObject({ status: "pending" });
    expect(warning).toHaveBeenCalledWith(
      "Herdsman reconcile skipped: Herdr session list unavailable",
      expect.any(Error),
    );
  });

  test("invalidates all orphan events across reconcile pages", async () => {
    const { harness, append } = setup();
    for (let index = 0; index < 250; index += 1)
      append({ paneId: `wA:gone-${index}`, terminalId: `term-${index}` });
    await reconciler(harness, []).reconcile();
    expect(harness.agentEvents.listReconcileCandidates(300)).toHaveLength(0);
    expect(
      harness.sqlite
        .prepare("select count(*) as count from agent_events where status = 'invalidated'")
        .get(),
    ).toEqual({ count: 0 });
  });
  test("c: reconciling the same state twice is idempotent", async () => {
    const { harness, append } = setup();
    append({ paneId: "wA:gone", terminalId: "term-gone" });
    const instance = reconciler(harness, []);
    expect(await instance.reconcile()).toEqual({ invalidated: 1, released: 0 });
    expect(await instance.reconcile()).toEqual({ invalidated: 0, released: 0 });
  });

  test("d: registry handoff permits closed-owner takeover but rejects an online owner", () => {
    const { harness } = setup();
    const service = new AgentOrchestratorService({
      agentEvents: harness.agentEvents,
      agents: harness.agents,
      scopes: harness.agentOrchestratorScopes,
    });
    service.claim({ ...scope, paneId: "wA:p1", terminalId: "term-1", ownerConnected: true });
    expect(() =>
      service.claim({ ...scope, paneId: "wA:p2", terminalId: "term-2", ownerConnected: true }),
    ).toThrow("ORCHESTRATOR_SCOPE_ALREADY_CLAIMED");
    expect(
      service.release({ ...scope, reason: "disconnected", terminalId: "term-1" }),
    ).toBeDefined();
    expect(
      service.claim({ ...scope, paneId: "wA:p2", terminalId: "term-2", ownerConnected: true })
        .current.owner?.terminalId,
    ).toBe("term-2");
    service.release({ ...scope, reason: "disconnected", terminalId: "term-2" });
    expect(
      service.claim({ ...scope, paneId: "wA:p3", terminalId: "term-3", ownerConnected: false })
        .current.owner?.terminalId,
    ).toBe("term-3");
  });

  test("e: concurrent online claims have exactly one winner and a clean final owner", async () => {
    const { harness } = setup();
    const service = new AgentOrchestratorService({
      agentEvents: harness.agentEvents,
      agents: harness.agents,
      scopes: harness.agentOrchestratorScopes,
    });
    const results = await Promise.allSettled(
      ["term-a", "term-b"].map((terminalId) =>
        Promise.resolve().then(() =>
          service.claim({ ...scope, paneId: `wA:${terminalId}`, terminalId, ownerConnected: true }),
        ),
      ),
    );
    expect(results.filter((result) => result.status === "fulfilled")).toHaveLength(1);
    expect(results.filter((result) => result.status === "rejected")).toHaveLength(1);
    expect(harness.agentOrchestratorScopes.get(scope)?.owner).toEqual(
      expect.objectContaining({ terminalId: expect.stringMatching(/^term-[ab]$/) }),
    );
  });

  test("f: stale pane close cannot release a newer generation, but matching close can", () => {
    const { harness } = setup();
    const service = new AgentOrchestratorService({
      agentEvents: harness.agentEvents,
      agents: harness.agents,
      scopes: harness.agentOrchestratorScopes,
    });
    service.claim({ ...scope, paneId: "wA:p1", terminalId: "term-1", ownerConnected: true });
    service.claim({ ...scope, paneId: "wA:p1", terminalId: "term-1", ownerConnected: true });
    expect(
      harness.agentOrchestratorScopes.releaseIfOwnerIdentity({
        ...scope,
        paneId: "wA:p1",
        terminalId: "term-old",
      }).changed,
    ).toBe(false);
    expect(harness.agentOrchestratorScopes.get(scope)?.owner).toEqual({
      paneId: "wA:p1",
      terminalId: "term-1",
    });
    expect(
      harness.agentOrchestratorScopes.releaseIfOwnerIdentity({
        ...scope,
        paneId: "wA:p1",
        terminalId: "term-1",
      }).changed,
    ).toBe(true);
    expect(harness.agentOrchestratorScopes.get(scope)?.owner).toBeNull();
  });
});
