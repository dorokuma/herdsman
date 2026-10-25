package observability

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"modernc.org/sqlite"
)

// This file ports src/observability/agent-context-service.ts.
//
// Import-cycle note: internal/agenthistory imports internal/observability, so
// the agent-history half of this service is expressed as an interface over
// observability types (AgentHistoryServiceLike). The concrete
// *agenthistory.AgentHistoryService and the module-level helpers it needs
// (`historySourceFromSessionRef`, `safeOfficialSessionPath`,
// `statSourceFingerprint`) reach this package through a thin adapter in the
// wiring layer instead of a direct import. internal/db already imports
// internal/observability, so the two stores appear here as interfaces the
// concrete db stores satisfy as they stand, and the snapshot put input is
// aliased from this package by internal/db.

// RefreshAgentContextInput mirrors RefreshAgentContextInput. OccupiedSessionPaths
// is nil when the caller passed no set; ForceRefresh is nil when the caller did
// not pass the flag at all (TS `undefined`).
type RefreshAgentContextInput struct {
	Agent                AgentIndexRecord
	IdentityChanged      bool
	ForceRefresh         *bool
	OccupiedSessionPaths []string
}

// RefreshAgentContextResult mirrors RefreshAgentContextResult.
type RefreshAgentContextResult struct {
	Changed  bool
	Snapshot AgentContextSnapshotRecord
}

// WorkspaceSnapshotInput mirrors `AgentScope & { excludeTerminalId }`.
type WorkspaceSnapshotInput struct {
	AgentScope
	ExcludeTerminalID string
}

// ---------------------------------------------------------------------------
// Store interfaces. Each method set is a subset of the concrete db store, so the
// stores satisfy them without an adapter.
// ---------------------------------------------------------------------------

// AgentContextSnapshotInput mirrors the TS put input: the record without the
// store-assigned updatedAt.
type AgentContextSnapshotInput struct {
	AgentID           string
	CompactHistory    CompactAgentHistory
	HistoryRef        *AgentHistoryRef
	PaneRevision      *int
	SourceFingerprint *AgentHistorySourceFingerprint
}

// AgentContextSnapshotStoreLike is the slice of
// *db.AgentContextSnapshotStore the context service depends on.
type AgentContextSnapshotStoreLike interface {
	Get(agentID string) (*AgentContextSnapshotRecord, error)
	ListByAgentIds(agentIDs []string) ([]AgentContextSnapshotRecord, error)
	Put(input AgentContextSnapshotInput) (AgentContextSnapshotRecord, error)
}

// AgentContextAgentStoreLike is the slice of *db.AgentStore the context service
// depends on.
type AgentContextAgentStoreLike interface {
	Exists(id string) (bool, error)
	List(scope AgentQueryScope) ([]AgentIndexRecord, error)
	ListForHerdrSession(herdrSessionName string) ([]AgentIndexRecord, error)
}

// ---------------------------------------------------------------------------
// Agent history port
// ---------------------------------------------------------------------------

// AgentHistoryLookupInput mirrors the TS history lookup input.
type AgentHistoryLookupInput struct {
	Agent            *string
	AgentSession     *AgentSessionRef
	HerdrSessionName string
	PaneID           string
}

// AgentHistoryResolveOptions mirrors the resolveCompactHistory options. A nil
// ForceRefresh or PreferredRef means the TS key was absent from the object.
type AgentHistoryResolveOptions struct {
	ForceDiscovery bool
	ForceRefresh   *bool
	PreferredRef   *AgentHistoryRef
}

// AgentHistoryResolved mirrors ResolvedCompactAgentHistory.
type AgentHistoryResolved struct {
	CompactHistory    CompactAgentHistory
	HistoryRef        *AgentHistoryRef
	SourceFingerprint *AgentHistorySourceFingerprint
}

