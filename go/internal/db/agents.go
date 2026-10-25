package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// HerdrAgentLike mirrors the TS alias of the same name: one entry of the
// snake_case `agent.list` / `session.snapshot` payload Herdr reports.
type HerdrAgentLike = map[string]any

// AgentStore ports src/db/agents.ts.
type AgentStore struct {
	db          *sql.DB
	agentEvents *AgentEventStore

	// terminalTitleMu guards the in-memory terminal-title cache, which mirrors
	// the TS #terminalTitleById map: titles are reported by Herdr only in the
	// scan payload, so the store keeps the last one it saw per agent id.
	terminalTitleMu sync.Mutex
	terminalTitles  map[string]string
}

// NewAgentStore builds a store on the shared connection. agentEvents is the
// AgentEventStore the pane-retirement path invalidates through, exactly like the
// TS constructor's second argument.
func NewAgentStore(db *sql.DB, agentEvents *AgentEventStore) *AgentStore {
	return &AgentStore{
		db:             db,
		agentEvents:    agentEvents,
		terminalTitles: map[string]string{},
	}
}

// agentColumns is the explicit `select *` column list of agents, in the order
// the ALTER TABLE migrations append the later columns.
const agentColumns = "agent, agent_session_hint_json, agent_session_json, agent_status, cwd," +
	" first_seen_at, focused, foreground_cwd, herdr_session_name, id, last_seen_at, name," +
	" pane_id, pane_revision, pane_generation, grok_home, tab_id, terminal_id, workspace_id"

// ReplaceAgentsForSessionInput mirrors the TS replaceForSession input.
type ReplaceAgentsForSessionInput struct {
	Agents           []HerdrAgentLike
	HerdrSessionName string
}

// RetirePaneInput mirrors the TS retirePane / markPaneClosed input.
type RetirePaneInput struct {
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
	AgentSession     observability.AgentSessionRef
	HerdrSessionName string
	TerminalID       string
}

// UpdateAgentStatusInput mirrors the TS updateStatus input.
type UpdateAgentStatusInput struct {
	AgentStatus      observability.AgentStatus
	HerdrSessionName string
	PaneGeneration   *string
	PaneID           string
}

