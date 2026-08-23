import { createReadStream, existsSync, lstatSync, realpathSync } from "node:fs";
import { lstat, readdir, readFile, stat } from "node:fs/promises";
import { isAbsolute, join, normalize, relative } from "node:path";
import { createInterface } from "node:readline";
import { DatabaseSync } from "node:sqlite";
import type { AgentHistoryRef, AgentSessionRef } from "@/observability/contracts.js";

// A pane agent's authoritative session ref (herdr detection or herdsman-pi
// registration) may not have landed when the agent is first observed, so
// discovery falls back to cwd-based guessing. A session file untouched since
// before the agent was first seen cannot be that agent's live session.
// Herdr observation delay is on the second scale, so a 10-minute grace window
// is far more generous than needed; it only discards sessions that had already
// stopped being written well before the agent appeared.
export const ALLOWED_SESSION_ROOTS = ["/root/.pi/agent/sessions", "/tmp/pi-role-sessions"] as const;

export const DISCOVERY_RECENCY_GRACE_MS = 10 * 60_000;
export type AgentHistoryLookupInput = {
  agent: string | null;
  agentSession: AgentSessionRef | null;
  cwd: string | null;
  foregroundCwd: string | null;
  firstSeenAtMs?: number;
  homeDir?: string;
  occupiedSessionPaths?: ReadonlySet<string>;
};

type Candidate = {
  cwd: string | null;
  mtimeMs: number;
  path: string;
  source: AgentHistoryRef["source"];
};

export async function discoverAgentHistory(
  input: AgentHistoryLookupInput,
): Promise<AgentHistoryRef | null> {
  if (input.agentSession?.kind === "path") {
    const resolved = safeAllowedSessionPath(input.agentSession.value, input.homeDir);
    if (resolved && existsSync(resolved)) {
      const source = historySourceFromSessionRef(input.agentSession);
      return { kind: "agent_session", path: resolved, source, value: resolved };
    }
  }

  const cwd = input.cwd ?? input.foregroundCwd;
  const normalizedCwd = normalizeCwd(cwd);
  const homeDir = input.homeDir ?? process.env.HOME ?? "";

  if (input.agentSession?.kind === "id") {
    const source = historySourceFromSessionRef(input.agentSession);
    if (source === "pi-jsonl") {
      const roots = new Set([join(homeDir, ".pi", "agent", "sessions"), ...ALLOWED_SESSION_ROOTS]);
      for (const root of roots) {
        const matches = await scanRootById(root, input.agentSession.value, source);
        const candidate = matches.find((item) => !input.occupiedSessionPaths?.has(item.path));
        if (candidate) {
          return {
            kind: "agent_session",
            path: candidate.path,
            source,
            value: input.agentSession.value,
          };
        }
      }
    }
    if (source === "opencode-sqlite") {
      const ref = discoverOpenCodeSession({ cwd, homeDir, sessionId: input.agentSession.value });
      if (ref) return { ...ref, kind: "agent_session" };
    }
  }

  const agent = input.agent?.toLowerCase() ?? input.agentSession?.agent.toLowerCase() ?? "";
  const candidates: Candidate[] = [];
  if (agent === "pi") {
    const roots = new Set([join(homeDir, ".pi", "agent", "sessions"), ...ALLOWED_SESSION_ROOTS]);
    for (const root of roots) {
      candidates.push(...(await scanRoot(root, "pi-jsonl")));
    }
  }
  if (agent === "claude") {
    candidates.push(...(await scanRoot(join(homeDir, ".claude", "projects"), "claude-jsonl")));
  }
  if (agent === "codex") {
    candidates.push(...(await scanRoot(join(homeDir, ".codex", "sessions"), "codex-jsonl")));
  }
  if (agent === "gemini") {
    candidates.push(...(await scanGeminiRoot(join(homeDir, ".gemini", "tmp"))));
  }
  if (agent === "opencode") {
    const ref = discoverOpenCodeSession({ cwd, homeDir, sessionId: null });
    if (ref) return ref;
  }
  const ranked = candidates
    .filter((candidate) => normalizedCwd !== null && normalizeCwd(candidate.cwd) === normalizedCwd)
    .filter((candidate) => !input.occupiedSessionPaths?.has(candidate.path))
    .filter(
      (candidate) =>
        input.firstSeenAtMs === undefined ||
        candidate.mtimeMs >= input.firstSeenAtMs - DISCOVERY_RECENCY_GRACE_MS,
    )
    .sort((a, b) => {
      if (a.mtimeMs !== b.mtimeMs) return b.mtimeMs - a.mtimeMs;
      return a.path.localeCompare(b.path);
    });
  const best = ranked[0];
  return best
    ? { kind: "discovered_file", path: best.path, source: best.source, value: best.path }
    : null;
}

