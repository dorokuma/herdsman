package agy_test

// This file ports the first half (lines 1-650) of
// test/unit/agy-hook-and-cli.test.ts: the `herdsman orchestrator CLI commands`
// describe block plus the first five cases of `agy lifecycle hooks (PreInvocation
// & Stop)`. The Stop/WakeWaiter cycle case that straddles line 650 and the
// remaining cases live in agy_hook_part2_test.go.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/agy"
	"github.com/dorokuma/herdsman/go/internal/cli"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// herdrEnv mirrors the TS HERDR_ENV constant.
var herdrEnv = map[string]string{
	"HERDR_ENV":          "1",
	"HERDR_PANE_ID":      "wB:p1",
	"HERDR_SOCKET_PATH":  "/tmp/herdr-default.sock",
	"HERDR_WORKSPACE_ID": "wB",
}

// recordedCall mirrors one entry of the TS `calls` array.
type recordedCall struct {
	method string
	params any
}

// hookFakeClient is the Go stand-in for the TS fake `connect` client: it records
// every Call and answers through an overridable respond function.
type hookFakeClient struct {
	calls   []recordedCall
	respond func(method string, params any) (any, error)
}

func (c *hookFakeClient) Close() {}

func (c *hookFakeClient) Call(method string, params any) (any, error) {
	c.calls = append(c.calls, recordedCall{method: method, params: params})
	if c.respond != nil {
		return c.respond(method, params)
	}
	return map[string]any{}, nil
}

// connectReturning adapts a fixed client to the agy.ConnectFunc seam.
func connectReturning(client agy.RpcClient) func(string, func(string, any)) (agy.RpcClient, error) {
	return func(string, func(string, any)) (agy.RpcClient, error) { return client, nil }
}

// connectReturningCli is connectReturning for the cli.RunCliDeps.Connect seam,
// which speaks cli.RpcClientLike (the same method set hookFakeClient has).
func connectReturningCli(client cli.RpcClientLike) func(string, func(string, any)) (cli.RpcClientLike, error) {
	return func(string, func(string, any)) (cli.RpcClientLike, error) { return client, nil }
}

// methodsOf projects the recorded calls onto their method names, mirroring
// `calls.map(([method]) => method)`.
func methodsOf(calls []recordedCall) []string {
	methods := make([]string, len(calls))
	for index, call := range calls {
		methods[index] = call.method
	}
	return methods
}

// mergeEnv copies base and applies the key/value pairs, mirroring `{ ...env, K: V }`.
func mergeEnv(base map[string]string, keyValues ...string) map[string]string {
	out := make(map[string]string, len(base)+len(keyValues)/2)
	for key, value := range base {
		out[key] = value
	}
	for index := 0; index+1 < len(keyValues); index += 2 {
		out[keyValues[index]] = keyValues[index+1]
	}
	return out
}

// makeDoneEvent mirrors the TS makeDoneEvent helper: an `agent.done` record for
// a peer terminal whose last assistant message is `text`.
// makeDoneEvent builds a done event of the default peer terminal. The
// variadic terminalID override mirrors the TS third parameter and keeps the
// existing two-argument calls unchanged.
func makeDoneEvent(id int64, text string, terminalIDOverride ...string) observability.AgentEventRecord {
	terminalID := "term_peer"
	paneID := "wB:p2"
	paneGeneration := "gen-1"
	agentID := "ag_peer"
	workspaceID := "wB"
	attempts := 0
	status := observability.AgentEventStatusPending
	path := "/tmp/pi-session.jsonl"
	if len(terminalIDOverride) > 0 {
		terminalID = terminalIDOverride[0]
		paneID = "wB:p3"
		agentID = "ag_" + terminalID
	}
	return observability.AgentEventRecord{
		AgentID: &agentID,
		CompactHistory: &observability.CompactAgentHistory{
			HistoryRef: &observability.AgentHistoryRef{
				Kind:   observability.HistoryRefAgentSession,
				Path:   &path,
				Source: observability.HistorySourcePiJSONL,
				Value:  "/tmp/pi-session.jsonl",
			},
			LastAssistantMessage: &observability.AgentHistoryExcerpt{
				Ref:       "ref-" + strconv.FormatInt(id, 10),
				Text:      text,
				Timestamp: strPtr("2026-10-21T12:00:00.000Z"),
			},
			LastToolResult:  nil,
			LastUserMessage: nil,
			MessageCount:    2,
			Source:          strPtr("pi-jsonl"),
			UpdatedAt:       strPtr("2026-10-21T12:00:00.000Z"),
		},
		CreatedAt:        "2026-10-21T12:00:00.000Z",
		Deliverable:      1,
		DeliveryAttempts: &attempts,
		HerdrSessionName: "default",
		ID:               id,
		PaneGeneration:   &paneGeneration,
		PaneID:           &paneID,
		Payload: map[string]any{
			"agent": "pi",
			"from":  "working",
			"name":  "role-scout-1",
			"to":    "done",
		},
		Status:      &status,
		TerminalID:  &terminalID,
		Type:        observability.AgentEventDone,
		WorkspaceID: &workspaceID,
	}
}

