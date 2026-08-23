import type { AgentHistoryMessage, AgentHistoryRef } from "@/observability/contracts.js";
import {
  type AgentHistoryReader,
  compactFromMessages,
  limitMessages,
  readJsonl,
} from "./readers.js";
import { messageRef, timestampFrom } from "./text.js";

/** Grok's pane-visible transcript is chat_history.jsonl; updates.jsonl is not selected here. */
export class GrokHistoryReader implements AgentHistoryReader {
  canRead(ref: AgentHistoryRef): boolean {
    return ref.source === "grok-jsonl" && Boolean(ref.path ?? ref.value);
  }

  async read(
    ref: AgentHistoryRef,
    options: { limit?: number } = {},
  ): Promise<AgentHistoryMessage[]> {
    const path = ref.path ?? ref.value;
    const messages: AgentHistoryMessage[] = [];
    for (const entry of await readJsonl(path)) {
      const role =
        entry.value.type === "user" || entry.value.type === "assistant" ? entry.value.type : null;
      if (!role) continue;
      // Deliberately inspect only the ordinary content field. In particular,
      // reasoning.encrypted_content must never enter the observable history.
      const text = grokText(entry.value.content);
      if (!text) continue;
      messages.push({
        ref: messageRef(
          path,
          typeof entry.value.id === "string" ? entry.value.id : undefined,
          entry.line,
        ),
        role,
        text,
        timestamp: timestampFrom(entry.value.timestamp) ?? timestampFrom(entry.value.created_at),
      });
    }
    return limitMessages(messages, options.limit);
  }

  async readCompact(ref: AgentHistoryRef) {
    return compactFromMessages(ref, await this.read(ref));
  }
}

function grokText(value: unknown): string | null {
  if (typeof value === "string") return value;
  if (!Array.isArray(value)) return null;
  const parts = value
    .map((item) => {
      if (typeof item === "string") return item;
      if (!item || typeof item !== "object") return "";
      const block = item as Record<string, unknown>;
      return typeof block.text === "string"
        ? block.text
        : typeof block.content === "string"
          ? block.content
          : "";
    })
    .filter(Boolean);
  return parts.length ? parts.join("\n") : null;
}
