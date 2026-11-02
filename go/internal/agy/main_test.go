package agy_test

import (
	"os"
	"testing"
)

// TestMain is the outermost fork-bomb fuse: even if a spawn site is ever
// missed, a test binary re-exec'd as `agy-hook WakeWaiter` exits before the
// suite runs again, and no host Herdr/Herdsman environment leaks into the
// package tests (this suite ran under HERDR_ENV=1 on the live pane when the
// storm happened).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "agy-hook" {
		os.Exit(0)
	}
	for _, key := range []string{
		"HERDR_ENV",
		"HERDR_PANE_ID",
		"HERDR_WORKSPACE_ID",
		"HERDR_TAB_ID",
		"HERDR_SOCKET_PATH",
		"HERDSMAN_HOME",
		"HERDSMAN_AUTO_ON",
		"HERDSMAN_AGY_SESSION_ID",
		"HERDSMAN_AGY_STATE_DIR",
		"HERDSMAN_AGY_WAITER_EPOCH",
	} {
		os.Unsetenv(key)
	}
	os.Exit(m.Run())
}
