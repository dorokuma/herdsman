import { existsSync } from "node:fs";
import type { DatabaseSync } from "node:sqlite";
import { safeAllowedSessionPath, sessionPathAllowedByShape } from "@/agent-history/discovery.js";
import { type AgentHistoryService, createAgentHistoryService } from "@/agent-history/service.js";
import { type AgentEventStore, hasNonEmptyAssistantMessage } from "@/db/agent-events.js";
import type { AgentHistoryCacheStore } from "@/db/agent-history-cache.js";
import type { AgentOrchestratorScopeStore } from "@/db/agent-orchestrator-scopes.js";
import type { AgentStore, HerdrAgentLike } from "@/db/agents.js";
import { runSqliteTransaction } from "@/db/client.js";
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
  VALID_AGENT_EVENT_TYPES,
} from "@/observability/contracts.js";
import type { TurnCompletionRegistry } from "@/observability/turn-completion.js";
import { TURN_SIGNAL_WAIT_MS } from "@/observability/turn-completion.js";

export function historyHasAdvanced(
  current: CompactAgentHistory,
  baseline: CompactAgentHistory | null | undefined,
  options?: { requireAssistantChange?: boolean | undefined },
): boolean {
  if (
    current.lastAssistantMessage?.ref &&
    current.lastAssistantMessage.ref !== (baseline?.lastAssistantMessage?.ref ?? null)
  ) {
    return true;
  }
  if (
    current.lastAssistantMessage?.text &&
    current.lastAssistantMessage.text !== (baseline?.lastAssistantMessage?.text ?? null)
  ) {
    return true;
  }
  if (options?.requireAssistantChange) {
    return false;
  }
  return current.messageCount > (baseline?.messageCount ?? 0);
}

function isTerminalAssistant(compact: CompactAgentHistory | null | undefined): boolean {
  const stopReason = compact?.lastAssistantMessage?.stopReason;
  return stopReason === "stop" || stopReason === "length";
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
  sqlite: DatabaseSync;
  statusEventPlans: StatusEventPlanStore;
};

type RefreshInput = {
  herdrSessionName: string;
  sessionDir: string;
  socketPath: string;
};

export type StatusEventPlan = {
  agent: AgentIndexRecord;
  attempts?: number;
  compactHistory: CompactAgentHistory | undefined;
  from: AgentStatus;
  herdrEventKey?: string;
  planId?: number;
  to: AgentStatus;
};

export const PLAN_WAITING_HISTORY = "PLAN_WAITING_HISTORY";

export class PlanWaitingHistoryError extends Error {
  constructor() {
    super(PLAN_WAITING_HISTORY);
    this.name = "PlanWaitingHistoryError";
  }
}

/**
 * Invariant for the waiting-history retry ring: only a genuine
 * `PlanWaitingHistoryError` may (re)arm the 10s timer. It is the single signal
 * that the agent history legitimately has not advanced yet. Synthetic retry
 * reasons such as `degraded` are still recorded on the row, but they never
 * authorize another spin: the pending row is drained by the next refresh
 * instead, so a degraded round cannot keep the ring alive on its own.
 */
