package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// RedeliveryFreshnessMs mirrors REDELIVERY_FRESHNESS_MS.
const RedeliveryFreshnessMs = 300_000

// LogicalClockPersistIntervalMs mirrors LOGICAL_CLOCK_PERSIST_INTERVAL_MS.
const LogicalClockPersistIntervalMs = 60_000

// LogicalClockMetaKey mirrors LOGICAL_CLOCK_META_KEY.
const LogicalClockMetaKey = "logical_now_ms"

// HasNonEmptyAssistantMessage mirrors hasNonEmptyAssistantMessage: the compact
// history counts as carrying a turn only when its last assistant message has
// non-blank text. The implementation lives in internal/observability, which owns
// the CompactAgentHistory contract.
func HasNonEmptyAssistantMessage(compactHistory *observability.CompactAgentHistory) bool {
	return observability.HasNonEmptyAssistantMessage(compactHistory)
}

// parseCompactHistoryJSON and agentKindFromPayloadJSON forward to the
// observability helpers of the same name, which internal/db cannot call
// directly because they are unexported there.
func parseCompactHistoryJSON(value *string) *observability.CompactAgentHistory {
	if value == nil || *value == "" {
		return nil
	}
	var compact observability.CompactAgentHistory
	if err := json.Unmarshal([]byte(*value), &compact); err != nil {
		return nil
	}
	return &compact
}

func agentKindFromPayloadJSON(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	var payload any
	if err := json.Unmarshal([]byte(*value), &payload); err != nil {
		return nil
	}
	kind, ok := payload.(map[string]any)["agent"].(string)
	if !ok {
		return nil
	}
	return &kind
}

// IsDeliverableAgentEvent mirrors isDeliverableAgentEvent: the single delivery
// predicate shared by pending discovery and acknowledgement.
//
// The scope is the orchestrator scope the owner terminal holds, and the agent is
// the live index record for the event's agent — nil once the pane was retired
// and its `agents` row physically removed.
func IsDeliverableAgentEvent(
	event observability.AgentEventRecord,
	agent *observability.AgentIndexRecord,
	scope observability.AgentScope,
	ownerTerminalID string,
) bool {
	failed := event.Type == observability.AgentEventFailed ||
		event.Type == observability.AgentEventDiscarded
	isRetainedInvalidated := event.Status != nil &&
		*event.Status == observability.AgentEventStatusInvalidated && event.Deliverable == 1
	isOrphanOutcome := event.AgentID == nil &&
		event.Deliverable == 1 &&
		(event.Type == observability.AgentEventDone || event.Type == observability.AgentEventIdle)

	// 1. Status admission: pending, delivered, and the invalidated rows retained
	// for a closed pane.
	statusAllowed := (event.Status != nil &&
		(*event.Status == observability.AgentEventStatusPending ||
			*event.Status == observability.AgentEventStatusDelivered)) || isRetainedInvalidated
	if !statusAllowed {
		return false
	}

	// 2. Base scope filtering.
	if (event.WorkspaceID == nil || *event.WorkspaceID != scope.WorkspaceID) ||
		event.HerdrSessionName != scope.HerdrSessionName ||
		event.TerminalID == nil ||
		*event.TerminalID == ownerTerminalID ||
		event.Type == observability.AgentEventStatusChanged ||
		(event.Type == observability.AgentEventIdle && payloadFrom(event.Payload) != "working") {
		return false
	}

	// 3. Delivery retry window guard.
	if (event.Status == nil || *event.Status != observability.AgentEventStatusDelivered) &&
		nextAttemptAtAfterNow(event) {
		return false
	}

	// 4. Terminal ownership check.
	if event.Status != nil && *event.Status == observability.AgentEventStatusDelivered &&
		(event.DeliveredToTerminalID == nil ||
			*event.DeliveredToTerminalID != ownerTerminalID) {
		return false
	}

	// 5. Core: decoupling from the live `agents` row. When the pane is closed,
	// retirePane physically clears the agents record, so agent is nil here. A
	// retained-invalidated row, an orphan outcome or a failed row is validated
	// from the event's own metadata instead.
	if failed || isRetainedInvalidated || isOrphanOutcome {
		payload := asRecord(event.Payload)
		if interactive, _ := payload["interactive"].(bool); interactive &&
			event.Type == observability.AgentEventIdle {
			return false
		}
		agentKind := "pi"
		if kind, ok := payload["agent"].(string); ok {
			agentKind = kind
		}
		if agentKind != "pi" && agentKind != "pig" &&
			(event.Type == observability.AgentEventIdle || event.Type == observability.AgentEventDone) &&
			!HasNonEmptyAssistantMessage(event.CompactHistory) {
			return false
		}
		return true
	}

	// 6. A live pane keeps the strict generation and ownership checks.
	if agent == nil {
		return false
	}
	agentKind := "pi"
	if agent.Agent != nil {
		agentKind = *agent.Agent
	}
	return !(observability.IsInteractivePiAgent(agent) && event.Type == observability.AgentEventIdle) &&
		!(agentKind != "pi" && agentKind != "pig" &&
			(event.Type == observability.AgentEventIdle || event.Type == observability.AgentEventDone) &&
			!HasNonEmptyAssistantMessage(event.CompactHistory)) &&
		agent.PaneID == *event.PaneID &&
		samePaneGeneration(agent.PaneGeneration, event.PaneGeneration) &&
		agent.WorkspaceID == scope.WorkspaceID
}

// asRecord mirrors asRecord: a non-object payload degrades to an empty record
// instead of throwing on the field reads.
func asRecord(value any) map[string]any {
	if record, ok := value.(map[string]any); ok {
		return record
	}
	return map[string]any{}
}

// payloadFrom mirrors `asRecord(payload).from`, collapsing an absent or
// non-string field to "".
func payloadFrom(payload any) string {
	if from, ok := asRecord(payload)["from"].(string); ok {
		return from
	}
	return ""
}

// nextAttemptAtAfterNow mirrors `event.nextAttemptAt > new Date()`. A timestamp
// that cannot be parsed cannot be ordered, so it does not block; the stored
// value always comes from formatTimestamp, which makes this defensive only.
func nextAttemptAtAfterNow(event observability.AgentEventRecord) bool {
	if event.NextAttemptAt == nil {
		return false
	}
	at, err := time.Parse(isoTimestampLayout, *event.NextAttemptAt)
	if err != nil {
		return false
	}
	return at.After(time.Now())
}

// samePaneGeneration mirrors `agent.paneGeneration === event.paneGeneration` on
// two nullable fields: an absent event generation matches any agent generation.
func samePaneGeneration(agentGeneration *string, eventGeneration *string) bool {
	if eventGeneration == nil {
		return true
	}
	return agentGeneration != nil && *agentGeneration == *eventGeneration
}

