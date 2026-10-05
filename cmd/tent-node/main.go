// Command tent-node is the node agent: it turns the machine it runs on into a Nomad agent of its node group.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/buildinfo"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/env"
)

// Exit codes of tent-node.
const (
	exitError = 1 // a command failed
	exitUsage = 2 // the command line is wrong, as the flag package reports it
)

// main runs tent-node. SIGTERM, which systemd sends to stop a unit, and Ctrl-C cancel the command's context.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, machineDeps())
	stop()
	os.Exit(code)
}

// deps are the parts of the machine that the commands use, so that tests can give them fakes. version uses none.
type deps struct {
	host        func(log *slog.Logger) (*nodeup.Host, error)       // the machine, which logs to log
	environment func(p v1alpha1.Provider) (env.Environment, error) // the metadata service of the cloud p
	executable  func() (string, error)                             // the path of the running tent-node
	lock        func(ctx context.Context) (func(), error)          // the lock between up and refresh-join
}

// machineDeps returns the deps of the machine that tent-node runs on.
func machineDeps() deps {
	return deps{host: nodeup.Local, environment: environment, executable: os.Executable, lock: nodeup.Lock}
}

// command is a tent-node command.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) error
}

// commands are the tent-node commands, in the order the usage lists them.
var commands = []command{
	{"install", "Install and start the systemd units that run tent-node", install},
	{"up", "Set the machine up as a Nomad agent of its node group", up},
	{"refresh-join", "Refresh the Nomad servers that the node joins", refreshJoin},
	{"version", "Print the version of tent-node", version},
}

// usageError is a wrong command line: tent-node exits with exitUsage.
type usageError string

func (e usageError) Error() string { return string(e) }

// run runs tent-node with args and returns the process exit code: 0 on success, exitError when the command fails and
// exitUsage for a wrong command line.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	fs := flag.NewFlagSet("tent-node", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeUsage(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage // the flag package has written the error and the usage
	}
	if fs.NArg() == 0 {
		writeUsage(stderr)
		return exitUsage
	}
	name := fs.Arg(0)
	for _, c := range commands {
		if c.name == name {
			return finish(stderr, c.run(ctx, fs.Args()[1:], stdout, stderr, d))
		}
	}
	return finish(stderr, usageError(fmt.Sprintf("unknown command %q; run tent-node -h for the commands", name)))
}

// finish writes the error of a command to stderr, if there is one, and returns the exit code it calls for. A command
// asked for its usage returns flag.ErrHelp once it has written it.
func finish(stderr io.Writer, err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	// Nowhere to report a failed write to stderr; the exit code still signals the failure.
	_, _ = fmt.Fprintf(stderr, "Error: %s\n", err)
	if _, ok := errors.AsType[usageError](err); ok {
		return exitUsage
	}
	return exitError
}

// writeUsage writes how to run tent-node.
func writeUsage(w io.Writer) {
	_, _ = io.WriteString(w, "Usage: tent-node <command>\n\nCommands:\n")
	for _, c := range commands {
		_, _ = fmt.Fprintf(w, "  %-13s %s\n", c.name, c.summary)
	}
}

// version prints the version, the commit, the build date, the Go version and the platform of tent-node.
func version(_ context.Context, args []string, stdout, _ io.Writer, _ deps) error {
	if len(args) > 0 {
		return usageError("version takes no arguments")
	}
	if _, err := fmt.Fprintf(stdout, "tent-node %s\n", buildinfo.Get()); err != nil {
		return fmt.Errorf("writing version: %w", err)
	}
	return nil
}
