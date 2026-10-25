package daemon_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/agenthistory"
	"github.com/dorokuma/herdsman/go/internal/daemon"
)

// The port of the reconciler-driven case of
// test/integration/history-cache-ttl.test.ts. The `AgentHistoryCacheStore
// cleanup` block of that file belongs to the cache store itself and is ported in
// internal/db.

// sourceFile mirrors sourceFile(): a readable history source next to the test
// database.
func (h *reconcileHarness) sourceFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, []byte("history\n"), 0o600); err != nil {
		t.Fatalf("writeFile(%s): %v", path, err)
	}
	return path
}

// putReconcileCache mirrors putCache(): one cached row per source path.
func putReconcileCache(t *testing.T, harness *reconcileHarness, sourcePath string) {
	t.Helper()
	source := string(agenthistory.SourcePiJSONL)
	if err := harness.agentHistoryCache.Put(agenthistory.HistoryCachePutInput{
		CompactHistory:   agenthistory.EmptyCompactHistory(&source),
		FormatterVersion: agenthistory.AgentHistoryFormatterVersion,
		HistoryRef: agenthistory.HistoryRef{
			Kind:   agenthistory.KindDiscoveredFile,
			Path:   &sourcePath,
			Source: agenthistory.SourcePiJSONL,
			Value:  sourcePath,
		},
		SourceMtimeMs: 1,
		SourcePath:    sourcePath,
		SourceSize:    1,
	}); err != nil {
		t.Fatalf("put cache (%s): %v", sourcePath, err)
	}
}

func (h *reconcileHarness) cacheSourcePaths(t *testing.T) []string {
	t.Helper()
	rows, err := h.sqlite.Query(
		"select source_path from agent_history_cache order by source_path")
	if err != nil {
		t.Fatalf("query cache source paths: %v", err)
	}
	defer func() { _ = rows.Close() }()
	paths := []string{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			t.Fatalf("scan cache source path: %v", err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate cache source paths: %v", err)
	}
	return paths
}

func TestHistoryCacheTtlReconcile(t *testing.T) {
	harness := openReconcileHarness(t)
	freshPath := harness.sourceFile(t, "fresh.jsonl")
	expiredPath := harness.sourceFile(t, "expired.jsonl")
	missingPath := filepath.Join(harness.dir, "missing.jsonl")
	putReconcileCache(t, harness, freshPath)
	putReconcileCache(t, harness, expiredPath)
	putReconcileCache(t, harness, missingPath)
	harness.exec(t, "update agent_history_cache set updated_at = ? where source_path = ?",
		nowMs()-daemon.HistoryCacheTtlMs-1_000, expiredPath)

	result, err := newReconciler(harness, staticPanes(), false).
		Reconcile(daemon.ReconcileOptions{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.HistoryCacheExpired != 1 {
		t.Fatalf("historyCacheExpired = %d, want 1", result.HistoryCacheExpired)
	}
	if result.HistoryCacheMissing != 1 {
		t.Fatalf("historyCacheMissing = %d, want 1", result.HistoryCacheMissing)
	}
	remaining := harness.cacheSourcePaths(t)
	if !reflect.DeepEqual(remaining, []string{freshPath}) {
		t.Fatalf("cached source paths = %v, want [%s]", remaining, freshPath)
	}
}
