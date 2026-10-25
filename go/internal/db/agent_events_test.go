package db

import (
	"database/sql"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The port of the AgentEventStore behaviour of test/integration/agent-events*.ts
// and the shared delivery predicate. The orchestrator-facing wrappers around
// these calls belong to the daemon packages and are not part of this port.

func int64Ptr(value int64) *int64 { return &value }

// openAgentEventStore mirrors the TS observability harness: a migrated database
// with the two Herdr sessions the agent_events foreign keys need.
func openAgentEventStore(t *testing.T) (*AgentEventStore, *sql.DB) {
	t.Helper()
	db := openMigratedTestDB(t)
	sessions := NewHerdrSessionStore(db)
	for _, name := range []string{"default", "other"} {
		if _, err := sessions.UpsertRunning(UpsertRunningHerdrSessionInput{
			Name:       name,
			SessionDir: "/tmp/" + name,
			SocketPath: "/tmp/" + name + ".sock",
		}); err != nil {
			t.Fatalf("upsertRunning(%s): %v", name, err)
		}
	}
	store, err := NewAgentEventStore(db)
	if err != nil {
		t.Fatalf("newAgentEventStore: %v", err)
	}
	return store, db
}

// seedAgentInput is the shape of one agents row the agent_events foreign key
// needs. The zero fields fall back to the values the fixtures use.
type seedAgentInput struct {
	HerdrSessionName string
	ID               string
	Kind             string
	PaneGeneration   *string
	PaneID           string
	TerminalID       string
	WorkspaceID      string
}

func seedAgent(t *testing.T, db *sql.DB, input seedAgentInput) {
	t.Helper()
	if input.HerdrSessionName == "" {
		input.HerdrSessionName = "default"
	}
	if input.WorkspaceID == "" {
		input.WorkspaceID = "wB"
	}
	if input.Kind == "" {
		input.Kind = "pi"
	}
	now := time.Now().UnixMilli()
	if _, err := db.Exec(
		`insert into agents
		   (agent, agent_status, cwd, first_seen_at, focused, foreground_cwd,
		    herdr_session_name, id, last_seen_at, pane_id, tab_id, terminal_id, workspace_id, pane_generation)
		 values (?, 'working', '/tmp/wB', ?, 0, '/tmp/wB', ?, ?, ?, ?, 'tab_1', ?, ?, ?)`,
		input.Kind, now, input.HerdrSessionName, input.ID, now,
		input.PaneID, input.TerminalID, input.WorkspaceID, input.PaneGeneration,
	); err != nil {
		t.Fatalf("seed agent %s: %v", input.ID, err)
	}
}

func appendEvent(
	t *testing.T,
	store *AgentEventStore,
	input AppendAgentEventInput,
) observability.AgentEventRecord {
	t.Helper()
	if input.HerdrSessionName == "" {
		input.HerdrSessionName = "default"
	}
	if input.PaneID == nil {
		input.PaneID = stringPtr("wB:p1")
	}
	if input.TerminalID == nil {
		input.TerminalID = stringPtr("term_1")
	}
	if input.WorkspaceID == nil {
		input.WorkspaceID = stringPtr("wB")
	}
	record, err := store.Append(input)
	if err != nil {
		t.Fatalf("append %s: %v", input.Type, err)
	}
	return record
}

// compactHistoryWithAssistant builds the compact history a non-pi terminal event
// needs to pass the assistant-message guard.
func compactHistoryWithAssistant(text string) *observability.CompactAgentHistory {
	return &observability.CompactAgentHistory{
		LastAssistantMessage: &observability.AgentHistoryExcerpt{Ref: "ref", Text: text},
		MessageCount:         1,
	}
}

func TestAgentEventStoreAppend(t *testing.T) {
	t.Run("stores a pending event and round-trips every column", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		compactHistory := compactHistoryWithAssistant("done")

		record := appendEvent(t, store, AppendAgentEventInput{
			AgentID:          stringPtr("agent_1"),
			CompactHistory:   compactHistory,
			HerdrSessionName: "default",
			IdempotencyKey:   stringPtr("idem-1"),
			PaneGeneration:   stringPtr("gen_1"),
			Payload:          map[string]any{"from": "working"},
			TerminalID:       stringPtr("term_1"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      stringPtr("wB"),
		})

		if record.ID == 0 {
			t.Fatalf("append did not assign an id")
		}
		if record.AgentID == nil || *record.AgentID != "agent_1" {
			t.Fatalf("agentId = %v, want agent_1", record.AgentID)
		}
		if record.HerdrSessionName != "default" {
			t.Fatalf("herdrSessionName = %q, want default", record.HerdrSessionName)
		}
		if record.PaneID == nil || *record.PaneID != "wB:p1" {
			t.Fatalf("paneId = %v, want wB:p1", record.PaneID)
		}
		if record.PaneGeneration == nil || *record.PaneGeneration != "gen_1" {
			t.Fatalf("paneGeneration = %v, want gen_1", record.PaneGeneration)
		}
		if record.WorkspaceID == nil || *record.WorkspaceID != "wB" {
			t.Fatalf("workspaceId = %v, want wB", record.WorkspaceID)
		}
		if record.TerminalID == nil || *record.TerminalID != "term_1" {
			t.Fatalf("terminalId = %v, want term_1", record.TerminalID)
		}
		if record.Type != observability.AgentEventDone {
			t.Fatalf("type = %q, want agent.done", record.Type)
		}
		if !reflect.DeepEqual(record.Payload, map[string]any{"from": "working"}) {
			t.Fatalf("payload = %#v, want {from:working}", record.Payload)
		}
		if !reflect.DeepEqual(record.CompactHistory, compactHistory) {
			t.Fatalf("compactHistory = %#v, want %#v", record.CompactHistory, compactHistory)
		}
		if record.Status == nil || *record.Status != observability.AgentEventStatusPending {
			t.Fatalf("status = %v, want pending", record.Status)
		}
		if record.Deliverable != 1 {
			t.Fatalf("deliverable = %d, want 1", record.Deliverable)
		}
		if record.DeliveryAttempts == nil || *record.DeliveryAttempts != 0 {
			t.Fatalf("deliveryAttempts = %v, want 0", record.DeliveryAttempts)
		}
		if record.LastAttemptAt != nil || record.NextAttemptAt != nil {
			t.Fatalf("attempt timestamps = %v / %v, want nil / nil",
				record.LastAttemptAt, record.NextAttemptAt)
		}
		if record.LastFailureCode != nil || record.InvalidatedReason != nil ||
			record.DeliveredToTerminalID != nil {
			t.Fatalf("delivery bookkeeping = %v / %v / %v, want nil",
				record.LastFailureCode, record.InvalidatedReason, record.DeliveredToTerminalID)
		}
		if _, err := time.Parse(isoTimestampLayout, record.CreatedAt); err != nil {
			t.Fatalf("createdAt %q is not an ISO timestamp: %v", record.CreatedAt, err)
		}

		fetched, err := store.Get(record.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", record.ID, err)
		}
		if !reflect.DeepEqual(fetched, record) {
			t.Fatalf("get(%d) = %#v, want %#v", record.ID, fetched, record)
		}
	})

	t.Run("returns the existing row for a repeated idempotency key", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		first := appendEvent(t, store, AppendAgentEventInput{
			IdempotencyKey: stringPtr("idem-1"),
			Payload:        map[string]any{"from": "working"},
			Type:           observability.AgentEventDone,
		})
		second := appendEvent(t, store, AppendAgentEventInput{
			IdempotencyKey: stringPtr("idem-1"),
			Payload:        map[string]any{"from": "idle"},
			Type:           observability.AgentEventIdle,
		})
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("duplicate append returned %#v, want %#v", second, first)
		}
		count, err := countAgentEvents(t, store.db, "default")
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 1 {
			t.Fatalf("row count = %d, want 1", count)
		}
	})

	t.Run("scopes the idempotency key by Herdr session", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		first := appendEvent(t, store, AppendAgentEventInput{
			HerdrSessionName: "default",
			IdempotencyKey:   stringPtr("idem-1"),
			Payload:          map[string]any{},
			Type:             observability.AgentEventDone,
		})
		second := appendEvent(t, store, AppendAgentEventInput{
			HerdrSessionName: "other",
			IdempotencyKey:   stringPtr("idem-1"),
			Payload:          map[string]any{},
			Type:             observability.AgentEventDone,
		})
		if first.ID == second.ID {
			t.Fatalf("the same idempotency key in two sessions collided on id %d", first.ID)
		}
	})

	t.Run("keeps a null compact history absent instead of an empty object", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		record := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventBlocked,
		})
		if record.CompactHistory != nil {
			t.Fatalf("compactHistory = %#v, want nil", record.CompactHistory)
		}
	})

	t.Run("downgrades to an orphan when the agents row is gone", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		record := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_gone"),
			Payload: map[string]any{},
			Type:    observability.AgentEventFailed,
		})
		if record.AgentID != nil {
			t.Fatalf("agentId = %v, want nil for the orphan row", record.AgentID)
		}
		var stored sql.NullString
		if err := db.QueryRow(
			"select agent_id from agent_events where id = ?", record.ID,
		).Scan(&stored); err != nil {
			t.Fatalf("read back the stored agent_id: %v", err)
		}
		if stored.Valid {
			t.Fatalf("stored agent_id = %q, want NULL", stored.String)
		}
	})

	t.Run("keeps the agent id when the agents row exists", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		record := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"),
			Payload: map[string]any{},
			Type:    observability.AgentEventDone,
		})
		if record.AgentID == nil || *record.AgentID != "agent_1" {
			t.Fatalf("agentId = %v, want agent_1", record.AgentID)
		}
	})

	t.Run("survives concurrent appends with distinct ids", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		const writers = 4
		const perWriter = 5
		var wg sync.WaitGroup
		ids := make([][]int64, writers)
		for worker := 0; worker < writers; worker += 1 {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for index := 0; index < perWriter; index += 1 {
					record := appendEvent(t, store, AppendAgentEventInput{
						Payload: map[string]any{"worker": worker, "index": index},
						Type:    observability.AgentEventBlocked,
					})
					ids[worker] = append(ids[worker], record.ID)
				}
			}(worker)
		}
		wg.Wait()

		seen := map[int64]bool{}
		createdAts := []string{}
		for _, workerIDs := range ids {
			if len(workerIDs) != perWriter {
				t.Fatalf("worker wrote %d rows, want %d", len(workerIDs), perWriter)
			}
			for _, id := range workerIDs {
				if seen[id] {
					t.Fatalf("duplicate event id %d across concurrent appends", id)
				}
				seen[id] = true
				record, err := store.Get(id)
				if err != nil {
					t.Fatalf("get(%d): %v", id, err)
				}
				createdAts = append(createdAts, record.CreatedAt)
			}
		}
		if len(seen) != writers*perWriter {
			t.Fatalf("distinct ids = %d, want %d", len(seen), writers*perWriter)
		}
	})
}

