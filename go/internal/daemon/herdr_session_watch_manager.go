package daemon

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// This file ports src/daemon/herdr-session-watch-manager.ts.
//
// The TS class is written against promises: #watchers keeps one loop promise per
// session, #retiringWatcherLoops and #inFlightPlans are sets of promises stop()
// awaits, refreshHerdrSession hands back a promise whose identity the publication
// dedup keys on. The Go port keeps that structure with the three constructs that
// stand for a promise here: a context for an abortable operation, a
// chan struct{} closed when a running operation settles (its pointer is the
// promise identity, so a Set of promises is a map keyed by that pointer), and
// observability.RefreshSource for the awaitable refresh the index service shares
// between concurrent callers.

// ActiveRevisionPollMs mirrors ACTIVE_REVISION_POLL_MS: how often the scheduler
// re-reads the working sessions for a new agent revision.
const ActiveRevisionPollMs = 10_000

// FullRescanMs mirrors FULL_RESCAN_MS: the age of the last full rescan at which
// a tick becomes a rescan instead of a revision poll.
const FullRescanMs = 60_000

// PlanDrainGraceMs mirrors PLAN_DRAIN_GRACE_MS.
//
// Grace for status event plans still executing when the manager stops. Must stay
// below the daemon shutdown budget (SHUTDOWN_BUDGET_MS in src/daemon/service.ts,
// 5000ms) and therefore below the unit's TimeoutStopSec (10s): a drain grace
// longer than the stop timeout is what let SIGTERM end in systemd SIGKILL.
const PlanDrainGraceMs = 3_000

// maxReconnectDelayMs is the 30_000 cap of the TS reconnect backoff.
const maxReconnectDelayMs = 30_000

// HerdrWatchEventStream is the Go form of the async iterator subscribeEvents
// returns: Next yields one event, reports a stream failure, or reports done once
// its context is cancelled and every queued event has drained.
// *herdr.HerdrEventStream satisfies it.
type HerdrWatchEventStream interface {
	Next() (any, bool, error)
}

// HerdrSessionWatchClient is the slice of *herdr.HerdrSocketClient the watch
// manager depends on: the TS `Pick<HerdrSocketClient, "close" |
// "subscribeEvents">`.
type HerdrSessionWatchClient interface {
	Close()
	SubscribeEvents(ctx context.Context, paneIDs []string) (HerdrWatchEventStream, error)
}

// HerdrSessionWatchClientFactory mirrors the TS clientFactory option. The TS
// default (`new HerdrSocketClient({ socketPath })`) connects lazily, so building
// a client never fails there; the Go client dials in its constructor, so a dial
// failure is reported here. The manager surfaces it to whoever was starting or
// restarting the watcher, which is where a throwing clientFactory would have
// rejected the TS watch loop.
type HerdrSessionWatchClientFactory func(socketPath string) (HerdrSessionWatchClient, error)

// defaultHerdrSessionWatchClientFactory mirrors the default clientFactory: a
// fresh socket client per subscription.
func defaultHerdrSessionWatchClientFactory(
	socketPath string,
) (HerdrSessionWatchClient, error) {
	client, err := herdr.NewHerdrSocketClient(herdr.HerdrSocketClientOptions{
		SocketPath: socketPath,
	})
	if err != nil {
		return nil, err
	}
	return &herdrSocketWatchClient{client: client}, nil
}

// herdrSocketWatchClient adapts *herdr.HerdrSocketClient to
// HerdrSessionWatchClient: the concrete stream becomes the stream interface.
type herdrSocketWatchClient struct {
	client *herdr.HerdrSocketClient
}

func (c *herdrSocketWatchClient) Close() { c.client.Close() }

