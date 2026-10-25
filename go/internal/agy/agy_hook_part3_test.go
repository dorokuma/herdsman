package agy_test

// This file ports lines 1130-1881 of test/unit/agy-hook-and-cli.test.ts: the
// settleDeadline/transient-RPC case, the plan-mode suppression cases, the
// deferred intermediate-completion case and the Stop-with-decision:continue
// waiter case. The cases that follow (standby waiter, wake-waiter cwd, the two
// ObservabilityRpcServer cases) live in agy_hook_part4_test.go.
//
// The TS cases run the WakeWaiter loop under vi fake timers, advancing the
// clock past settleDeadline, a wait timeout and a transient RPC error. The Go
// waiter is a synchronous real-time loop, so each round drives it in a
// goroutine and either waits for a concrete state change (a first agent.list
// call) or asserts only the outcomes that are deterministic regardless of
// scheduling — the final output, the persisted state and the bounded number of
// polls, which is what proves the loop does not spin at 0ms.

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/agy"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// planWaitingHistoryReason / planStalledReason mirror the TS constants
// PLAN_WAITING_HISTORY / PLAN_STALLED.
const (
	planWaitingHistoryReason = "PLAN_WAITING_HISTORY"
	planStalledReason        = "PLAN_STALLED"
)

// errTransientDaemonBusy mirrors the TS `throw new Error("transient daemon busy")`.
var errTransientDaemonBusy = errors.New("transient daemon busy")

// agentListEntry is one AgentListItem of the harness agent.list answer.
func agentListEntry(kind, status, firstSeenAt, paneID, terminalID string) map[string]any {
	return map[string]any{
		"agent":            kind,
		"agentStatus":      status,
		"firstSeenAt":      firstSeenAt,
		"herdrSessionName": "default",
		"id":               "ag_" + terminalID,
		"paneId":           paneID,
		"terminalId":       terminalID,
		"workspaceId":      "wB",
	}
}

// orchestratorStateResponse is the default `agent.orchestrator.*` answer the
// fixtures return: this terminal owns wB:p1 and the events/ack cursor are the
// mutable test values.
func orchestratorStateResponse(sessionID string, events []any, ackedEventID int64) map[string]any {
	return map[string]any{
		"events": events,
		"presence": map[string]any{
			"herdrSessionName": "default",
			"paneId":           "wB:p1",
			"subscriberId":     "agy:" + sessionID,
			"terminalId":       "term_self",
			"workspaceId":      "wB",
		},
		"state": map[string]any{
			"ackedEventId":     ackedEventID,
			"herdrSessionName": "default",
			"owner":            map[string]any{"paneId": "wB:p1", "terminalId": "term_self"},
			"updatedAt":        "2026-10-21T12:00:00.000Z",
			"workspaceId":      "wB",
		},
	}
}

// eventWith builds a typed event with an explicit terminal id and payload,
// mirroring `{ ...makeDoneEvent(id, "", terminalId), type, payload }`.
func eventWith(
	id int64,
	terminalID string,
	eventType observability.AgentEventType,
	payload map[string]any,
) observability.AgentEventRecord {
	event := makeDoneEvent(id, "")
	event.TerminalID = strPtr(terminalID)
	event.Type = eventType
	event.Payload = payload
	return event
}

// injectedMessage mirrors `out.injectSteps?.[0]?.ephemeralMessage ?? ""`.
func injectedMessage(output agy.HookOutput) string {
	if len(output.InjectSteps) == 0 {
		return ""
	}
	return output.InjectSteps[0].EphemeralMessage
}

// agentStatusPtr is the `&status` needed by the AgentListItemView fields.
func agentStatusPtr(value observability.AgentStatus) *observability.AgentStatus { return &value }

// waitUntil polls a condition (the Go stand-in for vi.waitFor/advanceTimers).
func waitUntil(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition was not met in time")
}

// --- F1 regression / peer, subagent and orphan edge cases (lines 1130-1422) --

