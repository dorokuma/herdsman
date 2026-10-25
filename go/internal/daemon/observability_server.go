package daemon

// This file ports src/daemon/observability-server.ts: the observability JSON-RPC
// server every Pi / Agy connector talks to over the daemon unix socket.
//
// Wire contract. Every request frame and every response/notification frame is a
// single JSON line, produced by the same shared encoder the TS daemon uses. The
// RPC method names, the parameter validation, and the JSON shape of each result
// and of the pushed notifications (`agent.event`,
// `agent.orchestrator.changed`, `agent.context.changed`) are reproduced field by
// field, so a TS `herdsman-pi` and a Go `herdsman-pi` can both drive a Go daemon
// and a TS daemon. `observability_server_frame_limit_test.go` covers the frame
// limit, and the two translated integration tests cover the orchestrator
// handshake end to end over a real socket.
//
// Concurrency. The TS daemon runs one event loop: each synchronous block between
// two `await`s is atomic, and no two handlers interleave inside such a block. The
// Go port reproduces that boundary with `mu`: every synchronous block of the TS
// original is one critical section here, and the seams TS awaits on
// (`resolvePaneIdentity`, `registerPiSessionRef`, the history reads) are taken
// with the lock released, exactly where the `await` sits. Store calls are
// synchronous in TS, so they stay inside the critical section; they never call
// back into the server.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/observability"
	"github.com/dorokuma/herdsman/go/internal/shared"
)

// DisconnectGraceMs mirrors DISCONNECT_GRACE_MS: how long a terminal may stay
// away before the owners it holds are released.
const DisconnectGraceMs = 5_000

// StartupReconnectGraceMs mirrors STARTUP_RECONNECT_GRACE_MS: how long a
// persisted owner may stay away after a daemon restart.
const StartupReconnectGraceMs = 10_000

// TerminalHeartbeatTimeoutMs mirrors TERMINAL_HEARTBEAT_TIMEOUT_MS.
const TerminalHeartbeatTimeoutMs = 90_000

// TerminalHeartbeatScanIntervalMs mirrors TERMINAL_HEARTBEAT_SCAN_INTERVAL_MS.
const TerminalHeartbeatScanIntervalMs = 30_000

// connectorPaneMismatchMessage is the ConnectorPaneMismatchError message; the
// TS class carries no other payload, so the message is the contract.
const connectorPaneMismatchMessage = "Connector process does not match Herdr pane"

// ErrConnectorPaneMismatch mirrors ConnectorPaneMismatchError.
var ErrConnectorPaneMismatch = errors.New(connectorPaneMismatchMessage)

// ObservabilityHistoryService is the slice of the agent-history module the
// observability RPC server depends on: the two entry points the `agent.get` and
// `agent.read` methods call. It speaks the observability-side records (the same
// shape with the same JSON tags), so the production adapter forwards to
// *agenthistory.AgentHistoryService and maps the agenthistory records onto them,
// exactly like the adapter internal/observability documents for
// AgentHistoryServiceLike.
type ObservabilityHistoryService interface {
	ResolveCompactHistory(
		input observability.AgentHistoryLookupInput,
		options observability.AgentHistoryResolveOptions,
	) observability.AgentHistoryResolved
	Read(
		input observability.AgentHistoryLookupInput,
		options ObservabilityHistoryReadOptions,
	) ObservabilityHistoryReadResult
}

// ObservabilityHistoryReadOptions mirrors the `read` options the RPC server
// passes: `{ forceDiscovery: true, limit }`.
type ObservabilityHistoryReadOptions struct {
	ForceDiscovery bool
	Limit          int
}

// ObservabilityHistoryReadResult mirrors the `{ historyRef, messages }` pair the
// TS `read` resolves to.
type ObservabilityHistoryReadResult struct {
	HistoryRef *observability.AgentHistoryRef
	Messages   []observability.AgentHistoryMessage
}

// PiPresence mirrors the TS `PiPresence` type (`AgentScope & { ... }`). The field
// order is the order the TS object literal writes its keys in, so the marshaled
// presence object keeps the TS key order. `subscriberKind` is optional in TS, so
// it is a pointer with omitempty.
type PiPresence struct {
	ConnectedAt      int64                         `json:"connectedAt"`
	HerdrSessionName string                        `json:"herdrSessionName"`
	PaneID           string                        `json:"paneId"`
	SubscriberID     string                        `json:"subscriberId"`
	SubscriberKind   *observability.SubscriberKind `json:"subscriberKind,omitempty"`
	TerminalID       string                        `json:"terminalId"`
	WorkspaceID      string                        `json:"workspaceId"`
}

// Scope is the presence's orchestrator scope.
func (p PiPresence) Scope() observability.AgentScope {
	return observability.AgentScope{HerdrSessionName: p.HerdrSessionName, WorkspaceID: p.WorkspaceID}
}

// OrchestratorConnectionState mirrors AgentOrchestratorConnectionStateResult:
// the result of `agent.orchestrator.register` / `agent.orchestrator.get`.
type OrchestratorConnectionState struct {
	Context  *observability.AgentWorkspaceContextSnapshot `json:"context"`
	Events   []observability.AgentEventRecord             `json:"events"`
	Presence PiPresence                                   `json:"presence"`
	State    *observability.AgentOrchestratorWireState    `json:"state"`
}

// ObservabilityAgentStores mirrors the TS `AgentStores` record. The TS class is
// handed the concrete stores; `herdrWorkspaces` is stored but unread there, and
// stays that way here.
type ObservabilityAgentStores struct {
	AgentEvents     *db.AgentEventStore
	Agents          *db.AgentStore
	HerdrSessions   *db.HerdrSessionStore
	HerdrWorkspaces *db.HerdrWorkspaceStore
}

// ResolvePaneIdentityInput is the resolver seam input, named so the wiring layer
// can pass herdr.ResolveHerdrPaneIdentity through a method value.
type ResolvePaneIdentityInput = herdr.ResolveHerdrPaneIdentityInput

