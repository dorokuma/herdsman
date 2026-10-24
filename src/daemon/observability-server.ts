import { chmodSync, existsSync, unlinkSync } from "node:fs";
import { createServer, type Server, type Socket } from "node:net";
import { Value } from "@sinclair/typebox/value";
import type { AgentHistoryService } from "@/agent-history/service.js";
import { defaultConnectSocket } from "@/daemon/process-manager.js";
import type { AgentEventStore } from "@/db/agent-events.js";
import type { AgentStore } from "@/db/agents.js";
import type { HerdrSessionStore } from "@/db/herdr-sessions.js";
import type { HerdrWorkspaceStore } from "@/db/herdr-workspaces.js";
import {
  type HerdrPaneIdentity,
  resolveHerdrPaneIdentity,
} from "@/herdr/pane-identity-resolver.js";
import type { AgentContextService } from "@/observability/agent-context-service.js";
import type { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import type {
  AgentEventRecord,
  AgentIndexRecord,
  AgentOrchestratorChanged,
  AgentOrchestratorState,
  AgentOrchestratorWireState,
  AgentQueryScope,
  AgentScope,
  AgentTurnCompletedInput,
  AgentWorkspaceContextSnapshot,
  PiPresenceRegistration,
} from "@/observability/contracts.js";
import { OrchestratorAckError } from "@/observability/errors.js";
import {
  agentEventsInputSchema,
  agentGetInputSchema,
  agentListInputSchema,
  agentOrchestratorAckInputSchema,
  agentOrchestratorGetInputSchema,
  agentOrchestratorRegisterInputSchema,
  agentOrchestratorSetInputSchema,
  agentPingInputSchema,
  agentReadInputSchema,
  agentTurnCompletedInputSchema,
} from "@/observability/schemas.js";
import { TurnCompletionRegistry } from "@/observability/turn-completion.js";
import {
  encodeJsonLine,
  JsonLineDecoder,
  JsonLineFrameTooLargeError,
} from "@/shared/json-lines.js";
import { linuxPeerPid, peerBoundToPaneCwd } from "@/shared/unix-peercred.js";

export const DISCONNECT_GRACE_MS = 5_000;
export const STARTUP_RECONNECT_GRACE_MS = 10_000;
export const TERMINAL_HEARTBEAT_TIMEOUT_MS = 90_000;
export const TERMINAL_HEARTBEAT_SCAN_INTERVAL_MS = 30_000;

type RpcRequest = { id?: number | string; method?: string; params?: unknown };

type AgentStores = {
  agentEvents: AgentEventStore;
  agents: AgentStore;
  herdrSessions: HerdrSessionStore;
  herdrWorkspaces: HerdrWorkspaceStore;
};

export type PiPresence = AgentScope & {
  connectedAt: number;
  paneId: string;
  subscriberId: string;
  subscriberKind?: "pi" | "agy";
  terminalId: string;
};

type AgentOrchestratorConnectionStateResult = {
  context: AgentWorkspaceContextSnapshot | null;
  events: AgentEventRecord[];
  presence: PiPresence;
  state: AgentOrchestratorWireState | null;
};

type TimerHandle = ReturnType<typeof setTimeout>;
type IntervalHandle = ReturnType<typeof setInterval>;

type GraceTimer = {
  handle: TimerHandle;
};

type AgyTerminalLease = {
  paneGeneration: string | null;
  presence: PiPresence;
};

export class ObservabilityRpcServer {
  readonly #clearInterval: (handle: IntervalHandle) => void;
  readonly #setInterval: (callback: () => void, delay: number) => IntervalHandle;
  readonly #clearTimeout: (handle: TimerHandle) => void;
  readonly #connectSocket: (socketPath: string) => Promise<boolean>;
  readonly #context: AgentContextService;
  readonly #connectionOrderBySocket = new Map<Socket, number>();
  readonly #publishedContextFingerprintByScope = new Map<string, string>();
  readonly #disconnectGraceMs: number;
  readonly #disconnectTimers = new Map<string, GraceTimer>();
  readonly #history: AgentHistoryService;
  readonly #heartbeatScanIntervalMs: number;
  readonly #heartbeatTimeoutMs: number;
  readonly #lastActivityByTerminal = new Map<string, number>();
  #heartbeatTimer: IntervalHandle | undefined;
  readonly #now: () => number;
  readonly #orchestrator: AgentOrchestratorService;
  readonly #peerPidOf: (socket: Socket) => number | undefined;
  readonly #agyPresenceByTerminal = new Map<string, AgyTerminalLease>();
  readonly #piPresenceBySocket = new Map<Socket, PiPresence>();
  readonly #socketsByTerminal = new Map<string, Set<Socket>>();
  readonly #registerPiSessionRef: (input: {
    herdrSessionName: string;
    sessionRef: PiPresenceRegistration["sessionRef"];
    terminalId: string;
  }) => Promise<{ contextChangedScopes: AgentScope[] }>;
  readonly #resolvePaneIdentity: (input: {
    paneId: string;
    socketPath: string;
  }) => Promise<HerdrPaneIdentity>;
  readonly #server: Server;
  readonly #setTimeout: (callback: () => void, delay: number) => TimerHandle;
  readonly #socketPath: string;
  readonly #sockets = new Set<Socket>();
  readonly #startupReconnectGraceMs: number;
  readonly #startupTimers = new Map<string, GraceTimer>();
  readonly #stores: AgentStores;
  readonly #turnCompletions: TurnCompletionRegistry;
  #connectionSequence = 0;
  #stopping = false;

  constructor(options: {
    clearInterval?: (handle: IntervalHandle) => void;
    setInterval?: (callback: () => void, delay: number) => IntervalHandle;
    clearTimeout?: (handle: TimerHandle) => void;
    connectSocket?: (socketPath: string) => Promise<boolean>;
    context: AgentContextService;
    disconnectGraceMs?: number;
    heartbeatScanIntervalMs?: number;
    heartbeatTimeoutMs?: number;
    history: AgentHistoryService;
    now?: () => number;
    orchestrator: AgentOrchestratorService;
    peerPidOf?: (socket: Socket) => number | undefined;
    registerPiSessionRef?: (input: {
      herdrSessionName: string;
      sessionRef: PiPresenceRegistration["sessionRef"];
      terminalId: string;
    }) => Promise<{ contextChangedScopes: AgentScope[] }>;
    resolvePaneIdentity?: (input: {
      paneId: string;
      socketPath: string;
    }) => Promise<HerdrPaneIdentity>;
    setTimeout?: (callback: () => void, delay: number) => TimerHandle;
    socketPath: string;
    startupReconnectGraceMs?: number;
    connectedTerminal?: (input: { herdrSessionName: string; terminalId: string }) => boolean;
    stores: AgentStores;
    turnCompletions?: TurnCompletionRegistry;
  }) {
    this.#clearInterval = options.clearInterval ?? clearInterval;
    this.#setInterval = options.setInterval ?? setInterval;
    this.#clearTimeout = options.clearTimeout ?? clearTimeout;
    this.#connectSocket = options.connectSocket ?? defaultConnectSocket;
    this.#context = options.context;
    this.#disconnectGraceMs = options.disconnectGraceMs ?? DISCONNECT_GRACE_MS;
    this.#heartbeatScanIntervalMs =
      options.heartbeatScanIntervalMs ?? TERMINAL_HEARTBEAT_SCAN_INTERVAL_MS;
    this.#heartbeatTimeoutMs = options.heartbeatTimeoutMs ?? TERMINAL_HEARTBEAT_TIMEOUT_MS;
    this.#history = options.history;
    this.#now = options.now ?? Date.now;
    this.#orchestrator = options.orchestrator;
    this.#peerPidOf = options.peerPidOf ?? linuxPeerPid;
    this.#registerPiSessionRef =
      options.registerPiSessionRef ?? (async () => ({ contextChangedScopes: [] }));
    this.#resolvePaneIdentity = options.resolvePaneIdentity ?? resolveHerdrPaneIdentity;
    this.#setTimeout = options.setTimeout ?? setTimeout;
    this.#socketPath = options.socketPath;
    this.#startupReconnectGraceMs = options.startupReconnectGraceMs ?? STARTUP_RECONNECT_GRACE_MS;
    this.#stores = options.stores;
    this.#turnCompletions = options.turnCompletions ?? new TurnCompletionRegistry();
    this.#server = createServer((socket) => this.#handleConnection(socket));
  }

  async start(): Promise<void> {
    this.#stopping = false;
    if (existsSync(this.#socketPath)) {
      if (await this.#connectSocket(this.#socketPath)) {
        throw new Error(`Herdsman daemon socket is already reachable: ${this.#socketPath}`);
      }
      unlinkSync(this.#socketPath);
    }
    process.umask(0o077);
    await new Promise<void>((resolve, reject) => {
      this.#server.once("error", reject);
      this.#server.listen(this.#socketPath, () => {
        chmodSync(this.#socketPath, 0o600);
        this.#server.off("error", reject);
        resolve();
      });
    });
    this.#heartbeatTimer = this.#setInterval(
      () => this.#scanHeartbeats(),
      this.#heartbeatScanIntervalMs,
    );
    this.#armStartupGrace();
  }

  async stop(): Promise<void> {
    this.#stopping = true;
    if (this.#heartbeatTimer) {
      this.#clearInterval(this.#heartbeatTimer);
      this.#heartbeatTimer = undefined;
    }
    this.#clearGraceTimers(this.#disconnectTimers);
    this.#clearGraceTimers(this.#startupTimers);
    for (const socket of this.#sockets) socket.destroy();
    this.#sockets.clear();
    this.#agyPresenceByTerminal.clear();
    this.#piPresenceBySocket.clear();
    this.#socketsByTerminal.clear();
    this.#connectionOrderBySocket.clear();
    const wasListening = this.#server.listening;
    await new Promise<void>((resolve, reject) => {
      if (!wasListening) {
        resolve();
        return;
      }
      this.#server.close((error) => (error ? reject(error) : resolve()));
    });
    if (wasListening && existsSync(this.#socketPath)) unlinkSync(this.#socketPath);
  }

  publishAgentContext(scope: AgentScope): void {
    const owner = this.#orchestrator.status(scope)?.owner;
    if (!owner) return;
    const socket = this.#newestSocketForTerminal({ ...scope, terminalId: owner.terminalId });
    if (!socket) return;
    const context = this.#context.workspaceSnapshot({
      ...scope,
      excludeTerminalId: owner.terminalId,
    });
    const contextFingerprint = JSON.stringify(context);
    const scopeKey = `${scope.herdrSessionName}\0${scope.workspaceId}`;
    if (this.#publishedContextFingerprintByScope.get(scopeKey) === contextFingerprint) return;
    this.#publishedContextFingerprintByScope.set(scopeKey, contextFingerprint);
    this.#write(socket, {
      method: "agent.context.changed",
      params: { context, herdrSessionName: scope.herdrSessionName, workspaceId: scope.workspaceId },
    });
  }

  publishAgentEvent(event: AgentEventRecord): void {
    // `agentId` is intentionally not required: a terminal event written after the
    // agents row was physically removed still has to reach the orchestrator. The
    // workspace/terminal scope is required, because without it the event cannot be
    // routed at all.
    if (!event.workspaceId || !event.terminalId) {
      // Dead letter: accepted as permanently undeliverable, but never silently.
      if (event.type === "agent.failed" || event.type === "agent.discarded") {
        console.warn("Herdsman terminal agent event has no deliverable scope (dead letter)", {
          eventId: event.id,
          herdrSessionName: event.herdrSessionName,
          terminalId: event.terminalId,
          type: event.type,
          workspaceId: event.workspaceId,
        });
      }
      return;
    }
    // Status changes are persisted for history but never routed as worker wake events.
    if (event.type === "agent.status.changed") return;
    if ((event.status !== "pending" && event.status !== "delivered") || event.deliverable !== 1) {
      return;
    }
    const scope = { herdrSessionName: event.herdrSessionName, workspaceId: event.workspaceId };
    const owner = this.#orchestrator.status(scope)?.owner;
    if (!owner) {
      // An ownerless workspace (cross-workspace dispatched worker) has no route of
      // its own: hand its event to the session's single active orchestrator.
      const sessionOwner = this.#singleActiveSessionOwner(
        event.herdrSessionName,
        event.workspaceId,
      );
      if (!sessionOwner) return;
      const socket = this.#newestSocketForSessionTerminal(
        event.herdrSessionName,
        sessionOwner.terminalId,
      );
      if (socket) this.#write(socket, { method: "agent.event", params: { event } });
      return;
    }
    if (event.terminalId === owner.terminalId) return;
    const socket = this.#newestSocketForTerminal({ ...scope, terminalId: owner.terminalId });
    if (socket) this.#write(socket, { method: "agent.event", params: { event } });
  }

  /**
   * The single active orchestrator of a Herdr session, if there is exactly one.
   * With several owners the ownerless workspace's event stays pull-only (the
   * owner still receives it through `pending()`), so no arbitrary owner is woken.
   */
  #singleActiveSessionOwner(
    herdrSessionName: string,
    eventWorkspaceId: string,
  ): { terminalId: string } | undefined {
    const terminalIds = new Set(
      this.#orchestrator
        .persistedOwners()
        .filter(
          (state) =>
            state.herdrSessionName === herdrSessionName &&
            state.workspaceId !== eventWorkspaceId &&
            state.owner !== null &&
            this.#hasTerminalPresence({
              herdrSessionName,
              terminalId: state.owner.terminalId,
            }),
        )
        .flatMap((state) => (state.owner ? [state.owner.terminalId] : [])),
    );
    if (terminalIds.size !== 1) return undefined;
    const [terminalId] = [...terminalIds];
    return terminalId ? { terminalId } : undefined;
  }

  #newestSocketForSessionTerminal(
    herdrSessionName: string,
    terminalId: string,
  ): Socket | undefined {
    let newest: { order: number; socket: Socket } | undefined;
    for (const [socket, presence] of this.#piPresenceBySocket) {
      if (
        presence.herdrSessionName !== herdrSessionName ||
        presence.terminalId !== terminalId ||
        socket.destroyed
      ) {
        continue;
      }
      const order = this.#connectionOrderBySocket.get(socket) ?? 0;
      if (!newest || order > newest.order) newest = { order, socket };
    }
    return newest?.socket;
  }

  reconcileAgentLocations(input: { agents: AgentIndexRecord[]; herdrSessionName: string }): void {
    const byTerminal = new Map(
      input.agents.flatMap((agent) =>
        agent.terminalId ? ([[agent.terminalId, agent]] as const) : [],
      ),
    );
    const owners = this.#orchestrator
      .persistedOwners()
      .filter((state) => state.herdrSessionName === input.herdrSessionName);

    for (const [socket, presence] of this.#piPresenceBySocket) {
      if (presence.herdrSessionName !== input.herdrSessionName) continue;
      const agent = byTerminal.get(presence.terminalId);
      // Missing terminal owners are retained until an explicit disconnect/release; this refresh only relocates known agents.
      if (!agent) continue;
      this.#piPresenceBySocket.set(socket, {
        ...presence,
        paneId: agent.paneId,
        workspaceId: agent.workspaceId,
      });
    }

    for (const [key, lease] of this.#agyPresenceByTerminal) {
      const { presence } = lease;
      if (presence.herdrSessionName !== input.herdrSessionName) continue;
      const agent = byTerminal.get(presence.terminalId);
      if (!agent) {
        if (
          this.#stores.agents.isPaneClosedSince({
            herdrSessionName: presence.herdrSessionName,
            paneId: presence.paneId,
            paneGeneration: lease.paneGeneration,
            sinceMs: presence.connectedAt,
          })
        ) {
          this.#agyPresenceByTerminal.delete(key);
          if ((this.#socketsByTerminal.get(key)?.size ?? 0) === 0) {
            this.#releaseCurrentOwnersForTerminal({
              herdrSessionName: input.herdrSessionName,
              reason: "disconnected",
              terminalId: presence.terminalId,
            });
          }
        }
        continue;
      }
      if (agent.agent !== null && agent.agent !== "agy") {
        this.#agyPresenceByTerminal.delete(key);
        if ((this.#socketsByTerminal.get(key)?.size ?? 0) === 0) {
          this.#releaseCurrentOwnersForTerminal({
            herdrSessionName: input.herdrSessionName,
            reason: "disconnected",
            terminalId: presence.terminalId,
          });
        }
        continue;
      }
      this.#agyPresenceByTerminal.set(key, {
        paneGeneration: agent.paneGeneration ?? lease.paneGeneration,
        presence: {
          ...presence,
          paneId: agent.paneId,
          workspaceId: agent.workspaceId,
        },
      });
    }

    for (const ownerState of owners) {
      const owner = ownerState.owner;
      if (!owner) continue;
      const current = this.#orchestrator.status(ownerState);
      if (current?.owner?.terminalId !== owner.terminalId) continue;
      const agent = byTerminal.get(owner.terminalId);
      // Missing terminal owners are retained until an explicit disconnect/release; this refresh only relocates known agents.
      if (!agent) continue;
      if (agent.workspaceId === ownerState.workspaceId) {
        if (agent.paneId === owner.paneId) continue;
        const change = this.#orchestrator.claim({
          ...ownerState,
          paneId: agent.paneId,
          terminalId: owner.terminalId,
        });
        this.#publishOrchestratorChange(toWireChange({ ...change, reason: "moved" }));
        continue;
      }
      const changes = this.#orchestrator.move({
        from: ownerState,
        paneId: agent.paneId,
        terminalId: owner.terminalId,
        to: {
          herdrSessionName: input.herdrSessionName,
          workspaceId: agent.workspaceId,
        },
      });
      for (const change of changes) {
        this.#publishOrchestratorChange(toWireChange(change));
      }
    }
  }

  #handleConnection(socket: Socket): void {
    this.#sockets.add(socket);
    this.#connectionSequence += 1;
    this.#connectionOrderBySocket.set(socket, this.#connectionSequence);
    const decoder = new JsonLineDecoder();
    socket.on("data", (chunk) => {
      try {
        for (const message of decoder.push(chunk.toString("utf8"))) {
          this.#markTerminalActivity(socket);
          void this.#handleRequest(socket, message as RpcRequest);
        }
      } catch (error) {
        if (error instanceof JsonLineFrameTooLargeError) {
          console.warn(`Closing observability connection: ${error.message}`);
        }
        socket.destroy();
      }
    });
    socket.on("close", () => this.#handleSocketClose(socket));
    socket.on("error", () => undefined);
  }

  async #handleRequest(socket: Socket, request: RpcRequest): Promise<void> {
    try {
      if (!request.method) throw new Error("Missing method");
      const result = await this.#dispatch(socket, request.method, request.params ?? {});
      this.#write(socket, { id: request.id, result });
    } catch (error) {
      this.#write(socket, {
        error: {
          ...(error instanceof OrchestratorAckError
            ? { code: error.code, retryable: error.retryable }
            : {}),
          message: error instanceof Error ? error.message : String(error),
        },
        id: request.id,
      });
    }
  }

  async #dispatch(socket: Socket, method: string, params: unknown): Promise<unknown> {
    switch (method) {
      case "agent.list": {
        assertSchema(agentListInputSchema, params);
        const input = params as AgentQueryScope & { includeOwnerlessWorkspaces?: boolean };
        const scope = this.#resolveScope(input);
        if (input.includeOwnerlessWorkspaces && scope.herdrSessionName && scope.workspaceId) {
          const ownScope = {
            herdrSessionName: scope.herdrSessionName,
            workspaceId: scope.workspaceId,
          };
          const siblingScopes = this.#orchestrator.ownerlessSiblingScopes(ownScope);
          return {
            agents: [
              ...this.#context.listAgents(scope),
              ...siblingScopes.flatMap((sibling) => this.#context.listAgents(sibling)),
            ],
          };
        }
        return { agents: this.#context.listAgents(scope) };
      }
      case "agent.get": {
        assertSchema(agentGetInputSchema, params);
        const input = params as AgentQueryScope & { target: string };
        const scope = this.#resolveScope(input);
        const agent = this.#stores.agents.resolveTarget(scope, input.target);
        // An explicit read is an operator action, not the daemon's background
        // cadence: it re-resolves from the official values so a lookup that just
        // failed (and is therefore memoized for the background rounds) cannot
        // answer this call with an empty history.
        const history = await this.#history.resolveCompactHistory(
          this.#context.historyLookupInput(agent),
          { forceDiscovery: true },
        );
        return { agent: { ...agent, history: history.compactHistory } };
      }
      case "agent.read": {
        assertSchema(agentReadInputSchema, params);
        const input = params as AgentQueryScope & { limit?: number; target: string };
        const scope = this.#resolveScope(input);
        const agent = this.#stores.agents.resolveTarget(scope, input.target);
        const read = await this.#history.read(this.#context.historyLookupInput(agent), {
          forceDiscovery: true,
          limit: input.limit ?? 20,
        });
        return { agent: { ...agent, historyRef: read.historyRef, messages: read.messages } };
      }
      case "agent.events": {
        assertSchema(agentEventsInputSchema, params);
        const input = params as AgentQueryScope & { afterEventId?: number; limit?: number };
        return { events: this.#stores.agentEvents.listAfter(input) };
      }
      case "agent.orchestrator.register": {
        assertSchema(agentOrchestratorRegisterInputSchema, params);
        const registrationInput = params as PiPresenceRegistration;
        const previous = this.#piPresenceBySocket.get(socket);
        const presence = await this.#resolvePiPresence(socket, registrationInput);
        if (previous) this.#unregisterTerminalSocket(socket, previous);
        this.#piPresenceBySocket.set(socket, presence);
        this.#registerTerminalSocket(socket, presence);
        const presenceKey = terminalPresenceKey(presence);
        const currentOwner = this.#orchestrator.status(presence)?.owner;
        if (presence.subscriberKind !== "agy") {
          this.#agyPresenceByTerminal.delete(presenceKey);
          this.#cancelGraceForTerminal(presence);
        } else if (
          currentOwner?.terminalId === presence.terminalId &&
          !this.#isPiTerminal(presence)
        ) {
          this.#agyPresenceByTerminal.set(presenceKey, {
            paneGeneration: this.#resolvePaneGeneration(presence),
            presence,
          });
          this.#cancelGraceForTerminal(presence);
        }
        this.#lastActivityByTerminal.set(presenceKey, this.#now());
        if (
          previous &&
          terminalPresenceKey(previous) !== presenceKey &&
          previous.subscriberKind !== "agy"
        ) {
          this.#scheduleDisconnect(previous);
        }
        if (
          presence.subscriberKind !== "agy" ||
          (registrationInput.sessionRef.kind === "id" &&
            registrationInput.sessionRef.source !== "agy-subagent" &&
            isUuidLikeSessionId(registrationInput.sessionRef.value) &&
            !this.#isPiTerminal(presence))
        ) {
          const registration = await this.#registerPiSessionRef({
            herdrSessionName: presence.herdrSessionName,
            sessionRef: registrationInput.sessionRef,
            terminalId: presence.terminalId,
          });
          for (const scope of registration.contextChangedScopes) this.publishAgentContext(scope);
        }
        return this.#connectionState(presence);
      }
      case "agent.orchestrator.get": {
        assertSchema(agentOrchestratorGetInputSchema, params);
        return this.#connectionState(this.#requirePiPresence(socket));
      }
      case "agent.orchestrator.set": {
        assertSchema(agentOrchestratorSetInputSchema, params);
        const presence = this.#requirePiPresence(socket);
        const enabled = (params as { enabled: boolean }).enabled;
        let changed = false;
        if (enabled) {
          const current = this.#orchestrator.status(presence);
          const ownerConnected = current?.owner
            ? this.#hasTerminalPresence({ ...presence, terminalId: current.owner.terminalId })
            : false;
          const change = this.#orchestrator.claim({ ...presence, ownerConnected });
          if (presence.subscriberKind === "agy" && !this.#isPiTerminal(presence)) {
            this.#agyPresenceByTerminal.set(terminalPresenceKey(presence), {
              paneGeneration: this.#resolvePaneGeneration(presence),
              presence,
            });
            this.#cancelGraceForTerminal(presence);
          }
          changed = !sameOwner(change.current.owner, change.previous.owner);
          if (changed) this.#publishOrchestratorChange(toWireChange(change));
        } else {
          this.#agyPresenceByTerminal.delete(terminalPresenceKey(presence));
          const change = this.#orchestrator.release({
            ...presence,
            reason: "released",
          });
          changed = change !== undefined;
          if (change) this.#publishOrchestratorChange(toWireChange(change));
        }
        return { ...this.#connectionState(presence), changed };
      }
      case "agent.notifications.ack": {
        assertSchema(agentOrchestratorAckInputSchema, params);
        const presence = this.#requirePiPresence(socket);
        const state = this.#orchestrator.ack({
          ...presence,
          eventId: (params as { eventId: number }).eventId,
        });
        return { acknowledged: true, state: toWireState(state) };
      }
      case "agent.turn.completed": {
        assertSchema(agentTurnCompletedInputSchema, params);
        const presence = this.#requirePiPresence(socket);
        const input = params as AgentTurnCompletedInput;
        this.#turnCompletions.record({
          confirmed: input.confirmed,
          ...(input.expectedText ? { expectedText: input.expectedText } : {}),
          herdrSessionName: presence.herdrSessionName,
          paneId: presence.paneId,
          terminalId: presence.terminalId,
          workspaceId: input.workspaceId,
        });
        return { accepted: true };
      }
      case "agent.ping": {
        assertSchema(agentPingInputSchema, params);
        return { ok: true };
      }
      default:
        throw new Error(`Unknown method: ${method}`);
    }
  }

  async #resolvePiPresence(socket: Socket, input: PiPresenceRegistration): Promise<PiPresence> {
    const session = this.#stores.herdrSessions.findRunningBySocketPath(input.herdrSocketPath);
    if (!session) throw new Error("Herdr socket is not registered as a running session");
    const indexed = this.#stores.agents.findByPane({
      herdrSessionName: session.name,
      paneId: input.paneId,
    });
    if (indexed?.workspaceId === input.workspaceId && indexed.terminalId) {
      const presence: PiPresence = {
        connectedAt: this.#now(),
        herdrSessionName: session.name,
        paneId: indexed.paneId,
        subscriberId: input.subscriberId,
        subscriberKind: input.subscriberKind,
        terminalId: indexed.terminalId,
        workspaceId: indexed.workspaceId,
      };
      this.#assertConnectorBoundToPane(
        socket,
        await this.#tryPaneIdentity(session.socketPath, input.paneId),
      );
      return presence;
    }

    try {
      const live = await this.#resolvePaneIdentity({
        paneId: input.paneId,
        socketPath: session.socketPath,
      });
      const presence: PiPresence = {
        connectedAt: this.#now(),
        herdrSessionName: session.name,
        paneId: live.paneId,
        subscriberId: input.subscriberId,
        subscriberKind: input.subscriberKind,
        terminalId: live.terminalId,
        workspaceId: live.workspaceId,
      };
      this.#assertConnectorBoundToPane(socket, live);
      return presence;
    } catch (error) {
      if (error instanceof ConnectorPaneMismatchError) throw error;
      if (!indexed) throw new Error("Herdr pane is not indexed yet");
      if (indexed.workspaceId !== input.workspaceId) {
        throw new Error("Pi presence workspace does not match indexed Herdr pane");
      }
      throw new Error("Herdr pane has no terminal identity");
    }
  }

  async #tryPaneIdentity(
    socketPath: string,
    paneId: string,
  ): Promise<HerdrPaneIdentity | undefined> {
    try {
      return await this.#resolvePaneIdentity({ paneId, socketPath });
    } catch {
      return undefined;
    }
  }

  #assertConnectorBoundToPane(
    socket: Socket,
    pane: Pick<HerdrPaneIdentity, "cwd" | "foregroundCwd"> | undefined,
  ): void {
    const paneCwd = pane?.cwd ?? pane?.foregroundCwd;
    if (!pane || !paneCwd) {
      console.warn("Herdsman could not resolve Herdr pane cwd for connector binding");
      return;
    }
    const peerPid = this.#peerPidOf(socket);
    if (peerPid === undefined) {
      console.warn("Herdsman could not read UDS peer pid for pane binding");
      return;
    }
    const bound =
      (pane.cwd !== undefined && peerBoundToPaneCwd(peerPid, pane.cwd)) ||
      (pane.foregroundCwd !== undefined && peerBoundToPaneCwd(peerPid, pane.foregroundCwd));
    if (!bound) {
      throw new ConnectorPaneMismatchError();
    }
  }

  #connectionState(presence: PiPresence): AgentOrchestratorConnectionStateResult {
    const state = this.#orchestrator.status(presence);
    const isOwner = state?.owner?.terminalId === presence.terminalId;
    const context = isOwner
      ? this.#context.workspaceSnapshot({ ...presence, excludeTerminalId: presence.terminalId })
      : null;
    return {
      context,
      events: this.#orchestrator.pending({
        ...presence,
        // The session's orchestrator also receives the events of ownerless
        // sibling workspaces (workers dispatched across workspaces).
        ...(isOwner ? { extraScopes: this.#orchestrator.ownerlessSiblingScopes(presence) } : {}),
        limit: 100,
      }),
      presence,
      state: state ? toWireState(state) : null,
    };
  }

  #requirePiPresence(socket: Socket): PiPresence {
    const presence = this.#piPresenceBySocket.get(socket);
    if (!presence) throw new Error("Pi presence is not registered for this connection");
    return presence;
  }

  #publishOrchestratorChange(change: AgentOrchestratorChanged): void {
    for (const [socket, presence] of this.#piPresenceBySocket) {
      if (sameScope(presence, change.previous) || sameScope(presence, change.current)) {
        this.#write(socket, { method: "agent.orchestrator.changed", params: { change } });
      }
    }
  }

  #newestSocketForTerminal(input: AgentScope & { terminalId: string }): Socket | undefined {
    let newest: { order: number; socket: Socket } | undefined;
    for (const [socket, presence] of this.#piPresenceBySocket) {
      if (
        sameScope(presence, input) &&
        presence.terminalId === input.terminalId &&
        !socket.destroyed
      ) {
        const order = this.#connectionOrderBySocket.get(socket) ?? 0;
        if (!newest || order > newest.order) newest = { order, socket };
      }
    }
    return newest?.socket;
  }

  #handleSocketClose(socket: Socket): void {
    if (!this.#sockets.delete(socket)) return;
    this.#connectionOrderBySocket.delete(socket);
    const presence = this.#piPresenceBySocket.get(socket);
    this.#piPresenceBySocket.delete(socket);
    if (presence) this.#unregisterTerminalSocket(socket, presence);
    // A socket that closes is no longer a trusted connected terminal. The
    // grace timer remains only to tolerate a same-terminal reconnect; active
    // non-pi terminals that leased ownership are preserved by #hasTerminalPresence.
    if (!this.#stopping && presence) {
      this.#scheduleDisconnect(presence);
    }
  }

  #scheduleDisconnect(presence: PiPresence): void {
    const key = terminalPresenceKey(presence);
    if (this.#hasTerminalPresence(presence) || this.#disconnectTimers.has(key)) return;
    const handle = this.#setTimeout(() => {
      this.#disconnectTimers.delete(key);
      if (this.#stopping || this.#hasTerminalPresence(presence)) return;
      this.#releaseCurrentOwnersForTerminal({
        ...presence,
        reason: "disconnected",
      });
    }, this.#disconnectGraceMs);
    this.#disconnectTimers.set(key, { handle });
  }

  #armStartupGrace(): void {
    for (const state of this.#orchestrator.persistedOwners()) {
      if (!state.owner) continue;
      const key = terminalPresenceKey({ ...state, terminalId: state.owner.terminalId });
      const indexed = this.#stores.agents.findByTerminal({
        herdrSessionName: state.herdrSessionName,
        terminalId: state.owner.terminalId,
      });
      if (indexed?.agent === "agy") {
        this.#agyPresenceByTerminal.set(key, {
          paneGeneration: indexed.paneGeneration ?? null,
          presence: {
            connectedAt: this.#now(),
            herdrSessionName: state.herdrSessionName,
            paneId: indexed.paneId,
            subscriberId: `agy:${state.owner.terminalId}`,
            subscriberKind: "agy",
            terminalId: state.owner.terminalId,
            workspaceId: state.workspaceId,
          },
        });
      }
      const handle = this.#setTimeout(() => {
        this.#startupTimers.delete(key);
        if (
          this.#stopping ||
          this.#hasTerminalPresence({ ...state, terminalId: state.owner?.terminalId ?? "" })
        ) {
          return;
        }
        this.#releaseCurrentOwnersForTerminal({
          herdrSessionName: state.herdrSessionName,
          reason: "startup_timeout",
          terminalId: state.owner?.terminalId ?? "",
        });
      }, this.#startupReconnectGraceMs);
      this.#startupTimers.set(key, { handle });
    }
  }

  #releaseCurrentOwnersForTerminal(input: {
    herdrSessionName: string;
    reason: "disconnected" | "startup_timeout";
    terminalId: string;
  }): void {
    const owners = this.#orchestrator
      .persistedOwners()
      .filter(
        (state) =>
          state.herdrSessionName === input.herdrSessionName &&
          state.owner?.terminalId === input.terminalId,
      );
    for (const owner of owners) {
      const change = this.#orchestrator.release({
        ...owner,
        reason: input.reason,
        terminalId: input.terminalId,
      });
      if (change) this.#publishOrchestratorChange(toWireChange(change));
    }
  }

  #cancelGraceForTerminal(presence: AgentScope & { terminalId: string }): void {
    const key = terminalPresenceKey(presence);
    this.#cancelTimer(this.#disconnectTimers, key);
    this.#cancelTimer(this.#startupTimers, key);
  }

  #resolvePaneGeneration(
    presence: Pick<PiPresence, "herdrSessionName" | "paneId" | "terminalId">,
  ): string | null {
    const indexed =
      this.#stores.agents.findByTerminal({
        herdrSessionName: presence.herdrSessionName,
        terminalId: presence.terminalId,
      }) ??
      this.#stores.agents.findByPane({
        herdrSessionName: presence.herdrSessionName,
        paneId: presence.paneId,
      });
    return indexed?.paneGeneration ?? null;
  }

  #isPiTerminal(presence: Pick<PiPresence, "herdrSessionName" | "paneId" | "terminalId">): boolean {
    const indexed =
      this.#stores.agents.findByTerminal({
        herdrSessionName: presence.herdrSessionName,
        terminalId: presence.terminalId,
      }) ??
      this.#stores.agents.findByPane({
        herdrSessionName: presence.herdrSessionName,
        paneId: presence.paneId,
      });
    if (indexed?.agent === "pi") return true;
    const sockets = this.#socketsByTerminal.get(terminalPresenceKey(presence));
    if (!sockets) return false;
    for (const socket of sockets) {
      if (this.#piPresenceBySocket.get(socket)?.subscriberKind === "pi") return true;
    }
    return false;
  }

  #hasTerminalPresence(input: { herdrSessionName: string; terminalId: string }): boolean {
    const key = terminalPresenceKey(input);
    if ((this.#socketsByTerminal.get(key)?.size ?? 0) > 0) return true;
    const lease = this.#agyPresenceByTerminal.get(key);
    if (!lease) return false;
    const { presence } = lease;
    const indexed =
      this.#stores.agents.findByTerminal({
        herdrSessionName: presence.herdrSessionName,
        terminalId: presence.terminalId,
      }) ??
      this.#stores.agents.findByPane({
        herdrSessionName: presence.herdrSessionName,
        paneId: presence.paneId,
      });
    if (indexed?.paneGeneration) {
      lease.paneGeneration = indexed.paneGeneration;
    }
    if (
      (indexed && indexed.agent !== null && indexed.agent !== "agy") ||
      this.#stores.agents.isPaneClosedSince({
        herdrSessionName: presence.herdrSessionName,
        paneId: presence.paneId,
        paneGeneration: lease.paneGeneration,
        sinceMs: presence.connectedAt,
      })
    ) {
      this.#agyPresenceByTerminal.delete(key);
      return false;
    }
    return true;
  }

  #registerTerminalSocket(socket: Socket, presence: PiPresence): void {
    const key = terminalPresenceKey(presence);
    let sockets = this.#socketsByTerminal.get(key);
    if (!sockets) {
      sockets = new Set();
      this.#socketsByTerminal.set(key, sockets);
    }
    sockets.add(socket);
  }

  #unregisterTerminalSocket(socket: Socket, presence: PiPresence): void {
    const key = terminalPresenceKey(presence);
    const sockets = this.#socketsByTerminal.get(key);
    sockets?.delete(socket);
    if (sockets?.size === 0) {
      this.#socketsByTerminal.delete(key);
      this.#lastActivityByTerminal.delete(key);
    }
  }

  #markTerminalActivity(socket: Socket): void {
    const presence = this.#piPresenceBySocket.get(socket);
    if (presence) this.#lastActivityByTerminal.set(terminalPresenceKey(presence), this.#now());
  }

  #scanHeartbeats(): void {
    if (this.#stopping) return;
    const cutoff = this.#now() - this.#heartbeatTimeoutMs;
    for (const [key, lastActivity] of this.#lastActivityByTerminal) {
      if (lastActivity >= cutoff) continue;
      for (const socket of this.#socketsByTerminal.get(key) ?? []) socket.destroy();
    }
  }

  /** Authoritative live Pi connection registry; intentionally empty after daemon startup. */
  isTerminalConnected(input: { herdrSessionName: string; terminalId: string }): boolean {
    return this.#hasTerminalPresence(input);
  }

  #cancelTimer(registry: Map<string, GraceTimer>, key: string): void {
    const timer = registry.get(key);
    if (!timer) return;
    this.#clearTimeout(timer.handle);
    registry.delete(key);
  }

  #clearGraceTimers(registry: Map<string, GraceTimer>): void {
    for (const timer of registry.values()) this.#clearTimeout(timer.handle);
    registry.clear();
  }

  #write(socket: Socket, message: unknown): void {
    if (!socket.destroyed) socket.write(encodeJsonLine(message));
  }

  #resolveScope(input: AgentQueryScope): AgentQueryScope {
    if (input.all)
      return {
        all: true,
        ...(input.herdrSessionName ? { herdrSessionName: input.herdrSessionName } : {}),
      };
    if (input.workspaceId && !input.herdrSessionName) {
      const sessions = new Set(
        this.#stores.agents
          .list({ workspaceId: input.workspaceId })
          .map((agent) => agent.herdrSessionName),
      );
      if (sessions.size > 1) {
        throw new Error(
          `workspace ${input.workspaceId} exists in multiple Herdr sessions; pass --session <name>: ${[...sessions].join(", ")}`,
        );
      }
    }
    if (input.workspaceId || input.herdrSessionName) {
      return {
        ...(input.herdrSessionName ? { herdrSessionName: input.herdrSessionName } : {}),
        ...(input.workspaceId ? { workspaceId: input.workspaceId } : {}),
      };
    }
    throw new Error(
      "agent scope requires current Herdr workspace, --workspace, --session, or --all",
    );
  }
}