func (c *herdrSocketWatchClient) SubscribeEvents(
	ctx context.Context,
	paneIDs []string,
) (HerdrWatchEventStream, error) {
	stream, err := c.client.SubscribeEvents(ctx, paneIDs)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// WatchIndexService is the slice of the index service the watch manager always
// needs: the refreshHerdrSession and handleHerdrEvent entry points, i.e. the
// branches of the TS `this.#index.refreshHerdrSessionFast ? … : …` and
// `this.#index.handleHerdrEventFast ? … : …` conditionals that run when the fast
// method is absent. The fast variants and executeStatusEventPlan are optional,
// exactly as the TS code feature-detects them; *observability.AgentIndexService
// implements all five, so production takes the fast branches.
type WatchIndexService interface {
	HandleHerdrEvent(
		input observability.HandleHerdrEventInput,
	) (*observability.AgentEventHandlingResult, error)
	RefreshHerdrSessionSource(input observability.RefreshInput) *observability.RefreshSource
}

// WatchIndexFastRefresher mirrors the optional refreshHerdrSessionFast. A fast
// refresh returns its status event plans instead of executing them, so the
// manager submits them itself and stop() can drain them.
type WatchIndexFastRefresher interface {
	RefreshHerdrSessionFast(
		input observability.RefreshInput,
	) (*observability.AgentIndexRefreshFastResult, error)
}

// WatchIndexFastEventHandler mirrors the optional handleHerdrEventFast.
type WatchIndexFastEventHandler interface {
	HandleHerdrEventFast(
		input observability.HandleHerdrEventInput,
	) (*observability.AgentEventHandlingFastResult, error)
}

// WatchIndexPlanExecutor mirrors the optional executeStatusEventPlan the TS
// #submitPlan calls with `?.` and skips when it is absent.
type WatchIndexPlanExecutor interface {
	ExecuteStatusEventPlan(
		plan observability.StatusEventPlan,
	) (*observability.AgentEventRecord, error)
}

// refreshPublication is the settled outcome of one refresh: the publication the
// TS #refresh promise resolves to. The manager publishes and submits the plans
// while it is being produced, so awaiting it only hands the outcome over.
type refreshPublication struct {
	agents []observability.AgentIndexRecord
	err    error
	done   chan struct{}
}

func (p *refreshPublication) await() ([]observability.AgentIndexRecord, error) {
	<-p.done
	return p.agents, p.err
}

// planTask is one executeStatusEventPlan run tracked in #inFlightPlans: the TS
// promise #submitPlan keeps until its finally removes it.
type planTask struct {
	done chan struct{}
}

func (t *planTask) await() { <-t.done }

// watchSession is the Watcher record. Everything except entry and ctx is guarded
// by the manager mutex: the watch goroutine, the scheduler tick and a rescan all
// touch the client, the subscription restart callback and the pane ids.
type watchSession struct {
	ctx   context.Context
	abort context.CancelFunc
	entry herdr.HerdrSessionListEntry
	loop  chan struct{}

	client              HerdrSessionWatchClient
	restartSubscription func()
	subscribedPaneIDs   []string
}

// HerdrSessionWatchOptions mirrors the TS constructor options. Optional callbacks
// are nil for their TS `?? () => undefined` defaults; an empty interval is the
// TS `?? DEFAULT`, and ReconnectDelayMs uses HasReconnectDelayMs to keep an
// explicit 0 (the tests' "reconnect without waiting") distinct from an omitted
// one.
type HerdrSessionWatchOptions struct {
	ActiveRevisionPollMs  int
	Agents                *db.AgentStore
	ClientFactory         HerdrSessionWatchClientFactory
	FullRescanMs          int
	HerdrSessions         *db.HerdrSessionStore
	Index                 WatchIndexService
	OnAgentContextChanged func(scope observability.AgentScope)
	OnAgentEvent          func(event observability.AgentEventRecord)
	OnAgentIndexRefreshed func(agents []observability.AgentIndexRecord, herdrSessionName string)
	ReconnectDelayMs      int
	HasReconnectDelayMs   bool
	SessionList           herdr.HerdrSessionListRunner
	// ShutdownSignal mirrors the optional shutdownSignal: it is aborted when the
	// daemon starts shutting down, so watchers stop as soon as it aborts instead
	// of waiting for the daemon to reach its watchManager.stop() step.
	ShutdownSignal <-chan struct{}
}

// HerdrSessionWatchManager ports the TS class of the same name.
type HerdrSessionWatchManager struct {
	activeRevisionPollMs  int
	agents                *db.AgentStore
	clientFactory         HerdrSessionWatchClientFactory
	fullRescanMs          int
	herdrSessions         *db.HerdrSessionStore
	index                 WatchIndexService
	onAgentContextChanged func(scope observability.AgentScope)
	onAgentEvent          func(event observability.AgentEventRecord)
	onAgentIndexRefreshed func(agents []observability.AgentIndexRecord, herdrSessionName string)
	reconnectDelayMs      int
	sessionList           herdr.HerdrSessionListRunner
	shutdownSignal        <-chan struct{}

	mu sync.Mutex
	// refreshPublications mirrors #refreshPublications: the publication already
	// produced for one in-flight refresh source. The TS WeakMap is keyed on the
	// promise; here it is keyed on the *RefreshSource pointer, which is the same
	// object shared by every caller of one mutation epoch. Entries are kept for the
	// manager's lifetime instead of waiting for the collector: a source that has
	// settled still has to hand its already-published result to a caller that
	// picked it up before the index cleared it from its own in-flight registry.
	refreshPublications map[*observability.RefreshSource]*refreshPublication
	// retiringWatcherLoops mirrors #retiringWatcherLoops.
	retiringWatcherLoops map[chan struct{}]struct{}
	// inFlightPlans mirrors #inFlightPlans.
	inFlightPlans map[*planTask]struct{}
	watchers      map[string]*watchSession
	// scheduler mirrors #scheduler: closing schedulerStop ends the interval.
	schedulerStop chan struct{}
	// tickInFlight mirrors #tickInFlight: nil when no tick is running.
	tickInFlight   chan struct{}
	lastFullRescan int64
	generation     int
	stopping       bool
}

// NewHerdrSessionWatchManager mirrors the TS constructor, including the shutdown
// signal wiring: an already-aborted signal tears the manager down right away,
// otherwise the first abort does.
func NewHerdrSessionWatchManager(options HerdrSessionWatchOptions) *HerdrSessionWatchManager {
	activeRevisionPollMs := options.ActiveRevisionPollMs
	if activeRevisionPollMs == 0 {
		activeRevisionPollMs = ActiveRevisionPollMs
	}
	fullRescanMs := options.FullRescanMs
	if fullRescanMs == 0 {
		fullRescanMs = FullRescanMs
	}
	reconnectDelayMs := 1_000
	if options.HasReconnectDelayMs {
		reconnectDelayMs = options.ReconnectDelayMs
	}
	clientFactory := options.ClientFactory
	if clientFactory == nil {
		clientFactory = defaultHerdrSessionWatchClientFactory
	}
	onAgentContextChanged := options.OnAgentContextChanged
	if onAgentContextChanged == nil {
		onAgentContextChanged = func(observability.AgentScope) {}
	}
	onAgentEvent := options.OnAgentEvent
	if onAgentEvent == nil {
		onAgentEvent = func(observability.AgentEventRecord) {}
	}
	onAgentIndexRefreshed := options.OnAgentIndexRefreshed
	if onAgentIndexRefreshed == nil {
		onAgentIndexRefreshed = func([]observability.AgentIndexRecord, string) {}
	}
	sessionList := options.SessionList
	if sessionList == nil {
		sessionList = herdr.CreateHerdrSessionListRunner(herdr.HerdrSessionListRunnerOptions{})
	}
	manager := &HerdrSessionWatchManager{
		activeRevisionPollMs:  activeRevisionPollMs,
		agents:                options.Agents,
		clientFactory:         clientFactory,
		fullRescanMs:          fullRescanMs,
		herdrSessions:         options.HerdrSessions,
		index:                 options.Index,
		onAgentContextChanged: onAgentContextChanged,
		onAgentEvent:          onAgentEvent,
		onAgentIndexRefreshed: onAgentIndexRefreshed,
		reconnectDelayMs:      reconnectDelayMs,
		sessionList:           sessionList,
		shutdownSignal:        options.ShutdownSignal,
		refreshPublications:   map[*observability.RefreshSource]*refreshPublication{},
		retiringWatcherLoops:  map[chan struct{}]struct{}{},
		inFlightPlans:         map[*planTask]struct{}{},
		watchers:              map[string]*watchSession{},
	}
	if manager.shutdownSignal != nil {
		go func() {
			<-manager.shutdownSignal
			manager.logBeginShutdownFailure(manager.beginShutdown())
		}()
		select {
		case <-manager.shutdownSignal:
			manager.logBeginShutdownFailure(manager.beginShutdown())
		default:
		}
	}
	return manager
}

// logBeginShutdownFailure is the TS `.catch` on the shutdown-signal teardown: no
// caller can receive the error there, so it is only reported.
func (m *HerdrSessionWatchManager) logBeginShutdownFailure(err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr,
		"Herdsman Herdr session watch shutdown failed {error:%v step:begin-shutdown}\n", err)
}