// ObservabilityRpcServerOptions mirrors the TS constructor options.
//
// The optional durations use 0 for "absent": every TS default is non-zero, so a
// caller can never express a default by writing 0. `Warn` replaces the TS
// `console.warn` calls; the default writes to stderr. `ConnectedTerminal` exists
// in the TS options and is never read by the TS class either (the daemon wires
// `connectedTerminal` through the reconciler instead), so it is accepted and
// unused here as well.
type ObservabilityRpcServerOptions struct {
	ClearInterval func(handle any)
	ClearTimeout  func(handle any)
	// ConnectSocket reports whether an existing socket path is live.
	ConnectSocket func(socketPath string) (bool, error)
	Context       *observability.AgentContextService
	// DisconnectGraceMs is the same-terminal reconnect window.
	DisconnectGraceMs       int
	HeartbeatScanIntervalMs int
	HeartbeatTimeoutMs      int
	History                 ObservabilityHistoryService
	Now                     func() int64
	Orchestrator            *observability.AgentOrchestratorService
	// PeerPidOf reads the connecting process pid for pane binding.
	PeerPidOf func(conn net.Conn) (int, bool)
	// RegisterPiSessionRef is the index service's Pi session registration.
	RegisterPiSessionRef func(
		input observability.RegisterPiSessionRefInput,
	) (*observability.PiSessionRefRegistrationResult, error)
	ResolvePaneIdentity     func(input ResolvePaneIdentityInput) (herdr.HerdrPaneIdentity, error)
	SetInterval             func(callback func(), delayMs int) any
	SetTimeout              func(callback func(), delayMs int) any
	SocketPath              string
	StartupReconnectGraceMs int
	ConnectedTerminal       func(input ConnectedTerminalInput) bool
	Stores                  ObservabilityAgentStores
	TurnCompletions         *observability.TurnCompletionRegistry
	Warn                    func(message string, payload any)
}

// graceTimer mirrors the TS GraceTimer wrapper.
type graceTimer struct {
	handle any
}

// agyTerminalLease mirrors the TS AgyTerminalLease.
type agyTerminalLease struct {
	PaneGeneration *string
	Presence       PiPresence
}

// ObservabilityRpcServer ports the TS class of the same name.
type ObservabilityRpcServer struct {
	// mu guards every field below it, mirroring the atomicity of one TS
	// synchronous block. See the file comment.
	mu sync.Mutex

	clearInterval func(handle any)
	clearTimeout  func(handle any)
	setInterval   func(callback func(), delayMs int) any
	connectSocket func(socketPath string) (bool, error)
	context       *observability.AgentContextService
	// connectionOrderBySocket mirrors #connectionOrderBySocket: the accept order
	// used to pick the newest socket of a terminal.
	connectionOrderBySocket map[*rpcConnection]int
	// publishedContextFingerprintByScope mirrors
	// #publishedContextFingerprintByScope.
	publishedContextFingerprintByScope map[string]string
	disconnectGraceMs                  int
	disconnectTimers                   map[string]graceTimer
	history                            ObservabilityHistoryService
	heartbeatScanIntervalMs            int
	heartbeatTimeoutMs                 int
	lastActivityByTerminal             map[string]int64
	heartbeatTimer                     any
	now                                func() int64
	orchestrator                       *observability.AgentOrchestratorService
	peerPidOf                          func(conn net.Conn) (int, bool)
	agyPresenceByTerminal              map[string]agyTerminalLease
	piPresenceBySocket                 map[*rpcConnection]PiPresence
	socketsByTerminal                  map[string]map[*rpcConnection]struct{}
	registerPiSessionRef               func(
		input observability.RegisterPiSessionRefInput,
	) (*observability.PiSessionRefRegistrationResult, error)
	resolvePaneIdentity func(input ResolvePaneIdentityInput) (herdr.HerdrPaneIdentity, error)
	setTimeout          func(callback func(), delayMs int) any
	socketPath          string
	sockets             map[*rpcConnection]struct{}
	startupReconnectMs  int
	startupTimers       map[string]graceTimer
	stores              ObservabilityAgentStores
	turnCompletions     *observability.TurnCompletionRegistry
	warn                func(message string, payload any)

	listener           net.Listener
	connectionSequence int
	stopping           bool
	// umaskMu guards the process-wide umask dance start() performs, mirroring
	// TS `process.umask(0o077)`.
	umaskMu sync.Mutex
}

