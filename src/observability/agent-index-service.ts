import { safeAllowedSessionPath } from "@/agent-history/discovery.js";
import { type AgentHistoryService, createAgentHistoryService } from "@/agent-history/service.js";
import { type AgentEventStore, hasNonEmptyAssistantMessage } from "@/db/agent-events.js";
import type { AgentHistoryCacheStore } from "@/db/agent-history-cache.js";
import type { AgentOrchestratorScopeStore } from "@/db/agent-orchestrator-scopes.js";
import type { AgentStore, HerdrAgentLike } from "@/db/agents.js";
import type { HerdrSessionStore } from "@/db/herdr-sessions.js";
import type { HerdrWorkspaceStore } from "@/db/herdr-workspaces.js";
import { normalizeHerdrSessionSnapshot } from "@/herdr/session-snapshot.js";
import { HerdrSocketClient } from "@/herdr/socket-client.js";
import { AgentContextService } from "@/observability/agent-context-service.js";
import {
  type AgentEventRecord,
  type AgentIndexRecord,
  type AgentScope,
  type AgentSessionRef,
  type AgentStatus,
  parseAgentStatus,
} from "@/observability/contracts.js";
import type { TurnCompletionRegistry } from "@/observability/turn-completion.js";
import { TURN_SIGNAL_WAIT_MS } from "@/observability/turn-completion.js";

export type AgentIndexRefreshResult = {
  agents: AgentIndexRecord[];
  contextChangedScopes: AgentScope[];
  events: AgentEventRecord[];
};

export type AgentEventHandlingResult = {
  contextChangedScopes: AgentScope[];
  events: AgentEventRecord[];
};

export type PiSessionRefRegistrationResult = {
  agent: AgentIndexRecord | undefined;
  contextChangedScopes: AgentScope[];
};

export type AgentIndexServiceStores = {
  agentContextSnapshots?: ConstructorParameters<
    typeof AgentContextService
  >[0]["stores"]["agentContextSnapshots"];
  agentEvents: AgentEventStore;
  agentOrchestratorScopes?: AgentOrchestratorScopeStore;
  agentHistoryCache?: AgentHistoryCacheStore;
  agents: AgentStore;
  herdrSessions: HerdrSessionStore;
  herdrWorkspaces: HerdrWorkspaceStore;
};

type RefreshInput = {
  herdrSessionName: string;
  sessionDir: string;
  socketPath: string;
};