func asEventID(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	}
	return 0
}

func strPtr(value string) *string { return &value }

func intPtr(value int) *int { return &value }

// --- herdsman orchestrator CLI commands -------------------------------------

func TestParseOnOffStatusAndAgyHookCommands(t *testing.T) {
	onCommand, err := cli.ParseCliArgs([]string{"on", "--if-unowned", "--json"}, herdrEnv)
	if err != nil {
		t.Fatal(err)
	}
	wantOn := cli.CliCommand{
		Action:          "on",
		Command:         cli.CommandOrchestrator,
		HerdrSocketPath: "/tmp/herdr-default.sock",
		IfUnowned:       true,
		JSON:            true,
		PaneID:          "wB:p1",
		WorkspaceID:     strPtr("wB"),
	}
	if !reflect.DeepEqual(onCommand, wantOn) {
		t.Fatalf("on command = %+v, want %+v", onCommand, wantOn)
	}

	offCommand, err := cli.ParseCliArgs([]string{"off"}, herdrEnv)
	if err != nil {
		t.Fatal(err)
	}
	wantOff := cli.CliCommand{
		Action:          "off",
		Command:         cli.CommandOrchestrator,
		HerdrSocketPath: "/tmp/herdr-default.sock",
		PaneID:          "wB:p1",
		WorkspaceID:     strPtr("wB"),
	}
	if !reflect.DeepEqual(offCommand, wantOff) {
		t.Fatalf("off command = %+v, want %+v", offCommand, wantOff)
	}

	statusCommand, err := cli.ParseCliArgs([]string{"status"}, herdrEnv)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus := cli.CliCommand{
		Action:          "status",
		Command:         cli.CommandOrchestrator,
		HerdrSocketPath: "/tmp/herdr-default.sock",
		PaneID:          "wB:p1",
		WorkspaceID:     strPtr("wB"),
	}
	if !reflect.DeepEqual(statusCommand, wantStatus) {
		t.Fatalf("status command = %+v, want %+v", statusCommand, wantStatus)
	}

	for _, eventName := range []agy.HookEventName{agy.HookPreInvocation, agy.HookStop, agy.HookWakeWaiter} {
		command, err := cli.ParseCliArgs([]string{"agy-hook", string(eventName)}, herdrEnv)
		if err != nil {
			t.Fatal(err)
		}
		want := cli.CliCommand{Command: cli.CommandAgyHook, HookEventName: eventName}
		if !reflect.DeepEqual(command, want) {
			t.Fatalf("agy-hook %s command = %+v, want %+v", eventName, command, want)
		}
	}

	if _, err := cli.ParseCliArgs([]string{"agy-hook", "Invalid"}, herdrEnv); err == nil ||
		err.Error() != "agy-hook requires PreInvocation, Stop, or WakeWaiter" {
		t.Fatalf("agy-hook Invalid error = %v", err)
	}
}

