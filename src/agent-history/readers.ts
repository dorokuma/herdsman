import { createReadStream } from "node:fs";
import { stat } from "node:fs/promises";
import { createInterface } from "node:readline";
import type {
  AgentHistoryMessage,
  AgentHistoryRef,
  CompactAgentHistory,
} from "@/observability/contracts.js";
import { sanitizeText } from "./text.js";

export type JsonlEntry = { line: number; value: Record<string, unknown> };

/** Cap JSONL reads; larger files are consumed from a tail window of this size. */
export const JSONL_MAX_BYTES = 32 * 1024 * 1024;

/** A writer can leave a partial final record while message_end is firing. */
export class UnstableJsonlError extends Error {
  constructor(readonly details: { malformedLines: number; size: number; mtimeMs: number }) {
    super(`JSONL tail is unstable (${details.malformedLines} malformed line(s))`);
    this.name = "UnstableJsonlError";
  }
}

export class JsonlTooLargeError extends Error {
  constructor(readonly details: { maxBytes: number; size: number }) {
    super(`JSONL file exceeds maximum size (${details.size} > ${details.maxBytes} bytes)`);
    this.name = "JsonlTooLargeError";
  }
}

export type AgentHistoryReader = {
  canRead(ref: AgentHistoryRef): boolean;
  read(ref: AgentHistoryRef, options: { limit?: number }): Promise<AgentHistoryMessage[]>;
  readCompact(ref: AgentHistoryRef): Promise<CompactAgentHistory>;
};

export async function readJsonl(
  path: string,
  options: { maxBytes?: number } = {},
): Promise<JsonlEntry[]> {
  const maxBytes = options.maxBytes ?? JSONL_MAX_BYTES;
  const metadata = await stat(path);
  const start = metadata.size > maxBytes ? metadata.size - maxBytes : 0;
  if (start > 0) {
    console.warn("Herdsman reading JSONL tail window to stay within size cap", {
      maxBytes,
      path,
      size: metadata.size,
    });
  }

  const input = createReadStream(path, { encoding: "utf8", start });
  let streamError: Error | undefined;
  input.on("error", (error: Error) => {
    streamError = error;
  });
  const lines = createInterface({ crlfDelay: Infinity, input });
  const entries: JsonlEntry[] = [];
  let malformedLines = 0;
  let lineNumber = 0;
  let skipPartialLead = start > 0;
  let tailMalformed = false;

  try {
    for await (const line of lines) {
      if (skipPartialLead) {
        skipPartialLead = false;
        continue;
      }
      lineNumber += 1;
      if (!line || line.trim().length === 0) continue;
      try {
        const parsed = JSON.parse(line) as unknown;
        if (typeof parsed === "object" && parsed !== null) {
          entries.push({ line: lineNumber, value: parsed as Record<string, unknown> });
        }
        tailMalformed = false;
      } catch {
        malformedLines += 1;
        tailMalformed = true;
      }
    }
  } finally {
    lines.close();
    input.destroy();
  }

  if (streamError) throw streamError;

  // Only the tail can be transient: malformed historical records remain
  // observable, but a malformed final record must not be mistaken for no data.
  if (malformedLines > 0 && tailMalformed) {
    throw new UnstableJsonlError({
      malformedLines,
      size: metadata.size,
      mtimeMs: metadata.mtimeMs,
    });
  }
  // A single record larger than the tail window is skipped as a partial lead,
  // which would otherwise look like an empty history with no error.
  if (start > 0 && entries.length === 0) {
    throw new JsonlTooLargeError({ maxBytes, size: metadata.size });
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
  const selected =
    !limit || messages.length <= limit ? messages : messages.slice(messages.length - limit);
  return selected.map(sanitizeHistoryMessage);
}

function lastByRole(
  messages: AgentHistoryMessage[],
  role: AgentHistoryMessage["role"],
): AgentHistoryMessage | undefined {
  return [...messages].reverse().find((message) => message.role === role);
}

function excerpt(message: AgentHistoryMessage) {
  return { ref: message.ref, text: sanitizeText(message.text).text, timestamp: message.timestamp };
}

function sanitizeHistoryMessage(message: AgentHistoryMessage): AgentHistoryMessage {
  const text = sanitizeText(message.text).text;
  return text === message.text ? message : { ...message, text };
}
