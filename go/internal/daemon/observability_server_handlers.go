package daemon

// This file holds the RPC half of the observability server ported from
// src/daemon/observability-server.ts: one connection, the method dispatch, the
// presence resolution and the wire shapes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/observability"
	"github.com/dorokuma/herdsman/go/internal/shared"
)

// errInvalidRPCParams mirrors the TS assertSchema failure: the message is the
// whole payload, and it is what every TS client has ever seen.
var errInvalidRPCParams = errors.New("Invalid RPC params")

// pendingFrame is a frame that has been built under the server lock and is
// waiting to be written once the lock is released.
type pendingFrame struct {
	target  *rpcConnection
	message map[string]any
}

// enqueueAll queues every collected frame on its target's send queue. It runs
// with the server lock held: the append is the ordering point for the frames, so
// it belongs where the server's own order is fixed.
func enqueueAll(frames []pendingFrame) {
	for _, frame := range frames {
		frame.target.enqueueMessage(frame.message)
	}
}

// flushAll drains every connection that has frames queued. It runs with the
// server lock released, so a wedged peer's write cannot stall the critical
// section.
func flushAll(frames []pendingFrame) {
	for _, frame := range frames {
		frame.target.flushQueue()
	}
}

// rpcWriteTimeout bounds one frame write. TS writes on a socket that applies
// backpressure and never fails; a Go socket blocks instead when the peer stops
// reading, so the port caps it. Two seconds is far beyond any local UDS frame
// and keeps a wedged client from stalling the daemon.
const rpcWriteTimeout = 2 * time.Second

// rpcConnection is one accepted client connection, the Go counterpart of the TS
// `Socket`. `destroyed` mirrors `socket.destroyed`.
//
// Frames do not reach the socket from the goroutine that produced them. A frame
// is built (and JSON-encoded) while its publisher holds whatever state produced
// it, then appended to this connection's send queue; a single drainer takes the
// queue in FIFO order and writes it. That is what keeps the peer's byte order
// equal to the order the frames were *generated* in: two goroutines that race
// for the socket after their generation order is fixed would otherwise reorder
// them (a newer frame overtaking an older one).
type rpcConnection struct {
	conn net.Conn

	queueMu sync.Mutex
	queue   []string
	// draining marks that a drainer owns the queue, so exactly one goroutine
	// writes at a time and the queue is never left unclaimed while non-empty.
	draining  bool
	destroyed atomic.Bool
}

// enqueueMessage encodes one frame and appends it to the send queue, then
// returns. It does no socket I/O of its own.
//
// The caller decides the order (this is called with the publisher's state held),
// so the append is the ordering point: a peer's byte order matches the order
// these appends happened in, not the order goroutines happened to be scheduled
// in afterwards. Encoding belongs here rather than in the writer for the same
// reason: an expensive-to-serialise frame must not be able to slip a newer,
// cheaper frame past it, which is exactly what a write-after-unlock shape does.
func (c *rpcConnection) enqueueMessage(message any) {
	frame, err := shared.EncodeJSONLine(message)
	if err != nil {
		// A value JSON.stringify cannot produce (impossible for these payloads,
		// which are built from decoded JSON and the port's own records) is dropped
		// exactly like a TS write that never happens.
		return
	}
	c.queueMu.Lock()
	if c.destroyed.Load() {
		c.queueMu.Unlock()
		return
	}
	c.queue = append(c.queue, frame)
	// Exactly one drainer at a time: the first append of a burst claims the
	// queue, and every later append only grows it.
	claim := !c.draining
	if claim {
		c.draining = true
	}
	c.queueMu.Unlock()
	if claim {
		go c.drain()
	}
}

// flushQueue makes sure the send queue is being drained. It is the entry point
// the publish paths call once they have released the server lock.
//
// It never writes a frame itself: a drainer already owns the queue whenever
// frames are waiting, so this only starts one when the queue was left idle (a
// connection whose drainer finished between the append and this call). Calling
// it therefore cannot block the caller on a wedged peer — the append already
// fixed the order, and the drain is the only part that touches the socket.
func (c *rpcConnection) flushQueue() {
	c.queueMu.Lock()
	claim := !c.draining && len(c.queue) > 0
	if claim {
		c.draining = true
	}
	c.queueMu.Unlock()
	if claim {
		go c.drain()
	}
}

// drain writes the send queue in strict FIFO order. It runs until the queue is
// empty, then hands the ownership back so the next append can start a new
// drainer. Exactly one goroutine is inside it per connection at any time.
func (c *rpcConnection) drain() {
	for {
		c.queueMu.Lock()
		if len(c.queue) == 0 {
			c.draining = false
			c.queueMu.Unlock()
			return
		}
		frame := c.queue[0]
		c.queue = c.queue[1:]
		c.queueMu.Unlock()

		if !c.writeFrame(frame) {
			// The connection is gone: drop whatever is queued behind the failed
			// frame instead of spending the write timeout on each of them.
			c.queueMu.Lock()
			c.queue = nil
			c.draining = false
			c.queueMu.Unlock()
			return
		}
	}
}

// destroy mirrors `socket.destroy()`: idempotent, and the connection stops
// accepting writes the moment it starts.
func (c *rpcConnection) destroy() {
	if c.destroyed.CompareAndSwap(false, true) {
		c.queueMu.Lock()
		c.queue = nil
		c.queueMu.Unlock()
		_ = c.conn.Close()
	}
}