func TestExecutesOnIfUnownedWithoutStealingOwnedWorkspace(t *testing.T) {
	registered := map[string]any{
		"presence": map[string]any{
			"herdrSessionName": "default",
			"paneId":           "wB:p1",
			"terminalId":       "term_self",
			"workspaceId":      "wB",
		},
		"state": map[string]any{
			"ackedEventId":     0,
			"herdrSessionName": "default",
			"owner":            map[string]any{"paneId": "wB:p9", "terminalId": "term_other"},
			"updatedAt":        "2026-10-21T12:00:00.000Z",
			"workspaceId":      "wB",
		},
	}
	client := &hookFakeClient{}
	client.respond = func(string, any) (any, error) { return registered, nil }

	lines := []string{}
	command, err := cli.ParseCliArgs([]string{"on", "--if-unowned"}, herdrEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.RunCliCommand(command, cli.RunCliDeps{
		Connect:    connectReturningCli(client),
		Output:     func(line string) { lines = append(lines, line) },
		SocketPath: "/tmp/herdsman.sock",
	}); err != nil {
		t.Fatal(err)
	}

	if want := []string{"agent.orchestrator.register"}; !reflect.DeepEqual(methodsOf(client.calls), want) {
		t.Fatalf("calls = %v, want %v", methodsOf(client.calls), want)
	}
	if want := []string{"Herdsman is off · owned by wB:p9 (default/wB)"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
}

func TestExecutesOnAndOffCommands(t *testing.T) {
	var owner any
	client := &hookFakeClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.orchestrator.set" {
			enabled := false
			if object, ok := params.(map[string]any); ok {
				if value, ok := object["enabled"].(bool); ok {
					enabled = value
				}
			}
			if enabled {
				owner = map[string]any{"paneId": "wB:p1", "terminalId": "term_self"}
			} else {
				owner = nil
			}
		}
		return map[string]any{
			"changed": true,
			"presence": map[string]any{
				"herdrSessionName": "default",
				"paneId":           "wB:p1",
				"terminalId":       "term_self",
				"workspaceId":      "wB",
			},
			"state": map[string]any{
				"ackedEventId":     0,
				"herdrSessionName": "default",
				"owner":            owner,
				"updatedAt":        "2026-10-21T12:00:00.000Z",
				"workspaceId":      "wB",
			},
		}, nil
	}

	lines := []string{}
	run := func(args []string) {
		t.Helper()
		command, err := cli.ParseCliArgs(args, herdrEnv)
		if err != nil {
			t.Fatal(err)
		}
		if err := cli.RunCliCommand(command, cli.RunCliDeps{
			Connect:    connectReturningCli(client),
			Output:     func(line string) { lines = append(lines, line) },
			SocketPath: "/tmp/herdsman.sock",
		}); err != nil {
			t.Fatal(err)
		}
	}
	run([]string{"on"})
	run([]string{"off"})

	wantMethods := []string{
		"agent.orchestrator.register",
		"agent.orchestrator.set",
		"agent.orchestrator.register",
		"agent.orchestrator.set",
	}
	if !reflect.DeepEqual(methodsOf(client.calls), wantMethods) {
		t.Fatalf("calls = %v, want %v", methodsOf(client.calls), wantMethods)
	}
	if !reflect.DeepEqual(client.calls[1].params, map[string]any{"enabled": true}) {
		t.Fatalf("on set params = %+v", client.calls[1].params)
	}
	if !reflect.DeepEqual(client.calls[3].params, map[string]any{"enabled": false}) {
		t.Fatalf("off set params = %+v", client.calls[3].params)
	}
	for _, index := range []int{0, 2} {
		object, ok := client.calls[index].params.(map[string]any)
		if !ok || object["paneId"] != "wB:p1" {
			t.Fatalf("register params[%d] = %+v, want paneId wB:p1", index, client.calls[index].params)
		}
	}
	wantLines := []string{
		"Herdsman is watching agent updates · default/wB · wB:p1",
		"Herdsman is off (default/wB)",
	}
	if !reflect.DeepEqual(lines, wantLines) {
		t.Fatalf("lines = %v, want %v", lines, wantLines)
	}
}

// --- agy lifecycle hooks (PreInvocation & Stop) -----------------------------

