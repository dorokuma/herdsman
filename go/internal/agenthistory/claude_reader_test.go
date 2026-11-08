package agenthistory

import (
	"path/filepath"
	"testing"
)

// Tests for the CC transcript reader: record filtering, message extraction,
// tool_result pairing and the streaming completeness guard.

func claudeRef(t *testing.T, lines []string) HistoryRef {
	t.Helper()
	content := ""
	for _, line := range lines {
		content += line + "\n"
	}
	path := writeSessionFile(t, filepath.Join(t.TempDir(), "session.jsonl"), content)
	return HistoryRef{
		Kind:   KindAgentSession,
		Path:   &path,
		Source: SourceClaudeJSONL,
		Value:  "3a4b5c6d-7e8f-4a1b-9c2d-3e4f5a6b7c8d",
	}
}

func TestClaudeReaderReadsUserAndAssistantTurns(t *testing.T) {
	reader := NewClaudeHistoryReader()
	ref := claudeRef(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"fix the bug"},"timestamp":"2026-11-08T01:00:00.000Z"}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done."},{"type":"text","text":"verified."}]},"timestamp":"2026-11-08T01:00:01.000Z"}`,
	})

	if !reader.CanRead(ref) {
		t.Fatal("CanRead(claude-jsonl) = false, want true")
	}
	messages, err := reader.Read(ref, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2: %+v", len(messages), messages)
	}
	if messages[0].Role != RoleUser || messages[0].Text != "fix the bug" {
		t.Fatalf("user message = %+v", messages[0])
	}
	if messages[1].Role != RoleAssistant || messages[1].Text != "done.\nverified." {
		t.Fatalf("assistant message = %+v", messages[1])
	}
	if messages[1].StopReason == nil || *messages[1].StopReason != "end_turn" {
		t.Fatalf("stopReason = %v, want end_turn", messages[1].StopReason)
	}
}

func TestClaudeReaderSkipsSidechainAndNonConversationalRecords(t *testing.T) {
	reader := NewClaudeHistoryReader()
	ref := claudeRef(t, []string{
		`{"type":"user","isSidechain":true,"uuid":"s1","message":{"role":"user","content":"subagent prompt"},"timestamp":"2026-11-08T01:00:00.000Z"}`,
		`{"type":"custom-title","customTitle":"session title"}`,
		`{"type":"file-history-snapshot","messageId":"m1","snapshot":{}}`,
		`{"type":"api-request","request":{"model":"claude"}}`,
		`{"type":"cost-state","cost":{}}`,
		`{"type":"summary","summary":"earlier"}`,
		`{"type":"assistant","isSidechain":true,"uuid":"s2","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"subagent answer"}]},"timestamp":"2026-11-08T01:00:01.000Z"}`,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"real question"},"timestamp":"2026-11-08T01:00:02.000Z"}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"real answer"}]},"timestamp":"2026-11-08T01:00:03.000Z"}`,
	})

	messages, err := reader.Read(ref, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want only the 2 main-chain records: %+v", len(messages), messages)
	}
	if messages[0].Text != "real question" || messages[1].Text != "real answer" {
		t.Fatalf("messages = %+v", messages)
	}
}

func TestClaudeReaderPairsToolResultsWithToolUseNames(t *testing.T) {
	reader := NewClaudeHistoryReader()
	ref := claudeRef(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"run tests"},"timestamp":"2026-11-08T01:00:00.000Z"}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"text","text":"running"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."}}]},"timestamp":"2026-11-08T01:00:01.000Z"}`,
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok all packages","is_error":false}]},"timestamp":"2026-11-08T01:00:02.000Z"}`,
		`{"type":"assistant","uuid":"a2","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"tests pass"}]},"timestamp":"2026-11-08T01:00:03.000Z"}`,
	})

	messages, err := reader.Read(ref, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(messages), messages)
	}
	tool := messages[2]
	if tool.Role != RoleToolResult {
		t.Fatalf("tool message role = %q, want tool_result: %+v", tool.Role, tool)
	}
	if tool.ToolName == nil || *tool.ToolName != "Bash" {
		t.Fatalf("toolName = %v, want Bash", tool.ToolName)
	}
	if tool.Compact == nil || tool.Compact.ToolName != "Bash" || tool.Compact.Text != "ok all packages" {
		t.Fatalf("compact = %+v", tool.Compact)
	}
}

