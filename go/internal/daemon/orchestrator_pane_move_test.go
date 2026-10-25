package daemon_test

// The port of test/integration/orchestrator-pane-move.test.ts.
//
// Pane movement is what the orchestrator ownership model exists for: a Herdr
// scan rewrites the agent index underneath live connections, and the daemon must
// follow the pane — relocate every presence that tracked it, move the claimed
// ownership to the destination workspace while keeping each scope's own ack
// cursor, release the source scope, and route worker events by the new scope.
// Both cases drive that through a real unix socket, a migrated database and the
// real orchestrator service, and assert what each connection was notified about.

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The fixture vocabulary of the TS test: two workspaces and three panes. The
// Herdr socket path and the session name live with the shared harness.
const (
	obsWorkspaceA    = "wA"
	obsWorkspaceB    = "wB"
	obsTermA         = "term_a"
	obsTermB         = "term_b"
	obsTermPeer      = "term_peer"
	obsPaneAMoved    = "wB:p-a-moved"
	obsPaneANew      = "wB:p-a-new"
	obsPiSessionFile = "/tmp/pi-session.jsonl"
)

func obsScopeA() observability.AgentScope {
	return observability.AgentScope{HerdrSessionName: obsSessionName, WorkspaceID: obsWorkspaceA}
}

func obsScopeB() observability.AgentScope {
	return observability.AgentScope{HerdrSessionName: obsSessionName, WorkspaceID: obsWorkspaceB}
}

// obsServerSetup is the TS openServer() return value: the started server, the
// orchestrator it shares with the test, and the socket the clients connect to.
type obsServerSetup struct {
	server       *daemon.ObservabilityRpcServer
	orchestrator *observability.AgentOrchestratorService
	socketPath   string
}

