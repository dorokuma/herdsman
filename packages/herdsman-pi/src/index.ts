import { agentIdentityLabel } from "./agent-display.js";
import { sanitizeText, textFromContent } from "./sanitize-text.js";
import { stripVTControlCharacters } from "node:util";
import { logHerdsmanPi } from "./logger.js";

export { logHerdsmanPi };
export type { HerdsmanPiLogLevel } from "./logger.js";

import {
  type AgentContextListItem,
  type AgentEventWireRecord,
  type AgentOrchestratorChanged,
  type AgentOrchestratorWireState,
  type AgentWorkspaceContextSnapshot,
  type DaemonStreamMessage,
  ReconnectingDaemonClient,
} from "./daemon-client.js";
import {
  type AgentUpdateMessageDetails,
  formatHerdsmanFooterStatus,
  renderAgentUpdateMessage,
  type HerdsmanFooterState,
} from "./agent-update-ui.js";
import {
  projectAgentOutcomes,
  formatAgentOutcomeUpdates,
  WAKE_SETTLE_MS,
} from "./wake.js";
import { loadWakeFilterConfig } from "./wake-filter-config.js";
import type { WakeFilterConfig } from "./upstream-error.js";
import { confirmSessionWrite } from "./turn-signal.js";

type PiAgentMessage = {
  content?: unknown;
  customType?: string;
  role?: string;
  [key: string]: unknown;
};

type AgentSessionRef = {
  agent: string;
  kind: "path";
  source: string;
  value: string;
};

type PiPresence = {
  connectedAt: number;
  herdrSessionName: string;
  paneId: string;
  subscriberId: string;
  terminalId: string;
  workspaceId: string;
};

type ConnectionStateResponse = {
  changed?: boolean;
  ackedEventId?: number;
  context?: AgentWorkspaceContextSnapshot | null;
  events?: AgentEventWireRecord[];
  presence: PiPresence;
  state: AgentOrchestratorWireState | null;
};

export type HerdsmanDaemonClient = {
  close(): void;
  onConnected: (() => Promise<void> | void) | undefined;
  onDisconnected: ((error: Error) => void) | undefined;
  onStreamMessage: ((message: DaemonStreamMessage) => void) | undefined;
  resetForSession?(): void;
  request(method: string, params: unknown): Promise<unknown>;
};

type CurrentScope = {
  herdrSessionName: string;
  paneId: string;
  terminalId: string;
  workspaceId: string;
};

type LaunchIdentity = {
  herdrSocketPath: string;
  paneId: string;
  workspaceId: string;
};

type DeliveredBatch = {
  abortedByUser: boolean;
  assistantFinalSucceeded: boolean;
  events: AgentEventWireRecord[];
  hasSubstantiveWork: boolean;
  invalidated: boolean;
  ownerTerminalId: string;
  herdsmanTriggered: boolean;
};

type HerdsmanState = {
  client: HerdsmanDaemonClient | undefined;
  connected: boolean;
  currentScope: CurrentScope | undefined;
  deliveredBatch: DeliveredBatch | undefined;
  ackInFlight: boolean;
  failedWakeThroughEventId: number;
  isOrchestrator: boolean;
  launchIdentity: LaunchIdentity | undefined;
  latestContext: AgentWorkspaceContextSnapshot | undefined;
  pendingEvents: AgentEventWireRecord[];
  pinnedContext: AgentWorkspaceContextSnapshot | undefined;
  presentedEventIds: Set<number>;
  reconnectingFromOn: boolean;
  registrationInFlight: Promise<void> | undefined;
  runActive: boolean;
  roleMutationInFlight: boolean;
  sessionRef: AgentSessionRef | undefined;
  subscriberId: string | undefined;
  /**
   * Delivery queue: events handed to Pi whose acknowledgement is still
   * outstanding. Id-keyed so a repeated id is stored exactly once (deliver
   * once), and every consumer sorts by id. Entries are only removed by an
   * acknowledgement (success or dead-letter) or by a role/scope reset that also
   * clears `presentedEventIds`, so releasing a deferred wake can never lose an
   * unconfirmed event.
   */
  unackedDelivered: Map<number, AgentEventWireRecord>;
  wakeDeferredUntilSettled: boolean;
  /** Wall-clock start of the current bounded wake deferral, if any. */
  wakeDeferredSince: number | undefined;
  /**
   * Set once the hard deferral budget elapsed: the next pass injects the batch
   * from the current state instead of deferring again.
   */
  wakeForcedRelease: boolean;
  /**
   * Event content queued for a busy orchestrator. Injected through the
   * `context` hook so the running turn sees the update without being
   * interrupted.
   */
  wakeContext: { content: string; eventIds: number[] } | undefined;
  wakeRequested: boolean;
  wakeRequestedThroughEventId: number;
  wakeTimer: ReturnType<typeof setTimeout> | undefined;
};

type PiContext = {
  abort?: () => void;
  isIdle?: () => boolean;
  sessionManager: { getSessionFile(): string; getSessionId(): string };
  ui: {
    notify?: (message: string, level?: "error" | "info" | "warning") => void;
    setStatus?: (key: string, value?: string) => void;
    theme: {
      bg(color: string, text: string): string;
      bold(text: string): string;
      fg(color: string, text: string): string;
    };
  };
};

type CommandOptions = {
  description: string;
  getArgumentCompletions?(prefix: string): Array<{ label: string; value: string }> | null;
  handler(args: string, ctx: PiContext): Promise<void>;
};

type PiApi = {
  appendEntry?: (customType: string, data: unknown) => void;
  on: (eventName: string, handler: (...args: any[]) => unknown) => void;
  registerCommand?: (name: string, options: CommandOptions) => void;
  registerMessageRenderer?: (
    customType: string,
    renderer: typeof renderAgentUpdateMessage,
  ) => void;
  registerTool?: (tool: unknown) => void;
  sendMessage?: (
    message: { content: string; customType: string; details?: unknown; display: boolean },
    options?: { deliverAs?: "steer" | "followUp" | "nextTurn"; triggerTurn?: boolean },
  ) => void;
  setSessionName?: (name: string) => void;
};

type ExtensionOptions = {
  clientFactory?: () => HerdsmanDaemonClient;
  onTurnCompletionSignal?: (completion: Promise<void>) => void;
  onStateExposed?: (state: HerdsmanState) => void;
  wakeFilter?: WakeFilterConfig;
};

const DEFAULT_HOME_NAME = ".herdsman";
const COMMAND_USAGE = "Usage: /herdsman [on|off|status]";
const HERDR_REQUIRED_MESSAGE = "Herdsman requires a Herdr workspace";
const RECONNECTING_MESSAGE = "Herdsman is reconnecting · try again shortly";
export const MAX_ACK_ATTEMPTS = 5;
export const ACK_BACKOFF_CAP_MS = 30_000;
const KEEPALIVE_INTERVAL_MS = 30_000;
/** Retry interval used while a wake cannot be injected (busy orchestrator). */
export const WAKE_BUSY_SPIN_MS = 100;
/**
 * Hard upper bound for every deferred wake. Once it elapses the scheduler stops
 * waiting for the orchestrator to become idle or for a settlement to arrive and
 * forces the injection decision from the current state, so a wake can never be
 * parked forever.
 */
export const WAKE_DEFERRED_TIMEOUT_MS = 5_000;

type AckFailureClass = "terminal" | "resync" | "transient";

type AckError = Error & { code?: string };

const ACK_FAILURE_CLASS_BY_CODE: Record<string, AckFailureClass> = {
  ORCHESTRATOR_NOT_OWNER: "terminal",
  ORCHESTRATOR_EVENT_INVALIDATED: "terminal",
  ORCHESTRATOR_EVENT_FAILED: "terminal",
  ORCHESTRATOR_EVENT_ALREADY_ACKED: "terminal",
  ORCHESTRATOR_EVENT_NOT_IN_SCOPE: "terminal",
  ORCHESTRATOR_EVENT_OUT_OF_ORDER: "resync",
  ORCHESTRATOR_OWNER_REPLACED: "terminal",
  ORCHESTRATOR_EVENT_NOT_FOUND: "terminal",
  ORCHESTRATOR_BUSY: "transient",
  ORCHESTRATOR_CONNECTION_LOST: "transient",
  ORCHESTRATOR_RECONCILING: "transient",
  ORCHESTRATOR_ACK_TIMEOUT: "transient",
  event_invalidated: "terminal",
};

