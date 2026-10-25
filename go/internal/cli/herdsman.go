// Package cli ports src/cli/herdsman.ts: argument parsing, the RPC command
// dispatch, the human/JSON renderers and the `daemon status` view.
//
// The output text is the user-visible contract of the TS CLI, so the help
// text, the tab-separated tables, the JSON decision errors and the exit codes
// are ported verbatim. JSON payloads are rendered through marshalJS, because
// `JSON.stringify` does not escape `<`, `>` or `&` the way encoding/json does
// by default.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/dorokuma/herdsman/go/internal/agy"
	"github.com/dorokuma/herdsman/go/internal/config"
	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The two scope errors of the TS CLI, verbatim.
const (
	currentHerdrWorkspaceError = "agent command requires HERDR_ENV=1 with HERDR_WORKSPACE_ID, --workspace <id>, --session <name>, or --all."
	currentHerdrPaneError      = "orchestrator command requires HERDR_ENV=1 with HERDR_SOCKET_PATH, HERDR_PANE_ID, and HERDR_WORKSPACE_ID."
)

// CommandKind is the `command` discriminator of the TS CliCommand union.
type CommandKind string

const (
	CommandDaemon       CommandKind = "daemon"
	CommandAgentList    CommandKind = "agent-list"
	CommandAgentGet     CommandKind = "agent-get"
	CommandAgentRead    CommandKind = "agent-read"
	CommandOrchestrator CommandKind = "orchestrator"
	CommandAgyHook      CommandKind = "agy-hook"
	CommandHelp         CommandKind = "help"
)

// CliCommand mirrors the TS CliCommand union in one struct: every field is only
// meaningful for the commands that carry it, exactly like the union members.
type CliCommand struct {
	Action           string
	All              bool
	Command          CommandKind
	HerdrSessionName *string
	HerdrSocketPath  string
	HookEventName    agy.HookEventName
	IfUnowned        bool
	JSON             bool
	Limit            *int
	PaneID           string
	Target           string
	Text             bool
	WorkspaceID      *string
}

// ParseCliArgs mirrors parseCliArgs. The returned error carries the TS message
// verbatim (the CLI prints it on stderr and exits 1).
func ParseCliArgs(args []string, environment map[string]string) (CliCommand, error) {
	rest := &argList{args: append([]string{}, args...)}
	command, _ := rest.shift()
	if command == "" || command == "--help" || command == "-h" || command == "help" {
		return CliCommand{Command: CommandHelp}, nil
	}

	switch command {
	case "daemon":
		action := "status"
		if value, ok := rest.shift(); ok {
			action = value
		}
		if action != "status" {
			return CliCommand{}, fmt.Errorf("Unknown daemon action: %s", action)
		}
		text := rest.takeFlag("--text")
		if err := rest.rejectExtra(); err != nil {
			return CliCommand{}, err
		}
		return CliCommand{Action: action, Command: CommandDaemon, Text: text}, nil

	case "agent":
		return parseAgentCommand(rest, environment)

	case "on", "off", "status":
		return parseOrchestratorCommand(command, rest, environment)

	case "agy-hook":
		hookEventName, _ := rest.shift()
		if hookEventName != string(agy.HookPreInvocation) &&
			hookEventName != string(agy.HookStop) &&
			hookEventName != string(agy.HookWakeWaiter) {
			return CliCommand{}, errors.New("agy-hook requires PreInvocation, Stop, or WakeWaiter")
		}
		if err := rest.rejectExtra(); err != nil {
			return CliCommand{}, err
		}
		return CliCommand{
			Command:       CommandAgyHook,
			HookEventName: agy.HookEventName(hookEventName),
		}, nil
	}

	return CliCommand{}, fmt.Errorf("Unknown command: %s", command)
}

// parseOrchestratorCommand mirrors parseOrchestratorCommand.
func parseOrchestratorCommand(
	action string,
	args *argList,
	environment map[string]string,
) (CliCommand, error) {
	rest := &argList{args: append([]string{}, args.args...)}
	jsonOutput := rest.takeFlag("--json")
	ifUnowned := false
	if action == "on" {
		ifUnowned = rest.takeFlag("--if-unowned")
	}
	if err := rest.rejectExtra(); err != nil {
		return CliCommand{}, err
	}
	if environment["HERDR_ENV"] != "1" ||
		environment["HERDR_SOCKET_PATH"] == "" ||
		environment["HERDR_PANE_ID"] == "" ||
		environment["HERDR_WORKSPACE_ID"] == "" {
		return CliCommand{}, errors.New(currentHerdrPaneError)
	}
	return CliCommand{
		Action:          action,
		Command:         CommandOrchestrator,
		HerdrSocketPath: environment["HERDR_SOCKET_PATH"],
		IfUnowned:       ifUnowned,
		JSON:            jsonOutput,
		PaneID:          environment["HERDR_PANE_ID"],
		WorkspaceID:     optionalString(environment["HERDR_WORKSPACE_ID"]),
	}, nil
}

