import type { DatabaseSync } from "node:sqlite";
import type {
  AgentEventRecord,
  AgentEventType,
  AgentIndexRecord,
  AgentQueryScope,
  CompactAgentHistory,
} from "@/observability/contracts.js";
import { isInteractivePiAgent } from "@/observability/interactive-pi.js";

/** The single delivery predicate shared by pending discovery and acknowledgement. */
export function isDeliverableAgentEvent(
  event: AgentEventRecord,
  agent: AgentIndexRecord | undefined,
  scope: { herdrSessionName: string; workspaceId: string },
  ownerTerminalId: string,
): boolean {
  return (
    (event.status === "pending" || event.status === "delivered") &&
    (event.status === "delivered" ||
      event.nextAttemptAt === null ||
      event.nextAttemptAt === undefined ||
      event.nextAttemptAt <= new Date()) &&
    (event.status === "pending" || event.deliveredToTerminalId === ownerTerminalId) &&
    event.agentId !== null &&
    agent !== undefined &&
    event.type !== "agent.status.changed" &&
    !(event.type === "agent.idle" && asRecord(event.payload).from !== "working") &&
    !(isInteractivePiAgent(agent) && event.type === "agent.idle") &&
    agent.paneId === event.paneId &&
    (event.paneGeneration === null || agent.paneGeneration === event.paneGeneration) &&
    agent.workspaceId === scope.workspaceId &&
    event.workspaceId === scope.workspaceId &&
    event.herdrSessionName === scope.herdrSessionName &&
    event.terminalId !== null &&
    event.terminalId !== ownerTerminalId
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

  constructor(sqlite: DatabaseSync) {
    this.#sqlite = sqlite;
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

    const result = this.#sqlite
      .prepare(
        `insert into agent_events
         (herdr_session_name, agent_id, pane_id, pane_generation, workspace_id, terminal_id, type, payload_json, compact_history_json, idempotency_key, deliverable, status, delivery_attempts, created_at)
         values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'pending', 0, ?)`,
      )
      .run(
        input.herdrSessionName,
        input.agentId ?? null,
        input.paneId ?? null,
        input.paneGeneration ?? null,
        input.workspaceId ?? null,
        input.terminalId ?? null,
        input.type,
        JSON.stringify(input.payload),
        input.compactHistory ? JSON.stringify(input.compactHistory) : null,
        input.idempotencyKey ?? null,
        Date.now(),
      );
    return this.get(Number(result.lastInsertRowid));
  }

  invalidatePane(input: {
    herdrSessionName: string;
    paneId: string;
    paneGeneration?: string | null;
    invalidatedReason?: string;
  }): void {
    const legacy = input.paneGeneration == null;
    this.#sqlite
      .prepare(
        `update agent_events set deliverable = 0, status = 'invalidated', invalidated_reason = ?
         where herdr_session_name = ? and pane_id = ?
           and status not in ('acked', 'invalidated', 'failed')
           and (${legacy ? "pane_generation is null" : "pane_generation = ?"})`,
      )
      .run(
        input.invalidatedReason ?? (legacy ? "LEGACY_CLOSE_WITHOUT_GENERATION" : "PANE_CLOSED"),
        input.herdrSessionName,
        input.paneId,
        ...(legacy ? [] : [input.paneGeneration ?? null]),
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

  deleteReconcileCandidate(id: number): boolean {
    return (
      Number(
        this.#sqlite
          .prepare("delete from agent_events where id = ? and status in ('pending', 'delivered')")
          .run(id).changes,
      ) > 0
    );
  }

  deleteInvalidated(): number {
    return Number(
      this.#sqlite.prepare("delete from agent_events where status = 'invalidated'").run().changes,
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

  listAfter(
    input: AgentQueryScope & { afterEventId?: number; limit?: number; ownerTerminalId?: string },
  ): AgentEventRecord[] {
    const clauses = [
      "id > ?",
      "(status = 'pending' and (next_attempt_at is null or next_attempt_at <= ?)) or (status = 'delivered' and delivered_to_terminal_id = ?)",
    ];
    const params: Array<number | string | null> = [
      input.afterEventId ?? 0,
      Date.now(),
      input.ownerTerminalId ?? null,
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
    const agentFilter = input.getAgent
      ? ""
      : `and exists (
             select 1 from agents
             where agents.id = agent_events.agent_id
               and agents.herdr_session_name = agent_events.herdr_session_name
               and agents.workspace_id = agent_events.workspace_id
               and agents.pane_id = agent_events.pane_id
           )`;
    let afterEventId = input.afterEventId;
    for (let page = 0; page < 50; page += 1) {
      const params = [
        afterEventId,
        Date.now(),
        input.ownerTerminalId,
        input.herdrSessionName,
        input.workspaceId,
        input.ownerTerminalId,
      ];
      const rows = this.#sqlite
        .prepare(
          `select * from agent_events
           where id > ? and ((status = 'pending' and (next_attempt_at is null or next_attempt_at <= ?)) or (status = 'delivered' and delivered_to_terminal_id = ?)) and herdr_session_name = ? and workspace_id = ?
             and terminal_id is not null and terminal_id != ? and agent_id is not null
             ${agentFilter}
           order by id asc limit 1000`,
        )
        .all(...params) as AgentEventRow[];
      if (rows.length === 0) return undefined;
      for (const row of rows) {
        const event = mapAgentEvent(row);
        const agentId = event.agentId;
        if (
          agentId !== null &&
          (!input.getAgent ||
            isDeliverableAgentEvent(event, input.getAgent(agentId), scope, input.ownerTerminalId))
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
      const now = Date.now();
      const idClause = ids && ids.length > 0 ? `and id in (${ids.map(() => "?").join(",")})` : "";
      const params: Array<number | string> = [now, terminalId];
      if (ids && ids.length > 0) params.push(...ids);
      params.push(limit);
      const rows = this.#sqlite
        .prepare(
          `select * from agent_events where ((status = 'pending' and (next_attempt_at is null or next_attempt_at <= ?)) or (status = 'delivered' and delivered_to_terminal_id = ?)) ${idClause} order by id asc limit ?`,
        )
        .all(...params) as AgentEventRow[];
      for (const row of rows) {
        this.#sqlite
          .prepare(
            `update agent_events set status = 'delivered', deliverable = 1, delivery_attempts = ?, last_attempt_at = ?, delivered_to_terminal_id = ? where id = ? and status = 'pending'`,
          )
          .run(row.delivery_attempts + 1, now, terminalId, row.id);
      }
      return rows.map((row) => this.get(row.id));
    });
  }

  reclaimDelivered(timeoutMs: number): number {
    return this.#transaction(() => {
      const result = this.#sqlite
        .prepare(
          `update agent_events set status = case when delivery_attempts >= 10 then 'failed' else 'pending' end, deliverable = case when delivery_attempts >= 10 then 0 else 1 end, last_failure_code = case when delivery_attempts >= 10 then 'DELIVERY_ATTEMPTS_EXCEEDED' else last_failure_code end, delivered_to_terminal_id = null, next_attempt_at = null where status = 'delivered' and last_attempt_at < ?`,
        )
        .run(Date.now() - timeoutMs);
      return Number(result.changes);
    });
  }

  markAcked(id: number): void {
    this.#sqlite
      .prepare(
        `update agent_events set status = 'acked', deliverable = 0 where id = ? and status in ('pending', 'delivered')`,
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
    deliverable: row.status === "pending" || row.status === "delivered" ? 1 : 0,
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
