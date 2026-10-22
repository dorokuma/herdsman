#!/usr/bin/env node
import { realpathSync } from "node:fs";
import { resolve } from "node:path";
import { argv, exit, stdin as processStdin } from "node:process";
import { fileURLToPath } from "node:url";
import { type AgyHookEventName, runAgyHook } from "@/agy/hook.js";
import { resolveRuntime, runtimePathsFromRecordOrDefault } from "@/config/runtime.js";
import { ObservabilityRpcClient } from "@/daemon/client.js";
import {
  type DaemonStatus,
  getDaemonStatus,
  isSystemdHerdsmanManaged,
  readDaemonRuntimeRecord,
} from "@/daemon/process-manager.js";
import type {
  AgentGetResult,
  AgentListItem,
  AgentOrchestratorWireState,
  AgentReadResult,
} from "@/observability/contracts.js";

const CURRENT_HERDR_WORKSPACE_ERROR =
  "agent command requires HERDR_ENV=1 with HERDR_WORKSPACE_ID, --workspace <id>, --session <name>, or --all.";
const CURRENT_HERDR_PANE_ERROR =
  "orchestrator command requires HERDR_ENV=1 with HERDR_SOCKET_PATH, HERDR_PANE_ID, and HERDR_WORKSPACE_ID.";

type DaemonAction = "status";

type AgentScope = {
  all?: boolean;
  herdrSessionName?: string;
  workspaceId?: string;
};

type HerdrPaneIdentityEnv = {
  herdrSocketPath: string;
  paneId: string;
  workspaceId: string;
};

export type CliCommand =
  | { action: DaemonAction; command: "daemon"; text: boolean }
  | ({ command: "agent-list"; json: boolean } & AgentScope)
  | ({ command: "agent-get"; json: boolean; target: string } & AgentScope)
  | ({ command: "agent-read"; json: boolean; limit?: number; target: string } & AgentScope)
  | ({
      action: "on" | "off" | "status";
      command: "orchestrator";
      ifUnowned?: boolean;
      json: boolean;
    } & HerdrPaneIdentityEnv)
  | { command: "agy-hook"; hookEventName: AgyHookEventName }
  | { command: "help" };

type RpcClientLike = Pick<ObservabilityRpcClient, "close" | "request">;

type RunCliDeps = {
  connect(
    socketPath: string,
    onNotification?: (method: string, params: unknown) => void,
  ): Promise<RpcClientLike>;
  environment?: NodeJS.ProcessEnv;
  output(line: string): void;
  socketPath: string;
  stateDir?: string;
  stdin?: string;
};

export function parseCliArgs(
  args: string[],
  environment: NodeJS.ProcessEnv = process.env,
): CliCommand {
  const [command, ...rest] = args;
  if (!command || command === "--help" || command === "-h" || command === "help") {
    return { command: "help" };
  }

  if (command === "daemon") {
    const [action = "status", ...extra] = rest;
    if (!isDaemonAction(action)) throw new Error(`Unknown daemon action: ${action}`);
    const text = takeFlag(extra, "--text");
    rejectExtra(extra);
    return { action, command: "daemon", text };
  }

  if (command === "agent") {
    return parseAgentCommand(rest, environment);
  }

  if (command === "on" || command === "off" || command === "status") {
    return parseOrchestratorCommand(command, rest, environment);
  }

  if (command === "agy-hook") {
    const [hookEventName, ...extra] = rest;
    if (
      hookEventName !== "PreInvocation" &&
      hookEventName !== "Stop" &&
      hookEventName !== "WakeWaiter"
    ) {
      throw new Error("agy-hook requires PreInvocation, Stop, or WakeWaiter");
    }
    rejectExtra(extra);
    return { command: "agy-hook", hookEventName };
  }

  throw new Error(`Unknown command: ${command}`);
}

