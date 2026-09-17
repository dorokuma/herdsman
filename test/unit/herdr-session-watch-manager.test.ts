import { afterEach, describe, expect, test, vi } from "vitest";
import { emptyCompactHistory } from "@/agent-history/service.js";
import {
  ACTIVE_REVISION_POLL_MS,
  FULL_RESCAN_MS,
  HerdrSessionWatchManager,
  PLAN_DRAIN_GRACE_MS,
} from "@/daemon/herdr-session-watch-manager.js";
import type {
  AgentIndexRefreshResult,
  StatusEventPlan,
} from "@/observability/agent-index-service.js";
import { AgentIndexService } from "@/observability/agent-index-service.js";
import type { AgentIndexRecord } from "@/observability/contracts.js";
import {
  cleanupTempDirs,
  openObservabilityDbHarness,
} from "../integration/observability-db-harness.js";

afterEach(() => {
  vi.useRealTimers();
  cleanupTempDirs();
});

describe("HerdrSessionWatchManager", () => {
  test("uses exact exported scheduler constants", () => {
    expect(ACTIVE_REVISION_POLL_MS).toBe(10_000);
    expect(FULL_RESCAN_MS).toBe(60_000);
  });

  test("polls working sessions at the active cadence and performs one full rescan at the boundary", async () => {
    vi.useFakeTimers();
    const harness = openObservabilityDbHarness();
    seedAgent(harness, "working");
    let refreshes = 0;
    let sessions = 0;
    const manager = managerFor(harness, {
      activeRevisionPollMs: 10,
      fullRescanMs: 60,
      index: {
        async handleHerdrEvent() {
          return { contextChangedScopes: [], events: [] };
        },
        async refreshHerdrSession() {
          refreshes += 1;
          return result([agentRecord("wB:p2", "wB", "working")]);
        },
      },
      sessionList: async () => {
        sessions += 1;
        return [entry()];
      },
    });
    await manager.start();
    await vi.advanceTimersByTimeAsync(50);
    expect(refreshes).toBe(6);
    expect(sessions).toBe(1);
    await vi.advanceTimersByTimeAsync(10);
    expect(refreshes).toBe(7);
    expect(sessions).toBe(2);
    await manager.stop();
    harness.sqlite.close();
  });

  test("does not poll all-idle sessions before the full rescan", async () => {
    vi.useFakeTimers();
    const harness = openObservabilityDbHarness();
    seedAgent(harness, "idle");
    let refreshes = 0;
    const manager = managerFor(harness, {
      activeRevisionPollMs: 10,
      fullRescanMs: 60,
      index: {
        async handleHerdrEvent() {
          return { contextChangedScopes: [], events: [] };
        },
        async refreshHerdrSession() {
          refreshes += 1;
          return result([agentRecord("wB:p2", "wB", "idle")]);
        },
      },
    });
    await manager.start();
    await vi.advanceTimersByTimeAsync(50);
    expect(refreshes).toBe(1);
    await manager.stop();
    harness.sqlite.close();
  });

  test("awaits a removed watcher before finalizing the stopped session", async () => {
    const harness = openObservabilityDbHarness();
    let sessions = [entry()];
    let subscriptions = 0;
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents() {
          subscriptions += 1;
          yield* [];
        },
      }),
      index: {
        async handleHerdrEvent() {
          return { contextChangedScopes: [], events: [] };
        },
        async refreshHerdrSession() {
          await gate;
          harness.herdrSessions.upsertRunning(entry());
          return result([]);
        },
      },
      sessionList: async () => sessions,
    });
    await manager.start();
    sessions = [];
    let settled = false;
    const rescanning = manager.rescanNow().then(() => {
      settled = true;
    });
    await Promise.resolve();
    expect(settled).toBe(false);

    release();
    await rescanning;

    expect(harness.herdrSessions.get("default").running).toBe(false);
    expect(subscriptions).toBe(0);
    await manager.stop();
    harness.sqlite.close();
  });

  test("does not start a watcher after stop wins a full-rescan race", async () => {
    vi.useFakeTimers();
    const harness = openObservabilityDbHarness();
    let listCalls = 0;
    let resolveRescan!: (entries: ReturnType<typeof entry>[]) => void;
    const rescan = new Promise<ReturnType<typeof entry>[]>((resolve) => {
      resolveRescan = resolve;
    });
    let clients = 0;
    const manager = managerFor(harness, {
      activeRevisionPollMs: 10,
      clientFactory: () => {
        clients += 1;
        return {
          close() {},
          async *subscribeEvents(_params, options) {
            await new Promise<void>((resolve) =>
              options?.signal?.addEventListener("abort", () => resolve(), { once: true }),
            );
          },
        };
      },
      fullRescanMs: 60,
      index: {
        async handleHerdrEvent() {
          return { contextChangedScopes: [], events: [] };
        },
        async refreshHerdrSession() {
          return result([]);
        },
      },
      sessionList: async () => {
        listCalls += 1;
        return listCalls === 1 ? [entry()] : rescan;
      },
    });
    await manager.start();
    const ticking = vi.advanceTimersByTimeAsync(60);
    await vi.waitFor(() => expect(listCalls).toBe(2));

    const stopping = manager.stop();
    resolveRescan([entry()]);
    await Promise.all([ticking, stopping]);

    expect(clients).toBe(1);
    harness.sqlite.close();
  });

  test("publishes one shared refresh result once", async () => {
    vi.useFakeTimers();
    const harness = openObservabilityDbHarness();
    seedAgent(harness, "working");
    let resolveRefresh!: (value: ReturnType<typeof result>) => void;
    const shared = new Promise<AgentIndexRefreshResult>((resolve) => {
      resolveRefresh = resolve;
    });
    let refreshCalls = 0;
    const published = { context: 0, events: 0, reconcile: 0 };
    const manager = managerFor(harness, {
      activeRevisionPollMs: 10,
      fullRescanMs: 60,
      index: {
        async handleHerdrEvent() {
          return { contextChangedScopes: [], events: [] };
        },
        refreshHerdrSession() {
          refreshCalls += 1;
          return shared;
        },
      },
      onAgentContextChanged: () => {
        published.context += 1;
      },
      onAgentEvent: () => {
        published.events += 1;
      },
      onAgentIndexRefreshed: () => {
        published.reconcile += 1;
      },
    });
    await manager.start();
    expect(refreshCalls).toBe(1);
    vi.advanceTimersByTime(10);
    await Promise.resolve();
    expect(refreshCalls).toBe(2);
    resolveRefresh({
      agents: [agentRecord("wB:p2", "wB", "working")],
      contextChangedScopes: [{ herdrSessionName: "default", workspaceId: "wB" }],
      events: [event()],
    });
    await shared;
    for (let index = 0; index < 5; index += 1) await Promise.resolve();

    expect(published).toEqual({ context: 1, events: 1, reconcile: 1 });
    await manager.stop();
    harness.sqlite.close();
  });

  test("reconnects when Herdr event stream closes without a restart event", async () => {
    const harness = openObservabilityDbHarness();
    const received: unknown[] = [];
    let subscribeCalls = 0;
    let handled = 0;
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents() {
          subscribeCalls += 1;
          if (subscribeCalls === 1) return;
          yield { agent_status: "idle", pane_id: "wB:p2", type: "pane.agent_status_changed" };
          await new Promise((resolve) => setTimeout(resolve, 20));
        },
      }),
      index: {
        async handleHerdrEvent() {
          handled += 1;
          return { contextChangedScopes: [], events: [event()] };
        },
        async refreshHerdrSession() {
          return result([]);
        },
      },
      onAgentEvent: (item) => received.push(item),
      reconnectDelayMs: 0,
    });
    await manager.start();
    await waitFor(() => handled > 0);
    await manager.stop();
    harness.sqlite.close();
    expect(subscribeCalls).toBeGreaterThanOrEqual(2);
    expect(received).toContainEqual(expect.objectContaining({ type: "agent.idle" }));
  });

  test("publishes agent.failed returned by executeStatusEventPlan", async () => {
    const harness = openObservabilityDbHarness();
    seedAgent(harness, "working");
    const received: unknown[] = [];
    let handled = 0;
    const failedEvent = { ...event(), type: "agent.failed" as const, terminalId: "term_agent" };
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents() {
          yield { agent_status: "done", pane_id: "wB:p2", type: "pane.agent_status_changed" };
          await new Promise<void>((resolve) => setTimeout(resolve, 200));
        },
      }),
      index: {
        async handleHerdrEventFast() {
          handled += 1;
          return {
            agents: [],
            contextChangedScopes: [],
            events: [],
            statusEventPlans: [testPlan()],
          };
        },
        async refreshHerdrSessionFast() {
          return { agents: [], contextChangedScopes: [], events: [], statusEventPlans: [] };
        },
        executeStatusEventPlan: async () => failedEvent,
      },
      onAgentEvent: (item) => received.push(item),
    });
    await manager.start();
    await waitFor(() => handled > 0 && received.length > 0);
    await manager.stop();
    harness.sqlite.close();
    expect(received).toContainEqual(expect.objectContaining({ type: "agent.failed" }));
  });

  test("publishes agent.discarded returned by executeStatusEventPlan through the whitelist", async () => {
    const harness = openObservabilityDbHarness();
    seedAgent(harness, "working");
    const received: unknown[] = [];
    let handled = 0;
    const discardedEvent = {
      ...event(),
      type: "agent.discarded" as const,
      terminalId: "term_agent",
    };
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents() {
          yield { agent_status: "done", pane_id: "wB:p2", type: "pane.agent_status_changed" };
          await new Promise<void>((resolve) => setTimeout(resolve, 200));
        },
      }),
      index: {
        async handleHerdrEventFast() {
          handled += 1;
          return {
            agents: [],
            contextChangedScopes: [],
            events: [],
            statusEventPlans: [testPlan()],
          };
        },
        async refreshHerdrSessionFast() {
          return { agents: [], contextChangedScopes: [], events: [], statusEventPlans: [] };
        },
        executeStatusEventPlan: async () => discardedEvent,
      },
      onAgentEvent: (item) => received.push(item),
    });
    await manager.start();
    await waitFor(() => handled > 0 && received.length > 0);
    await manager.stop();
    harness.sqlite.close();
    expect(received).toContainEqual(expect.objectContaining({ type: "agent.discarded" }));
  });

  test("rejects a status plan without escaping the watch loop (warn + stop completes)", async () => {
    const harness = openObservabilityDbHarness();
    seedAgent(harness, "working");
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
    const received: unknown[] = [];
    let handled = 0;
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents() {
          yield { agent_status: "done", pane_id: "wB:p2", type: "pane.agent_status_changed" };
          await new Promise<void>((resolve) =>
            // keep the stream open until the watcher aborts
            setTimeout(resolve, 200),
          );
        },
      }),
      index: {
        async handleHerdrEventFast() {
          handled += 1;
          return {
            agents: [],
            contextChangedScopes: [],
            events: [],
            statusEventPlans: [testPlan()],
          };
        },
        async refreshHerdrSessionFast() {
          return { agents: [], contextChangedScopes: [], events: [], statusEventPlans: [] };
        },
        executeStatusEventPlan: () => Promise.reject(new Error("plan boom")),
      },
      onAgentEvent: (event) => received.push(event),
    });
    await manager.start();
    await waitFor(() => handled > 0);
    // The rejection must be contained inside the watch loop: stop() completes
    // and no event escapes to the publisher.
    await manager.stop();
    expect(received).toEqual([]);
    expect(warnSpy).toHaveBeenCalledWith(
      "Herdsman status event plan failed",
      expect.objectContaining({
        agentId: "ag_1",
        error: expect.any(Error),
        from: "working",
        to: "done",
      }),
    );
    warnSpy.mockRestore();
    harness.sqlite.close();
  });

  test("stop() waits for an in-flight plan and settles once it completes", async () => {
    vi.useFakeTimers();
    const harness = openObservabilityDbHarness();
    seedAgent(harness, "working");
    let releasePlan!: () => void;
    const gate = new Promise<void>((resolve) => {
      releasePlan = resolve;
    });
    let planStarted = false;
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents(_params, options) {
          yield { agent_status: "done", pane_id: "wB:p2", type: "pane.agent_status_changed" };
          if (options?.signal?.aborted) return;
          await new Promise<void>((resolve) =>
            options?.signal?.addEventListener("abort", () => resolve(), { once: true }),
          );
        },
      }),
      index: {
        async handleHerdrEventFast() {
          return {
            agents: [],
            contextChangedScopes: [],
            events: [],
            statusEventPlans: [testPlan()],
          };
        },
        async refreshHerdrSessionFast() {
          return { agents: [], contextChangedScopes: [], events: [], statusEventPlans: [] };
        },
        executeStatusEventPlan: () => {
          planStarted = true;
          return gate.then(() => undefined);
        },
      },
    });
    await manager.start();
    for (let index = 0; index < 50 && !planStarted; index += 1) await Promise.resolve();
    expect(planStarted).toBe(true);

    let settled = false;
    const stopping = manager.stop().then(() => {
      settled = true;
    });
    await Promise.resolve();
    expect(settled).toBe(false);
    releasePlan();
    await stopping;
    expect(settled).toBe(true);
    harness.sqlite.close();
  });

  test("stop() settles after PLAN_DRAIN_GRACE_MS even when a plan never finishes", async () => {
    vi.useFakeTimers();
    const harness = openObservabilityDbHarness();
    seedAgent(harness, "working");
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents(_params, options) {
          yield { agent_status: "done", pane_id: "wB:p2", type: "pane.agent_status_changed" };
          if (options?.signal?.aborted) return;
          await new Promise<void>((resolve) =>
            options?.signal?.addEventListener("abort", () => resolve(), { once: true }),
          );
        },
      }),
      index: {
        async handleHerdrEventFast() {
          return {
            agents: [],
            contextChangedScopes: [],
            events: [],
            statusEventPlans: [testPlan()],
          };
        },
        async refreshHerdrSessionFast() {
          return { agents: [], contextChangedScopes: [], events: [], statusEventPlans: [] };
        },
        executeStatusEventPlan: () => new Promise<undefined>(() => {}),
      },
    });
    await manager.start();
    const stopping = manager.stop();
    await vi.advanceTimersByTimeAsync(PLAN_DRAIN_GRACE_MS + 1_000);
    await stopping;
    harness.sqlite.close();
  });

  test("serializes status plans per agent so a repeated transition appends a single done event", async () => {
    const harness = openObservabilityDbHarness();
    seedAgent(harness, "working");
    const index = new AgentIndexService({
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("claude-jsonl"),
              lastAssistantMessage: { ref: "history", text: "final answer", timestamp: null },
            },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as never,
      stores: harness,
    });
    const agent = harness.agents.listForHerdrSession("default")[0];
    if (!agent) throw new Error("expected indexed agent");
    const plan = testPlan(agent, {
      ...emptyCompactHistory("claude-jsonl"),
      lastAssistantMessage: { ref: "history", text: "final answer", timestamp: null },
    });
    // Two plans for the same agent (event + refresh paths can both produce
    // one). The per-agent queue must serialize them: the second execution sees
    // the first one's appended transition and deduplicates instead of
    // appending a second done.
    await Promise.all([
      index.executeStatusEventPlan(plan),
      index.executeStatusEventPlan({ ...plan }),
    ]);
    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wB",
    });
    expect(events.filter((event) => event.type === "agent.done")).toHaveLength(1);
    expect(events.filter((event) => event.type === "agent.status.changed")).toHaveLength(1);
    const rows = harness.sqlite
      .prepare("select id, status from status_event_plans order by id")
      .all();
    expect(rows).toHaveLength(2);
    expect(rows.every((row) => (row as { status: string }).status === "completed")).toBe(true);
    expect(harness.statusEventPlans.listUnfinished()).toEqual([]);
    harness.sqlite.close();
  });

  test("reconciles before publishing changed context and events after a pane move", async () => {
    const harness = openObservabilityDbHarness();
    const operations: string[] = [];
    let refreshCalls = 0;
    let subscribeCalls = 0;
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents(_params, options) {
          subscribeCalls += 1;
          if (subscribeCalls === 1) {
            yield { type: "pane.moved" };
            return;
          }
          await new Promise<void>((resolve) => {
            options?.signal?.addEventListener("abort", () => resolve(), { once: true });
          });
        },
      }),
      index: {
        async handleHerdrEvent() {
          return { contextChangedScopes: [], events: [] };
        },
        async refreshHerdrSession() {
          refreshCalls += 1;
          const agent = agentRecord(
            refreshCalls === 1 ? "wA:p1" : "wB:p3",
            refreshCalls === 1 ? "wA" : "wB",
            "working",
          );
          return {
            agents: [agent],
            contextChangedScopes: [{ herdrSessionName: "default", workspaceId: agent.workspaceId }],
            events: [event(agent.workspaceId)],
          };
        },
      },
      onAgentContextChanged: (scope) => operations.push(`context:${scope.workspaceId}`),
      onAgentEvent: (item) => operations.push(`event:${item.workspaceId}`),
      onAgentIndexRefreshed: ({ agents }) => operations.push(`reconcile:${agents[0]?.paneId}`),
      reconnectDelayMs: 0,
    });
    await manager.start();
    await waitFor(() => subscribeCalls === 2);
    await manager.stop();
    harness.sqlite.close();
    expect(operations).toEqual([
      "reconcile:wA:p1",
      "context:wA",
      "event:wA",
      "reconcile:wB:p3",
      "context:wB",
      "event:wB",
    ]);
  });

  for (const topologyType of ["pane.created", "pane.agent_detected"] as const) {
    test(`restarts subscription after ${topologyType} so the new pane is included`, async () => {
      const harness = openObservabilityDbHarness();
      const subscribed: string[][] = [];
      let refreshCalls = 0;
      const manager = managerFor(harness, {
        clientFactory: () => ({
          close() {},
          async *subscribeEvents(params, options) {
            subscribed.push([...(params?.paneIds ?? [])]);
            if (subscribed.length === 1) {
              yield { pane_id: "wB:p9", type: topologyType };
            }
            if (options?.signal?.aborted) return;
            await new Promise<void>((resolve) => {
              options?.signal?.addEventListener("abort", () => resolve(), { once: true });
            });
          },
        }),
        index: {
          async handleHerdrEvent() {
            return { contextChangedScopes: [], events: [] };
          },
          async refreshHerdrSession() {
            refreshCalls += 1;
            if (refreshCalls === 1) return result([]);
            return result([agentRecord("wB:p9", "wB", "working")]);
          },
        },
        reconnectDelayMs: 0,
      });
      await manager.start();
      await waitFor(() => subscribed.length === 2);
      await manager.stop();
      harness.sqlite.close();
      expect(subscribed[0]).toEqual([]);
      expect(subscribed[1]).toEqual(["wB:p9"]);
    });
  }

  test("restarts the live subscription when a refresh discovers a new pane", async () => {
    vi.useFakeTimers();
    const harness = openObservabilityDbHarness();
    seedAgent(harness, "working");
    const subscribed: string[][] = [];
    let refreshCalls = 0;
    const manager = managerFor(harness, {
      activeRevisionPollMs: 10,
      fullRescanMs: 60_000,
      clientFactory: () => ({
        close() {},
        async *subscribeEvents(params, options) {
          subscribed.push([...(params?.paneIds ?? [])]);
          if (options?.signal?.aborted) return;
          await new Promise<void>((resolve) => {
            options?.signal?.addEventListener("abort", () => resolve(), { once: true });
          });
        },
      }),
      index: {
        async handleHerdrEvent() {
          return { contextChangedScopes: [], events: [] };
        },
        async refreshHerdrSession() {
          refreshCalls += 1;
          if (refreshCalls === 1) {
            return result([agentRecord("wB:p2", "wB", "working")]);
          }
          return result([
            agentRecord("wB:p2", "wB", "working"),
            { ...agentRecord("wB:p3", "wB", "working"), id: "ag_2", paneId: "wB:p3" },
          ]);
        },
      },
    });
    await manager.start();
    for (let index = 0; index < 50 && subscribed.length === 0; index += 1) await Promise.resolve();
    expect(subscribed).toEqual([["wB:p2"]]);
    await vi.advanceTimersByTimeAsync(10);
    for (let index = 0; index < 50 && subscribed.length < 2; index += 1) await Promise.resolve();
    expect(subscribed[1]).toEqual(["wB:p2", "wB:p3"]);
    await manager.stop();
    harness.sqlite.close();
  });

  test("second connection idle after created and working refresh delivers agent.idle", async () => {
    const harness = openObservabilityDbHarness();
    const subscribed: string[][] = [];
    const handled: { event: unknown; stream: number }[] = [];
    const received: unknown[] = [];
    let refreshCalls = 0;
    const idleEvent = { ...event(), type: "agent.idle" as const, paneId: "wB:p9" };
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents(params, options) {
          const paneIds = [...(params?.paneIds ?? [])];
          subscribed.push(paneIds);
          const stream = subscribed.length;
          // Stream 1 (paneIds=[]) is topology-only. Even if working/idle are
          // offered here, pane-specific filtering must drop them.
          const offered =
            stream === 1
              ? [
                  { pane_id: "wB:p9", type: "pane.created" },
                  {
                    agent_status: "working",
                    pane_id: "wB:p9",
                    type: "pane.agent_status_changed",
                  },
                  {
                    agent_status: "idle",
                    pane_id: "wB:p9",
                    type: "pane.agent_status_changed",
                  },
                ]
              : [
                  {
                    agent_status: "idle",
                    pane_id: "wB:p9",
                    type: "pane.agent_status_changed",
                  },
                ];
          for (const event of herdrEventsForSubscription(paneIds, offered)) {
            yield taggedWatchEvent(event, stream);
          }
          if (options?.signal?.aborted) return;
          await new Promise<void>((resolve) => {
            options?.signal?.addEventListener("abort", () => resolve(), { once: true });
          });
        },
      }),
      index: {
        async handleHerdrEventFast(input?: { event: unknown }) {
          const record = (input?.event ?? {}) as {
            agent_status?: string;
            stream?: number;
          };
          handled.push({ event: input?.event, stream: record.stream ?? 0 });
          if (record.agent_status !== "idle") {
            return { agents: [], contextChangedScopes: [], events: [], statusEventPlans: [] };
          }
          return {
            agents: [],
            contextChangedScopes: [],
            events: [],
            statusEventPlans: [testPlan(agentRecord("wB:p9", "wB", "working"), undefined)],
          };
        },
        async refreshHerdrSessionFast() {
          refreshCalls += 1;
          if (refreshCalls === 1) {
            return { agents: [], contextChangedScopes: [], events: [], statusEventPlans: [] };
          }
          return {
            agents: [agentRecord("wB:p9", "wB", "working")],
            contextChangedScopes: [],
            events: [],
            statusEventPlans: [],
          };
        },
        executeStatusEventPlan: async () => idleEvent,
      },
      onAgentEvent: (item) => received.push(item),
      reconnectDelayMs: 0,
    });
    await manager.start();
    await waitFor(() => received.length > 0 && subscribed.length >= 2);
    await manager.stop();
    harness.sqlite.close();
    expect(subscribed[0]).toEqual([]);
    expect(subscribed[1]).toEqual(["wB:p9"]);
    expect(
      handled.filter(
        (item) => (item.event as { type?: string }).type === "pane.agent_status_changed",
      ),
    ).toEqual([
      expect.objectContaining({
        stream: 2,
        event: expect.objectContaining({ agent_status: "idle", pane_id: "wB:p9" }),
      }),
    ]);
    expect(received).toContainEqual(expect.objectContaining({ type: "agent.idle" }));
  });

  test("does not invent working-to-idle when first refresh of a created pane is already idle", async () => {
    const harness = openObservabilityDbHarness();
    const subscribed: string[][] = [];
    const received: unknown[] = [];
    let planCalls = 0;
    let refreshCalls = 0;
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents(params, options) {
          const paneIds = [...(params?.paneIds ?? [])];
          subscribed.push(paneIds);
          // Stream 1: topology only. Stream 2 may include the new pane_id, but
          // herdr does not replay status for a pane that is already idle.
          for (const event of herdrEventsForSubscription(
            paneIds,
            subscribed.length === 1 ? [{ pane_id: "wB:p9", type: "pane.created" }] : [],
          )) {
            yield event;
          }
          if (options?.signal?.aborted) return;
          await new Promise<void>((resolve) => {
            options?.signal?.addEventListener("abort", () => resolve(), { once: true });
          });
        },
      }),
      index: {
        async handleHerdrEventFast() {
          return { agents: [], contextChangedScopes: [], events: [], statusEventPlans: [] };
        },
        async refreshHerdrSessionFast() {
          refreshCalls += 1;
          if (refreshCalls === 1) {
            return { agents: [], contextChangedScopes: [], events: [], statusEventPlans: [] };
          }
          return {
            agents: [agentRecord("wB:p9", "wB", "idle")],
            contextChangedScopes: [],
            events: [],
            statusEventPlans: [],
          };
        },
        executeStatusEventPlan: async () => {
          planCalls += 1;
          return { ...event(), type: "agent.idle" as const };
        },
      },
      onAgentEvent: (item) => received.push(item),
      reconnectDelayMs: 0,
    });
    await manager.start();
    await waitFor(() => subscribed.length >= 2);
    await new Promise((resolve) => setTimeout(resolve, 40));
    await manager.stop();
    harness.sqlite.close();
    expect(subscribed[0]).toEqual([]);
    expect(subscribed[1]).toEqual(["wB:p9"]);
    expect(planCalls).toBe(0);
    expect(received).toEqual([]);
  });

  test("does not resubscribe in a loop when the next connection replays historical topology", async () => {
    const harness = openObservabilityDbHarness();
    const subscribed: string[][] = [];
    let refreshCalls = 0;
    const historical = [
      { pane_id: "wB:p9", type: "pane.created" },
      { pane_id: "wB:p9", type: "pane.moved" },
    ];
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents(params, options) {
          const paneIds = [...(params?.paneIds ?? [])];
          subscribed.push(paneIds);
          for (const event of herdrEventsForSubscription(paneIds, historical)) {
            if (options?.signal?.aborted) return;
            yield event;
          }
          if (options?.signal?.aborted) return;
          await new Promise<void>((resolve) => {
            options?.signal?.addEventListener("abort", () => resolve(), { once: true });
          });
        },
      }),
      index: {
        async handleHerdrEvent() {
          return { contextChangedScopes: [], events: [] };
        },
        async refreshHerdrSession() {
          refreshCalls += 1;
          if (refreshCalls === 1) return result([]);
          return result([agentRecord("wB:p9", "wB", "working")]);
        },
      },
      reconnectDelayMs: 0,
    });
    await manager.start();
    await waitFor(() => subscribed.length >= 2);
    await new Promise((resolve) => setTimeout(resolve, 40));
    await manager.stop();
    harness.sqlite.close();
    expect(subscribed[0]).toEqual([]);
    expect(subscribed[1]).toEqual(["wB:p9"]);
    expect(subscribed).toHaveLength(2);
  });

  test("does not restart after pane.moved when refresh has no new pane", async () => {
    const harness = openObservabilityDbHarness();
    const subscribed: string[][] = [];
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents(params, options) {
          subscribed.push([...(params?.paneIds ?? [])]);
          if (subscribed.length === 1) {
            yield { pane_id: "wB:p2", type: "pane.moved" };
            yield { pane_id: "wB:p2", type: "pane.created" };
          }
          if (options?.signal?.aborted) return;
          await new Promise<void>((resolve) => {
            options?.signal?.addEventListener("abort", () => resolve(), { once: true });
          });
        },
      }),
      index: {
        async handleHerdrEvent() {
          return { contextChangedScopes: [], events: [] };
        },
        async refreshHerdrSession() {
          return result([agentRecord("wB:p2", "wB", "working")]);
        },
      },
      reconnectDelayMs: 0,
    });
    await manager.start();
    await waitFor(() => subscribed.length === 1);
    await new Promise((resolve) => setTimeout(resolve, 40));
    await manager.stop();
    harness.sqlite.close();
    expect(subscribed).toEqual([["wB:p2"]]);
  });

  test("ignores a generation-less pane.closed while the pane is still live", async () => {
    const harness = openObservabilityDbHarness();
    const handled: unknown[] = [];
    const subscribed: string[][] = [];
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents(params, options) {
          subscribed.push([...(params?.paneIds ?? [])]);
          if (subscribed.length === 1) {
            yield { pane_id: "wB:p2", type: "pane.closed" };
          }
          if (options?.signal?.aborted) return;
          await new Promise<void>((resolve) => {
            options?.signal?.addEventListener("abort", () => resolve(), { once: true });
          });
        },
      }),
      index: {
        async handleHerdrEvent(input?: { event: unknown }) {
          handled.push(input?.event);
          return { contextChangedScopes: [], events: [] };
        },
        async refreshHerdrSession() {
          return result([agentRecord("wB:p2", "wB", "working")]);
        },
      },
      reconnectDelayMs: 0,
    });
    await manager.start();
    await waitFor(() => subscribed.length === 1);
    await new Promise((resolve) => setTimeout(resolve, 40));
    await manager.stop();
    harness.sqlite.close();
    expect(handled).toEqual([]);
    expect(subscribed).toEqual([["wB:p2"]]);
  });

  test("applies pane.closed after refresh shows the pane is gone", async () => {
    const harness = openObservabilityDbHarness();
    const handled: unknown[] = [];
    let refreshCalls = 0;
    const manager = managerFor(harness, {
      clientFactory: () => ({
        close() {},
        async *subscribeEvents(_params, options) {
          yield { pane_id: "wB:p2", type: "pane.closed" };
          if (options?.signal?.aborted) return;
          await new Promise<void>((resolve) => {
            options?.signal?.addEventListener("abort", () => resolve(), { once: true });
          });
        },
      }),
      index: {
        async handleHerdrEvent(input?: { event: unknown }) {
          handled.push(input?.event);
          return { contextChangedScopes: [], events: [] };
        },
        async refreshHerdrSession() {
          refreshCalls += 1;
          if (refreshCalls === 1) return result([agentRecord("wB:p2", "wB", "working")]);
          return result([]);
        },
      },
      reconnectDelayMs: 0,
    });
    await manager.start();
    await waitFor(() => handled.length > 0);
    await manager.stop();
    harness.sqlite.close();
    expect(handled).toContainEqual(
      expect.objectContaining({ pane_id: "wB:p2", type: "pane.closed" }),
    );
  });
});

