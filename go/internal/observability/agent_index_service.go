package observability

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/shared"
)

// This file ports src/observability/agent-index-service.ts.
//
// Concurrency note: the TS daemon is single-threaded on one DatabaseSync
// connection, so every map below is unguarded there. The Go daemon refreshes
// from more than one goroutine, so each map carries its own mutex and the
// per-session / per-agent operation tails are channels instead of promise
// chains. The AbortController-based waiter registry becomes a set of
// waiterControllers whose done channel stands in for the abort signal.

// ---------------------------------------------------------------------------
// Waiter and shutdown plumbing
// ---------------------------------------------------------------------------

// waiterController stands in for AbortController: its done channel closes on the
// first abort, exactly like the signal's "abort" event. It carries the agent it
// was created for, because #appendStatusEvents guards on
// `controller.signal.aborted || isPaneClosed(...)` for the *same* agent.
type waiterController struct {
	once  sync.Once
	done  chan struct{}
	agent AgentIndexRecord
}

func newWaiterController() *waiterController {
	return &waiterController{done: make(chan struct{})}
}

// newWaiterControllerFor builds a controller already bound to its agent.
func newWaiterControllerFor(agent AgentIndexRecord) *waiterController {
	return &waiterController{agent: agent, done: make(chan struct{})}
}

// Abort mirrors controller.abort(): idempotent, and wakes every waiter.
func (c *waiterController) Abort() {
	c.once.Do(func() { close(c.done) })
}

// Aborted mirrors controller.signal.aborted.
func (c *waiterController) Aborted() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// Done mirrors controller.signal as an awaitable channel.
func (c *waiterController) Done() <-chan struct{} { return c.done }

// ShutdownSignal stands in for the AbortSignal the daemon aborts on shutdown.
type ShutdownSignal struct {
	once sync.Once
	done chan struct{}
}

// NewShutdownSignal builds an un-aborted signal.
func NewShutdownSignal() *ShutdownSignal {
	return &ShutdownSignal{done: make(chan struct{})}
}

// Abort mirrors the abort event: idempotent.
func (s *ShutdownSignal) Abort() {
	s.once.Do(func() { close(s.done) })
}

// Aborted mirrors signal.aborted.
func (s *ShutdownSignal) Aborted() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Done mirrors the signal as an awaitable channel.
func (s *ShutdownSignal) Done() <-chan struct{} { return s.done }

// HerdrClientLike is the slice of *herdr.HerdrSocketClient the index service
// depends on, mirroring the TS `Pick<HerdrSocketClient, "close" |
// "sessionSnapshot">`.
type HerdrClientLike interface {
	Close()
	SessionSnapshot() (any, error)
}

// HerdrClientFactory mirrors the TS clientFactory option.
type HerdrClientFactory func(socketPath string) (HerdrClientLike, error)

