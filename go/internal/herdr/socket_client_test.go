package herdr

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/shared"
)

// This file ports test/integration/herdr-socket-client.test.ts.

// fakeHerdrServer is the Go form of openFakeHerdrServer: a Unix-socket server
// that records every request it receives and delegates to a per-test handler.
type fakeHerdrServer struct {
	socketPath string
	listener   net.Listener
	mu         sync.Mutex
	requests   []map[string]any
	handler    func(conn net.Conn, request map[string]any)
}

func openFakeHerdrServer(t *testing.T, handler func(conn net.Conn, request map[string]any)) *fakeHerdrServer {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &fakeHerdrServer{socketPath: socketPath, listener: listener, handler: handler}
	t.Cleanup(func() { _ = listener.Close() })
	go server.acceptLoop()
	return server
}

func (s *fakeHerdrServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *fakeHerdrServer) handleConn(conn net.Conn) {
	defer conn.Close()
	decoder, err := shared.NewJSONLineDecoder(shared.DefaultMaxFrameBytes)
	if err != nil {
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			frames, decodeErr := decoder.Push(string(buf[:n]))
			if decodeErr != nil {
				return
			}
			for _, message := range frames {
				request, ok := message.(map[string]any)
				if !ok {
					continue
				}
				s.mu.Lock()
				s.requests = append(s.requests, request)
				s.mu.Unlock()
				s.handler(conn, request)
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *fakeHerdrServer) recordedRequests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.requests))
	copy(out, s.requests)
	return out
}

