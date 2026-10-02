import { spawn } from "node:child_process";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
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
  readDaemonProcessIdentity,
  readDaemonRuntimeRecord,
  releaseDaemonLock,
  removeDaemonPidFile,
  writeDaemonPidFile,
} from "@/daemon/process-manager.js";
import { managedByFromCgroup, readSystemdRestartCount } from "@/daemon/service-supervision.js";

/**
 * D1/D8 (`.agents/notes/20260930-terminal-event-delivery-open-items.md`): when
 * `readChildPids` cannot read `/proc/<pid>/task/<pid>/children`, the release
 * barrier degrades to watching the flock process alone, and it announces that
 * once per process. This host's procfs does expose the children file, so that
 * failure is unreachable without injection — hence the mock below.
 *
 * It is a *partial* mock: `importOriginal` keeps the real module and only
 * `readFileSync` is wrapped, and only for the children-file path *shape*
 * (`/proc/<digits>/task/<digits>/children`; production always passes the same pid
 * twice, the regex does not require the two to be equal). Nothing is replaced by a
 * fake module, and the wrapper is a pure pass-through unless a case raises
 * `failChildrenRead`, so every other case in this file (stat reads, ack/lock/owner
 * files, runtime records) keeps the real implementation and its real behaviour.
 */
const fsInjection = vi.hoisted(() => ({
  /** Content of the last successful children read: the fd-sharing helper pids. */
  childrenContent: [] as string[],
  /** Path of every children read, so a case can tell which pid it probed. */
  childrenReads: [] as string[],
  childrenPathPattern: /^\/proc\/(\d+)\/task\/(\d+)\/children$/,
  /** Raised only inside the cases that want the read to fail. */
  failChildrenRead: false,
}));

vi.mock("node:fs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("node:fs")>();
  const readFileSyncWithChildrenSeam = (path: unknown, options?: unknown) => {
    const target = String(path);
    if (fsInjection.childrenPathPattern.test(target)) {
      fsInjection.childrenReads.push(target);
      if (fsInjection.failChildrenRead) {
        // Shaped like the production trigger: `CONFIG_PROC_CHILDREN` off / a
        // stripped-down procfs means the file is simply not there to read.
        throw Object.assign(new Error(`ENOENT: no such file or directory, open '${target}'`), {
          code: "ENOENT",
        });
      }
      const content = actual.readFileSync(target, "utf8");
      fsInjection.childrenContent.push(content);
      return content;
    }
    return (actual.readFileSync as unknown as (p: unknown, o?: unknown) => string | Buffer)(
      path,
      options,
    );
  };
  return {
    ...actual,
    default: actual,
    readFileSync: readFileSyncWithChildrenSeam as unknown as typeof actual.readFileSync,
  };
});

/** The flock pid the most recent children read belongs to. */
function flockPidOfLastChildrenRead(): number {
  const path = fsInjection.childrenReads.at(-1);
  const pid = Number(fsInjection.childrenPathPattern.exec(path ?? "")?.[1]);
  expect(Number.isInteger(pid)).toBe(true);
  expect(pid).toBeGreaterThan(0);
  return pid;
}

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

/**
 * Polls `condition` until it holds, so a test never depends on a fixed sleep
 * being long enough when the machine is loaded. Failing the timeout throws, so
 * a real regression still fails instead of being masked by the wait.
 */
async function waitForCondition(
  condition: () => boolean,
  options: { description: string; intervalMs?: number; timeoutMs?: number },
): Promise<void> {
  const timeoutMs = options.timeoutMs ?? 5000;
  const intervalMs = options.intervalMs ?? 20;
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (condition()) return;
    const remaining = deadline - Date.now();
    if (remaining <= 0) {
      throw new Error(`Timed out after ${timeoutMs}ms waiting for ${options.description}`);
    }
    await new Promise((resolve) => setTimeout(resolve, Math.min(intervalMs, remaining)));
  }
}

// flock(2) syscall numbers are only a fallback signal for kernels that do not
// expose wchan symbols; `wchan` is the primary check below.
const FLOCK_SYSCALL_NUMBERS: Partial<Record<NodeJS.Architecture, string>> = {
  arm64: "32",
  x64: "73",
};

/**
 * Best-effort read of the pid the fake `flock` wrappers below publish into their
 * `holderPidFile`. Used on the failure path: when the handshake wait times out,
 * the cleanup must still be able to reach the holder, and an absent/garbled file
 * must leave the caller with `undefined` rather than a bogus pid.
 */
function readPublishedHolderPid(holderPidFile: string): number | undefined {
  try {
    const parsed = Number(readFileSync(holderPidFile, "utf8").trim());
    return Number.isInteger(parsed) && parsed > 0 ? parsed : undefined;
  } catch {
    return undefined;
  }
}

/**
 * True once `pid` is parked inside the kernel acquiring the flock. A process
 * that already owns a lock waits somewhere else (for `flock -x f <cmd>`: in
 * `do_wait` for its `<cmd>` child), so this identifies a contending process
 * instead of guessing how long a fixed delay should be.
 */
