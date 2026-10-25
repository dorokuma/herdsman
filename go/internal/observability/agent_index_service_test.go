package observability_test

// The port of the core cases of test/integration/agent-index-service.test.ts
// (lines 1-470): incremental revision/cwd/terminal refresh, pane-move scope
// notifications, name-only publishing, status-transition event generation and
// the pane.closed tombstone cleanup.
//
// The harness mirrors openObservabilityDbHarness (a migrated database in
// t.TempDir() plus every store the index service needs), so the production
// ~/.herdsman home and the live daemon are never touched.

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// idxSnapshotHolder is the mutable `let current = ...` of the TS cases.
type idxSnapshotHolder struct {
	mu       sync.Mutex
	snapshot map[string]any
}

func (h *idxSnapshotHolder) set(snapshot map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snapshot = snapshot
}

func (h *idxSnapshotHolder) get() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot
}

// idxClient is the TS `clientFactory` stub; it can also fail like a timed-out
// `session.snapshot` request.
type idxClient struct {
	holder *idxSnapshotHolder
	mu     sync.Mutex
	fail   bool
}

func (c *idxClient) Close() {}

func (c *idxClient) SessionSnapshot() (any, error) {
	c.mu.Lock()
	fail := c.fail
	c.mu.Unlock()
	if fail {
		return nil, errSnapshotTimeout
	}
	return c.holder.get(), nil
}

var errSnapshotTimeout = &snapshotTimeoutError{}

type snapshotTimeoutError struct{}

func (e *snapshotTimeoutError) Error() string {
	return "Herdr session.snapshot request timed out after 2000ms"
}

func (c *idxClient) setFail(fail bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = fail
}

// idxHistory mirrors the TS `history(onResolve, assistantText)` fake: it records
// the agent of every resolution and answers with a fixed assistant excerpt.
type idxHistory struct {
	*fakeHistoryService
	mu              sync.Mutex
	calls           []string
	assistant       string
	resolveCount    int
	onCall          func(call int)
	resolveOverride func(call int) observability.AgentHistoryResolved
}

func newIdxHistory(assistant string) *idxHistory {
	return &idxHistory{assistant: assistant}
}

func (h *idxHistory) resetCalls() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = nil
	h.resolveCount = 0
}

func (h *idxHistory) recordedCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.calls...)
}

func (h *idxHistory) ResolveCompactHistory(
	input observability.AgentHistoryLookupInput,
	options observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	agentKind := "unknown"
	if input.Agent != nil {
		agentKind = *input.Agent
	}
	h.mu.Lock()
	h.calls = append(h.calls, agentKind)
	h.resolveCount++
	call := h.resolveCount
	onCall := h.onCall
	assistant := h.assistant
	h.mu.Unlock()
	if onCall != nil {
		onCall(call)
	}
	if resolveOverride := func() func(int) observability.AgentHistoryResolved {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.resolveOverride
	}(); resolveOverride != nil {
		return resolveOverride(call)
	}

	path := "/tmp/herdr-role-sessions/default/" + agentKind + "-history.jsonl"
	if options.PreferredRef != nil {
		if options.PreferredRef.Path != nil {
			path = *options.PreferredRef.Path
		} else if options.PreferredRef.Value != "" {
			path = options.PreferredRef.Value
		}
	}
	source := observability.HistorySourcePiJSONL
	historyRef := observability.AgentHistoryRef{
		Kind:   observability.HistoryRefAgentSession,
		Path:   &path,
		Source: source,
		Value:  path,
	}
	value := string(source)
	return observability.AgentHistoryResolved{
		CompactHistory: observability.CompactAgentHistory{
			HistoryRef: &historyRef,
			LastAssistantMessage: &observability.AgentHistoryExcerpt{
				Ref:       "history",
				Text:      assistant,
				Timestamp: nil,
			},
			Source: &value,
		},
		HistoryRef:        &historyRef,
		SourceFingerprint: &observability.AgentHistorySourceFingerprint{MtimeMs: 1, Path: path, Size: 1},
	}
}

// idxHarness mirrors openObservabilityDbHarness for this file.
type idxHarness struct {
	sqlite      *sql.DB
	agentEvents *db.AgentEventStore
	agents      *db.AgentStore
	scopes      *db.AgentOrchestratorScopeStore
	sessions    *db.HerdrSessionStore
}

