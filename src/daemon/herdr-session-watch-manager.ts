import type { AgentStore } from "@/db/agents.js";
import type { HerdrSessionStore } from "@/db/herdr-sessions.js";
import type { HerdrSessionListEntry, HerdrSessionListRunner } from "@/herdr/session-list.js";
import { HERDR_TOPOLOGY_EVENT_TYPES, HerdrSocketClient } from "@/herdr/socket-client.js";
import type {
  AgentIndexRefreshFastResult,
  AgentIndexRefreshResult,
  AgentIndexService,
  StatusEventPlan,
} from "@/observability/agent-index-service.js";
import type { AgentEventRecord, AgentIndexRecord, AgentScope } from "@/observability/contracts.js";

export const ACTIVE_REVISION_POLL_MS = 10_000;
export const FULL_RESCAN_MS = 60_000;
export const PLAN_DRAIN_GRACE_MS = 12_000;

const VALID_AGENT_EVENT_TYPES = new Set<string>([
  "agent.status.changed",
  "agent.idle",
  "agent.done",
  "agent.blocked",
  "agent.failed",
  "agent.discarded",
]);

type Client = Pick<HerdrSocketClient, "close" | "subscribeEvents">;

type Watcher = {
  abort: AbortController;
  client: Client;
  entry: HerdrSessionListEntry;
  loop: Promise<void>;
  restartSubscription?: () => void;
  subscribedPaneIds: string[];
};

