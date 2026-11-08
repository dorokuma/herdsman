package daemon_test

// Tests for the Claude Code registration path: subscriberKind "claude" binds
// its uuid sessionRef through the same registerPiSessionRef channel and, being
// a one-shot CLI connection, is exempt from the socket-disconnect recovery
// exactly like agy — a closed socket is not a terminal disconnect.

import (
	"path/filepath"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

const (
	obsClaudePane      = "wB:p-claude"
	obsClaudeTerm      = "term_claude"
	obsClaudeSessionID = "3a4b5c6d-7e8f-4a1b-9c2d-3e4f5a6b7c8d"
)

// obsClaudeRegisterParams is the payload `herdsman register-claude` sends.
func obsClaudeRegisterParams(sessionID string) map[string]any {
	return map[string]any{
		"herdrSocketPath": obsHerdrSocketPath,
		"paneId":          obsClaudePane,
		"sessionRef": map[string]any{
			"agent":  "claude",
			"kind":   "id",
			"source": "dispatch-agent:claude",
			"value":  sessionID,
		},
		"subscriberId":   "claude:" + sessionID,
		"subscriberKind": "claude",
		"workspaceId":    obsWorkspaceB,
	}
}

// obsSeedClaudePane indexes one claude-labelled pane for the running session.
func obsSeedClaudePane(t *testing.T, harness *obsHarness) {
	t.Helper()
	harness.upsertRunningSession(t)
	harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent":        "claude",
			"agent_status": "working",
			"pane_id":      obsClaudePane,
			"terminal_id":  obsClaudeTerm,
			"workspace_id": obsWorkspaceB,
		},
	})
}

func TestClaudeRegisterBindsTheSessionRefAndSurvivesSocketClose(t *testing.T) {
	harness := openObsHarness(t)
	obsSeedClaudePane(t, harness)

	var registered *observability.RegisterPiSessionRefInput
	setup := openObsServerWithOptions(t, harness, daemon.ObservabilityRpcServerOptions{
		RegisterPiSessionRef: func(
			input observability.RegisterPiSessionRefInput,
		) (*observability.PiSessionRefRegistrationResult, error) {
			captured := input
			registered = &captured
			return &observability.PiSessionRefRegistrationResult{}, nil
		},
	})

	client := obsDial(t, setup.socketPath)
	registeredResult := obsRequest(t, client, "agent.orchestrator.register",
		obsClaudeRegisterParams(obsClaudeSessionID))
	assertObsSubset(t, obsAt(t, registeredResult, "presence"), map[string]any{
		"paneId":     obsClaudePane,
		"terminalId": obsClaudeTerm,
	})

	if registered == nil {
		t.Fatal("registerPiSessionRef was not called for the claude registration")
	}
	if registered.TerminalID != obsClaudeTerm ||
		registered.HerdrSessionName != obsSessionName ||
		registered.SessionRef.Agent != "claude" ||
		registered.SessionRef.Kind != observability.SessionRefID ||
		registered.SessionRef.Value != obsClaudeSessionID {
		t.Fatalf("registerPiSessionRef input = %+v, want the claude id ref", registered)
	}

	// The one-shot CLI closes its socket right after the register RPC: the
	// terminal must keep its presence instead of starting disconnect reclaim.
	client.close()
	obsSocketTick()
	if !setup.server.IsTerminalConnected(daemon.ConnectedTerminalInput{
		HerdrSessionName: obsSessionName,
		TerminalID:       obsClaudeTerm,
	}) {
		t.Fatal("the claude terminal lost presence when its short-lived socket closed")
	}
}

func TestClaudeRegisterSkipsANonUuidSessionRefValue(t *testing.T) {
	harness := openObsHarness(t)
	obsSeedClaudePane(t, harness)

	called := 0
	setup := openObsServerWithOptions(t, harness, daemon.ObservabilityRpcServerOptions{
		RegisterPiSessionRef: func(
			input observability.RegisterPiSessionRefInput,
		) (*observability.PiSessionRefRegistrationResult, error) {
			called++
			return &observability.PiSessionRefRegistrationResult{}, nil
		},
	})

	client := obsDial(t, setup.socketPath)
	obsRequest(t, client, "agent.orchestrator.register", obsClaudeRegisterParams("not-a-uuid"))

	if called != 0 {
		t.Fatalf("registerPiSessionRef calls = %d, want 0 for a non-uuid value", called)
	}
	agent, err := harness.agents.FindByPane(db.FindAgentByPaneInput{
		HerdrSessionName: obsSessionName,
		PaneID:           obsClaudePane,
	})
	if err != nil {
		t.Fatalf("findByPane: %v", err)
	}
	if agent == nil {
		t.Fatal("indexed claude pane missing")
	}
	if agent.AgentSession != nil {
		t.Fatalf("agentSession = %+v, want nil for the skipped registration", agent.AgentSession)
	}

	// The skipped registration must not leave the socket-less lease behind
	// either: a sessionRef that cannot bind grants no agyPresenceByTerminal
	// entry, so closing the short-lived socket disconnects the terminal.
	client.close()
	obsSocketTick()
	if setup.server.IsTerminalConnected(daemon.ConnectedTerminalInput{
		HerdrSessionName: obsSessionName,
		TerminalID:       obsClaudeTerm,
	}) {
		t.Fatal("the non-uuid claude registration kept terminal presence after its socket closed")
	}
}

