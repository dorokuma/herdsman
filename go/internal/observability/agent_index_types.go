package observability

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// This file ports the types, constants and pure helpers of
// src/observability/agent-index-service.ts.
//
// Import-cycle note: internal/db imports internal/observability, so the store
// records and query inputs this service passes around are declared here and
// aliased back into internal/db (the pattern internal/db already uses for
// ListAfterInput / NextDeliverableAfterInput). The stores themselves are
// expressed as interfaces below, which *db.AgentStore, *db.AgentEventStore,
// *db.StatusEventPlanStore, *db.HerdrSessionStore, *db.HerdrWorkspaceStore,
// *db.AgentOrchestratorScopeStore and *agenthistory.Service satisfy as they
// stand.

// STALE_DUPLICATE_GUARD_SCAN_LIMIT mirrors the TS constant of the same name:
// how many prior terminal rows the stale-baseline duplicate guard compares
// against, newest first.
const STALE_DUPLICATE_GUARD_SCAN_LIMIT = 500

// WORKING_PLAN_MAX_STALL_ATTEMPTS mirrors the TS constant of the same name.
const WORKING_PLAN_MAX_STALL_ATTEMPTS = 18

// WORKING_PLAN_MAX_WAIT_MS mirrors the TS constant of the same name.
const WORKING_PLAN_MAX_WAIT_MS = 30 * 60 * 1000

// ORPHAN_SALVAGE_REASON mirrors the TS constant of the same name: the `reason`
// carried by the orphan terminal row written when a plan still held deliverable
// terminal content but its `agents` row was already gone.
const ORPHAN_SALVAGE_REASON = "pane_closed_before_delivery"

// PlanWaitingHistoryError mirrors the TS class of the same name.
type PlanWaitingHistoryError struct{}

func (e *PlanWaitingHistoryError) Error() string { return PlanWaitingHistory }

// Name mirrors `this.name = "PlanWaitingHistoryError"`.
func (e *PlanWaitingHistoryError) Name() string { return "PlanWaitingHistoryError" }

// retryRingAuthorized mirrors retryRingAuthorized: only a genuine
// PlanWaitingHistoryError may (re)arm the retry ring.
func retryRingAuthorized(err error) bool {
	var waiting *PlanWaitingHistoryError
	return errors.As(err, &waiting)
}

// HistoryHasAdvanced mirrors historyHasAdvanced. A nil options means the caller
// passed no options object at all.
func HistoryHasAdvanced(
	current CompactAgentHistory,
	baseline *CompactAgentHistory,
	options *HistoryHasAdvancedOptions,
) bool {
	if current.LastAssistantMessage != nil {
		baselineRef := ""
		if baseline != nil && baseline.LastAssistantMessage != nil {
			baselineRef = baseline.LastAssistantMessage.Ref
		}
		if current.LastAssistantMessage.Ref != "" &&
			current.LastAssistantMessage.Ref != baselineRef {
			return true
		}
		baselineText := ""
		if baseline != nil && baseline.LastAssistantMessage != nil {
			baselineText = baseline.LastAssistantMessage.Text
		}
		if current.LastAssistantMessage.Text != "" &&
			current.LastAssistantMessage.Text != baselineText {
			return true
		}
	}
	if options != nil && options.RequireAssistantChange {
		return false
	}
	baselineCount := 0
	if baseline != nil {
		baselineCount = baseline.MessageCount
	}
	return current.MessageCount > baselineCount
}

// HistoryHasAdvancedOptions mirrors the TS options object.
type HistoryHasAdvancedOptions struct {
	RequireAssistantChange bool
}

// isTerminalAssistant mirrors isTerminalAssistant.
func isTerminalAssistant(compact *CompactAgentHistory) bool {
	if compact == nil || compact.LastAssistantMessage == nil ||
		compact.LastAssistantMessage.StopReason == nil {
		return false
	}
	stopReason := *compact.LastAssistantMessage.StopReason
	return stopReason == "stop" || stopReason == "length"
}

