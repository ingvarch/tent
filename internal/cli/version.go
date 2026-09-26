package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/internal/buildinfo"
)

func newVersionCommand(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of tent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			return printObject(cmd.OutOrStdout(), opts.output, info, func(w io.Writer) error {
				if _, err := fmt.Fprintf(w, "tent %s\n", info); err != nil {
					return fmt.Errorf("writing version: %w", err)
				}
				return nil
			})
		},
	}
}