// parseAgentCommand mirrors parseAgentCommand.
func parseAgentCommand(args *argList, environment map[string]string) (CliCommand, error) {
	rest := &argList{args: append([]string{}, args.args...)}
	subcommand, _ := rest.shift()
	if subcommand == "" || subcommand == "help" || subcommand == "--help" || subcommand == "-h" {
		return CliCommand{Command: CommandHelp}, nil
	}
	jsonOutput := rest.takeFlag("--json")
	herdrSessionName, err := rest.takeOption("--session")
	if err != nil {
		return CliCommand{}, err
	}
	workspaceID, err := rest.takeOption("--workspace")
	if err != nil {
		return CliCommand{}, err
	}
	command := CliCommand{
		Command:          CommandAgentList,
		HerdrSessionName: optionalString(herdrSessionName),
		JSON:             jsonOutput,
		WorkspaceID:      optionalString(workspaceID),
	}

	switch subcommand {
	case "list":
		all := rest.takeFlag("--all")
		if err := rest.rejectExtra(); err != nil {
			return CliCommand{}, err
		}
		if all {
			// `all ? { all: true } : scopedOrCurrent(...)`: the TS list branch
			// replaces the whole scope object, so an explicit --session /
			// --workspace is dropped instead of being sent alongside `all`.
			command.All = true
			command.HerdrSessionName = nil
			command.WorkspaceID = nil
		} else if err := applyScopeOrCurrent(&command, environment); err != nil {
			return CliCommand{}, err
		}
		return command, nil

	case "get":
		target, hasTarget := rest.shift()
		if !hasTarget || target == "" {
			return CliCommand{}, errors.New("agent get requires <target>")
		}
		if err := rest.rejectExtra(); err != nil {
			return CliCommand{}, err
		}
		if err := applyScopeOrCurrent(&command, environment); err != nil {
			return CliCommand{}, err
		}
		command.Command = CommandAgentGet
		command.Target = target
		return command, nil

	case "read":
		limitValue, hasLimit, err := rest.takeOptionValue("--limit")
		if err != nil {
			return CliCommand{}, err
		}
		target, hasTarget := rest.shift()
		if !hasTarget || target == "" {
			return CliCommand{}, errors.New("agent read requires <target>")
		}
		if err := rest.rejectExtra(); err != nil {
			return CliCommand{}, err
		}
		// The limit is validated *before* the scope is resolved, exactly like the
		// TS order: `agent read <target> --limit 0` fails on the limit even outside
		// a Herdr workspace.
		if hasLimit {
			limit := parseJSNumber(limitValue)
			if !jsIsInteger(limit) || limit < 1 || limit > 500 {
				return CliCommand{}, errors.New("--limit must be between 1 and 500")
			}
			value := int(limit)
			command.Limit = &value
		}
		if err := applyScopeOrCurrent(&command, environment); err != nil {
			return CliCommand{}, err
		}
		command.Command = CommandAgentRead
		command.Target = target
		return command, nil
	}

	return CliCommand{}, fmt.Errorf("Unknown agent command: %s", subcommand)
}

// applyScopeOrCurrent mirrors scopedOrCurrent: an explicit scope (or `--all`)
// wins, otherwise the current Herdr workspace is used and its absence is an
// error.
func applyScopeOrCurrent(command *CliCommand, environment map[string]string) error {
	if optionalPresent(command.HerdrSessionName) || optionalPresent(command.WorkspaceID) || command.All {
		return nil
	}
	if environment["HERDR_ENV"] == "1" && environment["HERDR_WORKSPACE_ID"] != "" {
		command.WorkspaceID = optionalString(environment["HERDR_WORKSPACE_ID"])
		return nil
	}
	return errors.New(currentHerdrWorkspaceError)
}