// NewObservabilityRpcServer mirrors the TS constructor, including the default
// for every injected dependency.
func NewObservabilityRpcServer(options ObservabilityRpcServerOptions) *ObservabilityRpcServer {
	warn := options.Warn
	if warn == nil {
		warn = defaultObservabilityWarn
	}
	setTimeout := options.SetTimeout
	if setTimeout == nil {
		setTimeout = defaultSetTimeout
	}
	clearTimeout := options.ClearTimeout
	if clearTimeout == nil {
		clearTimeout = defaultClearTimeout
	}
	setInterval := options.SetInterval
	if setInterval == nil {
		setInterval = defaultSetInterval
	}
	clearInterval := options.ClearInterval
	if clearInterval == nil {
		clearInterval = defaultClearInterval
	}
	connectSocket := options.ConnectSocket
	if connectSocket == nil {
		connectSocket = DefaultConnectSocket
	}
	now := options.Now
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	peerPidOf := options.PeerPidOf
	if peerPidOf == nil {
		peerPidOf = defaultPeerPidOf
	}
	registerPiSessionRef := options.RegisterPiSessionRef
	if registerPiSessionRef == nil {
		// TS default: `async () => ({ contextChangedScopes: [] })`.
		registerPiSessionRef = func(observability.RegisterPiSessionRefInput,
		) (*observability.PiSessionRefRegistrationResult, error) {
			return &observability.PiSessionRefRegistrationResult{}, nil
		}
	}
	resolvePaneIdentity := options.ResolvePaneIdentity
	if resolvePaneIdentity == nil {
		resolvePaneIdentity = func(input ResolvePaneIdentityInput) (herdr.HerdrPaneIdentity, error) {
			return herdr.ResolveHerdrPaneIdentity(input)
		}
	}
	turnCompletions := options.TurnCompletions
	if turnCompletions == nil {
		turnCompletions = observability.NewTurnCompletionRegistry()
	}

	disconnectGraceMs := options.DisconnectGraceMs
	if disconnectGraceMs == 0 {
		disconnectGraceMs = DisconnectGraceMs
	}
	heartbeatScanIntervalMs := options.HeartbeatScanIntervalMs
	if heartbeatScanIntervalMs == 0 {
		heartbeatScanIntervalMs = TerminalHeartbeatScanIntervalMs
	}
	heartbeatTimeoutMs := options.HeartbeatTimeoutMs
	if heartbeatTimeoutMs == 0 {
		heartbeatTimeoutMs = TerminalHeartbeatTimeoutMs
	}
	startupReconnectMs := options.StartupReconnectGraceMs
	if startupReconnectMs == 0 {
		startupReconnectMs = StartupReconnectGraceMs
	}

	return &ObservabilityRpcServer{
		clearInterval:                      clearInterval,
		clearTimeout:                       clearTimeout,
		connectSocket:                      connectSocket,
		context:                            options.Context,
		connectionOrderBySocket:            map[*rpcConnection]int{},
		publishedContextFingerprintByScope: map[string]string{},
		disconnectGraceMs:                  disconnectGraceMs,
		disconnectTimers:                   map[string]graceTimer{},
		history:                            options.History,
		heartbeatScanIntervalMs:            heartbeatScanIntervalMs,
		heartbeatTimeoutMs:                 heartbeatTimeoutMs,
		lastActivityByTerminal:             map[string]int64{},
		now:                                now,
		orchestrator:                       options.Orchestrator,
		peerPidOf:                          peerPidOf,
		agyPresenceByTerminal:              map[string]agyTerminalLease{},
		piPresenceBySocket:                 map[*rpcConnection]PiPresence{},
		socketsByTerminal:                  map[string]map[*rpcConnection]struct{}{},
		registerPiSessionRef:               registerPiSessionRef,
		resolvePaneIdentity:                resolvePaneIdentity,
		setInterval:                        setInterval,
		setTimeout:                         setTimeout,
		socketPath:                         options.SocketPath,
		sockets:                            map[*rpcConnection]struct{}{},
		startupReconnectMs:                 startupReconnectMs,
		startupTimers:                      map[string]graceTimer{},
		stores:                             options.Stores,
		turnCompletions:                    turnCompletions,
		warn:                               warn,
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Start mirrors start(): it refuses a reachable socket, unlinks a stale one,
// listens with 0600 permissions, arms the heartbeat scan and the startup grace.
func (s *ObservabilityRpcServer) Start() error {
	s.mu.Lock()
	s.stopping = false
	s.mu.Unlock()

	if fileExists(s.socketPath) {
		reachable, err := s.connectSocket(s.socketPath)
		if err != nil {
			return err
		}
		if reachable {
			return fmt.Errorf("Herdsman daemon socket is already reachable: %s", s.socketPath)
		}
		if err := os.Remove(s.socketPath); err != nil {
			return err
		}
	}

	// TS: `process.umask(0o077)` before listen, so the socket is never created
	// world-readable. Node leaves the umask in place for the rest of the process
	// lifetime and so does this port.
	s.umaskMu.Lock()
	syscall.Umask(0o077)
	s.umaskMu.Unlock()

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		_ = listener.Close()
		return err
	}

	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	go s.acceptLoop(listener)

	s.mu.Lock()
	s.heartbeatTimer = s.setInterval(func() { s.scanHeartbeats() }, s.heartbeatScanIntervalMs)
	s.mu.Unlock()

	s.armStartupGrace()
	return nil
}

// Stop mirrors stop(): it stops scheduling, drops every registry entry, closes
// every connection and the listener, and unlinks the socket.
func (s *ObservabilityRpcServer) Stop() error {
	s.mu.Lock()
	s.stopping = true
	if s.heartbeatTimer != nil {
		s.clearInterval(s.heartbeatTimer)
		s.heartbeatTimer = nil
	}
	s.clearGraceTimers(s.disconnectTimers)
	s.clearGraceTimers(s.startupTimers)
	for connection := range s.sockets {
		connection.destroy()
	}
	s.sockets = map[*rpcConnection]struct{}{}
	s.agyPresenceByTerminal = map[string]agyTerminalLease{}
	s.piPresenceBySocket = map[*rpcConnection]PiPresence{}
	s.socketsByTerminal = map[string]map[*rpcConnection]struct{}{}
	s.connectionOrderBySocket = map[*rpcConnection]int{}
	listener := s.listener
	s.listener = nil
	s.mu.Unlock()

	wasListening := listener != nil
	if listener != nil {
		if err := listener.Close(); err != nil {
			return err
		}
	}
	if wasListening && fileExists(s.socketPath) {
		if err := os.Remove(s.socketPath); err != nil {
			return err
		}
	}
	return nil
}

// acceptLoop serves one connection per goroutine until the listener is closed.
// TS attaches no `error` handler to the server after listen succeeded, so an
// accept failure ends the loop the same way an explicit close does.
func (s *ObservabilityRpcServer) acceptLoop(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go s.handleConnection(conn)
	}
}

// ---------------------------------------------------------------------------
// Pushed notifications (public entry points)
// ---------------------------------------------------------------------------

// PublishAgentContext mirrors publishAgentContext: the owner's newest connection
// gets the workspace context, unless the very same fingerprint was already sent.
//
// The frames are written with the lock released: a peer that stopped reading
// blocks net.Conn.Write, and the TS original awaited its socket write outside
// the critical section for the same reason.
func (s *ObservabilityRpcServer) PublishAgentContext(scope observability.AgentScope) {
	s.mu.Lock()
	connection, message, ok := s.publishAgentContextLocked(scope)
	s.mu.Unlock()
	if ok {
		connection.writeMessage(message)
	}
}

