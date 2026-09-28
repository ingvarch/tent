package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
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
		Short: "Delete a cluster's cloud objects and its state",
		Long: "Delete the cloud objects of the cluster named by NAME or --name, then its state: first its nodes, " +
			"then its network, firewalls and SSH keys, and last its specs and the rest of its state in the state " +
			"store. tent finds the cloud objects by the markers it put on them, on the cloud that the stored " +
			"cluster.yaml names; for a cluster without cluster.yaml, or on a cloud that tent cannot manage yet, it " +
			"deletes the state only. Without --yes it prints what it would delete. With --yes it prints that, " +
			"deletes, prints each step on stderr as it goes, and then prints what it deleted; with -o json or -o " +
			"yaml it prints the plan it applied. It refuses to delete objects under the cluster in the state store " +
			"that tent does not know, unless --force is given. tent reads the cloud's credentials from the " +
			"environment: VULTR_API_KEY for Vultr.",
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
			switch {
			case !yes:
				return previewDelete(cmd, opts, svc, name, force)
			case opts.output != outputTable:
				return applyDeleteRecord(cmd, opts, svc, name, force)
			}
			return applyDeleteText(cmd, opts, svc, name, force)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "delete; without it, print what would be deleted")
	cmd.Flags().BoolVar(&force, "force", false, "delete objects under the cluster in the state store that tent "+
		"does not know, too")
	return cmd
}

// previewDelete prints the plan of the delete of the cluster, warns when it deletes the state alone, and says how to
// delete.
func previewDelete(cmd *cobra.Command, opts *globalOptions, svc *app.Service, cluster string, force bool) error {
	plan, err := svc.DeleteCluster(cmd.Context(), cluster, false, force)
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	warnStateOnly(stderr, cluster, plan)
	if err := printObject(cmd.OutOrStdout(), opts.output, plan, plan.WriteText); err != nil {
		return err
	}
	flags := "--yes"
	if force {
		flags += " --force"
	}
	// A hint that fails to print changes nothing.
	_, _ = io.WriteString(stderr, "run with "+flags+" to delete them\n")
	return nil
}

// applyDelete deletes the cluster, prints each step on stderr as it happens, and returns the plan it applied.
func applyDelete(cmd *cobra.Command, opts *globalOptions, svc *app.Service, cluster string, force bool) (
	app.DeletePlan, error,
) {
	svc.OnProgress = printProgress(cmd.ErrOrStderr(), opts.output)
	return svc.DeleteCluster(cmd.Context(), cluster, true, force)
}

// applyDeleteText deletes the cluster as delete cluster --yes does with -o table: it prints the plan made under the
// cluster's lock, warns when it deletes the state alone, deletes with each step on stderr, and prints a line that
// sums up what it deleted.
func applyDeleteText(cmd *cobra.Command, opts *globalOptions, svc *app.Service, cluster string, force bool) error {
	w := cmd.OutOrStdout()
	svc.OnDeletePlan = func(p app.DeletePlan) error {
		warnStateOnly(cmd.ErrOrStderr(), cluster, p)
		return p.WriteText(w)
	}
	plan, err := applyDelete(cmd, opts, svc, cluster, force)
	return report(err == nil || app.Saved(err), err, func() error { return writeApplied(w, plan) })
}

// applyDeleteRecord deletes the cluster as delete cluster --yes does with -o json or -o yaml: it prints each step on
// stderr, and then the plan it applied.
func applyDeleteRecord(cmd *cobra.Command, opts *globalOptions, svc *app.Service, cluster string, force bool) error {
	plan, err := applyDelete(cmd, opts, svc, cluster, force)
	did := err == nil || app.Saved(err)
	if did {
		warnStateOnly(cmd.ErrOrStderr(), cluster, plan)
	}
	return report(did, err, func() error { return printObject(cmd.OutOrStdout(), opts.output, plan, nil) })
}

// warnStateOnly warns on w when the delete plan p of the cluster deletes its state alone: tent cannot tell the
// cluster's cloud, or made no cloud objects on it.
func warnStateOnly(w io.Writer, cluster string, p app.DeletePlan) {
	var why string
	switch {
	case p.CloudUnknown:
		why = "cluster " + cluster + " has no cluster.yaml, so tent cannot tell its cloud"
	case p.Unsupported != "":
		why = "tent cannot manage clusters on " + string(p.Unsupported) + " yet, so it made no cloud objects for " +
			"cluster " + cluster
	default:
		return
	}
	// A warning that fails to print changes nothing.
	_, _ = io.WriteString(w, "WARNING: "+why+"; deleting its state only\n")
}