// HelpText mirrors helpText().
func HelpText() string {
	return strings.Join([]string{
		"Usage:",
		"  herdsman on [--if-unowned] [--json]",
		"  herdsman off [--json]",
		"  herdsman status [--json]",
		"  herdsman daemon status [--text]",
		"  herdsman agent list [--all] [--workspace <id>] [--session <name>] [--json]",
		"  herdsman agent get <target> [--workspace <id>] [--session <name>] [--json]",
		"  herdsman agent read <target> [--limit N] [--workspace <id>] [--session <name>] [--json]",
		"  herdsman agy-hook <PreInvocation|Stop|WakeWaiter>",
		"  herdsman help",
		"",
		"Notes:",
		"  `daemon status` keeps printing JSON by default (the existing machine",
		"  contract); `--text` is the human view. Its two supervision facts are",
		"  read-only: `managedBy` only says whether the pid belongs to the",
		"  herdsman.service system unit — `unmanaged` does not mean unsupervised, and",
		"  it is not a licence to kill anything. `restartCount` is systemd's",
		"  NRestarts, which reset-failed, a stop+start pair or an unloaded unit all",
		"  reset to 0 — so 0 is not \"never crashed\".",
		"",
	}, "\n")
}

// RpcClientLike mirrors `Pick<ObservabilityRpcClient, "close" | "request">`.
type RpcClientLike interface {
	Close()
	Call(method string, params any) (any, error)
}

// RunCliDeps mirrors RunCliDeps. Environment is the `process.env` stand-in and
// Stdin the optional captured stdin (the TS `deps.stdin`).
type RunCliDeps struct {
	Connect     func(socketPath string, onNotification func(method string, params any)) (RpcClientLike, error)
	Environment map[string]string
	Output      func(line string)
	SocketPath  string
	StateDir    *string
	Stdin       *string
}

// RunCliCommand mirrors runCliCommand.
func RunCliCommand(command CliCommand, deps RunCliDeps) error {
	if command.Command == CommandHelp {
		deps.Output(HelpText())
		return nil
	}
	if command.Command == CommandDaemon {
		return errors.New("daemon command is handled by main")
	}
	if command.Command == CommandAgyHook {
		rawStdin := ""
		switch {
		case deps.Stdin != nil:
			rawStdin = *deps.Stdin
		case command.HookEventName == agy.HookWakeWaiter:
			rawStdin = ""
		default:
			rawStdin = readAllStdin()
		}
		output := agy.RunAgyHook(command.HookEventName, rawStdin, agy.RunAgyHookOptions{
			Connect: func(socketPath string, onNotification func(method string, params any)) (agy.RpcClient, error) {
				if deps.Connect == nil {
					return nil, errors.New("connect is required")
				}
				return deps.Connect(socketPath, onNotification)
			},
			Environment: deps.Environment,
			SocketPath:  deps.SocketPath,
			StateDir:    deps.StateDir,
		})
		encoded, err := output.JSON()
		if err != nil {
			return err
		}
		deps.Output(encoded)
		return nil
	}

	if deps.Connect == nil {
		return errors.New("connect is required")
	}
	client, err := deps.Connect(deps.SocketPath, nil)
	if err != nil {
		return err
	}
	defer client.Close()
	result, err := dispatchRPCCommand(command, client)
	if err != nil {
		return err
	}
	return printResult(command, result, deps.Output)
}

// dispatchRPCCommand mirrors dispatchRpcCommand.
func dispatchRPCCommand(command CliCommand, client RpcClientLike) (any, error) {
	if command.Command == CommandOrchestrator {
		registered, err := client.Call("agent.orchestrator.register", map[string]any{
			"herdrSocketPath": command.HerdrSocketPath,
			"paneId":          command.PaneID,
			"sessionRef": map[string]any{
				"agent":  "agy",
				"kind":   "id",
				"source": "herdsman-cli",
				"value":  "herdsman-cli",
			},
			"subscriberId":   "cli:" + command.PaneID,
			"subscriberKind": "agy",
			"workspaceId":    derefOptional(command.WorkspaceID),
		})
		if err != nil {
			return nil, err
		}
		switch command.Action {
		case "status":
			return registered, nil
		case "on":
			view := decodeOrchestratorConnection(registered)
			if command.IfUnowned && view.State != nil && view.State.Owner != nil &&
				view.State.Owner.TerminalID != view.Presence.TerminalID {
				return withChanged(registered, false), nil
			}
			return client.Call("agent.orchestrator.set", map[string]any{"enabled": true})
		default:
			return client.Call("agent.orchestrator.set", map[string]any{"enabled": false})
		}
	}
	if command.Command == CommandAgentList {
		return client.Call("agent.list", scopeParams(command))
	}
	if command.Command == CommandAgentGet {
		params := scopeParams(command)
		params["target"] = command.Target
		return client.Call("agent.get", params)
	}
	params := scopeParams(command)
	if command.Limit != nil {
		params["limit"] = *command.Limit
	}
	params["target"] = command.Target
	return client.Call("agent.read", params)
}

