import { mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach } from "vitest";

const herdsmanTestHome = join(tmpdir(), "herdsman-vitest-home");
mkdirSync(herdsmanTestHome, { recursive: true });
process.env.HERDSMAN_HOME = herdsmanTestHome;

afterEach(() => {
  process.env.HERDSMAN_HOME = herdsmanTestHome;
});