// HasNonEmptyAssistantMessage mirrors hasNonEmptyAssistantMessage: the compact
// history counts as carrying a turn only when its last assistant message has
// non-blank text. internal/db re-exports it, because the shared delivery
// predicate lives there and cannot import this package back.
func HasNonEmptyAssistantMessage(compactHistory *CompactAgentHistory) bool {
	if compactHistory == nil || compactHistory.LastAssistantMessage == nil {
		return false
	}
	return strings.TrimSpace(compactHistory.LastAssistantMessage.Text) != ""
}

// AgentIndexRefreshResult mirrors the TS type of the same name.
type AgentIndexRefreshResult struct {
	Agents               []AgentIndexRecord
	ContextChangedScopes []AgentScope
	Events               []AgentEventRecord
}

// AgentEventHandlingResult mirrors the TS type of the same name.
type AgentEventHandlingResult struct {
	ContextChangedScopes []AgentScope
	Events               []AgentEventRecord
}

// PiSessionRefRegistrationResult mirrors the TS type of the same name.
type PiSessionRefRegistrationResult struct {
	Agent                *AgentIndexRecord
	ContextChangedScopes []AgentScope
}

// RefreshInput mirrors the TS type of the same name.
type RefreshInput struct {
	HerdrSessionName string
	SessionDir       string
	SocketPath       string
}

// StatusEventPlan mirrors the TS type of the same name.
type StatusEventPlan struct {
	Agent          AgentIndexRecord
	Attempts       *int
	CompactHistory *CompactAgentHistory
	From           AgentStatus
	HerdrEventKey  *string
	PlanID         *int64
	To             AgentStatus
}

// AgentIndexRefreshFastResult mirrors the TS type of the same name.
type AgentIndexRefreshFastResult struct {
	Agents               []AgentIndexRecord
	ContextChangedScopes []AgentScope
	Events               []AgentEventRecord
	StatusEventPlans     []StatusEventPlan
}

// AgentEventHandlingFastResult mirrors the TS type of the same name.
type AgentEventHandlingFastResult struct {
	ContextChangedScopes []AgentScope
	Events               []AgentEventRecord
	StatusEventPlans     []StatusEventPlan
}

// ---------------------------------------------------------------------------
// Store interfaces. Each method set is a subset of the concrete db store, so the
// stores satisfy them without an adapter.
// ---------------------------------------------------------------------------

// StatusPlansConfig mirrors the TS `Required<StatusPlansConfig>` the service
// normalises its options into. internal/config owns the defaults; the wiring
// layer maps them onto this shape.
type StatusPlansConfig struct {
	BackoffFactor    float64
	InitialBackoffMs float64
	MaxBackoffMs     float64
	TotalWindowMs    float64
}

// AgentIndexServiceStores mirrors the TS type of the same name.
type AgentIndexServiceStores struct {
	AgentContextSnapshots   AgentContextSnapshotStoreLike
	AgentEvents             AgentIndexEventStoreLike
	AgentOrchestratorScopes AgentIndexScopeStoreLike
	AgentHistoryCache       AgentHistoryCacheStoreLike
	Agents                  AgentIndexAgentStoreLike
	HerdrSessions           HerdrSessionStoreLike
	HerdrWorkspaces         HerdrWorkspaceStoreLike
	Sqlite                  *sql.DB
	StatusEventPlans        StatusEventPlanStoreLike
}