// nowMs is Date.now().
func (m *HerdrSessionWatchManager) nowMs() int64 {
	return time.Now().UnixMilli()
}

// Start mirrors start(): one rescan up front, then the revision-poll interval.
// The interval body never logs a tick failure that has no listener: the tick runs
// in its own goroutine and reports its own failure, exactly like the TS
// `.catch` attached where the promise is created rather than in stop().
func (m *HerdrSessionWatchManager) Start() error {
	m.mu.Lock()
	m.stopping = false
	m.generation++
	generation := m.generation
	m.mu.Unlock()

	if err := m.RescanNow(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping || generation != m.generation {
		return nil
	}
	if m.schedulerStop != nil {
		// The TS setInterval is not reentrant-safe either: a second start()
		// without stop() would leave the previous interval running.
		return nil
	}
	stop := make(chan struct{})
	m.schedulerStop = stop
	interval := time.Duration(m.activeRevisionPollMs) * time.Millisecond
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.scheduleTick()
			}
		}
	}()
	return nil
}

// Stop mirrors stop(): synchronous teardown, then the in-flight tick, the
// retiring watcher loops and finally the plan drain grace.
func (m *HerdrSessionWatchManager) Stop() error {
	if err := m.beginShutdown(); err != nil {
		return err
	}
	m.mu.Lock()
	tick := m.tickInFlight
	m.mu.Unlock()
	if tick != nil {
		<-tick
	}
	m.awaitLoops(m.retiringSnapshot())

	// Promise.race([Promise.all(inFlightPlans), delay(PLAN_DRAIN_GRACE_MS)]):
	// the grace is measured from here, and a plan still running when it expires is
	// abandoned (its goroutine keeps going; nothing awaits it any more).
	deadline := time.Now().Add(time.Duration(PlanDrainGraceMs) * time.Millisecond)
	for {
		m.mu.Lock()
		tasks := make([]*planTask, 0, len(m.inFlightPlans))
		for task := range m.inFlightPlans {
			tasks = append(tasks, task)
		}
		m.mu.Unlock()
		if len(tasks) == 0 {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		drained := make(chan struct{})
		go func() {
			for _, task := range tasks {
				task.await()
			}
			close(drained)
		}()
		timer := time.NewTimer(remaining)
		select {
		case <-drained:
			timer.Stop()
			// Re-read the set: a plan submitted while the drain was running is
			// drained too, until the grace is used up.
			continue
		case <-timer.C:
			return m.abortWatchers()
		}
	}
	return m.abortWatchers()
}

// beginShutdown mirrors #beginShutdown: the synchronous teardown entry. Stop
// scheduling ticks and abort every watcher, so an aborted shutdown can never
// leave watchers (and their sockets) running. It is also the shutdown-signal
// entry point, where no caller can receive an error; a teardown failure there is
// logged the way the TS `.catch` logs it.
func (m *HerdrSessionWatchManager) beginShutdown() error {
	m.mu.Lock()
	m.stopping = true
	m.generation++
	if m.schedulerStop != nil {
		close(m.schedulerStop)
		m.schedulerStop = nil
	}
	m.mu.Unlock()
	return m.abortWatchers()
}

// scheduleTick is the setInterval body: one tick at a time, with the failure
// handler attached where the tick is started.
func (m *HerdrSessionWatchManager) scheduleTick() {
	m.mu.Lock()
	if m.tickInFlight != nil {
		m.mu.Unlock()
		return
	}
	done := make(chan struct{})
	m.tickInFlight = done
	m.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			m.mu.Lock()
			if m.tickInFlight == done {
				m.tickInFlight = nil
			}
			m.mu.Unlock()
		}()
		if err := m.tick(); err != nil {
			fmt.Fprintf(os.Stderr,
				"Herdsman Herdr session watch tick failed {error:%v step:active-revision-poll}\n", err)
		}
	}()
}

