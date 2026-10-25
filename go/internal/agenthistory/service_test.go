package agenthistory

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// Ported from test/unit/agent-history-service.test.ts.

// serviceSourceFile writes a source file and returns its path, mirroring the TS
// `sourceFile` helper.
func serviceSourceFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("history\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func serviceRef(path string, source HistorySource) HistoryRef {
	return HistoryRef{Kind: KindDiscoveredFile, Path: &path, Source: source, Value: path}
}

// sameHistoryRef compares two refs by value: HistoryRef holds a *string, so the
// generated == would compare the pointer rather than the path.
func sameHistoryRef(got, want HistoryRef) bool {
	if got.Kind != want.Kind || got.Source != want.Source || got.Value != want.Value {
		return false
	}
	if (got.Path == nil) != (want.Path == nil) {
		return false
	}
	return got.Path == nil || *got.Path == *want.Path
}

func piLookupInput() AgentHistoryLookupInput {
	return AgentHistoryLookupInput{Agent: strp("pi"), AgentSession: nil}
}

// fakeReaderOptions mirrors the `{ failCompact, failRead, noAssistant }` doubles.
// It is shared by pointer so a test can flip `noAssistant` mid-test, exactly like
// the TS closure over its options object.
type fakeReaderOptions struct {
	failCompact bool
	failRead    bool
	noAssistant bool
}

// fakeHistoryReader records every ref it was asked to read.
type fakeHistoryReader struct {
	options     *fakeReaderOptions
	compactRefs []HistoryRef
	readRefs    []HistoryRef
}

func newFakeHistoryReader(options *fakeReaderOptions) *fakeHistoryReader {
	return &fakeHistoryReader{options: options}
}

func (r *fakeHistoryReader) CanRead(historyRef HistoryRef) bool {
	return historyRef.Source == SourcePiJSONL || historyRef.Source == SourceAntigravitySQLite
}

func (r *fakeHistoryReader) Read(historyRef HistoryRef, _ *int) ([]HistoryMessage, error) {
	r.readRefs = append(r.readRefs, historyRef)
	if r.options.failRead {
		return nil, errors.New("read failed")
	}
	return []HistoryMessage{{Ref: "entry", Role: RoleAssistant, Text: "done"}}, nil
}

func (r *fakeHistoryReader) ReadCompact(historyRef HistoryRef) (CompactAgentHistory, error) {
	r.compactRefs = append(r.compactRefs, historyRef)
	if r.options.failCompact {
		return CompactAgentHistory{}, errors.New("compact failed")
	}
	compact := EmptyCompactHistory(sourceString(historyRef.Source))
	compact.HistoryRef = &historyRef
	if !r.options.noAssistant {
		compact.LastAssistantMessage = &HistoryExcerpt{Ref: "entry", Text: "done"}
	}
	return compact, nil
}

// fakeHistoryCache mirrors the `Pick<AgentHistoryCacheStore, "getFresh" | "put">`
// double.
type fakeHistoryCache struct {
	getFresh func(HistoryCacheFreshInput) *HistoryCacheRecord
	putCalls []HistoryCachePutInput
}

func (c *fakeHistoryCache) GetFresh(input HistoryCacheFreshInput) *HistoryCacheRecord {
	if c.getFresh == nil {
		return nil
	}
	return c.getFresh(input)
}

func (c *fakeHistoryCache) Put(input HistoryCachePutInput) error {
	c.putCalls = append(c.putCalls, input)
	return nil
}

type serviceFixtureOptions struct {
	cache      *fakeHistoryCache
	discovered *HistoryRef
	reader     *fakeHistoryReader
}
type historyServiceFixture struct {
	discoveries int
	reader      *fakeHistoryReader
	service     *AgentHistoryService
}

func newHistoryServiceFixture(t *testing.T, options serviceFixtureOptions) *historyServiceFixture {
	t.Helper()
	fakeReader := options.reader
	if fakeReader == nil {
		fakeReader = newFakeHistoryReader(&fakeReaderOptions{})
	}
	fixture := &historyServiceFixture{reader: fakeReader}
	var cache HistoryCacheStore
	if options.cache != nil {
		cache = options.cache
	}
	fixture.service = CreateAgentHistoryService(AgentHistoryServiceOptions{
		Cache: cache,
		Discover: func(_ AgentHistoryLookupInput, _ DiscoverOptions) *HistoryRef {
			fixture.discoveries++
			return options.discovered
		},
		Readers: []HistoryReader{fakeReader},
	})
	return fixture
}

