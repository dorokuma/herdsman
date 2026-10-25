package daemon

import (
	"fmt"
	"os"
	"time"

	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/herdr"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// This file ports src/daemon/agent-event-reconciler.ts.

// ReconcileBatchLimit mirrors RECONCILE_BATCH_LIMIT.
const ReconcileBatchLimit = 100

// ReconcileReason mirrors RECONCILE_REASON.
const ReconcileReason = "PANE_NOT_PRESENT_RECONCILE"

// ScopeReleaseTtlMs mirrors SCOPE_RELEASE_TTL_MS: release rows are ownerless
// scopes; keep them reclaimable for 30 days, then drop them.
const ScopeReleaseTtlMs = 30 * 24 * 60 * 60 * 1000

// ReconcileSettledTtlMs mirrors RECONCILE_SETTLED_TTL_MS: terminal agent events
// (acked/failed) are purged after 7 days in the reconcile cycle.
const ReconcileSettledTtlMs = 7 * 24 * 60 * 60 * 1000

// HistoryCacheTtlMs mirrors HISTORY_CACHE_TTL_MS: history cache rows are purged
// after 30 days regardless of whether the source file still exists.
const HistoryCacheTtlMs = 30 * 24 * 60 * 60 * 1000

// InvalidatedGraceMs mirrors INVALIDATED_GRACE_MS.
//
// Invalidated events are retained this long so an owner that already received
// the event can still ack it (`isInvalidatedDelivered`) and advance the cursor.
// Must be >= REDELIVERY_FRESHNESS_MS; 1 hour covers reconnect delays without
// letting invalidated rows accumulate.
const InvalidatedGraceMs = 60 * 60 * 1000

// eventTimestampLayout is the millisecond ISO layout the TS contracts' Date
// fields serialize to, which is what agent_events.created_at stores.
const eventTimestampLayout = "2006-01-02T15:04:05.000Z"

// AgentEventReconcileResult mirrors AgentEventReconcileResult.
type AgentEventReconcileResult struct {
	HistoryCacheExpired int
	HistoryCacheMissing int
	Invalidated         int
	Purged              int
	Released            int
}

func emptyReconcileResult() AgentEventReconcileResult {
	return AgentEventReconcileResult{}
}

// ConnectedTerminalInput mirrors the connectedTerminal callback input.
type ConnectedTerminalInput struct {
	HerdrSessionName string
	TerminalID       string
}

// HerdrSessionSnapshotClient is the slice of *herdr.HerdrSocketClient the
// reconciler depends on: one pane snapshot and a close.
type HerdrSessionSnapshotClient interface {
	SessionSnapshot() (any, error)
	Close()
}

// HerdrSessionClientFactory builds the client for one Herdr session entry. The
// TS default (`new HerdrSocketClient({ socketPath })`) connects lazily, so a
// dial failure surfaced at the first request; the Go client dials in its
// constructor, so the factory reports the failure and the reconciler treats it
// exactly like a failed snapshot.
type HerdrSessionClientFactory func(
	entry herdr.HerdrSessionListEntry,
) (HerdrSessionSnapshotClient, error)

// AgentEventReconcilerOptions mirrors the TS constructor options. The stores are
// the concrete db stores, exactly like the TS class takes them; the cache and
// the status event plan store are optional (nil means absent).
type AgentEventReconcilerOptions struct {
	AgentHistoryCache *db.AgentHistoryCacheStore
	Events            *db.AgentEventStore
	Scopes            *db.AgentOrchestratorScopeStore
	SessionList       herdr.HerdrSessionListRunner
	ClientFactory     HerdrSessionClientFactory
	ConnectedTerminal func(input ConnectedTerminalInput) bool
	StatusEventPlans  *db.StatusEventPlanStore
}

// livePane mirrors LivePane.
type livePane struct {
	paneID     string
	generation *string
	terminalID *string
}

// AgentEventReconciler ports the TS class of the same name.
type AgentEventReconciler struct {
	agentHistoryCache *db.AgentHistoryCacheStore
	events            *db.AgentEventStore
	scopes            *db.AgentOrchestratorScopeStore
	sessionList       herdr.HerdrSessionListRunner
	clientFactory     HerdrSessionClientFactory
	connectedTerminal func(input ConnectedTerminalInput) bool
	statusEventPlans  *db.StatusEventPlanStore
}

// NewAgentEventReconciler mirrors the TS constructor.
func NewAgentEventReconciler(options AgentEventReconcilerOptions) *AgentEventReconciler {
	clientFactory := options.ClientFactory
	if clientFactory == nil {
		clientFactory = defaultHerdrSessionClientFactory
	}
	connectedTerminal := options.ConnectedTerminal
	if connectedTerminal == nil {
		connectedTerminal = func(ConnectedTerminalInput) bool { return true }
	}
	return &AgentEventReconciler{
		agentHistoryCache: options.AgentHistoryCache,
		events:            options.Events,
		scopes:            options.Scopes,
		sessionList:       options.SessionList,
		clientFactory:     clientFactory,
		connectedTerminal: connectedTerminal,
		statusEventPlans:  options.StatusEventPlans,
	}
}

// defaultHerdrSessionClientFactory mirrors the TS default client factory.
func defaultHerdrSessionClientFactory(
	entry herdr.HerdrSessionListEntry,
) (HerdrSessionSnapshotClient, error) {
	client, err := herdr.NewHerdrSocketClient(herdr.HerdrSocketClientOptions{
		SocketPath: entry.SocketPath,
	})
	if err != nil {
		return nil, err
	}
	return client, nil
}

// ReconcileOptions mirrors `{ releaseStaleOwners?: boolean }`: a nil
// ReleaseStaleOwners means the TS key was absent, which runs the release sweep.
type ReconcileOptions struct {
	ReleaseStaleOwners *bool
}

// Reconcile mirrors reconcile.
func (s *AgentEventReconciler) Reconcile(
	options ReconcileOptions,
) (AgentEventReconcileResult, error) {
	sessions, err := s.sessionList()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"Herdsman reconcile skipped: Herdr session list unavailable: %v\n", err)
		return emptyReconcileResult(), nil
	}
	live := map[string][]livePane{}
	clients := []HerdrSessionSnapshotClient{}
	closeClients := func() {
		for _, client := range clients {
			client.Close()
		}
	}
	for _, entry := range sessions {
		if !entry.Running {
			continue
		}
		client, clientErr := s.clientFactory(entry)
		if clientErr != nil {
			closeClients()
			fmt.Fprintf(os.Stderr,
				"Herdsman reconcile skipped: incomplete Herdr pane snapshot: %v\n", clientErr)
			return emptyReconcileResult(), nil
		}
		clients = append(clients, client)
		snapshotValue, snapshotErr := client.SessionSnapshot()
		if snapshotErr != nil {
			closeClients()
			fmt.Fprintf(os.Stderr,
				"Herdsman reconcile skipped: incomplete Herdr pane snapshot: %v\n", snapshotErr)
			return emptyReconcileResult(), nil
		}
		live[entry.Name] = livePanesFromSnapshot(
			herdr.NormalizeHerdrSessionSnapshot(snapshotValue))
	}
	closeClients()

	invalidated := 0
	var cursor int64
	for {
		candidates, listErr := s.events.ListReconcileCandidates(ReconcileBatchLimit, cursor)
		if listErr != nil {
			return AgentEventReconcileResult{}, listErr
		}
		if len(candidates) == 0 {
			break
		}
		for _, event := range candidates {
			cursor = event.ID
			panes, sessionLive := live[event.HerdrSessionName]
			if !sessionLive {
				continue
			}
			// 存活比对：代际匹配的存活 pane 继续正常保留
			matchingPane := false
			for _, pane := range panes {
				if paneMatchesEvent(pane, event) {
					matchingPane = true
					break
				}
			}
			if matchingPane {
				continue
			}
			// 执行点补完（判别 1）：快照中存在同名 paneId 但 paneMatchesEvent 判定不兼容（属 E3 跨代冲突）
			samePaneExists := false
			for _, pane := range panes {
				if event.PaneID != nil && pane.paneID == *event.PaneID {
					samePaneExists = true
					break
				}
			}
			if samePaneExists {
				// 跨代冲突不能物理删掉尚未投递过的终态失败行：它必须在 reconciler
				// 的下一轮扫描中活下去，直到拿到首次投递机会。
				if isRetainableTerminalFailure(event) {
					changed, markErr := s.events.MarkRetainedInvalidated(event.ID)
					if markErr != nil {
						return AgentEventReconcileResult{}, markErr
					}
					if changed {
						invalidated += 1
					}
				} else {
					deleted, deleteErr := s.events.DeleteReconcileCandidate(event.ID)
					if deleteErr != nil {
						return AgentEventReconcileResult{}, deleteErr
					}
					if deleted {
						invalidated += 1
					}
				}
				continue
			}
			// 执行点补完（判别 2）：paneId 在快照中完全缺失，评估保留资格与时限
			payload := asReconcileRecord(event.Payload)
			now := time.Now().UnixMilli()
			eventAge := now - eventAgeMs(event.CreatedAt, now)
			isFresh := eventAge <= db.RedeliveryFreshnessMs // E6 判定：300s 关页时限
			// 终态失败行不走 300s 新鲜度窗口：它的保留窗口以「首次投递尝试」为界
			// （delivery_attempts = 0），因为这条事件常常是在 pane 关闭（agents 行
			// 物理删除）之后才补写出来的。agent.discarded 不保留。
			isRetainable := isRetainableTerminalFailure(event) ||
				(event.Deliverable == 1 &&
					isFresh &&
					((event.Type == observability.AgentEventDone && payload["from"] == "working") ||
						(event.Type == observability.AgentEventIdle && payload["from"] == "working")))
			if isRetainable {
				// 保留交付性，转移为保留作废态
				if _, markErr := s.events.MarkRetainedInvalidated(event.ID); markErr != nil {
					return AgentEventReconcileResult{}, markErr
				}
				invalidated += 1
			} else {
				deleted, deleteErr := s.events.DeleteReconcileCandidate(event.ID)
				if deleteErr != nil {
					return AgentEventReconcileResult{}, deleteErr
				}
				if deleted {
					invalidated += 1
				}
			}
		}
	}
	invalidatedExpired, err := s.events.DeleteInvalidatedOlderThan(InvalidatedGraceMs)
	if err != nil {
		return AgentEventReconcileResult{}, err
	}
	invalidated += invalidatedExpired
	if _, err := s.events.DeleteSettledOlderThan(ReconcileSettledTtlMs); err != nil {
		return AgentEventReconcileResult{}, err
	}
	// Settled status event plans share the same 7-day TTL as settled agent
	// events: a drained/completed/cancelled/failed plan is pure bookkeeping.
	if s.statusEventPlans != nil {
		if _, err := s.statusEventPlans.DeleteSettledOlderThan(ReconcileSettledTtlMs); err != nil {
			return AgentEventReconcileResult{}, err
		}
	}
	historyCacheExpired, historyCacheMissing, err := s.purgeHistoryCache()
	if err != nil {
		return AgentEventReconcileResult{}, err
	}
	released := 0
	if options.ReleaseStaleOwners == nil || *options.ReleaseStaleOwners {
		ownedScopes, err := s.scopes.ListOwnedScopes()
		if err != nil {
			return AgentEventReconcileResult{}, err
		}
		for _, scope := range ownedScopes {
			panes, sessionLive := live[scope.HerdrSessionName]
			if !sessionLive {
				continue
			}
			livePaneIDs := map[string]struct{}{}
			liveTerminalIDs := map[string]struct{}{}
			for _, pane := range panes {
				livePaneIDs[pane.paneID] = struct{}{}
				if pane.terminalID != nil && s.connectedTerminal(ConnectedTerminalInput{
					HerdrSessionName: scope.HerdrSessionName,
					TerminalID:       *pane.terminalID,
				}) {
					liveTerminalIDs[*pane.terminalID] = struct{}{}
				}
			}
			scopeReleased, releaseErr := s.scopes.ReleaseIfStaleOwner(db.ReleaseIfStaleOwnerInput{
				AgentOrchestratorScopeKey: scope.AgentScope,
				LivePaneIDs:               livePaneIDs,
				LiveTerminalIDs:           liveTerminalIDs,
			})
			if releaseErr != nil {
				return AgentEventReconcileResult{}, releaseErr
			}
			if scopeReleased {
				released += 1
			}
		}
	}
	purged, err := s.scopes.PurgeReleasedOlderThan(ScopeReleaseTtlMs)
	if err != nil {
		return AgentEventReconcileResult{}, err
	}
	ownedScopes, err := s.scopes.ListOwnedScopes()
	if err != nil {
		return AgentEventReconcileResult{}, err
	}
	for _, scope := range ownedScopes {
		if scope.Owner != nil && scope.Owner.PaneID != "" {
			if _, err := s.events.AckSelfOwned(db.AckSelfOwnedInput{
				HerdrSessionName: scope.HerdrSessionName,
				PaneID:           scope.Owner.PaneID,
				WorkspaceID:      scope.WorkspaceID,
			}); err != nil {
				return AgentEventReconcileResult{}, err
			}
		}
	}
	return AgentEventReconcileResult{
		HistoryCacheExpired: historyCacheExpired,
		HistoryCacheMissing: historyCacheMissing,
		Invalidated:         invalidated,
		Purged:              purged,
		Released:            released,
	}, nil
}