/** Herdr only emits pane.agent_status_changed for pane-specific subscriptions. */
function herdrEventsForSubscription(
  paneIds: readonly string[],
  events: Array<Record<string, unknown>>,
): Array<Record<string, unknown>> {
  return events.filter((event) => {
    if (event.type !== "pane.agent_status_changed") return true;
    return typeof event.pane_id === "string" && paneIds.includes(event.pane_id);
  });
}

function taggedWatchEvent(event: Record<string, unknown>, stream: number): Record<string, unknown> {
  return { ...event, stream };
}

function managerFor(
  harness: ReturnType<typeof openObservabilityDbHarness>,
  overrides: Partial<Omit<ConstructorParameters<typeof HerdrSessionWatchManager>[0], "index">> & {
    index: {
      executeStatusEventPlan?: () => Promise<unknown>;
      handleHerdrEvent?: () => Promise<unknown>;
      handleHerdrEventFast?: () => Promise<unknown>;
      refreshHerdrSession?: () => Promise<unknown>;
      refreshHerdrSessionFast?: () => Promise<unknown>;
    };
  },
) {
  const { index, ...options } = overrides;
  return new HerdrSessionWatchManager({
    activeRevisionPollMs: 60_000,
    agents: harness.agents,
    clientFactory: () => ({
      close() {},
      async *subscribeEvents(_params, options) {
        if (options?.signal?.aborted) return;
        await new Promise<void>((resolve) =>
          options?.signal?.addEventListener("abort", () => resolve(), { once: true }),
        );
      },
    }),
    fullRescanMs: 60_000,
    herdrSessions: harness.herdrSessions,
    index: index as unknown as ConstructorParameters<typeof HerdrSessionWatchManager>[0]["index"],
    onAgentContextChanged() {},
    onAgentEvent() {},
    onAgentIndexRefreshed() {},
    reconnectDelayMs: 0,
    sessionList: async () => [entry()],
    ...options,
  });
}

