package daemon_test

// The port of test/integration/observability-rpc.test.ts: the observability RPC
// server driven end to end over a real unix socket against a migrated database.
// Every case keeps the TS arrangement — the real agent-history service rooted at
// a throwaway home directory, the real context and orchestrator services, the
// four stores — and asserts the decoded JSON wire objects the TS assertions
// match with toMatchObject. All fixtures live under t.TempDir(), so the
// production home and the running daemon are never touched.
//
// The scaffolding shared with the other observability-server ports (the
// database harness, the JSON-line notification client, the agent-history
// adapter) lives in observability_server_frame_limit_test.go.
//
// Three TS shapes need a Go-shaped equivalent rather than a literal copy:
//
//   - vi.spyOn(console, "warn") on the agent-history discovery warning becomes
//     captureStderr (the same seam the other daemon ports use), and the server's
//     own console.warn becomes the Warn option of the fixture.
//   - The liveHistory doubles of two cases read options.preferredRef on `read`.
//     The TS server hands read only {forceDiscovery, limit}, so the Go read
//     options carry no preferred-ref field at all: "a persisted ref never
//     reaches the read path" is asserted structurally there, and explicitly on
//     the resolveCompactHistory side where the option does exist.
//   - vi.waitFor(retry the claim) becomes obsWaitFor polling the same request;
//     a rejected claim is side-effect free, so polling is safe.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/agenthistory"
	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// ---------------------------------------------------------------------------
// openServer / openServerWithoutClient
// ---------------------------------------------------------------------------

// obsHistoryService is what an injected history double must implement: the RPC
// server's history port plus the wider port the context service reads. TS hands
// the same object to both, so a double has to satisfy both as well.
type obsHistoryService interface {
	daemon.ObservabilityHistoryService
	observability.AgentHistoryServiceLike
}

// obsRPCFixture is the TS openServer()/openServerWithoutClient() return value.
// `client` is only set by the variant that dials the ObservabilityRpcClient.
type obsRPCFixture struct {
	client     *daemon.ObservabilityRpcClient
	dir        string
	harness    *obsHarness
	server     *daemon.ObservabilityRpcServer
	socketPath string
}

type obsRPCServerOptions struct {
	// harness lets a case that needs the stores before the server starts (the
	// stubbed-history cases) open the database first.
	harness   *obsHarness
	history   obsHistoryService
	peerPidOf func(conn net.Conn) (int, bool)
	// resolvePaneIdentity is the live Herdr pane identity seam.
	resolvePaneIdentity func(input daemon.ResolvePaneIdentityInput) (herdr.HerdrPaneIdentity, error)
	turnCompletions     *observability.TurnCompletionRegistry
	// warn replaces the server's console.warn sink; the default drops it.
	warn func(message string, payload any)
}

func obsOpenRPCServerWithoutClient(t *testing.T, options obsRPCServerOptions) obsRPCFixture {
	t.Helper()
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "rpc.sock")
	harness := options.harness
	if harness == nil {
		harness = openObsHarness(t)
	}
	// TS: options.history ?? createAgentHistoryService({cache, homeDir: dir}).
	var history obsHistoryService = options.history
	if history == nil {
		history = newObsHistory(harness, dir)
	}
	server := daemon.NewObservabilityRpcServer(daemon.ObservabilityRpcServerOptions{
		Context:             obsRPCContextService(harness, history),
		History:             history,
		Orchestrator:        harness.orchestrator(),
		PeerPidOf:           options.peerPidOf,
		ResolvePaneIdentity: options.resolvePaneIdentity,
		SocketPath:          socketPath,
		Stores:              obsRPCStores(harness),
		TurnCompletions:     options.turnCompletions,
		Warn:                obsRPCWarn(options),
	})
	if err := server.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return obsRPCFixture{dir: dir, harness: harness, server: server, socketPath: socketPath}
}

func obsOpenRPCServer(t *testing.T, options obsRPCServerOptions) obsRPCFixture {
	t.Helper()
	fixture := obsOpenRPCServerWithoutClient(t, options)
	client, err := daemon.NewObservabilityRpcClient(
		daemon.ObservabilityRpcClientOptions{SocketPath: fixture.socketPath},
	)
	if err != nil {
		t.Fatalf("dial %s: %v", fixture.socketPath, err)
	}
	t.Cleanup(client.Close)
	fixture.client = client
	return fixture
}

// obsRPCWarn keeps the fixture quiet unless a case wants to read the warnings.
func obsRPCWarn(options obsRPCServerOptions) func(string, any) {
	if options.warn != nil {
		return options.warn
	}
	return quietObsWarn
}

// obsRPCStores is the TS `stores` record of the constructor.
func obsRPCStores(harness *obsHarness) daemon.ObservabilityAgentStores {
	return daemon.ObservabilityAgentStores{
		AgentEvents:     harness.agentEvents,
		Agents:          harness.agents,
		HerdrSessions:   harness.herdrSessions,
		HerdrWorkspaces: harness.herdrWorkspaces,
	}
}

// obsRPCContextService is the TS `new AgentContextService({ history, stores })`.
func obsRPCContextService(
	harness *obsHarness,
	history observability.AgentHistoryServiceLike,
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

// ---------------------------------------------------------------------------
// Client-side assertions
// ---------------------------------------------------------------------------

// obsCall is the TS `await expect(client.request(...)).resolves` half: it fails
// the test on an RPC error and returns the decoded result object.
func obsCall(
	t *testing.T,
	client *daemon.ObservabilityRpcClient,
	method string,
	params any,
) map[string]any {
	t.Helper()
	result, err := client.Call(method, params)
	if err != nil {
		t.Fatalf("%s: unexpected RPC failure: %v", method, err)
	}
	object, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("%s: expected an object result, got %T", method, result)
	}
	return object
}

// obsCallError is the `rejects.toThrow(...)` half: it fails when the call
// succeeds and hands back the error message.
func obsCallError(
	t *testing.T,
	client *daemon.ObservabilityRpcClient,
	method string,
	params any,
) string {
	t.Helper()
	result, err := client.Call(method, params)
	if err == nil {
		t.Fatalf("%s: expected a rejection, got %#v", method, result)
	}
	return err.Error()
}

// assertObsContains is `expect(message).toContain(needle)`.
func assertObsContains(t *testing.T, what, message, needle string) {
	t.Helper()
	if !strings.Contains(message, needle) {
		t.Fatalf("%s: expected %q to contain %q", what, message, needle)
	}
}

// assertObsNotContains is `expect(message).not.toContain(needle)`.
func assertObsNotContains(t *testing.T, what, message, needle string) {
	t.Helper()
	if strings.Contains(message, needle) {
		t.Fatalf("%s: expected %q not to contain %q", what, message, needle)
	}
}

// assertObsExact is `expect(value).toEqual({...})` over a decoded object: every
// key must be present and only the listed keys may be.
func assertObsExact(t *testing.T, what string, object any, want map[string]any) {
	t.Helper()
	typed, ok := object.(map[string]any)
	if !ok {
		t.Fatalf("%s: expected an object, got %T", what, object)
	}
	if len(typed) != len(want) {
		t.Fatalf("%s: expected the keys %v, got %v", what, obsKeys(want), obsKeys(typed))
	}
	assertObsSubset(t, typed, want)
}

func obsKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}

// obsList is the `agents` / `events` array of a decoded result.
func obsList(t *testing.T, what string, object map[string]any, key string) []any {
	t.Helper()
	items, ok := object[key].([]any)
	if !ok {
		t.Fatalf("%s: expected %s to be an array, got %T", what, key, object[key])
	}
	return items
}

// obsExactlyOneList is `toMatchObject({key: [expect.objectContaining(...)]})`:
// the array must hold exactly the one entry the case talks about.
func obsExactlyOneList(t *testing.T, what string, object map[string]any, key string) map[string]any {
	t.Helper()
	items := obsList(t, what, object, key)
	if len(items) != 1 {
		t.Fatalf("%s: expected exactly one %s, got %d: %v", what, key, len(items), items)
	}
	entry, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("%s: expected %s[0] to be an object, got %T", what, key, items[0])
	}
	return entry
}

// obsAgentMessages is the `messages` array of an agent.read result.
func obsAgentMessages(t *testing.T, result map[string]any) []any {
	t.Helper()
	agent, ok := result["agent"].(map[string]any)
	if !ok {
		t.Fatalf("expected an agent object, got %T", result["agent"])
	}
	messages, ok := agent["messages"].([]any)
	if !ok {
		t.Fatalf("expected messages to be an array, got %T", agent["messages"])
	}
	return messages
}

// assertObsNoHistory is `toMatchObject({agent: {historyRef: null, messages: []})`.
func assertObsNoHistory(t *testing.T, what string, result map[string]any) {
	t.Helper()
	agent, ok := result["agent"].(map[string]any)
	if !ok {
		t.Fatalf("%s: expected an agent object, got %T", what, result["agent"])
	}
	if ref, present := agent["historyRef"]; !present || ref != nil {
		t.Fatalf("%s: expected historyRef null, got %#v (present=%v)", what, ref, present)
	}
	messages, ok := agent["messages"].([]any)
	if !ok {
		t.Fatalf("%s: expected messages to be an array, got %T", what, agent["messages"])
	}
	if len(messages) != 0 {
		t.Fatalf("%s: expected no message, got %v", what, messages)
	}
}

// obsEmptyPiCompactHistory is the observability form of the TS
// `emptyCompactHistory("pi-jsonl")`.
func obsEmptyPiCompactHistory() observability.CompactAgentHistory {
	source := obsPiJSONLSource
	return observability.CompactAgentHistory{Source: &source}
}