func countAgentEvents(t *testing.T, db *sql.DB, herdrSessionName string) (int, error) {
	t.Helper()
	var count int
	err := db.QueryRow(
		"select count(*) from agent_events where herdr_session_name = ?", herdrSessionName,
	).Scan(&count)
	return count, err
}

func TestAgentEventStoreLogicalClock(t *testing.T) {
	t.Run("seeds from daemon_meta and never moves backwards", func(t *testing.T) {
		_, db := openAgentEventStore(t)
		future := time.Now().UnixMilli() + 600_000
		if _, err := db.Exec(
			`insert into daemon_meta (key, value) values (?, ?)
			 on conflict(key) do update set value = excluded.value`,
			LogicalClockMetaKey, itoa(future),
		); err != nil {
			t.Fatalf("seed the logical clock: %v", err)
		}
		restarted, err := NewAgentEventStore(db)
		if err != nil {
			t.Fatalf("newAgentEventStore after restart: %v", err)
		}

		record := appendEvent(t, restarted, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventBlocked,
		})
		want := formatTimestamp(future)
		if record.CreatedAt != want {
			t.Fatalf("createdAt = %q, want %q (the clock must not regress)", record.CreatedAt, want)
		}
	})

	t.Run("persists the clock only after the throttle interval", func(t *testing.T) {
		_, db := openAgentEventStore(t)
		seed := time.Now().UnixMilli() - 30_000
		if _, err := db.Exec(
			`insert into daemon_meta (key, value) values (?, ?)
			 on conflict(key) do update set value = excluded.value`,
			LogicalClockMetaKey, itoa(seed),
		); err != nil {
			t.Fatalf("seed the logical clock: %v", err)
		}
		restarted, err := NewAgentEventStore(db)
		if err != nil {
			t.Fatalf("newAgentEventStore: %v", err)
		}
		if restarted.lastPersistedLogicalNow != seed {
			t.Fatalf("lastPersistedLogicalNow = %d, want %d",
				restarted.lastPersistedLogicalNow, seed)
		}

		appendEvent(t, restarted, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventBlocked,
		})
		persisted, err := readDaemonMeta(t, db)
		if err != nil {
			t.Fatalf("read daemon_meta: %v", err)
		}
		if persisted != itoa(seed) {
			t.Fatalf("daemon_meta = %q, want %q (30s of drift must not persist)", persisted, itoa(seed))
		}

		large := time.Now().UnixMilli() + LogicalClockPersistIntervalMs + 60_000
		if _, err := db.Exec(
			`insert into daemon_meta (key, value) values (?, ?)
			 on conflict(key) do update set value = excluded.value`,
			LogicalClockMetaKey, itoa(large),
		); err != nil {
			t.Fatalf("re-seed the logical clock: %v", err)
		}
		restarted2, err := NewAgentEventStore(db)
		if err != nil {
			t.Fatalf("newAgentEventStore: %v", err)
		}
		appendEvent(t, restarted2, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventBlocked,
		})
		persisted, err = readDaemonMeta(t, db)
		if err != nil {
			t.Fatalf("read daemon_meta: %v", err)
		}
		if persisted != itoa(large) {
			t.Fatalf("daemon_meta = %q, want %q (a large advance must persist)", persisted, itoa(large))
		}
	})
}

func readDaemonMeta(t *testing.T, db *sql.DB) (string, error) {
	t.Helper()
	var value string
	err := db.QueryRow(
		"select value from daemon_meta where key = ?", LogicalClockMetaKey,
	).Scan(&value)
	return value, err
}

// itoa renders an epoch-milliseconds value the way daemon_meta stores it.
func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

