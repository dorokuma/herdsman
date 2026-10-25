package agenthistory

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// The agent history service, ported from src/agent-history/service.ts and the
// `historyHasAdvanced` guard of src/observability/agent-index-service.ts.

// AgentHistoryFormatterVersion mirrors agentHistoryFormatterVersion.
const AgentHistoryFormatterVersion = "agent-history-v3"

// HistoryCacheFreshInput mirrors the `getFresh` input of AgentHistoryCacheStore.
type HistoryCacheFreshInput struct {
	FormatterVersion string
	SourceMtimeMs    float64
	SourcePath       string
	SourceSize       int64
}

// HistoryCachePutInput mirrors the `put` input of AgentHistoryCacheStore.
type HistoryCachePutInput struct {
	CompactHistory   CompactAgentHistory
	FormatterVersion string
	HistoryRef       HistoryRef
	SourceMtimeMs    float64
	SourcePath       string
	SourceSize       int64
}

// HistoryCacheRecord mirrors AgentHistoryCacheRecord.
type HistoryCacheRecord struct {
	CompactHistory   CompactAgentHistory
	FormatterVersion string
	HistoryRef       HistoryRef
	ID               int64
	SourceMtimeMs    float64
	SourcePath       string
	SourceSize       int64
}

// HistoryCacheStore mirrors the `CacheLike` subset of AgentHistoryCacheStore the
// service depends on. GetFresh returns nil when nothing fresh is cached; Put
// returns the store's error (TS would throw instead).
type HistoryCacheStore interface {
	GetFresh(input HistoryCacheFreshInput) *HistoryCacheRecord
	Put(input HistoryCachePutInput) error
}

// HistoryDiscovery mirrors the injected `discover` dependency: the real
// discovery step, or a test double.
type HistoryDiscovery func(input AgentHistoryLookupInput, options DiscoverOptions) *HistoryRef

// AgentHistoryServiceOptions mirrors the createAgentHistoryService options.
type AgentHistoryServiceOptions struct {
	Cache    HistoryCacheStore
	Discover HistoryDiscovery
	HomeDir  *string
	Readers  []HistoryReader
}

// ResolvedCompactAgentHistory mirrors ResolvedCompactAgentHistory.
type ResolvedCompactAgentHistory struct {
	CompactHistory    CompactAgentHistory
	HistoryRef        *HistoryRef
	SourceFingerprint *SourceFingerprint
}

// HistoryReadResult mirrors the `{ historyRef, messages }` pair of `read`.
type HistoryReadResult struct {
	HistoryRef *HistoryRef
	Messages   []HistoryMessage
}

// ReadCompactOptions mirrors `{ forceRefresh?: boolean }`.
type ReadCompactOptions struct {
	ForceRefresh bool
}

// ResolveCompactOptions mirrors the resolveCompactHistory options.
type ResolveCompactOptions struct {
	ForceDiscovery bool
	ForceRefresh   bool
	PreferredRef   *HistoryRef
}

// ReadOptions mirrors the `read` options.
type ReadOptions struct {
	ForceDiscovery bool
	Limit          int
	PreferredRef   *HistoryRef
}

// AgentHistoryService mirrors the object createAgentHistoryService returns.
type AgentHistoryService struct {
	options             AgentHistoryServiceOptions
	readers             []HistoryReader
	discover            HistoryDiscovery
	discoverySuppressed func(input AgentHistoryLookupInput) bool
}

// CreateAgentHistoryService mirrors createAgentHistoryService.
func CreateAgentHistoryService(options AgentHistoryServiceOptions) *AgentHistoryService {
	// Exactly two agents have a reader: pi (official session file path) and agy
	// (official conversation id). Everything else resolves to `unknown` and is
	// rejected here, so a pane for another agent reports no history.
	readers := options.Readers
	if readers == nil {
		readers = []HistoryReader{NewPiHistoryReader(), NewAntigravityHistoryReader()}
	}
	discover := options.Discover
	if discover == nil {
		discover = func(input AgentHistoryLookupInput, discoverOptions DiscoverOptions) *HistoryRef {
			return DiscoverAgentHistory(withHomeDir(input, options.HomeDir), discoverOptions)
		}
	}
	service := &AgentHistoryService{options: options, readers: readers, discover: discover}
	// A pane whose official values just failed to resolve is not looked up again
	// (and not warned about again) until its retry window expires: the failure is
	// already reported once by `discoverAgentHistory`. Only the real discovery has
	// that memory, so an injected one keeps its own behaviour. The lookup key is
	// computed from the same input the discovery sees, `homeDir` included.
	service.discoverySuppressed = func(input AgentHistoryLookupInput) bool {
		if options.Discover != nil {
			return false
		}
		return DiscoveryFailureFresh(withHomeDir(input, options.HomeDir), time.Now().UnixMilli())
	}
	return service
}

