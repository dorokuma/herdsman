package herdr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/dorokuma/herdsman/go/internal/shared"
)

// This file ports src/herdr/socket-client.ts.

// HerdrRequestTimeoutMs mirrors HERDR_REQUEST_TIMEOUT_MS. Local Herdr sockets
// answer immediately, so a request still unanswered after this window is treated
// as failed. Without it a peer that stops replying (or a socket that accepts but
// never writes) parks the caller forever, which is how the daemon used to hang
// past the unit's TimeoutStopSec on shutdown.
const HerdrRequestTimeoutMs = 2_000

// HerdrTopologyEventTypes mirrors HERDR_TOPOLOGY_EVENT_TYPES: session-wide
// topology events that must be subscribed even when no pane ids are known yet.
var HerdrTopologyEventTypes = []string{
	"pane.created",
	"pane.closed",
	"pane.moved",
	"pane.agent_detected",
	"workspace.closed",
}

var errHerdrSocketClosed = errors.New("Herdr socket closed")

// HerdrSocketClientOptions mirrors HerdrSocketClientOptions. A non-positive
// RequestTimeoutMs falls back to HerdrRequestTimeoutMs, matching
// `options.requestTimeoutMs ?? HERDR_REQUEST_TIMEOUT_MS` for the omitted case.
type HerdrSocketClientOptions struct {
	RequestTimeoutMs int
	SocketPath       string
}

// HerdrMethodUnsupportedError reports that the Herdr peer does not implement a
// method. Herdr surfaces an unimplemented method as a serde "unknown variant"
// error (e.g. `session.snapshot` on an older protocol); the socket client
// recognizes it to fall back from session.snapshot to the per-resource list APIs
// instead of surfacing the failure. This is the herdr-side analogue of the
// "unknown method incompatibility" regression: an unsupported method is a
// per-request outcome, never a reason to tear the connection down.
type HerdrMethodUnsupportedError struct {
	Method  string
	Message string
}

func (e *HerdrMethodUnsupportedError) Error() string { return e.Message }

// herdrOutcome carries a settled request result (value or error) over a channel.
type herdrOutcome struct {
	value any
	err   error
}

// herdrPending tracks one in-flight request on the persistent socket.
type herdrPending struct {
	method string
	ch     chan herdrOutcome
}

// eventSubscriber is one active subscribeEvents stream. It is written by the
// socket reader goroutine (push/fail) and drained by the consumer (Next), so it
// guards its own queue and failure with a mutex.
type eventSubscriber struct {
	queue   []any
	failure error
	mu      sync.Mutex
	wake    chan struct{}
}

func newEventSubscriber() *eventSubscriber {
	return &eventSubscriber{wake: make(chan struct{}, 1)}
}

// pushBatch appends every event from one socket chunk and signals once, so a
// consumer woken by the first event already sees the rest of the chunk queued
// (the drain-after-abort path must not drop a same-chunk event).
func (s *eventSubscriber) pushBatch(events []any) {
	if len(events) == 0 {
		return
	}
	s.mu.Lock()
	s.queue = append(s.queue, events...)
	s.mu.Unlock()
	s.signal()
}

// fail records the first stream failure and signals a waiter, mirroring the TS
// subscriber's `failure ??= error`.
func (s *eventSubscriber) fail(err error) {
	s.mu.Lock()
	if s.failure == nil {
		s.failure = err
	}
	s.mu.Unlock()
	s.signal()
}

func (s *eventSubscriber) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// HerdrSocketClient mirrors the TS HerdrSocketClient.
type HerdrSocketClient struct {
	decoder          *shared.JsonLineDecoder
	pending          map[string]*herdrPending
	subscribers      map[*eventSubscriber]struct{}
	requestTimeoutMs int
	conn             net.Conn
	socketPath       string
	mu               sync.Mutex
	writeMu          sync.Mutex
	eventsSubscribed bool
	closed           bool
	nextID           int
}

