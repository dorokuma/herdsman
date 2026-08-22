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
