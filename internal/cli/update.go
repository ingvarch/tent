package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
)

func newUpdateCommand(opts *globalOptions) *cobra.Command {
	cmd := groupCommand("update", "Bring a cluster's cloud objects to its specs")
	cmd.AddCommand(newUpdateClusterCommand(opts))
	return cmd
}

func newUpdateClusterCommand(opts *globalOptions) *cobra.Command {
	var yes, exitCode, allowSingle bool
	cmd := &cobra.Command{
		Use:   "cluster [NAME]",
		Short: "Bring a cluster's cloud objects to its specs",
		Long: "Bring the cloud objects of the cluster named by NAME or --name to its specs in the state store: its " +
			"network, firewalls and SSH keys, and its nodes, which are created or deleted until each node group " +
			"has its size. It also makes the cluster's missing CA, gossip key and ACL bootstrap secret in the " +
			"state store, and never replaces them. It starts Nomad on the nodes, bootstraps its ACL system and " +
			"waits for the servers to be healthy and the clients to register. Once a node has joined its cluster, tent " +
			"replaces its user data with a stub. A client that did not register within 31 minutes of its creation is " +
			"deleted and created again. An update that would delete a node that joined fails. A development build of " +
			"tent needs TENT_NODE_URL and TENT_NODE_SHA256 to find the tent-node that its nodes run. " +
			"Without --yes it prints the plan and changes nothing. With --yes it prints the plan, applies it, " +
			"prints each step on stderr as it goes, and then prints what it did; with -o json or -o yaml it " +
			"prints the plan it applied. tent reads the cloud's credentials from the environment: VULTR_API_KEY " +
			"for Vultr.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if yes && exitCode {
				return errors.New("--exit-code works only without --yes")
			}
			name, err := opts.clusterArg(args)
			if err != nil {
				return err
			}
			svc, err := opts.service(cmd, v1alpha1.ValidateOptions{AllowSingleServer: allowSingle})
			if err != nil {
				return err
			}
			svc.OnWarning = warnOnce(cmd.ErrOrStderr())
			switch {
			case !yes:
				return previewUpdate(cmd, opts, svc, name, exitCode)
			case opts.output == outputTable:
				return applyUpdateText(cmd, opts, svc, name)
			}
			plan, err := applyUpdate(cmd, opts, svc, name)
			return report(err == nil || app.Saved(err), err, func() error {
				return printObject(cmd.OutOrStdout(), opts.output, plan, nil)
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "apply the plan; without it, print the plan")
	cmd.Flags().BoolVar(&exitCode, "exit-code", false, "exit with code 2 when the plan has changes; only "+
		"without --yes")
	addAllowSingleServer(cmd, &allowSingle)
	return cmd
}

// previewUpdate prints the plan of the cluster's update and, when it has changes, how to apply them. With exitCode,
// a plan with changes ends the command with errPlanHasChanges.
func previewUpdate(cmd *cobra.Command, opts *globalOptions, svc *app.Service, cluster string, exitCode bool) error {
	plan, err := svc.Update(cmd.Context(), cluster, false)
	if err != nil {
		return err
	}
	if err := printObject(cmd.OutOrStdout(), opts.output, plan, plan.WriteText); err != nil {
		return err
	}
	if !plan.HasChanges() {
		return nil
	}
	// A hint that fails to print changes nothing.
	_, _ = io.WriteString(cmd.ErrOrStderr(), "run with --yes to apply the changes\n")
	if exitCode {
		return errPlanHasChanges
	}
	return nil
}

// applyUpdate applies the update of the cluster, prints each step on stderr as it happens, and returns the plan it
// applied.
func applyUpdate(cmd *cobra.Command, opts *globalOptions, svc *app.Service, cluster string) (app.UpdatePlan, error) {
	svc.OnProgress = printProgress(cmd.ErrOrStderr(), opts.output)
	return svc.Update(cmd.Context(), cluster, true)
}

// applyUpdateText updates the cluster as update cluster --yes does with -o table: it prints the plan made under the
// cluster's lock, applies it with each step on stderr, and prints a line that sums up what it did; or only that the
// cluster is up to date.
func applyUpdateText(cmd *cobra.Command, opts *globalOptions, svc *app.Service, cluster string) error {
	w := cmd.OutOrStdout()
	svc.OnUpdatePlan = func(p app.UpdatePlan) error { return p.WriteText(w) }
	plan, err := applyUpdate(cmd, opts, svc, cluster)
	return report(err == nil || app.Saved(err), err, func() error {
		if !plan.HasChanges() {
			return writeClusterLine(w, cluster, "is up to date")
		}
		return writeApplied(w, plan)
	})
}

// writeClusterLine writes a line that says of the cluster that it is up to date or has nothing to roll.
func writeClusterLine(w io.Writer, cluster, state string) error {
	if _, err := fmt.Fprintf(w, "cluster %s %s\n", cluster, state); err != nil {
		return fmt.Errorf("writing the result: %w", err)
	}
	return nil
}

// writeApplied writes a blank line, then the line of the applied plan p that sums up what it did.
func writeApplied(w io.Writer, p interface{ WriteApplied(io.Writer) error }) error {
	if _, err := io.WriteString(w, "\n"); err != nil {
		return fmt.Errorf("writing the result: %w", err)
	}
	return p.WriteApplied(w)
}
