package agenthistory

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// Ported from test/unit/antigravity-proto.test.ts.
//
// The fixtures keep the tags 19/20/30 from real agy step blobs with the observed
// protobuf nesting, and stay deliberately tiny.

func hexBytes(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(strings.Join(strings.Fields(value), ""))
	if err != nil {
		t.Fatalf("decode hex %q: %v", value, err)
	}
	return decoded
}

func nestedField(field int, body []byte) []byte {
	tag := field<<3 | 2
	encoded := make([]byte, 0, 8)
	for tag > 127 {
		encoded = append(encoded, byte(tag&127)|128)
		tag >>= 7
	}
	encoded = append(encoded, byte(tag))
	encoded = append(encoded, encodeLength(len(body))...)
	return append(encoded, body...)
}

func encodeLength(length int) []byte {
	encoded := make([]byte, 0, 8)
	current := length
	for current >= 128 {
		encoded = append(encoded, byte(current&127)|128)
		current >>= 7
	}
	return append(encoded, byte(current))
}

var (
	realUser      = nestedField(19, nestedField(3, nestedField(1, []byte("plan list files in /tmp"))))
	realAssistant = nestedField(20, nestedField(8, []byte("Listing /tmp files")))
	realTool      = nestedField(30, nestedField(4, []byte("List Temporary Directory Files")))
)

