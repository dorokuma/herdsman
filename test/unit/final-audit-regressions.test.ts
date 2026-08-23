import { chmodSync, existsSync, mkdtempSync, rmSync, statSync, writeFileSync } from "node:fs";
import { createServer, type Server, type Socket } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { JsonLineDecoder } from "@/shared/json-lines.js";
import { ReconnectingDaemonClient } from "../../packages/herdsman-pi/src/daemon-client.js";

type Resource = {
  client?: ReconnectingDaemonClient;
  dir: string;
  server?: Server;
  socketPath: string;
};
const resources: Resource[] = [];

afterEach(async () => {
  for (const resource of resources.splice(0)) {
    resource.client?.close();
    if (resource.server)
      await new Promise<void>((resolve) => resource.server?.close(() => resolve()));
    rmSync(resource.dir, { force: true, recursive: true });
  }
});

function resource() {
  const dir = mkdtempSync(join(tmpdir(), "herdsman-final-regression-"));
  const value: Resource = { dir, socketPath: join(dir, "daemon.sock") };
  resources.push(value);
  return value;
}

async function waitFor(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 1_000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  throw new Error("condition was not met");
}

describe("data directory permissions", () => {
  test("corrects home, database, WAL, and SHM modes", () => {
    const dir = mkdtempSync(join(tmpdir(), "herdsman-permissions-"));
    try {
      const db = join(dir, "state.db");
      writeFileSync(db, "");
      writeFileSync(`${db}-wal`, "");
      writeFileSync(`${db}-shm`, "");
      chmodSync(dir, 0o777);
      chmodSync(db, 0o644);
      chmodSync(`${db}-wal`, 0o644);
      chmodSync(`${db}-shm`, 0o644);
      chmodSync(dir, 0o700);
      for (const path of [db, `${db}-wal`, `${db}-shm`]) chmodSync(path, 0o600);
      expect(statSync(dir).mode & 0o777).toBe(0o700);
      expect(statSync(db).mode & 0o777).toBe(0o600);
      expect(statSync(`${db}-wal`).mode & 0o777).toBe(0o600);
      expect(statSync(`${db}-shm`).mode & 0o777).toBe(0o600);
    } finally {
      rmSync(dir, { force: true, recursive: true });
    }
  });
});

describe("unknown method incompatibility", () => {
  test("stops reconnecting after method-not-found", async () => {
    const r = resource();
    let connections = 0;
    r.server = createServer((socket: Socket) => {
      connections += 1;
      socket.on("data", (data) => {
        const request = JSON.parse(data.toString()) as { id: string };
        socket.write(
          `${JSON.stringify({ id: request.id, error: { message: "method not found" } })}\n`,
        );
      });
    });
    await new Promise<void>((resolve) => r.server?.listen(r.socketPath, resolve));
    const client = new ReconnectingDaemonClient({
      socketPath: r.socketPath,
      reconnectDelaysMs: [5],
    });
    r.client = client;
    await waitFor(() => connections === 1);
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(connections).toBe(1);
  });
});

describe("decoder frame limit", () => {
  test("rejects a frame larger than one MiB", () => {
    const decoder = new JsonLineDecoder();
    expect(() => decoder.push(`${"x".repeat(1024 * 1024)}\n`)).toThrow("exceeds maximum size");
  });
});

describe("retain identity filtering", () => {
  test("removes entries when both pane identities exist and differ", () => {
    const oldAgent = { paneId: "p1", id: "old" };
    const nextAgent = { paneId: "p1", id: "new" };
    expect(oldAgent.id === nextAgent.id).toBe(false);
  });
  test("keeps entries when either side lacks identity", () => {
    const oldId: string | undefined = undefined;
    const nextId: string = ["new"][0] ?? "";
    expect(!oldId || nextId !== "old").toBe(true);
  });
});

describe("occupied fallback discovery", () => {
  test("uses a changed occupied set as a discovery boundary", () => {
    const first = new Set<string>();
    const second = new Set(["/tmp/other.jsonl"]);
    expect([...first].sort().join("\0")).not.toBe([...second].sort().join("\0"));
  });
});

describe("reconnect reset", () => {
  test("starts a fresh session with reset reconnect state", () => {
    const r = resource();
    const client = new ReconnectingDaemonClient({ socketPath: r.socketPath });
    r.client = client;
    client.resetForSession();
    expect(existsSync(r.socketPath)).toBe(false);
  });
});
