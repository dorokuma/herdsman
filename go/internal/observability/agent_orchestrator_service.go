package observability

import (
	"errors"
	"fmt"
	"os"
	"sort"
)

// This file ports src/observability/agent-orchestrator-service.ts.
//
// Import-cycle note: internal/db already imports internal/observability, so this
// package cannot import internal/db. The three stores therefore appear here as
// interfaces, and the query/result types those interfaces speak
// (ListAfterInput, NextDeliverableAfterInput, ClaimOrchestratorInput, …) live in
// this package too, with internal/db aliasing them. The concrete
// *db.AgentEventStore, *db.AgentStore and *db.AgentOrchestratorScopeStore satisfy
// the interfaces as they stand.

// AgentOrchestratorScopeKey mirrors the TS alias: a scope is addressed by its
// Herdr session and workspace.
type AgentOrchestratorScopeKey = AgentScope

// ---------------------------------------------------------------------------
// Store query inputs, ported from src/db so both packages share one type.
// ---------------------------------------------------------------------------

// ListAfterInput mirrors `AgentQueryScope & { afterEventId?, limit?,
// ownerTerminalId? }`. AfterEventID and Limit are optional; nil means the TS
// default (0 and 100 respectively).
type ListAfterInput struct {
	AfterEventID     *int64
	HerdrSessionName *string
	Limit            *int
	OwnerTerminalID  *string
	WorkspaceID      *string
}

// NextDeliverableAfterInput mirrors the TS input of the same name.
type NextDeliverableAfterInput struct {
	AfterEventID     int64
	GetAgent         func(agentID string) *AgentIndexRecord
	HerdrSessionName string
	OwnerTerminalID  string
	WorkspaceID      string
}

// ClaimOrchestratorInput mirrors the TS claim input.
type ClaimOrchestratorInput struct {
	AgentOrchestratorScopeKey
	AckedEventID   int64
	PaneID         string
	TerminalID     string
	OwnerConnected *bool
}

// ReleaseIfOwnerInput mirrors the TS releaseIfOwner input.
type ReleaseIfOwnerInput struct {
	AgentOrchestratorScopeKey
	TerminalID string
}

// AckInput mirrors the TS ack input.
type AckInput struct {
	AgentOrchestratorScopeKey
	EventID    int64
	TerminalID string
}

// MoveOwnerInput mirrors the TS moveOwner input.
type MoveOwnerInput struct {
	From               AgentOrchestratorScopeKey
	PaneID             string
	TargetAckedEventID int64
	TerminalID         string
	To                 AgentOrchestratorScopeKey
}

// AgentOrchestratorScopeChange mirrors ScopeChange: the state before and after
// one ownership change.
type AgentOrchestratorScopeChange struct {
	Current  AgentOrchestratorState
	Previous AgentOrchestratorState
}

// AgentOrchestratorReleaseResult mirrors the { changed, current, previous }
// shape the release operations return.
type AgentOrchestratorReleaseResult struct {
	Changed  bool
	Current  *AgentOrchestratorState
	Previous *AgentOrchestratorState
}

// ---------------------------------------------------------------------------
// Store interfaces. Each method set is a subset of the concrete db store, so the
// stores satisfy them without an adapter.
// ---------------------------------------------------------------------------

// DeliverabilityGuard is the single delivery predicate shared by pending
// discovery and acknowledgement.
type DeliverabilityGuard interface {
	IsDeliverableAgentEvent(
		event AgentEventRecord,
		agent *AgentIndexRecord,
		scope AgentScope,
		ownerTerminalID string,
	) bool
}

