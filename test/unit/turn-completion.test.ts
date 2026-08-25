import { describe, expect, test, vi } from "vitest";
import { TurnCompletionRegistry } from "@/observability/turn-completion.js";

function signal(
  overrides: Partial<{
    confirmed: boolean;
    herdrSessionName: string;
    paneId: string;
    terminalId: string;
    workspaceId: string;
  }> = {},
) {
  return {
    confirmed: overrides.confirmed ?? true,
    herdrSessionName: overrides.herdrSessionName ?? "default",
    paneId: overrides.paneId ?? "wJ:p2",
    terminalId: overrides.terminalId ?? "term_pi",
    workspaceId: overrides.workspaceId ?? "wJ",
  };
}

describe("TurnCompletionRegistry", () => {
  test("recorded signal is immediately consumed when wait starts at record time", async () => {
    vi.useFakeTimers();
    const current = 1_000;
    const registry = new TurnCompletionRegistry({ now: () => current, timeoutMs: 3_000 });
    registry.record(signal());
    await expect(
      registry.waitForSignal({
        herdrSessionName: "default",
        terminalId: "term_pi",
        recordedAfterMs: current,
      }),
    ).resolves.toEqual({ confirmed: true, received: true });
    vi.useRealTimers();
  });
  test("same recorded signal is consumed only once", async () => {
    vi.useFakeTimers();
    const current = 1_000;
    const registry = new TurnCompletionRegistry({ now: () => current, timeoutMs: 3_000 });
    registry.record(signal());
    await expect(
      registry.waitForSignal({
        herdrSessionName: "default",
        terminalId: "term_pi",
        recordedAfterMs: current,
      }),
    ).resolves.toEqual({ confirmed: true, received: true });
    const second = registry.waitForSignal({
      herdrSessionName: "default",
      terminalId: "term_pi",
      recordedAfterMs: current,
    });
    await vi.advanceTimersByTimeAsync(3_000);
    await expect(second).resolves.toEqual({ confirmed: false, received: false });
    vi.useRealTimers();
  });
  test("resolves immediately when a signal was already recorded for the terminal", async () => {
    const registry = new TurnCompletionRegistry();
    registry.record(signal());
    await expect(
      registry.waitForSignal({
        herdrSessionName: "default",
        recordedAfterMs: 0,
        terminalId: "term_pi",
      }),
    ).resolves.toEqual({ confirmed: true, received: true });
  });

  test("ignores a stale signal until a new signal arrives", async () => {
    vi.useFakeTimers();
    let current = 1_000;
    const registry = new TurnCompletionRegistry({ now: () => current, timeoutMs: 3_000 });
    registry.record(signal({ confirmed: true }));
    const pending = registry.waitForSignal({
      herdrSessionName: "default",
      recordedAfterMs: current + 1,
      terminalId: "term_pi",
    });
    await Promise.resolve();
    let settled = false;
    void pending.then(() => {
      settled = true;
    });
    await Promise.resolve();
    expect(settled).toBe(false);
    current += 1;
    registry.record(signal({ confirmed: false }));
    await expect(pending).resolves.toEqual({ confirmed: false, received: true });
    vi.useRealTimers();
  });

  test("resolves when a signal arrives while waiting", async () => {
    vi.useFakeTimers();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    const pending = registry.waitForSignal({
      herdrSessionName: "default",
      recordedAfterMs: Date.now() - 1,
      terminalId: "term_pi",
    });
    registry.record(signal({ confirmed: false }));
    await expect(pending).resolves.toEqual({ confirmed: false, received: true });
    vi.useRealTimers();
  });

  test("times out with received=false when no signal arrives", async () => {
    vi.useFakeTimers();
    const registry = new TurnCompletionRegistry({ timeoutMs: 3_000 });
    const pending = registry.waitForSignal({
      herdrSessionName: "default",
      recordedAfterMs: Date.now(),
      terminalId: "term_pi",
    });
    await vi.advanceTimersByTimeAsync(3_000);
    await expect(pending).resolves.toEqual({ confirmed: false, received: false });
    vi.useRealTimers();
  });

  test("scopes signals by herdr session and terminal", async () => {
    const registry = new TurnCompletionRegistry({ timeoutMs: 10 });
    registry.record(signal({ herdrSessionName: "default", terminalId: "term_pi" }));
    await expect(
      registry.waitForSignal({
        herdrSessionName: "other",
        recordedAfterMs: 0,
        terminalId: "term_pi",
      }),
    ).resolves.toEqual({ confirmed: false, received: false });
    await expect(
      registry.waitForSignal({
        herdrSessionName: "default",
        recordedAfterMs: 0,
        terminalId: "term_other",
      }),
    ).resolves.toEqual({ confirmed: false, received: false });
  });

  test("prunes stale signals on record to bound memory", async () => {
    vi.useFakeTimers();
    const now = Date.now();
    let current = now;
    const registry = new TurnCompletionRegistry({ now: () => current, timeoutMs: 10 });
    registry.record(signal({ terminalId: "term_old" }));
    current = now + 11 * 60_000;
    registry.record(signal({ terminalId: "term_new" }));
    const staleWait = registry.waitForSignal({
      herdrSessionName: "default",
      recordedAfterMs: current,
      terminalId: "term_old",
    });
    await vi.advanceTimersByTimeAsync(20);
    await expect(staleWait).resolves.toEqual({ confirmed: false, received: false });
    await expect(
      registry.waitForSignal({
        herdrSessionName: "default",
        recordedAfterMs: 0,
        terminalId: "term_new",
      }),
    ).resolves.toEqual({ confirmed: true, received: true });
    vi.useRealTimers();
  });
});
