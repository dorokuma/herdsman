package db

import (
	"reflect"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/agenthistory"
)

// The port of the `AgentHistoryCacheStore cleanup` block of
// test/integration/history-cache-ttl.test.ts. The reconciler-driven TTL case of
// that file belongs to the daemon reconciler and is not part of this port.

func openHistoryCacheTestStore(t *testing.T) *AgentHistoryCacheStore {
	t.Helper()
	return NewAgentHistoryCacheStore(openMigratedTestDB(t))
}

// putCache mirrors the TS putCache helper: one cached row per source path and
// formatter version.
func putCache(
	t *testing.T,
	store *AgentHistoryCacheStore,
	sourcePath string,
	formatterVersion string,
) *AgentHistoryCacheRecord {
	t.Helper()
	source := string(agenthistory.SourcePiJSONL)
	record, err := store.putRecord(agenthistory.HistoryCachePutInput{
		CompactHistory:   agenthistory.EmptyCompactHistory(&source),
		FormatterVersion: formatterVersion,
		HistoryRef: agenthistory.HistoryRef{
			Kind:   agenthistory.KindDiscoveredFile,
			Path:   &sourcePath,
			Source: agenthistory.SourcePiJSONL,
			Value:  sourcePath,
		},
		SourceMtimeMs: 1,
		SourcePath:    sourcePath,
		SourceSize:    1,
	})
	if err != nil {
		t.Fatalf("put %s (%s): %v", sourcePath, formatterVersion, err)
	}
	return record
}

func TestAgentHistoryCacheStoreCleanup(t *testing.T) {
	t.Run("deleteOlderThan removes only stale updated_at rows", func(t *testing.T) {
		store := openHistoryCacheTestStore(t)
		stale := putCache(t, store, "/tmp/stale.jsonl", agenthistory.AgentHistoryFormatterVersion)
		fresh := putCache(t, store, "/tmp/fresh.jsonl", agenthistory.AgentHistoryFormatterVersion)
		if _, err := store.db.Exec(
			"update agent_history_cache set updated_at = ? where id = ?",
			time.Now().UnixMilli()-2_000, stale.ID,
		); err != nil {
			t.Fatalf("age the stale row: %v", err)
		}
		deleted, err := store.DeleteOlderThan(1_000)
		if err != nil {
			t.Fatalf("deleteOlderThan: %v", err)
		}
		if deleted != 1 {
			t.Fatalf("deleteOlderThan(1000) = %d, want 1", deleted)
		}
		rows, err := store.db.Query("select id from agent_history_cache")
		if err != nil {
			t.Fatalf("list remaining ids: %v", err)
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan remaining id: %v", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("list remaining ids: %v", err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("list remaining ids: %v", err)
		}
		if !reflect.DeepEqual(ids, []int64{fresh.ID}) {
			t.Fatalf("remaining ids = %v, want [%d]", ids, fresh.ID)
		}
	})

	t.Run("listSourcePaths and deleteBySourcePaths operate on the given set", func(t *testing.T) {
		store := openHistoryCacheTestStore(t)
		putCache(t, store, "/tmp/a.jsonl", agenthistory.AgentHistoryFormatterVersion)
		putCache(t, store, "/tmp/b.jsonl", "v1")
		putCache(t, store, "/tmp/b.jsonl", "v2")
		putCache(t, store, "/tmp/c.jsonl", agenthistory.AgentHistoryFormatterVersion)

		paths, err := store.ListSourcePaths()
		if err != nil {
			t.Fatalf("listSourcePaths: %v", err)
		}
		want := []string{"/tmp/a.jsonl", "/tmp/b.jsonl", "/tmp/c.jsonl"}
		if !reflect.DeepEqual(paths, want) {
			t.Fatalf("listSourcePaths = %v, want %v", paths, want)
		}

		deleted, err := store.DeleteBySourcePaths(nil)
		if err != nil {
			t.Fatalf("deleteBySourcePaths(nil): %v", err)
		}
		if deleted != 0 {
			t.Fatalf("deleteBySourcePaths(nil) = %d, want 0", deleted)
		}

		deleted, err = store.DeleteBySourcePaths([]string{"/tmp/b.jsonl", "/tmp/missing.jsonl"})
		if err != nil {
			t.Fatalf("deleteBySourcePaths: %v", err)
		}
		if deleted != 2 {
			t.Fatalf("deleteBySourcePaths = %d, want 2", deleted)
		}

		paths, err = store.ListSourcePaths()
		if err != nil {
			t.Fatalf("listSourcePaths: %v", err)
		}
		want = []string{"/tmp/a.jsonl", "/tmp/c.jsonl"}
		if !reflect.DeepEqual(paths, want) {
			t.Fatalf("listSourcePaths = %v, want %v", paths, want)
		}
	})
}