// writeJSONLine writes one JSON Lines frame.
func writeJSONLine(t *testing.T, conn net.Conn, value any) {
	t.Helper()
	frame, err := shared.EncodeJSONLine(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := conn.Write([]byte(frame)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// writeJSONLines writes several frames as a single Write, so a burst (an ack
// followed by notifications) reaches the client in one chunk exactly as the TS
// test's back-to-back socket.write calls do on a local socket.
func writeJSONLines(t *testing.T, conn net.Conn, values ...any) {
	t.Helper()
	var buf []byte
	for _, value := range values {
		frame, err := shared.EncodeJSONLine(value)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		buf = append(buf, frame...)
	}
	if _, err := conn.Write(buf); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	gotJSON := normalizeJSON(t, got)
	wantJSON := normalizeJSON(t, want)
	if gotJSON != wantJSON {
		t.Fatalf("JSON mismatch:\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// normalizeJSON round-trips a value through a generic decode so struct field
// order and int/float number spelling do not matter: both sides compare as
// sorted-key JSON.
func normalizeJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	normalized, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(normalized)
}

func requestMethods(requests []map[string]any) []string {
	methods := make([]string, 0, len(requests))
	for _, request := range requests {
		method, _ := request["method"].(string)
		methods = append(methods, method)
	}
	return methods
}

func TestHerdrSocketClientGetsPaneMetadata(t *testing.T) {
	server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
		writeJSONLine(t, conn, map[string]any{
			"id":     request["id"],
			"result": map[string]any{"pane": map[string]any{"pane_id": "w1:p2", "terminal_id": "term_2"}},
		})
	})

	client, err := NewHerdrSocketClient(HerdrSocketClientOptions{SocketPath: server.socketPath})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	result, err := client.GetPane(map[string]any{"pane_id": "w1:p2"})
	if err != nil {
		t.Fatalf("getPane: %v", err)
	}
	assertJSONEqual(t, result, map[string]any{
		"pane": map[string]any{"pane_id": "w1:p2", "terminal_id": "term_2"},
	})

	assertJSONEqual(t, server.recordedRequests(), []map[string]any{
		{"id": "herdsman-1", "method": "pane.get", "params": map[string]any{"pane_id": "w1:p2"}},
	})
}

func TestHerdrSocketClientUsesSessionSnapshots(t *testing.T) {
	sessionSnapshot := map[string]any{
		"type": "session_snapshot",
		"snapshot": map[string]any{
			"agents":               []any{map[string]any{"agent": "pi", "pane_id": "w1:p1", "workspace_id": "w1"}},
			"focused_pane_id":      "w1:p1",
			"focused_tab_id":       "w1:t1",
			"focused_workspace_id": "w1",
			"layouts":              []any{map[string]any{"focused_pane_id": "w1:p1", "tab_id": "w1:t1", "workspace_id": "w1"}},
			"panes":                []any{map[string]any{"focused": true, "pane_id": "w1:p1", "workspace_id": "w1"}},
			"protocol":             16,
			"tabs":                 []any{map[string]any{"focused": true, "tab_id": "w1:t1", "workspace_id": "w1"}},
			"version":              "0.7.2",
			"workspaces":           []any{map[string]any{"focused": true, "label": "Repo", "workspace_id": "w1"}},
		},
	}
	server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
		writeJSONLine(t, conn, map[string]any{"id": request["id"], "result": sessionSnapshot})
	})

	client, err := NewHerdrSocketClient(HerdrSocketClientOptions{SocketPath: server.socketPath})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	result, err := client.SessionSnapshot()
	if err != nil {
		t.Fatalf("sessionSnapshot: %v", err)
	}
	assertJSONEqual(t, result, sessionSnapshot)

	assertJSONEqual(t, requestMethods(server.recordedRequests()), []string{"session.snapshot"})
}

func TestHerdrSocketClientFallsBackToListAPIs(t *testing.T) {
	server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
		method, _ := request["method"].(string)
		switch method {
		case "session.snapshot":
			writeJSONLine(t, conn, map[string]any{
				"id":    "",
				"error": map[string]any{"code": "invalid_request", "message": "invalid request: unknown variant `session.snapshot`"},
			})
		case "workspace.list":
			writeJSONLine(t, conn, map[string]any{
				"id":     request["id"],
				"result": map[string]any{"type": "workspace_list", "workspaces": []any{map[string]any{"focused": true, "label": "Repo", "workspace_id": "w1"}}},
			})
		case "pane.list":
			writeJSONLine(t, conn, map[string]any{
				"id":     request["id"],
				"result": map[string]any{"panes": []any{map[string]any{"focused": true, "pane_id": "w1:p1", "workspace_id": "w1"}}},
			})
		case "tab.list":
			writeJSONLine(t, conn, map[string]any{
				"id":     request["id"],
				"result": map[string]any{"tabs": []any{map[string]any{"focused": true, "tab_id": "w1:t1", "workspace_id": "w1"}}},
			})
		case "agent.list":
			writeJSONLine(t, conn, map[string]any{
				"id":     request["id"],
				"result": map[string]any{"agents": []any{map[string]any{"agent": "Pi", "pane_id": "w1:p1", "status": "working"}}},
			})
		default:
			writeJSONLine(t, conn, map[string]any{"id": request["id"], "result": map[string]any{}})
		}
	})

	client, err := NewHerdrSocketClient(HerdrSocketClientOptions{SocketPath: server.socketPath})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	result, err := client.SessionSnapshot()
	if err != nil {
		t.Fatalf("sessionSnapshot: %v", err)
	}
	assertJSONEqual(t, result, map[string]any{
		"snapshot": map[string]any{
			"agents":               []any{map[string]any{"agent": "Pi", "pane_id": "w1:p1", "status": "working"}},
			"focused_pane_id":      "w1:p1",
			"focused_workspace_id": "w1",
			"panes":                []any{map[string]any{"focused": true, "pane_id": "w1:p1", "workspace_id": "w1"}},
			"tabs":                 []any{map[string]any{"focused": true, "tab_id": "w1:t1", "workspace_id": "w1"}},
			"workspaces":           []any{map[string]any{"focused": true, "label": "Repo", "workspace_id": "w1"}},
		},
	})

	methods := requestMethods(server.recordedRequests())
	// session.snapshot is awaited before the fallback, so it is recorded first;
	// the four list methods follow concurrently and are compared as a set.
	if len(methods) != 5 || methods[0] != "session.snapshot" {
		t.Fatalf("methods = %v, want session.snapshot first of 5", methods)
	}
	rest := append([]string(nil), methods[1:]...)
	sort.Strings(rest)
	assertJSONEqual(t, rest, []string{"agent.list", "pane.list", "tab.list", "workspace.list"})
}

