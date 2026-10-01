import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";

/**
 * Read-only supervision facts for `herdsman daemon status`.
 *
 * These answer "who supervises this daemon, and how many times has it
 * restarted?" without touching any lifecycle behaviour: nothing here starts,
 * stops, restarts or locks the daemon. Every probe is best-effort, so a
 * missing `systemctl`, a denied `/proc` read or a wedged system manager
 * degrades the answer instead of failing `daemon status`.
 *
 * Diagnostic path only: `readSystemdRestartCount` runs a *synchronous* command
 * (`spawnSync`), which blocks the event loop for up to the probe timeout.
 * These probes belong to `daemon status`; daemon lifecycle code must never call
 * them.
 */

/** The systemd unit that supervises the production daemon (see AGENTS.md). */
export const HERDSMAN_SYSTEMD_UNIT = "herdsman.service";

/**
 * Short, fixed timeout for the best-effort `systemctl` probe. `daemon status`
 * is a diagnostic command, so it must never hang behind a wedged or unavailable
 * system manager; 1.5s is far above a healthy `systemctl show` (single-digit
 * ms) yet small enough to stay imperceptible when the manager is stuck.
 */
export const SUPERVISION_PROBE_TIMEOUT_MS = 1500;

/**
 * Who supervises the running daemon:
 * - `systemd:<unit>` — the daemon pid lives in that unit's cgroup inside the
 *   *system* slice, i.e. systemd's system manager owns it;
 * - `unmanaged` — the cgroup was readable and named something, but the pid is
 *   not supervised by `herdsman.service`. That is all it says: it is **not** a
 *   claim that nothing else supervises the process, and it is not a licence to
 *   kill it;
 * - `unknown` — no fact was available: the cgroup could not be read, or it read
 *   back empty/blank. "Nothing was read" is not the same as "not managed by
 *   that unit", so it must not be downgraded to `unmanaged`.
 *
 * When no daemon pid is known, this fact is not reported at all instead of
 * being set to `unknown` (see `DaemonStatusFacts` in `process-manager.ts`).
 */
export type DaemonManagedBy = "unmanaged" | "unknown" | `systemd:${string}`;

/** Prefix of the system manager's cgroup subtree, identical in cgroup v1 and v2. */
const SYSTEM_SLICE_PREFIX = "/system.slice/";

/**
 * True when `path` is the unit's own cgroup or a nested cgroup inside it. The
 * comparison is on the full path (not the basename) with a trailing-slash guard
 * so `herdsman.service.dev` is not mistaken for the unit itself.
 */
function isSystemUnitCgroupPath(path: string, unit: string): boolean {
  const unitPath = `${SYSTEM_SLICE_PREFIX}${unit}`;
  return path === unitPath || path.startsWith(`${unitPath}/`);
}

/**
 * Maps the content of `/proc/<pid>/cgroup` to a supervision fact. Pure, so it
 * can be exercised with fabricated cgroup text. Handles both layouts:
 * cgroup v2 `0::/system.slice/herdsman.service` and cgroup v1
 * `1:name=systemd:/system.slice/herdsman.service`.
 */
export function managedByFromCgroup(
  cgroup: string,
  unit: string = HERDSMAN_SYSTEMD_UNIT,
): DaemonManagedBy {
  let sawContent = false;
  for (const line of cgroup.split("\n")) {
    const trimmed = line.trim();
    if (trimmed === "") continue;
    sawContent = true;
    const path = trimmed.slice(trimmed.lastIndexOf(":") + 1);
    // The path must be the unit's own cgroup *and* live in the system slice.
    // The slice check is not cosmetic: a user-session unit of the same name
    // (`/user.slice/user-1000.slice/user@1000.service/app.slice/herdsman.service`)
    // ends in the very same basename, so a basename-only match reported it as
    // `systemd:herdsman.service` — the exact same string as the system unit —
    // and an operator following that would `systemctl restart` the wrong unit.
    if (isSystemUnitCgroupPath(path, unit)) return `systemd:${unit}`;
  }
  // Blank/empty content means the read produced no fact at all, which is
  // `unknown`, not "not managed by that unit".
  return sawContent ? "unmanaged" : "unknown";
}

/** Reads `/proc/<pid>/cgroup`; undefined when procfs is absent or unreadable. */
export function readProcessCgroup(pid: number): string | undefined {
  try {
    return readFileSync(`/proc/${pid}/cgroup`, "utf8");
  } catch {
    return undefined;
  }
}

/** Injectable command runner so tests can supply fake `systemctl` behaviour. */
export type SystemctlRunner = (
  command: string,
  args: string[],
  timeoutMs: number,
) => string | undefined;

/**
 * Default runner: bounded `spawnSync` that never throws. On spawn error, a
 * non-zero exit (including a timeout kill) or a missing stdout it reports
 * `undefined`, i.e. "no fact available".
 *
 * `killSignal: "SIGKILL"` is what makes `timeout` a hard bound. The default is
 * SIGTERM, which a child may simply ignore — and an ignoring child then keeps
 * this *synchronous* call parked for its whole lifetime, so `daemon status`
 * hangs behind it (measured on this machine with a `trap '' TERM; exec sleep 8`
 * shim and `timeoutMs: 300`: 8003ms with the default signal, 301ms with
 * SIGKILL). SIGKILL cannot be ignored, and `systemctl show` is a read-only
 * client with nothing to clean up, so killing it hard is safe. Same lesson as
 * the `execFile` timeout in `src/herdr/session-list.ts`.
 *
 * Diagnostic path only: this blocks the event loop for up to `timeoutMs`.
 */
function defaultSystemctlRunner(
  command: string,
  args: string[],
  timeoutMs: number,
): string | undefined {
  const result = spawnSync(command, args, {
    encoding: "utf8",
    killSignal: "SIGKILL",
    timeout: timeoutMs,
  });
  if (result.error || result.status !== 0) return undefined;
  return typeof result.stdout === "string" ? result.stdout : undefined;
}

/**
 * Best-effort `systemctl show <unit> -p NRestarts --value`. Returns undefined on
 * any failure (command missing, permission denied, timeout, non-zero exit,
 * unparsable output) and never throws, so the caller can omit the field.
 *
 * How to read the number: it is systemd's `NRestarts` for the *unit*, and
 * `systemctl reset-failed`, a `systemctl stop` + `start` pair, or a unit that
 * is not currently loaded all reset it to 0. So `0` means "systemd has not
 * restarted this unit since it last loaded it", never "this daemon has never
 * crashed".
 */
export function readSystemdRestartCount(options?: {
  command?: string;
  runner?: SystemctlRunner;
  timeoutMs?: number;
  unit?: string;
}): number | undefined {
  const unit = options?.unit ?? HERDSMAN_SYSTEMD_UNIT;
  const command = options?.command ?? "systemctl";
  const timeoutMs = options?.timeoutMs ?? SUPERVISION_PROBE_TIMEOUT_MS;
  const runner = options?.runner ?? defaultSystemctlRunner;

  let stdout: string | undefined;
  try {
    stdout = runner(command, ["show", unit, "-p", "NRestarts", "--value"], timeoutMs);
  } catch {
    return undefined;
  }
  if (stdout === undefined) return undefined;

  const value = Number(stdout.trim());
  return Number.isInteger(value) && value >= 0 ? value : undefined;
}