// writeFrame is the one place a frame reaches the socket. It reports whether
// the frame was written.
//
// A peer that stops reading must not be able to block the daemon: net.Conn.Write
// waits for the socket buffer, and the callers of this method are the server's
// publish paths, which the TS original ran while holding its lock. Every write
// therefore carries a deadline, and a failed write closes the connection — the
// socket is unusable after a write error anyway, and destroying it here unblocks
// the reader goroutine that is still parked on it.
func (c *rpcConnection) writeFrame(frame string) bool {
	if c.destroyed.Load() {
		return false
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(rpcWriteTimeout))
	if _, err := c.conn.Write([]byte(frame)); err != nil {
		c.destroy()
		return false
	}
	return true
}

// writeMessage queues one RPC frame for writing. See enqueueMessage for why the
// append, not the write, is the ordering point.
func (c *rpcConnection) writeMessage(message any) {
	c.enqueueMessage(message)
}

// rpcRequest is the decoded TS `RpcRequest`. `hasID` keeps "no id at all"
// distinct from `id: null`: TS omits an undefined id from the response, and
// `RpcTestClient` decides notification-versus-response by looking at that.
type rpcRequest struct {
	hasID  bool
	id     any
	method string
	params any
}

// rpcRequestOf decodes one frame. A frame that is not an object behaves like the
// TS cast it is: every property read is undefined, so it is reported as a
// missing method.
func rpcRequestOf(message any) rpcRequest {
	request := rpcRequest{params: map[string]any{}}
	object, ok := message.(map[string]any)
	if !ok {
		return request
	}
	if raw, present := object["id"]; present {
		request.hasID = true
		request.id = raw
	}
	if raw, present := object["params"]; present && raw != nil {
		request.params = raw
	}
	method, _ := object["method"].(string)
	request.method = method
	return request
}

// handleRequest mirrors #handleRequest: dispatch, then one response frame holding
// either the result or the error.
func (s *ObservabilityRpcServer) handleRequest(connection *rpcConnection, message any) {
	request := rpcRequestOf(message)
	result, err := s.dispatch(connection, request)
	response := map[string]any{}
	if request.hasID {
		response["id"] = request.id
	}
	if err != nil {
		response["error"] = observabilityRPCError(err)
	} else {
		response["result"] = result
	}
	connection.writeMessage(response)
}

// observabilityRPCError mirrors the TS error object: the OrchestratorAckError
// code and retryability first, then the message.
func observabilityRPCError(err error) map[string]any {
	errorObject := map[string]any{}
	var ackError *observability.OrchestratorAckError
	if errors.As(err, &ackError) {
		errorObject["code"] = string(ackError.Code)
		errorObject["retryable"] = ackError.Retryable()
	}
	errorObject["message"] = err.Error()
	return errorObject
}

// dispatch mirrors #dispatch. Every method validates its params against the same
// schema the TS server uses, and the lock is taken inside each handler around
// the synchronous block the TS original runs between two awaits.
func (s *ObservabilityRpcServer) dispatch(
	connection *rpcConnection,
	request rpcRequest,
) (any, error) {
	if request.method == "" {
		return nil, errors.New("Missing method")
	}
	switch request.method {
	case "agent.list":
		return s.handleAgentList(request.params)
	case "agent.get":
		return s.handleAgentGet(request.params)
	case "agent.read":
		return s.handleAgentRead(request.params)
	case "agent.events":
		return s.handleAgentEvents(request.params)
	case "agent.orchestrator.register":
		return s.handleOrchestratorRegister(connection, request.params)
	case "agent.orchestrator.get":
		return s.handleOrchestratorGet(connection, request.params)
	case "agent.orchestrator.set":
		return s.handleOrchestratorSet(connection, request.params)
	case "agent.notifications.ack":
		return s.handleOrchestratorAck(connection, request.params)
	case "agent.turn.completed":
		return s.handleTurnCompleted(connection, request.params)
	case "agent.ping":
		return s.handlePing(request.params)
	default:
		return nil, fmt.Errorf("Unknown method: %s", request.method)
	}
}

// ---------------------------------------------------------------------------
// agent.list / agent.get / agent.read / agent.events
// ---------------------------------------------------------------------------

// handleAgentList mirrors the agent.list branch.
func (s *ObservabilityRpcServer) handleAgentList(params any) (any, error) {
	if !observability.AgentListInputSchema.Check(params) {
		return nil, errInvalidRPCParams
	}
	var input struct {
		observability.AgentQueryScope
		IncludeOwnerlessWorkspaces *bool `json:"includeOwnerlessWorkspaces"`
	}
	if err := decodeRPCParams(params, &input); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	scope, err := s.resolveScope(input.AgentQueryScope)
	if err != nil {
		return nil, err
	}
	if truthy(input.IncludeOwnerlessWorkspaces) &&
		stringPresent(scope.HerdrSessionName) && stringPresent(scope.WorkspaceID) {
		ownScope := observability.AgentScope{
			HerdrSessionName: *scope.HerdrSessionName,
			WorkspaceID:      *scope.WorkspaceID,
		}
		siblingScopes, err := s.orchestrator.OwnerlessSiblingScopes(ownScope)
		if err != nil {
			return nil, err
		}
		agents, err := s.context.ListAgents(scope)
		if err != nil {
			return nil, err
		}
		for _, sibling := range siblingScopes {
			siblingAgents, err := s.context.ListAgents(agentQueryScopeOf(sibling))
			if err != nil {
				return nil, err
			}
			agents = append(agents, siblingAgents...)
		}
		return map[string]any{"agents": nonNilAgents(agents)}, nil
	}
	agents, err := s.context.ListAgents(scope)
	if err != nil {
		return nil, err
	}
	return map[string]any{"agents": nonNilAgents(agents)}, nil
}

