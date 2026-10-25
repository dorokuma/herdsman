package agy_test

// This file ports lines 1882-2500 of test/unit/agy-hook-and-cli.test.ts: the
// standby WakeWaiter, the detached waiter cwd resolution and the two
// ObservabilityRpcServer cases that close the TS file (the agy terminal
// presence lease and cross-workspace orchestration).
//
// The two RPC cases drive a real unix socket against a migrated database, so
// the file carries the same scaffolding the daemon-side observability ports
// use: a migrated harness, a fake agent-history service (the case touches no
// history), the real context/orchestrator/index services and the production RPC
// client. Everything lives under t.TempDir(), so the production home and the
// running daemon are never touched.

import (
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/agenthistory"
	"github.com/dorokuma/herdsman/go/internal/agy"
	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// --- Stop keeps a standby WakeWaiter until the first worker (1882-2008) ------

func TestStopKeepsStandbyWaiterUntilFirstWorkerCompletion(t *testing.T) {
	dir := t.TempDir()
	sessionID := "c1111111-2222-3333-4444-555555555555"
	pendingEvents := []any{}
	peerListed := false
	peerStatus := "working"
	spawnedWaiters := []float64{}
	wakePrompts := []wakePrompt{}

	client := &concClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.list" {
			agents := []any{agentListEntry("agy", "idle", "2026-10-21T11:50:00.000Z", "wB:p1", "term_self")}
			if peerListed {
				agents = append(agents, agentListEntry("pi", peerStatus, "2026-10-21T11:59:00.000Z", "wB:p2", "term_peer"))
			}
			return map[string]any{"agents": agents}, nil
		}
		return orchestratorStateResponse(sessionID, pendingEvents, 0), nil
	}

	// 1. The script-dispatched worker is not in agent.list yet: no active peer,
	// but Stop still spawns the guard waiter.
	stopResult := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, agy.RunAgyHookOptions{
		Connect:     client.connect,
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
		SpawnWakeWaiter: func(input agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
			spawnedWaiters = append(spawnedWaiters, input.WaiterEpoch)
			return &agy.SpawnWakeWaiterResult{PID: intPtr(os.Getpid())}
		},
	})
	if !isEmptyHookOutput(stopResult) {
		t.Fatalf("stop result = %+v, want {}", stopResult)
	}
	if want := []float64{1}; !reflect.DeepEqual(spawnedWaiters, want) {
		t.Fatalf("spawnedWaiters = %v, want %v", spawnedWaiters, want)
	}

	done := make(chan agy.HookOutput, 1)
	go func() {
		done <- agy.RunAgyHook(agy.HookWakeWaiter, "", agy.RunAgyHookOptions{
			Connect: client.connectCapturing,
			Environment: mergeEnv(herdrEnv,
				"HERDSMAN_AGY_SESSION_ID", sessionID,
				"HERDSMAN_AGY_STATE_DIR", dir,
				"HERDSMAN_AGY_WAITER_EPOCH", "1"),
			PollIntervalMs: floatPtr(25),
			SocketPath:     "/tmp/herdsman.sock",
			StateDir:       strPtr(dir),
			TriggerWakePrompt: func(paneID, promptText string) error {
				client.mu.Lock()
				wakePrompts = append(wakePrompts, wakePrompt{paneID, promptText})
				client.mu.Unlock()
				return nil
			},
			WaitTimeoutMs: floatPtr(5000),
		})
	}()

	// 2. No peer yet: the waiter stays on standby instead of exiting.
	time.Sleep(60 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the waiter exited while no peer was indexed")
	default:
	}

	// 3. The worker appears and completes; its push immediately wakes the pane.
	client.mu.Lock()
	peerListed = true
	peerStatus = "idle"
	pendingEvents = []any{makeDoneEvent(300, "脚本派发的工人完成。")}
	client.mu.Unlock()
	client.push(makeDoneEvent(300, "脚本派发的工人完成。"))

	if result := awaitHookOutput(t, done); !isEmptyHookOutput(result) {
		t.Fatalf("waiter result = %+v, want {}", result)
	}
	client.mu.Lock()
	prompts := append([]wakePrompt{}, wakePrompts...)
	client.mu.Unlock()
	if want := []wakePrompt{{paneID: "wB:p1", promptText: "[herdsman-wake]"}}; !reflect.DeepEqual(prompts, want) {
		t.Fatalf("wakePrompts = %+v, want %+v", prompts, want)
	}
}