function parseOrchestratorCommand(
  action: "on" | "off" | "status",
  args: string[],
  environment: NodeJS.ProcessEnv,
): CliCommand {
  const rest = [...args];
  const json = takeFlag(rest, "--json");
  const ifUnowned = action === "on" ? takeFlag(rest, "--if-unowned") : false;
  rejectExtra(rest);
  if (
    environment.HERDR_ENV !== "1" ||
    !environment.HERDR_SOCKET_PATH ||
    !environment.HERDR_PANE_ID ||
    !environment.HERDR_WORKSPACE_ID
  ) {
    throw new Error(CURRENT_HERDR_PANE_ERROR);
  }
  return {
    action,
    command: "orchestrator",
    herdrSocketPath: environment.HERDR_SOCKET_PATH,
    ...(ifUnowned ? { ifUnowned: true } : {}),
    json,
    paneId: environment.HERDR_PANE_ID,
    workspaceId: environment.HERDR_WORKSPACE_ID,
  };
}

function parseAgentCommand(args: string[], environment: NodeJS.ProcessEnv): CliCommand {
  const [subcommand, ...rest] = args;
  if (!subcommand || subcommand === "help" || subcommand === "--help" || subcommand === "-h") {
    return { command: "help" };
  }
  const json = takeFlag(rest, "--json");
  const herdrSessionName = takeOption(rest, "--session");
  const workspaceId = takeOption(rest, "--workspace");
  const explicitScope: AgentScope = {
    ...(herdrSessionName ? { herdrSessionName } : {}),
    ...(workspaceId ? { workspaceId } : {}),
  };

  if (subcommand === "list") {
    const all = takeFlag(rest, "--all");
    rejectExtra(rest);
    return {
      command: "agent-list",
      ...(all ? { all: true } : scopedOrCurrent(explicitScope, environment)),
      json,
    };
  }

  if (subcommand === "get") {
    const [target, ...extra] = rest;
    if (!target) throw new Error("agent get requires <target>");
    rejectExtra(extra);
    return {
      command: "agent-get",
      ...scopedOrCurrent(explicitScope, environment),
      json,
      target,
    };
  }

  if (subcommand === "read") {
    const limitValue = takeOption(rest, "--limit");
    const [target, ...extra] = rest;
    if (!target) throw new Error("agent read requires <target>");
    rejectExtra(extra);
    const limit = limitValue ? Number(limitValue) : undefined;
    if (limit !== undefined && (!Number.isInteger(limit) || limit < 1 || limit > 500)) {
      throw new Error("--limit must be between 1 and 500");
    }
    return {
      command: "agent-read",
      ...scopedOrCurrent(explicitScope, environment),
      json,
      ...(limit !== undefined ? { limit } : {}),
      target,
    };
  }

  throw new Error(`Unknown agent command: ${subcommand}`);
}

function scopedOrCurrent(scope: AgentScope, environment: NodeJS.ProcessEnv): AgentScope {
  if (scope.herdrSessionName || scope.workspaceId || scope.all) return scope;
  if (environment.HERDR_ENV === "1" && environment.HERDR_WORKSPACE_ID) {
    return { workspaceId: environment.HERDR_WORKSPACE_ID };
  }
  throw new Error(CURRENT_HERDR_WORKSPACE_ERROR);
}

export function helpText(): string {
  return `Usage:
  herdsman on [--if-unowned] [--json]
  herdsman off [--json]
  herdsman status [--json]
  herdsman daemon status [--text]
  herdsman agent list [--all] [--workspace <id>] [--session <name>] [--json]
  herdsman agent get <target> [--workspace <id>] [--session <name>] [--json]
  herdsman agent read <target> [--limit N] [--workspace <id>] [--session <name>] [--json]
  herdsman agy-hook <PreInvocation|Stop|WakeWaiter>
  herdsman help

Notes:
  \`daemon status\` keeps printing JSON by default (the existing machine
  contract); \`--text\` is the human view. Its two supervision facts are
  read-only: \`managedBy\` only says whether the pid belongs to the
  herdsman.service system unit — \`unmanaged\` does not mean unsupervised, and
  it is not a licence to kill anything. \`restartCount\` is systemd's
  NRestarts, which reset-failed, a stop+start pair or an unloaded unit all
  reset to 0 — so 0 is not "never crashed".
`;
}

