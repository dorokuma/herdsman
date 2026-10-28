package db

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The port of test/integration/agent-store-terminal-identity.test.ts, plus the
// pane-tombstone and target-resolution behaviour of AgentStore that the same
// harness exercises indirectly.

// openAgentHarness mirrors openObservabilityDbHarness: a migrated database with
// the running "default" Herdr session and the three stores wired together.
func openAgentHarness(t *testing.T) (*AgentStore, *AgentEventStore, *AgentContextSnapshotStore, *sql.DB) {
	t.Helper()
	db := openMigratedTestDB(t)
	sessions := NewHerdrSessionStore(db)
	if _, err := sessions.UpsertRunning(UpsertRunningHerdrSessionInput{
		Name:       "default",
		SessionDir: "/tmp/herdr",
		SocketPath: "/tmp/herdr.sock",
	}); err != nil {
		t.Fatalf("upsertRunning: %v", err)
	}
	events, err := NewAgentEventStore(db)
	if err != nil {
		t.Fatalf("newAgentEventStore: %v", err)
	}
	return NewAgentStore(db, events), events, NewAgentContextSnapshotStore(db), db
}

var (
	piSessionRef = observability.AgentSessionRef{
		Agent:  "pi",
		Kind:   observability.SessionRefPath,
		Source: "herdr:pi",
		Value:  "/tmp/pi-session.jsonl",
	}
	claudeSessionRef = observability.AgentSessionRef{
		Agent:  "claude",
		Kind:   observability.SessionRefID,
		Source: "herdr:claude",
		Value:  "session-1",
	}
	pigSessionRef = observability.AgentSessionRef{
		Agent:  "pig",
		Kind:   observability.SessionRefPath,
		Source: "herdr:pig",
		Value:  "/root/.pig/agent/sessions/ses-1.jsonl",
	}
)

// piAgentInput mirrors the TS replacePiAgent input. HasName distinguishes an
// absent `name` field from an explicit null one, exactly like Object.hasOwn.
type piAgentInput struct {
	Agent        *string
	AgentSession any
	HasName      bool
	Name         *string
	PaneID       *string
	Revision     *int
	TerminalID   *string
}

// replacePiAgent mirrors the TS helper: one agent snapshot for the "default"
// Herdr session, returning the first record of the replacement.
func replacePiAgent(
	t *testing.T,
	agents *AgentStore,
	input piAgentInput,
) *observability.AgentIndexRecord {
	t.Helper()
	agent := "pi"
	if input.Agent != nil {
		agent = *input.Agent
	}
	paneID := "wA:p1"
	if input.PaneID != nil {
		paneID = *input.PaneID
	}
	terminalID := "term_1"
	if input.TerminalID != nil {
		terminalID = *input.TerminalID
	}
	snapshot := HerdrAgentLike{
		"agent":        agent,
		"agent_status": "working",
		"pane_id":      paneID,
		"revision":     revisionValue(input.Revision),
		"terminal_id":  terminalID,
		"workspace_id": "wA",
	}
	snapshot["agent_session"] = input.AgentSession
	if input.HasName {
		if input.Name == nil {
			snapshot["name"] = nil
		} else {
			snapshot["name"] = *input.Name
		}
	}
	records, err := agents.ReplaceForSession(ReplaceAgentsForSessionInput{
		Agents:           []HerdrAgentLike{snapshot},
		HerdrSessionName: "default",
	})
	if err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	if len(records) == 0 {
		return nil
	}
	first := records[0]
	return &first
}

// revisionValue renders a reported pane revision the way the JSON payload does:
// a number, or an absent field.
func revisionValue(revision *int) any {
	if revision == nil {
		return nil
	}
	return float64(*revision)
}

