import { readFile } from "node:fs/promises";
import { describe, expect, test } from "vitest";

const ROOT_JSON_LINES_PATH = "src/shared/json-lines.ts";
const PI_JSON_LINES_PATH = "packages/herdsman-pi/src/shared/json-lines.ts";

describe("shared json-lines copies", () => {
  test("keeps the Pi package copy byte-identical to src/shared/json-lines.ts", async () => {
    const [root, pi] = await Promise.all([
      readFile(ROOT_JSON_LINES_PATH),
      readFile(PI_JSON_LINES_PATH),
    ]);

    expect(
      root.equals(pi),
      `${ROOT_JSON_LINES_PATH} and ${PI_JSON_LINES_PATH} differ; copy one file onto the other to keep them in sync manually`,
    ).toBe(true);
  });
});
