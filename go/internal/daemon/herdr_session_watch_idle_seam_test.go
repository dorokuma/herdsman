package daemon_test

// The port of test/integration/herdr-session-watch-idle-seam.test.ts.
//
// The idle seam is the window where a pane exists in the index but its status
// was never subscribed: the first subscription carries no pane id, so Herdr
// emits only topology; the pane.created event makes the refresh see the pane, and
// the manager must then re-subscribe with that pane id before the status
// transition can be delivered. Both tests drive that seam against a fake Herdr
// socket server and a real index service on a migrated database, and assert the
// two subscription shapes plus what did or did not reach the daemon.
//
// The harness mirrors openObservabilityDbHarness without the seeded agent: the
// manager indexes the pane itself. Every test uses t.TempDir() for the socket and
// the database, so the production home and the live daemon are never touched.

import (
	"database/sql"
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/agenthistory"
	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/observability"
	"github.com/dorokuma/herdsman/go/internal/shared"
)

const (
	seamPaneID      = "wB:p9"
	seamWorkspaceID = "wB"
	seamTerminalID  = "term_1"
	seamOwnerID     = "term_owner"
	seamSessionName = "default"
)

// seamScope is the orchestrator scope the delivered idle event must belong to.
var seamScope = observability.AgentScope{
	HerdrSessionName: seamSessionName,
	WorkspaceID:      seamWorkspaceID,
}

// The addressable forms the *string query inputs need.
var (
	seamSessionNameAddress = seamSessionName
	seamWorkspaceIDAddress = seamWorkspaceID
)

// ---------------------------------------------------------------------------
// Database harness
// ---------------------------------------------------------------------------

type seamHarness struct {
	sqlite           *sql.DB
	agentEvents      *db.AgentEventStore
	agents           *db.AgentStore
	scopes           *db.AgentOrchestratorScopeStore
	statusEventPlans *db.StatusEventPlanStore
	sessions         *db.HerdrSessionStore
}

// openSeamHarness mirrors openObservabilityDbHarness: a migrated database in a
// temp dir with every store the index service and the watch manager need. The
// running session row is written by the manager itself.
func openSeamHarness(t *testing.T) *seamHarness {
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
	return &seamHarness{
		sqlite:           sqlite,
		agentEvents:      agentEvents,
		agents:           db.NewAgentStore(sqlite, agentEvents),
		scopes:           db.NewAgentOrchestratorScopeStore(sqlite),
		statusEventPlans: db.NewStatusEventPlanStore(sqlite),
		sessions:         db.NewHerdrSessionStore(sqlite),
	}
}

// indexService mirrors `new AgentIndexService({ history, stores: harness })`.
func (h *seamHarness) indexService(t *testing.T) *observability.AgentIndexService {
	t.Helper()
	service, err := observability.NewAgentIndexService(observability.AgentIndexServiceOptions{
		History: seamHistory{},
		Stores: observability.AgentIndexServiceStores{
			AgentContextSnapshots:   db.NewAgentContextSnapshotStore(h.sqlite),
			AgentEvents:             h.agentEvents,
			AgentOrchestratorScopes: h.scopes,
			AgentHistoryCache:       db.NewAgentHistoryCacheStore(h.sqlite),
			Agents:                  h.agents,
			HerdrSessions:           h.sessions,
			HerdrWorkspaces:         db.NewHerdrWorkspaceStore(h.sqlite),
			Sqlite:                  h.sqlite,
			StatusEventPlans:        h.statusEventPlans,
		},
	})
	if err != nil {
		t.Fatalf("newAgentIndexService: %v", err)
	}
	return service
}

func (h *seamHarness) paneAgent(t *testing.T) *observability.AgentIndexRecord {
	t.Helper()
	agent, err := h.agents.FindByPane(db.FindAgentByPaneInput{
		HerdrSessionName: seamSessionName,
		PaneID:           seamPaneID,
	})
	if err != nil {
		t.Fatalf("findByPane: %v", err)
	}
	return agent
}

