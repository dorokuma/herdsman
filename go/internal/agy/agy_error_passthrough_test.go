package agy_test

// Regression tests for the agy-error passthrough: a sub-agent result whose text
// matches an upstream model error pattern (RESOURCE_EXHAUSTED, Quota exceeded,
// 429 ...) must be delivered verbatim to the main pane with a warning banner —
// never suppressed and never acked before delivery.
//
// The two fixtures in testdata/ are the real sub-agent reports that were
// silently swallowed on 2026-10-31 (events 80146 and 80165 in
// agy-wake-miss-evidence-20261031/).

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/agy"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

//go:embed testdata/researcher_quota_exceeded.txt
var researcherQuotaExceededText string

//go:embed testdata/scout_resource_exhausted.txt
var scoutResourceExhaustedText string

const suspectedErrorWarning = "疑似包含模型/上游报错"

// makeDoneEventWithText is makeDoneEvent with an explicit assistant text that
// may be huge; it exists to keep fixture wiring readable.
func makeDoneEventWithText(id int64, text string) observability.AgentEventRecord {
	return makeDoneEvent(id, text)
}

// upstreamErrFixture builds a fake RPC client answering orchestrator state with
// the given pending events and records acks + wake prompts.
type upstreamErrFixture struct {
	client       *concClient
	pending      []any
	ackedIDs     []int64
	ackedEventID int64
	wakePrompts  []wakePrompt
}

func newUpstreamErrFixture(sessionID string, events []any) *upstreamErrFixture {
	fixture := &upstreamErrFixture{pending: events}
	fixture.client = &concClient{}
	fixture.client.respond = func(method string, params any) (any, error) {
		switch method {
		case "agent.notifications.ack":
			eventID := asEventID(params.(map[string]any)["eventId"])
			fixture.ackedIDs = append(fixture.ackedIDs, eventID)
			if eventID > fixture.ackedEventID {
				fixture.ackedEventID = eventID
			}
			return map[string]any{"acknowledged": true}, nil
		case "agent.list":
			return map[string]any{"agents": []any{}}, nil
		}
		live := []any{}
		for _, event := range fixture.pending {
			if event.(observability.AgentEventRecord).ID > fixture.ackedEventID {
				live = append(live, event)
			}
		}
		return orchestratorStateResponse(sessionID, live, fixture.ackedEventID), nil
	}
	return fixture
}

func (f *upstreamErrFixture) waiterOptions(dir, sessionID string) agy.RunAgyHookOptions {
	return agy.RunAgyHookOptions{
		Connect: f.client.connectCapturing,
		Environment: mergeEnv(herdrEnv,
			"HERDSMAN_AGY_SESSION_ID", sessionID,
			"HERDSMAN_AGY_STATE_DIR", dir,
			"HERDSMAN_AGY_WAITER_EPOCH", "1"),
		PollIntervalMs: floatPtr(50),
		SocketPath:     "/tmp/herdsman.sock",
		StateDir:       strPtr(dir),
		ReadPaneText:   func(string) (*string, error) { return nil, nil },
		TriggerWakePrompt: func(paneID, promptText string) error {
			f.client.mu.Lock()
			f.wakePrompts = append(f.wakePrompts, wakePrompt{paneID, promptText})
			f.client.mu.Unlock()
			return nil
		},
		WaitTimeoutMs: floatPtr(3000),
	}
}

// readAuditLog reads <stateDir>/agy-wake-audit.jsonl into parsed entries.
func readAuditLog(t *testing.T, dir string) []map[string]any {
	t.Helper()
	path := filepath.Join(dir, "agy-wake-audit.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("audit log missing at %s: %v", path, err)
	}
	entries := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("audit line is not JSON: %q (%v)", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

// TestUpstreamErrorResultIsDeliveredVerbatimWithWarning is the core regression:
// the two real sub-agent reports that were swallowed now reach the wake batch,
// byte-identical, with the warning banner, and are not acked before the host
// receives them.
func TestUpstreamErrorResultIsDeliveredVerbatimWithWarning(t *testing.T) {
	cases := []struct {
		name    string
		eventID int64
		text    string
	}{
		{"researcher_quota_exceeded", 80146, researcherQuotaExceededText},
		{"scout_resource_exhausted", 80165, scoutResourceExhaustedText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sessionID := "e8014680-1650-4000-8000-00000000000" + string(rune('0'+tc.eventID%10))
			fixture := newUpstreamErrFixture(sessionID, []any{
				makeDoneEventWithText(tc.eventID, tc.text),
			})

			// 1. The waiter sees the flagged completion as a real wake batch and
			//    fires the wake prompt; nothing is acked before that.
			result := agy.RunAgyHook(agy.HookWakeWaiter, "", fixture.waiterOptions(dir, sessionID))
			if !isEmptyHookOutput(result) {
				t.Fatalf("waiter result = %+v, want {}", result)
			}
			if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(fixture.wakePrompts, want) {
				t.Fatalf("wakePrompts = %+v, want %+v", fixture.wakePrompts, want)
			}
			if len(fixture.ackedIDs) != 0 {
				t.Fatalf("ackedIDs = %v before injection, want none", fixture.ackedIDs)
			}

			// 2. PreInvocation injects the batch: verbatim text + warning banner.
			pre := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
				Connect:     fixture.client.connect,
				Environment: herdrEnv,
				SocketPath:  "/tmp/herdsman.sock",
				StateDir:    strPtr(dir),
			})
			if len(pre.InjectSteps) != 1 {
				t.Fatalf("injectSteps = %+v, want exactly one", pre.InjectSteps)
			}
			message := pre.InjectSteps[0].EphemeralMessage
			if !strings.Contains(message, tc.text) {
				t.Fatalf("injected message does not contain the verbatim report (%d bytes)", len(tc.text))
			}
			if !strings.Contains(message, suspectedErrorWarning) {
				t.Fatalf("injected message lacks the warning banner:\n%s", message[:400])
			}
			warnIndex := strings.Index(message, suspectedErrorWarning)
			textIndex := strings.Index(message, tc.text)
			if !(warnIndex < textIndex) {
				t.Fatalf("warning must precede the delivered text (warn=%d text=%d)", warnIndex, textIndex)
			}
			if len(fixture.ackedIDs) != 0 {
				t.Fatalf("ackedIDs = %v after injection, want none until the host's next Stop", fixture.ackedIDs)
			}

			// 3. The hit was written to the durable audit log.
			entries := readAuditLog(t, dir)
			found := false
			for _, entry := range entries {
				if entry["kind"] == "suspected_upstream_error" &&
					asEventID(entry["eventId"]) == tc.eventID {
					found = true
				}
			}
			if !found {
				t.Fatalf("audit log lacks a suspected_upstream_error entry for event %d: %v", tc.eventID, entries)
			}
		})
	}
}

