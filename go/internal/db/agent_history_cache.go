package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dorokuma/herdsman/go/internal/agenthistory"
)

// AgentHistoryCacheRecord mirrors the TS store record of the same name.
type AgentHistoryCacheRecord struct {
	CompactHistory   agenthistory.CompactAgentHistory
	FormatterVersion string
	HistoryRef       agenthistory.HistoryRef
	ID               int64
	SourceMtimeMs    float64
	SourcePath       string
	SourceSize       int64
	UpdatedAt        string
}

// AgentHistoryCacheStore ports src/db/agent-history-cache.ts. It satisfies
// agenthistory.HistoryCacheStore, the subset the agent history service depends
// on.
type AgentHistoryCacheStore struct {
	db *sql.DB
}

// NewAgentHistoryCacheStore builds a store on the shared connection.
func NewAgentHistoryCacheStore(db *sql.DB) *AgentHistoryCacheStore {
	return &AgentHistoryCacheStore{db: db}
}

var _ agenthistory.HistoryCacheStore = (*AgentHistoryCacheStore)(nil)

// agentHistoryCacheColumns is the explicit `select *` column list of
// agent_history_cache.
const agentHistoryCacheColumns = "compact_history_json, formatter_version, history_ref_json, id," +
	" source_mtime_ms, source_path, source_size, updated_at"

// GetFresh mirrors getFresh: the row for the same source path, formatter version
// and fingerprint, or nil when nothing fresh is cached. A stored row whose JSON
// cannot be parsed is reported as a miss — the service then re-reads the source
// and the next Put rewrites the row.
func (s *AgentHistoryCacheStore) GetFresh(
	input agenthistory.HistoryCacheFreshInput,
) *agenthistory.HistoryCacheRecord {
	row, ok, err := scanAgentHistoryCache(s.db.QueryRow(
		"select "+agentHistoryCacheColumns+
			` from agent_history_cache
			 where source_path = ? and formatter_version = ? and source_mtime_ms = ? and source_size = ?`,
		input.SourcePath, input.FormatterVersion, input.SourceMtimeMs, input.SourceSize,
	))
	if err != nil || !ok {
		return nil
	}
	record, err := mapAgentHistoryCache(row)
	if err != nil {
		return nil
	}
	return &agenthistory.HistoryCacheRecord{
		CompactHistory:   record.CompactHistory,
		FormatterVersion: record.FormatterVersion,
		HistoryRef:       record.HistoryRef,
		ID:               record.ID,
		SourceMtimeMs:    record.SourceMtimeMs,
		SourcePath:       record.SourcePath,
		SourceSize:       record.SourceSize,
	}
}

// Put mirrors put: the compact history and its ref replace any cached row for
// the same source path and formatter version.
func (s *AgentHistoryCacheStore) Put(input agenthistory.HistoryCachePutInput) error {
	_, err := s.putRecord(input)
	return err
}

// putRecord is Put, returning the stored record the way the TS put does. The
// HistoryCacheStore interface only needs the error, so this stays internal.
func (s *AgentHistoryCacheStore) putRecord(
	input agenthistory.HistoryCachePutInput,
) (*AgentHistoryCacheRecord, error) {
	compactHistoryJSON, err := json.Marshal(input.CompactHistory)
	if err != nil {
		return nil, err
	}
	historyRefJSON, err := json.Marshal(input.HistoryRef)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(
		`insert into agent_history_cache
		 (source_path, formatter_version, history_ref_json, compact_history_json, source_mtime_ms, source_size, updated_at)
		 values (?, ?, ?, ?, ?, ?, ?)
		 on conflict(source_path, formatter_version) do update set
		   history_ref_json = excluded.history_ref_json,
		   compact_history_json = excluded.compact_history_json,
		   source_mtime_ms = excluded.source_mtime_ms,
		   source_size = excluded.source_size,
		   updated_at = excluded.updated_at`,
		input.SourcePath, input.FormatterVersion, string(historyRefJSON), string(compactHistoryJSON),
		input.SourceMtimeMs, input.SourceSize, now,
	); err != nil {
		return nil, err
	}
	row, ok, err := scanAgentHistoryCache(s.db.QueryRow(
		"select "+agentHistoryCacheColumns+
			" from agent_history_cache where source_path = ? and formatter_version = ?",
		input.SourcePath, input.FormatterVersion,
	))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("Agent history cache write failed")
	}
	record, err := mapAgentHistoryCache(row)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// DeleteOlderThan mirrors deleteOlderThan: rows whose updated_at is older than
