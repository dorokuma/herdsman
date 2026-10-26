package db

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The port of test/unit/status-event-plan-store.test.ts.

func openStatusPlanStore(t *testing.T) *StatusEventPlanStore {
	t.Helper()
	return NewStatusEventPlanStore(openMigratedTestDB(t))
}

// planInput mirrors the common insertPending arguments of the TS tests.
func planInput(agentID string, paneID string) InsertPendingInput {
	return InsertPendingInput{
		AgentID:          agentID,
		FromStatus:       observability.AgentStatusWorking,
		HerdrSessionName: "session_a",
		PaneID:           paneID,
		ToStatus:         observability.AgentStatusDone,
	}
}

func insertPlan(t *testing.T, store *StatusEventPlanStore, input InsertPendingInput) StatusEventPlanRecord {
	t.Helper()
	record, err := store.InsertPending(input)
	if err != nil {
		t.Fatalf("insertPending: %v", err)
	}
	return record
}

func requirePlan(t *testing.T, store *StatusEventPlanStore, id int64) StatusEventPlanRecord {
	t.Helper()
	record, err := store.Get(id)
	if err != nil {
		t.Fatalf("get(%d): %v", id, err)
	}
	return record
}

func requirePlanMissing(t *testing.T, store *StatusEventPlanStore, id int64) {
	t.Helper()
	if _, err := store.Get(id); err == nil {
		t.Fatalf("get(%d) succeeded, want a not-found error", id)
	}
}

func planIDs(plans []StatusEventPlanRecord) []int64 {
	ids := make([]int64, 0, len(plans))
	for _, plan := range plans {
		ids = append(ids, plan.ID)
	}
	return ids
}

