import { afterEach, describe, expect, test } from "vitest";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { AgentIndexService } from "@/observability/agent-index-service.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

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
      "Only the next pending orchestrator event can be acknowledged",
    );
  });
});
