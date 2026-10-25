package daemon_test

// The port of test/unit/observability-server-frame-limit.test.ts.
//
// The frame limit is the only defence the observability socket has against a
// peer that never finishes a JSON line: the shared decoder refuses a buffer past
// 1 MiB, and the server must turn that error into a closed connection instead of
// growing forever.
//
// This file also holds the scaffolding the three observability-server test ports
// share: the migrated in-memory-shaped database harness (the Go form of
// test/integration/observability-db-harness.ts), the JSON-line RPC test client
// (test/integration/rpc-test-client.ts), and the agent-history adapter the
// production wiring layer provides. Every harness opens its database under
// t.TempDir(), so the production home and the live daemon are never touched.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/agenthistory"
	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
	"github.com/dorokuma/herdsman/go/internal/shared"
)

// ---------------------------------------------------------------------------
// Database harness (openObservabilityDbHarness)
// ---------------------------------------------------------------------------

// obsHarness mirrors the harness the TS integration tests open: one migrated
// database with every store the observability server and its services take.
type obsHarness struct {
	sqlite                 *sql.DB
	agentContextSnapshots  *db.AgentContextSnapshotStore
	agentEvents            *db.AgentEventStore
	agentHistoryCache      *db.AgentHistoryCacheStore
	agentOrchestratorScope *db.AgentOrchestratorScopeStore
	agents                 *db.AgentStore
	herdrSessions          *db.HerdrSessionStore
	herdrWorkspaces        *db.HerdrWorkspaceStore
	statusEventPlans       *db.StatusEventPlanStore
}

func openObsHarness(t *testing.T) *obsHarness {
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
	return &obsHarness{
		sqlite:                 sqlite,
		agentContextSnapshots:  db.NewAgentContextSnapshotStore(sqlite),
		agentEvents:            agentEvents,
		agentHistoryCache:      db.NewAgentHistoryCacheStore(sqlite),
		agentOrchestratorScope: db.NewAgentOrchestratorScopeStore(sqlite),
		agents:                 db.NewAgentStore(sqlite, agentEvents),
		herdrSessions:          db.NewHerdrSessionStore(sqlite),
		herdrWorkspaces:        db.NewHerdrWorkspaceStore(sqlite),
		statusEventPlans:       db.NewStatusEventPlanStore(sqlite),
	}
}

// orchestrator builds the TS `new AgentOrchestratorService({ ... })`.
func (h *obsHarness) orchestrator() *observability.AgentOrchestratorService {
	return observability.NewAgentOrchestratorService(struct {
		AgentEvents observability.AgentEventStoreLike
		Agents      observability.AgentStoreLike
		Scopes      observability.AgentOrchestratorScopeStoreLike
	}{
		AgentEvents: h.agentEvents,
		Agents:      h.agents,
		Scopes:      h.agentOrchestratorScope,
	})
}

// herdrSession is the running session every observability test registers: the
// socket path is what `agent.orchestrator.register` looks the session up by, and
// it deliberately does not exist so the pane-identity round trip fails over to
// the indexed presence, exactly like the TS fixtures.
const (
	obsHerdrSocketPath = "/tmp/herdr/herdr.sock"
	obsSessionName     = "default"
)

func (h *obsHarness) upsertRunningSession(t *testing.T) {
	t.Helper()
	if _, err := h.herdrSessions.UpsertRunning(observability.UpsertRunningHerdrSessionInput{
		Name:       obsSessionName,
		SessionDir: "/tmp/herdr",
		SocketPath: obsHerdrSocketPath,
	}); err != nil {
		t.Fatalf("upsertRunning: %v", err)
	}
}

// snapshot is the TS `snapshot(terminalId, paneId, workspaceId)` agent entry.
func obsSnapshot(terminalID, paneID, workspaceID string) db.HerdrAgentLike {
	return db.HerdrAgentLike{
		"agent":        "pi",
		"agent_status": "working",
		"pane_id":      paneID,
		"terminal_id":  terminalID,
		"workspace_id": workspaceID,
	}
}

// replaceAgents is the TS `replaceAgents(harness, agents)`.
func (h *obsHarness) replaceAgents(
	t *testing.T,
	agents []db.HerdrAgentLike,
) []observability.AgentIndexRecord {
	t.Helper()
	replaced, err := h.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents:           agents,
		HerdrSessionName: obsSessionName,
	})
	if err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	return replaced
}

