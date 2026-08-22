import { createConnection } from "node:net";
import { mkdtempSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { describe, expect, test } from "vitest";
import { createAgentHistoryService } from "@/agent-history/service.js";
import { ObservabilityRpcServer } from "@/daemon/observability-server.js";
import { AgentContextService } from "@/observability/agent-context-service.js";
import { AgentOrchestratorService } from "@/observability/agent-orchestrator-service.js";
import { openObservabilityDbHarness } from "../integration/observability-db-harness.js";

describe("observability server frame limits", () => {
  test("disconnects a socket after JsonLineFrameTooLargeError", async () => {
    const dir = mkdtempSync(join(tmpdir(), "herdsman-frame-limit-"));
    const harness = openObservabilityDbHarness();
    const history = createAgentHistoryService({ cache: harness.agentHistoryCache, homeDir: dir });
    const socketPath = join(dir, "rpc.sock");
    const server = new ObservabilityRpcServer({
      context: new AgentContextService({
        history,
        stores: { agentContextSnapshots: harness.agentContextSnapshots, agents: harness.agents },
      }),
      history,
      orchestrator: new AgentOrchestratorService({
        agentEvents: harness.agentEvents,
        agents: harness.agents,
        scopes: harness.agentOrchestratorScopes,
      }),
      socketPath,
      stores: {
        agentEvents: harness.agentEvents,
        agents: harness.agents,
        herdrSessions: harness.herdrSessions,
        herdrWorkspaces: harness.herdrWorkspaces,
      },
    });
    try {
      await server.start();
      const socket = createConnection(socketPath);
      await new Promise<void>((resolve, reject) => {
        socket.once("connect", () => {
          socket.once("close", resolve);
          socket.write("1".repeat(1024 * 1024 + 1));
        });
        socket.once("error", (error) => {
          if ((error as NodeJS.ErrnoException).code !== "EPIPE") reject(error);
        });
      });
      expect(socket.destroyed).toBe(true);
    } finally {
      await server.stop();
      harness.sqlite.close();
    }
  });
});
