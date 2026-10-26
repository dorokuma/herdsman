package observability_test

// The port of the core cases of
// test/integration/turn-completion-signal.test.ts (lines 1-470): the agent.done
// / agent.blocked turn-completion signal timing.
//
// The harness mirrors openObservabilityDbHarness: a migrated database in
// t.TempDir() plus every store the index service needs, so the production
// ~/.herdsman home and the live daemon are never touched.
//
// The TS file drives the waits with real timers and `sleep: async () => {}`;
// the Go port keeps both seams (a short TurnCompletionRegistry timeout and the
// Sleep option), so each case stays bounded in wall-clock time.

import (
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

const turnTestSession = "default"

// turnTestSessionName is the addressable form the *string inputs need.
var turnTestSessionName = turnTestSession

// turnScope is the orchestrator scope the events of this file live in.
var turnScope = observability.AgentScope{
	HerdrSessionName: turnTestSession,
	WorkspaceID:      "wJ",
}

// turnSessionInput mirrors the TS `sessionInput()` helper.
func turnSessionInput() observability.RefreshInput {
	return observability.RefreshInput{
		HerdrSessionName: "default",
		SessionDir:       "/tmp/herdr",
		SocketPath:       "/tmp/herdr.sock",
	}
}

// turnDoneEvent mirrors the TS `doneEvent` constant.
func turnDoneEvent() observability.HandleHerdrEventInput {
	return observability.HandleHerdrEventInput{
		Event: map[string]any{
			"agent_status": "done",
			"pane_id":      "wJ:p2",
			"type":         "pane.agent_status_changed",
		},
		HerdrSessionName: "default",
		SessionDir:       "/tmp/herdr",
		SocketPath:       "/tmp/herdr.sock",
	}
}

// turnHarness mirrors openObservabilityDbHarness.
type turnHarness struct {
	sqlite      *sql.DB
	agentEvents *db.AgentEventStore
	agents      *db.AgentStore
	scopes      *db.AgentOrchestratorScopeStore
}

func openTurnHarness(t *testing.T) *turnHarness {
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
		Name:       turnTestSession,
		SessionDir: "/tmp/herdr",
		SocketPath: "/tmp/herdr.sock",
	}); err != nil {
		t.Fatalf("upsertRunning: %v", err)
	}
	agentEvents, err := db.NewAgentEventStore(sqlite)
	if err != nil {
		t.Fatalf("newAgentEventStore: %v", err)
	}
	return &turnHarness{
		sqlite:      sqlite,
		agentEvents: agentEvents,
		agents:      db.NewAgentStore(sqlite, agentEvents),
		scopes:      db.NewAgentOrchestratorScopeStore(sqlite),
	}
}

