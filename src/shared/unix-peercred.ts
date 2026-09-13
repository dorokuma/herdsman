import { spawnSync } from "node:child_process";
import { existsSync, readFileSync, readlinkSync, realpathSync, statSync } from "node:fs";
import type { Socket } from "node:net";
import { isAbsolute } from "node:path";

/** Linux `SO_PEERCRED` (include/uapi/asm-generic/socket.h). */
const SOL_SOCKET = 1;
const SO_PEERCRED = 17;

/** Distro python3 only — never search PATH (a fake `python3` earlier on PATH must not win). */
export const PYTHON3_CANDIDATES = ["/usr/bin/python3", "/bin/python3"] as const;

let cachedPython3Path: string | undefined;

export function unixSocketFd(socket: Socket): number | undefined {
  const fd = (socket as { _handle?: { fd?: number } })._handle?.fd;
  return typeof fd === "number" && Number.isInteger(fd) && fd >= 0 ? fd : undefined;
}

/**
 * Read the connecting process pid from a Linux Unix-domain socket via SO_PEERCRED.
 * Same-UID threat model: this binds a connection to a process, not a user.
 */
export function linuxPeerPid(socket: Socket): number | undefined {
  if (process.platform !== "linux") return undefined;
  const fd = unixSocketFd(socket);
  return fd === undefined ? undefined : linuxPeerPidFromFd(fd);
}

export function linuxPeerPidFromFd(fd: number): number | undefined {
  if (process.platform !== "linux") return undefined;
  const python3 = resolvePython3Path();
  if (!python3) {
    console.error(
      "Herdsman cannot read UDS peer pid: python3 not found at /usr/bin/python3 or /bin/python3",
    );
    return undefined;
  }
  const result = spawnSync(
    python3,
    [
      "-c",
      [
        "import socket, struct, sys",
        `s = socket.fromfd(0, socket.AF_UNIX, socket.SOCK_STREAM)`,
        `cred = s.getsockopt(${SOL_SOCKET}, ${SO_PEERCRED}, struct.calcsize("3i"))`,
        "pid, _uid, _gid = struct.unpack('3i', cred)",
        "sys.stdout.write(str(pid))",
      ].join("; "),
    ],
    {
      encoding: "utf8",
      stdio: [fd, "pipe", "pipe"],
      timeout: 2_000,
    },
  );
  if (result.error || result.status !== 0) {
    console.error(
      "Herdsman python3 SO_PEERCRED helper failed",
      result.error?.message ?? (result.stderr || `exit ${result.status}`),
    );
    return undefined;
  }
  const pid = Number.parseInt((result.stdout ?? "").trim(), 10);
  return Number.isInteger(pid) && pid > 1 ? pid : undefined;
}

export function resolvePython3Path(): string | undefined {
  if (cachedPython3Path) return cachedPython3Path;
  for (const candidate of PYTHON3_CANDIDATES) {
    if (!isTrustedPython3(candidate)) continue;
    cachedPython3Path = candidate;
    return candidate;
  }
  return undefined;
}

export function peerBoundToPanePid(peerPid: number, panePid: number): boolean {
  if (!Number.isInteger(peerPid) || !Number.isInteger(panePid) || peerPid <= 1 || panePid <= 1) {
    return false;
  }
  if (peerPid === panePid) return true;
  return isAncestorPid(panePid, peerPid);
}

export function peerBoundToPaneCwd(peerPid: number, paneCwd: string): boolean {
  if (!Number.isInteger(peerPid) || peerPid <= 1) return false;
  if (typeof paneCwd !== "string" || paneCwd.length === 0) return false;
  const peerCwd = processCwd(peerPid);
  if (!peerCwd) return false;
  return sameCwd(peerCwd, paneCwd);
}

function processCwd(pid: number): string | undefined {
  try {
    return readlinkSync(`/proc/${pid}/cwd`);
  } catch {
    return undefined;
  }
}

function sameCwd(left: string, right: string): boolean {
  return normalizeCwd(left) === normalizeCwd(right);
}

function normalizeCwd(value: string): string {
  try {
    return realpathSync(value);
  } catch {
    return value.length > 1 && value.endsWith("/") ? value.slice(0, -1) : value;
  }
}

function isTrustedPython3(path: string): boolean {
  if (!isAbsolute(path)) return false;
  try {
    if (!existsSync(path)) return false;
    const st = statSync(path);
    if (!st.isFile()) return false;
    const resolved = realpathSync(path);
    return resolved.startsWith("/usr/bin/python") || resolved.startsWith("/bin/python");
  } catch {
    return false;
  }
}

function isAncestorPid(ancestor: number, pid: number): boolean {
  let current = pid;
  for (let step = 0; step < 32; step += 1) {
    const parent = parentPid(current);
    if (parent === undefined || parent <= 1) return false;
    if (parent === ancestor) return true;
    if (parent === current) return false;
    current = parent;
  }
  return false;
}

function parentPid(pid: number): number | undefined {
  try {
    const status = readFileSync(`/proc/${pid}/status`, "utf8");
    const match = /^PPid:\s*(\d+)\s*$/m.exec(status);
    if (!match) return undefined;
    const ppid = Number.parseInt(match[1] ?? "", 10);
    return Number.isInteger(ppid) && ppid >= 0 ? ppid : undefined;
  } catch {
    return undefined;
  }
}