// scopeParams mirrors scopeParams.
func scopeParams(scope CliCommand) map[string]any {
	params := map[string]any{}
	if scope.All {
		params["all"] = true
	}
	if value, ok := nonEmptyOptional(scope.HerdrSessionName); ok {
		params["herdrSessionName"] = value
	}
	if value, ok := nonEmptyOptional(scope.WorkspaceID); ok {
		params["workspaceId"] = value
	}
	return params
}

// withChanged mirrors `{ ...registered, changed: false }`: `changed` is added to
// the raw result without dropping any of its keys.
func withChanged(result any, changed bool) any {
	object := map[string]any{}
	encoded, err := marshalJS(result)
	if err != nil {
		return result
	}
	if err := json.Unmarshal([]byte(encoded), &object); err != nil {
		return result
	}
	object["changed"] = changed
	return object
}

// printResult mirrors printResult.
func printResult(command CliCommand, result any, output func(line string)) error {
	if command.JSON {
		encoded, err := marshalJS(result)
		if err != nil {
			return err
		}
		output(encoded)
		return nil
	}
	formatted, err := formatHumanResult(command, result)
	if err != nil {
		return err
	}
	output(formatted)
	return nil
}

// formatHumanResult mirrors formatHumanResult.
func formatHumanResult(command CliCommand, result any) (string, error) {
	switch command.Command {
	case CommandOrchestrator:
		return formatOrchestratorStatus(decodeOrchestratorConnection(result)), nil
	case CommandAgentList:
		return formatAgentList(decodeJSON[agentListResult](result)), nil
	case CommandAgentGet:
		return formatAgentGet(decodeJSON[agentGetResult](result)), nil
	case CommandAgentRead:
		return formatAgentRead(decodeJSON[agentReadResult](result)), nil
	}
	return marshalJS(result)
}

// orchestratorConnectionView mirrors the presence/state projection the CLI
// reads off `agent.orchestrator.register` / `.get` / `.set`.
type orchestratorConnectionView struct {
	Presence agy.Presence                              `json:"presence"`
	State    *observability.AgentOrchestratorWireState `json:"state"`
}

// agentListResult mirrors `{ agents?: AgentListItem[] }`.
type agentListResult struct {
	Agents []observability.AgentListItem `json:"agents"`
}

// agentGetResult mirrors `{ agent?: AgentGetResult }`.
type agentGetResult struct {
	Agent *observability.AgentGetResult `json:"agent"`
}

// agentReadResult mirrors `{ agent?: AgentReadResult }`.
type agentReadResult struct {
	Agent *observability.AgentReadResult `json:"agent"`
}

// decodeOrchestratorConnection mirrors the TS `as` cast.
func decodeOrchestratorConnection(value any) orchestratorConnectionView {
	return decodeJSON[orchestratorConnectionView](value)
}

// decodeJSON mirrors the unchecked TS casts: a value that cannot be decoded
// reads as the zero value of its target type.
func decodeJSON[T any](value any) T {
	var target T
	encoded, err := json.Marshal(value)
	if err != nil {
		return target
	}
	if err := json.Unmarshal(encoded, &target); err != nil {
		return target
	}
	return target
}

// formatOrchestratorStatus mirrors formatOrchestratorStatus.
func formatOrchestratorStatus(result orchestratorConnectionView) string {
	scope := result.Presence.HerdrSessionName + "/" + result.Presence.WorkspaceID
	if result.State != nil && result.State.Owner != nil {
		owner := result.State.Owner
		if owner.TerminalID == result.Presence.TerminalID &&
			result.State.HerdrSessionName == result.Presence.HerdrSessionName &&
			result.State.WorkspaceID == result.Presence.WorkspaceID {
			return "Herdsman is watching agent updates · " + scope + " · " + owner.PaneID
		}
		return "Herdsman is off · owned by " + owner.PaneID + " (" + scope + ")"
	}
	return "Herdsman is off (" + scope + ")"
}

// formatAgentList mirrors formatAgentList.
func formatAgentList(result agentListResult) string {
	if len(result.Agents) == 0 {
		return "No Herdsman agents indexed."
	}
	lines := []string{strings.Join([]string{"status", "name", "agent", "pane", "last user", "last assistant", "updated"}, "\t")}
	for _, agent := range result.Agents {
		lines = append(lines, strings.Join([]string{
			string(agent.AgentStatus),
			derefOptional(agent.Name),
			derefOr(agent.Agent, "unknown"),
			agent.PaneID,
			oneLine(excerptText(agent.History.LastUserMessage)),
			oneLine(excerptText(agent.History.LastAssistantMessage)),
			derefOptional(agent.History.UpdatedAt),
		}, "\t"))
	}
	return strings.Join(lines, "\n")
}