// handleAgentGet mirrors the agent.get branch.
//
// An explicit read is an operator action, not the daemon's background cadence:
// it re-resolves from the official values so a lookup that just failed (and is
// therefore memoized for the background rounds) cannot answer this call with an
// empty history.
func (s *ObservabilityRpcServer) handleAgentGet(params any) (any, error) {
	if !observability.AgentGetInputSchema.Check(params) {
		return nil, errInvalidRPCParams
	}
	var input struct {
		observability.AgentQueryScope
		Target string `json:"target"`
	}
	if err := decodeRPCParams(params, &input); err != nil {
		return nil, err
	}

	s.mu.Lock()
	scope, err := s.resolveScope(input.AgentQueryScope)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	agent, err := s.stores.Agents.ResolveTarget(scope, input.Target)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	lookup := s.context.HistoryLookupInput(agent)
	s.mu.Unlock()

	// TS awaits resolveCompactHistory here, so the history lookup runs outside
	// the critical section.
	resolved := s.history.ResolveCompactHistory(
		lookup,
		observability.AgentHistoryResolveOptions{ForceDiscovery: true},
	)

	agentObject, err := jsonValueObject(agent)
	if err != nil {
		return nil, err
	}
	agentObject["history"] = resolved.CompactHistory
	return map[string]any{"agent": agentObject}, nil
}

// handleAgentRead mirrors the agent.read branch.
func (s *ObservabilityRpcServer) handleAgentRead(params any) (any, error) {
	if !observability.AgentReadInputSchema.Check(params) {
		return nil, errInvalidRPCParams
	}
	var input struct {
		observability.AgentQueryScope
		Limit  *int   `json:"limit"`
		Target string `json:"target"`
	}
	if err := decodeRPCParams(params, &input); err != nil {
		return nil, err
	}

	s.mu.Lock()
	scope, err := s.resolveScope(input.AgentQueryScope)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	agent, err := s.stores.Agents.ResolveTarget(scope, input.Target)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	lookup := s.context.HistoryLookupInput(agent)
	s.mu.Unlock()

	limit := 20
	if input.Limit != nil {
		limit = *input.Limit
	}
	read := s.history.Read(lookup, ObservabilityHistoryReadOptions{
		ForceDiscovery: true,
		Limit:          limit,
	})

	agentObject, err := jsonValueObject(agent)
	if err != nil {
		return nil, err
	}
	agentObject["historyRef"] = read.HistoryRef
	agentObject["messages"] = nonNilMessages(read.Messages)
	return map[string]any{"agent": agentObject}, nil
}

// handleAgentEvents mirrors the agent.events branch.
func (s *ObservabilityRpcServer) handleAgentEvents(params any) (any, error) {
	if !observability.AgentEventsInputSchema.Check(params) {
		return nil, errInvalidRPCParams
	}
	var input db.ListAfterInput
	if err := decodeRPCParams(params, &input); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.stores.AgentEvents.ListAfter(input)
	if err != nil {
		return nil, err
	}
	return map[string]any{"events": nonNilEvents(events)}, nil
}

// ---------------------------------------------------------------------------
// agent.orchestrator.* and agent.turn.completed / agent.ping
// ---------------------------------------------------------------------------