func TestAntigravityProtoDecodesRealUserTurnField19(t *testing.T) {
	want := DecodedAntigravityMessage{Role: RoleUser, Text: "plan list files in /tmp"}
	if got, ok := DecodeAntigravityMessage(realUser); !ok || got != want {
		t.Fatalf("decodeAntigravityMessage(realUser) = (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

func TestAntigravityProtoDecodesRealAssistantTurnField20(t *testing.T) {
	want := DecodedAntigravityMessage{Role: RoleAssistant, Text: "Listing /tmp files"}
	if got, ok := DecodeAntigravityMessage(realAssistant); !ok || got != want {
		t.Fatalf("decodeAntigravityMessage(realAssistant) = (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

func TestAntigravityProtoDecodesRealToolStepField30(t *testing.T) {
	want := DecodedAntigravityMessage{Role: RoleToolResult, Text: "List Temporary Directory Files"}
	if got, ok := DecodeAntigravityMessage(realTool); !ok || got != want {
		t.Fatalf("decodeAntigravityMessage(realTool) = (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

func TestAntigravityProtoIgnoresThinkingOnlyField(t *testing.T) {
	thinkingOnly := nestedField(20, nestedField(3, []byte("thinking only...")))
	if got, ok := DecodeAntigravityMessage(thinkingOnly); ok {
		t.Fatalf("thinking-only blob decoded as %+v, want absent", got)
	}
}

func TestAntigravityProtoPrefersField20Sub8OverSub3(t *testing.T) {
	mixed := nestedField(20, concatBytes(
		nestedField(8, []byte("actual response")),
		nestedField(3, []byte("thinking...")),
	))
	want := DecodedAntigravityMessage{Role: RoleAssistant, Text: "actual response"}
	if got, ok := DecodeAntigravityMessage(mixed); !ok || got != want {
		t.Fatalf("decodeAntigravityMessage(mixed) = (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

func TestAntigravityProtoDecodesField20Sub1WhenAlone(t *testing.T) {
	field1Only := nestedField(20, nestedField(1, []byte("response at 1")))
	want := DecodedAntigravityMessage{Role: RoleAssistant, Text: "response at 1"}
	if got, ok := DecodeAntigravityMessage(field1Only); !ok || got != want {
		t.Fatalf("decodeAntigravityMessage(field1Only) = (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

func TestAntigravityProtoReturnsNothingForTruncatedAndMalformedBlobs(t *testing.T) {
	truncated := realUser[:len(realUser)-1]
	malformed := hexBytes(t, "9a81")
	cases := map[string]any{
		"truncated":    truncated,
		"bad varint":   malformed,
		"string input": "bad",
		"nil input":    nil,
	}
	for name, input := range cases {
		if got, ok := DecodeAntigravityMessage(input); ok {
			t.Errorf("%s: decodeAntigravityMessage = (%+v, true), want absent", name, got)
		}
	}
}

func TestAntigravityProtoUserTurnRequiresCleanUTF8(t *testing.T) {
	// A lone 0xFF byte becomes U+FFFD in Node's utf8 decoder, so the TS reader
	// rejects the field and falls through to the next path.
	invalid := nestedField(19, nestedField(2, []byte{'h', 0xff, 'i'}))
	if got, ok := DecodeAntigravityMessage(invalid); ok {
		t.Errorf("invalid utf8 decoded as %+v, want absent", got)
	}
	// A literal U+FFFD is rejected the same way.
	withReplacement := nestedField(19, nestedField(2, []byte("h\uFFFDi")))
	if got, ok := DecodeAntigravityMessage(withReplacement); ok {
		t.Errorf("U+FFFD text decoded as %+v, want absent", got)
	}
}

// TestAntigravityProtoRejectsOverflowingVarintTag is the regression for a tag
// varint whose 10th byte carries bits above bit 63: `2^64 + 154`. The TS reader
// accumulates into a BigInt, keeps 2^64 and rejects the field number; Go's
// uint64 dropped the high bit and truncated the tag to 154, i.e. a perfectly
// valid field 19 / wireType 2, so the very same blob decoded as a user turn.
//
// realUser starts with the two-byte tag `9a 01` (154 = field 19 << 3 | 2); the
// overflow blob swaps that for the ten-byte encoding of 2^64 + 154 and keeps
// the rest of the field intact, so a truncating reader would still find the
// nested text.
func TestAntigravityProtoRejectsOverflowingVarintTag(t *testing.T) {
	// The intact control case must keep decoding, otherwise the assertions below
	// would pass vacuously.
	if got, ok := DecodeAntigravityMessage(realUser); !ok || got.Role != RoleUser ||
		got.Text != "plan list files in /tmp" {
		t.Fatalf("realUser decoded as (%+v, %v), want the user turn", got, ok)
	}

	overflowTag := hexBytes(t, "9a818080808080808002")
	overflow := append(append([]byte{}, overflowTag...), realUser[2:]...)
	if got, ok := DecodeAntigravityMessage(overflow); ok {
		t.Fatalf("overflowing tag decoded as (%+v, true), want absent", got)
	}
	if fields := parseFields(overflow); fields != nil {
		t.Fatalf("parseFields(overflow) = %+v, want no fields", fields)
	}

	// A length varint that runs past the buffer must fail cleanly rather than
	// overflow `int(length)` negative and slice out of range.
	hugeLength := append(append([]byte{}, overflowTag[:2]...), hexBytes(t, "808080808080808001")...)
	hugeLength = append(hugeLength, []byte("x")...)
	if got, ok := DecodeAntigravityMessage(hugeLength); ok {
		t.Fatalf("huge length decoded as (%+v, true), want absent", got)
	}
}

func TestAntigravityProtoFallsBackThroughUserPaths(t *testing.T) {
	// [19,3,1] wins when present.
	both := nestedField(19, concatBytes(
		nestedField(3, nestedField(1, []byte("from 19.3.1"))),
		nestedField(2, []byte("from 19.2")),
	))
	if got, _ := DecodeAntigravityMessage(both); got.Text != "from 19.3.1" {
		t.Errorf("text = %q, want from 19.3.1", got.Text)
	}
	// [19,3,2] is used when [19,3,1] is absent.
	second := nestedField(19, concatBytes(
		nestedField(3, nestedField(2, []byte("from 19.3.2"))),
		nestedField(2, []byte("from 19.2")),
	))
	if got, _ := DecodeAntigravityMessage(second); got.Text != "from 19.3.2" {
		t.Errorf("text = %q, want from 19.3.2", got.Text)
	}
	// [19,2] is the last resort.
	third := nestedField(19, nestedField(2, []byte("from 19.2")))
	if got, _ := DecodeAntigravityMessage(third); got.Text != "from 19.2" {
		t.Errorf("text = %q, want from 19.2", got.Text)
	}
}

func TestAntigravityProtoToolResultPicksLongestPrintableString(t *testing.T) {
	blob := nestedField(30, concatBytes(
		nestedField(4, []byte("short")),
		nestedField(5, []byte("a much longer tool description")),
		nestedField(6, []byte{0x01, 0x02, 0x03}), // control characters are not text
	))
	want := DecodedAntigravityMessage{Role: RoleToolResult, Text: "a much longer tool description"}
	if got, ok := DecodeAntigravityMessage(blob); !ok || got != want {
		t.Fatalf("decodeAntigravityMessage(tool blob) = (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

func TestAntigravityProtoUserWinsOverAssistantAndTool(t *testing.T) {
	blob := concatBytes(realUser, realAssistant, realTool)
	if got, _ := DecodeAntigravityMessage(blob); got.Role != RoleUser {
		t.Errorf("role = %q, want user", got.Role)
	}
	withoutUser := concatBytes(realAssistant, realTool)
	if got, _ := DecodeAntigravityMessage(withoutUser); got.Role != RoleAssistant {
		t.Errorf("role = %q, want assistant", got.Role)
	}
}

func TestDecodeAntigravityMetadataTimestampPicksLargestValidTimestamp(t *testing.T) {
	userSec := int64(1792670400) // 2026-10-22T12:00:00.000Z
	// Two embedded timestamps: the larger one (field 7) wins, and nanos are
	// floored to milliseconds.
	blob := concatBytes(
		metadataTimestamp(1, userSec, 250_000_000),
		metadataTimestamp(7, userSec+5, 500_000_000),
	)
	got := DecodeAntigravityMetadataTimestamp(blob)
	if got == nil || *got != "2026-10-22T12:00:05.500Z" {
		t.Fatalf("timestamp = %v, want 2026-10-22T12:00:05.500Z", derefOrNil(got))
	}
}

func TestDecodeAntigravityMetadataTimestampRejectsOutOfRangeValues(t *testing.T) {
	cases := map[string][]byte{
		"seconds below the protobuf epoch bound": metadataTimestamp(1, 999_999_999, 0),
		"seconds above the protobuf epoch bound": metadataTimestamp(1, 10_000_000_001, 0),
		"nanos out of range":                     metadataTimestamp(1, 1_700_000_000, 1_000_000_000),
		"unknown sub field":                      unknownSubFieldTimestamp(),
		"repeated seconds field":                 repeatedSecondsTimestamp(),
		"no embedded timestamp":                  []byte{0x08, 0x01},
		"sub message too short":                  nestedField(1, []byte{0x08, 0x01}),
	}
	for name, input := range cases {
		if got := DecodeAntigravityMetadataTimestamp(input); got != nil {
			t.Errorf("%s: timestamp = %q, want absent", name, *got)
		}
	}
	if got := DecodeAntigravityMetadataTimestamp("not bytes"); got != nil {
		t.Errorf("string input: timestamp = %q, want absent", *got)
	}
	if got := DecodeAntigravityMetadataTimestamp(nil); got != nil {
		t.Errorf("nil input: timestamp = %q, want absent", *got)
	}
}

func TestDecodeAntigravityMetadataTimestampAcceptsNanosOnlyField(t *testing.T) {
	// Seconds alone (no nanos field) still yields a timestamp; nanos default to 0.
	got := DecodeAntigravityMetadataTimestamp(metadataTimestamp(1, 1_700_000_000, 0))
	if got == nil || *got != "2023-11-14T22:13:20.000Z" {
		t.Fatalf("timestamp = %v, want 2023-11-14T22:13:20.000Z", derefOrNil(got))
	}
}

// metadataTimestamp builds a length-delimited field holding a
// google.protobuf.Timestamp, exactly like the TS fixture helper.
func metadataTimestamp(fieldNumber int, seconds int64, nanos int64) []byte {
	body := []byte{0x08}
	body = append(body, encodeVarint(uint64(seconds))...)
	if nanos > 0 {
		body = append(body, 0x10)
		body = append(body, encodeVarint(uint64(nanos))...)
	}
	return nestedField(fieldNumber, body)
}

// unknownSubFieldTimestamp is a well-formed Timestamp body with one extra field
// number, which makes the whole candidate unusable.
func unknownSubFieldTimestamp() []byte {
	body := []byte{0x08}
	body = append(body, encodeVarint(1_700_000_000)...)
	return nestedField(1, append(body, 0x18, 0x01))
}

// repeatedSecondsField is a duplicate seconds field: the TS decoder only
// accepts the first one and marks the candidate invalid on the repeat.
func repeatedSecondsTimestamp() []byte {
	body := []byte{0x08}
	body = append(body, encodeVarint(1_700_000_000)...)
	body = append(body, 0x08)
	return nestedField(1, append(body, encodeVarint(1_700_000_001)...))
}

func encodeVarint(value uint64) []byte {
	encoded := make([]byte, 0, 10)
	for value >= 128 {
		encoded = append(encoded, byte(value&127)|128)
		value >>= 7
	}
	return append(encoded, byte(value))
}

func concatBytes(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}
