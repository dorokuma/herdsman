package daemon_test

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The port of test/integration/startup-reconcile.test.ts.
//
// The harness mirrors openObservabilityDbHarness: a migrated database in a temp
// dir with the running "default" Herdr session and one indexed agent on pane
// wA:live. The Herdr side is the client double the TS tests use, so no real
// Herdr socket is ever contacted. Every test uses t.TempDir(), so the production
// home and the live daemon are never touched.

const (
	reconcileTestSession = "default"
	reconcileWorkspaceID = "wA"
)

var reconcileScope = observability.AgentScope{
	HerdrSessionName: reconcileTestSession,
	WorkspaceID:      reconcileWorkspaceID,
}

var reconcileSessionEntry = herdr.HerdrSessionListEntry{
	Name:       reconcileTestSession,
	Running:    true,
	SessionDir: "/tmp/herdr",
	SocketPath: "/tmp/herdr.sock",
}

type reconcileHarness struct {
	dir               string
	sqlite            *sql.DB
	dbPath            string
	agentEvents       *db.AgentEventStore
	agents            *db.AgentStore
	agentHistoryCache *db.AgentHistoryCacheStore
	scopes            *db.AgentOrchestratorScopeStore
	statusEventPlans  *db.StatusEventPlanStore
	sessions          *db.HerdrSessionStore
	agent             observability.AgentIndexRecord
}

// openReconcileHarness mirrors setup(): the harness plus the seeded agent and
// the append helper the TS closure returns.
func openReconcileHarness(t *testing.T) *reconcileHarness {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.sqlite")
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
		Name:       reconcileTestSession,
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
	replaced, err := agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents: []db.HerdrAgentLike{{
			"agent":        "codex",
			"pane_id":      "wA:live",
			"terminal_id":  "term-live",
			"workspace_id": reconcileWorkspaceID,
		}},
		HerdrSessionName: reconcileTestSession,
	})
	if err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	if len(replaced) == 0 {
		t.Fatal("Expected indexed agent")
	}
	return &reconcileHarness{
		dir:               dir,
		sqlite:            sqlite,
		dbPath:            dbPath,
		agentEvents:       agentEvents,
		agents:            agents,
		agentHistoryCache: db.NewAgentHistoryCacheStore(sqlite),
		scopes:            db.NewAgentOrchestratorScopeStore(sqlite),
		statusEventPlans:  db.NewStatusEventPlanStore(sqlite),
		sessions:          sessions,
		agent:             replaced[0],
	}
}

type doneAppendInput struct {
	generation *string
	paneID     string
	payload    map[string]any
	terminalID string
}