// handleOrchestratorRegister mirrors the agent.orchestrator.register branch,
// including the Pi sessionRef registration and the Agy lease hand-off.
func (s *ObservabilityRpcServer) handleOrchestratorRegister(
	connection *rpcConnection,
	params any,
) (any, error) {
	if !observability.AgentOrchestratorRegisterInputSchema.Check(params) {
		return nil, errInvalidRPCParams
	}
	var registrationInput observability.PiPresenceRegistration
	if err := decodeRPCParams(params, &registrationInput); err != nil {
		return nil, err
	}

	s.mu.Lock()
	previous, hasPrevious := s.piPresenceBySocket[connection]
	s.mu.Unlock()

	presence, err := s.resolvePiPresence(connection, registrationInput)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if hasPrevious {
		s.unregisterTerminalSocket(connection, previous)
	}
	s.piPresenceBySocket[connection] = presence
	s.registerTerminalSocket(connection, presence)
	presenceKey := terminalPresenceKey(presence.HerdrSessionName, presence.TerminalID)
	state, err := s.orchestrator.Status(presence.Scope())
	if err != nil {
		return nil, err
	}
	var currentOwner *observability.AgentOrchestratorOwner
	if state != nil {
		currentOwner = state.Owner
	}
	if !presenceIsAgy(presence) {
		delete(s.agyPresenceByTerminal, presenceKey)
		s.cancelGraceForTerminal(presence.HerdrSessionName, presence.TerminalID)
	} else if currentOwner != nil && currentOwner.TerminalID == presence.TerminalID {
		isPi, err := s.isPiTerminal(presence.HerdrSessionName, presence.PaneID, presence.TerminalID)
		if err != nil {
			return nil, err
		}
		if !isPi {
			paneGeneration, err := s.resolvePaneGeneration(
				presence.HerdrSessionName,
				presence.PaneID,
				presence.TerminalID,
			)
			if err != nil {
				return nil, err
			}
			s.agyPresenceByTerminal[presenceKey] = agyTerminalLease{
				PaneGeneration: paneGeneration,
				Presence:       presence,
			}
			s.cancelGraceForTerminal(presence.HerdrSessionName, presence.TerminalID)
		}
	}
	s.lastActivityByTerminal[presenceKey] = s.now()

	if hasPrevious &&
		terminalPresenceKey(previous.HerdrSessionName, previous.TerminalID) != presenceKey &&
		!presenceIsAgy(previous) {
		if err := s.scheduleDisconnect(previous); err != nil {
			return nil, err
		}
	}

	shouldRegisterSessionRef := true
	if presenceIsAgy(presence) {
		// TS: sessionRef.kind === "id" && source !== "agy-subagent" &&
		// isUuidLikeSessionId(value) && !#isPiTerminal(presence). Every conjunct
		// short-circuits, so the terminal lookup only runs when the first three
		// hold.
		if registrationInput.SessionRef.Kind == observability.SessionRefID &&
			registrationInput.SessionRef.Source != "agy-subagent" &&
			isUuidLikeSessionID(registrationInput.SessionRef.Value) {
			isPi, err := s.isPiTerminal(
				presence.HerdrSessionName,
				presence.PaneID,
				presence.TerminalID,
			)
			if err != nil {
				return nil, err
			}
			shouldRegisterSessionRef = !isPi
		} else {
			shouldRegisterSessionRef = false
		}
	}
	if shouldRegisterSessionRef {
		// TS awaits registerPiSessionRef here, then publishes each changed scope.
		s.mu.Unlock()
		registration, err := s.registerPiSessionRef(observability.RegisterPiSessionRefInput{
			HerdrSessionName: presence.HerdrSessionName,
			SessionRef:       registrationInput.SessionRef,
			TerminalID:       presence.TerminalID,
		})
		s.mu.Lock()
		if err != nil {
			return nil, err
		}
		// The context publications are collected under the lock and appended to
		// the target queues under it as well, then drained after it is released,
		// exactly like PublishAgentContext: a frame write must never run inside
		// the critical section a wedged peer could block, and the order the
		// server generated the frames in must survive the release.
		contextFrames := make([]pendingFrame, 0, len(registration.ContextChangedScopes))
		for _, scope := range registration.ContextChangedScopes {
			if target, message, ok := s.publishAgentContextLocked(scope); ok {
				contextFrames = append(contextFrames, pendingFrame{target: target, message: message})
			}
		}
		enqueueAll(contextFrames)
		s.mu.Unlock()
		flushAll(contextFrames)
		s.mu.Lock()
	}
	return s.connectionStateLocked(presence)
}

