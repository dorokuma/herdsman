package agenthistory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// Ported from test/unit/agent-history-discovery.test.ts and
// test/unit/agent-history-discovery-regressions.test.ts.
//
// The "occupied fallback discovery" case of test/unit/final-audit-regressions.test.ts
// is deliberately not ported: it built two literal sets inside the test file and
// asserted their sorted join differed, without touching production code. The
// behaviour it named lives in the agent context service, where the occupied set
// is fingerprinted and a change forces discovery again; that path is covered for
// real by TestAgentContextServiceRediscoversOnOccupiedChange in
// go/internal/observability/agent_context_service_test.go, which drives
// AgentContextService.RefreshAgent and asserts the second resolve flips
// forceDiscovery to true.

// strp is the pointer helper for the optional string fields of the ported input
// records.
func strp(value string) *string { return &value }

func resetDiscoveryFailureMemory() {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	failedDiscoveryAt = map[string]int64{}
	failedDiscoveryOrder = nil
	warnedDiscoveryKeys = map[string]struct{}{}
	warnedDiscoveryOrder = nil
}

// setDiscoveryWarnSinkForTest installs sink for the duration of the test and
// restores the previous one afterwards.
func setDiscoveryWarnSinkForTest(t *testing.T, sink func(message string, warning DiscoveryWarning)) {
	t.Helper()
	discoveryMu.Lock()
	previous := discoveryWarnSink
	discoveryWarnSink = sink
	discoveryMu.Unlock()
	t.Cleanup(func() {
		discoveryMu.Lock()
		discoveryWarnSink = previous
		discoveryMu.Unlock()
	})
}

type discoveryWarningRecord struct {
	message string
	warning DiscoveryWarning
}

// captureDiscoveryWarnings collects every warning the discovery path emits.
func captureDiscoveryWarnings(t *testing.T) *[]discoveryWarningRecord {
	t.Helper()
	records := &[]discoveryWarningRecord{}
	setDiscoveryWarnSinkForTest(t, func(message string, warning DiscoveryWarning) {
		*records = append(*records, discoveryWarningRecord{message: message, warning: warning})
	})
	return records
}

// silenceDiscoveryWarnings drops the stderr noise of failures a test does not
// assert on.
func silenceDiscoveryWarnings(t *testing.T) {
	t.Helper()
	setDiscoveryWarnSinkForTest(t, func(string, DiscoveryWarning) {})
}

func hasWarning(records []discoveryWarningRecord, detailPart string) bool {
	for _, record := range records {
		if strings.Contains(record.message, DiscoveryWarnMessage) &&
			strings.Contains(record.warning.Detail, detailPart) {
			return true
		}
	}
	return false
}

func requireWarning(t *testing.T, records []discoveryWarningRecord, detailPart string) {
	t.Helper()
	if hasWarning(records, detailPart) {
		return
	}
	t.Fatalf("no warning with detail containing %q; got %d warning(s): %+v", detailPart, len(records), records)
}

