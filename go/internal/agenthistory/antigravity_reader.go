package agenthistory

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver: the port stays CGO_ENABLED=0 clean
)

// AntigravityHistoryReader, ported from src/agent-history/antigravity-reader.ts.
//
// The TS reader wraps its whole body in try/catch and yields no messages on any
// failure (missing file, locked database, unreadable schema), so Read reports
// success with an empty slice instead of an error; the diagnostics that TS emits
// with console.debug are dropped, as they already are for readJsonl's
// console.warn.

// Antigravity table, role and payload candidates, in TS preference order.
var (
	antigravityTables  = []string{"steps", "conversation_steps", "step"}
	antigravityRoles   = []string{"role", "envelope_role", "message_role"}
	antigravityPayload = []string{"step_payload", "payload", "content", "message"}
)

// AntigravityHistoryReader reads explicit envelopes and agy's protobuf step
// envelopes out of a conversation SQLite database.
type AntigravityHistoryReader struct{}

// NewAntigravityHistoryReader builds a reader.
func NewAntigravityHistoryReader() *AntigravityHistoryReader { return &AntigravityHistoryReader{} }

var _ HistoryReader = (*AntigravityHistoryReader)(nil)

// CanRead mirrors canRead: the source must be antigravity-sqlite and both path
// and value must be non-empty.
func (r *AntigravityHistoryReader) CanRead(ref HistoryRef) bool {
	if ref.Source != SourceAntigravitySQLite {
		return false
	}
	return ref.ResolvedPath() != "" && ref.Value != "" && ref.Path != nil && *ref.Path != ""
}

// Read mirrors read(ref, options).
func (r *AntigravityHistoryReader) Read(ref HistoryRef, limit *int) ([]HistoryMessage, error) {
	if ref.Path == nil || *ref.Path == "" || ref.Value == "" || strings.Contains(*ref.Path, "..") {
		return []HistoryMessage{}, nil
	}
	messages, err := readAntigravityMessages(*ref.Path)
	if err != nil {
		// Mirrors the TS catch-all: an unreadable conversation is simply empty.
		return []HistoryMessage{}, nil
	}
	return LimitMessages(messages, limit), nil
}

// ReadCompact mirrors readCompact(ref): the turn-aware variant of
// compactFromMessages, which only reports a ready assistant turn when an
// assistant message follows the last user message.
func (r *AntigravityHistoryReader) ReadCompact(ref HistoryRef) (CompactAgentHistory, error) {
	messages, err := r.Read(ref, nil)
	if err != nil {
		return CompactAgentHistory{}, err
	}
	compact := CompactFromMessages(ref, messages)
	if len(messages) == 0 {
		compact.LastAssistantMessage = nil
		reason := UnreadyEmptyHistory
		compact.UnreadyReason = &reason
		return compact, nil
	}
	lastUserIndex := -1
	for index, message := range messages {
		if message.Role == RoleUser {
			lastUserIndex = index
		}
	}
	if lastUserIndex == -1 {
		compact.LastAssistantMessage = nil
		reason := UnreadyNoUserMessage
		compact.UnreadyReason = &reason
		return compact, nil
	}
	var assistantAfterUser *HistoryMessage
	for index := len(messages) - 1; index > lastUserIndex; index-- {
		if messages[index].Role == RoleAssistant {
			assistantAfterUser = &messages[index]
			break
		}
	}
	if assistantAfterUser != nil {
		compact.LastAssistantMessage = &HistoryExcerpt{
			Ref:       assistantAfterUser.Ref,
			Text:      assistantAfterUser.Text,
			Timestamp: assistantAfterUser.Timestamp,
		}
		compact.UnreadyReason = nil
		return compact, nil
	}
	hasAnyAssistant := false
	for _, message := range messages {
		if message.Role == RoleAssistant {
			hasAnyAssistant = true
			break
		}
	}
	reason := UnreadyNoAssistantMessage
	if hasAnyAssistant {
		reason = UnreadyAssistantOnlyBeforeLastUser
	}
	compact.LastAssistantMessage = nil
	compact.UnreadyReason = &reason
	return compact, nil
}