// --- WakeWaiter cwd from workspacePaths (2009-2097) --------------------------

func TestStopResolvesWakeWaiterCwdFromWorkspacePaths(t *testing.T) {
	dir := t.TempDir()
	sessionID := "d1111111-2222-3333-4444-555555555555"
	spawnedCwds := []string{}

	// agy runs the global hooks with cwd = the directory holding hooks.json
	// (production: /root/.gemini/config) and reports the pane directory only as
	// `workspacePaths`, so the payload below mirrors a real Stop payload.
	client := &hookFakeClient{}
	client.respond = func(method string, params any) (any, error) {
		if method == "agent.list" {
			return map[string]any{"agents": []any{}}, nil
		}
		return orchestratorStateResponse(sessionID, []any{}, 0), nil
	}

	stopResult := agy.RunAgyHook(agy.HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL","workspacePaths":["/workspace/pi-extensions"]}`, agy.RunAgyHookOptions{
		Connect:     connectReturning(client),
		Environment: herdrEnv,
		SocketPath:  "/tmp/herdsman.sock",
		StateDir:    strPtr(dir),
		SpawnWakeWaiter: func(input agy.SpawnWakeWaiterInput) *agy.SpawnWakeWaiterResult {
			spawnedCwds = append(spawnedCwds, input.Cwd)
			return &agy.SpawnWakeWaiterResult{PID: intPtr(os.Getpid())}
		},
	})

	// Without the fix the waiter would inherit hooks.json's directory, which
	// pins PPid=1 after Stop exits and makes the daemon reject it with
	// ConnectorPaneMismatchError.
	if !isEmptyHookOutput(stopResult) {
		t.Fatalf("stop result = %+v, want {}", stopResult)
	}
	if want := []string{"/workspace/pi-extensions"}; !reflect.DeepEqual(spawnedCwds, want) {
		t.Fatalf("spawnedCwds = %v, want %v", spawnedCwds, want)
	}
	if workingDir, err := os.Getwd(); err == nil && spawnedCwds[0] == workingDir {
		t.Fatalf("spawnedCwds[0] = %q, want the workspace path, not the hook cwd", spawnedCwds[0])
	}
	assertStateMatches(t, readStateObject(t, sessionStatePath(dir, sessionID)), map[string]any{
		"turnEpoch": float64(1), "waiterEpoch": float64(1), "waiterPid": float64(os.Getpid()),
	})

	// Priority: payload.cwd > workspacePaths.
	if got := agy.ResolveWakeWaiterCwd(map[string]any{
		"cwd": "/workspace/pi-extensions", "workspacePaths": []any{"/workspace/other"},
	}, nil); got != "/workspace/pi-extensions" {
		t.Fatalf("resolveWakeWaiterCwd(payload.cwd) = %q", got)
	}
	// A workspacePaths entry wins even when it is not a real directory on disk.
	if got := agy.ResolveWakeWaiterCwd(map[string]any{
		"workspacePaths": []any{"/workspace/pi-extensions"},
	}, nil); got != "/workspace/pi-extensions" {
		t.Fatalf("resolveWakeWaiterCwd(workspacePaths) = %q", got)
	}
	// Empty entries are skipped instead of yielding a cwd of "".
	if got := agy.ResolveWakeWaiterCwd(map[string]any{
		"workspacePaths": []any{"", "/workspace/pi-extensions"},
	}, nil); got != "/workspace/pi-extensions" {
		t.Fatalf("resolveWakeWaiterCwd(empty entry) = %q", got)
	}
	// No payload directory and no readable ancestor (bogus pid): baseCwd.
	if got := agy.ResolveWakeWaiterCwd(map[string]any{}, &agy.ResolveWakeWaiterCwdOptions{
		BaseCwd: strPtr("/tmp/herdsman-wake-cwd-base"),
		PID:     4_000_000,
	}); got != "/tmp/herdsman-wake-cwd-base" {
		t.Fatalf("resolveWakeWaiterCwd(baseCwd) = %q", got)
	}
}