// defaultHerdrClientFactory mirrors the default: a fresh socket client per
// refresh.
func defaultHerdrClientFactory(socketPath string) (HerdrClientLike, error) {
	return herdr.NewHerdrSocketClient(herdr.HerdrSocketClientOptions{SocketPath: socketPath})
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// planProgressTracker mirrors the TS tracker record.
type planProgressTracker struct {
	firstSeenAt             int64
	lastProgressFingerprint string
	stallAttempts           int
}

// planWindowTracker mirrors the TS tracker record.
type planWindowTracker struct {
	idleStartedAt int64
}

// RefreshSource is one awaitable refreshHerdrSession call: the Go form of the
// promise refreshHerdrSession hands back in TS. Concurrent callers that observe
// the same mutation epoch receive the same *RefreshSource, exactly as the TS
// in-flight registry hands back the same promise, so a caller that keys work on
// the identity of what it was handed — the watch manager's #refreshPublications
// publication dedup — sees the same sharing the TS has.
type RefreshSource struct {
	done   chan struct{}
	result *AgentIndexRefreshResult
	err    error
}

// Await blocks until the refresh settles and returns its outcome. Every awaiter
// of one source reads the same fields, closed over by the close of done.
func (r *RefreshSource) Await() (*AgentIndexRefreshResult, error) {
	<-r.done
	return r.result, r.err
}

// NewRefreshSource returns one unsettled source together with the function that
// settles it (idempotent, like a promise). The index service builds its own
// sources internally; this exists so a caller of RefreshHerdrSessionSource can
// hand the same source to concurrent refreshes, the way the TS fake index hands
// back the same promise.
func NewRefreshSource() (*RefreshSource, func(*AgentIndexRefreshResult, error)) {
	source := &RefreshSource{done: make(chan struct{})}
	var once sync.Once
	return source, func(result *AgentIndexRefreshResult, err error) {
		once.Do(func() {
			source.result, source.err = result, err
			close(source.done)
		})
	}
}

// refreshInFlight is one awaitable in-flight refresh, mirroring the promise the
// TS keeps in #refreshInFlightBySession.
type refreshInFlight struct {
	epoch  int
	source *RefreshSource
}

func (r *refreshInFlight) await() (*AgentIndexRefreshResult, error) {
	return r.source.Await()
}

// AgentIndexServiceOptions mirrors the TS constructor options.
type AgentIndexServiceOptions struct {
	ClearRetry           func(timer *time.Timer)
	ClientFactory        HerdrClientFactory
	Context              *AgentContextService
	History              AgentHistoryServiceLike
	MaxWorkingPlanWaitMs int64
	Now                  func() int64
	ScheduleRetry        func(callback func(), delayMs int64) *time.Timer
	// SessionPaths is the session-path whitelist the registration protocol
	// needs. internal/agenthistory imports internal/observability, so the
	// agent-history helpers cannot be called directly; any
	// AgentHistoryServiceLike satisfies this and is used as the default.
	SessionPaths      SessionPathGate
	ShutdownSignal    *ShutdownSignal
	Sleep             func(ms int64, signal <-chan struct{})
	StatusPlansConfig *StatusPlansConfig
	Stores            AgentIndexServiceStores
	TurnCompletions   *TurnCompletionRegistry
	OnAgentEvent      func(event AgentEventRecord)
}

// SessionPathGate is the slice of the agent-history module's session-path
// whitelist the index service depends on.
type SessionPathGate interface {
	SafeAllowedSessionPath(value string) string
	SafeOfficialSessionPath(value string) string
	SessionPathAllowedByShape(value string) bool
}

// AgentIndexService ports the TS class of the same name.
type AgentIndexService struct {
	clearRetry           func(timer *time.Timer)
	clientFactory        HerdrClientFactory
	context              *AgentContextService
	stores               AgentIndexServiceStores
	scheduleRetry        func(callback func(), delayMs int64) *time.Timer
	shutdown             *ShutdownSignal
	sleep                func(ms int64, signal <-chan struct{})
	turnCompletions      *TurnCompletionRegistry
	now                  func() int64
	maxWorkingPlanWaitMs int64
	statusPlansConfig    StatusPlansConfig
	onAgentEvent         func(event AgentEventRecord)
	sessionPaths         SessionPathGate

	mu sync.Mutex
	// activeWaiters mirrors #activeWaiters.
	activeWaiters map[string]map[*waiterController]struct{}
	// emptyTurnAlertPanes mirrors #emptyTurnAlertPanes.
	emptyTurnAlertPanes map[string]struct{}
	// guardWindowSaturatedAgents mirrors #guardWindowSaturatedAgents.
	guardWindowSaturatedAgents map[string]struct{}
	// salvageWindowSaturatedKeys mirrors #salvageWindowSaturatedKeys.
	salvageWindowSaturatedKeys map[string]struct{}
	// mutationEpochBySession mirrors #mutationEpochBySession.
	mutationEpochBySession map[string]int
	// pendingPiSessionRefs mirrors #pendingPiSessionRefs.
	pendingPiSessionRefs map[string]AgentSessionRef
	// planTailByAgent mirrors #planTailByAgent.
	planTailByAgent map[string]chan struct{}
	// refreshInFlightBySession mirrors #refreshInFlightBySession.
	refreshInFlightBySession map[string]*refreshInFlight
	// sessionOperationTail mirrors #sessionOperationTail.
	sessionOperationTail map[string]chan struct{}
	// waitingHistoryTimers mirrors #waitingHistoryTimers.
	waitingHistoryTimers map[int64]*time.Timer
	// planProgressTrackers mirrors #planProgressTrackers.
	planProgressTrackers map[int64]*planProgressTracker
	// planWindowTrackers mirrors #planWindowTrackers.
	planWindowTrackers map[int64]*planWindowTracker
}

// NewAgentIndexService mirrors the TS constructor.
func NewAgentIndexService(options AgentIndexServiceOptions) (*AgentIndexService, error) {
	service := &AgentIndexService{
		stores:                     options.Stores,
		turnCompletions:            options.TurnCompletions,
		shutdown:                   options.ShutdownSignal,
		now:                        options.Now,
		maxWorkingPlanWaitMs:       options.MaxWorkingPlanWaitMs,
		onAgentEvent:               options.OnAgentEvent,
		sessionPaths:               options.SessionPaths,
		clearRetry:                 options.ClearRetry,
		scheduleRetry:              options.ScheduleRetry,
		sleep:                      options.Sleep,
		clientFactory:              options.ClientFactory,
		context:                    options.Context,
		activeWaiters:              map[string]map[*waiterController]struct{}{},
		emptyTurnAlertPanes:        map[string]struct{}{},
		guardWindowSaturatedAgents: map[string]struct{}{},
		salvageWindowSaturatedKeys: map[string]struct{}{},
		mutationEpochBySession:     map[string]int{},
		pendingPiSessionRefs:       map[string]AgentSessionRef{},
		planTailByAgent:            map[string]chan struct{}{},
		refreshInFlightBySession:   map[string]*refreshInFlight{},
		sessionOperationTail:       map[string]chan struct{}{},
		waitingHistoryTimers:       map[int64]*time.Timer{},
		planProgressTrackers:       map[int64]*planProgressTracker{},
		planWindowTrackers:         map[int64]*planWindowTracker{},
	}
	if service.now == nil {
		service.now = func() int64 { return time.Now().UnixMilli() }
	}
	if service.maxWorkingPlanWaitMs == 0 {
		service.maxWorkingPlanWaitMs = WORKING_PLAN_MAX_WAIT_MS
	}
	if service.clearRetry == nil {
		service.clearRetry = func(timer *time.Timer) { timer.Stop() }
	}
	if service.scheduleRetry == nil {
		service.scheduleRetry = func(callback func(), delayMs int64) *time.Timer {
			return time.AfterFunc(time.Duration(delayMs)*time.Millisecond, callback)
		}
	}
	if service.sleep == nil {
		service.sleep = sleepFor
	}
	if service.clientFactory == nil {
		service.clientFactory = defaultHerdrClientFactory
	}
	if service.onAgentEvent == nil {
		service.onAgentEvent = func(AgentEventRecord) {}
	}
	config := StatusPlansConfig{}
	if options.StatusPlansConfig != nil {
		config = *options.StatusPlansConfig
	}
	service.statusPlansConfig = StatusPlansConfig{
		BackoffFactor:    config.BackoffFactor,
		InitialBackoffMs: config.InitialBackoffMs,
		MaxBackoffMs:     config.MaxBackoffMs,
		TotalWindowMs:    config.TotalWindowMs,
	}
	if service.sessionPaths == nil {
		service.sessionPaths = options.History
	}
	if service.context == nil {
		if options.Stores.AgentContextSnapshots == nil {
			return nil, fmt.Errorf(
				"AgentIndexService requires context or agentContextSnapshots store")
		}
		if options.History == nil {
			return nil, fmt.Errorf(
				"AgentIndexService requires history when context is not supplied")
		}
		service.context = NewAgentContextService(struct {
			History AgentHistoryServiceLike
			Stores  struct {
				AgentContextSnapshots AgentContextSnapshotStoreLike
				Agents                AgentContextAgentStoreLike
			}
		}{
			History: options.History,
			Stores: struct {
				AgentContextSnapshots AgentContextSnapshotStoreLike
				Agents                AgentContextAgentStoreLike
			}{
				AgentContextSnapshots: options.Stores.AgentContextSnapshots,
				Agents:                options.Stores.Agents,
			},
		})
	}
	if service.shutdown != nil {
		go func() {
			<-service.shutdown.Done()
			service.AbortPendingWaits()
		}()
	}
	return service, nil
}

// RefreshHerdrSessionFast mirrors refreshHerdrSessionFast.
func (s *AgentIndexService) RefreshHerdrSessionFast(
	input RefreshInput,
) (*AgentIndexRefreshFastResult, error) {
	s.incrementMutationEpoch(input.HerdrSessionName)
	return s.enqueueSessionOperation(input.HerdrSessionName, func() (*AgentIndexRefreshFastResult, error) {
		return s.refreshHerdrSessionNow(input)
	})
}

// RefreshHerdrSession mirrors refreshHerdrSession.
func (s *AgentIndexService) RefreshHerdrSession(
	input RefreshInput,
) (*AgentIndexRefreshResult, error) {
	return s.RefreshHerdrSessionSource(input).Await()
}

// RefreshHerdrSessionSource is refreshHerdrSession's in-flight registry seen
// from the outside: it returns the source already running for the session's
// current mutation epoch, or starts one and returns it. The TS hands every such
// caller the same promise and its watch manager keys its publication dedup on
// that promise identity, so the sharing has to be observable here instead of
// only inside RefreshHerdrSession.
//
// The epoch read, the membership check and the install happen inside one lock
// hold. Split into two, two concurrent callers of the same epoch would both find
// no in-flight refresh and each start its own, handing back two different
// *RefreshSource values for work the TS side would have shared in one promise.
func (s *AgentIndexService) RefreshHerdrSessionSource(
	input RefreshInput,
) *RefreshSource {
	s.mu.Lock()
	epoch := s.mutationEpochBySession[input.HerdrSessionName]
	existing := s.refreshInFlightBySession[input.HerdrSessionName]
	if existing != nil && existing.epoch == epoch {
		source := existing.source
		s.mu.Unlock()
		return source
	}
	source := &RefreshSource{done: make(chan struct{})}
	inFlight := &refreshInFlight{epoch: epoch, source: source}
	s.refreshInFlightBySession[input.HerdrSessionName] = inFlight
	s.mu.Unlock()

	go func() {
		defer close(source.done)
		intermediate, err := s.enqueueSessionOperation(
			input.HerdrSessionName,
			func() (*AgentIndexRefreshFastResult, error) {
				return s.refreshHerdrSessionNow(input)
			},
		)
		if err != nil {
			source.err = err
			s.clearRefreshInFlight(input.HerdrSessionName, inFlight)
			return
		}
		events, err := s.runStatusEventPlans(intermediate.StatusEventPlans)
		if err != nil {
			source.err = err
			s.clearRefreshInFlight(input.HerdrSessionName, inFlight)
			return
		}
		source.result = &AgentIndexRefreshResult{
			Agents:               intermediate.Agents,
			ContextChangedScopes: intermediate.ContextChangedScopes,
			Events:               append(append([]AgentEventRecord{}, intermediate.Events...), events...),
		}
		s.clearRefreshInFlight(input.HerdrSessionName, inFlight)
	}()
	return source
}

func (s *AgentIndexService) clearRefreshInFlight(sessionName string, inFlight *refreshInFlight) {
	s.mu.Lock()
	if s.refreshInFlightBySession[sessionName] == inFlight {
		delete(s.refreshInFlightBySession, sessionName)
	}
	s.mu.Unlock()
}

// HandleHerdrEventFast mirrors handleHerdrEventFast.
func (s *AgentIndexService) HandleHerdrEventFast(input HandleHerdrEventInput) (
	*AgentEventHandlingFastResult, error,
) {
	s.incrementMutationEpoch(input.HerdrSessionName)
	return s.enqueueSessionEventOperation(input.HerdrSessionName, func() (*AgentEventHandlingFastResult, error) {
		return s.handleHerdrEventNow(input)
	})
}

// HandleHerdrEventInput mirrors the TS handleHerdrEvent input.
type HandleHerdrEventInput struct {
	Event            any
	HerdrSessionName string
	SessionDir       string
	SocketPath       string
}

// HandleHerdrEvent mirrors handleHerdrEvent.
func (s *AgentIndexService) HandleHerdrEvent(
	input HandleHerdrEventInput,
) (*AgentEventHandlingResult, error) {
	intermediate, err := s.HandleHerdrEventFast(input)
	if err != nil {
		return nil, err
	}
	events, err := s.runStatusEventPlans(intermediate.StatusEventPlans)
	if err != nil {
		return nil, err
	}
	return &AgentEventHandlingResult{
		ContextChangedScopes: intermediate.ContextChangedScopes,
		Events:               append(append([]AgentEventRecord{}, intermediate.Events...), events...),
	}, nil
}

// RegisterPiSessionRefInput mirrors the TS registerPiSessionRef input.
type RegisterPiSessionRefInput struct {
	HerdrSessionName string
	SessionRef       AgentSessionRef
	TerminalID       string
}

// RegisterPiSessionRef mirrors registerPiSessionRef.
func (s *AgentIndexService) RegisterPiSessionRef(
	input RegisterPiSessionRefInput,
) (*PiSessionRefRegistrationResult, error) {
	s.incrementMutationEpoch(input.HerdrSessionName)
	return s.enqueueSessionRegistrationOperation(input.HerdrSessionName, func() (*PiSessionRefRegistrationResult, error) {
		return s.registerPiSessionRefNow(input)
	})
}

// StopWaitingHistoryRetries mirrors stopWaitingHistoryRetries.
func (s *AgentIndexService) StopWaitingHistoryRetries() {
	s.mu.Lock()
	timers := make([]*time.Timer, 0, len(s.waitingHistoryTimers))
	for _, timer := range s.waitingHistoryTimers {
		timers = append(timers, timer)
	}
	s.waitingHistoryTimers = map[int64]*time.Timer{}
	s.planProgressTrackers = map[int64]*planProgressTracker{}
	s.planWindowTrackers = map[int64]*planWindowTracker{}
	s.mu.Unlock()
	for _, timer := range timers {
		s.clearRetry(timer)
	}
}

// HasPlanProgressTracker mirrors hasPlanProgressTracker.
func (s *AgentIndexService) HasPlanProgressTracker(planID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.planProgressTrackers[planID]
	return ok
}

// AbortPendingWaits mirrors abortPendingWaits.
func (s *AgentIndexService) AbortPendingWaits() {
	s.mu.Lock()
	waiters := s.activeWaiters
	s.activeWaiters = map[string]map[*waiterController]struct{}{}
	s.mu.Unlock()
	for _, controllers := range waiters {
		for controller := range controllers {
			controller.Abort()
		}
	}
}

// DrainInFlightPlans mirrors drainInFlightPlans.
func (s *AgentIndexService) DrainInFlightPlans() {
	s.mu.Lock()
	tails := make([]chan struct{}, 0, len(s.planTailByAgent))
	for _, tail := range s.planTailByAgent {
		tails = append(tails, tail)
	}
	s.mu.Unlock()
	for _, tail := range tails {
		<-tail
	}
}

// ---------------------------------------------------------------------------
// Session-scoped refresh and event handling
// ---------------------------------------------------------------------------

func (s *AgentIndexService) refreshHerdrSessionNow(
	input RefreshInput,
) (*AgentIndexRefreshFastResult, error) {
	client, err := s.clientFactory(input.SocketPath)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	previous, err := s.stores.Agents.ListForHerdrSession(input.HerdrSessionName)
	if err != nil {
		return nil, err
	}
	previousByPane := map[string]AgentIndexRecord{}
	for _, agent := range previous {
		previousByPane[paneIdentityKey(agent.PaneID, agent.PaneGeneration)] = agent
	}
	previousByTerminal := map[string]AgentIndexRecord{}
	for _, agent := range previous {
		if agent.TerminalID != nil {
			previousByTerminal[*agent.TerminalID] = agent
		}
	}
	s.mu.Lock()
	refreshEpoch := s.mutationEpochBySession[input.HerdrSessionName]
	s.mu.Unlock()
	rawSnapshot, err := client.SessionSnapshot()
	if err != nil {
		return nil, err
	}
	snapshot := herdr.NormalizeHerdrSessionSnapshot(rawSnapshot)
	s.mu.Lock()
	mutationEpoch := s.mutationEpochBySession[input.HerdrSessionName]
	s.mu.Unlock()
	if mutationEpoch != refreshEpoch {
		fmt.Fprintf(os.Stderr,
			"Herdsman discarding stale Herdr session refresh"+
				" (sessionName=%s, refreshEpoch=%d, mutationEpoch=%d)\n",
			input.HerdrSessionName, refreshEpoch, mutationEpoch)
		return &AgentIndexRefreshFastResult{
			Agents:               previous,
			ContextChangedScopes: []AgentScope{},
			Events:               []AgentEventRecord{},
			StatusEventPlans:     []StatusEventPlan{},
		}, nil
	}
	overlayByPane := map[string]paneOverlay{}
	for _, pane := range snapshot.Panes {
		value := record(pane)
		// `pane.list` panes are official `PaneInfo` records: snake_case only.
		paneID := stringValue(value["pane_id"])
		if paneID == nil {
			continue
		}
		revision := integerValue(value["revision"])
		terminalTitle := stringValue(value["terminal_title"])
		if revision == nil && terminalTitle == nil {
			continue
		}
		overlay := paneOverlay{}
		if revision != nil {
			overlay.Revision = revision
		}
		if terminalTitle != nil {
			overlay.TerminalTitle = terminalTitle
		}
		overlayByPane[*paneID] = overlay
	}
	snapshotAgents := make([]map[string]any, 0, len(snapshot.Agents))
	for _, agent := range snapshot.Agents {
		snapshotAgents = append(snapshotAgents, withPaneRevision(agent, overlayByPane))
	}
	// A live pane that reports a pane_generation overrides any generation-less
	// (legacy) close: drop the legacy tombstone before consulting isPaneClosed so
	// the generation-carrying agent is indexed instead of being swallowed by the
	// phantom close. Generation-scoped tombstones stay.
	for _, agent := range snapshotAgents {
		paneID := stringValue(agent["pane_id"])
		if paneID != nil && paneGenerationOf(agent) != nil {
			if err := s.stores.Agents.ClearLegacyPaneTombstone(ClearLegacyPaneTombstoneInput{
				HerdrSessionName: input.HerdrSessionName,
				PaneID:           *paneID,
			}); err != nil {
				return nil, err
			}
		}
	}
	liveSnapshotAgents := make([]map[string]any, 0, len(snapshotAgents))
	for _, agent := range snapshotAgents {
		if !s.isClosedPaneAgent(input.HerdrSessionName, agent) {
			liveSnapshotAgents = append(liveSnapshotAgents, agent)
		}
	}
	workspaces := make([]map[string]any, 0, len(snapshot.Workspaces))
	for _, workspace := range snapshot.Workspaces {
		typed, ok := workspace.(map[string]any)
		if !ok {
			continue
		}
		workspaces = append(workspaces, typed)
	}
	if _, err := s.stores.HerdrSessions.UpsertRunning(UpsertRunningHerdrSessionInput{
		Name:       input.HerdrSessionName,
		SessionDir: input.SessionDir,
		SocketPath: input.SocketPath,
	}); err != nil {
		return nil, err
	}
	if _, err := s.stores.HerdrWorkspaces.ReplaceForSession(ReplaceHerdrWorkspacesInput{
		HerdrSessionName: input.HerdrSessionName,
		Workspaces:       workspaces,
	}); err != nil {
		return nil, err
	}
	indexedAgents, err := s.stores.Agents.ReplaceForSession(ReplaceAgentsForSessionInput{
		Agents:           liveSnapshotAgents,
		HerdrSessionName: input.HerdrSessionName,
	})
	if err != nil {
		return nil, err
	}
	agents := make([]AgentIndexRecord, 0, len(indexedAgents))
	for _, agent := range indexedAgents {
		if agent.TerminalID == nil {
			agents = append(agents, agent)
			continue
		}
		key := terminalSessionKey(input.HerdrSessionName, *agent.TerminalID)
		s.mu.Lock()
		pending, ok := s.pendingPiSessionRefs[key]
		if ok {
			delete(s.pendingPiSessionRefs, key)
		}
		s.mu.Unlock()
		if !ok {
			agents = append(agents, agent)
			continue
		}
		updated, err := s.stores.Agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
			AgentSession:     pending,
			HerdrSessionName: input.HerdrSessionName,
			TerminalID:       *agent.TerminalID,
		})
		if err != nil {
			return nil, err
		}
		if updated == nil {
			agents = append(agents, agent)
			continue
		}
		agents = append(agents, *updated)
	}
	scopes := map[string]AgentScope{}
	events := []AgentEventRecord{}
	statusEventPlans := []StatusEventPlan{}
	currentIDs := map[string]struct{}{}
	for _, agent := range agents {
		currentIDs[agent.ID] = struct{}{}
	}
	for _, prior := range previous {
		if _, ok := currentIDs[prior.ID]; !ok {
			addScope(scopes, scopeOf(prior))
		}
	}
	occupiedSessionPaths := map[string]struct{}{}
	for _, candidate := range agents {
		if candidate.AgentSession != nil && candidate.AgentSession.Kind == SessionRefPath {
			occupiedSessionPaths[candidate.AgentSession.Value] = struct{}{}
		}
	}
	for _, agent := range agents {
		prior := matchingPrior(agent, previousByTerminal, previousByPane)
		identityChanged := prior == nil || !sameIdentity(*prior, agent)
		metadataChanged := prior == nil || !sameContextMetadata(*prior, agent)
		cached, err := s.context.GetAgentSnapshot(agent.ID)
		if err != nil {
			return nil, err
		}
		occupiedByOthers, err := s.context.OccupiedSessionPathsFor(agent)
		if err != nil {
			return nil, err
		}
		occupancyConflict := false
		if cached != nil && cached.HistoryRef != nil && cached.HistoryRef.Path != nil {
			_, conflict := occupiedByOthers[*cached.HistoryRef.Path]
			occupancyConflict = conflict
		}
		sessionReady := agent.AgentSession != nil &&
			agent.AgentSession.Kind == SessionRefPath &&
			s.safeOfficialSessionPath(agent.AgentSession.Value) != ""
		sessionUnbound := false
		if sessionReady && agent.AgentSession != nil {
			sessionUnbound = cached == nil || cached.HistoryRef == nil ||
				cached.HistoryRef.Kind != HistoryRefAgentSession ||
				cached.HistoryRef.Path == nil ||
				*cached.HistoryRef.Path != agent.AgentSession.Value
		}
		isStalled := (agent.AgentStatus == AgentStatusIdle || agent.AgentStatus == AgentStatusDone) &&
			s.isAgentStalled(agent.ID, input.HerdrSessionName)
		dirty := cached == nil ||
			agent.PaneRevision == nil ||
			cached.PaneRevision == nil ||
			*cached.PaneRevision != *agent.PaneRevision ||
			identityChanged ||
			cached.HistoryRef == nil ||
			cached.HistoryRef.Kind == HistoryRefDiscoveredFile ||
			occupancyConflict ||
			sessionUnbound ||
			isStalled
		refreshed := cached
		if dirty {
			occupiedForCurrent := make([]string, 0, len(occupiedSessionPaths))
			for path := range occupiedSessionPaths {
				occupiedForCurrent = append(occupiedForCurrent, path)
			}
			if agent.AgentSession != nil && agent.AgentSession.Kind == SessionRefPath {
				occupiedForCurrent = removeString(occupiedForCurrent, agent.AgentSession.Value)
			}
			var forceRefresh *bool
			if isStalled {
				forceRefresh = boolPointer(true)
			}
			result, err := s.context.RefreshAgent(RefreshAgentContextInput{
				Agent:                agent,
				ForceRefresh:         forceRefresh,
				IdentityChanged:      identityChanged,
				OccupiedSessionPaths: occupiedForCurrent,
			})
			if err != nil {
				return nil, err
			}
			refreshed = &result.Snapshot
			if result.Changed {
				addScope(scopes, scopeOf(agent))
			}
		}
		var compactHistory *CompactAgentHistory
		if refreshed != nil {
			compactHistory = &refreshed.CompactHistory
		}
		lateEvent := s.reconcileLateCompletion(agent, compactHistory)
		if lateEvent != nil {
			events = append(events, *lateEvent)
			addScope(scopes, scopeOf(agent))
		}
		if metadataChanged {
			if prior != nil && !sameScope(scopeOf(*prior), scopeOf(agent)) {
				addScope(scopes, scopeOf(*prior))
			}
			addScope(scopes, scopeOf(agent))
		}
		if prior != nil && prior.AgentStatus != agent.AgentStatus {
			statusEventPlans = append(statusEventPlans, StatusEventPlan{
				Agent:          agent,
				CompactHistory: compactHistory,
				From:           prior.AgentStatus,
				To:             agent.AgentStatus,
			})
			addScope(scopes, scopeOf(agent))
		}
	}
	return &AgentIndexRefreshFastResult{
		Agents:               agents,
		ContextChangedScopes: sortedScopes(scopes),
		Events:               events,
		StatusEventPlans:     statusEventPlans,
	}, nil
}

