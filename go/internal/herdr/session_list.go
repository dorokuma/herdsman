package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// This file ports src/herdr/session-list.ts.

// HerdrSessionListTimeoutMs mirrors HERDR_SESSION_LIST_TIMEOUT_MS. `herdr session
// list` is a local CLI call; the cap keeps a wedged `herdr` process from parking
// the caller (and its shutdown path) indefinitely. It is only a hard cap
// together with the SIGKILL kill signal below: execFile's default killSignal is
// SIGTERM, which the child may ignore, and a late success would then reach the
// callback without an error and still count as a run.
const HerdrSessionListTimeoutMs = 2_000

// HerdrSessionListEntry mirrors the TS type of the same name. Default is
// optional (kept only when the source emits a boolean), so it is a pointer.
type HerdrSessionListEntry struct {
	Default    *bool
	Name       string
	Running    bool
	SessionDir string
	SocketPath string
}

// HerdrSessionListRunner mirrors HerdrSessionListRunner: a zero-arg callable
// that shells out to `herdr session list --json` and normalizes its stdout.
type HerdrSessionListRunner func() ([]HerdrSessionListEntry, error)

// HerdrSessionListRunnerOptions mirrors the TS options object. HasTimeoutMs
// distinguishes an explicit timeoutMs (including 0, which means "no timeout")
// from an omitted one, matching `options.timeoutMs ?? HERDR_SESSION_LIST_TIMEOUT_MS`
// where 0 is a valid value rather than the default.
type HerdrSessionListRunnerOptions struct {
	Command      string
	Env          []string
	TimeoutMs    int
	HasTimeoutMs bool
}

// CreateHerdrSessionListRunner mirrors createHerdrSessionListRunner(options).
//
// The returned runner execs `<command> session list --json` with the configured
// environment (inheriting the current process's environment when Env is nil,
// matching `options.env ?? process.env`), caps it at the configured timeout with
// a SIGKILL kill signal, and normalizes the captured stdout.
func CreateHerdrSessionListRunner(options HerdrSessionListRunnerOptions) HerdrSessionListRunner {
	command := options.Command
	if command == "" {
		command = "herdr"
	}
	timeout := HerdrSessionListTimeoutMs
	if options.HasTimeoutMs {
		timeout = options.TimeoutMs
	}
	return func() ([]HerdrSessionListEntry, error) {
		ctx := context.Background()
		// A non-positive timeout means "no timeout" (execFile's timeout: 0),
		// so only arm the deadline when it is a real cap.
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
			defer cancel()
		}
		cmd := exec.CommandContext(ctx, command, "session", "list", "--json")
		// SIGKILL on timeout, matching killSignal: "SIGKILL".
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return os.ErrProcessDone
			}
			return cmd.Process.Kill()
		}
		if options.Env != nil {
			cmd.Env = options.Env
		}
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		return NormalizeHerdrSessionList(string(out))
	}
}

// NormalizeHerdrSessionList mirrors normalizeHerdrSessionList(value).
//
// A string input is JSON-parsed first (a parse failure throws the same
// "Failed to parse herdr session list --json output: ..." error); any other
// value is used as-is. The result is the `sessions` array when present, else [].
// Each session must carry non-empty string name/session_dir/socket_path or the
// whole call throws "Invalid herdr session list entry". `herdr session list
// --json` emits snake_case (session_dir, socket_path) and that is the only shape
// consumed here.
func NormalizeHerdrSessionList(value any) ([]HerdrSessionListEntry, error) {
	parsed := value
	if raw, ok := value.(string); ok {
		var decoded any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			return nil, fmt.Errorf("Failed to parse herdr session list --json output: %w", err)
		}
		parsed = decoded
	}

	record, ok := asJSONObject(parsed)
	if !ok {
		record = map[string]any{}
	}
	sessions, _ := record["sessions"].([]any)

	entries := make([]HerdrSessionListEntry, 0, len(sessions))
	for _, session := range sessions {
		item, itemOK := asJSONObject(session)
		if !itemOK {
			item = map[string]any{}
		}
		name := stringValue(item["name"])
		sessionDir := stringValue(item["session_dir"])
		socketPath := stringValue(item["socket_path"])
		if name == nil || sessionDir == nil || socketPath == nil {
			return nil, errors.New("Invalid herdr session list entry")
		}
		entry := HerdrSessionListEntry{
			Name:       *name,
			Running:    item["running"] == true,
			SessionDir: *sessionDir,
			SocketPath: *socketPath,
		}
		if isDefault, ok := item["default"].(bool); ok {
			entry.Default = &isDefault
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// stringValue returns the string when value is a non-empty string, else nil,
// mirroring `typeof value === "string" && value.length > 0 ? value : null`.
func stringValue(value any) *string {
	if s, ok := value.(string); ok && len(s) > 0 {
		return &s
	}
	return nil
}
