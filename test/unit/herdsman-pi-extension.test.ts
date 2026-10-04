import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, test, vi } from "vitest";
import type {
  AgentEventWireRecord,
  AgentWorkspaceContextSnapshot,
  DaemonStreamMessage,
} from "../../packages/herdsman-pi/src/daemon-client.js";

const extensionModuleUrl = new URL("../../packages/herdsman-pi/src/index.ts", import.meta.url).href;

type Handler = (...args: unknown[]) => unknown;
type Command = {
  description: string;
  getArgumentCompletions?(prefix: string): Array<{ label: string; value: string }> | null;
  handler(args: string, ctx: ReturnType<typeof fakeCtx>): Promise<void>;
};

type Module = {
  createHerdsmanPiExtension: (options?: {
    clientFactory?: () => FakeClient;
    onTurnCompletionSignal?: (completion: Promise<void>) => void;
    onStateExposed?: (state: {
      pendingEvents: AgentEventWireRecord[];
      presentedEventIds: Set<number>;
    }) => void;
    wakeFilter?: { enabled: boolean; extraPatterns: readonly string[] };
  }) => (pi: FakePi) => void;
  defaultSocketPath: () => string;
  formatHiddenAgentUpdates: (
    events: Array<{ id: number; type: string; payload: unknown }>,
  ) => string;
  classifyAckFailure: (error: unknown) => "terminal" | "resync" | "transient";
  logHerdsmanPi: (level: "info" | "warn" | "error", message: string) => void;
  MAX_ACK_ATTEMPTS: number;
  ACK_BACKOFF_CAP_MS: number;
};

type FakeClient = ReturnType<typeof createFakeClient>;
type FakePi = ReturnType<typeof createFakePi>;

const daemonClientModuleUrl = new URL(
  "../../packages/herdsman-pi/src/daemon-client.ts",
  import.meta.url,
).href;
const jsonLinesModuleUrl = new URL(
  "../../packages/herdsman-pi/src/shared/json-lines.ts",
  import.meta.url,
).href;

describe("herdsman-pi acknowledgement failure classification", () => {
  test.each([
    ["invalidated", "terminal"],
    ["no longer pending", "terminal"],
    ["Only the current orchestrator can acknowledge notifications", "terminal"],
    ["Only the next pending orchestrator event can be acknowledged", "resync"],
    ["an unknown daemon failure", "transient"],
  ])("classifies %s as %s", async (message, expected) => {
    const { classifyAckFailure } = (await import(extensionModuleUrl)) as Module;
    expect(classifyAckFailure(new Error(message))).toBe(expected);
  });

  test.each([
    ["ORCHESTRATOR_NOT_OWNER", "terminal"],
    ["ORCHESTRATOR_EVENT_INVALIDATED", "terminal"],
    ["ORCHESTRATOR_EVENT_FAILED", "terminal"],
    ["ORCHESTRATOR_EVENT_ALREADY_ACKED", "terminal"],
    ["ORCHESTRATOR_EVENT_NOT_IN_SCOPE", "terminal"],
    ["ORCHESTRATOR_EVENT_OUT_OF_ORDER", "resync"],
    ["ORCHESTRATOR_OWNER_REPLACED", "terminal"],
    ["ORCHESTRATOR_EVENT_NOT_FOUND", "terminal"],
    ["ORCHESTRATOR_BUSY", "transient"],
    ["ORCHESTRATOR_CONNECTION_LOST", "transient"],
    ["ORCHESTRATOR_RECONCILING", "transient"],
    ["ORCHESTRATOR_ACK_TIMEOUT", "transient"],
  ])("maps structured code %s to %s", async (code, expected) => {
    const { classifyAckFailure } = (await import(extensionModuleUrl)) as Module;
    expect(classifyAckFailure(Object.assign(new Error("legacy"), { code }))).toBe(expected);
  });
  test("structured error codes take precedence over the message", async () => {
    const { classifyAckFailure } = (await import(extensionModuleUrl)) as Module;
    expect(
      classifyAckFailure(Object.assign(new Error("invalidated"), { code: "temporary_failure" })),
    ).toBe("transient");
    expect(
      classifyAckFailure(Object.assign(new Error("temporary"), { code: "event_invalidated" })),
    ).toBe("terminal");
  });
});
describe("herdsman-pi extension self-contained loading", () => {
  test("loads daemon-client independently and rejects frames larger than 1 MiB before handlers see them", async () => {
    const { ReconnectingDaemonClient } = await import(daemonClientModuleUrl);
    expect(typeof ReconnectingDaemonClient).toBe("function");

    const { JsonLineDecoder, JsonLineFrameTooLargeError } = await import(jsonLinesModuleUrl);
    const decoder = new JsonLineDecoder();
    const handler = vi.fn();

    expect(() => {
      for (const message of decoder.push(`${"x".repeat(1024 * 1024 + 1)}\n`)) {
        handler(message);
      }
    }).toThrow(JsonLineFrameTooLargeError);
    expect(handler).not.toHaveBeenCalled();
  });
});