// ReplaceForSession mirrors replaceForSession: the scan snapshot becomes the
// authoritative agent index for one Herdr session. Stable terminals keep their
// agent id across pane and workspace moves, and rows whose agent disappeared are
// retired together with their events.
func (s *AgentStore) ReplaceForSession(
	input ReplaceAgentsForSessionInput,
) ([]observability.AgentIndexRecord, error) {
	now := time.Now().UnixMilli()
	// `agent.list` / `session.snapshot` are the base metadata source, and Herdr
	// (0.9.3, "herdr api schema --json" -> `AgentInfo`) emits snake_case only.
	// Fields Herdr does not report (e.g. `pane_generation`, `env`, `pid`) keep
	// their local fallbacks below.
	type agentSnapshot struct {
		agent       HerdrAgentLike
		paneID      string
		terminalID  *string
		workspaceID string
	}
	snapshots := []agentSnapshot{}
	for _, agent := range input.Agents {
		paneID := stringValue(agent["pane_id"])
		workspaceID := stringValue(agent["workspace_id"])
		if paneID == nil || workspaceID == nil {
			continue
		}
		snapshots = append(snapshots, agentSnapshot{
			agent:       agent,
			paneID:      *paneID,
			terminalID:  stringValue(agent["terminal_id"]),
			workspaceID: *workspaceID,
		})
	}

	var replaced []observability.AgentIndexRecord
	err := RunSqliteTransaction(s.db, func() error {
		existing, err := scanAgentRows(s.db.Query(
			"select "+agentColumns+" from agents where herdr_session_name = ?",
			input.HerdrSessionName,
		))
		if err != nil {
			return err
		}
		byPane := map[string]agentRow{}
		for _, row := range existing {
			byPane[row.paneID] = row
		}
		byTerminal := map[string]agentRow{}
		for _, row := range existing {
			if row.terminalID.Valid {
				byTerminal[row.terminalID.String] = row
			}
		}
		type matchedAgent struct {
			current  *agentRow
			snapshot agentSnapshot
		}
		matched := make([]matchedAgent, 0, len(snapshots))
		for _, snapshot := range snapshots {
			var terminalMatch *agentRow
			if snapshot.terminalID != nil {
				if row, ok := byTerminal[*snapshot.terminalID]; ok {
					terminalMatch = &row
				}
			}
			var paneMatch *agentRow
			if row, ok := byPane[snapshot.paneID]; ok {
				paneMatch = &row
			}
			incomingGeneration := paneGeneration(snapshot.agent)
			canUsePaneFallback := paneMatch != nil &&
				(snapshot.terminalID == nil || !paneMatch.terminalID.Valid)
			generationMatches := incomingGeneration == nil ||
				paneMatch == nil || !paneMatch.paneGeneration.Valid ||
				*incomingGeneration == paneMatch.paneGeneration.String
			var current *agentRow
			if terminalMatch != nil {
				current = terminalMatch
			} else if generationMatches && canUsePaneFallback {
				current = paneMatch
			}
			matched = append(matched, matchedAgent{current: current, snapshot: snapshot})
		}

		temporaryPaneIDs := map[string]struct{}{}
		temporaryPaneOrder := []string{}
		markTemporary := func(id string) {
			if _, seen := temporaryPaneIDs[id]; seen {
				return
			}
			temporaryPaneIDs[id] = struct{}{}
			temporaryPaneOrder = append(temporaryPaneOrder, id)
		}
		for _, entry := range matched {
			if entry.current != nil && entry.current.paneID != entry.snapshot.paneID {
				markTemporary(entry.current.id)
			}
			if occupant, ok := byPane[entry.snapshot.paneID]; ok {
				if entry.current == nil || occupant.id != entry.current.id {
					markTemporary(occupant.id)
				}
			}
		}
		for _, id := range temporaryPaneOrder {
			if _, err := s.db.Exec(
				"update agents set pane_id = ? where id = ?", "__herdsman_moving__:"+id, id,
			); err != nil {
				return err
			}
		}

		retainedIDs := make([]string, 0, len(matched))
		for _, entry := range matched {
			id := "ag_" + newUUID()
			if entry.current != nil {
				id = entry.current.id
			}
			retainedIDs = append(retainedIDs, id)
			agent := stringValue(entry.snapshot.agent["agent"])
			name := stringValue(entry.snapshot.agent["name"])
			terminalTitle := stringValue(entry.snapshot.agent["terminal_title"])
			s.setTerminalTitle(id, terminalTitle)
			var sessionHint sql.NullString
			if entry.current != nil && sameNullableString(
				nullableString(entry.current.agent), agent,
			) {
				sessionHint = entry.current.agentSessionHintJSON
			}
			// `grok_home` is a legacy column kept so the schema does not need a
			// migration; no reader and no discovery path reads it any more (grok
			// is not a supported history agent), so it is written as a constant
			// null.
			paneGenerationValue := paneGeneration(entry.snapshot.agent)
			if paneGenerationValue == nil && entry.current != nil {
				paneGenerationValue = nullableString(entry.current.paneGeneration)
			}
			values := []any{
				entry.snapshot.paneID,
				entry.snapshot.terminalID,
				stringValue(entry.snapshot.agent["tab_id"]),
				entry.snapshot.workspaceID,
				agent,
				name,
				string(observability.ParseAgentStatus(stringValueOr(
					entry.snapshot.agent["agent_status"], observability.AgentStatusUnknown,
				))),
				agentSessionJSON(entry.snapshot.agent["agent_session"]),
				sessionHint,
				integerValue(entry.snapshot.agent["revision"]),
				paneGenerationValue,
				nil,
				stringValue(entry.snapshot.agent["cwd"]),
				stringValue(entry.snapshot.agent["foreground_cwd"]),
				entry.snapshot.agent["focused"] == true,
				now,
			}
			if entry.current != nil {
				if _, err := s.db.Exec(
					`update agents
					 set pane_id = ?, terminal_id = ?, tab_id = ?, workspace_id = ?, agent = ?, name = ?,
					     agent_status = ?, agent_session_json = ?, agent_session_hint_json = ?, pane_revision = ?, pane_generation = ?, grok_home = ?,
					     cwd = ?, foreground_cwd = ?, focused = ?, last_seen_at = ?
					 where id = ?`,
					append(values, id)...,
				); err != nil {
					return err
				}
				continue
			}
			if _, err := s.db.Exec(
				`insert into agents
				 (id, herdr_session_name, pane_id, terminal_id, tab_id, workspace_id, agent, name, agent_status, agent_session_json, agent_session_hint_json, pane_revision, pane_generation, grok_home, cwd, foreground_cwd, focused, first_seen_at, last_seen_at)
				 values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				append([]any{id, input.HerdrSessionName}, append(values, now)...)...,
			); err != nil {
				return err
			}
		}

		// A live snapshot that carries a pane_generation proves the pane is open
		// again: drop any generation-less (legacy) close tombstone for that pane
		// so it can no longer block this pane. Generation-scoped tombstones stay.
		for _, entry := range matched {
			if paneGeneration(entry.snapshot.agent) != nil {
				if err := s.ClearLegacyPaneTombstone(ClearLegacyPaneTombstoneInput{
					HerdrSessionName: input.HerdrSessionName,
					PaneID:           entry.snapshot.paneID,
				}); err != nil {
					return err
				}
			}
		}

		retained := map[string]struct{}{}
		for _, id := range retainedIDs {
			retained[id] = struct{}{}
		}
		removedIDs := []string{}
		for _, row := range existing {
			if _, kept := retained[row.id]; !kept {
				removedIDs = append(removedIDs, row.id)
			}
		}
		for _, id := range removedIDs {
			s.deleteTerminalTitle(id)
		}
		for _, row := range existing {
			if _, removed := retained[row.id]; removed {
				continue
			}
			// The invalidation runs before the agents rows are deleted, so the
			// event rows still see the agent they belong to.
			if err := s.agentEvents.InvalidatePaneDirect(InvalidatePaneInput{
				HerdrSessionName: input.HerdrSessionName,
				PaneGeneration:   nullableString(row.paneGeneration),
				PaneID:           row.paneID,
			}); err != nil {
				return err
			}
		}
		if len(removedIDs) > 0 {
			if _, err := s.db.Exec(
				"delete from agent_context_snapshots where agent_id in ("+
					placeholders(len(removedIDs))+")",
				toAnySlice(removedIDs)...,
			); err != nil {
				return err
			}
		}
		if len(retainedIDs) == 0 {
			if _, err := s.db.Exec(
				"delete from agents where herdr_session_name = ?", input.HerdrSessionName,
			); err != nil {
				return err
			}
		} else {
			if _, err := s.db.Exec(
				"delete from agents where herdr_session_name = ? and id not in ("+
					placeholders(len(retainedIDs))+")",
				append([]any{input.HerdrSessionName}, toAnySlice(retainedIDs)...)...,
			); err != nil {
				return err
			}
		}
		replaced, err = s.ListForHerdrSession(input.HerdrSessionName)
		return err
	})
	if err != nil {
		return nil, err
	}
	return replaced, nil
}

// RetirePane mirrors retirePane: the pane is tombstoned, its agents and context
// snapshots are removed, and the removed records are returned.
func (s *AgentStore) RetirePane(input RetirePaneInput) ([]observability.AgentIndexRecord, error) {
	var retired []observability.AgentIndexRecord
	err := RunSqliteTransaction(s.db, func() error {
		if err := s.MarkPaneClosed(RetirePaneInput{
			HerdrSessionName: input.HerdrSessionName,
			PaneGeneration:   input.PaneGeneration,
			PaneID:           input.PaneID,
		}); err != nil {
			return err
		}
		generationClause, generationParams := agentGenerationClause(input.PaneGeneration)
		agents, err := scanAgentRows(s.db.Query(
			"select "+agentColumns+" from agents where herdr_session_name = ? and pane_id = ?"+
				generationClause,
			append([]any{input.HerdrSessionName, input.PaneID}, generationParams...)...,
		))
		if err != nil {
			return err
		}
		if len(agents) == 0 {
			retired = []observability.AgentIndexRecord{}
			return nil
		}
		ids := make([]string, 0, len(agents))
		for _, row := range agents {
			ids = append(ids, row.id)
			s.deleteTerminalTitle(row.id)
		}
		if _, err := s.db.Exec(
			"delete from agent_context_snapshots where agent_id in ("+
				placeholders(len(ids))+")",
			toAnySlice(ids)...,
		); err != nil {
			return err
		}
		if _, err := s.db.Exec(
			"delete from agents where herdr_session_name = ? and pane_id = ?"+generationClause,
			append([]any{input.HerdrSessionName, input.PaneID}, generationParams...)...,
		); err != nil {
			return err
		}
		retired = make([]observability.AgentIndexRecord, 0, len(agents))
		for _, row := range agents {
			retired = append(retired, s.record(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return retired, nil
}

// MarkPaneClosed mirrors markPaneClosed: it records a persistent "pane closed"
// tombstone so that any later path (stale refresh snapshots, late status events)
// can tell the pane has been retired and must not be resurrected.
func (s *AgentStore) MarkPaneClosed(input RetirePaneInput) error {
	if _, err := s.db.Exec(
		`delete from agent_pane_tombstones
		 where herdr_session_name = ? and pane_id = ? and pane_generation is ?`,
		input.HerdrSessionName, input.PaneID, input.PaneGeneration,
	); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`insert into agent_pane_tombstones
		 (herdr_session_name, pane_id, pane_generation, closed_at)
		 values (?, ?, ?, ?)`,
		input.HerdrSessionName, input.PaneID, input.PaneGeneration, time.Now().UnixMilli(),
	)
	return err
}

// IsPaneClosed mirrors isPaneClosed: true when the pane is recorded as closed. A
// generation-scoped tombstone matches only the same generation; a legacy
// tombstone (no generation, i.e. LEGACY_CLOSE_WITHOUT_GENERATION) matches any
// generation, mirroring AgentEventStore.InvalidatePane legacy semantics.
func (s *AgentStore) IsPaneClosed(input IsPaneClosedInput) (bool, error) {
	var marker int
	err := s.db.QueryRow(
		`select 1 from agent_pane_tombstones
		 where herdr_session_name = ? and pane_id = ?
		   and (pane_generation is ? or pane_generation is null)
		 limit 1`,
		input.HerdrSessionName, input.PaneID, input.PaneGeneration,
	).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// IsPaneClosedSince mirrors isPaneClosedSince: true when the pane is recorded as
// closed for the given generation, or when no generation was known at lease
// registration time and any tombstone for the pane was recorded at or after
// sinceMs.
func (s *AgentStore) IsPaneClosedSince(input IsPaneClosedInput) (bool, error) {
	if input.PaneGeneration != nil {
		return s.IsPaneClosed(IsPaneClosedInput{
			HerdrSessionName: input.HerdrSessionName,
			PaneGeneration:   input.PaneGeneration,
			PaneID:           input.PaneID,
		})
	}
	var marker int
	err := s.db.QueryRow(
		`select 1 from agent_pane_tombstones
		 where herdr_session_name = ? and pane_id = ?
		   and (pane_generation is null or closed_at >= ?)
		 limit 1`,
		input.HerdrSessionName, input.PaneID, input.SinceMs,
	).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ClearLegacyPaneTombstone mirrors clearLegacyPaneTombstone: a live pane that
// reports a pane_generation proves the pane is open again, so an earlier
// LEGACY_CLOSE_WITHOUT_GENERATION tombstone must not keep blocking it (active
// generation beats a generation-less close). Generation-scoped tombstones are
// untouched.
func (s *AgentStore) ClearLegacyPaneTombstone(input ClearLegacyPaneTombstoneInput) error {
	_, err := s.db.Exec(
		`delete from agent_pane_tombstones
		 where herdr_session_name = ? and pane_id = ? and pane_generation is null`,
		input.HerdrSessionName, input.PaneID,
	)
	return err
}

// HasGenerationScopedTombstone mirrors hasGenerationScopedTombstone: true when a
// generation-scoped tombstone exactly matching paneGeneration exists. Unlike
// IsPaneClosed this ignores legacy (generation-less) tombstones, so callers can
// tell "closed for this generation" apart from "closed without a generation".
func (s *AgentStore) HasGenerationScopedTombstone(
	input HasGenerationScopedTombstoneInput,
) (bool, error) {
	var marker int
	err := s.db.QueryRow(
		`select 1 from agent_pane_tombstones
		 where herdr_session_name = ? and pane_id = ? and pane_generation = ?
		 limit 1`,
		input.HerdrSessionName, input.PaneID, input.PaneGeneration,
	).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// SetSessionRefByTerminal mirrors setSessionRefByTerminal: a Pi session hint is
// stored only for a terminal whose indexed agent is the same agent kind.
func (s *AgentStore) SetSessionRefByTerminal(
	input SetSessionRefByTerminalInput,
) (*observability.AgentIndexRecord, error) {
	current, err := s.FindByTerminal(FindAgentByTerminalInput{
		HerdrSessionName: input.HerdrSessionName,
		TerminalID:       input.TerminalID,
	})
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, nil
	}
	compatible := current.Agent != nil &&
		strings.EqualFold(*current.Agent, input.AgentSession.Agent)
	var hintJSON any
	if compatible {
		encoded, err := json.Marshal(input.AgentSession)
		if err != nil {
			return nil, err
		}
		hintJSON = string(encoded)
	}
	if _, err := s.db.Exec(
		`update agents
		 set agent_session_hint_json = ?
		 where herdr_session_name = ? and terminal_id = ?`,
		hintJSON, input.HerdrSessionName, input.TerminalID,
	); err != nil {
		return nil, err
	}
	return s.FindByTerminal(FindAgentByTerminalInput{
		HerdrSessionName: input.HerdrSessionName,
		TerminalID:       input.TerminalID,
	})
}

// UpdateStatus mirrors updateStatus.
func (s *AgentStore) UpdateStatus(input UpdateAgentStatusInput) (*observability.AgentIndexRecord, error) {
	generationClause, generationParams := agentGenerationClause(input.PaneGeneration)
	if _, err := s.db.Exec(
		"update agents set agent_status = ?, last_seen_at = ?"+
			" where herdr_session_name = ? and pane_id = ?"+generationClause,
		append([]any{
			string(input.AgentStatus), time.Now().UnixMilli(),
			input.HerdrSessionName, input.PaneID,
		}, generationParams...)...,
	); err != nil {
		return nil, err
	}
	return s.FindByPane(FindAgentByPaneInput{
		HerdrSessionName: input.HerdrSessionName,
		PaneGeneration:   input.PaneGeneration,
		PaneID:           input.PaneID,
	})
}

// FindAgentByPaneInput mirrors the TS findByPane input.
type FindAgentByPaneInput struct {
	HerdrSessionName string
	PaneGeneration   *string
	PaneID           string
}

// FindAgentByTerminalInput mirrors the TS findByTerminal input.
type FindAgentByTerminalInput struct {
	HerdrSessionName string
	TerminalID       string
}

// List mirrors list: the agents of the running Herdr sessions, narrowed by the
// scope unless `all` is set.
func (s *AgentStore) List(scope observability.AgentQueryScope) ([]observability.AgentIndexRecord, error) {
	clauses := []string{"sessions.running = 1"}
	params := []any{}
	all := scope.All != nil && *scope.All
	if !all && scope.HerdrSessionName != nil && *scope.HerdrSessionName != "" {
		clauses = append(clauses, "agents.herdr_session_name = ?")
		params = append(params, *scope.HerdrSessionName)
	}
	if !all && scope.WorkspaceID != nil && *scope.WorkspaceID != "" {
		clauses = append(clauses, "agents.workspace_id = ?")
		params = append(params, *scope.WorkspaceID)
	}
	if all && scope.HerdrSessionName != nil && *scope.HerdrSessionName != "" {
		clauses = append(clauses, "agents.herdr_session_name = ?")
		params = append(params, *scope.HerdrSessionName)
	}
	return scanAgents(s.db.Query(
		`select agents.`+strings.ReplaceAll(agentColumns, ", ", ", agents.")+
			` from agents
		 inner join herdr_sessions as sessions
		   on sessions.name = agents.herdr_session_name
		 where `+strings.Join(clauses, " and ")+
			` order by agents.herdr_session_name, agents.workspace_id, agents.pane_id`,
		params...,
	))
}

// ListForHerdrSession mirrors listForHerdrSession.
func (s *AgentStore) ListForHerdrSession(
	herdrSessionName string,
) ([]observability.AgentIndexRecord, error) {
	return s.List(observability.AgentQueryScope{HerdrSessionName: &herdrSessionName})
}

// FindByPane mirrors findByPane.
func (s *AgentStore) FindByPane(input FindAgentByPaneInput) (*observability.AgentIndexRecord, error) {
	generationClause, generationParams := agentGenerationClause(input.PaneGeneration)
	row, ok, err := scanAgent(s.db.QueryRow(
		"select "+agentColumns+" from agents where herdr_session_name = ? and pane_id = ?"+
			generationClause,
		append([]any{input.HerdrSessionName, input.PaneID}, generationParams...)...,
	))
	if err != nil || !ok {
		return nil, err
	}
	record := s.record(row)
	return &record, nil
}

// FindByTerminal mirrors findByTerminal.
func (s *AgentStore) FindByTerminal(
	input FindAgentByTerminalInput,
) (*observability.AgentIndexRecord, error) {
	row, ok, err := scanAgent(s.db.QueryRow(
		"select "+agentColumns+" from agents where herdr_session_name = ? and terminal_id = ?",
		input.HerdrSessionName, input.TerminalID,
	))
	if err != nil || !ok {
		return nil, err
	}
	record := s.record(row)
	return &record, nil
}

// Get mirrors get: it fails when no agent with that id exists.
func (s *AgentStore) Get(id string) (observability.AgentIndexRecord, error) {
	row, ok, err := scanAgent(s.db.QueryRow(
		"select "+agentColumns+" from agents where id = ?", id,
	))
	if err != nil {
		return observability.AgentIndexRecord{}, err
	}
	if !ok {
		return observability.AgentIndexRecord{}, fmt.Errorf("Agent not found: %s", id)
	}
	return s.record(row), nil
}

// Exists mirrors exists: a non-throwing existence probe for callers that must
// decide whether dependent rows (e.g. `agent_context_snapshots`, which
// references `agents(id)`) can still be written for this agent.
func (s *AgentStore) Exists(id string) (bool, error) {
	var marker int
	err := s.db.QueryRow("select 1 as present from agents where id = ?", id).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ResolveTarget mirrors resolveTarget: an agent is addressed by pane, terminal
// or id first, then by name, then by agent kind. Exactly one candidate must
// match, otherwise the target is ambiguous or missing.
func (s *AgentStore) ResolveTarget(
	scope observability.AgentQueryScope,
	target string,
) (observability.AgentIndexRecord, error) {
	agents, err := s.List(scope)
	if err != nil {
		return observability.AgentIndexRecord{}, err
	}
	candidateGroups := [][]observability.AgentIndexRecord{
		filterAgents(agents, func(agent observability.AgentIndexRecord) bool {
			return agent.PaneID == target ||
				(agent.TerminalID != nil && *agent.TerminalID == target) ||
				agent.ID == target
		}),
		filterAgents(agents, func(agent observability.AgentIndexRecord) bool {
			return agent.Name != nil && *agent.Name == target
		}),
		filterAgents(agents, func(agent observability.AgentIndexRecord) bool {
			return agent.Agent != nil && *agent.Agent == target
		}),
	}
	for _, candidates := range candidateGroups {
		if len(candidates) == 1 {
			return candidates[0], nil
		}
		if len(candidates) > 1 {
			return observability.AgentIndexRecord{}, ambiguousTargetError(target, candidates)
		}
	}
	return observability.AgentIndexRecord{}, fmt.Errorf("agent target not found: %s", target)
}

func filterAgents(
	agents []observability.AgentIndexRecord,
	keep func(observability.AgentIndexRecord) bool,
) []observability.AgentIndexRecord {
	kept := []observability.AgentIndexRecord{}
	for _, agent := range agents {
		if keep(agent) {
			kept = append(kept, agent)
		}
	}
	return kept
}

// record mirrors #record: the mapped agent plus the terminal title the last scan
// reported for it, when there was one.
func (s *AgentStore) record(row agentRow) observability.AgentIndexRecord {
	mapped := mapAgent(row)
	if title, ok := s.terminalTitle(row.id); ok {
		mapped.TerminalTitle = &title
	}
	return mapped
}

func (s *AgentStore) terminalTitle(id string) (string, bool) {
	s.terminalTitleMu.Lock()
	defer s.terminalTitleMu.Unlock()
	title, ok := s.terminalTitles[id]
	return title, ok
}

func (s *AgentStore) setTerminalTitle(id string, title *string) {
	s.terminalTitleMu.Lock()
	defer s.terminalTitleMu.Unlock()
	if title == nil {
		delete(s.terminalTitles, id)
		return
	}
	s.terminalTitles[id] = *title
}

func (s *AgentStore) deleteTerminalTitle(id string) {
	s.terminalTitleMu.Lock()
	defer s.terminalTitleMu.Unlock()
	delete(s.terminalTitles, id)
}

// integerValue mirrors integerValue: only a non-negative integer counts.
func integerValue(value any) *int {
	number, ok := value.(float64)
	if !ok || number != float64(int(number)) || number < 0 {
		return nil
	}
	integer := int(number)
	return &integer
}

// stringValueOr renders an untyped field, falling back to the given default.
func stringValueOr(value any, fallback observability.AgentStatus) string {
	if text, ok := value.(string); ok {
		return text
	}
	return string(fallback)
}

// agentRow is one stored row of agents.
type agentRow struct {
	agent                sql.NullString
	agentSessionHintJSON sql.NullString
	agentSessionJSON     sql.NullString
	agentStatus          string
	cwd                  sql.NullString
	firstSeenAtMs        int64
	focused              int
	foregroundCwd        sql.NullString
	herdrSessionName     string
	id                   string
	lastSeenAtMs         int64
	name                 sql.NullString
	paneID               string
	paneRevision         sql.NullInt64
	paneGeneration       sql.NullString
	grokHome             sql.NullString
	tabID                sql.NullString
	terminalID           sql.NullString
	workspaceID          string
}

func scanAgent(row *sql.Row) (agentRow, bool, error) {
	var stored agentRow
	if err := row.Scan(
		&stored.agent,
		&stored.agentSessionHintJSON,
		&stored.agentSessionJSON,
		&stored.agentStatus,
		&stored.cwd,
		&stored.firstSeenAtMs,
		&stored.focused,
		&stored.foregroundCwd,
		&stored.herdrSessionName,
		&stored.id,
		&stored.lastSeenAtMs,
		&stored.name,
		&stored.paneID,
		&stored.paneRevision,
		&stored.paneGeneration,
		&stored.grokHome,
		&stored.tabID,
		&stored.terminalID,
		&stored.workspaceID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentRow{}, false, nil
		}
		return agentRow{}, false, err
	}
	return stored, true, nil
}

func scanAgents(rows *sql.Rows, err error) ([]observability.AgentIndexRecord, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	agents := []observability.AgentIndexRecord{}
	for rows.Next() {
		var stored agentRow
		if err := scanAgentRowInto(rows, &stored); err != nil {
			return nil, err
		}
		agents = append(agents, mapAgent(stored))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return agents, nil
}

// scanAgentRows scans the stored rows without mapping them, for the callers that
// need the raw columns the mapped record no longer carries.
func scanAgentRows(rows *sql.Rows, err error) ([]agentRow, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	stored := []agentRow{}
	for rows.Next() {
		var row agentRow
		if err := scanAgentRowInto(rows, &row); err != nil {
			return nil, err
		}
		stored = append(stored, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stored, nil
}

// scanAgentRowInto scans one agents row into row.
func scanAgentRowInto(rows *sql.Rows, row *agentRow) error {
	return rows.Scan(
		&row.agent,
		&row.agentSessionHintJSON,
		&row.agentSessionJSON,
		&row.agentStatus,
		&row.cwd,
		&row.firstSeenAtMs,
		&row.focused,
		&row.foregroundCwd,
		&row.herdrSessionName,
		&row.id,
		&row.lastSeenAtMs,
		&row.name,
		&row.paneID,
		&row.paneRevision,
		&row.paneGeneration,
		&row.grokHome,
		&row.tabID,
		&row.terminalID,
		&row.workspaceID,
	)
}

// mapAgent mirrors mapAgent. The reported session ref wins; the hint is only a
// fallback, and only when it was stored for the same agent kind.
func mapAgent(row agentRow) observability.AgentIndexRecord {
	reportedSession := parseAgentSession(row.agentSessionJSON)
	hintedSession := parseAgentSession(row.agentSessionHintJSON)
	var compatibleHint *observability.AgentSessionRef
	if row.agent.Valid && hintedSession != nil &&
		strings.EqualFold(row.agent.String, hintedSession.Agent) {
		compatibleHint = hintedSession
	}
	agentSession := reportedSession
	if agentSession == nil {
		agentSession = compatibleHint
	}
	return observability.AgentIndexRecord{
		Agent:            nullableString(row.agent),
		AgentSession:     agentSession,
		AgentStatus:      observability.AgentStatus(row.agentStatus),
		Cwd:              nullableString(row.cwd),
		FirstSeenAt:      formatTimestamp(row.firstSeenAtMs),
		Focused:          row.focused != 0,
		ForegroundCwd:    nullableString(row.foregroundCwd),
		HerdrSessionName: row.herdrSessionName,
		ID:               row.id,
		LastSeenAt:       formatTimestamp(row.lastSeenAtMs),
		Name:             nullableString(row.name),
		PaneID:           row.paneID,
		PaneRevision:     nullableInt(row.paneRevision),
		PaneGeneration:   nullableString(row.paneGeneration),
		TabID:            nullableString(row.tabID),
		TerminalID:       nullableString(row.terminalID),
		WorkspaceID:      row.workspaceID,
	}
}

// paneGeneration mirrors paneGeneration: the first of the four reported field
// spellings that is a non-empty string, else nil.
func paneGeneration(agent HerdrAgentLike) *string {
	for _, key := range []string{"pane_generation", "paneGeneration", "creation_id", "creationId"} {
		if value := stringValue(agent[key]); value != nil {
			return value
		}
	}
	return nil
}

// agentSessionJSON mirrors agentSessionJson: the reported session object is
// stored verbatim (field order included) when it is a well-formed AgentSessionRef.
func agentSessionJSON(value any) any {
	record, ok := asJSONRecord(value)
	if !ok {
		return nil
	}
	ref, ok := agentSessionRefFrom(record)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(ref)
	if err != nil {
		return nil
	}
	return string(encoded)
}

// asJSONRecord mirrors the TS `typeof value === "object"` gate: anything that is
// not a JSON object is rejected. Values that already are a map are taken as they
// are; anything else goes through a JSON round trip so a struct is accepted the
// same way an object literal is.
func asJSONRecord(value any) (map[string]any, bool) {
	if record, ok := value.(map[string]any); ok {
		return record, true
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var record map[string]any
	if err := json.Unmarshal(encoded, &record); err != nil || record == nil {
		return nil, false
	}
	return record, true
}

// parseAgentSession mirrors parseAgentSession: an absent, unparseable or
// malformed stored ref is reported as absent.
func parseAgentSession(value sql.NullString) *observability.AgentSessionRef {
	if !value.Valid || value.String == "" {
		return nil
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(value.String), &record); err != nil {
		return nil
	}
	ref, ok := agentSessionRefFrom(record)
	if !ok {
		return nil
	}
	return &ref
}

// agentSessionRefFrom validates the four fields of an AgentSessionRef, the way
// the TS object literals in agentSessionJson and parseAgentSession do.
func agentSessionRefFrom(
	record map[string]any,
) (observability.AgentSessionRef, bool) {
	agent, ok := record["agent"].(string)
	if !ok {
		return observability.AgentSessionRef{}, false
	}
	kind, ok := record["kind"].(string)
	if !ok || (kind != string(observability.SessionRefID) && kind != string(observability.SessionRefPath)) {
		return observability.AgentSessionRef{}, false
	}
	source, ok := record["source"].(string)
	if !ok {
		return observability.AgentSessionRef{}, false
	}
	value, ok := record["value"].(string)
	if !ok {
		return observability.AgentSessionRef{}, false
	}
	return observability.AgentSessionRef{
		Agent:  agent,
		Kind:   observability.AgentSessionRefKind(kind),
		Source: source,
		Value:  value,
	}, true
}

func ambiguousTargetError(
	target string,
	candidates []observability.AgentIndexRecord,
) error {
	parts := make([]string, 0, len(candidates))
	for _, agent := range candidates {
		parts = append(parts, fmt.Sprintf(
			"session=%s workspace=%s pane=%s terminal=%s name=%s agent=%s",
			agent.HerdrSessionName, agent.WorkspaceID, agent.PaneID,
			orUnknown(agent.TerminalID), orUnnamed(agent.Name), orUnknown(agent.Agent),
		))
	}
	return fmt.Errorf("agent target %s is ambiguous; candidates: %s",
		target, strings.Join(parts, "; "))
}

func orUnknown(value *string) string {
	if value == nil {
		return "unknown"
	}
	return *value
}

func orUnnamed(value *string) string {
	if value == nil {
		return "unnamed"
	}
	return *value
}

// agentGenerationClause renders the optional `and pane_generation = ?` clause
// the pane-scoped queries share.
func agentGenerationClause(paneGeneration *string) (string, []any) {
	if paneGeneration == nil {
		return "", []any{}
	}
	return " and pane_generation = ?", []any{paneGeneration}
}

// sameNullableString compares two nullable fields the way `===` does in TS.
func sameNullableString(left *string, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// toAnySlice widens a string slice for the IN-list parameters.
func toAnySlice(values []string) []any {
	widened := make([]any, 0, len(values))
	for _, value := range values {
		widened = append(widened, value)
	}
	return widened
}

// newUUID renders a random v4 UUID in the canonical lowercase 8-4-4-4-12 form
// node's randomUUID produces.
func newUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand only fails when the system entropy source is broken, in
		// which case there is no safe id to mint either.
		panic(fmt.Sprintf("herdsman: cannot read random bytes for an agent id: %v", err))
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
