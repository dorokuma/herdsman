package agenthistory

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Antigravity protobuf step decoding, ported from
// src/agent-history/antigravity-proto.ts.
//
// The TS original accepts Buffer/Uint8Array and swallows every malformed
// encoding by returning no fields, so the port keeps the same shape: only
// []byte is decodable, and a parse failure yields an empty field list rather
// than an error.

// DecodedAntigravityMessage mirrors AntigravityDecodedMessage.
type DecodedAntigravityMessage struct {
	Role MessageRole
	Text string
}

const replacementChar = rune(0xFFFD)

// DecodeAntigravityMessage mirrors decodeAntigravityMessage(value).
//
// Fields 19/20/30 are structural message envelopes copied from real agy step
// blobs: 19 is the user turn, 20 the model turn and 30 a tool execution.
// Inner path [20,3] is deliberately absent from the assistant paths because it
// carries thinking only, never assistant body text.
func DecodeAntigravityMessage(value any) (DecodedAntigravityMessage, bool) {
	bytes, ok := value.([]byte)
	if !ok {
		return DecodedAntigravityMessage{}, false
	}
	fields := parseFields(bytes)
	if text, found := firstTextAtPaths(fields, [][]int{{19, 3, 1}, {19, 3, 2}, {19, 2}}); found {
		return DecodedAntigravityMessage{Role: RoleUser, Text: text}, true
	}
	if text, found := firstTextAtPaths(fields, [][]int{{20, 8}, {20, 1}}); found {
		return DecodedAntigravityMessage{Role: RoleAssistant, Text: text}, true
	}
	if tools := stringsIn(fieldsFor(fields, 30)); len(tools) > 0 {
		return DecodedAntigravityMessage{Role: RoleToolResult, Text: tools[0]}, true
	}
	return DecodedAntigravityMessage{}, false
}

// DecodeAntigravityMetadataTimestamp mirrors
// decodeAntigravityMetadataTimestamp(value): the largest valid
// google.protobuf.Timestamp embedded in a step metadata blob.
func DecodeAntigravityMetadataTimestamp(value any) *string {
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	fields := parseFields(bytes)
	var maxMs *int64
	for _, field := range fields {
		if field.wireType != 2 {
			continue
		}
		subBytes := field.bytesValue()
		if len(subBytes) < 6 || len(subBytes) > 16 {
			continue
		}
		subFields := parseFields(subBytes)
		if len(subFields) == 0 {
			continue
		}
		var seconds *int64
		nanos := int64(0)
		valid := true
		for _, sub := range subFields {
			switch {
			case sub.number == 1 && sub.wireType == 0 && seconds == nil:
				value := sub.varintValue()
				seconds = &value
			case sub.number == 2 && sub.wireType == 0:
				nanos = sub.varintValue()
			default:
				// Any other field number, a non-varint encoding or a repeated
				// seconds field makes the candidate unusable.
				valid = false
			}
			if !valid {
				break
			}
		}
		if !valid || seconds == nil ||
			*seconds < 1_000_000_000 || *seconds > 10_000_000_000 ||
			nanos < 0 || nanos >= 1_000_000_000 {
			continue
		}
		ms := *seconds*1000 + nanos/1_000_000
		if maxMs == nil || ms > *maxMs {
			value := ms
			maxMs = &value
		}
	}
	if maxMs == nil {
		return nil
	}
	formatted := time.UnixMilli(*maxMs).UTC().Format("2006-01-02T15:04:05.000Z")
	return &formatted
}

type wireField struct {
	number   int
	wireType int
	value    any // int64 for varints, []byte for length-delimited
}

func (f wireField) varintValue() int64 {
	if value, ok := f.value.(int64); ok {
		return value
	}
	return 0
}

func (f wireField) bytesValue() []byte {
	if value, ok := f.value.([]byte); ok {
		return value
	}
	return nil
}

var errInvalidVarint = errors.New("invalid protobuf varint")

// parseFields mirrors parseFields(bytes): an unparsable blob yields no fields.
func parseFields(bytes []byte) (result []wireField) {
	defer func() {
		// readVarint signals a truncated or over-long varint with a panic, which
		// the TS implementation catches and turns into an empty field list.
		if recover() != nil {
			result = nil
		}
	}()
	offset := 0
	for offset < len(bytes) {
		tag, next, err := readVarint(bytes, offset)
		if err != nil {
			return nil
		}
		offset = next
		number := int(uint64(tag) >> 3)
		wireType := int(uint64(tag) & 7)
		if number < 1 || number > 536_870_911 {
			return nil
		}
		switch wireType {
		case 0:
			value, next, err := readVarint(bytes, offset)
			if err != nil {
				return nil
			}
			offset = next
			result = append(result, wireField{number: number, wireType: wireType, value: value})
		case 1:
			offset += 8
			result = append(result, wireField{number: number, wireType: wireType, value: int64(0)})
		case 2:
			length, next, err := readVarint(bytes, offset)
			if err != nil {
				return nil
			}
			offset = next
			// A length past the end of the buffer is rejected before the slice
			// expression: on a huge length `int(length)` can overflow negative and
			// the slice would panic instead of failing cleanly.
			if uint64(length) > uint64(len(bytes)-offset) {
				return nil
			}
			end := offset + int(length)
			if end > len(bytes) {
				return nil
			}
			result = append(result, wireField{
				number: number, wireType: wireType, value: bytes[offset:end:end],
			})
			offset = end
		case 5:
			offset += 4
			result = append(result, wireField{number: number, wireType: wireType, value: int64(0)})
		default:
			return nil
		}
		if offset > len(bytes) {
			return nil
		}
	}
	return result
}