// AgentIndexAgentStoreLike is the slice of *db.AgentStore the index service
// depends on: the base methods (declared next to AgentOrchestratorService as
// AgentStoreLike) plus the pane lookups and the session replace the index
// service performs.
type AgentIndexAgentStoreLike interface {
	AgentStoreLike
	ClearLegacyPaneTombstone(input ClearLegacyPaneTombstoneInput) error
	Exists(id string) (bool, error)
	FindByPane(input FindAgentByPaneInput) (*AgentIndexRecord, error)
	FindByTerminal(input FindAgentByTerminalInput) (*AgentIndexRecord, error)
	HasGenerationScopedTombstone(input HasGenerationScopedTombstoneInput) (bool, error)
	IsPaneClosed(input IsPaneClosedInput) (bool, error)
	ListForHerdrSession(herdrSessionName string) ([]AgentIndexRecord, error)
	ReplaceForSession(input ReplaceAgentsForSessionInput) ([]AgentIndexRecord, error)
	RetirePane(input RetirePaneInput) ([]AgentIndexRecord, error)
	SetSessionRefByTerminal(input SetSessionRefByTerminalInput) (*AgentIndexRecord, error)
	UpdateStatus(input UpdateAgentStatusInput) (*AgentIndexRecord, error)
}

// AgentIndexEventStoreLike is the slice of *db.AgentEventStore the index
// service depends on: the orchestrator service's slice plus the writes and
// reads only the index service performs.
type AgentIndexEventStoreLike interface {
	AgentEventStoreLike
	Append(input AppendAgentEventInput) (AgentEventRecord, error)
	HasTerminalEventAfter(agentID string, herdrSessionName string, afterID int64) (bool, error)
	InvalidateByID(id int64, reason string) (bool, error)
	InvalidatePane(input InvalidatePaneInput) error
	LatestCompletedTurnEvent(
		agentID string,
		herdrSessionName string,
	) (*AgentEventRecord, error)
	LatestStatusTransition(
		agentID string,
		herdrSessionName string,
	) (*AgentEventRecord, error)
	LatestTerminalEvent(
		agentID string,
		herdrSessionName string,
	) (*AgentEventRecord, error)
	// DeliveredTerminalHistories is the bounded delivered/acked terminal window
	// the stale-baseline duplicate guard compares against.
	DeliveredTerminalHistories(
		input DeliveredTerminalHistoriesInput,
	) ([]DeliveredTerminalHistoryRow, error)
	LatestPaneEventScope(input LatestPaneEventScopeInput) (*LatestPaneEventScopeRow, error)
	// PaneEventTail is the `order by id desc limit N` read over one pane's own
	// event history that #salvageableTerminalBody performs.
	PaneEventTail(input PaneEventTailInput) ([]PaneEventTailRow, error)
	// PaneDeliveredTerminalBodies is the bounded delivered/acked window the
	// salvage path compares against.
	PaneDeliveredTerminalBodies(
		input PaneDeliveredTerminalBodiesInput,
	) ([]PaneDeliveredTerminalBodyRow, error)
	// PlanOutcomeEventExists is the idempotency probe the failed/discarded
	// backfills run before appending.
	PlanOutcomeEventExists(herdrSessionName string, idempotencyKey string) (bool, error)
	EventIDByIdempotencyKey(idempotencyKey string) (int64, bool, error)
}

// StatusEventPlanStoreLike is the slice of *db.StatusEventPlanStore the index
// service depends on.
type StatusEventPlanStoreLike interface {
	// CancelSupersededPendingPlans returns the number of rows it cancelled,
	// mirroring the TS `Number(result.changes)` the caller logs.
	CancelSupersededPendingPlans(input CancelSupersededPendingPlansInput) (int, error)
	Get(id int64) (StatusEventPlanRecord, error)
	HasCompletedPlanAfter(agentID string, herdrSessionName string, afterID int64) (bool, error)
	InsertPending(input InsertPendingInput) (StatusEventPlanRecord, error)
	LatestStalledDiscard(
		agentID string,
		herdrSessionName string,
	) (*StalledDiscardPlanRow, error)
	ListDiscarded() ([]StatusEventPlanRecord, error)
	ListFailed() ([]StatusEventPlanRecord, error)
	ListUnfinished() ([]StatusEventPlanRecord, error)
	ListWaitingHistory() ([]StatusEventPlanRecord, error)
	MarkCancelled(id int64) error
	MarkCompleted(id int64) error
	MarkRetry(id int64, err error, options *MarkRetryOptions) (*StatusEventPlanRecord, error)
	MarkRunning(id int64) (bool, error)
	ResetRunningToPending() (int, error)
}

