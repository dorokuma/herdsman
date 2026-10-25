package observability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// This file ports the status-event-plan half of
// src/observability/agent-index-service.ts: the pending-plan store, the retry
// ring, the drain, and #appendStatusEvents with its degrade/release judgement.

// clearWaitingTimer mirrors #clearWaitingTimer.
func (s *AgentIndexService) clearWaitingTimer(planID int64, keepTracker bool) {
	s.mu.Lock()
	timer, ok := s.waitingHistoryTimers[planID]
	if ok {
		delete(s.waitingHistoryTimers, planID)
	}
	if !keepTracker {
		delete(s.planProgressTrackers, planID)
		delete(s.planWindowTrackers, planID)
	}
	s.mu.Unlock()
	if ok {
		s.clearRetry(timer)
	}
}

// findLiveAgent mirrors #findLiveAgent.
func (s *AgentIndexService) findLiveAgent(agent AgentIndexRecord) *AgentIndexRecord {
	closed, err := s.stores.Agents.IsPaneClosed(IsPaneClosedInput{
		HerdrSessionName: agent.HerdrSessionName,
		PaneID:           agent.PaneID,
		PaneGeneration:   agent.PaneGeneration,
	})
	if err == nil && closed {
		return nil
	}
	byPane, err := s.stores.Agents.FindByPane(FindAgentByPaneInput{
		HerdrSessionName: agent.HerdrSessionName,
		PaneID:           agent.PaneID,
		PaneGeneration:   agent.PaneGeneration,
	})
	if err == nil && byPane != nil {
		return byPane
	}
	found, err := s.stores.Agents.Get(agent.ID)
	if err != nil {
		return nil
	}
	return &found
}

// computeProgressFingerprint mirrors #computeProgressFingerprint.
func (s *AgentIndexService) computeProgressFingerprint(
	liveAgent *AgentIndexRecord,
	compactHistory *CompactAgentHistory,
) string {
	if compactHistory == nil && liveAgent == nil {
		return ""
	}
	updatedAt := ""
	messageCount := 0
	toolRef := ""
	toolTextLength := 0
	assistantRef := ""
	assistantText := ""
	if compactHistory != nil {
		if compactHistory.UpdatedAt != nil {
			updatedAt = *compactHistory.UpdatedAt
		}
		messageCount = compactHistory.MessageCount
		if compactHistory.LastToolResult != nil {
			toolRef = compactHistory.LastToolResult.Ref
			toolTextLength = len(compactHistory.LastToolResult.Text)
		}
		if compactHistory.LastAssistantMessage != nil {
			assistantRef = compactHistory.LastAssistantMessage.Ref
			assistantText = compactHistory.LastAssistantMessage.Text
		}
	}
	paneRevision := ""
	if liveAgent != nil && liveAgent.PaneRevision != nil {
		paneRevision = fmt.Sprintf("%d", *liveAgent.PaneRevision)
	}
	return fmt.Sprintf("%s|%d|%s|%d|%s|%s|%s",
		updatedAt, messageCount, toolRef, toolTextLength, assistantRef, assistantText, paneRevision)
}

// markPlanRetry mirrors #markPlanRetry.
func (s *AgentIndexService) markPlanRetry(
	planID int64,
	plan StatusEventPlan,
	retryError error,
	compactHistory *CompactAgentHistory,
	options *planRowOptions,
) (*StatusEventPlanRecord, error) {
	store := s.stores.StatusEventPlans
	if store == nil {
		return nil, nil
	}
	isDrain := options != nil && options.IsDrain
	allowWorkingExemption := !isDrain
	if options != nil && options.AllowWorkingExemption != nil {
		allowWorkingExemption = *options.AllowWorkingExemption
	}
	liveAgent := s.findLiveAgent(plan.Agent)
	agentKind := ""
	if plan.Agent.Agent != nil {
		agentKind = *plan.Agent.Agent
	}
	if liveAgent != nil && liveAgent.Agent != nil {
		agentKind = *liveAgent.Agent
	}
	isAgy := agentKind == "agy" ||
		(compactHistory != nil && compactHistory.Source != nil &&
			*compactHistory.Source == string(HistorySourceAntigravitySQLite))
	isLiveWorking := allowWorkingExemption && isAgy &&
		liveAgent != nil && liveAgent.AgentStatus == AgentStatusWorking

	// Invariant: when Herdr reports idle/done for an agy agent but the on-disk
	// history has not produced any assistant message newer than the latest
	// delivered completed turn ref, this is a suspected false idle. It is treated
	// as still working/in-progress and continues waiting within the total window
	// instead of entering premature discard countdown.
	latestDeliveredRef := ""
	if event, err := s.stores.AgentEvents.LatestCompletedTurnEvent(
		plan.Agent.ID, plan.Agent.HerdrSessionName,
	); err == nil && event != nil &&
		event.CompactHistory != nil && event.CompactHistory.LastAssistantMessage != nil {
		latestDeliveredRef = event.CompactHistory.LastAssistantMessage.Ref
	}
	currentAssistantRef := ""
	if compactHistory != nil && compactHistory.LastAssistantMessage != nil {
		currentAssistantRef = compactHistory.LastAssistantMessage.Ref
	}
	hasNewAssistantMessage := currentAssistantRef != "" && currentAssistantRef != latestDeliveredRef
	targetIsIdleOrDone := plan.To == AgentStatusIdle || plan.To == AgentStatusDone
	liveIsIdleOrDone := liveAgent != nil &&
		(liveAgent.AgentStatus == AgentStatusIdle || liveAgent.AgentStatus == AgentStatusDone)
	isSuspectedFalseIdle := allowWorkingExemption && isAgy &&
		(targetIsIdleOrDone || liveIsIdleOrDone) && !hasNewAssistantMessage

	currentPlan, err := store.Get(planID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	s.mu.Lock()
	windowTracker := s.planWindowTrackers[planID]
	if windowTracker == nil {
		windowTracker = &planWindowTracker{idleStartedAt: currentPlan.CreatedAtMs}
		if windowTracker.idleStartedAt == 0 {
			windowTracker.idleStartedAt = now
		}
		s.planWindowTrackers[planID] = windowTracker
	}
	s.mu.Unlock()

	if isLiveWorking {
		// 当 agent 处于 live working 时，重置/暂停 idle 窗口倒计时，不消耗 15 分钟预算
		windowTracker.idleStartedAt = now
	}

	elapsedIdleMs := now - windowTracker.idleStartedAt
	if elapsedIdleMs < 0 {
		elapsedIdleMs = 0
	}
	isWindowExpired := elapsedIdleMs >= int64(s.statusPlansConfig.TotalWindowMs)

	// Both genuine working status and suspected false idle are exempt from
	// premature discard, but suspected false idle is bounded by the total elastic
	// waiting window (default 15m). Genuine working is exempt from the window
	// entirely (countdown reset/paused during working).
	isTargetWorking := isLiveWorking || (isSuspectedFalseIdle && !isWindowExpired)

	stallDetected := false
	if isTargetWorking {
		currentFingerprint := s.computeProgressFingerprint(liveAgent, compactHistory)
		s.mu.Lock()
		tracker := s.planProgressTrackers[planID]
		if tracker == nil {
			s.planProgressTrackers[planID] = &planProgressTracker{
				firstSeenAt:             s.now(),
				lastProgressFingerprint: currentFingerprint,
				stallAttempts:           0,
			}
		} else if tracker.lastProgressFingerprint != currentFingerprint {
			tracker.lastProgressFingerprint = currentFingerprint
			tracker.stallAttempts = 0
		} else {
			tracker.stallAttempts += 1
		}
		tracker = s.planProgressTrackers[planID]
		s.mu.Unlock()
		attemptsStalled := tracker.stallAttempts >= WORKING_PLAN_MAX_STALL_ATTEMPTS
		timeStalled := s.now()-tracker.firstSeenAt >= s.maxWorkingPlanWaitMs
		stallDetected = attemptsStalled || timeStalled
	} else {
		s.mu.Lock()
		delete(s.planProgressTrackers, planID)
		s.mu.Unlock()
	}
	totalWindowMs := int64(s.statusPlansConfig.TotalWindowMs)
	updated, err := store.MarkRetry(planID, retryError, &MarkRetryOptions{
		IsTargetWorking: isTargetWorking,
		IsWindowExpired: isWindowExpired,
		StallDetected:   stallDetected,
		TotalWindowMs:   &totalWindowMs,
	})
	if err != nil {
		return nil, err
	}
	if updated != nil {
		switch updated.Status {
		case StatusPlanCompleted, StatusPlanCancelled, StatusPlanFailed,
			StatusPlanDiscarded:
			s.mu.Lock()
			delete(s.planProgressTrackers, planID)
			delete(s.planWindowTrackers, planID)
			s.mu.Unlock()
		}
	}
	return updated, nil
}

// scheduleWaitingHistoryRetry mirrors #scheduleWaitingHistoryRetry.
func (s *AgentIndexService) scheduleWaitingHistoryRetry(planID int64, plan StatusEventPlan) {
	s.clearWaitingTimer(planID, true)
	store := s.stores.StatusEventPlans
	attempts := 1
	if store != nil {
		current, err := store.Get(planID)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"Herdsman failed to read plan attempts for backoff; using initial backoff"+
					" (planId=%d, error=%v)\n", planID, err)
		} else {
			attempts = current.Attempts
		}
	}
	delayMs := s.computeBackoffMs(attempts)
	timer := s.scheduleRetry(func() {
		defer func() {
			if store != nil {
				current, err := store.Get(planID)
				if err == nil && isTerminalPlanStatus(current.Status) {
					s.clearWaitingTimer(planID, false)
				}
			}
		}()
		if store == nil {
			return
		}
		current, err := store.Get(planID)
		if err != nil {
			return
		}
		if current.Status != StatusPlanPending {
			s.clearWaitingTimer(planID, false)
			return
		}
		if err := s.enqueueAgentPlanError(plan.Agent.ID, func() error {
			return s.retryWaitingPlanRow(planID, plan)
		}); err != nil {
			// A failed retry callback used to be a silent debug line; the row it
			// was supposed to advance can then sit in `pending` forever with no
			// signal at all, so the failure is reported as a warning with the row
			// identity attached.
			fmt.Fprintf(os.Stderr,
				"Herdsman waiting history retry callback failed (planId=%d, agentId=%s,"+
					" herdrSessionName=%s, paneId=%s, error=%v)\n",
				planID, plan.Agent.ID, plan.Agent.HerdrSessionName, plan.Agent.PaneID, err)
		}
	}, delayMs)
	s.mu.Lock()
	s.waitingHistoryTimers[planID] = timer
	s.mu.Unlock()
}