// PublishAgentEvent mirrors publishAgentEvent: a deliverable worker event is
// routed to the workspace orchestrator's newest connection.
//
// The routing runs under the lock and the frames are written after it is
// released: net.Conn.Write blocks when a peer stops reading, and holding the
// server lock across it would stall every other RPC and the shutdown sequence.
func (s *ObservabilityRpcServer) PublishAgentEvent(event observability.AgentEventRecord) {
	s.mu.Lock()
	frames := s.publishAgentEventLocked(event)
	s.mu.Unlock()
	writeAll(frames)
}

// publishAgentEventLocked is publishAgentEvent's routing half, with the lock
// held: it resolves the target connections and builds their frames.
func (s *ObservabilityRpcServer) publishAgentEventLocked(
	event observability.AgentEventRecord,
) []pendingFrame {
	// `agentId` is intentionally not required: a terminal event written after the
	// agents row was physically removed still has to reach the orchestrator. The
	// workspace/terminal scope is required, because without it the event cannot be
	// routed at all.
	if stringAbsent(event.WorkspaceID) || stringAbsent(event.TerminalID) {
		// Dead letter: accepted as permanently undeliverable, but never silently.
		if event.Type == observability.AgentEventFailed ||
			event.Type == observability.AgentEventDiscarded {
			s.warn("Herdsman terminal agent event has no deliverable scope (dead letter)", map[string]any{
				"eventId":          event.ID,
				"herdrSessionName": event.HerdrSessionName,
				"terminalId":       derefString(event.TerminalID),
				"type":             string(event.Type),
				"workspaceId":      derefString(event.WorkspaceID),
			})
		}
		return nil
	}
	// Status changes are persisted for history but never routed as worker wake events.
	if event.Type == observability.AgentEventStatusChanged {
		return nil
	}
	status := ""
	if event.Status != nil {
		status = string(*event.Status)
	}
	if (status != string(observability.AgentEventStatusPending) &&
		status != string(observability.AgentEventStatusDelivered)) ||
		event.Deliverable != 1 {
		return nil
	}
	scope := observability.AgentScope{
		HerdrSessionName: event.HerdrSessionName,
		WorkspaceID:      *event.WorkspaceID,
	}
	state, err := s.orchestrator.Status(scope)
	if err != nil {
		s.warn("Herdsman could not read the orchestrator owner for an agent event", err)
		return nil
	}
	var owner *observability.AgentOrchestratorOwner
	if state != nil {
		owner = state.Owner
	}
	frame := map[string]any{
		"method": "agent.event",
		"params": map[string]any{"event": event},
	}
	if owner == nil {
		// An ownerless workspace (cross-workspace dispatched worker) has no route of
		// its own: hand its event to the session's single active orchestrator.
		sessionOwner, ownerErr := s.singleActiveSessionOwner(
			event.HerdrSessionName,
			*event.WorkspaceID,
		)
		if ownerErr != nil {
			s.warn("Herdsman could not resolve the session orchestrator for an agent event", ownerErr)
			return nil
		}
		if sessionOwner == nil {
			return nil
		}
		connection := s.newestSocketForSessionTerminal(event.HerdrSessionName, *sessionOwner)
		if connection == nil {
			return nil
		}
		return []pendingFrame{{target: connection, message: frame}}
	}
	if stringPresent(event.TerminalID) && *event.TerminalID == owner.TerminalID {
		return nil
	}
	connection := s.newestSocketForTerminal(scope, owner.TerminalID)
	if connection == nil {
		return nil
	}
	return []pendingFrame{{target: connection, message: frame}}
}

// singleActiveSessionOwner mirrors #singleActiveSessionOwner: the single active
// orchestrator of a Herdr session, if there is exactly one. With several owners
// the ownerless workspace's event stays pull-only (the owner still receives it
// through `pending()`), so no arbitrary owner is woken.
func (s *ObservabilityRpcServer) singleActiveSessionOwner(
	herdrSessionName string,
	eventWorkspaceID string,
) (*string, error) {
	states, err := s.orchestrator.PersistedOwners()
	if err != nil {
		return nil, err
	}
	terminalIDs := []string{}
	for _, state := range states {
		if state.HerdrSessionName != herdrSessionName ||
			state.WorkspaceID == eventWorkspaceID ||
			state.Owner == nil {
			continue
		}
		present, err := s.hasTerminalPresence(state.HerdrSessionName, state.Owner.TerminalID)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		if !containsString(terminalIDs, state.Owner.TerminalID) {
			terminalIDs = append(terminalIDs, state.Owner.TerminalID)
		}
	}
	if len(terminalIDs) != 1 {
		return nil, nil
	}
	return &terminalIDs[0], nil
}

// AgentLocationReconcile mirrors the reconcileAgentLocations input object
// `{ agents, herdrSessionName }`; the watch manager's onAgentIndexRefreshed
// callback hands it the agents a Herdr scan just produced.
type AgentLocationReconcile struct {
	Agents           []observability.AgentIndexRecord
	HerdrSessionName string
}

// ReconcileAgentLocations mirrors reconcileAgentLocations: the agent index a
// Herdr scan just produced relocates live presences, refreshes Agy leases and
// moves orchestrator ownership along with the pane.
func (s *ObservabilityRpcServer) ReconcileAgentLocations(
	input AgentLocationReconcile,
) error {
	// The frames collected by this pass are written once the lock is released:
	// a peer that stopped reading must not be able to block every other RPC
	// through the lock this function holds.
	var frames []pendingFrame
	var runErr error
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		frames, runErr = s.reconcileAgentLocationsLocked(input)
	}()
	writeAll(frames)
	return runErr
}

