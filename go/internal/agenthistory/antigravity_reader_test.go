package agenthistory

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Ported from test/unit/antigravity-reader.test.ts. The fixtures are built with
// the same pure-Go driver the reader uses, so no cgo SQLite is involved.

// createConversationDB builds a conversation database and returns its path.
func createConversationDB(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conversation.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	return path
}

func stepsSchema() string {
	return `create table steps (
		id integer primary key autoincrement,
		role text not null,
		payload text not null,
		created_at integer not null
	)`
}

func agyRef(path string) HistoryRef {
	return HistoryRef{Kind: KindAgentSession, Path: &path, Source: SourceAntigravitySQLite, Value: path}
}

func assertUnready(t *testing.T, compact CompactAgentHistory, want UnreadyReason) {
	t.Helper()
	if want == "" {
		if compact.UnreadyReason != nil {
			t.Fatalf("unreadyReason = %q, want absent", *compact.UnreadyReason)
		}
		return
	}
	if compact.UnreadyReason == nil {
		t.Fatalf("unreadyReason is absent, want %q", want)
	}
	if *compact.UnreadyReason != want {
		t.Fatalf("unreadyReason = %q, want %q", *compact.UnreadyReason, want)
	}
}

func TestAntigravityReaderCompactWithoutUserMessage(t *testing.T) {
	path := createConversationDB(t, stepsSchema(),
		`insert into steps (role, payload, created_at) values ('assistant', 'hello without user', 1000)`)
	compact, err := NewAntigravityHistoryReader().ReadCompact(agyRef(path))
	if err != nil {
		t.Fatalf("readCompact: %v", err)
	}
	if compact.LastAssistantMessage != nil {
		t.Errorf("lastAssistantMessage = %+v, want absent", compact.LastAssistantMessage)
	}
	if compact.MessageCount != 1 {
		t.Errorf("messageCount = %d, want 1", compact.MessageCount)
	}
	assertUnready(t, compact, UnreadyNoUserMessage)
}

func TestAntigravityReaderCompactOnEmptyStepsTable(t *testing.T) {
	path := createConversationDB(t, stepsSchema())
	compact, err := NewAntigravityHistoryReader().ReadCompact(agyRef(path))
	if err != nil {
		t.Fatalf("readCompact: %v", err)
	}
	if compact.LastAssistantMessage != nil {
		t.Errorf("lastAssistantMessage = %+v, want absent", compact.LastAssistantMessage)
	}
	if compact.MessageCount != 0 {
		t.Errorf("messageCount = %d, want 0", compact.MessageCount)
	}
	assertUnready(t, compact, UnreadyEmptyHistory)
}

func TestAntigravityReaderCompactWithOnlyUserMessage(t *testing.T) {
	path := createConversationDB(t, stepsSchema(),
		`insert into steps (role, payload, created_at) values ('user', 'only user', 1000)`)
	compact, err := NewAntigravityHistoryReader().ReadCompact(agyRef(path))
	if err != nil {
		t.Fatalf("readCompact: %v", err)
	}
	if compact.LastAssistantMessage != nil {
		t.Errorf("lastAssistantMessage = %+v, want absent", compact.LastAssistantMessage)
	}
	if compact.MessageCount != 1 {
		t.Errorf("messageCount = %d, want 1", compact.MessageCount)
	}
	assertUnready(t, compact, UnreadyNoAssistantMessage)
}

func TestAntigravityReaderCompactAfterUserTurnAdvance(t *testing.T) {
	path := createConversationDB(t, stepsSchema(),
		`insert into steps (role, payload, created_at) values ('user', 'first question', 1000)`,
		`insert into steps (role, payload, created_at) values ('assistant', 'first answer', 1001)`,
		`insert into steps (role, payload, created_at) values ('user', 'second question', 2000)`,
	)
	compact, err := NewAntigravityHistoryReader().ReadCompact(agyRef(path))
	if err != nil {
		t.Fatalf("readCompact: %v", err)
	}
	if compact.LastAssistantMessage != nil {
		t.Errorf("lastAssistantMessage = %+v, want absent", compact.LastAssistantMessage)
	}
	if compact.LastUserMessage == nil || compact.LastUserMessage.Text != "second question" {
		t.Errorf("lastUserMessage = %+v, want second question", compact.LastUserMessage)
	}
	assertUnready(t, compact, UnreadyAssistantOnlyBeforeLastUser)
}

