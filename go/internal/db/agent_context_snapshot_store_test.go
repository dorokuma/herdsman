package db

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The port of test/integration/agent-context-snapshot-store.test.ts. The TS
// harness builds its foreign-key parent agent through the AgentStore, which is
// not ported yet, so the tests insert that row directly.

func openSnapshotTestDB(t *testing.T) (*sql.DB, *AgentContextSnapshotStore) {
	t.Helper()
	db := openMigratedTestDB(t)
	return db, NewAgentContextSnapshotStore(db)
}

// createAgent mirrors the TS createAgent: one running Herdr session plus one
// indexed agent, the parent row the snapshot references.
func createAgent(t *testing.T, db *sql.DB) string {
	t.Helper()
	sessions := NewHerdrSessionStore(db)
	if _, err := sessions.UpsertRunning(UpsertRunningHerdrSessionInput{
		Name:       "default",
		SessionDir: "/tmp/herdr",
		SocketPath: "/tmp/herdr.sock",
	}); err != nil {
		t.Fatalf("upsertRunning: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := db.Exec(
		`insert into agents
		 (id, herdr_session_name, pane_id, terminal_id, workspace_id, agent_status, focused, first_seen_at, last_seen_at)
		 values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"agent-1", "default", "wB:p1", "term_1", "wB", "working", 0, now, now,
	); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	return "agent-1"
}

func snapshotInput(agentID string) AgentContextSnapshotInput {
	historyRef := observability.AgentHistoryRef{
		Kind:   observability.HistoryRefDiscoveredFile,
		Path:   stringPtr("/tmp/claude.jsonl"),
		Source: observability.HistorySourcePiJSONL,
		Value:  "/tmp/claude.jsonl",
	}
	return AgentContextSnapshotInput{
		AgentID: agentID,
		CompactHistory: observability.CompactAgentHistory{
			HistoryRef:           &historyRef,
			LastAssistantMessage: &observability.AgentHistoryExcerpt{Ref: "entry-2", Text: "done"},
			LastToolResult:       nil,
			LastUserMessage:      &observability.AgentHistoryExcerpt{Ref: "entry-1", Text: "work"},
			MessageCount:         2,
			Source:               stringPtr("pi-jsonl"),
			UpdatedAt:            stringPtr("2026-07-16T00:00:00.000Z"),
		},
		HistoryRef:   &historyRef,
		PaneRevision: intPtr(42),
		SourceFingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: 100,
			Path:    "/tmp/claude.jsonl",
			Size:    200,
		},
	}
}

func TestAgentContextSnapshotStore(t *testing.T) {
	t.Run("round-trips, upserts, lists, and cascades snapshots", func(t *testing.T) {
		db, snapshots := openSnapshotTestDB(t)
		agentID := createAgent(t, db)

		got, err := snapshots.Get(agentID)
		if err != nil {
			t.Fatalf("get before put: %v", err)
		}
		if got != nil {
			t.Fatalf("get before put = %+v, want nil", got)
		}

		input := snapshotInput(agentID)
		inserted, err := snapshots.Put(input)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		if !matchSnapshotInput(inserted, input) {
			t.Fatalf("inserted = %+v, want it to match %+v", inserted, input)
		}
		if inserted.UpdatedAt == "" {
			t.Fatal("inserted.updatedAt must be set")
		}
		fetched, err := snapshots.Get(agentID)
		if err != nil || fetched == nil {
			t.Fatalf("get after put = %v, %v; want the stored record", fetched, err)
		}
		if !reflect.DeepEqual(*fetched, inserted) {
			t.Fatalf("get after put = %+v, want %+v", *fetched, inserted)
		}

		updatedInput := input
		updatedInput.CompactHistory.LastAssistantMessage = &observability.AgentHistoryExcerpt{
			Ref:  "entry-3",
			Text: "finished",
		}
		updatedInput.PaneRevision = intPtr(43)
		updatedInput.SourceFingerprint = &observability.AgentHistorySourceFingerprint{
			MtimeMs: 101,
			Path:    "/tmp/claude.jsonl",
			Size:    200,
		}
		updated, err := snapshots.Put(updatedInput)
		if err != nil {
			t.Fatalf("put update: %v", err)
		}
		if updated.CompactHistory.LastAssistantMessage == nil ||
			updated.CompactHistory.LastAssistantMessage.Text != "finished" {
			t.Fatalf("updated last assistant message = %+v, want text \"finished\"",
				updated.CompactHistory.LastAssistantMessage)
		}
		if updated.PaneRevision == nil || *updated.PaneRevision != 43 {
			t.Fatalf("updated paneRevision = %v, want 43", updated.PaneRevision)
		}
		if updated.SourceFingerprint == nil || updated.SourceFingerprint.MtimeMs != 101 {
			t.Fatalf("updated source fingerprint = %+v, want mtimeMs 101", updated.SourceFingerprint)
		}
		var count int
		if err := db.QueryRow("select count(*) from agent_context_snapshots").Scan(&count); err != nil {
			t.Fatalf("count snapshots: %v", err)
		}
		if count != 1 {
			t.Fatalf("snapshot count = %d, want 1", count)
		}
		listed, err := snapshots.ListByAgentIds([]string{agentID, "unknown"})
		if err != nil {
			t.Fatalf("listByAgentIds: %v", err)
		}
		if len(listed) != 1 || !reflect.DeepEqual(listed[0], updated) {
			t.Fatalf("listByAgentIds = %+v, want [%+v]", listed, updated)
		}

		// Removing the agent cascades the snapshot away with it.
		if _, err := db.Exec("delete from agents where herdr_session_name = ?", "default"); err != nil {
			t.Fatalf("delete agents: %v", err)
		}
		got, err = snapshots.Get(agentID)
		if err != nil || got != nil {
			t.Fatalf("get after cascade = %v, %v; want nil, nil", got, err)
		}
	})

	t.Run("round-trips a snapshot without readable history", func(t *testing.T) {
		db, snapshots := openSnapshotTestDB(t)
		agentID := createAgent(t, db)
		snapshot, err := snapshots.Put(AgentContextSnapshotInput{
			AgentID:        agentID,
			CompactHistory: observability.CompactAgentHistory{MessageCount: 0},
		})
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		if snapshot.HistoryRef != nil {
			t.Fatalf("historyRef = %+v, want nil", snapshot.HistoryRef)
		}
		if snapshot.SourceFingerprint != nil {
			t.Fatalf("sourceFingerprint = %+v, want nil", snapshot.SourceFingerprint)
		}
		if snapshot.CompactHistory.HistoryRef != nil {
			t.Fatalf("compactHistory.historyRef = %+v, want nil", snapshot.CompactHistory.HistoryRef)
		}
	})

	t.Run("round-trips long excerpts without truncation", func(t *testing.T) {
		db, snapshots := openSnapshotTestDB(t)
		agentID := createAgent(t, db)
		input := snapshotInput(agentID)
		assistantText := strings.Repeat("x", 50000)
		userText := strings.Repeat("y", 2100)
		input.CompactHistory.LastAssistantMessage = &observability.AgentHistoryExcerpt{
			Ref:  "long",
			Text: assistantText,
		}
		input.CompactHistory.LastUserMessage = &observability.AgentHistoryExcerpt{
			Ref:  "long-user",
			Text: userText,
		}
		snapshot, err := snapshots.Put(input)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		if snapshot.CompactHistory.LastAssistantMessage == nil ||
			snapshot.CompactHistory.LastAssistantMessage.Text != assistantText {
			t.Fatalf("assistant text length = %d, want %d",
				len(snapshot.CompactHistory.LastAssistantMessage.Text), len(assistantText))
		}
		if snapshot.CompactHistory.LastUserMessage == nil ||
			snapshot.CompactHistory.LastUserMessage.Text != userText {
			t.Fatalf("user text length = %d, want %d",
				len(snapshot.CompactHistory.LastUserMessage.Text), len(userText))
		}
	})

	t.Run("rejects partial history metadata", func(t *testing.T) {
		db, snapshots := openSnapshotTestDB(t)
		agentID := createAgent(t, db)
		input := snapshotInput(agentID)
		input.SourceFingerprint = nil
		_, err := snapshots.Put(input)
		if err == nil || err.Error() !=
			"Agent context history ref and source fingerprint must both be null or non-null" {
			t.Fatalf("put with partial metadata: err = %v, want the both-null-or-non-null error", err)
		}
	})
}

// matchSnapshotInput mirrors the TS `toMatchObject(input)`: every field the
// caller supplied round-trips.
func matchSnapshotInput(
	record observability.AgentContextSnapshotRecord,
	input AgentContextSnapshotInput,
) bool {
	return record.AgentID == input.AgentID &&
		reflect.DeepEqual(record.CompactHistory, input.CompactHistory) &&
		reflect.DeepEqual(record.HistoryRef, input.HistoryRef) &&
		reflect.DeepEqual(record.PaneRevision, input.PaneRevision) &&
		reflect.DeepEqual(record.SourceFingerprint, input.SourceFingerprint)
}

func stringPtr(value string) *string { return &value }

func intPtr(value int) *int { return &value }
