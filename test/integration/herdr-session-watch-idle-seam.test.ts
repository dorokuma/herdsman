import { existsSync, mkdtempSync, rmSync, unlinkSync } from "node:fs";
import { createServer, type Server, type Socket } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import type { AgentHistoryService } from "@/agent-history/service.js";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { HerdrSessionWatchManager } from "@/daemon/herdr-session-watch-manager.js";
import { isDeliverableAgentEvent } from "@/db/agent-events.js";
import { HerdrSocketClient } from "@/herdr/socket-client.js";
import { AgentIndexService } from "@/observability/agent-index-service.js";
import type { AgentEventRecord } from "@/observability/contracts.js";
import { encodeJsonLine, JsonLineDecoder } from "@/shared/json-lines.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

const PANE_ID = "wB:p9";
const WORKSPACE_ID = "wB";
const TERMINAL_ID = "term_1";
const OWNER_TERMINAL_ID = "term_owner";
const SESSION_NAME = "default";

const tempDirs: string[] = [];
const servers: Server[] = [];
const managers: HerdrSessionWatchManager[] = [];

afterEach(async () => {
  await Promise.all(managers.splice(0).map((manager) => manager.stop()));
  await Promise.all(
    servers.splice(0).map(
      (server) =>
        new Promise<void>((resolve) => {
          server.close(() => resolve());
        }),
    ),
  );
  for (const dir of tempDirs.splice(0)) {
    rmSync(dir, { force: true, recursive: true });
  }
  cleanupTempDirs();
});

describe("HerdrSessionWatchManager idle seam", () => {
  test("refresh persists working prior then second-connection idle delivers agent.idle", async () => {
    const { harness, received, subscribed } = await startSeam({
      statusAfterCreate: "working",
      replayIdleOnSecondStream: true,
    });

    await waitFor(() => received.some((event) => event.type === "agent.idle"));
    await managers[0]?.stop();

    expect(subscribed[0]).toEqual([]);
    expect(subscribed[1]).toEqual([PANE_ID]);

    const idle = received.find((event) => event.type === "agent.idle");
    expect(idle).toMatchObject({
      paneId: PANE_ID,
      payload: expect.objectContaining({ from: "working", to: "idle" }),
      type: "agent.idle",
      workspaceId: WORKSPACE_ID,
    });

    const agent = harness.agents.findByPane({
      herdrSessionName: SESSION_NAME,
      paneId: PANE_ID,
    });
    expect(agent?.agentStatus).toBe("idle");
    expect(idle && agent && isDeliverableAgentEvent(idle, agent, scope(), OWNER_TERMINAL_ID)).toBe(
      true,
    );

    const plans = harness.statusEventPlans.listUnfinished();
    expect(plans).toEqual([]);
    harness.sqlite.close();
  });

  test("does not invent agent.idle when first refresh of a created pane is already idle", async () => {
    const { harness, received, subscribed } = await startSeam({
      statusAfterCreate: "idle",
      replayIdleOnSecondStream: false,
    });

    await waitFor(() => subscribed.length >= 2);
    await new Promise((resolve) => setTimeout(resolve, 80));
    await managers[0]?.stop();

    expect(subscribed[0]).toEqual([]);
    expect(subscribed[1]).toEqual([PANE_ID]);
    expect(received.filter((event) => event.type === "agent.idle")).toEqual([]);

    const agent = harness.agents.findByPane({
      herdrSessionName: SESSION_NAME,
      paneId: PANE_ID,
    });
    expect(agent?.agentStatus).toBe("idle");
    expect(harness.statusEventPlans.listUnfinished()).toEqual([]);
    expect(
      harness.agentEvents
        .listAfter({ herdrSessionName: SESSION_NAME, workspaceId: WORKSPACE_ID })
        .filter((event) => event.type === "agent.idle"),
    ).toEqual([]);
    harness.sqlite.close();
  });
});