func TestWakeWaiterResetsSettleDeadlineAndValidatesEdgeCases(t *testing.T) {
	dir := t.TempDir()
	sessionID := "88888888-2222-3333-4444-555555555555"

	agentListCalls := 0
	warns := []string{}

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.list" {
			agentListCalls++
			// Tick 1 sees the idle main pane; from tick 2 on selfAgentStatus is
			// "working" and the second tick is the one transient RPC error. The
			// fake-timer case arranged the same two facts by hand.
			selfStatus := "done"
			if agentListCalls >= 2 {
				selfStatus = "working"
			}
			if agentListCalls == 2 {
				return nil, errTransientDaemonBusy
			}
			return map[string]any{"agents": []any{
				agentListEntry("agy", selfStatus, "2026-10-22T11:50:00.000Z", "wB:p1", "term_self"),
				agentListEntry("pi", "done", "2026-10-22T11:50:00.000Z", "wB:p2", "term_peer"),
			}}, nil
		}
		return orchestratorStateResponse(sessionID, []any{}, 0), nil
	}

	// 1. F1 regression: a stale settleDeadline must not turn the loop into a
	// 0ms busy loop once selfAgentStatus becomes "working"; the finally block
	// still clears waiterPid when the wait timeout fires.
	writeStateFile(t, sessionStatePath(dir, sessionID), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})
	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connectCapturing,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(100),
			SettleGraceMs:  floatPtr(200),
			SocketPath:     "/tmp/herdsman.sock",
			StateDir:       strPtr(dir),
			WaitTimeoutMs:  floatPtr(550),
			Warn: func(message string) {
				client.mu.Lock()
				warns = append(warns, message)
				client.mu.Unlock()
			},
		})
	}()

	waitUntil(t, 5*time.Second, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return agentListCalls >= 1
	})

	if result := awaitHookOutput(t, done); !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	client.mu.Lock()
	calls := agentListCalls
	warnText := strings.Join(warns, "\n")
	client.mu.Unlock()
	if !strings.Contains(warnText, "Herdsman agy WakeWaiter transient RPC error (attempt 1/3)") {
		t.Fatalf("warns = %q, want the attempt 1/3 transient error", warnText)
	}
	// A 0ms busy loop produces thousands of polls inside the 550ms budget; the
	// TS case bounds it at 6 and the real-time loop lands on the same number.
	if calls > 6 {
		t.Fatalf("agentListCalls = %d, want <= 6 (0ms busy loop)", calls)
	}
	if _, ok := readStateObject(t, sessionStatePath(dir, sessionID))["waiterPid"]; ok {
		t.Fatal("waiterPid was not cleaned up after the wait timeout")
	}

	// 2. A subagent (CTXMODE_ROLE) receiving USER_CANCELED in Stop must not
	// write session state.
	subSessionID := "subagent-user-canceled"
	agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+subSessionID+`","terminationReason":"USER_CANCELED"}`, agy.RunAgyHookOptions{
		Connect:     client.connect,
		Environment: mergeEnv(herdrEnv, "CTXMODE_ROLE", "worker"),
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	if _, err := os.Stat(sessionStatePath(dir, subSessionID)); err == nil {
		t.Fatal("a subagent Stop wrote session state")
	}

	// 3. isPeerActiveOrSettling checks userMs > assistantMs, the ref-step
	// fallback on null timestamps, and stopReason: "toolUse".
	nowMs := float64(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC).UnixMilli())
	basePeer := func() agy.AgentListItemView {
		return agy.AgentListItemView{
			AgentStatus: agentStatusPtr(observability.AgentStatusDone),
			FirstSeenAt: strPtr("2026-10-22T11:55:00.000Z"),
			PaneID:      "wB:p2",
			TerminalID:  strPtr("term_peer"),
		}
	}

	userAfterAssistant := basePeer()
	userAfterAssistant.History = agy.PeerHistory{
		LastAssistantMessage: &agy.PeerHistoryExcerpt{Ref: strPtr("conv.db:3:0"), Timestamp: strPtr("2026-10-22T11:58:00.000Z")},
		LastUserMessage:      &agy.PeerHistoryExcerpt{Ref: strPtr("conv.db:4:0"), Timestamp: strPtr("2026-10-22T11:59:00.000Z")},
	}
	if !agy.IsPeerActiveOrSettling(userAfterAssistant, nowMs, 15_000, 12_000) {
		t.Fatal("isPeerActiveOrSettling(userMs > assistantMs) = false, want true")
	}

	refStepFallback := basePeer()
	refStepFallback.History = agy.PeerHistory{
		LastAssistantMessage: &agy.PeerHistoryExcerpt{Ref: strPtr("conv.db:3:0")},
		LastUserMessage:      &agy.PeerHistoryExcerpt{Ref: strPtr("conv.db:5:0")},
	}
	if !agy.IsPeerActiveOrSettling(refStepFallback, nowMs, 15_000, 12_000) {
		t.Fatal("isPeerActiveOrSettling(ref step fallback) = false, want true")
	}

	toolUse := basePeer()
	toolUse.History = agy.PeerHistory{
		LastAssistantMessage: &agy.PeerHistoryExcerpt{Ref: strPtr("conv.db:5:0"), StopReason: strPtr("toolUse")},
		LastUserMessage:      &agy.PeerHistoryExcerpt{Ref: strPtr("conv.db:5:0")},
	}
	if !agy.IsPeerActiveOrSettling(toolUse, nowMs, 15_000, 12_000) {
		t.Fatal("isPeerActiveOrSettling(stopReason toolUse) = false, want true")
	}

	// 4. isDeliverableAgentEvent allows an orphan deliverable agent.done in both
	// pending and delivered states, and rejects the invalidated row.
	scope := observability.AgentScope{HerdrSessionName: "default", WorkspaceID: "wB"}
	pending := observability.AgentEventStatusPending
	orphanDone := makeDoneEvent(77, "Completed before pane closed.")
	orphanDone.AgentID = nil
	orphanDone.Deliverable = 1
	orphanDone.DeliveredToTerminalID = strPtr("term_self")
	orphanDone.Status = &pending
	if !db.IsDeliverableAgentEvent(orphanDone, nil, scope, "term_self") {
		t.Fatal("pending orphan done was not deliverable")
	}
	delivered := orphanDone
	deliveredStatus := observability.AgentEventStatusDelivered
	delivered.Status = &deliveredStatus
	if !db.IsDeliverableAgentEvent(delivered, nil, scope, "term_self") {
		t.Fatal("delivered orphan done was not deliverable")
	}
	invalidated := orphanDone
	invalidated.Deliverable = 0
	invalidatedStatus := observability.AgentEventStatusInvalidated
	invalidated.Status = &invalidatedStatus
	if db.IsDeliverableAgentEvent(invalidated, nil, scope, "term_self") {
		t.Fatal("invalidated row was deliverable")
	}

	// 5. In plan mode the intermediate PLAN_WAITING_HISTORY failure is
	// suppressed while the genuine failure is surfaced.
	planEvents := []any{
		eventWith(88, "term_peer", observability.AgentEventFailed, map[string]any{
			"agent": "agy", "reason": planWaitingHistoryReason,
		}),
		eventWith(89, "term_peer", observability.AgentEventFailed, map[string]any{
			"agent": "agy", "reason": "typecheck failed: 2 errors",
		}),
	}
	planClient := &hookFakeClient{}
	planClient.respond = func(method string, params any) (any, error) {
		if method == "agent.notifications.ack" {
			return map[string]any{}, nil
		}
		return orchestratorStateResponse(sessionID, planEvents, 0), nil
	}
	planOut := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     connectReturning(planClient),
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	injected := injectedMessage(planOut)
	if strings.Contains(injected, planWaitingHistoryReason) {
		t.Fatalf("injected = %q, want no PLAN_WAITING_HISTORY", injected)
	}
	if !strings.Contains(injected, "typecheck failed: 2 errors") {
		t.Fatalf("injected = %q, want the genuine failure", injected)
	}
}