func TestAntigravityReaderCompactWhenTurnIsComplete(t *testing.T) {
	path := createConversationDB(t, stepsSchema(),
		`insert into steps (role, payload, created_at) values ('user', 'first question', 1000)`,
		`insert into steps (role, payload, created_at) values ('assistant', 'first answer', 1001)`,
		`insert into steps (role, payload, created_at) values ('user', 'second question', 2000)`,
		`insert into steps (role, payload, created_at) values ('assistant', 'second answer', 2001)`,
	)
	compact, err := NewAntigravityHistoryReader().ReadCompact(agyRef(path))
	if err != nil {
		t.Fatalf("readCompact: %v", err)
	}
	if compact.LastAssistantMessage == nil || compact.LastAssistantMessage.Text != "second answer" {
		t.Fatalf("lastAssistantMessage = %+v, want second answer", compact.LastAssistantMessage)
	}
	if compact.LastUserMessage == nil || compact.LastUserMessage.Text != "second question" {
		t.Errorf("lastUserMessage = %+v, want second question", compact.LastUserMessage)
	}
	assertUnready(t, compact, "")
}

func TestAntigravityReaderDecodesMetadataTimestamps(t *testing.T) {
	userSec := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC).Unix()
	asstSec := userSec + 5
	// The user turn and the assistant turn are two steps; the assistant metadata
	// holds two candidate timestamps so the larger one must win.
	firstMetadata := metadataTimestamp(1, userSec, 250_000_000)
	secondMetadata := concatBytes(metadataTimestamp(1, asstSec-1, 0), metadataTimestamp(7, asstSec, 500_000_000))
	path := createMetadataConversationDB(t, firstMetadata, secondMetadata)

	compact, err := NewAntigravityHistoryReader().ReadCompact(agyRef(path))
	if err != nil {
		t.Fatalf("readCompact: %v", err)
	}
	if compact.LastUserMessage == nil || compact.LastUserMessage.Text != "inspect worker status" {
		t.Fatalf("lastUserMessage = %+v", compact.LastUserMessage)
	}
	if compact.LastUserMessage.Timestamp == nil || *compact.LastUserMessage.Timestamp != "2026-10-22T12:00:00.250Z" {
		t.Errorf("lastUserMessage.timestamp = %v, want 2026-10-22T12:00:00.250Z",
			derefOrNil(compact.LastUserMessage.Timestamp))
	}
	if compact.LastAssistantMessage == nil || compact.LastAssistantMessage.Text != "worker completed all tasks" {
		t.Fatalf("lastAssistantMessage = %+v", compact.LastAssistantMessage)
	}
	if compact.LastAssistantMessage.Timestamp == nil || *compact.LastAssistantMessage.Timestamp != "2026-10-22T12:00:05.500Z" {
		t.Errorf("lastAssistantMessage.timestamp = %v, want 2026-10-22T12:00:05.500Z",
			derefOrNil(compact.LastAssistantMessage.Timestamp))
	}
	if compact.UpdatedAt == nil || *compact.UpdatedAt != "2026-10-22T12:00:05.500Z" {
		t.Errorf("updatedAt = %v, want 2026-10-22T12:00:05.500Z", derefOrNil(compact.UpdatedAt))
	}
}

// createMetadataConversationDB builds the idx/metadata steps table the TS fixture
// uses: no created_at or timestamp column, so timestamps come only from the
// embedded protobuf blobs.
func createMetadataConversationDB(t *testing.T, metadata ...[]byte) string {
	t.Helper()
	path := createConversationDB(t,
		`create table steps (
			idx integer primary key autoincrement,
			role text not null,
			payload text not null,
			metadata blob
		)`,
	)
	payloads := []string{"inspect worker status", "worker completed all tasks"}
	roles := []string{"user", "assistant"}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	for index, role := range roles {
		if _, err := db.ExecContext(ctx,
			`insert into steps (role, payload, metadata) values (?, ?, ?)`,
			role, payloads[index], metadata[index]); err != nil {
			t.Fatalf("insert step %d: %v", index, err)
		}
	}
	return path
}