func isTerminalPlanStatus(status StatusEventPlanStatus) bool {
	switch status {
	case StatusPlanCompleted, StatusPlanCancelled, StatusPlanFailed,
		StatusPlanDiscarded:
		return true
	default:
		return false
	}
}

// retryWaitingPlanRow mirrors #retryWaitingPlanRow.
func (s *AgentIndexService) retryWaitingPlanRow(planID int64, plan StatusEventPlan) error {
	store := s.stores.StatusEventPlans
	if store == nil {
		return nil
	}
	current, err := store.Get(planID)
	if err != nil {
		s.clearWaitingTimer(planID, false)
		return nil
	}

	refreshed, refreshErr := s.context.RefreshAgent(RefreshAgentContextInput{
		Agent:           plan.Agent,
		ForceRefresh:    boolPointer(true),
		IdentityChanged: false,
	})
	if refreshErr != nil {
		fmt.Fprintf(os.Stderr,
			"Herdsman failed to refresh agent before retry plan row, keeping waiting"+
				" (planId=%d, agentId=%s, error=%v)\n", planID, plan.Agent.ID, refreshErr)
		currentPlan, planErr := store.Get(planID)
		if planErr != nil {
			s.clearWaitingTimer(planID, false)
			return nil
		}
		var retryError error
		if currentPlan.LastError != nil && *currentPlan.LastError == DegradedPlanError {
			retryError = errors.New(DegradedPlanError)
		} else {
			retryError = &PlanWaitingHistoryError{}
		}
		// Design invariant: retryWaitingPlanRow executes as part of the live
		// runtime retry ring when the daemon is running normally and agent data is
		// fresh. Unlike startup drain (which does not exempt because on-disk agent
		// records might be stale), the live retry ring always maintains working
		// exemption (options omitted => allowWorkingExemption defaults to true).
		updated, retryErr := s.markPlanRetry(planID, plan, retryError, plan.CompactHistory, nil)
		if retryErr != nil {
			return retryErr
		}
		if updated == nil {
			s.clearWaitingTimer(planID, false)
			return nil
		}
		if updated.Status == StatusPlanPending {
			// Invariant: a synthetic retry reason never re-arms the ring (see
			// retryRingAuthorized); the pending row waits for the next refresh.
			if retryRingAuthorized(retryError) {
				s.scheduleWaitingHistoryRetry(planID, plan)
			}
		} else if updated.Status == StatusPlanDiscarded {
			s.clearWaitingTimer(planID, false)
			s.logInfo("Herdsman status event plan discarded after max attempts", map[string]any{
				"planId":           planID,
				"agentId":          plan.Agent.ID,
				"attempts":         updated.Attempts,
				"from":             plan.From,
				"to":               plan.To,
				"herdrSessionName": plan.Agent.HerdrSessionName,
			})
			event := s.appendPlanDiscardedEvent(appendPlanOutcomeInput{
				Agent:           plan.Agent,
				Attempts:        updated.Attempts,
				CompactHistory:  plan.CompactHistory,
				From:            plan.From,
				PlanCreatedAtMs: currentPlan.CreatedAtMs,
				PlanID:          planID,
				Reason:          updated.LastError,
				To:              plan.To,
			})
			s.publishPlanEvent(event)
		} else if updated.Status == StatusPlanFailed {
			s.clearWaitingTimer(planID, false)
			s.logWarn("Herdsman status event plan failed after max attempts", map[string]any{
				"planId":           planID,
				"agentId":          plan.Agent.ID,
				"attempts":         updated.Attempts,
				"from":             plan.From,
				"to":               plan.To,
				"herdrSessionName": plan.Agent.HerdrSessionName,
			})
			event := s.appendPlanFailedEvent(appendPlanOutcomeInput{
				Agent:           plan.Agent,
				Attempts:        updated.Attempts,
				CompactHistory:  plan.CompactHistory,
				From:            plan.From,
				PlanCreatedAtMs: currentPlan.CreatedAtMs,
				PlanID:          planID,
				Reason:          updated.LastError,
				To:              plan.To,
			})
			s.publishPlanEvent(event)
		}
		return nil
	}

	retryPlan := plan
	retryPlan.Attempts = &current.Attempts
	retryPlan.CompactHistory = &refreshed.Snapshot.CompactHistory
	event, err := s.runPlanRow(current, retryPlan, nil)
	if err != nil {
		return err
	}
	s.publishPlanEvent(event)
	return nil
}

// publishPlanEvent mirrors the `VALID_AGENT_EVENT_TYPES.has(...) -> onAgentEvent`
// funnel every plan-outcome call site runs.
func (s *AgentIndexService) publishPlanEvent(event *AgentEventRecord) {
	if event == nil {
		return
	}
	if _, ok := ValidAgentEventTypes[string(event.Type)]; !ok {
		return
	}
	s.onAgentEvent(*event)
}

// ExecuteStatusEventPlan mirrors executeStatusEventPlan.
func (s *AgentIndexService) ExecuteStatusEventPlan(
	plan StatusEventPlan,
) (*AgentEventRecord, error) {
	if plan.From == plan.To {
		return nil, nil
	}
	if s.stores.StatusEventPlans == nil {
		return enqueueAgentPlan(s, plan.Agent.ID, func() (*AgentEventRecord, error) {
			event, cancelled, err := s.appendStatusEvents(plan)
			if err != nil {
				var waiting *PlanWaitingHistoryError
				if errors.As(err, &waiting) {
					return nil, nil
				}
				return nil, err
			}
			if cancelled {
				return nil, nil
			}
			return event, nil
		})
	}
	var inserted StatusEventPlanRecord
	if plan.PlanID != nil {
		row, err := s.stores.StatusEventPlans.Get(*plan.PlanID)
		if err != nil {
			return nil, err
		}
		inserted = row
	} else {
		row, err := s.stores.StatusEventPlans.InsertPending(InsertPendingInput{
			Agent: &StatusEventPlanAgent{
				HerdrSessionName: plan.Agent.HerdrSessionName,
				ID:               plan.Agent.ID,
				PaneGeneration:   plan.Agent.PaneGeneration,
				PaneID:           plan.Agent.PaneID,
			},
			From:           plan.From,
			HerdrEventKey:  plan.HerdrEventKey,
			To:             plan.To,
			CompactHistory: plan.CompactHistory,
		})
		if err != nil {
			return nil, err
		}
		inserted = row
	}
	// Publication stays with the caller: every caller forwards this return value.
	// Pushing here as well would write the same `agent.event` twice on the socket,
	// breaking the "exactly once" delivery contract. The retry-exhausted failed
	// branch in #retryWaitingPlanRow keeps its own push because no caller sees its
	// result.
	return enqueueAgentPlan(s, plan.Agent.ID, func() (*AgentEventRecord, error) {
		return s.runPlanRow(inserted, plan, nil)
	})
}

// DrainPendingPlansOptions mirrors the TS drainPendingPlans options.
type DrainPendingPlansOptions struct {
	AllowWorkingExemption *bool
	Reason                string
}

// DrainPendingPlans mirrors drainPendingPlans.
func (s *AgentIndexService) DrainPendingPlans(options *DrainPendingPlansOptions) error {
	store := s.stores.StatusEventPlans
	if store == nil {
		return nil
	}
	if _, err := store.ResetRunningToPending(); err != nil {
		return err
	}
	rows, err := store.ListUnfinished()
	if err != nil {
		return err
	}
	allowWorkingExemption := false
	if options != nil {
		if options.AllowWorkingExemption != nil {
			allowWorkingExemption = *options.AllowWorkingExemption
		} else {
			allowWorkingExemption = options.Reason == "periodic"
		}
	}
	rowOptions := &planRowOptions{IsDrain: true, AllowWorkingExemption: &allowWorkingExemption}
	outcomes := make([]error, len(rows))
	for index, row := range rows {
		index, row := index, row
		outcomes[index] = s.enqueueAgentPlanError(row.AgentID, func() error {
			return s.drainPlanRow(row, rowOptions)
		})
	}
	// Individual rows may reject (after markRetry) but the drain itself never
	// rejects: every row is either drained, cancelled, or retried. A rejected row
	// is still an anomaly (its own drain path threw), so it is reported instead of
	// being silently folded into the settled results.
	rejected := 0
	messages := make([]string, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome != nil {
			rejected++
			messages = append(messages, outcome.Error())
		}
	}
	if rejected > 0 {
		fmt.Fprintf(os.Stderr,
			"Herdsman status event plan drain rejected rows (rejected=%d, total=%d,"+
				" errors=%v)\n", rejected, len(outcomes), messages)
	}
	if err := s.backfillFailedPlanEvents(); err != nil {
		return err
	}
	return s.backfillDiscardedPlanEvents()
}