// RescanNow mirrors rescanNow.
func (m *HerdrSessionWatchManager) RescanNow() error {
	m.mu.Lock()
	generation := m.generation
	m.mu.Unlock()

	sessions, err := m.sessionList()
	if err != nil {
		return err
	}
	if m.isStopping(generation) {
		return nil
	}
	running := make([]herdr.HerdrSessionListEntry, 0, len(sessions))
	runningNames := make([]string, 0, len(sessions))
	runningNameSet := map[string]struct{}{}
	for _, session := range sessions {
		if !session.Running {
			continue
		}
		running = append(running, session)
		if _, seen := runningNameSet[session.Name]; !seen {
			runningNameSet[session.Name] = struct{}{}
			runningNames = append(runningNames, session.Name)
		}
	}

	m.mu.Lock()
	retiring := make([]chan struct{}, 0, len(m.watchers))
	for name, watcher := range m.watchers {
		if _, ok := runningNameSet[name]; ok {
			continue
		}
		retiring = append(retiring, m.retireWatcherLocked(name, watcher))
	}
	m.mu.Unlock()
	if err := m.awaitLoops(retiring); err != nil {
		return err
	}
	if m.isStopping(generation) {
		return nil
	}
	if err := m.herdrSessions.MarkStoppedMissingFrom(runningNames); err != nil {
		return err
	}

	for _, entry := range running {
		if m.isStopping(generation) {
			return nil
		}
		m.mu.Lock()
		existing := m.watchers[entry.Name]
		var existingLoop chan struct{}
		if existing != nil {
			existingLoop = m.retireWatcherLocked(entry.Name, existing)
		}
		m.mu.Unlock()
		if existingLoop != nil {
			if err := m.awaitLoops([]chan struct{}{existingLoop}); err != nil {
				return err
			}
		}
		if err := m.startWatcher(entry, generation); err != nil {
			return err
		}
	}
	m.mu.Lock()
	if !m.stopping && generation == m.generation {
		m.lastFullRescan = m.nowMs()
	}
	m.mu.Unlock()
	return nil
}

// tick mirrors #tick.
func (m *HerdrSessionWatchManager) tick() error {
	m.mu.Lock()
	rescanDue := m.nowMs()-m.lastFullRescan >= int64(m.fullRescanMs)
	m.mu.Unlock()
	if rescanDue {
		return m.RescanNow()
	}

	m.mu.Lock()
	watchers := make([]*watchSession, 0, len(m.watchers))
	for _, watcher := range m.watchers {
		watchers = append(watchers, watcher)
	}
	m.mu.Unlock()

	working := make([]*watchSession, 0, len(watchers))
	for _, watcher := range watchers {
		agents, err := m.agents.ListForHerdrSession(watcher.entry.Name)
		if err != nil {
			return err
		}
		for _, agent := range agents {
			if agent.AgentStatus == observability.AgentStatusWorking {
				working = append(working, watcher)
				break
			}
		}
	}

	_, err := mapConcurrent(working, 4, func(watcher *watchSession) (struct{}, error) {
		agents, err := m.refresh(watcher.entry, context.Background())
		if err != nil {
			return struct{}{}, err
		}
		// The subscribed pane ids are read after the refresh, like the TS does:
		// the watch loop may have re-subscribed while this refresh was running.
		m.mu.Lock()
		subscribed := append([]string{}, watcher.subscribedPaneIDs...)
		restart := watcher.restartSubscription
		m.mu.Unlock()
		if containsNewPaneID(agents, subscribed) && restart != nil {
			restart()
		}
		return struct{}{}, nil
	})
	return err
}