// AppendAgentEventInput, InvalidatePaneInput and AckSelfOwnedInput live in
// internal/observability, because internal/observability cannot import
// internal/db (that would be an import cycle) yet its AgentIndexService needs
// to speak them. The aliases keep the existing call sites and test names
// unchanged.
type AppendAgentEventInput = observability.AppendAgentEventInput

type InvalidatePaneInput = observability.InvalidatePaneInput

type AckSelfOwnedInput = observability.AckSelfOwnedInput

type PaneEventTailInput = observability.PaneEventTailInput

type PaneEventTailRow = observability.PaneEventTailRow

type PaneDeliveredTerminalBodiesInput = observability.PaneDeliveredTerminalBodiesInput

type PaneDeliveredTerminalBodyRow = observability.PaneDeliveredTerminalBodyRow

type DeliveredTerminalHistoriesInput = observability.DeliveredTerminalHistoriesInput

type DeliveredTerminalHistoryRow = observability.DeliveredTerminalHistoryRow

type LatestPaneEventScopeInput = observability.LatestPaneEventScopeInput

type LatestPaneEventScopeRow = observability.LatestPaneEventScopeRow

// ListAfterInput, NextDeliverableAfterInput and the orchestrator query inputs
// live in internal/observability, because internal/observability cannot import
// internal/db (that would be an import cycle) yet its AgentOrchestratorService
// needs to speak them. The aliases keep the existing call sites and test names
// unchanged.
type ListAfterInput = observability.ListAfterInput

type NextDeliverableAfterInput = observability.NextDeliverableAfterInput

// IsDeliverableAgentEvent mirrors the shared delivery predicate as a method, so
// the observability service can reach it through its store interface. The
// package-level function of the same name stays the implementation.
func (s *AgentEventStore) IsDeliverableAgentEvent(
	event observability.AgentEventRecord,
	agent *observability.AgentIndexRecord,
	scope observability.AgentScope,
	ownerTerminalID string,
) bool {
	return IsDeliverableAgentEvent(event, agent, scope, ownerTerminalID)
}

// AgentEventStore ports src/db/agent-events.ts.
type AgentEventStore struct {
	db *sql.DB

	// clockMu guards the monotonic logical clock and its daemon_meta persist.
	// The TS side is single-threaded on one DatabaseSync connection; the Go port
	// shares one *sql.DB, so the clock needs its own lock to stay race-free
	// without ever being held across a statement.
	clockMu                 sync.Mutex
	lastPersistedLogicalNow int64
	logicalNow              int64
}

// NewAgentEventStore builds a store on the shared connection and seeds the
// monotonic logical clock from daemon_meta, exactly like the TS constructor.
func NewAgentEventStore(db *sql.DB) (*AgentEventStore, error) {
	store := &AgentEventStore{db: db}
	var value string
	err := db.QueryRow(
		"select value from daemon_meta where key = ?", LogicalClockMetaKey,
	).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return store, nil
	case err != nil:
		return nil, err
	}
	persisted, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr == nil && persisted > 0 {
		store.logicalNow = persisted
		store.lastPersistedLogicalNow = store.logicalNow
	}
	return store, nil
}

// now mirrors the private #now(): a monotonic logical clock that never moves
// backwards, seeded from daemon_meta and advanced to the wall clock when the
// wall clock is ahead. Persistence is throttled so read-only queries do not
// write+fsync on every call; on a crash the persisted value is only ever stale.
func (s *AgentEventStore) now() (int64, error) {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	wallClock := time.Now().UnixMilli()
	if wallClock <= s.logicalNow {
		return s.logicalNow, nil
	}
	s.logicalNow = wallClock
	if s.logicalNow-s.lastPersistedLogicalNow < LogicalClockPersistIntervalMs {
		return s.logicalNow, nil
	}
	s.lastPersistedLogicalNow = s.logicalNow
	if _, err := s.db.Exec(
		`insert into daemon_meta (key, value) values (?, ?)
		 on conflict(key) do update set value = excluded.value`,
		LogicalClockMetaKey, strconv.FormatInt(s.logicalNow, 10),
	); err != nil {
		return 0, err
	}
	return s.logicalNow, nil
}

// agentEventColumns is the explicit `select *` column list of agent_events.
const agentEventColumns = "agent_id, compact_history_json, created_at, herdr_session_name, id," +
	" idempotency_key, pane_id, pane_generation, payload_json, delivery_attempts," +
	" last_attempt_at, next_attempt_at, last_failure_code, invalidated_reason," +
	" delivered_to_terminal_id, status, deliverable, terminal_id, type, workspace_id"

// Append mirrors append: an event is inserted as pending, unless the same
// (session, idempotency key) already has a row — then that row is returned as it
// stands.
func (s *AgentEventStore) Append(input AppendAgentEventInput) (observability.AgentEventRecord, error) {
	if input.IdempotencyKey != nil && *input.IdempotencyKey != "" {
		existing, ok, err := s.getByIdempotencyKey(input.HerdrSessionName, *input.IdempotencyKey)
		if err != nil {
			return observability.AgentEventRecord{}, err
		}
		if ok {
			return existing, nil
		}
	}

	agentID, err := s.resolveAppendAgentID(input.AgentID)
	if err != nil {
		return observability.AgentEventRecord{}, err
	}
	now, err := s.now()
	if err != nil {
		return observability.AgentEventRecord{}, err
	}
	var compactHistoryJSON any
	if input.CompactHistory != nil {
		encoded, err := json.Marshal(input.CompactHistory)
		if err != nil {
			return observability.AgentEventRecord{}, err
		}
		compactHistoryJSON = string(encoded)
	}
	payloadJSON, err := json.Marshal(input.Payload)
	if err != nil {
		return observability.AgentEventRecord{}, err
	}
	result, err := s.db.Exec(
		`insert into agent_events
		 (herdr_session_name, agent_id, pane_id, pane_generation, workspace_id, terminal_id, type, payload_json, compact_history_json, idempotency_key, deliverable, status, delivery_attempts, created_at)
		 values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'pending', 0, ?)`,
		input.HerdrSessionName, agentID, input.PaneID, input.PaneGeneration,
		input.WorkspaceID, input.TerminalID, input.Type, string(payloadJSON),
		compactHistoryJSON, input.IdempotencyKey, now,
	)
	if err != nil {
		// The unique (session, idempotency key) index is the authority on a
		// concurrent duplicate: the existing row wins over the error.
		if input.IdempotencyKey != nil && *input.IdempotencyKey != "" {
			existing, ok, lookupErr := s.getByIdempotencyKey(input.HerdrSessionName, *input.IdempotencyKey)
			if lookupErr != nil {
				return observability.AgentEventRecord{}, lookupErr
			}
			if ok {
				return existing, nil
			}
		}
		return observability.AgentEventRecord{}, err
	}
	insertedID, err := result.LastInsertId()
	if err != nil {
		return observability.AgentEventRecord{}, err
	}
	return s.Get(insertedID)
}