func (s *AgentIndexService) backfillFailedPlanEvents() error {
	store := s.stores.StatusEventPlans
	if store == nil {
		return nil
	}
	rows, err := store.ListFailed()
	if err != nil {
		return err
	}
	backfilled := 0
	skipped := 0
	for _, row := range rows {
		// Per-row isolation: one unresolvable or unwritable row must not abort the
		// whole backfill round. The legacy loop had no guard, so the first
		// `FOREIGN KEY constraint failed` threw out of `drainPendingPlans` and no
		// later row was ever attempted.
		if row.LastError != nil && *row.LastError == PlanWaitingHistory {
			skipped++
			continue
		}
		existing, err := s.stores.AgentEvents.PlanOutcomeEventExists(
			row.HerdrSessionName, fmt.Sprintf("agent.failed:plan:%d", row.ID))
		if err != nil {
			skipped++
			continue
		}
		if existing {
			skipped++
			continue
		}
		agent := s.resolveBackfillAgent(row)
		if agent == nil {
			skipped++
			continue
		}
		event := s.appendPlanFailedEvent(appendPlanOutcomeInput{
			Agent:           *agent,
			Attempts:        row.Attempts,
			CompactHistory:  row.CompactHistory,
			From:            row.FromStatus,
			PlanCreatedAtMs: row.CreatedAtMs,
			PlanID:          row.ID,
			Reason:          row.LastError,
			To:              row.ToStatus,
		})
		// Phase 2 item 1: a failed row whose round carried no assistant content is
		// not recorded as `agent.failed` at all, so the backfill leaves it
		// un-backfilled by design (not an error).
		if event == nil {
			skipped++
			continue
		}
		// Stock terminal results are ingested silently: the row is written, then
		// immediately acknowledged/non-deliverable, so it stays queryable without
		// waking the current owner with a long-past failure.
		if err := s.stores.AgentEvents.MarkAcked(event.ID, nil); err != nil {
			skipped++
			s.logWarn("Herdsman skipping agent.failed backfill row", map[string]any{
				"agentId":          row.AgentID,
				"herdrSessionName": row.HerdrSessionName,
				"paneId":           row.PaneID,
				"planId":           row.ID,
				"error":            err,
			})
			continue
		}
		backfilled++
	}
	if backfilled > 0 || skipped > 0 {
		s.logInfo("Herdsman backfilled agent.failed rows", map[string]any{
			"backfilled": backfilled,
			"skipped":    skipped,
			"total":      backfilled + skipped,
		})
	}
	return nil
}

func (s *AgentIndexService) backfillDiscardedPlanEvents() error {
	store := s.stores.StatusEventPlans
	if store == nil {
		return nil
	}
	rows, err := store.ListDiscarded()
	if err != nil {
		return err
	}
	backfilled := 0
	skipped := 0
	for _, row := range rows {
		existing, err := s.stores.AgentEvents.PlanOutcomeEventExists(
			row.HerdrSessionName, fmt.Sprintf("agent.discarded:plan:%d", row.ID))
		if err != nil {
			skipped++
			continue
		}
		if existing {
			skipped++
			continue
		}
		agent := s.resolveBackfillAgent(row)
		if agent == nil {
			skipped++
			continue
		}
		event := s.appendPlanDiscardedEvent(appendPlanOutcomeInput{
			Agent:           *agent,
			Attempts:        row.Attempts,
			CompactHistory:  row.CompactHistory,
			From:            row.FromStatus,
			PlanCreatedAtMs: row.CreatedAtMs,
			PlanID:          row.ID,
			Reason:          row.LastError,
			To:              row.ToStatus,
		})
		// Same silent-ingest contract as the failed backfill.
		if err := s.stores.AgentEvents.MarkAcked(event.ID, nil); err != nil {
			skipped++
			s.logWarn("Herdsman skipping agent.discarded backfill row", map[string]any{
				"agentId":          row.AgentID,
				"herdrSessionName": row.HerdrSessionName,
				"paneId":           row.PaneID,
				"planId":           row.ID,
				"error":            err,
			})
			continue
		}
		backfilled++
	}
	if backfilled > 0 || skipped > 0 {
		s.logInfo("Herdsman backfilled agent.discarded rows", map[string]any{
			"backfilled": backfilled,
			"skipped":    skipped,
			"total":      backfilled + skipped,
		})
	}
	return nil
}

// resolveBackfillAgent mirrors #resolveBackfillAgent.
func (s *AgentIndexService) resolveBackfillAgent(row StatusEventPlanRecord) *AgentIndexRecord {
	agent, err := s.stores.Agents.Get(row.AgentID)
	if err == nil {
		return &agent
	}
	stitched := s.minimalAgentFromLatestEvent(minimalAgentInput{
		AgentID:          row.AgentID,
		CreatedBeforeMs:  row.CreatedAtMs,
		HerdrSessionName: row.HerdrSessionName,
		PaneGeneration:   row.PaneGeneration,
		PaneID:           row.PaneID,
	})
	if stitched == nil {
		s.logWarn("Herdsman skipping terminal event backfill; no agent row or pane events",
			map[string]any{
				"agentId":          row.AgentID,
				"herdrSessionName": row.HerdrSessionName,
				"paneId":           row.PaneID,
				"planId":           row.ID,
			})
	}
	return stitched
}

type minimalAgentInput struct {
	AgentID          string
	CreatedBeforeMs  int64
	HerdrSessionName string
	PaneGeneration   *string
	PaneID           string
}

// minimalAgentFromLatestEvent mirrors #minimalAgentFromLatestEvent.
//
// The lookup is keyed by `(herdr_session_name, pane_id)` plus the plan's
// `pane_generation`, and is bounded by `createdBefore` (the plan row's
// `created_at`). Matching on `agent_id` alone cannot work here: physical deletion
// of the `agents` row runs `ON DELETE SET NULL` over every event, so the legacy
// stitch always came back empty, and a pane-id-only match would happily stitch an
// old failure onto a newer instance of the same pane. The generation and time
// bounds are what keep the two apart.
func (s *AgentIndexService) minimalAgentFromLatestEvent(
	input minimalAgentInput,
) *AgentIndexRecord {
	latest, err := s.stores.AgentEvents.LatestPaneEventScope(LatestPaneEventScopeInput{
		CreatedBeforeMs:  input.CreatedBeforeMs,
		HerdrSessionName: input.HerdrSessionName,
		PaneGeneration:   input.PaneGeneration,
		PaneID:           input.PaneID,
	})
	if err != nil || latest == nil {
		return nil
	}
	if latest.PaneID == nil || *latest.PaneID == "" || latest.WorkspaceID == nil ||
		*latest.WorkspaceID == "" {
		return nil
	}
	herdrSessionName := input.HerdrSessionName
	if latest.HerdrSessionName != "" {
		herdrSessionName = latest.HerdrSessionName
	}
	return &AgentIndexRecord{
		Agent:            nil,
		AgentSession:     nil,
		AgentStatus:      AgentStatusUnknown,
		Cwd:              nil,
		FirstSeenAt:      "1970-01-01T00:00:00.000Z",
		Focused:          false,
		ForegroundCwd:    nil,
		HerdrSessionName: herdrSessionName,
		ID:               input.AgentID,
		LastSeenAt:       "1970-01-01T00:00:00.000Z",
		Name:             nil,
		PaneID:           *latest.PaneID,
		PaneRevision:     nil,
		PaneGeneration:   latest.PaneGeneration,
		TabID:            nil,
		TerminalID:       latest.TerminalID,
		WorkspaceID:      *latest.WorkspaceID,
	}
}

// salvageableTerminalBody mirrors #salvageableTerminalBody.
func (s *AgentIndexService) salvageableTerminalBody(
	row StatusEventPlanRecord,
) *CompactAgentHistory {
	recorded, err := s.stores.AgentEvents.PaneEventTail(PaneEventTailInput{
		CreatedBeforeMs:  row.CreatedAtMs,
		HerdrSessionName: row.HerdrSessionName,
		Limit:            20,
		PaneGeneration:   row.PaneGeneration,
		PaneID:           row.PaneID,
	})
	if err != nil {
		return nil
	}
	delivered, err := s.stores.AgentEvents.PaneDeliveredTerminalBodies(
		PaneDeliveredTerminalBodiesInput{
			CreatedBeforeMs:  row.CreatedAtMs,
			HerdrSessionName: row.HerdrSessionName,
			Limit:            STALE_DUPLICATE_GUARD_SCAN_LIMIT,
			PaneGeneration:   row.PaneGeneration,
			PaneID:           row.PaneID,
		})
	if err != nil {
		return nil
	}
	if len(delivered) >= STALE_DUPLICATE_GUARD_SCAN_LIMIT {
		// A full window means the newest STALE_DUPLICATE_GUARD_SCAN_LIMIT
		// delivered/acked terminal rows for this pane came back with no room to
		// spare, so any older row is invisible to the `alreadyDelivered` check
		// below — that saturation is the only trace a beyond-window salvage can
		// leave, so it is surfaced once per pane.
		s.noteSalvageWindowSaturated(noteWindowSaturatedInput{
			HerdrSessionName: row.HerdrSessionName,
			PaneID:           row.PaneID,
			Scanned:          len(delivered),
		})
	}
	// The pane's current agent kind, taken from the newest delivered row's
	// payload — the closest surviving evidence once the `agents` row is gone.
	// `sameTerminalAssistantContent` compares agy/antigravity tails by ref alone,
	// so a hardcoded "pi" would dedupe an agy body that shares a ref with an older
	// delivery but carries new text, and skip a real salvage.
	var deliveredKind *string
	if len(delivered) > 0 {
		deliveredKind = delivered[0].AgentKind
	}
	alreadyDelivered := func(body *CompactAgentHistory) bool {
		if body == nil {
			return false
		}
		for _, other := range delivered {
			if sameTerminalAssistantContent(body, other.CompactHistory, deliveredKind) {
				return true
			}
		}
		return false
	}
	if isTerminalAssistant(row.CompactHistory) &&
		HasNonEmptyAssistantMessage(row.CompactHistory) &&
		!alreadyDelivered(row.CompactHistory) {
		return row.CompactHistory
	}
	for _, candidate := range recorded {
		if candidate.DeliveryAttempts != 0 {
			continue
		}
		parsed := candidate.CompactHistory
		if !isTerminalAssistant(parsed) || !HasNonEmptyAssistantMessage(parsed) {
			continue
		}
		if alreadyDelivered(parsed) {
			continue
		}
		return parsed
	}
	return nil
}

