package shared

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// DefaultMaxFrameBytes mirrors the TypeScript JsonLineDecoder default (1 MiB).
const DefaultMaxFrameBytes = 1024 * 1024

// ErrJSONLineNotSerializable mirrors the TS TypeError raised by encodeJsonLine
// when JSON.stringify returns undefined (e.g. bare undefined).
var ErrJSONLineNotSerializable = errors.New("JSON Lines values must be JSON-serializable")

// ErrInvalidMaxFrameBytes mirrors the TS RangeError message verbatim.
var ErrInvalidMaxFrameBytes = errors.New("JSON Lines maximum frame size must be a positive integer")

// JsonLineFrameTooLargeError mirrors the TS class of the same name, including
// the exact message text.
type JsonLineFrameTooLargeError struct {
	MaxBytes int
}

func (e *JsonLineFrameTooLargeError) Error() string {
	return fmt.Sprintf("JSON Lines frame exceeds maximum size of %d bytes", e.MaxBytes)
}

// EncodeJSONLine encodes one JSON value as a newline-terminated frame.
//
// Mirrors `encodeJsonLine(value)`: the encoded output always ends with "\n".
// Values that encoding/json cannot marshal (funcs, chans, complex, NaN/Inf)
// have no JSON.stringify counterpart and map to ErrJSONLineNotSerializable.
func EncodeJSONLine(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", ErrJSONLineNotSerializable
	}
	return string(encoded) + "\n", nil
}

// JsonLineDecoder mirrors the TS JsonLineDecoder: it accumulates pushed chunks
// and yields parsed frames, enforcing a UTF-8 byte frame limit.
type JsonLineDecoder struct {
	buffer   string
	maxBytes int
}

// NewJSONLineDecoder builds a decoder. maxBytes <= 0 mirrors the TS RangeError.
// Use DefaultMaxFrameBytes for the default limit.
func NewJSONLineDecoder(maxBytes int) (*JsonLineDecoder, error) {
	if maxBytes <= 0 {
		return nil, ErrInvalidMaxFrameBytes
	}
	return &JsonLineDecoder{maxBytes: maxBytes}, nil
}

// Push appends one chunk and returns every complete frame it contains.
func (d *JsonLineDecoder) Push(chunk string) ([]any, error) {
	d.buffer += chunk
	if len(d.buffer) > d.maxBytes {
		return nil, &JsonLineFrameTooLargeError{MaxBytes: d.maxBytes}
	}

	lines := splitLines(d.buffer)
	// The final element after the last "\n" is the incomplete remainder.
	remainder := lines[len(lines)-1]
	complete := lines[:len(lines)-1]
	d.buffer = remainder

	out := make([]any, 0, len(complete))
	for _, line := range complete {
		// Empty lines are skipped, matching `lines.filter((line) => line.length > 0)`.
		if len(line) == 0 {
			continue
		}
		value, err := parseFrame(line)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// Flush returns the trailing frame that had no trailing newline.
func (d *JsonLineDecoder) Flush() ([]any, error) {
	if len(d.buffer) == 0 {
		return []any{}, nil
	}
	line := d.buffer
	d.buffer = ""
	value, err := parseFrame(line)
	if err != nil {
		return nil, err
	}
	return []any{value}, nil
}

// BufferedByteLength exposes the current UTF-8 byte size of the pending buffer.
func (d *JsonLineDecoder) BufferedByteLength() int { return len(d.buffer) }

// splitLines splits on "\n" exactly like String.prototype.split("\n"): a
// trailing newline yields a trailing empty element, and "" yields [""].
func splitLines(s string) []string {
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// parseFrame decodes one frame. TS feeds JSON.parse a JS string, so invalid
// UTF-8 bytes were already replaced by U+FFFD on ingress; replicate that so
// frame acceptance matches the TS original byte-for-byte.
func parseFrame(line string) (any, error) {
	data := []byte(line)
	if !utf8.Valid(data) {
		data = []byte(string([]rune(line)))
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("json-lines: decode frame: %w", err)
	}
	return value, nil
}