func (h *seamHarness) unfinishedPlans(t *testing.T) []db.StatusEventPlanRecord {
	t.Helper()
	plans, err := h.statusEventPlans.ListUnfinished()
	if err != nil {
		t.Fatalf("listUnfinished: %v", err)
	}
	return plans
}

func (h *seamHarness) storedEvents(t *testing.T) []observability.AgentEventRecord {
	t.Helper()
	events, err := h.agentEvents.ListAfter(db.ListAfterInput{
		HerdrSessionName: &seamSessionNameAddress,
		WorkspaceID:      &seamWorkspaceIDAddress,
	})
	if err != nil {
		t.Fatalf("listAfter: %v", err)
	}
	return events
}

// ---------------------------------------------------------------------------
// Agent-history fake
// ---------------------------------------------------------------------------

// seamHistory mirrors the TS history fake: one fixed compact history whose last
// assistant message is non-empty, so a working → idle plan is not parked in a
// waiting-history retry, with the module-level helpers forwarded to the real
// agent-history module the way the production adapter forwards them.
type seamHistory struct{}

func (seamHistory) ResolveCompactHistory(
	_ observability.AgentHistoryLookupInput,
	_ observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	source := "pi-jsonl"
	return observability.AgentHistoryResolved{
		CompactHistory: observability.CompactAgentHistory{
			Source: &source,
			LastAssistantMessage: &observability.AgentHistoryExcerpt{
				Ref:  "history",
				Text: "turn complete",
			},
		},
	}
}

func (seamHistory) StatSourceFingerprint(
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

func (seamHistory) HistorySourceFromSessionRef(
	ref observability.AgentSessionRef,
) observability.AgentHistoryRefSource {
	return observability.AgentHistoryRefSource(
		agenthistory.HistorySourceFromSessionRef(ref))
}

func (seamHistory) SafeOfficialSessionPath(value string) string {
	return agenthistory.SafeOfficialSessionPath(value)
}

func (seamHistory) SafeAllowedSessionPath(value string) string {
	return agenthistory.SafeAllowedSessionPath(value, nil)
}

func (seamHistory) SessionPathAllowedByShape(value string) bool {
	return agenthistory.SessionPathAllowedByShape(value, nil)
}

// ---------------------------------------------------------------------------
// Fake Herdr socket server
// ---------------------------------------------------------------------------

// seamRequest is one decoded JSON-line request from the socket client.
type seamRequest struct {
	ID     any    `json:"id"`
	Method string `json:"method"`
	Params struct {
		Subscriptions []struct {
			PaneID string `json:"pane_id"`
			Type   string `json:"type"`
		} `json:"subscriptions"`
	} `json:"params"`
}

// seamStream is one client connection. Writes are serialized because a
// notification written while the server loop is still reading the same
// connection must not interleave with another frame.
type seamStream struct {
	mu   sync.Mutex
	conn net.Conn
}

func (s *seamStream) write(frame map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line, err := shared.EncodeJSONLine(frame)
	if err != nil {
		return
	}
	_, _ = s.conn.Write([]byte(line))
}

// seamServer is the fake Herdr daemon: a JSON-line server on a unix socket in a
// temp dir. It answers session.snapshot with the pane only once it is visible,
// and records the pane ids of every events.subscribe the way the TS fake does.
type seamServer struct {
	socketPath string

	mu          sync.Mutex
	subscribed  [][]string
	paneVisible bool
}

func (s *seamServer) isPaneVisible() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paneVisible
}

func (s *seamServer) showPane() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paneVisible = true
}

func (s *seamServer) recordSubscribed(paneIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribed = append(s.subscribed, paneIDs)
}

// subscribedLists is a copy of every events.subscribe pane-id list, in order.
func (s *seamServer) subscribedLists() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([][]string, 0, len(s.subscribed))
	for _, paneIDs := range s.subscribed {
		result = append(result, append([]string{}, paneIDs...))
	}
	return result
}

// openSeamServer mirrors openFakeHerdrServer: the socket lives in t.TempDir(), so
// cleanup removes both the socket and the directory.
func openSeamServer(
	t *testing.T,
	handle func(*seamServer, *seamStream, *seamRequest),
) *seamServer {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen %s: %v", socketPath, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := &seamServer{socketPath: socketPath}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serveSeamConnection(server, conn, handle)
		}
	}()
	return server
}

