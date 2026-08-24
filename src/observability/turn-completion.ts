export const TURN_SIGNAL_WAIT_MS = 3_000;
export const TURN_SIGNAL_RETENTION_MS = 10 * 60_000;

export type TurnCompletionSignal = {
  confirmed: boolean;
  herdrSessionName: string;
  paneId: string;
  terminalId: string;
  workspaceId: string;
  recordedAtMs: number;
};

export type TurnCompletionWaitResult = {
  confirmed: boolean;
  received: boolean;
};

type TimerHandle = ReturnType<typeof setTimeout>;

/**
 * Cross-boundary store for Pi turn-completion notifications. The extension
 * records a signal through the observability RPC server once its final
 * assistant message is on disk; the agent index service waits (bounded) for
 * that signal before emitting `agent.done` / `agent.blocked` events for a Pi
 * agent, so the event captures a non-empty `lastAssistantMessage`.
 *
 * Signals are keyed by herdr session + terminal and retained for a bounded
 * window; a waiter resolves immediately when a signal already exists for the
 * terminal and otherwise resolves when a new signal arrives or the timeout
 * elapses. Old extension versions never record a signal, so waiters always
 * resolve through the timeout and the daemon keeps generating events as today.
 */
export class TurnCompletionRegistry {
  readonly #clearTimeout: (handle: TimerHandle) => void;
  readonly #latestByTerminal = new Map<string, TurnCompletionSignal>();
  readonly #now: () => number;
  readonly #setTimeout: (callback: () => void, delay: number) => TimerHandle;
  readonly #timeoutMs: number;
  readonly #waitersByTerminal = new Map<string, Set<() => void>>();

  constructor(
    options: {
      clearTimeout?: (handle: TimerHandle) => void;
      now?: () => number;
      setTimeout?: (callback: () => void, delay: number) => TimerHandle;
      timeoutMs?: number;
    } = {},
  ) {
    this.#clearTimeout = options.clearTimeout ?? clearTimeout;
    this.#now = options.now ?? Date.now;
    this.#setTimeout = options.setTimeout ?? setTimeout;
    this.#timeoutMs = options.timeoutMs ?? TURN_SIGNAL_WAIT_MS;
  }

  record(signal: Omit<TurnCompletionSignal, "recordedAtMs">): void {
    const key = terminalTurnKey(signal);
    this.#latestByTerminal.set(key, { ...signal, recordedAtMs: this.#now() });
    this.#prune();
    const waiters = this.#waitersByTerminal.get(key);
    if (waiters) {
      for (const resolve of [...waiters]) resolve();
    }
  }

  async waitForSignal(input: {
    herdrSessionName: string;
    terminalId: string;
    recordedAfterMs: number;
  }): Promise<TurnCompletionWaitResult> {
    const key = terminalTurnKey(input);
    const isNewSignal = (
      signal: TurnCompletionSignal | undefined,
    ): signal is TurnCompletionSignal =>
      signal !== undefined && signal.recordedAtMs > input.recordedAfterMs;
    const existing = this.#latestByTerminal.get(key);
    if (isNewSignal(existing)) return { confirmed: existing.confirmed, received: true };
    return new Promise((resolve) => {
      let timer: TimerHandle | undefined;
      const finish = (timedOut = false) => {
        const latest = this.#latestByTerminal.get(key);
        if (!timedOut && !isNewSignal(latest)) return;
        if (timer !== undefined) this.#clearTimeout(timer);
        this.#waitersByTerminal.get(key)?.delete(finish);
        resolve(
          latest && isNewSignal(latest)
            ? { confirmed: latest.confirmed, received: true }
            : { confirmed: false, received: false },
        );
      };
      let waiters = this.#waitersByTerminal.get(key);
      if (!waiters) {
        waiters = new Set();
        this.#waitersByTerminal.set(key, waiters);
      }
      waiters.add(finish);
      timer = this.#setTimeout(() => finish(true), this.#timeoutMs);
    });
  }

  #prune(): void {
    if (this.#latestByTerminal.size <= 1) return;
    const cutoff = this.#now() - TURN_SIGNAL_RETENTION_MS;
    for (const [key, signal] of this.#latestByTerminal) {
      if (signal.recordedAtMs < cutoff) this.#latestByTerminal.delete(key);
    }
  }
}

function terminalTurnKey(input: { herdrSessionName: string; terminalId: string }): string {
  return `${input.herdrSessionName}\0${input.terminalId}`;
}
