package agy_test

// This file ports the WakeWaiter half of test/unit/agy-hook-and-cli.test.ts
// whose full bodies fall within lines 497-1129 (the second Step-3 slice,
// 651-1260): the Stop→WakeWaiter cycle, the silent suppressed-ack, the
// waiterPid/turnEpoch race, the wake-prompt-failure/timeout case and the
// unsettled-peer settle-grace case. The "resets settleDeadline ..." case that
// starts at line 1130 but runs past 1260 belongs to the next sub-batch.
//
// The TS cases drive a suspended waiter with vi fake timers; the Go waiter is a
// synchronous, real-time loop, so each case runs RunAgyHook in a goroutine and
// coordinates through the notification seam and a mutex-guarded fake client,
// asserting only the states that are deterministic regardless of scheduling.

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/agy"
)

// concClient is a mutex-guarded fake RPC client for the cases where a waiter
// goroutine and the test body share it.
type concClient struct {
	mu      sync.Mutex
	calls   []recordedCall
	respond func(method string, params any) (any, error)
	notif   func(method string, params any)
}

func (c *concClient) Close() {}

func (c *concClient) Call(method string, params any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, recordedCall{method: method, params: params})
	return c.respond(method, params)
}

// connect captures the hook's notification handler (so the test can push an
// agent.event) and hands back the shared client.
func (c *concClient) connect(string, func(string, any)) (agy.RpcClient, error) {
	c.mu.Lock()
	c.notif = nil
	c.mu.Unlock()
	return c, nil
}

// captureNotif records the notification handler passed to connect. It is used
// as the connect func for clients that must observe pushes.
func (c *concClient) connectCapturing(_ string, onNotification func(method string, params any)) (agy.RpcClient, error) {
	c.mu.Lock()
	c.notif = onNotification
	c.mu.Unlock()
	return c, nil
}

// push delivers an agent.event notification to the waiter, mirroring
// `pushNotification?.("agent.event", { event })`.
func (c *concClient) push(event any) {
	c.mu.Lock()
	notif := c.notif
	c.mu.Unlock()
	if notif != nil {
		notif("agent.event", map[string]any{"event": event})
	}
}

func (c *concClient) callMethods() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return methodsOf(c.calls)
}

type wakePrompt struct {
	paneID     string
	promptText string
}

func floatPtr(value float64) *float64 { return &value }

// readStateObject reads and parses a session state file for toMatchObject-style
// assertions (JSON numbers arrive as float64).
func readStateObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	object := map[string]any{}
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func assertStateMatches(t *testing.T, object map[string]any, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if !reflect.DeepEqual(object[key], value) {
			t.Fatalf("state[%q] = %v, want %v", key, object[key], value)
		}
	}
}

func awaitHookOutput(t *testing.T, done <-chan agy.HookOutput) agy.HookOutput {
	t.Helper()
	select {
	case output := <-done:
		return output
	case <-time.After(15 * time.Second):
		t.Fatal("WakeWaiter did not return in time")
		return agy.HookOutput{}
	}
}

// --- Stop → WakeWaiter cycle (lines 497-658) --------------------------------

