package daemon

import (
	"path/filepath"
	"strings"
)

// isTestExecutable reports whether execPath is a Go test binary — the
// `<pkg>.test` (or `<pkg>.test.exe` on Windows) shape `go test` produces.
// spawnAgyWakeWaiterProcess can resolve `herdsman` to the daemon package's own
// test binary (the sibling lookup sees `daemon.test` next to
// `daemon.test.exe`, or a test-built `herdsman` in a scratch dir), and re-
// execing a test binary as `agy-hook WakeWaiter` re-runs the whole suite in a
// detached session — the fork bomb that OOMed the host. Shared by every
// waiter spawn site; a test that needs a real child injects Spawn.
func isTestExecutable(execPath string) bool {
	base := filepath.Base(execPath)
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe")
}
