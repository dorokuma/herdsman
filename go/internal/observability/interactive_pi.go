package observability

import (
	"path/filepath"
	"strings"
)

// DispatchedPiSessionRoot mirrors DISPATCHED_PI_SESSION_ROOT.
const DispatchedPiSessionRoot = "/tmp/herdr-role-sessions/"

// IsInteractivePiAgent mirrors isInteractivePiAgent: true only for pi-family
// (pi/pig) agents used as interactive observers, not dispatched roles.
//
// The TS implementation rejects any raw value containing "..", normalises the
// path, prefers realpathSync when it resolves, and finally tests the
// DISPATCHED_PI_SESSION_ROOT prefix. filepath.Clean is the Go equivalent of
// path.normalize for absolute POSIX paths; EvalSymlinks stands in for
// realpathSync and its failure is tolerated exactly like the TS catch block.
func IsInteractivePiAgent(agent *AgentIndexRecord) bool {
	if agent == nil || agent.Agent == nil ||
		(*agent.Agent != "pi" && *agent.Agent != "pig") {
		return false
	}
	if agent.AgentSession == nil || agent.AgentSession.Value == "" {
		return false
	}
	raw := agent.AgentSession.Value
	if strings.Contains(raw, "..") {
		return false
	}
	resolved := filepath.Clean(raw)
	if real, err := filepath.EvalSymlinks(raw); err == nil {
		resolved = real
	}
	return !strings.HasPrefix(resolved, DispatchedPiSessionRoot)
}
