// Package cli implements the tent command line, a thin layer over the use cases.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// Streams are where a command reads its input and writes its output.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Execute runs tent with args and returns the process exit code. The first Ctrl-C or SIGTERM cancels the command's
// context; a second ends tent at once with exit code 130.
func Execute(ctx context.Context, args []string, s Streams) int {
	sigs, stop := notifyStopSignals()
	defer stop()
	return executeWithSignals(ctx, args, s, sigs, exitProcess)
}

func execute(ctx context.Context, cmd *cobra.Command, args []string, stderr io.Writer) int {
	if args == nil {
		args = []string{} // cobra would otherwise read os.Args
	}
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		writeError(stderr, err)
		return 1
	}
	return 0
}

// writeError writes "Error: " and the message of err. Lines after the first are indented, so that each error of a
// joined error, and each problem of an invalid spec, is on a line of its own.
func writeError(w io.Writer, err error) {
	msg := err.Error()
	if problems, ok := errors.AsType[v1alpha1.Errors](err); ok && problems.Error() == msg {
		msg = "invalid spec:\n" + msg
	}
	lines := strings.Split(strings.TrimSuffix(msg, "\n"), "\n")
	for i, line := range lines[1:] {
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			lines[i+1] = "  " + line
		}
	}
	// Nowhere to report a failed write to stderr; the exit code still signals the failure.
	_, _ = io.WriteString(w, "Error: "+strings.Join(lines, "\n")+"\n")
}

// newRootCommand returns the tent command. Its subcommands receive opts, resolved before they run.
func newRootCommand(s Streams, opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "tent",
		Short:         "Provision and operate HashiCorp Nomad clusters",
		SilenceUsage:  true,
		SilenceErrors: true,
		// cobra runs only the nearest PersistentPreRunE, so subcommands must not set their own.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cobraCommand(cmd) {
				return nil // needs no settings, so a broken config file does not break it
			}
			return opts.resolve(cmd)
		},
	}
	cmd.SetIn(s.In)
	cmd.SetOut(s.Out)
	cmd.SetErr(s.Err)
	opts.addFlags(cmd)
	cmd.AddCommand(
		newVersionCommand(opts), newCreateCommand(opts), newGetCommand(opts), newReplaceCommand(opts),
		newDeleteCommand(opts), newStateCommand(opts), newEditCommand(opts),
	)
	return cmd
}

// groupCommand returns a command that holds subcommands only. Alone it prints its help; with an argument it fails,
// since the argument is a subcommand it does not have.
func groupCommand(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
}

// nameOrSubcommand takes the one NAME of get, which also has subcommands. With more arguments, a first one close to a
// subcommand's name is a mistyped subcommand; otherwise they name several clusters.
func nameOrSubcommand(cmd *cobra.Command, args []string) error {
	switch {
	case len(args) < 2:
		return nil
	case len(cmd.SuggestionsFor(args[0])) > 0:
		return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
	}
	return errors.New("get takes one NAME; list several clusters with get clusters " + strings.Join(args, " "))
}

// cobraCommand reports whether cmd is one that cobra adds: help, completion and its scripts, or a shell's request
// for completions.
func cobraCommand(cmd *cobra.Command) bool {
	for cmd.HasParent() && cmd.Parent().HasParent() {
		cmd = cmd.Parent()
	}
	switch cmd.Name() {
	case "help", "completion", cobra.ShellCompRequestCmd: // __completeNoDesc is an alias of __complete
		return true
	}
	return false
}
