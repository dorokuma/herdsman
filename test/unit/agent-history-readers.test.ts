import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { afterEach, describe, expect, test } from "vitest";
import { CodexHistoryReader } from "@/agent-history/codex-reader.js";
import { GeminiHistoryReader } from "@/agent-history/gemini-reader.js";
import { GrokHistoryReader } from "@/agent-history/grok-reader.js";
import { OpenCodeHistoryReader } from "@/agent-history/opencode-reader.js";
import { JsonlTooLargeError, readJsonl, UnstableJsonlError } from "@/agent-history/readers.js";
import { createAgentHistoryService } from "@/agent-history/service.js";

const tempDirs: string[] = [];

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

async function tempHome(name: string) {
  const dir = await mkdtemp(join(tmpdir(), name));
  tempDirs.push(dir);
  return dir;
}

describe("GrokHistoryReader", () => {
  test("returns the last assistant message from JSONL", async () => {
    const homeDir = await tempHome("herdsman-grok-reader-");
    const path = join(homeDir, "chat_history.jsonl");
    await writeFile(
      path,
      `${[
        { type: "assistant", id: "a1", content: "old" },
        { type: "user", content: "question" },
        { type: "assistant", id: "a2", content: [{ text: "latest" }] },
      ]
        .map((entry) => JSON.stringify(entry))
        .join("\n")}\n`,
    );
    await expect(
      new GrokHistoryReader().read({
        kind: "discovered_file",
        path,
        source: "grok-jsonl",
        value: path,
      }),
    ).resolves.toMatchObject([
      { role: "assistant", text: "old" },
      { role: "user", text: "question" },
      { role: "assistant", text: "latest" },
    ]);
  });
});

describe("CodexHistoryReader", () => {
  test("reads user, assistant, and tool output messages", async () => {
    const homeDir = await tempHome("herdsman-codex-reader-");
    const dir = join(homeDir, ".codex", "sessions", "2026", "07", "09");
    await mkdir(dir, { recursive: true });
    const path = join(
      dir,
      "rollout-2026-07-09T12-00-00-cccccccc-cccc-4ccc-8ccc-cccccccccccc.jsonl",
    );
    await writeFile(
      path,
      `${[
        { type: "session_meta", payload: { cwd: "/repo", timestamp: "2026-07-09T12:00:00.000Z" } },
        {
          type: "event_msg",
          payload: {
            type: "user_message",
            message: "please inspect",
            timestamp: "2026-07-09T12:00:01.000Z",
          },
        },
        {
          type: "response_item",
          payload: { type: "function_call", call_id: "call_1", name: "bash", arguments: "{}" },
        },
        {
          type: "response_item",
          payload: { type: "function_call_output", call_id: "call_1", output: "line 1\nline 2" },
        },
        {
          type: "response_item",
          payload: {
            type: "message",
            role: "assistant",
            content: [{ type: "output_text", text: "done" }],
          },
        },
      ]
        .map((entry) => JSON.stringify(entry))
        .join("\n")}\n`,
    );

    const messages = await new CodexHistoryReader().read(
      { kind: "discovered_file", path, source: "codex-jsonl", value: path },
      { limit: 20 },
    );

    expect(messages.map((message) => message.role)).toEqual(["user", "tool_result", "assistant"]);
    expect(messages[0]).toMatchObject({ role: "user", text: "please inspect" });
    expect(messages[1]).toMatchObject({ role: "tool_result", toolName: "bash" });
    expect(messages[1]?.compact?.text).toContain("line 1");
    expect(messages[2]).toMatchObject({ role: "assistant", text: "done" });
  });

  test("is registered in the default agent history service", async () => {
    const homeDir = await tempHome("herdsman-codex-service-");
    const dir = join(homeDir, ".codex", "sessions", "2026", "07", "09");
    await mkdir(dir, { recursive: true });
    const path = join(
      dir,
      "rollout-2026-07-09T13-00-00-dddddddd-dddd-4ddd-8ddd-dddddddddddd.jsonl",
    );
    await writeFile(
      path,
      `${JSON.stringify({ type: "session_meta", payload: { cwd: "/repo" } })}\n${JSON.stringify({ type: "event_msg", payload: { type: "user_message", message: "hello" } })}\n`,
    );

    const service = createAgentHistoryService({ homeDir });
    await expect(
      service.read(
        { agent: "codex", agentSession: null, cwd: "/repo", foregroundCwd: null },
        { limit: 10 },
      ),
    ).resolves.toMatchObject({
      historyRef: { source: "codex-jsonl", path },
      messages: [expect.objectContaining({ role: "user", text: "hello" })],
    });
  });
});

