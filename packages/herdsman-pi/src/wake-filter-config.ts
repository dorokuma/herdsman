/**
 * Pi-side reader for the `wake` section of `$HERDSMAN_HOME/config.yaml`.
 *
 * The Herdsman daemon validates `config.yaml` with the Runtime schema, but the
 * Pi extension is a standalone, zero-runtime-dependency npm package that must
 * not import `yaml` or the daemon config modules. It therefore parses only the
 * documented YAML subset it needs:
 *
 *   wake:
 *     filter_upstream_errors: false
 *     extra_upstream_error_patterns:
 *       - "overloaded"
 *       - /foo\s+bar/i
 *
 * Supported subset: the top-level `wake:` mapping, 2-space indentation,
 * `true`/`false` booleans, `- item` lists, `#` line comments, and quoted
 * strings. Anything else falls back to the defaults with a warning in the
 * Herdsman Pi log; the extension is never blocked by a malformed file.
 *
 * Environment overrides win over the file so operators (and tests) can flip the
 * filter without editing YAML:
 *   - HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS=true|false|1|0
 *   - HERDSMAN_WAKE_EXTRA_UPSTREAM_ERROR_PATTERNS (newline- or comma-separated)
 */
import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join } from "node:path";
import { logHerdsmanPi } from "./logger.js";
import { DEFAULT_WAKE_FILTER_CONFIG, type WakeFilterConfig } from "./upstream-error.js";

const WAKE_SECTION_KEY = "wake";
const FILTER_KEY = "filter_upstream_errors";
const EXTRA_PATTERNS_KEY = "extra_upstream_error_patterns";
const FILTER_ENV = "HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS";
const EXTRA_PATTERNS_ENV = "HERDSMAN_WAKE_EXTRA_UPSTREAM_ERROR_PATTERNS";
const DEFAULT_HOME_NAME = ".herdsman";
const DELIMITED_EXTRA_PATTERN = /^\/(.+)\/([gimsuy]*)$/;

type WakeFileValues = {
  extraPatterns: string[];
  filterUpstreamErrors: boolean;
};

type WakeFileParse =
  | undefined
  | { ok: true; value: WakeFileValues }
  | { message: string; ok: false };

function defaultHerdsmanHome(): string {
  const configured = process.env.HERDSMAN_HOME?.trim();
  return configured && isAbsolute(configured)
    ? configured
    : join(homedir(), DEFAULT_HOME_NAME);
}

function warn(message: string): void {
  logHerdsmanPi("warn", `[herdsman-pi] ${message}`);
}

function stripComment(line: string): string {
  let quote: string | undefined;
  for (let index = 0; index < line.length; index += 1) {
    const char = line[index];
    if (quote) {
      if (char === quote) quote = undefined;
      continue;
    }
    if (char === "'" || char === '"') {
      quote = char;
      continue;
    }
    if (char === "#") return line.slice(0, index);
  }
  return line;
}

function indentOf(line: string): number {
  let indent = 0;
  while (indent < line.length && line[indent] === " ") indent += 1;
  return indent;
}

function unquote(value: string): string | undefined {
  const trimmed = value.trim();
  if (trimmed.length === 0) return "";
  if (
    (trimmed.startsWith('"') && trimmed.endsWith('"') && trimmed.length >= 2) ||
    (trimmed.startsWith("'") && trimmed.endsWith("'") && trimmed.length >= 2)
  ) {
    return trimmed.slice(1, -1);
  }
  if (trimmed.startsWith('"') || trimmed.startsWith("'")) return undefined;
  return trimmed;
}

function parseBooleanScalar(value: string): boolean | undefined {
  const normalized = value.trim().toLowerCase();
  if (normalized === "true") return true;
  if (normalized === "false") return false;
  return undefined;
}

/**
 * Parses the documented subset of the `wake:` mapping. Returns `undefined` when
 * the file has no `wake:` section. Unknown keys inside the section are ignored
 * so a future daemon-only addition does not break the Pi reader.
 */
