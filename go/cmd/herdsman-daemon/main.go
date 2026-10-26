// Command herdsman-daemon is the bare daemon entrypoint, ported from
// src/cli/herdsman-daemon.ts. It takes no arguments: every knob lives in the
// Herdsman home (config.yaml, .env, the runtime.* overrides), exactly like the
// TS entrypoint.
//
// The systemd unit (/etc/systemd/system/herdsman.service) points ExecStart at
// this process, and the process basename is one of the daemon entrypoint names
// daemon.DaemonEntrypointNames accepts, so `herdsman daemon status` can verify
// the pid it finds in the pid file really is a daemon.
package main

import (
	"fmt"
	"os"

	"github.com/dorokuma/herdsman/go/internal/daemon"
)

func main() {
	// The TS entrypoint throws before touching the daemon when it is handed an
	// argument, and the rejection handler prints the message and exits 1.
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "herdsman-daemon does not accept CLI arguments")
		os.Exit(1)
	}
	if err := daemon.RunObservabilityDaemonService(nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// RunObservabilityDaemonService returns once startup has finished; the daemon
	// then lives on its own goroutines until a SIGINT/SIGTERM drives the graceful
	// shutdown, which exits the process itself. The TS entrypoint gets the same
	// behaviour from a promise that only settles when the process exits.
	select {}
}