func (s *AgentIndexService) handleHerdrEventNow(
	input HandleHerdrEventInput,
) (*AgentEventHandlingFastResult, error) {
	empty := func() *AgentEventHandlingFastResult {
		return &AgentEventHandlingFastResult{
			ContextChangedScopes: []AgentScope{},
			Events:               []AgentEventRecord{},
			StatusEventPlans:     []StatusEventPlan{},
		}
	}
	event := record(input.Event)
	paneID := stringValue(event["pane_id"])
	if paneID == nil {
		paneID = stringValue(event["paneId"])
	}
	if paneID == nil {
		return empty(), nil
	}
	eventType, _ := event["type"].(string)
	if eventType == "pane.closed" {
		return s.handlePaneClosed(input, *paneID, event)
	}
	if eventType != "pane.agent_status_changed" {
		return empty(), nil
	}
	eventGeneration := paneGenerationFromEvent(event)
	agent, err := s.stores.Agents.FindByPane(FindAgentByPaneInput{
		HerdrSessionName: input.HerdrSessionName,
		PaneID:           *paneID,
		PaneGeneration:   eventGeneration,
	})
	if err != nil {
		return nil, err
	}
	var recovered *AgentIndexRefreshFastResult
	if agent == nil {
		if eventGeneration != nil {
			// A generation-scoped tombstone still closes its own generation.
			tombstoned, err := s.stores.Agents.HasGenerationScopedTombstone(
				HasGenerationScopedTombstoneInput{
					HerdrSessionName: input.HerdrSessionName,
					PaneID:           *paneID,
					PaneGeneration:   *eventGeneration,
				})
			if err != nil {
				return nil, err
			}
			if tombstoned {
				return empty(), nil
			}
			// No generation-scoped close: a live generation-carrying status
			// overrides a generation-less close, so recover the pane (the refresh
			// below clears the legacy tombstone only when the agent is actually
			// present in the snapshot).
		} else {
			closed, err := s.stores.Agents.IsPaneClosed(IsPaneClosedInput{
				HerdrSessionName: input.HerdrSessionName,
				PaneID:           *paneID,
				PaneGeneration:   nil,
			})
			if err != nil {
				return nil, err
			}
			if closed {
				return empty(), nil
			}
		}
		recovered, err = s.refreshHerdrSessionNow(RefreshInput{
			HerdrSessionName: input.HerdrSessionName,
			SessionDir:       input.SessionDir,
			SocketPath:       input.SocketPath,
		})
		if err != nil {
			return nil, err
		}
		agent, err = s.stores.Agents.FindByPane(FindAgentByPaneInput{
			HerdrSessionName: input.HerdrSessionName,
			PaneID:           *paneID,
			PaneGeneration:   eventGeneration,
		})
		if err != nil {
			return nil, err
		}
	}
	if agent == nil {
		if recovered != nil {
			return &AgentEventHandlingFastResult{
				ContextChangedScopes: recovered.ContextChangedScopes,
				Events:               recovered.Events,
				StatusEventPlans:     recovered.StatusEventPlans,
			}, nil
		}
		return empty(), nil
	}
	from := agent.AgentStatus
	if recovered != nil {
		from = AgentStatusUnknown
	}
	to := ParseAgentStatus(eventStatusValue(event["agent_status"]))
	herdrEventKey := herdrInputIdempotencyKey(input.HerdrSessionName, *paneID, event)
	current := *agent
	current.AgentStatus = to
	refreshed, err := s.context.RefreshAgent(RefreshAgentContextInput{
		Agent:           current,
		IdentityChanged: false,
	})
	if err != nil {
		return nil, err
	}
	scopes := map[string]AgentScope{}
	if recovered != nil {
		for _, scope := range recovered.ContextChangedScopes {
			addScope(scopes, scope)
		}
	}
	if refreshed.Changed || from != to {
		addScope(scopes, scopeOf(current))
	}
	events := []AgentEventRecord{}
	if recovered != nil {
		events = append(events, recovered.Events...)
	}
	lateEvent := s.reconcileLateCompletion(current, &refreshed.Snapshot.CompactHistory)
	if lateEvent != nil {
		events = append(events, *lateEvent)
	}
	statusEventPlans := []StatusEventPlan{}
	if recovered != nil {
		statusEventPlans = append(statusEventPlans, recovered.StatusEventPlans...)
	}
	equivalent := false
	for _, candidate := range statusEventPlans {
		if candidate.Agent.ID == current.ID && candidate.To == to {
			equivalent = true
			break
		}
	}
	statusInput := UpdateAgentStatusInput{
		AgentStatus:      to,
		HerdrSessionName: input.HerdrSessionName,
		PaneGeneration:   eventGeneration,
		PaneID:           *paneID,
	}
	if from == to {
		if _, err := s.stores.Agents.UpdateStatus(statusInput); err != nil {
			return nil, err
		}
	} else if !equivalent {
		var plan StatusEventPlanRecord
		var updated AgentIndexRecord
		transactionErr := runIndexTransaction(s.stores.Sqlite, func() error {
			updatedRecord, updateErr := s.stores.Agents.UpdateStatus(statusInput)
			if updateErr != nil {
				return updateErr
			}
			if updatedRecord != nil {
				updated = *updatedRecord
			} else {
				updated = current
			}
			inserted, insertErr := s.stores.StatusEventPlans.InsertPending(InsertPendingInput{
				Agent: &StatusEventPlanAgent{
					HerdrSessionName: updated.HerdrSessionName,
					ID:               updated.ID,
					PaneGeneration:   updated.PaneGeneration,
					PaneID:           updated.PaneID,
				},
				From:          from,
				HerdrEventKey: herdrEventKey,
				To:            to,
			})
			if insertErr != nil {
				return insertErr
			}
			plan = inserted
			return nil
		})
		if transactionErr != nil {
			return nil, transactionErr
		}
		statusEventPlans = append(statusEventPlans, StatusEventPlan{
			Agent:          updated,
			CompactHistory: &refreshed.Snapshot.CompactHistory,
			From:           from,
			HerdrEventKey:  herdrEventKey,
			PlanID:         &plan.ID,
			To:             to,
		})
	} else {
		if _, err := s.stores.Agents.UpdateStatus(statusInput); err != nil {
			return nil, err
		}
	}
	return &AgentEventHandlingFastResult{
		ContextChangedScopes: sortedScopes(scopes),
		Events:               events,
		StatusEventPlans:     statusEventPlans,
	}, nil
}

