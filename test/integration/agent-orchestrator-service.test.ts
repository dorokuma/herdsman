import { afterEach, describe, expect, test } from "vitest";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

afterEach(cleanupTempDirs);

const scope = { herdrSessionName: "default", workspaceId: "wB" };

function openService() {
  const harness = openObservabilityDbHarness();
  harness.herdrSessions.upsertRunning({
    name: "default",
    sessionDir: "/tmp/herdr",
    socketPath: "/tmp/herdr.sock",
  });
  const service = new AgentOrchestratorService({
    agentEvents: harness.agentEvents,
    agents: harness.agents,
    scopes: harness.agentOrchestratorScopes,
  });
  return { harness, service };
}

function appendEvent(
  harness: ReturnType<typeof openObservabilityDbHarness>,
  input: {
    agent?: string;
    terminalId: string;
    type?: "agent.done" | "agent.idle" | "agent.status.changed";
    sessionPath?: string;
    workspaceId?: string;
  },
) {
  const workspaceId = input.workspaceId ?? "wB";
  const paneId = `${workspaceId}:${input.terminalId}`;
  const existing = harness.agents.list({ all: true, herdrSessionName: "default" }).map((agent) => ({
    agent: agent.agent,
    agent_status: agent.agentStatus,
    focused: agent.focused,
    name: agent.name,
    pane_id: agent.paneId,
    terminal_id: agent.terminalId,
    workspace_id: agent.workspaceId,
  }));
  const agent = harness.agents
    .replaceForSession({
      herdrSessionName: "default",
      agents: [
        ...existing,
        {
          agent: input.agent ?? "codex",
          agent_status: "working",
          focused: false,
          name: input.terminalId,
          pane_id: paneId,
          terminal_id: input.terminalId,
          workspace_id: workspaceId,
          ...(input.sessionPath
            ? {
                agent_session: {
                  agent: input.agent ?? "codex",
                  kind: "path",
                  source: "test",
                  value: input.sessionPath,
                },
              }
            : {}),
        },
      ],
    })
    .find((candidate) => candidate.terminalId === input.terminalId);
  if (!agent) throw new Error("Expected indexed agent");
  return harness.agentEvents.append({
    agentId: agent.id,
    herdrSessionName: "default",
    paneId,
    payload: {},
    terminalId: input.terminalId,
    type: input.type ?? "agent.done",
    workspaceId,
  });
}

