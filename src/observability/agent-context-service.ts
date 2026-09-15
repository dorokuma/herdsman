import {
  discoveryRecencyGraceMs,
  historySourceFromSessionRef,
  safeAllowedSessionPath,
} from "@/agent-history/discovery.js";
import type { AgentHistoryService } from "@/agent-history/service.js";
import { emptyCompactHistory } from "@/agent-history/service.js";
import { statSourceFingerprint } from "@/agent-history/source-fingerprint.js";
import type { AgentContextSnapshotStore } from "@/db/agent-context-snapshots.js";
import type { AgentStore } from "@/db/agents.js";
import type {
  AgentContextSnapshotRecord,
  AgentHistoryRef,
  AgentHistorySourceFingerprint,
  AgentIndexRecord,
  AgentListItem,
  AgentQueryScope,
  AgentScope,
  AgentWorkspaceContextSnapshot,
  CompactAgentHistory,
} from "./contracts.js";

export type RefreshAgentContextInput = {
  agent: AgentIndexRecord;
  identityChanged: boolean;
  forceRefresh?: boolean;
  occupiedSessionPaths?: ReadonlySet<string>;
};

export type RefreshAgentContextResult = {
  changed: boolean;
  snapshot: AgentContextSnapshotRecord;
};

export class AgentContextService {
  readonly #history: AgentHistoryService;
  readonly #stores: {
    agentContextSnapshots: AgentContextSnapshotStore;
    agents: AgentStore;
  };
  readonly #occupiedFingerprintByAgent = new Map<string, string>();

  constructor(options: {
    history: AgentHistoryService;
    stores: {
      agentContextSnapshots: AgentContextSnapshotStore;
      agents: AgentStore;
    };
  }) {
    this.#history = options.history;
    this.#stores = options.stores;
  }

  occupiedSessionPathsFor(agent: AgentIndexRecord): ReadonlySet<string> {
    return occupiedForAgent(agent, this.#stores.agents, this.#stores.agentContextSnapshots);
  }

  historyLookupInput(
    agent: AgentIndexRecord,
    occupiedSessionPaths?: ReadonlySet<string>,
  ): ReturnType<typeof historyLookup> {
    return historyLookup(
      agent,
      this.#stores.agents,
      this.#stores.agentContextSnapshots,
      occupiedSessionPaths,
    );
  }

  async preferredHistoryRef(agent: AgentIndexRecord): Promise<AgentHistoryRef | null> {
    const previous = this.#stores.agentContextSnapshots.get(agent.id);
    const occupiedSessionPaths = occupiedForAgent(
      agent,
      this.#stores.agents,
      this.#stores.agentContextSnapshots,
    );
    const occupiedFingerprint = fingerprintOccupied(occupiedSessionPaths);
    const priorOccupiedFingerprint = this.#occupiedFingerprintByAgent.get(agent.id);
    const occupiedChanged =
      priorOccupiedFingerprint !== undefined && priorOccupiedFingerprint !== occupiedFingerprint;
    return selectPreferredRef({
      agent,
      identityChanged: occupiedChanged,
      occupiedChanged,
      occupiedSessionPaths,
      previous,
    });
  }

  async refreshAgent(input: RefreshAgentContextInput): Promise<RefreshAgentContextResult> {
    const previous = this.#stores.agentContextSnapshots.get(input.agent.id);
    const occupiedSessionPaths = new Set([
      ...(input.occupiedSessionPaths ?? []),
      ...occupiedForAgent(input.agent, this.#stores.agents, this.#stores.agentContextSnapshots),
    ]);
    const occupiedFingerprint = fingerprintOccupied(occupiedSessionPaths);
    const priorOccupiedFingerprint = this.#occupiedFingerprintByAgent.get(input.agent.id);
    const occupiedChanged =
      priorOccupiedFingerprint !== undefined && priorOccupiedFingerprint !== occupiedFingerprint;
    this.#occupiedFingerprintByAgent.set(input.agent.id, occupiedFingerprint);
    const preferredRef = await selectPreferredRef({
      agent: input.agent,
      identityChanged: input.identityChanged || occupiedChanged,
      occupiedChanged,
      occupiedSessionPaths,
      previous,
    });
    const forceDiscovery =
      input.forceRefresh ||
      (await shouldForceDiscovery({
        agent: input.agent,
        directAuthoritativeRef: pathHistoryRefFromAgent(input.agent),
        identityChanged: input.identityChanged || occupiedChanged,
        preferredRef,
        previous,
      }));
    const resolved = bindAuthoritativeId(
      input.agent,
      await this.#history.resolveCompactHistory(
        this.historyLookupInput(input.agent, occupiedSessionPaths),
        {
          forceDiscovery,
          ...(input.forceRefresh === undefined ? {} : { forceRefresh: input.forceRefresh }),
          ...(preferredRef ? { preferredRef } : {}),
        },
      ),
    );
    const next = {
      agentId: input.agent.id,
      compactHistory: resolved.compactHistory,
      historyRef: resolved.historyRef,
      paneRevision: input.agent.paneRevision,
      sourceFingerprint: resolved.sourceFingerprint,
    };
    if (previous && sameSnapshotPayload(previous, next)) {
      return { changed: false, snapshot: previous };
    }
    return { changed: true, snapshot: this.#stores.agentContextSnapshots.put(next) };
  }

  getAgentSnapshot(agentId: string): AgentContextSnapshotRecord | undefined {
    return this.#stores.agentContextSnapshots.get(agentId);
  }

  listAgents(scope: AgentQueryScope): AgentListItem[] {
    const agents = this.#stores.agents.list(scope);
    const snapshots = new Map(
      this.#stores.agentContextSnapshots
        .listByAgentIds(agents.map((agent) => agent.id))
        .map((snapshot) => [snapshot.agentId, snapshot]),
    );
    return agents.map((agent) => listItem(agent, snapshots.get(agent.id)?.compactHistory));
  }

  workspaceSnapshot(
    input: AgentScope & { excludeTerminalId: string },
  ): AgentWorkspaceContextSnapshot | null {
    const agents = this.#stores.agents
      .list({ herdrSessionName: input.herdrSessionName, workspaceId: input.workspaceId })
      .filter((agent) => agent.terminalId !== input.excludeTerminalId);
    const snapshots = this.#stores.agentContextSnapshots.listByAgentIds(
      agents.map((agent) => agent.id),
    );
    if (snapshots.length === 0) return null;
    const firstSnapshot = snapshots[0];
    if (!firstSnapshot) return null;
    const byAgentId = new Map(snapshots.map((snapshot) => [snapshot.agentId, snapshot]));
    const updatedAt = snapshots.reduce(
      (latest, snapshot) => (snapshot.updatedAt > latest ? snapshot.updatedAt : latest),
      firstSnapshot.updatedAt,
    );
    return {
      agents: agents.map((agent) => listItem(agent, byAgentId.get(agent.id)?.compactHistory)),
      herdrSessionName: input.herdrSessionName,
      updatedAt: updatedAt.toISOString(),
      workspaceId: input.workspaceId,
    };
  }
}

