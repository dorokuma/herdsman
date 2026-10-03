import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import {
  discoverAgentHistory,
  safeAllowedSessionPath,
  safeOfficialSessionPath,
} from "@/agent-history/discovery.js";

const HERDR_ROLE_SESSIONS_ROOT = "/tmp/herdr-role-sessions";
const RETIRED_ROLE_SESSIONS_ROOT = "/tmp/pi-role-sessions";
const tempDirs: string[] = [];

vi.setConfig({ testTimeout: 30_000 });

afterEach(async () => {
  vi.restoreAllMocks();
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

async function tempRoot(name: string): Promise<string> {
  const dir = await mkdtemp(join("/tmp", name));
  tempDirs.push(dir);
  return dir;
}

async function sessionFile(dir: string, name: string): Promise<string> {
  await mkdir(dir, { recursive: true });
  const path = join(dir, `${name}.jsonl`);
  await writeFile(path, `${JSON.stringify({ cwd: "/repo" })}\n`);
  return path;
}

describe("agent history discovery (official value only)", () => {
  test("safeAllowedSessionPath keeps the root whitelist for herdsman-pi registration", async () => {
    await mkdir(HERDR_ROLE_SESSIONS_ROOT, { recursive: true });
    const root = await mkdtemp(join(HERDR_ROLE_SESSIONS_ROOT, "herdsman-regression-"));
    tempDirs.push(root);
    const path = await sessionFile(root, "safe");
    expect(safeAllowedSessionPath(path, "/nonexistent")).toBe(path);
    expect(safeAllowedSessionPath("relative/session.jsonl", "/nonexistent")).toBeNull();
    expect(safeAllowedSessionPath(join(root, "..", "escape"), "/nonexistent")).toBeNull();
    expect(
      safeAllowedSessionPath("/tmp/not-an-allowed-root/session.jsonl", "/nonexistent"),
    ).toBeNull();
  });

  test("safeOfficialSessionPath keeps the file-safety checks without a root whitelist", async () => {
    const root = await tempRoot("herdsman-official-shape-");
    const path = await sessionFile(root, "official-session");
    expect(safeOfficialSessionPath(path)).toBe(path);
    expect(safeOfficialSessionPath("relative/session.jsonl")).toBeNull();
    expect(safeOfficialSessionPath(join(root, "..", "escape"))).toBeNull();
    expect(safeOfficialSessionPath(join(root, "missing.jsonl"))).toBeNull();

    const directory = join(root, "a-directory.jsonl");
    await mkdir(directory, { recursive: true });
    expect(safeOfficialSessionPath(directory)).toBeNull();
  });

  test("a plausible session file on disk is never picked up without an official value", async () => {
    const root = await tempRoot("herdsman-no-scan-");
    await sessionFile(join(root, ".pi", "agent", "sessions"), "leaked");
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: null,
        herdrSessionName: "default",
        homeDir: root,
        paneId: "wA:p1",
      }),
    ).resolves.toBeNull();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("no usable official Herdr agent session"),
      expect.objectContaining({ paneId: "wA:p1" }),
    );
  });

  test("an official id is never resolved by walking a session directory", async () => {
    const root = await tempRoot("herdsman-no-traversal-");
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    // Both files carry the official id in their name, which is exactly what the
    // deleted scanners matched on. Nothing walks these trees any more: an id is
    // only resolved through its agent's deterministic template (`agy`), so an
    // unsupported agent's session file sitting on disk stays unresolved.
    await sessionFile(
      join(root, ".codex", "sessions", "2026", "07", "09"),
      "rollout-2026-07-09T10-00-00-aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
    );
    await sessionFile(
      join(root, ".claude", "projects", "-repo"),
      "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
    );

    for (const [agent, source, detail] of [
      ["codex", "herdr:codex", "is not a supported herdsman history agent"],
      ["claude", "herdr:claude", "is not a supported herdsman history agent"],
      // pi *is* supported, so its id-shaped value must not be described as an
      // unsupported agent: it is the documented gap (no id-based derivation).
      ["pi", "herdr:pi", "reported an id; herdsman has no id-based session-file derivation"],
    ] as const) {
      await expect(
        discoverAgentHistory({
          agent,
          agentSession: {
            agent,
            kind: "id",
            source,
            value: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
          },
          herdrSessionName: "default",
          homeDir: root,
          paneId: "wA:p1",
        }),
      ).resolves.toBeNull();
      expect(warn).toHaveBeenCalledWith(
        expect.stringContaining("no usable official Herdr agent session"),
        expect.objectContaining({
          detail: expect.stringContaining(detail),
          paneId: "wA:p1",
        }),
      );
    }
    expect(warn).toHaveBeenCalledTimes(3);
  });

  test("an official path under a retired role root is accepted; the registration whitelist still rejects it", async () => {
    await mkdir(RETIRED_ROLE_SESSIONS_ROOT, { recursive: true });
    const oldRoot = await mkdtemp(join(RETIRED_ROLE_SESSIONS_ROOT, "retired-"));
    tempDirs.push(oldRoot);
    const oldPath = await sessionFile(oldRoot, "role-old");

    // The registration whitelist only knows the current role root.
    expect(safeAllowedSessionPath(oldPath, "/nonexistent")).toBeNull();
    // An official value is consumed as reported, whatever root it lives in.
    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: { agent: "pi", kind: "path", source: "herdr:pi", value: oldPath },
        herdrSessionName: "default",
        paneId: "wA:p9",
      }),
    ).resolves.toMatchObject({ kind: "agent_session", path: oldPath, source: "pi-jsonl" });
  });
});