// reasonOf renders a nullable invalidated_reason for assertion messages.
func reasonOf(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

// attemptsOf renders a nullable delivery_attempts for assertion messages.
func attemptsOf(value *int) string {
	if value == nil {
		return "<nil>"
	}
	return strconv.Itoa(*value)
}

func TestIsDeliverableAgentEvent(t *testing.T) {
	scope := observability.AgentScope{HerdrSessionName: "default", WorkspaceID: "wB"}
	liveAgent := func() *observability.AgentIndexRecord {
		return &observability.AgentIndexRecord{
			Agent:            stringPtr("pi"),
			AgentStatus:      observability.AgentStatusWorking,
			HerdrSessionName: "default",
			ID:               "agent_1",
			PaneID:           "wB:p1",
			PaneGeneration:   stringPtr("gen_1"),
			WorkspaceID:      "wB",
		}
	}
	baseEvent := func() observability.AgentEventRecord {
		return observability.AgentEventRecord{
			AgentID:          stringPtr("agent_1"),
			CreatedAt:        formatTimestamp(time.Now().UnixMilli()),
			Deliverable:      1,
			HerdrSessionName: "default",
			ID:               1,
			PaneID:           stringPtr("wB:p1"),
			PaneGeneration:   stringPtr("gen_1"),
			Payload:          map[string]any{"from": "working"},
			Status:           statusPtr(observability.AgentEventStatusPending),
			TerminalID:       stringPtr("term_1"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      stringPtr("wB"),
		}
	}

	t.Run("admits a pending event for a live pane", func(t *testing.T) {
		if !IsDeliverableAgentEvent(baseEvent(), liveAgent(), scope, "term_owner") {
			t.Fatalf("a pending done event on a live pane must be deliverable")
		}
	})

	t.Run("rejects the owner terminal, status changes and non-working idles", func(t *testing.T) {
		event := baseEvent()
		event.TerminalID = stringPtr("term_owner")
		if IsDeliverableAgentEvent(event, liveAgent(), scope, "term_owner") {
			t.Fatalf("the owner's own terminal must not receive its own event")
		}

		event = baseEvent()
		event.Type = observability.AgentEventStatusChanged
		if IsDeliverableAgentEvent(event, liveAgent(), scope, "term_other") {
			t.Fatalf("agent.status.changed is never delivered")
		}

		event = baseEvent()
		event.Type = observability.AgentEventIdle
		event.Payload = map[string]any{"from": "unknown"}
		if IsDeliverableAgentEvent(event, liveAgent(), scope, "term_other") {
			t.Fatalf("an idle that did not come from working is never delivered")
		}
	})

	t.Run("rejects rows outside the scope", func(t *testing.T) {
		event := baseEvent()
		event.WorkspaceID = stringPtr("wA")
		if IsDeliverableAgentEvent(event, liveAgent(), scope, "term_other") {
			t.Fatalf("another workspace must not be delivered")
		}

		event = baseEvent()
		event.HerdrSessionName = "other"
		if IsDeliverableAgentEvent(event, liveAgent(), scope, "term_other") {
			t.Fatalf("another Herdr session must not be delivered")
		}

		event = baseEvent()
		event.TerminalID = nil
		if IsDeliverableAgentEvent(event, liveAgent(), scope, "term_other") {
			t.Fatalf("an event without a terminal must not be delivered")
		}

		event = baseEvent()
		event.PaneGeneration = stringPtr("gen_0")
		if IsDeliverableAgentEvent(event, liveAgent(), scope, "term_other") {
			t.Fatalf("a stale pane generation must not be delivered")
		}
	})

	t.Run("honours the retry window and the delivered terminal", func(t *testing.T) {
		event := baseEvent()
		event.NextAttemptAt = stringPtr(formatTimestamp(time.Now().UnixMilli() + 60_000))
		if IsDeliverableAgentEvent(event, liveAgent(), scope, "term_other") {
			t.Fatalf("a retry scheduled in the future must not be delivered")
		}

		event = baseEvent()
		event.NextAttemptAt = stringPtr(formatTimestamp(time.Now().UnixMilli() - 1_000))
		if !IsDeliverableAgentEvent(event, liveAgent(), scope, "term_other") {
			t.Fatalf("a retry whose window elapsed must be delivered")
		}

		event = baseEvent()
		event.Status = statusPtr(observability.AgentEventStatusDelivered)
		event.DeliveredToTerminalID = stringPtr("term_other")
		if IsDeliverableAgentEvent(event, liveAgent(), scope, "term_owner") {
			t.Fatalf("a delivered row belongs to the terminal it was delivered to")
		}
		if !IsDeliverableAgentEvent(event, liveAgent(), scope, "term_other") {
			t.Fatalf("the delivered terminal must still see its own row")
		}
	})

	t.Run("keeps the pane guards for a live agent", func(t *testing.T) {
		agent := liveAgent()
		agent.PaneID = "wB:p2"
		if IsDeliverableAgentEvent(baseEvent(), agent, scope, "term_other") {
			t.Fatalf("an event from another pane must not be delivered")
		}

		agent = liveAgent()
		agent.WorkspaceID = "wA"
		if IsDeliverableAgentEvent(baseEvent(), agent, scope, "term_other") {
			t.Fatalf("an agent in another workspace must not be delivered")
		}

		if IsDeliverableAgentEvent(baseEvent(), nil, scope, "term_other") {
			t.Fatalf("a non-retained row without a live agent must not be delivered")
		}
	})

	t.Run("requires an assistant message for non-pi terminal events", func(t *testing.T) {
		// The guard reads the agent record's own kind, not the payload's.
		agent := liveAgent()
		agent.Agent = stringPtr("codex")
		event := baseEvent()
		event.Type = observability.AgentEventIdle
		event.Payload = map[string]any{"from": "working"}
		if IsDeliverableAgentEvent(event, agent, scope, "term_other") {
			t.Fatalf("a non-pi idle without an assistant message must not be delivered")
		}
		event.CompactHistory = compactHistoryWithAssistant("hello")
		if !IsDeliverableAgentEvent(event, agent, scope, "term_other") {
			t.Fatalf("a non-pi idle with an assistant message must be delivered")
		}
	})

	t.Run("suppresses interactive pi idles", func(t *testing.T) {
		agent := liveAgent()
		agent.AgentSession = &observability.AgentSessionRef{
			Agent: "pi", Kind: observability.SessionRefPath,
			Source: "pi", Value: "/tmp/interactive-session.jsonl",
		}
		event := baseEvent()
		event.Type = observability.AgentEventIdle
		event.Payload = map[string]any{"from": "working"}
		if IsDeliverableAgentEvent(event, agent, scope, "term_other") {
			t.Fatalf("an interactive pi idle is consumed in place, never delivered")
		}
	})

	t.Run("validates retained and orphan rows from their own metadata", func(t *testing.T) {
		retained := baseEvent()
		retained.Status = statusPtr(observability.AgentEventStatusInvalidated)
		retained.InvalidatedReason = stringPtr("RETAINED_OUTCOME_PANE_CLOSED")
		retained.PaneGeneration = nil
		if !IsDeliverableAgentEvent(retained, nil, scope, "term_other") {
			t.Fatalf("a retained invalidated row survives without its agents row")
		}

		orphan := baseEvent()
		orphan.AgentID = nil
		if !IsDeliverableAgentEvent(orphan, nil, scope, "term_other") {
			t.Fatalf("an orphan done row survives without its agents row")
		}

		failed := baseEvent()
		failed.AgentID = nil
		failed.Type = observability.AgentEventFailed
		failed.Payload = map[string]any{}
		if !IsDeliverableAgentEvent(failed, nil, scope, "term_other") {
			t.Fatalf("a failed row survives without its agents row")
		}

		interactiveOrphan := baseEvent()
		interactiveOrphan.AgentID = nil
		interactiveOrphan.Type = observability.AgentEventIdle
		interactiveOrphan.Payload = map[string]any{"from": "working", "interactive": true}
		if IsDeliverableAgentEvent(interactiveOrphan, nil, scope, "term_other") {
			t.Fatalf("an interactive orphan idle must not be delivered")
		}

		// An orphan outcome is validated from its own metadata, so a stale pane
		// generation no longer disqualifies it.
		staleGeneration := baseEvent()
		staleGeneration.AgentID = nil
		staleGeneration.PaneGeneration = stringPtr("gen_0")
		if !IsDeliverableAgentEvent(staleGeneration, nil, scope, "term_other") {
			t.Fatalf("an orphan outcome on a stale pane generation must stay deliverable")
		}
	})
}

func statusPtr(status observability.AgentEventStatus) *observability.AgentEventStatus {
	return &status
}

func TestAgentEventStoreInvalidatePane(t *testing.T) {
	t.Run("invalidates every active row and retains the fresh outcomes", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		freshDone := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{"from": "working"},
			Type:    observability.AgentEventDone,
		})
		staleDone := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{"from": "working"},
			Type:    observability.AgentEventDone,
		})
		discarded := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventDiscarded,
		})
		pending := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventBlocked,
		})

		if err := store.InvalidatePane(InvalidatePaneInput{
			HerdrSessionName: "default",
			PaneID:           "wB:p1",
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}

		for _, id := range []int64{discarded.ID, pending.ID} {
			record, err := store.Get(id)
			if err != nil {
				t.Fatalf("get(%d): %v", id, err)
			}
			if record.Status == nil || *record.Status != observability.AgentEventStatusInvalidated {
				t.Fatalf("event %d status = %v, want invalidated", id, record.Status)
			}
			if reasonOf(record.InvalidatedReason) != "LEGACY_CLOSE_WITHOUT_GENERATION" {
				t.Fatalf("event %d reason = %s, want LEGACY_CLOSE_WITHOUT_GENERATION",
					id, reasonOf(record.InvalidatedReason))
			}
			if record.Deliverable != 0 {
				t.Fatalf("event %d deliverable = %d, want 0", id, record.Deliverable)
			}
		}
		for _, id := range []int64{freshDone.ID, staleDone.ID} {
			record, err := store.Get(id)
			if err != nil {
				t.Fatalf("get(%d): %v", id, err)
			}
			if record.Deliverable != 1 {
				t.Fatalf("event %d deliverable = %d, want 1 (retained outcome)",
					id, record.Deliverable)
			}
			if reasonOf(record.InvalidatedReason) != "RETAINED_OUTCOME_PANE_CLOSED" {
				t.Fatalf("event %d reason = %s, want RETAINED_OUTCOME_PANE_CLOSED",
					id, reasonOf(record.InvalidatedReason))
			}
		}
		retained, err := store.Get(freshDone.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", freshDone.ID, err)
		}
		if retained.Deliverable != 1 {
			t.Fatalf("a fresh done row must stay deliverable, got %d", retained.Deliverable)
		}
		if retained.InvalidatedReason == nil ||
			*retained.InvalidatedReason != "RETAINED_OUTCOME_PANE_CLOSED" {
			t.Fatalf("reason = %v, want RETAINED_OUTCOME_PANE_CLOSED", retained.InvalidatedReason)
		}
		stale, err := store.Get(staleDone.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", staleDone.ID, err)
		}
		if stale.Deliverable != 1 {
			t.Fatalf("a fresh done row inside the window must stay deliverable, got %d",
				stale.Deliverable)
		}
		drop, err := store.Get(discarded.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", discarded.ID, err)
		}
		if drop.Deliverable != 0 {
			t.Fatalf("agent.discarded is never retained, got deliverable = %d", drop.Deliverable)
		}
		still, err := store.Get(pending.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", pending.ID, err)
		}
		if still.Deliverable != 0 {
			t.Fatalf("a pending non-outcome row is not retained, got deliverable = %d",
				still.Deliverable)
		}
	})

	t.Run("ages out the retained outcome window", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		old := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{"from": "working"},
			Type:    observability.AgentEventDone,
		})
		if _, err := db.Exec(
			"update agent_events set created_at = ? where id = ?",
			time.Now().UnixMilli()-RedeliveryFreshnessMs-1_000, old.ID,
		); err != nil {
			t.Fatalf("age the row: %v", err)
		}

		if err := store.InvalidatePane(InvalidatePaneInput{
			HerdrSessionName: "default",
			PaneID:           "wB:p1",
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}
		record, err := store.Get(old.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", old.ID, err)
		}
		if record.Deliverable != 0 {
			t.Fatalf("a done row older than the freshness window must not be retained,"+
				" got deliverable = %d", record.Deliverable)
		}
	})

	t.Run("retains a never-delivered agent.failed without a freshness window", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		failed := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventFailed,
		})
		if _, err := db.Exec(
			"update agent_events set created_at = ? where id = ?",
			time.Now().UnixMilli()-RedeliveryFreshnessMs-60_000, failed.ID,
		); err != nil {
			t.Fatalf("age the row: %v", err)
		}

		if err := store.InvalidatePane(InvalidatePaneInput{
			HerdrSessionName: "default",
			PaneID:           "wB:p1",
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}
		record, err := store.Get(failed.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", failed.ID, err)
		}
		if record.Deliverable != 1 {
			t.Fatalf("a never-delivered agent.failed must be retained, got deliverable = %d",
				record.Deliverable)
		}
	})

	t.Run("only touches the named generation and the legacy rows", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		current := appendEvent(t, store, AppendAgentEventInput{
			PaneGeneration: stringPtr("gen_2"),
			Payload:        map[string]any{},
			Type:           observability.AgentEventBlocked,
		})
		older := appendEvent(t, store, AppendAgentEventInput{
			PaneGeneration: stringPtr("gen_1"),
			Payload:        map[string]any{},
			Type:           observability.AgentEventBlocked,
		})
		legacy := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventBlocked,
		})

		if err := store.InvalidatePane(InvalidatePaneInput{
			HerdrSessionName: "default",
			PaneID:           "wB:p1",
			PaneGeneration:   stringPtr("gen_2"),
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}

		for _, id := range []int64{current.ID, older.ID, legacy.ID} {
			record, err := store.Get(id)
			if err != nil {
				t.Fatalf("get(%d): %v", id, err)
			}
			if record.Status == nil || *record.Status != observability.AgentEventStatusInvalidated {
				t.Fatalf("event %d status = %v, want invalidated", id, record.Status)
			}
			if record.InvalidatedReason == nil || *record.InvalidatedReason != "PANE_CLOSED" {
				t.Fatalf("event %d reason = %v, want PANE_CLOSED", id, record.InvalidatedReason)
			}
		}
	})

	t.Run("leaves a newer generation alone", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		older := appendEvent(t, store, AppendAgentEventInput{
			PaneGeneration: stringPtr("gen_1"),
			Payload:        map[string]any{},
			Type:           observability.AgentEventBlocked,
		})
		newer := appendEvent(t, store, AppendAgentEventInput{
			PaneGeneration: stringPtr("gen_2"),
			Payload:        map[string]any{},
			Type:           observability.AgentEventBlocked,
		})

		if err := store.InvalidatePane(InvalidatePaneInput{
			HerdrSessionName: "default",
			PaneID:           "wB:p1",
			PaneGeneration:   stringPtr("gen_1"),
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}

		stale, err := store.Get(older.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", older.ID, err)
		}
		if stale.Status == nil || *stale.Status != observability.AgentEventStatusInvalidated {
			t.Fatalf("the closed generation status = %v, want invalidated", stale.Status)
		}
		kept, err := store.Get(newer.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", newer.ID, err)
		}
		if kept.Status == nil || *kept.Status != observability.AgentEventStatusPending {
			t.Fatalf("the newer generation status = %v, want pending", kept.Status)
		}
		if kept.Deliverable != 1 {
			t.Fatalf("the newer generation deliverable = %d, want 1", kept.Deliverable)
		}
	})

	t.Run("acks the delivered rows only on an explicit close", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		delivered := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventBlocked,
		})
		never := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventBlocked,
		})
		if _, err := store.ReservePending("term_1", 0, []int64{delivered.ID}); err != nil {
			t.Fatalf("reservePending: %v", err)
		}

		acknowledge := true
		if err := store.InvalidatePane(InvalidatePaneInput{
			AcknowledgeDelivered: &acknowledge,
			HerdrSessionName:     "default",
			PaneID:               "wB:p1",
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}

		acked, err := store.Get(delivered.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", delivered.ID, err)
		}
		if acked.Status == nil || *acked.Status != observability.AgentEventStatusAcked {
			t.Fatalf("delivered row status = %v, want acked", acked.Status)
		}
		if acked.Deliverable != 0 {
			t.Fatalf("delivered row deliverable = %d, want 0", acked.Deliverable)
		}
		if acked.InvalidatedReason != nil {
			t.Fatalf("delivered row reason = %v, want nil", acked.InvalidatedReason)
		}
		kept, err := store.Get(never.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", never.ID, err)
		}
		if kept.Status == nil || *kept.Status != observability.AgentEventStatusInvalidated {
			t.Fatalf("never-delivered row status = %v, want invalidated", kept.Status)
		}
		if _, err := db.Exec(
			"update agent_events set delivery_attempts = 0 where id = ?", delivered.ID,
		); err != nil {
			t.Fatalf("reset the delivery attempts: %v", err)
		}
	})

	t.Run("invalidates the direct form without a transaction", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		record := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{},
			Type:    observability.AgentEventBlocked,
		})
		if err := store.InvalidatePaneDirect(InvalidatePaneInput{
			HerdrSessionName: "default",
			PaneID:           "wB:p1",
		}); err != nil {
			t.Fatalf("invalidatePaneDirect: %v", err)
		}
		got, err := store.Get(record.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", record.ID, err)
		}
		if got.Status == nil || *got.Status != observability.AgentEventStatusInvalidated {
			t.Fatalf("status = %v, want invalidated", got.Status)
		}
	})
}