// formatAgentGet mirrors formatAgentGet.
func formatAgentGet(result agentGetResult) string {
	agent := result.Agent
	if agent == nil {
		return "Agent not found."
	}
	agentSession := "none"
	if agent.AgentSession != nil {
		agentSession = agent.AgentSession.Source + ":" + agent.AgentSession.Value
	}
	lastTool := ""
	if agent.History.LastToolResult != nil {
		lastTool = agent.History.LastToolResult.ToolName + " " + oneLine(agent.History.LastToolResult.Text)
	}
	return strings.Join([]string{
		"name: " + derefOr(agent.Name, "unnamed"),
		"agent: " + derefOr(agent.Agent, "unknown"),
		"status: " + string(agent.AgentStatus),
		"pane: " + agent.PaneID,
		"terminal: " + derefOr(agent.TerminalID, "unknown"),
		"workspace: " + agent.WorkspaceID,
		"Herdr session: " + agent.HerdrSessionName,
		"cwd: " + derefOr(derefOrPointer(agent.Cwd, agent.ForegroundCwd), "unknown"),
		"agent_session: " + agentSession,
		"last user: " + oneLine(excerptText(agent.History.LastUserMessage)),
		"last assistant: " + oneLine(excerptText(agent.History.LastAssistantMessage)),
		"last tool: " + lastTool,
	}, "\n")
}

// formatAgentRead mirrors formatAgentRead.
func formatAgentRead(result agentReadResult) string {
	agent := result.Agent
	if agent == nil {
		return "Agent not found."
	}
	lines := []string{
		"name: " + derefOr(agent.Name, "unnamed"),
		"agent: " + derefOr(agent.Agent, "unknown"),
		"pane: " + agent.PaneID,
		"",
	}
	for _, message := range agent.Messages {
		text := oneLine(message.Text)
		if message.Compact != nil {
			text = "[" + string(message.Compact.Compaction.Mode) + "] " + text
		}
		lines = append(lines, strings.Join([]string{
			derefOptional(message.Timestamp),
			string(message.Role),
			derefOptional(message.ToolName),
			text,
		}, "\t"))
	}
	return strings.Join(lines, "\n")
}

// oneLine mirrors oneLine: whitespace runs collapse to a single space and the
// result is cut to 160 JS string units.
func oneLine(value string) string {
	return sliceUTF16(collapseWhitespace(value), 160)
}

// collapseWhitespace mirrors `value.replace(/\s+/g, " ")` with the JS `\s` set
// (which differs from unicode.IsSpace: U+0085 is not JS whitespace).
func collapseWhitespace(value string) string {
	builder := strings.Builder{}
	builder.Grow(len(value))
	inWhitespace := false
	for _, current := range value {
		if isJSWhitespace(current) {
			if !inWhitespace {
				builder.WriteByte(' ')
			}
			inWhitespace = true
			continue
		}
		inWhitespace = false
		builder.WriteRune(current)
	}
	return builder.String()
}