describe("AgentOrchestratorService", () => {
  test("initializes once, replaces owners, and releases only the current owner", () => {
    const { harness, service } = openService();
    const baseline = appendEvent(harness, { terminalId: "term_agent" });

    expect(service.status(scope)).toBeUndefined();
    const first = service.claim({ ...scope, paneId: "wB:p1", terminalId: "term_a" });
    expect(first).toMatchObject({
      current: { ackedEventId: baseline.id, owner: { paneId: "wB:p1", terminalId: "term_a" } },
      previous: { ackedEventId: baseline.id, owner: null },
      reason: "claimed",
    });

    const same = service.claim({ ...scope, paneId: "wB:p1", terminalId: "term_a" });
    expect(same.current).toMatchObject({ ackedEventId: baseline.id, owner: same.previous.owner });
    const replacement = service.claim({ ...scope, paneId: "wB:p2", terminalId: "term_b" });
    expect(replacement).toMatchObject({
      current: { ackedEventId: baseline.id, owner: { terminalId: "term_b" } },
      previous: { owner: { terminalId: "term_a" } },
    });
    expect(service.release({ ...scope, reason: "released", terminalId: "term_a" })).toBeUndefined();
    expect(service.release({ ...scope, reason: "released", terminalId: "term_b" })).toMatchObject({
      current: { owner: null },
      reason: "released",
    });
  });

  test("does not deliver interactive Pi observer events", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:p-owner", terminalId: "term_owner" });
    const observerEvent = appendEvent(harness, {
      agent: "pi",
      sessionPath: "/root/.pi/agent/sessions/interactive.jsonl",
      terminalId: "term_other",
      type: "agent.idle",
    });

    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([]);
    expect(() =>
      service.ack({ ...scope, eventId: observerEvent.id, terminalId: "term_owner" }),
    ).toThrow("Only the next pending orchestrator event can be acknowledged");
  });

  test("still delivers and acknowledges dispatched Pi role events", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:p-owner", terminalId: "term_owner" });
    const workerEvent = appendEvent(harness, {
      agent: "pi",
      sessionPath: "/tmp/pi-role-sessions/role-worker-fd92d978/session.jsonl",
      terminalId: "term_worker",
      type: "agent.idle",
    });

    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: workerEvent.id }),
    ]);
    expect(service.ack({ ...scope, eventId: workerEvent.id, terminalId: "term_owner" })).toMatchObject({
      ackedEventId: workerEvent.id,
    });
  });

  test("does not filter non-Pi agent idle events", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:p-owner", terminalId: "term_owner" });
    const event = appendEvent(harness, { agent: "codex", terminalId: "term_other", type: "agent.idle" });
    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: event.id }),
    ]);
  });

  test("returns ordered non-self pending events across bounded scan pages", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:p1", terminalId: "term_owner" });
    for (let index = 0; index < 105; index += 1) {
      appendEvent(harness, { terminalId: "term_owner" });
    }
    harness.agentEvents.append({
      herdrSessionName: "default",
      payload: {},
      terminalId: null,
      type: "agent.done",
      workspaceId: "wB",
    });
    const agentEvent = appendEvent(harness, { terminalId: "term_agent" });
    appendEvent(harness, { terminalId: "term_other", workspaceId: "wC" });

    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: agentEvent.id }),
    ]);
    expect(service.pending({ ...scope, terminalId: "term_agent" })).toEqual([]);
  });

  test("drops ownerless events but preserves direct replacement events", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:p1", terminalId: "term_a" });
    service.release({ ...scope, reason: "released", terminalId: "term_a" });
    appendEvent(harness, { terminalId: "term_agent" });
    const ownerlessLater = appendEvent(harness, { terminalId: "term_agent_2" });
    const reclaimed = service.claim({ ...scope, paneId: "wB:p2", terminalId: "term_b" });

    expect(reclaimed.current.ackedEventId).toBe(ownerlessLater.id);
    expect(service.pending({ ...scope, terminalId: "term_b" })).toEqual([]);

    const pending = appendEvent(harness, { terminalId: "term_agent" });
    const later = appendEvent(harness, { terminalId: "term_agent_2" });
    service.claim({ ...scope, paneId: "wB:p3", terminalId: "term_c" });
    expect(service.pending({ ...scope, terminalId: "term_c" })).toMatchObject([
      { id: pending.id },
      { id: later.id },
    ]);
    expect(() => service.ack({ ...scope, eventId: later.id, terminalId: "term_c" })).toThrow(
      "Only the next pending orchestrator event can be acknowledged",
    );
    expect(() =>
      service.ack({ ...scope, eventId: later.id + 10_000, terminalId: "term_c" }),
    ).toThrow("Only the next pending orchestrator event can be acknowledged");
    expect(service.ack({ ...scope, eventId: pending.id, terminalId: "term_c" }).ackedEventId).toBe(
      pending.id,
    );
    expect(service.ack({ ...scope, eventId: pending.id, terminalId: "term_c" }).ackedEventId).toBe(
      pending.id,
    );
    expect(() => service.ack({ ...scope, eventId: later.id, terminalId: "term_b" })).toThrow(
      "Only the current orchestrator can acknowledge notifications",
    );
  });

  test("filters deleted and rebound agents, and ack advances past filtered events", () => {
    const { harness, service } = openService();
    const initial = harness.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [{ agent: "codex", pane_id: "wB:p1", terminal_id: "term_agent", workspace_id: "wB" }],
    })[0];
    if (!initial) throw new Error("Expected indexed agent");
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const deleted = harness.agentEvents.append({
      agentId: initial.id,
      herdrSessionName: "default",
      paneId: "wB:p1",
      payload: {},
      terminalId: "term_agent",
      type: "agent.done",
      workspaceId: "wB",
    });
    harness.agents.replaceForSession({ herdrSessionName: "default", agents: [] });
    const visible = harness.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [{ agent: "codex", pane_id: "wB:p2", terminal_id: "term_agent", workspace_id: "wB" }],
    })[0];
    if (!visible) throw new Error("Expected indexed agent");
    const visibleEvent = harness.agentEvents.append({
      agentId: visible.id,
      herdrSessionName: "default",
      paneId: "wB:p2",
      payload: {},
      terminalId: "term_agent",
      type: "agent.done",
      workspaceId: "wB",
    });
    expect(harness.sqlite.prepare("select agent_id, pane_id from agent_events").all()).toHaveLength(
      2,
    );
    expect(harness.agents.list({ herdrSessionName: "default", workspaceId: "wB" })).toHaveLength(1);
    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: visibleEvent.id }),
    ]);
    expect(
      service.ack({ ...scope, eventId: visibleEvent.id, terminalId: "term_owner" }).ackedEventId,
    ).toBe(visibleEvent.id);
    expect(deleted.id).toBeLessThan(visibleEvent.id);
  });

  test("does not deliver an event after its agent is rebound to another pane", () => {
    const { harness, service } = openService();
    const agent = harness.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [{ agent: "codex", pane_id: "wB:p1", terminal_id: "term_agent", workspace_id: "wB" }],
    })[0];
    if (!agent) throw new Error("Expected indexed agent");
    const event = harness.agentEvents.append({
      agentId: agent.id,
      herdrSessionName: "default",
      paneId: "wB:p1",
      payload: {},
      terminalId: "term_agent",
      type: "agent.done",
      workspaceId: "wB",
    });
    harness.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [{ agent: "codex", pane_id: "wB:p2", terminal_id: "term_agent", workspace_id: "wB" }],
    });
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([]);
    expect(event.id).toBeGreaterThan(0);
  });

  test("moves ownership with target ownerless-drop and active-owner preservation", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:p1", terminalId: "term_a" });
    const sourceEvent = appendEvent(harness, { terminalId: "term_agent" });
    service.ack({ ...scope, eventId: sourceEvent.id, terminalId: "term_a" });
    const target = { herdrSessionName: "default", workspaceId: "wC" };
    service.claim({ ...target, paneId: "wC:p1", terminalId: "term_c" });
    service.release({ ...target, reason: "released", terminalId: "term_c" });
    const ownerlessTargetEvent = appendEvent(harness, {
      terminalId: "term_agent",
      workspaceId: "wC",
    });

    expect(
      service.move({
        from: scope,
        paneId: "wC:p2",
        terminalId: "term_a",
        to: target,
      }),
    ).toMatchObject([
      { current: { ackedEventId: sourceEvent.id, owner: null }, reason: "moved" },
      {
        current: {
          ackedEventId: ownerlessTargetEvent.id,
          owner: { paneId: "wC:p2", terminalId: "term_a" },
        },
        previous: { owner: null },
        reason: "moved",
      },
    ]);
    expect(service.pending({ ...target, terminalId: "term_a" })).toEqual([]);

    const ownedTarget = { herdrSessionName: "default", workspaceId: "wD" };
    service.claim({ ...ownedTarget, paneId: "wD:p1", terminalId: "term_d" });
    const pendingTargetEvent = appendEvent(harness, {
      terminalId: "term_agent",
      workspaceId: "wD",
    });
    service.move({ from: target, paneId: "wD:p2", terminalId: "term_a", to: ownedTarget });
    expect(service.pending({ ...ownedTarget, terminalId: "term_a" })).toMatchObject([
      { id: pendingTargetEvent.id },
    ]);

    const unseen = { herdrSessionName: "default", workspaceId: "wE" };
    const unseenBaseline = appendEvent(harness, { terminalId: "term_agent", workspaceId: "wE" });
    service.move({ from: ownedTarget, paneId: "wE:p1", terminalId: "term_a", to: unseen });
    expect(service.status(unseen)).toMatchObject({ ackedEventId: unseenBaseline.id });
  });

  test("lists persisted owners across sessions", () => {
    const { harness, service } = openService();
    harness.herdrSessions.upsertRunning({
      name: "other",
      sessionDir: "/tmp/other",
      socketPath: "/tmp/other.sock",
    });
    service.claim({ ...scope, paneId: "wB:p1", terminalId: "term_a" });
    service.claim({
      herdrSessionName: "other",
      paneId: "wX:p1",
      terminalId: "term_x",
      workspaceId: "wX",
    });

    expect(service.persistedOwners()).toHaveLength(2);
  });
});