// ---------------------------------------------------------------------------
// ObservabilityRpcServer scaffolding
// ---------------------------------------------------------------------------

// agyRPCFakeHistory is the fake agent-history service: enough of both
// observability.AgentHistoryServiceLike and daemon.ObservabilityHistoryService
// to build the context service and the RPC server, with the module-level helpers
// forwarded to the real agent-history module. Neither case asserts history
// content — the TS fixtures hand the server the real service, which finds no
// session files for the synthetic pane paths either.
type agyRPCFakeHistory struct{}

func (agyRPCFakeHistory) ResolveCompactHistory(
	_ observability.AgentHistoryLookupInput,
	_ observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	source := "pi-jsonl"
	return observability.AgentHistoryResolved{
		CompactHistory: observability.CompactAgentHistory{
			Source:               &source,
			LastAssistantMessage: &observability.AgentHistoryExcerpt{Ref: "history", Text: "turn complete"},
		},
	}
}

func (agyRPCFakeHistory) Read(
	_ observability.AgentHistoryLookupInput,
	_ daemon.ObservabilityHistoryReadOptions,
) daemon.ObservabilityHistoryReadResult {
	return daemon.ObservabilityHistoryReadResult{}
}

func (agyRPCFakeHistory) StatSourceFingerprint(path string) *observability.AgentHistorySourceFingerprint {
	fingerprint := agenthistory.StatSourceFingerprint(path)
	if fingerprint == nil {
		return nil
	}
	return &observability.AgentHistorySourceFingerprint{
		MtimeMs: fingerprint.MtimeMs,
		Path:    fingerprint.Path,
		Size:    float64(fingerprint.Size),
	}
}

func (agyRPCFakeHistory) HistorySourceFromSessionRef(
	ref observability.AgentSessionRef,
) observability.AgentHistoryRefSource {
	return observability.AgentHistoryRefSource(agenthistory.HistorySourceFromSessionRef(ref))
}

func (agyRPCFakeHistory) SafeOfficialSessionPath(value string) string {
	return agenthistory.SafeOfficialSessionPath(value)
}

func (agyRPCFakeHistory) SafeAllowedSessionPath(value string) string {
	return agenthistory.SafeAllowedSessionPath(value, nil)
}

func (agyRPCFakeHistory) SessionPathAllowedByShape(value string) bool {
	return agenthistory.SessionPathAllowedByShape(value, nil)
}

// agyRPCHarness mirrors openObservabilityDbHarness: one migrated database with
// every store the observability server and its services take.
type agyRPCHarness struct {
	sqlite                *sql.DB
	agentContextSnapshots *db.AgentContextSnapshotStore
	agentEvents           *db.AgentEventStore
	agentHistoryCache     *db.AgentHistoryCacheStore
	scopes                *db.AgentOrchestratorScopeStore
	agents                *db.AgentStore
	herdrSessions         *db.HerdrSessionStore
	herdrWorkspaces       *db.HerdrWorkspaceStore
	statusEventPlans      *db.StatusEventPlanStore
}

func openAgyRPCHarness(t *testing.T) *agyRPCHarness {
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
	return &agyRPCHarness{
		sqlite:                sqlite,
		agentContextSnapshots: db.NewAgentContextSnapshotStore(sqlite),
		agentEvents:           agentEvents,
		agentHistoryCache:     db.NewAgentHistoryCacheStore(sqlite),
		scopes:                db.NewAgentOrchestratorScopeStore(sqlite),
		agents:                db.NewAgentStore(sqlite, agentEvents),
		herdrSessions:         db.NewHerdrSessionStore(sqlite),
		herdrWorkspaces:       db.NewHerdrWorkspaceStore(sqlite),
		statusEventPlans:      db.NewStatusEventPlanStore(sqlite),
	}
}

// upsertRunning writes the running "default" Herdr session row.
func (h *agyRPCHarness) upsertRunning(t *testing.T) {
	t.Helper()
	if _, err := h.herdrSessions.UpsertRunning(db.UpsertRunningHerdrSessionInput{
		Name:       "default",
		SessionDir: "/tmp/herdr-default",
		SocketPath: "/tmp/herdr-default.sock",
	}); err != nil {
		t.Fatalf("upsertRunning: %v", err)
	}
}

