package daemon_test

// The port of test/unit/herdr-session-watch-manager.test.ts, plus the
// final-audit-regressions cases that guard this manager's own refresh/reconnect
// rules ("retain identity filtering", "reconnect reset").
//
// The TS suite drives the manager with a fake index, a fake Herdr socket client
// and the real database harness, and leans on vi.useFakeTimers() to move the
// scheduler. The Go manager schedules with time.Ticker over the real clock (the
// TS fake clock has no Go counterpart in this package), so every cadence test
// uses a short interval plus a condition wait instead of advancing a clock: the
// assertions are the same observable facts (how often the working session is
// refreshed, that the boundary tick performs one rescan, which pane ids end up
// subscribed), only reached by waiting for them instead of by moving time.
//
// The two console.warn spies of the TS suite become the stderr capture the
// Go port writes its log lines to, asserted as substrings of the logged fields.
//
// Every test builds its database in t.TempDir(), so the production home and the
// live daemon are never touched.

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

const (
	// watchSessionName / watchWorkspaceID / watchPaneID / watchTerminalID are
	// the identifiers the TS fixtures use (entry(), agentRecord(), seedAgent()).
	watchSessionName = "default"
	watchWorkspaceID = "wB"
	watchPaneID      = "wB:p2"
	watchTerminalID  = "term_1"
	// watchSessionDir / watchSocketPath mirror entry().
	watchSessionDir = "/tmp/herdr"
	watchSocketPath = "/tmp/herdr.sock"
)

// ---------------------------------------------------------------------------
// Database harness
// ---------------------------------------------------------------------------

// watchHarness mirrors openObservabilityDbHarness: a migrated database in a temp
// dir with the stores the watch manager and the index service need.
type watchHarness struct {
	sqlite           *sql.DB
	agentEvents      *db.AgentEventStore
	agents           *db.AgentStore
	scopes           *db.AgentOrchestratorScopeStore
	statusEventPlans *db.StatusEventPlanStore
	sessions         *db.HerdrSessionStore
}

