package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// StatusPlanMaxAttempts, StatusEventPlanStatus, StatusEventPlanRecord,
// StatusEventPlanAgent, InsertPendingInput, MarkRetryOptions and
// CancelSupersededPendingPlansInput live in internal/observability, because
// internal/observability cannot import internal/db (that would be an import
// cycle) yet its AgentIndexService needs to speak them. The aliases keep the
// existing call sites and test names unchanged.
// StatusPlanMaxAttempts mirrors STATUS_PLAN_MAX_ATTEMPTS.
const StatusPlanMaxAttempts = observability.StatusPlanMaxAttempts

type StatusEventPlanStatus = observability.StatusEventPlanStatus

type StatusEventPlanRecord = observability.StatusEventPlanRecord

type StatusEventPlanAgent = observability.StatusEventPlanAgent

type InsertPendingInput = observability.InsertPendingInput

type MarkRetryOptions = observability.MarkRetryOptions

type CancelSupersededPendingPlansInput = observability.CancelSupersededPendingPlansInput

type StalledDiscardPlanRow = observability.StalledDiscardPlanRow

// StatusPlanPending mirrors the TS union member of the same name.
const StatusPlanPending StatusEventPlanStatus = observability.StatusPlanPending

// StatusPlanRunning mirrors the TS union member of the same name.
const StatusPlanRunning StatusEventPlanStatus = observability.StatusPlanRunning

// StatusPlanCompleted mirrors the TS union member of the same name.
const StatusPlanCompleted StatusEventPlanStatus = observability.StatusPlanCompleted

// StatusPlanCancelled mirrors the TS union member of the same name.
const StatusPlanCancelled StatusEventPlanStatus = observability.StatusPlanCancelled

// StatusPlanFailed mirrors the TS union member of the same name.
const StatusPlanFailed StatusEventPlanStatus = observability.StatusPlanFailed

// StatusPlanDiscarded mirrors the TS union member of the same name.
const StatusPlanDiscarded StatusEventPlanStatus = observability.StatusPlanDiscarded

// StatusEventPlanStore ports src/db/status-event-plans.ts.
type StatusEventPlanStore struct {
	db *sql.DB
}

// NewStatusEventPlanStore builds a store on the shared connection.
func NewStatusEventPlanStore(db *sql.DB) *StatusEventPlanStore {
	return &StatusEventPlanStore{db: db}
}

// statusEventPlanColumns is the explicit `select *` column list of
// status_event_plans.
const statusEventPlanColumns = "agent_id, attempts, compact_history_json, created_at, from_status," +
	" herdr_event_key, herdr_session_name, id, last_error, pane_generation, pane_id, status," +
	" to_status, updated_at"