// salvageOrphanTerminalEvent mirrors #salvageOrphanTerminalEvent.
func (s *AgentIndexService) salvageOrphanTerminalEvent(
	row StatusEventPlanRecord,
) *AgentEventRecord {
	body := s.salvageableTerminalBody(row)
	if body == nil {
		return nil
	}
	// Same orphan construction H1 uses for plan outcomes: resolve the pane's own
	// metadata from its event history, keep `agent_id = null` when the `agents`
	// row is gone, and leave the original id / pane / session in the payload.
	orphanAgent := AgentIndexRecord{
		Agent:            nil,
		AgentSession:     nil,
		AgentStatus:      AgentStatusUnknown,
		Cwd:              nil,
		FirstSeenAt:      "1970-01-01T00:00:00.000Z",
		Focused:          false,
		ForegroundCwd:    nil,
		HerdrSessionName: row.HerdrSessionName,
		ID:               row.AgentID,
		LastSeenAt:       "1970-01-01T00:00:00.000Z",
		Name:             nil,
		PaneID:           row.PaneID,
		PaneRevision:     nil,
		PaneGeneration:   row.PaneGeneration,
		TabID:            nil,
		TerminalID:       nil,
		WorkspaceID:      "",
	}
	// Deliverability is decided *before* the append. `#appendPlanFailedEvent`
	// stitches the orphan scope from the pane's own event history, but an event
	// written during *this* plan's execution is newer than the plan row, and the
	// pane may carry no earlier event at all. The orphan row then keeps the
	// placeholders above: `isDeliverableAgentEvent` rejects it and
	// `publishAgentEvent` dead-letters it, while the idempotency key
	// `agent.failed:plan:<id>` would make every later append of this plan return
	// that dead row — a strictly worse outcome than the cancel this method returns
	// instead.
	if !s.orphanScopeDeliverable(orphanAgent, row.CreatedAtMs) {
		s.logWarn(
			"Herdsman left a status event plan cancellable because the pane scope could not be stitched",
			map[string]any{
				"agentId":          row.AgentID,
				"herdrSessionName": row.HerdrSessionName,
				"paneId":           row.PaneID,
				"planId":           row.ID,
				"reason":           ORPHAN_SALVAGE_REASON,
			})
		return nil
	}
	event := s.appendPlanFailedEvent(appendPlanOutcomeInput{
		Agent:           orphanAgent,
		Attempts:        row.Attempts,
		CompactHistory:  body,
		From:            row.FromStatus,
		PlanCreatedAtMs: row.CreatedAtMs,
		PlanID:          row.ID,
		Reason:          stringPointer(ORPHAN_SALVAGE_REASON),
		To:              row.ToStatus,
	})
	if event == nil {
		// Unreachable: the Phase 2 item 1 guard only refuses a round with no
		// assistant content, and `#salvageableTerminalBody` returns a body only
		// when it holds a non-empty terminal assistant message.
		return nil
	}
	s.logWarn(
		"Herdsman salvaged an orphan terminal event for a plan whose agent row is gone",
		map[string]any{
			"agentId":          row.AgentID,
			"eventId":          event.ID,
			"from":             row.FromStatus,
			"herdrSessionName": row.HerdrSessionName,
			"paneId":           row.PaneID,
			"planId":           row.ID,
			"reason":           ORPHAN_SALVAGE_REASON,
			"to":               row.ToStatus,
		})
	return event
}

func stringPointer(value string) *string { return &value }

// orphanScopeDeliverable mirrors #orphanScopeDeliverable.
func (s *AgentIndexService) orphanScopeDeliverable(
	orphanAgent AgentIndexRecord,
	planCreatedAtMs int64,
) bool {
	resolved := s.resolvePlanOutcomeAgent(orphanAgent, &planCreatedAtMs)
	return resolved.agent.TerminalID != nil && resolved.agent.WorkspaceID != ""
}

type planOutcomeResolution struct {
	agent           AgentIndexRecord
	agentRowPresent bool
}

// salvageOrCancelMissingAgentPlan mirrors #salvageOrCancelMissingAgentPlan.
func (s *AgentIndexService) salvageOrCancelMissingAgentPlan(
	row StatusEventPlanRecord,
) *AgentEventRecord {
	s.mu.Lock()
	delete(s.planProgressTrackers, row.ID)
	// Same leak as `stopWaitingHistoryRetries`: the plan is settled here for
	// good, so its idle-window anchor must go with it.
	delete(s.planWindowTrackers, row.ID)
	s.mu.Unlock()
	var salvaged *AgentEventRecord
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				// The salvage must never take the drain down: a failed write falls
				// back to the pre-existing cancel behaviour.
				s.logWarn(
					"Herdsman failed to salvage a status event plan for a missing agent",
					map[string]any{
						"agentId":          row.AgentID,
						"herdrSessionName": row.HerdrSessionName,
						"paneId":           row.PaneID,
						"planId":           row.ID,
						"error":            recovered,
					})
			}
		}()
		salvaged = s.salvageOrphanTerminalEvent(row)
		if salvaged != nil {
			// The content reached the orphan channel, so the plan is settled — not
			// cancelled, and not left pending for the next tick to lose again.
			if err := s.stores.StatusEventPlans.MarkCompleted(row.ID); err != nil {
				salvaged = nil
			}
		}
	}()
	if salvaged != nil {
		return salvaged
	}
	// The row may already be gone; the drain must continue regardless.
	_ = s.stores.StatusEventPlans.MarkCancelled(row.ID)
	s.logWarn("Herdsman cancelling status event plan for missing agent", map[string]any{
		"agentId":          row.AgentID,
		"herdrSessionName": row.HerdrSessionName,
		"paneId":           row.PaneID,
		"from":             row.FromStatus,
		"to":               row.ToStatus,
	})
	return nil
}

// publishSalvagedEvent mirrors #publishSalvagedEvent.
func (s *AgentIndexService) publishSalvagedEvent(event *AgentEventRecord) {
	if event == nil {
		return
	}
	if _, ok := ValidAgentEventTypes[string(event.Type)]; !ok {
		return
	}
	s.onAgentEvent(*event)
}

type planRowOptions struct {
	AllowWorkingExemption *bool
	IsDrain               bool
}

// drainPlanRow mirrors #drainPlanRow.
func (s *AgentIndexService) drainPlanRow(row StatusEventPlanRecord, options *planRowOptions) error {
	store := s.stores.StatusEventPlans
	if store == nil {
		return nil
	}
	var current *StatusEventPlanRecord
	if err := runIndexTransaction(s.stores.Sqlite, func() error {
		candidate, err := store.Get(row.ID)
		if err != nil {
			return err
		}
		if candidate.Status != StatusPlanPending {
			return nil
		}
		current = &candidate
		return nil
	}); err != nil {
		s.clearWaitingTimer(row.ID, false)
		return nil
	}
	if current == nil {
		s.clearWaitingTimer(row.ID, false)
		return nil
	}

	agent, err := s.stores.Agents.FindByPane(FindAgentByPaneInput{
		HerdrSessionName: current.HerdrSessionName,
		PaneID:           current.PaneID,
		PaneGeneration:   current.PaneGeneration,
	})
	if err != nil || agent == nil {
		fallback, getErr := s.stores.Agents.Get(current.AgentID)
		if getErr != nil {
			agent = nil
		} else {
			agent = &fallback
		}
	}
	if agent == nil {
		// The agent row is gone (pane retired while the plan was pending). Cancel
		// only when the plan has no terminal content left to hand over; the salvage
		// is published from here because no caller sees this path's result (the
		// drain owns it).
		salvaged := s.salvageOrCancelMissingAgentPlan(*current)
		s.publishSalvagedEvent(salvaged)
		return nil
	}
	activeCompact := current.CompactHistory
	refreshed, refreshErr := s.context.RefreshAgent(RefreshAgentContextInput{
		Agent:           *agent,
		ForceRefresh:    boolPointer(true),
		IdentityChanged: false,
	})
	if refreshErr != nil {
		var retryError error
		if current.LastError != nil && *current.LastError == DegradedPlanError {
			retryError = errors.New(DegradedPlanError)
		} else {
			retryError = &PlanWaitingHistoryError{}
		}
		plan := StatusEventPlan{
			Agent:          *agent,
			Attempts:       &current.Attempts,
			CompactHistory: current.CompactHistory,
			From:           current.FromStatus,
			To:             current.ToStatus,
			HerdrEventKey:  current.HerdrEventKey,
		}
		updated, retryErr := s.markPlanRetry(current.ID, plan, retryError, current.CompactHistory, options)
		if retryErr != nil {
			return retryErr
		}
		if updated == nil {
			s.clearWaitingTimer(current.ID, false)
			return nil
		}
		if updated.Status == StatusPlanPending {
			s.logWarn("Herdsman failed to refresh agent during drain plan row, keeping waiting",
				map[string]any{
					"planId":  current.ID,
					"agentId": agent.ID,
					"error":   refreshErr,
				})
			// Invariant: the ring is only re-armed by a genuine
			// PlanWaitingHistoryError; a degraded row stays pending for the next
			// refresh to drain instead of spinning this callback again.
			if retryRingAuthorized(retryError) {
				s.scheduleWaitingHistoryRetry(current.ID, plan)
			}
		} else if updated.Status == StatusPlanDiscarded {
			s.clearWaitingTimer(current.ID, false)
			s.logInfo("Herdsman plan marked discarded during drain", map[string]any{
				"planId":  current.ID,
				"agentId": agent.ID,
				"from":    current.FromStatus,
				"to":      current.ToStatus,
				"reason":  updated.LastError,
			})
			event := s.appendPlanDiscardedEvent(appendPlanOutcomeInput{
				Agent:           *agent,
				Attempts:        updated.Attempts,
				CompactHistory:  current.CompactHistory,
				From:            current.FromStatus,
				PlanCreatedAtMs: current.CreatedAtMs,
				PlanID:          current.ID,
				Reason:          updated.LastError,
				To:              current.ToStatus,
			})
			// 下游投影器负责具体的抑制与噪声控制，因此此处统一为无条件推送，与重试环及
			// runPlanRow 路径判据保持一致。
			s.publishPlanEvent(event)
		} else if updated.Status == StatusPlanFailed {
			s.clearWaitingTimer(current.ID, false)
			s.logWarn("Herdsman plan marked failed", map[string]any{
				"planId":  current.ID,
				"agentId": agent.ID,
				"from":    current.FromStatus,
				"to":      current.ToStatus,
				"reason":  updated.LastError,
			})
			event := s.appendPlanFailedEvent(appendPlanOutcomeInput{
				Agent:           *agent,
				Attempts:        updated.Attempts,
				CompactHistory:  current.CompactHistory,
				From:            current.FromStatus,
				PlanCreatedAtMs: current.CreatedAtMs,
				PlanID:          current.ID,
				Reason:          updated.LastError,
				To:              current.ToStatus,
			})
			s.publishPlanEvent(event)
		}
		return nil
	}
	activeCompact = &refreshed.Snapshot.CompactHistory
	plan := StatusEventPlan{
		Agent:          *agent,
		Attempts:       &current.Attempts,
		CompactHistory: activeCompact,
		From:           current.FromStatus,
		To:             current.ToStatus,
		HerdrEventKey:  current.HerdrEventKey,
	}
	drainOptions := options
	if drainOptions == nil {
		drainOptions = &planRowOptions{IsDrain: true}
	}
	event, err := s.runPlanRow(*current, plan, drainOptions)
	if err != nil {
		return err
	}
	s.publishPlanEvent(event)
	return nil
}