// AgentHistoryServiceLike is the slice of the agent-history module the context
// service depends on: the service itself plus the three module-level helpers
// `agent-context-service.ts` imports from it. The production adapter forwards
// ResolveCompactHistory to *agenthistory.AgentHistoryService (mapping the
// agenthistory records onto the observability ones) and the helpers to
// agenthistory.StatSourceFingerprint,
// agenthistory.HistorySourceFromSessionRef and
// agenthistory.SafeOfficialSessionPath.
type AgentHistoryServiceLike interface {
	ResolveCompactHistory(
		input AgentHistoryLookupInput,
		options AgentHistoryResolveOptions,
	) AgentHistoryResolved
	StatSourceFingerprint(path string) *AgentHistorySourceFingerprint
	HistorySourceFromSessionRef(ref AgentSessionRef) AgentHistoryRefSource
	SafeOfficialSessionPath(value string) string
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// AgentContextService ports the TS class of the same name.
type AgentContextService struct {
	history    AgentHistoryServiceLike
	snapshots  AgentContextSnapshotStoreLike
	agents     AgentContextAgentStoreLike
	occupiedMu sync.Mutex
	// occupiedFingerprintByAgent mirrors #occupiedFingerprintByAgent. The TS
	// daemon is single-threaded; the Go daemon refreshes from more than one
	// goroutine, so the map is guarded.
	occupiedFingerprintByAgent map[string]string
}

// NewAgentContextService mirrors the TS constructor.
func NewAgentContextService(options struct {
	History AgentHistoryServiceLike
	Stores  struct {
		AgentContextSnapshots AgentContextSnapshotStoreLike
		Agents                AgentContextAgentStoreLike
	}
}) *AgentContextService {
	return &AgentContextService{
		history:                    options.History,
		snapshots:                  options.Stores.AgentContextSnapshots,
		agents:                     options.Stores.Agents,
		occupiedFingerprintByAgent: map[string]string{},
	}
}

// OccupiedSessionPathsFor mirrors occupiedSessionPathsFor.
func (s *AgentContextService) OccupiedSessionPathsFor(
	agent AgentIndexRecord,
) (map[string]struct{}, error) {
	return occupiedForAgent(agent, s.agents, s.snapshots)
}

// HistoryLookupInput mirrors historyLookupInput.
func (s *AgentContextService) HistoryLookupInput(agent AgentIndexRecord) AgentHistoryLookupInput {
	return historyLookup(agent)
}

// RefreshAgent mirrors refreshAgent.
func (s *AgentContextService) RefreshAgent(
	input RefreshAgentContextInput,
) (RefreshAgentContextResult, error) {
	previous, err := s.snapshots.Get(input.Agent.ID)
	if err != nil {
		return RefreshAgentContextResult{}, err
	}
	occupied, err := occupiedForAgent(input.Agent, s.agents, s.snapshots)
	if err != nil {
		return RefreshAgentContextResult{}, err
	}
	for _, path := range input.OccupiedSessionPaths {
		occupied[path] = struct{}{}
	}
	occupiedFingerprint := fingerprintOccupied(occupied)
	s.occupiedMu.Lock()
	priorOccupiedFingerprint, remembered := s.occupiedFingerprintByAgent[input.Agent.ID]
	s.occupiedFingerprintByAgent[input.Agent.ID] = occupiedFingerprint
	s.occupiedMu.Unlock()
	occupiedChanged := remembered && priorOccupiedFingerprint != occupiedFingerprint
	preferredRef := selectPreferredRef(input.Agent, previous, s.history)
	forceRefresh := input.ForceRefresh != nil && *input.ForceRefresh
	forceDiscovery := forceRefresh
	if !forceDiscovery {
		forced, err := s.shouldForceDiscovery(shouldForceDiscoveryInput{
			agent:                  input.Agent,
			directAuthoritativeRef: pathHistoryRefFromAgent(input.Agent, s.history),
			identityChanged:        input.IdentityChanged || occupiedChanged,
			previous:               previous,
		})
		if err != nil {
			return RefreshAgentContextResult{}, err
		}
		forceDiscovery = forced
	}
	resolved := bindAuthoritativeId(input.Agent, s.history, s.history.ResolveCompactHistory(
		historyLookup(input.Agent),
		AgentHistoryResolveOptions{
			ForceDiscovery: forceDiscovery,
			ForceRefresh:   input.ForceRefresh,
			PreferredRef:   preferredRef,
		},
	))
	next := AgentContextSnapshotInput{
		AgentID:           input.Agent.ID,
		CompactHistory:    resolved.CompactHistory,
		HistoryRef:        resolved.HistoryRef,
		PaneRevision:      input.Agent.PaneRevision,
		SourceFingerprint: resolved.SourceFingerprint,
	}
	if previous != nil && sameSnapshotPayload(*previous, next) {
		return RefreshAgentContextResult{Changed: false, Snapshot: *previous}, nil
	}
	// `agent_context_snapshots.agent_id` references `agents(id)` (drizzle/
	// 0003_opposite_tarantula.sql), and a pane can be physically deleted while the
	// daemon is still draining a persisted status event plan (journal 18:17:10
	// `status event plan drain rejected rows { rejected: 1, errors: [...] }`).
	// The write then fails with `FOREIGN KEY constraint failed` and takes the
	// whole drain batch (every other row in it) down with it. A deleted agent can
	// never be read back from this store anyway, so degrade to an in-memory
	// snapshot that still carries the round's compact history to its caller.
	agentExists, err := s.agents.Exists(input.Agent.ID)
	if err != nil {
		return RefreshAgentContextResult{}, err
	}
	if !agentExists {
		s.logDeletedAgentSkip(input.Agent)
		return RefreshAgentContextResult{Changed: true, Snapshot: inMemorySnapshot(next)}, nil
	}
	snapshot, err := s.snapshots.Put(next)
	if err != nil {
		// The row can still be deleted between the check above and the insert; the
		// foreign-key failure must stay contained to this one snapshot.
		if !isForeignKeyConstraintError(err) {
			return RefreshAgentContextResult{}, err
		}
		s.logDeletedAgentSkip(input.Agent)
		return RefreshAgentContextResult{Changed: true, Snapshot: inMemorySnapshot(next)}, nil
	}
	return RefreshAgentContextResult{Changed: true, Snapshot: snapshot}, nil
}

func (s *AgentContextService) logDeletedAgentSkip(agent AgentIndexRecord) {
	fmt.Fprintf(os.Stderr,
		"Herdsman skipped agent context snapshot write for a deleted agent"+
			" (agentId=%s, herdrSessionName=%s, paneId=%s)\n",
		agent.ID, agent.HerdrSessionName, agent.PaneID)
}

// GetAgentSnapshot mirrors getAgentSnapshot.
func (s *AgentContextService) GetAgentSnapshot(
	agentID string,
) (*AgentContextSnapshotRecord, error) {
	return s.snapshots.Get(agentID)
}

// ListAgents mirrors listAgents.
func (s *AgentContextService) ListAgents(scope AgentQueryScope) ([]AgentListItem, error) {
	agents, err := s.agents.List(scope)
	if err != nil {
		return nil, err
	}
	agentIDs := make([]string, 0, len(agents))
	for _, agent := range agents {
		agentIDs = append(agentIDs, agent.ID)
	}
	snapshots, err := s.snapshots.ListByAgentIds(agentIDs)
	if err != nil {
		return nil, err
	}
	byAgentID := make(map[string]AgentContextSnapshotRecord, len(snapshots))
	for _, snapshot := range snapshots {
		byAgentID[snapshot.AgentID] = snapshot
	}
	items := make([]AgentListItem, 0, len(agents))
	for _, agent := range agents {
		snapshot, ok := byAgentID[agent.ID]
		var compactHistory *CompactAgentHistory
		if ok {
			compactHistory = &snapshot.CompactHistory
		}
		items = append(items, listItem(agent, compactHistory))
	}
	return items, nil
}

// WorkspaceSnapshot mirrors workspaceSnapshot: the peers of one workspace, or
// nil when none of them has a persisted snapshot.
func (s *AgentContextService) WorkspaceSnapshot(
	input WorkspaceSnapshotInput,
) (*AgentWorkspaceContextSnapshot, error) {
	agents, err := s.agents.List(AgentQueryScope{
		HerdrSessionName: &input.HerdrSessionName,
		WorkspaceID:      &input.WorkspaceID,
	})
	if err != nil {
		return nil, err
	}
	peers := []AgentIndexRecord{}
	for _, agent := range agents {
		if agent.TerminalID == nil || *agent.TerminalID != input.ExcludeTerminalID {
			peers = append(peers, agent)
		}
	}
	agentIDs := make([]string, 0, len(peers))
	for _, agent := range peers {
		agentIDs = append(agentIDs, agent.ID)
	}
	snapshots, err := s.snapshots.ListByAgentIds(agentIDs)
	if err != nil {
		return nil, err
	}
	if len(snapshots) == 0 {
		return nil, nil
	}
	byAgentID := make(map[string]AgentContextSnapshotRecord, len(snapshots))
	for _, snapshot := range snapshots {
		byAgentID[snapshot.AgentID] = snapshot
	}
	updatedAt := snapshots[0].UpdatedAt
	for _, snapshot := range snapshots[1:] {
		if snapshot.UpdatedAt > updatedAt {
			updatedAt = snapshot.UpdatedAt
		}
	}
	items := make([]AgentListItem, 0, len(peers))
	for _, agent := range peers {
		snapshot, ok := byAgentID[agent.ID]
		var compactHistory *CompactAgentHistory
		if ok {
			compactHistory = &snapshot.CompactHistory
		}
		items = append(items, listItem(agent, compactHistory))
	}
	return &AgentWorkspaceContextSnapshot{
		AgentScope: input.AgentScope,
		Agents:     items,
		UpdatedAt:  updatedAt,
	}, nil
}

// isoTimestampLayout is JavaScript's Date#toISOString millisecond layout, which
// the TS contracts' Date fields serialize to.
const isoTimestampLayout = "2006-01-02T15:04:05.000Z"

// sqliteForeignKeyConstraint is SQLite's SQLITE_CONSTRAINT_FOREIGNKEY result
// code (19 | (3 << 8)), which `node:sqlite` exposes as `errcode` on the thrown
// Error.
const sqliteForeignKeyConstraint = 19 | (3 << 8)

func isForeignKeyConstraintError(err error) bool {
	var sqliteError *sqlite.Error
	return errors.As(err, &sqliteError) && sqliteError.Code() == sqliteForeignKeyConstraint
}

// inMemorySnapshot mirrors inMemorySnapshot: the snapshot a deleted agent would
// have received, stamped with the current time instead of the monotonic store
// timestamp.
func inMemorySnapshot(next AgentContextSnapshotInput) AgentContextSnapshotRecord {
	return AgentContextSnapshotRecord{
		AgentID:           next.AgentID,
		CompactHistory:    next.CompactHistory,
		HistoryRef:        next.HistoryRef,
		PaneRevision:      next.PaneRevision,
		SourceFingerprint: next.SourceFingerprint,
		UpdatedAt:         time.Now().UTC().Format(isoTimestampLayout),
	}
}

type shouldForceDiscoveryInput struct {
	agent                  AgentIndexRecord
	directAuthoritativeRef *AgentHistoryRef
	identityChanged        bool
	previous               *AgentContextSnapshotRecord
}

// shouldForceDiscovery mirrors shouldForceDiscovery: whether this regular
// refresh round must ignore the persisted ref and resolve again from the
// official values.
//
// A pane herdsman cannot resolve does **not** force anything: discovery runs for
// it on the ordinary path anyway, and its remembered failure is what decelerates
// that cadence (`discovery.ts`). Forcing every round would keep the memory
// permanently fresh, so its retry window could never expire and the deceleration
// would never apply. Explicit reads (`agent.get` / `agent.read`) and an external
// forced refresh do not go through this function and retry regardless.
//
// What is left here is the resolved case: re-resolve instead of reusing the
// persisted ref when the pane's identity changed, its revision moved in a way
// the persisted ref cannot explain, or its source content moved under the same
// fingerprint.
func (s *AgentContextService) shouldForceDiscovery(
	input shouldForceDiscoveryInput,
) (bool, error) {
	// The official path is readable, so the persisted ref is the current one.
	if input.directAuthoritativeRef != nil {
		return false, nil
	}
	// An id pane has no official path: its lookup is always the deterministic id
	// template, which discovery runs on the ordinary path anyway.
	if input.agent.AgentSession != nil && input.agent.AgentSession.Kind == SessionRefID {
		return false, nil
	}
	// No resolved ref yet: nothing to reuse, and nothing to force.
	if input.previous == nil || input.previous.HistoryRef == nil {
		return false, nil
	}
	if input.identityChanged {
		return true, nil
	}
	if paneRevisionDecreased(input.agent.PaneRevision, input.previous.PaneRevision) {
		return true, nil
	}
	if !paneRevisionIncreased(input.agent.PaneRevision, input.previous.PaneRevision) {
		return false, nil
	}
	fingerprint := input.previous.SourceFingerprint
	if fingerprint == nil {
		return true, nil
	}
	current := s.history.StatSourceFingerprint(fingerprint.Path)
	return current == nil || sameFingerprint(fingerprint, current), nil
}

func pathHistoryRefFromAgent(
	agent AgentIndexRecord,
	history AgentHistoryServiceLike,
) *AgentHistoryRef {
	if agent.AgentSession == nil || agent.AgentSession.Kind != SessionRefPath {
		return nil
	}
	path := history.SafeOfficialSessionPath(agent.AgentSession.Value)
	if path == "" {
		return nil
	}
	return &AgentHistoryRef{
		Kind:   HistoryRefAgentSession,
		Path:   &path,
		Source: history.HistorySourceFromSessionRef(*agent.AgentSession),
		Value:  agent.AgentSession.Value,
	}
}

func matchingAuthoritativeIdRef(
	agent AgentIndexRecord,
	previous *AgentHistoryRef,
	history AgentHistoryServiceLike,
) *AgentHistoryRef {
	session := agent.AgentSession
	if session == nil ||
		session.Kind != SessionRefID ||
		previous == nil ||
		previous.Kind != HistoryRefAgentSession ||
		previous.Path == nil ||
		previous.Source != history.HistorySourceFromSessionRef(*session) ||
		previous.Value != session.Value {
		return nil
	}
	return previous
}

// bindAuthoritativeId mirrors bindAuthoritativeId: an id pane keeps the path
// discovery resolved, but the ref it reports is rebuilt from the official id.
func bindAuthoritativeId(
	agent AgentIndexRecord,
	history AgentHistoryServiceLike,
	resolved AgentHistoryResolved,
) AgentHistoryResolved {
	session := agent.AgentSession
	if session == nil || session.Kind != SessionRefID || resolved.HistoryRef == nil ||
		resolved.HistoryRef.Path == nil {
		return resolved
	}
	historyRef := AgentHistoryRef{
		Kind:   HistoryRefAgentSession,
		Path:   resolved.HistoryRef.Path,
		Source: history.HistorySourceFromSessionRef(*session),
		Value:  session.Value,
	}
	resolved.CompactHistory.HistoryRef = &historyRef
	resolved.HistoryRef = &historyRef
	return resolved
}

// selectPreferredRef mirrors selectPreferredRef: the ref discovery is asked to
// read before it falls back to a lookup.
func selectPreferredRef(
	agent AgentIndexRecord,
	previous *AgentContextSnapshotRecord,
	history AgentHistoryServiceLike,
) *AgentHistoryRef {
	var previousRef *AgentHistoryRef
	if previous != nil {
		previousRef = previous.HistoryRef
	}
	directAuthoritativeRef := pathHistoryRefFromAgent(agent, history)
	if directAuthoritativeRef != nil {
		return directAuthoritativeRef
	}
	matchingID := matchingAuthoritativeIdRef(agent, previousRef, history)
	if matchingID != nil {
		return matchingID
	}
	// A `discovered_file` ref came from the deleted cwd/mtime scanner. Herdsman no
	// longer produces one and no longer trusts a persisted one: the official
	// `agent_session` value (or its explicit absence) decides from now on.
	if previousRef != nil && previousRef.Kind == HistoryRefDiscoveredFile {
		return nil
	}
	if agent.AgentSession != nil {
		return nil
	}
	return previousRef
}

// OccupiedForAgent mirrors occupiedForAgent: the session paths this agent must
// not claim, taken from the other agents' live official values and from their
// persisted snapshot refs.
func OccupiedForAgent(
	agent AgentIndexRecord,
	agents AgentContextAgentStoreLike,
	snapshots AgentContextSnapshotStoreLike,
) (map[string]struct{}, error) {
	return occupiedForAgent(agent, agents, snapshots)
}

func occupiedForAgent(
	agent AgentIndexRecord,
	agents AgentContextAgentStoreLike,
	snapshots AgentContextSnapshotStoreLike,
) (map[string]struct{}, error) {
	others, err := agents.ListForHerdrSession(agent.HerdrSessionName)
	if err != nil {
		return nil, err
	}
	otherIDs := []string{}
	occupied := map[string]struct{}{}
	for _, candidate := range others {
		if candidate.ID == agent.ID {
			continue
		}
		otherIDs = append(otherIDs, candidate.ID)
		if candidate.AgentSession != nil && candidate.AgentSession.Kind == SessionRefPath {
			occupied[candidate.AgentSession.Value] = struct{}{}
		}
	}
	otherSnapshots, err := snapshots.ListByAgentIds(otherIDs)
	if err != nil {
		return nil, err
	}
	for _, snapshot := range otherSnapshots {
		if snapshot.HistoryRef != nil && snapshot.HistoryRef.Path != nil {
			occupied[*snapshot.HistoryRef.Path] = struct{}{}
		}
	}
	return occupied, nil
}

func fingerprintOccupied(paths map[string]struct{}) string {
	sorted := make([]string, 0, len(paths))
	for path := range paths {
		sorted = append(sorted, path)
	}
	sort.Strings(sorted)
	joined := ""
	for index, path := range sorted {
		if index > 0 {
			joined += "\x00"
		}
		joined += path
	}
	return joined
}

func historyLookup(agent AgentIndexRecord) AgentHistoryLookupInput {
	return AgentHistoryLookupInput{
		Agent:            agent.Agent,
		AgentSession:     agent.AgentSession,
		HerdrSessionName: agent.HerdrSessionName,
		PaneID:           agent.PaneID,
	}
}

func paneRevisionIncreased(current *int, previous *int) bool {
	return current != nil && previous != nil && *current > *previous
}

func paneRevisionDecreased(current *int, previous *int) bool {
	return current != nil && previous != nil && *current < *previous
}

func sameSnapshotPayload(previous AgentContextSnapshotRecord, next AgentContextSnapshotInput) bool {
	previousJSON, err := json.Marshal(previous.CompactHistory)
	if err != nil {
		return false
	}
	nextJSON, err := json.Marshal(next.CompactHistory)
	if err != nil {
		return false
	}
	return string(previousJSON) == string(nextJSON) &&
		sameHistoryRef(previous.HistoryRef, next.HistoryRef) &&
		sameFingerprint(previous.SourceFingerprint, next.SourceFingerprint) &&
		sameOptionalInt(previous.PaneRevision, next.PaneRevision)
}

func sameFingerprint(left *AgentHistorySourceFingerprint, right *AgentHistorySourceFingerprint) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Path == right.Path && left.MtimeMs == right.MtimeMs && left.Size == right.Size
}

func sameHistoryRef(left *AgentHistoryRef, right *AgentHistoryRef) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Kind == right.Kind &&
		sameOptionalString(left.Path, right.Path) &&
		left.Source == right.Source &&
		left.Value == right.Value
}

func sameOptionalString(left *string, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameOptionalInt(left *int, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func listItem(agent AgentIndexRecord, compactHistory *CompactAgentHistory) AgentListItem {
	compact := CompactAgentHistory{}
	if compactHistory != nil {
		compact = *compactHistory
	}
	return AgentListItem{
		AgentIndexRecord: agent,
		History: AgentListItemHistory{
			LastAssistantMessage: compact.LastAssistantMessage,
			LastUserMessage:      compact.LastUserMessage,
			Source:               compact.Source,
			UpdatedAt:            compact.UpdatedAt,
		},
	}
}