// openObsServer mirrors openServer(): the real agent-history service, the real
// context and orchestrator services, and the four stores the TS constructor is
// handed. Every option keeps its TS default except the warn sink, which the test
// silences because the connector-binding warning is expected on a fixture socket
// that Herdr does not own.
func openObsServer(t *testing.T, harness *obsHarness) obsServerSetup {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "rpc.sock")
	history := newObsHistory(harness, "")
	orchestrator := harness.orchestrator()
	server := daemon.NewObservabilityRpcServer(daemon.ObservabilityRpcServerOptions{
		Context:      obsContextService(harness, history),
		History:      history,
		Orchestrator: orchestrator,
		SocketPath:   socketPath,
		Stores: daemon.ObservabilityAgentStores{
			AgentEvents:     harness.agentEvents,
			Agents:          harness.agents,
			HerdrSessions:   harness.herdrSessions,
			HerdrWorkspaces: harness.herdrWorkspaces,
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
	return obsServerSetup{server: server, orchestrator: orchestrator, socketPath: socketPath}
}

// obsContextService is the TS `new AgentContextService({ history, stores })`.
func obsContextService(
	harness *obsHarness,
	history *obsHistory,
) *observability.AgentContextService {
	return observability.NewAgentContextService(struct {
		History observability.AgentHistoryServiceLike
		Stores  struct {
			AgentContextSnapshots observability.AgentContextSnapshotStoreLike
			Agents                observability.AgentContextAgentStoreLike
		}
	}{
		History: history,
		Stores: struct {
			AgentContextSnapshots observability.AgentContextSnapshotStoreLike
			Agents                observability.AgentContextAgentStoreLike
		}{
			AgentContextSnapshots: harness.agentContextSnapshots,
			Agents:                harness.agents,
		},
	})
}

// seedObsAgents is the TS seedAgents(): the running Herdr session plus the three
// indexed panes.
func seedObsAgents(t *testing.T, harness *obsHarness) []observability.AgentIndexRecord {
	t.Helper()
	harness.upsertRunningSession(t)
	return harness.replaceAgents(t, []db.HerdrAgentLike{
		obsSnapshot(obsTermA, "wA:p-a", obsWorkspaceA),
		obsSnapshot(obsTermPeer, "wA:p-peer", obsWorkspaceA),
		obsSnapshot(obsTermB, "wB:p-b", obsWorkspaceB),
	})
}

// obsReconcile is the TS server.reconcileAgentLocations(...).
func obsReconcile(
	t *testing.T,
	setup obsServerSetup,
	agents []observability.AgentIndexRecord,
	herdrSessionName string,
) {
	t.Helper()
	if err := setup.server.ReconcileAgentLocations(daemon.AgentLocationReconcile{
		Agents:           agents,
		HerdrSessionName: herdrSessionName,
	}); err != nil {
		t.Fatalf("reconcileAgentLocations: %v", err)
	}
}

// obsRegister is the TS register(): a Pi connector for one pane of one
// workspace, answered with its connection state.
func obsRegister(
	t *testing.T,
	client *obsRPCClient,
	paneID string,
	subscriberID string,
	workspaceID string,
) map[string]any {
	t.Helper()
	result := obsRequest(t, client, "agent.orchestrator.register", map[string]any{
		"herdrSocketPath": obsHerdrSocketPath,
		"paneId":          paneID,
		"sessionRef": map[string]any{
			"agent":  "pi",
			"kind":   "path",
			"source": "herdr:pi",
			"value":  obsPiSessionFile,
		},
		"subscriberId":   subscriberID,
		"subscriberKind": "pi",
		"workspaceId":    workspaceID,
	})
	return result
}

// obsRequest asserts a successful RPC and returns its decoded result object.
func obsRequest(t *testing.T, client *obsRPCClient, method string, params any) map[string]any {
	t.Helper()
	result, err := client.request(method, params)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	object, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("%s: expected an object result, got %T", method, result)
	}
	return object
}

// obsAppendEvent is the TS appendEvent(): one deliverable agent.done row, with
// the two optional overrides the test uses.
func obsAppendEvent(
	t *testing.T,
	harness *obsHarness,
	terminalID string,
	workspaceID string,
	paneIDOverride *string,
	eventTerminalID *string,
) observability.AgentEventRecord {
	t.Helper()
	agent, err := harness.agents.FindByTerminal(db.FindAgentByTerminalInput{
		HerdrSessionName: obsSessionName,
		TerminalID:       terminalID,
	})
	if err != nil {
		t.Fatalf("findByTerminal(%s): %v", terminalID, err)
	}
	var agentID *string
	paneID := paneIDOverride
	if agent != nil {
		agentID = &agent.ID
		if paneID == nil {
			paneID = &agent.PaneID
		}
	}
	resolvedTerminal := eventTerminalID
	if resolvedTerminal == nil {
		resolvedTerminal = &terminalID
	}
	event, err := harness.agentEvents.Append(db.AppendAgentEventInput{
		AgentID:          agentID,
		HerdrSessionName: obsSessionName,
		PaneID:           paneID,
		Payload:          map[string]any{},
		TerminalID:       resolvedTerminal,
		Type:             observability.AgentEventDone,
		WorkspaceID:      &workspaceID,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return event
}

// obsOtherSessionAgent is the TS otherSessionAgent(): a finished index record of
// another Herdr session, handed straight to reconcileAgentLocations.
func obsOtherSessionAgent(
	terminalID, paneID, workspaceID string,
) observability.AgentIndexRecord {
	return observability.AgentIndexRecord{
		Agent:            obsString("pi"),
		AgentStatus:      observability.AgentStatusWorking,
		HerdrSessionName: "other",
		ID:               "ag_other",
		PaneID:           paneID,
		TerminalID:       &terminalID,
		WorkspaceID:      workspaceID,
	}
}

// TestOrchestratorPaneMoveUpdatesPresence is the TS case "updates non-owner
// presence from terminal identity and retains absent terminals".
func TestOrchestratorPaneMoveUpdatesPresence(t *testing.T) {
	harness := openObsHarness(t)
	setup := openObsServer(t, harness)
	seedObsAgents(t, harness)
	piA := obsDial(t, setup.socketPath)
	obsRegister(t, piA, "wA:p-a", "pi-a", obsWorkspaceA)

	moved := harness.replaceAgents(t, []db.HerdrAgentLike{
		obsSnapshot(obsTermA, obsPaneAMoved, obsWorkspaceB),
		obsSnapshot(obsTermPeer, "wA:p-peer", obsWorkspaceA),
		obsSnapshot(obsTermB, "wB:p-b", obsWorkspaceB),
	})
	obsReconcile(t, setup, moved, obsSessionName)

	// The presence followed its terminal across the workspace boundary.
	state := obsRequest(t, piA, "agent.orchestrator.get", map[string]any{})
	assertObsSubset(t, obsAt(t, state, "presence"), map[string]any{
		"herdrSessionName": obsSessionName,
		"paneId":           obsPaneAMoved,
		"terminalId":       obsTermA,
		"workspaceId":      obsWorkspaceB,
	})

	// And the claim lands on the workspace the terminal now belongs to.
	claimed := obsRequest(t, piA, "agent.orchestrator.set", map[string]any{"enabled": true})
	assertObsSubset(t, obsAt(t, claimed, "state"), map[string]any{
		"herdrSessionName": obsSessionName,
		"workspaceId":      obsWorkspaceB,
	})

	// A scan with no agents keeps the known location: absent terminals are
	// retained until an explicit disconnect or release.
	obsReconcile(t, setup, nil, obsSessionName)
	state = obsRequest(t, piA, "agent.orchestrator.get", map[string]any{})
	assertObsSubset(t, obsAt(t, state, "presence"), map[string]any{
		"paneId":      obsPaneAMoved,
		"workspaceId": obsWorkspaceB,
	})

	// The same terminal id inside another Herdr session is a different agent and
	// must not relocate this presence.
	obsReconcile(t, setup, []observability.AgentIndexRecord{
		obsOtherSessionAgent(obsTermA, "wX:p1", "wX"),
	}, "other")
	state = obsRequest(t, piA, "agent.orchestrator.get", map[string]any{})
	assertObsSubset(t, obsAt(t, state, "presence"), map[string]any{
		"paneId":      obsPaneAMoved,
		"workspaceId": obsWorkspaceB,
	})
}

// TestOrchestratorPaneMoveAcrossWorkspaces is the TS case "moves an owner across
// workspaces, replaces the destination, and routes by new scope".
func TestOrchestratorPaneMoveAcrossWorkspaces(t *testing.T) {
	harness := openObsHarness(t)
	setup := openObsServer(t, harness)
	seedObsAgents(t, harness)

	piA := obsDial(t, setup.socketPath)
	sourcePeer := obsDial(t, setup.socketPath)
	piB := obsDial(t, setup.socketPath)
	obsRegister(t, piA, "wA:p-a", "pi-a", obsWorkspaceA)
	obsRegister(t, sourcePeer, "wA:p-peer", "pi-peer", obsWorkspaceA)
	obsRegister(t, piB, "wB:p-b", "pi-b", obsWorkspaceB)

	sourceBaseline := obsAppendEvent(t, harness, obsTermPeer, obsWorkspaceA, obsString("wA:p-peer-old"), nil)
	targetBaseline := obsAppendEvent(t, harness, obsTermB, obsWorkspaceB, obsString("wB:p-b-old"), nil)

	obsRequest(t, piA, "agent.orchestrator.set", map[string]any{"enabled": true})
	obsRequest(t, piB, "agent.orchestrator.set", map[string]any{"enabled": true})
	for _, client := range []*obsRPCClient{piA, sourcePeer, piB} {
		if _, err := client.waitForNotification("agent.orchestrator.changed"); err != nil {
			t.Fatalf("waiting for the claim broadcast: %v", err)
		}
	}

	sourceAck := obsAppendEvent(t, harness, obsTermPeer, obsWorkspaceA, nil, nil)
	targetAck := obsAppendEvent(t, harness, obsTermB, obsWorkspaceB, nil, obsString(obsTermA))
	obsRequest(t, piA, "agent.notifications.ack", map[string]any{"eventId": sourceAck.ID})
	obsRequest(t, piB, "agent.notifications.ack", map[string]any{"eventId": targetAck.ID})
	piA.clearNotifications()
	sourcePeer.clearNotifications()
	piB.clearNotifications()

	moved := harness.replaceAgents(t, []db.HerdrAgentLike{
		obsSnapshot(obsTermA, obsPaneAMoved, obsWorkspaceB),
		obsSnapshot(obsTermPeer, "wA:p-peer", obsWorkspaceA),
		obsSnapshot(obsTermB, "wB:p-b", obsWorkspaceB),
	})
	obsReconcile(t, setup, moved, obsSessionName)

	// The source scope is ownerless with its own cursor, the destination keeps
	// its own cursor and now belongs to the moved terminal.
	assertObsScope(t, setup, obsScopeA(), sourceAck.ID, nil)
	assertObsScope(t, setup, obsScopeB(), targetAck.ID, &observability.AgentOrchestratorOwner{
		PaneID:     obsPaneAMoved,
		TerminalID: obsTermA,
	})
	assertObsLess(t, "source baseline vs source ack", sourceBaseline.ID, sourceAck.ID)
	assertObsLess(t, "target baseline vs target ack", targetBaseline.ID, targetAck.ID)

	// The source peer is told its workspace lost the orchestrator.
	sourceChange := obsNotificationChange(t, sourcePeer)
	assertObsSubset(t, obsAt(t, sourceChange, "current"), map[string]any{
		"owner":       nil,
		"workspaceId": obsWorkspaceA,
	})
	assertObsSubset(t, sourceChange, map[string]any{"reason": "moved"})

	// The destination peer sees the ownership replace the terminal it had.
	destinationChange := obsNotificationChange(t, piB)
	assertObsSubset(t, obsAt(t, destinationChange, "current"), map[string]any{
		"workspaceId": obsWorkspaceB,
	})
	assertObsSubset(t, obsAt(t, destinationChange, "current", "owner"), map[string]any{
		"terminalId": obsTermA,
	})
	assertObsSubset(t, obsAt(t, destinationChange, "previous", "owner"), map[string]any{
		"terminalId": obsTermB,
	})
	assertObsSubset(t, destinationChange, map[string]any{"reason": "moved"})

	// The moved owner sees its own new scope.
	ownerChange := obsNotificationChange(t, piA)
	assertObsSubset(t, obsAt(t, ownerChange, "current"), map[string]any{
		"workspaceId": obsWorkspaceB,
	})
	assertObsSubset(t, obsAt(t, ownerChange, "current", "owner"), map[string]any{
		"terminalId": obsTermA,
	})
	state := obsRequest(t, piA, "agent.orchestrator.get", map[string]any{})
	assertObsSubset(t, obsAt(t, state, "presence"), map[string]any{
		"paneId":      obsPaneAMoved,
		"workspaceId": obsWorkspaceB,
	})

	// A repeat scan over the same index is a no-op: nothing is re-broadcast.
	piA.clearNotifications()
	piB.clearNotifications()
	sourcePeer.clearNotifications()
	obsReconcile(t, setup, moved, obsSessionName)
	obsSocketTick()
	assertObsNoNotifications(t, "pi-a", piA)
	assertObsNoNotifications(t, "pi-b", piB)
	assertObsNoNotifications(t, "source peer", sourcePeer)

	// A destination worker event now reaches the moved owner.
	destinationEvent := obsAppendEvent(t, harness, obsTermB, obsWorkspaceB, nil, nil)
	setup.server.PublishAgentEvent(destinationEvent)
	routed := obsWaitNotification(t, piA, "agent.event")
	if got := int64(obsAt(t, routed, "params", "event", "id").(float64)); got != destinationEvent.ID {
		t.Fatalf("expected the routed agent.event %s, got %s",
			strconv.FormatInt(destinationEvent.ID, 10), strconv.FormatInt(got, 10))
	}

	// The delivery query agrees: the moved owner may deliver exactly that event
	// for its new scope.
	nextDeliverable, err := harness.agentEvents.NextDeliverableAfter(db.NextDeliverableAfterInput{
		AfterEventID:     targetAck.ID,
		HerdrSessionName: obsSessionName,
		OwnerTerminalID:  obsTermA,
		WorkspaceID:      obsWorkspaceB,
	})
	if err != nil {
		t.Fatalf("nextDeliverableAfter: %v", err)
	}
	if nextDeliverable == nil {
		t.Fatalf("expected a next deliverable event for the moved scope")
	}
	if nextDeliverable.ID != destinationEvent.ID {
		t.Fatalf("expected the next deliverable event to be the routed one, got %s",
			strconv.FormatInt(nextDeliverable.ID, 10))
	}

	acked := obsRequest(t, piA, "agent.notifications.ack", map[string]any{"eventId": destinationEvent.ID})
	assertObsSubset(t, acked, map[string]any{"acknowledged": true})
	assertObsSubset(t, obsAt(t, acked, "state"), map[string]any{
		"ackedEventId": destinationEvent.ID,
	})

	// The owner's own worker output is not a wake-up for itself.
	selfEvent := obsAppendEvent(t, harness, obsTermA, obsWorkspaceB, nil, nil)
	setup.server.PublishAgentEvent(selfEvent)
	obsSocketTick()
	assertObsNoNotifications(t, "pi-a", piA)

	// A pane move inside the owner's own workspace keeps the ownership and the
	// cursor, and reports the new pane.
	sameWorkspace := harness.replaceAgents(t, []db.HerdrAgentLike{
		obsSnapshot(obsTermA, obsPaneANew, obsWorkspaceB),
		obsSnapshot(obsTermPeer, "wA:p-peer", obsWorkspaceA),
		obsSnapshot(obsTermB, "wB:p-b", obsWorkspaceB),
	})
	obsReconcile(t, setup, sameWorkspace, obsSessionName)
	movedChange := obsNotificationChange(t, piA)
	assertObsSubset(t, obsAt(t, movedChange, "current"), map[string]any{
		"workspaceId": obsWorkspaceB,
	})
	assertObsSubset(t, obsAt(t, movedChange, "current", "owner"), map[string]any{
		"paneId": obsPaneANew,
	})
	assertObsSubset(t, movedChange, map[string]any{"reason": "moved"})
	assertObsScope(t, setup, obsScopeB(), destinationEvent.ID, &observability.AgentOrchestratorOwner{
		PaneID:     obsPaneANew,
		TerminalID: obsTermA,
	})
}

// obsNotificationChange takes the next orchestrator change notification.
func obsNotificationChange(t *testing.T, client *obsRPCClient) map[string]any {
	t.Helper()
	return obsAt(t, obsWaitNotification(t, client, "agent.orchestrator.changed"), "params", "change").(map[string]any)
}

// obsWaitNotification is waitForNotification with the failure spelled out.
func obsWaitNotification(t *testing.T, client *obsRPCClient, method string) map[string]any {
	t.Helper()
	message, err := client.waitForNotification(method)
	if err != nil {
		t.Fatalf("waiting for %s: %v", method, err)
	}
	return message
}

// assertObsNoNotifications is `expect(client.notifications).toEqual([])`.
func assertObsNoNotifications(t *testing.T, who string, client *obsRPCClient) {
	t.Helper()
	if count := client.notificationCount(); count != 0 {
		t.Fatalf("%s: expected no notification, got %d", who, count)
	}
}

// assertObsScope reads the persisted scope state and checks its cursor and owner.
func assertObsScope(
	t *testing.T,
	setup obsServerSetup,
	scope observability.AgentScope,
	ackedEventID int64,
	owner *observability.AgentOrchestratorOwner,
) {
	t.Helper()
	state, err := setup.orchestrator.Status(scope)
	if err != nil {
		t.Fatalf("status(%s/%s): %v", scope.HerdrSessionName, scope.WorkspaceID, err)
	}
	if state == nil {
		t.Fatalf("scope %s/%s has no orchestrator state", scope.HerdrSessionName, scope.WorkspaceID)
	}
	if state.AckedEventID != ackedEventID {
		t.Fatalf("scope %s/%s: expected ackedEventId %s, got %s",
			scope.HerdrSessionName, scope.WorkspaceID,
			strconv.FormatInt(ackedEventID, 10), strconv.FormatInt(state.AckedEventID, 10))
	}
	switch {
	case owner == nil && state.Owner != nil:
		t.Fatalf("scope %s/%s: expected no owner, got %+v",
			scope.HerdrSessionName, scope.WorkspaceID, *state.Owner)
	case owner != nil && state.Owner == nil:
		t.Fatalf("scope %s/%s: expected owner %+v, got none",
			scope.HerdrSessionName, scope.WorkspaceID, *owner)
	case owner != nil && *state.Owner != *owner:
		t.Fatalf("scope %s/%s: expected owner %+v, got %+v",
			scope.HerdrSessionName, scope.WorkspaceID, *owner, *state.Owner)
	}
}

// assertObsLess is `expect(left).toBeLessThan(right)`.
func assertObsLess(t *testing.T, what string, left, right int64) {
	t.Helper()
	if !(left < right) {
		t.Fatalf("%s: expected %s < %s", what,
			strconv.FormatInt(left, 10), strconv.FormatInt(right, 10))
	}
}

// obsAt walks a decoded JSON object the way toMatchObject walks a path.
func obsAt(t *testing.T, value any, steps ...string) any {
	t.Helper()
	current := value
	for _, step := range steps {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("expected an object while reading %q, got %T", step, current)
		}
		next, present := object[step]
		if !present {
			t.Fatalf("missing key %q in %#v", step, object)
		}
		current = next
	}
	return current
}

// assertObsSubset is the toMatchObject subset check over a decoded object. The
// expected values are the literals the assertions are written with; decoded JSON
// numbers are float64, so integers are compared through float64.
func assertObsSubset(t *testing.T, object any, want map[string]any) {
	t.Helper()
	typed, ok := object.(map[string]any)
	if !ok {
		t.Fatalf("expected an object to match against, got %T", object)
	}
	for key, expected := range want {
		actual, present := typed[key]
		if !present {
			t.Fatalf("missing key %q in %#v", key, typed)
		}
		if !obsJSONEqual(actual, expected) {
			t.Fatalf("%s: expected %v, got %v", key, expected, actual)
		}
	}
}

func obsJSONEqual(actual any, expected any) bool {
	switch want := expected.(type) {
	case nil:
		return actual == nil
	case int:
		number, ok := actual.(float64)
		return ok && number == float64(want)
	case int64:
		number, ok := actual.(float64)
		return ok && number == float64(want)
	default:
		return actual == expected
	}
}
