import { type AgentEventStore, isDeliverableAgentEvent } from "@/db/agent-events.js";
import type { AgentOrchestratorScopeStore } from "@/db/agent-orchestrator-scopes.js";
import type { AgentStore } from "@/db/agents.js";
import type {
  AgentEventRecord,
  AgentIndexRecord,
  AgentOrchestratorChangeReason,
  AgentOrchestratorState,
  AgentScope,
} from "@/observability/contracts.js";
import { ORCHESTRATOR_ACK_MESSAGES, OrchestratorAckError } from "@/observability/errors.js";

export type AgentOrchestratorChange = {
  current: AgentOrchestratorState;
  previous: AgentOrchestratorState;
  reason: AgentOrchestratorChangeReason;
};

export class AgentOrchestratorService {
  readonly #agentEvents: AgentEventStore;
  readonly #agents: AgentStore;
  readonly #scopes: AgentOrchestratorScopeStore;

  constructor(options: {
    agentEvents: AgentEventStore;
    agents: AgentStore;
    scopes: AgentOrchestratorScopeStore;
  }) {
    this.#agentEvents = options.agentEvents;
    this.#agents = options.agents;
    this.#scopes = options.scopes;
  }

  status(scope: AgentScope): AgentOrchestratorState | undefined {
    return this.#scopes.get(scope);
  }

  claim(
    input: AgentScope & { paneId: string; terminalId: string; ownerConnected?: boolean },
  ): AgentOrchestratorChange {
    const change = this.#scopes.claim({ ...input, ackedEventId: this.#claimCursor(input) });
    return { ...change, reason: "claimed" };
  }

  release(
    input: AgentScope & {
      reason: "disconnected" | "released" | "startup_timeout";
      terminalId: string;
    },
  ): AgentOrchestratorChange | undefined {
    const change = this.#scopes.releaseIfOwner(input);
    if (!change.changed || !change.current || !change.previous) return undefined;
    return { current: change.current, previous: change.previous, reason: input.reason };
  }