// readVarint mirrors readVarint(bytes, offset). A varint that runs past the end
// of the buffer or exceeds the 10-byte / 64-bit window reports an error.
//
// The TS implementation accumulates into a BigInt, so the 10th byte keeps every
// bit it carries. Go's uint64 drops bit 63 and above, which would silently turn
// an out-of-range tag into a valid one: `2^64 + 154` truncates to 154, i.e.
// field 19 / wireType 2, while the TS BigInt keeps 2^64 and the field-number
// range check rejects it. A 10th byte wider than 1 therefore fails here instead
// of being truncated.
func readVarint(bytes []byte, offset int) (int64, int, error) {
	var value uint64
	for shift := uint(0); offset < len(bytes) && shift <= 63; shift += 7 {
		next := bytes[offset]
		offset++
		if shift == 63 && (next&0x7f) > 1 {
			return 0, 0, errInvalidVarint
		}
		value |= uint64(next&127) << shift
		if next < 0x80 {
			return int64(value), offset, nil
		}
	}
	return 0, 0, errInvalidVarint
}

func fieldsFor(fields []wireField, number int) [][]byte {
	result := make([][]byte, 0, len(fields))
	for _, field := range fields {
		if field.number == number && field.wireType == 2 {
			result = append(result, field.bytesValue())
		}
	}
	return result
}

func firstTextAtPaths(fields []wireField, paths [][]int) (string, bool) {
	for _, path := range paths {
		if text, ok := textAtPath(fields, path); ok {
			return text, true
		}
	}
	return "", false
}

func textAtPath(fields []wireField, path []int) (string, bool) {
	current := fields
	for index, want := range path {
		var found *wireField
		for i := range current {
			if current[i].number == want && current[i].wireType == 2 {
				found = &current[i]
				break
			}
		}
		if found == nil {
			return "", false
		}
		bytes := found.bytesValue()
		if index == len(path)-1 {
			return decodeUTF8Strict(bytes)
		}
		current = parseFields(bytes)
	}
	return "", false
}

// decodeUTF8Strict mirrors `bytes.toString("utf8")` followed by
// `text && !text.includes("\uFFFD")`: Node replaces every ill-formed byte with
// U+FFFD, so invalid input and a literal U+FFFD both mean "not text".
func decodeUTF8Strict(bytes []byte) (string, bool) {
	text := string(bytes)
	if text == "" {
		return "", false
	}
	if !utf8.Valid(bytes) {
		return "", false
	}
	if strings.ContainsRune(text, replacementChar) {
		return "", false
	}
	return text, true
}

// printableUTF8 mirrors the per-character test inside stringsIn: newlines, CR,
// tab and anything from U+0020 up are printable, other control characters are
// not, and an ill-formed encoding is rejected via the U+FFFD check.
func printableUTF8(text string) bool {
	if text == "" {
		return false
	}
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		switch r {
		case '\n', '\r', '\t':
			continue
		}
		if r < 0x20 || r == replacementChar {
			return false
		}
	}
	return true
}

// stringsIn mirrors stringsIn(values): every printable string in the values and
// in their nested length-delimited fields, non-blank, longest first.
func stringsIn(values [][]byte) []string {
	result := []string{}
	for _, value := range values {
		if text, ok := decodeUTF8Strict(value); ok && printableUTF8(text) {
			result = append(result, text)
		}
		for _, nested := range parseFields(value) {
			if nested.wireType == 2 {
				result = append(result, stringsIn([][]byte{nested.bytesValue()})...)
			}
		}
	}
	kept := make([]string, 0, len(result))
	for _, text := range result {
		if !isJSBlank(text) {
			kept = append(kept, text)
		}
	}
	// Array.prototype.sort is stable in V8 and JS compares UTF-16 code units.
	sort.SliceStable(kept, func(i, j int) bool {
		return utf16Len(kept[i]) > utf16Len(kept[j])
	})
	return kept
}

// jsNumberToString mirrors String(number) for the numeric payloads the driver
// hands back, so a step id renders exactly like the TS reader writes it.
func jsNumberToString(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	}
	if value == math.Trunc(value) && value >= -9.223372036854776e18 && value <= 9.223372036854776e18 {
		// JS prints integral numbers without a fraction; strconv would add one.
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}