export class AgentIndexService {
  readonly #clientFactory: (input: {
    socketPath: string;
  }) => Pick<HerdrSocketClient, "close" | "sessionSnapshot">;
  readonly #context: AgentContextService;
  readonly #stores: AgentIndexServiceStores;
  readonly #mutationEpochBySession = new Map<string, number>();
  readonly #pendingPiSessionRefs = new Map<string, AgentSessionRef>();
  readonly #refreshInFlightBySession = new Map<
    string,
    { epoch: number; promise: Promise<AgentIndexRefreshResult> }
  >();
  readonly #sessionOperationTail = new Map<string, Promise<void>>();
  readonly #turnCompletions: TurnCompletionRegistry | undefined;

  constructor(options: {
    clientFactory?: (input: {
      socketPath: string;
    }) => Pick<HerdrSocketClient, "close" | "sessionSnapshot">;
    context?: AgentContextService;
    history?: AgentHistoryService;
    stores: AgentIndexServiceStores;
    turnCompletions?: TurnCompletionRegistry;
  }) {
    this.#clientFactory = options.clientFactory ?? ((input) => new HerdrSocketClient(input));
    this.#stores = options.stores;
    this.#turnCompletions = options.turnCompletions;
    if (options.context) {
      this.#context = options.context;
    } else {
      if (!options.stores.agentContextSnapshots) {
        throw new Error("AgentIndexService requires context or agentContextSnapshots store");
      }
      const history =
        options.history ??
        createAgentHistoryService({
          ...(options.stores.agentHistoryCache ? { cache: options.stores.agentHistoryCache } : {}),
        });
      this.#context = new AgentContextService({
        history,
        stores: {
          agentContextSnapshots: options.stores.agentContextSnapshots,
          agents: options.stores.agents,
        },
      });
    }
  }

  refreshHerdrSession(input: RefreshInput): Promise<AgentIndexRefreshResult> {
    const epoch = this.#mutationEpochBySession.get(input.herdrSessionName) ?? 0;
    const existing = this.#refreshInFlightBySession.get(input.herdrSessionName);
    if (existing?.epoch === epoch) return existing.promise;
    const promise = this.#enqueueSessionOperation(input.herdrSessionName, () =>
      this.#refreshHerdrSessionNow(input),
    );
    this.#refreshInFlightBySession.set(input.herdrSessionName, { epoch, promise });
    const clear = () => {
      if (this.#refreshInFlightBySession.get(input.herdrSessionName)?.promise === promise) {
        this.#refreshInFlightBySession.delete(input.herdrSessionName);
      }
    };
    void promise.then(clear, clear);
    return promise;
  }

  handleHerdrEvent(input: {
    event: unknown;
    herdrSessionName: string;
    sessionDir: string;
    socketPath: string;
  }): Promise<AgentEventHandlingResult> {
    this.#incrementMutationEpoch(input.herdrSessionName);
    return this.#enqueueSessionOperation(input.herdrSessionName, () =>
      this.#handleHerdrEventNow(input),
    );
  }

  registerPiSessionRef(input: {
    herdrSessionName: string;
    sessionRef: AgentSessionRef;
    terminalId: string;
  }): Promise<PiSessionRefRegistrationResult> {
    this.#incrementMutationEpoch(input.herdrSessionName);
    return this.#enqueueSessionOperation(input.herdrSessionName, () =>
      this.#registerPiSessionRefNow(input),
    );
  }

  async #refreshHerdrSessionNow(input: RefreshInput): Promise<AgentIndexRefreshResult> {
    const client = this.#clientFactory({ socketPath: input.socketPath });
    try {
      const previous = this.#stores.agents.listForHerdrSession(input.herdrSessionName);
      const previousByPane = new Map(
        previous.map((agent) => [paneIdentityKey(agent.paneId, agent.paneGeneration), agent]),
      );
      const previousByTerminal = new Map(
        previous.flatMap((agent) =>
          agent.terminalId ? ([[agent.terminalId, agent]] as const) : [],
        ),
      );
      const refreshEpoch = this.#mutationEpochBySession.get(input.herdrSessionName) ?? 0;
      const snapshot = normalizeHerdrSessionSnapshot(await client.sessionSnapshot());
      const mutationEpoch = this.#mutationEpochBySession.get(input.herdrSessionName) ?? 0;
      if (mutationEpoch !== refreshEpoch) {
        console.warn("Herdsman discarding stale Herdr session refresh", {
          sessionName: input.herdrSessionName,
          refreshEpoch,
          mutationEpoch,
        });
        return { agents: previous, contextChangedScopes: [], events: [] };
      }
      if (mutationEpoch !== refreshEpoch) {
        console.warn("Herdsman discarding stale Herdr session refresh", {
          sessionName: input.herdrSessionName,
          refreshEpoch,
          mutationEpoch,
        });
        return { agents: previous, contextChangedScopes: [], events: [] };
      }
      const revisionByPane = new Map(
        snapshot.panes.flatMap((pane) => {
          const value = record(pane);
          const paneId = stringValue(value.pane_id) ?? stringValue(value.paneId);
          const revision = integerValue(value.revision);
          return paneId && revision !== undefined ? ([[paneId, revision]] as const) : [];
        }),
      );
      const snapshotAgents = snapshot.agents.map((agent) =>
        withPaneRevision(agent, revisionByPane),
      );
      // A live pane that reports a pane_generation overrides any generation-less
      // (legacy) close: drop the legacy tombstone before consulting
      // isPaneClosed so the generation-carrying agent is indexed instead of
      // being swallowed by the phantom close. Generation-scoped tombstones stay.
      for (const agent of snapshotAgents) {
        const paneId = stringValue(agent.pane_id) ?? stringValue(agent.paneId);
        if (paneId && paneGenerationOf(agent)) {
          this.#stores.agents.clearLegacyPaneTombstone({
            herdrSessionName: input.herdrSessionName,
            paneId,
          });
        }
      }
      const liveSnapshotAgents = snapshotAgents.filter(
        (agent) => !this.#isClosedPaneAgent(input.herdrSessionName, agent),
      );
      this.#stores.herdrSessions.upsertRunning({
        name: input.herdrSessionName,
        sessionDir: input.sessionDir,
        socketPath: input.socketPath,
      });
      this.#stores.herdrWorkspaces.replaceForSession({
        herdrSessionName: input.herdrSessionName,
        workspaces: snapshot.workspaces.map(record),
      });
      const indexedAgents = this.#stores.agents.replaceForSession({
        agents: liveSnapshotAgents,
        herdrSessionName: input.herdrSessionName,
      });
      const agents = indexedAgents.map((agent) => {
        if (!agent.terminalId) return agent;
        const key = terminalSessionKey(input.herdrSessionName, agent.terminalId);
        const pending = this.#pendingPiSessionRefs.get(key);
        if (!pending) return agent;
        this.#pendingPiSessionRefs.delete(key);
        return (
          this.#stores.agents.setSessionRefByTerminal({
            agentSession: pending,
            herdrSessionName: input.herdrSessionName,
            terminalId: agent.terminalId,
          }) ?? agent
        );
      });
      const scopes = new Map<string, AgentScope>();
      const events: AgentEventRecord[] = [];
      const currentIds = new Set(agents.map((agent) => agent.id));
      for (const prior of previous) {
        if (!currentIds.has(prior.id)) addScope(scopes, scopeOf(prior));
      }
      const occupiedSessionPaths = new Set(
        agents.flatMap((candidate) =>
          candidate.agentSession?.kind === "path" ? [candidate.agentSession.value] : [],
        ),
      );
      for (const agent of agents) {
        const prior = matchingPrior(agent, previousByTerminal, previousByPane);
        const identityChanged = !prior || !sameIdentity(prior, agent);
        const metadataChanged = !prior || !sameContextMetadata(prior, agent);
        const cached = this.#context.getAgentSnapshot(agent.id);
        const dirty =
          !cached ||
          agent.paneRevision === null ||
          cached.paneRevision !== agent.paneRevision ||
          identityChanged;
        let refreshed = cached;
        if (dirty) {
          const occupiedForCurrent = new Set(occupiedSessionPaths);
          if (agent.agentSession?.kind === "path")
            occupiedForCurrent.delete(agent.agentSession.value);
          const result = await this.#context.refreshAgent({
            agent,
            identityChanged,
            occupiedSessionPaths: occupiedForCurrent,
          });
          refreshed = result.snapshot;
          if (result.changed) addScope(scopes, scopeOf(agent));
        }
        if (metadataChanged) {
          if (prior && !sameScope(scopeOf(prior), scopeOf(agent))) addScope(scopes, scopeOf(prior));
          addScope(scopes, scopeOf(agent));
        }
        if (prior && prior.agentStatus !== agent.agentStatus) {
          const event = await this.#appendStatusEvents({
            agent,
            compactHistory:
              refreshed?.compactHistory ?? this.#context.getAgentSnapshot(agent.id)?.compactHistory,
            from: prior.agentStatus,
            to: agent.agentStatus,
          });
          if (event) events.push(event);
          addScope(scopes, scopeOf(agent));
        }
      }
      return {
        agents,
        contextChangedScopes: sortedScopes(scopes),
        events,
      };
    } finally {
      client.close();
    }
  }

  async #handleHerdrEventNow(input: {
    event: unknown;
    herdrSessionName: string;
    sessionDir: string;
    socketPath: string;
  }): Promise<AgentEventHandlingResult> {
    const event = record(input.event);
    const paneId = stringValue(event.pane_id) ?? stringValue(event.paneId);
    if (!paneId) return { contextChangedScopes: [], events: [] };
    if (event.type === "pane.closed") {
      const closedGeneration = paneGenerationFromEvent(event);
      this.#stores.agentEvents.invalidatePane({
        herdrSessionName: input.herdrSessionName,
        paneId,
        paneGeneration: closedGeneration,
        invalidatedReason: closedGeneration ? "PANE_CLOSED" : "LEGACY_CLOSE_WITHOUT_GENERATION",
      });
      if (!closedGeneration) {
        console.warn("LEGACY_CLOSE_WITHOUT_GENERATION", {
          sessionName: input.herdrSessionName,
          paneId,
        });
      }
      const retired = this.#stores.agents.retirePane({
        herdrSessionName: input.herdrSessionName,
        paneId,
        paneGeneration: closedGeneration,
      });
      const scopes = new Map<string, AgentScope>();
      const closedTerminalId = stringValue(event.terminal_id) ?? stringValue(event.terminalId);
      if (closedTerminalId) {
        for (const agent of retired) {
          const scope = scopeOf(agent);
          const change = this.#stores.agentOrchestratorScopes?.releaseIfOwnerIdentity({
            ...scope,
            paneId,
            terminalId: closedTerminalId,
          });
          if (change?.changed) addScope(scopes, scope);
        }
      }
      for (const agent of retired) {
        const scope = scopeOf(agent);
        addScope(scopes, scope);
      }
      return {
        contextChangedScopes: sortedScopes(scopes),
        events: [],
      };
    }
    if (event.type !== "pane.agent_status_changed") return { contextChangedScopes: [], events: [] };
    const eventGeneration = paneGenerationFromEvent(event);
    let agent = this.#stores.agents.findByPane({
      herdrSessionName: input.herdrSessionName,
      paneId,
      paneGeneration: eventGeneration,
    });
    let recovered: AgentIndexRefreshResult | undefined;
    if (!agent) {
      if (eventGeneration) {
        // A generation-scoped tombstone still closes its own generation.
        if (
          this.#stores.agents.hasGenerationScopedTombstone({
            herdrSessionName: input.herdrSessionName,
            paneId,
            paneGeneration: eventGeneration,
          })
        ) {
          return { contextChangedScopes: [], events: [] };
        }
        // No generation-scoped close: a live generation-carrying status
        // overrides a generation-less close, so recover the pane (the refresh
        // below clears the legacy tombstone only when the agent is actually
        // present in the snapshot).
      } else if (
        this.#stores.agents.isPaneClosed({
          herdrSessionName: input.herdrSessionName,
          paneId,
          paneGeneration: null,
        })
      ) {
        return { contextChangedScopes: [], events: [] };
      }
      recovered = await this.#refreshHerdrSessionNow(input);
      agent = this.#stores.agents.findByPane({
        herdrSessionName: input.herdrSessionName,
        paneId,
        paneGeneration: eventGeneration,
      });
    }
    if (!agent) {
      return recovered
        ? { contextChangedScopes: recovered.contextChangedScopes, events: recovered.events }
        : { contextChangedScopes: [], events: [] };
    }
    const from = recovered ? "unknown" : agent.agentStatus;
    const to = parseAgentStatus(event.agent_status);
    const herdrEventKey = herdrInputIdempotencyKey(input.herdrSessionName, paneId, event, to);
    const updated = this.#stores.agents.updateStatus({
      agentStatus: to,
      herdrSessionName: input.herdrSessionName,
      paneId,
      paneGeneration: eventGeneration,
    });
    const current = updated ?? { ...agent, agentStatus: to };
    const refreshed = await this.#context.refreshAgent({ agent: current, identityChanged: false });
    const scopes = new Map<string, AgentScope>();
    for (const scope of recovered?.contextChangedScopes ?? []) addScope(scopes, scope);
    if (refreshed.changed || from !== to) addScope(scopes, scopeOf(current));
    const events = [...(recovered?.events ?? [])];
    const equivalent = events.some(
      (candidate) =>
        candidate.agentId === current.id &&
        candidate.type === statusEventType(to) &&
        (candidate.payload as { to?: AgentStatus }).to === to,
    );
    if (!equivalent) {
      const statusEvent = await this.#appendStatusEvents({
        agent: current,
        compactHistory: refreshed.snapshot.compactHistory,
        from,
        herdrEventKey,
        to,
      });

      if (statusEvent) events.push(statusEvent);
    }
    return { contextChangedScopes: sortedScopes(scopes), events };
  }

  async #registerPiSessionRefNow(input: {
    herdrSessionName: string;
    sessionRef: AgentSessionRef;
    terminalId: string;
  }): Promise<PiSessionRefRegistrationResult> {
    const key = terminalSessionKey(input.herdrSessionName, input.terminalId);
    if (input.sessionRef.kind === "path" && !safeAllowedSessionPath(input.sessionRef.value)) {
      return { agent: undefined, contextChangedScopes: [] };
    }
    const previous = this.#stores.agents.findByTerminal(input);
    if (!previous) {
      this.#pendingPiSessionRefs.set(key, input.sessionRef);
      return { agent: undefined, contextChangedScopes: [] };
    }
    const agent = this.#stores.agents.setSessionRefByTerminal({
      agentSession: input.sessionRef,
      herdrSessionName: input.herdrSessionName,
      terminalId: input.terminalId,
    });
    this.#pendingPiSessionRefs.delete(key);
    if (!agent || sameAgentSession(previous.agentSession, agent.agentSession)) {
      return { agent, contextChangedScopes: [] };
    }
    const refreshed = await this.#context.refreshAgent({ agent, identityChanged: true });
    return {
      agent,
      contextChangedScopes: refreshed.changed ? [scopeOf(agent)] : [],
    };
  }

  #incrementMutationEpoch(sessionName: string): void {
    this.#mutationEpochBySession.set(
      sessionName,
      (this.#mutationEpochBySession.get(sessionName) ?? 0) + 1,
    );
  }

  #enqueueSessionOperation<T>(sessionName: string, operation: () => Promise<T>): Promise<T> {
    const prior = this.#sessionOperationTail.get(sessionName) ?? Promise.resolve();
    const result = prior.catch(() => undefined).then(operation);
    const tail = result.then(
      () => undefined,
      () => undefined,
    );
    this.#sessionOperationTail.set(sessionName, tail);
    void tail.finally(() => {
      if (this.#sessionOperationTail.get(sessionName) === tail) {
        this.#sessionOperationTail.delete(sessionName);
      }
    });
    return result;
  }

  #isClosedPaneAgent(herdrSessionName: string, agent: HerdrAgentLike): boolean {
    const paneId = stringValue(agent.pane_id) ?? stringValue(agent.paneId);
    if (!paneId) return false;
    return this.#stores.agents.isPaneClosed({
      herdrSessionName,
      paneId,
      paneGeneration: paneGenerationOf(agent),
    });
  }

  async #appendStatusEvents(input: {
    agent: AgentIndexRecord;
    compactHistory:
      | NonNullable<ReturnType<AgentContextService["getAgentSnapshot"]>>["compactHistory"]
      | undefined;
    from: AgentStatus;
    herdrEventKey?: string;
    to: AgentStatus;
  }): Promise<AgentEventRecord | undefined> {
    if (input.from === input.to || !input.compactHistory) return undefined;
    if (
      this.#stores.agents.isPaneClosed({
        herdrSessionName: input.agent.herdrSessionName,
        paneId: input.agent.paneId,
        paneGeneration: input.agent.paneGeneration ?? null,
      })
    ) {
      return undefined;
    }
    const latest = this.#stores.agentEvents.latestStatusTransition(
      input.agent.id,
      input.agent.herdrSessionName,
    );
    if (latest && statusTransitionMatches(latest, input.from, input.to)) return undefined;
    const observationId = `transition:${latest?.id ?? 0}`;

    let compactHistory = input.compactHistory;
    if (
      this.#turnCompletions !== undefined &&
      (input.to === "done" || input.to === "blocked") &&
      input.agent.terminalId !== null
    ) {
      const turn =
        input.agent.agent === "pi"
          ? await this.#turnCompletions.waitForSignal({
              herdrSessionName: input.agent.herdrSessionName,
              recordedAfterMs: Date.now() - TURN_SIGNAL_WAIT_MS,
              terminalId: input.agent.terminalId,
            })
          : undefined;
      if (turn?.received) {
        let refreshed = await this.#context.refreshAgent({
          agent: input.agent,
          identityChanged: false,
          forceRefresh: true,
        });
        for (
          let attempt = 0;
          attempt < 8 && refreshed.snapshot.compactHistory.lastAssistantMessage === null;
          attempt += 1
        ) {
          await new Promise((resolve) => setTimeout(resolve, 500));
          refreshed = await this.#context.refreshAgent({
            agent: input.agent,
            identityChanged: false,
            forceRefresh: true,
          });
        }
        compactHistory = refreshed.snapshot.compactHistory;
        console.log(
          `Herdsman emitted pi agent.${input.to} after turn completion signal (confirmed=${turn.confirmed})`,
          {
            agentId: input.agent.id,
            herdrSessionName: input.agent.herdrSessionName,
            terminalId: input.agent.terminalId,
          },
        );
      } else {
        for (
          let attempt = 0;
          attempt < 8 && compactHistory.lastAssistantMessage === null;
          attempt += 1
        ) {
          await new Promise((resolve) => setTimeout(resolve, 500));
          const refreshed = await this.#context.refreshAgent({
            agent: input.agent,
            identityChanged: false,
            forceRefresh: true,
          });
          compactHistory = refreshed.snapshot.compactHistory;
        }
        console.warn(
          `Herdsman emitted pi agent.${input.to} without a turn completion signal after ${TURN_SIGNAL_WAIT_MS}ms`,
          {
            agentId: input.agent.id,
            herdrSessionName: input.agent.herdrSessionName,
            terminalId: input.agent.terminalId,
          },
        );
      }
    }

    const lastEvent = this.#appendAndAckSelfEvent({
      agentId: input.agent.id,
      compactHistory,
      herdrSessionName: input.agent.herdrSessionName,
      idempotencyKey: idempotencyKey(
        "agent.status.changed",
        input.agent,
        input.from,
        input.to,
        `${observationId}:${input.herdrEventKey ?? "legacy"}:agent.status.changed`,
      ),
      paneId: input.agent.paneId,
      paneGeneration: input.agent.paneGeneration ?? null,
      payload: payload(input.agent, input.from, input.to),
      terminalId: input.agent.terminalId,
      type: "agent.status.changed",
      workspaceId: input.agent.workspaceId,
    });
    const statusType = statusEventType(input.to);
    if (statusType) {
      if (
        statusType === "agent.idle" &&
        input.agent.agent !== "pi" &&
        !hasNonEmptyAssistantMessage(compactHistory)
      ) {
        console.debug(
          "Herdsman skipping agent.idle event generation for non-pi agent without assistant message",
          {
            agent: input.agent.agent,
            agentId: input.agent.id,
            from: input.from,
            herdrSessionName: input.agent.herdrSessionName,
            paneId: input.agent.paneId,
          },
        );
        return undefined;
      }
      return this.#appendAndAckSelfEvent({
        agentId: input.agent.id,
        compactHistory,
        herdrSessionName: input.agent.herdrSessionName,
        idempotencyKey: idempotencyKey(
          statusType,
          input.agent,
          input.from,
          input.to,
          `${observationId}:${input.herdrEventKey ?? "legacy"}:${statusType}`,
        ),
        paneId: input.agent.paneId,
        paneGeneration: input.agent.paneGeneration ?? null,
        payload: payload(input.agent, input.from, input.to),
        terminalId: input.agent.terminalId,
        type: statusType,
        workspaceId: input.agent.workspaceId,
      });
    }
    return lastEvent;
  }

  #appendAndAckSelfEvent(input: Parameters<AgentEventStore["append"]>[0]): AgentEventRecord {
    const event = this.#stores.agentEvents.append(input);
    const owner = this.#stores.agentOrchestratorScopes?.get({
      herdrSessionName: input.herdrSessionName,
      workspaceId: input.workspaceId ?? "",
    });
    if (owner?.owner?.paneId && owner.owner.paneId === input.paneId) {
      this.#stores.agentEvents.markAcked(event.id);
      return this.#stores.agentEvents.get(event.id);
    }
    return event;
  }
}