// the given age are physically removed, and the number of removed rows returned.
func (s *AgentHistoryCacheStore) DeleteOlderThan(ageMs int64) (int, error) {
	result, err := s.db.Exec(
		"delete from agent_history_cache where updated_at < ?", time.Now().UnixMilli()-ageMs)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// ListSourcePaths mirrors listSourcePaths.
func (s *AgentHistoryCacheStore) ListSourcePaths() ([]string, error) {
	rows, err := s.db.Query(
		"select distinct source_path from agent_history_cache order by source_path")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	paths := []string{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

// DeleteBySourcePaths mirrors deleteBySourcePaths: the given set is deduplicated
// and removed in batches of 100, and the number of removed rows returned.
func (s *AgentHistoryCacheStore) DeleteBySourcePaths(sourcePaths []string) (int, error) {
	unique := make([]string, 0, len(sourcePaths))
	seen := make(map[string]struct{}, len(sourcePaths))
	for _, path := range sourcePaths {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		unique = append(unique, path)
	}
	if len(unique) == 0 {
		return 0, nil
	}
	deleted := 0
	const batchSize = 100
	for offset := 0; offset < len(unique); offset += batchSize {
		end := min(offset+batchSize, len(unique))
		batch := unique[offset:end]
		args := make([]any, 0, len(batch))
		for _, path := range batch {
			args = append(args, path)
		}
		result, err := s.db.Exec(
			"delete from agent_history_cache where source_path in ("+placeholders(len(batch))+")",
			args...,
		)
		if err != nil {
			return deleted, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += int(affected)
	}
	return deleted, nil
}

// agentHistoryCacheRow is one stored row of agent_history_cache.
type agentHistoryCacheRow struct {
	compactHistoryJSON string
	formatterVersion   string
	historyRefJSON     string
	id                 int64
	sourceMtimeMs      int64
	sourcePath         string
	sourceSize         int64
	updatedAtMs        int64
}

func scanAgentHistoryCache(row *sql.Row) (agentHistoryCacheRow, bool, error) {
	var stored agentHistoryCacheRow
	if err := row.Scan(
		&stored.compactHistoryJSON,
		&stored.formatterVersion,
		&stored.historyRefJSON,
		&stored.id,
		&stored.sourceMtimeMs,
		&stored.sourcePath,
		&stored.sourceSize,
		&stored.updatedAtMs,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentHistoryCacheRow{}, false, nil
		}
		return agentHistoryCacheRow{}, false, err
	}
	return stored, true, nil
}

func mapAgentHistoryCache(
	row agentHistoryCacheRow,
) (AgentHistoryCacheRecord, error) {
	compactHistory, err := parseCachedCompactHistory(row.compactHistoryJSON)
	if err != nil {
		return AgentHistoryCacheRecord{}, err
	}
	historyRef, err := parseCachedHistoryRef(row.historyRefJSON)
	if err != nil {
		return AgentHistoryCacheRecord{}, err
	}
	return AgentHistoryCacheRecord{
		CompactHistory:   compactHistory,
		FormatterVersion: row.formatterVersion,
		HistoryRef:       historyRef,
		ID:               row.id,
		SourceMtimeMs:    float64(row.sourceMtimeMs),
		SourcePath:       row.sourcePath,
		SourceSize:       row.sourceSize,
		UpdatedAt:        formatTimestamp(row.updatedAtMs),
	}, nil
}

func parseCachedCompactHistory(value string) (agenthistory.CompactAgentHistory, error) {
	var compactHistory agenthistory.CompactAgentHistory
	if err := json.Unmarshal([]byte(value), &compactHistory); err != nil {
		return agenthistory.CompactAgentHistory{},
			fmt.Errorf("stored agent history cache compact history is invalid: %w", err)
	}
	return compactHistory, nil
}

func parseCachedHistoryRef(value string) (agenthistory.HistoryRef, error) {
	var historyRef agenthistory.HistoryRef
	if err := json.Unmarshal([]byte(value), &historyRef); err != nil {
		return agenthistory.HistoryRef{},
			fmt.Errorf("stored agent history cache history ref is invalid: %w", err)
	}
	return historyRef, nil
}