// --- diagnostics sinks ------------------------------------------------------

type serviceDiagnosticRecord struct {
	level   HistoryDiagnosticLevel
	message string
	payload any
}

func setHistoryDiagnosticSinkForTest(t *testing.T, sink func(HistoryDiagnostic)) {
	t.Helper()
	historyDiagnosticMu.Lock()
	previous := historyDiagnosticSink
	historyDiagnosticSink = sink
	historyDiagnosticMu.Unlock()
	t.Cleanup(func() {
		historyDiagnosticMu.Lock()
		historyDiagnosticSink = previous
		historyDiagnosticMu.Unlock()
	})
}

func captureServiceDiagnostics(t *testing.T) *[]serviceDiagnosticRecord {
	t.Helper()
	records := &[]serviceDiagnosticRecord{}
	setHistoryDiagnosticSinkForTest(t, func(diagnostic HistoryDiagnostic) {
		*records = append(*records, serviceDiagnosticRecord{
			level:   diagnostic.Level,
			message: diagnostic.Message,
			payload: diagnostic.Payload,
		})
	})
	return records
}

// silenceServiceDiagnostics drops the stderr noise of diagnostics a test does not
// assert on.
func silenceServiceDiagnostics(t *testing.T) {
	t.Helper()
	setHistoryDiagnosticSinkForTest(t, func(HistoryDiagnostic) {})
}

func requireServiceDiagnostic(
	t *testing.T,
	records []serviceDiagnosticRecord,
	level HistoryDiagnosticLevel,
	message string,
) {
	t.Helper()
	for _, record := range records {
		if record.level == level && record.message == message {
			return
		}
	}
	t.Fatalf("no %s diagnostic %q; got %d record(s): %+v", level, message, len(records), records)
}

func TestAgentHistoryServiceReadsAValidPreferredRefWithoutDiscovery(t *testing.T) {
	path := serviceSourceFile(t, "preferred.jsonl")
	preferred := serviceRef(path, SourcePiJSONL)
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{})

	result := fixture.service.ResolveCompactHistory(piLookupInput(), ResolveCompactOptions{PreferredRef: &preferred})

	if fixture.discoveries != 0 {
		t.Fatalf("discoveries = %d, want 0", fixture.discoveries)
	}
	if !reflect.DeepEqual(fixture.reader.compactRefs, []HistoryRef{preferred}) {
		t.Fatalf("compactRefs = %+v, want [%+v]", fixture.reader.compactRefs, preferred)
	}
	if result.HistoryRef == nil || !sameHistoryRef(*result.HistoryRef, preferred) {
		t.Fatalf("historyRef = %+v, want %+v", result.HistoryRef, preferred)
	}
	if result.CompactHistory.HistoryRef == nil ||
		!sameHistoryRef(*result.CompactHistory.HistoryRef, preferred) {
		t.Fatalf("compactHistory.historyRef = %+v, want %+v", result.CompactHistory.HistoryRef, preferred)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	want := &SourceFingerprint{
		MtimeMs: float64(info.ModTime().UnixMilli()),
		Path:    path,
		Size:    info.Size(),
	}
	if !reflect.DeepEqual(result.SourceFingerprint, want) {
		t.Fatalf("sourceFingerprint = %+v, want %+v", result.SourceFingerprint, want)
	}
}

func TestAgentHistoryServiceReturnsAFreshCachedCompactHistory(t *testing.T) {
	path := serviceSourceFile(t, "cached.jsonl")
	preferred := serviceRef(path, SourcePiJSONL)
	cached := EmptyCompactHistory(sourceString(SourcePiJSONL))
	cached.HistoryRef = &preferred
	cached.LastAssistantMessage = &HistoryExcerpt{Ref: "cached", Text: "cached result"}
	cached.MessageCount = 4
	cache := &fakeHistoryCache{
		getFresh: func(HistoryCacheFreshInput) *HistoryCacheRecord {
			return &HistoryCacheRecord{CompactHistory: cached}
		},
	}
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{cache: cache})

	result := fixture.service.ResolveCompactHistory(piLookupInput(), ResolveCompactOptions{PreferredRef: &preferred})

	if !reflect.DeepEqual(result.CompactHistory, cached) {
		t.Fatalf("compactHistory = %+v, want %+v", result.CompactHistory, cached)
	}
	if len(fixture.reader.compactRefs) != 0 {
		t.Fatalf("compactRefs = %+v, want none", fixture.reader.compactRefs)
	}
	if fixture.discoveries != 0 {
		t.Fatalf("discoveries = %d, want 0", fixture.discoveries)
	}
}