func TestAgentEventStoreReconcile(t *testing.T) {
	t.Run("lists only the pending and delivered rows above the cursor", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		first := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		second := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		third := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		if _, err := store.ReservePending("term_1", 0, []int64{first.ID}); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		if err := store.MarkAcked(third.ID, nil); err != nil {
			t.Fatalf("markAcked: %v", err)
		}

		records, err := store.ListReconcileCandidates(0, 0)
		if err != nil {
			t.Fatalf("listReconcileCandidates: %v", err)
		}
		if len(records) != 2 {
			t.Fatalf("candidates = %d, want 2 (pending and delivered only)", len(records))
		}
		if records[0].ID != first.ID || records[1].ID != second.ID {
			t.Fatalf("candidates = [%d %d], want [%d %d]",
				records[0].ID, records[1].ID, first.ID, second.ID)
		}

		after, err := store.ListReconcileCandidates(0, first.ID)
		if err != nil {
			t.Fatalf("listReconcileCandidates: %v", err)
		}
		if len(after) != 1 || after[0].ID != second.ID {
			t.Fatalf("candidates after %d = %v, want [%d]", first.ID, eventIDs(after), second.ID)
		}

		limited, err := store.ListReconcileCandidates(1, 0)
		if err != nil {
			t.Fatalf("listReconcileCandidates: %v", err)
		}
		if len(limited) != 1 || limited[0].ID != first.ID {
			t.Fatalf("limited candidates = %v, want [%d]", eventIDs(limited), first.ID)
		}
	})

	t.Run("marks a candidate retained and refuses to delete it afterwards", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		record := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})

		marked, err := store.MarkRetainedInvalidated(record.ID)
		if err != nil {
			t.Fatalf("markRetainedInvalidated: %v", err)
		}
		if !marked {
			t.Fatalf("markRetainedInvalidated(%d) = false, want true", record.ID)
		}
		got, err := store.Get(record.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", record.ID, err)
		}
		if got.Status == nil || *got.Status != observability.AgentEventStatusInvalidated ||
			got.Deliverable != 1 ||
			got.InvalidatedReason == nil ||
			*got.InvalidatedReason != "RETAINED_OUTCOME_PANE_CLOSED" {
			t.Fatalf("retained row = %#v", got)
		}

		again, err := store.MarkRetainedInvalidated(record.ID)
		if err != nil {
			t.Fatalf("markRetainedInvalidated again: %v", err)
		}
		if again {
			t.Fatalf("a second markRetainedInvalidated must not report a change")
		}
		deleted, err := store.DeleteReconcileCandidate(record.ID)
		if err != nil {
			t.Fatalf("deleteReconcileCandidate: %v", err)
		}
		if deleted {
			t.Fatalf("a retained row must no longer be a reconcile candidate")
		}
	})

	t.Run("deletes a pending candidate", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		record := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		deleted, err := store.DeleteReconcileCandidate(record.ID)
		if err != nil {
			t.Fatalf("deleteReconcileCandidate: %v", err)
		}
		if !deleted {
			t.Fatalf("deleteReconcileCandidate(%d) = false, want true", record.ID)
		}
		if _, err := store.Get(record.ID); err == nil {
			t.Fatalf("the deleted row is still readable")
		}
	})
}

