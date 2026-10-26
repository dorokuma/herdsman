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
	"sync/atomic"
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

// --- intermediate agent.done while the peer still works (lines 1599-1731) ---

func TestIntermediateAgentDoneWhilePeerStillWorkingIsDeferred(t *testing.T) {
	dir := t.TempDir()
	sessionID := "a1111111-2222-3333-4444-555555555555"
	pendingEvents := []any{makeDoneEvent(1118, "已改完代码，继续跑测试。")}
	ackedEventID := int64(0)
	ackedIDs := []int64{}
	peerStatus := "working"
	spawnedWaiters := []float64{}
	wakePrompts := []wakePrompt{}
	warns := []string{}

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
	// collectWarn mirrors the TS console.warn spy.
	collectWarn := func(message string) {
		client.mu.Lock()
		warns = append(warns, message)
		client.mu.Unlock()
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
		Warn: collectWarn,
	})

	// Intermediate completion must not be permanently acked while the peer works;
	// a waiter still guards the running turn.
	if !isEmptyHookOutput(stopResult) {
		t.Fatalf("stop result = %+v, want {}", stopResult)
	}
	client.mu.Lock()
	acked := append([]int64{}, ackedIDs...)
	epochs := append([]float64{}, spawnedWaiters...)
	stopWarns := append([]string{}, warns...)
	client.mu.Unlock()
	if len(acked) != 0 {
		t.Fatalf("ackedIDs = %v, want none", acked)
	}
	if want := []float64{1}; !reflect.DeepEqual(epochs, want) {
		t.Fatalf("spawnedWaiters = %v, want %v", epochs, want)
	}
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})
	if !anyWarnContains(stopWarns, "eventId=1118") {
		t.Fatalf("warns = %v, want one mentioning eventId=1118", stopWarns)
	}

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
			// readPaneText: no real `herdr pane read` in the fake-client cases.
			ReadPaneText: func(string) (*string, error) { return nil, nil },
			TriggerWakePrompt: func(paneID, promptText string) error {
				client.mu.Lock()
				wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
				client.mu.Unlock()
				return nil
			},
			WaitTimeoutMs: floatPtr(5000),
			Warn:          collectWarn,
		})
	}()

	// While the peer is still working the deferred batch must neither prompt nor
	// be acked.
	time.Sleep(40 * time.Millisecond)
	client.mu.Lock()
	earlyPrompts := len(wakePrompts)
	earlyAcked := append([]int64{}, ackedIDs...)
	client.mu.Unlock()
	if earlyPrompts != 0 {
		t.Fatalf("wakePrompts = %d before the real completion, want 0", earlyPrompts)
	}
	if len(earlyAcked) != 0 {
		t.Fatalf("ackedIDs = %v while the peer is working, want none", earlyAcked)
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

	// The wake prompt starts a new turn, so PreInvocation now takes the batch.
	// Both deferred completions of the same terminal are candidates at once; the
	// batch must carry only the newest one.
	wakeTurn := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":2,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect: client.connectCapturing,
		Environment: mergeEnv(herdrEnv,
			"HERDSMAN_AGY_SESSION_ID", sessionID,
			"HERDSMAN_AGY_STATE_DIR", dir),
		ReadPaneText: func(string) (*string, error) { return nil, nil },
		SocketPath:   "/tmp/herdsman.sock",
		StateDir:     strPtr(dir),
		Warn:         collectWarn,
	})
	injected := ""
	if len(wakeTurn.InjectSteps) > 0 {
		injected = wakeTurn.InjectSteps[0].EphemeralMessage
	}
	if !strings.Contains(injected, "全部测试通过。") || !strings.Contains(injected, "event: 1126") {
		t.Fatalf("injected = %q, want the newest completion 1126", injected)
	}
	if strings.Contains(injected, "已改完代码，继续跑测试。") || strings.Contains(injected, "event: 1118") {
		t.Fatalf("injected = %q, want the superseded 1118 removed", injected)
	}
	if got := strings.Count(injected, "- completed "); got != 1 {
		t.Fatalf("injected has %d `- completed` blocks, want 1", got)
	}
	client.mu.Lock()
	injectWarns := append([]string{}, warns...)
	client.mu.Unlock()
	if !anyWarnContains(injectWarns, "eventId=1118") ||
		!anyWarnContains(injectWarns, "superseded by newer completion for terminal term_peer") {
		t.Fatalf("warns = %v, want the 1118 supersession warning", injectWarns)
	}
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"presentedEventIds": []any{float64(1118), float64(1126)},
		"deliveredEventIds": []any{float64(1118), float64(1126)},
	})

	// The woken turn's Stop settles the batch, so the superseded mid-turn
	// completion is acked and can never be injected again later.
	client.mu.Lock()
	ackedBeforeStop := append([]int64{}, ackedIDs...)
	client.mu.Unlock()
	stopSettle := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     client.connectCapturing,
		Environment: herdrEnv,
		ReadPaneText: func(string) (*string, error) {
			return nil, nil
		},
		SocketPath: "/tmp/herdsman.sock",
		SpawnWakeWaiter: func(agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
			return &agy.SpawnWakeWaiterResult{PID: intPtr(os.Getpid())}
		},
		StateDir: strPtr(dir),
		Warn:     collectWarn,
	})
	if !isEmptyHookOutput(stopSettle) {
		t.Fatalf("stop result = %+v, want {}", stopSettle)
	}
	client.mu.Lock()
	ackedAfterStop := append([]int64{}, ackedIDs...)
	finalPrompts := append([]wakePrompt{}, wakePrompts...)
	client.mu.Unlock()
	ackedSet := map[int64]bool{}
	for _, id := range ackedAfterStop {
		ackedSet[id] = true
	}
	for _, want := range []int64{1118, 1126} {
		if !ackedSet[want] {
			t.Fatalf("ackedIDs = %v (before stop %v), want both 1118 and 1126", ackedAfterStop, ackedBeforeStop)
		}
	}
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(finalPrompts, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", finalPrompts, want)
	}
}

// --- WakeWaiter working-race / composer-draft deferral (lines 1733-2079) ----