export function classifyAckFailure(error: unknown): AckFailureClass {
  const candidate = error as { code?: unknown; message?: unknown };
  if (typeof candidate.code === "string" && candidate.code.length > 0) {
    const classification = ACK_FAILURE_CLASS_BY_CODE[candidate.code];
    if (classification) return classification;
    return "transient";
  }
  const message = typeof candidate.message === "string" ? candidate.message : String(error);
  if (/no longer pending|invalidated|Only the current orchestrator can acknowledge notifications/i.test(message)) {
    return "terminal";
  }
  if (/Only the next pending orchestrator event can be acknowledged/i.test(message)) return "resync";
  return "transient";
}

function ackFailureCode(error: unknown): string {
  const candidate = error as { code?: unknown; message?: unknown };
  return typeof candidate.code === "string" && candidate.code.length > 0
    ? candidate.code
    : typeof candidate.message === "string"
      ? candidate.message
      : String(error);
}

function ackBackoffMs(attempts: number): number {
  return Math.min(250 * 2 ** Math.max(0, attempts - 1), ACK_BACKOFF_CAP_MS);
}

function defaultHerdsmanHome() {
  return process.env.HERDSMAN_HOME || `${process.env.HOME || ""}/${DEFAULT_HOME_NAME}`;
}

export function defaultSocketPath() {
  return `${defaultHerdsmanHome().replace(/\/$/, "")}/herdsman.sock`;
}