func eventIDs(records []observability.AgentEventRecord) []int64 {
	ids := []int64{}
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

func TestAgentEventStoreInvalidateByID(t *testing.T) {
	t.Run("invalidates an active row with the given reason", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		record := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		invalidated, err := store.InvalidateByID(record.ID, "SUPERSEDED")
		if err != nil {
			t.Fatalf("invalidateById: %v", err)
		}
		if !invalidated {
			t.Fatalf("invalidateById = false, want true")
		}
		got, err := store.Get(record.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", record.ID, err)
		}
		if got.Status == nil || *got.Status != observability.AgentEventStatusInvalidated ||
			got.Deliverable != 0 || got.InvalidatedReason == nil ||
			*got.InvalidatedReason != "SUPERSEDED" {
			t.Fatalf("invalidated row = %#v", got)
		}

		again, err := store.InvalidateByID(record.ID, "SUPERSEDED")
		if err != nil {
			t.Fatalf("invalidateById again: %v", err)
		}
		if again {
			t.Fatalf("an already invalidated row must not be invalidated twice")
		}
	})
}

func TestAgentEventStoreTTL(t *testing.T) {
	t.Run("ages invalidated rows from their last delivery attempt", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		aged := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		kept := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		for _, id := range []int64{aged.ID, kept.ID} {
			if _, err := store.InvalidateByID(id, "SUPERSEDED"); err != nil {
				t.Fatalf("invalidateById(%d): %v", id, err)
			}
		}
		// The aged row was delivered first and then invalidated, so its age is
		// measured from last_attempt_at, not from created_at.
		lastAttempt := time.Now().UnixMilli() - 7_200_000
		if _, err := db.Exec(
			"update agent_events set last_attempt_at = ?, created_at = ? where id = ?",
			lastAttempt, time.Now().UnixMilli(), aged.ID,
		); err != nil {
			t.Fatalf("age the delivered row: %v", err)
		}

		deleted, err := store.DeleteInvalidatedOlderThan(3_600_000)
		if err != nil {
			t.Fatalf("deleteInvalidatedOlderThan: %v", err)
		}
		if deleted != 1 {
			t.Fatalf("deleted = %d, want 1", deleted)
		}
		if _, err := store.Get(aged.ID); err == nil {
			t.Fatalf("the aged invalidated row is still present")
		}
		if _, err := store.Get(kept.ID); err != nil {
			t.Fatalf("the fresh invalidated row was removed: %v", err)
		}
	})

	t.Run("keeps the retained invalidated rows out of the ordinary TTL", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		retained := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{"from": "working"}, Type: observability.AgentEventDone,
		})
		if err := store.InvalidatePane(InvalidatePaneInput{
			HerdrSessionName: "default", PaneID: "wB:p1",
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}
		if _, err := db.Exec(
			"update agent_events set created_at = ? where id = ?",
			time.Now().UnixMilli()-7_200_000, retained.ID,
		); err != nil {
			t.Fatalf("age the retained row: %v", err)
		}

		deleted, err := store.DeleteInvalidatedOlderThan(3_600_000)
		if err != nil {
			t.Fatalf("deleteInvalidatedOlderThan: %v", err)
		}
		if deleted != 0 {
			t.Fatalf("deleted = %d, want 0 (deliverable = 1 rows are not collected here)", deleted)
		}

		settled, err := store.DeleteSettledOlderThan(3_600_000)
		if err != nil {
			t.Fatalf("deleteSettledOlderThan: %v", err)
		}
		if settled != 1 {
			t.Fatalf("settled = %d, want 1", settled)
		}
		if _, err := store.Get(retained.ID); err == nil {
			t.Fatalf("the aged retained row is still present")
		}
	})

	t.Run("removes acked and failed rows but never an active one", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		acked := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		failed := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		active := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		if err := store.MarkAcked(acked.ID, nil); err != nil {
			t.Fatalf("markAcked: %v", err)
		}
		if _, err := db.Exec(
			"update agent_events set status = 'failed', last_failure_code = 'X' where id = ?",
			failed.ID,
		); err != nil {
			t.Fatalf("mark the failed row: %v", err)
		}
		for _, id := range []int64{acked.ID, failed.ID} {
			if _, err := db.Exec(
				"update agent_events set created_at = ? where id = ?",
				time.Now().UnixMilli()-7_200_000, id,
			); err != nil {
				t.Fatalf("age the settled row: %v", err)
			}
		}

		deleted, err := store.DeleteSettledOlderThan(3_600_000)
		if err != nil {
			t.Fatalf("deleteSettledOlderThan: %v", err)
		}
		if deleted != 2 {
			t.Fatalf("deleted = %d, want 2", deleted)
		}
		if _, err := store.Get(active.ID); err != nil {
			t.Fatalf("the active row was removed: %v", err)
		}
	})
}

func TestAgentEventStoreAcknowledge(t *testing.T) {
	t.Run("acks the self-owned pending rows of a pane", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		mine := appendEvent(t, store, AppendAgentEventInput{
			PaneID: stringPtr("wB:p1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked, WorkspaceID: stringPtr("wB"),
		})
		otherPane := appendEvent(t, store, AppendAgentEventInput{
			PaneID: stringPtr("wB:p2"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked, WorkspaceID: stringPtr("wB"),
		})
		otherWorkspace := appendEvent(t, store, AppendAgentEventInput{
			PaneID: stringPtr("wB:p1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked, WorkspaceID: stringPtr("wA"),
		})

		acked, err := store.AckSelfOwned(AckSelfOwnedInput{
			HerdrSessionName: "default", WorkspaceID: "wB", PaneID: "wB:p1",
		})
		if err != nil {
			t.Fatalf("ackSelfOwned: %v", err)
		}
		if acked != 1 {
			t.Fatalf("acked = %d, want 1", acked)
		}
		got, err := store.Get(mine.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", mine.ID, err)
		}
		if got.Status == nil || *got.Status != observability.AgentEventStatusAcked ||
			got.Deliverable != 0 {
			t.Fatalf("acked row = %#v", got)
		}
		for _, id := range []int64{otherPane.ID, otherWorkspace.ID} {
			untouched, err := store.Get(id)
			if err != nil {
				t.Fatalf("get(%d): %v", id, err)
			}
			if untouched.Status == nil || *untouched.Status != observability.AgentEventStatusPending {
				t.Fatalf("row %d status = %v, want pending", id, untouched.Status)
			}
		}
	})

	t.Run("acks the cursor inside a scope and a single row outside it", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		inScope := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
			WorkspaceID: stringPtr("wB"),
		})
		alsoInScope := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
			WorkspaceID: stringPtr("wB"),
		})
		elsewhere := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
			WorkspaceID: stringPtr("wA"),
		})
		if _, err := store.ReservePending("term_1", 0, []int64{alsoInScope.ID}); err != nil {
			t.Fatalf("reservePending: %v", err)
		}

		if err := store.MarkAcked(alsoInScope.ID, &observability.AgentScope{
			HerdrSessionName: "default", WorkspaceID: "wB",
		}); err != nil {
			t.Fatalf("markAcked with scope: %v", err)
		}
		for _, id := range []int64{inScope.ID, alsoInScope.ID} {
			got, err := store.Get(id)
			if err != nil {
				t.Fatalf("get(%d): %v", id, err)
			}
			if got.Status == nil || *got.Status != observability.AgentEventStatusAcked {
				t.Fatalf("row %d status = %v, want acked", id, got.Status)
			}
		}
		outside, err := store.Get(elsewhere.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", elsewhere.ID, err)
		}
		if outside.Status == nil || *outside.Status != observability.AgentEventStatusPending {
			t.Fatalf("row %d status = %v, want pending", elsewhere.ID, outside.Status)
		}

		if err := store.MarkAcked(elsewhere.ID, nil); err != nil {
			t.Fatalf("markAcked without scope: %v", err)
		}
		outside, err = store.Get(elsewhere.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", elsewhere.ID, err)
		}
		if outside.Status == nil || *outside.Status != observability.AgentEventStatusAcked {
			t.Fatalf("row %d status = %v, want acked", elsewhere.ID, outside.Status)
		}
	})

	t.Run("acks a retained invalidated row through the cursor", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		retained := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{"from": "working"}, Type: observability.AgentEventDone,
		})
		if err := store.InvalidatePane(InvalidatePaneInput{
			HerdrSessionName: "default", PaneID: "wB:p1",
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}
		if err := store.MarkAcked(retained.ID, nil); err != nil {
			t.Fatalf("markAcked: %v", err)
		}
		got, err := store.Get(retained.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", retained.ID, err)
		}
		if got.Status == nil || *got.Status != observability.AgentEventStatusAcked ||
			got.Deliverable != 0 {
			t.Fatalf("retained row after ack = %#v", got)
		}
	})
}

