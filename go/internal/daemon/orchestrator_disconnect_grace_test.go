package daemon_test

// The port of test/integration/orchestrator-disconnect-grace.test.ts.
//
// Losing a connector is not losing a terminal: a Pi pane reconnects within
// moments, and releasing its orchestrator ownership on every hiccup would drop
// the whole session's wake routing. Three timers hold that balance — the
// disconnect grace (the same-terminal reconnect window), the startup grace (the
// same window after a daemon restart) and the heartbeat scan (a terminal that
// stopped answering altogether). Every case drives them through a manual clock,
// so a grace window is advanced instead of waited for, and asserts both halves of
// the promise: an owner is retained inside its window and released the moment the
// window closes.
//
// The last two cases pin the interaction with delivery accounting: reclaiming a
// "delivered" row is keyed on the scope owner, so a terminal inside its grace
// window keeps the event it already delivered, and once its owner is released the
// same event goes back to pending for redelivery.

import (
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The fixture vocabulary of the TS test: the owner and the observer both live in
// wB, and the move target of two cases is wC.
const (
	obsPaneOwner      = "wB:p-owner"
	obsPaneObserver   = "wB:p-observer"
	obsPaneOwnerMoved = "wC:p-owner"
	obsTermOwner      = "term_owner"
	obsTermObserver   = "term_observer"
	obsWorkspaceC     = "wC"
)

func obsGraceScope() observability.AgentScope {
	return observability.AgentScope{HerdrSessionName: obsSessionName, WorkspaceID: obsWorkspaceB}
}

func obsScopeC() observability.AgentScope {
	return observability.AgentScope{HerdrSessionName: obsSessionName, WorkspaceID: obsWorkspaceC}
}

// ---------------------------------------------------------------------------
// Cases
// ---------------------------------------------------------------------------

// TestOrchestratorConnectionGraceHeartbeatTimeout is the TS case "heartbeat
// timeout destroys an idle terminal and releases its owner".
func TestOrchestratorConnectionGraceHeartbeatTimeout(t *testing.T) {
	scheduler := newObsScheduler()
	setup := obsOpenGracedServer(t, scheduler, obsHeartbeat{scanMs: 10, timeoutMs: 30})
	owner := obsDial(t, setup.socketPath)
	obsGraceRegister(t, owner, "owner")
	obsRequest(t, owner, "agent.orchestrator.set", map[string]any{"enabled": true})

	scheduler.advance(40)
	owner.waitForClose(t)
	obsWaitForTerminalDisconnect(t, setup.server)

	if setup.server.IsTerminalConnected(daemon.ConnectedTerminalInput{
		HerdrSessionName: obsSessionName, TerminalID: obsTermOwner,
	}) {
		t.Fatalf("expected the heartbeat timeout to drop the terminal presence")
	}
	if !owner.socketDestroyed() {
		t.Fatalf("expected the heartbeat timeout to destroy the client socket")
	}

	// The grace window starts from the destroy, not from the last heartbeat, and
	// it is honoured to the millisecond.
	scheduler.advance(49)
	assertObsOwnerTerminal(t, setup.orchestrator, obsGraceScope(), obsTermOwner)
	scheduler.advance(1)
	assertObsNoOwner(t, setup.orchestrator, obsGraceScope())
}

// TestOrchestratorConnectionGracePingKeepsTerminalConnected is the TS case
// "agent.ping keeps a terminal connected past heartbeat timeout".
func TestOrchestratorConnectionGracePingKeepsTerminalConnected(t *testing.T) {
	scheduler := newObsScheduler()
	setup := obsOpenGracedServer(t, scheduler, obsHeartbeat{scanMs: 10, timeoutMs: 30})
	owner := obsDial(t, setup.socketPath)
	obsGraceRegister(t, owner, "owner")
	obsRequest(t, owner, "agent.orchestrator.set", map[string]any{"enabled": true})

	for round := 0; round < 3; round++ {
		assertObsSubset(t, obsRequest(t, owner, "agent.ping", map[string]any{}),
			map[string]any{"ok": true})
		scheduler.advance(20)
	}

	if !setup.server.IsTerminalConnected(daemon.ConnectedTerminalInput{
		HerdrSessionName: obsSessionName, TerminalID: obsTermOwner,
	}) {
		t.Fatalf("expected the pings to keep the terminal connected")
	}
	assertObsOwnerTerminal(t, setup.orchestrator, obsGraceScope(), obsTermOwner)
	if owner.socketDestroyed() {
		t.Fatalf("expected the client socket to stay open")
	}
}

// TestOrchestratorConnectionGraceActivityKeepsTerminalConnected is the TS case
// "heartbeat activity keeps a terminal connected": any request, not only a ping,
// refreshes the activity of its terminal.
func TestOrchestratorConnectionGraceActivityKeepsTerminalConnected(t *testing.T) {
	scheduler := newObsScheduler()
	setup := obsOpenGracedServer(t, scheduler, obsHeartbeat{scanMs: 10, timeoutMs: 30})
	owner := obsDial(t, setup.socketPath)
	obsGraceRegister(t, owner, "owner")
	obsRequest(t, owner, "agent.orchestrator.set", map[string]any{"enabled": true})

	scheduler.advance(20)
	obsRequest(t, owner, "agent.orchestrator.get", map[string]any{})
	scheduler.advance(20)

	if !setup.server.IsTerminalConnected(daemon.ConnectedTerminalInput{
		HerdrSessionName: obsSessionName, TerminalID: obsTermOwner,
	}) {
		t.Fatalf("expected the request activity to keep the terminal connected")
	}
	assertObsOwnerTerminal(t, setup.orchestrator, obsGraceScope(), obsTermOwner)
	if owner.socketDestroyed() {
		t.Fatalf("expected the client socket to stay open")
	}
}

// TestOrchestratorConnectionGraceExpiresAbsentOwner is the TS case "expires an
// absent owner and broadcasts after the disconnect grace".
func TestOrchestratorConnectionGraceExpiresAbsentOwner(t *testing.T) {
	scheduler := newObsScheduler()
	setup := obsOpenGracedServer(t, scheduler, obsHeartbeat{})
	owner := obsDial(t, setup.socketPath)
	observer := obsDial(t, setup.socketPath)
	obsGraceRegister(t, owner, "owner")
	obsGraceRegister(t, observer, "observer")
	obsRequest(t, owner, "agent.orchestrator.set", map[string]any{"enabled": true})
	for _, client := range []*obsRPCClient{owner, observer} {
		if _, err := client.waitForNotification("agent.orchestrator.changed"); err != nil {
			t.Fatalf("waiting for the claim broadcast: %v", err)
		}
	}

	owner.close()
	obsWaitForTerminalDisconnect(t, setup.server)
	scheduler.advance(49)
	assertObsOwnerTerminal(t, setup.orchestrator, obsGraceScope(), obsTermOwner)
	scheduler.advance(1)
	assertObsNoOwner(t, setup.orchestrator, obsGraceScope())

	change := obsNotificationChange(t, observer)
	assertObsSubset(t, change, map[string]any{"reason": "disconnected"})
	assertObsSubset(t, obsAt(t, change, "current"), map[string]any{"owner": nil})
}

// TestOrchestratorConnectionGraceReconnectPreservesOwnership is the TS case
// "same-terminal reconnect and overlapping sockets preserve ownership".
func TestOrchestratorConnectionGraceReconnectPreservesOwnership(t *testing.T) {
	scheduler := newObsScheduler()
	setup := obsOpenGracedServer(t, scheduler, obsHeartbeat{})
	first := obsDial(t, setup.socketPath)
	obsGraceRegister(t, first, "owner")
	obsRequest(t, first, "agent.orchestrator.set", map[string]any{"enabled": true})

	// An overlapping socket on the same terminal means the disconnect that
	// follows is not a disconnect at all, so no grace timer is even armed.
	overlap := obsDial(t, setup.socketPath)
	obsGraceRegister(t, overlap, "owner-replacement")
	first.close()
	obsSocketTick()
	scheduler.advance(100)
	assertObsOwnerTerminal(t, setup.orchestrator, obsGraceScope(), obsTermOwner)

	// And a reconnect that lands inside the window cancels the armed timer.
	overlap.close()
	obsSocketTick()
	scheduler.advance(25)
	replacement := obsDial(t, setup.socketPath)
	obsGraceRegister(t, replacement, "owner-after-gap")
	scheduler.advance(100)
	assertObsOwnerTerminal(t, setup.orchestrator, obsGraceScope(), obsTermOwner)
}

// TestOrchestratorConnectionGraceExpiryFollowsMovedOwner is the TS case
// "disconnect expiry follows an owner moved during the grace window": the release
// is keyed on the terminal, so it follows the ownership to the workspace the pane
// moved to while the window was running.
func TestOrchestratorConnectionGraceExpiryFollowsMovedOwner(t *testing.T) {
	scheduler := newObsScheduler()
	setup := obsOpenGracedServer(t, scheduler, obsHeartbeat{})
	owner := obsDial(t, setup.socketPath)
	obsGraceRegister(t, owner, "owner")
	obsRequest(t, owner, "agent.orchestrator.set", map[string]any{"enabled": true})

	owner.close()
	obsWaitForTerminalDisconnect(t, setup.server)
	obsReconcile(t, setup.obsServerSetup, obsMoveOwnerAgent(t, setup.harness), obsSessionName)
	assertObsOwnerTerminal(t, setup.orchestrator, obsScopeC(), obsTermOwner)

	scheduler.advance(50)
	assertObsNoOwner(t, setup.orchestrator, obsScopeC())
}

// TestOrchestratorConnectionGraceStartupReconnect is the TS case "startup grace
// preserves a matching reconnect and expires an absent owner".
func TestOrchestratorConnectionGraceStartupReconnect(t *testing.T) {
	scheduler := newObsScheduler()

	// A persisted owner that reconnects inside the startup window keeps its
	// ownership, and the window neither extends nor repeats.
	first := obsNewGraceFixture(t)
	firstOrchestrator := first.harness.orchestrator()
	obsClaimOwner(t, firstOrchestrator)
	firstServer := obsStartGracedServer(t, first, firstOrchestrator, scheduler, obsHeartbeat{})

	scheduler.advance(99)
	assertObsHasOwner(t, firstOrchestrator, obsGraceScope())
	returning := obsDial(t, first.socketPath)
	obsGraceRegister(t, returning, "returning")
	scheduler.advance(1)
	assertObsOwnerTerminal(t, firstOrchestrator, obsGraceScope(), obsTermOwner)
	returning.close()
	if err := firstServer.server.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// The same window with no reconnect releases the owner — and follows it to the
	// workspace the pane moved to in the meantime.
	absent := obsNewGraceFixture(t)
	absentOrchestrator := absent.harness.orchestrator()
	obsClaimOwner(t, absentOrchestrator)
	absentServer := obsStartGracedServer(t, absent, absentOrchestrator, scheduler, obsHeartbeat{})
	obsReconcile(t, absentServer, obsMoveOwnerAgent(t, absent.harness), obsSessionName)
	assertObsOwnerTerminal(t, absentOrchestrator, obsScopeC(), obsTermOwner)

	scheduler.advance(100)
	assertObsNoOwner(t, absentOrchestrator, obsScopeC())
	if err := absentServer.server.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// TestOrchestratorConnectionGraceStopCancelsStartupTimers is the TS case
// "intentional stop cancels startup timers without clearing persisted owner": an
// intentional shutdown must not read as a disconnected fleet.
func TestOrchestratorConnectionGraceStopCancelsStartupTimers(t *testing.T) {
	scheduler := newObsScheduler()
	fixture := obsNewGraceFixture(t)
	orchestrator := fixture.harness.orchestrator()
	obsClaimOwner(t, orchestrator)
	setup := obsStartGracedServer(t, fixture, orchestrator, scheduler, obsHeartbeat{})

	if err := setup.server.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	scheduler.advance(1000)
	assertObsOwnerTerminal(t, orchestrator, obsGraceScope(), obsTermOwner)
}

// TestOrchestratorConnectionGraceDeliveredWithinWindow is the TS case "delivered
// events are not shortcut-reclaimed while disconnected terminal is within grace
// window".
func TestOrchestratorConnectionGraceDeliveredWithinWindow(t *testing.T) {
	scheduler := newObsScheduler()
	setup := obsOpenGracedServer(t, scheduler, obsHeartbeat{})
	owner := obsDial(t, setup.socketPath)
	obsGraceRegister(t, owner, "owner")
	obsRequest(t, owner, "agent.orchestrator.set", map[string]any{"enabled": true})

	event := obsReserveWakeEvent(t, setup.harness)
	owner.close()
	obsSocketTick()

	scheduler.advance(20)
	// The scope owner is retained for the whole grace window.
	assertObsOwnerTerminal(t, setup.orchestrator, obsGraceScope(), obsTermOwner)

	observer := obsDial(t, setup.socketPath)
	obsGraceRegister(t, observer, "observer")
	status, deliveredTo := obsEventDeliveryRow(t, setup.harness, event.ID)
	if status != string(observability.AgentEventStatusDelivered) {
		t.Fatalf("expected the event to stay delivered inside the grace window, got %q", status)
	}
	if deliveredTo == nil || *deliveredTo != obsTermOwner {
		t.Fatalf("expected the event to stay delivered to %s, got %v", obsTermOwner, deliveredTo)
	}
}

// TestOrchestratorConnectionGraceDeliveredAfterExpiry is the TS case "delivered
// events are shortcut-reclaimed once disconnect grace expires and scope owner is
// cleared".
func TestOrchestratorConnectionGraceDeliveredAfterExpiry(t *testing.T) {
	scheduler := newObsScheduler()
	setup := obsOpenGracedServer(t, scheduler, obsHeartbeat{})
	owner := obsDial(t, setup.socketPath)
	obsGraceRegister(t, owner, "owner")
	obsRequest(t, owner, "agent.orchestrator.set", map[string]any{"enabled": true})

	event := obsReserveWakeEvent(t, setup.harness)
	owner.close()
	obsWaitForTerminalDisconnect(t, setup.server)

	scheduler.advance(60)
	// The grace expired and the server cleared the scope owner.
	assertObsNoOwner(t, setup.orchestrator, obsGraceScope())

	observer := obsDial(t, setup.socketPath)
	obsGraceRegister(t, observer, "observer")
	status, deliveredTo := obsEventDeliveryRow(t, setup.harness, event.ID)
	if status != string(observability.AgentEventStatusPending) {
		t.Fatalf("expected the expired delivery to go back to pending, got %q", status)
	}
	if deliveredTo != nil {
		t.Fatalf("expected no delivered_to_terminal_id after reclaim, got %q", *deliveredTo)
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// obsHeartbeat carries the optional heartbeat overrides of the TS openServer().
// The zero value means "omit the option", which the server reads as its own
// default.
type obsHeartbeat struct {
	scanMs    int
	timeoutMs int
}

// obsGraceFixture is the TS createHarness(): a migrated harness whose running
// session already indexes the owner and the observer pane, plus the socket path
// the server will listen on. A fresh temp directory per fixture lets one test run
// two servers without sharing a path or a database.
type obsGraceFixture struct {
	harness    *obsHarness
	socketPath string
}

func obsNewGraceFixture(t *testing.T) obsGraceFixture {
	t.Helper()
	harness := openObsHarness(t)
	harness.upsertRunningSession(t)
	if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents: []db.HerdrAgentLike{
			{
				"agent":        "pi",
				"pane_id":      obsPaneOwner,
				"terminal_id":  obsTermOwner,
				"workspace_id": obsWorkspaceB,
			},
			{
				"agent":        "pi",
				"pane_id":      obsPaneObserver,
				"terminal_id":  obsTermObserver,
				"workspace_id": obsWorkspaceB,
			},
		},
		HerdrSessionName: obsSessionName,
	}); err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	return obsGraceFixture{
		harness:    harness,
		socketPath: filepath.Join(t.TempDir(), "rpc.sock"),
	}
}

// obsMoveOwnerAgent is the TS moveOwnerAgent(): the owner pane now lives in wC,
// and the observer stays behind in wB.
func obsMoveOwnerAgent(t *testing.T, harness *obsHarness) []observability.AgentIndexRecord {
	t.Helper()
	return harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent":        "pi",
			"pane_id":      obsPaneOwnerMoved,
			"terminal_id":  obsTermOwner,
			"workspace_id": obsWorkspaceC,
		},
		{
			"agent":        "pi",
			"pane_id":      obsPaneObserver,
			"terminal_id":  obsTermObserver,
			"workspace_id": obsWorkspaceB,
		},
	})
}