func TestAgentHistoryServiceForceDiscoveryIgnoresThePreferredRef(t *testing.T) {
	preferred := serviceRef(serviceSourceFile(t, "preferred.jsonl"), SourcePiJSONL)
	discovered := serviceRef(serviceSourceFile(t, "discovered.jsonl"), SourcePiJSONL)
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{discovered: &discovered})

	result := fixture.service.ResolveCompactHistory(piLookupInput(), ResolveCompactOptions{
		ForceDiscovery: true,
		PreferredRef:   &preferred,
	})

	if fixture.discoveries != 1 {
		t.Fatalf("discoveries = %d, want 1", fixture.discoveries)
	}
	if !reflect.DeepEqual(fixture.reader.compactRefs, []HistoryRef{discovered}) {
		t.Fatalf("compactRefs = %+v, want [%+v]", fixture.reader.compactRefs, discovered)
	}
	if result.HistoryRef == nil || !sameHistoryRef(*result.HistoryRef, discovered) {
		t.Fatalf("historyRef = %+v, want %+v", result.HistoryRef, discovered)
	}
}

func TestAgentHistoryServiceReturnsEmptyHistoryWhenADirectRefSourceDisappeared(t *testing.T) {
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{})
	missing := serviceRef(filepath.Join(t.TempDir(), "missing-history.jsonl"), SourcePiJSONL)

	result := fixture.service.ReadCompactRef(missing, ReadCompactOptions{})

	want := unresolvedCompactHistory(sourceString(SourcePiJSONL))
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("readCompactRef = %+v, want %+v", result, want)
	}
}

func TestAgentHistoryServiceDoesNotCacheCompactHistoryWithoutAnAssistantMessage(t *testing.T) {
	path := serviceSourceFile(t, "no-assistant.jsonl")
	readerOptions := &fakeReaderOptions{noAssistant: true}
	fakeReader := newFakeHistoryReader(readerOptions)
	cache := &fakeHistoryCache{}
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{cache: cache, reader: fakeReader})
	silenceServiceDiagnostics(t)

	first := fixture.service.ReadCompactRef(serviceRef(path, SourcePiJSONL), ReadCompactOptions{})
	readerOptions.noAssistant = false
	second := fixture.service.ReadCompactRef(serviceRef(path, SourcePiJSONL), ReadCompactOptions{})

	if first.CompactHistory.LastAssistantMessage != nil {
		t.Fatalf("first lastAssistantMessage = %+v, want nil", first.CompactHistory.LastAssistantMessage)
	}
	if second.CompactHistory.LastAssistantMessage == nil || second.CompactHistory.LastAssistantMessage.Text != "done" {
		t.Fatalf("second lastAssistantMessage = %+v, want text done", second.CompactHistory.LastAssistantMessage)
	}
	if len(cache.putCalls) != 1 {
		t.Fatalf("cache put calls = %d, want 1", len(cache.putCalls))
	}
	if len(fakeReader.compactRefs) != 2 {
		t.Fatalf("compactRefs = %d, want 2", len(fakeReader.compactRefs))
	}
}

func TestAgentHistoryServiceIgnoresACachedCompactHistoryWithoutAnAssistantMessage(t *testing.T) {
	path := serviceSourceFile(t, "cached-no-assistant.jsonl")
	cached := EmptyCompactHistory(sourceString(SourcePiJSONL))
	cachedRef := serviceRef(path, SourcePiJSONL)
	cached.HistoryRef = &cachedRef
	cache := &fakeHistoryCache{
		getFresh: func(HistoryCacheFreshInput) *HistoryCacheRecord {
			return &HistoryCacheRecord{CompactHistory: cached}
		},
	}
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{cache: cache})

	result := fixture.service.ReadCompactRef(serviceRef(path, SourcePiJSONL), ReadCompactOptions{})

	if result.CompactHistory.LastAssistantMessage == nil ||
		result.CompactHistory.LastAssistantMessage.Text != "done" {
		t.Fatalf("lastAssistantMessage = %+v, want text done", result.CompactHistory.LastAssistantMessage)
	}
	if len(fixture.reader.compactRefs) != 1 {
		t.Fatalf("compactRefs = %d, want 1", len(fixture.reader.compactRefs))
	}
}