func TestAgentEventStoreReservePending(t *testing.T) {
	t.Run("marks the matching rows delivered for the owner terminal", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		ready := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		scheduled := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		if _, err := store.db.Exec(
			"update agent_events set next_attempt_at = ? where id = ?",
			time.Now().UnixMilli()+60_000, scheduled.ID,
		); err != nil {
			t.Fatalf("schedule the retry: %v", err)
		}
		owned := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})

		reserved, err := store.ReservePending("term_1", 0, nil)
		if err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		if len(reserved) != 2 {
			t.Fatalf("reserved = %v, want [%d %d] (the scheduled retry is skipped)",
				eventIDs(reserved), ready.ID, owned.ID)
		}
		if reserved[0].ID != ready.ID || reserved[1].ID != owned.ID {
			t.Fatalf("reserved = %v, want [%d %d]", eventIDs(reserved), ready.ID, owned.ID)
		}
		for _, record := range reserved {
			if record.Status == nil || *record.Status != observability.AgentEventStatusDelivered {
				t.Fatalf("reserved row %d status = %v, want delivered", record.ID, record.Status)
			}
			if record.DeliveredToTerminalID == nil || *record.DeliveredToTerminalID != "term_1" {
				t.Fatalf("reserved row %d terminal = %v, want term_1",
					record.ID, record.DeliveredToTerminalID)
			}
			if record.DeliveryAttempts == nil || *record.DeliveryAttempts != 1 {
				t.Fatalf("reserved row %d attempts = %v, want 1",
					record.ID, record.DeliveryAttempts)
			}
			if record.LastAttemptAt == nil {
				t.Fatalf("reserved row %d has no lastAttemptAt", record.ID)
			}
			if record.Deliverable != 1 {
				t.Fatalf("reserved row %d deliverable = %d, want 1", record.ID, record.Deliverable)
			}
		}
		scheduledRow, err := store.Get(scheduled.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", scheduled.ID, err)
		}
		if scheduledRow.Status == nil ||
			*scheduledRow.Status != observability.AgentEventStatusPending {
			t.Fatalf("scheduled row status = %v, want pending", scheduledRow.Status)
		}
	})

	t.Run("re-reserves a delivered row for the same terminal only inside the window", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		record := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		if _, err := store.ReservePending("term_1", 0, nil); err != nil {
			t.Fatalf("first reservePending: %v", err)
		}

		again, err := store.ReservePending("term_1", 0, nil)
		if err != nil {
			t.Fatalf("second reservePending: %v", err)
		}
		if len(again) != 1 {
			t.Fatalf("re-reserved = %v, want [%d] for the owning terminal",
				eventIDs(again), record.ID)
		}
		// The re-reserve guard only moves a pending or retained row, so an
		// already delivered row keeps its attempt count.
		got, err := store.Get(record.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", record.ID, err)
		}
		if attemptsOf(got.DeliveryAttempts) != "1" {
			t.Fatalf("deliveryAttempts = %s, want 1 (an already delivered row is not"+
				" re-counted)", attemptsOf(got.DeliveryAttempts))
		}

		other, err := store.ReservePending("term_2", 0, nil)
		if err != nil {
			t.Fatalf("reservePending for another terminal: %v", err)
		}
		if len(other) != 0 {
			t.Fatalf("another terminal re-reserved %v, want none", eventIDs(other))
		}

		if _, err := db.Exec(
			"update agent_events set last_attempt_at = ? where id = ?",
			time.Now().UnixMilli()-RedeliveryFreshnessMs-1_000, record.ID,
		); err != nil {
			t.Fatalf("age the delivery: %v", err)
		}
		stale, err := store.ReservePending("term_1", 0, nil)
		if err != nil {
			t.Fatalf("reservePending after the freshness window: %v", err)
		}
		if len(stale) != 0 {
			t.Fatalf("a stale delivery was re-reserved: %v", eventIDs(stale))
		}
	})

	t.Run("re-reserves a retained invalidated row once", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		record := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{"from": "working"}, Type: observability.AgentEventDone,
		})
		if err := store.InvalidatePane(InvalidatePaneInput{
			HerdrSessionName: "default", PaneID: "wB:p1",
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}
		reserved, err := store.ReservePending("term_1", 0, nil)
		if err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		if len(reserved) != 1 || reserved[0].ID != record.ID {
			t.Fatalf("reserved = %v, want [%d]", eventIDs(reserved), record.ID)
		}
		if reserved[0].Status == nil ||
			*reserved[0].Status != observability.AgentEventStatusDelivered {
			t.Fatalf("status = %v, want delivered", reserved[0].Status)
		}
	})
}

func TestAgentEventStoreReclaimDelivered(t *testing.T) {
	t.Run("invalidates a delivery whose pane is gone", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		record := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		if _, err := store.ReservePending("term_1", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		// Pane retirement physically removes the agents row, which the ON DELETE
		// SET NULL foreign key turns into an orphan event.
		if _, err := db.Exec("delete from agents where id = ?", "agent_1"); err != nil {
			t.Fatalf("retire the pane: %v", err)
		}
		if _, err := db.Exec(
			"update agent_events set last_attempt_at = ? where id = ?",
			time.Now().UnixMilli()-120_000, record.ID,
		); err != nil {
			t.Fatalf("age the delivery: %v", err)
		}

		reclaimed, err := store.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 1 {
			t.Fatalf("reclaimed = %d, want 1", reclaimed)
		}
		got, err := store.Get(record.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", record.ID, err)
		}
		if got.Status == nil || *got.Status != observability.AgentEventStatusInvalidated ||
			got.Deliverable != 0 || got.InvalidatedReason == nil ||
			*got.InvalidatedReason != "PANE_CLOSED_RECLAIM" {
			t.Fatalf("reclaimed row = %#v", got)
		}
	})

	t.Run("leaves an in-flight delivery with its owner alone", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		record := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		if _, err := store.ReservePending("term_1", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		if _, err := db.Exec(
			`insert into agent_orchestrator_scopes
			   (acked_event_id, created_at, herdr_session_name, owner_pane_id, owner_terminal_id, updated_at, workspace_id)
			 values (0, ?, 'default', 'wB:p1', 'term_1', ?, 'wB')`,
			time.Now().UnixMilli(), time.Now().UnixMilli(),
		); err != nil {
			t.Fatalf("claim the scope: %v", err)
		}

		reclaimed, err := store.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 0 {
			t.Fatalf("reclaimed = %d, want 0 (the owner still holds a scope)", reclaimed)
		}
		got, err := store.Get(record.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", record.ID, err)
		}
		if got.Status == nil || *got.Status != observability.AgentEventStatusDelivered {
			t.Fatalf("status = %v, want delivered", got.Status)
		}
	})

	t.Run("reclaims a delivery whose owner lost the scope", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		record := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		if _, err := store.ReservePending("term_1", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}

		reclaimed, err := store.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 1 {
			t.Fatalf("reclaimed = %d, want 1", reclaimed)
		}
		got, err := store.Get(record.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", record.ID, err)
		}
		if got.Status == nil || *got.Status != observability.AgentEventStatusPending {
			t.Fatalf("status = %v, want pending", got.Status)
		}
		if got.Deliverable != 1 {
			t.Fatalf("deliverable = %d, want 1", got.Deliverable)
		}
		if got.DeliveredToTerminalID != nil {
			t.Fatalf("deliveredToTerminalId = %v, want nil", got.DeliveredToTerminalID)
		}
		if got.NextAttemptAt != nil {
			t.Fatalf("nextAttemptAt = %v, want nil", got.NextAttemptAt)
		}
	})

	t.Run("fails a delivery that exhausted the attempt budget", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		record := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		if _, err := store.ReservePending("term_1", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		if _, err := db.Exec(
			"update agent_events set delivery_attempts = 12 where id = ?", record.ID,
		); err != nil {
			t.Fatalf("exhaust the attempt budget: %v", err)
		}

		reclaimed, err := store.ReclaimDelivered(60_000)
		if err != nil {
			t.Fatalf("reclaimDelivered: %v", err)
		}
		if reclaimed != 1 {
			t.Fatalf("reclaimed = %d, want 1", reclaimed)
		}
		got, err := store.Get(record.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", record.ID, err)
		}
		if got.Status == nil || *got.Status != observability.AgentEventStatusFailed {
			t.Fatalf("status = %v, want failed", got.Status)
		}
		if got.Deliverable != 0 {
			t.Fatalf("deliverable = %d, want 0", got.Deliverable)
		}
		if got.LastFailureCode == nil ||
			*got.LastFailureCode != "DELIVERY_ATTEMPTS_EXCEEDED" {
			t.Fatalf("lastFailureCode = %v, want DELIVERY_ATTEMPTS_EXCEEDED",
				got.LastFailureCode)
		}
	})
}

