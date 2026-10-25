package daemon_test

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/config"
	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
)

// The port of test/unit/daemon-service.test.ts.
//
// Every case runs in its own t.TempDir() HERDSMAN_HOME, so the production home
// (/root/.herdsman), the installed CLI and the running herdsman.service are never
// touched. The TS suite reaches into the module graph with vi.spyOn and
// prototype mocks; the Go port uses the same seams the service already exposes
// (DaemonServiceOptions, the scheduler's interval doubles, the NewReconciler
// factory) plus a signal-target double, so no production code is patched.

// daemonWorkerEnv must match the constant process_manager_lock_test.go defines
// for the shared TestMain: it re-executes this test binary as a real daemon
// process on the HERDSMAN_HOME named by the value.
const daemonWorkerEnv = "HERDSMAN_GO_DAEMON_WORKER"

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// openDaemonTestHome creates the temp HERDSMAN_HOME one case runs in and returns
// the paths resolveRuntime derives from it, so the assertions name the same pid
// file, socket and database the service uses.
func openDaemonTestHome(t *testing.T) (string, config.RuntimePaths) {
	t.Helper()
	home := t.TempDir()
	previous, had := os.LookupEnv("HERDSMAN_HOME")
	t.Cleanup(func() {
		// The daemon writes the resolved environment back into the process
		// environment (mirroring applyEnvironment), so the suite restores it
		// instead of leaking one case's temp home into the next.
		if had {
			_ = os.Setenv("HERDSMAN_HOME", previous)
		} else {
			_ = os.Unsetenv("HERDSMAN_HOME")
		}
	})
	runtime, err := config.ResolveRuntime(&config.RuntimeResolveOptions{
		Environment: map[string]string{"HERDSMAN_HOME": home},
	})
	if err != nil {
		t.Fatalf("resolveRuntime: %v", err)
	}
	return home, runtime.Paths
}

// restoreUmask puts the process umask back: server.Start() sets 0o077 for the
// socket and leaves it in place (TS leaves process.umask the same way), which
// would otherwise leak into the other tests of this package.
func restoreUmask(t *testing.T) {
	t.Helper()
	previous := syscall.Umask(0o077)
	t.Cleanup(func() { _ = syscall.Umask(previous) })
}

func int64Ptr(value int64) *int64 { return &value }

func emptySessionList() ([]herdr.HerdrSessionListEntry, error) { return nil, nil }

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func requireFile(t *testing.T, path string) {
	t.Helper()
	if !fileExists(path) {
		t.Fatalf("expected %s to exist", path)
	}
}

func requireNoFile(t *testing.T, path string) {
	t.Helper()
	if fileExists(path) {
		t.Fatalf("expected %s to be gone", path)
	}
}

// waitFor polls predicate the way `vi.waitFor` does, so a goroutine that settles
// on a later turn does not make the case flaky.
func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not reached within 10s")
}

// ---------------------------------------------------------------------------
// console.log / console.warn capture
// ---------------------------------------------------------------------------

// logCapture swaps os.Stdout and os.Stderr for pipes and keeps what the daemon
// wrote, the way the TS suite spies on console. The pumps run for the whole
// capture window, so a line written by a goroutine that settles after the
// assertion's trigger is still observable.
type logCapture struct {
	mu          sync.Mutex
	stdoutLog   strings.Builder
	stderrLog   strings.Builder
	originalOut *os.File
	originalErr *os.File
	outReader   *os.File
	outWriter   *os.File
	errReader   *os.File
	errWriter   *os.File
	readers     sync.WaitGroup
	once        sync.Once
}

func startLogCapture(t *testing.T) *logCapture {
	t.Helper()
	capture := &logCapture{originalOut: os.Stdout, originalErr: os.Stderr}
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	errReader, errWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	capture.outReader, capture.outWriter = outReader, outWriter
	capture.errReader, capture.errWriter = errReader, errWriter
	os.Stdout = outWriter
	os.Stderr = errWriter
	capture.readers.Add(2)
	go capture.pump(outReader, &capture.stdoutLog)
	go capture.pump(errReader, &capture.stderrLog)
	t.Cleanup(capture.stop)
	return capture
}

func (c *logCapture) pump(reader *os.File, builder *strings.Builder) {
	defer c.readers.Done()
	buffer := make([]byte, 4096)
	for {
		read, err := reader.Read(buffer)
		if read > 0 {
			c.mu.Lock()
			builder.Write(buffer[:read])
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// stdout returns everything captured on the standard output so far.
func (c *logCapture) stdout() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdoutLog.String()
}

// stderr returns everything captured on the standard error so far.
func (c *logCapture) stderr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stderrLog.String()
}

// settle waits until ready holds on the captured output. The pump goroutines
// copy asynchronously, so a line written microseconds ago is not necessarily
// visible to a reader yet: polling with a deadline keeps the assertion honest
// where a fixed sleep would only make the flake rarer.
func (c *logCapture) settle(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ready() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 5s waiting for %s\nstdout = %q\nstderr = %q",
				description, c.stdout(), c.stderr())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (c *logCapture) stop() {
	c.once.Do(func() {
		os.Stdout = c.originalOut
		os.Stderr = c.originalErr
		_ = c.outWriter.Close()
		_ = c.errWriter.Close()
		c.readers.Wait()
		_ = c.outReader.Close()
		_ = c.errReader.Close()
	})
}

// fieldValue reads one `key=value` field out of a captured log line.
func fieldValue(line, key string) string {
	for _, field := range strings.Fields(line) {
		if value, ok := strings.CutPrefix(field, key+"="); ok {
			return value
		}
	}
	return ""
}

// loggedStepOrder returns the `step=` values of every captured line that starts
// with marker, in the order they were written. The trailing `)` of the
// `key=value)` group is not part of the value.
func loggedStepOrder(log, marker string) []string {
	steps := []string{}
	for _, line := range strings.Split(log, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, marker) {
			continue
		}
		steps = append(steps, strings.TrimSuffix(fieldValue(trimmed, "step"), ")"))
	}
	return steps
}

