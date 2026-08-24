import { randomUUID } from "node:crypto";
import { lstatSync, readFileSync, realpathSync } from "node:fs";
import type { DatabaseSync } from "node:sqlite";
import type { AgentEventStore } from "@/db/agent-events.js";
import {
  type AgentIndexRecord,
  type AgentQueryScope,
  type AgentSessionRef,
  type AgentStatus,
  parseAgentStatus,
} from "@/observability/contracts.js";

export type HerdrAgentLike = Record<string, unknown>;

type AgentRow = {
  agent: string | null;
  agent_session_hint_json: string | null;
  agent_session_json: string | null;
  agent_status: AgentStatus;
  cwd: string | null;
  first_seen_at: number;
  focused: 0 | 1;
  foreground_cwd: string | null;
  herdr_session_name: string;
  id: string;
  last_seen_at: number;
  name: string | null;
  pane_id: string;
  pane_revision: number | null;
  pane_generation: string | null;
  grok_home: string | null;
  tab_id: string | null;
  terminal_id: string | null;
  workspace_id: string;
};

export class AgentStore {
  readonly #sqlite: DatabaseSync;
  readonly #agentEvents: AgentEventStore;

  constructor(sqlite: DatabaseSync, agentEvents: AgentEventStore) {
    this.#sqlite = sqlite;
    this.#agentEvents = agentEvents;
  }

