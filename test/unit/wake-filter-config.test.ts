import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { loadWakeFilterConfig } from "../../packages/herdsman-pi/src/wake-filter-config.js";

const tempDirs: string[] = [];
const previousHome = process.env.HERDSMAN_HOME;

afterEach(() => {
  for (const dir of tempDirs.splice(0)) rmSync(dir, { force: true, recursive: true });
  if (previousHome === undefined) delete process.env.HERDSMAN_HOME;
  else process.env.HERDSMAN_HOME = previousHome;
});

function useHomeWithConfig(contents?: string): string {
  const dir = mkdtempSync(join(tmpdir(), "herdsman-wake-filter-"));
  tempDirs.push(dir);
  process.env.HERDSMAN_HOME = dir;
  if (contents !== undefined) writeFileSync(join(dir, "config.yaml"), contents, "utf8");
  return dir;
}

describe("Herdsman Pi wake filter config", () => {
  test("defaults to enabled with no extra patterns when config.yaml is absent", () => {
    useHomeWithConfig();
    expect(loadWakeFilterConfig({})).toEqual({ enabled: true, extraPatterns: [] });
  });

  test("defaults to enabled with no extra patterns when the wake section is absent", () => {
    useHomeWithConfig("runtime:\n  db_path: data/state.db\n");
    expect(loadWakeFilterConfig({})).toEqual({ enabled: true, extraPatterns: [] });
  });

  test("parses the documented wake subset", () => {
    useHomeWithConfig(
      [
        "# leading comment",
        "observability:",
        "  telemetry:",
        "    max_excerpt_bytes: 1024",
        "wake:",
        "  filter_upstream_errors: false # inline comment",
        "  extra_upstream_error_patterns:",
        '    - "checkpoint-stall"',
        "    - /foo\\s+bar/i",
        "runtime:",
        "  db_path: data/state.db",
        "",
      ].join("\n"),
    );

    expect(loadWakeFilterConfig({})).toEqual({
      enabled: false,
      extraPatterns: ["checkpoint-stall", "/foo\\s+bar/i"],
    });
  });

  test("accepts an explicit true switch and an empty pattern list", () => {
    useHomeWithConfig(
      ["wake:", "  filter_upstream_errors: true", "  extra_upstream_error_patterns: []", ""].join(
        "\n",
      ),
    );
    expect(loadWakeFilterConfig({})).toEqual({ enabled: true, extraPatterns: [] });
  });

  test("falls back to defaults without throwing on malformed wake yaml", () => {
    for (const broken of [
      "wake:\n      filter_upstream_errors: false\n",
      "wake:\n  filter_upstream_errors: maybe\n",
      "wake:\n  extra_upstream_error_patterns: [a, b]\n",
      "wake:\n  extra_upstream_error_patterns:\n\t- tabbed\n",
    ]) {
      useHomeWithConfig(broken);
      expect(() => loadWakeFilterConfig({})).not.toThrow();
      expect(loadWakeFilterConfig({})).toEqual({ enabled: true, extraPatterns: [] });
    }
  });

  test("environment variables override the file", () => {
    useHomeWithConfig(
      [
        "wake:",
        "  filter_upstream_errors: true",
        "  extra_upstream_error_patterns:",
        "    - file-pattern",
        "",
      ].join("\n"),
    );

    expect(
      loadWakeFilterConfig({
        HERDSMAN_WAKE_EXTRA_UPSTREAM_ERROR_PATTERNS: "env-a,env-b",
        HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS: "0",
      }),
    ).toEqual({ enabled: false, extraPatterns: ["env-a", "env-b"] });
    expect(
      loadWakeFilterConfig({
        HERDSMAN_WAKE_EXTRA_UPSTREAM_ERROR_PATTERNS: "env-newline-a\nenv-newline-b",
        HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS: "1",
      }),
    ).toEqual({ enabled: true, extraPatterns: ["env-newline-a", "env-newline-b"] });
  });

  test("ignores unsupported environment values and keeps the file value", () => {
    useHomeWithConfig("wake:\n  filter_upstream_errors: false\n");
    expect(loadWakeFilterConfig({ HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS: "sometimes" })).toEqual({
      enabled: false,
      extraPatterns: [],
    });
  });

  test("environment can force the filter on for emergency shutdown", () => {
    useHomeWithConfig("wake:\n  filter_upstream_errors: true\n");
    expect(loadWakeFilterConfig({ HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS: "false" })).toEqual({
      enabled: false,
      extraPatterns: [],
    });
    expect(loadWakeFilterConfig({ HERDSMAN_WAKE_FILTER_UPSTREAM_ERRORS: "true" })).toEqual({
      enabled: true,
      extraPatterns: [],
    });
  });
});