// anyWarnContains mirrors
// `warnSpy.mock.calls.some((call) => String(call[0]).includes(needle))`.
func anyWarnContains(warns []string, needle string) bool {
	for _, message := range warns {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

// workingRaceClient answers the three RPC methods the wake waiter drives, with
// a mutable peer status and ack cursor (the TS `connect` closure of the
// working-race cases).
func workingRaceClient(
	t *testing.T,
	sessionID string,
	events []any,
	peerStatus *string,
	ackedEventID *int64,
	ackedIDs *[]int64,
) *concClient {
	t.Helper()
	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		switch method {
		case "agent.notifications.ack":
			eventID := asEventID(params.(map[string]any)["eventId"])
			if ackedIDs != nil {
				*ackedIDs = append(*ackedIDs, eventID)
			}
			if eventID > *ackedEventID {
				*ackedEventID = eventID
			}
			return map[string]any{"acknowledged": true}, nil
		case "agent.list":
			return map[string]any{"agents": []any{
				agentListEntry("agy", "idle", "2026-10-25T05:06:00.000Z", "wB:p1", "term_self"),
				agentListEntry("agy", *peerStatus, "2026-10-25T05:06:00.000Z", "wB:p2", "term_peer"),
			}}, nil
		}
		return orchestratorStateResponse(sessionID, events, *ackedEventID), nil
	}
	return client
}

// presentedEventIDsOf projects the persisted `presentedEventIds` field of a
// session state file (missing means the TS `?? []`).
func presentedEventIDsOf(t *testing.T, dir, sessionID string) []any {
	t.Helper()
	object := readStateObject(t, sessionStatePath(dir, sessionID))
	ids, ok := object["presentedEventIds"].([]any)
	if !ok {
		return []any{}
	}
	return ids
}

func TestWakeWaiterRaceAgentDoneWhileWorkingNextPollWakesOnce(t *testing.T) {
	dir := t.TempDir()
	sessionID := "c1111111-2222-3333-4444-555555555555"
	events := []any{makeDoneEvent(73501, "子代理最终结论，很长一段正文。")}
	ackedEventID := int64(0)
	ackedIDs := []int64{}
	peerStatus := "working"
	wakePrompts := []wakePrompt{}
	warns := []string{}

	writeStateFile(t, sessionStatePath(dir, sessionID), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})

	client := workingRaceClient(t, sessionID, events, &peerStatus, &ackedEventID, &ackedIDs)
	collectWarn := func(message string) {
		client.mu.Lock()
		warns = append(warns, message)
		client.mu.Unlock()
	}

	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connect,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(30),
			SocketPath:     "/tmp/herdsman.sock",
			StateDir:       strPtr(dir),
			TriggerWakePrompt: func(paneID, promptText string) error {
				client.mu.Lock()
				wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
				client.mu.Unlock()
				return nil
			},
			ReadPaneText:  func(string) (*string, error) { return nil, nil },
			WaitTimeoutMs: floatPtr(5000),
			Warn:          collectWarn,
		})
	}()

	// The first peek deferred the done event: no ack, no wake prompt, and the
	// deferral is visible both in the warn output and in the untouched state.
	time.Sleep(50 * time.Millisecond)
	client.mu.Lock()
	prompts := len(wakePrompts)
	acked := append([]int64{}, ackedIDs...)
	seenWarns := append([]string{}, warns...)
	client.mu.Unlock()
	if prompts != 0 {
		t.Fatalf("wakePrompts = %d while the peer is working, want 0", prompts)
	}
	if len(acked) != 0 {
		t.Fatalf("ackedIDs = %v, want none", acked)
	}
	if !anyWarnContains(seenWarns, "eventId=73501") || !anyWarnContains(seenWarns, "still working") {
		t.Fatalf("warns = %v, want one deferring eventId=73501 as still working", seenWarns)
	}
	if ids := presentedEventIDsOf(t, dir, sessionID); len(ids) != 0 {
		t.Fatalf("presentedEventIds = %v, want none", ids)
	}

	// Live status catches up on the next poll, so the deferred completion wakes.
	client.mu.Lock()
	peerStatus = "done"
	client.mu.Unlock()

	if result := awaitHookOutput(t, done); !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	client.mu.Lock()
	promptsAfter := append([]wakePrompt{}, wakePrompts...)
	client.mu.Unlock()
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(promptsAfter, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", promptsAfter, want)
	}
}

func TestWakeWaiterDoesNotWakeWhileDeferredPeerStaysWorking(t *testing.T) {
	dir := t.TempDir()
	sessionID := "d1111111-2222-3333-4444-555555555555"
	events := []any{makeDoneEvent(8001, "中途一轮工具结果。")}
	ackedEventID := int64(0)
	wakePrompts := []wakePrompt{}

	writeStateFile(t, sessionStatePath(dir, sessionID), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		switch method {
		case "agent.notifications.ack":
			eventID := asEventID(params.(map[string]any)["eventId"])
			if eventID > ackedEventID {
				ackedEventID = eventID
			}
			return map[string]any{"acknowledged": true}, nil
		case "agent.list":
			// The peer never leaves `working`: the completion is a mid-turn tool
			// round, so it must stay deferred for the whole waiter lifetime.
			return map[string]any{"agents": []any{
				agentListEntry("agy", "idle", "2026-10-21T11:50:00.000Z", "wB:p1", "term_self"),
				agentListEntry("pi", "working", "2026-10-21T11:50:00.000Z", "wB:p2", "term_peer"),
			}}, nil
		}
		return orchestratorStateResponse(sessionID, events, ackedEventID), nil
	}

	result := agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
		Connect: client.connect,
		Environment: mergeEnv(herdrEnv,
			"HERDSMAN_AGY_SESSION_ID", sessionID,
			"HERDSMAN_AGY_STATE_DIR", dir,
			"HERDSMAN_AGY_WAITER_EPOCH", "1"),
		PollIntervalMs: floatPtr(20),
		SocketPath:     "/tmp/herdsman.sock",
		StateDir:       strPtr(dir),
		TriggerWakePrompt: func(paneID, promptText string) error {
			client.mu.Lock()
			wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
			client.mu.Unlock()
			return nil
		},
		ReadPaneText:  func(string) (*string, error) { return nil, nil },
		WaitTimeoutMs: floatPtr(120),
	})

	if !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	client.mu.Lock()
	prompts := append([]wakePrompt{}, wakePrompts...)
	client.mu.Unlock()
	if len(prompts) != 0 {
		t.Fatalf("wakePrompts = %+v, want none", prompts)
	}
	if ackedEventID != 0 {
		t.Fatalf("ackedEventId = %d, want 0", ackedEventID)
	}
	if ids := presentedEventIDsOf(t, dir, sessionID); len(ids) != 0 {
		t.Fatalf("presentedEventIds = %v, want none", ids)
	}
}

func TestWakeWaiterDefersTriggerWakePromptWhileComposerHasDraft(t *testing.T) {
	dir := t.TempDir()
	sessionID := "e1111111-2222-3333-4444-555555555555"
	events := []any{makeDoneEvent(9001, "工人完成。")}
	ackedEventID := int64(0)
	hasDraft := true
	wakePrompts := []wakePrompt{}
	warns := []string{}

	writeStateFile(t, sessionStatePath(dir, sessionID), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})

	rule := "───────────────────────────────────────────────────"
	emptyComposer := strings.Join([]string{
		"assistant output",
		rule,
		">",
		rule,
		"? for shortcuts             Gemini 4 Argon · medium",
	}, "\n")
	draftComposer := strings.Join([]string{
		"assistant output",
		rule,
		"> 用户正在打的草稿",
		rule,
		"? for shortcuts             Gemini 4 Argon · medium",
	}, "\n")

	client := &concClient{}
	client.respond = func(method string, _ any) (any, error) {
		switch method {
		case "agent.notifications.ack":
			return map[string]any{"acknowledged": true}, nil
		case "agent.list":
			return map[string]any{"agents": []any{
				agentListEntry("agy", "idle", "2026-10-21T11:50:00.000Z", "wB:p1", "term_self"),
			}}, nil
		}
		return orchestratorStateResponse(sessionID, events, ackedEventID), nil
	}

	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connect,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(25),
			ReadPaneText: func(string) (*string, error) {
				client.mu.Lock()
				defer client.mu.Unlock()
				if hasDraft {
					return &draftComposer, nil
				}
				return &emptyComposer, nil
			},
			SocketPath: "/tmp/herdsman.sock",
			StateDir:   strPtr(dir),
			TriggerWakePrompt: func(paneID, promptText string) error {
				client.mu.Lock()
				wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
				client.mu.Unlock()
				return nil
			},
			WaitTimeoutMs: floatPtr(5000),
			Warn: func(message string) {
				client.mu.Lock()
				warns = append(warns, message)
				client.mu.Unlock()
			},
		})
	}()

	// While the composer holds a draft the wake prompt must be deferred.
	time.Sleep(50 * time.Millisecond)
	client.mu.Lock()
	prompts := len(wakePrompts)
	seenWarns := append([]string{}, warns...)
	client.mu.Unlock()
	if prompts != 0 {
		t.Fatalf("wakePrompts = %d while the composer has a draft, want 0", prompts)
	}
	if !anyWarnContains(seenWarns, "输入框有草稿，推迟叫醒") {
		t.Fatalf("warns = %v, want the composer-draft deferral warning", seenWarns)
	}

	client.mu.Lock()
	hasDraft = false
	client.mu.Unlock()

	if result := awaitHookOutput(t, done); !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	client.mu.Lock()
	promptsAfter := append([]wakePrompt{}, wakePrompts...)
	client.mu.Unlock()
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(promptsAfter, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", promptsAfter, want)
	}
}

