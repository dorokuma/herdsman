import type { AgentIndexRecord } from "./contracts.js";

const DISPATCHED_PI_SESSION_PREFIX = "/tmp/pi-role-sessions/";

/** Returns true only for Pi agents used as interactive observers, not dispatched roles. */
export function isInteractivePiAgent(agent: AgentIndexRecord | undefined): boolean {
  return (
    agent?.agent === "pi" &&
    !agent.agentSession?.value.startsWith(DISPATCHED_PI_SESSION_PREFIX)
  );
}
