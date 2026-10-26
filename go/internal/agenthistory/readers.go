package agenthistory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// readJsonl / compactFromMessages / limitMessages, ported from
// src/agent-history/readers.ts.

// JSONLMaxBytes mirrors JSONL_MAX_BYTES (32 MiB).
const JSONLMaxBytes = 32 * 1024 * 1024

// UnstableJSONLError mirrors the TS UnstableJsonlError. A writer can leave a
// partial final record while message_end is firing.
type UnstableJSONLError struct {
	MalformedLines int
	Size           int64
	MtimeMs        float64
}

func (e *UnstableJSONLError) Error() string {
	return fmt.Sprintf("JSONL tail is unstable (%d malformed line(s))", e.MalformedLines)
}

// JSONLTooLargeError mirrors the TS JsonlTooLargeError.
type JSONLTooLargeError struct {
	MaxBytes int
	Size     int64
}

func (e *JSONLTooLargeError) Error() string {
	return fmt.Sprintf("JSONL file exceeds maximum size (%d > %d bytes)", e.Size, e.MaxBytes)
}

// JSONLEntry mirrors JsonlEntry: 1-based line number within the read window.
type JSONLEntry struct {
	Line  int
	Value map[string]any
}

// ReadJSONLOptions mirrors `{ maxBytes?: number }`.
type ReadJSONLOptions struct {
	MaxBytes int
}

// ReadJSONL mirrors readJsonl(path, options).
//
// Behaviour notes carried over from TS:
//   - Files larger than maxBytes are read from a tail window; the partial first
//     line of that window is skipped.
//   - A malformed *final* record means the tail is unstable and errors.
//   - When the tail window yields nothing at all, a single record larger than
//     the window is assumed and JSONLTooLargeError is raised instead of looking
//     like an empty history.
func ReadJSONL(path string, options ReadJSONLOptions) ([]JSONLEntry, error) {
	maxBytes := options.MaxBytes
	if maxBytes == 0 {
		maxBytes = JSONLMaxBytes
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	size := info.Size()
	start := int64(0)
	if size > int64(maxBytes) {
		start = size - int64(maxBytes)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	if start > 0 {
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return nil, err
		}
	}

	entries := make([]JSONLEntry, 0, 16)
	malformedLines := 0
	lineNumber := 0
	skipPartialLead := start > 0
	tailMalformed := false

	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		atEOF := errors.Is(readErr, io.EOF)
		if !atEOF {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
		}
		if atEOF && line == "" {
			break
		}

		if skipPartialLead {
			skipPartialLead = false
			if atEOF {
				break
			}
			continue
		}

		lineNumber++
		if line == "" || isJSBlank(line) {
			if atEOF {
				break
			}
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			malformedLines++
			tailMalformed = true
		} else {
			if object, ok := parsed.(map[string]any); ok {
				entries = append(entries, JSONLEntry{Line: lineNumber, Value: object})
			}
			tailMalformed = false
		}
		if atEOF {
			break
		}
	}

	// Only the tail can be transient: malformed historical records remain
	// observable, but a malformed final record must not be mistaken for no data.
	if malformedLines > 0 && tailMalformed {
		return nil, &UnstableJSONLError{
			MalformedLines: malformedLines,
			Size:           size,
			MtimeMs:        float64(info.ModTime().UnixMilli()),
		}
	}
	// A single record larger than the tail window is skipped as a partial lead,
	// which would otherwise look like an empty history with no error.
	if start > 0 && len(entries) == 0 {
		return nil, &JSONLTooLargeError{MaxBytes: maxBytes, Size: size}
	}
	return entries, nil
}

// CompactFromMessages mirrors compactFromMessages(ref, messages).
func CompactFromMessages(ref HistoryRef, messages []HistoryMessage) CompactAgentHistory {
	lastUser := lastByRole(messages, RoleUser)
	lastAssistant := lastByRole(messages, RoleAssistant)
	var lastTool *HistoryMessage
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == RoleToolResult {
			lastTool = &messages[index]
			break
		}
	}

	var unready *UnreadyReason
	if lastAssistant == nil {
		reason := UnreadyNoAssistantMessage
		switch {
		case len(messages) == 0:
			reason = UnreadyEmptyHistory
		case lastUser == nil:
			reason = UnreadyNoUserMessage
		default:
			lastUserIndex := -1
			hasAnyAssistant := false
			for index, message := range messages {
				if message.Role == RoleUser {
					lastUserIndex = index
				}
				if message.Role == RoleAssistant {
					hasAnyAssistant = true
				}
			}
			if hasAnyAssistant && lastUserIndex != -1 {
				reason = UnreadyAssistantOnlyBeforeLastUser
			}
		}
		unready = &reason
	}

	result := CompactAgentHistory{
		HistoryRef:     &ref,
		LastToolResult: lastToolCompact(lastTool),
		MessageCount:   len(messages),
		UnreadyReason:  unready,
		UpdatedAt:      lastTimestamp(messages),
	}
	source := string(ref.Source)
	result.Source = &source
	if lastAssistant != nil {
		excerpt := excerptOf(*lastAssistant)
		result.LastAssistantMessage = &excerpt
	}
	if lastUser != nil {
		excerpt := excerptOf(*lastUser)
		result.LastUserMessage = &excerpt
	}
	return result
}

func lastToolCompact(message *HistoryMessage) *CompactToolResult {
	if message == nil {
		return nil
	}
	return message.Compact
}

func lastTimestamp(messages []HistoryMessage) *string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Timestamp != nil {
			return messages[index].Timestamp
		}
	}
	return nil
}

// LimitMessages mirrors limitMessages(messages, limit): the tail-most `limit`
// entries, each sanitized.
func LimitMessages(messages []HistoryMessage, limit *int) []HistoryMessage {
	selected := messages
	if limit != nil && *limit > 0 && len(messages) > *limit {
		selected = messages[len(messages)-*limit:]
	}
	out := make([]HistoryMessage, 0, len(selected))
	for _, message := range selected {
		out = append(out, sanitizeHistoryMessage(message))
	}
	return out
}

func lastByRole(messages []HistoryMessage, role MessageRole) *HistoryMessage {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == role {
			return &messages[index]
		}
	}
	return nil
}

func excerptOf(message HistoryMessage) HistoryExcerpt {
	return HistoryExcerpt{
		Ref:        message.Ref,
		StopReason: message.StopReason,
		Text:       SanitizeString(message.Text).Text,
		Timestamp:  message.Timestamp,
	}
}

func sanitizeHistoryMessage(message HistoryMessage) HistoryMessage {
	text := SanitizeString(message.Text).Text
	if text == message.Text {
		return message
	}
	message.Text = text
	return message
}

// HistoryReader mirrors the TS AgentHistoryReader interface.
type HistoryReader interface {
	CanRead(ref HistoryRef) bool
	Read(ref HistoryRef, limit *int) ([]HistoryMessage, error)
	ReadCompact(ref HistoryRef) (CompactAgentHistory, error)
}