// handleOrchestratorGet mirrors the agent.orchestrator.get branch.
func (s *ObservabilityRpcServer) handleOrchestratorGet(
	connection *rpcConnection,
	params any,
) (any, error) {
	if !observability.AgentOrchestratorGetInputSchema.Check(params) {
		return nil, errInvalidRPCParams
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	presence, err := s.requirePiPresence(connection)
	if err != nil {
		return nil, err
	}
	return s.connectionStateLocked(presence)
}

// handleOrchestratorSet mirrors the agent.orchestrator.set branch.
func (s *ObservabilityRpcServer) handleOrchestratorSet(
	connection *rpcConnection,
	params any,
) (any, error) {
	if !observability.AgentOrchestratorSetInputSchema.Check(params) {
		return nil, errInvalidRPCParams
	}
	var input struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeRPCParams(params, &input); err != nil {
		return nil, err
	}

	// The orchestrator-change notifications are appended to their send queues
	// while the lock is held (the ordering point) and drained after it is
	// released: the TS original awaited its socket writes outside the critical
	// section, and a Go write to a peer that stopped reading blocks.
	frames, result, err := func() ([]pendingFrame, map[string]any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		frames, result, err := s.orchestratorSetLocked(connection, input.Enabled)
		enqueueAll(frames)
		return frames, result, err
	}()
	flushAll(frames)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// orchestratorSetLocked is the agent.orchestrator.set branch with the lock held.
// It returns the frames to write once the lock is released.
func (s *ObservabilityRpcServer) orchestratorSetLocked(
	connection *rpcConnection,
	enabled bool,
) ([]pendingFrame, map[string]any, error) {
	var frames []pendingFrame
	presence, err := s.requirePiPresence(connection)
	if err != nil {
		return nil, nil, err
	}
	changed := false
	if enabled {
		current, err := s.orchestrator.Status(presence.Scope())
		if err != nil {
			return frames, nil, err
		}
		ownerConnected := false
		if current != nil && current.Owner != nil {
			ownerConnected, err = s.hasTerminalPresence(
				presence.HerdrSessionName,
				current.Owner.TerminalID,
			)
			if err != nil {
				return frames, nil, err
			}
		}
		change, err := s.orchestrator.Claim(observability.OrchestratorClaimInput{
			AgentScope:     presence.Scope(),
			OwnerConnected: &ownerConnected,
			PaneID:         presence.PaneID,
			TerminalID:     presence.TerminalID,
		})
		if err != nil {
			return frames, nil, err
		}
		if presenceIsAgy(presence) {
			isPi, err := s.isPiTerminal(
				presence.HerdrSessionName,
				presence.PaneID,
				presence.TerminalID,
			)
			if err != nil {
				return frames, nil, err
			}
			if !isPi {
				paneGeneration, err := s.resolvePaneGeneration(
					presence.HerdrSessionName,
					presence.PaneID,
					presence.TerminalID,
				)
				if err != nil {
					return frames, nil, err
				}
				s.agyPresenceByTerminal[terminalPresenceKey(presence.HerdrSessionName, presence.TerminalID)] = agyTerminalLease{PaneGeneration: paneGeneration, Presence: presence}
				s.cancelGraceForTerminal(presence.HerdrSessionName, presence.TerminalID)
			}
		}
		changed = !sameOwner(change.Current.Owner, change.Previous.Owner)
		if changed {
			frames = append(frames, s.publishOrchestratorChange(toWireChange(change))...)
		}
	} else {
		delete(
			s.agyPresenceByTerminal,
			terminalPresenceKey(presence.HerdrSessionName, presence.TerminalID),
		)
		change, err := s.orchestrator.Release(observability.OrchestratorReleaseInput{
			AgentScope: presence.Scope(),
			Reason:     observability.OrchestratorChangeReleased,
			TerminalID: presence.TerminalID,
		})
		if err != nil {
			return frames, nil, err
		}
		changed = change != nil
		if change != nil {
			frames = append(frames, s.publishOrchestratorChange(toWireChange(*change))...)
		}
	}
	state, err := s.connectionStateLocked(presence)
	if err != nil {
		return frames, nil, err
	}
	result, err := jsonValueObject(state)
	if err != nil {
		return frames, nil, err
	}
	result["changed"] = changed
	return frames, result, nil
}

// handleOrchestratorAck mirrors the agent.notifications.ack branch.
func (s *ObservabilityRpcServer) handleOrchestratorAck(
	connection *rpcConnection,
	params any,
) (any, error) {
	if !observability.AgentOrchestratorAckInputSchema.Check(params) {
		return nil, errInvalidRPCParams
	}
	var input struct {
		EventID int64 `json:"eventId"`
	}
	if err := decodeRPCParams(params, &input); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	presence, err := s.requirePiPresence(connection)
	if err != nil {
		return nil, err
	}
	state, err := s.orchestrator.Ack(observability.OrchestratorAckInput{
		AgentScope: presence.Scope(),
		EventID:    input.EventID,
		TerminalID: presence.TerminalID,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"acknowledged": true, "state": toWireState(state)}, nil
}

// handleTurnCompleted mirrors the agent.turn.completed branch: the signal is
// recorded against the connection's presence, not against the workspace the
// caller names in `workspaceId`.
func (s *ObservabilityRpcServer) handleTurnCompleted(
	connection *rpcConnection,
	params any,
) (any, error) {
	if !observability.AgentTurnCompletedInputSchema.Check(params) {
		return nil, errInvalidRPCParams
	}
	var input observability.AgentTurnCompletedInput
	if err := decodeRPCParams(params, &input); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	presence, err := s.requirePiPresence(connection)
	if err != nil {
		return nil, err
	}
	signal := observability.SessionScopedSignal{
		Confirmed:        input.Confirmed,
		HerdrSessionName: presence.HerdrSessionName,
		PaneID:           presence.PaneID,
		TerminalID:       presence.TerminalID,
		WorkspaceID:      input.WorkspaceID,
	}
	if input.ExpectedText != nil {
		signal.HasExpectedText = true
		signal.ExpectedText = *input.ExpectedText
	}
	s.turnCompletions.Record(signal)
	return map[string]any{"accepted": true}, nil
}

// handlePing mirrors the agent.ping branch.
func (s *ObservabilityRpcServer) handlePing(params any) (any, error) {
	if !observability.AgentPingInputSchema.Check(params) {
		return nil, errInvalidRPCParams
	}
	return map[string]any{"ok": true}, nil
}

// ---------------------------------------------------------------------------
// Presence resolution
// ---------------------------------------------------------------------------

// resolvePiPresence mirrors #resolvePiPresence.
//
// The indexed branch answers straight from the agent index, but still binds the
// connector process to the pane when Herdr can identify it; the live branch asks
// Herdr itself. The Herdr round trip runs with the server lock released, because
// the TS original awaits it.
func (s *ObservabilityRpcServer) resolvePiPresence(
	connection *rpcConnection,
	input observability.PiPresenceRegistration,
) (PiPresence, error) {
	s.mu.Lock()
	session, err := s.stores.HerdrSessions.FindRunningBySocketPath(input.HerdrSocketPath)
	if err != nil {
		s.mu.Unlock()
		return PiPresence{}, err
	}
	if session == nil {
		s.mu.Unlock()
		return PiPresence{}, errors.New("Herdr socket is not registered as a running session")
	}
	indexed, err := s.stores.Agents.FindByPane(db.FindAgentByPaneInput{
		HerdrSessionName: session.Name,
		PaneID:           input.PaneID,
	})
	if err != nil {
		s.mu.Unlock()
		return PiPresence{}, err
	}
	socketPath := session.SocketPath
	sessionName := session.Name
	indexedMatches := indexed != nil && indexed.WorkspaceID == input.WorkspaceID &&
		stringPresent(indexed.TerminalID)
	// TS builds the indexed presence — connectedAt included — before it awaits the
	// pane identity, so the clock is read here rather than after the round trip.
	connectedAt := s.now()
	var indexedPresence *PiPresence
	if indexedMatches {
		indexedPresence = &PiPresence{
			ConnectedAt:      connectedAt,
			HerdrSessionName: sessionName,
			PaneID:           indexed.PaneID,
			SubscriberID:     input.SubscriberID,
			SubscriberKind:   subscriberKindValue(input.SubscriberKind),
			TerminalID:       *indexed.TerminalID,
			WorkspaceID:      indexed.WorkspaceID,
		}
	}
	s.mu.Unlock()

	if indexedPresence != nil {
		if err := s.assertConnectorBoundToPane(
			connection,
			s.tryPaneIdentity(socketPath, input.PaneID),
		); err != nil {
			return PiPresence{}, err
		}
		return *indexedPresence, nil
	}

	live, err := s.resolvePaneIdentity(herdr.ResolveHerdrPaneIdentityInput{
		PaneID:     input.PaneID,
		SocketPath: socketPath,
	})
	if err != nil {
		if errors.Is(err, ErrConnectorPaneMismatch) {
			return PiPresence{}, err
		}
		if indexed == nil {
			return PiPresence{}, errors.New("Herdr pane is not indexed yet")
		}
		if indexed.WorkspaceID != input.WorkspaceID {
			return PiPresence{}, errors.New(
				"Pi presence workspace does not match indexed Herdr pane",
			)
		}
		return PiPresence{}, errors.New("Herdr pane has no terminal identity")
	}
	s.mu.Lock()
	presence := PiPresence{
		// TS reads the clock after the Herdr round trip on this branch.
		ConnectedAt:      s.now(),
		HerdrSessionName: sessionName,
		PaneID:           live.PaneID,
		SubscriberID:     input.SubscriberID,
		SubscriberKind:   subscriberKindValue(input.SubscriberKind),
		TerminalID:       live.TerminalID,
		WorkspaceID:      live.WorkspaceID,
	}
	s.mu.Unlock()
	if err := s.assertConnectorBoundToPane(connection, &live); err != nil {
		return PiPresence{}, err
	}
	return presence, nil
}

// tryPaneIdentity mirrors #tryPaneIdentity: an unresolvable pane is not an
// error, it only removes the connector binding. Every failure is swallowed,
// including a connector mismatch, and only removes the pane from the binding
// check below.
func (s *ObservabilityRpcServer) tryPaneIdentity(
	socketPath string,
	paneID string,
) *herdr.HerdrPaneIdentity {
	pane, err := s.resolvePaneIdentity(herdr.ResolveHerdrPaneIdentityInput{
		PaneID:     paneID,
		SocketPath: socketPath,
	})
	if err != nil {
		return nil
	}
	return &pane
}

// assertConnectorBoundToPane mirrors #assertConnectorBoundToPane.
func (s *ObservabilityRpcServer) assertConnectorBoundToPane(
	connection *rpcConnection,
	pane *herdr.HerdrPaneIdentity,
) error {
	var paneCwd string
	if pane != nil {
		paneCwd = derefString(pane.CWD)
		if paneCwd == "" {
			paneCwd = derefString(pane.ForegroundCwd)
		}
	}
	if pane == nil || paneCwd == "" {
		s.warn("Herdsman could not resolve Herdr pane cwd for connector binding", nil)
		return nil
	}
	peerPid, ok := s.peerPidOf(connection.conn)
	if !ok {
		s.warn("Herdsman could not read UDS peer pid for pane binding", nil)
		return nil
	}
	bound := false
	if pane.CWD != nil && shared.PeerBoundToPaneCwd(peerPid, *pane.CWD) {
		bound = true
	}
	if !bound && pane.ForegroundCwd != nil &&
		shared.PeerBoundToPaneCwd(peerPid, *pane.ForegroundCwd) {
		bound = true
	}
	if !bound {
		return ErrConnectorPaneMismatch
	}
	return nil
}

// connectionStateLocked mirrors #connectionState.
func (s *ObservabilityRpcServer) connectionStateLocked(
	presence PiPresence,
) (OrchestratorConnectionState, error) {
	state, err := s.orchestrator.Status(presence.Scope())
	if err != nil {
		return OrchestratorConnectionState{}, err
	}
	isOwner := state != nil && state.Owner != nil && state.Owner.TerminalID == presence.TerminalID
	var context *observability.AgentWorkspaceContextSnapshot
	if isOwner {
		context, err = s.context.WorkspaceSnapshot(observability.WorkspaceSnapshotInput{
			AgentScope:        presence.Scope(),
			ExcludeTerminalID: presence.TerminalID,
		})
		if err != nil {
			return OrchestratorConnectionState{}, err
		}
	}
	pendingInput := observability.OrchestratorPendingInput{
		AgentScope: presence.Scope(),
		Limit:      intPointer(100),
		TerminalID: presence.TerminalID,
	}
	if isOwner {
		// The session's orchestrator also receives the events of ownerless
		// sibling workspaces (workers dispatched across workspaces).
		siblings, err := s.orchestrator.OwnerlessSiblingScopes(presence.Scope())
		if err != nil {
			return OrchestratorConnectionState{}, err
		}
		pendingInput.ExtraScopes = siblings
	}
	events, err := s.orchestrator.Pending(pendingInput)
	if err != nil {
		return OrchestratorConnectionState{}, err
	}
	result := OrchestratorConnectionState{
		Context:  context,
		Events:   nonNilEvents(events),
		Presence: presence,
	}
	if state != nil {
		wire := toWireState(*state)
		result.State = &wire
	}
	return result, nil
}

// requirePiPresence mirrors #requirePiPresence.
func (s *ObservabilityRpcServer) requirePiPresence(
	connection *rpcConnection,
) (PiPresence, error) {
	presence, ok := s.piPresenceBySocket[connection]
	if !ok {
		return PiPresence{}, errors.New("Pi presence is not registered for this connection")
	}
	return presence, nil
}

// publishAgentContextLocked is publishAgentContext with the lock held, the form
// the register handler uses after the Pi session registration returns. It
// decides the target and builds the frame; the caller writes it after releasing
// the lock, so a wedged peer cannot block every other RPC.
func (s *ObservabilityRpcServer) publishAgentContextLocked(
	scope observability.AgentScope,
) (*rpcConnection, map[string]any, bool) {
	state, err := s.orchestrator.Status(scope)
	if err != nil {
		s.warn("Herdsman could not read the orchestrator owner for a context publication", err)
		return nil, nil, false
	}
	if state == nil || state.Owner == nil {
		return nil, nil, false
	}
	connection := s.newestSocketForTerminal(scope, state.Owner.TerminalID)
	if connection == nil {
		return nil, nil, false
	}
	context, err := s.context.WorkspaceSnapshot(observability.WorkspaceSnapshotInput{
		AgentScope:        scope,
		ExcludeTerminalID: state.Owner.TerminalID,
	})
	if err != nil {
		s.warn("Herdsman could not build the workspace context snapshot", err)
		return nil, nil, false
	}
	fingerprint, err := jsonStringify(context)
	if err != nil {
		s.warn("Herdsman could not fingerprint the workspace context snapshot", err)
		return nil, nil, false
	}
	scopeKey := scope.HerdrSessionName + "\x00" + scope.WorkspaceID
	if s.publishedContextFingerprintByScope[scopeKey] == fingerprint {
		return nil, nil, false
	}
	s.publishedContextFingerprintByScope[scopeKey] = fingerprint
	return connection, map[string]any{
		"method": "agent.context.changed",
		"params": map[string]any{
			"context":          context,
			"herdrSessionName": scope.HerdrSessionName,
			"workspaceId":      scope.WorkspaceID,
		},
	}, true
}

// resolveScope mirrors #resolveScope.
func (s *ObservabilityRpcServer) resolveScope(
	input observability.AgentQueryScope,
) (observability.AgentQueryScope, error) {
	if truthy(input.All) {
		scope := observability.AgentQueryScope{All: boolPointer(true)}
		if stringPresent(input.HerdrSessionName) {
			scope.HerdrSessionName = input.HerdrSessionName
		}
		return scope, nil
	}
	if stringPresent(input.WorkspaceID) && !stringPresent(input.HerdrSessionName) {
		agents, err := s.stores.Agents.List(observability.AgentQueryScope{
			WorkspaceID: input.WorkspaceID,
		})
		if err != nil {
			return observability.AgentQueryScope{}, err
		}
		// A Set keeps first-seen order, which is the order the TS message lists
		// the colliding sessions in.
		sessions := []string{}
		for _, agent := range agents {
			if !containsString(sessions, agent.HerdrSessionName) {
				sessions = append(sessions, agent.HerdrSessionName)
			}
		}
		if len(sessions) > 1 {
			return observability.AgentQueryScope{}, fmt.Errorf(
				"workspace %s exists in multiple Herdr sessions; pass --session <name>: %s",
				*input.WorkspaceID,
				strings.Join(sessions, ", "),
			)
		}
	}
	if stringPresent(input.WorkspaceID) || stringPresent(input.HerdrSessionName) {
		scope := observability.AgentQueryScope{}
		if stringPresent(input.HerdrSessionName) {
			scope.HerdrSessionName = input.HerdrSessionName
		}
		if stringPresent(input.WorkspaceID) {
			scope.WorkspaceID = input.WorkspaceID
		}
		return scope, nil
	}
	return observability.AgentQueryScope{}, errors.New(
		"agent scope requires current Herdr workspace, --workspace, --session, or --all",
	)
}

// ---------------------------------------------------------------------------
// Wire helpers
// ---------------------------------------------------------------------------

// toWireState mirrors toWireState: the same record with the timestamp rendered
// as the ISO string the wire carries. The Go store already keeps updatedAt in
// that form, so this is the copy TS's object spread performs.
func toWireState(state observability.AgentOrchestratorState,
) observability.AgentOrchestratorWireState {
	return observability.AgentOrchestratorWireState{
		AgentScope:   state.AgentScope,
		AckedEventID: state.AckedEventID,
		Owner:        state.Owner,
		UpdatedAt:    state.UpdatedAt,
	}
}

// toWireChange mirrors toWireChange.
func toWireChange(change observability.AgentOrchestratorChange,
) observability.AgentOrchestratorChanged {
	return observability.AgentOrchestratorChanged{
		Current:  toWireState(change.Current),
		Previous: toWireState(change.Previous),
		Reason:   change.Reason,
	}
}

// sameScope mirrors sameScope.
func sameScope(left observability.AgentScope, right observability.AgentScope) bool {
	return left.HerdrSessionName == right.HerdrSessionName &&
		left.WorkspaceID == right.WorkspaceID
}

// sameOwner mirrors sameOwner, including the `undefined === undefined` case the
// TS optional chaining produces for two ownerless states.
func sameOwner(
	left *observability.AgentOrchestratorOwner,
	right *observability.AgentOrchestratorOwner,
) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.TerminalID == right.TerminalID && left.PaneID == right.PaneID
}

// terminalPresenceKey mirrors terminalPresenceKey.
func terminalPresenceKey(herdrSessionName string, terminalID string) string {
	return herdrSessionName + "\x00" + terminalID
}

// uuidLikeSessionIDPattern mirrors the TS regex behind isUuidLikeSessionId.
var uuidLikeSessionIDPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
)

// isUuidLikeSessionID mirrors isUuidLikeSessionId (case-insensitive).
func isUuidLikeSessionID(value string) bool {
	return uuidLikeSessionIDPattern.MatchString(strings.ToLower(value))
}

// presenceIsAgy reports the TS `presence.subscriberKind === "agy"`.
func presenceIsAgy(presence PiPresence) bool {
	return presence.SubscriberKind != nil && *presence.SubscriberKind == observability.SubscriberAgy
}

// subscriberKindValue keeps the optional subscriberKind absent when the client
// sent none, which the registration schema makes impossible in practice.
func subscriberKindValue(kind observability.SubscriberKind) *observability.SubscriberKind {
	if kind == "" {
		return nil
	}
	value := kind
	return &value
}

// decodeRPCParams decodes validated params into their Go shape. The schema check
// has already rejected unknown keys, so a plain JSON round trip is enough; the
// store input structs it is used on carry no json tags, which is fine because
// encoding/json matches field names case-insensitively.
func decodeRPCParams(params any, target any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return fmt.Errorf("invalid RPC params: %w", err)
	}
	return nil
}

