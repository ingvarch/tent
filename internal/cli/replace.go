package cli

import (
	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/internal/app"
)

func newReplaceCommand(opts *globalOptions) *cobra.Command {
	var (
		file        string
		allowSingle bool
	)
	cmd := &cobra.Command{
		Use:   "replace -f FILE",
		Short: "Replace specs in the state store with those of a file",
		Long: "Replace the Cluster and node groups in the state store with those of a spec file, such as the one " +
			"get prints. Every object in the file must exist; create new ones with create -f. Nothing changes in " +
			"the cloud.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeSpecFile(cmd, opts, file, allowSingle, (*app.Service).Replace)
		},
	}
	cmd.Flags().StringVarP(&file, "filename", "f", "", "spec `FILE`, or - for standard input")
	addAllowSingleServer(cmd, &allowSingle)
	_ = cmd.MarkFlagRequired("filename") // fails only for a flag that does not exist
	return cmd
}
