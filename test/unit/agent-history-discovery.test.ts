import { chmodSync, symlinkSync } from "node:fs";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import {
  discoverAgentHistory,
  historySourceFromSessionRef,
  resolveAgySessionCandidates,
  safeAllowedSessionPath,
  safeOfficialSessionPath,
} from "@/agent-history/discovery.js";

const tempDirs: string[] = [];

afterEach(async () => {
  vi.restoreAllMocks();
  vi.unstubAllEnvs();
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

async function tempHome(name: string) {
  const dir = await mkdtemp(join(tmpdir(), name));
  tempDirs.push(dir);
  return dir;
}

const AGY_ID = "01714b30-5613-49c9-b3a2-ce3f9e7406c1";

async function agyConversations(homeDir: string) {
  const dir = join(homeDir, ".gemini", "antigravity-cli", "conversations");
  await mkdir(dir, { recursive: true });
  return dir;
}

function agyLookup(homeDir: string, value: string, extra: Record<string, unknown> = {}) {
  return discoverAgentHistory({
    agent: "agy",
    agentSession: { agent: "agy", kind: "id", source: "herdr:antigravity_cli", value },
    homeDir,
    ...extra,
  });
}

describe("agent history discovery", () => {
  test("takes the official Herdr agent_session path as the history reference", async () => {
    const homeDir = await tempHome("herdsman-official-path-");
    const sessionPath = join(homeDir, ".pi", "agent", "sessions", "ses-1.jsonl");
    await mkdir(join(homeDir, ".pi", "agent", "sessions"), { recursive: true });
    await writeFile(sessionPath, "{}\n");

    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: { agent: "pi", kind: "path", source: "herdr:pi", value: sessionPath },
        homeDir,
        herdrSessionName: "default",
        paneId: "wA:p1",
      }),
    ).resolves.toEqual({
      kind: "agent_session",
      path: sessionPath,
      source: "pi-jsonl",
      value: sessionPath,
    });
  });

  test("accepts an official path outside the herdsman-pi session roots", async () => {
    // The path channel is generic: Herdr hands herdsman the value an installed
    // agent integration reported, so the root of that value is not whitelisted
    // (the whitelist only guards the herdsman-pi registration protocol). The
    // reference is returned as-is; whether a reader supports the agent is a
    // separate question, and the source is mapped honestly.
    const homeDir = await tempHome("herdsman-official-omp-");
    const dir = join(homeDir, ".omp", "sessions");
    await mkdir(dir, { recursive: true });
    const sessionPath = join(dir, "ses-2.jsonl");
    await writeFile(sessionPath, "{}\n");

    await expect(
      discoverAgentHistory({
        agent: "omp",
        agentSession: { agent: "omp", kind: "path", source: "herdr:omp", value: sessionPath },
        homeDir,
      }),
    ).resolves.toEqual({
      kind: "agent_session",
      path: sessionPath,
      source: "unknown",
      value: sessionPath,
    });
  });

  test("reports and refuses a missing official agent_session instead of scanning", async () => {
    // The pane's own conversation database exists under the temp home. Discovery
    // must not fall back to it: with no official value on the pane, herdsman has
    // nothing to read.
    const homeDir = await tempHome("herdsman-no-official-value-");
    const conversations = await agyConversations(homeDir);
    await writeFile(join(conversations, `${AGY_ID}.db`), "");
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    await expect(
      discoverAgentHistory({
        agent: "agy",
        agentSession: null,
        herdrSessionName: "default",
        paneId: "wA:p2",
      }),
    ).resolves.toBeNull();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("no usable official Herdr agent session"),
      expect.objectContaining({
        agent: "agy",
        detail: expect.stringContaining("no agent_session"),
        herdrSessionName: "default",
        paneId: "wA:p2",
      }),
    );
  });

  test("resolves an official agy conversation id to its conversation database", async () => {
    const homeDir = await tempHome("herdsman-agy-id-");
    const conversations = await agyConversations(homeDir);
    const dbPath = join(conversations, `${AGY_ID}.db`);
    await writeFile(dbPath, "");

    await expect(agyLookup(homeDir, AGY_ID)).resolves.toEqual({
      kind: "agent_session",
      path: dbPath,
      source: "antigravity-sqlite",
      value: AGY_ID,
    });
  });

  test("reports and refuses an agy conversation id with no database", async () => {
    const homeDir = await tempHome("herdsman-agy-miss-");
    await agyConversations(homeDir);
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    await expect(agyLookup(homeDir, AGY_ID, { paneId: "wA:p3" })).resolves.toBeNull();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("no usable official Herdr agent session"),
      expect.objectContaining({
        detail: expect.stringContaining("is not a readable session file"),
        paneId: "wA:p3",
      }),
    );
  });

  test("never joins a non-uuid agy id into the conversations path", async () => {
    const homeDir = await tempHome("herdsman-agy-nonuuid-");
    // `conversations/../escape.db` normalises to this file, so only the uuid
    // guard keeps the id from naming a conversation outside the store.
    const root = join(homeDir, ".gemini", "antigravity-cli");
    await mkdir(join(root, "conversations"), { recursive: true });
    await writeFile(join(root, "escape.db"), "");
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    await expect(agyLookup(homeDir, "../escape", { paneId: "wA:p7" })).resolves.toBeNull();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("no usable official Herdr agent session"),
      expect.objectContaining({
        detail: expect.stringContaining("agy conversation id is not a uuid"),
      }),
    );
  });

  test("refuses a symlinked or group-writable agy conversation database", async () => {
    const homeDir = await tempHome("herdsman-agy-gate-");
    const conversations = await agyConversations(homeDir);
    // One id per attempt: a repeated *failed* lookup for the same official
    // values is deliberately suppressed by the failure memory.
    const gateId = "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f";
    const okId = "1f1f1f1f-1f1f-4f1f-8f1f-1f1f1f1f1f1f";
    const linkId = "2f2f2f2f-2f2f-4f2f-8f2f-2f2f2f2f2f2f";
    const gateDb = join(conversations, `${gateId}.db`);
    await writeFile(gateDb, "");
    await writeFile(join(conversations, `${okId}.db`), "");
    const linkDb = join(conversations, `${linkId}.db`);
    await writeFile(linkDb, "");
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    // Group-writable: another principal could rewrite the conversation store.
    chmodSync(gateDb, 0o664);
    await expect(agyLookup(homeDir, gateId, { paneId: "wA:p12" })).resolves.toBeNull();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("no usable official Herdr agent session"),
      expect.objectContaining({
        detail: expect.stringContaining("not a readable session file"),
      }),
    );

    chmodSync(join(conversations, `${okId}.db`), 0o644);
    await expect(agyLookup(homeDir, okId)).resolves.toMatchObject({
      source: "antigravity-sqlite",
    });

    // Symlinked database: the gate must see the link, not its target.
    const elsewhere = join(homeDir, "elsewhere.db");
    await writeFile(elsewhere, "");
    await rm(linkDb);
    symlinkSync(elsewhere, linkDb);
    await expect(agyLookup(homeDir, linkId)).resolves.toBeNull();
  });

  test("reports and refuses an official id for any other agent", async () => {
    // Only pi (official path) and agy (official id) are supported agents. Every
    // other agent is refused with the same fail-closed warning, whether or not
    // Herdr has an integration for it — this is a deliberate, documented limit.
    const homeDir = await tempHome("herdsman-unsupported-agent-");
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    for (const agent of ["claude", "codex", "opencode", "grok", "gemini", "omp"]) {
      await expect(
        discoverAgentHistory({
          agent,
          agentSession: {
            agent,
            kind: "id",
            source: `herdr:${agent}`,
            value: "ses_1",
          },
          homeDir,
          herdrSessionName: "default",
          paneId: "wA:p4",
        }),
      ).resolves.toBeNull();
    }
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("no usable official Herdr agent session"),
      expect.objectContaining({
        detail: expect.stringContaining("is not a supported herdsman history agent"),
        herdrSessionName: "default",
        paneId: "wA:p4",
      }),
    );
  });

  test("reports and refuses an official path that is not a readable session file", async () => {
    const homeDir = await tempHome("herdsman-missing-path-");
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: {
          agent: "pi",
          kind: "path",
          source: "herdr:pi",
          value: join(homeDir, "gone.jsonl"),
        },
        homeDir,
        paneId: "wA:p5",
      }),
    ).resolves.toBeNull();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("no usable official Herdr agent session"),
      expect.objectContaining({
        detail: expect.stringContaining("not a readable session file"),
        paneId: "wA:p5",
      }),
    );
  });

  test("maps session refs for the supported agents and nothing else", () => {
    const source = (agent: string, kind: "id" | "path" = "path") =>
      historySourceFromSessionRef({
        agent,
        kind,
        source: `herdr:${agent}`,
        value: "/tmp/x.jsonl",
      });
    expect(source("pi")).toBe("pi-jsonl");
    // pig (PiG) emits the same pi-jsonl session format and herdsman protocol, so it reads
    // through the same reader.
    expect(source("pig")).toBe("pi-jsonl");
    expect(
      historySourceFromSessionRef({
        agent: "pig",
        kind: "path",
        source: "herdr:pig",
        value: "/root/.pig/agent/sessions/ses-1.jsonl",
      }),
    ).toBe("pi-jsonl");
    expect(
      historySourceFromSessionRef({
        agent: "antigravity_cli",
        kind: "id",
        source: "herdr:antigravity_cli",
        value: AGY_ID,
      }),
    ).toBe("antigravity-sqlite");
    // Everything else is `unknown`, which no reader accepts.
    for (const agent of ["claude", "codex", "gemini", "grok", "omp", "opencode"]) {
      expect(source(agent)).toBe("unknown");
    }
  });

  test("matches whole source segments, so a substring never picks the wrong reader", () => {
    const source = (agent: string) =>
      historySourceFromSessionRef({
        agent,
        kind: "path",
        source: `herdr:${agent}`,
        value: "/tmp/x.jsonl",
      });
    // `herdr:copilot` contains "pi" but is not a pi session: a substring match
    // would hand a copilot path to the pi reader.
    expect(source("copilot")).toBe("unknown");
    expect(source("pi")).toBe("pi-jsonl");
    // `herdr:omp` is a pi-family agent with no reader, so it must map to
    // `unknown` rather than being silently read as pi (documented gap).
    expect(source("omp")).toBe("unknown");
    expect(
      historySourceFromSessionRef({
        agent: "antigravity",
        kind: "id",
        source: "herdr:antigravity",
        value: AGY_ID,
      }),
    ).toBe("antigravity-sqlite");
  });

  test("rejects a session file that is itself a symlink, but not one behind a linked directory", async () => {
    const homeDir = await tempHome("herdsman-symlink-gate-");
    const sessions = join(homeDir, ".pi", "agent", "sessions");
    await mkdir(sessions, { recursive: true });
    const real = join(sessions, "real.jsonl");
    await writeFile(real, "{}\n");
    const link = join(sessions, "link.jsonl");
    symlinkSync(real, link);

    // The check has to happen before realpathSync, otherwise the resolved target
    // is inspected and the symlink is invisible.
    expect(safeOfficialSessionPath(link)).toBeNull();
    expect(safeAllowedSessionPath(link, homeDir)).toBeNull();
    expect(safeOfficialSessionPath(real)).toBe(real);
    expect(safeAllowedSessionPath(real, homeDir)).toBe(real);

    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: { agent: "pi", kind: "path", source: "herdr:pi", value: link },
        homeDir,
        paneId: "wA:p11",
      }),
    ).resolves.toBeNull();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("no usable official Herdr agent session"),
      expect.objectContaining({ detail: expect.stringContaining("not a readable session file") }),
    );

    // Only the file itself is checked: an official path reached through a
    // symlinked *parent* directory is a normal file and stays readable.
    const linkedRoot = join(homeDir, "linked-root");
    symlinkSync(sessions, linkedRoot);
    const viaLinkedDir = join(linkedRoot, "real.jsonl");
    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: { agent: "pi", kind: "path", source: "herdr:pi", value: viaLinkedDir },
        homeDir,
      }),
    ).resolves.toMatchObject({ path: real });
  });

  test("does not repeat a failed lookup, nor its warning, until the official values change", async () => {
    const homeDir = await tempHome("herdsman-negative-cache-");
    const conversations = await agyConversations(homeDir);
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const lookup = (value: string) => agyLookup(homeDir, value, { paneId: "wA:p13" });

    await expect(lookup(AGY_ID)).resolves.toBeNull();
    expect(warn).toHaveBeenCalledTimes(1);

    // The conversation database appears, but the same official values are still
    // inside their retry window: the pane is not re-resolved and does not warn
    // again.
    const dbPath = join(conversations, `${AGY_ID}.db`);
    await writeFile(dbPath, "");
    await expect(lookup(AGY_ID)).resolves.toBeNull();
    expect(warn).toHaveBeenCalledTimes(1);

    // A different official value is a different key and is attempted at once.
    const otherId = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff";
    await writeFile(join(conversations, `${otherId}.db`), "");
    await expect(lookup(otherId)).resolves.toMatchObject({
      path: join(conversations, `${otherId}.db`),
    });
    expect(warn).toHaveBeenCalledTimes(1);
  });

  test("resolves agy conversations across primary HOME and AGY_EXTRA_HOMES and tolerates missing extra homes", async () => {
    const ultraHome = await tempHome("herdsman-agy-ultra-");
    const proHome = await tempHome("herdsman-agy-pro-");
    const missingHome = join(ultraHome, "nonexistent-extra-home");
    const ultraConversations = await agyConversations(ultraHome);
    const proConversations = await agyConversations(proHome);
    const ultraId = "11111111-2222-4333-8444-555555555555";
    const proId = "66666666-7777-4888-8999-aaaaaaaaaaaa";
    const ultraDb = join(ultraConversations, `${ultraId}.db`);
    const proDb = join(proConversations, `${proId}.db`);
    await writeFile(ultraDb, "");
    await writeFile(proDb, "");

    vi.stubEnv("AGY_EXTRA_HOMES", `${missingHome}:${proHome}`);
    await expect(agyLookup(ultraHome, ultraId)).resolves.toEqual({
      kind: "agent_session",
      path: ultraDb,
      source: "antigravity-sqlite",
      value: ultraId,
    });
    await expect(agyLookup(ultraHome, proId)).resolves.toEqual({
      kind: "agent_session",
      path: proDb,
      source: "antigravity-sqlite",
      value: proId,
    });
  });

  test("deduplicates symlinked agy conversation stores by realpath and keeps Ultra priority", async () => {
    const ultraHome = await tempHome("herdsman-agy-dedup-ultra-");
    const proHome = await tempHome("herdsman-agy-dedup-pro-");
    const ultraConversations = await agyConversations(ultraHome);
    const proGemini = join(proHome, ".gemini", "antigravity-cli");
    await mkdir(proGemini, { recursive: true });
    symlinkSync(ultraConversations, join(proGemini, "conversations"));

    const sharedId = "cccccccc-dddd-4eee-8fff-000000000001";
    const ultraDb = join(ultraConversations, `${sharedId}.db`);
    await writeFile(ultraDb, "");

    const candidates = resolveAgySessionCandidates({
      extraHomeDirs: [proHome],
      homeDir: ultraHome,
      id: sharedId,
    });
    expect(candidates.refs).toEqual([
      {
        kind: "agent_session",
        path: ultraDb,
        source: "antigravity-sqlite",
        value: sharedId,
      },
    ]);
    await expect(agyLookup(ultraHome, sharedId, { extraHomeDirs: [proHome] })).resolves.toEqual({
      kind: "agent_session",
      path: ultraDb,
      source: "antigravity-sqlite",
      value: sharedId,
    });

    // Independent directories with the same conversation id still prefer primary (Ultra) HOME.
    const proIndependentHome = await tempHome("herdsman-agy-prio-pro-");
    const proIndependentConversations = await agyConversations(proIndependentHome);
    await writeFile(join(proIndependentConversations, `${sharedId}.db`), "");
    await expect(
      agyLookup(ultraHome, sharedId, { extraHomeDirs: [proIndependentHome] }),
    ).resolves.toMatchObject({ path: ultraDb });
  });

  test("enforces file-safety gates on extra agy homes", async () => {
    const ultraHome = await tempHome("herdsman-agy-extra-gate-ultra-");
    const proHome = await tempHome("herdsman-agy-extra-gate-pro-");
    await agyConversations(ultraHome);
    const proConversations = await agyConversations(proHome);
    const writableId = "cccccccc-dddd-4eee-8fff-000000000002";
    const symlinkId = "cccccccc-dddd-4eee-8fff-000000000003";
    const writableDb = join(proConversations, `${writableId}.db`);
    await writeFile(writableDb, "");
    chmodSync(writableDb, 0o664);

    const targetDb = join(proHome, "outside.db");
    await writeFile(targetDb, "");
    symlinkSync(targetDb, join(proConversations, `${symlinkId}.db`));

    vi.spyOn(console, "warn").mockImplementation(() => {});
    await expect(
      agyLookup(ultraHome, writableId, { extraHomeDirs: [proHome] }),
    ).resolves.toBeNull();
    await expect(agyLookup(ultraHome, symlinkId, { extraHomeDirs: [proHome] })).resolves.toBeNull();
  });

  test("invalidates negative cache when AGY_EXTRA_HOMES or extraHomeDirs changes within TTL", async () => {
    const ultraHome = await tempHome("herdsman-agy-negcache-ultra-");
    const proHome = await tempHome("herdsman-agy-negcache-pro-");
    await agyConversations(ultraHome);
    const proConversations = await agyConversations(proHome);
    const negCacheId = "cccccccc-dddd-4eee-8fff-000000000004";
    const proDb = join(proConversations, `${negCacheId}.db`);
    await writeFile(proDb, "");
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    // Phase 1: with no extra homes configured, lookup misses and enters the negative cache.
    vi.stubEnv("AGY_EXTRA_HOMES", "");
    await expect(agyLookup(ultraHome, negCacheId, { paneId: "wA:p14" })).resolves.toBeNull();
    expect(warn).toHaveBeenCalledTimes(1);
    await expect(agyLookup(ultraHome, negCacheId, { paneId: "wA:p14" })).resolves.toBeNull();
    expect(warn).toHaveBeenCalledTimes(1);

    // Phase 2: configuring extra homes (via extraHomeDirs or AGY_EXTRA_HOMES) within TTL
    // without force immediately resolves because extra homes are part of discoveryLookupKey.
    await expect(
      agyLookup(ultraHome, negCacheId, { extraHomeDirs: [proHome], paneId: "wA:p14" }),
    ).resolves.toEqual({
      kind: "agent_session",
      path: proDb,
      source: "antigravity-sqlite",
      value: negCacheId,
    });
    vi.stubEnv("AGY_EXTRA_HOMES", proHome);
    await expect(agyLookup(ultraHome, negCacheId, { paneId: "wA:p14" })).resolves.toEqual({
      kind: "agent_session",
      path: proDb,
      source: "antigravity-sqlite",
      value: negCacheId,
    });
  });
});
