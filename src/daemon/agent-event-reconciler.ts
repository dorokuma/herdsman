import { existsSync } from "node:fs";
import type { AgentEventStore } from "@/db/agent-events.js";
import type { AgentHistoryCacheStore } from "@/db/agent-history-cache.js";
import type { AgentOrchestratorScopeStore } from "@/db/agent-orchestrator-scopes.js";
import type { StatusEventPlanStore } from "@/db/status-event-plans.js";
import type { HerdrSessionListEntry, HerdrSessionListRunner } from "@/herdr/session-list.js";
import { normalizeHerdrSessionSnapshot } from "@/herdr/session-snapshot.js";
import { HerdrSocketClient } from "@/herdr/socket-client.js";
import type { AgentEventRecord } from "@/observability/contracts.js";

export const RECONCILE_BATCH_LIMIT = 100;
export const RECONCILE_REASON = "PANE_NOT_PRESENT_RECONCILE";

/** Release rows are ownerless scopes; keep them reclaimable for 30 days, then drop them. */
export const SCOPE_RELEASE_TTL_MS = 30 * 24 * 60 * 60 * 1000;

/** Terminal agent events (acked/failed) are purged after 7 days in the reconcile cycle. */
export const RECONCILE_SETTLED_TTL_MS = 7 * 24 * 60 * 60 * 1000;

/** History cache rows are purged after 30 days regardless of whether the source file still exists. */
export const HISTORY_CACHE_TTL_MS = 30 * 24 * 60 * 60 * 1000;

/**
 * Invalidated events are retained this long so an owner that already received
 * the event can still ack it (`isInvalidatedDelivered`) and advance the cursor.
 * Must be >= REDELIVERY_FRESHNESS_MS; 1 hour covers reconnect delays without
 * letting invalidated rows accumulate.
 */
export const INVALIDATED_GRACE_MS = 60 * 60 * 1000;

type LivePane = { paneId: string; generation: string | null; terminalId: string | null };

export type AgentEventReconcileResult = {
  historyCacheExpired: number;
  historyCacheMissing: number;
  invalidated: number;
  purged: number;
  released: number;
};

function emptyReconcileResult(): AgentEventReconcileResult {
  return {
    historyCacheExpired: 0,
    historyCacheMissing: 0,
    invalidated: 0,
    purged: 0,
    released: 0,
  };
}

export class AgentEventReconciler {
  readonly #agentHistoryCache: AgentHistoryCacheStore | undefined;
  readonly #events: AgentEventStore;
  readonly #scopes: AgentOrchestratorScopeStore;
  readonly #sessionList: HerdrSessionListRunner;
  readonly #connectedTerminal: (input: { herdrSessionName: string; terminalId: string }) => boolean;
  readonly #clientFactory: (entry: HerdrSessionListEntry) => HerdrSocketClient;
  readonly #statusEventPlans: StatusEventPlanStore | undefined;

  constructor(options: {
    agentHistoryCache?: AgentHistoryCacheStore;
    events: AgentEventStore;
    scopes: AgentOrchestratorScopeStore;
    sessionList: HerdrSessionListRunner;
    clientFactory?: (entry: HerdrSessionListEntry) => HerdrSocketClient;
    connectedTerminal?: (input: { herdrSessionName: string; terminalId: string }) => boolean;
    statusEventPlans?: StatusEventPlanStore;
  }) {
    this.#agentHistoryCache = options.agentHistoryCache;
    this.#events = options.events;
    this.#scopes = options.scopes;
    this.#sessionList = options.sessionList;
    this.#clientFactory =
      options.clientFactory ?? ((entry) => new HerdrSocketClient({ socketPath: entry.socketPath }));
    this.#connectedTerminal = options.connectedTerminal ?? (() => true);
    this.#statusEventPlans = options.statusEventPlans;
  }