func withHomeDir(input AgentHistoryLookupInput, homeDir *string) AgentHistoryLookupInput {
	if homeDir == nil || *homeDir == "" {
		return input
	}
	input.HomeDir = homeDir
	return input
}

// Discover runs the service's discovery step, real or injected.
func (s *AgentHistoryService) Discover(input AgentHistoryLookupInput, options DiscoverOptions) *HistoryRef {
	return s.discover(input, options)
}

// GetCompactHistory is the compatibility compact-history wrapper.
func (s *AgentHistoryService) GetCompactHistory(input AgentHistoryLookupInput) CompactAgentHistory {
	return s.ResolveCompactHistory(input, ResolveCompactOptions{}).CompactHistory
}

// ReadCompactRef mirrors readCompactRef.
func (s *AgentHistoryService) ReadCompactRef(historyRef HistoryRef, options ReadCompactOptions) ResolvedCompactAgentHistory {
	for _, candidate := range s.readers {
		if candidate.CanRead(historyRef) {
			return s.readCompactWith(candidate, historyRef, options)
		}
	}
	return unresolvedCompactHistory(sourceString(historyRef.Source))
}

func (s *AgentHistoryService) readCompactWith(
	reader HistoryReader,
	historyRef HistoryRef,
	options ReadCompactOptions,
) ResolvedCompactAgentHistory {
	path := historyRef.ResolvedPath()
	sourceFingerprint := StatSourceFingerprint(path)
	if sourceFingerprint == nil {
		return unresolvedCompactHistory(sourceString(historyRef.Source))
	}

	if !options.ForceRefresh && s.options.Cache != nil {
		cached := s.options.Cache.GetFresh(HistoryCacheFreshInput{
			FormatterVersion: AgentHistoryFormatterVersion,
			SourceMtimeMs:    sourceFingerprint.MtimeMs,
			SourcePath:       path,
			SourceSize:       sourceFingerprint.Size,
		})
		if cached != nil && cached.CompactHistory.LastAssistantMessage != nil {
			return ResolvedCompactAgentHistory{
				CompactHistory:    cached.CompactHistory,
				HistoryRef:        &historyRef,
				SourceFingerprint: sourceFingerprint,
			}
		}
	}

	compactHistory, err := reader.ReadCompact(historyRef)
	if err != nil {
		emitHistoryDiagnostic(DiagnosticWarn, "Herdsman could not read agent history", map[string]any{
			"path":    path,
			"source":  string(historyRef.Source),
			"error":   err.Error(),
			"size":    sourceFingerprint.Size,
			"mtimeMs": sourceFingerprint.MtimeMs,
		})
		return unresolvedCompactHistory(sourceString(historyRef.Source))
	}
	if compactHistory.LastAssistantMessage == nil {
		payload := map[string]any{
			"path":    path,
			"source":  string(historyRef.Source),
			"size":    sourceFingerprint.Size,
			"mtimeMs": sourceFingerprint.MtimeMs,
			"reason":  compactHistory.UnreadyReason,
		}
		if historyRef.Source == SourcePiJSONL {
			piEmptyAssistantCount.Add(1)
			emitHistoryDiagnostic(DiagnosticDebug,
				"Herdsman history read produced no assistant message", payload)
		} else {
			emitHistoryDiagnostic(DiagnosticWarn,
				"Herdsman history read produced no assistant message", payload)
		}
		return ResolvedCompactAgentHistory{
			CompactHistory:    compactHistory,
			HistoryRef:        &historyRef,
			SourceFingerprint: sourceFingerprint,
		}
	}
	if s.options.Cache != nil {
		_ = s.options.Cache.Put(HistoryCachePutInput{
			CompactHistory:   compactHistory,
			FormatterVersion: AgentHistoryFormatterVersion,
			HistoryRef:       historyRef,
			SourceMtimeMs:    sourceFingerprint.MtimeMs,
			SourcePath:       path,
			SourceSize:       sourceFingerprint.Size,
		})
	}
	return ResolvedCompactAgentHistory{
		CompactHistory:    compactHistory,
		HistoryRef:        &historyRef,
		SourceFingerprint: sourceFingerprint,
	}
}