// --- agent.discarded suppression (lines 1423-1498) ---------------------------

func TestPreInvocationSuppressesWaitingHistoryDiscarded(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess_discard_test"
	events := []any{
		eventWith(101, "term_peer", observability.AgentEventDiscarded, map[string]any{
			"agent": "agy", "reason": planWaitingHistoryReason,
		}),
		eventWith(102, "term_peer", observability.AgentEventFailed, map[string]any{
			"agent": "agy", "reason": "syntax error: unexpected token",
		}),
		eventWith(103, "term_peer", observability.AgentEventDiscarded, map[string]any{
			"agent": "agy", "reason": "user_aborted_operation",
		}),
	}

	client := &hookFakeClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.notifications.ack" {
			return map[string]any{}, nil
		}
		return orchestratorStateResponse(sessionID, events, 0), nil
	}

	out := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     connectReturning(client),
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	injected := injectedMessage(out)
	if strings.Contains(injected, planWaitingHistoryReason) {
		t.Fatalf("injected = %q, want no PLAN_WAITING_HISTORY", injected)
	}
	if !strings.Contains(injected, "syntax error: unexpected token") {
		t.Fatalf("injected = %q, want the real agent.failed", injected)
	}
	if !strings.Contains(injected, "user_aborted_operation") {
		t.Fatalf("injected = %q, want the non-waiting discarded", injected)
	}
	if !regexp.MustCompile(`- failed \S+ \S+\s+reason: syntax error: unexpected token`).MatchString(injected) {
		t.Fatalf("injected = %q, want the failed line for the syntax error", injected)
	}
	if !regexp.MustCompile(`- failed \S+ \S+\s+reason: user_aborted_operation`).MatchString(injected) {
		t.Fatalf("injected = %q, want the failed line for the aborted discarded", injected)
	}
}

