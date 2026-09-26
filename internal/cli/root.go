// Package cli implements the tent command line, a thin layer over the use cases.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// Streams are where a command writes its output.
type Streams struct {
	Out io.Writer
	Err io.Writer
}

// Output formats accepted by -o.
const (
	outputTable = "table"
	outputYAML  = "yaml"
	outputJSON  = "json"
)

// globalOptions hold the persistent flags shared by every command.
type globalOptions struct {
	output string
}

// Execute runs tent with args and returns the process exit code.
func Execute(args []string, s Streams) int {
	cmd := newRootCommand(s)
	if args == nil {
		args = []string{} // cobra would otherwise read os.Args
	}
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		// Nowhere to report a failed write to stderr; the exit code still signals the failure.
		_, _ = fmt.Fprintf(s.Err, "Error: %v\n", err)
		return 1
	}
	return 0
}

func newRootCommand(s Streams) *cobra.Command {
	opts := &globalOptions{}
	cmd := &cobra.Command{
		Use:           "tent",
		Short:         "Provision and operate HashiCorp Nomad clusters",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return validateOutput(opts.output)
		},
	}
	cmd.SetOut(s.Out)
	cmd.SetErr(s.Err)
	cmd.PersistentFlags().StringVarP(&opts.output, "output", "o", outputTable, "output format: table, yaml or json")
	cmd.AddCommand(newVersionCommand(opts))
	return cmd
}

func validateOutput(format string) error {
	switch format {
	case outputTable, outputYAML, outputJSON:
		return nil
	}
	return invalidOutputError(format)
}

func invalidOutputError(format string) error {
	return fmt.Errorf("invalid output format %q: want table, yaml or json", format)
}