func writeSessionFile(t *testing.T, path string, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const agyTestID = "01714b30-5613-49c9-b3a2-ce3f9e7406c1"

func agyConversations(t *testing.T, homeDir string) string {
	t.Helper()
	dir := filepath.Join(homeDir, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

type agyLookupOptions struct {
	extraHomeDirs []string
	paneID        *string
}

func agyLookup(t *testing.T, homeDir string, value string, options agyLookupOptions) *HistoryRef {
	t.Helper()
	return DiscoverAgentHistory(AgentHistoryLookupInput{
		Agent: strp("agy"),
		AgentSession: &observability.AgentSessionRef{
			Agent:  "agy",
			Kind:   observability.SessionRefID,
			Source: "herdr:antigravity_cli",
			Value:  value,
		},
		HomeDir:       strp(homeDir),
		ExtraHomeDirs: options.extraHomeDirs,
		PaneID:        options.paneID,
	}, DiscoverOptions{})
}

func requireRefEqual(t *testing.T, got *HistoryRef, want HistoryRef) {
	t.Helper()
	if got == nil {
		t.Fatalf("discoverAgentHistory = nil, want %+v", want)
	}
	if got.Kind != want.Kind || got.Source != want.Source || got.Value != want.Value {
		t.Fatalf("discoverAgentHistory = %+v, want %+v", *got, want)
	}
	if (got.Path == nil) != (want.Path == nil) {
		t.Fatalf("discoverAgentHistory path presence = %v, want %v", got.Path, want.Path)
	}
	if got.Path != nil && *got.Path != *want.Path {
		t.Fatalf("discoverAgentHistory path = %q, want %q", *got.Path, *want.Path)
	}
}

func TestDiscoveryTakesTheOfficialHerdrAgentSessionPathAsTheHistoryReference(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	sessionPath := writeSessionFile(t, filepath.Join(homeDir, ".pi", "agent", "sessions", "ses-1.jsonl"), "{}\n")

	requireRefEqual(t, DiscoverAgentHistory(AgentHistoryLookupInput{
		Agent: strp("pi"),
		AgentSession: &observability.AgentSessionRef{
			Agent: "pi", Kind: observability.SessionRefPath, Source: "herdr:pi", Value: sessionPath,
		},
		HomeDir:          strp(homeDir),
		HerdrSessionName: strp("default"),
		PaneID:           strp("wA:p1"),
	}, DiscoverOptions{}), HistoryRef{
		Kind: KindAgentSession, Path: &sessionPath, Source: SourcePiJSONL, Value: sessionPath,
	})
}

func TestDiscoveryAcceptsAnOfficialPathOutsideTheHerdsmanPiSessionRoots(t *testing.T) {
	resetDiscoveryFailureMemory()
	// The path channel is generic: Herdr hands herdsman the value an installed
	// agent integration reported, so the root of that value is not whitelisted
	// (the whitelist only guards the herdsman-pi registration protocol). The
	// reference is returned as-is; whether a reader supports the agent is a
	// separate question, and the source is mapped honestly.
	homeDir := t.TempDir()
	sessionPath := writeSessionFile(t, filepath.Join(homeDir, ".omp", "sessions", "ses-2.jsonl"), "{}\n")

	requireRefEqual(t, DiscoverAgentHistory(AgentHistoryLookupInput{
		Agent: strp("omp"),
		AgentSession: &observability.AgentSessionRef{
			Agent: "omp", Kind: observability.SessionRefPath, Source: "herdr:omp", Value: sessionPath,
		},
		HomeDir: strp(homeDir),
	}, DiscoverOptions{}), HistoryRef{
		Kind: KindAgentSession, Path: &sessionPath, Source: SourceUnknown, Value: sessionPath,
	})
}

func TestDiscoveryReportsAndRefusesAMissingOfficialAgentSessionInsteadOfScanning(t *testing.T) {
	resetDiscoveryFailureMemory()
	// The pane's own conversation database exists under the temp home. Discovery
	// must not fall back to it: with no official value on the pane, herdsman has
	// nothing to read.
	homeDir := t.TempDir()
	conversations := agyConversations(t, homeDir)
	writeSessionFile(t, filepath.Join(conversations, agyTestID+".db"), "")
	records := captureDiscoveryWarnings(t)

	if got := DiscoverAgentHistory(AgentHistoryLookupInput{
		Agent:            strp("agy"),
		AgentSession:     nil,
		HerdrSessionName: strp("default"),
		PaneID:           strp("wA:p2"),
	}, DiscoverOptions{}); got != nil {
		t.Fatalf("discoverAgentHistory = %+v, want nil", got)
	}
	requireWarning(t, *records, "no agent_session")
	for _, record := range *records {
		if record.warning.Agent == nil || *record.warning.Agent != "agy" {
			t.Fatalf("warning agent = %+v, want agy", record.warning.Agent)
		}
		if record.warning.HerdrSessionName == nil || *record.warning.HerdrSessionName != "default" {
			t.Fatalf("warning herdrSessionName = %+v, want default", record.warning.HerdrSessionName)
		}
		if record.warning.PaneID == nil || *record.warning.PaneID != "wA:p2" {
			t.Fatalf("warning paneId = %+v, want wA:p2", record.warning.PaneID)
		}
	}
}

func TestDiscoveryResolvesAnOfficialAgyConversationIDToItsConversationDatabase(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	dbPath := writeSessionFile(t, filepath.Join(agyConversations(t, homeDir), agyTestID+".db"), "")

	requireRefEqual(t, agyLookup(t, homeDir, agyTestID, agyLookupOptions{}), HistoryRef{
		Kind: KindAgentSession, Path: &dbPath, Source: SourceAntigravitySQLite, Value: agyTestID,
	})
}

func TestDiscoveryReportsAndRefusesAnAgyConversationIDWithNoDatabase(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	agyConversations(t, homeDir)
	records := captureDiscoveryWarnings(t)

	if got := agyLookup(t, homeDir, agyTestID, agyLookupOptions{paneID: strp("wA:p3")}); got != nil {
		t.Fatalf("discoverAgentHistory = %+v, want nil", got)
	}
	requireWarning(t, *records, "is not a readable session file")
	for _, record := range *records {
		if record.warning.PaneID == nil || *record.warning.PaneID != "wA:p3" {
			t.Fatalf("warning paneId = %+v, want wA:p3", record.warning.PaneID)
		}
	}
}

func TestDiscoveryNeverJoinsANonUuidAgyIDIntoTheConversationsPath(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	// `conversations/../escape.db` normalises to this file, so only the uuid
	// guard keeps the id from naming a conversation outside the store.
	root := filepath.Join(homeDir, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(filepath.Join(root, "conversations"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSessionFile(t, filepath.Join(root, "escape.db"), "")
	records := captureDiscoveryWarnings(t)

	if got := agyLookup(t, homeDir, "../escape", agyLookupOptions{paneID: strp("wA:p7")}); got != nil {
		t.Fatalf("discoverAgentHistory = %+v, want nil", got)
	}
	requireWarning(t, *records, "agy conversation id is not a uuid")
}

func TestDiscoveryRefusesASymlinkedOrGroupWritableAgyConversationDatabase(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	conversations := agyConversations(t, homeDir)
	// One id per attempt: a repeated *failed* lookup for the same official
	// values is deliberately suppressed by the failure memory.
	gateID := "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"
	okID := "1f1f1f1f-1f1f-4f1f-8f1f-1f1f1f1f1f1f"
	linkID := "2f2f2f2f-2f2f-4f2f-8f2f-2f2f2f2f2f2f"
	gateDb := writeSessionFile(t, filepath.Join(conversations, gateID+".db"), "")
	writeSessionFile(t, filepath.Join(conversations, okID+".db"), "")
	linkDb := writeSessionFile(t, filepath.Join(conversations, linkID+".db"), "")
	records := captureDiscoveryWarnings(t)

	// Group-writable: another principal could rewrite the conversation store.
	if err := os.Chmod(gateDb, 0o664); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if got := agyLookup(t, homeDir, gateID, agyLookupOptions{paneID: strp("wA:p12")}); got != nil {
		t.Fatalf("group-writable lookup = %+v, want nil", got)
	}
	requireWarning(t, *records, "not a readable session file")

	if err := os.Chmod(filepath.Join(conversations, okID+".db"), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if got := agyLookup(t, homeDir, okID, agyLookupOptions{}); got == nil ||
		got.Source != SourceAntigravitySQLite {
		t.Fatalf("readable lookup = %+v, want source %q", got, SourceAntigravitySQLite)
	}

	// Symlinked database: the gate must see the link, not its target.
	elsewhere := writeSessionFile(t, filepath.Join(homeDir, "elsewhere.db"), "")
	if err := os.Remove(linkDb); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(elsewhere, linkDb); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if got := agyLookup(t, homeDir, linkID, agyLookupOptions{}); got != nil {
		t.Fatalf("symlinked lookup = %+v, want nil", got)
	}
}

func TestDiscoveryReportsAndRefusesAnOfficialIDForAnyOtherAgent(t *testing.T) {
	resetDiscoveryFailureMemory()
	// Only pi (official path) and agy (official id) are supported agents. Every
	// other agent is refused with the same fail-closed warning, whether or not
	// Herdr has an integration for it — this is a deliberate, documented limit.
	homeDir := t.TempDir()
	records := captureDiscoveryWarnings(t)

	for _, agent := range []string{"claude", "codex", "opencode", "grok", "gemini", "omp"} {
		if got := DiscoverAgentHistory(AgentHistoryLookupInput{
			Agent: strp(agent),
			AgentSession: &observability.AgentSessionRef{
				Agent: agent, Kind: observability.SessionRefID, Source: "herdr:" + agent, Value: "ses_1",
			},
			HomeDir:          strp(homeDir),
			HerdrSessionName: strp("default"),
			PaneID:           strp("wA:p4"),
		}, DiscoverOptions{}); got != nil {
			t.Fatalf("agent %q lookup = %+v, want nil", agent, got)
		}
	}
	requireWarning(t, *records, "is not a supported herdsman history agent")
	for _, record := range *records {
		if record.warning.HerdrSessionName == nil || *record.warning.HerdrSessionName != "default" {
			t.Fatalf("warning herdrSessionName = %+v, want default", record.warning.HerdrSessionName)
		}
		if record.warning.PaneID == nil || *record.warning.PaneID != "wA:p4" {
			t.Fatalf("warning paneId = %+v, want wA:p4", record.warning.PaneID)
		}
	}
}

func TestDiscoveryReportsAndRefusesAnOfficialPathThatIsNotAReadableSessionFile(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	records := captureDiscoveryWarnings(t)

	if got := DiscoverAgentHistory(AgentHistoryLookupInput{
		Agent: strp("pi"),
		AgentSession: &observability.AgentSessionRef{
			Agent: "pi", Kind: observability.SessionRefPath, Source: "herdr:pi",
			Value: filepath.Join(homeDir, "gone.jsonl"),
		},
		HomeDir: strp(homeDir),
		PaneID:  strp("wA:p5"),
	}, DiscoverOptions{}); got != nil {
		t.Fatalf("discoverAgentHistory = %+v, want nil", got)
	}
	requireWarning(t, *records, "not a readable session file")
	for _, record := range *records {
		if record.warning.PaneID == nil || *record.warning.PaneID != "wA:p5" {
			t.Fatalf("warning paneId = %+v, want wA:p5", record.warning.PaneID)
		}
	}
}

func TestHistorySourceFromSessionRefMapsTheSupportedAgentsAndNothingElse(t *testing.T) {
	source := func(agent string, kind observability.AgentSessionRefKind) HistorySource {
		return HistorySourceFromSessionRef(observability.AgentSessionRef{
			Agent: agent, Kind: kind, Source: "herdr:" + agent, Value: "/tmp/x.jsonl",
		})
	}
	if got := source("pi", observability.SessionRefPath); got != SourcePiJSONL {
		t.Fatalf("pi source = %q, want %q", got, SourcePiJSONL)
	}
	// pig (PiG) emits the same pi-jsonl session format and herdsman protocol, so it reads
	// through the same reader.
	if got := source("pig", observability.SessionRefPath); got != SourcePiJSONL {
		t.Fatalf("pig source = %q, want %q", got, SourcePiJSONL)
	}
	if got := HistorySourceFromSessionRef(observability.AgentSessionRef{
		Agent: "pig", Kind: observability.SessionRefPath,
		Source: "herdr:pig", Value: "/root/.pig/agent/sessions/ses-1.jsonl",
	}); got != SourcePiJSONL {
		t.Fatalf("herdr:pig source = %q, want %q", got, SourcePiJSONL)
	}
	if got := HistorySourceFromSessionRef(observability.AgentSessionRef{
		Agent: "antigravity_cli", Kind: observability.SessionRefID,
		Source: "herdr:antigravity_cli", Value: agyTestID,
	}); got != SourceAntigravitySQLite {
		t.Fatalf("antigravity_cli source = %q, want %q", got, SourceAntigravitySQLite)
	}
	// Everything else is `unknown`, which no reader accepts.
	for _, agent := range []string{"claude", "codex", "gemini", "grok", "omp", "opencode"} {
		if got := source(agent, observability.SessionRefPath); got != SourceUnknown {
			t.Fatalf("agent %q source = %q, want %q", agent, got, SourceUnknown)
		}
	}
}

func TestHistorySourceFromSessionRefMatchesWholeSegments(t *testing.T) {
	source := func(agent string) HistorySource {
		return HistorySourceFromSessionRef(observability.AgentSessionRef{
			Agent: agent, Kind: observability.SessionRefPath, Source: "herdr:" + agent, Value: "/tmp/x.jsonl",
		})
	}
	// `herdr:copilot` contains "pi" but is not a pi session: a substring match
	// would hand a copilot path to the pi reader.
	if got := source("copilot"); got != SourceUnknown {
		t.Fatalf("copilot source = %q, want %q", got, SourceUnknown)
	}
	if got := source("pi"); got != SourcePiJSONL {
		t.Fatalf("pi source = %q, want %q", got, SourcePiJSONL)
	}
	// `herdr:omp` is a pi-family agent with no reader, so it must map to
	// `unknown` rather than being silently read as pi (documented gap).
	if got := source("omp"); got != SourceUnknown {
		t.Fatalf("omp source = %q, want %q", got, SourceUnknown)
	}
	if got := HistorySourceFromSessionRef(observability.AgentSessionRef{
		Agent: "antigravity", Kind: observability.SessionRefPath,
		Source: "herdr:antigravity", Value: agyTestID,
	}); got != SourceAntigravitySQLite {
		t.Fatalf("antigravity source = %q, want %q", got, SourceAntigravitySQLite)
	}
}

func TestDiscoveryRejectsASessionFileThatIsItselfASymlinkButNotOneBehindALinkedDirectory(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	sessions := filepath.Join(homeDir, ".pi", "agent", "sessions")
	real := writeSessionFile(t, filepath.Join(sessions, "real.jsonl"), "{}\n")
	link := filepath.Join(sessions, "link.jsonl")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// The check has to happen before the realpath resolution, otherwise the
	// resolved target is inspected and the symlink is invisible.
	if got := SafeOfficialSessionPath(link); got != "" {
		t.Fatalf("safeOfficialSessionPath(link) = %q, want empty", got)
	}
	if got := SafeAllowedSessionPath(link, strp(homeDir)); got != "" {
		t.Fatalf("safeAllowedSessionPath(link) = %q, want empty", got)
	}
	if got := SafeOfficialSessionPath(real); got != real {
		t.Fatalf("safeOfficialSessionPath(real) = %q, want %q", got, real)
	}
	if got := SafeAllowedSessionPath(real, strp(homeDir)); got != real {
		t.Fatalf("safeAllowedSessionPath(real) = %q, want %q", got, real)
	}

	records := captureDiscoveryWarnings(t)
	if got := DiscoverAgentHistory(AgentHistoryLookupInput{
		Agent: strp("pi"),
		AgentSession: &observability.AgentSessionRef{
			Agent: "pi", Kind: observability.SessionRefPath, Source: "herdr:pi", Value: link,
		},
		HomeDir: strp(homeDir),
		PaneID:  strp("wA:p11"),
	}, DiscoverOptions{}); got != nil {
		t.Fatalf("discoverAgentHistory(link) = %+v, want nil", got)
	}
	requireWarning(t, *records, "not a readable session file")

	// Only the file itself is checked: an official path reached through a
	// symlinked *parent* directory is a normal file and stays readable.
	linkedRoot := filepath.Join(homeDir, "linked-root")
	if err := os.Symlink(sessions, linkedRoot); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	viaLinkedDir := filepath.Join(linkedRoot, "real.jsonl")
	got := DiscoverAgentHistory(AgentHistoryLookupInput{
		Agent: strp("pi"),
		AgentSession: &observability.AgentSessionRef{
			Agent: "pi", Kind: observability.SessionRefPath, Source: "herdr:pi", Value: viaLinkedDir,
		},
		HomeDir: strp(homeDir),
	}, DiscoverOptions{})
	if got == nil || got.Path == nil || *got.Path != real {
		t.Fatalf("discoverAgentHistory(viaLinkedDir) = %+v, want path %q", got, real)
	}
}

func TestDiscoveryDoesNotRepeatAFailedLookupNorItsWarningUntilTheOfficialValuesChange(t *testing.T) {
	resetDiscoveryFailureMemory()
	homeDir := t.TempDir()
	conversations := agyConversations(t, homeDir)
	records := captureDiscoveryWarnings(t)
	lookup := func(value string) *HistoryRef {
		return agyLookup(t, homeDir, value, agyLookupOptions{paneID: strp("wA:p13")})
	}

	if got := lookup(agyTestID); got != nil {
		t.Fatalf("first lookup = %+v, want nil", got)
	}
	if len(*records) != 1 {
		t.Fatalf("warnings after first failure = %d, want 1", len(*records))
	}

	// The conversation database appears, but the same official values are still
	// inside their retry window: the pane is not re-resolved and does not warn
	// again.
	writeSessionFile(t, filepath.Join(conversations, agyTestID+".db"), "")
	if got := lookup(agyTestID); got != nil {
		t.Fatalf("cached-failure lookup = %+v, want nil", got)
	}
	if len(*records) != 1 {
		t.Fatalf("warnings after cached failure = %d, want 1", len(*records))
	}

	// A different official value is a different key and is attempted at once.
	otherID := "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	otherDb := writeSessionFile(t, filepath.Join(conversations, otherID+".db"), "")
	requireRefEqual(t, lookup(otherID), HistoryRef{
		Kind: KindAgentSession, Path: &otherDb, Source: SourceAntigravitySQLite, Value: otherID,
	})
	if len(*records) != 1 {
		t.Fatalf("warnings after a different official value = %d, want 1", len(*records))
	}
}

func TestDiscoveryResolvesAgyConversationsAcrossPrimaryHomeAndExtraHomes(t *testing.T) {
	resetDiscoveryFailureMemory()
	ultraHome := t.TempDir()
	proHome := t.TempDir()
	missingHome := filepath.Join(ultraHome, "nonexistent-extra-home")
	ultraConversations := agyConversations(t, ultraHome)
	proConversations := agyConversations(t, proHome)
	ultraID := "11111111-2222-4333-8444-555555555555"
	proID := "66666666-7777-4888-8999-aaaaaaaaaaaa"
	ultraDb := writeSessionFile(t, filepath.Join(ultraConversations, ultraID+".db"), "")
	proDb := writeSessionFile(t, filepath.Join(proConversations, proID+".db"), "")
	silenceDiscoveryWarnings(t)

	t.Setenv(AgyExtraHomesEnv, missingHome+":"+proHome)
	requireRefEqual(t, agyLookup(t, ultraHome, ultraID, agyLookupOptions{}), HistoryRef{
		Kind: KindAgentSession, Path: &ultraDb, Source: SourceAntigravitySQLite, Value: ultraID,
	})
	requireRefEqual(t, agyLookup(t, ultraHome, proID, agyLookupOptions{}), HistoryRef{
		Kind: KindAgentSession, Path: &proDb, Source: SourceAntigravitySQLite, Value: proID,
	})
}

func TestDiscoveryDeduplicatesSymlinkedAgyConversationStoresByRealpathAndKeepsUltraPriority(t *testing.T) {
	resetDiscoveryFailureMemory()
	ultraHome := t.TempDir()
	proHome := t.TempDir()
	ultraConversations := agyConversations(t, ultraHome)
	proGemini := filepath.Join(proHome, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(proGemini, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(ultraConversations, filepath.Join(proGemini, "conversations")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	silenceDiscoveryWarnings(t)

	sharedID := "cccccccc-dddd-4eee-8fff-000000000001"
	ultraDb := writeSessionFile(t, filepath.Join(ultraConversations, sharedID+".db"), "")

	candidates := ResolveAgySessionCandidates(AgySessionCandidatesInput{
		ExtraHomeDirs: []string{proHome},
		HomeDir:       strp(ultraHome),
		ID:            sharedID,
	})
	if len(candidates.Refs) != 1 {
		t.Fatalf("candidates.refs = %+v, want exactly one entry", candidates.Refs)
	}
	requireRefEqual(t, &candidates.Refs[0], HistoryRef{
		Kind: KindAgentSession, Path: &ultraDb, Source: SourceAntigravitySQLite, Value: sharedID,
	})
	requireRefEqual(t, agyLookup(t, ultraHome, sharedID, agyLookupOptions{extraHomeDirs: []string{proHome}}),
		HistoryRef{
			Kind: KindAgentSession, Path: &ultraDb, Source: SourceAntigravitySQLite, Value: sharedID,
		})

	// Independent directories with the same conversation id still prefer primary (Ultra) HOME.
	proIndependentHome := t.TempDir()
	proIndependentConversations := agyConversations(t, proIndependentHome)
	writeSessionFile(t, filepath.Join(proIndependentConversations, sharedID+".db"), "")
	requireRefEqual(t,
		agyLookup(t, ultraHome, sharedID, agyLookupOptions{extraHomeDirs: []string{proIndependentHome}}),
		HistoryRef{
			Kind: KindAgentSession, Path: &ultraDb, Source: SourceAntigravitySQLite, Value: sharedID,
		})
}

func TestDiscoveryEnforcesFileSafetyGatesOnExtraAgyHomes(t *testing.T) {
	resetDiscoveryFailureMemory()
	ultraHome := t.TempDir()
	proHome := t.TempDir()
	agyConversations(t, ultraHome)
	proConversations := agyConversations(t, proHome)
	writableID := "cccccccc-dddd-4eee-8fff-000000000002"
	symlinkID := "cccccccc-dddd-4eee-8fff-000000000003"
	writableDb := writeSessionFile(t, filepath.Join(proConversations, writableID+".db"), "")
	if err := os.Chmod(writableDb, 0o664); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	targetDb := writeSessionFile(t, filepath.Join(proHome, "outside.db"), "")
	if err := os.Symlink(targetDb, filepath.Join(proConversations, symlinkID+".db")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	silenceDiscoveryWarnings(t)

	if got := agyLookup(t, ultraHome, writableID, agyLookupOptions{extraHomeDirs: []string{proHome}}); got != nil {
		t.Fatalf("group-writable extra home lookup = %+v, want nil", got)
	}
	if got := agyLookup(t, ultraHome, symlinkID, agyLookupOptions{extraHomeDirs: []string{proHome}}); got != nil {
		t.Fatalf("symlinked extra home lookup = %+v, want nil", got)
	}
}

func TestDiscoveryInvalidatesNegativeCacheWhenAgyExtraHomesChangeWithinTTL(t *testing.T) {
	resetDiscoveryFailureMemory()
	ultraHome := t.TempDir()
	proHome := t.TempDir()
	agyConversations(t, ultraHome)
	proConversations := agyConversations(t, proHome)
	negCacheID := "cccccccc-dddd-4eee-8fff-000000000004"
	proDb := writeSessionFile(t, filepath.Join(proConversations, negCacheID+".db"), "")
	records := captureDiscoveryWarnings(t)

	// Phase 1: with no extra homes configured, lookup misses and enters the
	// negative cache.
	t.Setenv(AgyExtraHomesEnv, "")
	if got := agyLookup(t, ultraHome, negCacheID, agyLookupOptions{paneID: strp("wA:p14")}); got != nil {
		t.Fatalf("first lookup = %+v, want nil", got)
	}
	if len(*records) != 1 {
		t.Fatalf("warnings after first failure = %d, want 1", len(*records))
	}
	if got := agyLookup(t, ultraHome, negCacheID, agyLookupOptions{paneID: strp("wA:p14")}); got != nil {
		t.Fatalf("cached-failure lookup = %+v, want nil", got)
	}
	if len(*records) != 1 {
		t.Fatalf("warnings after cached failure = %d, want 1", len(*records))
	}

	// Phase 2: configuring extra homes (via extraHomeDirs or AGY_EXTRA_HOMES)
	// within TTL without force immediately resolves because extra homes are part
	// of discoveryLookupKey.
	requireRefEqual(t,
		agyLookup(t, ultraHome, negCacheID, agyLookupOptions{extraHomeDirs: []string{proHome}, paneID: strp("wA:p14")}),
		HistoryRef{
			Kind: KindAgentSession, Path: &proDb, Source: SourceAntigravitySQLite, Value: negCacheID,
		})
	t.Setenv(AgyExtraHomesEnv, proHome)
	requireRefEqual(t, agyLookup(t, ultraHome, negCacheID, agyLookupOptions{paneID: strp("wA:p14")}),
		HistoryRef{
			Kind: KindAgentSession, Path: &proDb, Source: SourceAntigravitySQLite, Value: negCacheID,
		})
}

// --- regressions: the official-value-only contract --------------------------

const (
	herdrRoleSessionsRoot    = "/tmp/herdr-role-sessions"
	retiredRoleSessionsRoot  = "/tmp/pi-role-sessions"
	discoverySessionFileBody = "{\"cwd\":\"/repo\"}\n"
	nonexistentDiscoveryHome = "/nonexistent"
)

func tempRootUnder(t *testing.T, root string, name string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	dir, err := os.MkdirTemp(root, name)
	if err != nil {
		t.Fatalf("mkdtemp under %s: %v", root, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestSafeAllowedSessionPathKeepsTheRootWhitelistForHerdsmanPiRegistration(t *testing.T) {
	root := tempRootUnder(t, herdrRoleSessionsRoot, "herdsman-regression-")
	path := writeSessionFile(t, filepath.Join(root, "safe.jsonl"), discoverySessionFileBody)

	if got := SafeAllowedSessionPath(path, strp(nonexistentDiscoveryHome)); got != path {
		t.Fatalf("safeAllowedSessionPath(path) = %q, want %q", got, path)
	}
	if got := SafeAllowedSessionPath("relative/session.jsonl", strp(nonexistentDiscoveryHome)); got != "" {
		t.Fatalf("safeAllowedSessionPath(relative) = %q, want empty", got)
	}
	if got := SafeAllowedSessionPath(filepath.Join(root, "..", "escape"), strp(nonexistentDiscoveryHome)); got != "" {
		t.Fatalf("safeAllowedSessionPath(escape) = %q, want empty", got)
	}
	if got := SafeAllowedSessionPath("/tmp/not-an-allowed-root/session.jsonl", strp(nonexistentDiscoveryHome)); got != "" {
		t.Fatalf("safeAllowedSessionPath(other root) = %q, want empty", got)
	}
}

func TestRegistrationWhitelistAcceptsPigSessionFilesUnderThePigHome(t *testing.T) {
	home := t.TempDir()
	pigPath := writeSessionFile(t, filepath.Join(home, ".pig", "agent", "sessions", "pig.jsonl"), discoverySessionFileBody)
	if got := SafeAllowedSessionPath(pigPath, strp(home)); got != pigPath {
		t.Fatalf("safeAllowedSessionPath(pig path) = %q, want %q", got, pigPath)
	}
	if !SessionPathAllowedByShape(pigPath, strp(home)) {
		t.Fatal("sessionPathAllowedByShape(pig path) = false, want true")
	}
	// pi keeps working exactly as before: both agent homes are whitelisted.
	piPath := writeSessionFile(t, filepath.Join(home, ".pi", "agent", "sessions", "pi.jsonl"), discoverySessionFileBody)
	if got := SafeAllowedSessionPath(piPath, strp(home)); got != piPath {
		t.Fatalf("safeAllowedSessionPath(pi path) = %q, want %q", got, piPath)
	}
	// A sibling directory whose name merely starts with the root name must not
	// pass the containment check.
	sibling := writeSessionFile(t, filepath.Join(home, ".pig", "agent", "sessions-backup", "sibling.jsonl"), discoverySessionFileBody)
	if got := SafeAllowedSessionPath(sibling, strp(home)); got != "" {
		t.Fatalf("safeAllowedSessionPath(sibling) = %q, want empty", got)
	}
	if SessionPathAllowedByShape(sibling, strp(home)) {
		t.Fatal("sessionPathAllowedByShape(sibling) = true, want false")
	}
}

func TestSafeOfficialSessionPathKeepsTheFileSafetyChecksWithoutARootWhitelist(t *testing.T) {
	root := t.TempDir()
	path := writeSessionFile(t, filepath.Join(root, "official-session.jsonl"), discoverySessionFileBody)

	if got := SafeOfficialSessionPath(path); got != path {
		t.Fatalf("safeOfficialSessionPath(path) = %q, want %q", got, path)
	}
	if got := SafeOfficialSessionPath("relative/session.jsonl"); got != "" {
		t.Fatalf("safeOfficialSessionPath(relative) = %q, want empty", got)
	}
	if got := SafeOfficialSessionPath(filepath.Join(root, "..", "escape")); got != "" {
		t.Fatalf("safeOfficialSessionPath(escape) = %q, want empty", got)
	}
	if got := SafeOfficialSessionPath(filepath.Join(root, "missing.jsonl")); got != "" {
		t.Fatalf("safeOfficialSessionPath(missing) = %q, want empty", got)
	}

	directory := filepath.Join(root, "a-directory.jsonl")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := SafeOfficialSessionPath(directory); got != "" {
		t.Fatalf("safeOfficialSessionPath(directory) = %q, want empty", got)
	}
}

func TestAPlausibleSessionFileOnDiskIsNeverPickedUpWithoutAnOfficialValue(t *testing.T) {
	resetDiscoveryFailureMemory()
	root := t.TempDir()
	writeSessionFile(t, filepath.Join(root, ".pi", "agent", "sessions", "leaked.jsonl"), discoverySessionFileBody)
	records := captureDiscoveryWarnings(t)

	if got := DiscoverAgentHistory(AgentHistoryLookupInput{
		Agent:            strp("pi"),
		AgentSession:     nil,
		HerdrSessionName: strp("default"),
		HomeDir:          strp(root),
		PaneID:           strp("wA:p1"),
	}, DiscoverOptions{}); got != nil {
		t.Fatalf("discoverAgentHistory = %+v, want nil", got)
	}
	for _, record := range *records {
		if record.warning.PaneID == nil || *record.warning.PaneID != "wA:p1" {
			t.Fatalf("warning paneId = %+v, want wA:p1", record.warning.PaneID)
		}
	}
}

func TestAnOfficialIDIsNeverResolvedByWalkingASessionDirectory(t *testing.T) {
	resetDiscoveryFailureMemory()
	root := t.TempDir()
	records := captureDiscoveryWarnings(t)
	// Both files carry the official id in their name, which is exactly what the
	// deleted scanners matched on. Nothing walks these trees any more: an id is
	// only resolved through its agent's deterministic template (`agy`), so an
	// unsupported agent's session file sitting on disk stays unresolved.
	writeSessionFile(t,
		filepath.Join(root, ".codex", "sessions", "2026", "07", "09",
			"rollout-2026-07-09T10-00-00-aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl"),
		discoverySessionFileBody)
	writeSessionFile(t,
		filepath.Join(root, ".claude", "projects", "-repo", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl"),
		discoverySessionFileBody)

	for _, testCase := range []struct {
		agent  string
		detail string
	}{
		{"codex", "is not a supported herdsman history agent"},
		{"claude", "is not a supported herdsman history agent"},
		// pi *is* supported, so its id-shaped value must not be described as an
		// unsupported agent: it is the documented gap (no id-based derivation).
		{"pi", "reported an id; herdsman has no id-based session-file derivation"},
	} {
		if got := DiscoverAgentHistory(AgentHistoryLookupInput{
			Agent: strp(testCase.agent),
			AgentSession: &observability.AgentSessionRef{
				Agent: testCase.agent, Kind: observability.SessionRefID,
				Source: "herdr:" + testCase.agent, Value: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			},
			HerdrSessionName: strp("default"),
			HomeDir:          strp(root),
			PaneID:           strp("wA:p1"),
		}, DiscoverOptions{}); got != nil {
			t.Fatalf("agent %q lookup = %+v, want nil", testCase.agent, got)
		}
		requireWarning(t, *records, testCase.detail)
	}
	if len(*records) != 3 {
		t.Fatalf("warnings = %d, want 3: %+v", len(*records), *records)
	}
	for _, record := range *records {
		if record.warning.PaneID == nil || *record.warning.PaneID != "wA:p1" {
			t.Fatalf("warning paneId = %+v, want wA:p1", record.warning.PaneID)
		}
	}
}

func TestAnOfficialPathUnderARetiredRoleRootIsAcceptedButTheWhitelistRejectsIt(t *testing.T) {
	resetDiscoveryFailureMemory()
	oldRoot := tempRootUnder(t, retiredRoleSessionsRoot, "retired-")
	oldPath := writeSessionFile(t, filepath.Join(oldRoot, "role-old.jsonl"), discoverySessionFileBody)

	// The registration whitelist only knows the current role root.
	if got := SafeAllowedSessionPath(oldPath, strp(nonexistentDiscoveryHome)); got != "" {
		t.Fatalf("safeAllowedSessionPath(retired root) = %q, want empty", got)
	}
	// An official value is consumed as reported, whatever root it lives in.
	requireRefEqual(t, DiscoverAgentHistory(AgentHistoryLookupInput{
		Agent: strp("pi"),
		AgentSession: &observability.AgentSessionRef{
			Agent: "pi", Kind: observability.SessionRefPath, Source: "herdr:pi", Value: oldPath,
		},
		HerdrSessionName: strp("default"),
		PaneID:           strp("wA:p9"),
	}, DiscoverOptions{}), HistoryRef{
		Kind: KindAgentSession, Path: &oldPath, Source: SourcePiJSONL, Value: oldPath,
	})
}
