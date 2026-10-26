package agy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// This file ports the WakeWaiter half of src/agy/hook.ts: the standby loop, the
// detached self-respawn, the wake-prompt trigger, the peer inspection helpers
// and `resolveWakeWaiterCwd`.

// RpcClient is the `Pick<ObservabilityRpcClient, "close" | "request">` seam the
// hook is handed. `daemon.ObservabilityRpcClient` satisfies it, and callers
// outside this package can plug in their own implementation through
// RunAgyHookOptions.Connect.
type RpcClient interface {
	Close()
	Call(method string, params any) (any, error)
}

// rpcClient is the package-internal spelling of RpcClient.
type rpcClient = RpcClient

// defaultConnect mirrors the production `connect` option.
func defaultConnect(socketPath string, onNotification func(method string, params any)) (rpcClient, error) {
	return daemon.NewObservabilityRpcClient(daemon.ObservabilityRpcClientOptions{
		OnNotification: onNotification,
		SocketPath:     socketPath,
	})
}

// errStopWaiter is the Go spelling of the TS `return {}` inside the waiter
// loop: the waiter stops without logging an error.
var errStopWaiter = errors.New("agy wake waiter stopped")

// AgentListItemView mirrors the AgentListItem projection the hook reads. The
// optional fields stay optional so a missing key keeps the TS `undefined`
// semantics the readiness checks rely on.
type AgentListItemView struct {
	AgentStatus *observability.AgentStatus `json:"agentStatus"`
	FirstSeenAt *string                    `json:"firstSeenAt"`
	History     PeerHistory                `json:"history"`
	PaneID      string                     `json:"paneId"`
	TerminalID  *string                    `json:"terminalId"`
	WorkspaceID *string                    `json:"workspaceId"`
}

// PeerHistory mirrors the `history` projection on AgentListItem.
type PeerHistory struct {
	LastAssistantMessage *PeerHistoryExcerpt `json:"lastAssistantMessage"`
	LastUserMessage      *PeerHistoryExcerpt `json:"lastUserMessage"`
	UpdatedAt            *string             `json:"updatedAt"`
}

// PeerHistoryExcerpt mirrors AgentHistoryExcerpt (the hook reads ref, stopReason
// and timestamp).
type PeerHistoryExcerpt struct {
	Ref        *string `json:"ref"`
	StopReason *string `json:"stopReason"`
	Timestamp  *string `json:"timestamp"`
}