func TestStatusEventPlanStore(t *testing.T) {
	t.Run("insertPending returns existing row when duplicate herdrEventKey matches, even when completed", func(t *testing.T) {
		store := openStatusPlanStore(t)
		eventKey := "evt_123"

		row1 := insertPlan(t, store, InsertPendingInput{
			AgentID:          "ag_1",
			FromStatus:       observability.AgentStatusWorking,
			HerdrEventKey:    &eventKey,
			HerdrSessionName: "session_a",
			PaneID:           "p1",
			ToStatus:         observability.AgentStatusDone,
		})
		if row1.ID == 0 {
			t.Fatal("inserted plan id must be defined")
		}
		if row1.Status != StatusPlanPending {
			t.Fatalf("status = %q, want %q", row1.Status, StatusPlanPending)
		}
		if row1.Attempts != 0 {
			t.Fatalf("attempts = %d, want 0", row1.Attempts)
		}

		if err := store.MarkCompleted(row1.ID); err != nil {
			t.Fatalf("markCompleted: %v", err)
		}
		if got := requirePlan(t, store, row1.ID); got.Status != StatusPlanCompleted {
			t.Fatalf("status = %q, want %q", got.Status, StatusPlanCompleted)
		}

		row2 := insertPlan(t, store, InsertPendingInput{
			AgentID:          "ag_1",
			FromStatus:       observability.AgentStatusUnknown,
			HerdrEventKey:    &eventKey,
			HerdrSessionName: "session_a",
			PaneID:           "p1",
			ToStatus:         observability.AgentStatusDone,
		})
		if row2.ID != row1.ID {
			t.Fatalf("duplicate key insert id = %d, want %d", row2.ID, row1.ID)
		}
		if row2.Status != StatusPlanCompleted {
			t.Fatalf("duplicate key insert status = %q, want %q", row2.Status, StatusPlanCompleted)
		}
	})

	t.Run("markRetry increments attempts and transitions to failed after 8 attempts", func(t *testing.T) {
		store := openStatusPlanStore(t)
		row := insertPlan(t, store, planInput("ag_1", "p1"))

		if StatusPlanMaxAttempts != 8 {
			t.Fatalf("StatusPlanMaxAttempts = %d, want 8", StatusPlanMaxAttempts)
		}

		for i := 1; i <= 7; i++ {
			updated, err := store.MarkRetry(row.ID, fmt.Errorf("err_%d", i), nil)
			if err != nil {
				t.Fatalf("markRetry(%d): %v", i, err)
			}
			if updated == nil {
				t.Fatalf("markRetry(%d) = nil, want the retried plan", i)
			}
			if updated.Attempts != i {
				t.Fatalf("attempts = %d, want %d", updated.Attempts, i)
			}
			if updated.Status != StatusPlanPending {
				t.Fatalf("status = %q, want %q", updated.Status, StatusPlanPending)
			}
			if updated.LastError == nil || *updated.LastError != fmt.Sprintf("err_%d", i) {
				t.Fatalf("lastError = %v, want err_%d", updated.LastError, i)
			}
		}

		failed, err := store.MarkRetry(row.ID, errors.New("err_8"), nil)
		if err != nil {
			t.Fatalf("markRetry(8): %v", err)
		}
		if failed == nil {
			t.Fatal("markRetry(8) = nil, want the failed plan")
		}
		if failed.Attempts != 8 {
			t.Fatalf("attempts = %d, want 8", failed.Attempts)
		}
		if failed.Status != StatusPlanFailed {
			t.Fatalf("status = %q, want %q", failed.Status, StatusPlanFailed)
		}
		if failed.LastError == nil || *failed.LastError != "err_8" {
			t.Fatalf("lastError = %v, want err_8", failed.LastError)
		}
	})

	t.Run("listUnfinished returns pending and running plans in order of id", func(t *testing.T) {
		store := openStatusPlanStore(t)
		row1 := insertPlan(t, store, planInput("ag_1", "p1"))
		row2 := insertPlan(t, store, planInput("ag_2", "p2"))
		row3 := insertPlan(t, store, planInput("ag_3", "p3"))

		running, err := store.MarkRunning(row2.ID)
		if err != nil {
			t.Fatalf("markRunning: %v", err)
		}
		if !running {
			t.Fatal("markRunning = false, want true")
		}
		if err := store.MarkCompleted(row1.ID); err != nil {
			t.Fatalf("markCompleted: %v", err)
		}

		unfinished, err := store.ListUnfinished()
		if err != nil {
			t.Fatalf("listUnfinished: %v", err)
		}
		if got := planIDs(unfinished); !reflect.DeepEqual(got, []int64{row2.ID, row3.ID}) {
			t.Fatalf("listUnfinished ids = %v, want [%d %d]", got, row2.ID, row3.ID)
		}
		if unfinished[0].Status != StatusPlanRunning || unfinished[1].Status != StatusPlanPending {
			t.Fatalf("listUnfinished statuses = [%q %q], want [%q %q]",
				unfinished[0].Status, unfinished[1].Status, StatusPlanRunning, StatusPlanPending)
		}

		if err := store.MarkCancelled(row3.ID); err != nil {
			t.Fatalf("markCancelled: %v", err)
		}
		unfinished, err = store.ListUnfinished()
		if err != nil {
			t.Fatalf("listUnfinished: %v", err)
		}
		if got := planIDs(unfinished); !reflect.DeepEqual(got, []int64{row2.ID}) {
			t.Fatalf("listUnfinished ids = %v, want [%d]", got, row2.ID)
		}
	})

	t.Run("deleteSettledOlderThan removes aged settled rows and keeps recent settled and unfinished rows", func(t *testing.T) {
		store := openStatusPlanStore(t)
		weekMs := int64(7 * 24 * 60 * 60 * 1000)
		age := func(id int64, when int64) {
			t.Helper()
			if _, err := store.db.Exec(
				"update status_event_plans set updated_at = ? where id = ?", when, id,
			); err != nil {
				t.Fatalf("age plan %d: %v", id, err)
			}
		}
		insert := func(agentID string, paneID string) StatusEventPlanRecord {
			t.Helper()
			return insertPlan(t, store, planInput(agentID, paneID))
		}

		oldCompleted := insert("ag_1", "p1")
		if err := store.MarkCompleted(oldCompleted.ID); err != nil {
			t.Fatalf("markCompleted: %v", err)
		}
		oldCancelled := insert("ag_2", "p2")
		if err := store.MarkCancelled(oldCancelled.ID); err != nil {
			t.Fatalf("markCancelled: %v", err)
		}
		oldFailed := insert("ag_3", "p3")
		for i := 0; i < StatusPlanMaxAttempts; i++ {
			if _, err := store.MarkRetry(oldFailed.ID, errors.New("err"), nil); err != nil {
				t.Fatalf("markRetry: %v", err)
			}
		}
		freshSettled := insert("ag_4", "p4")
		if err := store.MarkCompleted(freshSettled.ID); err != nil {
			t.Fatalf("markCompleted: %v", err)
		}
		pendingRow := insert("ag_5", "p5")
		runningRow := insert("ag_6", "p6")
		if _, err := store.MarkRunning(runningRow.ID); err != nil {
			t.Fatalf("markRunning: %v", err)
		}

		// Only the three settled rows are aged past the TTL.
		old := time.Now().UnixMilli() - weekMs - 60_000
		age(oldCompleted.ID, old)
		age(oldCancelled.ID, old)
		age(oldFailed.ID, old)

		deleted, err := store.DeleteSettledOlderThan(weekMs)
		if err != nil {
			t.Fatalf("deleteSettledOlderThan: %v", err)
		}
		if deleted != 3 {
			t.Fatalf("deleteSettledOlderThan = %d, want 3", deleted)
		}
		requirePlanMissing(t, store, oldCompleted.ID)
		requirePlanMissing(t, store, oldCancelled.ID)
		requirePlanMissing(t, store, oldFailed.ID)
		// Recent settled and unfinished rows are untouched.
		if got := requirePlan(t, store, freshSettled.ID); got.Status != StatusPlanCompleted {
			t.Fatalf("fresh settled status = %q, want %q", got.Status, StatusPlanCompleted)
		}
		if got := requirePlan(t, store, pendingRow.ID); got.Status != StatusPlanPending {
			t.Fatalf("pending status = %q, want %q", got.Status, StatusPlanPending)
		}
		if got := requirePlan(t, store, runningRow.ID); got.Status != StatusPlanRunning {
			t.Fatalf("running status = %q, want %q", got.Status, StatusPlanRunning)
		}
	})

	t.Run("listWaitingHistory returns only pending plans with PLAN_WAITING_HISTORY last_error", func(t *testing.T) {
		store := openStatusPlanStore(t)
		row1 := insertPlan(t, store, planInput("ag_1", "p1"))
		row2 := insertPlan(t, store, planInput("ag_2", "p2"))
		insertPlan(t, store, planInput("ag_3", "p3"))

		if _, err := store.MarkRetry(row1.ID,
			errors.New(observability.PlanWaitingHistory), nil); err != nil {
			t.Fatalf("markRetry(row1): %v", err)
		}
		if _, err := store.MarkRetry(row2.ID, errors.New("OTHER_ERROR"), nil); err != nil {
			t.Fatalf("markRetry(row2): %v", err)
		}

		waiting, err := store.ListWaitingHistory()
		if err != nil {
			t.Fatalf("listWaitingHistory: %v", err)
		}
		if got := planIDs(waiting); !reflect.DeepEqual(got, []int64{row1.ID}) {
			t.Fatalf("listWaitingHistory ids = %v, want [%d]", got, row1.ID)
		}
		if waiting[0].LastError == nil || *waiting[0].LastError != observability.PlanWaitingHistory {
			t.Fatalf("lastError = %v, want %q", waiting[0].LastError, observability.PlanWaitingHistory)
		}
		if waiting[0].Status != StatusPlanPending {
			t.Fatalf("status = %q, want %q", waiting[0].Status, StatusPlanPending)
		}
	})

	t.Run("listFailed returns only failed plans in order of id", func(t *testing.T) {
		store := openStatusPlanStore(t)
		row1 := insertPlan(t, store, planInput("ag_1", "p1"))
		row2 := insertPlan(t, store, planInput("ag_2", "p2"))
		insertPlan(t, store, planInput("ag_3", "p3"))

		for i := 0; i < StatusPlanMaxAttempts; i++ {
			if _, err := store.MarkRetry(row1.ID, errors.New("err"), nil); err != nil {
				t.Fatalf("markRetry(row1): %v", err)
			}
			if _, err := store.MarkRetry(row2.ID, errors.New("err"), nil); err != nil {
				t.Fatalf("markRetry(row2): %v", err)
			}
		}

		failed, err := store.ListFailed()
		if err != nil {
			t.Fatalf("listFailed: %v", err)
		}
		if got := planIDs(failed); !reflect.DeepEqual(got, []int64{row1.ID, row2.ID}) {
			t.Fatalf("listFailed ids = %v, want [%d %d]", got, row1.ID, row2.ID)
		}
		for _, plan := range failed {
			if plan.Status != StatusPlanFailed {
				t.Fatalf("status = %q, want %q", plan.Status, StatusPlanFailed)
			}
		}
	})

	t.Run("insertPending cancels a superseded un-wakeable idle plan for the same agent", func(t *testing.T) {
		store := openStatusPlanStore(t)

		// A startup/recovered idle plan whose event can never wake an
		// orchestrator (agent.idle with payload from != working) is stuck
		// waiting for history.
		stale := insertPlan(t, store, InsertPendingInput{
			AgentID:          "ag_1",
			FromStatus:       observability.AgentStatusUnknown,
			HerdrSessionName: "session_a",
			PaneID:           "p1",
			ToStatus:         observability.AgentStatusIdle,
		})
		if _, err := store.MarkRetry(stale.ID,
			errors.New(observability.PlanWaitingHistory), nil); err != nil {
			t.Fatalf("markRetry(stale): %v", err)
		}
		staleWaiting := requirePlan(t, store, stale.ID)
		if staleWaiting.LastError == nil ||
			*staleWaiting.LastError != observability.PlanWaitingHistory {
			t.Fatalf("lastError = %v, want %q", staleWaiting.LastError, observability.PlanWaitingHistory)
		}
		if staleWaiting.Status != StatusPlanPending {
			t.Fatalf("status = %q, want %q", staleWaiting.Status, StatusPlanPending)
		}

		// A newer transition for the same agent arrives: the stale plan must not
		// execute late and register its transition (or ref).
		fresh := insertPlan(t, store, InsertPendingInput{
			AgentID:          "ag_1",
			FromStatus:       observability.AgentStatusIdle,
			HerdrSessionName: "session_a",
			PaneID:           "p1",
			ToStatus:         observability.AgentStatusWorking,
		})

		cancelled := requirePlan(t, store, stale.ID)
		if cancelled.LastError == nil || *cancelled.LastError != "PLAN_SUPERSEDED" {
			t.Fatalf("lastError = %v, want PLAN_SUPERSEDED", cancelled.LastError)
		}
		if cancelled.Status != StatusPlanCancelled {
			t.Fatalf("status = %q, want %q", cancelled.Status, StatusPlanCancelled)
		}
		inserted := requirePlan(t, store, fresh.ID)
		if inserted.Status != StatusPlanPending {
			t.Fatalf("fresh status = %q, want %q", inserted.Status, StatusPlanPending)
		}
		if inserted.ToStatus != observability.AgentStatusWorking {
			t.Fatalf("fresh toStatus = %q, want %q", inserted.ToStatus, observability.AgentStatusWorking)
		}
		unfinished, err := store.ListUnfinished()
		if err != nil {
			t.Fatalf("listUnfinished: %v", err)
		}
		if got := planIDs(unfinished); !reflect.DeepEqual(got, []int64{fresh.ID}) {
			t.Fatalf("listUnfinished ids = %v, want [%d]", got, fresh.ID)
		}
	})

	t.Run("insertPending keeps unfinished plans that can still deliver a wake", func(t *testing.T) {
		store := openStatusPlanStore(t)
		insertFor := func(agentID, paneID string, from, to observability.AgentStatus) StatusEventPlanRecord {
			t.Helper()
			return insertPlan(t, store, InsertPendingInput{
				AgentID:          agentID,
				FromStatus:       from,
				HerdrSessionName: "session_a",
				PaneID:           paneID,
				ToStatus:         to,
			})
		}
		waitHistory := func(id int64) {
			t.Helper()
			if _, err := store.MarkRetry(id,
				errors.New(observability.PlanWaitingHistory), nil); err != nil {
				t.Fatalf("markRetry(%d): %v", id, err)
			}
		}
		requireStatus := func(id int64, want StatusEventPlanStatus) {
			t.Helper()
			if got := requirePlan(t, store, id); got.Status != want {
				t.Fatalf("plan %d status = %q, want %q", id, got.Status, want)
			}
		}

		// Case A: a pending working -> done completion plan survives a pane flip
		// to idle, otherwise the round would lose its wake.
		done := insertFor("ag_done", "p1", observability.AgentStatusWorking, observability.AgentStatusDone)
		waitHistory(done.ID)
		flipToIdle := insertFor("ag_done", "p1", observability.AgentStatusDone, observability.AgentStatusIdle)
		requireStatus(done.ID, StatusPlanPending)

		// Case B: a working -> idle plan is wake-worthy (agent.idle from
		// working), so it survives the next transition too.
		workingIdle := insertFor("ag_working_idle", "p2", observability.AgentStatusWorking, observability.AgentStatusIdle)
		waitHistory(workingIdle.ID)
		resume := insertFor("ag_working_idle", "p2", observability.AgentStatusIdle, observability.AgentStatusWorking)
		requireStatus(workingIdle.ID, StatusPlanPending)

		// Case C: a recovered unknown -> done plan is wake-worthy and survives.
		unknownDone := insertFor("ag_unknown_done", "p3", observability.AgentStatusUnknown, observability.AgentStatusDone)
		waitHistory(unknownDone.ID)
		afterDone := insertFor("ag_unknown_done", "p3", observability.AgentStatusDone, observability.AgentStatusIdle)
		requireStatus(unknownDone.ID, StatusPlanPending)

		// Case D: same agent id in another session is out of scope; another
		// agent in the same session stays untouched; every superseded idle plan
		// of the targeted agent is cancelled together.
		otherAgent := insertFor("ag_other", "p4", observability.AgentStatusUnknown, observability.AgentStatusIdle)
		otherSession := insertPlan(t, store, InsertPendingInput{
			AgentID:          "ag_other",
			FromStatus:       observability.AgentStatusUnknown,
			HerdrSessionName: "session_b",
			PaneID:           "p5",
			ToStatus:         observability.AgentStatusIdle,
		})
		supersededIdle := insertFor("ag_other", "p6", observability.AgentStatusUnknown, observability.AgentStatusIdle)
		newer := insertFor("ag_other", "p6", observability.AgentStatusIdle, observability.AgentStatusWorking)
		requireStatus(otherAgent.ID, StatusPlanCancelled)
		requireStatus(supersededIdle.ID, StatusPlanCancelled)
		requireStatus(otherSession.ID, StatusPlanPending)

		// Case E: settled rows and the freshly inserted row are never touched.
		settled := insertFor("ag_settled", "p8", observability.AgentStatusUnknown, observability.AgentStatusIdle)
		if err := store.MarkCompleted(settled.ID); err != nil {
			t.Fatalf("markCompleted: %v", err)
		}
		settledFollowUp := insertFor("ag_settled", "p8", observability.AgentStatusIdle, observability.AgentStatusWorking)
		requireStatus(settled.ID, StatusPlanCompleted)

		unfinished, err := store.ListUnfinished()
		if err != nil {
			t.Fatalf("listUnfinished: %v", err)
		}
		want := []int64{
			done.ID,
			flipToIdle.ID,
			workingIdle.ID,
			resume.ID,
			unknownDone.ID,
			afterDone.ID,
			otherSession.ID,
			newer.ID,
			settledFollowUp.ID,
		}
		if got := planIDs(unfinished); !reflect.DeepEqual(got, want) {
			t.Fatalf("listUnfinished ids = %v, want %v", got, want)
		}
	})

	t.Run("markRetry with PLAN_WAITING_HISTORY exhausts to discarded instead of failed", func(t *testing.T) {
		store := openStatusPlanStore(t)
		row := insertPlan(t, store, planInput("ag_1", "p1"))

		for i := 0; i < StatusPlanMaxAttempts-1; i++ {
			updated, err := store.MarkRetry(row.ID,
				errors.New(observability.PlanWaitingHistory), nil)
			if err != nil {
				t.Fatalf("markRetry(%d): %v", i, err)
			}
			if updated == nil {
				t.Fatalf("markRetry(%d) = nil, want the retried plan", i)
			}
			if updated.Status != StatusPlanPending {
				t.Fatalf("status = %q, want %q", updated.Status, StatusPlanPending)
			}
		}

		final, err := store.MarkRetry(row.ID, errors.New(observability.PlanWaitingHistory), nil)
		if err != nil {
			t.Fatalf("markRetry(final): %v", err)
		}
		if final == nil {
			t.Fatal("markRetry(final) = nil, want the discarded plan")
		}
		if final.Status != StatusPlanDiscarded {
			t.Fatalf("status = %q, want %q", final.Status, StatusPlanDiscarded)
		}
		if final.LastError == nil || *final.LastError != observability.PlanWaitingHistory {
			t.Fatalf("lastError = %v, want %q", final.LastError, observability.PlanWaitingHistory)
		}
		if final.Attempts != StatusPlanMaxAttempts {
			t.Fatalf("attempts = %d, want %d", final.Attempts, StatusPlanMaxAttempts)
		}

		// Discarded plans must not appear in listFailed.
		failed, err := store.ListFailed()
		if err != nil {
			t.Fatalf("listFailed: %v", err)
		}
		if len(failed) != 0 {
			t.Fatalf("listFailed = %v, want []", planIDs(failed))
		}

		// Discarded plans are settled, so deleteSettledOlderThan purges them.
		weekMs := int64(7 * 24 * 60 * 60 * 1000)
		old := time.Now().UnixMilli() - weekMs - 60_000
		if _, err := store.db.Exec(
			"update status_event_plans set updated_at = ? where id = ?", old, row.ID,
		); err != nil {
			t.Fatalf("age the discarded plan: %v", err)
		}
		deleted, err := store.DeleteSettledOlderThan(weekMs)
		if err != nil {
			t.Fatalf("deleteSettledOlderThan: %v", err)
		}
		if deleted != 1 {
			t.Fatalf("deleteSettledOlderThan = %d, want 1", deleted)
		}
		requirePlanMissing(t, store, row.ID)
	})

	t.Run("markDiscarded updates status to discarded and preserves/updates last_error", func(t *testing.T) {
		store := openStatusPlanStore(t)
		row := insertPlan(t, store, planInput("ag_1", "p1"))

		if err := store.MarkDiscarded(row.ID, stringPtr("MANUALLY_DISCARDED")); err != nil {
			t.Fatalf("markDiscarded: %v", err)
		}
		discarded := requirePlan(t, store, row.ID)
		if discarded.Status != StatusPlanDiscarded {
			t.Fatalf("status = %q, want %q", discarded.Status, StatusPlanDiscarded)
		}
		if discarded.LastError == nil || *discarded.LastError != "MANUALLY_DISCARDED" {
			t.Fatalf("lastError = %v, want MANUALLY_DISCARDED", discarded.LastError)
		}
	})

	t.Run("markRetry guards against reviving rows in terminal states (completed, cancelled, failed, discarded) and returns null", func(t *testing.T) {
		store := openStatusPlanStore(t)
		terminalStatuses := []StatusEventPlanStatus{
			StatusPlanCompleted,
			StatusPlanCancelled,
			StatusPlanFailed,
			StatusPlanDiscarded,
		}

		for _, status := range terminalStatuses {
			row := insertPlan(t, store, planInput("ag_"+string(status), "p1"))
			switch status {
			case StatusPlanCompleted:
				if err := store.MarkCompleted(row.ID); err != nil {
					t.Fatalf("markCompleted: %v", err)
				}
			case StatusPlanCancelled:
				if err := store.MarkCancelled(row.ID); err != nil {
					t.Fatalf("markCancelled: %v", err)
				}
			case StatusPlanDiscarded:
				if err := store.MarkDiscarded(row.ID,
					stringPtr("ORIGINAL_DISCARD")); err != nil {
					t.Fatalf("markDiscarded: %v", err)
				}
			case StatusPlanFailed:
				for i := 0; i < StatusPlanMaxAttempts; i++ {
					if _, err := store.MarkRetry(row.ID,
						errors.New("ORIGINAL_FAIL"), nil); err != nil {
						t.Fatalf("markRetry: %v", err)
					}
				}
			}

			current := requirePlan(t, store, row.ID)
			if current.Status != status {
				t.Fatalf("status = %q, want %q", current.Status, status)
			}
			attemptsBefore := current.Attempts

			// Attempting to retry an already-terminal row must return nil and
			// not modify it.
			retryResult, err := store.MarkRetry(row.ID, errors.New("ATTEMPT_REVIVAL"), nil)
			if err != nil {
				t.Fatalf("markRetry on a %q row: %v", status, err)
			}
			if retryResult != nil {
				t.Fatalf("markRetry on a %q row = %+v, want nil", status, retryResult)
			}

			after := requirePlan(t, store, row.ID)
			if after.Status != status {
				t.Fatalf("status = %q, want %q", after.Status, status)
			}
			if after.Attempts != attemptsBefore {
				t.Fatalf("attempts = %d, want %d", after.Attempts, attemptsBefore)
			}
		}
	})

	t.Run("listDiscarded returns only discarded plans in order of id", func(t *testing.T) {
		store := openStatusPlanStore(t)
		row1 := insertPlan(t, store, planInput("ag_1", "p1"))
		row2 := insertPlan(t, store, planInput("ag_2", "p2"))
		row3 := insertPlan(t, store, planInput("ag_3", "p3"))

		if err := store.MarkDiscarded(row1.ID, stringPtr("DISCARDED_1")); err != nil {
			t.Fatalf("markDiscarded(row1): %v", err)
		}
		if err := store.MarkDiscarded(row2.ID, stringPtr("DISCARDED_2")); err != nil {
			t.Fatalf("markDiscarded(row2): %v", err)
		}
		if err := store.MarkCompleted(row3.ID); err != nil {
			t.Fatalf("markCompleted(row3): %v", err)
		}

		discarded, err := store.ListDiscarded()
		if err != nil {
			t.Fatalf("listDiscarded: %v", err)
		}
		if got := planIDs(discarded); !reflect.DeepEqual(got, []int64{row1.ID, row2.ID}) {
			t.Fatalf("listDiscarded ids = %v, want [%d %d]", got, row1.ID, row2.ID)
		}
		for _, plan := range discarded {
			if plan.Status != StatusPlanDiscarded {
				t.Fatalf("status = %q, want %q", plan.Status, StatusPlanDiscarded)
			}
		}
	})

	t.Run("markRetry returns null when WHERE condition matches 0 rows (changes === 0)", func(t *testing.T) {
		store := openStatusPlanStore(t)
		row := insertPlan(t, store, planInput("ag_1", "p1"))

		// The status changes behind the store's back, the way a concurrent
		// drain would.
		if _, err := store.db.Exec(
			"update status_event_plans set status = 'completed' where id = ?", row.ID,
		); err != nil {
			t.Fatalf("force the row to completed: %v", err)
		}

		retryResult, err := store.MarkRetry(row.ID, errors.New("CONCURRENT_ERROR"), nil)
		if err != nil {
			t.Fatalf("markRetry: %v", err)
		}
		if retryResult != nil {
			t.Fatalf("markRetry = %+v, want nil", retryResult)
		}
	})
}
