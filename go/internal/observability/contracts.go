package observability

// AgentStatus mirrors the TS union: "blocked" | "done" | "idle" | "unknown" | "working".
type AgentStatus string

const (
	AgentStatusBlocked AgentStatus = "blocked"
	AgentStatusDone    AgentStatus = "done"
	AgentStatusIdle    AgentStatus = "idle"
	AgentStatusUnknown AgentStatus = "unknown"
	AgentStatusWorking AgentStatus = "working"
)

// AgentEventType mirrors the TS union of the seven `agent.*` event types.
type AgentEventType string

const (
	AgentEventBlocked       AgentEventType = "agent.blocked"
	AgentEventDiscarded     AgentEventType = "agent.discarded"
	AgentEventDone          AgentEventType = "agent.done"
	AgentEventFailed        AgentEventType = "agent.failed"
	AgentEventIdle          AgentEventType = "agent.idle"
	AgentEventStatusChanged AgentEventType = "agent.status.changed"
	AgentEventToolFailed    AgentEventType = "agent.tool.failed"
)

// AgentEventStatus mirrors the delivery-state union on AgentEventRecord.
type AgentEventStatus string

const (
	AgentEventStatusPending     AgentEventStatus = "pending"
	AgentEventStatusDelivered   AgentEventStatus = "delivered"
	AgentEventStatusAcked       AgentEventStatus = "acked"
	AgentEventStatusInvalidated AgentEventStatus = "invalidated"
	AgentEventStatusFailed      AgentEventStatus = "failed"
)

// AgentHistoryUnreadyReason mirrors the TS union of four unready reasons.
type AgentHistoryUnreadyReason string

const (
	UnreadyEmptyHistory       AgentHistoryUnreadyReason = "empty_history"
	UnreadyNoUserMessage      AgentHistoryUnreadyReason = "no_user_message"
	UnreadyAssistantOnly      AgentHistoryUnreadyReason = "assistant_only_before_last_user"
	UnreadyNoAssistantMessage AgentHistoryUnreadyReason = "no_assistant_message"
)

// CompactionMode mirrors the CompactToolResult compaction mode union.
type CompactionMode string

const (
	CompactionFailureFocus         CompactionMode = "failure_focus"
	CompactionGroupedMatches       CompactionMode = "grouped_matches"
	CompactionStructuredSummary    CompactionMode = "structured_summary"
	CompactionTruncatedPassthrough CompactionMode = "truncated_passthrough"
	CompactionWebSources           CompactionMode = "web_sources"
	CompactionUnknown              CompactionMode = "unknown"
)

// AgentHistoryRefKind mirrors `kind: "agent_session" | "discovered_file"`.
type AgentHistoryRefKind string

const (
	HistoryRefAgentSession   AgentHistoryRefKind = "agent_session"
	HistoryRefDiscoveredFile AgentHistoryRefKind = "discovered_file"
)

// AgentHistoryRefSource mirrors the history ref source union.
type AgentHistoryRefSource string

const (
	HistorySourceAntigravitySQLite AgentHistoryRefSource = "antigravity-sqlite"
	HistorySourcePiJSONL           AgentHistoryRefSource = "pi-jsonl"
	HistorySourceUnknown           AgentHistoryRefSource = "unknown"
)

// AgentMessageRole mirrors the AgentHistoryMessage role union.
type AgentMessageRole string

const (
	RoleAssistant  AgentMessageRole = "assistant"
	RoleToolResult AgentMessageRole = "tool_result"
	RoleUser       AgentMessageRole = "user"
)

// AgentSessionRefKind mirrors `AgentSessionRef.kind`.
type AgentSessionRefKind string

const (
	SessionRefID   AgentSessionRefKind = "id"
	SessionRefPath AgentSessionRefKind = "path"
)

// SubscriberKind mirrors `PiPresenceRegistration.subscriberKind`.
type SubscriberKind string

const (
	SubscriberPi  SubscriberKind = "pi"
	SubscriberAgy SubscriberKind = "agy"
)

// AgentOrchestratorChangeReason mirrors the orchestrator change reason union.
type AgentOrchestratorChangeReason string

const (
	OrchestratorChangeClaimed        AgentOrchestratorChangeReason = "claimed"
	OrchestratorChangeDisconnected   AgentOrchestratorChangeReason = "disconnected"
	OrchestratorChangeMoved          AgentOrchestratorChangeReason = "moved"
	OrchestratorChangeReleased       AgentOrchestratorChangeReason = "released"
	OrchestratorChangeStartupTimeout AgentOrchestratorChangeReason = "startup_timeout"
)