func TestPreInvocationAutoClaimsUnownedWorkspaceAndInjects(t *testing.T) {
	stateDir := t.TempDir()

	var ownerTerminalID *string
	firstClient := &hookFakeClient{}
	firstClient.respond = func(method string, params any) (any, error) {
		if method == "agent.orchestrator.set" {
			ownerTerminalID = strPtr("term_self")
		}
		var owner any
		if ownerTerminalID != nil {
			owner = map[string]any{"paneId": "wB:p1", "terminalId": *ownerTerminalID}
		}
		return map[string]any{
			"events": []any{makeDoneEvent(10, "Scout finished code search.")},
			"presence": map[string]any{
				"herdrSessionName": "default",
				"paneId":           "wB:p1",
				"subscriberId":     "agy:11111111-2222-3333-4444-555555555555",
				"terminalId":       "term_self",
				"workspaceId":      "wB",
			},
			"state": map[string]any{
				"ackedEventId":     0,
				"herdrSessionName": "default",
				"owner":            owner,
				"updatedAt":        "2026-10-21T12:00:00.000Z",
				"workspaceId":      "wB",
			},
		}, nil
	}

	result := agy.RunAgyHook(
		agy.HookPreInvocation,
		`{"sessionId":"11111111-2222-3333-4444-555555555555"}`,
		agy.RunAgyHookOptions{
			Connect:     connectReturning(firstClient),
			Environment: mergeEnv(herdrEnv, "HERDSMAN_AUTO_ON", "1"),
			SocketPath:  "/tmp/herdsman.sock",
			StateDir:    strPtr(stateDir),
		},
	)

	wantMethods := []string{
		"agent.orchestrator.register",
		"agent.orchestrator.set",
		"agent.list",
	}
	if !reflect.DeepEqual(methodsOf(firstClient.calls), wantMethods) {
		t.Fatalf("calls = %v, want %v", methodsOf(firstClient.calls), wantMethods)
	}
	if len(result.InjectSteps) != 1 {
		t.Fatalf("injectSteps = %+v, want exactly one", result.InjectSteps)
	}
	if message := result.InjectSteps[0].EphemeralMessage; !strings.Contains(message, "[HERDSMAN AGENT UPDATES]") ||
		!strings.Contains(message, "Scout finished code search.") {
		t.Fatalf("ephemeralMessage = %q", message)
	}

	secondClient := &hookFakeClient{}
	secondClient.respond = func(string, any) (any, error) {
		return map[string]any{
			"events": []any{},
			"presence": map[string]any{
				"herdrSessionName": "default",
				"paneId":           "wB:p2",
				"subscriberId":     "agy:22222222-2222-3333-4444-555555555555",
				"terminalId":       "term_p2",
				"workspaceId":      "wB",
			},
			"state": map[string]any{
				"ackedEventId":     0,
				"herdrSessionName": "default",
				"owner":            nil,
				"updatedAt":        "2026-10-21T12:00:00.000Z",
				"workspaceId":      "wB",
			},
		}, nil
	}
	nonMainPaneResult := agy.RunAgyHook(
		agy.HookPreInvocation,
		`{"sessionId":"22222222-2222-3333-4444-555555555555"}`,
		agy.RunAgyHookOptions{
			Connect:     connectReturning(secondClient),
			Environment: mergeEnv(herdrEnv, "HERDR_PANE_ID", "wB:p2", "HERDSMAN_AUTO_ON", "1"),
			SocketPath:  "/tmp/herdsman.sock",
			StateDir:    strPtr(stateDir),
		},
	)
	if want := []string{"agent.orchestrator.register"}; !reflect.DeepEqual(methodsOf(secondClient.calls), want) {
		t.Fatalf("non-main-pane calls = %v, want %v", methodsOf(secondClient.calls), want)
	}
	if !isEmptyHookOutput(nonMainPaneResult) {
		t.Fatalf("non-main-pane result = %+v, want {}", nonMainPaneResult)
	}
}