// replaceAgents mirrors `harness.agents.replaceForSession({ agents, herdrSessionName })`.
func (h *agyRPCHarness) replaceAgents(t *testing.T, agents []db.HerdrAgentLike) []observability.AgentIndexRecord {
	t.Helper()
	replaced, err := h.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents:           agents,
		HerdrSessionName: "default",
	})
	if err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	return replaced
}

func (h *agyRPCHarness) orchestrator() *observability.AgentOrchestratorService {
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

func (h *agyRPCHarness) contextService() *observability.AgentContextService {
	return observability.NewAgentContextService(struct {
		History observability.AgentHistoryServiceLike
		Stores  struct {
			AgentContextSnapshots observability.AgentContextSnapshotStoreLike
			Agents                observability.AgentContextAgentStoreLike
		}
	}{
		History: agyRPCFakeHistory{},
		Stores: struct {
			AgentContextSnapshots observability.AgentContextSnapshotStoreLike
			Agents                observability.AgentContextAgentStoreLike
		}{
			AgentContextSnapshots: h.agentContextSnapshots,
			Agents:                h.agents,
		},
	})
}

func (h *agyRPCHarness) indexService(t *testing.T) *observability.AgentIndexService {
	t.Helper()
	service, err := observability.NewAgentIndexService(observability.AgentIndexServiceOptions{
		Context:      h.contextService(),
		History:      agyRPCFakeHistory{},
		SessionPaths: agyRPCFakeHistory{},
		Stores: observability.AgentIndexServiceStores{
			AgentContextSnapshots:   h.agentContextSnapshots,
			AgentEvents:             h.agentEvents,
			AgentHistoryCache:       h.agentHistoryCache,
			AgentOrchestratorScopes: h.scopes,
			Agents:                  h.agents,
			HerdrSessions:           h.herdrSessions,
			HerdrWorkspaces:         h.herdrWorkspaces,
			Sqlite:                  h.sqlite,
			StatusEventPlans:        h.statusEventPlans,
		},
	})
	if err != nil {
		t.Fatalf("newAgentIndexService: %v", err)
	}
	return service
}

// agyRPCServerFixture is the started server plus the services the cases read.
type agyRPCServerFixture struct {
	server       *daemon.ObservabilityRpcServer
	orchestrator *observability.AgentOrchestratorService
	index        *observability.AgentIndexService
	socketPath   string
}

// startAgyRPCServer mirrors the TS `new ObservabilityRpcServer({ ... })` plus
// `await server.start()`.
func startAgyRPCServer(
	t *testing.T,
	harness *agyRPCHarness,
	resolvePaneIdentity func(daemon.ResolvePaneIdentityInput) (herdr.HerdrPaneIdentity, error),
) agyRPCServerFixture {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "herdsman.sock")
	orchestrator := harness.orchestrator()
	index := harness.indexService(t)
	server := daemon.NewObservabilityRpcServer(daemon.ObservabilityRpcServerOptions{
		Context:             harness.contextService(),
		DisconnectGraceMs:   25,
		History:             agyRPCFakeHistory{},
		Orchestrator:        orchestrator,
		PeerPidOf:           func(net.Conn) (int, bool) { return os.Getpid(), true },
		ResolvePaneIdentity: resolvePaneIdentity,
		SocketPath:          socketPath,
		Stores: daemon.ObservabilityAgentStores{
			AgentEvents:     harness.agentEvents,
			Agents:          harness.agents,
			HerdrSessions:   harness.herdrSessions,
			HerdrWorkspaces: harness.herdrWorkspaces,
		},
		Warn: func(string, any) {},
	})
	if err := server.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = server.Stop() })
	return agyRPCServerFixture{server: server, orchestrator: orchestrator, index: index, socketPath: socketPath}
}

// dialAgyRPC opens one connection with the production RPC client.
func dialAgyRPC(t *testing.T, socketPath string) *daemon.ObservabilityRpcClient {
	t.Helper()
	client, err := daemon.NewObservabilityRpcClient(daemon.ObservabilityRpcClientOptions{
		SocketPath: socketPath,
	})
	if err != nil {
		t.Fatalf("dial %s: %v", socketPath, err)
	}
	t.Cleanup(client.Close)
	return client
}

