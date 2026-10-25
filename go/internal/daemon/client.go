package daemon

import (
	"errors"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/dorokuma/herdsman/go/internal/shared"
)

// This file ports src/daemon/client.ts.

var errObservabilitySocketClosed = errors.New("Observability RPC socket closed")

// ObservabilityRpcError mirrors the TS Error augmented with the optional `code`
// and `retryable` fields copied from the RPC error object. They are pointers so
// an absent field stays absent (TS only assigns them when the peer sent them).
type ObservabilityRpcError struct {
	Message   string
	Code      *string
	Retryable *bool
}

func (e *ObservabilityRpcError) Error() string { return e.Message }

// ObservabilityRpcClientOptions mirrors the constructor options.
type ObservabilityRpcClientOptions struct {
	OnNotification func(method string, params any)
	SocketPath     string
}

// observabilityOutcome carries a settled request result over a channel.
type observabilityOutcome struct {
	value any
	err   error
}

// ObservabilityRpcClient mirrors the TS ObservabilityRpcClient.
type ObservabilityRpcClient struct {
	decoder        *shared.JsonLineDecoder
	pending        map[string]chan observabilityOutcome
	onNotification func(method string, params any)
	conn           net.Conn
	mu             sync.Mutex
	writeMu        sync.Mutex
	closed         bool
	nextID         int
}

// NewObservabilityRpcClient mirrors the constructor: it connects to the socket
// and wires the data/error/close handlers onto a reader goroutine. A dial
// failure is returned here (the TS constructor surfaces it asynchronously).
func NewObservabilityRpcClient(options ObservabilityRpcClientOptions) (*ObservabilityRpcClient, error) {
	conn, err := net.Dial("unix", options.SocketPath)
	if err != nil {
		return nil, err
	}
	decoder, err := shared.NewJSONLineDecoder(shared.DefaultMaxFrameBytes)
	if err != nil {
		conn.Close()
		return nil, err
	}
	client := &ObservabilityRpcClient{
		decoder:        decoder,
		pending:        make(map[string]chan observabilityOutcome),
		onNotification: options.OnNotification,
		conn:           conn,
		nextID:         1,
	}
	go client.readLoop()
	return client, nil
}

// Close mirrors close(): it destroys the socket, which rejects every pending
// request with "Observability RPC socket closed".
func (c *ObservabilityRpcClient) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.rejectAll(errObservabilitySocketClosed)
	c.conn.Close()
}

// Call mirrors request(method, params): it assigns the next numeric id, writes
// the frame, and settles when the matching response (or a socket error/close)
// arrives. The id is matched as its string form, exactly like `String(id)`.
//
// Note: the TS request has no per-request timeout — a pending call is settled
// only by a response or by rejectAll on socket error/close — so this port keeps
// the same contract rather than inventing a deadline.
func (c *ObservabilityRpcClient) Call(method string, params any) (any, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errObservabilitySocketClosed
	}
	id := c.nextID
	c.nextID++
	key := strconv.Itoa(id)
	ch := make(chan observabilityOutcome, 1)
	c.pending[key] = ch
	c.mu.Unlock()

	frame, err := shared.EncodeJSONLine(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		c.removePending(key)
		return nil, err
	}
	if err := c.writeFrame(frame); err != nil {
		c.removePending(key)
		return nil, err
	}

	outcome := <-ch
	return outcome.value, outcome.err
}

// readLoop mirrors the socket "data"/"error"/"close" handlers.
func (c *ObservabilityRpcClient) readLoop() {
	defer c.conn.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := c.conn.Read(buf)
		if n > 0 {
			c.handleData(string(buf[:n]))
		}
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if errors.Is(err, io.EOF) {
				c.rejectAll(errObservabilitySocketClosed)
			} else {
				c.rejectAll(err)
			}
			return
		}
	}
}

// handleData mirrors #handleData: a frame with an id settles the matching
// pending call; a frame without one is a notification dispatched to
// onNotification when it carries a method.
func (c *ObservabilityRpcClient) handleData(chunk string) {
	frames, err := c.decoder.Push(chunk)
	if err != nil {
		c.rejectAll(err)
		return
	}
	for _, message := range frames {
		response, ok := message.(map[string]any)
		if !ok {
			continue
		}
		rawID, hasID := response["id"]
		if !hasID {
			if method, ok := response["method"].(string); ok && method != "" && c.onNotification != nil {
				c.onNotification(method, response["params"])
			}
			continue
		}
		key, ok := observabilityIDKey(rawID)
		if !ok {
			continue
		}
		c.routeResponse(key, response)
	}
}

// routeResponse settles the pending call matching key, if it is still tracked.
func (c *ObservabilityRpcClient) routeResponse(key string, response map[string]any) {
	c.mu.Lock()
	ch, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if message, code, retryable, isErr := observabilityResponseError(response); isErr {
		ch <- observabilityOutcome{err: &ObservabilityRpcError{Message: message, Code: code, Retryable: retryable}}
		return
	}
	ch <- observabilityOutcome{value: response["result"]}
}

// rejectAll mirrors #rejectAll: it fails every pending call and clears the map.
func (c *ObservabilityRpcClient) rejectAll(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan observabilityOutcome)
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- observabilityOutcome{err: err}:
		default:
		}
	}
}

func (c *ObservabilityRpcClient) removePending(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

func (c *ObservabilityRpcClient) writeFrame(frame string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.conn.Write([]byte(frame))
	return err
}

// observabilityIDKey renders a response id as its pending-map key, matching
// `String(response.id)` for the numeric ids this client sends (and passing a
// string id through unchanged).
func observabilityIDKey(value any) (string, bool) {
	switch typed := value.(type) {
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case string:
		return typed, true
	default:
		return "", false
	}
}

// observabilityResponseError mirrors the TS error branch: it extracts the
// message (defaulting to "Observability RPC failed") and the optional code and
// retryable fields, returning isErr=true when the response carries a non-null
// error.
func observabilityResponseError(response map[string]any) (message string, code *string, retryable *bool, isErr bool) {
	errValue, ok := response["error"]
	if !ok || errValue == nil {
		return "", nil, nil, false
	}
	message = "Observability RPC failed"
	if errObject, ok := errValue.(map[string]any); ok {
		if m, ok := errObject["message"].(string); ok {
			message = m
		}
		if codeValue, ok := errObject["code"].(string); ok {
			code = &codeValue
		}
		if retryableValue, ok := errObject["retryable"].(bool); ok {
			retryable = &retryableValue
		}
	}
	return message, code, retryable, true
}
