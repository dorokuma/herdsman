package observability_test

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The port of test/integration/agent-event-delivery-lifecycle.test.ts.
//
// Every case runs against a migrated SQLite database inside t.TempDir(), so the
// production home and the live daemon are never touched. The one case the port
// cannot carry over is the herdsman-pi plugin reap test: the Pi extension lives
// in packages/herdsman-pi and has no Go port yet, so it is not part of this
// package.

// deliveryHarness mirrors prepareHarness: a migrated database with the running
// "default" Herdr session and one indexed agent on pane wA:p1.
type deliveryHarness struct {
	sqlite      *sql.DB
	dbPath      string
	agentEvents *db.AgentEventStore
	agents      *db.AgentStore
	scopes      *db.AgentOrchestratorScopeStore
}

func prepareDeliveryHarness(t *testing.T) *deliveryHarness {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	sqlite, err := db.OpenSqlite(dbPath)
	if err != nil {
		t.Fatalf("openSqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	if err := db.ApplyMigrations(sqlite); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	sessions := db.NewHerdrSessionStore(sqlite)
	if _, err := sessions.UpsertRunning(db.UpsertRunningHerdrSessionInput{
		Name:       "default",
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
	if _, err := agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents: []db.HerdrAgentLike{{
			"agent":        "codex",
			"agent_status": "working",
			"pane_id":      "wA:p1",
			"terminal_id":  "term-agent",
			"workspace_id": "wA",
		}},
		HerdrSessionName: "default",
	}); err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	return &deliveryHarness{
		sqlite:      sqlite,
		dbPath:      dbPath,
		agentEvents: agentEvents,
		agents:      agents,
		scopes:      db.NewAgentOrchestratorScopeStore(sqlite),
	}
}

// appendDeliveryEvent mirrors the TS appendEvent: one agent.done row on the
// indexed agent's pane, for the given terminal.
func (h *deliveryHarness) appendEvent(
	t *testing.T,
	terminalID string,
) observability.AgentEventRecord {
	t.Helper()
	listed, err := h.agents.ListForHerdrSession("default")
	if err != nil {
		t.Fatalf("listForHerdrSession: %v", err)
	}
	if len(listed) == 0 {
		t.Fatalf("expected indexed agent")
	}
	agent := listed[0]
	history := observability.CompactAgentHistory{
		Source: stringPtr("antigravity-sqlite"),
		LastAssistantMessage: &observability.AgentHistoryExcerpt{
			Ref:  "ref-1",
			Text: "done text",
		},
	}
	event, err := h.agentEvents.Append(db.AppendAgentEventInput{
		AgentID:          &agent.ID,
		CompactHistory:   &history,
		HerdrSessionName: "default",
		PaneID:           &agent.PaneID,
		Payload:          map[string]any{"ok": true},
		TerminalID:       &terminalID,
		Type:             observability.AgentEventDone,
		WorkspaceID:      &agent.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return event
}

// setEventColumns mirrors the raw `update agent_events set ... where id = ?`
// statements of the TS tests.
func (h *deliveryHarness) setEventColumns(
	t *testing.T,
	id int64,
	assignments string,
	args ...any,
) {
	t.Helper()
	if _, err := h.sqlite.Exec(
		"update agent_events set "+assignments+" where id = ?",
		append(args, id)...,
	); err != nil {
		t.Fatalf("update agent_events: %v", err)
	}
}

func (h *deliveryHarness) countAgents(t *testing.T) int {
	t.Helper()
	var count int
	if err := h.sqlite.QueryRow("select count(*) from agents").Scan(&count); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	return count
}

// captureStderr swaps os.Stderr for a pipe while fn runs and returns what fn
// wrote, the way the TS tests spy on console.error/console.warn.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	original := os.Stderr
	os.Stderr = writer
	done := make(chan string, 1)
	go func() {
		captured, _ := io.ReadAll(reader)
		done <- string(captured)
	}()
	defer func() {
		os.Stderr = original
		_ = writer.Close()
		_ = reader.Close()
	}()
	fn()
	_ = writer.Close()
	os.Stderr = original
	return <-done
}

func requireStatus(
	t *testing.T,
	event observability.AgentEventRecord,
	want observability.AgentEventStatus,
) {
	t.Helper()
	if event.Status == nil || *event.Status != want {
		t.Fatalf("event %d status = %v, want %s", event.ID, event.Status, want)
	}
}

func requireDeliverable(
	t *testing.T,
	event observability.AgentEventRecord,
	want int,
) {
	t.Helper()
	if event.Deliverable != want {
		t.Fatalf("event %d deliverable = %d, want %d", event.ID, event.Deliverable, want)
	}
}

func requireReason(
	t *testing.T,
	event observability.AgentEventRecord,
	want string,
) {
	t.Helper()
	if event.InvalidatedReason == nil || *event.InvalidatedReason != want {
		t.Fatalf("event %d invalidatedReason = %v, want %s",
			event.ID, event.InvalidatedReason, want)
	}
}

func requireIDs(t *testing.T, events []observability.AgentEventRecord, want ...int64) {
	t.Helper()
	got := make([]int64, 0, len(events))
	for _, event := range events {
		got = append(got, event.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

// listAfterScope mirrors the TS listAfter input, with the optional fields the
// tests leave at their defaults.
func listAfterScope(
	afterEventID int64,
	herdrSessionName string,
	workspaceID string,
	ownerTerminalID string,
) db.ListAfterInput {
	return db.ListAfterInput{
		AfterEventID:     &afterEventID,
		HerdrSessionName: &herdrSessionName,
		WorkspaceID:      &workspaceID,
		OwnerTerminalID:  &ownerTerminalID,
	}
}

func TestAgentEventDeliveryLifecycle(t *testing.T) {
	t.Run("migration backfills legacy deliverable values into status and reason", func(t *testing.T) {
		sqlite, err := db.OpenSqlite(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatalf("openSqlite: %v", err)
		}
		t.Cleanup(func() { _ = sqlite.Close() })
		// Migrate to the schema just before the delivery-state migration, the way
		// the TS test prunes 0007 out of a copied drizzle folder.
		if err := db.ApplyMigrationsThrough(sqlite, "0006_grok_home"); err != nil {
			t.Fatalf("applyMigrationsThrough: %v", err)
		}
		now := time.Now().UnixMilli()
		if _, err := sqlite.Exec(
			"insert into herdr_sessions (name, running, session_dir, socket_path, updated_at)"+
				" values (?, 1, ?, ?, ?)",
			"default", "/tmp", "/tmp/sock", now,
		); err != nil {
			t.Fatalf("insert herdr_sessions: %v", err)
		}
		for _, deliverable := range []int{1, 0} {
			if _, err := sqlite.Exec(
				"insert into agent_events"+
					" (herdr_session_name, payload_json, deliverable, terminal_id, type, created_at)"+
					" values (?, ?, ?, ?, ?, ?)",
				"default", "{}", deliverable, "term-a", "agent.done", now,
			); err != nil {
				t.Fatalf("insert legacy agent_events (deliverable=%d): %v", deliverable, err)
			}
		}
		statements, err := db.MigrationStatements("0007_moaning_guardian")
		if err != nil {
			t.Fatalf("migrationStatements: %v", err)
		}
		for _, statement := range statements {
			if _, err := sqlite.Exec(statement); err != nil {
				t.Fatalf("replay 0007 statement %q: %v", statement, err)
			}
		}

		rows, err := sqlite.Query(
			"select deliverable, status, invalidated_reason from agent_events order by id",
		)
		if err != nil {
			t.Fatalf("select backfilled rows: %v", err)
		}
		type backfilled struct {
			deliverable       int
			status            string
			invalidatedReason sql.NullString
		}
		got := []backfilled{}
		for rows.Next() {
			var row backfilled
			if err := rows.Scan(&row.deliverable, &row.status, &row.invalidatedReason); err != nil {
				t.Fatalf("scan backfilled row: %v", err)
			}
			got = append(got, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate backfilled rows: %v", err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close backfilled rows: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("backfilled rows = %d, want 2", len(got))
		}
		if got[0].deliverable != 1 || got[0].status != "pending" || got[0].invalidatedReason.Valid {
			t.Fatalf("row 0 = %+v, want deliverable 1 / pending / null reason", got[0])
		}
		if got[1].deliverable != 0 || got[1].status != "invalidated" ||
			!got[1].invalidatedReason.Valid ||
			got[1].invalidatedReason.String != "LEGACY_DELIVERABLE_FALSE" {
			t.Fatalf("row 1 = %+v, want deliverable 0 / invalidated / LEGACY_DELIVERABLE_FALSE",
				got[1])
		}
	})

	t.Run("acking a cursor marks older pending rows in the scope as acked", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		low := harness.appendEvent(t, "term-agent")
		cursor := harness.appendEvent(t, "term-agent")
		high := harness.appendEvent(t, "term-agent")
		if err := harness.agentEvents.MarkAcked(cursor.ID, &observability.AgentScope{
			HerdrSessionName: "default",
			WorkspaceID:      "wA",
		}); err != nil {
			t.Fatalf("markAcked: %v", err)
		}
		for _, event := range []observability.AgentEventRecord{low, cursor} {
			stored, err := harness.agentEvents.Get(event.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			requireStatus(t, stored, observability.AgentEventStatusAcked)
		}
		stored, err := harness.agentEvents.Get(high.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		requireStatus(t, stored, observability.AgentEventStatusPending)
	})

	t.Run("two connections reserve the same pending batch without duplicate delivery", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		event := harness.appendEvent(t, "term-agent")

		first, err := harness.agentEvents.ReservePending("term-owner-a", 0, nil)
		if err != nil {
			t.Fatalf("reservePending (a): %v", err)
		}
		requireIDs(t, first, event.ID)
		second, err := harness.agentEvents.ReservePending("term-owner-b", 0, nil)
		if err != nil {
			t.Fatalf("reservePending (b): %v", err)
		}
		requireIDs(t, second)

		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		requireStatus(t, stored, observability.AgentEventStatusDelivered)
		if stored.DeliveryAttempts == nil || *stored.DeliveryAttempts != 1 {
			t.Fatalf("deliveryAttempts = %v, want 1", stored.DeliveryAttempts)
		}
	})

	t.Run("two independent SQLite connections concurrently reserve each pending event once", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		for index := 0; index < 12; index += 1 {
			harness.appendEvent(t, "term-"+string(rune('0'+index)))
		}
		// A second connection over the same database file, the way the TS test
		// opens openObservabilityDbHarnessAt(first.dbPath).
		secondHandle, err := db.OpenSqlite(harness.dbPath)
		if err != nil {
			t.Fatalf("openSqlite (second connection): %v", err)
		}
		defer func() { _ = secondHandle.Close() }()
		secondEvents, err := db.NewAgentEventStore(secondHandle)
		if err != nil {
			t.Fatalf("newAgentEventStore (second connection): %v", err)
		}

		var (
			first  []observability.AgentEventRecord
			second []observability.AgentEventRecord
			group  sync.WaitGroup
		)
		group.Add(2)
		go func() {
			defer group.Done()
			reserved, err := harness.agentEvents.ReservePending("owner-a", 100, nil)
			if err != nil {
				t.Errorf("reservePending (owner-a): %v", err)
				return
			}
			first = reserved
		}()
		go func() {
			defer group.Done()
			reserved, err := secondEvents.ReservePending("owner-b", 100, nil)
			if err != nil {
				t.Errorf("reservePending (owner-b): %v", err)
				return
			}
			second = reserved
		}()
		group.Wait()

		seen := map[int64]struct{}{}
		for _, event := range append(append([]observability.AgentEventRecord{}, first...), second...) {
			seen[event.ID] = struct{}{}
		}
		if len(seen) != 12 {
			t.Fatalf("distinct reserved events = %d, want 12", len(seen))
		}
		if len(first)+len(second) != 12 {
			t.Fatalf("reserved total = %d, want 12", len(first)+len(second))
		}
		var (
			count    int
			attempts int
		)
		if err := harness.sqlite.QueryRow(
			"select count(*), max(delivery_attempts) from agent_events where status = 'delivered'",
		).Scan(&count, &attempts); err != nil {
			t.Fatalf("count delivered rows: %v", err)
		}
		if count != 12 || attempts != 1 {
			t.Fatalf("delivered rows = %d with max attempts %d, want 12 with 1", count, attempts)
		}
	})

	t.Run("listAfter excludes delivered rows before the cursor and filters scope", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		delivered := harness.appendEvent(t, "term-agent")
		pending := harness.appendEvent(t, "term-agent-2")
		harness.setEventColumns(t, delivered.ID,
			"status = 'delivered', delivered_to_terminal_id = ?", "term-owner")

		listed, err := harness.agentEvents.ListAfter(listAfterScope(
			delivered.ID, "default", "wA", "term-owner"))
		if err != nil {
			t.Fatalf("listAfter: %v", err)
		}
		requireIDs(t, listed, pending.ID)

		listed, err = harness.agentEvents.ListAfter(listAfterScope(
			0, "other", "wA", "term-owner"))
		if err != nil {
			t.Fatalf("listAfter (other session): %v", err)
		}
		requireIDs(t, listed)
		listed, err = harness.agentEvents.ListAfter(listAfterScope(
			0, "default", "other", "term-owner"))
		if err != nil {
			t.Fatalf("listAfter (other workspace): %v", err)
		}
		requireIDs(t, listed)
	})

	t.Run("repeated reserve for the same delivered event is idempotent", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		event := harness.appendEvent(t, "term-agent")
		first, err := harness.agentEvents.ReservePending("term-owner", 0, nil)
		if err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		if len(first) != 1 {
			t.Fatalf("reservePending = %d rows, want 1", len(first))
		}
		before, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		second, err := harness.agentEvents.ReservePending("term-owner", 0, nil)
		if err != nil {
			t.Fatalf("reservePending (repeated): %v", err)
		}
		if len(second) != 1 {
			t.Fatalf("reservePending (repeated) = %d rows, want 1", len(second))
		}
		after, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get (repeated): %v", err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("reserved row changed: %+v, want %+v", after, before)
		}
	})

	t.Run("does not re-list a delivered event after the redelivery freshness window", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		event := harness.appendEvent(t, "term-agent")
		if _, err := harness.agentEvents.ReservePending("term-owner", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		// Age the delivery beyond the redelivery freshness window.
		harness.setEventColumns(t, event.ID, "last_attempt_at = ?",
			time.Now().UnixMilli()-db.RedeliveryFreshnessMs-1)

		// The same terminal is no longer re-served the stale delivered event.
		reserved, err := harness.agentEvents.ReservePending("term-owner", 0, nil)
		if err != nil {
			t.Fatalf("reservePending (stale): %v", err)
		}
		requireIDs(t, reserved)
		listed, err := harness.agentEvents.ListAfter(listAfterScope(
			0, "default", "wA", "term-owner"))
		if err != nil {
			t.Fatalf("listAfter (stale): %v", err)
		}
		requireIDs(t, listed)

		// A crash-redelivery path still works: an event reset to pending is
		// reserved again (the freshness window only bounds the delivered re-list).
		harness.setEventColumns(t, event.ID,
			"status = 'pending', deliverable = 1, delivered_to_terminal_id = null,"+
				" last_attempt_at = null")
		reserved, err = harness.agentEvents.ReservePending("term-owner", 0, nil)
		if err != nil {
			t.Fatalf("reservePending (reset): %v", err)
		}
		requireIDs(t, reserved, event.ID)
	})

	t.Run("reclaims timed-out deliveries and fails events at the attempt limit", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		retry := harness.appendEvent(t, "term-agent")
		exhausted := harness.appendEvent(t, "term-agent-2")
		harness.setEventColumns(t, retry.ID,
			"status = 'delivered', deliverable = 1, delivery_attempts = 1, last_attempt_at = ?",
			time.Now().UnixMilli()-100_000)
		harness.setEventColumns(t, exhausted.ID,
			"status = 'delivered', deliverable = 1, delivery_attempts = 10, last_attempt_at = ?",
			time.Now().UnixMilli()-100_000)

		reclaimed, err := harness.agentEvents.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 2 {
			t.Fatalf("reclaimDelivered = %d, want 2", reclaimed)
		}
		retryStored, err := harness.agentEvents.Get(retry.ID)
		if err != nil {
			t.Fatalf("get (retry): %v", err)
		}
		requireStatus(t, retryStored, observability.AgentEventStatusPending)
		requireDeliverable(t, retryStored, 1)
		if retryStored.DeliveryAttempts == nil || *retryStored.DeliveryAttempts != 1 {
			t.Fatalf("retry deliveryAttempts = %v, want 1", retryStored.DeliveryAttempts)
		}
		exhaustedStored, err := harness.agentEvents.Get(exhausted.ID)
		if err != nil {
			t.Fatalf("get (exhausted): %v", err)
		}
		requireStatus(t, exhaustedStored, observability.AgentEventStatusFailed)
		requireDeliverable(t, exhaustedStored, 0)
		if exhaustedStored.DeliveryAttempts == nil || *exhaustedStored.DeliveryAttempts != 10 {
			t.Fatalf("exhausted deliveryAttempts = %v, want 10",
				exhaustedStored.DeliveryAttempts)
		}
		if exhaustedStored.LastFailureCode == nil ||
			*exhaustedStored.LastFailureCode != "DELIVERY_ATTEMPTS_EXCEEDED" {
			t.Fatalf("exhausted lastFailureCode = %v, want DELIVERY_ATTEMPTS_EXCEEDED",
				exhaustedStored.LastFailureCode)
		}
		reclaimed, err = harness.agentEvents.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered (second): %v", err)
		}
		if reclaimed != 0 {
			t.Fatalf("reclaimDelivered (second) = %d, want 0", reclaimed)
		}
	})

	t.Run("W8: reclaim logs when delivery attempts exceeded; attempts below 10 do not log", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		retry := harness.appendEvent(t, "term-agent")
		exhausted := harness.appendEvent(t, "term-agent-2")
		harness.setEventColumns(t, retry.ID,
			"status = 'delivered', deliverable = 1, delivery_attempts = 1, last_attempt_at = ?",
			time.Now().UnixMilli()-100_000)
		harness.setEventColumns(t, exhausted.ID,
			"status = 'delivered', deliverable = 1, delivery_attempts = 10, last_attempt_at = ?",
			time.Now().UnixMilli()-100_000)

		var reclaimed int
		captured := captureStderr(t, func() {
			var err error
			reclaimed, err = harness.agentEvents.ReclaimDelivered(60_000)
			if err != nil {
				t.Errorf("reclaimDelivered: %v", err)
			}
		})
		if reclaimed != 2 {
			t.Fatalf("reclaimDelivered = %d, want 2", reclaimed)
		}
		if !strings.Contains(captured, "Herdsman agent event delivery attempts exceeded") {
			t.Fatalf("stderr = %q, want the attempts-exceeded log line", captured)
		}
		for _, want := range []string{
			"eventId=" + itoa(exhausted.ID),
			"herdrSessionName=default",
			"workspaceId=wA",
			"deliveryAttempts=10",
			"DELIVERY_ATTEMPTS_EXCEEDED",
		} {
			if !strings.Contains(captured, want) {
				t.Fatalf("stderr = %q, want it to contain %q", captured, want)
			}
		}
		if strings.Count(captured, "Herdsman agent event delivery attempts exceeded") != 1 {
			t.Fatalf("stderr = %q, want exactly one attempts-exceeded line", captured)
		}
		if strings.Contains(captured, "eventId="+itoa(retry.ID)+" ") {
			t.Fatalf("stderr = %q, must not log the below-limit event %d",
				captured, retry.ID)
		}
	})

	t.Run("keeps a delivered event with the owner while the pane is open and the scope is still held", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		if _, err := harness.scopes.Claim(db.ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: observability.AgentScope{
				HerdrSessionName: "default",
				WorkspaceID:      "wA",
			},
			AckedEventID: 0,
			PaneID:       "wA:owner",
			TerminalID:   "term-owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, "term-agent")
		if _, err := harness.agentEvents.ReservePending("term-owner", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		harness.setEventColumns(t, event.ID, "last_attempt_at = ?",
			time.Now().UnixMilli()-100_000)

		reclaimed, err := harness.agentEvents.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 0 {
			t.Fatalf("reclaimDelivered = %d, want 0", reclaimed)
		}
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		requireStatus(t, stored, observability.AgentEventStatusDelivered)
		requireDeliverable(t, stored, 1)
		if stored.DeliveryAttempts == nil || *stored.DeliveryAttempts != 1 {
			t.Fatalf("deliveryAttempts = %v, want 1", stored.DeliveryAttempts)
		}
		if stored.DeliveredToTerminalID == nil || *stored.DeliveredToTerminalID != "term-owner" {
			t.Fatalf("deliveredToTerminalId = %v, want term-owner", stored.DeliveredToTerminalID)
		}
		if stored.InvalidatedReason != nil {
			t.Fatalf("invalidatedReason = %v, want nil", stored.InvalidatedReason)
		}
	})

	t.Run("invalidates a delivered event whose agent pane is closed instead of redelivering it", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		event := harness.appendEvent(t, "term-agent")
		if _, err := harness.agentEvents.ReservePending("term-owner", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		harness.setEventColumns(t, event.ID, "last_attempt_at = ?",
			time.Now().UnixMilli()-100_000)
		// Simulate the pane being closed/retired: the agents row disappears.
		if _, err := harness.sqlite.Exec("delete from agents"); err != nil {
			t.Fatalf("delete agents: %v", err)
		}

		reclaimed, err := harness.agentEvents.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 1 {
			t.Fatalf("reclaimDelivered = %d, want 1", reclaimed)
		}
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		requireStatus(t, stored, observability.AgentEventStatusInvalidated)
		requireDeliverable(t, stored, 0)
		requireReason(t, stored, "PANE_CLOSED_RECLAIM")
		reserved, err := harness.agentEvents.ReservePending("term-owner", 0, nil)
		if err != nil {
			t.Fatalf("reservePending (after reclaim): %v", err)
		}
		requireIDs(t, reserved)
		reclaimed, err = harness.agentEvents.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered (second): %v", err)
		}
		if reclaimed != 0 {
			t.Fatalf("reclaimDelivered (second) = %d, want 0", reclaimed)
		}
	})

	t.Run("reclaims a delivered event when the scope moved to another terminal", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		if _, err := harness.scopes.Claim(db.ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: observability.AgentScope{
				HerdrSessionName: "default",
				WorkspaceID:      "wA",
			},
			AckedEventID: 0,
			PaneID:       "wA:owner",
			TerminalID:   "term-new-owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, "term-agent")
		if _, err := harness.agentEvents.ReservePending("term-old-owner", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		harness.setEventColumns(t, event.ID, "last_attempt_at = ?",
			time.Now().UnixMilli()-100_000)

		reclaimed, err := harness.agentEvents.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 1 {
			t.Fatalf("reclaimDelivered = %d, want 1", reclaimed)
		}
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		requireStatus(t, stored, observability.AgentEventStatusPending)
		requireDeliverable(t, stored, 1)
		if stored.DeliveredToTerminalID != nil {
			t.Fatalf("deliveredToTerminalId = %v, want nil", stored.DeliveredToTerminalID)
		}
	})

	t.Run("reclaims a delivered event immediately when the delivery terminal does not hold scope", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		event := harness.appendEvent(t, "term-agent")
		if _, err := harness.agentEvents.ReservePending("term-owner", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		// last_attempt_at is very recent: without scope ownership, the event
		// is reclaimed immediately without waiting for 60s timeout.
		harness.setEventColumns(t, event.ID, "last_attempt_at = ?", time.Now().UnixMilli())

		reclaimed, err := harness.agentEvents.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 1 {
			t.Fatalf("reclaimDelivered = %d, want 1", reclaimed)
		}
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		requireStatus(t, stored, observability.AgentEventStatusPending)
		requireDeliverable(t, stored, 1)
		if stored.DeliveryAttempts == nil || *stored.DeliveryAttempts != 1 {
			t.Fatalf("deliveryAttempts = %v, want 1", stored.DeliveryAttempts)
		}
		if stored.DeliveredToTerminalID != nil {
			t.Fatalf("deliveredToTerminalId = %v, want nil", stored.DeliveredToTerminalID)
		}
		if stored.NextAttemptAt != nil {
			t.Fatalf("nextAttemptAt = %v, want nil", stored.NextAttemptAt)
		}
	})

	t.Run("retains delivered event when scope is held and reclaims immediately upon scope release", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		if _, err := harness.scopes.Claim(db.ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: observability.AgentScope{
				HerdrSessionName: "default",
				WorkspaceID:      "wA",
			},
			AckedEventID: 0,
			PaneID:       "wA:owner",
			TerminalID:   "term-owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		event := harness.appendEvent(t, "term-agent")
		if _, err := harness.agentEvents.ReservePending("term-owner", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		// last_attempt_at is fresh: scope owner holds the lease, so it is kept.
		harness.setEventColumns(t, event.ID, "last_attempt_at = ?", time.Now().UnixMilli())

		reclaimed, err := harness.agentEvents.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 0 {
			t.Fatalf("reclaimDelivered = %d, want 0", reclaimed)
		}
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		requireStatus(t, stored, observability.AgentEventStatusDelivered)
		if stored.DeliveredToTerminalID == nil || *stored.DeliveredToTerminalID != "term-owner" {
			t.Fatalf("deliveredToTerminalId = %v, want term-owner", stored.DeliveredToTerminalID)
		}

		// When scope owner is released/cleared, immediate reclaim recovers the event.
		if _, err := harness.scopes.ReleaseIfOwner(db.ReleaseIfOwnerInput{
			AgentOrchestratorScopeKey: observability.AgentScope{
				HerdrSessionName: "default",
				WorkspaceID:      "wA",
			},
			TerminalID: "term-owner",
		}); err != nil {
			t.Fatalf("releaseIfOwner: %v", err)
		}
		reclaimed, err = harness.agentEvents.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered (after release): %v", err)
		}
		if reclaimed != 1 {
			t.Fatalf("reclaimDelivered (after release) = %d, want 1", reclaimed)
		}
		stored, err = harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get (after release): %v", err)
		}
		requireStatus(t, stored, observability.AgentEventStatusPending)
		requireDeliverable(t, stored, 1)
		if stored.DeliveredToTerminalID != nil {
			t.Fatalf("deliveredToTerminalId = %v, want nil", stored.DeliveredToTerminalID)
		}
	})

	t.Run("monotonic logical clock survives a wall-clock rollback and a store restart", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		realNow := time.Now().UnixMilli()
		// The TS test rolls the wall clock back an hour with fake timers. The Go
		// port has no clock injection, so the same state is reproduced through the
		// persisted logical clock: daemon_meta holds a value an hour ahead of the
		// wall clock, exactly what a store that already reached it persists. A
		// restarted store must seed from it instead of falling back to the wall
		// clock, and the delivered row must stay inside its freshness window.
		logicalNow := realNow + 60*60*1_000
		harness.setDaemonMeta(t, "logical_now_ms", itoa(logicalNow))
		restarted, err := db.NewAgentEventStore(harness.sqlite)
		if err != nil {
			t.Fatalf("newAgentEventStore (restart): %v", err)
		}
		harness.agentEvents = restarted

		event := harness.appendEvent(t, "term-agent")
		if _, err := harness.agentEvents.ReservePending("term-owner", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		requireStatus(t, stored, observability.AgentEventStatusDelivered)

		listed, err := restarted.ListAfter(listAfterScope(
			0, "default", "wA", "term-owner"))
		if err != nil {
			t.Fatalf("listAfter (restarted): %v", err)
		}
		requireIDs(t, listed, event.ID)

		// The restarted store stamped the row with the persisted logical clock, not
		// with the (earlier) wall clock.
		if stored.CreatedAt == "" {
			t.Fatalf("createdAt is empty")
		}
		createdAt := parseInt(t, millisOf(t, stored.CreatedAt))
		if createdAt < logicalNow {
			t.Fatalf("createdAt = %d, want >= the persisted logical now %d",
				createdAt, logicalNow)
		}
		// The logical clock is monotonic and may have ticked one more millisecond
		// between realNow and the last persist, so only a lower bound is asserted:
		// a restart must never roll the clock back below the wall-clock moment the
		// test started at.
		var persisted string
		if err := harness.sqlite.QueryRow(
			"select value from daemon_meta where key = 'logical_now_ms'",
		).Scan(&persisted); err != nil {
			t.Fatalf("read logical_now_ms: %v", err)
		}
		if value := parseInt(t, persisted); value < realNow {
			t.Fatalf("persisted logical now = %d, want >= %d", value, realNow)
		}
	})

	t.Run("keeps deliverable and status equivalent after every transition", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		event := harness.appendEvent(t, "term-agent")
		assertMirror := func() {
			t.Helper()
			var (
				status      string
				deliverable int
			)
			if err := harness.sqlite.QueryRow(
				"select status, deliverable from agent_events where id = ?", event.ID,
			).Scan(&status, &deliverable); err != nil {
				t.Fatalf("read mirror: %v", err)
			}
			want := 0
			if status == "pending" || status == "delivered" {
				want = 1
			}
			if deliverable != want {
				t.Fatalf("status %q has deliverable %d, want %d", status, deliverable, want)
			}
		}
		assertMirror()
		if _, err := harness.agentEvents.ReservePending("term-owner", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		assertMirror()
		if err := harness.agentEvents.MarkAcked(event.ID, nil); err != nil {
			t.Fatalf("markAcked: %v", err)
		}
		assertMirror()
		for _, status := range []string{"invalidated", "failed"} {
			harness.setEventColumns(t, event.ID, "status = ?, deliverable = 0", status)
			assertMirror()
		}
	})

	t.Run("terminal states are excluded from pending and nextDeliverableAfter candidates", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		events := []observability.AgentEventRecord{
			harness.appendEvent(t, "term-agent"),
			harness.appendEvent(t, "term-agent-2"),
			harness.appendEvent(t, "term-agent-3"),
			harness.appendEvent(t, "term-agent-4"),
		}
		pending, acked, invalidated, failed := events[0], events[1], events[2], events[3]
		harness.setEventColumns(t, acked.ID, "status = 'acked', deliverable = 0")
		harness.setEventColumns(t, invalidated.ID, "status = 'invalidated', deliverable = 0")
		harness.setEventColumns(t, failed.ID, "status = 'failed', deliverable = 0")

		listed, err := harness.agentEvents.ListAfter(db.ListAfterInput{
			AfterEventID:     int64Ptr(0),
			HerdrSessionName: stringPtr("default"),
			WorkspaceID:      stringPtr("wA"),
		})
		if err != nil {
			t.Fatalf("listAfter: %v", err)
		}
		requireIDs(t, listed, pending.ID)

		listed2, err := harness.agents.ListForHerdrSession("default")
		if err != nil {
			t.Fatalf("listForHerdrSession: %v", err)
		}
		if len(listed2) == 0 {
			t.Fatalf("expected indexed agent")
		}
		next, err := harness.agentEvents.NextDeliverableAfter(db.NextDeliverableAfterInput{
			AfterEventID:     0,
			GetAgent:         func(string) *observability.AgentIndexRecord { return &listed2[0] },
			HerdrSessionName: "default",
			OwnerTerminalID:  "term-owner",
			WorkspaceID:      "wA",
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter: %v", err)
		}
		if next == nil || next.ID != pending.ID {
			t.Fatalf("nextDeliverableAfter = %+v, want event %d", next, pending.ID)
		}
	})

	t.Run("nextDeliverableAfter returns a retained row after the agents record is physically removed", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		listed, err := harness.agents.ListForHerdrSession("default")
		if err != nil {
			t.Fatalf("listForHerdrSession: %v", err)
		}
		if len(listed) == 0 {
			t.Fatalf("expected indexed agent")
		}
		agent := listed[0]

		// Non-retainable process evidence on the same pane: it must stay filtered
		// out once the agents row is gone (the original existence guard).
		nonOutcome, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			AgentID:          &agent.ID,
			HerdrSessionName: "default",
			PaneID:           &agent.PaneID,
			Payload:          map[string]any{"from": "working", "to": "done"},
			TerminalID:       stringPtr("term-agent-2"),
			Type:             observability.AgentEventStatusChanged,
			WorkspaceID:      &agent.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("append (non-outcome): %v", err)
		}
		// Fresh completion on the same pane: pane close retains it as a deliverable
		// invalidated row so an idle orchestrator can still pick it up.
		retainedHistory := observability.CompactAgentHistory{
			Source: stringPtr("pi-jsonl"),
			LastAssistantMessage: &observability.AgentHistoryExcerpt{
				Ref:  "ref-1",
				Text: "done text",
			},
		}
		retained, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			AgentID:          &agent.ID,
			CompactHistory:   &retainedHistory,
			HerdrSessionName: "default",
			PaneID:           &agent.PaneID,
			Payload:          map[string]any{"from": "working"},
			TerminalID:       agent.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &agent.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("append (retained): %v", err)
		}

		// Pane close without generation + physical removal of the agents record.
		if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents:           []db.HerdrAgentLike{},
			HerdrSessionName: "default",
		}); err != nil {
			t.Fatalf("replaceForSession (empty): %v", err)
		}
		if got := harness.countAgents(t); got != 0 {
			t.Fatalf("agents rows = %d, want 0", got)
		}
		nonOutcomeStored, err := harness.agentEvents.Get(nonOutcome.ID)
		if err != nil {
			t.Fatalf("get (non-outcome): %v", err)
		}
		requireStatus(t, nonOutcomeStored, observability.AgentEventStatusInvalidated)
		requireDeliverable(t, nonOutcomeStored, 0)
		retainedStored, err := harness.agentEvents.Get(retained.ID)
		if err != nil {
			t.Fatalf("get (retained): %v", err)
		}
		requireStatus(t, retainedStored, observability.AgentEventStatusInvalidated)
		requireDeliverable(t, retainedStored, 1)
		// Pane close physically deletes the agents row and the ON DELETE SET NULL
		// foreign key then clears agent_id: this is the gate the regression covers.
		if retainedStored.AgentID != nil {
			t.Fatalf("retained agentId = %v, want nil", retainedStored.AgentID)
		}

		// Without getAgent the SQL must still let the retained row through while
		// keeping the live-agent existence guard for every other row.
		next, err := harness.agentEvents.NextDeliverableAfter(db.NextDeliverableAfterInput{
			AfterEventID:     0,
			HerdrSessionName: "default",
			OwnerTerminalID:  "term-owner",
			WorkspaceID:      "wA",
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter (no resolver): %v", err)
		}
		if next == nil || next.ID != retained.ID {
			t.Fatalf("nextDeliverableAfter = %+v, want event %d", next, retained.ID)
		}
		requireStatus(t, *next, observability.AgentEventStatusInvalidated)
		requireDeliverable(t, *next, 1)

		// Passing getAgent is the other supported shape: an already-removed agent
		// must not block the retained row either.
		next, err = harness.agentEvents.NextDeliverableAfter(db.NextDeliverableAfterInput{
			AfterEventID:     0,
			GetAgent:         func(string) *observability.AgentIndexRecord { return nil },
			HerdrSessionName: "default",
			OwnerTerminalID:  "term-owner",
			WorkspaceID:      "wA",
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter (resolver): %v", err)
		}
		if next == nil || next.ID != retained.ID {
			t.Fatalf("nextDeliverableAfter (resolver) = %+v, want event %d", next, retained.ID)
		}
	})

	t.Run("pane close retains an unattempted agent.failed outcome but neither discarded nor an attempted failure", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		listed, err := harness.agents.ListForHerdrSession("default")
		if err != nil {
			t.Fatalf("listForHerdrSession: %v", err)
		}
		if len(listed) == 0 {
			t.Fatalf("expected indexed agent")
		}
		agent := listed[0]
		appendTerminal := func(eventType observability.AgentEventType) observability.AgentEventRecord {
			t.Helper()
			event, err := harness.agentEvents.Append(db.AppendAgentEventInput{
				AgentID:          &agent.ID,
				HerdrSessionName: "default",
				PaneID:           stringPtr("wA:p1"),
				Payload:          map[string]any{"agentId": agent.ID, "reason": "RETRY_EXHAUSTED"},
				TerminalID:       agent.TerminalID,
				Type:             eventType,
				WorkspaceID:      &agent.WorkspaceID,
			})
			if err != nil {
				t.Fatalf("append (%s): %v", eventType, err)
			}
			return event
		}
		failure := appendTerminal(observability.AgentEventFailed)
		discarded := appendTerminal(observability.AgentEventDiscarded)
		attempted := appendTerminal(observability.AgentEventFailed)
		harness.setEventColumns(t, attempted.ID, "delivery_attempts = 1, last_attempt_at = ?",
			time.Now().UnixMilli())

		if err := harness.agentEvents.InvalidatePane(db.InvalidatePaneInput{
			HerdrSessionName:  "default",
			PaneID:            "wA:p1",
			InvalidatedReason: stringPtr("LEGACY_CLOSE_WITHOUT_GENERATION"),
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}

		// Only failed is retained, and only until its first delivery attempt.
		failureStored, err := harness.agentEvents.Get(failure.ID)
		if err != nil {
			t.Fatalf("get (failure): %v", err)
		}
		requireDeliverable(t, failureStored, 1)
		requireReason(t, failureStored, "RETAINED_OUTCOME_PANE_CLOSED")
		requireStatus(t, failureStored, observability.AgentEventStatusInvalidated)
		for _, event := range []observability.AgentEventRecord{discarded, attempted} {
			stored, err := harness.agentEvents.Get(event.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			requireDeliverable(t, stored, 0)
			requireReason(t, stored, "LEGACY_CLOSE_WITHOUT_GENERATION")
			requireStatus(t, stored, observability.AgentEventStatusInvalidated)
		}
	})

	t.Run("pane close with a new generation still retains unattempted failed rows of older and null generations", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		listed, err := harness.agents.ListForHerdrSession("default")
		if err != nil {
			t.Fatalf("listForHerdrSession: %v", err)
		}
		if len(listed) == 0 {
			t.Fatalf("expected indexed agent")
		}
		agent := listed[0]
		appendFailed := func(paneGeneration *string) observability.AgentEventRecord {
			t.Helper()
			event, err := harness.agentEvents.Append(db.AppendAgentEventInput{
				AgentID:          &agent.ID,
				HerdrSessionName: "default",
				PaneGeneration:   paneGeneration,
				PaneID:           stringPtr("wA:p1"),
				Payload:          map[string]any{"agentId": agent.ID, "reason": "RETRY_EXHAUSTED"},
				TerminalID:       agent.TerminalID,
				Type:             observability.AgentEventFailed,
				WorkspaceID:      &agent.WorkspaceID,
			})
			if err != nil {
				t.Fatalf("append (failed): %v", err)
			}
			return event
		}
		// The pane is reopened as a new generation: the failed outcome of the
		// previous generation (and the legacy row written without one) is still
		// unattempted and must keep its single delivery chance.
		previousGeneration := appendFailed(stringPtr("gen-1"))
		generationless := appendFailed(nil)

		if err := harness.agentEvents.InvalidatePane(db.InvalidatePaneInput{
			HerdrSessionName:  "default",
			PaneID:            "wA:p1",
			PaneGeneration:    stringPtr("gen-2"),
			InvalidatedReason: stringPtr("PANE_CLOSED"),
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}

		for _, event := range []observability.AgentEventRecord{previousGeneration, generationless} {
			stored, err := harness.agentEvents.Get(event.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			requireDeliverable(t, stored, 1)
			requireReason(t, stored, "RETAINED_OUTCOME_PANE_CLOSED")
			requireStatus(t, stored, observability.AgentEventStatusInvalidated)
		}
	})

	t.Run("a retained orphan failed row is invalidated by reclaim after its first delivery attempt", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		failure, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			HerdrSessionName: "default",
			PaneID:           stringPtr("wA:gone"),
			Payload:          map[string]any{"agentId": "agent-gone", "reason": "RETRY_EXHAUSTED"},
			TerminalID:       stringPtr("term-gone"),
			Type:             observability.AgentEventFailed,
			WorkspaceID:      stringPtr("wA"),
		})
		if err != nil {
			t.Fatalf("append (orphan failure): %v", err)
		}
		// Retained state as produced by #invalidatePaneCore / the reconciler.
		harness.setEventColumns(t, failure.ID,
			"status = 'invalidated', deliverable = 1,"+
				" invalidated_reason = 'RETAINED_OUTCOME_PANE_CLOSED'")

		// The first delivery attempt consumes the retention window.
		reserved, err := harness.agentEvents.ReservePending("term-owner", 0, nil)
		if err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		requireIDs(t, reserved, failure.ID)
		stored, err := harness.agentEvents.Get(failure.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		requireDeliverable(t, stored, 1)
		if stored.DeliveryAttempts == nil || *stored.DeliveryAttempts != 1 {
			t.Fatalf("deliveryAttempts = %v, want 1", stored.DeliveryAttempts)
		}
		requireStatus(t, stored, observability.AgentEventStatusDelivered)

		// After delivery the ordinary invalidated + 1h collection takes over.
		harness.setEventColumns(t, failure.ID, "last_attempt_at = ?",
			time.Now().UnixMilli()-100_000)
		reclaimed, err := harness.agentEvents.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 1 {
			t.Fatalf("reclaimDelivered = %d, want 1", reclaimed)
		}
		stored, err = harness.agentEvents.Get(failure.ID)
		if err != nil {
			t.Fatalf("get (after reclaim): %v", err)
		}
		requireDeliverable(t, stored, 0)
		requireReason(t, stored, "PANE_CLOSED_RECLAIM")
		requireStatus(t, stored, observability.AgentEventStatusInvalidated)
		reserved, err = harness.agentEvents.ReservePending("term-owner", 0, nil)
		if err != nil {
			t.Fatalf("reservePending (after reclaim): %v", err)
		}
		requireIDs(t, reserved)
	})

	t.Run("append downgrades a dangling agentId to an orphan row instead of failing the FK", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		var failure observability.AgentEventRecord
		captured := captureStderr(t, func() {
			var err error
			// The agents row was deleted concurrently: an FK failure here would drop
			// the terminal outcome entirely.
			failure, err = harness.agentEvents.Append(db.AppendAgentEventInput{
				AgentID:          stringPtr("agent-gone"),
				HerdrSessionName: "default",
				PaneID:           stringPtr("wA:p1"),
				Payload:          map[string]any{"agentId": "agent-gone", "reason": "RETRY_EXHAUSTED"},
				TerminalID:       stringPtr("term-agent"),
				Type:             observability.AgentEventFailed,
				WorkspaceID:      stringPtr("wA"),
			})
			if err != nil {
				t.Errorf("append: %v", err)
			}
		})
		stored, err := harness.agentEvents.Get(failure.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.AgentID != nil {
			t.Fatalf("agentId = %v, want nil", stored.AgentID)
		}
		requireDeliverable(t, stored, 1)
		requireStatus(t, stored, observability.AgentEventStatusPending)
		if !strings.Contains(captured,
			"Herdsman downgrading agent event to orphan because the agents row is gone") {
			t.Fatalf("stderr = %q, want the orphan-downgrade warning", captured)
		}
		if !strings.Contains(captured, "agentId=agent-gone") {
			t.Fatalf("stderr = %q, want it to name agent-gone", captured)
		}
	})

	t.Run("deleteInvalidatedOlderThan keeps fresh rows and ages from last_attempt_at", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		staleCreated := harness.appendEvent(t, "term-stale-created")
		staleDelivered := harness.appendEvent(t, "term-stale-delivered")
		fresh := harness.appendEvent(t, "term-fresh")
		pending := harness.appendEvent(t, "term-pending")
		const graceMs = 60 * 60 * 1000
		staleCutoff := time.Now().UnixMilli() - graceMs - 1_000
		harness.setEventColumns(t, staleCreated.ID,
			"status = 'invalidated', deliverable = 0, last_attempt_at = null, created_at = ?",
			staleCutoff)
		harness.setEventColumns(t, staleDelivered.ID,
			"status = 'invalidated', deliverable = 0, last_attempt_at = ?, created_at = ?",
			staleCutoff, time.Now().UnixMilli())
		harness.setEventColumns(t, fresh.ID, "status = 'invalidated', deliverable = 1")

		removed, err := harness.agentEvents.DeleteInvalidatedOlderThan(graceMs)
		if err != nil {
			t.Fatalf("deleteInvalidatedOlderThan: %v", err)
		}
		if removed != 2 {
			t.Fatalf("deleteInvalidatedOlderThan = %d, want 2", removed)
		}
		rows, err := harness.sqlite.Query("select id, status from agent_events order by id")
		if err != nil {
			t.Fatalf("select remaining rows: %v", err)
		}
		type remaining struct {
			id     int64
			status string
		}
		got := []remaining{}
		for rows.Next() {
			var row remaining
			if err := rows.Scan(&row.id, &row.status); err != nil {
				t.Fatalf("scan remaining row: %v", err)
			}
			got = append(got, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate remaining rows: %v", err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close remaining rows: %v", err)
		}
		if len(got) != 2 || got[0].id != fresh.ID || got[0].status != "invalidated" ||
			got[1].id != pending.ID || got[1].status != "pending" {
			t.Fatalf("remaining rows = %+v, want %d/invalidated and %d/pending",
				got, fresh.ID, pending.ID)
		}
	})

	t.Run("delayed wake survives pane.closed without generation and wakes orchestrator upon idle", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		listed, err := harness.agents.ListForHerdrSession("default")
		if err != nil {
			t.Fatalf("listForHerdrSession: %v", err)
		}
		if len(listed) == 0 {
			t.Fatalf("expected indexed agent")
		}
		agent := listed[0]
		retainedHistory := observability.CompactAgentHistory{
			Source: stringPtr("pi-jsonl"),
			LastAssistantMessage: &observability.AgentHistoryExcerpt{
				Ref:  "ref-1",
				Text: "done text",
			},
		}
		event, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			AgentID:          &agent.ID,
			CompactHistory:   &retainedHistory,
			HerdrSessionName: "default",
			PaneID:           &agent.PaneID,
			Payload:          map[string]any{"from": "working"},
			TerminalID:       agent.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &agent.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}

		// Simulate legacy close without generation.
		if err := harness.agentEvents.InvalidatePane(db.InvalidatePaneInput{
			HerdrSessionName:  "default",
			PaneID:            agent.PaneID,
			InvalidatedReason: stringPtr("LEGACY_CLOSE_WITHOUT_GENERATION"),
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}
		stored, err := harness.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		requireStatus(t, stored, observability.AgentEventStatusInvalidated)
		requireDeliverable(t, stored, 1)

		// Orchestrator idle settle should still deliver the retained event.
		ownerTerminal := "term-other"
		if agent.TerminalID != nil {
			ownerTerminal = *agent.TerminalID
		}
		delivered, err := harness.agentEvents.ListAfter(listAfterScope(
			0, "default", "wA", ownerTerminal))
		if err != nil {
			t.Fatalf("listAfter: %v", err)
		}
		found := false
		for _, row := range delivered {
			if row.ID == event.ID &&
				row.Status != nil && *row.Status == observability.AgentEventStatusInvalidated &&
				row.Deliverable == 1 {
				found = true
			}
		}
		if !found {
			t.Fatalf("listAfter = %v, want the retained event %d", delivered, event.ID)
		}
	})

	t.Run("pending applies reclaim, deliverability, and reservation to a retained row", func(t *testing.T) {
		harness := prepareDeliveryHarness(t)
		if _, err := harness.scopes.Claim(db.ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: observability.AgentScope{
				HerdrSessionName: "default",
				WorkspaceID:      "wA",
			},
			AckedEventID: 0,
			PaneID:       "wA:owner",
			TerminalID:   "term-owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		listed, err := harness.agents.ListForHerdrSession("default")
		if err != nil {
			t.Fatalf("listForHerdrSession: %v", err)
		}
		if len(listed) == 0 {
			t.Fatalf("expected indexed agent")
		}
		agent := listed[0]
		retainedHistory := observability.CompactAgentHistory{
			Source: stringPtr("pi-jsonl"),
			LastAssistantMessage: &observability.AgentHistoryExcerpt{
				Ref:  "ref-retained",
				Text: "retained result",
			},
		}
		retained, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			AgentID:          &agent.ID,
			CompactHistory:   &retainedHistory,
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen-closed"),
			PaneID:           &agent.PaneID,
			Payload:          map[string]any{"agent": "codex", "from": "working"},
			TerminalID:       agent.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &agent.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("append (retained): %v", err)
		}
		reopened, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
			Agents: []db.HerdrAgentLike{{
				"agent":           "codex",
				"agent_status":    "working",
				"pane_generation": "gen-live",
				"pane_id":         agent.PaneID,
				"terminal_id":     "term-agent-live",
				"workspace_id":    "wA",
			}},
			HerdrSessionName: "default",
		})
		if err != nil {
			t.Fatalf("replaceForSession (reopened): %v", err)
		}
		if len(reopened) == 0 {
			t.Fatalf("expected reopened agent")
		}
		liveAgent := reopened[0]
		laterHistory := observability.CompactAgentHistory{
			Source: stringPtr("pi-jsonl"),
			LastAssistantMessage: &observability.AgentHistoryExcerpt{
				Ref:  "ref-later",
				Text: "later result",
			},
		}
		backingOff, err := harness.agentEvents.Append(db.AppendAgentEventInput{
			AgentID:          &liveAgent.ID,
			CompactHistory:   &laterHistory,
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen-live"),
			PaneID:           &liveAgent.PaneID,
			Payload:          map[string]any{"agent": "codex", "from": "working"},
			TerminalID:       liveAgent.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &liveAgent.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("append (backing off): %v", err)
		}
		harness.setEventColumns(t, retained.ID,
			"status = 'invalidated', deliverable = 1,"+
				" invalidated_reason = 'RETAINED_OUTCOME_PANE_CLOSED'")
		harness.setEventColumns(t, retained.ID,
			"status = 'delivered', deliverable = 1, delivery_attempts = 1,"+
				" delivered_to_terminal_id = 'term-owner', last_attempt_at = ?",
			time.Now().UnixMilli()-100_000)
		harness.setEventColumns(t, backingOff.ID, "next_attempt_at = ?",
			time.Now().UnixMilli()+60_000)

		service := observability.NewAgentOrchestratorService(struct {
			AgentEvents observability.AgentEventStoreLike
			Agents      observability.AgentStoreLike
			Scopes      observability.AgentOrchestratorScopeStoreLike
		}{
			AgentEvents: harness.agentEvents,
			Agents:      harness.agents,
			Scopes:      harness.scopes,
		})
		delivered, err := service.Pending(observability.OrchestratorPendingInput{
			AgentScope: observability.AgentScope{
				HerdrSessionName: "default",
				WorkspaceID:      "wA",
			},
			TerminalID: "term-owner",
		})
		if err != nil {
			t.Fatalf("pending: %v", err)
		}
		requireIDs(t, delivered)

		retainedStored, err := harness.agentEvents.Get(retained.ID)
		if err != nil {
			t.Fatalf("get (retained): %v", err)
		}
		requireDeliverable(t, retainedStored, 0)
		requireReason(t, retainedStored, "PANE_CLOSED_RECLAIM")
		requireStatus(t, retainedStored, observability.AgentEventStatusInvalidated)
		backingOffStored, err := harness.agentEvents.Get(backingOff.ID)
		if err != nil {
			t.Fatalf("get (backing off): %v", err)
		}
		requireDeliverable(t, backingOffStored, 1)
		requireStatus(t, backingOffStored, observability.AgentEventStatusPending)
	})
}