// livePanesFromSnapshot mirrors the snapshot.panes.flatMap of the TS reconcile:
// every pane object that reports a non-empty pane_id becomes one live pane.
func livePanesFromSnapshot(snapshot herdr.HerdrSessionSnapshot) []livePane {
	panes := []livePane{}
	for _, value := range snapshot.Panes {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		paneID := reconcileStringValue(item["pane_id"])
		if paneID == nil {
			continue
		}
		panes = append(panes, livePane{
			paneID:     *paneID,
			generation: reconcileStringValue(item["pane_generation"]),
			terminalID: reconcileStringValue(item["terminal_id"]),
		})
	}
	return panes
}

// reconcileStringValue mirrors stringValue: a non-empty string, else nil.
func reconcileStringValue(value any) *string {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	return &text
}

// asReconcileRecord mirrors `(event.payload ?? {}) as { from?: string }`: a
// non-object payload degrades to an empty record.
func asReconcileRecord(value any) map[string]any {
	if record, ok := value.(map[string]any); ok {
		return record
	}
	return map[string]any{}
}

// eventAgeMs mirrors `Date.now() - event.createdAt.getTime()`. A timestamp that
// cannot be parsed cannot be ordered, so the event counts as brand new rather
// than as infinitely old; the stored value always comes from the ISO layout,
// which makes this defensive only.
func eventAgeMs(createdAt string, now int64) int64 {
	parsed, err := time.Parse(eventTimestampLayout, createdAt)
	if err != nil {
		return 0
	}
	return now - parsed.UnixMilli()
}