// Frozen degraded-release metric-key contract. The string values are the
// contract: operators and dashboards key on them.
const (
	DegradedEventMessage  = "Herdsman emitted degraded status event"
	EmptyTurnAlertReason  = "upstream_empty_turn"
	EmptyTurnAlertMessage = "Herdsman observed an empty upstream turn"
	DegradedPlanError     = "degraded"
	PlanWaitingHistory    = "PLAN_WAITING_HISTORY"
	PlanStalled           = "PLAN_STALLED"
)

// DegradedReason mirrors DEGRADED_REASON.
type DegradedReason string

const (
	DegradedReasonExpectedTextMismatch DegradedReason = "expected_text_mismatch"
	DegradedReasonNoAdvanceFromInput   DegradedReason = "no_advance_from_input"
	DegradedReasonNonTerminalAssistant DegradedReason = "non_terminal_assistant"
	DegradedReasonStaleBaselineDup     DegradedReason = "stale_baseline_duplicate"
)

// DegradedReasonByKey maps the TS const-object keys to their frozen values.
var DegradedReasonByKey = map[string]DegradedReason{
	"expectedTextMismatch":   DegradedReasonExpectedTextMismatch,
	"noAdvanceFromInput":     DegradedReasonNoAdvanceFromInput,
	"nonTerminalAssistant":   DegradedReasonNonTerminalAssistant,
	"staleBaselineDuplicate": DegradedReasonStaleBaselineDup,
}

// StalledPlanErrors mirrors STALLED_PLAN_ERRORS, in source order.
var StalledPlanErrors = []string{PlanWaitingHistory, PlanStalled, DegradedPlanError}

// ValidAgentEventTypes mirrors VALID_AGENT_EVENT_TYPES. The TS set holds six
// values and deliberately omits `agent.tool.failed`, even though the type union
// includes it; that asymmetry is part of the contract and is preserved here.
var ValidAgentEventTypes = map[string]struct{}{
	string(AgentEventStatusChanged): {},
	string(AgentEventIdle):          {},
	string(AgentEventDone):          {},
	string(AgentEventBlocked):       {},
	string(AgentEventFailed):        {},
	string(AgentEventDiscarded):     {},
}

// ParseAgentStatus mirrors parseAgentStatus: anything outside the five known
// statuses collapses to "unknown".
func ParseAgentStatus(value string) AgentStatus {
	switch AgentStatus(value) {
	case AgentStatusBlocked:
		return AgentStatusBlocked
	case AgentStatusDone:
		return AgentStatusDone
	case AgentStatusIdle:
		return AgentStatusIdle
	case AgentStatusWorking:
		return AgentStatusWorking
	default:
		return AgentStatusUnknown
	}
}

// ---------------------------------------------------------------------------
// Records. Nullable TS fields become pointers so JSON null and "absent" stay
// distinguishable; `?:` optional fields are also pointers but taggged
// `omitempty` so marshaling reproduces the TS shape.
// ---------------------------------------------------------------------------

// HerdrSessionRecord mirrors the TS record of the same name.
type HerdrSessionRecord struct {
	LastScannedAt *string `json:"lastScannedAt"`
	Name          string  `json:"name"`
	Running       bool    `json:"running"`
	SessionDir    string  `json:"sessionDir"`
	SocketPath    string  `json:"socketPath"`
	UpdatedAt     string  `json:"updatedAt"`
}

// HerdrWorkspaceRecord mirrors the TS record of the same name.
type HerdrWorkspaceRecord struct {
	AgentStatus      AgentStatus `json:"agentStatus"`
	Focused          bool        `json:"focused"`
	HerdrSessionName string      `json:"herdrSessionName"`
	Label            *string     `json:"label"`
	LastSeenAt       string      `json:"lastSeenAt"`
	WorkspaceID      string      `json:"workspaceId"`
}

// AgentSessionRef mirrors the TS record of the same name.
type AgentSessionRef struct {
	Agent  string              `json:"agent"`
	Kind   AgentSessionRefKind `json:"kind"`
	Source string              `json:"source"`
	Value  string              `json:"value"`
}

