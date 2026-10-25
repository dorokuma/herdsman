package agenthistory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// Ported from test/unit/agent-history-readers.test.ts.

// writeJSONL mirrors the TS helper that writes JSON.stringify(entry) lines and
// keeps the byte layout identical to the original test fixtures.
func writeJSONL(t *testing.T, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	content := ""
	for _, line := range lines {
		content += line + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func refFor(path string) HistoryRef {
	return HistoryRef{Kind: KindAgentSession, Path: &path, Source: SourcePiJSONL, Value: path}
}

func intPtr(value int) *int { return &value }

func strPtr(value string) *string { return &value }

func TestPiHistoryReaderReadsUserAssistantAndToolResult(t *testing.T) {
	path := writeJSONL(t, "session.jsonl",
		`{"type":"session","id":"s1"}`,
		`{"type":"message","id":"u1","timestamp":"2026-07-09T12:00:00.000Z","message":{"role":"user","content":"please inspect"}}`,
		`{"type":"message","id":"t1","timestamp":"2026-07-09T12:00:01.000Z","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"line 1\nline 2"}]}}`,
		`{"type":"message","id":"a1","timestamp":"2026-07-09T12:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop"}}`,
	)

	messages, err := NewPiHistoryReader().Read(refFor(path), intPtr(20))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	wantRoles := []MessageRole{RoleUser, RoleToolResult, RoleAssistant}
	if len(messages) != len(wantRoles) {
		t.Fatalf("got %d messages, want %d: %+v", len(messages), len(wantRoles), messages)
	}
	for i, want := range wantRoles {
		if messages[i].Role != want {
			t.Errorf("messages[%d].role = %q, want %q", i, messages[i].Role, want)
		}
	}

	first := messages[0]
	if first.Text != "please inspect" {
		t.Errorf("messages[0].text = %q, want %q", first.Text, "please inspect")
	}
	if first.Timestamp == nil || *first.Timestamp != "2026-07-09T12:00:00.000Z" {
		t.Errorf("messages[0].timestamp = %v", derefOrNil(first.Timestamp))
	}
	if first.Ref != path+"#entry=u1" {
		t.Errorf("messages[0].ref = %q, want %q", first.Ref, path+"#entry=u1")
	}

	second := messages[1]
	if second.ToolName == nil || *second.ToolName != "bash" {
		t.Errorf("messages[1].toolName = %v, want bash", derefOrNil(second.ToolName))
	}
	if second.Compact == nil {
		t.Fatal("messages[1].compact is nil")
	}
	if !strings.Contains(second.Compact.Text, "line 1") {
		t.Errorf("messages[1].compact.text = %q, want it to contain %q", second.Compact.Text, "line 1")
	}
	if second.Compact.Compaction.Mode != ModeTruncatedPassthrough {
		t.Errorf("messages[1].compact.mode = %q, want %q", second.Compact.Compaction.Mode, ModeTruncatedPassthrough)
	}
	if second.Text != "line 1\nline 2" {
		t.Errorf("messages[1].text = %q", second.Text)
	}

	third := messages[2]
	if third.Text != "done" {
		t.Errorf("messages[2].text = %q, want done", third.Text)
	}
	if third.StopReason == nil || *third.StopReason != "stop" {
		t.Errorf("messages[2].stopReason = %v, want stop", derefOrNil(third.StopReason))
	}
}

func TestPiHistoryReaderRedactsSecretsInBodies(t *testing.T) {
	path := writeJSONL(t, "session.jsonl",
		`{"type":"message","id":"u1","message":{"role":"user","content":"password=hunter2 please use sk-abcdefghijklmnopqrstuvwxyz"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":"Authorization: Bearer super-secret-token"}}`,
	)

	messages, err := NewPiHistoryReader().Read(refFor(path), nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(messages))
	}
	want := []struct {
		role MessageRole
		text string
	}{
		{RoleUser, "password=[REDACTED] please use sk-[REDACTED]"},
		{RoleAssistant, "Authorization: Bearer [REDACTED]"},
	}
	for i, wc := range want {
		if messages[i].Role != wc.role {
			t.Errorf("messages[%d].role = %q, want %q", i, messages[i].Role, wc.role)
		}
		if messages[i].Text != wc.text {
			t.Errorf("messages[%d].text = %q, want %q", i, messages[i].Text, wc.text)
		}
	}
	joined := messages[0].Text + "\n" + messages[1].Text
	for _, forbidden := range []string{"hunter2", "super-secret-token"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("messages still contain %q: %q", forbidden, joined)
		}
	}
}

