/**
 * READ-ONLY SNAPSHOT of the outcome projection shipped in the *published*
 * `@dorokuma/herdsman-pi@0.15.0` package — i.e. what an orchestrator that has not
 * upgraded the pi extension actually runs.
 *
 * Provenance (reproduce with):
 *   npm pack @dorokuma/herdsman-pi@0.15.0
 *   tar xzf dorokuma-herdsman-pi-0.15.0.tgz
 *   # package/src/wake.ts, sha256 3d63716723cd05920ef080de481fbcab894e93801d8fb0d1a0a036d7ee4059a3
 *
 * The function bodies below are verbatim copies of that release's
 * `outcomeKind` and the two helpers it uses. Only the type imports were
 * re-pointed at the workspace source so the fixture type-checks; no logic was
 * edited. This exists so the backward-compatibility assertions in
 * `test/unit/herdsman-pi-wake.test.ts` run against real published behaviour
 * instead of a hand-written "what the old plugin probably did" lambda, which
 * proved nothing about the released artifact.
 *
 * Refresh rule: when a newer `@dorokuma/herdsman-pi` is released, re-extract and
 * update this file together with its sha256, or the compatibility claim silently
 * describes a version nobody installs.
 */

import type { AgentEventWireRecord } from "../../packages/herdsman-pi/src/daemon-client.js";
import type { AgentOutcome } from "../../packages/herdsman-pi/src/wake.js";

export const PLAN_WAITING_HISTORY = "PLAN_WAITING_HISTORY";

function asRecord(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}
function stringValue(value: unknown): string | undefined {
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

/** Verbatim `outcomeKind` from `@dorokuma/herdsman-pi@0.15.0` `src/wake.ts`. */
export function publishedOutcomeKind(
  event: AgentEventWireRecord,
): AgentOutcome["kind"] | undefined {
  if (!event.terminalId) return undefined;
  if (event.type === "agent.done") return "completed";
  if (event.type === "agent.blocked") return "blocked";
  if (event.type === "agent.failed") {
    const payload = asRecord(event.payload);
    const reason = stringValue(payload.reason);
    // Backward compatibility filter for legacy pre-upgrade failed rows with PLAN_WAITING_HISTORY
    // and degraded retries that exceeded the bounded retry budget.
    if (reason === PLAN_WAITING_HISTORY || reason === "degraded") {
      if (payload.fallbackOutcome === true) {
        return "failed";
      }
      return undefined;
    }
    return "failed";
  }
  if (event.type === "agent.discarded") {
    const payload = asRecord(event.payload);
    const agent = stringValue(payload.agent);
    const isAgyOrEmpty = agent === undefined || agent === "agy";
    if (isAgyOrEmpty) {
      const reason = stringValue(payload.reason);
      if (reason === PLAN_WAITING_HISTORY || reason === "degraded") {
        if (payload.fallbackOutcome === true) {
          return "failed";
        }
        return undefined;
      }
    }
    return "failed";
  }
  const payload = asRecord(event.payload);
  if (event.type === "agent.idle" && payload.from === "working") return "completed";
  return undefined;
}