// ResolveCompactHistory mirrors resolveCompactHistory.
func (s *AgentHistoryService) ResolveCompactHistory(
	input AgentHistoryLookupInput,
	options ResolveCompactOptions,
) ResolvedCompactAgentHistory {
	if options.PreferredRef != nil && !options.ForceDiscovery {
		preferred := s.ReadCompactRef(*options.PreferredRef, ReadCompactOptions{ForceRefresh: options.ForceRefresh})
		if preferred.HistoryRef != nil {
			return preferred
		}
	}
	// The failure memory only decelerates the ordinary refresh cadence: it skips
	// the lookup the daemon would otherwise repeat every round. An explicit
	// caller action (`forceDiscovery`) must get a real lookup instead, so it
	// bypasses both this skip and the memory inside `discoverAgentHistory`. The
	// warning is still deduplicated per key, so this adds no noise.
	forcedDiscovery := options.ForceDiscovery
	if !forcedDiscovery && s.discoverySuppressed(input) {
		return unresolvedCompactHistory(nil)
	}
	historyRef := s.discover(input, DiscoverOptions{Force: forcedDiscovery})
	if historyRef == nil {
		// `discoverAgentHistory` already warned about this failure with the richer
		// official values, so the real path must not log a second warning for the
		// same round. An injected discovery has no such report, so keep this one.
		if s.options.Discover != nil {
			emitHistoryDiagnostic(DiagnosticWarn,
				"Herdsman agent history discovery returned no reference", input)
		}
		return unresolvedCompactHistory(nil)
	}
	return s.ReadCompactRef(*historyRef, ReadCompactOptions{ForceRefresh: options.ForceRefresh})
}

// Read mirrors the service's live read.
func (s *AgentHistoryService) Read(input AgentHistoryLookupInput, options ReadOptions) HistoryReadResult {
	// `forceDiscovery` means "this caller asked right now": skip the persisted
	// ref like `resolveCompactHistory` does, and do not let the failure memory
	// answer with a memoized empty history.
	if options.PreferredRef != nil && !options.ForceDiscovery {
		preferred := s.readRef(*options.PreferredRef, options.Limit)
		if preferred.HistoryRef != nil {
			return preferred
		}
	}
	if !options.ForceDiscovery && s.discoverySuppressed(input) {
		return HistoryReadResult{}
	}
	historyRef := s.discover(input, DiscoverOptions{Force: options.ForceDiscovery})
	if historyRef == nil {
		return HistoryReadResult{}
	}
	return s.readRef(*historyRef, options.Limit)
}

func (s *AgentHistoryService) readRef(historyRef HistoryRef, limit int) HistoryReadResult {
	path := historyRef.ResolvedPath()
	var reader HistoryReader
	for _, candidate := range s.readers {
		if candidate.CanRead(historyRef) {
			reader = candidate
			break
		}
	}
	if reader == nil || statOrNil(path) == nil {
		emitHistoryDiagnostic(DiagnosticWarn, "Herdsman history reference could not be resolved",
			map[string]any{"path": path, "source": string(historyRef.Source)})
		return HistoryReadResult{}
	}
	messages, err := reader.Read(historyRef, &limit)
	if err != nil {
		emitHistoryDiagnostic(DiagnosticWarn, "Herdsman could not read agent history messages",
			map[string]any{"path": path, "source": string(historyRef.Source), "error": err.Error()})
		return HistoryReadResult{}
	}
	return HistoryReadResult{HistoryRef: &historyRef, Messages: messages}
}