  async reconcile(
    options: { releaseStaleOwners?: boolean } = {},
  ): Promise<AgentEventReconcileResult> {
    let sessions: HerdrSessionListEntry[];
    try {
      sessions = await this.#sessionList();
    } catch (error) {
      console.warn("Herdsman reconcile skipped: Herdr session list unavailable", error);
      return emptyReconcileResult();
    }
    const live = new Map<string, LivePane[]>();
    const clients: HerdrSocketClient[] = [];
    try {
      for (const entry of sessions.filter((item) => item.running)) {
        const client = this.#clientFactory(entry);
        clients.push(client);
        const snapshot = normalizeHerdrSessionSnapshot(await client.sessionSnapshot());
        live.set(
          entry.name,
          snapshot.panes.flatMap((pane) => {
            if (typeof pane !== "object" || pane === null) return [];
            const item = pane as Record<string, unknown>;
            const paneId = stringValue(item.pane_id) ?? stringValue(item.paneId);
            if (!paneId) return [];
            return [
              {
                paneId,
                generation: stringValue(item.pane_generation) ?? stringValue(item.paneGeneration),
                terminalId: stringValue(item.terminal_id) ?? stringValue(item.terminalId),
              },
            ];
          }),
        );
      }
    } catch (error) {
      for (const client of clients) client.close();
      console.warn("Herdsman reconcile skipped: incomplete Herdr pane snapshot", error);
      return emptyReconcileResult();
    } finally {
      for (const client of clients) client.close();
    }

    let invalidated = 0;
    let cursor = 0;
    for (;;) {
      const candidates = this.#events.listReconcileCandidates(RECONCILE_BATCH_LIMIT, cursor);
      if (candidates.length === 0) break;
      for (const event of candidates) {
        cursor = event.id;
        const panes = live.get(event.herdrSessionName);
        if (!panes) continue;
        const present = panes.some((pane) => paneMatchesEvent(pane, event));
        if (!present && this.#events.deleteReconcileCandidate(event.id)) invalidated += 1;
      }
    }
    invalidated += this.#events.deleteInvalidatedOlderThan(INVALIDATED_GRACE_MS);
    this.#events.deleteSettledOlderThan(RECONCILE_SETTLED_TTL_MS);
    // Settled status event plans share the same 7-day TTL as settled agent
    // events: a drained/completed/cancelled/failed plan is pure bookkeeping.
    this.#statusEventPlans?.deleteSettledOlderThan(RECONCILE_SETTLED_TTL_MS);
    const { historyCacheExpired, historyCacheMissing } = this.#purgeHistoryCache();
    let released = 0;
    if (options.releaseStaleOwners !== false) {
      for (const scope of this.#scopes.listOwnedScopes()) {
        const panes = live.get(scope.herdrSessionName);
        if (!panes) continue;
        if (
          this.#scopes.releaseIfStaleOwner({
            herdrSessionName: scope.herdrSessionName,
            workspaceId: scope.workspaceId,
            livePaneIds: new Set(panes.map((pane) => pane.paneId)),
            liveTerminalIds: new Set(
              panes.flatMap((pane) =>
                pane.terminalId &&
                this.#connectedTerminal({
                  herdrSessionName: scope.herdrSessionName,
                  terminalId: pane.terminalId,
                })
                  ? [pane.terminalId]
                  : [],
              ),
            ),
          })
        ) {
          released += 1;
        }
      }
    }
    const purged = this.#scopes.purgeReleasedOlderThan(SCOPE_RELEASE_TTL_MS);
    for (const scope of this.#scopes.listOwnedScopes()) {
      if (scope.owner?.paneId) {
        this.#events.ackSelfOwned({
          herdrSessionName: scope.herdrSessionName,
          workspaceId: scope.workspaceId,
          paneId: scope.owner.paneId,
        });
      }
    }
    return { historyCacheExpired, historyCacheMissing, invalidated, purged, released };
  }

  #purgeHistoryCache(): { historyCacheExpired: number; historyCacheMissing: number } {
    if (!this.#agentHistoryCache) {
      return { historyCacheExpired: 0, historyCacheMissing: 0 };
    }
    const historyCacheExpired = this.#agentHistoryCache.deleteOlderThan(HISTORY_CACHE_TTL_MS);
    const missing = this.#agentHistoryCache
      .listSourcePaths()
      .filter((sourcePath) => !historyCacheSourceExists(sourcePath));
    const historyCacheMissing = this.#agentHistoryCache.deleteBySourcePaths(missing);
    return { historyCacheExpired, historyCacheMissing };
  }
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}

/**
 * Cache keys for opencode-sqlite append `#session=...` onto the backing file
 * path. Existence is checked against the real path, not the synthetic key.
 */
function historyCacheSourceExists(sourcePath: string): boolean {
  if (existsSync(sourcePath)) return true;
  const marker = "#session=";
  const index = sourcePath.indexOf(marker);
  return index > 0 && existsSync(sourcePath.slice(0, index));
}

/**
 * A live pane matches an event when the pane ids are equal and the
 * generations do not contradict each other. A generation-less live pane is
 * treated as alive (the pane is present in the snapshot, so its events must
 * not be swept even though the pane reports no generation); only two
 * different non-null generations mean the pane was re-created and the old
 * generation's events are stale.
 */
function paneMatchesEvent(pane: LivePane, event: AgentEventRecord): boolean {
  if (pane.paneId !== event.paneId) return false;
  if (event.paneGeneration === null) return true;
  if (pane.generation === null) {
    console.warn("Herdsman reconcile treating generation-less live pane as alive", {
      eventId: event.id,
      eventPaneGeneration: event.paneGeneration,
      herdrSessionName: event.herdrSessionName,
      paneId: event.paneId,
    });
    return true;
  }
  return pane.generation === event.paneGeneration;
}
