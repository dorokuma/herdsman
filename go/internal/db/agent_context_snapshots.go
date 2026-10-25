package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// AgentContextSnapshotInput mirrors the TS put input: the record without the
// store-assigned updatedAt. The type lives in internal/observability (which
// internal/db imports, and which cannot import internal/db), so the context
// service and the store share one type; the alias keeps the existing call sites
// unchanged.
type AgentContextSnapshotInput = observability.AgentContextSnapshotInput

// AgentContextSnapshotStore ports src/db/agent-context-snapshots.ts.
type AgentContextSnapshotStore struct {
	db *sql.DB
}

// NewAgentContextSnapshotStore builds a store on the shared connection.
func NewAgentContextSnapshotStore(db *sql.DB) *AgentContextSnapshotStore {
	return &AgentContextSnapshotStore{db: db}
}

// agentContextSnapshotColumns is the explicit `select *` column list of
// agent_context_snapshots.
const agentContextSnapshotColumns = "agent_id, compact_history_json, history_ref_json, pane_revision," +
	" source_path, source_mtime_ms, source_size, updated_at"

// Get mirrors get: the stored snapshot, or nil when the agent has none.
func (s *AgentContextSnapshotStore) Get(
	agentID string,
) (*observability.AgentContextSnapshotRecord, error) {
	row, ok, err := scanAgentContextSnapshot(s.db.QueryRow(
		"select "+agentContextSnapshotColumns+
			" from agent_context_snapshots where agent_id = ?", agentID,
	))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	record, err := mapAgentContextSnapshot(row)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// ListByAgentIds mirrors listByAgentIds.
func (s *AgentContextSnapshotStore) ListByAgentIds(
	agentIDs []string,
) ([]observability.AgentContextSnapshotRecord, error) {
	if len(agentIDs) == 0 {
		return []observability.AgentContextSnapshotRecord{}, nil
	}
	args := make([]any, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		args = append(args, agentID)
	}
	rows, err := s.db.Query(
		"select "+agentContextSnapshotColumns+" from agent_context_snapshots where agent_id in ("+
			placeholders(len(agentIDs))+")",
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := []observability.AgentContextSnapshotRecord{}
	for rows.Next() {
		row, err := scanAgentContextSnapshotRow(rows)
		if err != nil {
			return nil, err
		}
		record, err := mapAgentContextSnapshot(row)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// Put mirrors put: the snapshot is inserted or replaced, its updatedAt is
// forced to move forward past any previous write, and the stored record is
// returned. The history ref and the source fingerprint must both be present or
// both be absent.
func (s *AgentContextSnapshotStore) Put(
	input AgentContextSnapshotInput,
) (observability.AgentContextSnapshotRecord, error) {
	if (input.HistoryRef == nil) != (input.SourceFingerprint == nil) {
		return observability.AgentContextSnapshotRecord{}, errors.New(
			"Agent context history ref and source fingerprint must both be null or non-null")
	}
	previous, ok, err := scanAgentContextSnapshot(s.db.QueryRow(
		"select "+agentContextSnapshotColumns+
			" from agent_context_snapshots where agent_id = ?", input.AgentID,
	))
	if err != nil {
		return observability.AgentContextSnapshotRecord{}, err
	}
	// The stored timestamp must never move backwards, so a write that lands in
	// the same millisecond as the previous one is nudged one millisecond past it.
	now := time.Now().UnixMilli()
	if ok && previous.updatedAtMs+1 > now {
		now = previous.updatedAtMs + 1
	}
	compactHistoryJSON, err := json.Marshal(input.CompactHistory)
	if err != nil {
		return observability.AgentContextSnapshotRecord{}, err
	}
	var historyRefJSON []byte
	if input.HistoryRef != nil {
		historyRefJSON, err = json.Marshal(input.HistoryRef)
		if err != nil {
			return observability.AgentContextSnapshotRecord{}, err
		}
	}
	var sourcePath, sourceMtimeMs, sourceSize any
	if input.SourceFingerprint != nil {
		sourcePath = input.SourceFingerprint.Path
		sourceMtimeMs = input.SourceFingerprint.MtimeMs
		sourceSize = input.SourceFingerprint.Size
	}
	if _, err := s.db.Exec(
		`insert into agent_context_snapshots
		 (agent_id, compact_history_json, history_ref_json, pane_revision, source_path, source_mtime_ms, source_size, updated_at)
		 values (?, ?, ?, ?, ?, ?, ?, ?)
		 on conflict(agent_id) do update set
		   compact_history_json = excluded.compact_history_json,
		   history_ref_json = excluded.history_ref_json,
		   pane_revision = excluded.pane_revision,
		   source_path = excluded.source_path,
		   source_mtime_ms = excluded.source_mtime_ms,
		   source_size = excluded.source_size,
		   updated_at = excluded.updated_at`,
		input.AgentID, string(compactHistoryJSON), nullableBytes(historyRefJSON),
		input.PaneRevision, sourcePath, sourceMtimeMs, sourceSize, now,
	); err != nil {
		return observability.AgentContextSnapshotRecord{}, err
	}
	record, err := s.Get(input.AgentID)
	if err != nil {
		return observability.AgentContextSnapshotRecord{}, err
	}
	if record == nil {
		return observability.AgentContextSnapshotRecord{}, errors.New(
			"Agent context snapshot write failed")
	}
	return *record, nil
}

// Delete mirrors delete.
func (s *AgentContextSnapshotStore) Delete(agentID string) error {
	_, err := s.db.Exec("delete from agent_context_snapshots where agent_id = ?", agentID)
	return err
}

// agentContextSnapshotRow is one stored row; the nullable columns keep their SQL
// shape so the mapping can mirror the TS null checks.
type agentContextSnapshotRow struct {
	agentID            string
	compactHistoryJSON string
	historyRefJSON     sql.NullString
	paneRevision       sql.NullInt64
	sourcePath         sql.NullString
	sourceMtimeMs      sql.NullFloat64
	sourceSize         sql.NullFloat64
	updatedAtMs        int64
}

func scanAgentContextSnapshot(row *sql.Row) (agentContextSnapshotRow, bool, error) {
	stored, err := scanAgentContextSnapshotRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentContextSnapshotRow{}, false, nil
		}
		return agentContextSnapshotRow{}, false, err
	}
	return stored, true, nil
}

// scanAgentContextSnapshotRow scans one row from either a *sql.Row or *sql.Rows.
func scanAgentContextSnapshotRow(scanner interface{ Scan(dest ...any) error }) (agentContextSnapshotRow, error) {
	var row agentContextSnapshotRow
	if err := scanner.Scan(
		&row.agentID,
		&row.compactHistoryJSON,
		&row.historyRefJSON,
		&row.paneRevision,
		&row.sourcePath,
		&row.sourceMtimeMs,
		&row.sourceSize,
		&row.updatedAtMs,
	); err != nil {
		return agentContextSnapshotRow{}, err
	}
	return row, nil
}

func mapAgentContextSnapshot(
	row agentContextSnapshotRow,
) (observability.AgentContextSnapshotRecord, error) {
	sourceFingerprint, err := mapSourceFingerprint(row)
	if err != nil {
		return observability.AgentContextSnapshotRecord{}, err
	}
	var historyRef *observability.AgentHistoryRef
	if row.historyRefJSON.Valid {
		parsed, err := parseContextHistoryRef(row.historyRefJSON.String)
		if err != nil {
			return observability.AgentContextSnapshotRecord{}, err
		}
		historyRef = &parsed
	}
	if (historyRef == nil) != (sourceFingerprint == nil) {
		return observability.AgentContextSnapshotRecord{}, errors.New(
			"Stored agent context history ref and source fingerprint are inconsistent")
	}
	compactHistory, err := parseCompactHistory(row.compactHistoryJSON)
	if err != nil {
		return observability.AgentContextSnapshotRecord{}, err
	}
	return observability.AgentContextSnapshotRecord{
		AgentID:           row.agentID,
		CompactHistory:    compactHistory,
		HistoryRef:        historyRef,
		PaneRevision:      nullableInt(row.paneRevision),
		SourceFingerprint: sourceFingerprint,
		UpdatedAt:         formatTimestamp(row.updatedAtMs),
	}, nil
}

// mapSourceFingerprint mirrors mapSourceFingerprint: the three source columns
// are either all absent or all present.
func mapSourceFingerprint(
	row agentContextSnapshotRow,
) (*observability.AgentHistorySourceFingerprint, error) {
	if !row.sourcePath.Valid && !row.sourceMtimeMs.Valid && !row.sourceSize.Valid {
		return nil, nil
	}
	if !row.sourcePath.Valid || !row.sourceMtimeMs.Valid || !row.sourceSize.Valid {
		return nil, errors.New("Stored agent context source fingerprint is invalid")
	}
	return &observability.AgentHistorySourceFingerprint{
		MtimeMs: row.sourceMtimeMs.Float64,
		Path:    row.sourcePath.String,
		Size:    row.sourceSize.Float64,
	}, nil
}

func parseCompactHistory(value string) (observability.CompactAgentHistory, error) {
	var compactHistory observability.CompactAgentHistory
	if err := json.Unmarshal([]byte(value), &compactHistory); err != nil {
		return observability.CompactAgentHistory{},
			fmt.Errorf("Stored agent context compact history is invalid: %w", err)
	}
	return compactHistory, nil
}

func parseContextHistoryRef(value string) (observability.AgentHistoryRef, error) {
	var historyRef observability.AgentHistoryRef
	if err := json.Unmarshal([]byte(value), &historyRef); err != nil {
		return observability.AgentHistoryRef{},
			fmt.Errorf("Stored agent context history ref is invalid: %w", err)
	}
	if historyRef.Kind != observability.HistoryRefAgentSession &&
		historyRef.Kind != observability.HistoryRefDiscoveredFile {
		return observability.AgentHistoryRef{}, fmt.Errorf(
			"Stored agent context history ref is invalid: kind %q", historyRef.Kind)
	}
	return historyRef, nil
}

// nullableBytes turns marshaled JSON into a nullable bind value.
func nullableBytes(value []byte) any {
	if value == nil {
		return nil
	}
	return string(value)
}

func nullableInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	parsed := int(value.Int64)
	return &parsed
}