func (s *AgentIndexService) handlePaneClosed(
	input HandleHerdrEventInput,
	paneID string,
	event map[string]any,
) (*AgentEventHandlingFastResult, error) {
	closedGeneration := paneGenerationFromEvent(event)
	closedLateEvents := []AgentEventRecord{}
	closingAgent, err := s.stores.Agents.FindByPane(FindAgentByPaneInput{
		HerdrSessionName: input.HerdrSessionName,
		PaneID:           paneID,
		PaneGeneration:   closedGeneration,
	})
	if err != nil {
		return nil, err
	}
	if closingAgent != nil && s.isAgentStalled(closingAgent.ID, input.HerdrSessionName) {
		refreshed, refreshErr := s.context.RefreshAgent(RefreshAgentContextInput{
			Agent:           *closingAgent,
			ForceRefresh:    boolPointer(true),
			IdentityChanged: false,
		})
		if refreshErr != nil {
			fmt.Fprintf(os.Stderr,
				"Herdsman late completion rescue on pane.closed failed"+
					" (agentId=%s, herdrSessionName=%s, paneId=%s, error=%v)\n",
				closingAgent.ID, input.HerdrSessionName, paneID, refreshErr)
		} else {
			lateEvent := s.reconcileLateCompletion(
				*closingAgent, &refreshed.Snapshot.CompactHistory)
			if lateEvent != nil {
				closedLateEvents = append(closedLateEvents, *lateEvent)
			}
		}
	}
	s.abortPendingWaiters(abortPendingWaitersInput{
		herdrSessionName: input.HerdrSessionName,
		paneID:           paneID,
		paneGeneration:   closedGeneration,
	})
	// 显式关 Tab = 隐含消费：已经投递给编排者的行在这里直接 ack（不再走关页保留、
	// 不再被 daemon 重投），只把「从未投递过」的行留给保留规则。崩溃/掉线路径
	// （reconciler / reclaimDelivered）不经过这里，保留语义不变。
	invalidatedReason := "LEGACY_CLOSE_WITHOUT_GENERATION"
	if closedGeneration != nil {
		invalidatedReason = "PANE_CLOSED"
	}
	if err := s.stores.AgentEvents.InvalidatePane(InvalidatePaneInput{
		AcknowledgeDelivered: boolPointer(true),
		HerdrSessionName:     input.HerdrSessionName,
		PaneID:               paneID,
		PaneGeneration:       closedGeneration,
		InvalidatedReason:    &invalidatedReason,
	}); err != nil {
		return nil, err
	}
	if closedGeneration == nil {
		fmt.Fprintf(os.Stderr,
			"LEGACY_CLOSE_WITHOUT_GENERATION (sessionName=%s, paneId=%s)\n",
			input.HerdrSessionName, paneID)
	}
	retired, err := s.stores.Agents.RetirePane(RetirePaneInput{
		HerdrSessionName: input.HerdrSessionName,
		PaneID:           paneID,
		PaneGeneration:   closedGeneration,
	})
	if err != nil {
		return nil, err
	}
	scopes := map[string]AgentScope{}
	closedTerminalID := stringValue(event["terminal_id"])
	if closedTerminalID == nil {
		closedTerminalID = stringValue(event["terminalId"])
	}
	for _, agent := range retired {
		scope := scopeOf(agent)
		terminalIDToRelease := ""
		if closedTerminalID != nil {
			terminalIDToRelease = *closedTerminalID
		} else if agent.TerminalID != nil {
			terminalIDToRelease = *agent.TerminalID
		}
		if terminalIDToRelease != "" {
			change, releaseErr := s.stores.AgentOrchestratorScopes.ReleaseIfOwnerIdentity(
				ReleaseIfOwnerIdentityInput{
					AgentOrchestratorScopeKey: scope,
					PaneID:                    paneID,
					TerminalID:                terminalIDToRelease,
				})
			if releaseErr != nil {
				return nil, releaseErr
			}
			if change.Changed {
				addScope(scopes, scope)
			}
		}
		addScope(scopes, scope)
	}
	return &AgentEventHandlingFastResult{
		ContextChangedScopes: sortedScopes(scopes),
		Events:               closedLateEvents,
		StatusEventPlans:     []StatusEventPlan{},
	}, nil
}

