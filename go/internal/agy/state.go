// Package agy ports src/agy/hook.ts: the agy hook entrypoints that keep the
// Herdr orchestrator awake and deliver `agent.*` event batches to an agy pane.
//
// The three hook events are `PreInvocation`, `Stop` and `WakeWaiter`; the CLI
// (`herdsman agy-hook <event>`) feeds the raw stdin payload in and prints the
// returned JSON on stdout, so every field name, key order and status code in
// this package is a byte-for-byte contract with the TS implementation.
package agy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SessionHookState mirrors AgySessionHookState: the per-agy-session hook state
// persisted in `${HERDSMAN_AGY_STATE_DIR}/<sessionId>.json`.
//
// The TS state file is the object that was parsed from disk plus the keys the
// hook then assigned, so every field is a pointer (or a pointer-to-slice): an
// absent key stays absent, which is what `delete sessionState.turnEpoch` and
// the `?? fallback` reads depend on.
type SessionHookState struct {
	AutoClaimAttempted *bool    `json:"autoClaimAttempted,omitempty"`
	AwaitingNextTurn   *bool    `json:"awaitingNextTurn,omitempty"`
	DeliveredEventIDs  *[]int64 `json:"deliveredEventIds,omitempty"`
	PresentedEventIDs  *[]int64 `json:"presentedEventIds,omitempty"`
	// The epochs are JS numbers (max 2^53, so float64 keeps every value the TS
	// implementation can write, including a fractional one).
	TurnEpoch   *float64 `json:"turnEpoch,omitempty"`
	WaiterEpoch *float64 `json:"waiterEpoch,omitempty"`
	WaiterPID   *int     `json:"waiterPid,omitempty"`
}

// Delivered mirrors `sessionState.deliveredEventIds ?? []`.
func (s SessionHookState) Delivered() []int64 { return derefIDs(s.DeliveredEventIDs) }

// Presented mirrors `sessionState.presentedEventIds ?? []`.
func (s SessionHookState) Presented() []int64 { return derefIDs(s.PresentedEventIDs) }

// SetDelivered mirrors `sessionState.deliveredEventIds = value`: an empty slice
// is still written (`[]`), which is why the field is not plain `omitempty`.
func (s *SessionHookState) SetDelivered(ids []int64) {
	if ids == nil {
		ids = []int64{}
	}
	s.DeliveredEventIDs = &ids
}

// SetPresented mirrors `sessionState.presentedEventIds = value`.
func (s *SessionHookState) SetPresented(ids []int64) {
	if ids == nil {
		ids = []int64{}
	}
	s.PresentedEventIDs = &ids
}

// Awaiting mirrors `sessionState.awaitingNextTurn === true`.
func (s SessionHookState) Awaiting() bool { return s.AwaitingNextTurn != nil && *s.AwaitingNextTurn }

// TurnEpochValue mirrors `sessionState.turnEpoch !== undefined ? value : undefined`.
func (s SessionHookState) TurnEpochValue() (float64, bool) {
	if s.TurnEpoch == nil {
		return 0, false
	}
	return *s.TurnEpoch, true
}

// WaiterPIDValue mirrors `sessionState.waiterPid !== undefined ? value : undefined`.
func (s SessionHookState) WaiterPIDValue() (int, bool) {
	if s.WaiterPID == nil {
		return 0, false
	}
	return *s.WaiterPID, true
}

// WaiterEpochValue mirrors `sessionState.waiterEpoch !== undefined ? value : undefined`.
func (s SessionHookState) WaiterEpochValue() (float64, bool) {
	if s.WaiterEpoch == nil {
		return 0, false
	}
	return *s.WaiterEpoch, true
}

// SetAutoClaimAttempted mirrors `sessionState.autoClaimAttempted = true`.
func (s *SessionHookState) SetAutoClaimAttempted() { value := true; s.AutoClaimAttempted = &value }

// AutoClaimAttemptedValue mirrors `sessionState.autoClaimAttempted` (truthiness).
func (s SessionHookState) AutoClaimAttemptedValue() bool {
	return s.AutoClaimAttempted != nil && *s.AutoClaimAttempted
}

func derefIDs(ids *[]int64) []int64 {
	if ids == nil {
		return nil
	}
	return *ids
}

// stateIDSanitizer mirrors `/[^a-zA-Z0-9._-]/g`.
var stateIDSanitizer = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// stateFilePath mirrors stateFilePath: the session id is sanitized so it can
// name a file even when it carries pane-id punctuation.
func stateFilePath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, stateIDSanitizer.ReplaceAllString(sessionID, "_")+".json")
}