// appendDone mirrors the TS append helper: one agent.done event for the agent
// setup() indexed.
func (h *reconcileHarness) appendDone(
	t *testing.T,
	input doneAppendInput,
) observability.AgentEventRecord {
	t.Helper()
	agentID := h.agent.ID
	workspaceID := reconcileWorkspaceID
	paneID := input.paneID
	terminalID := input.terminalID
	payload := input.payload
	if payload == nil {
		payload = map[string]any{}
	}
	event, err := h.agentEvents.Append(db.AppendAgentEventInput{
		AgentID:          &agentID,
		HerdrSessionName: reconcileTestSession,
		PaneGeneration:   input.generation,
		PaneID:           &paneID,
		Payload:          payload,
		TerminalID:       &terminalID,
		Type:             observability.AgentEventDone,
		WorkspaceID:      &workspaceID,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return event
}

type orphanAppendInput struct {
	eventType  observability.AgentEventType
	paneID     string
	payload    map[string]any
	terminalID string
}

// appendOrphan mirrors the direct agentEvents.append calls with agentId: null:
// an outcome event with no agent row behind it.
func (h *reconcileHarness) appendOrphan(
	t *testing.T,
	input orphanAppendInput,
) observability.AgentEventRecord {
	t.Helper()
	workspaceID := reconcileWorkspaceID
	paneID := input.paneID
	terminalID := input.terminalID
	payload := input.payload
	if payload == nil {
		payload = map[string]any{}
	}
	event, err := h.agentEvents.Append(db.AppendAgentEventInput{
		HerdrSessionName: reconcileTestSession,
		PaneID:           &paneID,
		Payload:          payload,
		TerminalID:       &terminalID,
		Type:             input.eventType,
		WorkspaceID:      &workspaceID,
	})
	if err != nil {
		t.Fatalf("append orphan: %v", err)
	}
	return event
}

func (h *reconcileHarness) getEvent(
	t *testing.T,
	id int64,
) observability.AgentEventRecord {
	t.Helper()
	event, err := h.agentEvents.Get(id)
	if err != nil {
		t.Fatalf("get(%d): %v", id, err)
	}
	return event
}

func (h *reconcileHarness) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := h.sqlite.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func (h *reconcileHarness) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var count int
	if err := h.sqlite.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return count
}

func (h *reconcileHarness) eventExists(t *testing.T, id int64) bool {
	t.Helper()
	return h.countRows(t, "select count(*) from agent_events where id = ?", id) == 1
}

// claimScope mirrors the direct scopes.claim calls of the TS tests.
func (h *reconcileHarness) claimScope(
	t *testing.T,
	input db.ClaimOrchestratorInput,
) db.AgentOrchestratorScopeChange {
	t.Helper()
	change, err := h.scopes.Claim(input)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return change
}

func (h *reconcileHarness) orchestratorService(
	t *testing.T,
) *observability.AgentOrchestratorService {
	t.Helper()
	return observability.NewAgentOrchestratorService(struct {
		AgentEvents observability.AgentEventStoreLike
		Agents      observability.AgentStoreLike
		Scopes      observability.AgentOrchestratorScopeStoreLike
	}{
		AgentEvents: h.agentEvents,
		Agents:      h.agents,
		Scopes:      h.scopes,
	})
}

// ---------------------------------------------------------------------------
// Herdr client double
// ---------------------------------------------------------------------------

// fakeSnapshotClient mirrors the TS client double: one pane snapshot and a
// counted close.
type fakeSnapshotClient struct {
	panes  func() ([]any, error)
	closes int
}

func (c *fakeSnapshotClient) SessionSnapshot() (any, error) {
	panes, err := c.panes()
	if err != nil {
		return nil, err
	}
	return map[string]any{"snapshot": map[string]any{"panes": panes}}, nil
}

func (c *fakeSnapshotClient) Close() {
	c.closes += 1
}

func staticPanes(panes ...map[string]any) func() ([]any, error) {
	values := make([]any, 0, len(panes))
	for _, pane := range panes {
		values = append(values, pane)
	}
	return func() ([]any, error) { return values, nil }
}

func failingPanes(err error) func() ([]any, error) {
	return func() ([]any, error) { return nil, err }
}

// livePaneEntry mirrors one entry of the snapshot's panes array.
func livePaneEntry(paneID, terminalID string, generation *string) map[string]any {
	pane := map[string]any{"pane_id": paneID, "terminal_id": terminalID}
	if generation != nil {
		pane["pane_generation"] = *generation
	}
	return pane
}

// newReconciler mirrors the TS reconciler() helper: one session, one client
// double, and the connected-terminal answer.
func newReconciler(
	h *reconcileHarness,
	panes func() ([]any, error),
	connectedTerminal bool,
) *daemon.AgentEventReconciler {
	return daemon.NewAgentEventReconciler(daemon.AgentEventReconcilerOptions{
		AgentHistoryCache: h.agentHistoryCache,
		Events:            h.agentEvents,
		Scopes:            h.scopes,
		SessionList: func() ([]herdr.HerdrSessionListEntry, error) {
			return []herdr.HerdrSessionListEntry{reconcileSessionEntry}, nil
		},
		ClientFactory: func(herdr.HerdrSessionListEntry) (daemon.HerdrSessionSnapshotClient, error) {
			return &fakeSnapshotClient{panes: panes}, nil
		},
		ConnectedTerminal: func(daemon.ConnectedTerminalInput) bool { return connectedTerminal },
	})
}

func requireReconcileResult(
	t *testing.T,
	got daemon.AgentEventReconcileResult,
	want daemon.AgentEventReconcileResult,
) {
	t.Helper()
	if got != want {
		t.Fatalf("reconcile result = %+v, want %+v", got, want)
	}
}

func requireEventStatus(
	t *testing.T,
	event observability.AgentEventRecord,
	status observability.AgentEventStatus,
) {
	t.Helper()
	if event.Status == nil || *event.Status != status {
		t.Fatalf("event %d status = %v, want %s", event.ID, event.Status, status)
	}
}

// captureStderr swaps os.Stderr for a pipe while fn runs and returns what fn
// wrote, the way the TS tests spy on console.warn.
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

func TestStartupReconcileCleansSelfOwnedEvents(t *testing.T) {
	harness := openReconcileHarness(t)
	harness.claimScope(t, db.ClaimOrchestratorInput{
		AgentOrchestratorScopeKey: reconcileScope,
		PaneID:                    "wA:live",
		TerminalID:                "term-owner",
	})
	self := harness.appendDone(t, doneAppendInput{paneID: "wA:live", terminalID: "term-owner"})
	external := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:external",
		terminalID: "term-external",
	})

	result, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:live", "term-owner", nil),
		livePaneEntry("wA:external", "term-external", nil),
	), true).Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{})

	selfEvent := harness.getEvent(t, self.ID)
	requireEventStatus(t, selfEvent, observability.AgentEventStatusAcked)
	if selfEvent.Deliverable != 0 {
		t.Fatalf("self deliverable = %d, want 0", selfEvent.Deliverable)
	}
	externalEvent := harness.getEvent(t, external.ID)
	requireEventStatus(t, externalEvent, observability.AgentEventStatusPending)
	if externalEvent.Deliverable != 1 {
		t.Fatalf("external deliverable = %d, want 1", externalEvent.Deliverable)
	}
}

func TestStartupReconcileInvalidatesAbsentPanesAndReleasesStaleOwner(t *testing.T) {
	harness := openReconcileHarness(t)
	zombiePending := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:gone",
		terminalID: "term-gone",
	})
	zombieDelivered := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:gone-2",
		terminalID: "term-gone-2",
	})
	if _, err := harness.agentEvents.ReservePending("term-delivery", 0, nil); err != nil {
		t.Fatalf("reservePending: %v", err)
	}
	harness.claimScope(t, db.ClaimOrchestratorInput{
		AgentOrchestratorScopeKey: reconcileScope,
		AckedEventID:              77,
		PaneID:                    "wA:gone",
		TerminalID:                "term-owner",
	})
	liveEvent := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:live",
		terminalID: "term-live",
	})

	result, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:live", "term-live", nil),
	), false).Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{Invalidated: 2, Released: 1})

	if harness.eventExists(t, zombiePending.ID) || harness.eventExists(t, zombieDelivered.ID) {
		t.Fatal("the absent panes' events survived the sweep")
	}
	requireEventStatus(t, harness.getEvent(t, liveEvent.ID),
		observability.AgentEventStatusPending)
	scope, err := harness.scopes.Get(reconcileScope)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if scope == nil {
		t.Fatal("scope = nil, want the claimed row")
	}
	if scope.AckedEventID != 77 {
		t.Fatalf("ackedEventId = %d, want 77", scope.AckedEventID)
	}
	if scope.Owner != nil {
		t.Fatalf("owner = %+v, want nil", scope.Owner)
	}
}