func TestPiHistoryReaderCanRead(t *testing.T) {
	path := "/tmp/session.jsonl"
	reader := NewPiHistoryReader()
	cases := []struct {
		name string
		ref  HistoryRef
		want bool
	}{
		{"pi path", HistoryRef{Source: SourcePiJSONL, Path: &path, Value: path}, true},
		{"pi value only", HistoryRef{Source: SourcePiJSONL, Value: path}, true},
		{"pi empty path and value", HistoryRef{Source: SourcePiJSONL}, false},
		{"antigravity source", HistoryRef{Source: SourceAntigravitySQLite, Value: path}, false},
		{"unknown source", HistoryRef{Source: SourceUnknown, Value: path}, false},
	}
	for _, tc := range cases {
		if got := reader.CanRead(tc.ref); got != tc.want {
			t.Errorf("%s: canRead = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReadJSONLStreamsLinesWithinCap(t *testing.T) {
	path := writeJSONL(t, "chat.jsonl",
		`{"type":"user","content":"ok"}`,
		`{"type":"assistant","content":"done"}`,
	)
	entries, err := ReadJSONL(path, ReadJSONLOptions{})
	if err != nil {
		t.Fatalf("readJsonl: %v", err)
	}
	want := []JSONLEntry{
		{Line: 1, Value: map[string]any{"type": "user", "content": "ok"}},
		{Line: 2, Value: map[string]any{"type": "assistant", "content": "done"}},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("entries = %+v, want %+v", entries, want)
	}
}

func TestReadJSONLReadsTailWindowOfOversizedFile(t *testing.T) {
	const dropped = `{"type":"user","content":"old"}`
	const keptUser = `{"type":"user","content":"recent"}`
	const keptAssistant = `{"type":"assistant","content":"kept"}`
	kept := keptUser + "\n" + keptAssistant + "\n"
	padding := strings.Repeat("x", 80) + "\n"

	path := filepath.Join(t.TempDir(), "huge.jsonl")
	if err := os.WriteFile(path, []byte(dropped+"\n"+padding+kept), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := ReadJSONL(path, ReadJSONLOptions{MaxBytes: len(kept) + 10})
	if err != nil {
		t.Fatalf("readJsonl: %v", err)
	}
	wantValues := []map[string]any{
		{"type": "user", "content": "recent"},
		{"type": "assistant", "content": "kept"},
	}
	if len(entries) != len(wantValues) {
		t.Fatalf("got %d entries, want %d: %+v", len(entries), len(wantValues), entries)
	}
	for i, want := range wantValues {
		if !reflect.DeepEqual(entries[i].Value, want) {
			t.Errorf("entries[%d].value = %+v, want %+v", i, entries[i].Value, want)
		}
	}
	// Line numbers are window-local: the partial lead line is skipped before
	// numbering, exactly as in TS, so the two kept records are 1 and 2. Tail
	// trimming therefore still has a referenceable line for the last entry.
	wantLines := []int{1, 2}
	for i, want := range wantLines {
		if entries[i].Line != want {
			t.Errorf("entries[%d].line = %d, want %d", i, entries[i].Line, want)
		}
	}
	limited := entries[len(entries)-1:]
	if !reflect.DeepEqual(limited[0].Value, wantValues[1]) {
		t.Errorf("limited entry = %+v, want %+v", limited[0], wantValues[1])
	}
}

func TestReadJSONLTreatsMalformedFinalRecordAsUnstableTail(t *testing.T) {
	path := writeJSONL(t, "tail.jsonl", `{"type":"user","content":"ok"}`, `{`)
	_, err := ReadJSONL(path, ReadJSONLOptions{})
	var unstable *UnstableJSONLError
	if !errors.As(err, &unstable) {
		t.Fatalf("error = %v, want *UnstableJSONLError", err)
	}
	if unstable.MalformedLines != 1 {
		t.Errorf("malformedLines = %d, want 1", unstable.MalformedLines)
	}
	if unstable.Size <= 0 {
		t.Errorf("size = %d, want the file size", unstable.Size)
	}
	if unstable.MtimeMs <= 0 {
		t.Errorf("mtimeMs = %v, want the file mtime", unstable.MtimeMs)
	}
}

func TestReadJSONLKeepsMalformedHistoricalRecordsObservable(t *testing.T) {
	path := writeJSONL(t, "middle.jsonl", `{"type":"user","content":"ok"}`, `{`, `{"type":"assistant","content":"done"}`)
	entries, err := ReadJSONL(path, ReadJSONLOptions{})
	if err != nil {
		t.Fatalf("readJsonl: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	// Line numbers count the malformed record: the window keeps source ordering.
	if entries[0].Line != 1 || entries[1].Line != 3 {
		t.Errorf("lines = %d,%d want 1,3", entries[0].Line, entries[1].Line)
	}
}

func TestReadJSONLThrowsTooLargeWhenSingleRecordExceedsWindow(t *testing.T) {
	path := writeJSONL(t, "huge-line.jsonl",
		`{"type":"user","content":"`+strings.Repeat("x", 200)+`"}`,
	)
	_, err := ReadJSONL(path, ReadJSONLOptions{MaxBytes: 40})
	var tooLarge *JSONLTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %v, want *JSONLTooLargeError", err)
	}
	if tooLarge.MaxBytes != 40 {
		t.Errorf("maxBytes = %d, want 40", tooLarge.MaxBytes)
	}
}

func TestReadJSONLRethrowsStreamErrorsAfterSuccessfulStat(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadJSONL(dir, ReadJSONLOptions{})
	if err == nil {
		t.Fatal("readJsonl(dir) = nil error, want EISDIR")
	}
	if !errors.Is(err, syscall.EISDIR) {
		t.Errorf("error = %v, want EISDIR", err)
	}
}

func TestReadJSONLMissingFileErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.jsonl")
	if _, err := ReadJSONL(missing, ReadJSONLOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want os.ErrNotExist", err)
	}
}

func TestLimitMessagesKeepsTailAndSanitizes(t *testing.T) {
	mark := "2026-07-09T12:00:00.000Z"
	messages := []HistoryMessage{
		{Ref: "r1", Role: RoleUser, Text: "first password=one"},
		{Ref: "r2", Role: RoleAssistant, Text: "second", Timestamp: &mark},
		{Ref: "r3", Role: RoleUser, Text: "third token=three"},
	}
	limited := LimitMessages(messages, intPtr(2))
	if len(limited) != 2 {
		t.Fatalf("got %d messages, want 2", len(limited))
	}
	if limited[0].Ref != "r2" || limited[1].Ref != "r3" {
		t.Errorf("refs = %q,%q want r2,r3", limited[0].Ref, limited[1].Ref)
	}
	if limited[1].Text != "third token=[REDACTED]" {
		t.Errorf("text = %q, want the secret redacted", limited[1].Text)
	}
	if limited[0].Timestamp == nil || *limited[0].Timestamp != mark {
		t.Errorf("timestamp = %v, want %s", derefOrNil(limited[0].Timestamp), mark)
	}
	if got := LimitMessages(messages, nil); len(got) != 3 {
		t.Errorf("limit nil kept %d messages, want 3", len(got))
	}
	if got := LimitMessages(messages, intPtr(0)); len(got) != 3 {
		t.Errorf("limit 0 kept %d messages, want 3 (limit <= 0 keeps everything)", len(got))
	}
}

func TestCompactFromMessagesDerivesUnreadyReason(t *testing.T) {
	ref := HistoryRef{Kind: KindAgentSession, Source: SourcePiJSONL, Value: "/tmp/s.jsonl"}
	user := HistoryMessage{Ref: "u", Role: RoleUser, Text: "hi"}
	assistant := HistoryMessage{Ref: "a", Role: RoleAssistant, Text: "yo"}
	tool := HistoryMessage{Ref: "t", Role: RoleToolResult, Text: "out", ToolName: strPtr("bash")}
	toolWithCompact := HistoryMessage{Ref: "t", Role: RoleToolResult, Text: "out", ToolName: strPtr("bash"),
		Compact: &CompactToolResult{
			ToolName:   "bash",
			Text:       "out",
			Compaction: ToolCompaction{Mode: ModeTruncatedPassthrough, OriginalChars: 3, ReturnedChars: 3},
		}}

	cases := []struct {
		name     string
		messages []HistoryMessage
		want     UnreadyReason
		count    int
	}{
		{"empty history", nil, UnreadyEmptyHistory, 0},
		{"no user message", []HistoryMessage{tool}, UnreadyNoUserMessage, 1},
		{"no assistant message", []HistoryMessage{user}, UnreadyNoAssistantMessage, 1},
	}
	// assistant_only_before_last_user is ported verbatim and stays unreachable,
	// exactly as in TS: any assistant message also sets lastAssistant, which
	// short-circuits the whole unreadyReason block.
	for _, tc := range cases {
		got := CompactFromMessages(ref, tc.messages)
		if got.UnreadyReason == nil {
			t.Errorf("%s: unreadyReason is absent, want %q", tc.name, tc.want)
			continue
		}
		if *got.UnreadyReason != tc.want {
			t.Errorf("%s: unreadyReason = %q, want %q", tc.name, *got.UnreadyReason, tc.want)
		}
		if got.MessageCount != tc.count {
			t.Errorf("%s: messageCount = %d, want %d", tc.name, got.MessageCount, tc.count)
		}
		if got.Source == nil || *got.Source != string(SourcePiJSONL) {
			t.Errorf("%s: source = %v, want pi-jsonl", tc.name, derefOrNil(got.Source))
		}
	}

	ready := CompactFromMessages(ref, []HistoryMessage{user, toolWithCompact, assistant})
	if ready.UnreadyReason != nil {
		t.Errorf("unreadyReason = %q, want absent for a ready history", *ready.UnreadyReason)
	}
	if ready.LastUserMessage == nil || ready.LastUserMessage.Text != "hi" {
		t.Errorf("lastUserMessage = %+v", ready.LastUserMessage)
	}
	if ready.LastAssistantMessage == nil || ready.LastAssistantMessage.Text != "yo" {
		t.Errorf("lastAssistantMessage = %+v", ready.LastAssistantMessage)
	}
	if ready.LastToolResult == nil || ready.LastToolResult.ToolName != "bash" {
		t.Errorf("lastToolResult = %+v", ready.LastToolResult)
	}
	if ready.MessageCount != 3 {
		t.Errorf("messageCount = %d, want 3", ready.MessageCount)
	}
	// A tool_result without a compact payload yields an absent lastToolResult,
	// mirroring `lastTool?.compact ?? null`.
	if got := CompactFromMessages(ref, []HistoryMessage{user, tool, assistant}); got.LastToolResult != nil {
		t.Errorf("lastToolResult = %+v, want absent", got.LastToolResult)
	}
	// Histories that do carry an assistant message are ready: no unreadyReason,
	// even when the assistant came before the last user message.
	for name, messages := range map[string][]HistoryMessage{
		"assistant only":        {assistant},
		"assistant before user": {assistant, user},
	} {
		if got := CompactFromMessages(ref, messages); got.UnreadyReason != nil {
			t.Errorf("%s: unreadyReason = %q, want absent", name, *got.UnreadyReason)
		}
	}
}

func TestCompactFromMessagesUsesLastTimestampAsUpdatedAt(t *testing.T) {
	ref := HistoryRef{Kind: KindAgentSession, Source: SourcePiJSONL, Value: "/tmp/s.jsonl"}
	first := "2026-07-09T12:00:00.000Z"
	last := "2026-07-09T12:00:09.000Z"
	got := CompactFromMessages(ref, []HistoryMessage{
		{Ref: "u", Role: RoleUser, Text: "hi", Timestamp: &first},
		{Ref: "a", Role: RoleAssistant, Text: "yo", Timestamp: &last},
	})
	if got.UpdatedAt == nil || *got.UpdatedAt != last {
		t.Errorf("updatedAt = %v, want %s", derefOrNil(got.UpdatedAt), last)
	}
}

func TestCompactMarshalsTSFieldNames(t *testing.T) {
	got := CompactFromMessages(HistoryRef{Kind: KindAgentSession, Source: SourcePiJSONL, Value: "/tmp/s.jsonl"}, nil)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"historyRef", "messageCount", "source", "unreadyReason", "updatedAt"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("marshaled compact history lacks %q: %s", key, raw)
		}
	}
	for _, key := range []string{"lastAssistantMessage", "lastToolResult", "lastUserMessage"} {
		if value, ok := decoded[key]; !ok || value != nil {
			t.Errorf("marshaled compact history has %q = %v, want null", key, value)
		}
	}
}