func (s *AgentIndexService) registerPiSessionRefNow(
	input RegisterPiSessionRefInput,
) (*PiSessionRefRegistrationResult, error) {
	key := terminalSessionKey(input.HerdrSessionName, input.TerminalID)
	if input.SessionRef.Kind == SessionRefPath && s.safeAllowedSessionPath(input.SessionRef.Value) == "" {
		if fileExists(input.SessionRef.Value) || !s.sessionPathAllowedByShape(input.SessionRef.Value) {
			return &PiSessionRefRegistrationResult{ContextChangedScopes: []AgentScope{}}, nil
		}
	}
	previous, err := s.stores.Agents.FindByTerminal(FindAgentByTerminalInput{
		HerdrSessionName: input.HerdrSessionName,
		TerminalID:       input.TerminalID,
	})
	if err != nil {
		return nil, err
	}
	if previous == nil {
		s.mu.Lock()
		s.pendingPiSessionRefs[key] = input.SessionRef
		s.mu.Unlock()
		return &PiSessionRefRegistrationResult{ContextChangedScopes: []AgentScope{}}, nil
	}
	agent, err := s.stores.Agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
		AgentSession:     input.SessionRef,
		HerdrSessionName: input.HerdrSessionName,
		TerminalID:       input.TerminalID,
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.pendingPiSessionRefs, key)
	s.mu.Unlock()
	if agent == nil || sameAgentSession(previous.AgentSession, agent.AgentSession) {
		result := &PiSessionRefRegistrationResult{ContextChangedScopes: []AgentScope{}}
		if agent != nil {
			result.Agent = agent
		}
		return result, nil
	}
	refreshed, err := s.context.RefreshAgent(RefreshAgentContextInput{
		Agent:           *agent,
		IdentityChanged: true,
	})
	if err != nil {
		return nil, err
	}
	result := &PiSessionRefRegistrationResult{Agent: agent}
	if refreshed.Changed {
		result.ContextChangedScopes = []AgentScope{scopeOf(*agent)}
	} else {
		result.ContextChangedScopes = []AgentScope{}
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Session-path gate
// ---------------------------------------------------------------------------

func (s *AgentIndexService) safeOfficialSessionPath(value string) string {
	if s.sessionPaths == nil {
		return ""
	}
	return s.sessionPaths.SafeOfficialSessionPath(value)
}

func (s *AgentIndexService) safeAllowedSessionPath(value string) string {
	if s.sessionPaths == nil {
		return ""
	}
	return s.sessionPaths.SafeAllowedSessionPath(value)
}

func (s *AgentIndexService) sessionPathAllowedByShape(value string) bool {
	if s.sessionPaths == nil {
		return false
	}
	return s.sessionPaths.SessionPathAllowedByShape(value)
}

// ---------------------------------------------------------------------------
// Waiter registry and mutation epochs
// ---------------------------------------------------------------------------

type abortPendingWaitersInput struct {
	herdrSessionName string
	paneID           string
	paneGeneration   *string
}

func (s *AgentIndexService) incrementMutationEpoch(sessionName string) {
	s.mu.Lock()
	s.mutationEpochBySession[sessionName] = s.mutationEpochBySession[sessionName] + 1
	s.mu.Unlock()
}

// enqueueSessionOperation mirrors #enqueueSessionOperation: operations for one
// session serialize, and a failed prior operation does not block the next.
func enqueueSessionOperation[T any](
	s *AgentIndexService,
	sessionName string,
	operation func() (T, error),
) (T, error) {
	var zero T
	s.mu.Lock()
	previous := s.sessionOperationTail[sessionName]
	tail := make(chan struct{})
	s.sessionOperationTail[sessionName] = tail
	s.mu.Unlock()
	if previous != nil {
		<-previous
	}
	result, err := operation()
	close(tail)
	s.mu.Lock()
	if s.sessionOperationTail[sessionName] != nil {
		// Channel identity comparison mirrors the TS promise-identity check.
		if s.sessionOperationTail[sessionName] == tail {
			delete(s.sessionOperationTail, sessionName)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return zero, err
	}
	return result, nil
}

// enqueueSessionOperation is the unexported alias the methods above call, so the
// generic helper reads like its TS counterpart at the call sites.
func (s *AgentIndexService) enqueueSessionOperation(
	sessionName string,
	operation func() (*AgentIndexRefreshFastResult, error),
) (*AgentIndexRefreshFastResult, error) {
	return enqueueSessionOperation(s, sessionName, operation)
}

func (s *AgentIndexService) enqueueSessionEventOperation(
	sessionName string,
	operation func() (*AgentEventHandlingFastResult, error),
) (*AgentEventHandlingFastResult, error) {
	return enqueueSessionOperation(s, sessionName, operation)
}

func (s *AgentIndexService) enqueueSessionRegistrationOperation(
	sessionName string,
	operation func() (*PiSessionRefRegistrationResult, error),
) (*PiSessionRefRegistrationResult, error) {
	return enqueueSessionOperation(s, sessionName, operation)
}

func (s *AgentIndexService) isClosedPaneAgent(herdrSessionName string, agent map[string]any) bool {
	paneID := stringValue(agent["pane_id"])
	if paneID == nil {
		return false
	}
	closed, err := s.stores.Agents.IsPaneClosed(IsPaneClosedInput{
		HerdrSessionName: herdrSessionName,
		PaneID:           *paneID,
		PaneGeneration:   paneGenerationOf(agent),
	})
	if err != nil {
		return false
	}
	return closed
}

// throwIfShutdownAbort mirrors #throwIfShutdownAbort: a shutdown abort leaves the
// pane alive, so the plan must stay retryable.
func (s *AgentIndexService) throwIfShutdownAbort(controller *waiterController) error {
	if controller.Aborted() && s.shutdown != nil && s.shutdown.Aborted() {
		return &PlanWaitingHistoryError{}
	}
	return nil
}

// registerActiveWaiter mirrors #registerActiveWaiter.
func (s *AgentIndexService) registerActiveWaiter(
	agent AgentIndexRecord,
	controller *waiterController,
) func() {
	key := agent.HerdrSessionName + "\x00" + agent.PaneID + "\x00" +
		paneGenerationOrEmpty(agent.PaneGeneration)
	// A wait that starts after shutdown already aborted must not sleep at all:
	// abort it up front so the caller short-circuits like a closed pane.
	if s.shutdown != nil && s.shutdown.Aborted() {
		controller.Abort()
	}
	s.mu.Lock()
	set := s.activeWaiters[key]
	if set == nil {
		set = map[*waiterController]struct{}{}
		s.activeWaiters[key] = set
	}
	set[controller] = struct{}{}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if set, ok := s.activeWaiters[key]; ok {
			delete(set, controller)
			if len(set) == 0 {
				delete(s.activeWaiters, key)
			}
		}
		s.mu.Unlock()
	}
}

func (s *AgentIndexService) abortPendingWaiters(input abortPendingWaitersInput) {
	s.mu.Lock()
	matched := map[*waiterController]struct{}{}
	for key, controllers := range s.activeWaiters {
		sessionName, keyPaneID, keyPaneGen := splitWaiterKey(key)
		if sessionName != input.herdrSessionName || keyPaneID != input.paneID {
			continue
		}
		if input.paneGeneration == nil || keyPaneGen == "" ||
			*input.paneGeneration == keyPaneGen {
			for controller := range controllers {
				matched[controller] = struct{}{}
			}
			delete(s.activeWaiters, key)
		}
	}
	s.mu.Unlock()
	for controller := range matched {
		controller.Abort()
	}
}

func splitWaiterKey(key string) (sessionName string, paneID string, paneGeneration string) {
	parts := strings.SplitN(key, "\x00", 3)
	if len(parts) == 0 {
		return "", "", ""
	}
	sessionName = parts[0]
	if len(parts) > 1 {
		paneID = parts[1]
	}
	if len(parts) > 2 {
		paneGeneration = parts[2]
	}
	return sessionName, paneID, paneGeneration
}

// ---------------------------------------------------------------------------
// Status event plan fan-out
// ---------------------------------------------------------------------------

// runStatusEventPlans mirrors the `Promise.all(plans.map(executeStatusEventPlan))`
// fan-out the non-fast refresh/handle wrappers perform.
func (s *AgentIndexService) runStatusEventPlans(
	plans []StatusEventPlan,
) ([]AgentEventRecord, error) {
	if len(plans) == 0 {
		return []AgentEventRecord{}, nil
	}
	type outcome struct {
		event *AgentEventRecord
		err   error
	}
	outcomes := make([]outcome, len(plans))
	var waiter sync.WaitGroup
	for index, plan := range plans {
		waiter.Add(1)
		go func(index int, plan StatusEventPlan) {
			defer waiter.Done()
			event, err := s.ExecuteStatusEventPlan(plan)
			outcomes[index] = outcome{event: event, err: err}
		}(index, plan)
	}
	waiter.Wait()
	events := make([]AgentEventRecord, 0, len(plans))
	for _, result := range outcomes {
		if result.err != nil {
			return nil, result.err
		}
		if result.event != nil {
			events = append(events, *result.event)
		}
	}
	return events, nil
}

// ---------------------------------------------------------------------------
// Logging helpers
// ---------------------------------------------------------------------------

func (s *AgentIndexService) logWarn(message string, fields map[string]any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", message, formatLogFields(fields))
}

func (s *AgentIndexService) logInfo(message string, fields map[string]any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", message, formatLogFields(fields))
}

// formatLogFields renders a field map deterministically, so a log line does not
// depend on Go's randomised map iteration order.
func formatLogFields(fields map[string]any) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, fields[key]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// ---------------------------------------------------------------------------
// Module-level pure helpers
// ---------------------------------------------------------------------------

// paneOverlay mirrors the TS type of the same name.
type paneOverlay struct {
	Revision      *int
	TerminalTitle *string
}

func withPaneRevision(agent any, overlayByPane map[string]paneOverlay) map[string]any {
	raw := record(agent)
	// Herdr payloads are snake_case only (`AgentInfo` / `PaneInfo`); no camelCase
	// alias is accepted, so a Herdr rename surfaces as a missing field instead of
	// a silent compatibility shim.
	paneID := stringValue(raw["pane_id"])
	var overlay paneOverlay
	if paneID != nil {
		overlay = overlayByPane[*paneID]
	}
	revision := integerValue(raw["revision"])
	if revision == nil {
		revision = overlay.Revision
	}
	terminalTitle := stringValue(raw["terminal_title"])
	if terminalTitle == nil {
		terminalTitle = overlay.TerminalTitle
	}
	if revision == nil && terminalTitle == nil {
		return raw
	}
	result := map[string]any{}
	for key, value := range raw {
		result[key] = value
	}
	if revision != nil {
		// The snapshot payloads are JSON-shaped, so the overlaid number is written
		// back as float64: the store's integerValue only accepts the JSON number
		// type, and writing a Go int here made every overlaid pane revision read
		// as NULL (each refresh then looked dirty and re-parsed the history).
		result["revision"] = float64(*revision)
	}
	if terminalTitle != nil {
		result["terminal_title"] = *terminalTitle
	}
	return result
}

func paneGenerationOf(agent map[string]any) *string {
	if generation := stringValue(agent["pane_generation"]); generation != nil {
		return generation
	}
	return stringValue(agent["creation_id"])
}

func matchingPrior(
	agent AgentIndexRecord,
	previousByTerminal map[string]AgentIndexRecord,
	previousByPane map[string]AgentIndexRecord,
) *AgentIndexRecord {
	var terminalMatch *AgentIndexRecord
	if agent.TerminalID != nil {
		if match, ok := previousByTerminal[*agent.TerminalID]; ok {
			terminalMatch = &match
		}
	}
	generationMatches := agent.PaneGeneration == nil ||
		terminalMatch == nil ||
		terminalMatch.PaneGeneration == nil ||
		*terminalMatch.PaneGeneration == *agent.PaneGeneration
	paneMatch, paneFound := previousByPane[paneIdentityKey(agent.PaneID, agent.PaneGeneration)]
	canUsePaneFallback := paneFound && (agent.TerminalID == nil || paneMatch.TerminalID == nil)
	if generationMatches && terminalMatch != nil {
		return terminalMatch
	}
	if canUsePaneFallback {
		return &paneMatch
	}
	return nil
}

func sameIdentity(left AgentIndexRecord, right AgentIndexRecord) bool {
	return stringPointerEqual(left.Agent, right.Agent) &&
		stringPointerEqual(left.TerminalID, right.TerminalID) &&
		stringPointerEqual(left.Cwd, right.Cwd) &&
		stringPointerEqual(left.ForegroundCwd, right.ForegroundCwd) &&
		sameAgentSession(left.AgentSession, right.AgentSession)
}

func sameContextMetadata(left AgentIndexRecord, right AgentIndexRecord) bool {
	return sameIdentity(left, right) &&
		stringPointerEqual(left.Name, right.Name) &&
		left.AgentStatus == right.AgentStatus &&
		left.PaneID == right.PaneID &&
		stringPointerEqual(left.TabID, right.TabID) &&
		left.WorkspaceID == right.WorkspaceID
}

func sameAgentSession(left *AgentSessionRef, right *AgentSessionRef) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Agent == right.Agent &&
		left.Kind == right.Kind &&
		left.Source == right.Source &&
		left.Value == right.Value
}

