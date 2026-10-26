package observability

import (
	"sync"
	"time"
)

// Timing constants mirroring turn-completion.ts.
const (
	TurnSignalWaitMs      = 5000
	TurnSignalRetentionMs = 10 * 60_000
)

// TurnCompletionSignal mirrors the TS type of the same name.
type TurnCompletionSignal struct {
	Confirmed        bool
	ExpectedText     string
	HasExpectedText  bool
	HerdrSessionName string
	PaneID           string
	TerminalID       string
	WorkspaceID      string
	RecordedAtMs     int64
}

// TurnCompletionWaitResult mirrors the TS type of the same name.
type TurnCompletionWaitResult struct {
	Confirmed       bool
	ExpectedText    string
	HasExpectedText bool
	Received        bool
}

// SessionScopedSignal is the input to Record (TS: Omit<TurnCompletionSignal, "recordedAtMs">).
type SessionScopedSignal struct {
	Confirmed        bool
	ExpectedText     string
	HasExpectedText  bool
	HerdrSessionName string
	PaneID           string
	TerminalID       string
	WorkspaceID      string
}

// WaitForSignalInput mirrors the TS waitForSignal input object.
type WaitForSignalInput struct {
	HerdrSessionName string
	TerminalID       string
	RecordedAfterMs  int64
	// Aborted plays the role of an already-aborted AbortSignal.
	Aborted bool
	// Signal, when non-nil, is watched for cancellation like AbortSignal.
	Signal <-chan struct{}
}

// TurnCompletionRegistry mirrors the TS class of the same name.
//
// The TS class injects timers and sleep for testability; the Go port keeps the
// same seams (Now / TimeoutMs) but drives timeouts with time.AfterFunc, so a
// short TimeoutMs is used in tests rather than a fake clock.
type TurnCompletionRegistry struct {
	mu                sync.Mutex
	latestByTerminal  map[string]TurnCompletionSignal
	waitersByTerminal map[string]map[*waiter]struct{}
	timeoutMs         int64
	now               func() int64
	retentionMs       int64
}

// waiter is one pending waitForSignal call.
type waiter struct {
	done          chan TurnCompletionWaitResult
	doneCh        chan struct{}
	recordedAfter int64
	finished      bool
}

// TurnCompletionOption customises a registry (mirrors the TS options object).
type TurnCompletionOption func(*TurnCompletionRegistry)

// WithTurnSettimeout overrides the wait timeout in milliseconds.
func WithTurnSettimeout(timeoutMs int64) TurnCompletionOption {
	return func(r *TurnCompletionRegistry) { r.timeoutMs = timeoutMs }
}

// WithTurnSetNow overrides the clock used for recordedAtMs.
func WithTurnSetNow(now func() int64) TurnCompletionOption {
	return func(r *TurnCompletionRegistry) { r.now = now }
}

// WithTurnSetRetention overrides the retention window in milliseconds.
func WithTurnSetRetention(retentionMs int64) TurnCompletionOption {
	return func(r *TurnCompletionRegistry) { r.retentionMs = retentionMs }
}

// NewTurnCompletionRegistry mirrors the TS constructor defaults.
func NewTurnCompletionRegistry(options ...TurnCompletionOption) *TurnCompletionRegistry {
	registry := &TurnCompletionRegistry{
		latestByTerminal:  make(map[string]TurnCompletionSignal),
		waitersByTerminal: make(map[string]map[*waiter]struct{}),
		timeoutMs:         TurnSignalWaitMs,
		now:               func() int64 { return time.Now().UnixMilli() },
		retentionMs:       TurnSignalRetentionMs,
	}
	for _, option := range options {
		option(registry)
	}
	return registry
}

// Record mirrors registry.record: store the signal, prune, then wake waiters.
func (r *TurnCompletionRegistry) Record(signal SessionScopedSignal) {
	key := terminalTurnKey(signal.HerdrSessionName, signal.TerminalID)
	r.mu.Lock()
	stored := TurnCompletionSignal{
		Confirmed:        signal.Confirmed,
		ExpectedText:     signal.ExpectedText,
		HasExpectedText:  signal.HasExpectedText,
		HerdrSessionName: signal.HerdrSessionName,
		PaneID:           signal.PaneID,
		TerminalID:       signal.TerminalID,
		WorkspaceID:      signal.WorkspaceID,
		RecordedAtMs:     r.now(),
	}
	r.latestByTerminal[key] = stored
	r.pruneLocked()

	// Wake every waiter for this key through finishLocked, which re-reads the
	// stored signal and resolves with its real payload. This mirrors the TS
	// `for (const resolve of [...waiters]) resolve();` loop, where each waiter's
	// own finish() decides whether the signal is new.
	waiters := make([]*waiter, 0, len(r.waitersByTerminal[key]))
	for pending := range r.waitersByTerminal[key] {
		waiters = append(waiters, pending)
	}
	for _, pending := range waiters {
		r.finishLocked(key, pending, false)
	}
	r.mu.Unlock()
}