// isJSWhitespace is the JS `\s` character class.
func isJSWhitespace(current rune) bool {
	switch current {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		'\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return current >= '\u2000' && current <= '\u200a'
}

// sliceUTF16 mirrors `value.slice(0, limit)` on JS strings: the cut is counted
// in UTF-16 code units, so a non-BMP rune counts twice. A cut that would split
// a surrogate pair drops the whole rune instead of emitting half of it.
func sliceUTF16(value string, limit int) string {
	units := 0
	for index, current := range value {
		size := 1
		if current > 0xffff {
			size = 2
		}
		if units+size > limit {
			return value[:index]
		}
		units += size
	}
	return value
}

// excerptText mirrors `excerpt?.text ?? ""`.
func excerptText(excerpt *observability.AgentHistoryExcerpt) string {
	if excerpt == nil {
		return ""
	}
	return excerpt.Text
}

// formatDaemonStatus mirrors formatDaemonStatus. The view adds the pid-file
// existence fact: the TS union encodes it in the object shape, and the text
// view needs it to decide whether a `stalePid` line is printed.
func FormatDaemonStatus(view DaemonStatusView) string {
	status := view.Status
	lines := []string{"state: " + status.State}
	if status.Pid != nil {
		lines = append(lines, "pid: "+strconv.Itoa(*status.Pid))
	}
	if view.branch() == branchPidFileMissing {
		lines = append(lines, "pidFileMissing: true")
	}
	if view.branch() == branchStale {
		lines = append(lines, "stalePid: "+stalePidText(status))
	}
	if view.hasSocketReachableKey() {
		lines = append(lines, "socketReachable: "+strconv.FormatBool(status.SocketReachable))
	}
	if status.ManagedBy != "" {
		lines = append(lines, "managedBy: "+string(status.ManagedBy))
		if daemon.IsSystemdHerdsmanManaged(status.ManagedBy) {
			restartCount := "unavailable"
			if status.RestartCount != nil {
				restartCount = strconv.Itoa(*status.RestartCount)
			}
			lines = append(lines, "restartCount: "+restartCount)
		}
	}
	return strings.Join(lines, "\n")
}

// stalePidText mirrors the JS template `${status.stalePid}` for the stale
// branch: a NaN pid (no integer to report) prints as `NaN`, exactly like JS.
func stalePidText(status daemon.DaemonStatus) string {
	if status.StalePid == nil {
		return "NaN"
	}
	return strconv.Itoa(*status.StalePid)
}

// DaemonStatusView pairs the daemon status with the pid-file existence fact.
type DaemonStatusView struct {
	PidFileExisted bool
	Status         daemon.DaemonStatus
}

// daemonStatusBranch mirrors the discriminant of the TS DaemonStatus union.
type daemonStatusBranch int

const (
	branchPidFileMissing daemonStatusBranch = iota
	branchStoppedNoPidFile
	branchStale
	branchRunningWithPid
)

func (v DaemonStatusView) branch() daemonStatusBranch {
	switch {
	case v.Status.PidFileMissing:
		return branchPidFileMissing
	case v.Status.Pid != nil:
		return branchRunningWithPid
	case v.PidFileExisted:
		return branchStale
	default:
		return branchStoppedNoPidFile
	}
}

// hasSocketReachableKey mirrors the union members that carry `socketReachable`:
// the two running variants (including the stale one, which only reports the
// socket when it answered the probe) but not the stopped ones.
func (v DaemonStatusView) hasSocketReachableKey() bool {
	switch v.branch() {
	case branchPidFileMissing, branchRunningWithPid:
		return true
	case branchStale:
		// The stale branch is only `running` when the socket answered; the stopped
		// variant drops the key entirely.
		return v.Status.SocketReachable
	default:
		return false
	}
}

// ReadDaemonStatus reads the status and the pid-file fact it is derived from.
func ReadDaemonStatus(pidPath, socketPath string) (DaemonStatusView, error) {
	status, err := daemon.GetDaemonStatus(pidPath, socketPath, nil)
	if err != nil {
		return DaemonStatusView{}, err
	}
	_, statErr := os.Stat(pidPath)
	return DaemonStatusView{Status: status, PidFileExisted: statErr == nil}, nil
}

// DaemonStatusJSON mirrors `JSON.stringify(status)` for the default (machine)
// output of `daemon status`. The key order of each union member is reproduced
// by hand: JSON key order is part of the contract, and the branch shapes differ
// per member.
func DaemonStatusJSON(view DaemonStatusView) (string, error) {
	status := view.Status
	object := &orderedJSON{}
	switch view.branch() {
	case branchPidFileMissing:
		object.add("pidPath", quoteJSONString(status.PidPath))
		object.add("pidFileMissing", "true")
		object.add("socketPath", quoteJSONString(status.SocketPath))
		object.add("socketReachable", "true")
		object.add("state", quoteJSONString(status.State))
	case branchStoppedNoPidFile:
		object.add("pidPath", quoteJSONString(status.PidPath))
		object.add("socketPath", quoteJSONString(status.SocketPath))
		object.add("state", quoteJSONString(status.State))
	case branchStale:
		object.add("pidPath", quoteJSONString(status.PidPath))
		object.add("socketPath", quoteJSONString(status.SocketPath))
		if status.SocketReachable {
			object.add("socketReachable", "true")
		}
		if status.StalePid == nil {
			object.add("stalePid", "null")
		} else {
			object.add("stalePid", strconv.Itoa(*status.StalePid))
		}
		object.add("state", quoteJSONString(status.State))
	case branchRunningWithPid:
		object.add("managedBy", quoteJSONString(string(status.ManagedBy)))
		if status.RestartCount != nil {
			object.add("restartCount", strconv.Itoa(*status.RestartCount))
		}
		object.add("pid", strconv.Itoa(*status.Pid))
		object.add("pidPath", quoteJSONString(status.PidPath))
		object.add("socketPath", quoteJSONString(status.SocketPath))
		object.add("socketReachable", strconv.FormatBool(status.SocketReachable))
		object.add("state", quoteJSONString(status.State))
	}
	return object.build(), nil
}

// orderedJSON renders an object with an explicit key order.
type orderedJSON struct {
	pairs []string
}

func (o *orderedJSON) add(key, rawValue string) {
	o.pairs = append(o.pairs, quoteJSONString(key)+":"+rawValue)
}

func (o *orderedJSON) build() string {
	return "{" + strings.Join(o.pairs, ",") + "}"
}

// ResolveRuntimeForCommand mirrors resolveRuntimeForCommand: the last daemon's
// runtime record wins, and only a missing record makes the CLI parse
// config.yaml (which an invalid config then fails).
func ResolveRuntimeForCommand(environment map[string]string) (config.RuntimeResolution, error) {
	paths, err := config.RuntimePathsFromRecordOrDefault(&config.RuntimePathsFromRecordOrDefaultOptions{
		Environment:  environment,
		RecordReader: daemonRuntimeRecordReader,
	})
	if err != nil {
		return config.RuntimeResolution{}, err
	}
	record, ok := daemon.ReadDaemonRuntimeRecord(paths.RuntimeRecordPath)
	if ok {
		return config.RuntimeResolution{
			Environment: environment,
			HomeDir:     record.HomeDir,
			Paths:       paths,
		}, nil
	}
	return config.ResolveRuntime(&config.RuntimeResolveOptions{Environment: environment})
}

// daemonRuntimeRecordReader adapts daemon.ReadDaemonRuntimeRecord to the config
// package's reader seam.
func daemonRuntimeRecordReader(path string) (config.RuntimeRecordPaths, bool) {
	record, ok := daemon.ReadDaemonRuntimeRecord(path)
	if !ok {
		return config.RuntimeRecordPaths{}, false
	}
	return config.RuntimeRecordPaths{
		DBPath:     record.DBPath,
		LogPath:    record.LogPath,
		PIDPath:    record.PidPath,
		SocketPath: record.SocketPath,
	}, true
}

// Environment mirrors `process.env`.
func Environment() map[string]string {
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		environment[key] = value
	}
	return environment
}

