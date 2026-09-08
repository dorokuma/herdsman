import { spawn } from "node:child_process";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import readline from "node:readline";
import { afterEach, describe, expect, test, vi } from "vitest";
import {
  acquireDaemonLock,
  acquireFlockHandle,
  type DaemonRuntimeRecord,
  daemonInstanceLockPath,
  getDaemonStatus,
  isFlockHeld,
  isProcessRunning,
  prepareDaemonSocketPath,
  readDaemonProcessIdentity,
  readDaemonRuntimeRecord,
  releaseDaemonLock,
  removeDaemonPidFile,
  startDaemonProcess,
  stopDaemonProcess,
  withDaemonLock,
  writeDaemonPidFile,
  writeDaemonRuntimeRecord,
} from "@/daemon/process-manager.js";

const tempDirs: string[] = [];

afterEach(() => {
  for (const dir of tempDirs.splice(0)) {
    rmSync(dir, { force: true, recursive: true });
  }
});

function tempDir(): string {
  const dir = mkdtempSync(join(tmpdir(), "herdsman-daemon-process-"));
  tempDirs.push(dir);
  return dir;
}

describe("daemon process manager", () => {
  test("reports stopped when the pid file does not exist", async () => {
    const dir = tempDir();
    await expect(
      getDaemonStatus({ pidPath: join(dir, "missing.pid"), socketPath: "/tmp/herdsman.sock" }),
    ).resolves.toEqual({
      pidPath: join(dir, "missing.pid"),
      socketPath: "/tmp/herdsman.sock",
      state: "stopped",
    });
  });

  test("reports running when the pid file is missing but the daemon socket is reachable", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "missing.pid");

    await expect(
      getDaemonStatus({
        deps: { connectSocket: async () => true },
        pidPath,
        socketPath: "/tmp/herdsman.sock",
      }),
    ).resolves.toEqual({
      pidPath,
      pidFileMissing: true,
      socketPath: "/tmp/herdsman.sock",
      socketReachable: true,
      state: "running",
    });
  });

  test("treats an EPERM process probe as a live process", () => {
    const probe = () => {
      throw Object.assign(new Error("operation not permitted"), { code: "EPERM" });
    };

    expect(isProcessRunning(1234, probe)).toBe(true);
  });

  test("treats an ESRCH process probe as a stopped process", () => {
    const probe = () => {
      throw Object.assign(new Error("no such process"), { code: "ESRCH" });
    };

    expect(isProcessRunning(1234, probe)).toBe(false);
  });

  test("reports running when the pid file contains a live process", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");

    await expect(
      getDaemonStatus({
        deps: {
          connectSocket: async () => true,
          identityProbe: () => true,
          isProcessRunning: (pid) => pid === 1234,
        },
        pidPath,
        socketPath: "/tmp/herdsman.sock",
      }),
    ).resolves.toEqual({
      pid: 1234,
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      socketReachable: true,
      state: "running",
    });
  });

  test("reports running with stalePid metadata when the pid is stale but the daemon socket is reachable", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");

    await expect(
      getDaemonStatus({
        deps: {
          connectSocket: async () => true,
          isProcessRunning: () => false,
        },
        pidPath,
        socketPath: "/tmp/herdsman.sock",
      }),
    ).resolves.toEqual({
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      socketReachable: true,
      stalePid: 1234,
      state: "running",
    });
  });

  test("writes the daemon pid file with 0o600 mode and creates parent directories", () => {
    const dir = tempDir();
    const nested = join(dir, "nested");
    const pidPath = join(nested, "herdsman.pid");

    writeDaemonPidFile(pidPath, 4242);

    expect(readFileSync(pidPath, "utf8")).toBe("4242\n");
    expect(statSync(pidPath).mode & 0o777).toBe(0o600);
  });

  test("removes the daemon pid file only when its content matches the expected pid", () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");

    // Missing file: no-op
    expect(removeDaemonPidFile(pidPath, 4242)).toBe(false);
    expect(existsSync(pidPath)).toBe(false);

    // Foreign pid: file must be left untouched
    writeFileSync(pidPath, "9999\n");
    expect(removeDaemonPidFile(pidPath, 4242)).toBe(false);
    expect(readFileSync(pidPath, "utf8")).toBe("9999\n");

    // Matching pid: removed
    expect(removeDaemonPidFile(pidPath, 9999)).toBe(true);
    expect(existsSync(pidPath)).toBe(false);
  });

  test("writes and reads a daemon runtime record", () => {
    const dir = tempDir();
    const recordPath = join(dir, "runtime.json");
    const record = runtimeRecord(dir);

    writeDaemonRuntimeRecord(recordPath, record);

    expect(readDaemonRuntimeRecord(recordPath)).toEqual(record);
    expect(statSync(recordPath).mode & 0o777).toBe(0o600);
  });

  test("returns undefined for missing or invalid runtime records", () => {
    const dir = tempDir();
    expect(readDaemonRuntimeRecord(join(dir, "missing.json"))).toBeUndefined();

    const invalidPath = join(dir, "runtime.json");
    writeFileSync(invalidPath, "not-json");
    expect(readDaemonRuntimeRecord(invalidPath)).toBeUndefined();
  });

  test("refuses to remove a reachable daemon socket", async () => {
    const dir = tempDir();
    const socketPath = join(dir, "herdsman.sock");
    writeFileSync(socketPath, "socket-placeholder");

    await expect(
      prepareDaemonSocketPath({
        deps: { connectSocket: async () => true },
        socketPath,
      }),
    ).rejects.toThrow("Herdsman daemon socket is already reachable");
    expect(existsSync(socketPath)).toBe(true);
  });

  test("removes an unreachable stale daemon socket", async () => {
    const dir = tempDir();
    const socketPath = join(dir, "herdsman.sock");
    writeFileSync(socketPath, "socket-placeholder");

    await prepareDaemonSocketPath({
      deps: { connectSocket: async () => false },
      socketPath,
    });

    expect(existsSync(socketPath)).toBe(false);
  });

  test("refuses to start when the daemon is already running", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");

    await expect(
      startDaemonProcess({
        deps: {
          connectSocket: async () => true,
          identityProbe: () => true,
          isProcessRunning: (pid) => pid === 1234,
          spawnProcess: () => ({ pid: 5678, unref() {} }),
        },
        entrypointPath: "/repo/dist/src/cli/herdsman-daemon.js",
        env: {},
        logPath: join(dir, "herdsman.log"),
        nodePath: "/usr/bin/node",
        pidPath,
        runtimeRecord: runtimeRecord(dir),
        runtimeRecordPath: join(dir, "runtime.json"),
        socketPath: "/tmp/herdsman.sock",
      }),
    ).rejects.toThrow("Herdsman daemon is already running with pid 1234");
  });

  test("refuses to start when a daemon socket is reachable even if the pid file is stale", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");
    let spawned = false;

    await expect(
      startDaemonProcess({
        deps: {
          connectSocket: async () => true,
          isProcessRunning: () => false,
          spawnProcess: () => {
            spawned = true;
            return { pid: 5678, unref() {} };
          },
        },
        entrypointPath: "/repo/dist/src/cli/herdsman-daemon.js",
        env: {},
        logPath: join(dir, "herdsman.log"),
        nodePath: "/usr/bin/node",
        pidPath,
        runtimeRecord: runtimeRecord(dir),
        runtimeRecordPath: join(dir, "runtime.json"),
        socketPath: "/tmp/herdsman.sock",
      }),
    ).rejects.toThrow("Herdsman daemon is already running");
    expect(spawned).toBe(false);
    // The stale pid file must be preserved: it is metadata for the running daemon
    expect(readFileSync(pidPath, "utf8")).toBe("1234\n");
  });

  test("refuses to start when the daemon process is alive but its socket is unreachable", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");
    let spawned = false;

    await expect(
      startDaemonProcess({
        deps: {
          connectSocket: async () => false,
          identityProbe: () => true,
          isProcessRunning: () => true,
          spawnProcess: () => {
            spawned = true;
            return { pid: 5678, unref() {} };
          },
        },
        entrypointPath: "/repo/dist/src/cli/herdsman-daemon.js",
        env: {},
        logPath: join(dir, "herdsman.log"),
        nodePath: "/usr/bin/node",
        pidPath,
        runtimeRecord: runtimeRecord(dir),
        runtimeRecordPath: join(dir, "runtime.json"),
        socketPath: "/tmp/herdsman.sock",
      }),
    ).rejects.toThrow(
      "Herdsman daemon process is already running with pid 1234 but its socket is not reachable",
    );
    expect(spawned).toBe(false);
  });

  test("starts normally when the pid file is stale and the socket is unreachable", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");

    const result = await startDaemonProcess({
      deps: {
        connectSocket: async () => false,
        isProcessRunning: () => false,
        readinessProbe: async () => true,
        spawnProcess: () => ({ pid: 5678, unref() {} }),
      },
      entrypointPath: "/repo/dist/src/cli/herdsman-daemon.js",
      env: {},
      logPath: join(dir, "herdsman.log"),
      nodePath: "/usr/bin/node",
      pidPath,
      runtimeRecord: runtimeRecord(dir),
      runtimeRecordPath: join(dir, "runtime.json"),
      socketPath: "/tmp/herdsman.sock",
    });

    expect(result).toEqual({ pid: 5678 });
    expect(readFileSync(pidPath, "utf8")).toBe("5678\n");
  });

  test("starts a detached daemon process and writes its pid and runtime record", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    const logPath = join(dir, "herdsman.log");
    const runtimeRecordPath = join(dir, "runtime.json");
    const spawned: unknown[] = [];

    const result = await startDaemonProcess({
      deps: {
        readinessProbe: async () => true,
        spawnProcess: (command, args, options) => {
          spawned.push({ args, command, options });
          return { pid: 5678, unref() {} };
        },
      },
      entrypointPath: "/repo/dist/src/cli/herdsman-daemon.js",
      env: { PATH: "/bin" },
      logPath,
      nodePath: "/usr/bin/node",
      pidPath,
      runtimeRecord: runtimeRecord(dir),
      runtimeRecordPath,
      socketPath: "/tmp/herdsman.sock",
    });

    expect(result).toEqual({ pid: 5678 });
    expect(readFileSync(pidPath, "utf8")).toBe("5678\n");
    const record = readDaemonRuntimeRecord(runtimeRecordPath);
    expect(record).toMatchObject({
      dbPath: join(dir, "state.db"),
      homeDir: dir,
      logPath,
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      version: 1,
    });
    // The runtime record deliberately carries no pid: the pid file is the pid
    // source of truth, and a stale pid would mislead liveness checks.
    expect(record?.pid).toBeUndefined();
    expect(spawned).toMatchObject([
      {
        args: ["/repo/dist/src/cli/herdsman-daemon.js"],
        command: "/usr/bin/node",
        options: { detached: true, env: { PATH: "/bin" } },
      },
    ]);
    expect(JSON.stringify(spawned)).not.toContain("--daemon-run");
    expect(JSON.stringify(spawned)).not.toContain("--db");
    expect(JSON.stringify(spawned)).not.toContain("--socket");
    expect(JSON.stringify(spawned)).not.toContain("--config");
    expect(existsSync(logPath)).toBe(true);
  });

  test("readiness failure escalates to SIGKILL and retains pid when death cannot be confirmed", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    const signals: NodeJS.Signals[] = [];
    await expect(
      startDaemonProcess({
        deps: {
          readinessProbe: async () => false,
          readinessTimeoutMs: 1,
          isProcessRunning: () => true,
          killProcess: (_pid, signal) => signals.push(signal),
          spawnProcess: () => ({ pid: 1234, unref() {} }),
          waitMs: async () => undefined,
        },
        entrypointPath: "/repo/daemon.js",
        env: {},
        logPath: join(dir, "daemon.log"),
        nodePath: "/usr/bin/node",
        pidPath,
        runtimeRecord: runtimeRecord(dir),
        runtimeRecordPath: join(dir, "runtime.json"),
        socketPath: "/tmp/missing.sock",
      }),
    ).rejects.toThrow("Timed out waiting");
    expect(signals).toEqual(["SIGTERM", "SIGKILL"]);
    expect(existsSync(pidPath)).toBe(true);
  });

  test("sends SIGTERM and removes the pid file after the process disappears", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");
    const signals: Array<{ pid: number; signal: NodeJS.Signals }> = [];
    let running = true;

    const result = await stopDaemonProcess({
      deps: {
        connectSocket: async () => true,
        identityProbe: () => true,
        isProcessRunning: (pid) => pid === 1234 && running,
        killProcess: (pid, signal) => {
          signals.push({ pid, signal });
          running = false;
        },
        waitMs: async () => undefined,
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      timeoutMs: 100,
    });

    expect(result).toEqual({ alreadyStopped: false, pid: 1234 });
    expect(signals).toEqual([{ pid: 1234, signal: "SIGTERM" }]);
    expect(existsSync(pidPath)).toBe(false);
  });

  test("reports stopped with stalePid when PID is live but identity probe returns false (PID reuse)", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");

    const status = await getDaemonStatus({
      deps: {
        connectSocket: async () => false,
        identityProbe: () => false,
        isProcessRunning: () => true,
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
    });

    expect(status).toEqual({
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      stalePid: 1234,
      state: "stopped",
    });
  });

  test("stopDaemonProcess does not send signals, removes pid file, and returns alreadyStopped when PID is reused by another process", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");
    const signals: Array<{ pid: number; signal: NodeJS.Signals }> = [];

    const result = await stopDaemonProcess({
      deps: {
        connectSocket: async () => false,
        identityProbe: () => false,
        isProcessRunning: () => true,
        killProcess: (pid, signal) => signals.push({ pid, signal }),
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      timeoutMs: 100,
    });

    expect(result).toEqual({ alreadyStopped: true });
    expect(signals).toHaveLength(0);
    expect(existsSync(pidPath)).toBe(false);
  });

  test("startDaemonProcess cleans stale PID file and starts daemon when old PID was reused by another process", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");

    const result = await startDaemonProcess({
      deps: {
        connectSocket: async () => false,
        identityProbe: () => false,
        isProcessRunning: () => true,
        readinessProbe: async () => true,
        spawnProcess: () => ({ pid: 5678, unref() {} }),
      },
      entrypointPath: "/repo/dist/src/cli/herdsman-daemon.js",
      env: {},
      logPath: join(dir, "herdsman.log"),
      nodePath: "/usr/bin/node",
      pidPath,
      runtimeRecord: runtimeRecord(dir),
      runtimeRecordPath: join(dir, "runtime.json"),
      socketPath: "/tmp/herdsman.sock",
    });

    expect(result).toEqual({ pid: 5678 });
    expect(readFileSync(pidPath, "utf8")).toBe("5678\n");
  });

  test("reports running with stalePid when PID is reused but socket is reachable, and stop/start refuse without killing", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");
    const signals: Array<{ pid: number; signal: NodeJS.Signals }> = [];
    let spawned = false;

    const status = await getDaemonStatus({
      deps: {
        connectSocket: async () => true,
        identityProbe: () => false,
        isProcessRunning: () => true,
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
    });

    expect(status).toEqual({
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      socketReachable: true,
      stalePid: 1234,
      state: "running",
    });

    await expect(
      stopDaemonProcess({
        deps: {
          connectSocket: async () => true,
          identityProbe: () => false,
          isProcessRunning: () => true,
          killProcess: (pid, signal) => signals.push({ pid, signal }),
        },
        pidPath,
        socketPath: "/tmp/herdsman.sock",
        timeoutMs: 100,
      }),
    ).rejects.toThrow("daemon is managed outside this pid file");
    expect(signals).toHaveLength(0);
    expect(existsSync(pidPath)).toBe(true);

    await expect(
      startDaemonProcess({
        deps: {
          connectSocket: async () => true,
          identityProbe: () => false,
          isProcessRunning: () => true,
          spawnProcess: () => {
            spawned = true;
            return { pid: 5678, unref() {} };
          },
        },
        entrypointPath: "/repo/dist/src/cli/herdsman-daemon.js",
        env: {},
        logPath: join(dir, "herdsman.log"),
        nodePath: "/usr/bin/node",
        pidPath,
        runtimeRecord: runtimeRecord(dir),
        runtimeRecordPath: join(dir, "runtime.json"),
        socketPath: "/tmp/herdsman.sock",
      }),
    ).rejects.toThrow("Herdsman daemon is already running");
    expect(spawned).toBe(false);
  });

  test("probe undefined degrades to running with console.warn (throttled)", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});

    const status1 = await getDaemonStatus({
      deps: {
        connectSocket: async () => true,
        identityProbe: () => undefined,
        isProcessRunning: () => true,
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
    });

    expect(status1).toEqual({
      pid: 1234,
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      socketReachable: true,
      state: "running",
    });
    expect(warnSpy).toHaveBeenCalledTimes(1);

    // Calling again with the same PID should not log another warning
    const status2 = await getDaemonStatus({
      deps: {
        connectSocket: async () => true,
        identityProbe: () => undefined,
        isProcessRunning: () => true,
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
    });
    expect(status2.state).toBe("running");
    expect(warnSpy).toHaveBeenCalledTimes(1);

    warnSpy.mockRestore();
  });

  test("readDaemonProcessIdentity safely returns false on non-existent PID or read failure", () => {
    const result = readDaemonProcessIdentity(999999999);
    if (process.platform === "linux" && existsSync("/proc")) {
      expect(result).toBe(false);
    } else {
      expect(result).toBeUndefined();
    }
  });

  test("daemon identity probe narrows to herdsman-daemon.js and rejects non-daemon entrypoints", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, `${process.pid}\n`);
    const signals: Array<{ pid: number; signal: NodeJS.Signals }> = [];

    // Current test runner process is not "herdsman-daemon.js"
    // When getDaemonStatus runs without mock probe on Linux, identity probe returns false
    const status = await getDaemonStatus({
      deps: {
        connectSocket: async () => false,
        isProcessRunning: () => true,
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
    });

    if (process.platform === "linux" && existsSync("/proc")) {
      expect(status).toEqual({
        pidPath,
        socketPath: "/tmp/herdsman.sock",
        stalePid: process.pid,
        state: "stopped",
      });
    }

    const stopResult = await stopDaemonProcess({
      deps: {
        connectSocket: async () => false,
        identityProbe: (_pid, expectedNames) => {
          // If argv contains herdsman but expectedNames is DAEMON_ENTRYPOINT_NAMES (["herdsman-daemon.js"]), reject
          if (
            expectedNames &&
            !expectedNames.includes("herdsman.js") &&
            !expectedNames.includes("herdsman")
          ) {
            return false;
          }
          return true;
        },
        isProcessRunning: () => true,
        killProcess: (pid, signal) => signals.push({ pid, signal }),
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      timeoutMs: 100,
    });

    expect(stopResult).toEqual({ alreadyStopped: true });
    expect(signals).toHaveLength(0);
    expect(existsSync(pidPath)).toBe(false);
  });

  test("daemonInstanceLockPath is distinct from the CLI operation lock", () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    expect(daemonInstanceLockPath(pidPath)).toBe(`${pidPath}.instance.lock`);
    expect(daemonInstanceLockPath(pidPath)).not.toBe(`${pidPath}.lock`);
  });

  test("reads a runtime record without a pid and keeps an optional legacy pid", () => {
    const dir = tempDir();
    const recordPath = join(dir, "runtime.json");
    const withoutPid: DaemonRuntimeRecord = { ...runtimeRecord(dir) };
    delete withoutPid.pid;
    writeDaemonRuntimeRecord(recordPath, withoutPid);
    const parsed = readDaemonRuntimeRecord(recordPath);
    expect(parsed).toEqual(withoutPid);
    expect(parsed?.pid).toBeUndefined();

    // A legacy record that still carries a pid remains readable (pid is
    // optional metadata, never a required validation field).
    writeDaemonRuntimeRecord(recordPath, runtimeRecord(dir));
    expect(readDaemonRuntimeRecord(recordPath)?.pid).toBe(1234);
  });

  test("isFlockHeld correctly detects flock state and handles errors", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    // Non-existent path reports false
    expect(isFlockHeld(lockPath)).toBe(false);

    const release = acquireDaemonLock(lockPath);
    expect(isFlockHeld(lockPath)).toBe(true);

    release();
    expect(isFlockHeld(lockPath)).toBe(false);
  });

  test("daemon lock enforces mutual exclusion and owner tracking with persistent lock file", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    const release = acquireDaemonLock(lockPath);
    expect(existsSync(lockPath)).toBe(true);
    expect(existsSync(`${lockPath}.owner.json`)).toBe(true);

    expect(() => acquireDaemonLock(lockPath)).toThrow(
      /Herdsman daemon operation lock is held.*确认无 daemon 操作在跑后可删除/,
    );

    release();
    // Lock file is persistent and NEVER removed
    expect(existsSync(lockPath)).toBe(true);
    expect(existsSync(`${lockPath}.owner.json`)).toBe(false);

    acquireDaemonLock(lockPath);
    expect(existsSync(lockPath)).toBe(true);
    expect(existsSync(`${lockPath}.owner.json`)).toBe(true);
    releaseDaemonLock(lockPath);
    expect(existsSync(lockPath)).toBe(true);
    expect(existsSync(`${lockPath}.owner.json`)).toBe(false);
  });

  test("restart lock prevents concurrent stop with clear error", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    await withDaemonLock(lockPath, async () => {
      expect(() => acquireDaemonLock(lockPath)).toThrow(/Herdsman daemon operation lock is held/);
    });
    expect(existsSync(lockPath)).toBe(true);
  });

  test("releaseDaemonLock does not remove owner metadata if owner PID is another process", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    writeFileSync(lockPath, "");
    writeFileSync(
      `${lockPath}.owner.json`,
      JSON.stringify({ pid: process.pid + 1000, startedAt: new Date().toISOString() }),
    );

    // Calling releaseDaemonLock with our PID should NOT delete someone else's owner.json
    releaseDaemonLock(lockPath, process.pid);
    expect(existsSync(`${lockPath}.owner.json`)).toBe(true);

    // Calling releaseDaemonLock with matching PID should delete owner.json
    releaseDaemonLock(lockPath, process.pid + 1000);
    expect(existsSync(`${lockPath}.owner.json`)).toBe(false);
  });

  test("withDaemonLock releases lock even when action throws", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    await expect(
      withDaemonLock(lockPath, async () => {
        expect(existsSync(lockPath)).toBe(true);
        throw new Error("action error");
      }),
    ).rejects.toThrow("action error");

    expect(existsSync(lockPath)).toBe(true);
    expect(isFlockHeld(lockPath)).toBe(false);
  });

  test("two real concurrent processes competing for daemon lock: exactly one succeeds", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");
    const tsxCli = join(process.cwd(), "node_modules/tsx/dist/cli.mjs");

    const workerCode = `
      import { acquireDaemonLock } from "./src/daemon/process-manager.ts";
      try {
        const release = acquireDaemonLock(process.argv[1]);
        process.stdout.write("ACQUIRED\\n");
        process.stdin.resume();
        process.stdin.on("end", () => {
          release();
          process.exit(0);
        });
      } catch {
        process.stdout.write("HELD\\n");
        process.exit(1);
      }
    `;

    const p1 = spawn(process.execPath, [tsxCli, "-e", workerCode, lockPath], {
      detached: true,
      stdio: ["pipe", "pipe", "inherit"],
    });
    const p2 = spawn(process.execPath, [tsxCli, "-e", workerCode, lockPath], {
      detached: true,
      stdio: ["pipe", "pipe", "inherit"],
    });

    const readLine = (p: typeof p1) =>
      new Promise<string>((resolve) => {
        p.stdout.once("data", (d) => resolve(d.toString().trim()));
      });

    const results = await Promise.all([readLine(p1), readLine(p2)]);
    expect(results.filter((r) => r === "ACQUIRED")).toHaveLength(1);
    expect(results.filter((r) => r === "HELD")).toHaveLength(1);

    const killGroup = (p: typeof p1) => {
      try {
        if (p.pid) process.kill(-p.pid, "SIGKILL");
      } catch {}
    };

    killGroup(p1);
    killGroup(p2);
    await Promise.all([
      new Promise((res) => p1.on("exit", res)),
      new Promise((res) => p2.on("exit", res)),
    ]);
  });

  test("flock held by child process is released immediately upon SIGKILL", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");
    const tsxCli = join(process.cwd(), "node_modules/tsx/dist/cli.mjs");

    const workerCode = `
      import { acquireDaemonLock } from "./src/daemon/process-manager.ts";
      acquireDaemonLock(process.argv[1]);
      process.stdout.write("READY\\n");
      process.stdin.resume();
    `;

    const child = spawn(process.execPath, [tsxCli, "-e", workerCode, lockPath], {
      detached: true,
      stdio: ["pipe", "pipe", "inherit"],
    });

    await new Promise<void>((resolve) => {
      child.stdout.once("data", () => resolve());
    });

    // Parent cannot acquire while child holds flock
    expect(() => acquireDaemonLock(lockPath)).toThrow(/Herdsman daemon operation lock is held/);

    // SIGKILL entire child process group
    try {
      if (child.pid) process.kill(-child.pid, "SIGKILL");
    } catch {}
    await new Promise((resolve) => child.on("exit", resolve));

    // Brief settling delay for kernel cleanup
    await new Promise((resolve) => setTimeout(resolve, 50));

    // Lock is released in kernel
    expect(isFlockHeld(lockPath)).toBe(false);

    // Parent can immediately acquire flock without delay
    const release = acquireDaemonLock(lockPath);
    expect(existsSync(lockPath)).toBe(true);
    release();
    expect(existsSync(lockPath)).toBe(true);
  });

  test("stale owner.json pointing to unrelated live PID (e.g. init PID 1) does not block lock acquisition", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    writeFileSync(lockPath, "");
    writeFileSync(
      `${lockPath}.owner.json`,
      JSON.stringify({
        pid: 1, // Init/systemd process is definitely alive
        startedAt: new Date(Date.now() - 3600_000).toISOString(),
      }),
    );

    // Because PID 1 does not hold kernel flock on lockPath, acquisition succeeds
    const release = acquireDaemonLock(lockPath);
    expect(existsSync(lockPath)).toBe(true);

    const updated = JSON.parse(readFileSync(`${lockPath}.owner.json`, "utf8"));
    expect(updated.pid).toBe(process.pid);

    release();
    expect(existsSync(lockPath)).toBe(true);
  });

  test("flock handle release does not busy wait when another process immediately holds lock", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    const handle = acquireFlockHandle(lockPath);
    expect(handle).not.toBeNull();

    // Start a background process blocked on acquiring the flock on lockPath
    const childHoldingNext = spawn("flock", ["-x", lockPath, "sleep", "1"], {
      detached: true,
      stdio: "ignore",
    });

    // Wait a brief moment for child to block on flock
    await new Promise((res) => setTimeout(res, 20));

    const start = Date.now();
    handle?.release();
    const elapsed = Date.now() - start;

    // Release should return promptly (well below 100ms) without busy waiting for isFlockHeld
    expect(elapsed).toBeLessThan(70);

    // The lock is now immediately held by the next child process
    expect(isFlockHeld(lockPath)).toBe(true);

    try {
      if (childHoldingNext.pid) process.kill(-childHoldingNext.pid, "SIGKILL");
    } catch {}
    try {
      childHoldingNext.kill("SIGKILL");
    } catch {}
  });

  test("acquireFlockHandle returns null if child process exits immediately after writing READY", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");
    const binDir = join(dir, "bin");
    const fakeFlock = join(binDir, "flock");

    mkdirSync(binDir, { mode: 0o700, recursive: true });
    // Write fake flock that writes READY to the ack file and immediately exits
    const fakeFlockScript = `#!/bin/sh
ack=$(echo "$*" | grep -o '[^ "]*\\.ack\\.[^ "]*')
printf READY > "$ack"
exit 0
`;
    writeFileSync(fakeFlock, fakeFlockScript, { mode: 0o755 });

    const origPath = process.env.PATH;
    process.env.PATH = `${binDir}:${origPath}`;

    try {
      const handle = acquireFlockHandle(lockPath);
      expect(handle).toBeNull();
    } finally {
      process.env.PATH = origPath;
    }
  });

  test("stress test: 200 rounds of simultaneous sub-millisecond lock contention yields zero double-masters", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");
    const tsxCli = join(process.cwd(), "node_modules/tsx/dist/cli.mjs");

    const workerScript = `
      import { acquireDaemonLock } from "./src/daemon/process-manager.ts";
      import readline from "node:readline";

      let releaseFn = null;
      const rl = readline.createInterface({ input: process.stdin });
      rl.on("line", (line) => {
        const [cmd, round] = line.trim().split(" ");
        if (cmd === "RACE") {
          try {
            releaseFn = acquireDaemonLock(process.argv[1]);
            process.stdout.write("WON " + round + "\\n");
          } catch {
            process.stdout.write("LOST " + round + "\\n");
          }
        } else if (cmd === "RELEASE") {
          if (releaseFn) {
            releaseFn();
            releaseFn = null;
          }
          process.stdout.write("RELEASED " + round + "\\n");
        }
      });
      process.stdout.write("READY\\n");
    `;

    function startWorker() {
      const child = spawn(process.execPath, [tsxCli, "-e", workerScript, lockPath], {
        stdio: ["pipe", "pipe", "inherit"],
      });
      const rl = readline.createInterface({ input: child.stdout });
      const lines: string[] = [];
      const waiters: Array<(line: string) => void> = [];
      rl.on("line", (line) => {
        const resolve = waiters.shift();
        if (resolve) {
          resolve(line);
        } else {
          lines.push(line);
        }
      });
      const nextLine = () => {
        const line = lines.shift();
        if (line !== undefined) {
          return Promise.resolve(line);
        }
        return new Promise<string>((resolve) => waiters.push(resolve));
      };
      return { child, nextLine };
    }

    const w1 = startWorker();
    const w2 = startWorker();

    await Promise.all([w1.nextLine(), w2.nextLine()]);

    const totalRounds = 200;
    let singleMasterCount = 0;
    let doubleMasterCount = 0;

    for (let r = 0; r < totalRounds; r++) {
      w1.child.stdin.write(`RACE ${r}\n`);
      w2.child.stdin.write(`RACE ${r}\n`);

      const [res1, res2] = await Promise.all([w1.nextLine(), w2.nextLine()]);
      const outcomes = [res1.split(" ")[0], res2.split(" ")[0]];
      const wonCount = outcomes.filter((o) => o === "WON").length;
      const lostCount = outcomes.filter((o) => o === "LOST").length;

      if (wonCount === 1 && lostCount === 1) {
        singleMasterCount++;
        const winner = res1.startsWith("WON") ? w1 : w2;
        winner.child.stdin.write(`RELEASE ${r}\n`);
        const rel = await winner.nextLine();
        expect(rel).toBe(`RELEASED ${r}`);
      } else {
        doubleMasterCount++;
      }
    }

    w1.child.kill("SIGKILL");
    w2.child.kill("SIGKILL");

    expect(doubleMasterCount).toBe(0);
    expect(singleMasterCount).toBe(200);
  });
});

function runtimeRecord(dir: string): DaemonRuntimeRecord {
  return {
    dbPath: join(dir, "state.db"),
    homeDir: dir,
    logPath: join(dir, "herdsman.log"),
    pid: 1234,
    pidPath: join(dir, "herdsman.pid"),
    socketPath: "/tmp/herdsman.sock",
    startedAt: "2026-06-29T00:00:00.000Z",
    version: 1,
  };
}
