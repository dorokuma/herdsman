import { agentIdentityLabel } from "./agent-display.js";
import { appendFileSync, mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, isAbsolute, join } from "node:path";

export type HerdsmanPiLogLevel = "info" | "warn" | "error";

export function logHerdsmanPi(level: HerdsmanPiLogLevel, message: string): void {
  try {
    const configuredHome = process.env.HERDSMAN_HOME?.trim();
    const home = configuredHome && isAbsolute(configuredHome) ? configuredHome : join(homedir(), ".herdsman");
    const now = new Date();
    const date = now.toISOString().slice(0, 10).replaceAll("-", "");
    const file = join(home, "logs", `herdsman-pi-${date}.log`);
    mkdirSync(dirname(file), { recursive: true });
    appendFileSync(file, `${now.toISOString()} [${level}] ${message}\n`, "utf8");
  } catch {
    // Diagnostics must never write to the terminal or interrupt the extension.
  }
}

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
  wakeDeferredUntilSettled: boolean;
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
};

const DEFAULT_HOME_NAME = ".herdsman";
const COMMAND_USAGE = "Usage: /herdsman [on|off|status]";
const HERDR_REQUIRED_MESSAGE = "Herdsman requires a Herdr workspace";
const RECONNECTING_MESSAGE = "Herdsman is reconnecting · try again shortly";
export const MAX_ACK_ATTEMPTS = 5;
export const ACK_BACKOFF_CAP_MS = 30_000;

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
      wakeDeferredUntilSettled: false,
      wakeRequested: false,
      wakeRequestedThroughEventId: 0,
      wakeTimer: undefined,
      ackInFlight: false,
    };
    let activeContext: PiContext | undefined;
    let wakeGeneration = 0;

    const setHerdsmanUi = (ctx: PiContext | undefined) => {
      if (!ctx) return;
      const footerState: HerdsmanFooterState = state.reconnectingFromOn
        ? { kind: "reconnecting" }
        : state.isOrchestrator
          ? {
              kind: "on",
              updateCount: projectAgentOutcomes(state.pendingEvents).outcomes.length,
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
      state.pinnedContext = undefined;
      state.runActive = false;
    };

    const pruneAcknowledgedEvents = (ackedEventId: number | undefined) => {
      if (ackedEventId === undefined) return;
      state.pendingEvents = state.pendingEvents.filter((event) => event.id > ackedEventId);
      for (const presentedId of [...state.presentedEventIds]) {
        if (presentedId <= ackedEventId) state.presentedEventIds.delete(presentedId);
      }
    };

    const isWakeableEvent = (event: AgentEventWireRecord | undefined) =>
      !event?.nextAttemptAt || event.nextAttemptAt <= Date.now();

    const applyOwnerContext = (response: ConnectionStateResponse) => {
      state.latestContext = isLocalOwner(response) ? response.context ?? undefined : undefined;
    };

    const scheduleWake = (ctx: PiContext | undefined) => {
      if (!ctx || !state.isOrchestrator || !state.currentScope || !pi.sendMessage) return;
      if (state.wakeTimer || state.wakeRequested) return;
      const outcomes = projectAgentOutcomes(state.pendingEvents).outcomes.filter(
        (outcome) =>
          outcome.eventId > state.failedWakeThroughEventId &&
          !state.presentedEventIds.has(outcome.eventId),
      );

      const wakeable = outcomes.filter((outcome) =>
        isWakeableEvent(state.pendingEvents.find((pending) => pending.id === outcome.eventId)),
      );
      if (wakeable.length === 0) {
        const nextAttemptAt = outcomes
          .map((outcome) => state.pendingEvents.find((event) => event.id === outcome.eventId)?.nextAttemptAt)
          .filter((value): value is number => value !== undefined)
          .sort((left, right) => left - right)[0];
        if (nextAttemptAt !== undefined) {
          state.wakeTimer = setTimeout(() => {
            state.wakeTimer = undefined;
            scheduleWake(ctx);
          }, Math.max(0, nextAttemptAt - Date.now()));
        }
        return;
      }

      if (state.deliveredBatch || state.ackInFlight || ctx.isIdle?.() === false) {
        state.wakeDeferredUntilSettled = true;
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
          if (ctx.isIdle?.() === false) {
            state.wakeTimer = undefined;
            state.wakeDeferredUntilSettled = true;
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
            state.wakeDeferredUntilSettled = true;
            return;
          }

          const batchEvents = [...state.pendingEvents].sort((left, right) => left.id - right.id);
          const batchOutcomes = projectAgentOutcomes(batchEvents).outcomes.filter(
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
          const deliveredBatch: DeliveredBatch = {
            abortedByUser: false,
            assistantFinalSucceeded: false,
            events: batchEvents.filter((event) =>
              batchOutcomes.some((outcome) => outcome.eventId === event.id),
            ),
            hasSubstantiveWork: false,
            invalidated: false,
            ownerTerminalId,
            herdsmanTriggered: true,
          };
          state.wakeTimer = undefined;
          state.wakeRequested = true;
          state.wakeRequestedThroughEventId = current.at(-1)?.eventId ?? 0;
          try {
            pi.sendMessage?.(
              {
                content: formatAgentOutcomeUpdates(batchOutcomes),
                customType: "herdsman-wake-context",
                details: { eventIds: batchEvents.map((event) => event.id) },
                display: false,
              },
              { deliverAs: "followUp", triggerTurn: true },
            );
            // Only expose the batch after the hidden context was accepted by pi. This
            // keeps an injection failure eligible for daemon redelivery.
            state.deliveredBatch = deliveredBatch;
            state.wakeRequested = false;
            state.wakeRequestedThroughEventId = 0;
            // Record the presentation so a reclaim redelivery of the same id is
            // not presented twice; the ids leave this set when the events leave
            // pendingEvents (ack, terminal failure, dead-letter, role loss,
            // scope change, shutdown) or become retryable again after a failed
            // acknowledgement.
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

    const loseRole = (ctx: PiContext | undefined, options: { abort?: boolean } = {}) => {
      if (state.deliveredBatch) {
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
      state.presentedEventIds.clear();
      state.reconnectingFromOn = false;
      setHerdsmanUi(ctx);
    };

    const markDisconnected = (ctx: PiContext | undefined) => {
      const reconnectingFromOn = state.reconnectingFromOn || state.isOrchestrator;
      loseRole(ctx, { abort: false });
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
      if (state.deliveredBatch?.invalidated) {
        // A transient disconnect invalidated the previous batch without acking
        // it. Clear it here so still-pending events are re-woken on this fresh
        // connection instead of being gated forever; already acked events are
        // covered by the server cursor (pruneAcknowledgedEvents below).
        state.deliveredBatch = undefined;
      }
      applyOwnerContext(response);
      setHerdsmanUi(ctx);
      addPendingEvents(response.events ?? [], ctx);
      pruneAcknowledgedEvents(response.state?.ackedEventId ?? response.ackedEventId);
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
                  agents: snapshot.agents.filter((agent) => {
                    const nextAgents = next?.agents.filter((candidate) => candidate.paneId === agent.paneId) ?? [];
                    if (nextAgents.length === 0) return false;
                    return nextAgents.some(
                      (nextAgent) => !agent.id || !nextAgent.id || agent.id === nextAgent.id,
                    );
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
          applyConnectionStateResponse(response as ConnectionStateResponse, ctx, {
            notifyReconnectLoss: true,
          });
        })
        .catch((error) => {
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
      state.client?.close();
      const client = options.clientFactory?.() ?? new ReconnectingDaemonClient({ socketPath: defaultSocketPath() });
      client.resetForSession?.();
      state.client = client;
      client.onConnected = () => registerPresence(ctx);
      client.onDisconnected = () => {
        state.connected = false;
        markDisconnected(activeContext);
      };
      client.onStreamMessage = handleStreamMessage;
    });

    pi.on("session_shutdown", () => {
      state.connected = false;
      loseRole(activeContext);
      state.deliveredBatch = undefined;
      state.presentedEventIds.clear();
      state.client?.close();
      state.client = undefined;
      activeContext = undefined;
    });

    const assistantMessageText = (message: Record<string, unknown>): string => {
      const content = message.content;
      if (typeof content === "string") return content;
      if (!Array.isArray(content)) return "";
      const parts: string[] = [];
      for (const block of content) {
        if (typeof block === "string") {
          if (block.length > 0) parts.push(block);
          continue;
        }
        const value = record(block);
        if (typeof value.text === "string" && value.text.length > 0) parts.push(value.text);
      }
      return parts.join("\n");
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
          await client.request("agent.turn.completed", {
            confirmed: check.confirmed,
            herdrSessionName: scope.herdrSessionName,
            paneId: scope.paneId,
            terminalId: scope.terminalId,
            workspaceId: scope.workspaceId,
          });
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
      const snapshot = state.pinnedContext;
      if (!snapshot || snapshot.agents.length === 0) return { messages };
      return {
        messages: [
          ...messages,
          {
            content: formatHiddenAgentContext({
              agents: snapshot.agents,
              workspaceId: snapshot.workspaceId,
            }),
            customType: "herdsman-agent-context",
            display: false,
            role: "custom",
            timestamp: Date.now(),
          },
        ],
      };
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
        setHerdsmanUi(ctx);
        scheduleWake(ctx);
      };

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

      for (const event of batch.events) {
        try {
          const ackResponse = (await state.client.request("agent.notifications.ack", {
            eventId: event.id,
          })) as { ackedEventId?: number; state?: { ackedEventId?: number } };
          pruneAcknowledgedEvents(ackResponse?.ackedEventId ?? ackResponse?.state?.ackedEventId);
          state.pendingEvents = state.pendingEvents.filter((pending) => pending.id !== event.id);
          state.presentedEventIds.delete(event.id);
          state.failedWakeThroughEventId = Math.max(state.failedWakeThroughEventId, event.id);
          setHerdsmanUi(ctx);
        } catch (error) {
          const failureCode = ackFailureCode(error);
          const classification = classifyAckFailure(error);
          const attempts = (event.attempts ?? 0) + 1;
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

          if (classification === "terminal") {
            state.pendingEvents = state.pendingEvents.filter((pending) => pending.id !== event.id);
            state.presentedEventIds.delete(event.id);
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
            state.presentedEventIds.delete(event.id);
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
              const response = (await state.client.request(
                "agent.orchestrator.get",
                {},
              )) as ConnectionStateResponse;
              // Refresh pending data without applying the full connection response: that
              // helper schedules a new wake, which would make this failed batch race
              // with the current settlement and can replay an earlier event. The
              // failed event remains pending and the next wake is scheduled by
              // finishBatch(), so this round performs no additional acknowledgements.
              addPendingEvents(response.events ?? [], ctx);
              pruneAcknowledgedEvents(response.state?.ackedEventId ?? response.ackedEventId);
              setHerdsmanUi(ctx);
            } catch (resyncError) {
              logHerdsmanPi(
                "warn",
                `[herdsman-pi] acknowledgement resync failed eventId=${event.id} attempts=${attempts} code=${ackFailureCode(resyncError)}`,
              );
            }
            // The event stays pending with a backoff and is eligible for a
            // later re-presentation; continue so one failed event does not
            // block the rest of the batch.
            state.presentedEventIds.delete(event.id);
            continue;
          }

          state.pendingEvents = state.pendingEvents.map((pending) =>
            pending.id === event.id
              ? { ...pending, nextAttemptAt: attemptedAt + ackBackoffMs(attempts) }
              : pending,
          );
          ctx.ui.notify?.(
            "Herdsman couldn’t acknowledge agent updates · updates remain pending",
            "warning",
          );
          setHerdsmanUi(ctx);
          // The event stays pending with a backoff and is eligible for a later
          // re-presentation; continue so one failed event does not block the
          // rest of the batch.
          state.presentedEventIds.delete(event.id);
          continue;
        }
      }
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
      return [
        `- ${identity} ${agent.paneId ?? "unknown"} ${agent.agentStatus ?? "unknown"}`,
        `  last user: ${oneLine(history.lastUserMessage?.text ?? "")}`,
        `  last assistant: ${oneLine(history.lastAssistantMessage?.text ?? "")}`,
      ].join("\n");
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
        `  last assistant: ${oneLine(history.lastAssistantMessage?.text ?? "")}`,
        `  event: ${event.id}`,
      ].join("\n");
    }),
  ].join("\n");
}

function isNormalHerdsmanContext(message: PiAgentMessage): boolean {
  return (
    message.customType === "herdsman-agent-context" ||
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

function oneLine(value: string): string {
  return value.replace(/\s+/g, " ");
}