// readSessionState mirrors readSessionState: a missing, unparseable or
// non-object state file reads as an empty state (never an error).
func readSessionState(stateDir, sessionID string) SessionHookState {
	data, err := os.ReadFile(stateFilePath(stateDir, sessionID))
	if err != nil {
		return SessionHookState{}
	}
	state := SessionHookState{}
	if err := json.Unmarshal(data, &state); err != nil {
		return SessionHookState{}
	}
	return state
}

// knownStateJSONKeys is the set of JSON keys SessionHookState owns. The
// write path overlays exactly these onto the persisted object, so a field the
// struct leaves absent is deleted from disk (mirroring `delete sessionState.x`
// / `= undefined`) while every other key is carried through untouched.
var knownStateJSONKeys = []string{
	"autoClaimAttempted",
	"awaitingNextTurn",
	"deliveredEventIds",
	"presentedEventIds",
	"turnEpoch",
	"waiterEpoch",
	"waiterPid",
}

// writeSessionState mirrors writeSessionState: a mode-0600 temp file next to
// the target plus a rename, with every failure swallowed as best effort state
// persistence.
//
// The TS readSessionState returns the parsed object verbatim, so keys the hook
// never assigns (unknown extension fields) survive a read→write round trip.
// The Go SessionHookState only models the known keys, so the existing file is
// re-read here and the known keys are overlaid on top of it: an absent struct
// field deletes its key, everything else is preserved.
func writeSessionState(stateDir, sessionID string, state SessionHookState, pid int, nowMs float64) {
	targetPath := stateFilePath(stateDir, sessionID)
	tmpPath := fmt.Sprintf("%s.%d.%d.tmp", targetPath, pid, int64(nowMs))
	merged := readRawStateObject(targetPath)
	if merged == nil {
		merged = map[string]any{}
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return
	}
	if err := overlayKnownStateFields(merged, encoded); err != nil {
		return
	}
	finalEncoded, err := json.Marshal(merged)
	if err != nil {
		return
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return
	}
	if err := os.WriteFile(tmpPath, finalEncoded, 0o600); err != nil {
		os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath)
	}
}

// readRawStateObject reads the persisted state file as a generic object. A
// missing, unparseable or non-object file reads as an empty object, exactly
// like readSessionState.
//
// A file holding the JSON literal `null` unmarshals into a nil map without an
// error, so the nil result is normalized here: callers assign into the map
// (overlayKnownStateFields), and an assignment to a nil map panics.
func readRawStateObject(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	object := map[string]any{}
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return map[string]any{}
	}
	return object
}

// overlayKnownStateFields writes the known keys of the marshaled struct onto
// merged and deletes the known keys the struct leaves absent.
func overlayKnownStateFields(merged map[string]any, structJSON []byte) error {
	present := map[string]any{}
	if err := json.Unmarshal(structJSON, &present); err != nil {
		return err
	}
	for _, key := range knownStateJSONKeys {
		if value, ok := present[key]; ok {
			merged[key] = value
		} else {
			delete(merged, key)
		}
	}
	return nil
}

// defaultStateDir mirrors defaultStateDir:
// `${HERDSMAN_HOME:-$HOME/.herdsman}/agy-state`.
func defaultStateDir(environment map[string]string) string {
	home, ok := nonEmptyStringValue(environment["HERDSMAN_HOME"])
	if !ok {
		home = filepath.Join(userHomeDir(), ".herdsman")
	}
	return filepath.Join(home, "agy-state")
}

// userHomeDir mirrors node's homedir(): `$HOME` when set, otherwise the
// platform lookup.
func userHomeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// parseHookPayload mirrors parseHookPayload: whitespace-only input and
// unparseable or non-object JSON both read as an empty payload.
func parseHookPayload(rawStdin string) map[string]any {
	trimmed := strings.TrimSpace(rawStdin)
	if trimmed == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return map[string]any{}
	}
	return asRecord(parsed)
}

// asRecord mirrors asRecord: only a plain object is a record.
func asRecord(value any) map[string]any {
	if record, ok := value.(map[string]any); ok {
		return record
	}
	return map[string]any{}
}

// nonEmptyStringValue mirrors nonEmptyString: a non-empty string, or nothing.
func nonEmptyStringValue(value any) (string, bool) {
	if text, ok := value.(string); ok && text != "" {
		return text, true
	}
	return "", false
}

// nonEmptyStringPtr is nonEmptyString over an optional typed field.
func nonEmptyStringPtr(value *string) (string, bool) {
	if value == nil {
		return "", false
	}
	return nonEmptyStringValue(*value)
}

// payloadString reads `payload[key]` through nonEmptyString.
func payloadString(payload map[string]any, key string) (string, bool) {
	return nonEmptyStringValue(payload[key])
}

// truthyString mirrors a JS truthiness check on an optional string field.
func truthyString(value *string) bool {
	return value != nil && *value != ""
}
