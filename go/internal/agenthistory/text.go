package agenthistory

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// textFromContent / sanitizeText / truncateChars / messageRef / timestampFrom /
// pathToPiSessionSlug, ported from src/agent-history/text.ts.
//
// The TS originals are defined over JS strings, i.e. UTF-16 code units. Every
// length-sensitive operation here therefore counts UTF-16 code units so that
// truncation and char counts match the TypeScript implementation exactly.

// utf16Len returns the UTF-16 code-unit length of s, matching s.length in JS.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
			continue
		}
		n++
	}
	return n
}

// jsTrim mirrors String.prototype.trim(): JS whitespace plus the BOM, which
// Go's strings.TrimSpace does not strip.
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0xFEFF, 0x2028, 0x2029:
			return true
		}
		return isUnicodeSpace(r)
	})
}

func isUnicodeSpace(r rune) bool {
	return r == 0x1680 || (r >= 0x2000 && r <= 0x200A) || r == 0x202F || r == 0x205F || r == 0x3000
}

// isJSBlank reports whether the string is empty after JS trim, i.e. `s.trim().length === 0`.
func isJSBlank(s string) bool { return jsTrim(s) == "" }

// TextFromContent mirrors textFromContent(content: unknown): string | null.
func TextFromContent(content any) (string, bool) {
	switch value := content.(type) {
	case string:
		return value, true
	case []any:
		parts := make([]string, 0, len(value))
		for _, block := range value {
			part := textFromBlock(block)
			if !isJSBlank(part) {
				parts = append(parts, part)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n"), true
		}
		return "", false
	default:
		return "", false
	}
}

func textFromBlock(block any) string {
	switch value := block.(type) {
	case string:
		return value
	case map[string]any:
		typeName, _ := value["type"].(string)
		if typeName == "thinking" || typeName == "reasoning" {
			return ""
		}
		if text, ok := value["text"].(string); ok {
			return text
		}
		if content, ok := value["content"].(string); ok {
			return content
		}
		if content, ok := value["content"].([]any); ok {
			// `textFromContent(record.content) ?? ""`
			text, _ := TextFromContent(content)
			return text
		}
		return ""
	default:
		return ""
	}
}

// Redaction patterns, in the order the TS implementation applies them. Go's
// regexp package is RE2, so \b, \s, and character classes behave the same; the
// `i` flag is preserved via (?i).
var redactionPatterns = []struct {
	re     *regexp.Regexp
	prefix int // submatch index holding the retained prefix
}{
	{regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)[^\s]+`), 1},
	{regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9._\-+=/]+`), 1},
	{regexp.MustCompile(`(?i)\b(token=)[^\s&]+`), 1},
	{regexp.MustCompile(`(?i)\b(password=)[^\s&]+`), 1},
	{regexp.MustCompile(`(?i)\b(secret=)[^\s&]+`), 1},
	{regexp.MustCompile(`(?i)\b(api_key=)[^\s&]+`), 1},
}

