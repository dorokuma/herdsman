import { stat } from "node:fs/promises";
import type { AgentHistoryCacheStore } from "@/db/agent-history-cache.js";
import type {
  AgentHistoryMessage,
  AgentHistoryRef,
  AgentHistorySourceFingerprint,
  CompactAgentHistory,
} from "@/observability/contracts.js";
import { AntigravityHistoryReader } from "./antigravity-reader.js";
import {
  type AgentHistoryLookupInput,
  discoverAgentHistory,
  discoveryFailureFresh,
} from "./discovery.js";
import { PiHistoryReader } from "./pi-reader.js";
import type { AgentHistoryReader } from "./readers.js";
import { statSourceFingerprint } from "./source-fingerprint.js";

export const agentHistoryFormatterVersion = "agent-history-v3";

type CacheLike = Pick<AgentHistoryCacheStore, "getFresh" | "put">;
type Discovery = (
  input: AgentHistoryLookupInput,
  options?: { force?: boolean },
) => Promise<AgentHistoryRef | null>;

export type ResolvedCompactAgentHistory = {
  compactHistory: CompactAgentHistory;
  historyRef: AgentHistoryRef | null;
  sourceFingerprint: AgentHistorySourceFingerprint | null;
};

export function createAgentHistoryService(
  options: {
    cache?: CacheLike;
    discover?: Discovery;
    homeDir?: string;
    readers?: AgentHistoryReader[];
  } = {},
) {
  // Exactly two agents have a reader: pi (official session file path) and agy
  // (official conversation id). Everything else resolves to `unknown` and is
  // rejected here, so a pane for another agent reports no history.
  const readers = options.readers ?? [new PiHistoryReader(), new AntigravityHistoryReader()];
  const withHomeDir = (input: AgentHistoryLookupInput): AgentHistoryLookupInput =>
    options.homeDir ? { ...input, homeDir: options.homeDir } : input;
  const discover: Discovery =
    options.discover ??
    ((input, discoverOptions) => discoverAgentHistory(withHomeDir(input), discoverOptions));

  /**
   * A pane whose official values just failed to resolve is not looked up again
   * (and not warned about again) until its retry window expires: the failure is
   * already reported once by `discoverAgentHistory`. Only the real discovery has
   * that memory, so an injected one keeps its own behaviour. The lookup key is
   * computed from the same input the discovery sees, `homeDir` included.
   *
   * This deceleration is for the ordinary per-round cadence only. Callers that
   * pass `forceDiscovery` (an explicit operator read, or an external forced
   * refresh) retry regardless. Warn dedup stays keyed, so a forced retry never
   * duplicates a warning.
   */
  const discoverySuppressed = (input: AgentHistoryLookupInput): boolean =>
    options.discover === undefined && discoveryFailureFresh(withHomeDir(input));

  async function readCompactRef(
    historyRef: AgentHistoryRef,
    readCompactOptions: { forceRefresh?: boolean | undefined } = {},
  ): Promise<ResolvedCompactAgentHistory> {
    const reader = readers.find((candidate) => candidate.canRead(historyRef));
    if (!reader) return unresolvedCompactHistory(historyRef.source);

    const path = historyRef.path ?? historyRef.value;
    const sourceFingerprint = await statSourceFingerprint(path);
    if (!sourceFingerprint) return unresolvedCompactHistory(historyRef.source);

    if (!readCompactOptions.forceRefresh) {
      const cached = options.cache?.getFresh({
        formatterVersion: agentHistoryFormatterVersion,
        sourceMtimeMs: sourceFingerprint.mtimeMs,
        sourcePath: path,
        sourceSize: sourceFingerprint.size,
      });
      if (cached && cached.compactHistory.lastAssistantMessage !== null) {
        return { compactHistory: cached.compactHistory, historyRef, sourceFingerprint };
      }
    }

    try {
      const compactHistory = await reader.readCompact(historyRef);
      if (compactHistory.lastAssistantMessage === null) {
        console.warn("Herdsman history read produced no assistant message", {
          path,
          source: historyRef.source,
          size: sourceFingerprint.size,
          mtimeMs: sourceFingerprint.mtimeMs,
        });
        return { compactHistory, historyRef, sourceFingerprint };
      }
      options.cache?.put({
        compactHistory,
        formatterVersion: agentHistoryFormatterVersion,
        historyRef,
        sourceMtimeMs: sourceFingerprint.mtimeMs,
        sourcePath: path,
        sourceSize: sourceFingerprint.size,
      });
      return { compactHistory, historyRef, sourceFingerprint };
    } catch (error) {
      console.warn("Herdsman could not read agent history", {
        path,
        source: historyRef.source,
        error: error instanceof Error ? error.message : String(error),
        size: sourceFingerprint.size,
        mtimeMs: sourceFingerprint.mtimeMs,
      });
      return unresolvedCompactHistory(historyRef.source);
    }
  }

  async function resolveCompactHistory(
    input: AgentHistoryLookupInput,
    resolveOptions: {
      forceDiscovery?: boolean;
      forceRefresh?: boolean;
      preferredRef?: AgentHistoryRef | null;
    } = {},
  ): Promise<ResolvedCompactAgentHistory> {
    if (resolveOptions.preferredRef && !resolveOptions.forceDiscovery) {
      const preferred = await readCompactRef(
        resolveOptions.preferredRef,
        resolveOptions.forceRefresh !== undefined
          ? { forceRefresh: resolveOptions.forceRefresh }
          : {},
      );
      if (preferred.historyRef) return preferred;
    }
    // The failure memory only decelerates the ordinary refresh cadence: it skips
    // the lookup the daemon would otherwise repeat every round. An explicit
    // caller action (`forceDiscovery`) must get a real lookup instead, so it
    // bypasses both this skip and the memory inside `discoverAgentHistory`. The
    // warning is still deduplicated per key, so this adds no noise.
    const forcedDiscovery = resolveOptions.forceDiscovery === true;
    if (!forcedDiscovery && discoverySuppressed(input)) {
      return unresolvedCompactHistory();
    }
    const historyRef = await discover(input, forcedDiscovery ? { force: true } : {});
    if (!historyRef) {
      // `discoverAgentHistory` already warned about this failure with the richer
      // official values, so the real path must not log a second warning for the
      // same round. An injected discovery has no such report, so keep this one.
      if (options.discover !== undefined) {
        console.warn("Herdsman agent history discovery returned no reference", input);
      }
      return unresolvedCompactHistory();
    }
    return readCompactRef(
      historyRef,
      resolveOptions.forceRefresh !== undefined
        ? { forceRefresh: resolveOptions.forceRefresh }
        : {},
    );
  }

  async function readRef(
    historyRef: AgentHistoryRef,
    readOptions: { limit: number },
  ): Promise<{ historyRef: AgentHistoryRef | null; messages: AgentHistoryMessage[] }> {
    const reader = readers.find((candidate) => candidate.canRead(historyRef));
    const path = historyRef.path ?? historyRef.value;
    if (!reader || !(await stat(path).catch(() => null))) {
      console.warn("Herdsman history reference could not be resolved", {
        path,
        source: historyRef.source,
      });
      return { historyRef: null, messages: [] };
    }
    try {
      return { historyRef, messages: await reader.read(historyRef, readOptions) };
    } catch (error) {
      console.warn("Herdsman could not read agent history messages", {
        path,
        source: historyRef.source,
        error: error instanceof Error ? error.message : String(error),
      });
      return { historyRef: null, messages: [] };
    }
  }

  return {
    discover,
    getCompactHistory: async (input: AgentHistoryLookupInput): Promise<CompactAgentHistory> =>
      (await resolveCompactHistory(input)).compactHistory,
    readCompactRef,
    resolveCompactHistory,
    async read(
      input: AgentHistoryLookupInput,
      readOptions: {
        forceDiscovery?: boolean | undefined;
        limit: number;
        preferredRef?: AgentHistoryRef | null;
      },
    ): Promise<{ historyRef: AgentHistoryRef | null; messages: AgentHistoryMessage[] }> {
      // `forceDiscovery` means "this caller asked right now": skip the persisted
      // ref like `resolveCompactHistory` does, and do not let the failure memory
      // answer with a memoized empty history.
      if (readOptions.preferredRef && !readOptions.forceDiscovery) {
        const preferred = await readRef(readOptions.preferredRef, readOptions);
        if (preferred.historyRef) return preferred;
      }
      if (!readOptions.forceDiscovery && discoverySuppressed(input)) {
        return { historyRef: null, messages: [] };
      }
      const historyRef = await discover(input, readOptions.forceDiscovery ? { force: true } : {});
      if (!historyRef) return { historyRef: null, messages: [] };
      return readRef(historyRef, readOptions);
    },
  };
}

function unresolvedCompactHistory(source: string | null = null): ResolvedCompactAgentHistory {
  return {
    compactHistory: emptyCompactHistory(source),
    historyRef: null,
    sourceFingerprint: null,
  };
}

export type AgentHistoryService = ReturnType<typeof createAgentHistoryService>;

export function emptyCompactHistory(source: string | null = null): CompactAgentHistory {
  return {
    historyRef: null,
    lastAssistantMessage: null,
    lastToolResult: null,
    lastUserMessage: null,
    messageCount: 0,
    source,
    updatedAt: null,
  };
}