// obsFindSeededAgent is the TS `harness.agents.findByPane(...)` plus its
// `if (!agent) throw new Error("missing seeded agent")`.
func obsFindSeededAgent(
	t *testing.T,
	harness *obsHarness,
	paneID string,
) observability.AgentIndexRecord {
	t.Helper()
	agent, err := harness.agents.FindByPane(db.FindAgentByPaneInput{
		HerdrSessionName: obsSessionName,
		PaneID:           paneID,
	})
	if err != nil {
		t.Fatalf("findByPane(%s): %v", paneID, err)
	}
	if agent == nil {
		t.Fatalf("missing seeded agent")
	}
	return *agent
}

// obsCountWarnLines is `warn.mock.calls.filter(([message]) =>
// String(message).includes(needle)).length` of a captured stderr.
func obsCountWarnLines(captured, needle string) int {
	return strings.Count(captured, needle)
}

// obsRegisterWith is the TS register() payload: a Pi connector for one pane of
// one workspace, with the Herdr socket path, subscriber id and kind spelled out
// by the cases that vary them.
func obsRegisterWith(
	socketPath string,
	paneID string,
	subscriberID string,
	subscriberKind string,
	workspaceID string,
) map[string]any {
	return map[string]any{
		"herdrSocketPath": socketPath,
		"paneId":          paneID,
		"sessionRef": map[string]any{
			"agent":  "pi",
			"kind":   "path",
			"source": "herdr:pi",
			"value":  obsPiSessionFile,
		},
		"subscriberId":   subscriberID,
		"subscriberKind": subscriberKind,
		"workspaceId":    workspaceID,
	}
}

// obsRegisterParams is the TS register() call of the first-half cases: the
// default socket, subscriber and workspace, only the pane varies.
func obsRegisterParams(paneID string) map[string]any {
	return obsRegisterWith(obsHerdrSocketPath, paneID, "pi-session", "pi", obsWorkspaceB)
}

// ---------------------------------------------------------------------------
// Seeds (the TS seed* helpers)
// ---------------------------------------------------------------------------

// obsSeedAgent is the TS seedAgent(): the running Herdr session plus the one Pi
// pane whose session file is an empty jsonl under dir.
func obsSeedAgent(t *testing.T, harness *obsHarness, baseDir string) {
	t.Helper()
	sessionDir := filepath.Join(baseDir, ".pi", "agent", "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", sessionDir, err)
	}
	sessionPath := filepath.Join(sessionDir, "agent-session.jsonl")
	if err := os.WriteFile(sessionPath, []byte(""), 0o600); err != nil {
		t.Fatalf("write %s: %v", sessionPath, err)
	}
	harness.upsertRunningSession(t)
	harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent": "pi",
			"agent_session": map[string]any{
				"kind":   "path",
				"source": "pi-jsonl",
				"value":  sessionPath,
			},
			"agent_status": "idle",
			"cwd":          "/repo",
			"pane_id":      "wB:p1",
			"terminal_id":  "term_1",
			"workspace_id": obsWorkspaceB,
		},
	})
}

// obsSeedTargetAgents is the TS seedTargetAgents(): three panes whose names
// collide with a terminal id, another agent kind and (optionally) an agent id.
func obsSeedTargetAgents(t *testing.T, harness *obsHarness, firstAgentID string) {
	t.Helper()
	claudeName := "term_1"
	if firstAgentID != "" {
		claudeName = firstAgentID
	}
	harness.upsertRunningSession(t)
	harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent":        "codex",
			"agent_status": "idle",
			"name":         "reviewer",
			"pane_id":      "wB:p1",
			"terminal_id":  "term_1",
			"workspace_id": obsWorkspaceB,
		},
		{
			"agent":        "reviewer",
			"agent_status": "idle",
			"name":         "codex",
			"pane_id":      "wB:p2",
			"terminal_id":  "term_2",
			"workspace_id": obsWorkspaceB,
		},
		{
			"agent":        "claude",
			"agent_status": "idle",
			"name":         claudeName,
			"pane_id":      "wB:p3",
			"terminal_id":  "term_3",
			"workspace_id": obsWorkspaceB,
		},
	})
}

// obsSeedAmbiguousTargetAgents is the TS seedAmbiguousTargetAgents().
func obsSeedAmbiguousTargetAgents(t *testing.T, harness *obsHarness) {
	t.Helper()
	harness.upsertRunningSession(t)
	harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent":        "codex",
			"agent_status": "idle",
			"name":         "shared",
			"pane_id":      "wB:p1",
			"terminal_id":  "term_1",
			"workspace_id": obsWorkspaceB,
		},
		{
			"agent":        "reviewer",
			"agent_status": "idle",
			"name":         "shared",
			"pane_id":      "wB:p2",
			"terminal_id":  "term_2",
			"workspace_id": obsWorkspaceB,
		},
		{
			"agent":        "shared",
			"agent_status": "idle",
			"name":         "other",
			"pane_id":      "wB:p3",
			"terminal_id":  "term_3",
			"workspace_id": obsWorkspaceB,
		},
	})
}

// obsAppendRoutedEvent is the TS appendRoutedEvent(): one deliverable agent.done
// row for the terminal's agent, used to drive publishAgentEvent.
func obsAppendRoutedEvent(
	t *testing.T,
	harness *obsHarness,
	terminalID string,
	workspaceID string,
) observability.AgentEventRecord {
	t.Helper()
	return obsAppendEvent(t, harness, terminalID, workspaceID, nil, nil)
}

// ---------------------------------------------------------------------------
// agent.list / agent.get / agent.read / agent.events / orchestrator / ack
// ---------------------------------------------------------------------------

// TestObservabilityRpcServerServesAgentMethodsOverJSONL is the TS case "serves
// agent methods over JSONL".
func TestObservabilityRpcServerServesAgentMethodsOverJSONL(t *testing.T) {
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{})
	obsSeedAgent(t, fixture.harness, fixture.dir)

	listed := obsCall(t, fixture.client, "agent.list", map[string]any{"workspaceId": obsWorkspaceB})
	assertObsSubset(t, obsExactlyOneList(t, "agent.list", listed, "agents"), map[string]any{
		"agent":  "pi",
		"paneId": "wB:p1",
	})

	fetched := obsCall(t, fixture.client, "agent.get", map[string]any{
		"target": "pi", "workspaceId": obsWorkspaceB,
	})
	assertObsSubset(t, obsAt(t, fetched, "agent"), map[string]any{
		"agent":  "pi",
		"paneId": "wB:p1",
	})

	read := obsCall(t, fixture.client, "agent.read", map[string]any{
		"limit": 10, "target": "pi", "workspaceId": obsWorkspaceB,
	})
	if messages := obsAgentMessages(t, read); len(messages) != 0 {
		t.Fatalf("agent.read: expected no message, got %d: %v", len(messages), messages)
	}

	event, err := fixture.harness.agentEvents.Append(db.AppendAgentEventInput{
		HerdrSessionName: obsSessionName,
		Payload:          map[string]any{"to": "idle"},
		Type:             observability.AgentEventIdle,
		WorkspaceID:      obsString(obsWorkspaceB),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	events := obsCall(t, fixture.client, "agent.events", map[string]any{"workspaceId": obsWorkspaceB})
	assertObsSubset(t, obsExactlyOneList(t, "agent.events", events, "events"), map[string]any{
		"id":   event.ID,
		"type": "agent.idle",
	})

	registered := obsCall(
		t,
		fixture.client,
		"agent.orchestrator.register",
		obsRegisterParams("wB:p1"),
	)
	if pending := obsList(t, "agent.orchestrator.register", registered, "events"); len(pending) != 0 {
		t.Fatalf("register: expected no pending event, got %d: %v", len(pending), pending)
	}
	assertObsSubset(t, obsAt(t, registered, "presence"), map[string]any{"terminalId": "term_1"})
	state, present := registered["state"]
	if !present || state != nil {
		t.Fatalf("register: expected state null, got %#v (present=%v)", state, present)
	}

	claimed := obsCall(t, fixture.client, "agent.orchestrator.set", map[string]any{"enabled": true})
	assertObsSubset(t, claimed, map[string]any{"changed": true})
	assertObsSubset(t, obsAt(t, claimed, "state", "owner"), map[string]any{"terminalId": "term_1"})

	acked := obsCall(t, fixture.client, "agent.notifications.ack", map[string]any{"eventId": event.ID})
	assertObsSubset(t, acked, map[string]any{"acknowledged": true})

	// The removed telemetry method is spelled dynamically in TS so no leftover
	// reference in the source can satisfy the assertion.
	removedMethod := strings.Join([]string{"agent", "telemetry"}, ".")
	assertObsContains(t, removedMethod, obsCallError(t, fixture.client, removedMethod, map[string]any{
		"event": map[string]any{}, "workspaceId": obsWorkspaceB,
	}), "Unknown method: "+removedMethod)
	assertObsContains(t, "legacy.method", obsCallError(t, fixture.client, "legacy.method", map[string]any{}), "Unknown method")
}

// TestObservabilityRpcServerPingWithoutSideEffects is the TS case "answers
// agent.ping without side effects and rejects extra params".
func TestObservabilityRpcServerPingWithoutSideEffects(t *testing.T) {
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{})
	assertObsExact(t, "agent.ping", obsCall(t, fixture.client, "agent.ping", map[string]any{}),
		map[string]any{"ok": true})
	assertObsContains(t, "agent.ping with extra params", obsCallError(t, fixture.client, "agent.ping", map[string]any{
		"unexpected": true,
	}), "Invalid RPC params")
}

// obsLetWaiterRegister mirrors the synchronous ordering of the TS test, where
// waitForSignal installs its waiter before the request that must not answer it
// is even written to the socket.
func obsLetWaiterRegister() { time.Sleep(100 * time.Millisecond) }