// ---------------------------------------------------------------------------
// Signal target double
// ---------------------------------------------------------------------------

// signalListener is one registered handler, kept behind a pointer so the
// double can remove exactly the listener it handed out (Go function values are
// not comparable).
type signalListener struct {
	handler func(signalName string)
}

// signalTargetDouble mirrors the TS createSignalTarget: `once` appends a
// one-shot listener, `off` removes it again, and emit runs whatever is
// registered for that signal.
type signalTargetDouble struct {
	mu        sync.Mutex
	listeners map[string][]*signalListener
}

func newSignalTargetDouble() *signalTargetDouble {
	return &signalTargetDouble{listeners: map[string][]*signalListener{}}
}

// Once mirrors `signalTarget.once(event, listener)`: one listener for both
// SIGINT and SIGTERM, plus the function that disarms it again.
func (d *signalTargetDouble) Once(handler func(signalName string)) func() {
	listener := &signalListener{handler: handler}
	d.mu.Lock()
	d.listeners["SIGINT"] = append(d.listeners["SIGINT"], listener)
	d.listeners["SIGTERM"] = append(d.listeners["SIGTERM"], listener)
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.listeners["SIGINT"] = removeSignalListener(d.listeners["SIGINT"], listener)
		d.listeners["SIGTERM"] = removeSignalListener(d.listeners["SIGTERM"], listener)
	}
}

func removeSignalListener(
	listeners []*signalListener,
	target *signalListener,
) []*signalListener {
	remaining := make([]*signalListener, 0, len(listeners))
	for _, candidate := range listeners {
		if candidate == target {
			continue
		}
		remaining = append(remaining, candidate)
	}
	return remaining
}

// emit mirrors the TS emit: it takes the listeners registered for the event and
// runs them. A graceful shutdown blocks for as long as its budget allows, so the
// caller decides whether to wait for the returned channel.
func (d *signalTargetDouble) emit(event string) chan struct{} {
	d.mu.Lock()
	taken := d.listeners[event]
	d.listeners[event] = nil
	d.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, listener := range taken {
			listener.handler(event)
		}
	}()
	return done
}

// waitForListener blocks until a listener is registered for the event again. The
// TS `await emit(...)` gets that guarantee for free, because the handler's
// synchronous prefix (detach + re-arm) has run by the time the awaited promise
// resolves; the Go handler runs on its own goroutine, so the second-signal case
// waits for the re-arm explicitly.
func (d *signalTargetDouble) waitForListener(t *testing.T, event string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		count := len(d.listeners[event])
		d.mu.Unlock()
		if count > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no %s listener was registered within 10s", event)
}

// ---------------------------------------------------------------------------
// describe("daemon service")
// ---------------------------------------------------------------------------

