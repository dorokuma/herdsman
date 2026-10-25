package db

import (
	"database/sql"
	"errors"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// AgentOrchestratorScopeKey mirrors the TS alias: a scope is addressed by its
// Herdr session and workspace.
type AgentOrchestratorScopeKey = observability.AgentScope

// ClaimOrchestratorInput and the other orchestrator scope inputs live in
// internal/observability (which internal/db imports, and which cannot import
// internal/db), so the service and the store share one type.
type ClaimOrchestratorInput = observability.ClaimOrchestratorInput

type ReleaseIfOwnerInput = observability.ReleaseIfOwnerInput

type AckInput = observability.AckInput

type MoveOwnerInput = observability.MoveOwnerInput

type AgentOrchestratorScopeChange = observability.AgentOrchestratorScopeChange

type AgentOrchestratorReleaseResult = observability.AgentOrchestratorReleaseResult

// ReleaseIfOwnerIdentityInput mirrors the releaseIfOwnerIdentity input.
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

// AgentOrchestratorScopeStore ports src/db/agent-orchestrator-scopes.ts.
type AgentOrchestratorScopeStore struct {
	db *sql.DB
}

// NewAgentOrchestratorScopeStore builds a store on the shared connection.
func NewAgentOrchestratorScopeStore(db *sql.DB) *AgentOrchestratorScopeStore {
	return &AgentOrchestratorScopeStore{db: db}
}

// scopeColumns is the explicit `select *` column list of agent_orchestrator_scopes.
const scopeColumns = "acked_event_id, created_at, herdr_session_name, owner_pane_id," +
	" owner_terminal_id, updated_at, workspace_id"

// Get mirrors get: the scope state, or nil when the scope was never claimed.
func (s *AgentOrchestratorScopeStore) Get(
	scope AgentOrchestratorScopeKey,
) (*observability.AgentOrchestratorState, error) {
	row, ok, err := s.getRow(scope)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	state, err := mapScope(row)
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// ListOwned mirrors listOwned.
func (s *AgentOrchestratorScopeStore) ListOwned() ([]observability.AgentOrchestratorState, error) {
	return scanScopes(s.db.Query(
		"select " + scopeColumns +
			" from agent_orchestrator_scopes where owner_terminal_id is not null" +
			" order by herdr_session_name, workspace_id",
	))
}

// ListOwnedForSession mirrors listOwnedForSession.
func (s *AgentOrchestratorScopeStore) ListOwnedForSession(
	herdrSessionName string,
) ([]observability.AgentOrchestratorState, error) {
	return scanScopes(s.db.Query(
		"select "+scopeColumns+
			" from agent_orchestrator_scopes where herdr_session_name = ? and owner_terminal_id is not null",
		herdrSessionName,
	))
}

// ListOwnedScopes mirrors listOwnedScopes.
func (s *AgentOrchestratorScopeStore) ListOwnedScopes() ([]observability.AgentOrchestratorState, error) {
	return scanScopes(s.db.Query(
		"select " + scopeColumns + " from agent_orchestrator_scopes where owner_terminal_id is not null",
	))
}

// PurgeReleasedOlderThan mirrors purgeReleasedOlderThan: released (ownerless)
// scope rows whose last update is older than the given age are physically
// removed; owned and recently released rows are kept.
func (s *AgentOrchestratorScopeStore) PurgeReleasedOlderThan(ageMs int64) (int, error) {
	result, err := s.db.Exec(
		"delete from agent_orchestrator_scopes where owner_terminal_id is null and updated_at < ?",
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

// ReleaseIfStaleOwner mirrors releaseIfStaleOwner: an owner whose pane and
// terminal are both gone from the scan is released, and whether the release
// happened is returned.
func (s *AgentOrchestratorScopeStore) ReleaseIfStaleOwner(
	input ReleaseIfStaleOwnerInput,
) (bool, error) {
	var released bool
	err := RunSqliteTransaction(s.db, func() error {
		row, ok, err := s.getRow(input.AgentOrchestratorScopeKey)
		if err != nil {
			return err
		}
		if !ok || !row.ownerPaneID.Valid || !row.ownerTerminalID.Valid {
			return nil
		}
		_, paneLive := input.LivePaneIDs[row.ownerPaneID.String]
		_, terminalLive := input.LiveTerminalIDs[row.ownerTerminalID.String]
		if paneLive && terminalLive {
			return nil
		}
		result, err := s.db.Exec(
			`update agent_orchestrator_scopes
			 set owner_pane_id = null, owner_terminal_id = null, updated_at = ?
			 where herdr_session_name = ? and workspace_id = ?
			   and owner_pane_id = ? and owner_terminal_id = ?`,
			time.Now().UnixMilli(), input.HerdrSessionName, input.WorkspaceID,
			row.ownerPaneID.String, row.ownerTerminalID.String,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		released = affected > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return released, nil
}

// Claim mirrors claim: the scope is created or re-owned, its cursor set to the
// authoritative value the caller supplies.
func (s *AgentOrchestratorScopeStore) Claim(
	input ClaimOrchestratorInput,
) (AgentOrchestratorScopeChange, error) {
	var change AgentOrchestratorScopeChange
	err := RunSqliteTransaction(s.db, func() error {
		var err error
		change, err = s.claim(input, false)
		return err
	})
	if err != nil {
		return AgentOrchestratorScopeChange{}, err
	}
	return change, nil
}

// ReleaseIfOwnerIdentity mirrors releaseIfOwnerIdentity: only the owner whose
// pane and terminal both match releases the scope.
func (s *AgentOrchestratorScopeStore) ReleaseIfOwnerIdentity(
	input ReleaseIfOwnerIdentityInput,
) (AgentOrchestratorReleaseResult, error) {
	var result AgentOrchestratorReleaseResult
	err := RunSqliteTransaction(s.db, func() error {
		row, ok, err := s.getRow(input.AgentOrchestratorScopeKey)
		if err != nil {
			return err
		}
		if !ok || !row.ownerTerminalID.Valid || row.ownerTerminalID.String != input.TerminalID ||
			!row.ownerPaneID.Valid || row.ownerPaneID.String != input.PaneID {
			if ok {
				current, err := mapScope(row)
				if err != nil {
					return err
				}
				result.Current = &current
			}
			return nil
		}
		previous, err := mapScope(row)
		if err != nil {
			return err
		}
		if err := s.releaseOwners(input.AgentOrchestratorScopeKey); err != nil {
			return err
		}
		current, err := s.Get(input.AgentOrchestratorScopeKey)
		if err != nil {
			return err
		}
		if current == nil {
			return errors.New("Orchestrator scope disappeared during pane close")
		}
		result.Changed = true
		result.Current = current
		result.Previous = &previous
		return nil
	})
	if err != nil {
		return AgentOrchestratorReleaseResult{}, err
	}
	return result, nil
}

// ReleaseIfOwner mirrors releaseIfOwner: only the owner terminal releases the
// scope, and the cursor is preserved.
func (s *AgentOrchestratorScopeStore) ReleaseIfOwner(
	input ReleaseIfOwnerInput,
) (AgentOrchestratorReleaseResult, error) {
	var result AgentOrchestratorReleaseResult
	err := RunSqliteTransaction(s.db, func() error {
		row, ok, err := s.getRow(input.AgentOrchestratorScopeKey)
		if err != nil {
			return err
		}
		if !ok || !row.ownerTerminalID.Valid || row.ownerTerminalID.String != input.TerminalID {
			if ok {
				current, err := mapScope(row)
				if err != nil {
					return err
				}
				result.Current = &current
			}
			return nil
		}
		previous, err := mapScope(row)
		if err != nil {
			return err
		}
		if err := s.releaseOwners(input.AgentOrchestratorScopeKey); err != nil {
			return err
		}
		current, err := s.Get(input.AgentOrchestratorScopeKey)
		if err != nil {
			return err
		}
		if current == nil {
			return errors.New("Orchestrator scope disappeared during release")
		}
		result.Changed = true
		result.Current = current
		result.Previous = &previous
		return nil
	})
	if err != nil {
		return AgentOrchestratorReleaseResult{}, err
	}
	return result, nil
}

// MoveOwner mirrors moveOwner: the source scope is released and the target is
// claimed with the authoritative cursor, atomically.
func (s *AgentOrchestratorScopeStore) MoveOwner(
	input MoveOwnerInput,
) ([]AgentOrchestratorScopeChange, error) {
	var changes []AgentOrchestratorScopeChange
	err := RunSqliteTransaction(s.db, func() error {
		sourceRow, ok, err := s.getRow(input.From)
		if err != nil {
			return err
		}
		if !ok || !sourceRow.ownerTerminalID.Valid ||
			sourceRow.ownerTerminalID.String != input.TerminalID {
			return errors.New("Only the current orchestrator can move ownership")
		}
		sourcePrevious, err := mapScope(sourceRow)
		if err != nil {
			return err
		}
		if err := s.releaseOwners(input.From); err != nil {
			return err
		}
		sourceCurrent, err := s.Get(input.From)
		if err != nil {
			return err
		}
		if sourceCurrent == nil {
			return errors.New("Orchestrator source disappeared during move")
		}
		target, err := s.claim(ClaimOrchestratorInput{
			AgentOrchestratorScopeKey: input.To,
			AckedEventID:              input.TargetAckedEventID,
			PaneID:                    input.PaneID,
			TerminalID:                input.TerminalID,
		}, true)
		if err != nil {
			return err
		}
		changes = []AgentOrchestratorScopeChange{
			{Current: *sourceCurrent, Previous: sourcePrevious},
			target,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changes, nil
}

// Ack mirrors ack: the cursor moves forward to the given event, never back, and
// only the current owner may acknowledge.
func (s *AgentOrchestratorScopeStore) Ack(
	input AckInput,
) (observability.AgentOrchestratorState, error) {
	var current observability.AgentOrchestratorState
	err := RunSqliteTransaction(s.db, func() error {
		row, ok, err := s.getRow(input.AgentOrchestratorScopeKey)
		if err != nil {
			return err
		}
		if !ok || !row.ownerTerminalID.Valid || row.ownerTerminalID.String != input.TerminalID {
			return errors.New("Only the current orchestrator can acknowledge notifications")
		}
		if _, err := s.db.Exec(
			`update agent_orchestrator_scopes
			 set acked_event_id = max(acked_event_id, ?), updated_at = ?
			 where herdr_session_name = ? and workspace_id = ?`,
			input.EventID, time.Now().UnixMilli(), input.HerdrSessionName, input.WorkspaceID,
		); err != nil {
			return err
		}
		state, err := s.Get(input.AgentOrchestratorScopeKey)
		if err != nil {
			return err
		}
		if state == nil {
			return errors.New("Orchestrator scope disappeared during acknowledgement")
		}
		current = *state
		return nil
	})
	if err != nil {
		return observability.AgentOrchestratorState{}, err
	}
	return current, nil
}

// claim mirrors #claim: a missing scope row is created, an existing one is
// re-owned. Without allowOwnerReplacement a claim from a connected terminal
// over an ownerless-by-another-terminal scope is refused.
func (s *AgentOrchestratorScopeStore) claim(
	input ClaimOrchestratorInput,
	allowOwnerReplacement bool,
) (AgentOrchestratorScopeChange, error) {
	row, ok, err := s.getRow(input.AgentOrchestratorScopeKey)
	if err != nil {
		return AgentOrchestratorScopeChange{}, err
	}
	now := time.Now().UnixMilli()
	if !ok {
		if _, err := s.db.Exec(
			`insert into agent_orchestrator_scopes
			 (herdr_session_name, workspace_id, acked_event_id, owner_pane_id, owner_terminal_id, created_at, updated_at)
			 values (?, ?, ?, ?, ?, ?, ?)`,
			input.HerdrSessionName, input.WorkspaceID, input.AckedEventID,
			input.PaneID, input.TerminalID, now, now,
		); err != nil {
			return AgentOrchestratorScopeChange{}, err
		}
		current, err := s.Get(input.AgentOrchestratorScopeKey)
		if err != nil {
			return AgentOrchestratorScopeChange{}, err
		}
		if current == nil {
			return AgentOrchestratorScopeChange{}, errors.New("Failed to create orchestrator scope")
		}
		previous := *current
		previous.Owner = nil
		return AgentOrchestratorScopeChange{Current: *current, Previous: previous}, nil
	}
	previous, err := mapScope(row)
	if err != nil {
		return AgentOrchestratorScopeChange{}, err
	}
	if !allowOwnerReplacement && row.ownerTerminalID.Valid &&
		row.ownerTerminalID.String != input.TerminalID &&
		input.OwnerConnected != nil && *input.OwnerConnected {
		return AgentOrchestratorScopeChange{}, errors.New(
			"ORCHESTRATOR_SCOPE_ALREADY_CLAIMED: scope is owned by another terminal")
	}
	if _, err := s.db.Exec(
		`update agent_orchestrator_scopes
		 set acked_event_id = ?, owner_pane_id = ?, owner_terminal_id = ?, updated_at = ?
		 where herdr_session_name = ? and workspace_id = ?`,
		input.AckedEventID, input.PaneID, input.TerminalID, now,
		input.HerdrSessionName, input.WorkspaceID,
	); err != nil {
		return AgentOrchestratorScopeChange{}, err
	}
	current, err := s.Get(input.AgentOrchestratorScopeKey)
	if err != nil {
		return AgentOrchestratorScopeChange{}, err
	}
	if current == nil {
		return AgentOrchestratorScopeChange{}, errors.New(
			"Orchestrator scope disappeared during claim")
	}
	return AgentOrchestratorScopeChange{Current: *current, Previous: previous}, nil
}

// releaseOwners clears the owner columns of one scope, mirroring the release
// updates of the TS operations.
func (s *AgentOrchestratorScopeStore) releaseOwners(scope AgentOrchestratorScopeKey) error {
	_, err := s.db.Exec(
		`update agent_orchestrator_scopes
		 set owner_pane_id = null, owner_terminal_id = null, updated_at = ?
		 where herdr_session_name = ? and workspace_id = ?`,
		time.Now().UnixMilli(), scope.HerdrSessionName, scope.WorkspaceID,
	)
	return err
}

// scopeRow is one stored row of agent_orchestrator_scopes.
type scopeRow struct {
	ackedEventID     int64
	createdAt        int64
	herdrSessionName string
	ownerPaneID      sql.NullString
	ownerTerminalID  sql.NullString
	updatedAt        int64
	workspaceID      string
}

func (s *AgentOrchestratorScopeStore) getRow(
	scope AgentOrchestratorScopeKey,
) (scopeRow, bool, error) {
	var row scopeRow
	if err := s.db.QueryRow(
		"select "+scopeColumns+
			" from agent_orchestrator_scopes where herdr_session_name = ? and workspace_id = ?",
		scope.HerdrSessionName, scope.WorkspaceID,
	).Scan(
		&row.ackedEventID,
		&row.createdAt,
		&row.herdrSessionName,
		&row.ownerPaneID,
		&row.ownerTerminalID,
		&row.updatedAt,
		&row.workspaceID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return scopeRow{}, false, nil
		}
		return scopeRow{}, false, err
	}
	return row, true, nil
}

func scanScopes(rows *sql.Rows, err error) ([]observability.AgentOrchestratorState, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	states := []observability.AgentOrchestratorState{}
	for rows.Next() {
		var row scopeRow
		if err := rows.Scan(
			&row.ackedEventID,
			&row.createdAt,
			&row.herdrSessionName,
			&row.ownerPaneID,
			&row.ownerTerminalID,
			&row.updatedAt,
			&row.workspaceID,
		); err != nil {
			return nil, err
		}
		state, err := mapScope(row)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return states, nil
}

// mapScope mirrors mapScope: the owner columns are either both set or both
// absent.
func mapScope(row scopeRow) (observability.AgentOrchestratorState, error) {
	if row.ownerPaneID.Valid != row.ownerTerminalID.Valid {
		return observability.AgentOrchestratorState{},
			errors.New("Corrupt orchestrator scope owner identity")
	}
	var owner *observability.AgentOrchestratorOwner
	if row.ownerPaneID.Valid && row.ownerTerminalID.Valid {
		owner = &observability.AgentOrchestratorOwner{
			PaneID:     row.ownerPaneID.String,
			TerminalID: row.ownerTerminalID.String,
		}
	}
	return observability.AgentOrchestratorState{
		AgentScope: observability.AgentScope{
			HerdrSessionName: row.herdrSessionName,
			WorkspaceID:      row.workspaceID,
		},
		AckedEventID: row.ackedEventID,
		Owner:        owner,
		UpdatedAt:    formatTimestamp(row.updatedAt),
	}, nil
}