export class HerdrSessionWatchManager {
  readonly #agents: AgentStore;
  readonly #activeRevisionPollMs: number;
  readonly #clientFactory: (input: {
    socketPath: string;
  }) => Pick<HerdrSocketClient, "close" | "subscribeEvents">;
  readonly #fullRescanMs: number;
  readonly #herdrSessions: HerdrSessionStore;
  readonly #index: AgentIndexService;
  readonly #onAgentContextChanged: (scope: AgentScope) => void;
  readonly #onAgentEvent: (event: AgentEventRecord) => void;
  readonly #onAgentIndexRefreshed: (input: {
    agents: AgentIndexRecord[];
    herdrSessionName: string;
  }) => void;
  readonly #reconnectDelayMs: number;
  readonly #refreshPublications = new WeakMap<
    Promise<AgentIndexRefreshFastResult | AgentIndexRefreshResult>,
    Promise<AgentIndexRecord[]>
  >();
  readonly #retiringWatcherLoops = new Set<Promise<void>>();
  readonly #inFlightPlans = new Set<Promise<void>>();
  readonly #sessionList: HerdrSessionListRunner;
  readonly #watchers = new Map<string, Watcher>();
  #lastFullRescanAt = 0;
  #lifecycleGeneration = 0;
  #scheduler: NodeJS.Timeout | undefined;
  #stopping = false;
  #tickInFlight: Promise<void> | undefined;

  constructor(options: {
    activeRevisionPollMs?: number;
    agents: AgentStore;
    clientFactory?: (input: {
      socketPath: string;
    }) => Pick<HerdrSocketClient, "close" | "subscribeEvents">;
    fullRescanMs?: number;
    herdrSessions: HerdrSessionStore;
    index: AgentIndexService;
    onAgentContextChanged?(scope: AgentScope): void;
    onAgentEvent(event: AgentEventRecord): void;
    onAgentIndexRefreshed?(input: { agents: AgentIndexRecord[]; herdrSessionName: string }): void;
    reconnectDelayMs?: number;
    sessionList: HerdrSessionListRunner;
  }) {
    this.#activeRevisionPollMs = options.activeRevisionPollMs ?? ACTIVE_REVISION_POLL_MS;
    this.#agents = options.agents;
    this.#clientFactory = options.clientFactory ?? ((input) => new HerdrSocketClient(input));
    this.#fullRescanMs = options.fullRescanMs ?? FULL_RESCAN_MS;
    this.#herdrSessions = options.herdrSessions;
    this.#index = options.index;
    this.#onAgentContextChanged = options.onAgentContextChanged ?? (() => undefined);
    this.#onAgentEvent = options.onAgentEvent;
    this.#onAgentIndexRefreshed = options.onAgentIndexRefreshed ?? (() => undefined);
    this.#reconnectDelayMs = options.reconnectDelayMs ?? 1_000;
    this.#sessionList = options.sessionList;
  }

  async start(): Promise<void> {
    this.#stopping = false;
    this.#lifecycleGeneration += 1;
    const generation = this.#lifecycleGeneration;
    await this.rescanNow();
    if (this.#stopping || generation !== this.#lifecycleGeneration) return;
    this.#scheduler = setInterval(() => {
      if (!this.#tickInFlight) {
        this.#tickInFlight = this.#tick().finally(() => {
          this.#tickInFlight = undefined;
        });
      }
    }, this.#activeRevisionPollMs);
  }

  async stop(): Promise<void> {
    this.#stopping = true;
    this.#lifecycleGeneration += 1;
    if (this.#scheduler) clearInterval(this.#scheduler);
    this.#scheduler = undefined;
    await this.#abortWatchers();
    await this.#tickInFlight?.catch(() => undefined);
    await Promise.all([...this.#retiringWatcherLoops]);
    if (this.#inFlightPlans.size > 0) {
      const controller = new AbortController();
      const timeout = delay(PLAN_DRAIN_GRACE_MS, controller.signal);
      await Promise.race([Promise.all([...this.#inFlightPlans]), timeout]).finally(() => {
        controller.abort();
      });
    }
    await this.#abortWatchers();
  }

  async rescanNow(): Promise<void> {
    const generation = this.#lifecycleGeneration;
    const sessions = await this.#sessionList();
    if (this.#stopping || generation !== this.#lifecycleGeneration) return;
    const running = sessions.filter((session) => session.running);
    const runningNames = new Set(running.map((session) => session.name));
    const removed: Promise<void>[] = [];

    for (const [name, watcher] of this.#watchers) {
      if (!runningNames.has(name)) removed.push(this.#retireWatcher(name, watcher));
    }
    await Promise.all(removed);
    if (this.#stopping || generation !== this.#lifecycleGeneration) return;
    this.#herdrSessions.markStoppedMissingFrom([...runningNames]);

    for (const entry of running) {
      if (this.#stopping || generation !== this.#lifecycleGeneration) return;
      const existing = this.#watchers.get(entry.name);
      if (existing) await this.#retireWatcher(entry.name, existing);
      await this.#startWatcher(entry, generation);
    }
    if (!this.#stopping && generation === this.#lifecycleGeneration) {
      this.#lastFullRescanAt = Date.now();
    }
  }

  async #tick(): Promise<void> {
    if (Date.now() - this.#lastFullRescanAt >= this.#fullRescanMs) {
      await this.rescanNow();
      return;
    }
    const workingSessions = [...this.#watchers.values()].filter(({ entry }) =>
      this.#agents.listForHerdrSession(entry.name).some((agent) => agent.agentStatus === "working"),
    );
    await mapConcurrent(workingSessions, 4, async (watcher) => {
      const agents = await this.#refresh(watcher.entry);
      if (containsNewPaneId(agents, watcher.subscribedPaneIds)) {
        watcher.restartSubscription?.();
      }
    });
  }

  async #startWatcher(entry: HerdrSessionListEntry, generation: number): Promise<void> {
    if (this.#stopping || generation !== this.#lifecycleGeneration) return;
    this.#herdrSessions.upsertRunning({
      name: entry.name,
      sessionDir: entry.sessionDir,
      socketPath: entry.socketPath,
    });
    const abort = new AbortController();
    const client = this.#clientFactory({ socketPath: entry.socketPath });
    const watcher: Watcher = {
      abort,
      client,
      entry,
      loop: Promise.resolve(),
      subscribedPaneIds: [],
    };
    watcher.loop = this.#watch(entry, watcher, generation, abort.signal).catch(() => undefined);
    this.#watchers.set(entry.name, watcher);
    if (this.#stopping || generation !== this.#lifecycleGeneration) {
      abort.abort();
      client.close();
      this.#watchers.delete(entry.name);
      await watcher.loop;
    }
  }

  #retireWatcher(name: string, watcher: Watcher): Promise<void> {
    if (this.#watchers.get(name) === watcher) this.#watchers.delete(name);
    watcher.abort.abort();
    watcher.client.close();
    this.#retiringWatcherLoops.add(watcher.loop);
    const clear = () => this.#retiringWatcherLoops.delete(watcher.loop);
    void watcher.loop.then(clear, clear);
    return watcher.loop;
  }

  async #abortWatchers(): Promise<void> {
    const retiring = [...this.#watchers].map(([name, watcher]) =>
      this.#retireWatcher(name, watcher),
    );
    await Promise.all([...retiring, ...this.#retiringWatcherLoops]);
  }

  async #watch(
    entry: HerdrSessionListEntry,
    watcher: Watcher,
    generation: number,
    signal: AbortSignal,
  ): Promise<void> {
    let reconnectCount = 0;
    let lastPaneIds: string[] = [];
    let lastEvent: Record<string, unknown> | undefined;
    let lastClosedTriggered = false;
    let restartAgents: AgentIndexRecord[] | undefined;
    while (!signal.aborted && generation === this.#lifecycleGeneration) {
      let restart = false;
      const subscriptionAbort = new AbortController();
      const stopSubscription = () => subscriptionAbort.abort();
      signal.addEventListener("abort", stopSubscription, { once: true });
      watcher.restartSubscription = () => {
        restart = true;
        subscriptionAbort.abort();
      };
      try {
        if (signal.aborted) return;
        const agents = restartAgents ?? (await this.#refresh(entry));
        restartAgents = undefined;
        reconnectCount = 0;
        if (signal.aborted) return;
        const paneIds = agents.map((agent) => agent.paneId);
        lastPaneIds = paneIds;
        watcher.subscribedPaneIds = paneIds;
        for await (const event of watcher.client.subscribeEvents(
          { paneIds },
          { signal: subscriptionAbort.signal },
        )) {
          if (signal.aborted) return;
          const eventRecord = record(event);
          reconnectCount = 0;
          lastEvent = eventRecord;
          if (eventRecord.type === "pane.agent_status_changed") {
            await this.#handleWatchEvent(entry, event);
            continue;
          }
          if (!isTopologyEvent(eventRecord.type)) continue;
          const refreshed = await this.#refresh(entry);
          lastPaneIds = refreshed.map((agent) => agent.paneId);
          if (eventRecord.type === "pane.closed" && !closedHitsLivePane(eventRecord, refreshed)) {
            await this.#handleWatchEvent(entry, event);
          }
          if (containsNewPaneId(refreshed, watcher.subscribedPaneIds)) {
            restart = true;
            restartAgents = refreshed;
            lastClosedTriggered = eventRecord.type === "pane.closed";
            subscriptionAbort.abort();
          }
        }
      } catch (error) {
        if (signal.aborted) return;
        if (!restart) {
          reconnectCount += 1;
          const reconnectDelayMs = Math.min(
            this.#reconnectDelayMs * 2 ** (reconnectCount - 1),
            30_000,
          );
          console.warn("Herdsman Herdr subscription reconnect", {
            sessionName: entry.name,
            socketPath: entry.socketPath,
            paneIds: lastPaneIds,
            subscriptionGeneration: this.#lifecycleGeneration,
            eventId: lastEvent?.id ?? lastEvent?.event_id ?? null,
            revision: lastEvent?.revision ?? lastEvent?.pane_revision ?? null,
            reconnectCount,
            paneClosedTriggered: lastClosedTriggered,
            error: error instanceof Error ? error.message : String(error),
          });
          lastClosedTriggered = false;
          restartAgents = undefined;
          if (signal.aborted) return;
          this.#replaceClient(watcher, entry.socketPath);
          await delay(reconnectDelayMs, signal);
          continue;
        }
      } finally {
        signal.removeEventListener("abort", stopSubscription);
      }
      if (signal.aborted) return;
      this.#replaceClient(watcher, entry.socketPath);
      if (restart) {
        reconnectCount = 0;
        continue;
      }
      reconnectCount = 0;
      await delay(this.#reconnectDelayMs, signal);
    }
  }

  #replaceClient(watcher: Watcher, socketPath: string): void {
    watcher.client.close();
    watcher.client = this.#clientFactory({ socketPath });
  }

  async #handleWatchEvent(entry: HerdrSessionListEntry, event: unknown): Promise<void> {
    const result = this.#index.handleHerdrEventFast
      ? await this.#index.handleHerdrEventFast({
          event,
          herdrSessionName: entry.name,
          sessionDir: entry.sessionDir,
          socketPath: entry.socketPath,
        })
      : await this.#index.handleHerdrEvent({
          event,
          herdrSessionName: entry.name,
          sessionDir: entry.sessionDir,
          socketPath: entry.socketPath,
        });
    this.#publishResult({
      agents: this.#agents.listForHerdrSession(entry.name),
      herdrSessionName: entry.name,
      ...result,
    });
    if ("statusEventPlans" in result && Array.isArray(result.statusEventPlans)) {
      for (const plan of result.statusEventPlans) {
        this.#submitPlan(plan);
      }
    }
  }

  #refresh(entry: HerdrSessionListEntry): Promise<AgentIndexRecord[]> {
    const source = (
      this.#index.refreshHerdrSessionFast
        ? this.#index.refreshHerdrSessionFast({
            herdrSessionName: entry.name,
            sessionDir: entry.sessionDir,
            socketPath: entry.socketPath,
          })
        : this.#index.refreshHerdrSession({
            herdrSessionName: entry.name,
            sessionDir: entry.sessionDir,
            socketPath: entry.socketPath,
          })
    ) as Promise<AgentIndexRefreshFastResult | AgentIndexRefreshResult>;
    const existing = this.#refreshPublications.get(source);
    if (existing) return existing;
    const publication = source.then((result) => {
      this.#publishResult({ herdrSessionName: entry.name, ...result });
      if ("statusEventPlans" in result && Array.isArray(result.statusEventPlans)) {
        for (const plan of result.statusEventPlans) {
          this.#submitPlan(plan);
        }
      }
      return result.agents;
    });
    this.#refreshPublications.set(source, publication);
    return publication;
  }

  #submitPlan(plan: StatusEventPlan): void {
    const execute = this.#index.executeStatusEventPlan?.(plan);
    if (!execute) return;
    const task: Promise<void> = Promise.resolve(execute)
      .then((planEvent) => {
        if (planEvent && VALID_AGENT_EVENT_TYPES.has(planEvent.type)) {
          this.#onAgentEvent(planEvent);
        }
      })
      .catch((error) => {
        console.warn("Herdsman status event plan failed", {
          agentId: plan.agent.id,
          error,
          from: plan.from,
          to: plan.to,
        });
      })
      .finally(() => {
        this.#inFlightPlans.delete(task);
      });
    this.#inFlightPlans.add(task);
  }

  #publishResult(input: {
    agents: AgentIndexRecord[];
    contextChangedScopes: AgentScope[];
    events: AgentEventRecord[];
    herdrSessionName: string;
  }): void {
    this.#onAgentIndexRefreshed({ agents: input.agents, herdrSessionName: input.herdrSessionName });
    for (const scope of input.contextChangedScopes) this.#onAgentContextChanged(scope);
    for (const event of input.events) this.#onAgentEvent(event);
  }
}

