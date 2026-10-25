package dedup

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/herdsman/go/internal/daemon"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRefusesWriteModeWhenInstanceLockIsHeld(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	release, err := daemon.AcquireDaemonLock(DaemonInstanceLockPath(pidPath), nil)
	if err != nil {
		t.Fatalf("acquiring the instance lock: %v", err)
	}
	defer release()

	live, err := LiveDaemonOwnerPid(pidPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if live.PID == nil || *live.PID != os.Getpid() || live.Source != "lock" {
		t.Fatalf("liveness = %+v, want this pid from the lock", live)
	}

	err = EnsureDaemonNotRunning(false, pidPath, nil, nil)
	if err == nil {
		t.Fatal("write mode must refuse while a daemon holds the instance flock")
	}
	for _, want := range []string{"Herdsman daemon is running", "refusing to clean agent event duplicates"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestRefusesWriteModeForEitherLockPathAndWarnsOnlyOnDryRun(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")

	for _, lockPath := range []string{pidPath + ".lock", DaemonInstanceLockPath(pidPath)} {
		release, err := daemon.AcquireDaemonLock(lockPath, nil)
		if err != nil {
			t.Fatalf("acquiring %s: %v", lockPath, err)
		}
		live, err := LiveDaemonOwnerPid(pidPath, nil)
		if err != nil {
			release()
			t.Fatal(err)
		}
		if live.PID == nil || *live.PID != os.Getpid() || live.Source != "lock" {
			release()
			t.Fatalf("liveness for %s = %+v", lockPath, live)
		}
		if err := EnsureDaemonNotRunning(false, pidPath, nil, nil); err == nil ||
			!strings.Contains(err.Error(), "refusing") {
			release()
			t.Fatalf("write mode for %s must refuse, got %v", lockPath, err)
		}
		release()
	}

	// Warns on dry-run when the lock is held.
	release, err := daemon.AcquireDaemonLock(DaemonInstanceLockPath(pidPath), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var warnings []string
	if err := EnsureDaemonNotRunning(true, pidPath, nil, func(message string) {
		warnings = append(warnings, message)
	}); err != nil {
		t.Fatalf("dry-run must not refuse: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "dry-run proceeds read-only") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestRefusesWriteModeWhenLockHeldButOwnerRecordMissing(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	lockPath := DaemonInstanceLockPath(pidPath)
	release, err := daemon.AcquireDaemonLock(lockPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// Intentionally remove owner.json while the kernel lock is still held.
	if err := os.Remove(lockPath + ".owner.json"); err != nil {
		t.Fatal(err)
	}
	live, err := LiveDaemonOwnerPid(pidPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if live.PID != nil || live.Source != "lock" {
		t.Fatalf("liveness = %+v, want no pid with source lock", live)
	}

	err = EnsureDaemonNotRunning(false, pidPath, nil, nil)
	if err == nil {
		t.Fatal("write mode must refuse when the flock is held without owner metadata")
	}
	if want := "Herdsman daemon is running (unknown pid, lock); refusing to clean agent event duplicates"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestIgnoresStalePidsAndUnheldLockFiles(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")

	// Even if the pid file and owner.json point at this live pid, without a held
	// flock everything here is stale.
	write(t, pidPath, itoa(os.Getpid())+"\n")
	write(t, pidPath+".lock", "")
	write(t, pidPath+".lock.owner.json", `{"pid":`+itoa(os.Getpid())+`}`)

	live, err := LiveDaemonOwnerPid(pidPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if live.PID != nil || live.Source != "" {
		t.Fatalf("liveness = %+v, want the empty verdict", live)
	}
	if err := EnsureDaemonNotRunning(false, pidPath, nil, nil); err != nil {
		t.Fatalf("no live daemon means no refusal, got %v", err)
	}

	// A corrupted owner.json is not treated as a live daemon.
	write(t, pidPath+".lock.owner.json", "not-json")
	if pid := ReadLockOwnerPid(pidPath + ".lock"); pid != nil {
		t.Fatalf("corrupt owner record returned pid %d", *pid)
	}
	if pid := ReadLockOwnerPid(filepath.Join(dir, "absent.lock")); pid != nil {
		t.Fatalf("missing owner record returned pid %d", *pid)
	}
}

func TestReadLockOwnerPidRejectsNonPositivePid(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")
	for _, body := range []string{`{"pid":0}`, `{"pid":-1}`, `{"pid":1.5}`, `{"pid":"7"}`, `{}`} {
		write(t, lockPath+".owner.json", body)
		if pid := ReadLockOwnerPid(lockPath); pid != nil {
			t.Fatalf("owner record %s yielded pid %d", body, *pid)
		}
	}
	write(t, lockPath+".owner.json", `{"pid":4242}`)
	pid := ReadLockOwnerPid(lockPath)
	if pid == nil || *pid != 4242 {
		t.Fatalf("pid = %v, want 4242", pid)
	}
}

func TestLiveDaemonOwnerPidPropagatesProbeError(t *testing.T) {
	probeErr := errors.New("flock probe exploded")
	if _, err := LiveDaemonOwnerPid("/tmp/x.pid", func(string) (bool, error) {
		return false, probeErr
	}); !errors.Is(err, probeErr) {
		t.Fatalf("error = %v, want the probe error", err)
	}
	if err := EnsureDaemonNotRunning(true, "/tmp/x.pid", func(string) (bool, error) {
		return false, probeErr
	}, nil); !errors.Is(err, probeErr) {
		t.Fatalf("guard error = %v, want the probe error", err)
	}
}

func TestLiveDaemonOwnerPidChecksInstanceLockFirst(t *testing.T) {
	var probed []string
	live, err := LiveDaemonOwnerPid("/home/x/herdsman.pid", func(lockPath string) (bool, error) {
		probed = append(probed, lockPath)
		return strings.HasSuffix(lockPath, ".instance.lock"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if live.Source != "lock" {
		t.Fatalf("liveness = %+v", live)
	}
	if len(probed) != 1 || !strings.HasSuffix(probed[0], ".instance.lock") {
		t.Fatalf("probed %v, want the instance lock to win before the plain lock", probed)
	}
}

func TestBacksUpDatabaseWithWalAndShmSidecars(t *testing.T) {
	dir := t.TempDir()
	databasePath := filepath.Join(dir, "state.db")
	write(t, databasePath, "main-db-content")
	write(t, databasePath+"-wal", "wal-content")
	write(t, databasePath+"-shm", "shm-content")

	backup, err := CopyDatabaseBackup(databasePath)
	if err != nil {
		t.Fatalf("CopyDatabaseBackup: %v", err)
	}
	if backup.BackupSizeBytes != int64(len("main-db-content")) {
		t.Fatalf("backup size = %d, want %d", backup.BackupSizeBytes, len("main-db-content"))
	}
	if !strings.HasPrefix(backup.BackupPath, databasePath+".bak-") {
		t.Fatalf("backup path = %q", backup.BackupPath)
	}
	for _, path := range []string{backup.BackupPath, backup.BackupPath + "-wal", backup.BackupPath + "-shm"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing backup member %s: %v", path, err)
		}
	}
	data, err := os.ReadFile(backup.BackupPath)
	if err != nil || string(data) != "main-db-content" {
		t.Fatalf("backup content = %q, err %v", data, err)
	}
}

func TestBacksUpDatabaseWithoutSidecars(t *testing.T) {
	dir := t.TempDir()
	databasePath := filepath.Join(dir, "state.db")
	write(t, databasePath, "main")

	backup, err := CopyDatabaseBackup(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(backup.BackupPath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("absent sidecar %s must not be backed up (stat err %v)", suffix, err)
		}
	}
}

func TestBackupTimestampMatchesIsoStrippedFormat(t *testing.T) {
	clock := func() time.Time {
		return time.Date(2026, 6, 29, 7, 8, 9, 123_000_000, time.UTC)
	}
	got, err := CopyDatabaseBackupWithClock(writeDb(t), clock)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got.BackupPath, ".bak-20260629070809123") {
		t.Fatalf("backup path = %q, want the stripped ISO timestamp suffix", got.BackupPath)
	}
}

func writeDb(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	write(t, path, "x")
	return path
}

func itoa(value int) string { return strconv.Itoa(value) }
