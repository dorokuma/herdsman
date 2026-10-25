package daemon

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dorokuma/herdsman/go/internal/agenthistory"
	"github.com/dorokuma/herdsman/go/internal/config"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// This file ports src/daemon/service.ts: the daemon root service, the periodic
// reconcile scheduler and the budgeted shutdown sequence.
//
// Concurrency. The TS daemon is one event loop: every block between two awaits is
// atomic, and the signal handlers, the startup path and the shutdown path never
// interleave inside such a block. The Go daemon runs the same steps on different
// goroutines (the accept loop, the watch manager, the signal delivery), so the
// few pieces of state the TS keeps in closure variables (stopping, exitRequested,
// the live signal registration) are guarded by one mutex here. Everything else
// keeps the TS order exactly.

// ReconcileIntervalMs mirrors RECONCILE_INTERVAL_MS. The 7-day (settled events)
// and 30-day (released scopes) TTLs only converge when
// AgentEventReconciler.Reconcile runs, so the daemon drives it on a fixed
// 15-minute cycle instead of only at startup. 15 minutes is deliberately coarser
// than the watch manager's 10s/60s refresh ticks: reconcile opens sockets to
// every running Herdr session, while the TTLs it enforces are days long, so a
// 15-minute cycle converges promptly without per-tick socket traffic.
const ReconcileIntervalMs = 15 * 60 * 1000

// ShutdownBudgetMs mirrors SHUTDOWN_BUDGET_MS. It must stay clearly below the
// unit's TimeoutStopSec (10s in /etc/systemd/system/herdsman.service): a shutdown
// that outlives it is SIGKILLed, which skips the finally cleanup (instance lock,
// pid file, helper child) and leaves the daemon half-stopped. Every step is
// bounded by whatever budget is left, so a single wedged step cannot run the
// sequence past this ceiling.
const ShutdownBudgetMs = 5_000

// ShutdownMinStepMs mirrors SHUTDOWN_MIN_STEP_MS. The floor granted to a step
// once the budget is spent. The sequence may therefore overshoot the budget by at
// most (steps - 1) * ShutdownMinStepMs, which still leaves several seconds of
// headroom under TimeoutStopSec; in exchange no step is skipped outright, so the
// pid file and socket are always cleaned up.
const ShutdownMinStepMs = 250

// ---------------------------------------------------------------------------
// Budgeted shutdown sequence
// ---------------------------------------------------------------------------

// ShutdownStep mirrors the TS type of the same name.
type ShutdownStep struct {
	Name string
	Run  func() error
}

// ShutdownStepsOptions mirrors `{ budgetMs?: number }`. BudgetMs is a pointer so
// an explicit 0 stays distinguishable from an omitted key: the TS `??` keeps 0.
type ShutdownStepsOptions struct {
	BudgetMs *int64
}

// RunShutdownSteps mirrors runShutdownSteps: the steps run in order under one
// shared budget, each raced against the time left. A step that overruns is logged
// and abandoned so the next step (and the caller's finally cleanup) still runs.
// A failure that is not a timeout is returned, aborting the remaining steps,
// exactly like the TS rejection that propagates out of the sequence.
func RunShutdownSteps(steps []ShutdownStep, options *ShutdownStepsOptions) error {
	budgetMs := int64(ShutdownBudgetMs)
	if options != nil && options.BudgetMs != nil {
		budgetMs = *options.BudgetMs
	}
	startedAt := time.Now()
	deadline := startedAt.Add(time.Duration(budgetMs) * time.Millisecond)
	for _, step := range steps {
		stepStartedAt := time.Now()
		remainingMs := deadline.Sub(stepStartedAt).Milliseconds()
		timeoutMs := remainingMs
		if timeoutMs < ShutdownMinStepMs {
			timeoutMs = ShutdownMinStepMs
		}
		fmt.Fprintf(os.Stdout,
			"Herdsman daemon shutdown step starting (budgetRemainingMs=%d step=%s timeoutMs=%d)\n",
			maxInt64(remainingMs, 0), step.Name, timeoutMs)
		timedOut, err := raceShutdownStep(step, timeoutMs)
		elapsedMs := time.Since(stepStartedAt).Milliseconds()
		if err != nil {
			return err
		}
		if timedOut {
			fmt.Fprintf(os.Stderr,
				"Herdsman daemon shutdown step timed out; continuing"+
					" (budgetMs=%d elapsedMs=%d step=%s timeoutMs=%d)\n",
				budgetMs, elapsedMs, step.Name, timeoutMs)
			continue
		}
		fmt.Fprintf(os.Stdout,
			"Herdsman daemon shutdown step finished (elapsedMs=%d step=%s)\n",
			elapsedMs, step.Name)
	}
	fmt.Fprintf(os.Stdout,
		"Herdsman daemon shutdown steps finished (budgetMs=%d elapsedMs=%d)\n",
		budgetMs, time.Since(startedAt).Milliseconds())
	return nil
}