func TestStartupReconcileStopsTheSessionWalkWhenShutdownFires(t *testing.T) {
	harness := openReconcileHarness(t)
	// A zombie that the sweep would invalidate if it got that far: the point is
	// that the walk stops before any snapshot is taken, not what the sweep does.
	harness.appendDone(t, doneAppendInput{
		paneID:     "wA:gone",
		terminalID: "term-gone",
	})

	// Four running sessions: the shutdown signal is already aborted, so none of
	// them may be visited. The TS start path shares an event loop with stop() and
	// simply never resumes; in Go the session walk has to consult the signal.
	sessions := []herdr.HerdrSessionListEntry{
		{Name: "s1", Running: true},
		{Name: "s2", Running: true},
		{Name: "s3", Running: true},
		{Name: "s4", Running: true},
	}
	var visited atomic.Int64
	signal := observability.NewShutdownSignal()
	signal.Abort()

	reconciler := daemon.NewAgentEventReconciler(daemon.AgentEventReconcilerOptions{
		AgentHistoryCache: harness.agentHistoryCache,
		Events:            harness.agentEvents,
		Scopes:            harness.scopes,
		SessionList:       func() ([]herdr.HerdrSessionListEntry, error) { return sessions, nil },
		ClientFactory: func(herdr.HerdrSessionListEntry) (daemon.HerdrSessionSnapshotClient, error) {
			visited.Add(1)
			return &fakeSnapshotClient{panes: staticPanes()}, nil
		},
		ConnectedTerminal: func(daemon.ConnectedTerminalInput) bool { return false },
	})
	if _, err := reconciler.Reconcile(daemon.ReconcileOptions{ShutdownSignal: signal.Done()}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := visited.Load(); got != 0 {
		t.Fatalf("visited %d sessions, want 0: the walk must stop once the shutdown signal fires", got)
	}
}

func TestStartupReconcileReleaseStaleOwnersFalse(t *testing.T) {
	harness := openReconcileHarness(t)
	absent := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:gone",
		terminalID: "term-gone",
	})
	harness.claimScope(t, db.ClaimOrchestratorInput{
		AgentOrchestratorScopeKey: reconcileScope,
		PaneID:                    "wA:live",
		TerminalID:                "term-owner",
	})
	releaseStaleOwners := false

	result, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:live", "term-owner", nil),
	), false).Reconcile(daemon.ReconcileOptions{ReleaseStaleOwners: &releaseStaleOwners})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{Invalidated: 1})

	if harness.eventExists(t, absent.ID) {
		t.Fatal("the absent pane's event survived the sweep")
	}
	scope, err := harness.scopes.Get(reconcileScope)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if scope == nil || scope.Owner == nil {
		t.Fatalf("scope owner = %+v, want the untouched owner", scope)
	}
	if scope.Owner.PaneID != "wA:live" || scope.Owner.TerminalID != "term-owner" {
		t.Fatalf("scope owner = %+v, want wA:live/term-owner", scope.Owner)
	}
}

func TestStartupReconcileRetainsInvalidatedWithinGrace(t *testing.T) {
	harness := openReconcileHarness(t)
	invalidatedOne := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:gone-1",
		terminalID: "term-1",
	})
	invalidatedTwo := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:gone-2",
		terminalID: "term-2",
	})
	acked := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:gone-acked",
		terminalID: "term-acked",
	})
	harness.exec(t, "update agent_events set status = 'invalidated' where id in (?, ?)",
		invalidatedOne.ID, invalidatedTwo.ID)
	harness.exec(t, "update agent_events set status = 'acked' where id = ?", acked.ID)

	result, err := newReconciler(harness, staticPanes(), false).
		Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{})

	if count := harness.countRows(t,
		"select count(*) from agent_events where status = 'invalidated'"); count != 2 {
		t.Fatalf("invalidated rows = %d, want 2", count)
	}
	requireEventStatus(t, harness.getEvent(t, invalidatedOne.ID),
		observability.AgentEventStatusInvalidated)
	requireEventStatus(t, harness.getEvent(t, invalidatedTwo.ID),
		observability.AgentEventStatusInvalidated)
	requireEventStatus(t, harness.getEvent(t, acked.ID),
		observability.AgentEventStatusAcked)
}

func TestStartupReconcileDeletesInvalidatedOlderThanGrace(t *testing.T) {
	harness := openReconcileHarness(t)
	staleCreated := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:stale-created",
		terminalID: "term-stale-created",
	})
	staleDelivered := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:stale-delivered",
		terminalID: "term-stale-delivered",
	})
	fresh := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:fresh-invalidated",
		terminalID: "term-fresh",
	})
	staleCutoff := nowMs() - daemon.InvalidatedGraceMs - 1_000
	harness.exec(t,
		"update agent_events set status = 'invalidated', deliverable = 0,"+
			" last_attempt_at = null, created_at = ? where id = ?",
		staleCutoff, staleCreated.ID)
	harness.exec(t,
		"update agent_events set status = 'invalidated', deliverable = 0,"+
			" last_attempt_at = ?, created_at = ? where id = ?",
		staleCutoff, nowMs(), staleDelivered.ID)
	harness.exec(t,
		"update agent_events set status = 'invalidated', deliverable = 1 where id = ?",
		fresh.ID)

	result, err := newReconciler(harness, staticPanes(), false).
		Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{Invalidated: 2})

	remaining := harness.invalidatedEventIDs(t)
	if len(remaining) != 1 || remaining[0] != fresh.ID {
		t.Fatalf("invalidated rows = %v, want [%d]", remaining, fresh.ID)
	}
	if harness.eventExists(t, staleCreated.ID) || harness.eventExists(t, staleDelivered.ID) {
		t.Fatal("the stale invalidated rows survived the sweep")
	}
}