// reconcileAgentLocationsLocked is the reconcile pass with the lock held. It
// returns the frames to write after the lock is released.
func (s *ObservabilityRpcServer) reconcileAgentLocationsLocked(
	input AgentLocationReconcile,
) ([]pendingFrame, error) {
	var frames []pendingFrame
	byTerminal := map[string]observability.AgentIndexRecord{}
	for _, agent := range input.Agents {
		if stringPresent(agent.TerminalID) {
			byTerminal[*agent.TerminalID] = agent
		}
	}
	owners, err := s.orchestrator.PersistedOwners()
	if err != nil {
		return nil, err
	}
	owners = filterStatesForSession(owners, input.HerdrSessionName)

	for connection, presence := range s.piPresenceBySocket {
		if presence.HerdrSessionName != input.HerdrSessionName {
			continue
		}
		agent, ok := byTerminal[presence.TerminalID]
		// Missing terminal owners are retained until an explicit disconnect/release;
		// this refresh only relocates known agents.
		if !ok {
			continue
		}
		presence.PaneID = agent.PaneID
		presence.WorkspaceID = agent.WorkspaceID
		s.piPresenceBySocket[connection] = presence
	}

	for key, lease := range s.agyPresenceByTerminal {
		presence := lease.Presence
		if presence.HerdrSessionName != input.HerdrSessionName {
			continue
		}
		agent, indexed := byTerminal[presence.TerminalID]
		if !indexed {
			closed, err := s.stores.Agents.IsPaneClosedSince(db.IsPaneClosedInput{
				HerdrSessionName: presence.HerdrSessionName,
				PaneID:           presence.PaneID,
				PaneGeneration:   lease.PaneGeneration,
				SinceMs:          presence.ConnectedAt,
			})
			if err != nil {
				return frames, err
			}
			if closed {
				delete(s.agyPresenceByTerminal, key)
				if len(s.socketsByTerminal[key]) == 0 {
					released, err := s.releaseCurrentOwnersForTerminal(
						input.HerdrSessionName,
						observability.OrchestratorChangeDisconnected,
						presence.TerminalID,
					)
					frames = append(frames, released...)
					if err != nil {
						return frames, err
					}
				}
			}
			continue
		}
		if agent.Agent != nil && *agent.Agent != "agy" {
			delete(s.agyPresenceByTerminal, key)
			if len(s.socketsByTerminal[key]) == 0 {
				released, err := s.releaseCurrentOwnersForTerminal(
					input.HerdrSessionName,
					observability.OrchestratorChangeDisconnected,
					presence.TerminalID,
				)
				frames = append(frames, released...)
				if err != nil {
					return frames, err
				}
			}
			continue
		}
		paneGeneration := lease.PaneGeneration
		if agent.PaneGeneration != nil {
			paneGeneration = agent.PaneGeneration
		}
		presence.PaneID = agent.PaneID
		presence.WorkspaceID = agent.WorkspaceID
		s.agyPresenceByTerminal[key] = agyTerminalLease{
			PaneGeneration: paneGeneration,
			Presence:       presence,
		}
	}

	for _, ownerState := range owners {
		owner := ownerState.Owner
		if owner == nil {
			continue
		}
		current, err := s.orchestrator.Status(ownerState.AgentScope)
		if err != nil {
			return frames, err
		}
		if current == nil || current.Owner == nil || current.Owner.TerminalID != owner.TerminalID {
			continue
		}
		agent, indexed := byTerminal[owner.TerminalID]
		// Missing terminal owners are retained until an explicit disconnect/release;
		// this refresh only relocates known agents.
		if !indexed {
			continue
		}
		if agent.WorkspaceID == ownerState.WorkspaceID {
			if agent.PaneID == owner.PaneID {
				continue
			}
			change, err := s.orchestrator.Claim(observability.OrchestratorClaimInput{
				AgentScope: ownerState.AgentScope,
				PaneID:     agent.PaneID,
				TerminalID: owner.TerminalID,
			})
			if err != nil {
				return frames, err
			}
			// TS: `toWireChange({ ...change, reason: "moved" })`.
			moved := observability.AgentOrchestratorChanged{
				Current:  toWireState(change.Current),
				Previous: toWireState(change.Previous),
				Reason:   observability.OrchestratorChangeMoved,
			}
			frames = append(frames, s.publishOrchestratorChange(moved)...)
			continue
		}
		changes, err := s.orchestrator.Move(observability.OrchestratorMoveInput{
			From:       ownerState.AgentScope,
			PaneID:     agent.PaneID,
			TerminalID: owner.TerminalID,
			To: observability.AgentScope{
				HerdrSessionName: input.HerdrSessionName,
				WorkspaceID:      agent.WorkspaceID,
			},
		})
		if err != nil {
			return frames, err
		}
		for _, change := range changes {
			frames = append(frames, s.publishOrchestratorChange(toWireChange(change))...)
		}
	}
	return frames, nil
}

// IsTerminalConnected mirrors isTerminalConnected: the authoritative live Pi
// connection registry, intentionally empty right after daemon startup.
//
// A store failure cannot be reported to the caller (the TS surface returns a
// boolean through the reconciler's `connectedTerminal`), so it is warned about
// and read as "not connected".
func (s *ObservabilityRpcServer) IsTerminalConnected(input ConnectedTerminalInput) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	connected, err := s.hasTerminalPresence(input.HerdrSessionName, input.TerminalID)
	if err != nil {
		s.warn("Herdsman could not read terminal presence", err)
		return false
	}
	return connected
}

// ---------------------------------------------------------------------------
// Connection handling
// ---------------------------------------------------------------------------

