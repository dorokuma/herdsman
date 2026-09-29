#!/usr/bin/env node
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { argv, env, exit } from "node:process";
import { fileURLToPath } from "node:url";
import { getHerdsmanHome } from "@/config/runtime.js";
import { runObservabilityDaemonService } from "@/daemon/service.js";

const SYSTEMD_CGROUP_PATTERN = /\.service(?:$|\/)/m;

export function isSystemdSupervised(
  input: { cgroupContent?: string | undefined; environment?: NodeJS.ProcessEnv | undefined } = {},
): boolean {
  const environment = input.environment ?? env;
  if ((environment.INVOCATION_ID ?? "").trim().length > 0) return true;
  const cgroupContent = input.cgroupContent ?? readCgroupOrUndefined();
  return typeof cgroupContent === "string" && SYSTEMD_CGROUP_PATTERN.test(cgroupContent);
}

function readCgroupOrUndefined(): string | undefined {
  try {
    return readFileSync("/proc/self/cgroup", "utf8");
  } catch {
    return undefined;
  }
}

/**
 * Refuses an unsupervised start against the default data directory.
 *
 * `~/.herdsman` is the production data directory and is served only by the
 * systemd-managed instance. A foreground start is allowed only with an
 * explicit throwaway `HERDSMAN_HOME`; this is a guard against experimenting on
 * the production directory, not a supported way to run the production daemon.
 */
export function assertDaemonStartAllowed(
  input: { cgroupContent?: string | undefined; environment?: NodeJS.ProcessEnv | undefined } = {},
): void {
  const environment = input.environment ?? env;
  const homeDir = getHerdsmanHome(environment);
  if (homeDir !== getHerdsmanHome({})) return;
  if (isSystemdSupervised({ cgroupContent: input.cgroupContent, environment })) return;
  throw new Error(
    `Refusing to start: ${homeDir} is the production data directory and may only be served by the systemd-managed instance (herdsman.service). For development or verification, pass an explicit throwaway directory, e.g. HERDSMAN_HOME=/tmp/herdsman-dev.`,
  );
}

async function main(): Promise<void> {
  if (argv.length > 2) {
    throw new Error("herdsman-daemon does not accept CLI arguments");
  }
  assertDaemonStartAllowed();
  await runObservabilityDaemonService();
}

if (fileURLToPath(import.meta.url) === resolve(argv[1] ?? "")) {
  main().catch((error: unknown) => {
    console.error(error instanceof Error ? error.message : error);
    exit(1);
  });
}
