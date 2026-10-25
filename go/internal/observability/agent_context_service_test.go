package observability_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/agenthistory"
	"github.com/dorokuma/herdsman/go/internal/db"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// The port of test/integration/agent-context-service.test.ts.
//
// The harness mirrors openObservabilityDbHarness: a migrated database in a temp
// dir with the running "default" Herdr session, the agent index and the context
// snapshot store. The history service is the fake the TS tests use, which
// records the options of every resolveCompactHistory call and delegates the
// fingerprint stat to the real agent-history module. Every test uses
// t.TempDir(), so the production home and the live daemon are never touched.

const contextTestSession = "default"

// contextAgentInput mirrors the openAgent / refreshAgent input.
type contextAgentInput struct {
	agentSession map[string]any
	revision     *int
}

type contextHarness struct {
	sqlite    *sql.DB
	agents    *db.AgentStore
	snapshots *db.AgentContextSnapshotStore
	agent     observability.AgentIndexRecord
}

func openContextHarness(t *testing.T) *contextHarness {
	t.Helper()
	sqlite, err := db.OpenSqlite(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("openSqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	if err := db.ApplyMigrations(sqlite); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	sessions := db.NewHerdrSessionStore(sqlite)
	if _, err := sessions.UpsertRunning(db.UpsertRunningHerdrSessionInput{
		Name:       contextTestSession,
		SessionDir: "/tmp/herdr",
		SocketPath: "/tmp/herdr.sock",
	}); err != nil {
		t.Fatalf("upsertRunning: %v", err)
	}
	agentEvents, err := db.NewAgentEventStore(sqlite)
	if err != nil {
		t.Fatalf("newAgentEventStore: %v", err)
	}
	return &contextHarness{
		sqlite:    sqlite,
		agents:    db.NewAgentStore(sqlite, agentEvents),
		snapshots: db.NewAgentContextSnapshotStore(sqlite),
	}
}

// openContextAgent mirrors openAgent: one indexed agent on pane wB:p1.
func openContextAgent(t *testing.T, input contextAgentInput) *contextHarness {
	t.Helper()
	harness := openContextHarness(t)
	harness.agent = replaceContextAgent(t, harness, input)
	return harness
}

// replaceContextAgent mirrors refreshAgent: the same pane re-reported with a new
// revision (and possibly a new official session ref).
func replaceContextAgent(
	t *testing.T,
	harness *contextHarness,
	input contextAgentInput,
) observability.AgentIndexRecord {
	t.Helper()
	revision := 1
	if input.revision != nil {
		revision = *input.revision
	}
	agent := map[string]any{
		"agent":        "pi",
		"agent_status": "working",
		"pane_id":      "wB:p1",
		"revision":     float64(revision),
		"terminal_id":  "term_pi",
		"workspace_id": "wB",
	}
	if input.agentSession != nil {
		agent["agent_session"] = input.agentSession
	}
	agents, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents:           []db.HerdrAgentLike{agent},
		HerdrSessionName: contextTestSession,
	})
	if err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	if len(agents) == 0 {
		t.Fatal("Expected agent")
	}
	return agents[0]
}

// contextSourceFile mirrors the source / allowedSession helpers: a readable
// session file plus its fingerprint and the ref discovery would resolve.
func contextSourceFile(t *testing.T, dir, name string) sessionSource {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("history\n"), 0o600); err != nil {
		t.Fatalf("writeFile(%s): %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat(%s): %v", path, err)
	}
	return sessionSource{
		fingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: float64(info.ModTime().UnixMilli()),
			Path:    path,
			Size:    float64(info.Size()),
		},
		ref: &observability.AgentHistoryRef{
			Kind:   observability.HistoryRefAgentSession,
			Path:   &path,
			Source: observability.HistorySourcePiJSONL,
			Value:  path,
		},
	}
}

type sessionSource struct {
	fingerprint *observability.AgentHistorySourceFingerprint
	ref         *observability.AgentHistoryRef
}

// contextSource mirrors source().
func contextSource(t *testing.T, name string) sessionSource {
	t.Helper()
	return contextSourceFile(t, t.TempDir(), name)
}