// HerdrSessionStoreLike is the slice of *db.HerdrSessionStore the index service
// depends on.
type HerdrSessionStoreLike interface {
	FindRunningBySocketPath(socketPath string) (*HerdrSessionRecord, error)
	ListRunning() ([]HerdrSessionRecord, error)
	UpsertRunning(input UpsertRunningHerdrSessionInput) (HerdrSessionRecord, error)
}

// HerdrWorkspaceStoreLike is the slice of *db.HerdrWorkspaceStore the index
// service depends on.
type HerdrWorkspaceStoreLike interface {
	ListForSession(herdrSessionName string) ([]HerdrWorkspaceRecord, error)
	ReplaceForSession(input ReplaceHerdrWorkspacesInput) ([]HerdrWorkspaceRecord, error)
}

// AgentIndexScopeStoreLike is the slice of *db.AgentOrchestratorScopeStore the
// index service depends on. The rest of that store's interface lives in
// agent_orchestrator_service.go.
type AgentIndexScopeStoreLike interface {
	Get(scope AgentScope) (*AgentOrchestratorState, error)
	ReleaseIfOwnerIdentity(
		input ReleaseIfOwnerIdentityInput,
	) (AgentOrchestratorReleaseResult, error)
	ReleaseIfStaleOwner(input ReleaseIfStaleOwnerInput) (bool, error)
}

// ReleaseIfOwnerIdentityInput mirrors the TS input of the same name.
type ReleaseIfOwnerIdentityInput struct {
	AgentOrchestratorScopeKey
	PaneID     string
	TerminalID string
}

// ReleaseIfStaleOwnerInput mirrors the releaseIfStaleOwner input. The live id
// sets are the panes and terminals a scan still sees.
type ReleaseIfStaleOwnerInput struct {
	AgentOrchestratorScopeKey
	LivePaneIDs     map[string]struct{}
	LiveTerminalIDs map[string]struct{}
}

// AgentHistoryCacheStoreLike is the slice of *db.AgentHistoryCacheStore the
// index service depends on.
type AgentHistoryCacheStoreLike interface {
	DeleteOlderThan(ageMs int64) (int, error)
}

// ---------------------------------------------------------------------------
// Raw-SQL read models the index service needs from the events table. They live
// in this package (and are aliased into internal/db) for the same import-cycle
// reason as the records above.
// ---------------------------------------------------------------------------

// PaneEventTailInput mirrors the parameter set of the newest-first pane read.
type PaneEventTailInput struct {
	CreatedBeforeMs  int64
	HerdrSessionName string
	Limit            int
	PaneGeneration   *string
	PaneID           string
}

// PaneEventTailRow mirrors one row of that read.
type PaneEventTailRow struct {
	CompactHistory   *CompactAgentHistory
	DeliveryAttempts int
}

// PaneDeliveredTerminalBodiesInput mirrors the parameter set of the
// delivered/acked terminal window read.
type PaneDeliveredTerminalBodiesInput struct {
	CreatedBeforeMs  int64
	HerdrSessionName string
	Limit            int
	PaneGeneration   *string
	PaneID           string
}

// PaneDeliveredTerminalBodyRow mirrors one row of that read.
type PaneDeliveredTerminalBodyRow struct {
	AgentKind      *string
	CompactHistory *CompactAgentHistory
}

// DeliveredTerminalHistoriesInput mirrors the parameter set of the bounded
// delivered/acked terminal window read the stale-baseline duplicate guard runs.
type DeliveredTerminalHistoriesInput struct {
	AgentID          string
	HerdrSessionName string
	Limit            int
}

