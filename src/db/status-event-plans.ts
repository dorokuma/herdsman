import type { DatabaseSync } from "node:sqlite";
import type { AgentStatus, CompactAgentHistory } from "@/observability/contracts.js";

export const STATUS_PLAN_MAX_ATTEMPTS = 8;

export type StatusEventPlanStatus =
  | "pending"
  | "running"
  | "completed"
  | "cancelled"
  | "failed"
  | "discarded";

export type StatusEventPlanRecord = {
  agentId: string;
  attempts: number;
  compactHistory?: CompactAgentHistory;
  createdAt: Date;
  fromStatus: AgentStatus;
  herdrEventKey: string | null;
  herdrSessionName: string;
  id: number;
  lastError: string | null;
  paneGeneration: string | null;
  paneId: string;
  status: StatusEventPlanStatus;
  toStatus: AgentStatus;
  updatedAt: Date;
};

type StatusEventPlanRow = {
  agent_id: string;
  attempts: number;
  compact_history_json: string | null;
  created_at: number;
  from_status: AgentStatus;
  herdr_event_key: string | null;
  herdr_session_name: string;
  id: number;
  last_error: string | null;
  pane_generation: string | null;
  pane_id: string;
  status: StatusEventPlanStatus;
  to_status: AgentStatus;
  updated_at: number;
};

function mapStatusEventPlan(row: StatusEventPlanRow): StatusEventPlanRecord {
  let compactHistory: CompactAgentHistory | undefined;
  if (row.compact_history_json) {
    try {
      compactHistory = JSON.parse(row.compact_history_json) as CompactAgentHistory;
    } catch {
      compactHistory = undefined;
    }
  }
  return {
    agentId: row.agent_id,
    attempts: row.attempts,
    ...(compactHistory ? { compactHistory } : {}),
    createdAt: new Date(row.created_at),
    fromStatus: row.from_status,
    herdrEventKey: row.herdr_event_key,
    herdrSessionName: row.herdr_session_name,
    id: row.id,
    lastError: row.last_error,
    paneGeneration: row.pane_generation,
    paneId: row.pane_id,
    status: row.status,
    toStatus: row.to_status,
    updatedAt: new Date(row.updated_at),
  };
}

export class StatusEventPlanStore {
  readonly #sqlite: DatabaseSync;

  constructor(sqlite: DatabaseSync) {
    this.#sqlite = sqlite;
  }