// --- Fix A & Fix B: PLAN_STALLED / pi PLAN_WAITING_HISTORY (lines 1499-1592) --

func TestPreInvocationWakesOnStalledAndPiWaitingHistory(t *testing.T) {
	dir := t.TempDir()
	sessionID := "sess_fix_ab_test"
	events := []any{
		eventWith(201, "term_peer1", observability.AgentEventDiscarded, map[string]any{
			"agent": "agy", "reason": planStalledReason,
		}),
		eventWith(202, "term_peer2", observability.AgentEventDiscarded, map[string]any{
			"agent": "pi", "reason": planWaitingHistoryReason,
		}),
		eventWith(203, "term_peer3", observability.AgentEventDiscarded, map[string]any{
			"agent": "agy", "reason": planWaitingHistoryReason,
		}),
		eventWith(204, "term_peer4", observability.AgentEventDiscarded, map[string]any{
			"reason": planWaitingHistoryReason,
		}),
		eventWith(205, "term_peer5", observability.AgentEventDiscarded, map[string]any{
			"agent": nil, "reason": planWaitingHistoryReason,
		}),
	}

	client := &hookFakeClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.notifications.ack" {
			return map[string]any{}, nil
		}
		return orchestratorStateResponse(sessionID, events, 0), nil
	}

	out := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     connectReturning(client),
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	injected := injectedMessage(out)
	// PLAN_STALLED is injected as failed.
	if !strings.Contains(injected, "reason: "+planStalledReason) {
		t.Fatalf("injected = %q, want reason: PLAN_STALLED", injected)
	}
	// The pi agent with PLAN_WAITING_HISTORY is injected as failed too.
	if !strings.Contains(injected, "reason: "+planWaitingHistoryReason) {
		t.Fatalf("injected = %q, want reason: PLAN_WAITING_HISTORY from the pi agent", injected)
	}
	// The agy / empty / null agent rows with PLAN_WAITING_HISTORY stay exempt.
	for _, id := range []string{"203", "204", "205"} {
		if strings.Contains(injected, "event: "+id) {
			t.Fatalf("injected = %q, want no event %s", injected, id)
		}
	}
}

// --- intermediate agent.done superseded by live working (lines 1593-1723) ----

