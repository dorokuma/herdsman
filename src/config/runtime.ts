import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, resolve } from "node:path";
import { type ConfigLoadError, loadHerdsmanConfig } from "@/config/load.js";
import type { HerdsmanConfig, StatusPlansConfig } from "@/config/schema.js";
import { readDaemonRuntimeRecord } from "@/daemon/process-manager.js";

export type RuntimePaths = {
  configPath: string;
  dbPath: string;
  envPath: string;
  homeDir: string;
  logPath: string;
  pidPath: string;
  piSessionDir: string;
  runtimeRecordPath: string;
  socketPath: string;
};

export type RuntimeResolution = {
  config?: HerdsmanConfig;
  configErrors?: ConfigLoadError[];
  configPath: string;
  environment: NodeJS.ProcessEnv;
  homeDir: string;
  paths: RuntimePaths;
};

export function getHerdsmanHome(environment: NodeJS.ProcessEnv = process.env): string {
  const explicit = environment.HERDSMAN_HOME?.trim();
  return resolve(explicit && explicit.length > 0 ? explicit : resolve(homedir(), ".herdsman"));
}

export function resolveRuntimePath(homeDir: string, value: string): string {
  return isAbsolute(value) ? value : resolve(homeDir, value);
}

export function resolveRuntimePaths(
  input: { config?: HerdsmanConfig | undefined; environment?: NodeJS.ProcessEnv | undefined } = {},
): RuntimePaths {
  const homeDir = getHerdsmanHome(input.environment);
  const runtime = input.config?.runtime;

  return {
    configPath: resolve(homeDir, "config.yaml"),
    dbPath: resolveRuntimePath(homeDir, runtime?.db_path ?? "state.db"),
    envPath: resolve(homeDir, ".env"),
    homeDir,
    logPath: resolveRuntimePath(homeDir, runtime?.log_path ?? "logs/herdsman.log"),
    pidPath: resolveRuntimePath(homeDir, runtime?.pid_path ?? "herdsman.pid"),
    piSessionDir: resolve(homeDir, "pi-sessions"),
    runtimeRecordPath: resolve(homeDir, "runtime.json"),
    socketPath: resolveRuntimePath(homeDir, runtime?.socket_path ?? "herdsman.sock"),
  };
}

export function loadHerdsmanDotEnv(input: {
  baseEnvironment?: NodeJS.ProcessEnv | undefined;
  envPath: string;
}): NodeJS.ProcessEnv {
  const next: NodeJS.ProcessEnv = { ...(input.baseEnvironment ?? process.env) };
  if (!existsSync(input.envPath)) {
    return next;
  }

  for (const rawLine of readFileSync(input.envPath, "utf8").split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line || line.startsWith("#")) {
      continue;
    }

    const equalsIndex = line.indexOf("=");
    if (equalsIndex <= 0) {
      continue;
    }

    const key = line.slice(0, equalsIndex).trim();
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) {
      continue;
    }
    next[key] = unquoteEnvValue(line.slice(equalsIndex + 1).trim());
  }

  return next;
}

export function runtimePathsFromRecordOrDefault(input: {
  environment?: NodeJS.ProcessEnv | undefined;
  recordPath?: string | undefined;
}): RuntimePaths {
  const defaultPaths = resolveRuntimePaths({ environment: input.environment });
  const record = readDaemonRuntimeRecord(input.recordPath ?? defaultPaths.runtimeRecordPath);
  if (!record) {
    return defaultPaths;
  }

  return {
    ...defaultPaths,
    dbPath: record.dbPath,
    logPath: record.logPath,
    pidPath: record.pidPath,
    socketPath: record.socketPath,
  };
}

export function resolveRuntime(
  input: {
    allowInvalidConfig?: boolean | undefined;
    environment?: NodeJS.ProcessEnv | undefined;
  } = {},
): RuntimeResolution {
  const initialPaths = resolveRuntimePaths({ environment: input.environment });
  const environment = loadHerdsmanDotEnv({
    baseEnvironment: input.environment,
    envPath: initialPaths.envPath,
  });
  const basePaths = resolveRuntimePaths({ environment });

  if (!existsSync(basePaths.configPath)) {
    return {
      configPath: basePaths.configPath,
      environment,
      homeDir: basePaths.homeDir,
      paths: basePaths,
    };
  }

  const loaded = loadHerdsmanConfig(basePaths.configPath);
  if (!loaded.ok) {
    if (input.allowInvalidConfig) {
      return {
        configErrors: loaded.errors,
        configPath: basePaths.configPath,
        environment,
        homeDir: basePaths.homeDir,
        paths: basePaths,
      };
    }
    throw new Error(
      `Invalid Herdsman config: ${loaded.errors.map((error) => error.message).join("; ")}`,
    );
  }

  const paths = resolveRuntimePaths({ config: loaded.value, environment });
  return {
    config: loaded.value,
    configPath: basePaths.configPath,
    environment,
    homeDir: paths.homeDir,
    paths,
  };
}

function unquoteEnvValue(value: string): string {
  if (
    (value.startsWith('"') && value.endsWith('"')) ||
    (value.startsWith("'") && value.endsWith("'"))
  ) {
    return value.slice(1, -1);
  }
  return value;
}

export const DEFAULT_STATUS_PLAN_CONFIG: Required<StatusPlansConfig> = {
  backoff_factor: 2,
  initial_backoff_ms: 5_000,
  max_backoff_ms: 60_000,
  total_window_ms: 15 * 60 * 1000,
};

export function resolveStatusPlansConfig(
  config?: HerdsmanConfig,
  environment: NodeJS.ProcessEnv = process.env,
): Required<StatusPlansConfig> {
  const fromConfig = config?.observability?.status_plans;
  const initialBackoffEnv = environment.HERDSMAN_STATUS_PLAN_INITIAL_BACKOFF_MS?.trim();
  const maxBackoffEnv = environment.HERDSMAN_STATUS_PLAN_MAX_BACKOFF_MS?.trim();
  const factorEnv = environment.HERDSMAN_STATUS_PLAN_BACKOFF_FACTOR?.trim();
  const totalWindowEnv = environment.HERDSMAN_STATUS_PLAN_TOTAL_WINDOW_MS?.trim();

  const initial_backoff_ms =
    initialBackoffEnv && Number(initialBackoffEnv) > 0
      ? Number(initialBackoffEnv)
      : (fromConfig?.initial_backoff_ms ?? DEFAULT_STATUS_PLAN_CONFIG.initial_backoff_ms);
  const max_backoff_ms =
    maxBackoffEnv && Number(maxBackoffEnv) > 0
      ? Number(maxBackoffEnv)
      : (fromConfig?.max_backoff_ms ?? DEFAULT_STATUS_PLAN_CONFIG.max_backoff_ms);
  const backoff_factor =
    factorEnv && Number(factorEnv) >= 1
      ? Number(factorEnv)
      : (fromConfig?.backoff_factor ?? DEFAULT_STATUS_PLAN_CONFIG.backoff_factor);
  const total_window_ms =
    totalWindowEnv && Number(totalWindowEnv) > 0
      ? Number(totalWindowEnv)
      : (fromConfig?.total_window_ms ?? DEFAULT_STATUS_PLAN_CONFIG.total_window_ms);

  return {
    backoff_factor,
    initial_backoff_ms,
    max_backoff_ms,
    total_window_ms,
  };
}
