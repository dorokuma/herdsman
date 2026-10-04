import { agentIdentityLabel } from "./agent-display.js";
import { sanitizeText, textFromContent } from "./sanitize-text.js";
import { stripVTControlCharacters } from "node:util";
import { logHerdsmanPi } from "./logger.js";

export { logHerdsmanPi };
export type { HerdsmanPiLogLevel } from "./logger.js";

import {
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
  /**
   * Event ids handed to Pi as a follow-up whose content has not been seen
   * entering the transcript yet.
   *
   * A wake is only ever injected into an idle orchestrator, and it always
   * triggers its own turn, so the copy Pi holds is a normal turning message
   * rather than a queued follow-up nothing may drain. This set is what keeps such
   * an in-flight delivery out of both the acknowledgement path and the
   * transcript, and it carries three guarantees at once:
   *
   * - never lost: while it is non-empty the next settlement leaves the id
   *   pending, so the next `scheduleWake` re-presents it (with the real update
   *   content, never with an empty marker run) instead of parking it in a
   *   follow-up queue nothing drains;
   * - never acknowledged unseen: these ids are excluded from the acknowledgement
   *   path, so the daemon keeps them pending and redelivers them when this
   *   session never consumes them (the only way a delivery Pi itself dropped —
   *   clearQueue / restoreQueuedMessagesToEditor — can still be recovered);
   * - never duplicated: they are excluded from every later injection, so a
   *   redelivery of the same id cannot put the same content into the transcript
   *   twice.
   *
   * Ids leave the set when their content reaches the transcript (consumption
   * evidence: the hidden wake message's `message_end`, which then moves them to
   * `wakeConsumptionObserved`) or when the event leaves the delivery queue for
   * good (acknowledged, covered by the acknowledgement watermark,
   * dead-lettered), and with the delivery queue on a role/scope reset. A
   * settlement that saw no evidence dead-letters them: it lifts the injection
   * barrier (via `wakeRetryableEventIds`) but not the watermark one, because the
   * content was never confirmed.
   */
  wakeAwaitingConsumption: Set<number>;
  /**
   * Ids whose content was observed in the transcript and which are therefore
   * confirmed on the evidence alone, whatever the turn that carried them did.
   *
   * The daemon confirms by a monotonic watermark (`where id <= ?`), so an id that
   * stays unacknowledged blocks every later one: a turn that ends in error after
   * the content already reached the orchestrator must not pin that watermark, so
   * the evidence — not the turn outcome — authorises these ids.
   */
  wakeConsumptionObserved: Set<number>;
  /**
   * Ids already handed to the orchestrator in this session that must never be
   * injected again, because the copy that carries them can outlive the bookkeeping
   * that knew about it.
   *
   * Pi's follow-up queue is process-wide and the extension has no API to query or
   * clear it, so an id can become "un-presented" again while its content is still
   * on its way: a role/scope reset clears the presentation guard (and the delivery
   * queue), and a consumed id leaves the queue without ever being acknowledged (its
   * batch is gone), so the daemon keeps redelivering it. This set is the guard that
   * survives all of that:
   *
   * - an unconsumed delivery carried over a reset lands here (see
   *   `clearDeliveryBookkeeping`), which deliberately trades away the redelivery
   *   remedy for it (logged there) in exchange for never presenting a duplicate;
   * - a consumed id lands here too, so a redelivery after the scope that consumed
   *   it is gone still cannot inject it a second time (the id also re-enters the
   *   current scope's `presentedEventIds`, see the consumption evidence handler).
   *
   * Ids leave the set when the daemon has confirmed them (their own
   * acknowledgement, the acknowledgement watermark, dead-lettering) or when they
   * leave the delivery queue for good — the daemon then holds nothing that could be
   * redelivered, so there is nothing left to block.
   */
  wakeSuppressedEventIds: Set<number>;
  /**
   * Last reason an event id was skipped for injection, for the rate-limited
   * diagnostic in `noteSkippedWakeInjection`: one line per id and reason.
   *
   * Skipping a redelivery is intentional but invisible, so without this the log
   * could not tell "no update arrived" from "updates were suppressed"; with it a
   * daemon that redelivers the same event in a loop still cannot flood the file.
   */
  wakeSkipLogReasons: Map<number, string>;
  /**
   * Ids whose delivery was dead-lettered and which a later wake must be allowed
   * to present again.
   *
   * `wakeAwaitingConsumption` deliberately keeps holding these ids: unconfirmed
   * content must still block the acknowledgement watermark, because the daemon
   * confirms by `where id <= ?` and letting a larger id through would swallow the
   * dead-lettered one for good. What the write-off changes is only the *other*
   * role of that set: an id that is still awaiting consumption is also barred
   * from every later injection, and a dead-lettered one has to be presented
   * again — it never reached the transcript, so a redelivery is the only remedy.
   *
   * An id leaves the set as soon as a wake hands it to Pi again (it is in flight
   * once more, so the ordinary `awaiting` guard applies), when the daemon
   * confirms it, or on a role/scope reset.
   */
  wakeRetryableEventIds: Set<number>;
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
/** `customType` of the hidden wake context this extension injects. */
const WAKE_CONTEXT_CUSTOM_TYPE = "herdsman-wake-context";

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
      presentedEventIds: new Set(),
      reconnectingFromOn: false,
      registrationInFlight: undefined,
      roleMutationInFlight: false,
      runActive: false,
      sessionRef: undefined,
      subscriberId: undefined,
      unackedDelivered: new Map(),
      wakeDeferredUntilSettled: false,
      wakeAwaitingConsumption: new Set(),
      wakeConsumptionObserved: new Set(),
      wakeSuppressedEventIds: new Set(),
      wakeSkipLogReasons: new Map(),
      wakeRetryableEventIds: new Set(),
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
    };

    const cancelWake = () => {
      cancelWakeTimer();
      state.wakeRequested = false;
      state.wakeRequestedThroughEventId = 0;
    };

    const clearAgentContext = () => {
      state.latestContext = undefined;
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
     * Drops one event from the delivery queue. The callers are the two
     * acknowledgement outcomes (accepted, or terminally refused by the daemon),
     * the acknowledgement watermark that covers a whole range of ids, and the
     * role/scope reset, which clears the entire stream through
     * `clearDeliveryBookkeeping`.
     *
     * An event that leaves the queue can no longer be awaiting consumption: its
     * acknowledgement cursor covered it, or it is dead-lettered and will never be
     * redelivered. The awaiting set therefore follows the queue here — without
     * that, a dead-lettered id would keep the settlement driving continuations
     * (and holding its batch open) for content that can never arrive. The other
     * two sets describe the same ids and follow the queue just as well: a
     * confirmed id needs no evidence flag, and an id that is gone from the queue
     * cannot be re-injected, so it needs no suppression either.
     */
    const dropUnackedDelivered = (eventId: number): void => {
      state.unackedDelivered.delete(eventId);
      state.wakeAwaitingConsumption.delete(eventId);
      state.wakeConsumptionObserved.delete(eventId);
      state.wakeSuppressedEventIds.delete(eventId);
      state.wakeRetryableEventIds.delete(eventId);
    };

    /**
     * Drops the presentation guard, the delivery queue, and the unconsumed
     * bookkeeping together.
     *
     * These describe the same events — handed to Pi, not yet confirmed by the
     * daemon — so they are only ever cleared together, and only by a reset that
     * also discards the pending projection: a genuine role/scope loss and
     * shutdown. A transient disconnect keeps all of them (`preservePresented`) so
     * an in-flight batch can still be settled and acknowledged after the
     * reconnect, and so no already-presented event is presented again.
     *
     * One thing survives the reset (`wakeSuppressedEventIds`), because nothing here
     * can invalidate it: an id handed to Pi may still sit in Pi's process-wide
     * follow-up queue (no API to query or clear it), so dropping it together with
     * the queue would let a daemon redelivery present the same update a second
     * time in the scope that takes over. Unconsumed ids are carried over for that
     * reason; ids consumed *after* their scope was reset join the same set from the
     * consumption handler. The trade-off is explicit: for a carried-over id the
     * redelivery remedy is given up on purpose (see the log line) until the daemon
     * confirms it some other way.
     */
    const clearDeliveryBookkeeping = () => {
      const carriedOver = [...state.wakeAwaitingConsumption].sort((left, right) => left - right);
      for (const eventId of carriedOver) state.wakeSuppressedEventIds.add(eventId);
      if (carriedOver.length > 0) {
        logHerdsmanPi(
          "info",
          `[herdsman-pi] keeping ${carriedOver.length} unconsumed wake event id(s) suppressed across the scope change eventIds=${carriedOver.join(",")} · daemon redelivery for them is traded away to keep the transcript single-copy`,
        );
      }
      state.presentedEventIds.clear();
      // Once the guard is gone the daemon's pending events can be presented (and
      // acknowledged) again, so keeping the old queue would only risk a stale id.
      state.unackedDelivered.clear();
      state.wakeAwaitingConsumption.clear();
      state.wakeConsumptionObserved.clear();
      state.wakeSkipLogReasons.clear();
      state.wakeRetryableEventIds.clear();
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
        if (eventId <= ackedEventId) dropUnackedDelivered(eventId);
      }
      // The watermark also covers ids that survived a reset as suppression
      // entries: the daemon considers them confirmed, so they will not be
      // redelivered and the suppression has nothing left to block.
      for (const eventId of [...state.wakeSuppressedEventIds]) {
        if (eventId <= ackedEventId) state.wakeSuppressedEventIds.delete(eventId);
      }
      for (const eventId of [...state.wakeSkipLogReasons.keys()]) {
        if (eventId <= ackedEventId) state.wakeSkipLogReasons.delete(eventId);
      }
    };

    /**
     * Records, once per id and reason, that an event which is already in the
     * orchestrator's hands was not injected again.
     *
     * Skipping a redelivery is deliberate — the content must not enter the
     * transcript twice — but it is also invisible: a daemon that keeps redelivering
     * one event would otherwise leave no trace at all, and "an update never
     * arrived" could not be told apart from "an update was suppressed" in the log.
     * The line is emitted once per id and reason (a change of reason is logged
     * again: `presented`, `awaiting` and `suppressed` describe different ownership
     * of the same id), so a redelivery loop cannot flood the file.
     */
    const noteSkippedWakeInjection = (eventId: number, reason: string): false => {
      if (state.wakeSkipLogReasons.get(eventId) !== reason) {
        state.wakeSkipLogReasons.set(eventId, reason);
        logHerdsmanPi(
          "info",
          `[herdsman-pi] wake injection skipped eventId=${eventId} reason=${reason} · update already presented (or still in flight) in this session, so a daemon redelivery is not shown twice`,
        );
      }
      return false;
    };

    /**
     * Whether an event was already handed to the orchestrator in this session and
     * therefore must not be injected again (logging why, at most once per reason).
     *
     * - `awaiting`: a copy of it was handed to Pi and has not been seen in the
     *   transcript yet (a dead-lettered one is re-presentable instead, see
     *   `wakeRetryableEventIds`, and is therefore checked first);
     * - `presented`: it was injected before (or its content has since been observed
     *   in the transcript), so a redelivery would duplicate it;
     * - `suppressed`: its copy may still sit in Pi's process-wide follow-up queue
     *   after a role/scope reset cleared the local guards, so the redelivery is the
     *   only one that must not be shown (see `wakeSuppressedEventIds`).
     */
    const alreadyPresented = (eventId: number): boolean => {
      if (state.wakeRetryableEventIds.has(eventId)) {
        // Its delivery was dead-lettered, so the content never reached the
        // transcript: a redelivery is the only remedy and must not be skipped.
        return false;
      }
      if (state.wakeAwaitingConsumption.has(eventId)) {
        noteSkippedWakeInjection(eventId, "awaiting");
        return true;
      }
      if (state.presentedEventIds.has(eventId)) {
        noteSkippedWakeInjection(eventId, "presented");
        return true;
      }
      if (state.wakeSuppressedEventIds.has(eventId)) {
        noteSkippedWakeInjection(eventId, "suppressed");
        return true;
      }
      return false;
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
     * Event ids a hidden wake message proves to have reached the transcript.
     *
     * A wake injection names the outcomes it presents in
     * `details.presentedEventIds`, and Pi writes those details onto the session
     * entry it emits on `message_end`. That emission is the only consumption
     * evidence there is: nothing else tells us that the content — not merely the
     * request that carried it — reached the orchestrator.
     *
     * The wider `details.eventIds` list is deliberately not evidence: it names
     * every id that was pending at injection time, including copies that an
     * earlier injection presented (and that may have been dropped by Pi), so using
     * it would confirm content nobody ever saw. Only real, numeric ids count, and
     * a message without readable ones proves nothing: the caller logs that instead
     * of confirming anything.
     */
    const wakeConsumedEventIds = (message: Record<string, unknown>): number[] => {
      const presented = record(message.details).presentedEventIds;
      if (!Array.isArray(presented)) return [];
      return presented.filter((eventId): eventId is number => typeof eventId === "number");
    };

    /**
     * The contiguous prefix of the delivery queue that may be acknowledged now.
     *
     * The delivery queue — not the injection snapshot — is what gets acknowledged:
     * it holds every event handed to Pi that is still unconfirmed (a release merges
     * batches instead of replacing them), is id-ascending, and an id leaves it only
     * when the daemon accepts it.
     *
     * An acknowledgement tells the daemon "the orchestrator has this", and the
     * daemon confirms by a monotonic watermark (`update agent_events set status =
     * 'acked' where id <= ?`), so acking a larger id confirms every smaller one with
     * it. The queue is therefore walked in ascending id order and only its
     * *contiguous* confirmable prefix is returned: the walk stops at the first id
     * that is not confirmable. Skipping an unconfirmed id would hand it to the
     * watermark, which swallows it for good.
     *
     * An id is confirmable when its content is known to have reached the transcript
     * (`wakeConsumptionObserved`), or — for a delivery that was never queued, i.e. a
     * triggered prompt — when the turn that received it produced a final response or
     * was aborted by the user. An id still awaiting consumption has no evidence and
     * blocks the prefix: leaving it unacknowledged keeps it pending, which is what
     * lets the daemon redeliver the one copy that never arrived.
     *
     * An id awaiting consumption is the in-flight state of a delivery that has been
     * handed to Pi but whose `message_end` has not been seen yet. It blocks the
     * prefix, and the settlement that still finds it there dead-letters it
     * (`writeOffStrandedWakeDelivery`): the content never reached the transcript, so
     * it must stay pending in the daemon and be eligible for the next wake instead
     * of being confirmed by a heuristic.
     *
     * An event whose own acknowledgement already failed is left out (the daemon's
     * cursor advance sweeps it, and a retry would reset its attempt/backoff
     * accounting), but it must not hold the prefix back: it stays in the queue until
     * that cursor or a scope reset confirms it, so a failed or dead-lettered
     * acknowledgement never depends on the timing of the release to stay
     * recoverable. Restricting to a *live* row with `attempts === 0` has two holes
     * on purpose: `?? 0` covers an event the live projection no longer holds at all
     * (the server stopped listing it, or `failedWakeThroughEventId` filters it out),
     * and the projection cannot tell us it already failed — so the event gets one
     * more attempt, a deliberate self-healing opportunity that then accumulates on
     * the queue copy's counter and can reach MAX_ACK_ATTEMPTS instead of restarting
     * at 1 every round.
     */
    const confirmableDeliveryPrefix = (turnProducedFinalResponse: boolean) => {
      const deliveryQueue = unackedDeliveredAscending();
      const confirmablePrefix: AgentEventWireRecord[] = [];
      let blockedByMissingTurn = false;
      for (const event of deliveryQueue) {
        // An id that was handed to Pi and whose evidence has not arrived yet
        // blocks the prefix: it has no evidence at all, and the settlement still
        // owes it either that evidence (a run drains the copy) or a dead-letter.
        // A dead-lettered id keeps blocking it for the same reason — unconfirmed
        // content must never be swept by the watermark — while an id that is not
        // awaiting consumption and not observed otherwise looks like a delivered,
        // triggered prompt and would be confirmed by the turn heuristic.
        if (
          state.wakeAwaitingConsumption.has(event.id) ||
          state.wakeRetryableEventIds.has(event.id)
        ) {
          break;
        }
        if (!state.wakeConsumptionObserved.has(event.id) && !turnProducedFinalResponse) {
          blockedByMissingTurn = true;
          break;
        }
        // An event whose own acknowledgement already failed is left to the
        // daemon's cursor sweep (see above); it must not hold the prefix back.
        if ((state.pendingEvents.find((pending) => pending.id === event.id)?.attempts ?? 0) > 0) {
          continue;
        }
        confirmablePrefix.push(event);
      }
      // Nothing else keeps the batch in flight once the settlement has decided:
      // any id that is still awaiting consumption has been dead-lettered above,
      // which lifts only the injection barrier — it still blocks the watermark.
      return { deliveryQueue, confirmablePrefix, blockedByMissingTurn };
    };

    /**
     * Dead-letters a wake delivery whose content never reached the transcript.
     *
     * The caller is a settlement that produced no consumption evidence for these
     * ids: the injected message was never carried out by any run (the turn ended
     * before its stop point, was aborted, or Pi dropped the message), so nothing
     * may be told to the daemon about them. An acknowledgement is a statement
     * that the orchestrator has the content, and here that statement would be a
     * lie: the daemon would mark the event consumed and never redeliver it.
     *
     * What the write-off does is local only: the ids stop being barred from a
     * later injection, so the next `scheduleWake` re-presents them with the real
     * update content. They deliberately stay in `wakeAwaitingConsumption` (and
     * therefore in the delivery queue): unconfirmed content must keep blocking
     * the acknowledgement watermark, because the daemon confirms by
     * `where id <= ?` and confirming a larger id would swallow them for good.
     * The events themselves stay pending in the daemon: nothing was acknowledged,
     * so a later wake is still eligible to present them, and a replacement
     * session can still receive them.
     *
     * The presentation guard is deliberately *not* extended here:
     * `presentedEventIds` is the "this content is in the transcript" set, and
     * these ids have no evidence of ever being there. Writing them off would
     * lock the update out of this session for good, which is exactly the
     * "busy delivery never arrives" failure this replaces.
     */
    const writeOffStrandedWakeDelivery = (eventIds: number[], ctx: PiContext): void => {
      if (eventIds.length === 0) return;
      for (const eventId of eventIds) {
        state.wakeAwaitingConsumption.delete(eventId);
        // Unconfirmed content must still block the acknowledgement watermark: the
        // daemon confirms by `where id <= ?`, so letting a later id through would
        // swallow this one for good. The retry set is that barrier, and it is also
        // what lifts the injection barrier — a delivery that never reached the
        // transcript has to be presented again (see `alreadyPresented`).
        state.wakeRetryableEventIds.add(eventId);
      }
      logHerdsmanPi(
        "warn",
        `[herdsman-pi] wake delivery dead-lettered eventIds=${eventIds.join(",")} · updates retained unacked in daemon: no consumption evidence, nothing acknowledged, and the ids stay eligible for a later wake`,
      );
      ctx.ui.notify?.(
        `Herdsman · ${eventIds.length} agent update${eventIds.length === 1 ? "" : "s"} could not be delivered by a wake turn · updates retained unacked in daemon: read the agent directly for the details, or hand this workspace to another terminal so the daemon delivers it there`,
        "warning",
      );
    };


    /**
     * Arms the deferral for a wake that cannot be injected right now.
     *
     * The retry spins every `WAKE_BUSY_SPIN_MS` while the orchestrator is busy
     * or an earlier delivery is still in flight, and it does so for as long as
     * that lasts. There is deliberately no deadline: a wake injected into a
     * busy orchestrator either lands in a follow-up queue nothing drains — the
     * "busy delivery never arrives" failure this replaces — or interrupts the
     * user's own turn, so waiting is the only outcome that loses nothing. The
     * spin is short and re-reads `isIdle`, so the gap between the turn that ends
     * and the injection is one tick, and `agent_settled` closes it immediately
     * through `scheduleWake`.
     */
    const scheduleDeferredWake = (ctx: PiContext) => {
      state.wakeDeferredUntilSettled = true;
      if (state.wakeTimer) return;
      state.wakeTimer = setTimeout(() => {
        state.wakeTimer = undefined;
        scheduleWake(ctx);
      }, WAKE_BUSY_SPIN_MS);
    };

    const scheduleWake = (ctx: PiContext | undefined) => {
      if (!ctx || !state.isOrchestrator || !state.currentScope || !pi.sendMessage) return;
      if (state.wakeTimer || state.wakeRequested) {
        // A pending wake owns the release; this branch is also hit re-entrantly by
        // a running pass (its fired settle timer is still set), and ending the
        // deferral there would let a second injection ride in behind it.
        return;
      }
      const projection = projectAgentOutcomes(state.pendingEvents, wakeFilter);
      const outcomes = projection.outcomes.filter(
        (outcome) =>
          outcome.eventId > state.failedWakeThroughEventId &&
          // Already in the orchestrator's hands: injecting it again would put the
          // same content into the transcript twice. See `alreadyPresented`.
          !alreadyPresented(outcome.eventId),
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
      // be interrupted, so neither is woken immediately — the wake stays
      // deferred on the spin until the batch settles and the orchestrator is
      // idle, and nothing is released past that gate.
      const inFlight = state.deliveredBatch !== undefined || state.ackInFlight;
      if (inFlight || ctx.isIdle?.() === false) {
        scheduleDeferredWake(ctx);
        return;
      }
      const generation = wakeGeneration;
      const ownerHerdrSessionName = state.currentScope.herdrSessionName;
      const ownerTerminalId = state.currentScope.terminalId;
      const ownerWorkspaceId = state.currentScope.workspaceId;
      state.wakeTimer = setTimeout(() => {
        const startWake = async () => {          if (
            generation !== wakeGeneration ||
            !state.isOrchestrator ||
            state.currentScope?.herdrSessionName !== ownerHerdrSessionName ||
            state.currentScope?.terminalId !== ownerTerminalId ||
            state.currentScope?.workspaceId !== ownerWorkspaceId
          ) {
            state.wakeTimer = undefined;
            return;
          }
          if (ctx.isIdle?.() === false) {
            state.wakeTimer = undefined;
            // Still busy: this pass re-defers and keeps waiting, so nothing is
            // injected into (or parked for) a running turn.
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
          if (ctx.isIdle?.() === false) {
            state.wakeTimer = undefined;
            // See the earlier re-check: a busy orchestrator keeps waiting.
            scheduleDeferredWake(ctx);
            return;
          }

          const batchEvents = [...state.pendingEvents].sort((left, right) => left.id - right.id);
          const batchProjection = projectAgentOutcomes(batchEvents, wakeFilter);
          const batchSuppressedIds = new Set(batchProjection.suppressedUpstreamErrorEventIds);
          const batchOutcomes = batchProjection.outcomes.filter(
            (outcome) =>
              outcome.eventId > state.failedWakeThroughEventId &&
              !alreadyPresented(outcome.eventId) &&
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
          // Single-track injection: the wake content is handed to Pi exactly
          // once, as the follow-up message below, and only once the orchestrator
          // is idle — a busy one has been deferred (see `scheduleDeferredWake`),
          // because a follow-up parked in a running turn's queue may never be
          // drained. There is deliberately no second copy on the `context` hook
          // either: the update enters the transcript exactly once per event id,
          // through the one message this path sends.
          const wakeContent = formatAgentOutcomeUpdates(batchOutcomes);
          // Single line, injection path only: the state the batch was taken from,
          // so a wake that still arrives late can be told apart from one that was
          // held back by the busy gate.
          logHerdsmanPi(
            "info",
            `[herdsman-pi] wake inject deliverAs=followUp triggerTurn=true runActive=${String(state.runActive)} isIdle=${ctx.isIdle === undefined ? "unknown" : String(ctx.isIdle())} eventIds=${batchOutcomes[0]?.eventId ?? 0}-${batchOutcomes.at(-1)?.eventId ?? 0} count=${batchOutcomes.length}`,
          );
          try {
            pi.sendMessage?.(
              {
                content: wakeContent,
                customType: WAKE_CONTEXT_CUSTOM_TYPE,
                // Suppressed upstream errors are dropped from the injected
                // context, but every other pending id stays listed so the
                // evidence trail for the decision still names what was pending.
                details: {
                  eventIds: batchEvents
                    .filter((event) => !batchSuppressedIds.has(event.id))
                    .map((event) => event.id),
                  // The evidence channel: the ids whose content this message
                  // actually carries (`eventIds` above lists everything that was
                  // pending, including copies an earlier injection presented).
                  // Only these prove consumption when a run drains this message.
                  presentedEventIds: batchOutcomes.map((outcome) => outcome.eventId),
                },
                display: false,
              },
              // `triggerTurn: true` is what makes this a real turn: pi hands the
              // message to the agent core, which produces a regular `message_end`
              // for the hidden entry, and only that emission is the consumption
              // evidence the acknowledgement waits for. A `triggerTurn: false`
              // variant only parks the message in a follow-up queue that a run
              // drains at its stop point — the copy that made "busy delivery
              // failed" possible.
              { deliverAs: "followUp", triggerTurn: true },
            );
            // A freshly delivered id is in flight until its content is seen in
            // the transcript: it can neither be acknowledged (the evidence is
            // what authorises that) nor injected again (the content must not
            // enter the transcript twice). An id merged into this batch from an
            // earlier delivery keeps that same state; a dead-lettered one is in
            // flight again and loses its retry flag.
            for (const outcome of batchOutcomes) {
              state.wakeAwaitingConsumption.add(outcome.eventId);
              state.wakeRetryableEventIds.delete(outcome.eventId);
            }
            // Only expose the batch after the hidden context was accepted by pi. This
            // keeps an injection failure eligible for daemon redelivery.
            //
            // The batch is the delivery queue plus this injection: previously
            // unconfirmed events are merged (never replaced) so that a later
            // release always leaves both the old and the new events deliverable
            // and acknowledgeable in id order. The turn-consumption flags of a
            // still-running previous batch are carried over, because they
            // describe whether the content already reached the orchestrator.
            //
            // `hasSubstantiveWork` is the sole gate that decides whether an
            // ownership/scope change may abort the in-flight turn (see loseRole
            // and resetForScopeChange), and aborting is only ever allowed for a
            // *pure* Herdsman wake turn. The injected message starts one of those
            // (it is only ever sent to an idle orchestrator, and it triggers its
            // own turn), and nothing here has produced substance yet, so the
            // flag starts false and the tool/assistant hooks raise it.
            state.deliveredBatch = {
              abortedByUser: previousBatch?.abortedByUser ?? false,
              assistantFinalSucceeded: previousBatch?.assistantFinalSucceeded ?? false,
              events: mergeUnackedDelivered(incomingEvents),
              hasSubstantiveWork: previousBatch?.hasSubstantiveWork ?? false,
              invalidated: false,
              ownerTerminalId,
              herdsmanTriggered: true,
            };
            state.wakeRequested = false;
            state.wakeRequestedThroughEventId = 0;
            // The ids deliberately do *not* enter `presentedEventIds` here: that set
            // means "this content is in the transcript", and only consumption
            // evidence (`pi.on("message_end")` for this hidden wake message, see
            // the handler below) knows that. Recording the injection instead would
            // let an update whose run never carried it out — a failed turn, an
            // abort, a message Pi dropped — become unrecoverable in this session,
            // which is the lock-out this path is meant to remove. The in-flight
            // state is exactly what keeps it out of the transcript twice in the
            // meantime: `alreadyPresented` reports "awaiting".
          } catch {
            state.deliveredBatch = undefined;
            state.wakeRequested = false;
            // Nothing was sent, so nothing is awaiting consumption on its account
            // either: the ids only enter the awaiting set once Pi accepted the
            // message. Nothing was delivered, so the event stays pending and the
            // next wake retries it.
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
      if (!options.preservePresented) clearDeliveryBookkeeping();
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
      clearDeliveryBookkeeping();
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
          state.latestContext = next;
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
      if (message.role === "custom" && message.customType === WAKE_CONTEXT_CUSTOM_TYPE) {
        // Consumption evidence: the wake content reached the transcript, so the
        // turn that carried it out is confirmed. The ids the message presented are
        // now authorised for acknowledgement on their own, whatever conclusion the
        // turn reached.
        const consumedEventIds = wakeConsumedEventIds(message);
        if (consumedEventIds.length === 0) {
          // Nothing may be confirmed from a message whose evidence cannot be read
          // (a shape this extension never emits), and that must not pass silently:
          // the ids that are still waiting for their evidence must be named so the
          // stuck delivery is traceable.
          const awaiting = [...state.wakeAwaitingConsumption].sort((left, right) => left - right);
          logHerdsmanPi(
            "warn",
            `[herdsman-pi] wake consumption evidence unusable customType=${String(message.customType)} awaiting=${awaiting.length === 0 ? "none" : awaiting.join(",")} detailsKeys=${Object.keys(record(message.details)).join(",") || "none"} · no event confirmed by this message`,
          );
        }
        for (const eventId of consumedEventIds) {
          state.wakeAwaitingConsumption.delete(eventId);
          // The delivery is settled (carried out, or dead-lettered and now
          // re-delivered): it owes no further drive and no further retry.
          state.wakeRetryableEventIds.delete(eventId);
          // The content reached the transcript, so this id counts as presented from
          // now on: a daemon redelivery of it (it is still unacknowledged whenever
          // its batch was already dropped) must not inject the same update again.
          // The scope's own guard may be gone — the copy can be drained after a
          // role/scope reset cleared it — so the session-wide suppression set keeps
          // the id as well; it is released when the daemon confirms the id.
          state.presentedEventIds.add(eventId);
          state.wakeSuppressedEventIds.add(eventId);
          // Only an id the daemon can still be told about is worth remembering.
          if (state.unackedDelivered.has(eventId)) state.wakeConsumptionObserved.add(eventId);
        }
      }
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
      // Deliberately no clearing here: a starting run does drain Pi's follow-up
      // queue, but that is not evidence that the content reached the transcript —
      // a run can end in error or be aborted before its stop point and leave the
      // queue untouched. Only the consumption signal (the hidden wake message's
      // `message_end`) writes a delivery off, so an update a run did not take
      // along is still driven out at the next settlement instead of waiting for
      // the next user message.
      if (state.runActive) return;
      state.runActive = true;
    });

    pi.on("context", (event: { messages: PiAgentMessage[] }) => {
      // The wake content rides the single follow-up track and is never pinned
      // into the context here, so this hook injects nothing at all. What it
      // still does is drop herdsman's own context/wake entries from the
      // incoming list: a session replayed from disk can carry an old
      // `[HERDSMAN AGENT CONTEXT]` preview (its truncated one-line reports
      // repeatedly read as cut-off reports, and live board state is queried
      // through herdr), so those entries are filtered out instead of being
      // re-served to the model. The wake message itself
      // (`herdsman-wake-context`, the full report text) is not filtered - it is
      // the evidence the turn acts on.
      const messages = event.messages.filter((message) => !isNormalHerdsmanContext(message));
      return { messages };
    });

    pi.on("agent_settled", async (_event: unknown, ctx: PiContext) => {
      state.runActive = false;
      // The settlement is the signal a deferral has been waiting for, so the
      // pending spin is dropped: the wake decision below is taken now instead of
      // one `WAKE_BUSY_SPIN_MS` tick later. A pass that is already being injected
      // (`wakeRequested`) keeps its timer — clearing it would drop the injection.
      if (state.wakeTimer && !state.wakeRequested) {
        const pendingDeferral = state.wakeTimer;
        state.wakeTimer = undefined;
        clearTimeout(pendingDeferral);
      }
      // A wake whose content never reached the transcript gets no run of its own:
      // there is deliberately no empty-marker continuation. The settlement writes
      // those ids off locally (dead-letter, no acknowledgement) and the next
      // `scheduleWake` re-presents them with the real update content, or a later
      // settlement confirms them once their copy finally drains.
      const unconsumed = [...state.wakeAwaitingConsumption].sort((left, right) => left - right);
      if (unconsumed.length > 0) writeOffStrandedWakeDelivery(unconsumed, ctx);
      const failBatch = () => {
        ctx.ui.notify?.(
          "Herdsman couldn’t acknowledge agent updates · updates remain pending",
          "warning",
        );
      };
      const finishBatch = () => {
        state.ackInFlight = false;
        state.wakeDeferredUntilSettled = false;
        setHerdsmanUi(ctx);
        scheduleWake(ctx);
      };

      const batch = state.deliveredBatch;
      if (!batch) {
        state.wakeDeferredUntilSettled = false;
        // A queue can outlive its batch: the batch record is dropped as soon as
        // nothing awaits consumption, while an acknowledgement it still owed can be
        // missing — most visibly when the daemon was unreachable while the content
        // was consumed. Without this pass those ids would pin the watermark until an
        // unrelated event happened to form a new batch, and a session that receives
        // no further update would never confirm what its transcript already holds.
        // With no batch left, no turn can vouch for an untracked delivery, so only
        // consumption evidence confirms an id.
        const stranded = confirmableDeliveryPrefix(false);
        if (
          stranded.confirmablePrefix.length > 0 &&
          state.isOrchestrator &&
          state.client !== undefined &&
          state.connected
        ) {
          const resumeAckInFlight = state.ackInFlight;
          state.ackInFlight = true;
          try {
            await acknowledgeEventIds(stranded.confirmablePrefix, { notify: true }, ctx);
          } finally {
            state.ackInFlight = resumeAckInFlight;
          }
        }
        scheduleWake(ctx);
        return;
      }

      const stillOwner =
        state.isOrchestrator && state.currentScope?.terminalId === batch.ownerTerminalId;
      const { deliveryQueue, confirmablePrefix, blockedByMissingTurn } = confirmableDeliveryPrefix(
        batch.assistantFinalSucceeded || batch.abortedByUser,
      );
      const reachable = stillOwner && state.client !== undefined && state.connected;
      // Every id awaiting consumption was dead-lettered at the top of this
      // handler, so nothing holds the cursor open any more and the batch can be
      // dropped: a later wake is not gated by it.
      state.deliveredBatch = undefined;
      state.ackInFlight = true;
      if (!reachable) {
        // Unreachable (ownership gone, or a disconnect): attempting an
        // acknowledgement now would only be refused and would burn the event's
        // attempt budget, while the queue keeps every event for the next
        // settlement on a live connection.
        if (deliveryQueue.length > 0) failBatch();
        finishBatch();
        return;
      }
      if (blockedByMissingTurn) failBatch();
      if (confirmablePrefix.length === 0) {
        // Nothing to confirm: an empty queue (the batch is settled) or a prefix
        // blocked by unconsumed content, which must not be confirmed yet.
        finishBatch();
        return;
      }

      await acknowledgeEventIds(confirmablePrefix, { notify: true }, ctx);
      finishBatch();
    });

  };
}

export default createHerdsmanPiExtension();

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
    // Defensive for session replay: older versions pinned a queued wake to the
    // context hook under this customType. A replayed history may still carry
    // those entries, and they are herdsman's own, so they are dropped from the
    // incoming list like the agent-context ones (see the context hook).
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
  // Same newline-preserving scheme as `normalizeExcerpt` in wake.ts: which byte a
  // line break is (CRLF, CR, U+2028/U+2029, NEL) is normalised to LF, trailing
  // whitespace is dropped per line, 3+ newlines collapse to a blank line, and
  // leading indentation plus inline runs of whitespace are kept as they are.
  // NEL (U+0085) is excluded from the control-character clearing regex below,
  // which would otherwise delete it as an unprintable byte; the rest of that
  // regex is left intact.
  return stripVTControlCharacters(value)
    .replace(
      /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u0084\u0086-\u009f\u00ad\u180e\u200b-\u200f\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff]/g,
      "",
    )
    .replace(/\r\n?/g, "\n")
    .replace(/[\u2028\u2029\u0085]/g, "\n")
    .replace(/[^\S\n]+$/gm, "")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

function sanitizeAndCleanContextText(value: string): string {
  const preCleaned = cleanContextText(value);
  if (!preCleaned) return "";
  const sanitized = sanitizeText(preCleaned).text;
  return cleanContextText(sanitized);
}