export function createHerdsmanPiExtension(options: ExtensionOptions = {}) {
  return function herdsmanPiExtension(pi: PiApi): void {
    pi.registerMessageRenderer?.("herdsman-wake", renderAgentUpdateMessage);
    // Read once per extension instance: config.yaml changes require a Pi restart,
    // matching the daemon's startup-time config model.
    const wakeFilter = options.wakeFilter ?? loadWakeFilterConfig();

    const state: HerdsmanState = {
      client: undefined,
      connected: false,
      currentScope: undefined,
      deliveredBatch: undefined,
      failedWakeThroughEventId: 0,
      isOrchestrator: false,
      launchIdentity: undefined,
      latestContext: undefined,
      pendingEvents: [],
      pinnedContext: undefined,
      presentedEventIds: new Set(),
      reconnectingFromOn: false,
      registrationInFlight: undefined,
      roleMutationInFlight: false,
      runActive: false,
      sessionRef: undefined,
      subscriberId: undefined,
      unackedDelivered: new Map(),
      wakeDeferredUntilSettled: false,
      wakeDeferredSince: undefined,
      wakeForcedRelease: false,
      wakeContext: undefined,
      wakeRequested: false,
      wakeRequestedThroughEventId: 0,
      wakeTimer: undefined,
      ackInFlight: false,
    };
    options.onStateExposed?.(state);
    let activeContext: PiContext | undefined;
    let keepaliveTimer: ReturnType<typeof setInterval> | undefined;
    let wakeGeneration = 0;

    const stopKeepalive = () => {
      if (!keepaliveTimer) return;
      clearInterval(keepaliveTimer);
      keepaliveTimer = undefined;
    };

    const startKeepalive = (client: HerdsmanDaemonClient) => {
      stopKeepalive();
      keepaliveTimer = setInterval(() => {
        void client.request("agent.ping", {}).catch(() => undefined);
      }, KEEPALIVE_INTERVAL_MS);
      keepaliveTimer.unref?.();
    };

    const setHerdsmanUi = (ctx: PiContext | undefined) => {
      if (!ctx) return;
      const footerState: HerdsmanFooterState = state.reconnectingFromOn
        ? { kind: "reconnecting" }
        : state.isOrchestrator
          ? {
              kind: "on",
              updateCount: projectAgentOutcomes(state.pendingEvents, wakeFilter).outcomes.length,
            }
          : { kind: "off" };
      ctx.ui.setStatus?.("herdsman", formatHerdsmanFooterStatus(footerState));
    };

    const cancelWakeTimer = () => {
      wakeGeneration += 1;
      if (state.wakeTimer) clearTimeout(state.wakeTimer);
      state.wakeTimer = undefined;
      state.wakeDeferredUntilSettled = false;
      state.wakeDeferredSince = undefined;
      state.wakeForcedRelease = false;
    };

    const cancelWake = () => {
      cancelWakeTimer();
      state.wakeRequested = false;
      state.wakeRequestedThroughEventId = 0;
      // A wake queued for a busy orchestrator belongs to the role/scope that
      // queued it: dropping it here keeps a stale event body out of the context
      // of whatever session takes over next.
      state.wakeContext = undefined;
    };

    const clearAgentContext = () => {
      state.latestContext = undefined;
      state.pinnedContext = undefined;
      state.runActive = false;
    };

    const unackedDeliveredAscending = (): AgentEventWireRecord[] =>
      [...state.unackedDelivered.values()].sort((left, right) => left.id - right.id);

    /**
     * Merges freshly injected events into the delivery queue.
     *
     * Invariants (Phase 1 completeness fix):
     *  - a new batch is merged into the queue, never substituted for it, so an
     *    unconfirmed batch keeps riding along with the next delivery instead of
     *    being stranded;
     *  - the result is id-ascending and id-deduped, so the same id is never
     *    delivered (or acknowledged) twice;
     *  - entries are only ever removed by `dropUnackedDelivered` (acknowledged or
     *    dead-lettered) or by a role/scope reset, so releasing the deferred wake
     *    cannot drop an unconfirmed event.
     */
    const mergeUnackedDelivered = (
      incoming: readonly AgentEventWireRecord[],
    ): AgentEventWireRecord[] => {
      for (const event of incoming) {
        if (!state.unackedDelivered.has(event.id)) state.unackedDelivered.set(event.id, event);
      }
      return unackedDeliveredAscending();
    };

    /**
     * Drops one event from the delivery queue. The only callers are the two
     * acknowledgement outcomes (accepted, or terminally refused by the daemon)
     * and the role/scope reset, which clears the whole stream together with
     * `presentedEventIds`.
     */
    const dropUnackedDelivered = (eventId: number): void => {
      state.unackedDelivered.delete(eventId);
    };

    const pruneAcknowledgedEvents = (ackedEventId: number | undefined) => {
      if (ackedEventId === undefined) return;
      // Acknowledged events leave the pending projection but intentionally stay
      // in presentedEventIds. The daemon never re-lists acked events (acked
      // rows are excluded from pending discovery), so retaining the ids cannot
      // suppress a legitimate future delivery; it only guarantees that an event
      // already presented in this scope session is never presented again, even
      // if a reconnect, an ack retry, or a daemon redelivery replays it. The
      // set is bounded by the number of events presented per session and is
      // cleared on role loss, scope change, and shutdown.
      state.pendingEvents = state.pendingEvents.filter((event) => event.id > ackedEventId);
      // The delivery queue follows the same watermark: an event covered by the
      // advanced acknowledgement cursor is confirmed even when its own ack was
      // superseded (the daemon acknowledges by watermark), so it leaves the
      // queue and is never re-acknowledged.
      for (const eventId of [...state.unackedDelivered.keys()]) {
        if (eventId <= ackedEventId) state.unackedDelivered.delete(eventId);
      }
    };

    const isWakeableEvent = (event: AgentEventWireRecord | undefined) =>
      !event?.nextAttemptAt || event.nextAttemptAt <= Date.now();

    const applyOwnerContext = (response: ConnectionStateResponse) => {
      state.latestContext = isLocalOwner(response) ? response.context ?? undefined : undefined;
    };

    const acknowledgeEventIds = async (
      events: readonly AgentEventWireRecord[],
      options: { notify: boolean },
      ctx: PiContext,
    ): Promise<void> => {
      for (const event of [...events].sort((left, right) => left.id - right.id)) {
        try {
          // A missing client (disconnect) must never be treated as a successful
          // acknowledgement: route it through the same failure path as a
          // transient RPC error so the id keeps its backoff and stays pending.
          if (!state.client) {
            throw new Error("Herdsman Pi is not connected; cannot acknowledge notifications");
          }
          const ackResponse = (await state.client.request("agent.notifications.ack", {
            eventId: event.id,
          })) as { ackedEventId?: number; state?: { ackedEventId?: number } } | undefined;
          pruneAcknowledgedEvents(ackResponse?.ackedEventId ?? ackResponse?.state?.ackedEventId);
          state.pendingEvents = state.pendingEvents.filter((pending) => pending.id !== event.id);
          // The event is confirmed: it leaves the delivery queue for good, which
          // is what keeps the acknowledgement watermark monotonic (ids are acked
          // in ascending order and never re-issued).
          dropUnackedDelivered(event.id);
          // The id intentionally stays in presentedEventIds: the event was
          // already presented this session and must not be injected again even
          // if the daemon replays it (for example after a reconnect
          // redelivery). The set is cleared only on role loss, scope change,
          // or shutdown.
          state.failedWakeThroughEventId = Math.max(state.failedWakeThroughEventId, event.id);
          setHerdsmanUi(ctx);
        } catch (error) {
          const failureCode = ackFailureCode(error);
          const classification = classifyAckFailure(error);
          // The attempt counter is read from the live projection, not from the
          // delivery-queue snapshot the caller iterated over: a stale base would
          // pin the counter at 1 for good and make the MAX_ACK_ATTEMPTS
          // dead-letter branch unreachable for every event retried here.
          const attempts =
            (state.pendingEvents.find((pending) => pending.id === event.id)?.attempts ??
              event.attempts ??
              0) + 1;
          const attemptedAt = Date.now();
          const updatedEvent = {
            ...event,
            attempts,
            lastAttemptAt: attemptedAt,
            lastFailureCode: failureCode,
          };
          state.pendingEvents = state.pendingEvents.map((pending) =>
            pending.id === event.id ? updatedEvent : pending,
          );
          // Keep the delivery-queue entry in step with the live accounting: the
          // same event may be retried later (another settlement, or a redelivery
          // that re-attaches it to a new batch), and that attempt must resume
          // from this counter instead of restarting at 1.
          if (state.unackedDelivered.has(event.id)) {
            state.unackedDelivered.set(event.id, updatedEvent);
          }

          if (classification === "terminal") {
            state.pendingEvents = state.pendingEvents.filter((pending) => pending.id !== event.id);
            // A terminally refused event is dead-lettered by the daemon
            // (failedWakeThroughEventId is the dead-letter barrier), so it can
            // never be confirmed later; it leaves the delivery queue as well.
            dropUnackedDelivered(event.id);
            state.failedWakeThroughEventId = Math.max(state.failedWakeThroughEventId, event.id);
            if (/Only the current orchestrator can acknowledge notifications/i.test(failureCode)) {
              state.isOrchestrator = false;
              logHerdsmanPi(
                "warn",
                `[herdsman-pi] lost orchestrator ownership while acknowledging event ${event.id}`,
              );
            } else {
              logHerdsmanPi(
                "warn",
                `[herdsman-pi] terminal acknowledgement failure eventId=${event.id} attempts=${attempts} code=${failureCode}`,
              );
            }
            setHerdsmanUi(ctx);
            continue;
          }

          if (attempts >= MAX_ACK_ATTEMPTS) {
            state.pendingEvents = state.pendingEvents.filter((pending) => pending.id !== event.id);
            // Dead-lettered by this client: the id is behind the dead-letter
            // barrier from now on, so it can never be confirmed later and leaves
            // the delivery queue with the pending projection.
            dropUnackedDelivered(event.id);
            state.failedWakeThroughEventId = Math.max(state.failedWakeThroughEventId, event.id);
            logHerdsmanPi(
              "warn",
              `[herdsman-pi] acknowledgement moved to dead-letter eventId=${event.id} attempts=${attempts} code=${failureCode}`,
            );
            setHerdsmanUi(ctx);
            continue;
          }

          if (classification === "resync") {
            const resyncEvent = {
              ...updatedEvent,
              nextAttemptAt: attemptedAt + ackBackoffMs(attempts),
            };
            state.pendingEvents = state.pendingEvents.map((pending) =>
              pending.id === event.id ? resyncEvent : pending,
            );
            try {
              const response = (await state.client?.request(
                "agent.orchestrator.get",
                {},
              )) as ConnectionStateResponse | undefined;
              // Refresh pending data without applying the full connection response: that
              // helper schedules a new wake, which would make this failed batch race
              // with the current settlement and can replay an earlier event. The
              // failed event remains pending and the next wake is scheduled by
              // finishBatch(), so this round performs no additional acknowledgements.
              if (response) addPendingEvents(response.events ?? [], ctx);
              pruneAcknowledgedEvents(response?.state?.ackedEventId ?? response?.ackedEventId);
              setHerdsmanUi(ctx);
            } catch (resyncError) {
              logHerdsmanPi(
                "warn",
                `[herdsman-pi] acknowledgement resync failed eventId=${event.id} attempts=${attempts} code=${ackFailureCode(resyncError)}`,
              );
            }
            // The event stays pending with a backoff so the ack cursor can
            // sweep it later, but it was already presented this session and is
            // not re-presented: presentedEventIds keeps the guard until scope
            // reset. Continue so one failed event does not block the rest of
            // the batch.
            continue;
          }

          state.pendingEvents = state.pendingEvents.map((pending) =>
            pending.id === event.id
              ? { ...pending, nextAttemptAt: attemptedAt + ackBackoffMs(attempts) }
              : pending,
          );
          if (options.notify) {
            ctx.ui.notify?.(
              "Herdsman couldn’t acknowledge agent updates · updates remain pending",
              "warning",
            );
          }
          setHerdsmanUi(ctx);
          // The event stays pending with a backoff so the ack cursor can sweep
          // it later, but it was already presented this session and is not
          // re-presented: presentedEventIds keeps the guard until scope reset.
          continue;
        }
      }
    };

    // Upstream model errors wake nobody, but they still have to leave the
    // daemon's pending queue or it never converges. The silent path therefore
    // acknowledges them without sendMessage, without notify, and without
    // touching presentedEventIds (reserved for genuinely presented outcomes).
    const scheduleSilentUpstreamErrorAck = (
      ctx: PiContext,
      events: readonly AgentEventWireRecord[],
    ) => {
      if (state.wakeTimer || state.wakeRequested) return;
      const scope = state.currentScope;
      if (!state.isOrchestrator || !scope) return;
      const generation = wakeGeneration;
      const ownerHerdrSessionName = scope.herdrSessionName;
      const ownerTerminalId = scope.terminalId;
      const ownerWorkspaceId = scope.workspaceId;
      state.wakeTimer = setTimeout(() => {
        state.wakeTimer = undefined;
        void (async () => {
          if (
            generation !== wakeGeneration ||
            !state.isOrchestrator ||
            state.currentScope?.herdrSessionName !== ownerHerdrSessionName ||
            state.currentScope?.terminalId !== ownerTerminalId ||
            state.currentScope?.workspaceId !== ownerWorkspaceId
          ) {
            return;
          }
          // A delivered batch or an in-flight ack owns the cursor; its
          // settlement schedules the next sweep instead of racing this one.
          if (state.deliveredBatch || state.ackInFlight) return;
          if (!state.client || !state.connected) return;
          state.ackInFlight = true;
          setHerdsmanUi(ctx);
          try {
            await acknowledgeEventIds(events, { notify: false }, ctx);
          } finally {
            state.ackInFlight = false;
            setHerdsmanUi(ctx);
          }
          scheduleWake(ctx);
        })();
      }, WAKE_SETTLE_MS);
    };

    /**
     * Arms the bounded deferral for a wake that cannot be injected right now.
     *
     * The retry spins every `WAKE_BUSY_SPIN_MS` while the orchestrator is busy
     * and flips `wakeForcedRelease` once `WAKE_DEFERRED_TIMEOUT_MS` has elapsed,
     * so the next pass injects the batch from the current state (as a queued,
     * non-triggering follow-up) instead of waiting for an idle signal or a
     * settlement that may never arrive.
     */
    const scheduleDeferredWake = (ctx: PiContext) => {
      state.wakeDeferredUntilSettled = true;
      const since = state.wakeDeferredSince ?? Date.now();
      state.wakeDeferredSince = since;
      const remaining = WAKE_DEFERRED_TIMEOUT_MS - (Date.now() - since);
      if (remaining <= 0) state.wakeForcedRelease = true;
      if (state.wakeTimer) return;
      state.wakeTimer = setTimeout(() => {
        state.wakeTimer = undefined;
        if (state.wakeForcedRelease && state.deliveredBatch && ctx.isIdle?.() !== false) {
          // The hard deadline only releases the delivery — it never discards an
          // unconfirmed event. The batch record is dropped so a wake turn that
          // is no longer running cannot gate later wakes, but its events stay in
          // the delivery queue (`unackedDelivered`) and are re-attached to the
          // batch injected right below, which acknowledges them once it settles.
          state.deliveredBatch = undefined;
        }
        scheduleWake(ctx);
      }, Math.max(0, Math.min(WAKE_BUSY_SPIN_MS, remaining)));
    };

    const scheduleWake = (ctx: PiContext | undefined) => {
      if (!ctx || !state.isOrchestrator || !state.currentScope || !pi.sendMessage) return;
      if (state.wakeTimer || state.wakeRequested) return;
      const projection = projectAgentOutcomes(state.pendingEvents, wakeFilter);
      const outcomes = projection.outcomes.filter(
        (outcome) =>
          outcome.eventId > state.failedWakeThroughEventId &&
          !state.presentedEventIds.has(outcome.eventId),
      );
      const suppressedEvents = projection.suppressedUpstreamErrorEventIds
        .filter(
          (eventId) =>
            eventId > state.failedWakeThroughEventId && !state.presentedEventIds.has(eventId),
        )
        .map((eventId) => state.pendingEvents.find((pending) => pending.id === eventId))
        .filter((event): event is AgentEventWireRecord => event !== undefined)
        .sort((left, right) => left.id - right.id);

      const wakeable = outcomes.filter((outcome) =>
        isWakeableEvent(state.pendingEvents.find((pending) => pending.id === outcome.eventId)),
      );
      if (wakeable.length === 0) {
        // A suppressed upstream error that is now due must be silently
        // acknowledged before any backoff timer is planted: planting the timer
        // first would make scheduleSilentUpstreamErrorAck's entry guard
        // (`if (state.wakeTimer || state.wakeRequested) return`) bounce the ack
        // off its own timer and, once the backoff window has elapsed, the
        // 0ms-timer + blocked-ack loop never converges the queue.
        const dueSuppressed = suppressedEvents.filter(isWakeableEvent);
        if (dueSuppressed.length > 0) {
          scheduleSilentUpstreamErrorAck(ctx, dueSuppressed);
          return;
        }
        // No suppressed event is due, so plant a backoff timer for the next
        // future `nextAttemptAt`. Expired timestamps are excluded (strictly
        // greater than now) so an already-past window does not produce a
        // zero-delay spin.
        const nextAttemptAt = [
          ...outcomes.map((outcome) => outcome.eventId),
          ...suppressedEvents.map((event) => event.id),
        ]
          .map((eventId) => state.pendingEvents.find((event) => event.id === eventId)?.nextAttemptAt)
          .filter((value): value is number => value !== undefined && value > Date.now())
          .sort((left, right) => left - right)[0];
        if (nextAttemptAt !== undefined) {
          state.wakeTimer = setTimeout(() => {
            state.wakeTimer = undefined;
            scheduleWake(ctx);
          }, nextAttemptAt - Date.now());
        }
        return;
      }

      // An in-flight batch owns the ack cursor and a busy orchestrator must not
      // be interrupted, so neither is woken immediately — but both are deferred
      // on a bounded spin (never parked until an event that may never come).
      const inFlight = state.deliveredBatch !== undefined || state.ackInFlight;
      if (!state.wakeForcedRelease && (inFlight || ctx.isIdle?.() === false)) {
        scheduleDeferredWake(ctx);
        return;
      }
      const generation = wakeGeneration;
      const ownerHerdrSessionName = state.currentScope.herdrSessionName;
      const ownerTerminalId = state.currentScope.terminalId;
      const ownerWorkspaceId = state.currentScope.workspaceId;
      state.wakeTimer = setTimeout(() => {
        const startWake = async () => {
          if (
            generation !== wakeGeneration ||
            !state.isOrchestrator ||
            state.currentScope?.herdrSessionName !== ownerHerdrSessionName ||
            state.currentScope?.terminalId !== ownerTerminalId ||
            state.currentScope?.workspaceId !== ownerWorkspaceId
          ) {
            state.wakeTimer = undefined;
            return;
          }
          if (ctx.isIdle?.() === false && !state.wakeForcedRelease) {
            state.wakeTimer = undefined;
            scheduleDeferredWake(ctx);
            return;
          }

          try {
            const response = (await state.client?.request(
              "agent.orchestrator.get",
              {},
            )) as ConnectionStateResponse | undefined;
            if (!response) {
              state.wakeTimer = undefined;
              return;
            }
            applyConnectionStateResponse(response, ctx);
          } catch {
            state.wakeTimer = undefined;
            // A failed load is only temporary: the batch stays pending and is
            // retried on the next wake instead of being permanently suppressed.
            ctx.ui.notify?.(
              "Herdsman couldn’t load agent updates · updates remain pending",
              "warning",
            );
            return;
          }

          if (
            generation !== wakeGeneration ||
            !state.isOrchestrator ||
            state.currentScope?.herdrSessionName !== ownerHerdrSessionName ||
            state.currentScope?.terminalId !== ownerTerminalId ||
            state.currentScope?.workspaceId !== ownerWorkspaceId
          ) {
            state.wakeTimer = undefined;
            return;
          }
          if (ctx.isIdle?.() === false && !state.wakeForcedRelease) {
            state.wakeTimer = undefined;
            scheduleDeferredWake(ctx);
            return;
          }

          const batchEvents = [...state.pendingEvents].sort((left, right) => left.id - right.id);
          const batchProjection = projectAgentOutcomes(batchEvents, wakeFilter);
          const batchSuppressedIds = new Set(batchProjection.suppressedUpstreamErrorEventIds);
          const batchOutcomes = batchProjection.outcomes.filter(
            (outcome) =>
              outcome.eventId > state.failedWakeThroughEventId &&
              !state.presentedEventIds.has(outcome.eventId) &&
              isWakeableEvent(batchEvents.find((event) => event.id === outcome.eventId)),
          );
          if (batchOutcomes.length === 0) {
            state.wakeTimer = undefined;
            return;
          }
          const current = batchOutcomes;
          const incomingEvents = batchEvents.filter((event) =>
            batchOutcomes.some((outcome) => outcome.eventId === event.id),
          );
          const previousBatch = state.deliveredBatch;
          state.wakeTimer = undefined;
          state.wakeRequested = true;
          state.wakeRequestedThroughEventId = current.at(-1)?.eventId ?? 0;
          // Dual-track injection: an idle orchestrator gets a triggered
          // follow-up turn (immediate delivery), while a busy one is not
          // interrupted — the same content is queued as a non-triggering
          // follow-up and additionally exposed through the `context` hook so the
          // running turn can already see it.
          const orchestratorBusy = ctx.isIdle?.() === false;
          const wakeContent = formatAgentOutcomeUpdates(batchOutcomes);
          try {
            pi.sendMessage?.(
              {
                content: wakeContent,
                customType: "herdsman-wake-context",
                // Suppressed upstream errors are dropped from the injected
                // context, but every other pending id stays listed so the
                // evidence trail for the decision still names what was pending.
                details: {
                  eventIds: batchEvents
                    .filter((event) => !batchSuppressedIds.has(event.id))
                    .map((event) => event.id),
                },
                display: false,
              },
              orchestratorBusy
                ? { deliverAs: "followUp", triggerTurn: false }
                : { deliverAs: "followUp", triggerTurn: true },
            );
            // A queued (non-triggering) delivery keeps the content available to
            // the current turn through the context hook until it is settled or
            // superseded by the next injection.
            state.wakeContext = orchestratorBusy
              ? { content: wakeContent, eventIds: batchOutcomes.map((outcome) => outcome.eventId) }
              : undefined;
            state.wakeForcedRelease = false;
            state.wakeDeferredSince = undefined;
            // Only expose the batch after the hidden context was accepted by pi. This
            // keeps an injection failure eligible for daemon redelivery.
            //
            // The batch is the delivery queue plus this injection: previously
            // unconfirmed events are merged (never replaced) so that a forced
            // release always leaves both the old and the new events deliverable
            // and acknowledgeable in id order. The turn-consumption flags of a
            // still-running previous batch are carried over, because they
            // describe whether the content already reached the orchestrator.
            //
            // `hasSubstantiveWork` is the sole gate that decides whether an
            // ownership/scope change may abort the in-flight turn (see loseRole
            // and resetForScopeChange), and aborting is only ever allowed for a
            // *pure* Herdsman wake turn. A busy orchestrator gets the batch as a
            // non-triggering queued follow-up, which rides the user's own turn:
            // that turn is not a Herdsman wake turn, so it must never be aborted
            // on our behalf and the flag is set here.
            state.deliveredBatch = {
              abortedByUser: previousBatch?.abortedByUser ?? false,
              assistantFinalSucceeded: previousBatch?.assistantFinalSucceeded ?? false,
              events: mergeUnackedDelivered(incomingEvents),
              hasSubstantiveWork: orchestratorBusy || (previousBatch?.hasSubstantiveWork ?? false),
              invalidated: false,
              ownerTerminalId,
              herdsmanTriggered: true,
            };
            state.wakeRequested = false;
            state.wakeRequestedThroughEventId = 0;
            // Record the presentation so a reclaim redelivery of the same id is
            // never presented twice. The set is monotonic for the lifetime of
            // the current orchestrator scope: ids are only removed on role
            // loss, scope change, or shutdown (pruneAcknowledgedEvents keeps
            // acknowledged ids on purpose).
            for (const outcome of batchOutcomes) {
              state.presentedEventIds.add(outcome.eventId);
            }
          } catch {
            state.deliveredBatch = undefined;
            state.wakeRequested = false;
          }
        };
        void startWake();
      }, WAKE_SETTLE_MS);
    };

    const loseRole = (
      ctx: PiContext | undefined,
      options: { abort?: boolean; preservePresented?: boolean } = {},
    ) => {
      if (state.deliveredBatch && !options.preservePresented) {
        state.deliveredBatch.invalidated = true;
        const abortedBatch = state.deliveredBatch;
        if (options.abort) {
          state.failedWakeThroughEventId = Math.max(
            state.failedWakeThroughEventId,
            ...abortedBatch.events.map((event) => event.id),
          );
          // Owner lost mid-turn: drop the in-flight batch so it cannot gate the next wake.
          state.deliveredBatch = undefined;
          state.wakeDeferredUntilSettled = false;
        }
        if (
          options.abort &&
          abortedBatch.herdsmanTriggered &&
          !abortedBatch.hasSubstantiveWork
        ) {
          ctx?.abort?.();
        }
      }
      if (state.wakeRequestedThroughEventId > 0) {
        state.failedWakeThroughEventId = Math.max(
          state.failedWakeThroughEventId,
          state.wakeRequestedThroughEventId,
        );
      }
      cancelWake();
      clearAgentContext();
      state.isOrchestrator = false;
      state.pendingEvents = [];
      // A transient disconnect (reconnect) keeps the presentation guard so an
      // event already presented in this scope session is not presented again;
      // only a genuine role/scope loss or shutdown resets it.
      if (!options.preservePresented) {
        state.presentedEventIds.clear();
        // The delivery queue dies with the presentation guard: once the guard is
        // gone the daemon's pending events can be presented (and acknowledged)
        // again, so keeping the old queue would only risk a stale id.
        state.unackedDelivered.clear();
      }
      state.reconnectingFromOn = false;
      setHerdsmanUi(ctx);
    };

    const markDisconnected = (ctx: PiContext | undefined) => {
      const reconnectingFromOn = state.reconnectingFromOn || state.isOrchestrator;
      // Reconnects are not scope resets: keep the presentation guard and the
      // in-flight batch so a settled turn can still be acknowledged and no
      // already-presented event is injected twice.
      loseRole(ctx, { abort: false, preservePresented: true });
      state.reconnectingFromOn = reconnectingFromOn;
      setHerdsmanUi(ctx);
    };

    const resetForScopeChange = (ctx: PiContext | undefined) => {
      clearAgentContext();
      if (
        state.deliveredBatch?.herdsmanTriggered &&
        !state.deliveredBatch.hasSubstantiveWork
      ) {
        // Scope changes invalidate the batch; an in-flight wake is only aborted
        // when it is still the pure, empty Herdsman wake turn.
        ctx?.abort?.();
      }
      if (state.deliveredBatch) state.deliveredBatch.invalidated = true;
      state.deliveredBatch = undefined;
      cancelWake();
      state.failedWakeThroughEventId = 0;
      state.pendingEvents = [];
      state.presentedEventIds.clear();
      state.unackedDelivered.clear();
      setHerdsmanUi(ctx);
    };

    const addPendingEvents = (events: AgentEventWireRecord[], ctx: PiContext | undefined) => {
      const byId = new Map(state.pendingEvents.map((event) => [event.id, event]));
      let addedNewEvent = false;
      for (const event of events) {
        const previous = byId.get(event.id);
        // Only an event that survives the dead-letter barrier counts as new.
        // The server keeps re-listing dead-lettered events (id <= failedWakeThroughEventId)
        // on every orchestrator.get; treating one as new would cancel the in-flight
        // wake (wakeGeneration++) on every get and stall the whole stream forever.
        if (!previous && event.id > state.failedWakeThroughEventId) addedNewEvent = true;
        byId.set(event.id, previous ? { ...event, ...previous } : event);
      }
      state.pendingEvents = [...byId.values()]
        .filter((event) => event.id > state.failedWakeThroughEventId)
        .sort((left, right) => left.id - right.id);
      setHerdsmanUi(ctx);
      if (addedNewEvent && state.wakeTimer) cancelWakeTimer();
      scheduleWake(ctx);
    };

    const syncPendingEventsFromServer = (
      serverEvents: AgentEventWireRecord[],
      ctx: PiContext | undefined,
    ) => {
      const serverEventById = new Map(serverEvents.map((event) => [event.id, event]));
      const inFlightIds = new Set(state.deliveredBatch?.events.map((event) => event.id) ?? []);
      const previousById = new Map(state.pendingEvents.map((event) => [event.id, event]));
      const isTruncated = serverEvents.length >= 100;
      const maxServerEventId = serverEvents.reduce((max, event) => Math.max(max, event.id), 0);

      let addedNewEvent = false;
      const nextPending: AgentEventWireRecord[] = [];

      for (const serverEvent of serverEvents) {
        const previous = previousById.get(serverEvent.id);
        if (!previous && serverEvent.id > state.failedWakeThroughEventId) addedNewEvent = true;
        nextPending.push(previous ? { ...serverEvent, ...previous } : serverEvent);
      }

      for (const [previousId, previousEvent] of previousById) {
        if (serverEventById.has(previousId)) continue;
        if (inFlightIds.has(previousId)) {
          nextPending.push(previousEvent);
        } else if (isTruncated && previousId > maxServerEventId) {
          nextPending.push(previousEvent);
        }
      }

      for (const inFlightId of inFlightIds) {
        if (!serverEventById.has(inFlightId) && !previousById.has(inFlightId)) {
          const inFlightEvent = state.deliveredBatch?.events.find(
            (event) => event.id === inFlightId,
          );
          if (inFlightEvent) {
            nextPending.push(inFlightEvent);
          }
        }
      }

      state.pendingEvents = nextPending
        .filter((event) => event.id > state.failedWakeThroughEventId)
        .sort((left, right) => left.id - right.id);

      setHerdsmanUi(ctx);
      if (state.pendingEvents.length === 0 && state.wakeTimer) {
        cancelWakeTimer();
      } else if (addedNewEvent && state.wakeTimer) {
        cancelWakeTimer();
      }
      scheduleWake(ctx);
    };

    const applyConnectionStateResponse = (
      response: ConnectionStateResponse,
      ctx: PiContext | undefined,
      options: { notifyReconnectLoss?: boolean } = {},
    ) => {
      const reconnectingOwner = options.notifyReconnectLoss && state.reconnectingFromOn;
      const scopeChanged =
        state.currentScope !== undefined &&
        (state.currentScope.herdrSessionName !== response.presence.herdrSessionName ||
          state.currentScope.workspaceId !== response.presence.workspaceId);
      if (scopeChanged) resetForScopeChange(ctx);
      state.currentScope = {
        herdrSessionName: response.presence.herdrSessionName,
        paneId: response.presence.paneId,
        terminalId: response.presence.terminalId,
        workspaceId: response.presence.workspaceId,
      };
      const isOwner = isLocalOwner(response);
      if (!isOwner) {
        loseRole(ctx);
        if (reconnectingOwner) {
          ctx?.ui.notify?.(
            response.state?.owner
              ? `Herdsman is off · moved to ${response.state.owner.paneId}`
              : "Herdsman is off",
            "info",
          );
        }
        return;
      }
      state.isOrchestrator = true;
      state.reconnectingFromOn = false;
      if (state.deliveredBatch) {
        if (state.deliveredBatch.invalidated || ctx?.isIdle?.() !== false) {
          // A batch invalidated by a genuine role/scope reset, or a batch whose
          // wake turn is no longer running (it never started, or it already
          // settled during the disconnect), can no longer be acknowledged by a
          // future settlement. Clear it so the still-pending events can be
          // re-woken on this fresh connection instead of being gated forever;
          // already acked events are covered by the server cursor
          // (pruneAcknowledgedEvents below).
          state.deliveredBatch = undefined;
          state.wakeContext = undefined;
        }
        // Otherwise the batch's wake turn is still in flight: keep it so the
        // settlement acknowledges it and the events are not re-presented.
      }
      applyOwnerContext(response);
      setHerdsmanUi(ctx);
      syncPendingEventsFromServer(response.events ?? [], ctx);
      pruneAcknowledgedEvents(response.state?.ackedEventId ?? response.ackedEventId);
      if (state.pendingEvents.length === 0 && state.wakeTimer) cancelWakeTimer();
      setHerdsmanUi(ctx);
      scheduleWake(ctx);
    };

    const handleAgentEvent = (event: AgentEventWireRecord, ctx: PiContext | undefined) => {
      if (!state.isOrchestrator || !state.currentScope || !event.terminalId) return;
      if (event.terminalId === state.currentScope.terminalId) return;
      addPendingEvents([event], ctx);
      pi.appendEntry?.("herdsman.agent_event", event);
      scheduleWake(ctx);
    };

    const refreshAfterRoleGain = async (ctx: PiContext | undefined) => {
      if (!state.client || !state.connected) return;
      try {
        const response = (await state.client.request(
          "agent.orchestrator.get",
          {},
        )) as ConnectionStateResponse;
        applyConnectionStateResponse(response, ctx);
      } catch {
        // Reconnect handling owns transport failures.
      }
    };

    const handleRoleChange = (change: AgentOrchestratorChanged, ctx: PiContext | undefined) => {
      const terminalId = state.currentScope?.terminalId;
      if (!terminalId) return;
      const wasOwner = change.previous.owner?.terminalId === terminalId;
      const isOwner = change.current.owner?.terminalId === terminalId;
      if (isOwner && change.current.owner) {
        const scopeChanged =
          state.currentScope?.herdrSessionName !== change.current.herdrSessionName ||
          state.currentScope?.workspaceId !== change.current.workspaceId;
        if (scopeChanged) resetForScopeChange(ctx);
        state.currentScope = {
          herdrSessionName: change.current.herdrSessionName,
          paneId: change.current.owner.paneId,
          terminalId,
          workspaceId: change.current.workspaceId,
        };
        const gainedRole = !state.isOrchestrator;
        state.isOrchestrator = true;
        state.reconnectingFromOn = false;
        setHerdsmanUi(ctx);
        if (gainedRole || scopeChanged) void refreshAfterRoleGain(ctx);
        return;
      }
      if (!wasOwner) return;
      state.currentScope = {
        herdrSessionName: change.current.herdrSessionName,
        paneId: state.currentScope?.paneId ?? change.previous.owner?.paneId ?? "unknown",
        terminalId,
        workspaceId: change.current.workspaceId,
      };
      loseRole(ctx, { abort: true });
      if (!state.roleMutationInFlight) {
        ctx?.ui.notify?.(
          change.current.owner
            ? `Herdsman is off · moved to ${change.current.owner.paneId}`
            : "Herdsman is off",
          "info",
        );
      }
    };

    const handleStreamMessage = (message: DaemonStreamMessage) => {
      if (message.method === "agent.event") {
        handleAgentEvent(message.params.event, activeContext);
        return;
      }
      if (message.method === "agent.context.changed") {
        if (
          state.isOrchestrator &&
          state.currentScope?.herdrSessionName === message.params.herdrSessionName &&
          state.currentScope.workspaceId === message.params.workspaceId
        ) {
          const next = message.params.context ?? undefined;
          const retain = (snapshot: AgentWorkspaceContextSnapshot | undefined) =>
            snapshot
              ? {
                  ...snapshot,
                  agents: snapshot.agents.flatMap((agent) => {
                    const nextAgents =
                      next?.agents.filter((candidate) => candidate.paneId === agent.paneId) ?? [];
                    if (nextAgents.length === 0) return [];
                    const nextAgent = nextAgents.find(
                      (candidate) => !agent.id || !candidate.id || agent.id === candidate.id,
                    );
                    return nextAgent ? [nextAgent] : [];
                  }),
                }
              : undefined;
          state.latestContext = next;
          state.pinnedContext = retain(state.pinnedContext);
        }
        return;
      }
      handleRoleChange(message.params.change, activeContext);
    };

    const registerPresence = (ctx: PiContext): Promise<void> => {
      if (state.registrationInFlight) return state.registrationInFlight;
      const client = state.client;
      const launchIdentity = state.launchIdentity;
      const subscriberId = state.subscriberId;
      const sessionRef = state.sessionRef;
      if (!client || !launchIdentity || !subscriberId) return Promise.resolve();
      if (!sessionRef?.value) {
        return Promise.reject(new Error("Pi session file is unavailable for Herdsman presence"));
      }
      const registration = client
        .request("agent.orchestrator.register", {
          herdrSocketPath: launchIdentity.herdrSocketPath,
          paneId: state.currentScope?.paneId ?? launchIdentity.paneId,
          sessionRef,
          subscriberId,
          subscriberKind: "pi",
          workspaceId: state.currentScope?.workspaceId ?? launchIdentity.workspaceId,
        })
        .then((response) => {
          state.connected = true;
          startKeepalive(client);
          applyConnectionStateResponse(response as ConnectionStateResponse, ctx, {
            notifyReconnectLoss: true,
          });
        })
        .catch((error) => {
          stopKeepalive();
          state.connected = false;
          const incompatibleMessage =
            error instanceof Error && /incompatible/i.test(error.message)
              ? error.message
              : undefined;
          if (incompatibleMessage) ctx.ui.notify?.(incompatibleMessage, "error");
          markDisconnected(ctx);
          throw error;
        })
        .finally(() => {
          state.registrationInFlight = undefined;
        });
      state.registrationInFlight = registration;
      return registration;
    };

    pi.registerCommand?.("herdsman", {
      description: "Watch Herdsman agent updates in this Pi",
      getArgumentCompletions(prefix: string) {
        const items = ["on", "off", "status"]
          .filter((value) => value.startsWith(prefix))
          .map((value) => ({ label: value, value }));
        return items.length > 0 ? items : null;
      },
      handler: async (args: string, ctx: PiContext) => {
        const value = args.trim();
        const action = value === "" ? "status" : value;
        if (action !== "on" && action !== "off" && action !== "status") {
          ctx.ui.notify?.(COMMAND_USAGE, "warning");
          return;
        }
        if (!state.launchIdentity) {
          ctx.ui.notify?.(HERDR_REQUIRED_MESSAGE, "error");
          return;
        }
        if (!state.client || !state.connected || !state.currentScope) {
          ctx.ui.notify?.(RECONNECTING_MESSAGE, "warning");
          return;
        }
        try {
          if (action === "status") {
            const response = (await state.client.request(
              "agent.orchestrator.get",
              {},
            )) as ConnectionStateResponse;
            applyConnectionStateResponse(response, ctx);
            notifyLocalStatus(response, ctx);
            return;
          }
          state.roleMutationInFlight = true;
          const response = (await state.client.request("agent.orchestrator.set", {
            enabled: action === "on",
          })) as ConnectionStateResponse;
          applyConnectionStateResponse(response, ctx);
          notifyLocalStatus(response, ctx);
        } catch (error) {
          ctx.ui.notify?.(error instanceof Error ? error.message : String(error), "error");
        } finally {
          state.roleMutationInFlight = false;
        }
      },
    });

    pi.on("session_start", (_event: unknown, ctx: PiContext) => {
      activeContext = ctx;
      state.subscriberId = ctx.sessionManager.getSessionId();
      state.sessionRef = {
        agent: "pi",
        kind: "path",
        source: "herdr:pi",
        value: ctx.sessionManager.getSessionFile(),
      };
      state.launchIdentity = herdrLaunchIdentity(process.env);
      if (!state.launchIdentity) {
        state.connected = false;
        loseRole(ctx);
        return;
      }
      stopKeepalive();
      state.client?.close();
      const client = options.clientFactory?.() ?? new ReconnectingDaemonClient({ socketPath: defaultSocketPath() });
      client.resetForSession?.();
      const closeClient = client.close.bind(client);
      client.close = () => {
        stopKeepalive();
        closeClient();
      };
      state.client = client;
      client.onConnected = () => registerPresence(ctx);
      client.onDisconnected = () => {
        stopKeepalive();
        state.connected = false;
        markDisconnected(activeContext);
      };
      client.onStreamMessage = handleStreamMessage;
    });

    pi.on("session_shutdown", () => {
      stopKeepalive();
      state.connected = false;
      loseRole(activeContext);
      state.deliveredBatch = undefined;
      state.presentedEventIds.clear();
      state.unackedDelivered.clear();
      state.client?.close();
      state.client = undefined;
      activeContext = undefined;
    });

    const assistantMessageText = (message: Record<string, unknown>): string => {
      const text = textFromContent(message.content);
      if (text === null) return "";
      return sanitizeText(text).text;
    };

    // Turn completion signal: after Pi's own final assistant message has been
    // written to its session file, tell the daemon so the agent.done/blocked
    // event it samples next captures a non-empty lastAssistantMessage. The
    // write is confirmed through a bounded stat check; the signal is still sent
    // on timeout so the daemon never waits on us forever.
    const signalTurnCompletion = (expectedText: string) => {
      const client = state.client;
      const scope = state.currentScope;
      const sessionPath = state.sessionRef?.value;
      if (!client || !state.connected || !scope || !sessionPath) return;
      const completion = (async () => {
        const check = await confirmSessionWrite({ expectedText, path: sessionPath });
        try {
          const params: Record<string, unknown> = {
            confirmed: check.confirmed,
            herdrSessionName: scope.herdrSessionName,
            paneId: scope.paneId,
            terminalId: scope.terminalId,
            workspaceId: scope.workspaceId,
          };
          if (expectedText) params.expectedText = expectedText;
          await client.request("agent.turn.completed", params);
        } catch (error) {
          logHerdsmanPi(
            "warn",
            `[herdsman-pi] turn completion signal failed reason=${check.reason} error=${error instanceof Error ? error.message : String(error)}`,
          );
        }
      })();
      options.onTurnCompletionSignal?.(completion);
    };

    pi.on("message_end", (event: Record<string, unknown>) => {
      const message = record(event.message);
      if (message.role !== "assistant") return;
      const stopReason = stringValue(message.stopReason);
      if (state.deliveredBatch) {
        state.deliveredBatch.abortedByUser = stopReason === "aborted";
        state.deliveredBatch.assistantFinalSucceeded =
          stopReason === "stop" || stopReason === "length";
        if (
          message.content !== undefined &&
          ((typeof message.content === "string" && message.content.length > 0) ||
            (Array.isArray(message.content) && message.content.length > 0))
        ) {
          state.deliveredBatch.hasSubstantiveWork = true;
        }
      }
      if (stopReason === "stop" || stopReason === "length") {
        signalTurnCompletion(assistantMessageText(message));
      }
    });

    pi.on("tool_execution_start", () => {
      if (state.deliveredBatch) state.deliveredBatch.hasSubstantiveWork = true;
    });

    pi.on("tool_result", () => {
      if (state.deliveredBatch) state.deliveredBatch.hasSubstantiveWork = true;
    });

    pi.on("agent_start", () => {
      if (state.runActive) return;
      state.runActive = true;
      state.pinnedContext =
        state.isOrchestrator && !state.deliveredBatch?.herdsmanTriggered
          ? state.latestContext
          : undefined;
    });

    pi.on("context", (event: { messages: PiAgentMessage[] }) => {
      const messages = event.messages.filter((message) => !isNormalHerdsmanContext(message));
      const additions: PiAgentMessage[] = [];
      const snapshot = state.pinnedContext;
      if (snapshot && snapshot.agents.length > 0) {
        additions.push({
          content: formatHiddenAgentContext({
            agents: snapshot.agents,
            workspaceId: snapshot.workspaceId,
          }),
          customType: "herdsman-agent-context",
          display: false,
          role: "custom",
          timestamp: Date.now(),
        });
      }
      // A wake queued for a busy orchestrator is not allowed to interrupt the
      // running tool chain, so its content is additionally pinned to the current
      // context: the orchestrator sees the child-agent outcome in this turn
      // without a triggered follow-up. The entry is dropped again by
      // isNormalHerdsmanContext, so at most one copy is present per call.
      //
      // `eventIds` mirrors what this turn actually presents (the freshly injected
      // outcomes): events carried over in the delivery queue were already shown
      // to the orchestrator in the turn that presented them, so they are not
      // re-listed here. The queued follow-up message itself keeps the wider
      // `details.eventIds` set (everything still unconfirmed).
      const queuedWake = state.wakeContext;
      if (queuedWake) {
        additions.push({
          content: queuedWake.content,
          customType: "herdsman-wake-queued",
          details: { eventIds: queuedWake.eventIds },
          display: false,
          role: "custom",
          timestamp: Date.now(),
        });
      }
      return additions.length === 0 ? { messages } : { messages: [...messages, ...additions] };
    });

    pi.on("agent_settled", async (_event: unknown, ctx: PiContext) => {
      state.runActive = false;
      state.pinnedContext = undefined;
      const batch = state.deliveredBatch;
      if (!batch) {
        state.wakeDeferredUntilSettled = false;
        scheduleWake(ctx);
        return;
      }
      state.deliveredBatch = undefined;
      state.ackInFlight = true;
      const stillOwner =
        state.isOrchestrator && state.currentScope?.terminalId === batch.ownerTerminalId;
      const failBatch = () => {
        ctx.ui.notify?.(
          "Herdsman couldn’t acknowledge agent updates · updates remain pending",
          "warning",
        );
      };
      const finishBatch = () => {
        state.ackInFlight = false;
        state.wakeDeferredUntilSettled = false;
        state.wakeDeferredSince = undefined;
        state.wakeForcedRelease = false;
        state.wakeContext = undefined;
        setHerdsmanUi(ctx);
        scheduleWake(ctx);
      };

      // The delivery queue — not the injection snapshot — is what gets
      // acknowledged: it holds every event handed to Pi that is still
      // unconfirmed (a release merges batches instead of replacing them), is
      // id-ascending, and an id leaves it only when the daemon accepts it.
      //
      // An event whose own acknowledgement already failed is left out here: it is
      // never retried by this path (the daemon's cursor advance sweeps it, and a
      // retry would reset its attempt/backoff accounting), but it stays in the
      // queue until that cursor or a scope reset confirms it. A failed or
      // dead-lettered acknowledgement thus never depends on the timing of the
      // release to stay recoverable.
      //
      // Restricting to a *live* row with `attempts === 0` has two holes on
      // purpose. `?? 0` covers an event the live projection no longer holds at
      // all (the server stopped listing it, or `failedWakeThroughEventId`
      // filters it out): the projection cannot tell us it already failed, so the
      // event gets one more attempt — a deliberate self-healing opportunity that
      // then accumulates on the queue copy's counter and can reach
      // MAX_ACK_ATTEMPTS instead of restarting at 1 every round.
      const ackable = unackedDeliveredAscending().filter(
        (event) =>
          (state.pendingEvents.find((pending) => pending.id === event.id)?.attempts ?? 0) === 0,
      );
      if (ackable.length === 0) {
        finishBatch();
        return;
      }

      if (
        (!batch.assistantFinalSucceeded && !batch.abortedByUser) ||
        batch.invalidated ||
        !stillOwner ||
        !state.client ||
        !state.connected
      ) {
        failBatch();
        finishBatch();
        return;
      }

      await acknowledgeEventIds(ackable, { notify: true }, ctx);
      finishBatch();
    });

  };
}