export async function runCliCommand(command: CliCommand, deps: RunCliDeps): Promise<void> {
  if (command.command === "help") {
    deps.output(helpText());
    return;
  }
  if (command.command === "daemon") throw new Error("daemon command is handled by main");
  if (command.command === "agy-hook") {
    const rawStdin =
      deps.stdin ?? (command.hookEventName === "WakeWaiter" ? "" : await readAllStdin());
    const output = await runAgyHook(command.hookEventName, rawStdin, {
      connect: (socketPath, onNotification) => deps.connect(socketPath, onNotification),
      ...(deps.environment ? { environment: deps.environment } : {}),
      socketPath: deps.socketPath,
      ...(deps.stateDir ? { stateDir: deps.stateDir } : {}),
    });
    deps.output(JSON.stringify(output));
    return;
  }
  const client = await deps.connect(deps.socketPath);
  try {
    const result = await dispatchRpcCommand(command, client);
    printResult(command, result, deps.output);
  } finally {
    client.close();
  }
}

async function dispatchRpcCommand(
  command: Exclude<CliCommand, { command: "agy-hook" | "daemon" | "help" }>,
  client: RpcClientLike,
) {
  if (command.command === "orchestrator") {
    const registered = (await client.request("agent.orchestrator.register", {
      herdrSocketPath: command.herdrSocketPath,
      paneId: command.paneId,
      sessionRef: {
        agent: "agy",
        kind: "id",
        source: "herdsman-cli",
        value: "herdsman-cli",
      },
      subscriberId: `cli:${command.paneId}`,
      subscriberKind: "agy",
      workspaceId: command.workspaceId,
    })) as {
      presence: {
        herdrSessionName: string;
        paneId: string;
        terminalId: string;
        workspaceId: string;
      };
      state: AgentOrchestratorWireState | null;
    };
    if (command.action === "status") {
      return registered;
    }
    if (command.action === "on") {
      if (
        command.ifUnowned &&
        registered.state?.owner &&
        registered.state.owner.terminalId !== registered.presence.terminalId
      ) {
        return { ...registered, changed: false };
      }
      return client.request("agent.orchestrator.set", { enabled: true });
    }
    return client.request("agent.orchestrator.set", { enabled: false });
  }
  if (command.command === "agent-list") {
    return client.request("agent.list", scopeParams(command));
  }
  if (command.command === "agent-get") {
    return client.request("agent.get", { ...scopeParams(command), target: command.target });
  }
  return client.request("agent.read", {
    ...scopeParams(command),
    ...(command.limit !== undefined ? { limit: command.limit } : {}),
    target: command.target,
  });
}

function scopeParams(scope: AgentScope): AgentScope {
  return {
    ...(scope.all ? { all: true } : {}),
    ...(scope.herdrSessionName ? { herdrSessionName: scope.herdrSessionName } : {}),
    ...(scope.workspaceId ? { workspaceId: scope.workspaceId } : {}),
  };
}

function printResult(command: CliCommand, result: unknown, output: (line: string) => void): void {
  if ("json" in command && command.json) {
    output(JSON.stringify(result));
    return;
  }
  output(formatHumanResult(command, result));
}

function formatHumanResult(command: CliCommand, result: unknown): string {
  if (command.command === "orchestrator") {
    return formatOrchestratorStatus(
      result as {
        presence: {
          herdrSessionName: string;
          paneId: string;
          terminalId: string;
          workspaceId: string;
        };
        state: AgentOrchestratorWireState | null;
      },
    );
  }
  if (command.command === "agent-list")
    return formatAgentList(result as { agents?: AgentListItem[] });
  if (command.command === "agent-get") return formatAgentGet(result as { agent?: AgentGetResult });
  if (command.command === "agent-read")
    return formatAgentRead(result as { agent?: AgentReadResult });
  return JSON.stringify(result);
}

