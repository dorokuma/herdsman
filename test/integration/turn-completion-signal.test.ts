import { afterEach, describe, expect, test } from "vitest";
import type { AgentHistoryService } from "@/agent-history/service.js";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { AgentIndexService } from "@/observability/agent-index-service.js";
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
  });

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
  });

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
              lastAssistantMessage: { ref: "history", text: "final answer", timestamp: null },
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
  });
  test("retries eight times after a received turn signal when history is still empty", async () => {
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
    calls = 0;
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

    const result = await pending;
    expect(calls).toBe(10);
    expect(result.events).toContainEqual(
      expect.objectContaining({
        compactHistory: expect.objectContaining({ lastAssistantMessage: null }),
        type: "agent.done",
      }),
    );
    harness.sqlite.close();
  });
  test("generates agent.done as-is with a warning when no turn signal arrives (old extension)", async () => {
    const harness = openObservabilityDbHarness();
    const registry = new TurnCompletionRegistry({ timeoutMs: 20 });
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
      turnCompletions: registry,
    });

    await index.refreshHerdrSession(sessionInput());
    calls = 0;
    const result = await index.handleHerdrEvent(doneEvent);
    expect(calls).toBe(9);
    expect(result.events).toContainEqual(
      expect.objectContaining({
        compactHistory: expect.objectContaining({ lastAssistantMessage: null }),
        type: "agent.done",
      }),
    );
    harness.sqlite.close();
  });

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
});