func TestStopSpawnsWaiterAndWakeWaiterWakesIdleMainPane(t *testing.T) {
	dir := t.TempDir()
	sessionID := "33333333-2222-3333-4444-555555555555"

	var pendingEvents []any
	var selfStatus = "working"
	var peerStatus = "working"
	var ackedEventID int64
	ackedIDs := []int64{}
	orchestratorGetCalls := 0
	spawnedWaiters := []agy.SpawnWakeWaiterInput{}
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
				map[string]any{"agent": "agy", "agentStatus": selfStatus, "firstSeenAt": "2026-10-21T11:50:00.000Z", "herdrSessionName": "default", "id": "ag_self", "paneId": "wB:p1", "terminalId": "term_self", "workspaceId": "wB"},
				map[string]any{"agent": "pi", "agentStatus": peerStatus, "firstSeenAt": "2026-10-21T11:50:00.000Z", "herdrSessionName": "default", "id": "ag_peer", "paneId": "wB:p2", "terminalId": "term_peer", "workspaceId": "wB"},
			}}, nil
		case "agent.orchestrator.get":
			orchestratorGetCalls++
		}
		return map[string]any{
			"events": pendingEvents,
			"presence": map[string]any{
				"herdrSessionName": "default", "paneId": "wB:p1", "subscriberId": "agy:" + sessionID,
				"terminalId": "term_self", "workspaceId": "wB",
			},
			"state": map[string]any{
				"ackedEventId": ackedEventID, "herdrSessionName": "default",
				"owner":     map[string]any{"paneId": "wB:p1", "terminalId": "term_self"},
				"updatedAt": "2026-10-21T12:00:00.000Z", "workspaceId": "wB",
			},
		}, nil
	}

	spawnWakeWaiter := func(input agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
		client.mu.Lock()
		spawnedWaiters = append(spawnedWaiters, input)
		client.mu.Unlock()
		pid := os.Getpid()
		return &agy.SpawnWakeWaiterResult{PID: &pid}
	}

	// 1. Stop sees an active peer, writes the epochs, spawns the waiter and
	// returns {} immediately.
	stopResult := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, agy.RunAgyHookOptions{
		Connect:         client.connect,
		Environment:     herdrEnv,
		SocketPath:      "/tmp/herdsman.sock",
		SpawnWakeWaiter: spawnWakeWaiter,
		StateDir:        strPtr(dir),
	})
	if !isEmptyHookOutput(stopResult) {
		t.Fatalf("stop result = %+v, want {}", stopResult)
	}
	if len(spawnedWaiters) != 1 || spawnedWaiters[0].SessionID != sessionID ||
		spawnedWaiters[0].StateDir != dir || spawnedWaiters[0].WaiterEpoch != 1 {
		t.Fatalf("spawnedWaiters = %+v, want one with waiterEpoch 1", spawnedWaiters)
	}
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})

	// 2. WakeWaiter runs; while the main pane is working it must not call
	// agent.orchestrator.get nor wake.
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
		})
	}()

	time.Sleep(40 * time.Millisecond)
	client.mu.Lock()
	pendingEvents = []any{makeDoneEvent(25, "Subagent finished implementation and tests pass.")}
	client.mu.Unlock()
	client.push(makeDoneEvent(25, "Subagent finished implementation and tests pass."))

	client.mu.Lock()
	getsWhileWorking := orchestratorGetCalls
	promptsWhileWorking := len(wakePrompts)
	client.mu.Unlock()
	if getsWhileWorking != 0 {
		t.Fatalf("orchestrator.get called %d times while working, want 0", getsWhileWorking)
	}
	if promptsWhileWorking != 0 {
		t.Fatalf("wakePrompts = %v while working, want none", wakePrompts)
	}

	// Once the main pane is idle, the waiter inspects, wakes and exits.
	client.mu.Lock()
	selfStatus = "idle"
	peerStatus = "idle"
	client.mu.Unlock()
	waiterResult := awaitHookOutput(t, done)
	if !isEmptyHookOutput(waiterResult) {
		t.Fatalf("waiter result = %+v, want {}", waiterResult)
	}

	client.mu.Lock()
	getsAfterIdle := orchestratorGetCalls
	promptsAfterIdle := append([]wakePrompt{}, wakePrompts...)
	ackedAfterIdle := append([]int64{}, ackedIDs...)
	client.mu.Unlock()
	if getsAfterIdle == 0 {
		t.Fatal("orchestrator.get was never called after the pane went idle")
	}
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(promptsAfterIdle, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", promptsAfterIdle, want)
	}
	if len(ackedAfterIdle) != 0 {
		t.Fatalf("ackedIDs = %v, want none", ackedAfterIdle)
	}

	// 3. The next PreInvocation injects the pending event (still unacked).
	preResult := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	if len(preResult.InjectSteps) != 1 ||
		!strings.Contains(preResult.InjectSteps[0].EphemeralMessage, "Subagent finished implementation and tests pass.") {
		t.Fatalf("preResult = %+v, want the pending event injected", preResult)
	}

	// 4. The final Stop acks the event in order.
	finalStop := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, agy.RunAgyHookOptions{
		Connect:         client.connect,
		Environment:     herdrEnv,
		SocketPath:      "/tmp/herdsman.sock",
		SpawnWakeWaiter: spawnWakeWaiter,
		StateDir:        strPtr(dir),
	})
	if !isEmptyHookOutput(finalStop) {
		t.Fatalf("finalStop = %+v, want {}", finalStop)
	}
	client.mu.Lock()
	ackedFinal := append([]int64{}, ackedIDs...)
	client.mu.Unlock()
	if want := []int64{25}; !reflect.DeepEqual(ackedFinal, want) {
		t.Fatalf("ackedIDs = %v, want %v", ackedFinal, want)
	}
}