// agyRPCRequest mirrors `await client.request(method, params)`: a failed call
// fails the test.
func agyRPCRequest(
	t *testing.T,
	client *daemon.ObservabilityRpcClient,
	method string,
	params any,
) any {
	t.Helper()
	result, err := client.Call(method, params)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return result
}

// agyRPCInto mirrors the TS casts on the request result.
func agyRPCInto(
	t *testing.T,
	client *daemon.ObservabilityRpcClient,
	method string,
	params any,
	target any,
) {
	t.Helper()
	result := agyRPCRequest(t, client, method, params)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("%s: marshal: %v", method, err)
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		t.Fatalf("%s: unmarshal: %v", method, err)
	}
}

// agyRPCRegister is the TS register block: one sessionRef registration.
func agyRPCRegister(
	t *testing.T,
	client *daemon.ObservabilityRpcClient,
	paneID string,
	sessionRef map[string]any,
	subscriberID string,
	subscriberKind string,
	workspaceID string,
) {
	t.Helper()
	agyRPCRequest(t, client, "agent.orchestrator.register", map[string]any{
		"herdrSocketPath": "/tmp/herdr-default.sock",
		"paneId":          paneID,
		"sessionRef":      sessionRef,
		"subscriberId":    subscriberID,
		"subscriberKind":  subscriberKind,
		"workspaceId":     workspaceID,
	})
}

// agyRPCOwners is the TS `orchestrator.status(scope)?.owner` pair the cases
// assert on.
func agyRPCOwner(t *testing.T, orchestrator *observability.AgentOrchestratorService, scope observability.AgentScope) *observability.AgentOrchestratorOwner {
	t.Helper()
	state, err := orchestrator.Status(scope)
	if err != nil {
		t.Fatalf("orchestrator.status: %v", err)
	}
	if state == nil {
		return nil
	}
	return state.Owner
}

func agyRPCScope() observability.AgentScope {
	return observability.AgentScope{HerdrSessionName: "default", WorkspaceID: "wB"}
}

// --- ObservabilityRpcServer agy terminal presence lease (2099-2276) ----------