class ConnectorPaneMismatchError extends Error {
  constructor() {
    super("Connector process does not match Herdr pane");
    this.name = "ConnectorPaneMismatchError";
  }
}

function assertSchema(schema: Parameters<typeof Value.Check>[0], value: unknown): void {
  if (!Value.Check(schema, value)) throw new Error("Invalid RPC params");
}

function toWireState(state: AgentOrchestratorState): AgentOrchestratorWireState {
  return { ...state, updatedAt: state.updatedAt.toISOString() };
}

function toWireChange(change: {
  current: AgentOrchestratorState;
  previous: AgentOrchestratorState;
  reason: AgentOrchestratorChanged["reason"];
}): AgentOrchestratorChanged {
  return {
    current: toWireState(change.current),
    previous: toWireState(change.previous),
    reason: change.reason,
  };
}

function sameScope(left: AgentScope, right: AgentScope): boolean {
  return left.herdrSessionName === right.herdrSessionName && left.workspaceId === right.workspaceId;
}

function sameOwner(
  left: AgentOrchestratorState["owner"],
  right: AgentOrchestratorState["owner"],
): boolean {
  return left?.terminalId === right?.terminalId && left?.paneId === right?.paneId;
}

function terminalPresenceKey(input: { herdrSessionName: string; terminalId: string }): string {
  return `${input.herdrSessionName}\0${input.terminalId}`;
}

function isUuidLikeSessionId(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}
