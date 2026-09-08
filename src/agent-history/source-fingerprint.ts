import { stat } from "node:fs/promises";
import type { AgentHistorySourceFingerprint } from "@/observability/contracts.js";

/**
 * Computes a fingerprint for an agent history file and any associated SQLite sidecars (-wal, -shm).
 * Missing sidecars are treated as mtimeMs=0, size=0 (fail-open), matching legacy single-file values.
 */
export async function statSourceFingerprint(
  path: string,
): Promise<AgentHistorySourceFingerprint | null> {
  const main = await stat(path).catch(() => null);
  if (!main) return null;

  const [wal, shm] = await Promise.all([
    stat(`${path}-wal`).catch(() => null),
    stat(`${path}-shm`).catch(() => null),
  ]);

  const walMtime = wal ? Math.trunc(wal.mtimeMs) : 0;
  const walSize = wal ? wal.size : 0;
  const shmMtime = shm ? Math.trunc(shm.mtimeMs) : 0;
  const shmSize = shm ? shm.size : 0;

  const mtimeMs = Math.trunc(main.mtimeMs) + walMtime * 3 + shmMtime * 7;
  const size = main.size + walSize + shmSize;

  return {
    mtimeMs,
    path,
    size,
  };
}