func TestAgyTerminalPresenceLeaseAcrossHookSocketCloses(t *testing.T) {
	harness := openAgyRPCHarness(t)
	harness.upsertRunning(t)
	harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent": "agy", "agent_status": "idle", "cwd": "/repo/a", "focused": true,
			"herdr_session_name": "default", "name": "main-agy", "pane_generation": "gen-agy-1",
			"pane_id": "wB:p-agy", "tab_id": "wB:t1", "terminal_id": "term_agy", "workspace_id": "wB",
		},
		{
			"agent": "pi", "agent_status": "idle", "cwd": "/repo/a", "focused": false,
			"herdr_session_name": "default", "name": "peer-pi", "pane_generation": "gen-pi-1",
			"pane_id": "wB:p-pi", "tab_id": "wB:t2", "terminal_id": "term_pi", "workspace_id": "wB",
		},
	})

	fixture := startAgyRPCServer(t, harness, func(input daemon.ResolvePaneIdentityInput) (herdr.HerdrPaneIdentity, error) {
		workingDir, _ := os.Getwd()
		terminalID := "term_pi"
		if input.PaneID == "wB:p-agy" {
			terminalID = "term_agy"
		}
		return herdr.HerdrPaneIdentity{
			PaneID:        input.PaneID,
			TerminalID:    terminalID,
			WorkspaceID:   "wB",
			CWD:           &workingDir,
			ForegroundCwd: &workingDir,
		}, nil
	})

	agyHookClient := dialAgyRPC(t, fixture.socketPath)
	agyRPCRegister(t, agyHookClient, "wB:p-agy", map[string]any{
		"agent": "agy", "kind": "id", "source": "agy-hook",
		"value": "11111111-2222-3333-4444-555555555555",
	}, "agy:11111111-2222-3333-4444-555555555555", "agy", "wB")
	agyRPCRequest(t, agyHookClient, "agent.orchestrator.set", map[string]any{"enabled": true})
	// Close the short-lived hook socket and wait past disconnectGraceMs (25ms).
	agyHookClient.Close()
	time.Sleep(60 * time.Millisecond)

	// Ownership and terminal presence must stay active because the Herdr pane is
	// still open.
	if !fixture.server.IsTerminalConnected(daemon.ConnectedTerminalInput{
		HerdrSessionName: "default", TerminalID: "term_agy",
	}) {
		t.Fatal("the agy terminal presence was dropped by the short hook socket close")
	}
	if owner := agyRPCOwner(t, fixture.orchestrator, agyRPCScope()); owner == nil || owner.TerminalID != "term_agy" {
		t.Fatalf("owner = %+v, want term_agy", owner)
	}

	// Production Herdr pane.closed carries pane_generation (no terminal_id);
	// both the owner and the agy lease must clear.
	if _, err := fixture.index.HandleHerdrEvent(observability.HandleHerdrEventInput{
		Event: map[string]any{
			"pane_generation": "gen-agy-1",
			"pane_id":         "wB:p-agy",
			"type":            "pane.closed",
		},
		HerdrSessionName: "default",
		SessionDir:       "/tmp/herdr-default",
		SocketPath:       "/tmp/herdr-default.sock",
	}); err != nil {
		t.Fatalf("handleHerdrEvent(pane.closed): %v", err)
	}
	if fixture.server.IsTerminalConnected(daemon.ConnectedTerminalInput{
		HerdrSessionName: "default", TerminalID: "term_agy",
	}) {
		t.Fatal("the agy lease survived a generation-scoped pane.closed")
	}
	if owner := agyRPCOwner(t, fixture.orchestrator, agyRPCScope()); owner != nil {
		t.Fatalf("owner = %+v, want null after pane.closed", owner)
	}

	// Now pi claims ownership on wB:p-pi, and a CLI status check with
	// subscriberKind="agy" runs in wB:p-pi.
	piClient := dialAgyRPC(t, fixture.socketPath)
	agyRPCRegister(t, piClient, "wB:p-pi", map[string]any{
		"agent": "pi", "kind": "path", "source": "herdr:pi", "value": "/tmp/pi-session.jsonl",
	}, "pi:term_pi", "pi", "wB")
	agyRPCRequest(t, piClient, "agent.orchestrator.set", map[string]any{"enabled": true})
	if owner := agyRPCOwner(t, fixture.orchestrator, agyRPCScope()); owner == nil || owner.TerminalID != "term_pi" {
		t.Fatalf("owner = %+v, want term_pi", owner)
	}

	cliInPiPane := dialAgyRPC(t, fixture.socketPath)
	agyRPCRegister(t, cliInPiPane, "wB:p-pi", map[string]any{
		"agent": "agy", "kind": "id", "source": "herdsman-cli", "value": "herdsman-cli",
	}, "cli:wB:p-pi", "agy", "wB")
	cliInPiPane.Close()

	// When piClient disconnects, the CLI's subscriberKind="agy" registration must
	// NOT block pi's disconnect grace release.
	piClient.Close()
	time.Sleep(60 * time.Millisecond)
	if fixture.server.IsTerminalConnected(daemon.ConnectedTerminalInput{
		HerdrSessionName: "default", TerminalID: "term_pi",
	}) {
		t.Fatal("the pi terminal presence outlived its disconnect grace")
	}
	if owner := agyRPCOwner(t, fixture.orchestrator, agyRPCScope()); owner != nil {
		t.Fatalf("owner = %+v, want null after the grace release", owner)
	}
}

// --- ObservabilityRpcServer cross-workspace orchestration (2277-2500) --------

