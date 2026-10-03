import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { PiHistoryReader } from "@/agent-history/pi-reader.js";
import { JsonlTooLargeError, readJsonl, UnstableJsonlError } from "@/agent-history/readers.js";

const tempDirs: string[] = [];

afterEach(async () => {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

async function tempHome(name: string) {
  const dir = await mkdtemp(join(tmpdir(), name));
  tempDirs.push(dir);
  return dir;
}

describe("PiHistoryReader", () => {
  test("reads user, assistant, and tool result messages from the session JSONL", async () => {
    const homeDir = await tempHome("herdsman-pi-reader-");
    const path = join(homeDir, "session.jsonl");
    await writeFile(
      path,
      `${[
        { type: "session", id: "s1" },
        {
          type: "message",
          id: "u1",
          timestamp: "2026-07-09T12:00:00.000Z",
          message: { role: "user", content: "please inspect" },
        },
        {
          type: "message",
          id: "t1",
          timestamp: "2026-07-09T12:00:01.000Z",
          message: {
            role: "toolResult",
            toolName: "bash",
            content: [{ type: "text", text: "line 1\nline 2" }],
          },
        },
        {
          type: "message",
          id: "a1",
          timestamp: "2026-07-09T12:00:02.000Z",
          message: {
            role: "assistant",
            content: [{ type: "text", text: "done" }],
            stopReason: "stop",
          },
        },
      ]
        .map((entry) => JSON.stringify(entry))
        .join("\n")}\n`,
    );

    const messages = await new PiHistoryReader().read(
      { kind: "agent_session", path, source: "pi-jsonl", value: path },
      { limit: 20 },
    );

    expect(messages.map((message) => message.role)).toEqual(["user", "tool_result", "assistant"]);
    expect(messages[0]).toMatchObject({
      role: "user",
      text: "please inspect",
      timestamp: "2026-07-09T12:00:00.000Z",
    });
    expect(messages[1]).toMatchObject({ role: "tool_result", toolName: "bash" });
    expect(messages[1]?.compact?.text).toContain("line 1");
    expect(messages[2]).toMatchObject({ role: "assistant", text: "done", stopReason: "stop" });
  });

  test("redacts secrets in user and assistant bodies", async () => {
    const homeDir = await tempHome("herdsman-pi-sanitize-");
    const path = join(homeDir, "session.jsonl");
    await writeFile(
      path,
      `${[
        {
          type: "message",
          id: "u1",
          message: {
            role: "user",
            content: "password=hunter2 please use sk-abcdefghijklmnopqrstuvwxyz",
          },
        },
        {
          type: "message",
          id: "a1",
          message: { role: "assistant", content: "Authorization: Bearer super-secret-token" },
        },
      ]
        .map((entry) => JSON.stringify(entry))
        .join("\n")}\n`,
    );
    const messages = await new PiHistoryReader().read({
      kind: "agent_session",
      path,
      source: "pi-jsonl",
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