// resolveAppendAgentID mirrors #resolveAppendAgentId: defence in depth for the
// terminal-event write path. A status plan can reach its terminal state after
// the pane was retired and its `agents` row physically deleted; the foreign key
// would then reject the insert. Instead of failing, the row is written as an
// orphan (agent_id = null) and the degradation is reported.
func (s *AgentEventStore) resolveAppendAgentID(agentID *string) (*string, error) {
	if agentID == nil {
		return nil, nil
	}
	var marker int
	err := s.db.QueryRow("select 1 from agents where id = ? limit 1", *agentID).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		fmt.Fprintf(os.Stderr,
			"Herdsman downgrading agent event to orphan because the agents row is gone: agentId=%s\n",
			*agentID)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return agentID, nil
}

// InvalidatePane mirrors invalidatePane: the two-step invalidation runs inside
// one transaction.
func (s *AgentEventStore) InvalidatePane(input InvalidatePaneInput) error {
	return RunSqliteTransaction(s.db, func() error {
		return s.invalidatePaneCore(input)
	})
}

// InvalidatePaneDirect mirrors invalidatePaneDirect: the same invalidation
// without the wrapping transaction.
//
// ONLY call this when the caller already holds a transaction (for example from
// AgentStore.ReplaceForSession); outside an explicit transaction the two-step
// invalidation is not atomic with respect to other concurrent writers.
func (s *AgentEventStore) InvalidatePaneDirect(input InvalidatePaneInput) error {
	return s.invalidatePaneCore(input)
}