export default createHerdsmanPiExtension();

export function formatHiddenAgentContext(input: {
  agents: AgentContextListItem[];
  workspaceId: string;
}): string {
  return [
    "[HERDSMAN AGENT CONTEXT]",
    `Current Herdr workspace: ${input.workspaceId}`,
    ...input.agents.map((agent) => {
      const history = agent.history ?? {};
      const identity = agentIdentityLabel({
        agent: agent.agent ?? "unknown",
        name: agent.name,
      });
      const paneId = agent.paneId ?? "unknown";
      const status = agent.agentStatus ?? "unknown";
      const prefix = `- ${identity} ${paneId} ${status}`;

      const rawAgent = record(agent);
      const tabTitleCandidate =
        stringValue(rawAgent.terminalTitle) ?? stringValue(rawAgent.label);
      const tabTitleCleaned =
        tabTitleCandidate !== null ? sanitizeAndCleanContextText(tabTitleCandidate) : "";
      const tabTitle =
        tabTitleCleaned.length > 0 ? truncateSummary(tabTitleCleaned, 60) : null;

      const rawHistory = record(history);
      const lastAssistantRecord = record(history.lastAssistantMessage);
      const timeCandidate =
        lastAssistantRecord.timestamp ??
        history.updatedAt ??
        rawHistory.updatedAt ??
        rawAgent.updatedAt ??
        rawAgent.time;
      const formattedTime = formatTimestamp(timeCandidate);

      const assistantRaw = history.lastAssistantMessage?.text;
      const assistantCleaned =
        assistantRaw !== undefined && assistantRaw !== null
          ? sanitizeAndCleanContextText(assistantRaw)
          : "";
      const assistantSummary =
        assistantCleaned.length > 0 ? truncateSummary(assistantCleaned, 100) : null;

      const segments = [
        prefix,
        tabTitle,
        formattedTime,
        assistantSummary,
      ].filter((segment): segment is string => segment !== null && segment.length > 0);

      return segments.join(" · ");
    }),
    "Use herdsman agent get/read if details are needed.",
  ].join("\n");
}