// TestObservabilityRpcServerRecordsTurnCompletionSignals is the TS case
// "records pi turn completion signals and validates their params".
func TestObservabilityRpcServerRecordsTurnCompletionSignals(t *testing.T) {
	registry := observability.NewTurnCompletionRegistry(observability.WithTurnSettimeout(1_000))
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{turnCompletions: registry})
	obsSeedAgent(t, fixture.harness, fixture.dir)
	obsCall(t, fixture.client, "agent.orchestrator.register", obsRegisterParams("wB:p1"))

	assertObsExact(t, "agent.turn.completed", obsCall(t, fixture.client, "agent.turn.completed", map[string]any{
		"confirmed":        true,
		"herdrSessionName": obsSessionName,
		"paneId":           "wB:p1",
		"terminalId":       "term_1",
		"workspaceId":      obsWorkspaceB,
	}), map[string]any{"accepted": true})

	otherWaiter := make(chan observability.TurnCompletionWaitResult, 1)
	go func() {
		otherWaiter <- registry.WaitForSignal(observability.WaitForSignalInput{
			HerdrSessionName: obsSessionName,
			RecordedAfterMs:  0,
			TerminalID:       "term_2",
		})
	}()
	obsLetWaiterRegister()

	// The signal is recorded against the connection's presence, so the second
	// call names another terminal but is stored under term_1: the term_2 waiter
	// times out with a not-received result.
	assertObsExact(t, "agent.turn.completed (other terminal)", obsCall(t, fixture.client, "agent.turn.completed", map[string]any{
		"confirmed":        true,
		"herdrSessionName": obsSessionName,
		"paneId":           "wB:p2",
		"terminalId":       "term_2",
		"workspaceId":      obsWorkspaceB,
	}), map[string]any{"accepted": true})

	select {
	case got := <-otherWaiter:
		if got != (observability.TurnCompletionWaitResult{}) {
			t.Fatalf("term_2 waiter: expected {confirmed:false, received:false}, got %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("term_2 waiter never settled")
	}

	recorded := registry.WaitForSignal(observability.WaitForSignalInput{
		HerdrSessionName: obsSessionName,
		RecordedAfterMs:  0,
		TerminalID:       "term_1",
	})
	if !recorded.Received || !recorded.Confirmed || recorded.HasExpectedText {
		t.Fatalf("term_1 waiter: expected {confirmed:true, received:true}, got %+v", recorded)
	}

	assertObsContains(t, "agent.turn.completed without a scope", obsCallError(t, fixture.client, "agent.turn.completed", map[string]any{
		"confirmed": true,
	}), "Invalid RPC params")
}

// TestObservabilityRpcServerTurnCompletionExpectedText is the TS case "accepts
// agent.turn.completed with expectedText and delivers it to waiters".
func TestObservabilityRpcServerTurnCompletionExpectedText(t *testing.T) {
	registry := observability.NewTurnCompletionRegistry(observability.WithTurnSettimeout(3_000))
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{turnCompletions: registry})
	obsSeedAgent(t, fixture.harness, fixture.dir)
	obsCall(t, fixture.client, "agent.orchestrator.register", obsRegisterParams("wB:p1"))

	assertObsExact(t, "agent.turn.completed", obsCall(t, fixture.client, "agent.turn.completed", map[string]any{
		"confirmed":        true,
		"expectedText":     "final answer",
		"herdrSessionName": obsSessionName,
		"paneId":           "wB:p1",
		"terminalId":       "term_1",
		"workspaceId":      obsWorkspaceB,
	}), map[string]any{"accepted": true})

	waiter := registry.WaitForSignal(observability.WaitForSignalInput{
		HerdrSessionName: obsSessionName,
		RecordedAfterMs:  0,
		TerminalID:       "term_1",
	})
	if !waiter.Received || !waiter.Confirmed || !waiter.HasExpectedText || waiter.ExpectedText != "final answer" {
		t.Fatalf("waiter: expected {confirmed:true, expectedText:final answer, received:true}, got %+v", waiter)
	}
}

// TestObservabilityRpcServerStructuredAckError is the TS case "serializes
// structured acknowledgement errors through the real RPC server and client".
func TestObservabilityRpcServerStructuredAckError(t *testing.T) {
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{})
	obsSeedAgent(t, fixture.harness, fixture.dir)
	obsCall(t, fixture.client, "agent.orchestrator.register", obsRegisterParams("wB:p1"))

	_, err := fixture.client.Call("agent.notifications.ack", map[string]any{"eventId": 99_999})
	var rpcError *daemon.ObservabilityRpcError
	if !errors.As(err, &rpcError) {
		t.Fatalf("agent.notifications.ack: expected an ObservabilityRpcError, got %#v", err)
	}
	if rpcError.Code == nil || *rpcError.Code != "ORCHESTRATOR_NOT_OWNER" {
		t.Fatalf("agent.notifications.ack: expected code ORCHESTRATOR_NOT_OWNER, got %#v", rpcError.Code)
	}
	if rpcError.Retryable == nil || *rpcError.Retryable {
		t.Fatalf("agent.notifications.ack: expected retryable false, got %#v", rpcError.Retryable)
	}
	if rpcError.Message != "Only the current orchestrator can acknowledge notifications" {
		t.Fatalf("agent.notifications.ack: unexpected message %q", rpcError.Message)
	}
}

// TestObservabilityRpcServerResolvesIdentifiersBeforeNames is the TS case
// "resolves identifiers before live names and live names before kinds".
func TestObservabilityRpcServerResolvesIdentifiersBeforeNames(t *testing.T) {
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{})
	obsSeedTargetAgents(t, fixture.harness, "")
	listed := obsCall(t, fixture.client, "agent.list", map[string]any{"workspaceId": obsWorkspaceB})
	var firstID string
	for _, entry := range obsList(t, "agent.list", listed, "agents") {
		agent, ok := entry.(map[string]any)
		if !ok || agent["paneId"] != "wB:p1" {
			continue
		}
		firstID, _ = agent["id"].(string)
	}
	if firstID == "" {
		t.Fatalf("Expected first agent id")
	}
	// Re-seed with the id as the third pane's name: an id, a terminal id and two
	// agent kinds now all collide with names.
	obsSeedTargetAgents(t, fixture.harness, firstID)

	for _, testCase := range []struct {
		target string
		want   map[string]any
	}{
		{target: "term_1", want: map[string]any{"paneId": "wB:p1"}},
		{target: firstID, want: map[string]any{"paneId": "wB:p1"}},
		{target: "reviewer", want: map[string]any{"name": "reviewer", "paneId": "wB:p1"}},
		{target: "codex", want: map[string]any{"name": "codex", "paneId": "wB:p2"}},
		{target: "claude", want: map[string]any{"agent": "claude", "paneId": "wB:p3"}},
	} {
		result := obsCall(t, fixture.client, "agent.get", map[string]any{
			"target": testCase.target, "workspaceId": obsWorkspaceB,
		})
		agent := obsAt(t, result, "agent")
		agentObject, ok := agent.(map[string]any)
		if !ok {
			t.Fatalf("agent.get %s: expected an agent object, got %T", testCase.target, agent)
		}
		for key, expected := range testCase.want {
			if !obsJSONEqual(agentObject[key], expected) {
				t.Fatalf("agent.get %s: expected %s %v, got %v",
					testCase.target, key, expected, agentObject[key])
			}
		}
	}
}

// assertObsEmptyList is `toEqual({key: []})`: the result holds only that key and
// its array is empty.
func assertObsEmptyList(t *testing.T, what string, object map[string]any, key string) {
	t.Helper()
	if len(object) != 1 {
		t.Fatalf("%s: expected only the key %q, got %v", what, key, obsKeys(object))
	}
	if items := obsList(t, what, object, key); len(items) != 0 {
		t.Fatalf("%s: expected no %s, got %v", what, key, items)
	}
}

// TestObservabilityRpcServerReportsAmbiguityAtHighestPriority is the TS case
// "reports ambiguity at the highest matching target priority".
func TestObservabilityRpcServerReportsAmbiguityAtHighestPriority(t *testing.T) {
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{})
	obsSeedAmbiguousTargetAgents(t, fixture.harness)

	nameError := obsCallError(t, fixture.client, "agent.get", map[string]any{
		"target": "shared", "workspaceId": obsWorkspaceB,
	})
	assertObsContains(t, "ambiguous name", nameError,
		"pane=wB:p1 terminal=term_1 name=shared agent=codex")
	assertObsContains(t, "ambiguous name", nameError,
		"pane=wB:p2 terminal=term_2 name=shared agent=reviewer")
	// The third pane matches only on its agent kind, the lowest priority: the
	// report stops at the highest priority that matched.
	assertObsNotContains(t, "ambiguous name", nameError, "pane=wB:p3")

	fixture.harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent":        "pi",
			"agent_status": "idle",
			"pane_id":      "wB:p1",
			"terminal_id":  "term_1",
			"workspace_id": obsWorkspaceB,
		},
		{
			"agent":        "pi",
			"agent_status": "idle",
			"pane_id":      "wB:p2",
			"terminal_id":  "term_2",
			"workspace_id": obsWorkspaceB,
		},
	})
	assertObsContains(t, "ambiguous kind", obsCallError(t, fixture.client, "agent.get", map[string]any{
		"target": "pi", "workspaceId": obsWorkspaceB,
	}), "pane=wB:p1 terminal=term_1 name=unnamed agent=pi; session=default workspace=wB pane=wB:p2 terminal=term_2 name=unnamed agent=pi")
}