func TestParseAgyComposerDraft(t *testing.T) {
	rule := "───────────────────────────────────────────────────"
	assertDraft := func(t *testing.T, paneText string, want *string) {
		t.Helper()
		got := agy.ParseAgyComposerDraft(paneText)
		if want == nil {
			if got != nil {
				t.Fatalf("ParseAgyComposerDraft(%q) = %q, want nil", paneText, *got)
			}
			return
		}
		if got == nil {
			t.Fatalf("ParseAgyComposerDraft(%q) = nil, want %q", paneText, *want)
		}
		if *got != *want {
			t.Fatalf("ParseAgyComposerDraft(%q) = %q, want %q", paneText, *got, *want)
		}
	}

	assertDraft(t, strings.Join([]string{"output", rule, ">", rule, "? for shortcuts             Gemini 4 Argon · medium"}, "\n"), strPtr(""))
	assertDraft(t, strings.Join([]string{"output", rule, "> hello draft", rule, "? for shortcuts"}, "\n"), strPtr("hello draft"))
	assertDraft(t, strings.Join([]string{"output", rule, "> line one", "line two", "line three", rule, "? for shortcuts"}, "\n"), strPtr("line one\nline two\nline three"))
	// A sole `>` with only whitespace is still empty (no placeholder observed on real panes).
	assertDraft(t, strings.Join([]string{"output", rule, ">   ", rule, "footer"}, "\n"), strPtr(""))
	// A draft that itself contains a rule must not collapse the box: the real top
	// rule is the last one followed by the `>` prompt line, not the previous rule.
	assertDraft(t, strings.Join([]string{"output", rule, "> 第一段", "--------", "第三段", rule, "? for shortcuts"}, "\n"), strPtr("第一段\n--------\n第三段"))
	assertDraft(t, strings.Join([]string{"output", rule, "> 第一段", "第二行", rule, rule, "? for shortcuts"}, "\n"), strPtr("第一段\n第二行\n"+rule))
	// A rule on the draft's last line would make the naive second-to-last rule sit
	// right above the bottom; the box must still be located.
	assertDraft(t, strings.Join([]string{"output", rule, "> 草稿", rule, rule, "? for shortcuts"}, "\n"), strPtr("草稿\n"+rule))
	// A composer box that cannot be located yields nil so wake keeps working.
	assertDraft(t, "no separators here\n> orphan", nil)
	assertDraft(t, rule+"\n"+rule, nil)
	// No rule above the box bottom is followed by a `>` prompt line.
	assertDraft(t, strings.Join([]string{"output", rule, "plain", rule, "footer"}, "\n"), nil)
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

// --- retryWakePending-only wake (84fc8b1) -----------------------------------

// TestRetryWakePendingAloneWakesWhenOneDeferredPeerSettles mirrors the TS
// "WakeWaiter wakes through retryWakePending alone when one deferred peer settles
// while another keeps working": peer A's deferred completion retries the poll on
// its own, and peer B keeps reporting working so the no-active-peers path can
// never fire. Without the retryWakePending flag the wake is lost.
func TestRetryWakePendingAloneWakesWhenOneDeferredPeerSettles(t *testing.T) {
	dir := t.TempDir()
	sessionID := "f1111111-2222-3333-4444-555555555555"
	pendingEvents := []any{makeDoneEvent(9500, "工人 A 的最终结论。", "term_peer_a")}
	ackedEventID := int64(0)
	peerAStatus := "working"
	wakePrompts := []wakePrompt{}
	warns := []string{}

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		switch method {
		case "agent.notifications.ack":
			eventID := asEventID(params.(map[string]any)["eventId"])
			if eventID > ackedEventID {
				ackedEventID = eventID
			}
			return map[string]any{"acknowledged": true}, nil
		case "agent.list":
			return map[string]any{"agents": []any{
				agentListEntry("agy", "idle", "2026-10-25T05:06:00.000Z", "wB:p1", "term_self"),
				agentListEntry("agy", peerAStatus, "2026-10-25T05:06:00.000Z", "wB:p2", "term_peer_a"),
				// Peer B never leaves working and no push notification ever arrives,
				// so `!hasActivePeers` can never trigger the check.
				agentListEntry("agy", "working", "2026-10-25T05:06:00.000Z", "wB:p3", "term_peer_b"),
			}}, nil
		}
		return orchestratorStateResponse(sessionID, pendingEvents, ackedEventID), nil
	}
	writeStateFile(t, sessionStatePath(dir, sessionID), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})
	collectWarn := func(message string) {
		client.mu.Lock()
		warns = append(warns, message)
		client.mu.Unlock()
	}

	// An empty composer: `herdr pane read` shows the lone `>` prompt line.
	emptyComposer := strings.Join([]string{
		"assistant output",
		"───────────────────────────────────────────────────",
		">",
		"───────────────────────────────────────────────────",
		"? for shortcuts",
	}, "\n")

	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connectCapturing,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(20),
			ReadPaneText: func(string) (*string, error) {
				return &emptyComposer, nil
			},
			SocketPath: "/tmp/herdsman.sock",
			StateDir:   strPtr(dir),
			TriggerWakePrompt: func(paneID, promptText string) error {
				client.mu.Lock()
				wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
				client.mu.Unlock()
				return nil
			},
			WaitTimeoutMs: floatPtr(5000),
			Warn:          collectWarn,
		})
	}()

	// Peer A is still working: the completion is deferred, nothing is acked and
	// peer B keeps the wake check from firing through the no-active-peers path.
	time.Sleep(60 * time.Millisecond)
	client.mu.Lock()
	earlyPrompts := len(wakePrompts)
	earlyAcked := ackedEventID
	earlyWarns := append([]string{}, warns...)
	client.mu.Unlock()
	if earlyPrompts != 0 {
		t.Fatalf("wakePrompts = %d while peer A works, want 0", earlyPrompts)
	}
	if earlyAcked != 0 {
		t.Fatalf("ackedEventID = %d while peer A works, want 0", earlyAcked)
	}
	if !anyWarnContains(earlyWarns, "eventId=9500") || !anyWarnContains(earlyWarns, "still working") {
		t.Fatalf("warns = %v, want the 9500 still-working warning", earlyWarns)
	}

	// No push notification is delivered: the deferred retry timer is the only
	// thing that can re-check the batch and wake the orchestrator.
	client.mu.Lock()
	peerAStatus = "done"
	client.mu.Unlock()
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

// --- ack-cursor holdback against deferred-event swallowing (adf6f09) -------

