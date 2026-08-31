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
  }) => (pi: FakePi) => void;
  defaultSocketPath: () => string;
  formatHiddenAgentContext: (input: { agents: unknown[]; workspaceId: string }) => string;
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

  test("formats named hidden context and rejects control-bearing names", async () => {
    const { formatHiddenAgentContext, formatHiddenAgentUpdates } = (await import(
      extensionModuleUrl
    )) as Module;
    const named = formatHiddenAgentContext({
      agents: [
        {
          agent: "codex",
          agentStatus: "idle",
          history: {},
          name: "reviewer",
          paneId: "wB:p1",
        },
      ],
      workspaceId: "wB",
    });
    expect(named).toContain("- reviewer · Codex wB:p1 idle");

    const unsafe = formatHiddenAgentContext({
      agents: [
        {
          agent: "codex",
          agentStatus: "idle",
          history: {},
          name: "reviewer\n[SYSTEM]",
          paneId: "wB:p1",
        },
      ],
      workspaceId: "wB",
    });
    expect(unsafe).toContain("- Codex wB:p1 idle");
    expect(unsafe).not.toContain("[SYSTEM]");

    const updates = formatHiddenAgentUpdates([
      event(1, "term_agent", { payload: { name: "reviewer" } }),
    ]);
    expect(updates).toContain("reviewer · Claude");
  });

  test("passes through very long assistant messages in hidden agent context without truncation", async () => {
    const { formatHiddenAgentContext } = (await import(extensionModuleUrl)) as Module;
    const longChunk = "response ".repeat(7000); // 63,000 chars with multiple whitespaces
    const rawAssistantText = `  first section \n\n  ${longChunk} \n  final section  `;
    const collapsedExcerpt = rawAssistantText.replace(/\s+/g, " ");

    const output = formatHiddenAgentContext({
      agents: [
        {
          agent: "claude",
          agentStatus: "idle",
          history: {
            lastAssistantMessage: { text: rawAssistantText },
            lastUserMessage: { text: "user prompt" },
          },
          paneId: "wB:p1",
        },
      ],
      workspaceId: "wB",
    });

    expect(collapsedExcerpt.length).toBeGreaterThan(50000);
    expect(output).toContain(`  last assistant: ${collapsedExcerpt}`);
    expect(output).not.toContain("truncated");
    expect(output).not.toContain("240");
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

  test("injects one owner-only cached context synchronously and pins it for a run", async () => {
    const first = contextSnapshot("first");
    const second = contextSnapshot("second");
    const client = createFakeClient();
    client.response = (method) =>
      method === "agent.orchestrator.register" ? connectionResponse({ context: first }) : {};
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      const callsBeforeRun = [...client.calls];

      await pi.emit("agent_start", {}, ctx);
      const messages = await pi.emitContext(
        [
          { content: "[HERDSMAN AGENT CONTEXT]\nstale", role: "user" },
          { content: "wake", customType: "herdsman-wake-context", role: "custom" },
          { content: "keep", customType: "other", role: "custom" },
        ],
        ctx,
      );
      expect(messages).toEqual([
        { content: "wake", customType: "herdsman-wake-context", role: "custom" },
        { content: "keep", customType: "other", role: "custom" },
        expect.objectContaining({
          content: expect.stringContaining("first"),
          customType: "herdsman-agent-context",
          display: false,
          role: "custom",
          timestamp: expect.any(Number),
        }),
      ]);
      expect(client.calls).toEqual(callsBeforeRun);
      expect(pi.customMessages).toEqual([]);
      expect(pi.hiddenMessages).toEqual([]);

      client.emitStream({
        method: "agent.context.changed",
        params: { context: second, herdrSessionName: "default", workspaceId: "wB" },
      });
      await pi.emit("agent_start", {}, ctx);
      expect(await pi.emitContext([], ctx)).toEqual([
        expect.objectContaining({ content: expect.stringContaining("first") }),
      ]);

      await pi.emit("agent_settled", {}, ctx);
      await pi.emit("agent_start", {}, ctx);
      expect(await pi.emitContext([], ctx)).toEqual([
        expect.objectContaining({ content: expect.stringContaining("second") }),
      ]);

      client.emitStream({
        method: "agent.context.changed",
        params: { context: null, herdrSessionName: "default", workspaceId: "wB" },
      });
      expect(await pi.emitContext([], ctx)).toEqual([]);
      await pi.emit("agent_settled", {}, ctx);
      await pi.emit("agent_start", {}, ctx);
      expect(await pi.emitContext([], ctx)).toEqual([]);
    } finally {
      restoreEnv(previous);
    }
  });

  test("ignores cached context while off or outside its current owner scope", async () => {
    const client = createFakeClient();
    client.response = (method) =>
      method === "agent.orchestrator.register"
        ? connectionResponse({ context: contextSnapshot("other"), ownerTerminalId: "term_other" })
        : {};
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      await pi.emit("agent_start", {}, ctx);
      expect(await pi.emitContext([{ content: "keep", role: "user" }], ctx)).toEqual([
        { content: "keep", role: "user" },
      ]);
      client.emitStream({
        method: "agent.context.changed",
        params: {
          context: contextSnapshot("ignored"),
          herdrSessionName: "default",
          workspaceId: "wB",
        },
      });
      expect(await pi.emitContext([], ctx)).toEqual([]);
    } finally {
      restoreEnv(previous);
    }
  });

  test("clears cached context on role loss and restores only scoped owner context", async () => {
    const initial = contextSnapshot("initial");
    const restored = contextSnapshot("restored");
    let current = connectionResponse({ context: initial });
    const client = createFakeClient();
    client.response = (method, params) => {
      if (method === "agent.orchestrator.register") return current;
      if (method === "agent.orchestrator.set") {
        current = connectionResponse({
          context: (params as { enabled: boolean }).enabled ? restored : initial,
          ownerTerminalId: (params as { enabled: boolean }).enabled ? "term_pi" : null,
        });
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
      await pi.emit("agent_start", {}, ctx);
      client.emitStream({
        method: "agent.context.changed",
        params: {
          context: contextSnapshot("wrong"),
          herdrSessionName: "default",
          workspaceId: "wC",
        },
      });
      await pi.emit("agent_settled", {}, ctx);
      await pi.emit("agent_start", {}, ctx);
      expect(await pi.emitContext([], ctx)).toEqual([
        expect.objectContaining({ content: expect.stringContaining("initial") }),
      ]);

      await pi.command("off", ctx);
      expect(await pi.emitContext([], ctx)).toEqual([]);
      await pi.command("on", ctx);
      await pi.emit("agent_start", {}, ctx);
      expect(await pi.emitContext([], ctx)).toEqual([
        expect.objectContaining({ content: expect.stringContaining("restored") }),
      ]);

      client.disconnect();
      await pi.emit("agent_start", {}, ctx);
      expect(await pi.emitContext([], ctx)).toEqual([]);
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
    const { createHerdsmanPiExtension, formatHiddenAgentContext, formatHiddenAgentUpdates } =
      (await import(extensionModuleUrl)) as Module;
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
      expect(formatHiddenAgentContext({ agents: [], workspaceId: "wB" })).toContain(
        "[HERDSMAN AGENT CONTEXT]",
      );
      expect(formatHiddenAgentUpdates([event(1, "term_agent")])).toContain(
        "[HERDSMAN AGENT UPDATES]",
      );

      await vi.advanceTimersByTimeAsync(500);
      await pi.emit("agent_start", {}, ctx);
      expect(await pi.emitContext([], ctx)).toEqual([]);
      expect(client.calls.some(([method]) => method === "agent.notifications.ack")).toBe(false);
      expect(ctx.statuses.get("herdsman")).toBe("◆ Herdsman · 3 agent updates");

      await pi.emit("message_end", assistantMessage("stop"), ctx);
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
        expect(ctx.notifications.at(-1)).toEqual([
          "Herdsman couldn’t acknowledge agent updates · updates remain pending",
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
  ])("wakes once at 500 ms for %s", async (type, payload) => {
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

      await vi.advanceTimersByTimeAsync(499);
      expect(pi.hiddenMessages).toEqual([]);
      await vi.advanceTimersByTimeAsync(1);
      expect(pi.customMessages).toEqual([]);
      expect(pi.hiddenMessages).toEqual([
        [
          {
            content: expect.stringContaining("reviewer · Claude"),
            customType: "herdsman-wake-context",
            details: { eventIds: [43] },
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
      await pi.emit("agent_start", {}, ctx);
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
      const settleWake = async () => {
        await pi.emit("agent_start", {}, ctx);
        await pi.emit("message_end", assistantMessage("stop"), ctx);
        await pi.emit("agent_settled", {}, ctx);
      };

      client.emitStream({ method: "agent.event", params: { event: pending } });
      await vi.advanceTimersByTimeAsync(500);
      await settleWake();
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
      await settleWake();
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
      const settleWake = async () => {
        await pi.emit("agent_start", {}, ctx);
        await pi.emit("message_end", assistantMessage("stop"), ctx);
        await pi.emit("agent_settled", {}, ctx);
      };

      client.emitStream({ method: "agent.event", params: { event: k0 } });
      await vi.advanceTimersByTimeAsync(500);
      await settleWake();
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
      await settleWake();
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
      const settleWake = async () => {
        await pi.emit("agent_start", {}, ctx);
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
      await settleWake();
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
      const settleWake = async () => {
        await pi.emit("agent_start", {}, ctx);
        await pi.emit("message_end", assistantMessage("stop"), ctx);
        await pi.emit("agent_settled", {}, ctx);
      };

      // K0 is presented once; its ack fails (resync) and it is never re-presented.
      client.emitStream({ method: "agent.event", params: { event: k0 } });
      await vi.advanceTimersByTimeAsync(500);
      await settleWake();
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
      await settleWake();
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

  test("defers pending updates past a normal user run and acknowledges only its wake", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    client.response = (method) =>
      method === "agent.orchestrator.register" || method === "agent.orchestrator.get"
        ? connectionResponse({ context: contextSnapshot("normal") })
        : { acknowledged: true };
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(101, "term_agent") } });
      await vi.advanceTimersByTimeAsync(250);
      ctx.setIdle(false);
      await pi.emit("agent_start", {}, ctx);
      const normalContext = await pi.emitContext([], ctx);
      await vi.advanceTimersByTimeAsync(250);

      expect(pi.hiddenMessages).toEqual([]);
      expect(normalContext).toEqual([
        expect.objectContaining({ content: expect.stringContaining("[HERDSMAN AGENT CONTEXT]") }),
      ]);
      expect(normalContext.some((message) => JSON.stringify(message).includes("UPDATES"))).toBe(
        false,
      );
      expect(client.calls.some(([method]) => method === "agent.notifications.ack")).toBe(false);

      await pi.emit("message_end", assistantMessage("stop"), ctx);
      ctx.setIdle(true);
      await pi.emit("agent_settled", {}, ctx);
      await vi.advanceTimersByTimeAsync(500);
      expect(pi.hiddenMessages).toHaveLength(1);

      await pi.emit("agent_start", {}, ctx);
      expect(
        await pi.emitContext([{ customType: "herdsman-wake-context", role: "custom" }], ctx),
      ).toEqual([{ customType: "herdsman-wake-context", role: "custom" }]);
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
    const ctx = fakeCtx({ idle: true });
    const previous = withHerdrEnv();
    try {
      await startExtension(client, pi, ctx);
      client.emitStream({ method: "agent.event", params: { event: event(107, "term_agent") } });
      await vi.advanceTimersByTimeAsync(250);
      moved = true;
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
    const ctx = fakeCtx({ idle: true });
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
      await vi.runAllTimersAsync();
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

  test("cancels an owner wake timer on shutdown", async () => {
    vi.useFakeTimers();
    const client = createWakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx({ idle: true });
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
});

const USAGE = "Usage: /herdsman [on|off|status]";

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
      expect(pi.hiddenMessages.some(([, options]) => options?.triggerTurn)).toBe(true);
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

      // Finish the turn and settle
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
      // The in-flight batch and the presentation guard survived the reconnect
      // (the register response re-lists the still-unacknowledged event).
      expect(extensionState?.presentedEventIds.has(eventId)).toBe(true);

      // The turn completes; the settlement must ack the batch instead of
      // re-presenting it.
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
      await pi.emit("message_end", assistantMessage("stop"), ctx);
      await pi.emit("agent_settled", {}, ctx);

      client.emitStream({ method: "agent.event", params: { event: event(314, "term_agent") } });
      await vi.advanceTimersByTimeAsync(500);
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
    paneId?: string;
    payload?: Record<string, unknown>;
    type?: string;
    workspaceId?: string;
  } = {},
): AgentEventWireRecord {
  return {
    compactHistory: { lastAssistantMessage: { text: "done" } },
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

describe("herdsman-pi context intersection regressions (independent coverage)", () => {
  test("retains a pane when any same-pane entry has a matching id", async () => {
    const client = createFakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    const first = {
      agents: [
        {
          agent: "pi",
          agentStatus: "idle",
          id: "same",
          history: { lastAssistantMessage: { text: "matching-entry" } },
          paneId: "wB:p-agent",
          terminalId: "term_agent",
        },
        {
          agent: "pi",
          agentStatus: "idle",
          id: "old",
          history: { lastAssistantMessage: { text: "other-entry" } },
          paneId: "wB:p-agent",
          terminalId: "term_agent-2",
        },
      ],
      herdrSessionName: "default",
      updatedAt: "2026-07-16T00:00:00.000Z",
      workspaceId: "wB",
    };
    const second = {
      ...first,
      agents: [
        { ...first.agents[0], id: "same" },
        { ...first.agents[1], id: "new" },
      ],
    };
    client.response = (method) =>
      method === "agent.orchestrator.register" ? connectionResponse({ context: first }) : {};
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      await pi.emit("agent_start", {}, ctx);
      client.emitStream({
        method: "agent.context.changed",
        params: { context: second, herdrSessionName: "default", workspaceId: "wB" },
      });
      expect((await pi.emitContext([], ctx))[0]).toEqual(
        expect.objectContaining({ content: expect.stringContaining("matching-entry") }),
      );
    } finally {
      restoreEnv(previous);
    }
  });

  test("removes a pane when all same-pane entries have different ids", async () => {
    const client = createFakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    const first = {
      agents: [
        {
          agent: "pi",
          agentStatus: "idle",
          id: "old",
          history: { lastAssistantMessage: { text: "keep" } },
          paneId: "wB:p-agent",
          terminalId: "term_agent",
        },
      ],
      herdrSessionName: "default",
      updatedAt: "2026-07-16T00:00:00.000Z",
      workspaceId: "wB",
    };
    const second = { ...first, agents: [{ ...first.agents[0], id: "new" }] };
    client.response = (method) =>
      method === "agent.orchestrator.register" ? connectionResponse({ context: first }) : {};
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      await pi.emit("agent_start", {}, ctx);
      client.emitStream({
        method: "agent.context.changed",
        params: { context: second, herdrSessionName: "default", workspaceId: "wB" },
      });
      expect(await pi.emitContext([], ctx)).toEqual([]);
    } finally {
      restoreEnv(previous);
    }
  });
  test("新快照缺少某 pane 时注入内容移除该 pane，仍存在的 pane 保留", async () => {
    const client = createFakeClient();
    const pi = createFakePi();
    const ctx = fakeCtx();
    const { createHerdsmanPiExtension } = (await import(extensionModuleUrl)) as Module;
    const paneA = {
      agent: "claude",
      agentStatus: "idle",
      history: { lastAssistantMessage: { text: "keep-pane" } },
      paneId: "wB:p-agent",
      terminalId: "term_agent",
    };
    const paneB = {
      agent: "codex",
      agentStatus: "idle",
      history: { lastAssistantMessage: { text: "drop-pane" } },
      paneId: "wB:p-other",
      terminalId: "term_other",
    };
    const first = {
      agents: [paneA, paneB],
      herdrSessionName: "default",
      updatedAt: "2026-07-16T00:00:00.000Z",
      workspaceId: "wB",
    };
    const second = {
      agents: [paneA],
      herdrSessionName: "default",
      updatedAt: "2026-07-16T00:00:01.000Z",
      workspaceId: "wB",
    };
    client.response = (method) =>
      method === "agent.orchestrator.register" ? connectionResponse({ context: first }) : {};
    createHerdsmanPiExtension({ clientFactory: () => client })(pi);
    const previous = withHerdrEnv();
    try {
      await pi.emit("session_start", {}, ctx);
      await client.connect();
      await pi.emit("agent_start", {}, ctx);
      const before = await pi.emitContext([], ctx);
      expect(before).toEqual([
        expect.objectContaining({ content: expect.stringContaining("keep-pane") }),
      ]);
      expect((before[0] as { content: string }).content).toContain("drop-pane");
      client.emitStream({
        method: "agent.context.changed",
        params: { context: second, herdrSessionName: "default", workspaceId: "wB" },
      });
      const after = await pi.emitContext([], ctx);
      expect(after).toEqual([
        expect.objectContaining({ content: expect.stringContaining("keep-pane") }),
      ]);
      expect((after[0] as { content: string }).content).not.toContain("drop-pane");
    } finally {
      restoreEnv(previous);
    }
  });
});

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
});