func serveSeamConnection(
	server *seamServer,
	conn net.Conn,
	handle func(*seamServer, *seamStream, *seamRequest),
) {
	defer func() { _ = conn.Close() }()
	stream := &seamStream{conn: conn}
	decoder, err := shared.NewJSONLineDecoder(shared.DefaultMaxFrameBytes)
	if err != nil {
		return
	}
	buffer := make([]byte, 32*1024)
	for {
		read, readErr := conn.Read(buffer)
		if read > 0 {
			messages, decodeErr := decoder.Push(string(buffer[:read]))
			if decodeErr != nil {
				return
			}
			for _, message := range messages {
				request, ok := asSeamRequest(message)
				if !ok {
					continue
				}
				handle(server, stream, &request)
			}
		}
		if readErr != nil {
			return
		}
	}
}

// asSeamRequest decodes one JSON-line message into the request shape.
func asSeamRequest(message any) (seamRequest, bool) {
	raw, err := json.Marshal(message)
	if err != nil {
		return seamRequest{}, false
	}
	var request seamRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return seamRequest{}, false
	}
	return request, true
}

// seamSubscribedPaneIds mirrors subscribedPaneIds: the pane ids among the
// subscriptions, i.e. the pane.agent_status_changed entries only.
func seamSubscribedPaneIds(request *seamRequest) []string {
	paneIDs := []string{}
	for _, subscription := range request.Params.Subscriptions {
		if subscription.Type != "pane.agent_status_changed" {
			continue
		}
		if subscription.PaneID == "" {
			continue
		}
		paneIDs = append(paneIDs, subscription.PaneID)
	}
	return paneIDs
}

// seamResponse is one answered request.
func seamResponse(id any, result any) map[string]any {
	return map[string]any{"id": id, "result": result}
}

// seamNotification is one server-initiated event, the shape herdr pushes and the
// socket client turns into `{...data, type}`.
func seamNotification(event string, data map[string]any) map[string]any {
	return map[string]any{"data": data, "event": event}
}

// seamEmptySnapshot mirrors emptySnapshot(): the workspace exists, no pane does.
func seamEmptySnapshot() map[string]any {
	return map[string]any{"snapshot": map[string]any{
		"agents":     []any{},
		"panes":      []any{},
		"tabs":       []any{},
		"workspaces": []any{seamWorkspace()},
	}}
}

// seamPaneSnapshot mirrors paneSnapshot(status): the one-pane view of the
// workspace, with the pane's agent in the requested status.
func seamPaneSnapshot(status string) map[string]any {
	return map[string]any{"snapshot": map[string]any{
		"agents": []any{map[string]any{
			"agent":        "claude",
			"agent_status": status,
			"cwd":          "/repo",
			"pane_id":      seamPaneID,
			"revision":     10,
			"terminal_id":  seamTerminalID,
			"workspace_id": seamWorkspaceID,
		}},
		"panes": []any{map[string]any{
			"pane_id":      seamPaneID,
			"revision":     10,
			"workspace_id": seamWorkspaceID,
		}},
		"tabs":       []any{},
		"workspaces": []any{seamWorkspace()},
	}}
}

func seamWorkspace() map[string]any {
	return map[string]any{"focused": true, "label": "Repo", "workspace_id": seamWorkspaceID}
}

// ---------------------------------------------------------------------------
// Received events
// ---------------------------------------------------------------------------

// seamEvents collects what the manager hands to onAgentEvent. The manager calls
// it from its watch goroutines, so it is guarded.
type seamEvents struct {
	mu     sync.Mutex
	events []observability.AgentEventRecord
}

func (e *seamEvents) add(event observability.AgentEventRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

func (e *seamEvents) ofType(eventType observability.AgentEventType) []observability.AgentEventRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	matching := []observability.AgentEventRecord{}
	for _, event := range e.events {
		if event.Type == eventType {
			matching = append(matching, event)
		}
	}
	return matching
}