func TestClaudeRegisterSkippedOnAPiTerminal(t *testing.T) {
	harness := openObsHarness(t)
	// The pane carries a pi label (and a pi session ref), yet the misdirected
	// `herdsman register-claude` CLI call arrives as subscriberKind "claude".
	harness.upsertRunningSession(t)
	harness.replaceAgents(t, []db.HerdrAgentLike{
		{
			"agent": "pi",
			"agent_session": map[string]any{
				"agent":  "pi",
				"kind":   "path",
				"source": "pi-jsonl",
				"value":  obsPiSessionFile,
			},
			"agent_status": "working",
			"pane_id":      obsClaudePane,
			"terminal_id":  obsClaudeTerm,
			"workspace_id": obsWorkspaceB,
		},
	})

	called := 0
	setup := openObsServerWithOptions(t, harness, daemon.ObservabilityRpcServerOptions{
		RegisterPiSessionRef: func(
			input observability.RegisterPiSessionRefInput,
		) (*observability.PiSessionRefRegistrationResult, error) {
			called++
			return &observability.PiSessionRefRegistrationResult{}, nil
		},
	})

	client := obsDial(t, setup.socketPath)
	obsRequest(t, client, "agent.orchestrator.register",
		obsClaudeRegisterParams(obsClaudeSessionID))

	// The pi terminal grants the claude registration nothing: no lease, and
	// no registerPiSessionRef call — so pendingPiSessionRefs never gets the
	// claude ref stashed for the pi terminal and the agent row's reported
	// session is never overwritten into a claude one.
	if called != 0 {
		t.Fatalf("registerPiSessionRef calls = %d, want 0 for a claude registration on a pi terminal", called)
	}
	agent, err := harness.agents.FindByPane(db.FindAgentByPaneInput{
		HerdrSessionName: obsSessionName,
		PaneID:           obsClaudePane,
	})
	if err != nil {
		t.Fatalf("findByPane: %v", err)
	}
	if agent == nil {
		t.Fatal("indexed pi pane missing")
	}
	if agent.AgentSession == nil ||
		agent.AgentSession.Agent != "pi" ||
		agent.AgentSession.Kind != observability.SessionRefPath ||
		agent.AgentSession.Value != obsPiSessionFile {
		t.Fatalf("agentSession = %+v, want the seeded pi ref untouched", agent.AgentSession)
	}

	// No socket-less lease either: closing the short-lived register-claude
	// socket disconnects the terminal exactly like the non-uuid case.
	client.close()
	obsSocketTick()
	if setup.server.IsTerminalConnected(daemon.ConnectedTerminalInput{
		HerdrSessionName: obsSessionName,
		TerminalID:       obsClaudeTerm,
	}) {
		t.Fatal("the claude registration kept terminal presence on a pi terminal")
	}
}

// openObsServerWithOptions mirrors openObsServer but lets the case override
// server options (only the fields this file sets; the rest keep their defaults).
func openObsServerWithOptions(
	t *testing.T,
	harness *obsHarness,
	options daemon.ObservabilityRpcServerOptions,
) obsServerSetup {
	t.Helper()
	socketPath := options.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join(t.TempDir(), "rpc.sock")
	}
	history := newObsHistory(harness, "")
	orchestrator := harness.orchestrator()
	options.Context = obsContextService(harness, history)
	options.History = history
	options.Orchestrator = orchestrator
	options.SocketPath = socketPath
	options.Stores = daemon.ObservabilityAgentStores{
		AgentEvents:     harness.agentEvents,
		Agents:          harness.agents,
		HerdrSessions:   harness.herdrSessions,
		HerdrWorkspaces: harness.herdrWorkspaces,
	}
	if options.Warn == nil {
		options.Warn = quietObsWarn
	}
	server := daemon.NewObservabilityRpcServer(options)
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