func TestStopDefersIntermediateDoneUntilTheRealCompletion(t *testing.T) {
	dir := t.TempDir()
	sessionID := "a1111111-2222-3333-4444-555555555555"
	pendingEvents := []any{makeDoneEvent(1118, "已改完代码，继续跑测试。")}
	ackedEventID := int64(0)
	ackedIDs := []int64{}
	peerStatus := "working"
	spawnedWaiters := []float64{}
	wakePrompts := []wakePrompt{}

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		switch method {
		case "agent.notifications.ack":
			eventID := asEventID(params.(map[string]any)["eventId"])
			ackedIDs = append(ackedIDs, eventID)
			if eventID > ackedEventID {
				ackedEventID = eventID
			}
			return map[string]any{"acknowledged": true}, nil
		case "agent.list":
			return map[string]any{"agents": []any{
				agentListEntry("agy", "idle", "2026-10-21T11:50:00.000Z", "wB:p1", "term_self"),
				agentListEntry("pi", peerStatus, "2026-10-21T11:50:00.000Z", "wB:p2", "term_peer"),
			}}, nil
		}
		return orchestratorStateResponse(sessionID, pendingEvents, ackedEventID), nil
	}

	stopResult := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, agy.RunAgyHookOptions{
		Connect:     client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
		SpawnWakeWaiter: func(input agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
			spawnedWaiters = append(spawnedWaiters, input.WaiterEpoch)
			return &agy.SpawnWakeWaiterResult{PID: intPtr(os.Getpid())}
		},
	})

	// The intermediate completion is acked silently instead of being handed over
	// as the final result, and a waiter guards the still-running turn.
	if !isEmptyHookOutput(stopResult) {
		t.Fatalf("stop result = %+v, want {}", stopResult)
	}
	client.mu.Lock()
	acked := append([]int64{}, ackedIDs...)
	epochs := append([]float64{}, spawnedWaiters...)
	client.mu.Unlock()
	if want := []int64{1118}; !reflect.DeepEqual(acked, want) {
		t.Fatalf("ackedIDs = %v, want %v", acked, want)
	}
	if want := []float64{1}; !reflect.DeepEqual(epochs, want) {
		t.Fatalf("spawnedWaiters = %v, want %v", epochs, want)
	}
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})

	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connectCapturing,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(25),
			SocketPath:     "/tmp/herdsman.sock",
			StateDir:       strPtr(dir),
			TriggerWakePrompt: func(paneID, promptText string) error {
				client.mu.Lock()
				wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
				client.mu.Unlock()
				return nil
			},
			WaitTimeoutMs: floatPtr(5000),
		})
	}()

	// While the peer is still working the intermediate batch must not prompt.
	time.Sleep(30 * time.Millisecond)
	client.mu.Lock()
	earlyPrompts := len(wakePrompts)
	client.mu.Unlock()
	if earlyPrompts != 0 {
		t.Fatalf("wakePrompts = %d before the real completion, want 0", earlyPrompts)
	}

	// The turn really finishes now: the peer goes idle and the real done lands.
	client.mu.Lock()
	peerStatus = "idle"
	pendingEvents = append(pendingEvents, makeDoneEvent(1126, "全部测试通过。"))
	client.mu.Unlock()
	client.push(makeDoneEvent(1126, "全部测试通过。"))

	if result := awaitHookOutput(t, done); !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	client.mu.Lock()
	prompts := append([]wakePrompt{}, wakePrompts...)
	client.mu.Unlock()
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(prompts, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", prompts, want)
	}
}

// --- Stop decision:continue + unacked delivered fallback (lines 1724-1881) ---