// AgentIndexRecord mirrors the TS record of the same name.
type AgentIndexRecord struct {
	Agent            *string          `json:"agent"`
	AgentSession     *AgentSessionRef `json:"agentSession"`
	AgentStatus      AgentStatus      `json:"agentStatus"`
	Cwd              *string          `json:"cwd"`
	FirstSeenAt      string           `json:"firstSeenAt"`
	Focused          bool             `json:"focused"`
	ForegroundCwd    *string          `json:"foregroundCwd"`
	HerdrSessionName string           `json:"herdrSessionName"`
	ID               string           `json:"id"`
	LastSeenAt       string           `json:"lastSeenAt"`
	Name             *string          `json:"name"`
	PaneID           string           `json:"paneId"`
	TerminalTitle    *string          `json:"terminalTitle,omitempty"`
	PaneRevision     *int             `json:"paneRevision"`
	PaneGeneration   *string          `json:"paneGeneration,omitempty"`
	TabID            *string          `json:"tabId"`
	TerminalID       *string          `json:"terminalId"`
	WorkspaceID      string           `json:"workspaceId"`
}

// AgentHistoryRef mirrors the TS record of the same name.
type AgentHistoryRef struct {
	Kind   AgentHistoryRefKind   `json:"kind"`
	Path   *string               `json:"path,omitempty"`
	Source AgentHistoryRefSource `json:"source"`
	Value  string                `json:"value"`
}

// AgentHistoryExcerpt mirrors the TS record of the same name.
type AgentHistoryExcerpt struct {
	Ref        string  `json:"ref"`
	StopReason *string `json:"stopReason,omitempty"`
	Text       string  `json:"text"`
	Timestamp  *string `json:"timestamp"`
}

// CompactToolResultCompaction mirrors the nested compaction descriptor.
type CompactToolResultCompaction struct {
	Mode          CompactionMode `json:"mode"`
	OriginalChars int            `json:"originalChars"`
	ReturnedChars int            `json:"returnedChars"`
}

// CompactToolResult mirrors the TS record of the same name.
type CompactToolResult struct {
	Compaction CompactToolResultCompaction `json:"compaction"`
	IsError    bool                        `json:"isError"`
	Ref        string                      `json:"ref"`
	Text       string                      `json:"text"`
	ToolName   string                      `json:"toolName"`
}

// CompactAgentHistory mirrors the TS record of the same name.
type CompactAgentHistory struct {
	HistoryRef           *AgentHistoryRef           `json:"historyRef"`
	LastAssistantMessage *AgentHistoryExcerpt       `json:"lastAssistantMessage"`
	LastToolResult       *CompactToolResult         `json:"lastToolResult"`
	LastUserMessage      *AgentHistoryExcerpt       `json:"lastUserMessage"`
	MessageCount         int                        `json:"messageCount"`
	Source               *string                    `json:"source"`
	UnreadyReason        *AgentHistoryUnreadyReason `json:"unreadyReason,omitempty"`
	UpdatedAt            *string                    `json:"updatedAt"`
}

// AgentHistorySourceFingerprint mirrors the TS record of the same name.
type AgentHistorySourceFingerprint struct {
	MtimeMs float64 `json:"mtimeMs"`
	Path    string  `json:"path"`
	Size    float64 `json:"size"`
}

// AgentContextSnapshotRecord mirrors the TS record of the same name.
type AgentContextSnapshotRecord struct {
	AgentID           string                         `json:"agentId"`
	CompactHistory    CompactAgentHistory            `json:"compactHistory"`
	HistoryRef        *AgentHistoryRef               `json:"historyRef"`
	PaneRevision      *int                           `json:"paneRevision"`
	SourceFingerprint *AgentHistorySourceFingerprint `json:"sourceFingerprint"`
	UpdatedAt         string                         `json:"updatedAt"`
}

// AgentHistoryMessage mirrors the TS record of the same name.
type AgentHistoryMessage struct {
	Compact    *CompactToolResult `json:"compact,omitempty"`
	Ref        string             `json:"ref"`
	Role       AgentMessageRole   `json:"role"`
	StopReason *string            `json:"stopReason,omitempty"`
	Text       string             `json:"text"`
	Timestamp  *string            `json:"timestamp"`
	ToolName   *string            `json:"toolName,omitempty"`
}

// AgentListItemHistory mirrors the Pick<> projection on AgentListItem.
type AgentListItemHistory struct {
	LastAssistantMessage *AgentHistoryExcerpt `json:"lastAssistantMessage"`
	LastUserMessage      *AgentHistoryExcerpt `json:"lastUserMessage"`
	Source               *string              `json:"source"`
	UpdatedAt            *string              `json:"updatedAt"`
}

// AgentListItem mirrors `AgentIndexRecord & { history: Pick<...> }`.
type AgentListItem struct {
	AgentIndexRecord
	History AgentListItemHistory `json:"history"`
}

// AgentGetResult mirrors `AgentIndexRecord & { history: CompactAgentHistory }`.
type AgentGetResult struct {
	AgentIndexRecord
	History CompactAgentHistory `json:"history"`
}

