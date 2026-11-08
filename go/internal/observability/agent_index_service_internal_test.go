package observability

// Package-internal regression tests for the pending-ref sequence CAS.
//
// registerPiSessionRefNow runs its store call (SetSessionRefByTerminal) outside
// s.mu, so a newer registration that lands during that window must survive the
// older call's tail: the claude branch may not overwrite it and the consumed
// non-claude branch may not delete it. Reproducing that interleaving needs a
// pendingPiSessionRefs write to land inside the older call's lock-free store
// window, which the serialized session-operation queue cannot express from the
// public API — and internal/db cannot be imported here (db already imports
// this package) — so the agent store is faked and the "concurrent" newer
// registration is injected synchronously by the store stub, which is the same
// ordering the race would produce.

import (
	"testing"
)

// internalFakeAgentStore is the minimal AgentIndexAgentStoreLike the
// registerPiSessionRefNow path needs: FindByTerminal answers a fixed row and
// SetSessionRefByTerminal runs an injection hook before answering, modelling
// the lock-free window a newer pending registration can land in.
type internalFakeAgentStore struct {
	previous        *AgentIndexRecord
	setResult       *AgentIndexRecord
	onSetSessionRef func()
}

func (s *internalFakeAgentStore) Get(id string) (AgentIndexRecord, error) {
	return AgentIndexRecord{}, nil
}

func (s *internalFakeAgentStore) List(scope AgentQueryScope) ([]AgentIndexRecord, error) {
	return nil, nil
}

func (s *internalFakeAgentStore) ClearLegacyPaneTombstone(input ClearLegacyPaneTombstoneInput) error {
	return nil
}

func (s *internalFakeAgentStore) Exists(id string) (bool, error) {
	return false, nil
}

func (s *internalFakeAgentStore) FindByPane(input FindAgentByPaneInput) (*AgentIndexRecord, error) {
	return nil, nil
}

func (s *internalFakeAgentStore) FindByTerminal(input FindAgentByTerminalInput) (*AgentIndexRecord, error) {
	return s.previous, nil
}

func (s *internalFakeAgentStore) HasGenerationScopedTombstone(
	input HasGenerationScopedTombstoneInput,
) (bool, error) {
	return false, nil
}

func (s *internalFakeAgentStore) IsPaneClosed(input IsPaneClosedInput) (bool, error) {
	return false, nil
}

func (s *internalFakeAgentStore) ListForHerdrSession(
	herdrSessionName string,
) ([]AgentIndexRecord, error) {
	return nil, nil
}

func (s *internalFakeAgentStore) ReplaceForSession(
	input ReplaceAgentsForSessionInput,
) ([]AgentIndexRecord, error) {
	return nil, nil
}

func (s *internalFakeAgentStore) RetirePane(input RetirePaneInput) ([]AgentIndexRecord, error) {
	return nil, nil
}

func (s *internalFakeAgentStore) SetSessionRefByTerminal(
	input SetSessionRefByTerminalInput,
) (*AgentIndexRecord, error) {
	if s.onSetSessionRef != nil {
		s.onSetSessionRef()
	}
	return s.setResult, nil
}

func (s *internalFakeAgentStore) UpdateStatus(input UpdateAgentStatusInput) (*AgentIndexRecord, error) {
	return nil, nil
}

func newInternalIndexService(t *testing.T, store *internalFakeAgentStore) *AgentIndexService {
	t.Helper()
	service, err := NewAgentIndexService(AgentIndexServiceOptions{
		// A non-nil (zero) context service keeps the constructor from building
		// one; registerPiSessionRefNow only reaches it when a mounted write
		// changed the session, which these scenarios never do.
		Context: &AgentContextService{},
		Stores:  AgentIndexServiceStores{Agents: store},
	})
	if err != nil {
		t.Fatalf("newAgentIndexService: %v", err)
	}
	return service
}

// The consumed non-claude branch of registerPiSessionRefNow must not delete a
// pending registration newer than its own: the newer Seq wins the CAS and the
// entry stays. The older entry it is meant to clear is still deleted.
func TestRegisterPiSessionRefKeepsNewerPendingRef(t *testing.T) {
	terminalID := "term_race"
	key := terminalSessionKey("default", terminalID)
	piAgent := "pi"

	// A claude registration that "arrives" while the older pi registration is
	// inside the lock-free SetSessionRefByTerminal window. It stashes pending
	// because the terminal is still labelled pi.
	claudeRef := AgentSessionRef{
		Agent:  "claude",
		Kind:   SessionRefID,
		Source: "dispatch-agent:claude",
		Value:  "newer-claude-session",
	}
	piRef := AgentSessionRef{
		Agent:  "pi",
		Kind:   SessionRefID,
		Source: "dispatch-agent:pi",
		Value:  "pi-session",
	}

	t.Run("newer pending registration survives the older call's tail", func(t *testing.T) {
		store := &internalFakeAgentStore{
			previous: &AgentIndexRecord{Agent: &piAgent, TerminalID: &terminalID},
		}
		service := newInternalIndexService(t, store)
		var injected error
		store.onSetSessionRef = func() {
			// The public register path is serialized on the session operation
			// tail, which the outer call holds right now, so the concurrent
			// registration is driven through registerPiSessionRefNow: same
			// write, same lock-free-window ordering.
			_, injected = service.registerPiSessionRefNow(RegisterPiSessionRefInput{
				HerdrSessionName: "default",
				SessionRef:       claudeRef,
				TerminalID:       terminalID,
			})
		}
		if _, err := service.RegisterPiSessionRef(RegisterPiSessionRefInput{
			HerdrSessionName: "default",
			SessionRef:       piRef,
			TerminalID:       terminalID,
		}); err != nil {
			t.Fatalf("registerPiSessionRef(pi): %v", err)
		}
		if injected != nil {
			t.Fatalf("concurrent registerPiSessionRefNow(claude): %v", injected)
		}

		service.mu.Lock()
		pending, ok := service.pendingPiSessionRefs[key]
		service.mu.Unlock()
		if !ok {
			t.Fatal("pendingPiSessionRefs entry missing: the older pi tail deleted the newer registration")
		}
		if pending.Ref.Agent != claudeRef.Agent || pending.Ref.Value != claudeRef.Value {
			t.Fatalf("pending ref = %+v, want the newer claude registration", pending.Ref)
		}
	})

	t.Run("an older leftover pending entry is still cleared", func(t *testing.T) {
		store := &internalFakeAgentStore{
			previous: &AgentIndexRecord{Agent: &piAgent, TerminalID: &terminalID},
		}
		service := newInternalIndexService(t, store)
		// A stale pending entry from an earlier registration: Seq 0 predates
		// every allocated sequence, so the consumed pi ref must clear it.
		service.mu.Lock()
		service.pendingPiSessionRefs[key] = pendingSessionRef{
			Ref:            claudeRef,
			Seq:            0,
			RegisteredAtMs: 0,
			SeenInSnapshot: true,
		}
		service.mu.Unlock()

		if _, err := service.RegisterPiSessionRef(RegisterPiSessionRefInput{
			HerdrSessionName: "default",
			SessionRef:       piRef,
			TerminalID:       terminalID,
		}); err != nil {
			t.Fatalf("registerPiSessionRef(pi): %v", err)
		}

		service.mu.Lock()
		pending, ok := service.pendingPiSessionRefs[key]
		service.mu.Unlock()
		if ok {
			t.Fatalf("pendingPiSessionRefs entry = %+v, want the stale leftover cleared", pending)
		}
	})
}