describe("herdsman-pi orchestrator bridge", () => {
  test("defaults to the Herdsman daemon socket", async () => {
    const { defaultSocketPath } = (await import(extensionModuleUrl)) as Module;
    const previousHome = process.env.HERDSMAN_HOME;
    process.env.HERDSMAN_HOME = "/tmp/herdsman-home";
    try {
      expect(defaultSocketPath()).toBe("/tmp/herdsman-home/herdsman.sock");
    } finally {
      process.env.HERDSMAN_HOME = previousHome;
    }
  });

  test("sanitizes zero-width and bidi characters and preserves wake structure in formatHiddenAgentUpdates", async () => {
    const { formatHiddenAgentUpdates } = (await import(extensionModuleUrl)) as Module;
    const updates = formatHiddenAgentUpdates([
      event(1, "term_agent", {
        compactHistory: {
          lastAssistantMessage: {
            text: "secret sk-\u200bant-api03-sample-key-12345678\u202e-injection",
          },
        },
        paneId: "wB:p1",
        payload: { agent: "codex", name: "worker" },
        type: "agent.done",
      }),
      event(2, "term_agent2", {
        compactHistory: {},
        paneId: "wB:p2",
        payload: { agent: "claude" },
        type: "agent.blocked",
      }),
    ]);

    const lines = updates.split("\n");
    expect(lines[0]).toBe("[HERDSMAN AGENT UPDATES]");
    expect(lines[1]).toBe("- agent.done worker · Codex wB:p1");
    expect(lines[2]).toBe("  last assistant: secret sk-[REDACTED]");
    expect(lines[3]).toBe("  event: 1");
    expect(lines[4]).toBe("- agent.blocked Claude wB:p2");
    expect(lines[5]).toBe("  last assistant: ");
    expect(lines[6]).toBe("  event: 2");
    expect(lines.length).toBe(7);
    expect(updates).not.toContain("sk-\u200bant-api03");
    expect(updates).not.toContain("\u202e");
    expect(updates).not.toContain("sample-key");
  });

  test("does not connect outside a complete Herdr environment", async () => {
    const pi = createFakePi();
    let clients = 0;
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({
      clientFactory: () => {
        clients += 1;
        return createFakeClient();
      },
    })(pi);
    const ctx = fakeCtx();

    const previous = {
      HERDR_ENV: process.env.HERDR_ENV,
      HERDR_PANE_ID: process.env.HERDR_PANE_ID,
      HERDR_SOCKET_PATH: process.env.HERDR_SOCKET_PATH,
      HERDR_WORKSPACE_ID: process.env.HERDR_WORKSPACE_ID,
    };
    delete process.env.HERDR_ENV;
    delete process.env.HERDR_PANE_ID;
    delete process.env.HERDR_SOCKET_PATH;
    delete process.env.HERDR_WORKSPACE_ID;
    try {
      await pi.emit("session_start", {}, ctx);
      expect(clients).toBe(0);
      expect(ctx.statuses.get("herdsman")).toBeUndefined();
      await pi.command("", ctx);
      expect(ctx.notifications.at(-1)).toEqual(["Herdsman requires a Herdr workspace", "error"]);
    } finally {
      restoreEnv(previous);
    }

    const herdrPrevious = withHerdrEnv();
    delete process.env.HERDR_PANE_ID;
    try {
      await pi.emit("session_start", {}, ctx);
      expect(clients).toBe(0);
    } finally {
      restoreEnv(herdrPrevious);
    }
  });

  test("registers presence, adopts daemon location, and reconnects", async () => {
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register" || method === "agent.orchestrator.get") {
        return connectionResponse({ paneId: "wC:p3", workspaceId: "wC" });
      }
      return { accepted: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv({ paneId: "wB:p1", workspaceId: "wB" });
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      expect(client.calls[0]).toEqual([
        "agent.orchestrator.register",
        {
          herdrSocketPath: "/tmp/herdr.sock",
          paneId: "wB:p1",
          sessionRef: {
            agent: "pi",
            kind: "path",
            source: "herdr:pi",
            value: "/tmp/pi-session.jsonl",
          },
          subscriberId: "pi-session",
          subscriberKind: "pi",
          workspaceId: "wB",
        },
      ]);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman");

      const callsBeforeTurnEvents = [...client.calls];
      await pi.emit("tool_execution_start", {
        input: "token=abc",
        toolCallId: "tool-1",
        toolName: "bash",
      });
      await pi.emit("tool_result", {
        content: "failed token=abc",
        isError: true,
        toolCallId: "tool-1",
        toolName: "bash",
        turnId: "turn-1",
      });
      await pi.emit("message_end", {
        message: {
          content: [{ text: "completed", type: "text" }],
          role: "assistant",
          stopReason: "stop",
          turnId: "turn-1",
        },
      });
      // The final assistant message ended, so the extension signals turn
      // completion. With no session file present the write cannot be confirmed
      // and the signal still goes out with confirmed=false.
      expect(client.calls).toEqual([
        ...callsBeforeTurnEvents,
        [
          "agent.turn.completed",
          {
            confirmed: false,
            expectedText: "completed",
            herdrSessionName: "default",
            paneId: "wC:p3",
            terminalId: "term_pi",
            workspaceId: "wC",
          },
        ],
      ]);

      await client.connect();
      expect(
        client.calls.filter(([method]) => method === "agent.orchestrator.register").at(-1),
      ).toEqual([
        "agent.orchestrator.register",
        expect.objectContaining({ paneId: "wC:p3", workspaceId: "wC" }),
      ]);
    } finally {
      restoreEnv(previous);
    }
  });

  test("acknowledges owner updates in ID order only after a final assistant response settles", async () => {
    vi.useFakeTimers();
    const pending = [event(42, "term_agent"), event(41, "term_agent")];
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register") return connectionResponse({ events: pending });
      if (method === "agent.orchestrator.get") return connectionResponse();
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const { createHerdsmanPiExtension, formatHiddenAgentUpdates } = (await import(
      extensionModuleUrl
    )) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      client.emitStream({ method: "agent.event", params: { event: event(43, "term_agent") } });
      client.emitStream({ method: "agent.event", params: { event: event(44, "term_pi") } });
      client.emitStream({ method: "agent.event", params: { event: event(45, null) } });

      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman · 3 agent updates");
      expect(ctx.widgets.size).toBe(0);
      expect(formatHiddenAgentUpdates([event(1, "term_agent")])).toContain(
        "[HERDSMAN AGENT UPDATES]",
      );

      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      // The injected wake content reaches the transcript, which is what
      // authorises the acknowledgement below — the turn outcome alone never does.
      await pi.emit("message_end", wakeConsumptionEvidence([41, 42, 43]), ctx);
      expect(await pi.emitContext([], ctx)).toEqual([]);
      expect(client.calls.some(([method]) => method === "agent.notifications.ack")).toBe(false);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman · 3 agent updates");

      await pi.emit(
        "message_end",
        {
          message: {
            content: [{ type: "thinking", text: "hidden reasoning" }],
            role: "assistant",
            stopReason: "stop",
            turnId: "turn-1",
          },
        },
        ctx,
      );
      expect(client.calls.some(([method]) => method === "agent.notifications.ack")).toBe(false);
      await pi.emit("agent_settled", {}, ctx);

      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: 41 }],
        ["agent.notifications.ack", { eventId: 42 }],
        ["agent.notifications.ack", { eventId: 43 }],
      ]);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman");
      expect(ctx.widgets.size).toBe(0);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test.each([
    [undefined, false],
    ["error", false],
    ["aborted", true],
    ["toolUse", false],
  ])("handles final assistant stop reason %s", async (stopReason, abortedByUser) => {
    vi.useFakeTimers();
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register") {
        return connectionResponse({ events: [event(51, "term_agent")] });
      }
      if (method === "agent.orchestrator.get") return connectionResponse();
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      if (stopReason === "aborted") {
        // A user abort still counts as a carried-out delivery once the content
        // reached the transcript: the evidence authorises the acknowledgement,
        // not the turn outcome.
        await pi.emit("message_end", wakeConsumptionEvidence([51]), ctx);
      }
      if (stopReason) await pi.emit("message_end", assistantMessage(stopReason), ctx);
      else await pi.emit("message_end", { message: { role: "user" } }, ctx);
      await pi.emit("agent_settled", {}, ctx);

      expect(ctx.statuses.get("herdsman")).toBe(
        abortedByUser ? "◆ Herdsman" : "◆ Herdsman · 1 agent update",
      );
      if (abortedByUser) {
        expect(
          client.calls.filter(([method]) => method === "agent.notifications.ack"),
        ).toHaveLength(1);
        expect(ctx.notifications.at(-1)).not.toEqual([
          "Herdsman couldn’t acknowledge agent updates · updates remain pending",
          "warning",
        ]);
      } else {
        // No consumption evidence, so nothing is confirmed: the delivery is
        // dead-lettered instead of being confirmed on the turn heuristic, and the
        // notice it raises is the specific one — not the generic "couldn't
        // acknowledge" one a blocked prefix would raise.
        expect(ctx.notifications).toContainEqual([
          "Herdsman · 1 agent update could not be delivered by a wake turn · updates retained unacked in daemon: read the agent directly for the details, or hand this workspace to another terminal so the daemon delivers it there",
          "warning",
        ]);
      }
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("retains only unacknowledged events after a partial acknowledgement failure", async () => {
    vi.useFakeTimers();
    const client = createFakeClient();
    client.response = (method, params) => {
      if (method === "agent.orchestrator.register") {
        return connectionResponse({ events: [event(61, "term_agent"), event(62, "term_agent")] });
      }
      if (method === "agent.orchestrator.get") return connectionResponse();
      if (method === "agent.list") return agentListResponse();
      if (method === "agent.notifications.ack" && (params as { eventId: number }).eventId === 62) {
        throw new Error("ack failed");
      }
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([61, 62]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);

      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: 61 }],
        ["agent.notifications.ack", { eventId: 62 }],
      ]);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman · 1 agent update");
      expect(ctx.widgets.size).toBe(0);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("refreshes the footer after each successful acknowledgement", async () => {
    vi.useFakeTimers();
    let releaseSecondAck: (() => void) | undefined;
    const secondAck = new Promise<void>((resolve) => {
      releaseSecondAck = resolve;
    });
    const client = createFakeClient();
    client.response = (method, params) => {
      if (method === "agent.orchestrator.register") {
        return connectionResponse({ events: [event(61, "term_agent"), event(62, "term_agent")] });
      }
      if (method === "agent.orchestrator.get") return connectionResponse();
      if (method === "agent.list") return agentListResponse();
      if (method === "agent.notifications.ack" && (params as { eventId: number }).eventId === 62) {
        return secondAck;
      }
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      ctx.setIdle(true);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(500);
      vi.runAllTicks();
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([61, 62]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      const settling = pi.emit("agent_settled", {}, ctx);
      for (let index = 0; index < 10; index += 1) await Promise.resolve();

      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman · 1 agent update");

      releaseSecondAck?.();
      await settling;
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman");
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("retains all events after a full acknowledgement failure", async () => {
    vi.useFakeTimers();
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register") {
        return connectionResponse({ events: [event(63, "term_agent"), event(64, "term_agent")] });
      }
      if (method === "agent.orchestrator.get") return connectionResponse();
      if (method === "agent.list") return agentListResponse();
      if (method === "agent.notifications.ack") throw new Error("ack failed");
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([63, 64]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);

      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: 63 }],
        ["agent.notifications.ack", { eventId: 64 }],
      ]);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman · 2 agent updates");
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("invalidates a delivered batch on role loss without aborting a normal turn", async () => {
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register") {
        return connectionResponse({ events: [event(71, "term_agent")] });
      }
      if (method === "agent.orchestrator.get") return connectionResponse();
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: roleChange("term_pi", "term_other", "wB:p-other") },
      });
      await pi.emit("agent_settled", {}, ctx);

      expect(ctx.aborts).toBe(0);
      expect(client.calls.some(([method]) => method === "agent.notifications.ack")).toBe(false);
    } finally {
      restoreEnv(previous);
    }
  });

  test("keeps context and updates disabled for a non-owner", async () => {
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register" || method === "agent.orchestrator.get") {
        return connectionResponse({
          events: [event(9, "term_agent")],
          ownerTerminalId: "term_other",
        });
      }
      if (method === "agent.list") return agentListResponse();
      return { accepted: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      client.emitStream({ method: "agent.event", params: { event: event(10, "term_agent") } });
      await pi.emit("agent_start", {}, ctx);

      expect(await pi.emitContext([], ctx)).toEqual([]);
      expect(client.calls.some(([method]) => method === "agent.list")).toBe(false);
      expect(client.calls.some(([method]) => method === "agent.notifications.ack")).toBe(false);
    } finally {
      restoreEnv(previous);
    }
  });

  test("implements direct local command parsing and status messages", async () => {
    const client = createFakeClient();
    let current = connectionResponse({ ownerTerminalId: null });
    client.response = (method, params) => {
      if (method === "agent.orchestrator.register" || method === "agent.orchestrator.get") {
        return current;
      }
      if (method === "agent.orchestrator.set") {
        const enabled = (params as { enabled: boolean }).enabled;
        if (enabled) current = connectionResponse({ changed: true });
        else if (current.state.owner?.terminalId === "term_pi") {
          current = connectionResponse({ changed: true, ownerTerminalId: null });
        } else current = { ...current, changed: false };
        return current;
      }
      return {};
    };
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await pi.command("on", ctx);
      expect(ctx.notifications.at(-1)).toEqual([
        "Herdsman is reconnecting · try again shortly",
        "warning",
      ]);
      await client.connect();

      expect(pi.commands.get("herdsman")?.description).toBe(
        "Watch Herdsman agent updates in this Pi",
      );
      expect(pi.commands.get("herdsman")?.getArgumentCompletions?.("")).toEqual([
        { label: "on", value: "on" },
        { label: "off", value: "off" },
        { label: "status", value: "status" },
      ]);

      await pi.command("", ctx);
      expect(ctx.notifications.at(-1)).toEqual(["Herdsman is off", "info"]);
      await pi.command("status", ctx);
      expect(ctx.notifications.at(-1)).toEqual(["Herdsman is off", "info"]);

      await pi.command("  on  ", ctx);
      expect(client.calls).toContainEqual(["agent.orchestrator.set", { enabled: true }]);
      expect(ctx.notifications.at(-1)).toEqual([
        "Herdsman is watching agent updates · default/wB · wB:p1",
        "info",
      ]);
      await pi.command("status", ctx);
      expect(ctx.notifications.at(-1)).toEqual([
        "Herdsman is watching agent updates · default/wB · wB:p1",
        "info",
      ]);

      await pi.command("off", ctx);
      expect(ctx.notifications.at(-1)).toEqual(["Herdsman is off", "info"]);

      current = connectionResponse({ ownerTerminalId: "term_other" });
      await pi.command("status", ctx);
      expect(ctx.notifications.at(-1)).toEqual(["Herdsman is off", "info"]);
      await pi.command("off", ctx);
      expect(current.state.owner?.terminalId).toBe("term_other");
      expect(ctx.notifications.at(-1)).toEqual(["Herdsman is off", "info"]);

      await pi.command("orchestrator on", ctx);
      expect(ctx.notifications.at(-1)).toEqual([USAGE, "warning"]);
      await pi.command("unknown", ctx);
      expect(ctx.notifications.at(-1)).toEqual([USAGE, "warning"]);
    } finally {
      restoreEnv(previous);
    }
  });

  test("notifies only a replaced owner and suppresses duplicate self-off stream feedback", async () => {
    const client = createFakeClient();
    let current = connectionResponse();
    client.response = async (method, params) => {
      if (method === "agent.orchestrator.register" || method === "agent.orchestrator.get") {
        return current;
      }
      if (
        method === "agent.orchestrator.set" &&
        (params as { enabled: boolean }).enabled === false
      ) {
        const change = roleChange("term_pi", null);
        current = connectionResponse({ changed: true, ownerTerminalId: null });
        client.emitStream({ method: "agent.orchestrator.changed", params: { change } });
        return current;
      }
      return current;
    };
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: roleChange("term_pi", "term_other", "wB:p-other") },
      });
      expect(ctx.notifications.at(-1)).toEqual(["Herdsman is off · moved to wB:p-other", "info"]);
      expect(ctx.statuses.get("herdsman")).toBeUndefined();

      current = connectionResponse();
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: roleChange("term_other", "term_pi") },
      });
      await tick();
      ctx.notifications.length = 0;
      await pi.command("off", ctx);
      expect(ctx.notifications).toEqual([["Herdsman is off", "info"]]);
      expect(ctx.statuses.get("herdsman")).toBeUndefined();
      expect(ctx.statuses.has("herdsman-connection")).toBe(false);
      expect(ctx.statuses.has("herdsman-orchestrator")).toBe(false);
    } finally {
      restoreEnv(previous);
    }
  });

  test("contains registration failures and shows reconnecting state", async () => {
    const client = createFakeClient();
    client.response = () => {
      throw new Error("registration failed");
    };
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await expect(pi.emit("session_start", {}, ctx)).resolves.toBeUndefined();
      await expect(client.connect()).resolves.toBeUndefined();
      expect(ctx.statuses.get("herdsman")).toBeUndefined();
      expect(ctx.statuses.has("herdsman-connection")).toBe(false);
      expect(ctx.statuses.has("herdsman-orchestrator")).toBe(false);
    } finally {
      restoreEnv(previous);
    }
  });

  test("shows reconnecting only for a previous owner and restores it without feedback", async () => {
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx();
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman");

      client.disconnect();
      expect(ctx.statuses.get("herdsman")).toBe("◇ Herdsman · reconnecting");
      expect(ctx.statuses.has("herdsman-connection")).toBe(false);
      expect(ctx.statuses.has("herdsman-orchestrator")).toBe(false);

      await client.connect();
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman");
      expect(ctx.notifications).toEqual([]);
    } finally {
      restoreEnv(previous);
    }
  });

  test("keeps a previous owner reconnecting across repeated registration failure callbacks", async () => {
    let registrations = 0;
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register") {
        registrations += 1;
        if (registrations > 1) throw new Error("registration failed");
        return connectionResponse();
      }
      if (method === "agent.orchestrator.get") return connectionResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx();
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.disconnect();
      expect(ctx.statuses.get("herdsman")).toBe("◇ Herdsman · reconnecting");

      await client.connect();

      expect(ctx.statuses.get("herdsman")).toBe("◇ Herdsman · reconnecting");
      expect(ctx.notifications).toEqual([]);
    } finally {
      restoreEnv(previous);
    }
  });

  test("keeps the footer absent when a non-owner disconnects", async () => {
    const client = createFakeClient();
    client.response = (method) =>
      method === "agent.orchestrator.register" || method === "agent.orchestrator.get"
        ? connectionResponse({ ownerTerminalId: "term_other" })
        : { acknowledged: true };
    const pi = createFakePi();
    const ctx = fakeCtx();
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      expect(ctx.statuses.get("herdsman")).toBeUndefined();

      client.disconnect();
      expect(ctx.statuses.get("herdsman")).toBeUndefined();
    } finally {
      restoreEnv(previous);
    }
  });

  test.each([
    ["term_other", "Herdsman is off · moved to wB:p-other"],
    [null, "Herdsman is off"],
  ])("reports ownership loss discovered on reconnect to %s", async (ownerTerminalId, message) => {
    let current = connectionResponse();
    const client = createFakeClient();
    client.response = (method) =>
      method === "agent.orchestrator.register" || method === "agent.orchestrator.get"
        ? current
        : { acknowledged: true };
    const pi = createFakePi();
    const ctx = fakeCtx();
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.disconnect();
      expect(ctx.statuses.get("herdsman")).toBe("◇ Herdsman · reconnecting");

      current = connectionResponse({ ownerTerminalId });
      await client.connect();

      expect(ctx.statuses.get("herdsman")).toBeUndefined();
      expect(ctx.notifications.at(-1)).toEqual([message, "info"]);
    } finally {
      restoreEnv(previous);
    }
  });

  test("refreshes pending state when the owner moves to another workspace", async () => {
    const client = createFakeClient();
    client.response = (method) =>
      method === "agent.orchestrator.get"
        ? connectionResponse({
            events: [event(77, "term_agent")],
            paneId: "wC:p3",
            workspaceId: "wC",
          })
        : connectionResponse();
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: {
          change: {
            ...roleChange("term_pi", "term_pi", "wC:p3"),
            current: {
              ...roleChange("term_pi", "term_pi", "wC:p3").current,
              workspaceId: "wC",
            },
          },
        },
      });
      await tick();

      expect(client.calls).toContainEqual(["agent.orchestrator.get", {}]);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman · 1 agent update");
    } finally {
      restoreEnv(previous);
    }
  });

  test.each([
    ["agent.done", {}],
    ["agent.blocked", {}],
    ["agent.idle", { from: "working", to: "idle" }],
  ])("wakes immediately on idle for %s", async (type, payload) => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      expect(pi.messageRenderers.has("herdsman-wake")).toBe(true);
      client.emitStream({
        method: "agent.event",
        params: {
          event: event(43, "term_agent", { payload: { name: "reviewer", ...payload }, type }),
        },
      });

      // 空闲直投：不再等待 settle 窗口，0ms 微任务内完成注入。
      expect(pi.hiddenMessages).toEqual([]);
      await vi.advanceTimersByTimeAsync(0);
      expect(pi.customMessages).toEqual([]);
      expect(pi.hiddenMessages).toEqual([
        [
          {
            content: expect.stringContaining("reviewer · Claude"),
            customType: "herdsman-wake-context",
            details: { eventIds: [43], presentedEventIds: [43] },
            display: false,
          },
          { deliverAs: "followUp", triggerTurn: true },
        ],
      ]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("does not reenter wake scheduling from synchronous sendMessage callbacks", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      const send = pi.sendMessage;
      pi.sendMessage = (message, options) => {
        send?.call(pi, message, options);
        if ((message as { customType?: string }).customType === "herdsman-wake-context") {
          client.emitStream({
            method: "agent.event",
            params: {
              event: event(44, "term_agent", { payload: { name: "nested" }, type: "agent.done" }),
            },
          });
        }
      };
      client.emitStream({
        method: "agent.event",
        params: {
          event: event(43, "term_agent", { payload: { name: "reviewer" }, type: "agent.done" }),
        },
      });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });
  test("ignores non-outcomes, done-to-idle duplicates, null-terminal, and self events", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      for (const candidate of [
        event(44, "term_agent", { type: "agent.status.changed" }),
        event(45, "term_agent", { type: "agent.tool.failed" }),
        event(46, "term_agent", { payload: { from: "done", to: "idle" }, type: "agent.idle" }),
        event(47, null),
        event(48, "term_pi"),
      ]) {
        client.emitStream({ method: "agent.event", params: { event: candidate } });
      }
      await vi.advanceTimersByTimeAsync(1_000);

      expect(pi.hiddenMessages).toEqual([]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("coalesces multiple outcomes into one hidden wake context", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(51, "term_agent") } });
      client.emitStream({
        method: "agent.event",
        params: { event: event(52, "term_other", { type: "agent.blocked" }) },
      });
      await vi.advanceTimersByTimeAsync(500);

      expect(pi.hiddenMessages).toMatchObject([
        [
          {
            content: expect.stringContaining("HERDSMAN AGENT UPDATES"),
            customType: "herdsman-wake-context",
            details: { eventIds: [51, 52] },
            display: false,
          },
          { deliverAs: "followUp", triggerTurn: true },
        ],
      ]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("retains pending outcomes when wake preparation fails", async () => {
    vi.useFakeTimers();
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register") return connectionResponse();
      if (method === "agent.orchestrator.get") throw new Error("refresh failed");
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(53, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);

      expect(pi.hiddenMessages).toEqual([]);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman · 1 agent update");
      expect(ctx.notifications.at(-1)).toEqual([
        "Herdsman couldn’t load agent updates · updates remain pending",
        "warning",
      ]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("defers a busy wake until Pi settles idle", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: false });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(61, "term_agent") } });
      await vi.advanceTimersByTimeAsync(1_000);
      expect(pi.hiddenMessages).toEqual([]);

      ctx.setIdle(true);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("retries a refused injection and keeps the busy gate closed", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let rejectNext = true;
    const deliver = pi.sendMessage;
    pi.sendMessage = (message: unknown, options?: unknown) => {
      if (rejectNext) {
        rejectNext = false;
        throw new Error("pi refused the hidden message");
      }
      deliver.call(pi, message, options);
    };
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(65, "term_agent") } });

      // The idle orchestrator is woken, but Pi refuses the message. Nothing was
      // delivered, and the event stays pending and eligible for redelivery.
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toEqual([]);

      // A later event still wakes normally once Pi accepts a message again.
      client.emitStream({ method: "agent.event", params: { event: event(66, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[1]).toEqual({ deliverAs: "followUp", triggerTurn: true });

      // A busy orchestrator is never waked into, whatever happened before: the
      // gate is a property of the current pass, not of an elapsed deadline.
      ctx.setIdle(false);
      client.emitStream({ method: "agent.event", params: { event: event(67, "term_agent") } });
      await vi.advanceTimersByTimeAsync(5_000);
      expect(pi.hiddenMessages).toHaveLength(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("dead-letters a delivery with no consumption evidence instead of driving an empty marker turn", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const ackedIds = () =>
      client.calls
        .filter(([method]) => method === "agent.notifications.ack")
        .map(([, params]) => (params as { eventId: number }).eventId);
    const deadLetters = () =>
      ctx.notifications.filter(([text]) => String(text).includes("retained unacked in daemon"));
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(67, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);

      // The wake is only ever injected into an idle orchestrator, and it always
      // triggers its own turn, so the copy the transcript gets is one message.
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[1]).toEqual({ deliverAs: "followUp", triggerTurn: true });

      // The run ends without ever carrying the update out. There is deliberately
      // no empty continuation run: a `herdsman-wake-continuation` marker would
      // start a turn whose only content is a prompt, which is what used to flood
      // the session with empty turns.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", assistantMessage("error"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(
        pi.hiddenMessages.filter(
          ([message]) => message.customType === "herdsman-wake-continuation",
        ),
      ).toEqual([]);

      // The write-off is a dead letter: nothing is acknowledged, so the event
      // stays pending in the daemon ...
      expect(ackedIds()).toEqual([]);
      expect(deadLetters()).toHaveLength(1);
      expect(deadLetters()[0]?.[1]).toBe("warning");
      expect(String(deadLetters()[0]?.[0])).toContain("retained unacked in daemon");

      // ... and the update is not locked out of this session: the next wake is
      // still allowed to present it.
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(2);
      expect(
        (pi.hiddenMessages[1]?.[0].details as { presentedEventIds: number[] }).presentedEventIds,
      ).toEqual([67]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("acknowledges a wake exactly once, only after its content reached the transcript", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const ackedIds = () =>
      client.calls
        .filter(([method]) => method === "agent.notifications.ack")
        .map(([, params]) => (params as { eventId: number }).eventId);
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(84, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      // The turn runs to its final response, but that alone is not evidence that
      // the injected content was carried out: the settlement may not confirm it.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([]);

      // The content enters the transcript and the message names the id it
      // presented — the consumption evidence.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([84]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([84]);

      // Confirmed exactly once: a later settlement must not acknowledge it again.
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(5_000);
      expect(ackedIds()).toEqual([84]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("never injects an in-flight event id twice when the daemon redelivers it", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(85, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      // The delivery is in flight until its `message_end` arrives, so the daemon
      // (which still holds the event, nothing was acknowledged) redelivering it
      // must not put the same content into the transcript a second time.
      client.emitStream({ method: "agent.event", params: { event: event(85, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      const injectedIds = pi.hiddenMessages
        .filter(([message]) => message.customType === "herdsman-wake-context")
        .map(([message]) => (message.details as { eventIds: number[] }).eventIds);
      expect(injectedIds).toEqual([[85]]);
      expect(ctx.aborts).toBe(0);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("never acknowledges a larger id past an unconsumed one (the watermark would swallow it)", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const ackedIds = () =>
      client.calls
        .filter(([method]) => method === "agent.notifications.ack")
        .map(([, params]) => (params as { eventId: number }).eventId);
    try {
      await startExtension(client, pi, ctx);
      // Both outcomes are pending at the same time, so they are injected as one
      // message: a single track, one copy per id, never a second one.
      client.emitStream({ method: "agent.event", params: { event: event(95, "term_agent") } });
      client.emitStream({ method: "agent.event", params: { event: event(96, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[1]).toEqual({ deliverAs: "followUp", triggerTurn: true });

      // Only the second id's content is seen in the transcript. The daemon
      // confirms by watermark (`where id <= ?`), so acknowledging 96 would mark
      // 95 acked as well and swallow it for good: the walk must stop at 95 and
      // confirm nothing.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([96], [95, 96]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([]);

      // 95 is dead-lettered instead of being confirmed: nothing was acknowledged
      // for it, so it stays pending and can still be presented again.
      expect(
        ctx.notifications.filter(([text]) => String(text).includes("retained unacked in daemon")),
      ).toHaveLength(1);

      // The first id reaches the transcript too: now the evidence is contiguous
      // and both ids are confirmed.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([95]), ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([96]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([95, 96]);
      await pi.emit("message_end", wakeConsumptionEvidence([96]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([95, 96]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("acknowledges content that reached the transcript even when its turn failed", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const ackedIds = () =>
      client.calls
        .filter(([method]) => method === "agent.notifications.ack")
        .map(([, params]) => (params as { eventId: number }).eventId);
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(97, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      // The run carries the content into the transcript and then ends in an
      // error, so the batch never reports a final response. The evidence is what
      // authorises the acknowledgement: leaving the id pending would pin the
      // daemon watermark behind content the orchestrator has seen.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([97]), ctx);
      await pi.emit("message_end", assistantMessage("error"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([97]);
      expect(
        ctx.notifications.filter(([text]) => String(text).includes("couldn’t acknowledge")),
      ).toEqual([]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("keeps an unconsumed delivery suppressed across a scope change", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const wakeInjections = () =>
      pi.hiddenMessages.filter(([message]) => message.customType === "herdsman-wake-context");
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(99, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(wakeInjections()).toHaveLength(1);

      // The copy is in flight — its `message_end` has not arrived yet — so it can
      // still be sitting in Pi's process-wide follow-up queue (the extension
      // cannot clear it). The workspace moves, which resets the local delivery
      // bookkeeping.
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: movedRoleChange() },
      });
      await vi.advanceTimersByTimeAsync(100);

      // The daemon still holds the event (it was never acknowledged) and redelivers
      // it. Re-presenting it now would put the update into the transcript a second
      // time, because the stale copy can still be drained into this scope, so the
      // id stays suppressed and the redelivery produces no second injection.
      client.emitStream({ method: "agent.event", params: { event: event(99, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(wakeInjections()).toHaveLength(1);
      expect(
        wakeInjections().filter(([message]) =>
          (message.details as { presentedEventIds: number[] }).presentedEventIds.includes(99),
        ),
      ).toHaveLength(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("confirms only the ids a drained copy actually presented", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const ackedIds = () =>
      client.calls
        .filter(([method]) => method === "agent.notifications.ack")
        .map(([, params]) => (params as { eventId: number }).eventId);
    try {
      await startExtension(client, pi, ctx);
      // Both outcomes are pending together, so the wake carries them in one
      // message; 102 is never seen entering the transcript on its own.
      client.emitStream({ method: "agent.event", params: { event: event(102, "term_agent") } });
      client.emitStream({ method: "agent.event", params: { event: event(103, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      // The second copy is drained. Its provenance list still names 102 (everything
      // that was pending), but its *content* only carries 103: confirming 102 here
      // would acknowledge content nobody has seen, so only 103 counts — and even
      // that may not be confirmed while 102 blocks the watermark.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([103], [102, 103]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([]);

      // 102 is dead-lettered instead of being confirmed behind 103's back.
      expect(
        ctx.notifications.filter(([text]) => String(text).includes("retained unacked in daemon")),
      ).toHaveLength(1);

      // The first copy is drained as well: now the evidence is contiguous and both
      // ids are confirmed.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([102]), ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([103]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([102, 103]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("warns instead of confirming when a drained copy carries no usable evidence", async () => {
    vi.useFakeTimers();
    const logHome = mkdtempSync(join(tmpdir(), "herdsman-pi-wake-evidence-"));
    const previousHome = process.env.HERDSMAN_HOME;
    process.env.HERDSMAN_HOME = logHome;
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const ackedIds = () =>
      client.calls
        .filter(([method]) => method === "agent.notifications.ack")
        .map(([, params]) => (params as { eventId: number }).eventId);
    const logPath = join(
      logHome,
      "logs",
      `herdsman-pi-${new Date().toISOString().slice(0, 10).replaceAll("-", "")}.log`,
    );
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(106, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      // A drained copy whose evidence list cannot be read (a shape this extension
      // never emits). The wide provenance list is not a substitute: it names ids
      // whose content this message does not carry, so confirming from it would
      // acknowledge content nobody saw. Nothing is confirmed, and that is not
      // silent: the stuck ids and the message type are logged.
      await pi.emit(
        "message_end",
        {
          message: {
            content: "[HERDSMAN AGENT UPDATES] …",
            customType: "herdsman-wake-context",
            details: { eventIds: [106] },
            display: false,
            role: "custom",
          },
        },
        ctx,
      );
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([]);
      expect(readFileSync(logPath, "utf8")).toContain(
        "wake consumption evidence unusable customType=herdsman-wake-context awaiting=106",
      );

      // The unusable message did not poison the delivery: the dead-letter kept the
      // id pending, and the real evidence for the same id still settles it, so the
      // watermark cannot be pinned by it.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([106]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([106]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
      rmSync(logHome, { recursive: true, force: true });
      if (previousHome === undefined) delete process.env.HERDSMAN_HOME;
      else process.env.HERDSMAN_HOME = previousHome;
    }
  });

  test("delivers one wake update on a single track to the transcript exactly once", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const ackedIds = () =>
      client.calls
        .filter(([method]) => method === "agent.notifications.ack")
        .map(([, params]) => (params as { eventId: number }).eventId);
    const wakeInjections = () =>
      pi.hiddenMessages.filter(([message]) => message.customType === "herdsman-wake-context");
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(105, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);

      // Single track: exactly one transcript entry (the triggered follow-up) and
      // no second copy anywhere else - not on the context hook.
      expect(wakeInjections()).toHaveLength(1);
      expect(wakeInjections()[0]?.[1]).toEqual({ deliverAs: "followUp", triggerTurn: true });
      const contextMessages = (await pi.emitContext([], ctx)) as Array<{
        customType?: string;
      }>;
      expect(contextMessages.some((message) => message.customType === "herdsman-wake-queued")).toBe(
        false,
      );
      expect(
        contextMessages.some((message) => message.customType === "herdsman-wake-context"),
      ).toBe(false);

      // No pin is consumption evidence either: only the drained copy is, so the
      // update may not be acknowledged yet.
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([]);

      // The delivery was dead-lettered and re-presented, so the transcript still
      // holds exactly one wake entry for the id, and it may now be acknowledged
      // once.
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("message_end", wakeConsumptionEvidence([105]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([105]);
      expect(
        wakeInjections().filter(([message]) => String(message.content).includes("event: 105")),
      ).toHaveLength(2);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("delivers a busy multi-event batch in ascending eventId order on the single track", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const wakeInjections = () =>
      pi.hiddenMessages.filter(([message]) => message.customType === "herdsman-wake-context");
    try {
      await startExtension(client, pi, ctx);
      // Three pending outcomes, emitted out of order, from three different panes.
      client.emitStream({
        method: "agent.event",
        params: { event: event(203, "term_third", { paneId: "wB:p-third" }) },
      });
      client.emitStream({
        method: "agent.event",
        params: { event: event(201, "term_agent") },
      });
      client.emitStream({
        method: "agent.event",
        params: {
          event: event(202, "term_other", { paneId: "wB:p-other", type: "agent.blocked" }),
        },
      });
      await vi.advanceTimersByTimeAsync(500);

      // One message, one track: a single triggered follow-up that carries every
      // pending outcome, listed in ascending eventId order. Because there is only
      // one message, the batch cannot arrive shuffled or split.
      expect(wakeInjections()).toHaveLength(1);
      expect(wakeInjections()[0]?.[1]).toEqual({ deliverAs: "followUp", triggerTurn: true });
      const content = String(wakeInjections()[0]?.[0]?.content);
      const first = content.indexOf("event: 201");
      const second = content.indexOf("event: 202");
      const third = content.indexOf("event: 203");
      expect(first).toBeGreaterThanOrEqual(0);
      expect(second).toBeGreaterThan(first);
      expect(third).toBeGreaterThan(second);
      expect(wakeInjections()[0]?.[0]?.details).toEqual({
        eventIds: [201, 202, 203],
        presentedEventIds: [201, 202, 203],
      });

      // Single track means single copy: the context hook adds nothing for it.
      const contextMessages = (await pi.emitContext([], ctx)) as Array<{
        customType?: string;
      }>;
      expect(contextMessages.some((message) => message.customType === "herdsman-wake-queued")).toBe(
        false,
      );
      expect(
        contextMessages.some((message) => message.customType === "herdsman-wake-context"),
      ).toBe(false);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("keeps a consumed id out of the transcript after the scope that consumed it is gone", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState: { presentedEventIds: Set<number> } | undefined;
    const wakeInjections = () =>
      pi.hiddenMessages.filter(([message]) => message.customType === "herdsman-wake-context");
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      // 113 is injected into an idle orchestrator as a triggered follow-up: its
      // copy is in flight, so it can still be sitting in Pi's follow-up queue.
      client.emitStream({ method: "agent.event", params: { event: event(113, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(wakeInjections()).toHaveLength(1);

      // The workspace moves while that copy is still in Pi's follow-up queue: the
      // local guards are reset and the id is carried over as a suppression entry.
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: movedRoleChange() },
      });
      await vi.advanceTimersByTimeAsync(100);

      // The stale copy is drained into the scope that took over, which is where its
      // evidence arrives — after the reset that knew about the id. Consuming it must
      // therefore also make the *current* scope remember the presentation: the event
      // was never acknowledged, so the daemon will redeliver it, and without that
      // guard the same update would enter the transcript a second time.
      await pi.emit("message_end", wakeConsumptionEvidence([113]), ctx);
      expect(extensionState?.presentedEventIds.has(113)).toBe(true);

      client.emitStream({ method: "agent.event", params: { event: event(113, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(wakeInjections()).toHaveLength(1);
      expect(
        wakeInjections().filter(([message]) =>
          (message.details as { presentedEventIds: number[] }).presentedEventIds.includes(113),
        ),
      ).toHaveLength(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("settles a consumed queue that outlived its batch once the daemon is reachable again", async () => {
    vi.useFakeTimers();
    const client = createWakeClient([event(114, "term_agent")]);
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const ackedIds = () =>
      client.calls
        .filter(([method]) => method === "agent.notifications.ack")
        .map(([, params]) => (params as { eventId: number }).eventId);
    try {
      await startExtension(client, pi, ctx);
      await vi.advanceTimersByTimeAsync(0);
      expect(pi.hiddenMessages).toHaveLength(1);

      // The content is in the transcript, then the daemon drops the socket before
      // the settlement can confirm anything: the settlement that follows closes the
      // batch without an acknowledgement (nothing is awaiting consumption), so the
      // event stays in the delivery queue with no batch left to settle it.
      await pi.emit(
        "message_end",
        {
          message: {
            content: "[HERDSMAN AGENT UPDATES] …",
            customType: "herdsman-wake-context",
            details: { eventIds: [114], presentedEventIds: [114] },
            display: false,
            role: "custom",
          },
        },
        ctx,
      );
      client.disconnect(new Error("transient disconnect"));
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([]);

      // Reconnect with no new update: only the queue survived, and a settlement
      // must still be able to confirm what the transcript already holds — otherwise
      // the daemon's watermark would stay pinned until some unrelated event formed
      // a new batch.
      await client.connect();
      await vi.advanceTimersByTimeAsync(0);
      expect(pi.hiddenMessages).toHaveLength(1);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds()).toEqual([114]);

      // Exactly once: later settlements find nothing left to confirm.
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(1_000);
      expect(ackedIds()).toEqual([114]);
      expect(pi.hiddenMessages).toHaveLength(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("logs a skipped redelivery once per id and reason instead of flooding", async () => {
    vi.useFakeTimers();
    const logHome = mkdtempSync(join(tmpdir(), "herdsman-pi-wake-skip-"));
    const previousHome = process.env.HERDSMAN_HOME;
    process.env.HERDSMAN_HOME = logHome;
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const wakeInjections = () =>
      pi.hiddenMessages.filter(([message]) => message.customType === "herdsman-wake-context");
    const logPath = join(
      logHome,
      "logs",
      `herdsman-pi-${new Date().toISOString().slice(0, 10).replaceAll("-", "")}.log`,
    );
    const skipLines = (reason: string) =>
      readFileSync(logPath, "utf8")
        .split("\n")
        .filter(
          (line) =>
            line.includes("wake injection skipped eventId=115") &&
            line.includes(`reason=${reason}`),
        ).length;
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(115, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(wakeInjections()).toHaveLength(1);

      // The daemon redelivers the same event while its copy is already in the
      // orchestrator's hands: the redelivery is skipped (never a second copy), and
      // the skip becomes diagnosable without logging once per attempt — `awaiting`
      // here, because the in-flight copy is what has to reach the transcript.
      for (let round = 0; round < 3; round += 1) {
        client.emitStream({ method: "agent.event", params: { event: event(115, "term_agent") } });
        await vi.advanceTimersByTimeAsync(500);
      }
      expect(wakeInjections()).toHaveLength(1);
      expect(skipLines("awaiting")).toBe(1);

      // Once the copy is drained the same id is skipped as `presented`, which is
      // worth its own line (it describes a different state) instead of repeating the
      // earlier reason.
      await pi.emit("message_end", wakeConsumptionEvidence([115]), ctx);
      client.emitStream({ method: "agent.event", params: { event: event(115, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(wakeInjections()).toHaveLength(1);
      expect(skipLines("awaiting")).toBe(1);
      expect(skipLines("presented")).toBe(1);

      // A role/scope change clears the per-scope presentation guard while the drained
      // copy may still sit in Pi's follow-up queue, so the next redelivery is skipped
      // for a third reason — still once each.
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: movedRoleChange() },
      });
      await vi.advanceTimersByTimeAsync(100);
      client.emitStream({ method: "agent.event", params: { event: event(115, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(wakeInjections()).toHaveLength(1);
      expect(skipLines("suppressed")).toBe(1);
      expect(skipLines("awaiting")).toBe(1);
      expect(skipLines("presented")).toBe(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
      rmSync(logHome, { recursive: true, force: true });
      if (previousHome === undefined) delete process.env.HERDSMAN_HOME;
      else process.env.HERDSMAN_HOME = previousHome;
    }
  });

  test("defers a busy wake until the orchestrator settles, then injects a clean triggerable turn", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: false });
    const previous = withHerdrEnv();
    const ackedIds = () =>
      client.calls
        .filter(([method]) => method === "agent.notifications.ack")
        .map(([, params]) => (params as { eventId: number }).eventId);
    const wakeInjections = () =>
      pi.hiddenMessages.filter(([message]) => message.customType === "herdsman-wake-context");
    try {
      await startExtension(client, pi, ctx);
      await pi.emit("agent_start", {}, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(63, "term_agent") } });

      // Bounded 100ms spin: while the orchestrator stays busy nothing is injected,
      // nothing is parked without a timer, and no deadline releases it either.
      await vi.advanceTimersByTimeAsync(60_000);
      expect(pi.hiddenMessages).toEqual([]);
      expect(ackedIds()).toEqual([]);

      // The queued follow-up the context hook could read adds no second copy.
      const contextMessages = (await pi.emitContext([], ctx)) as Array<{
        customType?: string;
      }>;
      expect(contextMessages.some((message) => message.customType === "herdsman-wake-queued")).toBe(
        false,
      );
      expect(
        contextMessages.some((message) => message.customType === "herdsman-wake-context"),
      ).toBe(false);

      // The turn settles: the wake is injected now, as a follow-up that triggers its
      // own turn — the only variant that runs the message through the agent core
      // and produces a regular `message_end` for it.
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      ctx.setIdle(true);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(0);
      expect(wakeInjections()).toHaveLength(1);
      expect(wakeInjections()[0]?.[1]).toEqual({ deliverAs: "followUp", triggerTurn: true });
      // The injected message carries the real update body, not a marker prompt.
      const content = String(wakeInjections()[0]?.[0]?.content);
      expect(content).toContain("[HERDSMAN WAKE POLICY]");
      expect(content).toContain("event: 63");
      expect(content).toContain("last assistant: done");
      expect(wakeInjections()[0]?.[0].details as { presentedEventIds: number[] }).toMatchObject({
        eventIds: [63],
        presentedEventIds: [63],
      });

      // A wake turn of our own, so nothing else was interrupted on the way there.
      expect(ctx.aborts).toBe(0);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("keeps a busy wake deferred instead of releasing it on a hard timeout", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: false });
    const previous = withHerdrEnv();
    const wakeInjections = () =>
      pi.hiddenMessages.filter(([message]) => message.customType === "herdsman-wake-context");
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(64, "term_agent") } });

      // No deadline force-releases the batch into a busy orchestrator: the spin
      // keeps re-checking `isIdle` instead, so a wake can only ever be injected
      // once the turn that is streaming has settled.
      await vi.advanceTimersByTimeAsync(120_000);
      expect(wakeInjections()).toEqual([]);

      // Only the settlement (or the spin noticing an idle orchestrator) releases it.
      ctx.setIdle(true);
      await vi.advanceTimersByTimeAsync(500);
      expect(wakeInjections()).toHaveLength(1);
      expect(wakeInjections()[0]?.[1]).toEqual({ deliverAs: "followUp", triggerTurn: true });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("merges an unconfirmed batch into the next wake instead of dropping or duplicating it", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const base = client.response;
    const ackedIds: number[] = [];
    client.response = (method, params) => {
      if (method === "agent.notifications.ack") {
        const { eventId } = params as { eventId: number };
        ackedIds.push(eventId);
        // The daemon answers with the advanced watermark for every accepted id.
        return { acknowledged: true, ackedEventId: eventId };
      }
      return base(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);

      // 1) Idle orchestrator: the first event is delivered immediately.
      client.emitStream({ method: "agent.event", params: { event: event(81, "term_agent") } });
      await vi.advanceTimersByTimeAsync(0);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(String(pi.hiddenMessages[0]?.[0]?.content)).toContain("event: 81");
      await pi.emit("agent_start", {}, ctx);
      // The wake turn keeps running, so the delivered batch stays unconfirmed.
      ctx.setIdle(false);

      // 2) A second event arrives while that batch is still unconfirmed. Nothing is
      // released into the running turn: the wake waits for the settlement.
      client.emitStream({ method: "agent.event", params: { event: event(82, "term_agent") } });
      await vi.advanceTimersByTimeAsync(1_000);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(ackedIds).toEqual([]);

      // 3) The settlement cannot confirm the undelivered 81, so it is dead-lettered
      // (still pending in the daemon) instead of being dropped or acknowledged.
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds).toEqual([]);
      expect(
        ctx.notifications.filter(([text]) => String(text).includes("retained unacked in daemon")),
      ).toHaveLength(1);

      // 4) The next wake re-presents only the fresh event, and its ids still cover
      // everything that was pending — the provenance list is wider than the content.
      ctx.setIdle(true);
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(2);
      const nextWake = pi.hiddenMessages[1]?.[0];
      expect(String(nextWake?.content)).toContain("event: 81");
      expect(String(nextWake?.content)).toContain("event: 82");
      expect(pi.hiddenMessages[1]?.[1]).toEqual({ deliverAs: "followUp", triggerTurn: true });
      expect(nextWake?.details).toEqual({
        eventIds: [81, 82],
        presentedEventIds: [81, 82],
      });

      // 5) Both copies reach the transcript: each id is confirmed exactly once (the
      // watermark only ever moves forward), so nothing is stranded or confirmed
      // unseen.
      await pi.emit("message_end", wakeConsumptionEvidence([81, 82]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackedIds).toEqual([81, 82]);
      expect(new Set(ackedIds).size).toBe(ackedIds.length);

      // 6) Confirmed: later settlements never repeat an acknowledgement.
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(5_000);
      expect(ackedIds).toEqual([81, 82]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("schedules a later wake for events arriving during a delivered batch", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(71, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(72, "term_other") } });
      // The first wake's content is carried out, so its settlement may confirm it;
      // the event that arrived meanwhile is not re-presented until the batch it
      // rides on is gone.
      await pi.emit("message_end", wakeConsumptionEvidence([71]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(500);

      expect(
        pi.hiddenMessages.map(([message]) => (message.details as { eventIds: number[] }).eventIds),
      ).toEqual([[71], [72]]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("consumes an aborted batch instead of retrying its event", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(81, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      // The abort still counts as a carried-out delivery once the content reached
      // the transcript: the batch is consumed rather than retried.
      await pi.emit("message_end", wakeConsumptionEvidence([81]), ctx);
      await pi.emit("message_end", assistantMessage("aborted"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(1_000);
      expect(
        client.calls.filter(
          ([method, params]) =>
            method === "agent.notifications.ack" && (params as { eventId: number }).eventId === 81,
        ),
      ).toHaveLength(1);
      expect(pi.hiddenMessages).toHaveLength(1);
      client.emitStream({ method: "agent.event", params: { event: event(82, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(2);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [82] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("removes a NOT_OWNER event after rejection without retrying or waking again", async () => {
    vi.useFakeTimers();
    const eventId = 88;
    const client = createWakeClient();
    const baseResponse = client.response;
    client.response = (method, params) => {
      if (
        method === "agent.notifications.ack" &&
        (params as { eventId: number }).eventId === eventId
      ) {
        throw Object.assign(
          new Error("Only the current orchestrator can acknowledge notifications"),
          {
            code: "ORCHESTRATOR_NOT_OWNER",
            retryable: false,
          },
        );
      }
      return baseResponse(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(eventId, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([eventId]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      const ackCalls = () =>
        client.calls.filter(
          ([method, params]) =>
            method === "agent.notifications.ack" &&
            (params as { eventId: number }).eventId === eventId,
        );
      expect(ackCalls()).toHaveLength(1);

      await vi.advanceTimersByTimeAsync(30_000);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(ackCalls()).toHaveLength(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("does not re-present an event whose acknowledgement failed; only new events wake", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const baseResponse = client.response;
    client.response = (method, params) => {
      if (method === "agent.notifications.ack") throw new Error("ack failed");
      return baseResponse(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(86, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      // The content reached the transcript, which is what records the
      // presentation: the guard follows the evidence, not the injection.
      await pi.emit("message_end", wakeConsumptionEvidence([86]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      // The failed acknowledgement does not replay the same event on any retry
      // timer: the presentation guard is monotonic until scope reset.
      await vi.advanceTimersByTimeAsync(120_000);
      expect(pi.hiddenMessages).toHaveLength(1);

      // A genuinely new event still wakes normally.
      client.emitStream({ method: "agent.event", params: { event: event(87, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(2);
      const lastWake = pi.hiddenMessages.at(-1)?.[0] as {
        content: string;
        details?: { eventIds: number[] };
      };
      // The wake content carries only the new event (the failed event stays in
      // the pending projection, so details.eventIds lists it too).
      expect(lastWake.content).toContain("event: 87");
      expect(lastWake.content).not.toContain("event: 86");
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("does not re-present a resync-failed event and converges it via the ack cursor sweep", async () => {
    vi.useFakeTimers();
    const eventId = 901;
    const pending = event(eventId, "term_agent");
    const client = createWakeClient();
    const base = client.response;
    client.response = (method, params) => {
      const body = params as { eventId?: number } | undefined;
      if (method === "agent.notifications.ack" && body?.eventId === eventId)
        throw new Error("Only the next pending orchestrator event can be acknowledged");
      if (method === "agent.notifications.ack" && body?.eventId === 903)
        return { acknowledged: true, ackedEventId: 903, state: { ackedEventId: 903 } };
      return base(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState:
      | { presentedEventIds: Set<number>; pendingEvents: Array<{ id: number }> }
      | undefined;
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      client.emitStream({ method: "agent.event", params: { event: pending } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [eventId] } });
      // The content reaches the transcript, so the delivery may be acknowledged
      // (the evidence, not the turn outcome, is what authorises it).
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([eventId]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(
        client.calls.filter(
          ([m, p]) =>
            m === "agent.notifications.ack" && (p as { eventId: number }).eventId === eventId,
        ),
      ).toHaveLength(1);
      // The resync-failed event is not re-presented on any retry timer...
      await vi.advanceTimersByTimeAsync(120_000);
      expect(pi.hiddenMessages).toHaveLength(1);
      // ...but it stays in the pending projection, guarded by presentedEventIds.
      expect(extensionState?.presentedEventIds.has(eventId)).toBe(true);
      expect(extensionState?.pendingEvents.some((item) => item.id === eventId)).toBe(true);

      const followUp = event(903, "term_agent");
      client.emitStream({ method: "agent.event", params: { event: followUp } });
      await vi.advanceTimersByTimeAsync(500);
      const followUpWake = pi.hiddenMessages.at(-1)?.[0] as {
        content: string;
        details?: { eventIds: number[] };
      };
      expect(followUpWake.content).toContain("event: 903");
      expect(followUpWake.content).not.toContain("event: 901");
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([followUp.id]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(
        client.calls.filter(
          ([m, p]) =>
            m === "agent.notifications.ack" && (p as { eventId: number }).eventId === followUp.id,
        ),
      ).toHaveLength(1);
      // The follow-up ack's cursor sweep prunes the failed event from the
      // pending projection; the guard remains (the daemon never re-lists acked
      // events, so keeping the id cannot suppress a future delivery).
      expect(extensionState?.pendingEvents.some((item) => item.id === eventId)).toBe(false);
      expect(extensionState?.presentedEventIds.has(eventId)).toBe(true);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("does not re-present a transient-failed event and converges it via the ack cursor sweep", async () => {
    vi.useFakeTimers();
    const { ACK_BACKOFF_CAP_MS } = (await import(extensionModuleUrl)) as Module;
    const eventId = 902;
    const pending = event(eventId, "term_agent");
    const client = createWakeClient();
    const base = client.response;
    client.response = (method, params) => {
      const body = params as { eventId?: number } | undefined;
      if (method === "agent.notifications.ack" && body?.eventId === eventId)
        throw new Error("unknown daemon failure");
      if (method === "agent.notifications.ack" && body?.eventId === 904)
        return { acknowledged: true, ackedEventId: 904, state: { ackedEventId: 904 } };
      return base(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState:
      | { presentedEventIds: Set<number>; pendingEvents: Array<{ id: number }> }
      | undefined;
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      const ackCount = () =>
        client.calls.filter(
          ([m, p]) =>
            m === "agent.notifications.ack" && (p as { eventId: number }).eventId === eventId,
        ).length;
      const settleWake = async (presented: number[]) => {
        await pi.emit("agent_start", {}, ctx);
        // The content reaches the transcript, so the delivery may be
        // acknowledged: the evidence, not the turn outcome, authorises it.
        if (presented.length > 0) {
          await pi.emit("message_end", wakeConsumptionEvidence(presented), ctx);
        }
        await pi.emit("message_end", assistantMessage("stop"), ctx);
        await pi.emit("agent_settled", {}, ctx);
      };

      client.emitStream({ method: "agent.event", params: { event: pending } });
      await vi.advanceTimersByTimeAsync(500);
      await settleWake([eventId]);
      expect(ackCount()).toBe(1);
      // No backoff retry re-presents the failed event: the backoff window passes
      // without any additional wake or ack attempt.
      await vi.advanceTimersByTimeAsync(ACK_BACKOFF_CAP_MS + 30_000);
      expect(ackCount()).toBe(1);
      expect(pi.hiddenMessages).toHaveLength(1);
      // The failed event stays in the pending projection, guarded by
      // presentedEventIds, until the ack cursor sweeps it.
      expect(extensionState?.presentedEventIds.has(eventId)).toBe(true);
      expect(extensionState?.pendingEvents.some((item) => item.id === eventId)).toBe(true);

      // A follow-up event wakes; its ack sweeps the failed event out of the
      // pending projection.
      const followUp = event(904, "term_agent");
      client.emitStream({ method: "agent.event", params: { event: followUp } });
      await vi.advanceTimersByTimeAsync(500);
      const followUpWake = pi.hiddenMessages.at(-1)?.[0] as {
        content: string;
        details?: { eventIds: number[] };
      };
      expect(followUpWake.content).toContain("event: 904");
      expect(followUpWake.content).not.toContain("event: 902");
      await settleWake([followUp.id]);
      expect(
        client.calls.filter(
          ([m, p]) =>
            m === "agent.notifications.ack" && (p as { eventId: number }).eventId === followUp.id,
        ),
      ).toHaveLength(1);
      expect(extensionState?.pendingEvents.some((item) => item.id === eventId)).toBe(false);
      expect(extensionState?.presentedEventIds.has(eventId)).toBe(true);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("a resync-failed event is not re-presented and K1's ack cursor sweeps it (no cascade)", async () => {
    vi.useFakeTimers();
    const k0 = event(910, "term_agent");
    const k1 = event(911, "term_agent");
    const client = createWakeClient();
    const base = client.response;
    const ackCount = (id: number) =>
      client.calls.filter(
        ([m, p]) => m === "agent.notifications.ack" && (p as { eventId: number }).eventId === id,
      ).length;
    // The server keeps the failed K0 delivered. Because delivered events
    // do not trip the ordering guard, acking K1 passes and the server's markAcked
    // (id <= cursor) sweeps K0: the client observes ackedEventId = K1.id.
    client.response = (method, params) => {
      const eventId = (params as { eventId?: number } | undefined)?.eventId;
      if (method === "agent.notifications.ack" && eventId === k0.id)
        throw new Error("Only the next pending orchestrator event can be acknowledged");
      if (method === "agent.notifications.ack" && eventId === k1.id)
        return { acknowledged: true, ackedEventId: k1.id, state: { ackedEventId: k1.id } };
      return base(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState: { pendingEvents: Array<{ id: number }> } | undefined;
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      const settleWake = async (presented: number[]) => {
        await pi.emit("agent_start", {}, ctx);
        // The content reaches the transcript, which is what authorises the
        // acknowledgement — the turn outcome never does.
        if (presented.length > 0) {
          await pi.emit("message_end", wakeConsumptionEvidence(presented), ctx);
        }
        await pi.emit("message_end", assistantMessage("stop"), ctx);
        await pi.emit("agent_settled", {}, ctx);
      };

      client.emitStream({ method: "agent.event", params: { event: k0 } });
      await vi.advanceTimersByTimeAsync(500);
      await settleWake([k0.id]);
      expect(ackCount(k0.id)).toBe(1);

      // K0 is not re-presented: even a daemon re-emission stays guarded.
      client.emitStream({ method: "agent.event", params: { event: k0 } });
      client.emitStream({ method: "agent.event", params: { event: k1 } });
      await vi.advanceTimersByTimeAsync(500);
      const k1Wake = pi.hiddenMessages.at(-1)?.[0] as {
        content: string;
        details?: { eventIds: number[] };
      };
      // K0 stays in the pending projection (details.eventIds lists every pending
      // id), but the wake content carries only the new event K1.
      expect(k1Wake.details?.eventIds).toEqual([k0.id, k1.id]);
      expect(k1Wake.content).toContain("event: 911");
      expect(k1Wake.content).not.toContain("event: 910");

      // K1's ack passes the delivered K0 and the cursor sweep prunes the client
      // state: K0 leaves the pending projection without any retry (no cascade).
      await settleWake([k1.id]);
      expect(ackCount(k1.id)).toBe(1);
      expect(ackCount(k0.id)).toBe(1);
      expect(extensionState?.pendingEvents.some((item) => item.id === k0.id)).toBe(false);
      await vi.advanceTimersByTimeAsync(120_000);
      expect(ackCount(k1.id)).toBe(1);
      expect(ackCount(k0.id)).toBe(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("resync-failed events stay guarded and later events still wake (no cascade)", async () => {
    vi.useFakeTimers();
    const k0 = event(912, "term_agent");
    const k1 = event(913, "term_agent");
    const k2 = event(914, "term_agent");
    const client = createWakeClient();
    const base = client.response;
    const ackCount = (id: number) =>
      client.calls.filter(
        ([m, p]) => m === "agent.notifications.ack" && (p as { eventId: number }).eventId === id,
      ).length;
    // Worst case the oracle feared: the server keeps the head event pending and
    // rejects every later ack with a resync. The extension must not enter a
    // re-presentation/retry storm for the failed events.
    client.response = (method, params) => {
      const eventId = (params as { eventId?: number } | undefined)?.eventId;
      if (
        method === "agent.notifications.ack" &&
        (eventId === k0.id || eventId === k1.id || eventId === k2.id)
      )
        throw new Error("Only the next pending orchestrator event can be acknowledged");
      return base(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      const settleWake = async (presented: number[]) => {
        await pi.emit("agent_start", {}, ctx);
        // The content reaches the transcript, which is what authorises the
        // acknowledgements below — the turn outcome never does.
        if (presented.length > 0) {
          await pi.emit("message_end", wakeConsumptionEvidence(presented), ctx);
        }
        await pi.emit("message_end", assistantMessage("stop"), ctx);
        await pi.emit("agent_settled", {}, ctx);
      };

      // All three events arrive; they wake together once.
      for (const event of [k0, k1, k2]) {
        client.emitStream({ method: "agent.event", params: { event } });
      }
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[0]).toMatchObject({
        details: { eventIds: [k0.id, k1.id, k2.id] },
      });
      await settleWake([k0.id, k1.id, k2.id]);
      // Each ack is attempted once and rejected; nothing is re-presented and no
      // retry storm follows (the presentation guard is monotonic until scope
      // reset, and the failed events converge via the ack cursor sweep).
      expect(ackCount(k0.id)).toBe(1);
      expect(ackCount(k1.id)).toBe(1);
      expect(ackCount(k2.id)).toBe(1);
      await vi.advanceTimersByTimeAsync(120_000);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(ackCount(k0.id)).toBe(1);
      expect(ackCount(k1.id)).toBe(1);
      expect(ackCount(k2.id)).toBe(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("does not churn: the server re-listing a failed K0 on every get no longer stalls K1's wake", async () => {
    vi.useFakeTimers();
    const k0 = event(915, "term_agent");
    const k1 = event(916, "term_agent");
    const client = createWakeClient();
    const base = client.response;
    const ackCount = (id: number) =>
      client.calls.filter(
        ([m, p]) => m === "agent.notifications.ack" && (p as { eventId: number }).eventId === id,
      ).length;
    let reListK0 = false; // server starts by holding K0 delivered/pending
    client.response = (method, params) => {
      const eventId = (params as { eventId?: number } | undefined)?.eventId;
      if (method === "agent.notifications.ack" && eventId === k0.id)
        throw new Error("Only the next pending orchestrator event can be acknowledged");
      // K0 stays delivered server-side, so the ordering guard passes K1 and
      // markAcked (id <= cursor) sweeps K0; the ack carries ackedEventId = K1.id.
      if (method === "agent.notifications.ack" && eventId === k1.id)
        return { acknowledged: true, ackedEventId: k1.id, state: { ackedEventId: k1.id } };
      if (method === "agent.orchestrator.get" && reListK0) {
        // Faithful to the real server: every get re-lists the failed K0
        // (it stays pending/delivered in the scope until acked). The extension
        // must not count it as a new event (which would cancel the in-flight
        // wake and stall the stream forever), and must not re-present it.
        return connectionResponse({ events: [k0, k1] });
      }
      return base(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      const settleWake = async (presented: number[]) => {
        await pi.emit("agent_start", {}, ctx);
        // The content reaches the transcript, which is what authorises the
        // acknowledgement — the turn outcome never does.
        if (presented.length > 0) {
          await pi.emit("message_end", wakeConsumptionEvidence(presented), ctx);
        }
        await pi.emit("message_end", assistantMessage("stop"), ctx);
        await pi.emit("agent_settled", {}, ctx);
      };

      // K0 is presented once; its ack fails (resync) and it is never re-presented.
      client.emitStream({ method: "agent.event", params: { event: k0 } });
      await vi.advanceTimersByTimeAsync(500);
      await settleWake([k0.id]);
      expect(ackCount(k0.id)).toBe(1);

      // K1 arrives; every subsequent get re-lists the failed K0.
      reListK0 = true;
      client.emitStream({ method: "agent.event", params: { event: k1 } });
      await vi.advanceTimersByTimeAsync(500);
      // K1 is presented on the first wake: the failed K0 no longer counts as a
      // new event, so the in-flight wake is not cancelled by its own get, and
      // K0 itself is not re-presented.
      const k1Wake = pi.hiddenMessages.at(-1)?.[0] as {
        content: string;
        details?: { eventIds: number[] };
      };
      expect(k1Wake.content).toContain("event: 916");
      expect(k1Wake.content).not.toContain("event: 915");
      await settleWake([k1.id]);
      expect(ackCount(k1.id)).toBe(1);
      // No wake churn and no re-presentation of the failed K0 afterwards: its
      // ack count stays at the single first attempt.
      await vi.advanceTimersByTimeAsync(120_000);
      expect(ackCount(k1.id)).toBe(1);
      expect(ackCount(k0.id)).toBe(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("accumulates ack attempts for an event whose live row vanished and dead-letters it at the cap", async () => {
    vi.useFakeTimers();
    const logHome = mkdtempSync(join(tmpdir(), "herdsman-pi-ack-attempts-"));
    const previousHome = process.env.HERDSMAN_HOME;
    process.env.HERDSMAN_HOME = logHome;
    const stranded = event(71, "term_agent");
    const client = createWakeClient();
    const base = client.response;
    const ackAttempts: number[] = [];
    client.response = (method, params) => {
      if (method === "agent.notifications.ack") {
        const { eventId } = params as { eventId: number };
        ackAttempts.push(eventId);
        // A transient failure: the daemon is busy, the event stays pending.
        const error = new Error("ORCHESTRATOR_BUSY") as Error & { code?: string };
        error.code = "ORCHESTRATOR_BUSY";
        throw error;
      }
      return base(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const attemptsFor = (eventId: number) => ackAttempts.filter((id) => id === eventId).length;
    const settleWake = async (presented: number[]) => {
      await pi.emit("agent_start", {}, ctx);
      // The content reaches the transcript, so the acknowledgement is attempted —
      // the evidence, not the turn outcome, authorises it.
      if (presented.length > 0) {
        await pi.emit("message_end", wakeConsumptionEvidence(presented), ctx);
      }
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
    };
    try {
      await startExtension(client, pi, ctx);

      client.emitStream({ method: "agent.event", params: { event: stranded } });
      await vi.advanceTimersByTimeAsync(0);
      expect(pi.hiddenMessages).toHaveLength(1);
      await settleWake([stranded.id]);
      expect(attemptsFor(stranded.id)).toBe(1);

      // The server stops listing the event: the live projection loses the row
      // while the delivery queue keeps it (the queue is only touched by an
      // acknowledgement or a scope reset).
      client.currentEvents.delete(stranded.id);

      for (const id of [72, 73, 74, 75]) {
        client.emitStream({ method: "agent.event", params: { event: event(id, "term_agent") } });
        await vi.advanceTimersByTimeAsync(0);
        await settleWake([id]);
      }
      // Five failed attempts in total: the counter accumulated on the queue copy
      // instead of restarting at 1 with every settlement.
      expect(attemptsFor(stranded.id)).toBe(5);

      // Dead-lettered at the cap: the event left the delivery queue, so a later
      // settlement no longer re-attempts it.
      client.emitStream({ method: "agent.event", params: { event: event(76, "term_agent") } });
      await vi.advanceTimersByTimeAsync(0);
      await settleWake([76]);
      expect(attemptsFor(stranded.id)).toBe(5);
      expect(attemptsFor(76)).toBe(1);

      const log = readFileSync(
        join(
          logHome,
          "logs",
          `herdsman-pi-${new Date().toISOString().slice(0, 10).replaceAll("-", "")}.log`,
        ),
        "utf8",
      );
      expect(log).toContain(
        "acknowledgement moved to dead-letter eventId=71 attempts=5 code=ORCHESTRATOR_BUSY",
      );
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
      rmSync(logHome, { recursive: true, force: true });
      if (previousHome === undefined) delete process.env.HERDSMAN_HOME;
      else process.env.HERDSMAN_HOME = previousHome;
    }
  });

  test("does not re-present an already-presented event after a reconnect clears its unstarted batch", async () => {
    vi.useFakeTimers();
    const pending = event(88, "term_agent");
    const client = createFakeClient();
    let registrations = 0;
    client.response = (method) => {
      if (method === "agent.orchestrator.register") {
        registrations += 1;
        return connectionResponse({ events: registrations === 1 ? [] : [pending] });
      }
      if (method === "agent.orchestrator.get") return connectionResponse();
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: pending } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
      // The content reaches the transcript before the disconnect, which is what
      // records the presentation: the guard follows the evidence, not the
      // injection.
      await pi.emit("message_end", wakeConsumptionEvidence([pending.id]), ctx);

      client.disconnect();
      await client.connect();
      await vi.advanceTimersByTimeAsync(1_000);
      // The batch's wake turn never started, so the fresh connection clears it
      // and the daemon re-lists the still-unacknowledged event — but the event
      // was already presented this session, so it is not presented a second
      // time (the presentation guard is monotonic until scope reset).
      expect(pi.hiddenMessages).toHaveLength(1);

      await pi.emit("agent_settled", {}, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(89, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      // The next wake's batch content contains only the new event
      // (details.eventIds still lists every pending id, including the
      // already-presented one).
      expect(pi.hiddenMessages).toHaveLength(2);
      const lastWake = pi.hiddenMessages.at(-1)?.[0] as {
        content: string;
        details?: { eventIds: number[] };
      };
      expect(lastWake.content).toContain("event: 89");
      expect(lastWake.content).not.toContain("event: 88");
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("wakes replayed pending outcomes after registration", async () => {
    vi.useFakeTimers();
    const client = createWakeClient([event(91, "term_agent")]);
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      await vi.advanceTimersByTimeAsync(500);

      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[0]).toMatchObject({ details: { eventIds: [91] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("lets a replacement Pi wake the previous owner's unacknowledged batch", async () => {
    vi.useFakeTimers();
    const pending = event(96, "term_agent");
    const firstClient = createWakeClient();
    const firstPi = createFakePi();
    const firstCtx = fakeCtx({ idle: true });
    const secondClient = createWakeClient([pending]);
    const secondPi = createFakePi();
    const secondCtx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(firstClient, firstPi, firstCtx);
      firstClient.emitStream({ method: "agent.event", params: { event: pending } });
      await vi.advanceTimersByTimeAsync(500);
      await firstPi.emit("agent_start", {}, firstCtx);
      firstClient.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: roleChange("term_pi", "term_other", "wB:p-other") },
      });
      expect(firstCtx.aborts).toBe(1);

      await startExtension(secondClient, secondPi, secondCtx);
      await vi.advanceTimersByTimeAsync(500);
      expect(secondPi.hiddenMessages).toHaveLength(1);
      expect(secondPi.hiddenMessages[0]?.[0]).toMatchObject({ details: { eventIds: [96] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("defers pending updates while a busy user run is in progress and acknowledges only its wake", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    client.response = (method) =>
      method === "agent.orchestrator.register" || method === "agent.orchestrator.get"
        ? connectionResponse({ context: contextSnapshot("normal") })
        : { acknowledged: true };
    const pi = createFakePi();
    // The orchestrator is busy when the update arrives, so the wake is deferred
    // instead of being injected into the running user turn.
    const ctx = fakeCtx({ idle: false });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(101, "term_agent") } });
      await vi.advanceTimersByTimeAsync(250);
      await pi.emit("agent_start", {}, ctx);
      const normalContext = await pi.emitContext([], ctx);
      await vi.advanceTimersByTimeAsync(250);

      expect(pi.hiddenMessages).toEqual([]);
      // Nothing is pinned on the context hook: the deferred wake rides the
      // follow-up track only, and the agent-context preview was removed.
      expect(normalContext).toEqual([]);
      expect(client.calls.some(([method]) => method === "agent.notifications.ack")).toBe(false);

      await pi.emit("message_end", assistantMessage("stop"), ctx);
      ctx.setIdle(true);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      await pi.emit("agent_start", {}, ctx);
      // The injected wake content reaches the transcript — that is the only
      // evidence that authorises the acknowledgement.
      await pi.emit("message_end", wakeConsumptionEvidence([101]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(client.calls).toContainEqual(["agent.notifications.ack", { eventId: 101 }]);
      expect(ctx.aborts).toBe(0);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("rebuilds pending wake state when timer refresh reveals a missed workspace move", async () => {
    vi.useFakeTimers();
    const target = event(104, "term_agent", {
      paneId: "wC:p-agent",
      workspaceId: "wC",
    });
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register") return connectionResponse();
      if (method === "agent.orchestrator.get") {
        return connectionResponse({ events: [target], paneId: "wC:p1", workspaceId: "wC" });
      }
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(103, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      await vi.advanceTimersByTimeAsync(500);

      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[0]).toMatchObject({ details: { eventIds: [104] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("resets an old batch when reconnect registration reveals a missed workspace move", async () => {
    vi.useFakeTimers();
    const target = event(106, "term_agent", {
      paneId: "wC:p-agent",
      workspaceId: "wC",
    });
    const client = createFakeClient();
    let moved = false;
    client.response = (method) => {
      if (method === "agent.orchestrator.register" || method === "agent.orchestrator.get") {
        return moved
          ? connectionResponse({ events: [target], paneId: "wC:p1", workspaceId: "wC" })
          : connectionResponse();
      }
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(105, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      ctx.setIdle(false);
      moved = true;
      await client.connect();

      expect(ctx.aborts).toBe(1);
      ctx.setIdle(true);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(500);

      expect(client.calls).not.toContainEqual(["agent.notifications.ack", { eventId: 105 }]);
      expect(
        pi.hiddenMessages.map(([message]) => (message.details as { eventIds: number[] }).eventIds),
      ).toEqual([[105], [106]]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("drops a stale timer when the same terminal moves workspaces", async () => {
    vi.useFakeTimers();
    const target = event(108, "term_agent", {
      paneId: "wC:p-agent",
      workspaceId: "wC",
    });
    const client = createFakeClient();
    let moved = false;
    client.response = (method) => {
      if (method === "agent.orchestrator.register") return connectionResponse();
      if (method === "agent.orchestrator.get") {
        return moved
          ? connectionResponse({ events: [target], paneId: "wC:p1", workspaceId: "wC" })
          : connectionResponse();
      }
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    // Busy orchestrator: event 107 is deferred (spinning, not yet injected) when
    // the terminal moves workspaces, so that stale deferral must be dropped.
    const ctx = fakeCtx({ idle: false });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(107, "term_agent") } });
      await vi.advanceTimersByTimeAsync(250);
      expect(pi.hiddenMessages).toEqual([]);
      moved = true;
      ctx.setIdle(true);
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: movedRoleChange() },
      });
      await vi.advanceTimersByTimeAsync(500);

      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[0]).toMatchObject({ details: { eventIds: [108] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("invalidates a delivered Herdsman batch on same-terminal workspace move without aborting substantive work", async () => {
    vi.useFakeTimers();
    const target = event(110, "term_agent", {
      paneId: "wC:p-agent",
      workspaceId: "wC",
    });
    const client = createFakeClient();
    let moved = false;
    client.response = (method) => {
      if (method === "agent.orchestrator.register") return connectionResponse();
      if (method === "agent.orchestrator.get") {
        return moved
          ? connectionResponse({ events: [target], paneId: "wC:p1", workspaceId: "wC" })
          : connectionResponse();
      }
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(109, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("tool_execution_start", { toolName: "bash", toolCallId: "tool-1" }, ctx);
      moved = true;
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: movedRoleChange() },
      });
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);

      expect(ctx.aborts).toBe(0);
      expect(
        pi.hiddenMessages.map(([message]) => (message.details as { eventIds: number[] }).eventIds),
      ).toEqual([[109], [110]]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("cancels pending wake and aborts only a Herdsman-triggered turn on role loss", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    // Busy orchestrator: the wake stays deferred, so the first role loss has
    // nothing injected to abort.
    const ctx = fakeCtx({ idle: false });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(111, "term_agent") } });
      await vi.advanceTimersByTimeAsync(250);
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: roleChange("term_pi", "term_other", "wB:p-other") },
      });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toEqual([]);
      expect(ctx.aborts).toBe(0);

      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: roleChange("term_other", "term_pi") },
      });
      await vi.advanceTimersByTimeAsync(500);
      ctx.setIdle(true);
      client.emitStream({ method: "agent.event", params: { event: event(112, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: roleChange("term_pi", "term_other", "wB:p-other") },
      });

      expect(ctx.aborts).toBe(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("never injects into or aborts a user turn while the orchestrator is busy", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    // The orchestrator is busy: the user's own turn is running.
    const ctx = fakeCtx({ idle: false });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      await pi.emit("agent_start", {}, ctx);

      // The update waits for the settlement instead of being handed over to the
      // user's turn: nothing is injected, so nothing rides that turn either.
      client.emitStream({ method: "agent.event", params: { event: event(121, "term_agent") } });
      await vi.advanceTimersByTimeAsync(60_000);
      expect(pi.hiddenMessages).toEqual([]);

      // Ownership moves away while that user turn is still running. Nothing was
      // ever handed to it, so there is also nothing of ours to abort
      // ("Never abort a normal user-triggered turn").
      client.emitStream({
        method: "agent.orchestrator.changed",
        params: { change: roleChange("term_pi", "term_other", "wB:p-other") },
      });
      await vi.advanceTimersByTimeAsync(100);
      expect(ctx.aborts).toBe(0);
      expect(pi.hiddenMessages).toEqual([]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("clears reconnecting UI on shutdown", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(113, "term_agent") } });
      await vi.advanceTimersByTimeAsync(250);
      client.disconnect();
      expect(ctx.statuses.get("herdsman")).toBe("◇ Herdsman · reconnecting");

      await pi.emit("session_shutdown");

      expect(ctx.statuses.get("herdsman")).toBeUndefined();
      expect(ctx.notifications).toEqual([]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("cancels a deferred owner wake on shutdown", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: false });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(114, "term_agent") } });
      await vi.advanceTimersByTimeAsync(250);

      await pi.emit("session_shutdown");
      await vi.advanceTimersByTimeAsync(500);

      expect(ctx.statuses.get("herdsman")).toBeUndefined();
      expect(pi.hiddenMessages).toEqual([]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("closes on shutdown and a fresh Pi session registers with its own subscriber id", async () => {
    const first = createFakeClient();
    const second = createFakeClient();
    const clients = [first, second];
    const pi = createFakePi();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => clients.shift() as FakeClient })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, fakeCtx({ sessionId: "pi-old" }));
      await first.connect();
      await pi.emit("session_shutdown");
      await pi.emit("session_start", {}, fakeCtx({ sessionId: "pi-new" }));
      await second.connect();

      expect(first.closed).toBe(true);
      expect(second.calls[0]).toEqual([
        "agent.orchestrator.register",
        expect.objectContaining({ subscriberId: "pi-new" }),
      ]);
    } finally {
      restoreEnv(previous);
    }
  });

  // Phase 1 busy wake delivery: the four contracts this change is pinned to.
  // 1) A busy orchestrator is deferred to `agent_settled`, and the wake it then
  //    gets is a clean, triggerable turn that carries the real update body.
  test("Phase 1: a busy orchestrator is deferred and woken with a clean triggerable turn", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: false });
    const previous = withHerdrEnv();
    const wakeInjections = () =>
      pi.hiddenMessages.filter(([message]) => message.customType === "herdsman-wake-context");
    try {
      await startExtension(client, pi, ctx);
      await pi.emit("agent_start", {}, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(501, "term_agent") } });

      // Busy: nothing is injected, whatever the elapsed time is.
      await vi.advanceTimersByTimeAsync(90_000);
      expect(wakeInjections()).toEqual([]);

      // Settled: one clean, triggerable injection carrying the real update body.
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      ctx.setIdle(true);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(0);
      expect(wakeInjections()).toHaveLength(1);
      expect(wakeInjections()[0]?.[1]).toEqual({ deliverAs: "followUp", triggerTurn: true });
      const content = String(wakeInjections()[0]?.[0]?.content);
      expect(content).toContain("[HERDSMAN WAKE POLICY]");
      expect(content).toContain("event: 501");
      expect(content).toContain("last assistant: done");
      expect(
        pi.hiddenMessages.filter(([message]) => message.customType === "herdsman-wake-queued"),
      ).toEqual([]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  // 2) The settlement never emits an empty `herdsman-wake-continuation` marker:
  //    a continuation, if it is owed at all, carries the real update content.
  test("Phase 1: settle never sends an empty herdsman-wake-continuation marker", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(502, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      // No run carries the update out, so the settlement owes a continuation. It
      // must not spend one on a marker whose only content is a prompt.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", assistantMessage("error"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(5_000);
      expect(
        pi.hiddenMessages.filter(
          ([message]) => message.customType === "herdsman-wake-continuation",
        ),
      ).toEqual([]);
      // The only marker-free recovery is the re-presentation of the event itself,
      // which is the message above and carries the update body.
      const wakeInjections = () =>
        pi.hiddenMessages.filter(([message]) => message.customType === "herdsman-wake-context");
      expect(wakeInjections()).toHaveLength(2);
      expect(String(wakeInjections()[1]?.[0]?.content)).toContain("event: 502");
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  // 3) The write-off is a dead letter: it never tells the daemon that an event the
  //    orchestrator never saw was consumed.
  test("Phase 1: the dead-letter write-off sends no acknowledgement", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    const ackCalls = () => client.calls.filter(([method]) => method === "agent.notifications.ack");
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(503, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      // Unconsumed, so the settlement dead-letters it.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", assistantMessage("error"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackCalls()).toEqual([]);

      // And it stays that way: the re-presentation the dead-letter allows must
      // never turn into an acknowledgement, so an event the orchestrator never
      // saw is still pending in the daemon.
      await vi.advanceTimersByTimeAsync(10_000);
      await pi.emit("agent_settled", {}, ctx);
      expect(ackCalls()).toEqual([]);
      // The user sees a notice per write-off (the retryable id can be re-presented
      // and written off again if it still reaches nobody), never silence.
      expect(
        ctx.notifications.filter(([text]) => String(text).includes("retained unacked in daemon"))
          .length,
      ).toBeGreaterThanOrEqual(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  // 4) An event whose `message_end` never arrived is not written into
  //    `presentedEventIds`, so the next wake is still allowed to present it.
  test("Phase 1: an event without message_end stays eligible for the next wake", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState: { presentedEventIds: Set<number> } | undefined;
    const ackedIds = () =>
      client.calls
        .filter(([method]) => method === "agent.notifications.ack")
        .map(([, params]) => (params as { eventId: number }).eventId);
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      client.emitStream({ method: "agent.event", params: { event: event(504, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      // The injection alone is not evidence: the id is in flight, not presented.
      expect(extensionState?.presentedEventIds.has(504)).toBe(false);

      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(extensionState?.presentedEventIds.has(504)).toBe(false);
      expect(ackedIds()).toEqual([]);

      // The next wake still qualifies: the update is presented again.
      await vi.advanceTimersByTimeAsync(500);
      const wakeInjections = pi.hiddenMessages.filter(
        ([message]) => message.customType === "herdsman-wake-context",
      );
      expect(wakeInjections).toHaveLength(2);
      expect(
        (wakeInjections[1]?.[0].details as { presentedEventIds: number[] }).presentedEventIds,
      ).toEqual([504]);

      // Now the content reaches the transcript: that, and only that, records the
      // presentation and authorises the acknowledgement.
      await pi.emit("message_end", wakeConsumptionEvidence([504]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(extensionState?.presentedEventIds.has(504)).toBe(true);
      expect(ackedIds()).toEqual([504]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });
});

const USAGE = "Usage: /herdsman [on|off|status]";

describe("herdsman-pi upstream error wake filter", () => {
  test("silently acknowledges a pure upstream error without waking or notifying", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState:
      | { pendingEvents: AgentEventWireRecord[]; presentedEventIds: Set<number> }
      | undefined;
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      client.emitStream({
        method: "agent.event",
        params: {
          event: event(43, "term_agent", {
            compactHistory: { lastAssistantMessage: { text: "API Error: 429 rate_limit_error" } },
          }),
        },
      });

      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman");
      expect(ctx.statuses.get("herdsman")).not.toContain("agent updates");
      const notifications = ctx.notifications.length;
      // 抑制的上游错误不再等 settle 窗口：0ms 内静默 ack，但仍不注入、不通知。
      await vi.advanceTimersByTimeAsync(0);
      expect(pi.hiddenMessages).toEqual([]);
      expect(ctx.notifications).toHaveLength(notifications);
      expect(client.calls).toContainEqual(["agent.notifications.ack", { eventId: 43 }]);
      expect(extensionState?.presentedEventIds.has(43)).toBe(false);
      expect(extensionState?.pendingEvents.some((item) => item.id === 43)).toBe(false);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman");
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("wakes normally when the wake filter is disabled", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx, {
        wakeFilter: { enabled: false, extraPatterns: [] },
      });
      client.emitStream({
        method: "agent.event",
        params: {
          event: event(43, "term_agent", {
            compactHistory: { lastAssistantMessage: { text: "API Error: 429 rate_limit_error" } },
          }),
        },
      });

      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[0]).toMatchObject({
        content: expect.stringContaining("API Error: 429"),
        details: { eventIds: [43] },
      });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("coalesces a normal outcome while dropping a suppressed error id", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState:
      | { pendingEvents: AgentEventWireRecord[]; presentedEventIds: Set<number> }
      | undefined;
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      // The daemon returns the swept cursor after an ack; the normal outcome's
      // acknowledgement therefore covers the suppressed error id as well.
      const baseResponse = client.response;
      client.response = (method, params) => {
        if (method === "agent.notifications.ack") {
          const eventId = (params as { eventId: number }).eventId;
          return { acknowledged: true, ackedEventId: eventId, state: { ackedEventId: eventId } };
        }
        return baseResponse(method, params);
      };
      client.emitStream({
        method: "agent.event",
        params: {
          event: event(43, "term_agent", {
            compactHistory: { lastAssistantMessage: { text: "API Error: 429 rate_limit_error" } },
          }),
        },
      });
      client.emitStream({ method: "agent.event", params: { event: event(44, "term_agent") } });

      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[0]).toMatchObject({ details: { eventIds: [44] } });
      expect(pi.hiddenMessages[0]?.[0]?.content).not.toContain("429");

      // The normal outcome's content reaches the transcript, which is what
      // authorises the acknowledgement below.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([44]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      // Only the normal outcome is acknowledged; the suppressed error id is
      // covered by the returned cursor and swept out of the pending set.
      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: 44 }],
      ]);
      expect(extensionState?.pendingEvents).toEqual([]);
      expect(extensionState?.presentedEventIds.has(43)).toBe(false);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("acknowledges a pure upstream error even while the user is not idle", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: false });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({
        method: "agent.event",
        params: {
          event: event(43, "term_agent", {
            compactHistory: { lastAssistantMessage: { text: "API Error: 429 rate_limit_error" } },
          }),
        },
      });

      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toEqual([]);
      expect(client.calls).toContainEqual(["agent.notifications.ack", { eventId: 43 }]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("retries a silent ack after a transient failure across the backoff window", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState:
      | { pendingEvents: AgentEventWireRecord[]; presentedEventIds: Set<number> }
      | undefined;
    let ackAttempts = 0;
    const baseResponse = client.response;
    client.response = (method, params) => {
      if (method === "agent.notifications.ack") {
        ackAttempts += 1;
        if (ackAttempts === 1) {
          throw Object.assign(new Error("orchestrator busy"), { code: "ORCHESTRATOR_BUSY" });
        }
      }
      return baseResponse(method, params);
    };
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      client.emitStream({
        method: "agent.event",
        params: {
          event: event(43, "term_agent", {
            compactHistory: { lastAssistantMessage: { text: "API Error: 429 rate_limit_error" } },
          }),
        },
      });

      // The first silent ack fires on the 0ms wake tick and fails transiently,
      // writing a future nextAttemptAt instead of being silently dropped.
      await vi.advanceTimersByTimeAsync(0);
      expect(ackAttempts).toBe(1);
      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: 43 }],
      ]);
      expect(extensionState?.pendingEvents.some((item) => item.id === 43)).toBe(true);
      expect(pi.hiddenMessages).toEqual([]);

      // No 0ms spin: nothing is re-attempted until the backoff window elapses.
      await vi.advanceTimersByTimeAsync(100);
      expect(ackAttempts).toBe(1);
      await vi.advanceTimersByTimeAsync(149);
      expect(ackAttempts).toBe(1);

      // After the 250ms backoff the ack is re-issued on the next wake tick.
      await vi.advanceTimersByTimeAsync(2);
      expect(ackAttempts).toBe(2);
      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: 43 }],
        ["agent.notifications.ack", { eventId: 43 }],
      ]);
      expect(extensionState?.pendingEvents.some((item) => item.id === 43)).toBe(false);
      expect(pi.hiddenMessages).toEqual([]);
      expect(ctx.notifications).toHaveLength(0);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });
});

describe("herdsman-pi disconnect regression (independent coverage)", () => {
  test("does not abort on a transient disconnect, invalidates the delivered batch, and advances the failed wake cursor", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(201, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[0]).toMatchObject({ details: { eventIds: [201] } });

      client.disconnect(new Error("transient disconnect"));
      expect(ctx.aborts).toBe(0);

      await client.connect();
      client.emitStream({ method: "agent.event", params: { event: event(202, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      // The invalidated batch is cleared on the fresh connection, so the new
      // pending event wakes immediately instead of waiting for a settle.
      expect(pi.hiddenMessages).toHaveLength(2);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [202] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });
});

describe("pi transient reconnect cursor regression (independent coverage)", () => {
  test("replays the same batch after a transient disconnect and advances the cursor only after ack", async () => {
    vi.useFakeTimers();
    const pending = event(220, "term_agent");
    let registrations = 0;
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register") {
        registrations += 1;
        return connectionResponse({ events: registrations === 1 ? [pending] : [pending] });
      }
      if (method === "agent.orchestrator.get") return connectionResponse();
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.disconnect(new Error("transient disconnect"));
      expect(pi.hiddenMessages).toEqual([]);
      await client.connect();
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
      await pi.emit("agent_start", {}, ctx);
      // The content reaches the transcript, which is what authorises the
      // acknowledgement: the reconnect kept the batch, and it settles now.
      await pi.emit("message_end", wakeConsumptionEvidence([220]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: 220 }],
      ]);
      client.emitStream({ method: "agent.event", params: { event: event(221, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [221] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });
  test("continues acking past a transient failure so the rest of the batch is acknowledged", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const baseResponse = client.response;
    client.response = (method, params) => {
      if (method === "agent.notifications.ack" && (params as { eventId: number }).eventId === 202) {
        throw new Error("ack failed");
      }
      return baseResponse(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      for (const id of [201, 202, 203]) {
        client.emitStream({ method: "agent.event", params: { event: event(id, "term_agent") } });
      }
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      // All three copies reach the transcript, which is what authorises the
      // acknowledgements below.
      await pi.emit("message_end", wakeConsumptionEvidence([201, 202, 203]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      // A single transient failure must not block the rest of the batch: 203 is
      // acknowledged in the same settlement round after 202 failed.
      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: 201 }],
        ["agent.notifications.ack", { eventId: 202 }],
        ["agent.notifications.ack", { eventId: 203 }],
      ]);
      client.emitStream({ method: "agent.event", params: { event: event(204, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      // 202 failed and the ack cursor advanced through the later events, so the
      // next wake presents only the new event.
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({
        details: { eventIds: [204] },
      });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });
});

describe("pi batch delivery fixes (independent coverage)", () => {
  test("acks the rest of a delivered batch after a resync failure on one event", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const baseResponse = client.response;
    client.response = (method, params) => {
      if (method === "agent.notifications.ack" && (params as { eventId: number }).eventId === 202) {
        throw new Error("Only the next pending orchestrator event can be acknowledged");
      }
      return baseResponse(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      for (const id of [202, 203, 204]) {
        client.emitStream({ method: "agent.event", params: { event: event(id, "term_agent") } });
      }
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      // The content reaches the transcript, which is what authorises the
      // acknowledgements below.
      await pi.emit("message_end", wakeConsumptionEvidence([202, 203, 204]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);
      // A single resync failure must not block the remaining events: 203 and 204
      // are acknowledged in the same settlement round after 202 failed.
      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: 202 }],
        ["agent.notifications.ack", { eventId: 203 }],
        ["agent.notifications.ack", { eventId: 204 }],
      ]);
      // The failed event stays pending (with backoff) and is not re-presented.
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman · 1 agent update");
      expect(pi.hiddenMessages).toHaveLength(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("does not re-present a reclaim-redelivered event that was already presented", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(121, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[0]).toMatchObject({ details: { eventIds: [121] } });
      // The copy reached the transcript, so the event is presented from now on.
      await pi.emit("message_end", wakeConsumptionEvidence([121]), ctx);

      // The daemon reclaims and redelivers the same event id while it is still
      // presented and unacknowledged: it must not be presented a second time.
      client.emitStream({ method: "agent.event", params: { event: event(121, "term_agent") } });
      await vi.advanceTimersByTimeAsync(1_000);
      expect(pi.hiddenMessages).toHaveLength(1);

      // Even a settle that never acknowledges (no terminal assistant message)
      // must not loop the batch back into a duplicate presentation.
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(1_000);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[0]).toMatchObject({ details: { eventIds: [121] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("does not permanently suppress a batch when wake preparation fails", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const baseResponse = client.response;
    let failGet = true;
    client.response = (method, params) => {
      if (method === "agent.orchestrator.get" && failGet) {
        failGet = false;
        throw new Error("refresh failed");
      }
      return baseResponse(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(53, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toEqual([]);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman · 1 agent update");
      expect(ctx.notifications.at(-1)).toEqual([
        "Herdsman couldn’t load agent updates · updates remain pending",
        "warning",
      ]);

      // The next wake retries the whole pending batch instead of permanently
      // suppressing the events whose load failed once.
      client.emitStream({ method: "agent.event", params: { event: event(54, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [53, 54] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });
});

describe("pi invalidated-event wake-loop regression (independent coverage)", () => {
  test("clears an invalidated event, advances through the delivered batch, and does not wake it again", async () => {
    vi.useFakeTimers();
    const invalidatedId = 301;
    const logHome = mkdtempSync(join(tmpdir(), "herdsman-pi-log-"));
    const previousHome = process.env.HERDSMAN_HOME;
    const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
    const error = vi.spyOn(console, "error").mockImplementation(() => undefined);
    process.env.HERDSMAN_HOME = logHome;
    const client = createWakeClient([event(invalidatedId, "term_agent"), event(302, "term_agent")]);
    const baseResponse = client.response;
    client.response = (method, params) => {
      if (
        method === "agent.notifications.ack" &&
        (params as { eventId: number }).eventId === invalidatedId
      ) {
        throw new Error("orchestrator event is no longer pending (invalidated)");
      }
      return baseResponse(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      // Both copies reach the transcript, which is what authorises the
      // acknowledgements below (the evidence, not the turn outcome).
      await pi.emit("message_end", wakeConsumptionEvidence([invalidatedId, 302]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);

      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: invalidatedId }],
        ["agent.notifications.ack", { eventId: 302 }],
      ]);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman");

      client.emitStream({ method: "agent.event", params: { event: event(303, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [303] } });
      // Every wake is a triggerable turn now: there is no non-triggering variant
      // that could park the update in a queue nothing drains.
      expect(pi.hiddenMessages.every(([, options]) => options?.triggerTurn === true)).toBe(true);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
      warn.mockRestore();
      error.mockRestore();
      const log = readFileSync(
        join(
          logHome,
          "logs",
          `herdsman-pi-${new Date().toISOString().slice(0, 10).replaceAll("-", "")}.log`,
        ),
        "utf8",
      );
      expect(log).toContain(`eventId=${invalidatedId}`);
      expect(log).toContain("code=orchestrator event is no longer pending (invalidated)");
      expect(warn).not.toHaveBeenCalled();
      expect(error).not.toHaveBeenCalled();
      rmSync(logHome, { recursive: true, force: true });
      if (previousHome === undefined) delete process.env.HERDSMAN_HOME;
      else process.env.HERDSMAN_HOME = previousHome;
    }
  });

  test("terminal ACK failure preserves presentedEventIds and does not re-present the event on duplicate delivery", async () => {
    vi.useFakeTimers();
    const terminalFailedId = 301;
    const client = createWakeClient([event(terminalFailedId, "term_agent")]);
    const baseResponse = client.response;
    client.response = (method, params) => {
      if (
        method === "agent.notifications.ack" &&
        (params as { eventId: number }).eventId === terminalFailedId
      ) {
        throw new Error("orchestrator event is no longer pending (invalidated)");
      }
      return baseResponse(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState: { presentedEventIds: Set<number> } | undefined;
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({
        details: { eventIds: [terminalFailedId] },
      });
      const hiddenMessageCountBefore = pi.hiddenMessages.length;

      // Finish the turn and settle, with the content reaching the transcript —
      // the evidence is what authorises the acknowledgement attempt.
      await pi.emit("message_end", wakeConsumptionEvidence([terminalFailedId]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);

      // Verify ACK was attempted and encountered terminal failure
      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: terminalFailedId }],
      ]);

      // Direct discriminating assertion: presentedEventIds must still retain the terminal failed event id
      expect(extensionState?.presentedEventIds.has(terminalFailedId)).toBe(true);

      // When subsequent events arrive (including the old terminalFailedId and a new event 302)
      client.emitStream({
        method: "agent.event",
        params: { event: event(terminalFailedId, "term_agent") },
      });
      client.emitStream({
        method: "agent.event",
        params: { event: event(302, "term_agent") },
      });
      await vi.advanceTimersByTimeAsync(500);

      // Only the new event 302 is presented; terminalFailedId must NOT be duplicated
      expect(pi.hiddenMessages.length).toBe(hiddenMessageCountBefore + 1);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [302] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("successful ACK keeps the event id in presentedEventIds so a redelivered event is never re-presented", async () => {
    vi.useFakeTimers();
    const eventId = 401;
    const client = createWakeClient([event(eventId, "term_agent")]);
    const baseResponse = client.response;
    client.response = (method, params) => {
      if (method === "agent.notifications.ack") {
        return {
          acknowledged: true,
          state: {
            ackedEventId: eventId,
            herdrSessionName: "default",
            owner: { paneId: "wB:p1", terminalId: "term_pi" },
            updatedAt: "2026-07-10T00:00:01.000Z",
            workspaceId: "wB",
          },
        };
      }
      return baseResponse(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState:
      | { presentedEventIds: Set<number>; pendingEvents: Array<{ id: number }> }
      | undefined;
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [eventId] } });

      // The content reaches the transcript, which is what authorises the
      // acknowledgement below (and records the presentation).
      await pi.emit("message_end", wakeConsumptionEvidence([eventId]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);

      // ACK succeeded and the cursor advanced through the event.
      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId }],
      ]);
      // The event left the pending projection after the ack...
      expect(extensionState?.pendingEvents.some((pending) => pending.id === eventId)).toBe(false);
      // ...but the presentation guard retains the id (old code deleted it here
      // and failed this assertion).
      expect(extensionState?.presentedEventIds.has(eventId)).toBe(true);

      // A daemon replay of the same event arrives; no wake is constructed.
      client.emitStream({ method: "agent.event", params: { event: event(eventId, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      // A genuinely new event still wakes.
      client.emitStream({ method: "agent.event", params: { event: event(402, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [402] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("retryable ACK failure keeps the event in presentedEventIds so a redelivered event is not re-presented", async () => {
    vi.useFakeTimers();
    const eventId = 411;
    const client = createWakeClient([event(eventId, "term_agent")]);
    const baseResponse = client.response;
    client.response = (method, params) => {
      if (
        method === "agent.notifications.ack" &&
        (params as { eventId: number }).eventId === eventId
      ) {
        throw Object.assign(new Error("temporary failure"), { code: "ORCHESTRATOR_BUSY" });
      }
      return baseResponse(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState:
      | { presentedEventIds: Set<number>; pendingEvents: Array<{ id: number }> }
      | undefined;
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [eventId] } });
      const hiddenCount = pi.hiddenMessages.length;

      // The content reaches the transcript, which authorises the acknowledgement
      // attempt and records the presentation.
      await pi.emit("message_end", wakeConsumptionEvidence([eventId]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);

      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId }],
      ]);
      // The failed event stays in the pending projection (for cursor-sweep
      // convergence) and its presentation guard is retained (old code deleted
      // the id on the retryable failure path and re-presented the event).
      expect(extensionState?.pendingEvents.some((pending) => pending.id === eventId)).toBe(true);
      expect(extensionState?.presentedEventIds.has(eventId)).toBe(true);

      // The daemon re-delivers the same event together with a new one.
      client.emitStream({ method: "agent.event", params: { event: event(eventId, "term_agent") } });
      client.emitStream({ method: "agent.event", params: { event: event(412, "term_agent") } });
      await vi.advanceTimersByTimeAsync(1_000);

      // Only the new event is presented; the failed one is not re-presented.
      expect(pi.hiddenMessages.length).toBe(hiddenCount + 1);
      const lastWake = pi.hiddenMessages.at(-1)?.[0] as {
        content: string;
        details?: { eventIds: number[] };
      };
      expect(lastWake.content).toContain("event: 412");
      expect(lastWake.content).not.toContain("event: 411");
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("reconnect preserves the in-flight batch and the presentation guard so the settle acks without a duplicate wake", async () => {
    vi.useFakeTimers();
    const eventId = 421;
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register")
        return connectionResponse({ events: [event(eventId, "term_agent")] });
      if (method === "agent.orchestrator.get") return connectionResponse();
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    let extensionState: { presentedEventIds: Set<number> } | undefined;
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);
      expect(pi.hiddenMessages[0]?.[0]).toMatchObject({ details: { eventIds: [eventId] } });

      // The wake turn starts; the daemon drops the socket mid-turn.
      ctx.setIdle(false);
      client.disconnect(new Error("transient disconnect"));
      await client.connect();
      await vi.advanceTimersByTimeAsync(0);
      // The in-flight batch survived the reconnect (the register response
      // re-lists the still-unacknowledged event).
      expect(extensionState?.presentedEventIds.has(eventId)).toBe(false);

      // The turn completes with the content in the transcript; the settlement must
      // ack the batch instead of re-presenting it.
      await pi.emit("message_end", wakeConsumptionEvidence([eventId]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      ctx.setIdle(true);
      await pi.emit("agent_settled", {}, ctx);
      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId }],
      ]);

      // No duplicate wake was constructed on any retry timer.
      await vi.advanceTimersByTimeAsync(1_000);
      expect(pi.hiddenMessages).toHaveLength(1);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("silently ignores an unwritable diagnostic log and preserves acknowledgement behavior", async () => {
    vi.useFakeTimers();
    const logHome = mkdtempSync(join(tmpdir(), "herdsman-pi-log-blocked-"));
    const blocker = join(logHome, "not-a-directory");
    writeFileSync(blocker, "block");
    const previousHome = process.env.HERDSMAN_HOME;
    process.env.HERDSMAN_HOME = blocker;
    const { logHerdsmanPi } = (await import(extensionModuleUrl)) as Module;
    expect(() => logHerdsmanPi("warn", "diagnostic failure")).not.toThrow();
    const client = createWakeClient([event(305, "term_agent")]);
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      expect(client.calls.some(([method]) => method === "agent.orchestrator.register")).toBe(true);
    } finally {
      restoreEnv(previous);
      vi.clearAllTimers();
      vi.useRealTimers();
      if (previousHome === undefined) delete process.env.HERDSMAN_HOME;
      else process.env.HERDSMAN_HOME = previousHome;
      rmSync(logHome, { recursive: true, force: true });
    }
  });
  test("retains a normal jump rejection and leaves the failed wake cursor unchanged", async () => {
    vi.useFakeTimers();
    const client = createWakeClient([event(311, "term_agent"), event(313, "term_agent")]);
    const baseResponse = client.response;
    client.response = (method, params) => {
      if (method === "agent.notifications.ack" && (params as { eventId: number }).eventId === 313) {
        throw new Error("Only the next pending orchestrator event can be acknowledged");
      }
      return baseResponse(method, params);
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      await vi.advanceTimersByTimeAsync(500);
      // Both delivered copies reach the transcript, which is what authorises the
      // acknowledgements below (the evidence, not the turn outcome).
      await pi.emit("agent_start", {}, ctx);
      await pi.emit("message_end", wakeConsumptionEvidence([311, 313]), ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);

      client.emitStream({ method: "agent.event", params: { event: event(314, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
      // The 311 ack advanced the cursor past it, so the next wake's `eventIds`
      // provenance list starts at the failed 313 — it stays pending (and listed),
      // while only the fresh event content is injected.
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [313, 314] } });
      expect(client.calls.filter(([method]) => method === "agent.notifications.ack")).toEqual([
        ["agent.notifications.ack", { eventId: 311 }],
        ["agent.notifications.ack", { eventId: 313 }],
      ]);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("prunes pending events through the acknowledged id from a register response", async () => {
    vi.useFakeTimers();
    const client = createWakeClient([event(321, "term_agent"), event(322, "term_agent")], 321);
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages.at(-1)?.[0]).toMatchObject({ details: { eventIds: [322] } });
      expect(pi.hiddenMessages.at(-1)?.[0]).not.toMatchObject({ details: { eventIds: [321] } });
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("prunes orphan pending events when orchestrator.get returns empty and skips wake", async () => {
    vi.useFakeTimers();
    let extensionState: { pendingEvents: AgentEventWireRecord[] } | undefined;
    const client = createFakeClient();
    client.response = (method) => {
      if (method === "agent.orchestrator.register") return connectionResponse({ events: [] });
      if (method === "agent.orchestrator.get") return connectionResponse({ events: [] });
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });
      client.emitStream({ method: "agent.event", params: { event: event(501, "term_agent") } });
      expect(extensionState?.pendingEvents).toHaveLength(1);

      await vi.advanceTimersByTimeAsync(500);

      expect(extensionState?.pendingEvents).toHaveLength(0);
      expect(pi.hiddenMessages).toHaveLength(0);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("retains local pending events beyond 100-event window when server response is truncated", async () => {
    vi.useFakeTimers();
    let extensionState: { pendingEvents: AgentEventWireRecord[] } | undefined;
    const client = createFakeClient();

    const server100Events: AgentEventWireRecord[] = Array.from({ length: 100 }, (_, i) =>
      event(i + 1, "term_agent"),
    );

    client.response = (method) => {
      if (method === "agent.orchestrator.register") {
        return connectionResponse({ events: server100Events });
      }
      if (method === "agent.orchestrator.get") {
        return connectionResponse({ events: server100Events });
      }
      if (method === "agent.list") return agentListResponse();
      return { acknowledged: true };
    };

    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx, {
        onStateExposed: (state) => {
          extensionState = state;
        },
      });

      expect(extensionState?.pendingEvents).toHaveLength(100);

      client.emitStream({ method: "agent.event", params: { event: event(105, "term_agent") } });
      expect(extensionState?.pendingEvents).toHaveLength(101);

      await vi.advanceTimersByTimeAsync(500);

      expect(extensionState?.pendingEvents.some((e) => e.id === 105)).toBe(true);
      expect(extensionState?.pendingEvents).toHaveLength(101);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });
});

describe("herdsman-pi daemon keepalive", () => {
  test("pings every 30s after register and stops on disconnect", async () => {
    vi.useFakeTimers();
    const client = createFakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx();
    const previous = withHerdrEnv();
    const pings = () => client.calls.filter(([method]) => method === "agent.ping");
    try {
      await startExtension(client, pi, ctx);
      expect(pings()).toEqual([]);

      await vi.advanceTimersByTimeAsync(29_999);
      expect(pings()).toEqual([]);

      await vi.advanceTimersByTimeAsync(1);
      expect(pings()).toEqual([["agent.ping", {}]]);

      await vi.advanceTimersByTimeAsync(30_000);
      expect(pings()).toHaveLength(2);

      client.disconnect(new Error("socket closed"));
      await vi.advanceTimersByTimeAsync(30_000);
      expect(pings()).toHaveLength(2);

      await client.connect();
      await vi.advanceTimersByTimeAsync(30_000);
      expect(pings()).toHaveLength(3);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });

  test("clears keepalive on session_shutdown and client.close", async () => {
    vi.useFakeTimers();
    const shutdownClient = createFakeClient();
    const closeClient = createFakeClient();
    const shutdownPi = createFakePi();
    const closePi = createFakePi();
    const shutdownCtx = fakeCtx();
    const closeCtx = fakeCtx({ sessionId: "pi-session-close" });
    const previous = withHerdrEnv();
    try {
      await startExtension(shutdownClient, shutdownPi, shutdownCtx);
      await vi.advanceTimersByTimeAsync(30_000);
      expect(shutdownClient.calls.filter(([method]) => method === "agent.ping")).toHaveLength(1);
      await shutdownPi.emit("session_shutdown");
      await vi.advanceTimersByTimeAsync(30_000);
      expect(shutdownClient.calls.filter(([method]) => method === "agent.ping")).toHaveLength(1);
      expect(shutdownClient.closed).toBe(true);

      await startExtension(closeClient, closePi, closeCtx);
      await vi.advanceTimersByTimeAsync(30_000);
      expect(closeClient.calls.filter(([method]) => method === "agent.ping")).toHaveLength(1);
      closeClient.close();
      await vi.advanceTimersByTimeAsync(30_000);
      expect(closeClient.calls.filter(([method]) => method === "agent.ping")).toHaveLength(1);
      expect(closeClient.closed).toBe(true);
    } finally {
      vi.clearAllTimers();
      vi.useRealTimers();
      restoreEnv(previous);
    }
  });
});

function createWakeClient(replayedEvents: AgentEventWireRecord[] = [], ackedEventId?: number) {
  const client = createFakeClient();
  for (const ev of replayedEvents) {
    if (ackedEventId === undefined || ev.id > ackedEventId) {
      client.currentEvents.set(ev.id, ev);
    }
  }
  client.response = (method, _params) => {
    if (method === "agent.orchestrator.register") {
      return connectionResponse({
        ...(ackedEventId === undefined ? {} : { ackedEventId }),
        events: replayedEvents,
      });
    }
    if (method === "agent.orchestrator.get") {
      return connectionResponse({
        ...(ackedEventId === undefined ? {} : { ackedEventId }),
        events: [...client.currentEvents.values()],
      });
    }
    if (method === "agent.list") return agentListResponse();
    return { acknowledged: true };
  };
  return client;
}

async function startExtension(
  client: FakeClient,
  pi: FakePi,
  ctx: ReturnType<typeof fakeCtx>,
  options: {
    onStateExposed?: (state: {
      pendingEvents: AgentEventWireRecord[];
      presentedEventIds: Set<number>;
    }) => void;
    wakeFilter?: { enabled: boolean; extraPatterns: readonly string[] };
  } = {},
): Promise<() => Promise<void>> {
  const completions: Promise<void>[] = [];
  const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
  createHerdsmanPiExtension({
    clientFactory: () => client,
    onTurnCompletionSignal: (completion) => completions.push(completion),
    ...options,
  })(pi);
  await pi.emit("session_start", {}, ctx);
  await client.connect();
  return async () => {
    await Promise.all(completions);
  };
}

let activeFakeClient: { currentEvents: Map<number, AgentEventWireRecord> } | undefined;

function createFakeClient() {
  let connected: (() => Promise<void> | void) | undefined;
  let disconnected: ((error: Error) => void) | undefined;
  let stream: ((message: DaemonStreamMessage) => void) | undefined;
  const currentEvents = new Map<number, AgentEventWireRecord>();
  let currentAckedEventId = 0;
  const client = {
    calls: [] as Array<[string, unknown]>,
    closed: false,
    currentEvents,
    response: (_method: string, _params: unknown): unknown => connectionResponse(),
    close() {
      client.closed = true;
    },
    async connect() {
      try {
        await connected?.();
      } catch (error) {
        disconnected?.(error instanceof Error ? error : new Error(String(error)));
      }
    },
    disconnect(error = new Error("disconnected")) {
      disconnected?.(error);
    },
    emitStream(message: DaemonStreamMessage) {
      if (
        message.method === "agent.event" &&
        message.params &&
        typeof message.params === "object" &&
        "event" in message.params &&
        (message.params as { event: AgentEventWireRecord }).event
      ) {
        const ev = (message.params as { event: AgentEventWireRecord }).event;
        if (ev.terminalId && ev.terminalId !== "term_pi" && ev.id > currentAckedEventId) {
          currentEvents.set(ev.id, ev);
        }
      }
      stream?.(message);
    },
    get onConnected() {
      return connected;
    },
    set onConnected(handler: (() => Promise<void> | void) | undefined) {
      connected = handler;
    },
    get onDisconnected() {
      return disconnected;
    },
    set onDisconnected(handler: ((error: Error) => void) | undefined) {
      disconnected = handler;
    },
    get onStreamMessage() {
      return stream;
    },
    set onStreamMessage(handler: ((message: DaemonStreamMessage) => void) | undefined) {
      stream = handler;
    },
    async request(method: string, params: unknown) {
      client.calls.push([method, params]);
      const res = await client.response(method, params);
      if (
        method === "agent.notifications.ack" &&
        params &&
        typeof params === "object" &&
        "eventId" in params
      ) {
        const ackId = (params as { eventId: number }).eventId;
        currentEvents.delete(ackId);
        if (ackId > currentAckedEventId) currentAckedEventId = ackId;
      }
      const acked =
        typeof res === "object" && res !== null
          ? ((res as { ackedEventId?: number }).ackedEventId ??
            (res as { state?: { ackedEventId?: number } }).state?.ackedEventId)
          : undefined;
      if (acked !== undefined && acked > currentAckedEventId) {
        currentAckedEventId = acked;
        for (const id of currentEvents.keys()) {
          if (id <= currentAckedEventId) currentEvents.delete(id);
        }
      }
      if (
        method === "agent.orchestrator.register" &&
        res &&
        typeof res === "object" &&
        "events" in res &&
        Array.isArray((res as { events: unknown }).events)
      ) {
        currentEvents.clear();
        for (const ev of (res as { events: AgentEventWireRecord[] }).events) {
          if (ev.terminalId && ev.terminalId !== "term_pi" && ev.id > currentAckedEventId) {
            currentEvents.set(ev.id, ev);
          }
        }
      }
      return res;
    },
  };
  activeFakeClient = client;
  return client;
}

function createFakePi() {
  const handlers = new Map<string, Handler>();
  const commands = new Map<string, Command>();
  return {
    commands,
    customMessages: [] as Array<
      [
        { content: string; customType: string; details?: unknown; display: boolean },
        { deliverAs?: string; triggerTurn?: boolean } | undefined,
      ]
    >,
    entries: [] as unknown[],
    handlers,
    hiddenMessages: [] as Array<
      [
        { content: string; customType: string; details?: unknown; display: boolean },
        { deliverAs?: string; triggerTurn?: boolean } | undefined,
      ]
    >,
    messageRenderers: new Map<string, Handler>(),
    appendEntry(customType: string, data: unknown) {
      this.entries.push([customType, data]);
    },
    async command(args: string, ctx: ReturnType<typeof fakeCtx>) {
      await commands.get("herdsman")?.handler(args, ctx);
    },
    emit: async (name: string, ...args: unknown[]) => handlers.get(name)?.(...args),
    async emitContext(messages: unknown[], ctx: ReturnType<typeof fakeCtx>) {
      return (
        (
          (await handlers.get("context")?.({ messages, type: "context" }, ctx)) as
            | { messages?: unknown[] }
            | undefined
        )?.messages ?? messages
      );
    },
    on: (name: string, handler: Handler) => handlers.set(name, handler),
    registerCommand(name: string, options: Command) {
      commands.set(name, options);
    },
    registerMessageRenderer(customType: string, renderer: Handler) {
      this.messageRenderers.set(customType, renderer);
    },
    registerTool() {},
    sendMessage(message: unknown, options?: unknown) {
      const target =
        (message as { display?: boolean }).display === false
          ? this.hiddenMessages
          : this.customMessages;
      target.push([message as never, options as never]);
    },
    setSessionName() {},
  };
}

function fakeCtx(options: { idle?: boolean; sessionFile?: string; sessionId?: string } = {}) {
  const runtime = { idle: options.idle ?? false };
  const ctx = {
    abort() {
      ctx.aborts += 1;
    },
    aborts: 0,
    isIdle: () => runtime.idle,
    notifications: [] as Array<[string, string | undefined]>,
    sessionManager: {
      getSessionFile: () => options.sessionFile ?? "/tmp/pi-session.jsonl",
      getSessionId: () => options.sessionId ?? "pi-session",
    },
    setIdle(value: boolean) {
      runtime.idle = value;
    },
    statuses: new Map<string, string | undefined>(),
    widgets: new Map<string, string[] | undefined>(),
    ui: {
      theme: {
        bg: (_color: string, text: string) => text,
        bold: (text: string) => text,
        fg: (_color: string, text: string) => text,
      },
      notify(message: string, level?: string) {
        ctx.notifications.push([message, level]);
      },
      setStatus(key: string, value?: string) {
        ctx.statuses.set(key, value);
      },
      setWidget(key: string, value?: string[]) {
        if (value?.some((line) => typeof line !== "string")) {
          throw new Error("widget lines must be strings");
        }
        ctx.widgets.set(key, value);
      },
    },
  };
  return ctx;
}

function connectionResponse(
  options: {
    ackedEventId?: number;
    changed?: boolean;
    context?: AgentWorkspaceContextSnapshot | null;
    events?: AgentEventWireRecord[];
    ownerTerminalId?: string | null;
    paneId?: string;
    workspaceId?: string;
  } = {},
) {
  const paneId = options.paneId ?? "wB:p1";
  const workspaceId = options.workspaceId ?? "wB";
  const ownerTerminalId =
    options.ownerTerminalId === undefined ? "term_pi" : options.ownerTerminalId;
  const events =
    options.events !== undefined
      ? options.events
      : activeFakeClient
        ? [...activeFakeClient.currentEvents.values()]
        : [];
  return {
    ...(options.changed === undefined ? {} : { changed: options.changed }),
    ...(options.context === undefined ? {} : { context: options.context }),
    ...(options.ackedEventId === undefined ? {} : { ackedEventId: options.ackedEventId }),
    events,
    presence: {
      connectedAt: 1,
      herdrSessionName: "default",
      paneId,
      subscriberId: "pi-session",
      terminalId: "term_pi",
      workspaceId,
    },
    state: {
      ackedEventId: options.ackedEventId ?? 0,
      herdrSessionName: "default",
      owner: ownerTerminalId
        ? {
            paneId: ownerTerminalId === "term_pi" ? paneId : "wB:p-other",
            terminalId: ownerTerminalId,
          }
        : null,
      updatedAt: "2026-07-10T00:00:00.000Z",
      workspaceId,
    },
  };
}

function event(
  id: number,
  terminalId: string | null,
  options: {
    compactHistory?: Record<string, unknown>;
    paneId?: string;
    payload?: Record<string, unknown>;
    type?: string;
    workspaceId?: string;
  } = {},
): AgentEventWireRecord {
  return {
    compactHistory: options.compactHistory ?? { lastAssistantMessage: { text: "done" } },
    id,
    paneId: options.paneId ?? "wB:p-agent",
    payload: { agent: "claude", ...options.payload },
    terminalId,
    type: options.type ?? "agent.done",
    workspaceId: options.workspaceId ?? "wB",
  };
}

function assistantMessage(stopReason: string) {
  return {
    message: {
      content: [{ text: "completed", type: "text" }],
      role: "assistant",
      stopReason,
      turnId: "turn-1",
    },
  };
}

/**
 * The `message_end` a hidden wake message produces once a run has carried its
 * content into the transcript. It is the only consumption evidence the
 * extension accepts, and it names the ids the message actually presented
 * (`eventIds` is the wider provenance list and proves nothing on its own).
 */
function wakeConsumptionEvidence(presentedEventIds: number[], eventIds?: number[]) {
  return {
    message: {
      content: "[HERDSMAN AGENT UPDATES] …",
      customType: "herdsman-wake-context",
      details: {
        eventIds: eventIds ?? presentedEventIds,
        presentedEventIds,
      },
      display: false,
      role: "custom",
    },
  };
}

function contextSnapshot(lastAssistantText: string): AgentWorkspaceContextSnapshot {
  return {
    agents: [
      {
        agent: "claude",
        agentStatus: "idle",
        history: { lastAssistantMessage: { text: lastAssistantText } },
        paneId: "wB:p-agent",
        terminalId: "term_agent",
      },
    ],
    herdrSessionName: "default",
    updatedAt: "2026-07-16T00:00:00.000Z",
    workspaceId: "wB",
  };
}

function agentListResponse() {
  return {
    agents: [
      {
        agent: "pi",
        agentStatus: "idle",
        history: {
          lastAssistantMessage: { text: "ready" },
          lastUserMessage: { text: "work" },
        },
        paneId: "wB:p1",
      },
    ],
  };
}

function roleChange(
  previousTerminalId: string | null,
  currentTerminalId: string | null,
  currentPaneId = "wB:p1",
) {
  return {
    current: {
      ackedEventId: 0,
      herdrSessionName: "default",
      owner: currentTerminalId ? { paneId: currentPaneId, terminalId: currentTerminalId } : null,
      updatedAt: "2026-07-10T00:00:01.000Z",
      workspaceId: "wB",
    },
    previous: {
      ackedEventId: 0,
      herdrSessionName: "default",
      owner: previousTerminalId ? { paneId: "wB:p1", terminalId: previousTerminalId } : null,
      updatedAt: "2026-07-10T00:00:00.000Z",
      workspaceId: "wB",
    },
    reason: "claimed" as const,
  };
}

function movedRoleChange() {
  const change = roleChange("term_pi", "term_pi", "wC:p1");
  return {
    ...change,
    current: { ...change.current, workspaceId: "wC" },
  };
}

function withHerdrEnv(options: { paneId?: string; workspaceId?: string } = {}) {
  const previous = {
    HERDR_ENV: process.env.HERDR_ENV,
    HERDR_PANE_ID: process.env.HERDR_PANE_ID,
    HERDR_SOCKET_PATH: process.env.HERDR_SOCKET_PATH,
    HERDR_WORKSPACE_ID: process.env.HERDR_WORKSPACE_ID,
  };
  process.env.HERDR_ENV = "1";
  process.env.HERDR_PANE_ID = options.paneId ?? "wB:p1";
  process.env.HERDR_SOCKET_PATH = "/tmp/herdr.sock";
  process.env.HERDR_WORKSPACE_ID = options.workspaceId ?? "wB";
  return previous;
}

function restoreEnv(previous: Record<string, string | undefined>) {
  for (const [key, value] of Object.entries(previous)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
}

async function tick(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0));
}

describe("herdsman-pi turn completion signal", () => {
  test("signals turn completion with confirmed=true when the final message is already on disk", async () => {
    const dir = mkdtempSync(join(tmpdir(), "herdsman-pi-turn-"));
    const sessionPath = join(dir, "pi-session.jsonl");
    const previous = withHerdrEnv();
    try {
      writeFileSync(
        sessionPath,
        `${JSON.stringify({
          message: { content: [{ text: "completed", type: "text" }], role: "assistant" },
          type: "message",
        })}\n`,
      );
      const client = createFakeClient();
      const pi = createFakePi();
      const ctx = fakeCtx({ idle: true, sessionFile: sessionPath });
      const flushTurnCompletion = await startExtension(client, pi, ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await flushTurnCompletion();
      expect(client.calls).toContainEqual([
        "agent.turn.completed",
        {
          confirmed: true,
          expectedText: "completed",
          herdrSessionName: "default",
          paneId: "wB:p1",
          terminalId: "term_pi",
          workspaceId: "wB",
        },
      ]);
    } finally {
      restoreEnv(previous);
      rmSync(dir, { force: true, recursive: true });
    }
  });

  test("signals turn completion with confirmed=false when the write is not observed before the timeout", async () => {
    vi.useFakeTimers();
    const dir = mkdtempSync(join(tmpdir(), "herdsman-pi-turn-"));
    const sessionPath = join(dir, "pi-session.jsonl");
    const previous = withHerdrEnv();
    try {
      writeFileSync(sessionPath, "no final message here\n");
      const client = createFakeClient();
      const pi = createFakePi();
      const ctx = fakeCtx({ idle: true, sessionFile: sessionPath });
      const flushTurnCompletion = await startExtension(client, pi, ctx);
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await Promise.resolve();
      await vi.advanceTimersByTimeAsync(3_100);
      await flushTurnCompletion();
      expect(client.calls).toContainEqual([
        "agent.turn.completed",
        expect.objectContaining({ confirmed: false }),
      ]);
    } finally {
      vi.useRealTimers();
      restoreEnv(previous);
      rmSync(dir, { force: true, recursive: true });
    }
  });

  test("does not signal turn completion for intermediate or non-terminal messages", async () => {
    const client = createFakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      await pi.emit("message_end", assistantMessage("toolUse"), ctx);
      await pi.emit("message_end", { message: { role: "user" } }, ctx);
      await pi.emit("message_end", assistantMessage("aborted"), ctx);
      await tick();
      expect(client.calls.filter(([method]) => method === "agent.turn.completed")).toEqual([]);
    } finally {
      restoreEnv(previous);
    }
  });

  test("omits expectedText from RPC when assistant message has no extractable text", async () => {
    const dir = mkdtempSync(join(tmpdir(), "herdsman-pi-turn-"));
    const sessionPath = join(dir, "pi-session.jsonl");
    const previous = withHerdrEnv();
    try {
      writeFileSync(
        sessionPath,
        `${JSON.stringify({
          message: {
            content: [{ type: "thinking", text: "hidden reasoning" }],
            role: "assistant",
          },
          type: "message",
        })}
`,
      );
      const client = createFakeClient();
      const pi = createFakePi();
      const ctx = fakeCtx({ idle: true, sessionFile: sessionPath });
      const flushTurnCompletion = await startExtension(client, pi, ctx);
      await pi.emit(
        "message_end",
        {
          message: {
            content: [{ type: "thinking", text: "hidden reasoning" }],
            role: "assistant",
            stopReason: "stop",
            turnId: "turn-1",
          },
        },
        ctx,
      );
      await flushTurnCompletion();
      // The call must NOT contain expectedText when textFromContent returns null.
      const turnCompletedCalls = client.calls.filter(
        ([method]) => method === "agent.turn.completed",
      );
      expect(turnCompletedCalls.length).toBeGreaterThan(0);
      for (const [, params] of turnCompletedCalls) {
        expect(params).not.toHaveProperty("expectedText");
      }
    } finally {
      restoreEnv(previous);
      rmSync(dir, { force: true, recursive: true });
    }
    // ~61% of the default 5s budget even in an isolated single run (3025ms
    // measured), i.e. above the >50% bar for raising only this case's budget:
    // D16 in .agents/notes/20260930-terminal-event-delivery-open-items.md.
    // ~3s is intrinsic: TURN_SIGNAL_TIMEOUT_MS = 3_000 (packages/herdsman-pi/src/turn-signal.ts:3)
    // and this case's expectedText="" makes the candidate.length > 0 guard always false.
  }, 30_000);
});