func TestPreInvocationNoAckAndStopRollbackAndOrderedAck(t *testing.T) {
	stateDir := t.TempDir()
	sessionID := "99999999-2222-3333-4444-555555555555"
	ackedIDs := []int64{}
	var ackedEventID int64

	client := &hookFakeClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.notifications.ack" {
			eventID := asEventID(params.(map[string]any)["eventId"])
			ackedIDs = append(ackedIDs, eventID)
			if eventID > ackedEventID {
				ackedEventID = eventID
			}
			return map[string]any{"acknowledged": true}, nil
		}
		return map[string]any{
			"events": []any{
				makeDoneEvent(10, "First subagent finished."),
				makeDoneEvent(12, "Second subagent finished."),
			},
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
		}, nil
	}
	options := func() agy.RunAgyHookOptions {
		return agy.RunAgyHookOptions{
			Connect:     connectReturning(client),
			Environment: herdrEnv,
			SocketPath:  "/tmp/herdsman.sock",
			StateDir:    strPtr(stateDir),
		}
	}

	// 1. PreInvocation #1 injects events 10 & 12 without acking.
	pre1 := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, options())
	if len(pre1.InjectSteps) != 1 {
		t.Fatalf("pre1 injectSteps = %+v, want one", pre1.InjectSteps)
	}
	if len(ackedIDs) != 0 {
		t.Fatalf("ackedIDs after pre1 = %v, want none", ackedIDs)
	}

	// 2. PreInvocation #2 neither re-injects nor acks.
	pre2 := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":2,"sessionId":"`+sessionID+`"}`, options())
	if !isEmptyHookOutput(pre2) {
		t.Fatalf("pre2 = %+v, want {}", pre2)
	}
	if len(ackedIDs) != 0 {
		t.Fatalf("ackedIDs after pre2 = %v, want none", ackedIDs)
	}

	// 3. USER_CANCELED Stop rolls back unacked events.
	agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"USER_CANCELED"}`, options())
	if len(ackedIDs) != 0 {
		t.Fatalf("ackedIDs after USER_CANCELED Stop = %v, want none", ackedIDs)
	}

	// 4. Next turn's PreInvocation re-delivers events 10 & 12.
	pre3 := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, options())
	if len(pre3.InjectSteps) != 1 {
		t.Fatalf("pre3 injectSteps = %+v, want one", pre3.InjectSteps)
	}

	// 5. NO_TOOL_CALL Stop acks events 10 and 12 in ascending order.
	stopOptions := options()
	stopOptions.SpawnWakeWaiter = func(agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
		pid := os.Getpid()
		return &agy.SpawnWakeWaiterResult{PID: &pid}
	}
	agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, stopOptions)
	if want := []int64{10, 12}; !reflect.DeepEqual(ackedIDs, want) {
		t.Fatalf("ackedIDs = %v, want %v", ackedIDs, want)
	}
}

func TestDispatchedSubagentSkipsPreInvocationAndSignalsTurnCompletion(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	transcript := strings.Join([]string{
		`{"content":"Find callers","type":"USER_INPUT"}`,
		`{"content":"Found 3 callers in src/daemon.","type":"PLANNER_RESPONSE"}`,
	}, "\n")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	env := mergeEnv(herdrEnv, "CTXMODE_ROLE", "scout", "HERDSMAN_AUTO_ON", "1")

	preClient := &hookFakeClient{}
	preClient.respond = func(string, any) (any, error) { return map[string]any{}, nil }
	preOut := agy.RunAgyHook(
		agy.HookPreInvocation,
		`{"sessionId":"22222222-2222-3333-4444-555555555555"}`,
		agy.RunAgyHookOptions{
			Connect:     connectReturning(preClient),
			Environment: env,
			SocketPath:  "/tmp/herdsman.sock",
			StateDir:    strPtr(dir),
		},
	)
	if !isEmptyHookOutput(preOut) {
		t.Fatalf("preOut = %+v, want {}", preOut)
	}
	if len(preClient.calls) != 0 {
		t.Fatalf("preClient calls = %v, want none", methodsOf(preClient.calls))
	}

	stopClient := &hookFakeClient{}
	stopClient.respond = func(string, any) (any, error) {
		return map[string]any{
			"events": []any{makeDoneEvent(11, "Should not wake subagent")},
			"presence": map[string]any{
				"herdrSessionName": "default",
				"paneId":           "wB:p1",
				"subscriberId":     "agy:22222222-2222-3333-4444-555555555555",
				"terminalId":       "term_subagent",
				"workspaceId":      "wB",
			},
			"state": map[string]any{
				"ackedEventId":     0,
				"herdrSessionName": "default",
				"owner":            map[string]any{"paneId": "wB:p1", "terminalId": "term_subagent"},
				"updatedAt":        "2026-10-21T12:00:00.000Z",
				"workspaceId":      "wB",
			},
		}, nil
	}
	stopOut := agy.RunAgyHook(
		agy.HookStop,
		`{"sessionId":"22222222-2222-3333-4444-555555555555","terminationReason":"NO_TOOL_CALL","transcriptPath":`+strconv.Quote(transcriptPath)+`}`,
		agy.RunAgyHookOptions{
			Connect:     connectReturning(stopClient),
			Environment: env,
			SocketPath:  "/tmp/herdsman.sock",
			StateDir:    strPtr(dir),
		},
	)
	if !isEmptyHookOutput(stopOut) {
		t.Fatalf("stopOut = %+v, want {}", stopOut)
	}

	wantRegister := map[string]any{
		"herdrSocketPath": "/tmp/herdr-default.sock",
		"paneId":          "wB:p1",
		"sessionRef": map[string]any{
			"agent":  "agy",
			"kind":   "id",
			"source": "agy-subagent",
			"value":  "22222222-2222-3333-4444-555555555555",
		},
		"subscriberId":   "agy:22222222-2222-3333-4444-555555555555",
		"subscriberKind": "agy",
		"workspaceId":    "wB",
	}
	wantCompleted := map[string]any{
		"confirmed":        true,
		"expectedText":     "Found 3 callers in src/daemon.",
		"herdrSessionName": "default",
		"paneId":           "wB:p1",
		"terminalId":       "term_subagent",
		"workspaceId":      "wB",
	}
	if len(stopClient.calls) != 2 {
		t.Fatalf("stopClient calls = %v, want register + turn.completed", methodsOf(stopClient.calls))
	}
	if !reflect.DeepEqual(stopClient.calls[0].params, wantRegister) {
		t.Fatalf("register params = %+v, want %+v", stopClient.calls[0].params, wantRegister)
	}
	if !reflect.DeepEqual(stopClient.calls[1].params, wantCompleted) {
		t.Fatalf("turn.completed params = %+v, want %+v", stopClient.calls[1].params, wantCompleted)
	}
}

// isEmptyHookOutput mirrors the TS `toEqual({})`: no inject step, no decision
// and no reason.
func isEmptyHookOutput(output agy.HookOutput) bool {
	return len(output.InjectSteps) == 0 && output.Decision == nil && output.Reason == nil
}

// TestPreInvocationKeepsLineSeparatorsLiteral is the regression for the U+2028 /
// U+2029 escaping gap: encoding/json writes both as `\u2028` / `\u2029` even with
// SetEscapeHTML(false), while JS's JSON.stringify leaves them literal, so a hook
// output carrying either character differed from the TS output byte for byte.
//
// The excerpt normalizer mirrors `normalizeExcerpt`, which folds both separators
// into `\n`, so they cannot reach the hook output through a real PreInvocation.
// The serialization contract is therefore asserted directly on HookOutput.JSON,
// including the case where the message holds the six-character text `\u2028`.
func TestPreInvocationKeepsLineSeparatorsLiteral(t *testing.T) {
	// 1. A real PreInvocation folds the separators into newlines, exactly like
	// the TS `replace(/[\u2028\u2029\u0085]/g, "\n")`.
	stateDir := t.TempDir()
	sessionID := "44444444-2222-3333-4444-555555555555"
	client := &hookFakeClient{}
	client.respond = func(string, any) (any, error) {
		return map[string]any{
			"events": []any{makeDoneEvent(31, "line1\u2028line2\u2029line3")},
			"presence": map[string]any{
				"herdrSessionName": "default",
				"paneId":           "wB:p1",
				"subscriberId":     "agy:" + sessionID,
				"terminalId":       "term_self",
				"workspaceId":      "wB",
			},
			"state": map[string]any{
				"ackedEventId":     0,
				"herdrSessionName": "default",
				"owner":            map[string]any{"paneId": "wB:p1", "terminalId": "term_self"},
				"updatedAt":        "2026-10-21T12:00:00.000Z",
				"workspaceId":      "wB",
			},
		}, nil
	}
	result := agy.RunAgyHook(
		agy.HookPreInvocation,
		`{"invocationNum":1,"sessionId":"`+sessionID+`"}`,
		agy.RunAgyHookOptions{
			Connect:     connectReturning(client),
			Environment: herdrEnv,
			SocketPath:  "/tmp/herdsman.sock",
			StateDir:    strPtr(stateDir),
		},
	)
	if len(result.InjectSteps) != 1 {
		t.Fatalf("injectSteps = %+v, want exactly one", result.InjectSteps)
	}
	if got := result.InjectSteps[0].EphemeralMessage; !strings.Contains(got, "line1\nline2\nline3") {
		t.Fatalf("ephemeralMessage = %q, want the separators folded into newlines", got)
	}

	// 2. The serialization itself: a non-empty output carrying both separators is
	// written with the literal characters, not the `\u2028` / `\u2029` escapes.
	decision := "continue"
	output := agy.HookOutput{
		Decision:    &decision,
		InjectSteps: []agy.InjectStep{{EphemeralMessage: "line1\u2028line2\u2029line3"}},
		Reason:      strPtr("line1\u2028line2\u2029line3"),
	}
	encoded, err := output.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, `\u2028`) || strings.Contains(encoded, `\u2029`) {
		t.Fatalf("JSON still escapes the separators: %s", encoded)
	}
	if want := `{"injectSteps":[{"ephemeralMessage":"line1 line2 line3"}],"decision":"continue","reason":"line1 line2 line3"}`; encoded != want {
		t.Fatalf("JSON = %s, want %s", encoded, want)
	}

	// 3. A message holding the six-character text `\u2028` (a literal backslash
	// plus `u2028`) keeps the escaped spelling, exactly like JSON.stringify.
	literal := agy.HookOutput{InjectSteps: []agy.InjectStep{{EphemeralMessage: `text\u2028text`}}}
	encodedLiteral, err := literal.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"injectSteps":[{"ephemeralMessage":"text\\u2028text"}]}`; encodedLiteral != want {
		t.Fatalf("JSON = %s, want %s", encodedLiteral, want)
	}
}