// NewHerdrSocketClient mirrors the TS constructor: it connects to the Herdr
// socket and wires the data/error/close handlers onto a reader goroutine. A dial
// failure is returned here (the TS constructor surfaces it asynchronously via
// the socket "error" event).
func NewHerdrSocketClient(options HerdrSocketClientOptions) (*HerdrSocketClient, error) {
	requestTimeoutMs := HerdrRequestTimeoutMs
	if options.RequestTimeoutMs > 0 {
		requestTimeoutMs = options.RequestTimeoutMs
	}
	conn, err := net.Dial("unix", options.SocketPath)
	if err != nil {
		return nil, err
	}
	decoder, err := shared.NewJSONLineDecoder(shared.DefaultMaxFrameBytes)
	if err != nil {
		conn.Close()
		return nil, err
	}
	client := &HerdrSocketClient{
		decoder:          decoder,
		pending:          make(map[string]*herdrPending),
		subscribers:      make(map[*eventSubscriber]struct{}),
		requestTimeoutMs: requestTimeoutMs,
		conn:             conn,
		socketPath:       options.SocketPath,
		nextID:           1,
	}
	go client.readLoop()
	return client, nil
}

// Close mirrors close(): it destroys the socket, which rejects every pending
// request with "Herdr socket closed".
func (c *HerdrSocketClient) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.rejectAll(errHerdrSocketClosed)
	c.conn.Close()
}

// readLoop mirrors the socket "data"/"error"/"close" handlers.
func (c *HerdrSocketClient) readLoop() {
	defer c.conn.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := c.conn.Read(buf)
		if n > 0 {
			c.handleData(string(buf[:n]))
		}
		if err != nil {
			// net.ErrClosed is our own Close(); Close already rejected everything.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A clean peer close is the "close" event ("Herdr socket closed");
			// any other read error is the "error" event carrying the error.
			if errors.Is(err, io.EOF) {
				c.rejectAll(errHerdrSocketClosed)
			} else {
				c.rejectAll(err)
			}
			return
		}
	}
}

// handleData mirrors #handleData: frames with an id resolve a pending request;
// frames without one are notifications, batched per chunk and published to every
// subscriber.
func (c *HerdrSocketClient) handleData(chunk string) {
	frames, err := c.decoder.Push(chunk)
	if err != nil {
		c.rejectAll(err)
		return
	}
	var events []any
	for _, message := range frames {
		response, ok := message.(map[string]any)
		if !ok {
			continue
		}
		id, hasID := response["id"].(string)
		if !hasID || id == "" {
			events = append(events, notificationEvent(response))
			continue
		}
		c.routeResponse(id, response)
	}
	if len(events) == 0 {
		return
	}
	c.mu.Lock()
	subscribers := make([]*eventSubscriber, 0, len(c.subscribers))
	for subscriber := range c.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	c.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.pushBatch(events)
	}
}

// routeResponse settles the pending request matching id, if it is still tracked.
func (c *HerdrSocketClient) routeResponse(id string, response map[string]any) {
	c.mu.Lock()
	pending, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if message, isErr := herdrResponseError(response); isErr {
		pending.ch <- herdrOutcome{err: c.newHerdrError(pending.method, message)}
		return
	}
	pending.ch <- herdrOutcome{value: response["result"]}
}

// rejectAll mirrors #rejectAll: it fails every pending request and every active
// subscriber, then clears the pending map (subscribers remove themselves when
// their stream finishes).
func (c *HerdrSocketClient) rejectAll(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]*herdrPending)
	subscribers := make([]*eventSubscriber, 0, len(c.subscribers))
	for subscriber := range c.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	c.mu.Unlock()
	for _, request := range pending {
		select {
		case request.ch <- herdrOutcome{err: err}:
		default:
		}
	}
	for _, subscriber := range subscribers {
		subscriber.fail(err)
	}
}

func (c *HerdrSocketClient) removeSubscriber(subscriber *eventSubscriber) {
	c.mu.Lock()
	delete(c.subscribers, subscriber)
	c.mu.Unlock()
}

// deletePending removes id and reports whether it was still tracked, so a late
// response and the request timeout race to settle a request exactly once.
func (c *HerdrSocketClient) deletePending(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	return ok
}

func (c *HerdrSocketClient) removePending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *HerdrSocketClient) writeFrame(frame string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.conn.Write([]byte(frame))
	return err
}

func (c *HerdrSocketClient) nextRequestID() string {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.mu.Unlock()
	return fmt.Sprintf("herdsman-%d", id)
}