// WaitForSignal mirrors registry.waitForSignal, including the 5 s default
// timeout and the "consume once" semantics.
func (r *TurnCompletionRegistry) WaitForSignal(input WaitForSignalInput) TurnCompletionWaitResult {
	key := terminalTurnKey(input.HerdrSessionName, input.TerminalID)
	isNewSignal := func(signal TurnCompletionSignal, ok bool) bool {
		return ok && signal.RecordedAtMs >= input.RecordedAfterMs
	}

	r.mu.Lock()
	existing, found := r.latestByTerminal[key]
	if isNewSignal(existing, found) {
		delete(r.latestByTerminal, key)
		r.mu.Unlock()
		return consumeResult(existing)
	}
	if input.Aborted {
		r.mu.Unlock()
		return TurnCompletionWaitResult{}
	}

	// A waiter can be finished twice — a timeout racing a recorded signal, or a
	// signal that arrives after the caller already stopped waiting — so the
	// finishing is guarded by `finished` and announced on doneCh. Without it a
	// late timeout would consume the signal the *next* wait registered, and the
	// goroutine watching options.Signal would stay parked on a channel nobody
	// will ever write to.
	pending := &waiter{
		done:          make(chan TurnCompletionWaitResult, 1),
		doneCh:        make(chan struct{}),
		recordedAfter: input.RecordedAfterMs,
	}
	if r.waitersByTerminal[key] == nil {
		r.waitersByTerminal[key] = make(map[*waiter]struct{})
	}
	r.waitersByTerminal[key][pending] = struct{}{}

	// Timeout: finish(true) after timeoutMs, mirroring setTimeout(...).
	timer := time.AfterFunc(time.Duration(r.timeoutMs)*time.Millisecond, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.finishLocked(key, pending, true)
	})

	// Abort: finish(true) as soon as the signal fires, matching the TS
	// `input.signal.addEventListener("abort", onAbort, { once: true })`. The
	// waiter's doneCh lets this goroutine leave as soon as the wait is over by
	// any other means, instead of holding the channel for the next round.
	if input.Signal != nil {
		signal := input.Signal
		go func() {
			select {
			case <-signal:
				r.mu.Lock()
				defer r.mu.Unlock()
				r.finishLocked(key, pending, true)
			case <-pending.doneCh:
				return
			}
		}()
	}
	r.mu.Unlock()

	result := <-pending.done
	// Equivalent of clearTimeout(timer) + removeEventListener: a resolved waiter
	// no longer writes to its channel, so stopping the timer is purely hygiene.
	timer.Stop()
	return result
}

// finishLocked mirrors the TS finish(): it only resolves when a new signal is
// present, or when timedOut is set.
func (r *TurnCompletionRegistry) finishLocked(key string, pending *waiter, timedOut bool) {
	// A waiter that is already finished — and a waiter the last wake already
	// removed from the registry — must not be resolved a second time: the
	// resolution would deliver to a channel nobody reads, and worse, could
	// consume the signal the next wait for this key has just registered.
	if pending.finished {
		return
	}
	if waiters, ok := r.waitersByTerminal[key]; ok {
		if _, stillPending := waiters[pending]; !stillPending {
			return
		}
	}
	latest, found := r.latestByTerminal[key]
	isNew := found && latest.RecordedAtMs >= pending.recordedAfter
	if !timedOut && !isNew {
		return
	}
	pending.finished = true
	close(pending.doneCh)
	r.removeWaiterLocked(key, pending)

	var result TurnCompletionWaitResult
	if found && isNew {
		delete(r.latestByTerminal, key)
		result = consumeResult(latest)
	}
	pending.done <- result
}

func consumeResult(signal TurnCompletionSignal) TurnCompletionWaitResult {
	result := TurnCompletionWaitResult{Confirmed: signal.Confirmed, Received: true}
	if signal.HasExpectedText && signal.ExpectedText != "" {
		result.ExpectedText = signal.ExpectedText
		result.HasExpectedText = true
	}
	return result
}

func (r *TurnCompletionRegistry) removeWaiterLocked(key string, pending *waiter) {
	if waiters, ok := r.waitersByTerminal[key]; ok {
		delete(waiters, pending)
		if len(waiters) == 0 {
			delete(r.waitersByTerminal, key)
		}
	}
}

// pruneLocked mirrors #prune: drop signals older than the retention window.
func (r *TurnCompletionRegistry) pruneLocked() {
	if len(r.latestByTerminal) <= 1 {
		return
	}
	cutoff := r.now() - r.retentionMs
	for key, signal := range r.latestByTerminal {
		if signal.RecordedAtMs < cutoff {
			delete(r.latestByTerminal, key)
		}
	}
}

func terminalTurnKey(herdrSessionName, terminalID string) string {
	return herdrSessionName + "\x00" + terminalID
}
