package db

import (
	"reflect"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The port of test/integration/agent-orchestrator-scope-store.test.ts.

var defaultScope = observability.AgentScope{HerdrSessionName: "default", WorkspaceID: "wB"}

// openScopeStore mirrors the TS openStore: a migrated database with the two
// Herdr sessions the scope foreign keys need.
func openScopeStore(t *testing.T) *AgentOrchestratorScopeStore {
	t.Helper()
	db := openMigratedTestDB(t)
	sessions := NewHerdrSessionStore(db)
	for _, session := range []struct{ name, dir, socket string }{
		{"default", "/tmp/herdr", "/tmp/herdr.sock"},
		{"other", "/tmp/other-herdr", "/tmp/other-herdr.sock"},
	} {
		if _, err := sessions.UpsertRunning(UpsertRunningHerdrSessionInput{
			Name:       session.name,
			SessionDir: session.dir,
			SocketPath: session.socket,
		}); err != nil {
			t.Fatalf("upsertRunning(%s): %v", session.name, err)
		}
	}
	return NewAgentOrchestratorScopeStore(db)
}

func TestAgentOrchestratorScopeStore(t *testing.T) {
	t.Run("is absent before its first claim", func(t *testing.T) {
		store := openScopeStore(t)
		got, err := store.Get(defaultScope)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got != nil {
			t.Fatalf("get = %+v, want nil", got)
		}
	})

	t.Run("applies the authoritative cursor supplied with every claim", func(t *testing.T) {
		store := openScopeStore(t)

		first, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: defaultScope,
			AckedEventID:              12,
			PaneID:                    "wB:p1",
			TerminalID:                "term_1",
		})
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}
		requireScope(t, &first.Current, observability.AgentOrchestratorState{
			AgentScope:   defaultScope,
			AckedEventID: 12,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wB:p1", TerminalID: "term_1"},
		})
		requireScope(t, &first.Previous, observability.AgentOrchestratorState{
			AgentScope:   defaultScope,
			AckedEventID: 12,
			Owner:        nil,
		})
		if first.Previous.UpdatedAt != first.Current.UpdatedAt {
			t.Fatalf("previous.updatedAt = %q, want %q",
				first.Previous.UpdatedAt, first.Current.UpdatedAt)
		}

		replacement, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: defaultScope,
			AckedEventID:              99,
			PaneID:                    "wB:p2",
			TerminalID:                "term_2",
		})
		if err != nil {
			t.Fatalf("replacement claim: %v", err)
		}
		requireScope(t, &replacement.Previous, observability.AgentOrchestratorState{
			AgentScope:   defaultScope,
			AckedEventID: 12,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wB:p1", TerminalID: "term_1"},
		})
		requireScope(t, &replacement.Current, observability.AgentOrchestratorState{
			AgentScope:   defaultScope,
			AckedEventID: 99,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wB:p2", TerminalID: "term_2"},
		})

		// A claim from the same terminal on a new pane is an owner move, not a
		// contested claim.
		movedPane, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: defaultScope,
			AckedEventID:              101,
			PaneID:                    "wB:p3",
			TerminalID:                "term_2",
		})
		if err != nil {
			t.Fatalf("moved pane claim: %v", err)
		}
		requireScope(t, &movedPane.Current, observability.AgentOrchestratorState{
			AgentScope:   defaultScope,
			AckedEventID: 101,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wB:p3", TerminalID: "term_2"},
		})
	})

	t.Run("releases only the current owner and preserves its cursor", func(t *testing.T) {
		store := openScopeStore(t)
		if _, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: defaultScope,
			AckedEventID:              12,
			PaneID:                    "wB:p1",
			TerminalID:                "term_1",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}

		foreign, err := store.ReleaseIfOwner(ReleaseIfOwnerInput{
			AgentOrchestratorScopeKey: defaultScope,
			TerminalID:                "term_2",
		})
		if err != nil {
			t.Fatalf("releaseIfOwner(term_2): %v", err)
		}
		if foreign.Changed {
			t.Fatal("releaseIfOwner(term_2).changed = true, want false")
		}
		if foreign.Current == nil {
			t.Fatal("releaseIfOwner(term_2).current = nil, want the unchanged state")
		}
		requireScope(t, foreign.Current, observability.AgentOrchestratorState{
			AgentScope:   defaultScope,
			AckedEventID: 12,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wB:p1", TerminalID: "term_1"},
		})

		owned, err := store.ReleaseIfOwner(ReleaseIfOwnerInput{
			AgentOrchestratorScopeKey: defaultScope,
			TerminalID:                "term_1",
		})
		if err != nil {
			t.Fatalf("releaseIfOwner(term_1): %v", err)
		}
		if !owned.Changed {
			t.Fatal("releaseIfOwner(term_1).changed = false, want true")
		}
		if owned.Current == nil {
			t.Fatal("releaseIfOwner(term_1).current = nil, want the released state")
		}
		requireScope(t, owned.Current, observability.AgentOrchestratorState{
			AgentScope:   defaultScope,
			AckedEventID: 12,
			Owner:        nil,
		})
		if owned.Previous == nil {
			t.Fatal("releaseIfOwner(term_1).previous = nil, want the pre-release state")
		}
		requireScope(t, owned.Previous, observability.AgentOrchestratorState{
			AgentScope:   defaultScope,
			AckedEventID: 12,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wB:p1", TerminalID: "term_1"},
		})
	})

	t.Run("acknowledges monotonically and only for the current owner", func(t *testing.T) {
		store := openScopeStore(t)
		if _, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: defaultScope,
			AckedEventID:              12,
			PaneID:                    "wB:p1",
			TerminalID:                "term_1",
		}); err != nil {
			t.Fatalf("claim: %v", err)
		}

		acked, err := store.Ack(AckInput{
			AgentOrchestratorScopeKey: defaultScope,
			EventID:                   20,
			TerminalID:                "term_1",
		})
		if err != nil {
			t.Fatalf("ack(20): %v", err)
		}
		if acked.AckedEventID != 20 {
			t.Fatalf("ack(20).ackedEventId = %d, want 20", acked.AckedEventID)
		}
		acked, err = store.Ack(AckInput{
			AgentOrchestratorScopeKey: defaultScope,
			EventID:                   18,
			TerminalID:                "term_1",
		})
		if err != nil {
			t.Fatalf("ack(18): %v", err)
		}
		if acked.AckedEventID != 20 {
			t.Fatalf("ack(18).ackedEventId = %d, want 20 (monotonic)", acked.AckedEventID)
		}
		_, err = store.Ack(AckInput{
			AgentOrchestratorScopeKey: defaultScope,
			EventID:                   21,
			TerminalID:                "term_2",
		})
		if err == nil ||
			err.Error() != "Only the current orchestrator can acknowledge notifications" {
			t.Fatalf("ack from a foreign terminal: err = %v, want the owner-only error", err)
		}
	})

	t.Run("moves ownership atomically with the authoritative target cursor", func(t *testing.T) {
		store := openScopeStore(t)
		if _, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: defaultScope,
			AckedEventID:              12,
			PaneID:                    "wB:p1",
			TerminalID:                "term_1",
		}); err != nil {
			t.Fatalf("claim source: %v", err)
		}
		if _, err := store.Ack(AckInput{
			AgentOrchestratorScopeKey: defaultScope,
			EventID:                   20,
			TerminalID:                "term_1",
		}); err != nil {
			t.Fatalf("ack source: %v", err)
		}
		target := observability.AgentScope{HerdrSessionName: "default", WorkspaceID: "wC"}
		if _, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: target,
			AckedEventID:              5,
			PaneID:                    "wC:p2",
			TerminalID:                "term_2",
		}); err != nil {
			t.Fatalf("claim target: %v", err)
		}

		changes, err := store.MoveOwner(MoveOwnerInput{
			From:               defaultScope,
			PaneID:             "wC:p3",
			TargetAckedEventID: 99,
			TerminalID:         "term_1",
			To:                 target,
		})
		if err != nil {
			t.Fatalf("moveOwner: %v", err)
		}
		if len(changes) != 2 {
			t.Fatalf("moveOwner changes = %d, want 2", len(changes))
		}
		requireScope(t, &changes[0].Current, observability.AgentOrchestratorState{
			AgentScope:   defaultScope,
			AckedEventID: 20,
			Owner:        nil,
		})
		requireScope(t, &changes[1].Current, observability.AgentOrchestratorState{
			AgentScope:   target,
			AckedEventID: 99,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wC:p3", TerminalID: "term_1"},
		})
		requireScope(t, &changes[1].Previous, observability.AgentOrchestratorState{
			AgentScope:   target,
			AckedEventID: 5,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wC:p2", TerminalID: "term_2"},
		})

		newTarget := observability.AgentScope{HerdrSessionName: "default", WorkspaceID: "wD"}
		if _, err := store.MoveOwner(MoveOwnerInput{
			From:               target,
			PaneID:             "wD:p1",
			TargetAckedEventID: 31,
			TerminalID:         "term_1",
			To:                 newTarget,
		}); err != nil {
			t.Fatalf("moveOwner again: %v", err)
		}
		moved, err := store.Get(newTarget)
		if err != nil {
			t.Fatalf("get new target: %v", err)
		}
		requireScope(t, moved, observability.AgentOrchestratorState{
			AgentScope:   newTarget,
			AckedEventID: 31,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wD:p1", TerminalID: "term_1"},
		})
	})

	t.Run("purges only released scopes older than the given age (TTL)", func(t *testing.T) {
		store := openScopeStore(t)
		if _, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: defaultScope,
			AckedEventID:              1,
			PaneID:                    "wB:p1",
			TerminalID:                "term_1",
		}); err != nil {
			t.Fatalf("claim stale: %v", err)
		}
		if _, err := store.ReleaseIfOwner(ReleaseIfOwnerInput{
			AgentOrchestratorScopeKey: defaultScope,
			TerminalID:                "term_1",
		}); err != nil {
			t.Fatalf("release stale: %v", err)
		}
		recent := observability.AgentScope{HerdrSessionName: "default", WorkspaceID: "wC"}
		if _, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: recent,
			AckedEventID:              1,
			PaneID:                    "wC:p1",
			TerminalID:                "term_2",
		}); err != nil {
			t.Fatalf("claim recent: %v", err)
		}
		if _, err := store.ReleaseIfOwner(ReleaseIfOwnerInput{
			AgentOrchestratorScopeKey: recent,
			TerminalID:                "term_2",
		}); err != nil {
			t.Fatalf("release recent: %v", err)
		}
		active := observability.AgentScope{HerdrSessionName: "default", WorkspaceID: "wD"}
		if _, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: active,
			AckedEventID:              1,
			PaneID:                    "wD:p1",
			TerminalID:                "term_3",
		}); err != nil {
			t.Fatalf("claim active: %v", err)
		}
		if _, err := store.db.Exec(
			"update agent_orchestrator_scopes set updated_at = ? where herdr_session_name = ? and workspace_id = ?",
			time.Now().UnixMilli()-31*24*60*60*1000, defaultScope.HerdrSessionName, defaultScope.WorkspaceID,
		); err != nil {
			t.Fatalf("age the released scope: %v", err)
		}

		purged, err := store.PurgeReleasedOlderThan(30 * 24 * 60 * 60 * 1000)
		if err != nil {
			t.Fatalf("purgeReleasedOlderThan: %v", err)
		}
		if purged != 1 {
			t.Fatalf("purgeReleasedOlderThan(30d) = %d, want 1", purged)
		}
		stale, err := store.Get(defaultScope)
		if err != nil {
			t.Fatalf("get purged scope: %v", err)
		}
		if stale != nil {
			t.Fatalf("purged scope = %+v, want nil", stale)
		}
		keptRecent, err := store.Get(recent)
		if err != nil {
			t.Fatalf("get recent scope: %v", err)
		}
		requireScope(t, keptRecent, observability.AgentOrchestratorState{
			AgentScope:   recent,
			AckedEventID: 1,
			Owner:        nil,
		})
		keptActive, err := store.Get(active)
		if err != nil {
			t.Fatalf("get active scope: %v", err)
		}
		requireScope(t, keptActive, observability.AgentOrchestratorState{
			AgentScope:   active,
			AckedEventID: 1,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wD:p1", TerminalID: "term_3"},
		})
	})

	t.Run("lists only owned scopes for the requested Herdr session", func(t *testing.T) {
		store := openScopeStore(t)
		if _, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: defaultScope,
			AckedEventID:              1,
			PaneID:                    "wB:p1",
			TerminalID:                "term_1",
		}); err != nil {
			t.Fatalf("claim default/wB: %v", err)
		}
		if _, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: observability.AgentScope{
				HerdrSessionName: "default",
				WorkspaceID:      "wC",
			},
			AckedEventID: 1,
			PaneID:       "wC:p1",
			TerminalID:   "term_2",
		}); err != nil {
			t.Fatalf("claim default/wC: %v", err)
		}
		if _, err := store.ReleaseIfOwner(ReleaseIfOwnerInput{
			AgentOrchestratorScopeKey: observability.AgentScope{
				HerdrSessionName: "default",
				WorkspaceID:      "wC",
			},
			TerminalID: "term_2",
		}); err != nil {
			t.Fatalf("release default/wC: %v", err)
		}
		if _, err := store.Claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: observability.AgentScope{
				HerdrSessionName: "other",
				WorkspaceID:      "wB",
			},
			AckedEventID: 1,
			PaneID:       "wB:p1",
			TerminalID:   "term_3",
		}); err != nil {
			t.Fatalf("claim other/wB: %v", err)
		}

		owned, err := store.ListOwnedForSession("default")
		if err != nil {
			t.Fatalf("listOwnedForSession: %v", err)
		}
		if len(owned) != 1 {
			t.Fatalf("listOwnedForSession(default) = %d scopes, want 1", len(owned))
		}
		requireScope(t, &owned[0], observability.AgentOrchestratorState{
			AgentScope:   defaultScope,
			AckedEventID: 1,
			Owner:        &observability.AgentOrchestratorOwner{PaneID: "wB:p1", TerminalID: "term_1"},
		})
	})
}

// requireScope mirrors the TS toMatchObject on a scope state: the cursor, the
// scope key and the owner must match; the timestamp is asserted separately
// where it matters.
func requireScope(t *testing.T, got *observability.AgentOrchestratorState, want observability.AgentOrchestratorState) {
	t.Helper()
	if got == nil {
		t.Fatalf("scope state = nil, want %+v", want)
	}
	if got.AckedEventID != want.AckedEventID {
		t.Fatalf("ackedEventId = %d, want %d", got.AckedEventID, want.AckedEventID)
	}
	if got.HerdrSessionName != want.HerdrSessionName || got.WorkspaceID != want.WorkspaceID {
		t.Fatalf("scope = %s/%s, want %s/%s",
			got.HerdrSessionName, got.WorkspaceID, want.HerdrSessionName, want.WorkspaceID)
	}
	if !reflect.DeepEqual(got.Owner, want.Owner) {
		t.Fatalf("owner = %+v, want %+v", got.Owner, want.Owner)
	}
}