// TestObservabilityRpcServerHidesStoppedSessionAgents is the TS case "hides
// retained agents after their Herdr session stops".
func TestObservabilityRpcServerHidesStoppedSessionAgents(t *testing.T) {
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{})
	obsSeedAgent(t, fixture.harness, fixture.dir)

	if err := fixture.harness.herdrSessions.MarkStoppedMissingFrom([]string{}); err != nil {
		t.Fatalf("markStoppedMissingFrom: %v", err)
	}
	retained, err := fixture.harness.agents.FindByPane(db.FindAgentByPaneInput{
		HerdrSessionName: obsSessionName,
		PaneID:           "wB:p1",
	})
	if err != nil {
		t.Fatalf("findByPane: %v", err)
	}
	if retained == nil {
		t.Fatalf("expected the retained agent row to survive the session stop")
	}

	assertObsEmptyList(t, "agent.list scoped", obsCall(t, fixture.client, "agent.list", map[string]any{
		"workspaceId": obsWorkspaceB,
	}), "agents")
	assertObsEmptyList(t, "agent.list all", obsCall(t, fixture.client, "agent.list", map[string]any{
		"all": true,
	}), "agents")
	assertObsContains(t, "agent.get", obsCallError(t, fixture.client, "agent.get", map[string]any{
		"target": "pi", "workspaceId": obsWorkspaceB,
	}), "agent target not found: pi")
	assertObsContains(t, "agent.read", obsCallError(t, fixture.client, "agent.read", map[string]any{
		"limit": 20, "target": "pi", "workspaceId": obsWorkspaceB,
	}), "agent target not found: pi")
}

// ---------------------------------------------------------------------------
// The history support matrix
// ---------------------------------------------------------------------------

// obsMkdirAll / obsWriteFile / obsJSONLine are the node:fs calls of the TS
// fixture, with the failure spelled out.
func obsMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func obsWriteFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func obsJSONLine(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %#v: %v", value, err)
	}
	return string(encoded)
}

// obsExec runs one statement of a fixture database.
func obsExec(t *testing.T, sqlite *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := sqlite.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// obsOpenFixtureSqlite opens one of the third-party session stores the fixture
// lays out on disk.
func obsOpenFixtureSqlite(t *testing.T, path string) *sql.DB {
	t.Helper()
	sqlite, err := db.OpenSqlite(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	return sqlite
}

// obsCreateFixtureSqlite writes a third-party store and hands the file back,
// exactly like the TS fixture that opens a DatabaseSync handle, creates its
// tables and closes it before the RPC calls run.
func obsCreateFixtureSqlite(t *testing.T, path string, statements ...string) {
	t.Helper()
	sqlite, err := db.OpenSqlite(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	for _, statement := range statements {
		obsExec(t, sqlite, statement)
	}
	if err := sqlite.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

type obsRuntimeSessions struct {
	agyID             string
	claudeID          string
	codexSessionPath  string
	geminiSessionPath string
	piSessionPath     string
}

// obsSeedAdditionalRuntimeAgents is the TS seedAdditionalRuntimeAgents(): six
// panes, one per supported or deliberately unsupported agent kind, each handing
// over its own official session reference.
func obsSeedAdditionalRuntimeAgents(t *testing.T, harness *obsHarness, sessions obsRuntimeSessions) {
	t.Helper()
	harness.upsertRunningSession(t)
	harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent": "agy",
			"agent_session": map[string]any{
				"agent":  "agy",
				"kind":   "id",
				"source": "herdr:antigravity_cli",
				"value":  sessions.agyID,
			},
			"agent_status": "idle",
			"cwd":          "/repo-agy",
			"pane_id":      "wB:p-agy",
			"terminal_id":  "term_agy",
			"workspace_id": obsWorkspaceB,
		},
		{
			"agent": "pi",
			"agent_session": map[string]any{
				"agent":  "pi",
				"kind":   "path",
				"source": "herdr:pi",
				"value":  sessions.piSessionPath,
			},
			"agent_status": "idle",
			"cwd":          "/repo-pi",
			"pane_id":      "wB:p-pi",
			"terminal_id":  "term_pi",
			"workspace_id": obsWorkspaceB,
		},
		{
			"agent": "claude",
			"agent_session": map[string]any{
				"agent":  "claude",
				"kind":   "id",
				"source": "herdr:claude",
				"value":  sessions.claudeID,
			},
			"agent_status": "idle",
			"cwd":          "/repo-claude",
			"pane_id":      "wB:p-claude",
			"terminal_id":  "term_claude",
			"workspace_id": obsWorkspaceB,
		},
		{
			"agent": "codex",
			"agent_session": map[string]any{
				"agent":  "codex",
				"kind":   "path",
				"source": "herdr:codex",
				"value":  sessions.codexSessionPath,
			},
			"agent_status": "idle",
			"cwd":          "/repo-codex",
			"pane_id":      "wB:p-codex",
			"terminal_id":  "term_codex",
			"workspace_id": obsWorkspaceB,
		},
		{
			"agent": "opencode",
			"agent_session": map[string]any{
				"agent":  "opencode",
				"kind":   "id",
				"source": "herdr:opencode",
				"value":  "oc_1",
			},
			"agent_status": "idle",
			"cwd":          "/repo-opencode",
			"pane_id":      "wB:p-opencode",
			"terminal_id":  "term_opencode",
			"workspace_id": obsWorkspaceB,
		},
		{
			"agent": "gemini",
			"agent_session": map[string]any{
				"agent":  "gemini",
				"kind":   "path",
				"source": "herdr:gemini",
				"value":  sessions.geminiSessionPath,
			},
			"agent_status": "idle",
			"cwd":          "/repo-gemini",
			"pane_id":      "wB:p-gemini",
			"terminal_id":  "term_gemini",
			"workspace_id": obsWorkspaceB,
		},
	})
}

// TestObservabilityRpcServerReadsSupportedHistories is the TS case "reads
// supported histories through agent.read and refuses every other agent".
func TestObservabilityRpcServerReadsSupportedHistories(t *testing.T) {
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{})
	dir := fixture.dir
	// Disk bait for the agents herdsman deliberately does not support: their own
	// session stores are populated exactly as their real layouts look. None of
	// them may be read, and the files must not be walked to find a session.
	codexSessionDir := filepath.Join(dir, ".codex", "sessions", "2026", "07", "09")
	obsMkdirAll(t, codexSessionDir)
	codexSessionPath := filepath.Join(codexSessionDir,
		"rollout-2026-07-09T12-00-00-eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee.jsonl")
	obsWriteFile(t, codexSessionPath,
		obsJSONLine(t, map[string]any{
			"type":    "session_meta",
			"payload": map[string]any{"cwd": "/repo-codex"},
		})+"\n"+
			obsJSONLine(t, map[string]any{
				"type":    "event_msg",
				"payload": map[string]any{"type": "user_message", "message": "codex user"},
			})+"\n",
	)

	geminiProjectDir := filepath.Join(dir, ".gemini", "tmp", "repo-gemini")
	geminiChatsDir := filepath.Join(geminiProjectDir, "chats")
	obsMkdirAll(t, geminiChatsDir)
	obsWriteFile(t, filepath.Join(geminiProjectDir, ".project_root"), "/repo-gemini\n")
	geminiSessionPath := filepath.Join(geminiChatsDir, "session-2026-07-09T12-00-00abcdef.json")
	obsWriteFile(t, geminiSessionPath, obsJSONLine(t, map[string]any{
		"messages": []any{map[string]any{
			"content": "gemini user", "id": "g1", "type": "user",
		}},
	}))

	claudeProjectDir := filepath.Join(dir, ".claude", "projects", "-repo-claude")
	obsMkdirAll(t, claudeProjectDir)
	claudeID := "11111111-1111-4111-8111-111111111111"
	obsWriteFile(t, filepath.Join(claudeProjectDir, claudeID+".jsonl"),
		obsJSONLine(t, map[string]any{
			"type":    "user",
			"uuid":    "u1",
			"message": map[string]any{"content": "claude user", "role": "user"},
		})+"\n",
	)

	openCodeDir := filepath.Join(dir, ".local", "share", "opencode")
	obsMkdirAll(t, openCodeDir)
	openCodeDB := obsOpenFixtureSqlite(t, filepath.Join(openCodeDir, "opencode.db"))
	obsExec(t, openCodeDB, `
		create table session (id text primary key, directory text not null, time_updated integer not null);
		create table message (id text primary key, session_id text not null, time_created integer not null, time_updated integer not null, data text not null);
		create table part (id text primary key, message_id text not null, session_id text not null, time_created integer not null, time_updated integer not null, data text not null);`)
	obsExec(t, openCodeDB,
		"insert into session (id, directory, time_updated) values (?, ?, ?)",
		"oc_1", "/repo-opencode", 1)
	obsExec(t, openCodeDB,
		"insert into message (id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?)",
		"m1", "oc_1", 1, 1, obsJSONLine(t, map[string]any{"role": "user"}))
	obsExec(t, openCodeDB,
		"insert into part (id, message_id, session_id, time_created, time_updated, data) values (?, ?, ?, ?, ?, ?)",
		"p1", "m1", "oc_1", 2, 2,
		obsJSONLine(t, map[string]any{"text": "opencode user", "type": "text"}))
	if err := openCodeDB.Close(); err != nil {
		t.Fatalf("close opencode db: %v", err)
	}

	// The two supported agents: agy resolves its official conversation id to the
	// conversation database, pi hands over its session file path directly.
	agyID := "22222222-2222-4222-8222-222222222222"
	conversationsDir := filepath.Join(dir, ".gemini", "antigravity-cli", "conversations")
	obsMkdirAll(t, conversationsDir)
	obsCreateFixtureSqlite(t, filepath.Join(conversationsDir, agyID+".db"),
		"create table unrelated (id text primary key)")

	piSessionDir := filepath.Join(dir, ".pi", "agent", "sessions")
	obsMkdirAll(t, piSessionDir)
	piSessionPath := filepath.Join(piSessionDir, "ses-rpc.jsonl")
	obsWriteFile(t, piSessionPath, obsJSONLine(t, map[string]any{
		"type":    "message",
		"id":      "u1",
		"message": map[string]any{"content": "pi user", "role": "user"},
	})+"\n")

	obsSeedAdditionalRuntimeAgents(t, fixture.harness, obsRuntimeSessions{
		agyID:             agyID,
		claudeID:          claudeID,
		codexSessionPath:  codexSessionPath,
		geminiSessionPath: geminiSessionPath,
		piSessionPath:     piSessionPath,
	})

	agyRead := obsCall(t, fixture.client, "agent.read", map[string]any{
		"limit": 10, "target": "agy", "workspaceId": obsWorkspaceB,
	})
	assertObsSubset(t, obsAt(t, agyRead, "agent", "historyRef"), map[string]any{
		"source": "antigravity-sqlite",
		"value":  agyID,
	})
	if messages := obsAgentMessages(t, agyRead); len(messages) != 0 {
		t.Fatalf("agent.read agy: expected no message, got %v", messages)
	}

	piRead := obsCall(t, fixture.client, "agent.read", map[string]any{
		"limit": 10, "target": "pi", "workspaceId": obsWorkspaceB,
	})
	assertObsSubset(t, obsAt(t, piRead, "agent", "historyRef"), map[string]any{
		"source": "pi-jsonl",
		"value":  piSessionPath,
	})
	piMessages := obsAgentMessages(t, piRead)
	if len(piMessages) != 1 {
		t.Fatalf("agent.read pi: expected one message, got %v", piMessages)
	}
	assertObsSubset(t, piMessages[0], map[string]any{"role": "user", "text": "pi user"})

	for _, target := range []string{"claude", "codex", "gemini", "opencode"} {
		refused := obsCall(t, fixture.client, "agent.read", map[string]any{
			"limit": 10, "target": target, "workspaceId": obsWorkspaceB,
		})
		assertObsNoHistory(t, "agent.read "+target, refused)
		detail := obsCall(t, fixture.client, "agent.get", map[string]any{
			"target": target, "workspaceId": obsWorkspaceB,
		})
		assertObsSubset(t, obsAt(t, detail, "agent", "history"), map[string]any{"messageCount": 0})
	}
}