// DeliveredTerminalHistoryRow mirrors one row of that read.
type DeliveredTerminalHistoryRow struct {
	CompactHistory *CompactAgentHistory
}

// LatestPaneEventScopeInput mirrors the parameter set of the newest-first pane
// scope read #minimalAgentFromLatestEvent performs.
type LatestPaneEventScopeInput struct {
	CreatedBeforeMs  int64
	HerdrSessionName string
	PaneGeneration   *string
	PaneID           string
}

// LatestPaneEventScopeRow mirrors one row of that read.
type LatestPaneEventScopeRow struct {
	HerdrSessionName string
	PaneGeneration   *string
	PaneID           *string
	TerminalID       *string
	WorkspaceID      *string
}

// StalledDiscardPlanRow mirrors the row #isAgentStalled reads: the newest
// discarded plan of one identity whose last_error is in STALLED_PLAN_ERRORS.
type StalledDiscardPlanRow struct {
	ID          int64
	LastError   *string
	UpdatedAtMs int64
}

// FindAgentByPaneInput mirrors the TS findByPane input.
type FindAgentByPaneInput struct {
	HerdrSessionName string
	PaneGeneration   *string
	PaneID           string
}

// IsPaneClosedInput mirrors the TS isPaneClosed / isPaneClosedSince input.
type IsPaneClosedInput struct {
	HerdrSessionName string
	PaneGeneration   *string
	PaneID           string
	SinceMs          int64
}

// ReplaceAgentsForSessionInput mirrors the TS replaceForSession input.
type ReplaceAgentsForSessionInput struct {
	Agents           []HerdrAgentLike
	HerdrSessionName string
}

// HerdrAgentLike mirrors the TS alias of the same name: one entry of the
// snake_case `agent.list` / `session.snapshot` payload Herdr reports.
type HerdrAgentLike = map[string]any

// AppendAgentEventInput mirrors the TS append input.
type AppendAgentEventInput struct {
	AgentID          *string
	CompactHistory   *CompactAgentHistory
	HerdrSessionName string
	IdempotencyKey   *string
	PaneID           *string
	PaneGeneration   *string
	Payload          any
	TerminalID       *string
	Type             AgentEventType
	WorkspaceID      *string
}

// InvalidatePaneInput mirrors the TS invalidatePane / invalidatePaneDirect
// input.
type InvalidatePaneInput struct {
	AcknowledgeDelivered *bool
	HerdrSessionName     string
	InvalidatedReason    *string
	PaneGeneration       *string
	PaneID               string
}

// AckSelfOwnedInput mirrors the TS ackSelfOwned input.
type AckSelfOwnedInput struct {
	HerdrSessionName string
	PaneID           string
	WorkspaceID      string
}

// UpsertRunningHerdrSessionInput mirrors the TS input of the same name.
type UpsertRunningHerdrSessionInput struct {
	Name       string
	SessionDir string
	SocketPath string
}

// ReplaceHerdrWorkspacesInput mirrors the replaceForSession input.
type ReplaceHerdrWorkspacesInput struct {
	HerdrSessionName string
	Workspaces       []map[string]any
}

// FindAgentByTerminalInput mirrors the TS findByTerminal input.
type FindAgentByTerminalInput struct {
	HerdrSessionName string
	TerminalID       string
}

// ClearLegacyPaneTombstoneInput mirrors the TS input of the same name.
type ClearLegacyPaneTombstoneInput struct {
	HerdrSessionName string
	PaneID           string
}

// HasGenerationScopedTombstoneInput mirrors the TS input of the same name.
type HasGenerationScopedTombstoneInput struct {
	HerdrSessionName string
	PaneGeneration   string
	PaneID           string
}