// contextPiSession mirrors a source() whose file the real pi reader can
// actually resolve: a well-formed JSONL session carrying one assistant message.
// contextSourceFile's "history\n" payload is only ever read through the fake, so
// a test that drives the real agent-history service needs this shape instead.
func contextPiSession(t *testing.T, name string) sessionSource {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	payload := strings.Join([]string{
		`{"type":"session","id":"s1"}`,
		`{"type":"message","id":"u1","timestamp":"2026-07-09T12:00:00.000Z",` +
			`"message":{"role":"user","content":"please inspect"}}`,
		`{"type":"message","id":"a1","timestamp":"2026-07-09T12:00:02.000Z",` +
			`"message":{"role":"assistant","content":[{"type":"text","text":"done"}],` +
			`"stopReason":"stop"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("writeFile(%s): %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat(%s): %v", path, err)
	}
	return sessionSource{
		fingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: float64(info.ModTime().UnixMilli()),
			Path:    path,
			Size:    float64(info.Size()),
		},
		ref: &observability.AgentHistoryRef{
			Kind:   observability.HistoryRefAgentSession,
			Path:   &path,
			Source: observability.HistorySourcePiJSONL,
			Value:  path,
		},
	}
}

// contextHistory mirrors history(): the compact history the fake resolves.
func contextHistory(ref *observability.AgentHistoryRef, text string) observability.CompactAgentHistory {
	compact := observability.CompactAgentHistory{}
	if ref == nil {
		return compact
	}
	source := string(ref.Source)
	compact.Source = &source
	compact.HistoryRef = ref
	compact.LastAssistantMessage = &observability.AgentHistoryExcerpt{Ref: "entry", Text: text}
	return compact
}

// contextResolved mirrors resolved().
func contextResolved(
	ref *observability.AgentHistoryRef,
	fingerprint *observability.AgentHistorySourceFingerprint,
) observability.AgentHistoryResolved {
	return observability.AgentHistoryResolved{
		CompactHistory:    contextHistory(ref, "done"),
		HistoryRef:        ref,
		SourceFingerprint: fingerprint,
	}
}

// contextSnapshotInput mirrors snapshotInput().
func contextSnapshotInput(
	agent observability.AgentIndexRecord,
	value observability.AgentHistoryResolved,
) observability.AgentContextSnapshotInput {
	return observability.AgentContextSnapshotInput{
		AgentID:           agent.ID,
		CompactHistory:    value.CompactHistory,
		HistoryRef:        value.HistoryRef,
		PaneRevision:      agent.PaneRevision,
		SourceFingerprint: value.SourceFingerprint,
	}
}

// fakeHistoryService mirrors fakeHistory: it records the options of every call
// and returns one fixed resolution.
type fakeHistoryService struct {
	calls  []observability.AgentHistoryResolveOptions
	result observability.AgentHistoryResolved
}

func newFakeHistoryService(
	result observability.AgentHistoryResolved,
) *fakeHistoryService {
	return &fakeHistoryService{result: result}
}

func (f *fakeHistoryService) ResolveCompactHistory(
	_ observability.AgentHistoryLookupInput,
	options observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	f.calls = append(f.calls, options)
	return f.result
}

// StatSourceFingerprint forwards to the real agent-history module, exactly like
// the TS service calls the module function directly.
func (f *fakeHistoryService) StatSourceFingerprint(
	path string,
) *observability.AgentHistorySourceFingerprint {
	fingerprint := agenthistory.StatSourceFingerprint(path)
	if fingerprint == nil {
		return nil
	}
	return &observability.AgentHistorySourceFingerprint{
		MtimeMs: fingerprint.MtimeMs,
		Path:    fingerprint.Path,
		Size:    float64(fingerprint.Size),
	}
}

func (f *fakeHistoryService) HistorySourceFromSessionRef(
	ref observability.AgentSessionRef,
) observability.AgentHistoryRefSource {
	return observability.AgentHistoryRefSource(
		agenthistory.HistorySourceFromSessionRef(ref))
}

func (f *fakeHistoryService) SafeOfficialSessionPath(value string) string {
	return agenthistory.SafeOfficialSessionPath(value)
}

func (f *fakeHistoryService) SafeAllowedSessionPath(value string) string {
	return agenthistory.SafeAllowedSessionPath(value, nil)
}

func (f *fakeHistoryService) SessionPathAllowedByShape(value string) bool {
	return agenthistory.SessionPathAllowedByShape(value, nil)
}

// realHistoryService is the same AgentHistoryServiceLike surface, but backed by
// the production agent-history service instead of a fixed answer. It records the
// options of every resolve so the occupied-change assertions below can still see
// what the context service asked for, while the discovery, the fingerprint stat
// and the compact-history read all run through the real module.
//
// It exists because internal/agenthistory imports internal/observability, so the
// records cannot be shared Go types and the mapping the production adapter in
// internal/daemon performs has to be repeated here. Only the records the context
// service actually round-trips are mapped.
type realHistoryService struct {
	service *agenthistory.AgentHistoryService
	mu      sync.Mutex
	calls   []observability.AgentHistoryResolveOptions
}

func newRealHistoryService() *realHistoryService {
	return &realHistoryService{
		service: agenthistory.CreateAgentHistoryService(agenthistory.AgentHistoryServiceOptions{}),
	}
}

func (r *realHistoryService) recordedCalls() []observability.AgentHistoryResolveOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]observability.AgentHistoryResolveOptions(nil), r.calls...)
}

func (r *realHistoryService) ResolveCompactHistory(
	input observability.AgentHistoryLookupInput,
	options observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	r.mu.Lock()
	r.calls = append(r.calls, options)
	r.mu.Unlock()

	forceRefresh := false
	if options.ForceRefresh != nil {
		forceRefresh = *options.ForceRefresh
	}
	resolved := r.service.ResolveCompactHistory(
		agenthistory.AgentHistoryLookupInput{
			Agent:            input.Agent,
			AgentSession:     input.AgentSession,
			HerdrSessionName: optionalContextString(input.HerdrSessionName),
			PaneID:           optionalContextString(input.PaneID),
		},
		agenthistory.ResolveCompactOptions{
			ForceDiscovery: options.ForceDiscovery,
			ForceRefresh:   forceRefresh,
			PreferredRef:   contextHistoryRef(options.PreferredRef),
		},
	)
	return observability.AgentHistoryResolved{
		CompactHistory:    realCompactHistory(resolved.CompactHistory),
		HistoryRef:        realHistoryRef(resolved.HistoryRef),
		SourceFingerprint: realFingerprint(resolved.SourceFingerprint),
	}
}

func (r *realHistoryService) StatSourceFingerprint(
	path string,
) *observability.AgentHistorySourceFingerprint {
	return realFingerprint(agenthistory.StatSourceFingerprint(path))
}

func (r *realHistoryService) HistorySourceFromSessionRef(
	ref observability.AgentSessionRef,
) observability.AgentHistoryRefSource {
	return observability.AgentHistoryRefSource(
		agenthistory.HistorySourceFromSessionRef(ref))
}

func (r *realHistoryService) SafeOfficialSessionPath(value string) string {
	return agenthistory.SafeOfficialSessionPath(value)
}

func (r *realHistoryService) SafeAllowedSessionPath(value string) string {
	return agenthistory.SafeAllowedSessionPath(value, nil)
}

func (r *realHistoryService) SessionPathAllowedByShape(value string) bool {
	return agenthistory.SessionPathAllowedByShape(value, nil)
}

// optionalContextString maps "" to "absent", the TS `undefined`: the
// observability records carry herdrSessionName and paneId as plain strings while
// agenthistory keeps them optional.
func optionalContextString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func realHistoryRef(ref *agenthistory.HistoryRef) *observability.AgentHistoryRef {
	if ref == nil {
		return nil
	}
	return &observability.AgentHistoryRef{
		Kind:   observability.AgentHistoryRefKind(ref.Kind),
		Path:   ref.Path,
		Source: observability.AgentHistoryRefSource(ref.Source),
		Value:  ref.Value,
	}
}

// contextHistoryRef is the reverse mapping: the observability ref the context
// service hands back as a preferred ref, expressed as the agenthistory record
// the real service expects.
func contextHistoryRef(ref *observability.AgentHistoryRef) *agenthistory.HistoryRef {
	if ref == nil {
		return nil
	}
	return &agenthistory.HistoryRef{
		Kind:   agenthistory.HistoryKind(ref.Kind),
		Path:   ref.Path,
		Source: agenthistory.HistorySource(ref.Source),
		Value:  ref.Value,
	}
}

func realFingerprint(
	fingerprint *agenthistory.SourceFingerprint,
) *observability.AgentHistorySourceFingerprint {
	if fingerprint == nil {
		return nil
	}
	return &observability.AgentHistorySourceFingerprint{
		MtimeMs: fingerprint.MtimeMs,
		Path:    fingerprint.Path,
		Size:    float64(fingerprint.Size),
	}
}

func realExcerpt(excerpt *agenthistory.HistoryExcerpt) *observability.AgentHistoryExcerpt {
	if excerpt == nil {
		return nil
	}
	return &observability.AgentHistoryExcerpt{
		Ref:        excerpt.Ref,
		StopReason: excerpt.StopReason,
		Text:       excerpt.Text,
		Timestamp:  excerpt.Timestamp,
	}
}

func realToolResult(
	result *agenthistory.CompactToolResult,
) *observability.CompactToolResult {
	if result == nil {
		return nil
	}
	return &observability.CompactToolResult{
		Compaction: observability.CompactToolResultCompaction{
			Mode:          observability.CompactionMode(result.Compaction.Mode),
			OriginalChars: result.Compaction.OriginalChars,
			ReturnedChars: result.Compaction.ReturnedChars,
		},
		IsError:  result.IsError,
		Ref:      result.Ref,
		Text:     result.Text,
		ToolName: result.ToolName,
	}
}

func realUnreadyReason(
	reason *agenthistory.UnreadyReason,
) *observability.AgentHistoryUnreadyReason {
	if reason == nil {
		return nil
	}
	value := observability.AgentHistoryUnreadyReason(*reason)
	return &value
}

func realCompactHistory(
	history agenthistory.CompactAgentHistory,
) observability.CompactAgentHistory {
	return observability.CompactAgentHistory{
		HistoryRef:           realHistoryRef(history.HistoryRef),
		LastAssistantMessage: realExcerpt(history.LastAssistantMessage),
		LastToolResult:       realToolResult(history.LastToolResult),
		LastUserMessage:      realExcerpt(history.LastUserMessage),
		MessageCount:         history.MessageCount,
		Source:               history.Source,
		UnreadyReason:        realUnreadyReason(history.UnreadyReason),
		UpdatedAt:            history.UpdatedAt,
	}
}

// historyMethodsThrow mirrors the TS Proxy: every method panics, so a cached
// read that touches the history service fails the test.
type historyMethodsThrow struct{}

func (historyMethodsThrow) ResolveCompactHistory(
	_ observability.AgentHistoryLookupInput,
	_ observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	panic("history must not run")
}

func (historyMethodsThrow) StatSourceFingerprint(
	_ string,
) *observability.AgentHistorySourceFingerprint {
	panic("history must not run")
}

func (historyMethodsThrow) HistorySourceFromSessionRef(
	_ observability.AgentSessionRef,
) observability.AgentHistoryRefSource {
	panic("history must not run")
}

func (historyMethodsThrow) SafeOfficialSessionPath(_ string) string {
	panic("history must not run")
}

func (historyMethodsThrow) SafeAllowedSessionPath(_ string) string {
	panic("history must not run")
}

func (historyMethodsThrow) SessionPathAllowedByShape(_ string) bool {
	panic("history must not run")
}

// newContextService mirrors context().
func newContextService(
	t *testing.T,
	harness *contextHarness,
	history observability.AgentHistoryServiceLike,
) *observability.AgentContextService {
	t.Helper()
	return observability.NewAgentContextService(struct {
		History observability.AgentHistoryServiceLike
		Stores  struct {
			AgentContextSnapshots observability.AgentContextSnapshotStoreLike
			Agents                observability.AgentContextAgentStoreLike
		}
	}{
		History: history,
		Stores: struct {
			AgentContextSnapshots observability.AgentContextSnapshotStoreLike
			Agents                observability.AgentContextAgentStoreLike
		}{
			AgentContextSnapshots: harness.snapshots,
			Agents:                harness.agents,
		},
	})
}

type expectedResolveCall struct {
	forceDiscovery bool
	preferredRef   *observability.AgentHistoryRef
}

// requireResolveCalls asserts the recorded options objects. The TS tests compare
// the whole object, so a call that carries a forceRefresh (the TS key is absent
// in every case here) fails.
func requireResolveCalls(
	t *testing.T,
	calls []observability.AgentHistoryResolveOptions,
	want ...expectedResolveCall,
) {
	t.Helper()
	if len(calls) != len(want) {
		t.Fatalf("resolve calls = %d (%+v), want %d", len(calls), calls, len(want))
	}
	for index, expected := range want {
		call := calls[index]
		if call.ForceDiscovery != expected.forceDiscovery {
			t.Fatalf("call %d forceDiscovery = %v, want %v",
				index, call.ForceDiscovery, expected.forceDiscovery)
		}
		if call.ForceRefresh != nil {
			t.Fatalf("call %d forceRefresh = %v, want absent", index, *call.ForceRefresh)
		}
		if !sameHistoryRefValue(call.PreferredRef, expected.preferredRef) {
			t.Fatalf("call %d preferredRef = %+v, want %+v",
				index, call.PreferredRef, expected.preferredRef)
		}
	}
}

func sameHistoryRefValue(left, right *observability.AgentHistoryRef) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return reflect.DeepEqual(*left, *right)
}

func requireRevision(t *testing.T, revision *int, want int) {
	t.Helper()
	if revision == nil || *revision != want {
		t.Fatalf("paneRevision = %v, want %d", revision, want)
	}
}

func TestAgentContextServiceUsesDiscoveryForMissingSnapshots(t *testing.T) {
	current := contextSource(t, "current.jsonl")
	harness := openContextAgent(t, contextAgentInput{})
	fake := newFakeHistoryService(contextResolved(current.ref, current.fingerprint))
	service := newContextService(t, harness, fake)

	result, err := service.RefreshAgent(observability.RefreshAgentContextInput{
		Agent:           harness.agent,
		IdentityChanged: false,
	})
	if err != nil {
		t.Fatalf("refreshAgent: %v", err)
	}
	if !result.Changed {
		t.Fatal("changed = false, want true")
	}
	if !sameHistoryRefValue(result.Snapshot.HistoryRef, current.ref) {
		t.Fatalf("snapshot historyRef = %+v, want %+v", result.Snapshot.HistoryRef, current.ref)
	}
	// Discovery still runs for this round (the fake was called at all), but it is
	// not *forced*: an unresolved pane must not keep the failure memory in
	// `discovery.ts` permanently fresh, otherwise its retry window could never
	// expire and the background deceleration would never apply.
	requireResolveCalls(t, fake.calls, expectedResolveCall{forceDiscovery: false})

	empty := contextResolved(nil, nil)
	stored, err := harness.snapshots.Put(contextSnapshotInput(harness.agent, empty))
	if err != nil {
		t.Fatalf("put empty snapshot: %v", err)
	}
	if stored.HistoryRef != nil {
		t.Fatalf("stored historyRef = %+v, want nil", stored.HistoryRef)
	}
	retry := newFakeHistoryService(contextResolved(current.ref, current.fingerprint))
	if _, err := newContextService(t, harness, retry).RefreshAgent(
		observability.RefreshAgentContextInput{
			Agent:           harness.agent,
			IdentityChanged: false,
		},
	); err != nil {
		t.Fatalf("refreshAgent (retry): %v", err)
	}
	requireResolveCalls(t, retry.calls, expectedResolveCall{forceDiscovery: false})
}

func TestAgentContextServiceReusesStoredRefForUpwardRevisions(t *testing.T) {
	changed := contextSource(t, "changed.jsonl")
	harness := openContextAgent(t, contextAgentInput{revision: intPointer(1)})
	changedFingerprint := *changed.fingerprint
	changedFingerprint.Size = 0
	if _, err := harness.snapshots.Put(observability.AgentContextSnapshotInput{
		AgentID:           harness.agent.ID,
		CompactHistory:    contextResolved(changed.ref, &changedFingerprint).CompactHistory,
		HistoryRef:        changed.ref,
		PaneRevision:      intPointer(1),
		SourceFingerprint: &changedFingerprint,
	}); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	fake := newFakeHistoryService(contextResolved(changed.ref, changed.fingerprint))
	refreshed := replaceContextAgent(t, harness, contextAgentInput{revision: intPointer(2)})

	if _, err := newContextService(t, harness, fake).RefreshAgent(
		observability.RefreshAgentContextInput{
			Agent:           refreshed,
			IdentityChanged: false,
		},
	); err != nil {
		t.Fatalf("refreshAgent: %v", err)
	}
	requireResolveCalls(t, fake.calls, expectedResolveCall{
		forceDiscovery: false,
		preferredRef:   changed.ref,
	})

	unchanged := contextSource(t, "unchanged.jsonl")
	unchangedHarness := openContextAgent(t, contextAgentInput{revision: intPointer(2)})
	if _, err := unchangedHarness.snapshots.Put(contextSnapshotInput(
		unchangedHarness.agent, contextResolved(unchanged.ref, unchanged.fingerprint),
	)); err != nil {
		t.Fatalf("put unchanged snapshot: %v", err)
	}
	unchangedFake := newFakeHistoryService(
		contextResolved(unchanged.ref, unchanged.fingerprint))
	if _, err := newContextService(t, unchangedHarness, unchangedFake).RefreshAgent(
		observability.RefreshAgentContextInput{
			Agent:           unchangedHarness.agent,
			IdentityChanged: false,
		},
	); err != nil {
		t.Fatalf("refreshAgent (unchanged): %v", err)
	}
	requireResolveCalls(t, unchangedFake.calls, expectedResolveCall{
		forceDiscovery: false,
		preferredRef:   unchanged.ref,
	})
}

func TestAgentContextServiceForcesDiscovery(t *testing.T) {
	original := contextSource(t, "original.jsonl")
	cases := []struct {
		identityChanged bool
		revision        int
		fingerprint     *observability.AgentHistorySourceFingerprint
	}{
		{identityChanged: false, revision: 2, fingerprint: original.fingerprint},
		{identityChanged: false, revision: 0, fingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: original.fingerprint.MtimeMs,
			Path:    original.fingerprint.Path,
			Size:    0,
		}},
		{identityChanged: true, revision: 1, fingerprint: original.fingerprint},
		{identityChanged: false, revision: 2, fingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: original.fingerprint.MtimeMs,
			Path:    filepath.Join(t.TempDir(), "removed-history.jsonl"),
			Size:    original.fingerprint.Size,
		}},
	}
	for index, item := range cases {
		harness := openContextAgent(t, contextAgentInput{revision: intPointer(1)})
		if _, err := harness.snapshots.Put(contextSnapshotInput(
			harness.agent, contextResolved(original.ref, item.fingerprint),
		)); err != nil {
			t.Fatalf("case %d put snapshot: %v", index, err)
		}
		fake := newFakeHistoryService(contextResolved(original.ref, original.fingerprint))
		agent := replaceContextAgent(t, harness, contextAgentInput{revision: intPointer(item.revision)})

		if _, err := newContextService(t, harness, fake).RefreshAgent(
			observability.RefreshAgentContextInput{
				Agent:           agent,
				IdentityChanged: item.identityChanged,
			},
		); err != nil {
			t.Fatalf("case %d refreshAgent: %v", index, err)
		}
		if len(fake.calls) == 0 {
			t.Fatalf("case %d recorded no resolve call", index)
		}
		if !fake.calls[0].ForceDiscovery {
			t.Fatalf("case %d forceDiscovery = false, want true", index)
		}
	}
}

func TestAgentContextServiceDoesNotPreferUnreadablePathSession(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-pref.jsonl")
	session := map[string]any{"agent": "pi", "kind": "path", "source": "pi", "value": missing}
	harness := openContextAgent(t, contextAgentInput{agentSession: session, revision: intPointer(1)})
	fake := newFakeHistoryService(contextResolved(nil, nil))

	if _, err := newContextService(t, harness, fake).RefreshAgent(
		observability.RefreshAgentContextInput{
			Agent:           harness.agent,
			IdentityChanged: true,
		},
	); err != nil {
		t.Fatalf("refreshAgent: %v", err)
	}
	// An unreadable official path is an unresolved pane: nothing to reuse, and no
	// forced lookup either (the failure memory owns that cadence). What matters
	// here is that no persisted ref is handed to discovery.
	requireResolveCalls(t, fake.calls, expectedResolveCall{forceDiscovery: false})
}

func TestAgentContextServicePrefersAuthoritativeSessionRef(t *testing.T) {
	oldSource := contextSource(t, "old.jsonl")
	authoritative := contextSource(t, "authoritative.jsonl")
	authoritativePath := *authoritative.ref.Path
	session := map[string]any{
		"agent":  "pi",
		"kind":   "path",
		"source": "pi",
		"value":  authoritative.ref.Value,
	}
	harness := openContextAgent(t, contextAgentInput{revision: intPointer(1)})
	if _, err := harness.snapshots.Put(contextSnapshotInput(
		harness.agent, contextResolved(oldSource.ref, oldSource.fingerprint),
	)); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	fake := newFakeHistoryService(contextResolved(authoritative.ref, authoritative.fingerprint))
	changedIdentity := replaceContextAgent(t, harness, contextAgentInput{
		agentSession: session,
		revision:     intPointer(2),
	})

	if _, err := newContextService(t, harness, fake).RefreshAgent(
		observability.RefreshAgentContextInput{
			Agent:           changedIdentity,
			IdentityChanged: true,
		},
	); err != nil {
		t.Fatalf("refreshAgent: %v", err)
	}
	requireResolveCalls(t, fake.calls, expectedResolveCall{
		forceDiscovery: false,
		preferredRef: &observability.AgentHistoryRef{
			Kind:   observability.HistoryRefAgentSession,
			Path:   &authoritativePath,
			Source: observability.HistorySourcePiJSONL,
			Value:  authoritative.ref.Value,
		},
	})

	retained := openContextAgent(t, contextAgentInput{
		agentSession: session,
		revision:     intPointer(1),
	})
	authoritativeRef := &observability.AgentHistoryRef{
		Kind:   observability.HistoryRefAgentSession,
		Path:   &authoritativePath,
		Source: observability.HistorySourcePiJSONL,
		Value:  authoritative.ref.Value,
	}
	if _, err := retained.snapshots.Put(contextSnapshotInput(
		retained.agent, contextResolved(authoritativeRef, authoritative.fingerprint),
	)); err != nil {
		t.Fatalf("put retained snapshot: %v", err)
	}
	retainedFake := newFakeHistoryService(
		contextResolved(authoritativeRef, authoritative.fingerprint))
	revised := replaceContextAgent(t, retained, contextAgentInput{
		agentSession: session,
		revision:     intPointer(2),
	})
	if _, err := newContextService(t, retained, retainedFake).RefreshAgent(
		observability.RefreshAgentContextInput{
			Agent:           revised,
			IdentityChanged: false,
		},
	); err != nil {
		t.Fatalf("refreshAgent (retained): %v", err)
	}
	requireResolveCalls(t, retainedFake.calls, expectedResolveCall{
		forceDiscovery: false,
		preferredRef:   authoritativeRef,
	})
}

func TestAgentContextServiceReusesResolvedPathForSameID(t *testing.T) {
	firstSource := contextSource(t, "id-first.jsonl")
	firstPath := *firstSource.ref.Path
	firstSession := map[string]any{"agent": "pi", "kind": "id", "source": "herdr:pi", "value": "session-1"}
	firstRef := &observability.AgentHistoryRef{
		Kind:   observability.HistoryRefAgentSession,
		Path:   &firstPath,
		Source: observability.HistorySourcePiJSONL,
		Value:  "session-1",
	}
	harness := openContextAgent(t, contextAgentInput{
		agentSession: firstSession,
		revision:     intPointer(1),
	})
	if _, err := harness.snapshots.Put(contextSnapshotInput(
		harness.agent, contextResolved(firstRef, firstSource.fingerprint),
	)); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	reused := newFakeHistoryService(contextResolved(firstRef, firstSource.fingerprint))
	revised := replaceContextAgent(t, harness, contextAgentInput{
		agentSession: firstSession,
		revision:     intPointer(2),
	})

	if _, err := newContextService(t, harness, reused).RefreshAgent(
		observability.RefreshAgentContextInput{
			Agent:           revised,
			IdentityChanged: false,
		},
	); err != nil {
		t.Fatalf("refreshAgent: %v", err)
	}
	requireResolveCalls(t, reused.calls, expectedResolveCall{
		forceDiscovery: false,
		preferredRef:   firstRef,
	})

	secondSource := contextSource(t, "id-second.jsonl")
	secondSession := map[string]any{
		"agent":  "pi",
		"kind":   "id",
		"source": "herdr:pi",
		"value":  "session-2",
	}
	discovered := contextResolved(secondSource.ref, secondSource.fingerprint)
	changed := newFakeHistoryService(discovered)
	replaced := replaceContextAgent(t, harness, contextAgentInput{
		agentSession: secondSession,
		revision:     intPointer(3),
	})
	result, err := newContextService(t, harness, changed).RefreshAgent(
		observability.RefreshAgentContextInput{
			Agent:           replaced,
			IdentityChanged: true,
		},
	)
	if err != nil {
		t.Fatalf("refreshAgent (changed id): %v", err)
	}
	// The official id changed, so no persisted ref matches it: discovery runs
	// (new key, nothing memoized) but the round does not force, because an id
	// pane is always resolved by the deterministic template anyway.
	requireResolveCalls(t, changed.calls, expectedResolveCall{forceDiscovery: false})
	wantRef := &observability.AgentHistoryRef{
		Kind:   observability.HistoryRefAgentSession,
		Path:   secondSource.ref.Path,
		Source: observability.HistorySourcePiJSONL,
		Value:  "session-2",
	}
	if !sameHistoryRefValue(result.Snapshot.HistoryRef, wantRef) {
		t.Fatalf("snapshot historyRef = %+v, want %+v", result.Snapshot.HistoryRef, wantRef)
	}
	if !sameHistoryRefValue(result.Snapshot.CompactHistory.HistoryRef, wantRef) {
		t.Fatalf("compact historyRef = %+v, want %+v",
			result.Snapshot.CompactHistory.HistoryRef, wantRef)
	}
}

func TestAgentContextServiceWritesOnlyOnPayloadChange(t *testing.T) {
	current := contextSource(t, "duplicate.jsonl")
	harness := openContextAgent(t, contextAgentInput{revision: intPointer(1)})
	value := contextResolved(current.ref, current.fingerprint)
	stored, err := harness.snapshots.Put(contextSnapshotInput(harness.agent, value))
	if err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	fake := newFakeHistoryService(value)
	service := newContextService(t, harness, fake)

	result, err := service.RefreshAgent(observability.RefreshAgentContextInput{
		Agent:           harness.agent,
		IdentityChanged: false,
	})
	if err != nil {
		t.Fatalf("refreshAgent: %v", err)
	}
	if result.Changed {
		t.Fatal("changed = true, want false")
	}
	if !reflect.DeepEqual(result.Snapshot, stored) {
		t.Fatalf("snapshot = %+v, want %+v", result.Snapshot, stored)
	}

	advanced := replaceContextAgent(t, harness, contextAgentInput{revision: intPointer(2)})
	refreshed, err := service.RefreshAgent(observability.RefreshAgentContextInput{
		Agent:           advanced,
		IdentityChanged: false,
	})
	if err != nil {
		t.Fatalf("refreshAgent (advanced): %v", err)
	}
	if !refreshed.Changed {
		t.Fatal("changed = false, want true")
	}
	requireRevision(t, refreshed.Snapshot.PaneRevision, 2)
	if refreshed.Snapshot.UpdatedAt <= stored.UpdatedAt {
		t.Fatalf("updatedAt = %s, want later than %s",
			refreshed.Snapshot.UpdatedAt, stored.UpdatedAt)
	}
}

func TestAgentContextServiceDegradesForDeletedAgent(t *testing.T) {
	harness := openContextAgent(t, contextAgentInput{})
	current := contextSource(t, "deleted-agent.jsonl")
	fake := newFakeHistoryService(contextResolved(current.ref, current.fingerprint))
	service := newContextService(t, harness, fake)
	// The pane/agent row can be physically deleted while the daemon is still
	// draining a status event plan (journal 18:17:10 `status event plan drain
	// rejected rows { rejected: 1, errors: [...] }`). `agent_context_snapshots`
	// references `agents(id)`, so the insert used to fail the whole drain with
	// `FOREIGN KEY constraint failed`.
	if _, err := harness.sqlite.Exec(
		"delete from agents where id = ?", harness.agent.ID,
	); err != nil {
		t.Fatalf("delete agent: %v", err)
	}

	var result observability.RefreshAgentContextResult
	var err error
	warned := captureStderr(t, func() {
		result, err = service.RefreshAgent(observability.RefreshAgentContextInput{
			Agent:           harness.agent,
			IdentityChanged: false,
		})
	})
	if err != nil {
		t.Fatalf("refreshAgent: %v", err)
	}
	if !result.Changed {
		t.Fatal("changed = false, want true")
	}
	if result.Snapshot.CompactHistory.LastAssistantMessage == nil ||
		result.Snapshot.CompactHistory.LastAssistantMessage.Text != "done" {
		t.Fatalf("lastAssistantMessage = %+v, want text \"done\"",
			result.Snapshot.CompactHistory.LastAssistantMessage)
	}
	if result.Snapshot.UpdatedAt == "" {
		t.Fatal("updatedAt is empty, want a timestamp")
	}
	// Nothing is persisted for a deleted agent; the caller only needs the body of
	// the round it is delivering.
	persisted, err := harness.snapshots.Get(harness.agent.ID)
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if persisted != nil {
		t.Fatalf("snapshot = %+v, want none", persisted)
	}
	if !strings.Contains(warned, "deleted agent") ||
		!strings.Contains(warned, harness.agent.ID) {
		t.Fatalf("warning = %q, want the deleted-agent skip naming %s",
			warned, harness.agent.ID)
	}
}

func TestAgentContextServiceIgnoresDiscoveredFileRef(t *testing.T) {
	speculative := contextSource(t, "speculative.jsonl")
	discovered := *speculative.ref
	discovered.Kind = observability.HistoryRefDiscoveredFile
	harness := openContextAgent(t, contextAgentInput{revision: intPointer(1)})
	if _, err := harness.snapshots.Put(contextSnapshotInput(
		harness.agent, contextResolved(&discovered, speculative.fingerprint),
	)); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	fake := newFakeHistoryService(contextResolved(nil, nil))

	if _, err := newContextService(t, harness, fake).RefreshAgent(
		observability.RefreshAgentContextInput{
			Agent:           harness.agent,
			IdentityChanged: false,
		},
	); err != nil {
		t.Fatalf("refreshAgent: %v", err)
	}
	// The scanner that produced discovered_file refs is gone, so a persisted one
	// is dropped and discovery runs again against the official value only.
	requireResolveCalls(t, fake.calls, expectedResolveCall{forceDiscovery: false})
}

func TestAgentContextServiceRediscoversOnOccupiedChange(t *testing.T) {
	// A file the real pi reader can resolve, because this test drives the real
	// agent-history service rather than the fake.
	speculative := contextPiSession(t, "occupied-change.jsonl")
	path := *speculative.ref.Path
	harness := openContextAgent(t, contextAgentInput{revision: intPointer(1)})
	if _, err := harness.snapshots.Put(contextSnapshotInput(
		harness.agent, contextResolved(speculative.ref, speculative.fingerprint),
	)); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	// The real agent-history service, not the fake: the occupied-set change
	// has to survive the production discovery, fingerprint and compact-history
	// path, not just a recorded options object.
	history := newRealHistoryService()
	service := newContextService(t, harness, history)
	if _, err := service.RefreshAgent(observability.RefreshAgentContextInput{
		Agent:           harness.agent,
		IdentityChanged: false,
	}); err != nil {
		t.Fatalf("refreshAgent: %v", err)
	}
	requireResolveCalls(t, history.recordedCalls(), expectedResolveCall{
		forceDiscovery: false,
		preferredRef:   speculative.ref,
	})

	if _, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents: []db.HerdrAgentLike{
			{
				"agent":        "pi",
				"agent_status": "working",
				"pane_id":      "wB:p1",
				"terminal_id":  "term_pi",
				"workspace_id": "wB",
			},
			{
				"agent": "pi",
				"agent_session": map[string]any{
					"agent":  "pi",
					"kind":   "path",
					"source": "pi",
					"value":  path,
				},
				"agent_status": "working",
				"pane_id":      "wB:p2",
				"terminal_id":  "term_other",
				"workspace_id": "wB",
			},
		},
		HerdrSessionName: contextTestSession,
	}); err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	self, err := harness.agents.FindByTerminal(db.FindAgentByTerminalInput{
		HerdrSessionName: contextTestSession,
		TerminalID:       "term_pi",
	})
	if err != nil {
		t.Fatalf("findByTerminal: %v", err)
	}
	if self == nil {
		t.Fatal("Expected self agent")
	}
	if _, err := service.RefreshAgent(observability.RefreshAgentContextInput{
		Agent:           *self,
		IdentityChanged: false,
	}); err != nil {
		t.Fatalf("refreshAgent (occupied change): %v", err)
	}
	if len(history.recordedCalls()) != 2 {
		t.Fatalf("resolve calls = %d, want 2", len(history.recordedCalls()))
	}
	calls := history.recordedCalls()
	if !calls[1].ForceDiscovery {
		t.Fatal("second call forceDiscovery = false, want true")
	}
	// An official ref belongs to the agent that reported it, so occupancy no
	// longer suppresses it the way it suppressed a guessed discovered_file.
	requireResolveCalls(t, calls[1:], expectedResolveCall{
		forceDiscovery: true,
		preferredRef:   speculative.ref,
	})
}