// raceShutdownStep mirrors raceShutdownStep: it returns (timedOut, err). The
// timeout wins by returning true with a nil error; a step failure returns false
// with the error, which the caller propagates.
//
// The step runs on its own goroutine and is abandoned when the timeout wins,
// exactly like the TS race where the abandoned promise keeps running with nobody
// left to observe it. A panic inside the step becomes an error, mirroring the
// promise rejection a synchronous throw produces (and keeping a wedged step from
// taking the whole daemon down).
func raceShutdownStep(step ShutdownStep, timeoutMs int64) (bool, error) {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("shutdown step %s panicked: %v", step.Name, recovered)
			}
		}()
		if step.Run == nil {
			done <- nil
			return
		}
		done <- step.Run()
	}()
	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		return false, err
	case <-timer.C:
		return true, nil
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// DefaultSetInterval mirrors the JS setInterval the scheduler defaults to.
func DefaultSetInterval(callback func(), delayMs int64) any {
	return defaultSetInterval(callback, int(delayMs))
}

// DefaultClearInterval mirrors clearInterval.
func DefaultClearInterval(handle any) {
	defaultClearInterval(handle)
}

// ---------------------------------------------------------------------------
// Periodic reconcile scheduler
// ---------------------------------------------------------------------------

// ReconcileRun mirrors the TS `() => Promise<unknown>` reconcile cycle.
type ReconcileRun func() error

// PeriodicReconcileSchedulerOptions mirrors the TS constructor options. A nil
// seam means "use the production implementation".
type PeriodicReconcileSchedulerOptions struct {
	ClearInterval func(handle any)
	IntervalMs    *int64
	Run           ReconcileRun
	SetInterval   func(callback func(), delayMs int64) any
}

// PeriodicReconcileScheduler ports the TS class of the same name: it runs the
// reconcile cycle on a fixed cadence while guaranteeing at most one in-flight
// run — a tick that fires while the previous run is still executing is skipped.
// This mirrors HerdrSessionWatchManager's in-flight tick guard. SQLite writes on
// the daemon's single shared connection are synchronous, so the only concurrency
// hazard between periodic reconcile and live event writes would be two reconcile
// loops interleaving at their await points; the in-flight guard prevents that,
// keeping event writes and reconcile serialized exactly like the rest of the
// daemon.
type PeriodicReconcileScheduler struct {
	clearInterval func(handle any)
	intervalMs    int64
	run           ReconcileRun
	setInterval   func(callback func(), delayMs int64) any

	mu       sync.Mutex
	handle   any
	hasStart bool
	inFlight chan struct{}
}

// NewPeriodicReconcileScheduler mirrors the TS constructor.
func NewPeriodicReconcileScheduler(
	options PeriodicReconcileSchedulerOptions,
) *PeriodicReconcileScheduler {
	scheduler := &PeriodicReconcileScheduler{
		clearInterval: options.ClearInterval,
		run:           options.Run,
		setInterval:   options.SetInterval,
	}
	if scheduler.clearInterval == nil {
		scheduler.clearInterval = DefaultClearInterval
	}
	if scheduler.setInterval == nil {
		scheduler.setInterval = DefaultSetInterval
	}
	scheduler.intervalMs = ReconcileIntervalMs
	if options.IntervalMs != nil {
		scheduler.intervalMs = *options.IntervalMs
	}
	return scheduler
}

