package observability

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

func testSignal(overrides func(*SessionScopedSignal)) SessionScopedSignal {
	signal := SessionScopedSignal{
		Confirmed:        true,
		HerdrSessionName: "default",
		PaneID:           "wJ:p2",
		TerminalID:       "term_pi",
		WorkspaceID:      "wJ",
	}
	if overrides != nil {
		overrides(&signal)
	}
	return signal
}

func TestRecordedSignalIsConsumedAtRecordTime(t *testing.T) {
	current := int64(1000)
	registry := NewTurnCompletionRegistry(
		WithTurnSetNow(func() int64 { return current }),
		WithTurnSettimeout(3000),
	)
	registry.Record(testSignal(nil))
	got := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "default",
		TerminalID:       "term_pi",
		RecordedAfterMs:  current,
	})
	if !got.Confirmed || !got.Received {
		t.Fatalf("result = %+v, want {confirmed:true received:true}", got)
	}
}

func TestRecordedSignalIsConsumedOnlyOnce(t *testing.T) {
	current := int64(1000)
	registry := NewTurnCompletionRegistry(
		WithTurnSetNow(func() int64 { return current }),
		WithTurnSettimeout(50),
	)
	registry.Record(testSignal(nil))
	first := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "default",
		TerminalID:       "term_pi",
		RecordedAfterMs:  current,
	})
	if !first.Received {
		t.Fatalf("first wait = %+v, want received", first)
	}
	second := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "default",
		TerminalID:       "term_pi",
		RecordedAfterMs:  current,
	})
	if second.Received || second.Confirmed {
		t.Fatalf("second wait = %+v, want {false,false}", second)
	}
}

func TestSignalAlreadyRecordedResolvesImmediately(t *testing.T) {
	registry := NewTurnCompletionRegistry()
	registry.Record(testSignal(nil))
	got := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "default",
		RecordedAfterMs:  0,
		TerminalID:       "term_pi",
	})
	if !got.Confirmed || !got.Received {
		t.Fatalf("result = %+v, want {true,true}", got)
	}
}

func TestStaleSignalIgnoredUntilNewSignalArrives(t *testing.T) {
	var mu sync.Mutex
	current := int64(1000)
	now := func() int64 {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	setNow := func(v int64) {
		mu.Lock()
		defer mu.Unlock()
		current = v
	}
	registry := NewTurnCompletionRegistry(
		WithTurnSetNow(now),
		WithTurnSettimeout(3000),
	)
	registry.Record(testSignal(nil))

	mu.Lock()
	after := current
	mu.Unlock()

	done := make(chan TurnCompletionWaitResult, 1)
	go func() {
		done <- registry.WaitForSignal(WaitForSignalInput{
			HerdrSessionName: "default",
			RecordedAfterMs:  after + 1,
			TerminalID:       "term_pi",
		})
	}()

	select {
	case got := <-done:
		t.Fatalf("wait settled early on a stale signal: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}

	setNow(after + 1)
	registry.Record(testSignal(func(s *SessionScopedSignal) { s.Confirmed = false }))

	select {
	case got := <-done:
		if got.Confirmed || !got.Received {
			t.Fatalf("result = %+v, want {confirmed:false received:true}", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not resolve after a new signal arrived")
	}
}

func TestResolvesWhenSignalArrivesWhileWaiting(t *testing.T) {
	registry := NewTurnCompletionRegistry(WithTurnSettimeout(3000))
	done := make(chan TurnCompletionWaitResult, 1)
	go func() {
		done <- registry.WaitForSignal(WaitForSignalInput{
			HerdrSessionName: "default",
			RecordedAfterMs:  time.Now().UnixMilli() - 1,
			TerminalID:       "term_pi",
		})
	}()

	registry.Record(testSignal(func(s *SessionScopedSignal) { s.Confirmed = false }))

	select {
	case got := <-done:
		if got.Confirmed || !got.Received {
			t.Fatalf("result = %+v, want {confirmed:false received:true}", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not resolve after record")
	}
}

func TestTimesOutWithReceivedFalse(t *testing.T) {
	registry := NewTurnCompletionRegistry(WithTurnSettimeout(50))
	started := time.Now()
	got := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "default",
		RecordedAfterMs:  time.Now().UnixMilli(),
		TerminalID:       "term_pi",
	})
	if got.Received || got.Confirmed {
		t.Fatalf("result = %+v, want {false,false}", got)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Fatalf("resolved after %v, expected to wait out the timeout", elapsed)
	}
}

func TestScopesSignalsByHerdrSessionAndTerminal(t *testing.T) {
	registry := NewTurnCompletionRegistry(WithTurnSettimeout(20))
	registry.Record(testSignal(nil))

	if got := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "other",
		RecordedAfterMs:  0,
		TerminalID:       "term_pi",
	}); got.Received {
		t.Fatalf("other herdr session must not match: %+v", got)
	}
	if got := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "default",
		RecordedAfterMs:  0,
		TerminalID:       "term_other",
	}); got.Received {
		t.Fatalf("other terminal must not match: %+v", got)
	}
}

func TestPrunesStaleSignalsOnRecord(t *testing.T) {
	base := int64(1_000_000)
	current := base
	registry := NewTurnCompletionRegistry(
		WithTurnSetNow(func() int64 { return current }),
		WithTurnSettimeout(20),
	)
	registry.Record(testSignal(func(s *SessionScopedSignal) { s.TerminalID = "term_old" }))

	current = base + 11*60*1000
	registry.Record(testSignal(func(s *SessionScopedSignal) { s.TerminalID = "term_new" }))

	if got := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "default",
		RecordedAfterMs:  current,
		TerminalID:       "term_old",
	}); got.Received {
		t.Fatalf("pruned signal should not resolve: %+v", got)
	}
	if got := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "default",
		RecordedAfterMs:  0,
		TerminalID:       "term_new",
	}); !got.Confirmed || !got.Received {
		t.Fatalf("fresh signal should resolve: %+v", got)
	}
}