func TestAntigravityReaderReadsProtobufStepPayloads(t *testing.T) {
	userSec := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC).Unix()
	path := createConversationDB(t,
		`create table conversation_steps (
			step_id integer primary key autoincrement,
			step_payload blob,
			metadata blob
		)`,
	)
	if err := insertStepPayloads(t, path,
		[]any{realUser, metadataTimestamp(1, userSec, 0)},
		[]any{realAssistant, metadataTimestamp(1, userSec+5, 0)},
		[]any{nestedField(20, nestedField(3, []byte("thinking only"))), nil},
		[]any{realTool, nil},
	); err != nil {
		t.Fatalf("insert step payloads: %v", err)
	}

	reader := NewAntigravityHistoryReader()
	messages, err := reader.Read(agyRef(path), nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	wantRoles := []MessageRole{RoleUser, RoleAssistant, RoleToolResult}
	if len(messages) != len(wantRoles) {
		t.Fatalf("got %d messages, want %d: %+v", len(messages), len(wantRoles), messages)
	}
	for index, want := range wantRoles {
		if messages[index].Role != want {
			t.Errorf("messages[%d].role = %q, want %q", index, messages[index].Role, want)
		}
	}
	if messages[0].Text != "plan list files in /tmp" {
		t.Errorf("messages[0].text = %q", messages[0].Text)
	}
	if messages[0].Ref != path+"#entry=1" {
		t.Errorf("messages[0].ref = %q, want %q", messages[0].Ref, path+"#entry=1")
	}
	if messages[0].Timestamp == nil || *messages[0].Timestamp != "2026-10-22T12:00:00.000Z" {
		t.Errorf("messages[0].timestamp = %v", derefOrNil(messages[0].Timestamp))
	}
	if messages[2].Text != "List Temporary Directory Files" {
		t.Errorf("messages[2].text = %q", messages[2].Text)
	}

	// The thinking-only step produces no message, so the tool result is still
	// the last entry and the turn is complete.
	compact, err := reader.ReadCompact(agyRef(path))
	if err != nil {
		t.Fatalf("readCompact: %v", err)
	}
	if compact.MessageCount != 3 {
		t.Errorf("messageCount = %d, want 3", compact.MessageCount)
	}
	assertUnready(t, compact, "")
	if compact.LastAssistantMessage == nil || compact.LastAssistantMessage.Text != "Listing /tmp files" {
		t.Errorf("lastAssistantMessage = %+v", compact.LastAssistantMessage)
	}
}

func insertStepPayloads(t *testing.T, path string, rows ...[]any) error {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	for _, row := range rows {
		if _, err := db.ExecContext(ctx,
			`insert into conversation_steps (step_payload, metadata) values (?, ?)`,
			row[0], row[1]); err != nil {
			return err
		}
	}
	return nil
}

func TestAntigravityReaderReadIsEmptyForUnusableInput(t *testing.T) {
	reader := NewAntigravityHistoryReader()
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.sqlite")

	cases := map[string]HistoryRef{
		"missing database": agyRef(missing),
		"parent traversal": agyRef(filepath.Join(dir, "..", "absent.sqlite")),
		"absent path":      {Kind: KindAgentSession, Source: SourceAntigravitySQLite, Value: missing},
		"empty value":      {Kind: KindAgentSession, Path: &missing, Source: SourceAntigravitySQLite},
		"pi source":        {Kind: KindAgentSession, Path: &missing, Source: SourcePiJSONL, Value: missing},
	}
	for name, ref := range cases {
		messages, err := reader.Read(ref, nil)
		if err != nil {
			t.Errorf("%s: read error = %v, want none", name, err)
		}
		if len(messages) != 0 {
			t.Errorf("%s: read = %+v, want empty", name, messages)
		}
	}
}

func TestAntigravityReaderReadIsEmptyWithoutKnownTable(t *testing.T) {
	path := createConversationDB(t, `create table other (id integer, content text)`)
	messages, err := NewAntigravityHistoryReader().Read(agyRef(path), nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("read = %+v, want empty", messages)
	}
}