var skTokenPattern = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`)

// SanitizeResult mirrors the { redacted, text } return shape.
type SanitizeResult struct {
	Redacted bool
	Text     string
}

// SanitizeText mirrors sanitizeText(value: unknown).
//
// Unlike the TS original it takes an already-decoded value: callers that hold
// raw JSON bytes must pass the decoded value so JSON.stringify semantics are
// reproduced by the caller's encoder (see SanitizeJSON).
func SanitizeText(value any) SanitizeResult {
	var text string
	switch typed := value.(type) {
	case string:
		text = typed
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			text = jsString(value)
		} else {
			text = string(encoded)
		}
	}
	return sanitizeString(text)
}

// SanitizeString is SanitizeText for a value that is already a string.
func SanitizeString(value string) SanitizeResult { return sanitizeString(value) }

// SanitizeJSON mirrors sanitizeText(JSON.parse(raw)): marshal-then-sanitize is
// not equivalent, so the caller decodes first and this helper re-encodes with
// JSON.stringify ordering preserved by encoding/json for maps (sorted keys).
func SanitizeJSON(raw []byte) SanitizeResult {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return sanitizeString(string(raw))
	}
	return SanitizeText(decoded)
}

func jsString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	default:
		return fmt.Sprint(value)
	}
}

func sanitizeString(text string) SanitizeResult {
	redacted := false
	for _, pattern := range redactionPatterns {
		text = replaceAllStringSubmatch(text, pattern.re, func(match []string) string {
			redacted = true
			return match[pattern.prefix] + "[REDACTED]"
		})
	}
	text = replaceAllStringSubmatch(text, skTokenPattern, func([]string) string {
		redacted = true
		return "sk-[REDACTED]"
	})
	return SanitizeResult{Redacted: redacted, Text: text}
}

// replaceAllStringSubmatch rewrites non-overlapping matches left to right. It
// returns match plus submatch strings, matching String.prototype.replace with a
// function replacer over the whole match.
func replaceAllStringSubmatch(text string, re *regexp.Regexp, repl func([]string) string) string {
	matches := re.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var builder strings.Builder
	last := 0
	for _, index := range matches {
		start, end := index[0], index[1]
		builder.WriteString(text[last:start])
		group := make([]string, 0, len(index)/2)
		for i := 0; i < len(index); i += 2 {
			if index[i] < 0 {
				group = append(group, "")
				continue
			}
			group = append(group, text[index[i]:index[i+1]])
		}
		builder.WriteString(repl(group))
		last = end
	}
	builder.WriteString(text[last:])
	return builder.String()
}

// TruncateChars mirrors truncateChars(value, maxChars): counts characters as
// JS does (UTF-16 code units) and appends "..." after keeping maxChars-3 units.
//
// When the cut would land inside an astral character the whole code point is
// dropped rather than emitting half a surrogate pair, which has no valid UTF-8
// encoding; that is the only observable divergence from JS here.
func TruncateChars(value string, maxChars int) string {
	if utf16Len(value) <= maxChars {
		return value
	}
	keep := maxChars - 3
	if keep < 0 {
		keep = 0
	}
	count := 0
	next := 0
	for index, r := range value {
		width := 1
		if r >= 0x10000 {
			width = 2
		}
		if count+width > keep {
			break
		}
		count += width
		next = index + utf8.RuneLen(r)
	}
	return value[:next] + "..."
}

// CharCount mirrors [...value].length in TS: the number of characters, where an
// astral character counts once (spread iterates code points).
func CharCount(value string) int {
	return len([]rune(value))
}

// MessageRef mirrors messageRef(path, id, line).
func MessageRef(path string, id *string, line int) string {
	if id != nil && *id != "" {
		return path + "#entry=" + *id
	}
	return path + "#line=" + strconv.Itoa(line)
}

// TimestampFrom mirrors timestampFrom(value): non-empty strings pass through,
// finite numbers become ISO8601, everything else is absent.
func TimestampFrom(value any) *string {
	switch typed := value.(type) {
	case string:
		if len(typed) > 0 {
			return &typed
		}
		return nil
	case float64:
		if typed != typed { // NaN
			return nil
		}
		if typed > 1.7976931348623157e308 || typed < -1.7976931348623157e308 {
			return nil
		}
		millis := int64(typed)
		formatted := time.UnixMilli(millis).UTC().Format("2006-01-02T15:04:05.000Z")
		return &formatted
	case int:
		formatted := time.UnixMilli(int64(typed)).UTC().Format("2006-01-02T15:04:05.000Z")
		return &formatted
	case int64:
		formatted := time.UnixMilli(typed).UTC().Format("2006-01-02T15:04:05.000Z")
		return &formatted
	default:
		return nil
	}
}

// PathToPiSessionSlug mirrors pathToPiSessionSlug(cwd): "/" becomes "-".
func PathToPiSessionSlug(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}
