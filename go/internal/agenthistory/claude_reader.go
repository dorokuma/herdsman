package agenthistory

// ClaudeHistoryReader reads Claude Code session JSONL transcripts.
//
// The CC transcript is one JSON object per line under
// `<claudeProjectsRoot>/<project-dir>/<session-id>.jsonl`. Only the main-chain
// conversational records are read:
//
//   - `isSidechain == true` records are skipped (they belong to Task subagents).
//   - `type` outside {user, assistant} is skipped, including the operational
//     records CC sprinkles into the same file (custom-title,
//     file-history-snapshot, api-request, cost-state, summary, system,
//     progress, ...).
//   - `type == "user"`: a string `message.content` is one user message. An
//     array content is walked block by block: `text` blocks are joined into a
//     user message and every `tool_result` block is compacted through the same
//     CompactToolResult the pi reader uses, named by the `tool_use` id of a
//     preceding assistant turn.
//   - `type == "assistant"`: the `text` blocks of `message.content` are joined
//     into an assistant message, `tool_use` blocks only feed the id -> tool
//     name table. `message.stop_reason` is carried through as StopReason.
//
// Multi-block aggregation. CC splits one API response that contains several
// content blocks into several consecutive `type == "assistant"` records that
// share the same non-empty `message.id` (`apiBlockIndex` 0, 1, 2, ...). Text
// produced by later records of the same `message.id` is merged into the same
// assistant message with "\n", and the later record's non-empty
// `stop_reason`/timestamp become the message's values — including a later
// record that carries no `text` at all (thinking/tool_use-only blocks).
//
// Streaming safety. CC appends the assistant record before the turn is
// finished, so the last main-chain assistant record can be a half-written or
// mid-tool-call state. ReadCompact only reports a final reply when its
// stop_reason is one of the terminal values ("end_turn", "max_tokens",
// "stop_sequence"); a record with no stop_reason (still streaming) or
// "tool_use" (still inside a tool call) makes the compact history unready
// instead of passing an intermediate state off as the completed answer.

// claudeNonConversationalTypes lists record types that share the transcript
// file but are not part of the conversation. Any other unknown type is skipped
// too — this list exists so new CC operational types are deliberately named,
// not accidentally read.
var claudeNonConversationalTypes = map[string]struct{}{
	"api-request":           {},
	"cost-state":            {},
	"custom-title":          {},
	"file-history-snapshot": {},
	"progress":              {},
	"queue-operation":       {},
	"summary":               {},
	"system":                {},
}

// claudeTerminalStopReasons are the stop_reason values that mark an assistant
// turn as complete. Anything else (absent, null, "tool_use", "pause_turn")
// means the record is not the finished reply.
var claudeTerminalStopReasons = map[string]struct{}{
	"end_turn":      {},
	"max_tokens":    {},
	"stop_sequence": {},
}

// claudeStopReasonTerminal reports whether the stop_reason marks a finished
// assistant turn; a nil reason (still streaming) is not terminal.
func claudeStopReasonTerminal(stopReason *string) bool {
	if stopReason == nil {
		return false
	}
	_, terminal := claudeTerminalStopReasons[*stopReason]
	return terminal
}

// ClaudeHistoryReader reads Claude Code session JSONL files.
type ClaudeHistoryReader struct{}

// NewClaudeHistoryReader builds a reader.
func NewClaudeHistoryReader() *ClaudeHistoryReader { return &ClaudeHistoryReader{} }

var _ HistoryReader = (*ClaudeHistoryReader)(nil)

// CanRead mirrors the pi reader's gate: source must be claude-jsonl and either
// path or value must be non-empty.
func (r *ClaudeHistoryReader) CanRead(ref HistoryRef) bool {
	if ref.Source != SourceClaudeJSONL {
		return false
	}
	if ref.Path != nil && *ref.Path != "" {
		return true
	}
	return ref.Value != ""
}

// claudeTranscript is the raw parse of a CC transcript plus the tail state
// that decides compact-read readiness.
type claudeTranscript struct {
	messages []HistoryMessage
	// tailPendingAssistant is true when the newest main-chain assistant record
	// produced no text and carried no terminal stop_reason — a pure
	// tool_use/thinking record that must not let an earlier reply pass as the
	// finished answer.
	tailPendingAssistant bool
}

// Read mirrors PiHistoryReader.read over the CC record format.
func (r *ClaudeHistoryReader) Read(ref HistoryRef, limit *int) ([]HistoryMessage, error) {
	transcript, err := r.readTranscript(ref)
	if err != nil {
		return nil, err
	}
	return LimitMessages(transcript.messages, limit), nil
}