// stores mirrors the TS harness object the service is constructed with.
func (h *turnHarness) stores() observability.AgentIndexServiceStores {
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

// listTurnEvents mirrors `h.agentEvents.listAfter({herdrSessionName, workspaceId})`.
func (h *turnHarness) listTurnEvents(t *testing.T) []observability.AgentEventRecord {
	t.Helper()
	events, err := h.agentEvents.ListAfter(db.ListAfterInput{
		HerdrSessionName: &turnTestSessionName,
		WorkspaceID:      &turnScope.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("listAfter: %v", err)
	}
	return events
}

// selectEventRows runs the TS `select * from agent_events where ...` probes.
func (h *turnHarness) selectEventRows(t *testing.T, status string, eventType string) []map[string]any {
	t.Helper()
	rows, err := h.sqlite.Query(
		"select id, type, status, payload_json, deliverable from agent_events where status = ? and type = ?",
		status, eventType,
	)
	if err != nil {
		t.Fatalf("query agent_events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	found := []map[string]any{}
	for rows.Next() {
		var (
			id          int64
			kind        string
			rowStatus   string
			payload     string
			deliverable int
		)
		if err := rows.Scan(&id, &kind, &rowStatus, &payload, &deliverable); err != nil {
			t.Fatalf("scan agent_events: %v", err)
		}
		found = append(found, map[string]any{
			"id": id, "type": kind, "status": rowStatus, "payload": payload, "deliverable": deliverable,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate agent_events: %v", err)
	}
	return found
}

// piAgentSnapshot mirrors the TS `piAgentSnapshot(status, agent)` helper.
func piAgentSnapshot(agentKind string, status string) map[string]any {
	return map[string]any{
		"agents": []any{map[string]any{
			"agent":        agentKind,
			"agent_status": status,
			"cwd":          "/repo",
			"pane_id":      "wJ:p2",
			"revision":     float64(10),
			"terminal_id":  "term_claude",
			"workspace_id": "wJ",
		}},
		"panes": []any{map[string]any{"pane_id": "wJ:p2", "revision": float64(10)}},
		"tabs":  []any{},
		"workspaces": []any{map[string]any{
			"agent_status": status,
			"focused":      true,
			"label":        "repo",
			"workspace_id": "wJ",
		}},
	}
}

// turnSnapshotClient is the TS `clientFactory` stub with a mutable snapshot.
type turnSnapshotClient struct {
	mu       sync.Mutex
	snapshot func() map[string]any
}

func (c *turnSnapshotClient) Close() {}

func (c *turnSnapshotClient) SessionSnapshot() (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"snapshot": c.snapshot()}, nil
}

// scriptedHistory mirrors the TS `history: { resolveCompactHistory() {...} }`
// fake: a call counter plus a per-call resolution function. The remaining
// agent-history methods come from the shared fake (they forward to the real
// agent-history module, like the TS service calling the module directly).
type scriptedHistory struct {
	*fakeHistoryService
	mu      sync.Mutex
	calls   int
	resolve func(call int) observability.AgentHistoryResolved
}

func (h *scriptedHistory) ResolveCompactHistory(
	input observability.AgentHistoryLookupInput,
	options observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	h.mu.Lock()
	h.calls++
	call := h.calls
	resolve := h.resolve
	h.mu.Unlock()
	return resolve(call)
}

func (h *scriptedHistory) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *scriptedHistory) reset(resolve func(call int) observability.AgentHistoryResolved) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = 0
	h.resolve = resolve
}

// emptyHistoryResolution mirrors
// `{ compactHistory: { ...emptyCompactHistory(source), lastAssistantMessage: null }, ... }`.
func emptyHistoryResolution(source string) observability.AgentHistoryResolved {
	value := source
	return observability.AgentHistoryResolved{
		CompactHistory: observability.CompactAgentHistory{Source: &value},
	}
}

// assistantHistoryResolution mirrors a resolution whose last assistant message
// carries `text` (ref "history", stopReason optional).
func assistantHistoryResolution(source string, text string, stopReason *string) observability.AgentHistoryResolved {
	value := source
	return observability.AgentHistoryResolved{
		CompactHistory: observability.CompactAgentHistory{
			LastAssistantMessage: &observability.AgentHistoryExcerpt{
				Ref:        "history",
				StopReason: stopReason,
				Text:       text,
				Timestamp:  nil,
			},
			Source: &value,
		},
	}
}

// turnSignal mirrors the TS `registry.record({...})` payload.
func turnSignal(confirmed bool) observability.SessionScopedSignal {
	return observability.SessionScopedSignal{
		Confirmed:        confirmed,
		HerdrSessionName: "default",
		PaneID:           "wJ:p2",
		TerminalID:       "term_claude",
		WorkspaceID:      "wJ",
	}
}

// newTurnService wires the index service the way the TS cases do.
func newTurnService(
	t *testing.T,
	harness *turnHarness,
	history *scriptedHistory,
	registry *observability.TurnCompletionRegistry,
	options *observability.AgentIndexServiceOptions,
) *observability.AgentIndexService {
	t.Helper()
	serviceOptions := observability.AgentIndexServiceOptions{
		ClientFactory: func(string) (observability.HerdrClientLike, error) {
			return &turnSnapshotClient{snapshot: func() map[string]any {
				return piAgentSnapshot("pi", "working")
			}}, nil
		},
		History:         history,
		Stores:          harness.stores(),
		TurnCompletions: registry,
	}
	if options != nil {
		if options.Sleep != nil {
			serviceOptions.Sleep = options.Sleep
		}
		if options.ClientFactory != nil {
			serviceOptions.ClientFactory = options.ClientFactory
		}
	}
	service, err := observability.NewAgentIndexService(serviceOptions)
	if err != nil {
		t.Fatalf("newAgentIndexService: %v", err)
	}
	return service
}

// firstAssistantText projects `events` onto the assistant text of the first
// agent.done record, mirroring the toContainEqual assertion.
func firstAssistantText(t *testing.T, events []observability.AgentEventRecord) (string, bool) {
	t.Helper()
	for _, event := range events {
		if event.Type != observability.AgentEventDone {
			continue
		}
		if event.CompactHistory == nil || event.CompactHistory.LastAssistantMessage == nil {
			t.Fatalf("agent.done event %d carries no lastAssistantMessage", event.ID)
		}
		return event.CompactHistory.LastAssistantMessage.Text, true
	}
	return "", false
}

// countEventsOfType mirrors `events.filter((e) => e.type === type)`.
func countEventsOfType(events []observability.AgentEventRecord, eventType observability.AgentEventType) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

// refreshGate is the Go stand-in for the TS `armGate()` helper: a one-shot
// signal that can be re-armed before the turn under test starts, so the initial
// refresh never counts as the status-observation refresh.
type refreshGate struct {
	mu    sync.Mutex
	ch    chan struct{}
	fired bool
}

func (g *refreshGate) rearm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ch = make(chan struct{})
	g.fired = false
}

func (g *refreshGate) fire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fired || g.ch == nil {
		return
	}
	g.fired = true
	close(g.ch)
}

func (g *refreshGate) wait() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ch
}