// obsGraceSetup is the TS openServer() return value: the started server and its
// orchestrator, plus the harness behind them.
type obsGraceSetup struct {
	obsServerSetup
	harness *obsHarness
}

// obsOpenGracedServer is the TS openServer(): harness, orchestrator and server.
func obsOpenGracedServer(
	t *testing.T,
	scheduler *obsScheduler,
	heartbeat obsHeartbeat,
) obsGraceSetup {
	t.Helper()
	fixture := obsNewGraceFixture(t)
	orchestrator := fixture.harness.orchestrator()
	return obsGraceSetup{
		obsServerSetup: obsStartGracedServer(t, fixture, orchestrator, scheduler, heartbeat),
		harness:        fixture.harness,
	}
}

// obsStartGracedServer mirrors startServer(): the server runs its whole timing
// machinery — now, both timer kinds, and therefore every grace window and the
// heartbeat scan — on the manual clock, while the unix socket stays real. The
// 50 ms disconnect grace and 100 ms startup grace are the two numbers every
// assertion below is written against.
func obsStartGracedServer(
	t *testing.T,
	fixture obsGraceFixture,
	orchestrator *observability.AgentOrchestratorService,
	scheduler *obsScheduler,
	heartbeat obsHeartbeat,
) obsServerSetup {
	t.Helper()
	history := newObsHistory(fixture.harness, "")
	server := daemon.NewObservabilityRpcServer(daemon.ObservabilityRpcServerOptions{
		ClearInterval:           scheduler.clearInterval,
		ClearTimeout:            scheduler.clear,
		Context:                 obsContextService(fixture.harness, history),
		DisconnectGraceMs:       50,
		HeartbeatScanIntervalMs: heartbeat.scanMs,
		HeartbeatTimeoutMs:      heartbeat.timeoutMs,
		History:                 history,
		Now:                     scheduler.now,
		Orchestrator:            orchestrator,
		SetInterval:             scheduler.setInterval,
		SetTimeout:              scheduler.set,
		SocketPath:              fixture.socketPath,
		StartupReconnectGraceMs: 100,
		Stores: daemon.ObservabilityAgentStores{
			AgentEvents:     fixture.harness.agentEvents,
			Agents:          fixture.harness.agents,
			HerdrSessions:   fixture.harness.herdrSessions,
			HerdrWorkspaces: fixture.harness.herdrWorkspaces,
		},
		Warn: quietObsWarn,
	})
	if err := server.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return obsServerSetup{server: server, orchestrator: orchestrator, socketPath: fixture.socketPath}
}