// The TS case walks up from the compiled module directory looking for
// drizzle/meta/_journal.json, because the TS daemon reads the migrations off
// disk relative to its own module. The Go port embeds them
// (`//go:embed all:migrations` in internal/db), so the journal travels inside the
// binary and the directory the daemon is launched from cannot change what is
// replayed. This asserts that embedded set is present and produces the
// bookkeeping table drizzle would have written.
func TestDaemonServiceFindsMigrationsFromThePackageRoot(t *testing.T) {
	dir := t.TempDir()
	sqlite, err := db.OpenSqlite(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("openSqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	if err := db.ApplyMigrations(sqlite); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}

	var applied int
	if err := sqlite.QueryRow(
		"select count(*) from __drizzle_migrations",
	).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied == 0 {
		t.Fatal("no migration was replayed, so the embedded journal is missing")
	}
	var agents int
	if err := sqlite.QueryRow("select count(*) from agent_events").Scan(&agents); err != nil {
		t.Fatalf("count agent_events: %v", err)
	}
}

// ---------------------------------------------------------------------------
// describe("periodic reconcile scheduling")
// ---------------------------------------------------------------------------

func TestPeriodicReconcileFiresOnConfiguredCadence(t *testing.T) {
	var tick func()
	runs := atomic.Int64{}
	instance := daemon.NewPeriodicReconcileScheduler(daemon.PeriodicReconcileSchedulerOptions{
		IntervalMs: int64Ptr(daemon.ReconcileIntervalMs),
		Run: func() error {
			runs.Add(1)
			return nil
		},
		SetInterval: func(callback func(), delayMs int64) any {
			tick = callback
			return 1
		},
		ClearInterval: func(any) {},
	})

	instance.Start()
	if runs.Load() != 0 {
		t.Fatalf("runs before the first tick = %d, want 0", runs.Load())
	}
	tick()
	waitFor(t, func() bool { return runs.Load() == 1 })
	tick()
	waitFor(t, func() bool { return runs.Load() == 2 })
	if err := instance.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestPeriodicReconcileSkipsTicksWhileInFlight(t *testing.T) {
	var tick func()
	release := make(chan struct{})
	runs := atomic.Int64{}
	instance := daemon.NewPeriodicReconcileScheduler(daemon.PeriodicReconcileSchedulerOptions{
		IntervalMs: int64Ptr(daemon.ReconcileIntervalMs),
		Run: func() error {
			runs.Add(1)
			<-release
			return nil
		},
		SetInterval: func(callback func(), delayMs int64) any {
			tick = callback
			return 1
		},
		ClearInterval: func(any) {},
	})

	instance.Start()
	tick()
	waitFor(t, func() bool { return runs.Load() == 1 })
	// Two more ticks while the first run is still in flight: both are skipped.
	tick()
	tick()
	time.Sleep(20 * time.Millisecond)
	if runs.Load() != 1 {
		t.Fatalf("runs = %d, want 1 (the in-flight ticks must be skipped)", runs.Load())
	}

	// Settle the in-flight run; the guard clears when it settles, so the next tick
	// starts the second run. Poll instead of assuming one settle turn was enough.
	close(release)
	waitFor(t, func() bool {
		tick()
		time.Sleep(2 * time.Millisecond)
		return runs.Load() == 2
	})
	if err := instance.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestPeriodicReconcileStopClearsIntervalAndAwaitsInFlight(t *testing.T) {
	var tick func()
	cleared := atomic.Int64{}
	release := make(chan struct{})
	started := atomic.Bool{}
	settled := atomic.Bool{}
	instance := daemon.NewPeriodicReconcileScheduler(daemon.PeriodicReconcileSchedulerOptions{
		IntervalMs: int64Ptr(daemon.ReconcileIntervalMs),
		Run: func() error {
			started.Store(true)
			<-release
			settled.Store(true)
			return nil
		},
		SetInterval: func(callback func(), delayMs int64) any {
			tick = callback
			return 1
		},
		ClearInterval: func(any) { cleared.Add(1) },
	})

	instance.Start()
	tick()
	waitFor(t, func() bool { return started.Load() })

	stopped := make(chan error, 1)
	go func() { stopped <- instance.Stop() }()
	select {
	case <-stopped:
		t.Fatal("stop returned before the in-flight run settled")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stop did not return after the in-flight run settled")
	}
	if cleared.Load() != 1 {
		t.Fatalf("clearInterval calls = %d, want 1", cleared.Load())
	}
	if !settled.Load() {
		t.Fatal("stop returned before the in-flight run finished")
	}
}

func TestPeriodicReconcileStartIsIdempotentAndRejectedRunKeepsCadence(t *testing.T) {
	var tick func()
	setCalls := atomic.Int64{}
	runs := atomic.Int64{}
	instance := daemon.NewPeriodicReconcileScheduler(daemon.PeriodicReconcileSchedulerOptions{
		IntervalMs: int64Ptr(daemon.ReconcileIntervalMs),
		Run: func() error {
			runs.Add(1)
			return errors.New("boom")
		},
		SetInterval: func(callback func(), delayMs int64) any {
			setCalls.Add(1)
			tick = callback
			return 1
		},
		ClearInterval: func(any) {},
	})

	instance.Start()
	instance.Start()
	if setCalls.Load() != 1 {
		t.Fatalf("setInterval calls = %d, want 1 (start must be idempotent)", setCalls.Load())
	}

	capture := startLogCapture(t)
	tick()
	waitFor(t, func() bool {
		return runs.Load() >= 1 && strings.Contains(capture.stderr(), "Herdsman periodic reconcile failed")
	})
	if !strings.Contains(capture.stderr(), "boom") {
		t.Fatalf("stderr = %q, want the rejected run to be reported", capture.stderr())
	}

	// The guard clears after the rejection, so the cadence continues.
	waitFor(t, func() bool {
		tick()
		time.Sleep(2 * time.Millisecond)
		return runs.Load() == 2
	})
	// Stopping the scheduler first: the capture has to stay in place for as long
	// as a tick goroutine can still write to it, and Stop() awaits the in-flight
	// run (whose warning is written before it settles).
	if err := instance.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// ---------------------------------------------------------------------------
// describe("shutdown budget")
// ---------------------------------------------------------------------------

func TestShutdownBudgetStaysClearOfTimeoutStopSec(t *testing.T) {
	// stop() runs four steps; once the budget is spent each remaining step still
	// gets ShutdownMinStepMs, so this is the sequence's worst case.
	const steps = 4
	if daemon.ShutdownBudgetMs+(steps-1)*daemon.ShutdownMinStepMs >= 10_000 {
		t.Fatalf(
			"worst case shutdown = %dms, want it below the unit's 10s TimeoutStopSec",
			daemon.ShutdownBudgetMs+(steps-1)*daemon.ShutdownMinStepMs)
	}
	// The plan drain grace runs inside the watchManager.stop() step.
	if daemon.PlanDrainGraceMs > daemon.ShutdownBudgetMs {
		t.Fatalf("PlanDrainGraceMs = %d, want at most %d",
			daemon.PlanDrainGraceMs, daemon.ShutdownBudgetMs)
	}
}

func TestShutdownBudgetBoundsWedgedStepAndContinuesInOrder(t *testing.T) {
	ran := []string{}
	blocked := make(chan struct{})
	capture := startLogCapture(t)

	startedAt := time.Now()
	err := daemon.RunShutdownSteps([]daemon.ShutdownStep{
		{Name: "drain", Run: func() error {
			<-blocked
			return nil
		}},
		{Name: "scheduler", Run: func() error {
			ran = append(ran, "scheduler")
			return nil
		}},
		{Name: "server", Run: func() error {
			ran = append(ran, "server")
			return nil
		}},
	}, &daemon.ShutdownStepsOptions{BudgetMs: int64Ptr(200)})
	elapsedMs := time.Since(startedAt).Milliseconds()
	if err != nil {
		t.Fatalf("runShutdownSteps: %v", err)
	}

	if len(ran) != 2 || ran[0] != "scheduler" || ran[1] != "server" {
		t.Fatalf("ran = %v, want [scheduler server]", ran)
	}
	if elapsedMs < 200 || elapsedMs >= 2_000 {
		t.Fatalf("elapsedMs = %d, want the 200ms budget respected", elapsedMs)
	}
	capture.settle(t, "the timed-out step to be reported and all three steps to start", func() bool {
		warning := capture.stderr()
		return strings.Contains(warning, "Herdsman daemon shutdown step timed out; continuing") &&
			strings.Contains(warning, "step=drain") &&
			strings.Contains(warning, "budgetMs=200") &&
			len(loggedStepOrder(capture.stdout(), "Herdsman daemon shutdown step starting (")) == 3
	})
	warning := capture.stderr()
	if !strings.Contains(warning, "Herdsman daemon shutdown step timed out; continuing") ||
		!strings.Contains(warning, "step=drain") ||
		!strings.Contains(warning, "budgetMs=200") {
		t.Fatalf("stderr = %q, want the timed-out step to be reported", warning)
	}
	starting := loggedStepOrder(capture.stdout(), "Herdsman daemon shutdown step starting (")
	if len(starting) != 3 || starting[0] != "drain" ||
		starting[1] != "scheduler" || starting[2] != "server" {
		t.Fatalf("starting steps = %v, want [drain scheduler server]", starting)
	}
}

func TestShutdownBudgetPropagatesGenuineStepFailure(t *testing.T) {
	ran := []string{}
	err := daemon.RunShutdownSteps([]daemon.ShutdownStep{
		{Name: "boom", Run: func() error {
			return errors.New("shutdown step exploded")
		}},
		{Name: "later", Run: func() error {
			ran = append(ran, "later")
			return nil
		}},
	}, &daemon.ShutdownStepsOptions{BudgetMs: int64Ptr(200)})
	if err == nil || !strings.Contains(err.Error(), "shutdown step exploded") {
		t.Fatalf("runShutdownSteps error = %v, want the step failure to propagate", err)
	}
	if len(ran) != 0 {
		t.Fatalf("ran = %v, want no later step to run", ran)
	}
}

func TestShutdownBudgetKeepsSigtermInsideBudget(t *testing.T) {
	home, paths := openDaemonTestHome(t)
	restoreUmask(t)
	capture := startLogCapture(t)

	exitCodes := []int{}
	signals := newSignalTargetDouble()
	var sessionListCalls atomic.Int64
	var tick func()

	err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		Environment: map[string]string{"HERDSMAN_HOME": home},
		Exit:        func(code int) { exitCodes = append(exitCodes, code) },
		PID:         2468,
		HasPID:      true,
		// Call 1 is the startup reconcile, call 2 the watcher rescan. Every call
		// after that never settles, wedging the periodic reconcile run and with it
		// the reconcileScheduler.stop() shutdown step: without the budget, stop()
		// would never return and systemd would SIGKILL the daemon.
		SessionList: func() ([]herdr.HerdrSessionListEntry, error) {
			if sessionListCalls.Add(1) <= 2 {
				return nil, nil
			}
			blocked := make(chan struct{})
			<-blocked
			return nil, nil
		},
		ReconcileClearInterval: func(any) {},
		ReconcileIntervalMs:    int64Ptr(1),
		ReconcileSetInterval: func(callback func(), delayMs int64) any {
			tick = callback
			return 1
		},
		ShutdownBudgetMs: int64Ptr(300),
		SignalTarget:     signals,
	})
	if err != nil {
		t.Fatalf("runObservabilityDaemonService: %v", err)
	}
	if got := readFileString(t, paths.PIDPath); got != "2468\n" {
		t.Fatalf("pid file = %q, want %q", got, "2468\n")
	}

	tick()
	waitFor(t, func() bool { return sessionListCalls.Load() >= 3 })

	startedAt := time.Now()
	<-signals.emit("SIGTERM")
	elapsedMs := time.Since(startedAt).Milliseconds()

	// Far below the unit's 10s TimeoutStopSec even though a step is wedged.
	if elapsedMs >= 3_000 {
		t.Fatalf("shutdown took %dms, want it below 3000ms", elapsedMs)
	}
	if len(exitCodes) != 1 || exitCodes[0] != 0 {
		t.Fatalf("exitCodes = %v, want [0]", exitCodes)
	}
	capture.settle(t, "the wedged reconcileScheduler.stop step to be reported and the other steps to finish", func() bool {
		warning := capture.stderr()
		return strings.Contains(warning, "Herdsman daemon shutdown step timed out; continuing") &&
			strings.Contains(warning, "step=reconcileScheduler.stop") &&
			len(loggedStepOrder(capture.stdout(), "Herdsman daemon shutdown step finished (")) == 3
	})
	if !strings.Contains(capture.stderr(), "Herdsman daemon shutdown step timed out; continuing") ||
		!strings.Contains(capture.stderr(), "step=reconcileScheduler.stop") {
		t.Fatalf("stderr = %q, want the wedged reconcileScheduler.stop to be reported",
			capture.stderr())
	}
	// The wedged step was skipped, not fatal: every other step still ran, so the
	// pid file, socket and instance lock are all cleaned up.
	finished := loggedStepOrder(capture.stdout(), "Herdsman daemon shutdown step finished (")
	want := []string{"index.drainInFlightPlans", "watchManager.stop", "server.stop"}
	if len(finished) != len(want) {
		t.Fatalf("finished steps = %v, want %v", finished, want)
	}
	for index, step := range want {
		if finished[index] != step {
			t.Fatalf("finished steps = %v, want %v", finished, want)
		}
	}
	requireNoFile(t, paths.PIDPath)
	requireNoFile(t, paths.SocketPath)
	requireNoFile(t, paths.PIDPath+".instance.lock.owner.json")
	release, err := daemon.AcquireDaemonLock(paths.PIDPath+".instance.lock", nil)
	if err != nil {
		t.Fatalf("acquireDaemonLock after shutdown: %v", err)
	}
	release()
}

func TestShutdownBudgetSecondSignalForcesExit(t *testing.T) {
	home, paths := openDaemonTestHome(t)
	restoreUmask(t)
	capture := startLogCapture(t)

	exitCodes := []int{}
	signals := newSignalTargetDouble()
	var sessionListCalls atomic.Int64
	var tick func()

	err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		Environment: map[string]string{"HERDSMAN_HOME": home},
		Exit:        func(code int) { exitCodes = append(exitCodes, code) },
		PID:         1357,
		HasPID:      true,
		SessionList: func() ([]herdr.HerdrSessionListEntry, error) {
			if sessionListCalls.Add(1) <= 2 {
				return nil, nil
			}
			blocked := make(chan struct{})
			<-blocked
			return nil, nil
		},
		ReconcileClearInterval: func(any) {},
		ReconcileIntervalMs:    int64Ptr(1),
		ReconcileSetInterval: func(callback func(), delayMs int64) any {
			tick = callback
			return 1
		},
		ShutdownBudgetMs: int64Ptr(400),
		SignalTarget:     signals,
	})
	if err != nil {
		t.Fatalf("runObservabilityDaemonService: %v", err)
	}

	tick()
	waitFor(t, func() bool { return sessionListCalls.Load() >= 3 })

	// The first SIGTERM starts a shutdown that is stuck on the wedged step.
	firstStop := signals.emit("SIGTERM")
	// The second one must be handled by us: falling through to the OS default
	// SIGTERM action would kill the process before any cleanup runs.
	signals.waitForListener(t, "SIGTERM")
	<-signals.emit("SIGTERM")

	capture.settle(t, "the forced-exit warning naming the pid", func() bool {
		warning := capture.stderr()
		return strings.Contains(warning, "Herdsman daemon received a second shutdown signal; forcing exit") &&
			strings.Contains(warning, "pid=1357")
	})
	if !strings.Contains(capture.stderr(),
		"Herdsman daemon received a second shutdown signal; forcing exit") ||
		!strings.Contains(capture.stderr(), "pid=1357") {
		t.Fatalf("stderr = %q, want the forced-exit warning naming the pid",
			capture.stderr())
	}
	if len(exitCodes) != 1 || exitCodes[0] != 0 {
		t.Fatalf("exitCodes = %v, want [0]", exitCodes)
	}
	// Proof the exit came from the forced path: the graceful sequence was still
	// running when it happened.
	if strings.Contains(capture.stdout(), "Herdsman daemon shutdown finished") {
		t.Fatalf("stdout = %q, want no graceful shutdown finish yet", capture.stdout())
	}
	requireNoFile(t, paths.PIDPath)
	requireNoFile(t, paths.SocketPath)
	requireNoFile(t, paths.PIDPath+".instance.lock.owner.json")
	release, err := daemon.AcquireDaemonLock(paths.PIDPath+".instance.lock", nil)
	if err != nil {
		t.Fatalf("acquireDaemonLock after the forced exit: %v", err)
	}
	release()

	// The graceful sequence finishes afterwards; its exit loses the race and must
	// not exit a second time.
	<-firstStop
	if len(exitCodes) != 1 || exitCodes[0] != 0 {
		t.Fatalf("exitCodes = %v, want the loser to stay silent", exitCodes)
	}
}

// ---------------------------------------------------------------------------
// describe("daemon service lifecycle and socket guard")
// ---------------------------------------------------------------------------

func TestDaemonServiceRefusesStartAndPreservesReachableSocket(t *testing.T) {
	home, paths := openDaemonTestHome(t)
	restoreUmask(t)
	if err := os.WriteFile(paths.SocketPath, []byte("dummy-socket"), 0o600); err != nil {
		t.Fatalf("write socket: %v", err)
	}

	exitCodes := []int{}
	err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		ConnectSocket: func(string) (bool, error) { return true, nil },
		Environment:   map[string]string{"HERDSMAN_HOME": home},
		Exit:          func(code int) { exitCodes = append(exitCodes, code) },
		PID:           1234,
		HasPID:        true,
		SessionList:   emptySessionList,
	})
	if err == nil ||
		!strings.Contains(err.Error(), "Herdsman daemon socket is already reachable") {
		t.Fatalf("runObservabilityDaemonService error = %v, want the reachable-socket refusal",
			err)
	}

	// PID file was not written.
	requireNoFile(t, paths.PIDPath)
	// Socket file was not unlinked.
	requireFile(t, paths.SocketPath)
	if len(exitCodes) != 0 {
		t.Fatalf("exitCodes = %v, want no exit", exitCodes)
	}
}

