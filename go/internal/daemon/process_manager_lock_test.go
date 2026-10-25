package daemon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lockWorkerEnv selects the re-exec worker mode inside this test binary. The TS
// suite spawned `node node_modules/tsx/dist/cli.mjs -e <workerCode>` importing
// the real module; the Go equivalent re-executes the test binary so the
// contender runs the real AcquireDaemonLock in its own process.
const lockWorkerEnv = "HERDSMAN_GO_LOCK_WORKER"

// TestMain doubles as the contender entry point: when lockWorkerEnv is set the
// process behaves as a lock worker instead of running the suite.
func TestMain(m *testing.M) {
	if mode := os.Getenv(lockWorkerEnv); mode != "" {
		runLockWorker(mode, os.Args[len(os.Args)-1])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runLockWorker implements the contender modes:
//
//	oneshot — acquire the lock, print ACQUIRED, hold until stdin closes, then
//	          release and exit 0; print HELD and exit 1 when refused.
//	hold    — acquire the lock (fatal on failure), print READY, hold forever.
//	race    — line protocol: "RACE <n>" answers WON/LOST, "RELEASE <n>"
//	          releases and answers RELEASED.
func runLockWorker(mode, lockPath string) {
	switch mode {
	case "oneshot":
		release, err := AcquireDaemonLock(lockPath, nil)
		if err != nil {
			fmt.Println("HELD")
			os.Exit(1)
		}
		fmt.Println("ACQUIRED")
		_, _ = io.Copy(io.Discard, os.Stdin)
		release()
	case "hold":
		if _, err := AcquireDaemonLock(lockPath, nil); err != nil {
			fmt.Fprintf(os.Stderr, "worker could not acquire the lock: %v\n", err)
			os.Exit(2)
		}
		fmt.Println("READY")
		select {}
	case "race":
		reader := bufio.NewReader(os.Stdin)
		var release func()
		fmt.Println("READY")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) == 0 {
				continue
			}
			suffix := strings.Join(fields[1:], " ")
			switch fields[0] {
			case "RACE":
				if release != nil {
					// Still holding from a previous round: this worker cannot be a
					// second master, so report the protocol break loudly.
					fmt.Println("LOGIC-STILL-HELD " + suffix)
					continue
				}
				got, acquireErr := AcquireDaemonLock(lockPath, nil)
				if acquireErr != nil {
					fmt.Println("LOST " + suffix)
					continue
				}
				release = got
				fmt.Println("WON " + suffix)
			case "RELEASE":
				if release != nil {
					release()
					release = nil
				}
				fmt.Println("RELEASED " + suffix)
			}
		}
	}
}

// lockWorker is a spawned contender: its process (group), its stdin pipe and a
// buffered reader over its stdout.
type lockWorker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// startLockWorker spawns this test binary as a lock contender in its own
// process group so the whole group can be SIGKILLed, mirroring the TS suite's
// detached workers.
func startLockWorker(t *testing.T, mode, lockPath string) *lockWorker {
	t.Helper()
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("process-group kill requires Linux")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$" /* worker mode ignores it */, "--", lockPath)
	cmd.Env = append(os.Environ(), lockWorkerEnv+"="+mode)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	worker := &lockWorker{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	t.Cleanup(worker.killGroup)
	return worker
}

// killGroup SIGKILLs the worker's whole process group, tolerating an
// already-dead pid.
func (w *lockWorker) killGroup() {
	if w.cmd.Process == nil {
		return
	}
	_ = w.stdin.Close()
	_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	_ = w.cmd.Wait()
}

func (w *lockWorker) send(t *testing.T, format string, args ...any) {
	t.Helper()
	if _, err := fmt.Fprintf(w.stdin, format+"\n", args...); err != nil {
		t.Fatalf("writing to worker %d stdin: %v", w.cmd.Process.Pid, err)
	}
}

// line reads one line of worker output; a worker that exited without output
// fails the test rather than hanging it, because the caller always writes a
// command first.
func (w *lockWorker) line(t *testing.T) string {
	t.Helper()
	line, err := w.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("reading worker %d output: %v", w.cmd.Process.Pid, err)
	}
	return strings.TrimRight(line, "\r\n")
}