function withPaneRevision(agent: unknown, revisionByPane: Map<string, number>): HerdrAgentLike {
  const raw = record(agent);
  if (integerValue(raw.revision) !== undefined) return raw;
  const paneId = stringValue(raw.pane_id) ?? stringValue(raw.paneId);
  const revision = paneId ? revisionByPane.get(paneId) : undefined;
  return revision === undefined ? raw : { ...raw, revision };
}

function paneGenerationOf(agent: HerdrAgentLike): string | null {
  return (
    stringValue(agent.pane_generation) ??
    stringValue(agent.paneGeneration) ??
    stringValue(agent.creation_id) ??
    stringValue(agent.creationId)
  );
}

function matchingPrior(
  agent: AgentIndexRecord,
  previousByTerminal: Map<string, AgentIndexRecord>,
  previousByPane: Map<string, AgentIndexRecord>,
): AgentIndexRecord | undefined {
  const terminalMatch = agent.terminalId ? previousByTerminal.get(agent.terminalId) : undefined;
  const generationMatches =
    !agent.paneGeneration ||
    !terminalMatch?.paneGeneration ||
    terminalMatch.paneGeneration === agent.paneGeneration;
  const paneMatch = previousByPane.get(paneIdentityKey(agent.paneId, agent.paneGeneration));
  const canUsePaneFallback = paneMatch && (!agent.terminalId || !paneMatch.terminalId);
  return (
    (generationMatches ? terminalMatch : undefined) ?? (canUsePaneFallback ? paneMatch : undefined)
  );
}