func TestAbortedWaitReturnsImmediately(t *testing.T) {
	registry := NewTurnCompletionRegistry(WithTurnSettimeout(3000))
	started := time.Now()
	got := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "default",
		RecordedAfterMs:  time.Now().UnixMilli(),
		TerminalID:       "term_pi",
		Aborted:          true,
	})
	if got.Received {
		t.Fatalf("aborted wait = %+v, want received:false", got)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("aborted wait blocked for %v", elapsed)
	}
}

func TestExpectedTextCarriedWhenPresent(t *testing.T) {
	registry := NewTurnCompletionRegistry()
	registry.Record(testSignal(func(s *SessionScopedSignal) {
		s.ExpectedText = "final answer"
		s.HasExpectedText = true
	}))
	got := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: "default",
		RecordedAfterMs:  0,
		TerminalID:       "term_pi",
	})
	if got.ExpectedText != "final answer" || !got.HasExpectedText {
		t.Fatalf("result = %+v, want expectedText carried through", got)
	}
}

// TestTurnCompletionLateFinishDoesNotConsumeNextTurnSignal is the deterministic
// regression for the window the earlier version of this test could not pin down:
// round 1's leftover timer (or cancel) path reaching finishLocked *after* round
// 2's signal has already been stored on the same key.
//
// Every step below is synchronous and reads the registry under its own lock, so
// the ordering is fixed by the test rather than by scheduling: round 1 really is
// suspended as a waiter before anything is recorded, and the late finishLocked
// really is invoked with round 2's signal already in place. The clock is
// injected, so the recordedAt comparisons do not depend on wall time either.
func TestTurnCompletionLateFinishDoesNotConsumeNextTurnSignal(t *testing.T) {
	const (
		session  = "default"
		terminal = "term_pi"
	)
	key := terminalTurnKey(session, terminal)

	// A long timeout: nothing in this test is allowed to end a wait by timing
	// out, because a timeout here would be the very race being ruled out.
	current := int64(1_000_000)
	registry := NewTurnCompletionRegistry(
		WithTurnSetNow(func() int64 { return current }),
		WithTurnSettimeout(60_000),
	)

	// 1. Round 1 takes the slow path: no signal is stored yet, so Wait registers
	//    a waiter and suspends. Polling the registry (rather than sleeping) is
	//    what proves the waiter really exists before anything else happens.
	first := make(chan TurnCompletionWaitResult, 1)
	go func() {
		first <- registry.WaitForSignal(WaitForSignalInput{
			HerdrSessionName: session,
			RecordedAfterMs:  1,
			TerminalID:       terminal,
		})
	}()
	w1 := suspendedWaiter(t, registry, key)

	// 2. Round 1's own signal resolves it and removes it from the registry.
	current++
	registry.Record(testSignal(func(s *SessionScopedSignal) {
		s.ExpectedText = "round-1"
		s.HasExpectedText = true
	}))
	select {
	case got := <-first:
		if !got.Received || got.ExpectedText != "round-1" {
			t.Fatalf("first wait = %+v, want it resolved by round 1's signal", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first wait never settled after its signal was recorded")
	}
	if !waiterFinished(registry, w1) {
		t.Fatal("w1.finished = false after its signal resolved it, want true")
	}
	if count := waiterCount(registry, key); count != 0 {
		t.Fatalf("waiters for the key = %d after the first wait ended, want 0", count)
	}

	// 3. Round 2's signal arrives while nobody is waiting, so it is only stored.
	current++
	registry.Record(testSignal(func(s *SessionScopedSignal) {
		s.ExpectedText = "round-2"
		s.HasExpectedText = true
	}))

	// 4. Round 1's leftover path finally gets the lock — strictly after round 2's
	//    signal landed — and runs finishLocked on the already-finished waiter.
	//    This is the exact interleaving the timer callback and the cancel
	//    listener can produce, reached here without any sleep or luck.
	registry.mu.Lock()
	registry.finishLocked(key, w1, false)
	registry.mu.Unlock()

	// 5. Round 2's signal must still be there. Checked before waiting so a
	//    regression reports the dropped signal instead of a timeout.
	stored, ok := storedSignal(registry, key)
	if !ok {
		t.Fatal("the late finishLocked dropped round 2's stored signal")
	}
	if stored.ExpectedText != "round-2" {
		t.Fatalf("stored signal = %q, want round-2", stored.ExpectedText)
	}

	// 6. And round 2 must actually receive it: the fast path returns it at once,
	//    so this also proves the late finish did not consume it.
	got := registry.WaitForSignal(WaitForSignalInput{
		HerdrSessionName: session,
		RecordedAfterMs:  1,
		TerminalID:       terminal,
	})
	if !got.Received || got.ExpectedText != "round-2" {
		t.Fatalf("second wait = %+v, want round 2's signal", got)
	}
}

// suspendedWaiter waits until exactly one waiter is registered for key and
// returns it. It reads the registry under its own lock, so the caller can rely
// on the wait really having suspended instead of resolving from a stored signal.
func suspendedWaiter(t *testing.T, registry *TurnCompletionRegistry, key string) *waiter {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		registry.mu.Lock()
		waiters := registry.waitersByTerminal[key]
		var found *waiter
		for pending := range waiters {
			found = pending
		}
		count := len(waiters)
		registry.mu.Unlock()
		if count == 1 && found != nil {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters for %q = %d, want exactly 1 registered waiter", key, count)
		}
		time.Sleep(time.Millisecond)
	}
}

// waiterCount reports how many waiters are registered for key.
func waiterCount(registry *TurnCompletionRegistry, key string) int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.waitersByTerminal[key])
}