// waitForSeam mirrors waitFor: a 3s deadline polled every 10ms.
func waitForSeam(t *testing.T, description string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if predicate() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition was not met: %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// startSeam
// ---------------------------------------------------------------------------

type seamInput struct {
	replayIdleOnSecondStream bool
	statusAfterCreate        string
}

type seamFixture struct {
	harness *seamHarness
	manager *daemon.HerdrSessionWatchManager
	server  *seamServer
	events  *seamEvents
}

// startSeam mirrors startSeam: the fake Herdr socket, the index service on a
// migrated database and a watch manager whose session list is this one running
// session, started and registered for teardown the way afterEach stops it.
func startSeam(t *testing.T, input seamInput) *seamFixture {
	t.Helper()
	harness := openSeamHarness(t)
	server := openSeamServer(t, func(
		srv *seamServer,
		stream *seamStream,
		request *seamRequest,
	) {
		if request.Method == "session.snapshot" {
			if srv.isPaneVisible() {
				stream.write(seamResponse(request.ID, seamPaneSnapshot(input.statusAfterCreate)))
				return
			}
			stream.write(seamResponse(request.ID, seamEmptySnapshot()))
			return
		}
		if request.Method != "events.subscribe" {
			stream.write(seamResponse(request.ID, map[string]any{}))
			return
		}

		paneIDs := seamSubscribedPaneIds(request)
		srv.recordSubscribed(paneIDs)
		stream.write(seamResponse(request.ID, map[string]any{"subscribed": true}))

		// Stream 1 is topology-only (paneIds=[]). Status is pane-specific: herdr
		// will not emit pane.agent_status_changed until that pane is subscribed.
		if len(paneIDs) == 0 {
			srv.showPane()
			stream.write(seamNotification("pane.created", map[string]any{
				"pane_id":      seamPaneID,
				"workspace_id": seamWorkspaceID,
			}))
			return
		}
		for _, paneID := range paneIDs {
			if input.replayIdleOnSecondStream && paneID == seamPaneID {
				stream.write(seamNotification("pane.agent_status_changed", map[string]any{
					"agent_status": "idle",
					"pane_id":      seamPaneID,
					"workspace_id": seamWorkspaceID,
				}))
			}
		}
	})

	events := &seamEvents{}
	manager := daemon.NewHerdrSessionWatchManager(daemon.HerdrSessionWatchOptions{
		ActiveRevisionPollMs: 60_000,
		Agents:               harness.agents,
		FullRescanMs:         60_000,
		HerdrSessions:        harness.sessions,
		Index:                harness.indexService(t),
		OnAgentEvent:         events.add,
		HasReconnectDelayMs:  true,
		SessionList: func() ([]herdr.HerdrSessionListEntry, error) {
			return []herdr.HerdrSessionListEntry{{
				Name:       seamSessionName,
				Running:    true,
				SessionDir: "/tmp/herdr",
				SocketPath: server.socketPath,
			}}, nil
		},
	})
	t.Cleanup(func() {
		if err := manager.Stop(); err != nil {
			t.Errorf("stop after test: %v", err)
		}
	})
	if err := manager.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	return &seamFixture{harness: harness, manager: manager, server: server, events: events}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestHerdrSessionWatchIdleSeamDeliversIdleAfterWorkingPrior ports the first TS
// case: the pane is created working, so the working status is persisted by the
// refresh; only the subscription that carries the pane id can then deliver the
// idle transition, and that transition must reach the daemon as agent.idle.
func TestHerdrSessionWatchIdleSeamDeliversIdleAfterWorkingPrior(t *testing.T) {
	fixture := startSeam(t, seamInput{
		replayIdleOnSecondStream: true,
		statusAfterCreate:        "working",
	})
	waitForSeam(t, "agent.idle delivered through the idle seam", func() bool {
		return len(fixture.events.ofType(observability.AgentEventIdle)) > 0
	})
	if err := fixture.manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	subscribed := fixture.server.subscribedLists()
	if len(subscribed) < 2 {
		t.Fatalf("subscriptions = %v, want at least two", subscribed)
	}
	if len(subscribed[0]) != 0 {
		t.Errorf("first subscription pane ids = %v, want []", subscribed[0])
	}
	if !reflect.DeepEqual(subscribed[1], []string{seamPaneID}) {
		t.Errorf("second subscription pane ids = %v, want [%s]", subscribed[1], seamPaneID)
	}

	idleEvents := fixture.events.ofType(observability.AgentEventIdle)
	if len(idleEvents) == 0 {
		t.Fatal("Expected an agent.idle event")
	}
	idle := idleEvents[0]
	if idle.PaneID == nil || *idle.PaneID != seamPaneID {
		t.Errorf("idle pane id = %v, want %s", idle.PaneID, seamPaneID)
	}
	if idle.WorkspaceID == nil || *idle.WorkspaceID != seamWorkspaceID {
		t.Errorf("idle workspace id = %v, want %s", idle.WorkspaceID, seamWorkspaceID)
	}
	payload, ok := idle.Payload.(map[string]any)
	if !ok {
		t.Fatalf("idle payload = %#v, want an object", idle.Payload)
	}
	if payload["from"] != "working" || payload["to"] != "idle" {
		t.Errorf("idle payload from/to = %v/%v, want working/idle", payload["from"], payload["to"])
	}

	agent := fixture.harness.paneAgent(t)
	if agent == nil {
		t.Fatal("Expected the pane to be indexed")
	}
	if agent.AgentStatus != observability.AgentStatusIdle {
		t.Errorf("agent status = %s, want idle", agent.AgentStatus)
	}
	if !db.IsDeliverableAgentEvent(idle, agent, seamScope, seamOwnerID) {
		t.Error("Expected the idle event to be deliverable to the owner terminal")
	}
	if plans := fixture.harness.unfinishedPlans(t); len(plans) != 0 {
		t.Errorf("unfinished plans = %d, want none", len(plans))
	}
}

// TestHerdrSessionWatchIdleSeamDoesNotInventIdle ports the second TS case: the
// first refresh of a created pane already sees it idle, so the working → idle
// transition never happened and no agent.idle may be invented — neither
// delivered to the daemon nor written to the event store.
func TestHerdrSessionWatchIdleSeamDoesNotInventIdle(t *testing.T) {
	fixture := startSeam(t, seamInput{
		replayIdleOnSecondStream: false,
		statusAfterCreate:        "idle",
	})
	waitForSeam(t, "the second subscription carrying the pane id", func() bool {
		return len(fixture.server.subscribedLists()) >= 2
	})
	// The TS waits 80ms so a late invention would land inside the assertions.
	time.Sleep(80 * time.Millisecond)
	if err := fixture.manager.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	subscribed := fixture.server.subscribedLists()
	if len(subscribed) < 2 {
		t.Fatalf("subscriptions = %v, want at least two", subscribed)
	}
	if len(subscribed[0]) != 0 {
		t.Errorf("first subscription pane ids = %v, want []", subscribed[0])
	}
	if !reflect.DeepEqual(subscribed[1], []string{seamPaneID}) {
		t.Errorf("second subscription pane ids = %v, want [%s]", subscribed[1], seamPaneID)
	}
	if idle := fixture.events.ofType(observability.AgentEventIdle); len(idle) != 0 {
		t.Errorf("delivered agent.idle events = %d, want none", len(idle))
	}

	agent := fixture.harness.paneAgent(t)
	if agent == nil {
		t.Fatal("Expected the pane to be indexed")
	}
	if agent.AgentStatus != observability.AgentStatusIdle {
		t.Errorf("agent status = %s, want idle", agent.AgentStatus)
	}
	if plans := fixture.harness.unfinishedPlans(t); len(plans) != 0 {
		t.Errorf("unfinished plans = %d, want none", len(plans))
	}
	stored := []observability.AgentEventType{}
	for _, event := range fixture.harness.storedEvents(t) {
		if event.Type == observability.AgentEventIdle {
			stored = append(stored, event.Type)
		}
	}
	if len(stored) != 0 {
		t.Errorf("stored agent.idle events = %d, want none", len(stored))
	}
}
