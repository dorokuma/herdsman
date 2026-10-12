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

  test("keeps the validation section on the deploy bullet's release-face footing", () => {
    const agents = readFileSync(`${repoRoot}AGENTS.md`, "utf8");
    const deployBullet = agents
      .split("\n")
      .find((line) => line.startsWith("- 部署（registry 单一渠道）"));
    expect(deployBullet).toBeDefined();

    const validationSection = agents
      .split(/^## /m)
      .find((section) => section.startsWith("验证步骤"));
    expect(validationSection).toBeDefined();

    // Motivation: these two passages drifted apart once. The deploy bullet was
    // reworded to stop demanding a pre-deploy gate run, while the validation
    // section still said the gate has to be re-run before deploying, so the file
    // contradicted itself. Both passages must now name the same release-face step
    // and both must state that deploying does not re-run the gate; that is the
    // assertion pair that stops the drift from happening a second time（防再次漂移）.
    expect(deployBullet).toContain("Validate source and package contents");
    expect(validationSection).toContain("Validate source and package contents");
    expect(deployBullet).toContain("不复跑");
    expect(validationSection).toContain("不复跑");

    // Neither passage may resurrect the pre-deploy re-run the release flow does
    // not do: a release-face gate is run on main, not at deploy time.
    expect(deployBullet).not.toMatch(/部署(或发布)?前[^。]*复跑/);
    expect(validationSection).not.toMatch(/部署(或发布)?前[^。]*复跑/);
    expect(validationSection).not.toMatch(/部署(或发布)?前[^。]*再跑/);
  });
});