func TestClaudeReaderToolResultWithoutToolUseFallsBackToUnknown(t *testing.T) {
	reader := NewClaudeHistoryReader()
	ref := claudeRef(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"text","text":"context"},{"type":"tool_result","tool_use_id":"toolu_gone","content":"result body","is_error":true}]},"timestamp":"2026-11-08T01:00:00.000Z"}`,
	})

	messages, err := reader.Read(ref, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2: %+v", len(messages), messages)
	}
	if messages[0].Role != RoleUser || messages[0].Text != "context" {
		t.Fatalf("user message = %+v", messages[0])
	}
	if messages[1].Role != RoleToolResult || messages[1].ToolName == nil || *messages[1].ToolName != "unknown" {
		t.Fatalf("tool message = %+v", messages[1])
	}
	if messages[1].Compact == nil || !messages[1].Compact.IsError {
		t.Fatalf("compact = %+v, want isError", messages[1].Compact)
	}
}

func TestClaudeReaderMergesBlocksOfOneMessageID(t *testing.T) {
	reader := NewClaudeHistoryReader()
	// Real v2.1.319 shape: one API response split into consecutive
	// type=assistant records sharing message.id (apiBlockIndex 0..3), where the
	// later blocks carry the text that belongs to the same reply and the final
	// block carries the terminal stop_reason.
	ref := claudeRef(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"explain"},"timestamp":"2026-11-08T01:00:00.000Z"}`,
		`{"type":"assistant","uuid":"a1","apiBlockIndex":0,"message":{"id":"msg_01","role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]},"timestamp":"2026-11-08T01:00:01.000Z"}`,
		`{"type":"assistant","uuid":"a2","apiBlockIndex":1,"message":{"id":"msg_01","role":"assistant","content":[{"type":"text","text":"part one"}]},"timestamp":"2026-11-08T01:00:02.000Z"}`,
		`{"type":"assistant","uuid":"a3","apiBlockIndex":2,"message":{"id":"msg_01","role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]},"timestamp":"2026-11-08T01:00:03.000Z"}`,
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"out","is_error":false}]},"timestamp":"2026-11-08T01:00:04.000Z"}`,
		`{"type":"assistant","uuid":"a4","apiBlockIndex":0,"message":{"id":"msg_02","role":"assistant","content":[{"type":"text","text":"first half"}]},"timestamp":"2026-11-08T01:00:05.000Z"}`,
		`{"type":"assistant","uuid":"a5","apiBlockIndex":1,"message":{"id":"msg_02","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"second half"}]},"timestamp":"2026-11-08T01:00:06.000Z"}`,
	})

	messages, err := reader.Read(ref, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(messages), messages)
	}
	first := messages[1]
	if first.Role != RoleAssistant || first.Text != "part one" {
		t.Fatalf("first assistant = %+v", first)
	}
	if first.StopReason == nil || *first.StopReason != "tool_use" {
		t.Fatalf("first stopReason = %v, want tool_use from the textless tool_use block", first.StopReason)
	}
	if first.Timestamp == nil || *first.Timestamp != "2026-11-08T01:00:03.000Z" {
		t.Fatalf("first timestamp = %v, want the latest block timestamp", first.Timestamp)
	}
	// The textless tool_use block still registered the tool name for the
	// tool_result that follows.
	if messages[2].Role != RoleToolResult || messages[2].ToolName == nil || *messages[2].ToolName != "Bash" {
		t.Fatalf("tool result = %+v, want tool_result named Bash", messages[2])
	}
	second := messages[3]
	if second.Role != RoleAssistant || second.Text != "first half\nsecond half" {
		t.Fatalf("second assistant = %+v, want text blocks joined with newline", second)
	}
	if second.StopReason == nil || *second.StopReason != "end_turn" {
		t.Fatalf("second stopReason = %v, want end_turn", second.StopReason)
	}
}