// readTranscript walks the CC records into messages. Consecutive assistant
// records sharing one non-empty `message.id` fold into a single assistant
// message: later non-empty text is joined with "\n" and each record's
// non-empty stop_reason/timestamp refresh the message.
func (r *ClaudeHistoryReader) readTranscript(ref HistoryRef) (claudeTranscript, error) {
	path := ref.ResolvedPath()
	entries, err := ReadJSONL(path, ReadJSONLOptions{})
	if err != nil {
		return claudeTranscript{}, err
	}

	messages := make([]HistoryMessage, 0, len(entries))
	toolNames := map[string]string{}
	// mergeableAssistantID is the non-empty `message.id` whose consecutive
	// assistant records produced the current tail assistant message; any other
	// processed main-chain record closes the run.
	mergeableAssistantID := ""
	tailPendingAssistant := false
	for _, entry := range entries {
		record := entry.Value
		if isTrue(record["isSidechain"]) {
			continue
		}
		recordType, _ := record["type"].(string)
		if _, skip := claudeNonConversationalTypes[recordType]; skip {
			continue
		}
		message := recordOf(record["message"])
		timestamp := TimestampFrom(record["timestamp"])
		if timestamp == nil {
			timestamp = TimestampFrom(message["timestamp"])
		}
		id, hasID := stringValue(record["uuid"])
		if !hasID {
			id, hasID = stringValue(record["requestId"])
		}
		var idPtr *string
		if hasID {
			idPtr = &id
		}
		refValue := MessageRef(path, idPtr, entry.Line)

		switch recordType {
		case "user":
			mergeableAssistantID = ""
			switch content := message["content"].(type) {
			case string:
				if !isJSBlank(content) {
					messages = append(messages, HistoryMessage{
						Ref:       refValue,
						Role:      RoleUser,
						Text:      content,
						Timestamp: timestamp,
					})
				}
			case []any:
				textParts := []string{}
				flush := func() {
					text := joinedClaudeText(textParts)
					textParts = textParts[:0]
					if text == "" {
						return
					}
					messages = append(messages, HistoryMessage{
						Ref:       refValue,
						Role:      RoleUser,
						Text:      text,
						Timestamp: timestamp,
					})
				}
				for _, block := range content {
					object, ok := block.(map[string]any)
					if !ok {
						continue
					}
					switch blockType, _ := object["type"].(string); blockType {
					case "text":
						if text, ok := object["text"].(string); ok && !isJSBlank(text) {
							textParts = append(textParts, text)
						}
					case "tool_result":
						// A tool_result block ends the run of text that precedes
						// it, so the message order matches the block order.
						flush()
						text, ok := TextFromContent(object["content"])
						if !ok {
							text = ""
						}
						toolName := "unknown"
						if toolUseID, ok := stringValue(object["tool_use_id"]); ok {
							if name, found := toolNames[toolUseID]; found {
								toolName = name
							}
						}
						compact := CompactToolResultValue(CompactToolResultInput{
							IsError:  isTrue(object["is_error"]),
							Ref:      refValue,
							Text:     text,
							ToolName: toolName,
						})
						name := toolName
						messages = append(messages, HistoryMessage{
							Compact:   &compact,
							Ref:       refValue,
							Role:      RoleToolResult,
							Text:      compact.Text,
							Timestamp: timestamp,
							ToolName:  &name,
						})
					}
				}
				flush()
			}
		case "assistant":
			messageID, _ := stringValue(message["id"])
			textParts := []string{}
			for _, block := range recordList(message["content"]) {
				switch blockType, _ := block["type"].(string); blockType {
				case "text":
					if text, ok := block["text"].(string); ok && !isJSBlank(text) {
						textParts = append(textParts, text)
					}
				case "tool_use":
					toolUseID, hasToolUseID := stringValue(block["id"])
					toolName, hasToolName := stringValue(block["name"])
					if hasToolUseID && hasToolName {
						toolNames[toolUseID] = toolName
					}
				}
			}
			text := joinedClaudeText(textParts)
			var stopReason *string
			if value, present := message["stop_reason"].(string); present && value != "" {
				stopReason = &value
			}
			tailPendingAssistant = text == "" && !claudeStopReasonTerminal(stopReason)
			last := len(messages) - 1
			if messageID != "" && messageID == mergeableAssistantID &&
				last >= 0 && messages[last].Role == RoleAssistant {
				// A later block of the same API response: fold its text into
				// the same assistant message and refresh stop_reason/timestamp
				// from the latest non-empty values.
				if text != "" {
					if messages[last].Text != "" {
						messages[last].Text += "\n"
					}
					messages[last].Text += text
				}
				if stopReason != nil {
					messages[last].StopReason = stopReason
				}
				if timestamp != nil {
					messages[last].Timestamp = timestamp
				}
				continue
			}
			// Any assistant record that did not merge — including one whose
			// message.id is empty — closes the run: a later record may not
			// re-merge into the earlier message of the same id
			// (msg_1 -> empty-id -> msg_1 stays two messages).
			mergeableAssistantID = ""
			if text == "" {
				continue
			}
			messages = append(messages, HistoryMessage{
				Ref:        refValue,
				Role:       RoleAssistant,
				StopReason: stopReason,
				Text:       text,
				Timestamp:  timestamp,
			})
			mergeableAssistantID = messageID
		}
	}
	return claudeTranscript{messages: messages, tailPendingAssistant: tailPendingAssistant}, nil
}

