import { chmodSync, mkdirSync, mkdtempSync, rmSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { validateGrokHome } from "@/db/agents.js";
import { openObservabilityDbHarness } from "../integration/observability-db-harness.js";

const dirs: string[] = [];
afterEach(() => {
  for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true });
});
function tempDir() {
  const dir = mkdtempSync(join(tmpdir(), "herdsman-grok-"));
  dirs.push(dir);
  return dir;
}

describe("grokHome validation", () => {
  test("accepts an owned private real directory and records it", () => {
    const home = tempDir();
    expect(validateGrokHome(home)).toBe(home);
    expect(validateGrokHome("relative/path")).toBeNull();
    expect(validateGrokHome(join(home, "..", "escape"))).toBeNull();
    const link = join(home, "link");
    symlinkSync(home, link);
    expect(validateGrokHome(link)).toBeNull();
    const writable = join(home, "world");
    mkdirSync(writable);
    chmodSync(writable, 0o777);
    expect(validateGrokHome(writable)).toBeNull();

    const h = openObservabilityDbHarness();
    h.herdrSessions.upsertRunning({ name: "default", sessionDir: "/tmp", socketPath: "/tmp/sock" });
    const record = h.agents.replaceForSession({
      herdrSessionName: "default",
      agents: [
        {
          agent: "grok",
          pane_id: "p1",
          terminal_id: "t1",
          workspace_id: "w1",
          env: { GROK_HOME: home },
        },
      ],
    })[0];
    expect(record).toMatchObject({ agent: "grok", grokHome: home });
    h.sqlite.close();
  });
});
