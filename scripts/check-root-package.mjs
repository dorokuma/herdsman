#!/usr/bin/env bun
import { execFileSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

const root = new URL("../", import.meta.url);
const manifest = JSON.parse(await readFile(new URL("package.json", root), "utf8"));
const bun = process.platform === "win32" ? "bun.exe" : "bun";
const output = execFileSync(bun, ["pm", "pack", "--dry-run"], {
  cwd: fileURLToPath(root),
  encoding: "utf8",
  stdio: ["ignore", "pipe", "inherit"],
});
const files = output
  .split("\n")
  .map((line) => line.match(/^packed\s+\S+\s+(.+?)\s*$/)?.[1])
  .filter(Boolean);
const tarball = output
  .split("\n")
  .map((line) => line.trim())
  .find((line) => line.endsWith(".tgz"));
const expectedTarball = `${manifest.name.replace(/^@/, "").replace("/", "-")}-${manifest.version}.tgz`;
const packed = { name: manifest.name, version: manifest.version };
if (tarball !== expectedTarball) {
  // Surface the mismatch through the same errors list below.
  packed.name = tarball ? `tarball ${tarball}` : undefined;
}
const required = [
  "dist/src/cli/herdsman.js",
  "dist/src/cli/herdsman-daemon.js",
  "drizzle/meta/_journal.json",
];
const topLevel = new Set(["LICENSE", "README.md", "package.json"]);
const unexpected = files.filter(
  (path) =>
    !topLevel.has(path) && !path.startsWith("dist/") && !path.startsWith("drizzle/"),
);
const stale = files.filter((path) => path.toLowerCase().includes("worker"));
const missing = required.filter((path) => !files.includes(path));
const errors = [];

if (tarball !== expectedTarball) {
  errors.push(`tarball: expected ${expectedTarball}, received ${tarball ?? "(none)"}`);
}
if (missing.length > 0) errors.push(`missing: ${missing.join(", ")}`);
if (unexpected.length > 0) errors.push(`unexpected: ${unexpected.join(", ")}`);
if (stale.length > 0) errors.push(`stale worker paths: ${stale.join(", ")}`);

if (errors.length > 0) {
  throw new Error(`Invalid root bun package:\n- ${errors.join("\n- ")}`);
}

console.log(`${manifest.name}@${manifest.version}: ${files.length} files`);