func unresolvedCompactHistory(source *string) ResolvedCompactAgentHistory {
	return ResolvedCompactAgentHistory{
		CompactHistory:    EmptyCompactHistory(source),
		HistoryRef:        nil,
		SourceFingerprint: nil,
	}
}

// EmptyCompactHistory mirrors emptyCompactHistory: a nil source means TS `null`.
func EmptyCompactHistory(source *string) CompactAgentHistory {
	return CompactAgentHistory{
		HistoryRef:           nil,
		LastAssistantMessage: nil,
		LastToolResult:       nil,
		LastUserMessage:      nil,
		MessageCount:         0,
		Source:               source,
		UpdatedAt:            nil,
	}
}

func sourceString(source HistorySource) *string {
	value := string(source)
	return &value
}

// HistoryHasAdvanced mirrors historyHasAdvanced of
// src/observability/agent-index-service.ts: true when the compact history moved
// past its baseline. A changed assistant reference or text always counts; the
// message count only counts when a change is not required.
func HistoryHasAdvanced(
	current CompactAgentHistory,
	baseline *CompactAgentHistory,
	options HistoryAdvancedOptions,
) bool {
	var baselineRef *string
	var baselineText *string
	baselineCount := 0
	if baseline != nil {
		if baseline.LastAssistantMessage != nil {
			baselineRef = &baseline.LastAssistantMessage.Ref
			baselineText = &baseline.LastAssistantMessage.Text
		}
		baselineCount = baseline.MessageCount
	}
	if current.LastAssistantMessage != nil {
		if current.LastAssistantMessage.Ref != "" && !sameOptionalString(&current.LastAssistantMessage.Ref, baselineRef) {
			return true
		}
		if current.LastAssistantMessage.Text != "" && !sameOptionalString(&current.LastAssistantMessage.Text, baselineText) {
			return true
		}
	}
	if options.RequireAssistantChange {
		return false
	}
	return current.MessageCount > baselineCount
}

// HistoryAdvancedOptions mirrors `{ requireAssistantChange?: boolean }`.
type HistoryAdvancedOptions struct {
	RequireAssistantChange bool
}

// sameOptionalString mirrors `a !== (b ?? null)`: an absent baseline is null, so
// any present value differs from it.
func sameOptionalString(current *string, baseline *string) bool {
	if baseline == nil {
		return false
	}
	return *current == *baseline
}

// --- diagnostics ------------------------------------------------------------

// HistoryDiagnosticLevel distinguishes console.debug from console.warn.
type HistoryDiagnosticLevel string

const (
	DiagnosticDebug HistoryDiagnosticLevel = "debug"
	DiagnosticWarn  HistoryDiagnosticLevel = "warn"
)

// HistoryDiagnostic mirrors one console.debug / console.warn call: the message
// plus whatever object TS would have logged next to it.
type HistoryDiagnostic struct {
	Level   HistoryDiagnosticLevel
	Message string
	Payload any
}

var (
	historyDiagnosticMu   sync.Mutex
	historyDiagnosticSink func(HistoryDiagnostic) = func(diagnostic HistoryDiagnostic) {
		fmt.Fprintf(os.Stderr, "[%s] %s %v\n", diagnostic.Level, diagnostic.Message, diagnostic.Payload)
	}
)

func emitHistoryDiagnostic(level HistoryDiagnosticLevel, message string, payload any) {
	historyDiagnosticMu.Lock()
	sink := historyDiagnosticSink
	historyDiagnosticMu.Unlock()
	sink(HistoryDiagnostic{Level: level, Message: message, Payload: payload})
}

// --- pi empty-assistant counter --------------------------------------------

var piEmptyAssistantCount atomic.Int64

// GetPiEmptyAssistantCount mirrors getPiEmptyAssistantCount.
func GetPiEmptyAssistantCount() int {
	return int(piEmptyAssistantCount.Load())
}

// ResetPiEmptyAssistantCount mirrors resetPiEmptyAssistantCount.
func ResetPiEmptyAssistantCount() {
	piEmptyAssistantCount.Store(0)
}
