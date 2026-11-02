package daemon

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// This file covers the daemon half of the agy wake fix (task B): when the
// daemon restarts it leaves every agy main pane that was parked waiting for a
// wake (`awaitingNextTurn=true`) without a live WakeWaiter — the waiter process
// dies with the old daemon's session (hook-spawned waiters lose the RPC socket;
// daemon-spawned waiters share the herdsman.service cgroup and are SIGTERM'd on
// stop) and nothing re-arms it, so completions that arrive afterwards are
// written to the DB but never pushed. On startup the daemon scans the agy-state
// directory and re-spawns a WakeWaiter for every session still parked in
// `awaitingNextTurn`, restoring the delivery path.
//
// The re-arm must also:
//  1. Rewrite `waiterPid` in the state file: Stop records the old waiter's pid,
//     and both `isWaiterSuperseded` and the wake-prompt race guard treat a
//     mismatched pid as "this waiter is stale — exit". Leaving the dead pid in
//     place would make the freshly spawned waiter exit immediately.
//  2. Restore the Herdr launch identity (`HERDR_ENV`, `HERDR_SOCKET_PATH`,
//     `HERDR_PANE_ID`, `HERDR_WORKSPACE_ID`) and the pane cwd. `RunAgyHook`
//     returns a silent `{}` without HERDR_*; with the wrong cwd the connector
//     pane-binding check rejects the process. Stop persists these on the state
//     file; for older state files that lack them we try to discover a live agy
//     process for the same `--conversation` id.
//  3. Wait on the child so an early exit does not leave a zombie under the
//     long-lived daemon (the hook's Release() is fine because the hook itself
//     exits and reparents; the daemon does not).
//  4. Never touch a session that is already served by a live waiter: the
//     /proc liveness check below keeps a daemon restart or a second re-arm
//     run from killing an in-flight waiter by clearing its waiterPid (O-5).
//  5. Commit conditionally: the state file is the source of truth shared with
//     concurrent Stop/PreInvocation turns, so every mutation re-reads the file
//     and only applies while the parked generation it saw is still the current
//     one. A concurrent new turn must win — the just-spawned waiter is then
//     killed instead of racing the turn's own waiter (O-2).

// agyStateDirName mirrors the hook's `${HERDSMAN_HOME}/agy-state` leaf.
const agyStateDirName = "agy-state"

// agyStateFilePattern picks the per-session `<sessionId>.json` state files.
// The hook sanitizes session ids to [a-zA-Z0-9._-]; agy session ids are UUIDs,
// so the file name round-trips back to the session id unchanged.
var agyStateFilePattern = regexp.MustCompile(`^(.+)\.json$`)

// allowRealWaiterSpawnInTest is the single, explicit gate that lets
// spawnAgyWakeWaiterProcess run a real child inside `go test`. Every other
// test in this package (and any transitively reached spawn site) must stay
// fused even when a herdsman binary happens to sit on the host PATH — an
// accidental exec of /root/.local/bin/herdsman is what the fuse exists to
// prevent (O-3). Only TestRearmAgyWaitersRealSubprocessStaysAlive flips this
// on, and it resets it through t.Cleanup.
var allowRealWaiterSpawnInTest atomic.Bool

// agyWakeStateSubset is the slice of SessionHookState the daemon needs. It is
// decoded loosely (unknown keys ignored) so the daemon does not depend on the
// agy package's state layout.
type agyWakeStateSubset struct {
	AwaitingNextTurn *bool    `json:"awaitingNextTurn"`
	TurnEpoch        *float64 `json:"turnEpoch"`
	WaiterEpoch      *float64 `json:"waiterEpoch"`
	HerdrSocketPath  *string  `json:"herdrSocketPath"`
	PaneID           *string  `json:"paneId"`
	WorkspaceID      *string  `json:"workspaceId"`
	WaiterCwd        *string  `json:"waiterCwd"`
	WaiterPID        *int     `json:"waiterPid"`
}

// agyWaiterGeneration is the identity of the parked turn a re-arm read off the
// state file. A conditional commit compares all three fields: a concurrent
// PreInvocation/Stop bumping turnEpoch or waiterEpoch, or a new turn recording
// its own waiterPid, must win the race.
type agyWaiterGeneration struct {
	Awaiting  bool
	Epoch     float64
	WaiterPID *int
}

