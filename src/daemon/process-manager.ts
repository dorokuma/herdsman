import { spawn, spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import {
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  rmSync,
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

export const DAEMON_ENTRYPOINT_NAMES = ["herdsman-daemon.js"] as const;

/**
 * Instance lock path for the daemon's bare entrypoint (herdsman-daemon.js).
 * Kept distinct from the plain `${pidPath}.lock` name so a daemon that owns
 * its home through this lock cannot collide with any other lock namespace.
 */
export function daemonInstanceLockPath(pidPath: string): string {
  return `${pidPath}.instance.lock`;
}

const warnedUnknownIdentityPids = new Set<number>();

export type DaemonProcessDependencies = {
  connectSocket?: (socketPath: string) => Promise<boolean>;
  isProcessRunning?: (pid: number) => boolean;
  identityProbe?: (pid: number, expectedNames?: readonly string[]) => boolean | undefined;
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

export type FlockHandleDependencies = {
  /**
   * Internal seam kept for tests only: replaces the `/proc/<pid>/stat` read used
   * by the child liveness check. Omitting it is byte-for-byte equivalent to the
   * production read (`readFileSync("/proc/<pid>/stat", "utf8")`); tests pass a
   * reader that simulates an unreadable or truncated procfs, which deliberately
   * changes what the liveness check concludes — that is the whole point of the
   * seam.
   */
  readProcessStat?: (pid: number) => string;
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

function isChildProcessActive(
  pid: number,
  readProcessStat: (pid: number) => string = (targetPid) =>
    readFileSync(`/proc/${targetPid}/stat`, "utf8"),
): boolean {
  if (existsSync("/proc")) {
    try {
      const stat = readProcessStat(pid);
      const lastParen = stat.lastIndexOf(")");
      // procfs reports st_size 0, so the read has no size hint and can come back
      // truncated/empty. Unparsable content proves nothing about liveness, so it
      // must not be read as "the process is gone".
      if (lastParen === -1) return true;
      const rest = stat.slice(lastParen + 2).trim();
      const state = rest.charAt(0);
      // Z/X are exited (the kernel already dropped their flock) while T is only
      // stopped but still holds it. T stays grouped with Z/X on purpose: this batch
      // splits the errno cases only. See A7 in
      // .agents/notes/20260930-terminal-event-delivery-open-items.md.
      return state !== "Z" && state !== "X" && state !== "T";
    } catch (error) {
      // Only the kernel proving the pid is gone counts as dead: ENOENT (no
      // /proc/<pid>/stat entry) or ESRCH. Any other failure (EACCES, EPERM, EIO,
      // unknown) means "cannot tell", and a lock-holding child must never be
      // SIGKILLed because of a transient procfs read failure.
      const code = (error as NodeJS.ErrnoException | null | undefined)?.code;
      return code !== "ENOENT" && code !== "ESRCH";
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
export function acquireFlockHandle(
  lockPath: string,
  deps?: FlockHandleDependencies,
): FlockHandle | null {
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
        if (content.startsWith("READY") && isChildProcessActive(child.pid, deps?.readProcessStat)) {
          acquired = true;
          try {
            rmSync(ackFile, { force: true });
          } catch {}
          break;
        }
      } catch {}
    }
    // Check if child exited (e.g. flock returned 1 because lock is held by another process)
    if (!isChildProcessActive(child.pid, deps?.readProcessStat)) {
      break;
    }
    const until = Date.now() + 1;
    while (Date.now() < until) {}
  }

  try {
    rmSync(ackFile, { force: true });
  } catch {}

  if (!acquired || !isChildProcessActive(child.pid, deps?.readProcessStat)) {
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
          if (!isChildProcessActive(child.pid, deps?.readProcessStat)) {
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
    return `Herdsman daemon operation lock is held${ownerInfo}: ${lockPath}. 先看 systemctl status herdsman.service 确认 daemon 状态；仅当确认无 daemon 与 CLI 操作在跑时才可删除锁文件（删掉会让新进程在新 inode 上重新加锁成功，可能出现两个实例同时跑）: ${lockPath}`;
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
