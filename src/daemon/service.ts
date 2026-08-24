import { chmodSync, existsSync, mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { env, exit } from "node:process";
import { fileURLToPath } from "node:url";
import { createAgentHistoryService } from "@/agent-history/service.js";
import { resolveRuntime } from "@/config/runtime.js";
import { AgentContextSnapshotStore } from "@/db/agent-context-snapshots.js";
import { AgentEventStore } from "@/db/agent-events.js";
import { AgentHistoryCacheStore } from "@/db/agent-history-cache.js";
import { AgentOrchestratorScopeStore } from "@/db/agent-orchestrator-scopes.js";
import { AgentStore } from "@/db/agents.js";
import { applyMigrations } from "@/db/apply-migrations.js";
import { openSqlite } from "@/db/client.js";
import { HerdrSessionStore } from "@/db/herdr-sessions.js";
import { HerdrWorkspaceStore } from "@/db/herdr-workspaces.js";
import { createHerdrSessionListRunner } from "@/herdr/session-list.js";
import { AgentContextService } from "@/observability/agent-context-service.js";
import { AgentIndexService } from "@/observability/agent-index-service.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import { TurnCompletionRegistry } from "@/observability/turn-completion.js";
import { AgentEventReconciler } from "./agent-event-reconciler.js";
import { HerdrSessionWatchManager } from "./herdr-session-watch-manager.js";
import { ObservabilityRpcServer } from "./observability-server.js";

export async function runObservabilityDaemonService(
  input: { environment?: NodeJS.ProcessEnv | undefined } = {},
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
    },
    turnCompletions,
  });

  let connectedTerminal = (_input: { herdrSessionName: string; terminalId: string }) => false;
  const reconciler = new AgentEventReconciler({
    connectedTerminal: (input) => connectedTerminal(input),
    events: agentEvents,
    scopes: agentOrchestratorScopes,
    sessionList: createHerdrSessionListRunner({ env: runtime.environment }),
  });

  const server = new ObservabilityRpcServer({
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
    sessionList: createHerdrSessionListRunner({ env: runtime.environment }),
  });

  await server.start();
  await reconciler.reconcile();
  await watchManager.start();
  console.log(`Herdsman daemon listening on ${runtime.paths.socketPath}`);

  const stop = async () => {
    await watchManager.stop();
    await server.stop();
    sqlite.close();
    exit(0);
  };
  process.once("SIGINT", stop);
  process.once("SIGTERM", stop);
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