// RearmAgyWaitersOptions injects the seams the unit tests need. A nil option
// set resolves every default from the process environment.
type RearmAgyWaitersOptions struct {
	// StateDir overrides `${HERDSMAN_HOME:-$HOME/.herdsman}/agy-state`.
	StateDir string
	// Environment is the base env handed to the spawned waiter (the daemon's
	// own env by default); the HERDSMAN_AGY_* and HERDR_* waiter variables are
	// appended on top.
	Environment map[string]string
	// Spawn re-spawns one waiter; defaults to spawnAgyWakeWaiterProcess.
	Spawn func(input AgyWakeSpawnInput) (pid int, err error)
	// Kill terminates a just-spawned waiter that lost the conditional-commit
	// race; defaults to SIGKILL on the pid.
	Kill func(pid int)
	// WaiterAlive reports whether the recorded waiterPid still names a live
	// WakeWaiter for this session; defaults to the /proc check.
	WaiterAlive func(pid int, sessionID string) bool
	// DiscoverLive looks up HERDR_* + cwd from a live agy process when the
	// state file has no persisted launch identity. Defaults to
	// discoverLiveAgyHerdrIdentity.
	DiscoverLive func(sessionID string) (AgyWakeLaunchIdentity, bool)
	// Warn receives best-effort failure notices; defaults to stderr.
	Warn func(message string)
}

// AgyWakeLaunchIdentity is the Herdr pane identity a WakeWaiter needs to pass
// RunAgyHook and the connector pane-cwd binding check.
type AgyWakeLaunchIdentity struct {
	HerdrSocketPath string
	PaneID          string
	WorkspaceID     string
	Cwd             string
	AutoOn          string
}

// AgyWakeSpawnInput is one parked session to re-arm.
type AgyWakeSpawnInput struct {
	SessionID   string
	StateDir    string
	WaiterEpoch float64
	Cwd         string
	Environment map[string]string
}

