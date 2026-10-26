// Package dedup ports the pure helpers of
// scripts/clean-agent-event-duplicates-lib.ts: the daemon-liveness guard and the
// database backup step of the agent_events duplicate cleanup script. It is kept
// separate from CLI argument parsing so each step can be unit-tested.
//
// The SQL statements of the cleanup itself (temp table, cursor rewrite, delete)
// belong to the observability DB port and are not part of this package.
package dedup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dorokuma/herdsman/go/internal/daemon"
)

// FlockProber is the daemon-liveness seam: it reports whether the kernel flock on
// a lock path is currently held. Production wires daemon.IsFlockHeld; tests can
// substitute a fake.
type FlockProber func(lockPath string) (bool, error)

func defaultIsFlockHeld(lockPath string) (bool, error) { return daemon.IsFlockHeld(lockPath) }

// DaemonInstanceLockPath mirrors daemon.DaemonInstanceLockPath so callers of the
// script can name the instance lock without importing the daemon package twice.
func DaemonInstanceLockPath(pidPath string) string { return daemon.DaemonInstanceLockPath(pidPath) }

// DaemonLiveness is the daemon-detection verdict for one Herdsman home.
type DaemonLiveness struct {
	// PID is the live daemon's pid, or nil when no daemon is known to run.
	PID *int
	// Source names what proved the daemon is live ("lock"), or "" when nothing did.
	Source string
}

// ReadLockOwnerPid reads the owner PID of a daemon lock directory
// (${pidPath}.lock / ${pidPath}.instance.lock). It returns nil when the lock does
// not exist or has no valid owner.json — a corrupted or foreign owner record is
// simply "no pid".
func ReadLockOwnerPid(lockPath string) *int {
	data, err := os.ReadFile(lockPath + ".owner.json")
	if err != nil {
		return nil
	}
	var record struct {
		Pid *float64 `json:"pid"`
	}
	if err := json.Unmarshal(data, &record); err != nil || record.Pid == nil {
		return nil
	}
	pid := *record.Pid
	// typeof pid === "number" && Number.isInteger(pid) && pid > 0
	if pid != pid || pid != float64(int64(pid)) || pid <= 0 {
		return nil
	}
	value := int(pid)
	return &value
}

// LiveDaemonOwnerPid detects a live daemon for a HERDSMAN_HOME: it probes
// non-blocking kernel flock on the lock paths (${pidPath}.instance.lock first,
// then ${pidPath}.lock) and returns the live pid from owner metadata together
// with the source that proved it, or an empty verdict when no live daemon holds a
// flock.
func LiveDaemonOwnerPid(pidPath string, isFlockHeld FlockProber) (DaemonLiveness, error) {
	if isFlockHeld == nil {
		isFlockHeld = defaultIsFlockHeld
	}
	for _, lockPath := range []string{pidPath + ".instance.lock", pidPath + ".lock"} {
		held, err := isFlockHeld(lockPath)
		if err != nil {
			return DaemonLiveness{}, err
		}
		if held {
			return DaemonLiveness{PID: ReadLockOwnerPid(lockPath), Source: "lock"}, nil
		}
	}
	return DaemonLiveness{}, nil
}

// EnsureDaemonNotRunning refuses (write mode) or warns (dry-run) when a daemon is
// live for the given pidPath. Write mode returns an error so the CLI exits
// non-zero; dry-run proceeds read-only. warn receives the dry-run message and
// defaults to stderr.
func EnsureDaemonNotRunning(dryRun bool, pidPath string, isFlockHeld FlockProber, warn func(string)) error {
	live, err := LiveDaemonOwnerPid(pidPath, isFlockHeld)
	if err != nil {
		return err
	}
	if live.Source == "" && live.PID == nil {
		return nil
	}
	pidDesc := "unknown pid"
	if live.PID != nil {
		pidDesc = fmt.Sprintf("pid %d", *live.PID)
	}
	source := live.Source
	if source == "" {
		source = "unknown"
	}
	if dryRun {
		if warn == nil {
			warn = func(message string) { fmt.Fprintln(os.Stderr, message) }
		}
		warn(fmt.Sprintf(
			"Herdsman daemon is running (%s, %s); dry-run proceeds read-only", pidDesc, source))
		return nil
	}
	return fmt.Errorf(
		"Herdsman daemon is running (%s, %s); refusing to clean agent event duplicates", pidDesc, source)
}

// DuplicateBackup describes the verified backup of the database.
type DuplicateBackup struct {
	BackupPath      string
	BackupSizeBytes int64
}

// CopyDatabaseBackup copies the database plus its -wal/-shm sidecars (when
// present) to a timestamped backup and verifies the main file's size. It returns
// an error when the main file copy fails the size check.
func CopyDatabaseBackup(databasePath string) (DuplicateBackup, error) {
	return CopyDatabaseBackupWithClock(databasePath, time.Now)
}

// CopyDatabaseBackupWithClock is CopyDatabaseBackup with an injectable clock, so
// the timestamp suffix of the backup name is observable in tests.
func CopyDatabaseBackupWithClock(databasePath string, now func() time.Time) (DuplicateBackup, error) {
	if now == nil {
		now = time.Now
	}
	backupPath := databasePath + ".bak-" + backupTimestamp(now())
	if err := copyFile(databasePath, backupPath); err != nil {
		return DuplicateBackup{}, err
	}
	backupInfo, err := os.Stat(backupPath)
	if err != nil {
		return DuplicateBackup{}, err
	}
	sourceInfo, err := os.Stat(databasePath)
	if err != nil {
		return DuplicateBackup{}, err
	}
	if backupInfo.Size() != sourceInfo.Size() {
		return DuplicateBackup{}, errors.New("Backup size verification failed")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		source := databasePath + suffix
		if _, statErr := os.Stat(source); statErr != nil {
			continue
		}
		if err := copyFile(source, backupPath+suffix); err != nil {
			return DuplicateBackup{}, err
		}
	}
	return DuplicateBackup{BackupPath: backupPath, BackupSizeBytes: backupInfo.Size()}, nil
}

// backupTimestamp formats the clock like the TS
// `new Date().toISOString().replace(/[-:.TZ]/g, "")`, e.g. 20260629000000000.
func backupTimestamp(t time.Time) string {
	iso := t.UTC().Format("2006-01-02T15:04:05.000Z")
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', ':', '.', 'T', 'Z':
			return -1
		}
		return r
	}, iso)
}

// copyFile mirrors node's copyFileSync: full file contents, mode preserved.
func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