describe("OpenCodeHistoryReader", () => {
  test("reads text and tool parts from an OpenCode SQLite session", async () => {
    const homeDir = await tempHome("herdsman-opencode-reader-");
    const dbPath = join(homeDir, "opencode.db");
    const sqlite = new DatabaseSync(dbPath);
    sqlite.exec(`
      create table session (id text primary key, directory text not null, time_updated integer not null);
      create table message (id text primary key, session_id text not null, time_created integer not null, time_updated integer not null, data text not null);
      create table part (id text primary key, message_id text not null, session_id text not null, time_created integer not null, time_updated integer not null, data text not null);
    `);
    sqlite
      .prepare("insert into session (id, directory, time_updated) values (?, ?, ?)")
      .run("s1", "/repo", 1000);
    sqlite
      .prepare(
        "insert into message (id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?)",
      )
      .run("m1", "s1", 1000, 1000, JSON.stringify({ role: "user" }));
    sqlite
      .prepare(
        "insert into part (id, message_id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?, ?)",
      )
      .run("p1", "m1", "s1", 1001, 1001, JSON.stringify({ type: "text", text: "inspect this" }));
    sqlite
      .prepare(
        "insert into message (id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?)",
      )
      .run("m2", "s1", 2000, 2000, JSON.stringify({ role: "assistant", finish: "tool-calls" }));
    sqlite
      .prepare(
        "insert into part (id, message_id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?, ?)",
      )
      .run(
        "p2",
        "m2",
        "s1",
        2001,
        2001,
        JSON.stringify({
          type: "tool",
          tool: "bash",
          state: { status: "completed", output: "ok" },
        }),
      );
    sqlite
      .prepare(
        "insert into part (id, message_id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?, ?)",
      )
      .run("p3", "m2", "s1", 2002, 2002, JSON.stringify({ type: "text", text: "done" }));
    sqlite.close();

    const messages = await new OpenCodeHistoryReader().read(
      { kind: "discovered_file", path: dbPath, source: "opencode-sqlite", value: "s1" },
      { limit: 10 },
    );

    expect(messages.map((message) => message.role)).toEqual(["user", "tool_result", "assistant"]);
    expect(messages[0]).toMatchObject({ role: "user", text: "inspect this" });
    expect(messages[1]).toMatchObject({ role: "tool_result", toolName: "bash" });
    expect(messages[1]?.compact?.text).toContain("ok");
    expect(messages[2]).toMatchObject({ role: "assistant", text: "done" });
  });

  test("returns empty history when the OpenCode DB schema is unreadable", async () => {
    const homeDir = await tempHome("herdsman-opencode-bad-db-");
    const dbPath = join(homeDir, "opencode.db");
    const sqlite = new DatabaseSync(dbPath);
    sqlite.exec("create table unrelated (id text primary key)");
    sqlite.close();

    await expect(
      new OpenCodeHistoryReader().read(
        { kind: "discovered_file", path: dbPath, source: "opencode-sqlite", value: "s1" },
        { limit: 10 },
      ),
    ).resolves.toEqual([]);
  });
});

describe("GeminiHistoryReader", () => {
  test("reads user and gemini assistant messages from object-shaped session JSON", async () => {
    const homeDir = await tempHome("herdsman-gemini-reader-");
    const projectDir = join(homeDir, ".gemini", "tmp", "repo-project");
    const chatsDir = join(projectDir, "chats");
    await mkdir(chatsDir, { recursive: true });
    const sessionPath = join(chatsDir, "session-2026-07-09T12-00-00abcdef.json");
    await writeFile(
      sessionPath,
      JSON.stringify({
        sessionId: "g1",
        messages: [
          {
            id: "u1",
            timestamp: "2026-07-09T12:00:00.000Z",
            type: "user",
            content: [{ text: "please check" }],
          },
          { id: "a1", timestamp: "2026-07-09T12:00:01.000Z", type: "gemini", content: "checked" },
          { id: "i1", timestamp: "2026-07-09T12:00:02.000Z", type: "info", content: "ignored" },
        ],
      }),
    );

    const messages = await new GeminiHistoryReader().read(
      { kind: "discovered_file", path: sessionPath, source: "gemini-json", value: sessionPath },
      { limit: 10 },
    );

    expect(messages).toEqual([
      expect.objectContaining({
        role: "user",
        text: "please check",
        timestamp: "2026-07-09T12:00:00.000Z",
      }),
      expect.objectContaining({
        role: "assistant",
        text: "checked",
        timestamp: "2026-07-09T12:00:01.000Z",
      }),
    ]);
  });

  test("reads tool result messages when Gemini session records tool output", async () => {
    const homeDir = await tempHome("herdsman-gemini-tool-");
    const sessionPath = join(homeDir, "session.json");
    await writeFile(
      sessionPath,
      JSON.stringify({
        messages: [
          {
            id: "t1",
            timestamp: "2026-07-09T12:00:03.000Z",
            type: "tool",
            tool: "shell",
            content: "ok",
          },
        ],
      }),
    );

    const messages = await new GeminiHistoryReader().read(
      { kind: "discovered_file", path: sessionPath, source: "gemini-json", value: sessionPath },
      { limit: 10 },
    );

    expect(messages).toEqual([expect.objectContaining({ role: "tool_result", toolName: "shell" })]);
    expect(messages[0]?.compact?.text).toContain("ok");
  });

  test("returns empty history when Gemini session JSON is malformed", async () => {
    const homeDir = await tempHome("herdsman-gemini-bad-json-");
    const sessionPath = join(homeDir, "session.json");
    await writeFile(sessionPath, "{not-json");

    await expect(
      new GeminiHistoryReader().read(
        { kind: "discovered_file", path: sessionPath, source: "gemini-json", value: sessionPath },
        { limit: 10 },
      ),
    ).resolves.toEqual([]);
  });
});

