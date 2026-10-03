import { existsSync, mkdirSync, mkdtempSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AgentHistoryService } from "@/agent-history/service.js";
import { createAgentHistoryService, emptyCompactHistory } from "@/agent-history/service.js";
import { ObservabilityRpcClient } from "@/daemon/client.js";
import { ObservabilityRpcServer } from "@/daemon/observability-server.js";
import { AgentContextService } from "@/observability/agent-context-service.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import { TurnCompletionRegistry } from "@/observability/turn-completion.js";
import {
  cleanupTempDirs,
  openObservabilityDbHarness,
  tempDirs,
} from "./observability-db-harness.js";
import { RpcTestClient } from "./rpc-test-client.js";

const servers: ObservabilityRpcServer[] = [];

afterEach(async () => {
  await Promise.all(servers.splice(0).map((server) => server.stop()));
  cleanupTempDirs();
});

describe("ObservabilityRpcServer", () => {
  test("serves agent methods over JSONL", async () => {
    const { client, dir, harness } = await openServer();
    seedAgent(harness, dir);

    await expect(client.request("agent.list", { workspaceId: "wB" })).resolves.toMatchObject({
      agents: [expect.objectContaining({ agent: "pi", paneId: "wB:p1" })],
    });
    await expect(
      client.request("agent.get", { target: "pi", workspaceId: "wB" }),
    ).resolves.toMatchObject({
      agent: expect.objectContaining({ agent: "pi", paneId: "wB:p1" }),
    });
    await expect(
      client.request("agent.read", { limit: 10, target: "pi", workspaceId: "wB" }),
    ).resolves.toMatchObject({ agent: expect.objectContaining({ messages: [] }) });

    const event = harness.agentEvents.append({
      herdrSessionName: "default",
      payload: { to: "idle" },
      type: "agent.idle",
      workspaceId: "wB",
    });
    await expect(client.request("agent.events", { workspaceId: "wB" })).resolves.toMatchObject({
      events: [expect.objectContaining({ id: event.id, type: "agent.idle" })],
    });

    await expect(
      client.request("agent.orchestrator.register", {
        herdrSocketPath: "/tmp/herdr/herdr.sock",
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
      }),
    ).resolves.toMatchObject({ events: [], presence: { terminalId: "term_1" }, state: null });
    await expect(
      client.request("agent.orchestrator.set", { enabled: true }),
    ).resolves.toMatchObject({
      changed: true,
      state: { owner: { terminalId: "term_1" } },
    });
    await expect(
      client.request("agent.notifications.ack", { eventId: event.id }),
    ).resolves.toMatchObject({
      acknowledged: true,
    });
    const removedMethod = ["agent", "telemetry"].join(".");
    await expect(
      client.request(removedMethod, {
        event: {},
        workspaceId: "wB",
      }),
    ).rejects.toThrow(`Unknown method: ${removedMethod}`);

    await expect(client.request("legacy.method", {})).rejects.toThrow("Unknown method");
    client.close();
    harness.sqlite.close();
  });

  test("answers agent.ping without side effects and rejects extra params", async () => {
    const { client, harness } = await openServer();
    await expect(client.request("agent.ping", {})).resolves.toEqual({ ok: true });
    await expect(client.request("agent.ping", { unexpected: true })).rejects.toThrow(
      "Invalid RPC params",
    );
    client.close();
    harness.sqlite.close();
  });

  test("records pi turn completion signals and validates their params", async () => {
    const registry = new TurnCompletionRegistry({ timeoutMs: 1_000 });
    const { client, dir, harness } = await openServer({ turnCompletions: registry });
    seedAgent(harness, dir);
    await client.request("agent.orchestrator.register", {
      herdrSocketPath: "/tmp/herdr/herdr.sock",
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
    });
    await expect(
      client.request("agent.turn.completed", {
        confirmed: true,
        herdrSessionName: "default",
        paneId: "wB:p1",
        terminalId: "term_1",
        workspaceId: "wB",
      }),
    ).resolves.toEqual({ accepted: true });
    const otherWaiter = registry.waitForSignal({
      herdrSessionName: "default",
      recordedAfterMs: 0,
      terminalId: "term_2",
    });
    await expect(
      client.request("agent.turn.completed", {
        confirmed: true,
        herdrSessionName: "default",
        paneId: "wB:p2",
        terminalId: "term_2",
        workspaceId: "wB",
      }),
    ).resolves.toEqual({ accepted: true });
    await expect(otherWaiter).resolves.toEqual({ confirmed: false, received: false });
    await expect(
      registry.waitForSignal({
        herdrSessionName: "default",
        recordedAfterMs: 0,
        terminalId: "term_1",
      }),
    ).resolves.toEqual({ confirmed: true, received: true });

    await expect(client.request("agent.turn.completed", { confirmed: true })).rejects.toThrow(
      "Invalid RPC params",
    );
    client.close();
    harness.sqlite.close();
  });

  test("accepts agent.turn.completed with expectedText and delivers it to waiters", async () => {
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    const { client, dir, harness } = await openServer({ turnCompletions: registry });
    seedAgent(harness, dir);
    await client.request("agent.orchestrator.register", {
      herdrSocketPath: "/tmp/herdr/herdr.sock",
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
    });

    await expect(
      client.request("agent.turn.completed", {
        confirmed: true,
        expectedText: "final answer",
        herdrSessionName: "default",
        paneId: "wB:p1",
        terminalId: "term_1",
        workspaceId: "wB",
      }),
    ).resolves.toEqual({ accepted: true });

    const waiter = registry.waitForSignal({
      herdrSessionName: "default",
      recordedAfterMs: 0,
      terminalId: "term_1",
    });
    await expect(waiter).resolves.toEqual({
      confirmed: true,
      expectedText: "final answer",
      received: true,
    });

    client.close();
    harness.sqlite.close();
  });

  test("serializes structured acknowledgement errors through the real RPC server and client", async () => {
    const { client, dir, harness } = await openServer();
    seedAgent(harness, dir);
    await client.request("agent.orchestrator.register", {
      herdrSocketPath: "/tmp/herdr/herdr.sock",
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
    });

    const error = await client
      .request("agent.notifications.ack", { eventId: 99_999 })
      .catch((value: unknown) => value);

    expect(error).toBeInstanceOf(Error);
    expect(error).toMatchObject({
      code: "ORCHESTRATOR_NOT_OWNER",
      retryable: false,
      message: "Only the current orchestrator can acknowledge notifications",
    });
    client.close();
    harness.sqlite.close();
  });

  test("resolves identifiers before live names and live names before kinds", async () => {
    const { client, harness } = await openServer();
    seedTargetAgents(harness);
    const listed = (await client.request("agent.list", { workspaceId: "wB" })) as {
      agents: Array<{ id: string; paneId: string }>;
    };
    const firstId = listed.agents.find((agent) => agent.paneId === "wB:p1")?.id;
    if (!firstId) throw new Error("Expected first agent id");
    seedTargetAgents(harness, firstId);

    await expect(
      client.request("agent.get", { target: "term_1", workspaceId: "wB" }),
    ).resolves.toMatchObject({ agent: { paneId: "wB:p1" } });
    await expect(
      client.request("agent.get", { target: firstId, workspaceId: "wB" }),
    ).resolves.toMatchObject({ agent: { paneId: "wB:p1" } });
    await expect(
      client.request("agent.get", { target: "reviewer", workspaceId: "wB" }),
    ).resolves.toMatchObject({ agent: { name: "reviewer", paneId: "wB:p1" } });
    await expect(
      client.request("agent.get", { target: "codex", workspaceId: "wB" }),
    ).resolves.toMatchObject({ agent: { name: "codex", paneId: "wB:p2" } });
    await expect(
      client.request("agent.get", { target: "claude", workspaceId: "wB" }),
    ).resolves.toMatchObject({ agent: { agent: "claude", paneId: "wB:p3" } });

    client.close();
    harness.sqlite.close();
  });

  test("reports ambiguity at the highest matching target priority", async () => {
    const { client, harness } = await openServer();
    seedAmbiguousTargetAgents(harness);

    const nameError = await client
      .request("agent.get", { target: "shared", workspaceId: "wB" })
      .catch((error: unknown) => error);
    expect(nameError).toBeInstanceOf(Error);
    expect((nameError as Error).message).toContain(
      "pane=wB:p1 terminal=term_1 name=shared agent=codex",
    );
    expect((nameError as Error).message).toContain(
      "pane=wB:p2 terminal=term_2 name=shared agent=reviewer",
    );
    expect((nameError as Error).message).not.toContain("pane=wB:p3");

    harness.agents.replaceForSession({
      agents: [
        {
          agent: "pi",
          agent_status: "idle",
          pane_id: "wB:p1",
          terminal_id: "term_1",
          workspace_id: "wB",
        },
        {
          agent: "pi",
          agent_status: "idle",
          pane_id: "wB:p2",
          terminal_id: "term_2",
          workspace_id: "wB",
        },
      ],
      herdrSessionName: "default",
    });
    await expect(client.request("agent.get", { target: "pi", workspaceId: "wB" })).rejects.toThrow(
      "pane=wB:p1 terminal=term_1 name=unnamed agent=pi; session=default workspace=wB pane=wB:p2 terminal=term_2 name=unnamed agent=pi",
    );

    client.close();
    harness.sqlite.close();
  });

  test("hides retained agents after their Herdr session stops", async () => {
    const { client, dir, harness } = await openServer();
    seedAgent(harness, dir);

    harness.herdrSessions.markStoppedMissingFrom([]);

    expect(
      harness.agents.findByPane({ herdrSessionName: "default", paneId: "wB:p1" }),
    ).toBeDefined();
    await expect(client.request("agent.list", { workspaceId: "wB" })).resolves.toEqual({
      agents: [],
    });
    await expect(client.request("agent.list", { all: true })).resolves.toEqual({ agents: [] });
    await expect(client.request("agent.get", { target: "pi", workspaceId: "wB" })).rejects.toThrow(
      "agent target not found: pi",
    );
    await expect(
      client.request("agent.read", { limit: 20, target: "pi", workspaceId: "wB" }),
    ).rejects.toThrow("agent target not found: pi");

    client.close();
    harness.sqlite.close();
  });

  test("reads supported histories through agent.read and refuses every other agent", async () => {
    const { client, dir, harness } = await openServer();
    // Disk bait for the agents herdsman deliberately does not support: their own
    // session stores are populated exactly as their real layouts look. None of
    // them may be read, and the files must not be walked to find a session.
    const codexSessionDir = join(dir, ".codex", "sessions", "2026", "07", "09");
    mkdirSync(codexSessionDir, { recursive: true });
    const codexSessionPath = join(
      codexSessionDir,
      "rollout-2026-07-09T12-00-00-eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee.jsonl",
    );
    writeFileSync(
      codexSessionPath,
      `${JSON.stringify({ type: "session_meta", payload: { cwd: "/repo-codex" } })}\n${JSON.stringify({ type: "event_msg", payload: { type: "user_message", message: "codex user" } })}\n`,
    );

    const geminiProjectDir = join(dir, ".gemini", "tmp", "repo-gemini");
    const geminiChatsDir = join(geminiProjectDir, "chats");
    mkdirSync(geminiChatsDir, { recursive: true });
    writeFileSync(join(geminiProjectDir, ".project_root"), "/repo-gemini\n");
    const geminiSessionPath = join(geminiChatsDir, "session-2026-07-09T12-00-00abcdef.json");
    writeFileSync(
      geminiSessionPath,
      JSON.stringify({ messages: [{ id: "g1", type: "user", content: "gemini user" }] }),
    );

    const claudeProjectDir = join(dir, ".claude", "projects", "-repo-claude");
    mkdirSync(claudeProjectDir, { recursive: true });
    const claudeId = "11111111-1111-4111-8111-111111111111";
    const claudeSessionPath = join(claudeProjectDir, `${claudeId}.jsonl`);
    writeFileSync(
      claudeSessionPath,
      `${JSON.stringify({ type: "user", uuid: "u1", message: { role: "user", content: "claude user" } })}\n`,
    );

    const openCodeDir = join(dir, ".local", "share", "opencode");
    mkdirSync(openCodeDir, { recursive: true });
    const openCodeDbPath = join(openCodeDir, "opencode.db");
    const sqlite = new DatabaseSync(openCodeDbPath);
    sqlite.exec(`
      create table session (id text primary key, directory text not null, time_updated integer not null);
      create table message (id text primary key, session_id text not null, time_created integer not null, time_updated integer not null, data text not null);
      create table part (id text primary key, message_id text not null, session_id text not null, time_created integer not null, time_updated integer not null, data text not null);
    `);
    sqlite
      .prepare("insert into session (id, directory, time_updated) values (?, ?, ?)")
      .run("oc_1", "/repo-opencode", 1);
    sqlite
      .prepare(
        "insert into message (id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?)",
      )
      .run("m1", "oc_1", 1, 1, JSON.stringify({ role: "user" }));
    sqlite
      .prepare(
        "insert into part (id, message_id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?, ?)",
      )
      .run("p1", "m1", "oc_1", 2, 2, JSON.stringify({ type: "text", text: "opencode user" }));
    sqlite.close();

    // The two supported agents: agy resolves its official conversation id to the
    // conversation database, pi hands over its session file path directly.
    const agyId = "22222222-2222-4222-8222-222222222222";
    const conversationsDir = join(dir, ".gemini", "antigravity-cli", "conversations");
    mkdirSync(conversationsDir, { recursive: true });
    const agyDbPath = join(conversationsDir, `${agyId}.db`);
    const agyDb = new DatabaseSync(agyDbPath);
    agyDb.exec("create table unrelated (id text primary key)");
    agyDb.close();
    const piSessionDir = join(dir, ".pi", "agent", "sessions");
    mkdirSync(piSessionDir, { recursive: true });
    const piSessionPath = join(piSessionDir, "ses-rpc.jsonl");
    writeFileSync(
      piSessionPath,
      `${JSON.stringify({ type: "message", id: "u1", message: { role: "user", content: "pi user" } })}\n`,
    );

    seedAdditionalRuntimeAgents(harness, {
      agyId,
      claudeId,
      codexSessionPath,
      geminiSessionPath,
      piSessionPath,
    });
    vi.spyOn(console, "warn").mockImplementation(() => {});

    await expect(
      client.request("agent.read", { limit: 10, target: "agy", workspaceId: "wB" }),
    ).resolves.toMatchObject({
      agent: {
        historyRef: { source: "antigravity-sqlite", value: agyId },
        messages: [],
      },
    });
    await expect(
      client.request("agent.read", { limit: 10, target: "pi", workspaceId: "wB" }),
    ).resolves.toMatchObject({
      agent: {
        historyRef: { source: "pi-jsonl", value: piSessionPath },
        messages: [expect.objectContaining({ role: "user", text: "pi user" })],
      },
    });
    for (const target of ["claude", "codex", "gemini", "opencode"]) {
      await expect(
        client.request("agent.read", { limit: 10, target, workspaceId: "wB" }),
      ).resolves.toMatchObject({
        agent: { historyRef: null, messages: [] },
      });
      await expect(
        client.request("agent.get", { target, workspaceId: "wB" }),
      ).resolves.toMatchObject({
        agent: { history: expect.objectContaining({ messageCount: 0 }) },
      });
    }

    client.close();
    harness.sqlite.close();
  });

  test("routes events to the newest owner socket and role changes to the exact scope", async () => {
    const { harness, server, socketPath } = await openServerWithoutClient();
    seedRoutingAgents(harness);
    const [piA, piB, piC, generic] = await Promise.all([
      RpcTestClient.connect(socketPath),
      RpcTestClient.connect(socketPath),
      RpcTestClient.connect(socketPath),
      RpcTestClient.connect(socketPath),
    ]);
    await Promise.all([
      register(piA, "wB:p-a", "pi-a", "wB"),
      register(piB, "wB:p-b", "pi-b", "wB"),
      register(piC, "wC:p-c", "pi-c", "wC"),
      generic.request("agent.events", { workspaceId: "wB" }),
    ]);

    const beforeOwner = appendRoutedEvent(harness, "term_b", "wB");
    server.publishAgentEvent(beforeOwner);
    await tick();
    expect(piA.notifications).toEqual([]);
    expect(piB.notifications).toEqual([]);

    await expect(piA.request("agent.orchestrator.set", { enabled: true })).resolves.toMatchObject({
      changed: true,
      state: { owner: { paneId: "wB:p-a", terminalId: "term_a" } },
    });
    await expect(piA.waitForNotification("agent.orchestrator.changed")).resolves.toMatchObject({
      params: { change: { reason: "claimed", current: { owner: { terminalId: "term_a" } } } },
    });
    await expect(piB.waitForNotification("agent.orchestrator.changed")).resolves.toBeDefined();
    expect(piC.notifications).toEqual([]);
    expect(generic.notifications).toEqual([]);

    const agentEvent = appendRoutedEvent(harness, "term_b", "wB");
    server.publishAgentEvent(agentEvent);
    await expect(piA.waitForNotification("agent.event")).resolves.toMatchObject({
      params: { event: { id: agentEvent.id } },
    });
    expect(piB.notifications).toEqual([]);
    await expect(
      piA.request("agent.notifications.ack", { eventId: agentEvent.id }),
    ).resolves.toMatchObject({
      acknowledged: true,
      state: { ackedEventId: agentEvent.id },
    });

    const self = appendRoutedEvent(harness, "term_a", "wB");
    const unknownTerminal = harness.agentEvents.append({
      herdrSessionName: "default",
      payload: {},
      terminalId: null,
      type: "agent.done",
      workspaceId: "wB",
    });
    server.publishAgentEvent(unknownTerminal);
    await tick();
    expect(piA.notifications).toEqual([]);

    // 孤儿终态事件（agents 行已物理删除，agent_id = null）仍然必须能推到 owner。
    const orphanFailure = harness.agentEvents.append({
      agentId: null,
      herdrSessionName: "default",
      paneId: "wB:p-b",
      payload: { agentId: "agent-gone", reason: "RETRY_EXHAUSTED" },
      terminalId: "term_b",
      type: "agent.failed",
      workspaceId: "wB",
    });
    server.publishAgentEvent(orphanFailure);
    await expect(piA.waitForNotification("agent.event")).resolves.toMatchObject({
      params: { event: { agentId: null, id: orphanFailure.id, type: "agent.failed" } },
    });

    const replacementError = await piB
      .request("agent.orchestrator.set", { enabled: true })
      .catch((error: unknown) => error);
    expect(replacementError).toBeInstanceOf(Error);
    expect((replacementError as Error).message).toContain("ORCHESTRATOR_SCOPE_ALREADY_CLAIMED");

    piA.close();
    // Wait for the server to observe piA's close (which drops the terminal
    // presence and lets term_b reclaim the scope) instead of sleeping a fixed
    // 75ms. A rejected `agent.orchestrator.set` is side-effect free: the scope
    // store throws ORCHESTRATOR_SCOPE_ALREADY_CLAIMED before it writes anything
    // (src/db/agent-orchestrator-scopes.ts:254-262), so polling it is safe.
    const replacement = await vi.waitFor(
      () => piB.request("agent.orchestrator.set", { enabled: true }),
      { interval: 10, timeout: 5_000 },
    );
    expect(replacement).toMatchObject({
      changed: true,
      events: [expect.objectContaining({ id: self.id })],
      state: { owner: { terminalId: "term_b" } },
    });
    await piB.waitForNotification("agent.orchestrator.changed");

    await expect(
      piB.request("agent.notifications.ack", { eventId: agentEvent.id }),
    ).resolves.toMatchObject({
      acknowledged: true,
      state: { ackedEventId: agentEvent.id },
    });
    await expect(piB.request("agent.orchestrator.set", { enabled: false })).resolves.toMatchObject({
      changed: true,
      state: { owner: null },
    });

    piB.close();
    piC.close();
    generic.close();
    harness.sqlite.close();
  });

  test("serves cached lists and re-resolves live detail reads", async () => {
    const calls: string[] = [];
    const liveHistory = {
      async read(_input: unknown, options: { forceDiscovery?: boolean }) {
        calls.push(`read:${options.forceDiscovery === true ? "fresh" : "reuse"}`);
        return { historyRef: null, messages: [] };
      },
      async resolveCompactHistory(_input: unknown, options: { forceDiscovery?: boolean } = {}) {
        calls.push(`get:${options.forceDiscovery === true ? "fresh" : "reuse"}`);
        return {
          compactHistory: emptyCompactHistory("pi-jsonl"),
          historyRef: null,
          sourceFingerprint: null,
        };
      },
    } as unknown as AgentHistoryService;
    const { client, dir, harness } = await openServer({ history: liveHistory });
    seedAgent(harness, dir);
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wB:p1" });
    if (!agent) throw new Error("missing seeded agent");
    harness.agentContextSnapshots.put({
      agentId: agent.id,
      compactHistory: {
        ...emptyCompactHistory("pi-jsonl"),
        lastUserMessage: { ref: "cached", text: "cached user", timestamp: null },
      },
      historyRef: {
        kind: "agent_session",
        path: "/tmp/history.jsonl",
        source: "pi-jsonl",
        value: "/tmp/history.jsonl",
      },
      paneRevision: null,
      sourceFingerprint: { mtimeMs: 1, path: "/tmp/history.jsonl", size: 1 },
    });
    await expect(client.request("agent.list", { workspaceId: "wB" })).resolves.toMatchObject({
      agents: [{ history: { lastUserMessage: { text: "cached user" } } }],
    });
    expect(calls).toEqual([]);
    await client.request("agent.get", { target: "pi", workspaceId: "wB" });
    await client.request("agent.read", { target: "pi", workspaceId: "wB" });
    // A list is served from the stored snapshots (no history read at all), while
    // an explicit detail read re-resolves from the official values instead of
    // reusing the persisted ref or a memoized failure.
    expect(calls).toEqual(["get:fresh", "read:fresh"]);
    client.close();
    harness.sqlite.close();
  });

  test("agent.read retries a lookup that failed moments ago instead of returning a memoized empty history", async () => {
    const { client, dir, harness } = await openServer();
    const agyId = "33333333-3333-4333-8333-333333333333";
    harness.herdrSessions.upsertRunning({
      name: "default",
      sessionDir: "/tmp/herdr",
      socketPath: "/tmp/herdr/herdr.sock",
    });
    harness.agents.replaceForSession({
      agents: [
        {
          agent: "agy",
          agent_session: {
            agent: "agy",
            kind: "id",
            source: "herdr:antigravity_cli",
            value: agyId,
          },
          agent_status: "idle",
          pane_id: "wB:p-agy",
          terminal_id: "term_agy",
          workspace_id: "wB",
        },
      ],
      herdrSessionName: "default",
    });
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const discoveryWarnings = () =>
      warn.mock.calls.filter(([message]) =>
        String(message).includes("no usable official Herdr agent session"),
      );

    // Nothing resolves yet, so the lookup fails and is remembered for the
    // daemon's background rounds.
    await expect(
      client.request("agent.read", { limit: 10, target: "agy", workspaceId: "wB" }),
    ).resolves.toMatchObject({
      agent: { historyRef: null, messages: [] },
    });
    expect(discoveryWarnings()).toHaveLength(1);

    // The operator fixes the reported problem (the conversation database appears)
    // inside the retry window: an explicit read resolves it now instead of
    // answering from the remembered failure.
    const conversations = join(dir, ".gemini", "antigravity-cli", "conversations");
    mkdirSync(conversations, { recursive: true });
    const dbPath = join(conversations, `${agyId}.db`);
    const db = new DatabaseSync(dbPath);
    db.exec("create table unrelated (id text primary key)");
    db.close();

    await expect(
      client.request("agent.read", { limit: 10, target: "agy", workspaceId: "wB" }),
    ).resolves.toMatchObject({
      agent: { historyRef: { path: dbPath, source: "antigravity-sqlite", value: agyId } },
    });
    await expect(
      client.request("agent.get", { target: "agy", workspaceId: "wB" }),
    ).resolves.toMatchObject({
      agent: { history: expect.objectContaining({ source: "antigravity-sqlite" }) },
    });
    // Forcing a retry never duplicates the per-key warning.
    expect(discoveryWarnings()).toHaveLength(1);

    client.close();
    harness.sqlite.close();
  });

  test("skips a persisted discovered_file ref on agent.get/read", async () => {
    const calls: Array<{
      method: string;
      preferred?: string | undefined;
    }> = [];
    const liveHistory = {
      async read(_input: unknown, options: { preferredRef?: { value: string } | null }) {
        calls.push({
          method: "read",
          preferred: options.preferredRef?.value,
        });
        return { historyRef: null, messages: [] };
      },
      async resolveCompactHistory(
        _input: unknown,
        options: { preferredRef?: { value: string } | null } = {},
      ) {
        calls.push({
          method: "get",
          preferred: options.preferredRef?.value,
        });
        return {
          compactHistory: emptyCompactHistory("pi-jsonl"),
          historyRef: null,
          sourceFingerprint: null,
        };
      },
    } as unknown as AgentHistoryService;
    const { client, dir, harness } = await openServer({ history: liveHistory });
    seedAgent(harness, dir);
    harness.agents.replaceForSession({
      agents: [
        {
          agent: "pi",
          agent_status: "idle",
          cwd: "/repo",
          pane_id: "wB:p1",
          terminal_id: "term_1",
          terminal_title: "π - role-worker-53c500b2 - root",
          workspace_id: "wB",
        },
      ],
      herdrSessionName: "default",
    });
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wB:p1" });
    if (!agent) throw new Error("missing seeded agent");
    harness.agentContextSnapshots.put({
      agentId: agent.id,
      compactHistory: emptyCompactHistory("pi-jsonl"),
      historyRef: {
        kind: "discovered_file",
        path: "/tmp/missing-discovered.jsonl",
        source: "pi-jsonl",
        value: "/tmp/missing-discovered.jsonl",
      },
      paneRevision: null,
      sourceFingerprint: { mtimeMs: 1, path: "/tmp/missing-discovered.jsonl", size: 1 },
    });
    await client.request("agent.get", { target: "pi", workspaceId: "wB" });
    await client.request("agent.read", { target: "pi", workspaceId: "wB" });
    expect(calls).toEqual([
      { method: "get", preferred: undefined },
      { method: "read", preferred: undefined },
    ]);
    client.close();
    harness.sqlite.close();
  });

  test("does not reuse a persisted discovered_file ref on agent.get/read", async () => {
    const calls: string[] = [];
    const liveHistory = {
      async read(_input: unknown, options: { preferredRef?: { value: string } | null }) {
        calls.push(options.preferredRef ? `read:${options.preferredRef.value}` : "read:discover");
        return { historyRef: null, messages: [] };
      },
      async resolveCompactHistory(
        _input: unknown,
        options: { preferredRef?: { value: string } | null } = {},
      ) {
        calls.push(options.preferredRef ? `get:${options.preferredRef.value}` : "get:discover");
        return {
          compactHistory: emptyCompactHistory("pi-jsonl"),
          historyRef: null,
          sourceFingerprint: null,
        };
      },
    } as unknown as AgentHistoryService;
    const { client, dir, harness } = await openServer({ history: liveHistory });
    seedAgent(harness, dir);
    const agent = harness.agents.findByPane({ herdrSessionName: "default", paneId: "wB:p1" });
    if (!agent) throw new Error("missing seeded agent");
    const path = join(dir, "recent-discovered.jsonl");
    writeFileSync(path, "history\n");
    harness.agentContextSnapshots.put({
      agentId: agent.id,
      compactHistory: emptyCompactHistory("pi-jsonl"),
      historyRef: {
        kind: "discovered_file",
        path,
        source: "pi-jsonl",
        value: path,
      },
      paneRevision: null,
      sourceFingerprint: { mtimeMs: Date.now(), path, size: 1 },
    });
    await client.request("agent.get", { target: "pi", workspaceId: "wB" });
    await client.request("agent.read", { target: "pi", workspaceId: "wB" });
    // Herdsman no longer trusts a ref the deleted cwd/mtime scanner produced, even
    // while the file is still recent: the official agent_session value decides.
    expect(calls).toEqual(["get:discover", "read:discover"]);
    client.close();
    harness.sqlite.close();
  });

  test("returns and pushes context only to the current owner, including null clearing snapshots", async () => {
    const { harness, server, socketPath } = await openServerWithoutClient();
    seedRoutingAgents(harness);
    const agents = harness.agents.list({ workspaceId: "wB" });
    for (const agent of agents) {
      harness.agentContextSnapshots.put({
        agentId: agent.id,
        compactHistory: {
          ...emptyCompactHistory("pi-jsonl"),
          lastAssistantMessage: {
            ref: agent.terminalId ?? "unknown",
            text: agent.paneId,
            timestamp: null,
          },
        },
        historyRef: null,
        paneRevision: null,
        sourceFingerprint: null,
      });
    }
    const [owner, off, other] = await Promise.all([
      RpcTestClient.connect(socketPath),
      RpcTestClient.connect(socketPath),
      RpcTestClient.connect(socketPath),
    ]);
    await expect(register(owner, "wB:p-a", "owner", "wB")).resolves.toMatchObject({
      context: null,
    });
    await register(off, "wB:p-b", "off", "wB");
    await register(other, "wC:p-c", "other", "wC");
    await expect(owner.request("agent.orchestrator.set", { enabled: true })).resolves.toMatchObject(
      {
        context: { agents: [{ terminalId: "term_b" }] },
      },
    );
    await tick();
    owner.clearNotifications();
    off.clearNotifications();
    other.clearNotifications();
    server.publishAgentContext({ herdrSessionName: "default", workspaceId: "wB" });
    await expect(owner.waitForNotification("agent.context.changed")).resolves.toMatchObject({
      params: { context: { agents: [{ terminalId: "term_b" }] }, workspaceId: "wB" },
    });
    await tick();
    server.publishAgentContext({ herdrSessionName: "default", workspaceId: "wB" });
    await tick();
    expect(owner.notifications).toEqual([]);
    expect(off.notifications).toEqual([]);
    expect(other.notifications).toEqual([]);

    const offAgent = agents.find((agent) => agent.terminalId === "term_b");
    if (!offAgent) throw new Error("missing off agent");
    harness.agentContextSnapshots.delete(offAgent.id);
    server.publishAgentContext({ herdrSessionName: "default", workspaceId: "wB" });
    await expect(owner.waitForNotification("agent.context.changed")).resolves.toMatchObject({
      params: { context: null, workspaceId: "wB" },
    });
    owner.close();
    off.close();
    other.close();
    harness.sqlite.close();
  });

  test("validates indexed presence and resolves a stale pane alias from live Herdr", async () => {
    const { harness, socketPath } = await openServerWithoutClient({
      resolvePaneIdentity: async () => ({
        paneId: "wC:p2",
        terminalId: "term_moved",
        workspaceId: "wC",
      }),
    });
    harness.herdrSessions.upsertRunning({
      name: "default",
      sessionDir: "/tmp/herdr",
      socketPath: "/tmp/herdr/herdr.sock",
    });
    const client = await RpcTestClient.connect(socketPath);

    await expect(register(client, "wB:p-old", "pi-moved", "wB")).resolves.toMatchObject({
      presence: {
        herdrSessionName: "default",
        paneId: "wC:p2",
        terminalId: "term_moved",
        workspaceId: "wC",
      },
    });
    await expect(
      client.request("agent.orchestrator.register", {
        herdrSocketPath: "/tmp/unknown.sock",
        paneId: "wB:p1",
        sessionRef: {
          agent: "pi",
          kind: "path",
          source: "herdr:pi",
          value: "/tmp/pi-session.jsonl",
        },
        subscriberId: "pi-unknown",
        subscriberKind: "pi",
        workspaceId: "wB",
      }),
    ).rejects.toThrow("running session");
    await expect(
      client.request("agent.orchestrator.register", {
        herdrSocketPath: "/tmp/herdr/herdr.sock",
        paneId: "wB:p1",
        sessionRef: {
          agent: "pi",
          kind: "path",
          source: "herdr:pi",
          value: "/tmp/pi-session.jsonl",
        },
        subscriberId: "bad-kind",
        subscriberKind: "claude",
        workspaceId: "wB",
      }),
    ).rejects.toThrow("Invalid RPC params");

    client.close();
    harness.sqlite.close();
  });

  test("rejects register when the connector cwd does not match the Herdr pane cwd", async () => {
    const { client, dir, harness } = await openServer({
      peerPidOf: () => process.pid,
      resolvePaneIdentity: async () => ({
        cwd: "/tmp/herdsman-not-this-pane",
        paneId: "wB:p1",
        terminalId: "term_1",
        workspaceId: "wB",
      }),
    });
    seedAgent(harness, dir);
    await expect(
      client.request("agent.orchestrator.register", {
        herdrSocketPath: "/tmp/herdr/herdr.sock",
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
      }),
    ).rejects.toThrow("does not match Herdr pane");
    client.close();
    harness.sqlite.close();
  });

  test("accepts register when the connector cwd matches the Herdr pane cwd", async () => {
    const { client, dir, harness } = await openServer({
      peerPidOf: () => process.pid,
      resolvePaneIdentity: async () => ({
        cwd: process.cwd(),
        paneId: "wB:p1",
        terminalId: "term_1",
        workspaceId: "wB",
      }),
    });
    seedAgent(harness, dir);
    await expect(
      client.request("agent.orchestrator.register", {
        herdrSocketPath: "/tmp/herdr/herdr.sock",
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
      }),
    ).resolves.toMatchObject({ presence: { terminalId: "term_1" } });
    client.close();
    harness.sqlite.close();
  });

  test("warns instead of silently skipping when pane cwd is missing", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const { client, dir, harness } = await openServer({
      peerPidOf: () => process.pid,
      resolvePaneIdentity: async () => ({
        paneId: "wB:p1",
        terminalId: "term_1",
        workspaceId: "wB",
      }),
    });
    seedAgent(harness, dir);
    await expect(
      client.request("agent.orchestrator.register", {
        herdrSocketPath: "/tmp/herdr/herdr.sock",
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
      }),
    ).resolves.toMatchObject({ presence: { terminalId: "term_1" } });
    expect(warn.mock.calls.some((args) => String(args[0]).includes("pane cwd"))).toBe(true);
    warn.mockRestore();
    client.close();
    harness.sqlite.close();
  });
});