// ---------------------------------------------------------------------------
// Event routing over several connections
// ---------------------------------------------------------------------------

// obsWorkspaceD is the ownerless sibling workspace of the routing fixture;
// obsWorkspaceC comes with the disconnect-grace port.
const obsWorkspaceD = "wD"

// obsSeedRoutingAgents is the TS seedRoutingAgents(): four panes over three
// orchestrated workspaces plus the ownerless sibling workspace wD.
func obsSeedRoutingAgents(t *testing.T, harness *obsHarness) {
	t.Helper()
	harness.upsertRunningSession(t)
	harness.replaceAgents(t, []db.HerdrAgentLike{
		{"agent": "pi", "pane_id": "wB:p-a", "terminal_id": "term_a", "workspace_id": obsWorkspaceB},
		{"agent": "pi", "pane_id": "wB:p-b", "terminal_id": "term_b", "workspace_id": obsWorkspaceB},
		{"agent": "pi", "pane_id": "wC:p-c", "terminal_id": "term_c", "workspace_id": obsWorkspaceC},
		// Ownerless sibling workspace: a script-dispatched worker pane whose
		// workspace never got an orchestrator of its own.
		{"agent": "pi", "pane_id": "wD:p-d", "terminal_id": "term_d", "workspace_id": obsWorkspaceD},
	})
}

// obsRegisterClient is the TS register() helper of the RPC test client: the
// subscriber id varies per connection, the sessionRef does not.
func obsRegisterClient(
	t *testing.T,
	client *obsRPCClient,
	paneID string,
	subscriberID string,
	workspaceID string,
) map[string]any {
	t.Helper()
	return obsRequest(t, client, "agent.orchestrator.register", obsRegisterWith(
		obsHerdrSocketPath, paneID, subscriberID, "pi", workspaceID,
	))
}

// obsNotificationsByMethod is `client.notifications.filter((message) =>
// message.method === method)`.
func obsNotificationsByMethod(client *obsRPCClient, method string) []map[string]any {
	client.mu.Lock()
	defer client.mu.Unlock()
	var matches []map[string]any
	for _, message := range client.notifications {
		if message["method"] == method {
			matches = append(matches, message)
		}
	}
	return matches
}

func assertObsNoNotificationsOfMethod(t *testing.T, who string, client *obsRPCClient, method string) {
	t.Helper()
	if matches := obsNotificationsByMethod(client, method); len(matches) != 0 {
		t.Fatalf("%s: expected no %s notification, got %v", who, method, matches)
	}
}

