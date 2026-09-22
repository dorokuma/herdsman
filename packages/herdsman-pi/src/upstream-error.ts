/**
 * Upstream model error classifier for Herdsman Pi wake filtering.
 *
 * Dependency-free by design so the file can be copy-synced into `src/shared`
 * later (the same manual-sync convention as `src/shared/json-lines.ts`). The
 * module only normalizes text and applies pattern rules; it never reads files,
 * env vars, or the network.
 */
export type WakeFilterConfig = {
  enabled: boolean;
  extraPatterns: readonly string[];
};

export type UpstreamErrorMatch = { matched: false } | { matched: true; pattern: string };

/**
 * Texts longer than this only count as error-shaped when they start with a
 * recognizable error envelope. The cap keeps normal assistant reports that
 * merely mention "429"/"timeout"/"rate limit" somewhere in a long body from
 * being suppressed.
 */
export const MAX_ERROR_SHAPED_CHARS = 400;

export const DEFAULT_WAKE_FILTER_CONFIG: WakeFilterConfig = {
  enabled: true,
  extraPatterns: [],
};

// A text qualifies as error-shaped when it *starts* with a recognizable error
// envelope (used for both short and long texts) or, for short texts only, when
// it carries a strong error token. Bare status codes, weak daily words, and
// plain `timeout`/`error` are not enough on their own.
const ENVELOPE_PATTERN =
  /^(?:api\s+error|error\s*[:：]|connection\s+error\s*[:：]|the\s+model\s+is\s+(?:currently\s+)?overloaded|econnreset|etimedout|enotfound|eai_again|socket\s+hang\s+up|fetch\s+failed|und_err_|request\s+timed\s+out|timeout\s+of\s+\d+\s*ms\s+exceeded|rate_limit_error|overloaded_error|resource_exhausted|insufficient_quota|you\s+exceeded\s+your\s+current\s+quota|quota\s+exceeded|(?:429|503|529)\s+(?:too\s+many\s+requests|service\s+unavailable|overloaded|bad\s+gateway|gateway\s+timeout))/i;

// Strong, terse error tokens that appear at the *start* of a short message and
// mark it as a genuine error rather than a report that merely mentions one.
// These stay start-anchored so sentences like "Implemented fetch failed fallback
// in transport.ts." are not misclassified by a token buried in prose. The first
// optional branch captures expressions like "The request failed" so that
// sentences starting with natural-language failure phrasing are classified
// without widening all strong-token alternatives.
const START_STRONG_TOKEN_PATTERN =
  /^(?:(?:the\s+)?request\s+failed|rate[_ -]?limit\s+(?:reached|exceeded|hit|error|exhausted)|socket\s+hang\s+up|fetch\s+failed|request\s+timed\s+out|timeout\s+of\s+\d+\s*ms\s+exceeded|quota\s+exceeded|you\s+exceeded\s+your\s+current\s+quota|the\s+model\s+is\s+(?:currently\s+)?overloaded|频率限制(?![与和及已的以而还也但并了在是也])|请求过于频繁(?![与和及已的以而还也但并了在是也])|模型过载(?![与和及已的以而还也但并了在是也])|资源耗尽(?![与和及已的以而还也但并了在是也]))/i;

// Distinctive structured error codes/identifiers that are safe to match anywhere
// (they do not appear as ordinary prose in the classifier's target cases).
const CODE_STRONG_TOKEN_PATTERN =
  /\b(?:rate_limit_error|overloaded_error|resource_exhausted|insufficient_quota|ECONNRESET|ECONNREFUSED|ETIMEDOUT|ENOTFOUND|EAI_AGAIN|und_err_)[\w]*\b/i;

