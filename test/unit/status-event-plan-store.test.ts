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

  test("listWaitingHistory returns only pending plans with PLAN_WAITING_HISTORY last_error", () => {
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
    store.insertPending({
      agentId: "ag_3",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p3",
      toStatus: "done",
    });

    store.markRetry(row1.id, new Error("PLAN_WAITING_HISTORY"));
    store.markRetry(row2.id, new Error("OTHER_ERROR"));
    // row3 is clean pending without error

    const waiting = store.listWaitingHistory();
    expect(waiting.map((p) => p.id)).toEqual([row1.id]);
    expect(waiting[0]?.lastError).toBe("PLAN_WAITING_HISTORY");
    expect(waiting[0]?.status).toBe("pending");

    harness.sqlite.close();
  });

  test("listFailed returns only failed plans in order of id", () => {
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
    store.insertPending({
      agentId: "ag_3",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p3",
      toStatus: "done",
    });

    for (let i = 0; i < STATUS_PLAN_MAX_ATTEMPTS; i += 1) {
      store.markRetry(row1.id, new Error("err"));
      store.markRetry(row2.id, new Error("err"));
    }
    expect(store.listFailed().map((plan) => plan.id)).toEqual([row1.id, row2.id]);
    expect(store.listFailed().every((plan) => plan.status === "failed")).toBe(true);

    harness.sqlite.close();
  });

  test("insertPending cancels a superseded un-wakeable idle plan for the same agent", () => {
    const harness = openObservabilityDbHarness();
    const store = harness.statusEventPlans;

    // A startup/recovered idle plan whose event can never wake an orchestrator
    // (agent.idle with payload from != working) is stuck waiting for history.
    const stale = store.insertPending({
      agentId: "ag_1",
      fromStatus: "unknown",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "idle",
    });
    store.markRetry(stale.id, new Error("PLAN_WAITING_HISTORY"));
    expect(store.get(stale.id)).toMatchObject({
      lastError: "PLAN_WAITING_HISTORY",
      status: "pending",
    });

    // A newer transition for the same agent arrives: the stale plan must not
    // execute late and register its transition (or ref).
    const fresh = store.insertPending({
      agentId: "ag_1",
      fromStatus: "idle",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "working",
    });

    expect(store.get(stale.id)).toMatchObject({
      lastError: "PLAN_SUPERSEDED",
      status: "cancelled",
    });
    expect(store.get(fresh.id)).toMatchObject({ status: "pending", toStatus: "working" });
    expect(store.listUnfinished().map((plan) => plan.id)).toEqual([fresh.id]);

    harness.sqlite.close();
  });

  test("insertPending keeps unfinished plans that can still deliver a wake", () => {
    const harness = openObservabilityDbHarness();
    const store = harness.statusEventPlans;

    // Case A: a pending working -> done completion plan survives a pane flip to
    // idle, otherwise the round would lose its wake.
    const done = store.insertPending({
      agentId: "ag_done",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "done",
    });
    store.markRetry(done.id, new Error("PLAN_WAITING_HISTORY"));
    const flipToIdle = store.insertPending({
      agentId: "ag_done",
      fromStatus: "done",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "idle",
    });
    expect(store.get(done.id).status).toBe("pending");

    // Case B: a working -> idle plan is wake-worthy (agent.idle from working), so
    // it survives the next transition too.
    const workingIdle = store.insertPending({
      agentId: "ag_working_idle",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p2",
      toStatus: "idle",
    });
    store.markRetry(workingIdle.id, new Error("PLAN_WAITING_HISTORY"));
    const resume = store.insertPending({
      agentId: "ag_working_idle",
      fromStatus: "idle",
      herdrSessionName: "session_a",
      paneId: "p2",
      toStatus: "working",
    });
    expect(store.get(workingIdle.id).status).toBe("pending");

    // Case C: a recovered unknown -> done plan is wake-worthy and survives.
    const unknownDone = store.insertPending({
      agentId: "ag_unknown_done",
      fromStatus: "unknown",
      herdrSessionName: "session_a",
      paneId: "p3",
      toStatus: "done",
    });
    store.markRetry(unknownDone.id, new Error("PLAN_WAITING_HISTORY"));
    const afterDone = store.insertPending({
      agentId: "ag_unknown_done",
      fromStatus: "done",
      herdrSessionName: "session_a",
      paneId: "p3",
      toStatus: "idle",
    });
    expect(store.get(unknownDone.id).status).toBe("pending");

    // Case D: same agent id in another session is out of scope; another agent in
    // the same session stays untouched; every superseded idle plan of the
    // targeted agent is cancelled together.
    const otherAgent = store.insertPending({
      agentId: "ag_other",
      fromStatus: "unknown",
      herdrSessionName: "session_a",
      paneId: "p4",
      toStatus: "idle",
    });
    const otherSession = store.insertPending({
      agentId: "ag_other",
      fromStatus: "unknown",
      herdrSessionName: "session_b",
      paneId: "p5",
      toStatus: "idle",
    });
    const supersededIdle = store.insertPending({
      agentId: "ag_other",
      fromStatus: "unknown",
      herdrSessionName: "session_a",
      paneId: "p6",
      toStatus: "idle",
    });
    const newer = store.insertPending({
      agentId: "ag_other",
      fromStatus: "idle",
      herdrSessionName: "session_a",
      paneId: "p6",
      toStatus: "working",
    });
    expect(store.get(otherAgent.id).status).toBe("cancelled");
    expect(store.get(supersededIdle.id).status).toBe("cancelled");
    expect(store.get(otherSession.id).status).toBe("pending");

    // Case E: settled rows and the freshly inserted row are never touched.
    const settled = store.insertPending({
      agentId: "ag_settled",
      fromStatus: "unknown",
      herdrSessionName: "session_a",
      paneId: "p8",
      toStatus: "idle",
    });
    store.markCompleted(settled.id);
    const settledFollowUp = store.insertPending({
      agentId: "ag_settled",
      fromStatus: "idle",
      herdrSessionName: "session_a",
      paneId: "p8",
      toStatus: "working",
    });
    expect(store.get(settled.id).status).toBe("completed");

    expect(store.listUnfinished().map((plan) => plan.id)).toEqual([
      done.id,
      flipToIdle.id,
      workingIdle.id,
      resume.id,
      unknownDone.id,
      afterDone.id,
      otherSession.id,
      newer.id,
      settledFollowUp.id,
    ]);

    harness.sqlite.close();
  });

  test("markRetry with PLAN_WAITING_HISTORY exhausts to discarded instead of failed", () => {
    const harness = openObservabilityDbHarness();
    const store = harness.statusEventPlans;

    const row = store.insertPending({
      agentId: "ag_1",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "done",
    });

    for (let i = 0; i < STATUS_PLAN_MAX_ATTEMPTS - 1; i += 1) {
      store.markRetry(row.id, new Error("PLAN_WAITING_HISTORY"));
      expect(store.get(row.id).status).toBe("pending");
    }

    store.markRetry(row.id, new Error("PLAN_WAITING_HISTORY"));
    const finalRow = store.get(row.id);
    expect(finalRow.status).toBe("discarded");
    expect(finalRow.lastError).toBe("PLAN_WAITING_HISTORY");
    expect(finalRow.attempts).toBe(STATUS_PLAN_MAX_ATTEMPTS);

    // Discarded plans must not appear in listFailed
    expect(store.listFailed()).toEqual([]);

    // Discarded plans are settled, so deleteSettledOlderThan purges them
    const weekMs = 7 * 24 * 60 * 60 * 1000;
    const old = Date.now() - weekMs - 60_000;
    harness.sqlite
      .prepare("update status_event_plans set updated_at = ? where id = ?")
      .run(old, row.id);
    expect(store.deleteSettledOlderThan(weekMs)).toBe(1);
    expect(() => store.get(row.id)).toThrow();

    harness.sqlite.close();
  });

  test("markDiscarded updates status to discarded and preserves/updates last_error", () => {
    const harness = openObservabilityDbHarness();
    const store = harness.statusEventPlans;

    const row = store.insertPending({
      agentId: "ag_1",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "done",
    });

    store.markDiscarded(row.id, "MANUALLY_DISCARDED");
    expect(store.get(row.id).status).toBe("discarded");
    expect(store.get(row.id).lastError).toBe("MANUALLY_DISCARDED");

    harness.sqlite.close();
  });

  test("markRetry guards against reviving rows in terminal states (completed, cancelled, failed, discarded) and returns null", () => {
    const harness = openObservabilityDbHarness();
    const store = harness.statusEventPlans;

    const terminalStatuses = ["completed", "cancelled", "failed", "discarded"] as const;

    for (const status of terminalStatuses) {
      const row = store.insertPending({
        agentId: `ag_${status}`,
        fromStatus: "working",
        herdrSessionName: "session_a",
        paneId: "p1",
        toStatus: "done",
      });

      if (status === "completed") {
        store.markCompleted(row.id);
      } else if (status === "cancelled") {
        store.markCancelled(row.id);
      } else if (status === "discarded") {
        store.markDiscarded(row.id, "ORIGINAL_DISCARD");
      } else if (status === "failed") {
        for (let i = 0; i < STATUS_PLAN_MAX_ATTEMPTS; i += 1) {
          store.markRetry(row.id, new Error("ORIGINAL_FAIL"));
        }
      }

      expect(store.get(row.id).status).toBe(status);
      const attemptsBefore = store.get(row.id).attempts;

      // Attempting to retry an already-terminal row must return null and not modify it
      const retryResult = store.markRetry(row.id, new Error("ATTEMPT_REVIVAL"));
      expect(retryResult).toBeNull();

      const current = store.get(row.id);
      expect(current.status).toBe(status);
      expect(current.attempts).toBe(attemptsBefore);
    }

    harness.sqlite.close();
  });

  test("listDiscarded returns only discarded plans in order of id", () => {
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

    store.markDiscarded(row1.id, "DISCARDED_1");
    store.markDiscarded(row2.id, "DISCARDED_2");
    store.markCompleted(row3.id);

    const discarded = store.listDiscarded();
    expect(discarded.map((plan) => plan.id)).toEqual([row1.id, row2.id]);
    expect(discarded.every((plan) => plan.status === "discarded")).toBe(true);

    harness.sqlite.close();
  });

  test("markRetry returns null when WHERE condition matches 0 rows (changes === 0)", () => {
    const harness = openObservabilityDbHarness();
    const store = harness.statusEventPlans;

    const row = store.insertPending({
      agentId: "ag_1",
      fromStatus: "working",
      herdrSessionName: "session_a",
      paneId: "p1",
      toStatus: "done",
    });

    // Directly alter status to 'completed' behind the scenes without going through store.get check
    // by mocking a race condition where DB status changes right after get()
    harness.sqlite
      .prepare("update status_event_plans set status = 'completed' where id = ?")
      .run(row.id);

    // Call markRetry on the completed row
    const retryResult = store.markRetry(row.id, new Error("CONCURRENT_ERROR"));
    expect(retryResult).toBeNull();

    harness.sqlite.close();
  });
});
