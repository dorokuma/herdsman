import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { isInteractivePiAgent } from "@/observability/interactive-pi.js";

const tempDirs: string[] = [];
afterEach(() => { for (const dir of tempDirs.splice(0)) rmSync(dir, { force: true, recursive: true }); });
function agent(value: string | null | undefined) {
  return { agent: "pi", agentSession: value === undefined ? undefined : value === null ? null : { value } } as any;
}
describe("interactive Pi session classification", () => {
  test("rejects a session path containing .. as non-interactive", () => {
    expect(isInteractivePiAgent(agent("/root/.pi/agent/sessions/../x.jsonl"))).toBe(false);
  });
  test("classifies a real dispatched role session as non-interactive", () => {
    expect(isInteractivePiAgent(agent("/tmp/pi-role-sessions/role-x/session.jsonl"))).toBe(false);
  });
  test("classifies a real root Pi session as interactive", () => {
    const dir = mkdtempSync(join(tmpdir(), "interactive-pi-test-")); tempDirs.push(dir);
    const file = join(dir, "x.jsonl"); writeFileSync(file, "");
    expect(isInteractivePiAgent(agent(file))).toBe(true);
  });
  test("does not isolate Pi agents without a session path", () => {
    expect(isInteractivePiAgent(agent(undefined))).toBe(false);
    expect(isInteractivePiAgent(agent(null))).toBe(false);
    expect(isInteractivePiAgent(agent(""))).toBe(false);
  });
});
