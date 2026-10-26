package herdr

// This file ports src/herdr/session-snapshot.ts.

// HerdrSessionSnapshot mirrors the TS type of the same name. The two focused
// ids are optional in the source (kept only when the snapshot carries a string
// under the snake_case key), so they are modeled here as pointers that stay nil
// when absent. The four collections always exist in TS (defaulting to []), so
// they are non-nil slices here.
type HerdrSessionSnapshot struct {
	Agents             []any
	FocusedPaneId      *string
	FocusedWorkspaceId *string
	Panes              []any
	Tabs               []any
	Workspaces         []any
}

// NormalizeHerdrSessionSnapshot mirrors normalizeHerdrSessionSnapshot(value).
//
// The source treats value as a Record<string, unknown> only when it is a
// non-null object, then unwraps an optional outer `{ snapshot: ... }` envelope
// (using the envelope's value only when it too is a non-null object). From the
// resolved snapshot it reads the four collections (falling back to [] when they
// are not arrays) and the two optional snake_case focused ids (kept only when
// they are strings).
func NormalizeHerdrSessionSnapshot(value any) HerdrSessionSnapshot {
	record, ok := asJSONObject(value)
	if !ok {
		record = map[string]any{}
	}
	snapshot := record
	if nested, nestedOK := asJSONObject(record["snapshot"]); nestedOK {
		snapshot = nested
	}

	result := HerdrSessionSnapshot{
		Agents:     jsonArrayOrEmpty(snapshot["agents"]),
		Panes:      jsonArrayOrEmpty(snapshot["panes"]),
		Tabs:       jsonArrayOrEmpty(snapshot["tabs"]),
		Workspaces: jsonArrayOrEmpty(snapshot["workspaces"]),
	}
	if id, ok := snapshot["focused_pane_id"].(string); ok {
		result.FocusedPaneId = &id
	}
	if id, ok := snapshot["focused_workspace_id"].(string); ok {
		result.FocusedWorkspaceId = &id
	}
	return result
}

// asJSONObject reports whether value is a JSON object and returns it as a
// map[string]any, mirroring the TS `typeof value === "object" && value !== null`
// guard that yields a Record<string, unknown>. JSON scalars (including null,
// which decodes to an untyped nil interface), strings, and arrays are not
// objects here.
func asJSONObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

// jsonArrayOrEmpty returns value when it is a JSON array, else a non-nil empty
// slice, mirroring `Array.isArray(x) ? x : []`.
func jsonArrayOrEmpty(value any) []any {
	if array, ok := value.([]any); ok {
		return array
	}
	return []any{}
}