// ---------------------------------------------------------------------------
// Agent history adapter
// ---------------------------------------------------------------------------

// obsHistory is the production adapter for one real agent-history service: the
// context service and the RPC server both speak the observability-side records,
// so every call is forwarded to *agenthistory.AgentHistoryService and its
// records are mapped back onto those types. The records of the two packages
// carry the same JSON tags by construction, so the mapping is a JSON round trip
// rather than a hand-written field copy that could drift.
type obsHistory struct {
	service *agenthistory.AgentHistoryService
}

func newObsHistory(harness *obsHarness, homeDir string) *obsHistory {
	options := agenthistory.AgentHistoryServiceOptions{Cache: harness.agentHistoryCache}
	if homeDir != "" {
		options.HomeDir = &homeDir
	}
	return &obsHistory{service: agenthistory.CreateAgentHistoryService(options)}
}

// ResolveCompactHistory mirrors resolveCompactHistory through the adapter.
func (h *obsHistory) ResolveCompactHistory(
	input observability.AgentHistoryLookupInput,
	options observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	resolved := h.service.ResolveCompactHistory(
		agentLookupInput(input),
		agenthistory.ResolveCompactOptions{
			ForceDiscovery: options.ForceDiscovery,
			ForceRefresh:   options.ForceRefresh != nil && *options.ForceRefresh,
			PreferredRef:   historyRefToAgent(options.PreferredRef),
		},
	)
	return observability.AgentHistoryResolved{
		CompactHistory:    jsonAs[observability.CompactAgentHistory](resolved.CompactHistory),
		HistoryRef:        historyRefFromAgent(resolved.HistoryRef),
		SourceFingerprint: fingerprintFromAgent(resolved.SourceFingerprint),
	}
}

// Read mirrors the service's `read` for the RPC server's agent.read method.
func (h *obsHistory) Read(
	input observability.AgentHistoryLookupInput,
	options daemon.ObservabilityHistoryReadOptions,
) daemon.ObservabilityHistoryReadResult {
	read := h.service.Read(agentLookupInput(input), agenthistory.ReadOptions{
		ForceDiscovery: options.ForceDiscovery,
		Limit:          options.Limit,
	})
	return daemon.ObservabilityHistoryReadResult{
		HistoryRef: historyRefFromAgent(read.HistoryRef),
		Messages:   jsonAs[[]observability.AgentHistoryMessage](read.Messages),
	}
}

// StatSourceFingerprint forwards to the real helper, as the wiring layer does.
func (h *obsHistory) StatSourceFingerprint(
	path string,
) *observability.AgentHistorySourceFingerprint {
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

func (h *obsHistory) HistorySourceFromSessionRef(
	ref observability.AgentSessionRef,
) observability.AgentHistoryRefSource {
	return observability.AgentHistoryRefSource(agenthistory.HistorySourceFromSessionRef(ref))
}

func (h *obsHistory) SafeOfficialSessionPath(value string) string {
	return agenthistory.SafeOfficialSessionPath(value)
}

func (h *obsHistory) SafeAllowedSessionPath(value string) string {
	return agenthistory.SafeAllowedSessionPath(value, nil)
}

func (h *obsHistory) SessionPathAllowedByShape(value string) bool {
	return agenthistory.SessionPathAllowedByShape(value, nil)
}

// agentLookupInput maps the observability lookup input onto the agent-history
// one. TS passes the same object to both, so an empty string stays absent here.
func agentLookupInput(input observability.AgentHistoryLookupInput) agenthistory.AgentHistoryLookupInput {
	mapped := agenthistory.AgentHistoryLookupInput{Agent: input.Agent, AgentSession: input.AgentSession}
	if input.HerdrSessionName != "" {
		mapped.HerdrSessionName = &input.HerdrSessionName
	}
	if input.PaneID != "" {
		mapped.PaneID = &input.PaneID
	}
	return mapped
}

func historyRefToAgent(ref *observability.AgentHistoryRef) *agenthistory.HistoryRef {
	if ref == nil {
		return nil
	}
	return jsonAs[*agenthistory.HistoryRef](ref)
}

func historyRefFromAgent(ref *agenthistory.HistoryRef) *observability.AgentHistoryRef {
	if ref == nil {
		return nil
	}
	return jsonAs[*observability.AgentHistoryRef](ref)
}

func fingerprintFromAgent(
	fingerprint *agenthistory.SourceFingerprint,
) *observability.AgentHistorySourceFingerprint {
	if fingerprint == nil {
		return nil
	}
	return &observability.AgentHistorySourceFingerprint{
		MtimeMs: fingerprint.MtimeMs,
		Path:    fingerprint.Path,
		Size:    float64(fingerprint.Size),
	}
}

// jsonAs re-decodes a value into an isomorphic record of another package.
func jsonAs[Target any](value any) Target {
	var target Target
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("observability test adapter: marshal: %v", err))
	}
	if err := json.Unmarshal(encoded, &target); err != nil {
		panic(fmt.Sprintf("observability test adapter: unmarshal: %v", err))
	}
	return target
}