func TestAgentHistoryServiceLogsDebugAndIncrementsCounterOnPiEmptyAssistantRead(t *testing.T) {
	ResetPiEmptyAssistantCount()
	records := captureServiceDiagnostics(t)
	piPath := serviceSourceFile(t, "pi-empty.jsonl")
	agyPath := serviceSourceFile(t, "agy-empty.db")
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{
		reader: newFakeHistoryReader(&fakeReaderOptions{noAssistant: true}),
	})

	piResult := fixture.service.ReadCompactRef(serviceRef(piPath, SourcePiJSONL), ReadCompactOptions{})
	if piResult.CompactHistory.LastAssistantMessage != nil {
		t.Fatalf("pi lastAssistantMessage = %+v, want nil", piResult.CompactHistory.LastAssistantMessage)
	}
	if got := GetPiEmptyAssistantCount(); got != 1 {
		t.Fatalf("piEmptyAssistantCount = %d, want 1", got)
	}
	requireServiceDiagnostic(t, *records, DiagnosticDebug,
		"Herdsman history read produced no assistant message")
	for _, record := range *records {
		payload, ok := record.payload.(map[string]any)
		if !ok || payload["source"] != string(SourcePiJSONL) {
			t.Fatalf("debug payload = %+v, want source %q", record.payload, SourcePiJSONL)
		}
		if record.level != DiagnosticDebug {
			t.Fatalf("pi diagnostic level = %q, want %q", record.level, DiagnosticDebug)
		}
	}

	agyResult := fixture.service.ReadCompactRef(serviceRef(agyPath, SourceAntigravitySQLite), ReadCompactOptions{})
	if agyResult.CompactHistory.LastAssistantMessage != nil {
		t.Fatalf("agy lastAssistantMessage = %+v, want nil", agyResult.CompactHistory.LastAssistantMessage)
	}
	if got := GetPiEmptyAssistantCount(); got != 1 {
		t.Fatalf("piEmptyAssistantCount after agy read = %d, want 1", got)
	}
	requireServiceDiagnostic(t, *records, DiagnosticWarn,
		"Herdsman history read produced no assistant message")
	warned := false
	for _, record := range *records {
		if record.level != DiagnosticWarn || record.message != "Herdsman history read produced no assistant message" {
			continue
		}
		payload, ok := record.payload.(map[string]any)
		if !ok || payload["source"] != string(SourceAntigravitySQLite) {
			t.Fatalf("warn payload = %+v, want source %q", record.payload, SourceAntigravitySQLite)
		}
		warned = true
	}
	if !warned {
		t.Fatalf("no warn diagnostic for the agy empty assistant read: %+v", *records)
	}
}

func TestAgentHistoryServiceFingerprintsTheResolvedFileWhilePreservingTheOfficialSessionID(t *testing.T) {
	path := serviceSourceFile(t, "agy.db")
	preferred := HistoryRef{
		Kind:   KindDiscoveredFile,
		Path:   &path,
		Source: SourceAntigravitySQLite,
		Value:  "session-a",
	}
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{})

	result := fixture.service.ReadCompactRef(preferred, ReadCompactOptions{})

	if result.HistoryRef == nil || !sameHistoryRef(*result.HistoryRef, preferred) {
		t.Fatalf("historyRef = %+v, want %+v", result.HistoryRef, preferred)
	}
	if result.SourceFingerprint == nil || result.SourceFingerprint.Path != path {
		t.Fatalf("sourceFingerprint = %+v, want path %q", result.SourceFingerprint, path)
	}
	if result.CompactHistory.HistoryRef == nil || result.CompactHistory.HistoryRef.Value != "session-a" {
		t.Fatalf("compactHistory.historyRef = %+v, want value session-a", result.CompactHistory.HistoryRef)
	}
}

