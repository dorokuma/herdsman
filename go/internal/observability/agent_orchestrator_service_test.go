package observability_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The port of test/integration/agent-orchestrator-service.test.ts.
//
// The harness mirrors openObservabilityDbHarness: a migrated database in a temp
// dir with the running "default" Herdr session and the three stores wired into
// the service. Every test uses t.TempDir(), so the production home and the live
// daemon are never touched.

const orchestratorTestSession = "default"

var orchestratorScope = observability.AgentScope{
	HerdrSessionName: orchestratorTestSession,
	WorkspaceID:      "wB",
}

type orchestratorHarness struct {
	sqlite      *sql.DB
	agentEvents *db.AgentEventStore
	agents      *db.AgentStore
	scopes      *db.AgentOrchestratorScopeStore
	service     *observability.AgentOrchestratorService
}

// openOrchestratorHarness mirrors openService: a migrated database with the
// running "default" Herdr session and the service wired to the three stores.
func openOrchestratorHarness(t *testing.T) *orchestratorHarness {
	t.Helper()
	sqlite, err := db.OpenSqlite(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("openSqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	if err := db.ApplyMigrations(sqlite); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	sessions := db.NewHerdrSessionStore(sqlite)
	if _, err := sessions.UpsertRunning(db.UpsertRunningHerdrSessionInput{
		Name:       orchestratorTestSession,
		SessionDir: "/tmp/herdr",
		SocketPath: "/tmp/herdr.sock",
	}); err != nil {
		t.Fatalf("upsertRunning: %v", err)
	}
	agentEvents, err := db.NewAgentEventStore(sqlite)
	if err != nil {
		t.Fatalf("newAgentEventStore: %v", err)
	}
	agents := db.NewAgentStore(sqlite, agentEvents)
	scopes := db.NewAgentOrchestratorScopeStore(sqlite)
	return &orchestratorHarness{
		sqlite:      sqlite,
		agentEvents: agentEvents,
		agents:      agents,
		scopes:      scopes,
		service: observability.NewAgentOrchestratorService(struct {
			AgentEvents observability.AgentEventStoreLike
			Agents      observability.AgentStoreLike
			Scopes      observability.AgentOrchestratorScopeStoreLike
		}{
			AgentEvents: agentEvents,
			Agents:      agents,
			Scopes:      scopes,
		}),
	}
}

// upsertRunningSession mirrors the extra upsertRunning calls of the TS tests.
func (h *orchestratorHarness) upsertRunningSession(t *testing.T, name string) {
	t.Helper()
	sessions := db.NewHerdrSessionStore(h.sqlite)
	if _, err := sessions.UpsertRunning(db.UpsertRunningHerdrSessionInput{
		Name:       name,
		SessionDir: "/tmp/" + name,
		SocketPath: "/tmp/" + name + ".sock",
	}); err != nil {
		t.Fatalf("upsertRunning(%s): %v", name, err)
	}
}

// orchestratorAppendInput mirrors the TS appendEvent input.
type orchestratorAppendInput struct {
	agent          *string
	compactHistory *observability.CompactAgentHistory
	hasHistory     bool
	terminalID     string
	eventType      *observability.AgentEventType
	from           *string
	sessionPath    *string
	workspaceID    *string
}

// appendOrchestratorEvent mirrors the TS appendEvent helper: it indexes the agent
// through replaceForSession (so the pane and generation guards see it) and then
// appends one event for it.
func (h *orchestratorHarness) appendEvent(
	t *testing.T,
	input orchestratorAppendInput,
) observability.AgentEventRecord {
	t.Helper()
	workspaceID := "wB"
	if input.workspaceID != nil {
		workspaceID = *input.workspaceID
	}
	paneID := workspaceID + ":" + input.terminalID
	agentKind := "codex"
	if input.agent != nil {
		agentKind = *input.agent
	}

	sessionName := orchestratorTestSession
	existing, err := h.agents.List(observability.AgentQueryScope{
		All:              boolPtr(true),
		HerdrSessionName: &sessionName,
	})
	if err != nil {
		t.Fatalf("agents.list: %v", err)
	}
	snapshots := make([]db.HerdrAgentLike, 0, len(existing)+1)
	for _, agent := range existing {
		snapshots = append(snapshots, db.HerdrAgentLike{
			"agent":        snapshotString(agent.Agent),
			"agent_status": string(agent.AgentStatus),
			"focused":      agent.Focused,
			"name":         snapshotString(agent.Name),
			"pane_id":      agent.PaneID,
			"terminal_id":  snapshotString(agent.TerminalID),
			"workspace_id": agent.WorkspaceID,
		})
	}
	snapshot := db.HerdrAgentLike{
		"agent":        agentKind,
		"agent_status": "working",
		"focused":      false,
		"name":         input.terminalID,
		"pane_id":      paneID,
		"terminal_id":  input.terminalID,
		"workspace_id": workspaceID,
	}
	if input.sessionPath != nil {
		snapshot["agent_session"] = map[string]any{
			"agent":  agentKind,
			"kind":   "path",
			"source": "test",
			"value":  *input.sessionPath,
		}
	}
	snapshots = append(snapshots, snapshot)
	replaced, err := h.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents:           snapshots,
		HerdrSessionName: orchestratorTestSession,
	})
	if err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	var indexed *observability.AgentIndexRecord
	for i, candidate := range replaced {
		if candidate.TerminalID != nil && *candidate.TerminalID == input.terminalID {
			indexed = &replaced[i]
		}
	}
	if indexed == nil {
		t.Fatalf("expected indexed agent for terminal %s", input.terminalID)
	}

	// emptyCompactHistory("antigravity-sqlite") plus the default assistant
	// message, exactly like the TS helper's defaultCompactHistory.
	defaultHistory := observability.CompactAgentHistory{
		Source: stringPtr("antigravity-sqlite"),
		LastAssistantMessage: &observability.AgentHistoryExcerpt{
			Ref:  "ref-default",
			Text: "default message",
		},
	}
	var compactHistory *observability.CompactAgentHistory
	if !input.hasHistory {
		compactHistory = &defaultHistory
	} else {
		compactHistory = input.compactHistory
	}
	payload := map[string]any{}
	eventType := observability.AgentEventDone
	if input.eventType != nil {
		eventType = *input.eventType
	}
	if eventType == observability.AgentEventIdle {
		from := "working"
		if input.from != nil {
			from = *input.from
		}
		payload = map[string]any{"from": from}
	}
	event, err := h.agentEvents.Append(db.AppendAgentEventInput{
		AgentID:          &indexed.ID,
		CompactHistory:   compactHistory,
		HerdrSessionName: orchestratorTestSession,
		PaneID:           &paneID,
		Payload:          payload,
		TerminalID:       &input.terminalID,
		Type:             eventType,
		WorkspaceID:      &workspaceID,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return event
}

// emptyCompactHistory mirrors emptyCompactHistory of src/agent-history/service.ts.
func emptyCompactHistory(source string) observability.CompactAgentHistory {
	return observability.CompactAgentHistory{
		Source: &source,
	}
}

// withAssistantMessage mirrors `{...emptyCompactHistory(source), lastAssistantMessage: {...}}`.
func withAssistantMessage(
	source string,
	text *string,
) observability.CompactAgentHistory {
	history := emptyCompactHistory(source)
	if text != nil {
		history.LastAssistantMessage = &observability.AgentHistoryExcerpt{
			Ref:  "history",
			Text: *text,
		}
	}
	return history
}

// setEventColumn mirrors the raw `update agent_events set ... where id = ?`
// statements of the TS tests.
func (h *orchestratorHarness) setEventColumn(t *testing.T, id int64, column string, value any) {
	t.Helper()
	if _, err := h.sqlite.Exec(
		"update agent_events set "+column+" = ? where id = ?", value, id,
	); err != nil {
		t.Fatalf("update agent_events %s: %v", column, err)
	}
}

func (h *orchestratorHarness) countEvents(t *testing.T) int {
	t.Helper()
	var count int
	if err := h.sqlite.QueryRow("select count(*) from agent_events").Scan(&count); err != nil {
		t.Fatalf("count agent_events: %v", err)
	}
	return count
}

// requirePendingIDs asserts the pending result is exactly the given ids, in order.
func requirePendingIDs(t *testing.T, pending []observability.AgentEventRecord, want ...int64) {
	t.Helper()
	got := make([]int64, 0, len(pending))
	for _, event := range pending {
		got = append(got, event.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("pending ids = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("pending ids = %v, want %v", got, want)
		}
	}
}

// requireAckError asserts the ack failure carries the expected code and message.
func requireAckError(t *testing.T, err error, code observability.OrchestratorAckErrorCode, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ack succeeded, want %s", code)
	}
	var ackErr *observability.OrchestratorAckError
	if !errors.As(err, &ackErr) {
		t.Fatalf("ack error = %v (%T), want *OrchestratorAckError", err, err)
	}
	if ackErr.Code != code {
		t.Fatalf("ack error code = %s, want %s", ackErr.Code, code)
	}
	if message != "" && ackErr.Message != message {
		t.Fatalf("ack error message = %q, want %q", ackErr.Message, message)
	}
	if retryable := observability.OrchestratorAckErrorCodeRetryable[code]; ackErr.Retryable() != retryable {
		t.Fatalf("ack error retryable = %v, want %v", ackErr.Retryable(), retryable)
	}
}

// requireAckMessage asserts the ack failure carries the expected message text.
func requireAckMessage(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("ack succeeded, want error %q", message)
	}
	if !strings.Contains(err.Error(), message) {
		t.Fatalf("ack error = %q, want it to contain %q", err.Error(), message)
	}
}

func boolPtr(value bool) *bool { return &value }

func stringPtr(value string) *string { return &value }

// snapshotString renders an optional record field the way the TS harness's
// object literals do: an absent field stays absent.
func snapshotString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// nowMinus renders a timestamp the given number of milliseconds in the past, the
// way the TS tests' `Date.now() - n` raw updates do.
func nowMinus(ms int64) int64 {
	return time.Now().UnixMilli() - ms
}

// staleAttemptAt mirrors `Date.now() - 120_000`: older than the 60s reclaim
// timeout but inside the 300s redelivery window.
func staleAttemptAt() int64 {
	return nowMinus(120_000)
}

// futureAttemptAt mirrors `Date.now() + 60_000`: a delivery backoff that has not
// elapsed yet.
func futureAttemptAt() int64 {
	return time.Now().UnixMilli() + 60_000
}

func TestAgentOrchestratorService(t *testing.T) {
	t.Run("does not reclaim a stale delivery while its owner agent is working", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents: []db.HerdrAgentLike{{
				"agent":        "codex",
				"agent_status": "working",
				"pane_id":      "wB:p-owner",
				"terminal_id":  "term_owner",
				"workspace_id": "wB",
			}},
			HerdrSessionName: orchestratorTestSession,
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent"})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, event.ID)

		harness.setEventColumn(t, event.ID, "last_attempt_at", staleAttemptAt())

		for round := 0; round < 2; round += 1 {
			pending, err = harness.service.Pending(observability.OrchestratorPendingInput{
				AgentScope: orchestratorScope,
				TerminalID: "term_owner",
			})
			if err != nil {
				t.Fatalf("pending (round %d): %v", round, err)
			}
			requirePendingIDs(t, pending, event.ID)
			if pending[0].DeliveryAttempts == nil || *pending[0].DeliveryAttempts != 1 {
				t.Fatalf("deliveryAttempts = %v, want 1", pending[0].DeliveryAttempts)
			}
		}
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusDelivered {
			t.Fatalf("status = %v, want delivered", stored.Status)
		}
		if stored.DeliveryAttempts == nil || *stored.DeliveryAttempts != 1 {
			t.Fatalf("deliveryAttempts = %v, want 1", stored.DeliveryAttempts)
		}
		if stored.DeliveredToTerminalID == nil || *stored.DeliveredToTerminalID != "term_owner" {
			t.Fatalf("deliveredToTerminalId = %v, want term_owner", stored.DeliveredToTerminalID)
		}
	})

	t.Run("keeps a stale delivery with its owner across a turn boundary while the scope is held", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents: []db.HerdrAgentLike{{
				"agent":        "codex",
				"agent_status": "working",
				"pane_id":      "wB:p-owner",
				"terminal_id":  "term_owner",
				"workspace_id": "wB",
			}},
			HerdrSessionName: orchestratorTestSession,
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent"})
		if _, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("pending: %v", err)
		}
		harness.setEventColumn(t, event.ID, "last_attempt_at", staleAttemptAt())
		if _, err := harness.agents.UpdateStatus(db.UpdateAgentStatusInput{
			AgentStatus:      observability.AgentStatusIdle,
			HerdrSessionName: orchestratorTestSession,
			PaneID:           "wB:p-owner",
		}); err != nil {
			t.Fatalf("updateStatus: %v", err)
		}

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, event.ID)
		if pending[0].DeliveryAttempts == nil || *pending[0].DeliveryAttempts != 1 {
			t.Fatalf("deliveryAttempts = %v, want 1", pending[0].DeliveryAttempts)
		}
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusDelivered {
			t.Fatalf("status = %v, want delivered", stored.Status)
		}
		if stored.DeliveredToTerminalID == nil || *stored.DeliveredToTerminalID != "term_owner" {
			t.Fatalf("deliveredToTerminalId = %v, want term_owner", stored.DeliveredToTerminalID)
		}
	})

	t.Run("reclaims a stale delivery after the owner disconnects", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents: []db.HerdrAgentLike{{
				"agent":        "codex",
				"agent_status": "working",
				"pane_id":      "wB:p-owner",
				"terminal_id":  "term_owner",
				"workspace_id": "wB",
			}},
			HerdrSessionName: orchestratorTestSession,
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent"})
		if _, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("pending: %v", err)
		}
		harness.setEventColumn(t, event.ID, "last_attempt_at", staleAttemptAt())
		if _, err := harness.agents.RetirePane(db.RetirePaneInput{
			HerdrSessionName: orchestratorTestSession,
			PaneID:           "wB:p-owner",
		}); err != nil {
			t.Fatalf("retirePane: %v", err)
		}
		if _, err := harness.service.Release(observability.OrchestratorReleaseInput{
			AgentScope: orchestratorScope,
			Reason:     observability.OrchestratorChangeDisconnected,
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("release: %v", err)
		}
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-new-owner",
			TerminalID: "term_new_owner",
		}); err != nil {
			t.Fatalf("re-claim: %v", err)
		}
		if _, err := harness.sqlite.Exec(
			"update agent_orchestrator_scopes set acked_event_id = 0"+
				" where herdr_session_name = ? and workspace_id = ?",
			orchestratorScope.HerdrSessionName, orchestratorScope.WorkspaceID,
		); err != nil {
			t.Fatalf("reset acked_event_id: %v", err)
		}

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_new_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, event.ID)
		if pending[0].DeliveryAttempts == nil || *pending[0].DeliveryAttempts != 2 {
			t.Fatalf("deliveryAttempts = %v, want 2", pending[0].DeliveryAttempts)
		}
	})

	t.Run("initializes once, replaces owners, and releases only the current owner", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		baseline := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent"})

		status, err := harness.service.Status(orchestratorScope)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if status != nil {
			t.Fatalf("status = %+v, want nil", status)
		}
		first, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p1",
			TerminalID: "term_a",
		})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if first.Reason != observability.OrchestratorChangeClaimed {
			t.Fatalf("reason = %s, want claimed", first.Reason)
		}
		if first.Current.AckedEventID != baseline.ID {
			t.Fatalf("current.ackedEventId = %d, want %d", first.Current.AckedEventID, baseline.ID)
		}
		if first.Current.Owner == nil ||
			first.Current.Owner.PaneID != "wB:p1" ||
			first.Current.Owner.TerminalID != "term_a" {
			t.Fatalf("current.owner = %+v, want wB:p1/term_a", first.Current.Owner)
		}
		if first.Previous.AckedEventID != baseline.ID || first.Previous.Owner != nil {
			t.Fatalf("previous = %+v, want acked %d and no owner",
				first.Previous, baseline.ID)
		}

		same, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p1",
			TerminalID: "term_a",
		})
		if err != nil {
			t.Fatalf("re-claim: %v", err)
		}
		if same.Current.AckedEventID != baseline.ID ||
			same.Current.Owner == nil ||
			same.Current.Owner.TerminalID != "term_a" {
			t.Fatalf("same.current = %+v, want acked %d owned by term_a",
				same.Current, baseline.ID)
		}
		if same.Previous.Owner == nil || same.Previous.Owner.TerminalID != "term_a" {
			t.Fatalf("same.previous.owner = %+v, want term_a", same.Previous.Owner)
		}

		replacement, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p2",
			TerminalID: "term_b",
		})
		if err != nil {
			t.Fatalf("replacement claim: %v", err)
		}
		if replacement.Current.Owner == nil ||
			replacement.Current.Owner.TerminalID != "term_b" {
			t.Fatalf("replacement.current.owner = %+v, want term_b", replacement.Current.Owner)
		}
		if replacement.Previous.Owner == nil ||
			replacement.Previous.Owner.TerminalID != "term_a" {
			t.Fatalf("replacement.previous.owner = %+v, want term_a", replacement.Previous.Owner)
		}

		released, err := harness.service.Release(observability.OrchestratorReleaseInput{
			AgentScope: orchestratorScope,
			Reason:     observability.OrchestratorChangeReleased,
			TerminalID: "term_a",
		})
		if err != nil {
			t.Fatalf("release (stale terminal): %v", err)
		}
		if released != nil {
			t.Fatalf("release = %+v, want nil", released)
		}
		released, err = harness.service.Release(observability.OrchestratorReleaseInput{
			AgentScope: orchestratorScope,
			Reason:     observability.OrchestratorChangeReleased,
			TerminalID: "term_b",
		})
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		if released == nil {
			t.Fatalf("release = nil, want a change")
		}
		if released.Current.Owner != nil {
			t.Fatalf("release.current.owner = %+v, want nil", released.Current.Owner)
		}
		if released.Reason != observability.OrchestratorChangeReleased {
			t.Fatalf("release.reason = %s, want released", released.Reason)
		}
	})

	t.Run("does not deliver interactive Pi observer events", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		observerEvent := harness.appendEvent(t, orchestratorAppendInput{
			agent:       stringPtr("pi"),
			sessionPath: stringPtr("/root/.pi/agent/sessions/interactive.jsonl"),
			terminalID:  "term_other",
			eventType:   eventTypePtr(observability.AgentEventIdle),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending)

		_, err = harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    observerEvent.ID,
			TerminalID: "term_owner",
		})
		requireAckMessage(t, err, observability.MsgAckOutOfOrder)
	})

	t.Run("still delivers and acknowledges dispatched Pi role events", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		workerEvent := harness.appendEvent(t, orchestratorAppendInput{
			agent:       stringPtr("pi"),
			sessionPath: stringPtr("/tmp/herdr-role-sessions/role-worker-fd92d978/session.jsonl"),
			terminalID:  "term_worker",
			from:        stringPtr("working"),
			eventType:   eventTypePtr(observability.AgentEventIdle),
		})
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, workerEvent.ID)

		state, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    workerEvent.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack: %v", err)
		}
		if state.AckedEventID != workerEvent.ID {
			t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, workerEvent.ID)
		}
	})

	t.Run("filters idle events unless they transition from working", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		history := withAssistantMessage("pi-jsonl", stringPtr("codex completed task"))
		working := harness.appendEvent(t, orchestratorAppendInput{
			agent:          stringPtr("codex"),
			compactHistory: &history,
			terminalID:     "term_other",
			from:           stringPtr("working"),
			eventType:      eventTypePtr(observability.AgentEventIdle),
		})
		unknown := harness.appendEvent(t, orchestratorAppendInput{
			agent:          stringPtr("codex"),
			compactHistory: &history,
			terminalID:     "term_unknown",
			from:           stringPtr("unknown"),
			eventType:      eventTypePtr(observability.AgentEventIdle),
		})
		blocked := harness.appendEvent(t, orchestratorAppendInput{
			agent:          stringPtr("codex"),
			compactHistory: &history,
			terminalID:     "term_blocked",
			from:           stringPtr("blocked"),
			eventType:      eventTypePtr(observability.AgentEventIdle),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, working.ID)
		if unknown.ID == working.ID || blocked.ID == working.ID {
			t.Fatalf("unexpected event id collision")
		}
	})

	t.Run("returns ordered non-self pending events across bounded scan pages", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p1",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		for index := 0; index < 105; index += 1 {
			harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_owner"})
		}
		// An event without a terminal can never be delivered to a scope owner.
		if _, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			HerdrSessionName: orchestratorTestSession,
			Payload:          map[string]any{},
			Type:             observability.AgentEventDone,
			WorkspaceID:      stringPtr("wB"),
		}); err != nil {
			t.Fatalf("append orphan: %v", err)
		}
		agentEvent := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent"})
		harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_other",
			workspaceID: stringPtr("wC"),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, agentEvent.ID)

		pending, err = harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_agent",
		})
		if err != nil {
			t.Fatalf("pending (agent terminal): %v", err)
		}
		requirePendingIDs(t, pending)
	})

	t.Run("preserves ownerless gap events across release and re-claim", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p1",
			TerminalID: "term_a",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if _, err := harness.service.Release(observability.OrchestratorReleaseInput{
			AgentScope: orchestratorScope,
			Reason:     observability.OrchestratorChangeReleased,
			TerminalID: "term_a",
		}); err != nil {
			t.Fatalf("release: %v", err)
		}
		firstGap := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent"})
		ownerlessLater := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent_2"})
		reclaimed, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p2",
			TerminalID: "term_b",
		})
		if err != nil {
			t.Fatalf("re-claim: %v", err)
		}

		// claimCursor keeps the scope's acked cursor while the row exists (owner
		// null), so events appended during the ownerless gap are picked up after
		// the re-claim instead of being skipped by a jump to latestEventId.
		if reclaimed.Current.AckedEventID != 0 {
			t.Fatalf("reclaimed.current.ackedEventId = %d, want 0",
				reclaimed.Current.AckedEventID)
		}
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_b",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, firstGap.ID, ownerlessLater.ID)

		// The reclaimed owner acknowledges the preserved gap events in order.
		state, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    firstGap.ID,
			TerminalID: "term_b",
		})
		if err != nil {
			t.Fatalf("ack (first gap): %v", err)
		}
		if state.AckedEventID != firstGap.ID {
			t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, firstGap.ID)
		}
		state, err = harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    ownerlessLater.ID,
			TerminalID: "term_b",
		})
		if err != nil {
			t.Fatalf("ack (ownerless later): %v", err)
		}
		if state.AckedEventID != ownerlessLater.ID {
			t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, ownerlessLater.ID)
		}

		// Subsequent replacement claims keep advancing from the acked cursor.
		pendingEvent := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent"})
		later := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent_2"})
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p3",
			TerminalID: "term_c",
		}); err != nil {
			t.Fatalf("third claim: %v", err)
		}
		// While both are still undelivered, an out-of-order ack is rejected.
		_, err = harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    later.ID,
			TerminalID: "term_c",
		})
		requireAckMessage(t, err, observability.MsgAckOutOfOrder)
		_, err = harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    later.ID + 10_000,
			TerminalID: "term_c",
		})
		requireAckMessage(t, err, observability.MsgAckOutOfOrder)

		// Once delivered to the owner, the batch ack may skip the intermediate event.
		pending, err = harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_c",
		})
		if err != nil {
			t.Fatalf("pending (third owner): %v", err)
		}
		requirePendingIDs(t, pending, pendingEvent.ID, later.ID)
		state, err = harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    later.ID,
			TerminalID: "term_c",
		})
		if err != nil {
			t.Fatalf("ack (later): %v", err)
		}
		if state.AckedEventID != later.ID {
			t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, later.ID)
		}
		state, err = harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    later.ID,
			TerminalID: "term_c",
		})
		if err != nil {
			t.Fatalf("ack (later, repeated): %v", err)
		}
		if state.AckedEventID != later.ID {
			t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, later.ID)
		}
		_, err = harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    later.ID,
			TerminalID: "term_b",
		})
		requireAckError(t, err, observability.AckNotOwner, observability.MsgAckNotOwner)
	})

	t.Run("filters deleted and rebound agents, and ack advances past filtered events", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		initial, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents: []db.HerdrAgentLike{{
				"agent":        "codex",
				"pane_id":      "wB:p1",
				"terminal_id":  "term_agent",
				"workspace_id": "wB",
			}},
			HerdrSessionName: orchestratorTestSession,
		})
		if err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		if len(initial) == 0 {
			t.Fatalf("expected indexed agent")
		}
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		deleted, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			AgentID: &initial[0].ID,
			CompactHistory: compactHistoryPtr(withAssistantMessage(
				"antigravity-sqlite", stringPtr("done"))),
			HerdrSessionName: orchestratorTestSession,
			PaneID:           stringPtr("wB:p1"),
			Payload:          map[string]any{},
			TerminalID:       stringPtr("term_agent"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      stringPtr("wB"),
		})
		if err != nil {
			t.Fatalf("append (deleted): %v", err)
		}
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents:           []db.HerdrAgentLike{},
			HerdrSessionName: orchestratorTestSession,
		}); err != nil {
			t.Fatalf("replaceForSession (empty): %v", err)
		}
		visible, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents: []db.HerdrAgentLike{{
				"agent":        "codex",
				"pane_id":      "wB:p2",
				"terminal_id":  "term_agent",
				"workspace_id": "wB",
			}},
			HerdrSessionName: orchestratorTestSession,
		})
		if err != nil {
			t.Fatalf("replaceForSession (rebound): %v", err)
		}
		if len(visible) == 0 {
			t.Fatalf("expected rebound agent")
		}
		visibleEvent, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			AgentID: &visible[0].ID,
			CompactHistory: compactHistoryPtr(withAssistantMessage(
				"antigravity-sqlite", stringPtr("done"))),
			HerdrSessionName: orchestratorTestSession,
			PaneID:           stringPtr("wB:p2"),
			Payload:          map[string]any{},
			TerminalID:       stringPtr("term_agent"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      stringPtr("wB"),
		})
		if err != nil {
			t.Fatalf("append (visible): %v", err)
		}
		if got := harness.countEvents(t); got != 2 {
			t.Fatalf("agent_events rows = %d, want 2", got)
		}
		listed, err := harness.agents.List(observability.AgentQueryScope{
			HerdrSessionName: stringPtr(orchestratorTestSession),
			WorkspaceID:      stringPtr("wB"),
		})
		if err != nil {
			t.Fatalf("agents.list: %v", err)
		}
		if len(listed) != 1 {
			t.Fatalf("agents = %d, want 1", len(listed))
		}
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, visibleEvent.ID)
		state, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    visibleEvent.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack: %v", err)
		}
		if state.AckedEventID != visibleEvent.ID {
			t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, visibleEvent.ID)
		}
		if deleted.ID >= visibleEvent.ID {
			t.Fatalf("deleted.id = %d, want < %d", deleted.ID, visibleEvent.ID)
		}
	})

	t.Run("does not deliver an event after its agent is rebound to another pane", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		agent, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents: []db.HerdrAgentLike{{
				"agent":        "codex",
				"pane_id":      "wB:p1",
				"terminal_id":  "term_agent",
				"workspace_id": "wB",
			}},
			HerdrSessionName: orchestratorTestSession,
		})
		if err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		if len(agent) == 0 {
			t.Fatalf("expected indexed agent")
		}
		event, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			AgentID:          &agent[0].ID,
			HerdrSessionName: orchestratorTestSession,
			PaneID:           stringPtr("wB:p1"),
			Payload:          map[string]any{},
			TerminalID:       stringPtr("term_agent"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      stringPtr("wB"),
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents: []db.HerdrAgentLike{{
				"agent":        "codex",
				"pane_id":      "wB:p2",
				"terminal_id":  "term_agent",
				"workspace_id": "wB",
			}},
			HerdrSessionName: orchestratorTestSession,
		}); err != nil {
			t.Fatalf("replaceForSession (rebound): %v", err)
		}
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending)
		if event.ID <= 0 {
			t.Fatalf("event.id = %d, want > 0", event.ID)
		}
	})

	t.Run("moves ownership with target gap-event preservation and active-owner preservation", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p1",
			TerminalID: "term_a",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		sourceEvent := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent"})
		if _, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    sourceEvent.ID,
			TerminalID: "term_a",
		}); err != nil {
			t.Fatalf("ack: %v", err)
		}
		target := observability.AgentScope{
			HerdrSessionName: orchestratorTestSession,
			WorkspaceID:      "wC",
		}
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: target,
			PaneID:     "wC:p1",
			TerminalID: "term_c",
		}); err != nil {
			t.Fatalf("claim (target): %v", err)
		}
		if _, err := harness.service.Release(observability.OrchestratorReleaseInput{
			AgentScope: target,
			Reason:     observability.OrchestratorChangeReleased,
			TerminalID: "term_c",
		}); err != nil {
			t.Fatalf("release (target): %v", err)
		}
		ownerlessTargetEvent := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_agent",
			workspaceID: stringPtr("wC"),
		})

		moves, err := harness.service.Move(observability.OrchestratorMoveInput{
			From:       orchestratorScope,
			PaneID:     "wC:p2",
			TerminalID: "term_a",
			To:         target,
		})
		if err != nil {
			t.Fatalf("move: %v", err)
		}
		if len(moves) != 2 {
			t.Fatalf("moves = %d, want 2", len(moves))
		}
		if moves[0].Reason != observability.OrchestratorChangeMoved ||
			moves[1].Reason != observability.OrchestratorChangeMoved {
			t.Fatalf("move reasons = %s/%s, want moved", moves[0].Reason, moves[1].Reason)
		}
		if moves[0].Current.AckedEventID != sourceEvent.ID || moves[0].Current.Owner != nil {
			t.Fatalf("moves[0].current = %+v, want acked %d and no owner",
				moves[0].Current, sourceEvent.ID)
		}
		if moves[1].Current.AckedEventID != 0 {
			t.Fatalf("moves[1].current.ackedEventId = %d, want 0", moves[1].Current.AckedEventID)
		}
		if moves[1].Current.Owner == nil ||
			moves[1].Current.Owner.PaneID != "wC:p2" ||
			moves[1].Current.Owner.TerminalID != "term_a" {
			t.Fatalf("moves[1].current.owner = %+v, want wC:p2/term_a", moves[1].Current.Owner)
		}
		if moves[1].Previous.Owner != nil {
			t.Fatalf("moves[1].previous.owner = %+v, want nil", moves[1].Previous.Owner)
		}

		// The target scope row exists (owner null), so claimCursor preserves its
		// acked cursor instead of jumping to latestEventId: the ownerless gap event
		// survives.
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: target,
			TerminalID: "term_a",
		})
		if err != nil {
			t.Fatalf("pending (moved target): %v", err)
		}
		requirePendingIDs(t, pending, ownerlessTargetEvent.ID)

		ownedTarget := observability.AgentScope{
			HerdrSessionName: orchestratorTestSession,
			WorkspaceID:      "wD",
		}
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: ownedTarget,
			PaneID:     "wD:p1",
			TerminalID: "term_d",
		}); err != nil {
			t.Fatalf("claim (owned target): %v", err)
		}
		pendingTargetEvent := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_agent",
			workspaceID: stringPtr("wD"),
		})
		if _, err := harness.service.Move(observability.OrchestratorMoveInput{
			From:       target,
			PaneID:     "wD:p2",
			TerminalID: "term_a",
			To:         ownedTarget,
		}); err != nil {
			t.Fatalf("move (owned target): %v", err)
		}
		pending, err = harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: ownedTarget,
			TerminalID: "term_a",
		})
		if err != nil {
			t.Fatalf("pending (owned target): %v", err)
		}
		requirePendingIDs(t, pending, pendingTargetEvent.ID)

		unseen := observability.AgentScope{
			HerdrSessionName: orchestratorTestSession,
			WorkspaceID:      "wE",
		}
		unseenBaseline := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_agent",
			workspaceID: stringPtr("wE"),
		})
		if _, err := harness.service.Move(observability.OrchestratorMoveInput{
			From:       ownedTarget,
			PaneID:     "wE:p1",
			TerminalID: "term_a",
			To:         unseen,
		}); err != nil {
			t.Fatalf("move (unseen): %v", err)
		}
		status, err := harness.service.Status(unseen)
		if err != nil {
			t.Fatalf("status (unseen): %v", err)
		}
		if status == nil || status.AckedEventID != unseenBaseline.ID {
			t.Fatalf("status (unseen) = %+v, want acked %d", status, unseenBaseline.ID)
		}
	})

	t.Run("lists persisted owners across sessions", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		harness.upsertRunningSession(t, "other")
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p1",
			TerminalID: "term_a",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: observability.AgentScope{
				HerdrSessionName: "other",
				WorkspaceID:      "wX",
			},
			PaneID:     "wX:p1",
			TerminalID: "term_x",
		}); err != nil {
			t.Fatalf("claim (other session): %v", err)
		}

		owners, err := harness.service.PersistedOwners()
		if err != nil {
			t.Fatalf("persistedOwners: %v", err)
		}
		if len(owners) != 2 {
			t.Fatalf("persistedOwners = %d, want 2", len(owners))
		}
	})
}

