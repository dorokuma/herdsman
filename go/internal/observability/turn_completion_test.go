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

// A finished waiter must stay finished: once waiter1 has been resolved and
// removed from the registry, its leftover timeout and cancel paths must not
// resolve it again — a second resolution would consume the signal the next
// round registered on the same key, and would close an already-closed doneCh.
// The goroutine watching the caller's signal channel used to park on it forever
// for the same reason.
func TestFinishedWaiterIgnoresLateSignalAndReleasesListener(t *testing.T) {
	t.Run("a finished waiter's leftover paths do not consume the next round's signal", func(t *testing.T) {
		// The timeout is short, so the leftover timer path is guaranteed to be
		// still pending when the signal resolves the first wait.
		registry := NewTurnCompletionRegistry(WithTurnSettimeout(120))

		abort := make(chan struct{})
		first := make(chan TurnCompletionWaitResult, 1)
		go func() {
			first <- registry.WaitForSignal(WaitForSignalInput{
				HerdrSessionName: "default",
				RecordedAfterMs:  time.Now().UnixMilli() - 1,
				Signal:           abort,
				TerminalID:       "term_pi",
			})
		}()

		// The signal resolves the first wait and removes it from the registry
		// while both its timeout and its cancel path are still outstanding.
		registry.Record(testSignal(nil))
		if got := <-first; !got.Received {
			t.Fatalf("first wait = %+v, want it resolved by the signal", got)
		}

		// The next round registers on the same key before anything leftover from
		// the first one runs.
		second := make(chan TurnCompletionWaitResult, 1)
		go func() {
			second <- registry.WaitForSignal(WaitForSignalInput{
				HerdrSessionName: "default",
				RecordedAfterMs:  time.Now().UnixMilli() - 1,
				TerminalID:       "term_pi",
			})
		}()

		// The next round's own signal, recorded while it is the only waiter.
		registry.Record(testSignal(func(s *SessionScopedSignal) {
			s.ExpectedText = "second round"
			s.HasExpectedText = true
		}))

		// Now fire both leftover paths of the finished waiter: its cancel channel
		// and, well past its budget, its timeout.
		close(abort)
		time.Sleep(300 * time.Millisecond)

		// The second round must still hold its own signal: a finished waiter that
		// stayed finished cannot have taken it.
		select {
		case got := <-second:
			if !got.Received || got.ExpectedText != "second round" {
				t.Fatalf("second wait = %+v, want the second round's signal", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the second round never settled after the first waiter's leftover paths ran")
		}

		// The registry is still usable, which also proves the leftover paths left
		// neither a wedge nor a closed channel behind.
		third := make(chan TurnCompletionWaitResult, 1)
		go func() {
			third <- registry.WaitForSignal(WaitForSignalInput{
				HerdrSessionName: "default",
				RecordedAfterMs:  time.Now().UnixMilli() - 1,
				TerminalID:       "term_pi",
			})
		}()
		registry.Record(testSignal(func(s *SessionScopedSignal) {
			s.ExpectedText = "third round"
			s.HasExpectedText = true
		}))
		select {
		case got := <-third:
			if !got.Received || got.ExpectedText != "third round" {
				t.Fatalf("third wait = %+v, want the third round's signal", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the registry stopped serving waits after the first waiter's leftover paths ran")
		}
	})

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