// RearmAgyWaiters scans the agy-state directory and re-spawns a WakeWaiter for
// every session whose state file still records `awaitingNextTurn=true`. It is
// best-effort: a session that cannot be re-armed is logged and skipped so a
// single corrupt file cannot hold back the rest.
//
// For each parked session the sequence mirrors Stop's spawn path: verify the
// recorded waiterPid is not already a live waiter for this session, clear it
// through a conditional commit, spawn with HERDR_* + pane cwd, then commit the
// new pid through a second conditional commit. A concurrent new turn that
// replaced the parked generation between those reads wins: the just-spawned
// waiter is killed and the session is not counted.
//
// Returns the number of sessions re-armed.
func RearmAgyWaiters(options *RearmAgyWaitersOptions) int {
	var opts RearmAgyWaitersOptions
	if options != nil {
		opts = *options
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = defaultAgyStateDir(opts.Environment)
	}
	spawn := opts.Spawn
	if spawn == nil {
		spawn = spawnAgyWakeWaiterProcess
	}
	kill := opts.Kill
	if kill == nil {
		kill = killAgyWaiterProcess
	}
	waiterAlive := opts.WaiterAlive
	if waiterAlive == nil {
		waiterAlive = isLiveAgyWakeWaiter
	}
	discover := opts.DiscoverLive
	if discover == nil {
		discover = discoverLiveAgyHerdrIdentity
	}
	warn := opts.Warn
	if warn == nil {
		warn = func(message string) { fmt.Fprintln(os.Stderr, message) }
	}
	environment := opts.Environment
	if environment == nil {
		environment = processEnvironment()
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		// No state dir means no parked sessions — not an error on a fresh host.
		return 0
	}
	rearmed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := agyStateFilePattern.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		sessionID := match[1]
		path := filepath.Join(stateDir, entry.Name())
		state, err := readAgyWakeState(path)
		if err != nil {
			warn(fmt.Sprintf("Herdsman could not parse agy wake state %s: %v", path, err))
			continue
		}
		if state.AwaitingNextTurn == nil || !*state.AwaitingNextTurn {
			continue
		}
		// A waiter pid that still names a live WakeWaiter for this very
		// session means the pane is already served (e.g. a daemon restart
		// raced a waiter that survived, or a concurrent re-arm already
		// spawned). Clearing that pid would make the live waiter exit as
		// superseded, so skip the session outright (O-5).
		if state.WaiterPID != nil && *state.WaiterPID > 0 && waiterAlive(*state.WaiterPID, sessionID) {
			continue
		}
		waiterEpoch := 0.0
		if state.WaiterEpoch != nil {
			waiterEpoch = *state.WaiterEpoch
		} else if state.TurnEpoch != nil {
			waiterEpoch = *state.TurnEpoch
		}
		generation := agyWaiterGeneration{
			Awaiting:  true,
			Epoch:     waiterEpoch,
			WaiterPID: state.WaiterPID,
		}
		// The pre-spawn clear keeps a generation that still names a dead pid
		// (the re-arm's whole point) but must not fire twice: once the pid is
		// cleared the generation moves on to WaiterPID=nil, which is what the
		// post-spawn commit expects.
		clearedGeneration := agyWaiterGeneration{
			Awaiting:  true,
			Epoch:     waiterEpoch,
			WaiterPID: nil,
		}
		launch, ok := launchIdentityFromState(state)
		if !ok {
			launch, ok = discover(sessionID)
		}
		if !ok {
			warn(fmt.Sprintf("Herdsman could not re-arm agy wake waiter for session %s: missing Herdr launch identity (herdrSocketPath/paneId/workspaceId/waiterCwd) and no live agy process to discover from", sessionID))
			continue
		}
		spawnEnv := cloneStringMap(environment)
		spawnEnv["HERDR_ENV"] = "1"
		spawnEnv["HERDR_SOCKET_PATH"] = launch.HerdrSocketPath
		spawnEnv["HERDR_PANE_ID"] = launch.PaneID
		spawnEnv["HERDR_WORKSPACE_ID"] = launch.WorkspaceID
		if launch.AutoOn != "" {
			spawnEnv["HERDSMAN_AUTO_ON"] = launch.AutoOn
		}
		// Drop the dead waiter's pid before spawn so the new process does not
		// immediately treat itself as superseded (Stop does the same). The
		// conditional commit only lands while the parked generation is still
		// current: a concurrent turn that bumped the epoch or wrote its own
		// waiterPid wins and this session is left for that turn's waiter.
		updated, err := updateAgyWaiterPIDConditionally(path, generation, nil)
		if err != nil {
			warn(fmt.Sprintf("Herdsman could not clear stale agy waiterPid for session %s: %v", sessionID, err))
			continue
		}
		if !updated {
			continue
		}
		pid, err := spawn(AgyWakeSpawnInput{
			SessionID:   sessionID,
			StateDir:    stateDir,
			WaiterEpoch: waiterEpoch,
			Cwd:         launch.Cwd,
			Environment: spawnEnv,
		})
		if err != nil {
			warn(fmt.Sprintf("Herdsman could not re-arm agy wake waiter for session %s: %v", sessionID, err))
			continue
		}
		// Writing the new pid is the second conditional commit. When the file
		// moved on (new turn, or another re-arm that already recorded its own
		// pid) this waiter belongs to a dead generation and must not keep
		// running: kill it and do not count the session. A disk write failure
		// leaves waiterPid unset on disk, so the just-spawned waiter would run
		// without an owner — kill it the same way instead of orphaning it.
		updated, err = updateAgyWaiterPIDConditionally(path, clearedGeneration, &pid)
		if err != nil {
			kill(pid)
			warn(fmt.Sprintf("Herdsman re-armed agy wake waiter for session %s (pid=%d) but could not record waiterPid: %v", sessionID, pid, err))
			continue
		}
		if !updated {
			kill(pid)
			continue
		}
		warn(fmt.Sprintf("Herdsman re-armed agy wake waiter for session %s (pid=%d)", sessionID, pid))
		rearmed++
	}
	return rearmed
}

// readAgyWakeState decodes one state file into the daemon's loose subset.
func readAgyWakeState(path string) (agyWakeStateSubset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return agyWakeStateSubset{}, err
	}
	state := agyWakeStateSubset{}
	if err := json.Unmarshal(data, &state); err != nil {
		return agyWakeStateSubset{}, err
	}
	return state, nil
}