describe("readJsonl", () => {
  test("streams lines from a file within the size cap", async () => {
    const homeDir = await tempHome("herdsman-jsonl-cap-");
    const path = join(homeDir, "chat.jsonl");
    await writeFile(
      path,
      `${JSON.stringify({ type: "user", content: "ok" })}\n${JSON.stringify({ type: "assistant", content: "done" })}\n`,
    );
    await expect(readJsonl(path)).resolves.toEqual([
      { line: 1, value: { type: "user", content: "ok" } },
      { line: 2, value: { type: "assistant", content: "done" } },
    ]);
  });

  test("reads a tail window from an oversized file so later limit trimming still has history", async () => {
    const homeDir = await tempHome("herdsman-jsonl-tail-");
    const path = join(homeDir, "huge.jsonl");
    const dropped = `${JSON.stringify({ type: "user", content: "old" })}\n`;
    const keptUser = JSON.stringify({ type: "user", content: "recent" });
    const keptAssistant = JSON.stringify({ type: "assistant", content: "kept" });
    const kept = `${keptUser}\n${keptAssistant}\n`;
    const padding = `${"x".repeat(80)}\n`;
    await writeFile(path, `${dropped}${padding}${kept}`);
    const maxBytes = Buffer.byteLength(kept, "utf8") + 10;
    const entries = await readJsonl(path, { maxBytes });
    expect(entries.map((entry) => entry.value)).toEqual([
      { type: "user", content: "recent" },
      { type: "assistant", content: "kept" },
    ]);
    const limited = entries.slice(Math.max(0, entries.length - 1));
    expect(limited).toEqual([
      { line: expect.any(Number), value: { type: "assistant", content: "kept" } },
    ]);
  });

  test("still treats a malformed final record as an unstable tail", async () => {
    const homeDir = await tempHome("herdsman-jsonl-unstable-");
    const path = join(homeDir, "tail.jsonl");
    await writeFile(path, `${JSON.stringify({ type: "user", content: "ok" })}\n{\n`);
    await expect(readJsonl(path)).rejects.toBeInstanceOf(UnstableJsonlError);
  });

  test("throws JsonlTooLargeError when a single record exceeds the tail window", async () => {
    const homeDir = await tempHome("herdsman-jsonl-one-line-");
    const path = join(homeDir, "huge-line.jsonl");
    const record = JSON.stringify({ type: "user", content: "x".repeat(200) });
    await writeFile(path, `${record}\n`);
    const maxBytes = 40;
    await expect(readJsonl(path, { maxBytes })).rejects.toBeInstanceOf(JsonlTooLargeError);
  });

  test("rethrows stream errors after a successful stat", async () => {
    const homeDir = await tempHome("herdsman-jsonl-stream-error-");
    await expect(readJsonl(homeDir)).rejects.toMatchObject({ code: "EISDIR" });
  });
});

describe("history body sanitization", () => {
  test("redacts secrets in user and assistant JSONL bodies", async () => {
    const homeDir = await tempHome("herdsman-jsonl-sanitize-");
    const path = join(homeDir, "chat_history.jsonl");
    await writeFile(
      path,
      `${[
        { type: "user", content: "password=hunter2 please use sk-abcdefghijklmnopqrstuvwxyz" },
        { type: "assistant", content: "Authorization: Bearer super-secret-token" },
      ]
        .map((entry) => JSON.stringify(entry))
        .join("\n")}\n`,
    );
    const messages = await new GrokHistoryReader().read({
      kind: "discovered_file",
      path,
      source: "grok-jsonl",
      value: path,
    });
    expect(messages).toEqual([
      expect.objectContaining({
        role: "user",
        text: "password=[REDACTED] please use sk-[REDACTED]",
      }),
      expect.objectContaining({
        role: "assistant",
        text: "Authorization: Bearer [REDACTED]",
      }),
    ]);
    expect(messages.map((message) => message.text).join("\n")).not.toContain("hunter2");
    expect(messages.map((message) => message.text).join("\n")).not.toContain("super-secret-token");
  });
});