// isRetainableTerminalFailure mirrors isRetainableTerminalFailure.
//
// A terminal `agent.failed` row that has never been delivered must survive the
// reconcile sweep: when a plan reaches its failed state after the pane was
// retired, the event is written with `agent_id = null` and no live pane to match
// it, so the ordinary 300s freshness rule would physically delete the only
// remaining evidence of the failure before any orchestrator could see it. The
// window is bounded by the first delivery attempt (`delivery_attempts = 0`),
// which is paired with the retention whitelist in `#invalidatePaneCore` and with
// the invalidation gate in `AgentEventStore#reclaimDelivered`. `agent.discarded`
// is deliberately not retained.
func isRetainableTerminalFailure(event observability.AgentEventRecord) bool {
	return event.Type == observability.AgentEventFailed &&
		event.Deliverable == 1 &&
		event.DeliveryAttempts != nil &&
		*event.DeliveryAttempts == 0
}

// historyCacheSourceExists mirrors historyCacheSourceExists: whether the history
// cache's backing file still exists. Cache keys are plain file paths now (agy's
// conversation database, pi's session jsonl).
func historyCacheSourceExists(sourcePath string) bool {
	_, err := os.Stat(sourcePath)
	return err == nil
}

// paneMatchesEvent mirrors paneMatchesEvent.
//
// A live pane matches an event when the pane ids are equal and the generations
// do not contradict each other. A generation-less live pane is treated as alive
// (the pane is present in the snapshot, so its events must not be swept even
// though the pane reports no generation); only two different non-null
// generations mean the pane was re-created and the old generation's events are
// stale.
func paneMatchesEvent(pane livePane, event observability.AgentEventRecord) bool {
	if event.PaneID == nil || pane.paneID != *event.PaneID {
		return false
	}
	if event.PaneGeneration == nil {
		return true
	}
	if pane.generation == nil {
		fmt.Fprintf(os.Stderr,
			"Herdsman reconcile treating generation-less live pane as alive"+
				" (eventId=%d eventPaneGeneration=%s herdrSessionName=%s paneId=%s)\n",
			event.ID, *event.PaneGeneration, event.HerdrSessionName, pane.paneID)
		return true
	}
	return *pane.generation == *event.PaneGeneration
}

// purgeHistoryCache mirrors #purgeHistoryCache.
func (s *AgentEventReconciler) purgeHistoryCache() (int, int, error) {
	if s.agentHistoryCache == nil {
		return 0, 0, nil
	}
	historyCacheExpired, err := s.agentHistoryCache.DeleteOlderThan(HistoryCacheTtlMs)
	if err != nil {
		return 0, 0, err
	}
	sourcePaths, err := s.agentHistoryCache.ListSourcePaths()
	if err != nil {
		return 0, 0, err
	}
	missing := []string{}
	for _, sourcePath := range sourcePaths {
		if !historyCacheSourceExists(sourcePath) {
			missing = append(missing, sourcePath)
		}
	}
	historyCacheMissing, err := s.agentHistoryCache.DeleteBySourcePaths(missing)
	if err != nil {
		return 0, 0, err
	}
	return historyCacheExpired, historyCacheMissing, nil
}