function formatOrchestratorStatus(result: {
  presence: {
    herdrSessionName: string;
    paneId: string;
    terminalId: string;
    workspaceId: string;
  };
  state: AgentOrchestratorWireState | null;
}): string {
  const scope = `${result.presence.herdrSessionName}/${result.presence.workspaceId}`;
  const owner = result.state?.owner;
  if (
    owner &&
    owner.terminalId === result.presence.terminalId &&
    result.state?.herdrSessionName === result.presence.herdrSessionName &&
    result.state?.workspaceId === result.presence.workspaceId
  ) {
    return `Herdsman is watching agent updates · ${scope} · ${owner.paneId}`;
  }
  if (owner) {
    return `Herdsman is off · owned by ${owner.paneId} (${scope})`;
  }
  return `Herdsman is off (${scope})`;
}

function formatAgentList(result: { agents?: AgentListItem[] }): string {
  const agents = result.agents ?? [];
  if (agents.length === 0) return "No Herdsman agents indexed.";
  const lines = [
    ["status", "name", "agent", "pane", "last user", "last assistant", "updated"].join("\t"),
  ];
  for (const agent of agents) {
    lines.push(
      [
        agent.agentStatus,
        agent.name ?? "",
        agent.agent ?? "unknown",
        agent.paneId,
        oneLine(agent.history.lastUserMessage?.text ?? ""),
        oneLine(agent.history.lastAssistantMessage?.text ?? ""),
        agent.history.updatedAt ?? "",
      ].join("\t"),
    );
  }
  return lines.join("\n");
}

function formatAgentGet(result: { agent?: AgentGetResult }): string {
  const agent = result.agent;
  if (!agent) return "Agent not found.";
  return [
    `name: ${agent.name ?? "unnamed"}`,
    `agent: ${agent.agent ?? "unknown"}`,
    `status: ${agent.agentStatus}`,
    `pane: ${agent.paneId}`,
    `terminal: ${agent.terminalId ?? "unknown"}`,
    `workspace: ${agent.workspaceId}`,
    `Herdr session: ${agent.herdrSessionName}`,
    `cwd: ${agent.cwd ?? agent.foregroundCwd ?? "unknown"}`,
    `agent_session: ${agent.agentSession ? `${agent.agentSession.source}:${agent.agentSession.value}` : "none"}`,
    `last user: ${oneLine(agent.history.lastUserMessage?.text ?? "")}`,
    `last assistant: ${oneLine(agent.history.lastAssistantMessage?.text ?? "")}`,
    `last tool: ${agent.history.lastToolResult ? `${agent.history.lastToolResult.toolName} ${oneLine(agent.history.lastToolResult.text)}` : ""}`,
  ].join("\n");
}

function formatAgentRead(result: { agent?: AgentReadResult }): string {
  const agent = result.agent;
  if (!agent) return "Agent not found.";
  const lines = [
    `name: ${agent.name ?? "unnamed"}`,
    `agent: ${agent.agent ?? "unknown"}`,
    `pane: ${agent.paneId}`,
    "",
  ];
  for (const message of agent.messages) {
    lines.push(
      [
        message.timestamp ?? "",
        message.role,
        message.toolName ?? "",
        message.compact
          ? `[${message.compact.compaction.mode}] ${oneLine(message.text)}`
          : oneLine(message.text),
      ].join("\t"),
    );
  }
  return lines.join("\n");
}

function oneLine(value: string): string {
  return value.replace(/\s+/g, " ").slice(0, 160);
}

/**
 * Human-readable projection of `daemon status` for `--text`. The default output
 * stays JSON, so scripts and the SKILL contract are unaffected; this view keeps
 * only the fields a human reads at a glance and spells an unavailable restart
 * count out instead of dropping it silently.
 *
 * `restartCount` is rendered only for the system unit that owns the pid. For
 * `unmanaged`/`unknown` the JSON omits it too, and showing a number here would
 * attribute another unit's restarts to this daemon.
 */
