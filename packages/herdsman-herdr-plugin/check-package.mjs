#!/usr/bin/env bun
// @ts-check
import { execFileSync } from "node:child_process";

const bun = process.platform === "win32" ? "bun.exe" : "bun";
const output = execFileSync(bun, ["pm", "pack", "--dry-run"], { encoding: "utf8" });
const files = output
  .split("\n")
  .map((line) => line.match(/^packed\s+\S+\s+(.+?)\s*$/)?.[1])
  .filter(/** @returns {p is string} */ (p) => Boolean(p));

for (const required of ["index.mjs", "herdr-plugin.toml"]) {
  if (!files.includes(required)) {
    throw new Error(`Herdr plugin package is missing ${required}`);
  }
}

if (files.some((file) => file.startsWith("dist/"))) {
  throw new Error("Herdr plugin package must not include dist output");
}