func TestAgentHistoryServiceUsesAPreferredRefForLiveReadsAndFallsBackOnce(t *testing.T) {
	preferred := serviceRef(serviceSourceFile(t, "preferred.jsonl"), SourcePiJSONL)
	discovered := serviceRef(serviceSourceFile(t, "discovered.jsonl"), SourcePiJSONL)
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{discovered: &discovered})
	silenceServiceDiagnostics(t)

	first := fixture.service.Read(piLookupInput(), ReadOptions{Limit: 1, PreferredRef: &preferred})
	if first.HistoryRef == nil || !sameHistoryRef(*first.HistoryRef, preferred) {
		t.Fatalf("first historyRef = %+v, want %+v", first.HistoryRef, preferred)
	}
	if len(first.Messages) != 1 || first.Messages[0].Text != "done" {
		t.Fatalf("first messages = %+v, want one done message", first.Messages)
	}
	if fixture.discoveries != 0 {
		t.Fatalf("discoveries = %d, want 0", fixture.discoveries)
	}
	if !reflect.DeepEqual(fixture.reader.readRefs, []HistoryRef{preferred}) {
		t.Fatalf("readRefs = %+v, want [%+v]", fixture.reader.readRefs, preferred)
	}

	missing := serviceRef(filepath.Join(t.TempDir(), "missing-history.jsonl"), SourcePiJSONL)
	second := fixture.service.Read(piLookupInput(), ReadOptions{Limit: 1, PreferredRef: &missing})
	if second.HistoryRef == nil || !sameHistoryRef(*second.HistoryRef, discovered) {
		t.Fatalf("second historyRef = %+v, want %+v", second.HistoryRef, discovered)
	}
	if fixture.discoveries != 1 {
		t.Fatalf("discoveries = %d, want 1", fixture.discoveries)
	}
	if !reflect.DeepEqual(fixture.reader.readRefs, []HistoryRef{preferred, discovered}) {
		t.Fatalf("readRefs = %+v, want [%+v %+v]", fixture.reader.readRefs, preferred, discovered)
	}
}

func TestAgentHistoryServiceRediscoversExactlyOnceWhenAPreferredRefFails(t *testing.T) {
	discovered := serviceRef(serviceSourceFile(t, "discovered.jsonl"), SourcePiJSONL)
	missingFixture := newHistoryServiceFixture(t, serviceFixtureOptions{discovered: &discovered})
	missing := serviceRef(filepath.Join(t.TempDir(), "missing-history.jsonl"), SourcePiJSONL)

	result := missingFixture.service.ResolveCompactHistory(piLookupInput(), ResolveCompactOptions{PreferredRef: &missing})
	if result.HistoryRef == nil || !sameHistoryRef(*result.HistoryRef, discovered) {
		t.Fatalf("historyRef = %+v, want %+v", result.HistoryRef, discovered)
	}
	if missingFixture.discoveries != 1 {
		t.Fatalf("discoveries = %d, want 1", missingFixture.discoveries)
	}

	failedReader := newFakeHistoryReader(&fakeReaderOptions{failCompact: true})
	failedFixture := newHistoryServiceFixture(t, serviceFixtureOptions{discovered: &discovered, reader: failedReader})
	silenceServiceDiagnostics(t)
	failed := failedFixture.service.ResolveCompactHistory(piLookupInput(), ResolveCompactOptions{
		PreferredRef: func() *HistoryRef {
			ref := serviceRef(serviceSourceFile(t, "bad.jsonl"), SourcePiJSONL)
			return &ref
		}(),
	})
	want := unresolvedCompactHistory(sourceString(SourcePiJSONL))
	if !reflect.DeepEqual(failed, want) {
		t.Fatalf("resolveCompactHistory = %+v, want %+v", failed, want)
	}
	if failedFixture.discoveries != 1 {
		t.Fatalf("discoveries = %d, want 1", failedFixture.discoveries)
	}
	if len(failedReader.compactRefs) != 2 {
		t.Fatalf("compactRefs = %d, want 2", len(failedReader.compactRefs))
	}
}