export function historySourceFromSessionRef(ref: AgentSessionRef): AgentHistoryRef["source"] {
  const agent = ref.agent.toLowerCase();
  const source = ref.source.toLowerCase();
  if (agent === "pi" || source.includes("pi")) return "pi-jsonl";
  if (agent === "claude" || source.includes("claude")) return "claude-jsonl";
  if (agent === "codex" || source.includes("codex")) return "codex-jsonl";
  if (agent === "opencode" || source.includes("opencode")) return "opencode-sqlite";
  if (agent === "gemini" || source.includes("gemini")) return "gemini-json";
  return "unknown";
}

export function safeAllowedSessionPath(value: string, homeDir?: string): string | null {
  if (!isAbsolute(value) || value.includes("..")) return null;
  const resolved = normalize(value);
  try {
    const real = realpathSync(resolved);
    const homeSessionRoot = join(homeDir ?? process.env.HOME ?? "/root", ".pi/agent/sessions");
    const roots = [homeSessionRoot, ...ALLOWED_SESSION_ROOTS];
    return roots.some((root) => {
      const rest = relative(root, real);
      return rest === "" || (!rest.startsWith("..") && !isAbsolute(rest));
    })
      ? real
      : null;
  } catch {
    return null;
  }
}
const CURRENT_EUID = process.geteuid?.() ?? -1;

async function scanRootById(
  root: string,
  id: string,
  source: AgentHistoryRef["source"],
): Promise<Candidate[]> {
  if (!existsSync(root)) return [];
  const rootStats = await stat(root).catch(() => null);
  if (!rootStats || rootStats.uid !== CURRENT_EUID || (rootStats.mode & 0o022) !== 0) {
    console.warn(`Skipping unsafe discovery root: ${root}`);
    return [];
  }
  const files = (await listJsonlFiles(root)).filter((path) => path.split("/").pop()?.includes(id));
  const candidates: Candidate[] = [];
  for (const path of files) {
    const stats = await stat(path).catch(() => null);
    const linkStats = await lstat(path).catch(() => null);
    if (!stats?.isFile() || !linkStats?.isFile() || stats.uid !== CURRENT_EUID) continue;
    candidates.push({ cwd: null, mtimeMs: stats.mtimeMs, path, source });
  }
  return candidates;
}

function normalizeCwd(value: string | null): string | null {
  if (value === null) return null;
  const compact = value.replace(/\/{2,}/g, "/");
  if (compact === "/") return compact;
  return compact.replace(/\/+$/, "");
}

async function scanRoot(root: string, source: AgentHistoryRef["source"]): Promise<Candidate[]> {
  if (!existsSync(root)) return [];
  const rootStats = await stat(root).catch(() => null);
  if (!rootStats || rootStats.uid !== CURRENT_EUID || (rootStats.mode & 0o022) !== 0) {
    console.warn(`Skipping unsafe discovery root: ${root}`);
    return [];
  }
  const files = await listJsonlFiles(root);
  const candidates: Candidate[] = [];
  for (const path of files) {
    const stats = await stat(path).catch(() => null);
    const linkStats = await lstat(path).catch(() => null);
    if (!stats?.isFile() || !linkStats?.isFile() || stats.uid !== CURRENT_EUID) continue;
    candidates.push({ cwd: await readCandidateCwd(path), mtimeMs: stats.mtimeMs, path, source });
  }
  return candidates;
}

const MAX_DISCOVERY_DEPTH = 4;
const MAX_DISCOVERY_FILES = 2000;

async function listJsonlFiles(root: string, depth = 0, state = { count: 0 }): Promise<string[]> {
  if (depth > MAX_DISCOVERY_DEPTH || state.count >= MAX_DISCOVERY_FILES) return [];
  const entries = await readdir(root, { withFileTypes: true }).catch(() => []);
  const files: string[] = [];
  for (const entry of entries) {
    if (state.count >= MAX_DISCOVERY_FILES) break;
    const path = join(root, entry.name);
    if (entry.isDirectory()) files.push(...(await listJsonlFiles(path, depth + 1, state)));
    if (entry.isFile() && entry.name.endsWith(".jsonl")) {
      const candidate = join(root, entry.name);
      if (!lstatSync(candidate, { throwIfNoEntry: false })?.isFile()) continue;
      files.push(candidate);
      state.count += 1;
    }
  }
  return files;
}

