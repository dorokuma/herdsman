import { type AgentEventStore, isDeliverableAgentEvent } from "@/db/agent-events.js";
import type { AgentOrchestratorScopeStore } from "@/db/agent-orchestrator-scopes.js";
import type { AgentStore } from "@/db/agents.js";
import type {
  AgentEventRecord,
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

  pending(input: AgentScope & { limit?: number; terminalId: string }): AgentEventRecord[] {
    this.#agentEvents.reclaimDelivered(60_000);
    const state = this.#scopes.get(input);
    if (!state?.owner || state.owner.terminalId !== input.terminalId) return [];

    const limit = input.limit ?? 100;
    const validAgents = new Map(this.#agents.list(input).map((agent) => [agent.id, agent]));
    const pending: AgentEventRecord[] = [];
    let afterEventId = state.ackedEventId;
    let scanned = 0;
    while (pending.length < limit && scanned < 1_000) {
      const batch = this.#agentEvents.listAfter({
        ...input,
        afterEventId,
        ownerTerminalId: input.terminalId,
        limit: Math.min(100, 1_000 - scanned),
      });
      if (batch.length === 0) break;
      scanned += batch.length;
      afterEventId = batch.at(-1)?.id ?? afterEventId;
      for (const event of batch) {
        const valid = isDeliverableAgentEvent(
          event,
          event.agentId ? validAgents.get(event.agentId) : undefined,
          input,
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

  ack(input: AgentScope & { eventId: number; terminalId: string }): AgentOrchestratorState {
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
      event.workspaceId !== input.workspaceId
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
    if (event.status !== "pending" && event.status !== "delivered") {
      throw new OrchestratorAckError({
        code: "ORCHESTRATOR_EVENT_INVALIDATED",
        message: ORCHESTRATOR_ACK_MESSAGES.invalidated,
      });
    }
    const eventAgent = event.agentId ? this.#agents.get(event.agentId) : undefined;
    if (
      eventAgent &&
      !isDeliverableAgentEvent(
        event.status === "delivered" ? { ...event, status: "pending" } : event,
        eventAgent,
        input,
        input.terminalId,
      )
    ) {
      throw new OrchestratorAckError({
        code: "ORCHESTRATOR_EVENT_OUT_OF_ORDER",
        message: ORCHESTRATOR_ACK_MESSAGES.outOfOrder,
      });
    }
    const next = this.#agentEvents.nextDeliverableAfter({
      ...input,
      afterEventId: state.ackedEventId,
      ownerTerminalId: input.terminalId,
      getAgent: (agentId) => this.#agents.get(agentId),
    });
    if (next && input.eventId > next.id) {
      throw new OrchestratorAckError({
        code: "ORCHESTRATOR_EVENT_OUT_OF_ORDER",
        message: ORCHESTRATOR_ACK_MESSAGES.outOfOrder,
      });
    }
    this.#agentEvents.markAcked(input.eventId);
    return this.#scopes.ack(input);
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
    return current?.owner ? current.ackedEventId : this.#agentEvents.latestEventId(scope);
  }
}