func TestAgentHistoryServiceUsesTheSameOneFallbackRuleForReaderFailuresDuringLiveReads(t *testing.T) {
	preferred := serviceRef(serviceSourceFile(t, "preferred.jsonl"), SourcePiJSONL)
	discovered := serviceRef(serviceSourceFile(t, "discovered.jsonl"), SourcePiJSONL)
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{
		discovered: &discovered,
		reader:     newFakeHistoryReader(&fakeReaderOptions{failRead: true}),
	})
	silenceServiceDiagnostics(t)

	result := fixture.service.Read(piLookupInput(), ReadOptions{Limit: 1, PreferredRef: &preferred})

	if result.HistoryRef != nil || len(result.Messages) != 0 {
		t.Fatalf("read = %+v, want no ref and no messages", result)
	}
	if fixture.discoveries != 1 {
		t.Fatalf("discoveries = %d, want 1", fixture.discoveries)
	}
	if !reflect.DeepEqual(fixture.reader.readRefs, []HistoryRef{preferred, discovered}) {
		t.Fatalf("readRefs = %+v, want [%+v %+v]", fixture.reader.readRefs, preferred, discovered)
	}
}

func TestAgentHistoryServiceKeepsGetCompactHistoryAsTheCompatibilityWrapper(t *testing.T) {
	discovered := serviceRef(serviceSourceFile(t, "discovered.jsonl"), SourcePiJSONL)
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{discovered: &discovered})

	want := EmptyCompactHistory(sourceString(SourcePiJSONL))
	want.HistoryRef = &discovered
	want.LastAssistantMessage = &HistoryExcerpt{Ref: "entry", Text: "done"}
	if got := fixture.service.GetCompactHistory(piLookupInput()); !reflect.DeepEqual(got, want) {
		t.Fatalf("getCompactHistory = %+v, want %+v", got, want)
	}
}

func TestAgentHistoryServiceForwardsThePaneIdentityToDiscovery(t *testing.T) {
	discovered := serviceRef(serviceSourceFile(t, "discovered.jsonl"), SourcePiJSONL)
	fakeReader := newFakeHistoryReader(&fakeReaderOptions{})
	var discoveryInput *AgentHistoryLookupInput
	service := CreateAgentHistoryService(AgentHistoryServiceOptions{
		Discover: func(input AgentHistoryLookupInput, _ DiscoverOptions) *HistoryRef {
			captured := input
			discoveryInput = &captured
			return &discovered
		},
		Readers: []HistoryReader{fakeReader},
	})

	service.ResolveCompactHistory(AgentHistoryLookupInput{
		Agent:            strp("pi"),
		AgentSession:     nil,
		HerdrSessionName: strp("default"),
		PaneID:           strp("wA:p1"),
	}, ResolveCompactOptions{ForceDiscovery: true})

	want := AgentHistoryLookupInput{
		Agent:            strp("pi"),
		AgentSession:     nil,
		HerdrSessionName: strp("default"),
		PaneID:           strp("wA:p1"),
	}
	if discoveryInput == nil || !reflect.DeepEqual(*discoveryInput, want) {
		t.Fatalf("discovery input = %+v, want %+v", discoveryInput, want)
	}
}

func TestAgentHistoryServiceForceRefreshBypassesGetFreshAndUpdatesCache(t *testing.T) {
	path := serviceSourceFile(t, "force-refresh.jsonl")
	preferred := serviceRef(path, SourcePiJSONL)
	stale := EmptyCompactHistory(sourceString(SourcePiJSONL))
	stale.HistoryRef = &preferred
	stale.LastAssistantMessage = &HistoryExcerpt{Ref: "stale", Text: "stale cache"}
	getFreshCalls := 0
	cache := &fakeHistoryCache{
		getFresh: func(HistoryCacheFreshInput) *HistoryCacheRecord {
			getFreshCalls++
			return &HistoryCacheRecord{CompactHistory: stale}
		},
	}
	fixture := newHistoryServiceFixture(t, serviceFixtureOptions{cache: cache})

	result := fixture.service.ResolveCompactHistory(piLookupInput(), ResolveCompactOptions{
		ForceRefresh: true,
		PreferredRef: &preferred,
	})

	if getFreshCalls != 0 {
		t.Fatalf("getFresh calls = %d, want 0", getFreshCalls)
	}
	if result.CompactHistory.LastAssistantMessage == nil ||
		result.CompactHistory.LastAssistantMessage.Text != "done" {
		t.Fatalf("lastAssistantMessage = %+v, want text done", result.CompactHistory.LastAssistantMessage)
	}
	if len(cache.putCalls) != 1 {
		t.Fatalf("cache put calls = %d, want 1", len(cache.putCalls))
	}
	if cache.putCalls[0].FormatterVersion != AgentHistoryFormatterVersion {
		t.Fatalf("put formatterVersion = %q, want %q",
			cache.putCalls[0].FormatterVersion, AgentHistoryFormatterVersion)
	}
	if cache.putCalls[0].SourcePath != path {
		t.Fatalf("put sourcePath = %q, want %q", cache.putCalls[0].SourcePath, path)
	}
}