// TestRunAgyHookReadsTheProcessEnvironmentWhenEnvironmentIsOmitted is the
// regression for `options.environment ?? process.env`: an omitted environment
// must fall back to the process environment. Go used an empty map, so the
// Herdr launch identity could not resolve and the hook silently returned {}.
func TestRunAgyHookReadsTheProcessEnvironmentWhenEnvironmentIsOmitted(t *testing.T) {
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "wB:p1")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr-default.sock")
	t.Setenv("HERDR_WORKSPACE_ID", "wB")
	t.Setenv("HERDSMAN_AGY_SESSION_ID", "55555555-2222-3333-4444-555555555555")
	// The test process itself runs as a dispatched subagent, which would make
	// PreInvocation bail out before the connect seam.
	t.Setenv("CTXMODE_ROLE", "")

	// WakeWaiter resolves the session id from the environment, so a hook output
	// proves the process environment was read.
	result := agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{})
	if !isEmptyHookOutput(result) {
		t.Fatalf("result = %+v, want {}", result)
	}

	// The launch identity resolves from the process environment too, which the
	// PreInvocation path proves by reaching the connect seam instead of bailing
	// out before it.
	connected := false
	client := &hookFakeClient{}
	client.respond = func(string, any) (any, error) { return map[string]any{}, nil }
	pre := agy.RunAgyHook(agy.HookPreInvocation, `{"sessionId":"55555555-2222-3333-4444-555555555555"}`,
		agy.RunAgyHookOptions{
			Connect: func(string, func(string, any)) (agy.RpcClient, error) {
				connected = true
				return client, nil
			},
			SocketPath: "/tmp/herdsman.sock",
			StateDir:   strPtr(t.TempDir()),
		})
	if !connected {
		t.Fatal("Connect was never called: the process environment was not read")
	}
	if !isEmptyHookOutput(pre) {
		t.Fatalf("pre = %+v, want {}", pre)
	}

	// An explicitly passed empty map stays empty: the identity cannot resolve and
	// the hook returns {} without ever connecting.
	explicit := false
	pre = agy.RunAgyHook(agy.HookPreInvocation, `{"sessionId":"55555555-2222-3333-4444-555555555555"}`,
		agy.RunAgyHookOptions{
			Connect: func(string, func(string, any)) (agy.RpcClient, error) {
				explicit = true
				return client, nil
			},
			Environment: map[string]string{},
			SocketPath:  "/tmp/herdsman.sock",
			StateDir:    strPtr(t.TempDir()),
		})
	if explicit {
		t.Fatal("Connect was called with an explicit empty environment, want no resolution")
	}
	if !isEmptyHookOutput(pre) {
		t.Fatalf("pre = %+v, want {}", pre)
	}
}