export function formatHiddenAgentUpdates(events: AgentEventWireRecord[]): string {
  return [
    "[HERDSMAN AGENT UPDATES]",
    ...events.map((event) => {
      const payload = record(event.payload);
      const history = event.compactHistory ?? {};
      const identity = agentIdentityLabel({
        agent: stringValue(payload.agent) ?? "unknown",
        name: stringValue(payload.name),
      });
      return [
        `- ${event.type} ${identity} ${event.paneId ?? "unknown"}`,
        `  last assistant: ${sanitizeAndCleanContextText(history.lastAssistantMessage?.text ?? "")}`,
        `  event: ${event.id}`,
      ].join("\n");
    }),
  ].join("\n");
}

function isNormalHerdsmanContext(message: PiAgentMessage): boolean {
  return (
    message.customType === "herdsman-agent-context" ||
    message.customType === "herdsman-wake-queued" ||
    contentIncludesMarker(message.content, "[HERDSMAN AGENT CONTEXT]")
  );
}

function contentIncludesMarker(content: unknown, marker: string): boolean {
  if (typeof content === "string") return content.includes(marker);
  if (!Array.isArray(content)) return false;
  return content.some((block) => {
    const value = record(block);
    return (
      contentIncludesMarker(value.text, marker) || contentIncludesMarker(value.content, marker)
    );
  });
}