func TestStatSourceFingerprintIncludesWalAndShmSidecars(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "agy.sqlite")
	walPath := mainPath + "-wal"
	shmPath := mainPath + "-shm"
	if err := os.WriteFile(mainPath, []byte("main file"), 0o600); err != nil {
		t.Fatalf("write main: %v", err)
	}

	withoutSidecars := StatSourceFingerprint(mainPath)
	mainInfo, err := os.Stat(mainPath)
	if err != nil {
		t.Fatalf("stat main: %v", err)
	}
	wantWithout := &SourceFingerprint{
		MtimeMs: float64(mainInfo.ModTime().UnixMilli()),
		Path:    mainPath,
		Size:    mainInfo.Size(),
	}
	if !reflect.DeepEqual(withoutSidecars, wantWithout) {
		t.Fatalf("statSourceFingerprint = %+v, want %+v", withoutSidecars, wantWithout)
	}

	if err := os.WriteFile(walPath, []byte("wal file data"), 0o600); err != nil {
		t.Fatalf("write wal: %v", err)
	}
	if err := os.WriteFile(shmPath, []byte("shm data"), 0o600); err != nil {
		t.Fatalf("write shm: %v", err)
	}
	walInfo, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	shmInfo, err := os.Stat(shmPath)
	if err != nil {
		t.Fatalf("stat shm: %v", err)
	}

	withSidecars := StatSourceFingerprint(mainPath)
	wantWith := &SourceFingerprint{
		MtimeMs: float64(mainInfo.ModTime().UnixMilli()) +
			float64(walInfo.ModTime().UnixMilli())*3 +
			float64(shmInfo.ModTime().UnixMilli())*7,
		Path: mainPath,
		Size: mainInfo.Size() + walInfo.Size() + shmInfo.Size(),
	}
	if !reflect.DeepEqual(withSidecars, wantWith) {
		t.Fatalf("statSourceFingerprint with sidecars = %+v, want %+v", withSidecars, wantWith)
	}
}

// --- failed discovery is not repeated every round ---------------------------

func agySessionInput(value string) AgentHistoryLookupInput {
	return AgentHistoryLookupInput{
		Agent: strp("agy"),
		AgentSession: &observability.AgentSessionRef{
			Agent:  "agy",
			Kind:   observability.SessionRefID,
			Source: "herdr:antigravity_cli",
			Value:  value,
		},
	}
}

func TestFailedDiscoveryWarnsOnceAndReadsNothingTwice(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	// The real discovery (no injected one): this is what the daemon runs.
	service := CreateAgentHistoryService(AgentHistoryServiceOptions{HomeDir: strp(homeDir)})
	input := agySessionInput("99999999-9999-4999-8999-999999999999")
	discoveryRecords := captureDiscoveryWarnings(t)
	serviceRecords := captureServiceDiagnostics(t)

	first := service.ResolveCompactHistory(input, ResolveCompactOptions{})
	second := service.ResolveCompactHistory(input, ResolveCompactOptions{})
	read := service.Read(input, ReadOptions{Limit: 10})

	if first.HistoryRef != nil {
		t.Fatalf("first historyRef = %+v, want nil", first.HistoryRef)
	}
	if second.HistoryRef != nil {
		t.Fatalf("second historyRef = %+v, want nil", second.HistoryRef)
	}
	if read.HistoryRef != nil || len(read.Messages) != 0 {
		t.Fatalf("read = %+v, want no ref and no messages", read)
	}
	// Exactly one warning: the discovery one. The "discovery returned no
	// reference" warning and the repeated lookup are both suppressed.
	total := len(*discoveryRecords) + len(*serviceRecords)
	if total != 1 {
		t.Fatalf("warnings = %d (%d discovery, %d service), want 1: %+v / %+v",
			total, len(*discoveryRecords), len(*serviceRecords), *discoveryRecords, *serviceRecords)
	}
	if !strings.Contains((*discoveryRecords)[0].message, DiscoveryWarnMessage) {
		t.Fatalf("warning message = %q, want it to contain %q",
			(*discoveryRecords)[0].message, DiscoveryWarnMessage)
	}
}