// AgentReadResult mirrors `AgentIndexRecord & { historyRef, messages }`.
type AgentReadResult struct {
	AgentIndexRecord
	HistoryRef *AgentHistoryRef      `json:"historyRef"`
	Messages   []AgentHistoryMessage `json:"messages"`
}

// AgentEventRecord mirrors the TS record of the same name.
type AgentEventRecord struct {
	AgentID               *string              `json:"agentId"`
	CompactHistory        *CompactAgentHistory `json:"compactHistory"`
	CreatedAt             string               `json:"createdAt"`
	HerdrSessionName      string               `json:"herdrSessionName"`
	ID                    int64                `json:"id"`
	PaneID                *string              `json:"paneId"`
	PaneGeneration        *string              `json:"paneGeneration,omitempty"`
	Deliverable           int                  `json:"deliverable"`
	Status                *AgentEventStatus    `json:"status,omitempty"`
	DeliveryAttempts      *int                 `json:"deliveryAttempts,omitempty"`
	LastAttemptAt         *string              `json:"lastAttemptAt,omitempty"`
	NextAttemptAt         *string              `json:"nextAttemptAt,omitempty"`
	LastFailureCode       *string              `json:"lastFailureCode,omitempty"`
	InvalidatedReason     *string              `json:"invalidatedReason,omitempty"`
	DeliveredToTerminalID *string              `json:"deliveredToTerminalId,omitempty"`
	Payload               any                  `json:"payload"`
	TerminalID            *string              `json:"terminalId"`
	Type                  AgentEventType       `json:"type"`
	WorkspaceID           *string              `json:"workspaceId"`
}

// AgentScope mirrors the TS record of the same name.
type AgentScope struct {
	HerdrSessionName string `json:"herdrSessionName"`
	WorkspaceID      string `json:"workspaceId"`
}

// AgentWorkspaceContextSnapshot mirrors `AgentScope & { agents, updatedAt }`.
type AgentWorkspaceContextSnapshot struct {
	AgentScope
	Agents    []AgentListItem `json:"agents"`
	UpdatedAt string          `json:"updatedAt"`
}

// AgentOrchestratorOwner mirrors the TS record of the same name.
type AgentOrchestratorOwner struct {
	PaneID     string `json:"paneId"`
	TerminalID string `json:"terminalId"`
}

// AgentOrchestratorState mirrors the TS record (Date updatedAt).
type AgentOrchestratorState struct {
	AgentScope
	AckedEventID int64                   `json:"ackedEventId"`
	Owner        *AgentOrchestratorOwner `json:"owner"`
	UpdatedAt    string                  `json:"updatedAt"`
}

// AgentOrchestratorWireState mirrors the wire variant (string updatedAt).
type AgentOrchestratorWireState struct {
	AgentScope
	AckedEventID int64                   `json:"ackedEventId"`
	Owner        *AgentOrchestratorOwner `json:"owner"`
	UpdatedAt    string                  `json:"updatedAt"`
}

// AgentOrchestratorChanged mirrors the TS record of the same name.
type AgentOrchestratorChanged struct {
	Current  AgentOrchestratorWireState    `json:"current"`
	Previous AgentOrchestratorWireState    `json:"previous"`
	Reason   AgentOrchestratorChangeReason `json:"reason"`
}

// PiPresenceRegistration mirrors the TS record of the same name.
type PiPresenceRegistration struct {
	HerdrSocketPath string          `json:"herdrSocketPath"`
	PaneID          string          `json:"paneId"`
	SessionRef      AgentSessionRef `json:"sessionRef"`
	SubscriberID    string          `json:"subscriberId"`
	SubscriberKind  SubscriberKind  `json:"subscriberKind"`
	WorkspaceID     string          `json:"workspaceId"`
}

// AgentTurnCompletedInput mirrors the TS record of the same name.
type AgentTurnCompletedInput struct {
	Confirmed        bool    `json:"confirmed"`
	ExpectedText     *string `json:"expectedText,omitempty"`
	HerdrSessionName string  `json:"herdrSessionName"`
	PaneID           string  `json:"paneId"`
	TerminalID       string  `json:"terminalId"`
	WorkspaceID      string  `json:"workspaceId"`
}

// AgentQueryScope mirrors the TS record of the same name.
type AgentQueryScope struct {
	All              *bool   `json:"all,omitempty"`
	HerdrSessionName *string `json:"herdrSessionName,omitempty"`
	WorkspaceID      *string `json:"workspaceId,omitempty"`
}
