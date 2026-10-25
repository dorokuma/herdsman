package shared

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxPeerPidReadsConnectorPid(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), "herdsman-peercred-"+itoa(os.Getpid())+".sock")
	_ = os.Remove(socketPath)
	defer os.Remove(socketPath)

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	type result struct {
		pid int
		ok  bool
	}
	got := make(chan result, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			got <- result{}
			return
		}
		defer conn.Close()
		pid, ok := LinuxPeerPid(conn)
		got <- result{pid: pid, ok: ok}
	}()

	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	res := <-got
	if !res.ok {
		t.Fatal("LinuxPeerPid returned !ok for a live connection")
	}
	if res.pid != os.Getpid() {
		t.Fatalf("peer pid = %d, want %d", res.pid, os.Getpid())
	}
}

func TestUnixSocketConnFdMatchesDescriptor(t *testing.T) {
	if !IsLinux() {
		t.Skip("SO_PEERCRED fd probing is Linux-only")
	}
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "fd.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan *net.UnixConn, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()

	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	server := <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	defer server.Close()

	fd, ok := UnixSocketConnFd(server)
	if !ok || fd < 0 {
		t.Fatalf("UnixSocketConnFd = (%d, %v), want a valid fd", fd, ok)
	}
	if pid, ok := LinuxPeerPidFromFd(fd); !ok || pid != os.Getpid() {
		t.Fatalf("LinuxPeerPidFromFd = (%d, %v), want (%d, true)", pid, ok, os.Getpid())
	}
}

func TestResolvePython3PathUsesTrustedAbsoluteCandidate(t *testing.T) {
	resolved, ok := ResolvePython3Path()
	if !ok {
		t.Skip("no trusted distro python3 on this host")
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("resolved path %q is not absolute", resolved)
	}
	matched := false
	for _, candidate := range Python3Candidates {
		if candidate == resolved {
			matched = true
		}
		if !filepath.IsAbs(candidate) {
			t.Fatalf("candidate %q is not absolute", candidate)
		}
	}
	if !matched {
		t.Fatalf("resolved %q not in Python3Candidates", resolved)
	}
}

func TestPeerBoundToPanePid(t *testing.T) {
	self := os.Getpid()
	if !PeerBoundToPanePid(self, self) {
		t.Fatal("a process must be bound to its own pane pid")
	}
	if PeerBoundToPanePid(self, 2_147_483_647) {
		t.Fatal("unrelated pane pid must not match")
	}
	if PeerBoundToPanePid(1, self) {
		t.Fatal("peer pid 1 must be rejected")
	}
	if ppid, ok := ParentPid(self); ok && ppid > 1 {
		if !PeerBoundToPanePid(self, ppid) {
			t.Fatalf("peer pid %d must bind to its parent pane pid %d", self, ppid)
		}
	}
}

func TestPeerBoundToPaneCwd(t *testing.T) {
	if !IsLinux() {
		t.Skip("/proc cwd probing is Linux-only")
	}
	self := os.Getpid()
	cwd, err := os.Readlink("/proc/" + itoa(self) + "/cwd")
	if err != nil {
		t.Fatalf("readlink cwd: %v", err)
	}
	if !PeerBoundToPaneCwd(self, cwd) {
		t.Fatalf("peer %d must bind to its own cwd %q", self, cwd)
	}
	if workdir, err := os.Getwd(); err == nil {
		if !PeerBoundToPaneCwd(self, workdir) {
			t.Fatalf("peer %d must bind to its process cwd %q", self, workdir)
		}
	}
	if PeerBoundToPaneCwd(self, "/tmp/herdsman-not-this-pane") {
		t.Fatal("unrelated pane cwd must not match")
	}
	if PeerBoundToPaneCwd(1, cwd) {
		t.Fatal("peer pid 1 must be rejected")
	}
}

func TestDecodePeerCredRequiresTwelveBytes(t *testing.T) {
	if _, err := decodePeerCred(make([]byte, 8)); err == nil {
		t.Fatal("expected an error for a short SO_PEERCRED payload")
	}
	raw := make([]byte, 12)
	raw[0] = 42
	cred, err := decodePeerCred(raw)
	if err != nil {
		t.Fatalf("decodePeerCred: %v", err)
	}
	if cred.Pid != 42 {
		t.Fatalf("pid = %d, want 42", cred.Pid)
	}
}

// itoa avoids importing strconv in the test file for one-off pid formatting.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return string(digits[i:])
}