// SetSessionRefByTerminalInput mirrors the TS input of the same name.
type SetSessionRefByTerminalInput struct {
	AgentSession     AgentSessionRef
	HerdrSessionName string
	TerminalID       string
}

// UpdateAgentStatusInput mirrors the TS updateStatus input.
type UpdateAgentStatusInput struct {
	AgentStatus      AgentStatus
	HerdrSessionName string
	PaneGeneration   *string
	PaneID           string
}

// RetirePaneInput mirrors the TS retirePane / markPaneClosed input.
type RetirePaneInput struct {
	HerdrSessionName string
	PaneGeneration   *string
	PaneID           string
}

// ---------------------------------------------------------------------------
// Status event plan records and store inputs.
// ---------------------------------------------------------------------------

// StatusPlanMaxAttempts mirrors STATUS_PLAN_MAX_ATTEMPTS.
const StatusPlanMaxAttempts = 8

// StatusEventPlanStatus mirrors the TS union of the same name.
type StatusEventPlanStatus string

const (
	StatusPlanPending   StatusEventPlanStatus = "pending"
	StatusPlanRunning   StatusEventPlanStatus = "running"
	StatusPlanCompleted StatusEventPlanStatus = "completed"
	StatusPlanCancelled StatusEventPlanStatus = "cancelled"
	StatusPlanFailed    StatusEventPlanStatus = "failed"
	StatusPlanDiscarded StatusEventPlanStatus = "discarded"
)

// StatusEventPlanRecord mirrors the TS record of the same name. The timestamps
// are ISO strings and the nullable columns stay pointers, so an absent value is
// distinguishable from a stored one.
type StatusEventPlanRecord struct {
	AgentID          string                `json:"agentId"`
	Attempts         int                   `json:"attempts"`
	CompactHistory   *CompactAgentHistory  `json:"compactHistory,omitempty"`
	CreatedAt        string                `json:"createdAt"`
	CreatedAtMs      int64                 `json:"-"`
	FromStatus       AgentStatus           `json:"fromStatus"`
	HerdrEventKey    *string               `json:"herdrEventKey"`
	HerdrSessionName string                `json:"herdrSessionName"`
	ID               int64                 `json:"id"`
	LastError        *string               `json:"lastError"`
	PaneGeneration   *string               `json:"paneGeneration"`
	PaneID           string                `json:"paneId"`
	Status           StatusEventPlanStatus `json:"status"`
	ToStatus         AgentStatus           `json:"toStatus"`
	UpdatedAt        string                `json:"updatedAt"`
}

// StatusEventPlanAgent mirrors the nested agent shape of the TS insertPending
// overload.
type StatusEventPlanAgent struct {
	HerdrSessionName string
	ID               string
	PaneGeneration   *string
	PaneID           string
}

// InsertPendingInput mirrors the union the TS insertPending implementation
// accepts: either the flat fields or the nested agent with from/to.
type InsertPendingInput struct {
	Agent            *StatusEventPlanAgent
	AgentID          string
	CompactHistory   *CompactAgentHistory
	From             AgentStatus
	FromStatus       AgentStatus
	HerdrEventKey    *string
	HerdrSessionName string
	PaneGeneration   *string
	PaneID           string
	To               AgentStatus
	ToStatus         AgentStatus
}

// MarkRetryOptions mirrors the TS markRetry options. The zero values mean "not
// supplied": IsTargetWorking and StallDetected default to false, MaxAttempts to
// StatusPlanMaxAttempts, and the window is only checked when TotalWindowMs is
// set.
type MarkRetryOptions struct {
	IsTargetWorking bool
	IsWindowExpired bool
	MaxAttempts     int
	StallDetected   bool
	TotalWindowMs   *int64
}

// CancelSupersededPendingPlansInput mirrors the TS input of the same name.
type CancelSupersededPendingPlansInput struct {
	AgentID          string
	HerdrSessionName string
	KeepID           int64
	ToStatus         AgentStatus
}

