import { realpathSync } from "node:fs";
import { normalize } from "node:path";
import type { AgentIndexRecord } from "./contracts.js";

export const DISPATCHED_PI_SESSION_ROOT = "/tmp/herdr-role-sessions/";

/** Returns true only for pi-family (pi/pig) agents used as interactive observers, not dispatched roles. */
export function isInteractivePiAgent(agent: AgentIndexRecord | undefined): boolean {
  const kind = agent?.agent?.toLowerCase();
  if ((kind !== "pi" && kind !== "pig") || !agent?.agentSession?.value) return false;
  const raw = agent.agentSession.value;
  if (raw.includes("..")) return false;
  let resolved = normalize(raw);
  try {
    resolved = realpathSync(raw);
  } catch {
    // A not-yet-created session can still be classified by its normalized path.
  }
  return !resolved.startsWith(DISPATCHED_PI_SESSION_ROOT);
}