func TestHerdrSocketClientSubscribesAndYieldsNotifications(t *testing.T) {
	server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
		writeJSONLines(t, conn,
			map[string]any{"id": request["id"], "result": map[string]any{"subscribed": true}},
			map[string]any{
				"data":  map[string]any{"agent_status": "idle", "pane_id": "w1:p1", "workspace_id": "w1"},
				"event": "pane.agent_status_changed",
			},
			map[string]any{
				"data":  map[string]any{"pane_id": "w1:p2", "workspace_id": "w1"},
				"event": "pane_created",
			},
		)
	})

	client, err := NewHerdrSocketClient(HerdrSocketClientOptions{SocketPath: server.socketPath})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.SubscribeEvents(ctx, []string{"w1:p1"})
	if err != nil {
		t.Fatalf("subscribeEvents: %v", err)
	}

	event1, done, err := stream.Next()
	if err != nil || done {
		t.Fatalf("next 1: done=%v err=%v", done, err)
	}
	assertJSONEqual(t, event1, map[string]any{
		"agent_status": "idle",
		"pane_id":      "w1:p1",
		"type":         "pane.agent_status_changed",
		"workspace_id": "w1",
	})

	event2, done, err := stream.Next()
	if err != nil || done {
		t.Fatalf("next 2: done=%v err=%v", done, err)
	}
	assertJSONEqual(t, event2, map[string]any{
		"pane_id":      "w1:p2",
		"type":         "pane.created",
		"workspace_id": "w1",
	})

	cancel()

	requests := server.recordedRequests()
	if len(requests) == 0 || requests[0]["method"] != "events.subscribe" {
		t.Fatalf("first request = %v, want events.subscribe", requests)
	}
	assertJSONEqual(t, requests[0]["params"], map[string]any{
		"subscriptions": []any{
			map[string]any{"type": "pane.created"},
			map[string]any{"type": "pane.closed"},
			map[string]any{"type": "pane.moved"},
			map[string]any{"type": "pane.agent_detected"},
			map[string]any{"type": "workspace.closed"},
			map[string]any{"type": "pane.agent_status_changed", "pane_id": "w1:p1"},
		},
	})
}

func TestHerdrSocketClientSubscribesTopologyEventsWithoutPaneIDs(t *testing.T) {
	server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
		writeJSONLines(t, conn,
			map[string]any{"id": request["id"], "result": map[string]any{"subscribed": true}},
			map[string]any{
				"data":  map[string]any{"pane_id": "w1:p2", "workspace_id": "w1"},
				"event": "pane.created",
			},
		)
	})

	client, err := NewHerdrSocketClient(HerdrSocketClientOptions{SocketPath: server.socketPath})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.SubscribeEvents(ctx, nil)
	if err != nil {
		t.Fatalf("subscribeEvents: %v", err)
	}

	event, done, err := stream.Next()
	if err != nil || done {
		t.Fatalf("next: done=%v err=%v", done, err)
	}
	assertJSONEqual(t, event, map[string]any{
		"pane_id":      "w1:p2",
		"type":         "pane.created",
		"workspace_id": "w1",
	})

	cancel()

	requests := server.recordedRequests()
	if len(requests) == 0 || requests[0]["method"] != "events.subscribe" {
		t.Fatalf("first request = %v, want events.subscribe", requests)
	}
	assertJSONEqual(t, requests[0]["params"], map[string]any{
		"subscriptions": []any{
			map[string]any{"type": "pane.created"},
			map[string]any{"type": "pane.closed"},
			map[string]any{"type": "pane.moved"},
			map[string]any{"type": "pane.agent_detected"},
			map[string]any{"type": "workspace.closed"},
		},
	})
}

func TestHerdrSocketClientRefusesSecondEventsSubscribe(t *testing.T) {
	server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
		writeJSONLine(t, conn, map[string]any{"id": request["id"], "result": map[string]any{"subscribed": true}})
	})

	client, err := NewHerdrSocketClient(HerdrSocketClientOptions{SocketPath: server.socketPath})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.SubscribeEvents(ctx, []string{"w1:p1"})
	if err != nil {
		t.Fatalf("first subscribeEvents: %v", err)
	}

	// The fake server answers events.subscribe and pushes nothing, so no event
	// may arrive on the first stream within this window; the timeout branch
	// winning is the assertion itself.
	type nextResult struct {
		value any
		done  bool
		err   error
	}
	nextCh := make(chan nextResult, 1)
	go func() {
		value, done, err := stream.Next()
		nextCh <- nextResult{value: value, done: done, err: err}
	}()
	select {
	case res := <-nextCh:
		t.Fatalf("unexpected first-stream outcome: value=%v done=%v err=%v", res.value, res.done, res.err)
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := client.SubscribeEvents(ctx, nil); err == nil || !strings.Contains(err.Error(), "already has an events.subscribe") {
		t.Fatalf("second subscribeEvents err = %v, want already-subscribed", err)
	}

	cancel()
	<-nextCh // let the first stream's Next observe the cancel and finish

	subscribeCount := 0
	for _, request := range server.recordedRequests() {
		if request["method"] == "events.subscribe" {
			subscribeCount++
		}
	}
	if subscribeCount != 1 {
		t.Fatalf("events.subscribe count = %d, want 1", subscribeCount)
	}
}