// TestObservabilityRpcRoutesEventsToNewestOwner is the TS case "routes events to
// the newest owner socket and role changes to the exact scope".
func TestObservabilityRpcRoutesEventsToNewestOwner(t *testing.T) {
	fixture := obsOpenRPCServerWithoutClient(t, obsRPCServerOptions{})
	obsSeedRoutingAgents(t, fixture.harness)
	piA := obsDial(t, fixture.socketPath)
	piB := obsDial(t, fixture.socketPath)
	piC := obsDial(t, fixture.socketPath)
	generic := obsDial(t, fixture.socketPath)
	obsRegisterClient(t, piA, "wB:p-a", "pi-a", obsWorkspaceB)
	obsRegisterClient(t, piB, "wB:p-b", "pi-b", obsWorkspaceB)
	obsRegisterClient(t, piC, "wC:p-c", "pi-c", obsWorkspaceC)
	obsRequest(t, generic, "agent.events", map[string]any{"workspaceId": obsWorkspaceB})

	// Nothing may be pushed while the scope has no owner.
	beforeOwner := obsAppendRoutedEvent(t, fixture.harness, "term_b", obsWorkspaceB)
	fixture.server.PublishAgentEvent(beforeOwner)
	obsSocketTick()
	assertObsNoNotifications(t, "pi-a", piA)
	assertObsNoNotifications(t, "pi-b", piB)

	claimed := obsRequest(t, piA, "agent.orchestrator.set", map[string]any{"enabled": true})
	assertObsSubset(t, claimed, map[string]any{"changed": true})
	assertObsSubset(t, obsAt(t, claimed, "state", "owner"), map[string]any{
		"paneId":     "wB:p-a",
		"terminalId": "term_a",
	})
	change := obsAt(t, obsWaitNotification(t, piA, "agent.orchestrator.changed"), "params", "change")
	assertObsSubset(t, change, map[string]any{"reason": "claimed"})
	assertObsSubset(t, obsAt(t, change, "current", "owner"), map[string]any{"terminalId": "term_a"})
	obsWaitNotification(t, piB, "agent.orchestrator.changed")
	// The role change is scoped: the other workspace's connector and the
	// connector that never registered stay silent.
	assertObsNoNotifications(t, "pi-c", piC)
	assertObsNoNotifications(t, "generic", generic)

	agentEvent := obsAppendRoutedEvent(t, fixture.harness, "term_b", obsWorkspaceB)
	fixture.server.PublishAgentEvent(agentEvent)
	eventParams := obsAt(t, obsWaitNotification(t, piA, "agent.event"), "params", "event")
	assertObsSubset(t, eventParams, map[string]any{"id": agentEvent.ID})
	assertObsNoNotifications(t, "pi-b", piB)
	acked := obsRequest(t, piA, "agent.notifications.ack", map[string]any{"eventId": agentEvent.ID})
	assertObsSubset(t, acked, map[string]any{"acknowledged": true})
	assertObsSubset(t, obsAt(t, acked, "state"), map[string]any{"ackedEventId": agentEvent.ID})

	// The owner's own terminal event is only appended: it stays pending until the
	// scope is claimed again.
	self := obsAppendRoutedEvent(t, fixture.harness, "term_a", obsWorkspaceB)
	unknownTerminal, err := fixture.harness.agentEvents.Append(db.AppendAgentEventInput{
		HerdrSessionName: obsSessionName,
		Payload:          map[string]any{},
		TerminalID:       nil,
		Type:             observability.AgentEventDone,
		WorkspaceID:      obsString(obsWorkspaceB),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	fixture.server.PublishAgentEvent(unknownTerminal)
	obsSocketTick()
	assertObsNoNotifications(t, "pi-a", piA)

	// 孤儿终态事件（agents 行已物理删除，agent_id = null）仍然必须能推到 owner。
	orphanFailure, err := fixture.harness.agentEvents.Append(db.AppendAgentEventInput{
		AgentID:          nil,
		HerdrSessionName: obsSessionName,
		PaneID:           obsString("wB:p-b"),
		Payload:          map[string]any{"agentId": "agent-gone", "reason": "RETRY_EXHAUSTED"},
		TerminalID:       obsString("term_b"),
		Type:             observability.AgentEventFailed,
		WorkspaceID:      obsString(obsWorkspaceB),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	fixture.server.PublishAgentEvent(orphanFailure)
	assertObsSubset(t, obsAt(t, obsWaitNotification(t, piA, "agent.event"), "params", "event"), map[string]any{
		"agentId": nil,
		"id":      orphanFailure.ID,
		"type":    "agent.failed",
	})

	_, err = piB.request("agent.orchestrator.set", map[string]any{"enabled": true})
	if err == nil {
		t.Fatalf("piB claim: expected ORCHESTRATOR_SCOPE_ALREADY_CLAIMED")
	}
	assertObsContains(t, "piB claim", err.Error(), "ORCHESTRATOR_SCOPE_ALREADY_CLAIMED")

	piA.close()
	// Wait for the server to observe piA's close (which drops the terminal
	// presence and lets term_b reclaim the scope) instead of sleeping a fixed
	// 75ms. A rejected `agent.orchestrator.set` is side-effect free: the scope
	// store throws ORCHESTRATOR_SCOPE_ALREADY_CLAIMED before it writes anything,
	// so polling it is safe.
	var replacement map[string]any
	obsWaitFor(t, "term_b to reclaim the scope", func() bool {
		result, claimErr := piB.request("agent.orchestrator.set", map[string]any{"enabled": true})
		if claimErr != nil {
			return false
		}
		replacement, _ = result.(map[string]any)
		return replacement != nil
	})
	assertObsSubset(t, replacement, map[string]any{"changed": true})
	assertObsSubset(t, obsAt(t, replacement, "state", "owner"), map[string]any{"terminalId": "term_b"})
	reclaimedEvents := obsList(t, "piB claim", replacement, "events")
	if len(reclaimedEvents) != 1 {
		t.Fatalf("piB claim: expected exactly the pending self event, got %v", reclaimedEvents)
	}
	assertObsSubset(t, reclaimedEvents[0], map[string]any{"id": self.ID})
	obsWaitNotification(t, piB, "agent.orchestrator.changed")

	reAcked := obsRequest(t, piB, "agent.notifications.ack", map[string]any{"eventId": agentEvent.ID})
	assertObsSubset(t, reAcked, map[string]any{"acknowledged": true})
	assertObsSubset(t, obsAt(t, reAcked, "state"), map[string]any{"ackedEventId": agentEvent.ID})

	released := obsRequest(t, piB, "agent.orchestrator.set", map[string]any{"enabled": false})
	assertObsSubset(t, released, map[string]any{"changed": true})
	state := released["state"]
	if value, present := state.(map[string]any); !present || value["owner"] != nil {
		t.Fatalf("piB release: expected state.owner null, got %#v", state)
	}

	piB.close()
	piC.close()
	generic.close()
}

// TestObservabilityRpcPushesOwnerlessSiblingEvent is the TS case "pushes an
// ownerless sibling workspace event only to the single active session owner".
func TestObservabilityRpcPushesOwnerlessSiblingEvent(t *testing.T) {
	fixture := obsOpenRPCServerWithoutClient(t, obsRPCServerOptions{})
	obsSeedRoutingAgents(t, fixture.harness)
	owner := obsDial(t, fixture.socketPath)
	secondOwner := obsDial(t, fixture.socketPath)
	obsRegisterClient(t, owner, "wB:p-a", "pi-a", obsWorkspaceB)
	claimed := obsRequest(t, owner, "agent.orchestrator.set", map[string]any{"enabled": true})
	assertObsSubset(t, claimed, map[string]any{"changed": true})
	assertObsSubset(t, obsAt(t, claimed, "state", "owner"), map[string]any{"terminalId": "term_a"})
	obsWaitNotification(t, owner, "agent.orchestrator.changed")

	// Exactly one active owner in the session: the ownerless workspace's event is
	// routed to it, because the worker pane has no orchestrator of its own.
	siblingEvent := obsAppendRoutedEvent(t, fixture.harness, "term_d", obsWorkspaceD)
	fixture.server.PublishAgentEvent(siblingEvent)
	assertObsSubset(t, obsAt(t, obsWaitNotification(t, owner, "agent.event"), "params", "event"), map[string]any{
		"id":          siblingEvent.ID,
		"terminalId":  "term_d",
		"workspaceId": obsWorkspaceD,
	})
	// The push is a hint only: the owner still pulls the same event through
	// pending() (the acknowledgement path for ownerless sibling workspaces).
	state := obsRequest(t, owner, "agent.orchestrator.get", map[string]any{})
	pending := obsList(t, "agent.orchestrator.get", state, "events")
	if len(pending) != 1 {
		t.Fatalf("agent.orchestrator.get: expected only the sibling event, got %v", pending)
	}
	assertObsSubset(t, pending[0], map[string]any{
		"id":          siblingEvent.ID,
		"workspaceId": obsWorkspaceD,
	})

	// Two active owners: no broadcast (that would double-deliver one event), so the
	// ownerless workspace stays pull-only for both owners.
	obsRegisterClient(t, secondOwner, "wC:p-c", "pi-c", obsWorkspaceC)
	secondClaim := obsRequest(t, secondOwner, "agent.orchestrator.set", map[string]any{"enabled": true})
	assertObsSubset(t, secondClaim, map[string]any{"changed": true})
	assertObsSubset(t, obsAt(t, secondClaim, "state", "owner"), map[string]any{"terminalId": "term_c"})
	obsSocketTick()
	pullOnly := obsAppendRoutedEvent(t, fixture.harness, "term_d", obsWorkspaceD)
	fixture.server.PublishAgentEvent(pullOnly)
	obsSocketTick()
	assertObsNoNotificationsOfMethod(t, "owner", owner, "agent.event")
	assertObsNoNotificationsOfMethod(t, "second-owner", secondOwner, "agent.event")

	owner.close()
	secondOwner.close()
}

// ---------------------------------------------------------------------------
// Cached list vs. live detail read
// ---------------------------------------------------------------------------

const obsPiJSONLSource = "pi-jsonl"

// obsCallRecordingHistory is the TS liveHistory double: it records which entry
// point ran with which forceDiscovery flag and answers with an empty history.
// The embedded real service only backs the path helpers the context service
// reads; neither history entry point reaches it.
type obsCallRecordingHistory struct {
	*obsHistory
	mu    sync.Mutex
	calls []string
}

func newObsCallRecordingHistory(harness *obsHarness, homeDir string) *obsCallRecordingHistory {
	return &obsCallRecordingHistory{obsHistory: newObsHistory(harness, homeDir)}
}

func (h *obsCallRecordingHistory) record(entry string, forceDiscovery bool) {
	outcome := "reuse"
	if forceDiscovery {
		outcome = "fresh"
	}
	h.mu.Lock()
	h.calls = append(h.calls, entry+":"+outcome)
	h.mu.Unlock()
}

func (h *obsCallRecordingHistory) taken() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.calls...)
}

func (h *obsCallRecordingHistory) ResolveCompactHistory(
	_ observability.AgentHistoryLookupInput,
	options observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	h.record("get", options.ForceDiscovery)
	source := obsPiJSONLSource
	return observability.AgentHistoryResolved{
		CompactHistory: observability.CompactAgentHistory{
			HistoryRef:           nil,
			LastAssistantMessage: nil,
			LastToolResult:       nil,
			LastUserMessage:      nil,
			MessageCount:         0,
			Source:               &source,
			UpdatedAt:            nil,
		},
		HistoryRef:        nil,
		SourceFingerprint: nil,
	}
}

func (h *obsCallRecordingHistory) Read(
	_ observability.AgentHistoryLookupInput,
	options daemon.ObservabilityHistoryReadOptions,
) daemon.ObservabilityHistoryReadResult {
	h.record("read", options.ForceDiscovery)
	return daemon.ObservabilityHistoryReadResult{HistoryRef: nil, Messages: nil}
}

// TestObservabilityRpcServerCachedListLiveDetail is the TS case "serves cached
// lists and re-resolves live detail reads".
func TestObservabilityRpcServerCachedListLiveDetail(t *testing.T) {
	harness := openObsHarness(t)
	stub := newObsCallRecordingHistory(harness, t.TempDir())
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{harness: harness, history: stub})
	obsSeedAgent(t, harness, fixture.dir)

	agent := obsFindSeededAgent(t, harness, "wB:p1")
	if _, err := harness.agentContextSnapshots.Put(db.AgentContextSnapshotInput{
		AgentID: agent.ID,
		CompactHistory: observability.CompactAgentHistory{
			LastUserMessage: &observability.AgentHistoryExcerpt{
				Ref:       "cached",
				Text:      "cached user",
				Timestamp: nil,
			},
			MessageCount: 0,
			Source:       obsString(obsPiJSONLSource),
		},
		HistoryRef: &observability.AgentHistoryRef{
			Kind:   observability.HistoryRefAgentSession,
			Path:   obsString("/tmp/history.jsonl"),
			Source: observability.HistorySourcePiJSONL,
			Value:  "/tmp/history.jsonl",
		},
		PaneRevision: nil,
		SourceFingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: 1,
			Path:    "/tmp/history.jsonl",
			Size:    1,
		},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	listed := obsCall(t, fixture.client, "agent.list", map[string]any{"workspaceId": obsWorkspaceB})
	agents := obsList(t, "agent.list", listed, "agents")
	if len(agents) != 1 {
		t.Fatalf("agent.list: expected one agent, got %v", agents)
	}
	assertObsSubset(t, obsAt(t, agents[0], "history", "lastUserMessage"),
		map[string]any{"text": "cached user"})
	if calls := stub.taken(); len(calls) != 0 {
		t.Fatalf("agent.list: expected no history call, got %v", calls)
	}

	obsCall(t, fixture.client, "agent.get", map[string]any{"target": "pi", "workspaceId": obsWorkspaceB})
	obsCall(t, fixture.client, "agent.read", map[string]any{"target": "pi", "workspaceId": obsWorkspaceB})
	// A list is served from the stored snapshots (no history read at all), while
	// an explicit detail read re-resolves from the official values instead of
	// reusing the persisted ref or a memoized failure.
	if got := strings.Join(stub.taken(), ","); got != "get:fresh,read:fresh" {
		t.Fatalf("expected the history calls [get:fresh, read:fresh], got [%s]", got)
	}
}

// ---------------------------------------------------------------------------
// Re-resolving a recently failed lookup, and persisted discovered_file refs
// ---------------------------------------------------------------------------

// TestObservabilityRpcServerRetriesFailedLookup is the TS case "agent.read
// retries a lookup that failed moments ago instead of returning a memoized empty
// history".
func TestObservabilityRpcServerRetriesFailedLookup(t *testing.T) {
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{})
	agyID := "33333333-3333-4333-8333-333333333333"
	fixture.harness.upsertRunningSession(t)
	fixture.harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent": "agy",
			"agent_session": map[string]any{
				"agent":  "agy",
				"kind":   "id",
				"source": "herdr:antigravity_cli",
				"value":  agyID,
			},
			"agent_status": "idle",
			"pane_id":      "wB:p-agy",
			"terminal_id":  "term_agy",
			"workspace_id": obsWorkspaceB,
		},
	})

	// Nothing resolves yet, so the lookup fails and is remembered for the
	// daemon's background rounds — and warned about exactly once.
	var failed map[string]any
	captured := captureStderr(t, func() {
		failed = obsCall(t, fixture.client, "agent.read", map[string]any{
			"limit": 10, "target": "agy", "workspaceId": obsWorkspaceB,
		})
	})
	assertObsNoHistory(t, "agent.read before the database exists", failed)
	if count := obsCountWarnLines(captured, agenthistory.DiscoveryWarnMessage); count != 1 {
		t.Fatalf("expected exactly one discovery warning, got %d in:\n%s", count, captured)
	}

	// The operator fixes the reported problem (the conversation database appears)
	// inside the retry window: an explicit read resolves it now instead of
	// answering from the remembered failure.
	conversations := filepath.Join(fixture.dir, ".gemini", "antigravity-cli", "conversations")
	obsMkdirAll(t, conversations)
	dbPath := filepath.Join(conversations, agyID+".db")
	obsCreateFixtureSqlite(t, dbPath, "create table unrelated (id text primary key)")

	var resolved map[string]any
	captured = captureStderr(t, func() {
		resolved = obsCall(t, fixture.client, "agent.read", map[string]any{
			"limit": 10, "target": "agy", "workspaceId": obsWorkspaceB,
		})
	})
	assertObsSubset(t, obsAt(t, resolved, "agent", "historyRef"), map[string]any{
		"path":   dbPath,
		"source": "antigravity-sqlite",
		"value":  agyID,
	})
	detail := obsCall(t, fixture.client, "agent.get", map[string]any{
		"target": "agy", "workspaceId": obsWorkspaceB,
	})
	assertObsSubset(t, obsAt(t, detail, "agent", "history"), map[string]any{"source": "antigravity-sqlite"})
	// Forcing a retry never duplicates the per-key warning: the TS assertion is on
	// the total since the spy was installed, so the second window must stay clean.
	if count := obsCountWarnLines(captured, agenthistory.DiscoveryWarnMessage); count != 0 {
		t.Fatalf("retry must not warn again, got %d in:\n%s", count, captured)
	}
}

