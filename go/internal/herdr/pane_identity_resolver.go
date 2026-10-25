package herdr

import "errors"

// This file ports src/herdr/pane-identity-resolver.ts.

// HerdrPaneIdentity mirrors the TS type of the same name. The four optional
// fields are present only when Herdr reports a non-empty value, so they are
// pointers that are omitted from JSON when nil.
type HerdrPaneIdentity struct {
	PaneID        string  `json:"paneId"`
	TerminalID    string  `json:"terminalId"`
	WorkspaceID   string  `json:"workspaceId"`
	Agent         *string `json:"agent,omitempty"`
	CWD           *string `json:"cwd,omitempty"`
	ForegroundCwd *string `json:"foregroundCwd,omitempty"`
	TabID         *string `json:"tabId,omitempty"`
}

// PaneClient mirrors `Pick<HerdrSocketClient, "close" | "getPane">`: the subset
// of the socket client the resolver depends on, so tests can inject a fake.
type PaneClient interface {
	GetPane(params map[string]any) (any, error)
	Close()
}

// ResolveHerdrPaneIdentityInput mirrors the resolver's input object.
type ResolveHerdrPaneIdentityInput struct {
	ClientFactory func(socketPath string) PaneClient
	PaneID        string
	SocketPath    string
}

// ResolveHerdrPaneIdentity mirrors resolveHerdrPaneIdentity(input).
//
// It reads pane identity straight from Herdr's own `pane.get` answer, consuming
// the official snake_case `pane_info` envelope fields as-is: no camelCase
// aliases, no unwrapped response shape, no local mapping of identity fields. A
// missing terminal identity (pane_id / terminal_id / workspace_id) or a
// non-official response shape is an error, and the client is always closed.
func ResolveHerdrPaneIdentity(input ResolveHerdrPaneIdentityInput) (HerdrPaneIdentity, error) {
	var client PaneClient
	if input.ClientFactory != nil {
		client = input.ClientFactory(input.SocketPath)
	} else {
		// The TS default is `new HerdrSocketClient({ socketPath })`; the Go
		// constructor reports a dial failure, which is surfaced here.
		defaultClient, err := NewHerdrSocketClient(HerdrSocketClientOptions{SocketPath: input.SocketPath})
		if err != nil {
			return HerdrPaneIdentity{}, err
		}
		client = defaultClient
	}
	defer client.Close()

	result, err := client.GetPane(map[string]any{"pane_id": input.PaneID})
	if err != nil {
		return HerdrPaneIdentity{}, err
	}
	pane, err := paneInfo(result)
	if err != nil {
		return HerdrPaneIdentity{}, err
	}
	paneID := stringField(pane, "pane_id")
	terminalID := stringField(pane, "terminal_id")
	workspaceID := stringField(pane, "workspace_id")
	if paneID == nil || terminalID == nil || workspaceID == nil {
		return HerdrPaneIdentity{}, errors.New("Herdr pane response has no terminal identity")
	}
	identity := HerdrPaneIdentity{
		PaneID:      *paneID,
		TerminalID:  *terminalID,
		WorkspaceID: *workspaceID,
	}
	if agent := stringField(pane, "agent"); agent != nil {
		identity.Agent = agent
	}
	if cwd := stringField(pane, "cwd"); cwd != nil {
		identity.CWD = cwd
	}
	if foregroundCwd := stringField(pane, "foreground_cwd"); foregroundCwd != nil {
		identity.ForegroundCwd = foregroundCwd
	}
	if tabID := stringField(pane, "tab_id"); tabID != nil {
		identity.TabID = tabID
	}
	return identity, nil
}

// paneInfo mirrors paneInfo(value): `pane.get` only answers with the official
// `pane_info` envelope; anything else is a contract violation.
func paneInfo(value any) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("Invalid Herdr pane response")
	}
	pane, ok := object["pane"].(map[string]any)
	if !ok {
		return nil, errors.New("Invalid Herdr pane response")
	}
	return pane, nil
}

// stringField mirrors stringField(value, field): the field when it is a
// non-empty string, else nil.
func stringField(value map[string]any, field string) *string {
	if s, ok := value[field].(string); ok && len(s) > 0 {
		return &s
	}
	return nil
}