func TestHerdrSocketClientYieldsQueuedEventsAfterAbort(t *testing.T) {
	// Drain protects status already queued for panes this connection subscribed
	// to (w1:p1). It is not a same-stream path for a newly created pane.
	server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
		writeJSONLines(t, conn,
			map[string]any{"id": request["id"], "result": map[string]any{"subscribed": true}},
			map[string]any{
				"data":  map[string]any{"agent_status": "working", "pane_id": "w1:p1"},
				"event": "pane.agent_status_changed",
			},
			map[string]any{
				"data":  map[string]any{"agent_status": "idle", "pane_id": "w1:p1"},
				"event": "pane.agent_status_changed",
			},
		)
	})

	client, err := NewHerdrSocketClient(HerdrSocketClientOptions{SocketPath: server.socketPath})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.SubscribeEvents(ctx, []string{"w1:p1"})
	if err != nil {
		t.Fatalf("subscribeEvents: %v", err)
	}

	event1, done, err := stream.Next()
	if err != nil || done {
		t.Fatalf("next 1: done=%v err=%v", done, err)
	}
	assertJSONEqual(t, event1, map[string]any{
		"agent_status": "working",
		"pane_id":      "w1:p1",
		"type":         "pane.agent_status_changed",
	})

	cancel()

	event2, done, err := stream.Next()
	if err != nil || done {
		t.Fatalf("next 2 (drain): done=%v err=%v", done, err)
	}
	assertJSONEqual(t, event2, map[string]any{
		"agent_status": "idle",
		"pane_id":      "w1:p1",
		"type":         "pane.agent_status_changed",
	})

	_, done, err = stream.Next()
	if err != nil {
		t.Fatalf("next 3 err: %v", err)
	}
	if !done {
		t.Fatal("next 3 done = false, want true")
	}
}

func TestHerdrSocketClientSilentPeerTimesOut(t *testing.T) {
	// The server accepts the connection and never writes a response: before the
	// request timeout this parked the caller (and daemon shutdown) forever.
	server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {})

	client, err := NewHerdrSocketClient(HerdrSocketClientOptions{RequestTimeoutMs: 50, SocketPath: server.socketPath})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	if _, err := client.SessionSnapshot(); err == nil || !strings.Contains(err.Error(), "Herdr session.snapshot request timed out after 50ms") {
		t.Fatalf("sessionSnapshot err = %v, want 50ms timeout", err)
	}
	if _, err := client.GetPane(map[string]any{"pane_id": "w1:p1"}); err == nil || !strings.Contains(err.Error(), "Herdr pane.get request timed out after 50ms") {
		t.Fatalf("getPane err = %v, want 50ms timeout", err)
	}
}

