package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/statestore"
)

func newStateCommand(opts *globalOptions) *cobra.Command {
	cmd := groupCommand("state", "Manage the state store")
	cmd.AddCommand(newStateUnlockCommand(opts))
	return cmd
}

func newStateUnlockCommand(opts *globalOptions) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "unlock [NAME]",
		Short: "Remove the stale lock of a cluster",
		Long: "Remove the lock of the cluster named by NAME or --name when its holder expired or ended. With " +
			"--force it also removes the lock of a holder that looks alive, for when you know that tent is gone. " +
			"On a file:// store the OS holds the lock while its holder runs, so nothing removes that one: stop " +
			"the holder instead.",
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
			removed, err := svc.Unlock(cmd.Context(), name, force)
			if err != nil {
				return err
			}
			out := unlockOutput{Cluster: name, Removed: removed}
			return printObject(cmd.OutOrStdout(), opts.output, out, func(w io.Writer) error {
				msg := "cluster " + name + " is not locked\n"
				if removed != nil {
					msg = "removed the lock of cluster " + name + " held by " + removed.String() + "\n"
				}
				if _, err := io.WriteString(w, msg); err != nil {
					return fmt.Errorf("writing the result: %w", err)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove the lock even if its holder looks alive")
	return cmd
}

// unlockOutput is what state unlock prints with -o json and -o yaml: the lease it removed, or null.
type unlockOutput struct {
	Cluster string            `json:"cluster"`
	Removed *statestore.Lease `json:"removed"`
}