function isLocalOwner(response: ConnectionStateResponse): boolean {
  return (
    response.state?.owner?.terminalId === response.presence.terminalId &&
    response.state.herdrSessionName === response.presence.herdrSessionName &&
    response.state.workspaceId === response.presence.workspaceId
  );
}

function localStatusMessage(response: ConnectionStateResponse): string {
  if (!isLocalOwner(response) || !response.state?.owner) return "Herdsman is off";
  const scope = `${response.presence.herdrSessionName}/${response.presence.workspaceId}`;
  return `Herdsman is watching agent updates · ${scope} · ${response.state.owner.paneId}`;
}

function notifyLocalStatus(response: ConnectionStateResponse, ctx: PiContext): void {
  ctx.ui.notify?.(localStatusMessage(response), "info");
}

function herdrLaunchIdentity(environment: NodeJS.ProcessEnv): LaunchIdentity | undefined {
  if (environment.HERDR_ENV !== "1") return undefined;
  const herdrSocketPath = stringValue(environment.HERDR_SOCKET_PATH);
  const paneId = stringValue(environment.HERDR_PANE_ID);
  const workspaceId = stringValue(environment.HERDR_WORKSPACE_ID);
  if (!herdrSocketPath || !paneId || !workspaceId) return undefined;
  return { herdrSocketPath, paneId, workspaceId };
}