// TestLowIDWorkingDeferredEventIsNeverSwallowedByHigherIDAck mirrors the TS
// "a low-id working-deferred event is never swallowed by the ack of a higher-id
// suppressed event": `agent.notifications.ack` settles *every* row with
// `id <= eventId`, so the ack of the suppressed 105 must wait while the deferred
// 100 still owes its projection.
func TestLowIDWorkingDeferredEventIsNeverSwallowedByHigherIDAck(t *testing.T) {
	dir := t.TempDir()
	sessionID := "d1111111-2222-3333-4444-555555555555"
	ackedIDs := []int64{}
	ackedEventID := int64(0)
	peerAStatus := "working"

	peerAEvent := makeDoneEvent(100, "工人 A 的最终结论。", "term_peer_a")
	peerAEvent.AgentID = strPtr("ag_peer_a")
	peerAEvent.PaneID = strPtr("wB:p2")
	// Peer B finished with an empty assistant text: permanently suppressed noise.
	peerBEvent := makeDoneEvent(105, "", "term_peer_b")
	peerBEvent.AgentID = strPtr("ag_peer_b")
	peerBEvent.PaneID = strPtr("wB:p3")
	pendingEvents := []any{peerAEvent, peerBEvent}

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
				agentListEntry("agy", "idle", "2026-10-25T05:06:00.000Z", "wB:p1", "term_self"),
				agentListEntry("pi", peerAStatus, "2026-10-25T05:06:00.000Z", "wB:p2", "term_peer_a"),
				agentListEntry("pi", "idle", "2026-10-25T05:06:00.000Z", "wB:p3", "term_peer_b"),
			}}, nil
		}
		// Real markAcked only re-lists rows above the cursor.
		live := []any{}
		for _, event := range pendingEvents {
			if event.(observability.AgentEventRecord).ID > ackedEventID {
				live = append(live, event)
			}
		}
		return orchestratorStateResponse(sessionID, live, ackedEventID), nil
	}
	stopOptions := func() agy.RunAgyHookOptions {
		return agy.RunAgyHookOptions{
			Connect:     client.connect,
			Environment: herdrEnv,
			SocketPath:  "/tmp/herdsman.sock",
			SpawnWakeWaiter: func(agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
				return &agy.SpawnWakeWaiterResult{PID: intPtr(os.Getpid())}
			},
			StateDir: strPtr(dir),
		}
	}
	readState := func() map[string]any {
		return readStateObject(t, sessionStatePath(dir, sessionID))
	}
	// 1. Peer A is still `working`, so event 100 is deferred; event 105 is
	//    permanently suppressed noise sitting above it.
	firstStop := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, stopOptions())
	if !isEmptyHookOutput(firstStop) {
		t.Fatalf("firstStop = %+v, want {}", firstStop)
	}
	client.mu.Lock()
	firstAcked := append([]int64{}, ackedIDs...)
	client.mu.Unlock()
	// The ack of 105 must not fire: it would settle `id <= 105` and swallow the
	// deferred event 100 forever.
	if len(firstAcked) != 0 {
		t.Fatalf("ackedIDs = %v after the deferred stop, want none", firstAcked)
	}
	assertStateMatches(t, readState(), map[string]any{
		"deferredEventIds":   []any{float64(100)},
		"pendingAckEventIds": []any{float64(105)},
		"presentedEventIds":  []any{float64(105)},
	})

	// 2. Peer A stops reporting working: event 100 becomes a real outcome. The
	//    held-back ack of 105 must still wait, because acking it would advance
	//    the cursor past the not-yet-delivered event 100.
	client.mu.Lock()
	peerAStatus = "done"
	client.mu.Unlock()
	secondStop := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, stopOptions())
	if secondStop.Decision == nil || *secondStop.Decision != "continue" {
		t.Fatalf("secondStop = %+v, want decision continue", secondStop)
	}
	if secondStop.Reason == nil || !strings.Contains(*secondStop.Reason, "工人 A 的最终结论。") {
		t.Fatalf("secondStop.Reason = %v, want the peer A completion", secondStop.Reason)
	}
	client.mu.Lock()
	secondAcked := append([]int64{}, ackedIDs...)
	client.mu.Unlock()
	if len(secondAcked) != 0 {
		t.Fatalf("ackedIDs = %v while 100 is undelivered, want none", secondAcked)
	}
	assertStateMatches(t, readState(), map[string]any{
		"deliveredEventIds":  []any{float64(100)},
		"pendingAckEventIds": []any{float64(105)},
	})

	// 3. Once both events are presented (consumed by the host) nothing blocks
	//    the cursor any more and both are acknowledged.
	thirdStop := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, stopOptions())
	if !isEmptyHookOutput(thirdStop) {
		t.Fatalf("thirdStop = %+v, want {}", thirdStop)
	}
	client.mu.Lock()
	thirdAcked := append([]int64{}, ackedIDs...)
	client.mu.Unlock()
	if !reflect.DeepEqual(thirdAcked, []int64{100, 105}) {
		t.Fatalf("ackedIDs = %v, want [100 105]", thirdAcked)
	}
	assertStateMatches(t, readState(), map[string]any{
		"deliveredEventIds":  []any{},
		"pendingAckEventIds": []any{},
	})
}

// --- wake-epoch race while readComposerDraft is in flight (adf6f09) --------

// TestWakeWaiterCancelsWhenTurnEpochAdvancesDuringComposerRead mirrors the TS
// "WakeWaiter cancels the wake when turnEpoch advances while readComposerDraft
// is in flight": the pane read is up to 5s of I/O, and a new turn started in the
// meantime makes every shared-state write by this waiter stale.
func TestWakeWaiterCancelsWhenTurnEpochAdvancesDuringComposerRead(t *testing.T) {
	dir := t.TempDir()
	sessionID := "e1111111-2222-3333-4444-555555555555"
	pendingEvents := []any{makeDoneEvent(300, "工人完成，测试绿。")}
	wakePrompts := []wakePrompt{}

	emptyComposer := strings.Join([]string{
		"assistant output",
		"───────────────────────────────────────────────────",
		">",
		"───────────────────────────────────────────────────",
		"? for shortcuts",
	}, "\n")

	readGate := make(chan string, 1)
	readCalls := int64(0)

	client := workingRaceClient(t, sessionID, pendingEvents, strPtr("idle"), new(int64), nil)
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.list" {
			return map[string]any{"agents": []any{
				agentListEntry("agy", "idle", "2026-10-25T05:06:00.000Z", "wB:p1", "term_self"),
				agentListEntry("pi", "idle", "2026-10-25T05:06:00.000Z", "wB:p2", "term_peer"),
			}}, nil
		}
		return orchestratorStateResponse(sessionID, pendingEvents, 0), nil
	}

	stopResult := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, agy.RunAgyHookOptions{
		Connect:     client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		SpawnWakeWaiter: func(agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
			return &agy.SpawnWakeWaiterResult{PID: intPtr(os.Getpid())}
		},
		StateDir: strPtr(dir),
	})
	if stopResult.Decision == nil || *stopResult.Decision != "continue" {
		t.Fatalf("stop result = %+v, want decision continue", stopResult)
	}
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"deliveredEventIds": []any{float64(300)},
		"presentedEventIds": []any{float64(300)},
		"turnEpoch":         float64(1),
		"waiterEpoch":       float64(1),
		"waiterPid":         float64(os.Getpid()),
	})

	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connectCapturing,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(20),
			ReadPaneText: func(string) (*string, error) {
				atomic.AddInt64(&readCalls, 1)
				// Mirrors the TS readGate: the pane read blocks until the test
				// releases it, so the epoch bump below lands while it is in flight.
				text := <-readGate
				return &text, nil
			},
			SocketPath: "/tmp/herdsman.sock",
			StateDir:   strPtr(dir),
			TriggerWakePrompt: func(paneID, promptText string) error {
				client.mu.Lock()
				wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
				client.mu.Unlock()
				return nil
			},
			WaitTimeoutMs: floatPtr(5000),
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&readCalls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt64(&readCalls) == 0 {
		t.Fatal("readPaneText was never called, want exactly one composer read")
	}

	// The user submits a new turn while the pane read is still in flight: the
	// epoch this waiter was started for is gone.
	inFlightState := readStateObject(t, sessionStatePath(dir, sessionID))
	inFlightState["turnEpoch"] = float64(2)
	writeStateFile(t, sessionStatePath(dir, sessionID), inFlightState)
	readGate <- emptyComposer

	if result := awaitHookOutput(t, done); !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	if len(wakePrompts) != 0 {
		t.Fatalf("wakePrompts = %+v, want none", wakePrompts)
	}
	// `rearmDeliveredEvents` must not have dropped the delivered batch from
	// `presentedEventIds`, otherwise the next PreInvocation injects it twice.
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"deliveredEventIds": []any{float64(300)},
		"presentedEventIds": []any{float64(300)},
		"turnEpoch":         float64(2),
	})
}

