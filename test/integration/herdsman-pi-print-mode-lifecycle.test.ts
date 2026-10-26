import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, unlinkSync } from "node:fs";
import { createServer, type Server, type Socket } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, test } from "vitest";
import { JsonLineDecoder } from "@/shared/json-lines.js";

const repoRoot = fileURLToPath(new URL("../..", import.meta.url));
const extensionPath = join(repoRoot, "packages/herdsman-pi/src/index.ts");
const fixturePath = fileURLToPath(
  new URL("../fixtures/herdsman-pi-print-mode-lifecycle.ts", import.meta.url),
);
const tsxCli = join(repoRoot, "node_modules/tsx/dist/cli.mjs");

/**
 * Wall clock a finished lifecycle is allowed before the child is considered
 * stuck. A healthy run drains in well under a second; the pre-fix hang never
 * ends, so this is what makes the failure visible instead of hanging the suite.
 */
const CHILD_TIMEOUT_MS = 10_000;

const tempDirs: string[] = [];
const servers: Server[] = [];

afterEach(async () => {
  for (const server of servers.splice(0)) {
    // Defensive: a connection the fixture somehow left behind must not keep the
    // server (and with it the suite) from closing.
    (server as Server & { closeAllConnections?: () => void }).closeAllConnections?.();
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
  for (const dir of tempDirs.splice(0)) rmSync(dir, { force: true, recursive: true });
});

// X3 fix ③ (with ① and ②): a finished print-mode run must reach an empty event
// loop on its own. Pre-fix, the daemon client was left referenced and the
// superseded socket was never closed, so the child hung until it was killed and
// the daemon socket kept an orphan connection.
describe("herdsman-pi print-mode process lifecycle", () => {
  test("a print-mode run exits on its own and opens no daemon connection", async () => {
    const home = createHome();
    const daemon = await startDaemon(home);
    const result = await runFixture("print", home);

    expect(result.timedOut).toBe(false);
    expect(result.exitCode).toBe(0);
    expect(result.signal).toBeNull();
    expect(result.stderr).toContain("herdsman-pi-print-lifecycle:done:print");
    // A non-interactive run must not connect at all, so the daemon socket saw
    // no traffic and keeps nothing behind.
    expect(daemon.registerCalls).toBe(0);
    expect(daemon.liveConnections).toEqual([]);
  }, 30_000);

  test("a connecting run exits on its own and leaves no connection behind", async () => {
    const home = createHome();
    const daemon = await startDaemon(home);
    const result = await runFixture("connecting", home);

    expect(result.timedOut).toBe(false);
    expect(result.exitCode).toBe(0);
    expect(result.signal).toBeNull();
    expect(result.stderr).toContain("herdsman-pi-print-lifecycle:done:connecting");
    // Presence was registered exactly once, and the shutdown closed the only
    // connection this session ever opened.
    expect(daemon.registerCalls).toBe(1);
    expect(daemon.liveConnections).toEqual([]);
  }, 30_000);
});

type Daemon = {
  liveConnections: Socket[];
  registerCalls: number;
};

function createHome(): string {
  const home = mkdtempSync(join(tmpdir(), "herdsman-pi-print-home-"));
  tempDirs.push(home);
  return home;
}

async function startDaemon(home: string): Promise<Daemon> {
  const socketPath = join(home, "herdsman.sock");
  const liveConnections: Socket[] = [];
  const daemon: Daemon = { liveConnections, registerCalls: 0 };
  if (existsSync(socketPath)) unlinkSync(socketPath);
  const server = createServer((socket) => {
    liveConnections.push(socket);
    socket.on("close", () => {
      const index = liveConnections.indexOf(socket);
      if (index >= 0) liveConnections.splice(index, 1);
    });
    const decoder = new JsonLineDecoder();
    socket.on("data", (chunk) => {
      for (const message of decoder.push(chunk.toString("utf8"))) {
        const record = message as { id?: number | string; method?: string };
        if (record.method === "agent.orchestrator.register") daemon.registerCalls += 1;
        socket.write(`${JSON.stringify({ id: record.id, result: registerResponse() })}\n`);
      }
    });
  });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, resolve);
  });
  servers.push(server);
  return daemon;
}

function registerResponse() {
  return {
    presence: {
      connectedAt: 1,
      herdrSessionName: "default",
      paneId: "wB:p1",
      subscriberId: "pi-print-lifecycle",
      terminalId: "term_pi",
      workspaceId: "wB",
    },
    state: {
      ackedEventId: 0,
      herdrSessionName: "default",
      owner: { paneId: "wB:p1", terminalId: "term_pi" },
      updatedAt: "2026-07-16T00:00:00.000Z",
      workspaceId: "wB",
    },
  };
}

type FixtureResult = {
  exitCode: number | null;
  signal: NodeJS.Signals | null;
  stderr: string;
  stdout: string;
  timedOut: boolean;
};

async function runFixture(scenario: "connecting" | "print", home: string): Promise<FixtureResult> {
  // `detached` puts the child in its own process group so a timeout can take
  // down the whole tree: the tsx CLI re-executes the fixture in a grandchild,
  // and killing only the direct child would leave that grandchild holding the
  // daemon socket open.
  const child = spawn(
    process.execPath,
    [tsxCli, fixturePath, join(home, "herdsman.sock"), extensionPath, scenario],
    {
      detached: true,
      env: {
        ...process.env,
        HERDR_ENV: "1",
        HERDR_PANE_ID: "wB:p1",
        HERDR_SOCKET_PATH: "/tmp/herdr.sock",
        HERDR_WORKSPACE_ID: "wB",
        HERDSMAN_HOME: home,
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk: Buffer) => {
    stdout += chunk.toString("utf8");
  });
  child.stderr.on("data", (chunk: Buffer) => {
    stderr += chunk.toString("utf8");
  });
  const exited = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve) => {
    child.on("exit", (code, signal) => resolve({ code, signal }));
  });
  const killTree = () => {
    if (child.pid === undefined) {
      child.kill("SIGKILL");
      return;
    }
    try {
      process.kill(-child.pid, "SIGKILL");
    } catch {
      child.kill("SIGKILL");
    }
  };
  const timer = setTimeout(killTree, CHILD_TIMEOUT_MS);
  const { code, signal } = await exited;
  clearTimeout(timer);
  return { exitCode: code, signal, stderr, stdout, timedOut: signal === "SIGKILL" };
}