async function openServer(options: Parameters<typeof openServerWithoutClient>[0] = {}) {
  const { dir, harness, server, socketPath } = await openServerWithoutClient(options);
  const client = new ObservabilityRpcClient({ socketPath });
  return { client, dir, harness, server };
}

async function openServerWithoutClient(
  options: {
    history?: AgentHistoryService;
    peerPidOf?: () => number | undefined;
    resolvePaneIdentity?: () => Promise<{
      paneId: string;
      cwd?: string;
      terminalId: string;
      workspaceId: string;
    }>;
    turnCompletions?: TurnCompletionRegistry;
  } = {},
) {
  const dir = mkdtempSync(join(tmpdir(), "herdsman-agent-rpc-"));
  tempDirs.push(dir);
  const socketPath = join(dir, "rpc.sock");
  if (existsSync(socketPath)) unlinkSync(socketPath);
  const harness = openObservabilityDbHarness();
  const history =
    options.history ??
    createAgentHistoryService({ cache: harness.agentHistoryCache, homeDir: dir });
  const context = new AgentContextService({
    history,
    stores: { agentContextSnapshots: harness.agentContextSnapshots, agents: harness.agents },
  });
  const server = new ObservabilityRpcServer({
    context,
    history,
    orchestrator: new AgentOrchestratorService({
      agentEvents: harness.agentEvents,
      agents: harness.agents,
      scopes: harness.agentOrchestratorScopes,
    }),
    ...(options.peerPidOf ? { peerPidOf: options.peerPidOf } : {}),
    ...(options.resolvePaneIdentity ? { resolvePaneIdentity: options.resolvePaneIdentity } : {}),
    ...(options.turnCompletions ? { turnCompletions: options.turnCompletions } : {}),
    socketPath,
    stores: {
      agentEvents: harness.agentEvents,
      agents: harness.agents,
      herdrSessions: harness.herdrSessions,
      herdrWorkspaces: harness.herdrWorkspaces,
    },
  });
  servers.push(server);
  await server.start();
  return { dir, harness, server, socketPath };
}