// obsGraceRegister mirrors register(): the observer subscriber watches the
// observer pane, every other subscriber the owner pane.
func obsGraceRegister(t *testing.T, client *obsRPCClient, subscriberID string) map[string]any {
	t.Helper()
	paneID := obsPaneOwner
	if subscriberID == "observer" {
		paneID = obsPaneObserver
	}
	return obsRegister(t, client, paneID, subscriberID, obsWorkspaceB)
}

// obsClaimOwner is the TS orchestrator.claim({...scope, paneId, terminalId}) of
// the two startup-grace cases, which seed persisted ownership before the server
// exists.
func obsClaimOwner(t *testing.T, orchestrator *observability.AgentOrchestratorService) {
	t.Helper()
	if _, err := orchestrator.Claim(observability.OrchestratorClaimInput{
		AgentScope: obsGraceScope(),
		PaneID:     obsPaneOwner,
		TerminalID: obsTermOwner,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
}

// obsReserveWakeEvent appends the wake event of the two delivered-event cases and
// reserves it for the owner terminal, which is what marks it delivered.
func obsReserveWakeEvent(t *testing.T, harness *obsHarness) observability.AgentEventRecord {
	t.Helper()
	// The TS case looks the owner agent up by terminal id in the workspace index;
	// findByTerminal is the same lookup.
	ownerAgent, err := harness.agents.FindByTerminal(db.FindAgentByTerminalInput{
		HerdrSessionName: obsSessionName,
		TerminalID:       obsTermOwner,
	})
	if err != nil {
		t.Fatalf("findByTerminal(%s): %v", obsTermOwner, err)
	}
	if ownerAgent == nil {
		t.Fatalf("owner agent not found")
	}
	paneID := obsPaneOwner
	observerTerminal := obsTermObserver
	workspaceID := obsWorkspaceB
	event, err := harness.agentEvents.Append(db.AppendAgentEventInput{
		AgentID:          &ownerAgent.ID,
		HerdrSessionName: obsSessionName,
		PaneID:           &paneID,
		Payload:          map[string]any{"message": "wake"},
		TerminalID:       &observerTerminal,
		Type:             observability.AgentEventIdle,
		WorkspaceID:      &workspaceID,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	reserved, err := harness.agentEvents.ReservePending(obsTermOwner, 10, []int64{event.ID})
	if err != nil {
		t.Fatalf("reservePending: %v", err)
	}
	if len(reserved) != 1 {
		t.Fatalf("expected exactly one reserved event, got %d", len(reserved))
	}
	if reserved[0].Status == nil || *reserved[0].Status != observability.AgentEventStatusDelivered {
		t.Fatalf("expected the reserved event to be delivered, got %v", reserved[0].Status)
	}
	if reserved[0].DeliveredToTerminalID == nil || *reserved[0].DeliveredToTerminalID != obsTermOwner {
		t.Fatalf("expected the reserved event to be delivered to %s, got %v",
			obsTermOwner, reserved[0].DeliveredToTerminalID)
	}
	return event
}

// obsEventDeliveryRow reads the two delivery columns straight from the database,
// like the TS `select status, delivered_to_terminal_id ...` probe.
func obsEventDeliveryRow(
	t *testing.T,
	harness *obsHarness,
	eventID int64,
) (string, *string) {
	t.Helper()
	var status string
	var deliveredTo *string
	if err := harness.sqlite.QueryRow(
		"select status, delivered_to_terminal_id from agent_events where id = ?", eventID,
	).Scan(&status, &deliveredTo); err != nil {
		t.Fatalf("read delivery row %d: %v", eventID, err)
	}
	return status, deliveredTo
}

// obsWaitForTerminalDisconnect is the TS deterministic barrier: it returns once
// the server has actually processed the close of the owner's socket. That
// observation implies the grace timer exists, because the server drops the
// terminal from its registry and arms the disconnect timer in one synchronous
// block; a fixed sleep carries no such implication and could advance a clock whose
// grace timer was never armed, orphaning it and leaving the owner in place.
func obsWaitForTerminalDisconnect(t *testing.T, server *daemon.ObservabilityRpcServer) {
	t.Helper()
	obsWaitFor(t, "the owner terminal to leave the presence registry", func() bool {
		return !server.IsTerminalConnected(daemon.ConnectedTerminalInput{
			HerdrSessionName: obsSessionName, TerminalID: obsTermOwner,
		})
	})
}

// ---------------------------------------------------------------------------
// Owner assertions
// ---------------------------------------------------------------------------

func assertObsOwnerTerminal(
	t *testing.T,
	orchestrator *observability.AgentOrchestratorService,
	scope observability.AgentScope,
	terminalID string,
) {
	t.Helper()
	state, err := orchestrator.Status(scope)
	if err != nil {
		t.Fatalf("status(%s/%s): %v", scope.HerdrSessionName, scope.WorkspaceID, err)
	}
	if state == nil || state.Owner == nil {
		t.Fatalf("scope %s/%s: expected owner %s, got none",
			scope.HerdrSessionName, scope.WorkspaceID, terminalID)
	}
	if state.Owner.TerminalID != terminalID {
		t.Fatalf("scope %s/%s: expected owner %s, got %s",
			scope.HerdrSessionName, scope.WorkspaceID, terminalID, state.Owner.TerminalID)
	}
}

func assertObsNoOwner(
	t *testing.T,
	orchestrator *observability.AgentOrchestratorService,
	scope observability.AgentScope,
) {
	t.Helper()
	state, err := orchestrator.Status(scope)
	if err != nil {
		t.Fatalf("status(%s/%s): %v", scope.HerdrSessionName, scope.WorkspaceID, err)
	}
	if state != nil && state.Owner != nil {
		t.Fatalf("scope %s/%s: expected no owner, got %+v",
			scope.HerdrSessionName, scope.WorkspaceID, *state.Owner)
	}
}

func assertObsHasOwner(
	t *testing.T,
	orchestrator *observability.AgentOrchestratorService,
	scope observability.AgentScope,
) {
	t.Helper()
	state, err := orchestrator.Status(scope)
	if err != nil {
		t.Fatalf("status(%s/%s): %v", scope.HerdrSessionName, scope.WorkspaceID, err)
	}
	if state == nil || state.Owner == nil {
		t.Fatalf("scope %s/%s: expected an owner", scope.HerdrSessionName, scope.WorkspaceID)
	}
}

// ---------------------------------------------------------------------------
// ManualScheduler
// ---------------------------------------------------------------------------

// obsScheduler is the TS ManualScheduler: a virtual clock with one-shot and
// repeating timers, advanced by explicit amounts. The server is handed every
// timer seam, so the grace and heartbeat machinery runs on this clock while the
// sockets stay real.
//
// Callbacks run with the scheduler lock released. The server calls now/set/clear
// from its own goroutines while holding its state lock, so running a callback
// under this lock would invert the lock order against a server goroutine that is
// about to arm a timer.
type obsScheduler struct {
	mu     sync.Mutex
	nextID int
	nowMs  int64
	timers map[int]*obsScheduled
}

// obsScheduled is one armed timer; a positive interval marks a repeating timer.
type obsScheduled struct {
	armed    bool
	callback func()
	due      int64
	interval int64
	id       int
}

func newObsScheduler() *obsScheduler {
	return &obsScheduler{nextID: 1, timers: map[int]*obsScheduled{}}
}

func (s *obsScheduler) now() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nowMs
}

// set arms a one-shot timer and returns its handle.
func (s *obsScheduler) set(callback func(), delayMs int) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.arm(callback, int64(delayMs), 0)
}

// setInterval arms a repeating timer and returns its handle.
func (s *obsScheduler) setInterval(callback func(), delayMs int) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.arm(callback, int64(delayMs), int64(delayMs))
}

func (s *obsScheduler) arm(callback func(), delayMs, intervalMs int64) int {
	id := s.nextID
	s.nextID++
	s.timers[id] = &obsScheduled{
		armed:    true,
		callback: callback,
		due:      s.nowMs + delayMs,
		interval: intervalMs,
		id:       id,
	}
	return id
}

// clear disarms a one-shot handle, clearInterval a repeating one.
func (s *obsScheduler) clear(handle any)         { s.disarm(handle) }
func (s *obsScheduler) clearInterval(handle any) { s.disarm(handle) }

func (s *obsScheduler) disarm(handle any) {
	id, ok := handle.(int)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if timer, tracked := s.timers[id]; tracked {
		timer.armed = false
		delete(s.timers, id)
	}
}

// advance moves the virtual clock and runs everything that became due, due order
// first, repeating until a pass yields nothing: a callback that arms another timer
// inside the same window is served by the same advance, like the outer loop of the
// TS scheduler.
func (s *obsScheduler) advance(milliseconds int) {
	s.mu.Lock()
	s.nowMs += int64(milliseconds)
	s.mu.Unlock()

	for {
		s.mu.Lock()
		due := make([]*obsScheduled, 0, len(s.timers))
		for _, timer := range s.timers {
			if timer.armed && timer.due <= s.nowMs {
				due = append(due, timer)
			}
		}
		sort.SliceStable(due, func(left, right int) bool {
			if due[left].due != due[right].due {
				return due[left].due < due[right].due
			}
			return due[left].id < due[right].id
		})
		if len(due) == 0 {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		for _, timer := range due {
			if !s.fire(timer) {
				continue
			}
			timer.callback()
		}
	}
}

// fire re-checks one timer that is about to run and moves it out of the way: a
// repeating timer is rescheduled, a one-shot is disarmed. It reports false when an
// earlier callback of the same pass already disarmed the timer, which is what
// keeps a reconnect that cancels a grace timer from having that grace fire anyway.
func (s *obsScheduler) fire(timer *obsScheduled) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !timer.armed {
		return false
	}
	if timer.interval > 0 {
		timer.due += timer.interval
		return true
	}
	timer.armed = false
	delete(s.timers, timer.id)
	return true
}