async function delay(ms: number, signal: AbortSignal): Promise<void> {
  if (ms <= 0 || signal.aborted) return;
  await new Promise<void>((resolve) => {
    const timeout = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(timeout);
        resolve();
      },
      { once: true },
    );
  });
}

function isTopologyEvent(type: unknown): boolean {
  return (HERDR_TOPOLOGY_EVENT_TYPES as readonly string[]).includes(type as string);
}

function closedHitsLivePane(
  event: Record<string, unknown>,
  agents: ReadonlyArray<{ paneId: string; paneGeneration?: string | null }>,
): boolean {
  const paneId = stringValue(event.pane_id) ?? stringValue(event.paneId);
  if (!paneId) return false;
  const generation = stringValue(event.pane_generation) ?? stringValue(event.paneGeneration);
  return agents.some(
    (agent) =>
      agent.paneId === paneId && (generation == null || agent.paneGeneration === generation),
  );
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}

function containsNewPaneId(
  agents: ReadonlyArray<{ paneId: string }>,
  subscribedPaneIds: readonly string[],
): boolean {
  if (agents.length === 0) return false;
  const subscribed = new Set(subscribedPaneIds);
  return agents.some((agent) => !subscribed.has(agent.paneId));
}

function record(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

async function mapConcurrent<T, R>(
  items: T[],
  limit: number,
  fn: (item: T) => Promise<R>,
): Promise<R[]> {
  const results: R[] = [];
  for (let i = 0; i < items.length; i += limit) {
    const chunk = items.slice(i, i + limit);
    const chunkResults = await Promise.all(chunk.map(fn));
    results.push(...chunkResults);
  }
  return results;
}