// Start mirrors start(): it arms the interval once; a second call is a no-op.
func (s *PeriodicReconcileScheduler) Start() {
	s.mu.Lock()
	// hasStart covers both the TS `#handle !== undefined` guard and the window
	// while setInterval is still running, so a setInterval double that calls back
	// synchronously cannot re-enter here.
	if s.hasStart {
		s.mu.Unlock()
		return
	}
	s.hasStart = true
	s.mu.Unlock()

	handle := s.setInterval(s.tick, s.intervalMs)

	s.mu.Lock()
	s.handle = handle
	s.mu.Unlock()
}

// Stop mirrors stop(): it clears the interval and awaits the in-flight run, if
// any. A nil receiver is a no-op, mirroring the TS `reconcileScheduler?.stop()`
// on a scheduler that was never created because startup failed early.
func (s *PeriodicReconcileScheduler) Stop() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.handle != nil {
		s.clearInterval(s.handle)
		s.handle = nil
	}
	inFlight := s.inFlight
	s.mu.Unlock()
	if inFlight != nil {
		<-inFlight
	}
	return nil
}

// tick is the interval callback: it skips while a run is in flight.
func (s *PeriodicReconcileScheduler) tick() {
	s.mu.Lock()
	if s.inFlight != nil {
		s.mu.Unlock()
		return
	}
	done := make(chan struct{})
	s.inFlight = done
	s.mu.Unlock()

	go func() {
		// The guard clears before the run is reported as settled, exactly like the
		// TS `.finally` that runs inside the promise chain stop() awaits.
		defer func() {
			s.mu.Lock()
			s.inFlight = nil
			s.mu.Unlock()
		}()
		defer close(done)
		if err := s.run(); err != nil {
			fmt.Fprintf(os.Stderr, "Herdsman periodic reconcile failed: %v\n", err)
		}
	}()
}

// ---------------------------------------------------------------------------
// Daemon root service
// ---------------------------------------------------------------------------

// DaemonServiceOptions mirrors the input object of runObservabilityDaemonService.
// A nil field means "use the production implementation", exactly like the TS
// `?? default` fallbacks. Optional numbers are pointers so an explicit 0 stays
// distinguishable from an omitted key (`??` keeps 0).
type DaemonServiceOptions struct {
	// ApplyMigrations overrides the migration replay.
	ApplyMigrations func(sqlite *sql.DB) error
	// ConnectSocket overrides the "is this socket live" probe the server start
	// performs before it unlinks a stale socket.
	ConnectSocket func(socketPath string) (bool, error)
	// Environment is the environment resolveRuntime reads. Nil means the process
	// environment, mirroring the TS default.
	Environment map[string]string
	// Exit overrides process.exit.
	Exit func(code int)
	// OpenSqlite overrides the SQLite open.
	OpenSqlite func(path string) (*sql.DB, error)
	// PID overrides process.pid; HasPID keeps an explicit 0 distinct from an
	// omitted key.
	PID    int
	HasPID bool
	// NewReconciler overrides the AgentEventReconciler the service builds. The TS
	// suite reaches the same seam by spying on AgentEventReconciler.prototype; a
	// Go seam is the equivalent, and the production default is the real
	// constructor.
	NewReconciler func(options AgentEventReconcilerOptions) ReconcileRunner
	// ReconcileClearInterval / ReconcileSetInterval override the scheduler's
	// interval seams.
	ReconcileClearInterval func(handle any)
	ReconcileSetInterval   func(callback func(), delayMs int64) any
	// ReconcileIntervalMs overrides the reconcile cadence.
	ReconcileIntervalMs *int64
	// SessionList overrides the Herdr session list runner.
	SessionList herdr.HerdrSessionListRunner
	// ShutdownBudgetMs overrides the shutdown budget.
	ShutdownBudgetMs *int64
	// SignalTarget overrides the SIGINT/SIGTERM registration seam.
	SignalTarget DaemonSignalTarget
}

// DaemonSignalTarget mirrors the TS `signalTarget` seam: the object whose
// `once`/`off` the service registers on. The production target is the process
// itself; tests inject one they can fire by hand.
type DaemonSignalTarget interface {
	// Once arms handler for the next SIGINT or SIGTERM and returns the function
	// that disarms it again, covering both `signalTarget.once(event, handler)`
	// calls and the `off` detachSignalHandlers performs.
	Once(handler func(signalName string)) (off func())
}

