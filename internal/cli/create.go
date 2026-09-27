package cli

import (
	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/internal/app"
)

func newCreateCommand(opts *globalOptions) *cobra.Command {
	var (
		file        string
		allowSingle bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a cluster or node groups in the state store",
		Long: "Create a cluster or node groups in the state store from a spec file, or generate a cluster with " +
			"create cluster. A spec file holds a Cluster and its node groups, or node groups to add to an existing " +
			"cluster. Nothing is created in the cloud.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("filename") {
				return cmd.Help()
			}
			return writeSpecFile(cmd, opts, file, allowSingle, (*app.Service).Create)
		},
	}
	cmd.Flags().StringVarP(&file, "filename", "f", "", "spec `FILE`, or - for standard input")
	addAllowSingleServer(cmd, &allowSingle)
	cmd.AddCommand(newCreateClusterCommand(opts))
	return cmd
}