func (w *lockWorker) waitExit(t *testing.T, description string) {
	t.Helper()
	// cmd.Wait is the only proof the pid was reaped: a signalled-but-unreaped
	// child still answers kill(pid, 0).
	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out after 5s waiting for %s", description)
	}
}

// waitFlockFree polls until the kernel flock on lockPath is observably free.
func waitFlockFree(t *testing.T, lockPath string) {
	t.Helper()
	waitForCondition(t, "the flock to be observable as free", func() bool {
		held, err := IsFlockHeld(lockPath)
		return err == nil && !held
	})
}

func TestTwoConcurrentProcessesCompetingExactlyOneSucceeds(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")

	w1 := startLockWorker(t, "oneshot", lockPath)
	w2 := startLockWorker(t, "oneshot", lockPath)

	results := []string{w1.line(t), w2.line(t)}
	acquired, held := 0, 0
	for _, result := range results {
		switch result {
		case "ACQUIRED":
			acquired++
		case "HELD":
			held++
		}
	}
	if acquired != 1 || held != 1 {
		t.Fatalf("results = %v, want exactly one ACQUIRED and one HELD", results)
	}

	// Reap both workers under an explicit bound: they are detached and keep the
	// lock until their stdin ends, so an unbounded exit wait would hang the suite
	// if the group SIGKILL never landed.
	_ = syscall.Kill(-w1.cmd.Process.Pid, syscall.SIGKILL)
	_ = syscall.Kill(-w2.cmd.Process.Pid, syscall.SIGKILL)
	w1.waitExit(t, "the first lock contender to be reaped after SIGKILL")
	w2.waitExit(t, "the second lock contender to be reaped after SIGKILL")
}

func TestFlockHeldByChildIsReleasedOnSIGKILL(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")

	child := startLockWorker(t, "hold", lockPath)
	if line := child.line(t); line != "READY" {
		t.Fatalf("worker said %q, want READY", line)
	}

	// The parent cannot acquire while the child holds the flock.
	if _, err := AcquireDaemonLock(lockPath, nil); err == nil {
		t.Fatal("acquiring a lock held by a child must fail")
	} else if !strings.Contains(err.Error(), "Herdsman daemon operation lock is held") {
		t.Fatalf("error = %v", err)
	}

	// SIGKILL the whole child process group.
	_ = syscall.Kill(-child.cmd.Process.Pid, syscall.SIGKILL)
	// Kernel cleanup after SIGKILL is not observable at a fixed offset, so poll
	// until the flock is really released instead of assuming a fixed delay is
	// enough on a loaded machine.
	child.waitExit(t, "the SIGKILLed flock holder to exit")
	waitFlockFree(t, lockPath)

	// The parent can immediately acquire the lock without delay.
	release, err := AcquireDaemonLock(lockPath, nil)
	if err != nil {
		t.Fatalf("acquiring after the holder was killed: %v", err)
	}
	if !fileExists(lockPath) {
		t.Fatal("lock file must exist")
	}
	release()
	if !fileExists(lockPath) {
		t.Fatal("lock file must be persistent")
	}
}