// TestWakeWaiterEpochAcceptsRadixPrefixes is the regression for the JS radix
// prefixes strconv.ParseFloat rejects: `Number("0x5")` is 5, not a NaN. The waiter
// epoch is read from HERDSMAN_AGY_WAITER_EPOCH through parseJSNumber, and the loop
// only runs while it equals the state's turnEpoch, so a dropped value silently
// stopped the standby loop instead of letting it deliver the wake batch.
//
// The state carries turnEpoch 5 with waiterEpoch 5: only a parsed
// environment epoch of 5 keeps the loop alive long enough to prompt. Both
// epochs agree, because the post-draft re-check of the 5b batch compares
// waiterEpoch against the resolved waiter epoch too.
func TestWakeWaiterEpochAcceptsRadixPrefixes(t *testing.T) {
	for _, raw := range []string{"0x5", "0X5", "0b101", "0o5"} {
		dir := t.TempDir()
		sessionID := "66666666-2222-3333-4444-555555555555"
		writeStateFile(t, sessionStatePath(dir, sessionID), map[string]any{
			"turnEpoch": float64(5), "waiterEpoch": float64(5), "waiterPid": float64(os.Getpid()),
		})

		peerStatus := "done"
		client := workingRaceClient(t, sessionID, []any{makeDoneEvent(41, "前缀 epoch 完成。")}, &peerStatus, new(int64), nil)
		wakePrompts := []wakePrompt{}
		result := agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connect,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", raw),
			PollIntervalMs: floatPtr(25),
			SocketPath:     "/tmp/herdsman.sock",
			TriggerWakePrompt: func(paneID, promptText string) error {
				wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
				return nil
			},
			WaitTimeoutMs: floatPtr(5000),
		})
		if !isEmptyHookOutput(result) {
			t.Fatalf("result = %+v, want {}", result)
		}
		if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(wakePrompts, want) {
			t.Fatalf("wakePrompts for %q = %+v, want %+v", raw, wakePrompts, want)
		}
	}

	// A signed prefix is a NaN in JS, so the stale state epoch 7 wins, the loop
	// stops immediately and no wake prompt is delivered.
	dir := t.TempDir()
	sessionID := "77777777-2222-3333-4444-555555555555"
	writeStateFile(t, sessionStatePath(dir, sessionID), map[string]any{
		"turnEpoch": float64(5), "waiterEpoch": float64(7), "waiterPid": float64(os.Getpid()),
	})
	peerStatus := "done"
	client := workingRaceClient(t, sessionID, []any{makeDoneEvent(42, "签名前缀完成。")}, &peerStatus, new(int64), nil)
	wakePrompts := []wakePrompt{}
	result := agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
		Connect: client.connect,
		Environment: mergeEnv(herdrEnv,
			"HERDSMAN_AGY_SESSION_ID", sessionID,
			"HERDSMAN_AGY_STATE_DIR", dir,
			"HERDSMAN_AGY_WAITER_EPOCH", "-0x5"),
		PollIntervalMs: floatPtr(25),
		SocketPath:     "/tmp/herdsman.sock",
		TriggerWakePrompt: func(paneID, promptText string) error {
			wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
			return nil
		},
		WaitTimeoutMs: floatPtr(5000),
	})
	if !isEmptyHookOutput(result) {
		t.Fatalf("result = %+v, want {}", result)
	}
	if len(wakePrompts) != 0 {
		t.Fatalf("wakePrompts = %+v, want none", wakePrompts)
	}
}