func TestAgentOrchestratorServiceAckRegression(t *testing.T) {
	t.Run("rejects non-owner acknowledgement with its structured contract", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    1,
			TerminalID: "term_not_owner",
		})
		requireAckError(t, err, observability.AckNotOwner,
			observability.MsgAckNotOwner)
	})

	t.Run("rejects an invalidated acknowledgement with its structured contract", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_worker"})
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents:           []db.HerdrAgentLike{},
			HerdrSessionName: orchestratorTestSession,
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}

		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		})
		requireAckError(t, err, observability.AckEventInvalidated,
			observability.MsgAckInvalidated)
	})

	t.Run("rejects an out-of-order acknowledgement with its structured contract", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_first"})
		later := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_second"})

		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    later.ID,
			TerminalID: "term_owner",
		})
		requireAckError(t, err, observability.AckEventOutOfOrder,
			observability.MsgAckOutOfOrder)
	})

	t.Run("not-found and out-of-scope rejections reuse the legacy retry wording (known compatibility behavior)", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		otherScopeEvent := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_other",
			workspaceID: stringPtr("wC"),
		})
		// A workspace owned by another terminal stays out of scope; the
		// ownerless-sibling rule only covers workspaces without their own owner.
		if _, err := harness.scopes.Claim(db.ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: observability.AgentScope{
				HerdrSessionName: orchestratorTestSession,
				WorkspaceID:      "wC",
			},
			AckedEventID: 0,
			PaneID:       "wC:owner",
			TerminalID:   "term_other_owner",
		}); err != nil {
			t.Fatalf("claim (other workspace): %v", err)
		}

		// Legacy wording is reused from out-of-order; old clients treat these as
		// retryable. This known compatibility behavior is covered by the
		// subsequent state-machine batch.
		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    99_999,
			TerminalID: "term_owner",
		})
		requireAckError(t, err, observability.AckEventNotFound,
			observability.MsgAckOutOfOrder)
		_, err = harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    otherScopeEvent.ID,
			TerminalID: "term_owner",
		})
		requireAckError(t, err, observability.AckEventNotInScope,
			observability.MsgAckOutOfOrder)
	})

	t.Run("already acknowledged events are idempotent without side effects", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_worker"})
		first, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack: %v", err)
		}
		eventCount := harness.countEvents(t)

		again, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack (repeated): %v", err)
		}
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("ack (repeated) = %+v, want %+v", again, first)
		}
		if got := harness.countEvents(t); got != eventCount {
			t.Fatalf("agent_events rows = %d, want %d", got, eventCount)
		}
		status, err := harness.service.Status(orchestratorScope)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if status == nil || !reflect.DeepEqual(*status, first) {
			t.Fatalf("status = %+v, want %+v", status, first)
		}
	})

	t.Run("allows acknowledging an event invalidated after delivery with its retired worker pane", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_worker"})
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, event.ID)
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents:           []db.HerdrAgentLike{},
			HerdrSessionName: orchestratorTestSession,
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}

		state, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack: %v", err)
		}
		if state.AckedEventID != event.ID {
			t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, event.ID)
		}
		status, err := harness.service.Status(orchestratorScope)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if status == nil || status.AckedEventID != event.ID {
			t.Fatalf("status.ackedEventId = %+v, want %d", status, event.ID)
		}
	})

	t.Run("rejects an invalidated event without blocking a later pending event", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_worker"})
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents:           []db.HerdrAgentLike{},
			HerdrSessionName: orchestratorTestSession,
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}

		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		})
		requireAckMessage(t, err, observability.MsgAckInvalidated)

		later := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_later"})
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, later.ID)
		state, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    later.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack (later): %v", err)
		}
		if state.AckedEventID != later.ID {
			t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, later.ID)
		}
	})

	t.Run("still rejects acknowledging past a later deliverable event", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		first := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_first"})
		second := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_second"})

		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    second.ID,
			TerminalID: "term_owner",
		})
		requireAckMessage(t, err, observability.MsgAckOutOfOrder)
		if first.ID >= second.ID {
			t.Fatalf("first.id = %d, want < %d", first.ID, second.ID)
		}
	})

	t.Run("rejects nonexistent and cross-scope event ids", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		otherScopeEvent := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_other",
			workspaceID: stringPtr("wC"),
		})
		if _, err := harness.scopes.Claim(db.ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: observability.AgentScope{
				HerdrSessionName: orchestratorTestSession,
				WorkspaceID:      "wC",
			},
			AckedEventID: 0,
			PaneID:       "wC:owner",
			TerminalID:   "term_other_owner",
		}); err != nil {
			t.Fatalf("claim (other workspace): %v", err)
		}

		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    otherScopeEvent.ID,
			TerminalID: "term_owner",
		})
		requireAckMessage(t, err, observability.MsgAckOutOfOrder)
		_, err = harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    otherScopeEvent.ID + 10_000,
			TerminalID: "term_owner",
		})
		requireAckMessage(t, err, observability.MsgAckOutOfOrder)
	})

	t.Run("repeated acknowledgement is idempotent and never moves the cursor backward", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_worker"})
		first, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack: %v", err)
		}

		again, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack (repeated): %v", err)
		}
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("ack (repeated) = %+v, want %+v", again, first)
		}
	})

	t.Run("allows a batch ack to skip a stuck delivered event (M1)", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		stuck := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_first"})
		second := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_second"})
		third := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_third"})
		// Deliver the first event to the owner but never ack it (stuck delivery).
		if _, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("pending: %v", err)
		}
		stored, err := harness.agentEvents.Get(stuck.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusDelivered {
			t.Fatalf("status = %v, want delivered", stored.Status)
		}

		// Acking a later event is allowed now: the next candidate is already
		// delivered to this owner, and markAcked's id <= cursor semantics
		// acknowledges the intermediate delivered/pending events too.
		state, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    third.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack (third): %v", err)
		}
		if state.AckedEventID != third.ID {
			t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, third.ID)
		}
		for _, event := range []observability.AgentEventRecord{stuck, second, third} {
			stored, err := harness.agentEvents.Get(event.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if stored.Status == nil || *stored.Status != observability.AgentEventStatusAcked {
				t.Fatalf("event %d status = %v, want acked", event.ID, stored.Status)
			}
		}
	})

	t.Run("still rejects an out-of-order ack that skips an undelivered pending event (M1)", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		first := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_first"})
		second := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_second"})

		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    second.ID,
			TerminalID: "term_owner",
		})
		requireAckError(t, err, observability.AckEventOutOfOrder,
			observability.MsgAckOutOfOrder)
		if first.ID >= second.ID {
			t.Fatalf("first.id = %d, want < %d", first.ID, second.ID)
		}
		for _, event := range []observability.AgentEventRecord{first, second} {
			stored, err := harness.agentEvents.Get(event.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if stored.Status == nil || *stored.Status != observability.AgentEventStatusPending {
				t.Fatalf("event %d status = %v, want pending", event.ID, stored.Status)
			}
		}
	})
}