function isBlockedAcquiringFlock(pid: number): boolean {
  try {
    if (readFileSync(`/proc/${pid}/wchan`, "utf8").includes("locks_lock_inode_wait")) return true;
  } catch {}
  try {
    const [syscallNumber, , operation] = readFileSync(`/proc/${pid}/syscall`, "utf8")
      .trim()
      .split(/\s+/);
    const expected = FLOCK_SYSCALL_NUMBERS[process.arch];
    if (expected !== undefined && syscallNumber === expected && operation === "0x2") return true;
  } catch {}
  return false;
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
          readCgroup: () => undefined,
          readServiceRestartCount: () => undefined,
        },
        pidPath,
        socketPath: "/tmp/herdsman.sock",
      }),
    ).resolves.toEqual({
      managedBy: "unknown",
      pid: 1234,
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      socketReachable: true,
      state: "running",
    });
  });

  test("reports the supervising systemd unit and restart count for a known daemon pid", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");

    const status = await getDaemonStatus({
      deps: {
        connectSocket: async () => true,
        identityProbe: () => true,
        isProcessRunning: (pid) => pid === 1234,
        readCgroup: () => "0::/system.slice/herdsman.service\n",
        readServiceRestartCount: () => 7,
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
    });

    expect(status).toEqual({
      managedBy: "systemd:herdsman.service",
      pid: 1234,
      pidPath,
      restartCount: 7,
      socketPath: "/tmp/herdsman.sock",
      socketReachable: true,
      state: "running",
    });
  });

  test("omits the restart count when the daemon is not supervised by the herdsman unit", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");
    let restartProbes = 0;

    const status = await getDaemonStatus({
      deps: {
        connectSocket: async () => true,
        identityProbe: () => true,
        isProcessRunning: (pid) => pid === 1234,
        readCgroup: () => "0::/user.slice/user-0.slice/session-1.scope\n",
        readServiceRestartCount: () => {
          restartProbes += 1;
          return 0;
        },
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
    });

    // `NRestarts` belongs to `herdsman.service`, which does not own this pid:
    // printing it next to `managedBy: unmanaged` would read like "this stray
    // daemon restarted 0 times". The probe is not even run.
    expect(status).toMatchObject({ managedBy: "unmanaged", state: "running" });
    expect(status).not.toHaveProperty("restartCount");
    expect(restartProbes).toBe(0);
  });

  test("degrades supervision facts to unknown/omitted without changing the rest of the status", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");
    let restartProbes = 0;

    const status = await getDaemonStatus({
      deps: {
        connectSocket: async () => true,
        identityProbe: () => true,
        isProcessRunning: (pid) => pid === 1234,
        readCgroup: () => undefined,
        readServiceRestartCount: () => {
          restartProbes += 1;
          return undefined;
        },
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
    });

    expect(status).toMatchObject({
      managedBy: "unknown",
      pid: 1234,
      pidPath,
      socketPath: "/tmp/herdsman.sock",
      socketReachable: true,
      state: "running",
    });
    expect(status).not.toHaveProperty("restartCount");
    // `unknown` is not a systemd unit either, so no `systemctl` probe is spent.
    expect(restartProbes).toBe(0);
  });

  test("a throwing supervision probe never fails daemon status or changes its state", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");

    const status = await getDaemonStatus({
      deps: {
        connectSocket: async () => true,
        identityProbe: () => true,
        isProcessRunning: (pid) => pid === 1234,
        readCgroup: () => {
          throw new Error("cgroup read exploded");
        },
        readServiceRestartCount: () => {
          throw new Error("systemctl exploded");
        },
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
    });

    expect(status).toMatchObject({ managedBy: "unknown", pid: 1234, state: "running" });
    expect(status).not.toHaveProperty("restartCount");
  });

  test("managedByFromCgroup requires the system slice and rejects lookalike units", () => {
    // cgroup v2 and cgroup v1 system-manager layouts.
    expect(managedByFromCgroup("0::/system.slice/herdsman.service\n")).toBe(
      "systemd:herdsman.service",
    );
    expect(managedByFromCgroup("1:name=systemd:/system.slice/herdsman.service\n0::/\n")).toBe(
      "systemd:herdsman.service",
    );
    // A nested cgroup inside the unit still belongs to that unit.
    expect(managedByFromCgroup("0::/system.slice/herdsman.service/child.scope\n")).toBe(
      "systemd:herdsman.service",
    );
    // Same unit name, but a *user* session unit: a different object that must
    // not be reported as the system service (matching on the basename alone did).
    expect(
      managedByFromCgroup(
        "0::/user.slice/user-1000.slice/user@1000.service/app.slice/herdsman.service\n",
      ),
    ).toBe("unmanaged");
    // A unit that merely starts with the same prefix must not be mistaken for it
    expect(managedByFromCgroup("0::/system.slice/herdsman.service.dev\n")).toBe("unmanaged");
    // Readable content that names no herdsman unit: genuinely unmanaged.
    expect(managedByFromCgroup("0::/user.slice/user-0.slice/session-1.scope\n")).toBe("unmanaged");
    expect(managedByFromCgroup("0::/\n")).toBe("unmanaged");
    // Nothing was read: no fact available, which is `unknown`, not `unmanaged`.
    expect(managedByFromCgroup("")).toBe("unknown");
    expect(managedByFromCgroup("\n")).toBe("unknown");
    expect(managedByFromCgroup("   \n\t\n")).toBe("unknown");
  });

  test("readSystemdRestartCount parses NRestarts and degrades on unusable output", () => {
    const calls: Array<{ args: string[]; command: string; timeoutMs: number }> = [];
    expect(
      readSystemdRestartCount({
        runner: (command, args, timeoutMs) => {
          calls.push({ args, command, timeoutMs });
          return "7\n";
        },
      }),
    ).toBe(7);
    expect(calls).toEqual([
      {
        args: ["show", "herdsman.service", "-p", "NRestarts", "--value"],
        command: "systemctl",
        timeoutMs: 1500,
      },
    ]);

    expect(readSystemdRestartCount({ runner: () => "not-a-number" })).toBeUndefined();
    expect(readSystemdRestartCount({ runner: () => "-1" })).toBeUndefined();
    expect(readSystemdRestartCount({ runner: () => undefined })).toBeUndefined();
    expect(
      readSystemdRestartCount({
        runner: () => {
          throw new Error("systemctl exploded");
        },
      }),
    ).toBeUndefined();
  });

  test("readSystemdRestartCount returns undefined when systemctl is missing", () => {
    const dir = tempDir();
    expect(
      readSystemdRestartCount({ command: join(dir, "missing-systemctl"), timeoutMs: 500 }),
    ).toBeUndefined();
  });

  test("readSystemdRestartCount returns within the timeout when the probe ignores SIGTERM", () => {
    const dir = tempDir();
    const stubborn = join(dir, "stubborn-systemctl");
    // `trap '' TERM` + `exec`: the child ignores SIGTERM (SIG_IGN survives exec)
    // and the sleeping image *is* the direct child, so only a hard kill can end
    // it. A plain `sleep 5` would die on SIGTERM and give false confidence.
    writeFileSync(stubborn, "#!/bin/sh\ntrap '' TERM\nexec sleep 8\n", { mode: 0o755 });

    const start = Date.now();
    expect(readSystemdRestartCount({ command: stubborn, timeoutMs: 300 })).toBeUndefined();
    const elapsed = Date.now() - start;
    // Without `killSignal: "SIGKILL"` this waited for the shim's full lifetime
    // (8003ms measured on this machine), i.e. `daemon status` hung for 8s.
    expect(elapsed).toBeLessThan(1000);
    expect(elapsed).toBeGreaterThanOrEqual(250);
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

  test("returns undefined for missing or invalid runtime records", () => {
    const dir = tempDir();
    expect(readDaemonRuntimeRecord(join(dir, "missing.json"))).toBeUndefined();

    const invalidPath = join(dir, "runtime.json");
    writeFileSync(invalidPath, "not-json");
    expect(readDaemonRuntimeRecord(invalidPath)).toBeUndefined();
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

  test("reports running with stalePid when the PID is reused by another process but the socket is reachable", async () => {
    const dir = tempDir();
    const pidPath = join(dir, "herdsman.pid");
    writeFileSync(pidPath, "1234\n");

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
        readCgroup: () => undefined,
        readServiceRestartCount: () => undefined,
      },
      pidPath,
      socketPath: "/tmp/herdsman.sock",
    });

    expect(status1).toEqual({
      managedBy: "unknown",
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
        readCgroup: () => undefined,
        readServiceRestartCount: () => undefined,
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
  });

  test("daemonInstanceLockPath is distinct from the plain lock path", () => {
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
    writeFileSync(recordPath, `${JSON.stringify(withoutPid, null, 2)}\n`);
    const parsed = readDaemonRuntimeRecord(recordPath);
    expect(parsed).toEqual(withoutPid);
    expect(parsed?.pid).toBeUndefined();

    // A legacy record that still carries a pid remains readable (pid is
    // optional metadata, never a required validation field).
    writeFileSync(recordPath, `${JSON.stringify(runtimeRecord(dir), null, 2)}\n`);
    expect(readDaemonRuntimeRecord(recordPath)?.pid).toBe(1234);
  });

  test("isFlockHeld correctly detects flock state and handles errors", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    // Non-existent path reports false
    expect(isFlockHeld(lockPath)).toBe(false);

    const release = acquireDaemonLock(lockPath);
    expect(isFlockHeld(lockPath)).toBe(true);

    release();
    // release() signals the lock-holding child; the kernel only drops the flock
    // once that process is gone, which can lag behind the signal under load.
    await waitForCondition(() => !isFlockHeld(lockPath), {
      description: "the released flock to be observable as free",
    });
    expect(isFlockHeld(lockPath)).toBe(false);
  });

  test("daemon lock enforces mutual exclusion and owner tracking with persistent lock file", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    const release = acquireDaemonLock(lockPath);
    expect(existsSync(lockPath)).toBe(true);
    expect(existsSync(`${lockPath}.owner.json`)).toBe(true);

    expect(() => acquireDaemonLock(lockPath)).toThrow(
      /Herdsman daemon operation lock is held.*systemctl status herdsman\.service.*仅当确认无 daemon 与 CLI 操作在跑时才可删除锁文件/s,
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

    const killGroup = (p: typeof p1) => {
      try {
        if (p.pid) process.kill(-p.pid, "SIGKILL");
      } catch {}
    };

    try {
      const results = await Promise.all([readLine(p1), readLine(p2)]);
      expect(results.filter((r) => r === "ACQUIRED")).toHaveLength(1);
      expect(results.filter((r) => r === "HELD")).toHaveLength(1);

      killGroup(p1);
      killGroup(p2);
      // Reap both workers under an explicit bound: they are detached and keep the
      // lock until their stdin ends, so an unbounded `on("exit")` wait would hang
      // until the test-level budget if the group SIGKILL never landed.
      await waitForCondition(() => p1.exitCode !== null || p1.signalCode !== null, {
        description: "the first lock contender to be reaped after SIGKILL",
      });
      await waitForCondition(() => p2.exitCode !== null || p2.signalCode !== null, {
        description: "the second lock contender to be reaped after SIGKILL",
      });
    } finally {
      // A failing assertion above must not leave two lock contenders behind: they
      // keep their stdin (and the lock) until they are signalled. `killGroup`
      // tolerates an already-dead pid, so the happy path's kills stay where they
      // are and this is a no-op when the test already reaped both workers.
      killGroup(p1);
      killGroup(p2);
    }
    // The two `waitForCondition` reaps above own a 5s budget; the test-level
    // deadline is kept longer so a stalled reap reports its own timeout instead of
    // being masked by `Test timed out in 5000ms`.
  }, 10_000);

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
    // Wait for the kill to land under an explicit bound instead of awaiting the
    // `exit` event with no upper limit: a SIGKILL that reaches nothing (wrong pid,
    // a process the caller may not signal) would otherwise hang here until the
    // test-level budget reports a bare `Test timed out`. The poll fails with its
    // own named error well inside that budget.
    await waitForCondition(() => child.exitCode !== null || child.signalCode !== null, {
      description: "the SIGKILLed flock holder to exit",
    });

    // Kernel cleanup after the SIGKILL is not observable at a fixed offset, so
    // poll until the flock is really released instead of assuming 50ms is enough
    // on a loaded machine. The release is bounded by the poll's own timeout.
    await waitForCondition(() => !isFlockHeld(lockPath), {
      description: "the killed holder's flock to be observable as free",
    });

    // Lock is released in kernel
    expect(isFlockHeld(lockPath)).toBe(false);

    // Parent can immediately acquire flock without delay
    const release = acquireDaemonLock(lockPath);
    expect(existsSync(lockPath)).toBe(true);
    release();
    expect(existsSync(lockPath)).toBe(true);
    // `waitForCondition` owns a 5s budget; the test-level deadline is kept longer
    // so a stalled barrier reports its own timeout instead of being masked by
    // `Test timed out in 5000ms`.
  }, 10_000);

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
    // Written by the contender only after it owns the lock, and held far longer
    // than the assertions below need, so nothing here races a `sleep` expiry.
    const markerPath = join(dir, "next-holder-acquired");

    const handle = acquireFlockHandle(lockPath);
    expect(handle).not.toBeNull();

    // Start a background process blocked on acquiring the flock on lockPath
    const childHoldingNext = spawn(
      "flock",
      ["-x", lockPath, "sh", "-c", `printf TAKEN > "${markerPath}"; sleep 30`],
      {
        detached: true,
        stdio: "ignore",
      },
    );

    // Wait until the contender is really parked inside flock(2) instead of
    // assuming a fixed delay was long enough under load.
    try {
      await waitForCondition(
        () => childHoldingNext.pid !== undefined && isBlockedAcquiringFlock(childHoldingNext.pid),
        {
          description: "the contending flock process to block on the lock",
          intervalMs: 10,
        },
      );

      const start = Date.now();
      handle?.release();
      const elapsed = Date.now() - start;

      // Release should return promptly (well below 100ms) without busy waiting for isFlockHeld
      expect(elapsed).toBeLessThan(70);

      // The lock is now immediately held by the next child process
      await waitForCondition(() => existsSync(markerPath), {
        description: "the contender to take the lock over after release",
      });
      expect(isFlockHeld(lockPath)).toBe(true);
    } finally {
      // The contender holds the lock for 30s, so it must never outlive this test,
      // not even when an assertion above fails.
      try {
        if (childHoldingNext.pid) process.kill(-childHoldingNext.pid, "SIGKILL");
      } catch {}
      try {
        childHoldingNext.kill("SIGKILL");
      } catch {}
    }
  });

  /** Resolves a binary on PATH, to be called before a test overrides PATH. */
  function resolveOnPath(name: string): string {
    for (const dir of (process.env.PATH ?? "").split(":")) {
      const candidate = join(dir, name);
      if (existsSync(candidate)) return candidate;
    }
    throw new Error(`${name} not found on PATH`);
  }

  // The flock handle learns about the command process sharing its fd from
  // `/proc/<pid>/task/<pid>/children`; when procfs does not expose that file the
  // release barrier degrades to watching the flock process alone (the documented
  // fallback), so there is nothing to assert on such a kernel.
  const hasProcChildren = existsSync(`/proc/${process.pid}/task/${process.pid}/children`);

  test.skipIf(!hasProcChildren)(
    "flock handle release waits for every helper process that shares the lock fd",
    async () => {
      const dir = tempDir();
      const lockPath = join(dir, "herdsman.pid.lock");
      const binDir = join(dir, "bin");
      const holderPidFile = join(dir, "holder.pid");
      const fakeFlock = join(binDir, "flock");
      const realFlock = resolveOnPath("flock");
      const setsid = resolveOnPath("setsid");
      mkdirSync(binDir, { mode: 0o700, recursive: true });
      // `flock` forks and the command it runs inherits the flock fd, so the real
      // helper has two fd holders and the kernel only drops the flock once the
      // last of them is gone. This fake keeps that topology but moves the real
      // holder into its own session, where the release-time `kill(-pgid, SIGKILL)`
      // cannot reach it: a deterministic stand-in for the kernel-side teardown
      // lag, in which the process acquireFlockHandle watches is gone while the
      // flock itself is still held.
      writeFileSync(
        fakeFlock,
        `#!/bin/sh
ack=$(echo "$*" | grep -o '[^ "]*\\.ack\\.[^ "]*')
if [ -z "$ack" ]; then
  # Not a handshake invocation (e.g. the isFlockHeld probe): behave normally.
  exec ${realFlock} "$@"
fi
${setsid} ${realFlock} -x -n ${lockPath} sh -c "printf READY > \\"$ack\\"; exec sleep 30" >/dev/null 2>&1 &
printf %s "$!" > ${holderPidFile}
n=0
while [ ! -f "$ack" ] && [ $n -lt 200 ]; do
  n=$((n + 1))
  sleep 0.005
done
exec cat
`,
        { mode: 0o755 },
      );

      const origPath = process.env.PATH;
      process.env.PATH = `${binDir}:${origPath}`;
      let holderPid: number | undefined;
      try {
        const handle = acquireFlockHandle(lockPath);
        expect(handle).not.toBeNull();
        // READY proves the *lock holder* is up, not that this wrapper published its
        // pid: the fake flock backgrounds the holder first and writes
        // `holderPidFile` afterwards, so on a loaded full-suite run that write can
        // land after the handshake the acquisition above returned on. Wait for the
        // pid file to be observable instead of blind-reading it — the blind read
        // raised ENOENT roughly once per full parallel run (the single-file run
        // never lost the race, which is why it read as flaky).
        await waitForCondition(
          () => existsSync(holderPidFile) && readFileSync(holderPidFile, "utf8").trim() !== "",
          {
            description: `the fake flock wrapper to publish the helper pid into ${holderPidFile}`,
            intervalMs: 5,
            timeoutMs: 2000,
          },
        );
        holderPid = Number(readFileSync(holderPidFile, "utf8"));

        const start = Date.now();
        handle?.release();
        const elapsed = Date.now() - start;

        // The holder is still alive, so release() must spend its whole
        // best-effort budget waiting for it...
        expect(elapsed).toBeGreaterThanOrEqual(90);
        // ...and it must not report the lock as free while the holder owns it.
        expect(isFlockHeld(lockPath)).toBe(true);
      } finally {
        process.env.PATH = origPath;
        // A wait that timed out leaves `holderPid` unassigned even though the
        // wrapper may already have published it by then — re-probe once, so a
        // failed wait cannot leak a lock-holding holder (it lives in its own
        // session, so only its pid lets us reach it).
        holderPid ??= readPublishedHolderPid(holderPidFile);
        if (holderPid !== undefined) {
          try {
            process.kill(-holderPid, "SIGKILL");
          } catch {}
        }
      }
    },
  );

  test.skipIf(!hasProcChildren)(
    "flock handle release() returns only once the lock is observably free",
    async () => {
      const dir = tempDir();
      const lockPath = join(dir, "herdsman.pid.lock");
      const binDir = join(dir, "bin");
      const holderScript = join(binDir, "holder.sh");
      const holderPidFile = join(dir, "holder.pid");
      const releaseTrigger = join(dir, "holder.release");
      const fakeFlock = join(binDir, "flock");
      const realFlock = resolveOnPath("flock");
      const setsid = resolveOnPath("setsid");
      mkdirSync(binDir, { mode: 0o700, recursive: true });
      // The real holder runs in its own session, so the release-time
      // `kill(-pgid, SIGKILL)` cannot reach it, and — unlike the sibling test
      // above — it *exits by itself*: it publishes READY, waits for the trigger
      // file this test writes immediately before calling release() (so it is
      // provably still holding then), and then holds for 40ms more. That makes
      // the core post-condition directly assertable: release() has to wait for
      // the surviving fd holder before it returns, so the lock must already be
      // free when it does. Bounded trigger wait (2s) keeps a failing run from
      // leaving the lock held in the temp directory.
      writeFileSync(
        holderScript,
        `#!/bin/sh
printf READY > "$1"
n=0
while [ ! -f "$2" ] && [ $n -lt 400 ]; do
  n=$((n + 1))
  sleep 0.005
done
sleep 0.04
`,
        { mode: 0o755 },
      );
      writeFileSync(
        fakeFlock,
        `#!/bin/sh
ack=$(echo "$*" | grep -o '[^ "]*\\.ack\\.[^ "]*')
if [ -z "$ack" ]; then
  # Not a handshake invocation (e.g. the isFlockHeld probe): behave normally.
  exec ${realFlock} "$@"
fi
${setsid} ${realFlock} -x -n ${lockPath} ${holderScript} "$ack" ${releaseTrigger} >/dev/null 2>&1 &
printf %s "$!" > ${holderPidFile}
n=0
while [ ! -f "$ack" ] && [ $n -lt 200 ]; do
  n=$((n + 1))
  sleep 0.005
done
exec cat
`,
        { mode: 0o755 },
      );

      const origPath = process.env.PATH;
      process.env.PATH = `${binDir}:${origPath}`;
      let holderPid: number | undefined;
      try {
        const handle = acquireFlockHandle(lockPath);
        expect(handle).not.toBeNull();
        // Same handshake-order race as the sibling test above: the fake flock
        // backgrounds the holder and publishes `holderPidFile` only afterwards, so
        // READY (which the acquisition above already waited for) can be observable
        // before the pid file is. Wait for the pid file rather than blind-reading it.
        await waitForCondition(
          () => existsSync(holderPidFile) && readFileSync(holderPidFile, "utf8").trim() !== "",
          {
            description: `the fake flock wrapper to publish the holder pid into ${holderPidFile}`,
            intervalMs: 5,
            timeoutMs: 2000,
          },
        );
        holderPid = Number(readFileSync(holderPidFile, "utf8"));
        expect(Number.isInteger(holderPid) && holderPid > 0).toBe(true);
        // The holder is parked before its short hold, so the lock really is held
        // right now — the barrier below is what makes it free again in time.
        expect(isFlockHeld(lockPath)).toBe(true);

        // Release the holder from its pre-hold wait, then tear our helper down.
        writeFileSync(releaseTrigger, "");
        const start = Date.now();
        handle?.release();
        const elapsed = Date.now() - start;

        // The post-condition this whole barrier exists for: once release() has
        // returned, the lock is free for the next acquirer.
        expect(isFlockHeld(lockPath)).toBe(false);
        // Loose upper bound only: release() returns as soon as the holder is
        // gone instead of serving the whole 100ms budget. The lower bound is
        // deliberately not asserted here (that is the sibling test's job) to
        // keep this one free of scheduling flake.
        expect(elapsed).toBeLessThan(90);
      } finally {
        process.env.PATH = origPath;
        // Same failure-path cleanup as the sibling test above: a timed-out wait
        // must not leak the holder just because `holderPid` was never assigned.
        holderPid ??= readPublishedHolderPid(holderPidFile);
        if (holderPid !== undefined) {
          try {
            process.kill(-holderPid, "SIGKILL");
          } catch {}
        }
      }
    },
  );

  test("acquireFlockHandle retries a bounded number of lock-helper exits, but not while a live owner is registered", () => {
    const dir = tempDir();
    const binDir = join(dir, "bin");
    const counterFile = join(dir, "attempts");
    const succeedFromFile = join(dir, "succeed-from-attempt");
    const fakeFlock = join(binDir, "flock");
    const realFlock = resolveOnPath("flock");
    mkdirSync(binDir, { mode: 0o700, recursive: true });
    // Stands in for `flock -n` failing because a *previous* holder is still
    // finishing its kernel-side teardown: exit 1 without publishing READY until
    // the attempt number in succeedFromFile, then behave like the real flock.
    writeFileSync(
      fakeFlock,
      `#!/bin/sh
attempts=$(cat ${counterFile} 2>/dev/null || echo 0)
attempts=$((attempts + 1))
printf %s "$attempts" > ${counterFile}
if [ "$attempts" -lt "$(cat ${succeedFromFile})" ]; then
  exit 1
fi
exec ${realFlock} "$@"
`,
      { mode: 0o755 },
    );

    const origPath = process.env.PATH;
    process.env.PATH = `${binDir}:${origPath}`;
    try {
      // The helper loses a race against a holder that is already gone: the next
      // attempt must take the lock instead of reporting it as held.
      writeFileSync(succeedFromFile, "2");
      const recovered = acquireFlockHandle(join(dir, "recovered.lock"));
      expect(recovered).not.toBeNull();
      expect(readFileSync(counterFile, "utf8")).toBe("2");
      recovered?.release();

      // A lock that really stays taken is still reported as held, after a bounded
      // number of attempts instead of an unbounded wait inside the same window.
      writeFileSync(succeedFromFile, "999");
      writeFileSync(counterFile, "0");
      expect(acquireFlockHandle(join(dir, "held.lock"))).toBeNull();
      const attempts = Number(readFileSync(counterFile, "utf8"));
      expect(attempts).toBeGreaterThan(1);
      expect(attempts).toBeLessThanOrEqual(5);

      // A live registered owner means the lock is held on purpose: retrying
      // cannot help, so the diagnostic "lock is held" path stays immediate.
      writeFileSync(succeedFromFile, "1");
      const ownedLock = join(dir, "owned.lock");
      const releaseOwner = acquireDaemonLock(ownedLock);
      writeFileSync(counterFile, "0");
      expect(acquireFlockHandle(ownedLock)).toBeNull();
      expect(readFileSync(counterFile, "utf8")).toBe("1");
      releaseOwner();
    } finally {
      process.env.PATH = origPath;
    }
  });

  test("acquireFlockHandle returns null if child process exits immediately after writing READY", () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");
    const binDir = join(dir, "bin");
    const fakeFlock = join(binDir, "flock");

    mkdirSync(binDir, { mode: 0o700, recursive: true });
    // Publish READY only after this pid is gone so acquireFlockHandle waits on a
    // definite child-exit event instead of racing READY-visible vs still-active.
    const fakeFlockScript = `#!/bin/sh
ack=$(echo "$*" | grep -o '[^ "]*\\.ack\\.[^ "]*')
pid=$$
(
  trap '' HUP
  n=0
  while [ $n -lt 100 ] && [ -d "/proc/$pid" ]; do
    n=$((n + 1))
    sleep 0.01
  done
  printf READY > "$ack"
) &
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

  test("a failed or truncated /proc/<pid>/stat read never counts as a dead lock-holding child", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    // Models procfs breaking while the child is provably alive: READY is only
    // published after `flock -n` succeeded and the child is parked in `cat`
    // holding the flock. Every case below must still hand back a handle, and that
    // handle must really own the flock — so a second acquisition without the seam
    // has to be refused (double-master regression surface) and the lock must be
    // observably free again once the handle is released.
    const expectSurvivingHolder = async (readProcessStat: (pid: number) => string) => {
      const handle = acquireFlockHandle(lockPath, { readProcessStat });
      expect(handle).not.toBeNull();
      expect(acquireFlockHandle(lockPath)).toBeNull();
      handle?.release();
      await waitForCondition(() => !isFlockHeld(lockPath), {
        description: "the released flock to be observable as free",
      });
      expect(isFlockHeld(lockPath)).toBe(false);
    };

    // A read error carrying an errno (EIO) proves nothing about liveness.
    await expectSurvivingHolder(() => {
      throw Object.assign(new Error("input/output error"), { code: "EIO" });
    });

    // A read error without any `code` is just as inconclusive.
    await expectSurvivingHolder(() => {
      throw new Error("procfs read failed");
    });

    // Empty/truncated stat content (procfs reports `st_size` 0) parses to nothing.
    await expectSurvivingHolder(() => "");
  });

  test("a simulated T-state (SIGSTOP-paused seam) lock-holding child counts as alive and keeps its handle", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");

    // Simulated here through the seam (no real signal is sent): `T` (SIGSTOP/SIGTSTP)
    // only pauses the child, but the kernel still holds its flock,
    // so the acquisition must survive it. Before the D6 fix this read as dead, so
    // the just-acquired lock-holding helper was SIGKILLed and acquireFlockHandle
    // returned null. The seam rewrites only the state field of the real stat line,
    // so nothing else about the read differs from a healthy procfs.
    //
    // Only the post-READY phase is rewritten: once READY is out, the helper is the
    // paused *holder* this case is about, whereas a T read before the handshake is
    // (correctly) abandonable — rewriting that one too would make this case exercise
    // the pre-READY path below instead of the holder-survives-T path.
    const readyPublished = () => {
      try {
        return readdirSync(dir).some(
          (name) =>
            name.includes(".ack.") && readFileSync(join(dir, name), "utf8").startsWith("READY"),
        );
      } catch {
        return false;
      }
    };

    const pausedStat = (pid: number) => {
      const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
      const lastParen = stat.lastIndexOf(")");
      const paused = `${stat.slice(0, lastParen + 2)}T${stat.slice(lastParen + 3)}`;
      return readyPublished() ? paused : stat;
    };

    const handle = acquireFlockHandle(lockPath, { readProcessStat: pausedStat });
    expect(handle).not.toBeNull();
    // The handle must really own the flock: a second acquisition without the seam
    // has to be refused, otherwise this case would not pin the double-master surface.
    expect(acquireFlockHandle(lockPath)).toBeNull();
    handle?.release();
    await waitForCondition(() => !isFlockHeld(lockPath), {
      description: "the released flock to be observable as free",
    });
    expect(isFlockHeld(lockPath)).toBe(false);
  });

  test("a pre-READY paused (T) helper is abandoned: the acquisition kills it and retries", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");
    const binDir = join(dir, "bin");
    const fakeSh = join(binDir, "sh");

    // The helper's READY handshake runs through the `sh` it finds on PATH, so a slow
    // `sh` parks the first helper in the pre-READY phase for 50ms — orders of
    // magnitude longer than the parent's first liveness read (microseconds after
    // spawn). "The first helper never published READY" is therefore a fact of this
    // case rather than a race: the state read really does land in the pre-READY
    // phase, before the ack file can exist.
    mkdirSync(binDir, { mode: 0o700, recursive: true });
    writeFileSync(
      fakeSh,
      `#!/bin/sh
sleep 0.05
exec /bin/sh "$@"
`,
      { mode: 0o755 },
    );

    // Semantics under test (D6 refined): before READY the helper is not a legal
    // holder, so a paused (T) helper is abandonable — the acquisition must SIGKILL
    // it and retry on the fast path instead of spending its whole window on it.
    // Only the first helper is reported as T: it is the attempt whose READY never
    // lands, and T is exactly the state a SIGSTOPped pre-READY helper has in
    // production.
    const helperPids: number[] = [];
    const preReadyPausedStat = (pid: number) => {
      const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
      if (!helperPids.includes(pid)) {
        helperPids.push(pid);
      }
      if (helperPids[0] !== pid) {
        return stat;
      }
      // Rewrite only the state field of the real stat line, like the post-READY T
      // case above, so nothing else about the read differs from a healthy procfs.
      const lastParen = stat.lastIndexOf(")");
      return `${stat.slice(0, lastParen + 2)}T${stat.slice(lastParen + 3)}`;
    };

    const origPath = process.env.PATH;
    process.env.PATH = `${binDir}:${origPath}`;
    let handle: ReturnType<typeof acquireFlockHandle> = null;
    let elapsed = 0;
    try {
      const start = Date.now();
      handle = acquireFlockHandle(lockPath, { readProcessStat: preReadyPausedStat });
      elapsed = Date.now() - start;
    } finally {
      process.env.PATH = origPath;
    }

    expect(handle).not.toBeNull();
    // Fast path: the paused helper was killed and a retry took the lock. The bound
    // is loose on purpose (the fake `sh` alone holds READY back ~50ms); what it pins
    // is that this case stays far below the 1000ms acquisition window it spends when
    // a pre-READY T is read as alive, as the version it replaces did.
    expect(elapsed).toBeLessThan(500);
    // The kill-and-retry really happened: a second helper was spawned.
    expect(helperPids.length).toBeGreaterThanOrEqual(2);
    // The surviving handle must really own the flock: a second acquisition without
    // the seam is refused, so this case pins the double-master surface too.
    expect(isFlockHeld(lockPath)).toBe(true);
    expect(acquireFlockHandle(lockPath)).toBeNull();
    handle?.release();
    await waitForCondition(() => !isFlockHeld(lockPath), {
      description: "the released flock to be observable as free",
    });
    expect(isFlockHeld(lockPath)).toBe(false);
  });

  // Deliberately unguarded, unlike the two neighbouring cases that `skipIf` on
  // `hasProcChildren`: this one injects the read failure, so it holds on any Linux
  // host — including a kernel whose procfs does not expose the children file, where
  // that same code path is simply real.
  test("an unreadable children file warns exactly once per process (D1 degraded barrier)", async () => {
    const dir = tempDir();
    const lockPath = join(dir, "herdsman.pid.lock");
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
    fsInjection.failChildrenRead = true;
    try {
      // The one-shot flag is module state nothing outside the module can reset, so
      // this case works on a fresh copy of the module: "exactly once" then holds no
      // matter what the cases before it did, instead of depending on this case
      // running first.
      vi.resetModules();
      const { acquireFlockHandle: acquireWithFreshWarningState } = await import(
        "@/daemon/process-manager.js"
      );

      // Two successful acquisitions with the children file unreadable: the second
      // must stay silent. That is the point of the module-level flag — the
      // degradation is a property of the kernel/procfs, not of one lock, so it must
      // not warn on every acquisition.
      const first = acquireWithFreshWarningState(lockPath);
      expect(first).not.toBeNull();
      first?.release();
      const second = acquireWithFreshWarningState(lockPath);
      expect(second).not.toBeNull();
      second?.release();

      expect(warnSpy).toHaveBeenCalledTimes(1);
      // Pins the D1 warning itself rather than any unrelated `console.warn`: it
      // names the children file that could not be read and the degraded barrier.
      const message = String(warnSpy.mock.calls[0]?.[0]);
      expect(message).toContain("/children is unavailable");
      expect(message).toContain("release barrier degraded");
      expect(message).toMatch(/^\[herdsman\] \/proc\/\d+\/task\/\d+\/children/);
    } finally {
      fsInjection.failChildrenRead = false;
      warnSpy.mockRestore();
    }

    await waitForCondition(() => !isFlockHeld(lockPath), {
      description: "the flock to be observable as free after the degraded releases",
    });
  });

  test.skipIf(!hasProcChildren)(
    "an unreadable children file leaves release watching the flock process alone (D1 degradation)",
    async () => {
      const dir = tempDir();
      const lockPath = join(dir, "herdsman.pid.lock");
      // This case triggers the D1 fallback on purpose; the warning itself is pinned by
      // the case above, so keep it out of the test output here.
      const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});

      // The release barrier's observation surface is the seam below: it is called once
      // per helper pid the barrier waits for (`helperPids.some(...)`, which stops at the
      // first pid that is still alive), so recording its arguments shows exactly which
      // pids a release watched. The flock pid is reported as already gone — the state a
      // freshly SIGKILLed helper is in — so the wait loop must look at the next entry
      // if the list has one.
      const pidsWatchedByRelease: number[] = [];
      let flockPid = 0;
      const releaseWatchSeam = (pid: number) => {
        pidsWatchedByRelease.push(pid);
        if (pid === flockPid) return `${pid} (flock) Z 0 0 0`;
        return readFileSync(`/proc/${pid}/stat`, "utf8");
      };

      // Both handles are held outside `try` so the `finally` below can release
      // them: an assertion that fails before its own `release()` must not leave the
      // helpers holding this temporary lock until the process exits. (Some sibling
      // spawn cases clean up in `finally` the same way — the measured inventory is
      // in the ledger's D17 (n); the two lock-contention cases kill inline.)
      // `release()` is idempotent, so releasing twice is a no-op.
      let degraded: ReturnType<typeof acquireFlockHandle> = null;
      let healthy: ReturnType<typeof acquireFlockHandle> = null;

      try {
        // Degraded: the children file cannot be read, so `helperPids` holds the flock
        // process alone and release must not wait on anything else.
        fsInjection.failChildrenRead = true;
        degraded = acquireFlockHandle(lockPath, { readProcessStat: releaseWatchSeam });
        expect(degraded).not.toBeNull();
        flockPid = flockPidOfLastChildrenRead();
        // Load-bearing self-check: every assertion in this case rests on the seam
        // reporting the flock pid as *gone*. Relax that state to a live one and the wait
        // loop short-circuits on the flock pid, so the watch set can never grow and the
        // comparison below would pass for the wrong reason. `isChildProcessActive` is not
        // exported, so this pins the same field it reads: the character after `") "` of
        // the stat line, where `Z` is the kernel saying "this pid has exited". (This check
        // registers one probe on the seam too; the reset just below discards it together
        // with the acquisition-phase probes.)
        const flockStatFromSeam = releaseWatchSeam(flockPid);
        expect(
          flockStatFromSeam
            .slice(flockStatFromSeam.lastIndexOf(")") + 2)
            .trim()
            .charAt(0),
        ).toBe("Z");
        pidsWatchedByRelease.length = 0;
        degraded?.release();
        expect(new Set(pidsWatchedByRelease)).toEqual(new Set([flockPid]));

        // Control: with a readable children file the very same call watches the
        // fd-sharing child as well, so the single-entry watch set above is a real
        // consequence of the degradation and not an artefact of the instrumentation.
        fsInjection.failChildrenRead = false;
        healthy = acquireFlockHandle(lockPath, { readProcessStat: releaseWatchSeam });
        expect(healthy).not.toBeNull();
        const healthyFlockPid = flockPidOfLastChildrenRead();
        const fdSharingPid = Number(fsInjection.childrenContent.at(-1)?.trim());
        expect(Number.isInteger(fdSharingPid)).toBe(true);
        expect(fdSharingPid).toBeGreaterThan(0);
        expect(fdSharingPid).not.toBe(healthyFlockPid);
        flockPid = healthyFlockPid;
        pidsWatchedByRelease.length = 0;
        healthy?.release();
        expect(new Set(pidsWatchedByRelease)).toEqual(new Set([healthyFlockPid, fdSharingPid]));

        await waitForCondition(() => !isFlockHeld(lockPath), {
          description: "the flock to be observable as free after the watched releases",
        });
      } finally {
        degraded?.release();
        healthy?.release();
        fsInjection.failChildrenRead = false;
        warnSpy.mockRestore();
      }
    },
  );

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

    try {
      await Promise.all([w1.nextLine(), w2.nextLine()]);

      const totalRounds = 200;
      let singleMasterCount = 0;
      let doubleMasterCount = 0;
      let bothLostCount = 0;

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
        } else if (wonCount === 2) {
          doubleMasterCount++;
        } else if (lostCount === 2) {
          bothLostCount++;
        } else {
          throw new Error(`unexpected race outcomes for round ${r}: ${res1}, ${res2}`);
        }
      }

      w1.child.kill("SIGKILL");
      w2.child.kill("SIGKILL");

      const countsMsg = `(WON,WON) double-master=${doubleMasterCount}; (LOST,LOST) both-lost=${bothLostCount}; (WON,LOST) single=${singleMasterCount}`;
      expect(doubleMasterCount, countsMsg).toBe(0);
      expect(bothLostCount, countsMsg).toBeLessThanOrEqual(10);
      expect(singleMasterCount + bothLostCount + doubleMasterCount, countsMsg).toBe(200);
    } finally {
      // Both workers keep their stdin (and the lock) until they are signalled, so
      // they must not outlive this test even when an assertion above fails; the
      // inline kills stay as the happy path. `child.kill` on an exited child is a
      // no-op rather than an error.
      w1.child.kill("SIGKILL");
      w2.child.kill("SIGKILL");
    }
    // 单跑约 1.4s，但并行复跑会升到 4～6.5s（3 路并发的全量复跑已越过 vitest 默认 5s），
    // 故显式抬高本用例超时，避免并行复跑时的假失败。
  }, 30_000);
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