// enqueueAgentPlan mirrors #enqueueAgentPlan: operations for one agent
// serialize, and a failed prior operation does not block the next.
func enqueueAgentPlan[T any](
	s *AgentIndexService,
	agentID string,
	operation func() (T, error),
) (T, error) {
	var zero T
	s.mu.Lock()
	previous := s.planTailByAgent[agentID]
	tail := make(chan struct{})
	s.planTailByAgent[agentID] = tail
	s.mu.Unlock()
	if previous != nil {
		<-previous
	}
	result, err := operation()
	close(tail)
	s.mu.Lock()
	if s.planTailByAgent[agentID] == tail {
		delete(s.planTailByAgent, agentID)
	}
	s.mu.Unlock()
	if err != nil {
		return zero, err
	}
	return result, nil
}

// enqueueAgentPlanError is the error-only seam the plan-row operations use.
func (s *AgentIndexService) enqueueAgentPlanError(agentID string, operation func() error) error {
	_, err := enqueueAgentPlan(s, agentID, func() (struct{}, error) {
		err := operation()
		return struct{}{}, err
	})
	return err
}

// runPlanRow mirrors #runPlanRow.
func (s *AgentIndexService) runPlanRow(
	row StatusEventPlanRecord,
	plan StatusEventPlan,
	options *planRowOptions,
) (*AgentEventRecord, error) {
	store := s.stores.StatusEventPlans
	if store == nil {
		return nil, nil
	}
	current, err := store.Get(row.ID)
	if err != nil {
		return nil, err
	}
	if isTerminalPlanStatus(current.Status) {
		s.clearWaitingTimer(row.ID, false)
		return nil, nil
	}
	running, err := store.MarkRunning(row.ID)
	if err != nil {
		return nil, err
	}
	if !running {
		s.clearWaitingTimer(row.ID, false)
		return nil, nil
	}

	activePlan := plan
	activePlan.Attempts = &row.Attempts
	// Row correlation for observability (see #logDegradedRelease): the plan id is
	// only known here, where the store row is in hand.
	activePlan.PlanID = &row.ID
	if row.Attempts > 0 {
		refreshed, refreshErr := s.context.RefreshAgent(RefreshAgentContextInput{
			Agent:           plan.Agent,
			ForceRefresh:    boolPointer(true),
			IdentityChanged: false,
		})
		if refreshErr != nil {
			s.logWarn("Herdsman failed to refresh agent before running retry row, keeping waiting",
				map[string]any{
					"planId":  row.ID,
					"agentId": plan.Agent.ID,
					"error":   refreshErr,
				})
			// Keep the degraded reason when the plan was already degraded by an
			// earlier round: markRetry decides discarded (silent) vs failed (wakes)
			// from this message, so passing PLAN_WAITING_HISTORY here would let a
			// degraded plan exhaust silently.
			var retryError error
			if row.LastError != nil && *row.LastError == DegradedPlanError {
				retryError = errors.New(DegradedPlanError)
			} else {
				retryError = &PlanWaitingHistoryError{}
			}
			updated, retryErr := s.markPlanRetry(row.ID, plan, retryError, plan.CompactHistory, options)
			if retryErr != nil {
				return nil, retryErr
			}
			if updated == nil {
				s.clearWaitingTimer(row.ID, false)
				return nil, nil
			}
			if updated.Status == StatusPlanPending {
				// Invariant: only a genuine PlanWaitingHistoryError authorizes the
				// ring; a degraded pre-refresh round records the synthetic reason and
				// waits for the next refresh instead.
				if retryRingAuthorized(retryError) {
					s.scheduleWaitingHistoryRetry(row.ID, plan)
				}
			} else {
				s.clearWaitingTimer(row.ID, false)
				if updated.Status == StatusPlanDiscarded {
					s.logInfo("Herdsman status event plan discarded after max attempts",
						map[string]any{
							"planId":           row.ID,
							"agentId":          row.AgentID,
							"attempts":         updated.Attempts,
							"from":             row.FromStatus,
							"to":               row.ToStatus,
							"herdrSessionName": row.HerdrSessionName,
						})
					return s.appendPlanDiscardedEvent(appendPlanOutcomeInput{
						Agent:           plan.Agent,
						Attempts:        updated.Attempts,
						CompactHistory:  plan.CompactHistory,
						From:            row.FromStatus,
						PlanCreatedAtMs: row.CreatedAtMs,
						PlanID:          row.ID,
						Reason:          updated.LastError,
						To:              row.ToStatus,
					}), nil
				}
				if updated.Status == StatusPlanFailed {
					s.logWarn("Herdsman status event plan failed after max attempts",
						map[string]any{
							"planId":           row.ID,
							"agentId":          row.AgentID,
							"attempts":         updated.Attempts,
							"from":             row.FromStatus,
							"to":               row.ToStatus,
							"herdrSessionName": row.HerdrSessionName,
						})
					return s.appendPlanFailedEvent(appendPlanOutcomeInput{
						Agent:           plan.Agent,
						Attempts:        updated.Attempts,
						CompactHistory:  plan.CompactHistory,
						From:            row.FromStatus,
						PlanCreatedAtMs: row.CreatedAtMs,
						PlanID:          row.ID,
						Reason:          updated.LastError,
						To:              row.ToStatus,
					}), nil
				}
			}
			return nil, nil
		}
		activePlan.CompactHistory = &refreshed.Snapshot.CompactHistory
	}

	event, cancelled, appendErr := s.appendStatusEvents(activePlan)
	if appendErr != nil {
		var waiting *PlanWaitingHistoryError
		if errors.As(appendErr, &waiting) {
			var retryError error
			if row.LastError != nil && *row.LastError == DegradedPlanError {
				retryError = errors.New(DegradedPlanError)
			} else {
				retryError = appendErr
			}
			updated, retryErr := s.markPlanRetry(
				row.ID, plan, retryError, activePlan.CompactHistory, options)
			if retryErr != nil {
				return nil, retryErr
			}
			if updated == nil {
				s.clearWaitingTimer(row.ID, false)
				return nil, nil
			}
			if updated.Status == StatusPlanPending {
				// Invariant: only a genuine PlanWaitingHistoryError authorizes the
				// ring. A row that already carries the synthetic `degraded` reason is
				// recorded as degraded again and waits for the next refresh instead of
				// re-arming the 10s timer, so this catch cannot be used as a back door
				// that lets a degraded round keep the ring alive.
				if retryRingAuthorized(retryError) {
					s.scheduleWaitingHistoryRetry(row.ID, plan)
				}
			} else {
				s.clearWaitingTimer(row.ID, false)
				if updated.Status == StatusPlanDiscarded {
					s.logInfo("Herdsman status event plan discarded after max attempts",
						map[string]any{
							"planId":           row.ID,
							"agentId":          row.AgentID,
							"attempts":         updated.Attempts,
							"from":             row.FromStatus,
							"to":               row.ToStatus,
							"herdrSessionName": row.HerdrSessionName,
						})
					return s.appendPlanDiscardedEvent(appendPlanOutcomeInput{
						Agent:           plan.Agent,
						Attempts:        updated.Attempts,
						CompactHistory:  activePlan.CompactHistory,
						From:            row.FromStatus,
						PlanCreatedAtMs: row.CreatedAtMs,
						PlanID:          row.ID,
						Reason:          updated.LastError,
						To:              row.ToStatus,
					}), nil
				}
				if updated.Status == StatusPlanFailed {
					s.logWarn("Herdsman status event plan failed after max attempts",
						map[string]any{
							"planId":           row.ID,
							"agentId":          row.AgentID,
							"attempts":         updated.Attempts,
							"from":             row.FromStatus,
							"to":               row.ToStatus,
							"herdrSessionName": row.HerdrSessionName,
						})
					return s.appendPlanFailedEvent(appendPlanOutcomeInput{
						Agent:           plan.Agent,
						Attempts:        updated.Attempts,
						CompactHistory:  activePlan.CompactHistory,
						From:            row.FromStatus,
						PlanCreatedAtMs: row.CreatedAtMs,
						PlanID:          row.ID,
						Reason:          updated.LastError,
						To:              row.ToStatus,
					}), nil
				}
			}
			return nil, nil
		}
		// Generic failure path.
		paneClosed, closedErr := s.stores.Agents.IsPaneClosed(IsPaneClosedInput{
			HerdrSessionName: plan.Agent.HerdrSessionName,
			PaneID:           plan.Agent.PaneID,
			PaneGeneration:   plan.Agent.PaneGeneration,
		})
		if closedErr != nil {
			return nil, closedErr
		}
		isAborted := isAbortError(appendErr)
		if paneClosed || isAborted {
			s.clearWaitingTimer(row.ID, false)
			// Same salvage as the other mid-execution pane-close exits above.
			salvaged := s.salvageOrCancelMissingAgentPlan(row)
			s.publishSalvagedEvent(salvaged)
			return nil, nil
		}
		s.clearWaitingTimer(row.ID, true)
		var retryError error
		if row.LastError != nil && *row.LastError == DegradedPlanError {
			retryError = errors.New(DegradedPlanError)
		} else {
			retryError = appendErr
		}
		updated, retryErr := s.markPlanRetry(
			row.ID, plan, retryError, activePlan.CompactHistory, options)
		if retryErr != nil {
			return nil, retryErr
		}
		if updated == nil {
			s.clearWaitingTimer(row.ID, false)
			return nil, nil
		}
		// Invariant: only a genuine PlanWaitingHistoryError re-arms the retry ring,
		// and that case was handled above. On this generic path `retryError` is
		// either the synthetic `degraded` reason or an unrelated failure, so no spin
		// is scheduled: a still-pending row is drained by the next refresh.
		if updated.Status == StatusPlanDiscarded {
			s.clearWaitingTimer(row.ID, false)
			s.logInfo("Herdsman status event plan discarded after max attempts", map[string]any{
				"planId":           row.ID,
				"agentId":          row.AgentID,
				"attempts":         updated.Attempts,
				"from":             row.FromStatus,
				"to":               row.ToStatus,
				"herdrSessionName": row.HerdrSessionName,
			})
			return s.appendPlanDiscardedEvent(appendPlanOutcomeInput{
				Agent:           plan.Agent,
				Attempts:        updated.Attempts,
				CompactHistory:  activePlan.CompactHistory,
				From:            row.FromStatus,
				PlanCreatedAtMs: row.CreatedAtMs,
				PlanID:          row.ID,
				Reason:          updated.LastError,
				To:              row.ToStatus,
			}), nil
		}
		if updated.Status == StatusPlanFailed {
			s.clearWaitingTimer(row.ID, false)
			s.logWarn("Herdsman status event plan failed after max attempts", map[string]any{
				"planId":           row.ID,
				"agentId":          row.AgentID,
				"attempts":         updated.Attempts,
				"from":             row.FromStatus,
				"to":               row.ToStatus,
				"herdrSessionName": row.HerdrSessionName,
			})
			return s.appendPlanFailedEvent(appendPlanOutcomeInput{
				Agent:           plan.Agent,
				Attempts:        updated.Attempts,
				CompactHistory:  activePlan.CompactHistory,
				From:            row.FromStatus,
				PlanCreatedAtMs: row.CreatedAtMs,
				PlanID:          row.ID,
				Reason:          updated.LastError,
				To:              row.ToStatus,
			}), nil
		}
		return nil, appendErr
	}
	if cancelled {
		s.clearWaitingTimer(row.ID, false)
		// The append-time guard refused the plan because the agent row is gone
		// (pane closed mid-execution — the common case: the orchestrator's
		// "finish and close" lands in the TURN_SIGNAL_WAIT_MS window). That is the
		// same content-loss the drain-time salvage fixes, so it settles through the
		// same path; the published result keeps waking the orchestrator exactly as
		// a normal terminal event would.
		salvaged := s.salvageOrCancelMissingAgentPlan(row)
		s.publishSalvagedEvent(salvaged)
		return nil, nil
	}
	if event == nil {
		paneClosed, closedErr := s.stores.Agents.IsPaneClosed(IsPaneClosedInput{
			HerdrSessionName: plan.Agent.HerdrSessionName,
			PaneID:           plan.Agent.PaneID,
			PaneGeneration:   plan.Agent.PaneGeneration,
		})
		if closedErr != nil {
			return nil, closedErr
		}
		if paneClosed {
			s.clearWaitingTimer(row.ID, false)
			// Execution-time pane close (abort/early return before the append round):
			// same salvage as the drain-time missing-agent path instead of a blind
			// cancel, so content on disk is not lost with the plan.
			salvaged := s.salvageOrCancelMissingAgentPlan(row)
			s.publishSalvagedEvent(salvaged)
			return nil, nil
		}
	}
	eventPayload := map[string]any{}
	if event != nil {
		eventPayload = record(event.Payload)
	}
	isDegraded := event != nil && eventPayload["degraded"] == true
	if isDegraded {
		if _, err := s.stores.AgentEvents.InvalidateByID(event.ID, "degraded_retry"); err != nil {
			return nil, err
		}
		invalidated := AgentEventStatusInvalidated
		event.Status = &invalidated
		event.Deliverable = 0
		s.clearWaitingTimer(row.ID, true)
		degradedError := errors.New(DegradedPlanError)
		updated, retryErr := s.markPlanRetry(
			row.ID, plan, degradedError, activePlan.CompactHistory, options)
		if retryErr != nil {
			return nil, retryErr
		}
		if updated == nil {
			s.clearWaitingTimer(row.ID, false)
			return nil, nil
		}
		if updated.Status == StatusPlanPending {
			// Invariant: `degraded` is a synthetic reason, so it never authorizes
			// the retry ring; the row stays pending and the next refresh re-drains it.
			if retryRingAuthorized(degradedError) {
				s.scheduleWaitingHistoryRetry(row.ID, plan)
			}
			return nil, nil
		}
		s.clearWaitingTimer(row.ID, false)
		if updated.Status == StatusPlanDiscarded {
			return s.appendPlanDiscardedEvent(appendPlanOutcomeInput{
				Agent:           plan.Agent,
				Attempts:        updated.Attempts,
				CompactHistory:  activePlan.CompactHistory,
				From:            row.FromStatus,
				PlanCreatedAtMs: row.CreatedAtMs,
				PlanID:          row.ID,
				Reason:          updated.LastError,
				To:              row.ToStatus,
			}), nil
		}
		if updated.Status == StatusPlanFailed {
			return s.appendPlanFailedEvent(appendPlanOutcomeInput{
				Agent:           plan.Agent,
				Attempts:        updated.Attempts,
				CompactHistory:  activePlan.CompactHistory,
				From:            row.FromStatus,
				PlanCreatedAtMs: row.CreatedAtMs,
				PlanID:          row.ID,
				Reason:          updated.LastError,
				To:              row.ToStatus,
			}), nil
		}
		return nil, nil
	}
	s.clearWaitingTimer(row.ID, false)
	if err := store.MarkCompleted(row.ID); err != nil {
		return nil, err
	}
	return event, nil
}