// AgentEventStoreLike is the slice of *db.AgentEventStore the orchestrator
// service depends on. AgentIndexEventStoreLike (the index service's slice)
// embeds it.
type AgentEventStoreLike interface {
	DeliverabilityGuard
	Get(id int64) (AgentEventRecord, error)
	LatestEventID(scope AgentQueryScope) (int64, error)
	ListAfter(input ListAfterInput) ([]AgentEventRecord, error)
	MarkAcked(id int64, scope *AgentScope) error
	NextDeliverableAfter(input NextDeliverableAfterInput) (*AgentEventRecord, error)
	ReclaimDelivered(timeoutMs int64) (int, error)
	ReservePending(terminalID string, limit int, ids []int64) ([]AgentEventRecord, error)
}

// AgentStoreLike is the slice of *db.AgentStore the orchestrator service depends
// on. AgentIndexAgentStoreLike extends it for the index service.
type AgentStoreLike interface {
	Get(id string) (AgentIndexRecord, error)
	List(scope AgentQueryScope) ([]AgentIndexRecord, error)
}

// AgentOrchestratorScopeStoreLike is the slice of *db.AgentOrchestratorScopeStore
// the orchestrator service depends on.
type AgentOrchestratorScopeStoreLike interface {
	Ack(input AckInput) (AgentOrchestratorState, error)
	Claim(input ClaimOrchestratorInput) (AgentOrchestratorScopeChange, error)
	Get(scope AgentScope) (*AgentOrchestratorState, error)
	ListOwned() ([]AgentOrchestratorState, error)
	ListOwnedForSession(herdrSessionName string) ([]AgentOrchestratorState, error)
	MoveOwner(input MoveOwnerInput) ([]AgentOrchestratorScopeChange, error)
	ReleaseIfOwner(input ReleaseIfOwnerInput) (AgentOrchestratorReleaseResult, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// AgentOrchestratorChange mirrors the TS record of the same name.
type AgentOrchestratorChange struct {
	Current  AgentOrchestratorState
	Previous AgentOrchestratorState
	Reason   AgentOrchestratorChangeReason
}

// AgentOrchestratorService ports the TS class of the same name.
type AgentOrchestratorService struct {
	agentEvents AgentEventStoreLike
	agents      AgentStoreLike
	scopes      AgentOrchestratorScopeStoreLike
}

// NewAgentOrchestratorService mirrors the TS constructor.
func NewAgentOrchestratorService(options struct {
	AgentEvents AgentEventStoreLike
	Agents      AgentStoreLike
	Scopes      AgentOrchestratorScopeStoreLike
}) *AgentOrchestratorService {
	return &AgentOrchestratorService{
		agentEvents: options.AgentEvents,
		agents:      options.Agents,
		scopes:      options.Scopes,
	}
}

// Status mirrors status: the scope state, or nil when the scope was never
// claimed.
func (s *AgentOrchestratorService) Status(scope AgentScope) (*AgentOrchestratorState, error) {
	return s.scopes.Get(scope)
}

// OrchestratorClaimInput mirrors the TS claim input.
type OrchestratorClaimInput struct {
	AgentScope
	PaneID         string
	TerminalID     string
	OwnerConnected *bool
}

// Claim mirrors claim.
func (s *AgentOrchestratorService) Claim(input OrchestratorClaimInput) (AgentOrchestratorChange, error) {
	cursor, err := s.claimCursor(input.AgentScope)
	if err != nil {
		return AgentOrchestratorChange{}, err
	}
	change, err := s.scopes.Claim(ClaimOrchestratorInput{
		AgentOrchestratorScopeKey: input.AgentScope,
		AckedEventID:              cursor,
		PaneID:                    input.PaneID,
		TerminalID:                input.TerminalID,
		OwnerConnected:            input.OwnerConnected,
	})
	if err != nil {
		return AgentOrchestratorChange{}, err
	}
	return AgentOrchestratorChange{
		Current:  change.Current,
		Previous: change.Previous,
		Reason:   OrchestratorChangeClaimed,
	}, nil
}

// OrchestratorReleaseInput mirrors the TS release input.
type OrchestratorReleaseInput struct {
	AgentScope
	Reason     AgentOrchestratorChangeReason
	TerminalID string
}

// Release mirrors release: a nil change means nothing changed.
func (s *AgentOrchestratorService) Release(input OrchestratorReleaseInput) (*AgentOrchestratorChange, error) {
	change, err := s.scopes.ReleaseIfOwner(ReleaseIfOwnerInput{
		AgentOrchestratorScopeKey: input.AgentScope,
		TerminalID:                input.TerminalID,
	})
	if err != nil {
		return nil, err
	}
	if !change.Changed || change.Current == nil || change.Previous == nil {
		return nil, nil
	}
	return &AgentOrchestratorChange{
		Current:  *change.Current,
		Previous: *change.Previous,
		Reason:   input.Reason,
	}, nil
}

// OrchestratorPendingInput mirrors the TS pending input.
type OrchestratorPendingInput struct {
	AgentScope
	ExtraScopes []AgentScope
	Limit       *int
	TerminalID  string
}

// Pending mirrors pending.
func (s *AgentOrchestratorService) Pending(input OrchestratorPendingInput) ([]AgentEventRecord, error) {
	if _, err := s.agentEvents.ReclaimDelivered(60_000); err != nil {
		return nil, err
	}
	state, err := s.scopes.Get(input.AgentScope)
	if err != nil {
		return nil, err
	}
	if state == nil || state.Owner == nil || state.Owner.TerminalID != input.TerminalID {
		return []AgentEventRecord{}, nil
	}

	limit := 100
	if input.Limit != nil {
		limit = *input.Limit
	}
	scopes := mergeScopes(append([]AgentScope{input.AgentScope}, input.ExtraScopes...))
	validAgents := map[string]AgentIndexRecord{}
	for _, scope := range scopes {
		agents, err := s.agents.List(AgentQueryScope{
			HerdrSessionName: &scope.HerdrSessionName,
			WorkspaceID:      &scope.WorkspaceID,
		})
		if err != nil {
			return nil, err
		}
		for _, agent := range agents {
			validAgents[agent.ID] = agent
		}
	}
	pending := []AgentEventRecord{}
	afterEventID := state.AckedEventID
	scanned := 0
	for len(pending) < limit && scanned < 1_000 {
		batchLimit := min(100, 1_000-scanned)
		batch, err := s.listAfterAcrossScopes(scopes, ListAfterInput{
			AfterEventID:    &afterEventID,
			Limit:           &batchLimit,
			OwnerTerminalID: &input.TerminalID,
		})
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		scanned += len(batch)
		afterEventID = batch[len(batch)-1].ID
		for _, event := range batch {
			valid := s.agentEvents.IsDeliverableAgentEvent(
				event,
				agentFor(validAgents, event.AgentID),
				eventScope(event, input.WorkspaceID),
				input.TerminalID,
			)
			if valid && event.TerminalID != nil && *event.TerminalID != input.TerminalID {
				pending = append(pending, event)
			}
			if len(pending) == limit {
				break
			}
		}
	}
	if len(pending) == 0 {
		return []AgentEventRecord{}, nil
	}
	ids := make([]int64, 0, len(pending))
	for _, event := range pending {
		ids = append(ids, event.ID)
	}
	return s.agentEvents.ReservePending(input.TerminalID, limit, ids)
}

// OwnerlessSiblingScopes mirrors ownerlessSiblingScopes.
//
// Workspaces in the same Herdr session that have no orchestrator owner of their
// own. Workers dispatched into them (script-dispatched `herdr tab create` panes)
// stay invisible to the session orchestrator unless its owner is allowed to
// receive and acknowledge their events.
func (s *AgentOrchestratorService) OwnerlessSiblingScopes(input AgentScope) ([]AgentScope, error) {
	owned, err := s.scopes.ListOwnedForSession(input.HerdrSessionName)
	if err != nil {
		return nil, err
	}
	ownedWorkspaces := map[string]struct{}{}
	for _, scope := range owned {
		ownedWorkspaces[scope.WorkspaceID] = struct{}{}
	}
	agents, err := s.agents.List(AgentQueryScope{
		HerdrSessionName: &input.HerdrSessionName,
	})
	if err != nil {
		return nil, err
	}
	workspaces := map[string]struct{}{}
	for _, agent := range agents {
		workspaces[agent.WorkspaceID] = struct{}{}
	}
	ids := make([]string, 0, len(workspaces))
	for workspaceID := range workspaces {
		ids = append(ids, workspaceID)
	}
	sort.Strings(ids)
	siblings := make([]AgentScope, 0, len(ids))
	for _, workspaceID := range ids {
		if workspaceID == input.WorkspaceID {
			continue
		}
		if _, owned := ownedWorkspaces[workspaceID]; owned {
			continue
		}
		siblings = append(siblings, AgentScope{
			HerdrSessionName: input.HerdrSessionName,
			WorkspaceID:      workspaceID,
		})
	}
	return siblings, nil
}

// listAfterAcrossScopes mirrors #listAfterAcrossScopes.
func (s *AgentOrchestratorService) listAfterAcrossScopes(
	scopes []AgentScope,
	input ListAfterInput,
) ([]AgentEventRecord, error) {
	if len(scopes) == 1 {
		merged := input
		merged.HerdrSessionName = &scopes[0].HerdrSessionName
		merged.WorkspaceID = &scopes[0].WorkspaceID
		return s.agentEvents.ListAfter(merged)
	}
	byID := map[int64]AgentEventRecord{}
	for _, scope := range scopes {
		merged := input
		merged.HerdrSessionName = &scope.HerdrSessionName
		merged.WorkspaceID = &scope.WorkspaceID
		events, err := s.agentEvents.ListAfter(merged)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			byID[event.ID] = event
		}
	}
	mergedEvents := make([]AgentEventRecord, 0, len(byID))
	for _, event := range byID {
		mergedEvents = append(mergedEvents, event)
	}
	sort.Slice(mergedEvents, func(left, right int) bool {
		return mergedEvents[left].ID < mergedEvents[right].ID
	})
	limit := 100
	if input.Limit != nil {
		limit = *input.Limit
	}
	if len(mergedEvents) > limit {
		mergedEvents = mergedEvents[:limit]
	}
	return mergedEvents, nil
}

// nextDeliverableAcrossScopes mirrors #nextDeliverableAcrossScopes.
//
// The lowest deliverable event id the owner can still receive, across its own
// workspace and every ownerless sibling workspace it serves.
//
// Event ids are global, so ordering has to be judged on the merged view: a lower
// id sitting in a sibling workspace blocks the acknowledgement of a higher id in
// the owner's own workspace just as much as the other way round. afterEventId is
// the owner's own cursor, matching the scan pending() uses for both workspace
// kinds.
func (s *AgentOrchestratorService) nextDeliverableAcrossScopes(
	input AgentScope,
	terminalID string,
	afterEventID int64,
) (*AgentEventRecord, error) {
	siblings, err := s.OwnerlessSiblingScopes(input)
	if err != nil {
		return nil, err
	}
	var next *AgentEventRecord
	for _, scope := range mergeScopes(append([]AgentScope{input}, siblings...)) {
		candidate, err := s.agentEvents.NextDeliverableAfter(NextDeliverableAfterInput{
			AfterEventID:     afterEventID,
			GetAgent:         s.safeGetAgent,
			HerdrSessionName: scope.HerdrSessionName,
			OwnerTerminalID:  terminalID,
			WorkspaceID:      scope.WorkspaceID,
		})
		if err != nil {
			return nil, err
		}
		if candidate != nil && (next == nil || candidate.ID < next.ID) {
			next = candidate
		}
	}
	return next, nil
}

// OrchestratorAckInput mirrors the TS ack input.
type OrchestratorAckInput struct {
	AgentScope
	EventID    int64
	TerminalID string
}

// Ack mirrors ack.
func (s *AgentOrchestratorService) Ack(input OrchestratorAckInput) (AgentOrchestratorState, error) {
	state, err := s.ack(input)
	if err != nil {
		var ackErr *OrchestratorAckError
		if errors.As(err, &ackErr) {
			current, getErr := s.scopes.Get(input.AgentScope)
			if getErr != nil {
				return AgentOrchestratorState{}, getErr
			}
			expectedNextEventID := "null"
			if current != nil && current.Owner != nil &&
				current.Owner.TerminalID == input.TerminalID {
				next, nextErr := s.nextDeliverableAcrossScopes(
					input.AgentScope, input.TerminalID, current.AckedEventID,
				)
				if nextErr != nil {
					return AgentOrchestratorState{}, nextErr
				}
				if next != nil {
					expectedNextEventID = fmt.Sprintf("%d", next.ID)
				}
			}
			fmt.Fprintf(os.Stderr,
				"Herdsman orchestrator ack rejected: eventId=%d requestTerminal=%s"+
					" expectedNextEventId=%s code=%s herdrSessionName=%s workspaceId=%s\n",
				input.EventID, input.TerminalID, expectedNextEventID, ackErr.Code,
				input.HerdrSessionName, input.WorkspaceID)
		}
		return AgentOrchestratorState{}, err
	}
	return state, nil
}

// safeGetAgent mirrors #safeGetAgent: a missing or unreadable agent row is
// reported as absent instead of failing the whole delivery decision.
func (s *AgentOrchestratorService) safeGetAgent(agentID string) *AgentIndexRecord {
	agent, err := s.agents.Get(agentID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Herdsman orchestrator skipped missing agent: agentId=%s\n", agentID)
		return nil
	}
	return &agent
}

func (s *AgentOrchestratorService) ack(input OrchestratorAckInput) (AgentOrchestratorState, error) {
	state, err := s.scopes.Get(input.AgentScope)
	if err != nil {
		return AgentOrchestratorState{}, err
	}
	if state == nil || state.Owner == nil || state.Owner.TerminalID != input.TerminalID {
		return AgentOrchestratorState{}, NewOrchestratorAckError(AckNotOwner, MsgAckNotOwner)
	}
	if input.EventID <= state.AckedEventID {
		return *state, nil
	}
	event, err := s.agentEvents.Get(input.EventID)
	if err != nil {
		return AgentOrchestratorState{}, NewOrchestratorAckError(AckEventNotFound, MsgAckOutOfOrder)
	}
	if event.HerdrSessionName != input.HerdrSessionName {
		return AgentOrchestratorState{}, NewOrchestratorAckError(AckEventNotInScope, MsgAckOutOfOrder)
	}
	allowed, err := s.isAckScopeAllowed(event, input)
	if err != nil {
		return AgentOrchestratorState{}, err
	}
	if !allowed {
		return AgentOrchestratorState{}, NewOrchestratorAckError(AckEventNotInScope, MsgAckOutOfOrder)
	}
	if event.Status != nil && *event.Status == AgentEventStatusDelivered &&
		(event.DeliveredToTerminalID == nil ||
			*event.DeliveredToTerminalID != input.TerminalID) {
		return AgentOrchestratorState{}, NewOrchestratorAckError(AckEventOutOfOrder, MsgAckOutOfOrder)
	}
	isDeliveredToCurrentTerminal := event.Status != nil &&
		*event.Status == AgentEventStatusDelivered &&
		event.DeliveredToTerminalID != nil &&
		*event.DeliveredToTerminalID == input.TerminalID
	isInvalidatedDelivered := event.Status != nil &&
		*event.Status == AgentEventStatusInvalidated &&
		event.DeliveredToTerminalID != nil &&
		*event.DeliveredToTerminalID == input.TerminalID

	if !isPendingOrDelivered(event.Status) && !isInvalidatedDelivered {
		return AgentOrchestratorState{}, NewOrchestratorAckError(AckEventInvalidated, MsgAckInvalidated)
	}
	if !isInvalidatedDelivered {
		var eventAgent *AgentIndexRecord
		if event.AgentID != nil {
			eventAgent = s.safeGetAgent(*event.AgentID)
		}
		if eventAgent != nil {
			candidate := event
			if isDeliveredToCurrentTerminal {
				pending := AgentEventStatusPending
				candidate.Status = &pending
			}
			if !s.agentEvents.IsDeliverableAgentEvent(
				candidate, eventAgent, eventScope(event, input.WorkspaceID), input.TerminalID,
			) {
				return AgentOrchestratorState{}, NewOrchestratorAckError(
					AckEventOutOfOrder, MsgAckOutOfOrder)
			}
		}
	}
	next, err := s.nextDeliverableAcrossScopes(input.AgentScope, input.TerminalID, state.AckedEventID)
	if err != nil {
		return AgentOrchestratorState{}, err
	}
	// Only an undelivered (pending) later event blocks an out-of-order ack. If the
	// next candidate is already delivered to this owner, or there is no candidate
	// (acked/invalidated/failed/null), let the batch ack through: markAcked uses an
	// id <= cursor so the intermediate delivered events get acknowledged too. This
	// keeps one stuck delivered event from blocking the whole trailing batch.
	isUnconsumedRetained := next != nil &&
		next.Status != nil && *next.Status == AgentEventStatusInvalidated &&
		next.Deliverable == 1 &&
		(next.DeliveredToTerminalID == nil || *next.DeliveredToTerminalID != input.TerminalID)
	if next != nil &&
		((next.Status != nil && *next.Status == AgentEventStatusPending) || isUnconsumedRetained) &&
		input.EventID > next.ID {
		return AgentOrchestratorState{}, NewOrchestratorAckError(AckEventOutOfOrder, MsgAckOutOfOrder)
	}
	if event.WorkspaceID == nil || *event.WorkspaceID != input.WorkspaceID {
		// Cross-workspace ack (ownerless sibling workspace): settle only this single
		// row and leave the owner's own cursor untouched. A scope-cursor ack would
		// rewrite every `id <= eventId` row of the owner's workspace to acked and
		// advance `acked_event_id` past them, silently swallowing its own events
		// that were never delivered. The settled row needs no redelivery: `listAfter`
		// only scans pending/delivered/retained-invalidated rows.
		if err := s.agentEvents.MarkAcked(input.EventID, nil); err != nil {
			return AgentOrchestratorState{}, err
		}
		current, err := s.scopes.Get(input.AgentScope)
		if err != nil {
			return AgentOrchestratorState{}, err
		}
		if current == nil {
			return *state, nil
		}
		return *current, nil
	}
	if err := s.agentEvents.MarkAcked(input.EventID, &input.AgentScope); err != nil {
		return AgentOrchestratorState{}, err
	}
	return s.scopes.Ack(AckInput{
		AgentOrchestratorScopeKey: input.AgentScope,
		EventID:                   input.EventID,
		TerminalID:                input.TerminalID,
	})
}

// isAckScopeAllowed mirrors #isAckScopeAllowed.
//
// Ack scope guard: the owner's own workspace plus every sibling workspace in the
// session that has no orchestrator owner of its own. Events of a workspace owned
// by another terminal stay out of scope.
func (s *AgentOrchestratorService) isAckScopeAllowed(
	event AgentEventRecord,
	input OrchestratorAckInput,
) (bool, error) {
	if event.WorkspaceID != nil && *event.WorkspaceID == input.WorkspaceID {
		return true, nil
	}
	if event.WorkspaceID == nil {
		return false, nil
	}
	siblings, err := s.OwnerlessSiblingScopes(input.AgentScope)
	if err != nil {
		return false, err
	}
	for _, scope := range siblings {
		if scope.WorkspaceID == *event.WorkspaceID {
			return true, nil
		}
	}
	return false, nil
}

// OrchestratorMoveInput mirrors the TS move input.
type OrchestratorMoveInput struct {
	From       AgentScope
	PaneID     string
	TerminalID string
	To         AgentScope
}

// Move mirrors move.
func (s *AgentOrchestratorService) Move(input OrchestratorMoveInput) ([]AgentOrchestratorChange, error) {
	cursor, err := s.claimCursor(input.To)
	if err != nil {
		return nil, err
	}
	changes, err := s.scopes.MoveOwner(MoveOwnerInput{
		From:               input.From,
		PaneID:             input.PaneID,
		TargetAckedEventID: cursor,
		TerminalID:         input.TerminalID,
		To:                 input.To,
	})
	if err != nil {
		return nil, err
	}
	moved := make([]AgentOrchestratorChange, 0, len(changes))
	for _, change := range changes {
		moved = append(moved, AgentOrchestratorChange{
			Current:  change.Current,
			Previous: change.Previous,
			Reason:   OrchestratorChangeMoved,
		})
	}
	return moved, nil
}

// PersistedOwners mirrors persistedOwners.
func (s *AgentOrchestratorService) PersistedOwners() ([]AgentOrchestratorState, error) {
	return s.scopes.ListOwned()
}

// claimCursor mirrors #claimCursor.
func (s *AgentOrchestratorService) claimCursor(scope AgentScope) (int64, error) {
	current, err := s.scopes.Get(scope)
	// Once a scope row exists, keep its acked cursor even when the owner has been
	// released (owner null): pending events appended during the ownerless gap are
	// then picked up by pending() after the next claim. Only a truly first claim
	// (no row at all) advances to latestEventId to avoid redelivering history.
	if err != nil {
		return 0, err
	}
	if current != nil {
		return current.AckedEventID, nil
	}
	return s.agentEvents.LatestEventID(AgentQueryScope{
		HerdrSessionName: &scope.HerdrSessionName,
		WorkspaceID:      &scope.WorkspaceID,
	})
}

// mergeScopes mirrors mergeScopes: deduplicated, insertion-ordered scopes keyed
// by (session, workspace).
func mergeScopes(scopes []AgentScope) []AgentScope {
	byKey := map[string]AgentScope{}
	order := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		key := scope.HerdrSessionName + "\x00" + scope.WorkspaceID
		if _, seen := byKey[key]; !seen {
			order = append(order, key)
		}
		byKey[key] = AgentScope{
			HerdrSessionName: scope.HerdrSessionName,
			WorkspaceID:      scope.WorkspaceID,
		}
	}
	merged := make([]AgentScope, 0, len(order))
	for _, key := range order {
		merged = append(merged, byKey[key])
	}
	return merged
}

// eventScope mirrors eventScope: the scope an event belongs to; a null workspace
// can never match a scope, so it falls back to the owner's own workspace.
func eventScope(event AgentEventRecord, fallbackWorkspaceID string) AgentScope {
	workspaceID := fallbackWorkspaceID
	if event.WorkspaceID != nil {
		workspaceID = *event.WorkspaceID
	}
	return AgentScope{
		HerdrSessionName: event.HerdrSessionName,
		WorkspaceID:      workspaceID,
	}
}

// agentFor mirrors `event.agentId ? validAgents.get(event.agentId) : undefined`.
func agentFor(validAgents map[string]AgentIndexRecord, agentID *string) *AgentIndexRecord {
	if agentID == nil {
		return nil
	}
	agent, ok := validAgents[*agentID]
	if !ok {
		return nil
	}
	return &agent
}

// isPendingOrDelivered mirrors `status === "pending" || status === "delivered"`.
func isPendingOrDelivered(status *AgentEventStatus) bool {
	return status != nil &&
		(*status == AgentEventStatusPending || *status == AgentEventStatusDelivered)
}