// handleConnection mirrors the createServer callback plus the data/close/error
// socket handlers.
func (s *ObservabilityRpcServer) handleConnection(conn net.Conn) {
	connection := &rpcConnection{conn: conn}

	s.mu.Lock()
	s.sockets[connection] = struct{}{}
	s.connectionSequence++
	s.connectionOrderBySocket[connection] = s.connectionSequence
	s.mu.Unlock()

	decoder, err := shared.NewJSONLineDecoder(shared.DefaultMaxFrameBytes)
	if err != nil {
		connection.destroy()
		s.handleSocketClose(connection)
		return
	}

	buffer := make([]byte, 32*1024)
	for {
		read, readErr := conn.Read(buffer)
		if read > 0 {
			messages, pushErr := decoder.Push(string(buffer[:read]))
			if pushErr != nil {
				var tooLarge *shared.JsonLineFrameTooLargeError
				if errors.As(pushErr, &tooLarge) {
					s.warn(fmt.Sprintf("Closing observability connection: %s", pushErr.Error()), nil)
				}
				connection.destroy()
				break
			}
			for _, message := range messages {
				s.markTerminalActivity(connection)
				s.handleRequest(connection, message)
			}
		}
		if readErr != nil {
			break
		}
	}
	// TS ignores socket `error` events; the close handler is the one that matters.
	connection.destroy()
	s.handleSocketClose(connection)
}

// handleSocketClose mirrors #handleSocketClose.
func (s *ObservabilityRpcServer) handleSocketClose(connection *rpcConnection) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sockets[connection]; !ok {
		return
	}
	delete(s.sockets, connection)
	delete(s.connectionOrderBySocket, connection)
	presence, hadPresence := s.piPresenceBySocket[connection]
	delete(s.piPresenceBySocket, connection)
	if hadPresence {
		s.unregisterTerminalSocket(connection, presence)
	}
	// A socket that closes is no longer a trusted connected terminal. The
	// grace timer remains only to tolerate a same-terminal reconnect; active
	// non-pi terminals that leased ownership are preserved by hasTerminalPresence.
	if !s.stopping && hadPresence {
		if err := s.scheduleDisconnect(presence); err != nil {
			s.warn("Herdsman could not schedule a terminal disconnect", err)
		}
	}
}

// markTerminalActivity mirrors #markTerminalActivity.
func (s *ObservabilityRpcServer) markTerminalActivity(connection *rpcConnection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	presence, ok := s.piPresenceBySocket[connection]
	if ok {
		s.lastActivityByTerminal[terminalPresenceKey(presence.HerdrSessionName, presence.TerminalID)] = s.now()
	}
}

// scanHeartbeats mirrors #scanHeartbeats.
func (s *ObservabilityRpcServer) scanHeartbeats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return
	}
	cutoff := s.now() - int64(s.heartbeatTimeoutMs)
	for key, lastActivity := range s.lastActivityByTerminal {
		if lastActivity >= cutoff {
			continue
		}
		for connection := range s.socketsByTerminal[key] {
			connection.destroy()
		}
	}
}

// registerTerminalSocket mirrors #registerTerminalSocket.
func (s *ObservabilityRpcServer) registerTerminalSocket(
	connection *rpcConnection,
	presence PiPresence,
) {
	key := terminalPresenceKey(presence.HerdrSessionName, presence.TerminalID)
	sockets, ok := s.socketsByTerminal[key]
	if !ok {
		sockets = map[*rpcConnection]struct{}{}
		s.socketsByTerminal[key] = sockets
	}
	sockets[connection] = struct{}{}
}

// unregisterTerminalSocket mirrors #unregisterTerminalSocket.
func (s *ObservabilityRpcServer) unregisterTerminalSocket(
	connection *rpcConnection,
	presence PiPresence,
) {
	key := terminalPresenceKey(presence.HerdrSessionName, presence.TerminalID)
	sockets, ok := s.socketsByTerminal[key]
	if !ok {
		return
	}
	delete(sockets, connection)
	if len(sockets) == 0 {
		delete(s.socketsByTerminal, key)
		delete(s.lastActivityByTerminal, key)
	}
}

// newestSocketForTerminal mirrors #newestSocketForTerminal.
func (s *ObservabilityRpcServer) newestSocketForTerminal(
	scope observability.AgentScope,
	terminalID string,
) *rpcConnection {
	var newest *rpcConnection
	newestOrder := 0
	for connection, presence := range s.piPresenceBySocket {
		if presence.HerdrSessionName == scope.HerdrSessionName &&
			presence.WorkspaceID == scope.WorkspaceID &&
			presence.TerminalID == terminalID &&
			!connection.destroyed.Load() {
			order := s.connectionOrderBySocket[connection]
			if newest == nil || order > newestOrder {
				newest, newestOrder = connection, order
			}
		}
	}
	return newest
}

// newestSocketForSessionTerminal mirrors #newestSocketForSessionTerminal.
func (s *ObservabilityRpcServer) newestSocketForSessionTerminal(
	herdrSessionName string,
	terminalID string,
) *rpcConnection {
	var newest *rpcConnection
	newestOrder := 0
	for connection, presence := range s.piPresenceBySocket {
		if presence.HerdrSessionName != herdrSessionName ||
			presence.TerminalID != terminalID ||
			connection.destroyed.Load() {
			continue
		}
		order := s.connectionOrderBySocket[connection]
		if newest == nil || order > newestOrder {
			newest, newestOrder = connection, order
		}
	}
	return newest
}

// ---------------------------------------------------------------------------
// Presence, grace and heartbeat bookkeeping
// ---------------------------------------------------------------------------

