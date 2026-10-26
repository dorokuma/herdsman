package observability_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The port of test/integration/event-dedup-pane-generation.test.ts.
//
// The harness mirrors openObservabilityDbHarness: a migrated database in a temp
// dir with the running "default" Herdr session and every store the index service
// needs. Every test uses t.TempDir(), so the production home and the live daemon
// are never touched.
//
// The two `console.warn` assertions of the TS file have no Go counterpart (the
// port logs to stderr, which carries no spy); the invalidated reason and the
// release outcome they guard are asserted instead, which is the observable
// contract those warnings merely report.

const dedupTestSession = "default"

// dedupTestSessionName is the addressable form the *string inputs need.
var dedupTestSessionName = dedupTestSession

// dedupScope mirrors the TS `scope`: the orchestrator scope the events of this
// test file live in.
var dedupScope = observability.AgentScope{
	HerdrSessionName: dedupTestSession,
	WorkspaceID:      "wJ",
}

// dedupSession mirrors the TS `session` refresh input.
var dedupSession = observability.RefreshInput{
	HerdrSessionName: "default",
	SessionDir:       "/tmp/herdr",
	SocketPath:       "/tmp/herdr.sock",
}

type dedupHarness struct {
	sqlite      *sql.DB
	agentEvents *db.AgentEventStore
	agents      *db.AgentStore
	scopes      *db.AgentOrchestratorScopeStore
}

// openDedupHarness mirrors openObservabilityDbHarness.
func openDedupHarness(t *testing.T) *dedupHarness {
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
		Name:       dedupTestSession,
		SessionDir: "/tmp/herdr",
		SocketPath: "/tmp/herdr.sock",
	}); err != nil {
		t.Fatalf("upsertRunning: %v", err)
	}
	agentEvents, err := db.NewAgentEventStore(sqlite)
	if err != nil {
		t.Fatalf("newAgentEventStore: %v", err)
	}
	return &dedupHarness{
		sqlite:      sqlite,
		agentEvents: agentEvents,
		agents:      db.NewAgentStore(sqlite, agentEvents),
		scopes:      db.NewAgentOrchestratorScopeStore(sqlite),
	}
}

// indexService mirrors `new AgentIndexService({ stores: h })`: the stores-only
// construction, with the agent-history fake the TS side injects through the
// `history` option.
func (h *dedupHarness) indexService(t *testing.T, history observability.AgentHistoryServiceLike) *observability.AgentIndexService {
	t.Helper()
	service, err := observability.NewAgentIndexService(observability.AgentIndexServiceOptions{
		History: history,
		Stores:  h.stores(),
	})
	if err != nil {
		t.Fatalf("newAgentIndexService: %v", err)
	}
	return service
}

// stores mirrors the TS harness object the service is constructed with.
func (h *dedupHarness) stores() observability.AgentIndexServiceStores {
	return observability.AgentIndexServiceStores{
		AgentContextSnapshots:   db.NewAgentContextSnapshotStore(h.sqlite),
		AgentEvents:             h.agentEvents,
		AgentOrchestratorScopes: h.scopes,
		AgentHistoryCache:       db.NewAgentHistoryCacheStore(h.sqlite),
		Agents:                  h.agents,
		HerdrSessions:           db.NewHerdrSessionStore(h.sqlite),
		HerdrWorkspaces:         db.NewHerdrWorkspaceStore(h.sqlite),
		Sqlite:                  h.sqlite,
		StatusEventPlans:        db.NewStatusEventPlanStore(h.sqlite),
	}
}

// snapshotClient mirrors the TS clientFactory stub: a close() no-op plus one
// sessionSnapshot answer, with an optional call counter for the tombstone tests.
type snapshotClient struct {
	snapshot map[string]any
	calls    *int
}

func (c *snapshotClient) Close() {}

func (c *snapshotClient) SessionSnapshot() (any, error) {
	if c.calls != nil {
		*c.calls++
	}
	return map[string]any{"snapshot": c.snapshot}, nil
}

// snapshotFactory mirrors `clientFactory: () => ({...})`.
func snapshotFactory(client *snapshotClient) observability.HerdrClientFactory {
	return func(string) (observability.HerdrClientLike, error) { return client, nil }
}

// agentSnapshot mirrors the TS `agent(name, generation)` helper: one working
// agent on pane wJ:p2.
func agentSnapshot(name string, generation string) map[string]any {
	return map[string]any{
		"agent":           name,
		"agent_status":    "working",
		"cwd":             "/repo",
		"pane_id":         "wJ:p2",
		"pane_generation": generation,
		"revision":        1,
		"terminal_id":     "term-" + name,
		"workspace_id":    "wJ",
	}
}

// snapshotPayload mirrors the `snapshot: {agents, panes, tabs, workspaces}` shape
// the stub client returns.
func snapshotPayload(agents ...map[string]any) map[string]any {
	list := make([]any, 0, len(agents))
	for _, agent := range agents {
		list = append(list, agent)
	}
	panes := []any{}
	if len(agents) > 0 {
		panes = []any{map[string]any{
			"pane_id":  "wJ:p2",
			"revision": 1,
		}}
	}
	return map[string]any{
		"agents":     list,
		"panes":      panes,
		"tabs":       []any{},
		"workspaces": []any{},
	}
}