function retryRingAuthorized(error: unknown): error is PlanWaitingHistoryError {
  return error instanceof PlanWaitingHistoryError;
}

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
  readonly #clearRetry: (timer: unknown) => void;
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
  readonly #scheduleRetry: (callback: () => void, delayMs: number) => unknown;
  readonly #sessionOperationTail = new Map<string, Promise<void>>();
  readonly #shutdownSignal: AbortSignal | undefined;
  readonly #sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
  readonly #turnCompletions: TurnCompletionRegistry | undefined;
  readonly #waitingHistoryTimers = new Map<number, unknown>();
  readonly #now: () => number;
  readonly #onAgentEvent?: (event: AgentEventRecord) => void;

  constructor(options: {
    clearRetry?: (timer: unknown) => void;
    clientFactory?: (input: {
      socketPath: string;
    }) => Pick<HerdrSocketClient, "close" | "sessionSnapshot">;
    context?: AgentContextService;
    history?: AgentHistoryService;
    now?: () => number;
    scheduleRetry?: (callback: () => void, delayMs: number) => unknown;
    /**
     * Aborted when the daemon starts shutting down. In-flight status waits
     * (history-advance window, turn completion, readiness polling) are aborted
     * with it so draining plans cannot outlive the shutdown budget, and waits
     * registered after the abort return immediately instead of sleeping.
     */
    shutdownSignal?: AbortSignal;
    sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
    stores: AgentIndexServiceStores;
    turnCompletions?: TurnCompletionRegistry;
    onAgentEvent?: (event: AgentEventRecord) => void;
  }) {
    this.#clearRetry = options.clearRetry ?? ((timer) => clearTimeout(timer as NodeJS.Timeout));
    this.#clientFactory = options.clientFactory ?? ((input) => new HerdrSocketClient(input));
    this.#scheduleRetry = options.scheduleRetry ?? ((cb, delay) => setTimeout(cb, delay));
    this.#sleep = options.sleep ?? sleep;
    this.#shutdownSignal = options.shutdownSignal;
    this.#shutdownSignal?.addEventListener("abort", () => this.abortPendingWaits(), { once: true });
    this.#now = options.now ?? (() => Date.now());
    this.#stores = options.stores;
    this.#turnCompletions = options.turnCompletions;
    this.#onAgentEvent = options.onAgentEvent ?? (() => {});
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

  stopWaitingHistoryRetries(): void {
    for (const timer of this.#waitingHistoryTimers.values()) {
      this.#clearRetry(timer);
    }
    this.#waitingHistoryTimers.clear();
  }

  /**
   * Aborts every in-flight status wait (history advance, turn completion,
   * readiness polling). Called on shutdown through the shutdown signal; the
   * waiters treat an abort exactly like a closed pane, so no event is appended
   * for a half-observed transition and the plan row stays retryable.
   */
  abortPendingWaits(): void {
    for (const [key, controllers] of this.#activeWaiters) {
      for (const controller of controllers) {
        controller.abort();
      }
      controllers.clear();
      this.#activeWaiters.delete(key);
    }
  }

  async drainInFlightPlans(): Promise<void> {
    const tails = Array.from(this.#planTailByAgent.values());
    await Promise.allSettled(tails);
  }

  #clearWaitingTimer(planId: number): void {
    const existing = this.#waitingHistoryTimers.get(planId);
    if (existing !== undefined) {
      this.#clearRetry(existing);
      this.#waitingHistoryTimers.delete(planId);
    }
  }

  #scheduleWaitingHistoryRetry(planId: number, plan: StatusEventPlan): void {
    this.#clearWaitingTimer(planId);
    const timer = this.#scheduleRetry(async () => {
      try {
        const store = this.#stores.statusEventPlans;
        if (!store) return;
        const current = store.get(planId);
        if (current.status !== "pending") {
          this.#clearWaitingTimer(planId);
          return;
        }
        await this.#enqueueAgentPlan(plan.agent.id, () => this.#retryWaitingPlanRow(planId, plan));
      } catch (error) {
        // A failed retry callback used to be a silent `console.debug`; the row it
        // was supposed to advance can then sit in `pending` forever with no
        // signal at all, so the failure is reported as a warning with the row
        // identity attached.
        console.warn("Herdsman waiting history retry callback failed", {
          planId,
          agentId: plan.agent.id,
          herdrSessionName: plan.agent.herdrSessionName,
          paneId: plan.agent.paneId,
          error,
        });
      } finally {
        const store = this.#stores.statusEventPlans;
        if (store) {
          const current = store.get(planId);
          if (
            current.status === "completed" ||
            current.status === "cancelled" ||
            current.status === "failed" ||
            current.status === "discarded"
          ) {
            this.#clearWaitingTimer(planId);
          }
        }
      }
    }, 10_000);
    this.#waitingHistoryTimers.set(planId, timer);
  }

  async #retryWaitingPlanRow(planId: number, plan: StatusEventPlan): Promise<void> {
    const store = this.#stores.statusEventPlans;
    if (!store) return;
    let current: StatusEventPlanRecord;
    try {
      current = store.get(planId);
    } catch {
      this.#clearWaitingTimer(planId);
      return;
    }

    let retryPlan: StatusEventPlan;
    try {
      const refreshed = await this.#context.refreshAgent({
        agent: plan.agent,
        forceRefresh: true,
        identityChanged: false,
      });
      retryPlan = {
        ...plan,
        attempts: current.attempts,
        compactHistory: refreshed.snapshot.compactHistory,
      };
    } catch (err) {
      console.warn("Herdsman failed to refresh agent before retry plan row, keeping waiting", {
        planId,
        agentId: plan.agent.id,
        error: err,
      });
      const currentPlan = store.get(planId);
      const retryError =
        currentPlan.lastError === "degraded"
          ? new Error("degraded")
          : new PlanWaitingHistoryError();
      const updated = store.markRetry(planId, retryError);
      if (!updated) {
        this.#clearWaitingTimer(planId);
        return;
      }
      if (updated.status === "pending") {
        // Invariant: a synthetic retry reason never re-arms the ring (see
        // retryRingAuthorized); the pending row waits for the next refresh.
        if (retryRingAuthorized(retryError)) this.#scheduleWaitingHistoryRetry(planId, plan);
      } else if (updated.status === "discarded") {
        this.#clearWaitingTimer(planId);
        console.info("Herdsman status event plan discarded after max attempts", {
          planId,
          agentId: plan.agent.id,
          attempts: updated.attempts,
          from: plan.from,
          to: plan.to,
          herdrSessionName: plan.agent.herdrSessionName,
        });
        this.#appendPlanDiscardedEvent({
          agent: plan.agent,
          attempts: updated.attempts,
          compactHistory: plan.compactHistory,
          from: plan.from,
          planCreatedAt: currentPlan.createdAt.getTime(),
          planId,
          reason: updated.lastError,
          to: plan.to,
        });
      } else if (updated.status === "failed") {
        this.#clearWaitingTimer(planId);
        console.warn("Herdsman status event plan failed after max attempts", {
          planId,
          agentId: plan.agent.id,
          attempts: updated.attempts,
          from: plan.from,
          to: plan.to,
          herdrSessionName: plan.agent.herdrSessionName,
        });
        const failedEvent = this.#appendPlanFailedEvent({
          agent: plan.agent,
          attempts: updated.attempts,
          compactHistory: plan.compactHistory,
          from: plan.from,
          planCreatedAt: currentPlan.createdAt.getTime(),
          planId,
          reason: updated.lastError,
          to: plan.to,
        });
        if (VALID_AGENT_EVENT_TYPES.has(failedEvent.type)) {
          this.#onAgentEvent?.(failedEvent);
        }
      }
      return;
    }

    const event = await this.#runPlanRow(current, retryPlan);
    if (event && VALID_AGENT_EVENT_TYPES.has(event.type)) {
      this.#onAgentEvent?.(event);
    }
  }

  async executeStatusEventPlan(plan: StatusEventPlan): Promise<AgentEventRecord | undefined> {
    if (plan.from === plan.to) return undefined;
    if (!this.#stores.statusEventPlans) {
      return this.#enqueueAgentPlan(plan.agent.id, async () => {
        try {
          const event = await this.#appendStatusEvents(plan);
          return event === PLAN_CANCELLED ? undefined : event;
        } catch (error) {
          if (error instanceof PlanWaitingHistoryError) {
            return undefined;
          }
          throw error;
        }
      });
    }
    const inserted =
      plan.planId !== undefined
        ? this.#stores.statusEventPlans.get(plan.planId)
        : this.#stores.statusEventPlans.insertPending({
            agent: plan.agent,
            from: plan.from,
            herdrEventKey: plan.herdrEventKey ?? null,
            to: plan.to,
            ...(plan.compactHistory ? { compactHistory: plan.compactHistory } : {}),
          });
    const result = await this.#enqueueAgentPlan(plan.agent.id, () =>
      this.#runPlanRow(inserted, plan),
    );
    // Publication stays with the caller: every caller forwards this return value
    // (`HerdrSessionWatchManager#submitPlan` -> `publishAgentEvent`, or the
    // non-fast refresh/handle wrappers putting it into `events`). Pushing here as
    // well would write the same `agent.event` twice on the socket, breaking the
    // "exactly once" delivery contract. The retry-exhausted failed branch in
    // `#retryWaitingPlanRow` keeps its own push because no caller sees its result.
    return result;
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
    // rejects: every row is either drained, cancelled, or retried. A rejected
    // row is still an anomaly (its own drain path threw), so it is reported
    // instead of being silently folded into the settled results.
    const results = await Promise.allSettled(tasks);
    const rejected = results.flatMap((result) =>
      result.status === "rejected" ? [result.reason] : [],
    );
    if (rejected.length > 0) {
      console.error("Herdsman status event plan drain rejected rows", {
        rejected: rejected.length,
        total: results.length,
        errors: rejected.map((reason) =>
          reason instanceof Error ? reason.message : String(reason),
        ),
      });
    }
    await this.#backfillFailedPlanEvents();
    await this.#backfillDiscardedPlanEvents();
  }

  async #backfillFailedPlanEvents(): Promise<void> {
    const store = this.#stores.statusEventPlans;
    if (!store) return;
    let backfilled = 0;
    let skipped = 0;
    for (const row of store.listFailed()) {
      // Per-row isolation: one unresolvable or unwritable row must not abort the
      // whole backfill round. The legacy loop had no guard, so the first
      // `FOREIGN KEY constraint failed` threw out of `drainPendingPlans` and no
      // later row was ever attempted.
      try {
        if (row.lastError === PLAN_WAITING_HISTORY) {
          skipped += 1;
          continue;
        }
        const existing = this.#stores.sqlite
          .prepare(
            "select 1 from agent_events where herdr_session_name = ? and idempotency_key = ?",
          )
          .get(row.herdrSessionName, `agent.failed:plan:${row.id}`);
        if (existing) {
          skipped += 1;
          continue;
        }
        const agent = this.#resolveBackfillAgent(row);
        if (!agent) {
          skipped += 1;
          continue;
        }
        const event = this.#appendPlanFailedEvent({
          agent,
          attempts: row.attempts,
          compactHistory: row.compactHistory,
          from: row.fromStatus,
          planCreatedAt: row.createdAt.getTime(),
          planId: row.id,
          reason: row.lastError,
          to: row.toStatus,
        });
        // Stock terminal results are ingested silently: the row is written, then
        // immediately acknowledged/non-deliverable, so it stays queryable without
        // waking the current owner with a long-past failure.
        this.#stores.agentEvents.markAcked(event.id);
        backfilled += 1;
      } catch (error) {
        skipped += 1;
        console.warn("Herdsman skipping agent.failed backfill row", {
          agentId: row.agentId,
          herdrSessionName: row.herdrSessionName,
          paneId: row.paneId,
          planId: row.id,
          error: error instanceof Error ? error.message : String(error),
        });
      }
    }
    if (backfilled > 0 || skipped > 0) {
      console.info("Herdsman backfilled agent.failed rows", {
        backfilled,
        skipped,
        total: backfilled + skipped,
      });
    }
  }

  async #backfillDiscardedPlanEvents(): Promise<void> {
    const store = this.#stores.statusEventPlans;
    if (!store) return;
    let backfilled = 0;
    let skipped = 0;
    for (const row of store.listDiscarded()) {
      try {
        const existing = this.#stores.sqlite
          .prepare(
            "select 1 from agent_events where herdr_session_name = ? and idempotency_key = ?",
          )
          .get(row.herdrSessionName, `agent.discarded:plan:${row.id}`);
        if (existing) {
          skipped += 1;
          continue;
        }
        const agent = this.#resolveBackfillAgent(row);
        if (!agent) {
          skipped += 1;
          continue;
        }
        const event = this.#appendPlanDiscardedEvent({
          agent,
          attempts: row.attempts,
          compactHistory: row.compactHistory,
          from: row.fromStatus,
          planCreatedAt: row.createdAt.getTime(),
          planId: row.id,
          reason: row.lastError,
          to: row.toStatus,
        });
        // Same silent-ingest contract as the failed backfill.
        this.#stores.agentEvents.markAcked(event.id);
        backfilled += 1;
      } catch (error) {
        skipped += 1;
        console.warn("Herdsman skipping agent.discarded backfill row", {
          agentId: row.agentId,
          herdrSessionName: row.herdrSessionName,
          paneId: row.paneId,
          planId: row.id,
          error: error instanceof Error ? error.message : String(error),
        });
      }
    }
    if (backfilled > 0 || skipped > 0) {
      console.info("Herdsman backfilled agent.discarded rows", {
        backfilled,
        skipped,
        total: backfilled + skipped,
      });
    }
  }

  /**
   * Resolves the metadata a terminal plan outcome may use to reach an
   * orchestrator: the live `agents` row when it still exists, otherwise a
   * minimal record stitched from the pane's own event history.
   */
  #resolveBackfillAgent(row: StatusEventPlanRecord): AgentIndexRecord | undefined {
    try {
      return this.#stores.agents.get(row.agentId);
    } catch {
      const stitched = this.#minimalAgentFromLatestEvent({
        agentId: row.agentId,
        createdBefore: row.createdAt.getTime(),
        herdrSessionName: row.herdrSessionName,
        paneGeneration: row.paneGeneration,
        paneId: row.paneId,
      });
      if (!stitched) {
        console.warn("Herdsman skipping terminal event backfill; no agent row or pane events", {
          agentId: row.agentId,
          herdrSessionName: row.herdrSessionName,
          paneId: row.paneId,
          planId: row.id,
        });
      }
      return stitched;
    }
  }

  /**
   * Reconstructs the minimum agent metadata a terminal event needs from the
   * pane's own event history.
   *
   * The lookup is keyed by `(herdr_session_name, pane_id)` plus the plan's
   * `pane_generation`, and is bounded by `createdBefore` (the plan row's
   * `created_at`). Matching on `agent_id` alone cannot work here: physical
   * deletion of the `agents` row runs `ON DELETE SET NULL` over every event, so
   * the legacy stitch always came back empty, and a pane-id-only match would
   * happily stitch an old failure onto a newer instance of the same pane. The
   * generation and time bounds are what keep the two apart.
   */
  #minimalAgentFromLatestEvent(input: {
    agentId: string;
    createdBefore: number;
    herdrSessionName: string;
    paneGeneration: string | null;
    paneId: string;
  }): AgentIndexRecord | undefined {
    const generationClause =
      input.paneGeneration === null ? "pane_generation is null" : "pane_generation = ?";
    const params: Array<number | string> = [
      input.herdrSessionName,
      input.paneId,
      input.createdBefore,
    ];
    if (input.paneGeneration !== null) params.push(input.paneGeneration);
    const latest = this.#stores.sqlite
      .prepare(
        `select pane_id, pane_generation, terminal_id, workspace_id, herdr_session_name
         from agent_events
         where herdr_session_name = ? and pane_id = ? and created_at <= ? and ${generationClause}
         order by id desc limit 1`,
      )
      .get(...params) as
      | {
          herdr_session_name: string;
          pane_generation: string | null;
          pane_id: string | null;
          terminal_id: string | null;
          workspace_id: string | null;
        }
      | undefined;
    if (!latest?.pane_id || !latest.workspace_id) return undefined;
    return {
      agent: null,
      agentSession: null,
      agentStatus: "unknown",
      cwd: null,
      firstSeenAt: new Date(0),
      focused: false,
      foregroundCwd: null,
      herdrSessionName: latest.herdr_session_name || input.herdrSessionName,
      id: input.agentId,
      lastSeenAt: new Date(0),
      name: null,
      paneId: latest.pane_id,
      paneRevision: null,
      ...(latest.pane_generation === null ? {} : { paneGeneration: latest.pane_generation }),
      tabId: null,
      terminalId: latest.terminal_id,
      workspaceId: latest.workspace_id,
    };
  }

  async #drainPlanRow(row: StatusEventPlanRecord): Promise<void> {
    const store = this.#stores.statusEventPlans;
    if (!store) return;
    let current: StatusEventPlanRecord | null;
    try {
      current = runSqliteTransaction(this.#stores.sqlite, () => {
        const candidate = store.get(row.id);
        if (candidate.status !== "pending") return null;
        return candidate;
      });
    } catch {
      this.#clearWaitingTimer(row.id);
      return;
    }
    if (!current) {
      this.#clearWaitingTimer(row.id);
      return;
    }

    let agent: AgentIndexRecord | undefined;
    try {
      agent =
        this.#stores.agents.findByPane({
          herdrSessionName: current.herdrSessionName,
          paneId: current.paneId,
          paneGeneration: current.paneGeneration,
        }) ?? this.#stores.agents.get(current.agentId);
    } catch {
      agent = undefined;
    }
    if (!agent) {
      store.markCancelled(current.id);
      console.warn("Herdsman cancelling status event plan for missing agent", {
        agentId: current.agentId,
        herdrSessionName: current.herdrSessionName,
        paneId: current.paneId,
        from: current.fromStatus,
        to: current.toStatus,
      });
      return;
    }
    let activeCompact = current.compactHistory;
    try {
      const refreshed = await this.#context.refreshAgent({
        agent,
        forceRefresh: true,
        identityChanged: false,
      });
      activeCompact = refreshed.snapshot.compactHistory;
    } catch (err) {
      const retryError =
        current.lastError === "degraded" ? new Error("degraded") : new PlanWaitingHistoryError();
      const updated = store.markRetry(current.id, retryError);
      if (!updated) {
        this.#clearWaitingTimer(current.id);
        return;
      }
      if (updated.status === "pending") {
        console.warn("Herdsman failed to refresh agent during drain plan row, keeping waiting", {
          planId: current.id,
          agentId: agent.id,
          error: err,
        });
        // Invariant: the ring is only re-armed by a genuine
        // PlanWaitingHistoryError; a degraded row stays pending for the next
        // refresh to drain instead of spinning this callback again.
        if (retryRingAuthorized(retryError)) {
          this.#scheduleWaitingHistoryRetry(current.id, {
            agent,
            compactHistory: current.compactHistory,
            from: current.fromStatus,
            to: current.toStatus,
            ...(current.herdrEventKey ? { herdrEventKey: current.herdrEventKey } : {}),
          });
        }
      } else if (updated.status === "discarded") {
        this.#clearWaitingTimer(current.id);
        console.info("Herdsman plan marked discarded during drain", {
          planId: current.id,
          agentId: agent.id,
          from: current.fromStatus,
          to: current.toStatus,
          reason: updated.lastError,
        });
        this.#appendPlanDiscardedEvent({
          agent,
          attempts: updated.attempts,
          compactHistory: current.compactHistory,
          from: current.fromStatus,
          planCreatedAt: current.createdAt.getTime(),
          planId: current.id,
          reason: updated.lastError,
          to: current.toStatus,
        });
      } else if (updated.status === "failed") {
        this.#clearWaitingTimer(current.id);
        console.warn("Herdsman plan marked failed", {
          planId: current.id,
          agentId: agent.id,
          from: current.fromStatus,
          to: current.toStatus,
          reason: updated.lastError,
        });
        const failedEvent = this.#appendPlanFailedEvent({
          agent,
          attempts: updated.attempts,
          compactHistory: current.compactHistory,
          from: current.fromStatus,
          planCreatedAt: current.createdAt.getTime(),
          planId: current.id,
          reason: updated.lastError,
          to: current.toStatus,
        });
        if (VALID_AGENT_EVENT_TYPES.has(failedEvent.type)) {
          this.#onAgentEvent?.(failedEvent);
        }
      }
      return;
    }
    const plan: StatusEventPlan = {
      agent,
      attempts: current.attempts,
      compactHistory: activeCompact,
      from: current.fromStatus,
      to: current.toStatus,
      ...(current.herdrEventKey ? { herdrEventKey: current.herdrEventKey } : {}),
    };
    const event = await this.#runPlanRow(current, plan);
    if (event && VALID_AGENT_EVENT_TYPES.has(event.type)) {
      this.#onAgentEvent?.(event);
    }
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
      current.status === "failed" ||
      current.status === "discarded"
    ) {
      this.#clearWaitingTimer(row.id);
      return undefined;
    }
    if (!store.markRunning(row.id)) {
      this.#clearWaitingTimer(row.id);
      return undefined;
    }

    let activePlan: StatusEventPlan = {
      ...plan,
      attempts: row.attempts,
      // Row correlation for observability (see `#logDegradedRelease`): the plan
      // id is only known here, where the store row is in hand.
      planId: row.id,
    };
    if (row.attempts > 0) {
      try {
        const refreshed = await this.#context.refreshAgent({
          agent: plan.agent,
          forceRefresh: true,
          identityChanged: false,
        });
        activePlan = {
          ...plan,
          attempts: row.attempts,
          compactHistory: refreshed.snapshot.compactHistory,
          planId: row.id,
        };
      } catch (err) {
        console.warn("Herdsman failed to refresh agent before running retry row, keeping waiting", {
          planId: row.id,
          agentId: plan.agent.id,
          error: err,
        });
        // Keep the degraded reason when the plan was already degraded by an
        // earlier round: markRetry decides discarded (silent) vs failed (wakes)
        // from this message, so passing PLAN_WAITING_HISTORY here would let a
        // degraded plan exhaust silently.
        const retryError =
          row.lastError === "degraded" ? new Error("degraded") : new PlanWaitingHistoryError();
        const updated = store.markRetry(row.id, retryError);
        if (!updated) {
          this.#clearWaitingTimer(row.id);
          return undefined;
        }
        if (updated.status === "pending") {
          // Invariant: only a genuine PlanWaitingHistoryError authorizes the
          // ring; a degraded pre-refresh round records the synthetic reason and
          // waits for the next refresh instead.
          if (retryRingAuthorized(retryError)) this.#scheduleWaitingHistoryRetry(row.id, plan);
        } else {
          this.#clearWaitingTimer(row.id);
          if (updated.status === "discarded") {
            console.info("Herdsman status event plan discarded after max attempts", {
              planId: row.id,
              agentId: row.agentId,
              attempts: updated.attempts,
              from: row.fromStatus,
              to: row.toStatus,
              herdrSessionName: row.herdrSessionName,
            });
            return this.#appendPlanDiscardedEvent({
              agent: plan.agent,
              attempts: updated.attempts,
              compactHistory: plan.compactHistory,
              from: row.fromStatus,
              planCreatedAt: row.createdAt.getTime(),
              planId: row.id,
              reason: updated.lastError,
              to: row.toStatus,
            });
          }
          if (updated.status === "failed") {
            console.warn("Herdsman status event plan failed after max attempts", {
              planId: row.id,
              agentId: row.agentId,
              attempts: updated.attempts,
              from: row.fromStatus,
              to: row.toStatus,
              herdrSessionName: row.herdrSessionName,
            });
            return this.#appendPlanFailedEvent({
              agent: plan.agent,
              attempts: updated.attempts,
              compactHistory: plan.compactHistory,
              from: row.fromStatus,
              planCreatedAt: row.createdAt.getTime(),
              planId: row.id,
              reason: updated.lastError,
              to: row.toStatus,
            });
          }
        }
        return undefined;
      }
    }

    try {
      const event = await this.#appendStatusEvents(activePlan);
      if (event === PLAN_CANCELLED) {
        this.#clearWaitingTimer(row.id);
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
        this.#clearWaitingTimer(row.id);
        store.markCancelled(row.id);
        return undefined;
      }
      this.#clearWaitingTimer(row.id);
      const eventPayload = event?.payload;
      const isDegraded =
        !!event &&
        typeof eventPayload === "object" &&
        eventPayload !== null &&
        (eventPayload as Record<string, unknown>).degraded === true;
      if (isDegraded) {
        if (event) {
          this.#stores.agentEvents.invalidateById(event.id, "degraded_retry");
          Object.assign(event, { status: "invalidated", deliverable: 0 });
        }
        const degradedError = new Error("degraded");
        const updated = store.markRetry(row.id, degradedError);
        if (!updated) {
          return undefined;
        }
        if (updated.status === "pending") {
          // Invariant: `degraded` is a synthetic reason, so it never authorizes
          // the retry ring; the row stays pending and the next refresh
          // re-drains it.
          if (retryRingAuthorized(degradedError)) this.#scheduleWaitingHistoryRetry(row.id, plan);
          return undefined;
        } else {
          this.#clearWaitingTimer(row.id);
          if (updated.status === "discarded") {
            return this.#appendPlanDiscardedEvent({
              agent: plan.agent,
              attempts: updated.attempts,
              compactHistory: activePlan.compactHistory,
              from: row.fromStatus,
              planCreatedAt: row.createdAt.getTime(),
              planId: row.id,
              reason: updated.lastError,
              to: row.toStatus,
            });
          }
          if (updated.status === "failed") {
            return this.#appendPlanFailedEvent({
              agent: plan.agent,
              attempts: updated.attempts,
              compactHistory: activePlan.compactHistory,
              from: row.fromStatus,
              planCreatedAt: row.createdAt.getTime(),
              planId: row.id,
              reason: updated.lastError,
              to: row.toStatus,
            });
          }
        }
        return undefined;
      }
      store.markCompleted(row.id);
      return event;
    } catch (error) {
      if (error instanceof PlanWaitingHistoryError) {
        const retryError = row.lastError === "degraded" ? new Error("degraded") : error;
        const updated = store.markRetry(row.id, retryError);
        if (!updated) {
          this.#clearWaitingTimer(row.id);
          return undefined;
        }
        if (updated.status === "pending") {
          // Invariant: only a genuine PlanWaitingHistoryError authorizes the
          // ring. A row that already carries the synthetic `degraded` reason is
          // recorded as degraded again and waits for the next refresh instead of
          // re-arming the 10s timer, so this catch cannot be used as a back door
          // that lets a degraded round keep the ring alive.
          if (retryRingAuthorized(retryError)) this.#scheduleWaitingHistoryRetry(row.id, plan);
        } else {
          this.#clearWaitingTimer(row.id);
          if (updated.status === "discarded") {
            console.info("Herdsman status event plan discarded after max attempts", {
              planId: row.id,
              agentId: row.agentId,
              attempts: updated.attempts,
              from: row.fromStatus,
              to: row.toStatus,
              herdrSessionName: row.herdrSessionName,
            });
            return this.#appendPlanDiscardedEvent({
              agent: plan.agent,
              attempts: updated.attempts,
              compactHistory: activePlan.compactHistory,
              from: row.fromStatus,
              planCreatedAt: row.createdAt.getTime(),
              planId: row.id,
              reason: updated.lastError,
              to: row.toStatus,
            });
          }
          if (updated.status === "failed") {
            console.warn("Herdsman status event plan failed after max attempts", {
              planId: row.id,
              agentId: row.agentId,
              attempts: updated.attempts,
              from: row.fromStatus,
              to: row.toStatus,
              herdrSessionName: row.herdrSessionName,
            });
            return this.#appendPlanFailedEvent({
              agent: plan.agent,
              attempts: updated.attempts,
              compactHistory: activePlan.compactHistory,
              from: row.fromStatus,
              planCreatedAt: row.createdAt.getTime(),
              planId: row.id,
              reason: updated.lastError,
              to: row.toStatus,
            });
          }
        }
        return undefined;
      }
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
        this.#clearWaitingTimer(row.id);
        store.markCancelled(row.id);
        return undefined;
      }
      this.#clearWaitingTimer(row.id);
      const retryError = row.lastError === "degraded" ? new Error("degraded") : error;
      const updated = store.markRetry(row.id, retryError);
      if (!updated) {
        return undefined;
      }
      // Invariant: only a genuine PlanWaitingHistoryError re-arms the retry ring,
      // and that case was handled above. On this generic path `retryError` is
      // either the synthetic `degraded` reason or an unrelated failure, so no
      // spin is scheduled: a still-pending row is drained by the next refresh.
      if (updated.status === "discarded") {
        console.info("Herdsman status event plan discarded after max attempts", {
          planId: row.id,
          agentId: row.agentId,
          attempts: updated.attempts,
          from: row.fromStatus,
          to: row.toStatus,
          herdrSessionName: row.herdrSessionName,
        });
        return this.#appendPlanDiscardedEvent({
          agent: plan.agent,
          attempts: updated.attempts,
          compactHistory: activePlan.compactHistory,
          from: row.fromStatus,
          planCreatedAt: row.createdAt.getTime(),
          planId: row.id,
          reason: updated.lastError,
          to: row.toStatus,
        });
      }
      if (updated.status === "failed") {
        console.warn("Herdsman status event plan failed after max attempts", {
          planId: row.id,
          agentId: row.agentId,
          attempts: updated.attempts,
          from: row.fromStatus,
          to: row.toStatus,
          herdrSessionName: row.herdrSessionName,
        });
        return this.#appendPlanFailedEvent({
          agent: plan.agent,
          attempts: updated.attempts,
          compactHistory: activePlan.compactHistory,
          from: row.fromStatus,
          planCreatedAt: row.createdAt.getTime(),
          planId: row.id,
          reason: updated.lastError,
          to: row.toStatus,
        });
      }
      throw error;
    }
  }

  async #waitForHistoryAdvance(input: {
    agent: AgentIndexRecord;
    baseline: CompactAgentHistory | null | undefined;
    controller: AbortController;
    initial?: CompactAgentHistory;
    maxAttempts?: number;
    requireAssistantChange?: boolean;
  }): Promise<CompactAgentHistory | undefined> {
    const maxAttempts = input.maxAttempts ?? 8;
    // Fixed 200ms interval, no exponential backoff: the history cache either
    // advances shortly after the turn signal or it does not, and a doubling
    // delay only pushed the first deliverable terminal event tens of seconds
    // away. The whole window is hard bounded by maxTotalMs.
    const baseDelay = 200;
    const maxTotalMs = 1500;
    let refreshed: { snapshot: { compactHistory: CompactAgentHistory } } = input.initial
      ? { snapshot: { compactHistory: input.initial } }
      : await this.#context.refreshAgent({
          agent: input.agent,
          forceRefresh: true,
          identityChanged: false,
        });
    const start = this.#now();
    for (
      let attempt = 0;
      attempt < maxAttempts &&
      !historyHasAdvanced(refreshed.snapshot.compactHistory, input.baseline, {
        requireAssistantChange: input.requireAssistantChange,
      }) &&
      !input.controller.signal.aborted &&
      this.#now() - start < maxTotalMs;
      attempt += 1
    ) {
      // Clamp the last step to the remaining budget so the total wait never
      // exceeds maxTotalMs even though the loop condition is checked before
      // sleeping.
      const remaining = maxTotalMs - (this.#now() - start);
      if (remaining <= 0) break;
      await this.#sleep(Math.min(baseDelay, remaining), input.controller.signal);
      if (input.controller.signal.aborted) {
        console.debug(
          "Herdsman retrying status event plan because wait was aborted during history refresh",
          {
            agentId: input.agent.id,
            herdrSessionName: input.agent.herdrSessionName,
            paneId: input.agent.paneId,
          },
        );
        throw new PlanWaitingHistoryError();
      }
      if (
        this.#stores.agents.isPaneClosed({
          herdrSessionName: input.agent.herdrSessionName,
          paneId: input.agent.paneId,
          paneGeneration: input.agent.paneGeneration ?? null,
        })
      ) {
        console.debug(
          "Herdsman skipping status event generation because pane closed during history refresh",
          {
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
      const overlayByPane = new Map<string, PaneOverlay>();
      for (const pane of snapshot.panes) {
        const value = record(pane);
        const paneId = stringValue(value.pane_id) ?? stringValue(value.paneId);
        if (!paneId) continue;
        const revision = integerValue(value.revision);
        const terminalTitle = stringValue(value.terminal_title) ?? stringValue(value.terminalTitle);
        if (revision === undefined && !terminalTitle) continue;
        overlayByPane.set(paneId, {
          ...(revision === undefined ? {} : { revision }),
          ...(terminalTitle ? { terminalTitle } : {}),
        });
      }
      const snapshotAgents = snapshot.agents.map((agent) => withPaneRevision(agent, overlayByPane));
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
        const occupiedByOthers = this.#context.occupiedSessionPathsFor(agent);
        const occupancyConflict = Boolean(
          cached?.historyRef?.path && occupiedByOthers.has(cached.historyRef.path),
        );
        const sessionReady =
          agent.agentSession?.kind === "path" &&
          safeAllowedSessionPath(agent.agentSession.value) !== null;
        const sessionUnbound =
          sessionReady &&
          (cached?.historyRef?.kind !== "agent_session" ||
            cached.historyRef.path !== agent.agentSession?.value);
        const dirty =
          !cached ||
          agent.paneRevision === null ||
          cached.paneRevision !== agent.paneRevision ||
          identityChanged ||
          cached.historyRef == null ||
          cached.historyRef.kind === "discovered_file" ||
          occupancyConflict ||
          sessionUnbound;
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
    const current = { ...agent, agentStatus: to };
    const refreshed = await this.#context.refreshAgent({ agent: current, identityChanged: false });
    const scopes = new Map<string, AgentScope>();
    for (const scope of recovered?.contextChangedScopes ?? []) addScope(scopes, scope);
    if (refreshed.changed || from !== to) addScope(scopes, scopeOf(current));
    const events = [...(recovered?.events ?? [])];
    const statusEventPlans = [...(recovered?.statusEventPlans ?? [])];
    const equivalent = statusEventPlans.some(
      (candidate) => candidate.agent.id === current.id && candidate.to === to,
    );
    const statusInput = {
      agentStatus: to,
      herdrSessionName: input.herdrSessionName,
      paneId,
      paneGeneration: eventGeneration,
    };
    if (from === to) {
      this.#stores.agents.updateStatus(statusInput);
    } else if (!equivalent) {
      const inserted = runSqliteTransaction(this.#stores.sqlite, () => {
        const updated = this.#stores.agents.updateStatus(statusInput) ?? current;
        const plan = this.#stores.statusEventPlans.insertPending({
          agent: updated,
          from,
          herdrEventKey: herdrEventKey ?? null,
          to,
          ...(refreshed.snapshot.compactHistory
            ? { compactHistory: refreshed.snapshot.compactHistory }
            : {}),
        });
        return { plan, updated };
      });
      statusEventPlans.push({
        agent: inserted.updated,
        compactHistory: refreshed.snapshot.compactHistory,
        from,
        ...(herdrEventKey ? { herdrEventKey } : {}),
        planId: inserted.plan.id,
        to,
      });
    } else {
      this.#stores.agents.updateStatus(statusInput);
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
      if (
        existsSync(input.sessionRef.value) ||
        !sessionPathAllowedByShape(input.sessionRef.value)
      ) {
        return { agent: undefined, contextChangedScopes: [] };
      }
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

  /**
   * Distinguishes the two reasons an in-flight wait can be aborted. A closed pane
   * makes the transition moot, so the plan is skipped; a daemon shutdown leaves
   * the pane alive, so the plan must stay retryable instead of being completed
   * without ever emitting the transition (it is drained on the next start).
   */
  #throwIfShutdownAbort(controller: AbortController): void {
    if (controller.signal.aborted && this.#shutdownSignal?.aborted) {
      throw new PlanWaitingHistoryError();
    }
  }

  #registerActiveWaiter(agent: AgentIndexRecord, controller: AbortController): () => void {
    const key = `${agent.herdrSessionName}\0${agent.paneId}\0${agent.paneGeneration ?? ""}`;
    // A wait that starts after shutdown already aborted must not sleep at all:
    // abort it up front so the caller short-circuits like a closed pane.
    if (this.#shutdownSignal?.aborted) controller.abort();
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

  /**
   * Observability for a degraded release. The emitted status event carries the
   * specific `degradedReason`, but the plan row itself only stores the synthetic
   * `degraded` marker (`row.lastError`), so without this line the cause is
   * invisible in the daemon log. It cannot ride along in the failed-event payload
   * either: for most append sites only `row.lastError` is at hand, so handing them
   * the specific reason would mean persisting a new plan field (schema change).
   * That is not universal — at least three call sites in this file already have the
   * specific reason in their own call stack (`:1074` the status event payload's
   * `degradedReason`, `:1136` the `PlanWaitingHistoryError`, `:1203` the generic
   * `Error`), so routing the reason into the payload does not necessarily require a
   * schema change. Log-only on purpose — no behaviour change (see
   * .agents/notes/20260930-terminal-event-delivery-open-items.md A1).
   *
   * `plan.planId` is the row correlation that already exists on
   * `StatusEventPlan` (see `executeStatusEventPlan`); it is `null` for the
   * row-less in-memory plans executed without the SQLite store.
   */
  #logDegradedRelease(plan: StatusEventPlan, degradedReason: string): void {
    console.warn("Herdsman emitted degraded status event", {
      agentId: plan.agent.id,
      attempts: plan.attempts ?? 0,
      degradedReason,
      herdrSessionName: plan.agent.herdrSessionName,
      paneId: plan.agent.paneId,
      planId: plan.planId ?? null,
    });
  }

  async #appendStatusEvents(
    input: StatusEventPlan,
  ): Promise<AgentEventRecord | undefined | typeof PLAN_CANCELLED> {
    // Invariant: compactHistory is always populated as an object before calling #appendStatusEvents; !input.compactHistory check below is defensive and unreachable in normal operation.
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
    let skipStatusChanged = false;
    if (latest && statusTransitionMatches(latest, input.from, input.to)) {
      const hasTerminalAfter = this.#stores.agentEvents.hasTerminalEventAfter(
        input.agent.id,
        input.agent.herdrSessionName,
        latest.id,
      );
      if (!hasTerminalAfter) {
        if (statusEventType(input.to) === undefined) {
          console.debug("Herdsman skipping duplicate status transition event", {
            agentId: input.agent.id,
            from: input.from,
            to: input.to,
          });
          return undefined;
        }
        skipStatusChanged = true;
      }
      if (hasTerminalAfter && !input.herdrEventKey) {
        const isAgyHistory =
          input.agent.agent === "agy" || input.compactHistory?.source === "antigravity-sqlite";
        // For an agy/antigravity pane only a terminal event that already
        // represented a completed turn may count as the delivered baseline: an
        // `unknown -> idle` startup row would otherwise make the `working -> done`
        // row of the same round look like a duplicate.
        const lastTerminal = isAgyHistory
          ? this.#stores.agentEvents.latestCompletedTurnEvent(
              input.agent.id,
              input.agent.herdrSessionName,
            )
          : this.#stores.agentEvents.latestTerminalEvent(
              input.agent.id,
              input.agent.herdrSessionName,
            );
        if (
          sameTerminalAssistantContent(
            input.compactHistory,
            lastTerminal?.compactHistory,
            input.agent.agent,
          )
        ) {
          // An agy round whose ref already reached a completed turn is settled: a
          // retry must skip it instead of exhausting the budget into `discarded`.
          if ((input.attempts ?? 0) > 0 && !isAgyHistory) {
            throw new PlanWaitingHistoryError();
          }
          console.debug(
            "Herdsman skipping legacy duplicate status transition event with identical history ref",
            {
              agentId: input.agent.id,
              from: input.from,
              to: input.to,
            },
          );
          return undefined;
        }
      }
    }
    const observationId = `transition:${latest?.id ?? 0}`;

    const controller = new AbortController();
    const unregister = this.#registerActiveWaiter(input.agent, controller);

    try {
      let compactHistory = input.compactHistory;
      let payloadExtra: Record<string, unknown> = {};
      if (
        this.#turnCompletions !== undefined &&
        (input.to === "done" || input.to === "blocked") &&
        input.agent.terminalId !== null &&
        input.agent.agent === "pi"
      ) {
        const isRetry = (input.attempts ?? 0) > 0;
        const latestTerminal = this.#stores.agentEvents.latestTerminalEvent(
          input.agent.id,
          input.agent.herdrSessionName,
        );
        const baseline =
          isRetry && latestTerminal?.compactHistory
            ? latestTerminal.compactHistory
            : input.compactHistory;

        const isAlreadyEmittedInRetry =
          isRetry &&
          latestTerminal?.compactHistory &&
          sameTerminalAssistantContent(input.compactHistory, latestTerminal.compactHistory, "pi");

        const turn = await this.#turnCompletions.waitForSignal({
          herdrSessionName: input.agent.herdrSessionName,
          recordedAfterMs: Date.now() - TURN_SIGNAL_WAIT_MS,
          signal: controller.signal,
          terminalId: input.agent.terminalId,
        });

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
          this.#throwIfShutdownAbort(controller);
          return undefined;
        }

        if (turn?.received) {
          const fresh = (
            await this.#context.refreshAgent({
              agent: input.agent,
              forceRefresh: true,
              identityChanged: false,
            })
          ).snapshot.compactHistory;
          if (
            this.#stores.agents.isPaneClosed({
              herdrSessionName: input.agent.herdrSessionName,
              paneId: input.agent.paneId,
              paneGeneration: input.agent.paneGeneration ?? null,
            })
          ) {
            console.debug(
              "Herdsman skipping status event generation because pane closed after turn signal",
              {
                agentId: input.agent.id,
                herdrSessionName: input.agent.herdrSessionName,
                paneId: input.agent.paneId,
              },
            );
            return undefined;
          }
          const freshIsTerminal = isTerminalAssistant(fresh);
          const expectedText = turn.expectedText?.trim();
          const lastAssistantText = fresh.lastAssistantMessage?.text?.trim() ?? "";
          const textMatches = !expectedText || lastAssistantText.endsWith(expectedText);
          // A client-confirmed turn is a deliverable terminal state: `confirmed`
          // means Pi verified the final assistant message is on disk, so a
          // confirmed round is delivered straight through and is never marked
          // degraded (which is what made #runPlanRow run
          // invalidateById(..., "degraded_retry") on already-written content).
          const confirmedTerminal = turn.confirmed === true;
          const degradeOrRelease = (degradedReason: string): Record<string, unknown> => {
            if (confirmedTerminal) return { staleSnapshot: false };
            this.#logDegradedRelease(input, degradedReason);
            return { degraded: true, degradedReason, staleSnapshot: false };
          };
          if (
            confirmedTerminal &&
            freshIsTerminal &&
            textMatches &&
            hasNonEmptyAssistantMessage(fresh)
          ) {
            compactHistory = fresh;
          } else if (
            !isAlreadyEmittedInRetry &&
            freshIsTerminal &&
            textMatches &&
            hasNonEmptyAssistantMessage(fresh) &&
            (isTerminalAssistant(input.compactHistory) ||
              historyHasAdvanced(fresh, input.compactHistory))
          ) {
            compactHistory = fresh;
          } else {
            const advanced = await this.#waitForHistoryAdvance({
              agent: input.agent,
              baseline,
              controller,
              initial: fresh,
              maxAttempts: 8,
              requireAssistantChange: isRetry,
            });
            if (advanced === undefined) return undefined;
            if (
              isRetry &&
              !historyHasAdvanced(advanced, baseline, { requireAssistantChange: true })
            ) {
              throw new PlanWaitingHistoryError();
            }
            if (
              !isRetry &&
              !historyHasAdvanced(advanced, input.compactHistory, { requireAssistantChange: true })
            ) {
              compactHistory = { ...advanced, lastAssistantMessage: null };
              payloadExtra = degradeOrRelease("no_advance_from_input");
            } else {
              const advancedText = advanced.lastAssistantMessage?.text?.trim() ?? "";
              const advancedMatchesExpected = !expectedText || advancedText.endsWith(expectedText);
              if (!advancedMatchesExpected) {
                compactHistory = { ...advanced, lastAssistantMessage: null };
                payloadExtra = degradeOrRelease("expected_text_mismatch");
              } else if (!isTerminalAssistant(advanced) || !hasNonEmptyAssistantMessage(advanced)) {
                compactHistory = { ...advanced, lastAssistantMessage: null };
                payloadExtra = degradeOrRelease("non_terminal_assistant");
              } else {
                compactHistory = advanced;
              }
            }
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
            baseline,
            controller,
            maxAttempts: 8,
            requireAssistantChange: isRetry,
          });
          if (advanced === undefined) return undefined;
          if (
            isRetry &&
            !historyHasAdvanced(advanced, baseline, { requireAssistantChange: true })
          ) {
            throw new PlanWaitingHistoryError();
          }
          if (
            !isRetry &&
            !historyHasAdvanced(advanced, input.compactHistory, { requireAssistantChange: true })
          ) {
            compactHistory = { ...advanced, lastAssistantMessage: null };
            this.#logDegradedRelease(input, "no_advance_from_input");
            payloadExtra = {
              degraded: true,
              degradedReason: "no_advance_from_input",
              staleSnapshot: false,
            };
          } else if (!isTerminalAssistant(advanced) || !hasNonEmptyAssistantMessage(advanced)) {
            compactHistory = { ...advanced, lastAssistantMessage: null };
            this.#logDegradedRelease(input, "non_terminal_assistant");
            payloadExtra = {
              degraded: true,
              degradedReason: "non_terminal_assistant",
              staleSnapshot: false,
            };
          } else {
            compactHistory = advanced;
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
      } else if (input.agent.agent !== "pi" && (input.to === "idle" || input.to === "done")) {
        // Note: blocked is an interactive intermediate state, exempt from ready gate and empty delivery gate.
        const isAgyHistory = (compact: CompactAgentHistory | undefined): boolean =>
          compact?.source === "antigravity-sqlite" || input.agent.agent === "agy";
        // Only a terminal event that already represented a completed turn may
        // consume the assistant ref: a late startup idle (`unknown -> idle`),
        // an `agent.status.changed` row or a discarded plan must not make the
        // following `working -> done` plan look like a duplicate round.
        const latestCompletedTurnRef = (): string | null =>
          this.#stores.agentEvents.latestCompletedTurnEvent(
            input.agent.id,
            input.agent.herdrSessionName,
          )?.compactHistory?.lastAssistantMessage?.ref ?? null;
        const isReadyNonPi = (compact: CompactAgentHistory | undefined): boolean => {
          if (!compact || !hasNonEmptyAssistantMessage(compact)) return false;
          if (isAgyHistory(compact)) {
            const prevRef = latestCompletedTurnRef();
            const currentRef = compact.lastAssistantMessage?.ref ?? null;
            return currentRef !== null && currentRef !== prevRef;
          }
          const latestTerminal = this.#stores.agentEvents.latestTerminalEvent(
            input.agent.id,
            input.agent.herdrSessionName,
          );
          if (!latestTerminal) return true;
          const currentMsg = compact.lastAssistantMessage;
          const prevMsg = latestTerminal.compactHistory?.lastAssistantMessage;
          const refChanged = (currentMsg?.ref ?? null) !== (prevMsg?.ref ?? null);
          const textChanged = (currentMsg?.text ?? "") !== (prevMsg?.text ?? "");
          const same = sameTerminalAssistantContent(
            compact,
            latestTerminal.compactHistory,
            input.agent.agent,
          );
          return !same || refChanged || textChanged;
        };
        // A non-empty assistant ref that was already delivered by a completed
        // turn is a genuine skip (the round was announced), never a reason to
        // exhaust the retry budget into `discarded`.
        const isAlreadyDeliveredCompletedTurn = (
          compact: CompactAgentHistory | undefined,
        ): boolean => {
          if (!compact || !isAgyHistory(compact) || !hasNonEmptyAssistantMessage(compact)) {
            return false;
          }
          const currentRef = compact.lastAssistantMessage?.ref ?? null;
          return currentRef !== null && currentRef === latestCompletedTurnRef();
        };

        const latestTerminalForSkip = this.#stores.agentEvents.latestTerminalEvent(
          input.agent.id,
          input.agent.herdrSessionName,
        );
        if (
          input.agent.agent !== "agy" &&
          compactHistory?.source !== "antigravity-sqlite" &&
          input.herdrEventKey &&
          latestTerminalForSkip &&
          compactHistory &&
          hasNonEmptyAssistantMessage(compactHistory)
        ) {
          const currentMsg = compactHistory.lastAssistantMessage;
          const prevMsg = latestTerminalForSkip.compactHistory?.lastAssistantMessage;
          const refChanged = (currentMsg?.ref ?? null) !== (prevMsg?.ref ?? null);
          const textChanged = (currentMsg?.text ?? "") !== (prevMsg?.text ?? "");
          const same = sameTerminalAssistantContent(
            compactHistory,
            latestTerminalForSkip.compactHistory,
            input.agent.agent,
          );
          if (same && !refChanged && !textChanged) {
            return undefined;
          }
        }

        if (!isReadyNonPi(compactHistory)) {
          for (let attempt = 0; attempt < 8; attempt += 1) {
            await this.#sleep(500, controller.signal);
            if (
              controller.signal.aborted ||
              this.#stores.agents.isPaneClosed({
                herdrSessionName: input.agent.herdrSessionName,
                paneId: input.agent.paneId,
                paneGeneration: input.agent.paneGeneration ?? null,
              })
            ) {
              console.debug(
                "Herdsman skipping status event generation because wait was aborted or pane closed",
                {
                  aborted: controller.signal.aborted,
                  agentId: input.agent.id,
                  herdrSessionName: input.agent.herdrSessionName,
                  paneId: input.agent.paneId,
                },
              );
              this.#throwIfShutdownAbort(controller);
              return undefined;
            }
            try {
              const refreshed = await this.#context.refreshAgent({
                agent: input.agent,
                forceRefresh: true,
                identityChanged: false,
              });
              compactHistory = refreshed.snapshot.compactHistory;
            } catch (err) {
              console.debug("Herdsman failed to refresh agent during ready wait", err);
            }
            if (isReadyNonPi(compactHistory)) {
              break;
            }
          }
        }

        if (!isReadyNonPi(compactHistory)) {
          if (isAlreadyDeliveredCompletedTurn(compactHistory)) {
            console.debug(
              "Herdsman skipping non-pi terminal status event because the assistant ref was already delivered by a completed turn",
              {
                agent: input.agent.agent,
                agentId: input.agent.id,
                from: input.from,
                ref: compactHistory?.lastAssistantMessage?.ref ?? null,
                to: input.to,
              },
            );
            return undefined;
          }
          throw new PlanWaitingHistoryError();
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
        this.#throwIfShutdownAbort(controller);
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
        if (
          input.to === "done" ||
          input.to === "blocked" ||
          (input.to === "idle" && currentAgent.agent === "agy")
        ) {
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

      const eventPayload = { ...payload(targetAgent, input.from, input.to), ...payloadExtra };
      const lastEvent = skipStatusChanged
        ? undefined
        : this.#appendAndAckSelfEvent({
            agentId: targetAgent.id,
            compactHistory: compactHistory ?? null,
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
            payload: eventPayload,
            terminalId: targetAgent.terminalId,
            type: "agent.status.changed",
            workspaceId: targetAgent.workspaceId,
          });
      const statusType = statusEventType(input.to);
      if (statusType) {
        return this.#appendAndAckSelfEvent({
          agentId: targetAgent.id,
          compactHistory: compactHistory ?? null,
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
          payload: eventPayload,
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

  /**
   * Resolves the metadata a terminal plan outcome is attributed to and whether
   * the event may keep its `agent_id` foreign key.
   *
   * A live `agents` row wins because it carries the current pane generation.
   * Once the row is gone (pane retired / physically deleted) the plan's own
   * pane/session/generation are the authority, refined by the newest event of
   * the same pane + generation that predates the plan, so an old failure is
   * never stitched onto a newer instance of the same pane. The event is still
   * written in that case, but as an orphan (`agentRowPresent: false`).
   */
  #resolvePlanOutcomeAgent(
    agent: AgentIndexRecord,
    planCreatedAt: number | undefined,
  ): { agent: AgentIndexRecord; agentRowPresent: boolean } {
    try {
      return { agent: this.#stores.agents.get(agent.id), agentRowPresent: true };
    } catch {
      const stitched = this.#minimalAgentFromLatestEvent({
        agentId: agent.id,
        // The plan row's own creation time is the upper bound: anything newer on
        // the same pane belongs to a later instance.
        createdBefore: planCreatedAt ?? Date.now(),
        herdrSessionName: agent.herdrSessionName,
        paneGeneration: agent.paneGeneration ?? null,
        paneId: agent.paneId,
      });
      return { agent: stitched ?? agent, agentRowPresent: false };
    }
  }

  #appendPlanFailedEvent(input: {
    agent: AgentIndexRecord;
    attempts: number;
    compactHistory: CompactAgentHistory | null | undefined;
    fallbackOutcome?: boolean;
    from: AgentStatus;
    planCreatedAt?: number;
    planId: number;
    reason: string | null;
    to: AgentStatus;
  }): AgentEventRecord {
    const originalAgentId = input.agent.id;
    const resolved = this.#resolvePlanOutcomeAgent(input.agent, input.planCreatedAt);
    const agent = resolved.agent;
    const fallbackOutcome =
      input.fallbackOutcome ??
      ((input.to === "done" || input.to === "blocked") && input.reason === "degraded");
    return this.#appendAndAckSelfEvent({
      agentId: resolved.agentRowPresent ? agent.id : null,
      compactHistory: input.compactHistory ?? null,
      herdrSessionName: agent.herdrSessionName,
      idempotencyKey: `agent.failed:plan:${input.planId}`,
      paneId: agent.paneId,
      paneGeneration: agent.paneGeneration ?? null,
      payload: {
        agent: agent.agent,
        // The original id survives in the payload even when the row had to be
        // written as an orphan, so the failure stays traceable to its agent.
        agentId: originalAgentId,
        attempts: input.attempts,
        fallbackOutcome,
        from: input.from,
        herdrSessionName: agent.herdrSessionName,
        name: agent.name,
        paneId: agent.paneId,
        reason: input.reason ?? "unknown",
        terminalId: agent.terminalId,
        to: input.to,
        workspaceId: agent.workspaceId,
      },
      terminalId: agent.terminalId,
      type: "agent.failed",
      workspaceId: agent.workspaceId,
    });
  }

  #appendPlanDiscardedEvent(input: {
    agent: AgentIndexRecord;
    attempts: number;
    compactHistory: CompactAgentHistory | null | undefined;
    from: AgentStatus;
    planCreatedAt?: number;
    planId: number;
    reason: string | null;
    to: AgentStatus;
  }): AgentEventRecord {
    const originalAgentId = input.agent.id;
    const resolved = this.#resolvePlanOutcomeAgent(input.agent, input.planCreatedAt);
    const agent = resolved.agent;
    return this.#appendAndAckSelfEvent({
      agentId: resolved.agentRowPresent ? agent.id : null,
      compactHistory: input.compactHistory ?? null,
      herdrSessionName: agent.herdrSessionName,
      idempotencyKey: `agent.discarded:plan:${input.planId}`,
      paneId: agent.paneId,
      paneGeneration: agent.paneGeneration ?? null,
      payload: {
        agent: agent.agent,
        agentId: originalAgentId,
        attempts: input.attempts,
        from: input.from,
        herdrSessionName: agent.herdrSessionName,
        name: agent.name,
        paneId: agent.paneId,
        planId: input.planId,
        reason: input.reason ?? "unknown",
        terminalId: agent.terminalId,
        to: input.to,
        workspaceId: agent.workspaceId,
      },
      terminalId: agent.terminalId,
      type: "agent.discarded",
      workspaceId: agent.workspaceId,
    });
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