// jsonValueObject renders a value as the JSON object it marshals to, which is how
// this port performs the TS object spread (`{ ...agent, history }`) that adds
// keys to a record without duplicating its fields.
func jsonValueObject(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	object := map[string]any{}
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	return object, nil
}

// jsonStringify mirrors JSON.stringify for the publication fingerprint.
func jsonStringify(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// filterStatesForSession keeps the orchestrator states of one Herdr session.
func filterStatesForSession(
	states []observability.AgentOrchestratorState,
	herdrSessionName string,
) []observability.AgentOrchestratorState {
	kept := make([]observability.AgentOrchestratorState, 0, len(states))
	for _, state := range states {
		if state.HerdrSessionName == herdrSessionName {
			kept = append(kept, state)
		}
	}
	return kept
}

// agentQueryScopeOf turns an orchestrator scope into the query scope the TS
// `listAgents(scope)` call receives: both keys present, `all` absent.
func agentQueryScopeOf(scope observability.AgentScope) observability.AgentQueryScope {
	herdrSessionName := scope.HerdrSessionName
	workspaceID := scope.WorkspaceID
	return observability.AgentQueryScope{
		HerdrSessionName: &herdrSessionName,
		WorkspaceID:      &workspaceID,
	}
}

// nonNilAgents keeps an empty result an empty JSON array, like TS does.
func nonNilAgents(items []observability.AgentListItem) []observability.AgentListItem {
	if items == nil {
		return []observability.AgentListItem{}
	}
	return items
}

// nonNilEvents keeps an empty result an empty JSON array, like TS does.
func nonNilEvents(items []observability.AgentEventRecord) []observability.AgentEventRecord {
	if items == nil {
		return []observability.AgentEventRecord{}
	}
	return items
}

// nonNilMessages keeps an empty result an empty JSON array, like TS does.
func nonNilMessages(items []observability.AgentHistoryMessage) []observability.AgentHistoryMessage {
	if items == nil {
		return []observability.AgentHistoryMessage{}
	}
	return items
}

// truthy is the JS truthiness of an optional boolean.
func truthy(value *bool) bool { return value != nil && *value }

// stringPresent is the JS truthiness of an optional string.
func stringPresent(value *string) bool { return value != nil && *value != "" }

// containsString reports membership, mirroring `Array.prototype.includes`.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// boolPointer and intPointer address the optionals the service inputs take.
func boolPointer(value bool) *bool { return &value }

func intPointer(value int) *int { return &value }

func kindPointer(kind observability.SubscriberKind) *observability.SubscriberKind {
	return &kind
}

// stringAbsent is the JS falsiness of an optional string.
func stringAbsent(value *string) bool { return value == nil || *value == "" }

// derefString renders an optional string for a log payload: TS logs `null` for
// a nullable field it never checked.
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
