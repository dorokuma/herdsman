import { lstatSync, realpathSync } from "node:fs";
import { isAbsolute, join, normalize, relative } from "node:path";
import type { AgentHistoryRef, AgentSessionRef } from "@/observability/contracts.js";

/**
 * Herdsman supports session history for exactly two agents, by decision:
 *
 * 1. `pi` — Herdr's own integration reports the session file itself, so the
 *    official `agent_session.kind == "path"` value is used as-is after the
 *    file-safety gate (this is the generic path channel, not a pi-only branch).
 * 2. `agy` — Herdr reports only `kind == "id"`, and an agy conversation id is
 *    the name of its own conversation database, so the id resolves to one
 *    deterministic candidate per configured agy home (primary `HOME` first,
 *    followed by `AGY_EXTRA_HOMES` / `DEFAULT_AGY_EXTRA_HOMES`):
 *      - `agy` -> `<home>/.gemini/antigravity-cli/conversations/<id>.db`
 *
 * Every other agent — whether or not Herdr has an integration for it — is
 * deliberately not resolved: herdsman does not walk agent directories, rank
 * candidates by mtime/cwd, match role directories from the terminal title, or
 * pick "the newest plausible file". An unresolvable pane is an explicit failure:
 * one `console.warn` naming the official values plus the derived candidate path,
 * then `null`.
 */
export const ALLOWED_SESSION_ROOTS = ["/tmp/herdr-role-sessions"] as const;
export const DEFAULT_AGY_EXTRA_HOMES = ["/root/.agy-pro-home"] as const;

export type AgentHistoryLookupInput = {
  agent: string | null;
  agentSession: AgentSessionRef | null;
  extraHomeDirs?: readonly string[];
  herdrSessionName?: string;
  homeDir?: string;
  paneId?: string | null;
};

/**
 * A pane whose official value cannot be resolved must not be re-resolved — nor
 * re-warned — on every refresh round. The failure is remembered per official
 * input (agent, kind, source, value, store root) for
 * `DISCOVERY_RETRY_AFTER_FAILURE_MS`: within that window the lookup is skipped
 * entirely, and only the first failure for a key logs a warning. Any change in
 * the official values (or in the store root) is a new key and is attempted
 * immediately, and the window is deliberately bounded so a session file that
 * appears a few seconds after the pane reports its id is still picked up.
 *
 * The memory only decelerates the ordinary cadence — the background refresh the
 * daemon runs every round. It never answers a caller that is asking right now: an
 * explicit read (`agent.get` / `agent.read`) and an external forced refresh pass
 * `options.force` and resolve for real. The warning stays deduplicated per key,
 * so forcing does not produce extra noise.
 */
const DISCOVERY_RETRY_AFTER_FAILURE_MS = 60_000;
const DISCOVERY_FAILURE_CACHE_LIMIT = 512;
const failedDiscoveryAt = new Map<string, number>();
const warnedDiscoveryKeys = new Set<string>();

export function resolveAgyExtraHomes(
  extraHomeDirs?: readonly string[],
  envValue = process.env.AGY_EXTRA_HOMES,
): string[] {
  const raw =
    extraHomeDirs !== undefined
      ? extraHomeDirs
      : envValue !== undefined
        ? envValue.split(":")
        : DEFAULT_AGY_EXTRA_HOMES;
  return raw.map((entry) => entry.trim()).filter(Boolean);
}

export function resolveAgyHomeDirs(
  input: { extraHomeDirs?: readonly string[]; homeDir?: string } = {},
): string[] {
  const primary = (input.homeDir ?? process.env.HOME ?? "").trim();
  const homes: string[] = [];
  const seen = new Set<string>();
  for (const candidate of [primary, ...resolveAgyExtraHomes(input.extraHomeDirs)]) {
    if (!candidate) continue;
    const normalized = normalize(candidate);
    if (seen.has(normalized)) continue;
    seen.add(normalized);
    homes.push(candidate);
  }
  if (homes.length === 0) {
    homes.push("");
  }
  return homes;
}