func (h *reconcileHarness) invalidatedEventIDs(t *testing.T) []int64 {
	t.Helper()
	rows, err := h.sqlite.Query(
		"select id from agent_events where status = 'invalidated' order by id")
	if err != nil {
		t.Fatalf("query invalidated rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan invalidated row: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate invalidated rows: %v", err)
	}
	return ids
}

func TestInvalidatedGraceMsCoversRedeliveryFreshness(t *testing.T) {
	if daemon.InvalidatedGraceMs < db.RedeliveryFreshnessMs {
		t.Fatalf("InvalidatedGraceMs = %d, want at least %d",
			daemon.InvalidatedGraceMs, db.RedeliveryFreshnessMs)
	}
}

func TestStartupReconcileGracePeriodInvalidatedStaysAckable(t *testing.T) {
	harness := openReconcileHarness(t)
	service := harness.orchestratorService(t)
	if _, err := service.Claim(observability.OrchestratorClaimInput{
		AgentScope: reconcileScope,
		PaneID:     "wA:live",
		TerminalID: "term-owner",
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	event := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:external",
		terminalID: "term-external",
	})
	if _, err := harness.agentEvents.ReservePending("term-owner", 0, nil); err != nil {
		t.Fatalf("reservePending: %v", err)
	}
	reserved := harness.getEvent(t, event.ID)
	requireEventStatus(t, reserved, observability.AgentEventStatusDelivered)
	if reserved.DeliveredToTerminalID == nil || *reserved.DeliveredToTerminalID != "term-owner" {
		t.Fatalf("deliveredToTerminalId = %v, want term-owner", reserved.DeliveredToTerminalID)
	}
	invalidated, err := harness.agentEvents.InvalidateByID(event.ID, "PANE_CLOSED")
	if err != nil {
		t.Fatalf("invalidateById: %v", err)
	}
	if !invalidated {
		t.Fatal("invalidateById = false, want true")
	}

	if _, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:live", "term-owner", nil),
	), true).Reconcile(daemon.ReconcileOptions{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	retained := harness.getEvent(t, event.ID)
	requireEventStatus(t, retained, observability.AgentEventStatusInvalidated)
	if retained.DeliveredToTerminalID == nil || *retained.DeliveredToTerminalID != "term-owner" {
		t.Fatalf("deliveredToTerminalId = %v, want term-owner", retained.DeliveredToTerminalID)
	}
	state, err := service.Ack(observability.OrchestratorAckInput{
		AgentScope: reconcileScope,
		EventID:    event.ID,
		TerminalID: "term-owner",
	})
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if state.AckedEventID != event.ID {
		t.Fatalf("ackedEventId = %d, want %d", state.AckedEventID, event.ID)
	}
	status, err := service.Status(reconcileScope)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status == nil || status.AckedEventID != event.ID {
		t.Fatalf("status = %+v, want ackedEventId %d", status, event.ID)
	}

	harness.exec(t, "update agent_events set last_attempt_at = ? where id = ?",
		nowMs()-daemon.InvalidatedGraceMs-1_000, event.ID)
	result, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:live", "term-owner", nil),
	), true).Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile (expired): %v", err)
	}
	if result.Invalidated != 1 {
		t.Fatalf("invalidated = %d, want 1", result.Invalidated)
	}
	if harness.eventExists(t, event.ID) {
		t.Fatal("the expired invalidated row survived the sweep")
	}
}