// startWatcher mirrors #startWatcher: upsert the running session row, build the
// client, hand the watcher its own subscription, then verify the generation once
// more and tear the watcher back down if the manager moved on meanwhile.
func (m *HerdrSessionWatchManager) startWatcher(
	entry herdr.HerdrSessionListEntry,
	generation int,
) error {
	if m.isStopping(generation) {
		return nil
	}
	if _, err := m.herdrSessions.UpsertRunning(db.UpsertRunningHerdrSessionInput{
		Name:       entry.Name,
		SessionDir: entry.SessionDir,
		SocketPath: entry.SocketPath,
	}); err != nil {
		return err
	}
	client, err := m.clientFactory(entry.SocketPath)
	if err != nil {
		return err
	}
	ctx, abort := context.WithCancel(context.Background())
	watcher := &watchSession{
		ctx:    ctx,
		abort:  abort,
		entry:  entry,
		loop:   make(chan struct{}),
		client: client,
	}
	m.mu.Lock()
	m.watchers[entry.Name] = watcher
	m.mu.Unlock()

	go func() {
		defer close(watcher.loop)
		// The TS loop promise carries `.catch(() => undefined)`: a watch loop
		// that dies on an unexpected failure takes its socket with it and leaves
		// the rescan to replace it.
		_ = m.watch(watcher, generation)
	}()

	if m.isStopping(generation) {
		m.mu.Lock()
		if m.watchers[entry.Name] == watcher {
			delete(m.watchers, entry.Name)
		}
		watcher.abort()
		watcher.client.Close()
		m.mu.Unlock()
		<-watcher.loop
	}
	return nil
}

// retireWatcherLocked mirrors #retireWatcher. The caller holds m.mu; it awaits
// the returned loop outside the lock.
func (m *HerdrSessionWatchManager) retireWatcherLocked(
	name string,
	watcher *watchSession,
) chan struct{} {
	if m.watchers[name] == watcher {
		delete(m.watchers, name)
	}
	watcher.abort()
	watcher.client.Close()
	m.retiringWatcherLoops[watcher.loop] = struct{}{}
	go func(loop chan struct{}) {
		<-loop
		m.mu.Lock()
		delete(m.retiringWatcherLoops, loop)
		m.mu.Unlock()
	}(watcher.loop)
	return watcher.loop
}

// abortWatchers mirrors #abortWatchers.
func (m *HerdrSessionWatchManager) abortWatchers() error {
	m.mu.Lock()
	loops := make([]chan struct{}, 0, len(m.watchers)+len(m.retiringWatcherLoops))
	for name, watcher := range m.watchers {
		loops = append(loops, m.retireWatcherLocked(name, watcher))
	}
	for loop := range m.retiringWatcherLoops {
		loops = append(loops, loop)
	}
	m.mu.Unlock()
	return m.awaitLoops(loops)
}

// awaitLoops is Promise.all over watcher loop channels: every loop is awaited.
// A watch loop never fails — its goroutine swallows the error the way the TS
// `.catch(() => undefined)` does — so there is no rejection to report; the error
// return keeps the shape of the TS call sites, which all await these promises
// inside steps that can themselves fail.
func (m *HerdrSessionWatchManager) awaitLoops(loops []chan struct{}) error {
	for _, loop := range loops {
		<-loop
	}
	return nil
}

func (m *HerdrSessionWatchManager) retiringSnapshot() []chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	loops := make([]chan struct{}, 0, len(m.retiringWatcherLoops))
	for loop := range m.retiringWatcherLoops {
		loops = append(loops, loop)
	}
	return loops
}

// isStopping mirrors the `this.#stopping || generation !==
// this.#lifecycleGeneration` guard.
func (m *HerdrSessionWatchManager) isStopping(generation int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopping || generation != m.generation
}