func terminalSessionKey(herdrSessionName string, terminalID string) string {
	return herdrSessionName + "\x00" + terminalID
}

func scopeOf(agent AgentIndexRecord) AgentScope {
	return AgentScope{
		HerdrSessionName: agent.HerdrSessionName,
		WorkspaceID:      agent.WorkspaceID,
	}
}

func addScope(scopes map[string]AgentScope, scope AgentScope) {
	scopes[scope.HerdrSessionName+"\x00"+scope.WorkspaceID] = scope
}

func sameScope(left AgentScope, right AgentScope) bool {
	return left.HerdrSessionName == right.HerdrSessionName && left.WorkspaceID == right.WorkspaceID
}

func sortedScopes(scopes map[string]AgentScope) []AgentScope {
	sorted := make([]AgentScope, 0, len(scopes))
	for _, scope := range scopes {
		sorted = append(sorted, scope)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].HerdrSessionName != sorted[j].HerdrSessionName {
			return sorted[i].HerdrSessionName < sorted[j].HerdrSessionName
		}
		return sorted[i].WorkspaceID < sorted[j].WorkspaceID
	})
	return sorted
}

// statusEventType mirrors statusEventType.
func statusEventType(status AgentStatus) *AgentEventType {
	switch status {
	case AgentStatusBlocked:
		blocked := AgentEventBlocked
		return &blocked
	case AgentStatusDone:
		done := AgentEventDone
		return &done
	case AgentStatusIdle:
		idle := AgentEventIdle
		return &idle
	default:
		return nil
	}
}