// stateMatchesGeneration reports whether the on-disk state still describes the
// parked generation the re-arm read: awaitingNextTurn is set, the effective
// waiter epoch is unchanged and no concurrent turn rewrote waiterPid.
func stateMatchesGeneration(state agyWakeStateSubset, generation agyWaiterGeneration) bool {
	if !generation.Awaiting {
		return false
	}
	if state.AwaitingNextTurn == nil || !*state.AwaitingNextTurn {
		return false
	}
	epoch := 0.0
	if state.WaiterEpoch != nil {
		epoch = *state.WaiterEpoch
	} else if state.TurnEpoch != nil {
		epoch = *state.TurnEpoch
	}
	if epoch != generation.Epoch {
		return false
	}
	if generation.WaiterPID == nil {
		return state.WaiterPID == nil
	}
	return state.WaiterPID != nil && *state.WaiterPID == *generation.WaiterPID
}

// updateAgyWaiterPIDConditionally rewrites only the `waiterPid` field of a
// state file — atomically (same-directory 0600 temp file + rename, so a
// concurrently started WakeWaiter never reads a truncated file) and only while
// the parked generation still matches the file on disk. A nil pid deletes the
// field (the pre-spawn clear); a non-nil pid records the freshly spawned
// waiter. Returns false (without touching the file) when the generation moved
// on; an error means the file itself could not be read or written.
func updateAgyWaiterPIDConditionally(path string, generation agyWaiterGeneration, pid *int) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	raw := map[string]any{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, err
	}
	state := agyWakeStateSubset{}
	if err := json.Unmarshal(data, &state); err != nil {
		return false, err
	}
	if !stateMatchesGeneration(state, generation) {
		return false, nil
	}
	if pid == nil {
		delete(raw, "waiterPid")
	} else {
		raw["waiterPid"] = *pid
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return false, err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return false, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return false, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return false, err
	}
	return true, nil
}

// launchIdentityFromState reads the Herdr identity Stop persisted on the state
// file. All four fields are required; a partial record is treated as missing
// so we fall through to live-process discovery.
func launchIdentityFromState(state agyWakeStateSubset) (AgyWakeLaunchIdentity, bool) {
	if state.HerdrSocketPath == nil || strings.TrimSpace(*state.HerdrSocketPath) == "" {
		return AgyWakeLaunchIdentity{}, false
	}
	if state.PaneID == nil || strings.TrimSpace(*state.PaneID) == "" {
		return AgyWakeLaunchIdentity{}, false
	}
	if state.WorkspaceID == nil || strings.TrimSpace(*state.WorkspaceID) == "" {
		return AgyWakeLaunchIdentity{}, false
	}
	if state.WaiterCwd == nil || strings.TrimSpace(*state.WaiterCwd) == "" {
		return AgyWakeLaunchIdentity{}, false
	}
	return AgyWakeLaunchIdentity{
		HerdrSocketPath: strings.TrimSpace(*state.HerdrSocketPath),
		PaneID:          strings.TrimSpace(*state.PaneID),
		WorkspaceID:     strings.TrimSpace(*state.WorkspaceID),
		Cwd:             strings.TrimSpace(*state.WaiterCwd),
	}, true
}