func openWatchHarness(t *testing.T) *watchHarness {
	t.Helper()
	sqlite, err := db.OpenSqlite(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("openSqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	if err := db.ApplyMigrations(sqlite); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	agentEvents, err := db.NewAgentEventStore(sqlite)
	if err != nil {
		t.Fatalf("newAgentEventStore: %v", err)
	}
	return &watchHarness{
		sqlite:           sqlite,
		agentEvents:      agentEvents,
		agents:           db.NewAgentStore(sqlite, agentEvents),
		scopes:           db.NewAgentOrchestratorScopeStore(sqlite),
		statusEventPlans: db.NewStatusEventPlanStore(sqlite),
		sessions:         db.NewHerdrSessionStore(sqlite),
	}
}

// watchIndexService mirrors `new AgentIndexService({ history, stores: harness })`
// with the fake Herdr client the identity-filtering cases drive a refresh
// through. The history fake is the one the idle-seam port already provides.
func watchIndexService(
	t *testing.T,
	harness *watchHarness,
	clientFactory observability.HerdrClientFactory,
) *observability.AgentIndexService {
	t.Helper()
	service, err := observability.NewAgentIndexService(observability.AgentIndexServiceOptions{
		ClientFactory: clientFactory,
		History:       seamHistory{},
		Stores: observability.AgentIndexServiceStores{
			AgentContextSnapshots:   db.NewAgentContextSnapshotStore(harness.sqlite),
			AgentEvents:             harness.agentEvents,
			AgentOrchestratorScopes: harness.scopes,
			AgentHistoryCache:       db.NewAgentHistoryCacheStore(harness.sqlite),
			Agents:                  harness.agents,
			HerdrSessions:           harness.sessions,
			HerdrWorkspaces:         db.NewHerdrWorkspaceStore(harness.sqlite),
			Sqlite:                  harness.sqlite,
			StatusEventPlans:        harness.statusEventPlans,
		},
	})
	if err != nil {
		t.Fatalf("newAgentIndexService: %v", err)
	}
	return service
}

// seedWatchAgent mirrors seedAgent(harness, status): the running session row
// plus the one indexed pane of the requested status.
func seedWatchAgent(t *testing.T, harness *watchHarness, status observability.AgentStatus) {
	t.Helper()
	entry := watchSessionEntry()
	if _, err := harness.sessions.UpsertRunning(db.UpsertRunningHerdrSessionInput{
		Name:       entry.Name,
		SessionDir: entry.SessionDir,
		SocketPath: entry.SocketPath,
	}); err != nil {
		t.Fatalf("upsertRunning: %v", err)
	}
	if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents: []db.HerdrAgentLike{{
			"agent":        "claude",
			"agent_status": string(status),
			"pane_id":      watchPaneID,
			"terminal_id":  watchTerminalID,
			"workspace_id": watchWorkspaceID,
		}},
		HerdrSessionName: watchSessionName,
	}); err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fixtures mirroring the TS module-level helpers
// ---------------------------------------------------------------------------

// watchSessionEntry mirrors entry().
func watchSessionEntry() herdr.HerdrSessionListEntry {
	return herdr.HerdrSessionListEntry{
		Name:       watchSessionName,
		Running:    true,
		SessionDir: watchSessionDir,
		SocketPath: watchSocketPath,
	}
}

// watchResult mirrors result(agents).
func watchResult(
	agents ...observability.AgentIndexRecord,
) *observability.AgentIndexRefreshResult {
	return &observability.AgentIndexRefreshResult{
		Agents:               agents,
		ContextChangedScopes: []observability.AgentScope{},
		Events:               []observability.AgentEventRecord{},
	}
}

// watchEvent mirrors event(workspaceId).
func watchEvent() observability.AgentEventRecord {
	return watchEventInWorkspace(watchWorkspaceID)
}

// watchEventInWorkspace mirrors event(workspaceId) with an explicit workspace.
func watchEventInWorkspace(workspaceID string) observability.AgentEventRecord {
	workspace := workspaceID
	paneID := watchPaneID
	return observability.AgentEventRecord{
		HerdrSessionName: watchSessionName,
		ID:               1,
		PaneID:           &paneID,
		Payload:          map[string]any{},
		Type:             observability.AgentEventIdle,
		WorkspaceID:      &workspace,
	}
}

// watchEventWithType mirrors `{ ...event(), type, ... }`.
func watchEventWithType(
	eventType observability.AgentEventType,
	terminalID *string,
	paneID string,
) observability.AgentEventRecord {
	event := watchEvent()
	pane := paneID
	event.Type = eventType
	event.PaneID = &pane
	event.TerminalID = terminalID
	return event
}

// watchAgentRecord mirrors agentRecord(paneId, workspaceId, agentStatus).
func watchAgentRecord(
	paneID string,
	workspaceID string,
	status observability.AgentStatus,
) observability.AgentIndexRecord {
	agent := "pi"
	terminalID := watchTerminalID
	return observability.AgentIndexRecord{
		Agent:            &agent,
		AgentStatus:      status,
		HerdrSessionName: watchSessionName,
		ID:               "ag_1",
		PaneID:           paneID,
		TerminalID:       &terminalID,
		WorkspaceID:      workspaceID,
	}
}

// watchTestPlan mirrors testPlan(agent, compactHistory).
func watchTestPlan(
	agent observability.AgentIndexRecord,
	compactHistory *observability.CompactAgentHistory,
) observability.StatusEventPlan {
	return observability.StatusEventPlan{
		Agent:          agent,
		CompactHistory: compactHistory,
		From:           observability.AgentStatusWorking,
		To:             observability.AgentStatusDone,
	}
}

// watchStatusEvent mirrors the pane.agent_status_changed payload the TS fakes
// yield on the socket.
func watchStatusEvent(paneID, agentStatus string) map[string]any {
	return map[string]any{
		"agent_status": agentStatus,
		"pane_id":      paneID,
		"type":         "pane.agent_status_changed",
	}
}

// watchSettledSource mirrors a fake refreshHerdrSession whose promise is
// already resolved.
func watchSettledSource(
	result *observability.AgentIndexRefreshResult,
) *observability.RefreshSource {
	source, settle := observability.NewRefreshSource()
	settle(result, nil)
	return source
}

// ---------------------------------------------------------------------------
// Fake Herdr watch client
// ---------------------------------------------------------------------------

// watchFakeClient is the Go form of the TS clientFactory fake: Close counts the
// teardown the way the tests count `close() {}` calls, SubscribeEvents hands out
// one scripted stream per subscription.
type watchFakeClient struct {
	onClose     func()
	onSubscribe func(
		ctx context.Context,
		paneIDs []string,
	) (daemon.HerdrWatchEventStream, error)
}

func (c *watchFakeClient) Close() {
	if c.onClose != nil {
		c.onClose()
	}
}

func (c *watchFakeClient) SubscribeEvents(
	ctx context.Context,
	paneIDs []string,
) (daemon.HerdrWatchEventStream, error) {
	return c.onSubscribe(ctx, paneIDs)
}

// watchStream is one scripted subscription. Its events are yielded in order;
// then the stream either ends (the TS generator returning) or parks until the
// subscription context is cancelled (the TS generator awaiting the abort
// listener). abortAware is the TS `if (options?.signal?.aborted) return` guard
// evaluated when the generator body starts.
type watchStream struct {
	ctx        context.Context
	events     []any
	index      int
	abortAware bool
	park       bool
}

func newWatchStream(
	ctx context.Context,
	abortAware bool,
	park bool,
	events ...any,
) *watchStream {
	return &watchStream{abortAware: abortAware, ctx: ctx, events: events, park: park}
}

func (s *watchStream) Next() (any, bool, error) {
	if s.index == 0 && s.abortAware && s.ctx.Err() != nil {
		return nil, true, nil
	}
	if s.index < len(s.events) {
		event := s.events[s.index]
		s.index++
		return event, false, nil
	}
	if !s.park {
		return nil, true, nil
	}
	<-s.ctx.Done()
	return nil, true, nil
}

// watchFailingStream is the TS stream whose iteration rejects.
type watchFailingStream struct {
	err error
}

func (s *watchFailingStream) Next() (any, bool, error) {
	return nil, false, s.err
}

// watchParkingFactory is managerFor's default clientFactory: a client that
// never delivers anything and whose subscription ends when it is aborted.
func watchParkingFactory(onClose func()) daemon.HerdrSessionWatchClientFactory {
	return func(string) (daemon.HerdrSessionWatchClient, error) {
		return &watchFakeClient{
			onClose: onClose,
			onSubscribe: func(
				ctx context.Context,
				_ []string,
			) (daemon.HerdrWatchEventStream, error) {
				return newWatchStream(ctx, true, true), nil
			},
		}, nil
	}
}

// watchStatusThenParkFactory is the clientFactory of the status-plan tests: the
// stream yields one agent status transition, then stays open until the watcher
// aborts.
func watchStatusThenParkFactory(
	onClose func(),
	paneID string,
	agentStatus string,
) daemon.HerdrSessionWatchClientFactory {
	return func(string) (daemon.HerdrSessionWatchClient, error) {
		return &watchFakeClient{
			onClose: onClose,
			onSubscribe: func(
				ctx context.Context,
				_ []string,
			) (daemon.HerdrWatchEventStream, error) {
				return newWatchStream(
					ctx, true, true, watchStatusEvent(paneID, agentStatus),
				), nil
			},
		}, nil
	}
}

// ---------------------------------------------------------------------------
// Fake index service
// ---------------------------------------------------------------------------

// watchIndexFake is the Go form of the TS index fake. It always carries the two
// methods the manager requires; the optional fast methods and the plan executor
// live on watchIndexFastFake, so a test selects the same branch the TS fake
// selected by which methods it defined (the Go port feature-detects them with a
// type assertion instead of a runtime `if (index.x)` check).
type watchIndexFake struct {
	onHandleHerdrEvent    func(input observability.HandleHerdrEventInput) (*observability.AgentEventHandlingResult, error)
	onRefreshHerdrSession func(input observability.RefreshInput) *observability.RefreshSource
}

func (f *watchIndexFake) HandleHerdrEvent(
	input observability.HandleHerdrEventInput,
) (*observability.AgentEventHandlingResult, error) {
	return f.onHandleHerdrEvent(input)
}

func (f *watchIndexFake) RefreshHerdrSessionSource(
	input observability.RefreshInput,
) *observability.RefreshSource {
	return f.onRefreshHerdrSession(input)
}

// watchIndexFastFake adds refreshHerdrSessionFast, handleHerdrEventFast and
// executeStatusEventPlan.
type watchIndexFastFake struct {
	watchIndexFake
	onHandleHerdrEventFast    func(input observability.HandleHerdrEventInput) (*observability.AgentEventHandlingFastResult, error)
	onRefreshHerdrSessionFast func(input observability.RefreshInput) (*observability.AgentIndexRefreshFastResult, error)
	onExecuteStatusEventPlan  func(plan observability.StatusEventPlan) (*observability.AgentEventRecord, error)
}

func (f *watchIndexFastFake) HandleHerdrEventFast(
	input observability.HandleHerdrEventInput,
) (*observability.AgentEventHandlingFastResult, error) {
	return f.onHandleHerdrEventFast(input)
}

func (f *watchIndexFastFake) RefreshHerdrSessionFast(
	input observability.RefreshInput,
) (*observability.AgentIndexRefreshFastResult, error) {
	return f.onRefreshHerdrSessionFast(input)
}

func (f *watchIndexFastFake) ExecuteStatusEventPlan(
	plan observability.StatusEventPlan,
) (*observability.AgentEventRecord, error) {
	return f.onExecuteStatusEventPlan(plan)
}

// watchEmptyHandle is the TS default `handleHerdrEvent` fake.
func watchEmptyHandle(
	observability.HandleHerdrEventInput,
) (*observability.AgentEventHandlingResult, error) {
	return &observability.AgentEventHandlingResult{
		ContextChangedScopes: []observability.AgentScope{},
		Events:               []observability.AgentEventRecord{},
	}, nil
}

// watchEmptyFastHandle is the TS default `handleHerdrEventFast` fake.
func watchEmptyFastHandle(
	observability.HandleHerdrEventInput,
) (*observability.AgentEventHandlingFastResult, error) {
	return &observability.AgentEventHandlingFastResult{
		ContextChangedScopes: []observability.AgentScope{},
		Events:               []observability.AgentEventRecord{},
		StatusEventPlans:     []observability.StatusEventPlan{},
	}, nil
}

// watchEmptyFastRefresh is the TS default `refreshHerdrSessionFast` fake.
func watchEmptyFastRefresh(
	observability.RefreshInput,
) (*observability.AgentIndexRefreshFastResult, error) {
	return &observability.AgentIndexRefreshFastResult{
		Agents:               []observability.AgentIndexRecord{},
		ContextChangedScopes: []observability.AgentScope{},
		Events:               []observability.AgentEventRecord{},
		StatusEventPlans:     []observability.StatusEventPlan{},
	}, nil
}

// watchRefreshReturns builds a slow refresh callback that returns one settled
// result per call, counting the calls.
func watchRefreshReturns(
	calls *watchCounter,
	agents func(call int) []observability.AgentIndexRecord,
) func(observability.RefreshInput) *observability.RefreshSource {
	return func(observability.RefreshInput) *observability.RefreshSource {
		call := calls.addAndGet()
		return watchSettledSource(watchResult(agents(call)...))
	}
}

// ---------------------------------------------------------------------------
// Manager factory and observation helpers
// ---------------------------------------------------------------------------

// watchManagerOptions mirrors the managerFor overrides. The zero value of each
// interval means "the TS managerFor default" (60s), never the constructor
// default, because the TS helper always passes both explicitly.
type watchManagerOptions struct {
	activeRevisionPollMs  int
	clientFactory         daemon.HerdrSessionWatchClientFactory
	fullRescanMs          int
	index                 daemon.WatchIndexService
	onAgentContextChanged func(scope observability.AgentScope)
	onAgentEvent          func(event observability.AgentEventRecord)
	onAgentIndexRefreshed func(agents []observability.AgentIndexRecord, herdrSessionName string)
	reconnectDelayMs      int
	sessionList           herdr.HerdrSessionListRunner
	shutdownSignal        <-chan struct{}
}

// watchManagerFor mirrors managerFor: the harness stores, the TS defaults (60s
// intervals, a client that only parks, an explicit reconnectDelayMs of 0, this
// one running session) and the per-test overrides on top.
func watchManagerFor(
	harness *watchHarness,
	options watchManagerOptions,
) *daemon.HerdrSessionWatchManager {
	activeRevisionPollMs := options.activeRevisionPollMs
	if activeRevisionPollMs == 0 {
		activeRevisionPollMs = 60_000
	}
	fullRescanMs := options.fullRescanMs
	if fullRescanMs == 0 {
		fullRescanMs = 60_000
	}
	clientFactory := options.clientFactory
	if clientFactory == nil {
		clientFactory = watchParkingFactory(nil)
	}
	sessionList := options.sessionList
	if sessionList == nil {
		sessionList = func() ([]herdr.HerdrSessionListEntry, error) {
			return []herdr.HerdrSessionListEntry{watchSessionEntry()}, nil
		}
	}
	return daemon.NewHerdrSessionWatchManager(daemon.HerdrSessionWatchOptions{
		ActiveRevisionPollMs:  activeRevisionPollMs,
		Agents:                harness.agents,
		ClientFactory:         clientFactory,
		FullRescanMs:          fullRescanMs,
		HerdrSessions:         harness.sessions,
		Index:                 options.index,
		OnAgentContextChanged: options.onAgentContextChanged,
		OnAgentEvent:          options.onAgentEvent,
		OnAgentIndexRefreshed: options.onAgentIndexRefreshed,
		// The TS helper always passes reconnectDelayMs (defaulting to 0), so an
		// explicit value is always present; HasReconnectDelayMs keeps that 0 from
		// falling back to the constructor's 1s default.
		ReconnectDelayMs:    options.reconnectDelayMs,
		HasReconnectDelayMs: true,
		SessionList:         sessionList,
		ShutdownSignal:      options.shutdownSignal,
	})
}

// startWatchManager mirrors `await manager.start()` together with the afterEach
// that stops the manager.
func startWatchManager(t *testing.T, manager *daemon.HerdrSessionWatchManager) {
	t.Helper()
	t.Cleanup(func() {
		if err := manager.Stop(); err != nil {
			t.Errorf("stop after test: %v", err)
		}
	})
	if err := manager.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
}

// watchCounter is a test counter for the calls the manager makes from its
// watch goroutines.
type watchCounter struct {
	mu    sync.Mutex
	value int
}

func (c *watchCounter) add() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value++
}

func (c *watchCounter) addAndGet() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value++
	return c.value
}

func (c *watchCounter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

// watchPublishCounts collects the three publication callbacks.
type watchPublishCounts struct {
	mu         sync.Mutex
	contexts   int
	events     int
	reconciles int
}

func (p *watchPublishCounts) addContext() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.contexts++
}

func (p *watchPublishCounts) addEvent() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events++
}

func (p *watchPublishCounts) addReconcile() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reconciles++
}

func (p *watchPublishCounts) snapshot() (int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.contexts, p.events, p.reconciles
}

// watchEventLog collects what the manager hands to onAgentEvent.
type watchEventLog struct {
	mu     sync.Mutex
	events []observability.AgentEventRecord
}

func (l *watchEventLog) add(event observability.AgentEventRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *watchEventLog) length() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

func (l *watchEventLog) hasType(eventType observability.AgentEventType) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, event := range l.events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func (l *watchEventLog) types() []observability.AgentEventType {
	l.mu.Lock()
	defer l.mu.Unlock()
	types := make([]observability.AgentEventType, 0, len(l.events))
	for _, event := range l.events {
		types = append(types, event.Type)
	}
	return types
}

// watchPaneIDLog records the pane ids of every subscription, the TS `subscribed`
// array.
type watchPaneIDLog struct {
	mu         sync.Mutex
	subscribed [][]string
}

func (l *watchPaneIDLog) add(paneIDs []string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	recorded := append([]string{}, paneIDs...)
	l.subscribed = append(l.subscribed, recorded)
	return len(l.subscribed)
}

func (l *watchPaneIDLog) length() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.subscribed)
}

func (l *watchPaneIDLog) at(index int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if index >= len(l.subscribed) {
		return nil
	}
	return append([]string{}, l.subscribed[index]...)
}