type obsHistoryCall struct {
	method    string
	preferred string
}

// obsPreferredRecordingHistory is the liveHistory double of the two
// discovered_file cases: it records the preferred ref each entry point was
// handed. The read options type carries no preferred-ref field at all, because
// the TS server hands read only {forceDiscovery, limit} — which is the read half
// of what these cases assert: a persisted ref never reaches the read path.
type obsPreferredRecordingHistory struct {
	*obsHistory
	mu    sync.Mutex
	calls []obsHistoryCall
}

func newObsPreferredRecordingHistory(
	harness *obsHarness,
	homeDir string,
) *obsPreferredRecordingHistory {
	return &obsPreferredRecordingHistory{obsHistory: newObsHistory(harness, homeDir)}
}

func (h *obsPreferredRecordingHistory) record(method string, preferred string) {
	h.mu.Lock()
	h.calls = append(h.calls, obsHistoryCall{method: method, preferred: preferred})
	h.mu.Unlock()
}

func (h *obsPreferredRecordingHistory) taken() []obsHistoryCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]obsHistoryCall{}, h.calls...)
}

// described renders the labels of the second TS double: "<method>:discover"
// without a preferred ref, "<method>:<value>" with one.
func (h *obsPreferredRecordingHistory) described() []string {
	calls := h.taken()
	labels := make([]string, 0, len(calls))
	for _, call := range calls {
		if call.preferred == "" {
			labels = append(labels, call.method+":discover")
			continue
		}
		labels = append(labels, call.method+":"+call.preferred)
	}
	return labels
}

func (h *obsPreferredRecordingHistory) ResolveCompactHistory(
	_ observability.AgentHistoryLookupInput,
	options observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	preferred := ""
	if options.PreferredRef != nil {
		preferred = options.PreferredRef.Value
	}
	h.record("get", preferred)
	return observability.AgentHistoryResolved{
		CompactHistory:    obsEmptyPiCompactHistory(),
		HistoryRef:        nil,
		SourceFingerprint: nil,
	}
}

func (h *obsPreferredRecordingHistory) Read(
	_ observability.AgentHistoryLookupInput,
	_ daemon.ObservabilityHistoryReadOptions,
) daemon.ObservabilityHistoryReadResult {
	h.record("read", "")
	return daemon.ObservabilityHistoryReadResult{HistoryRef: nil, Messages: nil}
}

// obsPutSnapshot writes one agent context snapshot, failing the test on error.
func obsPutSnapshot(
	t *testing.T,
	harness *obsHarness,
	input db.AgentContextSnapshotInput,
) {
	t.Helper()
	if _, err := harness.agentContextSnapshots.Put(input); err != nil {
		t.Fatalf("put: %v", err)
	}
}

// TestObservabilityRpcServerSkipsPersistedDiscoveredFileRef is the TS case
// "skips a persisted discovered_file ref on agent.get/read".
func TestObservabilityRpcServerSkipsPersistedDiscoveredFileRef(t *testing.T) {
	harness := openObsHarness(t)
	stub := newObsPreferredRecordingHistory(harness, t.TempDir())
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{harness: harness, history: stub})
	obsSeedAgent(t, harness, fixture.dir)
	// The pane reports no agent_session at all, and the pane title carries the
	// marker the deleted cwd/mtime scanner used to key on; only the persisted
	// discovered_file ref — pointing at a file that never existed — could answer.
	harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent":          "pi",
			"agent_status":   "idle",
			"cwd":            "/repo",
			"pane_id":        "wB:p1",
			"terminal_id":    "term_1",
			"terminal_title": "π - role-worker-53c500b2 - root",
			"workspace_id":   obsWorkspaceB,
		},
	})
	agent := obsFindSeededAgent(t, harness, "wB:p1")
	obsPutSnapshot(t, harness, db.AgentContextSnapshotInput{
		AgentID:        agent.ID,
		CompactHistory: obsEmptyPiCompactHistory(),
		HistoryRef: &observability.AgentHistoryRef{
			Kind:   observability.HistoryRefDiscoveredFile,
			Path:   obsString("/tmp/missing-discovered.jsonl"),
			Source: observability.HistorySourcePiJSONL,
			Value:  "/tmp/missing-discovered.jsonl",
		},
		PaneRevision: nil,
		SourceFingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: 1,
			Path:    "/tmp/missing-discovered.jsonl",
			Size:    1,
		},
	})

	obsCall(t, fixture.client, "agent.get", map[string]any{"target": "pi", "workspaceId": obsWorkspaceB})
	obsCall(t, fixture.client, "agent.read", map[string]any{"target": "pi", "workspaceId": obsWorkspaceB})
	calls := stub.taken()
	if len(calls) != 2 {
		t.Fatalf("expected one get and one read, got %v", calls)
	}
	if calls[0] != (obsHistoryCall{method: "get"}) || calls[1] != (obsHistoryCall{method: "read"}) {
		t.Fatalf("expected no preferred ref on either entry point, got %v", calls)
	}
}

// TestObservabilityRpcServerDoesNotReusePersistedDiscoveredFileRef is the TS
// case "does not reuse a persisted discovered_file ref on agent.get/read".
func TestObservabilityRpcServerDoesNotReusePersistedDiscoveredFileRef(t *testing.T) {
	harness := openObsHarness(t)
	stub := newObsPreferredRecordingHistory(harness, t.TempDir())
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{harness: harness, history: stub})
	obsSeedAgent(t, harness, fixture.dir)
	agent := obsFindSeededAgent(t, harness, "wB:p1")
	path := filepath.Join(fixture.dir, "recent-discovered.jsonl")
	obsWriteFile(t, path, "history\n")
	obsPutSnapshot(t, harness, db.AgentContextSnapshotInput{
		AgentID:        agent.ID,
		CompactHistory: obsEmptyPiCompactHistory(),
		HistoryRef: &observability.AgentHistoryRef{
			Kind:   observability.HistoryRefDiscoveredFile,
			Path:   obsString(path),
			Source: observability.HistorySourcePiJSONL,
			Value:  path,
		},
		PaneRevision: nil,
		SourceFingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: float64(time.Now().UnixMilli()),
			Path:    path,
			Size:    1,
		},
	})

	obsCall(t, fixture.client, "agent.get", map[string]any{"target": "pi", "workspaceId": obsWorkspaceB})
	obsCall(t, fixture.client, "agent.read", map[string]any{"target": "pi", "workspaceId": obsWorkspaceB})
	// Herdsman no longer trusts a ref the deleted cwd/mtime scanner produced, even
	// while the file is still recent: the official agent_session value decides.
	if got := strings.Join(stub.described(), ","); got != "get:discover,read:discover" {
		t.Fatalf("expected [get:discover, read:discover], got [%s]", got)
	}
}

// ---------------------------------------------------------------------------
// Workspace context push and connector binding
// ---------------------------------------------------------------------------

