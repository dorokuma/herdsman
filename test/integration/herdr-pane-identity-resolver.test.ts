import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, test, vi } from "vitest";
import { resolveHerdrPaneIdentity } from "@/herdr/pane-identity-resolver.js";
import { cleanupTempDirs, openObservabilityDbHarness } from "./observability-db-harness.js";

afterEach(cleanupTempDirs);

const herdrPaneGetFixture = JSON.parse(
  readFileSync(
    join(dirname(fileURLToPath(import.meta.url)), "../fixtures/herdr-pane-get.json"),
    "utf8",
  ),
) as Record<string, unknown>;

describe("Herdr pane identity", () => {
  test("finds only running sessions by exact socket path", () => {
    const harness = openObservabilityDbHarness();
    harness.herdrSessions.upsertRunning({
      name: "default",
      sessionDir: "/tmp/herdr",
      socketPath: "/tmp/herdr.sock",
    });
    harness.herdrSessions.upsertRunning({
      name: "stopped",
      sessionDir: "/tmp/stopped",
      socketPath: "/tmp/stopped.sock",
    });
    harness.herdrSessions.markStoppedMissingFrom(["default"]);

    expect(harness.herdrSessions.findRunningBySocketPath("/tmp/herdr.sock")?.name).toBe("default");
    expect(harness.herdrSessions.findRunningBySocketPath("/tmp/stopped.sock")).toBeUndefined();
    expect(harness.herdrSessions.findRunningBySocketPath("/tmp/herdr.sock.other")).toBeUndefined();
  });

  test.each([
    {
      pane_id: "wB:p2",
      terminal_id: "term_2",
      workspace_id: "wB",
    },
    {
      pane: {
        paneId: "wB:p2",
        terminalId: "term_2",
        workspaceId: "wB",
      },
    },
  ])("normalizes direct and wrapped pane results", async (result) => {
    const close = vi.fn();
    const getPane = vi.fn().mockResolvedValue(result);

    await expect(
      resolveHerdrPaneIdentity({
        clientFactory: () => ({ close, getPane }),
        paneId: "wA:p1",
        socketPath: "/tmp/herdr.sock",
      }),
    ).resolves.toEqual({ paneId: "wB:p2", terminalId: "term_2", workspaceId: "wB" });
    expect(getPane).toHaveBeenCalledWith({ pane_id: "wA:p1" });
    expect(close).toHaveBeenCalledOnce();
  });

  test("locks the real Herdr pane.get shape and extracts cwd, not pid", async () => {
    const pane = herdrPaneGetFixture.pane as Record<string, unknown>;
    expect(herdrPaneGetFixture.type).toBe("pane_info");
    expect(pane).not.toHaveProperty("pid");
    expect(Object.keys(pane).sort()).toEqual([
      "agent",
      "agent_session",
      "agent_status",
      "cwd",
      "display_agent",
      "focused",
      "foreground_cwd",
      "label",
      "pane_id",
      "revision",
      "scroll",
      "state_labels",
      "tab_id",
      "terminal_id",
      "terminal_title",
      "terminal_title_stripped",
      "title",
      "tokens",
      "workspace_id",
    ]);

    const close = vi.fn();
    const getPane = vi.fn().mockResolvedValue(herdrPaneGetFixture);
    await expect(
      resolveHerdrPaneIdentity({
        clientFactory: () => ({ close, getPane }),
        paneId: "wB:p2",
        socketPath: "/tmp/herdr.sock",
      }),
    ).resolves.toEqual({
      agent: "pi",
      cwd: "/root/herdsman",
      foregroundCwd: "/root/herdsman",
      paneId: "wB:p2",
      tabId: "wB:t1",
      terminalId: "term_2",
      workspaceId: "wB",
    });
    expect(getPane).toHaveBeenCalledWith({ pane_id: "wB:p2" });
    expect(close).toHaveBeenCalledOnce();
  });

  test("ignores a pid field that is not part of pane.get", async () => {
    const close = vi.fn();
    const getPane = vi.fn().mockResolvedValue({
      type: "pane_info",
      pane: {
        pane_id: "wB:p2",
        pid: 4242,
        terminal_id: "term_2",
        workspace_id: "wB",
        cwd: "/repo",
      },
    });
    await expect(
      resolveHerdrPaneIdentity({
        clientFactory: () => ({ close, getPane }),
        paneId: "wB:p2",
        socketPath: "/tmp/herdr.sock",
      }),
    ).resolves.toEqual({
      cwd: "/repo",
      paneId: "wB:p2",
      terminalId: "term_2",
      workspaceId: "wB",
    });
    expect(close).toHaveBeenCalledOnce();
  });

  test("rejects incomplete identity and always closes the client", async () => {
    const close = vi.fn();
    await expect(
      resolveHerdrPaneIdentity({
        clientFactory: () => ({ close, getPane: vi.fn().mockResolvedValue({ pane_id: "wB:p2" }) }),
        paneId: "wB:p2",
        socketPath: "/tmp/herdr.sock",
      }),
    ).rejects.toThrow("Herdr pane response has no terminal identity");
    expect(close).toHaveBeenCalledOnce();
  });
});