// payload mirrors payload(): the shared status-event payload fields.
func payload(agent AgentIndexRecord, from AgentStatus, to AgentStatus) map[string]any {
	result := map[string]any{
		"agent":            agent.Agent,
		"from":             string(from),
		"name":             agent.Name,
		"herdrSessionName": agent.HerdrSessionName,
		"paneId":           agent.PaneID,
		"terminalId":       agent.TerminalID,
		"to":               string(to),
		"workspaceId":      agent.WorkspaceID,
	}
	if IsInteractivePiAgent(&agent) {
		result["interactive"] = true
	}
	return result
}

// idempotencyKey mirrors idempotencyKey.
func idempotencyKey(
	eventType AgentEventType,
	agent AgentIndexRecord,
	from AgentStatus,
	to AgentStatus,
	observationID string,
) string {
	return string(eventType) + ":" + agent.HerdrSessionName + ":" + agent.PaneID + ":" +
		string(from) + ":" + string(to) + ":" + observationID
}

// herdrInputIdempotencyKey mirrors herdrInputIdempotencyKey.
func herdrInputIdempotencyKey(
	sessionName string,
	paneID string,
	event map[string]any,
) *string {
	eventID := stringValue(event["id"])
	if eventID == nil {
		eventID = stringValue(event["event_id"])
	}
	if eventID == nil {
		eventID = stringValue(event["eventId"])
	}
	if eventID == nil {
		return nil
	}
	eventType, _ := event["type"].(string)
	key := sessionName + ":" + paneID + ":" + eventType + ":" + *eventID
	return &key
}