func TestFlockHandleReleaseDoesNotBusyWaitForNextHolder(t *testing.T) {
	realFlock, err := exec.LookPath("flock")
	if err != nil {
		t.Skip("flock(1) not available")
	}
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")
	// Written by the contender only after it owns the lock, and held far longer
	// than the assertions below need, so nothing here races a `sleep` expiry.
	markerPath := filepath.Join(dir, "next-holder-acquired")

	handle, err := AcquireFlockHandle(lockPath)
	if err != nil || handle == nil {
		t.Fatalf("AcquireFlockHandle = %v/%v", handle, err)
	}

	// Start a background process blocked on acquiring the flock on lockPath.
	contender := exec.Command(realFlock, "-x", lockPath, "sh", "-c",
		fmt.Sprintf("printf TAKEN > %s; sleep 30", markerPath))
	contender.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	contender.Stderr = os.Stderr
	if err := contender.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// The contender holds the lock for 30s, so it must never outlive this
		// test, not even when an assertion above fails.
		_ = syscall.Kill(-contender.Process.Pid, syscall.SIGKILL)
		_ = contender.Wait()
	}()

	// Wait until the contender is really parked inside flock(2) instead of
	// assuming a fixed delay was long enough under load.
	waitForCondition(t, "the contending flock process to block on the lock", func() bool {
		return isBlockedAcquiringFlock(contender.Process.Pid)
	})

	start := time.Now()
	handle.Release()
	// Release must return promptly (well below 100ms) without busy waiting.
	if elapsed := time.Since(start); elapsed >= 70*time.Millisecond {
		t.Fatalf("release took %s, want well under 100ms", elapsed)
	}

	// The lock is now immediately held by the next child process.
	waitForCondition(t, "the contender to take the lock over after release", func() bool {
		return fileExists(markerPath)
	})
	held, err := IsFlockHeld(lockPath)
	if err != nil || !held {
		t.Fatalf("held = %v/%v after the contender took over", held, err)
	}
}

// isBlockedAcquiringFlock is true once pid is parked inside the kernel
// acquiring the flock. A process that already owns a lock waits somewhere else
// (for `flock -x f <cmd>`: in do_wait for its <cmd> child), so this identifies a
// contending process instead of guessing how long a fixed delay should be.
func isBlockedAcquiringFlock(pid int) bool {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/wchan", pid)); err == nil &&
		strings.Contains(string(data), "locks_lock_inode_wait") {
		return true
	}
	// The flock(2) syscall number is only a fallback signal for kernels that do
	// not expose wchan symbols; wchan is the primary check above.
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/syscall", pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) < 3 {
		return false
	}
	expected := flockSyscallNumber()
	return expected != "" && fields[0] == expected && fields[2] == "0x2"
}

func flockSyscallNumber() string {
	switch runtime.GOARCH {
	case "arm64":
		return "32"
	case "amd64":
		return "73"
	}
	return ""
}

func TestFlockHandleReleaseReturnsOnlyOnceTheLockIsFree(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")

	handle, err := AcquireFlockHandle(lockPath)
	if err != nil || handle == nil {
		t.Fatalf("AcquireFlockHandle = %v/%v", handle, err)
	}
	held, err := IsFlockHeld(lockPath)
	if err != nil || !held {
		t.Fatalf("held = %v/%v while the handle owns the lock", held, err)
	}

	// The post-condition this barrier exists for: once Release has returned the
	// lock is free for the next acquirer. The TS implementation had to wait for a
	// surviving fd-sharing helper child here; owning the descriptor directly makes
	// the post-release hold zero by construction, so it is asserted as zero.
	start := time.Now()
	handle.Release()
	elapsed := time.Since(start)
	held, err = IsFlockHeld(lockPath)
	if err != nil || held {
		t.Fatalf("held = %v/%v immediately after release", held, err)
	}
	if elapsed >= 70*time.Millisecond {
		t.Fatalf("release took %s, want to return as soon as the lock is dropped", elapsed)
	}
	// Release is idempotent.
	handle.Release()
}