// historyStub mirrors historyStub(): the fixed resolution whose last assistant
// message carries a non-empty body, so the non-Pi readiness gate releases the
// round instead of waiting for history to advance.
func historyStub() observability.AgentHistoryServiceLike {
	source := "pi-jsonl"
	return newFakeHistoryService(observability.AgentHistoryResolved{
		CompactHistory: observability.CompactAgentHistory{
			LastAssistantMessage: &observability.AgentHistoryExcerpt{
				Ref:  "x",
				Text: "result",
			},
			Source: &source,
		},
	})
}

// appendDedupEvent mirrors the TS `h.agentEvents.append({...})` call sites.
func (h *dedupHarness) appendDedupEvent(
	t *testing.T,
	input db.AppendAgentEventInput,
) observability.AgentEventRecord {
	t.Helper()
	record, err := h.agentEvents.Append(input)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return record
}

// replaceDedupAgents mirrors the TS `h.agents.replaceForSession({...})` call
// sites, which return the indexed records.
func (h *dedupHarness) replaceDedupAgents(
	t *testing.T,
	agents ...map[string]any,
) []observability.AgentIndexRecord {
	t.Helper()
	records, err := h.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents:           agents,
		HerdrSessionName: dedupTestSession,
	})
	if err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	return records
}

// firstAgent mirrors the TS `[0]` plus its "Expected agent" guard.
func firstAgent(t *testing.T, records []observability.AgentIndexRecord) observability.AgentIndexRecord {
	t.Helper()
	if len(records) == 0 {
		t.Fatalf("Expected agent")
	}
	return records[0]
}