func TestDaemonServiceUnlinksStaleSocketWritesPidAndCleansOnSigterm(t *testing.T) {
	home, paths := openDaemonTestHome(t)
	restoreUmask(t)
	if err := os.WriteFile(paths.SocketPath, []byte("stale-socket"), 0o600); err != nil {
		t.Fatalf("write socket: %v", err)
	}

	exitCodes := []int{}
	signals := newSignalTargetDouble()
	err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		ConnectSocket: func(string) (bool, error) { return false, nil },
		Environment:   map[string]string{"HERDSMAN_HOME": home},
		Exit:          func(code int) { exitCodes = append(exitCodes, code) },
		PID:           5678,
		HasPID:        true,
		SessionList:   emptySessionList,
		SignalTarget:  signals,
	})
	if err != nil {
		t.Fatalf("runObservabilityDaemonService: %v", err)
	}

	// PID file written after server.start().
	requireFile(t, paths.PIDPath)
	if got := readFileString(t, paths.PIDPath); got != "5678\n" {
		t.Fatalf("pid file = %q, want %q", got, "5678\n")
	}

	// Send SIGTERM.
	<-signals.emit("SIGTERM")

	// PID file removed.
	requireNoFile(t, paths.PIDPath)
	if len(exitCodes) != 1 || exitCodes[0] != 0 {
		t.Fatalf("exitCodes = %v, want [0]", exitCodes)
	}
}

