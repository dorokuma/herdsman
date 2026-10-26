package agenthistory

// Agents history contracts, ported from src/observability/contracts.ts.

// HistorySource mirrors the TS AgentHistoryRef["source"] union.
type HistorySource string

const (
	SourceAntigravitySQLite HistorySource = "antigravity-sqlite"
	SourcePiJSONL           HistorySource = "pi-jsonl"
	SourceUnknown           HistorySource = "unknown"
)

// HistoryKind mirrors the TS AgentHistoryRef["kind"] union.
type HistoryKind string

const (
	KindAgentSession   HistoryKind = "agent_session"
	KindDiscoveredFile HistoryKind = "discovered_file"
)

// HistoryRef mirrors AgentHistoryRef. Path is optional (nil = absent).
type HistoryRef struct {
	Kind   HistoryKind   `json:"kind"`
	Path   *string       `json:"path,omitempty"`
	Source HistorySource `json:"source"`
	Value  string        `json:"value"`
}

// ResolvedPath returns Path when present, else Value. Mirrors `ref.path ?? ref.value`.
func (r HistoryRef) ResolvedPath() string {
	if r.Path != nil {
		return *r.Path
	}
	return r.Value
}

// HistoryExcerpt mirrors AgentHistoryExcerpt.
type HistoryExcerpt struct {
	Ref        string  `json:"ref"`
	StopReason *string `json:"stopReason,omitempty"`
	Text       string  `json:"text"`
	Timestamp  *string `json:"timestamp"`
}

// CompactionMode mirrors CompactToolResult["compaction"]["mode"].
type CompactionMode string

const (
	ModeFailureFocus         CompactionMode = "failure_focus"
	ModeGroupedMatches       CompactionMode = "grouped_matches"
	ModeStructuredSummary    CompactionMode = "structured_summary"
	ModeTruncatedPassthrough CompactionMode = "truncated_passthrough"
	ModeWebSources           CompactionMode = "web_sources"
	ModeUnknown              CompactionMode = "unknown"
)

// ToolCompaction mirrors CompactToolResult["compaction"].
type ToolCompaction struct {
	Mode          CompactionMode `json:"mode"`
	OriginalChars int            `json:"originalChars"`
	ReturnedChars int            `json:"returnedChars"`
}

// CompactToolResult mirrors CompactToolResult.
type CompactToolResult struct {
	Compaction ToolCompaction `json:"compaction"`
	IsError    bool           `json:"isError"`
	Ref        string         `json:"ref"`
	Text       string         `json:"text"`
	ToolName   string         `json:"toolName"`
}

// UnreadyReason mirrors AgentHistoryUnreadyReason. Empty string means absent.
type UnreadyReason string

const (
	UnreadyEmptyHistory                UnreadyReason = "empty_history"
	UnreadyNoUserMessage               UnreadyReason = "no_user_message"
	UnreadyAssistantOnlyBeforeLastUser UnreadyReason = "assistant_only_before_last_user"
	UnreadyNoAssistantMessage          UnreadyReason = "no_assistant_message"
)

// CompactAgentHistory mirrors CompactAgentHistory. In TS `unreadyReason` is
// either absent or a non-null reason, so a nil pointer means the key is absent.
type CompactAgentHistory struct {
	HistoryRef           *HistoryRef        `json:"historyRef"`
	LastAssistantMessage *HistoryExcerpt    `json:"lastAssistantMessage"`
	LastToolResult       *CompactToolResult `json:"lastToolResult"`
	LastUserMessage      *HistoryExcerpt    `json:"lastUserMessage"`
	MessageCount         int                `json:"messageCount"`
	Source               *string            `json:"source"`
	UnreadyReason        *UnreadyReason     `json:"unreadyReason,omitempty"`
	UpdatedAt            *string            `json:"updatedAt"`
}

// SourceFingerprint mirrors AgentHistorySourceFingerprint.
type SourceFingerprint struct {
	MtimeMs float64 `json:"mtimeMs"`
	Path    string  `json:"path"`
	Size    int64   `json:"size"`
}

// MessageRole mirrors AgentHistoryMessage["role"].
type MessageRole string

const (
	RoleAssistant  MessageRole = "assistant"
	RoleToolResult MessageRole = "tool_result"
	RoleUser       MessageRole = "user"
)

// HistoryMessage mirrors AgentHistoryMessage.
type HistoryMessage struct {
	Compact    *CompactToolResult `json:"compact,omitempty"`
	Ref        string             `json:"ref"`
	Role       MessageRole        `json:"role"`
	StopReason *string            `json:"stopReason,omitempty"`
	Text       string             `json:"text"`
	Timestamp  *string            `json:"timestamp"`
	ToolName   *string            `json:"toolName,omitempty"`
}