// ReconcileRunner is the slice of AgentEventReconciler the daemon drives: the
// startup reconcile, the periodic reconcile and nothing else.
type ReconcileRunner interface {
	Reconcile(options ReconcileOptions) (AgentEventReconcileResult, error)
}

// processSignalTarget is the production DaemonSignalTarget: the OS signals
// delivered to this process.
type processSignalTarget struct{}

// Once mirrors the TS pair of `once` registrations.
func (processSignalTarget) Once(handler func(signalName string)) func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	stop := make(chan struct{})
	go func() {
		select {
		case received := <-signals:
			name := "SIGTERM"
			if sig, ok := received.(syscall.Signal); ok && sig == syscall.SIGINT {
				name = "SIGINT"
			}
			handler(name)
		case <-stop:
		}
	}()
	return func() {
		signal.Stop(signals)
		close(stop)
	}
}

// RunObservabilityDaemonService mirrors runObservabilityDaemonService: it prepares
// the data directories, takes the instance lock, opens and migrates the database,
// wires every daemon service, starts the RPC server, runs the startup reconcile
// and drain, arms the periodic reconcile scheduler and starts the watch manager.
//
// It returns when startup has finished. On a startup failure it undoes what it
// started and returns the error; on success the daemon keeps running on its own
// goroutines until a signal drives the shutdown path (which calls the injected
// Exit), exactly like the TS promise that only settles when the process exits.
func RunObservabilityDaemonService(options *DaemonServiceOptions) error {
	var opts DaemonServiceOptions
	if options != nil {
		opts = *options
	}

	environment := opts.Environment
	if environment == nil {
		environment = processEnvironment()
	}

	runtime, err := config.ResolveRuntime(&config.RuntimeResolveOptions{Environment: environment})
	if err != nil {
		return err
	}
	applyEnvironment(runtime.Environment)
	if err := PrepareDaemonPaths(
		runtime.Paths.HomeDir, runtime.Paths.DBPath, runtime.Paths.SocketPath,
	); err != nil {
		return err
	}

	currentPid := os.Getpid()
	if opts.HasPID {
		currentPid = opts.PID
	}

	// The TS entrypoint can clear this variable from any code path because it
	// is single-threaded. In Go the graceful path (stop) and the forced path
	// (onSecondSignal) are separate signal handlers that may be invoked from
	// different goroutines, so the "call once, then clear" idiom needs its own
	// mutex or the release runs twice / races on the variable itself.
	var instanceLockMu sync.Mutex
	var releaseInstanceLock func()
	releaseInstanceLockIfHeld := func() {
		instanceLockMu.Lock()
		defer instanceLockMu.Unlock()
		if releaseInstanceLock != nil {
			releaseInstanceLock()
			releaseInstanceLock = nil
		}
	}

	// The bare daemon entrypoint (herdsman-daemon.js) takes an instance lock so
	// two daemon processes cannot share one HERDSMAN_HOME. It is acquired before
	// the SQLite database is opened: a second or orphaned daemon is rejected
	// while it has not touched (opened, migrated, or locked) the database at all.
	// The lock is released on graceful stop and on every startup failure path.
	// The assignment below runs before any handler goroutine exists, so it needs
	// no lock of its own.
	releaseInstanceLock, err = AcquireDaemonLock(
		DaemonInstanceLockPath(runtime.Paths.PIDPath),
		&DaemonProcessDependencies{PID: currentPid, HasPID: true},
	)
	if err != nil {
		return err
	}

	openSqliteImpl := opts.OpenSqlite
	if openSqliteImpl == nil {
		openSqliteImpl = db.OpenSqlite
	}
	applyMigrationsImpl := opts.ApplyMigrations
	if applyMigrationsImpl == nil {
		applyMigrationsImpl = db.ApplyMigrations
	}

	sqlite, err := openSqliteImpl(runtime.Paths.DBPath)
	if err != nil {
		return err
	}
	// The database and its WAL/SHM sidecars are tightened to 0600 once the file
	// exists. PrepareDaemonPaths already ran this loop, but before the open, where
	// a database that does not exist yet is skipped — so a freshly created file is
	// chmodded here, exactly like the TS loop that follows openSqlite.
	for _, path := range []string{
		runtime.Paths.DBPath,
		runtime.Paths.DBPath + "-wal",
		runtime.Paths.DBPath + "-shm",
	} {
		if _, statErr := os.Stat(path); statErr != nil {
			continue
		}
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			return chmodErr
		}
	}
	if err := applyMigrationsImpl(sqlite); err != nil {
		return err
	}

	herdrSessions := db.NewHerdrSessionStore(sqlite)
	herdrWorkspaces := db.NewHerdrWorkspaceStore(sqlite)
	agentEvents, err := db.NewAgentEventStore(sqlite)
	if err != nil {
		return err
	}
	agents := db.NewAgentStore(sqlite, agentEvents)
	agentHistoryCache := db.NewAgentHistoryCacheStore(sqlite)
	agentContextSnapshots := db.NewAgentContextSnapshotStore(sqlite)
	agentOrchestratorScopes := db.NewAgentOrchestratorScopeStore(sqlite)
	statusEventPlans := db.NewStatusEventPlanStore(sqlite)
	history := agenthistory.CreateAgentHistoryService(agenthistory.AgentHistoryServiceOptions{
		Cache: agentHistoryCache,
	})
	// internal/agenthistory imports internal/observability, so the history
	// service reaches the observability layer through this adapter.
	historyAdapter := newAgentHistoryAdapter(history)
	context := observability.NewAgentContextService(struct {
		History observability.AgentHistoryServiceLike
		Stores  struct {
			AgentContextSnapshots observability.AgentContextSnapshotStoreLike
			Agents                observability.AgentContextAgentStoreLike
		}
	}{
		History: historyAdapter,
		Stores: struct {
			AgentContextSnapshots observability.AgentContextSnapshotStoreLike
			Agents                observability.AgentContextAgentStoreLike
		}{
			AgentContextSnapshots: agentContextSnapshots,
			Agents:                agents,
		},
	})
	orchestrator := observability.NewAgentOrchestratorService(struct {
		AgentEvents observability.AgentEventStoreLike
		Agents      observability.AgentStoreLike
		Scopes      observability.AgentOrchestratorScopeStoreLike
	}{
		AgentEvents: agentEvents,
		Agents:      agents,
		Scopes:      agentOrchestratorScopes,
	})
	turnCompletions := observability.NewTurnCompletionRegistry()

	// The TS daemon installs unhandledRejection / uncaughtException handlers that
	// only log. Go has no equivalent global hook (an unrecovered panic in a
	// goroutine ends the process), so the port has nothing to attach or detach;
	// every goroutine the daemon starts reports its own failures instead.

	var publishEvent func(event observability.AgentEventRecord) = func(observability.AgentEventRecord) {}

	// One controller drives every cancellable wait in the daemon. It is aborted at
	// the start of stop(), so in-flight status waits (history-advance window, turn
	// waits) and watcher loops end immediately instead of running out their own
	// 30s / 12s windows and pushing the daemon past TimeoutStopSec.
	shutdownController := observability.NewShutdownSignal()

	statusPlans := config.ResolveStatusPlansConfig(runtime.Config, runtime.Environment)
	index, err := observability.NewAgentIndexService(observability.AgentIndexServiceOptions{
		Context: context,
		History: historyAdapter,
		// The closure is reassigned once the server exists, so the index pushes
		// into the live server from the very first event.
		OnAgentEvent:   func(event observability.AgentEventRecord) { publishEvent(event) },
		SessionPaths:   historyAdapter,
		ShutdownSignal: shutdownController,
		StatusPlansConfig: &observability.StatusPlansConfig{
			BackoffFactor:    statusPlans.BackoffFactor,
			InitialBackoffMs: statusPlans.InitialBackoffMs,
			MaxBackoffMs:     statusPlans.MaxBackoffMs,
			TotalWindowMs:    statusPlans.TotalWindowMs,
		},
		Stores: observability.AgentIndexServiceStores{
			AgentContextSnapshots:   agentContextSnapshots,
			AgentEvents:             agentEvents,
			AgentHistoryCache:       agentHistoryCache,
			AgentOrchestratorScopes: agentOrchestratorScopes,
			Agents:                  agents,
			HerdrSessions:           herdrSessions,
			HerdrWorkspaces:         herdrWorkspaces,
			Sqlite:                  sqlite,
			StatusEventPlans:        statusEventPlans,
		},
		TurnCompletions: turnCompletions,
	})
	if err != nil {
		return err
	}

	sessionList := opts.SessionList
	if sessionList == nil {
		sessionList = herdr.CreateHerdrSessionListRunner(herdr.HerdrSessionListRunnerOptions{
			Env: environmentEntries(runtime.Environment),
		})
	}

	newReconciler := opts.NewReconciler
	if newReconciler == nil {
		newReconciler = func(options AgentEventReconcilerOptions) ReconcileRunner {
			return NewAgentEventReconciler(options)
		}
	}

	var connectedTerminal func(input ConnectedTerminalInput) bool = func(ConnectedTerminalInput) bool {
		return false
	}
	reconciler := newReconciler(AgentEventReconcilerOptions{
		AgentHistoryCache: agentHistoryCache,
		ConnectedTerminal: connectedTerminal,
		Events:            agentEvents,
		Scopes:            agentOrchestratorScopes,
		SessionList:       sessionList,
		StatusEventPlans:  statusEventPlans,
	})

	serverOptions := ObservabilityRpcServerOptions{
		Context:      context,
		History:      historyAdapter,
		Orchestrator: orchestrator,
		RegisterPiSessionRef: func(
			input observability.RegisterPiSessionRefInput,
		) (*observability.PiSessionRefRegistrationResult, error) {
			return index.RegisterPiSessionRef(input)
		},
		SocketPath: runtime.Paths.SocketPath,
		Stores: ObservabilityAgentStores{
			AgentEvents:     agentEvents,
			Agents:          agents,
			HerdrSessions:   herdrSessions,
			HerdrWorkspaces: herdrWorkspaces,
		},
		TurnCompletions: turnCompletions,
	}
	if opts.ConnectSocket != nil {
		serverOptions.ConnectSocket = opts.ConnectSocket
	}
	server := NewObservabilityRpcServer(serverOptions)
	connectedTerminal = func(input ConnectedTerminalInput) bool {
		return server.IsTerminalConnected(input)
	}
	publishEvent = func(event observability.AgentEventRecord) { server.PublishAgentEvent(event) }

	watchManager := NewHerdrSessionWatchManager(HerdrSessionWatchOptions{
		Agents:        agents,
		HerdrSessions: herdrSessions,
		Index:         index,
		OnAgentContextChanged: func(scope observability.AgentScope) {
			server.PublishAgentContext(scope)
		},
		OnAgentEvent: func(event observability.AgentEventRecord) {
			server.PublishAgentEvent(event)
		},
		OnAgentIndexRefreshed: func(
			refreshed []observability.AgentIndexRecord,
			herdrSessionName string,
		) {
			// The TS callback returns the promise of reconcileAgentLocations
			// without awaiting it, so a failure surfaces as an unhandled
			// rejection the daemon logs. There is no such global hook here, so
			// the failure is reported at the call site instead of being
			// swallowed.
			if err := server.ReconcileAgentLocations(AgentLocationReconcile{
				Agents:           refreshed,
				HerdrSessionName: herdrSessionName,
			}); err != nil {
				fmt.Fprintf(os.Stderr,
					"Herdsman daemon agent location reconcile failed"+
						" (herdrSessionName=%s): %v\n", herdrSessionName, err)
			}
		},
		SessionList:    sessionList,
		ShutdownSignal: shutdownController.Done(),
	})

	signalTarget := opts.SignalTarget
	if signalTarget == nil {
		signalTarget = processSignalTarget{}
	}
	exitFunc := opts.Exit
	if exitFunc == nil {
		exitFunc = os.Exit
	}
	shutdownBudgetMs := int64(ShutdownBudgetMs)
	if opts.ShutdownBudgetMs != nil {
		shutdownBudgetMs = *opts.ShutdownBudgetMs
	}

	// Register signal handlers before the first step that can block (server start,
	// reconcile): once the pid file is written the daemon must always clean it up
	// on a graceful shutdown, even if startup is still in progress.
	var reconcileScheduler *PeriodicReconcileScheduler

	// stopping, exitRequested and the live signal registration are touched from
	// the signal goroutine and from the startup path, so they share one mutex.
	// The TS keeps them in closure variables, which its single event loop makes
	// atomic.
	var lifecycleMu sync.Mutex
	stopping := false
	exitRequested := false
	var signalOff func()

	// The forced-exit path can win the race against the graceful one; whichever
	// gets there first decides the exit code, and the loser must not exit again.
	requestExit := func(code int) {
		lifecycleMu.Lock()
		if exitRequested {
			lifecycleMu.Unlock()
			return
		}
		exitRequested = true
		lifecycleMu.Unlock()
		exitFunc(code)
	}

	attachSignalHandlers := func(handler func()) {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		if signalOff != nil {
			signalOff()
		}
		signalOff = signalTarget.Once(func(string) { handler() })
	}
	detachSignalHandlers := func() {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		if signalOff != nil {
			signalOff()
			signalOff = nil
		}
	}

	// A second SIGTERM/SIGINT means the first shutdown is not converging (or the
	// supervisor is about to SIGKILL). Re-arming through these handlers keeps that
	// signal away from the OS default action, which would kill the process before
	// the finally cleanup and leave the pid file, socket and instance lock behind.
	onSecondSignal := func() {
		fmt.Fprintf(os.Stderr,
			"Herdsman daemon received a second shutdown signal; forcing exit (pid=%d)\n",
			currentPid)
		RemoveDaemonPidFile(runtime.Paths.PIDPath, currentPid)
		removeDaemonSocket(runtime.Paths.SocketPath)
		releaseInstanceLockIfHeld()
		// Exit 0 on purpose: this is still an intentional stop, and a non-zero code
		// would make `Restart=on-failure` pull the daemon back up.
		requestExit(0)
	}

	stop := func() {
		lifecycleMu.Lock()
		if stopping {
			lifecycleMu.Unlock()
			return
		}
		stopping = true
		lifecycleMu.Unlock()
		detachSignalHandlers()
		attachSignalHandlers(onSecondSignal)

		startedAt := time.Now()
		exitCode := 0
		fmt.Fprintf(os.Stdout,
			"Herdsman daemon shutdown starting (budgetMs=%d pid=%d)\n",
			shutdownBudgetMs, currentPid)
		if err := runShutdownSequence(shutdownSequenceInput{
			budgetMs:   shutdownBudgetMs,
			index:      index,
			reconciler: reconcileScheduler,
			server:     server,
			sqlite:     sqlite,
			startedAt:  startedAt,
			watch:      watchManager,
			signal:     shutdownController,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Herdsman daemon shutdown failed: %v\n", err)
			exitCode = 1
		}
		// The TS finally block: the pid file and the instance lock are released
		// whatever the sequence did, so a failed or wedged shutdown still leaves
		// nothing behind for the next daemon.
		RemoveDaemonPidFile(runtime.Paths.PIDPath, currentPid)
		releaseInstanceLockIfHeld()
		fmt.Fprintf(os.Stdout,
			"Herdsman daemon shutdown finished (elapsedMs=%d exitCode=%d)\n",
			time.Since(startedAt).Milliseconds(), exitCode)
		requestExit(exitCode)
	}

	attachSignalHandlers(stop)

	startupFailure := func(cause error) {
		detachSignalHandlers()
		// Same abort as the graceful path: a half-started watcher must not keep its
		// socket (and the failing startup) alive.
		shutdownController.Abort()
		_ = reconcileScheduler.Stop()
		_ = watchManager.Stop()
		_ = server.Stop()
		RemoveDaemonPidFile(runtime.Paths.PIDPath, currentPid)
		releaseInstanceLockIfHeld()
		_ = sqlite.Close()
	}

	if err := server.Start(); err != nil {
		startupFailure(err)
		return err
	}
	WriteDaemonPidFile(runtime.Paths.PIDPath, currentPid)
	if _, err := reconciler.Reconcile(ReconcileOptions{
		ReleaseStaleOwners: boolPointer(false),
	}); err != nil {
		startupFailure(err)
		return err
	}
	if err := index.DrainPendingPlans(&observability.DrainPendingPlansOptions{
		Reason: "startup",
	}); err != nil {
		startupFailure(err)
		return err
	}
	// Periodic reconcile keeps the 7-day/30-day TTLs converging on long-running
	// daemons; the in-flight guard inside the scheduler prevents overlapping runs.
	schedulerOptions := PeriodicReconcileSchedulerOptions{
		Run: func() error {
			if _, err := reconciler.Reconcile(ReconcileOptions{
				ReleaseStaleOwners: boolPointer(false),
			}); err != nil {
				return err
			}
			// Long-lived daemons must also keep retrying runtime-failed status
			// event plans: drainPendingPlans is idempotent (pending/running rows
			// only, per-agent serial, attempts capped) and the periodic cadence
			// prevents a hot loop, so piggybacking it on the reconcile cycle
			// completes the retry path that the startup-only drain leaves open.
			return index.DrainPendingPlans(&observability.DrainPendingPlansOptions{
				Reason: "periodic",
			})
		},
	}
	if opts.ReconcileClearInterval != nil {
		schedulerOptions.ClearInterval = opts.ReconcileClearInterval
	}
	if opts.ReconcileSetInterval != nil {
		schedulerOptions.SetInterval = opts.ReconcileSetInterval
	}
	schedulerOptions.IntervalMs = opts.ReconcileIntervalMs
	reconcileScheduler = NewPeriodicReconcileScheduler(schedulerOptions)
	reconcileScheduler.Start()
	if err := watchManager.Start(); err != nil {
		startupFailure(err)
		return err
	}
	fmt.Fprintf(os.Stdout, "Herdsman daemon listening on %s\n", runtime.Paths.SocketPath)
	return nil
}