  insertPending(plan: {
    agentId: string;
    compactHistory?: CompactAgentHistory;
    fromStatus: AgentStatus;
    herdrEventKey?: string | null;
    herdrSessionName: string;
    paneGeneration?: string | null;
    paneId: string;
    toStatus: AgentStatus;
  }): StatusEventPlanRecord;
  insertPending(plan: {
    agent: { herdrSessionName: string; id: string; paneId: string; paneGeneration?: string | null };
    compactHistory?: CompactAgentHistory;
    from: AgentStatus;
    herdrEventKey?: string | null;
    to: AgentStatus;
  }): StatusEventPlanRecord;
  insertPending(plan: {
    agent?: {
      herdrSessionName: string;
      id: string;
      paneId: string;
      paneGeneration?: string | null;
    };
    agentId?: string;
    compactHistory?: CompactAgentHistory;
    from?: AgentStatus;
    fromStatus?: AgentStatus;
    herdrEventKey?: string | null;
    herdrSessionName?: string;
    paneGeneration?: string | null;
    paneId?: string;
    to?: AgentStatus;
    toStatus?: AgentStatus;
  }): StatusEventPlanRecord {
    const herdrSessionName = plan.herdrSessionName ?? plan.agent?.herdrSessionName;
    const agentId = plan.agentId ?? plan.agent?.id;
    const paneId = plan.paneId ?? plan.agent?.paneId;
    const paneGeneration = plan.paneGeneration ?? plan.agent?.paneGeneration ?? null;
    const fromStatus = plan.fromStatus ?? plan.from;
    const toStatus = plan.toStatus ?? plan.to;
    const herdrEventKey = plan.herdrEventKey ?? null;

    if (!herdrSessionName || !agentId || !paneId || !fromStatus || !toStatus) {
      throw new Error("Missing required fields for status event plan");
    }

    if (herdrEventKey) {
      const existing = this.#sqlite
        .prepare(
          "select * from status_event_plans where herdr_session_name = ? and herdr_event_key = ?",
        )
        .get(herdrSessionName, herdrEventKey) as StatusEventPlanRow | undefined;
      if (existing) {
        return mapStatusEventPlan(existing);
      }
    }

    const now = Date.now();
    const compactHistoryJson = plan.compactHistory ? JSON.stringify(plan.compactHistory) : null;

    let insertedId: number;
    try {
      const result = this.#sqlite
        .prepare(
          `insert into status_event_plans
           (agent_id, attempts, compact_history_json, created_at, from_status, herdr_event_key, herdr_session_name, last_error, pane_generation, pane_id, status, to_status, updated_at)
           values (?, 0, ?, ?, ?, ?, ?, null, ?, ?, 'pending', ?, ?)`,
        )
        .run(
          agentId,
          compactHistoryJson,
          now,
          fromStatus,
          herdrEventKey,
          herdrSessionName,
          paneGeneration,
          paneId,
          toStatus,
          now,
        );
      insertedId = Number(result.lastInsertRowid);
    } catch (error) {
      if (herdrEventKey) {
        const existing = this.#sqlite
          .prepare(
            "select * from status_event_plans where herdr_session_name = ? and herdr_event_key = ?",
          )
          .get(herdrSessionName, herdrEventKey) as StatusEventPlanRow | undefined;
        if (existing) return mapStatusEventPlan(existing);
      }
      throw error;
    }

    const row = this.get(insertedId);
    const superseded = this.cancelSupersededPendingPlans({
      agentId,
      herdrSessionName,
      keepId: row.id,
      toStatus,
    });
    if (superseded > 0) {
      console.debug("Herdsman cancelled superseded status event plans", {
        agentId,
        cancelled: superseded,
        from: fromStatus,
        herdrSessionName,
        to: toStatus,
      });
    }
    return row;
  }

  /**
   * Cancels pending/running plans for the same agent that a newly observed plan
   * supersedes, so an obsolete plan can no longer execute late and register a
   * stale transition (or the assistant ref of a round that was never announced
   * through that transition).
   *
   * An unfinished plan P is superseded by the newer plan N (same agent +
   * session) when the newer transition has a different target
   * (P.to_status <> N.to_status) and P can never wake an orchestrator anyway:
   * an `idle` target whose origin is not `working` (an `unknown -> idle`
   * startup plan, a `done -> idle` / `blocked -> idle` plan) delivers an
   * `agent.idle` row with payload `from` not `working`, which the delivery and
   * wake predicates both discard. Cancelling such a plan can therefore only
   * remove a stale, useless emission.
   *
   * Plans that can still deliver a wake (a `done`/`blocked` target, or an `idle`
   * target reached from `working`) are deliberately left in place: a pane that
   * reports `done` and then flips to `idle`/`working` while the completion plan
   * is still waiting for history must not lose its wake.
   *
   * The freshly inserted row itself is never touched, and settled rows
   * (completed/cancelled/failed/discarded) stay as they are. Cancelled rows are
   * swept by the same settled-row cleanup as other cancelled plans.
   */
  cancelSupersededPendingPlans(input: {
    agentId: string;
    herdrSessionName: string;
    keepId: number;
    toStatus: AgentStatus;
  }): number {
    const now = Date.now();
    const result = this.#sqlite
      .prepare(
        `update status_event_plans
         set status = 'cancelled', last_error = 'PLAN_SUPERSEDED', updated_at = ?
         where id <> ?
           and agent_id = ?
           and herdr_session_name = ?
           and status in ('pending', 'running')
           and to_status = 'idle'
           and from_status <> 'working'
           and to_status <> ?`,
      )
      .run(now, input.keepId, input.agentId, input.herdrSessionName, input.toStatus);
    return Number(result.changes);
  }