// FormatCliError mirrors formatCliError: a connection failure gets the
// systemd hint appended, everything else is printed as-is.
//
// The TS check keys on the Node error strings ("ENOENT" / "ECONNREFUSED"),
// which a Go `net.Dial("unix", ...)` failure never emits: the OS surfaces it
// as `errors.Is(err, os.ErrNotExist)` / `syscall.ENOENT` (a missing socket)
// or `syscall.ECONNREFUSED`, with the message text "no such file or directory"
// / "connection refused". All of those spellings are recognised so a real Unix
// socket failure prints the same systemd hint the TS CLI does.
func FormatCliError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if strings.Contains(message, "ENOENT") ||
		strings.Contains(message, "ECONNREFUSED") ||
		strings.Contains(message, "no such file or directory") ||
		strings.Contains(message, "connection refused") ||
		strings.Contains(message, "Herdsman daemon socket closed") ||
		strings.Contains(message, "Observability RPC socket closed") ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNREFUSED) {
		return message + "\n" +
			"Herdsman's daemon has no CLI start/stop commands; check the systemd-managed service with `systemctl status herdsman.service`."
	}
	return message
}

// readAllStdin mirrors readAllStdin: an interactive stdin reads as "empty".
func readAllStdin() string {
	info, err := os.Stdin.Stat()
	if err == nil && info.Mode()&os.ModeCharDevice != 0 {
		return ""
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return ""
	}
	return string(data)
}

// marshalJS mirrors JSON.stringify's escaping: `<`, `>` and `&` are literal,
// which encoding/json's default HTML escaping would rewrite.
func marshalJS(value any) (string, error) {
	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return restoreJSLineSeparators(strings.TrimSuffix(buffer.String(), "\n")), nil
}

// escapedLineSeparator and escapedParagraphSeparator are the six-character
// spellings encoding/json always writes for U+2028 and U+2029.
const (
	escapedLineSeparator      = `\u2028`
	escapedParagraphSeparator = `\u2029`
)

