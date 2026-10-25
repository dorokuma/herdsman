package observability_test

// The in-flight refresh registry of AgentIndexService: callers that ask for the
// same session in the same mutation epoch must share one refresh, exactly like
// the TS hands every caller the same promise.
//
// The check and the install used to run in two separate lock holds, so two
// goroutines that both found the registry empty each started their own refresh
// and each got its own *RefreshSource back. The watch manager keys its
// publication dedup on that identity, so a duplicate source is a duplicated
// publication, not just a wasted scan.

import (
	"sync"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// TestRefreshHerdrSessionSourceSharesOneRefreshPerEpoch is the regression test:
// concurrent callers of the same epoch get the same *RefreshSource and the
// underlying snapshot is taken exactly once.
func TestRefreshHerdrSessionSourceSharesOneRefreshPerEpoch(t *testing.T) {
	harness := openIdxHarness(t)
	history := newIdxHistory("result")
	holder := &idxSnapshotHolder{}
	holder.set(idxOneAgent("working", 10, "claude", nil))
	service, _ := newIdxService(t, harness, holder, history, nil)

	// A barrier that holds every caller until they are all inside the call, so
	// the registry check really does race.
	const callers = 64
	start := make(chan struct{})
	var waiting sync.WaitGroup
	sources := make([]*observability.RefreshSource, callers)
	waiting.Add(callers)
	for index := 0; index < callers; index++ {
		go func(index int) {
			defer waiting.Done()
			<-start
			sources[index] = service.RefreshHerdrSessionSource(turnSessionInput())
		}(index)
	}
	close(start)
	waiting.Wait()

	for index, source := range sources {
		if source == nil {
			t.Fatalf("caller %d got a nil refresh source", index)
		}
		if source != sources[0] {
			t.Fatalf("caller %d got a different *RefreshSource than caller 0", index)
		}
	}

	// Every caller shares one refresh, so the snapshot behind it is taken once.
	// The history records one call per agent per refresh.
	for _, source := range sources {
		source.Await()
	}
	if got := len(history.recordedCalls()); got != 1 {
		t.Fatalf("history resolutions = %d, want 1 (the refresh was not shared)", got)
	}
}
