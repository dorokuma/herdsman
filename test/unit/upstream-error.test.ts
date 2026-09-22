import { describe, expect, test } from "vitest";
import {
  DEFAULT_WAKE_FILTER_CONFIG,
  isErrorShaped,
  isUpstreamModelError,
  MAX_ERROR_SHAPED_CHARS,
  matchUpstreamModelError,
} from "../../packages/herdsman-pi/src/upstream-error.js";

describe("upstream model error classifier", () => {
  // Must-match positives (review/oracle mandated). Each is expected to be
  // classified as an upstream model error under the default-on filter.
  test.each([
    ["T1", "API Error: 429 Rate limit exceeded"],
    ["T1", '{"type":"error","error":{"type":"rate_limit_error","message":"Rate limit reached"}}'],
    ["T1", "rate_limit_error"],
    ["T1", "overloaded_error"],
    ["T1", "resource_exhausted"],
    ["T1", "ECONNRESET"],
    ["T1", "ETIMEDOUT"],
    ["T1", "ENOTFOUND"],
    ["T1", "EAI_AGAIN"],
    ["T1", "read ECONNRESET"],
    ["T1", "socket hang up"],
    ["T1", "fetch failed"],
    ["T1", "und_err_123"],
    ["T1", "request timed out after 60s"],
    ["T1", "timeout of 30000ms exceeded"],
    ["T1", "connection error: other side closed"],
    ["T1", "the model is overloaded, please retry"],
    ["T1", "the model is currently overloaded, please try again"],
    ["T1", "you exceeded your current quota, please check your plan and billing details"],
    ["T1", "insufficient_quota"],
    ["T1", "quota exceeded"],
    ["T1", "频率限制"],
    ["T1", "请求过于频繁"],
    ["T1", "模型过载"],
    ["T1", "请求过于频繁，请稍后重试"],
    ["T1", "资源耗尽"],
    ["T1", "Rate limit reached for gpt-4 in organization org-x on tokens per minute (TPM)"],
    ["T1", "Rate limit hit. Please slow down."],
    ["T1", "The request failed with status 503"],
    ["T1", "error 429"],
    ["T1", "error: 429"],
    ["T1", "error status 429"],
    ["T1", "error code 503"],
    ["T1", "err 429"],
    ["T1", "failed 503"],
    ["T1", "failed with status 503"],
    ["T1", "failed with status code 503"],
    ["T1", "Error 429: rate limited"],
    ["T1", "Request failed with status code 503"],
    ["T1", "频率限制，请稍后重试"],
    ["T1", "模型过载，请稍候。"],
    ["T5", "429 Too Many Requests"],
    ["T5", "503 Service Unavailable"],
    ["T5", "529 overloaded"],
    ["T1", "ECONNREFUSED"],
    ["T1", "connect ECONNREFUSED"],
  ])("matches a real upstream error shape (%s): %s", (_tier, text) => {
    const match = matchUpstreamModelError(text);
    expect(match.matched).toBe(true);
    if (match.matched) expect(match.pattern.length).toBeGreaterThan(0);
    expect(isUpstreamModelError(text)).toBe(true);
  });

  // Must-not-match negatives (review/oracle mandated). None of these represent
  // an actually-unsuppressed upstream provider failure.
  test.each([
    ["HTTP 429 状态码代表客户端发送的请求过多（Too Many Requests）。"],
    ["查过了，日志里一共发现了 3 次 503 错误。"],
    ["Fix completed. I updated the rate limit logic in the config."],
    ["已修复：我们在客户端请求中增加了重试，避免因为频率限制导致失败。"],
    ["Connection error handling has been implemented in network.ts."],
    ["Please try again later if the pane is still starting."],
    ["Done. Added HTTP 503 retry in the client."],
    ["Updated tokens per minute documentation."],
    ["Implemented fetch failed fallback in transport.ts."],
    ["已说明频率限制与配额的区别。"],
    ["Server error, please try again later"],
    ["429"],
    ["503"],
    ["529"],
    ["timeout"],
    ["error"],
    ["please try again later"],
    ["Documented status 429 handling."],
    ["The provider returned status 429 once."],
    ["See code 429 in the table."],
    ["The response code was 503."],
    ["Documented error 429 handling."],
    ["Fixed error handling for 429."],
    ["Done. Added error 503 retry in the client."],
    ["Fix complete. The error handler now treats 429 as retryable."],
    ["The build failed. 503 was only mentioned in comments."],
    ["I preferred 429 as the test fixture."],
    ["No error occurred; the 429 was expected."],
    ["We handled the error, then documented 429."],
    ["Documented error handling for 429."],
    ["The test failed; we now cover 429."],
    ["interrupt 429"],
    ["Ferrari 429"],
    ["The error message said 429 so I documented it."],
    ["failed the test because 503 was mocked"],
    ["频率限制与配额的区别已说明。"],
    ["频率限制已修复。"],
    ["频率限制的处理已加到配置里。"],
    ["请求过于频繁的场景我们已在文档中说明。"],
    ["资源耗尽的配置项已修复。"],
    ["频率限制以及重试间隔需要产品确认。"],
    ["请求过于频繁是常见现象。"],
    ["资源耗尽也被写进了文档。"],
    ["模型过载了需要扩容。"],
    ["模型过载在高峰期出现。"],
    ["Error 429 was documented in the README."],
    ["Failed 503 times in the test suite."],
    ["error with 503 retries configured."],
  ])("does not misclassify ordinary/short prose as an upstream error: %s", (text) => {
    expect(isUpstreamModelError(text)).toBe(false);
    expect(matchUpstreamModelError(text)).toEqual({ matched: false });
  });

  test("does not match empty or whitespace-only input", () => {
    for (const text of ["", "   ", "\t", "\n"]) {
      expect(isUpstreamModelError(text)).toBe(false);
      expect(matchUpstreamModelError(text)).toEqual({ matched: false });
    }
  });

  test("does not match a long substantive report that merely mentions error tokens", () => {
    const report = `${"Chapter about the implementation. ".repeat(
      30,
    )} The run reported a 429 from the provider once, and the retry logic also handles timeout and rate limit paths. ${"More prose. ".repeat(
      40,
    )}`;
    expect(report.length).toBeGreaterThan(MAX_ERROR_SHAPED_CHARS);
    expect(isErrorShaped(report)).toBe(false);
    expect(isUpstreamModelError(report)).toBe(false);
  });

  test("does not match a long text that starts with a markdown heading or contains a code fence", () => {
    const fenced = `# Report\n${"detail ".repeat(80)}\n\`\`\`\napi error: 429\n\`\`\``;
    expect(isUpstreamModelError(fenced)).toBe(false);
    const codeOnly = `api error: 429\n\`\`\`\nretry\n\`\`\``;
    expect(isErrorShaped(codeOnly)).toBe(true);
    expect(isUpstreamModelError(codeOnly)).toBe(false);
  });

  test("does not match an error-looking short string that contains a code fence", () => {
    expect(isUpstreamModelError("API Error: 429 rate_limit_error ```")).toBe(false);
  });

  test("respects the enabled switch", () => {
    const disabled = { enabled: false, extraPatterns: [] };
    expect(isUpstreamModelError("API Error: 429 rate_limit_error", disabled)).toBe(false);
    expect(isUpstreamModelError("read ECONNRESET", disabled)).toBe(false);
    expect(
      isUpstreamModelError("API Error: 429 rate_limit_error", DEFAULT_WAKE_FILTER_CONFIG),
    ).toBe(true);
  });

  test("matches custom substring patterns case-insensitively", () => {
    const config = { enabled: true, extraPatterns: ["CHECKPOINT-STALL"] };
    expect(isUpstreamModelError("warning: checkpoint-stall detected", config)).toBe(true);
    expect(isUpstreamModelError("warning: nothing here", config)).toBe(false);
    expect(matchUpstreamModelError("checkpoint-stall", config)).toEqual({
      matched: true,
      pattern: "extra:CHECKPOINT-STALL",
    });
  });

  test("supports delimited custom regular expressions", () => {
    const config = { enabled: true, extraPatterns: ["/foo\\s+bar/i"] };
    expect(isUpstreamModelError("FOO    bar", config)).toBe(true);
    expect(isUpstreamModelError("foo-bar", config)).toBe(false);
  });

  test("custom patterns bypass the error-shaped latch", () => {
    const long = `${"narrative ".repeat(60)}CHECKPOINT-STALL`;
    expect(long.length).toBeGreaterThan(MAX_ERROR_SHAPED_CHARS);
    expect(isUpstreamModelError(long)).toBe(false);
    expect(isUpstreamModelError(long, { enabled: true, extraPatterns: ["checkpoint-stall"] })).toBe(
      true,
    );
  });

  test("invalid custom regular expressions are skipped without breaking built-in rules", () => {
    const config = { enabled: true, extraPatterns: ["/[/"] };
    expect(() => isUpstreamModelError("API Error: 429 rate_limit_error", config)).not.toThrow();
    expect(isUpstreamModelError("API Error: 429 rate_limit_error", config)).toBe(true);
    expect(isUpstreamModelError("nothing to see", config)).toBe(false);
  });

  test("normalizes terminal control sequences and whitespace before matching", () => {
    expect(isErrorShaped("\u001b[31mAPI Error: 429\u001b[0m")).toBe(true);
    expect(isUpstreamModelError("\u001b[31mAPI   Error:\n\t429\u001b[0m")).toBe(true);
    expect(isUpstreamModelError("   read    ECONNRESET   ")).toBe(true);
  });

  test("treats already normalized input idempotently", () => {
    const normalized = "API Error: 429 rate_limit_error";
    expect(isUpstreamModelError(normalized)).toBe(true);
    expect(isUpstreamModelError(normalized.replace(/\s+/g, " ").trim())).toBe(true);
  });

  test("plain 'timeout' or 'error' words alone are not enough", () => {
    expect(isUpstreamModelError("timeout")).toBe(false);
    expect(isUpstreamModelError("error")).toBe(false);
    expect(isUpstreamModelError("finished with an error in the log")).toBe(false);
  });
});