// --- peek must not wipe a turn's presentedEventIds (74df962) ---------------

// TestPeekNextWakeBatchDoesNotWipePresentedEventIDsOfConcurrentTurn mirrors the
// TS "peekNextWakeBatch does not wipe presentedEventIds written by a turn that
// started during agent.list": the agent.list round trip inside the peek is
// asynchronous, so the turn this waiter was started for can already be gone by
// the time it writes back.
func TestPeekNextWakeBatchDoesNotWipePresentedEventIDsOfConcurrentTurn(t *testing.T) {
	dir := t.TempDir()
	sessionID := "f1111111-2222-3333-4444-555555555555"
	statePath := sessionStatePath(dir, sessionID)
	pendingEvents := []any{makeDoneEvent(300, "工人完成，测试绿。")}
	wakePrompts := []wakePrompt{}
	agentListCalls := int64(0)
	listGate := make(chan struct{})

	// A waiter owns the session; the batch is not delivered/presented yet.
	writeStateFile(t, statePath, map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})

	emptyComposer := strings.Join([]string{
		"assistant output",
		"──────────────────────────────────────────────────",
		">",
		"──────────────────────────────────────────────────",
		"? for shortcuts",
	}, "\n")

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.list" {
			calls := atomic.AddInt64(&agentListCalls, 1)
			// One waiter iteration calls `agent.list` twice: inspectWorkspaceAgents
			// and then fetchLivePeerScope inside peekNextWakeBatch. Gate the second
			// one, i.e. freeze the peek right at its await.
			if calls == 2 {
				<-listGate
			}
			return map[string]any{"agents": []any{
				agentListEntry("agy", "idle", "2026-10-25T05:06:00.000Z", "wB:p1", "term_self"),
				agentListEntry("pi", "idle", "2026-10-25T05:06:00.000Z", "wB:p2", "term_peer"),
			}}, nil
		}
		return orchestratorStateResponse(sessionID, pendingEvents, 0), nil
	}

	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connectCapturing,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(20),
			ReadPaneText: func(string) (*string, error) {
				return &emptyComposer, nil
			},
			SocketPath: "/tmp/herdsman.sock",
			StateDir:   strPtr(dir),
			TriggerWakePrompt: func(paneID, promptText string) error {
				client.mu.Lock()
				wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
				client.mu.Unlock()
				return nil
			},
			WaitTimeoutMs: floatPtr(5000),
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&agentListCalls) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt64(&agentListCalls) < 2 {
		t.Fatalf("agent.list calls = %d, want at least 2 (the peek froze)", atomic.LoadInt64(&agentListCalls))
	}

	// The user submits a turn while the peek is awaiting `agent.list`: the new
	// PreInvocation consumes the batch and marks 300 presented on disk.
	writeStateFile(t, statePath, map[string]any{
		"deliveredEventIds": []any{float64(300)},
		"presentedEventIds": []any{float64(300)},
		"turnEpoch":         float64(2),
	})
	close(listGate)

	if result := awaitHookOutput(t, done); !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	// The stale peek must not hand the batch to the host a second time.
	if len(wakePrompts) != 0 {
		t.Fatalf("wakePrompts = %+v, want none", wakePrompts)
	}
	// ...and its write-back must not erase the batch the new turn recorded.
	assertStateMatches(t, readStateObject(t, statePath), map[string]any{
		"deliveredEventIds": []any{float64(300)},
		"presentedEventIds": []any{float64(300)},
		"turnEpoch":         float64(2),
	})
}

// --- unconsumed events outlive the daemon 300s redelivery window (74df962) --

