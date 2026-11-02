package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRearmAgyWaitersRespawnsParkedSessions is the regression for the
// daemon-restart half of the wake loss (wake-miss2): a main pane parked in
// `awaitingNextTurn` loses its WakeWaiter when the daemon exits, and the new
// daemon used to leave it orphaned so later completions were written but never
// pushed. On startup the daemon must re-spawn a waiter for every parked
// session, with the Herdr launch identity + pane cwd Stop persisted.
func TestRearmAgyWaitersRespawnsParkedSessions(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A parked session: awaitingNextTurn=true, turnEpoch recorded, a stale
	// waiterPid from the waiter that died with the previous daemon, and the
	// Herdr launch identity Stop now persists for re-arm.
	write("sess-parked.json", `{
		"awaitingNextTurn":true,
		"turnEpoch":4,
		"waiterEpoch":4,
		"waiterPid":2868506,
		"deliveredEventIds":[80711],
		"herdrSocketPath":"/tmp/herdr.sock",
		"paneId":"wB:p1",
		"workspaceId":"wB",
		"waiterCwd":"/tmp/pane-cwd"
	}`)
	// A session that already completed a turn: not parked, must be left alone.
	write("sess-active.json", `{"awaitingNextTurn":false,"turnEpoch":2,"waiterPid":111}`)
	// A session with no awaiting flag at all.
	write("sess-fresh.json", `{"turnEpoch":1}`)
	// A corrupt file must not hold back the rest.
	write("sess-corrupt.json", `{not json`)
	// A non-state file is ignored.
	write("agy-wake-audit.jsonl", `{"kind":"x"}`+"\n")

	spawned := []AgyWakeSpawnInput{}
	count := RearmAgyWaiters(&RearmAgyWaitersOptions{
		StateDir: dir,
		Environment: map[string]string{
			"HERDSMAN_HOME": "/home/x/.herdsman",
			"PATH":          "/bin",
		},
		Spawn: func(input AgyWakeSpawnInput) (int, error) {
			spawned = append(spawned, input)
			return 4242, nil
		},
		Warn: func(string) {},
	})

	if count != 1 {
		t.Fatalf("rearmed = %d, want 1 (only the parked session)", count)
	}
	if len(spawned) != 1 || spawned[0].SessionID != "sess-parked" {
		t.Fatalf("spawned = %+v, want exactly sess-parked", spawned)
	}
	if spawned[0].WaiterEpoch != 4 {
		t.Fatalf("waiterEpoch = %v, want 4 (the parked turn epoch)", spawned[0].WaiterEpoch)
	}
	if spawned[0].StateDir != dir {
		t.Fatalf("stateDir = %q, want %q", spawned[0].StateDir, dir)
	}
	if spawned[0].Cwd != "/tmp/pane-cwd" {
		t.Fatalf("cwd = %q, want /tmp/pane-cwd (pane cwd, not the state dir)", spawned[0].Cwd)
	}
	if spawned[0].Environment["HERDR_ENV"] != "1" ||
		spawned[0].Environment["HERDR_SOCKET_PATH"] != "/tmp/herdr.sock" ||
		spawned[0].Environment["HERDR_PANE_ID"] != "wB:p1" ||
		spawned[0].Environment["HERDR_WORKSPACE_ID"] != "wB" {
		t.Fatalf("HERDR env incomplete: %+v", spawned[0].Environment)
	}

	// The state file must carry the new waiterPid (and keep every other field).
	// Leaving the dead pid in place is what made a re-armed waiter exit as
	// superseded before it could deliver anything.
	raw, err := os.ReadFile(filepath.Join(dir, "sess-parked.json"))
	if err != nil {
		t.Fatal(err)
	}
	parsed := map[string]any{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["waiterPid"] != float64(4242) {
		t.Fatalf("waiterPid = %v, want 4242 (the re-armed pid)", parsed["waiterPid"])
	}
	if parsed["awaitingNextTurn"] != true {
		t.Fatalf("awaitingNextTurn cleared; must stay true for the parked pane")
	}
	if parsed["turnEpoch"] != float64(4) || parsed["waiterEpoch"] != float64(4) {
		t.Fatalf("epochs mutated: %+v", parsed)
	}
	if parsed["herdrSocketPath"] != "/tmp/herdr.sock" || parsed["paneId"] != "wB:p1" {
		t.Fatalf("herdr identity stripped: %+v", parsed)
	}
	// Unrelated sessions must keep their original waiterPid untouched.
	active, err := os.ReadFile(filepath.Join(dir, "sess-active.json"))
	if err != nil {
		t.Fatal(err)
	}
	activeParsed := map[string]any{}
	if err := json.Unmarshal(active, &activeParsed); err != nil {
		t.Fatal(err)
	}
	if activeParsed["waiterPid"] != float64(111) {
		t.Fatalf("active session waiterPid = %v, want untouched 111", activeParsed["waiterPid"])
	}
}

// TestRearmAgyWaitersSkipsMissingLaunchIdentity ensures a parked session with
// no persisted Herdr identity and no live discovery result is skipped rather
// than spawned into the silent-exit path that produced zombies on f5fdf19.
func TestRearmAgyWaitersSkipsMissingLaunchIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-orphan.json")
	if err := os.WriteFile(path, []byte(`{"awaitingNextTurn":true,"turnEpoch":3,"waiterPid":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	spawned := 0
	warnings := []string{}
	count := RearmAgyWaiters(&RearmAgyWaitersOptions{
		StateDir: dir,
		Spawn: func(input AgyWakeSpawnInput) (int, error) {
			spawned++
			return 1, nil
		},
		DiscoverLive: func(string) (AgyWakeLaunchIdentity, bool) {
			return AgyWakeLaunchIdentity{}, false
		},
		Warn: func(message string) { warnings = append(warnings, message) },
	})
	if count != 0 || spawned != 0 {
		t.Fatalf("rearmed=%d spawned=%d, want both 0 when identity is missing", count, spawned)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "missing Herdr launch identity") {
		t.Fatalf("warnings = %v, want missing-identity notice", warnings)
	}
	// waiterPid must stay untouched when we refuse to spawn.
	raw, _ := os.ReadFile(path)
	parsed := map[string]any{}
	_ = json.Unmarshal(raw, &parsed)
	if parsed["waiterPid"] != float64(99) {
		t.Fatalf("waiterPid = %v, want untouched 99", parsed["waiterPid"])
	}
}

// TestRearmAgyWaitersDiscoversLiveIdentity covers older state files that predate
// the persisted herdrSocketPath/paneId/workspaceId/waiterCwd fields: a live agy
// process for the same conversation supplies the launch identity.
func TestRearmAgyWaitersDiscoversLiveIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sess-old.json"), []byte(`{"awaitingNextTurn":true,"turnEpoch":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var got AgyWakeSpawnInput
	count := RearmAgyWaiters(&RearmAgyWaitersOptions{
		StateDir: dir,
		Spawn: func(input AgyWakeSpawnInput) (int, error) {
			got = input
			return 77, nil
		},
		DiscoverLive: func(sessionID string) (AgyWakeLaunchIdentity, bool) {
			if sessionID != "sess-old" {
				t.Fatalf("discover session = %q, want sess-old", sessionID)
			}
			return AgyWakeLaunchIdentity{
				HerdrSocketPath: "/run/herdr.sock",
				PaneID:          "w9:p1",
				WorkspaceID:     "w9",
				Cwd:             "/work/pane",
				AutoOn:          "1",
			}, true
		},
		Warn: func(string) {},
	})
	if count != 1 {
		t.Fatalf("rearmed = %d, want 1", count)
	}
	if got.Cwd != "/work/pane" {
		t.Fatalf("cwd = %q, want /work/pane", got.Cwd)
	}
	if got.Environment["HERDR_PANE_ID"] != "w9:p1" || got.Environment["HERDSMAN_AUTO_ON"] != "1" {
		t.Fatalf("discovered env incomplete: %+v", got.Environment)
	}
}

// TestRearmAgyWaitersSkipsLiveWaiter covers the O-5 race: a state file that
// still records a waiterPid belonging to a genuinely live WakeWaiter for the
// same session must be left alone — clearing the pid would make the live
// waiter exit as superseded and spawn a duplicate.
func TestRearmAgyWaitersSkipsLiveWaiter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-live.json")
	if err := os.WriteFile(path, []byte(`{
		"awaitingNextTurn":true,
		"turnEpoch":6,
		"waiterEpoch":6,
		"waiterPid":4242,
		"herdrSocketPath":"/tmp/herdr.sock",
		"paneId":"wB:p1",
		"workspaceId":"wB",
		"waiterCwd":"/tmp/pane-cwd"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	spawned := 0
	count := RearmAgyWaiters(&RearmAgyWaitersOptions{
		StateDir: dir,
		Spawn: func(AgyWakeSpawnInput) (int, error) {
			spawned++
			return 1, nil
		},
		WaiterAlive: func(pid int, sessionID string) bool {
			if pid != 4242 || sessionID != "sess-live" {
				t.Fatalf("liveness probe = (%d,%q), want (4242,sess-live)", pid, sessionID)
			}
			return true
		},
		Warn: func(string) {},
	})
	if count != 0 || spawned != 0 {
		t.Fatalf("rearmed=%d spawned=%d, want both 0 while the recorded waiter lives", count, spawned)
	}
	// waiterPid must stay untouched: the live waiter still validates itself
	// against it.
	parsed := map[string]any{}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["waiterPid"] != float64(4242) {
		t.Fatalf("waiterPid = %v, want untouched 4242", parsed["waiterPid"])
	}
}

// TestRearmAgyWaitersKillsSupersededSpawn covers the O-2 conditional commit:
// when a concurrent turn replaces the parked generation between the
// pre-spawn clear and the post-spawn write, the just-spawned waiter belongs
// to a dead generation and must be killed instead of being counted.
func TestRearmAgyWaitersKillsSupersededSpawn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-race.json")
	if err := os.WriteFile(path, []byte(`{
		"awaitingNextTurn":true,
		"turnEpoch":3,
		"waiterEpoch":3,
		"waiterPid":111,
		"herdrSocketPath":"/tmp/herdr.sock",
		"paneId":"wB:p1",
		"workspaceId":"wB",
		"waiterCwd":"/tmp/pane-cwd"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	killed := []int{}
	spawned := []AgyWakeSpawnInput{}
	count := RearmAgyWaiters(&RearmAgyWaitersOptions{
		StateDir: dir,
		Spawn: func(input AgyWakeSpawnInput) (int, error) {
			spawned = append(spawned, input)
			// Simulate a concurrent new turn landing while the spawn was in
			// flight: it bumps the epoch and records its own waiterPid.
			if err := os.WriteFile(path, []byte(`{
				"awaitingNextTurn":true,
				"turnEpoch":4,
				"waiterEpoch":4,
				"waiterPid":9999,
				"herdrSocketPath":"/tmp/herdr.sock",
				"paneId":"wB:p1",
				"workspaceId":"wB",
				"waiterCwd":"/tmp/pane-cwd"
			}`), 0o600); err != nil {
				t.Fatalf("rewrite state mid-spawn: %v", err)
			}
			return 5555, nil
		},
		Kill: func(pid int) { killed = append(killed, pid) },
		WaiterAlive: func(int, string) bool {
			return false
		},
		Warn: func(string) {},
	})
	if count != 0 {
		t.Fatalf("rearmed = %d, want 0 for the superseded spawn", count)
	}
	if len(spawned) != 1 {
		t.Fatalf("spawned = %v, want exactly one spawn attempt", spawned)
	}
	if !reflect.DeepEqual(killed, []int{5555}) {
		t.Fatalf("killed = %v, want [5555] (the superseded waiter must not live)", killed)
	}
	// The new turn's own waiterPid survives: the re-arm's write was refused.
	parsed := map[string]any{}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["waiterPid"] != float64(9999) || parsed["waiterEpoch"] != float64(4) {
		t.Fatalf("state = %+v, want the new turn's epoch/pid preserved", parsed)
	}
}

// TestRearmAgyWaitersKillsSpawnWhenPidWriteFails covers the disk-error half
// of the O-2 second conditional commit: the pre-spawn clear succeeds and the
// waiter is spawned, but recording the new waiterPid fails. On disk the file
// still shows waiterPid=nil, so the just-spawned waiter has no recorded owner
// — leaving it running would orphan a live waiter that can race the next
// turn. The re-arm must kill the child it just created instead of counting
// the session.
func TestRearmAgyWaitersKillsSpawnWhenPidWriteFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-writefail.json")
	if err := os.WriteFile(path, []byte(`{
		"awaitingNextTurn":true,
		"turnEpoch":3,
		"waiterEpoch":3,
		"waiterPid":111,
		"herdrSocketPath":"/tmp/herdr.sock",
		"paneId":"wB:p1",
		"workspaceId":"wB",
		"waiterCwd":"/tmp/pane-cwd"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	killed := []int{}
	spawned := []AgyWakeSpawnInput{}
	count := RearmAgyWaiters(&RearmAgyWaitersOptions{
		StateDir: dir,
		Spawn: func(input AgyWakeSpawnInput) (int, error) {
			spawned = append(spawned, input)
			// Between spawn and the pid-write, turn the state file into a
			// directory: the conditional update still opens it successfully
			// for reading, but the atomic temp-file rename onto it fails — a
			// real disk-level write error, not a lost generation race.
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove state file mid-spawn: %v", err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatalf("replace state file with directory mid-spawn: %v", err)
			}
			return 7777, nil
		},
		Kill:        func(pid int) { killed = append(killed, pid) },
		WaiterAlive: func(int, string) bool { return false },
		Warn:        func(string) {},
	})
	if count != 0 {
		t.Fatalf("rearmed = %d, want 0 when the pid write fails", count)
	}
	if len(spawned) != 1 {
		t.Fatalf("spawned = %v, want exactly one spawn attempt", spawned)
	}
	if !reflect.DeepEqual(killed, []int{7777}) {
		t.Fatalf("killed = %v, want [7777] — a waiter whose pid never reached the state file must not be orphaned", killed)
	}
	// The state file is gone (now a directory): nothing was recorded, matching
	// the failed-write shape the kill guards against.
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("state path = %v (err=%v), want the directory that made the write fail", info, err)
	}
}

// TestRearmAgyWaitersAbortsOnConcurrentTurnBeforeSpawn covers the first
// conditional commit: when a new turn lands before the pre-spawn clear, the
// re-arm must not clear waiterPid nor spawn at all.
func TestRearmAgyWaitersAbortsOnConcurrentTurnBeforeSpawn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-flip.json")
	if err := os.WriteFile(path, []byte(`{
		"awaitingNextTurn":false,
		"turnEpoch":4,
		"waiterPid":222,
		"herdrSocketPath":"/tmp/herdr.sock",
		"paneId":"wB:p1",
		"workspaceId":"wB",
		"waiterCwd":"/tmp/pane-cwd"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	spawned := 0
	count := RearmAgyWaiters(&RearmAgyWaitersOptions{
		StateDir:    dir,
		Spawn:       func(AgyWakeSpawnInput) (int, error) { spawned++; return 1, nil },
		WaiterAlive: func(int, string) bool { return false },
		Warn:        func(string) {},
	})
	if count != 0 || spawned != 0 {
		t.Fatalf("rearmed=%d spawned=%d, want both 0 for an unparked session", count, spawned)
	}
	parsed := map[string]any{}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["waiterPid"] != float64(222) {
		t.Fatalf("waiterPid = %v, want untouched 222", parsed["waiterPid"])
	}
}

// TestIsLiveAgyWakeWaiter covers the /proc liveness probe: the current test
// process is alive and non-zombie but is not a WakeWaiter, and a pid that no
// longer exists is dead outright.
func TestIsLiveAgyWakeWaiter(t *testing.T) {
	if isLiveAgyWakeWaiter(os.Getpid(), "sess-x") {
		t.Fatal("test binary reported as a live WakeWaiter")
	}
	// A pid far above anything plausible must not count as alive.
	if isLiveAgyWakeWaiter(1<<30-1, "sess-x") {
		t.Fatal("nonexistent pid reported as a live WakeWaiter")
	}
	if isLiveAgyWakeWaiter(0, "sess-x") || isLiveAgyWakeWaiter(-1, "sess-x") {
		t.Fatal("non-positive pid reported as a live WakeWaiter")
	}
}

func TestRearmAgyWaitersEnvironmentStripsStaleWaiterVars(t *testing.T) {
	env := agyWaiterEnvironment(AgyWakeSpawnInput{
		SessionID:   "sess-1",
		StateDir:    "/tmp/state",
		WaiterEpoch: 7,
		Environment: map[string]string{
			"HERDSMAN_AGY_SESSION_ID":   "stale",
			"HERDSMAN_AGY_STATE_DIR":    "stale",
			"HERDSMAN_AGY_WAITER_EPOCH": "stale",
			"HERDSMAN_HOME":             "/home/x/.herdsman",
			"HERDR_ENV":                 "1",
		},
	})
	found := map[string]string{}
	for _, entry := range env {
		for _, key := range []string{"HERDSMAN_AGY_SESSION_ID", "HERDSMAN_AGY_STATE_DIR", "HERDSMAN_AGY_WAITER_EPOCH", "HERDSMAN_HOME", "HERDR_ENV"} {
			if len(entry) > len(key) && entry[:len(key)] == key && entry[len(key)] == '=' {
				found[key] = entry[len(key)+1:]
			}
		}
	}
	if found["HERDSMAN_AGY_SESSION_ID"] != "sess-1" {
		t.Fatalf("session id env = %q, want sess-1", found["HERDSMAN_AGY_SESSION_ID"])
	}
	if found["HERDSMAN_AGY_STATE_DIR"] != "/tmp/state" {
		t.Fatalf("state dir env = %q, want /tmp/state", found["HERDSMAN_AGY_STATE_DIR"])
	}
	if found["HERDSMAN_AGY_WAITER_EPOCH"] != "7" {
		t.Fatalf("waiter epoch env = %q, want 7", found["HERDSMAN_AGY_WAITER_EPOCH"])
	}
	if found["HERDSMAN_HOME"] != "/home/x/.herdsman" {
		t.Fatalf("HERDSMAN_HOME env = %q, want passthrough", found["HERDSMAN_HOME"])
	}
	if found["HERDR_ENV"] != "1" {
		t.Fatalf("HERDR_ENV env = %q, want passthrough 1", found["HERDR_ENV"])
	}
}

// TestFormatAgyEpochMatchesFormatJSNumber guards the TrimRight("%v") bug that
// turned epoch 10 into "1" (and 100 into "1"): the waiter would then refuse the
// spawn as a mismatched generation.
func TestFormatAgyEpochMatchesFormatJSNumber(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{0, "0"},
		{1, "1"},
		{7, "7"},
		{10, "10"},
		{11, "11"},
		{100, "100"},
		{4.5, "4.5"},
	}
	for _, tc := range cases {
		if got := formatAgyEpoch(tc.value); got != tc.want {
			t.Fatalf("formatAgyEpoch(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}
	env := agyWaiterEnvironment(AgyWakeSpawnInput{
		SessionID:   "sess-10",
		StateDir:    "/tmp/state",
		WaiterEpoch: 10,
		Environment: map[string]string{},
	})
	found := ""
	for _, entry := range env {
		if len(entry) > len("HERDSMAN_AGY_WAITER_EPOCH=") && entry[:len("HERDSMAN_AGY_WAITER_EPOCH=")] == "HERDSMAN_AGY_WAITER_EPOCH=" {
			found = entry[len("HERDSMAN_AGY_WAITER_EPOCH="):]
		}
	}
	if found != "10" {
		t.Fatalf("HERDSMAN_AGY_WAITER_EPOCH = %q, want 10 (not TrimRight-mangled 1)", found)
	}
}

// TestRearmAgyWaitersRealSubprocessStaysAlive is the near-production regression
// for wake-rearm4: spawn a real `herdsman agy-hook WakeWaiter` child the way the
// daemon does, and assert it is still alive (non-zombie) a few seconds later
// with state.waiterPid matching. On f5fdf19 this fails because re-arm omitted
// HERDR_* and used the state dir as cwd, so the child exited immediately.
func TestRearmAgyWaitersRealSubprocessStaysAlive(t *testing.T) {
	// The only sanctioned way through the go-test spawn fuse: this test wants
	// the production spawn path, so it arms the gate and resets it on exit.
	allowRealWaiterSpawnInTest.Store(true)
	t.Cleanup(func() { allowRealWaiterSpawnInTest.Store(false) })

	home := t.TempDir()
	stateDir := filepath.Join(home, "agy-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	paneCwd := t.TempDir()
	socketPath := filepath.Join(home, "herdsman.sock")
	if err := os.WriteFile(filepath.Join(home, "runtime.json"), []byte(fmt.Sprintf(
		`{"dbPath":%q,"homeDir":%q,"logPath":%q,"pidPath":%q,"socketPath":%q,"startedAt":"2026-01-01T00:00:00.000Z","version":1}`,
		filepath.Join(home, "state.db"), home, filepath.Join(home, "herdsman.log"), filepath.Join(home, "herdsman.pid"), socketPath,
	)), 0o600); err != nil {
		t.Fatal(err)
	}

	sessionID := "11111111-2222-3333-4444-555555555555"
	statePath := filepath.Join(stateDir, sessionID+".json")
	stateBody := fmt.Sprintf(`{
		"awaitingNextTurn":true,
		"turnEpoch":3,
		"waiterEpoch":3,
		"waiterPid":999001,
		"herdrSocketPath":"/tmp/herdr-rearm-test.sock",
		"paneId":"wTest:p1",
		"workspaceId":"wTest",
		"waiterCwd":%q
	}`, paneCwd)
	if err := os.WriteFile(statePath, []byte(stateBody), 0o600); err != nil {
		t.Fatal(err)
	}

	stopStub := startAgyWakeRPCStub(t, socketPath)
	defer stopStub()

	binDir := t.TempDir()
	herdsmanBin := filepath.Join(binDir, "herdsman")
	build := exec.Command("go", "build", "-o", herdsmanBin, "github.com/dorokuma/herdsman/go/cmd/herdsman")
	build.Dir = filepath.Join(findModuleRoot(t), "go")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build herdsman: %v\n%s", err, out)
	}

	pathEnv := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	// herdsmanCLIPath uses LookPath against this process's PATH (not the child
	// env map), so the freshly built binary must be visible here too.
	t.Setenv("PATH", pathEnv)
	count := RearmAgyWaiters(&RearmAgyWaitersOptions{
		StateDir: stateDir,
		Environment: map[string]string{
			"HERDSMAN_HOME": home,
			"HOME":          home,
			"PATH":          pathEnv,
			"USER":          "rearm-test",
		},
		// Real spawn — the production path under test.
		Spawn: nil,
		Warn:  func(string) {},
	})
	if count != 1 {
		t.Fatalf("rearmed = %d, want 1", count)
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	parsed := map[string]any{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	pidFloat, ok := parsed["waiterPid"].(float64)
	if !ok || pidFloat <= 0 {
		t.Fatalf("waiterPid = %v, want positive pid", parsed["waiterPid"])
	}
	pid := int(pidFloat)
	// Any assertion failure below must not leak the detached waiter or leave it
	// racing the next test; killing it here is the only reliable teardown.
	t.Cleanup(func() {
		proc, err := os.FindProcess(pid)
		if err == nil {
			_ = proc.Kill()
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		alive, zombie := procAliveState(pid)
		if !alive {
			t.Fatalf("re-armed waiter pid=%d exited within the survival window (f5fdf19-style silent exit)", pid)
		}
		if zombie {
			t.Fatalf("re-armed waiter pid=%d is a zombie (daemon did not Wait)", pid)
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Final consistency: state still points at the live pid.
	raw, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	parsed = map[string]any{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["waiterPid"] != float64(pid) {
		t.Fatalf("waiterPid drifted to %v, want %d", parsed["waiterPid"], pid)
	}

	// Tear down is already registered as t.Cleanup above: the waiter is
	// detached so only an explicit kill guarantees it cannot outlive the test
	// process, including on an early Fatal inside the survival window.
}

// startAgyWakeRPCStub listens on socketPath and answers the JSON-line RPC
// methods a WakeWaiter issues on startup / idle poll with enough of a
// connection state for the loop to keep running (no peers, not owner — just
// stay parked). The returned stop closes the listener *and* every accepted
// connection before waiting, so a mid-test failure cannot wedge wg.Wait on a
// connection the parked waiter keeps open (R6).
func startAgyWakeRPCStub(t *testing.T, socketPath string) (stop func()) {
	t.Helper()
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen %s: %v", socketPath, err)
	}
	var wg sync.WaitGroup
	var connsMu sync.Mutex
	conns := map[net.Conn]struct{}{}
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns[conn] = struct{}{}
			connsMu.Unlock()
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() {
					connsMu.Lock()
					delete(conns, c)
					connsMu.Unlock()
					c.Close()
				}()
				serveAgyWakeRPCStub(c)
			}(conn)
		}
	}()
	return func() {
		close(done)
		_ = listener.Close()
		connsMu.Lock()
		for conn := range conns {
			_ = conn.Close()
		}
		connsMu.Unlock()
		wg.Wait()
	}
}

func serveAgyWakeRPCStub(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue
		}
		result := stubResultForMethod(req.Method)
		frame, err := json.Marshal(map[string]any{"id": req.ID, "result": result})
		if err != nil {
			return
		}
		if _, err := conn.Write(append(frame, '\n')); err != nil {
			return
		}
	}
}

func stubResultForMethod(method string) any {
	// Owner must match presence or the waiter stops on the first
	// `!isLocalOwner` check even with an empty wake batch.
	presence := map[string]any{
		"connectedAt":      "2026-01-01T00:00:00.000Z",
		"herdrSessionName": "default",
		"paneId":           "wTest:p1",
		"subscriberId":     "agy:stub",
		"subscriberKind":   "agy",
		"terminalId":       "term-stub",
		"workspaceId":      "wTest",
	}
	state := map[string]any{
		"ackedEventId":     0,
		"herdrSessionName": "default",
		"workspaceId":      "wTest",
		"updatedAt":        "2026-01-01T00:00:00.000Z",
		"owner": map[string]any{
			"paneId":     "wTest:p1",
			"terminalId": "term-stub",
		},
	}
	switch method {
	case "agent.orchestrator.register", "agent.orchestrator.get":
		return map[string]any{
			"presence": presence,
			"state":    state,
			"events":   []any{},
		}
	case "agent.list":
		return map[string]any{"agents": []any{}}
	default:
		return map[string]any{}
	}
}

func procAliveState(pid int) (alive bool, zombie bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false, false
	}
	alive = true
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "State:") {
			// e.g. "State:\tZ (zombie)" or "State:\tS (sleeping)"
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == "Z" {
				zombie = true
			}
			break
		}
	}
	return alive, zombie
}

// findModuleRoot walks up from this test file's directory to the repo root that
// contains go/go.mod (the herdsman Go module).
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go", "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find herdsman module root (go/go.mod)")
		}
		dir = parent
	}
}
