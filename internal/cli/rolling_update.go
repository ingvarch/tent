package cli

import (
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
)

func newRollingUpdateCommand(opts *globalOptions) *cobra.Command {
	cmd := groupCommand("rolling-update", "Replace the outdated nodes of a cluster")
	cmd.AddCommand(newRollingUpdateClusterCommand(opts))
	return cmd
}

func newRollingUpdateClusterCommand(opts *globalOptions) *cobra.Command {
	var yes, force, exitCode, allowSingle bool
	var nodeGroups []string
	cmd := &cobra.Command{
		Use:   "cluster [NAME]",
		Short: "Replace the outdated nodes of a cluster",
		Long: "Replace the outdated nodes of the cluster named by NAME or --name. A node is outdated when its spec " +
			"hash differs from its node group's, or --force is given and its group rolls. A client group rolls by " +
			"its maxSurge and maxUnavailable: tent creates new nodes first, marks each old node ineligible, drains " +
			"it within the group's drainTimeout, deletes its machine and purges its node from Nomad once Nomad " +
			"lists it down. tent cannot roll server groups yet: when the server or combined group has anything to " +
			"do, which --force always gives it, select the client groups with --nodegroups. Run tent update " +
			"cluster first after a change of the specs. Without --yes it prints the outdated nodes of each group " +
			"and the next step; with --exit-code it exits with 2 while a next step is due. With --yes it takes " +
			"the cluster's lock, prints the plan, prints each step on stderr as it goes and then what it did; " +
			"with -o json or -o yaml it prints the plan it applied. A run that stops is finished by running the " +
			"command again; a run with --force is finished by running it again with --force, which also replaces " +
			"the nodes that the first run made. A development build of tent needs TENT_NODE_URL and " +
			"TENT_NODE_SHA256 to find the tent-node that its nodes run. tent reads the cloud's credentials from " +
			"the environment: VULTR_API_KEY for Vultr.",
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
			roll := app.RollOptions{Apply: yes, NodeGroups: nodeGroups, Force: force}
			if !yes {
				return previewRoll(cmd, opts, svc, name, roll, exitCode)
			}
			return applyRoll(cmd, opts, svc, name, roll)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "roll the nodes; without it, print the plan")
	cmd.Flags().StringSliceVar(&nodeGroups, "nodegroups", nil, "`NAMES` of the node groups to roll; repeat or "+
		"separate with commas; every node group of the specs by default")
	cmd.Flags().BoolVar(&exitCode, "exit-code", false, "exit with code 2 when a roll is due; only without --yes")
	cmd.Flags().BoolVar(&force, "force", false, "replace every node of the selected groups, whatever its spec hash")
	addAllowSingleServer(cmd, &allowSingle)
	return cmd
}

// refused reports whether err is a refusal of the roll's decisions, which comes with the plan of the groups and no next
// step, and not a failed check, which comes with no plan.
func refused(plan app.RollPlan, err error) bool {
	return err != nil && len(plan.Groups) > 0 && plan.Next == nil
}

// previewRoll prints the plan of the cluster's rolling update and, when it has a next step, how to roll the nodes. A
// refusal prints the plan too, before its error. With exitCode, a plan with a next step ends the command with
// errPlanHasChanges.
func previewRoll(cmd *cobra.Command, opts *globalOptions, svc *app.Service, cluster string,
	roll app.RollOptions, exitCode bool,
) error {
	plan, err := svc.RollingUpdate(cmd.Context(), cluster, roll)
	err = report(err == nil || refused(plan, err), err, func() error {
		if err := printObject(cmd.OutOrStdout(), opts.output, plan, plan.WriteText); err != nil {
			return err
		}
		if plan.Next != nil {
			// A hint that fails to print changes nothing.
			_, _ = io.WriteString(cmd.ErrOrStderr(), "run with --yes to roll the nodes\n")
		}
		return nil
	})
	if err == nil && exitCode && plan.Next != nil {
		return errPlanHasChanges
	}
	return err
}

// applyRoll rolls the cluster's nodes and prints each step on stderr as it happens. With -o table it prints the plan
// made under the cluster's lock and then a line that sums up what it did, or only that there was nothing to roll; with
// another format, the plan it applied. A refusal prints its plan before the error, and a roll that finished but lost
// its lock before the release prints what it did; after any other error it prints nothing more.
func applyRoll(cmd *cobra.Command, opts *globalOptions, svc *app.Service, cluster string, roll app.RollOptions,
) error {
	w := cmd.OutOrStdout()
	svc.OnProgress = printProgress(cmd.ErrOrStderr(), opts.output)
	if opts.output == outputTable {
		svc.OnRollPlan = func(p app.RollPlan) error { return p.WriteText(w) }
	}
	plan, err := svc.RollingUpdate(cmd.Context(), cluster, roll)
	return report(err == nil || app.Saved(err) || refused(plan, err), err, func() error {
		switch {
		case opts.output != outputTable || refused(plan, err):
			return printObject(w, opts.output, plan, plan.WriteText)
		case plan.Next == nil:
			return writeClusterLine(w, cluster, "has nothing to roll")
		}
		return writeApplied(w, plan)
	})
}