// shutdownSequenceInput carries what the TS stop() closure captures.
type shutdownSequenceInput struct {
	budgetMs   int64
	index      *observability.AgentIndexService
	reconciler *PeriodicReconcileScheduler
	server     *ObservabilityRpcServer
	signal     *observability.ShutdownSignal
	sqlite     *sql.DB
	startedAt  time.Time
	watch      *HerdrSessionWatchManager
}

// runShutdownSequence is the try body of the TS stop(): the in-flight aborts, the
// four budgeted steps and the database close. The returned error is the TS catch
// branch; the caller's finally cleanup runs either way.
func runShutdownSequence(input shutdownSequenceInput) error {
	// Abort before draining: waits that are already in flight end now instead of
	// holding the drain open for their own window.
	input.index.StopWaitingHistoryRetries()
	input.signal.Abort()
	fmt.Fprintf(os.Stdout,
		"Herdsman daemon shutdown aborted in-flight waits (elapsedMs=%d)\n",
		time.Since(input.startedAt).Milliseconds())

	if err := RunShutdownSteps([]ShutdownStep{
		{Name: "index.drainInFlightPlans", Run: func() error {
			input.index.DrainInFlightPlans()
			return nil
		}},
		{Name: "reconcileScheduler.stop", Run: func() error {
			return input.reconciler.Stop()
		}},
		{Name: "watchManager.stop", Run: input.watch.Stop},
		{Name: "server.stop", Run: input.server.Stop},
	}, &ShutdownStepsOptions{BudgetMs: &input.budgetMs}); err != nil {
		return err
	}
	return input.sqlite.Close()
}