export function discoveryLookupKey(input: AgentHistoryLookupInput): string {
  return [
    input.agent ?? "",
    input.agentSession?.kind ?? "",
    input.agentSession?.source ?? "",
    input.agentSession?.value ?? "",
    input.homeDir ?? process.env.HOME ?? "",
    resolveAgyExtraHomes(input.extraHomeDirs).join(":"),
  ].join("\u0000");
}

/**
 * True while the very same official values are still inside their retry window,
 * so callers can skip the lookup the pane would otherwise repeat every round.
 */
export function discoveryFailureFresh(input: AgentHistoryLookupInput, now = Date.now()): boolean {
  const at = failedDiscoveryAt.get(discoveryLookupKey(input));
  return at !== undefined && now - at < DISCOVERY_RETRY_AFTER_FAILURE_MS;
}

function rememberDiscoveryFailure(
  key: string,
  input: AgentHistoryLookupInput,
  detail: string,
): void {
  failedDiscoveryAt.delete(key);
  failedDiscoveryAt.set(key, Date.now());
  if (failedDiscoveryAt.size > DISCOVERY_FAILURE_CACHE_LIMIT) {
    const oldest = failedDiscoveryAt.keys().next();
    if (!oldest.done) failedDiscoveryAt.delete(oldest.value);
  }
  if (warnedDiscoveryKeys.has(key)) return;
  warnedDiscoveryKeys.add(key);
  if (warnedDiscoveryKeys.size > DISCOVERY_FAILURE_CACHE_LIMIT) {
    const oldest = warnedDiscoveryKeys.values().next();
    if (!oldest.done) warnedDiscoveryKeys.delete(oldest.value);
  }
  reportMissingOfficialSession(input, detail);
}

export async function discoverAgentHistory(
  input: AgentHistoryLookupInput,
  options: { force?: boolean } = {},
): Promise<AgentHistoryRef | null> {
  const key = discoveryLookupKey(input);
  if (!options.force) {
    const failureAt = failedDiscoveryAt.get(key);
    if (failureAt !== undefined && Date.now() - failureAt < DISCOVERY_RETRY_AFTER_FAILURE_MS) {
      return null;
    }
  }
  const fail = (detail: string): null => {
    rememberDiscoveryFailure(key, input, detail);
    return null;
  };
  const session = input.agentSession;
  if (!session) {
    return fail("Herdr reported no agent_session for this pane; herdsman does not guess one");
  }
  if (session.kind === "path") {
    const resolved = safeOfficialSessionPath(session.value);
    if (!resolved) {
      return fail(
        `Herdr agent_session path is not a readable session file owned by herdsman: ${session.value}`,
      );
    }
    return {
      kind: "agent_session",
      path: resolved,
      source: historySourceFromSessionRef(session),
      value: resolved,
    };
  }
  const resolvedById = resolveSessionFileById({
    ...(input.extraHomeDirs === undefined ? {} : { extraHomeDirs: input.extraHomeDirs }),
    ...(input.homeDir === undefined ? {} : { homeDir: input.homeDir }),
    session,
  });
  if (resolvedById.ref) return resolvedById.ref;
  return fail(resolvedById.detail);
}

/**
 * Deterministic id -> session file resolution for `agy` across primary `HOME`
 * and configured extra homes (`AGY_EXTRA_HOMES` / `DEFAULT_AGY_EXTRA_HOMES`).
 * Each candidate home derives one path from the official id alone and verifies
 * it through `containedFileRef` (`safeRegularFile` + `isInside`). Hits are
 * deduplicated by `realpath` while preserving home priority (primary/Ultra
 * first).
 *
 * An agent without a template here is not guessed. That splits in two, and the
 * warning has to say which one it is:
 * - a supported agent whose official value arrived as an id (pi): herdsman has no
 *   id-based derivation for it, so this pane reports no history;
 * - an agent herdsman does not support at all.
 */