func TestAcquireFailsFastWhileAnotherProcessHoldsTheLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")

	child := startLockWorker(t, "hold", lockPath)
	if line := child.line(t); line != "READY" {
		t.Fatalf("worker said %q, want READY", line)
	}

	// A live registered owner means the lock is held on purpose: the diagnostic
	// "lock is held" path stays immediate instead of spending a retry window.
	start := time.Now()
	_, err := AcquireDaemonLock(lockPath, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("acquisition must fail while another process holds the lock")
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("contended acquisition took %s; the live-owner path stays immediate", elapsed)
	}
	if want := fmt.Sprintf("is held by PID %d", child.cmd.Process.Pid); !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q must name the live owner (%s)", err, want)
	}

	// Without a registered owner the contended acquisition still reports the lock
	// as held, and still stays fast.
	if err := os.Remove(lockPath + ".owner.json"); err != nil {
		t.Fatal(err)
	}
	handle, err := AcquireFlockHandle(lockPath)
	if err != nil {
		t.Fatalf("AcquireFlockHandle: %v", err)
	}
	if handle != nil {
		t.Fatal("AcquireFlockHandle must return nil while another process holds the flock")
	}

	_ = syscall.Kill(-child.cmd.Process.Pid, syscall.SIGKILL)
	child.waitExit(t, "the holder to exit")
	waitFlockFree(t, lockPath)
	// The lock is genuinely reusable once the holder is gone.
	release, err := AcquireDaemonLock(lockPath, nil)
	if err != nil {
		t.Fatalf("AcquireDaemonLock after the holder exited: %v", err)
	}
	release()
}

func TestStressLockContentionHasNoDoubleMasters(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")

	w1 := startLockWorker(t, "race", lockPath)
	w2 := startLockWorker(t, "race", lockPath)
	if line := w1.line(t); line != "READY" {
		t.Fatalf("worker 1 said %q, want READY", line)
	}
	if line := w2.line(t); line != "READY" {
		t.Fatalf("worker 2 said %q, want READY", line)
	}

	const totalRounds = 200
	var singleMaster, doubleMaster, bothLost int
	for round := 0; round < totalRounds; round++ {
		w1.send(t, "RACE %d", round)
		w2.send(t, "RACE %d", round)
		res1, res2 := w1.line(t), w2.line(t)
		outcomes := []string{res1, res2}
		won, lost := 0, 0
		for _, outcome := range outcomes {
			if outcome == "" {
				t.Fatalf("unexpected empty outcome for round %d: %q / %q", round, res1, res2)
			}
			switch strings.Fields(outcome)[0] {
			case "WON":
				won++
			case "LOST":
				lost++
			}
		}
		switch {
		case won == 1 && lost == 1:
			singleMaster++
			winner, loser := w1, w2
			if strings.HasPrefix(res2, "WON") {
				winner, loser = w2, w1
			}
			winner.send(t, "RELEASE %d", round)
			if got := winner.line(t); got != fmt.Sprintf("RELEASED %d", round) {
				t.Fatalf("round %d: winner said %q, want RELEASED %d", round, got, round)
			}
			// The loser never entered the protocol, so it must not have written a
			// lock record either; reading its line again below keeps the streams in
			// step.
			_ = loser
		case won == 2:
			doubleMaster++
			// Both believe they own it: release whatever each holds so the loop
			// keeps running instead of wedging on the next round.
			w1.send(t, "RELEASE %d", round)
			w2.send(t, "RELEASE %d", round)
			w1.line(t)
			w2.line(t)
		case lost == 2:
			bothLost++
		default:
			t.Fatalf("unexpected race outcomes for round %d: %s, %s", round, res1, res2)
		}
	}

	_ = syscall.Kill(w1.cmd.Process.Pid, syscall.SIGKILL)
	_ = syscall.Kill(w2.cmd.Process.Pid, syscall.SIGKILL)
	_ = w1.cmd.Wait()
	_ = w2.cmd.Wait()

	counts := fmt.Sprintf("(WON,WON) double-master=%d; (LOST,LOST) both-lost=%d; (WON,LOST) single=%d",
		doubleMaster, bothLost, singleMaster)
	t.Logf("%d contention rounds: %s", totalRounds, counts)
	if doubleMaster != 0 {
		t.Fatalf("double masters observed: %s", counts)
	}
	if bothLost > 10 {
		t.Fatalf("too many both-lost rounds: %s", counts)
	}
	if singleMaster+bothLost+doubleMaster != totalRounds {
		t.Fatalf("round accounting broken: %s (want %d rounds)", counts, totalRounds)
	}
}