func TestAgentEventStoreQueries(t *testing.T) {
	t.Run("finds the latest status transition and terminal events", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		first := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{"to": "working"},
			Type: observability.AgentEventStatusChanged,
		})
		latest := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{"to": "idle"},
			Type: observability.AgentEventStatusChanged,
		})
		blocked := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		done := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{"from": "working"},
			Type: observability.AgentEventDone,
		})
		unknownIdle := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{"from": "unknown"},
			Type: observability.AgentEventIdle,
		})
		if _, err := store.InvalidateByID(blocked.ID, "SUPERSEDED"); err != nil {
			t.Fatalf("invalidateById: %v", err)
		}

		transition, err := store.LatestStatusTransition("agent_1", "default")
		if err != nil {
			t.Fatalf("latestStatusTransition: %v", err)
		}
		if transition == nil || transition.ID != latest.ID {
			t.Fatalf("latestStatusTransition = %v, want %d", eventID(transition), latest.ID)
		}

		terminal, err := store.LatestTerminalEvent("agent_1", "default")
		if err != nil {
			t.Fatalf("latestTerminalEvent: %v", err)
		}
		if terminal == nil || terminal.ID != unknownIdle.ID {
			t.Fatalf("latestTerminalEvent = %v, want %d (the invalidated blocked row is skipped)",
				eventID(terminal), unknownIdle.ID)
		}

		if after, err := store.HasTerminalEventAfter("agent_1", "default", first.ID); err != nil {
			t.Fatalf("hasTerminalEventAfter: %v", err)
		} else if !after {
			t.Fatalf("hasTerminalEventAfter(%d) = false, want true", first.ID)
		}
		if after, err := store.HasTerminalEventAfter("agent_1", "default", done.ID); err != nil {
			t.Fatalf("hasTerminalEventAfter: %v", err)
		} else if !after {
			t.Fatalf("hasTerminalEventAfter(%d) = false, want true (the idle follows the done)",
				done.ID)
		}
		if after, err := store.HasTerminalEventAfter("agent_1", "default", unknownIdle.ID); err != nil {
			t.Fatalf("hasTerminalEventAfter: %v", err)
		} else if after {
			t.Fatalf("hasTerminalEventAfter(%d) = true, want false", unknownIdle.ID)
		}

		if missing, err := store.LatestStatusTransition("agent_1", "other"); err != nil {
			t.Fatalf("latestStatusTransition(other): %v", err)
		} else if missing != nil {
			t.Fatalf("latestStatusTransition(other) = %v, want nil", eventID(missing))
		}
	})

	t.Run("finds the latest completed turn event", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		emptyIdle := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{"from": "working"},
			Type: observability.AgentEventIdle,
		})
		unknownIdle := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{"from": "unknown"},
			Type: observability.AgentEventIdle,
		})
		statusChange := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventStatusChanged,
		})
		turn := appendEvent(t, store, AppendAgentEventInput{
			AgentID:        stringPtr("agent_1"),
			CompactHistory: compactHistoryWithAssistant("the answer"),
			Payload:        map[string]any{"from": "working"},
			Type:           observability.AgentEventDone,
		})

		found, err := store.LatestCompletedTurnEvent("agent_1", "default")
		if err != nil {
			t.Fatalf("latestCompletedTurnEvent: %v", err)
		}
		if found == nil || found.ID != turn.ID {
			t.Fatalf("latestCompletedTurnEvent = %v, want %d", eventID(found), turn.ID)
		}

		// A late unknown -> idle plan must not register as a completed turn, so
		// it has to stay invisible to this query.
		for _, id := range []int64{emptyIdle.ID, unknownIdle.ID, statusChange.ID} {
			record, err := store.Get(id)
			if err != nil {
				t.Fatalf("get(%d): %v", id, err)
			}
			_ = record
		}
		if found.CompactHistory == nil || found.CompactHistory.LastAssistantMessage == nil ||
			found.CompactHistory.LastAssistantMessage.Text != "the answer" {
			t.Fatalf("compactHistory = %#v, want the assistant message",
				found.CompactHistory)
		}

		// An agent.idle from working with an assistant message also counts.
		idle := appendEvent(t, store, AppendAgentEventInput{
			AgentID:        stringPtr("agent_1"),
			CompactHistory: compactHistoryWithAssistant("later"),
			Payload:        map[string]any{"from": "working"},
			Type:           observability.AgentEventIdle,
		})
		found, err = store.LatestCompletedTurnEvent("agent_1", "default")
		if err != nil {
			t.Fatalf("latestCompletedTurnEvent: %v", err)
		}
		if found == nil || found.ID != idle.ID {
			t.Fatalf("latestCompletedTurnEvent = %v, want %d", eventID(found), idle.ID)
		}
	})

	t.Run("reports the latest event id per scope", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		if latest, err := store.LatestEventID(observability.AgentQueryScope{}); err != nil {
			t.Fatalf("latestEventId on an empty table: %v", err)
		} else if latest != 0 {
			t.Fatalf("latestEventId = %d, want 0", latest)
		}
		first := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
			WorkspaceID: stringPtr("wB"),
		})
		second := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
			WorkspaceID: stringPtr("wB"),
		})
		elsewhere := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
			WorkspaceID: stringPtr("wA"),
		})

		if latest, err := store.LatestEventID(observability.AgentQueryScope{}); err != nil {
			t.Fatalf("latestEventId: %v", err)
		} else if latest != elsewhere.ID {
			t.Fatalf("latestEventId = %d, want %d", latest, elsewhere.ID)
		}
		if latest, err := store.LatestEventID(observability.AgentQueryScope{
			WorkspaceID: stringPtr("wB"),
		}); err != nil {
			t.Fatalf("latestEventId(wB): %v", err)
		} else if latest != second.ID {
			t.Fatalf("latestEventId(wB) = %d, want %d", latest, second.ID)
		}
		if latest, err := store.LatestEventID(observability.AgentQueryScope{
			HerdrSessionName: stringPtr("default"),
		}); err != nil {
			t.Fatalf("latestEventId(default): %v", err)
		} else if latest != elsewhere.ID {
			t.Fatalf("latestEventId(default) = %d, want %d", latest, elsewhere.ID)
		}
		if latest, err := store.LatestEventID(observability.AgentQueryScope{
			HerdrSessionName: stringPtr("other"),
		}); err != nil {
			t.Fatalf("latestEventId(other): %v", err)
		} else if latest != 0 {
			t.Fatalf("latestEventId(other) = %d, want 0", latest)
		}
		if first.ID >= second.ID {
			t.Fatalf("ids are not increasing: %d then %d", first.ID, second.ID)
		}
	})
}

func eventID(record *observability.AgentEventRecord) int64 {
	if record == nil {
		return 0
	}
	return record.ID
}