// scheduleDisconnect mirrors #scheduleDisconnect.
func (s *ObservabilityRpcServer) scheduleDisconnect(presence PiPresence) error {
	key := terminalPresenceKey(presence.HerdrSessionName, presence.TerminalID)
	present, err := s.hasTerminalPresence(presence.HerdrSessionName, presence.TerminalID)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if _, pending := s.disconnectTimers[key]; pending {
		return nil
	}
	var handle any
	handle = s.setTimeout(func() {
		// The frames this release produces are written once the lock is
		// released, so a wedged peer cannot stall the daemon through it.
		var released []pendingFrame
		func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.disconnectTimers, key)
			if s.stopping {
				return
			}
			stillPresent, err := s.hasTerminalPresence(presence.HerdrSessionName, presence.TerminalID)
			if err != nil {
				s.warn("Herdsman could not read terminal presence while releasing an owner", err)
				return
			}
			if stillPresent {
				return
			}
			owned, err := s.releaseCurrentOwnersForTerminal(
				presence.HerdrSessionName,
				observability.OrchestratorChangeDisconnected,
				presence.TerminalID,
			)
			if err != nil {
				s.warn("Herdsman could not release the owners of a disconnected terminal", err)
				return
			}
			released = owned
		}()
		writeAll(released)
	}, s.disconnectGraceMs)
	s.disconnectTimers[key] = graceTimer{handle: handle}
	return nil
}

// armStartupGrace mirrors #armStartupGrace: every persisted owner gets a
// reconnect window, and an Agy-owned terminal is leased up front so a scan
// cannot mistake it for a vanished owner.
func (s *ObservabilityRpcServer) armStartupGrace() {
	s.mu.Lock()
	defer s.mu.Unlock()

	states, err := s.orchestrator.PersistedOwners()
	if err != nil {
		s.warn("Herdsman could not read persisted orchestrator owners", err)
		return
	}
	for _, state := range states {
		owner := state.Owner
		if owner == nil {
			continue
		}
		key := terminalPresenceKey(state.HerdrSessionName, owner.TerminalID)
		indexed, err := s.stores.Agents.FindByTerminal(db.FindAgentByTerminalInput{
			HerdrSessionName: state.HerdrSessionName,
			TerminalID:       owner.TerminalID,
		})
		if err != nil {
			s.warn("Herdsman could not index the terminal of a persisted owner", err)
			continue
		}
		if indexed != nil && indexed.Agent != nil && *indexed.Agent == "agy" {
			s.agyPresenceByTerminal[key] = agyTerminalLease{
				PaneGeneration: indexed.PaneGeneration,
				Presence: PiPresence{
					ConnectedAt:      s.now(),
					HerdrSessionName: state.HerdrSessionName,
					PaneID:           indexed.PaneID,
					SubscriberID:     "agy:" + owner.TerminalID,
					SubscriberKind:   kindPointer(observability.SubscriberAgy),
					TerminalID:       owner.TerminalID,
					WorkspaceID:      state.WorkspaceID,
				},
			}
		}
		captured := state
		var handle any
		handle = s.setTimeout(func() {
			// Same shape as the disconnect timer: collect under the lock, write
			// after it is released.
			var released []pendingFrame
			func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				delete(s.startupTimers, key)
				ownerTerminalID := ""
				if captured.Owner != nil {
					ownerTerminalID = captured.Owner.TerminalID
				}
				if s.stopping {
					return
				}
				present, err := s.hasTerminalPresence(captured.HerdrSessionName, ownerTerminalID)
				if err != nil {
					s.warn("Herdsman could not read terminal presence in the startup grace", err)
					return
				}
				if present {
					return
				}
				owned, err := s.releaseCurrentOwnersForTerminal(
					captured.HerdrSessionName,
					observability.OrchestratorChangeStartupTimeout,
					ownerTerminalID,
				)
				if err != nil {
					s.warn("Herdsman could not release the owners of a stale startup owner", err)
					return
				}
				released = owned
			}()
			writeAll(released)
		}, s.startupReconnectMs)
		s.startupTimers[key] = graceTimer{handle: handle}
	}
}

// releaseCurrentOwnersForTerminal mirrors #releaseCurrentOwnersForTerminal. It
// returns the frames to write once the caller has released the lock.
func (s *ObservabilityRpcServer) releaseCurrentOwnersForTerminal(
	herdrSessionName string,
	reason observability.AgentOrchestratorChangeReason,
	terminalID string,
) ([]pendingFrame, error) {
	states, err := s.orchestrator.PersistedOwners()
	if err != nil {
		return nil, err
	}
	var frames []pendingFrame
	for _, state := range states {
		if state.HerdrSessionName != herdrSessionName {
			continue
		}
		if state.Owner == nil || state.Owner.TerminalID != terminalID {
			continue
		}
		change, err := s.orchestrator.Release(observability.OrchestratorReleaseInput{
			AgentScope: state.AgentScope,
			Reason:     reason,
			TerminalID: terminalID,
		})
		if err != nil {
			return frames, err
		}
		if change != nil {
			frames = append(frames, s.publishOrchestratorChange(toWireChange(*change))...)
		}
	}
	return frames, nil
}

// cancelGraceForTerminal mirrors #cancelGraceForTerminal.
func (s *ObservabilityRpcServer) cancelGraceForTerminal(
	herdrSessionName string,
	terminalID string,
) {
	key := terminalPresenceKey(herdrSessionName, terminalID)
	s.cancelTimer(s.disconnectTimers, key)
	s.cancelTimer(s.startupTimers, key)
}

// resolvePaneGeneration mirrors #resolvePaneGeneration.
func (s *ObservabilityRpcServer) resolvePaneGeneration(
	herdrSessionName string,
	paneID string,
	terminalID string,
) (*string, error) {
	indexed, err := s.indexedForPresence(herdrSessionName, paneID, terminalID)
	if err != nil || indexed == nil {
		return nil, err
	}
	return indexed.PaneGeneration, nil
}

// isPiTerminal mirrors #isPiTerminal.
func (s *ObservabilityRpcServer) isPiTerminal(
	herdrSessionName string,
	paneID string,
	terminalID string,
) (bool, error) {
	indexed, err := s.indexedForPresence(herdrSessionName, paneID, terminalID)
	if err != nil {
		return false, err
	}
	if indexed != nil && indexed.Agent != nil && *indexed.Agent == "pi" {
		return true, nil
	}
	for connection := range s.socketsByTerminal[terminalPresenceKey(herdrSessionName, terminalID)] {
		if presence, ok := s.piPresenceBySocket[connection]; ok &&
			presence.SubscriberKind != nil &&
			*presence.SubscriberKind == observability.SubscriberPi {
			return true, nil
		}
	}
	return false, nil
}

