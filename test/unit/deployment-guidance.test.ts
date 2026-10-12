import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

const repoRoot = fileURLToPath(new URL("../../", import.meta.url));

describe("deployment guidance", () => {
  test("sends the full gate to the release face instead of a pre-deploy re-run", () => {
    const agents = readFileSync(`${repoRoot}AGENTS.md`, "utf8");
    const releasing = readFileSync(`${repoRoot}docs/releasing.md`, "utf8");
    const deployBullet = agents
      .split("\n")
      .find((line) => line.startsWith("- 部署（registry 单一渠道）"));
    expect(deployBullet).toBeDefined();

    // The release face owns the full gate. The deployment section has to point at
    // that step by name, so the two documents cannot drift apart again.
    const validateSection = releasing
      .split(/^## /m)
      .find((section) => section.startsWith("Validate source and package contents"));
    expect(validateSection).toBeDefined();
    expect(validateSection).toContain("pnpm check");
    expect(deployBullet).toContain("Validate source and package contents");

    // Deployment itself never runs the gate — release rounds already ran it — so
    // the deployment section must not ask for a second run.
    expect(deployBullet).not.toMatch(/部署(或发布)?前[^。]*复跑/);
    expect(deployBullet).toContain("不复跑");
  });
});
