package observability

import (
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