func TestAgentEventStoreListAfter(t *testing.T) {
	t.Run("returns the deliverable rows above the cursor in order", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		first := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		second := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		invalidated := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		acked := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		if _, err := store.InvalidateByID(invalidated.ID, "SUPERSEDED"); err != nil {
			t.Fatalf("invalidateById: %v", err)
		}
		if err := store.MarkAcked(acked.ID, nil); err != nil {
			t.Fatalf("markAcked: %v", err)
		}

		records, err := store.ListAfter(ListAfterInput{
			HerdrSessionName: stringPtr("default"),
			WorkspaceID:      stringPtr("wB"),
			OwnerTerminalID:  stringPtr("term_1"),
		})
		if err != nil {
			t.Fatalf("listAfter: %v", err)
		}
		if len(records) != 2 || records[0].ID != first.ID || records[1].ID != second.ID {
			t.Fatalf("listAfter = %v, want [%d %d]",
				eventIDs(records), first.ID, second.ID)
		}

		after, err := store.ListAfter(ListAfterInput{
			AfterEventID:     int64Ptr(first.ID),
			HerdrSessionName: stringPtr("default"),
			OwnerTerminalID:  stringPtr("term_1"),
			WorkspaceID:      stringPtr("wB"),
		})
		if err != nil {
			t.Fatalf("listAfter: %v", err)
		}
		if len(after) != 1 || after[0].ID != second.ID {
			t.Fatalf("listAfter(%d) = %v, want [%d]", first.ID, eventIDs(after), second.ID)
		}

		limited, err := store.ListAfter(ListAfterInput{
			Limit:            intPtr(1),
			HerdrSessionName: stringPtr("default"),
			OwnerTerminalID:  stringPtr("term_1"),
			WorkspaceID:      stringPtr("wB"),
		})
		if err != nil {
			t.Fatalf("listAfter: %v", err)
		}
		if len(limited) != 1 || limited[0].ID != first.ID {
			t.Fatalf("limited listAfter = %v, want [%d]", eventIDs(limited), first.ID)
		}

		otherSession, err := store.ListAfter(ListAfterInput{
			HerdrSessionName: stringPtr("other"),
			OwnerTerminalID:  stringPtr("term_1"),
		})
		if err != nil {
			t.Fatalf("listAfter: %v", err)
		}
		if len(otherSession) != 0 {
			t.Fatalf("listAfter(other) = %v, want none", eventIDs(otherSession))
		}
	})

	t.Run("keeps a delivered row visible to its own terminal only", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		record := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		if _, err := store.ReservePending("term_1", 0, nil); err != nil {
			t.Fatalf("reservePending: %v", err)
		}

		for terminal, want := range map[string]int{"term_1": 1, "term_2": 0} {
			records, err := store.ListAfter(ListAfterInput{
				HerdrSessionName: stringPtr("default"),
				OwnerTerminalID:  stringPtr(terminal),
				WorkspaceID:      stringPtr("wB"),
			})
			if err != nil {
				t.Fatalf("listAfter(%s): %v", terminal, err)
			}
			if len(records) != want {
				t.Fatalf("listAfter(%s) = %v, want %d rows",
					terminal, eventIDs(records), want)
			}
		}
		if _, err := store.db.Exec(
			"update agent_events set last_attempt_at = ? where id = ?",
			time.Now().UnixMilli()-RedeliveryFreshnessMs-1_000, record.ID,
		); err != nil {
			t.Fatalf("age the delivery: %v", err)
		}
		stale, err := store.ListAfter(ListAfterInput{
			HerdrSessionName: stringPtr("default"),
			OwnerTerminalID:  stringPtr("term_1"),
			WorkspaceID:      stringPtr("wB"),
		})
		if err != nil {
			t.Fatalf("listAfter: %v", err)
		}
		if len(stale) != 0 {
			t.Fatalf("a stale delivery is still listed: %v", eventIDs(stale))
		}
	})
}

func TestAgentEventStoreNextDeliverableAfter(t *testing.T) {
	t.Run("filters by the live agent when no resolver is supplied", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		seedAgent(t, db, seedAgentInput{
			ID: "agent_2", PaneID: "wB:p9", TerminalID: "term_2",
		})
		live := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		// The agents row for agent_2 sits on another pane, so the pane guard in
		// SQL can no longer match and the row is skipped.
		stalePane := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_2"), PaneID: stringPtr("wB:p8"),
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		orphan := appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})

		found, err := store.NextDeliverableAfter(NextDeliverableAfterInput{
			HerdrSessionName: "default",
			OwnerTerminalID:  "term_owner",
			WorkspaceID:      "wB",
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter: %v", err)
		}
		if found == nil || found.ID != live.ID {
			t.Fatalf("nextDeliverableAfter = %v, want %d", eventID(found), live.ID)
		}
		if stalePane.ID <= live.ID || orphan.ID <= live.ID {
			t.Fatalf("fixture ids are not increasing")
		}

		// Without the pane guard the orphan row (agent_id is null) is skipped by
		// the SQL existence filter, so it never reaches the predicate.
		if orphan.AgentID == nil {
			_ = orphan
		}
	})

	t.Run("applies the shared predicate with a resolver", func(t *testing.T) {
		store, db := openAgentEventStore(t)
		seedAgent(t, db, seedAgentInput{ID: "agent_1", PaneID: "wB:p1", TerminalID: "term_1"})
		skipped := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})
		delivered := appendEvent(t, store, AppendAgentEventInput{
			AgentID: stringPtr("agent_1"), Payload: map[string]any{},
			Type: observability.AgentEventBlocked,
		})

		agents := map[string]*observability.AgentIndexRecord{}
		// Without a known agent the predicate rejects the row, so nothing is
		// deliverable yet.
		found, err := store.NextDeliverableAfter(NextDeliverableAfterInput{
			GetAgent: func(agentID string) *observability.AgentIndexRecord {
				return agents[agentID]
			},
			HerdrSessionName: "default",
			OwnerTerminalID:  "term_owner",
			WorkspaceID:      "wB",
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter: %v", err)
		}
		if found != nil {
			t.Fatalf("nextDeliverableAfter = %v, want nil (the agent is unknown)",
				eventID(found))
		}

		// A known agent whose pane no longer matches is skipped as well.
		agents["agent_1"] = &observability.AgentIndexRecord{
			Agent:            stringPtr("pi"),
			AgentStatus:      observability.AgentStatusWorking,
			HerdrSessionName: "default",
			ID:               "agent_1",
			PaneID:           "wB:p7",
			WorkspaceID:      "wB",
		}
		found, err = store.NextDeliverableAfter(NextDeliverableAfterInput{
			GetAgent: func(agentID string) *observability.AgentIndexRecord {
				return agents[agentID]
			},
			HerdrSessionName: "default",
			OwnerTerminalID:  "term_owner",
			WorkspaceID:      "wB",
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter: %v", err)
		}
		if found != nil {
			t.Fatalf("nextDeliverableAfter = %v, want nil (the pane moved)",
				eventID(found))
		}

		// With the pane back in place the oldest row wins.
		agents["agent_1"].PaneID = "wB:p1"
		found, err = store.NextDeliverableAfter(NextDeliverableAfterInput{
			GetAgent: func(agentID string) *observability.AgentIndexRecord {
				return agents[agentID]
			},
			HerdrSessionName: "default",
			OwnerTerminalID:  "term_owner",
			WorkspaceID:      "wB",
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter: %v", err)
		}
		if found == nil || found.ID != skipped.ID {
			t.Fatalf("nextDeliverableAfter = %v, want %d", eventID(found), skipped.ID)
		}

		after, err := store.NextDeliverableAfter(NextDeliverableAfterInput{
			AfterEventID: skipped.ID,
			GetAgent: func(agentID string) *observability.AgentIndexRecord {
				return agents[agentID]
			},
			HerdrSessionName: "default",
			OwnerTerminalID:  "term_owner",
			WorkspaceID:      "wB",
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter: %v", err)
		}
		if after == nil || after.ID != delivered.ID {
			t.Fatalf("nextDeliverableAfter(%d) = %v, want %d",
				skipped.ID, eventID(after), delivered.ID)
		}
	})

	t.Run("returns nil for a terminal that owns every event", func(t *testing.T) {
		store, _ := openAgentEventStore(t)
		appendEvent(t, store, AppendAgentEventInput{
			Payload: map[string]any{}, Type: observability.AgentEventBlocked,
		})
		found, err := store.NextDeliverableAfter(NextDeliverableAfterInput{
			HerdrSessionName: "default",
			OwnerTerminalID:  "term_1",
			WorkspaceID:      "wB",
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter: %v", err)
		}
		if found != nil {
			t.Fatalf("nextDeliverableAfter = %v, want nil", eventID(found))
		}
	})
}