// ---------------------------------------------------------------------------
// RPC test client (rpc-test-client.ts)
// ---------------------------------------------------------------------------

// obsRPCTimeout is the client-side budget of the TS waitForNotification loop:
// 1000 attempts at 2 ms each.
const obsRPCTimeout = 2 * time.Second

type obsOutcome struct {
	value any
	err   error
}

// obsRPCClient mirrors RpcTestClient: JSON lines in, matching ids, and a
// notification buffer the assertions drain.
type obsRPCClient struct {
	conn          net.Conn
	decoder       *shared.JsonLineDecoder
	pending       map[string]chan obsOutcome
	notifications []map[string]any
	mu            sync.Mutex
	writeMu       sync.Mutex
	nextID        int
	socketClosed  bool
	closeSignal   chan struct{}
	closeOnce     sync.Once
}

func obsDial(t *testing.T, socketPath string) *obsRPCClient {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial %s: %v", socketPath, err)
	}
	decoder, err := shared.NewJSONLineDecoder(shared.DefaultMaxFrameBytes)
	if err != nil {
		t.Fatalf("newJsonLineDecoder: %v", err)
	}
	client := &obsRPCClient{
		conn:         conn,
		decoder:      decoder,
		pending:      map[string]chan obsOutcome{},
		nextID:       1,
		closeSignal:  make(chan struct{}),
		socketClosed: false,
	}
	t.Cleanup(client.close)
	go client.readLoop()
	return client
}

func (c *obsRPCClient) readLoop() {
	buffer := make([]byte, 32*1024)
	for {
		read, err := c.conn.Read(buffer)
		if read > 0 {
			c.handleData(string(buffer[:read]))
		}
		if err != nil {
			break
		}
	}
	// The peer closed the connection: from the client side that is exactly what
	// TS `socket.destroyed` reports after a server-side destroy.
	c.markClosed()
	c.rejectAll(errors.New("RPC test socket closed"))
}

func (c *obsRPCClient) markClosed() {
	c.mu.Lock()
	c.socketClosed = true
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.closeSignal) })
}

func (c *obsRPCClient) handleData(chunk string) {
	messages, err := c.decoder.Push(chunk)
	if err != nil {
		c.rejectAll(err)
		return
	}
	for _, message := range messages {
		object, ok := message.(map[string]any)
		if !ok {
			continue
		}
		rawID, hasID := object["id"]
		if !hasID {
			c.mu.Lock()
			c.notifications = append(c.notifications, object)
			c.mu.Unlock()
			continue
		}
		key := fmt.Sprintf("%v", rawID)
		if numeric, isNumber := rawID.(float64); isNumber {
			key = strconv.FormatFloat(numeric, 'f', -1, 64)
		}
		c.mu.Lock()
		channel, tracked := c.pending[key]
		if tracked {
			delete(c.pending, key)
		}
		c.mu.Unlock()
		if !tracked {
			continue
		}
		if failure, isFailure := object["error"].(map[string]any); isFailure {
			message := "RPC request failed"
			if text, isText := failure["message"].(string); isText {
				message = text
			}
			channel <- obsOutcome{err: errors.New(message)}
			continue
		}
		channel <- obsOutcome{value: object["result"]}
	}
}

func (c *obsRPCClient) rejectAll(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[string]chan obsOutcome{}
	c.mu.Unlock()
	for _, channel := range pending {
		select {
		case channel <- obsOutcome{err: err}:
		default:
		}
	}
}