func TestDaemonServicePreservesForeignPidFileOnStop(t *testing.T) {
	home, paths := openDaemonTestHome(t)
	restoreUmask(t)

	exitCodes := []int{}
	signals := newSignalTargetDouble()
	err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		Environment:  map[string]string{"HERDSMAN_HOME": home},
		Exit:         func(code int) { exitCodes = append(exitCodes, code) },
		PID:          5678,
		HasPID:       true,
		SessionList:  emptySessionList,
		SignalTarget: signals,
	})
	if err != nil {
		t.Fatalf("runObservabilityDaemonService: %v", err)
	}
	if got := readFileString(t, paths.PIDPath); got != "5678\n" {
		t.Fatalf("pid file = %q, want %q", got, "5678\n")
	}

	// Overwrite pid file with foreign PID (e.g. from an external supervisor or
	// concurrent process).
	if err := os.WriteFile(paths.PIDPath, []byte("99999\n"), 0o600); err != nil {
		t.Fatalf("overwrite pid file: %v", err)
	}

	// Send SIGTERM to stop this daemon (pid 5678).
	<-signals.emit("SIGTERM")

	// Foreign PID file must NOT be deleted.
	requireFile(t, paths.PIDPath)
	if got := readFileString(t, paths.PIDPath); got != "99999\n" {
		t.Fatalf("pid file = %q, want %q", got, "99999\n")
	}
	if len(exitCodes) != 1 || exitCodes[0] != 0 {
		t.Fatalf("exitCodes = %v, want [0]", exitCodes)
	}
}

