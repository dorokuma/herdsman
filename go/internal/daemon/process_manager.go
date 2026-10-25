package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// DaemonRuntimeRecord is the on-disk `runtime.json` record. Field tags, the
// `version: 1` discriminator and the optional `pid` match the TS type
// byte-for-byte (JSON is the cross-implementation contract).
type DaemonRuntimeRecord struct {
	DBPath     string `json:"dbPath"`
	HomeDir    string `json:"homeDir"`
	LogPath    string `json:"logPath"`
	Pid        *int   `json:"pid,omitempty"`
	PidPath    string `json:"pidPath"`
	SocketPath string `json:"socketPath"`
	StartedAt  string `json:"startedAt"`
	Version    int    `json:"version"`
}

// DaemonStatusFacts carries read-only facts about which supervisor owns the
// running daemon. Computed only on the branch where a daemon pid is known *and*
// its identity was confirmed; every other branch omits the fields entirely
// (empty string / nil pointer) rather than guessing.
//
// ManagedBy is set whenever that pid branch is taken, and falls back to
// "unknown" when its probe yields nothing. RestartCount is set only when
// ManagedBy is `systemd:herdsman.service` *and* the probe succeeded: the value
// comes from `herdsman.service`, so it is meaningless for any other supervisor
// (see IsSystemdHerdsmanManaged).
type DaemonStatusFacts struct {
	ManagedBy    DaemonManagedBy
	RestartCount *int
}

// IsSystemdHerdsmanManaged is true when the daemon pid is supervised by the
// herdsman *system* unit — the only case in which the systemd `NRestarts` probe
// describes this pid.
func IsSystemdHerdsmanManaged(managedBy DaemonManagedBy) bool {
	return managedBy == SystemdManagedBy(HerdsmanSystemdUnit)
}

// DaemonStatus states.
const (
	DaemonStateRunning = "running"
	DaemonStateStopped = "stopped"
)

// DaemonStatus is the result of GetDaemonStatus. The TS union is modelled with
// one struct plus the discriminating fields:
//
//   - running with a live pid: Pid set, SocketReachable reported;
//   - running with a missing pid file: PidFileMissing true;
//   - running with a stale/foreign pid: StalePid set;
//   - stopped: State "stopped", StalePid set only when a pid file was readable.
//
// Empty PidPath/SocketPath are impossible (always copied from the input).
type DaemonStatus struct {
	State           string
	PidPath         string
	SocketPath      string
	Pid             *int
	PidFileMissing  bool
	SocketReachable bool
	StalePid        *int
	ManagedBy       DaemonManagedBy
	RestartCount    *int
}

// DaemonEntrypointNames are the process basenames accepted as the daemon
// entrypoint.
var DaemonEntrypointNames = []string{"herdsman-daemon.js"}

// DaemonInstanceLockPath is the instance lock path for the daemon's bare
// entrypoint (herdsman-daemon.js). Kept distinct from the plain
// `${pidPath}.lock` name so a daemon that owns its home through this lock
// cannot collide with any other lock namespace.
func DaemonInstanceLockPath(pidPath string) string {
	return pidPath + ".instance.lock"
}

var warnedUnknownIdentityPids sync.Map

// resetUnknownIdentityWarnings clears the per-pid "cannot verify identity" throttle.
// The TS suite gets the same fresh state via `vi.resetModules()`, because "exactly
// once per process" is module state nothing outside the module can reset; this
// export keeps that property testable (and repeat test runs independent) without
// changing production behaviour.
func resetUnknownIdentityWarnings() { warnedUnknownIdentityPids.Clear() }

// DaemonProcessDependencies are the injectable seams of the process manager.
// A nil field means "use the production implementation".
type DaemonProcessDependencies struct {
	// ConnectSocket reports whether the daemon socket accepts a connection.
	ConnectSocket func(socketPath string) (bool, error)
	// IsProcessRunning reports liveness; the TS EPERM/EACCES/ESRCH semantics are
	// already folded into the returned bool by the override.
	IsProcessRunning func(pid int) bool
	// IdentityProbe returns (result, hasResult); hasResult false models the TS
	// `undefined` ("cannot verify").
	IdentityProbe func(pid int, expectedNames []string) (bool, bool)
	// PID overrides process.pid.
	PID    int
	HasPID bool
	// ReadCgroup overrides the `/proc/<pid>/cgroup` read used to name the
	// supervisor; ok false models the TS `undefined`.
	ReadCgroup func(pid int) (string, bool)
	// ReadServiceRestartCount overrides the best-effort `systemctl show ...
	// NRestarts` probe. Only called when the pid is supervised by the herdsman
	// system unit.
	ReadServiceRestartCount func() (int, bool)
}