// runWakeWaiter mirrors the WakeWaiter branch of runAgyHook.
func (r *runner) runWakeWaiter() HookOutput {
	if IsDispatchedSubagent(r.env) {
		return HookOutput{}
	}
	initialSessionState := r.readState()
	rawEnvEpoch, hasEnvEpoch := nonEmptyStringValue(r.env["HERDSMAN_AGY_WAITER_EPOCH"])
	parsedEnvEpoch := math.NaN()
	if hasEnvEpoch {
		parsedEnvEpoch = parseJSNumber(rawEnvEpoch)
	}
	waiterEpoch := 0.0
	switch {
	case isIntegerNumber(parsedEnvEpoch):
		waiterEpoch = parsedEnvEpoch
	default:
		if value, ok := initialSessionState.WaiterEpochValue(); ok {
			waiterEpoch = value
		} else if value, ok := initialSessionState.TurnEpochValue(); ok {
			waiterEpoch = value
		}
	}

	waitTimeoutMs := defaultWaitTimeoutMs
	if r.options.WaitTimeoutMs != nil {
		waitTimeoutMs = *r.options.WaitTimeoutMs
	}
	requestTimeoutMs := defaultRequestTimeoutMs
	if r.options.RequestTimeoutMs != nil {
		requestTimeoutMs = *r.options.RequestTimeoutMs
	}
	pollIntervalMs := defaultPollIntervalMs
	if r.options.PollIntervalMs != nil {
		pollIntervalMs = *r.options.PollIntervalMs
	}
	idlePollIntervalMs := defaultIdlePollIntervalMs
	if r.options.IdlePollIntervalMs != nil {
		idlePollIntervalMs = *r.options.IdlePollIntervalMs
	}
	newAgentGraceMs := defaultNewAgentGraceMs
	if r.options.NewAgentGraceMs != nil {
		newAgentGraceMs = *r.options.NewAgentGraceMs
	}
	settleGraceMs := defaultSettleGraceMs
	if r.options.SettleGraceMs != nil {
		settleGraceMs = *r.options.SettleGraceMs
	}
	exitWhenNoPeers := r.options.ExitWhenNoPeers != nil && *r.options.ExitWhenNoPeers

	w := &waiter{
		runner:             r,
		sessionID:          r.sessionID,
		stateDir:           r.stateDir,
		waiterEpoch:        waiterEpoch,
		now:                r.now,
		pollIntervalMs:     pollIntervalMs,
		idlePollIntervalMs: idlePollIntervalMs,
		exitWhenNoPeers:    exitWhenNoPeers,
		newAgentGraceMs:    newAgentGraceMs,
		settleGraceMs:      settleGraceMs,
		deadline:           r.now() + waitTimeoutMs,
		wakeCh:             make(chan struct{}, 1),
		isFirstIteration:   true,
		isFirstEventCheck:  true,
	}

	rawClient, err := r.connect(r.options.SocketPath, func(method string, params any) {
		if method == "agent.event" || method == "agent.orchestrator.changed" {
			w.hasPushNotification.Store(true)
			select {
			case w.wakeCh <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		r.warn("Herdsman agy WakeWaiter exited after error: " + err.Error())
		return HookOutput{}
	}
	w.client = &timeoutClient{client: rawClient, timeoutMs: requestTimeoutMs}
	defer func() {
		w.client.Close()
		w.clearOwnWaiterPidSafely()
	}()

	connState, err := r.registerAgyPresence(w.client, false)
	if err != nil {
		r.warn("Herdsman agy WakeWaiter exited after error: " + err.Error())
		return HookOutput{}
	}
	w.connState = connState

	if err := w.loop(); err != nil && !errors.Is(err, errStopWaiter) {
		r.warn("Herdsman agy WakeWaiter exited after error: " + err.Error())
	}
	return HookOutput{}
}

// waiter holds the standby loop state that the TS locals keep across
// iterations.
type waiter struct {
	runner             *runner
	client             rpcClient
	connState          ConnectionState
	sessionID          string
	stateDir           string
	waiterEpoch        float64
	now                func() float64
	pollIntervalMs     float64
	idlePollIntervalMs float64
	exitWhenNoPeers    bool
	newAgentGraceMs    float64
	settleGraceMs      float64
	deadline           float64
	wakeCh             chan struct{}

	hasPushNotification        atomic.Bool
	isFirstIteration           bool
	isFirstEventCheck          bool
	retryWakePending           bool
	deliveredBatchGracePending bool
	wakePromptFailures         int
	rpcFailures                int
	sawActivePeers             bool
	sawSelf                    bool
	settleDeadline             *float64
}

// clearOwnWaiterPidSafely mirrors the `try { clearOwnWaiterPid(...) } catch {}`
// of the finally block.
func (w *waiter) clearOwnWaiterPidSafely() {
	defer func() { _ = recover() }()
	w.runner.clearOwnWaiterPid()
}

func (w *waiter) clearOwnWaiterPid() { w.runner.clearOwnWaiterPid() }

func (w *waiter) runnerPID() int { return w.runner.pid }

// loop is the `while (now() < deadline)` body of the TS implementation. It
// returns errStopWaiter for a clean early stop and any other error for the
// outer catch (which logs and returns an empty output).
func (w *waiter) loop() error {
	for w.now() < w.deadline {
		sessionState := readSessionState(w.stateDir, w.sessionID)
		if turnEpoch, ok := sessionState.TurnEpochValue(); ok && turnEpoch != w.waiterEpoch {
			return errStopWaiter
		}
		if !w.isFirstIteration {
			if pid, ok := sessionState.WaiterPIDValue(); ok && pid != w.runnerPID() {
				return errStopWaiter
			}
		}
		w.isFirstIteration = false

		// Already-delivered but unacked events are a wake batch of their own.
		hasUnackedDeliveredEvents := len(sessionState.Delivered()) > 0

		hasActivePeers := false
		hasPeers := false
		var selfAgentStatus *observability.AgentStatus
		hasWakeBatch := false
		stop := false

		err := func() error {
			var inspectErr error
			hasActivePeers, hasPeers, selfAgentStatus, inspectErr = w.runner.inspectWorkspaceAgents(w.client, w.connState.Presence)
			if inspectErr != nil {
				return inspectErr
			}
			if selfAgentStatus != nil {
				w.sawSelf = true
			}
			if w.sawSelf && selfAgentStatus == nil {
				// The orchestrator pane itself left the Herdr index.
				w.clearOwnWaiterPid()
				stop = true
				return nil
			}
			if hasActivePeers {
				w.sawActivePeers = true
				w.settleDeadline = nil
			}
			selfReadyToWake := w.selfReadyToWake(selfAgentStatus)

			if selfReadyToWake && (w.isFirstEventCheck ||
				w.hasPushNotification.Load() ||
				w.retryWakePending ||
				w.deliveredBatchGracePending ||
				!hasActivePeers) {
				latestStateValue, callErr := w.client.Call("agent.orchestrator.get", map[string]any{})
				if callErr != nil {
					return callErr
				}
				latestState := decodeConnectionState(latestStateValue)
				if !isLocalOwner(latestState) {
					stop = true
					return nil
				}

				peekedBatch, peekErr := w.runner.peekNextWakeBatch(w.client, latestState, &sessionState, &waiterIdentity{
					waiterEpoch: w.waiterEpoch,
					waiterPID:   w.runnerPID(),
				})
				if peekErr != nil {
					return peekErr
				}
				hasWakeBatch = hasUnackedDeliveredEvents || peekedBatch.HasOutcomes
				w.isFirstEventCheck = false
				w.hasPushNotification.Store(false)
				w.retryWakePending = false
				// Completions deferred because agent.list still reports the peer as
				// working must be re-checked on the next poll: a stale "working" row
				// is the race that used to permanently ack the real done event.
				if peekedBatch.HasDeferredWorking {
					w.retryWakePending = true
				}

				if hasWakeBatch && hasUnackedDeliveredEvents && !peekedBatch.HasOutcomes && !w.deliveredBatchGracePending {
					// Give the host one poll interval to start the next invocation
					// (which bumps turnEpoch and cancels this waiter) before falling
					// back to a wake prompt.
					w.deliveredBatchGracePending = true
				} else if hasWakeBatch {
					w.deliveredBatchGracePending = false
					preTriggerState := readSessionState(w.stateDir, w.sessionID)
					if turnEpoch, ok := preTriggerState.TurnEpochValue(); ok && turnEpoch != w.waiterEpoch {
						stop = true
						return nil
					}
					// Avoid submitting the wake prompt on top of a half-typed draft
					// in the agy composer. Deferring is safe: the next poll retries,
					// and if the user submits the draft themselves PreInvocation
					// still runs takeNextWakeBatch over the pending events.
					if w.runner.readComposerDraft(w.runner.launch.PaneID) == composerDraftHasDraft {
						w.runner.warn("Herdsman agy wake deferred: 输入框有草稿，推迟叫醒 (pane " + w.runner.launch.PaneID + ")")
						w.retryWakePending = true
					} else {
						// readComposerDraft is I/O (up to 5s). While it was in flight the
						// user may have submitted a new turn (bumping turnEpoch) or a fresh
						// Stop may have started a new waiter, so the state this loop decided
						// on can already be superseded. Re-check before touching shared
						// state: rearming the batch or prompting on behalf of a stale turn
						// would duplicate an injection the host already received.
						postReadState := readSessionState(w.stateDir, w.sessionID)
						stale := false
						if turnEpoch, ok := postReadState.TurnEpochValue(); ok && turnEpoch != w.waiterEpoch {
							stale = true
						}
						if waiterEpoch, ok := postReadState.WaiterEpochValue(); ok && waiterEpoch != w.waiterEpoch {
							stale = true
						}
						if waiterPID, ok := postReadState.WaiterPIDValue(); ok && waiterPID != w.runnerPID() {
							stale = true
						}
						if stale {
							stop = true
							return nil
						}
						// The stop hook marked the batch presented before returning it
						// to the host; the turn started by this prompt has to receive
						// it in PreInvocation, so re-arm the unacked batch first.
						w.runner.rearmDeliveredEvents()
						if promptErr := w.runner.triggerWakePrompt(w.runner.launch.PaneID, wakePromptText); promptErr != nil {
							w.wakePromptFailures++
							w.runner.warn(fmt.Sprintf(
								"Herdsman agy wake prompt failed (attempt %d/%d): %s",
								w.wakePromptFailures, maxWakePromptAttempts, promptErr.Error(),
							))
							if w.wakePromptFailures >= maxWakePromptAttempts {
								stop = true
								return nil
							}
							w.retryWakePending = true
						} else {
							postTriggerState := readSessionState(w.stateDir, w.sessionID)
							if turnEpoch, ok := postTriggerState.TurnEpochValue(); ok && turnEpoch != w.waiterEpoch {
								w.runner.warn(fmt.Sprintf(
									"Herdsman agy wake prompt raced with a new turn (waiterEpoch=%s, turnEpoch=%s)",
									formatJSNumber(w.waiterEpoch), formatJSNumber(turnEpoch),
								))
								stop = true
								return nil
							}
							if pid, ok := postTriggerState.WaiterPIDValue(); ok && pid == w.runnerPID() {
								postTriggerState.WaiterPID = nil
								w.runner.writeState(postTriggerState)
							}
							stop = true
							return nil
						}
					}
				}
			}
			w.rpcFailures = 0
			return nil
		}()
		if err != nil {
			w.rpcFailures++
			if w.rpcFailures >= maxWakePromptAttempts {
				return err
			}
			w.runner.warn(fmt.Sprintf(
				"Herdsman agy WakeWaiter transient RPC error (attempt %d/%d): %s",
				w.rpcFailures, maxWakePromptAttempts, err.Error(),
			))
			remainingAfterErrMs := w.deadline - w.now()
			if remainingAfterErrMs <= 0 {
				return nil
			}
			w.waitForNotificationOrTimeout(math.Min(w.pollIntervalMs, math.Max(1, remainingAfterErrMs)))
			continue
		}
		if stop {
			return errStopWaiter
		}

		selfReadyToWake := w.selfReadyToWake(selfAgentStatus)
		if !hasActivePeers && !hasWakeBatch && selfReadyToWake && !w.retryWakePending {
			// Default: stay on standby for the next batch. Only an explicit
			// `exitWhenNoPeers` restores the old exit-when-idle behaviour.
			if w.exitWhenNoPeers {
				if (w.sawActivePeers || hasPeers) && w.settleGraceMs > 0 {
					if w.settleDeadline == nil {
						settleDeadline := w.now() + w.settleGraceMs
						w.settleDeadline = &settleDeadline
					}
					if w.now() >= *w.settleDeadline {
						w.clearOwnWaiterPid()
						return errStopWaiter
					}
				} else {
					w.clearOwnWaiterPid()
					return errStopWaiter
				}
			} else {
				w.settleDeadline = nil
			}
		} else {
			w.settleDeadline = nil
		}

		remainingMs := w.deadline - w.now()
		if remainingMs <= 0 {
			return nil
		}
		settleRemainingMs := remainingMs
		if w.settleDeadline != nil {
			settleRemainingMs = math.Max(0, *w.settleDeadline-w.now())
		}
		isIdleStandby := !w.exitWhenNoPeers &&
			!hasActivePeers &&
			!hasPeers &&
			!hasWakeBatch &&
			!hasUnackedDeliveredEvents &&
			selfReadyToWake &&
			!w.retryWakePending
		baseSleepMs := w.pollIntervalMs
		if isIdleStandby {
			baseSleepMs = math.Max(w.pollIntervalMs, w.idlePollIntervalMs)
		}
		sleepMs := math.Min(math.Min(baseSleepMs, remainingMs), settleRemainingMs)
		if sleepMs <= 0 {
			continue
		}
		w.waitForNotificationOrTimeout(sleepMs)
	}
	return nil
}

// selfReadyToWake mirrors `selfAgentStatus === undefined || "idle" || "done"`.
func (w *waiter) selfReadyToWake(status *observability.AgentStatus) bool {
	if status == nil {
		return true
	}
	return *status == observability.AgentStatusIdle || *status == observability.AgentStatusDone
}

// waitForNotificationOrTimeout mirrors waitForNotificationOrTimeout: only a
// notification that arrives *during* the wait may cut it short (the TS
// `notifyWakeWaiter` seam is armed for the duration of the wait only).
func (w *waiter) waitForNotificationOrTimeout(timeoutMs float64) {
	for drained := false; !drained; {
		select {
		case <-w.wakeCh:
		default:
			drained = true
		}
	}
	limit := math.Min(math.Max(0, timeoutMs), maxSetTimeoutMs)
	if limit <= 0 {
		return
	}
	timer := time.NewTimer(time.Duration(limit * float64(time.Millisecond)))
	defer timer.Stop()
	select {
	case <-w.wakeCh:
	case <-timer.C:
	}
}

// inspectWorkspaceAgents mirrors inspectWorkspaceAgents.
func (r *runner) inspectWorkspaceAgents(
	client rpcClient,
	presence Presence,
) (bool, bool, *observability.AgentStatus, error) {
	result, err := client.Call("agent.list", map[string]any{
		"herdrSessionName": presence.HerdrSessionName,
		// Workers dispatched into a sibling workspace of the same Herdr session
		// have no orchestrator owner of their own.
		"includeOwnerlessWorkspaces": true,
		"workspaceId":                presence.WorkspaceID,
	})
	if err != nil {
		return false, false, nil, err
	}
	agents := decodeAgentList(result)
	var selfAgentStatus *observability.AgentStatus
	peers := make([]AgentListItemView, 0, len(agents))
	for _, agent := range agents {
		if agent.PaneID == presence.PaneID && selfAgentStatus == nil {
			selfAgentStatus = agent.AgentStatus
		}
		if !terminalIDEquals(agent.TerminalID, presence.TerminalID) {
			peers = append(peers, agent)
		}
	}
	hasActivePeers := false
	nowMs := r.now()
	for _, peer := range peers {
		if IsPeerActiveOrSettling(peer, nowMs, r.newAgentGraceMs(), r.settleGraceMs()) {
			hasActivePeers = true
			break
		}
	}
	return hasActivePeers, len(peers) > 0, selfAgentStatus, nil
}

func (r *runner) newAgentGraceMs() float64 {
	if r.options.NewAgentGraceMs != nil {
		return *r.options.NewAgentGraceMs
	}
	return defaultNewAgentGraceMs
}

func (r *runner) settleGraceMs() float64 {
	if r.options.SettleGraceMs != nil {
		return *r.options.SettleGraceMs
	}
	return defaultSettleGraceMs
}

// fetchLivePeerScope mirrors fetchLivePeerScope: the live peer view of this
// orchestrator scope, straight from `agent.list`.
//
// A nil result is the TS `undefined`: `agent.list` failed, so nothing is known.
// Completions are then projected as real outcomes (the pre-fix behaviour)
// instead of dropping the whole batch, and unvalidated snapshot rows are kept.
func (r *runner) fetchLivePeerScope(client rpcClient, presence Presence) *livePeerScope {
	result, err := client.Call("agent.list", map[string]any{
		"herdrSessionName":           presence.HerdrSessionName,
		"includeOwnerlessWorkspaces": true,
		"workspaceId":                presence.WorkspaceID,
	})
	if err != nil {
		return nil
	}
	agents := decodeAgentList(result)
	liveScopeWorkspaces := map[string]struct{}{presence.WorkspaceID: {}}
	liveTerminalIDs := map[string]struct{}{}
	workingTerminalIDs := map[string]struct{}{}
	for _, agent := range agents {
		if workspaceID, ok := nonEmptyStringPtr(agent.WorkspaceID); ok {
			liveScopeWorkspaces[workspaceID] = struct{}{}
		}
		if !truthyString(agent.TerminalID) || terminalIDEquals(agent.TerminalID, presence.TerminalID) {
			continue
		}
		liveTerminalIDs[*agent.TerminalID] = struct{}{}
		if agent.AgentStatus != nil && *agent.AgentStatus == observability.AgentStatusWorking {
			workingTerminalIDs[*agent.TerminalID] = struct{}{}
		}
	}
	return &livePeerScope{
		LiveScopeWorkspaces: liveScopeWorkspaces,
		LiveTerminalIDs:     liveTerminalIDs,
		WorkingTerminalIDs:  workingTerminalIDs,
	}
}

// decodeAgentList mirrors the `as { agents?: AgentListItem[] }` cast plus the
// `?? []` fallback.
func decodeAgentList(value any) []AgentListItemView {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	response := struct {
		Agents []AgentListItemView `json:"agents"`
	}{}
	if err := json.Unmarshal(encoded, &response); err != nil {
		return nil
	}
	return response.Agents
}

// timeoutClient mirrors withRequestTimeout.
type timeoutClient struct {
	client    rpcClient
	timeoutMs float64
}

type rpcOutcome struct {
	value any
	err   error
}

func (c *timeoutClient) Close() { c.client.Close() }

// Call mirrors the wrapped request: the client is closed on timeout, which is
// what makes the racing request fail instead of leaking.
func (c *timeoutClient) Call(method string, params any) (any, error) {
	effectiveTimeoutMs := math.Max(1, math.Min(c.timeoutMs, maxSetTimeoutMs))
	settled := make(chan rpcOutcome, 1)
	go func() {
		value, err := c.client.Call(method, params)
		settled <- rpcOutcome{value: value, err: err}
	}()
	timer := time.NewTimer(time.Duration(effectiveTimeoutMs * float64(time.Millisecond)))
	defer timer.Stop()
	select {
	case outcome := <-settled:
		return outcome.value, outcome.err
	case <-timer.C:
		func() {
			defer func() { _ = recover() }()
			c.client.Close()
		}()
		return nil, fmt.Errorf(
			"Herdsman agy WakeWaiter RPC request timed out after %sms: %s",
			formatJSNumber(effectiveTimeoutMs), method,
		)
	}
}

// defaultTriggerWakePrompt mirrors defaultTriggerWakePrompt:
// `herdr agent prompt <paneId> [herdsman-wake]` with a 5s timeout.
func defaultTriggerWakePrompt(paneID, promptText string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "herdr", "agent", "prompt", paneID, promptText).Run()
}

// defaultReadPaneText mirrors defaultReadPaneText: `herdr pane read <paneId>
// --lines 40` with a 5s timeout (40 lines so a multi-line draft plus the box
// frame stays inside the dump). Any failure reads as "no text" (a nil result)
// so the caller falls back to the pre-fix wake behaviour.
func defaultReadPaneText(paneID string) (*string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, err := exec.CommandContext(ctx, "herdr", "pane", "read", paneID, "--lines", "40").Output()
	if err != nil {
		return nil, nil
	}
	text := string(stdout)
	return &text, nil
}

// composerSeparatorPattern mirrors `/^[\s]*[─-]{8,}[\s]*$/`: a box-drawing
// horizontal rule (an ASCII-dash fallback is tolerated the same way, so the
// class is written `[-─]` for RE2).
var composerSeparatorPattern = regexp.MustCompile(`^[\s]*[-─]{8,}[\s]*$`)

// composerPromptPattern mirrors `/^(?:>\s?)(.*)$/` on the first composer line.
var composerPromptPattern = regexp.MustCompile(`^(?:>\s?)(.*)$`)

// composerTrailingWhitespacePattern mirrors `/[ \t]+$/gm`.
var composerTrailingWhitespacePattern = regexp.MustCompile(`(?m)[ \t]+$`)

// ParseAgyComposerDraft mirrors parseAgyComposerDraft: it parses the agy
// composer at the bottom of a `herdr pane read` dump.
//
// An empty composer (observed on real panes) is a lone `>` between two `───`
// rules with no placeholder text after the prompt; a non-empty draft is any
// trimmed content after that `>` and/or extra lines between the rules.
//
// A nil result is the TS `undefined`: the composer region could not be located,
// so callers fall back to the pre-fix wake behaviour instead of blocking.
func ParseAgyComposerDraft(paneText string) *string {
	lines := strings.Split(composerTrailingNewlinePattern.ReplaceAllString(paneText, "\n"), "\n")
	separatorIndexes := make([]int, 0, 4)
	for index, line := range lines {
		if composerSeparatorPattern.MatchString(line) {
			separatorIndexes = append(separatorIndexes, index)
		}
	}
	if len(separatorIndexes) < 2 {
		return nil
	}
	bottom := separatorIndexes[len(separatorIndexes)-1]
	// The rule right above the status bar is the bottom of the composer box.
	bottomLine := strings.TrimSpace(lines[bottom])
	// The top rule is *not* simply the previous rule: a multi-line draft may itself
	// contain a `--------` / `────────` line followed by a `>` quote line, which then
	// looks exactly like a composer top rule and would collapse the box (and, for an
	// empty `>`, return an empty draft). The composer frame is drawn by one TUI
	// component, so its top and bottom rules share the same characters and width:
	// prefer candidates that match the bottom rule byte for byte, and among those
	// walk outwards until one yields a non-empty draft.
	followedByPrompt := make([]int, 0, len(separatorIndexes))
	for _, separatorIndex := range separatorIndexes[:len(separatorIndexes)-1] {
		if composerPromptLinePattern.MatchString(lineAt(lines, separatorIndex+1)) {
			followedByPrompt = append(followedByPrompt, separatorIndex)
		}
	}
	if len(followedByPrompt) == 0 {
		return nil
	}
	matchingFrame := make([]int, 0, len(followedByPrompt))
	for _, separatorIndex := range followedByPrompt {
		if strings.TrimSpace(lineAt(lines, separatorIndex)) == bottomLine {
			matchingFrame = append(matchingFrame, separatorIndex)
		}
	}
	topCandidates := matchingFrame
	if len(topCandidates) == 0 {
		topCandidates = followedByPrompt
	}

	sawCandidate := false
	for index := len(topCandidates) - 1; index >= 0; index-- {
		separatorIndex := topCandidates[index]
		if separatorIndex >= bottom-1 {
			continue
		}
		region := lines[separatorIndex+1 : bottom]
		if len(region) == 0 {
			continue
		}
		sawCandidate = true
		draftLines := make([]string, 0, len(region))
		for regionIndex, line := range region {
			if regionIndex == 0 {
				if match := composerPromptPattern.FindStringSubmatch(line); match != nil {
					draftLines = append(draftLines, match[1])
					continue
				}
			}
			draftLines = append(draftLines, line)
		}
		draft := strings.TrimRightFunc(
			composerTrailingWhitespacePattern.ReplaceAllString(strings.Join(draftLines, "\n"), ""),
			unicode.IsSpace,
		)
		// An innermost candidate that parses to an empty draft is the rule the user
		// typed inside the composer; the real frame is the enclosing one.
		if strings.TrimSpace(draft) != "" {
			return &draft
		}
	}
	// Every same-frame candidate is an empty `>` prompt line: the composer is empty.
	if !sawCandidate {
		return nil
	}
	empty := ""
	return &empty
}

// composerPromptLinePattern mirrors `/^>(?:\s|$)/`: the `>` prompt line that must
// follow a composer top rule.
var composerPromptLinePattern = regexp.MustCompile(`^>(?:\s|$)`)

// lineAt mirrors the TS `lines[index] ?? ""` guard.
func lineAt(lines []string, index int) string {
	if index < 0 || index >= len(lines) {
		return ""
	}
	return lines[index]
}

// composerTrailingNewlinePattern mirrors `replace(/\r\n?/g, "\n")`.
var composerTrailingNewlinePattern = regexp.MustCompile(`\r\n?`)

// composerDraftState mirrors the `"empty" | "has-draft" | "unknown"` result of
// readComposerDraft.
type composerDraftState int

const (
	composerDraftEmpty composerDraftState = iota
	composerDraftHasDraft
	composerDraftUnknown
)

// readComposerDraft mirrors readComposerDraft: a read or parse failure is
// `unknown`, which keeps the pre-fix wake behaviour.
func (r *runner) readComposerDraft(paneID string) composerDraftState {
	if r.readPaneText == nil {
		return composerDraftUnknown
	}
	paneText, err := r.readPaneText(paneID)
	if err != nil || paneText == nil {
		return composerDraftUnknown
	}
	draft := ParseAgyComposerDraft(*paneText)
	if draft == nil {
		return composerDraftUnknown
	}
	if len(strings.TrimSpace(*draft)) > 0 {
		return composerDraftHasDraft
	}
	return composerDraftEmpty
}

// defaultSpawnWakeWaiter mirrors defaultSpawnWakeWaiter: the waiter is this
// binary re-entered as `agy-hook WakeWaiter`, detached from the hook process and
// handed the three HERDSMAN_AGY_* variables.
func (r *runner) defaultSpawnWakeWaiter(input SpawnWakeWaiterInput) *SpawnWakeWaiterResult {
	executable, err := os.Executable()
	if err != nil {
		return nil
	}
	command := exec.Command(executable, "agy-hook", "WakeWaiter")
	command.Dir = input.Cwd
	command.Env = wakeWaiterEnvironment(input)
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	// detach: the waiter outlives the hook and is reparented to PID 1.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		r.warn("Failed to spawn Herdsman AGY wake waiter: " + err.Error())
		return nil
	}
	pid := command.Process.Pid
	_ = command.Process.Release()
	return &SpawnWakeWaiterResult{PID: &pid}
}

// wakeWaiterEnvironment mirrors the env object of the TS spawn: the input
// environment plus the three waiter variables.
func wakeWaiterEnvironment(input SpawnWakeWaiterInput) []string {
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
		"HERDSMAN_AGY_WAITER_EPOCH="+formatJSNumber(input.WaiterEpoch),
	)
	return environment
}