func TestAgentStoreTerminalIdentity(t *testing.T) {
	t.Run("keeps an agent id when its stable terminal moves to a new pane and workspace", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		if _, err := agents.ReplaceForSession(ReplaceAgentsForSessionInput{
			Agents: []HerdrAgentLike{{
				"agent":        "pi",
				"agent_status": "working",
				"pane_id":      "wA:p1",
				"terminal_id":  "term_1",
				"workspace_id": "wA",
			}},
			HerdrSessionName: "default",
		}); err != nil {
			t.Fatalf("first replaceForSession: %v", err)
		}
		original, err := agents.FindByPane(FindAgentByPaneInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("findByPane: %v", err)
		}
		if original == nil {
			t.Fatalf("the initial agent is missing")
		}
		originalID := original.ID

		if _, err := agents.ReplaceForSession(ReplaceAgentsForSessionInput{
			Agents: []HerdrAgentLike{{
				"agent":        "pi",
				"agent_status": "working",
				"pane_id":      "wB:p3",
				"terminal_id":  "term_1",
				"workspace_id": "wB",
			}},
			HerdrSessionName: "default",
		}); err != nil {
			t.Fatalf("second replaceForSession: %v", err)
		}

		moved, err := agents.FindByTerminal(FindAgentByTerminalInput{
			HerdrSessionName: "default",
			TerminalID:       "term_1",
		})
		if err != nil {
			t.Fatalf("findByTerminal: %v", err)
		}
		if moved == nil {
			t.Fatalf("the moved agent is missing")
		}
		if moved.ID != originalID {
			t.Fatalf("id = %q, want %q (the stable terminal keeps its identity)", moved.ID, originalID)
		}
		if moved.PaneID != "wB:p3" || moved.WorkspaceID != "wB" {
			t.Fatalf("paneId/workspaceId = %q/%q, want wB:p3/wB", moved.PaneID, moved.WorkspaceID)
		}
		if moved.TerminalID == nil || *moved.TerminalID != "term_1" {
			t.Fatalf("terminalId = %v, want term_1", moved.TerminalID)
		}
		left, err := agents.FindByPane(FindAgentByPaneInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("findByPane: %v", err)
		}
		if left != nil {
			t.Fatalf("the old pane still has an agent: %#v", left)
		}
	})

	t.Run("maps pane revisions and preserves reported and Pi session refs by priority", func(t *testing.T) {
		agents, _, _, db := openAgentHarness(t)
		initial := replacePiAgent(t, agents, piAgentInput{Revision: intPtr(41)})
		if initial == nil {
			t.Fatalf("the initial agent is missing")
		}
		if initial.PaneRevision == nil || *initial.PaneRevision != 41 {
			t.Fatalf("paneRevision = %v, want 41", initial.PaneRevision)
		}
		if revision := replacePiAgent(t, agents, piAgentInput{}); revision == nil ||
			revision.PaneRevision != nil {
			t.Fatalf("paneRevision = %v, want nil", revision)
		}

		hinted, err := agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
			AgentSession:     piSessionRef,
			HerdrSessionName: "default",
			TerminalID:       "term_1",
		})
		if err != nil {
			t.Fatalf("setSessionRefByTerminal: %v", err)
		}
		if hinted == nil {
			t.Fatalf("setSessionRefByTerminal returned no agent")
		}
		if !reflect.DeepEqual(hinted.AgentSession, &piSessionRef) {
			t.Fatalf("agentSession = %#v, want %#v", hinted.AgentSession, &piSessionRef)
		}
		if hinted.PaneRevision != nil {
			t.Fatalf("paneRevision = %v, want nil", hinted.PaneRevision)
		}

		var reportedJSON, hintJSON sql.NullString
		if err := db.QueryRow(
			"select agent_session_json, agent_session_hint_json from agents where id = ?",
			initial.ID,
		).Scan(&reportedJSON, &hintJSON); err != nil {
			t.Fatalf("read the session columns: %v", err)
		}
		if reportedJSON.Valid {
			t.Fatalf("agent_session_json = %q, want NULL", reportedJSON.String)
		}
		wantHint := mustJSON(t, piSessionRef)
		if hintJSON.String != wantHint {
			t.Fatalf("agent_session_hint_json = %q, want %q", hintJSON.String, wantHint)
		}

		if got := replacePiAgent(t, agents, piAgentInput{Revision: intPtr(42)}); got == nil ||
			!reflect.DeepEqual(got.AgentSession, &piSessionRef) {
			t.Fatalf("agentSession = %#v, want the preserved Pi hint", got)
		}
		if got := replacePiAgent(t, agents, piAgentInput{
			AgentSession: claudeSessionRef,
			Revision:     intPtr(43),
		}); got == nil || !reflect.DeepEqual(got.AgentSession, &claudeSessionRef) {
			t.Fatalf("agentSession = %#v, want the reported Claude ref", got)
		}
		if err := db.QueryRow(
			"select agent_session_hint_json from agents where id = ?", initial.ID,
		).Scan(&hintJSON); err != nil {
			t.Fatalf("read the hint column: %v", err)
		}
		if hintJSON.String != wantHint {
			t.Fatalf("agent_session_hint_json = %q, want %q (the hint survives a same-kind agent)",
				hintJSON.String, wantHint)
		}
		if got := replacePiAgent(t, agents, piAgentInput{Revision: intPtr(44)}); got == nil ||
			!reflect.DeepEqual(got.AgentSession, &piSessionRef) {
			t.Fatalf("agentSession = %#v, want the preserved Pi hint", got)
		}
	})

	t.Run("accepts a pi-family hint across the pi and pig boundary", func(t *testing.T) {
		// pig runs the same herdsman-pi extension, so a pi-family hint stays
		// valid when the terminal is indexed under the other family member.
		agents, _, _, _ := openAgentHarness(t)
		pig := replacePiAgent(t, agents, piAgentInput{Agent: stringPtr("pig")})
		if pig == nil {
			t.Fatalf("the pig agent is missing")
		}
		got, err := agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
			AgentSession:     piSessionRef,
			HerdrSessionName: "default",
			TerminalID:       "term_1",
		})
		if err != nil {
			t.Fatalf("setSessionRefByTerminal: %v", err)
		}
		if got == nil || got.Agent == nil || *got.Agent != "pig" ||
			!reflect.DeepEqual(got.AgentSession, &piSessionRef) {
			t.Fatalf("agentSession = %#v, want the pi hint on the pig terminal", got)
		}

		pi := replacePiAgent(t, agents, piAgentInput{Agent: stringPtr("pi")})
		if pi == nil {
			t.Fatalf("the pi agent is missing")
		}
		got, err = agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
			AgentSession:     pigSessionRef,
			HerdrSessionName: "default",
			TerminalID:       "term_1",
		})
		if err != nil {
			t.Fatalf("setSessionRefByTerminal: %v", err)
		}
		if got == nil || got.Agent == nil || *got.Agent != "pi" ||
			!reflect.DeepEqual(got.AgentSession, &pigSessionRef) {
			t.Fatalf("agentSession = %#v, want the pig hint on the pi terminal", got)
		}
	})

	t.Run("rejects a Pi session hint for a terminal indexed as another agent", func(t *testing.T) {
		agents, _, _, db := openAgentHarness(t)
		claude := replacePiAgent(t, agents, piAgentInput{Agent: stringPtr("claude")})
		if claude == nil {
			t.Fatalf("the Claude agent is missing")
		}

		got, err := agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
			AgentSession:     piSessionRef,
			HerdrSessionName: "default",
			TerminalID:       "term_1",
		})
		if err != nil {
			t.Fatalf("setSessionRefByTerminal: %v", err)
		}
		if got == nil {
			t.Fatalf("setSessionRefByTerminal returned no agent")
		}
		if got.Agent == nil || *got.Agent != "claude" {
			t.Fatalf("agent = %v, want claude", got.Agent)
		}
		if got.AgentSession != nil {
			t.Fatalf("agentSession = %#v, want nil", got.AgentSession)
		}
		var hintJSON sql.NullString
		if err := db.QueryRow(
			"select agent_session_hint_json from agents where id = ?", claude.ID,
		).Scan(&hintJSON); err != nil {
			t.Fatalf("read the hint column: %v", err)
		}
		if hintJSON.Valid {
			t.Fatalf("agent_session_hint_json = %q, want NULL", hintJSON.String)
		}
	})

	t.Run("clears a Pi hint when the stable terminal changes agent", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		replacePiAgent(t, agents, piAgentInput{})
		if _, err := agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
			AgentSession:     piSessionRef,
			HerdrSessionName: "default",
			TerminalID:       "term_1",
		}); err != nil {
			t.Fatalf("setSessionRefByTerminal: %v", err)
		}

		got := replacePiAgent(t, agents, piAgentInput{
			Agent:    stringPtr("claude"),
			Revision: intPtr(45),
		})
		if got == nil {
			t.Fatalf("the replacement is missing")
		}
		if got.Agent == nil || *got.Agent != "claude" {
			t.Fatalf("agent = %v, want claude", got.Agent)
		}
		if got.AgentSession != nil {
			t.Fatalf("agentSession = %#v, want nil", got.AgentSession)
		}
		if got.PaneRevision == nil || *got.PaneRevision != 45 {
			t.Fatalf("paneRevision = %v, want 45", got.PaneRevision)
		}
	})

	t.Run("preserves identity and refs when a terminal moves panes", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		initial := replacePiAgent(t, agents, piAgentInput{
			AgentSession: piSessionRef,
			Revision:     intPtr(41),
		})
		if initial == nil {
			t.Fatalf("the initial agent is missing")
		}
		if _, err := agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
			AgentSession:     piSessionRef,
			HerdrSessionName: "default",
			TerminalID:       "term_1",
		}); err != nil {
			t.Fatalf("setSessionRefByTerminal: %v", err)
		}

		got := replacePiAgent(t, agents, piAgentInput{
			AgentSession: piSessionRef,
			PaneID:       stringPtr("wB:p3"),
			Revision:     intPtr(42),
		})
		if got == nil {
			t.Fatalf("the moved agent is missing")
		}
		if got.ID != initial.ID {
			t.Fatalf("id = %q, want %q", got.ID, initial.ID)
		}
		if !reflect.DeepEqual(got.AgentSession, &piSessionRef) {
			t.Fatalf("agentSession = %#v, want %#v", got.AgentSession, &piSessionRef)
		}
		if got.PaneID != "wB:p3" {
			t.Fatalf("paneId = %q, want wB:p3", got.PaneID)
		}
		if got.PaneRevision == nil || *got.PaneRevision != 42 {
			t.Fatalf("paneRevision = %v, want 42", got.PaneRevision)
		}
		if got.TerminalID == nil || *got.TerminalID != "term_1" {
			t.Fatalf("terminalId = %v, want term_1", got.TerminalID)
		}
	})

	t.Run("does not reuse a pane occupant's identity, refs, hint, or snapshot", func(t *testing.T) {
		agents, _, snapshots, _ := openAgentHarness(t)
		initial := replacePiAgent(t, agents, piAgentInput{
			AgentSession: piSessionRef,
			Revision:     intPtr(41),
		})
		if initial == nil {
			t.Fatalf("the initial agent is missing")
		}
		if _, err := agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
			AgentSession:     piSessionRef,
			HerdrSessionName: "default",
			TerminalID:       "term_1",
		}); err != nil {
			t.Fatalf("setSessionRefByTerminal: %v", err)
		}
		if _, err := snapshots.Put(AgentContextSnapshotInput{
			AgentID: initial.ID,
			CompactHistory: observability.CompactAgentHistory{
				HistoryRef:   nil,
				MessageCount: 0,
				Source:       nil,
				UpdatedAt:    nil,
			},
			HistoryRef:        nil,
			PaneRevision:      intPtr(41),
			SourceFingerprint: nil,
		}); err != nil {
			t.Fatalf("put the context snapshot: %v", err)
		}

		replacement := replacePiAgent(t, agents, piAgentInput{
			Agent:      stringPtr("claude"),
			PaneID:     stringPtr("wA:p1"),
			Revision:   intPtr(1),
			TerminalID: stringPtr("term_2"),
		})
		if replacement == nil {
			t.Fatalf("the replacement is missing")
		}
		if replacement.Agent == nil || *replacement.Agent != "claude" {
			t.Fatalf("agent = %v, want claude", replacement.Agent)
		}
		if replacement.AgentSession != nil {
			t.Fatalf("agentSession = %#v, want nil", replacement.AgentSession)
		}
		if replacement.PaneRevision == nil || *replacement.PaneRevision != 1 {
			t.Fatalf("paneRevision = %v, want 1", replacement.PaneRevision)
		}
		if replacement.TerminalID == nil || *replacement.TerminalID != "term_2" {
			t.Fatalf("terminalId = %v, want term_2", replacement.TerminalID)
		}
		if replacement.ID == initial.ID {
			t.Fatalf("the replacement reused id %q", initial.ID)
		}
		stored, err := snapshots.Get(initial.ID)
		if err != nil {
			t.Fatalf("get the context snapshot: %v", err)
		}
		if stored != nil {
			t.Fatalf("the retired agent still has a context snapshot: %#v", stored)
		}
	})

	t.Run("keeps identity and refs when a live name changes or clears", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		initial := replacePiAgent(t, agents, piAgentInput{
			AgentSession: piSessionRef,
			HasName:      true,
			Name:         stringPtr("reviewer"),
			Revision:     intPtr(41),
		})
		if initial == nil {
			t.Fatalf("the initial agent is missing")
		}
		if _, err := agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
			AgentSession:     piSessionRef,
			HerdrSessionName: "default",
			TerminalID:       "term_1",
		}); err != nil {
			t.Fatalf("setSessionRefByTerminal: %v", err)
		}

		renamed := replacePiAgent(t, agents, piAgentInput{
			HasName:  true,
			Name:     stringPtr("implementer"),
			Revision: intPtr(42),
		})
		if renamed == nil {
			t.Fatalf("the renamed agent is missing")
		}
		if renamed.ID != initial.ID {
			t.Fatalf("id = %q, want %q", renamed.ID, initial.ID)
		}
		if renamed.Name == nil || *renamed.Name != "implementer" {
			t.Fatalf("name = %v, want implementer", renamed.Name)
		}
		if !reflect.DeepEqual(renamed.AgentSession, &piSessionRef) {
			t.Fatalf("agentSession = %#v, want %#v", renamed.AgentSession, &piSessionRef)
		}
		if renamed.TerminalID == nil || *renamed.TerminalID != "term_1" {
			t.Fatalf("terminalId = %v, want term_1", renamed.TerminalID)
		}

		cleared := replacePiAgent(t, agents, piAgentInput{
			HasName:  true,
			Name:     nil,
			Revision: intPtr(43),
		})
		if cleared == nil {
			t.Fatalf("the cleared agent is missing")
		}
		if cleared.ID != initial.ID {
			t.Fatalf("id = %q, want %q", cleared.ID, initial.ID)
		}
		if cleared.Name != nil {
			t.Fatalf("name = %v, want nil", cleared.Name)
		}
		if !reflect.DeepEqual(cleared.AgentSession, &piSessionRef) {
			t.Fatalf("agentSession = %#v, want %#v", cleared.AgentSession, &piSessionRef)
		}
	})

	t.Run("does not create an agent for an unknown terminal hint", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		got, err := agents.SetSessionRefByTerminal(SetSessionRefByTerminalInput{
			AgentSession:     piSessionRef,
			HerdrSessionName: "default",
			TerminalID:       "unknown",
		})
		if err != nil {
			t.Fatalf("setSessionRefByTerminal: %v", err)
		}
		if got != nil {
			t.Fatalf("setSessionRefByTerminal = %#v, want nil", got)
		}
		listed, err := agents.ListForHerdrSession("default")
		if err != nil {
			t.Fatalf("listForHerdrSession: %v", err)
		}
		if len(listed) != 0 {
			t.Fatalf("listForHerdrSession = %#v, want no agents", listed)
		}
	})

	t.Run("persists terminal identity for new events and maps migrated events as null", func(t *testing.T) {
		_, events, _, _ := openAgentHarness(t)
		record, err := events.Append(AppendAgentEventInput{
			HerdrSessionName: "default",
			Payload:          map[string]any{},
			TerminalID:       stringPtr("term_1"),
			Type:             observability.AgentEventDone,
			WorkspaceID:      stringPtr("wA"),
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if record.TerminalID == nil || *record.TerminalID != "term_1" {
			t.Fatalf("terminalId = %v, want term_1", record.TerminalID)
		}
		fetched, err := events.Get(record.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", record.ID, err)
		}
		if fetched.TerminalID == nil || *fetched.TerminalID != "term_1" {
			t.Fatalf("terminalId = %v, want term_1", fetched.TerminalID)
		}

		migrated, err := events.Append(AppendAgentEventInput{
			HerdrSessionName: "default",
			Payload:          map[string]any{},
			Type:             observability.AgentEventIdle,
			WorkspaceID:      stringPtr("wA"),
		})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if migrated.TerminalID != nil {
			t.Fatalf("terminalId = %v, want nil for the migrated row", migrated.TerminalID)
		}
	})
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %#v: %v", value, err)
	}
	return string(encoded)
}

