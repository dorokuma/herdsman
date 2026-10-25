package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dorokuma/herdsman/go/internal/observability"
)

// UpsertRunningHerdrSessionInput mirrors the TS input of the same name.
type UpsertRunningHerdrSessionInput struct {
	Name       string
	SessionDir string
	SocketPath string
}

// HerdrSessionStore ports src/db/herdr-sessions.ts.
type HerdrSessionStore struct {
	db *sql.DB
}

// NewHerdrSessionStore builds a store on the shared connection.
func NewHerdrSessionStore(db *sql.DB) *HerdrSessionStore {
	return &HerdrSessionStore{db: db}
}

// herdrSessionColumns is the explicit `select *` column list of herdr_sessions.
const herdrSessionColumns = "last_scanned_at, name, running, session_dir, socket_path, updated_at"

// UpsertRunning mirrors upsertRunning: the session is inserted (or flipped back
// to running) with a fresh scan timestamp, and the stored record is returned.
func (s *HerdrSessionStore) UpsertRunning(
	input UpsertRunningHerdrSessionInput,
) (observability.HerdrSessionRecord, error) {
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(
		`insert into herdr_sessions (name, running, session_dir, socket_path, last_scanned_at, updated_at)
		 values (?, 1, ?, ?, ?, ?)
		 on conflict(name) do update set
		   running = 1,
		   session_dir = excluded.session_dir,
		   socket_path = excluded.socket_path,
		   last_scanned_at = excluded.last_scanned_at,
		   updated_at = excluded.updated_at`,
		input.Name, input.SessionDir, input.SocketPath, now, now,
	); err != nil {
		return observability.HerdrSessionRecord{}, err
	}
	return s.Get(input.Name)
}

// MarkStoppedMissingFrom mirrors markStoppedMissingFrom: every session whose
// name is not in runningNames is marked stopped. An empty list stops all of
// them, which is what a scan with no live sessions reports.
func (s *HerdrSessionStore) MarkStoppedMissingFrom(runningNames []string) error {
	now := time.Now().UnixMilli()
	if len(runningNames) == 0 {
		_, err := s.db.Exec("update herdr_sessions set running = 0, updated_at = ?", now)
		return err
	}
	args := make([]any, 0, len(runningNames)+1)
	args = append(args, now)
	for _, name := range runningNames {
		args = append(args, name)
	}
	_, err := s.db.Exec(
		"update herdr_sessions set running = 0, updated_at = ? where name not in ("+
			placeholders(len(runningNames))+")",
		args...,
	)
	return err
}

// FindRunningBySocketPath mirrors findRunningBySocketPath: the running session
// owning the socket, or nil when there is none.
func (s *HerdrSessionStore) FindRunningBySocketPath(
	socketPath string,
) (*observability.HerdrSessionRecord, error) {
	record, ok, err := scanHerdrSession(s.db.QueryRow(
		"select "+herdrSessionColumns+" from herdr_sessions where running = 1 and socket_path = ?",
		socketPath,
	))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &record, nil
}

// ListRunning mirrors listRunning.
func (s *HerdrSessionStore) ListRunning() ([]observability.HerdrSessionRecord, error) {
	return scanHerdrSessions(s.db.Query(
		"select " + herdrSessionColumns + " from herdr_sessions where running = 1 order by name",
	))
}

// List mirrors list.
func (s *HerdrSessionStore) List() ([]observability.HerdrSessionRecord, error) {
	return scanHerdrSessions(s.db.Query(
		"select " + herdrSessionColumns + " from herdr_sessions order by name",
	))
}

// Get mirrors get: it fails when no session with that name exists.
func (s *HerdrSessionStore) Get(name string) (observability.HerdrSessionRecord, error) {
	record, ok, err := scanHerdrSession(s.db.QueryRow(
		"select "+herdrSessionColumns+" from herdr_sessions where name = ?", name,
	))
	if err != nil {
		return observability.HerdrSessionRecord{}, err
	}
	if !ok {
		return observability.HerdrSessionRecord{}, fmt.Errorf("Herdr session not found: %s", name)
	}
	return record, nil
}

func scanHerdrSession(row *sql.Row) (observability.HerdrSessionRecord, bool, error) {
	var (
		lastScannedAt sql.NullInt64
		name          string
		running       int64
		sessionDir    string
		socketPath    string
		updatedAt     int64
	)
	if err := row.Scan(&lastScannedAt, &name, &running, &sessionDir, &socketPath, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return observability.HerdrSessionRecord{}, false, nil
		}
		return observability.HerdrSessionRecord{}, false, err
	}
	return observability.HerdrSessionRecord{
		LastScannedAt: nullableTimestamp(lastScannedAt),
		Name:          name,
		Running:       running != 0,
		SessionDir:    sessionDir,
		SocketPath:    socketPath,
		UpdatedAt:     formatTimestamp(updatedAt),
	}, true, nil
}

func scanHerdrSessions(rows *sql.Rows, err error) ([]observability.HerdrSessionRecord, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := []observability.HerdrSessionRecord{}
	for rows.Next() {
		var (
			lastScannedAt sql.NullInt64
			name          string
			running       int64
			sessionDir    string
			socketPath    string
			updatedAt     int64
		)
		if err := rows.Scan(&lastScannedAt, &name, &running, &sessionDir, &socketPath, &updatedAt); err != nil {
			return nil, err
		}
		records = append(records, observability.HerdrSessionRecord{
			LastScannedAt: nullableTimestamp(lastScannedAt),
			Name:          name,
			Running:       running != 0,
			SessionDir:    sessionDir,
			SocketPath:    socketPath,
			UpdatedAt:     formatTimestamp(updatedAt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}