func TestStartupReconcileUnackedInvalidatedPastGraceReportsNotFound(t *testing.T) {
	harness := openReconcileHarness(t)
	service := harness.orchestratorService(t)
	if _, err := service.Claim(observability.OrchestratorClaimInput{
		AgentScope: reconcileScope,
		PaneID:     "wA:live",
		TerminalID: "term-owner",
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	event := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:external",
		terminalID: "term-external",
	})
	if _, err := harness.agentEvents.ReservePending("term-owner", 0, nil); err != nil {
		t.Fatalf("reservePending: %v", err)
	}
	if _, err := harness.agentEvents.InvalidateByID(event.ID, "PANE_CLOSED"); err != nil {
		t.Fatalf("invalidateById: %v", err)
	}
	harness.exec(t, "update agent_events set last_attempt_at = ? where id = ?",
		nowMs()-daemon.InvalidatedGraceMs-1_000, event.ID)

	if _, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:live", "term-owner", nil),
	), true).Reconcile(daemon.ReconcileOptions{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if harness.eventExists(t, event.ID) {
		t.Fatal("the expired invalidated row survived the sweep")
	}
	_, err := service.Ack(observability.OrchestratorAckInput{
		AgentScope: reconcileScope,
		EventID:    event.ID,
		TerminalID: "term-owner",
	})
	if err == nil || !strings.Contains(err.Error(), observability.MsgAckOutOfOrder) {
		t.Fatalf("ack error = %v, want %q", err, observability.MsgAckOutOfOrder)
	}
}

func TestStartupReconcilePurgesSettledOlderThanSevenDays(t *testing.T) {
	harness := openReconcileHarness(t)
	staleAcked := harness.appendDone(t, doneAppendInput{paneID: "wA:live", terminalID: "term-a"})
	freshAcked := harness.appendDone(t, doneAppendInput{paneID: "wA:live", terminalID: "term-b"})
	staleFailed := harness.appendDone(t, doneAppendInput{paneID: "wA:live", terminalID: "term-c"})
	freshFailed := harness.appendDone(t, doneAppendInput{paneID: "wA:live", terminalID: "term-d"})
	stalePending := harness.appendDone(t, doneAppendInput{paneID: "wA:live", terminalID: "term-e"})
	old := nowMs() - 8*24*60*60*1000
	harness.exec(t, "update agent_events set status = 'acked', created_at = ? where id = ?",
		old, staleAcked.ID)
	harness.exec(t, "update agent_events set status = 'failed', created_at = ? where id = ?",
		old, staleFailed.ID)
	harness.exec(t, "update agent_events set status = 'acked' where id = ?", freshAcked.ID)
	harness.exec(t, "update agent_events set status = 'failed' where id = ?", freshFailed.ID)
	harness.exec(t, "update agent_events set created_at = ? where id = ?", old, stalePending.ID)

	if _, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:live", "term-live", nil),
	), false).Reconcile(daemon.ReconcileOptions{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	remaining := harness.eventStatusRows(t)
	want := []eventStatusRow{
		{id: freshAcked.ID, status: "acked"},
		{id: freshFailed.ID, status: "failed"},
		{id: stalePending.ID, status: "pending"},
	}
	if !reflect.DeepEqual(remaining, want) {
		t.Fatalf("remaining events = %+v, want %+v", remaining, want)
	}
}

type eventStatusRow struct {
	id     int64
	status string
}

func (h *reconcileHarness) eventStatusRows(t *testing.T) []eventStatusRow {
	t.Helper()
	rows, err := h.sqlite.Query("select id, status from agent_events order by id")
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	events := []eventStatusRow{}
	for rows.Next() {
		var event eventStatusRow
		if err := rows.Scan(&event.id, &event.status); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return events
}

func TestStartupReconcileSnapshotFailureChangesNothing(t *testing.T) {
	harness := openReconcileHarness(t)
	event := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:gone",
		terminalID: "term-gone",
	})
	harness.claimScope(t, db.ClaimOrchestratorInput{
		AgentOrchestratorScopeKey: reconcileScope,
		AckedEventID:              event.ID,
		PaneID:                    "wA:gone",
		TerminalID:                "term-owner",
	})
	before, err := harness.scopes.Get(reconcileScope)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	snapshotError := errors.New("snapshot unavailable")

	var result daemon.AgentEventReconcileResult
	warned := captureStderr(t, func() {
		result, err = newReconciler(harness, failingPanes(snapshotError), false).
			Reconcile(daemon.ReconcileOptions{})
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{})

	requireEventStatus(t, harness.getEvent(t, event.ID),
		observability.AgentEventStatusPending)
	after, err := harness.scopes.Get(reconcileScope)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("scope = %+v, want %+v", after, before)
	}
	if !strings.Contains(warned, "Herdsman reconcile skipped: incomplete Herdr pane snapshot") ||
		!strings.Contains(warned, snapshotError.Error()) {
		t.Fatalf("warning = %q, want the incomplete-snapshot skip naming %q",
			warned, snapshotError.Error())
	}
}

func TestStartupReconcileSkipsEverySessionWhenALaterSnapshotFails(t *testing.T) {
	harness := openReconcileHarness(t)
	event := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:gone",
		terminalID: "term-gone",
	})
	secondSession := reconcileSessionEntry
	secondSession.Name = "second"
	secondSession.SocketPath = "/tmp/herdr-second.sock"
	snapshotError := errors.New("second snapshot unavailable")

	var result daemon.AgentEventReconcileResult
	var err error
	warned := captureStderr(t, func() {
		reconciler := daemon.NewAgentEventReconciler(daemon.AgentEventReconcilerOptions{
			Events: harness.agentEvents,
			Scopes: harness.scopes,
			SessionList: func() ([]herdr.HerdrSessionListEntry, error) {
				return []herdr.HerdrSessionListEntry{reconcileSessionEntry, secondSession}, nil
			},
			ClientFactory: func(
				entry herdr.HerdrSessionListEntry,
			) (daemon.HerdrSessionSnapshotClient, error) {
				if entry.Name == "second" {
					return &fakeSnapshotClient{panes: failingPanes(snapshotError)}, nil
				}
				return &fakeSnapshotClient{panes: staticPanes(
					livePaneEntry("wA:gone", "term-gone", nil),
				)}, nil
			},
		})
		result, err = reconciler.Reconcile(daemon.ReconcileOptions{})
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{})

	requireEventStatus(t, harness.getEvent(t, event.ID),
		observability.AgentEventStatusPending)
	if !strings.Contains(warned, "Herdsman reconcile skipped: incomplete Herdr pane snapshot") ||
		!strings.Contains(warned, snapshotError.Error()) {
		t.Fatalf("warning = %q, want the incomplete-snapshot skip naming %q",
			warned, snapshotError.Error())
	}
}

