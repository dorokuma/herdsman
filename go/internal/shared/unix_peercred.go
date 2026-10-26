package shared

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// SO_PEERCRED for Linux (include/uapi/asm-generic/socket.h).
const (
	solSocket   = 1
	soPeerCred  = 17
	maxAncestor = 32
	maxCwdDepth = 4
)

// Python3Candidates mirrors PYTHON3_CANDIDATES: distro python3 only, never a
// PATH lookup (a fake `python3` earlier on PATH must not win).
var Python3Candidates = []string{"/usr/bin/python3", "/bin/python3"}

// IsLinux reports whether peer-credential probes apply (TS: process.platform).
func IsLinux() bool { return runtime.GOOS == "linux" }

// UnixSocketConnFd returns the OS file descriptor of a Unix-domain connection.
// The TS helper pokes at Node's internal `_handle.fd`; on Go the *net.UnixConn
// reports it directly, so this is the faithful equivalent.
func UnixSocketConnFd(conn *net.UnixConn) (int, bool) {
	if conn == nil {
		return 0, false
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, false
	}
	fd := -1
	if err := raw.Control(func(c uintptr) { fd = int(c) }); err != nil {
		return 0, false
	}
	if fd < 0 {
		return 0, false
	}
	return fd, true
}

// LinuxPeerPid reads the connecting process pid from a Unix-domain socket via
// SO_PEERCRED. Same-UID threat model: this binds a connection to a process,
// not to a user.
func LinuxPeerPid(conn *net.UnixConn) (int, bool) {
	if !IsLinux() {
		return 0, false
	}
	fd, ok := UnixSocketConnFd(conn)
	if !ok {
		return 0, false
	}
	return LinuxPeerPidFromFd(fd)
}

// LinuxPeerPidFromFd issues SO_PEERCRED on an existing descriptor.
//
// The TS implementation shells out to python3 (`socket.fromfd` +
// `getsockopt(SOL_SOCKET, SO_PEERCRED)`); Go reaches the same kernel data
// structure directly, which is the behaviour-equivalent port. A pid <= 1 is
// rejected exactly like the TS `pid > 1` guard.
func LinuxPeerPidFromFd(fd int) (int, bool) {
	if !IsLinux() {
		return 0, false
	}
	creds, err := syscallPeerCred(fd)
	if err != nil {
		return 0, false
	}
	if creds.Pid <= 1 {
		return 0, false
	}
	return int(creds.Pid), true
}

// ResolvePython3Path mirrors resolvePython3Path: it prefers a trusted distro
// interpreter and never consults PATH. Retained for parity with the TS surface;
// the Go implementation does not need it for SO_PEERCRED.
func ResolvePython3Path() (string, bool) {
	for _, candidate := range Python3Candidates {
		if isTrustedPython3(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// PeerBoundToPanePid reports whether peerPid is the pane pid itself or one of
// its descendants (TS: isAncestorPid(panePid, peerPid) walks parents upward).
func PeerBoundToPanePid(peerPid, panePid int) bool {
	if peerPid <= 1 || panePid <= 1 {
		return false
	}
	if peerPid == panePid {
		return true
	}
	return isAncestorPid(panePid, peerPid)
}

// PeerBoundToPaneCwd reports whether peerPid, or one of its ancestors within
// maxCwdDepth levels, has a /proc cwd equal to paneCwd.
func PeerBoundToPaneCwd(peerPid int, paneCwd string) bool {
	if peerPid <= 1 {
		return false
	}
	if paneCwd == "" {
		return false
	}
	current := peerPid
	for step := 0; step <= maxCwdDepth && current > 1; step++ {
		currentCwd, ok := processCwd(current)
		if !ok {
			break
		}
		if sameCwd(currentCwd, paneCwd) {
			return true
		}
		parent, ok := ParentPid(current)
		if !ok || parent <= 1 || parent == current {
			break
		}
		current = parent
	}
	return false
}

// ParentPid returns PPid from /proc/<pid>/status.
func ParentPid(pid int) (int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "PPid:"))
		if len(fields) != 1 {
			return 0, false
		}
		ppid, err := strconv.Atoi(fields[0])
		if err != nil || ppid < 0 {
			return 0, false
		}
		return ppid, true
	}
	return 0, false
}

func processCwd(pid int) (string, bool) {
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return "", false
	}
	return cwd, true
}

func sameCwd(left, right string) bool {
	return normalizeCwd(left) == normalizeCwd(right)
}

// normalizeCwd mirrors the TS helper: realpath when possible, otherwise trim a
// single trailing slash for non-root paths.
func normalizeCwd(value string) string {
	if resolved, err := filepath.EvalSymlinks(value); err == nil {
		return resolved
	}
	if len(value) > 1 && strings.HasSuffix(value, "/") {
		return strings.TrimSuffix(value, "/")
	}
	return value
}

func isTrustedPython3(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return strings.HasPrefix(resolved, "/usr/bin/python") || strings.HasPrefix(resolved, "/bin/python")
}

func isAncestorPid(ancestor, pid int) bool {
	current := pid
	for step := 0; step < maxAncestor; step++ {
		parent, ok := ParentPid(current)
		if !ok || parent <= 1 {
			return false
		}
		if parent == ancestor {
			return true
		}
		if parent == current {
			return false
		}
		current = parent
	}
	return false
}

// errPeerCredUnavailable marks SO_PEERCRED read failures.
var errPeerCredUnavailable = errors.New("peer credentials unavailable")

// peerCred holds the raw `struct ucred { pid_t pid; uid_t uid; gid_t gid; }`.
type peerCred struct {
	Pid int32
	Uid uint32
	Gid uint32
}

// decodePeerCred parses the fixed 3x int32 SO_PEERCRED payload.
func decodePeerCred(raw []byte) (peerCred, error) {
	if len(raw) != 12 {
		return peerCred{}, fmt.Errorf("%w: got %d bytes", errPeerCredUnavailable, len(raw))
	}
	return peerCred{
		Pid: int32(binary.NativeEndian.Uint32(raw[0:4])),
		Uid: binary.NativeEndian.Uint32(raw[4:8]),
		Gid: binary.NativeEndian.Uint32(raw[8:12]),
	}, nil
}
