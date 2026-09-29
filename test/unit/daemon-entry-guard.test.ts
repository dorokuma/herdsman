import { describe, expect, test } from "vitest";
import { assertDaemonStartAllowed, isSystemdSupervised } from "@/cli/herdsman-daemon.js";

const UNSUPERVISED_CGROUP = "0::/user.slice/user-0.slice/session-4.scope";
const SUPERVISED_CGROUP_V2 = "0::/system.slice/herdsman.service";
const SUPERVISED_CGROUP_V1 = "1:name=systemd:/system.slice/herdsman.service";

describe("daemon entry guard", () => {
  test("refuses an unsupervised start against the default production data directory", () => {
    expect(() =>
      assertDaemonStartAllowed({ cgroupContent: UNSUPERVISED_CGROUP, environment: {} }),
    ).toThrow(/production data directory.*herdsman\.service.*HERDSMAN_HOME=\/tmp\//s);
  });

  test("allows an unsupervised start with an explicit throwaway HERDSMAN_HOME", () => {
    expect(() =>
      assertDaemonStartAllowed({
        cgroupContent: UNSUPERVISED_CGROUP,
        environment: { HERDSMAN_HOME: "/tmp/herdsman-foreground-test" },
      }),
    ).not.toThrow();
  });

  test("allows the systemd-managed instance by INVOCATION_ID or service cgroup", () => {
    expect(
      isSystemdSupervised({
        cgroupContent: UNSUPERVISED_CGROUP,
        environment: { INVOCATION_ID: "x" },
      }),
    ).toBe(true);
    expect(() =>
      assertDaemonStartAllowed({
        cgroupContent: SUPERVISED_CGROUP_V2,
        environment: { INVOCATION_ID: "" },
      }),
    ).not.toThrow();
    expect(() =>
      assertDaemonStartAllowed({
        cgroupContent: SUPERVISED_CGROUP_V1,
        environment: { INVOCATION_ID: "" },
      }),
    ).not.toThrow();
    expect(() =>
      assertDaemonStartAllowed({
        cgroupContent: UNSUPERVISED_CGROUP,
        environment: { INVOCATION_ID: "" },
      }),
    ).toThrow(/production data directory/);
    expect(isSystemdSupervised({ cgroupContent: UNSUPERVISED_CGROUP, environment: {} })).toBe(
      false,
    );
  });
});