func TestDaemonServiceExitsOneWhenShutdownStepFails(t *testing.T) {
	home, paths := openDaemonTestHome(t)
	restoreUmask(t)
	capture := startLogCapture(t)

	exitCodes := []int{}
	signals := newSignalTargetDouble()
	err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		Environment: map[string]string{"HERDSMAN_HOME": home},
		Exit:        func(code int) { exitCodes = append(exitCodes, code) },
		PID:         5678,
		HasPID:      true,
		// The TS case throws from inside the reconcileScheduler.stop() step; a Go
		// seam that throws panics, which the shutdown sequence turns back into the
		// step's error exactly like the TS rejection.
		ReconcileClearInterval: func(any) {
			panic(errors.New("unexpected scheduler crash"))
		},
		SessionList:  emptySessionList,
		SignalTarget: signals,
	})
	if err != nil {
		t.Fatalf("runObservabilityDaemonService: %v", err)
	}
	requireFile(t, paths.PIDPath)

	<-signals.emit("SIGTERM")

	// Finally block still cleaned the matching PID file.
	requireNoFile(t, paths.PIDPath)
	// Exited with 1 due to the caught shutdown failure.
	if len(exitCodes) != 1 || exitCodes[0] != 1 {
		t.Fatalf("exitCodes = %v, want [1]", exitCodes)
	}
	capture.settle(t, "the shutdown failure to be reported", func() bool {
		warning := capture.stderr()
		return strings.Contains(warning, "Herdsman daemon shutdown failed") &&
			strings.Contains(warning, "unexpected scheduler crash")
	})
	if !strings.Contains(capture.stderr(), "Herdsman daemon shutdown failed") ||
		!strings.Contains(capture.stderr(), "unexpected scheduler crash") {
		t.Fatalf("stderr = %q, want the shutdown failure to be reported", capture.stderr())
	}
}

// scriptedReconciler is the Go stand-in for the TS
// `vi.spyOn(AgentEventReconciler.prototype, "reconcile")`: the service drives it
// through the NewReconciler seam, and the first call fails the way the TS mock
// does.
type scriptedReconciler struct {
	mu        sync.Mutex
	calls     int
	failFirst bool
}

