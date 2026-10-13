export type AgentStatus = "blocked" | "done" | "idle" | "unknown" | "working";

export type HerdrSessionRecord = {
  lastScannedAt: Date | null;
  name: string;
  running: boolean;
  sessionDir: string;
  socketPath: string;
  updatedAt: Date;
};

export type HerdrWorkspaceRecord = {
  agentStatus: AgentStatus;
  focused: boolean;
  herdrSessionName: string;
  label: string | null;
  lastSeenAt: Date;
  workspaceId: string;
};

export type AgentSessionRef = {
  agent: string;
  kind: "id" | "path";
  source: string;
  value: string;
};

export type AgentIndexRecord = {
  agent: string | null;
  agentSession: AgentSessionRef | null;
  agentStatus: AgentStatus;
  cwd: string | null;
  firstSeenAt: Date;
  focused: boolean;
  foregroundCwd: string | null;
  herdrSessionName: string;
  id: string;
  lastSeenAt: Date;
  name: string | null;
  paneId: string;
  terminalTitle?: string | null;
  paneRevision: number | null;
  paneGeneration?: string | null;
  tabId: string | null;
  terminalId: string | null;
  workspaceId: string;
};

export type AgentHistoryRef = {
  kind: "agent_session" | "discovered_file";
  path?: string;
  source: "antigravity-sqlite" | "pi-jsonl" | "unknown";
  value: string;
};

export type AgentHistoryExcerpt = {
  ref: string;
  stopReason?: string;
  text: string;
  timestamp: string | null;
};

export type CompactToolResult = {
  compaction: {
    mode:
      | "failure_focus"
      | "grouped_matches"
      | "structured_summary"
      | "truncated_passthrough"
      | "web_sources"
      | "unknown";
    originalChars: number;
    returnedChars: number;
  };
  isError: boolean;
  ref: string;
  text: string;
  toolName: string;
};

export type CompactAgentHistory = {
  historyRef: AgentHistoryRef | null;
  lastAssistantMessage: AgentHistoryExcerpt | null;
  lastToolResult: CompactToolResult | null;
  lastUserMessage: AgentHistoryExcerpt | null;
  messageCount: number;
  source: string | null;
  updatedAt: string | null;
};

export type AgentHistorySourceFingerprint = {
  mtimeMs: number;
  path: string;
  size: number;
};

export type AgentContextSnapshotRecord = {
  agentId: string;
  compactHistory: CompactAgentHistory;
  historyRef: AgentHistoryRef | null;
  paneRevision: number | null;
  sourceFingerprint: AgentHistorySourceFingerprint | null;
  updatedAt: Date;
};

export type AgentHistoryMessage = {
  compact?: CompactToolResult;
  ref: string;
  role: "assistant" | "tool_result" | "user";
  stopReason?: string;
  text: string;
  timestamp: string | null;
  toolName?: string;
};

export type AgentListItem = AgentIndexRecord & {
  history: Pick<
    CompactAgentHistory,
    "lastAssistantMessage" | "lastUserMessage" | "source" | "updatedAt"
  >;
};

export type AgentGetResult = AgentIndexRecord & {
  history: CompactAgentHistory;
};

export type AgentReadResult = AgentIndexRecord & {
  historyRef: AgentHistoryRef | null;
  messages: AgentHistoryMessage[];
};

export type AgentEventType =
  | "agent.blocked"
  | "agent.discarded"
  | "agent.done"
  | "agent.failed"
  | "agent.idle"
  | "agent.status.changed"
  | "agent.tool.failed";

export type AgentEventRecord = {
  agentId: string | null;
  compactHistory: CompactAgentHistory | null;
  createdAt: Date;
  herdrSessionName: string;
  id: number;
  paneId: string | null;
  paneGeneration?: string | null;
  deliverable: 0 | 1;
  status?: "pending" | "delivered" | "acked" | "invalidated" | "failed";
  deliveryAttempts?: number;
  lastAttemptAt?: Date | null;
  nextAttemptAt?: Date | null;
  lastFailureCode?: string | null;
  invalidatedReason?: string | null;
  deliveredToTerminalId?: string | null;
  payload: unknown;
  terminalId: string | null;
  type: AgentEventType;
  workspaceId: string | null;
};

export type AgentScope = {
  herdrSessionName: string;
  workspaceId: string;
};

export type AgentWorkspaceContextSnapshot = AgentScope & {
  agents: AgentListItem[];
  updatedAt: string;
};

export type AgentOrchestratorOwner = {
  paneId: string;
  terminalId: string;
};