func (l *watchPaneIDLog) all() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]string, 0, len(l.subscribed))
	for _, paneIDs := range l.subscribed {
		out = append(out, append([]string{}, paneIDs...))
	}
	return out
}

// watchOperations is the TS `operations` string log.
type watchOperations struct {
	mu  sync.Mutex
	ops []string
}

func (o *watchOperations) add(operation string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ops = append(o.ops, operation)
}

func (o *watchOperations) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string{}, o.ops...)
}

// waitForWatch mirrors the TS waitFor: poll the predicate until it holds.
func waitForWatch(t *testing.T, description string, predicate func() bool) {
	t.Helper()
	const timeout = 5 * time.Second
	deadline := time.Now().Add(timeout)
	for {
		if predicate() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", timeout, description)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// captureWatchStderr swaps os.Stderr for a pipe while fn runs and returns what
// fn wrote — the Go form of vi.spyOn(console, "warn"). The manager reports
// warnings on stderr, so the assertion is on the logged fields.
func captureWatchStderr(t *testing.T, fn func()) string {
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
	fn()
	_ = writer.Close()
	os.Stderr = original
	return <-done
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestHerdrSessionWatchManagerUsesExactSchedulerConstants ports "uses exact
// exported scheduler constants".
func TestHerdrSessionWatchManagerUsesExactSchedulerConstants(t *testing.T) {
	if daemon.ActiveRevisionPollMs != 10_000 {
		t.Errorf("ActiveRevisionPollMs = %d, want 10000", daemon.ActiveRevisionPollMs)
	}
	if daemon.FullRescanMs != 60_000 {
		t.Errorf("FullRescanMs = %d, want 60000", daemon.FullRescanMs)
	}
}

// TestHerdrSessionWatchManagerPollsWorkingSessionsAtActiveCadence ports "polls
// working sessions at the active cadence and performs one full rescan at the
// boundary".
func TestHerdrSessionWatchManagerPollsWorkingSessionsAtActiveCadence(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusWorking)
	refreshes := &watchCounter{}
	lists := &watchCounter{}
	manager := watchManagerFor(harness, watchManagerOptions{
		activeRevisionPollMs: 10,
		// The TS boundary is 6x the poll interval; the Go port keeps that ratio
		// wide so the "no rescan before the boundary" assertion cannot be raced
		// by a loaded machine.
		fullRescanMs: 500,
		index: &watchIndexFake{
			onHandleHerdrEvent: watchEmptyHandle,
			onRefreshHerdrSession: watchRefreshReturns(refreshes, func(int) []observability.AgentIndexRecord {
				return []observability.AgentIndexRecord{
					watchAgentRecord("wB:p2", watchWorkspaceID, observability.AgentStatusWorking),
				}
			}),
		},
		sessionList: func() ([]herdr.HerdrSessionListEntry, error) {
			lists.add()
			return []herdr.HerdrSessionListEntry{watchSessionEntry()}, nil
		},
	})
	startWatchManager(t, manager)

	// The TS advances 50ms of a 10ms cadence and counts six refreshes; the Go
	// port waits for the cadence to be visibly repeating.
	waitForWatch(t, "the active cadence to keep polling the working session", func() bool {
		return refreshes.get() >= 3
	})
	// Not one of those ticks rescanned: the session list was read exactly once,
	// by start().
	if got := lists.get(); got != 1 {
		t.Fatalf("session list reads before the full-rescan boundary = %d, want 1", got)
	}
	// The boundary tick performs one full rescan, which restarts the watcher and
	// therefore refreshes again.
	waitForWatch(t, "one full rescan at the boundary", func() bool {
		return lists.get() >= 2
	})
	if got := refreshes.get(); got < 4 {
		t.Errorf("refreshes = %d after the boundary rescan, want the rescan refresh on top of the polled ones", got)
	}
}

// TestHerdrSessionWatchManagerDoesNotPollAllIdleSessionsBeforeFullRescan ports
// "does not poll all-idle sessions before the full rescan".
func TestHerdrSessionWatchManagerDoesNotPollAllIdleSessionsBeforeFullRescan(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusIdle)
	refreshes := &watchCounter{}
	manager := watchManagerFor(harness, watchManagerOptions{
		activeRevisionPollMs: 10,
		fullRescanMs:         500,
		index: &watchIndexFake{
			onHandleHerdrEvent: watchEmptyHandle,
			onRefreshHerdrSession: watchRefreshReturns(refreshes, func(int) []observability.AgentIndexRecord {
				return []observability.AgentIndexRecord{
					watchAgentRecord("wB:p2", watchWorkspaceID, observability.AgentStatusIdle),
				}
			}),
		},
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the watcher's first refresh", func() bool { return refreshes.get() >= 1 })
	// The TS advances ten ticks' worth of time and still counts one refresh; the
	// Go port waits ten tick intervals of real time instead.
	time.Sleep(100 * time.Millisecond)
	if got := refreshes.get(); got != 1 {
		t.Errorf("refreshes of an all-idle session = %d, want 1", got)
	}
}

// TestHerdrSessionWatchManagerAwaitsRemovedWatcherBeforeFinalizingStoppedSession
// ports "awaits a removed watcher before finalizing the stopped session".
func TestHerdrSessionWatchManagerAwaitsRemovedWatcherBeforeFinalizingStoppedSession(t *testing.T) {
	harness := openWatchHarness(t)
	currentSessions := []herdr.HerdrSessionListEntry{watchSessionEntry()}
	sessionMutation := &watchCounter{}
	subscriptions := &watchCounter{}
	gate := make(chan struct{})
	var upsertErr error
	var upsertMu sync.Mutex
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					_ []string,
				) (daemon.HerdrWatchEventStream, error) {
					subscriptions.add()
					return newWatchStream(ctx, false, false), nil
				},
			}, nil
		},
		index: &watchIndexFake{
			onHandleHerdrEvent: watchEmptyHandle,
			onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
				sessionMutation.add()
				<-gate
				// The refresh re-asserts the session is running, which is what a
				// rescan that marked it stopped first would undo.
				if _, err := harness.sessions.UpsertRunning(db.UpsertRunningHerdrSessionInput{
					Name:       watchSessionName,
					SessionDir: watchSessionDir,
					SocketPath: watchSocketPath,
				}); err != nil {
					upsertMu.Lock()
					upsertErr = err
					upsertMu.Unlock()
				}
				return watchSettledSource(watchResult())
			},
		},
		sessionList: func() ([]herdr.HerdrSessionListEntry, error) { return currentSessions, nil },
	})
	startWatchManager(t, manager)
	// The watcher's refresh is now parked on the gate.
	waitForWatch(t, "the watcher refresh to park on the gate", func() bool {
		return sessionMutation.get() >= 1
	})

	currentSessions = nil
	rescanned := make(chan error, 1)
	go func() { rescanned <- manager.RescanNow() }()
	// The session disappeared from the list, but the rescan must not finalize it
	// while the removed watcher's refresh is still in flight.
	select {
	case err := <-rescanned:
		t.Fatalf("rescanNow settled while the removed watcher was still in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(gate)
	if err := <-rescanned; err != nil {
		t.Fatalf("rescanNow: %v", err)
	}
	upsertMu.Lock()
	defer upsertMu.Unlock()
	if upsertErr != nil {
		t.Fatalf("refresh upsertRunning: %v", upsertErr)
	}
	session, err := harness.sessions.Get(watchSessionName)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if session.Running {
		t.Error("expected the removed session to be finalized as not running")
	}
	if got := subscriptions.get(); got != 0 {
		t.Errorf("subscriptions = %d, want 0", got)
	}
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// TestHerdrSessionWatchManagerDoesNotStartWatcherAfterStopWinsRescanRace ports
// "does not start a watcher after stop wins a full-rescan race".
func TestHerdrSessionWatchManagerDoesNotStartWatcherAfterStopWinsRescanRace(t *testing.T) {
	harness := openWatchHarness(t)
	lists := &watchCounter{}
	clients := &watchCounter{}
	closes := &watchCounter{}
	rescanRelease := make(chan struct{})
	manager := watchManagerFor(harness, watchManagerOptions{
		activeRevisionPollMs: 10,
		fullRescanMs:         30,
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			clients.add()
			return &watchFakeClient{
				onClose: closes.add,
				onSubscribe: func(
					ctx context.Context,
					_ []string,
				) (daemon.HerdrWatchEventStream, error) {
					return newWatchStream(ctx, true, true), nil
				},
			}, nil
		},
		index: &watchIndexFake{
			onHandleHerdrEvent: watchEmptyHandle,
			onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
				return watchSettledSource(watchResult())
			},
		},
		sessionList: func() ([]herdr.HerdrSessionListEntry, error) {
			call := lists.addAndGet()
			if call == 1 {
				return []herdr.HerdrSessionListEntry{watchSessionEntry()}, nil
			}
			// The boundary rescan is parked on the session list when stop() lands.
			<-rescanRelease
			return []herdr.HerdrSessionListEntry{watchSessionEntry()}, nil
		},
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the boundary tick to park inside the rescan", func() bool {
		return lists.get() >= 2
	})
	if got := clients.get(); got != 1 {
		t.Fatalf("clients = %d, want 1", got)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- manager.Stop() }()
	// stop() has torn the running watcher down (the TS awaits #beginShutdown's
	// synchronous part before it resolves the parked session list), so the
	// rescan that follows loses the generation race.
	waitForWatch(t, "stop to abort the running watcher", func() bool { return closes.get() >= 1 })
	close(rescanRelease)
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := clients.get(); got != 1 {
		t.Errorf("clients = %d, want 1: the losing rescan must not start a watcher", got)
	}
}

