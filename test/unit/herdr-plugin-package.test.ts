import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { describe, expect, test } from "vitest";

const GO_COMMAND = "/root/.local/lib/herdsman-go/herdsman";

function readManifest() {
  return readFileSync("packages/herdsman-herdr-plugin/herdr-plugin.toml", "utf8");
}

describe("herdsman Herdr plugin package", () => {
  test("declares agent actions", () => {
    const manifest = readManifest();
    expect(manifest).toContain('id = "agent-list"');
    expect(manifest).toContain('title = "Show Herdsman agents"');
    expect(manifest).toContain('title = "Herdsman Agents"');
    expect(manifest).toContain('id = "agents"');
    expect(manifest).not.toContain('id = "legacy"');
  });

  test("manifest action and pane exec the Go herdsman binary", () => {
    const manifest = readManifest();
    const commands = [
      ...manifest.matchAll(/^command = \["([^"]+)", "([^"]+)", "([^"]+)"\]$/gm),
    ].map((match) => [match[1], match[2], match[3]]);

    // Both the [[actions]] entry and the [[panes]] entry must point at the Go
    // CLI — Node was removed from this host, so a node/index.mjs command would
    // be dead on arrival.
    expect(commands).toEqual([
      [GO_COMMAND, "agent", "list"],
      [GO_COMMAND, "agent", "list"],
    ]);
    expect(manifest).not.toContain("index.mjs");
    expect(manifest).not.toContain('"node"');
  });

  test("Go binary answers agent list", () => {
    // The vitest setup redirects HERDSMAN_HOME to an empty tmp dir; the live
    // daemon socket lives in the default home.
    const output = execFileSync(GO_COMMAND, ["agent", "list"], {
      encoding: "utf8",
      env: { ...process.env, HERDSMAN_HOME: undefined },
    });
    expect(output).toMatch(/^status\tname\tagent\tpane/m);
  }, 15_000);

  test("packages the manifest without build output", () => {
    const output = execFileSync("bun", ["pm", "pack", "--dry-run"], {
      cwd: "packages/herdsman-herdr-plugin",
      encoding: "utf8",
    });
    const files = output
      .split("\n")
      .map((line) => /^packed\s+\S+\s+(.+?)\s*$/.exec(line)?.[1])
      .filter((path): path is string => Boolean(path));

    expect(files).toContain("index.mjs");
    expect(files).toContain("herdr-plugin.toml");
    expect(files.some((file) => file.startsWith("dist/"))).toBe(false);
  }, 30_000);
});
