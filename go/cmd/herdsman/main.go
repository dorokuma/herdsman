// Command herdsman is the Herdsman CLI, ported from src/cli/herdsman.ts.
//
// It is a thin shell around the cli package: it parses argv, resolves the
// runtime paths, and prints the rendered line on stdout. Errors go to stderr
// with exit code 1, exactly like the TS entrypoint's rejection handler.
package main

import (
	"fmt"
	"os"

	"github.com/dorokuma/herdsman/go/internal/cli"
	"github.com/dorokuma/herdsman/go/internal/config"
	"github.com/dorokuma/herdsman/go/internal/daemon"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, cli.FormatCliError(err))
		os.Exit(1)
	}
}

func run() error {
	environment := cli.Environment()
	command, err := cli.ParseCliArgs(os.Args[1:], environment)
	if err != nil {
		return err
	}
	runtime, err := cli.ResolveRuntimeForCommand(environment)
	if err != nil {
		return err
	}
	if command.Command == cli.CommandDaemon {
		return runDaemonCommand(command, runtime)
	}
	return cli.RunCliCommand(command, cli.RunCliDeps{
		Connect: func(socketPath string, onNotification func(method string, params any)) (cli.RpcClientLike, error) {
			return daemon.NewObservabilityRpcClient(daemon.ObservabilityRpcClientOptions{
				OnNotification: onNotification,
				SocketPath:     socketPath,
			})
		},
		Environment: environment,
		Output:      func(line string) { fmt.Fprintln(os.Stdout, line) },
		SocketPath:  runtime.Paths.SocketPath,
	})
}

// runDaemonCommand mirrors runDaemonCommand: JSON by default (the machine
// contract), `--text` for the human view.
func runDaemonCommand(command cli.CliCommand, runtime config.RuntimeResolution) error {
	view, err := cli.ReadDaemonStatus(runtime.Paths.PIDPath, runtime.Paths.SocketPath)
	if err != nil {
		return err
	}
	if command.Text {
		fmt.Fprintln(os.Stdout, cli.FormatDaemonStatus(view))
		return nil
	}
	encoded, err := cli.DaemonStatusJSON(view)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, encoded)
	return nil
}