// TestHerdrSessionWatchManagerPublishesOneSharedRefreshResultOnce ports
// "publishes one shared refresh result once".
func TestHerdrSessionWatchManagerPublishesOneSharedRefreshResultOnce(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusWorking)
	shared, settleShared := observability.NewRefreshSource()
	refreshes := &watchCounter{}
	published := &watchPublishCounts{}
	manager := watchManagerFor(harness, watchManagerOptions{
		activeRevisionPollMs: 10,
		fullRescanMs:         60_000,
		index: &watchIndexFake{
			onHandleHerdrEvent: watchEmptyHandle,
			onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
				refreshes.add()
				return shared
			},
		},
		onAgentContextChanged: func(observability.AgentScope) { published.addContext() },
		onAgentEvent:          func(observability.AgentEventRecord) { published.addEvent() },
		onAgentIndexRefreshed: func([]observability.AgentIndexRecord, string) {
			published.addReconcile()
		},
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the refresh start() performed", func() bool { return refreshes.get() >= 1 })
	waitForWatch(t, "the second refresh of the active tick", func() bool {
		return refreshes.get() >= 2
	})

	settleShared(&observability.AgentIndexRefreshResult{
		Agents: []observability.AgentIndexRecord{
			watchAgentRecord("wB:p2", watchWorkspaceID, observability.AgentStatusWorking),
		},
		ContextChangedScopes: []observability.AgentScope{{
			HerdrSessionName: watchSessionName,
			WorkspaceID:      watchWorkspaceID,
		}},
		Events: []observability.AgentEventRecord{watchEvent()},
	}, nil)
	waitForWatch(t, "the shared publication", func() bool {
		_, _, reconciles := published.snapshot()
		return reconciles >= 1
	})
	// The TS drains five microtasks so a second publication of the same result
	// would land inside the assertion window.
	time.Sleep(50 * time.Millisecond)
	contexts, events, reconciles := published.snapshot()
	if contexts != 1 || events != 1 || reconciles != 1 {
		t.Errorf(
			"published = {context:%d events:%d reconcile:%d}, want {context:1 events:1 reconcile:1}",
			contexts, events, reconciles,
		)
	}
}

// TestHerdrSessionWatchManagerReconnectsWhenStreamClosesWithoutRestart ports
// "reconnects when Herdr event stream closes without a restart event".
func TestHerdrSessionWatchManagerReconnectsWhenStreamClosesWithoutRestart(t *testing.T) {
	harness := openWatchHarness(t)
	received := &watchEventLog{}
	subscribes := &watchCounter{}
	handled := &watchCounter{}
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					_ []string,
				) (daemon.HerdrWatchEventStream, error) {
					call := subscribes.addAndGet()
					if call == 1 {
						// The TS generator returns without yielding: the stream
						// closed with no restart event, so the loop must reconnect.
						return newWatchStream(ctx, false, false), nil
					}
					return newWatchStream(
						ctx, false, true, watchStatusEvent(watchPaneID, "idle"),
					), nil
				},
			}, nil
		},
		index: &watchIndexFake{
			onHandleHerdrEvent: func(
				observability.HandleHerdrEventInput,
			) (*observability.AgentEventHandlingResult, error) {
				handled.add()
				return &observability.AgentEventHandlingResult{
					ContextChangedScopes: []observability.AgentScope{},
					Events:               []observability.AgentEventRecord{watchEvent()},
				}, nil
			},
			onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
				return watchSettledSource(watchResult())
			},
		},
		onAgentEvent: received.add,
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the reconnected subscription to deliver an event", func() bool {
		return handled.get() > 0
	})
	if got := subscribes.get(); got < 2 {
		t.Errorf("subscriptions = %d, want at least 2", got)
	}
	if !received.hasType(observability.AgentEventIdle) {
		t.Errorf("received events = %v, want an agent.idle", received.types())
	}
}

// TestHerdrSessionWatchManagerPublishesAgentFailedFromStatusEventPlan ports
// "publishes agent.failed returned by executeStatusEventPlan".
func TestHerdrSessionWatchManagerPublishesAgentFailedFromStatusEventPlan(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusWorking)
	received := &watchEventLog{}
	handled := &watchCounter{}
	terminalID := "term_agent"
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					_ []string,
				) (daemon.HerdrWatchEventStream, error) {
					return newWatchStream(
						ctx, false, true, watchStatusEvent(watchPaneID, "done"),
					), nil
				},
			}, nil
		},
		index: &watchIndexFastFake{
			watchIndexFake: watchIndexFake{
				onHandleHerdrEvent: watchEmptyHandle,
				onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
					return watchSettledSource(watchResult())
				},
			},
			onHandleHerdrEventFast: func(
				observability.HandleHerdrEventInput,
			) (*observability.AgentEventHandlingFastResult, error) {
				handled.add()
				return &observability.AgentEventHandlingFastResult{
					ContextChangedScopes: []observability.AgentScope{},
					Events:               []observability.AgentEventRecord{},
					StatusEventPlans: []observability.StatusEventPlan{watchTestPlan(
						watchAgentRecord(watchPaneID, watchWorkspaceID, observability.AgentStatusWorking),
						nil,
					)},
				}, nil
			},
			onRefreshHerdrSessionFast: watchEmptyFastRefresh,
			onExecuteStatusEventPlan: func(observability.StatusEventPlan) (*observability.AgentEventRecord, error) {
				event := watchEventWithType(observability.AgentEventFailed, &terminalID, watchPaneID)
				return &event, nil
			},
		},
		onAgentEvent: received.add,
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the plan's agent.failed event", func() bool {
		return handled.get() > 0 && received.length() > 0
	})
	if !received.hasType(observability.AgentEventFailed) {
		t.Errorf("received events = %v, want an agent.failed", received.types())
	}
}

// TestHerdrSessionWatchManagerPublishesAgentDiscardedThroughWhitelist ports
// "publishes agent.discarded returned by executeStatusEventPlan through the
// whitelist".
func TestHerdrSessionWatchManagerPublishesAgentDiscardedThroughWhitelist(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusWorking)
	received := &watchEventLog{}
	handled := &watchCounter{}
	terminalID := "term_agent"
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					_ []string,
				) (daemon.HerdrWatchEventStream, error) {
					return newWatchStream(
						ctx, false, true, watchStatusEvent(watchPaneID, "done"),
					), nil
				},
			}, nil
		},
		index: &watchIndexFastFake{
			watchIndexFake: watchIndexFake{
				onHandleHerdrEvent: watchEmptyHandle,
				onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
					return watchSettledSource(watchResult())
				},
			},
			onHandleHerdrEventFast: func(
				observability.HandleHerdrEventInput,
			) (*observability.AgentEventHandlingFastResult, error) {
				handled.add()
				return &observability.AgentEventHandlingFastResult{
					ContextChangedScopes: []observability.AgentScope{},
					Events:               []observability.AgentEventRecord{},
					StatusEventPlans: []observability.StatusEventPlan{watchTestPlan(
						watchAgentRecord(watchPaneID, watchWorkspaceID, observability.AgentStatusWorking),
						nil,
					)},
				}, nil
			},
			onRefreshHerdrSessionFast: watchEmptyFastRefresh,
			onExecuteStatusEventPlan: func(observability.StatusEventPlan) (*observability.AgentEventRecord, error) {
				event := watchEventWithType(observability.AgentEventDiscarded, &terminalID, watchPaneID)
				return &event, nil
			},
		},
		onAgentEvent: received.add,
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the plan's agent.discarded event", func() bool {
		return handled.get() > 0 && received.length() > 0
	})
	if !received.hasType(observability.AgentEventDiscarded) {
		t.Errorf("received events = %v, want an agent.discarded", received.types())
	}
}

// TestHerdrSessionWatchManagerContainsPlanRejectionInWatchLoop ports "rejects a
// status plan without escaping the watch loop (warn + stop completes)".
func TestHerdrSessionWatchManagerContainsPlanRejectionInWatchLoop(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusWorking)
	received := &watchEventLog{}
	handled := &watchCounter{}
	output := captureWatchStderr(t, func() {
		manager := watchManagerFor(harness, watchManagerOptions{
			clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
				return &watchFakeClient{
					onSubscribe: func(
						ctx context.Context,
						_ []string,
					) (daemon.HerdrWatchEventStream, error) {
						// The stream stays open until the watcher aborts.
						return newWatchStream(
							ctx, false, true, watchStatusEvent(watchPaneID, "done"),
						), nil
					},
				}, nil
			},
			index: &watchIndexFastFake{
				watchIndexFake: watchIndexFake{
					onHandleHerdrEvent: watchEmptyHandle,
					onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
						return watchSettledSource(watchResult())
					},
				},
				onHandleHerdrEventFast: func(
					observability.HandleHerdrEventInput,
				) (*observability.AgentEventHandlingFastResult, error) {
					handled.add()
					return &observability.AgentEventHandlingFastResult{
						ContextChangedScopes: []observability.AgentScope{},
						Events:               []observability.AgentEventRecord{},
						StatusEventPlans: []observability.StatusEventPlan{watchTestPlan(
							watchAgentRecord(
								watchPaneID, watchWorkspaceID, observability.AgentStatusWorking,
							),
							nil,
						)},
					}, nil
				},
				onRefreshHerdrSessionFast: watchEmptyFastRefresh,
				onExecuteStatusEventPlan: func(
					observability.StatusEventPlan,
				) (*observability.AgentEventRecord, error) {
					return nil, errors.New("plan boom")
				},
			},
			onAgentEvent: received.add,
		})
		startWatchManager(t, manager)
		waitForWatch(t, "the status event to be handled", func() bool { return handled.get() > 0 })
		// The rejection has to stay inside the watch loop: stop() completes and no
		// event escapes to the publisher.
		if err := manager.Stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	if got := received.length(); got != 0 {
		t.Errorf("received events = %d, want 0", got)
	}
	for _, want := range []string{
		"Herdsman status event plan failed",
		"agentId:ag_1",
		"error:plan boom",
		"from:working",
		"to:done",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("warning output does not contain %q:\n%s", want, output)
		}
	}
}

