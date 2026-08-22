import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import {
  getHerdsmanHome,
  loadHerdsmanDotEnv,
  resolveRuntime,
  resolveRuntimePath,
  resolveRuntimePaths,
  runtimePathsFromRecordOrDefault,
} from "@/config/runtime.js";

const tempDirs: string[] = [];

afterEach(() => {
  for (const dir of tempDirs.splice(0)) {
    rmSync(dir, { force: true, recursive: true });
  }
});

describe("Herdsman runtime resolver", () => {
  test("uses ~/.herdsman when SHEPHERD_HOME is absent", () => {
    expect(getHerdsmanHome({})).toBe(resolve(homedir(), ".herdsman"));
  });

  test("uses explicit SHEPHERD_HOME", () => {
    expect(getHerdsmanHome({ SHEPHERD_HOME: "/tmp/herdsman-dev" })).toBe("/tmp/herdsman-dev");
  });

  test("resolves default runtime paths under Herdsman home", () => {
    const homeDir = tempHome();

    const runtime = resolveRuntime({ environment: { SHEPHERD_HOME: homeDir } });

    expect(runtime.paths).toMatchObject({
      configPath: join(homeDir, "config.yaml"),
      dbPath: join(homeDir, "state.db"),
      envPath: join(homeDir, ".env"),
      homeDir,
      logPath: join(homeDir, "logs/herdsman.log"),
      pidPath: join(homeDir, "herdsman.pid"),
      piSessionDir: join(homeDir, "pi-sessions"),
      runtimeRecordPath: join(homeDir, "runtime.json"),
      socketPath: join(homeDir, "herdsman.sock"),
    });
  });

  test("resolves relative runtime config paths from Herdsman home", () => {
    const homeDir = tempHome();
    writeValidConfig(
      homeDir,
      `runtime:
  db_path: data/state.sqlite
  socket_path: sockets/dev.sock
  pid_path: run/dev.pid
  log_path: logs/dev.log
`,
    );

    const runtime = resolveRuntime({ environment: { SHEPHERD_HOME: homeDir } });

    expect(runtime.paths.dbPath).toBe(join(homeDir, "data/state.sqlite"));
    expect(runtime.paths.socketPath).toBe(join(homeDir, "sockets/dev.sock"));
    expect(runtime.paths.pidPath).toBe(join(homeDir, "run/dev.pid"));
    expect(runtime.paths.logPath).toBe(join(homeDir, "logs/dev.log"));
  });

  test("keeps absolute runtime config paths", () => {
    const homeDir = tempHome();
    writeValidConfig(
      homeDir,
      `runtime:
  db_path: /var/tmp/herdsman/state.sqlite
  socket_path: /var/tmp/herdsman/herdsman.sock
  pid_path: /var/tmp/herdsman/herdsman.pid
  log_path: /var/tmp/herdsman/herdsman.log
`,
    );

    const runtime = resolveRuntime({ environment: { SHEPHERD_HOME: homeDir } });

    expect(runtime.paths.dbPath).toBe("/var/tmp/herdsman/state.sqlite");
    expect(runtime.paths.socketPath).toBe("/var/tmp/herdsman/herdsman.sock");
    expect(runtime.paths.pidPath).toBe("/var/tmp/herdsman/herdsman.pid");
    expect(runtime.paths.logPath).toBe("/var/tmp/herdsman/herdsman.log");
  });

  test("loads .env values over shell values while ignoring SHEPHERD variables", () => {
    const homeDir = tempHome();
    const envPath = join(homeDir, "dotenv-test");
    writeFileSync(
      envPath,
      `EXAMPLE_SERVICE_TOKEN=file-token
OPENAI_API_KEY="file-key"
SHEPHERD_HOME=/tmp/ignored
SHEPHERD_INTERNAL_SOCKET_PATH=/tmp/ignored.sock
`,
    );

    const environment = loadHerdsmanDotEnv({
      baseEnvironment: {
        EXISTING: "kept",
        OPENAI_API_KEY: "shell-key",
        SHEPHERD_HOME: homeDir,
      },
      envPath,
    });

    expect(environment.EXAMPLE_SERVICE_TOKEN).toBe("file-token");
    expect(environment.OPENAI_API_KEY).toBe("file-key");
    expect(environment.EXISTING).toBe("kept");
    expect(environment.SHEPHERD_HOME).toBe(homeDir);
    expect(environment.SHEPHERD_INTERNAL_SOCKET_PATH).toBeUndefined();
  });

  test("throws on invalid config unless invalid config is allowed", () => {
    const homeDir = tempHome();
    writeFileSync(join(homeDir, "config.yaml"), "runtime: [");

    expect(() => resolveRuntime({ environment: { SHEPHERD_HOME: homeDir } })).toThrow(
      "Invalid Herdsman config",
    );

    const runtime = resolveRuntime({
      allowInvalidConfig: true,
      environment: { SHEPHERD_HOME: homeDir },
    });
    expect(runtime.configErrors?.length).toBeGreaterThan(0);
    expect(runtime.paths.dbPath).toBe(join(homeDir, "state.db"));
  });

  test("falls back to runtime record paths for management commands", () => {
    const homeDir = tempHome();
    writeFileSync(join(homeDir, "config.yaml"), "runtime: [");
    writeFileSync(
      join(homeDir, "runtime.json"),
      JSON.stringify({
        dbPath: join(homeDir, "last-state.db"),
        homeDir,
        logPath: join(homeDir, "last.log"),
        pid: 1234,
        pidPath: join(homeDir, "last.pid"),
        socketPath: join(homeDir, "last.sock"),
        startedAt: "2026-06-29T00:00:00.000Z",
        version: 1,
      }),
    );

    const runtime = resolveRuntime({
      allowInvalidConfig: true,
      environment: { SHEPHERD_HOME: homeDir },
    });
    const paths = runtimePathsFromRecordOrDefault({ environment: runtime.environment });

    expect(runtime.configErrors?.length).toBeGreaterThan(0);
    expect(paths.dbPath).toBe(join(homeDir, "last-state.db"));
    expect(paths.socketPath).toBe(join(homeDir, "last.sock"));
    expect(paths.pidPath).toBe(join(homeDir, "last.pid"));
    expect(paths.logPath).toBe(join(homeDir, "last.log"));
  });

  test("falls back to home defaults when runtime record is missing", () => {
    const homeDir = tempHome();
    writeFileSync(join(homeDir, "config.yaml"), "runtime: [");

    const runtime = resolveRuntime({
      allowInvalidConfig: true,
      environment: { SHEPHERD_HOME: homeDir },
    });
    const paths = runtimePathsFromRecordOrDefault({ environment: runtime.environment });

    expect(runtime.configErrors?.length).toBeGreaterThan(0);
    expect(paths.dbPath).toBe(join(homeDir, "state.db"));
    expect(paths.socketPath).toBe(join(homeDir, "herdsman.sock"));
    expect(paths.pidPath).toBe(join(homeDir, "herdsman.pid"));
    expect(paths.logPath).toBe(join(homeDir, "logs/herdsman.log"));
  });

  test("resolves explicit runtime path values", () => {
    expect(resolveRuntimePath("/home/herdsman", "state.db")).toBe("/home/herdsman/state.db");
    expect(resolveRuntimePath("/home/herdsman", "/tmp/state.db")).toBe("/tmp/state.db");
  });

  test("resolves paths from an already loaded config", () => {
    const paths = resolveRuntimePaths({
      config: {
        runtime: { db_path: "data/state.db" },
      },
      environment: { SHEPHERD_HOME: "/tmp/herdsman-home" },
    });

    expect(paths.dbPath).toBe("/tmp/herdsman-home/data/state.db");
  });
});

function tempHome(): string {
  const dir = mkdtempSync(join(tmpdir(), "herdsman-runtime-"));
  tempDirs.push(dir);
  mkdirSync(dir, { recursive: true });
  return dir;
}

function writeValidConfig(homeDir: string, extraYaml = ""): void {
  writeFileSync(
    join(homeDir, "config.yaml"),
    `${extraYaml}observability:
  telemetry: {}
`,
  );
}
