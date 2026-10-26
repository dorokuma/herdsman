/**
 * X3 fixture: a real child process that runs the `herdsman-pi` extension through
 * a print-mode lifecycle and then simply returns, without ever calling
 * `process.exit()`.
 *
 * Pi's print mode tears its runtime down and lets the process drain, so every
 * handle the extension still holds referenced decides whether the process can
 * leave. That is exactly what the X3 hang was: the extension left a daemon
 * socket ESTABLISHED and referenced, so the finished `pi --print` process never
 * reached an empty event loop.
 *
 * Usage:
 *   tsx herdsman-pi-print-mode-lifecycle.ts <daemonSocketPath> <extensionPath> <scenario>
 *
 * `scenario` selects the lifecycle:
 * - `print`      a `print`-mode session (`hasUI: false`, `mode: "print"`), which
 *                must not open a daemon connection at all;
 * - `connecting` a session whose mode Pi did not report, which still connects
 *                and registers presence and must still let the process exit.
 */

type Handler = (...args: unknown[]) => unknown;

const [socketPath, extensionPath, scenario] = process.argv.slice(2);
if (!socketPath || !extensionPath || (scenario !== "print" && scenario !== "connecting")) {
  console.error(
    "usage: herdsman-pi-print-mode-lifecycle <daemonSocketPath> <extensionPath> <print|connecting>",
  );
  process.exitCode = 2;
} else {
  const handlers = new Map<string, Handler>();
  const pi = {
    on: (name: string, handler: Handler) => handlers.set(name, handler),
    registerCommand() {},
    registerMessageRenderer() {},
    // Deliberately absent: `sendMessage` is what arms the wake timer, and this
    // fixture models a run that never receives an agent update.
  };

  const { createHerdsmanPiExtension } = (await import(extensionPath)) as {
    createHerdsmanPiExtension: (options?: Record<string, unknown>) => (pi: unknown) => void;
  };
  createHerdsmanPiExtension()(pi);

  const theme = {
    bg: (_color: string, text: string) => text,
    bold: (text: string) => text,
    fg: (_color: string, text: string) => text,
  };
  const statuses = new Map<string, string | undefined>();
  const ctx = {
    hasUI: scenario === "print" ? false : undefined,
    isIdle: () => true,
    mode: scenario === "print" ? "print" : undefined,
    sessionManager: {
      getSessionFile: () => "/tmp/herdsman-pi-print-lifecycle.jsonl",
      getSessionId: () => "pi-print-lifecycle",
    },
    ui: {
      notify() {},
      setStatus(key: string, value?: string) {
        statuses.set(key, value);
      },
      theme,
    },
  };

  await handlers.get("session_start")?.({ reason: "startup", type: "session_start" }, ctx);
  // Model the run's own work: stay referenced until the extension has actually
  // finished its daemon round-trip (it only sets the footer status once the
  // registration response was applied), then shut down. Without this the
  // unref'd socket would be correct but never even connect, and the fixture
  // would prove nothing about a live connection.
  const deadline = Date.now() + 5_000;
  while (!statuses.has("herdsman") && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  await handlers.get("session_shutdown")?.({ reason: "shutdown", type: "session_shutdown" }, ctx);
  console.error(`herdsman-pi-print-lifecycle:done:${scenario}`);
}