// ResolveWakeWaiterCwdOptions mirrors the `{ baseCwd, pid }` options of
// resolveWakeWaiterCwd plus the Go-only platform and `/proc` seams.
type ResolveWakeWaiterCwdOptions struct {
	BaseCwd  *string
	GOOS     string
	PID      int
	ReadCwd  func(pid int) (string, bool)
	ReadPPID func(pid int) (int, bool)
}

// maxWakeCwdAncestorDepth mirrors MAX_WAKE_CWD_ANCESTOR_DEPTH; it is kept in
// sync with the daemon's pane-binding ancestor walk.
const maxWakeCwdAncestorDepth = 4

// ResolveWakeWaiterCwd mirrors resolveWakeWaiterCwd:
//
//  1. `payload.cwd`;
//  2. the first existing entry of `payload.workspacePaths` (else the first
//     non-empty one);
//  3. the cwd of this process' nearest ancestor that differs from `baseCwd`;
//  4. `baseCwd` itself.
func ResolveWakeWaiterCwd(payload map[string]any, options *ResolveWakeWaiterCwdOptions) string {
	if explicitCwd, ok := payloadString(payload, "cwd"); ok {
		return explicitCwd
	}

	candidates := []string{}
	if workspacePaths, ok := payload["workspacePaths"].([]any); ok {
		for _, candidate := range workspacePaths {
			if value, ok := nonEmptyStringValue(candidate); ok {
				candidates = append(candidates, value)
			}
		}
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}

	baseCwd := ""
	if options != nil && options.BaseCwd != nil && *options.BaseCwd != "" {
		baseCwd = *options.BaseCwd
	}
	if baseCwd == "" {
		if workingDir, err := os.Getwd(); err == nil {
			baseCwd = workingDir
		}
	}
	pid := os.Getpid()
	goos := runtime.GOOS
	readCwd := readProcessCwd
	readPPID := readProcessParentPid
	if options != nil {
		if options.PID != 0 {
			pid = options.PID
		}
		if options.GOOS != "" {
			goos = options.GOOS
		}
		if options.ReadCwd != nil {
			readCwd = options.ReadCwd
		}
		if options.ReadPPID != nil {
			readPPID = options.ReadPPID
		}
	}
	if ancestor, ok := ancestorCwdDifferingFrom(baseCwd, pid, goos, readCwd, readPPID); ok {
		return ancestor
	}
	return baseCwd
}