func TestClaudeReadCompactProducesTheFinishedReply(t *testing.T) {
	reader := NewClaudeHistoryReader()
	for _, stopReason := range []string{"end_turn", "max_tokens", "stop_sequence"} {
		ref := claudeRef(t, []string{
			`{"type":"user","uuid":"u1","message":{"role":"user","content":"q"},"timestamp":"2026-11-08T01:00:00.000Z"}`,
			`{"type":"assistant","uuid":"a1","message":{"role":"assistant","stop_reason":"` + stopReason + `","content":[{"type":"text","text":"answer"}]},"timestamp":"2026-11-08T01:00:01.000Z"}`,
		})
		compact, err := reader.ReadCompact(ref)
		if err != nil {
			t.Fatalf("ReadCompact(%s): %v", stopReason, err)
		}
		if compact.UnreadyReason != nil {
			t.Fatalf("ReadCompact(%s) unreadyReason = %v, want none", stopReason, *compact.UnreadyReason)
		}
		if compact.LastAssistantMessage == nil || compact.LastAssistantMessage.Text != "answer" {
			t.Fatalf("ReadCompact(%s) lastAssistant = %+v", stopReason, compact.LastAssistantMessage)
		}
		if compact.LastAssistantMessage.StopReason == nil ||
			*compact.LastAssistantMessage.StopReason != stopReason {
			t.Fatalf("ReadCompact(%s) stopReason = %v", stopReason, compact.LastAssistantMessage.StopReason)
		}
	}
}

func TestClaudeReadCompactRefusesAnUnfinishedOrMidToolAssistantTurn(t *testing.T) {
	reader := NewClaudeHistoryReader()
	user := `{"type":"user","uuid":"u1","message":{"role":"user","content":"q"},"timestamp":"2026-11-08T01:00:00.000Z"}`
	for _, tc := range []struct {
		name      string
		assistant string
	}{
		{
			name:      "no stop_reason (still streaming)",
			assistant: `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"partial ans"}]},"timestamp":"2026-11-08T01:00:01.000Z"}`,
		},
		{
			name:      "stop_reason tool_use (mid tool call)",
			assistant: `{"type":"assistant","uuid":"a1","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"text","text":"calling tool"}]},"timestamp":"2026-11-08T01:00:01.000Z"}`,
		},
		{
			name:      "stop_reason null (still streaming)",
			assistant: `{"type":"assistant","uuid":"a1","message":{"role":"assistant","stop_reason":null,"content":[{"type":"text","text":"partial ans"}]},"timestamp":"2026-11-08T01:00:01.000Z"}`,
		},
	} {
		ref := claudeRef(t, []string{user, tc.assistant})
		compact, err := reader.ReadCompact(ref)
		if err != nil {
			t.Fatalf("%s: ReadCompact: %v", tc.name, err)
		}
		if compact.LastAssistantMessage != nil {
			t.Fatalf("%s: lastAssistant = %+v, want none (unready)", tc.name, compact.LastAssistantMessage)
		}
		if compact.UnreadyReason == nil || *compact.UnreadyReason != UnreadyNoAssistantMessage {
			t.Fatalf("%s: unreadyReason = %v, want no_assistant_message", tc.name, compact.UnreadyReason)
		}
	}
}

