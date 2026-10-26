package observability

import (
	"encoding/json"
	"testing"
)

// decode is a test helper that mirrors Value.Check's input (decoded JSON).
func decode(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return value
}

func TestAgentListInputSchemaValidatesScopes(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{}`, true},
		{`{"all":true}`, true},
		{`{"herdrSessionName":"default","workspaceId":"wB"}`, true},
		{`{"observedWorkspaceId":"ow_1"}`, false},
	}
	for _, tc := range cases {
		if got := AgentListInputSchema.Check(decode(t, tc.raw)); got != tc.want {
			t.Errorf("agentListInputSchema(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestAgentGetAndReadInputSchemaValidate(t *testing.T) {
	if !AgentGetInputSchema.Check(decode(t, `{"target":"claude","workspaceId":"wB"}`)) {
		t.Error("agentGetInputSchema should accept target+workspaceId")
	}
	cases := []struct {
		raw  string
		want bool
	}{
		{`{"limit":10,"target":"wB:p2"}`, true},
		{`{"limit":0,"target":"wB:p2"}`, false},
		{`{"limit":501,"target":"wB:p2"}`, false},
	}
	for _, tc := range cases {
		if got := AgentReadInputSchema.Check(decode(t, tc.raw)); got != tc.want {
			t.Errorf("agentReadInputSchema(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestAgentOrchestratorRegisterInputSchemaValidatesConnectionBoundRPC(t *testing.T) {
	valid := `{
		"herdrSocketPath": "/tmp/herdr.sock",
		"paneId": "wB:p1",
		"sessionRef": {"agent":"pi","kind":"path","source":"herdr:pi","value":"/tmp/pi-session.jsonl"},
		"subscriberId": "pi-session",
		"subscriberKind": "pi",
		"workspaceId": "wB"
	}`
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"valid pi registration", valid, true},
		{"pi sessionRef with kind=id", `{
			"herdrSocketPath":"/tmp/herdr.sock","paneId":"wB:p1",
			"sessionRef":{"agent":"pi","kind":"id","source":"herdr:pi","value":"/tmp/pi-session.jsonl"},
			"subscriberId":"pi-session","subscriberKind":"pi","workspaceId":"wB"}`, false},
		{"pi sessionRef with agent=claude", `{
			"herdrSocketPath":"/tmp/herdr.sock","paneId":"wB:p1",
			"sessionRef":{"agent":"claude","kind":"path","source":"herdr:pi","value":"/tmp/pi-session.jsonl"},
			"subscriberId":"pi-session","subscriberKind":"pi","workspaceId":"wB"}`, false},
		{"empty sessionRef value", `{
			"herdrSocketPath":"/tmp/herdr.sock","paneId":"wB:p1",
			"sessionRef":{"agent":"pi","kind":"path","source":"herdr:pi","value":""},
			"subscriberId":"pi-session","subscriberKind":"pi","workspaceId":"wB"}`, false},
		{"missing sessionRef", `{
			"herdrSocketPath":"/tmp/herdr.sock","paneId":"wB:p1",
			"subscriberId":"pi-session","subscriberKind":"pi","workspaceId":"wB"}`, false},
		{"extra autoResume key", `{
			"herdrSocketPath":"/tmp/herdr.sock","paneId":"wB:p1","autoResume":true,
			"sessionRef":{"agent":"pi","kind":"path","source":"herdr:pi","value":"/tmp/pi-session.jsonl"},
			"subscriberId":"pi-session","subscriberKind":"pi","workspaceId":"wB"}`, false},
		{"subscriberKind claude", `{
			"herdrSocketPath":"/tmp/herdr.sock","paneId":"wB:p1",
			"sessionRef":{"agent":"pi","kind":"path","source":"herdr:pi","value":"/tmp/pi-session.jsonl"},
			"subscriberId":"pi-session","subscriberKind":"claude","workspaceId":"wB"}`, false},
		{"autoResume with no sessionRef", `{
			"autoResume":true,"herdrSocketPath":"/tmp/herdr.sock","paneId":"wB:p1",
			"subscriberId":"pi-session-1","subscriberKind":"pi","workspaceId":"wB"}`, false},
		{"agy sessionRef kind=id", `{
			"herdrSocketPath":"/tmp/herdr.sock","paneId":"wB:p1",
			"sessionRef":{"agent":"agy","kind":"id","source":"herdr:agy","value":"conv-1"},
			"subscriberId":"agy-1","subscriberKind":"agy","workspaceId":"wB"}`, true},
		{"agy sessionRef on pi subscriberKind", `{
			"herdrSocketPath":"/tmp/herdr.sock","paneId":"wB:p1",
			"sessionRef":{"agent":"agy","kind":"id","source":"herdr:agy","value":"conv-1"},
			"subscriberId":"agy-1","subscriberKind":"pi","workspaceId":"wB"}`, true},
	}
	for _, tc := range cases {
		if got := AgentOrchestratorRegisterInputSchema.Check(decode(t, tc.raw)); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOrchestratorSetGetPingAndAckSchemas(t *testing.T) {
	if !AgentOrchestratorSetInputSchema.Check(decode(t, `{"enabled":true}`)) {
		t.Error("set(enabled=true) should pass")
	}
	if !AgentOrchestratorSetInputSchema.Check(decode(t, `{"enabled":false}`)) {
		t.Error("set(enabled=false) should pass")
	}
	if AgentOrchestratorSetInputSchema.Check(decode(t, `{}`)) {
		t.Error("set({}) should fail (enabled is required)")
	}
	if !AgentOrchestratorGetInputSchema.Check(decode(t, `{}`)) {
		t.Error("get({}) should pass")
	}
	if !AgentPingInputSchema.Check(decode(t, `{}`)) {
		t.Error("ping({}) should pass")
	}
	if AgentPingInputSchema.Check(decode(t, `{"unexpected":true}`)) {
		t.Error("ping with an unknown key must be rejected")
	}
	if !AgentOrchestratorAckInputSchema.Check(decode(t, `{"eventId":42}`)) {
		t.Error("ack(eventId=42) should pass")
	}
	if AgentOrchestratorAckInputSchema.Check(decode(t, `{"eventId":42,"unexpected":"legacy"}`)) {
		t.Error("ack with an extra key must be rejected")
	}
}

func TestAgentSessionRefSchemaRequiresNonEmptyStrings(t *testing.T) {
	if !AgentSessionRefSchema.Check(decode(t, `{"agent":"pi","kind":"id","source":"s","value":"v"}`)) {
		t.Error("well-formed ref should pass")
	}
	if AgentSessionRefSchema.Check(decode(t, `{"agent":"pi","kind":"other","source":"s","value":"v"}`)) {
		t.Error("kind outside {id,path} must be rejected")
	}
	if AgentSessionRefSchema.Check(decode(t, `{"agent":"","kind":"id","source":"s","value":"v"}`)) {
		t.Error("empty agent must be rejected (minLength 1)")
	}
}

func TestAgentEventsInputSchemaBounds(t *testing.T) {
	if !AgentEventsInputSchema.Check(decode(t, `{"afterEventId":0}`)) {
		t.Error("afterEventId 0 is allowed")
	}
	if AgentEventsInputSchema.Check(decode(t, `{"afterEventId":-1}`)) {
		t.Error("afterEventId -1 must be rejected")
	}
	if !AgentEventsInputSchema.Check(decode(t, `{"limit":500}`)) {
		t.Error("limit 500 is allowed")
	}
	if AgentEventsInputSchema.Check(decode(t, `{"limit":501}`)) {
		t.Error("limit 501 must be rejected")
	}
}

func TestAgentTurnCompletedInputSchema(t *testing.T) {
	if !AgentTurnCompletedInputSchema.Check(decode(t, `{
		"confirmed":true,"herdrSessionName":"default","paneId":"wB:p1",
		"terminalId":"t1","workspaceId":"wB"}`)) {
		t.Error("turn.completed without expectedText should pass")
	}
	if AgentTurnCompletedInputSchema.Check(decode(t, `{
		"confirmed":true,"herdrSessionName":"","paneId":"wB:p1",
		"terminalId":"t1","workspaceId":"wB"}`)) {
		t.Error("empty herdrSessionName must be rejected")
	}
}

func TestRecordsMarshalWithTSFieldNames(t *testing.T) {
	id := func(s string) *string { return &s }
	agent := AgentIndexRecord{
		Agent:            id("pi"),
		AgentStatus:      AgentStatusWorking,
		Focused:          true,
		HerdrSessionName: "default",
		ID:               "ag_1",
		PaneID:           "wB:p1",
		TerminalID:       id("term_1"),
		WorkspaceID:      "wB",
	}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal AgentIndexRecord: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"agentStatus", "herdrSessionName", "paneId", "terminalId", "workspaceId"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("marshaled record lacks %q: %s", key, raw)
		}
	}
	if _, ok := decoded["terminalTitle"]; ok {
		t.Errorf("nil optional terminalTitle should be omitted: %s", raw)
	}
}

func TestParseAgentStatusCollapsesUnknown(t *testing.T) {
	cases := map[string]AgentStatus{
		"blocked": AgentStatusBlocked,
		"done":    AgentStatusDone,
		"idle":    AgentStatusIdle,
		"working": AgentStatusWorking,
		"unknown": AgentStatusUnknown,
		"claude":  AgentStatusUnknown,
		"":        AgentStatusUnknown,
	}
	for input, want := range cases {
		if got := ParseAgentStatus(input); got != want {
			t.Errorf("ParseAgentStatus(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestValidAgentEventTypesOmitsToolFailed(t *testing.T) {
	if len(ValidAgentEventTypes) != 6 {
		t.Fatalf("ValidAgentEventTypes has %d entries, want 6", len(ValidAgentEventTypes))
	}
	if _, ok := ValidAgentEventTypes["agent.tool.failed"]; ok {
		t.Error("VALID_AGENT_EVENT_TYPES deliberately omits agent.tool.failed")
	}
	for _, eventType := range []string{
		"agent.status.changed", "agent.idle", "agent.done",
		"agent.blocked", "agent.failed", "agent.discarded",
	} {
		if _, ok := ValidAgentEventTypes[eventType]; !ok {
			t.Errorf("ValidAgentEventTypes missing %q", eventType)
		}
	}
}

func TestFrozenMetricKeyContract(t *testing.T) {
	if DegradedEventMessage != "Herdsman emitted degraded status event" {
		t.Errorf("DegradedEventMessage = %q", DegradedEventMessage)
	}
	if EmptyTurnAlertReason != "upstream_empty_turn" {
		t.Errorf("EmptyTurnAlertReason = %q", EmptyTurnAlertReason)
	}
	if EmptyTurnAlertMessage != "Herdsman observed an empty upstream turn" {
		t.Errorf("EmptyTurnAlertMessage = %q", EmptyTurnAlertMessage)
	}
	if DegradedPlanError != "degraded" {
		t.Errorf("DegradedPlanError = %q", DegradedPlanError)
	}
	if PlanWaitingHistory != "PLAN_WAITING_HISTORY" {
		t.Errorf("PlanWaitingHistory = %q", PlanWaitingHistory)
	}
	if PlanStalled != "PLAN_STALLED" {
		t.Errorf("PlanStalled = %q", PlanStalled)
	}
	want := []string{"PLAN_WAITING_HISTORY", "PLAN_STALLED", "degraded"}
	if len(StalledPlanErrors) != len(want) {
		t.Fatalf("StalledPlanErrors = %v, want %v", StalledPlanErrors, want)
	}
	for i, value := range want {
		if StalledPlanErrors[i] != value {
			t.Errorf("StalledPlanErrors[%d] = %q, want %q", i, StalledPlanErrors[i], value)
		}
	}
	if len(DegradedReasonByKey) != 4 {
		t.Fatalf("DegradedReasonByKey has %d keys, want 4", len(DegradedReasonByKey))
	}
	if DegradedReasonByKey["staleBaselineDuplicate"] != DegradedReasonStaleBaselineDup {
		t.Error("staleBaselineDuplicate must map to stale_baseline_duplicate")
	}
}

func TestOrchestratorAckErrors(t *testing.T) {
	error := NewOrchestratorAckError(AckTimeout, MsgAckOutOfOrder)
	if error.Error() != MsgAckOutOfOrder {
		t.Errorf("message = %q, want %q", error.Error(), MsgAckOutOfOrder)
	}
	if error.Name() != "OrchestratorAckError" {
		t.Errorf("name = %q", error.Name())
	}
	if !error.Retryable() {
		t.Error("ORCHESTRATOR_ACK_TIMEOUT must be retryable")
	}
	if NewOrchestratorAckError(AckNotOwner, MsgAckNotOwner).Retryable() {
		t.Error("ORCHESTRATOR_NOT_OWNER must not be retryable")
	}
	if len(OrchestratorAckErrorCodeRetryable) != 12 {
		t.Errorf("OrchestratorAckErrorCodeRetryable has %d entries, want 12", len(OrchestratorAckErrorCodeRetryable))
	}
	retryable := map[OrchestratorAckErrorCode]bool{
		AckBusy: true, AckConnectionLost: true, AckReconciling: true, AckTimeout: true,
		AckNotOwner: false, AckEventInvalidated: false, AckEventFailed: false,
		AckEventAlreadyAcked: false, AckEventNotInScope: false, AckEventOutOfOrder: false,
		AckOwnerReplaced: false, AckEventNotFound: false,
	}
	for code, want := range retryable {
		if got := OrchestratorAckErrorCodeRetryable[code]; got != want {
			t.Errorf("%s retryable = %v, want %v", code, got, want)
		}
	}
}

func TestOrchestratorAckMessages(t *testing.T) {
	if MsgAckNotOwner != "Only the current orchestrator can acknowledge notifications" {
		t.Errorf("MsgAckNotOwner = %q", MsgAckNotOwner)
	}
	if MsgAckInvalidated != "orchestrator event is no longer pending (invalidated)" {
		t.Errorf("MsgAckInvalidated = %q", MsgAckInvalidated)
	}
	if MsgAckOutOfOrder != "Only the next pending orchestrator event can be acknowledged" {
		t.Errorf("MsgAckOutOfOrder = %q", MsgAckOutOfOrder)
	}
}