// TestUnconsumedEventSurvivesDaemonRedeliveryWindow mirrors the TS "an unconsumed
// event survives the daemon 300s redelivery window: long composer draft and long
// working-defer". `listAfter` only re-lists delivered rows whose last_attempt_at
// is inside REDELIVERY_FRESHNESS_MS, so an event the hook has not consumed yet
// disappears from connState.events: without the retained snapshot it can neither
// be injected nor hold the ack cursor back.
func TestUnconsumedEventSurvivesDaemonRedeliveryWindow(t *testing.T) {
	selfAgent := agentListEntry("agy", "idle", "2026-10-21T11:50:00.000Z", "wB:p1", "term_self")
	// The pane that produced the completion is still open — only its `agent.done`
	// row left the daemon's redelivery window — so `agent.list` keeps reporting it.
	// That is what filterEligibleRetainedEvents uses to tell "too old to be
	// re-listed" apart from "source pane closed".
	peerAgent := agentListEntry("pi", "done", "2026-10-21T11:50:00.000Z", "wB:p2", "term_peer")
	emptyComposer := strings.Join([]string{
		"assistant output",
		"──────────────────────────────────────────────────",
		">",
		"──────────────────────────────────────────────────",
		"? for shortcuts",
	}, "\n")
	draftComposer := strings.Join([]string{
		"assistant output",
		"──────────────────────────────────────────────────",
		"> 用户正在打的草稿",
		"──────────────────────────────────────────────────",
		"? for shortcuts",
	}, "\n")

	// --- 1. the composer draft outlives REDELIVERY_FRESHNESS_MS --------------
	draftDir := t.TempDir()
	draftSessionID := "f2222222-2222-3333-4444-555555555555"
	draftStatePath := sessionStatePath(draftDir, draftSessionID)
	draftEvent := makeDoneEvent(9001, "工人完成。")
	draftWakePrompts := []wakePrompt{}
	draftWarns := []string{}
	// `agent.orchestrator.get` hands out event 9001 once (reservePending marks it
	// `delivered`); after 300s listAfter stops re-listing that row while the hook
	// still has not consumed it.
	daemonListsDraftEvent := true
	composerHasDraft := true

	writeStateFile(t, draftStatePath, map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})

	draftClient := &concClient{}
	draftClient.respond = func(method string, params any) (any, error) {
		if method == "agent.list" {
			return map[string]any{"agents": []any{selfAgent, peerAgent}}, nil
		}
		events := []any{}
		if daemonListsDraftEvent {
			events = append(events, draftEvent)
		}
		return orchestratorStateResponse(draftSessionID, events, 0), nil
	}
	draftWarn := func(message string) {
		draftClient.mu.Lock()
		draftWarns = append(draftWarns, message)
		draftClient.mu.Unlock()
	}

	draftDone := make(chan agy.HookOutput, 1)
	go func() {
		draftDone <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: draftClient.connectCapturing,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", draftSessionID,
				"HERDSMAN_AGY_STATE_DIR", draftDir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(20),
			ReadPaneText: func(string) (*string, error) {
				if composerHasDraft {
					return &draftComposer, nil
				}
				return &emptyComposer, nil
			},
			SocketPath: "/tmp/herdsman.sock",
			StateDir:   strPtr(draftDir),
			TriggerWakePrompt: func(paneID, promptText string) error {
				draftClient.mu.Lock()
				draftWakePrompts = append(draftWakePrompts, wakePrompt{paneID, promptText})
				draftClient.mu.Unlock()
				return nil
			},
			WaitTimeoutMs: floatPtr(2000),
			Warn:          draftWarn,
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		draftClient.mu.Lock()
		deferred := anyWarnContains(draftWarns, "输入框有草稿，推迟叫醒")
		draftClient.mu.Unlock()
		if deferred || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	draftClient.mu.Lock()
	sawDraftDefer := anyWarnContains(draftWarns, "输入框有草稿，推迟叫醒")
	draftClient.mu.Unlock()
	if !sawDraftDefer {
		t.Fatalf("warns = %v, want the composer-draft deferral", draftWarns)
	}

	// The user keeps typing past the redelivery window: the daemon no longer
	// lists the event, but the batch must stay alive in the session state.
	draftClient.mu.Lock()
	daemonListsDraftEvent = false
	draftClient.mu.Unlock()
	time.Sleep(80 * time.Millisecond)
	draftClient.mu.Lock()
	earlyDraftPrompts := append([]wakePrompt{}, draftWakePrompts...)
	draftClient.mu.Unlock()
	if len(earlyDraftPrompts) != 0 {
		t.Fatalf("wakePrompts = %+v while the draft persists, want none", earlyDraftPrompts)
	}

	// The draft is gone: the retained batch must still wake the orchestrator.
	draftClient.mu.Lock()
	composerHasDraft = false
	draftClient.mu.Unlock()

	if result := awaitHookOutput(t, draftDone); !isEmptyHookOutput(result) {
		t.Fatalf("draft waiter result = %+v, want {}", result)
	}
	draftClient.mu.Lock()
	draftPrompts := append([]wakePrompt{}, draftWakePrompts...)
	draftClient.mu.Unlock()
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(draftPrompts, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", draftPrompts, want)
	}
	// A peek never presents: the event is still unacked, unpresented and retained.
	draftState := readStateObject(t, draftStatePath)
	assertStateMatches(t, draftState, map[string]any{
		"presentedEventIds": []any{},
	})
	retained, ok := draftState["retainedCandidateEvents"].([]any)
	if !ok || len(retained) != 1 {
		t.Fatalf("retainedCandidateEvents = %v, want one entry", draftState["retainedCandidateEvents"])
	}
	if retainedID := retained[0].(map[string]any)["id"].(float64); retainedID != 9001 {
		t.Fatalf("retainedCandidateEvents[0].id = %v, want 9001", retainedID)
	}

	// The wake prompt starts the turn, whose PreInvocation must still inject it
	// even though `agent.orchestrator.get` returns no events any more.
	draftPre := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":2,"sessionId":"`+draftSessionID+`"}`, agy.RunAgyHookOptions{
		Connect:      draftClient.connect,
		Environment:  herdrEnv,
		ReadPaneText: func(string) (*string, error) { return nil, nil },
		SocketPath:   "/tmp/herdsman.sock",
		StateDir:     strPtr(draftDir),
	})
	if len(draftPre.InjectSteps) == 0 || !strings.Contains(draftPre.InjectSteps[0].EphemeralMessage, "工人完成。") {
		t.Fatalf("draftPre.InjectSteps = %+v, want the retained completion", draftPre.InjectSteps)
	}
	assertStateMatches(t, readStateObject(t, draftStatePath), map[string]any{
		"deliveredEventIds": []any{float64(9001)},
		"presentedEventIds": []any{float64(9001)},
	})

	// --- 2. a working-deferred low id outlives the window ---------------------
	deferDir := t.TempDir()
	deferSessionID := "f3333333-2222-3333-4444-555555555555"
	deferStatePath := sessionStatePath(deferDir, deferSessionID)
	ackedIDs := []int64{}
	ackedEventID := int64(0)
	peerAStatus := "working"

	peerAEvent := makeDoneEvent(100, "工人 A 的最终结论。", "term_peer_a")
	peerAEvent.AgentID = strPtr("ag_peer_a")
	peerAEvent.PaneID = strPtr("wB:p2")
	// Peer B finished with an empty assistant text: permanently suppressed noise.
	peerBEvent := makeDoneEvent(105, "", "term_peer_b")
	peerBEvent.AgentID = strPtr("ag_peer_b")
	peerBEvent.PaneID = strPtr("wB:p3")
	// What the daemon still re-lists: after 300s only the freshly pending row 105
	// is there, the delivered-but-unconsumed row 100 is gone.
	listedEvents := []any{peerAEvent}

	deferClient := &concClient{}
	deferClient.respond = func(method string, params any) (any, error) {
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
				selfAgent,
				agentListEntry("pi", peerAStatus, "2026-10-25T05:06:00.000Z", "wB:p2", "term_peer_a"),
				agentListEntry("pi", "idle", "2026-10-25T05:06:00.000Z", "wB:p3", "term_peer_b"),
			}}, nil
		}
		live := []any{}
		for _, event := range listedEvents {
			if event.(observability.AgentEventRecord).ID > ackedEventID {
				live = append(live, event)
			}
		}
		return orchestratorStateResponse(deferSessionID, live, ackedEventID), nil
	}
	deferOptions := func() agy.RunAgyHookOptions {
		return agy.RunAgyHookOptions{
			Connect:     deferClient.connect,
			Environment: herdrEnv,
			SocketPath:  "/tmp/herdsman.sock",
			SpawnWakeWaiter: func(agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
				return &agy.SpawnWakeWaiterResult{PID: intPtr(os.Getpid())}
			},
			StateDir: strPtr(deferDir),
		}
	}
	runStop := func() agy.HookOutput {
		return agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+deferSessionID+`","terminationReason":"NO_TOOL_CALL"}`, deferOptions())
	}

	// 1. Peer A is `working`: event 100 is deferred and gets handed to the daemon
	//    as a delivered row by this very poll.
	if first := runStop(); !isEmptyHookOutput(first) {
		t.Fatalf("first stop = %+v, want {}", first)
	}
	assertStateMatches(t, readStateObject(t, deferStatePath), map[string]any{
		"deferredEventIds": []any{float64(100)},
	})

	// 2. More than 300s later: the daemon dropped the delivered row 100 and now
	//    only reports the freshly pending noise event 105.
	deferClient.mu.Lock()
	listedEvents = []any{peerBEvent}
	deferClient.mu.Unlock()
	if second := runStop(); !isEmptyHookOutput(second) {
		t.Fatalf("second stop = %+v, want {}", second)
	}
	// Without the retained snapshot, 105 would be acked right here (nothing holds
	// the cursor any more) and settle `id <= 105` over the deferred 100.
	deferClient.mu.Lock()
	ackedSoFar := append([]int64{}, ackedIDs...)
	deferClient.mu.Unlock()
	if len(ackedSoFar) != 0 {
		t.Fatalf("ackedIDs = %v, want none", ackedSoFar)
	}
	assertStateMatches(t, readStateObject(t, deferStatePath), map[string]any{
		"deferredEventIds":   []any{float64(100)},
		"pendingAckEventIds": []any{float64(105)},
		"presentedEventIds":  []any{float64(105)},
	})

	// 3. Peer A stops reporting working: the stale-looking event is still there
	//    and gets injected, and 105 still may not ack past it.
	deferClient.mu.Lock()
	peerAStatus = "done"
	deferClient.mu.Unlock()
	settleStop := runStop()
	if settleStop.Decision == nil || *settleStop.Decision != "continue" {
		t.Fatalf("settle stop = %+v, want decision continue", settleStop)
	}
	if settleStop.Reason == nil || !strings.Contains(*settleStop.Reason, "工人 A 的最终结论。") {
		t.Fatalf("settle stop reason = %v, want the peer A completion", settleStop.Reason)
	}
	deferClient.mu.Lock()
	ackedSoFar = append([]int64{}, ackedIDs...)
	deferClient.mu.Unlock()
	if len(ackedSoFar) != 0 {
		t.Fatalf("ackedIDs = %v, want none", ackedSoFar)
	}
	assertStateMatches(t, readStateObject(t, deferStatePath), map[string]any{
		"deliveredEventIds":  []any{float64(100)},
		"pendingAckEventIds": []any{float64(105)},
	})

	// 4. Both are consumed, so nothing blocks the cursor any more.
	if fourth := runStop(); !isEmptyHookOutput(fourth) {
		t.Fatalf("fourth stop = %+v, want {}", fourth)
	}
	deferClient.mu.Lock()
	ackedSoFar = append([]int64{}, ackedIDs...)
	deferClient.mu.Unlock()
	if !reflect.DeepEqual(ackedSoFar, []int64{100, 105}) {
		t.Fatalf("ackedIDs = %v, want [100 105]", ackedSoFar)
	}
	assertStateMatches(t, readStateObject(t, deferStatePath), map[string]any{
		"deliveredEventIds":  []any{},
		"pendingAckEventIds": []any{},
		// Acked and presented: the snapshot entry is gone for good.
		"retainedCandidateEvents": []any{},
	})
}