// --- WakeWaiter silently acks suppressed events (lines 659-732) -------------

func TestWakeWaiterSilentlyAcksSuppressedEvents(t *testing.T) {
	dir := t.TempDir()
	sessionID := "44444444-2222-3333-4444-555555555555"
	ackedIDs := []int64{}
	var ackedEventID int64
	wakePrompts := []wakePrompt{}

	// Event 30 is an upstream 429 failure; event 31 is a completed event with a
	// whitespace-only excerpt. Both are suppressed by the projection and must be
	// acked without a wake prompt.
	suppressed := []any{
		func() any {
			event := makeDoneEvent(30, "")
			event.Type = "agent.failed"
			event.Payload = map[string]any{"agent": "pi", "name": "role-worker-1", "reason": "429 rate limit exceeded"}
			return event
		}(),
		makeDoneEvent(31, "   "),
	}

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
			return map[string]any{"agents": []any{}}, nil
		}
		return map[string]any{
			"events": suppressed,
			"presence": map[string]any{
				"herdrSessionName": "default", "paneId": "wB:p1", "subscriberId": "agy:" + sessionID,
				"terminalId": "term_self", "workspaceId": "wB",
			},
			"state": map[string]any{
				"ackedEventId": ackedEventID, "herdrSessionName": "default",
				"owner":     map[string]any{"paneId": "wB:p1", "terminalId": "term_self"},
				"updatedAt": "2026-10-21T12:00:00.000Z", "workspaceId": "wB",
			},
		}, nil
	}

	result := agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
		Connect: client.connect,
		Environment: mergeEnv(herdrEnv,
			"HERDSMAN_AGY_SESSION_ID", sessionID,
			"HERDSMAN_AGY_STATE_DIR", dir,
			"HERDSMAN_AGY_WAITER_EPOCH", "1"),
		SocketPath: "/tmp/herdsman.sock",
		StateDir:   strPtr(dir),
		// readPaneText: no real `herdr pane read` in the fake-client cases.
		ReadPaneText: func(string) (*string, error) { return nil, nil },
		TriggerWakePrompt: func(paneID, promptText string) error {
			wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
			return nil
		},
		WaitTimeoutMs: floatPtr(1000),
	})

	if !isEmptyHookOutput(result) {
		t.Fatalf("result = %+v, want {}", result)
	}
	if want := []int64{30, 31}; !reflect.DeepEqual(ackedIDs, want) {
		t.Fatalf("ackedIDs = %v, want %v", ackedIDs, want)
	}
	if len(wakePrompts) != 0 {
		t.Fatalf("wakePrompts = %+v, want none", wakePrompts)
	}
}

// sessionStatePath mirrors stateFilePath for the assertions that read the file.
func sessionStatePath(stateDir, sessionID string) string {
	return stateDir + "/" + sanitizeSessionID(sessionID) + ".json"
}