func TestAgentOrchestratorServiceCrossWorkspaceAcknowledgement(t *testing.T) {
	t.Run("rejects an ack that skips a lower pending event of the ownerless sibling workspace", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		own := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_w",
			workspaceID: stringPtr("wB"),
		})
		sibling := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_s",
			workspaceID: stringPtr("wD"),
		})
		if own.ID >= sibling.ID {
			t.Fatalf("own.id = %d, want < %d", own.ID, sibling.ID)
		}

		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    sibling.ID,
			TerminalID: "term_owner",
		})
		requireAckError(t, err, observability.AckEventOutOfOrder,
			observability.MsgAckOutOfOrder)
		for _, event := range []observability.AgentEventRecord{own, sibling} {
			stored, err := harness.agentEvents.Get(event.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if stored.Status == nil || *stored.Status != observability.AgentEventStatusPending {
				t.Fatalf("event %d status = %v, want pending", event.ID, stored.Status)
			}
		}
	})

	t.Run("rejects an ack of the owner workspace event while a lower sibling event is pending", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		sibling := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_s",
			workspaceID: stringPtr("wD"),
		})
		own := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_w",
			workspaceID: stringPtr("wB"),
		})
		if sibling.ID >= own.ID {
			t.Fatalf("sibling.id = %d, want < %d", sibling.ID, own.ID)
		}

		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    own.ID,
			TerminalID: "term_owner",
		})
		requireAckError(t, err, observability.AckEventOutOfOrder,
			observability.MsgAckOutOfOrder)
		for _, event := range []observability.AgentEventRecord{sibling, own} {
			stored, err := harness.agentEvents.Get(event.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if stored.Status == nil || *stored.Status != observability.AgentEventStatusPending {
				t.Fatalf("event %d status = %v, want pending", event.ID, stored.Status)
			}
		}
	})

	t.Run("settles only the acknowledged sibling row and never advances the owner cursor", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		deferred := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_w",
			workspaceID: stringPtr("wB"),
		})
		// Not deliverable right now (delivery backoff), so the ordering guard cannot
		// see it: exactly the owner-workspace row the scope-cursor ack used to
		// swallow.
		harness.setEventColumn(t, deferred.ID, "next_attempt_at", futureAttemptAt())
		sibling := harness.appendEvent(t, orchestratorAppendInput{
			terminalID:  "term_s",
			workspaceID: stringPtr("wD"),
		})
		if deferred.ID >= sibling.ID {
			t.Fatalf("deferred.id = %d, want < %d", deferred.ID, sibling.ID)
		}

		state, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    sibling.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack (sibling): %v", err)
		}
		if state.AckedEventID != 0 {
			t.Fatalf("ackedEventId = %d, want 0", state.AckedEventID)
		}
		status, err := harness.service.Status(orchestratorScope)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if status == nil || status.AckedEventID != 0 {
			t.Fatalf("status.ackedEventId = %+v, want 0", status)
		}
		stored, err := harness.agentEvents.Get(sibling.ID)
		if err != nil {
			t.Fatalf("get (sibling): %v", err)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusAcked {
			t.Fatalf("sibling status = %v, want acked", stored.Status)
		}
		stored, err = harness.agentEvents.Get(deferred.ID)
		if err != nil {
			t.Fatalf("get (deferred): %v", err)
		}
		if stored.Deliverable != 1 {
			t.Fatalf("deferred deliverable = %d, want 1", stored.Deliverable)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusPending {
			t.Fatalf("deferred status = %v, want pending", stored.Status)
		}

		// The deferred event is still reachable once its backoff expires.
		harness.setEventColumn(t, deferred.ID, "next_attempt_at", nil)
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, deferred.ID)
	})
}