func TestAgentStorePaneTombstones(t *testing.T) {
	t.Run("records a legacy tombstone that matches any generation", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		if err := agents.MarkPaneClosed(RetirePaneInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
		}); err != nil {
			t.Fatalf("markPaneClosed: %v", err)
		}

		for _, generation := range []*string{nil, stringPtr("gen_1")} {
			closed, err := agents.IsPaneClosed(IsPaneClosedInput{
				HerdrSessionName: "default",
				PaneGeneration:   generation,
				PaneID:           "wA:p1",
			})
			if err != nil {
				t.Fatalf("isPaneClosed(%v): %v", generation, err)
			}
			if !closed {
				t.Fatalf("isPaneClosed(%v) = false, want true (a legacy tombstone matches any generation)",
					generation)
			}
		}
		scoped, err := agents.HasGenerationScopedTombstone(HasGenerationScopedTombstoneInput{
			HerdrSessionName: "default",
			PaneGeneration:   "gen_1",
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("hasGenerationScopedTombstone: %v", err)
		}
		if scoped {
			t.Fatalf("a legacy tombstone must not report as generation-scoped")
		}

		// A live pane that reports a generation clears the legacy tombstone.
		replacePiAgent(t, agents, piAgentInput{
			AgentSession: nil,
			Revision:     intPtr(1),
		})
		snapshot := HerdrAgentLike{
			"agent":           "pi",
			"agent_status":    "working",
			"pane_generation": "gen_1",
			"pane_id":         "wA:p1",
			"terminal_id":     "term_1",
			"workspace_id":    "wA",
		}
		if _, err := agents.ReplaceForSession(ReplaceAgentsForSessionInput{
			Agents:           []HerdrAgentLike{snapshot},
			HerdrSessionName: "default",
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		cleared, err := agents.IsPaneClosed(IsPaneClosedInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_1"),
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("isPaneClosed: %v", err)
		}
		if cleared {
			t.Fatalf("a live generation must beat a generation-less close")
		}
	})

	t.Run("scopes a generation tombstone to its own generation", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		if err := agents.MarkPaneClosed(RetirePaneInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_1"),
			PaneID:           "wA:p1",
		}); err != nil {
			t.Fatalf("markPaneClosed: %v", err)
		}

		closed, err := agents.IsPaneClosed(IsPaneClosedInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_1"),
			PaneID:           "wA:p1",
		})
		if err != nil || !closed {
			t.Fatalf("isPaneClosed(gen_1) = %v/%v, want true/nil", closed, err)
		}
		other, err := agents.IsPaneClosed(IsPaneClosedInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_2"),
			PaneID:           "wA:p1",
		})
		if err != nil || other {
			t.Fatalf("isPaneClosed(gen_2) = %v/%v, want false/nil", other, err)
		}
		scoped, err := agents.HasGenerationScopedTombstone(HasGenerationScopedTombstoneInput{
			HerdrSessionName: "default",
			PaneGeneration:   "gen_1",
			PaneID:           "wA:p1",
		})
		if err != nil || !scoped {
			t.Fatalf("hasGenerationScopedTombstone = %v/%v, want true/nil", scoped, err)
		}

		// Closing another generation replaces only that generation's own
		// tombstone, so the earlier one stays recorded.
		if err := agents.MarkPaneClosed(RetirePaneInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_2"),
			PaneID:           "wA:p1",
		}); err != nil {
			t.Fatalf("markPaneClosed: %v", err)
		}
		stale, err := agents.IsPaneClosed(IsPaneClosedInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_1"),
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("isPaneClosed: %v", err)
		}
		if !stale {
			t.Fatalf("closing gen_2 must not erase the gen_1 tombstone")
		}
		current, err := agents.IsPaneClosed(IsPaneClosedInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_2"),
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("isPaneClosed: %v", err)
		}
		if !current {
			t.Fatalf("the gen_2 tombstone must be recorded")
		}
	})

	t.Run("answers isPaneClosedSince for a lease without a generation", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		leasedAt := time.Now().UnixMilli()
		if err := agents.MarkPaneClosed(RetirePaneInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_1"),
			PaneID:           "wA:p1",
		}); err != nil {
			t.Fatalf("markPaneClosed: %v", err)
		}

		// Without a generation the timestamp decides: only a tombstone recorded
		// at or after sinceMs counts.
		since, err := agents.IsPaneClosedSince(IsPaneClosedInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
			SinceMs:          leasedAt + 60_000,
		})
		if err != nil {
			t.Fatalf("isPaneClosedSince: %v", err)
		}
		if since {
			t.Fatalf("a tombstone older than the lease must not count")
		}
		before, err := agents.IsPaneClosedSince(IsPaneClosedInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
			SinceMs:          leasedAt - 60_000,
		})
		if err != nil {
			t.Fatalf("isPaneClosedSince: %v", err)
		}
		if !before {
			t.Fatalf("a tombstone at or after sinceMs must count")
		}
		// A generation-scoped tombstone counts no matter how old it is, because
		// the query delegates to isPaneClosed and ignores the timestamp.
		scoped, err := agents.IsPaneClosedSince(IsPaneClosedInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_1"),
			PaneID:           "wA:p1",
			SinceMs:          leasedAt + 60_000,
		})
		if err != nil {
			t.Fatalf("isPaneClosedSince: %v", err)
		}
		if !scoped {
			t.Fatalf("a generation-scoped query must ignore sinceMs")
		}

		// A legacy tombstone always counts, whatever the timestamp says.
		if err := agents.MarkPaneClosed(RetirePaneInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p2",
		}); err != nil {
			t.Fatalf("markPaneClosed: %v", err)
		}
		legacy, err := agents.IsPaneClosedSince(IsPaneClosedInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p2",
			SinceMs:          leasedAt + 60_000,
		})
		if err != nil {
			t.Fatalf("isPaneClosedSince: %v", err)
		}
		if !legacy {
			t.Fatalf("a legacy tombstone must count for any lease")
		}
	})

	t.Run("retires a pane, its events and its snapshots", func(t *testing.T) {
		agents, events, snapshots, _ := openAgentHarness(t)
		agent := replacePiAgent(t, agents, piAgentInput{Revision: intPtr(7)})
		if agent == nil {
			t.Fatalf("the initial agent is missing")
		}
		event := appendEvent(t, events, AppendAgentEventInput{
			AgentID:          stringPtr(agent.ID),
			HerdrSessionName: "default",
			Payload:          map[string]any{},
			TerminalID:       stringPtr("term_1"),
			Type:             observability.AgentEventBlocked,
			WorkspaceID:      stringPtr("wA"),
		})
		if _, err := snapshots.Put(AgentContextSnapshotInput{
			AgentID:        agent.ID,
			CompactHistory: observability.CompactAgentHistory{MessageCount: 0},
			PaneRevision:   intPtr(7),
		}); err != nil {
			t.Fatalf("put the context snapshot: %v", err)
		}

		retired, err := agents.RetirePane(RetirePaneInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("retirePane: %v", err)
		}
		if len(retired) != 1 || retired[0].ID != agent.ID {
			t.Fatalf("retired = %#v, want the single agent %q", retired, agent.ID)
		}
		listed, err := agents.ListForHerdrSession("default")
		if err != nil {
			t.Fatalf("listForHerdrSession: %v", err)
		}
		if len(listed) != 0 {
			t.Fatalf("listForHerdrSession = %#v, want no agents", listed)
		}
		stored, err := snapshots.Get(agent.ID)
		if err != nil {
			t.Fatalf("get the context snapshot: %v", err)
		}
		if stored != nil {
			t.Fatalf("the context snapshot survived the retirement: %#v", stored)
		}
		invalidated, err := events.Get(event.ID)
		if err != nil {
			t.Fatalf("get(%d): %v", event.ID, err)
		}
		// retirePane does not invalidate events: the ON DELETE set null foreign
		// key turns the row into an orphan instead.
		if invalidated.Status == nil ||
			*invalidated.Status != observability.AgentEventStatusPending {
			t.Fatalf("event status = %v, want pending", invalidated.Status)
		}
		if invalidated.AgentID != nil {
			t.Fatalf("agentId = %v, want nil (the agents row is gone)", invalidated.AgentID)
		}

		// Retiring again is a no-op that still tombstones the pane.
		again, err := agents.RetirePane(RetirePaneInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("retirePane again: %v", err)
		}
		if len(again) != 0 {
			t.Fatalf("retirePane again = %#v, want no agents", again)
		}
		closed, err := agents.IsPaneClosed(IsPaneClosedInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
		})
		if err != nil || !closed {
			t.Fatalf("isPaneClosed = %v/%v, want true/nil", closed, err)
		}
	})

	t.Run("retires only the named generation", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		older := replacePiAgent(t, agents, piAgentInput{Revision: intPtr(1)})
		if older == nil {
			t.Fatalf("the older agent is missing")
		}
		newer, err := agents.ReplaceForSession(ReplaceAgentsForSessionInput{
			Agents: []HerdrAgentLike{
				{
					"agent":           "pi",
					"agent_status":    "working",
					"pane_generation": "gen_2",
					"pane_id":         "wA:p2",
					"terminal_id":     "term_2",
					"workspace_id":    "wA",
				},
				{
					"agent":        "pi",
					"agent_status": "working",
					"pane_id":      "wA:p1",
					"terminal_id":  "term_1",
					"workspace_id": "wA",
				},
			},
			HerdrSessionName: "default",
		})
		if err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		if len(newer) != 2 {
			t.Fatalf("replaceForSession returned %d agents, want 2", len(newer))
		}

		// A generation that does not match the stored row retires nothing.
		missed, err := agents.RetirePane(RetirePaneInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_9"),
			PaneID:           "wA:p2",
		})
		if err != nil {
			t.Fatalf("retirePane: %v", err)
		}
		if len(missed) != 0 {
			t.Fatalf("retirePane(gen_9) = %#v, want no agents", missed)
		}

		retired, err := agents.RetirePane(RetirePaneInput{
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_2"),
			PaneID:           "wA:p2",
		})
		if err != nil {
			t.Fatalf("retirePane: %v", err)
		}
		if len(retired) != 1 || retired[0].PaneID != "wA:p2" {
			t.Fatalf("retired = %#v, want the wA:p2 agent", retired)
		}
		left, err := agents.FindByPane(FindAgentByPaneInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("findByPane: %v", err)
		}
		if left == nil || left.ID != older.ID {
			t.Fatalf("the wA:p1 agent was retired too: %#v", left)
		}
	})
}