function stringValue(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}

function record(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

function cleanContextText(value: string): string {
  return stripVTControlCharacters(value)
    .replace(
      /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f\u00ad\u180e\u200b-\u200f\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff]/g,
      "",
    )
    .replace(/\s+/g, " ")
    .trim();
}

function sanitizeAndCleanContextText(value: string): string {
  const preCleaned = cleanContextText(value);
  if (!preCleaned) return "";
  const sanitized = sanitizeText(preCleaned).text;
  return cleanContextText(sanitized);
}

function truncateSummary(value: string, limit = 100): string {
  if (value.length <= limit) return value;
  const maxSearch = Math.min(value.length, limit + 8);
  const searchSlice = value.slice(0, maxSearch);
  const lastSpaceIndex = searchSlice.search(/\s[^\s]*$/);
  if (lastSpaceIndex >= limit - 20) {
    return `${value.slice(0, lastSpaceIndex)}…`;
  }
  return `${value.slice(0, limit)}…`;
}

function formatTimestamp(value: unknown): string | null {
  if (typeof value === "string") {
    const trimmed = value.trim();
    if (/^\d{2}:\d{2}:\d{2}$/.test(trimmed)) return trimmed;
    if (/^\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(trimmed)) return trimmed;
  }
  if (!value) return null;
  const date = value instanceof Date ? value : new Date(value as string | number);
  if (Number.isNaN(date.getTime())) return null;
  const pad = (n: number) => String(n).padStart(2, "0");
  const month = pad(date.getMonth() + 1);
  const day = pad(date.getDate());
  const hours = pad(date.getHours());
  const minutes = pad(date.getMinutes());
  const seconds = pad(date.getSeconds());
  return `${month}-${day} ${hours}:${minutes}:${seconds}`;
}