// assertObsContextAgents is `toMatchObject({agents: [{terminalId: ...}]})` over a
// workspace context snapshot: exactly the one non-owner agent, because the owner
// is handed its workspace with its own terminal excluded.
func assertObsContextAgents(t *testing.T, what string, context any, terminalID string) {
	t.Helper()
	entries := obsAt(t, context, "agents")
	items, ok := entries.([]any)
	if !ok {
		t.Fatalf("%s: expected context.agents to be an array, got %T", what, entries)
	}
	if len(items) != 1 {
		t.Fatalf("%s: expected exactly one context agent, got %v", what, items)
	}
	assertObsSubset(t, items[0], map[string]any{"terminalId": terminalID})
}

// TestObservabilityRpcServerPushesContextToOwnerOnly is the TS case "returns and
// pushes context only to the current owner, including null clearing snapshots".
func TestObservabilityRpcServerPushesContextToOwnerOnly(t *testing.T) {
	fixture := obsOpenRPCServerWithoutClient(t, obsRPCServerOptions{})
	obsSeedRoutingAgents(t, fixture.harness)
	agents, err := fixture.harness.agents.List(observability.AgentQueryScope{
		WorkspaceID: obsString(obsWorkspaceB),
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, agent := range agents {
		terminal := "unknown"
		if agent.TerminalID != nil {
			terminal = *agent.TerminalID
		}
		obsPutSnapshot(t, fixture.harness, db.AgentContextSnapshotInput{
			AgentID: agent.ID,
			CompactHistory: observability.CompactAgentHistory{
				LastAssistantMessage: &observability.AgentHistoryExcerpt{
					Ref:       terminal,
					Text:      agent.PaneID,
					Timestamp: nil,
				},
				Source: obsString(obsPiJSONLSource),
			},
			HistoryRef:        nil,
			PaneRevision:      nil,
			SourceFingerprint: nil,
		})
	}

	owner := obsDial(t, fixture.socketPath)
	off := obsDial(t, fixture.socketPath)
	other := obsDial(t, fixture.socketPath)
	registered := obsRegisterClient(t, owner, "wB:p-a", "owner", obsWorkspaceB)
	if value, present := registered["context"]; !present || value != nil {
		t.Fatalf("register: expected context null for a non-owner, got %#v (present=%v)", value, present)
	}
	obsRegisterClient(t, off, "wB:p-b", "off", obsWorkspaceB)
	obsRegisterClient(t, other, "wC:p-c", "other", obsWorkspaceC)

	claimed := obsRequest(t, owner, "agent.orchestrator.set", map[string]any{"enabled": true})
	assertObsContextAgents(t, "claim", obsAt(t, claimed, "context"), "term_b")
	obsSocketTick()
	owner.clearNotifications()
	off.clearNotifications()
	other.clearNotifications()

	scope := observability.AgentScope{HerdrSessionName: obsSessionName, WorkspaceID: obsWorkspaceB}
	fixture.server.PublishAgentContext(scope)
	params := obsAt(t, obsWaitNotification(t, owner, "agent.context.changed"), "params")
	assertObsSubset(t, params, map[string]any{"workspaceId": obsWorkspaceB})
	assertObsContextAgents(t, "agent.context.changed", obsAt(t, params, "context"), "term_b")

	obsSocketTick()
	fixture.server.PublishAgentContext(scope)
	obsSocketTick()
	// The unchanged snapshot is not pushed a second time, and only the owner's
	// scope is in play at all.
	assertObsNoNotifications(t, "owner", owner)
	assertObsNoNotifications(t, "off", off)
	assertObsNoNotifications(t, "other", other)

	var offAgentID string
	for _, agent := range agents {
		if agent.TerminalID != nil && *agent.TerminalID == "term_b" {
			offAgentID = agent.ID
		}
	}
	if offAgentID == "" {
		t.Fatalf("missing off agent")
	}
	if err := fixture.harness.agentContextSnapshots.Delete(offAgentID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	fixture.server.PublishAgentContext(scope)
	cleared := obsAt(t, obsWaitNotification(t, owner, "agent.context.changed"), "params")
	assertObsSubset(t, cleared, map[string]any{"workspaceId": obsWorkspaceB})
	clearedObject, ok := cleared.(map[string]any)
	if !ok {
		t.Fatalf("expected the context notification params to be an object, got %T", cleared)
	}
	if value, present := clearedObject["context"]; !present || value != nil {
		t.Fatalf("expected an emptied workspace to push context null, got %#v (present=%v)", value, present)
	}

	owner.close()
	off.close()
	other.close()
}

// TestObservabilityRpcServerResolvesStalePaneAlias is the TS case "validates
// indexed presence and resolves a stale pane alias from live Herdr".
func TestObservabilityRpcServerResolvesStalePaneAlias(t *testing.T) {
	fixture := obsOpenRPCServerWithoutClient(t, obsRPCServerOptions{
		resolvePaneIdentity: func(_ daemon.ResolvePaneIdentityInput) (herdr.HerdrPaneIdentity, error) {
			return herdr.HerdrPaneIdentity{
				PaneID:      "wC:p2",
				TerminalID:  "term_moved",
				WorkspaceID: obsWorkspaceC,
			}, nil
		},
	})
	fixture.harness.upsertRunningSession(t)
	client := obsDial(t, fixture.socketPath)

	// The indexed pane id is stale: live Herdr answers with the real identity, and
	// the presence follows it.
	registered := obsRequest(t, client, "agent.orchestrator.register", obsRegisterWith(
		obsHerdrSocketPath, "wB:p-old", "pi-moved", "pi", obsWorkspaceB,
	))
	assertObsSubset(t, obsAt(t, registered, "presence"), map[string]any{
		"herdrSessionName": obsSessionName,
		"paneId":           "wC:p2",
		"terminalId":       "term_moved",
		"workspaceId":      obsWorkspaceC,
	})

	_, err := client.request("agent.orchestrator.register", obsRegisterWith(
		"/tmp/unknown.sock", "wB:p1", "pi-unknown", "pi", obsWorkspaceB,
	))
	if err == nil {
		t.Fatalf("unregistered herdr socket: expected a rejection")
	}
	assertObsContains(t, "unregistered herdr socket", err.Error(), "running session")

	_, err = client.request("agent.orchestrator.register", obsRegisterWith(
		obsHerdrSocketPath, "wB:p1", "bad-kind", "claude", obsWorkspaceB,
	))
	if err == nil {
		t.Fatalf("unsupported subscriber kind: expected a rejection")
	}
	assertObsContains(t, "unsupported subscriber kind", err.Error(), "Invalid RPC params")

	client.close()
}

// obsSelfPidPeer is the TS `peerPidOf: () => process.pid`: the connector process
// is the test process itself, so the pane cwd check compares against this
// process' own working directory.
func obsSelfPidPeer(net.Conn) (int, bool) { return os.Getpid(), true }

// obsPaneIdentityOf builds the live Herdr answer of the three binding cases.
func obsPaneIdentityOf(cwd *string) func(daemon.ResolvePaneIdentityInput) (herdr.HerdrPaneIdentity, error) {
	return func(_ daemon.ResolvePaneIdentityInput) (herdr.HerdrPaneIdentity, error) {
		return herdr.HerdrPaneIdentity{
			PaneID:      "wB:p1",
			TerminalID:  "term_1",
			WorkspaceID: obsWorkspaceB,
			CWD:         cwd,
		}, nil
	}
}

// TestObservabilityRpcServerRejectsConnectorCwdMismatch is the TS case "rejects
// register when the connector cwd does not match the Herdr pane cwd".
func TestObservabilityRpcServerRejectsConnectorCwdMismatch(t *testing.T) {
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{
		peerPidOf:           obsSelfPidPeer,
		resolvePaneIdentity: obsPaneIdentityOf(obsString("/tmp/herdsman-not-this-pane")),
	})
	obsSeedAgent(t, fixture.harness, fixture.dir)

	assertObsContains(t, "register with a foreign connector cwd", obsCallError(
		t, fixture.client, "agent.orchestrator.register", obsRegisterParams("wB:p1"),
	), "does not match Herdr pane")
}

// TestObservabilityRpcServerAcceptsMatchingConnectorCwd is the TS case "accepts
// register when the connector cwd matches the Herdr pane cwd".
func TestObservabilityRpcServerAcceptsMatchingConnectorCwd(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{
		peerPidOf:           obsSelfPidPeer,
		resolvePaneIdentity: obsPaneIdentityOf(obsString(cwd)),
	})
	obsSeedAgent(t, fixture.harness, fixture.dir)

	registered := obsCall(t, fixture.client, "agent.orchestrator.register", obsRegisterParams("wB:p1"))
	assertObsSubset(t, obsAt(t, registered, "presence"), map[string]any{"terminalId": "term_1"})
}

// TestObservabilityRpcServerWarnsOnMissingPaneCwd is the TS case "warns instead
// of silently skipping when pane cwd is missing".
func TestObservabilityRpcServerWarnsOnMissingPaneCwd(t *testing.T) {
	var (
		mu       sync.Mutex
		warnings []string
	)
	fixture := obsOpenRPCServer(t, obsRPCServerOptions{
		peerPidOf: obsSelfPidPeer,
		// Herdr answers the pane identity without any cwd at all.
		resolvePaneIdentity: obsPaneIdentityOf(nil),
		warn: func(message string, _ any) {
			mu.Lock()
			warnings = append(warnings, message)
			mu.Unlock()
		},
	})
	obsSeedAgent(t, fixture.harness, fixture.dir)

	registered := obsCall(t, fixture.client, "agent.orchestrator.register", obsRegisterParams("wB:p1"))
	assertObsSubset(t, obsAt(t, registered, "presence"), map[string]any{"terminalId": "term_1"})

	mu.Lock()
	defer mu.Unlock()
	for _, message := range warnings {
		if strings.Contains(message, "pane cwd") {
			return
		}
	}
	t.Fatalf("expected a pane cwd warning, got %v", warnings)
}
