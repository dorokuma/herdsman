import { DatabaseSync } from "node:sqlite";
import type { AgentHistoryMessage, AgentHistoryRef } from "@/observability/contracts.js";
import { decodeAntigravityMessage } from "./antigravity-proto.js";
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
      const rows = db
        .prepare(
          `select ${quote(idColumn)} as id, ${roleColumn ? `${quote(roleColumn)} as role,` : ""} ${quote(payloadColumn)} as payload${timeColumn ? `, ${quote(timeColumn)} as time` : ""} from ${quote(table)} order by rowid`,
        )
        .all() as { id: string | number; role?: unknown; payload: unknown; time?: unknown }[];
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
            timestamp: timestampFrom(row.time),
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
    return compactFromMessages(ref, await this.read(ref));
  }
}

function payloadText(value: unknown): string | null {
  if (typeof value === "string") return value || null;
  return null;
}

function quote(value: string): string {
  return `"${value.replaceAll('"', '""')}"`;
}