func TestFailedDiscoveryResolvesAsSoonAsTheOfficialValuesChange(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	service := CreateAgentHistoryService(AgentHistoryServiceOptions{HomeDir: strp(homeDir)})
	id := "12121212-1212-4121-8121-121212121212"
	otherID := "34343434-3434-4343-8343-343434343434"
	input := agySessionInput(id)
	silenceDiscoveryWarnings(t)
	captureServiceDiagnostics(t)

	if got := service.ResolveCompactHistory(input, ResolveCompactOptions{}); got.HistoryRef != nil {
		t.Fatalf("historyRef = %+v, want nil", got.HistoryRef)
	}
	conversations := filepath.Join(homeDir, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(conversations, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(conversations, id+".db"), []byte(""), 0o600); err != nil {
		t.Fatalf("write id db: %v", err)
	}

	// A changed official id is a new lookup key, so the pane is attempted
	// immediately instead of waiting out the previous failure's window.
	other := agySessionInput(otherID)
	if err := os.WriteFile(filepath.Join(conversations, otherID+".db"), []byte(""), 0o600); err != nil {
		t.Fatalf("write other db: %v", err)
	}
	resolved := service.ResolveCompactHistory(other, ResolveCompactOptions{})
	if resolved.HistoryRef == nil || resolved.HistoryRef.Source != SourceAntigravitySQLite ||
		resolved.HistoryRef.Value != otherID {
		t.Fatalf("historyRef = %+v, want source %q and value %q",
			resolved.HistoryRef, SourceAntigravitySQLite, otherID)
	}
	if resolved.CompactHistory.Source == nil || *resolved.CompactHistory.Source != string(SourceAntigravitySQLite) {
		t.Fatalf("compactHistory.source = %+v, want %q",
			resolved.CompactHistory.Source, SourceAntigravitySQLite)
	}
}

func TestForcedDiscoveryBypassesTheFailureMemoryForUnchangedOfficialValues(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	service := CreateAgentHistoryService(AgentHistoryServiceOptions{HomeDir: strp(homeDir)})
	id := "56565656-5656-4565-8565-565656565656"
	input := agySessionInput(id)
	discoveryRecords := captureDiscoveryWarnings(t)
	captureServiceDiagnostics(t)

	if got := service.ResolveCompactHistory(input, ResolveCompactOptions{}); got.HistoryRef != nil {
		t.Fatalf("historyRef = %+v, want nil", got.HistoryRef)
	}
	if len(*discoveryRecords) != 1 {
		t.Fatalf("discovery warnings = %d, want 1", len(*discoveryRecords))
	}

	// The operator fixes the reported problem inside the 60s window and forces a
	// refresh: the memory only decelerates the ordinary cadence, so the lookup
	// happens for real. The warning stays deduplicated per key.
	conversations := filepath.Join(homeDir, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(conversations, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(conversations, id+".db"), []byte(""), 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	stillSuppressed := service.ResolveCompactHistory(input, ResolveCompactOptions{})
	if stillSuppressed.HistoryRef != nil {
		t.Fatalf("stillSuppressed historyRef = %+v, want nil", stillSuppressed.HistoryRef)
	}

	forced := service.ResolveCompactHistory(input, ResolveCompactOptions{ForceDiscovery: true})
	if forced.HistoryRef == nil || forced.HistoryRef.Source != SourceAntigravitySQLite ||
		forced.HistoryRef.Value != id {
		t.Fatalf("forced historyRef = %+v, want source %q and value %q",
			forced.HistoryRef, SourceAntigravitySQLite, id)
	}
	if forced.CompactHistory.Source == nil ||
		*forced.CompactHistory.Source != string(SourceAntigravitySQLite) {
		t.Fatalf("forced compactHistory.source = %+v, want %q",
			forced.CompactHistory.Source, SourceAntigravitySQLite)
	}
	// The forced lookup really ran, yet the discovery warning is still reported
	// once per official value: forcing adds no warning noise. (The db written
	// here is an empty file, so the reader's own "no assistant message" warning
	// is counted separately.)
	if len(*discoveryRecords) != 1 {
		t.Fatalf("discovery warnings = %d, want 1: %+v", len(*discoveryRecords), *discoveryRecords)
	}
}