func (s *AgentEventStore) invalidatePaneCore(input InvalidatePaneInput) error {
	legacy := input.PaneGeneration == nil
	now, err := s.now()
	if err != nil {
		return err
	}
	defaultReason := "PANE_CLOSED"
	if input.InvalidatedReason != nil {
		defaultReason = *input.InvalidatedReason
	} else if legacy {
		defaultReason = "LEGACY_CLOSE_WITHOUT_GENERATION"
	}
	// Step 1: invalidate every active row of the pane, defaulting deliverable
	// to 0. With an explicit paneGeneration only that generation and the older
	// legacy rows (<=) are invalidated, so a newer generation is not killed.
	legacyOrMatched := "1 = 1"
	legacyOrMatchedParams := []any{}
	if !legacy {
		legacyOrMatched = "(pane_generation = ? or pane_generation is null or pane_generation < ?)"
		legacyOrMatchedParams = []any{input.PaneGeneration, input.PaneGeneration}
	}
	if input.AcknowledgeDelivered != nil && *input.AcknowledgeDelivered {
		// Step 0 (explicit pane close only): rows that were already delivered
		// are acked as implicitly consumed. An explicit tab close is the user
		// wrapping up, and the orchestrator already has that update (even when
		// it read it out of band from the pane, so the extension never acks);
		// keeping the close retention for such a row would leave a
		// delivered-forever row that the daemon redelivers inside the 300s
		// window forever, keeping wakeAwaitingConsumption non-empty and driving
		// empty continuations. Missing one update is better than an unbounded
		// redelivery from a closed pane.
		//
		// Only rows with delivery_attempts >= 1 (delivered at least once) are
		// acked: a row that was never delivered still deserves one attempt and
		// is handled by the step 2 retention rule. The crash/disconnect paths
		// (reconciler, reclaimDelivered) never come through here, so their
		// retention semantics are unchanged.
		if _, err := s.db.Exec(
			`update agent_events
			    set status = 'acked', deliverable = 0, invalidated_reason = null
			  where herdr_session_name = ? and pane_id = ? and status in ('pending', 'delivered')
			    and delivery_attempts >= 1 and `+legacyOrMatched,
			append([]any{input.HerdrSessionName, input.PaneID}, legacyOrMatchedParams...)...,
		); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(
		`update agent_events
		     set deliverable = 0, status = 'invalidated', invalidated_reason = ?
		   where herdr_session_name = ? and pane_id = ? and status not in ('acked', 'invalidated', 'failed')
		     and `+legacyOrMatched,
		append([]any{defaultReason, input.HerdrSessionName, input.PaneID}, legacyOrMatchedParams...)...,
	); err != nil {
		return err
	}
	// Step 2: restore deliverable = 1 for the retained completion events.
	// Close retention only lasts until the first delivery attempt: done/idle
	// keep the 300s freshness window, and agent.failed is bounded by
	// delivery_attempts = 0 (never delivered) — a failure is often written
	// after the pane/agents row is already gone, so it needs one attempt.
	// agent.discarded is never retained. An explicit close (acknowledgeDelivered)
	// already acked the delivered rows in step 0, so this rule only ever hits
	// rows that were never delivered; the crash/disconnect paths do not look at
	// that flag and keep the same semantics.
	genCondition := "1 = 1"
	genParams := []any{}
	if !legacy {
		genCondition = "pane_generation = ?"
		genParams = []any{input.PaneGeneration}
	}
	_, err = s.db.Exec(
		`update agent_events
		      set deliverable = 1, invalidated_reason = 'RETAINED_OUTCOME_PANE_CLOSED'
		    where herdr_session_name = ? and pane_id = ? and status = 'invalidated' and deliverable = 0 and invalidated_reason = ?
		      and ((`+genCondition+`)
		         and ((type = 'agent.done' and json_extract(payload_json, '$.from') = 'working')
		           or (type = 'agent.idle' and json_extract(payload_json, '$.from') = 'working'))
		         and created_at >= ?
		      or (type = 'agent.failed' and delivery_attempts = 0))`,
		append([]any{
			input.HerdrSessionName, input.PaneID, defaultReason,
		}, append(genParams, now-RedeliveryFreshnessMs)...)...,
	)
	return err
}

// ListReconcileCandidates mirrors listReconcileCandidates. A limit of 0 means
// the TS default of 100.
func (s *AgentEventStore) ListReconcileCandidates(
	limit int,
	afterID int64,
) ([]observability.AgentEventRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	return scanAgentEvents(s.db.Query(
		"select "+agentEventColumns+
			" from agent_events where id > ? and status in ('pending', 'delivered')"+
			" order by id asc limit ?",
		afterID, limit,
	))
}

// MarkRetainedInvalidated mirrors markRetainedInvalidated.
func (s *AgentEventStore) MarkRetainedInvalidated(id int64) (bool, error) {
	result, err := s.db.Exec(
		`update agent_events
		    set status = 'invalidated', deliverable = 1, invalidated_reason = 'RETAINED_OUTCOME_PANE_CLOSED'
		  where id = ? and status in ('pending', 'delivered')`,
		id,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// DeleteReconcileCandidate mirrors deleteReconcileCandidate.
func (s *AgentEventStore) DeleteReconcileCandidate(id int64) (bool, error) {
	result, err := s.db.Exec(
		"delete from agent_events where id = ? and status in ('pending', 'delivered')",
		id,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// DeleteInvalidatedOlderThan mirrors deleteInvalidatedOlderThan: invalidated
// rows older than the given age are removed, measured from
// coalesce(last_attempt_at, created_at) so a delivered-then-invalidated event is
// aged from its last delivery. The short grace window lets the owner that
// already received the event ack it and advance the cursor.
func (s *AgentEventStore) DeleteInvalidatedOlderThan(ageMs int64) (int, error) {
	result, err := s.db.Exec(
		`delete from agent_events
		 where status = 'invalidated' and deliverable = 0 and coalesce(last_attempt_at, created_at) < ?`,
		time.Now().UnixMilli()-ageMs,
	)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// DeleteSettledOlderThan mirrors deleteSettledOlderThan: terminal events
// (acked/failed, plus the retained invalidated rows) older than the given age
// are removed. These statuses are never redelivered or reconciled, so the TTL
// bounds the table's growth without touching pending/delivered/invalidated rows.
func (s *AgentEventStore) DeleteSettledOlderThan(ageMs int64) (int, error) {
	result, err := s.db.Exec(
		`delete from agent_events where (status in ('acked', 'failed') or (status = 'invalidated' and deliverable = 1)) and created_at < ?`,
		time.Now().UnixMilli()-ageMs,
	)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// AckSelfOwned mirrors ackSelfOwned.
func (s *AgentEventStore) AckSelfOwned(input AckSelfOwnedInput) (int, error) {
	result, err := s.db.Exec(
		`update agent_events set status = 'acked', deliverable = 0
		 where herdr_session_name = ? and workspace_id = ? and pane_id = ?
		   and status in ('pending', 'delivered')`,
		input.HerdrSessionName, input.WorkspaceID, input.PaneID,
	)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// InvalidateByID mirrors invalidateById.
func (s *AgentEventStore) InvalidateByID(id int64, reason string) (bool, error) {
	result, err := s.db.Exec(
		`update agent_events set deliverable = 0, status = 'invalidated', invalidated_reason = ? where id = ? and status in ('pending', 'delivered')`,
		reason, id,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// LatestStatusTransition mirrors latestStatusTransition.
func (s *AgentEventStore) LatestStatusTransition(
	agentID string,
	herdrSessionName string,
) (*observability.AgentEventRecord, error) {
	row, ok, err := scanAgentEvent(s.db.QueryRow(
		"select "+agentEventColumns+
			" from agent_events where agent_id = ? and herdr_session_name = ?"+
			" and type = 'agent.status.changed' order by id desc limit 1",
		agentID, herdrSessionName,
	))
	if err != nil || !ok {
		return nil, err
	}
	return &row, nil
}

// HasTerminalEventAfter mirrors hasTerminalEventAfter.
func (s *AgentEventStore) HasTerminalEventAfter(
	agentID string,
	herdrSessionName string,
	afterID int64,
) (bool, error) {
	var marker int
	err := s.db.QueryRow(
		`select 1 from agent_events
		 where agent_id = ? and herdr_session_name = ? and id > ?
		   and type in ('agent.idle', 'agent.done', 'agent.blocked')
		   and status != 'invalidated'
		 limit 1`,
		agentID, herdrSessionName, afterID,
	).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// LatestTerminalEvent mirrors latestTerminalEvent.
func (s *AgentEventStore) LatestTerminalEvent(
	agentID string,
	herdrSessionName string,
) (*observability.AgentEventRecord, error) {
	row, ok, err := scanAgentEvent(s.db.QueryRow(
		"select "+agentEventColumns+
			" from agent_events where agent_id = ? and herdr_session_name = ?"+
			" and type in ('agent.idle', 'agent.done', 'agent.blocked')"+
			" and status != 'invalidated' order by id desc limit 1",
		agentID, herdrSessionName,
	))
	if err != nil || !ok {
		return nil, err
	}
	return &row, nil
}

// LatestCompletedTurnEvent mirrors latestCompletedTurnEvent: the latest event
// that represents a completed turn whose assistant ref may be treated as
// already delivered.
//
// Every agent.done qualifies, plus agent.idle rows emitted from `working`.
// Rows that must never consume the assistant ref of a round are excluded:
// agent.status.changed (never a delivery), agent.discarded, agent.idle rows
// whose payload `from` is not `working` (startup/unknown idles restored before
// indexing), and rows whose assistant message is empty. A late
// `unknown -> idle` plan finishing after the round already ended would
// otherwise register the final ref as an agent.idle terminal event and make the
// following `working -> done` plan look like a duplicate.
func (s *AgentEventStore) LatestCompletedTurnEvent(
	agentID string,
	herdrSessionName string,
) (*observability.AgentEventRecord, error) {
	row, ok, err := scanAgentEvent(s.db.QueryRow(
		"select "+agentEventColumns+
			" from agent_events where agent_id = ? and herdr_session_name = ?"+
			" and (type = 'agent.done'"+
			"      or (type = 'agent.idle' and json_extract(payload_json, '$.from') = 'working'))"+
			" and length(trim(coalesce(json_extract(compact_history_json, '$.lastAssistantMessage.text'), ''))) > 0"+
			" order by id desc limit 1",
		agentID, herdrSessionName,
	))
	if err != nil || !ok {
		return nil, err
	}
	return &row, nil
}

// ListAfter mirrors listAfter: the cursor scan behind the orchestrator get.
func (s *AgentEventStore) ListAfter(input ListAfterInput) ([]observability.AgentEventRecord, error) {
	afterEventID := int64(0)
	if input.AfterEventID != nil {
		afterEventID = *input.AfterEventID
	}
	limit := 100
	if input.Limit != nil {
		limit = *input.Limit
	}
	now, err := s.now()
	if err != nil {
		return nil, err
	}
	clauses := []string{
		"id > ?",
		"((status = 'pending' and (next_attempt_at is null or next_attempt_at <= ?))" +
			" or (status = 'delivered' and delivered_to_terminal_id = ? and id > ? and last_attempt_at >= ?)" +
			" or (status = 'invalidated' and deliverable = 1 and (next_attempt_at is null or next_attempt_at <= ?)))",
	}
	params := []any{
		afterEventID,
		now,
		input.OwnerTerminalID,
		afterEventID,
		now - RedeliveryFreshnessMs,
		now,
	}
	if input.HerdrSessionName != nil && *input.HerdrSessionName != "" {
		clauses = append(clauses, "herdr_session_name = ?")
		params = append(params, *input.HerdrSessionName)
	}
	if input.WorkspaceID != nil && *input.WorkspaceID != "" {
		clauses = append(clauses, "workspace_id = ?")
		params = append(params, *input.WorkspaceID)
	}
	params = append(params, limit)
	return scanAgentEvents(s.db.Query(
		"select "+agentEventColumns+" from agent_events where "+
			strings.Join(clauses, " and ")+" order by id asc limit ?",
		params...,
	))
}

// orphanTerminalFilter is the SQL fragment that lets orphan terminal rows
// through the agent-existence guard: they carry agent_id = null and still have
// to be visible, because otherwise the cursor ack (markAcked(id <= cursor))
// would silently swallow an event that was never delivered.
const orphanTerminalFilter = "or type in ('agent.failed', 'agent.discarded')" +
	" or (agent_id is null and deliverable = 1 and type in ('agent.done', 'agent.idle'))"

// NextDeliverableAfter mirrors nextDeliverableAfter: the first event the owner
// terminal may deliver, scanning in pages and applying the shared delivery
// predicate to each row.
func (s *AgentEventStore) NextDeliverableAfter(
	input NextDeliverableAfterInput,
) (*observability.AgentEventRecord, error) {
	scope := observability.AgentScope{
		HerdrSessionName: input.HerdrSessionName,
		WorkspaceID:      input.WorkspaceID,
	}
	// Retained rows (status = 'invalidated' and deliverable = 1) outlive the
	// live `agents` row because pane close physically clears it, so they must
	// never be filtered by the existence guard. Orphan terminal rows
	// (agent.failed / agent.discarded written after the agents row vanished)
	// have the same problem. Every other row keeps the original "the agent is
	// still indexed" check.
	agentFilter := ""
	if input.GetAgent == nil {
		agentFilter = "and (\n" +
			"       (status = 'invalidated' and deliverable = 1)\n" +
			"       " + orphanTerminalFilter + "\n" +
			"       or exists (\n" +
			"         select 1 from agents\n" +
			"         where agents.id = agent_events.agent_id\n" +
			"           and agents.herdr_session_name = agent_events.herdr_session_name\n" +
			"           and agents.workspace_id = agent_events.workspace_id\n" +
			"           and agents.pane_id = agent_events.pane_id\n" +
			"       )\n" +
			"     )"
	}
	afterEventID := input.AfterEventID
	for page := 0; page < 50; page += 1 {
		now, err := s.now()
		if err != nil {
			return nil, err
		}
		rows, err := scanAgentEvents(s.db.Query(
			"select "+agentEventColumns+
				" from agent_events"+
				" where id > ? and ((status = 'pending' and (next_attempt_at is null or next_attempt_at <= ?))"+
				" or (status = 'delivered' and delivered_to_terminal_id = ? and last_attempt_at >= ?)"+
				" or (status = 'invalidated' and deliverable = 1 and (next_attempt_at is null or next_attempt_at <= ?)))"+
				" and herdr_session_name = ? and workspace_id = ?"+
				" and terminal_id is not null and terminal_id != ?"+
				" and (agent_id is not null or (status = 'invalidated' and deliverable = 1) "+orphanTerminalFilter+")"+
				" "+agentFilter+
				" order by id asc limit 1000",
			afterEventID, now, input.OwnerTerminalID, now-RedeliveryFreshnessMs, now,
			input.HerdrSessionName, input.WorkspaceID, input.OwnerTerminalID,
		))
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, nil
		}
		for _, event := range rows {
			// Pane close physically removes the agents row and the ON DELETE SET
			// NULL foreign key then clears agent_id on the retained event, so
			// retained rows are evaluated from their own metadata instead of the
			// live agent record. Orphan terminal rows follow the same rule.
			retained := event.Status != nil &&
				*event.Status == observability.AgentEventStatusInvalidated && event.Deliverable == 1
			orphanTerminal := event.Type == observability.AgentEventFailed ||
				event.Type == observability.AgentEventDiscarded
			orphanOutcome := event.AgentID == nil &&
				event.Deliverable == 1 &&
				(event.Type == observability.AgentEventDone || event.Type == observability.AgentEventIdle)
			if event.AgentID == nil && !retained && !orphanTerminal && !orphanOutcome {
				continue
			}
			var liveAgent *observability.AgentIndexRecord
			if event.AgentID != nil && input.GetAgent != nil {
				liveAgent = input.GetAgent(*event.AgentID)
			}
			shouldValidate := input.GetAgent != nil || event.AgentID == nil
			if !shouldValidate ||
				IsDeliverableAgentEvent(event, liveAgent, scope, input.OwnerTerminalID) {
				found := event
				return &found, nil
			}
		}
		afterEventID = rows[len(rows)-1].ID
	}
	fmt.Fprintf(os.Stderr,
		"Herdsman stopped scanning pending agent events after 50 pages: herdrSessionName=%s workspaceId=%s\n",
		input.HerdrSessionName, input.WorkspaceID)
	return nil, nil
}

// ReservePending mirrors reservePending: the matching rows are marked delivered
// for the owner terminal inside one transaction, and the reserved records are
// returned. A limit of 0 means the TS default of 100.
func (s *AgentEventStore) ReservePending(
	terminalID string,
	limit int,
	ids []int64,
) ([]observability.AgentEventRecord, error) {
	var reserved []observability.AgentEventRecord
	err := RunSqliteTransaction(s.db, func() error {
		if limit <= 0 {
			limit = 100
		}
		now, err := s.now()
		if err != nil {
			return err
		}
		idClause := ""
		params := []any{now, terminalID, now - RedeliveryFreshnessMs, now}
		if len(ids) > 0 {
			idClause = "and id in (" + placeholders(len(ids)) + ")"
			for _, id := range ids {
				params = append(params, id)
			}
		}
		params = append(params, limit)
		rows, err := scanAgentEvents(s.db.Query(
			"select "+agentEventColumns+" from agent_events where"+
				" ((status = 'pending' and (next_attempt_at is null or next_attempt_at <= ?))"+
				" or (status = 'delivered' and delivered_to_terminal_id = ? and last_attempt_at >= ?)"+
				" or (status = 'invalidated' and deliverable = 1 and (next_attempt_at is null or next_attempt_at <= ?)))"+
				" "+idClause+" order by id asc limit ?",
			params...,
		))
		if err != nil {
			return err
		}
		for _, row := range rows {
			attempts := 0
			if row.DeliveryAttempts != nil {
				attempts = *row.DeliveryAttempts
			}
			if _, err := s.db.Exec(
				`update agent_events
				   set status = 'delivered', deliverable = 1, delivery_attempts = ?, last_attempt_at = ?, delivered_to_terminal_id = ?
				 where id = ? and (status = 'pending' or (status = 'invalidated' and deliverable = 1))`,
				attempts+1, now, terminalID, row.ID,
			); err != nil {
				return err
			}
		}
		if len(rows) > 0 {
			fmt.Fprintf(os.Stdout,
				"Herdsman agent event delivery reserved: firstEventId=%d lastEventId=%d count=%d terminal=%s\n",
				rows[0].ID, rows[len(rows)-1].ID, len(rows), terminalID)
		}
		reserved = make([]observability.AgentEventRecord, 0, len(rows))
		for _, row := range rows {
			record, err := s.Get(row.ID)
			if err != nil {
				return err
			}
			reserved = append(reserved, record)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reserved, nil
}

// ReclaimDelivered mirrors reclaimDelivered: a delivery whose owner terminal no
// longer holds an orchestrator scope is reclaimed, and a delivery whose agent
// pane is closed is invalidated instead of being redelivered. Rows that already
// exceeded the attempt budget fail instead of being retried.
func (s *AgentEventStore) ReclaimDelivered(timeoutMs int64) (int, error) {
	var reclaimed int
	err := RunSqliteTransaction(s.db, func() error {
		cutoff, err := s.now()
		if err != nil {
			return err
		}
		cutoff -= timeoutMs
		// The delivered event's own agent row (matched by pane) is the pane-open
		// guard: it decides whether branch (a) and branch (b) apply.
		agentPaneOpen := "exists (\n" +
			"        select 1 from agents\n" +
			"        where agents.id = agent_events.agent_id\n" +
			"          and agents.herdr_session_name = agent_events.herdr_session_name\n" +
			"          and agents.workspace_id = agent_events.workspace_id\n" +
			"          and agents.pane_id = agent_events.pane_id\n" +
			"      )"
		// Branch (b): the agent pane is closed/retired, so the delivered event is
		// invalidated instead of being reclaimed and redelivered. This is also
		// where the "retained until the first delivery attempt" window of a
		// terminal agent.failed row ends and the ordinary invalidated + 1h
		// collection takes over. reservePending is the only writer that sets
		// status = 'delivered' and it always increments delivery_attempts, so the
		// explicit `delivery_attempts >= 1` guard states the invariant that no row
		// may leave the retained window before it was handed to an owner once.
		invalidatedResult, err := s.db.Exec(
			`update agent_events
			   set deliverable = 0, status = 'invalidated', invalidated_reason = 'PANE_CLOSED_RECLAIM'
			 where status = 'delivered' and delivery_attempts >= 1 and last_attempt_at < ? and not `+agentPaneOpen,
			cutoff,
		)
		if err != nil {
			return err
		}
		// Branch (c): the pane is still open but the delivery terminal no longer
		// holds any orchestrator scope in this Herdr session
		// (owner_terminal_id != delivered_to_terminal_id or no owner), so reclaim
		// immediately without waiting for the last_attempt_at timeout. If the
		// delivery terminal still owns a scope (branch a), the event remains
		// in-flight with the owner. The scope is matched session-wide because an
		// owner also receives events of ownerless sibling workspaces
		// (cross-workspace dispatched workers).
		reclaimWhere := "status = 'delivered'\n" +
			"             and " + agentPaneOpen + "\n" +
			"             and not exists (\n" +
			"               select 1 from agent_orchestrator_scopes\n" +
			"               where agent_orchestrator_scopes.herdr_session_name = agent_events.herdr_session_name\n" +
			"                 and agent_orchestrator_scopes.owner_terminal_id = agent_events.delivered_to_terminal_id\n" +
			"             )"
		exceededRows, err := s.db.Query(
			"select id, agent_id, herdr_session_name, workspace_id, delivery_attempts" +
				" from agent_events where " + reclaimWhere + " and delivery_attempts >= 10",
		)
		if err != nil {
			return err
		}
		type exceededRow struct {
			agentID          sql.NullString
			deliveryAttempts int
			herdrSessionName string
			id               int64
			workspaceID      sql.NullString
		}
		exceeded := []exceededRow{}
		for exceededRows.Next() {
			var row exceededRow
			if err := exceededRows.Scan(
				&row.id, &row.agentID, &row.herdrSessionName, &row.workspaceID,
				&row.deliveryAttempts,
			); err != nil {
				_ = exceededRows.Close()
				return err
			}
			exceeded = append(exceeded, row)
		}
		if err := exceededRows.Err(); err != nil {
			_ = exceededRows.Close()
			return err
		}
		if err := exceededRows.Close(); err != nil {
			return err
		}
		reclaimedResult, err := s.db.Exec(
			`update agent_events
			   set status = case when delivery_attempts >= 10 then 'failed' else 'pending' end,
			       deliverable = case when delivery_attempts >= 10 then 0 else 1 end,
			       last_failure_code = case when delivery_attempts >= 10 then 'DELIVERY_ATTEMPTS_EXCEEDED' else last_failure_code end,
			       delivered_to_terminal_id = null,
			       next_attempt_at = null
			 where ` + reclaimWhere,
		)
		if err != nil {
			return err
		}
		invalidated, err := invalidatedResult.RowsAffected()
		if err != nil {
			return err
		}
		reclaimedCount, err := reclaimedResult.RowsAffected()
		if err != nil {
			return err
		}
		for _, row := range exceeded {
			fmt.Fprintf(os.Stderr,
				"Herdsman agent event delivery attempts exceeded: eventId=%d agentId=%s"+
					" herdrSessionName=%s workspaceId=%s deliveryAttempts=%d lastFailureCode=%s\n",
				row.id, logNullable(row.agentID), row.herdrSessionName,
				logNullable(row.workspaceID), row.deliveryAttempts, "DELIVERY_ATTEMPTS_EXCEEDED")
		}
		reclaimed = int(invalidated) + int(reclaimedCount)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return reclaimed, nil
}

// logNullable renders a nullable field the way the TS structured logs show it.
func logNullable(value sql.NullString) string {
	if !value.Valid {
		return "null"
	}
	return value.String
}

// MarkAcked mirrors markAcked: with a scope the whole cursor up to id is acked
// for that session and workspace, without it only the single row is.
func (s *AgentEventStore) MarkAcked(id int64, scope *observability.AgentScope) error {
	if scope != nil {
		_, err := s.db.Exec(
			`update agent_events set status = 'acked', deliverable = 0
			 where herdr_session_name = ? and workspace_id = ? and id <= ?
			   and (status in ('pending', 'delivered') or (status = 'invalidated' and deliverable = 1))`,
			scope.HerdrSessionName, scope.WorkspaceID, id,
		)
		return err
	}
	_, err := s.db.Exec(
		`update agent_events set status = 'acked', deliverable = 0
		 where id = ? and (status in ('pending', 'delivered') or (status = 'invalidated' and deliverable = 1))`,
		id,
	)
	return err
}

// LatestEventID mirrors latestEventId: the newest event id inside the scope, or
// 0 when the scope has none.
func (s *AgentEventStore) LatestEventID(scope observability.AgentQueryScope) (int64, error) {
	clauses := []string{}
	params := []any{}
	if scope.HerdrSessionName != nil && *scope.HerdrSessionName != "" {
		clauses = append(clauses, "herdr_session_name = ?")
		params = append(params, *scope.HerdrSessionName)
	}
	if scope.WorkspaceID != nil && *scope.WorkspaceID != "" {
		clauses = append(clauses, "workspace_id = ?")
		params = append(params, *scope.WorkspaceID)
	}
	where := ""
	if len(clauses) > 0 {
		where = " where " + strings.Join(clauses, " and ")
	}
	var id sql.NullInt64
	if err := s.db.QueryRow(
		"select max(id) as id from agent_events"+where,
		params...,
	).Scan(&id); err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// LatestPaneEventScope mirrors the newest-first pane read
// #minimalAgentFromLatestEvent performs: the scope metadata of the newest event
// of one pane + generation that predates the plan.
func (s *AgentEventStore) LatestPaneEventScope(
	input LatestPaneEventScopeInput,
) (*LatestPaneEventScopeRow, error) {
	generationClause := "pane_generation is null"
	params := []any{input.HerdrSessionName, input.PaneID, input.CreatedBeforeMs}
	if input.PaneGeneration != nil {
		generationClause = "pane_generation = ?"
		params = append(params, *input.PaneGeneration)
	}
	var row LatestPaneEventScopeRow
	err := s.db.QueryRow(
		`select pane_id, pane_generation, terminal_id, workspace_id, herdr_session_name
		 from agent_events
		 where herdr_session_name = ? and pane_id = ? and created_at <= ? and `+
			generationClause+`
		 order by id desc limit 1`,
		params...,
	).Scan(
		&row.PaneID,
		&row.PaneGeneration,
		&row.TerminalID,
		&row.WorkspaceID,
		&row.HerdrSessionName,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// EventIDByIdempotencyKey mirrors the `select id from agent_events where
// idempotency_key = ? limit 1` probe #isAgentStalled performs.
func (s *AgentEventStore) EventIDByIdempotencyKey(idempotencyKey string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRow(
		"select id from agent_events where idempotency_key = ? limit 1", idempotencyKey,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// PaneEventTail mirrors the newest-first read over one pane's own event
// history that #salvageableTerminalBody performs. The generation clause is
// `pane_generation is null` when the plan carries no generation, exactly like
// the TS query.
func (s *AgentEventStore) PaneEventTail(
	input PaneEventTailInput,
) ([]PaneEventTailRow, error) {
	generationClause := "pane_generation is null"
	params := []any{input.HerdrSessionName, input.PaneID, input.CreatedBeforeMs}
	if input.PaneGeneration != nil {
		generationClause = "pane_generation = ?"
		params = append(params, *input.PaneGeneration)
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 20
	}
	params = append(params, limit)
	rows, err := s.db.Query(
		`select compact_history_json, delivery_attempts from agent_events
		 where herdr_session_name = ? and pane_id = ? and created_at <= ? and `+
			generationClause+`
		 order by id desc limit ?`,
		params...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	tail := []PaneEventTailRow{}
	for rows.Next() {
		var compactHistoryJSON sql.NullString
		var deliveryAttempts int
		if err := rows.Scan(&compactHistoryJSON, &deliveryAttempts); err != nil {
			return nil, err
		}
		tail = append(tail, PaneEventTailRow{
			CompactHistory:   parseCompactHistoryJSON(nullableString(compactHistoryJSON)),
			DeliveryAttempts: deliveryAttempts,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tail, nil
}

// PaneDeliveredTerminalBodies mirrors the bounded delivered/acked terminal
// window read the salvage path compares against.
func (s *AgentEventStore) PaneDeliveredTerminalBodies(
	input PaneDeliveredTerminalBodiesInput,
) ([]PaneDeliveredTerminalBodyRow, error) {
	generationClause := "pane_generation is null"
	params := []any{input.HerdrSessionName, input.PaneID, input.CreatedBeforeMs}
	if input.PaneGeneration != nil {
		generationClause = "pane_generation = ?"
		params = append(params, *input.PaneGeneration)
	}
	limit := input.Limit
	if limit <= 0 {
		limit = observability.STALE_DUPLICATE_GUARD_SCAN_LIMIT
	}
	params = append(params, limit)
	rows, err := s.db.Query(
		`select compact_history_json, payload_json from agent_events
		 where herdr_session_name = ? and pane_id = ? and created_at <= ? and `+
			generationClause+`
		   and type in ('agent.idle', 'agent.done', 'agent.blocked')
		   and status in ('delivered', 'acked')
		 order by id desc limit ?`,
		params...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	bodies := []PaneDeliveredTerminalBodyRow{}
	for rows.Next() {
		var compactHistoryJSON sql.NullString
		var payloadJSON sql.NullString
		if err := rows.Scan(&compactHistoryJSON, &payloadJSON); err != nil {
			return nil, err
		}
		bodies = append(bodies, PaneDeliveredTerminalBodyRow{
			AgentKind:      agentKindFromPayloadJSON(nullableString(payloadJSON)),
			CompactHistory: parseCompactHistoryJSON(nullableString(compactHistoryJSON)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return bodies, nil
}

// PlanOutcomeEventExists mirrors the `select 1 from agent_events where
// herdr_session_name = ? and idempotency_key = ?` probe the failed/discarded
// backfills run before appending.
func (s *AgentEventStore) PlanOutcomeEventExists(
	herdrSessionName string,
	idempotencyKey string,
) (bool, error) {
	var marker int
	err := s.db.QueryRow(
		"select 1 from agent_events where herdr_session_name = ? and idempotency_key = ?",
		herdrSessionName, idempotencyKey,
	).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// DeliveredTerminalHistories mirrors the bounded delivered/acked terminal
// window read the stale-baseline duplicate guard performs. The window is
// restricted to rows that represent a deliverable terminal round, matching
// isDeliverableAgentEvent: `agent.done`, `agent.blocked`, or `agent.idle` whose
// payload `from` is `working`.
func (s *AgentEventStore) DeliveredTerminalHistories(
	input DeliveredTerminalHistoriesInput,
) ([]DeliveredTerminalHistoryRow, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = observability.STALE_DUPLICATE_GUARD_SCAN_LIMIT
	}
	rows, err := s.db.Query(
		`select compact_history_json from agent_events
		 where agent_id = ? and herdr_session_name = ?
		   and (type in ('agent.done', 'agent.blocked')
		        or (type = 'agent.idle' and json_extract(payload_json, '$.from') = 'working'))
		   and status in ('delivered', 'acked')
		 order by id desc limit ?`,
		input.AgentID, input.HerdrSessionName, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	histories := []DeliveredTerminalHistoryRow{}
	for rows.Next() {
		var compactHistoryJSON sql.NullString
		if err := rows.Scan(&compactHistoryJSON); err != nil {
			return nil, err
		}
		histories = append(histories, DeliveredTerminalHistoryRow{
			CompactHistory: parseCompactHistoryJSON(nullableString(compactHistoryJSON)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return histories, nil
}

// Get mirrors get: it fails when no event with that id exists.
func (s *AgentEventStore) Get(id int64) (observability.AgentEventRecord, error) {
	row, ok, err := scanAgentEvent(s.db.QueryRow(
		"select "+agentEventColumns+" from agent_events where id = ?", id,
	))
	if err != nil {
		return observability.AgentEventRecord{}, err
	}
	if !ok {
		return observability.AgentEventRecord{}, fmt.Errorf("Agent event not found: %d", id)
	}
	return row, nil
}

func (s *AgentEventStore) getByIdempotencyKey(
	herdrSessionName string,
	idempotencyKey string,
) (observability.AgentEventRecord, bool, error) {
	row, ok, err := scanAgentEvent(s.db.QueryRow(
		"select "+agentEventColumns+
			" from agent_events where herdr_session_name = ? and idempotency_key = ?",
		herdrSessionName, idempotencyKey,
	))
	if err != nil || !ok {
		return observability.AgentEventRecord{}, false, err
	}
	return row, true, nil
}

// agentEventRow is one stored row of agent_events.
type agentEventRow struct {
	agentID               sql.NullString
	compactHistoryJSON    sql.NullString
	createdAtMs           int64
	herdrSessionName      string
	id                    int64
	idempotencyKey        sql.NullString
	paneID                sql.NullString
	paneGeneration        sql.NullString
	payloadJSON           string
	deliveryAttempts      int
	lastAttemptAtMs       sql.NullInt64
	nextAttemptAtMs       sql.NullInt64
	lastFailureCode       sql.NullString
	invalidatedReason     sql.NullString
	deliveredToTerminalID sql.NullString
	status                string
	deliverable           int
	terminalID            sql.NullString
	eventType             string
	workspaceID           sql.NullString
}

// scanAgentEvent scans one row; ok is false when there is no such row.
func scanAgentEvent(row *sql.Row) (observability.AgentEventRecord, bool, error) {
	var stored agentEventRow
	if err := row.Scan(
		&stored.agentID,
		&stored.compactHistoryJSON,
		&stored.createdAtMs,
		&stored.herdrSessionName,
		&stored.id,
		&stored.idempotencyKey,
		&stored.paneID,
		&stored.paneGeneration,
		&stored.payloadJSON,
		&stored.deliveryAttempts,
		&stored.lastAttemptAtMs,
		&stored.nextAttemptAtMs,
		&stored.lastFailureCode,
		&stored.invalidatedReason,
		&stored.deliveredToTerminalID,
		&stored.status,
		&stored.deliverable,
		&stored.terminalID,
		&stored.eventType,
		&stored.workspaceID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return observability.AgentEventRecord{}, false, nil
		}
		return observability.AgentEventRecord{}, false, err
	}
	return mapAgentEvent(stored), true, nil
}

func scanAgentEvents(
	rows *sql.Rows,
	err error,
) ([]observability.AgentEventRecord, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := []observability.AgentEventRecord{}
	for rows.Next() {
		var stored agentEventRow
		if err := rows.Scan(
			&stored.agentID,
			&stored.compactHistoryJSON,
			&stored.createdAtMs,
			&stored.herdrSessionName,
			&stored.id,
			&stored.idempotencyKey,
			&stored.paneID,
			&stored.paneGeneration,
			&stored.payloadJSON,
			&stored.deliveryAttempts,
			&stored.lastAttemptAtMs,
			&stored.nextAttemptAtMs,
			&stored.lastFailureCode,
			&stored.invalidatedReason,
			&stored.deliveredToTerminalID,
			&stored.status,
			&stored.deliverable,
			&stored.terminalID,
			&stored.eventType,
			&stored.workspaceID,
		); err != nil {
			return nil, err
		}
		events = append(events, mapAgentEvent(stored))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// mapAgentEvent mirrors mapAgentEvent: an unparseable or absent JSON column is
// reported as absent, never as an error, and an absent payload becomes {}.
func mapAgentEvent(row agentEventRow) observability.AgentEventRecord {
	status := observability.AgentEventStatus(row.status)
	deliveryAttempts := row.deliveryAttempts
	return observability.AgentEventRecord{
		AgentID:               nullableString(row.agentID),
		CompactHistory:        nullableCompactHistory(row.compactHistoryJSON),
		CreatedAt:             formatTimestamp(row.createdAtMs),
		HerdrSessionName:      row.herdrSessionName,
		ID:                    row.id,
		PaneID:                nullableString(row.paneID),
		PaneGeneration:        nullableString(row.paneGeneration),
		Deliverable:           row.deliverable,
		Status:                &status,
		DeliveryAttempts:      &deliveryAttempts,
		LastAttemptAt:         nullableTimestamp(row.lastAttemptAtMs),
		NextAttemptAt:         nullableTimestamp(row.nextAttemptAtMs),
		LastFailureCode:       nullableString(row.lastFailureCode),
		InvalidatedReason:     nullableString(row.invalidatedReason),
		DeliveredToTerminalID: nullableString(row.deliveredToTerminalID),
		Payload:               parsePayload(row.payloadJSON),
		TerminalID:            nullableString(row.terminalID),
		Type:                  observability.AgentEventType(row.eventType),
		WorkspaceID:           nullableString(row.workspaceID),
	}
}

// parsePayload mirrors `parseJson<unknown>(row.payload_json) ?? {}`: a missing,
// unparseable or explicitly null payload becomes an empty record.
func parsePayload(value string) any {
	if value == "" {
		return map[string]any{}
	}
	var payload any
	if err := json.Unmarshal([]byte(value), &payload); err != nil || payload == nil {
		return map[string]any{}
	}
	return payload
}

// nullableCompactHistory mirrors `parseJson<CompactAgentHistory>(...)`: an absent
// or unparseable compact history is reported as absent, never as an error.
func nullableCompactHistory(value sql.NullString) *observability.CompactAgentHistory {
	if !value.Valid || value.String == "" {
		return nil
	}
	var parsed observability.CompactAgentHistory
	if err := json.Unmarshal([]byte(value.String), &parsed); err != nil {
		return nil
	}
	return &parsed
}