// TestCleanCompletionDeliversWithoutWarning is the control case: a normal
// completion with no upstream error keywords still wakes, injects without the
// banner, and leaves no audit entry.
func TestCleanCompletionDeliversWithoutWarning(t *testing.T) {
	dir := t.TempDir()
	sessionID := "c111eaa1-2222-3333-4444-555555555555"
	fixture := newUpstreamErrFixture(sessionID, []any{
		makeDoneEventWithText(70, "Scout finished the code search, all findings delivered."),
	})

	result := agy.RunAgyHook(agy.HookWakeWaiter, "", fixture.waiterOptions(dir, sessionID))
	if !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(fixture.wakePrompts, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", fixture.wakePrompts, want)
	}

	pre := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     fixture.client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	if len(pre.InjectSteps) != 1 {
		t.Fatalf("injectSteps = %+v, want exactly one", pre.InjectSteps)
	}
	message := pre.InjectSteps[0].EphemeralMessage
	if !strings.Contains(message, "Scout finished the code search") {
		t.Fatalf("injected message lacks the completion text:\n%s", message)
	}
	if strings.Contains(message, suspectedErrorWarning) {
		t.Fatalf("clean completion must not carry the warning banner:\n%s", message)
	}
	if _, err := os.Stat(filepath.Join(dir, "agy-wake-audit.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("audit log must not exist for a clean batch, stat err = %v", err)
	}
}

// TestUpstreamErrorFailedOutcomeStillDelivers covers `agent.failed` whose
// reason (not the assistant text) matches an upstream error pattern: it is
// delivered as a flagged failure outcome, not acked away.
func TestUpstreamErrorFailedOutcomeStillDelivers(t *testing.T) {
	dir := t.TempDir()
	sessionID := "f411ed00-2222-3333-4444-555555555555"

	failed := makeDoneEventWithText(90, "Worker tried twice and gave up.")
	failed.Type = "agent.failed"
	failed.Payload = map[string]any{
		"agent":  "pi",
		"name":   "role-worker-1",
		"reason": "agent executor error: RESOURCE_EXHAUSTED (code 429): quota exceeded",
	}
	fixture := newUpstreamErrFixture(sessionID, []any{failed})

	result := agy.RunAgyHook(agy.HookWakeWaiter, "", fixture.waiterOptions(dir, sessionID))
	if !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(fixture.wakePrompts, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", fixture.wakePrompts, want)
	}
	if len(fixture.ackedIDs) != 0 {
		t.Fatalf("ackedIDs = %v before injection, want none", fixture.ackedIDs)
	}

	pre := agy.RunAgyHook(agy.HookPreInvocation, `{"invocationNum":1,"sessionId":"`+sessionID+`"}`, agy.RunAgyHookOptions{
		Connect:     fixture.client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
	})
	if len(pre.InjectSteps) != 1 {
		t.Fatalf("injectSteps = %+v, want exactly one", pre.InjectSteps)
	}
	message := pre.InjectSteps[0].EphemeralMessage
	if !strings.Contains(message, "RESOURCE_EXHAUSTED") {
		t.Fatalf("injected message must carry the failure reason verbatim:\n%s", message)
	}
	if !strings.Contains(message, suspectedErrorWarning) {
		t.Fatalf("injected message lacks the warning banner:\n%s", message)
	}

	entries := readAuditLog(t, dir)
	found := false
	for _, entry := range entries {
		if entry["kind"] == "suspected_upstream_error" && asEventID(entry["eventId"]) == 90 {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit log lacks a suspected_upstream_error entry for event 90: %v", entries)
	}
}

// TestSuppressedEmptyTextStillNotWoken keeps the remaining suppression class
// honest: an empty assistant text is still swallowed, but still without ack
// before delivery semantics change (it is the only permanently suppressed
// outcome kind left).
func TestSuppressedEmptyTextStillNotWoken(t *testing.T) {
	dir := t.TempDir()
	sessionID := "511ed000-2222-3333-4444-555555555555"
	fixture := newUpstreamErrFixture(sessionID, []any{
		makeDoneEventWithText(31, "   "),
	})
	result := agy.RunAgyHook(agy.HookWakeWaiter, "", fixture.waiterOptions(dir, sessionID))
	if !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	if len(fixture.wakePrompts) != 0 {
		t.Fatalf("wakePrompts = %+v, want none", fixture.wakePrompts)
	}
}
