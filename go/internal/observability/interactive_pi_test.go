package observability

import (
	"os"
	"path/filepath"
	"testing"
)

func piAgent(sessionValue *string) *AgentIndexRecord {
	agent := "pi"
	record := &AgentIndexRecord{Agent: &agent}
	if sessionValue != nil {
		record.AgentSession = &AgentSessionRef{
			Agent:  "pi",
			Kind:   SessionRefPath,
			Source: "herdr:pi",
			Value:  *sessionValue,
		}
	}
	return record
}

func strPtr(s string) *string { return &s }

func TestRejectsSessionPathContainingParentRef(t *testing.T) {
	if IsInteractivePiAgent(piAgent(strPtr("/root/.pi/agent/sessions/../x.jsonl"))) {
		t.Fatal("a session path containing .. must be non-interactive")
	}
}

func TestClassifiesDispatchedRoleSessionAsNonInteractive(t *testing.T) {
	path := "/tmp/herdr-role-sessions/role-x/session.jsonl"
	if IsInteractivePiAgent(piAgent(strPtr(path))) {
		t.Fatalf("%q must be classified as a dispatched role session", path)
	}
}

func TestDoesNotClassifyNewRootHerdrSessionAsInteractive(t *testing.T) {
	path := "/tmp/herdr-role-sessions/beta/role-worker/session.jsonl"
	if IsInteractivePiAgent(piAgent(strPtr(path))) {
		t.Fatalf("%q must be non-interactive", path)
	}
}

func TestClassifiesRealRootSessionAsInteractive(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "x.jsonl")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	if !IsInteractivePiAgent(piAgent(strPtr(file))) {
		t.Fatalf("a real root session %q must be interactive", file)
	}
}

func TestDoesNotIsolatePiAgentsWithoutSessionPath(t *testing.T) {
	if IsInteractivePiAgent(piAgent(nil)) {
		t.Fatal("agent without agentSession must be non-interactive")
	}
	if IsInteractivePiAgent(piAgent(strPtr(""))) {
		t.Fatal("agent with an empty session value must be non-interactive")
	}
	if IsInteractivePiAgent(nil) {
		t.Fatal("nil agent must be non-interactive")
	}
	other := "claude"
	if IsInteractivePiAgent(&AgentIndexRecord{Agent: &other, AgentSession: &AgentSessionRef{Value: "/tmp/x.jsonl"}}) {
		t.Fatal("a non-pi agent must be non-interactive")
	}
}