// TestHerdrSocketClientDeadlineCoversWriteAndRead is the regression test for the
// request deadline: the budget must bound the dial, the write and the read of a
// request, not just the logical wait for a response.
//
// A peer that accepts the connection and then goes silent used to leave the
// caller parked in the read until the OS gave up on the socket — far past the
// request timeout. Both the one-shot and the persistent request path have to
// return inside the budget.
func TestHerdrSocketClientDeadlineCoversWriteAndRead(t *testing.T) {
	const timeoutMs = 50
	budget := time.Duration(timeoutMs) * time.Millisecond

	t.Run("requestOnce times out inside the budget", func(t *testing.T) {
		// The peer accepts the connection and never answers, so only the deadline
		// can bring the caller back.
		server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {})

		client, err := NewHerdrSocketClient(HerdrSocketClientOptions{
			RequestTimeoutMs: timeoutMs,
			SocketPath:       server.socketPath,
		})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		defer client.Close()

		started := time.Now()
		_, err = client.SessionSnapshot()
		elapsed := time.Since(started)
		if err == nil {
			t.Fatal("sessionSnapshot = nil error, want a timeout")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("sessionSnapshot err = %v, want a request timeout", err)
		}
		// Generous ceiling: the budget plus scheduling slack. Without the deadline
		// this read would block for the OS socket timeout, which is minutes.
		if elapsed > budget*4 {
			t.Fatalf("sessionSnapshot took %v, want it bounded by the %v budget", elapsed, budget)
		}
	})

	t.Run("persistent request times out inside the budget", func(t *testing.T) {
		server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
			buf := make([]byte, 32*1024)
			_, _ = conn.Read(buf)
		})

		client, err := NewHerdrSocketClient(HerdrSocketClientOptions{
			RequestTimeoutMs: timeoutMs,
			SocketPath:       server.socketPath,
		})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		defer client.Close()

		started := time.Now()
		_, err = client.GetPane(map[string]any{"pane_id": "w1:p1"})
		elapsed := time.Since(started)
		if err == nil {
			t.Fatal("getPane = nil error, want a timeout")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("getPane err = %v, want a request timeout", err)
		}
		if elapsed > budget*4 {
			t.Fatalf("getPane took %v, want it bounded by the %v budget", elapsed, budget)
		}
	})

	t.Run("a timed-out request leaves the socket usable", func(t *testing.T) {
		// The peer reads every request frame and stays silent on the first one,
		// then answers the rest, so the persistent reader must survive the timeout.
		var mu sync.Mutex
		answered := 0
		server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
			mu.Lock()
			answered++
			count := answered
			mu.Unlock()
			if count == 1 {
				return
			}
			writeJSONLine(t, conn, map[string]any{"id": request["id"], "result": map[string]any{"pane_id": "w1:p1"}})
		})

		client, err := NewHerdrSocketClient(HerdrSocketClientOptions{
			RequestTimeoutMs: timeoutMs,
			SocketPath:       server.socketPath,
		})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		defer client.Close()

		if _, err := client.GetPane(map[string]any{"pane_id": "w1:p1"}); err == nil {
			t.Fatal("first getPane = nil error, want a timeout")
		}
		result, err := client.GetPane(map[string]any{"pane_id": "w1:p1"})
		if err != nil {
			t.Fatalf("second getPane after a timeout: %v", err)
		}
		pane, ok := result.(map[string]any)
		if !ok || pane["pane_id"] != "w1:p1" {
			t.Fatalf("second getPane result = %v, want the answered pane", result)
		}
	})
}