async function shouldForceDiscovery(input: {
  agent: AgentIndexRecord;
  directAuthoritativeRef: AgentHistoryRef | null;
  identityChanged: boolean;
  preferredRef: AgentHistoryRef | null;
  previous: AgentContextSnapshotRecord | undefined;
}): Promise<boolean> {
  if (input.directAuthoritativeRef) return false;
  if (input.agent.agentSession?.kind === "id") return input.preferredRef === null;
  if (input.identityChanged || !input.previous?.historyRef) return true;
  if (paneRevisionDecreased(input.agent.paneRevision, input.previous.paneRevision)) return true;
  if (!paneRevisionIncreased(input.agent.paneRevision, input.previous.paneRevision)) return false;
  const fingerprint = input.previous.sourceFingerprint;
  if (!fingerprint) return true;
  const current = await statSourceFingerprint(fingerprint.path);
  return !current || sameFingerprint(fingerprint, current);
}

function pathHistoryRefFromAgent(agent: AgentIndexRecord): AgentHistoryRef | null {
  if (agent.agentSession?.kind !== "path") return null;
  const path = safeAllowedSessionPath(agent.agentSession.value);
  if (!path) return null;
  return {
    kind: "agent_session",
    path,
    source: historySourceFromSessionRef(agent.agentSession),
    value: agent.agentSession.value,
  };
}

function matchingAuthoritativeIdRef(
  agent: AgentIndexRecord,
  previous: AgentHistoryRef | null,
): AgentHistoryRef | null {
  const session = agent.agentSession;
  if (
    session?.kind !== "id" ||
    previous?.kind !== "agent_session" ||
    !previous.path ||
    previous.source !== historySourceFromSessionRef(session) ||
    previous.value !== session.value
  ) {
    return null;
  }
  return previous;
}

function bindAuthoritativeId(
  agent: AgentIndexRecord,
  resolved: Awaited<ReturnType<AgentHistoryService["resolveCompactHistory"]>>,
): Awaited<ReturnType<AgentHistoryService["resolveCompactHistory"]>> {
  const session = agent.agentSession;
  if (session?.kind !== "id" || !resolved.historyRef?.path) return resolved;
  const historyRef: AgentHistoryRef = {
    kind: "agent_session",
    path: resolved.historyRef.path,
    source: historySourceFromSessionRef(session),
    value: session.value,
  };
  return {
    ...resolved,
    compactHistory: { ...resolved.compactHistory, historyRef },
    historyRef,
  };
}

