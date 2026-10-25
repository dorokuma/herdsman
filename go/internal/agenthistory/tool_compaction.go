package agenthistory

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// compactToolResult, ported from src/agent-history/tool-compaction.ts.

// MaxToolResultChars mirrors maxToolResultChars.
const MaxToolResultChars = 1600

// passthroughMarker mirrors passthroughMarkers[0].
const passthroughMarker = "[HERDSMAN:TRUNCATED_TOOL_RESULT]"

var (
	crlfPattern              = regexp.MustCompile(`\r?\n`)
	failureSignalPattern     = regexp.MustCompile(`(?i)(FAIL|Failed|AssertionError|Test Files|Tests|ERR_PNPM|expected|received|Error:)`)
	failureLinePattern       = regexp.MustCompile(`(?i)(FAIL|Failed|AssertionError|Test Files|Tests|ERR_PNPM|expected|received|Error:|\bnot ok\b)`)
	leadingWhitespacePattern = regexp.MustCompile(`^\s+`)
	atPattern                = regexp.MustCompile(`at\s+`)
	webToolPattern           = regexp.MustCompile(`(?i)(web_search|fetch_content|mcp)`)
	urlPattern               = regexp.MustCompile(`https?://`)
	markerLinePattern        = regexp.MustCompile(`^\s*[-*#]`)
	titleSourceURLPattern    = regexp.MustCompile(`(?i)title|source|url`)
	digitsPattern            = regexp.MustCompile(`\d+`)
)

// CompactToolResultInput mirrors the TS input object.
type CompactToolResultInput struct {
	IsError  bool
	Ref      string
	Text     string
	ToolName string
}

// CompactToolResultValue mirrors compactToolResult(input). The TS export is
// compactToolResult over the type CompactToolResult; Go cannot have both at
// package scope, so the function carries the Value suffix.
func CompactToolResultValue(input CompactToolResultInput) CompactToolResult {
	sanitized := SanitizeString(input.Text).Text
	originalChars := CharCount(input.Text)
	mode, text := compactText(input.ToolName, sanitized)
	bounded := boundWithMarker(text, mode)
	return CompactToolResult{
		Compaction: ToolCompaction{
			Mode:          mode,
			OriginalChars: originalChars,
			ReturnedChars: CharCount(bounded),
		},
		IsError:  input.IsError,
		Ref:      input.Ref,
		Text:     bounded,
		ToolName: input.ToolName,
	}
}

func compactText(toolName, text string) (CompactionMode, string) {
	if failure, ok := failureFocus(text); ok {
		return ModeFailureFocus, failure
	}
	if web, ok := webSources(toolName, text); ok {
		return ModeWebSources, web
	}
	if structured, ok := structuredSummary(text); ok {
		return ModeStructuredSummary, structured
	}
	if grouped, ok := dedupeLines(text); ok && grouped != text {
		return ModeGroupedMatches, grouped
	}
	return ModeTruncatedPassthrough, text
}

func failureFocus(text string) (string, bool) {
	if !failureSignalPattern.MatchString(text) {
		return "", false
	}
	lines := crlfPattern.Split(text, -1)
	selected := make([]string, 0, 16)
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		if failureLinePattern.MatchString(line) {
			selected = append(selected, line)
			limit := index + 4
			if limit > len(lines) {
				limit = len(lines)
			}
			for next := index + 1; next < limit; next++ {
				neighbor := lines[next]
				if leadingWhitespacePattern.MatchString(neighbor) ||
					atPattern.MatchString(neighbor) ||
					isJSBlank(neighbor) {
					selected = append(selected, neighbor)
				}
			}
		}
		if len(selected) >= 40 {
			break
		}
	}
	kept := make([]string, 0, len(selected))
	for index, line := range selected {
		if !isJSBlank(line) {
			kept = append(kept, line)
			continue
		}
		// Keep a blank line only when the previous kept line is non-blank.
		if index > 0 && !isJSBlank(selected[index-1]) {
			kept = append(kept, line)
		}
	}
	compact := jsTrim(strings.Join(kept, "\n"))
	if len(compact) == 0 {
		return "", false
	}
	return compact, true
}

func webSources(toolName, text string) (string, bool) {
	if !webToolPattern.MatchString(toolName) && !urlPattern.MatchString(text) {
		return "", false
	}
	lines := crlfPattern.Split(text, -1)
	result := make([]string, 0, 8)
	for _, line := range lines {
		if urlPattern.MatchString(line) ||
			markerLinePattern.MatchString(line) ||
			titleSourceURLPattern.MatchString(line) {
			result = append(result, jsTrim(line))
		}
		if len(result) >= 30 {
			break
		}
	}
	if len(result) == 0 {
		return "", false
	}
	return strings.Join(result, "\n"), true
}

func structuredSummary(text string) (string, bool) {
	trimmed := jsTrim(text)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return "", false
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return "", false
	}
	switch typed := parsed.(type) {
	case []any:
		sample := typed
		if len(sample) > 5 {
			sample = sample[:5]
		}
		described := make([]string, 0, len(sample))
		for _, item := range sample {
			described = append(described, describeValue(item))
		}
		return "array(" + itoa(len(typed)) + ") [" + strings.Join(described, ", ") + "]", true
	case map[string]any:
		entries := make([]string, 0, len(typed))
		for key, value := range typed {
			if len(entries) >= 30 {
				break
			}
			entries = append(entries, key+": "+describeValue(value))
		}
		return strings.Join(entries, "\n"), true
	default:
		return "", false
	}
}

// describeValue mirrors describeValue(value): typeof for scalars, array(n) for
// arrays, object(n) for objects.
func describeValue(value any) string {
	switch typed := value.(type) {
	case []any:
		return "array(" + itoa(len(typed)) + ")"
	case map[string]any:
		return "object(" + itoa(len(typed)) + ")"
	case nil:
		return "object"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	default:
		return "object"
	}
}

func dedupeLines(text string) (string, bool) {
	lines := make([]string, 0, 16)
	for _, line := range crlfPattern.Split(text, -1) {
		if !isJSBlank(line) {
			lines = append(lines, line)
		}
	}
	if len(lines) < 4 {
		return "", false
	}
	type entry struct {
		count int
		line  string
	}
	order := make([]string, 0, len(lines))
	counts := make(map[string]*entry, len(lines))
	for _, line := range lines {
		key := digitsPattern.ReplaceAllString(jsTrim(line), "#")
		if current, ok := counts[key]; ok {
			current.count++
			continue
		}
		counts[key] = &entry{count: 1, line: line}
		order = append(order, key)
	}
	hasDuplicate := false
	for _, key := range order {
		if counts[key].count > 1 {
			hasDuplicate = true
			break
		}
	}
	if !hasDuplicate {
		return "", false
	}
	if len(order) > 80 {
		order = order[:80]
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		item := counts[key]
		if item.count > 1 {
			out = append(out, item.line+" (x"+itoa(item.count)+")")
			continue
		}
		out = append(out, item.line)
	}
	return strings.Join(out, "\n"), true
}

func boundWithMarker(text string, mode CompactionMode) string {
	if CharCount(text) <= MaxToolResultChars {
		return text
	}
	marker := ""
	if mode == ModeTruncatedPassthrough {
		marker = "\n" + passthroughMarker
	}
	return TruncateChars(text, MaxToolResultChars) + marker
}

// itoa keeps the integer formatting call sites terse.
func itoa(value int) string { return strconv.Itoa(value) }