func TestAgentOrchestratorServiceInvalidatedAcknowledgementWording(t *testing.T) {
	t.Run("uses invalidated wording for an event invalidated before delivery", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		known := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_worker"})
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents:           []db.HerdrAgentLike{},
			HerdrSessionName: orchestratorTestSession,
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}

		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    known.ID,
			TerminalID: "term_owner",
		})
		requireAckMessage(t, err, observability.MsgAckInvalidated)
	})

	t.Run("uses the original next-pending wording for an unknown event", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}

		_, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    99_999,
			TerminalID: "term_owner",
		})
		requireAckMessage(t, err, observability.MsgAckOutOfOrder)
	})

	t.Run("advances cursor when event was delivered and subsequently invalidated via invalidatePane", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_worker"})
		// Reserve/deliver to term_owner.
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, event.ID)
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusDelivered {
			t.Fatalf("status = %v, want delivered", stored.Status)
		}
		if stored.DeliveredToTerminalID == nil || *stored.DeliveredToTerminalID != "term_owner" {
			t.Fatalf("deliveredToTerminalId = %v, want term_owner",
				stored.DeliveredToTerminalID)
		}

		// Pane closed / invalidated.
		if err := harness.agentEvents.InvalidatePane(db.InvalidatePaneInput{
			HerdrSessionName:  orchestratorScope.HerdrSessionName,
			PaneID:            eventPaneID(event),
			InvalidatedReason: stringPtr("PANE_CLOSED"),
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}
		stored, err = harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusInvalidated {
			t.Fatalf("status = %v, want invalidated", stored.Status)
		}
		if stored.DeliveredToTerminalID == nil || *stored.DeliveredToTerminalID != "term_owner" {
			t.Fatalf("deliveredToTerminalId = %v, want term_owner",
				stored.DeliveredToTerminalID)
		}

		// ACK advances ackedEventId cursor.
		result, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack: %v", err)
		}
		if result.AckedEventID != event.ID {
			t.Fatalf("ackedEventId = %d, want %d", result.AckedEventID, event.ID)
		}
		status, err := harness.service.Status(orchestratorScope)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if status == nil || status.AckedEventID != event.ID {
			t.Fatalf("status.ackedEventId = %+v, want %d", status, event.ID)
		}
		stored, err = harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusInvalidated {
			t.Fatalf("status = %v, want invalidated", stored.Status)
		}

		// Repeated ACK is idempotent.
		again, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack (repeated): %v", err)
		}
		if !reflect.DeepEqual(again, result) {
			t.Fatalf("ack (repeated) = %+v, want %+v", again, result)
		}

		// Subsequent pending call does not redeliver.
		pending, err = harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending)
	})

	t.Run("advances cursor when event was delivered and pane generation drifted before ACK", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		agent, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents: []db.HerdrAgentLike{{
				"agent":           "codex",
				"agent_status":    "working",
				"name":            "term_worker",
				"pane_id":         "wB:p_worker",
				"pane_generation": "gen-1",
				"terminal_id":     "term_worker",
				"workspace_id":    "wB",
			}},
			HerdrSessionName: orchestratorTestSession,
		})
		if err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		if len(agent) == 0 {
			t.Fatalf("expected indexed agent")
		}
		event, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			AgentID: &agent[0].ID,
			CompactHistory: compactHistoryPtr(withAssistantMessage(
				"antigravity-sqlite", stringPtr("done"))),
			HerdrSessionName: orchestratorTestSession,
			PaneID:           stringPtr("wB:p_worker"),
			PaneGeneration:   stringPtr("gen-1"),
			Payload:          map[string]any{},
			TerminalID:       stringPtr("term_worker"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      stringPtr("wB"),
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}

		// Reserve/deliver to term_owner.
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, event.ID)
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusDelivered {
			t.Fatalf("status = %v, want delivered", stored.Status)
		}
		if stored.DeliveredToTerminalID == nil || *stored.DeliveredToTerminalID != "term_owner" {
			t.Fatalf("deliveredToTerminalId = %v, want term_owner",
				stored.DeliveredToTerminalID)
		}
		if stored.PaneGeneration == nil || *stored.PaneGeneration != "gen-1" {
			t.Fatalf("paneGeneration = %v, want gen-1", stored.PaneGeneration)
		}

		// Invalidate event for old generation.
		if err := harness.agentEvents.InvalidatePane(db.InvalidatePaneInput{
			HerdrSessionName:  orchestratorScope.HerdrSessionName,
			PaneID:            "wB:p_worker",
			PaneGeneration:    stringPtr("gen-1"),
			InvalidatedReason: stringPtr("PANE_CLOSED"),
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}
		stored, err = harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusInvalidated {
			t.Fatalf("status = %v, want invalidated", stored.Status)
		}
		if stored.DeliveredToTerminalID == nil || *stored.DeliveredToTerminalID != "term_owner" {
			t.Fatalf("deliveredToTerminalId = %v, want term_owner",
				stored.DeliveredToTerminalID)
		}

		// Agent generation drifts to gen-2.
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents: []db.HerdrAgentLike{{
				"agent":           "codex",
				"agent_status":    "working",
				"name":            "term_worker",
				"pane_id":         "wB:p_worker",
				"pane_generation": "gen-2",
				"terminal_id":     "term_worker",
				"workspace_id":    "wB",
			}},
			HerdrSessionName: orchestratorTestSession,
		}); err != nil {
			t.Fatalf("replaceForSession (drift): %v", err)
		}

		// ACK must succeed and advance cursor directly without throwing OUT_OF_ORDER.
		result, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack: %v", err)
		}
		if result.AckedEventID != event.ID {
			t.Fatalf("ackedEventId = %d, want %d", result.AckedEventID, event.ID)
		}
		status, err := harness.service.Status(orchestratorScope)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if status == nil || status.AckedEventID != event.ID {
			t.Fatalf("status.ackedEventId = %+v, want %d", status, event.ID)
		}
		stored, err = harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.Status == nil || *stored.Status != observability.AgentEventStatusInvalidated {
			t.Fatalf("status = %v, want invalidated", stored.Status)
		}
	})
}

