import { appendFileSync, mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, isAbsolute, join } from "node:path";

export type HerdsmanPiLogLevel = "info" | "warn" | "error";

export function logHerdsmanPi(level: HerdsmanPiLogLevel, message: string): void {
  try {
    const configuredHome = process.env.HERDSMAN_HOME?.trim();
    const home = configuredHome && isAbsolute(configuredHome) ? configuredHome : join(homedir(), ".herdsman");
    const now = new Date();
    const date = now.toISOString().slice(0, 10).replaceAll("-", "");
    const file = join(home, "logs", `herdsman-pi-${date}.log`);
    mkdirSync(dirname(file), { recursive: true });
    appendFileSync(file, `${now.toISOString()} [${level}] ${message}\n`, "utf8");
  } catch {
    // Diagnostics must never write to the terminal or interrupt the extension.
  }
}