// request mirrors #request on the persistent socket: it assigns the next id,
// registers a pending slot, writes the frame, and settles on the first of
// (response, timeout, abort). A late response after a timeout/abort finds no
// pending slot and is ignored.
func (c *HerdrSocketClient) request(ctx context.Context, method string, params any) (any, error) {
	if ctx.Err() != nil {
		return nil, errors.New("Herdr request aborted")
	}
	id := c.nextRequestID()
	ch := make(chan herdrOutcome, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errHerdrSocketClosed
	}
	c.pending[id] = &herdrPending{method: method, ch: ch}
	c.mu.Unlock()

	timer := time.AfterFunc(time.Duration(c.requestTimeoutMs)*time.Millisecond, func() {
		if c.deletePending(id) {
			ch <- herdrOutcome{err: fmt.Errorf("Herdr %s request timed out after %dms", method, c.requestTimeoutMs)}
		}
	})

	frame, err := shared.EncodeJSONLine(map[string]any{"id": id, "method": method, "params": paramsOrEmpty(params)})
	if err != nil {
		timer.Stop()
		c.removePending(id)
		return nil, err
	}
	if err := c.writeFrame(frame); err != nil {
		timer.Stop()
		c.removePending(id)
		return nil, err
	}

	select {
	case outcome := <-ch:
		timer.Stop()
		return outcome.value, outcome.err
	case <-ctx.Done():
		timer.Stop()
		c.removePending(id)
		return nil, errors.New("Herdr request aborted")
	}
}

// requestOnce mirrors #requestOnce: a fresh one-shot connection per call, so a
// wedged peer on the persistent socket cannot block these fallback reads. It
// settles on the first of (matching response, error response, timeout, close).
func (c *HerdrSocketClient) requestOnce(method string, params any) (any, error) {
	id := c.nextRequestID()
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	decoder, err := shared.NewJSONLineDecoder(shared.DefaultMaxFrameBytes)
	if err != nil {
		return nil, err
	}

	resultCh := make(chan herdrOutcome, 1)
	var once sync.Once
	finish := func(outcome herdrOutcome) {
		once.Do(func() { resultCh <- outcome })
	}

	frame, err := shared.EncodeJSONLine(map[string]any{"id": id, "method": method, "params": paramsOrEmpty(params)})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte(frame)); err != nil {
		return nil, err
	}

	timer := time.AfterFunc(time.Duration(c.requestTimeoutMs)*time.Millisecond, func() {
		finish(herdrOutcome{err: fmt.Errorf("Herdr %s request timed out after %dms", method, c.requestTimeoutMs)})
	})
	defer timer.Stop()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				frames, decodeErr := decoder.Push(string(buf[:n]))
				if decodeErr != nil {
					finish(herdrOutcome{err: decodeErr})
					return
				}
				for _, message := range frames {
					response, ok := message.(map[string]any)
					if !ok {
						continue
					}
					if msg, isErr := herdrResponseError(response); isErr {
						finish(herdrOutcome{err: c.newHerdrError(method, msg)})
						return
					}
					if responseID, ok := response["id"].(string); ok && responseID == id {
						finish(herdrOutcome{value: response["result"]})
						return
					}
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					finish(herdrOutcome{err: errHerdrSocketClosed})
				} else {
					finish(herdrOutcome{err: err})
				}
				return
			}
		}
	}()

	outcome := <-resultCh
	return outcome.value, outcome.err
}

// GetPane mirrors getPane(params).
func (c *HerdrSocketClient) GetPane(params map[string]any) (any, error) {
	return c.request(context.Background(), "pane.get", params)
}