// watch mirrors #watch: the per-session subscribe/reconnect loop. ctx is the
// watcher's abort context; each iteration subscribes through a child context,
// which is how the TS `signal.addEventListener("abort", stopSubscription)` plus
// the restart callback abort the subscription.
func (m *HerdrSessionWatchManager) watch(watcher *watchSession, generation int) error {
	ctx := watcher.ctx
	entry := watcher.entry
	reconnectCount := 0
	lastPaneIDs := []string{}
	lastEvent := map[string]any{}
	lastClosedTriggered := false
	var restartAgents []observability.AgentIndexRecord

	for ctx.Err() == nil && !m.isStopping(generation) {
		// restart is written by the watch goroutine and by whoever invokes the
		// subscription restart callback (the scheduler tick), so it lives behind
		// the manager mutex.
		restart := false
		setRestart := func() {
			m.mu.Lock()
			restart = true
			m.mu.Unlock()
		}
		isRestart := func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return restart
		}
		// The TS catch path delays by the exponential backoff and the ordinary
		// reconnect path by reconnectDelayMs, so the two stay two separate tails.
		subscriptionCtx, stopSubscription := context.WithCancel(ctx)
		m.mu.Lock()
		watcher.restartSubscription = func() {
			setRestart()
			stopSubscription()
		}
		m.mu.Unlock()

		err := m.watchOnce(
			watcher, subscriptionCtx, setRestart, &reconnectCount, &restartAgents,
			&lastPaneIDs, &lastEvent, &lastClosedTriggered,
		)
		stopSubscription()
		if err != nil && ctx.Err() == nil && !isRestart() {
			// A stream that failed for a reason other than a restart is the TS catch
			// path: count the reconnect, warn, replace the client, back off.
			reconnectCount++
			reconnectDelay := min(
				m.reconnectDelayMs*int(math.Pow(2, float64(reconnectCount-1))),
				maxReconnectDelayMs,
			)
			fmt.Fprintf(os.Stderr,
				"Herdsman Herdr subscription reconnect {sessionName:%s socketPath:%s paneIds:%v subscriptionGeneration:%d eventId:%v revision:%v reconnectCount:%d paneClosedTriggered:%t error:%v}\n",
				entry.Name, entry.SocketPath, lastPaneIDs, m.currentGeneration(),
				eventField(lastEvent, "id", "event_id"), eventField(lastEvent, "revision", "pane_revision"),
				reconnectCount, lastClosedTriggered, err)
			lastClosedTriggered = false
			restartAgents = nil
			if ctx.Err() != nil {
				return nil
			}
			if replaceErr := m.replaceClient(watcher, entry.SocketPath); replaceErr != nil {
				return replaceErr
			}
			sleepWithContext(ctx, reconnectDelay)
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if replaceErr := m.replaceClient(watcher, entry.SocketPath); replaceErr != nil {
			return replaceErr
		}
		if isRestart() {
			reconnectCount = 0
			continue
		}
		reconnectCount = 0
		// The TS delay is bounded by the watcher's own signal, not the
		// subscription's, which the iteration just ended.
		sleepWithContext(ctx, m.reconnectDelayMs)
	}
	return nil
}

// watchOnce is one subscription of #watch: the body of the TS try block. It
// returns nil when the subscription ended (the loop then reconnects or restarts)
// and an error when it failed (the TS catch path).
func (m *HerdrSessionWatchManager) watchOnce(
	watcher *watchSession,
	subscriptionCtx context.Context,
	setRestart func(),
	reconnectCount *int,
	restartAgents *[]observability.AgentIndexRecord,
	lastPaneIDs *[]string,
	lastEvent *map[string]any,
	lastClosedTriggered *bool,
) error {
	entry := watcher.entry
	var agents []observability.AgentIndexRecord
	var err error
	if *restartAgents != nil {
		agents = *restartAgents
		*restartAgents = nil
	} else {
		agents, err = m.refresh(entry, subscriptionCtx)
		if err != nil {
			return err
		}
	}
	// A refresh that came back is progress: the reconnect backoff starts over.
	*reconnectCount = 0
	if subscriptionCtx.Err() != nil {
		return nil
	}
	paneIDs := make([]string, 0, len(agents))
	for _, agent := range agents {
		paneIDs = append(paneIDs, agent.PaneID)
	}
	*lastPaneIDs = paneIDs
	m.mu.Lock()
	watcher.subscribedPaneIDs = paneIDs
	client := watcher.client
	m.mu.Unlock()

	stream, err := client.SubscribeEvents(subscriptionCtx, paneIDs)
	if err != nil {
		return err
	}
	for {
		event, done, err := stream.Next()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if subscriptionCtx.Err() != nil {
			return nil
		}
		eventRecord := record(event)
		*lastEvent = eventRecord
		*reconnectCount = 0
		eventType, _ := eventRecord["type"].(string)
		if eventType == "pane.agent_status_changed" {
			if err := m.handleWatchEvent(entry, event); err != nil {
				return err
			}
			continue
		}
		if !isTopologyEvent(eventType) {
			continue
		}
		refreshed, err := m.refresh(entry, subscriptionCtx)
		if err != nil {
			return err
		}
		*lastPaneIDs = paneIDsOf(refreshed)
		if eventType == "pane.closed" && !closedHitsLivePane(eventRecord, refreshed) {
			if err := m.handleWatchEvent(entry, event); err != nil {
				return err
			}
		}
		m.mu.Lock()
		subscribed := append([]string{}, watcher.subscribedPaneIDs...)
		m.mu.Unlock()
		if containsNewPaneID(refreshed, subscribed) {
			setRestart()
			*restartAgents = refreshed
			*lastClosedTriggered = eventType == "pane.closed"
			return nil
		}
	}
}

