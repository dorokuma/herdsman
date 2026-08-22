import type { AgentEventStore } from "@/db/agent-events.js";
import type { AgentOrchestratorScopeStore } from "@/db/agent-orchestrator-scopes.js";
import type { AgentStore } from "@/db/agents.js";
import type {
  AgentEventRecord,
  AgentOrchestratorChangeReason,
  AgentOrchestratorState,
  AgentScope,
} from "@/observability/contracts.js";

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

  claim(input: AgentScope & { paneId: string; terminalId: string }): AgentOrchestratorChange {
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
        limit: Math.min(100, 1_000 - scanned),
      });
      if (batch.length === 0) break;
      scanned += batch.length;
      afterEventId = batch.at(-1)?.id ?? afterEventId;
      for (const event of batch) {
        // Strict identity prevents an old event from being delivered after a pane is rebound.
        const valid =
          event.agentId !== null &&
          !(
            validAgents.get(event.agentId)?.agent === "pi" &&
            (event.type === "agent.idle" || event.type === "agent.status.changed")
          ) &&
          event.deliverable === 1 &&
          validAgents.get(event.agentId)?.paneId === event.paneId &&
          (event.paneGeneration === null ||
            validAgents.get(event.agentId)?.paneGeneration === event.paneGeneration) &&
          validAgents.get(event.agentId)?.workspaceId === input.workspaceId &&
          event.workspaceId === input.workspaceId &&
          event.herdrSessionName === input.herdrSessionName;
        if (valid && event.terminalId !== null && event.terminalId !== input.terminalId) {
          pending.push(event);
        }
        if (pending.length === limit) break;
      }
    }
    return pending;
  }

  ack(input: AgentScope & { eventId: number; terminalId: string }): AgentOrchestratorState {
    const state = this.#scopes.get(input);
    if (!state?.owner || state.owner.terminalId !== input.terminalId) {
      throw new Error("Only the current orchestrator can acknowledge notifications");
    }
    if (input.eventId <= state.ackedEventId) return state;
    const next = this.#agentEvents.nextDeliverableAfter({
      ...input,
      afterEventId: state.ackedEventId,
      ownerTerminalId: input.terminalId,
    });
    if (next?.id === input.eventId) {
      const nextAgent = next.agentId ? this.#agents.get(next.agentId) : undefined;
      if (
        nextAgent?.agent !== "pi" ||
        (next?.type !== "agent.idle" && next?.type !== "agent.status.changed")
      ) {
        return this.#scopes.ack(input);
      }
    }
    if (!next) {
      const candidate = this.#agentEvents.get(input.eventId);
      const candidateAgent = candidate.agentId ? this.#agents.get(candidate.agentId) : undefined;
      if (
        candidate.herdrSessionName === input.herdrSessionName &&
        candidate.workspaceId === input.workspaceId &&
        candidate.agentId !== null &&
        !(
          candidateAgent?.agent === "pi" &&
          (candidate.type === "agent.idle" || candidate.type === "agent.status.changed")
        ) &&
        candidate.paneId !== null &&
        (candidate.paneGeneration === null ||
          candidate.paneGeneration === candidateAgent?.paneGeneration) &&
        candidate.id > state.ackedEventId
      ) {
        return this.#scopes.ack(input);
      }
    }
    throw new Error("Only the next pending orchestrator event can be acknowledged");
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
