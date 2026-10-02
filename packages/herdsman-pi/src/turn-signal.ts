import { closeSync, openSync, readSync, statSync } from "node:fs";

export const TURN_SIGNAL_TIMEOUT_MS = 3_000;
export const TURN_SIGNAL_POLL_MS = 50;
export const TURN_SIGNAL_TAIL_CHARS = 8_000;

export type TurnCompletionCheckReason = "already_written" | "timeout" | "unavailable";

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
 * disk when the `message_end` hook fires; otherwise the file tail is polled
 * until the expected text (or its JSONL-escaped form) appears.
 *
 * The root cause of the mismatches this guards against is the source of
 * `expectedText`, not the tail it is compared to: the extension reports the
 * *sanitized* text it saw at `message_end` (`assistantMessageText` in
 * `index.ts` runs the body through `sanitizeText`), while rewrite-type
 * extensions (e.g. `no-tables` turning `### X` into bold or tables into
 * bullets, plus link handling) keep rewriting the transcript that lands on disk
 * *after* that hook. The client-side text and the on-disk original are therefore
 * systematically inconsistent, and the disk original is the authoritative body —
 * never the reported guess. "The file grew" was only a secondary mechanism that
 * let such a mismatch be *confirmed* instead of caught (tool results, streamed
 * partials and any other session write also grow the file): that is how
 * mismatch signals ended up as empty `agent.done` releases (sample figures and
 * the caveats around them live in
 * `.agents/notes/20261002-empty-wake-expectedtext-rootcause.md`). The
 * daemon's `expected_text_mismatch` arm now trusts the on-disk body, so a
 * timeout here degrades honestly instead of blanking it. Do not trace this back
 * to tail guessing alone.
 *
 * A timeout (or an unavailable file) still resolves so the caller can signal
 * with the actual status instead of hanging forever, and it now reports
 * `confirmed: false` honestly instead of fabricating a confirmation.
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
  // The session file is JSONL, so the written text is escaped (`\n`, `\"`,
  // `\\`). Matching the raw candidate only would miss every multi-line final
  // message, so the escaped form of the same suffix is accepted as well.
  const escapedCandidate = JSON.stringify(candidate).slice(1, -1);
  const containsText = (tail: string | null) =>
    tail !== null &&
    candidate.length > 0 &&
    (tail.includes(candidate) || tail.includes(escapedCandidate));

  const initialSize = probe.size(input.path);
  if (initialSize === null) return { confirmed: false, reason: "unavailable" };
  if (containsText(probe.readTail(input.path, TURN_SIGNAL_TAIL_CHARS))) {
    return { confirmed: true, reason: "already_written" };
  }
  while (Date.now() < deadline) {
    await probe.sleep(pollMs);
    if (containsText(probe.readTail(input.path, TURN_SIGNAL_TAIL_CHARS))) {
      return { confirmed: true, reason: "already_written" };
    }
  }
  return { confirmed: false, reason: "timeout" };
}