// TestHerdrSessionWatchManagerStopWaitsForInFlightPlan ports "stop() waits for an
// in-flight plan and settles once it completes".
func TestHerdrSessionWatchManagerStopWaitsForInFlightPlan(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusWorking)
	closes := &watchCounter{}
	planStarted := make(chan struct{})
	var planOnce sync.Once
	gate := make(chan struct{})
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: watchStatusThenParkFactory(closes.add, watchPaneID, "done"),
		index: &watchIndexFastFake{
			watchIndexFake: watchIndexFake{
				onHandleHerdrEvent: watchEmptyHandle,
				onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
					return watchSettledSource(watchResult())
				},
			},
			onHandleHerdrEventFast: func(
				observability.HandleHerdrEventInput,
			) (*observability.AgentEventHandlingFastResult, error) {
				return &observability.AgentEventHandlingFastResult{
					ContextChangedScopes: []observability.AgentScope{},
					Events:               []observability.AgentEventRecord{},
					StatusEventPlans: []observability.StatusEventPlan{watchTestPlan(
						watchAgentRecord(watchPaneID, watchWorkspaceID, observability.AgentStatusWorking),
						nil,
					)},
				}, nil
			},
			onRefreshHerdrSessionFast: watchEmptyFastRefresh,
			onExecuteStatusEventPlan: func(
				observability.StatusEventPlan,
			) (*observability.AgentEventRecord, error) {
				planOnce.Do(func() { close(planStarted) })
				<-gate
				return nil, nil
			},
		},
	})
	startWatchManager(t, manager)
	select {
	case <-planStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the status event plan never started")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- manager.Stop() }()
	// stop() has aborted the watchers and is draining the in-flight plan.
	waitForWatch(t, "stop to abort the watcher", func() bool { return closes.get() >= 1 })
	select {
	case err := <-stopped:
		t.Fatalf("stop settled before the in-flight plan finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// TestHerdrSessionWatchManagerShutdownSignalAbortsWatchers ports "a shutdown
// signal aborts watchers before stop() is reached".
func TestHerdrSessionWatchManagerShutdownSignalAbortsWatchers(t *testing.T) {
	harness := openWatchHarness(t)
	closes := &watchCounter{}
	lists := &watchCounter{}
	shutdown := make(chan struct{})
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: watchParkingFactory(closes.add),
		index: &watchIndexFake{
			onHandleHerdrEvent: watchEmptyHandle,
			onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
				return watchSettledSource(watchResult(
					watchAgentRecord(watchPaneID, watchWorkspaceID, observability.AgentStatusWorking),
				))
			},
		},
		sessionList: func() ([]herdr.HerdrSessionListEntry, error) {
			lists.add()
			return []herdr.HerdrSessionListEntry{watchSessionEntry()}, nil
		},
		shutdownSignal: shutdown,
	})
	startWatchManager(t, manager)
	if got := closes.get(); got != 0 {
		t.Fatalf("client closes before the shutdown = %d, want 0", got)
	}
	if got := lists.get(); got != 1 {
		t.Fatalf("session list reads = %d, want 1", got)
	}

	// Aborting the daemon shutdown signal must tear the watcher down right away:
	// waiting for the watchManager.stop() step would let a shutdown budget that
	// is already spent abandon the step and leave the watcher running.
	close(shutdown)
	waitForWatch(t, "the shutdown signal to close the watcher client", func() bool {
		return closes.get() >= 1
	})
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := manager.RescanNow(); err != nil {
		t.Fatalf("rescanNow: %v", err)
	}
	if got := closes.get(); got != 1 {
		t.Errorf("client closes = %d, want 1: no watcher may start after the shutdown", got)
	}
}

// TestHerdrSessionWatchManagerLogsFailingActiveRevisionTick ports "logs a failing
// active-revision tick and stop() still resolves".
func TestHerdrSessionWatchManagerLogsFailingActiveRevisionTick(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusWorking)
	lists := &watchCounter{}
	output := captureWatchStderr(t, func() {
		manager := watchManagerFor(harness, watchManagerOptions{
			activeRevisionPollMs: 10,
			fullRescanMs:         30,
			index: &watchIndexFake{
				onHandleHerdrEvent: watchEmptyHandle,
				onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
					return watchSettledSource(watchResult(
						watchAgentRecord(
							watchPaneID, watchWorkspaceID, observability.AgentStatusWorking,
						),
					))
				},
			},
			sessionList: func() ([]herdr.HerdrSessionListEntry, error) {
				call := lists.addAndGet()
				if call > 1 {
					return nil, errors.New("session list boom")
				}
				return []herdr.HerdrSessionListEntry{watchSessionEntry()}, nil
			},
		})
		startWatchManager(t, manager)
		// The tick at the full-rescan boundary fails. The failure handler is
		// attached where the tick is started, so the rejection is logged rather
		// than escaping — the Go equivalent of the TS unhandled-rejection listener
		// staying empty is that the tick's own goroutine reports it.
		waitForWatch(t, "the boundary tick to reach the failing session list", func() bool {
			return lists.get() >= 2
		})
		time.Sleep(50 * time.Millisecond)
		if err := manager.Stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	for _, want := range []string{
		"Herdsman Herdr session watch tick failed",
		"error:session list boom",
		"step:active-revision-poll",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("warning output does not contain %q:\n%s", want, output)
		}
	}
}

// TestHerdrSessionWatchManagerStopSettlesAfterPlanDrainGrace ports "stop()
// settles after PLAN_DRAIN_GRACE_MS even when a plan never finishes".
func TestHerdrSessionWatchManagerStopSettlesAfterPlanDrainGrace(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusWorking)
	planStarted := make(chan struct{})
	var planOnce sync.Once
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: watchStatusThenParkFactory(nil, watchPaneID, "done"),
		index: &watchIndexFastFake{
			watchIndexFake: watchIndexFake{
				onHandleHerdrEvent: watchEmptyHandle,
				onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
					return watchSettledSource(watchResult())
				},
			},
			onHandleHerdrEventFast: func(
				observability.HandleHerdrEventInput,
			) (*observability.AgentEventHandlingFastResult, error) {
				return &observability.AgentEventHandlingFastResult{
					ContextChangedScopes: []observability.AgentScope{},
					Events:               []observability.AgentEventRecord{},
					StatusEventPlans: []observability.StatusEventPlan{watchTestPlan(
						watchAgentRecord(watchPaneID, watchWorkspaceID, observability.AgentStatusWorking),
						nil,
					)},
				}, nil
			},
			onRefreshHerdrSessionFast: watchEmptyFastRefresh,
			// A plan that never settles: stop() may only return when the grace is
			// used up, which is what keeps the drain below the systemd stop timeout.
			onExecuteStatusEventPlan: func(
				observability.StatusEventPlan,
			) (*observability.AgentEventRecord, error) {
				planOnce.Do(func() { close(planStarted) })
				select {}
			},
		},
	})
	startWatchManager(t, manager)
	select {
	case <-planStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the status event plan never started")
	}

	grace := time.Duration(daemon.PlanDrainGraceMs) * time.Millisecond
	startedAt := time.Now()
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	elapsed := time.Since(startedAt)
	if elapsed < grace-200*time.Millisecond {
		t.Errorf("stop returned after %s, want the %s plan drain grace", elapsed, grace)
	}
	if elapsed > grace+10*time.Second {
		t.Errorf("stop returned after %s, want no later than %s plus slack", elapsed, grace)
	}
}

// TestHerdrSessionWatchManagerSerializesStatusPlansPerAgent ports "serializes
// status plans per agent so a repeated transition appends a single done event".
func TestHerdrSessionWatchManagerSerializesStatusPlansPerAgent(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusWorking)
	index := watchIndexService(t, harness, nil)
	agents, err := harness.agents.ListForHerdrSession(watchSessionName)
	if err != nil {
		t.Fatalf("listForHerdrSession: %v", err)
	}
	if len(agents) == 0 {
		t.Fatal("expected an indexed agent")
	}
	source := "pi-jsonl"
	compactHistory := &observability.CompactAgentHistory{
		Source: &source,
		LastAssistantMessage: &observability.AgentHistoryExcerpt{
			Ref:  "history",
			Text: "final answer",
		},
	}
	plan := watchTestPlan(agents[0], compactHistory)
	// Two plans for the same agent (the event and the refresh path can both
	// produce one). The per-agent queue must serialize them: the second execution
	// sees the first one's appended transition and deduplicates instead of
	// appending a second done.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for call := 0; call < 2; call++ {
		wg.Add(1)
		go func(call int) {
			defer wg.Done()
			_, errs[call] = index.ExecuteStatusEventPlan(plan)
		}(call)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("executeStatusEventPlan: %v", err)
		}
	}

	workspaceID := watchWorkspaceID
	sessionName := watchSessionName
	events, err := harness.agentEvents.ListAfter(db.ListAfterInput{
		HerdrSessionName: &sessionName,
		WorkspaceID:      &workspaceID,
	})
	if err != nil {
		t.Fatalf("listAfter: %v", err)
	}
	done, changed := 0, 0
	for _, event := range events {
		switch event.Type {
		case observability.AgentEventDone:
			done++
		case observability.AgentEventStatusChanged:
			changed++
		}
	}
	if done != 1 {
		t.Errorf("agent.done events = %d, want 1", done)
	}
	if changed != 1 {
		t.Errorf("agent.status.changed events = %d, want 1", changed)
	}

	rows, err := harness.sqlite.Query("select id, status from status_event_plans order by id")
	if err != nil {
		t.Fatalf("query plan rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type planRow struct {
		id     int64
		status string
	}
	planRows := []planRow{}
	for rows.Next() {
		var row planRow
		if err := rows.Scan(&row.id, &row.status); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		planRows = append(planRows, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan rows: %v", err)
	}
	if len(planRows) != 2 {
		t.Fatalf("plan rows = %d, want 2", len(planRows))
	}
	for _, row := range planRows {
		if row.status != string(db.StatusPlanCompleted) {
			t.Errorf("plan row %d status = %s, want completed", row.id, row.status)
		}
	}
	unfinished, err := harness.statusEventPlans.ListUnfinished()
	if err != nil {
		t.Fatalf("listUnfinished: %v", err)
	}
	if len(unfinished) != 0 {
		t.Errorf("unfinished plans = %d, want none", len(unfinished))
	}
}

// TestHerdrSessionWatchManagerReconcilesBeforePublishingAfterPaneMove ports
// "reconciles before publishing changed context and events after a pane move".
func TestHerdrSessionWatchManagerReconcilesBeforePublishingAfterPaneMove(t *testing.T) {
	harness := openWatchHarness(t)
	operations := &watchOperations{}
	refreshes := &watchCounter{}
	subscribes := &watchCounter{}
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					_ []string,
				) (daemon.HerdrWatchEventStream, error) {
					call := subscribes.addAndGet()
					if call == 1 {
						return newWatchStream(
							ctx, false, false, map[string]any{"type": "pane.moved"},
						), nil
					}
					return newWatchStream(ctx, true, true), nil
				},
			}, nil
		},
		index: &watchIndexFake{
			onHandleHerdrEvent: watchEmptyHandle,
			onRefreshHerdrSession: func(observability.RefreshInput) *observability.RefreshSource {
				call := refreshes.addAndGet()
				paneID, workspaceID := "wA:p1", "wA"
				if call > 1 {
					paneID, workspaceID = "wB:p3", "wB"
				}
				agent := watchAgentRecord(paneID, workspaceID, observability.AgentStatusWorking)
				return watchSettledSource(&observability.AgentIndexRefreshResult{
					Agents: []observability.AgentIndexRecord{agent},
					ContextChangedScopes: []observability.AgentScope{{
						HerdrSessionName: watchSessionName,
						WorkspaceID:      workspaceID,
					}},
					Events: []observability.AgentEventRecord{watchEventInWorkspace(workspaceID)},
				})
			},
		},
		onAgentContextChanged: func(scope observability.AgentScope) {
			operations.add("context:" + scope.WorkspaceID)
		},
		onAgentEvent: func(event observability.AgentEventRecord) {
			workspace := ""
			if event.WorkspaceID != nil {
				workspace = *event.WorkspaceID
			}
			operations.add("event:" + workspace)
		},
		onAgentIndexRefreshed: func(agents []observability.AgentIndexRecord, _ string) {
			pane := "<nil>"
			if len(agents) > 0 {
				pane = agents[0].PaneID
			}
			operations.add("reconcile:" + pane)
		},
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the second subscription after the pane move", func() bool {
		return subscribes.get() == 2
	})
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	want := []string{
		"reconcile:wA:p1", "context:wA", "event:wA",
		"reconcile:wB:p3", "context:wB", "event:wB",
	}
	if got := operations.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("operations = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Second half: subscription restart / topology handling
// ---------------------------------------------------------------------------

// watchRecord is the object view of a decoded socket event, the TS record().
func watchRecord(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

// watchEventsForSubscription mirrors herdrEventsForSubscription: Herdr only
// emits pane.agent_status_changed for pane-specific subscriptions.
func watchEventsForSubscription(
	paneIDs []string,
	events []map[string]any,
) []map[string]any {
	filtered := make([]map[string]any, 0, len(events))
	for _, event := range events {
		if eventType, _ := event["type"].(string); eventType != "pane.agent_status_changed" {
			filtered = append(filtered, event)
			continue
		}
		paneID, _ := event["pane_id"].(string)
		if paneID == "" {
			continue
		}
		for _, subscribed := range paneIDs {
			if subscribed == paneID {
				filtered = append(filtered, event)
				break
			}
		}
	}
	return filtered
}

// watchTaggedEvent mirrors taggedWatchEvent: which subscription produced the
// event, so the two streams stay tellable apart in the index fake.
func watchTaggedEvent(event map[string]any, stream int) map[string]any {
	tagged := map[string]any{}
	for key, value := range event {
		tagged[key] = value
	}
	tagged["stream"] = stream
	return tagged
}

// watchHandledEvent is one handleHerdrEvent(Fast) call the index fake recorded.
type watchHandledEvent struct {
	event  map[string]any
	stream int
}

// watchHandledLog records what reached the index, the TS `handled` array.
type watchHandledLog struct {
	mu    sync.Mutex
	items []watchHandledEvent
}

func (l *watchHandledLog) add(event map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	stream := 0
	if tagged, ok := event["stream"].(int); ok {
		stream = tagged
	}
	l.items = append(l.items, watchHandledEvent{event: event, stream: stream})
}

func (l *watchHandledLog) length() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.items)
}

