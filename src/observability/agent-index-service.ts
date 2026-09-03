import { safeAllowedSessionPath } from "@/agent-history/discovery.js";
import { type AgentHistoryService, createAgentHistoryService } from "@/agent-history/service.js";
import { type AgentEventStore, hasNonEmptyAssistantMessage } from "@/db/agent-events.js";
import type { AgentHistoryCacheStore } from "@/db/agent-history-cache.js";
import type { AgentOrchestratorScopeStore } from "@/db/agent-orchestrator-scopes.js";
import type { AgentStore, HerdrAgentLike } from "@/db/agents.js";
import type { HerdrSessionStore } from "@/db/herdr-sessions.js";
import type { HerdrWorkspaceStore } from "@/db/herdr-workspaces.js";
import type { StatusEventPlanRecord, StatusEventPlanStore } from "@/db/status-event-plans.js";
import { normalizeHerdrSessionSnapshot } from "@/herdr/session-snapshot.js";
import { HerdrSocketClient } from "@/herdr/socket-client.js";
import { AgentContextService } from "@/observability/agent-context-service.js";
import {
  type AgentEventRecord,
  type AgentIndexRecord,
  type AgentScope,
  type AgentSessionRef,
  type AgentStatus,
  type CompactAgentHistory,
  parseAgentStatus,
} from "@/observability/contracts.js";
import type { TurnCompletionRegistry } from "@/observability/turn-completion.js";
import { TURN_SIGNAL_WAIT_MS } from "@/observability/turn-completion.js";

export function historyHasAdvanced(
  current: CompactAgentHistory,
  baseline: CompactAgentHistory | null | undefined,
): boolean {
  if (
    current.lastAssistantMessage?.ref &&
    current.lastAssistantMessage.ref !== (baseline?.lastAssistantMessage?.ref ?? null)
  ) {
    return true;
  }
  return current.messageCount > (baseline?.messageCount ?? 0);
}

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
  statusEventPlans: StatusEventPlanStore;
};

type RefreshInput = {
  herdrSessionName: string;
  sessionDir: string;
  socketPath: string;
};

export type StatusEventPlan = {
  agent: AgentIndexRecord;
  compactHistory: CompactAgentHistory | undefined;
  from: AgentStatus;
  herdrEventKey?: string;
  to: AgentStatus;
};

/**
 * Sentinel returned by #appendStatusEvents when the plan must be CANCELLED
 * rather than skipped (skip still maps to completed in #runPlanRow). The agent
 * row is gone, so appending would create an undeliverable dangling event.
 */
const PLAN_CANCELLED = Symbol("herdsman.status-event-plan-cancelled");

export type AgentIndexRefreshFastResult = {
  agents: AgentIndexRecord[];
  contextChangedScopes: AgentScope[];
  events: AgentEventRecord[];
  statusEventPlans: StatusEventPlan[];
};

export type AgentEventHandlingFastResult = {
  contextChangedScopes: AgentScope[];
  events: AgentEventRecord[];
  statusEventPlans: StatusEventPlan[];
};

type RefreshInternalResult = AgentIndexRefreshFastResult;
type EventHandlingInternalResult = AgentEventHandlingFastResult;