// ReadCompact mirrors the streaming-aware compact read: a main-chain assistant
// record that is still streaming (no stop_reason) or still inside a tool call
// (stop_reason "tool_use") must not be delivered as the final reply.
//
// Two extra guards cover the real multi-block format:
//   - a newer `type == "user"` instruction after the last assistant message
//     (a `｜pending` follow-up) means the finished reply belongs to the
//     previous turn: unready assistant_only_before_last_user.
//   - a newer main-chain assistant record that produced no text and no
//     terminal stop_reason (a tool_use/thinking-only record) means the turn is
//     still in flight: unready no_assistant_message.
func (r *ClaudeHistoryReader) ReadCompact(ref HistoryRef) (CompactAgentHistory, error) {
	transcript, err := r.readTranscript(ref)
	if err != nil {
		return CompactAgentHistory{}, err
	}
	messages := LimitMessages(transcript.messages, nil)
	compact := CompactFromMessages(ref, messages)
	lastAssistant := lastByRole(messages, RoleAssistant)
	if lastAssistant == nil {
		return compact, nil
	}
	lastAssistantIndex := -1
	lastUserIndex := -1
	for index, message := range messages {
		switch message.Role {
		case RoleAssistant:
			lastAssistantIndex = index
		case RoleUser:
			lastUserIndex = index
		}
	}
	if lastUserIndex > lastAssistantIndex {
		// The last assistant message belongs to the previous turn: a newer user
		// instruction (queued while CC was busy, then sent) came after it, so the
		// finished reply is not this turn's reply.
		reason := UnreadyAssistantOnlyBeforeLastUser
		compact.LastAssistantMessage = nil
		compact.UnreadyReason = &reason
		return compact, nil
	}
	if transcript.tailPendingAssistant {
		// The newest main-chain assistant record is a tool_use/thinking-only
		// record with no terminal stop_reason: the turn is still in flight, so
		// the last assistant message is not the finished reply.
		reason := UnreadyNoAssistantMessage
		compact.LastAssistantMessage = nil
		compact.UnreadyReason = &reason
		return compact, nil
	}
	if lastAssistant.StopReason == nil {
		// No stop_reason: CC has appended the assistant record but is still
		// streaming into it, so it is not the finished reply.
		reason := UnreadyNoAssistantMessage
		compact.LastAssistantMessage = nil
		compact.UnreadyReason = &reason
		return compact, nil
	}
	if _, terminal := claudeTerminalStopReasons[*lastAssistant.StopReason]; !terminal {
		// "tool_use" and every other non-terminal stop reason: the last
		// assistant record is a middle turn, not the completed reply.
		reason := UnreadyNoAssistantMessage
		compact.LastAssistantMessage = nil
		compact.UnreadyReason = &reason
		return compact, nil
	}
	if compact.LastAssistantMessage != nil {
		compact.LastAssistantMessage.StopReason = lastAssistant.StopReason
	}
	return compact, nil
}

// recordList returns the object entries of a JSON array field; a non-array or
// absent field yields nothing.
func recordList(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	objects := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			objects = append(objects, object)
		}
	}
	return objects
}

// joinedClaudeText joins the extracted text blocks; blank blocks were already
// dropped by the callers, so an empty result means "no text".
func joinedClaudeText(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	text := ""
	for index, part := range parts {
		if index > 0 {
			text += "\n"
		}
		text += part
	}
	return text
}