function sameIdentity(left: AgentIndexRecord, right: AgentIndexRecord): boolean {
  return (
    left.agent === right.agent &&
    left.terminalId === right.terminalId &&
    left.cwd === right.cwd &&
    left.foregroundCwd === right.foregroundCwd &&
    sameAgentSession(left.agentSession, right.agentSession)
  );
}

function sameContextMetadata(left: AgentIndexRecord, right: AgentIndexRecord): boolean {
  return (
    sameIdentity(left, right) &&
    left.name === right.name &&
    left.agentStatus === right.agentStatus &&
    left.paneId === right.paneId &&
    left.tabId === right.tabId &&
    left.workspaceId === right.workspaceId
  );
}

function sameAgentSession(left: AgentSessionRef | null, right: AgentSessionRef | null): boolean {
  return (
    left?.agent === right?.agent &&
    left?.kind === right?.kind &&
    left?.source === right?.source &&
    left?.value === right?.value
  );
}

function terminalSessionKey(herdrSessionName: string, terminalId: string): string {
  return `${herdrSessionName}\0${terminalId}`;
}

function scopeOf(agent: AgentIndexRecord): AgentScope {
  return { herdrSessionName: agent.herdrSessionName, workspaceId: agent.workspaceId };
}

function addScope(scopes: Map<string, AgentScope>, scope: AgentScope): void {
  scopes.set(`${scope.herdrSessionName}\0${scope.workspaceId}`, scope);
}