// statusTransitions returns the recorded pane.agent_status_changed events, the
// TS `handled.filter((item) => item.event.type === "pane.agent_status_changed")`.
func (l *watchHandledLog) statusTransitions() []watchHandledEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	matching := []watchHandledEvent{}
	for _, item := range l.items {
		if eventType, _ := item.event["type"].(string); eventType == "pane.agent_status_changed" {
			matching = append(matching, item)
		}
	}
	return matching
}

func (l *watchHandledLog) events() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	events := make([]map[string]any, 0, len(l.items))
	for _, item := range l.items {
		events = append(events, item.event)
	}
	return events
}

// watchPaneEvent is a topology event on the fake socket.
func watchPaneEvent(paneID, eventType string) map[string]any {
	return map[string]any{"pane_id": paneID, "type": eventType}
}

// TestHerdrSessionWatchManagerRestartsSubscriptionAfterTopologyEvent ports
// "restarts subscription after <pane.created|pane.agent_detected> so the new pane
// is included".
func TestHerdrSessionWatchManagerRestartsSubscriptionAfterTopologyEvent(t *testing.T) {
	for _, topologyType := range []string{"pane.created", "pane.agent_detected"} {
		topologyType := topologyType
		t.Run(topologyType, func(t *testing.T) {
			harness := openWatchHarness(t)
			subscribed := &watchPaneIDLog{}
			refreshes := &watchCounter{}
			manager := watchManagerFor(harness, watchManagerOptions{
				clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
					return &watchFakeClient{
						onSubscribe: func(
							ctx context.Context,
							paneIDs []string,
						) (daemon.HerdrWatchEventStream, error) {
							if subscribed.add(paneIDs) == 1 {
								return newWatchStream(
									ctx, true, true, watchPaneEvent("wB:p9", topologyType),
								), nil
							}
							return newWatchStream(ctx, true, true), nil
						},
					}, nil
				},
				index: &watchIndexFake{
					onHandleHerdrEvent: watchEmptyHandle,
					onRefreshHerdrSession: func(
						observability.RefreshInput,
					) *observability.RefreshSource {
						if refreshes.addAndGet() == 1 {
							return watchSettledSource(watchResult())
						}
						return watchSettledSource(watchResult(
							watchAgentRecord("wB:p9", watchWorkspaceID, observability.AgentStatusWorking),
						))
					},
				},
			})
			startWatchManager(t, manager)
			waitForWatch(t, "the restarted subscription", func() bool {
				return subscribed.length() == 2
			})
			if err := manager.Stop(); err != nil {
				t.Fatalf("stop: %v", err)
			}
			if got := subscribed.at(0); len(got) != 0 {
				t.Errorf("first subscription pane ids = %v, want []", got)
			}
			if got := subscribed.at(1); !reflect.DeepEqual(got, []string{"wB:p9"}) {
				t.Errorf("second subscription pane ids = %v, want [wB:p9]", got)
			}
		})
	}
}

// TestHerdrSessionWatchManagerRestartsLiveSubscriptionOnNewPane ports "restarts
// the live subscription when a refresh discovers a new pane".
func TestHerdrSessionWatchManagerRestartsLiveSubscriptionOnNewPane(t *testing.T) {
	harness := openWatchHarness(t)
	seedWatchAgent(t, harness, observability.AgentStatusWorking)
	subscribed := &watchPaneIDLog{}
	refreshes := &watchCounter{}
	manager := watchManagerFor(harness, watchManagerOptions{
		// The TS advances one 10ms tick after the first subscription settled; the
		// Go port keeps a wide interval so "exactly one subscription so far" can be
		// observed before the first tick fires.
		activeRevisionPollMs: 200,
		fullRescanMs:         60_000,
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					paneIDs []string,
				) (daemon.HerdrWatchEventStream, error) {
					subscribed.add(paneIDs)
					return newWatchStream(ctx, true, true), nil
				},
			}, nil
		},
		index: &watchIndexFake{
			onHandleHerdrEvent: watchEmptyHandle,
			onRefreshHerdrSession: func(
				observability.RefreshInput,
			) *observability.RefreshSource {
				working := watchAgentRecord(watchPaneID, watchWorkspaceID, observability.AgentStatusWorking)
				if refreshes.addAndGet() == 1 {
					return watchSettledSource(watchResult(working))
				}
				extra := watchAgentRecord(
					"wB:p3", watchWorkspaceID, observability.AgentStatusWorking,
				)
				extra.ID = "ag_2"
				return watchSettledSource(watchResult(working, extra))
			},
		},
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the first subscription", func() bool { return subscribed.length() >= 1 })
	if got := subscribed.all(); !reflect.DeepEqual(got, [][]string{{watchPaneID}}) {
		t.Fatalf("subscriptions before the tick = %v, want [[%s]]", got, watchPaneID)
	}
	waitForWatch(t, "the restarted subscription carrying the new pane", func() bool {
		return subscribed.length() >= 2
	})
	if got := subscribed.at(1); !reflect.DeepEqual(got, []string{watchPaneID, "wB:p3"}) {
		t.Errorf("second subscription pane ids = %v, want [%s wB:p3]", got, watchPaneID)
	}
}