// --- an ignored decision:continue batch survives repeated peeks (3985333) --

// TestIgnoredContinueBatchSurvivesRepeatedWaiterPeeks mirrors the TS "a batch the
// host ignored survives repeated waiter peeks past the redelivery window and is
// still injected after the wake prompt": the Stop hook marks the batch presented
// *and* records it in deliveredEventIds, so the snapshot has to keep it even
// though it is no longer a candidate.
func TestIgnoredContinueBatchSurvivesRepeatedWaiterPeeks(t *testing.T) {
	dir := t.TempDir()
	sessionID := "f4444444-2222-3333-4444-555555555555"
	statePath := sessionStatePath(dir, sessionID)
	// `Stop` hands event 9500 to the host through `decision: continue`; the host
	// ignores it, so the WakeWaiter is the only one left to surface the batch.
	// After 300s the daemon stops re-listing the delivered row, leaving the
	// persisted snapshot as the only source of the record.
	peerEvent := makeDoneEvent(9500, "被忽略的批次结论。")
	daemonListsEvent := true
	composerHasDraft := true
	wakePrompts := []wakePrompt{}
	warns := []string{}

	emptyComposer := strings.Join([]string{
		"assistant output",
		"───────────────────────────────────────────",
		">",
		"───────────────────────────────────────────",
		"? for shortcuts",
	}, "\n")
	draftComposer := strings.Join([]string{
		"assistant output",
		"───────────────────────────────────────────",
		"> 用户正在打的草稿",
		"───────────────────────────────────────────",
		"? for shortcuts",
	}, "\n")

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.list" {
			return map[string]any{"agents": []any{
				agentListEntry("agy", "idle", "2026-10-21T11:50:00.000Z", "wB:p1", "term_self"),
				agentListEntry("pi", "done", "2026-10-21T11:50:00.000Z", "wB:p2", "term_peer"),
			}}, nil
		}
		events := []any{}
		if daemonListsEvent {
			events = append(events, peerEvent)
		}
		return orchestratorStateResponse(sessionID, events, 0), nil
	}
	collectWarn := func(message string) {
		client.mu.Lock()
		warns = append(warns, message)
		client.mu.Unlock()
	}

	stopResult := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, agy.RunAgyHookOptions{
		Connect:     client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		SpawnWakeWaiter: func(agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
			return &agy.SpawnWakeWaiterResult{PID: intPtr(os.Getpid())}
		},
		StateDir: strPtr(dir),
	})
	if stopResult.Decision == nil || *stopResult.Decision != "continue" {
		t.Fatalf("stop result = %+v, want decision continue", stopResult)
	}
	if stopResult.Reason == nil || !strings.Contains(*stopResult.Reason, "被忽略的批次结论。") {
		t.Fatalf("stop reason = %v, want the ignored batch text", stopResult.Reason)
	}
	// The batch is presented (handed to the host) *and* delivered-but-unacked,
	// so its record is retained even though it is no longer a candidate.
	assertStateMatches(t, readStateObject(t, statePath), map[string]any{
		"deliveredEventIds": []any{float64(9500)},
		"presentedEventIds": []any{float64(9500)},
	})
	assertRetainedIDs(t, readStateObject(t, statePath), []int64{9500})

	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connectCapturing,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(20),
			ReadPaneText: func(string) (*string, error) {
				if composerHasDraft {
					return &draftComposer, nil
				}
				return &emptyComposer, nil
			},
			SocketPath: "/tmp/herdsman.sock",
			StateDir:   strPtr(dir),
			TriggerWakePrompt: func(paneID, promptText string) error {
				client.mu.Lock()
				wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
				client.mu.Unlock()
				return nil
			},
			WaitTimeoutMs: floatPtr(5000),
			Warn:          collectWarn,
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		client.mu.Lock()
		deferred := anyWarnContains(warns, "输入框有草稿，推迟叫醒")
		client.mu.Unlock()
		if deferred || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	client.mu.Lock()
	sawDraftDefer := anyWarnContains(warns, "输入框有草稿，推迟叫醒")
	client.mu.Unlock()
	if !sawDraftDefer {
		t.Fatalf("warns = %v, want the composer-draft deferral", warns)
	}

	// The user keeps typing past the redelivery window while the waiter peeks
	// over the batch again and again. `candidateEvents` is empty from the first
	// peek on (the id is presented), and that must not clear the snapshot: the
	// pre-fix behaviour wiped it here and lost the batch for good.
	client.mu.Lock()
	daemonListsEvent = false
	client.mu.Unlock()
	time.Sleep(120 * time.Millisecond)
	client.mu.Lock()
	earlyPrompts := append([]wakePrompt{}, wakePrompts...)
	client.mu.Unlock()
	if len(earlyPrompts) != 0 {
		t.Fatalf("wakePrompts = %+v while the draft persists, want none", earlyPrompts)
	}
	assertRetainedIDs(t, readStateObject(t, statePath), []int64{9500})

	// Draft gone: the waiter re-arms the delivered batch and prompts.
	client.mu.Lock()
	composerHasDraft = false
	client.mu.Unlock()
	if result := awaitHookOutput(t, done); !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	client.mu.Lock()
	prompts := append([]wakePrompt{}, wakePrompts...)
	client.mu.Unlock()
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(prompts, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", prompts, want)
	}
	assertStateMatches(t, readStateObject(t, statePath), map[string]any{
		"deliveredEventIds": []any{float64(9500)},
		"presentedEventIds": []any{},
	})
	assertRetainedIDs(t, readStateObject(t, statePath), []int64{9500})

	// The prompt starts the turn: `PreInvocation` has to rebuild the batch from
	// the snapshot, because `agent.orchestrator.get` no longer returns it.
	preOut := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":2,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:      client.connect,
		Environment:  herdrEnv,
		ReadPaneText: func(string) (*string, error) { return nil, nil },
		SocketPath:   "/tmp/herdsman.sock",
		StateDir:     strPtr(dir),
	})
	if len(preOut.InjectSteps) == 0 || !strings.Contains(preOut.InjectSteps[0].EphemeralMessage, "被忽略的批次结论。") {
		t.Fatalf("preOut.InjectSteps = %+v, want the retained batch", preOut.InjectSteps)
	}
	assertStateMatches(t, readStateObject(t, statePath), map[string]any{
		"deliveredEventIds": []any{float64(9500)},
		"presentedEventIds": []any{float64(9500)},
	})
}

