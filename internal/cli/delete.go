package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
)

func newDeleteCommand(opts *globalOptions) *cobra.Command {
	cmd := groupCommand("delete", "Delete a cluster")
	cmd.AddCommand(newDeleteClusterCommand(opts))
	return cmd
}

func newDeleteClusterCommand(opts *globalOptions) *cobra.Command {
	var yes, force bool
	cmd := &cobra.Command{
		Use:   "cluster [NAME]",
		Short: "Delete the state of a cluster",
		Long: "Delete the specs and the rest of the state of the cluster named by NAME or --name from the state " +
			"store. In this version it deletes only the state, not the cloud resources. Without --yes it prints " +
			"what it would delete. It refuses to delete objects under the cluster that tent does not know, " +
			"unless --force is given.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := opts.clusterArg(args)
			if err != nil {
				return err
			}
			svc, err := opts.service(cmd, v1alpha1.ValidateOptions{})
			if err != nil {
				return err
			}
			paths, err := svc.DeleteState(cmd.Context(), name, yes, force)
			out := deleteOutput{Cluster: name, Applied: yes, Paths: paths}
			return report(paths != nil, err, func() error {
				return printObject(cmd.OutOrStdout(), opts.output, out, func(w io.Writer) error {
					return writeDeleted(w, paths, yes, force)
				})
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "delete; without it, print what would be deleted")
	cmd.Flags().BoolVar(&force, "force", false, "delete objects under the cluster that tent does not know, too")
	return cmd
}

// deleteOutput is what delete cluster prints with -o json and -o yaml. Applied tells the deletion from the preview.
type deleteOutput struct {
	Cluster string   `json:"cluster"`
	Applied bool     `json:"applied"`
	Paths   []string `json:"paths"`
}

// writeDeleted writes the paths deleted, or with deleted unset, the paths that would be deleted and how to delete
// them.
func writeDeleted(w io.Writer, paths []string, deleted, force bool) error {
	var b strings.Builder
	if deleted {
		for _, p := range paths {
			b.WriteString("deleted " + p + "\n")
		}
	} else {
		b.WriteString("would delete:\n")
		for _, p := range paths {
			b.WriteString("  " + p + "\n")
		}
		flags := "--yes"
		if force {
			flags += " --force"
		}
		b.WriteString("run with " + flags + " to delete them\n")
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing the paths: %w", err)
	}
	return nil
}