function entry() {
  return {
    name: "default",
    running: true,
    sessionDir: "/tmp/herdr",
    socketPath: "/tmp/herdr.sock",
  };
}

function seedAgent(
  harness: ReturnType<typeof openObservabilityDbHarness>,
  status: "idle" | "working",
) {
  harness.herdrSessions.upsertRunning(entry());
  harness.agents.replaceForSession({
    agents: [
      {
        agent: "claude",
        agent_status: status,
        pane_id: "wB:p2",
        terminal_id: "term_1",
        workspace_id: "wB",
      },
    ],
    herdrSessionName: "default",
  });
}

function result(agents: AgentIndexRecord[]): AgentIndexRefreshResult {
  return { agents, contextChangedScopes: [], events: [] };
}

function event(workspaceId = "wB") {
  return {
    agentId: null,
    compactHistory: null,
    createdAt: new Date(),
    deliverable: 0 as const,
    herdrSessionName: "default",
    id: 1,
    paneId: "wB:p2",
    payload: {},
    terminalId: null,
    type: "agent.idle" as const,
    workspaceId,
  };
}

function agentRecord(
  paneId: string,
  workspaceId: string,
  agentStatus: "idle" | "working",
): AgentIndexRecord {
  return {
    agent: "pi",
    agentSession: null,
    agentStatus,
    cwd: null,
    firstSeenAt: new Date(0),
    focused: false,
    foregroundCwd: null,
    herdrSessionName: "default",
    id: "ag_1",
    lastSeenAt: new Date(0),
    name: null,
    paneId,
    paneRevision: null,
    tabId: null,
    terminalId: "term_1",
    workspaceId,
  };
}

function testPlan(
  agent: AgentIndexRecord = agentRecord("wB:p2", "wB", "working"),
  compactHistory: StatusEventPlan["compactHistory"] = undefined,
): StatusEventPlan {
  return {
    agent,
    compactHistory,
    from: "working",
    to: "done",
  };
}

async function waitFor(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 1_000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  throw new Error("condition was not met");
}