function register(
  client: RpcTestClient,
  paneId: string,
  subscriberId: string,
  workspaceId: string,
): Promise<unknown> {
  return client.request("agent.orchestrator.register", {
    herdrSocketPath: "/tmp/herdr/herdr.sock",
    paneId,
    sessionRef: {
      agent: "pi",
      kind: "path",
      source: "herdr:pi",
      value: "/tmp/pi-session.jsonl",
    },
    subscriberId,
    subscriberKind: "pi",
    workspaceId,
  });
}

function appendRoutedEvent(
  harness: ReturnType<typeof openObservabilityDbHarness>,
  terminalId: string,
  workspaceId: string,
) {
  const agent = harness.agents.findByTerminal({ herdrSessionName: "default", terminalId });
  return harness.agentEvents.append({
    agentId: agent?.id ?? null,
    paneId: agent?.paneId ?? null,
    herdrSessionName: "default",
    payload: {},
    terminalId,
    type: "agent.done",
    workspaceId,
  });
}

function seedRoutingAgents(harness: ReturnType<typeof openObservabilityDbHarness>) {
  harness.herdrSessions.upsertRunning({
    name: "default",
    sessionDir: "/tmp/herdr",
    socketPath: "/tmp/herdr/herdr.sock",
  });
  harness.agents.replaceForSession({
    agents: [
      { agent: "pi", pane_id: "wB:p-a", terminal_id: "term_a", workspace_id: "wB" },
      { agent: "pi", pane_id: "wB:p-b", terminal_id: "term_b", workspace_id: "wB" },
      { agent: "pi", pane_id: "wC:p-c", terminal_id: "term_c", workspace_id: "wC" },
    ],
    herdrSessionName: "default",
  });
}

