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
