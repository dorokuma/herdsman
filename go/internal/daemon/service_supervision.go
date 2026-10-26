// Package daemon ports src/daemon/service-supervision.ts and
// src/daemon/process-manager.ts.
//
// Read-only supervision facts for `herdsman daemon status`.
//
// These answer "who supervises this daemon, and how many times has it
// restarted?" without touching any lifecycle behaviour: nothing here starts,
// stops, restarts or locks the daemon. Every probe is best-effort, so a
// missing `systemctl`, a denied `/proc` read or a wedged system manager
// degrades the answer instead of failing `daemon status`.
package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// HerdsmanSystemdUnit is the systemd unit that supervises the production daemon.
const HerdsmanSystemdUnit = "herdsman.service"

// SupervisionProbeTimeout is the short, fixed timeout for the best-effort
// `systemctl` probe. `daemon status` is a diagnostic command, so it must never
// hang behind a wedged or unavailable system manager; 1.5s is far above a
// healthy `systemctl show` (single-digit ms) yet small enough to stay
// imperceptible when the manager is stuck.
const SupervisionProbeTimeout = 1500 * time.Millisecond

// DaemonManagedBy says who supervises the running daemon:
//   - "systemd:<unit>" — the daemon pid lives in that unit's cgroup inside the
//     *system* slice, i.e. systemd's system manager owns it;
//   - "unmanaged" — the cgroup was readable and named something, but the pid is
//     not supervised by `herdsman.service`. That is all it says: it is **not** a
//     claim that nothing else supervises the process, and it is not a licence
//     to kill it;
//   - "unknown" — no fact was available: the cgroup could not be read, or it
//     read back empty/blank. "Nothing was read" is not the same as "not managed
//     by that unit", so it must not be downgraded to "unmanaged".
//
// When no daemon pid is known, this fact is not reported at all instead of
// being set to unknown (see DaemonStatusFacts in process_manager.go).
type DaemonManagedBy string

const (
	DaemonManagedByUnmanaged DaemonManagedBy = "unmanaged"
	DaemonManagedByUnknown   DaemonManagedBy = "unknown"
)

// SystemdManagedBy reports the supervision fact for a system unit.
func SystemdManagedBy(unit string) DaemonManagedBy {
	return DaemonManagedBy("systemd:" + unit)
}

// systemSlicePrefix is the system manager's cgroup subtree, identical in cgroup
// v1 and v2.
const systemSlicePrefix = "/system.slice/"

// IsSystemUnitCgroupPath is true when path is the unit's own cgroup or a nested
// cgroup inside it. The comparison is on the full path (not the basename) with a
// trailing-slash guard so `herdsman.service.dev` is not mistaken for the unit
// itself.
func IsSystemUnitCgroupPath(path, unit string) bool {
	unitPath := systemSlicePrefix + unit
	return path == unitPath || strings.HasPrefix(path, unitPath+"/")
}

// ManagedByFromCgroup maps the content of `/proc/<pid>/cgroup` to a supervision
// fact. Pure, so it can be exercised with fabricated cgroup text. Handles both
// layouts: cgroup v2 `0::/system.slice/herdsman.service` and cgroup v1
// `1:name=systemd:/system.slice/herdsman.service`.
func ManagedByFromCgroup(cgroup string, unit string) DaemonManagedBy {
	if unit == "" {
		unit = HerdsmanSystemdUnit
	}
	sawContent := false
	for _, line := range strings.Split(cgroup, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		sawContent = true
		path := trimmed[strings.LastIndex(trimmed, ":")+1:]
		// The path must be the unit's own cgroup *and* live in the system slice.
		// The slice check is not cosmetic: a user-session unit of the same name
		// (`/user.slice/user-1000.slice/user@1000.service/app.slice/herdsman.service`)
		// ends in the very same basename, so a basename-only match reported it as
		// `systemd:herdsman.service` — the exact same string as the system unit —
		// and an operator following that would `systemctl restart` the wrong unit.
		if IsSystemUnitCgroupPath(path, unit) {
			return SystemdManagedBy(unit)
		}
	}
	// Blank/empty content means the read produced no fact at all, which is
	// unknown, not "not managed by that unit".
	if sawContent {
		return DaemonManagedByUnmanaged
	}
	return DaemonManagedByUnknown
}

// ReadProcessCgroup reads `/proc/<pid>/cgroup`; the second result is false when
// procfs is absent or unreadable (the TS `undefined`).
func ReadProcessCgroup(pid int) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// SystemctlRunner is an injectable command runner so tests can supply fake
// `systemctl` behaviour. It returns the raw stdout; ok is false when no fact is
// available (the TS `undefined`).
type SystemctlRunner func(command string, args []string, timeout time.Duration) (string, bool)

// defaultSystemctlRunner is a bounded run that never panics. On spawn error, a
// non-zero exit (including a timeout kill) or a missing stdout it reports "no
// fact available".
//
// The timeout is a hard bound: the child is SIGKILLed, because SIGTERM may
// simply be ignored, and an ignoring child would keep this call parked for its
// whole lifetime, so `daemon status` would hang behind it. SIGKILL cannot be
// ignored, and `systemctl show` is a read-only client with nothing to clean up,
// so killing it hard is safe.
//
// Diagnostic path only: this blocks the caller for up to timeout.
func defaultSystemctlRunner(command string, args []string, timeout time.Duration) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = timeout
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// SystemdRestartCountOptions mirrors the TS options object.
type SystemdRestartCountOptions struct {
	Command   string
	Runner    SystemctlRunner
	Timeout   time.Duration
	Unit      string
	HasUnit   bool
	HasRunner bool
}

// ReadSystemdRestartCount performs the best-effort
// `systemctl show <unit> -p NRestarts --value`. It reports ok=false on any
// failure (command missing, permission denied, timeout, non-zero exit,
// unparsable output) and never panics, so the caller can omit the field.
//
// How to read the number: it is systemd's `NRestarts` for the *unit*, and
// `systemctl reset-failed`, a `systemctl stop` + `start` pair, or a unit that is
// not currently loaded all reset it to 0. So `0` means "systemd has not
// restarted this unit since it last loaded it", never "this daemon has never
// crashed".
func ReadSystemdRestartCount(options *SystemdRestartCountOptions) (int, bool) {
	var opts SystemdRestartCountOptions
	if options != nil {
		opts = *options
	}
	unit := opts.Unit
	if unit == "" {
		unit = HerdsmanSystemdUnit
	}
	command := opts.Command
	if command == "" {
		command = "systemctl"
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = SupervisionProbeTimeout
	}
	runner := opts.Runner
	if runner == nil {
		runner = defaultSystemctlRunner
	}

	var stdout string
	var ok bool
	func() {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		stdout, ok = runner(command, []string{"show", unit, "-p", "NRestarts", "--value"}, timeout)
	}()
	if !ok {
		return 0, false
	}

	// Number(stdout.trim()) + Number.isInteger(value) && value >= 0. JS `Number`
	// accepts "" and whitespace as 0, so reproduce that exactly.
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return 0, true
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, false
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, false
	}
	if parsed < 0 || value != float64(parsed) {
		return 0, false
	}
	return int(parsed), true
}
