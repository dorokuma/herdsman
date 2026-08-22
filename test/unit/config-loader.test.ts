import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { loadHerdsmanConfig } from "@/config/load.js";
import { parseHerdsmanConfig } from "@/config/schema.js";

const tempDirs: string[] = [];

afterEach(() => {
  for (const dir of tempDirs.splice(0)) {
    rmSync(dir, { force: true, recursive: true });
  }
});

describe("Herdsman config loader", () => {
  test("loads a valid observability runtime YAML config", () => {
    const path = writeTempConfig(`
runtime:
  db_path: data/state.db
  socket_path: herdsman.sock
  pid_path: herdsman.pid
  log_path: logs/herdsman.log
observability:
  telemetry:
    max_excerpt_bytes: 2048
`);

    const result = loadHerdsmanConfig(path);

    expect(result.ok).toBe(true);
    if (result.ok) {
      expect(result.value.runtime?.db_path).toBe("data/state.db");
      expect(result.value.runtime?.socket_path).toBe("herdsman.sock");
      expect(result.value.observability?.telemetry?.max_excerpt_bytes).toBe(2048);
    }
  });

  test("rejects unknown config fields", () => {
    const result = parseHerdsmanConfig({
      old_agents: { enabled: true },
      providers: { example: {} },
    });

    expect(result.ok).toBe(false);
    if (!result.ok)
      expect(result.errors.some((error) => error.keyword === "additionalProperties")).toBe(true);
  });

  test("returns YAML parse errors without throwing", () => {
    const path = writeTempConfig("runtime: [");

    const result = loadHerdsmanConfig(path);

    expect(result.ok).toBe(false);
  });
});

function writeTempConfig(contents: string): string {
  const dir = mkdtempSync(join(tmpdir(), "herdsman-config-"));
  tempDirs.push(dir);

  const path = join(dir, "herdsman.yaml");
  writeFileSync(path, contents);

  return path;
}
