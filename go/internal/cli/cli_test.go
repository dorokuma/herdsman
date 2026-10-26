package cli

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/agy"
	"github.com/dorokuma/herdsman/go/internal/daemon"
)

// This file ports test/unit/cli.test.ts plus the CLI case of
// test/unit/config-runtime.test.ts.

// fakeCall mirrors the TS `calls` array entry: a method+params pair or the
// `["close"]` marker.
type fakeCall struct {
	Close  bool
	Method string
	Params any
}

type fakeClient struct {
	calls   []fakeCall
	respond func(method string, params any) (any, error)
}

func (c *fakeClient) Close() {
	c.calls = append(c.calls, fakeCall{Close: true})
}

func (c *fakeClient) Call(method string, params any) (any, error) {
	c.calls = append(c.calls, fakeCall{Method: method, Params: params})
	return c.respond(method, params)
}

// createFakeClient mirrors the TS createFakeClient helper: the same agent list,
// agent get and agent read payloads, with an overridable `name`.
func createFakeClient(name any) *fakeClient {
	client := &fakeClient{}
	client.respond = func(method string, params any) (any, error) {
		switch method {
		case "agent.list":
			return map[string]any{
				"agents": []any{
					map[string]any{
						"agent":       "codex",
						"agentStatus": "idle",
						"history": map[string]any{
							"lastAssistantMessage": map[string]any{"text": "done", "timestamp": nil, "ref": "r2"},
							"lastUserMessage":      map[string]any{"text": "fix bug", "timestamp": nil, "ref": "r1"},
							"source":               "pi-jsonl",
							"updatedAt":            "2026-07-22T00:00:00.000Z",
						},
						"name":   name,
						"paneId": "wB:p1",
					},
					map[string]any{
						"agent":       "codex",
						"agentStatus": "idle",
						"history":     map[string]any{},
						"name":        nil,
						"paneId":      "wB:p2",
					},
				},
			}, nil
		case "agent.get":
			return map[string]any{
				"agent": map[string]any{
					"agent":            "codex",
					"agentStatus":      "idle",
					"herdrSessionName": "default",
					"history":          map[string]any{},
					"name":             name,
					"paneId":           "wB:p1",
					"terminalId":       "term_1",
					"workspaceId":      "wB",
				},
			}, nil
		case "agent.read":
			return map[string]any{
				"agent": map[string]any{
					"agent":    "codex",
					"messages": []any{},
					"name":     name,
					"paneId":   "wB:p1",
				},
			}, nil
		}
		return map[string]any{}, nil
	}
	return client
}