func TestStopSpawnsWaiterAlongsideDecisionContinue(t *testing.T) {
	dir := t.TempDir()
	sessionID := "b1111111-2222-3333-4444-555555555555"
	pendingEvents := []any{makeDoneEvent(200, "工人完成，测试绿。")}
	ackedEventID := int64(0)
	ackedIDs := []int64{}
	spawnedWaiters := []float64{}
	wakePrompts := []wakePrompt{}

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		switch method {
		case "agent.notifications.ack":
			eventID := asEventID(params.(map[string]any)["eventId"])
			ackedIDs = append(ackedIDs, eventID)
			if eventID > ackedEventID {
				ackedEventID = eventID
			}
			return map[string]any{"acknowledged": true}, nil
		case "agent.list":
			return map[string]any{"agents": []any{
				agentListEntry("agy", "idle", "2026-10-21T11:50:00.000Z", "wB:p1", "term_self"),
				agentListEntry("pi", "idle", "2026-10-21T11:50:00.000Z", "wB:p2", "term_peer"),
			}}, nil
		}
		return orchestratorStateResponse(sessionID, pendingEvents, ackedEventID), nil
	}

	spawnWakeWaiter := func(input agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
		client.mu.Lock()
		spawnedWaiters = append(spawnedWaiters, input.WaiterEpoch)
		client.mu.Unlock()
		return &agy.SpawnWakeWaiterResult{PID: intPtr(os.Getpid())}
	}
	triggerWakePrompt := func(paneID, promptText string) error {
		client.mu.Lock()
		wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
		client.mu.Unlock()
		return nil
	}
	stopOptions := func() agy.RunAgyHookOptions {
		return agy.RunAgyHookOptions{
			Connect:         client.connect,
			Environment:     herdrEnv,
			SocketPath:      "/tmp/herdsman.sock",
			SpawnWakeWaiter: spawnWakeWaiter,
			StateDir:        strPtr(dir),
		}
	}
	waiterOptions := func(epoch string) agy.RunAgyHookOptions {
		return agy.RunAgyHookOptions{
			Connect: client.connect,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", epoch),
			PollIntervalMs:    floatPtr(25),
			SocketPath:        "/tmp/herdsman.sock",
			StateDir:          strPtr(dir),
			TriggerWakePrompt: triggerWakePrompt,
			WaitTimeoutMs:     floatPtr(5000),
		}
	}

	stopResult := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, stopOptions())
	if stopResult.Decision == nil || *stopResult.Decision != "continue" {
		t.Fatalf("stop result = %+v, want decision continue", stopResult)
	}
	client.mu.Lock()
	epochs := append([]float64{}, spawnedWaiters...)
	client.mu.Unlock()
	if want := []float64{1}; !reflect.DeepEqual(epochs, want) {
		t.Fatalf("spawnedWaiters = %v, want %v", epochs, want)
	}
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"deliveredEventIds": []any{float64(200)},
		"turnEpoch":         float64(1),
		"waiterEpoch":       float64(1),
		"waiterPid":         float64(os.Getpid()),
	})

	// The host ignores `decision: continue`: the already delivered but unacked
	// batch is the fallback wake signal.
	waiterResult := agy.RunAgyHook(agy.HookWakeWaiter, "", waiterOptions("1"))
	if !isEmptyHookOutput(waiterResult) {
		t.Fatalf("waiter result = %+v, want {}", waiterResult)
	}
	client.mu.Lock()
	prompts := append([]wakePrompt{}, wakePrompts...)
	acked := append([]int64{}, ackedIDs...)
	client.mu.Unlock()
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(prompts, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", prompts, want)
	}
	if len(acked) != 0 {
		t.Fatalf("ackedIDs = %v, want none", acked)
	}

	// A second batch keeps the same contract, but this time the host continues:
	// the next PreInvocation advances turnEpoch and the fallback waiter exits
	// without a duplicate prompt.
	client.mu.Lock()
	pendingEvents = append(pendingEvents, makeDoneEvent(201, "第二批完成。"))
	client.mu.Unlock()
	secondStop := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, stopOptions())
	if secondStop.Decision == nil || *secondStop.Decision != "continue" {
		t.Fatalf("second stop result = %+v, want decision continue", secondStop)
	}
	client.mu.Lock()
	epochs = append([]float64{}, spawnedWaiters...)
	acked = append([]int64{}, ackedIDs...)
	client.mu.Unlock()
	if want := []float64{1, 2}; !reflect.DeepEqual(epochs, want) {
		t.Fatalf("spawnedWaiters = %v, want %v", epochs, want)
	}
	if want := []int64{200}; !reflect.DeepEqual(acked, want) {
		t.Fatalf("ackedIDs = %v, want %v", acked, want)
	}

	preOut := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":2,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	if !isEmptyHookOutput(preOut) {
		t.Fatalf("preOut = %+v, want {}", preOut)
	}
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"turnEpoch": float64(3),
	})

	cancelledWaiter := agy.RunAgyHook(agy.HookWakeWaiter, "", waiterOptions("2"))
	if !isEmptyHookOutput(cancelledWaiter) {
		t.Fatalf("cancelled waiter result = %+v, want {}", cancelledWaiter)
	}
	client.mu.Lock()
	promptsAfter := len(wakePrompts)
	client.mu.Unlock()
	if promptsAfter != 1 {
		t.Fatalf("wakePrompts = %d, want the single fallback prompt", promptsAfter)
	}
}