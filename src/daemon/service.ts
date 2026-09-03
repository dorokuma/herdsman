import { chmodSync, existsSync, mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { env, exit } from "node:process";
import { fileURLToPath } from "node:url";
import { createAgentHistoryService } from "@/agent-history/service.js";
import { resolveRuntime } from "@/config/runtime.js";
import { removeDaemonPidFile, writeDaemonPidFile } from "@/daemon/process-manager.js";
import { AgentContextSnapshotStore } from "@/db/agent-context-snapshots.js";
import { AgentEventStore } from "@/db/agent-events.js";
import { AgentHistoryCacheStore } from "@/db/agent-history-cache.js";
import { AgentOrchestratorScopeStore } from "@/db/agent-orchestrator-scopes.js";
import { AgentStore } from "@/db/agents.js";
import { applyMigrations } from "@/db/apply-migrations.js";
import { openSqlite } from "@/db/client.js";
import { HerdrSessionStore } from "@/db/herdr-sessions.js";
import { HerdrWorkspaceStore } from "@/db/herdr-workspaces.js";
import { StatusEventPlanStore } from "@/db/status-event-plans.js";
import { createHerdrSessionListRunner, type HerdrSessionListRunner } from "@/herdr/session-list.js";
import { AgentContextService } from "@/observability/agent-context-service.js";
import { AgentIndexService } from "@/observability/agent-index-service.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import { TurnCompletionRegistry } from "@/observability/turn-completion.js";
import { AgentEventReconciler } from "./agent-event-reconciler.js";
import { HerdrSessionWatchManager } from "./herdr-session-watch-manager.js";
import { ObservabilityRpcServer } from "./observability-server.js";

/**
 * Periodic reconcile cadence. The 7-day (settled events) and 30-day (released
 * scopes) TTLs only converge when AgentEventReconciler.reconcile runs, so the
 * daemon drives it on a fixed 15-minute cycle instead of only at startup.
 * 15 minutes is deliberately coarser than the watch manager's 10s/60s refresh
 * ticks: reconcile opens sockets to every running Herdr session, while the TTLs
 * it enforces are days long, so a 15-minute cycle converges promptly without
 * per-tick socket traffic.
 */
export const RECONCILE_INTERVAL_MS = 15 * 60 * 1000;

type ReconcileRun = () => Promise<unknown>;
type IntervalHandle = ReturnType<typeof setInterval>;

/**
 * Runs the reconcile cycle on a fixed cadence while guaranteeing at most one
 * in-flight run: a tick that fires while the previous run is still executing is
 * skipped. This mirrors HerdrSessionWatchManager's in-flight tick guard. SQLite
 * writes on the daemon's single shared connection are synchronous, so the only
 * concurrency hazard between periodic reconcile and live event writes would be
 * two reconcile loops interleaving at their await points; the in-flight guard
 * prevents that, keeping event writes and reconcile serialized exactly like the
 * rest of the daemon.
 */
export class PeriodicReconcileScheduler {
  readonly #clearInterval: (handle: IntervalHandle) => void;
  readonly #intervalMs: number;
  readonly #run: ReconcileRun;
  readonly #setInterval: (callback: () => void, delay: number) => IntervalHandle;
  #handle: IntervalHandle | undefined;
  #inFlight: Promise<unknown> | undefined;

  constructor(options: {
    clearInterval?: (handle: IntervalHandle) => void;
    intervalMs: number;
    run: ReconcileRun;
    setInterval?: (callback: () => void, delay: number) => IntervalHandle;
  }) {
    this.#clearInterval = options.clearInterval ?? clearInterval;
    this.#intervalMs = options.intervalMs;
    this.#run = options.run;
    this.#setInterval = options.setInterval ?? setInterval;
  }

  start(): void {
    if (this.#handle !== undefined) return;
    this.#handle = this.#setInterval(() => {
      if (this.#inFlight) return;
      this.#inFlight = Promise.resolve()
        .then(this.#run)
        .catch((error) => {
          console.warn("Herdsman periodic reconcile failed", error);
        })
        .finally(() => {
          this.#inFlight = undefined;
        });
    }, this.#intervalMs);
  }

  async stop(): Promise<void> {
    if (this.#handle !== undefined) {
      this.#clearInterval(this.#handle);
      this.#handle = undefined;
    }
    await this.#inFlight?.catch(() => undefined);
  }
}

export async function runObservabilityDaemonService(
  input: {
    connectSocket?: (socketPath: string) => Promise<boolean>;
    environment?: NodeJS.ProcessEnv | undefined;
    exit?: (code?: number) => void;
    pid?: number;
    reconcileClearInterval?: (handle: ReturnType<typeof setInterval>) => void;
    reconcileIntervalMs?: number;
    reconcileSetInterval?: (callback: () => void, delay: number) => ReturnType<typeof setInterval>;
    sessionList?: HerdrSessionListRunner;
    signalTarget?: {
      off?: (event: "SIGINT" | "SIGTERM", listener: () => void | Promise<void>) => void;
      once: (event: "SIGINT" | "SIGTERM", listener: () => void | Promise<void>) => void;
    };
  } = {},
): Promise<void> {
  const runtime = resolveRuntime({ environment: input.environment });
  applyEnvironment(runtime.environment);
  mkdirSync(runtime.paths.homeDir, { mode: 0o700, recursive: true });
  chmodSync(runtime.paths.homeDir, 0o700);
  mkdirSync(dirname(runtime.paths.dbPath), { mode: 0o700, recursive: true });
  chmodSync(dirname(runtime.paths.dbPath), 0o700);
  mkdirSync(dirname(runtime.paths.socketPath), { mode: 0o700, recursive: true });
  chmodSync(dirname(runtime.paths.socketPath), 0o700);

  const { sqlite } = openSqlite(runtime.paths.dbPath);
  for (const path of [
    runtime.paths.dbPath,
    `${runtime.paths.dbPath}-wal`,
    `${runtime.paths.dbPath}-shm`,
  ]) {
    if (existsSync(path)) chmodSync(path, 0o600);
  }
  applyMigrations(sqlite, {
    migrationsFolder: resolveMigrationsFolder(dirname(fileURLToPath(import.meta.url))),
  });

  const herdrSessions = new HerdrSessionStore(sqlite);
  const herdrWorkspaces = new HerdrWorkspaceStore(sqlite);
  const agentEvents = new AgentEventStore(sqlite);
  const agents = new AgentStore(sqlite, agentEvents);
  const agentHistoryCache = new AgentHistoryCacheStore(sqlite);
  const agentContextSnapshots = new AgentContextSnapshotStore(sqlite);
  const agentOrchestratorScopes = new AgentOrchestratorScopeStore(sqlite);
  const statusEventPlans = new StatusEventPlanStore(sqlite);
  const history = createAgentHistoryService({ cache: agentHistoryCache });
  const context = new AgentContextService({
    history,
    stores: { agentContextSnapshots, agents },
  });
  const daemonServices = { context, history };
  const orchestrator = new AgentOrchestratorService({
    agentEvents,
    agents,
    scopes: agentOrchestratorScopes,
  });
  const turnCompletions = new TurnCompletionRegistry();
  const index = new AgentIndexService({
    context: daemonServices.context,
    stores: {
      agentEvents,
      agentHistoryCache,
      agentOrchestratorScopes,
      agents,
      herdrSessions,
      herdrWorkspaces,
      statusEventPlans,
    },
    turnCompletions,
  });

  const sessionList =
    input.sessionList ?? createHerdrSessionListRunner({ env: runtime.environment });

  let connectedTerminal = (_input: { herdrSessionName: string; terminalId: string }) => false;
  const reconciler = new AgentEventReconciler({
    connectedTerminal: (input) => connectedTerminal(input),
    events: agentEvents,
    scopes: agentOrchestratorScopes,
    sessionList,
    statusEventPlans,
  });

  const server = new ObservabilityRpcServer({
    ...(input.connectSocket !== undefined ? { connectSocket: input.connectSocket } : {}),
    context: daemonServices.context,
    history: daemonServices.history,
    orchestrator,
    registerPiSessionRef: (registration) => index.registerPiSessionRef(registration),
    socketPath: runtime.paths.socketPath,
    stores: { agentEvents, agents, herdrSessions, herdrWorkspaces },
    turnCompletions,
  });
  connectedTerminal = (input) => server.isTerminalConnected(input);
  const watchManager = new HerdrSessionWatchManager({
    agents,
    herdrSessions,
    index,
    onAgentContextChanged: (scope) => server.publishAgentContext(scope),
    onAgentEvent: (event) => server.publishAgentEvent(event),
    onAgentIndexRefreshed: (refreshed) => server.reconcileAgentLocations(refreshed),
    sessionList,
  });

  const currentPid = input.pid ?? process.pid;
  const signalTarget = input.signalTarget ?? process;
  const doExit = input.exit ?? exit;

  // Register signal handlers before the first await that can block (server
  // start, reconcile): once the pid file is written the daemon must always
  // clean it up on a graceful shutdown, even if startup is still in progress.
  let reconcileScheduler: PeriodicReconcileScheduler | undefined;
  const stop = async () => {
    let exitCode = 0;
    try {
      await reconcileScheduler?.stop();
      await watchManager.stop();
      await server.stop();
      sqlite.close();
    } catch (error) {
      console.error("Herdsman daemon shutdown failed", error);
      exitCode = 1;
    } finally {
      removeDaemonPidFile(runtime.paths.pidPath, currentPid);
      doExit(exitCode);
    }
  };
  signalTarget.once("SIGINT", stop);
  signalTarget.once("SIGTERM", stop);

  try {
    await server.start();
  } catch (error) {
    if (typeof signalTarget.off === "function") {
      signalTarget.off("SIGINT", stop);
      signalTarget.off("SIGTERM", stop);
    }
    sqlite.close();
    throw error;
  }
  writeDaemonPidFile(runtime.paths.pidPath, currentPid);
  await reconciler.reconcile({ releaseStaleOwners: false });
  // Periodic reconcile keeps the 7-day/30-day TTLs converging on long-running
  // daemons; the in-flight guard inside the scheduler prevents overlapping runs.
  reconcileScheduler = new PeriodicReconcileScheduler({
    ...(input.reconcileClearInterval === undefined
      ? {}
      : { clearInterval: input.reconcileClearInterval }),
    intervalMs: input.reconcileIntervalMs ?? RECONCILE_INTERVAL_MS,
    // Long-lived daemons must also keep retrying runtime-failed status event
    // plans: drainPendingPlans is idempotent (pending/running rows only,
    // per-agent serial, attempts capped) and the periodic cadence prevents a
    // hot loop, so piggybacking it on the reconcile cycle completes the retry
    // path that the startup-only drain leaves open.
    run: async () => {
      await reconciler.reconcile({ releaseStaleOwners: false });
      await index.drainPendingPlans();
    },
    ...(input.reconcileSetInterval === undefined
      ? {}
      : { setInterval: input.reconcileSetInterval }),
  });
  reconcileScheduler.start();
  await watchManager.start();
  console.log(`Herdsman daemon listening on ${runtime.paths.socketPath}`);
}

export function resolveMigrationsFolder(startDir: string): string {
  let current = resolve(startDir);
  while (true) {
    const migrationsFolder = resolve(current, "drizzle");
    if (existsSync(resolve(migrationsFolder, "meta", "_journal.json"))) {
      return migrationsFolder;
    }
    const parent = dirname(current);
    if (parent === current) {
      throw new Error(`Cannot find Herdsman migrations above ${startDir}`);
    }
    current = parent;
  }
}

function applyEnvironment(environment: NodeJS.ProcessEnv): void {
  for (const [key, value] of Object.entries(environment)) {
    if (value === undefined) delete env[key];
    else env[key] = value;
  }
}