func TestCrossWorkspaceOrchestrationSurfacesOwnerlessSibling(t *testing.T) {
	harness := openAgyRPCHarness(t)
	harness.upsertRunning(t)
	harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent": "agy", "agent_status": "idle", "cwd": "/repo/a", "focused": true,
			"herdr_session_name": "default", "name": "main-agy", "pane_generation": "gen-main",
			"pane_id": "wB:p1", "tab_id": "wB:t1", "terminal_id": "term_main", "workspace_id": "wB",
		},
		{
			"agent": "pi", "agent_status": "done", "cwd": "/repo/b", "focused": false,
			"herdr_session_name": "default", "name": "cross-worker", "pane_generation": "gen-cross",
			"pane_id": "wC:p1", "tab_id": "wC:t1", "terminal_id": "term_cross", "workspace_id": "wC",
		},
	})

	fixture := startAgyRPCServer(t, harness, func(input daemon.ResolvePaneIdentityInput) (herdr.HerdrPaneIdentity, error) {
		workingDir, _ := os.Getwd()
		terminalID := "term_cross"
		workspaceID := "wC"
		if input.PaneID == "wB:p1" {
			terminalID = "term_main"
			workspaceID = "wB"
		}
		return herdr.HerdrPaneIdentity{
			PaneID:        input.PaneID,
			TerminalID:    terminalID,
			WorkspaceID:   workspaceID,
			CWD:           &workingDir,
			ForegroundCwd: &workingDir,
		}, nil
	})

	// The TS reads `harness.agents.list({ workspaceId: "wC" })[0]`; the pane
	// lookup is the same indexed row.
	crossAgent, err := harness.agents.FindByPane(db.FindAgentByPaneInput{
		HerdrSessionName: "default",
		PaneID:           "wC:p1",
	})
	if err != nil {
		t.Fatalf("findByPane(wC:p1): %v", err)
	}
	if crossAgent == nil {
		t.Fatal("expected the cross-workspace agent to be indexed")
	}

	crossCompactHistory := &observability.CompactAgentHistory{
		HistoryRef: &observability.AgentHistoryRef{
			Kind:   observability.HistoryRefAgentSession,
			Path:   strPtr("/tmp/cross-session.jsonl"),
			Source: observability.HistorySourcePiJSONL,
			Value:  "/tmp/cross-session.jsonl",
		},
		LastAssistantMessage: &observability.AgentHistoryExcerpt{
			Ref:       "cross-1",
			Text:      "跨工作区工人完成。",
			Timestamp: strPtr("2026-10-21T12:00:00.000Z"),
		},
		MessageCount: 2,
		Source:       strPtr("pi-jsonl"),
		UpdatedAt:    strPtr("2026-10-21T12:00:00.000Z"),
	}
	appendCrossDone := func(agentKind string) observability.AgentEventRecord {
		record, appendErr := harness.agentEvents.Append(db.AppendAgentEventInput{
			AgentID:          strPtr(crossAgent.ID),
			CompactHistory:   crossCompactHistory,
			HerdrSessionName: "default",
			PaneGeneration:   strPtr("gen-cross"),
			PaneID:           strPtr("wC:p1"),
			Payload:          map[string]any{"agent": "pi", "from": "working", "name": agentKind, "to": "done"},
			TerminalID:       strPtr("term_cross"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      strPtr("wC"),
		})
		if appendErr != nil {
			t.Fatalf("append cross done: %v", appendErr)
		}
		return record
	}

	mainClient := dialAgyRPC(t, fixture.socketPath)
	agyRPCRegister(t, mainClient, "wB:p1", map[string]any{
		"agent": "agy", "kind": "id", "source": "agy-hook",
		"value": "11111111-2222-3333-4444-555555555555",
	}, "agy:11111111-2222-3333-4444-555555555555", "agy", "wB")
	agyRPCRequest(t, mainClient, "agent.orchestrator.set", map[string]any{"enabled": true})

	// The ownerless sibling workspace is folded into the orchestrated agent
	// list...
	var expanded struct {
		Agents []struct {
			PaneID string `json:"paneId"`
		} `json:"agents"`
	}
	agyRPCInto(t, mainClient, "agent.list", map[string]any{
		"herdrSessionName":           "default",
		"includeOwnerlessWorkspaces": true,
		"workspaceId":                "wB",
	}, &expanded)
	expandedPaneIDs := make([]string, 0, len(expanded.Agents))
	for _, agent := range expanded.Agents {
		expandedPaneIDs = append(expandedPaneIDs, agent.PaneID)
	}
	sortStrings(expandedPaneIDs)
	if want := []string{"wB:p1", "wC:p1"}; !reflect.DeepEqual(expandedPaneIDs, want) {
		t.Fatalf("expanded agents = %v, want %v", expandedPaneIDs, want)
	}

	// ...only on the explicit opt-in.
	var plain struct {
		Agents []struct {
			PaneID string `json:"paneId"`
		} `json:"agents"`
	}
	agyRPCInto(t, mainClient, "agent.list", map[string]any{
		"herdrSessionName": "default",
		"workspaceId":      "wB",
	}, &plain)
	plainPaneIDs := make([]string, 0, len(plain.Agents))
	for _, agent := range plain.Agents {
		plainPaneIDs = append(plainPaneIDs, agent.PaneID)
	}
	if want := []string{"wB:p1"}; !reflect.DeepEqual(plainPaneIDs, want) {
		t.Fatalf("plain agents = %v, want %v", plainPaneIDs, want)
	}

	// A completion from the ownerless workspace is delivered to the session owner
	// and can be acknowledged across workspaces.
	crossEvent := appendCrossDone("cross-worker-1")
	var delivered struct {
		Events []observability.AgentEventRecord `json:"events"`
	}
	agyRPCInto(t, mainClient, "agent.orchestrator.get", map[string]any{}, &delivered)
	if !eventIDsContain(delivered.Events, crossEvent.ID) {
		t.Fatalf("orchestrator.get events = %v, want %d", delivered.Events, crossEvent.ID)
	}
	acked := agyRPCRequest(t, mainClient, "agent.notifications.ack", map[string]any{"eventId": crossEvent.ID})
	if object, ok := acked.(map[string]any); !ok || object["acknowledged"] != true {
		t.Fatalf("ack result = %#v, want acknowledged true", acked)
	}

	// Once that workspace owns its own orchestrator, it leaves the main owner's
	// visibility again: no agent list, no events, no cross-workspace ack.
	crossClient := dialAgyRPC(t, fixture.socketPath)
	agyRPCRegister(t, crossClient, "wC:p1", map[string]any{
		"agent": "pi", "kind": "path", "source": "herdr:pi", "value": "/tmp/cross-session.jsonl",
	}, "pi:term_cross", "pi", "wC")
	agyRPCRequest(t, crossClient, "agent.orchestrator.set", map[string]any{"enabled": true})

	var narrowed struct {
		Agents []struct {
			PaneID string `json:"paneId"`
		} `json:"agents"`
	}
	agyRPCInto(t, mainClient, "agent.list", map[string]any{
		"herdrSessionName":           "default",
		"includeOwnerlessWorkspaces": true,
		"workspaceId":                "wB",
	}, &narrowed)
	narrowedPaneIDs := make([]string, 0, len(narrowed.Agents))
	for _, agent := range narrowed.Agents {
		narrowedPaneIDs = append(narrowedPaneIDs, agent.PaneID)
	}
	if want := []string{"wB:p1"}; !reflect.DeepEqual(narrowedPaneIDs, want) {
		t.Fatalf("narrowed agents = %v, want %v", narrowedPaneIDs, want)
	}

	ownedEvent := appendCrossDone("cross-worker-2")
	var afterOwnership struct {
		Events []observability.AgentEventRecord `json:"events"`
	}
	agyRPCInto(t, mainClient, "agent.orchestrator.get", map[string]any{}, &afterOwnership)
	if eventIDsContain(afterOwnership.Events, ownedEvent.ID) {
		t.Fatalf("orchestrator.get events = %v, want no event %d", afterOwnership.Events, ownedEvent.ID)
	}
	_, ackErr := mainClient.Call("agent.notifications.ack", map[string]any{"eventId": ownedEvent.ID})
	if ackErr == nil {
		t.Fatal("the cross-workspace ack of an owned workspace event succeeded")
	}
	if !regexp.MustCompile(`(?i)next pending orchestrator event|not in scope|no longer pending`).MatchString(ackErr.Error()) {
		t.Fatalf("cross-workspace ack error = %v", ackErr)
	}
}

// sortStrings sorts in place (the TS `.sort()` of the pane ids).
func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current] < values[current-1]; current-- {
			values[current], values[current-1] = values[current-1], values[current]
		}
	}
}

// eventIDsContain mirrors `events.map(e => e.id).toContain(id)`.
func eventIDsContain(events []observability.AgentEventRecord, id int64) bool {
	for _, event := range events {
		if event.ID == id {
			return true
		}
	}
	return false
}