func (r *scriptedReconciler) Reconcile(
	daemon.ReconcileOptions,
) (daemon.AgentEventReconcileResult, error) {
	r.mu.Lock()
	r.calls++
	calls := r.calls
	r.mu.Unlock()
	if r.failFirst && calls == 1 {
		return daemon.AgentEventReconcileResult{},
			errors.New("simulated reconcile startup explosion")
	}
	return daemon.AgentEventReconcileResult{}, nil
}

func TestDaemonServiceReleasesLockAndCleansPidWhenStartupReconcileFails(t *testing.T) {
	home, paths := openDaemonTestHome(t)
	restoreUmask(t)

	reconciler := &scriptedReconciler{failFirst: true}
	err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		Environment: map[string]string{"HERDSMAN_HOME": home},
		PID:         7777,
		HasPID:      true,
		SessionList: emptySessionList,
		NewReconciler: func(daemon.AgentEventReconcilerOptions) daemon.ReconcileRunner {
			return reconciler
		},
	})
	if err == nil || !strings.Contains(err.Error(), "simulated reconcile startup explosion") {
		t.Fatalf("runObservabilityDaemonService error = %v, want the reconcile failure", err)
	}

	// Assert that pid file was removed and not left over.
	requireNoFile(t, paths.PIDPath)
	// The server it had started is closed, so its socket is gone too.
	requireNoFile(t, paths.SocketPath)
	// Instance lock was released and can immediately be acquired by another daemon.
	release, err := daemon.AcquireDaemonLock(paths.PIDPath+".instance.lock", nil)
	if err != nil {
		t.Fatalf("acquireDaemonLock after the failed startup: %v", err)
	}
	release()
}

func TestDaemonServiceSecondDaemonOnSameHomeFailsOnInstanceLock(t *testing.T) {
	home, paths := openDaemonTestHome(t)
	restoreUmask(t)

	// A real daemon process holds the lock for this HERDSMAN_HOME. The TS suite
	// spawns `node --import tsx herdsman-daemon.js`; the Go port re-executes this
	// test binary in the daemon worker mode the shared TestMain dispatches, so the
	// child runs the real service with the real signal handlers.
	command, stdout, stderr := startDaemonWorker(t, home)
	waitForDaemonWorkerPidFile(t, paths.PIDPath, command.Process.Pid, stdout, stderr)
	requireFile(t, paths.PIDPath+".instance.lock")

	exitCodes := []int{}
	signals := newSignalTargetDouble()
	err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		Environment:  map[string]string{"HERDSMAN_HOME": home},
		Exit:         func(code int) { exitCodes = append(exitCodes, code) },
		PID:          2222,
		HasPID:       true,
		SessionList:  emptySessionList,
		SignalTarget: signals,
	})
	if err == nil || !strings.Contains(err.Error(), "Herdsman daemon operation lock is held") {
		t.Fatalf("second daemon error = %v, want the instance-lock rejection", err)
	}
	if len(exitCodes) != 0 {
		t.Fatalf("exitCodes = %v, want no exit", exitCodes)
	}

	// Stopping the child daemon releases the instance lock.
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal daemon worker: %v", err)
	}
	waitForDaemonWorkerExit(t, command, stderr)
	// The lock file itself is never unlinked; only the lock is dropped.
	requireFile(t, paths.PIDPath+".instance.lock")
	requireNoFile(t, paths.PIDPath)

	// With the lock released, the same HERDSMAN_HOME can be started again.
	restartSignals := newSignalTargetDouble()
	if err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		Environment:  map[string]string{"HERDSMAN_HOME": home},
		Exit:         func(code int) { exitCodes = append(exitCodes, code) },
		PID:          3333,
		HasPID:       true,
		SessionList:  emptySessionList,
		SignalTarget: restartSignals,
	}); err != nil {
		t.Fatalf("restart after the lock was released: %v", err)
	}
	if got := readFileString(t, paths.PIDPath); got != "3333\n" {
		t.Fatalf("pid file = %q, want %q", got, "3333\n")
	}
	<-restartSignals.emit("SIGTERM")
	if len(exitCodes) != 1 || exitCodes[0] != 0 {
		t.Fatalf("exitCodes = %v, want [0]", exitCodes)
	}
}

func TestDaemonServiceRejectedByInstanceLockNeverOpensDatabase(t *testing.T) {
	home, paths := openDaemonTestHome(t)
	restoreUmask(t)

	// Another daemon already owns this HERDSMAN_HOME.
	release, err := daemon.AcquireDaemonLock(paths.PIDPath+".instance.lock", nil)
	if err != nil {
		t.Fatalf("acquireDaemonLock: %v", err)
	}
	t.Cleanup(release)

	var openCalls, migrateCalls atomic.Int64
	exitCodes := []int{}
	err = daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		ApplyMigrations: func(*sql.DB) error {
			migrateCalls.Add(1)
			return errors.New("applyMigrations must not run when the instance lock is held")
		},
		Environment: map[string]string{"HERDSMAN_HOME": home},
		Exit:        func(code int) { exitCodes = append(exitCodes, code) },
		OpenSqlite: func(string) (*sql.DB, error) {
			openCalls.Add(1)
			return nil, errors.New("openSqlite must not run when the instance lock is held")
		},
		PID:         5555,
		HasPID:      true,
		SessionList: emptySessionList,
	})
	if err == nil || !strings.Contains(err.Error(), "Herdsman daemon operation lock is held") {
		t.Fatalf("runObservabilityDaemonService error = %v, want the instance-lock rejection",
			err)
	}

	// The lock is taken before the database is opened: the rejected daemon never
	// touched the SQLite file.
	if openCalls.Load() != 0 {
		t.Fatalf("openSqlite calls = %d, want 0", openCalls.Load())
	}
	if migrateCalls.Load() != 0 {
		t.Fatalf("applyMigrations calls = %d, want 0", migrateCalls.Load())
	}
	requireNoFile(t, paths.DBPath)
	requireNoFile(t, paths.PIDPath)
	if len(exitCodes) != 0 {
		t.Fatalf("exitCodes = %v, want no exit", exitCodes)
	}
}

