import type { DatabaseSync } from "node:sqlite";
import type { AgentStatus, CompactAgentHistory } from "@/observability/contracts.js";

export const STATUS_PLAN_MAX_ATTEMPTS = 8;

export type StatusEventPlanStatus = "pending" | "running" | "completed" | "cancelled" | "failed";

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
      return this.get(Number(result.lastInsertRowid));
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

  markRetry(id: number, error: unknown): void {
    const now = Date.now();
    const errorMessage = error instanceof Error ? error.message : String(error);
    const current = this.get(id);
    const attempts = current.attempts + 1;
    const newStatus: StatusEventPlanStatus =
      attempts >= STATUS_PLAN_MAX_ATTEMPTS ? "failed" : "pending";

    this.#sqlite
      .prepare(
        "update status_event_plans set attempts = ?, status = ?, last_error = ?, updated_at = ? where id = ?",
      )
      .run(attempts, newStatus, errorMessage, now, id);
  }

  /**
   * Purges settled plans (completed/cancelled/failed) whose updated_at is
   * older than ageMs. Pending and running rows are never touched: they are
   * still owned by the drain/retry cycle.
   */
  deleteSettledOlderThan(ageMs: number): number {
    const cutoff = Date.now() - ageMs;
    const result = this.#sqlite
      .prepare(
        `delete from status_event_plans
         where status in ('completed', 'cancelled', 'failed') and updated_at < ?`,
      )
      .run(cutoff);
    return Number(result.changes);
  }
}