// ancestorCwdDifferingFrom mirrors ancestorCwdDifferingFrom.
func ancestorCwdDifferingFrom(
	baseCwd string,
	pid int,
	goos string,
	readCwd func(pid int) (string, bool),
	readPPID func(pid int) (int, bool),
) (string, bool) {
	if goos != "linux" {
		return "", false
	}
	if pid <= 1 {
		return "", false
	}
	normalizedBase := normalizeWakeCwd(baseCwd)
	current := pid
	for step := 0; step <= maxWakeCwdAncestorDepth && current > 1; step++ {
		currentCwd, ok := readCwd(current)
		if !ok {
			return "", false
		}
		if normalizeWakeCwd(currentCwd) != normalizedBase {
			return currentCwd, true
		}
		parent, ok := readPPID(current)
		if !ok || parent <= 1 || parent == current {
			return "", false
		}
		current = parent
	}
	return "", false
}

// readProcessCwd mirrors readProcessCwd.
func readProcessCwd(pid int) (string, bool) {
	target, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return "", false
	}
	return target, true
}

// ppidPattern mirrors `/^PPid:\s*(\d+)\s*$/m`.
var ppidPattern = regexp.MustCompile(`(?m)^PPid:[ \t]*([0-9]+)[ \t]*$`)

// readProcessParentPid mirrors readProcessParentPid.
func readProcessParentPid(pid int) (int, bool) {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	match := ppidPattern.FindStringSubmatch(string(status))
	if match == nil {
		return 0, false
	}
	parent, err := strconv.Atoi(match[1])
	if err != nil || parent < 0 {
		return 0, false
	}
	return parent, true
}

// normalizeWakeCwd mirrors normalizeWakeCwd.
func normalizeWakeCwd(value string) string {
	if resolved, err := filepath.EvalSymlinks(value); err == nil {
		return resolved
	}
	if len(value) > 1 && strings.HasSuffix(value, "/") {
		return value[:len(value)-1]
	}
	return value
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

// isIntegerNumber mirrors Number.isInteger.
func isIntegerNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value)
}

// formatJSNumber mirrors `String(number)` for the numbers this package renders
// (epochs, timeouts and durations): integers print without a fractional part.
func formatJSNumber(value float64) string {
	if isIntegerNumber(value) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}