  replaceForSession(input: {
    agents: HerdrAgentLike[];
    herdrSessionName: string;
  }): AgentIndexRecord[] {
    const now = Date.now();
    const snapshots = input.agents.flatMap((agent) => {
      const paneId = stringValue(agent.pane_id) ?? stringValue(agent.paneId);
      const workspaceId = stringValue(agent.workspace_id) ?? stringValue(agent.workspaceId);
      if (!paneId || !workspaceId) return [];
      return [
        {
          agent,
          paneId,
          terminalId: stringValue(agent.terminal_id) ?? stringValue(agent.terminalId),
          workspaceId,
        },
      ];
    });

    return this.#transaction(() => {
      const existing = this.#sqlite
        .prepare("select * from agents where herdr_session_name = ?")
        .all(input.herdrSessionName) as AgentRow[];
      const byPane = new Map(existing.map((agent) => [agent.pane_id, agent]));
      const byTerminal = new Map(
        existing.flatMap((agent) =>
          agent.terminal_id ? [[agent.terminal_id, agent] as const] : [],
        ),
      );
      const matched = snapshots.map((snapshot) => {
        const terminalMatch = snapshot.terminalId ? byTerminal.get(snapshot.terminalId) : undefined;
        const paneMatch = byPane.get(snapshot.paneId);
        const incomingGeneration = paneGeneration(snapshot.agent);
        const canUsePaneFallback =
          paneMatch && (snapshot.terminalId === null || paneMatch.terminal_id === null);
        const generationMatches =
          !incomingGeneration ||
          !paneMatch?.pane_generation ||
          incomingGeneration === paneMatch.pane_generation;
        return {
          existing:
            terminalMatch ?? (generationMatches && canUsePaneFallback ? paneMatch : undefined),
          snapshot,
        };
      });
      const temporaryPaneIds = new Set<string>();
      for (const { existing: current, snapshot } of matched) {
        if (current && current.pane_id !== snapshot.paneId) temporaryPaneIds.add(current.id);
        const occupant = byPane.get(snapshot.paneId);
        if (occupant && occupant.id !== current?.id) temporaryPaneIds.add(occupant.id);
      }
      for (const id of temporaryPaneIds) {
        this.#sqlite
          .prepare("update agents set pane_id = ? where id = ?")
          .run(`__herdsman_moving__:${id}`, id);
      }

      const retainedIds: string[] = [];
      for (const { existing: current, snapshot } of matched) {
        const id = current?.id ?? `ag_${randomUUID()}`;
        retainedIds.push(id);
        const agent = stringValue(snapshot.agent.agent);
        const name = stringValue(snapshot.agent.name);
        const sessionHint = current?.agent === agent ? current.agent_session_hint_json : null;
        const grokHome =
          stringValue(snapshot.agent.agent)?.toLowerCase() === "grok"
            ? grokHomeForAgent(snapshot.agent)
            : null;
        const values = [
          snapshot.paneId,
          snapshot.terminalId,
          stringValue(snapshot.agent.tab_id) ?? stringValue(snapshot.agent.tabId),
          snapshot.workspaceId,
          agent,
          name,
          parseAgentStatus(snapshot.agent.agent_status),
          agentSessionJson(snapshot.agent.agent_session),
          sessionHint,
          integerValue(snapshot.agent.revision),
          paneGeneration(snapshot.agent) ?? current?.pane_generation ?? null,
          grokHome ?? current?.grok_home ?? null,
          stringValue(snapshot.agent.cwd),
          stringValue(snapshot.agent.foreground_cwd) ?? stringValue(snapshot.agent.foregroundCwd),
          snapshot.agent.focused === true ? 1 : 0,
          now,
        ];
        if (current) {
          this.#sqlite
            .prepare(
              `update agents
               set pane_id = ?, terminal_id = ?, tab_id = ?, workspace_id = ?, agent = ?, name = ?,
                   agent_status = ?, agent_session_json = ?, agent_session_hint_json = ?, pane_revision = ?, pane_generation = ?, grok_home = ?,
                   cwd = ?, foreground_cwd = ?, focused = ?, last_seen_at = ?
               where id = ?`,
            )
            .run(...values, id);
        } else {
          this.#sqlite
            .prepare(
              `insert into agents
               (id, herdr_session_name, pane_id, terminal_id, tab_id, workspace_id, agent, name, agent_status, agent_session_json, agent_session_hint_json, pane_revision, pane_generation, grok_home, cwd, foreground_cwd, focused, first_seen_at, last_seen_at)
               values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
            )
            .run(id, input.herdrSessionName, ...values, now);
        }
      }

      const removedIds = existing
        .map((agent) => agent.id)
        .filter((id) => !retainedIds.includes(id));
      for (const agent of existing.filter((candidate) => removedIds.includes(candidate.id))) {
        this.#agentEvents.invalidatePane({
          herdrSessionName: input.herdrSessionName,
          paneId: agent.pane_id,
          paneGeneration: agent.pane_generation,
        });
      }
      if (removedIds.length > 0) {
        const placeholders = removedIds.map(() => "?").join(", ");
        this.#sqlite
          .prepare(`delete from agent_context_snapshots where agent_id in (${placeholders})`)
          .run(...removedIds);
      }
      if (retainedIds.length === 0) {
        this.#sqlite
          .prepare("delete from agents where herdr_session_name = ?")
          .run(input.herdrSessionName);
      } else {
        const placeholders = retainedIds.map(() => "?").join(", ");
        this.#sqlite
          .prepare(
            `delete from agents where herdr_session_name = ? and id not in (${placeholders})`,
          )
          .run(input.herdrSessionName, ...retainedIds);
      }
      return this.listForHerdrSession(input.herdrSessionName);
    });
  }

  retirePane(input: {
    herdrSessionName: string;
    paneId: string;
    paneGeneration?: string | null;
  }): AgentIndexRecord[] {
    return this.#transaction(() => {
      const generationClause = input.paneGeneration == null ? "" : " and pane_generation = ?";
      const generationParams = input.paneGeneration == null ? [] : [input.paneGeneration];
      const agents = this.#sqlite
        .prepare(
          `select * from agents where herdr_session_name = ? and pane_id = ?${generationClause}`,
        )
        .all(input.herdrSessionName, input.paneId, ...generationParams) as AgentRow[];
      if (agents.length === 0) return [];
      const ids = agents.map((agent) => agent.id);
      const placeholders = ids.map(() => "?").join(", ");
      this.#sqlite
        .prepare(`delete from agent_context_snapshots where agent_id in (${placeholders})`)
        .run(...ids);
      this.#sqlite
        .prepare(
          `delete from agents where herdr_session_name = ? and pane_id = ?${generationClause}`,
        )
        .run(input.herdrSessionName, input.paneId, ...generationParams);
      return agents.map(mapAgent);
    });
  }

  setSessionRefByTerminal(input: {
    agentSession: AgentSessionRef;
    herdrSessionName: string;
    terminalId: string;
  }): AgentIndexRecord | undefined {
    const current = this.findByTerminal(input);
    if (!current) return undefined;
    const compatible = current.agent?.toLowerCase() === input.agentSession.agent.toLowerCase();
    this.#sqlite
      .prepare(
        `update agents
         set agent_session_hint_json = ?
         where herdr_session_name = ? and terminal_id = ?`,
      )
      .run(
        compatible ? JSON.stringify(input.agentSession) : null,
        input.herdrSessionName,
        input.terminalId,
      );
    return this.findByTerminal(input);
  }

  updateStatus(input: {
    agentStatus: AgentStatus;
    herdrSessionName: string;
    paneId: string;
    paneGeneration?: string | null;
  }): AgentIndexRecord | undefined {
    const now = Date.now();
    const generationClause = input.paneGeneration == null ? "" : " and pane_generation = ?";
    const generationParams = input.paneGeneration == null ? [] : [input.paneGeneration];
    this.#sqlite
      .prepare(
        `update agents set agent_status = ?, last_seen_at = ? where herdr_session_name = ? and pane_id = ?${generationClause}`,
      )
      .run(input.agentStatus, now, input.herdrSessionName, input.paneId, ...generationParams);
    return this.findByPane(input);
  }

  list(scope: AgentQueryScope = {}): AgentIndexRecord[] {
    const clauses = ["sessions.running = 1"];
    const params: Array<number | string | null> = [];
    if (!scope.all && scope.herdrSessionName) {
      clauses.push("agents.herdr_session_name = ?");
      params.push(scope.herdrSessionName);
    }
    if (!scope.all && scope.workspaceId) {
      clauses.push("agents.workspace_id = ?");
      params.push(scope.workspaceId);
    }
    if (scope.all && scope.herdrSessionName) {
      clauses.push("agents.herdr_session_name = ?");
      params.push(scope.herdrSessionName);
    }
    const where = ` where ${clauses.join(" and ")}`;
    const rows = this.#sqlite
      .prepare(
        `select agents.*
         from agents
         inner join herdr_sessions as sessions
           on sessions.name = agents.herdr_session_name
         ${where}
         order by agents.herdr_session_name, agents.workspace_id, agents.pane_id`,
      )
      .all(...params) as AgentRow[];
    return rows.map(mapAgent);
  }

  listForHerdrSession(herdrSessionName: string): AgentIndexRecord[] {
    return this.list({ herdrSessionName });
  }

  findByPane(input: {
    herdrSessionName: string;
    paneId: string;
    paneGeneration?: string | null;
  }): AgentIndexRecord | undefined {
    const generationClause = input.paneGeneration == null ? "" : " and pane_generation = ?";
    const row = this.#sqlite
      .prepare(
        `select * from agents where herdr_session_name = ? and pane_id = ?${generationClause}`,
      )
      .get(
        input.herdrSessionName,
        input.paneId,
        ...(input.paneGeneration == null ? [] : [input.paneGeneration]),
      ) as AgentRow | undefined;
    return row ? mapAgent(row) : undefined;
  }

  findByTerminal(input: {
    herdrSessionName: string;
    terminalId: string;
  }): AgentIndexRecord | undefined {
    const row = this.#sqlite
      .prepare("select * from agents where herdr_session_name = ? and terminal_id = ?")
      .get(input.herdrSessionName, input.terminalId) as AgentRow | undefined;
    return row ? mapAgent(row) : undefined;
  }

  get(id: string): AgentIndexRecord {
    const row = this.#sqlite.prepare("select * from agents where id = ?").get(id) as
      | AgentRow
      | undefined;
    if (!row) throw new Error(`Agent not found: ${id}`);
    return mapAgent(row);
  }

  resolveTarget(scope: AgentQueryScope, target: string): AgentIndexRecord {
    const agents = this.list(scope);
    const candidateGroups = [
      agents.filter(
        (agent) => agent.paneId === target || agent.terminalId === target || agent.id === target,
      ),
      agents.filter((agent) => agent.name === target),
      agents.filter((agent) => agent.agent === target),
    ];
    for (const candidates of candidateGroups) {
      if (candidates.length === 1) return candidates[0] as AgentIndexRecord;
      if (candidates.length > 1) throw ambiguousTargetError(target, candidates);
    }
    throw new Error(`agent target not found: ${target}`);
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
}

function mapAgent(row: AgentRow): AgentIndexRecord {
  const reportedSession = parseAgentSession(row.agent_session_json);
  const hintedSession = parseAgentSession(row.agent_session_hint_json);
  const compatibleHint =
    row.agent?.toLowerCase() === hintedSession?.agent.toLowerCase() ? hintedSession : null;
  return {
    agent: row.agent,
    agentSession: reportedSession ?? compatibleHint,
    agentStatus: row.agent_status,
    cwd: row.cwd,
    firstSeenAt: new Date(row.first_seen_at),
    focused: row.focused === 1,
    foregroundCwd: row.foreground_cwd,
    herdrSessionName: row.herdr_session_name,
    id: row.id,
    lastSeenAt: new Date(row.last_seen_at),
    name: row.name,
    paneId: row.pane_id,
    paneRevision: row.pane_revision,
    ...(row.pane_generation === null ? {} : { paneGeneration: row.pane_generation }),
    tabId: row.tab_id,
    terminalId: row.terminal_id,
    workspaceId: row.workspace_id,
    grokHome: row.grok_home,
  };
}

function grokHomeForAgent(agent: HerdrAgentLike): string | null {
  const env = agent.env;
  const explicit =
    typeof env === "object" && env !== null
      ? stringValue((env as Record<string, unknown>).GROK_HOME)
      : null;
  // Proc fallback cannot prove the pid is the pane's agent; prefer explicit metadata.
  const raw =
    explicit ??
    (() => {
      const pid = agent.pid;
      if (typeof pid !== "number" || !Number.isInteger(pid) || pid <= 0) return null;
      try {
        return (
          readFileSync(`/proc/${pid}/environ`)
            .toString("utf8")
            .split("\0")
            .find((item) => item.startsWith("GROK_HOME="))
            ?.slice(10) ?? null
        );
      } catch {
        return null;
      }
    })();
  return raw ? validateGrokHome(raw) : null;
}

function validateGrokHome(value: string): string | null {
  if (!value.startsWith("/") || value.includes("..")) return null;
  try {
    const link = lstatSync(value);
    if (
      !link.isDirectory() ||
      link.isSymbolicLink() ||
      (link.mode & 0o022) !== 0 ||
      link.uid !== (process.geteuid?.() ?? -1)
    )
      return null;
    const real = realpathSync(value);
    const relativeRoot = require("node:path").relative(value, real) as string;
    if (relativeRoot.startsWith("..") || relativeRoot.includes("/")) return null;
    return real;
  } catch {
    return null;
  }
}

function paneGeneration(agent: HerdrAgentLike): string | null {
  return (
    stringValue(agent.pane_generation) ??
    stringValue(agent.paneGeneration) ??
    stringValue(agent.creation_id) ??
    stringValue(agent.creationId)
  );
}
function agentSessionJson(value: unknown): string | null {
  if (typeof value !== "object" || value === null) return null;
  const record = value as Record<string, unknown>;
  if (
    typeof record.agent === "string" &&
    (record.kind === "id" || record.kind === "path") &&
    typeof record.source === "string" &&
    typeof record.value === "string"
  ) {
    return JSON.stringify({
      agent: record.agent,
      kind: record.kind,
      source: record.source,
      value: record.value,
    } satisfies AgentSessionRef);
  }
  return null;
}

function parseAgentSession(value: string | null): AgentSessionRef | null {
  if (!value) return null;
  try {
    const parsed = JSON.parse(value) as unknown;
    if (typeof parsed !== "object" || parsed === null) return null;
    const record = parsed as Record<string, unknown>;
    if (
      typeof record.agent === "string" &&
      (record.kind === "id" || record.kind === "path") &&
      typeof record.source === "string" &&
      typeof record.value === "string"
    ) {
      return {
        agent: record.agent,
        kind: record.kind,
        source: record.source,
        value: record.value,
      };
    }
  } catch {
    return null;
  }
  return null;
}

function ambiguousTargetError(target: string, candidates: AgentIndexRecord[]): Error {
  return new Error(
    `agent target ${target} is ambiguous; candidates: ${candidates
      .map(
        (agent) =>
          `session=${agent.herdrSessionName} workspace=${agent.workspaceId} pane=${agent.paneId} terminal=${agent.terminalId ?? "unknown"} name=${agent.name ?? "unnamed"} agent=${agent.agent ?? "unknown"}`,
      )
      .join("; ")}`,
  );
}

function integerValue(value: unknown): number | null {
  return typeof value === "number" && Number.isInteger(value) && value >= 0 ? value : null;
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}