func (c *obsRPCClient) request(method string, params any) (any, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	channel := make(chan obsOutcome, 1)
	c.pending[fmt.Sprintf("%d", id)] = channel
	c.mu.Unlock()

	frame, err := shared.EncodeJSONLine(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	c.writeMu.Lock()
	_, writeErr := c.conn.Write([]byte(frame))
	c.writeMu.Unlock()
	if writeErr != nil {
		return nil, writeErr
	}
	select {
	case outcome := <-channel:
		return outcome.value, outcome.err
	case <-time.After(obsRPCTimeout):
		return nil, fmt.Errorf("timed out waiting for the response to %s", method)
	}
}

// waitForNotification is the TS polling loop: the notification buffer is polled,
// and the first matching message is taken out of it.
func (c *obsRPCClient) waitForNotification(method string) (map[string]any, error) {
	deadline := time.Now().Add(obsRPCTimeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for index, message := range c.notifications {
			if message["method"] != method {
				continue
			}
			taken := c.notifications[index]
			c.notifications = append(c.notifications[:index], c.notifications[index+1:]...)
			c.mu.Unlock()
			return taken, nil
		}
		c.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	return nil, fmt.Errorf("Timed out waiting for %s", method)
}

// clearNotifications mirrors clearNotifications().
func (c *obsRPCClient) clearNotifications() {
	c.mu.Lock()
	c.notifications = nil
	c.mu.Unlock()
}

// notificationCount is the length the TS `client.notifications` assertions
// check.
func (c *obsRPCClient) notificationCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.notifications)
}

// socketDestroyed mirrors socketDestroyed().
func (c *obsRPCClient) socketDestroyed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.socketClosed
}

// waitForClose mirrors waitForClose().
func (c *obsRPCClient) waitForClose(t *testing.T) {
	t.Helper()
	if c.socketDestroyed() {
		return
	}
	select {
	case <-c.closeSignal:
	case <-time.After(obsRPCTimeout):
		t.Fatalf("timed out waiting for the client socket to close")
	}
}

// close mirrors close().
func (c *obsRPCClient) close() {
	if err := c.conn.Close(); err != nil && !isObsClosedError(err) {
		return
	}
	c.markClosed()
}

func isObsClosedError(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

// ---------------------------------------------------------------------------
// Shared assertions and small pointers
// ---------------------------------------------------------------------------

func obsString(value string) *string { return &value }
func obsInt64(value int64) *int64    { return &value }
func obsBool(value bool) *bool       { return &value }

// obsStatus addresses the optional delivery status of an event record.
func obsStatus(value observability.AgentEventStatus) *observability.AgentEventStatus {
	return &value
}

// obsPath digs into a decoded RPC object the way toMatchObject walks it, and
// reports the first missing or mistyped step.
func obsPath(t *testing.T, value any, steps ...string) any {
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

// obsWaitFor polls a predicate the way vi.waitFor does: 10 ms apart, 5 s budget.
func obsWaitFor(t *testing.T, description string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if predicate() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// obsSocketTick is the TS fixed settle window, used only for the
// "assert that nothing happened" shapes.
func obsSocketTick() { time.Sleep(20 * time.Millisecond) }

// TestObservabilityServerFrameLimit is the port of the single
// observability-server-frame-limit.test.ts case.
func TestObservabilityServerFrameLimit(t *testing.T) {
	harness := openObsHarness(t)
	history := newObsHistory(harness, t.TempDir())
	socketPath := filepath.Join(t.TempDir(), "rpc.sock")

	server := daemon.NewObservabilityRpcServer(daemon.ObservabilityRpcServerOptions{
		Context: observability.NewAgentContextService(struct {
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
		}),
		History:      history,
		Orchestrator: harness.orchestrator(),
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
	defer func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial %s: %v", socketPath, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(
		[]byte(strings.Repeat("1", shared.DefaultMaxFrameBytes+1)),
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	// TS waits for the socket's `close` event; reading until the peer's answer
	// ends is the same observation on a Go conn.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setReadDeadline: %v", err)
	}
	buffer := make([]byte, 1)
	if _, err := conn.Read(buffer); !errors.Is(err, io.EOF) {
		t.Fatalf("expected the oversized frame to close the connection, got %v", err)
	}
}

// quietObsWarn drops the warnings the TS run prints to stdout, so a green test
// stays readable. The default sink (stderr) is what the daemon itself uses.
func quietObsWarn(string, any) {}
