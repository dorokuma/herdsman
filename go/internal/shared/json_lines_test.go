package shared

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustDecoder(t *testing.T, maxBytes int) *JsonLineDecoder {
	t.Helper()
	dec, err := NewJSONLineDecoder(maxBytes)
	if err != nil {
		t.Fatalf("NewJSONLineDecoder(%d) error: %v", maxBytes, err)
	}
	return dec
}

func TestEncodeJSONLine(t *testing.T) {
	got, err := EncodeJSONLine(map[string]any{"id": 1, "method": "agent.events"})
	if err != nil {
		t.Fatalf("EncodeJSONLine error: %v", err)
	}
	// TS fixture: '{"id":1,"method":"agent.events"}\n'
	if want := "{\"id\":1,\"method\":\"agent.events\"}\n"; got != want {
		t.Fatalf("encode = %q, want %q", got, want)
	}
}

func TestEncodeJSONLineRejectsUnserializable(t *testing.T) {
	if _, err := EncodeJSONLine(func() {}); !errors.Is(err, ErrJSONLineNotSerializable) {
		t.Fatalf("err = %v, want ErrJSONLineNotSerializable", err)
	}
}

func TestJSONLineDecoderSplitsFramesAcrossChunks(t *testing.T) {
	dec := mustDecoder(t, DefaultMaxFrameBytes)

	got, err := dec.Push(`{"id":1`)
	if err != nil {
		t.Fatalf("push 1: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("push 1 frames = %v, want none", got)
	}

	got, err = dec.Push("}\n" + `{"id":2}` + "\n")
	if err != nil {
		t.Fatalf("push 2: %v", err)
	}
	if want := `[{"id":1},{"id":2}]`; len(got) != 2 {
		t.Fatalf("push 2 frames = %v, want 2", got)
	} else if encoded, _ := json.Marshal(got); string(encoded) != want {
		t.Fatalf("push 2 frames = %s, want %s", encoded, want)
	}
}

func TestJSONLineDecoderRejectsFrameAboveConfiguredLimit(t *testing.T) {
	dec := mustDecoder(t, 4)
	_, err := dec.Push("12345")
	var tooLarge *JsonLineFrameTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want JsonLineFrameTooLargeError", err)
	}
	if !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("message %q lacks %q", err.Error(), "maximum size")
	}
	if tooLarge.MaxBytes != 4 {
		t.Fatalf("MaxBytes = %d, want 4", tooLarge.MaxBytes)
	}
}

func TestJSONLineDecoderRejectsUnterminatedFrameAboveDefaultLimit(t *testing.T) {
	dec := mustDecoder(t, DefaultMaxFrameBytes)
	_, err := dec.Push(strings.Repeat("1", DefaultMaxFrameBytes+1))
	if !errors.As(err, new(*JsonLineFrameTooLargeError)) {
		t.Fatalf("err = %v, want JsonLineFrameTooLargeError", err)
	}
}

func TestJSONLineDecoderParsesFrameJustBelowDefaultLimit(t *testing.T) {
	dec := mustDecoder(t, DefaultMaxFrameBytes)
	value := strings.Repeat("a", DefaultMaxFrameBytes-20)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := dec.Push(string(encoded) + "\n")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("frames = %d, want 1", len(got))
	}
	if got[0] != value {
		t.Fatalf("decoded value mismatch (len %d)", len(got[0].(string)))
	}
}

func TestJSONLineDecoderRejectsFrameExceedingLimitAcrossChunks(t *testing.T) {
	dec := mustDecoder(t, DefaultMaxFrameBytes)
	if _, err := dec.Push(strings.Repeat("1", 512*1024)); err != nil {
		t.Fatalf("first chunk should fit: %v", err)
	}
	_, err := dec.Push(strings.Repeat("1", 512*1024+1))
	if !errors.As(err, new(*JsonLineFrameTooLargeError)) {
		t.Fatalf("err = %v, want JsonLineFrameTooLargeError", err)
	}
}

func TestJSONLineDecoderFlushesFinalFrame(t *testing.T) {
	dec := mustDecoder(t, DefaultMaxFrameBytes)
	if got, err := dec.Push(`{"id":1}`); err != nil || len(got) != 0 {
		t.Fatalf("push frames = %v err = %v, want none", got, err)
	}
	got, err := dec.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("flush frames = %v, want 1", got)
	}
}

func TestJSONLineDecoderRejectsInvalidMaxBytes(t *testing.T) {
	if _, err := NewJSONLineDecoder(0); !errors.Is(err, ErrInvalidMaxFrameBytes) {
		t.Fatalf("err = %v, want ErrInvalidMaxFrameBytes", err)
	}
	if _, err := NewJSONLineDecoder(-1); !errors.Is(err, ErrInvalidMaxFrameBytes) {
		t.Fatalf("err = %v, want ErrInvalidMaxFrameBytes", err)
	}
}

// TestDecoderFrameLimitRejectsFrameLargerThanOneMiB ports the
// final-audit-regressions "decoder frame limit" case: a 1 MiB frame plus its
// newline must be rejected because the byte count exceeds the limit.
func TestDecoderFrameLimitRejectsFrameLargerThanOneMiB(t *testing.T) {
	dec := mustDecoder(t, DefaultMaxFrameBytes)
	_, err := dec.Push(strings.Repeat("x", 1024*1024) + "\n")
	if err == nil {
		t.Fatal("expected rejection ov a 1 MiB + newline frame")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("message %q lacks %q", err.Error(), "exceeds maximum size")
	}
}