// discoverLiveAgyHerdrIdentity scans /proc for an agy process whose argv carries
// `--conversation <sessionID>` and reads its HERDR_* environ + cwd. Used when
// re-arming a state file that predates the persisted-identity fields.
func discoverLiveAgyHerdrIdentity(sessionID string) (AgyWakeLaunchIdentity, bool) {
	if sessionID == "" {
		return AgyWakeLaunchIdentity{}, false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return AgyWakeLaunchIdentity{}, false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pidName := entry.Name()
		if _, err := strconv.Atoi(pidName); err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", pidName, "cmdline"))
		if err != nil || !cmdlineHasConversation(cmdline, sessionID) {
			continue
		}
		environ, err := os.ReadFile(filepath.Join("/proc", pidName, "environ"))
		if err != nil {
			continue
		}
		envMap := parseProcEnviron(environ)
		if envMap["HERDR_ENV"] != "1" {
			continue
		}
		socketPath := strings.TrimSpace(envMap["HERDR_SOCKET_PATH"])
		paneID := strings.TrimSpace(envMap["HERDR_PANE_ID"])
		workspaceID := strings.TrimSpace(envMap["HERDR_WORKSPACE_ID"])
		if socketPath == "" || paneID == "" || workspaceID == "" {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", pidName, "cwd"))
		if err != nil || cwd == "" {
			continue
		}
		return AgyWakeLaunchIdentity{
			HerdrSocketPath: socketPath,
			PaneID:          paneID,
			WorkspaceID:     workspaceID,
			Cwd:             cwd,
			AutoOn:          strings.TrimSpace(envMap["HERDSMAN_AUTO_ON"]),
		}, true
	}
	return AgyWakeLaunchIdentity{}, false
}

// isLiveAgyWakeWaiter reports whether pid still names a running WakeWaiter of
// the given session: /proc/<pid>/cmdline must carry `agy-hook WakeWaiter`, the
// environ must match HERDSMAN_AGY_SESSION_ID, and the process must not be a
// zombie (stat state "Z"). A pid whose /proc entry is unreadable — gone,
// reaped, or owned by another uid — counts as dead.
func isLiveAgyWakeWaiter(pid int, sessionID string) bool {
	if pid <= 0 {
		return false
	}
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return false
	}
	// /proc/<pid>/stat: "pid (comm) state ..." — comm can itself contain
	// spaces and parentheses, so the state field is the first token after the
	// final ")".
	stateField := ""
	if closeParen := strings.LastIndexByte(string(stat), ')'); closeParen >= 0 && closeParen+2 < len(stat) {
		fields := strings.Fields(string(stat[closeParen+2:]))
		if len(fields) > 0 {
			stateField = fields[0]
		}
	}
	if stateField == "" || stateField == "Z" {
		return false
	}
	cmdline, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
	if err != nil {
		return false
	}
	args := strings.Split(string(cmdline), "\x00")
	sawAgyHook := false
	sawWakeWaiter := false
	for _, arg := range args {
		if arg == "agy-hook" {
			sawAgyHook = true
		}
		if arg == "WakeWaiter" {
			sawWakeWaiter = true
		}
	}
	if !sawAgyHook || !sawWakeWaiter {
		return false
	}
	if sessionID == "" {
		return true
	}
	environ, err := os.ReadFile(filepath.Join(procDir, "environ"))
	if err != nil {
		return false
	}
	return parseProcEnviron(environ)["HERDSMAN_AGY_SESSION_ID"] == sessionID
}

// cmdlineHasConversation reports whether a NUL-separated /proc cmdline contains
// `--conversation <sessionID>` (agy's flag form).
func cmdlineHasConversation(cmdline []byte, sessionID string) bool {
	parts := strings.Split(string(cmdline), "\x00")
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if part == "--conversation" && i+1 < len(parts) && parts[i+1] == sessionID {
			return true
		}
		if strings.HasPrefix(part, "--conversation=") && strings.TrimPrefix(part, "--conversation=") == sessionID {
			return true
		}
	}
	return false
}

// parseProcEnviron turns a /proc/<pid>/environ blob into a map.
func parseProcEnviron(blob []byte) map[string]string {
	out := map[string]string{}
	for _, entry := range strings.Split(string(blob), "\x00") {
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		out[key] = value
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// killAgyWaiterProcess terminates a waiter that lost the conditional-commit
// race: the generation it was spawned for is dead, so it must not deliver on
// the new turn's behalf. SIGKILL keeps this unconditional — a waiter that is
// stuck in a syscall would ignore SIGTERM and keep racing the new turn.
func killAgyWaiterProcess(pid int) {
	if pid <= 0 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Signal(syscall.SIGKILL)
}

// defaultAgyStateDir mirrors the hook's `${HERDSMAN_HOME:-$HOME/.herdsman}/agy-state`.
func defaultAgyStateDir(environment map[string]string) string {
	home := ""
	if environment != nil {
		home = environment["HERDSMAN_HOME"]
	}
	if home == "" {
		home = os.Getenv("HERDSMAN_HOME")
	}
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".herdsman")
		}
	}
	return filepath.Join(home, agyStateDirName)
}

