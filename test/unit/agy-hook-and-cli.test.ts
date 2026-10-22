import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import { createAgentHistoryService } from "@/agent-history/service.js";
import { runAgyHook } from "@/agy/hook.js";
import { parseCliArgs, runCliCommand } from "@/cli/herdsman.js";
import { ObservabilityRpcServer } from "@/daemon/observability-server.js";
import { AgentContextService } from "@/observability/agent-context-service.js";
import { AgentIndexService } from "@/observability/agent-index-service.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import type { AgentEventRecord } from "@/observability/contracts.js";
import {
  cleanupTempDirs,
  openObservabilityDbHarness,
  tempDirs,
} from "../integration/observability-db-harness.js";
import { RpcTestClient } from "../integration/rpc-test-client.js";

const servers: ObservabilityRpcServer[] = [];

afterEach(async () => {
  await Promise.all(servers.splice(0).map((server) => server.stop()));
  cleanupTempDirs();
});

const HERDR_ENV: NodeJS.ProcessEnv = {
  HERDR_ENV: "1",
  HERDR_PANE_ID: "wB:p1",
  HERDR_SOCKET_PATH: "/tmp/herdr-default.sock",
  HERDR_WORKSPACE_ID: "wB",
};

function makeDoneEvent(id: number, text: string, terminalId = "term_peer"): AgentEventRecord {
  return {
    agentId: "ag_peer",
    compactHistory: {
      historyRef: {
        kind: "agent_session",
        path: "/tmp/pi-session.jsonl",
        source: "pi-jsonl",
        value: "/tmp/pi-session.jsonl",
      },
      lastAssistantMessage: {
        ref: `ref-${id}`,
        text,
        timestamp: "2026-10-21T12:00:00.000Z",
      },
      lastToolResult: null,
      lastUserMessage: null,
      messageCount: 2,
      source: "pi-jsonl",
      updatedAt: "2026-10-21T12:00:00.000Z",
    },
    createdAt: new Date("2026-10-21T12:00:00.000Z"),
    deliverable: 1,
    deliveryAttempts: 0,
    herdrSessionName: "default",
    id,
    invalidatedReason: null,
    nextAttemptAt: null,
    paneGeneration: "gen-1",
    paneId: "wB:p2",
    payload: { agent: "pi", from: "working", name: "role-scout-1", to: "done" },
    status: "pending",
    terminalId,
    type: "agent.done",
    workspaceId: "wB",
  };
}

describe("herdsman orchestrator CLI commands", () => {
  test("parses on, off, status, and agy-hook commands", () => {
    expect(parseCliArgs(["on", "--if-unowned", "--json"], HERDR_ENV)).toEqual({
      action: "on",
      command: "orchestrator",
      herdrSocketPath: "/tmp/herdr-default.sock",
      ifUnowned: true,
      json: true,
      paneId: "wB:p1",
      workspaceId: "wB",
    });
    expect(parseCliArgs(["off"], HERDR_ENV)).toEqual({
      action: "off",
      command: "orchestrator",
      herdrSocketPath: "/tmp/herdr-default.sock",
      json: false,
      paneId: "wB:p1",
      workspaceId: "wB",
    });
    expect(parseCliArgs(["status"], HERDR_ENV)).toEqual({
      action: "status",
      command: "orchestrator",
      herdrSocketPath: "/tmp/herdr-default.sock",
      json: false,
      paneId: "wB:p1",
      workspaceId: "wB",
    });
    expect(parseCliArgs(["agy-hook", "PreInvocation"], HERDR_ENV)).toEqual({
      command: "agy-hook",
      hookEventName: "PreInvocation",
    });
    expect(parseCliArgs(["agy-hook", "Stop"], HERDR_ENV)).toEqual({
      command: "agy-hook",
      hookEventName: "Stop",
    });
    expect(parseCliArgs(["agy-hook", "WakeWaiter"], HERDR_ENV)).toEqual({
      command: "agy-hook",
      hookEventName: "WakeWaiter",
    });
    expect(() => parseCliArgs(["agy-hook", "Invalid"], HERDR_ENV)).toThrow(
      "agy-hook requires PreInvocation, Stop, or WakeWaiter",
    );
  });

  test("executes herdsman on --if-unowned without stealing an already owned workspace", async () => {
    const calls: Array<[string, unknown]> = [];
    const lines: string[] = [];
    const cmd = parseCliArgs(["on", "--if-unowned"], HERDR_ENV);
    await runCliCommand(cmd, {
      connect: async () => ({
        close: () => undefined,
        request: async (method, params) => {
          calls.push([method, params]);
          return {
            presence: {
              herdrSessionName: "default",
              paneId: "wB:p1",
              terminalId: "term_self",
              workspaceId: "wB",
            },
            state: {
              ackedEventId: 0,
              herdrSessionName: "default",
              owner: { paneId: "wB:p9", terminalId: "term_other" },
              updatedAt: "2026-10-21T12:00:00.000Z",
              workspaceId: "wB",
            },
          };
        },
      }),
      output: (line) => lines.push(line),
      socketPath: "/tmp/herdsman.sock",
    });

    expect(calls.map(([method]) => method)).toEqual(["agent.orchestrator.register"]);
    expect(lines).toEqual(["Herdsman is off · owned by wB:p9 (default/wB)"]);
  });

  test("executes herdsman on and herdsman off commands", async () => {
    const calls: Array<[string, unknown]> = [];
    const lines: string[] = [];
    let owner: { paneId: string; terminalId: string } | null = null;
    const connect = async () => ({
      close: () => undefined,
      request: async (method: string, params: unknown) => {
        calls.push([method, params]);
        if (method === "agent.orchestrator.set") {
          const enabled = (params as { enabled?: boolean })?.enabled === true;
          owner = enabled ? { paneId: "wB:p1", terminalId: "term_self" } : null;
        }
        return {
          changed: true,
          presence: {
            herdrSessionName: "default",
            paneId: "wB:p1",
            terminalId: "term_self",
            workspaceId: "wB",
          },
          state: {
            ackedEventId: 0,
            herdrSessionName: "default",
            owner,
            updatedAt: "2026-10-21T12:00:00.000Z",
            workspaceId: "wB",
          },
        };
      },
    });

    await runCliCommand(parseCliArgs(["on"], HERDR_ENV), {
      connect,
      output: (line) => lines.push(line),
      socketPath: "/tmp/herdsman.sock",
    });
    await runCliCommand(parseCliArgs(["off"], HERDR_ENV), {
      connect,
      output: (line) => lines.push(line),
      socketPath: "/tmp/herdsman.sock",
    });

    expect(calls.map(([method, params]) => [method, params])).toEqual([
      ["agent.orchestrator.register", expect.objectContaining({ paneId: "wB:p1" })],
      ["agent.orchestrator.set", { enabled: true }],
      ["agent.orchestrator.register", expect.objectContaining({ paneId: "wB:p1" })],
      ["agent.orchestrator.set", { enabled: false }],
    ]);
    expect(lines).toEqual([
      "Herdsman is watching agent updates · default/wB · wB:p1",
      "Herdsman is off (default/wB)",
    ]);
  });
});