async function startSeam(input: {
  replayIdleOnSecondStream: boolean;
  statusAfterCreate: "idle" | "working";
}) {
  const harness = openObservabilityDbHarness();
  const received: AgentEventRecord[] = [];
  const subscribed: string[][] = [];
  let paneVisible = false;

  const { socketPath } = await openFakeHerdrServer((socket, request) => {
    if (request.method === "session.snapshot") {
      socket.write(
        encodeJsonLine({
          id: request.id,
          result: paneVisible ? paneSnapshot(input.statusAfterCreate) : emptySnapshot(),
        }),
      );
      return;
    }
    if (request.method !== "events.subscribe") {
      socket.write(encodeJsonLine({ id: request.id, result: {} }));
      return;
    }

    const paneIds = subscribedPaneIds(request);
    subscribed.push(paneIds);
    socket.write(encodeJsonLine({ id: request.id, result: { subscribed: true } }));

    // Stream 1 is topology-only (paneIds=[]). Status is pane-specific: herdr
    // will not emit pane.agent_status_changed until that pane is subscribed.
    if (paneIds.length === 0) {
      paneVisible = true;
      socket.write(
        encodeJsonLine({
          data: { pane_id: PANE_ID, workspace_id: WORKSPACE_ID },
          event: "pane.created",
        }),
      );
      return;
    }

    if (input.replayIdleOnSecondStream && paneIds.includes(PANE_ID)) {
      socket.write(
        encodeJsonLine({
          data: { agent_status: "idle", pane_id: PANE_ID, workspace_id: WORKSPACE_ID },
          event: "pane.agent_status_changed",
        }),
      );
    }
  });

  const index = new AgentIndexService({
    history: {
      async resolveCompactHistory() {
        return {
          compactHistory: {
            ...emptyCompactHistory("claude-jsonl"),
            lastAssistantMessage: { ref: "history", text: "turn complete", timestamp: null },
          },
          historyRef: null,
          sourceFingerprint: null,
        };
      },
    } as unknown as AgentHistoryService,
    stores: harness,
  });

  const manager = new HerdrSessionWatchManager({
    activeRevisionPollMs: 60_000,
    agents: harness.agents,
    clientFactory: (clientInput) => new HerdrSocketClient(clientInput),
    fullRescanMs: 60_000,
    herdrSessions: harness.herdrSessions,
    index,
    onAgentEvent: (event) => received.push(event),
    reconnectDelayMs: 0,
    sessionList: async () => [
      {
        name: SESSION_NAME,
        running: true,
        sessionDir: "/tmp/herdr",
        socketPath,
      },
    ],
  });
  managers.push(manager);
  await manager.start();
  return { harness, received, subscribed };
}

function scope() {
  return { herdrSessionName: SESSION_NAME, workspaceId: WORKSPACE_ID };
}

function emptySnapshot() {
  return {
    snapshot: {
      agents: [],
      panes: [],
      tabs: [],
      workspaces: [{ focused: true, label: "Repo", workspace_id: WORKSPACE_ID }],
    },
  };
}

function paneSnapshot(status: "idle" | "working") {
  return {
    snapshot: {
      agents: [
        {
          agent: "claude",
          agent_status: status,
          cwd: "/repo",
          pane_id: PANE_ID,
          revision: 10,
          terminal_id: TERMINAL_ID,
          workspace_id: WORKSPACE_ID,
        },
      ],
      panes: [{ pane_id: PANE_ID, revision: 10, workspace_id: WORKSPACE_ID }],
      tabs: [],
      workspaces: [{ focused: true, label: "Repo", workspace_id: WORKSPACE_ID }],
    },
  };
}

function subscribedPaneIds(request: Record<string, unknown>): string[] {
  const params =
    typeof request.params === "object" && request.params !== null
      ? (request.params as { subscriptions?: unknown })
      : {};
  if (!Array.isArray(params.subscriptions)) return [];
  return params.subscriptions.flatMap((item) => {
    if (typeof item !== "object" || item === null) return [];
    const subscription = item as { pane_id?: unknown; type?: unknown };
    if (subscription.type !== "pane.agent_status_changed") return [];
    return typeof subscription.pane_id === "string" ? [subscription.pane_id] : [];
  });
}

async function openFakeHerdrServer(
  onRequest: (socket: Socket, request: Record<string, unknown>) => void,
): Promise<{ requests: Record<string, unknown>[]; socketPath: string }> {
  const dir = mkdtempSync(join(tmpdir(), "herdsman-herdr-"));
  tempDirs.push(dir);
  const socketPath = join(dir, "herdr.sock");
  if (existsSync(socketPath)) unlinkSync(socketPath);

  const requests: Record<string, unknown>[] = [];
  const server = createServer((socket) => {
    const decoder = new JsonLineDecoder();
    socket.on("data", (chunk) => {
      for (const message of decoder.push(chunk.toString("utf8"))) {
        const request = message as Record<string, unknown>;
        requests.push(request);
        onRequest(socket, request);
      }
    });
  });
  servers.push(server);

  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, () => {
      server.off("error", reject);
      resolve();
    });
  });

  return { requests, socketPath };
}

async function waitFor(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 3_000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  throw new Error("condition was not met");
}