// TestHerdrSessionWatchManagerSecondConnectionDeliversIdle ports "second
// connection idle after created and working refresh delivers agent.idle".
func TestHerdrSessionWatchManagerSecondConnectionDeliversIdle(t *testing.T) {
	harness := openWatchHarness(t)
	subscribed := &watchPaneIDLog{}
	handled := &watchHandledLog{}
	received := &watchEventLog{}
	refreshes := &watchCounter{}
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					paneIDs []string,
				) (daemon.HerdrWatchEventStream, error) {
					stream := subscribed.add(paneIDs)
					// Stream 1 (paneIds=[]) is topology-only. Even if working/idle are
					// offered here, pane-specific filtering must drop them.
					offered := []map[string]any{watchPaneEvent("wB:p9", "pane.created")}
					if stream == 1 {
						offered = append(offered,
							watchStatusEvent("wB:p9", "working"),
							watchStatusEvent("wB:p9", "idle"),
						)
					} else {
						offered = []map[string]any{watchStatusEvent("wB:p9", "idle")}
					}
					events := make([]any, 0, len(paneIDs)+1)
					for _, event := range watchEventsForSubscription(paneIDs, offered) {
						events = append(events, watchTaggedEvent(event, stream))
					}
					return newWatchStream(ctx, true, true, events...), nil
				},
			}, nil
		},
		index: &watchIndexFastFake{
			watchIndexFake: watchIndexFake{
				onHandleHerdrEvent: watchEmptyHandle,
				onRefreshHerdrSession: func(
					observability.RefreshInput,
				) *observability.RefreshSource {
					refreshes.add()
					return watchSettledSource(watchResult())
				},
			},
			onHandleHerdrEventFast: func(
				input observability.HandleHerdrEventInput,
			) (*observability.AgentEventHandlingFastResult, error) {
				event := watchRecord(input.Event)
				handled.add(event)
				if status, _ := event["agent_status"].(string); status != "idle" {
					return watchEmptyFastHandle(input)
				}
				return &observability.AgentEventHandlingFastResult{
					ContextChangedScopes: []observability.AgentScope{},
					Events:               []observability.AgentEventRecord{},
					StatusEventPlans: []observability.StatusEventPlan{watchTestPlan(
						watchAgentRecord(
							"wB:p9", watchWorkspaceID, observability.AgentStatusWorking,
						),
						nil,
					)},
				}, nil
			},
			onRefreshHerdrSessionFast: func(
				input observability.RefreshInput,
			) (*observability.AgentIndexRefreshFastResult, error) {
				if refreshes.addAndGet() == 1 {
					return watchEmptyFastRefresh(input)
				}
				return &observability.AgentIndexRefreshFastResult{
					Agents: []observability.AgentIndexRecord{watchAgentRecord(
						"wB:p9", watchWorkspaceID, observability.AgentStatusWorking,
					)},
					ContextChangedScopes: []observability.AgentScope{},
					Events:               []observability.AgentEventRecord{},
					StatusEventPlans:     []observability.StatusEventPlan{},
				}, nil
			},
			onExecuteStatusEventPlan: func(
				observability.StatusEventPlan,
			) (*observability.AgentEventRecord, error) {
				event := watchEventWithType(observability.AgentEventIdle, nil, "wB:p9")
				return &event, nil
			},
		},
		onAgentEvent: received.add,
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the idle event delivered through the second subscription", func() bool {
		return received.length() > 0 && subscribed.length() >= 2
	})
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := subscribed.at(0); len(got) != 0 {
		t.Errorf("first subscription pane ids = %v, want []", got)
	}
	if got := subscribed.at(1); !reflect.DeepEqual(got, []string{"wB:p9"}) {
		t.Errorf("second subscription pane ids = %v, want [wB:p9]", got)
	}
	statusEvents := handled.statusTransitions()
	if len(statusEvents) != 1 {
		t.Fatalf("handled status events = %v, want exactly the idle one on stream 2", handled.events())
	}
	if statusEvents[0].stream != 2 {
		t.Errorf("handled status event stream = %d, want 2", statusEvents[0].stream)
	}
	if statusEvents[0].event["agent_status"] != "idle" ||
		statusEvents[0].event["pane_id"] != "wB:p9" {
		t.Errorf("handled status event = %v, want idle on wB:p9", statusEvents[0].event)
	}
	if !received.hasType(observability.AgentEventIdle) {
		t.Errorf("received events = %v, want an agent.idle", received.types())
	}
}

// TestHerdrSessionWatchManagerDoesNotInventIdleWhenFirstRefreshIsIdle ports "does
// not invent working-to-idle when first refresh of a created pane is already
// idle".
func TestHerdrSessionWatchManagerDoesNotInventIdleWhenFirstRefreshIsIdle(t *testing.T) {
	harness := openWatchHarness(t)
	subscribed := &watchPaneIDLog{}
	received := &watchEventLog{}
	planCalls := &watchCounter{}
	refreshes := &watchCounter{}
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					paneIDs []string,
				) (daemon.HerdrWatchEventStream, error) {
					stream := subscribed.add(paneIDs)
					// Stream 1: topology only. Stream 2 may include the new pane_id,
					// but herdr does not replay status for a pane that is already idle.
					offered := []map[string]any{}
					if stream == 1 {
						offered = []map[string]any{watchPaneEvent("wB:p9", "pane.created")}
					}
					events := make([]any, 0, len(offered))
					for _, event := range watchEventsForSubscription(paneIDs, offered) {
						events = append(events, event)
					}
					return newWatchStream(ctx, true, true, events...), nil
				},
			}, nil
		},
		index: &watchIndexFastFake{
			watchIndexFake: watchIndexFake{
				onHandleHerdrEvent: watchEmptyHandle,
				onRefreshHerdrSession: func(
					observability.RefreshInput,
				) *observability.RefreshSource {
					refreshes.add()
					return watchSettledSource(watchResult())
				},
			},
			onHandleHerdrEventFast:    watchEmptyFastHandle,
			onRefreshHerdrSessionFast: watchIdleAfterCreateRefresh(refreshes),
			onExecuteStatusEventPlan: func(
				observability.StatusEventPlan,
			) (*observability.AgentEventRecord, error) {
				planCalls.add()
				event := watchEventWithType(observability.AgentEventIdle, nil, watchPaneID)
				return &event, nil
			},
		},
		onAgentEvent: received.add,
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the second subscription carrying the new pane", func() bool {
		return subscribed.length() >= 2
	})
	time.Sleep(40 * time.Millisecond)
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := subscribed.at(0); len(got) != 0 {
		t.Errorf("first subscription pane ids = %v, want []", got)
	}
	if got := subscribed.at(1); !reflect.DeepEqual(got, []string{"wB:p9"}) {
		t.Errorf("second subscription pane ids = %v, want [wB:p9]", got)
	}
	if got := planCalls.get(); got != 0 {
		t.Errorf("status event plans executed = %d, want 0", got)
	}
	if got := received.types(); len(got) != 0 {
		t.Errorf("received events = %v, want none", got)
	}
}

// watchIdleAfterCreateRefresh is the refreshHerdrSessionFast of the two
// created-pane cases: nothing on the first pass, then the one new pane.
func watchIdleAfterCreateRefresh(
	refreshes *watchCounter,
) func(observability.RefreshInput) (*observability.AgentIndexRefreshFastResult, error) {
	return func(
		input observability.RefreshInput,
	) (*observability.AgentIndexRefreshFastResult, error) {
		if refreshes.addAndGet() == 1 {
			return watchEmptyFastRefresh(input)
		}
		return &observability.AgentIndexRefreshFastResult{
			Agents: []observability.AgentIndexRecord{watchAgentRecord(
				"wB:p9", watchWorkspaceID, observability.AgentStatusIdle,
			)},
			ContextChangedScopes: []observability.AgentScope{},
			Events:               []observability.AgentEventRecord{},
			StatusEventPlans:     []observability.StatusEventPlan{},
		}, nil
	}
}

// TestHerdrSessionWatchManagerDoesNotResubscribeOnReplayedTopology ports "does
// not resubscribe in a loop when the next connection replays historical
// topology".
func TestHerdrSessionWatchManagerDoesNotResubscribeOnReplayedTopology(t *testing.T) {
	harness := openWatchHarness(t)
	subscribed := &watchPaneIDLog{}
	refreshes := &watchCounter{}
	historical := []map[string]any{
		watchPaneEvent("wB:p9", "pane.created"),
		watchPaneEvent("wB:p9", "pane.moved"),
	}
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					paneIDs []string,
				) (daemon.HerdrWatchEventStream, error) {
					subscribed.add(paneIDs)
					events := make([]any, 0, len(historical))
					for _, event := range watchEventsForSubscription(paneIDs, historical) {
						events = append(events, event)
					}
					return newWatchStream(ctx, true, true, events...), nil
				},
			}, nil
		},
		index: &watchIndexFake{
			onHandleHerdrEvent: watchEmptyHandle,
			onRefreshHerdrSession: func(
				observability.RefreshInput,
			) *observability.RefreshSource {
				if refreshes.addAndGet() == 1 {
					return watchSettledSource(watchResult())
				}
				return watchSettledSource(watchResult(
					watchAgentRecord("wB:p9", watchWorkspaceID, observability.AgentStatusWorking),
				))
			},
		},
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the restarted subscription", func() bool { return subscribed.length() >= 2 })
	// A subscription that replays the same topology must not start another one.
	time.Sleep(40 * time.Millisecond)
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	want := [][]string{{}, {"wB:p9"}}
	if got := subscribed.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("subscriptions = %v, want %v", got, want)
	}
}

// TestHerdrSessionWatchManagerDoesNotRestartAfterMovedWithoutNewPane ports "does
// not restart after pane.moved when refresh has no new pane".
func TestHerdrSessionWatchManagerDoesNotRestartAfterMovedWithoutNewPane(t *testing.T) {
	harness := openWatchHarness(t)
	subscribed := &watchPaneIDLog{}
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					paneIDs []string,
				) (daemon.HerdrWatchEventStream, error) {
					if subscribed.add(paneIDs) == 1 {
						return newWatchStream(
							ctx, true, true,
							watchPaneEvent(watchPaneID, "pane.moved"),
							watchPaneEvent(watchPaneID, "pane.created"),
						), nil
					}
					return newWatchStream(ctx, true, true), nil
				},
			}, nil
		},
		index: &watchIndexFake{
			onHandleHerdrEvent: watchEmptyHandle,
			onRefreshHerdrSession: func(
				observability.RefreshInput,
			) *observability.RefreshSource {
				return watchSettledSource(watchResult(
					watchAgentRecord(watchPaneID, watchWorkspaceID, observability.AgentStatusWorking),
				))
			},
		},
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the first subscription", func() bool { return subscribed.length() >= 1 })
	time.Sleep(40 * time.Millisecond)
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	want := [][]string{{watchPaneID}}
	if got := subscribed.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("subscriptions = %v, want %v", got, want)
	}
}

// TestHerdrSessionWatchManagerIgnoresGenerationLessPaneClosed ports "ignores a
// generation-less pane.closed while the pane is still live".
func TestHerdrSessionWatchManagerIgnoresGenerationLessPaneClosed(t *testing.T) {
	harness := openWatchHarness(t)
	handled := &watchHandledLog{}
	subscribed := &watchPaneIDLog{}
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					paneIDs []string,
				) (daemon.HerdrWatchEventStream, error) {
					if subscribed.add(paneIDs) == 1 {
						return newWatchStream(
							ctx, true, true, watchPaneEvent(watchPaneID, "pane.closed"),
						), nil
					}
					return newWatchStream(ctx, true, true), nil
				},
			}, nil
		},
		index: &watchIndexFake{
			onHandleHerdrEvent: func(
				input observability.HandleHerdrEventInput,
			) (*observability.AgentEventHandlingResult, error) {
				handled.add(watchRecord(input.Event))
				return watchEmptyHandle(input)
			},
			onRefreshHerdrSession: func(
				observability.RefreshInput,
			) *observability.RefreshSource {
				return watchSettledSource(watchResult(
					watchAgentRecord(watchPaneID, watchWorkspaceID, observability.AgentStatusWorking),
				))
			},
		},
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the first subscription", func() bool { return subscribed.length() >= 1 })
	time.Sleep(40 * time.Millisecond)
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := handled.length(); got != 0 {
		t.Errorf("handled events = %v, want none", handled.events())
	}
	want := [][]string{{watchPaneID}}
	if got := subscribed.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("subscriptions = %v, want %v", got, want)
	}
}

