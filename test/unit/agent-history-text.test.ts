import { describe, expect, test } from "vitest";
import { sanitizeText } from "@/agent-history/text.js";
import { sanitizeText as sanitizePiText } from "../../packages/herdsman-pi/src/sanitize-text.js";

describe("sanitizeText", () => {
  test("redacts bearer, credential assignments, and common sk- tokens", () => {
    const input = [
      "Authorization: Bearer super-secret-token",
      "standalone Bearer abcdefghijklmnop",
      "token=abc123",
      "password=hunter2",
      "secret=shh",
      "api_key=abcd",
      "openai sk-abcdefghijklmnopqrstuvwxyz",
    ].join("\n");
    const sanitized = sanitizeText(input);
    expect(sanitized.redacted).toBe(true);
    expect(sanitized.text).toContain("Authorization: Bearer [REDACTED]");
    expect(sanitized.text).toContain("Bearer [REDACTED]");
    expect(sanitized.text).toContain("token=[REDACTED]");
    expect(sanitized.text).toContain("password=[REDACTED]");
    expect(sanitized.text).toContain("secret=[REDACTED]");
    expect(sanitized.text).toContain("api_key=[REDACTED]");
    expect(sanitized.text).toContain("sk-[REDACTED]");
    expect(sanitized.text).not.toContain("super-secret-token");
    expect(sanitized.text).not.toContain("abcdefghijklmnop");
    expect(sanitized.text).not.toContain("hunter2");
    expect(sanitized.text).not.toContain("sk-abcdefghijklmnopqrstuvwxyz");
  });

  test("leaves ordinary prose unchanged", () => {
    expect(sanitizeText("please inspect skip-list")).toEqual({
      redacted: false,
      text: "please inspect skip-list",
    });
  });

  test("redacts bearer tokens regardless of case", () => {
    for (const sanitize of [sanitizeText, sanitizePiText]) {
      for (const token of ["bearer", "BEARER", "BeArEr"]) {
        const sanitized = sanitize(`${token} super-secret-token`);
        expect(sanitized.redacted).toBe(true);
        expect(sanitized.text).toBe(`${token} [REDACTED]`);
        expect(sanitized.text).not.toContain("super-secret-token");
      }
    }
  });
});