describe("agy lifecycle hooks (PreInvocation & Stop)", () => {
  test("PreInvocation auto-claims unowned workspace on HERDSMAN_AUTO_ON=1 and injects pending updates via ephemeralMessage", async () => {
    const stateDir = mkdtempSync(join(tmpdir(), "herdsman-agy-state-"));
    const calls: Array<[string, unknown]> = [];
    try {
      let ownerTerminalId: string | null = null;
      const result = await runAgyHook(
        "PreInvocation",
        JSON.stringify({ sessionId: "11111111-2222-3333-4444-555555555555" }),
        {
          connect: async () => ({
            close: () => undefined,
            request: async (method, params) => {
              calls.push([method, params]);
              if (method === "agent.orchestrator.set") {
                ownerTerminalId = "term_self";
              }
              return {
                events: [makeDoneEvent(10, "Scout finished code search.")],
                presence: {
                  herdrSessionName: "default",
                  paneId: "wB:p1",
                  subscriberId: "agy:11111111-2222-3333-4444-555555555555",
                  terminalId: "term_self",
                  workspaceId: "wB",
                },
                state: {
                  ackedEventId: 0,
                  herdrSessionName: "default",
                  owner: ownerTerminalId ? { paneId: "wB:p1", terminalId: ownerTerminalId } : null,
                  updatedAt: "2026-10-21T12:00:00.000Z",
                  workspaceId: "wB",
                },
              };
            },
          }),
          environment: { ...HERDR_ENV, HERDSMAN_AUTO_ON: "1" },
          socketPath: "/tmp/herdsman.sock",
          stateDir,
        },
      );

      expect(calls.map(([method]) => method)).toEqual([
        "agent.orchestrator.register",
        "agent.orchestrator.set",
      ]);
      expect(result.injectSteps).toHaveLength(1);
      expect(result.injectSteps?.[0]?.ephemeralMessage).toContain("[HERDSMAN AGENT UPDATES]");
      expect(result.injectSteps?.[0]?.ephemeralMessage).toContain("Scout finished code search.");

      calls.length = 0;
      const nonMainPaneResult = await runAgyHook(
        "PreInvocation",
        JSON.stringify({ sessionId: "22222222-2222-3333-4444-555555555555" }),
        {
          connect: async () => ({
            close: () => undefined,
            request: async (method, params) => {
              calls.push([method, params]);
              return {
                events: [],
                presence: {
                  herdrSessionName: "default",
                  paneId: "wB:p2",
                  subscriberId: "agy:22222222-2222-3333-4444-555555555555",
                  terminalId: "term_p2",
                  workspaceId: "wB",
                },
                state: {
                  ackedEventId: 0,
                  herdrSessionName: "default",
                  owner: null,
                  updatedAt: "2026-10-21T12:00:00.000Z",
                  workspaceId: "wB",
                },
              };
            },
          }),
          environment: { ...HERDR_ENV, HERDR_PANE_ID: "wB:p2", HERDSMAN_AUTO_ON: "1" },
          socketPath: "/tmp/herdsman.sock",
          stateDir,
        },
      );
      expect(calls.map(([method]) => method)).toEqual(["agent.orchestrator.register"]);
      expect(nonMainPaneResult).toEqual({});
    } finally {
      rmSync(stateDir, { force: true, recursive: true });
    }
  });

  test("PreInvocation does not ack before turn end, USER_CANCELED Stop rolls back unacked events, and NO_TOOL_CALL Stop acks in order", async () => {
    const stateDir = mkdtempSync(join(tmpdir(), "herdsman-agy-ack-order-"));
    const ackedIds: number[] = [];
    let ackedEventId = 0;
    const sessionId = "99999999-2222-3333-4444-555555555555";
    const connect = async () => ({
      close: () => undefined,
      request: async (method: string, params: unknown) => {
        if (method === "agent.notifications.ack") {
          const eventId = (params as { eventId: number }).eventId;
          ackedIds.push(eventId);
          ackedEventId = Math.max(ackedEventId, eventId);
          return { acknowledged: true };
        }
        return {
          events: [
            makeDoneEvent(10, "First subagent finished."),
            makeDoneEvent(12, "Second subagent finished."),
          ],
          presence: {
            herdrSessionName: "default",
            paneId: "wB:p1",
            subscriberId: `agy:${sessionId}`,
            terminalId: "term_self",
            workspaceId: "wB",
          },
          state: {
            ackedEventId,
            herdrSessionName: "default",
            owner: { paneId: "wB:p1", terminalId: "term_self" },
            updatedAt: "2026-10-21T12:00:00.000Z",
            workspaceId: "wB",
          },
        };
      },
    });
    try {
      // 1. PreInvocation #1 injects events 10 & 12 without acking.
      const pre1 = await runAgyHook(
        "PreInvocation",
        JSON.stringify({ invocationNum: 1, sessionId }),
        { connect, environment: HERDR_ENV, socketPath: "/tmp/herdsman.sock", stateDir },
      );
      expect(pre1.injectSteps).toHaveLength(1);
      expect(ackedIds).toEqual([]);

      // 2. PreInvocation #2 (after a tool call in the same turn) neither re-injects nor acks.
      const pre2 = await runAgyHook(
        "PreInvocation",
        JSON.stringify({ invocationNum: 2, sessionId }),
        { connect, environment: HERDR_ENV, socketPath: "/tmp/herdsman.sock", stateDir },
      );
      expect(pre2).toEqual({});
      expect(ackedIds).toEqual([]);

      // 3. User cancels the turn -> Stop(USER_CANCELED) rolls back unacked events.
      await runAgyHook("Stop", JSON.stringify({ sessionId, terminationReason: "USER_CANCELED" }), {
        connect,
        environment: HERDR_ENV,
        socketPath: "/tmp/herdsman.sock",
        stateDir,
      });
      expect(ackedIds).toEqual([]);

      // 4. Next turn's PreInvocation re-delivers events 10 & 12 because they were rolled back.
      const pre3 = await runAgyHook(
        "PreInvocation",
        JSON.stringify({ invocationNum: 1, sessionId }),
        { connect, environment: HERDR_ENV, socketPath: "/tmp/herdsman.sock", stateDir },
      );
      expect(pre3.injectSteps).toHaveLength(1);

      // 5. Normal turn completion -> Stop(NO_TOOL_CALL) acks events 10 and 12 in ascending order.
      await runAgyHook("Stop", JSON.stringify({ sessionId, terminationReason: "NO_TOOL_CALL" }), {
        connect,
        environment: HERDR_ENV,
        socketPath: "/tmp/herdsman.sock",
        stateDir,
        waitTimeoutMs: 10,
      });
      expect(ackedIds).toEqual([10, 12]);
    } finally {
      rmSync(stateDir, { force: true, recursive: true });
    }
  });

  test("dispatched subagent (CTXMODE_ROLE set) skips PreInvocation and only signals turn completion on Stop", async () => {
    const dir = mkdtempSync(join(tmpdir(), "herdsman-agy-subagent-"));
    const transcriptPath = join(dir, "transcript.jsonl");
    writeFileSync(
      transcriptPath,
      [
        JSON.stringify({ content: "Find callers", type: "USER_INPUT" }),
        JSON.stringify({ content: "Found 3 callers in src/daemon.", type: "PLANNER_RESPONSE" }),
      ].join("\n"),
    );
    const calls: Array<[string, unknown]> = [];
    try {
      const env = { ...HERDR_ENV, CTXMODE_ROLE: "scout", HERDSMAN_AUTO_ON: "1" };
      const preOut = await runAgyHook(
        "PreInvocation",
        JSON.stringify({ sessionId: "22222222-2222-3333-4444-555555555555" }),
        {
          connect: async () => ({
            close: () => undefined,
            request: async (method, params) => {
              calls.push([method, params]);
              return {};
            },
          }),
          environment: env,
          socketPath: "/tmp/herdsman.sock",
          stateDir: dir,
        },
      );
      expect(preOut).toEqual({});
      expect(calls).toEqual([]);

      const stopOut = await runAgyHook(
        "Stop",
        JSON.stringify({
          sessionId: "22222222-2222-3333-4444-555555555555",
          terminationReason: "NO_TOOL_CALL",
          transcriptPath,
        }),
        {
          connect: async () => ({
            close: () => undefined,
            request: async (method, params) => {
              calls.push([method, params]);
              return {
                events: [makeDoneEvent(11, "Should not wake subagent")],
                presence: {
                  herdrSessionName: "default",
                  paneId: "wB:p1",
                  subscriberId: "agy:22222222-2222-3333-4444-555555555555",
                  terminalId: "term_subagent",
                  workspaceId: "wB",
                },
                state: {
                  ackedEventId: 0,
                  herdrSessionName: "default",
                  owner: { paneId: "wB:p1", terminalId: "term_subagent" },
                  updatedAt: "2026-10-21T12:00:00.000Z",
                  workspaceId: "wB",
                },
              };
            },
          }),
          environment: env,
          socketPath: "/tmp/herdsman.sock",
          stateDir: dir,
        },
      );
      expect(stopOut).toEqual({});
      expect(calls).toEqual([
        [
          "agent.orchestrator.register",
          {
            herdrSocketPath: "/tmp/herdr-default.sock",
            paneId: "wB:p1",
            sessionRef: {
              agent: "agy",
              kind: "id",
              source: "agy-subagent",
              value: "22222222-2222-3333-4444-555555555555",
            },
            subscriberId: "agy:22222222-2222-3333-4444-555555555555",
            subscriberKind: "agy",
            workspaceId: "wB",
          },
        ],
        [
          "agent.turn.completed",
          {
            confirmed: true,
            expectedText: "Found 3 callers in src/daemon.",
            herdrSessionName: "default",
            paneId: "wB:p1",
            terminalId: "term_subagent",
            workspaceId: "wB",
          },
        ],
      ]);
    } finally {
      rmSync(dir, { force: true, recursive: true });
    }
  });

  test("Stop hook returns immediately by spawning detached WakeWaiter, and WakeWaiter wakes idle main pane on agent.event for next PreInvocation/Stop cycle", async () => {
    const dir = mkdtempSync(join(tmpdir(), "herdsman-agy-stop-wait-"));
    const sessionId = "33333333-2222-3333-4444-555555555555";
    let pendingEvents: AgentEventRecord[] = [];
    let ackedEventId = 0;
    const ackedIds: number[] = [];
    let selfStatus: "idle" | "working" = "working";
    let peerStatus: "idle" | "working" = "working";
    let orchestratorGetCalls = 0;
    let pushNotification: ((method: string, params: unknown) => void) | undefined;
    const spawnedWaiters: Array<{ sessionId: string; stateDir: string; waiterEpoch: number }> = [];
    const wakePrompts: Array<{ paneId: string; promptText: string }> = [];

    const connect = async (
      _socketPath: string,
      onNotification?: (method: string, params: unknown) => void,
    ) => {
      if (onNotification) pushNotification = onNotification;
      return {
        close: () => undefined,
        request: async (method: string, params: unknown) => {
          if (method === "agent.notifications.ack") {
            const eventId = (params as { eventId: number }).eventId;
            ackedIds.push(eventId);
            ackedEventId = Math.max(ackedEventId, eventId);
            return { acknowledged: true };
          }
          if (method === "agent.list") {
            return {
              agents: [
                {
                  agent: "agy",
                  agentStatus: selfStatus,
                  firstSeenAt: "2026-10-21T11:50:00.000Z",
                  herdrSessionName: "default",
                  id: "ag_self",
                  paneId: "wB:p1",
                  terminalId: "term_self",
                  workspaceId: "wB",
                },
                {
                  agent: "pi",
                  agentStatus: peerStatus,
                  firstSeenAt: "2026-10-21T11:50:00.000Z",
                  herdrSessionName: "default",
                  id: "ag_peer",
                  paneId: "wB:p2",
                  terminalId: "term_peer",
                  workspaceId: "wB",
                },
              ],
            };
          }
          if (method === "agent.orchestrator.get") {
            orchestratorGetCalls += 1;
          }
          return {
            events: pendingEvents,
            presence: {
              herdrSessionName: "default",
              paneId: "wB:p1",
              subscriberId: `agy:${sessionId}`,
              terminalId: "term_self",
              workspaceId: "wB",
            },
            state: {
              ackedEventId,
              herdrSessionName: "default",
              owner: { paneId: "wB:p1", terminalId: "term_self" },
              updatedAt: "2026-10-21T12:00:00.000Z",
              workspaceId: "wB",
            },
          };
        },
      };
    };

    try {
      // 1. Stop sees active peer, writes turnEpoch/waiterEpoch/waiterPid, spawns WakeWaiter, and returns {} immediately.
      const stopResult = await runAgyHook(
        "Stop",
        JSON.stringify({ sessionId, terminationReason: "NO_TOOL_CALL" }),
        {
          connect,
          environment: HERDR_ENV,
          socketPath: "/tmp/herdsman.sock",
          spawnWakeWaiter: (input) => {
            spawnedWaiters.push({
              sessionId: input.sessionId,
              stateDir: input.stateDir,
              waiterEpoch: input.waiterEpoch,
            });
            return { pid: process.pid };
          },
          stateDir: dir,
        },
      );
      expect(stopResult).toEqual({});
      expect(spawnedWaiters).toEqual([{ sessionId, stateDir: dir, waiterEpoch: 1 }]);
      const stateAfterStop = JSON.parse(readFileSync(join(dir, `${sessionId}.json`), "utf8"));
      expect(stateAfterStop).toMatchObject({
        turnEpoch: 1,
        waiterEpoch: 1,
        waiterPid: process.pid,
      });

      // 2. WakeWaiter runs in background; while selfStatus is "working", it must not call agent.orchestrator.get.
      const waiterPromise = runAgyHook("WakeWaiter", "", {
        connect,
        environment: {
          ...HERDR_ENV,
          HERDSMAN_AGY_SESSION_ID: sessionId,
          HERDSMAN_AGY_STATE_DIR: dir,
          HERDSMAN_AGY_WAITER_EPOCH: "1",
        },
        pollIntervalMs: 25,
        socketPath: "/tmp/herdsman.sock",
        stateDir: dir,
        triggerWakePrompt: async (input) => {
          wakePrompts.push(input);
        },
        waitTimeoutMs: 5_000,
      });

      await new Promise((resolve) => setTimeout(resolve, 15));
      pendingEvents = [makeDoneEvent(25, "Subagent finished implementation and tests pass.")];
      pushNotification?.("agent.event", { event: pendingEvents[0] });
      expect(orchestratorGetCalls).toBe(0);
      expect(wakePrompts).toEqual([]);

      // Once main pane transitions to idle, WakeWaiter inspects pending events, triggers wake prompt without acking, and exits.
      selfStatus = "idle";
      peerStatus = "idle";
      const waiterResult = await waiterPromise;
      expect(waiterResult).toEqual({});
      expect(orchestratorGetCalls).toBeGreaterThan(0);
      expect(wakePrompts).toEqual([{ paneId: "wB:p1", promptText: "[herdsman-wake]" }]);
      expect(ackedIds).toEqual([]);

      // 3. Subsequent PreInvocation injects the pending event and Stop acks it in order.
      const preResult = await runAgyHook(
        "PreInvocation",
        JSON.stringify({ invocationNum: 1, sessionId }),
        { connect, environment: HERDR_ENV, socketPath: "/tmp/herdsman.sock", stateDir: dir },
      );
      expect(preResult.injectSteps?.[0]?.ephemeralMessage).toContain(
        "Subagent finished implementation and tests pass.",
      );
      expect(ackedIds).toEqual([]);

      const finalStop = await runAgyHook(
        "Stop",
        JSON.stringify({ sessionId, terminationReason: "NO_TOOL_CALL" }),
        { connect, environment: HERDR_ENV, socketPath: "/tmp/herdsman.sock", stateDir: dir },
      );
      expect(finalStop).toEqual({});
      expect(ackedIds).toEqual([25]);
    } finally {
      rmSync(dir, { force: true, recursive: true });
    }
  });

  test("WakeWaiter silently acks suppressed events (upstream 429 error and empty completed) without triggering wake prompt", async () => {
    const dir = mkdtempSync(join(tmpdir(), "herdsman-agy-suppressed-"));
    const sessionId = "44444444-2222-3333-4444-555555555555";
    const ackedIds: number[] = [];
    let ackedEventId = 0;
    const wakePrompts: Array<{ paneId: string; promptText: string }> = [];
    const suppressedEvents: AgentEventRecord[] = [
      {
        ...makeDoneEvent(30, ""),
        payload: {
          agent: "pi",
          name: "role-worker-1",
          reason: "429 rate limit exceeded",
        },
        type: "agent.failed",
      },
      makeDoneEvent(31, "   "),
    ];

    try {
      const result = await runAgyHook("WakeWaiter", "", {
        connect: async () => ({
          close: () => undefined,
          request: async (method, params) => {
            if (method === "agent.notifications.ack") {
              const eventId = (params as { eventId: number }).eventId;
              ackedIds.push(eventId);
              ackedEventId = Math.max(ackedEventId, eventId);
              return { acknowledged: true };
            }
            if (method === "agent.list") {
              return { agents: [] };
            }
            return {
              events: suppressedEvents,
              presence: {
                herdrSessionName: "default",
                paneId: "wB:p1",
                subscriberId: `agy:${sessionId}`,
                terminalId: "term_self",
                workspaceId: "wB",
              },
              state: {
                ackedEventId,
                herdrSessionName: "default",
                owner: { paneId: "wB:p1", terminalId: "term_self" },
                updatedAt: "2026-10-21T12:00:00.000Z",
                workspaceId: "wB",
              },
            };
          },
        }),
        environment: {
          ...HERDR_ENV,
          HERDSMAN_AGY_SESSION_ID: sessionId,
          HERDSMAN_AGY_STATE_DIR: dir,
          HERDSMAN_AGY_WAITER_EPOCH: "1",
        },
        socketPath: "/tmp/herdsman.sock",
        stateDir: dir,
        triggerWakePrompt: async (input) => {
          wakePrompts.push(input);
        },
        waitTimeoutMs: 1_000,
      });

      expect(result).toEqual({});
      expect(ackedIds).toEqual([30, 31]);
      expect(wakePrompts).toEqual([]);
    } finally {
      rmSync(dir, { force: true, recursive: true });
    }
  });

  test("WakeWaiter tolerates first-iteration waiterPid write ordering and exits without waking when PreInvocation advances turnEpoch", async () => {
    vi.useFakeTimers();
    const dir = mkdtempSync(join(tmpdir(), "herdsman-agy-epoch-race-"));
    const sessionId = "55555555-2222-3333-4444-555555555555";
    const wakePrompts: Array<{ paneId: string; promptText: string }> = [];
    let pendingEvents: AgentEventRecord[] = [];
    let pushNotification: ((method: string, params: unknown) => void) | undefined;

    try {
      // Simulate Stop having written turnEpoch=1 & waiterEpoch=1 before waiterPid is flushed, or with stale waiterPid on first tick.
      writeFileSync(
        join(dir, `${sessionId}.json`),
        JSON.stringify({ turnEpoch: 1, waiterEpoch: 1, waiterPid: process.pid + 999 }),
      );

      const connect = async (
        _socketPath: string,
        onNotification?: (method: string, params: unknown) => void,
      ) => {
        if (onNotification) pushNotification = onNotification;
        return {
          close: () => undefined,
          request: async (method: string) => {
            if (method === "agent.list") {
              return {
                agents: [
                  {
                    agent: "pi",
                    agentStatus: "working",
                    firstSeenAt: "2026-10-21T12:00:00.000Z",
                    herdrSessionName: "default",
                    id: "ag_peer",
                    paneId: "wB:p2",
                    terminalId: "term_peer",
                    workspaceId: "wB",
                  },
                ],
              };
            }
            return {
              events: pendingEvents,
              presence: {
                herdrSessionName: "default",
                paneId: "wB:p1",
                subscriberId: `agy:${sessionId}`,
                terminalId: "term_self",
                workspaceId: "wB",
              },
              state: {
                ackedEventId: 0,
                herdrSessionName: "default",
                owner: { paneId: "wB:p1", terminalId: "term_self" },
                updatedAt: "2026-10-21T12:00:00.000Z",
                workspaceId: "wB",
              },
            };
          },
        };
      };

      const waiterPromise = runAgyHook("WakeWaiter", "", {
        connect,
        environment: {
          ...HERDR_ENV,
          HERDSMAN_AGY_SESSION_ID: sessionId,
          HERDSMAN_AGY_STATE_DIR: dir,
          HERDSMAN_AGY_WAITER_EPOCH: "1",
        },
        pollIntervalMs: 1_000,
        socketPath: "/tmp/herdsman.sock",
        stateDir: dir,
        triggerWakePrompt: async (input) => {
          wakePrompts.push(input);
        },
        waitTimeoutMs: 10_000,
      });

      await vi.advanceTimersByTimeAsync(10);
      // Stop finishes writing waiterPid = process.pid, then user starts a new turn via PreInvocation (advancing turnEpoch to 2).
      writeFileSync(
        join(dir, `${sessionId}.json`),
        JSON.stringify({ turnEpoch: 1, waiterEpoch: 1, waiterPid: process.pid }),
      );
      await runAgyHook("PreInvocation", JSON.stringify({ invocationNum: 1, sessionId }), {
        connect,
        environment: HERDR_ENV,
        socketPath: "/tmp/herdsman.sock",
        stateDir: dir,
      });

      pendingEvents = [makeDoneEvent(35, "Late subagent result after user already started turn.")];
      pushNotification?.("agent.event", { event: pendingEvents[0] });
      await vi.advanceTimersByTimeAsync(20);

      expect(await waiterPromise).toEqual({});
      expect(wakePrompts).toEqual([]);
    } finally {
      vi.useRealTimers();
      rmSync(dir, { force: true, recursive: true });
    }
  });

  test("WakeWaiter logs console.warn and preserves events for next PreInvocation when triggerWakePrompt fails or RPC request times out", async () => {
    vi.useFakeTimers();
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const dir = mkdtempSync(join(tmpdir(), "herdsman-agy-wake-fail-"));
    const sessionId = "66666666-2222-3333-4444-555555555555";
    const pendingEvents = [makeDoneEvent(40, "Worker completed changes.")];
    let wakeAttempts = 0;

    const connect = async () => ({
      close: () => undefined,
      request: async (method: string) => {
        if (method === "agent.list") {
          return { agents: [] };
        }
        return {
          events: pendingEvents,
          presence: {
            herdrSessionName: "default",
            paneId: "wB:p1",
            subscriberId: `agy:${sessionId}`,
            terminalId: "term_self",
            workspaceId: "wB",
          },
          state: {
            ackedEventId: 0,
            herdrSessionName: "default",
            owner: { paneId: "wB:p1", terminalId: "term_self" },
            updatedAt: "2026-10-21T12:00:00.000Z",
            workspaceId: "wB",
          },
        };
      },
    });

    try {
      const waiterPromise = runAgyHook("WakeWaiter", "", {
        connect,
        environment: {
          ...HERDR_ENV,
          HERDSMAN_AGY_SESSION_ID: sessionId,
          HERDSMAN_AGY_STATE_DIR: dir,
          HERDSMAN_AGY_WAITER_EPOCH: "1",
        },
        pollIntervalMs: 100,
        socketPath: "/tmp/herdsman.sock",
        stateDir: dir,
        triggerWakePrompt: async () => {
          wakeAttempts += 1;
          throw new Error("herdr prompt timed out");
        },
        waitTimeoutMs: 5_000,
      });

      await vi.advanceTimersByTimeAsync(500);
      expect(await waiterPromise).toEqual({});
      expect(wakeAttempts).toBe(3);
      expect(warnSpy).toHaveBeenCalledWith(
        expect.stringContaining("Herdsman agy wake prompt failed (attempt 3/3)"),
      );

      // Event 40 remains unconsumed and is delivered on the user's next PreInvocation.
      const preOut = await runAgyHook(
        "PreInvocation",
        JSON.stringify({ invocationNum: 1, sessionId }),
        { connect, environment: HERDR_ENV, socketPath: "/tmp/herdsman.sock", stateDir: dir },
      );
      expect(preOut.injectSteps?.[0]?.ephemeralMessage).toContain("Worker completed changes.");

      // Also verify RPC requestTimeoutMs closes hung client and exits cleanly with console.warn.
      let closedHungClient = false;
      const hungWaiterPromise = runAgyHook("WakeWaiter", "", {
        connect: async () => ({
          close: () => {
            closedHungClient = true;
          },
          request: () => new Promise<never>(() => undefined),
        }),
        environment: {
          ...HERDR_ENV,
          HERDSMAN_AGY_SESSION_ID: sessionId,
          HERDSMAN_AGY_STATE_DIR: dir,
          HERDSMAN_AGY_WAITER_EPOCH: "2",
        },
        requestTimeoutMs: 50,
        socketPath: "/tmp/herdsman.sock",
        stateDir: dir,
      });
      await vi.advanceTimersByTimeAsync(100);
      expect(await hungWaiterPromise).toEqual({});
      expect(closedHungClient).toBe(true);
      expect(warnSpy).toHaveBeenCalledWith(
        expect.stringContaining("Herdsman agy WakeWaiter RPC request timed out after 50ms"),
      );
    } finally {
      warnSpy.mockRestore();
      vi.useRealTimers();
      rmSync(dir, { force: true, recursive: true });
    }
  });
});

