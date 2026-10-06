import { existsSync } from "node:fs";
import type { DatabaseSync } from "node:sqlite";
import {
  safeAllowedSessionPath,
  safeOfficialSessionPath,
  sessionPathAllowedByShape,
} from "@/agent-history/discovery.js";
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

/**
 * How many prior terminal rows the stale-baseline duplicate guard compares
 * against, newest first. The guard used to compare only the single latest
 * terminal row; M2 widens the accepted body set to unconfirmed rounds, so a body
 * that was already delivered as an earlier terminal row must be recognised too.
 * The window keeps that comparison bounded instead of scanning every terminal row
 * the agent ever emitted (those live up to the 7-day settled TTL).
 *
 * Why 200: within that 7-day TTL a single agent's delivered/acked terminal rows
 * in production top out around 250, and the body the guard cares about is always
 * one of the most recent deliveries, so 200 covers it with margin while keeping
 * the read to a few hundred KB of JSON.
 *
 * Above the window the guard stops recognising a duplicate, so an older body that
 * resurfaces at the tail could be re-released as this round's answer once more.
 * That is the accepted cost of bounding the scan instead of walking every row.
 */
const STALE_DUPLICATE_GUARD_SCAN_LIMIT = 200;

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

/**
 * `reason` carried by the orphan terminal row written when a plan still held
 * deliverable terminal content but its `agents` row was already gone (the
 * orchestrator closed the pane while the plan was pending). It names the shape,
 * not a failure of the round: the body it carries is the round's answer.
 */
