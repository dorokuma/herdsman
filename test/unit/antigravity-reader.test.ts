import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { afterEach, describe, expect, test } from "vitest";
import { AntigravityHistoryReader } from "@/agent-history/antigravity-reader.js";

const tempDirs: string[] = [];

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

async function tempDb(name: string) {
  const dir = await mkdtemp(join(tmpdir(), name));
  tempDirs.push(dir);
  const dbPath = join(dir, "conversation.sqlite");
  const sqlite = new DatabaseSync(dbPath);
  sqlite.exec(`
    create table steps (
      id integer primary key autoincrement,
      role text not null,
      payload text not null,
      created_at integer not null
    );
  `);
  return { dbPath, sqlite };
}

describe("AntigravityHistoryReader readCompact", () => {
  test("returns null for lastAssistantMessage when there is no user message", async () => {
    const { dbPath, sqlite } = await tempDb("herdsman-agy-no-user-");
    sqlite
      .prepare("insert into steps (role, payload, created_at) values (?, ?, ?)")
      .run("assistant", "hello without user", 1000);
    sqlite.close();

    const reader = new AntigravityHistoryReader();
    const compact = await reader.readCompact({
      kind: "agent_session",
      path: dbPath,
      source: "antigravity-sqlite",
      value: dbPath,
    });

    expect(compact.lastAssistantMessage).toBeNull();
    expect(compact.messageCount).toBe(1);
  });

  test("returns null when a new user message arrives after old assistant message without new assistant", async () => {
    const { dbPath, sqlite } = await tempDb("herdsman-agy-turn-advance-");
    const insert = sqlite.prepare("insert into steps (role, payload, created_at) values (?, ?, ?)");
    insert.run("user", "first question", 1000);
    insert.run("assistant", "first answer", 1001);
    insert.run("user", "second question", 2000);
    sqlite.close();

    const reader = new AntigravityHistoryReader();
    const compact = await reader.readCompact({
      kind: "agent_session",
      path: dbPath,
      source: "antigravity-sqlite",
      value: dbPath,
    });

    expect(compact.lastAssistantMessage).toBeNull();
    expect(compact.lastUserMessage).toMatchObject({ text: "second question" });
  });

  test("returns new assistant message once assistant responds to the last user message", async () => {
    const { dbPath, sqlite } = await tempDb("herdsman-agy-turn-complete-");
    const insert = sqlite.prepare("insert into steps (role, payload, created_at) values (?, ?, ?)");
    insert.run("user", "first question", 1000);
    insert.run("assistant", "first answer", 1001);
    insert.run("user", "second question", 2000);
    insert.run("assistant", "second answer", 2001);
    sqlite.close();

    const reader = new AntigravityHistoryReader();
    const compact = await reader.readCompact({
      kind: "agent_session",
      path: dbPath,
      source: "antigravity-sqlite",
      value: dbPath,
    });

    expect(compact.lastAssistantMessage).toMatchObject({ text: "second answer" });
    expect(compact.lastUserMessage).toMatchObject({ text: "second question" });
  });
});
