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
    from?: "working" | "unknown" | "blocked";
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
    payload: input.type === "agent.idle" ? { from: input.from ?? "working" } : {},
    terminalId: input.terminalId,
    type: input.type ?? "agent.done",
    workspaceId,
  });
}

describe("AgentOrchestratorService", () => {
  test("does not reclaim a stale delivery while its owner agent is working", () => {
    const { harness, service } = openService();
    harness.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [
        {
          agent: "codex",
          agent_status: "working",
          pane_id: "wB:p-owner",
          terminal_id: "term_owner",
          workspace_id: "wB",
        },
      ],
    });
    service.claim({ ...scope, paneId: "wB:p-owner", terminalId: "term_owner" });
    const event = appendEvent(harness, { terminalId: "term_agent" });
    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: event.id }),
    ]);
    harness.sqlite
      .prepare("update agent_events set last_attempt_at = ? where id = ?")
      .run(Date.now() - 120_000, event.id);

    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: event.id, deliveryAttempts: 1 }),
    ]);
    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: event.id, deliveryAttempts: 1 }),
    ]);
    expect(harness.agentEvents.get(event.id)).toMatchObject({
      status: "delivered",
      deliveryAttempts: 1,
      deliveredToTerminalId: "term_owner",
    });
  });

  test("keeps a stale delivery with its owner across a turn boundary while the scope is held", () => {
    const { harness, service } = openService();
    harness.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [
        {
          agent: "codex",
          agent_status: "working",
          pane_id: "wB:p-owner",
          terminal_id: "term_owner",
          workspace_id: "wB",
        },
      ],
    });
    service.claim({ ...scope, paneId: "wB:p-owner", terminalId: "term_owner" });
    const event = appendEvent(harness, { terminalId: "term_agent" });
    service.pending({ ...scope, terminalId: "term_owner" });
    harness.sqlite
      .prepare("update agent_events set last_attempt_at = ? where id = ?")
      .run(Date.now() - 120_000, event.id);
    harness.agents.updateStatus({
      agentStatus: "idle",
      herdrSessionName: "default",
      paneId: "wB:p-owner",
    });

    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: event.id, deliveryAttempts: 1 }),
    ]);
    expect(harness.agentEvents.get(event.id)).toMatchObject({
      status: "delivered",
      deliveryAttempts: 1,
      deliveredToTerminalId: "term_owner",
    });
  });

  test("reclaims a stale delivery after the owner disconnects", () => {
    const { harness, service } = openService();
    harness.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [
        {
          agent: "codex",
          agent_status: "working",
          pane_id: "wB:p-owner",
          terminal_id: "term_owner",
          workspace_id: "wB",
        },
      ],
    });
    service.claim({ ...scope, paneId: "wB:p-owner", terminalId: "term_owner" });
    const event = appendEvent(harness, { terminalId: "term_agent" });
    service.pending({ ...scope, terminalId: "term_owner" });
    harness.sqlite
      .prepare("update agent_events set last_attempt_at = ? where id = ?")
      .run(Date.now() - 120_000, event.id);
    harness.agents.retirePane({ herdrSessionName: "default", paneId: "wB:p-owner" });
    service.release({ ...scope, reason: "disconnected", terminalId: "term_owner" });
    service.claim({ ...scope, paneId: "wB:p-new-owner", terminalId: "term_new_owner" });
    harness.sqlite
      .prepare(
        "update agent_orchestrator_scopes set acked_event_id = 0 where herdr_session_name = ? and workspace_id = ?",
      )
      .run(scope.herdrSessionName, scope.workspaceId);

    expect(service.pending({ ...scope, terminalId: "term_new_owner" })).toEqual([
      expect.objectContaining({ id: event.id, deliveryAttempts: 2 }),
    ]);
  });

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
      from: "working",
      type: "agent.idle",
    });
    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: workerEvent.id }),
    ]);
    expect(
      service.ack({ ...scope, eventId: workerEvent.id, terminalId: "term_owner" }),
    ).toMatchObject({
      ackedEventId: workerEvent.id,
    });
  });

  test("filters idle events unless they transition from working", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:p-owner", terminalId: "term_owner" });
    const working = appendEvent(harness, {
      agent: "codex",
      terminalId: "term_other",
      from: "working",
      type: "agent.idle",
    });
    const unknown = appendEvent(harness, {
      agent: "codex",
      terminalId: "term_unknown",
      from: "unknown",
      type: "agent.idle",
    });
    const blocked = appendEvent(harness, {
      agent: "codex",
      terminalId: "term_blocked",
      from: "blocked",
      type: "agent.idle",
    });
    const pending = service.pending({ ...scope, terminalId: "term_owner" });
    expect(pending.map((event) => event.id)).toContain(working.id);
    expect(pending.map((event) => event.id)).not.toContain(unknown.id);
    expect(pending.map((event) => event.id)).not.toContain(blocked.id);
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

  test("preserves ownerless gap events across release and re-claim", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:p1", terminalId: "term_a" });
    service.release({ ...scope, reason: "released", terminalId: "term_a" });
    const firstGap = appendEvent(harness, { terminalId: "term_agent" });
    const ownerlessLater = appendEvent(harness, { terminalId: "term_agent_2" });
    const reclaimed = service.claim({ ...scope, paneId: "wB:p2", terminalId: "term_b" });

    // claimCursor keeps the scope's acked cursor while the row exists (owner null),
    // so events appended during the ownerless gap are picked up after the re-claim
    // instead of being skipped by a jump to latestEventId.
    expect(reclaimed.current.ackedEventId).toBe(0);
    expect(service.pending({ ...scope, terminalId: "term_b" })).toMatchObject([
      { id: firstGap.id },
      { id: ownerlessLater.id },
    ]);

    // The reclaimed owner acknowledges the preserved gap events in order.
    expect(service.ack({ ...scope, eventId: firstGap.id, terminalId: "term_b" }).ackedEventId).toBe(
      firstGap.id,
    );
    expect(
      service.ack({ ...scope, eventId: ownerlessLater.id, terminalId: "term_b" }).ackedEventId,
    ).toBe(ownerlessLater.id);

    // Subsequent replacement claims keep advancing from the acked cursor.
    const pending = appendEvent(harness, { terminalId: "term_agent" });
    const later = appendEvent(harness, { terminalId: "term_agent_2" });
    service.claim({ ...scope, paneId: "wB:p3", terminalId: "term_c" });
    // While both are still undelivered, an out-of-order ack is rejected.
    expect(() => service.ack({ ...scope, eventId: later.id, terminalId: "term_c" })).toThrow(
      "Only the next pending orchestrator event can be acknowledged",
    );
    expect(() =>
      service.ack({ ...scope, eventId: later.id + 10_000, terminalId: "term_c" }),
    ).toThrow("Only the next pending orchestrator event can be acknowledged");
    // Once delivered to the owner, the batch ack may skip the intermediate event.
    expect(service.pending({ ...scope, terminalId: "term_c" })).toMatchObject([
      { id: pending.id },
      { id: later.id },
    ]);
    expect(service.ack({ ...scope, eventId: later.id, terminalId: "term_c" }).ackedEventId).toBe(
      later.id,
    );
    expect(service.ack({ ...scope, eventId: later.id, terminalId: "term_c" }).ackedEventId).toBe(
      later.id,
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

  test("moves ownership with target gap-event preservation and active-owner preservation", () => {
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
          ackedEventId: 0,
          owner: { paneId: "wC:p2", terminalId: "term_a" },
        },
        previous: { owner: null },
        reason: "moved",
      },
    ]);
    // The target scope row exists (owner null), so claimCursor preserves its acked
    // cursor instead of jumping to latestEventId: the ownerless gap event survives.
    expect(service.pending({ ...target, terminalId: "term_a" })).toEqual([
      expect.objectContaining({ id: ownerlessTargetEvent.id }),
    ]);

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

describe("AgentOrchestratorService ack regression", () => {
  test("rejects non-owner acknowledgement with its structured contract", () => {
    const { service } = openService();

    expect(() => service.ack({ ...scope, eventId: 1, terminalId: "term_not_owner" })).toThrow(
      expect.objectContaining({
        code: "ORCHESTRATOR_NOT_OWNER",
        retryable: false,
        message: "Only the current orchestrator can acknowledge notifications",
      }),
    );
  });

  test("rejects an invalidated acknowledgement with its structured contract", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const event = appendEvent(harness, { terminalId: "term_worker" });
    harness.agents.replaceForSession({ herdrSessionName: "default", agents: [] });

    expect(() => service.ack({ ...scope, eventId: event.id, terminalId: "term_owner" })).toThrow(
      expect.objectContaining({
        code: "ORCHESTRATOR_EVENT_INVALIDATED",
        retryable: false,
        message: "orchestrator event is no longer pending (invalidated)",
      }),
    );
  });

  test("rejects an out-of-order acknowledgement with its structured contract", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    appendEvent(harness, { terminalId: "term_first" });
    const later = appendEvent(harness, { terminalId: "term_second" });

    expect(() => service.ack({ ...scope, eventId: later.id, terminalId: "term_owner" })).toThrow(
      expect.objectContaining({
        code: "ORCHESTRATOR_EVENT_OUT_OF_ORDER",
        retryable: false,
        message: "Only the next pending orchestrator event can be acknowledged",
      }),
    );
  });

  test("not-found and out-of-scope rejections reuse the legacy retry wording (known compatibility behavior)", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const otherScopeEvent = appendEvent(harness, { terminalId: "term_other", workspaceId: "wC" });

    // Legacy wording is reused from out-of-order; old clients treat these as retryable.
    // This known compatibility behavior is covered by the subsequent state-machine batch.
    expect(() => service.ack({ ...scope, eventId: 99_999, terminalId: "term_owner" })).toThrow(
      expect.objectContaining({
        code: "ORCHESTRATOR_EVENT_NOT_FOUND",
        message: "Only the next pending orchestrator event can be acknowledged",
      }),
    );
    expect(() =>
      service.ack({ ...scope, eventId: otherScopeEvent.id, terminalId: "term_owner" }),
    ).toThrow(
      expect.objectContaining({
        code: "ORCHESTRATOR_EVENT_NOT_IN_SCOPE",
        message: "Only the next pending orchestrator event can be acknowledged",
      }),
    );
  });

  test("already acknowledged events are idempotent without side effects", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const event = appendEvent(harness, { terminalId: "term_worker" });
    const first = service.ack({ ...scope, eventId: event.id, terminalId: "term_owner" });
    const eventCount = harness.sqlite.prepare("select count(*) as count from agent_events").get();

    expect(service.ack({ ...scope, eventId: event.id, terminalId: "term_owner" })).toEqual(first);
    expect(harness.sqlite.prepare("select count(*) as count from agent_events").get()).toEqual(
      eventCount,
    );
    expect(service.status(scope)).toEqual(first);
  });

  test("rejects acknowledging an event invalidated with its retired worker pane", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const event = appendEvent(harness, { terminalId: "term_worker" });
    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: event.id }),
    ]);
    harness.agents.replaceForSession({ herdrSessionName: "default", agents: [] });

    expect(() => service.ack({ ...scope, eventId: event.id, terminalId: "term_owner" })).toThrow(
      "orchestrator event is no longer pending (invalidated)",
    );
  });

  test("rejects an invalidated event without blocking a later pending event", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const event = appendEvent(harness, { terminalId: "term_worker" });
    harness.agents.replaceForSession({ herdrSessionName: "default", agents: [] });

    expect(() => service.ack({ ...scope, eventId: event.id, terminalId: "term_owner" })).toThrow(
      "orchestrator event is no longer pending (invalidated)",
    );
    const later = appendEvent(harness, { terminalId: "term_later" });
    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: later.id }),
    ]);
    expect(service.ack({ ...scope, eventId: later.id, terminalId: "term_owner" })).toMatchObject({
      ackedEventId: later.id,
    });
  });

  test("still rejects acknowledging past a later deliverable event", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const first = appendEvent(harness, { terminalId: "term_first" });
    const second = appendEvent(harness, { terminalId: "term_second" });

    expect(() => service.ack({ ...scope, eventId: second.id, terminalId: "term_owner" })).toThrow(
      "Only the next pending orchestrator event can be acknowledged",
    );
    expect(first.id).toBeLessThan(second.id);
  });

  test("rejects nonexistent and cross-scope event ids", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const otherScopeEvent = appendEvent(harness, { terminalId: "term_other", workspaceId: "wC" });

    expect(() =>
      service.ack({ ...scope, eventId: otherScopeEvent.id, terminalId: "term_owner" }),
    ).toThrow("Only the next pending orchestrator event can be acknowledged");
    expect(() =>
      service.ack({ ...scope, eventId: otherScopeEvent.id + 10_000, terminalId: "term_owner" }),
    ).toThrow("Only the next pending orchestrator event can be acknowledged");
  });

  test("repeated acknowledgement is idempotent and never moves the cursor backward", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const event = appendEvent(harness, { terminalId: "term_worker" });
    const first = service.ack({ ...scope, eventId: event.id, terminalId: "term_owner" });

    expect(service.ack({ ...scope, eventId: event.id, terminalId: "term_owner" })).toEqual(first);
  });

  test("allows a batch ack to skip a stuck delivered event (M1)", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const stuck = appendEvent(harness, { terminalId: "term_first" });
    const second = appendEvent(harness, { terminalId: "term_second" });
    const third = appendEvent(harness, { terminalId: "term_third" });
    // Deliver the first event to the owner but never ack it (stuck delivery).
    service.pending({ ...scope, terminalId: "term_owner" });
    expect(harness.agentEvents.get(stuck.id)).toMatchObject({ status: "delivered" });

    // Acking a later event is allowed now: the next candidate is already delivered
    // to this owner, and markAcked's id <= cursor semantics acknowledges the
    // intermediate delivered/pending events too.
    expect(service.ack({ ...scope, eventId: third.id, terminalId: "term_owner" })).toMatchObject({
      ackedEventId: third.id,
    });
    expect(harness.agentEvents.get(stuck.id)).toMatchObject({ status: "acked" });
    expect(harness.agentEvents.get(second.id)).toMatchObject({ status: "acked" });
    expect(harness.agentEvents.get(third.id)).toMatchObject({ status: "acked" });
  });

  test("still rejects an out-of-order ack that skips an undelivered pending event (M1)", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const first = appendEvent(harness, { terminalId: "term_first" });
    const second = appendEvent(harness, { terminalId: "term_second" });

    expect(() => service.ack({ ...scope, eventId: second.id, terminalId: "term_owner" })).toThrow(
      expect.objectContaining({
        code: "ORCHESTRATOR_EVENT_OUT_OF_ORDER",
        retryable: false,
        message: "Only the next pending orchestrator event can be acknowledged",
      }),
    );
    expect(first.id).toBeLessThan(second.id);
    expect(harness.agentEvents.get(first.id)).toMatchObject({ status: "pending" });
    expect(harness.agentEvents.get(second.id)).toMatchObject({ status: "pending" });
  });
});

describe("AgentOrchestratorService invalidated acknowledgement wording (independent coverage)", () => {
  test("uses invalidated wording for a known event that is no longer deliverable", () => {
    const { harness, service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });
    const known = appendEvent(harness, { terminalId: "term_worker" });
    expect(service.pending({ ...scope, terminalId: "term_owner" })).toEqual([
      expect.objectContaining({ id: known.id }),
    ]);
    harness.agents.replaceForSession({ herdrSessionName: "default", agents: [] });

    expect(() => service.ack({ ...scope, eventId: known.id, terminalId: "term_owner" })).toThrow(
      "orchestrator event is no longer pending (invalidated)",
    );
  });

  test("uses the original next-pending wording for an unknown event", () => {
    const { service } = openService();
    service.claim({ ...scope, paneId: "wB:owner", terminalId: "term_owner" });

    expect(() => service.ack({ ...scope, eventId: 99_999, terminalId: "term_owner" })).toThrow(
      "Only the next pending orchestrator event can be acknowledged",
    );
  });
});