func TestDaemonServiceDoubleStartRaceSharesSocketPath(t *testing.T) {
	home, paths := openDaemonTestHome(t)
	restoreUmask(t)

	exitCodes1 := []int{}
	signals1 := newSignalTargetDouble()

	// Start primary daemon 1.
	if err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		Environment:  map[string]string{"HERDSMAN_HOME": home},
		Exit:         func(code int) { exitCodes1 = append(exitCodes1, code) },
		PID:          1001,
		HasPID:       true,
		SessionList:  emptySessionList,
		SignalTarget: signals1,
	}); err != nil {
		t.Fatalf("daemon 1: %v", err)
	}
	if got := readFileString(t, paths.PIDPath); got != "1001\n" {
		t.Fatalf("pid file = %q, want %q", got, "1001\n")
	}

	// Daemon 2 attempts to start with the same HERDSMAN_HOME while daemon 1 is
	// running and listening.
	exitCodes2 := []int{}
	err := daemon.RunObservabilityDaemonService(&daemon.DaemonServiceOptions{
		Environment: map[string]string{"HERDSMAN_HOME": home},
		Exit:        func(code int) { exitCodes2 = append(exitCodes2, code) },
		PID:         2002,
		HasPID:      true,
		SessionList: emptySessionList,
	})
	lockHeld := err != nil && strings.Contains(err.Error(),
		"Herdsman daemon operation lock is held")
	socketReachable := err != nil && strings.Contains(err.Error(),
		"Herdsman daemon socket is already reachable")
	if !lockHeld && !socketReachable {
		t.Fatalf("daemon 2 error = %v, want the instance-lock or socket refusal", err)
	}

	// Daemon 1's PID file is untouched.
	if got := readFileString(t, paths.PIDPath); got != "1001\n" {
		t.Fatalf("pid file = %q, want %q", got, "1001\n")
	}
	// Daemon 2 never exited via stop.
	if len(exitCodes2) != 0 {
		t.Fatalf("exitCodes2 = %v, want no exit", exitCodes2)
	}

	// Shutdown daemon 1 cleanly.
	<-signals1.emit("SIGTERM")
	requireNoFile(t, paths.PIDPath)
	if len(exitCodes1) != 1 || exitCodes1[0] != 0 {
		t.Fatalf("exitCodes1 = %v, want [0]", exitCodes1)
	}
}

// ---------------------------------------------------------------------------
// Daemon worker (child process)
// ---------------------------------------------------------------------------

// startDaemonWorker re-executes this test binary as a real daemon process on
// home and returns the command plus the files its output is captured into.
func startDaemonWorker(t *testing.T, home string) (*exec.Cmd, *os.File, *os.File) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "daemon-worker-stdout")
	if err != nil {
		t.Fatalf("createTemp stdout: %v", err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "daemon-worker-stderr")
	if err != nil {
		t.Fatalf("createTemp stderr: %v", err)
	}
	command := exec.Command(os.Args[0])
	command.Env = append(environmentWithout("HERDSMAN_HOME"),
		"HERDSMAN_HOME="+home, daemonWorkerEnv+"="+home)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start daemon worker: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	return command, stdout, stderr
}

// environmentWithout drops every entry of key, so a value this test process
// picked up cannot leak into the child.
func environmentWithout(key string) []string {
	entries := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, key+"=") {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// waitForDaemonWorkerPidFile waits until the child daemon has written its pid
// file, which the TS suite treats as proof the child holds the instance lock.
func waitForDaemonWorkerPidFile(
	t *testing.T,
	pidPath string,
	expectedPid int,
	stdout, stderr *os.File,
) {
	t.Helper()
	want := strconv.Itoa(expectedPid) + "\n"
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if fileExists(pidPath) {
			got := readFileString(t, pidPath)
			if got != want {
				t.Fatalf("pid file = %q, want %q (worker stdout: %s; stderr: %s)",
					got, want, readWorkerOutput(stdout), readWorkerOutput(stderr))
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon worker pid file %s never appeared (stdout: %s; stderr: %s)",
		pidPath, readWorkerOutput(stdout), readWorkerOutput(stderr))
}

// waitForDaemonWorkerExit waits for the child daemon to exit and asserts it shut
// down gracefully, the way the TS case checks the child's exit code and output.
func waitForDaemonWorkerExit(t *testing.T, command *exec.Cmd, stderr *os.File) {
	t.Helper()
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("daemon worker exit = %v, want exit code 0 (stderr: %s)",
				err, readWorkerOutput(stderr))
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon worker did not exit (stderr: %s)", readWorkerOutput(stderr))
	}
}

func readWorkerOutput(file *os.File) string {
	if _, err := file.Seek(0, 0); err != nil {
		return ""
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		return ""
	}
	return string(data)
}