export function resolveAgySessionCandidates(input: {
  extraHomeDirs?: readonly string[];
  homeDir?: string;
  id: string;
}): { detail: string; refs: AgentHistoryRef[] } {
  if (!isUuidLike(input.id)) {
    return { detail: `agy conversation id is not a uuid: ${input.id}`, refs: [] };
  }
  const homes = resolveAgyHomeDirs({
    ...(input.extraHomeDirs === undefined ? {} : { extraHomeDirs: input.extraHomeDirs }),
    ...(input.homeDir === undefined ? {} : { homeDir: input.homeDir }),
  });
  const refs: AgentHistoryRef[] = [];
  const seenRealpaths = new Set<string>();
  const hitDetails: string[] = [];
  const missDetails: string[] = [];
  for (const homeDir of homes) {
    const dir = join(homeDir, ".gemini", "antigravity-cli", "conversations");
    const resolution = containedFileRef({
      candidate: join(dir, `${input.id}.db`),
      id: input.id,
      label: "agy",
      root: dir,
      source: "antigravity-sqlite",
    });
    const resolvedPath = resolution.ref?.path;
    if (resolution.ref && resolvedPath) {
      if (!seenRealpaths.has(resolvedPath)) {
        seenRealpaths.add(resolvedPath);
        refs.push(resolution.ref);
        hitDetails.push(resolution.detail);
      }
    } else {
      missDetails.push(resolution.detail);
    }
  }
  return {
    detail: hitDetails[0] ?? missDetails.join("; "),
    refs,
  };
}

function resolveSessionFileById(input: {
  extraHomeDirs?: readonly string[];
  homeDir?: string;
  session: AgentSessionRef;
}): SessionFileResolution {
  const source = historySourceFromSessionRef(input.session);
  const id = input.session.value;
  const miss = (detail: string): SessionFileResolution => ({ detail, ref: null });
  if (source === "antigravity-sqlite") {
    const resolved = resolveAgySessionCandidates({
      ...(input.extraHomeDirs === undefined ? {} : { extraHomeDirs: input.extraHomeDirs }),
      ...(input.homeDir === undefined ? {} : { homeDir: input.homeDir }),
      id,
    });
    return { detail: resolved.detail, ref: resolved.refs[0] ?? null };
  }
  if (source !== "unknown") {
    return miss(
      `${input.session.agent} reported an id; herdsman has no id-based session-file derivation for it (only agy does)`,
    );
  }
  return miss(
    `${input.session.agent} is not a supported herdsman history agent (only pi and agy are)`,
  );
}

type SessionFileResolution = { detail: string; ref: AgentHistoryRef | null };

/**
 * One candidate path, verified: regular file, not a symlink, owned by herdsman,
 * not writable by other principals, and really inside the agent's store root.
 * Any failure is a miss (the caller reports it), never a fallback search.
 */
function containedFileRef(input: {
  candidate: string;
  id: string;
  label: string;
  root: string;
  source: AgentHistoryRef["source"];
}): SessionFileResolution {
  const real = safeOfficialSessionPath(input.candidate);
  if (!real) {
    return {
      detail: `${input.label}: ${input.candidate} is not a readable session file`,
      ref: null,
    };
  }
  if (!isInside(input.root, real)) {
    return { detail: `${input.label}: ${real} escapes ${input.root}`, ref: null };
  }
  return {
    detail: `${input.label}: resolved ${real}`,
    ref: { kind: "agent_session", path: real, source: input.source, value: input.id },
  };
}

function isInside(root: string, path: string): boolean {
  const realRoot = realpathOrNormalize(root);
  const rest = relative(realRoot, path);
  return rest !== "" && !rest.startsWith("..") && !isAbsolute(rest);
}

function realpathOrNormalize(value: string): string {
  try {
    return realpathSync(value);
  } catch {
    return normalize(value);
  }
}