const ORPHAN_SALVAGE_REASON = "pane_closed_before_delivery";

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
    const store = this.#stores.statusEventPlans;
    if (!store) return;
    store.resetRunningToPending();
    const rows = store.listUnfinished();
    const tasks = rows.map((row) => {
      try {
        return this.#enqueueAgentPlan(row.agentId, () => this.#drainPlanRow(row));
      } catch (_error) {
        // A row whose agent cannot even be resolved must never take down the
        // daemon boot drain (that failure mode used to boot-loop systemd). Its
        // terminal content is salvaged first: a pending plan whose pane was
        // closed still holds this round's answer.
        const salvaged = this.#salvageOrCancelMissingAgentPlan(store, row);
        if (salvaged) this.#publishSalvagedEvent(salvaged);
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

  /**
   * Terminal content a plan whose agent can no longer be resolved may still
   * hand to the orchestrator, or null when there is nothing worth salvaging.
   *
   * Two sources, in order:
   *  1. the plan row's own baseline snapshot (`compact_history_json`, written once
   *     at plan creation and never updated). In the frozen-baseline shape that
   *     snapshot *is* this round's answer — it is the session tail as of plan
   *     creation — which is exactly the content a `no_advance_from_input` degrade
   *     blanks and the replacement row then never gets to deliver;
   *  2. the pane's newest recorded tail that never reached the orchestrator.
   *     Re-reading the session file is not possible here: history discovery needs
   *     the `agent_session` that lived on the now-deleted `agents` row, and
   *     herdsman deliberately does not guess a session path
   *     (`src/agent-history/discovery.ts`), so the pane's own event history is the
   *     only remaining record of that tail. A row the orchestrator already
   *     consumed (`delivery_attempts > 0`) is skipped, and so is a body that
   *     matches one it already consumed: those are not lost, so re-sending them
   *     would only duplicate a delivery.
   *
   * Both sources run the same "already delivered" comparison, over the same
   * window (this pane's delivered/acked terminal rows that predate the plan).
   * The baseline is frequently the previous round's already-delivered body (the
   * pane closes right after a finished round), so source ① skipping that
   * comparison would resurrect an answer the orchestrator already has — as a
   * false `agent.failed` carrying a stale body, which no same-pane `completed`
   * row would suppress.
   */
  #salvageableTerminalBody(row: StatusEventPlanRecord): CompactAgentHistory | null {
    const generationClause =
      row.paneGeneration === null ? "pane_generation is null" : "pane_generation = ?";
    const paneParams: Array<string | number> = [
      row.herdrSessionName,
      row.paneId,
      row.createdAt.getTime(),
    ];
    if (row.paneGeneration !== null) paneParams.push(row.paneGeneration);
    const recorded = this.#stores.sqlite
      .prepare(
        `select compact_history_json, delivery_attempts from agent_events
         where herdr_session_name = ? and pane_id = ? and created_at <= ? and ${generationClause}
         order by id desc limit 20`,
      )
      .all(...paneParams) as Array<{
      compact_history_json: string | null;
      delivery_attempts: number;
    }>;
    const delivered = this.#stores.sqlite
      .prepare(
        `select compact_history_json, payload_json from agent_events
         where herdr_session_name = ? and pane_id = ? and created_at <= ?
           and type in ('agent.idle', 'agent.done', 'agent.blocked')
           and status in ('delivered', 'acked')
         order by id desc limit ?`,
      )
      .all(
        row.herdrSessionName,
        row.paneId,
        row.createdAt.getTime(),
        STALE_DUPLICATE_GUARD_SCAN_LIMIT,
      ) as Array<{ compact_history_json: string | null; payload_json: string }>;
    // The pane's current agent kind, taken from the newest delivered row's
    // payload — the closest surviving evidence once the `agents` row is gone.
    // `sameTerminalAssistantContent` compares agy/antigravity tails by ref alone,
    // so a hardcoded "pi" would dedupe an agy body that shares a ref with an
    // older delivery but carries new text, and skip a real salvage.
    const deliveredKind = agentKindFromPayloadJson(delivered[0]?.payload_json);
    const alreadyDelivered = (body: CompactAgentHistory | null | undefined): boolean =>
      body !== null &&
      body !== undefined &&
      delivered.some((other) =>
        sameTerminalAssistantContent(
          body,
          parseCompactHistoryJson(other.compact_history_json),
          deliveredKind,
        ),
      );
    if (
      isTerminalAssistant(row.compactHistory) &&
      hasNonEmptyAssistantMessage(row.compactHistory) &&
      !alreadyDelivered(row.compactHistory)
    ) {
      return row.compactHistory ?? null;
    }
    for (const candidate of recorded) {
      if (candidate.delivery_attempts !== 0) continue;
      const parsed = parseCompactHistoryJson(candidate.compact_history_json);
      if (!isTerminalAssistant(parsed) || !hasNonEmptyAssistantMessage(parsed)) continue;
      if (alreadyDelivered(parsed)) continue;
      return parsed;
    }
    return null;
  }

  /**
   * Keeps a pending plan's terminal content from dying with its agent row.
   *
   * A plan can still be pending when the orchestrator closes the pane (the
   * normal "finished, close it" discipline): the `agents` row is physically
   * deleted, and the drain that used to cancel the row as a "missing agent"
   * then lost the content for good — the plan never produced its replacement
   * row and nothing else carried the body. When the plan still holds (or the
   * pane still records) a legal terminal assistant body, the content is emitted
   * through H1's orphan terminal channel instead of being cancelled.
   *
   * Returns the emitted event, or null when the plan had nothing to
   * salvage (cancelling is then the correct outcome). Null also covers the
   * undeliverable case — content exists, but the orphan row that would carry it
   * could not pass the delivery gate (its scope cannot be stitched) — because
   * writing such a row is strictly worse than cancelling: it dead-letters, and
   * its idempotency key would make every later append return that dead row.
   */
  #salvageOrphanTerminalEvent(row: StatusEventPlanRecord): AgentEventRecord | null {
    const body = this.#salvageableTerminalBody(row);
    if (!body) return null;
    // Same orphan construction H1 uses for plan outcomes: resolve the pane's own
    // metadata from its event history, keep `agent_id = null` when the `agents`
    // row is gone, and leave the original id / pane / session in the payload. The
    // row is written as an orphan terminal event (`agent.failed`), which is the
    // one shape the delivery gate still lets through without a live agent row;
    // an orphan `agent.done` would be written and then never delivered.
    const orphanAgent: AgentIndexRecord = {
      agent: null,
      agentSession: null,
      agentStatus: "unknown",
      cwd: null,
      firstSeenAt: new Date(0),
      focused: false,
      foregroundCwd: null,
      herdrSessionName: row.herdrSessionName,
      id: row.agentId,
      lastSeenAt: new Date(0),
      name: null,
      paneId: row.paneId,
      paneRevision: null,
      ...(row.paneGeneration === null ? {} : { paneGeneration: row.paneGeneration }),
      tabId: null,
      terminalId: null,
      workspaceId: "",
    };
    // Deliverability is decided *before* the append. `#appendPlanFailedEvent`
    // stitches the orphan scope from the pane's own event history
    // (`#resolvePlanOutcomeAgent` -> `#minimalAgentFromLatestEvent`, bound to
    // `created_at <= plan.createdAt`), but an event written during *this* plan's
    // execution is newer than the plan row, and the pane may carry no earlier
    // event at all. The orphan row then keeps the placeholders above
    // (`terminal_id = null`, `workspace_id = ""`): `isDeliverableAgentEvent`
    // rejects it and `publishAgentEvent` dead-letters it, while the idempotency
    // key `agent.failed:plan:<id>` would make every later append of this plan
    // return that dead row — a strictly worse outcome than the cancel this
    // method returns instead. A stitch that resolved is taken at face value:
    // `#appendAndAckSelfEvent` re-resolves the same row, so the append writes
    // exactly what was just judged deliverable.
    if (!this.#orphanScopeDeliverable(orphanAgent, row.createdAt.getTime())) {
      console.warn(
        "Herdsman left a status event plan cancellable because the pane scope could not be stitched",
        {
          agentId: row.agentId,
          herdrSessionName: row.herdrSessionName,
          paneId: row.paneId,
          planId: row.id,
          reason: ORPHAN_SALVAGE_REASON,
        },
      );
      return null;
    }
    const event = this.#appendPlanFailedEvent({
      agent: orphanAgent,
      attempts: row.attempts,
      compactHistory: body,
      from: row.fromStatus,
      planCreatedAt: row.createdAt.getTime(),
      planId: row.id,
      reason: ORPHAN_SALVAGE_REASON,
      to: row.toStatus,
    });
    console.warn("Herdsman salvaged an orphan terminal event for a plan whose agent row is gone", {
      agentId: row.agentId,
      eventId: event.id,
      from: row.fromStatus,
      herdrSessionName: row.herdrSessionName,
      paneId: row.paneId,
      planId: row.id,
      reason: ORPHAN_SALVAGE_REASON,
      to: row.toStatus,
    });
    return event;
  }

  /**
   * Whether an orphan terminal event for this plan would carry a scope the
   * delivery gate can route. Mirrors exactly what `#appendPlanFailedEvent` will
   * resolve: a non-null `terminal_id` and non-empty `workspace_id` are what
   * `publishAgentEvent` requires (everything else the gate checks — an
   * `agent.failed` row in `pending` state — holds by construction).
   */
  #orphanScopeDeliverable(orphanAgent: AgentIndexRecord, planCreatedAt: number): boolean {
    const resolved = this.#resolvePlanOutcomeAgent(orphanAgent, planCreatedAt).agent;
    return resolved.terminalId !== null && resolved.workspaceId !== "";
  }

  /**
   * Settles a plan whose agent row can no longer be resolved: salvage its
   * terminal content as an orphan event, or cancel it when there is none.
   *
   * The cancel branch keeps its original shape (including the warning that makes
   * a content-free cancellation visible), so only the "there was something to
   * deliver" case changes behaviour. Returns the salvaged event (or null when
   * the plan was cancelled) so callers can publish it: the orchestrator's wake
   * is `agent.event`-push driven (`handleAgentEvent` in the pi extension), and
   * a salvaged row that is only written to the database would sit `pending`
   * until the next reconcile tick rather than waking the (already idle, pane
   * closed) orchestrator now.
   */
  #salvageOrCancelMissingAgentPlan(
    store: StatusEventPlanStore,
    row: StatusEventPlanRecord,
  ): AgentEventRecord | null {
    let salvaged: AgentEventRecord | null = null;
    try {
      salvaged = this.#salvageOrphanTerminalEvent(row);
      if (salvaged) {
        // The content reached the orphan channel, so the plan is settled — not
        // cancelled, and not left pending for the next tick to lose again.
        store.markCompleted(row.id);
      }
    } catch (error) {
      // The salvage must never take the drain down: a failed write falls back to
      // the pre-existing cancel behaviour.
      console.warn("Herdsman failed to salvage a status event plan for a missing agent", {
        agentId: row.agentId,
        herdrSessionName: row.herdrSessionName,
        paneId: row.paneId,
        planId: row.id,
        error: error instanceof Error ? error.message : String(error),
      });
    }
    if (salvaged) return salvaged;
    try {
      store.markCancelled(row.id);
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
    return null;
  }

  /**
   * Publishes a salvaged orphan event on the same channel a normal terminal
   * event takes (`#onAgentEvent`, used for the retry-exhausted failed branch and
   * by every caller that forwards `#runPlanRow`'s result). The wake is
   * `agent.event`-push driven (`handleAgentEvent` in the pi extension; keepalive
   * only pings), so a salvage that is merely written to the database would leave
   * the row `pending` while the orchestrator — pane closed, idle — has nothing
   * to trigger a fetch.
   */
  #publishSalvagedEvent(event: AgentEventRecord): void {
    if (VALID_AGENT_EVENT_TYPES.has(event.type)) {
      this.#onAgentEvent?.(event);
    }
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
      // The agent row is gone (pane retired while the plan was pending). Cancel
      // only when the plan has no terminal content left to hand over; the
      // salvage is published from here because no caller sees this path's
      // result (the drain owns it).
      const salvaged = this.#salvageOrCancelMissingAgentPlan(store, current);
      if (salvaged) this.#publishSalvagedEvent(salvaged);
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
        // The append-time guard refused the plan because the agent row is gone
        // (pane closed mid-execution — the common case: the orchestrator's
        // "finish and close" lands in the TURN_SIGNAL_WAIT_MS window). That is
        // the same content-loss the drain-time salvage fixes, so it settles
        // through the same path; the published result keeps waking the
        // orchestrator exactly as a normal terminal event would.
        const salvaged = this.#salvageOrCancelMissingAgentPlan(store, row);
        if (salvaged) this.#publishSalvagedEvent(salvaged);
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
        // Execution-time pane close (abort/early return before the append
        // round): same salvage as the drain-time missing-agent path instead of a
        // blind cancel, so content on disk is not lost with the plan.
        const salvaged = this.#salvageOrCancelMissingAgentPlan(store, row);
        if (salvaged) this.#publishSalvagedEvent(salvaged);
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
        // Same salvage as the other mid-execution pane-close exits above.
        const salvaged = this.#salvageOrCancelMissingAgentPlan(store, row);
        if (salvaged) this.#publishSalvagedEvent(salvaged);
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
        // `pane.list` panes are official `PaneInfo` records: snake_case only.
        const paneId = stringValue(value.pane_id);
        if (!paneId) continue;
        const revision = integerValue(value.revision);
        const terminalTitle = stringValue(value.terminal_title);
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
          safeOfficialSessionPath(agent.agentSession.value) !== null;
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
      // 显式关 Tab = 隐含消费：已经投递给编排者的行在这里直接 ack（不再走关页保留、
      // 不再被 daemon 重投），只把「从未投递过」的行留给保留规则。崩溃/掉线路径
      // （reconciler / reclaimDelivered）不经过这里，保留语义不变。
      this.#stores.agentEvents.invalidatePane({
        acknowledgeDelivered: true,
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
    const paneId = stringValue(agent.pane_id);
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
          // M2: the "trust the on-disk body" judgement is decoupled from
          // `confirmed`. That the turn-end signal arrived is already guaranteed by
          // the enclosing `if (turn?.received)`, so the deliverability judgement
          // below only has to decide whether the disk carries a deliverable body —
          // a round no longer needs a (systematically unreliable) client-side
          // `confirmed` to be released. `confirmedTerminal` keeps its own meaning
          // for the released/degraded logging and payload.
          const degradeOrRelease = (degradedReason: string): Record<string, unknown> => {
            if (confirmedTerminal) {
              // A confirmed round stays non-degraded (see above), but the reason
              // is no longer swallowed: this path used to release silently with
              // an emptied body, so an empty `agent.done` could only be
              // diagnosed by reading the pane by hand. `degraded: true` must
              // stay off here — #runPlanRow would `invalidateById(...,
              // "degraded_retry")` the row it has already written.
              console.warn(
                "Herdsman released a confirmed pi status event with no deliverable text",
                {
                  agentId: input.agent.id,
                  degradedReason,
                  herdrSessionName: input.agent.herdrSessionName,
                  paneId: input.agent.paneId,
                  planId: input.planId ?? null,
                  terminalId: input.agent.terminalId,
                },
              );
              return { staleSnapshot: false };
            }
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
            // `historyHasAdvanced(..., { requireAssistantChange: true })` compares
            // the assistant ref/text against the plan baseline. That baseline can
            // hold this round's answer (a plan row's `compact_history_json` is
            // written once at plan creation and never updated, and creation
            // measured 131ms/308ms after the final assistant message reached
            // disk). It can also hold the answer of a *previous* round — the same
            // frozen baseline is exactly what the daemon last delivered — so a
            // round that writes nothing new would otherwise re-release the
            // previous round's body as if it were this one's. The guard compares
            // `advanced` against the prior terminal rows for this agent that can
            // have reached the orchestrator — a bounded window, newest first (the
            // `agent_events` query below). Two things changed from the single-row
            // guard: (i) it now looks at a window, because a body delivered before
            // the current latest terminal row would otherwise slip through and, now
            // that M2 accepts unconfirmed rounds, be re-released as this round's
            // answer; (ii) it only counts rows in `delivered`/`acked`, because a
            // never-deliverable row (`agent.idle` from a non-`working` status, per
            // `isDeliverableAgentEvent`; ~50% of ref-bearing `agent.idle` rows for a
            // given agent (47/94 in a 7-day sample) carry the same ref as a `done` row
            // of that agent; they are never deliverable) can sit in the window and must
            // not blank a deliverable round; the tightened guard is not a strict
            // superset of the old one (its window is narrower than "every terminal
            // row", and the same window now also answers the no-advance release
            // question below — "did this agent ever deliver a terminal row?" —
            // which for unconfirmed rounds replaced `confirmed` as the gate; a
            // confirmed round is released there regardless of that answer).
            // Kept as rows (not just the boolean below) so the frozen-baseline
            // release can ask the same window whether this agent ever handed a
            // terminal row to the orchestrator: one query, one window, no second
            // parallel criterion.
            const deliveredTerminalHistories = this.#stores.sqlite
              .prepare(
                `select compact_history_json from agent_events
                 where agent_id = ? and herdr_session_name = ?
                   and type in ('agent.idle', 'agent.done', 'agent.blocked')
                   and status in ('delivered', 'acked')
                 order by id desc limit ?`,
              )
              .all(
                input.agent.id,
                input.agent.herdrSessionName,
                STALE_DUPLICATE_GUARD_SCAN_LIMIT,
              ) as Array<{ compact_history_json: string | null }>;
            const staleBaselineDuplicate = deliveredTerminalHistories.some((row) =>
              sameTerminalAssistantContent(
                advanced,
                parseCompactHistoryJson(row.compact_history_json),
                "pi",
              ),
            );
            // Whether this agent ever delivered a terminal row. This is an
            // existence question over the bounded window, not a content question:
            // `limit 200` fetches the newest ≤200 delivered/acked terminal rows, so
            // any delivery at all makes `length > 0` (an agent with 300 delivered
            // rows still reads > 0 — "older than 200 rows is invisible" applies
            // only to `staleBaselineDuplicate`, which compares a *specific* body
            // against the window and so can miss a body older than the newest 200).
            // The existence judgement therefore carries no >200-row cost.
            const hasDeliveredTerminalRow = deliveredTerminalHistories.length > 0;
            // M2 deliverability judgement: the turn-end signal already arrived
            // (the enclosing `if (turn?.received)`), so only the disk matters — a
            // non-empty terminal assistant message that was not already delivered
            // (`diskDeliverable`) is this round's answer. The client's `confirmed`
            // flag is deliberately not a conjunct here: its `expectedText` is only
            // the extension's own guess at the final tail and can systematically
            // disagree with the transcript that landed on disk (rewrite-type
            // extensions and credential redaction both make the text-level
            // confirmation never match), while the disk tail is the authoritative
            // evidence.
            const diskDeliverable =
              isTerminalAssistant(advanced) &&
              hasNonEmptyAssistantMessage(advanced) &&
              !staleBaselineDuplicate;
            if (
              !isRetry &&
              !historyHasAdvanced(advanced, input.compactHistory, { requireAssistantChange: true })
            ) {
              // Frozen-baseline release. `historyHasAdvanced(...,
              // { requireAssistantChange: true })` compares the assistant
              // ref/text against the plan baseline, and that baseline is written
              // once when the plan row is created (no UPDATE ever follows) —
              // routinely after the final assistant message already reached disk.
              // "No advance" therefore does not mean "nothing to deliver": it
              // usually means the answer is already in place, and blanking it is
              // what lost finished work (degrade -> invalidate -> the replacement
              // row waits for a 15-minute tick). Release is judged from the disk
              // (`diskDeliverable`), never from `confirmed`.
              //
              // The extra conjunct is the one the mismatch arm below does not
              // need, and it is scoped strictly to unconfirmed rounds: this arm
              // cannot tell "the frozen baseline holds this round's answer" from
              // "this round wrote nothing new". Once the agent has delivered a
              // terminal row before, an unchanged history is ambiguous — the tail
              // may be that earlier, already-delivered answer — and only the
              // content-level guard (bounded window, delivered/acked rows only,
              // ref/text comparison) stands between the two, which is not sound
              // enough to release on. For an agent that never delivered a
              // terminal row (its first terminal round) there is no earlier
              // answer to re-release, so the body on disk can only be this
              // round's.
              //
              // `confirmedTerminal` overrides that structural hold, and only for
              // this one arm. A confirmed round already proved (text-level, at
              // the client) that the on-disk tail is this round's final message,
              // so "the agent delivered a terminal row before" no longer makes
              // the tail ambiguous: it is not "this round wrote nothing new", it
              // is "this round's answer is already in place and verified". The
              // structural hold therefore stays the fallback for the unconfirmed
              // round — where nothing vouches the tail is this round's — and
              // keeps the safety netting the previous empty-body bug
              // (`20261001-pi-confirmed-turn-frozen-baseline-empty-body.md`)
              // depended on: a confirmed round with a *blanked* body is no
              // longer reachable, because the release runs before the blanking.
              // Confirming is *not* a licence to blank: with the hold lifted the
              // body is released, and a confirmed round whose disk body is
              // empty still takes `degradeOrRelease`'s soften path below.
              if (diskDeliverable && (!hasDeliveredTerminalRow || confirmedTerminal)) {
                // Trust the already-in-place answer: release it as a non-stale
                // snapshot and without a `degraded` marker (`degraded: true` is
                // what made #runPlanRow invalidate the content it had just
                // written).
                compactHistory = advanced;
                payloadExtra = { staleSnapshot: false };
                console.warn(
                  "Herdsman released a pi status event from a frozen baseline that already holds the on-disk answer",
                  {
                    agentId: input.agent.id,
                    confirmed: confirmedTerminal,
                    herdrSessionName: input.agent.herdrSessionName,
                    paneId: input.agent.paneId,
                    planId: input.planId ?? null,
                    terminalId: input.agent.terminalId,
                  },
                );
              } else {
                compactHistory = { ...advanced, lastAssistantMessage: null };
                payloadExtra = degradeOrRelease(
                  staleBaselineDuplicate ? "stale_baseline_duplicate" : "no_advance_from_input",
                );
              }
            } else {
              const advancedText = advanced.lastAssistantMessage?.text?.trim() ?? "";
              const advancedMatchesExpected = !expectedText || advancedText.endsWith(expectedText);
              // When the guess disagrees the body is still released as-is —
              // `degraded` stays off, because `degraded: true` makes #runPlanRow
              // call invalidateById(..., "degraded_retry") on content that is
              // already written — and the mismatch is recorded as a warning (split
              // by `confirmed`) so it stays diagnosable.
              if (diskDeliverable) {
                if (!advancedMatchesExpected) {
                  console.warn(
                    confirmedTerminal
                      ? "Herdsman accepted a confirmed pi status event despite an expectedText mismatch"
                      : "Herdsman accepted an unconfirmed pi status event despite an expectedText mismatch",
                    {
                      agentId: input.agent.id,
                      confirmed: confirmedTerminal,
                      degradedReason: "expected_text_mismatch",
                      herdrSessionName: input.agent.herdrSessionName,
                      paneId: input.agent.paneId,
                      planId: input.planId ?? null,
                      terminalId: input.agent.terminalId,
                    },
                  );
                  payloadExtra = { staleSnapshot: false };
                }
                compactHistory = advanced;
              } else if (!advancedMatchesExpected) {
                compactHistory = { ...advanced, lastAssistantMessage: null };
                payloadExtra = degradeOrRelease("expected_text_mismatch");
              } else if (!isTerminalAssistant(advanced) || !hasNonEmptyAssistantMessage(advanced)) {
                // `diskDeliverable` is false in this arm by construction (its
                // terminal/non-empty conjuncts are exactly this condition's
                // negation), so a round with a deliverable disk body never reaches
                // it and `non_terminal_assistant` keeps its pre-existing meaning:
                // only a non-terminal or empty tail is degraded.
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
        // The agent row is gone (pane retired mid-execution): appending would
        // create a dangling event with a dead agent_id that only the reconciler
        // could sweep, so no terminal row is appended here. `#runPlanRow` settles
        // the plan from here — salvaging its terminal content as an orphan event
        // when there is any, cancelling otherwise (the skip->completed branch
        // must stay reserved for genuine skips).
        console.debug(
          "Herdsman refusing to append a terminal status event because the agent row is gone",
          {
            agentId: input.agent.id,
            herdrSessionName: input.agent.herdrSessionName,
            paneId: input.agent.paneId,
            from: input.from,
            to: input.to,
          },
        );
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
  // Herdr payloads are snake_case only (`AgentInfo` / `PaneInfo`); no camelCase
  // alias is accepted, so a Herdr rename surfaces as a missing field instead of
  // a silent compatibility shim.
  const paneId = stringValue(raw.pane_id);
  const overlay = paneId ? overlayByPane.get(paneId) : undefined;
  const revision = integerValue(raw.revision) ?? overlay?.revision;
  const terminalTitle = stringValue(raw.terminal_title) ?? overlay?.terminalTitle;
  if (revision === undefined && !terminalTitle) return raw;
  return {
    ...raw,
    ...(revision === undefined ? {} : { revision }),
    ...(terminalTitle ? { terminal_title: terminalTitle } : {}),
  };
}

function paneGenerationOf(agent: HerdrAgentLike): string | null {
  return stringValue(agent.pane_generation) ?? stringValue(agent.creation_id);
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
  return stringValue(event.pane_generation);
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

function parseCompactHistoryJson(value: string | null): CompactAgentHistory | null {
  if (!value) return null;
  try {
    return JSON.parse(value) as CompactAgentHistory;
  } catch {
    return null;
  }
}

function agentKindFromPayloadJson(value: string | null | undefined): string | null {
  if (!value) return null;
  try {
    const payload = JSON.parse(value) as unknown;
    const agent =
      typeof payload === "object" && payload !== null
        ? (payload as { agent?: unknown }).agent
        : undefined;
    return typeof agent === "string" ? agent : null;
  } catch {
    return null;
  }
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

  // Non-agy (pi):
  if (currentMsg.ref !== null && prevMsg.ref !== null) {
    return currentMsg.ref === prevMsg.ref;
  }
  if (currentMsg.text.length > 0 && prevMsg.text.length > 0) {
    return currentMsg.text === prevMsg.text;
  }
  return false;
}