export type AgentOrchestratorState = AgentScope & {
  ackedEventId: number;
  owner: AgentOrchestratorOwner | null;
  updatedAt: Date;
};

export type AgentOrchestratorWireState = AgentScope & {
  ackedEventId: number;
  owner: AgentOrchestratorOwner | null;
  updatedAt: string;
};

export type AgentOrchestratorChangeReason =
  | "claimed"
  | "disconnected"
  | "moved"
  | "released"
  | "startup_timeout";

export type AgentOrchestratorChanged = {
  current: AgentOrchestratorWireState;
  previous: AgentOrchestratorWireState;
  reason: AgentOrchestratorChangeReason;
};

export type PiPresenceRegistration = {
  herdrSocketPath: string;
  paneId: string;
  sessionRef: AgentSessionRef;
  subscriberId: string;
  subscriberKind: "pi";
  workspaceId: string;
};

/**
 * Frozen metric-key contract for degraded releases and empty-turn alerts.
 *
 * Gate: `.agents/notes/20260930-terminal-event-delivery-open-items.md` A2 (the
 * degraded metric keys had to be frozen before Phase 2 built anything on them);
 * the contract itself and every value below are recorded in
 * `.agents/notes/20261013-phase2-delivery-semantics.md`.
 *
 * The contract surface is: the event message prefix, the `degradedReason`
 * values, the empty-turn alert reason, and the log dimension names
 * (`planId / agentId / attempts / degradedReason / herdrSessionName / paneId`).
 * The string values are the contract — operators and dashboards key on them.
 * Renaming a value or the prefix is a contract change and must be recorded in
 * the Phase 2 note and in the Phase 1 note's "遗留" section; adding a value is
 * an additive contract change and must be recorded the same way.
 *
 * `stale_baseline_duplicate` is part of the same key family: it has been emitted
 * since `.agents/notes/20261001-pi-confirmed-turn-frozen-baseline-empty-body.md`
 * (judgement three) and is folded into the contract here rather than left as a
 * bare literal.
 */
export const DEGRADED_EVENT_MESSAGE = "Herdsman emitted degraded status event";

export const DEGRADED_REASON = {
  /** The client's `expectedText` guess disagreed with the on-disk tail. */
  expectedTextMismatch: "expected_text_mismatch",
  /** History did not advance past the frozen plan baseline. */
  noAdvanceFromInput: "no_advance_from_input",
  /** The on-disk tail is not a terminal assistant message, or has no text. */
  nonTerminalAssistant: "non_terminal_assistant",
  /** The on-disk tail was already delivered as an earlier terminal row. */
  staleBaselineDuplicate: "stale_baseline_duplicate",
} as const;

export type DegradedReason = (typeof DEGRADED_REASON)[keyof typeof DEGRADED_REASON];

/**
 * Alert reason for a round that settles with no deliverable assistant text at
 * all (`upstream_empty_turn`). It shares the degraded log's field shape, but it
 * is not a degradation: nothing was degraded — the round simply carried nothing
 * to hand over. Emitted at most once per pane per empty streak (the suppression
 * reset lives with the append funnel in `AgentIndexService`).
 */
export const EMPTY_TURN_ALERT_REASON = "upstream_empty_turn";
export const EMPTY_TURN_ALERT_MESSAGE = "Herdsman observed an empty upstream turn";

/**
 * The synthetic plan-row error marker for a round whose deliverable body was
 * degraded away. It is written to `status_event_plans.last_error` (and handed
 * to `markRetry` as an `Error`) instead of a real failure message, and it is
 * part of the same key family as the reasons above: operators grep for it, and
 * Phase 2's "no assistant content ⇒ no `agent.failed`" rule keys off it.
 * Changing the value is a contract change under the same rule.
 */
export const DEGRADED_PLAN_ERROR = "degraded";

export type AgentTurnCompletedInput = {
  confirmed: boolean;
  expectedText?: string;
  herdrSessionName: string;
  paneId: string;
  terminalId: string;
  workspaceId: string;
};

export type AgentQueryScope = {
  all?: boolean;
  herdrSessionName?: string;
  workspaceId?: string;
};

export const VALID_AGENT_EVENT_TYPES = new Set<string>([
  "agent.status.changed",
  "agent.idle",
  "agent.done",
  "agent.blocked",
  "agent.failed",
  "agent.discarded",
]);

export function parseAgentStatus(value: unknown): AgentStatus {
  return value === "blocked" ||
    value === "done" ||
    value === "idle" ||
    value === "unknown" ||
    value === "working"
    ? value
    : "unknown";
}
