package daemon_test

// The slow-reader half of the observability-server concurrency requirement: a
// peer that stops reading must not stall the daemon.
//
// TS runs every publish behind `await socket.write`, so a socket whose buffer is
// full only slows its own promise. Go's net.Conn.Write blocks instead, so the
// port writes frames with the server lock released and gives each write a
// deadline. Without both, one wedged client parks the RPC dispatch and the
// shutdown sequence on a socket nobody drains.

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/daemon"
	"github.com/dorokuma/herdsman/go/internal/observability"
	"github.com/dorokuma/herdsman/go/internal/shared"
)

// obsWriteTimeoutCeiling mirrors the server's per-frame write deadline. A
// publish path that holds the server lock across a socket write makes every
// other RPC wait out most of that deadline, so the assertions below bound the
// wait at half of it: a healthy local-UDS RPC answers in well under a
// millisecond, so anything approaching the write timeout is the lock being held.
const obsWriteTimeoutCeiling = 1 * time.Second

// obsNonReadingClient dials the socket and never reads from it, so the kernel
// receive buffer fills and a write to it blocks until the write deadline. It can
// still send its own requests; it only refuses to drain what the server sends
// back.
type obsNonReadingClient struct {
	conn net.Conn
}

func dialNonReadingClient(t *testing.T, socketPath string) *obsNonReadingClient {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial %s: %v", socketPath, err)
	}
	// Shrink the receive buffer so a handful of pushed frames is enough to fill
	// it; the default is far larger than the payloads a test produces.
	if unix, ok := conn.(*net.UnixConn); ok {
		_ = unix.SetReadBuffer(1024)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &obsNonReadingClient{conn: conn}
}

// request writes one JSON-line frame and returns the first response line that
// carries the matching id, draining any notification that precedes it.
func (c *obsNonReadingClient) request(t *testing.T, method string, params any, id string) map[string]any {
	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"id":     id,
		"method": method,
		"params": params,
	})
	if err != nil {
		t.Fatalf("marshal %s: %v", method, err)
	}
	if _, err := c.conn.Write(append(frame, '\n')); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}

	type readResult struct {
		message map[string]any
		err     error
	}
	done := make(chan readResult, 1)
	go func() {
		decoder, err := shared.NewJSONLineDecoder(shared.DefaultMaxFrameBytes)
		if err != nil {
			done <- readResult{err: err}
			return
		}
		buffer := make([]byte, 32*1024)
		for {
			read, readErr := c.conn.Read(buffer)
			if read > 0 {
				frames, pushErr := decoder.Push(string(buffer[:read]))
				if pushErr != nil {
					done <- readResult{err: pushErr}
					return
				}
				for _, decoded := range frames {
					message, ok := decoded.(map[string]any)
					if !ok {
						continue
					}
					if message["id"] != id {
						// A notification pushed before the response: skip it.
						continue
					}
					done <- readResult{message: message}
					return
				}
			}
			if readErr != nil {
				done <- readResult{err: readErr}
				return
			}
		}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("read %s response: %v", method, result.err)
		}
		return result.message
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was never answered", method)
		return nil
	}
}

// obsClaimOrchestrator registers a presence on paneID and claims ownership, so
// the server has a route that pushes to this connection.
func obsClaimOrchestrator(t *testing.T, client *obsNonReadingClient, paneID string) {
	t.Helper()
	client.request(t, "agent.orchestrator.register", obsRegisterParams(paneID), "register")
	client.request(t, "agent.orchestrator.set", map[string]any{"enabled": true}, "claim")
}

// obsSlowClientFrameBytes is the size of one pushed agent.event payload. The
// Linux AF_UNIX default send buffer is ~212 KB, and the buffer only grows to a
// few hundred KB with the peer's receive buffer, so a payload of a handful of
// bytes (the pre-3D shape: 64 frames of ~344 B = 22 KB) never fills it and the
// server's Write returns immediately. A 48 KB body forces the kernel to accept
// the first few frames and then park a Write on sndbuf.
const obsSlowClientFrameBytes = 48 * 1024

// obsSlowClientFloodCount is the number of frames the flood holds. With a
// 48 KB payload each, the first frames fill the peer's receive buffer and the
// kernel send buffer and the rest stay queued, so the publish path spends its
// time inside a blocked Write instead of finishing the replay.
const obsSlowClientFloodCount = 342