func openIdxHarness(t *testing.T) *idxHarness {
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
	return &idxHarness{
		sqlite:      sqlite,
		agentEvents: agentEvents,
		agents:      db.NewAgentStore(sqlite, agentEvents),
		scopes:      db.NewAgentOrchestratorScopeStore(sqlite),
		sessions:    sessions,
	}
}

func (h *idxHarness) stores() observability.AgentIndexServiceStores {
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

// listEvents mirrors `h.agentEvents.listAfter({herdrSessionName, workspaceId})`.
func (h *idxHarness) listEvents(t *testing.T, workspaceID string) []observability.AgentEventRecord {
	t.Helper()
	events, err := h.agentEvents.ListAfter(db.ListAfterInput{
		HerdrSessionName: idxStrPtr("default"),
		WorkspaceID:      &workspaceID,
	})
	if err != nil {
		t.Fatalf("listAfter: %v", err)
	}
	return events
}

// idxStatusEvents mirrors the TS `doneEvent` / status-event inputs.
func idxStatusEvent(paneID string, status string, generation *string) observability.HandleHerdrEventInput {
	event := map[string]any{
		"agent_status": status,
		"pane_id":      paneID,
		"type":         "pane.agent_status_changed",
	}
	if generation != nil {
		event["pane_generation"] = *generation
	}
	return observability.HandleHerdrEventInput{
		Event:            event,
		HerdrSessionName: "default",
		SessionDir:       "/tmp/herdr",
		SocketPath:       "/tmp/herdr.sock",
	}
}

// idxAgent mirrors the TS `agent(input)` helper with its defaults.
func idxAgent(overrides map[string]any) map[string]any {
	base := map[string]any{
		"agent":          "claude",
		"agent_status":   "working",
		"cwd":            "/repo",
		"foreground_cwd": "/repo",
		"tab_id":         "wJ:t1",
	}
	for key, value := range overrides {
		base[key] = value
	}
	return base
}

// idxSnapshot mirrors the TS `snapshot(agents, panes)` helper.
func idxSnapshot(agents []any, panes []any) map[string]any {
	return map[string]any{
		"snapshot": map[string]any{
			"agents": agents,
			"panes":  panes,
			"tabs":   []any{},
			"workspaces": []any{
				map[string]any{"agent_status": "working", "focused": true, "label": "repo", "workspace_id": "wJ"},
				map[string]any{"agent_status": "working", "focused": false, "label": "other", "workspace_id": "wK"},
			},
		},
	}
}

// idxOneAgent mirrors the TS `oneAgent(status, revision, agentKind, name)`.
// Revisions are float64 because the service reads numbers the way JSON decodes
// them (see integerValue).
func idxOneAgent(status string, revision int, agentKind string, name any) map[string]any {
	if agentKind == "" {
		agentKind = "claude"
	}
	return idxSnapshot([]any{idxAgent(map[string]any{
		"agent":        agentKind,
		"agent_status": status,
		"name":         name,
		"pane_id":      "wJ:p2",
		"revision":     float64(revision),
		"terminal_id":  "term_claude",
		"workspace_id": "wJ",
	})}, []any{map[string]any{"pane_id": "wJ:p2", "revision": float64(revision)}})
}

// idxTwoAgents mirrors the TS `twoAgents(input)` helper.
type idxTwoAgentsInput struct {
	ClaudePane      *string
	ClaudeRevision  *int
	ClaudeTerminal  any // string, or nil for an explicit null terminal
	HasClaudeTerm   bool
	ClaudeWorkspace *string
	CodexCwd        *string
}

func idxTwoAgents(input idxTwoAgentsInput) map[string]any {
	claudePane := "wJ:p2"
	if input.ClaudePane != nil {
		claudePane = *input.ClaudePane
	}
	claudeRevision := 10
	if input.ClaudeRevision != nil {
		claudeRevision = *input.ClaudeRevision
	}
	claudeWorkspace := "wJ"
	if input.ClaudeWorkspace != nil {
		claudeWorkspace = *input.ClaudeWorkspace
	}
	claudeTerminal := any("term_claude")
	if input.HasClaudeTerm {
		claudeTerminal = input.ClaudeTerminal
	}
	codexCwd := "/repo"
	if input.CodexCwd != nil {
		codexCwd = *input.CodexCwd
	}
	return idxSnapshot([]any{
		idxAgent(map[string]any{
			"pane_id":      claudePane,
			"revision":     nil,
			"terminal_id":  claudeTerminal,
			"workspace_id": claudeWorkspace,
		}),
		idxAgent(map[string]any{
			"agent":        "codex",
			"cwd":          codexCwd,
			"pane_id":      "wJ:p3",
			"revision":     float64(20),
			"terminal_id":  "term_codex",
			"workspace_id": "wJ",
		}),
	}, []any{
		map[string]any{"pane_id": claudePane, "revision": float64(claudeRevision)},
		map[string]any{"pane_id": "wJ:p3", "revision": float64(20)},
	})
}

// newIdxService wires an index service over a mutable snapshot holder.
func newIdxService(
	t *testing.T,
	harness *idxHarness,
	holder *idxSnapshotHolder,
	history *idxHistory,
	options *observability.AgentIndexServiceOptions,
) (*observability.AgentIndexService, *idxClient) {
	t.Helper()
	client := &idxClient{holder: holder}
	serviceOptions := observability.AgentIndexServiceOptions{
		ClientFactory: func(string) (observability.HerdrClientLike, error) { return client, nil },
		History:       history,
		Stores:        harness.stores(),
	}
	if options != nil {
		if options.TurnCompletions != nil {
			serviceOptions.TurnCompletions = options.TurnCompletions
		}
		if options.Sleep != nil {
			serviceOptions.Sleep = options.Sleep
		}
	}
	service, err := observability.NewAgentIndexService(serviceOptions)
	if err != nil {
		t.Fatalf("newAgentIndexService: %v", err)
	}
	return service, client
}

// idxStrPtr is the *string helper of this file (the package-internal test
// helpers live in `package observability` and are not visible here).
func idxStrPtr(value string) *string { return &value }

// idxIntPtr is the *int the twoAgents overrides need.
func idxIntPtr(value int) *int { return &value }

// payloadPair projects `{from, to}` out of an event payload.
func payloadPair(event observability.AgentEventRecord) map[string]string {
	payload, _ := event.Payload.(map[string]any)
	pair := map[string]string{}
	for _, key := range []string{"from", "to"} {
		if value, ok := payload[key].(string); ok {
			pair[key] = value
		}
	}
	return pair
}

// eventsOfType filters stored events by type.
func eventsOfType(
	events []observability.AgentEventRecord,
	eventType observability.AgentEventType,
) []observability.AgentEventRecord {
	filtered := []observability.AgentEventRecord{}
	for _, event := range events {
		if event.Type == eventType {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

// TestIndexRefreshesOnlyChangedAgents ports
// "refreshes only missing, revised, or identity-changed agents and overlays
// pane revisions".
func TestIndexRefreshesOnlyChangedAgents(t *testing.T) {
	harness := openIdxHarness(t)
	history := newIdxHistory("result")
	holder := &idxSnapshotHolder{}
	holder.set(idxTwoAgents(idxTwoAgentsInput{}))
	service, _ := newIdxService(t, harness, holder, history, nil)
	refresh := func() *observability.AgentIndexRefreshResult {
		t.Helper()
		result, err := service.RefreshHerdrSession(turnSessionInput())
		if err != nil {
			t.Fatalf("refreshHerdrSession: %v", err)
		}
		return result
	}

	first := refresh()
	if want := []string{"claude", "codex"}; !reflect.DeepEqual(history.recordedCalls(), want) {
		t.Fatalf("history calls = %v, want %v", history.recordedCalls(), want)
	}
	wantScope := []observability.AgentScope{{HerdrSessionName: "default", WorkspaceID: "wJ"}}
	if !reflect.DeepEqual(first.ContextChangedScopes, wantScope) {
		t.Fatalf("contextChangedScopes = %+v, want %+v", first.ContextChangedScopes, wantScope)
	}

	history.resetCalls()
	refresh()
	if got := history.recordedCalls(); len(got) != 0 {
		t.Fatalf("history calls = %v, want none for an unchanged snapshot", got)
	}

	holder.set(idxTwoAgents(idxTwoAgentsInput{ClaudeRevision: idxIntPtr(11)}))
	refresh()
	if want := []string{"claude"}; !reflect.DeepEqual(history.recordedCalls(), want) {
		t.Fatalf("history calls after a revision bump = %v, want %v", history.recordedCalls(), want)
	}

	history.resetCalls()
	holder.set(idxTwoAgents(idxTwoAgentsInput{
		ClaudeRevision: idxIntPtr(11),
		CodexCwd:       idxStrPtr("/other"),
	}))
	refresh()
	if want := []string{"codex"}; !reflect.DeepEqual(history.recordedCalls(), want) {
		t.Fatalf("history calls after a cwd change = %v, want %v", history.recordedCalls(), want)
	}

	history.resetCalls()
	holder.set(idxTwoAgents(idxTwoAgentsInput{
		ClaudePane:      idxStrPtr("wK:p2"),
		ClaudeRevision:  idxIntPtr(11),
		ClaudeWorkspace: idxStrPtr("wK"),
		CodexCwd:        idxStrPtr("/other"),
	}))
	moved := refresh()
	if got := history.recordedCalls(); len(got) != 0 {
		t.Fatalf("history calls after a pane move = %v, want none", got)
	}
	wantMoved := []observability.AgentScope{
		{HerdrSessionName: "default", WorkspaceID: "wJ"},
		{HerdrSessionName: "default", WorkspaceID: "wK"},
	}
	if !reflect.DeepEqual(moved.ContextChangedScopes, wantMoved) {
		t.Fatalf("contextChangedScopes after a pane move = %+v, want %+v", moved.ContextChangedScopes, wantMoved)
	}
	relocated, err := harness.agents.FindByPane(db.FindAgentByPaneInput{
		HerdrSessionName: "default",
		PaneID:           "wK:p2",
	})
	if err != nil {
		t.Fatalf("findByPane: %v", err)
	}
	if relocated == nil {
		t.Fatal("findByPane(wK:p2) = nil, want the relocated agent")
	}
	if relocated.PaneRevision == nil || *relocated.PaneRevision != 11 {
		t.Fatalf("paneRevision = %v, want 11", relocated.PaneRevision)
	}

	history.resetCalls()
	holder.set(idxTwoAgents(idxTwoAgentsInput{
		ClaudePane:      idxStrPtr("wK:p2"),
		ClaudeRevision:  idxIntPtr(11),
		ClaudeTerminal:  nil,
		HasClaudeTerm:   true,
		ClaudeWorkspace: idxStrPtr("wK"),
		CodexCwd:        idxStrPtr("/other"),
	}))
	refresh()
	if want := []string{"claude"}; !reflect.DeepEqual(history.recordedCalls(), want) {
		t.Fatalf("history calls after a terminal loss = %v, want %v", history.recordedCalls(), want)
	}

	history.resetCalls()
	holder.set(idxTwoAgents(idxTwoAgentsInput{
		ClaudePane:      idxStrPtr("wK:p2"),
		ClaudeRevision:  idxIntPtr(11),
		ClaudeTerminal:  "term_claude",
		HasClaudeTerm:   true,
		ClaudeWorkspace: idxStrPtr("wJ"),
		CodexCwd:        idxStrPtr("/other"),
	}))
	restored := refresh()
	if !reflect.DeepEqual(restored.ContextChangedScopes, wantMoved) {
		t.Fatalf("contextChangedScopes after a terminal restore = %+v, want %+v",
			restored.ContextChangedScopes, wantMoved)
	}
}

// TestIndexFallsBackToLocalIndexOnSnapshotFailure ports
// "falls back to the local index when the official agent.list snapshot fails".
func TestIndexFallsBackToLocalIndexOnSnapshotFailure(t *testing.T) {
	harness := openIdxHarness(t)
	history := newIdxHistory("result")
	holder := &idxSnapshotHolder{}
	holder.set(idxOneAgent("working", 10, "codex", "reviewer"))
	service, client := newIdxService(t, harness, holder, history, nil)

	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	indexed, err := harness.agents.List(observability.AgentQueryScope{HerdrSessionName: idxStrPtr("default")})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(indexed) != 1 {
		t.Fatalf("indexed agents = %d, want 1", len(indexed))
	}

	client.setFail(true)
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err == nil {
		t.Fatal("refreshHerdrSession = nil error, want the snapshot timeout")
	} else if !strings.Contains(err.Error(), "Herdr session.snapshot request timed out after 2000ms") {
		t.Fatalf("refreshHerdrSession error = %v, want the snapshot timeout", err)
	}

	// The failed official call must not empty the base list: the last indexed
	// snapshot keeps serving `agent.list` until Herdr answers again.
	afterFailure, err := harness.agents.List(observability.AgentQueryScope{HerdrSessionName: idxStrPtr("default")})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(afterFailure) != len(indexed) || afterFailure[0].ID != indexed[0].ID {
		t.Fatalf("indexed agents after the failure = %+v, want the previous snapshot", afterFailure)
	}
	sessions, err := harness.sessions.List()
	if err != nil {
		t.Fatalf("sessions.list: %v", err)
	}
	running := 0
	for _, session := range sessions {
		if session.Running {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running sessions = %d, want 1", running)
	}
}

// TestIndexPublishesNameOnlyChanges ports
// "publishes name-only changes without reparsing history and snapshots names in
// events".
func TestIndexPublishesNameOnlyChanges(t *testing.T) {
	harness := openIdxHarness(t)
	history := newIdxHistory("result")
	holder := &idxSnapshotHolder{}
	holder.set(idxOneAgent("working", 10, "codex", "reviewer"))
	service, _ := newIdxService(t, harness, holder, history, nil)

	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	history.resetCalls()
	holder.set(idxOneAgent("working", 10, "codex", "implementer"))
	renamed, err := service.RefreshHerdrSession(turnSessionInput())
	if err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}

	if got := history.recordedCalls(); len(got) != 0 {
		t.Fatalf("history calls = %v, want none for a name-only change", got)
	}
	wantScope := []observability.AgentScope{{HerdrSessionName: "default", WorkspaceID: "wJ"}}
	if !reflect.DeepEqual(renamed.ContextChangedScopes, wantScope) {
		t.Fatalf("contextChangedScopes = %+v, want %+v", renamed.ContextChangedScopes, wantScope)
	}
	if len(renamed.Agents) == 0 {
		t.Fatal("renamed.Agents is empty")
	}
	published := renamed.Agents[0]
	if published.Agent == nil || *published.Agent != "codex" {
		t.Fatalf("agent = %v, want codex", published.Agent)
	}
	if published.Name == nil || *published.Name != "implementer" {
		t.Fatalf("name = %v, want implementer", published.Name)
	}
	if published.TerminalID == nil || *published.TerminalID != "term_claude" {
		t.Fatalf("terminalId = %v, want term_claude", published.TerminalID)
	}

	status, err := service.HandleHerdrEvent(idxStatusEvent("wJ:p2", "done", nil))
	if err != nil {
		t.Fatalf("handleHerdrEvent: %v", err)
	}
	dones := eventsOfType(status.Events, observability.AgentEventDone)
	if len(dones) != 1 {
		t.Fatalf("agent.done events = %d, want 1 (%+v)", len(dones), status.Events)
	}
	payload, _ := dones[0].Payload.(map[string]any)
	if payload["agent"] != "codex" || payload["name"] != "implementer" || payload["to"] != "done" {
		t.Fatalf("payload = %+v, want agent=codex name=implementer to=done", payload)
	}
}

// TestIndexStatusRefreshAndUnknownPaneOnce ports
// "refreshes status immediately and synthesizes an unknown-pane transition
// exactly once".
func TestIndexStatusRefreshAndUnknownPaneOnce(t *testing.T) {
	harness := openIdxHarness(t)
	history := newIdxHistory("final result")
	holder := &idxSnapshotHolder{}
	holder.set(idxOneAgent("working", 10, "claude", nil))
	service, _ := newIdxService(t, harness, holder, history, nil)

	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	history.resetCalls()

	status, err := service.HandleHerdrEvent(idxStatusEvent("wJ:p2", "done", nil))
	if err != nil {
		t.Fatalf("handleHerdrEvent: %v", err)
	}
	if want := []string{"claude"}; !reflect.DeepEqual(history.recordedCalls(), want) {
		t.Fatalf("history calls = %v, want %v", history.recordedCalls(), want)
	}
	wantScope := []observability.AgentScope{{HerdrSessionName: "default", WorkspaceID: "wJ"}}
	if !reflect.DeepEqual(status.ContextChangedScopes, wantScope) {
		t.Fatalf("contextChangedScopes = %+v, want %+v", status.ContextChangedScopes, wantScope)
	}
	dones := eventsOfType(status.Events, observability.AgentEventDone)
	if len(dones) != 1 {
		t.Fatalf("agent.done events = %d, want 1", len(dones))
	}
	if dones[0].CompactHistory == nil || dones[0].CompactHistory.LastAssistantMessage == nil ||
		dones[0].CompactHistory.LastAssistantMessage.Text != "final result" {
		t.Fatalf("agent.done compactHistory = %+v, want the final result excerpt", dones[0].CompactHistory)
	}

	// The same status event again is a no-op: no second transition, no new row.
	history.resetCalls()
	duplicate, err := service.HandleHerdrEvent(idxStatusEvent("wJ:p2", "done", nil))
	if err != nil {
		t.Fatalf("handleHerdrEvent(duplicate): %v", err)
	}
	if want := []string{"claude"}; !reflect.DeepEqual(history.recordedCalls(), want) {
		t.Fatalf("history calls on the duplicate = %v, want %v", history.recordedCalls(), want)
	}
	if len(duplicate.ContextChangedScopes) != 0 || len(duplicate.Events) != 0 {
		t.Fatalf("duplicate result = %+v, want no scopes and no events", duplicate)
	}

	// A pane the index never saw synthesizes exactly one transition out of
	// `unknown`.
	unknownHarness := openIdxHarness(t)
	unknownHistory := newIdxHistory("result")
	unknownHolder := &idxSnapshotHolder{}
	unknownHolder.set(idxOneAgent("idle", 10, "claude", nil))
	unknown, _ := newIdxService(t, unknownHarness, unknownHolder, unknownHistory, nil)
	recovered, err := unknown.HandleHerdrEvent(idxStatusEvent("wJ:p2", "idle", nil))
	if err != nil {
		t.Fatalf("handleHerdrEvent(unknown pane): %v", err)
	}
	idles := eventsOfType(recovered.Events, observability.AgentEventIdle)
	if len(idles) != 1 {
		t.Fatalf("agent.idle events = %d, want 1 (%+v)", len(idles), recovered.Events)
	}
	payload, _ := idles[0].Payload.(map[string]any)
	if payload["from"] != "unknown" || payload["to"] != "idle" {
		t.Fatalf("payload = %+v, want from=unknown to=idle", payload)
	}
	if name, ok := payload["name"]; !ok || name != nil {
		t.Fatalf("payload[name] = %v, want null", name)
	}
	stored, err := unknownHarness.agentEvents.ListAfter(db.ListAfterInput{
		HerdrSessionName: idxStrPtr("default"),
		WorkspaceID:      idxStrPtr("wJ"),
	})
	if err != nil {
		t.Fatalf("listAfter: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored events = %d, want 2", len(stored))
	}
}

// TestIndexWaitsForNewHistoryRef ports
// "S3: waits for a new history ref instead of advancing on a repeated old ref".
func TestIndexWaitsForNewHistoryRef(t *testing.T) {
	harness := openIdxHarness(t)
	registry := observability.NewTurnCompletionRegistry(observability.WithTurnSettimeout(50))
	source := string(observability.HistorySourcePiJSONL)
	oldRef := "x#entry=1"
	newRef := "x#entry=2"
	oldHistory := observability.CompactAgentHistory{
		LastAssistantMessage: &observability.AgentHistoryExcerpt{
			Ref:        oldRef,
			Text:       "old",
			StopReason: stringPtr("stop"),
		},
		MessageCount: 2,
		Source:       &source,
	}

	history := newIdxHistory("old")
	history.resolveOverride = func(call int) observability.AgentHistoryResolved {
		if call <= 2 {
			// The same ref must NOT count as history advancing.
			return observability.AgentHistoryResolved{CompactHistory: oldHistory}
		}
		advanced := oldHistory
		excerpt := *oldHistory.LastAssistantMessage
		excerpt.Ref = newRef
		advanced.LastAssistantMessage = &excerpt
		return observability.AgentHistoryResolved{CompactHistory: advanced}
	}

	holder := &idxSnapshotHolder{}
	holder.set(idxOneAgent("working", 10, "pi", nil))
	service, _ := newIdxService(t, harness, holder, history, &observability.AgentIndexServiceOptions{
		TurnCompletions: registry,
		Sleep:           func(int64, <-chan struct{}) {},
	})
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	history.resetCalls()

	result, err := service.HandleHerdrEvent(idxStatusEvent("wJ:p2", "done", nil))
	if err != nil {
		t.Fatalf("handleHerdrEvent: %v", err)
	}
	if got := len(history.recordedCalls()); got < 3 {
		t.Fatalf("history calls = %d, want at least 3 resolutions until the ref advances", got)
	}
	dones := eventsOfType(result.Events, observability.AgentEventDone)
	if len(dones) != 1 {
		t.Fatalf("agent.done events = %d, want 1 (%+v)", len(dones), result.Events)
	}
	if dones[0].CompactHistory == nil || dones[0].CompactHistory.LastAssistantMessage == nil ||
		dones[0].CompactHistory.LastAssistantMessage.Ref != newRef {
		t.Fatalf("agent.done ref = %+v, want %s", dones[0].CompactHistory, newRef)
	}
}

// TestIndexOneDoneWhenRefreshAndEventPlansOverlap ports
// "recovered path emits exactly one agent.done when refresh and event plans
// overlap".
func TestIndexOneDoneWhenRefreshAndEventPlansOverlap(t *testing.T) {
	harness := openIdxHarness(t)
	history := newIdxHistory("final result")
	holder := &idxSnapshotHolder{}
	holder.set(idxOneAgent("working", 10, "claude", nil))
	service, _ := newIdxService(t, harness, holder, history, nil)
	// Index the agent without a generation first.
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}

	// The pane is re-created with generation g1 and already terminal (done), so
	// the DB row (generation-less, working) does not match and the daemon
	// recovers through an internal refresh: the refresh-side plan
	// (working -> done) and the event-side plan (unknown -> done) overlap and
	// the guard must keep exactly one.
	generation := "g1"
	holder.set(idxSnapshot([]any{idxAgent(map[string]any{
		"agent_status":    "done",
		"pane_generation": generation,
		"pane_id":         "wJ:p2",
		"revision":        float64(11),
		"terminal_id":     "term_claude",
		"workspace_id":    "wJ",
	})}, []any{map[string]any{"pane_id": "wJ:p2", "revision": float64(11)}}))

	result, err := service.HandleHerdrEvent(idxStatusEvent("wJ:p2", "done", &generation))
	if err != nil {
		t.Fatalf("handleHerdrEvent: %v", err)
	}
	if got := len(eventsOfType(result.Events, observability.AgentEventDone)); got != 1 {
		t.Fatalf("handled agent.done count = %d, want 1 (%+v)", got, result.Events)
	}
	stored := harness.listEvents(t, "wJ")
	if got := len(eventsOfType(stored, observability.AgentEventDone)); got != 1 {
		t.Fatalf("stored agent.done count = %d, want 1", got)
	}
}

// TestIndexDeduplicatesRefreshTransitionRepeatedByEvent ports
// "deduplicates a refresh transition repeated by a realtime event in the same
// session".
func TestIndexDeduplicatesRefreshTransitionRepeatedByEvent(t *testing.T) {
	harness := openIdxHarness(t)
	history := newIdxHistory("result")
	holder := &idxSnapshotHolder{}
	holder.set(idxOneAgent("idle", 10, "claude", nil))
	service, _ := newIdxService(t, harness, holder, history, nil)

	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	holder.set(idxOneAgent("working", 11, "claude", nil))
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	if _, err := service.HandleHerdrEvent(idxStatusEvent("wJ:p2", "working", nil)); err != nil {
		t.Fatalf("handleHerdrEvent: %v", err)
	}

	statusChanges := eventsOfType(harness.listEvents(t, "wJ"), observability.AgentEventStatusChanged)
	if len(statusChanges) != 1 {
		t.Fatalf("agent.status.changed count = %d, want 1", len(statusChanges))
	}
	if got := payloadPair(statusChanges[0]); got["from"] != "idle" || got["to"] != "working" {
		t.Fatalf("payload = %+v, want from=idle to=working", got)
	}
}

// TestIndexPreservesOppositeStatusTransitions ports
// "preserves opposite status transitions repeated within the same second".
func TestIndexPreservesOppositeStatusTransitions(t *testing.T) {
	harness := openIdxHarness(t)
	history := newIdxHistory("result")
	holder := &idxSnapshotHolder{}
	holder.set(idxOneAgent("idle", 10, "claude", nil))
	service, _ := newIdxService(t, harness, holder, history, nil)

	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	for _, status := range []string{"working", "idle", "working"} {
		if _, err := service.HandleHerdrEvent(idxStatusEvent("wJ:p2", status, nil)); err != nil {
			t.Fatalf("handleHerdrEvent(%s): %v", status, err)
		}
	}

	statusChanges := eventsOfType(harness.listEvents(t, "wJ"), observability.AgentEventStatusChanged)
	if len(statusChanges) != 3 {
		t.Fatalf("agent.status.changed count = %d, want 3", len(statusChanges))
	}
	want := []map[string]string{
		{"from": "idle", "to": "working"},
		{"from": "working", "to": "idle"},
		{"from": "idle", "to": "working"},
	}
	for index, event := range statusChanges {
		if got := payloadPair(event); !reflect.DeepEqual(got, want[index]) {
			t.Fatalf("payload[%d] = %+v, want %+v", index, got, want[index])
		}
	}
}

// TestIndexPaneClosedRetiresAgentAndReleasesScope ports the pane.closed
// tombstone cleanup the AgentIndexService performs: the pane's agent row is
// retired, its orchestrator scope is released and the scope change is published.
func TestIndexPaneClosedRetiresAgentAndReleasesScope(t *testing.T) {
	harness := openIdxHarness(t)
	history := newIdxHistory("result")
	holder := &idxSnapshotHolder{}
	holder.set(idxOneAgent("working", 10, "claude", nil))
	service, _ := newIdxService(t, harness, holder, history, nil)
	if _, err := service.RefreshHerdrSession(turnSessionInput()); err != nil {
		t.Fatalf("refreshHerdrSession: %v", err)
	}
	indexed, err := harness.agents.List(observability.AgentQueryScope{HerdrSessionName: idxStrPtr("default")})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(indexed) != 1 {
		t.Fatalf("indexed agents = %d, want 1", len(indexed))
	}

	closed, err := service.HandleHerdrEvent(observability.HandleHerdrEventInput{
		Event:            map[string]any{"pane_id": "wJ:p2", "type": "pane.closed"},
		HerdrSessionName: "default",
		SessionDir:       "/tmp/herdr",
		SocketPath:       "/tmp/herdr.sock",
	})
	if err != nil {
		t.Fatalf("handleHerdrEvent(pane.closed): %v", err)
	}
	wantScope := []observability.AgentScope{{HerdrSessionName: "default", WorkspaceID: "wJ"}}
	if !reflect.DeepEqual(closed.ContextChangedScopes, wantScope) {
		t.Fatalf("contextChangedScopes = %+v, want %+v", closed.ContextChangedScopes, wantScope)
	}

	remaining, err := harness.agents.List(observability.AgentQueryScope{HerdrSessionName: idxStrPtr("default")})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("indexed agents after pane.closed = %d, want 0 (tombstoned)", len(remaining))
	}
	found, err := harness.agents.FindByPane(db.FindAgentByPaneInput{
		HerdrSessionName: "default",
		PaneID:           "wJ:p2",
	})
	if err != nil {
		t.Fatalf("findByPane: %v", err)
	}
	if found != nil {
		t.Fatalf("findByPane after pane.closed = %+v, want nil", found)
	}
	owner, err := harness.scopes.Get(observability.AgentOrchestratorScopeKey{
		HerdrSessionName: "default",
		WorkspaceID:      "wJ",
	})
	if err != nil {
		t.Fatalf("scopes.find: %v", err)
	}
	if owner != nil && owner.Owner != nil {
		t.Fatalf("scope owner after pane.closed = %+v, want the released scope", owner.Owner)
	}
}
