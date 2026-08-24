import type { AgentEventStore } from "@/db/agent-events.js";
import type { AgentOrchestratorScopeStore } from "@/db/agent-orchestrator-scopes.js";
import type { HerdrSessionListEntry, HerdrSessionListRunner } from "@/herdr/session-list.js";
import { normalizeHerdrSessionSnapshot } from "@/herdr/session-snapshot.js";
import { HerdrSocketClient } from "@/herdr/socket-client.js";

export const RECONCILE_BATCH_LIMIT = 100;
export const RECONCILE_REASON = "PANE_NOT_PRESENT_RECONCILE";

type LivePane = { paneId: string; generation: string | null; terminalId: string | null };

export class AgentEventReconciler {
  readonly #events: AgentEventStore;
  readonly #scopes: AgentOrchestratorScopeStore;
  readonly #sessionList: HerdrSessionListRunner;
  readonly #connectedTerminal: (input: { herdrSessionName: string; terminalId: string }) => boolean;
  readonly #clientFactory: (entry: HerdrSessionListEntry) => HerdrSocketClient;

  constructor(options: {
    events: AgentEventStore;
    scopes: AgentOrchestratorScopeStore;
    sessionList: HerdrSessionListRunner;
    clientFactory?: (entry: HerdrSessionListEntry) => HerdrSocketClient;
    connectedTerminal?: (input: { herdrSessionName: string; terminalId: string }) => boolean;
  }) {
    this.#events = options.events;
    this.#scopes = options.scopes;
    this.#sessionList = options.sessionList;
    this.#clientFactory =
      options.clientFactory ?? ((entry) => new HerdrSocketClient({ socketPath: entry.socketPath }));
    this.#connectedTerminal = options.connectedTerminal ?? (() => true);
  }

  async reconcile(): Promise<{ invalidated: number; released: number }> {
    let sessions: HerdrSessionListEntry[];
    try {
      sessions = await this.#sessionList();
    } catch (error) {
      console.warn("Herdsman reconcile skipped: Herdr session list unavailable", error);
      return { invalidated: 0, released: 0 };
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
      return { invalidated: 0, released: 0 };
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
        const present = panes.some(
          (pane) =>
            pane.paneId === event.paneId &&
            (event.paneGeneration === null || pane.generation === event.paneGeneration),
        );
        if (!present && this.#events.deleteReconcileCandidate(event.id)) invalidated += 1;
      }
    }
    invalidated += this.#events.deleteInvalidated();
    let released = 0;
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
      )
        released += 1;
    }
    return { invalidated, released };
  }
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}