// listDedupEvents mirrors `h.agentEvents.listAfter(scope)`.
func (h *dedupHarness) listDedupEvents(t *testing.T) []observability.AgentEventRecord {
	t.Helper()
	events, err := h.agentEvents.ListAfter(db.ListAfterInput{
		HerdrSessionName: &dedupTestSessionName,
		WorkspaceID:      &dedupScope.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("listAfter: %v", err)
	}
	return events
}

func countEventType(events []observability.AgentEventRecord, eventType observability.AgentEventType) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func filterEventType(events []observability.AgentEventRecord, eventType observability.AgentEventType) []observability.AgentEventRecord {
	filtered := make([]observability.AgentEventRecord, 0, len(events))
	for _, event := range events {
		if event.Type == eventType {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func dedupAgentInput(t *testing.T, event map[string]any) observability.HandleHerdrEventInput {
	t.Helper()
	return observability.HandleHerdrEventInput{
		Event:            event,
		HerdrSessionName: dedupTestSession,
		SessionDir:       dedupSession.SessionDir,
		SocketPath:       dedupSession.SocketPath,
	}
}

// TestEventDedupPaneGeneration ports the "event deduplication and pane
// generations" describe block.
func TestEventDedupPaneGeneration(t *testing.T) {
	t.Run("does not persist the same Herdr input event twice", func(t *testing.T) {
		h := openDedupHarness(t)
		client := &snapshotClient{snapshot: snapshotPayload(agentSnapshot("claude", "gen-1"))}
		index := h.indexServiceWithClient(t, historyStub(), snapshotFactory(client))
		if _, err := index.RefreshHerdrSession(dedupSession); err != nil {
			t.Fatalf("refreshHerdrSession: %v", err)
		}
		input := dedupAgentInput(t, map[string]any{
			"agent_status": "done",
			"event_id":     "herdr-42",
			"pane_id":      "wJ:p2",
			"type":         "pane.agent_status_changed",
		})
		if _, err := index.HandleHerdrEvent(input); err != nil {
			t.Fatalf("handleHerdrEvent: %v", err)
		}
		if _, err := index.HandleHerdrEvent(input); err != nil {
			t.Fatalf("handleHerdrEvent (repeat): %v", err)
		}
		if got := countEventType(h.listDedupEvents(t), observability.AgentEventDone); got != 1 {
			t.Fatalf("agent.done count = %d, want 1", got)
		}
	})

	t.Run("invalidates closed-pane events and isolates a new generation identity", func(t *testing.T) {
		h := openDedupHarness(t)
		old := firstAgent(t, h.replaceDedupAgents(t, agentSnapshot("old", "gen-1")))
		event := h.appendDedupEvent(t, db.AppendAgentEventInput{
			AgentID:          &old.ID,
			HerdrSessionName: dedupTestSession,
			PaneID:           stringPtr("wJ:p2"),
			PaneGeneration:   stringPtr("gen-1"),
			Payload:          map[string]any{},
			TerminalID:       stringPtr("term-old"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      &dedupScope.WorkspaceID,
		})
		if err := h.agentEvents.InvalidatePane(db.InvalidatePaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-1"),
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}
		ownerTerminal := "term-owner"
		next, err := h.agentEvents.NextDeliverableAfter(observability.NextDeliverableAfterInput{
			AfterEventID:     0,
			HerdrSessionName: dedupTestSession,
			OwnerTerminalID:  ownerTerminal,
			WorkspaceID:      dedupScope.WorkspaceID,
			GetAgent:         func(string) *observability.AgentIndexRecord { return &old },
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter: %v", err)
		}
		if next != nil {
			t.Fatalf("nextDeliverableAfter = %+v, want none", *next)
		}
		fresh := firstAgent(t, h.replaceDedupAgents(t, agentSnapshot("new", "gen-2")))
		if fresh.ID == old.ID {
			t.Fatalf("fresh agent id = %s, want a different id", fresh.ID)
		}
		stored, err := h.agentEvents.Get(event.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if stored.PaneGeneration == nil || *stored.PaneGeneration != "gen-1" {
			t.Fatalf("event paneGeneration = %v, want gen-1", stored.PaneGeneration)
		}
	})

	t.Run("retires only the closed pane generation when a pane id is reused", func(t *testing.T) {
		h := openDedupHarness(t)
		index := h.indexService(t, historyStub())
		old := firstAgent(t, h.replaceDedupAgents(t, agentSnapshot("old", "gen-1")))
		oldEvent := h.appendDedupEvent(t, db.AppendAgentEventInput{
			AgentID:          &old.ID,
			HerdrSessionName: dedupTestSession,
			PaneID:           stringPtr(old.PaneID),
			PaneGeneration:   old.PaneGeneration,
			Payload:          map[string]any{},
			TerminalID:       old.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &dedupScope.WorkspaceID,
		})

		if _, err := index.HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id":         old.PaneID,
			"pane_generation": derefOr(old.PaneGeneration, ""),
			"type":            "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent(pane.closed): %v", err)
		}
		fresh := firstAgent(t, h.replaceDedupAgents(t, agentSnapshot("fresh", "gen-2")))
		freshEvent := h.appendDedupEvent(t, db.AppendAgentEventInput{
			AgentID:          &fresh.ID,
			HerdrSessionName: dedupTestSession,
			PaneID:           stringPtr(fresh.PaneID),
			PaneGeneration:   fresh.PaneGeneration,
			Payload:          map[string]any{},
			TerminalID:       fresh.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &dedupScope.WorkspaceID,
		})

		if _, err := index.HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id":         fresh.PaneID,
			"pane_generation": "gen-1",
			"type":            "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent(pane.closed, gen-1): %v", err)
		}
		retired, err := h.agents.FindByPane(db.FindAgentByPaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-1"),
		})
		if err != nil {
			t.Fatalf("findByPane(gen-1): %v", err)
		}
		if retired != nil {
			t.Fatalf("findByPane(gen-1) = %+v, want none", *retired)
		}
		survivor, err := h.agents.FindByPane(db.FindAgentByPaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-2"),
		})
		if err != nil {
			t.Fatalf("findByPane(gen-2): %v", err)
		}
		if survivor == nil {
			t.Fatalf("findByPane(gen-2) = none, want the fresh agent")
		}
		if survivor.ID != fresh.ID || derefOr(survivor.Agent, "") != "fresh" ||
			survivor.AgentStatus != observability.AgentStatusWorking {
			t.Fatalf("findByPane(gen-2) = %+v, want the fresh working agent", *survivor)
		}
		requireStatus(t, h.getEvent(t, oldEvent.ID), observability.AgentEventStatusInvalidated)
		requireReason(t, h.getEvent(t, oldEvent.ID), "PANE_CLOSED")
		requireStatus(t, h.getEvent(t, freshEvent.ID), observability.AgentEventStatusPending)
		if got := h.getEvent(t, freshEvent.ID).PaneGeneration; got == nil || *got != "gen-2" {
			t.Fatalf("fresh event paneGeneration = %v, want gen-2", got)
		}
	})

	t.Run("pane.closed retires and invalidates the matching generation", func(t *testing.T) {
		h := openDedupHarness(t)
		indexed := firstAgent(t, h.replaceDedupAgents(t, agentSnapshot("claude", "gen-7")))
		event := h.appendDedupEvent(t, db.AppendAgentEventInput{
			AgentID:          &indexed.ID,
			HerdrSessionName: dedupTestSession,
			PaneID:           stringPtr(indexed.PaneID),
			PaneGeneration:   indexed.PaneGeneration,
			Payload:          map[string]any{},
			TerminalID:       indexed.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &dedupScope.WorkspaceID,
		})
		if _, err := h.indexService(t, historyStub()).HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id":         "wJ:p2",
			"pane_generation": "gen-7",
			"type":            "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent: %v", err)
		}
		retired, err := h.agents.FindByPane(db.FindAgentByPaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-7"),
		})
		if err != nil {
			t.Fatalf("findByPane: %v", err)
		}
		if retired != nil {
			t.Fatalf("findByPane(gen-7) = %+v, want none", *retired)
		}
		requireStatus(t, h.getEvent(t, event.ID), observability.AgentEventStatusInvalidated)
		requireReason(t, h.getEvent(t, event.ID), "PANE_CLOSED")
	})

	t.Run("explicit pane.closed writes off an already delivered event instead of retaining it for redelivery", func(t *testing.T) {
		h := openDedupHarness(t)
		indexed := firstAgent(t, h.replaceDedupAgents(t, agentSnapshot("claude", "gen-7")))
		event := h.appendDedupEvent(t, db.AppendAgentEventInput{
			AgentID: &indexed.ID,
			CompactHistory: compactHistoryPtr(withAssistantMessage(
				"pi-jsonl", stringPtr("done text"))),
			HerdrSessionName: dedupTestSession,
			PaneID:           stringPtr(indexed.PaneID),
			PaneGeneration:   indexed.PaneGeneration,
			Payload:          map[string]any{"from": "working"},
			TerminalID:       indexed.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &dedupScope.WorkspaceID,
		})

		// The orchestrator already received the delivery (it read the pane out of
		// band) and never acknowledged it — the row that used to stay
		// delivered/invalidated-forever and be handed out again on every get.
		reserved, err := h.agentEvents.ReservePending("term-owner", 0, nil)
		if err != nil {
			t.Fatalf("reservePending: %v", err)
		}
		requireIDs(t, reserved, event.ID)
		delivered := h.getEvent(t, event.ID)
		requireStatus(t, delivered, observability.AgentEventStatusDelivered)
		if delivered.DeliveryAttempts == nil || *delivered.DeliveryAttempts != 1 {
			t.Fatalf("deliveryAttempts = %v, want 1", delivered.DeliveryAttempts)
		}

		if _, err := h.indexService(t, historyStub()).HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id":         "wJ:p2",
			"pane_generation": "gen-7",
			"type":            "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent: %v", err)
		}

		// Closing the tab is implicit consumption: the row is acknowledged, so it
		// can never be redelivered — neither through a pending reservation nor
		// through the after-cursor listing the extension polls.
		acked := h.getEvent(t, event.ID)
		requireStatus(t, acked, observability.AgentEventStatusAcked)
		requireDeliverable(t, acked, 0)
		again, err := h.agentEvents.ReservePending("term-owner", 0, nil)
		if err != nil {
			t.Fatalf("reservePending (after close): %v", err)
		}
		if len(again) != 0 {
			t.Fatalf("reservePending (after close) = %+v, want empty", again)
		}
		ownerTerminal := "term-owner"
		listed, err := h.agentEvents.ListAfter(db.ListAfterInput{
			AfterEventID:     int64Ptr(0),
			HerdrSessionName: &dedupTestSessionName,
			OwnerTerminalID:  &ownerTerminal,
			WorkspaceID:      &dedupScope.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("listAfter: %v", err)
		}
		if len(listed) != 0 {
			t.Fatalf("listAfter (after close) = %+v, want empty", listed)
		}
	})

	t.Run("explicit pane.closed keeps the delivery chance of an event that was never delivered", func(t *testing.T) {
		h := openDedupHarness(t)
		indexed := firstAgent(t, h.replaceDedupAgents(t, agentSnapshot("claude", "gen-7")))
		event := h.appendDedupEvent(t, db.AppendAgentEventInput{
			AgentID: &indexed.ID,
			CompactHistory: compactHistoryPtr(withAssistantMessage(
				"pi-jsonl", stringPtr("done text"))),
			HerdrSessionName: dedupTestSession,
			PaneID:           stringPtr(indexed.PaneID),
			PaneGeneration:   indexed.PaneGeneration,
			Payload:          map[string]any{"from": "working"},
			TerminalID:       indexed.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &dedupScope.WorkspaceID,
		})

		if _, err := h.indexService(t, historyStub()).HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id":         "wJ:p2",
			"pane_generation": "gen-7",
			"type":            "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent: %v", err)
		}

		// Never handed to anybody, so the pane-close retention still applies: the
		// outcome of a tab the user closed while the agent was finishing still has
		// to reach the orchestrator exactly once.
		stored := h.getEvent(t, event.ID)
		requireStatus(t, stored, observability.AgentEventStatusInvalidated)
		requireReason(t, stored, "RETAINED_OUTCOME_PANE_CLOSED")
		requireDeliverable(t, stored, 1)
	})

	t.Run("legacy pane.closed invalidates legacy events with an explicit reason", func(t *testing.T) {
		h := openDedupHarness(t)
		legacy := agentSnapshot("claude", "legacy-gen")
		delete(legacy, "pane_generation")
		indexed := firstAgent(t, h.replaceDedupAgents(t, legacy))
		event := h.appendDedupEvent(t, db.AppendAgentEventInput{
			AgentID:          &indexed.ID,
			HerdrSessionName: dedupTestSession,
			PaneID:           stringPtr(indexed.PaneID),
			PaneGeneration:   nil,
			Payload:          map[string]any{},
			TerminalID:       indexed.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &dedupScope.WorkspaceID,
		})
		if _, err := h.indexService(t, historyStub()).HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id": "wJ:p2",
			"type":    "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent: %v", err)
		}
		requireStatus(t, h.getEvent(t, event.ID), observability.AgentEventStatusInvalidated)
		requireReason(t, h.getEvent(t, event.ID), "LEGACY_CLOSE_WITHOUT_GENERATION")
	})

	t.Run("legacy pane.closed invalidates pending events from every pane generation", func(t *testing.T) {
		h := openDedupHarness(t)
		indexed := firstAgent(t, h.replaceDedupAgents(t, agentSnapshot("claude", "gen-1")))
		event := h.appendDedupEvent(t, db.AppendAgentEventInput{
			AgentID:          &indexed.ID,
			HerdrSessionName: dedupTestSession,
			PaneID:           stringPtr(indexed.PaneID),
			PaneGeneration:   stringPtr("gen-1"),
			Payload:          map[string]any{},
			TerminalID:       indexed.TerminalID,
			Type:             observability.AgentEventDone,
			WorkspaceID:      &dedupScope.WorkspaceID,
		})
		result, err := h.indexService(t, historyStub()).HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id": indexed.PaneID,
			"type":    "pane.closed",
		}))
		if err != nil {
			t.Fatalf("handleHerdrEvent: %v", err)
		}
		requireStatus(t, h.getEvent(t, event.ID), observability.AgentEventStatusInvalidated)
		requireReason(t, h.getEvent(t, event.ID), "LEGACY_CLOSE_WITHOUT_GENERATION")
		// A legacy close invalidates pending done events but never emits done events.
		if len(result.Events) != 0 {
			t.Fatalf("events = %+v, want empty", result.Events)
		}
	})

	t.Run("pane.closed tombstone: late done does not refresh or append new events", func(t *testing.T) {
		h := openDedupHarness(t)
		snapshots := 0
		client := &snapshotClient{
			calls:    &snapshots,
			snapshot: snapshotPayload(agentSnapshot("claude", "gen-1")),
		}
		index := h.indexServiceWithClient(t, historyStub(), snapshotFactory(client))
		if _, err := index.RefreshHerdrSession(dedupSession); err != nil {
			t.Fatalf("refreshHerdrSession: %v", err)
		}
		indexed, err := h.agents.FindByPane(db.FindAgentByPaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-1"),
		})
		if err != nil {
			t.Fatalf("findByPane: %v", err)
		}
		if indexed == nil {
			t.Fatalf("findByPane(gen-1) = none, want the indexed agent")
		}
		if _, err := index.HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id":         "wJ:p2",
			"pane_generation": "gen-1",
			"type":            "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent(pane.closed): %v", err)
		}
		snapshotsAfterClose := snapshots
		late, err := index.HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"agent_status":    "done",
			"pane_id":         "wJ:p2",
			"pane_generation": "gen-1",
			"type":            "pane.agent_status_changed",
		}))
		if err != nil {
			t.Fatalf("handleHerdrEvent(late done): %v", err)
		}
		if snapshots != snapshotsAfterClose {
			t.Fatalf("sessionSnapshot calls = %d, want %d", snapshots, snapshotsAfterClose)
		}
		if len(late.Events) != 0 || len(late.ContextChangedScopes) != 0 {
			t.Fatalf("late result = %+v, want empty", *late)
		}
		retired, err := h.agents.FindByPane(db.FindAgentByPaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-1"),
		})
		if err != nil {
			t.Fatalf("findByPane (after close): %v", err)
		}
		if retired != nil {
			t.Fatalf("findByPane(gen-1) = %+v, want none", *retired)
		}
		if got := countEventType(h.listDedupEvents(t), observability.AgentEventDone); got != 0 {
			t.Fatalf("agent.done count = %d, want 0", got)
		}
	})

	t.Run("pane.closed tombstone: refresh with a stale snapshot does not restore the retired agent", func(t *testing.T) {
		h := openDedupHarness(t)
		client := &snapshotClient{snapshot: snapshotPayload(agentSnapshot("claude", "gen-1"))}
		index := h.indexServiceWithClient(t, historyStub(), snapshotFactory(client))
		if _, err := index.RefreshHerdrSession(dedupSession); err != nil {
			t.Fatalf("refreshHerdrSession: %v", err)
		}
		if _, err := index.HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id":         "wJ:p2",
			"pane_generation": "gen-1",
			"type":            "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent: %v", err)
		}
		refreshed, err := index.RefreshHerdrSession(dedupSession)
		if err != nil {
			t.Fatalf("refreshHerdrSession (after close): %v", err)
		}
		if len(refreshed.Agents) != 0 {
			t.Fatalf("refreshed agents = %+v, want empty", refreshed.Agents)
		}
		retired, err := h.agents.FindByPane(db.FindAgentByPaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-1"),
		})
		if err != nil {
			t.Fatalf("findByPane: %v", err)
		}
		if retired != nil {
			t.Fatalf("findByPane(gen-1) = %+v, want none", *retired)
		}
		listed, err := h.agents.ListForHerdrSession(dedupTestSession)
		if err != nil {
			t.Fatalf("listForHerdrSession: %v", err)
		}
		if len(listed) != 0 {
			t.Fatalf("listForHerdrSession = %+v, want empty", listed)
		}
	})

	t.Run("legacy pane.closed releases an owner matching the closed pane and terminal", func(t *testing.T) {
		h := openDedupHarness(t)
		if _, err := h.scopes.Claim(db.ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: db.AgentOrchestratorScopeKey(dedupScope),
			AckedEventID:              0,
			PaneID:                    "wJ:p2",
			TerminalID:                "term-owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		h.replaceDedupAgents(t, agentSnapshot("claude", "gen-1"))
		if _, err := h.indexService(t, historyStub()).HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id":     "wJ:p2",
			"terminal_id": "term-owner",
			"type":        "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent: %v", err)
		}
		owner, err := h.scopes.Get(db.AgentOrchestratorScopeKey(dedupScope))
		if err != nil {
			t.Fatalf("scopes.get: %v", err)
		}
		if owner == nil || owner.Owner != nil {
			t.Fatalf("owner = %+v, want released (nil)", owner)
		}
	})

	t.Run("isPaneClosed: a legacy tombstone matches every pane generation", func(t *testing.T) {
		h := openDedupHarness(t)
		// Fix the semantics: markPaneClosed(paneGeneration: "gen-1") tombstones only
		// gen-1, while markPaneClosed(paneGeneration: null) (a legacy close) matches
		// any generation — including a later gen-2 — mirroring
		// AgentEventStore.invalidatePane legacy semantics.
		if err := h.agents.MarkPaneClosed(db.RetirePaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-1"),
		}); err != nil {
			t.Fatalf("markPaneClosed(gen-1): %v", err)
		}
		if err := h.agents.MarkPaneClosed(db.RetirePaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   nil,
		}); err != nil {
			t.Fatalf("markPaneClosed(legacy): %v", err)
		}
		assertPaneClosed(t, h, "gen-1", true)
		assertPaneClosed(t, h, "gen-2", true)
		// Dropping the legacy tombstone unblocks other generations while the
		// generation-scoped tombstone still closes only its own generation.
		if err := h.agents.ClearLegacyPaneTombstone(db.ClearLegacyPaneTombstoneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
		}); err != nil {
			t.Fatalf("clearLegacyPaneTombstone: %v", err)
		}
		assertPaneClosed(t, h, "gen-2", false)
		assertPaneClosed(t, h, "gen-1", true)
	})

	t.Run("legacy pane.closed tombstone is cleared when a new generation pane is indexed", func(t *testing.T) {
		h := openDedupHarness(t)
		client := &snapshotClient{snapshot: snapshotPayload(agentSnapshot("fresh", "gen-2"))}
		index := h.indexServiceWithClient(t, historyStub(), snapshotFactory(client))
		// A legacy (generation-less) close tombstones the pane for every generation.
		if _, err := index.HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id": "wJ:p2",
			"type":    "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent(pane.closed): %v", err)
		}
		assertPaneClosed(t, h, "gen-2", true)
		// Indexing a live gen-2 agent clears the legacy tombstone and restores it.
		refreshed, err := index.RefreshHerdrSession(dedupSession)
		if err != nil {
			t.Fatalf("refreshHerdrSession: %v", err)
		}
		if len(refreshed.Agents) != 1 {
			t.Fatalf("refreshed agents = %d, want 1", len(refreshed.Agents))
		}
		restored, err := h.agents.FindByPane(db.FindAgentByPaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-2"),
		})
		if err != nil {
			t.Fatalf("findByPane: %v", err)
		}
		if restored == nil {
			t.Fatalf("findByPane(gen-2) = none, want the fresh agent")
		}
		if derefOr(restored.Agent, "") != "fresh" ||
			restored.AgentStatus != observability.AgentStatusWorking {
			t.Fatalf("findByPane(gen-2) = %+v, want the fresh working agent", *restored)
		}
		assertPaneClosed(t, h, "gen-2", false)
	})

	t.Run("legacy pane.closed does not block a live generation-carrying late status", func(t *testing.T) {
		h := openDedupHarness(t)
		client := &snapshotClient{snapshot: snapshotPayload(agentSnapshot("fresh", "gen-2"))}
		index := h.indexServiceWithClient(t, historyStub(), snapshotFactory(client))
		if _, err := index.HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"pane_id": "wJ:p2",
			"type":    "pane.closed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent(pane.closed): %v", err)
		}
		late, err := index.HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"agent_status":    "done",
			"pane_id":         "wJ:p2",
			"pane_generation": "gen-2",
			"type":            "pane.agent_status_changed",
		}))
		if err != nil {
			t.Fatalf("handleHerdrEvent(late status): %v", err)
		}
		if got := countEventType(late.Events, observability.AgentEventDone); got != 1 {
			t.Fatalf("agent.done count = %d, want 1 (events=%+v)", got, late.Events)
		}
		restored, err := h.agents.FindByPane(db.FindAgentByPaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-2"),
		})
		if err != nil {
			t.Fatalf("findByPane: %v", err)
		}
		if restored == nil {
			t.Fatalf("findByPane(gen-2) = none, want the fresh agent")
		}
		if derefOr(restored.Agent, "") != "fresh" ||
			restored.AgentStatus != observability.AgentStatusDone {
			t.Fatalf("findByPane(gen-2) = %+v, want the fresh done agent", *restored)
		}
		assertPaneClosed(t, h, "gen-2", false)
	})

	t.Run("status-derived events persist the source agent pane generation", func(t *testing.T) {
		h := openDedupHarness(t)
		client := &snapshotClient{snapshot: snapshotPayload(agentSnapshot("claude", "gen-status"))}
		index := h.indexServiceWithClient(t, historyStub(), snapshotFactory(client))
		if _, err := index.RefreshHerdrSession(dedupSession); err != nil {
			t.Fatalf("refreshHerdrSession: %v", err)
		}
		if _, err := index.HandleHerdrEvent(dedupAgentInput(t, map[string]any{
			"agent_status":    "done",
			"pane_id":         "wJ:p2",
			"pane_generation": "gen-status",
			"type":            "pane.agent_status_changed",
		})); err != nil {
			t.Fatalf("handleHerdrEvent: %v", err)
		}
		generated := filterEventType(h.listDedupEvents(t), observability.AgentEventDone)
		if len(generated) == 0 {
			t.Fatalf("no agent.done events generated")
		}
		for _, event := range generated {
			if event.PaneGeneration == nil || *event.PaneGeneration != "gen-status" {
				t.Fatalf("event %d paneGeneration = %v, want gen-status", event.ID, event.PaneGeneration)
			}
		}
	})

	t.Run("advances cursor past an undeliverable old event", func(t *testing.T) {
		h := openDedupHarness(t)
		service := observability.NewAgentOrchestratorService(struct {
			AgentEvents observability.AgentEventStoreLike
			Agents      observability.AgentStoreLike
			Scopes      observability.AgentOrchestratorScopeStoreLike
		}{
			AgentEvents: h.agentEvents,
			Agents:      h.agents,
			Scopes:      h.scopes,
		})
		if _, err := service.Claim(observability.OrchestratorClaimInput{
			AgentScope: dedupScope,
			PaneID:     "owner",
			TerminalID: "term-owner",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}
		a := firstAgent(t, h.replaceDedupAgents(t, agentSnapshot("a", "gen-1")))
		event := h.appendDedupEvent(t, db.AppendAgentEventInput{
			AgentID:          &a.ID,
			HerdrSessionName: dedupTestSession,
			PaneID:           stringPtr("wJ:p2"),
			PaneGeneration:   stringPtr("gen-1"),
			Payload:          map[string]any{},
			TerminalID:       stringPtr("term-agent"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      &dedupScope.WorkspaceID,
		})
		if err := h.agentEvents.InvalidatePane(db.InvalidatePaneInput{
			HerdrSessionName: dedupTestSession,
			PaneID:           "wJ:p2",
			PaneGeneration:   stringPtr("gen-1"),
		}); err != nil {
			t.Fatalf("invalidatePane: %v", err)
		}
		_, err := service.Ack(observability.OrchestratorAckInput{
			AgentScope: dedupScope,
			EventID:    event.ID,
			TerminalID: "term-owner",
		})
		requireAckError(t, err, observability.AckEventInvalidated,
			observability.MsgAckInvalidated)
	})
}

// indexServiceWithClient mirrors the TS construction with a clientFactory stub.
func (h *dedupHarness) indexServiceWithClient(
	t *testing.T,
	history observability.AgentHistoryServiceLike,
	factory observability.HerdrClientFactory,
) *observability.AgentIndexService {
	t.Helper()
	service, err := observability.NewAgentIndexService(observability.AgentIndexServiceOptions{
		ClientFactory: factory,
		History:       history,
		Stores:        h.stores(),
	})
	if err != nil {
		t.Fatalf("newAgentIndexService: %v", err)
	}
	return service
}

func (h *dedupHarness) getEvent(t *testing.T, id int64) observability.AgentEventRecord {
	t.Helper()
	record, err := h.agentEvents.Get(id)
	if err != nil {
		t.Fatalf("get(%d): %v", id, err)
	}
	return record
}

func assertPaneClosed(t *testing.T, h *dedupHarness, generation string, want bool) {
	t.Helper()
	closed, err := h.agents.IsPaneClosed(db.IsPaneClosedInput{
		HerdrSessionName: dedupTestSession,
		PaneID:           "wJ:p2",
		PaneGeneration:   &generation,
	})
	if err != nil {
		t.Fatalf("isPaneClosed(%s): %v", generation, err)
	}
	if closed != want {
		t.Fatalf("isPaneClosed(%s) = %t, want %t", generation, closed, want)
	}
}

// derefOr renders a *string the way the TS `${agent}` template would: the empty
// string for an unset value.
func derefOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

// TestEventDedupPaginationRegressions ports the "agent event pagination
// regressions (independent coverage)" describe block.
func TestEventDedupPaginationRegressions(t *testing.T) {
	t.Run("nextDeliverableAfter crosses more than 1000 filtered noise events", func(t *testing.T) {
		h := openDedupHarness(t)
		h.upsertRunning(t, "default")
		agent := firstAgent(t, h.replaceWorkspaceAgents(t, "wB", "term-agent", "g1"))
		appendNoiseEvents(t, h, agent.ID, "wB", "wB:noise", "term-agent", 1001)
		pending := h.appendDedupEvent(t, db.AppendAgentEventInput{
			AgentID: &agent.ID,
			CompactHistory: compactHistoryPtr(withAssistantMessage(
				"antigravity-sqlite", stringPtr("done"))),
			HerdrSessionName: dedupTestSession,
			PaneID:           stringPtr("wB:p2"),
			Payload:          map[string]any{},
			TerminalID:       stringPtr("term-agent"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      stringPtr("wB"),
		})
		next, err := h.agentEvents.NextDeliverableAfter(observability.NextDeliverableAfterInput{
			AfterEventID:     0,
			HerdrSessionName: dedupTestSession,
			OwnerTerminalID:  "term-owner",
			WorkspaceID:      "wB",
			GetAgent:         func(string) *observability.AgentIndexRecord { return &agent },
		})
		if err != nil {
			t.Fatalf("nextDeliverableAfter: %v", err)
		}
		if next == nil {
			t.Fatalf("nextDeliverableAfter = none, want event %d", pending.ID)
		}
		if next.ID != pending.ID {
			t.Fatalf("nextDeliverableAfter id = %d, want %d", next.ID, pending.ID)
		}
	})

	t.Run("returns empty and warns when every event is noise", func(t *testing.T) {
		h := openDedupHarness(t)
		h.upsertRunning(t, "default")
		agent := firstAgent(t, h.replaceWorkspaceAgents(t, "wB", "term-agent", "g1"))
		appendNoiseEvents(t, h, agent.ID, "wB", "wB:noise", "term-agent", 50001)
		var next *observability.AgentEventRecord
		var scanErr error
		output := captureStderr(t, func() {
			next, scanErr = h.agentEvents.NextDeliverableAfter(
				observability.NextDeliverableAfterInput{
					AfterEventID:     0,
					HerdrSessionName: dedupTestSession,
					OwnerTerminalID:  "term-owner",
					WorkspaceID:      "wB",
					GetAgent:         func(string) *observability.AgentIndexRecord { return &agent },
				})
		})
		if scanErr != nil {
			t.Fatalf("nextDeliverableAfter: %v", scanErr)
		}
		if next != nil {
			t.Fatalf("nextDeliverableAfter = %+v, want none", *next)
		}
		// The port logs to stderr instead of `console.warn`; the "stopped scanning"
		// line is the observable contract the TS assertion checks.
		if !strings.Contains(output, "stopped scanning") {
			t.Fatalf("stderr = %q, want it to contain \"stopped scanning\"", output)
		}
	})
}

// upsertRunning mirrors the extra upsertRunning call of the pagination tests.
func (h *dedupHarness) upsertRunning(t *testing.T, name string) {
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

// replaceWorkspaceAgents mirrors the replaceForSession call of the pagination
// tests: one agent on the given workspace/pane/terminal.
func (h *dedupHarness) replaceWorkspaceAgents(
	t *testing.T,
	workspaceID string,
	terminalID string,
	generation string,
) []observability.AgentIndexRecord {
	t.Helper()
	return h.replaceDedupAgents(t, map[string]any{
		"agent":           "claude",
		"agent_status":    "working",
		"cwd":             "/repo",
		"pane_id":         workspaceID + ":p2",
		"pane_generation": generation,
		"revision":        1,
		"terminal_id":     terminalID,
		"workspace_id":    workspaceID,
	})
}

// appendNoiseEvents mirrors the `begin immediate` / loop / `commit` block the TS
// tests use to fill the scan window with undeliverable noise rows.
func appendNoiseEvents(
	t *testing.T,
	h *dedupHarness,
	agentID string,
	workspaceID string,
	paneID string,
	terminalID string,
	count int,
) {
	t.Helper()
	tx, err := h.sqlite.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for i := 0; i < count; i++ {
		if _, err := tx.Exec(
			"insert into agent_events (agent_id, created_at, herdr_session_name,"+
				" idempotency_key, pane_id, payload_json, terminal_id, type,"+
				" workspace_id) values (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			agentID,
			nowMinus(int64(i)),
			dedupTestSession,
			nil,
			paneID,
			"{}",
			terminalID,
			string(observability.AgentEventDone),
			workspaceID,
		); err != nil {
			t.Fatalf("insert noise event %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed = true
}

// guardUnused keeps the strings import honest if a future edit drops the only
// current use.
var _ = strings.TrimSpace