func isAbortError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return errors.Is(err, context.Canceled) ||
		containsFold(message, "aborted") ||
		containsFold(message, "aborterror")
}

// containsFold mirrors the TS `.includes(...)` on a message: case-insensitive,
// so `AbortError` and `aborted` both match.
func containsFold(haystack string, needleLower string) bool {
	return strings.Contains(strings.ToLower(haystack), needleLower)
}

// waitForHistoryAdvance mirrors #waitForHistoryAdvance.
func (s *AgentIndexService) waitForHistoryAdvance(
	input waitForHistoryAdvanceInput,
) (*CompactAgentHistory, error) {
	maxAttempts := input.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 8
	}
	// Fixed 200ms interval, no exponential backoff: the history cache either
	// advances shortly after the turn signal or it does not, and a doubling delay
	// only pushed the first deliverable terminal event tens of seconds away. The
	// whole window is hard bounded by maxTotalMs.
	baseDelay := int64(200)
	maxTotalMs := int64(1500)
	var refreshed *CompactAgentHistory
	if input.Initial != nil {
		refreshed = input.Initial
	} else {
		result, err := s.context.RefreshAgent(RefreshAgentContextInput{
			Agent:           input.Agent,
			ForceRefresh:    boolPointer(true),
			IdentityChanged: false,
		})
		if err != nil {
			return nil, err
		}
		refreshed = &result.Snapshot.CompactHistory
	}
	start := s.now()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		advanced := HistoryHasAdvanced(*refreshed, input.Baseline, &HistoryHasAdvancedOptions{
			RequireAssistantChange: input.RequireAssistantChange,
		})
		if advanced || input.Controller.Aborted() || s.now()-start >= maxTotalMs {
			break
		}
		// Clamp the last step to the remaining budget so the total wait never
		// exceeds maxTotalMs even though the loop condition is checked before
		// sleeping.
		remaining := maxTotalMs - (s.now() - start)
		if remaining <= 0 {
			break
		}
		delay := baseDelay
		if remaining < delay {
			delay = remaining
		}
		s.sleep(delay, input.Controller.Done())
		if input.Controller.Aborted() {
			fmt.Fprintf(os.Stderr,
				"Herdsman retrying status event plan because wait was aborted during"+
					" history refresh (agentId=%s, herdrSessionName=%s, paneId=%s)\n",
				input.Agent.ID, input.Agent.HerdrSessionName, input.Agent.PaneID)
			return nil, &PlanWaitingHistoryError{}
		}
		paneClosed, err := s.stores.Agents.IsPaneClosed(IsPaneClosedInput{
			HerdrSessionName: input.Agent.HerdrSessionName,
			PaneID:           input.Agent.PaneID,
			PaneGeneration:   input.Agent.PaneGeneration,
		})
		if err != nil {
			return nil, err
		}
		if paneClosed {
			fmt.Fprintf(os.Stderr,
				"Herdsman skipping status event generation because pane closed during"+
					" history refresh (agentId=%s, herdrSessionName=%s, paneId=%s)\n",
				input.Agent.ID, input.Agent.HerdrSessionName, input.Agent.PaneID)
			return nil, nil
		}
		result, err := s.context.RefreshAgent(RefreshAgentContextInput{
			Agent:           input.Agent,
			ForceRefresh:    boolPointer(true),
			IdentityChanged: false,
		})
		if err != nil {
			return nil, err
		}
		refreshed = &result.Snapshot.CompactHistory
	}
	return refreshed, nil
}