function sameScope(left: AgentScope, right: AgentScope): boolean {
  return left.herdrSessionName === right.herdrSessionName && left.workspaceId === right.workspaceId;
}

function sortedScopes(scopes: Map<string, AgentScope>): AgentScope[] {
  return [...scopes.values()].sort(
    (left, right) =>
      left.herdrSessionName.localeCompare(right.herdrSessionName) ||
      left.workspaceId.localeCompare(right.workspaceId),
  );
}

function statusEventType(
  status: AgentStatus,
): "agent.blocked" | "agent.done" | "agent.idle" | undefined {
  if (status === "blocked") return "agent.blocked";
  if (status === "done") return "agent.done";
  if (status === "idle") return "agent.idle";
  return undefined;
}

function payload(agent: AgentIndexRecord, from: AgentStatus, to: AgentStatus) {
  return {
    agent: agent.agent,
    from,
    name: agent.name,
    herdrSessionName: agent.herdrSessionName,
    paneId: agent.paneId,
    terminalId: agent.terminalId,
    to,
    workspaceId: agent.workspaceId,
  };
}

function idempotencyKey(
  type: string,
  agent: AgentIndexRecord,
  from: AgentStatus,
  to: AgentStatus,
  observationId: string,
): string {
  return `${type}:${agent.herdrSessionName}:${agent.paneId}:${from}:${to}:${observationId}`;
}