func TestStartupReconcileSessionListFailureChangesNothing(t *testing.T) {
	harness := openReconcileHarness(t)
	event := harness.appendDone(t, doneAppendInput{
		paneID:     "wA:gone",
		terminalID: "term-gone",
	})
	listError := errors.New("list unavailable")

	var result daemon.AgentEventReconcileResult
	var err error
	warned := captureStderr(t, func() {
		reconciler := daemon.NewAgentEventReconciler(daemon.AgentEventReconcilerOptions{
			Events: harness.agentEvents,
			Scopes: harness.scopes,
			SessionList: func() ([]herdr.HerdrSessionListEntry, error) {
				return nil, listError
			},
		})
		result, err = reconciler.Reconcile(daemon.ReconcileOptions{})
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{})

	requireEventStatus(t, harness.getEvent(t, event.ID),
		observability.AgentEventStatusPending)
	if !strings.Contains(warned, "Herdsman reconcile skipped: Herdr session list unavailable") ||
		!strings.Contains(warned, listError.Error()) {
		t.Fatalf("warning = %q, want the session-list skip naming %q",
			warned, listError.Error())
	}
}

func TestStartupReconcileInvalidatesOrphansAcrossPages(t *testing.T) {
	harness := openReconcileHarness(t)
	for index := 0; index < 250; index += 1 {
		harness.appendDone(t, doneAppendInput{
			paneID:     "wA:gone-" + itoa(index),
			terminalID: "term-" + itoa(index),
		})
	}

	result, err := newReconciler(harness, staticPanes(), false).
		Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{Invalidated: 250})

	candidates, err := harness.agentEvents.ListReconcileCandidates(300, 0)
	if err != nil {
		t.Fatalf("listReconcileCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("reconcile candidates = %d, want 0", len(candidates))
	}
	if count := harness.countRows(t,
		"select count(*) from agent_events where status = 'invalidated'"); count != 0 {
		t.Fatalf("invalidated rows = %d, want 0", count)
	}
}

func TestStartupReconcileIsIdempotent(t *testing.T) {
	harness := openReconcileHarness(t)
	harness.appendDone(t, doneAppendInput{paneID: "wA:gone", terminalID: "term-gone"})
	reconciler := newReconciler(harness, staticPanes(), false)

	first, err := reconciler.Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	requireReconcileResult(t, first, daemon.AgentEventReconcileResult{Invalidated: 1})

	second, err := reconciler.Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	requireReconcileResult(t, second, daemon.AgentEventReconcileResult{})
}

func TestStartupReconcileKeepsGenerationTaggedEventForGenerationlessPane(t *testing.T) {
	harness := openReconcileHarness(t)
	generation := "g1"
	generationTagged := harness.appendDone(t, doneAppendInput{
		generation: &generation,
		paneID:     "wA:g1",
		terminalID: "term-g1",
	})

	var err error
	warned := captureStderr(t, func() {
		_, err = newReconciler(harness, staticPanes(
			livePaneEntry("wA:g1", "term-g1", nil),
		), false).Reconcile(daemon.ReconcileOptions{})
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The generation-less live pane is treated as alive: its events must not
	// be swept even though the event carries a generation.
	event := harness.getEvent(t, generationTagged.ID)
	requireEventStatus(t, event, observability.AgentEventStatusPending)
	if event.Deliverable != 1 {
		t.Fatalf("deliverable = %d, want 1", event.Deliverable)
	}
	for _, want := range []string{
		"Herdsman reconcile treating generation-less live pane as alive",
		"eventId=" + itoa64(generationTagged.ID),
		"eventPaneGeneration=g1",
		"herdrSessionName=default",
		"paneId=wA:g1",
	} {
		if !strings.Contains(warned, want) {
			t.Fatalf("warning = %q, want it to contain %q", warned, want)
		}
	}
}

func TestStartupReconcileDeletesGenerationTaggedEventForAbsentPane(t *testing.T) {
	harness := openReconcileHarness(t)
	generation := "g1"
	orphan := harness.appendDone(t, doneAppendInput{
		generation: &generation,
		paneID:     "wA:gone",
		terminalID: "term-gone",
	})

	if _, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:other", "term-other", nil),
	), false).Reconcile(daemon.ReconcileOptions{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if harness.eventExists(t, orphan.ID) {
		t.Fatal("the generation-tagged event of an absent pane survived the sweep")
	}
}

func TestStartupReconcileDeletesGenerationTaggedEventForAnotherGeneration(t *testing.T) {
	harness := openReconcileHarness(t)
	generation := "g1"
	stale := harness.appendDone(t, doneAppendInput{
		generation: &generation,
		paneID:     "wA:g1",
		terminalID: "term-g1",
	})
	liveGeneration := "g2"

	if _, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:g1", "term-g1", &liveGeneration),
	), false).Reconcile(daemon.ReconcileOptions{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Both sides carry a generation and they differ: the pane was re-created,
	// so the old generation's events are stale and must be deleted.
	if harness.eventExists(t, stale.ID) {
		t.Fatal("the stale generation's event survived the sweep")
	}
}

func TestStartupReconcileKeepsUndeliveredOrphanFailure(t *testing.T) {
	harness := openReconcileHarness(t)
	failure := harness.appendOrphan(t, orphanAppendInput{
		eventType:  observability.AgentEventFailed,
		paneID:     "wA:gone",
		terminalID: "term-gone",
		payload: map[string]any{
			"agentId": "agent-gone",
			"reason":  "RETRY_EXHAUSTED",
		},
	})
	discarded := harness.appendOrphan(t, orphanAppendInput{
		eventType:  observability.AgentEventDiscarded,
		paneID:     "wA:gone-2",
		terminalID: "term-gone-2",
		payload: map[string]any{
			"agentId": "agent-gone",
			"reason":  observability.PlanWaitingHistory,
		},
	})
	// Both are older than the 300s freshness window that governs the completion
	// retention rule: the failure must survive on its own rule (no delivery
	// attempt yet), while discarded keeps the ordinary delete path.
	stale := nowMs() - db.RedeliveryFreshnessMs - 1_000
	harness.exec(t, "update agent_events set created_at = ? where id in (?, ?)",
		stale, failure.ID, discarded.ID)

	result, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:other", "term-other", nil),
	), false).Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{Invalidated: 2})

	// 保留到首次投递尝试为止：孤儿失败行转为保留作废态，不被物理删除。
	retained := harness.getEvent(t, failure.ID)
	requireEventStatus(t, retained, observability.AgentEventStatusInvalidated)
	if retained.Deliverable != 1 {
		t.Fatalf("deliverable = %d, want 1", retained.Deliverable)
	}
	if retained.DeliveryAttempts == nil || *retained.DeliveryAttempts != 0 {
		t.Fatalf("deliveryAttempts = %v, want 0", retained.DeliveryAttempts)
	}
	if retained.InvalidatedReason == nil ||
		*retained.InvalidatedReason != "RETAINED_OUTCOME_PANE_CLOSED" {
		t.Fatalf("invalidatedReason = %v, want RETAINED_OUTCOME_PANE_CLOSED",
			retained.InvalidatedReason)
	}
	// discarded 不保留：仍然被物理删除。
	if harness.eventExists(t, discarded.ID) {
		t.Fatal("the discarded row survived the sweep")
	}
}

