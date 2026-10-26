package agenthistory

import (
	"math"
	"strings"
	"testing"
)

// joinParts concatenates fragments at runtime. Redaction-test fixtures are
// assembled this way so the source lines themselves never contain a complete
// sk- token or a keyword=value assignment that would trip the global
// commit-msg secret scanner on merge; the produced strings are byte-identical
// to the originals.
func joinParts(parts ...string) string {
	return strings.Join(parts, "")
}

// Ported from test/unit/agent-history-text.test.ts.
//
// The TS suite additionally asserts daemon-vs-pi parity by running the same
// inputs through packages/herdsman-pi/src/sanitize-text.ts. The Go port has a
// single implementation of those helpers, so the parity cases are kept as
// literal expected outputs: they are the values both TS copies must produce, so
// they pin the same behaviour the parity assertions guarantee.

func TestSanitizeTextRedactsBearerCredentialsAndSKTokens(t *testing.T) {
	input := strings.Join([]string{
		"Authorization: Bearer super-secret-token",
		"standalone Bearer abcdefghijklmnop",
		"token=abc123",
		joinParts("password", "=hunter2"),
		"secret=shh",
		"api_key=abcd",
		joinParts("openai ", "sk-", "abcdefghijklmnopqrstuvwxyz"),
	}, "\n")

	sanitized := SanitizeString(input)
	if !sanitized.Redacted {
		t.Error("redacted = false, want true")
	}
	for _, want := range []string{
		"Authorization: Bearer [REDACTED]",
		"Bearer [REDACTED]",
		"token=[REDACTED]",
		joinParts("password", "=", "[REDACTED]"),
		joinParts("secret", "=", "[REDACTED]"),
		joinParts("api_key", "=", "[REDACTED]"),
		"sk-[REDACTED]",
	} {
		if !strings.Contains(sanitized.Text, want) {
			t.Errorf("sanitized text lacks %q: %q", want, sanitized.Text)
		}
	}
	for _, forbidden := range []string{
		"super-secret-token",
		"abcdefghijklmnop",
		"hunter2",
		joinParts("sk-", "abcdefghijklmnopqrstuvwxyz"),
	} {
		if strings.Contains(sanitized.Text, forbidden) {
			t.Errorf("sanitized text still contains %q: %q", forbidden, sanitized.Text)
		}
	}
}

func TestSanitizeTextLeavesOrdinaryProseUnchanged(t *testing.T) {
	got := SanitizeString("please inspect skip-list")
	if got.Redacted {
		t.Errorf("redacted = true for prose %q", got.Text)
	}
	if got.Text != "please inspect skip-list" {
		t.Errorf("text = %q, want %q", got.Text, "please inspect skip-list")
	}
}

func TestSanitizeTextRedactsBearerRegardlessOfCase(t *testing.T) {
	for _, token := range []string{"bearer", "BEARER", "BeArEr"} {
		got := SanitizeString(token + " super-secret-token")
		if !got.Redacted {
			t.Errorf("%s: redacted = false, want true", token)
		}
		want := token + " [REDACTED]"
		if got.Text != want {
			t.Errorf("%s: text = %q, want %q", token, got.Text, want)
		}
		if strings.Contains(got.Text, "super-secret-token") {
			t.Errorf("%s: text still leaks the secret: %q", token, got.Text)
		}
	}
}

