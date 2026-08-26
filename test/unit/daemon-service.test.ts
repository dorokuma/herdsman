import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test, vi } from "vitest";
import {
  PeriodicReconcileScheduler,
  RECONCILE_INTERVAL_MS,
  resolveMigrationsFolder,
} from "@/daemon/service.js";

const tempDirs: string[] = [];

afterEach(() => {
  for (const dir of tempDirs.splice(0)) rmSync(dir, { force: true, recursive: true });
});

describe("daemon service", () => {
  test("finds migrations from the package root instead of the launch cwd", () => {
    const root = mkdtempSync(join(tmpdir(), "herdsman-daemon-service-"));
    tempDirs.push(root);
    const moduleDir = join(root, "dist", "src", "daemon");
    const migrationsFolder = join(root, "drizzle");
    mkdirSync(moduleDir, { recursive: true });
    mkdirSync(join(migrationsFolder, "meta"), { recursive: true });
    writeFileSync(join(migrationsFolder, "meta", "_journal.json"), "{}\n");

    expect(resolveMigrationsFolder(moduleDir)).toBe(migrationsFolder);
  });
});

describe("periodic reconcile scheduling", () => {
  function scheduler(options: {
    run: () => Promise<void>;
    onClear?: () => void;
    onSet?: (callback: () => void) => void;
  }) {
    const { run, onClear, onSet } = options;
    return new PeriodicReconcileScheduler({
      intervalMs: RECONCILE_INTERVAL_MS,
      run,
      setInterval: (callback) => {
        onSet?.(callback);
        return 1 as unknown as ReturnType<typeof setInterval>;
      },
      clearInterval: () => onClear?.(),
    });
  }

  test("fires the reconcile run on the configured cadence", async () => {
    let tick: (() => void) | undefined;
    const runs: number[] = [];
    const instance = scheduler({
      run: async () => {
        runs.push(1);
      },
      onSet: (callback) => {
        tick = callback;
      },
    });
    instance.start();
    expect(runs).toHaveLength(0);
    tick?.();
    await vi.waitFor(() => expect(runs).toHaveLength(1));
    tick?.();
    await vi.waitFor(() => expect(runs).toHaveLength(2));
    await instance.stop();
  });

  test("skips ticks while a reconcile is in flight and resumes after it settles", async () => {
    let tick: (() => void) | undefined;
    let resolveRun: (() => void) | undefined;
    const runs: string[] = [];
    const instance = scheduler({
      run: () =>
        new Promise<void>((resolve) => {
          resolveRun = resolve;
          runs.push("started");
        }),
      onSet: (callback) => {
        tick = callback;
      },
    });
    instance.start();
    tick?.();
    await vi.waitFor(() => expect(runs).toEqual(["started"]));
    // Two more ticks while the first run is still in flight: both are skipped.
    tick?.();
    tick?.();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(runs).toEqual(["started"]);
    // Settle the in-flight run and let its promise chain (catch + finally that
    // clears the in-flight guard) fully drain before the next tick.
    resolveRun?.();
    await new Promise((resolve) => setTimeout(resolve, 0));
    tick?.();
    await vi.waitFor(() => expect(runs).toEqual(["started", "started"]));
    resolveRun?.();
    await instance.stop();
  });

  test("stop clears the interval and awaits the in-flight run", async () => {
    let tick: (() => void) | undefined;
    let cleared = 0;
    let resolveRun: (() => void) | undefined;
    let runSettled = false;
    const instance = scheduler({
      run: () =>
        new Promise<void>((resolve) => {
          resolveRun = resolve;
        }).finally(() => {
          runSettled = true;
        }),
      onClear: () => {
        cleared += 1;
      },
      onSet: (callback) => {
        tick = callback;
      },
    });
    instance.start();
    tick?.();
    await vi.waitFor(() => expect(resolveRun).toBeDefined());
    const stopped = instance.stop();
    resolveRun?.();
    await stopped;
    expect(cleared).toBe(1);
    expect(runSettled).toBe(true);
  });

  test("start is idempotent and a rejected run does not break the cadence", async () => {
    let tick: (() => void) | undefined;
    const warning = vi.spyOn(console, "warn").mockImplementation(() => {});
    const runs: string[] = [];
    const instance = scheduler({
      run: async () => {
        runs.push("attempt");
        throw new Error("boom");
      },
      onSet: (callback) => {
        tick = callback;
      },
    });
    instance.start();
    instance.start();
    tick?.();
    await vi.waitFor(() => expect(runs).toEqual(["attempt"]));
    expect(warning).toHaveBeenCalledWith("Herdsman periodic reconcile failed", expect.any(Error));
    // Let the catch/finally chain clear the in-flight guard before ticking again.
    await new Promise((resolve) => setTimeout(resolve, 0));
    tick?.();
    await vi.waitFor(() => expect(runs).toEqual(["attempt", "attempt"]));
    warning.mockRestore();
    await instance.stop();
  });
});
