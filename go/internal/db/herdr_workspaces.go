package db

import (
	"database/sql"
	"strings"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// HerdrWorkspaceLike mirrors HerdrWorkspaceLike: one untyped workspace entry of
// a Herdr session snapshot, as it arrives from the Herdr client.
type HerdrWorkspaceLike map[string]any

// ReplaceForSessionInput mirrors the replaceForSession input.
type ReplaceForSessionInput struct {
	HerdrSessionName string
	Workspaces       []HerdrWorkspaceLike
}

// HerdrWorkspaceStore ports src/db/herdr-workspaces.ts.
type HerdrWorkspaceStore struct {
	db *sql.DB
}

// NewHerdrWorkspaceStore builds a store on the shared connection.
func NewHerdrWorkspaceStore(db *sql.DB) *HerdrWorkspaceStore {
	return &HerdrWorkspaceStore{db: db}
}

// herdrWorkspaceColumns is the explicit `select *` column list of
// herdr_workspaces.
const herdrWorkspaceColumns = "agent_status, focused, herdr_session_name, label, last_seen_at, workspace_id"

// ReplaceForSession mirrors replaceForSession: the session's workspaces are
// replaced wholesale, and the stored records are returned. A workspace without
// a usable id (workspace_id, else id) is skipped, exactly like the TS filter.
func (s *HerdrWorkspaceStore) ReplaceForSession(
	input ReplaceForSessionInput,
) ([]observability.HerdrWorkspaceRecord, error) {
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(
		"delete from herdr_workspaces where herdr_session_name = ?", input.HerdrSessionName,
	); err != nil {
		return nil, err
	}
	for _, workspace := range input.Workspaces {
		workspaceID := stringValue(workspace["workspace_id"])
		if workspaceID == nil {
			workspaceID = stringValue(workspace["id"])
		}
		if workspaceID == nil {
			continue
		}
		label := stringValue(workspace["label"])
		if label == nil {
			label = stringValue(workspace["name"])
		}
		focused := 0
		if booleanValue(workspace["focused"]) {
			focused = 1
		}
		if _, err := s.db.Exec(
			`insert into herdr_workspaces
			 (herdr_session_name, workspace_id, label, focused, agent_status, last_seen_at)
			 values (?, ?, ?, ?, ?, ?)`,
			input.HerdrSessionName, *workspaceID, label, focused,
			workspaceAgentStatus(workspace["agent_status"]), now,
		); err != nil {
			return nil, err
		}
	}
	return s.ListForSession(input.HerdrSessionName)
}

// ListForSession mirrors listForSession.
func (s *HerdrWorkspaceStore) ListForSession(
	herdrSessionName string,
) ([]observability.HerdrWorkspaceRecord, error) {
	return scanHerdrWorkspaces(s.db.Query(
		"select "+herdrWorkspaceColumns+
			" from herdr_workspaces where herdr_session_name = ? order by workspace_id",
		herdrSessionName,
	))
}

// List mirrors list.
func (s *HerdrWorkspaceStore) List() ([]observability.HerdrWorkspaceRecord, error) {
	return scanHerdrWorkspaces(s.db.Query(
		"select " + herdrWorkspaceColumns +
			" from herdr_workspaces order by herdr_session_name, workspace_id",
	))
}

func scanHerdrWorkspaces(rows *sql.Rows, err error) ([]observability.HerdrWorkspaceRecord, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := []observability.HerdrWorkspaceRecord{}
	for rows.Next() {
		var (
			agentStatus      string
			focused          int64
			herdrSessionName string
			label            sql.NullString
			lastSeenAt       int64
			workspaceID      string
		)
		if err := rows.Scan(&agentStatus, &focused, &herdrSessionName, &label, &lastSeenAt, &workspaceID); err != nil {
			return nil, err
		}
		records = append(records, observability.HerdrWorkspaceRecord{
			AgentStatus:      observability.AgentStatus(agentStatus),
			Focused:          focused != 0,
			HerdrSessionName: herdrSessionName,
			Label:            nullableString(label),
			LastSeenAt:       formatTimestamp(lastSeenAt),
			WorkspaceID:      workspaceID,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// stringValue mirrors stringValue: only a non-empty string survives, anything
// else is absent.
func stringValue(value any) *string {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	return &text
}

// booleanValue mirrors booleanValue: only a literal true counts.
func booleanValue(value any) bool {
	return value == true
}

// workspaceAgentStatus mirrors the parseAgentStatus call on the untyped
// workspace field: a non-string value collapses to "unknown".
func workspaceAgentStatus(value any) observability.AgentStatus {
	text, ok := value.(string)
	if !ok {
		return observability.AgentStatusUnknown
	}
	return observability.ParseAgentStatus(text)
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

// placeholders renders `?, ?, ...` for an IN list of the given size.
func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", count), ", ")
}