// waiterFinished reports the waiter's finished flag, read under the registry lock.
func waiterFinished(registry *TurnCompletionRegistry, pending *waiter) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return pending.finished
}

// storedSignal reports the signal currently stored for key, read under the
// registry lock.
func storedSignal(registry *TurnCompletionRegistry, key string) (TurnCompletionSignal, bool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	signal, ok := registry.latestByTerminal[key]
	return signal, ok
}

// TestFinishedWaiterReleasesListenerGoroutine covers the other half of the same
// invariant: the goroutine watching the caller's signal channel must leave as
// soon as the wait is over by any other means, instead of staying parked on a
// channel nobody will ever write to.
func TestFinishedWaiterReleasesListenerGoroutine(t *testing.T) {
	t.Run("listener goroutine leaves when the wait is already over", func(t *testing.T) {
		registry := NewTurnCompletionRegistry(WithTurnSettimeout(50))
		// Sampled before the wait starts, so the only goroutine this subtest adds
		// is the one watching the signal channel.
		baseline := runtime.NumGoroutine()

		// An open channel nobody writes to: the listener must leave on doneCh
		// instead of staying parked on the signal.
		signal := make(chan struct{})
		got := registry.WaitForSignal(WaitForSignalInput{
			HerdrSessionName: "default",
			RecordedAfterMs:  time.Now().UnixMilli(),
			Signal:           signal,
			TerminalID:       "term_pi",
		})
		if got.Received {
			t.Fatalf("result = %+v, want a plain timeout", got)
		}

		deadline := time.Now().Add(2 * time.Second)
		for {
			if runtime.NumGoroutine() <= baseline {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf(
					"signal listener goroutine did not exit after the wait finished (%d goroutines, baseline %d)",
					runtime.NumGoroutine(), baseline)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}
