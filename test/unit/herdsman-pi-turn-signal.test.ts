import { describe, expect, test } from "vitest";
import type { SessionWriteProbe } from "../../packages/herdsman-pi/src/turn-signal.js";
import { confirmSessionWrite } from "../../packages/herdsman-pi/src/turn-signal.js";

function probeState(initialContent: string) {
  let content = initialContent;
  const sleeps: number[] = [];
  return {
    probe: {
      readTail: (_path: string, chars: number) => content.slice(-chars),
      size: (): number | null => content.length,
      sleep: (ms: number) => {
        sleeps.push(ms);
        return new Promise<void>((resolve) => setTimeout(resolve, ms));
      },
    } satisfies SessionWriteProbe,
    grow(text: string) {
      content += text;
    },
    sleeps,
  };
}

describe("confirmSessionWrite", () => {
  test("confirms immediately when the message text is already present in the file", async () => {
    const state = probeState('{"message":{"role":"assistant","content":"final answer"}}');
    await expect(
      confirmSessionWrite({ expectedText: "final answer", path: "/x", probe: state.probe }),
    ).resolves.toEqual({ confirmed: true, reason: "already_written" });
    expect(state.sleeps).toEqual([]);
  });

  test("confirms once the message text reaches the file after the turn ended", async () => {
    const state = probeState("user turn only\n");
    const pending = confirmSessionWrite({
      expectedText: "final answer",
      path: "/x",
      pollMs: 10,
      probe: state.probe,
      timeoutMs: 1_000,
    });
    await new Promise((resolve) => setTimeout(resolve, 15));
    state.grow('{"message":{"role":"assistant","content":"final answer"}}');
    await expect(pending).resolves.toEqual({ confirmed: true, reason: "already_written" });
  });

  test("does not confirm when the file only grows without the expected text", async () => {
    const state = probeState("user turn only\n");
    const pending = confirmSessionWrite({
      expectedText: "final answer",
      path: "/x",
      pollMs: 5,
      probe: state.probe,
      timeoutMs: 40,
    });
    // A tool result line lands after `message_end` but the final assistant text is
    // still missing: growth alone used to confirm the write,
    // which is what let `expectedText` drift away from the disk body.
    state.grow('{"type":"tool_result","content":"unrelated output"}\n');
    await expect(pending).resolves.toEqual({ confirmed: false, reason: "timeout" });
    expect(state.sleeps.length).toBeGreaterThan(1);
  });

  test("matches a JSONL-escaped tail so a multi-line message still confirms", async () => {
    // Session files are JSONL: a written newline is the two characters `\\` + `n`,
    // so the raw candidate can never match and a text-only confirmation would
    // time out on every multi-line final message.
    const state = probeState("");
    state.grow(
      '{"message":{"role":"assistant","content":[{"text":"line one\\nline two","type":"text"}]}}',
    );
    await expect(
      confirmSessionWrite({
        expectedText: "line one\nline two",
        path: "/x",
        probe: state.probe,
      }),
    ).resolves.toEqual({ confirmed: true, reason: "already_written" });
    expect(state.sleeps).toEqual([]);
  });

  test("still resolves with confirmed=false on timeout instead of hanging", async () => {
    const state = probeState("no growth ever\n");
    await expect(
      confirmSessionWrite({
        expectedText: "final answer",
        path: "/x",
        pollMs: 5,
        probe: state.probe,
        timeoutMs: 40,
      }),
    ).resolves.toEqual({ confirmed: false, reason: "timeout" });
    expect(state.sleeps.length).toBeGreaterThan(0);
  });

  test("fails fast with confirmed=false when the session file is unavailable", async () => {
    const state = probeState("anything");
    state.probe.size = () => null;
    await expect(
      confirmSessionWrite({ expectedText: "final answer", path: "/x", probe: state.probe }),
    ).resolves.toEqual({ confirmed: false, reason: "unavailable" });
    expect(state.sleeps).toEqual([]);
  });

  test("never matches when a rewrite-type extension rewrote the tail window", async () => {
    // `no-tables` (and friends) rewrite the transcript *after* `message_end`:
    // a `### X` heading becomes `**X**` in the file that lands on disk. Text-level
    // confirmation compares the client-side `expectedText` against the disk tail,
    // so a rewritten tail can never match. This used to be a silent property of
    // the implementation; pin it as expected behaviour instead.
    const expectedText = `${"a".repeat(250)}### X`;
    // The candidate is the last 200 chars of expectedText, i.e. it straddles the
    // rewritten heading, and the on-disk body carries the rewritten `**X**`.
    const state = probeState(
      JSON.stringify({ message: { role: "assistant", content: `${"a".repeat(250)}**X**` } }),
    );
    await expect(
      confirmSessionWrite({
        expectedText,
        path: "/x",
        pollMs: 5,
        probe: state.probe,
        timeoutMs: 40,
      }),
    ).resolves.toEqual({ confirmed: false, reason: "timeout" });
    expect(state.sleeps.length).toBeGreaterThan(1);
  });

  test("does not throw when the 200-char cut splits a 4-byte emoji surrogate pair", async () => {
    // `expectedText.slice(-200)` is UTF-16 based, so a cut can land between the
    // high and low surrogate of a 4-byte emoji. Only the "never throws" bottom
    // line is pinned: the candidate keeps a lone low surrogate, which still
    // occurs verbatim in the disk tail here, so the current outcome is
    // `already_written` (a `timeout` would be equally acceptable).
    const expectedText = `${"x".repeat(10)}😀${"y".repeat(199)}`;
    // The cut is at index 11, the low surrogate of the emoji at index 10.
    expect(expectedText.slice(-200).charCodeAt(0)).toBe(expectedText.charCodeAt(11));
    const state = probeState(
      JSON.stringify({ message: { role: "assistant", content: expectedText } }),
    );
    const result = await confirmSessionWrite({ expectedText, path: "/x", probe: state.probe });
    expect(["already_written", "timeout"]).toContain(result.reason);
    expect(result.confirmed).toBe(result.reason === "already_written");
    expect(state.sleeps).toEqual([]);
  });
});