type waitForHistoryAdvanceInput struct {
	Agent                  AgentIndexRecord
	Baseline               *CompactAgentHistory
	Controller             *waiterController
	Initial                *CompactAgentHistory
	MaxAttempts            int
	RequireAssistantChange bool
}

// resolvePlanOutcomeAgent mirrors #resolvePlanOutcomeAgent.
func (s *AgentIndexService) resolvePlanOutcomeAgent(
	agent AgentIndexRecord,
	planCreatedAtMs *int64,
) planOutcomeResolution {
	if live, err := s.stores.Agents.Get(agent.ID); err == nil {
		return planOutcomeResolution{agent: live, agentRowPresent: true}
	}
	createdBefore := s.now()
	if planCreatedAtMs != nil {
		createdBefore = *planCreatedAtMs
	}
	stitched := s.minimalAgentFromLatestEvent(minimalAgentInput{
		AgentID: agent.ID,
		// The plan row's own creation time is the upper bound: anything newer on
		// the same pane belongs to a later instance.
		CreatedBeforeMs:  createdBefore,
		HerdrSessionName: agent.HerdrSessionName,
		PaneGeneration:   agent.PaneGeneration,
		PaneID:           agent.PaneID,
	})
	if stitched == nil {
		return planOutcomeResolution{agent: agent, agentRowPresent: false}
	}
	return planOutcomeResolution{agent: *stitched, agentRowPresent: false}
}

// appendPlanOutcomeInput mirrors the TS input of #appendPlanFailedEvent /
// #appendPlanDiscardedEvent.
type appendPlanOutcomeInput struct {
	Agent           AgentIndexRecord
	Attempts        int
	CompactHistory  *CompactAgentHistory
	FallbackOutcome *bool
	From            AgentStatus
	PlanCreatedAtMs int64
	PlanID          int64
	Reason          *string
	To              AgentStatus
}

// appendPlanFailedEvent mirrors #appendPlanFailedEvent.
func (s *AgentIndexService) appendPlanFailedEvent(
	input appendPlanOutcomeInput,
) *AgentEventRecord {
	// Phase 2 item 1 (spec: `.agents/notes/20260930-phase1-delivery-latency.md`
	// lines 193 and 204-205). A round that settles with no assistant content at
	// all must not be recorded as `agent.failed`: "the agent failed" is a claim
	// this daemon cannot support when the round produced no assistant text, and
	// the row it would write has an empty body. The plan row itself has already
	// settled as `failed` — that is a plan-settlement state, not a verdict about
	// the agent — and the empty round stays observable through the per-pane
	// `upstream_empty_turn` alert (#noteEmptyTurnAlert).
	if input.Reason != nil && *input.Reason == DegradedPlanError &&
		!HasNonEmptyAssistantMessage(input.CompactHistory) {
		// The empty round is still observable: the alert is the replacement for
		// the fallback wake this row used to carry, and the per-pane suppression
		// keeps it to one line per empty streak.
		s.noteEmptyTurnAlert(noteEmptyTurnAlertInput{
			AgentID:          input.Agent.ID,
			Attempts:         input.Attempts,
			HerdrSessionName: input.Agent.HerdrSessionName,
			PaneID:           &input.Agent.PaneID,
			PlanID:           input.PlanID,
		})
		return nil
	}
	originalAgentID := input.Agent.ID
	resolved := s.resolvePlanOutcomeAgent(input.Agent, &input.PlanCreatedAtMs)
	agent := resolved.agent
	fallbackOutcome := input.FallbackOutcome
	if fallbackOutcome == nil {
		defaultFallback := (input.To == AgentStatusDone || input.To == AgentStatusBlocked) &&
			input.Reason != nil && *input.Reason == DegradedPlanError
		fallbackOutcome = &defaultFallback
	}
	reason := "unknown"
	if input.Reason != nil {
		reason = *input.Reason
	}
	return s.appendAndAckSelfEvent(AppendAgentEventInput{
		AgentID:          agentIDOrNil(resolved.agentRowPresent, agent.ID),
		CompactHistory:   input.CompactHistory,
		HerdrSessionName: agent.HerdrSessionName,
		IdempotencyKey:   stringPointer(fmt.Sprintf("agent.failed:plan:%d", input.PlanID)),
		PaneID:           stringPointer(agent.PaneID),
		PaneGeneration:   agent.PaneGeneration,
		Payload: map[string]any{
			"agent": agent.Agent,
			// The original id survives in the payload even when the row had to be
			// written as an orphan, so the failure stays traceable to its agent.
			"agentId":          originalAgentID,
			"attempts":         input.Attempts,
			"fallbackOutcome":  *fallbackOutcome,
			"from":             string(input.From),
			"herdrSessionName": agent.HerdrSessionName,
			"name":             agent.Name,
			"paneId":           agent.PaneID,
			"reason":           reason,
			"terminalId":       agent.TerminalID,
			"to":               string(input.To),
			"workspaceId":      agent.WorkspaceID,
			"interactive":      IsInteractivePiAgent(&agent),
		},
		TerminalID:  agent.TerminalID,
		Type:        AgentEventFailed,
		WorkspaceID: stringPointer(agent.WorkspaceID),
	})
}

// appendPlanDiscardedEvent mirrors #appendPlanDiscardedEvent.
func (s *AgentIndexService) appendPlanDiscardedEvent(
	input appendPlanOutcomeInput,
) *AgentEventRecord {
	originalAgentID := input.Agent.ID
	resolved := s.resolvePlanOutcomeAgent(input.Agent, &input.PlanCreatedAtMs)
	agent := resolved.agent
	// A discard is a *lost round* for every one of these reasons: the plan never
	// observed a deliverable terminal body, so the pane's answer (if it ever
	// lands) still has to reach the orchestrator. `PLAN_STALLED` joined the set
	// with the jitter fix: the stall fallback settles the plan exactly like the
	// elastic-window timeout, and without the marker the late-completion replay
	// stayed disarmed and the agy pane went silent.
	reason := ""
	if input.Reason != nil {
		reason = *input.Reason
	}
	fallbackOutcome := false
	for _, stalled := range StalledPlanErrors {
		if reason == stalled {
			fallbackOutcome = true
			break
		}
	}
	event := s.appendAndAckSelfEvent(AppendAgentEventInput{
		AgentID:          agentIDOrNil(resolved.agentRowPresent, agent.ID),
		CompactHistory:   input.CompactHistory,
		HerdrSessionName: agent.HerdrSessionName,
		IdempotencyKey:   stringPointer(fmt.Sprintf("agent.discarded:plan:%d", input.PlanID)),
		PaneID:           stringPointer(agent.PaneID),
		PaneGeneration:   agent.PaneGeneration,
		Payload: map[string]any{
			"agent":            agent.Agent,
			"agentId":          originalAgentID,
			"attempts":         input.Attempts,
			"fallbackOutcome":  fallbackOutcome,
			"from":             string(input.From),
			"herdrSessionName": agent.HerdrSessionName,
			"name":             agent.Name,
			"paneId":           agent.PaneID,
			"planId":           input.PlanID,
			"reason":           reason,
			"terminalId":       agent.TerminalID,
			"to":               string(input.To),
			"workspaceId":      agent.WorkspaceID,
			"interactive":      IsInteractivePiAgent(&agent),
		},
		TerminalID:  agent.TerminalID,
		Type:        AgentEventDiscarded,
		WorkspaceID: stringPointer(agent.WorkspaceID),
	})
	fmt.Fprintf(os.Stderr,
		"Herdsman plan discarded event recorded and published (agentId=%s, eventId=%d,"+
			" fallbackOutcome=%t, paneId=%s, planId=%d, reason=%s)\n",
		agent.ID, event.ID, fallbackOutcome, agent.PaneID, input.PlanID, reason)
	return event
}

func agentIDOrNil(present bool, agentID string) *string {
	if !present {
		return nil
	}
	return &agentID
}

// appendAndAckSelfEvent mirrors #appendAndAckSelfEvent.
func (s *AgentIndexService) appendAndAckSelfEvent(
	input AppendAgentEventInput,
) *AgentEventRecord {
	// Phase 2 item 4: a round that appends a body ends the pane's empty streak.
	// Dropping the key here is what lets a later empty streak alert again, and
	// keeps the suppression from lasting for the life of the pane.
	if HasNonEmptyAssistantMessage(input.CompactHistory) {
		key := emptyTurnAlertKey(input.HerdrSessionName, input.PaneID)
		s.mu.Lock()
		delete(s.emptyTurnAlertPanes, key)
		s.mu.Unlock()
	}
	event, err := s.stores.AgentEvents.Append(input)
	if err != nil {
		// The TS append is the only writer here and its failure propagates; the Go
		// port keeps that contract by reporting it through the plan's error path.
		fmt.Fprintf(os.Stderr, "Herdsman failed to append agent event (error=%v)\n", err)
		return nil
	}
	if s.stores.AgentOrchestratorScopes != nil {
		workspaceID := ""
		if input.WorkspaceID != nil {
			workspaceID = *input.WorkspaceID
		}
		owner, ownerErr := s.stores.AgentOrchestratorScopes.Get(AgentScope{
			HerdrSessionName: input.HerdrSessionName,
			WorkspaceID:      workspaceID,
		})
		if ownerErr == nil && owner != nil && owner.Owner != nil && owner.Owner.PaneID != "" {
			paneID := ""
			if input.PaneID != nil {
				paneID = *input.PaneID
			}
			if owner.Owner.PaneID == paneID {
				if err := s.stores.AgentEvents.MarkAcked(event.ID, nil); err != nil {
					fmt.Fprintf(os.Stderr,
						"Herdsman failed to ack self-owned agent event (error=%v)\n", err)
					return &event
				}
				acked, err := s.stores.AgentEvents.Get(event.ID)
				if err != nil {
					return &event
				}
				return &acked
			}
		}
	}
	return &event
}