// SessionSnapshot mirrors sessionSnapshot(): it tries session.snapshot once and,
// when the peer reports the method as an unsupported variant, falls back to the
// per-resource list APIs (run concurrently, matching Promise.all) and assembles
// the same snapshot shape.
func (c *HerdrSocketClient) SessionSnapshot() (any, error) {
	result, err := c.requestOnce("session.snapshot", nil)
	if err == nil {
		return result, nil
	}
	if !isUnsupportedSessionSnapshotError(err) {
		return nil, err
	}

	type listResult struct {
		value any
		err   error
	}
	methods := []string{"workspace.list", "pane.list", "tab.list", "agent.list"}
	results := make([]listResult, len(methods))
	var wg sync.WaitGroup
	for index, method := range methods {
		wg.Add(1)
		go func(index int, method string) {
			defer wg.Done()
			value, listErr := c.requestOnce(method, nil)
			results[index] = listResult{value: value, err: listErr}
		}(index, method)
	}
	wg.Wait()
	for _, listRes := range results {
		if listRes.err != nil {
			return nil, listRes.err
		}
	}

	workspaces := arrayProperty(results[0].value, "workspaces")
	panes := arrayProperty(results[1].value, "panes")
	tabs := arrayProperty(results[2].value, "tabs")
	agents := arrayProperty(results[3].value, "agents")

	snapshot := map[string]any{
		"agents":     agents,
		"panes":      panes,
		"tabs":       tabs,
		"workspaces": workspaces,
	}
	if id, ok := focusedID(panes, "pane_id"); ok {
		snapshot["focused_pane_id"] = id
	}
	if id, ok := focusedID(workspaces, "workspace_id"); ok {
		snapshot["focused_workspace_id"] = id
	}
	return map[string]any{"snapshot": snapshot}, nil
}

// SubscribeEvents mirrors subscribeEvents(params, options). It refuses a second
// subscription on the same connection, returns an immediately-finished stream
// when ctx is already cancelled (leaving the connection free to subscribe
// later), and otherwise registers a subscriber and issues events.subscribe.
//
// The returned stream is drained with Next. A subscribe failure that is not an
// abort is surfaced on the stream (the TS generator would throw); an abort
// during the subscribe request is swallowed so already-queued events still
// drain.
func (c *HerdrSocketClient) SubscribeEvents(ctx context.Context, paneIDs []string) (*HerdrEventStream, error) {
	c.mu.Lock()
	if c.eventsSubscribed {
		c.mu.Unlock()
		return nil, errors.New("Herdr connection already has an events.subscribe")
	}
	if c.closed {
		c.mu.Unlock()
		return nil, errHerdrSocketClosed
	}
	if ctx.Err() != nil {
		c.mu.Unlock()
		return &HerdrEventStream{sub: newEventSubscriber(), ctx: ctx, client: c, finished: true}, nil
	}
	c.eventsSubscribed = true
	sub := newEventSubscriber()
	c.subscribers[sub] = struct{}{}
	c.mu.Unlock()

	stream := &HerdrEventStream{sub: sub, ctx: ctx, client: c}

	subscriptions := buildSubscriptions(paneIDs)
	if _, err := c.request(ctx, "events.subscribe", map[string]any{"subscriptions": subscriptions}); err != nil && ctx.Err() == nil {
		c.removeSubscriber(sub)
		sub.fail(err)
	}
	return stream, nil
}

// HerdrEventStream is the Go form of the async iterator returned by
// subscribeEvents. Next yields one event, reports a stream failure, or reports
// done once the context is cancelled and every already-queued event has drained.
type HerdrEventStream struct {
	sub      *eventSubscriber
	ctx      context.Context
	client   *HerdrSocketClient
	finished bool
}

// Next mirrors one `iterator.next()`. It returns (event, false, nil) for a
// yielded event, (nil, false, err) when the stream failed, and (nil, true, nil)
// when the stream ended after draining. A failure is checked before the queue,
// and the queue before the abort, so a restart does not drop status already
// received for panes this connection subscribed to.
func (s *HerdrEventStream) Next() (any, bool, error) {
	if s.finished {
		return nil, true, nil
	}
	for {
		s.sub.mu.Lock()
		if s.sub.failure != nil {
			err := s.sub.failure
			s.sub.mu.Unlock()
			s.finish()
			return nil, false, err
		}
		if len(s.sub.queue) > 0 {
			event := s.sub.queue[0]
			s.sub.queue = s.sub.queue[1:]
			s.sub.mu.Unlock()
			return event, false, nil
		}
		s.sub.mu.Unlock()
		if s.ctx.Err() != nil {
			s.finish()
			return nil, true, nil
		}
		select {
		case <-s.sub.wake:
		case <-s.ctx.Done():
		}
	}
}

// finish removes the subscriber from its client (the TS generator's finally) and
// marks the stream ended, so a later Next reports done.
func (s *HerdrEventStream) finish() {
	s.finished = true
	if s.client != nil && s.sub != nil {
		s.client.removeSubscriber(s.sub)
	}
}

