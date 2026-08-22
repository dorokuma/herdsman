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
    event.agentId !== null &&
    agent !== undefined &&
    event.type !== "agent.status.changed" &&
    !(isInteractivePiAgent(agent) && event.type === "agent.idle") &&
    event.deliverable === 1 &&
    agent.paneId === event.paneId &&
    (event.paneGeneration === null || agent.paneGeneration === event.paneGeneration) &&
    agent.workspaceId === scope.workspaceId &&
    event.workspaceId === scope.workspaceId &&
    event.herdrSessionName === scope.herdrSessionName &&
    event.terminalId !== null &&
    event.terminalId !== ownerTerminalId
  );
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
         (herdr_session_name, agent_id, pane_id, pane_generation, workspace_id, terminal_id, type, payload_json, compact_history_json, idempotency_key, deliverable, created_at)
         values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
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

  invalidatePane(input: { herdrSessionName: string; paneId: string; paneGeneration?: string | null }): void {
    this.#sqlite
      .prepare(
        `update agent_events set deliverable = 0
         where herdr_session_name = ? and pane_id = ?
           and (? is null or pane_generation = ? or pane_generation is null)`,
      )
      .run(input.herdrSessionName, input.paneId, input.paneGeneration ?? null, input.paneGeneration ?? null);
  }

  latestStatusTransition(
    agentId: string,
    herdrSessionName: string,
  ): AgentEventRecord | undefined {
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
    input: AgentQueryScope & { afterEventId?: number; limit?: number },
  ): AgentEventRecord[] {
    const clauses = ["id > ?", "deliverable = 1"];
    const params: Array<number | string | null> = [input.afterEventId ?? 0];
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
    const agentFilter = input.getAgent
      ? ""
      : `and exists (
             select 1 from agents
             where agents.id = agent_events.agent_id
               and agents.herdr_session_name = agent_events.herdr_session_name
               and agents.workspace_id = agent_events.workspace_id
               and agents.pane_id = agent_events.pane_id
           )`;
    const params = [input.afterEventId, input.herdrSessionName, input.workspaceId, input.ownerTerminalId];
    const rows = this.#sqlite
      .prepare(
        `select * from agent_events
         where id > ? and deliverable = 1 and herdr_session_name = ? and workspace_id = ?
           and terminal_id is not null and terminal_id != ? and agent_id is not null
           ${agentFilter}
         order by id asc limit 1000`,
      )
      .all(...params) as AgentEventRow[];
    const scope = { herdrSessionName: input.herdrSessionName, workspaceId: input.workspaceId };
    for (const row of rows) {
      const event = mapAgentEvent(row);
      if (!input.getAgent || isDeliverableAgentEvent(event, input.getAgent(event.agentId!), scope, input.ownerTerminalId)) {
        return event;
      }
    }
    return undefined;
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