func TestParseCliArgsAgentListWithCurrentHerdrWorkspace(t *testing.T) {
	command, err := ParseCliArgs([]string{"agent", "list"}, map[string]string{
		"HERDR_ENV":          "1",
		"HERDR_WORKSPACE_ID": "wB",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := CliCommand{Command: CommandAgentList, WorkspaceID: strPtr("wB")}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %+v, want %+v", command, want)
	}
}

func TestParseCliArgsExplicitAgentScopes(t *testing.T) {
	command, err := ParseCliArgs([]string{"agent", "list", "--all", "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(command, CliCommand{All: true, Command: CommandAgentList, JSON: true}) {
		t.Fatalf("command = %+v", command)
	}

	command, err = ParseCliArgs([]string{"agent", "list", "--workspace", "wB", "--session", "default"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := CliCommand{
		Command:          CommandAgentList,
		HerdrSessionName: strPtr("default"),
		WorkspaceID:      strPtr("wB"),
	}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %+v, want %+v", command, want)
	}

	command, err = ParseCliArgs([]string{"agent", "list", "--session", "default"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want = CliCommand{Command: CommandAgentList, HerdrSessionName: strPtr("default")}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %+v, want %+v", command, want)
	}
}

// TestParseCliArgsAgentListAllDropsExplicitScope is the regression for the
// TS `all ? { all: true } : scopedOrCurrent(...)` spread: an explicit --session
// / --workspace is replaced by `all`, never sent next to it. The Go parser used
// to keep both, so the RPC carried `all: true` together with the scope.
//
// Argument order must not matter, because TS reads --session / --workspace before
// the subcommand branch that consumes --all.
func TestParseCliArgsAgentListAllDropsExplicitScope(t *testing.T) {
	for _, args := range [][]string{
		{"agent", "list", "--session", "default", "--workspace", "wA", "--all"},
		{"agent", "list", "--all", "--session", "default", "--workspace", "wA"},
	} {
		command, err := ParseCliArgs(args, map[string]string{
			"HERDR_ENV":          "1",
			"HERDR_WORKSPACE_ID": "wB",
		})
		if err != nil {
			t.Fatalf("ParseCliArgs(%v): %v", args, err)
		}
		if want := (CliCommand{All: true, Command: CommandAgentList}); !reflect.DeepEqual(command, want) {
			t.Fatalf("ParseCliArgs(%v) = %+v, want %+v", args, command, want)
		}
		if params := scopeParams(command); !reflect.DeepEqual(params, map[string]any{"all": true}) {
			t.Fatalf("scopeParams = %+v, want {all:true}", params)
		}
	}

	// The explicit scope still survives without --all, so the clearing above is
	// specific to --all.
	command, err := ParseCliArgs([]string{"agent", "list", "--session", "default", "--workspace", "wA"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := CliCommand{Command: CommandAgentList, HerdrSessionName: strPtr("default"), WorkspaceID: strPtr("wA")}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %+v, want %+v", command, want)
	}

	// And --all on its own keeps the plain { all: true } shape.
	command, err = ParseCliArgs([]string{"agent", "list", "--all", "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (CliCommand{All: true, Command: CommandAgentList, JSON: true}); !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %+v, want %+v", command, want)
	}
}

// TestParseJSNumberAcceptsRadixPrefixes is the regression for the JS radix
// prefixes strconv.ParseFloat rejects: `Number("0x14")` is 20, `Number("0b101")`
// is 5 and `Number("0o17")` is 15. A sign in front of the prefix is not accepted
// by JS either (`Number("-0x14")` is a NaN), and a non-digit in the body fails.
func TestParseJSNumberAcceptsRadixPrefixes(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"0x14", 20},
		{"0X14", 20},
		{"0b101", 5},
		{"0B101", 5},
		{"0o17", 15},
		{"0O17", 15},
		{"0x14g", math.NaN()},
		{"0b2", math.NaN()},
		{"0o18", math.NaN()},
		{"0x", math.NaN()},
		{"-0x14", math.NaN()},
		{"+0x14", math.NaN()},
		{"0", 0},
		{"", 0},
		{"  12  ", 12},
		{"1.5", 1.5},
	}
	for _, testCase := range cases {
		got := parseJSNumber(testCase.in)
		if math.IsNaN(testCase.want) {
			if !math.IsNaN(got) {
				t.Errorf("parseJSNumber(%q) = %v, want NaN", testCase.in, got)
			}
			continue
		}
		if got != testCase.want {
			t.Errorf("parseJSNumber(%q) = %v, want %v", testCase.in, got, testCase.want)
		}
	}
}

// TestParseCliArgsAgentReadAcceptsRadixPrefixLimit is the production surface of
// the same gap: `--limit` runs through `Number(...)`, so `0x14` is 20 rather than
// a rejected value.
func TestParseCliArgsAgentReadAcceptsRadixPrefixLimit(t *testing.T) {
	command, err := ParseCliArgs([]string{"agent", "read", "wB:p2", "--limit", "0x14"}, map[string]string{
		"HERDR_ENV":          "1",
		"HERDR_WORKSPACE_ID": "wB",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := CliCommand{Command: CommandAgentRead, Limit: intPtr(20), Target: "wB:p2", WorkspaceID: strPtr("wB")}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %+v, want %+v", command, want)
	}

	// A signed prefix is still rejected by the range check, because JS reads it as
	// a NaN and NaN is not an integer.
	if _, err := ParseCliArgs([]string{"agent", "read", "wB:p2", "--limit", "-0x14"}, nil); err == nil {
		t.Fatal("error = nil, want the --limit range failure")
	}
}

func TestParseCliArgsAgentGetAndRead(t *testing.T) {
	command, err := ParseCliArgs([]string{"agent", "get", "claude", "--json"}, map[string]string{
		"HERDR_ENV":          "1",
		"HERDR_WORKSPACE_ID": "wB",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := CliCommand{Command: CommandAgentGet, JSON: true, Target: "claude", WorkspaceID: strPtr("wB")}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %+v, want %+v", command, want)
	}

	command, err = ParseCliArgs([]string{"agent", "get", "claude", "--session", "default", "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want = CliCommand{Command: CommandAgentGet, HerdrSessionName: strPtr("default"), JSON: true, Target: "claude"}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %+v, want %+v", command, want)
	}

	command, err = ParseCliArgs([]string{"agent", "read", "wB:p2", "--limit", "20", "--json"}, map[string]string{
		"HERDR_ENV":          "1",
		"HERDR_WORKSPACE_ID": "wB",
	})
	if err != nil {
		t.Fatal(err)
	}
	want = CliCommand{Command: CommandAgentRead, JSON: true, Limit: intPtr(20), Target: "wB:p2", WorkspaceID: strPtr("wB")}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %+v, want %+v", command, want)
	}

	if _, err := ParseCliArgs([]string{"agent", "read", "wB:p2", "--limit", "0"}, nil); err == nil ||
		err.Error() != "--limit must be between 1 and 500" {
		t.Fatalf("error = %v, want the --limit range failure", err)
	}
}

func TestParseCliArgsRejectsUnknownCommands(t *testing.T) {
	_, err := ParseCliArgs([]string{"legacy-command"}, nil)
	if err == nil || !strings.Contains(err.Error(), "Unknown command") {
		t.Fatalf("error = %v, want an Unknown command failure", err)
	}
	_, err = ParseCliArgs([]string{"agent", "sessions"}, nil)
	if err == nil || err.Error() != "Unknown agent command: sessions" {
		t.Fatalf("error = %v, want an Unknown agent command failure", err)
	}
}

func TestParseCliArgsKeepsOnlyTheReadOnlyDaemonStatusAction(t *testing.T) {
	for _, action := range []string{"start", "stop", "restart"} {
		_, err := ParseCliArgs([]string{"daemon", action}, nil)
		if err == nil || err.Error() != "Unknown daemon action: "+action {
			t.Fatalf("daemon %s error = %v", action, err)
		}
	}

	command, err := ParseCliArgs([]string{"daemon", "status"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(command, CliCommand{Action: "status", Command: CommandDaemon}) {
		t.Fatalf("command = %+v", command)
	}

	command, err = ParseCliArgs([]string{"daemon"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(command, CliCommand{Action: "status", Command: CommandDaemon}) {
		t.Fatalf("command = %+v", command)
	}

	command, err = ParseCliArgs([]string{"daemon", "status", "--text"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(command, CliCommand{Action: "status", Command: CommandDaemon, Text: true}) {
		t.Fatalf("command = %+v", command)
	}

	if _, err := ParseCliArgs([]string{"daemon", "status", "--bogus"}, nil); err == nil ||
		err.Error() != "Invalid argument: --bogus" {
		t.Fatalf("error = %v, want an Invalid argument failure", err)
	}

	help := HelpText()
	if !strings.Contains(help, "herdsman daemon status") {
		t.Fatalf("help text misses the daemon status line:\n%s", help)
	}
	// The two supervision facts are user-visible, so their caveats ship with the
	// help text itself.
	if !strings.Contains(help, "unmanaged") || !strings.Contains(help, "NRestarts") {
		t.Fatalf("help text misses the supervision caveats:\n%s", help)
	}
	for _, action := range []string{"start", "stop", "restart"} {
		if strings.Contains(help, "daemon "+action) {
			t.Fatalf("help text still lists `daemon %s`", action)
		}
	}
}

func TestParseCliArgsAgyHookAndOrchestrator(t *testing.T) {
	herdrEnvironment := map[string]string{
		"HERDR_ENV":          "1",
		"HERDR_PANE_ID":      "wB:p1",
		"HERDR_SOCKET_PATH":  "/tmp/h.sock",
		"HERDR_WORKSPACE_ID": "wB",
	}

	command, err := ParseCliArgs([]string{"on", "--if-unowned", "--json"}, herdrEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	want := CliCommand{
		Action:          "on",
		Command:         CommandOrchestrator,
		HerdrSocketPath: "/tmp/h.sock",
		IfUnowned:       true,
		JSON:            true,
		PaneID:          "wB:p1",
		WorkspaceID:     strPtr("wB"),
	}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("command = %+v, want %+v", command, want)
	}

	command, err = ParseCliArgs([]string{"off"}, herdrEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if command.Action != "off" || command.IfUnowned {
		t.Fatalf("command = %+v", command)
	}

	// `--if-unowned` only exists for `on`, and the Herdr pane identity is
	// required for every orchestrator action.
	if _, err := ParseCliArgs([]string{"status", "--if-unowned"}, herdrEnvironment); err == nil ||
		err.Error() != "Invalid argument: --if-unowned" {
		t.Fatalf("error = %v", err)
	}
	if _, err := ParseCliArgs([]string{"on"}, map[string]string{}); err == nil ||
		err.Error() != currentHerdrPaneError {
		t.Fatalf("error = %v, want the current-Herdr-pane failure", err)
	}
	if _, err := ParseCliArgs([]string{"agent", "list"}, map[string]string{}); err == nil ||
		err.Error() != currentHerdrWorkspaceError {
		t.Fatalf("error = %v, want the current-Herdr-workspace failure", err)
	}

	if command, err = ParseCliArgs([]string{"agy-hook", "Stop"}, nil); err != nil || command.HookEventName != agy.HookStop {
		t.Fatalf("command = %+v, err = %v", command, err)
	}
	for _, args := range [][]string{{"agy-hook"}, {"agy-hook", "Bogus"}} {
		if _, err := ParseCliArgs(args, nil); err == nil ||
			err.Error() != "agy-hook requires PreInvocation, Stop, or WakeWaiter" {
			t.Fatalf("args %v error = %v", args, err)
		}
	}
	if _, err := ParseCliArgs([]string{"agy-hook", "Stop", "extra"}, nil); err == nil ||
		err.Error() != "Invalid argument: extra" {
		t.Fatalf("error = %v", err)
	}

	if command, err = ParseCliArgs([]string{"--help"}, nil); err != nil || command.Command != CommandHelp {
		t.Fatalf("command = %+v, err = %v", command, err)
	}
	if command, err = ParseCliArgs(nil, nil); err != nil || command.Command != CommandHelp {
		t.Fatalf("command = %+v, err = %v", command, err)
	}
}

func TestFormatDaemonStatusRendersSupervisionFacts(t *testing.T) {
	got := FormatDaemonStatus(DaemonStatusView{
		PidFileExisted: true,
		Status: daemon.DaemonStatus{
			ManagedBy:       daemon.SystemdManagedBy(daemon.HerdsmanSystemdUnit),
			Pid:             intPtr(4242),
			PidPath:         "/tmp/herdsman.pid",
			RestartCount:    intPtr(3),
			SocketPath:      "/tmp/herdsman.sock",
			SocketReachable: true,
			State:           "running",
		},
	})
	want := strings.Join([]string{
		"state: running",
		"pid: 4242",
		"socketReachable: true",
		"managedBy: systemd:herdsman.service",
		"restartCount: 3",
	}, "\n")
	if got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}

	// Managed but the probe failed: the missing count is spelled out.
	got = FormatDaemonStatus(DaemonStatusView{
		PidFileExisted: true,
		Status: daemon.DaemonStatus{
			ManagedBy:       daemon.SystemdManagedBy(daemon.HerdsmanSystemdUnit),
			Pid:             intPtr(4242),
			PidPath:         "/tmp/herdsman.pid",
			SocketPath:      "/tmp/herdsman.sock",
			SocketReachable: true,
			State:           "running",
		},
	})
	if !strings.Contains(got, "managedBy: systemd:herdsman.service\nrestartCount: unavailable") {
		t.Fatalf("status text = %q, want the unavailable restart count", got)
	}

	// Not the herdsman system unit: `NRestarts` belongs to that unit, so the text
	// view keeps it out entirely — same as the JSON.
	for _, managedBy := range []string{"unmanaged", "unknown"} {
		got = FormatDaemonStatus(DaemonStatusView{
			PidFileExisted: true,
			Status: daemon.DaemonStatus{
				ManagedBy:       daemon.DaemonManagedBy(managedBy),
				Pid:             intPtr(4242),
				PidPath:         "/tmp/herdsman.pid",
				SocketPath:      "/tmp/herdsman.sock",
				SocketReachable: true,
				State:           "running",
			},
		})
		want = strings.Join([]string{
			"state: running",
			"pid: 4242",
			"socketReachable: true",
			"managedBy: " + managedBy,
		}, "\n")
		if got != want {
			t.Fatalf("status text = %q, want %q", got, want)
		}
	}

	// No daemon pid: the supervision facts are absent from the text output too.
	got = FormatDaemonStatus(DaemonStatusView{
		Status: daemon.DaemonStatus{
			PidPath:    "/tmp/herdsman.pid",
			SocketPath: "/tmp/herdsman.sock",
			State:      "stopped",
		},
	})
	if got != "state: stopped" {
		t.Fatalf("status text = %q", got)
	}
}

// The stale-pid branch is the one the TS union expresses through its shape: a
// pid file with no usable pid reports `stalePid` (NaN when the file content is
// not a number) and no socket probe line when the socket is unreachable.
func TestFormatDaemonStatusReportsStalePidBranches(t *testing.T) {
	got := FormatDaemonStatus(DaemonStatusView{
		PidFileExisted: true,
		Status: daemon.DaemonStatus{
			PidPath:         "/tmp/herdsman.pid",
			SocketPath:      "/tmp/herdsman.sock",
			SocketReachable: true,
			StalePid:        intPtr(999),
			State:           "running",
		},
	})
	want := strings.Join([]string{
		"state: running",
		"stalePid: 999",
		"socketReachable: true",
	}, "\n")
	if got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}

	got = FormatDaemonStatus(DaemonStatusView{
		PidFileExisted: true,
		Status: daemon.DaemonStatus{
			PidPath:    "/tmp/herdsman.pid",
			SocketPath: "/tmp/herdsman.sock",
			State:      "stopped",
		},
	})
	want = strings.Join([]string{
		"state: stopped",
		"stalePid: NaN",
	}, "\n")
	if got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}
}

func TestDaemonStatusJSONKeepsTheUnionMemberKeyShapes(t *testing.T) {
	cases := []struct {
		name string
		view DaemonStatusView
		want string
	}{
		{
			name: "running with a missing pid file",
			view: DaemonStatusView{
				Status: daemon.DaemonStatus{
					PidFileMissing:  true,
					PidPath:         "/tmp/herdsman.pid",
					SocketPath:      "/tmp/herdsman.sock",
					SocketReachable: true,
					State:           "running",
				},
			},
			want: `{"pidPath":"/tmp/herdsman.pid","pidFileMissing":true,"socketPath":"/tmp/herdsman.sock","socketReachable":true,"state":"running"}`,
		},
		{
			name: "stopped without a pid file",
			view: DaemonStatusView{
				Status: daemon.DaemonStatus{
					PidPath:    "/tmp/herdsman.pid",
					SocketPath: "/tmp/herdsman.sock",
					State:      "stopped",
				},
			},
			want: `{"pidPath":"/tmp/herdsman.pid","socketPath":"/tmp/herdsman.sock","state":"stopped"}`,
		},
		{
			name: "stale pid with a reachable socket",
			view: DaemonStatusView{
				PidFileExisted: true,
				Status: daemon.DaemonStatus{
					PidPath:         "/tmp/herdsman.pid",
					SocketPath:      "/tmp/herdsman.sock",
					SocketReachable: true,
					StalePid:        intPtr(999),
					State:           "running",
				},
			},
			want: `{"pidPath":"/tmp/herdsman.pid","socketPath":"/tmp/herdsman.sock","socketReachable":true,"stalePid":999,"state":"running"}`,
		},
		{
			name: "stale pid without a usable number",
			view: DaemonStatusView{
				PidFileExisted: true,
				Status: daemon.DaemonStatus{
					PidPath:    "/tmp/herdsman.pid",
					SocketPath: "/tmp/herdsman.sock",
					State:      "stopped",
				},
			},
			want: `{"pidPath":"/tmp/herdsman.pid","socketPath":"/tmp/herdsman.sock","stalePid":null,"state":"stopped"}`,
		},
		{
			name: "running daemon with supervision facts",
			view: DaemonStatusView{
				PidFileExisted: true,
				Status: daemon.DaemonStatus{
					ManagedBy:       daemon.SystemdManagedBy(daemon.HerdsmanSystemdUnit),
					Pid:             intPtr(4242),
					PidPath:         "/tmp/herdsman.pid",
					RestartCount:    intPtr(3),
					SocketPath:      "/tmp/herdsman.sock",
					SocketReachable: true,
					State:           "running",
				},
			},
			want: `{"managedBy":"systemd:herdsman.service","restartCount":3,"pid":4242,"pidPath":"/tmp/herdsman.pid","socketPath":"/tmp/herdsman.sock","socketReachable":true,"state":"running"}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := DaemonStatusJSON(testCase.view)
			if err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf("status JSON = %s, want %s", got, testCase.want)
			}
		})
	}
}

func TestHelpTextRendersAgentCommands(t *testing.T) {
	help := HelpText()
	for _, expected := range []string{
		"herdsman agent list",
		"herdsman agent get <target>",
		"herdsman agent read <target>",
		"herdsman help",
	} {
		if !strings.Contains(help, expected) {
			t.Fatalf("help text misses %q:\n%s", expected, help)
		}
	}
}

// TestRunCliCommandJSONKeepsLineSeparatorsLiteral is the regression for the
// U+2028 / U+2029 escaping gap in the CLI JSON output: encoding/json writes both
// as `\u2028` / `\u2029` even with SetEscapeHTML(false), while JS's
// JSON.stringify leaves them literal, so `agent list --json` differed from the TS
// output byte for byte for any agent name carrying either character.
func TestRunCliCommandJSONKeepsLineSeparatorsLiteral(t *testing.T) {
	name := "line1\u2028line2\u2029line3"
	client := createFakeClient(name)
	output := []string{}
	err := RunCliCommand(CliCommand{All: true, Command: CommandAgentList, JSON: true}, RunCliDeps{
		Connect:    func(string, func(string, any)) (RpcClientLike, error) { return client, nil },
		Output:     func(line string) { output = append(output, line) },
		SocketPath: "/tmp/s.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 1 {
		t.Fatalf("output = %v, want one line", output)
	}
	if strings.Contains(output[0], `\u2028`) || strings.Contains(output[0], `\u2029`) {
		t.Fatalf("output still escapes the separators: %s", output[0])
	}
	if !strings.Contains(output[0], `"name":"line1 line2 line3"`) {
		t.Fatalf("output = %s, want the literal separators in the name", output[0])
	}

	// The same name through `agent get --json`, which goes through the same
	// marshalJS seam.
	getOutput := []string{}
	err = RunCliCommand(CliCommand{Command: CommandAgentGet, JSON: true, Target: "reviewer"}, RunCliDeps{
		Connect:    func(string, func(string, any)) (RpcClientLike, error) { return client, nil },
		Output:     func(line string) { getOutput = append(getOutput, line) },
		SocketPath: "/tmp/s.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(getOutput) != 1 {
		t.Fatalf("getOutput = %v, want one line", getOutput)
	}
	if strings.Contains(getOutput[0], `\u2028`) || strings.Contains(getOutput[0], `\u2029`) {
		t.Fatalf("getOutput still escapes the separators: %s", getOutput[0])
	}
}

func TestRunCliCommandDispatchesAgentJSONCommands(t *testing.T) {
	client := createFakeClient("reviewer")
	output := []string{}
	err := RunCliCommand(CliCommand{
		Command:     CommandAgentRead,
		JSON:        true,
		Limit:       intPtr(10),
		Target:      "claude",
		WorkspaceID: strPtr("wB"),
	}, RunCliDeps{
		Connect:    func(string, func(string, any)) (RpcClientLike, error) { return client, nil },
		Output:     func(line string) { output = append(output, line) },
		SocketPath: "/tmp/s.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []fakeCall{
		{Method: "agent.read", Params: map[string]any{"limit": 10, "target": "claude", "workspaceId": "wB"}},
		{Close: true},
	}
	if !reflect.DeepEqual(client.calls, wantCalls) {
		t.Fatalf("calls = %+v, want %+v", client.calls, wantCalls)
	}
	if len(output) != 1 {
		t.Fatalf("output = %v, want one line", output)
	}
	if !strings.Contains(output[0], `"agent":{"agent":"codex","messages":[],"name":"reviewer","paneId":"wB:p1"}`) {
		t.Fatalf("output[0] = %s", output[0])
	}
}

func TestRunCliCommandRendersHumanAgentList(t *testing.T) {
	client := createFakeClient("reviewer")
	output := []string{}
	err := RunCliCommand(CliCommand{Command: CommandAgentList, WorkspaceID: strPtr("wB")}, RunCliDeps{
		Connect:    func(string, func(string, any)) (RpcClientLike, error) { return client, nil },
		Output:     func(line string) { output = append(output, line) },
		SocketPath: "/tmp/s.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 1 {
		t.Fatalf("output = %v", output)
	}
	if !strings.Contains(output[0], "status\tname\tagent\tpane\tlast user\tlast assistant\tupdated") {
		t.Fatalf("output[0] misses the header: %s", output[0])
	}
	if !strings.Contains(output[0], "idle\treviewer\tcodex\twB:p1\tfix bug\tdone") {
		t.Fatalf("output[0] misses the first row: %s", output[0])
	}
	if !strings.Contains(output[0], "idle\t\tcodex\twB:p2") {
		t.Fatalf("output[0] misses the unnamed row: %s", output[0])
	}
}

func TestRunCliCommandRendersLiveNameAndAgentKindInHumanGetAndRead(t *testing.T) {
	client := createFakeClient("reviewer")
	getOutput := []string{}
	err := RunCliCommand(CliCommand{Command: CommandAgentGet, Target: "reviewer", WorkspaceID: strPtr("wB")}, RunCliDeps{
		Connect:    func(string, func(string, any)) (RpcClientLike, error) { return client, nil },
		Output:     func(line string) { getOutput = append(getOutput, line) },
		SocketPath: "/tmp/s.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(getOutput[0], "name: reviewer\nagent: codex") {
		t.Fatalf("get output = %s", getOutput[0])
	}

	readOutput := []string{}
	err = RunCliCommand(CliCommand{Command: CommandAgentRead, Target: "reviewer", WorkspaceID: strPtr("wB")}, RunCliDeps{
		Connect:    func(string, func(string, any)) (RpcClientLike, error) { return client, nil },
		Output:     func(line string) { readOutput = append(readOutput, line) },
		SocketPath: "/tmp/s.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readOutput[0], "name: reviewer\nagent: codex\npane: wB:p1") {
		t.Fatalf("read output = %s", readOutput[0])
	}

	unnamed := createFakeClient(nil)
	unnamedOutput := []string{}
	err = RunCliCommand(CliCommand{Command: CommandAgentGet, Target: "codex", WorkspaceID: strPtr("wB")}, RunCliDeps{
		Connect:    func(string, func(string, any)) (RpcClientLike, error) { return unnamed, nil },
		Output:     func(line string) { unnamedOutput = append(unnamedOutput, line) },
		SocketPath: "/tmp/s.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unnamedOutput[0], "name: unnamed\nagent: codex") {
		t.Fatalf("unnamed output = %s", unnamedOutput[0])
	}
}

func TestRunCliCommandRendersHelpAndRejectsTheDaemonCommand(t *testing.T) {
	output := []string{}
	if err := RunCliCommand(CliCommand{Command: CommandHelp}, RunCliDeps{
		Output: func(line string) { output = append(output, line) },
	}); err != nil {
		t.Fatal(err)
	}
	if len(output) != 1 || !strings.Contains(output[0], "Usage:") {
		t.Fatalf("output = %v", output)
	}

	err := RunCliCommand(CliCommand{Action: "status", Command: CommandDaemon}, RunCliDeps{
		Output: func(string) {},
	})
	if err == nil || err.Error() != "daemon command is handled by main" {
		t.Fatalf("error = %v", err)
	}
}

func TestRunCliCommandDispatchesOrchestratorCommands(t *testing.T) {
	registered := map[string]any{
		"presence": map[string]any{
			"herdrSessionName": "default",
			"paneId":           "wB:p1",
			"terminalId":       "term_1",
			"workspaceId":      "wB",
		},
		"state": nil,
	}

	t.Run("status returns the registration", func(t *testing.T) {
		client := &fakeClient{}
		client.respond = func(method string, params any) (any, error) { return registered, nil }
		output := []string{}
		err := RunCliCommand(CliCommand{
			Action:          "status",
			Command:         CommandOrchestrator,
			HerdrSocketPath: "/tmp/h.sock",
			PaneID:          "wB:p1",
			WorkspaceID:     strPtr("wB"),
		}, RunCliDeps{
			Connect:    func(string, func(string, any)) (RpcClientLike, error) { return client, nil },
			Output:     func(line string) { output = append(output, line) },
			SocketPath: "/tmp/s.sock",
		})
		if err != nil {
			t.Fatal(err)
		}
		if output[0] != "Herdsman is off (default/wB)" {
			t.Fatalf("output = %s", output[0])
		}
		wantRegistration := map[string]any{
			"herdrSocketPath": "/tmp/h.sock",
			"paneId":          "wB:p1",
			"sessionRef": map[string]any{
				"agent":  "agy",
				"kind":   "id",
				"source": "herdsman-cli",
				"value":  "herdsman-cli",
			},
			"subscriberId":   "cli:wB:p1",
			"subscriberKind": "agy",
			"workspaceId":    "wB",
		}
		if !reflect.DeepEqual(client.calls[0].Params, wantRegistration) {
			t.Fatalf("register params = %+v, want %+v", client.calls[0].Params, wantRegistration)
		}
	})

	t.Run("on claims an unowned workspace but keeps a foreign owner", func(t *testing.T) {
		owned := map[string]any{
			"presence": map[string]any{
				"herdrSessionName": "default",
				"paneId":           "wB:p2",
				"terminalId":       "term_2",
				"workspaceId":      "wB",
			},
			"state": map[string]any{
				"ackedEventId":     0,
				"herdrSessionName": "default",
				"owner":            map[string]any{"paneId": "wB:p1", "terminalId": "term_1"},
				"updatedAt":        "2026-07-22T00:00:00.000Z",
				"workspaceId":      "wB",
			},
		}
		client := &fakeClient{}
		client.respond = func(method string, params any) (any, error) { return owned, nil }
		output := []string{}
		err := RunCliCommand(CliCommand{
			Action:          "on",
			Command:         CommandOrchestrator,
			HerdrSocketPath: "/tmp/h.sock",
			IfUnowned:       true,
			JSON:            true,
			PaneID:          "wB:p2",
			WorkspaceID:     strPtr("wB"),
		}, RunCliDeps{
			Connect:    func(string, func(string, any)) (RpcClientLike, error) { return client, nil },
			Output:     func(line string) { output = append(output, line) },
			SocketPath: "/tmp/s.sock",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(client.calls) != 2 { // register + close
			t.Fatalf("calls = %+v, want no set call", client.calls)
		}
		if !strings.Contains(output[0], `"changed":false`) {
			t.Fatalf("output = %s", output[0])
		}
	})

	t.Run("off disables the orchestrator", func(t *testing.T) {
		client := &fakeClient{}
		client.respond = func(method string, params any) (any, error) {
			if method == "agent.orchestrator.set" {
				return map[string]any{"changed": true}, nil
			}
			return registered, nil
		}
		output := []string{}
		err := RunCliCommand(CliCommand{
			Action:          "off",
			Command:         CommandOrchestrator,
			HerdrSocketPath: "/tmp/h.sock",
			JSON:            true,
			PaneID:          "wB:p1",
			WorkspaceID:     strPtr("wB"),
		}, RunCliDeps{
			Connect:    func(string, func(string, any)) (RpcClientLike, error) { return client, nil },
			Output:     func(line string) { output = append(output, line) },
			SocketPath: "/tmp/s.sock",
		})
		if err != nil {
			t.Fatal(err)
		}
		if client.calls[1].Method != "agent.orchestrator.set" ||
			!reflect.DeepEqual(client.calls[1].Params, map[string]any{"enabled": false}) {
			t.Fatalf("calls = %+v", client.calls)
		}
		if output[0] != `{"changed":true}` {
			t.Fatalf("output = %s", output[0])
		}
	})
}

func TestRunCliCommandAgyHookWritesTheJSONContract(t *testing.T) {
	// A missing Herdr launch identity is the documented "not inside Herdr" answer:
	// an empty object, and no connect at all.
	output := []string{}
	err := RunCliCommand(CliCommand{Command: CommandAgyHook, HookEventName: agy.HookPreInvocation}, RunCliDeps{
		Connect: func(string, func(string, any)) (RpcClientLike, error) {
			t.Fatal("connect must not be called without a Herdr launch identity")
			return nil, nil
		},
		Environment: map[string]string{},
		Output:      func(line string) { output = append(output, line) },
		SocketPath:  "/tmp/s.sock",
		Stdin:       strPtr("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 1 || output[0] != "{}" {
		t.Fatalf("output = %v", output)
	}
}

func TestResolveRuntimeForCommandUsesTheRuntimeRecordBeforeParsingConfig(t *testing.T) {
	homeDir := t.TempDir()
	writeTestFile(t, filepath.Join(homeDir, "config.yaml"), "runtime: [")
	record := map[string]any{
		"dbPath":     filepath.Join(homeDir, "record-state.db"),
		"homeDir":    homeDir,
		"logPath":    filepath.Join(homeDir, "record.log"),
		"pid":        1234,
		"pidPath":    filepath.Join(homeDir, "record.pid"),
		"socketPath": filepath.Join(homeDir, "record.sock"),
		"startedAt":  "2026-06-29T00:00:00.000Z",
		"version":    1,
	}
	writeTestJSON(t, filepath.Join(homeDir, "runtime.json"), record)

	environment := map[string]string{"HERDSMAN_HOME": homeDir}
	runtime, err := ResolveRuntimeForCommand(environment)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runtime.HomeDir != homeDir {
		t.Fatalf("HomeDir = %q, want %q", runtime.HomeDir, homeDir)
	}
	if runtime.Paths.DBPath != record["dbPath"] || runtime.Paths.PIDPath != record["pidPath"] ||
		runtime.Paths.SocketPath != record["socketPath"] {
		t.Fatalf("paths = %+v, want the runtime record paths", runtime.Paths)
	}

	if err := os.Remove(filepath.Join(homeDir, "runtime.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveRuntimeForCommand(environment); err == nil ||
		!strings.HasPrefix(err.Error(), "Invalid Herdsman config") {
		t.Fatalf("error = %v, want an Invalid Herdsman config failure", err)
	}
}

func TestOneLineCollapsesWhitespaceAndCutsAt160Units(t *testing.T) {
	if got := oneLine("a \t\n b"); got != "a b" {
		t.Fatalf("oneLine = %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := oneLine(long); len(got) != 160 {
		t.Fatalf("oneLine length = %d, want 160", len(got))
	}
	// A cut inside a surrogate pair drops the whole rune instead of emitting half
	// of it (JS would keep a lone surrogate, which Go cannot represent).
	if got := oneLine(strings.Repeat("x", 159) + "\U0001F600"); utf16Length(got) != 159 {
		t.Fatalf("oneLine = %q (units %d), want the emoji dropped", got, utf16Length(got))
	}
	if got := oneLine(strings.Repeat("x", 158) + "\U0001F600"); utf16Length(got) != 160 {
		t.Fatalf("oneLine units = %d, want 160", utf16Length(got))
	}
}

// utf16Length counts the JS string units of a Go string.
func utf16Length(value string) int {
	units := 0
	for _, current := range value {
		if current > 0xffff {
			units += 2
			continue
		}
		units++
	}
	return units
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := marshalJS(value)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, encoded)
}

func strPtr(value string) *string { return &value }

func intPtr(value int) *int { return &value }

// ---------------------------------------------------------------------------
// The shell scripts: the CLI switch (go/scripts/herdsman) and the isolated
// trial daemon (go/scripts/herdsman-go-trial).
// ---------------------------------------------------------------------------

// scriptEnv builds the environment for a script run: every HERDSMAN_/HERDR_
// variable of the current process is dropped (so the test is independent of
// the pane it runs in) and the given overrides are applied.
func scriptEnv(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "HERDSMAN_") || strings.HasPrefix(key, "HERDR_") {
			continue
		}
		environment = append(environment, entry)
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

// runScript runs a script and returns its stdout, its stderr and its exit
// code. A non-zero exit is a result, not a failure.
func runScript(t *testing.T, path string, args []string, overrides map[string]string) (string, string, int) {
	t.Helper()
	command := exec.Command(path, args...)
	command.Env = scriptEnv(overrides)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	code := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run %s: %v", path, err)
		}
		code = exitError.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// writeStubScript writes an executable stub that prints one word.
func writeStubScript(t *testing.T, directory, name, word string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	writeTestFile(t, path, "#!/bin/sh\necho "+word+"\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestWrapperAndTrialScripts covers the two scripts of the 第 6 批 preparation:
// the CLI switch (go/scripts/herdsman) routes HERDSMAN_IMPL=ts|go to the
// right entrypoint and rejects any other value, and the isolated trial daemon
// (go/scripts/herdsman-go-trial) refuses to run against the production home.
func TestWrapperAndTrialScripts(t *testing.T) {
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	scriptDir := filepath.Join(workingDir, "..", "..", "scripts")
	wrapper := filepath.Join(scriptDir, "herdsman")
	trial := filepath.Join(scriptDir, "herdsman-go-trial")
	for _, path := range []string{wrapper, trial} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat %s: %v", path, statErr)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable (mode %v)", path, info.Mode().Perm())
		}
	}

	tempDir := t.TempDir()
	tsStub := writeStubScript(t, tempDir, "ts-stub.sh", "ts")
	goStub := writeStubScript(t, tempDir, "go-stub.sh", "go")

	t.Run("the ts branch runs the TypeScript entrypoint", func(t *testing.T) {
		stdout, stderr, code := runScript(t, wrapper, []string{"daemon", "status"}, map[string]string{
			"HERDSMAN_IMPL":     "ts",
			"HERDSMAN_NODE":     "/bin/sh",
			"HERDSMAN_TS_ENTRY": tsStub,
		})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
		}
		if strings.TrimSpace(stdout) != "ts" {
			t.Fatalf("stdout = %q, want %q", stdout, "ts")
		}
	})

	t.Run("the default branch is ts", func(t *testing.T) {
		stdout, stderr, code := runScript(t, wrapper, []string{"daemon", "status"}, map[string]string{
			"HERDSMAN_NODE":     "/bin/sh",
			"HERDSMAN_TS_ENTRY": tsStub,
		})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
		}
		if strings.TrimSpace(stdout) != "ts" {
			t.Fatalf("stdout = %q, want %q", stdout, "ts")
		}
	})

	t.Run("the go branch runs the Go binary", func(t *testing.T) {
		stdout, stderr, code := runScript(t, wrapper, []string{"daemon", "status"}, map[string]string{
			"HERDSMAN_IMPL":   "go",
			"HERDSMAN_GO_BIN": goStub,
		})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
		}
		if strings.TrimSpace(stdout) != "go" {
			t.Fatalf("stdout = %q, want %q", stdout, "go")
		}
	})

	t.Run("the go branch falls back to the installed binary", func(t *testing.T) {
		stdout, stderr, code := runScript(t, wrapper, nil, map[string]string{"HERDSMAN_IMPL": "go"})
		installed := "/root/.local/lib/herdsman-go/herdsman"
		_, installedErr := os.Stat(installed)
		if installedErr == nil {
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
			}
			if strings.Contains(stderr, "Go binary not found") {
				t.Fatalf("stderr = %q, want the installed Go binary to be used", stderr)
			}
			return
		}
		if code == 0 {
			t.Fatalf("exit code = 0, want a failure when no Go binary exists (stdout: %s)", stdout)
		}
		if !strings.Contains(stderr, "Go binary not found") {
			t.Fatalf("stderr = %q, want the missing-binary message", stderr)
		}
	})

	t.Run("an unsupported implementation is rejected", func(t *testing.T) {
		stdout, stderr, code := runScript(t, wrapper, []string{"daemon", "status"}, map[string]string{
			"HERDSMAN_IMPL": "rust",
		})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stdout: %s, stderr: %s)", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "HERDSMAN_IMPL must be 'go' or 'ts'") {
			t.Fatalf("stderr = %q, want the rejection message", stderr)
		}
	})

	// The trial script must refuse the production home before it touches
	// anything: the production database is checked before and after.
	productionDB := "/root/.herdsman/state.db"
	before := fileIdentity(productionDB)

	for _, testCase := range []struct {
		name    string
		home    string
		message string
	}{
		{name: "the production home is refused", home: "/root/.herdsman", message: "refusing to run"},
		{name: "the user home is refused", home: "/root/.herdsman/", message: "refusing to run"},
		{name: "an empty home is refused", home: "", message: "is empty"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stdout, stderr, code := runScript(t, trial, []string{"init"}, map[string]string{
				"HERDSMAN_TRIAL_HOME": testCase.home,
			})
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stdout: %s, stderr: %s)", code, stdout, stderr)
			}
			if !strings.Contains(stderr, testCase.message) {
				t.Fatalf("stderr = %q, want %q", stderr, testCase.message)
			}
		})
	}

	after := fileIdentity(productionDB)
	if before != after {
		t.Fatalf("the production database changed during the refused runs (before %q, after %q)", before, after)
	}
}

// fileIdentity is the size and the modification time of a file, used to prove
// that a refused run did not write to it. A missing file has no identity.
func fileIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%s", info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
}
