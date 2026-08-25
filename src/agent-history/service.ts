import { stat } from "node:fs/promises";
import type { AgentHistoryCacheStore } from "@/db/agent-history-cache.js";
import type {
  AgentHistoryMessage,
  AgentHistoryRef,
  AgentHistorySourceFingerprint,
  CompactAgentHistory,
} from "@/observability/contracts.js";
import { AntigravityHistoryReader } from "./antigravity-reader.js";
import { ClaudeHistoryReader } from "./claude-reader.js";
import { CodexHistoryReader } from "./codex-reader.js";
import { type AgentHistoryLookupInput, discoverAgentHistory } from "./discovery.js";
import { GeminiHistoryReader } from "./gemini-reader.js";
import { GrokHistoryReader } from "./grok-reader.js";
import { OpenCodeHistoryReader } from "./opencode-reader.js";
import { PiHistoryReader } from "./pi-reader.js";
import type { AgentHistoryReader } from "./readers.js";

export const agentHistoryFormatterVersion = "agent-history-v1";

type CacheLike = Pick<AgentHistoryCacheStore, "getFresh" | "put">;
type Discovery = (input: AgentHistoryLookupInput) => Promise<AgentHistoryRef | null>;

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
  const readers = options.readers ?? [
    new PiHistoryReader(),
    new ClaudeHistoryReader(),
    new CodexHistoryReader(),
    new OpenCodeHistoryReader(),
    new GeminiHistoryReader(),
    new AntigravityHistoryReader(),
    new GrokHistoryReader(),
  ];
  const discover: Discovery =
    options.discover ??
    ((input) =>
      discoverAgentHistory({
        ...input,
        ...(options.homeDir ? { homeDir: options.homeDir } : {}),
      }));

  async function readCompactRef(historyRef: AgentHistoryRef): Promise<ResolvedCompactAgentHistory> {
    const reader = readers.find((candidate) => candidate.canRead(historyRef));
    if (!reader) return unresolvedCompactHistory(historyRef.source);

    const path = historyRef.path ?? historyRef.value;
    const stats = await stat(path).catch(() => null);
    if (!stats) return unresolvedCompactHistory(historyRef.source);

    const sourceFingerprint = {
      mtimeMs: Math.trunc(stats.mtimeMs),
      path,
      size: stats.size,
    };
    const cacheSourcePath = cacheSourcePathForRef(historyRef);
    const cached = options.cache?.getFresh({
      formatterVersion: agentHistoryFormatterVersion,
      sourceMtimeMs: sourceFingerprint.mtimeMs,
      sourcePath: cacheSourcePath,
      sourceSize: sourceFingerprint.size,
    });
    if (cached && cached.compactHistory.lastAssistantMessage !== null) {
      return { compactHistory: cached.compactHistory, historyRef, sourceFingerprint };
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
        sourcePath: cacheSourcePath,
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
    if (
      !resolveOptions.forceRefresh &&
      resolveOptions.preferredRef &&
      !resolveOptions.forceDiscovery
    ) {
      const preferred = await readCompactRef(resolveOptions.preferredRef);
      if (preferred.historyRef) return preferred;
    }
    const historyRef = await discover(input);
    if (!historyRef) {
      console.warn("Herdsman agent history discovery returned no reference", input);
      return unresolvedCompactHistory();
    }
    return readCompactRef(historyRef);
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
      readOptions: { limit: number; preferredRef?: AgentHistoryRef | null },
    ): Promise<{ historyRef: AgentHistoryRef | null; messages: AgentHistoryMessage[] }> {
      if (readOptions.preferredRef) {
        const preferred = await readRef(readOptions.preferredRef, readOptions);
        if (preferred.historyRef) return preferred;
      }
      const historyRef = await discover(input);
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

export function cacheSourcePathForRef(historyRef: {
  kind?: string;
  path?: string;
  source: string;
  value: string;
}): string {
  const path = historyRef.path ?? historyRef.value;
  return historyRef.source === "opencode-sqlite" ? `${path}#session=${historyRef.value}` : path;
}

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
