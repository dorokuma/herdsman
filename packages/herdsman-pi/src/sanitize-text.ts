// Sync guard: this is the extension-side copy of textFromContent/sanitizeText.
// The daemon-side copy lives in src/agent-history/text.ts.
// Keep both implementations identical; see test/unit/agent-history-text.test.ts for parity tests.
export function textFromContent(content: unknown): string | null {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return null;
  const parts = content
    .map((block) => {
      if (typeof block === "string") return block;
      if (typeof block !== "object" || block === null) return "";
      const record = block as Record<string, unknown>;
      if (record.type === "thinking" || record.type === "reasoning") return "";
      if (typeof record.text === "string") return record.text;
      if (typeof record.content === "string") return record.content;
      if (Array.isArray(record.content)) return textFromContent(record.content) ?? "";
      return "";
    })
    .filter((part) => part.trim().length > 0);
  return parts.length > 0 ? parts.join("\n") : null;
}

export function sanitizeText(value: unknown): { redacted: boolean; text: string } {
  let text = typeof value === "string" ? value : JSON.stringify(value);
  if (text === undefined) text = String(value);
  let redacted = false;
  for (const pattern of [
    /(Authorization:\s*Bearer\s+)[^\s]+/gi,
    /\b(Bearer\s+)[A-Za-z0-9._\-+=/]+/gi,
    /\b(token=)[^\s&]+/gi,
    /\b(password=)[^\s&]+/gi,
    /\b(secret=)[^\s&]+/gi,
    /\b(api_key=)[^\s&]+/gi,
  ]) {
    text = text.replace(pattern, (_match, prefix: string) => {
      redacted = true;
      return `${prefix}[REDACTED]`;
    });
  }
  text = text.replace(/\bsk-[A-Za-z0-9_-]{8,}/g, () => {
    redacted = true;
    return "sk-[REDACTED]";
  });
  return { redacted, text };
}