// TestHerdrSocketClientConcurrentRequestsKeepTheirOwnTimeouts is the
// regression test for the persistent connection's deadline handling.
//
// The persistent socket used to carry one `SetDeadline` for whichever request
// happened to be in flight, and cleared it when that request's response arrived.
// With two requests outstanding, the first one to finish removed the budget the
// second one was still waiting under, so a peer that had already stopped
// answering could park the second caller until the OS gave up on the socket.
//
// The write is now bounded by its own write-only deadline inside writeMu, and
// the read is bounded by the request's own timer, so neither request can clear
// the other's budget.
func TestHerdrSocketClientConcurrentRequestsKeepTheirOwnTimeouts(t *testing.T) {
	const timeoutMs = 400
	budget := time.Duration(timeoutMs) * time.Millisecond

	t.Run("a timed-out request does not clear the other request's deadline", func(t *testing.T) {
		// The peer answers the request for w1:p1 only: the request for w1:p2 has
		// nothing to read, so only its own timer can bring the caller back. Keying
		// the answer on the request's own pane id (rather than on arrival order)
		// keeps which goroutine is answered deterministic.
		server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
			params, _ := request["params"].(map[string]any)
			if paneID, _ := params["pane_id"].(string); paneID != "w1:p1" {
				return
			}
			writeJSONLine(t, conn, map[string]any{"id": request["id"], "result": map[string]any{"pane_id": "w1:p1"}})
		})

		client, err := NewHerdrSocketClient(HerdrSocketClientOptions{
			RequestTimeoutMs: timeoutMs,
			SocketPath:       server.socketPath,
		})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		defer client.Close()

		// Both requests are in flight at the same time, so the first one's
		// completion cannot be the only thing that ends the second one.
		type outcome struct {
			value any
			err   error
		}
		first := make(chan outcome, 1)
		second := make(chan outcome, 1)
		go func() {
			value, err := client.GetPane(map[string]any{"pane_id": "w1:p1"})
			first <- outcome{value: value, err: err}
		}()
		go func() {
			value, err := client.GetPane(map[string]any{"pane_id": "w1:p2"})
			second <- outcome{value: value, err: err}
		}()

		select {
		case got := <-first:
			if got.err != nil {
				t.Fatalf("first getPane: %v", got.err)
			}
			pane, ok := got.value.(map[string]any)
			if !ok || pane["pane_id"] != "w1:p1" {
				t.Fatalf("first getPane result = %v, want the answered pane", got.value)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the answered request never settled")
		}

		// The second request must settle on its own timer. If the first request's
		// completion cleared the shared deadline, this read would block for the OS
		// socket timeout instead, which is minutes rather than the budget.
		started := time.Now()
		select {
		case got := <-second:
			if got.err == nil || !strings.Contains(got.err.Error(), "timed out") {
				t.Fatalf("second getPane err = %v, want a request timeout", got.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the unanswered request never settled: its deadline was cleared")
		}
		if elapsed := time.Since(started); elapsed > budget*4 {
			t.Fatalf("the unanswered request took %v, want it bounded by the %v budget", elapsed, budget)
		}
	})

	t.Run("the connection survives a timed-out request", func(t *testing.T) {
		// The peer stays silent on the first request and answers the rest, so the
		// reader must survive a timeout that no longer touches the socket
		// deadline.
		var mu sync.Mutex
		answered := 0
		server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
			mu.Lock()
			answered++
			count := answered
			mu.Unlock()
			if count == 1 {
				return
			}
			writeJSONLine(t, conn, map[string]any{"id": request["id"], "result": map[string]any{"pane_id": "w1:p1"}})
		})

		client, err := NewHerdrSocketClient(HerdrSocketClientOptions{
			RequestTimeoutMs: timeoutMs,
			SocketPath:       server.socketPath,
		})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		defer client.Close()

		if _, err := client.GetPane(map[string]any{"pane_id": "w1:p1"}); err == nil {
			t.Fatal("first getPane = nil error, want a timeout")
		}
		result, err := client.GetPane(map[string]any{"pane_id": "w1:p1"})
		if err != nil {
			t.Fatalf("second getPane after a timeout: %v", err)
		}
		pane, ok := result.(map[string]any)
		if !ok || pane["pane_id"] != "w1:p1" {
			t.Fatalf("second getPane result = %v, want the answered pane", result)
		}
	})

	t.Run("a failed write resets the connection instead of reusing it", func(t *testing.T) {
		// The peer accepts the constructor's dial and closes the connection
		// without ever reading a frame, so the client's first write lands on a
		// closed socket. A partial frame left on a reused connection would
		// desynchronise the peer's JSON-line framing forever, so the write failure
		// has to tear the connection down and report the client as closed.
		socketPath := filepath.Join(t.TempDir(), "herdr.sock")
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}()

		client, err := NewHerdrSocketClient(HerdrSocketClientOptions{
			RequestTimeoutMs: timeoutMs,
			SocketPath:       socketPath,
		})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		defer client.Close()

		// Let the peer's close reach this side before the first request, so the
		// write is the operation that fails.
		time.Sleep(50 * time.Millisecond)

		_, firstErr := client.GetPane(map[string]any{"pane_id": "w1:p1"})
		if firstErr == nil {
			t.Fatal("getPane on a closed peer = nil error, want a write failure")
		}
		if !strings.Contains(firstErr.Error(), "closed network connection") {
			t.Fatalf("first getPane err = %v, want the write to fail on the closed socket", firstErr)
		}

		// The connection is gone, so the next request must be refused as such
		// rather than attempting another write onto the dead socket.
		if _, err := client.GetPane(map[string]any{"pane_id": "w1:p1"}); err == nil ||
			!strings.Contains(err.Error(), "Herdr socket closed") {
			t.Fatalf("getPane after a failed write err = %v, want Herdr socket closed", err)
		}
	})
}

func TestHerdrSocketClientRejectsEventStreamOnSocketClose(t *testing.T) {
	server := openFakeHerdrServer(t, func(conn net.Conn, request map[string]any) {
		writeJSONLine(t, conn, map[string]any{"id": request["id"], "result": map[string]any{"subscribed": true}})
		_ = conn.Close()
	})

	client, err := NewHerdrSocketClient(HerdrSocketClientOptions{SocketPath: server.socketPath})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.SubscribeEvents(ctx, nil)
	if err != nil {
		t.Fatalf("subscribeEvents: %v", err)
	}

	type nextResult struct {
		value any
		done  bool
		err   error
	}
	nextCh := make(chan nextResult, 1)
	go func() {
		value, done, err := stream.Next()
		nextCh <- nextResult{value: value, done: done, err: err}
	}()
	var res nextResult
	select {
	case res = <-nextCh:
	case <-time.After(2 * time.Second):
		t.Fatal("event stream did not close")
	}
	if res.err == nil || !strings.Contains(res.err.Error(), "Herdr socket closed") {
		t.Fatalf("stream err = %v, want Herdr socket closed", res.err)
	}
}