// replaceClient mirrors #replaceClient.
func (m *HerdrSessionWatchManager) replaceClient(
	watcher *watchSession,
	socketPath string,
) error {
	client, err := m.clientFactory(socketPath)
	if err != nil {
		return err
	}
	m.mu.Lock()
	watcher.client.Close()
	watcher.client = client
	m.mu.Unlock()
	return nil
}

// handleWatchEvent mirrors #handleWatchEvent, including the optional
// handleHerdrEventFast branch: the fast result carries the status event plans the
// manager submits itself.
func (m *HerdrSessionWatchManager) handleWatchEvent(
	entry herdr.HerdrSessionListEntry,
	event any,
) error {
	input := observability.HandleHerdrEventInput{
		Event:            event,
		HerdrSessionName: entry.Name,
		SessionDir:       entry.SessionDir,
		SocketPath:       entry.SocketPath,
	}
	var scopes []observability.AgentScope
	var events []observability.AgentEventRecord
	var plans []observability.StatusEventPlan
	if fast, ok := m.index.(WatchIndexFastEventHandler); ok {
		result, err := fast.HandleHerdrEventFast(input)
		if err != nil {
			return err
		}
		scopes, events, plans = result.ContextChangedScopes, result.Events, result.StatusEventPlans
	} else {
		result, err := m.index.HandleHerdrEvent(input)
		if err != nil {
			return err
		}
		scopes, events = result.ContextChangedScopes, result.Events
	}
	agents, err := m.agents.ListForHerdrSession(entry.Name)
	if err != nil {
		return err
	}
	m.publishResult(agents, scopes, events, entry.Name)
	for _, plan := range plans {
		m.submitPlan(plan)
	}
	return nil
}

// refresh mirrors #refresh: ask the index for the session (the fast method when
// it has one), share the publication between every caller that was handed the
// same in-flight source, publish once and submit the plans the fast result
// returned.
func (m *HerdrSessionWatchManager) refresh(
	entry herdr.HerdrSessionListEntry,
	ctx context.Context,
) ([]observability.AgentIndexRecord, error) {
	if fast, ok := m.index.(WatchIndexFastRefresher); ok {
		// A fast refresh hands back a fresh result every call, so there is no
		// shared source to key a publication on: the TS WeakMap never hits here
		// either.
		result, err := fast.RefreshHerdrSessionFast(observability.RefreshInput{
			HerdrSessionName: entry.Name,
			SessionDir:       entry.SessionDir,
			SocketPath:       entry.SocketPath,
		})
		if err != nil {
			return nil, err
		}
		m.publishResult(result.Agents, result.ContextChangedScopes, result.Events, entry.Name)
		for _, plan := range result.StatusEventPlans {
			m.submitPlan(plan)
		}
		return result.Agents, nil
	}

	source := m.index.RefreshHerdrSessionSource(observability.RefreshInput{
		HerdrSessionName: entry.Name,
		SessionDir:       entry.SessionDir,
		SocketPath:       entry.SocketPath,
	})
	m.mu.Lock()
	publication, shared := m.refreshPublications[source]
	if !shared {
		publication = &refreshPublication{done: make(chan struct{})}
		m.refreshPublications[source] = publication
		go func(publication *refreshPublication) {
			defer close(publication.done)
			result, err := source.Await()
			if err != nil {
				publication.err = err
				return
			}
			m.publishResult(result.Agents, result.ContextChangedScopes, result.Events, entry.Name)
			// The slow refresh has already run its status event plans, so its
			// result carries none: `"statusEventPlans" in result` is false.
			publication.agents = result.Agents
		}(publication)
	}
	m.mu.Unlock()

	return m.awaitPublication(ctx, publication)
}

// awaitPublication waits for the publication, honouring the abort the caller
// passes (the TS side has no abort here: the caller's own promise is just
// abandoned, which in Go means this call has to stop waiting).
func (m *HerdrSessionWatchManager) awaitPublication(
	ctx context.Context,
	publication *refreshPublication,
) ([]observability.AgentIndexRecord, error) {
	// A publication that already settled is reported as settled even when the
	// caller's context was aborted at the same moment: the TS awaiter still reads
	// the resolved value and only then checks the abort.
	select {
	case <-publication.done:
		return publication.await()
	default:
	}
	select {
	case <-publication.done:
		return publication.await()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// submitPlan mirrors #submitPlan, including the optional executeStatusEventPlan:
// with no executor there is nothing to track, so #inFlightPlans stays empty.
func (m *HerdrSessionWatchManager) submitPlan(plan observability.StatusEventPlan) {
	executor, ok := m.index.(WatchIndexPlanExecutor)
	if !ok {
		return
	}
	task := &planTask{done: make(chan struct{})}
	m.mu.Lock()
	m.inFlightPlans[task] = struct{}{}
	m.mu.Unlock()
	go func() {
		defer close(task.done)
		defer func() {
			m.mu.Lock()
			delete(m.inFlightPlans, task)
			m.mu.Unlock()
		}()
		planEvent, err := executor.ExecuteStatusEventPlan(plan)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"Herdsman status event plan failed {agentId:%s error:%v from:%s to:%s}\n",
				plan.Agent.ID, err, plan.From, plan.To)
			return
		}
		if planEvent != nil {
			if _, valid := observability.ValidAgentEventTypes[string(planEvent.Type)]; valid {
				m.onAgentEvent(*planEvent)
			}
		}
	}()
}