export function formatDaemonStatus(status: DaemonStatus): string {
  const lines = [`state: ${status.state}`];
  const pid = "pid" in status ? status.pid : undefined;
  if (pid !== undefined) lines.push(`pid: ${pid}`);
  if ("pidFileMissing" in status && status.pidFileMissing) lines.push("pidFileMissing: true");
  if ("stalePid" in status && status.stalePid !== undefined) {
    lines.push(`stalePid: ${status.stalePid}`);
  }
  if ("socketReachable" in status) lines.push(`socketReachable: ${status.socketReachable}`);
  if (status.managedBy !== undefined) {
    lines.push(`managedBy: ${status.managedBy}`);
    if (isSystemdHerdsmanManaged(status.managedBy)) {
      lines.push(`restartCount: ${status.restartCount ?? "unavailable"}`);
    }
  }
  return lines.join("\n");
}

async function main(): Promise<void> {
  const command = parseCliArgs(argv.slice(2));
  const runtime = resolveRuntimeForCommand();
  if (command.command === "daemon") {
    await runDaemonCommand(command, runtime);
    return;
  }
  await runCliCommand(command, {
    connect: (socketPath, onNotification) =>
      Promise.resolve(
        new ObservabilityRpcClient({
          ...(onNotification ? { onNotification } : {}),
          socketPath,
        }),
      ),
    output: (line) => console.log(line),
    socketPath: runtime.paths.socketPath,
  });
}

async function runDaemonCommand(
  command: Extract<CliCommand, { command: "daemon" }>,
  runtime: ReturnType<typeof resolveRuntimeForCommand>,
): Promise<void> {
  const status = await getDaemonStatus({
    pidPath: runtime.paths.pidPath,
    socketPath: runtime.paths.socketPath,
  });
  console.log(command.text ? formatDaemonStatus(status) : JSON.stringify(status));
}

export function resolveRuntimeForCommand() {
  const paths = runtimePathsFromRecordOrDefault({ environment: process.env });
  const record = readDaemonRuntimeRecord(paths.runtimeRecordPath);
  if (record) return { environment: process.env, homeDir: record.homeDir, paths };
  return resolveRuntime({ environment: process.env });
}

function takeFlag(args: string[], name: string): boolean {
  const index = args.indexOf(name);
  if (index < 0) return false;
  args.splice(index, 1);
  return true;
}

function takeOption(args: string[], name: string): string | undefined {
  const index = args.indexOf(name);
  if (index < 0) return undefined;
  const value = args[index + 1];
  if (!value) throw new Error(`${name} requires a value`);
  args.splice(index, 2);
  return value;
}

function rejectExtra(args: string[]): void {
  if (args.length > 0) throw new Error(`Invalid argument: ${args[0]}`);
}

function isDaemonAction(value: string): value is DaemonAction {
  return value === "status";
}

function formatCliError(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error);
  if (
    message.includes("ENOENT") ||
    message.includes("ECONNREFUSED") ||
    message.includes("Herdsman daemon socket closed") ||
    message.includes("Observability RPC socket closed")
  ) {
    return `${message}\nHerdsman's daemon has no CLI start/stop commands; check the systemd-managed service with \`systemctl status herdsman.service\`.`;
  }
  return message;
}

export function shouldRunCliMain(input: {
  argvPath: string | undefined;
  modulePath: string;
  realArgvPath?: string | undefined;
}): boolean {
  const modulePath = resolve(input.modulePath);
  return [input.argvPath, input.realArgvPath]
    .filter((value): value is string => typeof value === "string" && value.length > 0)
    .map((value) => resolve(value))
    .includes(modulePath);
}

function realpathOrUndefined(path: string | undefined): string | undefined {
  if (!path) return undefined;
  try {
    return realpathSync(path);
  } catch {
    return undefined;
  }
}

async function readAllStdin(): Promise<string> {
  if (processStdin.isTTY) return "";
  const chunks: Buffer[] = [];
  for await (const chunk of processStdin) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(String(chunk)));
  }
  return Buffer.concat(chunks).toString("utf8");
}

const modulePath = fileURLToPath(import.meta.url);
if (
  shouldRunCliMain({
    argvPath: process.argv[1],
    modulePath,
    realArgvPath: realpathOrUndefined(process.argv[1]),
  })
) {
  main().catch((error: unknown) => {
    console.error(formatCliError(error));
    exit(1);
  });
}
