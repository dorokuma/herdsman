import { chmodSync, existsSync, mkdirSync, rmSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { env, exit } from "node:process";
import { fileURLToPath } from "node:url";
import { createAgentHistoryService } from "@/agent-history/service.js";
import { resolveRuntime } from "@/config/runtime.js";
import {
  acquireDaemonLock,
  daemonInstanceLockPath,
  removeDaemonPidFile,
  writeDaemonPidFile,
} from "@/daemon/process-manager.js";
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
import type { AgentEventRecord } from "@/observability/contracts.js";
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

/**
 * Ceiling for the whole shutdown sequence. It must stay clearly below the unit's
 * TimeoutStopSec (10s in /etc/systemd/system/herdsman.service): a shutdown that
 * outlives it is SIGKILLed, which skips the finally cleanup (instance lock, pid
 * file, helper child) and leaves the daemon half-stopped. Every step is bounded
 * by whatever budget is left, so a single wedged step cannot run the sequence
 * past this ceiling.
 */
export const SHUTDOWN_BUDGET_MS = 5_000;

/**
 * Floor granted to a step once the budget is spent. The sequence may therefore
 * overshoot the budget by at most (steps - 1) * SHUTDOWN_MIN_STEP_MS, which still
 * leaves several seconds of headroom under TimeoutStopSec; in exchange no step is
 * skipped outright, so the pid file and socket are always cleaned up.
 */
export const SHUTDOWN_MIN_STEP_MS = 250;

export type ShutdownStep = {
  name: string;
  run: () => Promise<unknown> | unknown;
};

/**
 * Runs shutdown steps in order under one shared budget. Each step is raced
 * against the time left: a step that overruns is logged and abandoned so the
 * next step (and the caller's finally cleanup) still runs. Failures that are not
 * timeouts are not swallowed — they reject the sequence as before, so a genuine
 * shutdown error still surfaces instead of hiding behind the budget.
 */
export async function runShutdownSteps(
  steps: readonly ShutdownStep[],
  options: { budgetMs?: number } = {},
): Promise<void> {
  const budgetMs = options.budgetMs ?? SHUTDOWN_BUDGET_MS;
  const startedAt = Date.now();
  const deadlineMs = startedAt + budgetMs;
  for (const step of steps) {
    const stepStartedAt = Date.now();
    const remainingMs = deadlineMs - stepStartedAt;
    const timeoutMs = Math.max(remainingMs, SHUTDOWN_MIN_STEP_MS);
    console.log("Herdsman daemon shutdown step starting", {
      budgetRemainingMs: Math.max(remainingMs, 0),
      step: step.name,
      timeoutMs,
    });
    const timedOut = await raceShutdownStep(step, timeoutMs);
    const elapsedMs = Date.now() - stepStartedAt;
    if (timedOut) {
      console.warn("Herdsman daemon shutdown step timed out; continuing", {
        budgetMs,
        elapsedMs,
        step: step.name,
        timeoutMs,
      });
      continue;
    }
    console.log("Herdsman daemon shutdown step finished", { elapsedMs, step: step.name });
  }
  console.log("Herdsman daemon shutdown steps finished", {
    budgetMs,
    elapsedMs: Date.now() - startedAt,
  });
}

/** Returns true when the timeout won; a step rejection propagates unchanged. */
async function raceShutdownStep(step: ShutdownStep, timeoutMs: number): Promise<boolean> {
  let timer: NodeJS.Timeout | undefined;
  try {
    const timedOut = new Promise<true>((resolve) => {
      timer = setTimeout(() => resolve(true), timeoutMs);
      timer.unref();
    });
    const finished = Promise.resolve()
      .then(() => step.run())
      .then(() => false as const);
    return await Promise.race([finished, timedOut]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

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
    applyMigrations?: typeof applyMigrations;
    connectSocket?: (socketPath: string) => Promise<boolean>;
    environment?: NodeJS.ProcessEnv | undefined;
    exit?: (code?: number) => void;
    openSqlite?: typeof openSqlite;
    pid?: number;
    reconcileClearInterval?: (handle: ReturnType<typeof setInterval>) => void;
    reconcileIntervalMs?: number;
    reconcileSetInterval?: (callback: () => void, delay: number) => ReturnType<typeof setInterval>;
    sessionList?: HerdrSessionListRunner;
    shutdownBudgetMs?: number;
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

  const currentPid = input.pid ?? process.pid;

  let releaseInstanceLock: (() => void) | undefined;
  const releaseInstanceLockIfHeld = () => {
    if (releaseInstanceLock) {
      releaseInstanceLock();
      releaseInstanceLock = undefined;
    }
  };

  // The bare daemon entrypoint (herdsman-daemon.js) takes an instance lock so
  // two daemon processes cannot share one HERDSMAN_HOME. It is acquired before
  // the SQLite database is opened: a second or orphaned daemon is rejected
  // while it has not touched (opened, migrated, or locked) the database at all.
  // The lock is released on graceful stop and on every startup failure path.
  releaseInstanceLock = acquireDaemonLock(daemonInstanceLockPath(runtime.paths.pidPath), {
    pid: currentPid,
  });

  const openSqliteImpl = input.openSqlite ?? openSqlite;
  const applyMigrationsImpl = input.applyMigrations ?? applyMigrations;

  const { sqlite } = openSqliteImpl(runtime.paths.dbPath);
  for (const path of [
    runtime.paths.dbPath,
    `${runtime.paths.dbPath}-wal`,
    `${runtime.paths.dbPath}-shm`,
  ]) {
    if (existsSync(path)) chmodSync(path, 0o600);
  }
  applyMigrationsImpl(sqlite, {
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
  let publishEvent = (_event: AgentEventRecord) => {};

  // One controller drives every cancellable wait in the daemon. It is aborted at
  // the start of stop(), so in-flight status waits (history-advance window, turn
  // waits) and watcher loops end immediately instead of running out their own
  // 30s / 12s windows and pushing the daemon past TimeoutStopSec.
  const shutdownController = new AbortController();

  const index = new AgentIndexService({
    context: daemonServices.context,
    onAgentEvent: (event) => publishEvent(event), // 闭包注入
    shutdownSignal: shutdownController.signal,
    stores: {
      agentEvents,
      agentHistoryCache,
      agentOrchestratorScopes,
      agents,
      herdrSessions,
      herdrWorkspaces,
      sqlite,
      statusEventPlans,
    },
    turnCompletions,
  });

  const sessionList =
    input.sessionList ?? createHerdrSessionListRunner({ env: runtime.environment });

  let connectedTerminal = (_input: { herdrSessionName: string; terminalId: string }) => false;
  const reconciler = new AgentEventReconciler({
    agentHistoryCache,
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
  publishEvent = (event) => server.publishAgentEvent(event); // 接通推送
  const watchManager = new HerdrSessionWatchManager({
    agents,
    herdrSessions,
    index,
    onAgentContextChanged: (scope) => server.publishAgentContext(scope),
    onAgentEvent: (event) => server.publishAgentEvent(event),
    onAgentIndexRefreshed: (refreshed) => server.reconcileAgentLocations(refreshed),
    sessionList,
    shutdownSignal: shutdownController.signal,
  });

  const onUnhandledRejection = (reason: unknown) => {
    console.error("Herdsman daemon unhandled rejection", reason);
  };
  const onUncaughtException = (error: Error) => {
    console.error("Herdsman daemon uncaught exception", error);
  };
  process.on("unhandledRejection", onUnhandledRejection);
  process.on("uncaughtException", onUncaughtException);

  const signalTarget = input.signalTarget ?? process;
  const doExit = input.exit ?? exit;
  const shutdownBudgetMs = input.shutdownBudgetMs ?? SHUTDOWN_BUDGET_MS;

  // Register signal handlers before the first await that can block (server
  // start, reconcile): once the pid file is written the daemon must always
  // clean it up on a graceful shutdown, even if startup is still in progress.
  let reconcileScheduler: PeriodicReconcileScheduler | undefined;
  let stopping = false;
  let exitRequested = false;

  // The forced-exit path can win the race against the graceful one; whichever
  // gets there first decides the exit code, and the loser must not exit again.
  const requestExit = (code: number) => {
    if (exitRequested) return;
    exitRequested = true;
    doExit(code);
  };

  // A second SIGTERM/SIGINT means the first shutdown is not converging (or the
  // supervisor is about to SIGKILL). Re-arming through these handlers keeps that
  // signal away from Node's default action, which would kill the process before
  // the finally cleanup and leave the pid file, socket and instance lock behind.
  const onSecondSignal = () => {
    console.warn("Herdsman daemon received a second shutdown signal; forcing exit", {
      pid: currentPid,
    });
    try {
      removeDaemonPidFile(runtime.paths.pidPath, currentPid);
    } catch {}
    try {
      rmSync(runtime.paths.socketPath, { force: true });
    } catch {}
    try {
      releaseInstanceLockIfHeld();
    } catch {}
    // Exit 0 on purpose: this is still an intentional stop, and a non-zero code
    // would make `Restart=on-failure` pull the daemon back up.
    requestExit(0);
  };

  const stop = async () => {
    if (stopping) return;
    stopping = true;
    detachSignalHandlers();
    attachSignalHandlers(onSecondSignal);
    const startedAt = Date.now();
    let exitCode = 0;
    console.log("Herdsman daemon shutdown starting", {
      budgetMs: shutdownBudgetMs,
      pid: currentPid,
    });
    try {
      process.off("unhandledRejection", onUnhandledRejection);
      process.off("uncaughtException", onUncaughtException);
      index.stopWaitingHistoryRetries();
      // Abort before draining: waits that are already in flight end now instead
      // of holding the drain open for their own window.
      shutdownController.abort();
      console.log("Herdsman daemon shutdown aborted in-flight waits", {
        elapsedMs: Date.now() - startedAt,
      });
      await runShutdownSteps(
        [
          { name: "index.drainInFlightPlans", run: () => index.drainInFlightPlans() },
          {
            name: "reconcileScheduler.stop",
            run: async () => {
              await reconcileScheduler?.stop();
            },
          },
          { name: "watchManager.stop", run: () => watchManager.stop() },
          { name: "server.stop", run: () => server.stop() },
        ],
        { budgetMs: shutdownBudgetMs },
      );
      sqlite.close();
    } catch (error) {
      console.error("Herdsman daemon shutdown failed", error);
      exitCode = 1;
    } finally {
      removeDaemonPidFile(runtime.paths.pidPath, currentPid);
      releaseInstanceLockIfHeld();
      console.log("Herdsman daemon shutdown finished", {
        elapsedMs: Date.now() - startedAt,
        exitCode,
      });
      requestExit(exitCode);
    }
  };

  const attachSignalHandlers = (handler: () => void | Promise<void>) => {
    signalTarget.once("SIGINT", handler);
    signalTarget.once("SIGTERM", handler);
  };

  const detachSignalHandlers = () => {
    if (typeof signalTarget.off !== "function") return;
    signalTarget.off("SIGINT", stop);
    signalTarget.off("SIGTERM", stop);
    signalTarget.off("SIGINT", onSecondSignal);
    signalTarget.off("SIGTERM", onSecondSignal);
  };

  attachSignalHandlers(stop);

  try {
    await server.start();
    writeDaemonPidFile(runtime.paths.pidPath, currentPid);
    await reconciler.reconcile({ releaseStaleOwners: false });
    await index.drainPendingPlans();
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
  } catch (error) {
    process.off("unhandledRejection", onUnhandledRejection);
    process.off("uncaughtException", onUncaughtException);
    detachSignalHandlers();
    // Same abort as the graceful path: a half-started watcher must not keep its
    // socket (and the failing startup) alive.
    shutdownController.abort();
    try {
      await reconcileScheduler?.stop();
    } catch {}
    try {
      await watchManager.stop();
    } catch {}
    try {
      await server.stop();
    } catch {}
    removeDaemonPidFile(runtime.paths.pidPath, currentPid);
    releaseInstanceLockIfHeld();
    sqlite.close();
    throw error;
  }
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