// fillSlowClientBuffer pushes frames at the wedged peer until its socket write
// has to block, so the server is inside a write that cannot drain. The events
// come from term_b, which is never the owner, so every one of them is routed to
// the owner's socket.
//
// The events are appended up front so the goroutine that replays them touches
// nothing but the server: a helper that called t.Fatalf after the test had
// already finished would panic the run.
func fillSlowClientBuffer(
	t *testing.T,
	_ *daemon.ObservabilityRpcServer,
	harness *obsHarness,
) []observability.AgentEventRecord {
	t.Helper()
	body := strings.Repeat("s", obsSlowClientFrameBytes)
	events := make([]observability.AgentEventRecord, 0, obsSlowClientFloodCount)
	for index := 0; index < obsSlowClientFloodCount; index++ {
		event := obsAppendRoutedEvent(t, harness, "term_b", obsWorkspaceB)
		event.Payload = map[string]any{"body": body}
		events = append(events, event)
	}
	return events
}

// replayEvents pushes the pre-built events at the server until stop is closed.
// It is safe to run from a goroutine that outlives the test body, because it
// never touches *testing.T.
func replayEvents(server *daemon.ObservabilityRpcServer, events []observability.AgentEventRecord, stop <-chan struct{}) {
	for _, event := range events {
		select {
		case <-stop:
			return
		default:
		}
		server.PublishAgentEvent(event)
	}
}

// TestObservabilityRpcSlowClientDoesNotBlockOtherClients is the regression test
// for the publish path: a client that stopped reading must not stop the daemon
// from answering another client's RPC.
//
// The wedged peer's write is deliberately left in flight while the healthy
// client's ping is issued, and the ping is required to come back well inside the
// write timeout. Holding the server lock across the write — the shape this port
// inherited from the TS critical section — makes the ping wait out that whole
// timeout.
func TestObservabilityRpcSlowClientDoesNotBlockOtherClients(t *testing.T) {
	fixture := obsOpenRPCServerWithoutClient(t, obsRPCServerOptions{})
	obsSeedRoutingAgents(t, fixture.harness)

	// The wedged peer owns workspace wB, so every worker event is pushed to it.
	slow := dialNonReadingClient(t, fixture.socketPath)
	obsClaimOrchestrator(t, slow, "wB:p-a")
	flood := fillSlowClientBuffer(t, fixture.server, fixture.harness)

	// A healthy client on another pane, registered before the flood starts.
	reader := obsDial(t, fixture.socketPath)
	obsRegisterClient(t, reader, "wB:p-b", "pi-b", obsWorkspaceB)

	flooding := make(chan struct{})
	stopFlood := make(chan struct{})
	defer func() {
		close(stopFlood)
		<-flooding
	}()
	go func() {
		defer close(flooding)
		replayEvents(fixture.server, flood, stopFlood)
	}()

	// Wait until the wedged peer's socket buffer is certainly full and the server
	// is parked inside a write that cannot drain. Without this settle the ping can
	// slip in before the first blocking write, and the test would pass against the
	// very shape it is meant to catch.
	time.Sleep(300 * time.Millisecond)

	started := time.Now()
	if _, err := reader.request("agent.ping", map[string]any{}); err != nil {
		t.Fatalf("agent.ping on a healthy client: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed >= obsWriteTimeoutCeiling {
		t.Fatalf(
			"agent.ping took %v, which is not faster than the %v write timeout: "+
				"the publish path is holding the server lock across a socket write",
			elapsed, obsWriteTimeoutCeiling)
	}
}

// TestObservabilityRpcSlowClientDoesNotBlockShutdown is the companion case: the
// same wedged peer must not keep server.Stop() from returning either. Stop is
// issued while the flood is still in flight and must not wait out the write
// timeout.
func TestObservabilityRpcSlowClientDoesNotBlockShutdown(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "rpc.sock")
	harness := openObsHarness(t)
	server := daemon.NewObservabilityRpcServer(daemon.ObservabilityRpcServerOptions{
		Context:      obsRPCContextService(harness, newObsHistory(harness, dir)),
		History:      newObsHistory(harness, dir),
		Orchestrator: harness.orchestrator(),
		SocketPath:   socketPath,
		Stores:       obsRPCStores(harness),
	})
	if err := server.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	slow := dialNonReadingClient(t, socketPath)
	obsSeedRoutingAgents(t, harness)
	obsClaimOrchestrator(t, slow, "wB:p-a")
	flood := fillSlowClientBuffer(t, server, harness)

	flooding := make(chan struct{})
	stopFlood := make(chan struct{})
	defer func() {
		close(stopFlood)
		<-flooding
	}()
	go func() {
		defer close(flooding)
		replayEvents(server, flood, stopFlood)
	}()

	// Same settle as the RPC case: the server must already be parked in a write
	// that cannot drain before the shutdown is requested.
	time.Sleep(300 * time.Millisecond)

	started := time.Now()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server.Stop was blocked by the non-reading client's socket write")
	}
	if elapsed := time.Since(started); elapsed >= obsWriteTimeoutCeiling {
		t.Fatalf(
			"server.Stop took %v, which is not faster than the %v write timeout: "+
				"the publish path is holding the server lock across a socket write",
			elapsed, obsWriteTimeoutCeiling)
	}
}
