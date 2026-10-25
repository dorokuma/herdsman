// Driver for the TS↔Go dual-client interop test: it drives the real
// `ReconnectingDaemonClient` of packages/herdsman-pi against the Go
// ObservabilityRpcServer, so the two client implementations are exercised
// against one server at the same time.
//
// It is run by tsx from go/internal/daemon/observability_rpc_dual_client_test.go.
// Every RPC step and every pushed notification becomes one JSON object on
// stdout (`{name, ok, result}`) that the Go test waits for; the Go test drives
// the phases by writing command words on stdin, so the overlap of the two
// sessions is ordered by the test instead of raced.
import { ReconnectingDaemonClient } from "../../../../packages/herdsman-pi/src/daemon-client.ts";

const [socketPath, paneId, subscriberId] = process.argv.slice(2);
if (!socketPath || !paneId || !subscriberId) {
  process.stderr.write("usage: pi_client_driver.ts <socketPath> <paneId> <subscriberId>\n");
  process.exit(2);
}

// The scope constants mirror the Go fixture (obsHerdrSocketPath / obsSessionName
// / obsWorkspaceB / obsPiSessionFile).
const HERDR_SOCKET_PATH = "/tmp/herdr/herdr.sock";
const HERDR_SESSION_NAME = "default";
const WORKSPACE_ID = "wB";
const PI_SESSION_FILE = "/tmp/pi-session.jsonl";

const write = (value: unknown): void => {
  process.stdout.write(`${JSON.stringify(value)}\n`);
};

// A request step plus its result, reported under the RPC method name.
const step = async (name: string, run: () => Promise<unknown>): Promise<void> => {
  try {
    write({ name, ok: true, result: await run() });
  } catch (error) {
    write({ name, ok: false, error: error instanceof Error ? error.message : String(error) });
  }
};

const client = new ReconnectingDaemonClient({ reconnectDelaysMs: [50], socketPath });

client.onStreamMessage = (message) => {
  write({ name: message.method, ok: true, result: (message as { params?: unknown }).params });
};
client.onDisconnected = (error) => {
  write({ name: "disconnected", ok: false, error: error.message });
};

// Commands the Go test sends on stdin. `ack <eventId>` carries an argument.
const commands: Record<string, (argument: string) => Promise<void>> = {
  ack: async (argument) => {
    await step("agent.notifications.ack", () =>
      client.request("agent.notifications.ack", { eventId: Number(argument) }),
    );
  },
  claim: () =>
    step("agent.orchestrator.set(enabled)", () =>
      client.request("agent.orchestrator.set", { enabled: true }),
    ),
  list: async () => {
    await step("agent.list", () => client.request("agent.list", { workspaceId: WORKSPACE_ID }));
    await step("agent.ping", () => client.request("agent.ping", {}));
    await step("agent.turn.completed", () =>
      client.request("agent.turn.completed", {
        confirmed: true,
        herdrSessionName: HERDR_SESSION_NAME,
        paneId,
        terminalId: "term_1",
        workspaceId: WORKSPACE_ID,
      }),
    );
  },
  quit: async () => {
    write({ name: "done", ok: true });
    client.close();
    // The stdin pipe is the only handle left open; releasing it lets the loop
    // drain the queued stdout lines and exit 0 on its own.
    process.stdin.destroy();
  },
  state: () =>
    step("agent.orchestrator.get", () =>
      client.request("agent.orchestrator.get", {}),
    ),
};

const readCommands = async (): Promise<void> => {
  const decoder = new TextDecoder();
  let buffer = "";
  for await (const chunk of process.stdin as AsyncIterable<Uint8Array>) {
    buffer += decoder.decode(chunk, { stream: true });
    let index = buffer.indexOf("\n");
    while (index >= 0) {
      const line = buffer.slice(0, index).trim();
      buffer = buffer.slice(index + 1);
      index = buffer.indexOf("\n");
      if (line.length === 0) continue;
      const [command, argument] = line.split(" ");
      const handler = commands[command];
      if (!handler) {
        write({ name: `command:${command}`, ok: false, error: "unknown command" });
        continue;
      }
      await handler(argument ?? "");
      if (command === "quit") return;
    }
  }
};

client.onConnected = async () => {
  write({ name: "connected", ok: true });
  await step("agent.orchestrator.register", () =>
    client.request("agent.orchestrator.register", {
      herdrSocketPath: HERDR_SOCKET_PATH,
      paneId,
      sessionRef: { agent: "pi", kind: "path", source: "herdr:pi", value: PI_SESSION_FILE },
      subscriberId,
      subscriberKind: "pi",
      workspaceId: WORKSPACE_ID,
    }),
  );
  await readCommands();
};

// A socket that never answers must not hang the Go test until its own deadline.
const guard = setTimeout(() => {
  write({ name: "guard", ok: false, error: "the TS driver timed out" });
  process.exit(1);
}, 60_000);
guard.unref?.();
process.on("exit", () => clearTimeout(guard));
