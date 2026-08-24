import { readFile, stat } from "node:fs/promises";
import type {
  AgentHistoryMessage,
  AgentHistoryRef,
  CompactAgentHistory,
} from "@/observability/contracts.js";

export type JsonlEntry = { line: number; value: Record<string, unknown> };

/** A writer can leave a partial final record while message_end is firing. */
export class UnstableJsonlError extends Error {
  constructor(readonly details: { malformedLines: number; size: number; mtimeMs: number }) {
    super(`JSONL tail is unstable (${details.malformedLines} malformed line(s))`);
    this.name = "UnstableJsonlError";
  }
}

export type AgentHistoryReader = {
  canRead(ref: AgentHistoryRef): boolean;
  read(ref: AgentHistoryRef, options: { limit?: number }): Promise<AgentHistoryMessage[]>;
  readCompact(ref: AgentHistoryRef): Promise<CompactAgentHistory>;
};

export async function readJsonl(path: string): Promise<JsonlEntry[]> {
  const [content, metadata] = await Promise.all([readFile(path, "utf8"), stat(path)]);
  const entries: JsonlEntry[] = [];
  const lines = content.split(/\r?\n/);
  let malformedLines = 0;
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index];
    if (!line || line.trim().length === 0) continue;
    try {
      const parsed = JSON.parse(line) as unknown;
      if (typeof parsed === "object" && parsed !== null) {
        entries.push({ line: index + 1, value: parsed as Record<string, unknown> });
      }
    } catch {
      malformedLines += 1;
    }
  }
  // Only the tail can be transient: malformed historical records remain
  // observable, but a malformed final record must not be mistaken for no data.
  const tail = lines.findLastIndex((line) => line.trim().length > 0);
  if (malformedLines > 0 && tail >= 0) {
    try {
      JSON.parse(lines[tail] ?? "");
    } catch {
      throw new UnstableJsonlError({
        malformedLines,
        size: metadata.size,
        mtimeMs: metadata.mtimeMs,
      });
    }
  }
  return entries;
}

export function compactFromMessages(
  ref: AgentHistoryRef,
  messages: AgentHistoryMessage[],
): CompactAgentHistory {
  const lastUser = lastByRole(messages, "user");
  const lastAssistant = lastByRole(messages, "assistant");
  const lastTool = [...messages].reverse().find((message) => message.role === "tool_result");
  return {
    historyRef: ref,
    lastAssistantMessage: lastAssistant ? excerpt(lastAssistant) : null,
    lastToolResult: lastTool?.compact ?? null,
    lastUserMessage: lastUser ? excerpt(lastUser) : null,
    messageCount: messages.length,
    source: ref.source,
    updatedAt: [...messages].reverse().find((message) => message.timestamp)?.timestamp ?? null,
  };
}

export function limitMessages(
  messages: AgentHistoryMessage[],
  limit: number | undefined,
): AgentHistoryMessage[] {
  if (!limit || messages.length <= limit) return messages;
  return messages.slice(messages.length - limit);
}

function lastByRole(
  messages: AgentHistoryMessage[],
  role: AgentHistoryMessage["role"],
): AgentHistoryMessage | undefined {
  return [...messages].reverse().find((message) => message.role === role);
}

function excerpt(message: AgentHistoryMessage) {
  return { ref: message.ref, text: message.text, timestamp: message.timestamp };
}