// ---------------------------------------------------------------------------
// Pure helpers ported from the module scope of agent-index-service.ts.
// ---------------------------------------------------------------------------

// stalledPlanErrorFilter mirrors the `?` placeholder list built over
// STALLED_PLAN_ERRORS for the `last_error in (...)` filter.
var stalledPlanErrorFilter = func() string {
	placeholders := make([]string, len(StalledPlanErrors))
	for index := range StalledPlanErrors {
		placeholders[index] = "?"
	}
	return strings.Join(placeholders, ", ")
}()

// paneIdentityKey mirrors paneIdentityKey.
func paneIdentityKey(paneID string, paneGeneration *string) string {
	generation := ""
	if paneGeneration != nil {
		generation = *paneGeneration
	}
	return paneID + "\x00" + generation
}

// emptyTurnAlertKey mirrors emptyTurnAlertKey.
func emptyTurnAlertKey(herdrSessionName string, paneID *string) string {
	pane := ""
	if paneID != nil {
		pane = *paneID
	}
	return herdrSessionName + "\x00" + pane
}

// stringValue mirrors stringValue.
func stringValue(value any) *string {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	return &text
}

// integerValue mirrors integerValue.
func integerValue(value any) *int {
	number, ok := value.(float64)
	if !ok || number != float64(int(number)) || number < 0 {
		return nil
	}
	asInt := int(number)
	return &asInt
}

// record mirrors record: the payload as a map, or an empty map.
func record(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

// paneGenerationFromEvent mirrors paneGenerationFromEvent.
func paneGenerationFromEvent(event map[string]any) *string {
	return stringValue(event["pane_generation"])
}

// parseCompactHistoryJson mirrors parseCompactHistoryJson.
func parseCompactHistoryJson(value *string) *CompactAgentHistory {
	if value == nil || *value == "" {
		return nil
	}
	var compact CompactAgentHistory
	if err := json.Unmarshal([]byte(*value), &compact); err != nil {
		return nil
	}
	return &compact
}

// agentKindFromPayloadJson mirrors agentKindFromPayloadJson.
func agentKindFromPayloadJson(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	var payload any
	if err := json.Unmarshal([]byte(*value), &payload); err != nil {
		return nil
	}
	agent, ok := record(payload)["agent"].(string)
	if !ok {
		return nil
	}
	return &agent
}

// sameTerminalAssistantContent mirrors sameTerminalAssistantContent.
func sameTerminalAssistantContent(
	current *CompactAgentHistory,
	previous *CompactAgentHistory,
	agentKind *string,
) bool {
	if current == nil || previous == nil ||
		current.LastAssistantMessage == nil || previous.LastAssistantMessage == nil {
		return false
	}
	currentMessage := current.LastAssistantMessage
	previousMessage := previous.LastAssistantMessage
	kind := ""
	if agentKind != nil {
		kind = *agentKind
	}
	if kind == "agy" || (current.Source != nil && *current.Source == "antigravity-sqlite") {
		if currentMessage.Ref != "" && previousMessage.Ref != "" {
			return currentMessage.Ref == previousMessage.Ref
		}
		return currentMessage.Text != "" && currentMessage.Text == previousMessage.Text
	}
	// Non-agy (pi):
	if currentMessage.Ref != "" && previousMessage.Ref != "" {
		return currentMessage.Ref == previousMessage.Ref
	}
	if len(currentMessage.Text) > 0 && len(previousMessage.Text) > 0 {
		return currentMessage.Text == previousMessage.Text
	}
	return false
}

// statusTransitionMatches mirrors statusTransitionMatches.
func statusTransitionMatches(event AgentEventRecord, from AgentStatus, to AgentStatus) bool {
	payload := record(event.Payload)
	eventFrom, _ := payload["from"].(string)
	eventTo, _ := payload["to"].(string)
	return AgentStatus(eventFrom) == from && AgentStatus(eventTo) == to
}