// writeStateFile seeds a session state file atomically (temp + rename) so a
// concurrently looping waiter never reads a half-written file.
func writeStateFile(t *testing.T, path string, object map[string]any) {
	t.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".test-tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// blockForever is a channel that is never signalled; the hung-client Call
// receives from it to model `request: () => new Promise(() => undefined)`.
var blockForever = make(chan struct{})

// hungRpcClient models a client whose Call never returns; its Close records the
// fact so the test can assert the request-timeout wrapper closed it.
type hungRpcClient struct {
	mu     *sync.Mutex
	closed *bool
}

func (c *hungRpcClient) Close() {
	c.mu.Lock()
	*c.closed = true
	c.mu.Unlock()
}

func (c *hungRpcClient) Call(string, any) (any, error) {
	<-blockForever
	return nil, nil
}

func sanitizeSessionID(sessionID string) string {
	var builder strings.Builder
	for _, current := range sessionID {
		if (current >= 'a' && current <= 'z') || (current >= 'A' && current <= 'Z') ||
			(current >= '0' && current <= '9') || current == '.' || current == '_' || current == '-' {
			builder.WriteRune(current)
			continue
		}
		builder.WriteByte('_')
	}
	return builder.String()
}

// --- waiterPid write ordering / turnEpoch race (lines 733-834) --------------

func TestWakeWaiterExitsWithoutWakingWhenTurnEpochAdvances(t *testing.T) {
	dir := t.TempDir()
	sessionID := "55555555-2222-3333-4444-555555555555"
	wakePrompts := []wakePrompt{}
	var pendingEvents []any

	// Stop wrote turnEpoch=1 & waiterEpoch=1 with a stale waiterPid before the
	// real pid was flushed.
	writeStateFile(t, sessionStatePath(dir, sessionID), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid() + 999),
	})

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.list" {
			return map[string]any{"agents": []any{
				map[string]any{"agent": "pi", "agentStatus": "working", "firstSeenAt": "2026-10-21T12:00:00.000Z", "herdrSessionName": "default", "id": "ag_peer", "paneId": "wB:p2", "terminalId": "term_peer", "workspaceId": "wB"},
			}}, nil
		}
		return map[string]any{
			"events":   pendingEvents,
			"presence": map[string]any{"herdrSessionName": "default", "paneId": "wB:p1", "subscriberId": "agy:" + sessionID, "terminalId": "term_self", "workspaceId": "wB"},
			"state":    map[string]any{"ackedEventId": 0, "herdrSessionName": "default", "owner": map[string]any{"paneId": "wB:p1", "terminalId": "term_self"}, "updatedAt": "2026-10-21T12:00:00.000Z", "workspaceId": "wB"},
		}, nil
	}

	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connectCapturing,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(50),
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
		})
	}()

	time.Sleep(20 * time.Millisecond)
	// Stop finishes writing waiterPid, then the user starts a new turn, which
	// advances turnEpoch to 2 via PreInvocation.
	writeStateFile(t, sessionStatePath(dir, sessionID), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})
	agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	client.mu.Lock()
	pendingEvents = []any{makeDoneEvent(35, "Late subagent result after user already started turn.")}
	client.mu.Unlock()
	client.push(makeDoneEvent(35, "Late subagent result after user already started turn."))

	result := awaitHookOutput(t, done)
	if !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(wakePrompts) != 0 {
		t.Fatalf("wakePrompts = %+v, want none", wakePrompts)
	}
}

// --- wake-prompt failure and RPC timeout (lines 835-934) --------------------

func TestWakeWaiterWarnsOnWakePromptFailureAndPreservesEvents(t *testing.T) {
	dir := t.TempDir()
	sessionID := "66666666-2222-3333-4444-555555555555"
	warns := []string{}
	wakeAttempts := 0
	pendingEvents := []any{makeDoneEvent(40, "Worker completed changes.")}

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.list" {
			return map[string]any{"agents": []any{}}, nil
		}
		return map[string]any{
			"events":   pendingEvents,
			"presence": map[string]any{"herdrSessionName": "default", "paneId": "wB:p1", "subscriberId": "agy:" + sessionID, "terminalId": "term_self", "workspaceId": "wB"},
			"state":    map[string]any{"ackedEventId": 0, "herdrSessionName": "default", "owner": map[string]any{"paneId": "wB:p1", "terminalId": "term_self"}, "updatedAt": "2026-10-21T12:00:00.000Z", "workspaceId": "wB"},
		}, nil
	}

	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connect,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(100),
			SocketPath:     "/tmp/herdsman.sock",
			StateDir:       strPtr(dir),
			// readPaneText: no real `herdr pane read` in the fake-client cases.
			ReadPaneText: func(string) (*string, error) { return nil, nil },
			TriggerWakePrompt: func(paneID, promptText string) error {
				client.mu.Lock()
				wakeAttempts++
				client.mu.Unlock()
				return errWakePromptFailed
			},
			WaitTimeoutMs: floatPtr(5000),
			Warn: func(message string) {
				client.mu.Lock()
				warns = append(warns, message)
				client.mu.Unlock()
			},
		})
	}()

	if result := awaitHookOutput(t, done); !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	client.mu.Lock()
	attempts := wakeAttempts
	warnText := strings.Join(warns, "\n")
	client.mu.Unlock()
	if attempts != 3 {
		t.Fatalf("wakeAttempts = %d, want 3", attempts)
	}
	if !strings.Contains(warnText, "Herdsman agy wake prompt failed (attempt 3/3)") {
		t.Fatalf("warns = %q, want the attempt 3/3 failure", warnText)
	}

	// Event 40 remains unconsumed and is delivered on the next PreInvocation.
	preOut := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	if len(preOut.InjectSteps) != 1 || !strings.Contains(preOut.InjectSteps[0].EphemeralMessage, "Worker completed changes.") {
		t.Fatalf("preOut = %+v, want event 40 injected", preOut)
	}

	// A hung request is closed by requestTimeoutMs and the waiter exits cleanly.
	var closedHung bool
	hung := &hungRpcClient{mu: &client.mu, closed: &closedHung}
	hungResult := agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
		Connect: func(string, func(string, any)) (agy.RpcClient, error) { return hung, nil },
		Environment: mergeEnv(herdrEnv,
			"HERDSMAN_AGY_SESSION_ID", sessionID,
			"HERDSMAN_AGY_STATE_DIR", dir,
			"HERDSMAN_AGY_WAITER_EPOCH", "2"),
		RequestTimeoutMs: floatPtr(50),
		SocketPath:       "/tmp/herdsman.sock",
		StateDir:         strPtr(dir),
		Warn: func(message string) {
			client.mu.Lock()
			warns = append(warns, message)
			client.mu.Unlock()
		},
	})
	if !isEmptyHookOutput(hungResult) {
		t.Fatalf("hung waiter result = %+v, want {}", hungResult)
	}
	if !closedHung {
		t.Fatal("the hung client was not closed by the request timeout")
	}
	client.mu.Lock()
	warnText = strings.Join(warns, "\n")
	client.mu.Unlock()
	if !strings.Contains(warnText, "Herdsman agy WakeWaiter RPC request timed out after 50ms") {
		t.Fatalf("warns = %q, want the 50ms request timeout", warnText)
	}
}