// InsertPending mirrors insertPending: a plan is inserted as pending, unless the
// same (session, herdr event key) already has a row — then that row is returned
// as it stands, even when it is already settled.
func (s *StatusEventPlanStore) InsertPending(
	input InsertPendingInput,
) (StatusEventPlanRecord, error) {
	herdrSessionName := input.HerdrSessionName
	agentID := input.AgentID
	paneID := input.PaneID
	paneGeneration := input.PaneGeneration
	if input.Agent != nil {
		if herdrSessionName == "" {
			herdrSessionName = input.Agent.HerdrSessionName
		}
		if agentID == "" {
			agentID = input.Agent.ID
		}
		if paneID == "" {
			paneID = input.Agent.PaneID
		}
		if paneGeneration == nil {
			paneGeneration = input.Agent.PaneGeneration
		}
	}
	fromStatus := input.FromStatus
	if fromStatus == "" {
		fromStatus = input.From
	}
	toStatus := input.ToStatus
	if toStatus == "" {
		toStatus = input.To
	}
	if herdrSessionName == "" || agentID == "" || paneID == "" || fromStatus == "" || toStatus == "" {
		return StatusEventPlanRecord{}, errors.New("Missing required fields for status event plan")
	}

	if input.HerdrEventKey != nil && *input.HerdrEventKey != "" {
		existing, ok, err := s.getByEventKey(herdrSessionName, *input.HerdrEventKey)
		if err != nil {
			return StatusEventPlanRecord{}, err
		}
		if ok {
			return existing, nil
		}
	}

	now := time.Now().UnixMilli()
	var compactHistoryJSON any
	if input.CompactHistory != nil {
		encoded, err := json.Marshal(input.CompactHistory)
		if err != nil {
			return StatusEventPlanRecord{}, err
		}
		compactHistoryJSON = string(encoded)
	}
	result, err := s.db.Exec(
		`insert into status_event_plans
		 (agent_id, attempts, compact_history_json, created_at, from_status, herdr_event_key, herdr_session_name, last_error, pane_generation, pane_id, status, to_status, updated_at)
		 values (?, 0, ?, ?, ?, ?, ?, null, ?, ?, 'pending', ?, ?)`,
		agentID, compactHistoryJSON, now, fromStatus, input.HerdrEventKey, herdrSessionName,
		paneGeneration, paneID, toStatus, now,
	)
	if err != nil {
		// The unique (session, herdr event key) index is the authority on a
		// concurrent duplicate: the existing row wins over the error.
		if input.HerdrEventKey != nil && *input.HerdrEventKey != "" {
			existing, ok, lookupErr := s.getByEventKey(herdrSessionName, *input.HerdrEventKey)
			if lookupErr != nil {
				return StatusEventPlanRecord{}, lookupErr
			}
			if ok {
				return existing, nil
			}
		}
		return StatusEventPlanRecord{}, err
	}
	insertedID, err := result.LastInsertId()
	if err != nil {
		return StatusEventPlanRecord{}, err
	}
	row, err := s.Get(insertedID)
	if err != nil {
		return StatusEventPlanRecord{}, err
	}
	if _, err := s.CancelSupersededPendingPlans(CancelSupersededPendingPlansInput{
		AgentID:          agentID,
		HerdrSessionName: herdrSessionName,
		KeepID:           row.ID,
		ToStatus:         toStatus,
	}); err != nil {
		return StatusEventPlanRecord{}, err
	}
	return row, nil
}

