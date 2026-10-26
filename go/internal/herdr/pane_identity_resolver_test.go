package herdr

import (
	"sort"
	"strings"
	"testing"
)

// This file ports the pane-identity cases of
// test/integration/herdr-pane-identity-resolver.test.ts. The file's first case
// ("finds only running sessions by exact socket path") targets the herdr
// sessions DB store, which is ported separately in internal/db.

// mockPaneClient is the Go form of the vitest `{ close, getPane }` fake.
type mockPaneClient struct {
	getPaneFunc func(params map[string]any) (any, error)
	onClose     func()
}

func (m *mockPaneClient) GetPane(params map[string]any) (any, error) {
	return m.getPaneFunc(params)
}

func (m *mockPaneClient) Close() {
	if m.onClose != nil {
		m.onClose()
	}
}

// paneClientRecorder hands out a fresh mock per call (as the TS clientFactory
// does) while recording every getPane params and every close across all of them.
type paneClientRecorder struct {
	responses    []any
	index        int
	getPaneCalls []map[string]any
	closeCount   int
}

func (r *paneClientRecorder) factory(socketPath string) PaneClient {
	return &mockPaneClient{
		getPaneFunc: func(params map[string]any) (any, error) {
			r.getPaneCalls = append(r.getPaneCalls, params)
			response := r.responses[r.index]
			r.index++
			return response, nil
		},
		onClose: func() { r.closeCount++ },
	}
}

func TestResolveHerdrPaneIdentityReadsOfficialEnvelope(t *testing.T) {
	rec := &paneClientRecorder{responses: []any{
		map[string]any{
			"type": "pane_info",
			"pane": map[string]any{"pane_id": "wB:p2", "terminal_id": "term_2", "workspace_id": "wB"},
		},
		// Herdsman used to also accept camelCase aliases and an unwrapped result;
		// Herdr 0.9.3 only ever answers with the snake_case pane_info envelope.
		map[string]any{
			"type": "pane_info",
			"pane": map[string]any{"paneId": "wB:p2", "terminalId": "term_2", "workspaceId": "wB"},
		},
		map[string]any{"pane_id": "wB:p2", "terminal_id": "term_2", "workspace_id": "wB"},
	}}
	input := ResolveHerdrPaneIdentityInput{ClientFactory: rec.factory, PaneID: "wA:p1", SocketPath: "/tmp/herdr.sock"}

	identity, err := ResolveHerdrPaneIdentity(input)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	assertJSONEqual(t, identity, map[string]any{"paneId": "wB:p2", "terminalId": "term_2", "workspaceId": "wB"})

	if _, err := ResolveHerdrPaneIdentity(input); err == nil || !strings.Contains(err.Error(), "Herdr pane response has no terminal identity") {
		t.Fatalf("second resolve err = %v, want no terminal identity", err)
	}
	if _, err := ResolveHerdrPaneIdentity(input); err == nil || !strings.Contains(err.Error(), "Invalid Herdr pane response") {
		t.Fatalf("third resolve err = %v, want invalid response", err)
	}

	for _, call := range rec.getPaneCalls {
		assertJSONEqual(t, call, map[string]any{"pane_id": "wA:p1"})
	}
	if rec.closeCount != 3 {
		t.Fatalf("close count = %d, want 3", rec.closeCount)
	}
}

func TestResolveHerdrPaneIdentityLocksRealPaneGetShape(t *testing.T) {
	// The exact shape from test/fixtures/herdr-pane-get.json.
	fixture := map[string]any{
		"type": "pane_info",
		"pane": map[string]any{
			"agent":                   "pi",
			"agent_session":           nil,
			"agent_status":            "idle",
			"cwd":                     "/root/herdsman",
			"display_agent":           "pi",
			"focused":                 true,
			"foreground_cwd":          "/root/herdsman",
			"label":                   nil,
			"pane_id":                 "wB:p2",
			"revision":                3,
			"scroll":                  nil,
			"state_labels":            map[string]any{},
			"tab_id":                  "wB:t1",
			"terminal_id":             "term_2",
			"terminal_title":          nil,
			"terminal_title_stripped": nil,
			"title":                   nil,
			"tokens":                  map[string]any{},
			"workspace_id":            "wB",
		},
	}
	pane := fixture["pane"].(map[string]any)
	if _, hasPID := pane["pid"]; hasPID {
		t.Fatal("fixture pane must not carry pid")
	}
	gotKeys := make([]string, 0, len(pane))
	for key := range pane {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	assertJSONEqual(t, gotKeys, []string{
		"agent", "agent_session", "agent_status", "cwd", "display_agent", "focused",
		"foreground_cwd", "label", "pane_id", "revision", "scroll", "state_labels",
		"tab_id", "terminal_id", "terminal_title", "terminal_title_stripped", "title",
		"tokens", "workspace_id",
	})

	rec := &paneClientRecorder{responses: []any{fixture}}
	identity, err := ResolveHerdrPaneIdentity(ResolveHerdrPaneIdentityInput{
		ClientFactory: rec.factory,
		PaneID:        "wB:p2",
		SocketPath:    "/tmp/herdr.sock",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// cwd is extracted, and there is no pid to extract.
	assertJSONEqual(t, identity, map[string]any{
		"agent":         "pi",
		"cwd":           "/root/herdsman",
		"foregroundCwd": "/root/herdsman",
		"paneId":        "wB:p2",
		"tabId":         "wB:t1",
		"terminalId":    "term_2",
		"workspaceId":   "wB",
	})
	for _, call := range rec.getPaneCalls {
		assertJSONEqual(t, call, map[string]any{"pane_id": "wB:p2"})
	}
	if rec.closeCount != 1 {
		t.Fatalf("close count = %d, want 1", rec.closeCount)
	}
}

func TestResolveHerdrPaneIdentityIgnoresPID(t *testing.T) {
	rec := &paneClientRecorder{responses: []any{
		map[string]any{
			"type": "pane_info",
			"pane": map[string]any{
				"pane_id":      "wB:p2",
				"pid":          4242,
				"terminal_id":  "term_2",
				"workspace_id": "wB",
				"cwd":          "/repo",
			},
		},
	}}
	identity, err := ResolveHerdrPaneIdentity(ResolveHerdrPaneIdentityInput{
		ClientFactory: rec.factory,
		PaneID:        "wB:p2",
		SocketPath:    "/tmp/herdr.sock",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	assertJSONEqual(t, identity, map[string]any{
		"cwd":         "/repo",
		"paneId":      "wB:p2",
		"terminalId":  "term_2",
		"workspaceId": "wB",
	})
	if rec.closeCount != 1 {
		t.Fatalf("close count = %d, want 1", rec.closeCount)
	}
}

func TestResolveHerdrPaneIdentityRejectsIncompleteAndCloses(t *testing.T) {
	rec := &paneClientRecorder{responses: []any{
		map[string]any{"type": "pane_info", "pane": map[string]any{"pane_id": "wB:p2"}},
	}}
	if _, err := ResolveHerdrPaneIdentity(ResolveHerdrPaneIdentityInput{
		ClientFactory: rec.factory,
		PaneID:        "wB:p2",
		SocketPath:    "/tmp/herdr.sock",
	}); err == nil || !strings.Contains(err.Error(), "Herdr pane response has no terminal identity") {
		t.Fatalf("resolve err = %v, want no terminal identity", err)
	}
	if rec.closeCount != 1 {
		t.Fatalf("close count = %d, want 1", rec.closeCount)
	}
}