export class AgentIndexService {
  readonly #activeWaiters = new Map<string, Set<AbortController>>();
  readonly #clientFactory: (input: {
    socketPath: string;
  }) => Pick<HerdrSocketClient, "close" | "sessionSnapshot">;
  readonly #context: AgentContextService;
  readonly #stores: AgentIndexServiceStores;
  readonly #mutationEpochBySession = new Map<string, number>();
  readonly #pendingPiSessionRefs = new Map<string, AgentSessionRef>();
  readonly #planTailByAgent = new Map<string, Promise<void>>();
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

  refreshHerdrSessionFast(input: RefreshInput): Promise<AgentIndexRefreshFastResult> {
    this.#incrementMutationEpoch(input.herdrSessionName);
    return this.#enqueueSessionOperation(input.herdrSessionName, () =>
      this.#refreshHerdrSessionNow(input),
    );
  }

  refreshHerdrSession(input: RefreshInput): Promise<AgentIndexRefreshResult> {
    const epoch = this.#mutationEpochBySession.get(input.herdrSessionName) ?? 0;
    const existing = this.#refreshInFlightBySession.get(input.herdrSessionName);
    if (existing?.epoch === epoch) return existing.promise;
    const promise = this.#enqueueSessionOperation(input.herdrSessionName, () =>
      this.#refreshHerdrSessionNow(input),
    ).then(async (intermediate) => {
      const statusEvents = await Promise.all(
        intermediate.statusEventPlans.map((plan) => this.executeStatusEventPlan(plan)),
      );
      return {
        agents: intermediate.agents,
        contextChangedScopes: intermediate.contextChangedScopes,
        events: [
          ...intermediate.events,
          ...statusEvents.filter((event): event is AgentEventRecord => event !== undefined),
        ],
      };
    });
    this.#refreshInFlightBySession.set(input.herdrSessionName, { epoch, promise });
    const clear = () => {
      if (this.#refreshInFlightBySession.get(input.herdrSessionName)?.promise === promise) {
        this.#refreshInFlightBySession.delete(input.herdrSessionName);
      }
    };
    void promise.then(clear, clear);
    return promise;
  }

  handleHerdrEventFast(input: {
    event: unknown;
    herdrSessionName: string;
    sessionDir: string;
    socketPath: string;
  }): Promise<AgentEventHandlingFastResult> {
    this.#incrementMutationEpoch(input.herdrSessionName);
    return this.#enqueueSessionOperation(input.herdrSessionName, () =>
      this.#handleHerdrEventNow(input),
    );
  }

  async handleHerdrEvent(input: {
    event: unknown;
    herdrSessionName: string;
    sessionDir: string;
    socketPath: string;
  }): Promise<AgentEventHandlingResult> {
    const intermediate = await this.handleHerdrEventFast(input);
    const statusEvents = await Promise.all(
      intermediate.statusEventPlans.map((plan) => this.executeStatusEventPlan(plan)),
    );
    return {
      contextChangedScopes: intermediate.contextChangedScopes,
      events: [
        ...intermediate.events,
        ...statusEvents.filter((event): event is AgentEventRecord => event !== undefined),
      ],
    };
  }

  async executeStatusEventPlan(plan: StatusEventPlan): Promise<AgentEventRecord | undefined> {
    if (plan.from === plan.to) return undefined;
    if (!this.#stores.statusEventPlans) {
      return this.#enqueueAgentPlan(plan.agent.id, async () => {
        const event = await this.#appendStatusEvents(plan);
        return event === PLAN_CANCELLED ? undefined : event;
      });
    }
    const inserted = this.#stores.statusEventPlans.insertPending({
      agent: plan.agent,
      from: plan.from,
      herdrEventKey: plan.herdrEventKey ?? null,
      to: plan.to,
      ...(plan.compactHistory ? { compactHistory: plan.compactHistory } : {}),
    });
    return this.#enqueueAgentPlan(plan.agent.id, () => this.#runPlanRow(inserted, plan));
  }

  async drainPendingPlans(): Promise<void> {
    if (!this.#stores.statusEventPlans) return;
    this.#stores.statusEventPlans.resetRunningToPending();
    const rows = this.#stores.statusEventPlans.listUnfinished();
    const tasks = rows.map((row) => {
      try {
        return this.#enqueueAgentPlan(row.agentId, () => this.#drainPlanRow(row));
      } catch (_error) {
        // A row whose agent cannot even be resolved must never take down the
        // daemon boot drain (that failure mode used to boot-loop systemd).
        try {
          this.#stores.statusEventPlans.markCancelled(row.id);
        } catch {
          // The row may already be gone; the drain must continue regardless.
        }
        console.warn("Herdsman cancelling status event plan for missing agent", {
          agentId: row.agentId,
          herdrSessionName: row.herdrSessionName,
          paneId: row.paneId,
          from: row.fromStatus,
          to: row.toStatus,
        });
        return Promise.resolve();
      }
    });
    // Individual rows may reject (after markRetry) but the drain itself never
    // rejects: every row is either drained, cancelled, or retried.
    await Promise.allSettled(tasks);
  }

  async #drainPlanRow(row: StatusEventPlanRecord): Promise<void> {
    let agent: AgentIndexRecord | undefined;
    try {
      agent =
        this.#stores.agents.findByPane({
          herdrSessionName: row.herdrSessionName,
          paneId: row.paneId,
          paneGeneration: row.paneGeneration,
        }) ?? this.#stores.agents.get(row.agentId);
    } catch {
      agent = undefined;
    }
    if (!agent) {
      this.#stores.statusEventPlans.markCancelled(row.id);
      console.warn("Herdsman cancelling status event plan for missing agent", {
        agentId: row.agentId,
        herdrSessionName: row.herdrSessionName,
        paneId: row.paneId,
        from: row.fromStatus,
        to: row.toStatus,
      });
      return;
    }
    const plan: StatusEventPlan = {
      agent,
      compactHistory: row.compactHistory,
      from: row.fromStatus,
      to: row.toStatus,
      ...(row.herdrEventKey ? { herdrEventKey: row.herdrEventKey } : {}),
    };
    await this.#runPlanRow(row, plan);
  }

  #enqueueAgentPlan<T>(agentId: string, operation: () => Promise<T>): Promise<T> {
    const previous = this.#planTailByAgent.get(agentId) ?? Promise.resolve();
    const result = previous.then(operation, operation);
    const tail = result.then(
      () => undefined,
      () => undefined,
    );
    this.#planTailByAgent.set(agentId, tail);
    void tail.finally(() => {
      if (this.#planTailByAgent.get(agentId) === tail) {
        this.#planTailByAgent.delete(agentId);
      }
    });
    return result;
  }

  async #runPlanRow(
    row: StatusEventPlanRecord,
    plan: StatusEventPlan,
  ): Promise<AgentEventRecord | undefined> {
    const store = this.#stores.statusEventPlans;
    if (!store) return undefined;
    const current = store.get(row.id);
    if (
      current.status === "completed" ||
      current.status === "cancelled" ||
      current.status === "failed"
    ) {
      return undefined;
    }
    store.markRunning(row.id);
    try {
      const event = await this.#appendStatusEvents(plan);
      if (event === PLAN_CANCELLED) {
        store.markCancelled(row.id);
        return undefined;
      }
      if (
        event === undefined &&
        this.#stores.agents.isPaneClosed({
          herdrSessionName: plan.agent.herdrSessionName,
          paneId: plan.agent.paneId,
          paneGeneration: plan.agent.paneGeneration ?? null,
        })
      ) {
        store.markCancelled(row.id);
        return undefined;
      }
      store.markCompleted(row.id);
      return event;
    } catch (error) {
      const err = error instanceof Error ? error : undefined;
      const isPaneClosed =
        this.#stores.agents.isPaneClosed({
          herdrSessionName: plan.agent.herdrSessionName,
          paneId: plan.agent.paneId,
          paneGeneration: plan.agent.paneGeneration ?? null,
        }) ||
        err?.name === "AbortError" ||
        err?.message?.includes("aborted");
      if (isPaneClosed) {
        store.markCancelled(row.id);
        return undefined;
      }
      store.markRetry(row.id, error);
      throw error;
    }
  }

  async #waitForHistoryAdvance(input: {
    agent: AgentIndexRecord;
    baseline: CompactAgentHistory | null | undefined;
    controller: AbortController;
    initial?: CompactAgentHistory;
    maxAttempts?: number;
  }): Promise<CompactAgentHistory | undefined> {
    const maxAttempts = input.maxAttempts ?? 8;
    let refreshed: { snapshot: { compactHistory: CompactAgentHistory } } = input.initial
      ? { snapshot: { compactHistory: input.initial } }
      : await this.#context.refreshAgent({
          agent: input.agent,
          forceRefresh: true,
          identityChanged: false,
        });
    for (
      let attempt = 0;
      attempt < maxAttempts &&
      !historyHasAdvanced(refreshed.snapshot.compactHistory, input.baseline) &&
      !input.controller.signal.aborted;
      attempt += 1
    ) {
      await sleep(500, input.controller.signal);
      if (
        input.controller.signal.aborted ||
        this.#stores.agents.isPaneClosed({
          herdrSessionName: input.agent.herdrSessionName,
          paneId: input.agent.paneId,
          paneGeneration: input.agent.paneGeneration ?? null,
        })
      ) {
        console.debug(
          "Herdsman skipping status event generation because wait was aborted during history refresh",
          {
            aborted: input.controller.signal.aborted,
            agentId: input.agent.id,
            herdrSessionName: input.agent.herdrSessionName,
            paneId: input.agent.paneId,
          },
        );
        return undefined;
      }
      refreshed = await this.#context.refreshAgent({
        agent: input.agent,
        forceRefresh: true,
        identityChanged: false,
      });
    }
    return refreshed.snapshot.compactHistory;
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

  async #refreshHerdrSessionNow(input: RefreshInput): Promise<RefreshInternalResult> {
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
        return { agents: previous, contextChangedScopes: [], events: [], statusEventPlans: [] };
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
      const statusEventPlans: StatusEventPlan[] = [];
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
          statusEventPlans.push({
            agent,
            compactHistory:
              refreshed?.compactHistory ?? this.#context.getAgentSnapshot(agent.id)?.compactHistory,
            from: prior.agentStatus,
            to: agent.agentStatus,
          });
          addScope(scopes, scopeOf(agent));
        }
      }
      return {
        agents,
        contextChangedScopes: sortedScopes(scopes),
        events,
        statusEventPlans,
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
  }): Promise<EventHandlingInternalResult> {
    const event = record(input.event);
    const paneId = stringValue(event.pane_id) ?? stringValue(event.paneId);
    if (!paneId) return { contextChangedScopes: [], events: [], statusEventPlans: [] };
    if (event.type === "pane.closed") {
      const closedGeneration = paneGenerationFromEvent(event);
      this.#abortPendingWaiters({
        herdrSessionName: input.herdrSessionName,
        paneId,
        paneGeneration: closedGeneration,
      });
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
        statusEventPlans: [],
      };
    }
    if (event.type !== "pane.agent_status_changed") {
      return { contextChangedScopes: [], events: [], statusEventPlans: [] };
    }
    const eventGeneration = paneGenerationFromEvent(event);
    let agent = this.#stores.agents.findByPane({
      herdrSessionName: input.herdrSessionName,
      paneId,
      paneGeneration: eventGeneration,
    });
    let recovered: RefreshInternalResult | undefined;
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
          return { contextChangedScopes: [], events: [], statusEventPlans: [] };
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
        return { contextChangedScopes: [], events: [], statusEventPlans: [] };
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
        ? {
            contextChangedScopes: recovered.contextChangedScopes,
            events: recovered.events,
            statusEventPlans: recovered.statusEventPlans,
          }
        : { contextChangedScopes: [], events: [], statusEventPlans: [] };
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
    const statusEventPlans = [...(recovered?.statusEventPlans ?? [])];
    const equivalent = statusEventPlans.some(
      (candidate) => candidate.agent.id === current.id && candidate.to === to,
    );
    if (!equivalent) {
      statusEventPlans.push({
        agent: current,
        compactHistory: refreshed.snapshot.compactHistory,
        from,
        ...(herdrEventKey ? { herdrEventKey } : {}),
        to,
      });
    }
    return { contextChangedScopes: sortedScopes(scopes), events, statusEventPlans };
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

  #registerActiveWaiter(agent: AgentIndexRecord, controller: AbortController): () => void {
    const key = `${agent.herdrSessionName}\0${agent.paneId}\0${agent.paneGeneration ?? ""}`;
    let set = this.#activeWaiters.get(key);
    if (!set) {
      set = new Set();
      this.#activeWaiters.set(key, set);
    }
    set.add(controller);
    return () => {
      set.delete(controller);
      if (set.size === 0 && this.#activeWaiters.get(key) === set) {
        this.#activeWaiters.delete(key);
      }
    };
  }

  #abortPendingWaiters(input: {
    herdrSessionName: string;
    paneId: string;
    paneGeneration?: string | null;
  }): void {
    for (const [key, controllers] of this.#activeWaiters) {
      const [sessionName, paneId, paneGen] = key.split("\0");
      if (sessionName === input.herdrSessionName && paneId === input.paneId) {
        if (!input.paneGeneration || !paneGen || input.paneGeneration === paneGen) {
          for (const controller of controllers) {
            controller.abort();
          }
          controllers.clear();
          this.#activeWaiters.delete(key);
        }
      }
    }
  }

  async #appendStatusEvents(
    input: StatusEventPlan,
  ): Promise<AgentEventRecord | undefined | typeof PLAN_CANCELLED> {
    if (input.from === input.to || !input.compactHistory) return undefined;
    if (
      this.#stores.agents.isPaneClosed({
        herdrSessionName: input.agent.herdrSessionName,
        paneId: input.agent.paneId,
        paneGeneration: input.agent.paneGeneration ?? null,
      })
    ) {
      console.debug("Herdsman skipping status event generation because pane is closed", {
        agentId: input.agent.id,
        herdrSessionName: input.agent.herdrSessionName,
        paneId: input.agent.paneId,
      });
      return undefined;
    }
    const latest = this.#stores.agentEvents.latestStatusTransition(
      input.agent.id,
      input.agent.herdrSessionName,
    );
    if (latest && statusTransitionMatches(latest, input.from, input.to)) {
      console.debug("Herdsman skipping duplicate status transition event", {
        agentId: input.agent.id,
        from: input.from,
        to: input.to,
      });
      return undefined;
    }
    const observationId = `transition:${latest?.id ?? 0}`;

    const controller = new AbortController();
    const unregister = this.#registerActiveWaiter(input.agent, controller);

    try {
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
                signal: controller.signal,
                terminalId: input.agent.terminalId,
              })
            : undefined;

        if (
          controller.signal.aborted ||
          this.#stores.agents.isPaneClosed({
            herdrSessionName: input.agent.herdrSessionName,
            paneId: input.agent.paneId,
            paneGeneration: input.agent.paneGeneration ?? null,
          })
        ) {
          console.debug(
            "Herdsman skipping status event generation because turn wait was aborted or pane closed",
            {
              aborted: controller.signal.aborted,
              agentId: input.agent.id,
              herdrSessionName: input.agent.herdrSessionName,
              paneId: input.agent.paneId,
            },
          );
          return undefined;
        }

        if (turn?.received) {
          if (hasNonEmptyAssistantMessage(input.compactHistory)) {
            compactHistory = input.compactHistory;
          } else {
            const advanced = await this.#waitForHistoryAdvance({
              agent: input.agent,
              baseline: input.compactHistory,
              controller,
              maxAttempts: 8,
            });
            if (advanced === undefined) return undefined;
            compactHistory = advanced;
          }
          console.log(
            `Herdsman emitted pi agent.${input.to} after turn completion signal (confirmed=${turn.confirmed})`,
            {
              agentId: input.agent.id,
              herdrSessionName: input.agent.herdrSessionName,
              terminalId: input.agent.terminalId,
            },
          );
        } else if (!controller.signal.aborted) {
          const advanced = await this.#waitForHistoryAdvance({
            agent: input.agent,
            baseline: input.compactHistory,
            controller,
            maxAttempts: 8,
          });
          if (advanced === undefined) return undefined;
          compactHistory = advanced;
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

      if (
        controller.signal.aborted ||
        this.#stores.agents.isPaneClosed({
          herdrSessionName: input.agent.herdrSessionName,
          paneId: input.agent.paneId,
          paneGeneration: input.agent.paneGeneration ?? null,
        })
      ) {
        console.debug(
          "Herdsman skipping status event generation because turn wait was aborted or pane closed before append",
          {
            aborted: controller.signal.aborted,
            agentId: input.agent.id,
            herdrSessionName: input.agent.herdrSessionName,
            paneId: input.agent.paneId,
          },
        );
        return undefined;
      }

      const currentAgent = this.#stores.agents.findByPane({
        herdrSessionName: input.agent.herdrSessionName,
        paneId: input.agent.paneId,
        paneGeneration: input.agent.paneGeneration ?? null,
      });
      if (!currentAgent) {
        // The agent row is gone: appending would create a dangling event with
        // a dead agent_id that only the reconciler could sweep. Cancel the
        // plan instead of marking it completed (the skip->completed branch
        // must stay reserved for genuine skips).
        console.debug("Herdsman cancelling terminal status plan because agent row is gone", {
          agentId: input.agent.id,
          herdrSessionName: input.agent.herdrSessionName,
          paneId: input.agent.paneId,
          from: input.from,
          to: input.to,
        });
        return PLAN_CANCELLED;
      }
      let targetAgent: AgentIndexRecord;
      if (currentAgent.agentStatus !== input.to) {
        if (input.to === "done" || input.to === "blocked") {
          console.info("Herdsman emitting terminal status event after subsequent status change", {
            agentId: input.agent.id,
            current: currentAgent.agentStatus,
            expected: input.to,
            herdrSessionName: input.agent.herdrSessionName,
            paneId: input.agent.paneId,
          });
          targetAgent = currentAgent;
        } else {
          console.debug("Herdsman skipping status event generation due to status mismatch", {
            agentId: input.agent.id,
            current: currentAgent.agentStatus,
            expected: input.to,
            herdrSessionName: input.agent.herdrSessionName,
            paneId: input.agent.paneId,
          });
          return undefined;
        }
      } else {
        targetAgent = currentAgent;
      }

      const lastEvent = this.#appendAndAckSelfEvent({
        agentId: targetAgent.id,
        compactHistory,
        herdrSessionName: targetAgent.herdrSessionName,
        idempotencyKey: idempotencyKey(
          "agent.status.changed",
          targetAgent,
          input.from,
          input.to,
          `${observationId}:${input.herdrEventKey ?? "legacy"}:agent.status.changed`,
        ),
        paneId: targetAgent.paneId,
        paneGeneration: targetAgent.paneGeneration ?? null,
        payload: payload(targetAgent, input.from, input.to),
        terminalId: targetAgent.terminalId,
        type: "agent.status.changed",
        workspaceId: targetAgent.workspaceId,
      });
      const statusType = statusEventType(input.to);
      if (
        statusType === "agent.idle" &&
        targetAgent.agent !== "pi" &&
        !hasNonEmptyAssistantMessage(compactHistory)
      ) {
        const advanced = await this.#waitForHistoryAdvance({
          agent: input.agent,
          baseline: input.compactHistory,
          controller,
          initial: compactHistory,
          maxAttempts: 8,
        });
        if (advanced === undefined) return undefined;
        compactHistory = advanced;
        if (!hasNonEmptyAssistantMessage(compactHistory)) {
          console.debug(
            "Herdsman skipping agent.idle event generation for non-pi agent without assistant message",
            {
              agent: targetAgent.agent,
              agentId: targetAgent.id,
              from: input.from,
              herdrSessionName: targetAgent.herdrSessionName,
              paneId: targetAgent.paneId,
            },
          );
          return undefined;
        }
      }
      if (statusType) {
        return this.#appendAndAckSelfEvent({
          agentId: targetAgent.id,
          compactHistory,
          herdrSessionName: targetAgent.herdrSessionName,
          idempotencyKey: idempotencyKey(
            statusType,
            targetAgent,
            input.from,
            input.to,
            `${observationId}:${input.herdrEventKey ?? "legacy"}:${statusType}`,
          ),
          paneId: targetAgent.paneId,
          paneGeneration: targetAgent.paneGeneration ?? null,
          payload: payload(targetAgent, input.from, input.to),
          terminalId: targetAgent.terminalId,
          type: statusType,
          workspaceId: targetAgent.workspaceId,
        });
      }
      return lastEvent;
    } finally {
      unregister();
    }
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
  _targetStatus: AgentStatus,
): string | undefined {
  const eventId =
    stringValue(event.id) ?? stringValue(event.event_id) ?? stringValue(event.eventId);
  if (!eventId) return undefined;
  return `${sessionName}:${paneId}:${String(event.type)}:${eventId}`;
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

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    let timer: NodeJS.Timeout | undefined;
    const onAbort = () => {
      if (timer !== undefined) clearTimeout(timer);
      resolve();
    };
    timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
