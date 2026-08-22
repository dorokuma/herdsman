import { describe, expect, test } from "vitest";
import { parseHerdsmanConfig } from "@/config/schema.js";

describe("Herdsman config schema", () => {
  test("accepts minimal observability config with runtime paths", () => {
    const result = parseHerdsmanConfig({
      observability: { telemetry: { max_excerpt_bytes: 2048 } },
      runtime: {
        db_path: "data/state.db",
        log_path: "logs/herdsman.log",
        pid_path: "herdsman.pid",
        socket_path: "herdsman.sock",
      },
    });

    expect(result.ok).toBe(true);
  });

  test("defaults telemetry excerpt limit", () => {
    const result = parseHerdsmanConfig({ observability: { telemetry: {} } });
    expect(result).toMatchObject({
      ok: true,
      value: { observability: { telemetry: { max_excerpt_bytes: 4096 } } },
    });
  });

  test("rejects unknown top-level config surfaces", () => {
    for (const config of [
      { old_agents: { enabled: true } },
      { providers: { example: {} } },
      { orchestration: { queue: {} } },
    ]) {
      const result = parseHerdsmanConfig(config);
      expect(result.ok).toBe(false);
      if (!result.ok)
        expect(result.errors.some((error) => error.keyword === "additionalProperties")).toBe(true);
    }
  });

  test("rejects unknown runtime and observability keys", () => {
    expect(parseHerdsmanConfig({ runtime: { extra: "nope" } }).ok).toBe(false);
    expect(parseHerdsmanConfig({ observability: { retention: { days: 7 } } }).ok).toBe(false);
  });
});