// TestStateFileContainingJSONNullDoesNotPanic is the regression for a state file
// whose content is the JSON literal `null`: json.Unmarshal succeeds and leaves a
// nil map, and the write path then assigned into that nil map (`assignment to
// entry in nil map`). Both hook events must treat it as an empty state and
// rewrite the file with the real object.
func TestStateFileContainingJSONNullDoesNotPanic(t *testing.T) {
	stateDir := t.TempDir()
	sessionID := "33333333-2222-3333-4444-555555555555"
	statePath := filepath.Join(stateDir, sessionID+".json")
	if err := os.WriteFile(statePath, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := &hookFakeClient{}
	client.respond = func(method string, _ any) (any, error) {
		if method == "agent.notifications.ack" {
			return map[string]any{"acknowledged": true}, nil
		}
		return map[string]any{
			"events": []any{makeDoneEvent(21, "Null state finished.")},
			"presence": map[string]any{
				"herdrSessionName": "default",
				"paneId":           "wB:p1",
				"subscriberId":     "agy:" + sessionID,
				"terminalId":       "term_self",
				"workspaceId":      "wB",
			},
			"state": map[string]any{
				"ackedEventId":     0,
				"herdrSessionName": "default",
				"owner":            map[string]any{"paneId": "wB:p1", "terminalId": "term_self"},
				"updatedAt":        "2026-10-21T12:00:00.000Z",
				"workspaceId":      "wB",
			},
		}, nil
	}
	options := func() agy.RunAgyHookOptions {
		return agy.RunAgyHookOptions{
			Connect:     connectReturning(client),
			Environment: herdrEnv,
			SocketPath:  "/tmp/herdsman.sock",
			StateDir:    strPtr(stateDir),
		}
	}

	pre := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, options())
	if len(pre.InjectSteps) != 1 {
		t.Fatalf("pre injectSteps = %+v, want one", pre.InjectSteps)
	}

	stopOptions := options()
	stopOptions.SpawnWakeWaiter = func(agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
		pid := os.Getpid()
		return &agy.SpawnWakeWaiterResult{PID: &pid}
	}
	stop := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, stopOptions)
	if !isEmptyHookOutput(stop) {
		t.Fatalf("stop = %+v, want {}", stop)
	}

	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	written := map[string]any{}
	if err := json.Unmarshal(persisted, &written); err != nil {
		t.Fatalf("state file is not an object: %v (%s)", err, persisted)
	}
	if written["turnEpoch"] != float64(2) {
		t.Fatalf("turnEpoch = %v, want 2 (PreInvocation bumped it, Stop armed the next turn)", written["turnEpoch"])
	}
	if written["awaitingNextTurn"] != true {
		t.Fatalf("awaitingNextTurn = %v, want true", written["awaitingNextTurn"])
	}
	// The Stop branch acks the delivered event, so the acked id lands in
	// presentedEventIds while deliveredEventIds is emptied again.
	if want := []any{float64(21)}; !reflect.DeepEqual(written["presentedEventIds"], want) {
		t.Fatalf("presentedEventIds = %v, want %v", written["presentedEventIds"], want)
	}
}