func TestAgentOrchestratorServiceAckAndRedeliveryFreshness(t *testing.T) {
	t.Run("ack then immediate orchestrator.get does not return the acked event", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent"})
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, event.ID)
		if _, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    event.ID,
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("ack: %v", err)
		}
		pending, err = harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending (after ack): %v", err)
		}
		requirePendingIDs(t, pending)
	})

	t.Run("pending does not return a delivered event beyond the redelivery freshness window", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, orchestratorAppendInput{terminalID: "term_agent"})
		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, event.ID)
		harness.setEventColumn(t, event.ID, "last_attempt_at",
			nowMinus(db.RedeliveryFreshnessMs+1))

		pending, err = harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending (stale): %v", err)
		}
		requirePendingIDs(t, pending)
	})
}

func TestNonPiAgentCompletedDeliveryConditions(t *testing.T) {
	t.Run("agy (non-pi): working -> idle with empty or missing lastAssistantMessage is not delivered", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}

		// Case 1: compactHistory is null.
		nullHistoryEvent := harness.appendEvent(t, orchestratorAppendInput{
			agent:          stringPtr("agy"),
			compactHistory: nil,
			hasHistory:     true,
			terminalID:     "term_agy_1",
			from:           stringPtr("working"),
			eventType:      eventTypePtr(observability.AgentEventIdle),
		})
		// Case 2: lastAssistantMessage is null.
		emptyHistoryEvent := harness.appendEvent(t, orchestratorAppendInput{
			agent:          stringPtr("agy"),
			compactHistory: compactHistoryPtr(withAssistantMessage("antigravity-sqlite", nil)),
			hasHistory:     true,
			terminalID:     "term_agy_2",
			from:           stringPtr("working"),
			eventType:      eventTypePtr(observability.AgentEventIdle),
		})
		// Case 3: lastAssistantMessage is whitespace only.
		whitespaceHistoryEvent := harness.appendEvent(t, orchestratorAppendInput{
			agent: stringPtr("agy"),
			compactHistory: compactHistoryPtr(withAssistantMessage(
				"antigravity-sqlite", stringPtr("   \n\t  "))),
			hasHistory: true,
			terminalID: "term_agy_3",
			from:       stringPtr("working"),
			eventType:  eventTypePtr(observability.AgentEventIdle),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending)

		// Out-of-order / deliverable checks for ack.
		_, err = harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    nullHistoryEvent.ID,
			TerminalID: "term_owner",
		})
		requireAckMessage(t, err, observability.MsgAckOutOfOrder)
		_ = emptyHistoryEvent
		_ = whitespaceHistoryEvent
	})

	t.Run("agy (non-pi): working -> idle with non-empty lastAssistantMessage is delivered", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		validEvent := harness.appendEvent(t, orchestratorAppendInput{
			agent: stringPtr("agy"),
			compactHistory: compactHistoryPtr(withAssistantMessage(
				"antigravity-sqlite", stringPtr("Task completed successfully"))),
			terminalID: "term_agy_worker",
			from:       stringPtr("working"),
			eventType:  eventTypePtr(observability.AgentEventIdle),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, validEvent.ID)
		state, err := harness.service.Ack(observability.OrchestratorAckInput{
			AgentScope: orchestratorScope,
			EventID:    validEvent.ID,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("ack: %v", err)
		}
		if state.AckedEventID != validEvent.ID {
			t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, validEvent.ID)
		}
	})

	t.Run("agy (non-pi): working -> done with empty or missing lastAssistantMessage is not delivered", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		harness.appendEvent(t, orchestratorAppendInput{
			agent:          stringPtr("agy"),
			compactHistory: compactHistoryPtr(withAssistantMessage("antigravity-sqlite", nil)),
			hasHistory:     true,
			terminalID:     "term_agy_done_null",
			eventType:      eventTypePtr(observability.AgentEventDone),
		})
		harness.appendEvent(t, orchestratorAppendInput{
			agent: stringPtr("agy"),
			compactHistory: compactHistoryPtr(withAssistantMessage(
				"antigravity-sqlite", stringPtr("   "))),
			hasHistory: true,
			terminalID: "term_agy_done_empty_txt",
			eventType:  eventTypePtr(observability.AgentEventDone),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending)
	})

	t.Run("agy (non-pi): done and idle with non-empty lastAssistantMessage are delivered", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		doneWithMessage := harness.appendEvent(t, orchestratorAppendInput{
			agent: stringPtr("agy"),
			compactHistory: compactHistoryPtr(withAssistantMessage(
				"antigravity-sqlite", stringPtr("Final result"))),
			terminalID: "term_agy_done_msg",
			eventType:  eventTypePtr(observability.AgentEventDone),
		})
		idleWithMessage := harness.appendEvent(t, orchestratorAppendInput{
			agent: stringPtr("agy"),
			compactHistory: compactHistoryPtr(withAssistantMessage(
				"antigravity-sqlite", stringPtr("Intermediate result"))),
			terminalID: "term_agy_idle_msg",
			from:       stringPtr("working"),
			eventType:  eventTypePtr(observability.AgentEventIdle),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, doneWithMessage.ID, idleWithMessage.ID)
	})

	t.Run("pi agent: empty done is not filtered by non-pi empty gate and is delivered", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		piDoneNull := harness.appendEvent(t, orchestratorAppendInput{
			agent:          stringPtr("pi"),
			compactHistory: compactHistoryPtr(withAssistantMessage("pi-jsonl", nil)),
			hasHistory:     true,
			terminalID:     "term_pi_done_null",
			eventType:      eventTypePtr(observability.AgentEventDone),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, piDoneNull.ID)
	})

	t.Run("non-pi agent: blocked with empty lastAssistantMessage is delivered (exempt from empty delivery gate)", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		blockedNull := harness.appendEvent(t, orchestratorAppendInput{
			agent:          stringPtr("agy"),
			compactHistory: compactHistoryPtr(withAssistantMessage("antigravity-sqlite", nil)),
			hasHistory:     true,
			terminalID:     "term_agy_blocked_null",
			eventType:      eventTypePtr(observability.AgentEventBlocked),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, blockedNull.ID)
	})

	t.Run("agent.failed with empty compactHistory is delivered (failure alert skips empty-body gate)", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		failedEmpty := harness.appendEvent(t, orchestratorAppendInput{
			agent:          stringPtr("agy"),
			compactHistory: nil,
			hasHistory:     true,
			terminalID:     "term_agy_failed",
			eventType:      eventTypePtr(observability.AgentEventFailed),
		})
		failedMissing := harness.appendEvent(t, orchestratorAppendInput{
			agent: stringPtr("claude"),
			compactHistory: compactHistoryPtr(withAssistantMessage(
				"pi-jsonl", nil)),
			hasHistory: true,
			terminalID: "term_claude_failed",
			eventType:  eventTypePtr(observability.AgentEventFailed),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, failedEmpty.ID, failedMissing.ID)
	})

	t.Run("pi agent: delivery semantics remain unchanged for dispatched roles and interactive observers", func(t *testing.T) {
		harness := openOrchestratorHarness(t)
		if _, err := harness.service.Claim(observability.OrchestratorClaimInput{
			AgentScope: orchestratorScope,
			PaneID:     "wB:p-owner",
			TerminalID: "term_owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		// Dispatched role pi agent can deliver agent.idle (from working).
		dispatchedPi := harness.appendEvent(t, orchestratorAppendInput{
			agent:       stringPtr("pi"),
			sessionPath: stringPtr("/tmp/herdr-role-sessions/role-scout-123/session.jsonl"),
			terminalID:  "term_scout",
			from:        stringPtr("working"),
			eventType:   eventTypePtr(observability.AgentEventIdle),
		})
		// Interactive pi agent observer cannot deliver agent.idle.
		harness.appendEvent(t, orchestratorAppendInput{
			agent:       stringPtr("pi"),
			sessionPath: stringPtr("/root/.pi/agent/sessions/interactive.jsonl"),
			terminalID:  "term_interactive",
			from:        stringPtr("working"),
			eventType:   eventTypePtr(observability.AgentEventIdle),
		})

		pending, err := harness.service.Pending(observability.OrchestratorPendingInput{
			AgentScope: orchestratorScope,
			TerminalID: "term_owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requirePendingIDs(t, pending, dispatchedPi.ID)
	})
}

func compactHistoryPtr(history observability.CompactAgentHistory) *observability.CompactAgentHistory {
	return &history
}

func eventTypePtr(eventType observability.AgentEventType) *observability.AgentEventType {
	return &eventType
}

// eventPaneID mirrors `event.paneId ?? "wB:p_worker"`.
func eventPaneID(event observability.AgentEventRecord) string {
	if event.PaneID != nil {
		return *event.PaneID
	}
	return "wB:p_worker"
}
