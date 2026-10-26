package daemon

import (
	"github.com/dorokuma/herdsman/go/internal/agenthistory"
	"github.com/dorokuma/herdsman/go/internal/observability"
)

// This file is the production adapter between internal/agenthistory and the
// observability-side interfaces the daemon wires together.
//
// Why it exists: internal/agenthistory imports internal/observability, so the
// agent-history records cannot be shared Go types. Each package therefore
// declares its own copy of the history contracts (identical JSON tags, distinct
// Go types) and internal/observability expresses its dependency on the history
// module as AgentHistoryServiceLike. TypeScript has a single module graph and
// hands createAgentHistoryService's result straight to AgentContextService,
// AgentIndexService and ObservabilityRpcServer; the port maps the records here
// instead. See the import-cycle notes in internal/observability
// (agent_context_service.go) and in internal/daemon (observability_server.go).

// agentHistoryAdapter forwards to *agenthistory.AgentHistoryService and maps the
// agenthistory records onto the observability ones. It satisfies all three
// interfaces the daemon hands the history service to:
// observability.AgentHistoryServiceLike (its SessionPathGate half included) and
// ObservabilityHistoryService.
type agentHistoryAdapter struct {
	service *agenthistory.AgentHistoryService
}

// Compile-time proof that one adapter covers every consumer.
var (
	_ observability.AgentHistoryServiceLike = (*agentHistoryAdapter)(nil)
	_ observability.SessionPathGate         = (*agentHistoryAdapter)(nil)
	_ ObservabilityHistoryService           = (*agentHistoryAdapter)(nil)
)

// newAgentHistoryAdapter wraps the production agent-history service.
func newAgentHistoryAdapter(service *agenthistory.AgentHistoryService) *agentHistoryAdapter {
	return &agentHistoryAdapter{service: service}
}

// ResolveCompactHistory mirrors resolveCompactHistory.
func (a *agentHistoryAdapter) ResolveCompactHistory(
	input observability.AgentHistoryLookupInput,
	options observability.AgentHistoryResolveOptions,
) observability.AgentHistoryResolved {
	resolved := a.service.ResolveCompactHistory(a.lookup(input), agenthistory.ResolveCompactOptions{
		ForceDiscovery: options.ForceDiscovery,
		ForceRefresh:   options.ForceRefresh != nil && *options.ForceRefresh,
		PreferredRef:   a.agenthistoryRef(options.PreferredRef),
	})
	return observability.AgentHistoryResolved{
		CompactHistory:    a.compact(resolved.CompactHistory),
		HistoryRef:        a.ref(resolved.HistoryRef),
		SourceFingerprint: a.fingerprint(resolved.SourceFingerprint),
	}
}

// Read mirrors read: the live read the `agent.read` RPC method serves.
func (a *agentHistoryAdapter) Read(
	input observability.AgentHistoryLookupInput,
	options ObservabilityHistoryReadOptions,
) ObservabilityHistoryReadResult {
	result := a.service.Read(a.lookup(input), agenthistory.ReadOptions{
		ForceDiscovery: options.ForceDiscovery,
		Limit:          options.Limit,
	})
	messages := make([]observability.AgentHistoryMessage, 0, len(result.Messages))
	for _, message := range result.Messages {
		messages = append(messages, a.message(message))
	}
	return ObservabilityHistoryReadResult{
		HistoryRef: a.ref(result.HistoryRef),
		Messages:   messages,
	}
}

// SafeAllowedSessionPath mirrors safeAllowedSessionPath.
func (a *agentHistoryAdapter) SafeAllowedSessionPath(value string) string {
	return agenthistory.SafeAllowedSessionPath(value, nil)
}

// SafeOfficialSessionPath mirrors safeOfficialSessionPath.
func (a *agentHistoryAdapter) SafeOfficialSessionPath(value string) string {
	return agenthistory.SafeOfficialSessionPath(value)
}

// SessionPathAllowedByShape mirrors sessionPathAllowedByShape.
func (a *agentHistoryAdapter) SessionPathAllowedByShape(value string) bool {
	return agenthistory.SessionPathAllowedByShape(value, nil)
}

// StatSourceFingerprint mirrors statSourceFingerprint.
func (a *agentHistoryAdapter) StatSourceFingerprint(
	path string,
) *observability.AgentHistorySourceFingerprint {
	return a.fingerprint(agenthistory.StatSourceFingerprint(path))
}

// HistorySourceFromSessionRef mirrors historySourceFromSessionRef.
func (a *agentHistoryAdapter) HistorySourceFromSessionRef(
	ref observability.AgentSessionRef,
) observability.AgentHistoryRefSource {
	return observability.AgentHistoryRefSource(agenthistory.HistorySourceFromSessionRef(ref))
}

// lookup maps the observability lookup input onto the agenthistory one. The
// observability record types herdrSessionName and paneId as plain strings (an
// indexed agent always carries both) while agenthistory keeps them optional, so
// an empty string maps to "absent". AgentSession is the same observability type
// on both sides and is forwarded as it stands.
func (a *agentHistoryAdapter) lookup(
	input observability.AgentHistoryLookupInput,
) agenthistory.AgentHistoryLookupInput {
	return agenthistory.AgentHistoryLookupInput{
		Agent:            input.Agent,
		AgentSession:     input.AgentSession,
		HerdrSessionName: optionalString(input.HerdrSessionName),
		PaneID:           optionalString(input.PaneID),
	}
}

func (a *agentHistoryAdapter) ref(ref *agenthistory.HistoryRef) *observability.AgentHistoryRef {
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

func (a *agentHistoryAdapter) agenthistoryRef(
	ref *observability.AgentHistoryRef,
) *agenthistory.HistoryRef {
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

func (a *agentHistoryAdapter) compact(
	history agenthistory.CompactAgentHistory,
) observability.CompactAgentHistory {
	return observability.CompactAgentHistory{
		HistoryRef:           a.ref(history.HistoryRef),
		LastAssistantMessage: a.excerpt(history.LastAssistantMessage),
		LastToolResult:       a.toolResult(history.LastToolResult),
		LastUserMessage:      a.excerpt(history.LastUserMessage),
		MessageCount:         history.MessageCount,
		Source:               history.Source,
		UnreadyReason:        a.unreadyReason(history.UnreadyReason),
		UpdatedAt:            history.UpdatedAt,
	}
}

func (a *agentHistoryAdapter) excerpt(
	excerpt *agenthistory.HistoryExcerpt,
) *observability.AgentHistoryExcerpt {
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

func (a *agentHistoryAdapter) toolResult(
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

func (a *agentHistoryAdapter) unreadyReason(
	reason *agenthistory.UnreadyReason,
) *observability.AgentHistoryUnreadyReason {
	if reason == nil {
		return nil
	}
	value := observability.AgentHistoryUnreadyReason(*reason)
	return &value
}

func (a *agentHistoryAdapter) fingerprint(
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

func (a *agentHistoryAdapter) message(
	message agenthistory.HistoryMessage,
) observability.AgentHistoryMessage {
	return observability.AgentHistoryMessage{
		Compact:    a.toolResult(message.Compact),
		Ref:        message.Ref,
		Role:       observability.AgentMessageRole(message.Role),
		StopReason: message.StopReason,
		Text:       message.Text,
		Timestamp:  message.Timestamp,
		ToolName:   message.ToolName,
	}
}

// optionalString maps "" to "absent" (nil), the TS `undefined`.
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