// publishResult mirrors #publishResult.
func (m *HerdrSessionWatchManager) publishResult(
	agents []observability.AgentIndexRecord,
	contextChangedScopes []observability.AgentScope,
	events []observability.AgentEventRecord,
	herdrSessionName string,
) {
	m.onAgentIndexRefreshed(agents, herdrSessionName)
	for _, scope := range contextChangedScopes {
		m.onAgentContextChanged(scope)
	}
	for _, event := range events {
		m.onAgentEvent(event)
	}
}

func (m *HerdrSessionWatchManager) currentGeneration() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}

// ---------------------------------------------------------------------------
// Module-level helpers
// ---------------------------------------------------------------------------

// delay mirrors the TS delay(ms, signal): a non-positive wait or an already
// aborted signal resolves immediately, and an abort cancels the pending timer.
func sleepWithContext(ctx context.Context, ms int) {
	if ms <= 0 || ctx.Err() != nil {
		return
	}
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// isTopologyEvent mirrors isTopologyEvent.
func isTopologyEvent(eventType string) bool {
	for _, candidate := range herdr.HerdrTopologyEventTypes {
		if candidate == eventType {
			return true
		}
	}
	return false
}

// closedHitsLivePane mirrors closedHitsLivePane: does this pane.closed event name
// a pane the refresh still sees?
func closedHitsLivePane(
	event map[string]any,
	agents []observability.AgentIndexRecord,
) bool {
	paneID := stringValue(event["pane_id"])
	if paneID == nil {
		return false
	}
	generation := stringValue(event["pane_generation"])
	for _, agent := range agents {
		if agent.PaneID != *paneID {
			continue
		}
		if generation == nil {
			return true
		}
		if agent.PaneGeneration != nil && *agent.PaneGeneration == *generation {
			return true
		}
	}
	return false
}

// containsNewPaneId mirrors containsNewPaneId: did the refresh find a pane the
// current subscription never asked about?
func containsNewPaneID(
	agents []observability.AgentIndexRecord,
	subscribedPaneIDs []string,
) bool {
	if len(agents) == 0 {
		return false
	}
	subscribed := make(map[string]struct{}, len(subscribedPaneIDs))
	for _, paneID := range subscribedPaneIDs {
		subscribed[paneID] = struct{}{}
	}
	for _, agent := range agents {
		if _, ok := subscribed[agent.PaneID]; !ok {
			return true
		}
	}
	return false
}

// paneIdsOf is `agents.map((agent) => agent.paneId)`.
func paneIDsOf(agents []observability.AgentIndexRecord) []string {
	paneIDs := make([]string, 0, len(agents))
	for _, agent := range agents {
		paneIDs = append(paneIDs, agent.PaneID)
	}
	return paneIDs
}

// stringValue mirrors stringValue: a non-empty string, else nil.
func stringValue(value any) *string {
	if text, ok := value.(string); ok && text != "" {
		return &text
	}
	return nil
}

// record mirrors record: the object view of a decoded socket event.
func record(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

// eventField mirrors `lastEvent?.id ?? lastEvent?.event_id ?? null` and its
// revision twin: the first present spelling of one event field. The reconnect log
// line reads both because the socket has emitted both spellings historically; it
// never decides behaviour, so an unknown spelling is simply reported as null.
func eventField(event map[string]any, snake string, camel string) any {
	for _, key := range []string{snake, camel} {
		if value, ok := event[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

// mapConcurrent mirrors mapConcurrent: fixed-size chunks, awaited one chunk at a
// time, results in input order.
func mapConcurrent[T any, R any](
	items []T,
	limit int,
	fn func(item T) (R, error),
) ([]R, error) {
	results := make([]R, 0, len(items))
	for start := 0; start < len(items); start += limit {
		end := min(start+limit, len(items))
		chunk := items[start:end]
		chunkResults := make([]R, len(chunk))
		errs := make([]error, len(chunk))
		var wg sync.WaitGroup
		for index := range chunk {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				chunkResults[index], errs[index] = fn(chunk[index])
			}(index)
		}
		wg.Wait()
		// Promise.all rejects with the first rejection but still runs the rest of
		// the chunk; every result is collected before the failure is reported.
		for _, err := range errs {
			if err != nil {
				return results, err
			}
		}
		results = append(results, chunkResults...)
	}
	return results, nil
}
