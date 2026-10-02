import { afterEach, describe, expect, test, vi } from "vitest";
import type { AgentHistoryService } from "@/agent-history/service.js";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { HerdrSessionWatchManager } from "@/daemon/herdr-session-watch-manager.js";
import { AgentIndexService } from "@/observability/agent-index-service.js";
import type { AgentEventRecord, CompactAgentHistory } from "@/observability/contracts.js";
import { TurnCompletionRegistry } from "@/observability/turn-completion.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

afterEach(cleanupTempDirs);

function sessionInput() {
  return { herdrSessionName: "default", sessionDir: "/tmp/herdr", socketPath: "/tmp/herdr.sock" };
}

function piAgentSnapshot(status: string, agent = "pi") {
  return {
    snapshot: {
      agents: [
        {
          agent,
          agent_status: status,
          cwd: "/repo",
          pane_id: "wJ:p2",
          revision: 10,
          terminal_id: "term_claude",
          workspace_id: "wJ",
        },
      ],
      panes: [{ pane_id: "wJ:p2", revision: 10 }],
      tabs: [],
      workspaces: [{ agent_status: status, focused: true, label: "repo", workspace_id: "wJ" }],
    },
  };
}

const doneEvent = {
  event: { agent_status: "done", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
  ...sessionInput(),
};

describe("agent.done / agent.blocked turn completion signal timing", () => {
  test("waits for the pi turn signal before emitting agent.done so lastAssistantMessage is non-empty", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    let calls = 0;
    let signalRecorded = false;
    let releaseFirstRefreshStarted!: () => void;
    let firstRefreshStarted!: Promise<void>;
    const armGate = () => {
      firstRefreshStarted = new Promise<void>((resolve) => {
        releaseFirstRefreshStarted = resolve;
      });
    };
    armGate();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          calls += 1;
          // The status-observation refresh runs before the final message is on
          // disk, so it always sees an empty history (the race under test).
          if (calls === 1) {
            releaseFirstRefreshStarted();
            return {
              compactHistory: { ...emptyCompactHistory("pi-jsonl"), lastAssistantMessage: null },
              historyRef: null,
              sourceFingerprint: null,
            };
          }
          return signalRecorded
            ? {
                compactHistory: {
                  ...emptyCompactHistory("pi-jsonl"),
                  lastAssistantMessage: {
                    ref: "history",
                    text: "final answer",
                    timestamp: null,
                    stopReason: "stop",
                  },
                },
                historyRef: null,
                sourceFingerprint: null,
              }
            : {
                compactHistory: { ...emptyCompactHistory("pi-jsonl"), lastAssistantMessage: null },
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
    // Re-arm the gate so the initial refresh does not count as the
    // status-observation refresh for the turn under test.
    armGate();

    // The daemon observes the status flip to "done" while the final assistant
    // message is not yet on disk (first refresh returns an empty history).
    const pending = index.handleHerdrEvent(doneEvent);
    await firstRefreshStarted;
    expect(calls).toBe(1);

    // Then the extension writes its final message and signals the daemon after
    // the turn-completion waiter has been installed.
    signalRecorded = true;
    setTimeout(() => {
      registry.record({
        confirmed: true,
        herdrSessionName: "default",
        paneId: "wJ:p2",
        terminalId: "term_claude",
        workspaceId: "wJ",
      });
    }, 10);

    const result = await pending;
    // The daemon re-refreshed after the signal (second history resolution) and
    // the done event carries the freshly written assistant message.
    expect(calls).toBeGreaterThanOrEqual(2);
    expect(result.events).toContainEqual(
      expect.objectContaining({
        compactHistory: expect.objectContaining({
          lastAssistantMessage: expect.objectContaining({ text: "final answer" }),
        }),
        type: "agent.done",
      }),
    );
    harness.sqlite.close();
  }, 10_000);

  test("retries for agy without waiting for a turn signal", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    let calls = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working", "agy");
        },
      }),
      history: {
        async resolveCompactHistory() {
          calls += 1;
          return {
            compactHistory:
              calls < 3
                ? { ...emptyCompactHistory("antigravity-sqlite"), lastAssistantMessage: null }
                : {
                    ...emptyCompactHistory("antigravity-sqlite"),
                    lastAssistantMessage: {
                      ref: "history",
                      text: "agy final answer",
                      timestamp: null,
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
    // Non-terminal agent status ('agy') means terminal check fails immediately;
    // drain emits done event without waiting for turn signal.
    expect(calls).toBe(3);
    expect(result.events).toContainEqual(
      expect.objectContaining({
        compactHistory: expect.objectContaining({
          lastAssistantMessage: expect.objectContaining({ text: "agy final answer" }),
        }),
        type: "agent.done",
      }),
    );
    harness.sqlite.close();
  }, 10_000);

  test("emits agent.done promptly when the turn signal was recorded first", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 50 });
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "history",
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
      stores: harness,
      turnCompletions: registry,
    });

    await index.refreshHerdrSession(sessionInput());
    registry.record({
      confirmed: true,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    const startedAt = Date.now();
    const result = await index.handleHerdrEvent(doneEvent);
    expect(Date.now() - startedAt).toBeLessThan(50 + 100);
    expect(result.events).toContainEqual(
      expect.objectContaining({
        compactHistory: expect.objectContaining({
          lastAssistantMessage: expect.objectContaining({ text: "final answer" }),
        }),
        type: "agent.done",
      }),
    );
    harness.sqlite.close();
  }, 10_000);
  test("delivers a confirmed empty-history turn without invalidating it as degraded", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    let _calls = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          _calls += 1;
          return {
            compactHistory: { ...emptyCompactHistory("pi-jsonl"), lastAssistantMessage: null },
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
    _calls = 0;
    const pending = index.handleHerdrEvent(doneEvent);
    setTimeout(() => {
      registry.record({
        confirmed: true,
        herdrSessionName: "default",
        paneId: "wJ:p2",
        terminalId: "term_claude",
        workspaceId: "wJ",
      });
    }, 10);

    const _result = await pending;
    // A client-confirmed turn is a deliverable terminal state: it stays
    // deliverable even when the refreshed snapshot still carries no assistant
    // excerpt, and it is never invalidated as a degraded retry.
    expect(_result.events.filter((e) => e.type === "agent.done")).toHaveLength(1);
    const listAfterEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const deliveredDones = listAfterEvents.filter((e) => e.type === "agent.done");
    expect(deliveredDones).toHaveLength(1);
    expect(deliveredDones[0]).toMatchObject({ deliverable: 1, status: "pending" });
    expect(deliveredDones[0]?.payload).not.toEqual(expect.objectContaining({ degraded: true }));
    const invalidatedRows = harness.sqlite
      .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
      .all();
    expect(invalidatedRows).toHaveLength(0);
    harness.sqlite.close();
  }, 20_000);
  test("generates agent.done as-is with a warning when no turn signal arrives (old extension)", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 20 });
    let _calls = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          _calls += 1;
          return {
            compactHistory: { ...emptyCompactHistory("pi-jsonl"), lastAssistantMessage: null },
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
    _calls = 0;
    const _result = await index.handleHerdrEvent(doneEvent);
    // Guard blocks emission (empty assistant history); verify degraded record in DB.
    expect(_result.events.filter((e) => e.type === "agent.done")).toHaveLength(0);
    const listAfterEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(listAfterEvents.filter((e) => e.type === "agent.done")).toHaveLength(0);
    const invalidatedRows = harness.sqlite
      .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
      .all();
    expect(invalidatedRows.length).toBeGreaterThanOrEqual(1);
    harness.sqlite.close();
  }, 20_000);

  test("does not wait for a turn signal when no registry is configured", async () => {
    const harness = openObservabilityDbHarness();
    let calls = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          calls += 1;
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
    calls = 0;
    const result = await index.handleHerdrEvent(doneEvent);
    expect(calls).toBe(1);
    expect(result.events).toContainEqual(expect.objectContaining({ type: "agent.done" }));
    harness.sqlite.close();
  });

  test("aborts turn completion wait immediately on pane.closed and suppresses agent.done", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 5_000 });
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: { ...emptyCompactHistory("pi-jsonl"), lastAssistantMessage: null },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
      turnCompletions: registry,
    });

    await index.refreshHerdrSession(sessionInput());

    const donePromise = index.handleHerdrEvent(doneEvent);

    // Concurrently close the pane before the turn completion signal arrives
    const closeResult = await index.handleHerdrEvent({
      event: { pane_id: "wJ:p2", type: "pane.closed" },
      ...sessionInput(),
    });
    expect(closeResult.contextChangedScopes).toEqual([
      { herdrSessionName: "default", workspaceId: "wJ" },
    ]);

    const doneResult = await donePromise;
    expect(doneResult.events).toEqual([]);

    // Check agent events in database: no agent.done was written
    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(events.filter((e) => e.type === "agent.done")).toHaveLength(0);

    harness.sqlite.close();
  }, 10_000);

  test("watch loop delivers status_changed(done) followed by pane.closed in single stream, aborting done event promptly", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 5_000 });
    let closed = false;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return closed
            ? { snapshot: { agents: [], panes: [], tabs: [], workspaces: [] } }
            : piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: { ...emptyCompactHistory("pi-jsonl"), lastAssistantMessage: null },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
      turnCompletions: registry,
    });

    const emittedEvents: AgentEventRecord[] = [];
    let closeProcessed = false;

    async function* eventStream(_signal?: AbortSignal) {
      if (closed) return;
      yield { agent_status: "done", pane_id: "wJ:p2", type: "pane.agent_status_changed" };
      await new Promise((resolve) => setTimeout(resolve, 20));
      closed = true;
      closeProcessed = true;
      yield { pane_id: "wJ:p2", type: "pane.closed" };
    }

    const manager = new HerdrSessionWatchManager({
      agents: harness.agents,
      clientFactory: () => ({
        close() {},
        subscribeEvents: (_input, options) => eventStream(options?.signal),
      }),
      herdrSessions: harness.herdrSessions,
      index,
      onAgentEvent: (event) => {
        emittedEvents.push(event);
      },
      sessionList: async () => [
        { name: "default", running: true, sessionDir: "/tmp/herdr", socketPath: "/tmp/herdr.sock" },
      ],
    });

    await manager.start();

    const start = Date.now();
    while (!closeProcessed && Date.now() - start < 1000) {
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    expect(closeProcessed).toBe(true);

    await new Promise((resolve) => setTimeout(resolve, 100));

    await manager.stop();

    expect(emittedEvents.filter((e) => e.type === "agent.done")).toHaveLength(0);
    const dbEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(dbEvents.filter((e) => e.type === "agent.done")).toHaveLength(0);

    harness.sqlite.close();
  }, 10_000);

  test("emits terminal event when status flips during wait (done -> working)", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 5_000 });
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: { ...emptyCompactHistory("pi-jsonl"), lastAssistantMessage: null },
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

    const fastResult = await index.handleHerdrEventFast(doneEvent);
    expect(fastResult.statusEventPlans).toHaveLength(1);
    const plan = fastResult.statusEventPlans[0];
    if (!plan) throw new Error("expected statusEventPlan");

    const planPromise = index.executeStatusEventPlan(plan);

    // Status flips back to working in store. Fast now persists that plan in the
    // same transaction as the status write (W1); execute it after the done plan
    // so listUnfinished drains the way handleHerdrEvent would.
    const flip = await index.handleHerdrEventFast({
      event: { agent_status: "working", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });

    // Send turn signal
    registry.record({
      confirmed: true,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    const planResult = await planPromise;
    // A confirmed turn is delivered even with an empty assistant snapshot: the
    // plan completes instead of entering the degraded retry.
    expect(planResult?.type).toBe("agent.done");
    expect(planResult?.payload).not.toEqual(expect.objectContaining({ degraded: true }));
    await Promise.all(flip.statusEventPlans.map((next) => index.executeStatusEventPlan(next)));

    const listAfterEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(listAfterEvents.filter((e) => e.type === "agent.done")).toHaveLength(1);
    // A confirmed turn is never invalidated as degraded; verify via direct SQL.
    const agentForDone = harness.agents.findByPane({
      herdrSessionName: "default",
      paneId: "wJ:p2",
    });
    if (!agentForDone) throw new Error("expected agent");
    const invalidatedRows = harness.sqlite
      .prepare(
        "select * from agent_events where agent_id = ? and herdr_session_name = ? and status = 'invalidated' and type = 'agent.done'",
      )
      .all(agentForDone.id, "default");
    expect(invalidatedRows).toHaveLength(0);
    // The delivered row keeps its terminal payload and stays deliverable.
    const doneRows = harness.sqlite
      .prepare(
        "select * from agent_events where agent_id = ? and herdr_session_name = ? and type = 'agent.done'",
      )
      .all(agentForDone.id, "default") as Array<Record<string, unknown>>;
    expect(doneRows).toHaveLength(1);
    expect(doneRows[0]).toMatchObject({ deliverable: 1, status: "pending" });
    expect(JSON.parse(String(doneRows[0]?.payload_json))).toEqual(
      expect.objectContaining({ from: "working", to: "done" }),
    );
    // Both plans settle: the confirmed done plan completes and the flipped
    // working plan does not linger in a degraded retry.
    expect(harness.statusEventPlans.listUnfinished()).toEqual([]);

    harness.sqlite.close();
  }, 20_000);

  test("emits agent.done even when an idle event flips the status during the turn wait", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 5_000 });
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "history",
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
      stores: harness,
      turnCompletions: registry,
    });

    await index.refreshHerdrSession(sessionInput());
    const fast = await index.handleHerdrEventFast(doneEvent);
    expect(fast.statusEventPlans).toHaveLength(1);
    const plan = fast.statusEventPlans[0];
    if (!plan) throw new Error("expected statusEventPlan");

    // The done plan starts its turn wait; an idle event immediately flips the
    // stored status. The done plan must still emit its terminal event (the
    // idle flip may be skipped by the plan's terminal-emission logic).
    const planPromise = index.executeStatusEventPlan(plan);
    await index.handleHerdrEventFast({
      event: { agent_status: "idle", pane_id: "wJ:p2", type: "pane.agent_status_changed" },
      ...sessionInput(),
    });

    registry.record({
      confirmed: true,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    const planResult = await planPromise;
    expect(planResult).toEqual(expect.objectContaining({ type: "agent.done" }));
    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(events.filter((e) => e.type === "agent.done")).toHaveLength(1);
    harness.sqlite.close();
  }, 10_000);

  test("drains persisted pending plans after a crash without new events", async () => {
    const harness = openObservabilityDbHarness();
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "history",
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
      stores: harness,
    });
    await index.refreshHerdrSession(sessionInput());

    // Simulate a crash mid-plan: a pending plan row survives in the DB and the
    // agent is already at the terminal state; no new Herdr event will arrive.
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wJ:p2" });
    if (!agent) throw new Error("expected indexed agent");
    harness.statusEventPlans.insertPending({
      agentId: agent.id,
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: {
          ref: "history",
          text: "final answer",
          timestamp: null,
          stopReason: "stop",
        },
      },
      fromStatus: "working",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      toStatus: "done",
    });
    harness.agents.updateStatus({
      agentStatus: "done",
      herdrSessionName: "default",
      paneId: "wJ:p2",
    });

    // A fresh index service instance (daemon restart) drains the plan.
    const restarted = new AgentIndexService({
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "history",
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
      stores: harness,
    });
    await restarted.drainPendingPlans();

    const events = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(events.filter((e) => e.type === "agent.done")).toHaveLength(1);
    expect(harness.statusEventPlans.listUnfinished()).toEqual([]);
    harness.sqlite.close();
  });

  test("re-registering active waiter with same key after abort is not deleted by old waiter unregister", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 5_000 });
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: { ...emptyCompactHistory("pi-jsonl"), lastAssistantMessage: null },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      stores: harness,
      turnCompletions: registry,
    });

    await index.refreshHerdrSession(sessionInput());

    const firstPlanResult = await index.handleHerdrEventFast(doneEvent);
    const firstPlan = firstPlanResult.statusEventPlans[0];
    if (!firstPlan) throw new Error("expected statusEventPlan");
    const firstPromise = index.executeStatusEventPlan(firstPlan);

    // Abort first waiter (deletes set from activeWaiters map)
    await index.handleHerdrEventFast({
      event: { pane_id: "wJ:p2", type: "pane.closed" },
      ...sessionInput(),
    });

    // Start second waiter for the same key while firstPromise is resolving/finishing
    const secondPlan: typeof firstPlan = {
      agent: { ...firstPlan.agent },
      compactHistory: firstPlan.compactHistory,
      from: "working",
      to: "done",
    };
    const secondPromise = index.executeStatusEventPlan(secondPlan);

    // Await first waiter completion so its finally/unregister runs
    await firstPromise;

    // Abort second waiter - it must still be registered and reachable
    await index.handleHerdrEventFast({
      event: { pane_id: "wJ:p2", type: "pane.closed" },
      ...sessionInput(),
    });

    const secondResult = await secondPromise;
    expect(secondResult).toBeUndefined();

    harness.sqlite.close();
  }, 10_000);

  test("W12: pi received signal re-reads disk so done body is M2 not Fast-time M1", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    let diskText = "turn-1";
    let diskRef = "m1";
    let releaseFirstRefreshStarted!: () => void;
    let firstRefreshStarted = Promise.resolve();
    const armGate = () => {
      firstRefreshStarted = new Promise<void>((resolve) => {
        releaseFirstRefreshStarted = resolve;
      });
    };
    armGate();
    let gateArmed = false;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          const snapshot = { ref: diskRef, text: diskText };
          if (gateArmed) {
            gateArmed = false;
            releaseFirstRefreshStarted();
          }
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: snapshot.ref,
                text: snapshot.text,
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
    armGate();
    gateArmed = true;
    const fastPromise = index.handleHerdrEventFast(doneEvent);
    await firstRefreshStarted;
    diskRef = "m2";
    diskText = "turn-2";
    const fast = await fastPromise;
    expect(fast.statusEventPlans).toHaveLength(1);
    const plan = fast.statusEventPlans[0];
    if (!plan) throw new Error("expected statusEventPlan");
    expect(plan.compactHistory?.lastAssistantMessage?.text).toBe("turn-1");

    registry.record({
      confirmed: true,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });
    const event = await index.executeStatusEventPlan(plan);
    expect(event?.type).toBe("agent.done");
    expect(event?.compactHistory?.lastAssistantMessage?.text).toBe("turn-2");
    expect(event?.compactHistory?.lastAssistantMessage?.text).not.toBe("turn-1");
    expect(event?.compactHistory?.lastAssistantMessage?.ref).toBe("m2");
    harness.sqlite.close();
  });

  test("W13: pi timeout without advance emits degraded with null lastAssistant", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 0 });
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "m1",
                text: "turn-1",
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
    const _result = await index.handleHerdrEvent(doneEvent);
    // Guard blocks terminal emission (empty assistant messages); verify degraded record in DB.
    const invalidatedRows = harness.sqlite
      .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
      .all();
    expect(invalidatedRows).toHaveLength(1);
    const degraded = invalidatedRows[0];
    if (!degraded) throw new Error("expected degraded event row");
    const payload = JSON.parse(degraded.payload_json as string);
    expect(payload.degradedReason).toBe("no_advance_from_input");
    expect(payload.staleSnapshot).toBe(false);
    const compactHistory = JSON.parse(degraded.compact_history_json as string);
    expect(compactHistory.lastAssistantMessage).toBeNull();
    harness.sqlite.close();
  });

  test("W13: pi timeout with disk advanced to M2 emits M2 body", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 0 });
    let diskText = "turn-1";
    let diskRef = "m1";
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: diskRef,
                text: diskText,
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
    const fast = await index.handleHerdrEventFast(doneEvent);
    const plan = fast.statusEventPlans[0];
    if (!plan) throw new Error("expected statusEventPlan");
    diskRef = "m2";
    diskText = "turn-2";
    const event = await index.executeStatusEventPlan(plan);
    expect(event?.type).toBe("agent.done");
    expect(event?.compactHistory?.lastAssistantMessage?.text).toBe("turn-2");
    expect(event?.payload).not.toEqual(expect.objectContaining({ degraded: true }));
    harness.sqlite.close();
  });

  test("downgrades to waitForHistoryAdvance when signal expectedText does not match fresh lastAssistant", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    let calls = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          calls += 1;
          // First return returns the mismatched text (simulating intermediate message).
          // Second return returns the correct terminal message.
          if (calls === 1) {
            return {
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
          }
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "m2",
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
    calls = 0;
    const pending = index.handleHerdrEvent(doneEvent);
    // Record signal with expectedText that does NOT match the first fresh snapshot.
    setTimeout(() => {
      registry.record({
        confirmed: true,
        expectedText: "final answer",
        herdrSessionName: "default",
        paneId: "wJ:p2",
        terminalId: "term_claude",
        workspaceId: "wJ",
      });
    }, 10);

    const result = await pending;
    // The daemon should have re-read history after the mismatch and emitted the
    // correct terminal message (ref m2), not the intermediate m1.
    expect(result.events).toContainEqual(
      expect.objectContaining({
        compactHistory: expect.objectContaining({
          lastAssistantMessage: expect.objectContaining({ ref: "m2", text: "final answer" }),
        }),
        type: "agent.done",
      }),
    );
    harness.sqlite.close();
  }, 10_000);

  test("signal fast path carries expectedText through sync waitForSignal resolution", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: "m1",
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
      stores: harness,
      turnCompletions: registry,
    });

    await index.refreshHerdrSession(sessionInput());
    // Record signal BEFORE handleHerdrEvent so waitForSignal hits the sync fast path.
    registry.record({
      confirmed: true,
      expectedText: "final answer",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    const result = await index.handleHerdrEvent(doneEvent);
    expect(result.events).toContainEqual(
      expect.objectContaining({
        compactHistory: expect.objectContaining({
          lastAssistantMessage: expect.objectContaining({ text: "final answer" }),
        }),
        type: "agent.done",
      }),
    );
    harness.sqlite.close();
  }, 10_000);

  test("confirmed mismatch on the sync snapshot fast path stays deliverable", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    let callCount = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          callCount += 1;
          if (callCount === 1) {
            return {
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
          }
          if (callCount === 2) {
            return {
              compactHistory: {
                ...emptyCompactHistory("pi-jsonl"),
                lastAssistantMessage: {
                  ref: "m2",
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
                ref: "m3",
                text: "still not final",
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
    // Record signal BEFORE handleHerdrEvent so waitForSignal hits the sync fast path.
    registry.record({
      confirmed: true,
      expectedText: "final answer",
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    const _result = await index.handleHerdrEvent(doneEvent);
    // A confirmed turn whose refreshed snapshot still does not match expectedText
    // is delivered as a terminal event instead of being invalidated as a
    // degraded retry: the client already confirmed the write reached disk.
    expect(_result.events.filter((e) => e.type === "agent.done")).toHaveLength(1);
    const invalidatedRows = harness.sqlite
      .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
      .all();
    expect(invalidatedRows).toHaveLength(0);
    const doneRows = harness.sqlite
      .prepare("select * from agent_events where type = 'agent.done'")
      .all() as Array<Record<string, unknown>>;
    expect(doneRows).toHaveLength(1);
    expect(doneRows[0]).toMatchObject({ deliverable: 1, status: "pending" });
    expect(JSON.parse(String(doneRows[0]?.payload_json))).not.toEqual(
      expect.objectContaining({ degraded: true }),
    );
    harness.sqlite.close();
  }, 10_000);

  test("50549: a confirmed turn keeps the disk body when the signal expectedText mismatches", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    let calls = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          calls += 1;
          // Round baseline (m1) is the previous delivered answer; the disk the
          // daemon re-reads after the signal holds this round's real answer (m2).
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: calls === 1 ? "m1" : "m2",
                text: calls === 1 ? "previous round answer" : "assembled final body",
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
    calls = 0;
    const pending = index.handleHerdrEvent(doneEvent);
    // Production shape (event 50549): the extension confirmed the write but its
    // expectedText is a stale/escaped tail that the on-disk body does not end
    // with. The disk body is still this round's answer and must be delivered.
    setTimeout(() => {
      registry.record({
        confirmed: true,
        expectedText: "expected tail that never reached disk",
        herdrSessionName: "default",
        paneId: "wJ:p2",
        terminalId: "term_claude",
        workspaceId: "wJ",
      });
    }, 10);

    const result = await pending;
    expect(result.events).toContainEqual(
      expect.objectContaining({
        compactHistory: expect.objectContaining({
          lastAssistantMessage: expect.objectContaining({
            ref: "m2",
            text: "assembled final body",
          }),
        }),
        type: "agent.done",
      }),
    );
    // The mismatch stays diagnosable through a warning, but it must not blank the
    // body nor degrade the (already written) row.
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("expectedText mismatch"),
      expect.objectContaining({ degradedReason: "expected_text_mismatch" }),
    );
    const doneRows = harness.sqlite
      .prepare("select * from agent_events where type = 'agent.done'")
      .all() as Array<Record<string, unknown>>;
    expect(doneRows).toHaveLength(1);
    expect(doneRows[0]).toMatchObject({ deliverable: 1, status: "pending" });
    expect(JSON.parse(String(doneRows[0]?.payload_json))).not.toEqual(
      expect.objectContaining({ degraded: true }),
    );
    expect(JSON.parse(String(doneRows[0]?.compact_history_json))).toMatchObject({
      lastAssistantMessage: { ref: "m2", text: "assembled final body" },
    });
    harness.sqlite.close();
  }, 10_000);

  test("pi timeout with terminal but empty assistant emits degraded non_terminal_assistant", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 0 });
    let callCount = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          callCount += 1;
          return {
            compactHistory: {
              ...emptyCompactHistory("pi-jsonl"),
              lastAssistantMessage: {
                ref: `m${callCount}`,
                text: "",
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
    callCount = 0;
    const _result = await index.handleHerdrEvent(doneEvent);
    // Guard blocks emission of empty-assistant terminal events; verify degraded record in DB.
    expect(_result.events.filter((e) => e.type === "agent.done")).toHaveLength(0);
    const listAfterEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    expect(listAfterEvents.filter((e) => e.type === "agent.done")).toHaveLength(0);
    const invalidatedRows = harness.sqlite
      .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
      .all();
    expect(invalidatedRows.length).toBeGreaterThanOrEqual(1);
    const degraded = invalidatedRows.find((row: Record<string, unknown>) => {
      const payload = JSON.parse(row.payload_json as string);
      return payload.degraded && payload.degradedReason === "non_terminal_assistant";
    });
    if (!degraded) throw new Error("expected degraded empty-assistant event");
    const compactHistory = JSON.parse(degraded.compact_history_json as string);
    expect(compactHistory.lastAssistantMessage).toBeNull();
    harness.sqlite.close();
  }, 10_000);

  test("degraded empty-assistant event is invalidated and retry delivers only the complete event", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 0 });
    let callCount = 0;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          callCount += 1;
          if (callCount <= 3) {
            return {
              compactHistory: {
                ...emptyCompactHistory("pi-jsonl"),
                lastAssistantMessage: {
                  ref: `m${callCount}`,
                  text: "",
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
                ref: `m${callCount}`,
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
    callCount = 0;

    const _result = await index.handleHerdrEvent(doneEvent);
    // Degraded event is invalidated in-DB and not delivered to callers.
    expect(_result.events.filter((e) => e.type === "agent.done")).toHaveLength(0);
    // Degraded event is invalidated in-DB; verify via DB query (not result.events).
    const invalidatedRows = harness.sqlite
      .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
      .all();
    expect(invalidatedRows).toHaveLength(1);

    const afterFirst = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    // Degraded event is invalidated, so listAfter returns no deliverable events yet.
    expect(afterFirst.filter((e) => e.type === "agent.done")).toHaveLength(0);

    await index.drainPendingPlans();

    const finalEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const validDones = finalEvents.filter(
      (e) => e.type === "agent.done" && e.status !== "invalidated",
    );
    expect(validDones).toHaveLength(1);
    expect(validDones[0]?.compactHistory?.lastAssistantMessage?.text).toBe("final answer");
    expect(harness.statusEventPlans.listUnfinished()).toEqual([]);
    harness.sqlite.close();
  }, 30_000);

  test("degraded empty-assistant event retry pushes to onAgentEvent and emits outcome exactly once", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 0 });
    let callCount = 0;
    const pushedEvents: AgentEventRecord[] = [];
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          callCount += 1;
          if (callCount <= 4) {
            return {
              compactHistory: {
                ...emptyCompactHistory("pi-jsonl"),
                lastAssistantMessage: {
                  ref: `m${callCount}`,
                  text: "",
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
                ref: `m${callCount}`,
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
      onAgentEvent: (event) => {
        pushedEvents.push(event);
      },
      sleep: async () => {},
      stores: harness,
      turnCompletions: registry,
    });

    await index.refreshHerdrSession(sessionInput());
    callCount = 0;

    const _result = await index.handleHerdrEvent(doneEvent);
    // Degraded event is invalidated in-DB and not delivered to callers.
    expect(_result.events.filter((e) => e.type === "agent.done")).toHaveLength(0);
    const invalidatedRows = harness.sqlite
      .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
      .all();
    expect(invalidatedRows.length).toBeGreaterThanOrEqual(1);

    await index.drainPendingPlans();

    // The retry-generated successful event must be pushed exactly once.
    expect(pushedEvents.filter((e) => e.type === "agent.done")).toHaveLength(1);
    const pushedDone = pushedEvents.find((e) => e.type === "agent.done");
    expect(pushedDone).toBeDefined();
    expect(pushedDone?.status).toBe("pending");
    expect(pushedDone?.deliverable).toBe(1);

    const finalEvents = harness.agentEvents.listAfter({
      herdrSessionName: "default",
      workspaceId: "wJ",
    });
    const validDones = finalEvents.filter(
      (e) => e.type === "agent.done" && e.status !== "invalidated",
    );
    expect(validDones).toHaveLength(1);
    expect(validDones[0]?.compactHistory?.lastAssistantMessage?.text).toBe("final answer");
    harness.sqlite.close();
  }, 30_000);

  test("W14: a confirmed pi turn keeps the answer when the plan baseline already holds it", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    // Production shape (worker event 48085 / scout event 48117): the plan row's
    // compact_history_json is written once at plan creation and never updated,
    // and that creation happened 131ms/308ms AFTER the final assistant message
    // reached disk. The baseline therefore already carries the answer this round
    // delivers, so the refreshed snapshot is byte-identical to it and
    // `historyHasAdvanced(..., { requireAssistantChange: true })` reports "no
    // advance" for a perfectly deliverable confirmed turn.
    const terminalHistory: CompactAgentHistory = {
      ...emptyCompactHistory("pi-jsonl"),
      lastAssistantMessage: {
        ref: "m2",
        stopReason: "stop",
        text: "final answer",
        timestamp: null,
      },
    };
    // The first read after the turn signal sees the same assistant message
    // without its terminal stop reason (the message is readable before the tail
    // settles); every later read is terminal, exactly like the frozen baseline.
    const nonTerminalHistory: CompactAgentHistory = {
      ...emptyCompactHistory("pi-jsonl"),
      lastAssistantMessage: { ref: "m2", text: "final answer", timestamp: null },
    };
    let serveNonTerminalOnce = false;
    const index = new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          const compactHistory = serveNonTerminalOnce ? nonTerminalHistory : terminalHistory;
          serveNonTerminalOnce = false;
          return {
            compactHistory: { ...compactHistory },
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
      compactHistory: terminalHistory,
      from: "working",
      to: "done",
    });

    serveNonTerminalOnce = true;
    registry.record({
      confirmed: true,
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });

    const event = await index.executeStatusEventPlan({
      agent,
      compactHistory: terminalHistory,
      from: "working",
      to: "done",
      planId: plan.id,
    });

    // A confirmed round whose refreshed snapshot equals the frozen plan baseline
    // is still a deliverable round: the answer must not be blanked out as
    // `no_advance_from_input`.
    expect(event?.type).toBe("agent.done");
    expect(event?.compactHistory?.lastAssistantMessage?.text).toBe("final answer");
    expect(event?.payload).toMatchObject({ staleSnapshot: false });
    expect(event?.payload).not.toEqual(expect.objectContaining({ degraded: true }));
    harness.sqlite.close();
  }, 20_000);

  test("W15: a confirmed pi turn with no deliverable text logs the release reason", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      const emptyHistory: CompactAgentHistory = {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: null,
      };
      const index = new AgentIndexService({
        clientFactory: () => ({
          close() {},
          async sessionSnapshot() {
            return piAgentSnapshot("working");
          },
        }),
        history: {
          async resolveCompactHistory() {
            return {
              compactHistory: { ...emptyHistory },
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
        compactHistory: emptyHistory,
        from: "working",
        to: "done",
      });

      registry.record({
        confirmed: true,
        herdrSessionName: "default",
        paneId: "wJ:p2",
        terminalId: "term_claude",
        workspaceId: "wJ",
      });

      const event = await index.executeStatusEventPlan({
        agent,
        compactHistory: emptyHistory,
        from: "working",
        to: "done",
        planId: plan.id,
      });

      // Nothing to deliver: the body stays empty and the round is still released
      // non-degraded (a `degraded: true` row would be invalidated and re-run by
      // #runPlanRow), but the release is no longer silent.
      expect(event?.type).toBe("agent.done");
      expect(event?.compactHistory?.lastAssistantMessage ?? null).toBeNull();
      expect(event?.payload).toMatchObject({ staleSnapshot: false });
      expect(event?.payload).not.toEqual(expect.objectContaining({ degraded: true }));

      const warnCalls = warn.mock.calls as unknown as Array<[unknown, Record<string, unknown>]>;
      const releaseLogs = warnCalls.filter(
        (call) =>
          call[0] === "Herdsman released a confirmed pi status event with no deliverable text" &&
          call[1]?.degradedReason === "no_advance_from_input",
      );
      expect(releaseLogs).toHaveLength(1);
      expect(releaseLogs[0]?.[1]).toMatchObject({
        agentId: agent.id,
        paneId: "wJ:p2",
      });
    } finally {
      warn.mockRestore();
    }
    harness.sqlite.close();
  }, 20_000);

  // Shared fixture for the stale-baseline guard cases: the frozen plan baseline
  // is handed in per round and the daemon always re-reads `disk()` from JSONL.
  function staleGuardIndex(input: {
    disk: () => CompactAgentHistory;
    harness: ReturnType<typeof openObservabilityDbHarness>;
    registry: TurnCompletionRegistry;
  }): AgentIndexService {
    return new AgentIndexService({
      clientFactory: () => ({
        close() {},
        async sessionSnapshot() {
          return piAgentSnapshot("working");
        },
      }),
      history: {
        async resolveCompactHistory() {
          return {
            compactHistory: { ...input.disk() },
            historyRef: null,
            sourceFingerprint: null,
          };
        },
      } as unknown as AgentHistoryService,
      sleep: async () => {},
      stores: input.harness,
      turnCompletions: input.registry,
    });
  }

  // Drives one confirmed round through a persisted status event plan: the
  // production shape, where `compact_history_json` is written once when the plan
  // row is created and never updated afterwards. `eventKey` mirrors the Herdr
  // event key every production plan carries.
  async function runConfirmedPlanRound(input: {
    baseline: CompactAgentHistory;
    eventKey: string;
    expectedText?: string;
    harness: ReturnType<typeof openObservabilityDbHarness>;
    index: AgentIndexService;
    registry: TurnCompletionRegistry;
  }): Promise<AgentEventRecord | undefined> {
    const agent = input.harness.agents.findByPane({
      herdrSessionName: "default",
      paneId: "wJ:p2",
    });
    if (!agent) throw new Error("expected agent");
    const plan = input.harness.statusEventPlans.insertPending({
      agent,
      compactHistory: input.baseline,
      from: "working",
      herdrEventKey: input.eventKey,
      to: "done",
    });
    input.registry.record({
      confirmed: true,
      ...(input.expectedText ? { expectedText: input.expectedText } : {}),
      herdrSessionName: "default",
      paneId: "wJ:p2",
      terminalId: "term_claude",
      workspaceId: "wJ",
    });
    return input.index.executeStatusEventPlan({
      agent,
      compactHistory: input.baseline,
      from: "working",
      herdrEventKey: input.eventKey,
      to: "done",
      planId: plan.id,
    });
  }

  test("W16: a confirmed pi turn whose frozen baseline is the last delivered body is released as a stale duplicate", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      // Round 1 delivers "round-1 answer"; that row is then the last terminal
      // body on record for this pane.
      const firstRound: CompactAgentHistory = {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: {
          ref: "m1",
          stopReason: "stop",
          text: "round-1 answer",
          timestamp: null,
        },
      };
      const emptyHistory: CompactAgentHistory = {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: null,
      };
      let disk: CompactAgentHistory = firstRound;
      const index = staleGuardIndex({ disk: () => disk, harness, registry });
      await index.refreshHerdrSession(sessionInput());

      const first = await runConfirmedPlanRound({
        baseline: emptyHistory,
        eventKey: "evt-round-1",
        harness,
        index,
        registry,
      });
      expect(first?.compactHistory?.lastAssistantMessage?.text).toBe("round-1 answer");

      // Round 2 (production shape of the M1 sample): the plan baseline is the
      // body already delivered in round 1, the pane writes no new message, and
      // the extension's `expectedText` names a body that never reached disk —
      // which is why these rounds take the `#waitForHistoryAdvance` path at all.
      disk = { ...firstRound };
      const stale = await runConfirmedPlanRound({
        baseline: firstRound,
        eventKey: "evt-round-2",
        expectedText: "round-2 answer that never reached disk",
        harness,
        index,
        registry,
      });

      // Delivering "round-1 answer" again would hand the orchestrator the
      // previous round's result as if it were this one's: the body must be
      // emptied, still released without a `degraded` marker (that would make
      // #runPlanRow invalidate and re-run it), and logged with its own reason.
      expect(stale?.type).toBe("agent.done");
      expect(stale?.compactHistory?.lastAssistantMessage ?? null).toBeNull();
      expect(stale?.payload).toMatchObject({ staleSnapshot: false });
      expect(stale?.payload).not.toEqual(expect.objectContaining({ degraded: true }));

      const warnCalls = warn.mock.calls as unknown as Array<[unknown, Record<string, unknown>]>;
      const releaseLogs = warnCalls.filter(
        (call) =>
          call[0] === "Herdsman released a confirmed pi status event with no deliverable text" &&
          call[1]?.degradedReason === "stale_baseline_duplicate",
      );
      expect(releaseLogs).toHaveLength(1);
      expect(
        warnCalls.filter((call) => call[1]?.degradedReason === "no_advance_from_input"),
      ).toHaveLength(0);

      const invalidatedRows = harness.sqlite
        .prepare("select * from agent_events where status = 'invalidated' and type = 'agent.done'")
        .all();
      expect(invalidatedRows).toHaveLength(0);
    } finally {
      warn.mockRestore();
    }
    harness.sqlite.close();
  }, 20_000);

  test("W17: a confirmed pi turn whose baseline holds this round's answer is still delivered when the last delivered body differs", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      // Round 1 delivers "round-1 answer" (ref m1).
      const firstRound: CompactAgentHistory = {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: {
          ref: "m1",
          stopReason: "stop",
          text: "round-1 answer",
          timestamp: null,
        },
      };
      // Round 2's answer is a different message still on disk (ref m2).
      const thisRound: CompactAgentHistory = {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: {
          ref: "m2",
          stopReason: "stop",
          text: "round-2 answer",
          timestamp: null,
        },
      };
      const emptyHistory: CompactAgentHistory = {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: null,
      };
      let disk: CompactAgentHistory = firstRound;
      const index = staleGuardIndex({ disk: () => disk, harness, registry });
      await index.refreshHerdrSession(sessionInput());

      await runConfirmedPlanRound({
        baseline: emptyHistory,
        eventKey: "evt-round-1",
        harness,
        index,
        registry,
      });

      // The frozen baseline already carries this round's answer, so the refresh
      // looks like "no advance" — but the content is neither new-round-verified
      // duplicate nor previously delivered: it must reach the orchestrator.
      disk = thisRound;
      const delivered = await runConfirmedPlanRound({
        baseline: thisRound,
        eventKey: "evt-round-2",
        expectedText: "answer that never reached disk",
        harness,
        index,
        registry,
      });

      expect(delivered?.type).toBe("agent.done");
      expect(delivered?.compactHistory?.lastAssistantMessage?.text).toBe("round-2 answer");
      expect(delivered?.payload).toMatchObject({ staleSnapshot: false });
      expect(delivered?.payload).not.toEqual(expect.objectContaining({ degraded: true }));
      const warnCalls = warn.mock.calls as unknown as Array<[unknown, Record<string, unknown>]>;
      // The stale guard must not fire: the last delivered body (m1) differs from
      // the content being released (m2).
      expect(
        warnCalls.filter((call) => call[1]?.degradedReason === "stale_baseline_duplicate"),
      ).toHaveLength(0);
    } finally {
      warn.mockRestore();
    }
    harness.sqlite.close();
  }, 20_000);

  test("W18: a confirmed pi turn keeps never-delivered content even when expectedText does not match", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      const emptyHistory: CompactAgentHistory = {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: null,
      };
      const thisRound: CompactAgentHistory = {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: {
          ref: "m9",
          stopReason: "stop",
          text: "round-2 answer",
          timestamp: null,
        },
      };
      let disk: CompactAgentHistory = emptyHistory;
      const index = staleGuardIndex({ disk: () => disk, harness, registry });
      await index.refreshHerdrSession(sessionInput());

      // Round 1 has nothing to deliver, so the last delivered row for this pane
      // carries an empty body (its `lastAssistantMessage` is null).
      await runConfirmedPlanRound({
        baseline: emptyHistory,
        eventKey: "evt-round-1",
        harness,
        index,
        registry,
      });
      warn.mockClear();

      // Reviewer's counterexample, pinned as the ruled semantics: `expectedText`
      // disagrees with the text on disk and that text was never delivered. The
      // `endsWith(expectedText)` mismatch is NOT a release blocker (the ruling:
      // `expectedText` is systematically unreliable, which is a separate item —
      // see .agents/notes/20261001-pi-confirmed-turn-frozen-baseline-empty-body.md
      // "遗留 / 观察项" ①); the body is released as-is.
      disk = thisRound;
      const delivered = await runConfirmedPlanRound({
        baseline: thisRound,
        eventKey: "evt-round-2",
        expectedText: "expected text that never reached disk",
        harness,
        index,
        registry,
      });

      expect(delivered?.type).toBe("agent.done");
      expect(delivered?.compactHistory?.lastAssistantMessage?.text).toBe("round-2 answer");
      expect(delivered?.compactHistory?.lastAssistantMessage?.ref).toBe("m9");
      expect(delivered?.payload).not.toEqual(expect.objectContaining({ degraded: true }));
      // No release with an empty body happened for this round either.
      const warnCalls = warn.mock.calls as unknown as Array<[unknown, Record<string, unknown>]>;
      expect(
        warnCalls.filter(
          (call) =>
            call[0] === "Herdsman released a confirmed pi status event with no deliverable text",
        ),
      ).toHaveLength(0);
    } finally {
      warn.mockRestore();
    }
    harness.sqlite.close();
  }, 20_000);

  test("W14b: a confirmed pi turn stays deliverable when every reread is terminal and identical to the frozen baseline", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      // O5 gap of W14: there the first read after the signal was non-terminal
      // (`serveNonTerminalOnce`). The measured production shape is that every
      // read — the signal-time refresh included — is terminal and byte-identical
      // to the frozen plan baseline. This is the first round for the pane, so the
      // body was never delivered before either.
      const terminalHistory: CompactAgentHistory = {
        ...emptyCompactHistory("pi-jsonl"),
        lastAssistantMessage: {
          ref: "m2",
          stopReason: "stop",
          text: "final answer",
          timestamp: null,
        },
      };
      const index = staleGuardIndex({ disk: () => terminalHistory, harness, registry });
      await index.refreshHerdrSession(sessionInput());

      const event = await runConfirmedPlanRound({
        baseline: terminalHistory,
        eventKey: "evt-round-1",
        expectedText: "expected text that never reached disk",
        harness,
        index,
        registry,
      });

      expect(event?.type).toBe("agent.done");
      expect(event?.compactHistory?.lastAssistantMessage?.text).toBe("final answer");
      expect(event?.payload).toMatchObject({ staleSnapshot: false });
      expect(event?.payload).not.toEqual(expect.objectContaining({ degraded: true }));
      const warnCalls = warn.mock.calls as unknown as Array<[unknown, Record<string, unknown>]>;
      expect(
        warnCalls.filter(
          (call) =>
            call[0] === "Herdsman released a confirmed pi status event with no deliverable text",
        ),
      ).toHaveLength(0);
    } finally {
      warn.mockRestore();
    }
    harness.sqlite.close();
  }, 20_000);
});
