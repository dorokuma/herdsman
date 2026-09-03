import { afterEach, describe, expect, test } from "vitest";
import { STATUS_PLAN_MAX_ATTEMPTS } from "@/db/status-event-plans.js";
import {
  cleanupTempDirs,
  openObservabilityDbHarness,
} from "../integration/observability-db-harness.js";

afterEach(cleanupTempDirs);

describe("StatusEventPlanStore", () => {
  test("insertPending returns existing row when duplicate herdrEventKey matches, even when completed", () => {
    const harness = openObservabilityDbHarness();
    const store = harness.statusEventPlans;

    const row1 = store.insertPending({
      agentId: "ag_1",
      fromStatus: "working",
      herdrEventKey: "evt_123",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "done",
    });
    expect(row1.id).toBeDefined();
    expect(row1.status).toBe("pending");
    expect(row1.attempts).toBe(0);

    // mark completed
    store.markCompleted(row1.id);
    const completedRow = store.get(row1.id);
    expect(completedRow.status).toBe("completed");

    // try inserting another with same herdrSessionName and herdrEventKey
    const row2 = store.insertPending({
      agentId: "ag_1",
      fromStatus: "unknown",
      herdrEventKey: "evt_123",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "done",
    });

    expect(row2.id).toBe(row1.id);
    expect(row2.status).toBe("completed");

    harness.sqlite.close();
  });

  test("markRetry increments attempts and transitions to failed after 8 attempts", () => {
    const harness = openObservabilityDbHarness();
    const store = harness.statusEventPlans;

    const row = store.insertPending({
      agentId: "ag_1",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "done",
    });

    expect(STATUS_PLAN_MAX_ATTEMPTS).toBe(8);

    for (let i = 1; i <= 7; i++) {
      store.markRetry(row.id, new Error(`err_${i}`));
      const updated = store.get(row.id);
      expect(updated.attempts).toBe(i);
      expect(updated.status).toBe("pending");
      expect(updated.lastError).toBe(`err_${i}`);
    }

    // 8th attempt
    store.markRetry(row.id, new Error("err_8"));
    const failed = store.get(row.id);
    expect(failed.attempts).toBe(8);
    expect(failed.status).toBe("failed");
    expect(failed.lastError).toBe("err_8");

    harness.sqlite.close();
  });

  test("listUnfinished returns pending and running plans in order of id", () => {
    const harness = openObservabilityDbHarness();
    const store = harness.statusEventPlans;

    const row1 = store.insertPending({
      agentId: "ag_1",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "done",
    });
    const row2 = store.insertPending({
      agentId: "ag_2",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p2",
      toStatus: "done",
    });
    const row3 = store.insertPending({
      agentId: "ag_3",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p3",
      toStatus: "done",
    });

    store.markRunning(row2.id);
    store.markCompleted(row1.id);

    const unfinished = store.listUnfinished();
    expect(unfinished.map((p) => p.id)).toEqual([row2.id, row3.id]);
    expect(unfinished.map((p) => p.status)).toEqual(["running", "pending"]);

    store.markCancelled(row3.id);
    expect(store.listUnfinished().map((p) => p.id)).toEqual([row2.id]);

    harness.sqlite.close();
  });

  test("deleteSettledOlderThan removes aged settled rows and keeps recent settled and unfinished rows", () => {
    const harness = openObservabilityDbHarness();
    const store = harness.statusEventPlans;
    const weekMs = 7 * 24 * 60 * 60 * 1000;
    const age = (id: number, when: number) =>
      harness.sqlite
        .prepare("update status_event_plans set updated_at = ? where id = ?")
        .run(when, id);
    const insert = (agentId: string, paneId: string) =>
      store.insertPending({
        agentId,
        fromStatus: "working",
        herdrSessionName: "session_a",
        paneId,
        toStatus: "done",
      });

    const oldCompleted = insert("ag_1", "p1");
    store.markCompleted(oldCompleted.id);
    const oldCancelled = insert("ag_2", "p2");
    store.markCancelled(oldCancelled.id);
    const oldFailed = insert("ag_3", "p3");
    for (let i = 0; i < STATUS_PLAN_MAX_ATTEMPTS; i += 1) {
      store.markRetry(oldFailed.id, new Error("err"));
    }
    const freshSettled = insert("ag_4", "p4");
    store.markCompleted(freshSettled.id);
    const pendingRow = insert("ag_5", "p5");
    const runningRow = insert("ag_6", "p6");
    store.markRunning(runningRow.id);

    // Only the three settled rows are aged past the TTL.
    const old = Date.now() - weekMs - 60_000;
    age(oldCompleted.id, old);
    age(oldCancelled.id, old);
    age(oldFailed.id, old);

    expect(store.deleteSettledOlderThan(weekMs)).toBe(3);
    expect(() => store.get(oldCompleted.id)).toThrow();
    expect(() => store.get(oldCancelled.id)).toThrow();
    expect(() => store.get(oldFailed.id)).toThrow();
    // Recent settled and unfinished rows are untouched.
    expect(store.get(freshSettled.id).status).toBe("completed");
    expect(store.get(pendingRow.id).status).toBe("pending");
    expect(store.get(runningRow.id).status).toBe("running");

    harness.sqlite.close();
  });
});
