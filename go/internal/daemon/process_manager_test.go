package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitForCondition polls condition until it holds, so a test never depends on a
// fixed sleep being long enough when the machine is loaded. Failing the timeout
// fails the test instead of being masked by the wait.
func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	const timeout = 5 * time.Second
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func intPtr(v int) *int { return &v }

func TestReportsStoppedWhenThePidFileDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "missing.pid")
	socketPath := "/tmp/herdsman.sock"

	status, err := GetDaemonStatus(pidPath, socketPath, nil)
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	want := DaemonStatus{State: DaemonStateStopped, PidPath: pidPath, SocketPath: socketPath}
	if status != want {
		t.Fatalf("status = %+v, want %+v", status, want)
	}
}

func TestReportsRunningWhenPidFileMissingButSocketReachable(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "missing.pid")
	socketPath := "/tmp/herdsman.sock"

	status, err := GetDaemonStatus(pidPath, socketPath, &DaemonProcessDependencies{
		ConnectSocket: func(string) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	want := DaemonStatus{
		State:           DaemonStateRunning,
		PidPath:         pidPath,
		PidFileMissing:  true,
		SocketPath:      socketPath,
		SocketReachable: true,
	}
	if status != want {
		t.Fatalf("status = %+v, want %+v", status, want)
	}
}

func TestTreatsEPERMProcessProbeAsLive(t *testing.T) {
	probe := func(int, syscall.Signal) error { return syscall.EPERM }
	if !IsProcessRunningWith(1234, probe) {
		t.Fatal("EPERM probe must count as a live process")
	}
}

func TestTreatsESRCHProcessProbeAsStopped(t *testing.T) {
	probe := func(int, syscall.Signal) error { return syscall.ESRCH }
	if IsProcessRunningWith(1234, probe) {
		t.Fatal("ESRCH probe must count as a stopped process")
	}
}

func TestReportsRunningWhenPidFileContainsLiveProcess(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	if err := os.WriteFile(pidPath, []byte("1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	socketPath := "/tmp/herdsman.sock"

	status, err := GetDaemonStatus(pidPath, socketPath, &DaemonProcessDependencies{
		ConnectSocket:    func(string) (bool, error) { return true, nil },
		IdentityProbe:    func(int, []string) (bool, bool) { return true, true },
		IsProcessRunning: func(pid int) bool { return pid == 1234 },
		ReadCgroup:       func(int) (string, bool) { return "", false },
		ReadServiceRestartCount: func() (int, bool) {
			return 0, false
		},
	})
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	if status.State != DaemonStateRunning || status.Pid == nil || *status.Pid != 1234 {
		t.Fatalf("status = %+v", status)
	}
	if status.ManagedBy != DaemonManagedByUnknown {
		t.Fatalf("managedBy = %q, want unknown", status.ManagedBy)
	}
	if status.RestartCount != nil {
		t.Fatalf("restartCount = %v, want omitted", *status.RestartCount)
	}
	if !status.SocketReachable || status.PidPath != pidPath || status.SocketPath != socketPath {
		t.Fatalf("status = %+v", status)
	}
	if status.PidFileMissing || status.StalePid != nil {
		t.Fatalf("status = %+v", status)
	}
}

func TestReportsSupervisingUnitAndRestartCount(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	if err := os.WriteFile(pidPath, []byte("1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := GetDaemonStatus(pidPath, "/tmp/herdsman.sock", &DaemonProcessDependencies{
		ConnectSocket:    func(string) (bool, error) { return true, nil },
		IdentityProbe:    func(int, []string) (bool, bool) { return true, true },
		IsProcessRunning: func(pid int) bool { return pid == 1234 },
		ReadCgroup:       func(int) (string, bool) { return "0::/system.slice/herdsman.service\n", true },
		ReadServiceRestartCount: func() (int, bool) {
			return 7, true
		},
	})
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	if status.ManagedBy != SystemdManagedBy(HerdsmanSystemdUnit) {
		t.Fatalf("managedBy = %q, want systemd:herdsman.service", status.ManagedBy)
	}
	if status.RestartCount == nil || *status.RestartCount != 7 {
		t.Fatalf("restartCount = %v, want 7", status.RestartCount)
	}
	if status.State != DaemonStateRunning || status.Pid == nil || *status.Pid != 1234 || !status.SocketReachable {
		t.Fatalf("status = %+v", status)
	}
}

func TestOmitsRestartCountWhenNotSupervisedByHerdsmanUnit(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	if err := os.WriteFile(pidPath, []byte("1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restartProbes := 0

	status, err := GetDaemonStatus(pidPath, "/tmp/herdsman.sock", &DaemonProcessDependencies{
		ConnectSocket:    func(string) (bool, error) { return true, nil },
		IdentityProbe:    func(int, []string) (bool, bool) { return true, true },
		IsProcessRunning: func(pid int) bool { return pid == 1234 },
		ReadCgroup:       func(int) (string, bool) { return "0::/user.slice/user-0.slice/session-1.scope\n", true },
		ReadServiceRestartCount: func() (int, bool) {
			restartProbes++
			return 0, true
		},
	})
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	// `NRestarts` belongs to `herdsman.service`, which does not own this pid:
	// printing it next to managedBy: unmanaged would read like "this stray daemon
	// restarted 0 times". The probe is not even run.
	if status.ManagedBy != DaemonManagedByUnmanaged || status.State != DaemonStateRunning {
		t.Fatalf("status = %+v", status)
	}
	if status.RestartCount != nil {
		t.Fatalf("restartCount = %d, want omitted", *status.RestartCount)
	}
	if restartProbes != 0 {
		t.Fatalf("restart probes = %d, want 0", restartProbes)
	}
}

func TestDegradesSupervisionFactsWithoutChangingStatus(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	if err := os.WriteFile(pidPath, []byte("1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restartProbes := 0

	status, err := GetDaemonStatus(pidPath, "/tmp/herdsman.sock", &DaemonProcessDependencies{
		ConnectSocket:    func(string) (bool, error) { return true, nil },
		IdentityProbe:    func(int, []string) (bool, bool) { return true, true },
		IsProcessRunning: func(pid int) bool { return pid == 1234 },
		ReadCgroup:       func(int) (string, bool) { return "", false },
		ReadServiceRestartCount: func() (int, bool) {
			restartProbes++
			return 0, false
		},
	})
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	if status.ManagedBy != DaemonManagedByUnknown || status.RestartCount != nil {
		t.Fatalf("status = %+v", status)
	}
	if status.State != DaemonStateRunning || status.Pid == nil || *status.Pid != 1234 || !status.SocketReachable {
		t.Fatalf("status = %+v", status)
	}
	// unknown is not a systemd unit either, so no `systemctl` probe is spent.
	if restartProbes != 0 {
		t.Fatalf("restart probes = %d, want 0", restartProbes)
	}
}

func TestThrowingSupervisionProbeNeverFailsDaemonStatus(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	if err := os.WriteFile(pidPath, []byte("1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := GetDaemonStatus(pidPath, "/tmp/herdsman.sock", &DaemonProcessDependencies{
		ConnectSocket:    func(string) (bool, error) { return true, nil },
		IdentityProbe:    func(int, []string) (bool, bool) { return true, true },
		IsProcessRunning: func(pid int) bool { return pid == 1234 },
		ReadCgroup: func(int) (string, bool) {
			panic("cgroup read exploded")
		},
		ReadServiceRestartCount: func() (int, bool) {
			panic("systemctl exploded")
		},
	})
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	if status.ManagedBy != DaemonManagedByUnknown || status.RestartCount != nil {
		t.Fatalf("status = %+v", status)
	}
	if status.State != DaemonStateRunning || status.Pid == nil || *status.Pid != 1234 {
		t.Fatalf("status = %+v", status)
	}
}

func TestManagedByFromCgroupRequiresSystemSlice(t *testing.T) {
	cases := []struct {
		name   string
		cgroup string
		want   DaemonManagedBy
	}{
		// cgroup v2 system-manager layout.
		{"v2 system slice", "0::/system.slice/herdsman.service\n", SystemdManagedBy(HerdsmanSystemdUnit)},
		// cgroup v1 layout.
		{"v1 systemd controller", "1:name=systemd:/system.slice/herdsman.service\n0::/\n", SystemdManagedBy(HerdsmanSystemdUnit)},
		// A nested cgroup inside the unit still belongs to that unit.
		{"nested cgroup", "0::/system.slice/herdsman.service/child.scope\n", SystemdManagedBy(HerdsmanSystemdUnit)},
		// Same unit name in a *user* session unit is a different object.
		{"user session unit", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/herdsman.service\n", DaemonManagedByUnmanaged},
		// A unit that merely starts with the same prefix must not be mistaken for it.
		{"lookalike unit", "0::/system.slice/herdsman.service.dev\n", DaemonManagedByUnmanaged},
		// Readable content that names no herdsman unit: genuinely unmanaged.
		{"unrelated scope", "0::/user.slice/user-0.slice/session-1.scope\n", DaemonManagedByUnmanaged},
		{"root cgroup", "0::/\n", DaemonManagedByUnmanaged},
		// Nothing was read: no fact available, which is unknown, not unmanaged.
		{"empty", "", DaemonManagedByUnknown},
		{"newline only", "\n", DaemonManagedByUnknown},
		{"blank only", "   \n\t\n", DaemonManagedByUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ManagedByFromCgroup(tc.cgroup, ""); got != tc.want {
				t.Fatalf("ManagedByFromCgroup(%q) = %q, want %q", tc.cgroup, got, tc.want)
			}
		})
	}
}

func TestReadSystemdRestartCountParsesAndDegrades(t *testing.T) {
	type call struct {
		command string
		args    string
		timeout time.Duration
	}
	var calls []call

	got, ok := ReadSystemdRestartCount(&SystemdRestartCountOptions{
		Runner: func(command string, args []string, timeout time.Duration) (string, bool) {
			calls = append(calls, call{command: command, args: strings.Join(args, " "), timeout: timeout})
			return "7\n", true
		},
	})
	if !ok || got != 7 {
		t.Fatalf("restart count = %d/%v, want 7/true", got, ok)
	}
	if len(calls) != 1 || calls[0].command != "systemctl" ||
		calls[0].args != "show herdsman.service -p NRestarts --value" ||
		calls[0].timeout != 1500*time.Millisecond {
		t.Fatalf("calls = %+v", calls)
	}

	for _, unusable := range []func() (string, bool){
		func() (string, bool) { return "not-a-number", true },
		func() (string, bool) { return "-1", true },
		func() (string, bool) { return "7.5", true },
		func() (string, bool) { return "", false },
		func() (string, bool) { panic("systemctl exploded") },
	} {
		runner := unusable
		if _, ok := ReadSystemdRestartCount(&SystemdRestartCountOptions{Runner: func(string, []string, time.Duration) (string, bool) {
			return runner()
		}}); ok {
			t.Fatal("expected no restart count for unusable output")
		}
	}
}

func TestReadSystemdRestartCountMissingSystemctl(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadSystemdRestartCount(&SystemdRestartCountOptions{
		Command: filepath.Join(dir, "missing-systemctl"),
		Timeout: 500 * time.Millisecond,
	}); ok {
		t.Fatal("a missing systemctl must yield no fact")
	}
}

func TestReadSystemdRestartCountHardKillsSIGTERMIgnoringProbe(t *testing.T) {
	dir := t.TempDir()
	stubborn := filepath.Join(dir, "stubborn-systemctl")
	// `trap '' TERM` + `exec`: the child ignores SIGTERM (SIG_IGN survives exec)
	// and the sleeping image *is* the direct child, so only a hard kill can end
	// it. A plain `sleep 8` would die on SIGTERM and give false confidence.
	if err := os.WriteFile(stubborn, []byte("#!/bin/sh\ntrap '' TERM\nexec sleep 8\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, ok := ReadSystemdRestartCount(&SystemdRestartCountOptions{Command: stubborn, Timeout: 300 * time.Millisecond}); ok {
		t.Fatal("a killed probe must yield no fact")
	}
	elapsed := time.Since(start)
	// Without a SIGKILL-based bound this waited for the shim's full lifetime
	// (8003ms measured on this machine), i.e. `daemon status` hung for 8s.
	if elapsed >= time.Second {
		t.Fatalf("probe took %s; the timeout must be a hard bound", elapsed)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("probe took %s; expected to use the 300ms budget", elapsed)
	}
}

func TestReportsRunningWithStalePidWhenSocketReachable(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	if err := os.WriteFile(pidPath, []byte("1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := GetDaemonStatus(pidPath, "/tmp/herdsman.sock", &DaemonProcessDependencies{
		ConnectSocket:    func(string) (bool, error) { return true, nil },
		IsProcessRunning: func(int) bool { return false },
	})
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	if status.State != DaemonStateRunning || status.StalePid == nil || *status.StalePid != 1234 ||
		!status.SocketReachable || status.Pid != nil || status.PidFileMissing {
		t.Fatalf("status = %+v", status)
	}
}

func TestWriteDaemonPidFileModeAndParentDirs(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "nested", "herdsman.pid")
	if err := WriteDaemonPidFile(pidPath, 4242); err != nil {
		t.Fatalf("WriteDaemonPidFile: %v", err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "4242\n" {
		t.Fatalf("pid file = %q, want \"4242\\n\"", data)
	}
	info, err := os.Stat(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("pid file mode = %v, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(pidPath))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("parent dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}
}

func TestRemoveDaemonPidFileOnlyForMatchingPid(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")

	// Missing file: no-op
	if RemoveDaemonPidFile(pidPath, 4242) {
		t.Fatal("removing a missing pid file must report false")
	}
	if fileExists(pidPath) {
		t.Fatal("pid file must not exist")
	}

	// Foreign pid: file must be left untouched
	if err := os.WriteFile(pidPath, []byte("9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if RemoveDaemonPidFile(pidPath, 4242) {
		t.Fatal("a foreign pid must not be removed")
	}
	data, err := os.ReadFile(pidPath)
	if err != nil || string(data) != "9999\n" {
		t.Fatalf("pid file = %q, err %v", data, err)
	}

	// Matching pid: removed
	if !RemoveDaemonPidFile(pidPath, 9999) {
		t.Fatal("matching pid must be removed")
	}
	if fileExists(pidPath) {
		t.Fatal("pid file must be gone")
	}
}

func TestReadDaemonRuntimeRecordMissingOrInvalid(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadDaemonRuntimeRecord(filepath.Join(dir, "missing.json")); ok {
		t.Fatal("missing record must not parse")
	}

	recordPath := filepath.Join(dir, "runtime.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(recordPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("not-json")
	if _, ok := ReadDaemonRuntimeRecord(recordPath); ok {
		t.Fatal("unparsable record must not parse")
	}

	valid := map[string]any{
		"dbPath":     "/home/x/state.db",
		"homeDir":    "/home/x",
		"logPath":    "/home/x/herdsman.log",
		"pid":        1234,
		"pidPath":    "/home/x/herdsman.pid",
		"socketPath": "/home/x/herdsman.sock",
		"startedAt":  "2026-06-29T00:00:00.000Z",
		"version":    1,
	}
	encode := func(mutate func(map[string]any)) string {
		record := map[string]any{}
		for key, value := range valid {
			record[key] = value
		}
		mutate(record)
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}

	// The version discriminator and the JSON type of every required field are
	// validated: the TS checks are `value.version !== 1` and
	// `typeof value.<field> !== "string"`. An empty string is a string, so it stays
	// accepted (see the sibling test for the round trip).
	for _, entry := range []struct{ name, body string }{
		{"version 2", encode(func(r map[string]any) { r["version"] = 2 })},
		{"version 0", encode(func(r map[string]any) { r["version"] = 0 })},
		{"version missing", encode(func(r map[string]any) { delete(r, "version") })},
		{"version as string", encode(func(r map[string]any) { r["version"] = "1" })},
		{"dbPath missing", encode(func(r map[string]any) { delete(r, "dbPath") })},
		{"homeDir null", encode(func(r map[string]any) { r["homeDir"] = nil })},
		{"logPath numeric", encode(func(r map[string]any) { r["logPath"] = 7 })},
		{"pidPath missing", encode(func(r map[string]any) { delete(r, "pidPath") })},
		{"socketPath object", encode(func(r map[string]any) { r["socketPath"] = map[string]any{} })},
		{"startedAt missing", encode(func(r map[string]any) { delete(r, "startedAt") })},
		{"array instead of object", "[]"},
		{"null", "null"},
	} {
		write(entry.body)
		if record, ok := ReadDaemonRuntimeRecord(recordPath); ok {
			t.Fatalf("%s must be rejected, got %+v", entry.name, record)
		}
	}
}

func TestReadDaemonRuntimeRecordAcceptsEmptyStringFields(t *testing.T) {
	// `typeof value.dbPath !== "string"` accepts "", so the Go port must not treat
	// an empty path as a missing one.
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "runtime.json")
	body := `{"dbPath":"","homeDir":"","logPath":"","pidPath":"","socketPath":"","startedAt":"","version":1}`
	if err := os.WriteFile(recordPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	record, ok := ReadDaemonRuntimeRecord(recordPath)
	if !ok {
		t.Fatalf("record %s must be accepted", body)
	}
	if record.DBPath != "" || record.Pid != nil {
		t.Fatalf("record = %+v", record)
	}
}

func TestReleaseDaemonLockRemovesRecordWithoutNumericPid(t *testing.T) {
	// The TS guard is `typeof data.pid === "number" && data.pid !== expectedPid`, so
	// only a numeric foreign owner protects the record: a record whose pid is
	// absent or not a number is dropped.
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")
	ownerPath := lockPath + ".owner.json"

	for _, body := range []string{`{}`, `{"pid":null}`, `{"pid":"7"}`} {
		if err := os.WriteFile(ownerPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		ReleaseDaemonLock(lockPath, os.Getpid())
		if fileExists(ownerPath) {
			t.Fatalf("owner record %s must be removed", body)
		}
	}
}

func TestStoppedWithStalePidWhenIdentityProbeFalse(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	if err := os.WriteFile(pidPath, []byte("1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := GetDaemonStatus(pidPath, "/tmp/herdsman.sock", &DaemonProcessDependencies{
		ConnectSocket:    func(string) (bool, error) { return false, nil },
		IdentityProbe:    func(int, []string) (bool, bool) { return false, true },
		IsProcessRunning: func(int) bool { return true },
	})
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	if status.State != DaemonStateStopped || status.StalePid == nil || *status.StalePid != 1234 ||
		status.SocketReachable || status.Pid != nil || status.ManagedBy != "" {
		t.Fatalf("status = %+v", status)
	}
}

func TestRunningWithStalePidWhenPidReusedAndSocketReachable(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	if err := os.WriteFile(pidPath, []byte("1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := GetDaemonStatus(pidPath, "/tmp/herdsman.sock", &DaemonProcessDependencies{
		ConnectSocket:    func(string) (bool, error) { return true, nil },
		IdentityProbe:    func(int, []string) (bool, bool) { return false, true },
		IsProcessRunning: func(int) bool { return true },
	})
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	if status.State != DaemonStateRunning || status.StalePid == nil || *status.StalePid != 1234 ||
		!status.SocketReachable || status.ManagedBy != "" {
		t.Fatalf("status = %+v", status)
	}
}

func TestIdentityProbeUndefinedDegradesToRunningAndWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	// "exactly once" is module state, so the case starts from a clean slate instead
	// of depending on what earlier cases (or an earlier -count pass) did.
	resetUnknownIdentityWarnings()
	pidPath := filepath.Join(dir, "herdsman.pid")
	// A pid nothing else in this file exercises, so the module-wide throttle is
	// observed from a clean slate.
	if err := os.WriteFile(pidPath, []byte("424242\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	warnFile := filepath.Join(dir, "stderr.log")
	f, err := os.OpenFile(warnFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = f
	defer func() {
		os.Stderr = origStderr
		_ = f.Close()
	}()

	deps := &DaemonProcessDependencies{
		ConnectSocket:    func(string) (bool, error) { return true, nil },
		IdentityProbe:    func(int, []string) (bool, bool) { return false, false },
		IsProcessRunning: func(int) bool { return true },
		ReadCgroup:       func(int) (string, bool) { return "", false },
		ReadServiceRestartCount: func() (int, bool) {
			return 0, false
		},
	}

	status, err := GetDaemonStatus(pidPath, "/tmp/herdsman.sock", deps)
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	if status.State != DaemonStateRunning || status.Pid == nil || *status.Pid != 424242 ||
		status.ManagedBy != DaemonManagedByUnknown || status.RestartCount != nil || !status.SocketReachable {
		t.Fatalf("status = %+v", status)
	}
	if warnings := readWarnings(t, f); warnings != 1 {
		t.Fatalf("warnings = %d, want 1\n%s", warnings, stderrText(t, f))
	}

	// Calling again with the same PID must not log another warning.
	status, err = GetDaemonStatus(pidPath, "/tmp/herdsman.sock", deps)
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	if status.State != DaemonStateRunning {
		t.Fatalf("status = %+v", status)
	}
	if warnings := readWarnings(t, f); warnings != 1 {
		t.Fatalf("warnings after second call = %d, want 1\n%s", warnings, stderrText(t, f))
	}
	if message := stderrText(t, f); !strings.Contains(message, "Unable to verify daemon process identity for PID 424242") {
		t.Fatalf("warning text = %q", message)
	}
}

func stderrText(t *testing.T, f *os.File) string {
	t.Helper()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readWarnings(t *testing.T, f *os.File) int {
	t.Helper()
	text := stderrText(t, f)
	if text == "" {
		return 0
	}
	return len(strings.Split(strings.TrimRight(text, "\n"), "\n"))
}

func TestReadDaemonProcessIdentityOnMissingPid(t *testing.T) {
	identified, hasResult := ReadDaemonProcessIdentity(999999999, nil)
	if _, err := os.Stat("/proc"); err != nil {
		if hasResult {
			t.Fatal("without /proc the identity probe must report \"cannot verify\"")
		}
		return
	}
	if !hasResult || identified {
		t.Fatalf("identity = %v/%v, want false/true for a non-existent pid", identified, hasResult)
	}
}

func TestIdentityProbeNarrowsToDaemonEntrypoint(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	selfPid := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", selfPid)), 0o600); err != nil {
		t.Fatal(err)
	}

	// The test runner process is not "herdsman-daemon.js".
	status, err := GetDaemonStatus(pidPath, "/tmp/herdsman.sock", &DaemonProcessDependencies{
		ConnectSocket:    func(string) (bool, error) { return false, nil },
		IsProcessRunning: func(int) bool { return true },
	})
	if err != nil {
		t.Fatalf("GetDaemonStatus: %v", err)
	}
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("/proc unavailable: identity probe degrades instead of rejecting")
	}
	if status.State != DaemonStateStopped || status.StalePid == nil || *status.StalePid != selfPid {
		t.Fatalf("status = %+v, want stopped with stalePid %d", status, selfPid)
	}
}

func TestDaemonInstanceLockPathIsDistinct(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "herdsman.pid")
	if got := DaemonInstanceLockPath(pidPath); got != pidPath+".instance.lock" {
		t.Fatalf("instance lock path = %q", got)
	}
	if DaemonInstanceLockPath(pidPath) == pidPath+".lock" {
		t.Fatal("instance lock path must differ from the plain lock path")
	}
}

func TestReadRuntimeRecordWithoutPidAndWithLegacyPid(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "runtime.json")

	withoutPid := runtimeRecord(dir)
	withoutPid.Pid = nil
	encoded, err := json.MarshalIndent(withoutPid, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, ok := ReadDaemonRuntimeRecord(recordPath)
	if !ok {
		t.Fatal("record without pid must parse")
	}
	if parsed.Pid != nil {
		t.Fatalf("pid = %d, want absent", *parsed.Pid)
	}

	// A legacy record that still carries a pid remains readable (pid is optional
	// metadata, never a required validation field).
	withPid := runtimeRecord(dir)
	encoded, err = json.MarshalIndent(withPid, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, ok = ReadDaemonRuntimeRecord(recordPath)
	if !ok || parsed.Pid == nil || *parsed.Pid != 1234 {
		t.Fatalf("legacy pid record = %+v/%v", parsed, ok)
	}
}

func TestIsFlockHeldDetectsState(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")

	// Non-existent path reports false.
	held, err := IsFlockHeld(lockPath)
	if err != nil || held {
		t.Fatalf("held = %v/%v for a missing lock file", held, err)
	}

	release, err := AcquireDaemonLock(lockPath, nil)
	if err != nil {
		t.Fatalf("AcquireDaemonLock: %v", err)
	}
	held, err = IsFlockHeld(lockPath)
	if err != nil || !held {
		t.Fatalf("held = %v/%v while the lock is owned", held, err)
	}

	release()
	// release() drops the descriptor; the kernel can lag a moment behind it.
	waitForCondition(t, "the released flock to be observable as free", func() bool {
		held, err := IsFlockHeld(lockPath)
		return err == nil && !held
	})
	held, err = IsFlockHeld(lockPath)
	if err != nil || held {
		t.Fatalf("held = %v/%v after release", held, err)
	}
}

func TestDaemonLockMutualExclusionAndOwnerTracking(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")

	release, err := AcquireDaemonLock(lockPath, nil)
	if err != nil {
		t.Fatalf("AcquireDaemonLock: %v", err)
	}
	if !fileExists(lockPath) || !fileExists(lockPath+".owner.json") {
		t.Fatalf("lock/owner files missing after acquire: %v %v",
			fileExists(lockPath), fileExists(lockPath+".owner.json"))
	}

	_, err = AcquireDaemonLock(lockPath, nil)
	if err == nil {
		t.Fatal("a second acquisition must fail while the lock is held")
	}
	message := err.Error()
	for _, want := range []string{
		"Herdsman daemon operation lock is held",
		"systemctl status herdsman.service",
		"仅当确认无 daemon 与 CLI 操作在跑时才可删除锁文件",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("lock error %q missing %q", message, want)
		}
	}

	release()
	// The lock file is persistent and NEVER removed.
	if !fileExists(lockPath) {
		t.Fatal("lock file must survive release")
	}
	if fileExists(lockPath + ".owner.json") {
		t.Fatal("owner record must be removed on release")
	}

	release2, err := AcquireDaemonLock(lockPath, nil)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if !fileExists(lockPath) || !fileExists(lockPath+".owner.json") {
		t.Fatal("lock/owner files missing after re-acquire")
	}
	release2()
	if !fileExists(lockPath) {
		t.Fatal("lock file must survive the second release")
	}
	if fileExists(lockPath + ".owner.json") {
		t.Fatal("owner record must be removed on the second release")
	}
}

func TestReleaseDaemonLockKeepsForeignOwnerRecord(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")
	ownerPath := lockPath + ".owner.json"
	foreignPid := os.Getpid() + 1000

	if err := os.WriteFile(lockPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := DaemonLockOwner{Pid: foreignPid, StartedAt: isoTimestampMillis(time.Now())}
	encoded, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	// Releasing with our PID must NOT delete someone else's owner.json.
	ReleaseDaemonLock(lockPath, os.Getpid())
	if !fileExists(ownerPath) {
		t.Fatal("foreign owner record must be kept")
	}

	// Releasing with the matching PID deletes owner.json.
	ReleaseDaemonLock(lockPath, foreignPid)
	if fileExists(ownerPath) {
		t.Fatal("matching owner record must be removed")
	}
}

func TestReleaseDaemonLockKeepsCorruptOwnerRecord(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")
	ownerPath := lockPath + ".owner.json"
	if err := os.WriteFile(ownerPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	ReleaseDaemonLock(lockPath, os.Getpid())
	if !fileExists(ownerPath) {
		t.Fatal("a corrupt owner record must not be removed")
	}
}

func TestStaleOwnerRecordWithLiveUnrelatedPidDoesNotBlockAcquire(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")
	ownerPath := lockPath + ".owner.json"

	if err := os.WriteFile(lockPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := DaemonLockOwner{
		Pid:       1, // Init/systemd process is definitely alive
		StartedAt: isoTimestampMillis(time.Now().Add(-time.Hour)),
	}
	encoded, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	// PID 1 does not hold the kernel flock on lockPath, so acquisition succeeds.
	release, err := AcquireDaemonLock(lockPath, nil)
	if err != nil {
		t.Fatalf("AcquireDaemonLock: %v", err)
	}
	if !fileExists(lockPath) {
		t.Fatal("lock file must exist")
	}
	var updated DaemonLockOwner
	data, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &updated); err != nil {
		t.Fatalf("owner record unreadable: %v", err)
	}
	if updated.Pid != os.Getpid() {
		t.Fatalf("owner pid = %d, want %d", updated.Pid, os.Getpid())
	}

	release()
	if !fileExists(lockPath) {
		t.Fatal("lock file must stay persistent")
	}
}

func TestAcquireDaemonLockUsesInjectedPid(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "herdsman.pid.lock")

	release, err := AcquireDaemonLock(lockPath, &DaemonProcessDependencies{PID: 4242, HasPID: true})
	if err != nil {
		t.Fatalf("AcquireDaemonLock: %v", err)
	}
	data, err := os.ReadFile(lockPath + ".owner.json")
	if err != nil {
		t.Fatal(err)
	}
	var owner DaemonLockOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		t.Fatal(err)
	}
	if owner.Pid != 4242 {
		t.Fatalf("owner pid = %d, want the injected 4242", owner.Pid)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", owner.StartedAt); err != nil {
		t.Fatalf("startedAt %q is not an ISO millisecond timestamp: %v", owner.StartedAt, err)
	}
	release()
}

func TestLockHeldMessageFormatsOwnerPid(t *testing.T) {
	if got := LockHeldMessage("/tmp/x.lock", nil); !strings.HasPrefix(got, "Herdsman daemon operation lock is held: /tmp/x.lock.") {
		t.Fatalf("message = %q", got)
	}
	if strings.Contains(LockHeldMessage("/tmp/x.lock", nil), " by PID") {
		t.Fatal("message must omit the owner when unknown")
	}
	if got := LockHeldMessage("/tmp/x.lock", intPtr(4321)); !strings.Contains(got, " is held by PID 4321: /tmp/x.lock.") {
		t.Fatalf("message = %q", got)
	}
}

func TestReadOwnerPidValidation(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "a.lock")
	ownerPath := lockPath + ".owner.json"

	if pid := readOwnerPid(lockPath); pid != nil {
		t.Fatalf("missing owner record returned %v", *pid)
	}
	for _, tc := range []struct {
		name  string
		body  string
		valid bool
	}{
		{"positive", `{"pid":7,"startedAt":"x"}`, true},
		{"zero", `{"pid":0,"startedAt":"x"}`, false},
		{"negative", `{"pid":-3,"startedAt":"x"}`, false},
		{"fractional", `{"pid":1.5,"startedAt":"x"}`, false},
		{"string", `{"pid":"7","startedAt":"x"}`, false},
		{"corrupt", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(ownerPath, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			pid := readOwnerPid(lockPath)
			if tc.valid != (pid != nil) {
				t.Fatalf("readOwnerPid(%s) = %v, want valid=%v", tc.body, pid, tc.valid)
			}
		})
	}
}

func TestDefaultConnectSocketOnClosedSocket(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "herdsman.sock")

	if reachable, err := DefaultConnectSocket(socketPath); err != nil || reachable {
		t.Fatalf("reachable = %v/%v for an absent socket", reachable, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if reachable, err := DefaultConnectSocket(socketPath); err != nil || !reachable {
		t.Fatalf("reachable = %v/%v for a listening socket", reachable, err)
	}
}

func TestJsNumberAndIntegerSemantics(t *testing.T) {
	if got := jsNumber("1234\n"); got != 1234 {
		t.Fatalf("jsNumber(\"1234\\n\") = %v", got)
	}
	if got := jsNumber(""); got != 0 {
		t.Fatalf("jsNumber(\"\") = %v, want 0 like JS Number(\"\")", got)
	}
	if got := jsNumber("abc"); !math.IsNaN(got) {
		t.Fatalf("jsNumber(\"abc\") = %v, want NaN", got)
	}
	if isIntegerNumber(jsNaN) || isIntegerNumber(math.Inf(1)) || isIntegerNumber(1.5) {
		t.Fatal("isIntegerNumber must reject NaN, Infinity and fractions")
	}
	if !isIntegerNumber(0) || !isIntegerNumber(-7) {
		t.Fatal("isIntegerNumber must accept integers")
	}
	// The NaN sentinel never compares equal, exactly like JS NaN.
	if jsNaN == jsNaN {
		t.Fatal("jsNaN must not equal itself")
	}
	if !errors.Is(syscall.EWOULDBLOCK, syscall.EAGAIN) {
		t.Log("EWOULDBLOCK and EAGAIN differ on this platform")
	}
}

func TestPrepareDaemonPathsCorrectsHomeDBWALAndSHM(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	socketPath := filepath.Join(dir, "herdsman.sock")
	for _, path := range []string{db, db + "-wal", db + "-shm"} {
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	if err := PrepareDaemonPaths(dir, db, socketPath); err != nil {
		t.Fatalf("PrepareDaemonPaths: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("home mode = %o, want 0700", info.Mode().Perm())
	}
	for _, path := range []string{db, db + "-wal", db + "-shm"} {
		fileInfo, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fileInfo.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", path, fileInfo.Mode().Perm())
		}
	}
}

func TestPrepareDaemonPathsCreatesHomeDBAndSocketDirs(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "nested", "herdsman")
	db := filepath.Join(home, "data", "state.db")
	socketPath := filepath.Join(home, "run", "herdsman.sock")

	if err := PrepareDaemonPaths(home, db, socketPath); err != nil {
		t.Fatalf("PrepareDaemonPaths: %v", err)
	}
	for _, dir := range []string{home, filepath.Dir(db), filepath.Dir(socketPath)} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s must exist: %v", dir, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", dir, info.Mode().Perm())
		}
	}
	// Absent database files are not created by the permission fix.
	for _, path := range []string{db, db + "-wal", db + "-shm"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s must not be created (stat err %v)", path, err)
		}
	}
}

func TestPrepareDaemonPathsTightensExistingDBDir(t *testing.T) {
	home := t.TempDir()
	dbDir := filepath.Join(home, "data")
	db := filepath.Join(dbDir, "state.sqlite")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(db, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := PrepareDaemonPaths(home, db, filepath.Join(home, "herdsman.sock")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dbDir: 0o700, db: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

func runtimeRecord(dir string) DaemonRuntimeRecord {
	pid := 1234
	return DaemonRuntimeRecord{
		DBPath:     filepath.Join(dir, "state.db"),
		HomeDir:    dir,
		LogPath:    filepath.Join(dir, "herdsman.log"),
		Pid:        &pid,
		PidPath:    filepath.Join(dir, "herdsman.pid"),
		SocketPath: "/tmp/herdsman.sock",
		StartedAt:  "2026-06-29T00:00:00.000Z",
		Version:    1,
	}
}