func TestEmptyDatabaseMigrationChain(t *testing.T) {
	t.Run("applies every migration and matches schema columns", func(t *testing.T) {
		sqlite, err := db.OpenSqlite(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatalf("openSqlite: %v", err)
		}
		t.Cleanup(func() { _ = sqlite.Close() })
		if err := db.ApplyMigrations(sqlite); err != nil {
			t.Fatalf("applyMigrations: %v", err)
		}
		columns, err := sqlite.Query("pragma table_info(agent_events)")
		if err != nil {
			t.Fatalf("pragma table_info: %v", err)
		}
		names := []string{}
		for columns.Next() {
			var (
				cid          int
				name         string
				columnType   string
				notNull      int
				defaultValue sql.NullString
				primaryKey   int
			)
			if err := columns.Scan(
				&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey,
			); err != nil {
				t.Fatalf("scan table_info: %v", err)
			}
			names = append(names, name)
		}
		if err := columns.Err(); err != nil {
			t.Fatalf("iterate table_info: %v", err)
		}
		if err := columns.Close(); err != nil {
			t.Fatalf("close table_info: %v", err)
		}
		for _, want := range []string{
			"deliverable",
			"status",
			"delivery_attempts",
			"last_attempt_at",
			"next_attempt_at",
			"last_failure_code",
			"invalidated_reason",
			"delivered_to_terminal_id",
		} {
			if !containsString(names, want) {
				t.Fatalf("agent_events columns = %v, want it to contain %q", names, want)
			}
		}
		var applied int
		if err := sqlite.QueryRow(
			"select count(*) from __drizzle_migrations",
		).Scan(&applied); err != nil {
			t.Fatalf("count migrations: %v", err)
		}
		journal, err := db.MigrationJournal()
		if err != nil {
			t.Fatalf("migrationJournal: %v", err)
		}
		if applied != len(journal) {
			t.Fatalf("applied migrations = %d, want %d", applied, len(journal))
		}
		// The journal's drizzle versions decide the SQL flavour of each entry,
		// and the TS test pins them: entries 0-5, 8, 9 and everything from 10 on
		// are version 6, entries 6 and 7 are version 7.
		for index, entry := range journal {
			want := "7"
			if index < 6 || index == 8 || index == 9 || index >= 10 {
				want = "6"
			}
			if entry.Version != want {
				t.Fatalf("journal entry %d (%s) version = %q, want %q",
					index, entry.Tag, entry.Version, want)
			}
		}
		for _, tag := range []string{
			"0007_moaning_guardian",
			"0008_pane_tombstones",
			"0009_high_lake",
		} {
			entry, ok := journalEntry(journal, tag)
			if !ok {
				t.Fatalf("journal has no entry for %s", tag)
			}
			want := "6"
			if tag == "0007_moaning_guardian" {
				want = "7"
			}
			if entry.Version != want {
				t.Fatalf("%s version = %q, want %q", tag, entry.Version, want)
			}
		}
	})
}

func journalEntry(entries []db.MigrationJournalEntry, tag string) (db.MigrationJournalEntry, bool) {
	for _, entry := range entries {
		if entry.Tag == tag {
			return entry, true
		}
	}
	return db.MigrationJournalEntry{}, false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func int64Ptr(value int64) *int64 { return &value }

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

// setDaemonMeta mirrors writing the daemon_meta row the logical clock persists.
func (h *deliveryHarness) setDaemonMeta(t *testing.T, key string, value string) {
	t.Helper()
	if _, err := h.sqlite.Exec(
		"insert into daemon_meta (key, value) values (?, ?)"+
			" on conflict(key) do update set value = excluded.value",
		key, value,
	); err != nil {
		t.Fatalf("write daemon_meta: %v", err)
	}
}

func parseInt(t *testing.T, value string) int64 {
	t.Helper()
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// millisOf renders an ISO timestamp the record fields carry back into epoch
// milliseconds, the way the TS tests compare Date values.
func millisOf(t *testing.T, value string) string {
	t.Helper()
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", value)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", value, err)
	}
	return itoa(parsed.UnixMilli())
}