func TestAgentContextServiceReportsOccupiedPaths(t *testing.T) {
	harness := openContextAgent(t, contextAgentInput{})
	agents, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents: []db.HerdrAgentLike{
			{
				"agent":        "pi",
				"agent_status": "working",
				"pane_id":      "wB:p1",
				"terminal_id":  "term_pi",
				"workspace_id": "wB",
			},
			{
				"agent": "pi",
				"agent_session": map[string]any{
					"agent":  "pi",
					"kind":   "path",
					"source": "pi",
					"value":  "/tmp/herdr-role-sessions/default/role-direct/session.jsonl",
				},
				"agent_status": "working",
				"pane_id":      "wB:p2",
				"terminal_id":  "term_other",
				"workspace_id": "wB",
			},
			{
				"agent":        "pi",
				"agent_status": "working",
				"pane_id":      "wB:p3",
				"terminal_id":  "term_speculative",
				"workspace_id": "wB",
			},
		},
		HerdrSessionName: contextTestSession,
	})
	if err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	var self, speculative *observability.AgentIndexRecord
	for index := range agents {
		switch *agents[index].TerminalID {
		case "term_pi":
			self = &agents[index]
		case "term_speculative":
			speculative = &agents[index]
		}
	}
	if self == nil || speculative == nil {
		t.Fatal("Expected seeded agents")
	}
	snapshotPath := "/tmp/herdr-role-sessions/default/role-spec/session.jsonl"
	snapshotRef := &observability.AgentHistoryRef{
		Kind:   observability.HistoryRefDiscoveredFile,
		Path:   &snapshotPath,
		Source: observability.HistorySourcePiJSONL,
		Value:  snapshotPath,
	}
	if _, err := harness.snapshots.Put(observability.AgentContextSnapshotInput{
		AgentID:        speculative.ID,
		CompactHistory: contextHistory(snapshotRef, "speculative done"),
		HistoryRef:     snapshotRef,
		PaneRevision:   intPointer(1),
		SourceFingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: 1,
			Path:    snapshotPath,
			Size:    1,
		},
	}); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	service := newContextService(t, harness, historyMethodsThrow{})

	// The set is computed from the other agents' live agentSession paths and
	// their persisted snapshot refs, before any refresh writes one for `self`.
	occupied, err := service.OccupiedSessionPathsFor(*self)
	if err != nil {
		t.Fatalf("occupiedSessionPathsFor: %v", err)
	}
	paths := make([]string, 0, len(occupied))
	for path := range occupied {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	want := []string{
		"/tmp/herdr-role-sessions/default/role-direct/session.jsonl",
		snapshotPath,
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("occupied = %v, want %v", paths, want)
	}
}

