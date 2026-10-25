package daemon_test

// Dual-client interop: the TypeScript `ReconnectingDaemonClient` of
// packages/herdsman-pi and the Go ObservabilityRpcClient connect to the same Go
// ObservabilityRpcServer at the same time. There is no TS counterpart of this
// case — the TS suite only ever drove the TS server with the TS client — so the
// assertion set is the pi-facing surface PLAN.md keeps frozen (register, list,
// orchestrator.get/set, ping, turn.completed, the pushed agent.event and
// agent.context.changed, notifications.ack) plus the evidence that both sessions
// were live at the same moment: each client sees the other's presence, orchestrator
// claim and turn signal, and only the connection that owns the role is pushed.
//
// The TS side runs as a Node subprocess (tsx) driving the unmodified client
// source; see testdata/pi_client_driver.ts. The Go test releases one phase at a
// time over the driver's stdin, so the overlap is ordered rather than raced.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// obsDriverStep is one JSON line the TS driver writes: either the outcome of one
// RPC step or one pushed notification, both reported under the method name.
type obsDriverStep struct {
	Error  string         `json:"error"`
	Name   string         `json:"name"`
	OK     bool           `json:"ok"`
	Result map[string]any `json:"result"`
}

// obsRepoRoot walks up from the test's working directory to the checkout root,
// recognised by the pi package the driver imports.
func obsRepoRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		marker := filepath.Join(directory, "packages", "herdsman-pi", "src", "daemon-client.ts")
		if _, err := os.Stat(marker); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("packages/herdsman-pi/src/daemon-client.ts not found above %s", directory)
		}
		directory = parent
	}
}

// obsTsxRunner finds the tsx binary that can load the TS client. A worktree
// usually shares the checkout's install, so the primary worktree is searched
// too; without any tsx the case skips with the reason instead of passing silently.
func obsTsxRunner(t *testing.T) string {
	t.Helper()
	candidates := []string{}
	if fromEnv := os.Getenv("HERDSMAN_TSX"); fromEnv != "" {
		candidates = append(candidates, fromEnv)
	}
	root := obsRepoRoot(t)
	candidates = append(candidates, filepath.Join(root, "node_modules", ".bin", "tsx"))
	if main := obsMainWorktree(t); main != "" {
		candidates = append(candidates, filepath.Join(main, "node_modules", ".bin", "tsx"))
	}
	if fromPath, err := exec.LookPath("tsx"); err == nil {
		candidates = append(candidates, fromPath)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Skipf("tsx is not installed (looked for: %s); skipping the TS client interop case",
		strings.Join(candidates, ", "))
	return ""
}

// obsMainWorktree is the primary worktree of this checkout, which holds the
// dependency install when the current worktree has none.
func obsMainWorktree(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "worktree", "list", "--porcelain").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if path, found := strings.CutPrefix(line, "worktree "); found {
			return strings.TrimSpace(path)
		}
	}
	return ""
}