// ReadDaemonRuntimeRecord reads and validates the runtime record. It returns
// (record, true) only when the file exists, parses as JSON and carries version 1
// plus every required string field. Like the TS validator, a field is judged by
// its JSON type: an empty string is a string and is therefore accepted, while a
// missing or non-string field rejects the record. `pid` stays optional metadata
// that never takes part in validation.
func ReadDaemonRuntimeRecord(path string) (*DaemonRuntimeRecord, bool) {
	if !fileExists(path) {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	// Pointer fields distinguish "absent / wrong type" (nil) from "empty string",
	// which the TS `typeof x === "string"` check accepts.
	var value struct {
		DBPath     *string  `json:"dbPath"`
		HomeDir    *string  `json:"homeDir"`
		LogPath    *string  `json:"logPath"`
		Pid        any      `json:"pid"`
		PidPath    *string  `json:"pidPath"`
		SocketPath *string  `json:"socketPath"`
		StartedAt  *string  `json:"startedAt"`
		Version    *float64 `json:"version"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, false
	}
	if value.Version == nil || *value.Version != 1 ||
		value.DBPath == nil || value.HomeDir == nil || value.LogPath == nil ||
		value.PidPath == nil || value.SocketPath == nil || value.StartedAt == nil {
		return nil, false
	}
	record := &DaemonRuntimeRecord{
		DBPath:     *value.DBPath,
		HomeDir:    *value.HomeDir,
		LogPath:    *value.LogPath,
		PidPath:    *value.PidPath,
		SocketPath: *value.SocketPath,
		StartedAt:  *value.StartedAt,
		Version:    1,
	}
	// pid is optional metadata that never takes part in validation, and the TS
	// validator does not type-check it either — a foreign type must not invalidate
	// the record, it simply has no int representation to carry.
	if number, ok := value.Pid.(float64); ok && number == float64(int(number)) {
		pid := int(number)
		record.Pid = &pid
	}
	return record, true
}

// WriteDaemonPidFile writes `<pid>\n` with mode 0600, creating the parent
// directory with mode 0700.
func WriteDaemonPidFile(pidPath string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// RemoveDaemonPidFile removes the pid file only when its content matches
// expectedPid. Any read/remove failure is swallowed: pid clean-up failure must
// never abort shutdown.
func RemoveDaemonPidFile(pidPath string, expectedPid int) bool {
	if !fileExists(pidPath) {
		return false
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	if jsNumber(strings.TrimSpace(string(data))) != float64(expectedPid) {
		return false
	}
	if err := os.Remove(pidPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return true
}

// GetDaemonStatus computes the daemon status. Mirrors getDaemonStatus in
// process-manager.ts, including the exact branch order, the stale-pid metadata
// and the identity-probe warning throttle.
func GetDaemonStatus(pidPath, socketPath string, deps *DaemonProcessDependencies) (DaemonStatus, error) {
	var d DaemonProcessDependencies
	if deps != nil {
		d = *deps
	}
	processIsRunning := d.IsProcessRunning
	if processIsRunning == nil {
		processIsRunning = IsProcessRunning
	}
	identityProbe := d.IdentityProbe
	if identityProbe == nil {
		identityProbe = ReadDaemonProcessIdentity
	}
	connectSocket := d.ConnectSocket
	if connectSocket == nil {
		connectSocket = DefaultConnectSocket
	}
	probeCgroup := d.ReadCgroup
	if probeCgroup == nil {
		probeCgroup = ReadProcessCgroup
	}
	probeRestartCount := d.ReadServiceRestartCount
	if probeRestartCount == nil {
		probeRestartCount = func() (int, bool) { return ReadSystemdRestartCount(nil) }
	}
	connect := func() bool {
		reachable, err := connectSocket(socketPath)
		if err != nil {
			return false
		}
		return reachable
	}

	if !fileExists(pidPath) {
		if connect() {
			return DaemonStatus{
				State:           DaemonStateRunning,
				PidPath:         pidPath,
				PidFileMissing:  true,
				SocketPath:      socketPath,
				SocketReachable: true,
			}, nil
		}
		return DaemonStatus{State: DaemonStateStopped, PidPath: pidPath, SocketPath: socketPath}, nil
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		return DaemonStatus{}, err
	}
	rawPid := strings.TrimSpace(string(data))
	pidFloat := jsNumber(rawPid)
	pid := 0
	pidValid := isIntegerNumber(pidFloat) && pidFloat > 0 && pidFloat == float64(int(pidFloat))
	if pidValid {
		pid = int(pidFloat)
	}
	if !pidValid || !processIsRunning(pid) {
		return staleStatus(pidFloat, pidPath, socketPath, connect), nil
	}

	identified, hasIdentity := identityProbe(pid, DaemonEntrypointNames)
	if !hasIdentity {
		if _, warned := warnedUnknownIdentityPids.LoadOrStore(pid, struct{}{}); !warned {
			fmt.Fprintf(os.Stderr, "Unable to verify daemon process identity for PID %d: /proc unavailable\n", pid)
		}
	} else if !identified {
		return staleStatus(pidFloat, pidPath, socketPath, connect), nil
	}

	status := DaemonStatus{
		State:           DaemonStateRunning,
		PidPath:         pidPath,
		Pid:             &pid,
		SocketPath:      socketPath,
		SocketReachable: connect(),
	}
	facts := supervisionFacts(pid, probeCgroup, probeRestartCount)
	status.ManagedBy = facts.ManagedBy
	status.RestartCount = facts.RestartCount
	return status, nil
}

// staleStatus builds the "pid file points at a dead or foreign process" verdict.
// StalePid is reported only for a finite integer pid; JS reports NaN there, which
// has no int representation and is rendered as "no stale pid".
func staleStatus(pidFloat float64, pidPath, socketPath string, connect func() bool) DaemonStatus {
	status := DaemonStatus{PidPath: pidPath, SocketPath: socketPath}
	if isIntegerNumber(pidFloat) && pidFloat == float64(int(pidFloat)) {
		pid := int(pidFloat)
		status.StalePid = &pid
	}
	if connect() {
		status.State = DaemonStateRunning
		status.SocketReachable = true
		return status
	}
	status.State = DaemonStateStopped
	return status
}

// supervisionFacts computes best-effort supervision facts for a known daemon
// pid. It never fails: a probe that errors only degrades its own field, so
// `daemon status` keeps its state and exit code no matter how broken `/proc` or
// `systemctl` is.
//
// Diagnostic path only: probeRestartCount spawns `systemctl` synchronously (see
// service_supervision.go), so this must never be called from daemon lifecycle
// or hot paths.
func supervisionFacts(
	pid int,
	probeCgroup func(pid int) (string, bool),
	probeRestartCount func() (int, bool),
) DaemonStatusFacts {
	managedBy := DaemonManagedByUnknown
	func() {
		defer func() {
			if recover() != nil {
				managedBy = DaemonManagedByUnknown
			}
		}()
		if cgroup, ok := probeCgroup(pid); ok {
			managedBy = ManagedByFromCgroup(cgroup, "")
		}
	}()

	// Only probe the restart count for a pid the herdsman system unit actually
	// supervises. `NRestarts` is a property of `herdsman.service`, so for an
	// `unmanaged` (or `unknown`) daemon it would be the count of a unit that does
	// not own this pid — and `managedBy: unmanaged` + `restartCount: 3` reads
	// like "this stray daemon restarted three times". Skipping the probe also
	// keeps the diagnostic path from spawning `systemctl` for an answer that
	// could not mean anything.
	if !IsSystemdHerdsmanManaged(managedBy) {
		return DaemonStatusFacts{ManagedBy: managedBy}
	}

	var restartCount *int
	func() {
		defer func() {
			if recover() != nil {
				restartCount = nil
			}
		}()
		if value, ok := probeRestartCount(); ok {
			restartCount = &value
		}
	}()

	// `0` here means "systemd has not restarted the unit since it loaded it"
	// (reset-failed / stop+start / an unloaded unit all reset the counter), not
	// "this daemon has never crashed".
	return DaemonStatusFacts{ManagedBy: managedBy, RestartCount: restartCount}
}

// ReadDaemonProcessIdentity checks `/proc/<pid>/cmdline` for one of
// expectedNames (as a basename). It returns (result, hasResult): hasResult
// false models "cannot verify" (`/proc` missing, EACCES/EPERM or any unexpected
// read error).
func ReadDaemonProcessIdentity(pid int, expectedNames []string) (bool, bool) {
	if expectedNames == nil {
		expectedNames = DaemonEntrypointNames
	}
	if !fileExists("/proc") {
		return false, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			switch {
			case errors.Is(pathErr.Err, syscall.ENOENT), errors.Is(pathErr.Err, syscall.ESRCH):
				return false, true
			case errors.Is(pathErr.Err, syscall.EACCES), errors.Is(pathErr.Err, syscall.EPERM):
				return false, false
			}
		}
		// Other read errors (e.g. EINVAL, EIO): safe fallback is "cannot verify".
		return false, false
	}
	for _, part := range strings.Split(string(data), "\x00") {
		if part == "" {
			continue
		}
		for _, expected := range expectedNames {
			if filepath.Base(part) == expected {
				return true, true
			}
		}
	}
	return false, true
}

// ProcessProbe is the injectable `process.kill(pid, signal)` seam.
type ProcessProbe func(pid int, signal syscall.Signal) error

// IsProcessRunning reports whether pid exists. A permission error (EPERM/EACCES)
// counts as alive, exactly like the TS `process.kill(pid, 0)` handling.
func IsProcessRunning(pid int) bool {
	return IsProcessRunningWith(pid, nil)
}

// IsProcessRunningWith is IsProcessRunning with an injectable probe. A nil probe
// uses `syscall.Kill(pid, 0)`.
func IsProcessRunningWith(pid int, probe ProcessProbe) bool {
	var err error
	if probe == nil {
		err = syscall.Kill(pid, 0)
	} else {
		err = probe(pid, 0)
	}
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// DaemonLockOwner is the `<lockPath>.owner.json` diagnostic record.
type DaemonLockOwner struct {
	Pid       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
}

// ReleaseDaemonLock removes `<lockPath>.owner.json` when it does not name a
// different process (expectedPid 0 means the current process). The TS check is
// `typeof data.pid === "number" && data.pid !== expectedPid`, so only a *numeric*
// foreign owner protects the record; a record without a usable pid is dropped.
// A corrupt (unparsable) owner record is left in place.
func ReleaseDaemonLock(lockPath string, expectedPid int) {
	if expectedPid == 0 {
		expectedPid = os.Getpid()
	}
	ownerPath := lockPath + ".owner.json"
	if !fileExists(ownerPath) {
		return
	}
	data, err := os.ReadFile(ownerPath)
	if err != nil {
		return
	}
	var owner map[string]any
	if err := json.Unmarshal(data, &owner); err != nil {
		// If owner.json is corrupted, do not remove
		return
	}
	if recorded, ok := owner["pid"].(float64); ok && recorded != float64(expectedPid) {
		return
	}
	_ = os.Remove(ownerPath)
}

// FlockHandle owns a kernel flock. It is safe for concurrent use and its
// Release is idempotent.
type FlockHandle struct {
	file *os.File
	done atomic.Bool
	once sync.Once
}

// Release drops the lock and closes the descriptor. Calling it more than once
// is a no-op.
func (h *FlockHandle) Release() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.done.Store(true)
		_ = syscall.Flock(int(h.file.Fd()), syscall.LOCK_UN)
		_ = h.file.Close()
	})
}

// IsFlockHeld reports whether the kernel flock on lockPath is currently held by
// another open file description. A missing lock file is reported as not held.
// Environmental errors are returned rather than swallowed, matching the TS
// throw contract for unexpected exits.
func IsFlockHeld(lockPath string) (bool, error) {
	if !fileExists(lockPath) {
		return false, nil
	}
	file, err := os.OpenFile(lockPath, os.O_RDONLY, 0o600)
	if err != nil {
		return false, fmt.Errorf("failed to probe flock on %s: %w", lockPath, err)
	}
	defer func() { _ = file.Close() }()
	// LOCK_EX|LOCK_NB on a *different* open file description fails with EWOULDBLOCK
	// exactly when someone else holds the lock. Our own probe fd never conflicts
	// with itself, so nothing needs to be undone on success.
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return false, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
		return true, nil
	default:
		return false, fmt.Errorf("flock probe failed on %s: %w", lockPath, err)
	}
}

// AcquireFlockHandle takes a non-blocking exclusive flock on lockPath and
// returns the handle, or nil when another process holds it.
//
// The lock file is a persistent regular file and is NEVER removed: unlinking it
// would let a second process lock a fresh inode and produce two masters.
func AcquireFlockHandle(lockPath string) (*FlockHandle, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	// "a" keeps the file persistent and creates it when absent.
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, nil
		}
		return nil, err
	}
	return &FlockHandle{file: file}, nil
}

// LockHeldMessage formats the diagnostic "lock is held" error. The Chinese
// suffix is part of the operator-facing contract and is kept verbatim.
func LockHeldMessage(lockPath string, ownerPid *int) string {
	ownerInfo := ""
	if ownerPid != nil {
		ownerInfo = fmt.Sprintf(" by PID %d", *ownerPid)
	}
	return fmt.Sprintf(
		"Herdsman daemon operation lock is held%s: %s. 先看 systemctl status herdsman.service 确认 daemon 状态；仅当确认无 daemon 与 CLI 操作在跑时才可删除锁文件（删掉会让新进程在新 inode 上重新加锁成功，可能出现两个实例同时跑）: %s",
		ownerInfo, lockPath, lockPath,
	)
}

// readOwnerPid returns the pid recorded in `<lockPath>.owner.json`, or nil when
// the file is missing, unparsable or carries no positive integer pid.
func readOwnerPid(lockPath string) *int {
	ownerPath := lockPath + ".owner.json"
	if !fileExists(ownerPath) {
		return nil
	}
	data, err := os.ReadFile(ownerPath)
	if err != nil {
		return nil
	}
	var owner struct {
		Pid *float64 `json:"pid"`
	}
	if err := json.Unmarshal(data, &owner); err != nil || owner.Pid == nil {
		return nil
	}
	if !isIntegerNumber(*owner.Pid) || *owner.Pid <= 0 {
		return nil
	}
	pid := int(*owner.Pid)
	return &pid
}

// AcquireDaemonLock takes the daemon operation lock and returns an idempotent
// release function, or an error carrying LockHeldMessage when another holder
// owns it.
func AcquireDaemonLock(lockPath string, deps *DaemonProcessDependencies) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	currentPid := os.Getpid()
	if deps != nil && deps.HasPID {
		currentPid = deps.PID
	}

	handle, err := AcquireFlockHandle(lockPath)
	if err != nil {
		return nil, err
	}
	if handle == nil {
		return nil, errors.New(LockHeldMessage(lockPath, readOwnerPid(lockPath)))
	}

	// Flock acquired successfully. Write diagnostic owner metadata.
	ownerPath := lockPath + ".owner.json"
	owner := DaemonLockOwner{
		Pid:       currentPid,
		StartedAt: isoTimestampMillis(time.Now()),
	}
	if encoded, err := json.MarshalIndent(owner, "", "  "); err == nil {
		// Diagnostic write error does not invalidate the kernel lock.
		_ = os.WriteFile(ownerPath, append(encoded, '\n'), 0o600)
	}

	var released atomic.Bool
	return func() {
		if released.Swap(true) {
			return
		}
		if fileExists(ownerPath) {
			if data, err := os.ReadFile(ownerPath); err == nil {
				var recorded DaemonLockOwner
				if err := json.Unmarshal(data, &recorded); err == nil && recorded.Pid == currentPid {
					_ = os.Remove(ownerPath)
				}
			}
		}
		handle.Release()
	}, nil
}

// DefaultConnectSocket dials the daemon unix socket with a 200ms connect
// timeout, reporting reachability.
func DefaultConnectSocket(socketPath string) (bool, error) {
	conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
	if err != nil {
		return false, nil
	}
	_ = conn.Close()
	return true, nil
}

// PrepareDaemonPaths creates and tightens the daemon's data directories, exactly
// as the daemon startup does in src/daemon/service.ts: home, database directory
// and socket directory are 0700, and an already-present database plus its WAL and
// SHM sidecars are chmodded to 0600 after the database is opened.
//
// It also gives the "data directory permissions" regression
// (test/unit/final-audit-regressions.test.ts) something to assert against: the TS
// case chmods the files by hand and then re-reads them, so it proves nothing about
// the daemon. Note that chmod overrides a umask that would otherwise widen the
// freshly created directories.
func PrepareDaemonPaths(homeDir, dbPath, socketPath string) error {
	for _, dir := range []string{homeDir, filepath.Dir(dbPath), filepath.Dir(socketPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if !fileExists(path) {
			continue
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isoTimestampMillis formats like JS `new Date().toISOString()`:
// 2026-06-29T00:00:00.000Z.
func isoTimestampMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// jsNumber reproduces the JS `Number(string)` conversion for the inputs this
// package sees: trimmed numeric text, "" and whitespace (0), otherwise NaN.
func jsNumber(raw string) float64 {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return jsNaN
	}
	return value
}

// jsNaN mirrors JS NaN: never equal to anything, so every comparison against it
// fails just like `Number.isInteger(Number("abc"))` returning false.
var jsNaN = math.NaN()

// isIntegerNumber is JS `Number.isInteger`: finite and without a fractional part.
func isIntegerNumber(value float64) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return false
	}
	return value == math.Trunc(value)
}
