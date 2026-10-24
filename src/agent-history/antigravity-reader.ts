import { DatabaseSync } from "node:sqlite";
import type {
  AgentHistoryMessage,
  AgentHistoryRef,
  AgentHistoryUnreadyReason,
} from "@/observability/contracts.js";
import {
  decodeAntigravityMessage,
  decodeAntigravityMetadataTimestamp,
} from "./antigravity-proto.js";
import { type AgentHistoryReader, compactFromMessages, limitMessages } from "./readers.js";
import { messageRef, timestampFrom } from "./text.js";

/** Reads explicit envelopes and agy's protobuf step envelopes. */
export class AntigravityHistoryReader implements AgentHistoryReader {
  canRead(ref: AgentHistoryRef): boolean {
    return ref.source === "antigravity-sqlite" && Boolean(ref.path) && Boolean(ref.value);
  }

  async read(
    ref: AgentHistoryRef,
    options: { limit?: number } = {},
  ): Promise<AgentHistoryMessage[]> {
    if (!ref.path || !ref.value || ref.path.includes("..")) return [];
    let db: DatabaseSync | null = null;
    try {
      db = new DatabaseSync(ref.path, { readOnly: true });
      const tables = db.prepare("select name from sqlite_master where type = 'table'").all() as {
        name: string;
      }[];
      const table = tables.find((item) =>
        ["steps", "conversation_steps", "step"].includes(item.name),
      )?.name;
      if (!table) return [];
      const columns = db.prepare(`pragma table_info(${quote(table)})`).all() as { name: string }[];
      const names = new Set(columns.map((column) => column.name));
      const roleColumn = ["role", "envelope_role", "message_role"].find((name) => names.has(name));
      const payloadColumn = ["step_payload", "payload", "content", "message"].find((name) =>
        names.has(name),
      );
      if (!payloadColumn) return [];
      const idColumn = names.has("id") ? "id" : names.has("step_id") ? "step_id" : "idx";
      const timeColumn = names.has("created_at")
        ? "created_at"
        : names.has("timestamp")
          ? "timestamp"
          : null;
      const metadataColumn = names.has("metadata") ? "metadata" : null;
      const rows = db
        .prepare(
          `select ${quote(idColumn)} as id, ${roleColumn ? `${quote(roleColumn)} as role,` : ""} ${quote(payloadColumn)} as payload${timeColumn ? `, ${quote(timeColumn)} as time` : ""}${metadataColumn ? `, ${quote(metadataColumn)} as metadata` : ""} from ${quote(table)} order by rowid`,
        )
        .all() as {
        id: string | number;
        metadata?: unknown;
        payload: unknown;
        role?: unknown;
        time?: unknown;
      }[];
      const lastRow = rows.at(-1);
      const lastStepId = lastRow?.id ?? null;
      const lastStepTimestamp = lastRow
        ? (timestampFrom(lastRow.time) ?? decodeAntigravityMetadataTimestamp(lastRow.metadata))
        : null;
      console.debug("Herdsman antigravity reader inspected steps", {
        conversationPath: ref.path,
        lastStepId,
        lastStepTimestamp,
        stepCount: rows.length,
      });
      const messages: AgentHistoryMessage[] = rows.flatMap((row) => {
        const explicitRole: AgentHistoryMessage["role"] | null =
          row.role === "user" || row.role === "assistant" ? row.role : null;
        const decoded = explicitRole
          ? { role: explicitRole, text: payloadText(row.payload) }
          : decodeAntigravityMessage(row.payload);
        if (!decoded?.text) return [];
        return [
          {
            ref: messageRef(ref.path as string, String(row.id), 0),
            role: decoded.role,
            text: decoded.text,
            timestamp: timestampFrom(row.time) ?? decodeAntigravityMetadataTimestamp(row.metadata),
          },
        ];
      });
      return limitMessages(messages, options.limit);
    } catch {
      return [];
    } finally {
      db?.close();
    }
  }

  async readCompact(ref: AgentHistoryRef) {
    const messages = await this.read(ref);
    const compact = compactFromMessages(ref, messages);
    if (messages.length === 0) {
      return {
        ...compact,
        lastAssistantMessage: null,
        unreadyReason: "empty_history" as const,
      };
    }
    const lastUserIndex = messages.findLastIndex((message) => message.role === "user");
    if (lastUserIndex === -1) {
      return {
        ...compact,
        lastAssistantMessage: null,
        unreadyReason: "no_user_message" as const,
      };
    }
    const assistantAfterUser = messages
      .slice(lastUserIndex + 1)
      .findLast((message) => message.role === "assistant");
    if (assistantAfterUser) {
      return {
        ...compact,
        lastAssistantMessage: {
          ref: assistantAfterUser.ref,
          text: assistantAfterUser.text,
          timestamp: assistantAfterUser.timestamp,
        },
        unreadyReason: null,
      };
    }
    const hasAnyAssistant = messages.some((message) => message.role === "assistant");
    const unreadyReason: AgentHistoryUnreadyReason = hasAnyAssistant
      ? "assistant_only_before_last_user"
      : "no_assistant_message";
    return {
      ...compact,
      lastAssistantMessage: null,
      unreadyReason,
    };
  }
}

function payloadText(value: unknown): string | null {
  if (typeof value === "string") return value || null;
  return null;
}

function quote(value: string): string {
  return `"${value.replaceAll('"', '""')}"`;
}
