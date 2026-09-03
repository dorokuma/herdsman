import { spawn } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import { resolveRuntime } from "@/config/runtime.js";
import {
  PeriodicReconcileScheduler,
  RECONCILE_INTERVAL_MS,
  resolveMigrationsFolder,
  runObservabilityDaemonService,
} from "@/daemon/service.js";

const tempDirs: string[] = [];
const children: Array<import("node:child_process").ChildProcess> = [];

const repoRoot = process.cwd();

function killChildProcesses(): void {
  for (const child of children.splice(0)) {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  }
}

afterEach(() => {
  killChildProcesses();
  for (const dir of tempDirs.splice(0)) rmSync(dir, { force: true, recursive: true });
});

describe("daemon service", () => {
  test("finds migrations from the package root instead of the launch cwd", () => {
    const root = mkdtempSync(join(tmpdir(), "herdsman-daemon-service-"));
    tempDirs.push(root);
    const moduleDir = join(root, "dist", "src", "daemon");
    const migrationsFolder = join(root, "drizzle");
    mkdirSync(moduleDir, { recursive: true });
    mkdirSync(join(migrationsFolder, "meta"), { recursive: true });
    writeFileSync(join(migrationsFolder, "meta", "_journal.json"), "{}\n");

    expect(resolveMigrationsFolder(moduleDir)).toBe(migrationsFolder);
  });
});

describe("periodic reconcile scheduling", () => {
  function scheduler(options: {
    run: () => Promise<void>;
    onClear?: () => void;
    onSet?: (callback: () => void) => void;
  }) {
    const { run, onClear, onSet } = options;
    return new PeriodicReconcileScheduler({
      intervalMs: RECONCILE_INTERVAL_MS,
      run,
      setInterval: (callback) => {
        onSet?.(callback);
        return 1 as unknown as ReturnType<typeof setInterval>;
      },
      clearInterval: () => onClear?.(),
    });
  }

  test("fires the reconcile run on the configured cadence", async () => {
    let tick: (() => void) | undefined;
    const runs: number[] = [];
    const instance = scheduler({
      run: async () => {
        runs.push(1);
      },
      onSet: (callback) => {
        tick = callback;
      },
    });
    instance.start();
    expect(runs).toHaveLength(0);
    tick?.();
    await vi.waitFor(() => expect(runs).toHaveLength(1));
    tick?.();
    await vi.waitFor(() => expect(runs).toHaveLength(2));
    await instance.stop();
  });

  test("skips ticks while a reconcile is in flight and resumes after it settles", async () => {
    let tick: (() => void) | undefined;
    let resolveRun: (() => void) | undefined;
    const runs: string[] = [];
    const instance = scheduler({
      run: () =>
        new Promise<void>((resolve) => {
          resolveRun = resolve;
          runs.push("started");
        }),
      onSet: (callback) => {
        tick = callback;
      },
    });
    instance.start();
    tick?.();
    await vi.waitFor(() => expect(runs).toEqual(["started"]));
    // Two more ticks while the first run is still in flight: both are skipped.
    tick?.();
    tick?.();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(runs).toEqual(["started"]);
    // Settle the in-flight run and let its promise chain (catch + finally that
    // clears the in-flight guard) fully drain before the next tick.
    resolveRun?.();
    await new Promise((resolve) => setTimeout(resolve, 0));
    tick?.();
    await vi.waitFor(() => expect(runs).toEqual(["started", "started"]));
    resolveRun?.();
    await instance.stop();
  });

  test("stop clears the interval and awaits the in-flight run", async () => {
    let tick: (() => void) | undefined;
    let cleared = 0;
    let resolveRun: (() => void) | undefined;
    let runSettled = false;
    const instance = scheduler({
      run: () =>
        new Promise<void>((resolve) => {
          resolveRun = resolve;
        }).finally(() => {
          runSettled = true;
        }),
      onClear: () => {
        cleared += 1;
      },
      onSet: (callback) => {
        tick = callback;
      },
    });
    instance.start();
    tick?.();
    await vi.waitFor(() => expect(resolveRun).toBeDefined());
    const stopped = instance.stop();
    resolveRun?.();
    await stopped;
    expect(cleared).toBe(1);
    expect(runSettled).toBe(true);
  });

  test("start is idempotent and a rejected run does not break the cadence", async () => {
    let tick: (() => void) | undefined;
    const warning = vi.spyOn(console, "warn").mockImplementation(() => {});
    const runs: string[] = [];
    const instance = scheduler({
      run: async () => {
        runs.push("attempt");
        throw new Error("boom");
      },
      onSet: (callback) => {
        tick = callback;
      },
    });
    instance.start();
    instance.start();
    tick?.();
    await vi.waitFor(() => expect(runs).toEqual(["attempt"]));
    expect(warning).toHaveBeenCalledWith("Herdsman periodic reconcile failed", expect.any(Error));
    // Let the catch/finally chain clear the in-flight guard before ticking again.
    await new Promise((resolve) => setTimeout(resolve, 0));
    tick?.();
    await vi.waitFor(() => expect(runs).toEqual(["attempt", "attempt"]));
    warning.mockRestore();
    await instance.stop();
  });
});