// TestTurnSignalWaitsForPiFinalMessage ports
// "waits for the pi turn signal before emitting agent.done so
// lastAssistantMessage is non-empty".
func TestTurnSignalWaitsForPiFinalMessage(t *testing.T) {
	harness := openTurnHarness(t)
	registry := observability.NewTurnCompletionRegistry(observability.WithTurnSettimeout(3_000))
	history := &scriptedHistory{}
	var signalRecorded atomic.Bool
	var gate refreshGate
	gate.rearm()
	armHistory := func() {
		gate.rearm()
		history.reset(func(call int) observability.AgentHistoryResolved {
			if call == 1 {
				// The status-observation refresh runs before the final message is
				// on disk, so it always sees an empty history (the race under test).
				gate.fire()
				return emptyHistoryResolution("pi-jsonl")
			}
			if signalRecorded.Load() {
				return assistantHistoryResolution("pi-jsonl", "final answer", stringPtr("stop"))
			}
			return emptyHistoryResolution("pi-jsonl")
		})
	}
	armHistory()

	service := newTurnService(t, harness, history, registry, nil)
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	// Re-arm so the initial refresh does not count as the status-observation
	// refresh for the turn under test.
	armHistory()

	type handled struct {
		events []observability.AgentEventRecord
		err    error
	}
	pending := make(chan handled, 1)
	go func() {
		result, err := service.HandleHerdrEvent(turnDoneEvent())
		events := []observability.AgentEventRecord{}
		if result != nil {
			events = result.Events
		}
		pending <- handled{events: events, err: err}
	}()

	<-gate.wait()
	if history.callCount() < 1 {
		t.Fatalf("history calls = %d, want the status-observation refresh to have run", history.callCount())
	}

	// The extension writes its final message and signals the daemon after the
	// turn-completion waiter has been installed.
	signalRecorded.Store(true)
	go func() {
		time.Sleep(10 * time.Millisecond)
		registry.Record(turnSignal(true))
	}()

	select {
	case outcome := <-pending:
		if outcome.err != nil {
			t.Fatalf("handleHerdrEvent: %v", outcome.err)
		}
		if calls := history.callCount(); calls < 2 {
			t.Fatalf("history calls = %d, want a re-refresh after the turn signal", calls)
		}
		text, ok := firstAssistantText(t, outcome.events)
		if !ok {
			t.Fatalf("events = %+v, want an agent.done record", outcome.events)
		}
		if text != "final answer" {
			t.Fatalf("agent.done lastAssistantMessage = %q, want %q", text, "final answer")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("handleHerdrEvent did not return")
	}
}

// TestTurnSignalAgyRetriesWithoutSignal ports "retries for agy without waiting
// for a turn signal".
func TestTurnSignalAgyRetriesWithoutSignal(t *testing.T) {
	harness := openTurnHarness(t)
	registry := observability.NewTurnCompletionRegistry(observability.WithTurnSettimeout(3_000))
	history := &scriptedHistory{}
	history.resolve = func(call int) observability.AgentHistoryResolved {
		if call < 3 {
			return emptyHistoryResolution("antigravity-sqlite")
		}
		return assistantHistoryResolution("antigravity-sqlite", "agy final answer", nil)
	}

	service := newTurnService(t, harness, history, registry, &observability.AgentIndexServiceOptions{
		ClientFactory: func(string) (observability.HerdrClientLike, error) {
			return &turnSnapshotClient{snapshot: func() map[string]any {
				return piAgentSnapshot("agy", "working")
			}}, nil
		},
	})
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	history.reset(history.resolve)

	// A non-terminal agent (agy) fails the terminal check immediately, so the
	// drain emits the done event without waiting for a turn signal.
	startedAt := time.Now()
	result, err := service.HandleHerdrEvent(turnDoneEvent())
	if err != nil {
		t.Fatalf("handleHerdrEvent: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 9*time.Second {
		t.Fatalf("handleHerdrEvent took %s, want no turn-signal wait", elapsed)
	}
	if calls := history.callCount(); calls < 3 {
		t.Fatalf("history calls = %d, want the retry loop to reach the 3rd resolution", calls)
	}
	text, ok := firstAssistantText(t, result.Events)
	if !ok {
		t.Fatalf("events = %+v, want an agent.done record", result.Events)
	}
	if text != "agy final answer" {
		t.Fatalf("agent.done lastAssistantMessage = %q, want %q", text, "agy final answer")
	}
}

// TestTurnSignalRecordedFirstEmitsPromptly ports
// "emits agent.done promptly when the turn signal was recorded first".
func TestTurnSignalRecordedFirstEmitsPromptly(t *testing.T) {
	harness := openTurnHarness(t)
	registry := observability.NewTurnCompletionRegistry(observability.WithTurnSettimeout(50))
	history := &scriptedHistory{}
	history.resolve = func(int) observability.AgentHistoryResolved {
		return assistantHistoryResolution("pi-jsonl", "final answer", stringPtr("stop"))
	}

	service := newTurnService(t, harness, history, registry, nil)
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	registry.Record(turnSignal(true))

	startedAt := time.Now()
	result, err := service.HandleHerdrEvent(turnDoneEvent())
	if err != nil {
		t.Fatalf("handleHerdrEvent: %v", err)
	}
	// The recorded signal is consumed immediately, so the wait never runs the
	// 50 ms timeout plus its retry sleeps.
	if elapsed := time.Since(startedAt); elapsed > 150*time.Millisecond {
		t.Fatalf("handleHerdrEvent took %s, want the recorded signal to be consumed at once", elapsed)
	}
	text, ok := firstAssistantText(t, result.Events)
	if !ok {
		t.Fatalf("events = %+v, want an agent.done record", result.Events)
	}
	if text != "final answer" {
		t.Fatalf("agent.done lastAssistantMessage = %q, want %q", text, "final answer")
	}
}

// TestTurnSignalConfirmedEmptyHistoryIsDelivered ports
// "delivers a confirmed empty-history turn without invalidating it as degraded".
func TestTurnSignalConfirmedEmptyHistoryIsDelivered(t *testing.T) {
	harness := openTurnHarness(t)
	registry := observability.NewTurnCompletionRegistry(observability.WithTurnSettimeout(3_000))
	history := &scriptedHistory{}
	history.resolve = func(int) observability.AgentHistoryResolved {
		return emptyHistoryResolution("pi-jsonl")
	}

	service := newTurnService(t, harness, history, registry, &observability.AgentIndexServiceOptions{
		Sleep: func(int64, <-chan struct{}) {},
	})
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}

	pending := make(chan *observability.AgentEventHandlingResult, 1)
	go func() {
		result, err := service.HandleHerdrEvent(turnDoneEvent())
		if err != nil {
			t.Errorf("handleHerdrEvent: %v", err)
		}
		pending <- result
	}()
	go func() {
		time.Sleep(10 * time.Millisecond)
		registry.Record(turnSignal(true))
	}()

	var result *observability.AgentEventHandlingResult
	select {
	case result = <-pending:
	case <-time.After(20 * time.Second):
		t.Fatal("handleHerdrEvent did not return")
	}
	// A client-confirmed turn is a deliverable terminal state: it stays
	// deliverable even when the refreshed snapshot still carries no assistant
	// excerpt, and it is never invalidated as a degraded retry.
	if got := countEventsOfType(result.Events, observability.AgentEventDone); got != 1 {
		t.Fatalf("handled agent.done count = %d, want 1", got)
	}
	stored := harness.listTurnEvents(t)
	if got := countEventsOfType(stored, observability.AgentEventDone); got != 1 {
		t.Fatalf("stored agent.done count = %d, want 1", got)
	}
	var delivered *observability.AgentEventRecord
	for index := range stored {
		if stored[index].Type == observability.AgentEventDone {
			delivered = &stored[index]
			break
		}
	}
	if delivered == nil {
		t.Fatal("no stored agent.done record")
	}
	if delivered.Deliverable != 1 {
		t.Fatalf("deliverable = %d, want 1", delivered.Deliverable)
	}
	if delivered.Status == nil || *delivered.Status != observability.AgentEventStatusPending {
		t.Fatalf("status = %v, want pending", delivered.Status)
	}
	payload, _ := delivered.Payload.(map[string]any)
	if degraded, ok := payload["degraded"]; ok && degraded == true {
		t.Fatalf("payload = %+v, want no degraded flag", delivered.Payload)
	}
	if rows := harness.selectEventRows(t, "invalidated", "agent.done"); len(rows) != 0 {
		t.Fatalf("invalidated agent.done rows = %d, want 0", len(rows))
	}
}

// TestTurnSignalTimeoutInvalidatesEmptyHistory ports
// "generates agent.done as-is with a warning when no turn signal arrives (old
// extension)": the empty assistant history is guarded, so nothing is delivered
// and the row is invalidated instead.
func TestTurnSignalTimeoutInvalidatesEmptyHistory(t *testing.T) {
	harness := openTurnHarness(t)
	registry := observability.NewTurnCompletionRegistry(observability.WithTurnSettimeout(20))
	history := &scriptedHistory{}
	history.resolve = func(int) observability.AgentHistoryResolved {
		return emptyHistoryResolution("pi-jsonl")
	}

	service := newTurnService(t, harness, history, registry, &observability.AgentIndexServiceOptions{
		Sleep: func(int64, <-chan struct{}) {},
	})
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}

	result, err := service.HandleHerdrEvent(turnDoneEvent())
	if err != nil {
		t.Fatalf("handleHerdrEvent: %v", err)
	}
	if got := countEventsOfType(result.Events, observability.AgentEventDone); got != 0 {
		t.Fatalf("handled agent.done count = %d, want 0 (the guard blocks emission)", got)
	}
	if got := countEventsOfType(harness.listTurnEvents(t), observability.AgentEventDone); got != 0 {
		t.Fatalf("stored agent.done count = %d, want 0", got)
	}
	if rows := harness.selectEventRows(t, "invalidated", "agent.done"); len(rows) < 1 {
		t.Fatalf("invalidated agent.done rows = %d, want at least 1 (degraded record)", len(rows))
	}
}

