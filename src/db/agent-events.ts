import type { DatabaseSync } from "node:sqlite";
import type {
  AgentEventRecord,
  AgentEventType,
  AgentIndexRecord,
  AgentQueryScope,
  CompactAgentHistory,
} from "@/observability/contracts.js";
import { isInteractivePiAgent } from "@/observability/interactive-pi.js";

/**
 * A delivered event is re-listed for the same owner terminal only within this
 * freshness window after its last delivery attempt. The window covers the
 * crash/redelivery path (an extension that missed the first delivery re-fetches
 * it right after reconnecting) while preventing an unacked delivered event from
 * being re-returned indefinitely on every orchestrator.get, which would feed
 * duplicate presentations whenever the extension's local dedup state is lost.
 */
export const REDELIVERY_FRESHNESS_MS = 300_000;

/**
 * Persist interval for the monotonic logical clock. The clock is written to
 * daemon_meta only when it advances by at least this much since the last
 * persist, so read-only queries do not trigger a write+fsync on every call.
 * The freshness window is 300s, so a 60s persist granularity does not weaken
 * the anti-rollback semantics.
 */
export const LOGICAL_CLOCK_PERSIST_INTERVAL_MS = 60_000;

export const LOGICAL_CLOCK_META_KEY = "logical_now_ms";

export function hasNonEmptyAssistantMessage(
  compactHistory: CompactAgentHistory | null | undefined,
): boolean {
  const text = compactHistory?.lastAssistantMessage?.text;
  return typeof text === "string" && text.trim().length > 0;
}

/** The single delivery predicate shared by pending discovery and acknowledgement. */
export function isDeliverableAgentEvent(
  event: AgentEventRecord,
  agent: AgentIndexRecord | undefined,
  scope: { herdrSessionName: string; workspaceId: string },
  ownerTerminalId: string,
): boolean {
  const failed = event.type === "agent.failed" || event.type === "agent.discarded";
  const isRetainedInvalidated = event.status === "invalidated" && event.deliverable === 1;

  // 1. 状态放行：允许 pending、delivered 以及关页保留的 invalidated 行
  const statusAllowed =
    event.status === "pending" || event.status === "delivered" || isRetainedInvalidated;
  if (!statusAllowed) return false;

  // 2. 基础范围过滤
  if (
    event.workspaceId !== scope.workspaceId ||
    event.herdrSessionName !== scope.herdrSessionName ||
    event.terminalId === null ||
    event.terminalId === ownerTerminalId ||
    event.type === "agent.status.changed" ||
    (event.type === "agent.idle" && asRecord(event.payload).from !== "working")
  ) {
    return false;
  }

  // 3. 投递重试窗口守卫
  if (
    event.status !== "delivered" &&
    event.nextAttemptAt !== null &&
    event.nextAttemptAt !== undefined &&
    event.nextAttemptAt > new Date()
  ) {
    return false;
  }

  // 4. 终端归属校验
  if (event.status === "delivered" && event.deliveredToTerminalId !== ownerTerminalId) {
    return false;
  }

  // 5. 核心：解耦对 live agent 表记录的依赖
  // 当 pane 被关闭，retirePane 物理清除了 agents 表记录，此时 agent === undefined。
  // 若该事件已被标记为保留作废态 (isRetainedInvalidated) 或 failed，直接从事件自身元数据验证有效性
  if (failed || isRetainedInvalidated) {
    const payload = asRecord(event.payload);
    const agentKind = typeof payload.agent === "string" ? payload.agent : "pi";
    if (
      agentKind !== "pi" &&
      (event.type === "agent.idle" || event.type === "agent.done") &&
      !hasNonEmptyAssistantMessage(event.compactHistory)
    ) {
      return false;
    }
    return true;
  }

  // 6. 正常存活 pane 维持原有的严密代际与所属校验
  if (agent === undefined) return false;
  return (
    !(isInteractivePiAgent(agent) && event.type === "agent.idle") &&
    !(
      agent.agent !== "pi" &&
      (event.type === "agent.idle" || event.type === "agent.done") &&
      !hasNonEmptyAssistantMessage(event.compactHistory)
    ) &&
    agent.paneId === event.paneId &&
    (event.paneGeneration === null || agent.paneGeneration === event.paneGeneration) &&
    agent.workspaceId === scope.workspaceId
  );
}