async function tick(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 20));
}

function seedAdditionalRuntimeAgents(
  harness: ReturnType<typeof openObservabilityDbHarness>,
  sessions: {
    agyId: string;
    claudeId: string;
    codexSessionPath: string;
    geminiSessionPath: string;
    piSessionPath: string;
  },
) {
  harness.herdrSessions.upsertRunning({
    name: "default",
    sessionDir: "/tmp/herdr",
    socketPath: "/tmp/herdr/herdr.sock",
  });
  harness.agents.replaceForSession({
    agents: [
      {
        agent: "agy",
        agent_session: {
          agent: "agy",
          kind: "id",
          source: "herdr:antigravity_cli",
          value: sessions.agyId,
        },
        agent_status: "idle",
        cwd: "/repo-agy",
        pane_id: "wB:p-agy",
        terminal_id: "term_agy",
        workspace_id: "wB",
      },
      {
        agent: "pi",
        agent_session: {
          agent: "pi",
          kind: "path",
          source: "herdr:pi",
          value: sessions.piSessionPath,
        },
        agent_status: "idle",
        cwd: "/repo-pi",
        pane_id: "wB:p-pi",
        terminal_id: "term_pi",
        workspace_id: "wB",
      },
      {
        agent: "claude",
        agent_session: {
          agent: "claude",
          kind: "id",
          source: "herdr:claude",
          value: sessions.claudeId,
        },
        agent_status: "idle",
        cwd: "/repo-claude",
        pane_id: "wB:p-claude",
        terminal_id: "term_claude",
        workspace_id: "wB",
      },
      {
        agent: "codex",
        agent_session: {
          agent: "codex",
          kind: "path",
          source: "herdr:codex",
          value: sessions.codexSessionPath,
        },
        agent_status: "idle",
        cwd: "/repo-codex",
        pane_id: "wB:p-codex",
        terminal_id: "term_codex",
        workspace_id: "wB",
      },
      {
        agent: "opencode",
        agent_session: {
          agent: "opencode",
          kind: "id",
          source: "herdr:opencode",
          value: "oc_1",
        },
        agent_status: "idle",
        cwd: "/repo-opencode",
        pane_id: "wB:p-opencode",
        terminal_id: "term_opencode",
        workspace_id: "wB",
      },
      {
        agent: "gemini",
        agent_session: {
          agent: "gemini",
          kind: "path",
          source: "herdr:gemini",
          value: sessions.geminiSessionPath,
        },
        agent_status: "idle",
        cwd: "/repo-gemini",
        pane_id: "wB:p-gemini",
        terminal_id: "term_gemini",
        workspace_id: "wB",
      },
    ],
    herdrSessionName: "default",
  });
}