func TestAgentContextServiceCachedReads(t *testing.T) {
	harness := openContextAgent(t, contextAgentInput{})
	agents, err := harness.agents.ReplaceForSession(db.ReplaceAgentsForSessionInput{
		Agents: []db.HerdrAgentLike{
			{
				"agent":        "pi",
				"agent_status": "working",
				"pane_id":      "wB:p1",
				"terminal_id":  "term_pi",
				"workspace_id": "wB",
			},
			{
				"agent":        "claude",
				"agent_status": "done",
				"pane_id":      "wB:p2",
				"terminal_id":  "term_claude",
				"workspace_id": "wB",
			},
			{
				"agent":        "pi",
				"agent_status": "working",
				"pane_id":      "wB:p3",
				"terminal_id":  "term_other_pi",
				"workspace_id": "wB",
			},
		},
		HerdrSessionName: contextTestSession,
	})
	if err != nil {
		t.Fatalf("replaceForSession: %v", err)
	}
	var claude, otherPi *observability.AgentIndexRecord
	for index := range agents {
		switch *agents[index].TerminalID {
		case "term_claude":
			claude = &agents[index]
		case "term_other_pi":
			otherPi = &agents[index]
		}
	}
	if claude == nil || otherPi == nil {
		t.Fatal("Expected seeded agents")
	}
	claudeRef := &observability.AgentHistoryRef{
		Kind:   observability.HistoryRefDiscoveredFile,
		Path:   stringPointer("/tmp/claude.jsonl"),
		Source: observability.HistorySourcePiJSONL,
		Value:  "/tmp/claude.jsonl",
	}
	piRef := &observability.AgentHistoryRef{
		Kind:   observability.HistoryRefDiscoveredFile,
		Path:   stringPointer("/tmp/pi.jsonl"),
		Source: observability.HistorySourcePiJSONL,
		Value:  "/tmp/pi.jsonl",
	}
	claudeSnapshot, err := harness.snapshots.Put(observability.AgentContextSnapshotInput{
		AgentID:        claude.ID,
		CompactHistory: contextHistory(claudeRef, "claude done"),
		HistoryRef:     claudeRef,
		SourceFingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: 1,
			Path:    "/tmp/claude.jsonl",
			Size:    1,
		},
	})
	if err != nil {
		t.Fatalf("put claude snapshot: %v", err)
	}
	otherPiSnapshot, err := harness.snapshots.Put(observability.AgentContextSnapshotInput{
		AgentID:        otherPi.ID,
		CompactHistory: contextHistory(piRef, "pi done"),
		HistoryRef:     piRef,
		SourceFingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: 1,
			Path:    "/tmp/pi.jsonl",
			Size:    1,
		},
	})
	if err != nil {
		t.Fatalf("put other pi snapshot: %v", err)
	}
	service := newContextService(t, harness, historyMethodsThrow{})

	snapshot, err := service.GetAgentSnapshot(claude.ID)
	if err != nil {
		t.Fatalf("getAgentSnapshot: %v", err)
	}
	if snapshot == nil || !reflect.DeepEqual(*snapshot, claudeSnapshot) {
		t.Fatalf("getAgentSnapshot = %+v, want %+v", snapshot, claudeSnapshot)
	}
	workspaceID := "wB"
	items, err := service.ListAgents(observability.AgentQueryScope{
		HerdrSessionName: stringPointer(contextTestSession),
		WorkspaceID:      &workspaceID,
	})
	if err != nil {
		t.Fatalf("listAgents: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("listAgents = %d agents, want 3", len(items))
	}
	if items[0].Agent == nil || *items[0].Agent != "pi" ||
		items[0].TerminalID == nil || *items[0].TerminalID != "term_pi" {
		t.Fatalf("items[0] = %+v, want the term_pi agent", items[0])
	}
	if items[0].History.LastAssistantMessage != nil || items[0].History.LastUserMessage != nil ||
		items[0].History.Source != nil || items[0].History.UpdatedAt != nil {
		t.Fatalf("items[0].history = %+v, want empty", items[0].History)
	}
	if items[1].Agent == nil || *items[1].Agent != "claude" ||
		items[1].TerminalID == nil || *items[1].TerminalID != "term_claude" {
		t.Fatalf("items[1] = %+v, want the term_claude agent", items[1])
	}
	if items[1].History.LastAssistantMessage == nil ||
		items[1].History.LastAssistantMessage.Text != "claude done" {
		t.Fatalf("items[1].history = %+v, want the claude assistant message",
			items[1].History)
	}
	if items[2].Agent == nil || *items[2].Agent != "pi" ||
		items[2].TerminalID == nil || *items[2].TerminalID != "term_other_pi" {
		t.Fatalf("items[2] = %+v, want the term_other_pi agent", items[2])
	}
	workspace, err := service.WorkspaceSnapshot(observability.WorkspaceSnapshotInput{
		AgentScope: observability.AgentScope{
			HerdrSessionName: contextTestSession,
			WorkspaceID:      workspaceID,
		},
		ExcludeTerminalID: "term_pi",
	})
	if err != nil {
		t.Fatalf("workspaceSnapshot: %v", err)
	}
	if workspace == nil {
		t.Fatal("workspaceSnapshot = nil, want the peer agents")
	}
	if len(workspace.Agents) != 2 {
		t.Fatalf("workspace agents = %d, want 2", len(workspace.Agents))
	}
	if workspace.Agents[0].TerminalID == nil || *workspace.Agents[0].TerminalID != "term_claude" ||
		workspace.Agents[1].TerminalID == nil || *workspace.Agents[1].TerminalID != "term_other_pi" {
		t.Fatalf("workspace agents = %+v, want term_claude and term_other_pi",
			workspace.Agents)
	}
	if workspace.HerdrSessionName != contextTestSession || workspace.WorkspaceID != workspaceID {
		t.Fatalf("workspace scope = %+v, want %s/%s",
			workspace.AgentScope, contextTestSession, workspaceID)
	}
	latestUpdatedAt := claudeSnapshot.UpdatedAt
	if otherPiSnapshot.UpdatedAt > latestUpdatedAt {
		latestUpdatedAt = otherPiSnapshot.UpdatedAt
	}
	if workspace.UpdatedAt != latestUpdatedAt {
		t.Fatalf("workspace updatedAt = %s, want %s", workspace.UpdatedAt, latestUpdatedAt)
	}
}

