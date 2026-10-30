import { execFileSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

type PackageManifest = {
  bin?: Record<string, string>;
  files?: string[];
  name: string;
  private?: boolean;
  publishConfig?: { access?: string };
  repository?: { directory?: string; type?: string; url?: string };
  scripts?: Record<string, string>;
  version: string;
};

async function readManifest(relativePath: string): Promise<PackageManifest> {
  return JSON.parse(
    await readFile(new URL(relativePath, import.meta.url), "utf8"),
  ) as PackageManifest;
}

describe("npm publication metadata", () => {
  test("publishes the scoped runtime and public Herdr integration", async () => {
    const root = await readManifest("../../package.json");
    const pi = await readManifest("../../packages/herdsman-pi/package.json");
    const herdr = await readManifest("../../packages/herdsman-herdr-plugin/package.json");
    const pluginToml = await readFile(
      new URL("../../packages/herdsman-herdr-plugin/herdr-plugin.toml", import.meta.url),
      "utf8",
    );
    const pluginVersion = /^version = "([^"]+)"$/m.exec(pluginToml)?.[1];

    expect(root.name).toBe("@dorokuma/herdsman");
    expect(root.bin).toEqual({ herdsman: "dist/src/cli/herdsman.js" });
    expect(root.files).toEqual(["dist", "drizzle"]);
    expect(root.publishConfig?.access).toBe("public");
    expect(root.scripts).toMatchObject({
      "clean:dist": "bun scripts/clean-dist.mjs",
      "package:check": "bun scripts/check-root-package.mjs",
      postinstall: "husky",
      prepack: "bun run build",
    });
    expect(root.scripts).not.toHaveProperty("prepare");
    expect(root.scripts?.build).toContain("bun run clean:dist");
    expect(root.scripts?.check).toContain("bun run package:check");

    expect(pi.name).toBe("@dorokuma/herdsman-pi");
    expect(pi.files).toEqual(["src", "LICENSE"]);
    expect(pi.publishConfig?.access).toBe("public");
    expect(pi.repository).toEqual({
      type: "git",
      url: "git+https://github.com/dorokuma/herdsman.git",
      directory: "packages/herdsman-pi",
    });

    expect(herdr.private).toBe(true);
    expect(pluginVersion).toBeDefined();
    expect(new Set([root.version, pi.version, herdr.version, pluginVersion])).toEqual(
      new Set([root.version]),
    );
  });

  test("includes LICENSE in the Pi package tarball", () => {
    const cwd = fileURLToPath(new URL("../../packages/herdsman-pi", import.meta.url));
    const output = execFileSync("bun", ["pm", "pack", "--dry-run"], {
      cwd,
      encoding: "utf8",
    });
    const files = output
      .split("\n")
      .map((line) => /^packed\s+\S+\s+(.+?)\s*$/.exec(line)?.[1])
      .filter((path): path is string => Boolean(path));
    expect(files).toContain("LICENSE");
  }, 30_000);
});