async function readCandidateCwd(path: string): Promise<string | null> {
  const input = createReadStream(path, { encoding: "utf8", start: 0, end: 256 * 1024 - 1 });
  const lines = createInterface({ input, crlfDelay: Infinity });
  let inspected = 0;
  try {
    for await (const line of lines) {
      if (line.trim().length === 0) continue;
      inspected += 1;
      try {
        const parsed = JSON.parse(line) as unknown;
        const record = recordValue(parsed);
        const cwd = stringValue(record.cwd) ?? stringValue(record.foreground_cwd);
        if (cwd) return cwd;
        const payload = recordValue(record.payload);
        const payloadCwd = stringValue(payload.cwd) ?? stringValue(payload.foreground_cwd);
        if (payloadCwd) return payloadCwd;
        const message = recordValue(record.message);
        const nestedCwd = stringValue(message.cwd) ?? stringValue(message.foreground_cwd);
        if (nestedCwd) return nestedCwd;
      } catch {
        // Ignore malformed candidate records, preserving existing matching semantics.
      }
      if (inspected >= 100) break;
    }
  } finally {
    lines.close();
    input.destroy();
  }
  return null;
}

async function scanGeminiRoot(root: string): Promise<Candidate[]> {
  if (!existsSync(root)) return [];
  const projectDirs = await listGeminiProjectDirs(root);
  const candidates: Candidate[] = [];
  for (const projectDir of projectDirs) {
    const cwd =
      (await readFile(join(projectDir, ".project_root"), "utf8").catch(() => "")).trim() || null;
    const sessions = await listGeminiSessionFiles(join(projectDir, "chats"));
    for (const path of sessions) {
      const stats = await stat(path).catch(() => null);
      if (!stats?.isFile()) continue;
      candidates.push({ cwd, mtimeMs: stats.mtimeMs, path, source: "gemini-json" });
    }
  }
  return candidates;
}

async function listGeminiProjectDirs(root: string): Promise<string[]> {
  const entries = await readdir(root, { withFileTypes: true }).catch(() => []);
  const dirs: string[] = [];
  for (const entry of entries) {
    const path = join(root, entry.name);
    if (!entry.isDirectory()) continue;
    if (existsSync(join(path, ".project_root"))) dirs.push(path);
  }
  return dirs;
}

async function listGeminiSessionFiles(chatsDir: string): Promise<string[]> {
  const entries = await readdir(chatsDir, { withFileTypes: true }).catch(() => []);
  return entries
    .filter(
      (entry) =>
        entry.isFile() && entry.name.startsWith("session-") && entry.name.endsWith(".json"),
    )
    .map((entry) => join(chatsDir, entry.name));
}

function discoverOpenCodeSession(input: {
  cwd: string | null;
  homeDir: string;
  sessionId: string | null;
}): AgentHistoryRef | null {
  const dbPath = resolveOpenCodeDbPath(input.homeDir);
  if (!existsSync(dbPath)) return null;
  let sqlite: DatabaseSync | null = null;
  try {
    sqlite = new DatabaseSync(dbPath, { readOnly: true });
    if (input.sessionId) {
      const row = sqlite
        .prepare("select id from session where id = ? limit 1")
        .get(input.sessionId) as { id: string } | undefined;
      return row
        ? { kind: "discovered_file", path: dbPath, source: "opencode-sqlite", value: row.id }
        : null;
    }
    if (!input.cwd) return null;
    const row = sqlite
      .prepare("select id from session where directory = ? order by time_updated desc limit 1")
      .get(input.cwd) as { id: string } | undefined;
    return row
      ? { kind: "discovered_file", path: dbPath, source: "opencode-sqlite", value: row.id }
      : null;
  } catch {
    return null;
  } finally {
    sqlite?.close();
  }
}

function resolveOpenCodeDbPath(homeDir: string): string {
  const override = process.env.OPENCODE_DB;
  if (override && override !== ":memory:") {
    return override.startsWith("/")
      ? override
      : join(homeDir, ".local", "share", "opencode", override);
  }
  return join(homeDir, ".local", "share", "opencode", "opencode.db");
}

function recordValue(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" && value.length > 0 ? value : null;
}