// obsSeedPiPanes is the two-pane variant of obsSeedAgent: one pane per client, so
// each connection resolves its own presence terminal.
func obsSeedPiPanes(t *testing.T, harness *obsHarness) []observability.AgentIndexRecord {
	t.Helper()
	harness.upsertRunningSession(t)
	harness.replaceAgents(t, []db.HerdrAgentLike{
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
	agents, err := harness.agents.List(observability.AgentQueryScope{WorkspaceID: obsString(obsWorkspaceB)})
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	return agents
}

// obsSeedContextSnapshots gives every agent of the workspace a compact history so
// publishAgentContext has something to deliver.
func obsSeedContextSnapshots(t *testing.T, harness *obsHarness, agents []observability.AgentIndexRecord) {
	t.Helper()
	for _, agent := range agents {
		terminal := ""
		if agent.TerminalID != nil {
			terminal = *agent.TerminalID
		}
		obsPutSnapshot(t, harness, db.AgentContextSnapshotInput{
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
}

// obsDriver is the running Node subprocess and its step stream.
type obsDriver struct {
	command chan<- string
	cmd     *exec.Cmd
	notes   []string
	notesMu sync.Mutex
	steps   chan obsDriverStep
}

func (d *obsDriver) note(line string) {
	d.notesMu.Lock()
	defer d.notesMu.Unlock()
	if len(d.notes) < 200 {
		d.notes = append(d.notes, line)
	}
}

func (d *obsDriver) snapshotNotes() []string {
	d.notesMu.Lock()
	defer d.notesMu.Unlock()
	return append([]string(nil), d.notes...)
}

// obsStartDriver launches the TS client against socketPath and starts pumping its
// stdout into the returned struct's step channel.
func obsStartDriver(t *testing.T, tsx string, socketPath string) *obsDriver {
	t.Helper()
	driverPath := filepath.Join(obsRepoRoot(t), "go", "internal", "daemon", "testdata", "pi_client_driver.ts")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, tsx, driverPath, socketPath, "wB:p1", "ts-driver")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("driver stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("driver stdout: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("driver stderr: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the TS driver with %s: %v", tsx, err)
	}
	// The pumps never touch *testing.T: cmd.Wait() closes the pipes while they are
	// still reading, and a goroutine logging after the test ended would fail it.
	// Everything the driver writes beside its step stream is kept on the struct and
	// replayed by the cleanup, which runs while the test still accepts output.
	driver := &obsDriver{cmd: cmd, steps: make(chan obsDriverStep, 64)}
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			driver.note(scanner.Text())
		}
	}()
	commands := make(chan string, 8)
	driver.command = commands
	go func() {
		for command := range commands {
			if _, err := io.WriteString(stdin, command+"\n"); err != nil {
				driver.note(fmt.Sprintf("writing %q failed: %v", command, err))
				return
			}
		}
	}()
	go func() {
		defer close(driver.steps)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var step obsDriverStep
			if err := json.Unmarshal([]byte(line), &step); err != nil {
				driver.note("non-JSON driver line: " + line)
				continue
			}
			driver.steps <- step
		}
		// A pipe closed by cmd.Wait() ends the stream; an unexpected exit still
		// surfaces, as the missing step obsWait is waiting for.
	}()
	t.Cleanup(func() {
		close(driver.command)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		for _, line := range driver.snapshotNotes() {
			t.Logf("ts driver: %s", line)
		}
	})
	return driver
}

// obsSend hands one command word to the driver.
func (d *obsDriver) obsSend(command string) { d.command <- command }

// obsWait blocks until the driver reports the named step and fails the test on a
// step that reported an RPC or protocol failure.
func (d *obsDriver) obsWait(t *testing.T, name string) obsDriverStep {
	t.Helper()
	for {
		select {
		case step, open := <-d.steps:
			if !open {
				t.Fatalf("the TS driver exited while waiting for its %q step", name)
			}
			if step.Name == name {
				if !step.OK {
					t.Fatalf("the TS driver step %q failed: %s", name, step.Error)
				}
				return step
			}
			// A pushed notification (agent.orchestrator.changed and friends) is
			// expected traffic; only a failed request aborts the case.
			if !step.OK && step.Name != "disconnected" {
				t.Fatalf("the TS driver step %q failed while waiting for %q: %s",
					step.Name, name, step.Error)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("waiting for the TS driver step %q timed out", name)
		}
	}
}

// obsNotifications records what the Go client is pushed, so the case can tell the
// owner's pushes from a bystander's silence.
type obsNotifications struct {
	mu      sync.Mutex
	methods []string
}

func (n *obsNotifications) record(method string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.methods = append(n.methods, method)
}

func (n *obsNotifications) count(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	total := 0
	for _, recorded := range n.methods {
		if recorded == method {
			total++
		}
	}
	return total
}

func (n *obsNotifications) clear() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.methods = nil
}

func TestObservabilityRpcServerSimultaneousTsAndGoClients(t *testing.T) {
	registry := observability.NewTurnCompletionRegistry(observability.WithTurnSettimeout(20_000))
	fixture := obsOpenRPCServerWithoutClient(t, obsRPCServerOptions{turnCompletions: registry})
	obsSeedContextSnapshots(t, fixture.harness, obsSeedPiPanes(t, fixture.harness))

	pushed := &obsNotifications{}
	goClient, err := daemon.NewObservabilityRpcClient(daemon.ObservabilityRpcClientOptions{
		OnNotification: func(method string, _ any) { pushed.record(method) },
		SocketPath:     fixture.socketPath,
	})
	if err != nil {
		t.Fatalf("dial %s: %v", fixture.socketPath, err)
	}
	t.Cleanup(goClient.Close)

	// Both turn signals are awaited before either client sends them, mirroring the
	// daemon side that blocks a status event on the signal of its own pane.
	type signalResult struct {
		result observability.TurnCompletionWaitResult
	}
	tsSignal := make(chan signalResult, 1)
	goSignal := make(chan signalResult, 1)
	wait := func(terminal string, sink chan signalResult) {
		go func() {
			sink <- signalResult{registry.WaitForSignal(observability.WaitForSignalInput{
				HerdrSessionName: obsSessionName,
				RecordedAfterMs:  0,
				TerminalID:       terminal,
			})}
		}()
	}
	wait("term_1", tsSignal)
	wait("term_2", goSignal)

	// The Go pi client takes the second pane, then the TS client joins on the first.
	goRegistered := obsCall(t, goClient, "agent.orchestrator.register", obsRegisterParams("wB:p2"))
	assertObsSubset(t, obsAt(t, goRegistered, "presence"), map[string]any{"terminalId": "term_2"})

	driver := obsStartDriver(t, obsTsxRunner(t), fixture.socketPath)
	tsRegistered := driver.obsWait(t, "agent.orchestrator.register")
	assertObsSubset(t, obsAt(t, tsRegistered.Result, "presence"), map[string]any{"terminalId": "term_1"})

	// Phase 1 — the four pi methods, issued while both sessions are connected.
	driver.obsSend("list")
	tsListed := driver.obsWait(t, "agent.list")
	goListed := obsCall(t, goClient, "agent.list", map[string]any{"workspaceId": obsWorkspaceB})
	// Each client lists the same workspace and sees the other client's presence.
	for name, result := range map[string]map[string]any{"ts": tsListed.Result, "go": goListed} {
		agents := obsList(t, name+" agent.list", result, "agents")
		if len(agents) != 2 {
			t.Fatalf("%s agent.list: expected the two pi panes, got %d: %v", name, len(agents), agents)
		}
	}
	assertObsExact(t, "agent.ping", obsCall(t, goClient, "agent.ping", map[string]any{}),
		map[string]any{"ok": true})
	assertObsExact(t, "ts agent.ping", driver.obsWait(t, "agent.ping").Result, map[string]any{"ok": true})

	assertObsExact(t, "agent.turn.completed", obsCall(t, goClient, "agent.turn.completed", map[string]any{
		"confirmed":        true,
		"herdrSessionName": obsSessionName,
		"paneId":           "wB:p2",
		"terminalId":       "term_2",
		"workspaceId":      obsWorkspaceB,
	}), map[string]any{"accepted": true})
	assertObsExact(t, "ts agent.turn.completed",
		driver.obsWait(t, "agent.turn.completed").Result, map[string]any{"accepted": true})

	// Both signals landed in the daemon-side registry, each under the terminal of
	// the connection that sent it.
	for _, waiter := range []struct {
		terminal string
		result   chan signalResult
	}{{"term_1", tsSignal}, {"term_2", goSignal}} {
		select {
		case got := <-waiter.result:
			if !got.result.Received || !got.result.Confirmed {
				t.Fatalf("%s: expected a confirmed turn signal, got %+v", waiter.terminal, got.result)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: no turn completion signal arrived", waiter.terminal)
		}
	}

	// Phase 2 — one orchestrator role, two clients: whatever the other connection
	// holds is what agent.orchestrator.get reports.
	scope := observability.AgentScope{
		HerdrSessionName: obsSessionName,
		WorkspaceID:      obsWorkspaceB,
	}
	obsCall(t, goClient, "agent.orchestrator.set", map[string]any{"enabled": true})
	// The claimant is the connection the server pushes, so the bystander check on
	// the TS side below compares against a push path that is known to work.
	obsSocketTick()
	pushed.clear()
	fixture.server.PublishAgentContext(scope)
	obsWaitFor(t, "the Go client to be pushed the workspace context", func() bool {
		return pushed.count("agent.context.changed") > 0
	})

	driver.obsSend("state")
	tsState := driver.obsWait(t, "agent.orchestrator.get")
	assertObsSubset(t, obsAt(t, tsState.Result, "state", "owner"), map[string]any{
		"paneId":     "wB:p2",
		"terminalId": "term_2",
	})

	obsCall(t, goClient, "agent.orchestrator.set", map[string]any{"enabled": false})
	driver.obsSend("claim")
	tsClaimed := driver.obsWait(t, "agent.orchestrator.set(enabled)")
	assertObsSubset(t, tsClaimed.Result, map[string]any{"changed": true})
	assertObsSubset(t, obsAt(t, tsClaimed.Result, "state", "owner"), map[string]any{
		"paneId":     "wB:p1",
		"terminalId": "term_1",
	})
	goState := obsCall(t, goClient, "agent.orchestrator.get", map[string]any{})
	assertObsSubset(t, obsAt(t, goState, "state", "owner"), map[string]any{
		"paneId":     "wB:p1",
		"terminalId": "term_1",
	})

	// Phase 3 — the pushes follow the role, and the owner acks over the TS client.
	obsSocketTick()
	pushed.clear()
	fixture.server.PublishAgentContext(scope)
	tsContext := driver.obsWait(t, "agent.context.changed")
	assertObsSubset(t, tsContext.Result, map[string]any{"workspaceId": obsWorkspaceB})
	obsSocketTick()
	if got := pushed.count("agent.context.changed"); got != 0 {
		t.Fatalf("the non-owner Go client was pushed %d agent.context.changed, want 0", got)
	}

	routed := obsAppendRoutedEvent(t, fixture.harness, "term_2", obsWorkspaceB)
	fixture.server.PublishAgentEvent(routed)
	tsEvent := driver.obsWait(t, "agent.event")
	assertObsSubset(t, obsAt(t, tsEvent.Result, "event"), map[string]any{"id": routed.ID})
	obsSocketTick()
	if got := pushed.count("agent.event"); got != 0 {
		t.Fatalf("the non-owner Go client was pushed %d agent.event, want 0", got)
	}

	driver.obsSend(fmt.Sprintf("ack %d", routed.ID))
	acked := driver.obsWait(t, "agent.notifications.ack")
	assertObsSubset(t, acked.Result, map[string]any{"acknowledged": true})
	assertObsSubset(t, obsAt(t, acked.Result, "state"), map[string]any{"ackedEventId": routed.ID})

	// Phase 4 — the TS session ends and the Go client keeps working on the socket.
	driver.obsSend("quit")
	driver.obsWait(t, "done")
	assertObsExact(t, "agent.ping after the TS client closed",
		obsCall(t, goClient, "agent.ping", map[string]any{}), map[string]any{"ok": true})
	if err := driver.cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 0 {
			t.Fatalf("the TS driver exited with %v", err)
		}
	}
}