func TestClaudeReadCompactRefusesStaleReplyAfterNewerUserMessage(t *testing.T) {
	reader := NewClaudeHistoryReader()
	// A follow-up user instruction sent while the previous turn had already
	// ended (the `｜pending` case): the new turn has only a tool_use record and
	// its tool_result so far. The previous end_turn reply must not be
	// delivered as the current reply.
	ref := claudeRef(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"first question"},"timestamp":"2026-11-08T01:00:00.000Z"}`,
		`{"type":"assistant","uuid":"a1","message":{"id":"msg_01","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"first answer"}]},"timestamp":"2026-11-08T01:00:01.000Z"}`,
		`{"type":"user","uuid":"u2","message":{"role":"user","content":"now do something else"},"timestamp":"2026-11-08T01:00:02.000Z"}`,
		`{"type":"assistant","uuid":"a2","message":{"id":"msg_02","role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"Bash"}]},"timestamp":"2026-11-08T01:00:03.000Z"}`,
		`{"type":"user","uuid":"u3","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},"timestamp":"2026-11-08T01:00:04.000Z"}`,
	})

	compact, err := reader.ReadCompact(ref)
	if err != nil {
		t.Fatalf("ReadCompact: %v", err)
	}
	if compact.LastAssistantMessage != nil {
		t.Fatalf("lastAssistant = %+v, want none (the end_turn reply is the previous turn's)", compact.LastAssistantMessage)
	}
	if compact.UnreadyReason == nil || *compact.UnreadyReason != UnreadyAssistantOnlyBeforeLastUser {
		t.Fatalf("unreadyReason = %v, want assistant_only_before_last_user", compact.UnreadyReason)
	}
}

func TestClaudeReadCompactRefusesTrailingTextlessAssistantRecord(t *testing.T) {
	reader := NewClaudeHistoryReader()
	user := `{"type":"user","uuid":"u1","message":{"role":"user","content":"q"},"timestamp":"2026-11-08T01:00:00.000Z"}`
	finished := `{"type":"assistant","uuid":"a1","message":{"id":"msg_01","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"old answer"}]},"timestamp":"2026-11-08T01:00:01.000Z"}`
	for _, tc := range []struct {
		name      string
		assistant string
	}{
		{
			name:      "tool_use-only record without stop_reason",
			assistant: `{"type":"assistant","uuid":"a2","message":{"id":"msg_02","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash"}]},"timestamp":"2026-11-08T01:00:02.000Z"}`,
		},
		{
			name:      "tool_use-only record with stop_reason tool_use",
			assistant: `{"type":"assistant","uuid":"a2","message":{"id":"msg_02","role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"Bash"}]},"timestamp":"2026-11-08T01:00:02.000Z"}`,
		},
	} {
		ref := claudeRef(t, []string{user, finished, tc.assistant})
		compact, err := reader.ReadCompact(ref)
		if err != nil {
			t.Fatalf("%s: ReadCompact: %v", tc.name, err)
		}
		if compact.LastAssistantMessage != nil {
			t.Fatalf("%s: lastAssistant = %+v, want none (a textless record is in flight)", tc.name, compact.LastAssistantMessage)
		}
		if compact.UnreadyReason == nil || *compact.UnreadyReason != UnreadyNoAssistantMessage {
			t.Fatalf("%s: unreadyReason = %v, want no_assistant_message", tc.name, compact.UnreadyReason)
		}
	}
}

func TestClaudeReadCompactMiddleTurnDoesNotShadowTheFinalReply(t *testing.T) {
	reader := NewClaudeHistoryReader()
	// A tool_use middle turn followed by the finished reply: only the last
	// main-chain assistant record decides readiness.
	ref := claudeRef(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"q"},"timestamp":"2026-11-08T01:00:00.000Z"}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"toolu_1","name":"Bash"}]},"timestamp":"2026-11-08T01:00:01.000Z"}`,
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},"timestamp":"2026-11-08T01:00:02.000Z"}`,
		`{"type":"assistant","uuid":"a2","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"final"}]},"timestamp":"2026-11-08T01:00:03.000Z"}`,
	})

	compact, err := reader.ReadCompact(ref)
	if err != nil {
		t.Fatalf("ReadCompact: %v", err)
	}
	if compact.LastAssistantMessage == nil || compact.LastAssistantMessage.Text != "final" {
		t.Fatalf("lastAssistant = %+v, want the finished reply", compact.LastAssistantMessage)
	}
	if compact.LastToolResult == nil || compact.LastToolResult.ToolName != "Bash" {
		t.Fatalf("lastToolResult = %+v, want Bash", compact.LastToolResult)
	}
}