// restoreJSLineSeparators turns encoding/json's `\u2028` / `\u2029` escapes back
// into the literal UTF-8 characters, because JS's JSON.stringify leaves both
// characters as they are (they are legal in JSON strings, only illegal in JS
// source). SetEscapeHTML(false) does not cover them, so without this a CLI JSON
// line carrying either character differs from the TS output byte for byte.
//
// Only a real escape is rewritten. A run of backslashes ends with the candidate
// escape, so an odd run length means that final backslash was not itself escaped
// and the sequence is a real escape; an even run means the backslash was escaped
// and the text behind it is literal.
func restoreJSLineSeparators(encoded string) string {
	if !strings.Contains(encoded, `\u202`) {
		return encoded
	}
	var builder strings.Builder
	builder.Grow(len(encoded))
	for index := 0; index < len(encoded); {
		if encoded[index] != '\\' {
			builder.WriteByte(encoded[index])
			index++
			continue
		}
		start := index
		for index < len(encoded) && encoded[index] == '\\' {
			index++
		}
		backslashes := index - start
		if backslashes%2 == 1 {
			if strings.HasPrefix(encoded[index-1:], escapedLineSeparator) {
				builder.WriteString(encoded[start : index-1])
				builder.WriteString("\u2028")
				index += len(escapedLineSeparator) - 1
				continue
			}
			if strings.HasPrefix(encoded[index-1:], escapedParagraphSeparator) {
				builder.WriteString(encoded[start : index-1])
				builder.WriteString("\u2029")
				index += len(escapedParagraphSeparator) - 1
				continue
			}
		}
		builder.WriteString(encoded[start:index])
	}
	return builder.String()
}

// quoteJSONString renders a JSON string like JSON.stringify does.
func quoteJSONString(value string) string {
	encoded, err := marshalJS(value)
	if err != nil {
		return `""`
	}
	return encoded
}

// argList is the mutable argument vector the TS helpers splice.
type argList struct {
	args []string
}

// shift pops the first argument, mirroring `const [head, ...rest] = args`.
func (a *argList) shift() (string, bool) {
	if len(a.args) == 0 {
		return "", false
	}
	head := a.args[0]
	a.args = a.args[1:]
	return head, true
}

// takeFlag mirrors takeFlag.
func (a *argList) takeFlag(name string) bool {
	for index, value := range a.args {
		if value == name {
			a.args = append(a.args[:index:index], a.args[index+1:]...)
			return true
		}
	}
	return false
}

// takeOption mirrors takeOption: a flag without a following non-empty value is
// an error (`--session requires a value`).
func (a *argList) takeOption(name string) (string, error) {
	value, hasValue, err := a.takeOptionValue(name)
	if err != nil {
		return "", err
	}
	if !hasValue {
		return "", nil
	}
	return value, nil
}

// takeOptionValue mirrors takeOption including the "absent" case.
func (a *argList) takeOptionValue(name string) (string, bool, error) {
	for index, current := range a.args {
		if current != name {
			continue
		}
		if index+1 >= len(a.args) || a.args[index+1] == "" {
			return "", false, fmt.Errorf("%s requires a value", name)
		}
		value := a.args[index+1]
		a.args = append(a.args[:index:index], a.args[index+2:]...)
		return value, true, nil
	}
	return "", false, nil
}

// rejectExtra mirrors rejectExtra.
func (a *argList) rejectExtra() error {
	if len(a.args) > 0 {
		return fmt.Errorf("Invalid argument: %s", a.args[0])
	}
	return nil
}

// optionalString mirrors a TS `foo ? { foo } : {}` spread: an empty string is
// absent, exactly like the `herdrSessionName ? ... : {}` conditions.
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// optionalPresent mirrors the truthiness test on an optional string.
func optionalPresent(value *string) bool {
	return value != nil && *value != ""
}

// nonEmptyOptional mirrors nonEmptyString over an optional field.
func nonEmptyOptional(value *string) (string, bool) {
	if value == nil || *value == "" {
		return "", false
	}
	return *value, true
}

// derefOptional mirrors `value ?? ""`.
func derefOptional(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// derefOr mirrors `value ?? fallback`.
func derefOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

// derefOrPointer mirrors `value ?? other`: only an absent first value falls
// through, so an empty string is kept.
func derefOrPointer(value, fallback *string) *string {
	if value != nil {
		return value
	}
	return fallback
}

// parseJSNumber mirrors `Number(value)`, including the unsigned 0x / 0b / 0o
// prefixes strconv.ParseFloat rejects: `Number("0x14")` is 20, not a NaN. A sign
// in front of the prefix is not accepted by JS either (`Number("-0x14")` is a
// NaN), so only the bare two-character prefixes take the radix path.
func parseJSNumber(value string) float64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0
	}
	if len(trimmed) > 2 {
		base := 0
		switch trimmed[:2] {
		case "0x", "0X":
			base = 16
		case "0b", "0B":
			base = 2
		case "0o", "0O":
			base = 8
		}
		if base != 0 {
			parsed, err := strconv.ParseUint(trimmed[2:], base, 64)
			if err != nil {
				return math.NaN()
			}
			return float64(parsed)
		}
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return math.NaN()
	}
	return parsed
}

// jsIsInteger mirrors Number.isInteger.
func jsIsInteger(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value)
}
