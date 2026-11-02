package agy

// Fork-bomb regression tests for the 2026-11-02 OOM: in a Herdr pane
// (HERDR_ENV=1) `go test ./...` ran the agy package suite, and a Stop hook call
// reached defaultSpawnWakeWaiter, which re-exec'd os.Executable() — the
// `agy.test` binary — as `agy-hook WakeWaiter`. The test binary ignored the
// positional args and re-ran the whole suite, every rerun spawning more
// detached waiters: an exponential process storm. The fuses under test:
//
//  1. defaultSpawnWakeWaiter never execs under `go test` (testing.Testing()
//     and the .test/.test.exe executable-name guard both refuse);
//  2. the daemon's spawnAgyWakeWaiterProcess carries the same fuse plus the
//     resolved-binary check;
//  3. TestMain exits a stray `agy-hook` re-exec and strips the host env.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsTestExecutable(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp/go-build/agy.test", true},
		{"/tmp/go-build/agy.test.exe", true},
		{"agy.test", true},
		{"daemon.test.exe", true},
		{"/root/.local/lib/herdsman-go/herdsman", false},
		{"/usr/bin/herdsman.test.bin", false},
		{"/tmp/attest", false},
		{"/tmp/test", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isTestExecutable(tc.path); got != tc.want {
			t.Fatalf("isTestExecutable(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestDefaultSpawnWakeWaiterRefusesUnderGoTest(t *testing.T) {
	// os.Executable() inside `go test` is <pkg>.test, so the isTestExecutable
	// branch — not just testing.Testing() — must be what refuses here.
	executable, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	if !isTestExecutable(executable) {
		t.Fatalf("os.Executable() = %q, want a *.test binary under go test", executable)
	}
	r := &runner{}
	if got := r.defaultSpawnWakeWaiter(SpawnWakeWaiterInput{
		Cwd:         t.TempDir(),
		Environment: map[string]string{},
		SessionID:   "sess-guard",
		StateDir:    t.TempDir(),
		WaiterEpoch: 1,
	}); got != nil {
		t.Fatalf("defaultSpawnWakeWaiter = %+v, want nil under go test", got)
	}
}

func TestStopWithDefaultSpawnDoesNotSpawnWaiterUnderGoTest(t *testing.T) {
	dir := t.TempDir()
	sessionID := "aaaaaaaa-0000-4000-8000-000000000000"

	// A minimal fake client: Stop registers presence, is told this pane owns
	// the workspace, finds no delivered ids and no batch, then spawns the
	// standby waiter — the code path that fork-bombed when the spawn was not
	// fused.
	client := &fakeGuardClient{respond: func(method string, params any) (any, error) {
		if method == "agent.list" {
			return map[string]any{"agents": []any{}}, nil
		}
		return map[string]any{
			"events": []any{},
			"presence": map[string]any{
				"herdrSessionName": "default", "paneId": "wB:p1", "subscriberId": "agy:" + sessionID,
				"terminalId": "term_self", "workspaceId": "wB",
			},
			"state": map[string]any{
				"ackedEventId": 0, "herdrSessionName": "default",
				"owner":     map[string]any{"paneId": "wB:p1", "terminalId": "term_self"},
				"updatedAt": "2026-11-02T00:00:00.000Z", "workspaceId": "wB",
			},
		}, nil
	}}

	// HERDR_ENV=1 launch identity, no injected SpawnWakeWaiter: the production
	// defaultSpawnWakeWaiter is the only spawn path, and it must refuse.
	output := RunAgyHook(HookStop, `{"sessionId":"`+sessionID+`","terminationReason":"NO_TOOL_CALL"}`, RunAgyHookOptions{
		Connect: func(string, func(string, any)) (RpcClient, error) { return client, nil },
		Environment: map[string]string{
			"HERDR_ENV":          "1",
			"HERDR_PANE_ID":      "wB:p1",
			"HERDR_SOCKET_PATH":  "/tmp/herdr.sock",
			"HERDR_WORKSPACE_ID": "wB",
		},
		SocketPath: "/tmp/herdsman.sock",
		StateDir:   &dir,
	})
	if output.Decision != nil || output.Reason != nil || len(output.InjectSteps) != 0 {
		t.Fatalf("output = %+v, want {}", output)
	}
	// The state file records the armed turn but waiterPid must stay unset: a
	// refused spawn yields a nil result and no pid is ever written. (A spawned
	// waiter would also still be running; the fuse makes that impossible to
	// observe directly, so the absent pid is the observable contract.)
	data, err := os.ReadFile(filepath.Join(dir, sessionID+".json"))
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	if strings.Contains(string(data), `"waiterPid"`) {
		t.Fatalf("state carries waiterPid — a waiter was spawned under go test:\n%s", data)
	}
	if !strings.Contains(string(data), `"awaitingNextTurn":true`) {
		t.Fatalf("state lacks awaitingNextTurn — Stop did not arm the turn:\n%s", data)
	}
}

// fakeGuardClient is the minimal rpcClient the Stop path drives.
type fakeGuardClient struct {
	respond func(method string, params any) (any, error)
}

func (c *fakeGuardClient) Close() {}

func (c *fakeGuardClient) Call(method string, params any) (any, error) {
	return c.respond(method, params)
}