async function selectPreferredRef(input: {
  agent: AgentIndexRecord;
  identityChanged: boolean;
  occupiedChanged: boolean;
  occupiedSessionPaths: ReadonlySet<string>;
  previous: AgentContextSnapshotRecord | undefined;
}): Promise<AgentHistoryRef | null> {
  const previousRef = input.previous?.historyRef ?? null;
  const directAuthoritativeRef = pathHistoryRefFromAgent(input.agent);
  if (directAuthoritativeRef) return directAuthoritativeRef;
  const matchingId = matchingAuthoritativeIdRef(input.agent, previousRef);
  if (matchingId) return matchingId;
  if (previousRef?.kind === "discovered_file") {
    const path = previousRef.path ?? previousRef.value;
    if (input.identityChanged || input.occupiedChanged || input.occupiedSessionPaths.has(path)) {
      return null;
    }
    if (!(await discoveredFileStillRecent(input.agent, path))) return null;
    return previousRef;
  }
  if (input.agent.agentSession) return null;
  return previousRef;
}

async function discoveredFileStillRecent(agent: AgentIndexRecord, path: string): Promise<boolean> {
  const fingerprint = await statSourceFingerprint(path);
  if (!fingerprint) return false;
  const graceMs = discoveryRecencyGraceMs({
    agent: agent.agent,
    agentSession: agent.agentSession,
    ...(agent.terminalTitle ? { terminalTitle: agent.terminalTitle } : {}),
  });
  return fingerprint.mtimeMs >= agent.firstSeenAt.getTime() - graceMs;
}

export function occupiedForAgent(
  agent: AgentIndexRecord,
  agents: AgentStore,
  snapshots: AgentContextSnapshotStore,
): ReadonlySet<string> {
  const others = agents
    .listForHerdrSession(agent.herdrSessionName)
    .filter((candidate) => candidate.id !== agent.id);
  return new Set([
    ...others.flatMap((candidate) =>
      candidate.agentSession?.kind === "path" ? [candidate.agentSession.value] : [],
    ),
    ...snapshots
      .listByAgentIds(others.map((candidate) => candidate.id))
      .flatMap((snapshot) => (snapshot.historyRef?.path ? [snapshot.historyRef.path] : [])),
  ]);
}

function fingerprintOccupied(paths: ReadonlySet<string>): string {
  return [...paths].sort().join("\0");
}

function historyLookup(
  agent: AgentIndexRecord,
  agents: AgentStore,
  snapshots: AgentContextSnapshotStore,
  occupiedSessionPaths?: ReadonlySet<string>,
) {
  return {
    agent: agent.agent,
    agentSession: agent.agentSession,
    cwd: agent.cwd,
    firstSeenAtMs: agent.firstSeenAt.getTime(),
    foregroundCwd: agent.foregroundCwd,
    herdrSessionName: agent.herdrSessionName,
    ...(agent.agent?.toLowerCase() === "grok" && agent.grokHome
      ? { grokHome: agent.grokHome }
      : {}),
    ...(agent.terminalTitle ? { terminalTitle: agent.terminalTitle } : {}),
    occupiedSessionPaths: occupiedSessionPaths ?? occupiedForAgent(agent, agents, snapshots),
  };
}

function paneRevisionIncreased(current: number | null, previous: number | null): boolean {
  return current !== null && previous !== null && current > previous;
}

function paneRevisionDecreased(current: number | null, previous: number | null): boolean {
  return current !== null && previous !== null && current < previous;
}

function sameSnapshotPayload(
  previous: AgentContextSnapshotRecord,
  next: Omit<AgentContextSnapshotRecord, "updatedAt">,
): boolean {
  return (
    JSON.stringify(previous.compactHistory) === JSON.stringify(next.compactHistory) &&
    sameHistoryRef(previous.historyRef, next.historyRef) &&
    sameFingerprint(previous.sourceFingerprint, next.sourceFingerprint) &&
    previous.paneRevision === next.paneRevision
  );
}

function sameFingerprint(
  left: AgentHistorySourceFingerprint | null,
  right: AgentHistorySourceFingerprint | null,
): boolean {
  return (
    left?.path === right?.path && left?.mtimeMs === right?.mtimeMs && left?.size === right?.size
  );
}

function sameHistoryRef(left: AgentHistoryRef | null, right: AgentHistoryRef | null): boolean {
  return (
    left?.kind === right?.kind &&
    left?.path === right?.path &&
    left?.source === right?.source &&
    left?.value === right?.value
  );
}

function listItem(
  agent: AgentIndexRecord,
  compactHistory: CompactAgentHistory | undefined,
): AgentListItem {
  const compact = compactHistory ?? emptyCompactHistory();
  return {
    ...agent,
    history: {
      lastAssistantMessage: compact.lastAssistantMessage,
      lastUserMessage: compact.lastUserMessage,
      source: compact.source,
      updatedAt: compact.updatedAt,
    },
  };
}