// hasTerminalPresence mirrors #hasTerminalPresence: a live Pi socket, or an Agy
// lease whose pane is still open. The lease is refreshed and dropped in place,
// exactly like the TS mutation of the stored lease object.
func (s *ObservabilityRpcServer) hasTerminalPresence(
	herdrSessionName string,
	terminalID string,
) (bool, error) {
	key := terminalPresenceKey(herdrSessionName, terminalID)
	if len(s.socketsByTerminal[key]) > 0 {
		return true, nil
	}
	lease, ok := s.agyPresenceByTerminal[key]
	if !ok {
		return false, nil
	}
	presence := lease.Presence
	indexed, err := s.indexedForPresence(
		presence.HerdrSessionName,
		presence.PaneID,
		presence.TerminalID,
	)
	if err != nil {
		return false, err
	}
	if indexed != nil && indexed.PaneGeneration != nil {
		lease.PaneGeneration = indexed.PaneGeneration
		s.agyPresenceByTerminal[key] = lease
	}
	expired := indexed != nil && indexed.Agent != nil && *indexed.Agent != "agy"
	if !expired {
		closed, closeErr := s.stores.Agents.IsPaneClosedSince(db.IsPaneClosedInput{
			HerdrSessionName: presence.HerdrSessionName,
			PaneID:           presence.PaneID,
			PaneGeneration:   lease.PaneGeneration,
			SinceMs:          presence.ConnectedAt,
		})
		if closeErr != nil {
			return false, closeErr
		}
		expired = closed
	}
	if expired {
		delete(s.agyPresenceByTerminal, key)
		return false, nil
	}
	return true, nil
}

// indexedForPresence mirrors the `findByTerminal(...) ?? findByPane(...)` pair
// the three presence helpers share.
func (s *ObservabilityRpcServer) indexedForPresence(
	herdrSessionName string,
	paneID string,
	terminalID string,
) (*observability.AgentIndexRecord, error) {
	byTerminal, err := s.stores.Agents.FindByTerminal(db.FindAgentByTerminalInput{
		HerdrSessionName: herdrSessionName,
		TerminalID:       terminalID,
	})
	if err != nil {
		return nil, err
	}
	if byTerminal != nil {
		return byTerminal, nil
	}
	return s.stores.Agents.FindByPane(db.FindAgentByPaneInput{
		HerdrSessionName: herdrSessionName,
		PaneID:           paneID,
	})
}

// publishOrchestratorChange mirrors #publishOrchestratorChange. It collects the
// frames of every connection in either scope; the caller writes them once the
// lock is released.
func (s *ObservabilityRpcServer) publishOrchestratorChange(
	change observability.AgentOrchestratorChanged,
) []pendingFrame {
	frames := make([]pendingFrame, 0, len(s.piPresenceBySocket))
	for connection, presence := range s.piPresenceBySocket {
		if sameScope(presence.Scope(), change.Previous.AgentScope) ||
			sameScope(presence.Scope(), change.Current.AgentScope) {
			frames = append(frames, pendingFrame{
				target: connection,
				message: map[string]any{
					"method": "agent.orchestrator.changed",
					"params": map[string]any{"change": change},
				},
			})
		}
	}
	return frames
}

// cancelTimer mirrors #cancelTimer.
func (s *ObservabilityRpcServer) cancelTimer(registry map[string]graceTimer, key string) {
	timer, ok := registry[key]
	if !ok {
		return
	}
	s.clearTimeout(timer.handle)
	delete(registry, key)
}

// clearGraceTimers mirrors #clearGraceTimers.
func (s *ObservabilityRpcServer) clearGraceTimers(registry map[string]graceTimer) {
	for _, timer := range registry {
		s.clearTimeout(timer.handle)
	}
	clear(registry)
}

// ---------------------------------------------------------------------------
// Timer and process seams
// ---------------------------------------------------------------------------

// defaultSetTimeout mirrors the JS setTimeout with a *time.Timer handle. The
// callback runs on its own goroutine and takes the server lock itself, so no
// lock is ever held across a timer firing.
func defaultSetTimeout(callback func(), delayMs int) any {
	return time.AfterFunc(time.Duration(delayMs)*time.Millisecond, callback)
}

func defaultClearTimeout(handle any) {
	if timer, ok := handle.(*time.Timer); ok {
		timer.Stop()
	}
}

// intervalHandle is the Go counterpart of a JS interval handle.
type intervalHandle struct {
	stop chan struct{}
	once sync.Once
}

func (h *intervalHandle) stopTicking() { h.once.Do(func() { close(h.stop) }) }

// defaultSetInterval mirrors the JS setInterval: the callback repeats until the
// handle is cleared.
func defaultSetInterval(callback func(), delayMs int) any {
	handle := &intervalHandle{stop: make(chan struct{})}
	interval := time.Duration(delayMs) * time.Millisecond
	if interval <= 0 {
		interval = time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-handle.stop:
				return
			case <-ticker.C:
				callback()
			}
		}
	}()
	return handle
}

func defaultClearInterval(handle any) {
	if typed, ok := handle.(*intervalHandle); ok {
		typed.stopTicking()
	}
}

// defaultPeerPidOf mirrors the TS `linuxPeerPid(socket)` default for the
// generic net.Conn the accept loop hands out.
func defaultPeerPidOf(conn net.Conn) (int, bool) {
	unix, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	return shared.LinuxPeerPid(unix)
}

// defaultObservabilityWarn mirrors console.warn.
func defaultObservabilityWarn(message string, payload any) {
	if payload == nil {
		fmt.Fprintf(os.Stderr, "%s\n", message)
		return
	}
	fmt.Fprintf(os.Stderr, "%s %v\n", message, payload)
}