// A status code only counts when it co-occurs with an error-context word on
// the same line and uses a recognized inter-token separator. The pattern is
// start-anchored, uses word boundaries for `error`/`err`/`failed`, and only
// allows `status`, `code`, or `with` as readable prefixes (plus `:=`/`:`/`=`).
// The trailing lookahead requires the code to end the text, be followed by
// punctuation, or be followed by an error-ish word — this is what keeps
// "Error 429 was documented in the README." / "Failed 503 times in the test
// suite." out while "Error 429: rate limited" and bare "error 429" still match.
// This keeps `The request failed with status 503` covered by the strong token
// rather than by a bare status-code match.
const STATUS_CONTEXT_PATTERN =
  /^(?:error|err|failed)\b\s*(?:(?:status|code|with)\s*)*[:=]?\s*(?:429|503|529)\b(?=$|\s*[:：,，。.！!?？;；)]|\s+(?:rate|overload|limit|exceed|quota|unavailable|busy|slow|too\s+many|retry|please|try)\b)/i;

const SUBSTANTIVE_HEADING_PATTERN = /^#{1,6}\s/m;

const DELIMITED_EXTRA_PATTERN = /^\/(.+)\/([gimsuy]*)$/;

const VT_CONTROL_PATTERN = /\u001b\[[0-9;?]*[ -/]*[@-~]/g;

// C0 controls except \t \n \r, the C1 range, and DEL.
const CONTROL_CHARS_PATTERN = /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f]/g;

type BuiltInPattern = { label: string; pattern: RegExp };

const BUILT_IN_PATTERNS: readonly BuiltInPattern[] = [
  // T1 — explicit transport/provider error tokens.
  { label: "api error", pattern: /\bapi\s+error\b\s*[:：]?\s*[45]\d\d\b/i },
  { label: "error envelope", pattern: /\berror\s*[:：]\s*[45]\d\d\b/i },
  { label: "request failed", pattern: /\brequest\s+failed\b/i },
  { label: "rate_limit_error", pattern: /\brate_limit_error\b/i },
  { label: "overloaded_error", pattern: /\boverloaded_error\b/i },
  { label: "resource_exhausted", pattern: /\bresource_exhausted\b/i },
  { label: "insufficient_quota", pattern: /\binsufficient_quota\b/i },
  { label: "you exceeded your current quota", pattern: /\byou\s+exceeded\s+your\s+current\s+quota\b/i },
  { label: "quota exceeded", pattern: /\bquota\s+exceeded\b/i },
  { label: "the model is overloaded", pattern: /\bthe\s+model\s+is\s+(?:currently\s+)?overloaded\b/i },
  // T2 — rate limiting phrasing with an explicit action/failure context.
  { label: "rate limit hit", pattern: /\brate[_ -]?limit\s+(?:reached|exceeded|hit|error|exhausted)\b/i },
  // T3 — transport failures (no bare \btimeout\b / \berror\b matching).
  { label: "connection error", pattern: /\bconnection\s+error\b\s*[:：]/i },
  { label: "econnreset", pattern: /\bECONNRESET\b/i },
  { label: "econnrefused", pattern: /\bECONNREFUSED\b/i },
  { label: "etimedout", pattern: /\bETIMEDOUT\b/i },
  { label: "enotfound", pattern: /\bENOTFOUND\b/i },
  { label: "eai_again", pattern: /\bEAI_AGAIN\b/i },
  { label: "socket hang up", pattern: /\bsocket\s+hang\s+up\b/i },
  { label: "fetch failed", pattern: /\bfetch\s+failed\b/i },
  { label: "und_err_", pattern: /\bund_err_[\w]*/i },
  { label: "request timed out", pattern: /\brequest\s+timed\s+out\b/i },
  { label: "timeout of Nms exceeded", pattern: /\btimeout\s+of\s+\d+\s*ms\s+exceeded\b/i },
  // T4 — bare status codes only when they co-occur with an error context.
  { label: "status code", pattern: /^(?:error|err|failed)\b\s*(?:(?:status|code|with)\s*)*[:=]?\s*(?:429|503|529)\b(?=$|\s*[:：,，。.！!?？;；)]|\s+(?:rate|overload|limit|exceed|quota|unavailable|busy|slow|too\s+many|retry|please|try)\b)/i },
  // T5 — a bare HTTP status line that names the failure itself.
  { label: "bare status line", pattern: /^(?:429|503|529)\s+(?:too\s+many\s+requests|service\s+unavailable|overloaded|bad\s+gateway|gateway\s+timeout)/i },
  // Chinese strong tokens (start-anchored; the negative lookahead keeps
  // explanatory continuations such as "频率限制已修复。" from matching).
  { label: "频率限制", pattern: /^频率限制(?![与和及已的以而还也但并了在是也])/ },
  { label: "请求过于频繁", pattern: /^请求过于频繁(?![与和及已的以而还也但并了在是也])/ },
  { label: "模型过载", pattern: /^模型过载(?![与和及已的以而还也但并了在是也])/ },
  { label: "资源耗尽", pattern: /^资源耗尽(?![与和及已的以而还也但并了在是也])/ },
];