// CancelSupersededPendingPlans mirrors cancelSupersededPendingPlans: pending or
// running plans of the same agent and session that a newer transition
// supersedes are cancelled, so an obsolete plan can no longer execute late. See
// the TS doc comment for why only un-wakeable idle plans qualify.
func (s *StatusEventPlanStore) CancelSupersededPendingPlans(
	input CancelSupersededPendingPlansInput,
) (int, error) {
	result, err := s.db.Exec(
		`update status_event_plans
		 set status = 'cancelled', last_error = 'PLAN_SUPERSEDED', updated_at = ?
		 where id <> ?
		   and agent_id = ?
		   and herdr_session_name = ?
		   and status in ('pending', 'running')
		   and to_status = 'idle'
		   and from_status <> 'working'
		   and to_status <> ?`,
		time.Now().UnixMilli(), input.KeepID, input.AgentID, input.HerdrSessionName, input.ToStatus,
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

// Get mirrors get: it fails when no plan with that id exists.
func (s *StatusEventPlanStore) Get(id int64) (StatusEventPlanRecord, error) {
	row, ok, err := s.getRow(id)
	if err != nil {
		return StatusEventPlanRecord{}, err
	}
	if !ok {
		return StatusEventPlanRecord{}, fmt.Errorf("StatusEventPlan with id %d not found", id)
	}
	return mapStatusEventPlan(row), nil
}

// ListUnfinished mirrors listUnfinished.
func (s *StatusEventPlanStore) ListUnfinished() ([]StatusEventPlanRecord, error) {
	return scanStatusEventPlans(s.db.Query(
		"select " + statusEventPlanColumns +
			" from status_event_plans where status in ('pending', 'running') order by id asc",
	))
}

// ListFailed mirrors listFailed.
func (s *StatusEventPlanStore) ListFailed() ([]StatusEventPlanRecord, error) {
	return scanStatusEventPlans(s.db.Query(
		"select " + statusEventPlanColumns +
			" from status_event_plans where status = 'failed' order by id asc",
	))
}

// ListDiscarded mirrors listDiscarded.
func (s *StatusEventPlanStore) ListDiscarded() ([]StatusEventPlanRecord, error) {
	return scanStatusEventPlans(s.db.Query(
		"select " + statusEventPlanColumns +
			" from status_event_plans where status = 'discarded' order by id asc",
	))
}

// ListWaitingHistory mirrors listWaitingHistory.
func (s *StatusEventPlanStore) ListWaitingHistory() ([]StatusEventPlanRecord, error) {
	return scanStatusEventPlans(s.db.Query(
		"select "+statusEventPlanColumns+
			" from status_event_plans where status = 'pending' and last_error = ? order by id asc",
		observability.PlanWaitingHistory,
	))
}

// ResetRunningToPending mirrors resetRunningToPending: plans left running by a
// crashed drain are made pending again.
func (s *StatusEventPlanStore) ResetRunningToPending() (int, error) {
	result, err := s.db.Exec(
		"update status_event_plans set status = 'pending', updated_at = ? where status = 'running'",
		time.Now().UnixMilli(),
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

// MarkRunning mirrors markRunning: only a pending plan starts running.
func (s *StatusEventPlanStore) MarkRunning(id int64) (bool, error) {
	result, err := s.db.Exec(
		"update status_event_plans set status = 'running', updated_at = ? where id = ? and status = 'pending'",
		time.Now().UnixMilli(), id,
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

// MarkCompleted mirrors markCompleted.
func (s *StatusEventPlanStore) MarkCompleted(id int64) error {
	return s.markSettled(id, string(StatusPlanCompleted))
}

// MarkCancelled mirrors markCancelled.
func (s *StatusEventPlanStore) MarkCancelled(id int64) error {
	return s.markSettled(id, string(StatusPlanCancelled))
}

// MarkDiscarded mirrors markDiscarded: the plan is discarded regardless of its
// current status, and the reason replaces the stored error when given.
func (s *StatusEventPlanStore) MarkDiscarded(id int64, reason *string) error {
	current, err := s.Get(id)
	if err != nil {
		return err
	}
	lastError := current.LastError
	if reason != nil {
		lastError = reason
	}
	_, err = s.db.Exec(
		"update status_event_plans set status = 'discarded', last_error = ?, updated_at = ? where id = ?",
		lastError, time.Now().UnixMilli(), id,
	)
	return err
}

// markSettled mirrors the shared shape of markCompleted and markCancelled: only
// an unfinished plan settles.
func (s *StatusEventPlanStore) markSettled(id int64, status string) error {
	_, err := s.db.Exec(
		"update status_event_plans set status = '"+status+
			"', updated_at = ? where id = ? and status in ('pending', 'running')",
		time.Now().UnixMilli(), id,
	)
	return err
}

// MarkRetry mirrors markRetry: the attempt counter moves forward and the plan
// is retried, failed or discarded depending on the stall, window and attempt
// budget. A row in a terminal state — or one that changed concurrently — is left
// alone and reported as nil.
func (s *StatusEventPlanStore) MarkRetry(
	id int64,
	err error,
	options *MarkRetryOptions,
) (*StatusEventPlanRecord, error) {
	row, ok, lookupErr := s.getRow(id)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if !ok {
		return nil, fmt.Errorf("StatusEventPlan with id %d not found", id)
	}
	current := mapStatusEventPlan(row)
	switch current.Status {
	case StatusPlanCompleted, StatusPlanCancelled, StatusPlanFailed, StatusPlanDiscarded:
		return nil, nil
	}
	errorMessage := "undefined"
	if err != nil {
		errorMessage = err.Error()
	}
	isTargetWorking := false
	stallDetected := false
	maxAttempts := StatusPlanMaxAttempts
	var totalWindowMs *int64
	isWindowExpired := false
	if options != nil {
		isTargetWorking = options.IsTargetWorking
		stallDetected = options.StallDetected
		isWindowExpired = options.IsWindowExpired
		if options.MaxAttempts > 0 {
			maxAttempts = options.MaxAttempts
		}
		totalWindowMs = options.TotalWindowMs
	}

	attempts := current.Attempts + 1
	newStatus := StatusPlanPending
	now := time.Now().UnixMilli()
	elapsedMs := now - row.createdAtMs
	if !isWindowExpired && totalWindowMs != nil {
		isWindowExpired = elapsedMs >= *totalWindowMs
	}
	attemptsExhausted := attempts >= maxAttempts
	if stallDetected || isWindowExpired || attemptsExhausted {
		if isTargetWorking && !stallDetected && !isWindowExpired {
			newStatus = StatusPlanPending
		} else if errorMessage == observability.PlanWaitingHistory {
			newStatus = StatusPlanDiscarded
		} else {
			newStatus = StatusPlanFailed
		}
	}
	finalError := errorMessage
	if stallDetected && newStatus != StatusPlanPending {
		finalError = observability.PlanStalled
	}

	result, execErr := s.db.Exec(
		"update status_event_plans set attempts = ?, status = ?, last_error = ?, updated_at = ?"+
			" where id = ? and status in ('pending', 'running')",
		attempts, string(newStatus), finalError, now, id,
	)
	if execErr != nil {
		return nil, execErr
	}
	affected, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return nil, affectedErr
	}
	if affected == 0 {
		return nil, nil
	}
	updated, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// DeleteSettledOlderThan mirrors deleteSettledOlderThan: settled plans
// (completed/cancelled/failed/discarded) older than the given age are removed;
// pending and running rows are never touched.
func (s *StatusEventPlanStore) DeleteSettledOlderThan(ageMs int64) (int, error) {
	result, err := s.db.Exec(
		`delete from status_event_plans
		 where status in ('completed', 'cancelled', 'failed', 'discarded') and updated_at < ?`,
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

// LatestStalledDiscard mirrors the newest discarded plan of one identity whose
// last_error is in STALLED_PLAN_ERRORS, or nil when there is none.
func (s *StatusEventPlanStore) LatestStalledDiscard(
	agentID string,
	herdrSessionName string,
) (*StalledDiscardPlanRow, error) {
	placeholders := make([]string, 0, len(observability.StalledPlanErrors))
	params := []any{agentID, herdrSessionName}
	for _, planError := range observability.StalledPlanErrors {
		placeholders = append(placeholders, "?")
		params = append(params, planError)
	}
	var row StalledDiscardPlanRow
	err := s.db.QueryRow(
		"select id, last_error, updated_at from status_event_plans"+
			" where agent_id = ? and herdr_session_name = ? and status = 'discarded'"+
			" and last_error in ("+strings.Join(placeholders, ", ")+")"+
			" order by id desc limit 1",
		params...,
	).Scan(&row.ID, &row.LastError, &row.UpdatedAtMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// HasCompletedPlanAfter mirrors the `id > ? and status = 'completed'` existence
// probe #isAgentStalled performs.
func (s *StatusEventPlanStore) HasCompletedPlanAfter(
	agentID string,
	herdrSessionName string,
	afterID int64,
) (bool, error) {
	var marker int
	err := s.db.QueryRow(
		"select id from status_event_plans"+
			" where agent_id = ? and herdr_session_name = ? and id > ? and status = 'completed'"+
			" limit 1",
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

// statusEventPlanRow is one stored row of status_event_plans.
type statusEventPlanRow struct {
	agentID            string
	attempts           int
	compactHistoryJSON sql.NullString
	createdAtMs        int64
	fromStatus         string
	herdrEventKey      sql.NullString
	herdrSessionName   string
	id                 int64
	lastError          sql.NullString
	paneGeneration     sql.NullString
	paneID             string
	status             string
	toStatus           string
	updatedAtMs        int64
}

func (s *StatusEventPlanStore) getRow(id int64) (statusEventPlanRow, bool, error) {
	var row statusEventPlanRow
	if err := s.db.QueryRow(
		"select "+statusEventPlanColumns+" from status_event_plans where id = ?", id,
	).Scan(
		&row.agentID,
		&row.attempts,
		&row.compactHistoryJSON,
		&row.createdAtMs,
		&row.fromStatus,
		&row.herdrEventKey,
		&row.herdrSessionName,
		&row.id,
		&row.lastError,
		&row.paneGeneration,
		&row.paneID,
		&row.status,
		&row.toStatus,
		&row.updatedAtMs,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return statusEventPlanRow{}, false, nil
		}
		return statusEventPlanRow{}, false, err
	}
	return row, true, nil
}

func (s *StatusEventPlanStore) getByEventKey(
	herdrSessionName string,
	herdrEventKey string,
) (StatusEventPlanRecord, bool, error) {
	var row statusEventPlanRow
	if err := s.db.QueryRow(
		"select "+statusEventPlanColumns+
			" from status_event_plans where herdr_session_name = ? and herdr_event_key = ?",
		herdrSessionName, herdrEventKey,
	).Scan(
		&row.agentID,
		&row.attempts,
		&row.compactHistoryJSON,
		&row.createdAtMs,
		&row.fromStatus,
		&row.herdrEventKey,
		&row.herdrSessionName,
		&row.id,
		&row.lastError,
		&row.paneGeneration,
		&row.paneID,
		&row.status,
		&row.toStatus,
		&row.updatedAtMs,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return StatusEventPlanRecord{}, false, nil
		}
		return StatusEventPlanRecord{}, false, err
	}
	return mapStatusEventPlan(row), true, nil
}

func scanStatusEventPlans(
	rows *sql.Rows,
	err error,
) ([]StatusEventPlanRecord, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	plans := []StatusEventPlanRecord{}
	for rows.Next() {
		var row statusEventPlanRow
		if err := rows.Scan(
			&row.agentID,
			&row.attempts,
			&row.compactHistoryJSON,
			&row.createdAtMs,
			&row.fromStatus,
			&row.herdrEventKey,
			&row.herdrSessionName,
			&row.id,
			&row.lastError,
			&row.paneGeneration,
			&row.paneID,
			&row.status,
			&row.toStatus,
			&row.updatedAtMs,
		); err != nil {
			return nil, err
		}
		plans = append(plans, mapStatusEventPlan(row))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return plans, nil
}

// mapStatusEventPlan mirrors mapStatusEventPlan: an unparseable or absent
// compact history is reported as absent, never as an error.
func mapStatusEventPlan(row statusEventPlanRow) StatusEventPlanRecord {
	var compactHistory *observability.CompactAgentHistory
	if row.compactHistoryJSON.Valid && row.compactHistoryJSON.String != "" {
		var parsed observability.CompactAgentHistory
		if err := json.Unmarshal([]byte(row.compactHistoryJSON.String), &parsed); err == nil {
			compactHistory = &parsed
		}
	}
	return StatusEventPlanRecord{
		AgentID:          row.agentID,
		Attempts:         row.attempts,
		CompactHistory:   compactHistory,
		CreatedAt:        formatTimestamp(row.createdAtMs),
		CreatedAtMs:      row.createdAtMs,
		FromStatus:       observability.AgentStatus(row.fromStatus),
		HerdrEventKey:    nullableString(row.herdrEventKey),
		HerdrSessionName: row.herdrSessionName,
		ID:               row.id,
		LastError:        nullableString(row.lastError),
		PaneGeneration:   nullableString(row.paneGeneration),
		PaneID:           row.paneID,
		Status:           StatusEventPlanStatus(row.status),
		ToStatus:         observability.AgentStatus(row.toStatus),
		UpdatedAt:        formatTimestamp(row.updatedAtMs),
	}
}