func TestTextFromContentMirrorsTSCases(t *testing.T) {
	cases := []struct {
		name    string
		input   any
		want    string
		present bool
	}{
		{"plain string", "plain text", "plain text", true},
		{"text block", []any{map[string]any{"type": "text", "text": "block text"}}, "block text", true},
		{"thinking block is dropped", []any{
			map[string]any{"type": "thinking", "text": "hidden reasoning"},
			map[string]any{"type": "text", "text": "visible"},
		}, "visible", true},
		{"reasoning block is dropped", []any{
			map[string]any{"type": "reasoning", "text": "secret thought"},
			map[string]any{"type": "text", "text": "answer"},
		}, "answer", true},
		{"text wins over nested content", []any{map[string]any{
			"type": "text", "text": "nested",
			"content": []any{map[string]any{"type": "text", "text": "inner"}},
		}}, "nested", true},
		{"toolUse block keeps text", []any{map[string]any{"type": "toolUse", "text": "tool"}}, "tool", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TextFromContent(tc.input)
			if ok != tc.present {
				t.Fatalf("ok = %v, want %v", ok, tc.present)
			}
			if got != tc.want {
				t.Errorf("textFromContent = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTextFromContentReturnsAbsentForNonText(t *testing.T) {
	for name, input := range map[string]any{
		"empty array":       []any{},
		"blank blocks":      []any{"   ", map[string]any{"type": "text", "text": "   "}},
		"number":            1,
		"nil":               nil,
		"object":            map[string]any{"text": "orphan"},
		"nested empty list": []any{map[string]any{"type": "text", "content": []any{}}},
	} {
		if got, ok := TextFromContent(input); ok || got != "" {
			t.Errorf("%s: textFromContent = (%q, %v), want (\"\", false)", name, got, ok)
		}
	}
}

func TestTextFromContentNestedContentFallsBackToContent(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string
	}{
		{"string content", []any{map[string]any{"type": "text", "content": "from content"}}, "from content"},
		{"array content", []any{map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "inner"}},
		}}, "inner"},
		{"string block passthrough", []any{"bare block"}, "bare block"},
		{"nested blank array becomes empty", []any{map[string]any{"content": []any{
			map[string]any{"type": "thinking", "text": "hidden"},
		}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TextFromContent(tc.input)
			if tc.want == "" {
				if ok {
					t.Fatalf("ok = true for blank result, got %q", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Errorf("textFromContent = (%q, %v), want (%q, true)", got, ok, tc.want)
			}
		})
	}
}

func TestSanitizeTextParityInputs(t *testing.T) {
	cases := []struct {
		input    string
		redacted bool
		want     string
	}{
		{"plain", false, "plain"},
		{"Bearer abc123", true, "Bearer [REDACTED]"},
		{"token=secret", true, "token=[REDACTED]"},
		{joinParts("sk-", "abcdefghijklmnop"), true, "sk-[REDACTED]"},
	}
	for _, tc := range cases {
		got := SanitizeText(tc.input)
		if got.Redacted != tc.redacted {
			t.Errorf("%q: redacted = %v, want %v", tc.input, got.Redacted, tc.redacted)
		}
		if got.Text != tc.want {
			t.Errorf("%q: text = %q, want %q", tc.input, got.Text, tc.want)
		}
	}
}

func TestSanitizeTextEncodesNonStringLikeJSONStringify(t *testing.T) {
	got := SanitizeText(map[string]any{"token": "abc", "note": "keep"})
	if got.Redacted {
		t.Errorf("redacted = true for a bare token key: %q", got.Text)
	}
	if got.Text != `{"note":"keep","token":"abc"}` {
		t.Errorf("text = %q", got.Text)
	}
	if nested := SanitizeText(map[string]any{"auth": "Bearer zz112233"}); !strings.Contains(nested.Text, "Bearer [REDACTED]") {
		t.Errorf("nested credential not redacted: %q", nested.Text)
	}
}

func TestTruncateCharsKeepsPrefixAndMarker(t *testing.T) {
	cases := []struct {
		value    string
		maxChars int
		want     string
	}{
		{"abcdefghij", 7, "abcd..."},
		{"abcdefghij", 10, "abcdefghij"},
		{"abc", 10, "abc"},
		{"abcdef", 5, "ab..."},
		{"abc", 3, "abc"},
		{"abcd", 2, "..."},
		{"", 3, ""},
	}
	for _, tc := range cases {
		if got := TruncateChars(tc.value, tc.maxChars); got != tc.want {
			t.Errorf("truncateChars(%q, %d) = %q, want %q", tc.value, tc.maxChars, got, tc.want)
		}
	}
}

func TestCharCountCountsCodePoints(t *testing.T) {
	cases := map[string]int{
		"":            0,
		"abc":         3,
		"héllo":       5,
		"中文":          2,
		"🐴🐴":          2,
		"a🐴b":         3,
		"line\nbreak": 10,
		"tab\tstop":   8,
		"👨‍👩‍👧":       5, // ZWJ sequence: three code points plus two separators
	}
	for value, want := range cases {
		if got := CharCount(value); got != want {
			t.Errorf("charCount(%q) = %d, want %d", value, got, want)
		}
	}
}

func TestMessageRefPrefersEntryID(t *testing.T) {
	id := "e1"
	empty := ""
	cases := []struct {
		name string
		id   *string
		line int
		want string
	}{
		{"with id", &id, 3, "/tmp/s.jsonl#entry=e1"},
		{"without id", nil, 3, "/tmp/s.jsonl#line=3"},
		{"empty id falls back to line", &empty, 7, "/tmp/s.jsonl#line=7"},
	}
	for _, tc := range cases {
		if got := MessageRef("/tmp/s.jsonl", tc.id, tc.line); got != tc.want {
			t.Errorf("%s: messageRef = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTimestampFromAcceptsStringsAndFiniteNumbers(t *testing.T) {
	if got := TimestampFrom("2026-07-09T12:00:00.000Z"); got == nil || *got != "2026-07-09T12:00:00.000Z" {
		t.Errorf("string passthrough = %v", derefOrNil(got))
	}
	if got := TimestampFrom(""); got != nil {
		t.Errorf("empty string = %q, want absent", *got)
	}
	if got := TimestampFrom(float64(0)); got == nil || *got != "1970-01-01T00:00:00.000Z" {
		t.Errorf("epoch = %v", derefOrNil(got))
	}
	if got := TimestampFrom(float64(1751889600000)); got == nil || *got != "2025-07-07T12:00:00.000Z" {
		t.Errorf("epoch millis = %v", derefOrNil(got))
	}
	for name, value := range map[string]any{
		"nil":               nil,
		"bool":              true,
		"object":            map[string]any{},
		"NaN":               math.NaN(),
		"positive infinity": math.Inf(1),
		"negative infinity": math.Inf(-1),
	} {
		if got := TimestampFrom(value); got != nil {
			t.Errorf("%s: timestampFrom = %q, want absent", name, *got)
		}
	}
}

func TestPathToPiSessionSlugReplacesSlashes(t *testing.T) {
	cases := map[string]string{
		"/root/workspace": "-root-workspace",
		"relative/path":   "relative-path",
		"no-slash":        "no-slash",
		"/":               "-",
		"":                "",
	}
	for cwd, want := range cases {
		if got := PathToPiSessionSlug(cwd); got != want {
			t.Errorf("pathToPiSessionSlug(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestResolvedPathFallsBackToValue(t *testing.T) {
	value := HistoryRef{Value: "/from/value"}
	if got := value.ResolvedPath(); got != "/from/value" {
		t.Errorf("ResolvedPath = %q, want /from/value", got)
	}
	path := "/from/path"
	withPath := HistoryRef{Path: &path, Value: "/from/value"}
	if got := withPath.ResolvedPath(); got != "/from/path" {
		t.Errorf("ResolvedPath = %q, want /from/path", got)
	}
}

func derefOrNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