func TestAntigravityReaderReadIsEmptyWithoutPayloadColumn(t *testing.T) {
	path := createConversationDB(t, `create table steps (id integer, role text, created_at integer)`)
	messages, err := NewAntigravityHistoryReader().Read(agyRef(path), nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("read = %+v, want empty", messages)
	}
}

func TestAntigravityReaderReadPrefersKnownColumns(t *testing.T) {
	path := createConversationDB(t,
		`create table steps (
			id integer primary key autoincrement,
			message_role text,
			content text,
			timestamp text
		)`,
		`insert into steps (message_role, content, timestamp) values ('user', 'from content column', '2026-07-09T12:00:00.000Z')`,
		`insert into steps (message_role, content, timestamp) values ('assistant', 'Bearer super-secret-token', '2026-07-09T12:00:01.000Z')`,
	)
	messages, err := NewAntigravityHistoryReader().Read(agyRef(path), nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(messages), messages)
	}
	if messages[0].Text != "from content column" {
		t.Errorf("messages[0].text = %q", messages[0].Text)
	}
	if messages[0].Timestamp == nil || *messages[0].Timestamp != "2026-07-09T12:00:00.000Z" {
		t.Errorf("messages[0].timestamp = %v, want the ISO passthrough", derefOrNil(messages[0].Timestamp))
	}
	// limitMessages sanitizes every returned message.
	if messages[1].Text != "Bearer [REDACTED]" {
		t.Errorf("messages[1].text = %q, want the credential redacted", messages[1].Text)
	}
	if strings.Contains(messages[1].Text, "super-secret-token") {
		t.Errorf("messages[1].text still leaks the secret: %q", messages[1].Text)
	}

	limited, err := NewAntigravityHistoryReader().Read(agyRef(path), intPtr(1))
	if err != nil {
		t.Fatalf("read with limit: %v", err)
	}
	if len(limited) != 1 || limited[0].Role != RoleAssistant {
		t.Errorf("limited = %+v, want only the assistant message", limited)
	}
}

func TestAntigravityReaderIgnoresRowsWithoutText(t *testing.T) {
	path := createConversationDB(t, stepsSchema(),
		`insert into steps (role, payload, created_at) values ('user', '', 1000)`,
		`insert into steps (role, payload, created_at) values ('system', 'not a turn role', 1001)`,
		`insert into steps (role, payload, created_at) values ('assistant', 'kept', 1002)`,
	)
	messages, err := NewAntigravityHistoryReader().Read(agyRef(path), nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1: %+v", len(messages), messages)
	}
	if messages[0].Text != "kept" || messages[0].Role != RoleAssistant {
		t.Errorf("messages[0] = %+v, want the assistant message", messages[0])
	}
}

func TestAntigravityReaderDoesNotCreateMissingDatabase(t *testing.T) {
	// A read-only open must not create a database next to a missing file.
	path := filepath.Join(t.TempDir(), "not-created.sqlite")
	if _, err := NewAntigravityHistoryReader().Read(agyRef(path), nil); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s = %v, want the file to remain absent", path, err)
	}
}

func TestAntigravityReaderCanRead(t *testing.T) {
	path := "/tmp/conversation.sqlite"
	reader := NewAntigravityHistoryReader()
	cases := []struct {
		name string
		ref  HistoryRef
		want bool
	}{
		{"path and value", agyRef(path), true},
		{"absent path", HistoryRef{Source: SourceAntigravitySQLite, Value: path}, false},
		{"absent value", HistoryRef{Source: SourceAntigravitySQLite, Path: &path}, false},
		{"empty value", HistoryRef{Source: SourceAntigravitySQLite, Path: &path, Value: ""}, false},
		{"pi source", HistoryRef{Source: SourcePiJSONL, Path: &path, Value: path}, false},
	}
	for _, tc := range cases {
		if got := reader.CanRead(tc.ref); got != tc.want {
			t.Errorf("%s: canRead = %v, want %v", tc.name, got, tc.want)
		}
	}
}