func TestStartupReconcileDeletesOrphanFailureAfterFirstAttempt(t *testing.T) {
	harness := openReconcileHarness(t)
	attempted := harness.appendOrphan(t, orphanAppendInput{
		eventType:  observability.AgentEventFailed,
		paneID:     "wA:gone",
		terminalID: "term-gone",
		payload: map[string]any{
			"agentId": "agent-gone",
			"reason":  "RETRY_EXHAUSTED",
		},
	})
	// A row that was already handed to an owner once no longer holds the
	// retained window: the ordinary sweep applies again.
	harness.exec(t,
		"update agent_events set delivery_attempts = 1, last_attempt_at = ? where id = ?",
		nowMs()-db.RedeliveryFreshnessMs-1_000, attempted.ID)

	result, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:other", "term-other", nil),
	), false).Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{Invalidated: 1})

	if harness.eventExists(t, attempted.ID) {
		t.Fatal("the attempted orphan failure survived the sweep")
	}
}

func TestStartupReconcilePurgesReleasedScopesOlderThanTtl(t *testing.T) {
	harness := openReconcileHarness(t)
	harness.claimScope(t, db.ClaimOrchestratorInput{
		AgentOrchestratorScopeKey: reconcileScope,
		PaneID:                    "wA:live",
		TerminalID:                "term-owner",
	})
	if _, err := harness.scopes.ReleaseIfOwner(db.ReleaseIfOwnerInput{
		AgentOrchestratorScopeKey: reconcileScope,
		TerminalID:                "term-owner",
	}); err != nil {
		t.Fatalf("releaseIfOwner: %v", err)
	}
	recent := observability.AgentScope{
		HerdrSessionName: reconcileTestSession,
		WorkspaceID:      "wA-recent",
	}
	harness.claimScope(t, db.ClaimOrchestratorInput{
		AgentOrchestratorScopeKey: recent,
		PaneID:                    "wA:live",
		TerminalID:                "term-owner",
	})
	if _, err := harness.scopes.ReleaseIfOwner(db.ReleaseIfOwnerInput{
		AgentOrchestratorScopeKey: recent,
		TerminalID:                "term-owner",
	}); err != nil {
		t.Fatalf("releaseIfOwner (recent): %v", err)
	}
	active := observability.AgentScope{
		HerdrSessionName: reconcileTestSession,
		WorkspaceID:      "wA-active",
	}
	harness.claimScope(t, db.ClaimOrchestratorInput{
		AgentOrchestratorScopeKey: active,
		PaneID:                    "wA:live",
		TerminalID:                "term-live",
	})
	// Age only the first released row past the 30-day TTL.
	harness.exec(t,
		"update agent_orchestrator_scopes set updated_at = ?"+
			" where herdr_session_name = ? and workspace_id = ?",
		nowMs()-31*24*60*60*1000, reconcileTestSession, reconcileWorkspaceID)

	result, err := newReconciler(harness, staticPanes(
		livePaneEntry("wA:live", "term-live", nil),
	), true).Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requireReconcileResult(t, result, daemon.AgentEventReconcileResult{Purged: 1})

	scope, err := harness.scopes.Get(reconcileScope)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if scope != nil {
		t.Fatalf("purged scope = %+v, want none", scope)
	}
	recentScope, err := harness.scopes.Get(recent)
	if err != nil {
		t.Fatalf("get recent scope: %v", err)
	}
	if recentScope == nil || recentScope.Owner != nil {
		t.Fatalf("recent scope = %+v, want an ownerless row", recentScope)
	}
	activeScope, err := harness.scopes.Get(active)
	if err != nil {
		t.Fatalf("get active scope: %v", err)
	}
	if activeScope == nil || activeScope.Owner == nil ||
		activeScope.Owner.PaneID != "wA:live" ||
		activeScope.Owner.TerminalID != "term-live" {
		t.Fatalf("active scope owner = %+v, want wA:live/term-live", activeScope)
	}
}