// assertRetainedIDs mirrors the TS
// `expect(state.retainedCandidateEvents).toEqual(ids.map((id) => expect.objectContaining({ id })))`.
func assertRetainedIDs(t *testing.T, state map[string]any, want []int64) {
	t.Helper()
	raw, ok := state["retainedCandidateEvents"]
	if !ok {
		if len(want) == 0 {
			return
		}
		t.Fatalf("retainedCandidateEvents is missing, want %v", want)
	}
	entries, ok := raw.([]any)
	if !ok {
		t.Fatalf("retainedCandidateEvents = %v, want a list", raw)
	}
	if len(entries) != len(want) {
		t.Fatalf("retainedCandidateEvents has %d entries (%v), want %v", len(entries), raw, want)
	}
	for index, id := range want {
		entry, ok := entries[index].(map[string]any)
		if !ok {
			t.Fatalf("retainedCandidateEvents[%d] = %v, want an object", index, entries[index])
		}
		if got, ok := entry["id"].(float64); !ok || int64(got) != id {
			t.Fatalf("retainedCandidateEvents[%d].id = %v, want %d", index, entry["id"], id)
		}
	}
}

// --- retained-row eligibility: closed pane / taken-over sibling (3985333) --

// TestRetainedRowsOfClosedPaneAndTakenOverSiblingAreDropped mirrors the TS
// "retained rows whose source pane closed or whose sibling workspace was taken
// over are dropped, not injected and not holding the ack cursor": a row that only
// the snapshot knows is validated against the live agent.list before it is
// injected or allowed to hold the acknowledgement cursor back.
func TestRetainedRowsOfClosedPaneAndTakenOverSiblingAreDropped(t *testing.T) {
	dir := t.TempDir()
	sessionID := "f6666666-2222-3333-4444-555555555555"
	statePath := sessionStatePath(dir, sessionID)

	// Only in the snapshot: its pane has been closed, so the row was reclaimed
	// and the terminal left `agent.list` for good.
	closedPaneEvent := makeDoneEvent(400, "已关闭窗格的旧结论。", "term_gone")
	// Only in the snapshot: the sibling workspace now has an orchestrator owner
	// of its own, so it is no longer part of this owner's ownerless scope.
	takenOverSiblingEvent := makeDoneEvent(410, "被接管兄弟工作区的旧结论。", "term_sibling")
	takenOverSiblingEvent.PaneID = strPtr("wC:p9")
	takenOverSiblingEvent.WorkspaceID = strPtr("wC")
	// Still open, still this workspace: the peer of the delivered row 420 lives on.
	livePeerEvent := makeDoneEvent(420, "在线工人的结论。", "term_live")

	// All three rows left the daemon's redelivery window: none of them comes back
	// through `agent.orchestrator.get`, so only the eligibility check below can
	// tell the two dead rows apart from the live one.
	listedEvents := []any{}
	ackedEventID := int64(0)
	ackedIDs := []int64{}
	rejectAcks := false

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		switch method {
		case "agent.notifications.ack":
			eventID := asEventID(params.(map[string]any)["eventId"])
			if rejectAcks {
				return nil, errors.New(`{"code":"ORCHESTRATOR_EVENT_INVALIDATED","message":"event invalidated"}`)
			}
			ackedIDs = append(ackedIDs, eventID)
			if eventID > ackedEventID {
				ackedEventID = eventID
			}
			return map[string]any{"acknowledged": true}, nil
		case "agent.list":
			return map[string]any{"agents": []any{
				agentListEntry("agy", "idle", "2026-10-21T11:50:00.000Z", "wB:p1", "term_self"),
				agentListEntry("pi", "done", "2026-10-21T11:50:00.000Z", "wB:p7", "term_live"),
			}}, nil
		}
		live := []any{}
		for _, event := range listedEvents {
			if event.(observability.AgentEventRecord).ID > ackedEventID {
				live = append(live, event)
			}
		}
		return orchestratorStateResponse(sessionID, live, ackedEventID), nil
	}
	seed := func() {
		writeStateFile(t, statePath, map[string]any{
			"deliveredEventIds":       []any{float64(420)},
			"pendingAckEventIds":      []any{},
			"presentedEventIds":       []any{float64(420)},
			"retainedCandidateEvents": []any{closedPaneEvent, takenOverSiblingEvent, livePeerEvent},
			"turnEpoch":               float64(0),
		})
	}
	runStop := func() agy.HookOutput {
		return agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, agy.RunAgyHookOptions{
			Connect:     client.connect,
			Environment: herdrEnv,
			SocketPath:  "/tmp/herdsman.sock",
			SpawnWakeWaiter: func(agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
				return &agy.SpawnWakeWaiterResult{PID: intPtr(os.Getpid())}
			},
			StateDir: strPtr(dir),
		})
	}

	// 1. The daemon rejects the ack (the row was invalidated), so the cursor stays
	//    at 0 and the two dead rows keep showing up. They must be dropped by the
	//    eligibility check instead of being injected as outcomes — the pre-fix
	//    behaviour woke the orchestrator with a closed pane's text.
	client.mu.Lock()
	rejectAcks = true
	client.mu.Unlock()
	seed()
	if first := runStop(); !isEmptyHookOutput(first) {
		t.Fatalf("first stop = %+v, want {}", first)
	}
	assertStateMatches(t, readStateObject(t, statePath), map[string]any{
		"deliveredEventIds":  []any{},
		"pendingAckEventIds": []any{},
	})
	// Both stale records are gone; only the still-open peer's row was a candidate
	// and it was already handed over.
	assertRetainedIDs(t, readStateObject(t, statePath), nil)

	// 2. Same snapshot, but the ack of the delivered row succeeds: a dead low id
	//    must not hold the cursor back, so 420 is acknowledged right away (pre-fix
	//    it stayed in pendingAckEventIds forever) and nothing stale is injected on
	//    top of it.
	client.mu.Lock()
	rejectAcks = false
	ackedIDs = []int64{}
	ackedEventID = 0
	client.mu.Unlock()
	seed()
	if second := runStop(); !isEmptyHookOutput(second) {
		t.Fatalf("second stop = %+v, want {}", second)
	}
	client.mu.Lock()
	ackedSoFar := append([]int64{}, ackedIDs...)
	cursor := ackedEventID
	client.mu.Unlock()
	if !reflect.DeepEqual(ackedSoFar, []int64{420}) {
		t.Fatalf("ackedIDs = %v, want [420]", ackedSoFar)
	}
	if cursor != 420 {
		t.Fatalf("ackedEventID = %d, want 420", cursor)
	}
	assertStateMatches(t, readStateObject(t, statePath), map[string]any{
		"deliveredEventIds":  []any{},
		"pendingAckEventIds": []any{},
	})
	assertRetainedIDs(t, readStateObject(t, statePath), nil)

	// 3. A row the daemon re-listed is kept unconditionally, even for a peer this
	//    `agent.list` response does not report: the daemon just validated its
	//    scope and deliverability (a `retained invalidated` row inside the grace
	//    window is exactly this case).
	client.mu.Lock()
	listedEvents = []any{closedPaneEvent}
	ackedEventID = 0
	ackedIDs = []int64{}
	client.mu.Unlock()
	writeStateFile(t, statePath, map[string]any{"turnEpoch": float64(0)})
	listedStop := runStop()
	if listedStop.Reason == nil || !strings.Contains(*listedStop.Reason, "已关闭窗格的旧结论。") {
		t.Fatalf("listed stop reason = %v, want the closed-pane row", listedStop.Reason)
	}
	assertStateMatches(t, readStateObject(t, statePath), map[string]any{
		"deliveredEventIds": []any{float64(400)},
		"presentedEventIds": []any{float64(400)},
	})
	assertRetainedIDs(t, readStateObject(t, statePath), []int64{400})
}