type PaneOverlay = {
  revision?: number;
  terminalTitle?: string;
};

function withPaneRevision(agent: unknown, overlayByPane: Map<string, PaneOverlay>): HerdrAgentLike {
  const raw = record(agent);
  const paneId = stringValue(raw.pane_id) ?? stringValue(raw.paneId);
  const overlay = paneId ? overlayByPane.get(paneId) : undefined;
  const revision = integerValue(raw.revision) ?? overlay?.revision;
  const terminalTitle =
    stringValue(raw.terminal_title) ?? stringValue(raw.terminalTitle) ?? overlay?.terminalTitle;
  if (revision === undefined && !terminalTitle) return raw;
  // Collapse dual keys onto herdr-canonical snake_case `terminal_title`.
  // Incoming snapshots may carry camelCase `terminalTitle`; spreading `raw`
  // would otherwise leave both keys on the overlay object. Downstream
  // HerdrAgentLike readers (AgentStore.replaceForSession) still accept
  // camelCase as fallback, and AgentIndexRecord.terminalTitle is populated
  // later from whichever key is present.
  const rest = { ...raw };
  delete rest.terminalTitle;
  return {
    ...rest,
    ...(revision === undefined ? {} : { revision }),
    ...(terminalTitle ? { terminal_title: terminalTitle } : {}),
  };
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

function sameTerminalAssistantContent(
  current: CompactAgentHistory | undefined | null,
  previous: CompactAgentHistory | undefined | null,
  agentKind: string | null | undefined,
): boolean {
  const currentMsg = current?.lastAssistantMessage;
  const prevMsg = previous?.lastAssistantMessage;
  if (!currentMsg || !prevMsg) return false;

  if (agentKind === "agy" || current?.source === "antigravity-sqlite") {
    if (currentMsg.ref !== null && prevMsg.ref !== null) {
      return currentMsg.ref === prevMsg.ref;
    }
    return Boolean(currentMsg.text && currentMsg.text === prevMsg.text);
  }

  // Non-agy (pi, claude, codex, grok, opencode):
  if (currentMsg.ref !== null && prevMsg.ref !== null) {
    return currentMsg.ref === prevMsg.ref;
  }
  if (currentMsg.text.length > 0 && prevMsg.text.length > 0) {
    return currentMsg.text === prevMsg.text;
  }
  return false;
}
