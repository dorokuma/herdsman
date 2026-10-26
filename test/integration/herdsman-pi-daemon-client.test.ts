import { existsSync, mkdtempSync, rmSync, unlinkSync } from "node:fs";
import { createServer, type Server, Socket } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import { JsonLineDecoder } from "@/shared/json-lines.js";
import {
  type DaemonStreamMessage,
  ReconnectingDaemonClient,
} from "../../packages/herdsman-pi/src/daemon-client.js";

const resources: Array<{ client?: ReconnectingDaemonClient; dir: string; server?: Server }> = [];

afterEach(async () => {
  for (const resource of resources.splice(0)) {
    resource.client?.close();
    await closeServer(resource.server);
    rmSync(resource.dir, { force: true, recursive: true });
  }
});

describe("ReconnectingDaemonClient", () => {
  test("connects after the server appears and handles responses and streams", async () => {
    const resource = createResource();
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [5],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    let connected = 0;
    const streams: DaemonStreamMessage[] = [];
    client.onConnected = () => {
      connected += 1;
    };
    client.onStreamMessage = (message) => streams.push(message);

    const sockets: Socket[] = [];
    resource.server = await startServer(resource.socketPath, (socket, message) => {
      sockets.push(socket);
      socket.write(`${JSON.stringify({ id: message.id, result: { method: message.method } })}\n`);
      socket.write(
        `${JSON.stringify({ method: "agent.event", params: { event: { id: 1, payload: {}, type: "agent.done" } } })}\n`,
      );
    });
    await waitFor(() => connected === 1);

    await expect(client.request("agent.list", {})).resolves.toEqual({ method: "agent.list" });
    await waitFor(() => streams.length === 1);
    expect(streams[0]).toMatchObject({ method: "agent.event", params: { event: { id: 1 } } });
    expect(sockets.length).toBeGreaterThan(0);
  });

  test("decodes agent context change streams", async () => {
    const resource = createResource();
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [5],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    const streams: DaemonStreamMessage[] = [];
    let connections = 0;
    client.onStreamMessage = (message) => streams.push(message);
    resource.server = await startServer(
      resource.socketPath,
      (socket, message) => {
        socket.write(`${JSON.stringify({ id: message.id, result: {} })}\n`);
        socket.write(
          `${JSON.stringify({
            method: "agent.context.changed",
            params: {
              context: {
                agents: [],
                herdrSessionName: "default",
                updatedAt: "2026-07-16T00:00:00.000Z",
                workspaceId: "wB",
              },
              herdrSessionName: "default",
              workspaceId: "wB",
            },
          })}\n`,
        );
      },
      () => {
        connections += 1;
      },
    );

    await waitFor(() => connections === 1);
    await expect(client.request("agent.list", {})).resolves.toEqual({});
    await waitFor(() => streams.length === 1);
    expect(streams[0]).toMatchObject({
      method: "agent.context.changed",
      params: { context: { workspaceId: "wB" } },
    });
  });

  test("rejects in-flight work, reconnects once, and accepts later requests", async () => {
    const resource = createResource();
    let connectionCount = 0;
    resource.server = await startServer(
      resource.socketPath,
      (socket, message) => {
        if (message.method === "hold") {
          socket.destroy();
          return;
        }
        socket.write(`${JSON.stringify({ id: message.id, result: { ok: true } })}\n`);
      },
      () => {
        connectionCount += 1;
      },
    );
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [5],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    let disconnected = 0;
    client.onDisconnected = () => {
      disconnected += 1;
    };
    await waitFor(() => connectionCount === 1);

    await expect(client.request("hold", {})).rejects.toThrow("socket closed");
    await waitFor(() => connectionCount === 2);
    expect(disconnected).toBe(1);
    await expect(client.request("later", {})).resolves.toEqual({ ok: true });
  });

  test("malformed JSON disconnects without crashing and then reconnects", async () => {
    const resource = createResource();
    let connectionCount = 0;
    resource.server = await startServer(
      resource.socketPath,
      (socket, message) => {
        socket.write(`${JSON.stringify({ id: message.id, result: { ok: true } })}\n`);
      },
      (socket) => {
        connectionCount += 1;
        if (connectionCount === 1) socket.write("{not-json}\n");
      },
    );
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [5],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    await waitFor(() => connectionCount === 2);

    await expect(client.request("healthy", {})).resolves.toEqual({ ok: true });
  });

  test("explicit close stops reconnect and rejects later requests", async () => {
    const resource = createResource();
    resource.server = await startServer(resource.socketPath, (socket, message) => {
      socket.write(`${JSON.stringify({ id: message.id, result: {} })}\n`);
    });
    const client = new ReconnectingDaemonClient({ socketPath: resource.socketPath });
    resource.client = client;
    await new Promise((resolve) => setTimeout(resolve, 10));

    client.close();
    await expect(client.request("agent.list", {})).rejects.toThrow(
      "Herdsman daemon client is closed",
    );
  });

  test("treats agent.ping as optional so an old daemon keeps the connection", async () => {
    const resource = createResource();
    let connectionCount = 0;
    resource.server = await startServer(
      resource.socketPath,
      (socket, message) => {
        if (message.method === "agent.ping") {
          socket.write(
            `${JSON.stringify({
              error: { message: `Unknown method: ${message.method}` },
              id: message.id,
            })}\n`,
          );
          return;
        }
        socket.write(`${JSON.stringify({ id: message.id, result: { ok: true } })}\n`);
      },
      () => {
        connectionCount += 1;
      },
    );
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [5],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    let disconnected = 0;
    client.onDisconnected = () => {
      disconnected += 1;
    };
    await waitFor(() => connectionCount === 1);

    await expect(client.request("agent.ping", {})).rejects.toThrow("Unknown method: agent.ping");
    await expect(client.request("agent.list", {})).resolves.toEqual({ ok: true });
    expect(disconnected).toBe(0);
    expect(connectionCount).toBe(1);
  });

  test("treats agent.turn.completed as optional so an old daemon keeps the connection", async () => {
    const resource = createResource();
    let connectionCount = 0;
    resource.server = await startServer(
      resource.socketPath,
      (socket, message) => {
        if (message.method === "agent.turn.completed") {
          socket.write(
            `${JSON.stringify({
              error: { message: `Unknown method: ${message.method}` },
              id: message.id,
            })}\n`,
          );
          return;
        }
        socket.write(`${JSON.stringify({ id: message.id, result: { ok: true } })}\n`);
      },
      () => {
        connectionCount += 1;
      },
    );
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [5],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    let disconnected = 0;
    client.onDisconnected = () => {
      disconnected += 1;
    };
    await waitFor(() => connectionCount === 1);

    await expect(client.request("agent.turn.completed", { confirmed: true })).rejects.toThrow(
      "Unknown method: agent.turn.completed",
    );
    // The connection must survive and keep serving the required methods.
    await expect(client.request("agent.list", {})).resolves.toEqual({ ok: true });
    expect(disconnected).toBe(0);
    expect(connectionCount).toBe(1);
  });

  test("still marks a daemon incompatible on an unknown required method", async () => {
    const resource = createResource();
    let connectionCount = 0;
    resource.server = await startServer(
      resource.socketPath,
      (socket, message) => {
        socket.write(
          `${JSON.stringify({
            error: { message: `Unknown method: ${message.method}` },
            id: message.id,
          })}\n`,
        );
      },
      () => {
        connectionCount += 1;
      },
    );
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [5],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    let disconnected = 0;
    client.onDisconnected = () => {
      disconnected += 1;
    };
    await waitFor(() => connectionCount === 1);

    const error = await client
      .request("agent.orchestrator.get", {})
      .catch((value: unknown) => value);
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toBe("Unknown method: agent.orchestrator.get");
    // The unknown required method marks the daemon incompatible: the client
    // reports the disconnect and stops reconnecting.
    expect(disconnected).toBe(1);
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(connectionCount).toBe(1);
    await expect(client.request("agent.list", {})).rejects.toThrow(
      "Herdsman daemon client is not connected",
    );
  });

  test("rejects a request the daemon never answers and ignores a late response", async () => {
    const resource = createResource();
    resource.server = await startServer(resource.socketPath, (socket, message) => {
      if (message.method === "hang") {
        // Deliberately never answer the hung request.
        return;
      }
      socket.write(`${JSON.stringify({ id: message.id, result: { ok: true } })}\n`);
    });
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [5],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    let connected = 0;
    client.onConnected = () => {
      connected += 1;
    };
    await waitFor(() => connected === 1);

    vi.useFakeTimers();
    try {
      const hung = client.request("hang", {});
      const rejection = expect(hung).rejects.toThrow("Herdsman daemon request timed out");
      await vi.advanceTimersByTimeAsync(15_000);
      await rejection;

      // The timed-out slot was removed, so the connection still serves later
      // requests normally (and any response that arrives for the old id is
      // ignored because it is no longer pending).
      const later = client.request("agent.list", {});
      const resolution = expect(later).resolves.toEqual({ ok: true });
      await vi.advanceTimersByTimeAsync(0);
      await resolution;
    } finally {
      vi.useRealTimers();
    }
  });

  // X3 fix ①: a session reset (or any new connection) must release what the
  // client already holds. The pre-fix client overwrote `#socket` without
  // closing the old one, so `new ReconnectingDaemonClient(...)` followed by a
  // synchronous `resetForSession()` left the first socket ESTABLISHED on
  // `herdsman.sock` with nobody holding a reference to close it.
  test("a synchronous resetForSession after construction leaves no orphan connection", async () => {
    const resource = createResource();
    const live = new Set<Socket>();
    const destroySpy = vi.spyOn(Socket.prototype, "destroy");
    resource.server = await startServer(
      resource.socketPath,
      (socket, message) => {
        socket.write(`${JSON.stringify({ id: message.id, result: {} })}\n`);
      },
      (socket) => {
        live.add(socket);
        socket.on("close", () => live.delete(socket));
      },
    );
    let connected = 0;
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [5],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    client.onConnected = () => {
      connected += 1;
    };
    // The race the extension hits: the constructor only queues its connect as a
    // microtask, so this runs before any socket exists on the client side.
    client.resetForSession();
    await waitFor(() => connected === 1);
    await waitFor(() => live.size === 1);
    expect(connected).toBe(1);
    expect(live.size).toBe(1);
    expect(destroySpy.mock.calls.length).toBeGreaterThan(0);

    client.close();
    await waitFor(() => live.size === 0);
    expect(live.size).toBe(0);
  });

  test("repeated resetForSession on a live client keeps exactly one connection", async () => {
    const resource = createResource();
    const live = new Set<Socket>();
    resource.server = await startServer(
      resource.socketPath,
      (socket, message) => {
        socket.write(`${JSON.stringify({ id: message.id, result: {} })}\n`);
      },
      (socket) => {
        live.add(socket);
        socket.on("close", () => live.delete(socket));
      },
    );
    let connected = 0;
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [5],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    client.onConnected = () => {
      connected += 1;
    };
    await waitFor(() => connected === 1);

    // Each reset runs against a live connection: the old socket is closed and
    // exactly one connection stays established.
    client.resetForSession();
    await waitFor(() => connected === 2);
    client.resetForSession();
    await waitFor(() => connected === 3);
    client.resetForSession();
    await waitFor(() => connected === 4);
    await waitFor(() => live.size === 1);
    expect(live.size).toBe(1);
    await expect(client.request("agent.list", {})).resolves.toEqual({});

    client.close();
    await waitFor(() => live.size === 0);
    expect(live.size).toBe(0);
  });

  test("a pending reconnect timer is dropped when a new connection starts", async () => {
    const resource = createResource();
    const live = new Set<Socket>();
    let accepted = 0;
    let firstServerSocket: Socket | undefined;
    resource.server = await startServer(
      resource.socketPath,
      (socket, message) => {
        socket.write(`${JSON.stringify({ id: message.id, result: {} })}\n`);
      },
      (socket) => {
        accepted += 1;
        live.add(socket);
        socket.on("close", () => live.delete(socket));
        firstServerSocket ??= socket;
      },
    );
    let connected = 0;
    let disconnected = 0;
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [20],
      socketPath: resource.socketPath,
    });
    resource.client = client;
    client.onConnected = () => {
      connected += 1;
    };
    client.onDisconnected = () => {
      disconnected += 1;
    };
    await waitFor(() => connected === 1);

    // Drop the live connection and wait for the client to report it: that is
    // the exact point at which the 20ms reconnect timer is armed.
    firstServerSocket?.destroy();
    await waitFor(() => disconnected === 1);
    client.resetForSession();
    await waitFor(() => connected === 2);
    await waitFor(() => live.size === 1);

    // The stale timer must not fire against the replacement connection.
    await new Promise((resolve) => setTimeout(resolve, 120));
    expect(live.size).toBe(1);
    expect(accepted).toBe(2);

    client.close();
    await waitFor(() => live.size === 0);
  });

  // X3 fix ③: the client socket and its reconnect timer must be unref'd, or a
  // print-mode process that never calls `process.exit()` hangs on them.
  test("the client socket and the reconnect timer are unref'd", async () => {
    const resource = createResource();
    const socketUnref = vi.spyOn(Socket.prototype, "unref");
    // A reconnect delay nothing else in this run uses, so the wrapped handle is
    // unambiguously the client's reconnect timer and not a test/scheduler one.
    const reconnectDelayMs = 60_000;
    const realSetTimeout = globalThis.setTimeout;
    let reconnectTimerUnref: ReturnType<typeof vi.fn> | undefined;
    vi.spyOn(globalThis, "setTimeout").mockImplementation(((
      ...args: Parameters<typeof setTimeout>
    ) => {
      const handle = realSetTimeout(...args);
      if (args[1] === reconnectDelayMs) {
        const unref = vi.fn();
        (handle as { unref?: () => void }).unref = unref;
        reconnectTimerUnref = unref;
      }
      return handle;
    }) as unknown as typeof setTimeout);
    resource.server = await startServer(
      resource.socketPath,
      (socket, message) => {
        socket.write(`${JSON.stringify({ id: message.id, result: {} })}\n`);
      },
      // The server drops the connection immediately, which is what arms the
      // reconnect timer on the client side.
      (socket) => socket.destroy(),
    );
    const client = new ReconnectingDaemonClient({
      reconnectDelaysMs: [reconnectDelayMs],
      socketPath: resource.socketPath,
    });
    resource.client = client;

    await waitFor(() => Boolean(reconnectTimerUnref));
    expect(socketUnref.mock.calls.length).toBeGreaterThan(0);
    expect(reconnectTimerUnref).toHaveBeenCalled();

    client.close();
  });
});

type Message = { id?: number | string; method?: string; params?: unknown };

async function startServer(
  socketPath: string,
  onMessage: (socket: Socket, message: Message) => void,
  onConnection: (socket: Socket) => void = () => undefined,
): Promise<Server> {
  if (existsSync(socketPath)) unlinkSync(socketPath);
  const server = createServer((socket) => {
    onConnection(socket);
    const decoder = new JsonLineDecoder();
    socket.on("data", (chunk) => {
      for (const message of decoder.push(chunk.toString("utf8"))) {
        onMessage(socket, message as Message);
      }
    });
  });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(socketPath, resolve);
  });
  return server;
}

function createResource() {
  const dir = mkdtempSync(join(tmpdir(), "herdsman-pi-client-"));
  const resource: {
    client?: ReconnectingDaemonClient;
    dir: string;
    server?: Server;
    socketPath: string;
  } = { dir, socketPath: join(dir, "daemon.sock") };
  resources.push(resource);
  return resource;
}

async function closeServer(server: Server | undefined): Promise<void> {
  if (!server) return;
  await new Promise<void>((resolve) => server.close(() => resolve()));
}

async function waitFor(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 1_000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  throw new Error("condition was not met");
}