function herdrInputIdempotencyKey(
  sessionName: string,
  paneId: string,
  event: Record<string, unknown>,
  targetStatus: AgentStatus,
): string {
  const eventId =
    stringValue(event.id) ?? stringValue(event.event_id) ?? stringValue(event.eventId);
  if (eventId) return `${sessionName}:${paneId}:${String(event.type)}:${eventId}`;
  const revision = integerValue(event.revision) ?? integerValue(event.pane_revision) ?? "unknown";
  return `${sessionName}:${paneId}:${revision}:${targetStatus}`;
}

function statusTransitionMatches(
  event: AgentEventRecord,
  from: AgentStatus,
  to: AgentStatus,
): boolean {
  const eventPayload = event.payload as { from?: AgentStatus; to?: AgentStatus };
  return eventPayload.from === from && eventPayload.to === to;
}

function record(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

function paneGenerationFromEvent(event: Record<string, unknown>): string | null {
  return stringValue(event.pane_generation) ?? stringValue(event.paneGeneration);
}

function paneIdentityKey(paneId: string, paneGeneration: string | null | undefined): string {
  return `${paneId}\0${paneGeneration ?? ""}`;
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}

function integerValue(value: unknown): number | undefined {
  return typeof value === "number" && Number.isInteger(value) && value >= 0 ? value : undefined;
}