  pending(
    input: AgentScope & {
      extraScopes?: AgentScope[];
      limit?: number;
      terminalId: string;
    },
  ): AgentEventRecord[] {
    this.#agentEvents.reclaimDelivered(60_000);
    const state = this.#scopes.get(input);
    if (!state?.owner || state.owner.terminalId !== input.terminalId) return [];

    const limit = input.limit ?? 100;
    const scopes = mergeScopes([input, ...(input.extraScopes ?? [])]);
    const validAgents = new Map(
      scopes.flatMap((scope) => this.#agents.list(scope)).map((agent) => [agent.id, agent]),
    );
    const pending: AgentEventRecord[] = [];
    let afterEventId = state.ackedEventId;
    let scanned = 0;
    while (pending.length < limit && scanned < 1_000) {
      const batch = this.#listAfterAcrossScopes(scopes, {
        afterEventId,
        limit: Math.min(100, 1_000 - scanned),
        ownerTerminalId: input.terminalId,
      });
      if (batch.length === 0) break;
      scanned += batch.length;
      afterEventId = batch.at(-1)?.id ?? afterEventId;
      for (const event of batch) {
        const valid = isDeliverableAgentEvent(
          event,
          event.agentId ? validAgents.get(event.agentId) : undefined,
          eventScope(event, input.workspaceId),
          input.terminalId,
        );
        if (valid && event.terminalId !== null && event.terminalId !== input.terminalId) {
          pending.push(event);
        }
        if (pending.length === limit) break;
      }
    }
    if (pending.length === 0) return [];
    return this.#agentEvents.reservePending(
      input.terminalId,
      limit,
      pending.map((event) => event.id),
    );
  }

  /**
   * Workspaces in the same Herdr session that have no orchestrator owner of
   * their own. Workers dispatched into them (script-dispatched `herdr tab
   * create` panes) stay invisible to the session orchestrator unless its owner
   * is allowed to receive and acknowledge their events.
   */
  ownerlessSiblingScopes(input: AgentScope): AgentScope[] {
    const ownedWorkspaces = new Set(
      this.#scopes.listOwnedForSession(input.herdrSessionName).map((scope) => scope.workspaceId),
    );
    const workspaces = new Set(
      this.#agents
        .list({ herdrSessionName: input.herdrSessionName })
        .map((agent) => agent.workspaceId),
    );
    return [...workspaces]
      .filter(
        (workspaceId) => workspaceId !== input.workspaceId && !ownedWorkspaces.has(workspaceId),
      )
      .sort()
      .map((workspaceId) => ({ herdrSessionName: input.herdrSessionName, workspaceId }));
  }

  #listAfterAcrossScopes(
    scopes: AgentScope[],
    input: { afterEventId: number; limit: number; ownerTerminalId: string },
  ): AgentEventRecord[] {
    if (scopes.length === 1) {
      return this.#agentEvents.listAfter({ ...scopes[0], ...input });
    }
    const byId = new Map<number, AgentEventRecord>();
    for (const scope of scopes) {
      for (const event of this.#agentEvents.listAfter({ ...scope, ...input })) {
        byId.set(event.id, event);
      }
    }
    return [...byId.values()].sort((left, right) => left.id - right.id).slice(0, input.limit);
  }

  /**
   * The lowest deliverable event id the owner can still receive, across its own
   * workspace and every ownerless sibling workspace it serves.
   *
   * Event ids are global, so ordering has to be judged on the merged view: a
   * lower id sitting in a sibling workspace blocks the acknowledgement of a
   * higher id in the owner's own workspace just as much as the other way round.
   * `afterEventId` is the owner's own cursor, matching the scan `pending()` uses
   * for both workspace kinds.
   */
  #nextDeliverableAcrossScopes(
    input: AgentScope & { terminalId: string },
    afterEventId: number,
  ): AgentEventRecord | undefined {
    let next: AgentEventRecord | undefined;
    for (const scope of mergeScopes([input, ...this.ownerlessSiblingScopes(input)])) {
      const candidate = this.#agentEvents.nextDeliverableAfter({
        ...scope,
        afterEventId,
        ownerTerminalId: input.terminalId,
        getAgent: (agentId) => this.#safeGetAgent(agentId),
      });
      if (candidate && (next === undefined || candidate.id < next.id)) next = candidate;
    }
    return next;
  }

  ack(input: AgentScope & { eventId: number; terminalId: string }): AgentOrchestratorState {
    try {
      return this.#ack(input);
    } catch (error) {
      if (error instanceof OrchestratorAckError) {
        const state = this.#scopes.get(input);
        let expectedNextEventId: number | null = null;
        if (state?.owner?.terminalId === input.terminalId) {
          expectedNextEventId =
            this.#nextDeliverableAcrossScopes(input, state.ackedEventId)?.id ?? null;
        }
        console.warn("Herdsman orchestrator ack rejected", {
          eventId: input.eventId,
          requestTerminal: input.terminalId,
          expectedNextEventId,
          code: error.code,
          herdrSessionName: input.herdrSessionName,
          workspaceId: input.workspaceId,
        });
      }
      throw error;
    }
  }

  #safeGetAgent(agentId: string): AgentIndexRecord | undefined {
    try {
      return this.#agents.get(agentId);
    } catch {
      console.warn("Herdsman orchestrator skipped missing agent", { agentId });
      return undefined;
    }
  }

  #ack(input: AgentScope & { eventId: number; terminalId: string }): AgentOrchestratorState {
    const state = this.#scopes.get(input);
    if (!state?.owner || state.owner.terminalId !== input.terminalId) {
      throw new OrchestratorAckError({
        code: "ORCHESTRATOR_NOT_OWNER",
        message: ORCHESTRATOR_ACK_MESSAGES.notOwner,
      });
    }
    if (input.eventId <= state.ackedEventId) return state;
    let event: AgentEventRecord;
    try {
      event = this.#agentEvents.get(input.eventId);
    } catch {
      throw new OrchestratorAckError({
        code: "ORCHESTRATOR_EVENT_NOT_FOUND",
        message: ORCHESTRATOR_ACK_MESSAGES.outOfOrder,
      });
    }
    if (
      event.herdrSessionName !== input.herdrSessionName ||
      !this.#isAckScopeAllowed(event, input)
    ) {
      throw new OrchestratorAckError({
        code: "ORCHESTRATOR_EVENT_NOT_IN_SCOPE",
        message: ORCHESTRATOR_ACK_MESSAGES.outOfOrder,
      });
    }
    if (event.status === "delivered" && event.deliveredToTerminalId !== input.terminalId) {
      throw new OrchestratorAckError({
        code: "ORCHESTRATOR_EVENT_OUT_OF_ORDER",
        message: ORCHESTRATOR_ACK_MESSAGES.outOfOrder,
      });
    }
    const isDeliveredToCurrentTerminal =
      event.status === "delivered" && event.deliveredToTerminalId === input.terminalId;
    const isInvalidatedDelivered =
      event.status === "invalidated" && event.deliveredToTerminalId === input.terminalId;

    if (event.status !== "pending" && event.status !== "delivered" && !isInvalidatedDelivered) {
      throw new OrchestratorAckError({
        code: "ORCHESTRATOR_EVENT_INVALIDATED",
        message: ORCHESTRATOR_ACK_MESSAGES.invalidated,
      });
    }
    if (!isInvalidatedDelivered) {
      const eventAgent = event.agentId ? this.#safeGetAgent(event.agentId) : undefined;
      if (
        eventAgent &&
        !isDeliverableAgentEvent(
          isDeliveredToCurrentTerminal ? { ...event, status: "pending" } : event,
          eventAgent,
          eventScope(event, input.workspaceId),
          input.terminalId,
        )
      ) {
        throw new OrchestratorAckError({
          code: "ORCHESTRATOR_EVENT_OUT_OF_ORDER",
          message: ORCHESTRATOR_ACK_MESSAGES.outOfOrder,
        });
      }
    }
    const next = this.#nextDeliverableAcrossScopes(input, state.ackedEventId);
    // Only an undelivered (pending) later event blocks an out-of-order ack. If the
    // next candidate is already delivered to this owner, or there is no candidate
    // (acked/invalidated/failed/null), let the batch ack through: markAcked uses an
    // id <= cursor so the intermediate delivered events get acknowledged too. This
    // keeps one stuck delivered event from blocking the whole trailing batch.
    const isUnconsumedRetained =
      next &&
      next.status === "invalidated" &&
      next.deliverable === 1 &&
      (!next.deliveredToTerminalId || next.deliveredToTerminalId !== input.terminalId);
    if (next && (next.status === "pending" || isUnconsumedRetained) && input.eventId > next.id) {
      throw new OrchestratorAckError({
        code: "ORCHESTRATOR_EVENT_OUT_OF_ORDER",
        message: ORCHESTRATOR_ACK_MESSAGES.outOfOrder,
      });
    }
    if (event.workspaceId !== input.workspaceId) {
      // Cross-workspace ack (ownerless sibling workspace): settle only this single
      // row and leave the owner's own cursor untouched. A scope-cursor ack would
      // rewrite every `id <= eventId` row of the owner's workspace to acked and
      // advance `acked_event_id` past them, silently swallowing its own events
      // that were never delivered. The settled row needs no redelivery: `listAfter`
      // only scans pending/delivered/retained-invalidated rows.
      this.#agentEvents.markAcked(input.eventId);
      return this.#scopes.get(input) ?? state;
    }
    this.#agentEvents.markAcked(input.eventId, {
      herdrSessionName: input.herdrSessionName,
      workspaceId: input.workspaceId,
    });
    return this.#scopes.ack(input);
  }

  /**
   * Ack scope guard: the owner's own workspace plus every sibling workspace in
   * the session that has no orchestrator owner of its own. Events of a workspace
   * owned by another terminal stay out of scope.
   */
  #isAckScopeAllowed(event: AgentEventRecord, input: AgentScope): boolean {
    if (event.workspaceId === input.workspaceId) return true;
    if (event.workspaceId === null) return false;
    return this.ownerlessSiblingScopes(input).some(
      (scope) => scope.workspaceId === event.workspaceId,
    );
  }

  move(input: {
    from: AgentScope;
    paneId: string;
    terminalId: string;
    to: AgentScope;
  }): AgentOrchestratorChange[] {
    return this.#scopes
      .moveOwner({ ...input, targetAckedEventId: this.#claimCursor(input.to) })
      .map((change) => ({ ...change, reason: "moved" }));
  }

  persistedOwners(): AgentOrchestratorState[] {
    return this.#scopes.listOwned();
  }

  #claimCursor(scope: AgentScope): number {
    const current = this.#scopes.get(scope);
    // Once a scope row exists, keep its acked cursor even when the owner has been
    // released (owner null): pending events appended during the ownerless gap are
    // then picked up by pending() after the next claim. Only a truly first claim
    // (no row at all) advances to latestEventId to avoid redelivering history.
    return current ? current.ackedEventId : this.#agentEvents.latestEventId(scope);
  }
}

function mergeScopes(scopes: AgentScope[]): AgentScope[] {
  const byKey = new Map<string, AgentScope>();
  for (const scope of scopes) {
    byKey.set(`${scope.herdrSessionName}\0${scope.workspaceId}`, {
      herdrSessionName: scope.herdrSessionName,
      workspaceId: scope.workspaceId,
    });
  }
  return [...byKey.values()];
}

/** The scope an event belongs to; a null workspace can never match a scope. */
function eventScope(
  event: AgentEventRecord,
  fallbackWorkspaceId: string,
): { herdrSessionName: string; workspaceId: string } {
  return {
    herdrSessionName: event.herdrSessionName,
    workspaceId: event.workspaceId ?? fallbackWorkspaceId,
  };
}