describe("daemon service lifecycle and socket guard", () => {
  function createSignalTarget() {
    const listeners: Record<string, (() => Promise<void> | void)[]> = {};
    return {
      listeners,
      target: {
        once: (event: "SIGINT" | "SIGTERM", listener: () => Promise<void> | void) => {
          if (!listeners[event]) listeners[event] = [];
          listeners[event].push(listener);
        },
        off: (event: "SIGINT" | "SIGTERM", listener: () => Promise<void> | void) => {
          listeners[event] = (listeners[event] ?? []).filter((l) => l !== listener);
        },
      },
      emit: async (event: "SIGINT" | "SIGTERM") => {
        const list = listeners[event]?.splice(0) ?? [];
        for (const listener of list) {
          await listener();
        }
      },
    };
  }

  test("refuses to start and preserves socket when socket is reachable", async () => {
    const root = mkdtempSync(join(tmpdir(), "herdsman-guard-"));
    tempDirs.push(root);
    const runtime = resolveRuntime({ environment: { HERDSMAN_HOME: root } });
    mkdirSync(root, { recursive: true });
    writeFileSync(runtime.paths.socketPath, "dummy-socket");

    const exitCodes: number[] = [];
    await expect(
      runObservabilityDaemonService({
        connectSocket: async () => true,
        environment: { HERDSMAN_HOME: root },
        exit: (code) => {
          exitCodes.push(code ?? 0);
        },
        pid: 1234,
        sessionList: async () => [],
      }),
    ).rejects.toThrow("Herdsman daemon socket is already reachable");

    // PID file was not written
    expect(existsSync(runtime.paths.pidPath)).toBe(false);
    // Socket file was not unlinked
    expect(existsSync(runtime.paths.socketPath)).toBe(true);
    expect(exitCodes).toHaveLength(0);
  });

  test("unlinks stale socket, writes pid on start, and cleans pid on SIGTERM", async () => {
    const root = mkdtempSync(join(tmpdir(), "herdsman-guard-"));
    tempDirs.push(root);
    const runtime = resolveRuntime({ environment: { HERDSMAN_HOME: root } });
    mkdirSync(root, { recursive: true });
    writeFileSync(runtime.paths.socketPath, "stale-socket");

    const exitCodes: number[] = [];
    const signals = createSignalTarget();

    await runObservabilityDaemonService({
      connectSocket: async () => false,
      environment: { HERDSMAN_HOME: root },
      exit: (code) => {
        exitCodes.push(code ?? 0);
      },
      pid: 5678,
      sessionList: async () => [],
      signalTarget: signals.target,
    });

    // PID file written after server.start()
    expect(existsSync(runtime.paths.pidPath)).toBe(true);
    expect(readFileSync(runtime.paths.pidPath, "utf8")).toBe("5678\n");

    // Send SIGTERM
    await signals.emit("SIGTERM");

    // PID file removed
    expect(existsSync(runtime.paths.pidPath)).toBe(false);
    expect(exitCodes).toEqual([0]);
  });

  test("preserves foreign pid file when stop() runs", async () => {
    const root = mkdtempSync(join(tmpdir(), "herdsman-guard-"));
    tempDirs.push(root);
    const runtime = resolveRuntime({ environment: { HERDSMAN_HOME: root } });
    mkdirSync(root, { recursive: true });

    const exitCodes: number[] = [];
    const signals = createSignalTarget();

    await runObservabilityDaemonService({
      environment: { HERDSMAN_HOME: root },
      exit: (code) => {
        exitCodes.push(code ?? 0);
      },
      pid: 5678,
      sessionList: async () => [],
      signalTarget: signals.target,
    });

    expect(readFileSync(runtime.paths.pidPath, "utf8")).toBe("5678\n");

    // Overwrite pid file with foreign PID (e.g. from an external supervisor or concurrent process)
    writeFileSync(runtime.paths.pidPath, "99999\n");

    // Send SIGTERM to stop this daemon (pid 5678)
    await signals.emit("SIGTERM");

    // Foreign PID file must NOT be deleted
    expect(existsSync(runtime.paths.pidPath)).toBe(true);
    expect(readFileSync(runtime.paths.pidPath, "utf8")).toBe("99999\n");
    expect(exitCodes).toEqual([0]);
  });

  test("falls back to exit(1) and removes pid file when stop encounters an unexpected error", async () => {
    const root = mkdtempSync(join(tmpdir(), "herdsman-guard-"));
    tempDirs.push(root);
    const runtime = resolveRuntime({ environment: { HERDSMAN_HOME: root } });
    mkdirSync(root, { recursive: true });

    const exitCodes: number[] = [];
    const signals = createSignalTarget();
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});

    await runObservabilityDaemonService({
      environment: { HERDSMAN_HOME: root },
      exit: (code) => {
        exitCodes.push(code ?? 0);
      },
      pid: 5678,
      reconcileClearInterval: () => {
        throw new Error("unexpected scheduler crash");
      },
      sessionList: async () => [],
      signalTarget: signals.target,
    });

    expect(existsSync(runtime.paths.pidPath)).toBe(true);

    await signals.emit("SIGTERM");

    // Finally block still cleaned matching PID file
    expect(existsSync(runtime.paths.pidPath)).toBe(false);
    // Exited with 1 due to caught shutdown failure
    expect(exitCodes).toEqual([1]);
    expect(consoleError).toHaveBeenCalledWith("Herdsman daemon shutdown failed", expect.any(Error));
    consoleError.mockRestore();
  });

  test("a second daemon on the same HERDSMAN_HOME fails on the instance lock; SIGTERM releases it and allows restart", async () => {
    const root = mkdtempSync(join(tmpdir(), "herdsman-instance-lock-"));
    tempDirs.push(root);
    const runtime = resolveRuntime({ environment: { HERDSMAN_HOME: root } });
    mkdirSync(root, { recursive: true });

    // A real daemon process whose entrypoint basename is herdsman-daemon.js
    // (so the instance lock's identity probe recognizes it) holds the lock for
    // this HERDSMAN_HOME. The wrapper runs the actual service via tsx.
    const wrapperPath = join(root, "herdsman-daemon.js");
    writeFileSync(
      wrapperPath,
      [
        `import { runObservabilityDaemonService } from ${JSON.stringify(join(repoRoot, "src/daemon/service.ts"))};`,
        "runObservabilityDaemonService({ environment: process.env, sessionList: async () => [] });",
      ].join("\n"),
    );
    const child = spawn(process.execPath, ["--import", "tsx", wrapperPath], {
      cwd: repoRoot,
      env: { ...process.env, HERDSMAN_HOME: root },
      stdio: ["ignore", "pipe", "pipe"],
    });
    children.push(child);
    let childOutput = "";
    child.stdout.on("data", (chunk) => (childOutput += String(chunk)));
    child.stderr.on("data", (chunk) => (childOutput += String(chunk)));
    const childExit = new Promise<number | null>((resolve) => {
      child.once("exit", (code) => resolve(code));
    });

    try {
      // The pid file is written only after the instance lock is acquired and
      // the server is listening, so its presence proves the lock is held.
      await vi.waitFor(
        () => {
          if (!existsSync(runtime.paths.pidPath)) {
            throw new Error(`child daemon not ready; output: ${childOutput}`);
          }
          const pid = Number(readFileSync(runtime.paths.pidPath, "utf8").trim());
          if (pid !== child.pid) throw new Error("pid file belongs to another daemon");
        },
        { timeout: 20_000, interval: 100 },
      );
      expect(existsSync(`${runtime.paths.pidPath}.instance.lock`)).toBe(true);

      const exitCodes: number[] = [];
      const signals = createSignalTarget();
      await expect(
        runObservabilityDaemonService({
          environment: { HERDSMAN_HOME: root },
          exit: (code) => {
            exitCodes.push(code ?? 0);
          },
          pid: 2222,
          sessionList: async () => [],
          signalTarget: signals.target,
        }),
      ).rejects.toThrow(/Herdsman daemon operation lock is held/);
      expect(exitCodes).toHaveLength(0);

      // Stopping the child daemon releases the instance lock.
      child.kill("SIGTERM");
      const exitCode = await Promise.race([
        childExit,
        new Promise<null>((resolve) => setTimeout(() => resolve(null), 20_000)),
      ]);
      expect(exitCode).toBe(0);
      expect(childOutput).not.toContain("shutdown failed");
      expect(existsSync(`${runtime.paths.pidPath}.instance.lock`)).toBe(false);
      expect(existsSync(runtime.paths.pidPath)).toBe(false);

      // With the lock released, the same HERDSMAN_HOME can be started again.
      await runObservabilityDaemonService({
        environment: { HERDSMAN_HOME: root },
        exit: (code) => {
          exitCodes.push(code ?? 0);
        },
        pid: 3333,
        sessionList: async () => [],
        signalTarget: signals.target,
      });
      expect(readFileSync(runtime.paths.pidPath, "utf8")).toBe("3333\n");
      await signals.emit("SIGTERM");
      expect(exitCodes).toEqual([0]);
    } finally {
      killChildProcesses();
    }
  });

  test("simulates double-start race between two daemons sharing socket path", async () => {
    const root = mkdtempSync(join(tmpdir(), "herdsman-race-"));
    tempDirs.push(root);
    const runtime = resolveRuntime({ environment: { HERDSMAN_HOME: root } });
    mkdirSync(root, { recursive: true });

    const exitCodes1: number[] = [];
    const signals1 = createSignalTarget();

    // Start primary daemon 1
    await runObservabilityDaemonService({
      environment: { HERDSMAN_HOME: root },
      exit: (code) => {
        exitCodes1.push(code ?? 0);
      },
      pid: 1001,
      sessionList: async () => [],
      signalTarget: signals1.target,
    });

    expect(readFileSync(runtime.paths.pidPath, "utf8")).toBe("1001\n");

    // Daemon 2 attempts to start with the same HERDSMAN_HOME while Daemon 1 is running and listening
    const exitCodes2: number[] = [];
    const signals2 = createSignalTarget();

    await expect(
      runObservabilityDaemonService({
        environment: { HERDSMAN_HOME: root },
        exit: (code) => {
          exitCodes2.push(code ?? 0);
        },
        pid: 2002,
        sessionList: async () => [],
        signalTarget: signals2.target,
      }),
    ).rejects.toThrow("Herdsman daemon socket is already reachable");

    // Daemon 1's PID file is untouched
    expect(readFileSync(runtime.paths.pidPath, "utf8")).toBe("1001\n");
    // Daemon 2 never exited via stop
    expect(exitCodes2).toHaveLength(0);

    // Shutdown Daemon 1 cleanly
    await signals1.emit("SIGTERM");
    expect(existsSync(runtime.paths.pidPath)).toBe(false);
    expect(exitCodes1).toEqual([0]);
  });
});
