import { spawn } from "node:child_process";
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
import { basename, dirname, join } from "node:path";

export type DaemonRuntimeRecord = {
  dbPath: string;
  homeDir: string;
  logPath: string;
  pid: number;
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
      pidPath: string;
      socketPath: string;
      socketReachable: true;
      stalePid: number;
      state: "orphaned";
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
      typeof value.pid !== "number" ||
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
        state: "orphaned",
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
        state: "orphaned",
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
  if (existing.state === "running")
    throw new Error(`Herdsman daemon is already running with pid ${existing.pid}`);
  if (existing.state === "orphaned")
    throw new Error(
      `Herdsman daemon socket is reachable but its PID is stale: ${existing.socketPath}. Remove the stale socket or stop the listening process before restarting.`,
    );
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
    writeFileSync(input.pidPath, `${child.pid}\n`, { mode: 0o600 });
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
    writeDaemonRuntimeRecord(input.runtimeRecordPath, {
      ...input.runtimeRecord,
      pid: child.pid,
      startedAt: new Date().toISOString(),
      version: 1,
    });
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
  if (status.state === "orphaned") {
    throw new Error(
      `Herdsman daemon socket is reachable but its PID is stale: ${status.socketPath}. Remove the stale socket or stop the listening process before stopping.`,
    );
  }

  const killProcess = deps.killProcess ?? ((pid, signal) => process.kill(pid, signal));
  const processIsRunning = deps.isProcessRunning ?? isProcessRunning;
  const waitMs = deps.waitMs ?? ((ms) => new Promise<void>((resolve) => setTimeout(resolve, ms)));
  const pid = status.pid;
  if (pid === undefined) throw new Error("Herdsman daemon status is missing a PID");

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
  throw new Error(`Timed out waiting for Herdsman daemon pid ${status.pid} to stop after SIGKILL`);
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
    const ownerPath = join(lockPath, "owner.json");
    if (existsSync(ownerPath)) {
      try {
        const data = JSON.parse(readFileSync(ownerPath, "utf8")) as Partial<DaemonLockOwner>;
        if (typeof data.pid === "number" && data.pid !== expectedPid) {
          return;
        }
      } catch {
        // If owner.json is corrupted, do not remove lock belonging to others
      }
    }
    rmSync(lockPath, { force: true, recursive: true });
  } catch {
    // Ignore lock release error
  }
}

function sleepSync(ms: number): void {
  try {
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
  } catch {
    const start = Date.now();
    while (Date.now() - start < ms) {
      // fallback busy-wait
    }
  }
}

export function acquireDaemonLock(lockPath: string, deps?: DaemonProcessDependencies): () => void {
  mkdirSync(dirname(lockPath), { mode: 0o700, recursive: true });
  const processIsRunning = deps?.isProcessRunning ?? isProcessRunning;
  const identityProbe = deps?.identityProbe ?? readDaemonProcessIdentity;

  const writeAndVerifyOwner = (): boolean => {
    const ownerPath = join(lockPath, "owner.json");
    const owner: DaemonLockOwner = {
      pid: process.pid,
      startedAt: new Date().toISOString(),
    };
    try {
      writeFileSync(ownerPath, `${JSON.stringify(owner, null, 2)}\n`, { mode: 0o600 });
      const content = readFileSync(ownerPath, "utf8");
      const data = JSON.parse(content) as Partial<DaemonLockOwner>;
      return data.pid === process.pid;
    } catch {
      return false;
    }
  };

  const checkOwnerAlive = (): { alive: boolean; pid?: number } => {
    const ownerPath = join(lockPath, "owner.json");
    if (!existsSync(ownerPath)) {
      try {
        const stat = statSync(lockPath);
        const ageMs = Date.now() - stat.mtimeMs;
        if (ageMs < 1000) {
          // Grace period: lock directory is fresh (< 1s), back off briefly and recheck
          sleepSync(50);
          if (!existsSync(ownerPath)) {
            // Still missing, but lock directory is < 1s old; do not break!
            return { alive: true };
          }
        } else {
          // Directory is older than 1s and owner.json is missing -> stale lock
          return { alive: false };
        }
      } catch {
        return { alive: false };
      }
    }

    try {
      const data = JSON.parse(readFileSync(ownerPath, "utf8")) as Partial<DaemonLockOwner>;
      const ownerPid = data.pid;
      if (typeof ownerPid !== "number" || !Number.isInteger(ownerPid) || ownerPid <= 0) {
        return { alive: false };
      }
      if (!processIsRunning(ownerPid)) {
        return { alive: false, pid: ownerPid };
      }
      const probe = identityProbe(ownerPid, CLI_ENTRYPOINT_NAMES);
      if (probe === false) {
        return { alive: false, pid: ownerPid };
      }
      return { alive: true, pid: ownerPid };
    } catch {
      try {
        const stat = statSync(lockPath);
        if (Date.now() - stat.mtimeMs < 1000) {
          return { alive: true };
        }
      } catch {}
      return { alive: false };
    }
  };

  const formatLockHeldError = (pid?: number) => {
    const ownerInfo = pid !== undefined ? ` by PID ${pid}` : "";
    return `Herdsman daemon operation lock is held${ownerInfo}: ${lockPath}. 确认无 daemon 操作在跑后可删除: ${lockPath}`;
  };

  const tryAcquire = (): boolean => {
    try {
      mkdirSync(lockPath, { mode: 0o700 });
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "EEXIST") {
        return false;
      }
      throw error;
    }
    return writeAndVerifyOwner();
  };

  if (tryAcquire()) {
    return () => releaseDaemonLock(lockPath, process.pid);
  }

  const ownerStatus = checkOwnerAlive();
  if (ownerStatus.alive) {
    throw new Error(formatLockHeldError(ownerStatus.pid));
  }

  rmSync(lockPath, { force: true, recursive: true });

  if (tryAcquire()) {
    return () => releaseDaemonLock(lockPath, process.pid);
  }

  const retryOwner = checkOwnerAlive();
  throw new Error(formatLockHeldError(retryOwner.pid));
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

function defaultConnectSocket(socketPath: string): Promise<boolean> {
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
