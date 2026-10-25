package agenthistory

// PiHistoryReader, ported from src/agent-history/pi-reader.ts.

// PiHistoryReader reads Pi session JSONL files.
type PiHistoryReader struct{}

// NewPiHistoryReader builds a reader.
func NewPiHistoryReader() *PiHistoryReader { return &PiHistoryReader{} }

// CanRead mirrors PiHistoryReader.canRead: source must be pi-jsonl and either
// path or value must be non-empty.
func (r *PiHistoryReader) CanRead(ref HistoryRef) bool {
	if ref.Source != SourcePiJSONL {
		return false
	}
	if ref.Path != nil && *ref.Path != "" {
		return true
	}
	return ref.Value != ""
}

// Read mirrors PiHistoryReader.read(ref, options).
func (r *PiHistoryReader) Read(ref HistoryRef, limit *int) ([]HistoryMessage, error) {
	path := ref.ResolvedPath()
	entries, err := ReadJSONL(path, ReadJSONLOptions{})
	if err != nil {
		return nil, err
	}

	messages := make([]HistoryMessage, 0, len(entries))
	for _, entry := range entries {
		message := recordOf(entry.Value["message"])
		role, hasRole := stringValue(message["role"])
		if entry.Value["type"] != "message" || !hasRole {
			continue
		}
		id, hasID := stringValue(entry.Value["id"])
		var idPtr *string
		if hasID {
			idPtr = &id
		}
		timestamp := TimestampFrom(entry.Value["timestamp"])
		if timestamp == nil {
			timestamp = TimestampFrom(message["timestamp"])
		}
		refValue := MessageRef(path, idPtr, entry.Line)

		switch role {
		case "user", "assistant":
			text, ok := TextFromContent(message["content"])
			if ok {
				var stopReason *string
				if value, present := stringValue(message["stopReason"]); present {
					stopReason = &value
				}
				messages = append(messages, HistoryMessage{
					Ref:        refValue,
					Role:       MessageRole(role),
					StopReason: stopReason,
					Text:       text,
					Timestamp:  timestamp,
				})
			}
		case "toolResult":
			text, ok := TextFromContent(message["content"])
			if !ok {
				text = ""
			}
			toolName, present := stringValue(message["toolName"])
			if !present {
				toolName = "unknown"
			}
			compact := CompactToolResultValue(CompactToolResultInput{
				IsError:  isTrue(message["isError"]),
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
	return LimitMessages(messages, limit), nil
}

// ReadCompact mirrors PiHistoryReader.readCompact(ref).
func (r *PiHistoryReader) ReadCompact(ref HistoryRef) (CompactAgentHistory, error) {
	messages, err := r.Read(ref, nil)
	if err != nil {
		return CompactAgentHistory{}, err
	}
	return CompactFromMessages(ref, messages), nil
}

func recordOf(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

func stringValue(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || text == "" {
		return "", false
	}
	return text, true
}

func isTrue(value any) bool {
	flag, ok := value.(bool)
	return ok && flag
}