function seedAmbiguousTargetAgents(harness: ReturnType<typeof openObservabilityDbHarness>) {
  harness.herdrSessions.upsertRunning({
    name: "default",
    sessionDir: "/tmp/herdr",
    socketPath: "/tmp/herdr/herdr.sock",
  });
  harness.agents.replaceForSession({
    agents: [
      {
        agent: "codex",
        agent_status: "idle",
        name: "shared",
        pane_id: "wB:p1",
        terminal_id: "term_1",
        workspace_id: "wB",
      },
      {
        agent: "reviewer",
        agent_status: "idle",
        name: "shared",
        pane_id: "wB:p2",
        terminal_id: "term_2",
        workspace_id: "wB",
      },
      {
        agent: "shared",
        agent_status: "idle",
        name: "other",
        pane_id: "wB:p3",
        terminal_id: "term_3",
        workspace_id: "wB",
      },
    ],
    herdrSessionName: "default",
  });
}

function seedTargetAgents(
  harness: ReturnType<typeof openObservabilityDbHarness>,
  firstAgentId?: string,
) {
  harness.herdrSessions.upsertRunning({
    name: "default",
    sessionDir: "/tmp/herdr",
    socketPath: "/tmp/herdr/herdr.sock",
  });
  harness.agents.replaceForSession({
    agents: [
      {
        agent: "codex",
        agent_status: "idle",
        name: "reviewer",
        pane_id: "wB:p1",
        terminal_id: "term_1",
        workspace_id: "wB",
      },
      {
        agent: "reviewer",
        agent_status: "idle",
        name: "codex",
        pane_id: "wB:p2",
        terminal_id: "term_2",
        workspace_id: "wB",
      },
      {
        agent: "claude",
        agent_status: "idle",
        name: firstAgentId ?? "term_1",
        pane_id: "wB:p3",
        terminal_id: "term_3",
        workspace_id: "wB",
      },
    ],
    herdrSessionName: "default",
  });
}

function seedAgent(harness: ReturnType<typeof openObservabilityDbHarness>, dir?: string) {
  const baseDir = dir ?? mkdtempSync(join(tmpdir(), "herdsman-seed-agent-"));
  if (!dir) tempDirs.push(baseDir);
  const sessionDir = join(baseDir, ".pi/agent/sessions");
  mkdirSync(sessionDir, { mode: 0o700, recursive: true });
  const sessionPath = join(sessionDir, "agent-session.jsonl");
  writeFileSync(sessionPath, "", { mode: 0o600 });
  harness.herdrSessions.upsertRunning({
    name: "default",
    sessionDir: "/tmp/herdr",
    socketPath: "/tmp/herdr/herdr.sock",
  });
  harness.agents.replaceForSession({
    agents: [
      {
        agent: "pi",
        agent_session: { kind: "path", source: "pi-jsonl", value: sessionPath },
        agent_status: "idle",
        cwd: "/repo",
        pane_id: "wB:p1",
        terminal_id: "term_1",
        workspace_id: "wB",
      },
    ],
    herdrSessionName: "default",
  });
}