func TestStartupReconcileRegistryHandoff(t *testing.T) {
	harness := openReconcileHarness(t)
	service := harness.orchestratorService(t)
	ownerConnected := true
	if _, err := service.Claim(observability.OrchestratorClaimInput{
		AgentScope:     reconcileScope,
		PaneID:         "wA:p1",
		TerminalID:     "term-1",
		OwnerConnected: &ownerConnected,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, err := service.Claim(observability.OrchestratorClaimInput{
		AgentScope:     reconcileScope,
		PaneID:         "wA:p2",
		TerminalID:     "term-2",
		OwnerConnected: &ownerConnected,
	})
	if err == nil ||
		!strings.Contains(err.Error(), "ORCHESTRATOR_SCOPE_ALREADY_CLAIMED") {
		t.Fatalf("second claim error = %v, want ORCHESTRATOR_SCOPE_ALREADY_CLAIMED", err)
	}
	released, err := service.Release(observability.OrchestratorReleaseInput{
		AgentScope: reconcileScope,
		Reason:     observability.OrchestratorChangeDisconnected,
		TerminalID: "term-1",
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released == nil {
		t.Fatal("release = nil, want a change")
	}
	takeover, err := service.Claim(observability.OrchestratorClaimInput{
		AgentScope:     reconcileScope,
		PaneID:         "wA:p2",
		TerminalID:     "term-2",
		OwnerConnected: &ownerConnected,
	})
	if err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if takeover.Current.Owner == nil || takeover.Current.Owner.TerminalID != "term-2" {
		t.Fatalf("takeover owner = %+v, want term-2", takeover.Current.Owner)
	}
	if _, err := service.Release(observability.OrchestratorReleaseInput{
		AgentScope: reconcileScope,
		Reason:     observability.OrchestratorChangeDisconnected,
		TerminalID: "term-2",
	}); err != nil {
		t.Fatalf("release (term-2): %v", err)
	}
	ownerDisconnected := false
	closed, err := service.Claim(observability.OrchestratorClaimInput{
		AgentScope:     reconcileScope,
		PaneID:         "wA:p3",
		TerminalID:     "term-3",
		OwnerConnected: &ownerDisconnected,
	})
	if err != nil {
		t.Fatalf("closed-owner claim: %v", err)
	}
	if closed.Current.Owner == nil || closed.Current.Owner.TerminalID != "term-3" {
		t.Fatalf("closed-owner claim owner = %+v, want term-3", closed.Current.Owner)
	}
}

func TestStartupReconcileConcurrentOnlineClaims(t *testing.T) {
	harness := openReconcileHarness(t)
	service := harness.orchestratorService(t)
	ownerConnected := true
	terminals := []string{"term-a", "term-b"}
	results := make(chan error, len(terminals))
	for _, terminalID := range terminals {
		go func(terminalID string) {
			_, err := service.Claim(observability.OrchestratorClaimInput{
				AgentScope:     reconcileScope,
				PaneID:         "wA:" + terminalID,
				TerminalID:     terminalID,
				OwnerConnected: &ownerConnected,
			})
			results <- err
		}(terminalID)
	}
	failures := 0
	for range terminals {
		if err := <-results; err != nil {
			failures += 1
		}
	}
	if failures != 1 {
		t.Fatalf("failed claims = %d, want 1", failures)
	}
	scope, err := harness.scopes.Get(reconcileScope)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if scope == nil || scope.Owner == nil ||
		(scope.Owner.TerminalID != "term-a" && scope.Owner.TerminalID != "term-b") {
		t.Fatalf("final owner = %+v, want term-a or term-b", scope)
	}
}

func TestStartupReconcileStalePaneClose(t *testing.T) {
	harness := openReconcileHarness(t)
	service := harness.orchestratorService(t)
	ownerConnected := true
	if _, err := service.Claim(observability.OrchestratorClaimInput{
		AgentScope:     reconcileScope,
		PaneID:         "wA:p1",
		TerminalID:     "term-1",
		OwnerConnected: &ownerConnected,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	stale, err := harness.scopes.ReleaseIfOwnerIdentity(db.ReleaseIfOwnerIdentityInput{
		AgentOrchestratorScopeKey: reconcileScope,
		PaneID:                    "wA:p1",
		TerminalID:                "term-old",
	})
	if err != nil {
		t.Fatalf("releaseIfOwnerIdentity (stale): %v", err)
	}
	if stale.Changed {
		t.Fatal("stale close changed = true, want false")
	}
	scope, err := harness.scopes.Get(reconcileScope)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if scope == nil || scope.Owner == nil ||
		scope.Owner.PaneID != "wA:p1" || scope.Owner.TerminalID != "term-1" {
		t.Fatalf("scope owner = %+v, want wA:p1/term-1", scope)
	}
	matching, err := harness.scopes.ReleaseIfOwnerIdentity(db.ReleaseIfOwnerIdentityInput{
		AgentOrchestratorScopeKey: reconcileScope,
		PaneID:                    "wA:p1",
		TerminalID:                "term-1",
	})
	if err != nil {
		t.Fatalf("releaseIfOwnerIdentity (matching): %v", err)
	}
	if !matching.Changed {
		t.Fatal("matching close changed = false, want true")
	}
	scope, err = harness.scopes.Get(reconcileScope)
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if scope == nil || scope.Owner != nil {
		t.Fatalf("scope owner = %+v, want nil", scope)
	}
}

// The status event plans share the agent events' 7-day settled TTL, so the
// reconciler sweeps them in the same cycle.
func TestStartupReconcilePurgesSettledStatusEventPlans(t *testing.T) {
	harness := openReconcileHarness(t)
	eventKey := "evt_settled"
	plan, err := harness.statusEventPlans.InsertPending(db.InsertPendingInput{
		AgentID:          harness.agent.ID,
		FromStatus:       observability.AgentStatusWorking,
		HerdrEventKey:    &eventKey,
		HerdrSessionName: reconcileTestSession,
		PaneID:           "wA:live",
		ToStatus:         observability.AgentStatusDone,
	})
	if err != nil {
		t.Fatalf("insertPending: %v", err)
	}
	if err := harness.statusEventPlans.MarkCompleted(plan.ID); err != nil {
		t.Fatalf("markCompleted: %v", err)
	}
	harness.exec(t, "update status_event_plans set updated_at = ? where id = ?",
		nowMs()-8*24*60*60*1000, plan.ID)

	// The TS reconciler() helper leaves the plan store unwired, so this cycle is
	// the one place the sweep is exercised end to end.
	if _, err := daemon.NewAgentEventReconciler(daemon.AgentEventReconcilerOptions{
		AgentHistoryCache: harness.agentHistoryCache,
		Events:            harness.agentEvents,
		Scopes:            harness.scopes,
		StatusEventPlans:  harness.statusEventPlans,
		SessionList: func() ([]herdr.HerdrSessionListEntry, error) {
			return []herdr.HerdrSessionListEntry{reconcileSessionEntry}, nil
		},
		ClientFactory: func(herdr.HerdrSessionListEntry) (daemon.HerdrSessionSnapshotClient, error) {
			return &fakeSnapshotClient{panes: staticPanes(
				livePaneEntry("wA:live", "term-live", nil),
			)}, nil
		},
	}).Reconcile(daemon.ReconcileOptions{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := harness.statusEventPlans.Get(plan.ID); err == nil {
		t.Fatal("the settled plan survived the sweep")
	}
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

func itoa64(value int64) string {
	return strconv.FormatInt(value, 10)
}