// removeDaemonSocket mirrors `rmSync(socketPath, { force: true })` inside a
// try/catch: the socket is removed when it is there, and any other failure is
// ignored because shutdown cleanup must never abort the sequence.
func removeDaemonSocket(socketPath string) {
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return
	}
}

// processEnvironment mirrors `process.env` as the map the runtime resolver reads.
func processEnvironment() map[string]string {
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		environment[key] = value
	}
	return environment
}

// applyEnvironment mirrors applyEnvironment: the resolved environment (process
// environment plus the .env file) is written back into the process environment,
// so every child (herdr, pi) and the discovery code that reads HOME see it. The
// TS loop also deletes the keys whose value is `undefined`; a Go map cannot hold
// an undefined value, so only the set half remains.
func applyEnvironment(environment map[string]string) {
	for key, value := range environment {
		_ = os.Setenv(key, value)
	}
}

// environmentEntries renders a map as the `KEY=value` slice the Herdr session
// list runner passes to its child process.
func environmentEntries(environment map[string]string) []string {
	entries := make([]string, 0, len(environment))
	for key, value := range environment {
		entries = append(entries, key+"="+value)
	}
	return entries
}

// DefaultSetInterval / DefaultClearInterval are the module-level setInterval /
// clearInterval the TS scheduler defaults to. The delay is int64 because the
// reconcile cadence is an int64 constant; the ticker itself is the same
// intervalHandle the RPC server's seams use.