  get(id: number): StatusEventPlanRecord {
    const row = this.#sqlite.prepare("select * from status_event_plans where id = ?").get(id) as
      | StatusEventPlanRow
      | undefined;
    if (!row) throw new Error(`StatusEventPlan with id ${id} not found`);
    return mapStatusEventPlan(row);
  }

  listUnfinished(): StatusEventPlanRecord[] {
    const rows = this.#sqlite
      .prepare(
        "select * from status_event_plans where status in ('pending', 'running') order by id asc",
      )
      .all() as StatusEventPlanRow[];
    return rows.map(mapStatusEventPlan);
  }

  listFailed(): StatusEventPlanRecord[] {
    const rows = this.#sqlite
      .prepare("select * from status_event_plans where status = 'failed' order by id asc")
      .all() as StatusEventPlanRow[];
    return rows.map(mapStatusEventPlan);
  }

  listDiscarded(): StatusEventPlanRecord[] {
    const rows = this.#sqlite
      .prepare("select * from status_event_plans where status = 'discarded' order by id asc")
      .all() as StatusEventPlanRow[];
    return rows.map(mapStatusEventPlan);
  }

  listWaitingHistory(): StatusEventPlanRecord[] {
    const rows = this.#sqlite
      .prepare(
        "select * from status_event_plans where status = 'pending' and last_error = 'PLAN_WAITING_HISTORY' order by id asc",
      )
      .all() as StatusEventPlanRow[];
    return rows.map(mapStatusEventPlan);
  }

  resetRunningToPending(): number {
    const now = Date.now();
    const result = this.#sqlite
      .prepare(
        "update status_event_plans set status = 'pending', updated_at = ? where status = 'running'",
      )
      .run(now);
    return Number(result.changes);
  }

  markRunning(id: number): void {
    const now = Date.now();
    this.#sqlite
      .prepare("update status_event_plans set status = 'running', updated_at = ? where id = ?")
      .run(now, id);
  }

  markCompleted(id: number): void {
    const now = Date.now();
    this.#sqlite
      .prepare("update status_event_plans set status = 'completed', updated_at = ? where id = ?")
      .run(now, id);
  }

  markCancelled(id: number): void {
    const now = Date.now();
    this.#sqlite
      .prepare("update status_event_plans set status = 'cancelled', updated_at = ? where id = ?")
      .run(now, id);
  }

  markDiscarded(id: number, reason?: string): void {
    const now = Date.now();
    const current = this.get(id);
    const lastError = reason ?? current.lastError;
    this.#sqlite
      .prepare(
        "update status_event_plans set status = 'discarded', last_error = ?, updated_at = ? where id = ?",
      )
      .run(lastError, now, id);
  }

  markRetry(id: number, error: unknown): StatusEventPlanRecord | null {
    const current = this.get(id);
    if (
      current.status === "completed" ||
      current.status === "cancelled" ||
      current.status === "failed" ||
      current.status === "discarded"
    ) {
      return null;
    }
    const now = Date.now();
    const errorMessage = error instanceof Error ? error.message : String(error);
    const attempts = current.attempts + 1;
    let newStatus: StatusEventPlanStatus = "pending";
    if (attempts >= STATUS_PLAN_MAX_ATTEMPTS) {
      newStatus = errorMessage === "PLAN_WAITING_HISTORY" ? "discarded" : "failed";
    }

    const result = this.#sqlite
      .prepare(
        "update status_event_plans set attempts = ?, status = ?, last_error = ?, updated_at = ? where id = ? and status in ('pending', 'running')",
      )
      .run(attempts, newStatus, errorMessage, now, id);
    if (Number(result.changes) === 0) {
      return null;
    }
    return this.get(id);
  }

  /**
   * Purges settled plans (completed/cancelled/failed/discarded) whose updated_at is
   * older than ageMs. Pending and running rows are never touched: they are
   * still owned by the drain/retry cycle.
   */
  deleteSettledOlderThan(ageMs: number): number {
    const cutoff = Date.now() - ageMs;
    const result = this.#sqlite
      .prepare(
        `delete from status_event_plans
         where status in ('completed', 'cancelled', 'failed', 'discarded') and updated_at < ?`,
      )
      .run(cutoff);
    return Number(result.changes);
  }
}
