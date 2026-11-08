package agenthistory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// Tests for the claude-jsonl id resolution: the deterministic projects-root
// enumeration, the file-safety gate, and the transient miss that must never
// enter the 60-second discovery failure cache.

const claudeTestID = "3a4b5c6d-7e8f-4a1b-9c2d-3e4f5a6b7c8d"

func claudeLookup(t *testing.T, root string, value string) *HistoryRef {
	t.Helper()
	return DiscoverAgentHistory(AgentHistoryLookupInput{
		Agent: strp("claude"),
		AgentSession: &observability.AgentSessionRef{
			Agent:  "claude",
			Kind:   observability.SessionRefID,
			Source: "dispatch-agent:claude",
			Value:  value,
		},
		ClaudeProjectsRoot: strp(root),
		HerdrSessionName:   strp("default"),
		PaneID:             strp("wA:p9"),
	}, DiscoverOptions{})
}

func TestDiscoveryResolvesAClaudeSessionIDToItsProjectJSONL(t *testing.T) {
	resetDiscoveryFailureMemory()
	root := t.TempDir()
	projectDir := filepath.Join(root, "-repo-src")
	jsonl := writeSessionFile(t, filepath.Join(projectDir, claudeTestID+".jsonl"), "{}\n")

	requireRefEqual(t, claudeLookup(t, root, claudeTestID), HistoryRef{
		Kind: KindAgentSession, Path: &jsonl, Source: SourceClaudeJSONL, Value: claudeTestID,
	})
}

func TestDiscoveryClaudeIDIsCaseInsensitivelyLowercasedForTheFileName(t *testing.T) {
	resetDiscoveryFailureMemory()
	root := t.TempDir()
	projectDir := filepath.Join(root, "-repo-src")
	upper := "3A4B5C6D-7E8F-4A1B-9C2D-3E4F5A6B7C8D"
	jsonl := writeSessionFile(t, filepath.Join(projectDir, claudeTestID+".jsonl"), "{}\n")

	requireRefEqual(t, claudeLookup(t, root, upper), HistoryRef{
		Kind: KindAgentSession, Path: &jsonl, Source: SourceClaudeJSONL, Value: claudeTestID,
	})
}

func TestDiscoveryClaudeOnlyMatchesInsideProjectSubdirectories(t *testing.T) {
	resetDiscoveryFailureMemory()
	root := t.TempDir()
	// A transcript directly in the root or nested two levels deep is never a
	// candidate: only <root>/<project-dir>/<id>.jsonl is.
	writeSessionFile(t, filepath.Join(root, claudeTestID+".jsonl"), "{}\n")
	deep := filepath.Join(root, "-repo", "nested")
	writeSessionFile(t, filepath.Join(deep, claudeTestID+".jsonl"), "{}\n")
	records := captureDiscoveryWarnings(t)

	if got := claudeLookup(t, root, claudeTestID); got != nil {
		t.Fatalf("discoverAgentHistory = %+v, want nil", got)
	}
	// Only the transient "not created yet" miss is allowed here: nothing was
	// resolved and nothing entered the failure cache (asserted below).
	if len(*records) != 0 {
		t.Fatalf("transient claude miss warned %d time(s): %+v", len(*records), *records)
	}
}

func TestDiscoveryClaudeTransientMissSkipsTheNegativeCache(t *testing.T) {
	resetDiscoveryFailureMemory()
	root := t.TempDir()
	projectDir := filepath.Join(root, "-repo-src")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	records := captureDiscoveryWarnings(t)

	// The transcript does not exist yet (CC creates it on session start): the
	// miss must be silent and must not enter the failure memory.
	if got := claudeLookup(t, root, claudeTestID); got != nil {
		t.Fatalf("first lookup = %+v, want nil", got)
	}
	if len(*records) != 0 {
		t.Fatalf("transient miss produced %d warning(s): %+v", len(*records), *records)
	}
	if DiscoveryFailureFresh(AgentHistoryLookupInput{
		Agent: strp("claude"),
		AgentSession: &observability.AgentSessionRef{
			Agent:  "claude",
			Kind:   observability.SessionRefID,
			Source: "dispatch-agent:claude",
			Value:  claudeTestID,
		},
		ClaudeProjectsRoot: strp(root),
	}, time.Now().UnixMilli()) {
		t.Fatal("transient claude miss entered the discovery failure cache")
	}

	// The file appears a moment later: the very next lookup resolves it, so a
	// task that finishes inside five seconds is still readable.
	jsonl := writeSessionFile(t, filepath.Join(projectDir, claudeTestID+".jsonl"), "{}\n")
	requireRefEqual(t, claudeLookup(t, root, claudeTestID), HistoryRef{
		Kind: KindAgentSession, Path: &jsonl, Source: SourceClaudeJSONL, Value: claudeTestID,
	})
}

func TestDiscoveryClaudeMissingProjectsRootIsTransient(t *testing.T) {
	resetDiscoveryFailureMemory()
	root := filepath.Join(t.TempDir(), "does-not-exist")
	records := captureDiscoveryWarnings(t)

	if got := claudeLookup(t, root, claudeTestID); got != nil {
		t.Fatalf("lookup = %+v, want nil", got)
	}
	if len(*records) != 0 {
		t.Fatalf("missing root produced %d warning(s): %+v", len(*records), *records)
	}
}

func TestDiscoveryClaudeNonUuidIDIsARealFailure(t *testing.T) {
	resetDiscoveryFailureMemory()
	root := t.TempDir()
	records := captureDiscoveryWarnings(t)

	if got := claudeLookup(t, root, "../escape"); got != nil {
		t.Fatalf("non-uuid lookup = %+v, want nil", got)
	}
	requireWarning(t, *records, "claude session id is not a uuid")
}

func TestDiscoveryClaudeUnsafeCandidateIsARealFailure(t *testing.T) {
	resetDiscoveryFailureMemory()
	root := t.TempDir()
	projectDir := filepath.Join(root, "-repo-src")
	// Group-writable: the file exists but fails the safety gate, which is a real
	// (warned, cached) failure, not the transient not-created-yet miss.
	jsonl := writeSessionFile(t, filepath.Join(projectDir, claudeTestID+".jsonl"), "{}\n")
	if err := os.Chmod(jsonl, 0o664); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	records := captureDiscoveryWarnings(t)

	if got := claudeLookup(t, root, claudeTestID); got != nil {
		t.Fatalf("unsafe lookup = %+v, want nil", got)
	}
	requireWarning(t, *records, "is not a readable session file")
}