// ---------------------------------------------------------------------------
// Small shared utilities
// ---------------------------------------------------------------------------

func boolPointer(value bool) *bool {
	return &value
}

func stringPointerEqual(left *string, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func paneGenerationOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func eventStatusValue(value any) string {
	status, _ := value.(string)
	return status
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// sleepFor mirrors the module-level sleep: it returns as soon as the signal
// fires, and never later than ms.
func sleepFor(ms int64, signal <-chan struct{}) {
	if signal != nil {
		select {
		case <-signal:
			return
		default:
		}
	}
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	if signal == nil {
		<-timer.C
		return
	}
	select {
	case <-timer.C:
	case <-signal:
	}
}

// runIndexTransaction mirrors runSqliteTransaction for the two call sites that
// need a value out of the transaction: the closure writes into captured
// variables, exactly like the TS callback returning a value.
func runIndexTransaction(sqlite *sql.DB, operation func() error) error {
	if sqlite == nil {
		return operation()
	}
	return shared.RunSqliteTransaction(sqlite, operation)
}

// computeBackoffMs mirrors #computeBackoffMs.
func (s *AgentIndexService) computeBackoffMs(attempts int) int64 {
	config := s.statusPlansConfig
	if attempts <= 1 {
		return int64(config.InitialBackoffMs)
	}
	exponential := config.InitialBackoffMs * math.Pow(config.BackoffFactor, float64(attempts-1))
	if exponential > config.MaxBackoffMs {
		return int64(config.MaxBackoffMs)
	}
	return int64(exponential)
}

// encodeCompactHistory mirrors the JSON round-trip the TS performs when it reads
// a stored compact_history_json column.
func encodeCompactHistory(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