describe("ObservabilityRpcServer agy terminal presence lease", () => {
  test("retains agy orchestrator ownership across short-lived hook socket closes, releases on generation-scoped pane.closed, and does not block pi disconnect grace", async () => {
    const harness = openObservabilityDbHarness();
    const dir = mkdtempSync(join(tmpdir(), "herdsman-agy-rpc-"));
    tempDirs.push(dir);
    const socketPath = join(dir, "herdsman.sock");
    harness.herdrSessions.upsertRunning({
      name: "default",
      sessionDir: "/tmp/herdr-default",
      socketPath: "/tmp/herdr-default.sock",
    });
    harness.agents.replaceForSession({
      agents: [
        {
          agent: "agy",
          agent_status: "idle",
          cwd: "/repo/a",
          focused: true,
          herdr_session_name: "default",
          name: "main-agy",
          pane_generation: "gen-agy-1",
          pane_id: "wB:p-agy",
          tab_id: "wB:t1",
          terminal_id: "term_agy",
          workspace_id: "wB",
        },
        {
          agent: "pi",
          agent_status: "idle",
          cwd: "/repo/a",
          focused: false,
          herdr_session_name: "default",
          name: "peer-pi",
          pane_generation: "gen-pi-1",
          pane_id: "wB:p-pi",
          tab_id: "wB:t2",
          terminal_id: "term_pi",
          workspace_id: "wB",
        },
      ],
      herdrSessionName: "default",
    });

    const history = createAgentHistoryService({ cache: harness.agentHistoryCache, homeDir: dir });
    const context = new AgentContextService({
      history,
      stores: {
        agentContextSnapshots: harness.agentContextSnapshots,
        agents: harness.agents,
      },
    });
    const orchestrator = new AgentOrchestratorService({
      agentEvents: harness.agentEvents,
      agents: harness.agents,
      scopes: harness.agentOrchestratorScopes,
    });
    const index = new AgentIndexService({
      context,
      stores: harness,
    });
    const server = new ObservabilityRpcServer({
      context,
      disconnectGraceMs: 25,
      history,
      orchestrator,
      peerPidOf: () => process.pid,
      resolvePaneIdentity: async ({ paneId }) => ({
        cwd: process.cwd(),
        foregroundCwd: process.cwd(),
        paneId,
        terminalId: paneId === "wB:p-agy" ? "term_agy" : "term_pi",
        workspaceId: "wB",
      }),
      socketPath,
      stores: {
        agentEvents: harness.agentEvents,
        agents: harness.agents,
        herdrSessions: harness.herdrSessions,
        herdrWorkspaces: harness.herdrWorkspaces,
      },
    });
    await server.start();
    servers.push(server);

    const agyHookClient = await RpcTestClient.connect(socketPath);
    await agyHookClient.request("agent.orchestrator.register", {
      herdrSocketPath: "/tmp/herdr-default.sock",
      paneId: "wB:p-agy",
      sessionRef: {
        agent: "agy",
        kind: "id",
        source: "agy-hook",
        value: "11111111-2222-3333-4444-555555555555",
      },
      subscriberId: "agy:11111111-2222-3333-4444-555555555555",
      subscriberKind: "agy",
      workspaceId: "wB",
    });
    await agyHookClient.request("agent.orchestrator.set", { enabled: true });
    // Close the short-lived hook socket and wait past disconnectGraceMs (25ms).
    agyHookClient.close();
    await new Promise((resolve) => setTimeout(resolve, 60));

    // Ownership and terminal presence must stay active because the Herdr pane is still open.
    expect(
      server.isTerminalConnected({ herdrSessionName: "default", terminalId: "term_agy" }),
    ).toBe(true);
    expect(
      orchestrator.status({ herdrSessionName: "default", workspaceId: "wB" })?.owner?.terminalId,
    ).toBe("term_agy");

    // Production Herdr pane.closed carries pane_generation (no terminal_id); both owner and agy lease must clear.
    await index.handleHerdrEvent({
      event: {
        pane_generation: "gen-agy-1",
        pane_id: "wB:p-agy",
        type: "pane.closed",
      },
      herdrSessionName: "default",
      sessionDir: "/tmp/herdr-default",
      socketPath: "/tmp/herdr-default.sock",
    });
    expect(
      server.isTerminalConnected({ herdrSessionName: "default", terminalId: "term_agy" }),
    ).toBe(false);
    expect(
      orchestrator.status({ herdrSessionName: "default", workspaceId: "wB" })?.owner,
    ).toBeNull();

    // Now pi claims ownership on wB:p-pi, and a CLI status check with subscriberKind="agy" runs in wB:p-pi.
    const piClient = await RpcTestClient.connect(socketPath);
    await piClient.request("agent.orchestrator.register", {
      herdrSocketPath: "/tmp/herdr-default.sock",
      paneId: "wB:p-pi",
      sessionRef: {
        agent: "pi",
        kind: "path",
        source: "herdr:pi",
        value: "/tmp/pi-session.jsonl",
      },
      subscriberId: "pi:term_pi",
      subscriberKind: "pi",
      workspaceId: "wB",
    });
    await piClient.request("agent.orchestrator.set", { enabled: true });
    expect(
      orchestrator.status({ herdrSessionName: "default", workspaceId: "wB" })?.owner?.terminalId,
    ).toBe("term_pi");

    const cliInPiPane = await RpcTestClient.connect(socketPath);
    await cliInPiPane.request("agent.orchestrator.register", {
      herdrSocketPath: "/tmp/herdr-default.sock",
      paneId: "wB:p-pi",
      sessionRef: {
        agent: "agy",
        kind: "id",
        source: "herdsman-cli",
        value: "herdsman-cli",
      },
      subscriberId: "cli:wB:p-pi",
      subscriberKind: "agy",
      workspaceId: "wB",
    });
    cliInPiPane.close();

    // When piClient disconnects, the CLI's subscriberKind="agy" registration must NOT block pi's disconnect grace release.
    piClient.close();
    await new Promise((resolve) => setTimeout(resolve, 60));
    expect(server.isTerminalConnected({ herdrSessionName: "default", terminalId: "term_pi" })).toBe(
      false,
    );
    expect(
      orchestrator.status({ herdrSessionName: "default", workspaceId: "wB" })?.owner,
    ).toBeNull();

    harness.sqlite.close();
  });
});