function parseWakeSection(source: string): WakeFileParse {
  const lines = source.split(/\r?\n/);
  let sectionStart = -1;
  for (let index = 0; index < lines.length; index += 1) {
    const line = stripComment(lines[index] ?? "");
    if (line.trim() === `${WAKE_SECTION_KEY}:`) {
      sectionStart = index;
      break;
    }
  }
  if (sectionStart < 0) return undefined;

  const value: WakeFileValues = {
    extraPatterns: [],
    filterUpstreamErrors: DEFAULT_WAKE_FILTER_CONFIG.enabled,
  };
  let currentListKey: string | undefined;

  for (let index = sectionStart + 1; index < lines.length; index += 1) {
    const rawLine = stripComment(lines[index] ?? "");
    if (rawLine.trim().length === 0) continue;
    const indent = indentOf(rawLine);
    if (indent === 0) break;
    if (rawLine.includes("\t")) {
      return { ok: false, message: `tab indentation is not supported (line ${index + 1})` };
    }
    const body = rawLine.trim();

    if (body.startsWith("-")) {
      // List items may align with the key (2 spaces) or nest one level deeper
      // (4 spaces), which is how most hand-written configs indent them.
      if (indent !== 2 && indent !== 4) {
        return { ok: false, message: `unexpected list indentation (line ${index + 1})` };
      }
      if (currentListKey !== EXTRA_PATTERNS_KEY) {
        return { ok: false, message: `unexpected list item (line ${index + 1})` };
      }
      const item = unquote(body.slice(1));
      if (item === undefined || item.length === 0) {
        return { ok: false, message: `invalid list item (line ${index + 1})` };
      }
      value.extraPatterns.push(item);
      continue;
    }

    if (indent !== 2) {
      return { ok: false, message: `expected 2-space indentation (line ${index + 1})` };
    }

    const separator = body.indexOf(":");
    if (separator <= 0) {
      return { ok: false, message: `invalid mapping entry (line ${index + 1})` };
    }
    const key = body.slice(0, separator).trim();
    const rawValue = body.slice(separator + 1);
    if (key === FILTER_KEY) {
      const normalized = rawValue.trim();
      if (normalized === "[]" || normalized === "{}") {
        return { ok: false, message: `invalid boolean for ${FILTER_KEY} (line ${index + 1})` };
      }
      const parsed = parseBooleanScalar(normalized);
      if (parsed === undefined) {
        return { ok: false, message: `invalid boolean for ${FILTER_KEY} (line ${index + 1})` };
      }
      value.filterUpstreamErrors = parsed;
      currentListKey = key;
      continue;
    }
    if (key === EXTRA_PATTERNS_KEY) {
      const normalized = rawValue.trim();
      if (normalized === "[]") {
        value.extraPatterns = [];
        currentListKey = undefined;
        continue;
      }
      if (normalized.length > 0) {
        return {
          ok: false,
          message: `inline ${EXTRA_PATTERNS_KEY} values are not supported (line ${index + 1})`,
        };
      }
      currentListKey = key;
      continue;
    }
    currentListKey = undefined;
  }

  return { ok: true, value };
}

function parseFilterEnv(value: string | undefined): boolean | undefined {
  if (value === undefined) return undefined;
  const normalized = value.trim().toLowerCase();
  if (normalized === "true" || normalized === "1") return true;
  if (normalized === "false" || normalized === "0") return false;
  warn(`${FILTER_ENV} has an unsupported value; ignoring it`);
  return undefined;
}

function parseExtraPatternsEnv(value: string | undefined): string[] | undefined {
  if (value === undefined) return undefined;
  const parts = value.includes("\n") ? value.split("\n") : value.split(",");
  return parts.map((part) => part.trim()).filter((part) => part.length > 0);
}

function extraPatternWarning(candidates: readonly string[]): string | undefined {
  for (const candidate of candidates) {
    const delimited = DELIMITED_EXTRA_PATTERN.exec(candidate.trim());
    if (!delimited) continue;
    try {
      new RegExp(delimited[1] ?? "", delimited[2] ?? "");
    } catch {
      return `invalid extra upstream error pattern ${candidate}`;
    }
  }
  return undefined;
}

export function loadWakeFilterConfig(environment: NodeJS.ProcessEnv = process.env): WakeFilterConfig {
  let config: WakeFilterConfig = { ...DEFAULT_WAKE_FILTER_CONFIG, extraPatterns: [] };

  const configPath = join(defaultHerdsmanHome(), "config.yaml");
  let source: string | undefined;
  try {
    source = readFileSync(configPath, "utf8");
  } catch (error) {
    const code = (error as { code?: string }).code;
    if (code !== "ENOENT") {
      warn(
        `could not read ${configPath} for wake filter config; using defaults (${
          error instanceof Error ? error.message : String(error)
        })`,
      );
    }
  }

  if (source !== undefined) {
    const parsed = parseWakeSection(source);
    if (parsed?.ok) {
      config = {
        enabled: parsed.value.filterUpstreamErrors,
        extraPatterns: [...parsed.value.extraPatterns],
      };
    } else if (parsed) {
      warn(`invalid wake filter config in ${configPath}: ${parsed.message}; using defaults`);
    }
  }

  const envFilter = parseFilterEnv(environment[FILTER_ENV]);
  if (envFilter !== undefined) config = { ...config, enabled: envFilter };
  const envPatterns = parseExtraPatternsEnv(environment[EXTRA_PATTERNS_ENV]);
  if (envPatterns !== undefined) config = { ...config, extraPatterns: envPatterns };

  const invalidPattern = extraPatternWarning(config.extraPatterns);
  if (invalidPattern) warn(`${invalidPattern}; it will never match`);

  return config;
}