function isUuidLike(value: string): boolean {
  // Version-agnostic on purpose: agy conversation ids are uuids, and a uuid must
  // stay pure hex+dashes so it can never act as a path.
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

function reportMissingOfficialSession(input: AgentHistoryLookupInput, detail: string): void {
  console.warn("Herdsman has no usable official Herdr agent session", {
    agent: input.agent,
    agentSession: input.agentSession,
    detail,
    herdrSessionName: input.herdrSessionName ?? null,
    paneId: input.paneId ?? null,
  });
}

/**
 * Maps an official `agent_session` to the reader that can read it. Only the two
 * supported agents have a reader; everything else is `unknown`, which every
 * reader rejects. `source` looks like `herdr:<agent>`, so segments are matched
 * whole — `herdr:copilot` contains "pi" and must never be read as a pi session.
 */
export function historySourceFromSessionRef(ref: AgentSessionRef): AgentHistoryRef["source"] {
  const agent = ref.agent.toLowerCase();
  const segments = [agent, ...ref.source.toLowerCase().split(/[^a-z0-9_]+/)].filter(Boolean);
  const is = (...names: string[]) => names.some((name) => segments.includes(name));
  if (is("agy", "antigravity", "antigravity_cli", "antigravity-cli")) return "antigravity-sqlite";
  if (is("pi")) return "pi-jsonl";
  return "unknown";
}

export function safeAllowedSessionPath(value: string, homeDir?: string): string | null {
  const real = safeRegularFile(value);
  if (!real) return null;
  const homeSessionRoot = join(homeDir ?? process.env.HOME ?? "/root", ".pi/agent/sessions");
  const roots = [homeSessionRoot, ...ALLOWED_SESSION_ROOTS];
  return roots.some((root) => {
    const rest = relative(root, real);
    return rest === "" || (!rest.startsWith("..") && !isAbsolute(rest));
  })
    ? real
    : null;
}

/**
 * File-safety gate shared by `safeOfficialSessionPath` and
 * `safeAllowedSessionPath`.
 *
 * The raw path is checked with `lstatSync` **before** it is resolved: after
 * `realpathSync` the symlink itself is gone, so an `isSymbolicLink()` check on
 * the resolved path could never fire. A symlink (or anything that is not a
 * regular file) at the reported path is rejected outright; the resolved target
 * is then checked again, so a hard-link/symlink swap behind a directory cannot
 * smuggle in a file herdsman does not own.
 */
function safeRegularFile(value: string): string | null {
  if (!isAbsolute(value) || value.includes("..")) return null;
  const reported = normalize(value);
  try {
    const raw = lstatSync(reported);
    if (raw.isSymbolicLink() || !raw.isFile()) return null;
    const real = realpathSync(reported);
    const target = lstatSync(real);
    if (
      target.isSymbolicLink() ||
      !target.isFile() ||
      target.uid !== CURRENT_EUID ||
      (target.mode & 0o022) !== 0
    )
      return null;
    return real;
  } catch {
    return null;
  }
}

/**
 * File-safety gate for an official session path reported by Herdr.
 *
 * Herdr only reports back the value an installed agent integration handed it
 * (`pane.report_agent_session`), so the *root* of an official path is not
 * guessed any more and is deliberately not whitelisted: pi keeps its sessions
 * under its own agent home, and a fixed root list would silently reject those
 * official values. What is still enforced is that herdsman only reads a regular,
 * non-symlink file it owns and that no other principal can rewrite behind it.
 * `safeAllowedSessionPath` keeps the root whitelist for the herdsman-pi
 * registration protocol, where a pane claims a path itself.
 */
export function safeOfficialSessionPath(value: string): string | null {
  return safeRegularFile(value);
}

const CURRENT_EUID = process.geteuid?.() ?? -1;

export function sessionPathAllowedByShape(value: string, homeDir?: string): boolean {
  if (!isAbsolute(value) || value.includes("..")) return false;
  const resolved = normalize(value);
  const homeSessionRoot = join(homeDir ?? process.env.HOME ?? "/root", ".pi/agent/sessions");
  const roots = [homeSessionRoot, ...ALLOWED_SESSION_ROOTS];
  return roots.some((root) => {
    const rest = relative(root, resolved);
    return rest !== "" && !rest.startsWith("..") && !isAbsolute(rest);
  });
}
