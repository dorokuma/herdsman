import { mkdir, mkdtemp, rm, utimes, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { discoverAgentHistory } from "@/agent-history/discovery.js";

const roleDirs: string[] = [];

afterEach(async () => {
  await Promise.all(roleDirs.splice(0).map((dir) => rm(dir, { force: true, recursive: true })));
});

async function roleRoot() {
  await mkdir("/tmp/pi-role-sessions", { recursive: true });
  const root = await mkdtemp(join("/tmp/pi-role-sessions", "herdsman-history-regression-"));
  roleDirs.push(root);
  return root;
}

async function session(root: string, name: string, cwd: string) {
  const path = join(root, name, "session.jsonl");
  await mkdir(join(root, name), { recursive: true });
  await writeFile(path, `${JSON.stringify({ cwd })}\n`);
  return path;
}

describe("agent history discovery regressions (independent coverage)", () => {
  test("role 会话会被 pi fallback 发现", async () => {
    const root = await roleRoot();
    const path = await session(root, "role-x", "/repo");

    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: null,
        cwd: "/repo",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
      }),
    ).resolves.toMatchObject({ kind: "discovered_file", path, source: "pi-jsonl", value: path });
  });

  test("cwd 不匹配的较新候选不会被 pi fallback 采纳", async () => {
    const root = await roleRoot();
    const path = await session(root, "role-x", "/main-agent-repo");
    const old = Date.now() - 1_000;
    await utimes(path, new Date(old), new Date(old));

    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: null,
        cwd: "/repo",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
      }),
    ).resolves.toBeNull();
  });

  test("occupiedSessionPaths 命中的候选会被跳过且唯一占用候选返回 null", async () => {
    const root = await roleRoot();
    const older = await session(root, "role-old", "/repo");
    const newer = await session(root, "role-new", "/repo");
    const newerTime = Date.now();
    await utimes(older, new Date(newerTime - 2_000), new Date(newerTime - 2_000));
    await utimes(newer, new Date(newerTime), new Date(newerTime));

    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: null,
        cwd: "/repo",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
        occupiedSessionPaths: new Set([newer]),
      }),
    ).resolves.toMatchObject({ path: older, value: older });

    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: null,
        cwd: "/repo",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
        occupiedSessionPaths: new Set([older, newer]),
      }),
    ).resolves.toBeNull();
  });
});

describe("agent history discovery bounds (independent coverage)", () => {
  test("does not scan jsonl beyond maxDepth=4", async () => {
    const root = await roleRoot();
    let current = root;
    for (let depth = 1; depth <= 5; depth += 1) {
      current = join(current, `depth-${depth}`);
      await mkdir(current, { recursive: true });
    }
    await writeFile(join(current, "too-deep.jsonl"), `${JSON.stringify({ cwd: "/repo" })}\n`);
    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: null,
        cwd: "/repo",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
      }),
    ).resolves.toBeNull();
  });

  test("Pi id ref resolves a filename match before mtime discovery", async () => {
    const root = await roleRoot();
    const id = "ses-target-123";
    const matched = join(root, `${id}-new.jsonl`);
    const competing = join(root, "unrelated.jsonl");
    await writeFile(matched, `${JSON.stringify({ cwd: "/wrong" })}\n`);
    await writeFile(competing, `${JSON.stringify({ cwd: "/repo" })}\n`);
    const now = Date.now();
    await utimes(matched, new Date(now - 2_000), new Date(now - 2_000));
    await utimes(competing, new Date(now), new Date(now));

    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: { agent: "pi", kind: "id", source: "herdr:pi", value: id },
        cwd: "/repo",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
      }),
    ).resolves.toMatchObject({ kind: "agent_session", path: matched, value: id });
  });

  test("Pi id ref falls back to recursive discovery when no filename contains the id", async () => {
    const root = await roleRoot();
    const fallback = await session(root, "nested-session", "/repo");
    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: { agent: "pi", kind: "id", source: "herdr:pi", value: "missing-id" },
        cwd: "/repo",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
      }),
    ).resolves.toMatchObject({ kind: "discovered_file", path: fallback, value: fallback });
  });

  test("normalizes trailing and repeated slashes when matching candidate cwd", async () => {
    const root = await roleRoot();
    const path = await session(root, "normalized-cwd", "//repo///");
    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: null,
        cwd: "/repo/",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
      }),
    ).resolves.toMatchObject({ path, value: path });
  });

  test("stops discovery at maxFiles=2000 while returning candidates before the bound", async () => {
    const root = await roleRoot();
    const dir = join(root, "many");
    await mkdir(dir, { recursive: true });
    for (let index = 0; index < 2_000; index += 1) {
      await writeFile(
        join(dir, `session-${String(index).padStart(4, "0")}.jsonl`),
        `${JSON.stringify({ cwd: "/repo" })}\n`,
      );
    }
    await writeFile(
      join(root, "many", "session-2000.jsonl"),
      `${JSON.stringify({ cwd: "/repo" })}\n`,
    );
    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: null,
        cwd: "/repo",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
      }),
    ).resolves.toMatchObject({ path: expect.stringContaining("session-19") });
    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: null,
        cwd: "/repo",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
      }),
    ).resolves.not.toMatchObject({ path: join(dir, "session-2000.jsonl") });
  }, 30_000);

  test("reads only the bounded prefix of an oversized jsonl and finds cwd", async () => {
    const root = await roleRoot();
    const path = join(root, "large", "session.jsonl");
    await mkdir(join(root, "large"), { recursive: true });
    await writeFile(path, `${JSON.stringify({ cwd: "/repo" })}\n${"x".repeat(300 * 1024)}\n`);
    const started = performance.now();
    await expect(
      discoverAgentHistory({
        agent: "pi",
        agentSession: null,
        cwd: "/repo",
        foregroundCwd: null,
        homeDir: "/nonexistent-herdsman-home",
      }),
    ).resolves.toMatchObject({ path });
    expect(performance.now() - started).toBeLessThan(2_000);
  }, 30_000);
});