function asRecord(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

type AgentEventRow = {
  agent_id: string | null;
  compact_history_json: string | null;
  created_at: number;
  herdr_session_name: string;
  id: number;
  idempotency_key: string | null;
  pane_id: string | null;
  pane_generation: string | null;
  payload_json: string;
  delivery_attempts: number;
  last_attempt_at: number | null;
  next_attempt_at: number | null;
  last_failure_code: string | null;
  invalidated_reason: string | null;
  delivered_to_terminal_id: string | null;
  status: "pending" | "delivered" | "acked" | "invalidated" | "failed";
  deliverable: 0 | 1;
  terminal_id: string | null;
  type: AgentEventType;
  workspace_id: string | null;
};

export class AgentEventStore {
  readonly #sqlite: DatabaseSync;
  #lastPersistedLogicalNowMs: number;
  #logicalNowMs: number;

  constructor(sqlite: DatabaseSync) {
    this.#sqlite = sqlite;
    const row = this.#sqlite
      .prepare("select value from daemon_meta where key = ?")
      .get(LOGICAL_CLOCK_META_KEY) as { value: string } | undefined;
    const persisted = row ? Number(row.value) : 0;
    this.#logicalNowMs = Number.isFinite(persisted) && persisted > 0 ? persisted : 0;
    this.#lastPersistedLogicalNowMs = this.#logicalNowMs;
  }

  /**
   * Monotonic logical clock: never goes backwards, even when the wall clock
   * is rolled back. The in-memory value is seeded from daemon_meta on
   * construction and advanced to the wall clock when the wall clock is ahead.
   * Persistence is throttled so read-only queries do not write+fsync every
   * call; on crash the persisted value is only ever stale (never ahead of the
   * last persisted value), so the anti-rollback invariant holds across
   * restarts.
   */
  #now(): number {
    const wallClock = Date.now();
    if (wallClock <= this.#logicalNowMs) return this.#logicalNowMs;
    this.#logicalNowMs = wallClock;
    if (this.#logicalNowMs - this.#lastPersistedLogicalNowMs >= LOGICAL_CLOCK_PERSIST_INTERVAL_MS) {
      this.#lastPersistedLogicalNowMs = this.#logicalNowMs;
      this.#sqlite
        .prepare(
          `insert into daemon_meta (key, value) values (?, ?)
           on conflict(key) do update set value = excluded.value`,
        )
        .run(LOGICAL_CLOCK_META_KEY, String(this.#logicalNowMs));
    }
    return this.#logicalNowMs;
  }

  append(input: {
    agentId?: string | null;
    compactHistory?: CompactAgentHistory | null;
    herdrSessionName: string;
    idempotencyKey?: string | null;
    paneId?: string | null;
    paneGeneration?: string | null;
    payload: unknown;
    terminalId?: string | null;
    type: AgentEventType;
    workspaceId?: string | null;
  }): AgentEventRecord {
    const existing = input.idempotencyKey
      ? (this.#sqlite
          .prepare(
            "select * from agent_events where herdr_session_name = ? and idempotency_key = ?",
          )
          .get(input.herdrSessionName, input.idempotencyKey) as AgentEventRow | undefined)
      : undefined;
    if (existing) return mapAgentEvent(existing);

    const agentId = this.#resolveAppendAgentId(input.agentId ?? null);
    const result = this.#sqlite
      .prepare(
        `insert into agent_events
         (herdr_session_name, agent_id, pane_id, pane_generation, workspace_id, terminal_id, type, payload_json, compact_history_json, idempotency_key, deliverable, status, delivery_attempts, created_at)
         values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'pending', 0, ?)`,
      )
      .run(
        input.herdrSessionName,
        agentId,
        input.paneId ?? null,
        input.paneGeneration ?? null,
        input.workspaceId ?? null,
        input.terminalId ?? null,
        input.type,
        JSON.stringify(input.payload),
        input.compactHistory ? JSON.stringify(input.compactHistory) : null,
        input.idempotencyKey ?? null,
        this.#now(),
      );
    return this.get(Number(result.lastInsertRowid));
  }

  /**
   * Defence in depth for the terminal-event write path. A status plan can reach
   * its terminal state after the pane was retired and its `agents` row was
   * physically deleted; the foreign key would then reject the insert
   * (`FOREIGN KEY constraint failed`) and the terminal outcome would be lost
   * before it ever reached the store. Instead of throwing, the row is written as
   * an orphan (`agent_id = null`) and logged, so the degradation is explicit
   * rather than silent.
   */
  #resolveAppendAgentId(agentId: string | null): string | null {
    if (agentId === null) return null;
    const row = this.#sqlite.prepare("select 1 from agents where id = ? limit 1").get(agentId);
    if (row) return agentId;
    console.warn("Herdsman downgrading agent event to orphan because the agents row is gone", {
      agentId,
    });
    return null;
  }

  invalidatePane(input: {
    herdrSessionName: string;
    paneId: string;
    paneGeneration?: string | null;
    invalidatedReason?: string;
  }): void {
    this.#transaction(() => this.#invalidatePaneCore(input));
  }

  /**
   * Direct invalidatePane without wrapping in a transaction.
   *
   * ONLY call this when the caller already holds an SQLite transaction
   * (for example from AgentStore.replaceForSession). Calling this outside
   * an explicit transaction will leave the two-step invalidation non-atomic
   * with respect to other concurrent writers.
   */
  invalidatePaneDirect(input: {
    herdrSessionName: string;
    paneId: string;
    paneGeneration?: string | null;
    invalidatedReason?: string;
  }): void {
    this.#invalidatePaneCore(input);
  }

  #invalidatePaneCore(input: {
    herdrSessionName: string;
    paneId: string;
    paneGeneration?: string | null;
    invalidatedReason?: string;
  }): void {
    const legacy = input.paneGeneration == null;
    const now = this.#now();
    const defaultReason =
      input.invalidatedReason ?? (legacy ? "LEGACY_CLOSE_WITHOUT_GENERATION" : "PANE_CLOSED");
    // 步骤 1：全量作废该 pane 当前所有活跃行，默认置为 deliverable = 0
    // 当指定了明确 paneGeneration 时，只作废该代及更早代（<=）的遗留行，避免误杀新代事件。
    const legacyOrMatched = legacy
      ? "1 = 1"
      : "(pane_generation = ? or pane_generation is null or pane_generation < ?)";
    const legacyOrMatchedParams = legacy
      ? []
      : [input.paneGeneration ?? null, input.paneGeneration ?? null];
    this.#sqlite
      .prepare(
        `update agent_events
             set deliverable = 0, status = 'invalidated', invalidated_reason = ?
           where herdr_session_name = ? and pane_id = ? and status not in ('acked', 'invalidated', 'failed')
             and ${legacyOrMatched}`,
      )
      .run(defaultReason, input.herdrSessionName, input.paneId, ...legacyOrMatchedParams);
    // 步骤 2：对满足保留条件的完成类事件恢复 deliverable = 1
    // 关页保留只保到「首次投递尝试」为止：done/idle 沿用 300s 新鲜度窗口，
    // agent.failed 则以 delivery_attempts = 0（尚未投递过）为界——失败结果常常
    // 是在 pane/agents 行已经消失之后才写出来的，必须有一轮投递机会。
    // agent.discarded 不保留。
    const genCondition = legacy ? "1 = 1" : "pane_generation = ?";
    const genParams = legacy ? [] : [input.paneGeneration ?? null];
    this.#sqlite
      .prepare(
        `update agent_events
              set deliverable = 1, invalidated_reason = 'RETAINED_OUTCOME_PANE_CLOSED'
            where herdr_session_name = ? and pane_id = ? and status = 'invalidated' and deliverable = 0 and invalidated_reason = ?
              and ((${genCondition})
                 and ((type = 'agent.done' and json_extract(payload_json, '$.from') = 'working')
                   or (type = 'agent.idle' and json_extract(payload_json, '$.from') = 'working'))
                 and created_at >= ?
                or (type = 'agent.failed' and delivery_attempts = 0))`,
      )
      .run(
        input.herdrSessionName,
        input.paneId,
        defaultReason,
        ...genParams,
        now - REDELIVERY_FRESHNESS_MS,
      );
  }

  listReconcileCandidates(limit = 100, afterId = 0): AgentEventRecord[] {
    const rows = this.#sqlite
      .prepare(
        `select * from agent_events where id > ? and status in ('pending', 'delivered') order by id asc limit ?`,
      )
      .all(afterId, limit) as AgentEventRow[];
    return rows.map(mapAgentEvent);
  }

  markRetainedInvalidated(id: number): boolean {
    return (
      Number(
        this.#sqlite
          .prepare(
            `update agent_events
                set status = 'invalidated', deliverable = 1, invalidated_reason = 'RETAINED_OUTCOME_PANE_CLOSED'
              where id = ? and status in ('pending', 'delivered')`,
          )
          .run(id).changes,
      ) > 0
    );
  }

  deleteReconcileCandidate(id: number): boolean {
    return (
      Number(
        this.#sqlite
          .prepare("delete from agent_events where id = ? and status in ('pending', 'delivered')")
          .run(id).changes,
      ) > 0
    );
  }

  /**
   * Physically removes invalidated events older than the given age, measured from
   * coalesce(last_attempt_at, created_at) so a delivered-then-invalidated event is
   * aged from its last delivery. A short grace window lets the owner that already
   * received the event ack it and advance the cursor.
   */
  deleteInvalidatedOlderThan(ageMs: number): number {
    return Number(
      this.#sqlite
        .prepare(
          `delete from agent_events
           where status = 'invalidated' and deliverable = 0 and coalesce(last_attempt_at, created_at) < ?`,
        )
        .run(Date.now() - ageMs).changes,
    );
  }

  /**
   * Physically removes terminal events (acked/failed) older than the given age.
   * These statuses are never redelivered or reconciled, so a periodic TTL bounds
   * the table's growth without touching pending/delivered/invalidated rows.
   */
  deleteSettledOlderThan(ageMs: number): number {
    return Number(
      this.#sqlite
        .prepare(
          `delete from agent_events where (status in ('acked', 'failed') or (status = 'invalidated' and deliverable = 1)) and created_at < ?`,
        )
        .run(Date.now() - ageMs).changes,
    );
  }

  ackSelfOwned(input: { herdrSessionName: string; workspaceId: string; paneId: string }): number {
    return Number(
      this.#sqlite
        .prepare(
          `update agent_events set status = 'acked', deliverable = 0
           where herdr_session_name = ? and workspace_id = ? and pane_id = ?
             and status in ('pending', 'delivered')`,
        )
        .run(input.herdrSessionName, input.workspaceId, input.paneId).changes,
    );
  }

  invalidateById(id: number, reason: string): boolean {
    return (
      Number(
        this.#sqlite
          .prepare(
            `update agent_events set deliverable = 0, status = 'invalidated', invalidated_reason = ? where id = ? and status in ('pending', 'delivered')`,
          )
          .run(reason, id).changes,
      ) > 0
    );
  }
  latestStatusTransition(agentId: string, herdrSessionName: string): AgentEventRecord | undefined {
    const row = this.#sqlite
      .prepare(
        `select * from agent_events
         where agent_id = ? and herdr_session_name = ? and type = 'agent.status.changed'
         order by id desc limit 1`,
      )
      .get(agentId, herdrSessionName) as AgentEventRow | undefined;
    return row ? mapAgentEvent(row) : undefined;
  }

  hasTerminalEventAfter(agentId: string, herdrSessionName: string, afterId: number): boolean {
    const row = this.#sqlite
      .prepare(
        `select 1 from agent_events
         where agent_id = ? and herdr_session_name = ? and id > ?
           and type in ('agent.idle', 'agent.done', 'agent.blocked')
           and status != 'invalidated'
         limit 1`,
      )
      .get(agentId, herdrSessionName, afterId);
    return Boolean(row);
  }

  latestTerminalEvent(agentId: string, herdrSessionName: string): AgentEventRecord | undefined {
    const row = this.#sqlite
      .prepare(
        `select * from agent_events
         where agent_id = ? and herdr_session_name = ?
           and type in ('agent.idle', 'agent.done', 'agent.blocked')
           and status != 'invalidated'
         order by id desc limit 1`,
      )
      .get(agentId, herdrSessionName) as AgentEventRow | undefined;
    return row ? mapAgentEvent(row) : undefined;
  }

  /**
   * Latest event that represents a *completed turn* whose assistant ref may be
   * treated as "already delivered": every `agent.done`, plus `agent.idle` rows
   * that were emitted from `working`. Rows that must never consume the
   * assistant ref of a round are excluded:
   *
   * - `agent.status.changed` (never a delivery), `agent.discarded`,
   * - `agent.idle` rows whose payload `from` is not `working` (startup/unknown
   *   idles restored before indexing),
   * - rows whose assistant message is empty.
   *
   * A late `unknown -> idle` plan that finishes after the round already ended
   * would otherwise register the final ref as an `agent.idle` terminal event and
   * make the following `working -> done` plan look like a duplicate.
   */
  latestCompletedTurnEvent(
    agentId: string,
    herdrSessionName: string,
  ): AgentEventRecord | undefined {
    const row = this.#sqlite
      .prepare(
        `select * from agent_events
         where agent_id = ? and herdr_session_name = ?
           and (type = 'agent.done'
                or (type = 'agent.idle' and json_extract(payload_json, '$.from') = 'working'))
           and length(trim(coalesce(json_extract(compact_history_json, '$.lastAssistantMessage.text'), ''))) > 0
         order by id desc limit 1`,
      )
      .get(agentId, herdrSessionName) as AgentEventRow | undefined;
    return row ? mapAgentEvent(row) : undefined;
  }

  listAfter(
    input: AgentQueryScope & { afterEventId?: number; limit?: number; ownerTerminalId?: string },
  ): AgentEventRecord[] {
    const clauses = [
      "id > ?",
      "((status = 'pending' and (next_attempt_at is null or next_attempt_at <= ?)) or (status = 'delivered' and delivered_to_terminal_id = ? and id > ? and last_attempt_at >= ?) or (status = 'invalidated' and deliverable = 1 and (next_attempt_at is null or next_attempt_at <= ?)))",
    ];
    const params: Array<number | string | null> = [
      input.afterEventId ?? 0,
      this.#now(),
      input.ownerTerminalId ?? null,
      input.afterEventId ?? 0,
      this.#now() - REDELIVERY_FRESHNESS_MS,
      this.#now(),
    ];
    if (input.herdrSessionName) {
      clauses.push("herdr_session_name = ?");
      params.push(input.herdrSessionName);
    }
    if (input.workspaceId) {
      clauses.push("workspace_id = ?");
      params.push(input.workspaceId);
    }
    const limit = input.limit ?? 100;
    const rows = this.#sqlite
      .prepare(`select * from agent_events where ${clauses.join(" and ")} order by id asc limit ?`)
      .all(...params, limit) as AgentEventRow[];
    return rows.map(mapAgentEvent);
  }

  nextDeliverableAfter(input: {
    afterEventId: number;
    herdrSessionName: string;
    ownerTerminalId: string;
    workspaceId: string;
    getAgent?: (agentId: string) => AgentIndexRecord | undefined;
  }): AgentEventRecord | undefined {
    const scope = { herdrSessionName: input.herdrSessionName, workspaceId: input.workspaceId };
    // Retained rows (status='invalidated' and deliverable=1) outlive the live
    // `agents` row because pane close physically clears it, so they must never
    // be filtered by the existence guard. Orphan terminal rows (`agent.failed` /
    // `agent.discarded` written after the agents row vanished) have the same
    // problem: they carry `agent_id = null` and still have to be visible here,
    // because otherwise the cursor ack (`markAcked(id <= cursor)`) would silently
    // swallow an event that was never delivered. Every other row keeps the
    // original "the agent is still indexed" check.
    const orphanTerminalFilter = `or type in ('agent.failed', 'agent.discarded')`;
    const agentFilter = input.getAgent
      ? ""
      : `and (
             (status = 'invalidated' and deliverable = 1)
             ${orphanTerminalFilter}
             or exists (
               select 1 from agents
               where agents.id = agent_events.agent_id
                 and agents.herdr_session_name = agent_events.herdr_session_name
                 and agents.workspace_id = agent_events.workspace_id
                 and agents.pane_id = agent_events.pane_id
             )
           )`;
    let afterEventId = input.afterEventId;
    for (let page = 0; page < 50; page += 1) {
      const params = [
        afterEventId,
        this.#now(),
        input.ownerTerminalId,
        this.#now() - REDELIVERY_FRESHNESS_MS,
        this.#now(),
        input.herdrSessionName,
        input.workspaceId,
        input.ownerTerminalId,
      ];
      const sql = `select * from agent_events
           where id > ? and ((status = 'pending' and (next_attempt_at is null or next_attempt_at <= ?)) or (status = 'delivered' and delivered_to_terminal_id = ? and last_attempt_at >= ?) or (status = 'invalidated' and deliverable = 1 and (next_attempt_at is null or next_attempt_at <= ?))) and herdr_session_name = ? and workspace_id = ?
             and terminal_id is not null and terminal_id != ?
             and (agent_id is not null or (status = 'invalidated' and deliverable = 1) ${orphanTerminalFilter})
             ${agentFilter}
           order by id asc limit 1000`;
      const rows = this.#sqlite.prepare(sql).all(...params) as AgentEventRow[];
      if (rows.length === 0) return undefined;
      for (const row of rows) {
        const event = mapAgentEvent(row);
        const agentId = event.agentId;
        // Pane close physically removes the agents row and the ON DELETE SET NULL
        // foreign key then clears agent_id on the retained event, so retained rows
        // are evaluated from their own metadata instead of the live agent record.
        // Orphan terminal rows follow the same rule (their own payload/metadata is
        // the only remaining source of truth).
        const retained = event.status === "invalidated" && event.deliverable === 1;
        const orphanTerminal = event.type === "agent.failed" || event.type === "agent.discarded";
        if (agentId === null && !retained && !orphanTerminal) continue;
        if (
          !input.getAgent ||
          isDeliverableAgentEvent(
            event,
            agentId === null ? undefined : input.getAgent(agentId),
            scope,
            input.ownerTerminalId,
          )
        )
          return event;
      }
      afterEventId = rows.at(-1)?.id ?? afterEventId;
    }
    console.warn("Herdsman stopped scanning pending agent events after 50 pages", scope);
    return undefined;
  }

  reservePending(terminalId: string, limit = 100, ids?: number[]): AgentEventRecord[] {
    return this.#transaction(() => {
      const now = this.#now();
      const idClause = ids && ids.length > 0 ? `and id in (${ids.map(() => "?").join(",")})` : "";
      const params: Array<number | string> = [now, terminalId, now - REDELIVERY_FRESHNESS_MS, now];
      if (ids && ids.length > 0) params.push(...ids);
      params.push(limit);
      const rows = this.#sqlite
        .prepare(
          `select * from agent_events where ((status = 'pending' and (next_attempt_at is null or next_attempt_at <= ?)) or (status = 'delivered' and delivered_to_terminal_id = ? and last_attempt_at >= ?) or (status = 'invalidated' and deliverable = 1 and (next_attempt_at is null or next_attempt_at <= ?))) ${idClause} order by id asc limit ?`,
        )
        .all(...params) as AgentEventRow[];
      for (const row of rows) {
        this.#sqlite
          .prepare(
            `update agent_events set status = 'delivered', deliverable = 1, delivery_attempts = ?, last_attempt_at = ?, delivered_to_terminal_id = ? where id = ? and (status = 'pending' or (status = 'invalidated' and deliverable = 1))`,
          )
          .run(row.delivery_attempts + 1, now, terminalId, row.id);
      }
      if (rows.length > 0) {
        console.info("Herdsman agent event delivery reserved", {
          firstEventId: rows[0]?.id,
          lastEventId: rows.at(-1)?.id,
          count: rows.length,
          terminal: terminalId,
        });
      }
      return rows.map((row) => this.get(row.id));
    });
  }

  reclaimDelivered(timeoutMs: number): number {
    return this.#transaction(() => {
      const cutoff = this.#now() - timeoutMs;
      // The delivered event's own agent row (matched by pane) is the pane-open
      // guard: it decides whether branch (a) and branch (b) apply.
      const agentPaneOpen = `exists (
        select 1 from agents
        where agents.id = agent_events.agent_id
          and agents.herdr_session_name = agent_events.herdr_session_name
          and agents.workspace_id = agent_events.workspace_id
          and agents.pane_id = agent_events.pane_id
      )`;
      // Branch (b): the agent pane is closed/retired, so the delivered event is
      // invalidated instead of being reclaimed and redelivered. This is also the
      // point where the "retained until the first delivery attempt" window of a
      // terminal `agent.failed` row ends and the ordinary invalidated + 1h
      // collection takes over. `reservePending` is the only writer that sets
      // status = 'delivered' and it always increments delivery_attempts, so the
      // explicit `delivery_attempts >= 1` guard states the invariant that no row
      // may leave the retained window before it was handed to an owner once.
      const invalidated = this.#sqlite
        .prepare(
          `update agent_events
           set deliverable = 0, status = 'invalidated', invalidated_reason = 'PANE_CLOSED_RECLAIM'
           where status = 'delivered' and delivery_attempts >= 1 and last_attempt_at < ? and not ${agentPaneOpen}`,
        )
        .run(cutoff).changes;
      // Branch (c): the pane is still open but the delivery terminal no longer
      // holds the orchestrator scope (owner_terminal_id != delivered_to_terminal_id
      // or no owner), so reclaim immediately without waiting for last_attempt_at
      // timeout. If the scope is still owned by the delivery terminal (branch a),
      // the event remains in-flight with the owner.
      const reclaimWhere = `status = 'delivered'
             and ${agentPaneOpen}
             and not exists (
               select 1 from agent_orchestrator_scopes
               where agent_orchestrator_scopes.herdr_session_name = agent_events.herdr_session_name
                 and agent_orchestrator_scopes.workspace_id = agent_events.workspace_id
                 and agent_orchestrator_scopes.owner_terminal_id = agent_events.delivered_to_terminal_id
             )`;
      const exceeded = this.#sqlite
        .prepare(
          `select id, agent_id, herdr_session_name, workspace_id, delivery_attempts
           from agent_events
           where ${reclaimWhere} and delivery_attempts >= 10`,
        )
        .all() as Array<{
        agent_id: string | null;
        delivery_attempts: number;
        herdr_session_name: string;
        id: number;
        workspace_id: string | null;
      }>;
      const reclaimed = this.#sqlite
        .prepare(
          `update agent_events
           set status = case when delivery_attempts >= 10 then 'failed' else 'pending' end,
               deliverable = case when delivery_attempts >= 10 then 0 else 1 end,
               last_failure_code = case when delivery_attempts >= 10 then 'DELIVERY_ATTEMPTS_EXCEEDED' else last_failure_code end,
               delivered_to_terminal_id = null,
               next_attempt_at = null
           where ${reclaimWhere}`,
        )
        .run().changes;
      for (const row of exceeded) {
        console.error("Herdsman agent event delivery attempts exceeded", {
          eventId: row.id,
          agentId: row.agent_id,
          herdrSessionName: row.herdr_session_name,
          workspaceId: row.workspace_id,
          deliveryAttempts: row.delivery_attempts,
          lastFailureCode: "DELIVERY_ATTEMPTS_EXCEEDED",
        });
      }
      return Number(invalidated) + Number(reclaimed);
    });
  }

  markAcked(id: number, scope?: { herdrSessionName: string; workspaceId: string }): void {
    if (scope) {
      this.#sqlite
        .prepare(
          `update agent_events set status = 'acked', deliverable = 0
           where herdr_session_name = ? and workspace_id = ? and id <= ? and (status in ('pending', 'delivered') or (status = 'invalidated' and deliverable = 1))`,
        )
        .run(scope.herdrSessionName, scope.workspaceId, id);
      return;
    }
    this.#sqlite
      .prepare(
        `update agent_events set status = 'acked', deliverable = 0 where id = ? and (status in ('pending', 'delivered') or (status = 'invalidated' and deliverable = 1))`,
      )
      .run(id);
  }

  #transaction<T>(operation: () => T): T {
    this.#sqlite.exec("begin immediate");
    try {
      const result = operation();
      this.#sqlite.exec("commit");
      return result;
    } catch (error) {
      this.#sqlite.exec("rollback");
      throw error;
    }
  }
  latestEventId(scope: AgentQueryScope = {}): number {
    const clauses: string[] = [];
    const params: Array<number | string | null> = [];
    if (scope.herdrSessionName) {
      clauses.push("herdr_session_name = ?");
      params.push(scope.herdrSessionName);
    }
    if (scope.workspaceId) {
      clauses.push("workspace_id = ?");
      params.push(scope.workspaceId);
    }
    const where = clauses.length > 0 ? ` where ${clauses.join(" and ")}` : "";
    const row = this.#sqlite
      .prepare(`select max(id) as id from agent_events${where}`)
      .get(...params) as { id: number | null } | undefined;
    return row?.id ?? 0;
  }

  get(id: number): AgentEventRecord {
    const row = this.#sqlite.prepare("select * from agent_events where id = ?").get(id) as
      | AgentEventRow
      | undefined;
    if (!row) throw new Error(`Agent event not found: ${id}`);
    return mapAgentEvent(row);
  }
}

export function mapAgentEvent(row: AgentEventRow): AgentEventRecord {
  return {
    agentId: row.agent_id,
    compactHistory: parseJson<CompactAgentHistory>(row.compact_history_json),
    createdAt: new Date(row.created_at),
    herdrSessionName: row.herdr_session_name,
    id: row.id,
    paneId: row.pane_id,
    paneGeneration: row.pane_generation,
    deliverable: row.deliverable,
    status: row.status,
    deliveryAttempts: row.delivery_attempts,
    lastAttemptAt: row.last_attempt_at === null ? null : new Date(row.last_attempt_at),
    nextAttemptAt: row.next_attempt_at === null ? null : new Date(row.next_attempt_at),
    lastFailureCode: row.last_failure_code,
    invalidatedReason: row.invalidated_reason,
    deliveredToTerminalId: row.delivered_to_terminal_id,
    payload: parseJson<unknown>(row.payload_json) ?? {},
    terminalId: row.terminal_id,
    type: row.type,
    workspaceId: row.workspace_id,
  };
}

function parseJson<T>(value: string | null): T | null {
  if (!value) return null;
  try {
    return JSON.parse(value) as T;
  } catch {
    return null;
  }
}