function normalizeText(value: string | null | undefined): string {
  const raw = typeof value === "string" ? value : "";
  return raw
    .replace(VT_CONTROL_PATTERN, "")
    .replace(CONTROL_CHARS_PATTERN, "")
    .replace(/\s+/g, " ")
    .trim();
}

function matchesExtraPattern(text: string, candidate: string): boolean {
  const raw = candidate.trim();
  if (raw.length === 0) return false;
  const delimited = DELIMITED_EXTRA_PATTERN.exec(raw);
  if (delimited) {
    const source = delimited[1] ?? "";
    const flags = delimited[2] ?? "";
    try {
      return new RegExp(source, flags).test(text);
    } catch {
      // Invalid custom regular expressions are skipped without affecting the
      // other rules; the loader logs the warning.
      return false;
    }
  }
  return text.toLowerCase().includes(raw.toLowerCase());
}

/**
 * Error-shaped latch. Long texts must start with a recognizable error envelope.
 * Short texts must also carry an envelope at the start or a strong error token
 * (a start-anchored terse phrase, a distinctive structured code anywhere, or a
 * status code in an error-context). This keeps short, ordinary assistant
 * replies that merely mention `429`, `rate limit`, `503`, etc. from being
 * suppressed by the default-on filter.
 */
export function isErrorShaped(text: string): boolean {
  const normalized = normalizeText(text);
  if (normalized.length === 0) return false;
  if (normalized.length > MAX_ERROR_SHAPED_CHARS) {
    return ENVELOPE_PATTERN.test(normalized.slice(0, 120));
  }
  return (
    ENVELOPE_PATTERN.test(normalized.slice(0, 120)) ||
    START_STRONG_TOKEN_PATTERN.test(normalized.slice(0, 120)) ||
    CODE_STRONG_TOKEN_PATTERN.test(normalized) ||
    STATUS_CONTEXT_PATTERN.test(normalized)
  );
}

function hasSubstantiveWork(normalized: string): boolean {
  if (normalized.includes("```")) return true;
  if (SUBSTANTIVE_HEADING_PATTERN.test(normalized)) return true;
  return (
    normalized.length > MAX_ERROR_SHAPED_CHARS &&
    !ENVELOPE_PATTERN.test(normalized.slice(0, 120))
  );
}

export function matchUpstreamModelError(
  text: string | null | undefined,
  config: WakeFilterConfig = DEFAULT_WAKE_FILTER_CONFIG,
): UpstreamErrorMatch {
  if (!config.enabled) return { matched: false };
  const normalized = normalizeText(text);
  if (normalized.length === 0) return { matched: false };

  // Custom patterns bypass the error-shaped latch: an operator who configures
  // one has explicitly opted into matching it.
  for (const candidate of config.extraPatterns) {
    if (matchesExtraPattern(normalized, candidate)) {
      return { matched: true, pattern: `extra:${candidate.trim()}` };
    }
  }

  if (!isErrorShaped(normalized)) return { matched: false };
  if (hasSubstantiveWork(normalized)) return { matched: false };

  for (const builtIn of BUILT_IN_PATTERNS) {
    if (builtIn.pattern.test(normalized)) return { matched: true, pattern: builtIn.label };
  }
  return { matched: false };
}

export function isUpstreamModelError(
  text: string | null | undefined,
  config: WakeFilterConfig = DEFAULT_WAKE_FILTER_CONFIG,
): boolean {
  return matchUpstreamModelError(text, config).matched;
}