func TestAgentContextServiceWorkspaceSnapshotWithoutPeers(t *testing.T) {
	harness := openContextAgent(t, contextAgentInput{})
	ownerRef := &observability.AgentHistoryRef{
		Kind:   observability.HistoryRefDiscoveredFile,
		Path:   stringPointer("/tmp/owner.jsonl"),
		Source: observability.HistorySourcePiJSONL,
		Value:  "/tmp/owner.jsonl",
	}
	if _, err := harness.snapshots.Put(observability.AgentContextSnapshotInput{
		AgentID:        harness.agent.ID,
		CompactHistory: contextHistory(ownerRef, "owner done"),
		HistoryRef:     ownerRef,
		SourceFingerprint: &observability.AgentHistorySourceFingerprint{
			MtimeMs: 1,
			Path:    "/tmp/owner.jsonl",
			Size:    1,
		},
	}); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	service := newContextService(t, harness, historyMethodsThrow{})
	scope := observability.AgentScope{
		HerdrSessionName: contextTestSession,
		WorkspaceID:      "wB",
	}

	workspace, err := service.WorkspaceSnapshot(observability.WorkspaceSnapshotInput{
		AgentScope:        scope,
		ExcludeTerminalID: "term_pi",
	})
	if err != nil {
		t.Fatalf("workspaceSnapshot: %v", err)
	}
	if workspace != nil {
		t.Fatalf("workspaceSnapshot = %+v, want nil", workspace)
	}
	sessions := db.NewHerdrSessionStore(harness.sqlite)
	if err := sessions.MarkStoppedMissingFrom(nil); err != nil {
		t.Fatalf("markStoppedMissingFrom: %v", err)
	}
	items, err := service.ListAgents(observability.AgentQueryScope{
		HerdrSessionName: stringPointer(contextTestSession),
		WorkspaceID:      stringPointer("wB"),
	})
	if err != nil {
		t.Fatalf("listAgents: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("listAgents = %d agents, want 0", len(items))
	}
	workspace, err = service.WorkspaceSnapshot(observability.WorkspaceSnapshotInput{
		AgentScope:        scope,
		ExcludeTerminalID: "other",
	})
	if err != nil {
		t.Fatalf("workspaceSnapshot (stopped session): %v", err)
	}
	if workspace != nil {
		t.Fatalf("workspaceSnapshot = %+v, want nil", workspace)
	}
}

func intPointer(value int) *int {
	return &value
}

func stringPointer(value string) *string {
	return &value
}
