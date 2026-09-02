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
import { afterEach, describe, expect, test, vi } from "vitest";
import {
  acquireDaemonLock,
  type DaemonRuntimeRecord,
  getDaemonStatus,
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
    expect(readDaemonRuntimeRecord(runtimeRecordPath)).toMatchObject({
      dbPath: join(dir, "state.db"),
      homeDir: dir,
      logPath,
      pid: 5678,
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      version: 1,
    });
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

  test("daemon lock enforces mutual exclusion and owner tracking with manual disposal guidance", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    const release = acquireDaemonLock(lockPath, {
      identityProbe: () => true,
      isProcessRunning: () => true,
    });
    expect(existsSync(lockPath)).toBe(true);
    expect(existsSync(join(lockPath, "owner.json"))).toBe(true);

    expect(() =>
      acquireDaemonLock(lockPath, {
        identityProbe: () => true,
        isProcessRunning: () => true,
      }),
    ).toThrow(/Herdsman daemon operation lock is held.*确认无 daemon 操作在跑后可删除/);

    release();
    expect(existsSync(lockPath)).toBe(false);

    acquireDaemonLock(lockPath, {
      identityProbe: () => true,
      isProcessRunning: () => true,
    });
    expect(existsSync(lockPath)).toBe(true);
    releaseDaemonLock(lockPath);
    expect(existsSync(lockPath)).toBe(false);
  });

  test("daemon lock breaks stale locks when owner is dead or foreign PID", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    mkdirSync(lockPath, { recursive: true });
    writeFileSync(
      join(lockPath, "owner.json"),
      JSON.stringify({ pid: 9999, startedAt: new Date().toISOString() }),
    );

    const release = acquireDaemonLock(lockPath, {
      identityProbe: () => false,
      isProcessRunning: () => false,
    });
    expect(existsSync(lockPath)).toBe(true);
    release();
    expect(existsSync(lockPath)).toBe(false);
  });

  test("daemon lock does not break fresh lock directory when owner.json is missing (< 1s)", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    // Fresh lock dir with no owner.json yet (simulating window right after mkdir)
    mkdirSync(lockPath, { recursive: true });

    expect(() =>
      acquireDaemonLock(lockPath, {
        identityProbe: () => true,
        isProcessRunning: () => true,
      }),
    ).toThrow(/Herdsman daemon operation lock is held/);

    // Lock dir should still exist and not be removed
    expect(existsSync(lockPath)).toBe(true);
  });

  test("releaseDaemonLock does not remove lock if owner PID is another process", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    mkdirSync(lockPath, { recursive: true });
    writeFileSync(
      join(lockPath, "owner.json"),
      JSON.stringify({ pid: process.pid + 1000, startedAt: new Date().toISOString() }),
    );

    // Calling releaseDaemonLock with our PID should NOT delete someone else's lock
    releaseDaemonLock(lockPath, process.pid);
    expect(existsSync(lockPath)).toBe(true);

    // Calling releaseDaemonLock with matching PID should delete it
    releaseDaemonLock(lockPath, process.pid + 1000);
    expect(existsSync(lockPath)).toBe(false);
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

    expect(existsSync(lockPath)).toBe(false);
  });

  test("restart lock prevents concurrent stop with clear error", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    await withDaemonLock(
      lockPath,
      async () => {
        expect(() =>
          acquireDaemonLock(lockPath, {
            identityProbe: () => true,
            isProcessRunning: () => true,
          }),
        ).toThrow("Herdsman daemon operation lock is held by PID");
      },
      {
        identityProbe: () => true,
        isProcessRunning: () => true,
      },
    );
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