func TestAgentStoreListAndResolveTarget(t *testing.T) {
	t.Run("lists only the running sessions unless all is set", func(t *testing.T) {
		agents, _, _, db := openAgentHarness(t)
		if _, err := agents.ReplaceForSession(ReplaceAgentsForSessionInput{
			Agents: []HerdrAgentLike{{
				"agent":        "pi",
				"agent_status": "working",
				"pane_id":      "wA:p1",
				"terminal_id":  "term_1",
				"workspace_id": "wA",
			}},
			HerdrSessionName: "default",
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		if _, err := db.Exec(
			`insert into herdr_sessions (name, running, session_dir, socket_path, updated_at)
			 values ('stopped', 0, '/tmp/stopped', '/tmp/stopped.sock', ?)`,
			time.Now().UnixMilli(),
		); err != nil {
			t.Fatalf("insert the stopped session: %v", err)
		}
		if _, err := db.Exec(
			`insert into agents
			   (id, herdr_session_name, pane_id, terminal_id, workspace_id, agent, agent_status,
			    first_seen_at, last_seen_at, focused)
			 values ('ag_stopped', 'stopped', 'wZ:p1', 'term_z', 'wZ', 'pi', 'working', ?, ?, 0)`,
			time.Now().UnixMilli(), time.Now().UnixMilli(),
		); err != nil {
			t.Fatalf("insert the stopped agent: %v", err)
		}

		listed, err := agents.List(observability.AgentQueryScope{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(listed) != 1 || listed[0].HerdrSessionName != "default" {
			t.Fatalf("list = %#v, want only the running session", listed)
		}

		all, err := agents.List(observability.AgentQueryScope{All: boolPtr(true)})
		if err != nil {
			t.Fatalf("list(all): %v", err)
		}
		// `all` only lifts the session/workspace narrowing: the running-session
		// join stays, so a stopped session's agent is still invisible.
		if len(all) != 1 || all[0].HerdrSessionName != "default" {
			t.Fatalf("list(all) = %#v, want only the running session", all)
		}

		narrowed, err := agents.List(observability.AgentQueryScope{
			HerdrSessionName: stringPtr("stopped"),
		})
		if err != nil {
			t.Fatalf("list(stopped): %v", err)
		}
		if len(narrowed) != 0 {
			t.Fatalf("list(stopped) = %#v, want none (the session is not running)", narrowed)
		}

		workspaceScoped, err := agents.List(observability.AgentQueryScope{
			WorkspaceID: stringPtr("wZ"),
		})
		if err != nil {
			t.Fatalf("list(wZ): %v", err)
		}
		if len(workspaceScoped) != 0 {
			t.Fatalf("list(wZ) = %#v, want none", workspaceScoped)
		}

		sessionAll, err := agents.List(observability.AgentQueryScope{
			All:              boolPtr(true),
			HerdrSessionName: stringPtr("stopped"),
		})
		if err != nil {
			t.Fatalf("list(all, stopped): %v", err)
		}
		if len(sessionAll) != 0 {
			t.Fatalf("list(all, stopped) = %#v, want none", sessionAll)
		}
	})

	t.Run("resolves a target by pane, terminal, id, name and agent kind", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		if _, err := agents.ReplaceForSession(ReplaceAgentsForSessionInput{
			Agents: []HerdrAgentLike{
				{
					"agent":        "pi",
					"agent_status": "working",
					"name":         "reviewer",
					"pane_id":      "wA:p1",
					"terminal_id":  "term_1",
					"workspace_id": "wA",
				},
				{
					"agent":        "claude",
					"agent_status": "working",
					"pane_id":      "wA:p2",
					"terminal_id":  "term_2",
					"workspace_id": "wA",
				},
			},
			HerdrSessionName: "default",
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		scope := observability.AgentQueryScope{HerdrSessionName: stringPtr("default")}
		byPane, err := agents.ResolveTarget(scope, "wA:p1")
		if err != nil {
			t.Fatalf("resolveTarget(wA:p1): %v", err)
		}
		if byPane.PaneID != "wA:p1" {
			t.Fatalf("resolveTarget(wA:p1) = %#v", byPane)
		}
		byTerminal, err := agents.ResolveTarget(scope, "term_2")
		if err != nil {
			t.Fatalf("resolveTarget(term_2): %v", err)
		}
		if byTerminal.TerminalID == nil || *byTerminal.TerminalID != "term_2" {
			t.Fatalf("resolveTarget(term_2) = %#v", byTerminal)
		}
		byID, err := agents.ResolveTarget(scope, byPane.ID)
		if err != nil {
			t.Fatalf("resolveTarget(id): %v", err)
		}
		if byID.ID != byPane.ID {
			t.Fatalf("resolveTarget(id) = %#v, want %q", byID, byPane.ID)
		}
		byName, err := agents.ResolveTarget(scope, "reviewer")
		if err != nil {
			t.Fatalf("resolveTarget(reviewer): %v", err)
		}
		if byName.Name == nil || *byName.Name != "reviewer" {
			t.Fatalf("resolveTarget(reviewer) = %#v", byName)
		}
		byKind, err := agents.ResolveTarget(scope, "claude")
		if err != nil {
			t.Fatalf("resolveTarget(claude): %v", err)
		}
		if byKind.Agent == nil || *byKind.Agent != "claude" {
			t.Fatalf("resolveTarget(claude) = %#v", byKind)
		}

		if _, err := agents.ResolveTarget(scope, "missing"); err == nil {
			t.Fatalf("resolveTarget(missing) must fail")
		}
		// Two agents share the kind, so the kind group is ambiguous.
		if _, err := agents.ReplaceForSession(ReplaceAgentsForSessionInput{
			Agents: []HerdrAgentLike{
				{
					"agent":        "pi",
					"agent_status": "working",
					"name":         "reviewer",
					"pane_id":      "wA:p1",
					"terminal_id":  "term_1",
					"workspace_id": "wA",
				},
				{
					"agent":        "claude",
					"agent_status": "working",
					"pane_id":      "wA:p2",
					"terminal_id":  "term_2",
					"workspace_id": "wA",
				},
				{
					"agent":        "claude",
					"agent_status": "working",
					"pane_id":      "wA:p3",
					"terminal_id":  "term_3",
					"workspace_id": "wA",
				},
			},
			HerdrSessionName: "default",
		}); err != nil {
			t.Fatalf("replaceForSession: %v", err)
		}
		_, err = agents.ResolveTarget(scope, "claude")
		if err == nil {
			t.Fatalf("resolveTarget(claude) must be ambiguous")
		}
	})

	t.Run("reports existence and fails on an unknown id", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		agent := replacePiAgent(t, agents, piAgentInput{})
		if agent == nil {
			t.Fatalf("the initial agent is missing")
		}
		exists, err := agents.Exists(agent.ID)
		if err != nil {
			t.Fatalf("exists: %v", err)
		}
		if !exists {
			t.Fatalf("exists(%q) = false, want true", agent.ID)
		}
		missing, err := agents.Exists("ag_missing")
		if err != nil {
			t.Fatalf("exists: %v", err)
		}
		if missing {
			t.Fatalf("exists(ag_missing) = true, want false")
		}
		got, err := agents.Get(agent.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ID != agent.ID {
			t.Fatalf("get = %#v, want %q", got, agent.ID)
		}
		if _, err := agents.Get("ag_missing"); err == nil {
			t.Fatalf("get(ag_missing) must fail")
		}
	})

	t.Run("updates the status of a pane and returns the row", func(t *testing.T) {
		agents, _, _, _ := openAgentHarness(t)
		replacePiAgent(t, agents, piAgentInput{})
		updated, err := agents.UpdateStatus(UpdateAgentStatusInput{
			AgentStatus:      observability.AgentStatusIdle,
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("updateStatus: %v", err)
		}
		if updated == nil {
			t.Fatalf("updateStatus returned no agent")
		}
		if updated.AgentStatus != observability.AgentStatusIdle {
			t.Fatalf("agentStatus = %q, want idle", updated.AgentStatus)
		}

		// A generation-scoped update that does not match the stored row leaves it
		// alone and reports nothing.
		missed, err := agents.UpdateStatus(UpdateAgentStatusInput{
			AgentStatus:      observability.AgentStatusWorking,
			HerdrSessionName: "default",
			PaneGeneration:   stringPtr("gen_9"),
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("updateStatus: %v", err)
		}
		if missed != nil {
			t.Fatalf("updateStatus = %#v, want nil", missed)
		}
		current, err := agents.FindByPane(FindAgentByPaneInput{
			HerdrSessionName: "default",
			PaneID:           "wA:p1",
		})
		if err != nil {
			t.Fatalf("findByPane: %v", err)
		}
		if current == nil || current.AgentStatus != observability.AgentStatusIdle {
			t.Fatalf("agentStatus = %v, want idle", current)
		}
	})
}

func boolPtr(value bool) *bool { return &value }