func readAntigravityMessages(path string) ([]HistoryMessage, error) {
	db, err := openReadOnlySQLite(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	table, err := findAntigravityTable(ctx, db)
	if err != nil {
		return nil, err
	}
	if table == "" {
		return nil, nil
	}
	names, err := tableColumnNames(ctx, db, table)
	if err != nil {
		return nil, err
	}
	roleColumn := firstPresent(names, antigravityRoles)
	payloadColumn := firstPresent(names, antigravityPayload)
	if payloadColumn == "" {
		return nil, nil
	}
	idColumn := "idx"
	if names["id"] {
		idColumn = "id"
	} else if names["step_id"] {
		idColumn = "step_id"
	}
	timeColumn := ""
	switch {
	case names["created_at"]:
		timeColumn = "created_at"
	case names["timestamp"]:
		timeColumn = "timestamp"
	}
	metadataColumn := ""
	if names["metadata"] {
		metadataColumn = "metadata"
	}

	query := fmt.Sprintf(
		"select %s as id, %s %s as payload%s%s from %s order by rowid",
		quoteIdentifier(idColumn),
		roleSelect(roleColumn),
		quoteIdentifier(payloadColumn),
		timeSelect(timeColumn),
		metadataSelect(metadataColumn),
		quoteIdentifier(table),
	)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	messages := []HistoryMessage{}
	for rows.Next() {
		columns, err := scanColumns(rows)
		if err != nil {
			return nil, err
		}
		role, text, ok := stepMessage(columns)
		if !ok {
			continue
		}
		timestamp := TimestampFrom(columns["time"])
		if timestamp == nil {
			timestamp = DecodeAntigravityMetadataTimestamp(columns["metadata"])
		}
		id := jsValueString(columns["id"])
		messages = append(messages, HistoryMessage{
			Ref:       MessageRef(path, &id, 0),
			Role:      role,
			Text:      text,
			Timestamp: timestamp,
		})
	}
	return messages, rows.Err()
}

// stepMessage mirrors the per-row mapping: an explicit user/assistant role wins
// and its payload must already be text, anything else goes through the protobuf
// step decoder.
func stepMessage(columns map[string]any) (MessageRole, string, bool) {
	if role, ok := columns["role"].(string); ok && (role == "user" || role == "assistant") {
		text := payloadText(columns["payload"])
		if text == "" {
			return "", "", false
		}
		return MessageRole(role), text, true
	}
	decoded, ok := DecodeAntigravityMessage(columns["payload"])
	if !ok || decoded.Text == "" {
		return "", "", false
	}
	return decoded.Role, decoded.Text, true
}

// payloadText mirrors payloadText: only a text payload counts, everything else
// (including a protobuf blob beside an explicit role column) is absent.
func payloadText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func openReadOnlySQLite(path string) (*sql.DB, error) {
	// mode=ro matches `new DatabaseSync(path, { readOnly: true })`: a missing or
	// unreadable file fails to open instead of being created.
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func findAntigravityTable(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "select name from sqlite_master where type = 'table'")
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		if containsString(antigravityTables, name) {
			return name, nil
		}
	}
	return "", rows.Err()
}

func tableColumnNames(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "pragma table_info("+quoteIdentifier(table)+")")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	names := map[string]bool{}
	for rows.Next() {
		columns, err := scanColumns(rows)
		if err != nil {
			return nil, err
		}
		if name, ok := columns["name"].(string); ok {
			names[name] = true
		}
	}
	return names, rows.Err()
}

func scanColumns(rows *sql.Rows) (map[string]any, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	raw := make([]any, len(columns))
	targets := make([]any, len(columns))
	for index := range raw {
		targets[index] = &raw[index]
	}
	if err := rows.Scan(targets...); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(columns))
	for index, name := range columns {
		out[name] = raw[index]
	}
	return out, nil
}

func roleSelect(role string) string {
	if role == "" {
		return ""
	}
	return quoteIdentifier(role) + " as role,"
}

func timeSelect(timeColumn string) string {
	if timeColumn == "" {
		return ""
	}
	return ", " + quoteIdentifier(timeColumn) + " as time"
}

func metadataSelect(metadataColumn string) string {
	if metadataColumn == "" {
		return ""
	}
	return ", " + quoteIdentifier(metadataColumn) + " as metadata"
}

func firstPresent(names map[string]bool, candidates []string) string {
	for _, candidate := range candidates {
		if names[candidate] {
			return candidate
		}
	}
	return ""
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// quoteIdentifier mirrors quote(value).
func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// jsValueString mirrors String(value) for the column values the driver returns.
func jsValueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case []byte:
		return string(typed)
	case bool:
		return strconv.FormatBool(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case int:
		return strconv.Itoa(typed)
	case float64:
		return jsNumberToString(typed)
	default:
		return fmt.Sprint(typed)
	}
}
