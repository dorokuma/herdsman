import { spawn, spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import {
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { createConnection } from "node:net";
import { basename, dirname } from "node:path";

export type DaemonRuntimeRecord = {
  dbPath: string;
  homeDir: string;
  logPath: string;
  pid?: number;
  pidPath: string;
  socketPath: string;
  startedAt: string;
  version: 1;
};

export type DaemonStatus =
  | {
      pid?: number;
      pidPath: string;
      pidFileMissing: true;
      socketPath: string;
      socketReachable: true;
      state: "running";
    }
  | {
      // Socket is reachable, so a daemon is running, but the pid file points to a
      // dead or foreign PID. The daemon is managed outside this pid file (e.g.
      // by systemd); stalePid is metadata only, not an orphan signal.
      pidPath: string;
      socketPath: string;
      socketReachable: true;
      stalePid: number;
      state: "running";
    }
  | { pidPath: string; socketPath: string; state: "stopped"; stalePid?: number }
  | {
      pid: number;
      pidPath: string;
      socketPath: string;
      socketReachable: boolean;
      state: "running";
    };

type DaemonSpawnProcess = (
  command: string,
  args: string[],
  options: {
    detached: boolean;
    env: NodeJS.ProcessEnv;
    stdio: ["ignore", number, number];
  },
) => { pid: number | undefined; unref(): void };

export const DAEMON_ENTRYPOINT_NAMES = ["herdsman-daemon.js"] as const;
export const CLI_ENTRYPOINT_NAMES = ["herdsman-daemon.js", "herdsman.js", "herdsman"] as const;

/**
 * Instance lock path for the daemon's bare entrypoint (herdsman-daemon.js).
 * Deliberately distinct from the CLI operation lock (`${pidPath}.lock`) so a
 * daemon started outside the CLI does not collide with CLI start/stop/restart
 * operations, and vice versa.
 */
export function daemonInstanceLockPath(pidPath: string): string {
  return `${pidPath}.instance.lock`;
}

const warnedUnknownIdentityPids = new Set<number>();

export type DaemonProcessDependencies = {
  connectSocket?: (socketPath: string) => Promise<boolean>;
  readinessProbe?: (socketPath: string) => Promise<boolean>;
  isProcessRunning?: (pid: number) => boolean;
  identityProbe?: (pid: number, expectedNames?: readonly string[]) => boolean | undefined;
  killProcess?: (pid: number, signal: NodeJS.Signals) => void;
  spawnProcess?: DaemonSpawnProcess;
  waitMs?: (ms: number) => Promise<void>;
  readinessTimeoutMs?: number;
  pid?: number | undefined;
};
export function readDaemonRuntimeRecord(path: string): DaemonRuntimeRecord | undefined {
  if (!existsSync(path)) {
    return undefined;
  }

  try {
    const value = JSON.parse(readFileSync(path, "utf8")) as Partial<DaemonRuntimeRecord>;
    if (
      value.version !== 1 ||
      typeof value.dbPath !== "string" ||
      typeof value.homeDir !== "string" ||
      typeof value.logPath !== "string" ||
      typeof value.pidPath !== "string" ||
      typeof value.socketPath !== "string" ||
      typeof value.startedAt !== "string"
    ) {
      return undefined;
    }

    return value as DaemonRuntimeRecord;
  } catch {
    return undefined;
  }
}

export function writeDaemonPidFile(pidPath: string, pid: number): void {
  mkdirSync(dirname(pidPath), { mode: 0o700, recursive: true });
  writeFileSync(pidPath, `${pid}\n`, { mode: 0o600 });
}

export function removeDaemonPidFile(pidPath: string, expectedPid: number): boolean {
  try {
    if (!existsSync(pidPath)) return false;
    const content = readFileSync(pidPath, "utf8").trim();
    if (Number(content) !== expectedPid) return false;
    rmSync(pidPath, { force: true });
    return true;
  } catch {
    // Never let pid clean-up failure abort shutdown
    return false;
  }
}

export function writeDaemonRuntimeRecord(path: string, record: DaemonRuntimeRecord): void {
  mkdirSync(dirname(path), { mode: 0o700, recursive: true });
  writeFileSync(path, `${JSON.stringify(record, null, 2)}\n`, { mode: 0o600 });
}

export async function prepareDaemonSocketPath(input: {
  deps?: DaemonProcessDependencies;
  socketPath: string;
}): Promise<void> {
  mkdirSync(dirname(input.socketPath), { mode: 0o700, recursive: true });
  if (!existsSync(input.socketPath)) {
    return;
  }

  const connectSocket = input.deps?.connectSocket ?? defaultConnectSocket;
  if (await connectSocket(input.socketPath)) {
    throw new Error(`Herdsman daemon socket is already reachable: ${input.socketPath}`);
  }

  rmSync(input.socketPath, { force: true });
}

export async function getDaemonStatus(input: {
  deps?: DaemonProcessDependencies;
  pidPath: string;
  socketPath: string;
}): Promise<DaemonStatus> {
  const processIsRunning = input.deps?.isProcessRunning ?? isProcessRunning;
  const identityProbe = input.deps?.identityProbe ?? readDaemonProcessIdentity;
  const connectSocket = input.deps?.connectSocket ?? defaultConnectSocket;

  if (!existsSync(input.pidPath)) {
    if (await connectSocket(input.socketPath)) {
      return {
        pidPath: input.pidPath,
        pidFileMissing: true,
        socketPath: input.socketPath,
        socketReachable: true,
        state: "running",
      };
    }
    return { pidPath: input.pidPath, socketPath: input.socketPath, state: "stopped" };
  }

  const pid = Number(readFileSync(input.pidPath, "utf8").trim());
  if (!Number.isInteger(pid) || pid <= 0 || !processIsRunning(pid)) {
    if (await connectSocket(input.socketPath)) {
      return {
        pidPath: input.pidPath,
        socketPath: input.socketPath,
        socketReachable: true,
        stalePid: pid,
        state: "running",
      };
    }
    return {
      pidPath: input.pidPath,
      socketPath: input.socketPath,
      stalePid: pid,
      state: "stopped",
    };
  }

  const isIdentified = identityProbe(pid, DAEMON_ENTRYPOINT_NAMES);
  if (isIdentified === false) {
    if (await connectSocket(input.socketPath)) {
      return {
        pidPath: input.pidPath,
        socketPath: input.socketPath,
        socketReachable: true,
        stalePid: pid,
        state: "running",
      };
    }
    return {
      pidPath: input.pidPath,
      socketPath: input.socketPath,
      stalePid: pid,
      state: "stopped",
    };
  }

  if (isIdentified === undefined && !warnedUnknownIdentityPids.has(pid)) {
    warnedUnknownIdentityPids.add(pid);
    console.warn(`Unable to verify daemon process identity for PID ${pid}: /proc unavailable`);
  }

  return {
    pid,
    pidPath: input.pidPath,
    socketPath: input.socketPath,
    socketReachable: await connectSocket(input.socketPath),
    state: "running",
  };
}

export async function startDaemonProcess(input: {
  deps?: DaemonProcessDependencies;
  entrypointPath: string;
  env: NodeJS.ProcessEnv;
  logPath: string;
  nodePath: string;
  pidPath: string;
  runtimeRecord: Omit<DaemonRuntimeRecord, "pid" | "startedAt" | "version">;
  runtimeRecordPath: string;
  socketPath: string;
}): Promise<{ pid: number }> {
  mkdirSync(dirname(input.pidPath), { mode: 0o700, recursive: true });
  mkdirSync(dirname(input.logPath), { mode: 0o700, recursive: true });
  const existing = await getDaemonStatus({
    ...(input.deps !== undefined ? { deps: input.deps } : {}),
    pidPath: input.pidPath,
    socketPath: input.socketPath,
  });
  if (existing.state === "running") {
    if (existing.socketReachable) {
      const pid = "pid" in existing ? existing.pid : undefined;
      throw new Error(
        pid !== undefined
          ? `Herdsman daemon is already running with pid ${pid}: ${existing.socketPath}`
          : `Herdsman daemon is already running: ${existing.socketPath}`,
      );
    }
    const pid = "pid" in existing ? existing.pid : undefined;
    throw new Error(
      `Herdsman daemon process is already running${pid !== undefined ? ` with pid ${pid}` : ""} but its socket is not reachable: ${existing.socketPath}`,
    );
  }
  if (existing.stalePid !== undefined) rmSync(input.pidPath, { force: true });
  let pidFd: number;
  try {
    pidFd = openSync(input.pidPath, "wx", 0o600);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "EEXIST") {
      throw new Error(`Herdsman daemon is already running: ${input.pidPath}`);
    }
    throw error;
  }
  let childPid: number | undefined;
  let childConfirmedDead = false;
  try {
    await prepareDaemonSocketPath({
      ...(input.deps !== undefined ? { deps: input.deps } : {}),
      socketPath: input.socketPath,
    });
    const logFd = openRotatedLog(input.logPath);
    let child: { pid: number | undefined; unref(): void };
    try {
      child = (input.deps?.spawnProcess ?? spawnDaemonProcess)(
        input.nodePath,
        [input.entrypointPath],
        {
          detached: true,
          env: input.env,
          stdio: ["ignore", logFd, logFd],
        },
      );
    } finally {
      closeSync(logFd);
    }

    if (!child.pid) throw new Error("Failed to start Herdsman daemon: child pid was not assigned");
    childPid = child.pid;
    child.unref();
    writeDaemonPidFile(input.pidPath, child.pid);
    try {
      const waitMs =
        input.deps?.waitMs ?? ((ms) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
      const connectSocket = input.deps?.readinessProbe ?? defaultConnectSocket;
      const deadline = Date.now() + (input.deps?.readinessTimeoutMs ?? 10_000);
      while (Date.now() < deadline && !(await connectSocket(input.socketPath))) await waitMs(50);
      if (!(await connectSocket(input.socketPath)))
        throw new Error("Timed out waiting for Herdsman daemon socket");
    } catch (error) {
      const killProcess = input.deps?.killProcess ?? ((pid, signal) => process.kill(pid, signal));
      const processIsRunning = input.deps?.isProcessRunning ?? isProcessRunning;
      const waitMs =
        input.deps?.waitMs ?? ((ms) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
      killProcess(child.pid, "SIGTERM");
      const deadline = Date.now() + (input.deps?.readinessTimeoutMs ?? 10_000);
      while (Date.now() < deadline && processIsRunning(child.pid)) await waitMs(50);
      if (processIsRunning(child.pid)) {
        killProcess(child.pid, "SIGKILL");
        const killDeadline = Date.now() + (input.deps?.readinessTimeoutMs ?? 10_000);
        while (Date.now() < killDeadline && processIsRunning(child.pid)) await waitMs(50);
      }
      if (!processIsRunning(child.pid)) {
        childConfirmedDead = true;
        rmSync(input.pidPath, { force: true });
      }
      throw error;
    }
    const record: DaemonRuntimeRecord = {
      ...input.runtimeRecord,
      startedAt: new Date().toISOString(),
      version: 1,
    };
    // The runtime record no longer carries a pid: the pid file is the pid
    // source of truth, and a stale pid here misleads liveness checks.
    delete record.pid;
    writeDaemonRuntimeRecord(input.runtimeRecordPath, record);
    return { pid: child.pid };
  } catch (error) {
    if (childPid !== undefined && childConfirmedDead) rmSync(input.pidPath, { force: true });
    throw error;
  } finally {
    closeSync(pidFd);
  }
}

export async function stopDaemonProcess(input: {
  deps?: DaemonProcessDependencies;
  pidPath: string;
  socketPath: string;
  timeoutMs: number;
}): Promise<{ alreadyStopped: boolean; pid?: number }> {
  const deps = input.deps ?? {};
  const status = await getDaemonStatus({
    deps,
    pidPath: input.pidPath,
    socketPath: input.socketPath,
  });
  if (status.state === "stopped") {
    rmSync(input.pidPath, { force: true });
    return { alreadyStopped: true };
  }

  const killProcess = deps.killProcess ?? ((pid, signal) => process.kill(pid, signal));
  const processIsRunning = deps.isProcessRunning ?? isProcessRunning;
  const waitMs = deps.waitMs ?? ((ms) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
  const pid = "pid" in status ? status.pid : undefined;
  if (pid === undefined) {
    const stalePid = "stalePid" in status ? status.stalePid : undefined;
    if (status.state === "running" && stalePid !== undefined) {
      throw new Error(
        `Herdsman daemon socket is reachable at ${status.socketPath} but pid file ${status.pidPath} refers to stale PID ${stalePid}; the daemon is managed outside this pid file and cannot be stopped via CLI`,
      );
    }
    throw new Error("Herdsman daemon status is missing a PID");
  }

  killProcess(pid, "SIGTERM");
  const deadline = Date.now() + input.timeoutMs;
  while (Date.now() < deadline) {
    if (!processIsRunning(pid)) {
      rmSync(input.pidPath, { force: true });
      return { alreadyStopped: false, pid };
    }
    await waitMs(50);
  }

  killProcess(pid, "SIGKILL");
  const killDeadline = Date.now() + input.timeoutMs;
  while (Date.now() < killDeadline && processIsRunning(pid)) await waitMs(50);
  if (!processIsRunning(pid)) {
    rmSync(input.pidPath, { force: true });
    return { alreadyStopped: false, pid };
  }
  throw new Error(`Timed out waiting for Herdsman daemon pid ${pid} to stop after SIGKILL`);
}

function openRotatedLog(path: string): number {
  try {
    if (statSync(path).size > 10 * 1024 * 1024) renameSync(path, `${path}.1`);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  return openSync(path, "a", 0o600);
}
function spawnDaemonProcess(
  command: string,
  args: string[],
  options: Parameters<DaemonSpawnProcess>[2],
): { pid: number | undefined; unref(): void } {
  const child = spawn(command, args, options);
  return { pid: child.pid, unref: () => child.unref() };
}

type ProcessProbe = (pid: number, signal: 0) => unknown;

export function readDaemonProcessIdentity(
  pid: number,
  expectedNames: readonly string[] = DAEMON_ENTRYPOINT_NAMES,
): boolean | undefined {
  if (!existsSync("/proc")) {
    return undefined;
  }
  try {
    const cmdline = readFileSync(`/proc/${pid}/cmdline`, "utf8");
    const parts = cmdline.split("\0").filter(Boolean);
    return parts.some((part) => expectedNames.includes(basename(part)));
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === "ENOENT" || code === "ESRCH") {
      return false;
    }
    if (code === "EACCES" || code === "EPERM") {
      return undefined;
    }
    // Other read errors (e.g. EINVAL, EIO): safe fallback is undefined (unknown/cannot verify)
    return undefined;
  }
}

export function isProcessRunning(pid: number, probe: ProcessProbe = process.kill): boolean {
  try {
    probe(pid, 0);
    return true;
  } catch (error) {
    return typeof error === "object" && error !== null && "code" in error && error.code === "EPERM";
  }
}

export type DaemonLockOwner = {
  pid: number;
  startedAt: string;
};

export function releaseDaemonLock(lockPath: string, expectedPid: number = process.pid): void {
  try {
    const ownerPath = `${lockPath}.owner.json`;
    if (existsSync(ownerPath)) {
      try {
        const data = JSON.parse(readFileSync(ownerPath, "utf8")) as Partial<DaemonLockOwner>;
        if (typeof data.pid === "number" && data.pid !== expectedPid) {
          return;
        }
        rmSync(ownerPath, { force: true });
      } catch {
        // If owner.json is corrupted, do not remove
        return;
      }
    }
  } catch {
    // Ignore lock release error
  }
}

export type FlockHandle = {
  release: () => void;
};

/**
 * Checks whether a kernel flock is currently held on lockPath by an active process.
 * Spawns non-blocking `flock -x -n <lockPath> true`.
 * Returns true only if flock exited with status 1 (lock held).
 * Throws on environmental/spawn errors or unexpected status codes.
 */
export function isFlockHeld(lockPath: string): boolean {
  if (!existsSync(lockPath)) {
    return false;
  }
  const res = spawnSync("flock", ["-x", "-n", lockPath, "true"], {
    stdio: "ignore",
  });
  if (res.error) {
    throw new Error(`Failed to probe flock on ${lockPath}: ${res.error.message}`, {
      cause: res.error,
    });
  }
  if (res.status === 0) {
    return false;
  }
  if (res.status === 1) {
    return true;
  }
  throw new Error(`flock probe exited with unexpected status ${res.status} on ${lockPath}`);
}

function isChildProcessActive(pid: number): boolean {
  if (existsSync("/proc")) {
    try {
      const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
      const lastParen = stat.lastIndexOf(")");
      if (lastParen === -1) return false;
      const rest = stat.slice(lastParen + 2).trim();
      const state = rest.charAt(0);
      return state !== "Z" && state !== "X" && state !== "T";
    } catch {
      return false;
    }
  }
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

/**
 * Acquires an exclusive non-blocking flock on lockPath using a child process.
 * The child process runs `flock -x -n <lockPath> sh -c 'printf READY > <ackFile>; exec cat'`.
 * The parent process waits for the READY handshake confirmation.
 * The lock file is a persistent regular file and is NEVER removed to prevent inode reuse race conditions.
 */
export function acquireFlockHandle(lockPath: string): FlockHandle | null {
  mkdirSync(dirname(lockPath), { mode: 0o700, recursive: true });
  // Ensure persistent lock file exists and is never removed
  const fd = openSync(lockPath, "a", 0o600);
  closeSync(fd);

  const ackFile = `${lockPath}.ack.${process.pid}.${Date.now()}.${randomUUID()}`;

  const child = spawn(
    "flock",
    ["-x", "-n", lockPath, "sh", "-c", `printf READY > "${ackFile}"; exec cat`],
    {
      detached: true,
      stdio: ["pipe", "ignore", "ignore"],
    },
  );

  if (!child.pid) {
    return null;
  }

  const start = Date.now();
  let acquired = false;

  while (Date.now() - start < 1000) {
    if (existsSync(ackFile)) {
      try {
        const content = readFileSync(ackFile, "utf8");
        if (content.startsWith("READY") && isChildProcessActive(child.pid)) {
          acquired = true;
          try {
            rmSync(ackFile, { force: true });
          } catch {}
          break;
        }
      } catch {}
    }
    // Check if child exited (e.g. flock returned 1 because lock is held by another process)
    if (!isChildProcessActive(child.pid)) {
      break;
    }
    const until = Date.now() + 1;
    while (Date.now() < until) {}
  }

  try {
    rmSync(ackFile, { force: true });
  } catch {}

  if (!acquired || !isChildProcessActive(child.pid)) {
    try {
      if (child.pid) process.kill(-child.pid, "SIGKILL");
    } catch {}
    try {
      child.kill("SIGKILL");
    } catch {}
    return null;
  }

  child.unref();

  let released = false;
  const release = () => {
    if (released) return;
    released = true;
    try {
      try {
        if (child.pid) process.kill(-child.pid, "SIGKILL");
      } catch {}
      try {
        child.kill("SIGKILL");
      } catch {}
      try {
        child.stdin?.destroy();
      } catch {}
      if (child.pid) {
        const deadline = Date.now() + 100;
        while (Date.now() < deadline) {
          if (!isChildProcessActive(child.pid)) {
            break;
          }
          const until = Date.now() + 1;
          while (Date.now() < until) {}
        }
      }
    } catch {
      // Ignore release errors to prevent blocking the release chain
    }
  };

  return { release };
}

export function acquireDaemonLock(lockPath: string, deps?: DaemonProcessDependencies): () => void {
  mkdirSync(dirname(lockPath), { mode: 0o700, recursive: true });
  const currentPid = deps?.pid ?? process.pid;

  const readOwnerPid = (): number | undefined => {
    const ownerPath = `${lockPath}.owner.json`;
    if (!existsSync(ownerPath)) return undefined;
    try {
      const data = JSON.parse(readFileSync(ownerPath, "utf8")) as Partial<DaemonLockOwner>;
      return typeof data.pid === "number" && Number.isInteger(data.pid) && data.pid > 0
        ? data.pid
        : undefined;
    } catch {
      return undefined;
    }
  };

  const formatLockHeldError = (pid?: number) => {
    const ownerInfo = pid !== undefined ? ` by PID ${pid}` : "";
    return `Herdsman daemon operation lock is held${ownerInfo}: ${lockPath}. 确认无 daemon 操作在跑后可删除: ${lockPath}`;
  };

  const flockHandle = acquireFlockHandle(lockPath);
  if (!flockHandle) {
    const ownerPid = readOwnerPid();
    throw new Error(formatLockHeldError(ownerPid));
  }

  // Flock acquired successfully. Write diagnostic owner metadata.
  const ownerPath = `${lockPath}.owner.json`;
  const owner: DaemonLockOwner = {
    pid: currentPid,
    startedAt: new Date().toISOString(),
  };
  try {
    writeFileSync(ownerPath, `${JSON.stringify(owner, null, 2)}\n`, { mode: 0o600 });
  } catch {
    // Diagnostic write error does not invalidate the kernel lock
  }

  let released = false;
  return () => {
    if (released) return;
    released = true;
    try {
      if (existsSync(ownerPath)) {
        try {
          const data = JSON.parse(readFileSync(ownerPath, "utf8")) as Partial<DaemonLockOwner>;
          if (data.pid === currentPid) {
            rmSync(ownerPath, { force: true });
          }
        } catch {}
      }
    } catch {
      // Ignore cleanup error
    }
    flockHandle.release();
  };
}

export async function withDaemonLock<T>(
  lockPath: string,
  action: () => Promise<T>,
  deps?: DaemonProcessDependencies,
): Promise<T> {
  const release = acquireDaemonLock(lockPath, deps);
  try {
    return await action();
  } finally {
    release();
  }
}

export function defaultConnectSocket(socketPath: string): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = createConnection(socketPath);
    const done = (value: boolean) => {
      socket.removeAllListeners();
      socket.destroy();
      resolve(value);
    };
    socket.once("connect", () => done(true));
    socket.once("error", () => done(false));
    socket.setTimeout(200, () => done(false));
  });
}