// spawnAgyWakeWaiterProcess re-spawns the waiter as `herdsman agy-hook
// WakeWaiter`, detached into its own session (Setsid) like the hook's
// defaultSpawnWakeWaiter. Unlike the hook — which exits and reparents the
// child to PID 1 — the daemon stays alive, so we Wait in a goroutine to reap
// the child and avoid zombies when a waiter exits. The herdsman CLI binary is
// located next to the daemon binary first (they ship in the same directory),
// falling back to PATH.
//
// Under `go test` the spawn is refused by default: a suite that accidentally
// resolves the production /root/.local/bin/herdsman on PATH would otherwise
// leak real waiters onto the host (and a test binary on the resolved path
// would fork-bomb). Only a test that explicitly arms
// allowRealWaiterSpawnInTest — currently the near-production
// TestRearmAgyWaitersRealSubprocessStaysAlive — is allowed through, and the
// test-binary check below stays enforced even then.
func spawnAgyWakeWaiterProcess(input AgyWakeSpawnInput) (int, error) {
	if testing.Testing() && !allowRealWaiterSpawnInTest.Load() {
		return 0, fmt.Errorf("refusing to spawn an agy wake waiter under go test without allowRealWaiterSpawnInTest")
	}
	binary, err := herdsmanCLIPath()
	if err != nil {
		if testing.Testing() {
			// Same fork-bomb fuse as the hook's defaultSpawnWakeWaiter: inside
			// `go test` an unresolved binary must never fall through to exec.
			return 0, fmt.Errorf("herdsman agy wake re-arm cannot resolve a herdsman binary under go test: %w", err)
		}
		return 0, err
	}
	if isTestExecutable(binary) {
		// A test binary re-exec'd as `agy-hook WakeWaiter` ignores the
		// positional args and re-runs the whole suite in a detached session —
		// the fork bomb that OOMed the host. Tests that need a real child build
		// a herdsman binary and put it on PATH; anything resolving to *.test is
		// refused even with the test spawn gate armed.
		return 0, fmt.Errorf("refusing to re-arm an agy wake waiter from test binary %s", binary)
	}
	command := exec.Command(binary, "agy-hook", "WakeWaiter")
	command.Dir = input.Cwd
	command.Env = agyWaiterEnvironment(input)
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	pid := command.Process.Pid
	go func() { _ = command.Wait() }()
	return pid, nil
}

// herdsmanCLIPath finds the `herdsman` CLI binary: the daemon's own directory
// first (both binaries are installed side by side under
// /root/.local/lib/herdsman-go), then PATH.
func herdsmanCLIPath() (string, error) {
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "herdsman")
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	return exec.LookPath("herdsman")
}

// agyWaiterEnvironment mirrors the hook's wakeWaiterEnvironment: the base env
// minus any stale HERDSMAN_AGY_* waiter variables, plus the three this spawn
// sets. HERDR_* are expected to already be present on input.Environment (the
// re-arm path writes them before calling spawn).
func agyWaiterEnvironment(input AgyWakeSpawnInput) []string {
	environment := make([]string, 0, len(input.Environment)+3)
	for key, value := range input.Environment {
		if key == "HERDSMAN_AGY_SESSION_ID" || key == "HERDSMAN_AGY_STATE_DIR" || key == "HERDSMAN_AGY_WAITER_EPOCH" {
			continue
		}
		environment = append(environment, key+"="+value)
	}
	environment = append(environment,
		"HERDSMAN_AGY_SESSION_ID="+input.SessionID,
		"HERDSMAN_AGY_STATE_DIR="+input.StateDir,
		"HERDSMAN_AGY_WAITER_EPOCH="+formatAgyEpoch(input.WaiterEpoch),
	)
	return environment
}

// formatAgyEpoch formats a JS-number epoch the way the hook expects to read it
// back (integers without a decimal point, matching formatJSNumber). The previous
// TrimRight("%v") form mangled 10 → "1" and 100 → "1".
func formatAgyEpoch(value float64) string {
	if !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}