// TestHerdrSessionWatchManagerAppliesPaneClosedAfterPaneIsGone ports "applies
// pane.closed after refresh shows the pane is gone".
func TestHerdrSessionWatchManagerAppliesPaneClosedAfterPaneIsGone(t *testing.T) {
	harness := openWatchHarness(t)
	handled := &watchHandledLog{}
	refreshes := &watchCounter{}
	manager := watchManagerFor(harness, watchManagerOptions{
		clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
			return &watchFakeClient{
				onSubscribe: func(
					ctx context.Context,
					_ []string,
				) (daemon.HerdrWatchEventStream, error) {
					return newWatchStream(
						ctx, true, true, watchPaneEvent(watchPaneID, "pane.closed"),
					), nil
				},
			}, nil
		},
		index: &watchIndexFake{
			onHandleHerdrEvent: func(
				input observability.HandleHerdrEventInput,
			) (*observability.AgentEventHandlingResult, error) {
				handled.add(watchRecord(input.Event))
				return watchEmptyHandle(input)
			},
			onRefreshHerdrSession: func(
				observability.RefreshInput,
			) *observability.RefreshSource {
				if refreshes.addAndGet() == 1 {
					return watchSettledSource(watchResult(
						watchAgentRecord(
							watchPaneID, watchWorkspaceID, observability.AgentStatusWorking,
						),
					))
				}
				return watchSettledSource(watchResult())
			},
		},
	})
	startWatchManager(t, manager)
	waitForWatch(t, "the pane.closed event to reach the index", func() bool {
		return handled.length() > 0
	})
	if err := manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	found := false
	for _, event := range handled.events() {
		if event["type"] == "pane.closed" && event["pane_id"] == watchPaneID {
			found = true
		}
	}
	if !found {
		t.Errorf("handled events = %v, want the pane.closed of %s", handled.events(), watchPaneID)
	}
}

// ---------------------------------------------------------------------------
// final-audit-regressions cases that guard this manager's own rules
// ---------------------------------------------------------------------------

// watchSnapshotClient is the fake Herdr client the index service refreshes
// through for the identity-filtering cases.
type watchSnapshotClient struct {
	provider func() any
}

func (c *watchSnapshotClient) Close() {}

func (c *watchSnapshotClient) SessionSnapshot() (any, error) { return c.provider(), nil }

// watchSnapshotProvider hands every refresh the snapshot the test set last.
type watchSnapshotProvider struct {
	mu       sync.Mutex
	snapshot any
}

func (p *watchSnapshotProvider) set(snapshot any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot = snapshot
}

func (p *watchSnapshotProvider) current() any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshot
}

func (p *watchSnapshotProvider) factory() observability.HerdrClientFactory {
	return func(string) (observability.HerdrClientLike, error) {
		return &watchSnapshotClient{provider: p.current}, nil
	}
}

// watchSnapshot builds a herdr session snapshot carrying exactly these agents.
func watchSnapshot(agents ...map[string]any) map[string]any {
	entries := make([]any, 0, len(agents))
	for _, agent := range agents {
		entries = append(entries, agent)
	}
	return map[string]any{"snapshot": map[string]any{
		"agents": entries,
		"panes":  []any{},
		"tabs":   []any{},
		"workspaces": []any{map[string]any{
			"focused":      true,
			"label":        "Repo",
			"workspace_id": watchWorkspaceID,
		}},
	}}
}

// refreshTwice runs two refreshes through the real index service and returns the
// indexed agent of each.
func refreshTwice(
	t *testing.T,
	index *observability.AgentIndexService,
	first map[string]any,
	second map[string]any,
	provider *watchSnapshotProvider,
) (observability.AgentIndexRecord, observability.AgentIndexRecord) {
	t.Helper()
	input := observability.RefreshInput{
		HerdrSessionName: watchSessionName,
		SessionDir:       watchSessionDir,
		SocketPath:       watchSocketPath,
	}
	provider.set(watchSnapshot(first))
	firstResult, err := index.RefreshHerdrSessionFast(input)
	if err != nil {
		t.Fatalf("refreshHerdrSessionFast (first): %v", err)
	}
	if len(firstResult.Agents) != 1 {
		t.Fatalf("first refresh indexed %d agents, want 1", len(firstResult.Agents))
	}
	provider.set(watchSnapshot(second))
	secondResult, err := index.RefreshHerdrSessionFast(input)
	if err != nil {
		t.Fatalf("refreshHerdrSessionFast (second): %v", err)
	}
	if len(secondResult.Agents) != 1 {
		t.Fatalf("second refresh indexed %d agents, want 1", len(secondResult.Agents))
	}
	return firstResult.Agents[0], secondResult.Agents[0]
}

// TestHerdrSessionWatchManagerRetainIdentityFiltering ports the
// final-audit-regressions "retain identity filtering" cases. The watch manager
// refreshes through the index service, whose replace-for-session step matches the
// previous row of a pane by terminal identity first and only falls back to the
// pane identity; these two cases pin that filter, which is what decides whether a
// re-appearing pane keeps its agent row.
func TestHerdrSessionWatchManagerRetainIdentityFiltering(t *testing.T) {
	t.Run("removes entries when both pane identities exist and differ", func(t *testing.T) {
		harness := openWatchHarness(t)
		provider := &watchSnapshotProvider{}
		index := watchIndexService(t, harness, provider.factory())
		first, second := refreshTwice(
			t, index,
			map[string]any{
				"agent": "pi", "agent_status": "working",
				"pane_id": watchPaneID, "terminal_id": "term_1", "workspace_id": watchWorkspaceID,
			},
			map[string]any{
				"agent": "pi", "agent_status": "working",
				"pane_id": watchPaneID, "terminal_id": "term_2", "workspace_id": watchWorkspaceID,
			},
			provider,
		)
		if first.ID == second.ID {
			t.Errorf(
				"agent id survived the refresh (%s): both identities exist and differ, "+
					"so the previous entry must not be retained",
				first.ID,
			)
		}
		if second.TerminalID == nil || *second.TerminalID != "term_2" {
			t.Errorf("indexed terminal id = %v, want term_2", second.TerminalID)
		}
	})

	t.Run("keeps entries when either side lacks identity", func(t *testing.T) {
		harness := openWatchHarness(t)
		provider := &watchSnapshotProvider{}
		index := watchIndexService(t, harness, provider.factory())
		// The previous row carries no terminal identity, so the pane identity is
		// the only key available and the entry is kept.
		first, second := refreshTwice(
			t, index,
			map[string]any{
				"agent": "pi", "agent_status": "working",
				"pane_id": watchPaneID, "workspace_id": watchWorkspaceID,
			},
			map[string]any{
				"agent": "pi", "agent_status": "working",
				"pane_id": watchPaneID, "terminal_id": "term_2", "workspace_id": watchWorkspaceID,
			},
			provider,
		)
		if first.ID != second.ID {
			t.Errorf(
				"agent id = %s after the refresh, want the retained %s: one side has no "+
					"terminal identity, so the pane identity keeps the entry",
				second.ID, first.ID,
			)
		}
		// The retained row is the same record updated in place, not a re-insert.
		if second.TerminalID == nil || *second.TerminalID != "term_2" {
			t.Errorf("retained row terminal id = %v, want term_2", second.TerminalID)
		}
	})
}

// TestHerdrSessionWatchManagerFreshSessionResetsReconnectState ports the
// final-audit-regressions "reconnect reset" case ("starts a fresh session with
// reset reconnect state") onto the reconnect state this manager owns: the
// exponential backoff of the watch loop. The TS case checks that a fresh session
// carries no reconnect state at all; the Go watch loop's reconnect state is the
// backoff counter, reported on the reconnect warning, so the port checks that a
// fresh session starts at one and that a reconnect that ended cleanly starts over
// at one instead of doubling.
func TestHerdrSessionWatchManagerFreshSessionResetsReconnectState(t *testing.T) {
	harness := openWatchHarness(t)
	subscribes := &watchCounter{}
	output := captureWatchStderr(t, func() {
		manager := watchManagerFor(harness, watchManagerOptions{
			clientFactory: func(string) (daemon.HerdrSessionWatchClient, error) {
				return &watchFakeClient{
					onSubscribe: func(
						ctx context.Context,
						_ []string,
					) (daemon.HerdrWatchEventStream, error) {
						switch subscribes.addAndGet() {
						case 1:
							// The first subscription of a fresh session fails.
							return &watchFailingStream{err: errors.New("subscription boom")}, nil
						case 2:
							// The reconnect itself ends cleanly: the backoff is reset.
							return newWatchStream(ctx, false, false), nil
						case 3:
							// Failing again must restart the backoff from one.
							return &watchFailingStream{err: errors.New("subscription boom")}, nil
						default:
							// Everything after that parks, so the log keeps exactly the
							// two failures this test reads.
							return newWatchStream(ctx, true, true), nil
						}
					},
				}, nil
			},
			index: &watchIndexFake{
				onHandleHerdrEvent: watchEmptyHandle,
				onRefreshHerdrSession: func(
					observability.RefreshInput,
				) *observability.RefreshSource {
					return watchSettledSource(watchResult())
				},
			},
			reconnectDelayMs: 1,
		})
		startWatchManager(t, manager)
		waitForWatch(t, "the third subscription attempt", func() bool {
			return subscribes.get() >= 3
		})
		if err := manager.Stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	if got := strings.Count(output, "Herdsman Herdr subscription reconnect"); got != 2 {
		t.Errorf("reconnect warnings = %d, want 2:\n%s", got, output)
	}
	// Both failures logged the first backoff step: the fresh session had no state
	// and the clean reconnect in between cleared it again.
	if got := strings.Count(output, "reconnectCount:1 "); got != 2 {
		t.Errorf("reconnectCount:1 warnings = %d, want 2:\n%s", got, output)
	}
	if strings.Contains(output, "reconnectCount:2") {
		t.Errorf("the backoff kept its state across the clean reconnect:\n%s", output)
	}
}
