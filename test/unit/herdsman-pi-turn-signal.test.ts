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

  test("confirms once new content appears after the message ended", async () => {
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
    await expect(pending).resolves.toEqual({ confirmed: true, reason: "new_content" });
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
});