// isAgentStalled mirrors #isAgentStalled.
func (s *AgentIndexService) isAgentStalled(agentID string, herdrSessionName string) bool {
	latestStalledDiscard, err := s.stores.StatusEventPlans.LatestStalledDiscard(
		agentID, herdrSessionName)
	if err != nil || latestStalledDiscard == nil {
		return false
	}
	laterCompletedPlan, err := s.stores.StatusEventPlans.HasCompletedPlanAfter(
		agentID, herdrSessionName, latestStalledDiscard.ID)
	if err == nil && laterCompletedPlan {
		return false
	}
	latestCompleted, err := s.stores.AgentEvents.LatestCompletedTurnEvent(
		agentID, herdrSessionName)
	if err == nil && latestCompleted != nil {
		discardEventID, found, eventErr := s.stores.AgentEvents.EventIDByIdempotencyKey(
			fmt.Sprintf("agent.discarded:plan:%d", latestStalledDiscard.ID))
		if eventErr == nil && found {
			// A completion produced after the stall discard — the replay itself or a
			// later round's own terminal event — means the lost round was settled.
			if latestCompleted.ID > discardEventID {
				return false
			}
		} else if parseEventTimeMs(latestCompleted.CreatedAt) >= latestStalledDiscard.UpdatedAtMs {
			// The discard event row is gone (pruned) but a completion landed after
			// the plan was settled: same conclusion, timestamps as the fallback
			// evidence.
			return false
		}
	}
	return true
}

// reconcileLateCompletion mirrors #reconcileLateCompletion.
func (s *AgentIndexService) reconcileLateCompletion(
	agent AgentIndexRecord,
	compactHistory *CompactAgentHistory,
) *AgentEventRecord {
	// A late answer may only be replayed for a pane that has *settled*. While the
	// pane reports `working` it is in the middle of a new round: replaying the
	// intermediate assistant body written so far would emit a fake
	// `working -> idle` completion (`lateCompletion: true`) in the middle of that
	// round, and the real completion would then look like a duplicate.
	if agent.AgentStatus != AgentStatusIdle && agent.AgentStatus != AgentStatusDone {
		return nil
	}
	if compactHistory == nil || !HasNonEmptyAssistantMessage(compactHistory) {
		return nil
	}
	if !s.isAgentStalled(agent.ID, agent.HerdrSessionName) {
		return nil
	}
	currentRef := ""
	if compactHistory.LastAssistantMessage != nil {
		currentRef = compactHistory.LastAssistantMessage.Ref
	}
	if currentRef == "" {
		return nil
	}
	latestCompletedRef := ""
	if event, err := s.stores.AgentEvents.LatestCompletedTurnEvent(
		agent.ID, agent.HerdrSessionName,
	); err == nil && event != nil && event.CompactHistory != nil &&
		event.CompactHistory.LastAssistantMessage != nil {
		latestCompletedRef = event.CompactHistory.LastAssistantMessage.Ref
	}
	if currentRef == latestCompletedRef {
		return nil
	}

	targetStatus := AgentStatusIdle
	if agent.AgentStatus == AgentStatusDone {
		targetStatus = AgentStatusDone
	}
	statusType := statusEventType(targetStatus)
	if statusType == nil {
		idle := AgentEventIdle
		statusType = &idle
	}
	eventPayload := payload(agent, AgentStatusWorking, targetStatus)
	// `from` is pinned to `working` even when the lost plan started at `unknown`:
	// the replay has to look like an ordinary completed round to
	// `latestCompletedTurnEvent` (which only accepts `agent.idle` rows from
	// `working`) and to the orchestrator-side projector.
	eventPayload["from"] = string(AgentStatusWorking)
	eventPayload["lateCompletion"] = true
	event := s.appendAndAckSelfEvent(AppendAgentEventInput{
		AgentID:          &agent.ID,
		CompactHistory:   compactHistory,
		HerdrSessionName: agent.HerdrSessionName,
		IdempotencyKey: stringPointer(
			fmt.Sprintf("agent.late_completed:%s:%s", agent.ID, currentRef)),
		PaneID:         stringPointer(agent.PaneID),
		PaneGeneration: agent.PaneGeneration,
		Payload:        eventPayload,
		TerminalID:     agent.TerminalID,
		Type:           *statusType,
		WorkspaceID:    stringPointer(agent.WorkspaceID),
	})
	if event == nil {
		return nil
	}
	s.logInfo("Herdsman emitted late completed turn event for stalled agent", map[string]any{
		"agentId":          agent.ID,
		"eventId":          event.ID,
		"herdrSessionName": agent.HerdrSessionName,
		"paneId":           agent.PaneID,
		"ref":              currentRef,
		"type":             string(*statusType),
	})
	// Publication stays with the caller, exactly as in `executeStatusEventPlan`:
	// all three call sites put this event into the `events` array they return.
	// `publishAgentEvent` has no id-level dedupe, so pushing here as well wrote the
	// same `agent.event` twice on the socket and broke the exactly-once delivery
	// contract.
	return event
}

// ---------------------------------------------------------------------------
// Observability notes
// ---------------------------------------------------------------------------

type noteEmptyTurnAlertInput struct {
	AgentID          string
	Attempts         int
	HerdrSessionName string
	PaneID           *string
	PlanID           int64
}

// noteEmptyTurnAlert mirrors #noteEmptyTurnAlert.
func (s *AgentIndexService) noteEmptyTurnAlert(input noteEmptyTurnAlertInput) {
	key := emptyTurnAlertKey(input.HerdrSessionName, input.PaneID)
	s.mu.Lock()
	if _, ok := s.emptyTurnAlertPanes[key]; ok {
		s.mu.Unlock()
		return
	}
	s.emptyTurnAlertPanes[key] = struct{}{}
	s.mu.Unlock()
	s.logWarn(EmptyTurnAlertMessage, map[string]any{
		"agentId":          input.AgentID,
		"attempts":         input.Attempts,
		"herdrSessionName": input.HerdrSessionName,
		"paneId":           input.PaneID,
		"planId":           input.PlanID,
		"reason":           EmptyTurnAlertReason,
	})
}

type noteWindowSaturatedInput struct {
	AgentID          string
	HerdrSessionName string
	PaneID           string
	Scanned          int
}

// noteGuardWindowSaturated mirrors #noteGuardWindowSaturated.
func (s *AgentIndexService) noteGuardWindowSaturated(input noteWindowSaturatedInput) {
	key := input.AgentID + "\x00" + input.HerdrSessionName
	s.mu.Lock()
	if _, ok := s.guardWindowSaturatedAgents[key]; ok {
		s.mu.Unlock()
		return
	}
	s.guardWindowSaturatedAgents[key] = struct{}{}
	s.mu.Unlock()
	s.logWarn("Herdsman stale-duplicate guard window is saturated", map[string]any{
		"agentId":          input.AgentID,
		"herdrSessionName": input.HerdrSessionName,
		"limit":            STALE_DUPLICATE_GUARD_SCAN_LIMIT,
		"paneId":           input.PaneID,
		"scanned":          input.Scanned,
	})
}

// noteSalvageWindowSaturated mirrors #noteSalvageWindowSaturated.
func (s *AgentIndexService) noteSalvageWindowSaturated(input noteWindowSaturatedInput) {
	key := input.HerdrSessionName + "\x00" + input.PaneID
	s.mu.Lock()
	if _, ok := s.salvageWindowSaturatedKeys[key]; ok {
		s.mu.Unlock()
		return
	}
	s.salvageWindowSaturatedKeys[key] = struct{}{}
	s.mu.Unlock()
	s.logWarn("Herdsman salvage window is saturated", map[string]any{
		"herdrSessionName": input.HerdrSessionName,
		"limit":            STALE_DUPLICATE_GUARD_SCAN_LIMIT,
		"paneId":           input.PaneID,
		"scanned":          input.Scanned,
	})
}

// logDegradedRelease mirrors #logDegradedRelease.
func (s *AgentIndexService) logDegradedRelease(
	plan StatusEventPlan,
	degradedReason DegradedReason,
) {
	attempts := 0
	if plan.Attempts != nil {
		attempts = *plan.Attempts
	}
	var planID any
	if plan.PlanID != nil {
		planID = *plan.PlanID
	}
	s.logWarn(DegradedEventMessage, map[string]any{
		"agentId":          plan.Agent.ID,
		"attempts":         attempts,
		"degradedReason":   string(degradedReason),
		"herdrSessionName": plan.Agent.HerdrSessionName,
		"paneId":           plan.Agent.PaneID,
		"planId":           planID,
	})
	s.noteEmptyTurnAlert(noteEmptyTurnAlertInput{
		AgentID:          plan.Agent.ID,
		Attempts:         attempts,
		HerdrSessionName: plan.Agent.HerdrSessionName,
		PaneID:           &plan.Agent.PaneID,
		PlanID:           planIDValue(plan),
	})
}

func planIDValue(plan StatusEventPlan) int64 {
	if plan.PlanID == nil {
		return 0
	}
	return *plan.PlanID
}