// TestTurnSignalAbsentRegistryDoesNotWait ports
// "does not wait for a turn signal when no registry is configured".
func TestTurnSignalAbsentRegistryDoesNotWait(t *testing.T) {
	harness := openTurnHarness(t)
	history := &scriptedHistory{}
	history.resolve = func(int) observability.AgentHistoryResolved {
		return emptyHistoryResolution("pi-jsonl")
	}

	service := newTurnService(t, harness, history, nil, nil)
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	history.reset(history.resolve)

	result, err := service.HandleHerdrEvent(turnDoneEvent())
	if err != nil {
		t.Fatalf("handleHerdrEvent: %v", err)
	}
	if got := history.callCount(); got != 1 {
		t.Fatalf("history calls = %d, want 1", got)
	}
	if got := countEventsOfType(result.Events, observability.AgentEventDone); got != 1 {
		t.Fatalf("handled agent.done count = %d, want 1", got)
	}
}

// TestTurnSignalAbortedByPaneClosed ports
// "aborts turn completion wait immediately on pane.closed and suppresses
// agent.done".
func TestTurnSignalAbortedByPaneClosed(t *testing.T) {
	harness := openTurnHarness(t)
	registry := observability.NewTurnCompletionRegistry(observability.WithTurnSettimeout(5_000))
	history := &scriptedHistory{}
	history.resolve = func(int) observability.AgentHistoryResolved {
		return emptyHistoryResolution("pi-jsonl")
	}

	service := newTurnService(t, harness, history, registry, nil)
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}

	pending := make(chan *observability.AgentEventHandlingResult, 1)
	go func() {
		result, err := service.HandleHerdrEvent(turnDoneEvent())
		if err != nil {
			t.Errorf("handleHerdrEvent: %v", err)
		}
		pending <- result
	}()

	// The pane closes before any turn-completion signal arrives.
	startedAt := time.Now()
	closeResult, err := service.HandleHerdrEvent(observability.HandleHerdrEventInput{
		Event: map[string]any{
			"pane_id": "wJ:p2",
			"type":    "pane.closed",
		},
		HerdrSessionName: "default",
		SessionDir:       "/tmp/herdr",
		SocketPath:       "/tmp/herdr.sock",
	})
	if err != nil {
		t.Fatalf("handleHerdrEvent(pane.closed): %v", err)
	}
	wantScopes := []observability.AgentScope{{HerdrSessionName: "default", WorkspaceID: "wJ"}}
	if len(closeResult.ContextChangedScopes) != len(wantScopes) ||
		closeResult.ContextChangedScopes[0] != wantScopes[0] {
		t.Fatalf("contextChangedScopes = %+v, want %+v", closeResult.ContextChangedScopes, wantScopes)
	}

	var doneResult *observability.AgentEventHandlingResult
	select {
	case doneResult = <-pending:
	case <-time.After(20 * time.Second):
		t.Fatal("the pending agent.done wait was not aborted by pane.closed")
	}
	if elapsed := time.Since(startedAt); elapsed > 5*time.Second {
		t.Fatalf("the aborted wait took %s, want it to stop promptly", elapsed)
	}
	if len(doneResult.Events) != 0 {
		t.Fatalf("aborted done events = %+v, want none", doneResult.Events)
	}
	if got := countEventsOfType(harness.listTurnEvents(t), observability.AgentEventDone); got != 0 {
		t.Fatalf("stored agent.done count = %d, want 0", got)
	}
}