// errWakePromptFailed mirrors the TS `throw new Error("herdr prompt timed out")`.
var errWakePromptFailed = errors.New("herdr prompt timed out")

// --- unsettled peer turn / settle grace (lines 935-1129) --------------------

func TestStopDetectsUnsettledPeerAndWaiterStaysAliveAcrossSettleGrace(t *testing.T) {
	dir := t.TempDir()
	sessionID := "77777777-2222-3333-4444-555555555555"
	var peerLastAssistantMessage any
	var pendingEvents []any
	wakePrompts := []wakePrompt{}
	spawnedEpochs := []float64{}

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.list" {
			return map[string]any{"agents": []any{
				map[string]any{
					"agent": "agy", "agentStatus": "done", "firstSeenAt": "2026-10-22T11:50:00.000Z", "herdrSessionName": "default",
					"history": map[string]any{
						"lastAssistantMessage": map[string]any{"ref": "self-asst-1", "text": "Dispatched subagent.", "timestamp": nil},
						"lastUserMessage":      map[string]any{"ref": "self-user-1", "text": "Dispatch subagent.", "timestamp": nil},
						"source":               "antigravity-sqlite", "updatedAt": nil,
					},
					"id": "ag_self", "paneId": "wB:p1", "terminalId": "term_self", "workspaceId": "wB",
				},
				map[string]any{
					"agent": "agy", "agentStatus": "done", "firstSeenAt": "2026-10-22T11:55:00.000Z", "herdrSessionName": "default",
					"history": map[string]any{
						"lastAssistantMessage": peerLastAssistantMessage,
						"lastUserMessage":      map[string]any{"ref": "peer-user-1", "text": "Verify the implementation", "timestamp": "2026-10-22T11:59:30.000Z"},
						"source":               "antigravity-sqlite", "updatedAt": nil,
					},
					"id": "ag_peer", "paneId": "wB:p2", "terminalId": "term_peer", "workspaceId": "wB",
				},
			}}, nil
		}
		return map[string]any{
			"events":   pendingEvents,
			"presence": map[string]any{"herdrSessionName": "default", "paneId": "wB:p1", "subscriberId": "agy:" + sessionID, "terminalId": "term_self", "workspaceId": "wB"},
			"state":    map[string]any{"ackedEventId": 0, "herdrSessionName": "default", "owner": map[string]any{"paneId": "wB:p1", "terminalId": "term_self"}, "updatedAt": "2026-10-22T12:00:00.000Z", "workspaceId": "wB"},
		}, nil
	}
	spawnWakeWaiter := func(input agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
		client.mu.Lock()
		spawnedEpochs = append(spawnedEpochs, input.WaiterEpoch)
		client.mu.Unlock()
		pid := os.Getpid()
		return &agy.SpawnWakeWaiterResult{PID: &pid}
	}
	triggerWakePrompt := func(paneID, promptText string) error {
		client.mu.Lock()
		wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
		client.mu.Unlock()
		return nil
	}

	// 1. The peer is already done but its turn is unsettled (last assistant
	// message still null while a user message exists): Stop arms the next turn
	// and spawns a waiter.
	stopResult := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, agy.RunAgyHookOptions{
		Connect:         client.connect,
		Environment:     herdrEnv,
		SocketPath:      "/tmp/herdsman.sock",
		SpawnWakeWaiter: spawnWakeWaiter,
		StateDir:        strPtr(dir),
	})
	if !isEmptyHookOutput(stopResult) {
		t.Fatalf("stop result = %+v, want {}", stopResult)
	}
	client.mu.Lock()
	epochs := append([]float64{}, spawnedEpochs...)
	client.mu.Unlock()
	if want := []float64{1}; !reflect.DeepEqual(epochs, want) {
		t.Fatalf("spawnedEpochs = %v, want %v", epochs, want)
	}
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"awaitingNextTurn": true, "turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})

	// 2. The peer's assistant response flushes (still no agent.done): the waiter
	// stays alive across settleGraceMs and wakes the main pane once the event
	// arrives, even though the main pane is already "done".
	client.mu.Lock()
	peerLastAssistantMessage = map[string]any{"ref": "peer-asst-1", "text": "All checks green.", "timestamp": nil}
	client.mu.Unlock()
	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connectCapturing,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs:    floatPtr(100),
			SettleGraceMs:     floatPtr(500),
			SocketPath:        "/tmp/herdsman.sock",
			StateDir:          strPtr(dir),
			TriggerWakePrompt: triggerWakePrompt,
			WaitTimeoutMs:     floatPtr(5000),
		})
	}()
	time.Sleep(60 * time.Millisecond)
	client.mu.Lock()
	promptsBeforeEvent := len(wakePrompts)
	client.mu.Unlock()
	if promptsBeforeEvent != 0 {
		t.Fatalf("wakePrompts before the event = %d, want 0", promptsBeforeEvent)
	}
	client.mu.Lock()
	pendingEvents = []any{makeDoneEvent(50, "All checks green.")}
	client.mu.Unlock()
	client.push(makeDoneEvent(50, "All checks green."))
	if result := awaitHookOutput(t, done); !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	client.mu.Lock()
	promptsAfterEvent := append([]wakePrompt{}, wakePrompts...)
	client.mu.Unlock()
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(promptsAfterEvent, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", promptsAfterEvent, want)
	}

	// 3. The next turn's PreInvocation (invocationNum > 1) still advances the
	// epoch because Stop set awaitingNextTurn, and it injects the flushed event.
	preOut := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":4,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	if len(preOut.InjectSteps) != 1 || !strings.Contains(preOut.InjectSteps[0].EphemeralMessage, "All checks green.") {
		t.Fatalf("preOut = %+v, want the flushed event injected", preOut)
	}
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"turnEpoch": float64(2),
	})

	// 4. With exitWhenNoPeers opted in and no further events, the waiter clears
	// waiterPid and exits once settleGraceMs elapses.
	client.mu.Lock()
	pendingEvents = []any{}
	client.mu.Unlock()
	writeStateFile(t, sessionStatePath(dir, sessionID), map[string]any{
		"turnEpoch": float64(2), "waiterEpoch": float64(2), "waiterPid": float64(os.Getpid()),
	})
	expiringResult := agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
		Connect: client.connect,
		Environment: mergeEnv(herdrEnv,
			"HERDSMAN_AGY_SESSION_ID", sessionID,
			"HERDSMAN_AGY_STATE_DIR", dir,
			"HERDSMAN_AGY_WAITER_EPOCH", "2"),
		ExitWhenNoPeers:   boolPtr(true),
		PollIntervalMs:    floatPtr(100),
		SettleGraceMs:     floatPtr(300),
		SocketPath:        "/tmp/herdsman.sock",
		StateDir:          strPtr(dir),
		TriggerWakePrompt: triggerWakePrompt,
		WaitTimeoutMs:     floatPtr(5000),
	})
	if !isEmptyHookOutput(expiringResult) {
		t.Fatalf("expiring waiter result = %+v, want {}", expiringResult)
	}
	if _, ok := readStateObject(t, sessionStatePath(dir, sessionID))["waiterPid"]; ok {
		t.Fatal("waiterPid was not cleared after settleGraceMs elapsed")
	}
}

func boolPtr(value bool) *bool { return &value }