// buildSubscriptions mirrors the subscriptions array: the topology types first,
// then a pane.agent_status_changed subscription per pane id.
func buildSubscriptions(paneIDs []string) []any {
	subscriptions := make([]any, 0, len(HerdrTopologyEventTypes)+len(paneIDs))
	for _, eventType := range HerdrTopologyEventTypes {
		subscriptions = append(subscriptions, map[string]any{"type": eventType})
	}
	for _, paneID := range paneIDs {
		subscriptions = append(subscriptions, map[string]any{
			"pane_id": paneID,
			"type":    "pane.agent_status_changed",
		})
	}
	return subscriptions
}

// notificationEvent mirrors notificationEvent(message).
func notificationEvent(message map[string]any) any {
	if event, ok := message["event"].(string); ok {
		if data, ok := message["data"].(map[string]any); ok {
			return withEventType(data, event)
		}
	}
	if params, ok := message["params"].(map[string]any); ok {
		if nested, ok := params["event"]; ok && nested != nil {
			return notificationPayload(nested)
		}
		return notificationPayload(params)
	}
	if result, ok := message["result"]; ok && result != nil {
		return notificationPayload(result)
	}
	return notificationPayload(message)
}

// notificationPayload mirrors notificationPayload(value).
func notificationPayload(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	event, ok := object["event"].(string)
	if !ok {
		return value
	}
	data, ok := object["data"].(map[string]any)
	if !ok {
		return value
	}
	return withEventType(data, event)
}

// withEventType returns a copy of data with `type` set to the normalized event
// name, mirroring `{ ...data, type: normalizeEventName(event) }`.
func withEventType(data map[string]any, event string) map[string]any {
	result := make(map[string]any, len(data)+1)
	for key, value := range data {
		result[key] = value
	}
	result["type"] = normalizeEventName(event)
	return result
}

// normalizeEventName mirrors normalizeEventName(value): a dotted name is kept,
// otherwise the first underscore becomes a dot.
func normalizeEventName(value string) string {
	if strings.Contains(value, ".") {
		return value
	}
	return strings.Replace(value, "_", ".", 1)
}

// herdrResponseError mirrors the TS `if (response.error)` truthiness check and
// `response.error.message ?? "Herdr request failed"`. It returns (message, true)
// when the response carries a non-null error, else ("", false).
func herdrResponseError(response map[string]any) (string, bool) {
	errValue, ok := response["error"]
	if !ok || errValue == nil {
		return "", false
	}
	if errObject, ok := errValue.(map[string]any); ok {
		if message, ok := errObject["message"].(string); ok {
			return message, true
		}
	}
	return "Herdr request failed", true
}

// newHerdrError builds the error a failed request rejects with. A serde "unknown
// variant" message is tagged as *HerdrMethodUnsupportedError so callers can
// detect an unimplemented method; every other failure keeps the peer message.
func (c *HerdrSocketClient) newHerdrError(method, message string) error {
	if strings.Contains(message, "unknown variant") {
		return &HerdrMethodUnsupportedError{Method: method, Message: message}
	}
	return errors.New(message)
}

// isUnsupportedSessionSnapshotError mirrors isUnsupportedSessionSnapshotError:
// the session.snapshot request failed because the peer does not implement it.
func isUnsupportedSessionSnapshotError(err error) bool {
	var unsupported *HerdrMethodUnsupportedError
	if !errors.As(err, &unsupported) {
		return false
	}
	return strings.Contains(unsupported.Message, "session.snapshot") &&
		strings.Contains(unsupported.Message, "unknown variant")
}

// arrayProperty mirrors arrayProperty(value, key).
func arrayProperty(value any, key string) []any {
	object, ok := value.(map[string]any)
	if !ok {
		return []any{}
	}
	if array, ok := object[key].([]any); ok {
		return array
	}
	return []any{}
}

// focusedID mirrors focusedId(records, recordKey): it returns the recordKey of
// the first record marked focused, when that value is a string.
func focusedID(records []any, recordKey string) (string, bool) {
	for _, record := range records {
		object, ok := record.(map[string]any)
		if !ok {
			continue
		}
		if focused, ok := object["focused"].(bool); ok && focused {
			if id, ok := object[recordKey].(string); ok {
				return id, true
			}
			return "", false
		}
	}
	return "", false
}

func paramsOrEmpty(params any) any {
	if params == nil {
		return map[string]any{}
	}
	return params
}
