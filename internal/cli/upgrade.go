package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
)

func newUpgradeCommand(opts *globalOptions) *cobra.Command {
	cmd := groupCommand("upgrade", "Move a cluster to the Nomad version that its channel recommends")
	cmd.AddCommand(newUpgradeClusterCommand(opts))
	return cmd
}

func newUpgradeClusterCommand(opts *globalOptions) *cobra.Command {
	var yes, allowSingle bool
	cmd := &cobra.Command{
		Use:   "cluster [NAME]",
		Short: "Move a cluster to the Nomad version that its channel recommends",
		Long: "Move the cluster named by NAME or --name to the Nomad version that its release channel " +
			"recommends. The cluster's version is spec.nomad.version of its spec, or else the version that tent " +
			"update cluster pinned. When the channel recommends a newer one, tent checks that HashiCorp's signed " +
			"checksums of that release verify, and prints the upgrade. With --yes it takes the cluster's lock and " +
			"writes the new version into spec.nomad.version; nothing changes in the cloud. Then run tent update " +
			"cluster, which applies the version and lists every node as outdated, and tent rolling-update " +
			"cluster, which replaces the servers first and the clients after them. tent moves a cluster by at " +
			"most one minor version at a time. To move to another version that the channel allows, set " +
			"spec.nomad.version with tent edit cluster. tent reads the release from releases.hashicorp.com and " +
			"needs no cloud credentials.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := opts.clusterArg(args)
			if err != nil {
				return err
			}
			svc, err := opts.service(cmd, v1alpha1.ValidateOptions{AllowSingleServer: allowSingle})
			if err != nil {
				return err
			}
			svc.OnWarning = warnOnce(cmd.ErrOrStderr())
			plan, err := svc.UpgradeCluster(cmd.Context(), name, yes)
			return report(err == nil || app.Saved(err), err, func() error {
				if err := printObject(cmd.OutOrStdout(), opts.output, plan, plan.WriteText); err != nil {
					return err
				}
				if plan.Due() && !yes {
					// A hint that fails to print changes nothing.
					_, _ = io.WriteString(cmd.ErrOrStderr(), "run with --yes to write spec.nomad.version\n")
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "write the new version; without it, print the plan")
	addAllowSingleServer(cmd, &allowSingle)
	return cmd
}
