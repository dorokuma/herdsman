import { closeSync, openSync, readSync, statSync } from "node:fs";

export const TURN_SIGNAL_TIMEOUT_MS = 3_000;
export const TURN_SIGNAL_POLL_MS = 50;
export const TURN_SIGNAL_TAIL_CHARS = 8_000;

export type TurnCompletionCheckReason = "already_written" | "new_content" | "timeout" | "unavailable";

export type TurnCompletionCheck = {
  confirmed: boolean;
  reason: TurnCompletionCheckReason;
};

export type SessionWriteProbe = {
  readTail(path: string, chars: number): string | null;
  size(path: string): number | null;
  sleep(ms: number): Promise<void>;
};

function defaultProbe(): SessionWriteProbe {
  return {
    readTail(path, chars) {
      let fd: number | undefined;
      try {
        const size = statSync(path).size;
        const start = Math.max(0, size - chars);
        fd = openSync(path, "r");
        const buffer = Buffer.alloc(Math.max(0, size - start));
        const bytesRead = readSync(fd, buffer, 0, buffer.length, start);
        return buffer.toString("utf8", 0, bytesRead);
      } catch {
        return null;
      } finally {
        if (fd !== undefined) {
          try {
            closeSync(fd);
          } catch {
            // The file may have vanished mid-read; the next probe re-checks.
          }
        }
      }
    },
    size(path) {
      try {
        return statSync(path).size;
      } catch {
        return null;
      }
    },
    sleep(ms) {
      return new Promise((resolve) => setTimeout(resolve, ms));
    },
  };
}

/**
 * Bounded confirmation that the final assistant message reached the Pi session
 * file before the extension signals the daemon. The message may already be on
 * disk when the `message_end` hook fires (checked via the file tail containing
 * the expected text or its distinctive ending); otherwise the file size is
 * polled until new content appears. A timeout (or an unavailable file) still
 * resolves so the caller can signal with the actual status instead of hanging
 * forever.
 */
export async function confirmSessionWrite(input: {
  expectedText: string;
  path: string;
  pollMs?: number;
  probe?: SessionWriteProbe;
  timeoutMs?: number;
}): Promise<TurnCompletionCheck> {
  const probe = input.probe ?? defaultProbe();
  const timeoutMs = input.timeoutMs ?? TURN_SIGNAL_TIMEOUT_MS;
  const pollMs = input.pollMs ?? TURN_SIGNAL_POLL_MS;
  const expectedText = input.expectedText.trim();
  const deadline = Date.now() + timeoutMs;
  const candidate = expectedText.length > 200 ? expectedText.slice(-200) : expectedText;
  const containsText = (tail: string | null) =>
    tail !== null && candidate.length > 0 && tail.includes(candidate);

  const initialSize = probe.size(input.path);
  if (initialSize === null) return { confirmed: false, reason: "unavailable" };
  if (containsText(probe.readTail(input.path, TURN_SIGNAL_TAIL_CHARS))) {
    return { confirmed: true, reason: "already_written" };
  }
  while (Date.now() < deadline) {
    await probe.sleep(pollMs);
    const size = probe.size(input.path);
    if (size !== null && size > initialSize) return { confirmed: true, reason: "new_content" };
    if (containsText(probe.readTail(input.path, TURN_SIGNAL_TAIL_CHARS))) {
      return { confirmed: true, reason: "already_written" };
    }
  }
  return { confirmed: false, reason: "timeout" };
}