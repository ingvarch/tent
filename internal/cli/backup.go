package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
)

// restoredNotice is what a restore that ended tells on stderr.
const restoredNotice = "Nomad now holds the jobs, nodes, ACL tokens, variables and keyring of the backup: run tent " +
	"export nomad again.\n" +
	"Nomad lists the backup's nodes as ready for about 5 minutes whether they answer or not, and it refuses a " +
	"client\nthat registered with these servers before the restore. After 5 minutes run tent validate cluster, " +
	"and replace the\nnodes that it reports with tent rolling-update cluster --yes --force --nodegroups GROUPS.\n"

func newBackupCommand(opts *globalOptions) *cobra.Command {
	cmd := groupCommand("backup", "Back up and restore Nomad's state")
	cmd.AddCommand(newBackupCreateCommand(opts), newBackupListCommand(opts), newBackupRestoreCommand(opts))
	return cmd
}

func newBackupCreateCommand(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "create [NAME]",
		Short: "Store a snapshot of a cluster's Nomad state",
		Long: "Take a snapshot of the Nomad state of the cluster named by NAME or --name and store it in the state " +
			"store, under backups/. The snapshot holds the jobs, the nodes, the ACL tokens, the variables and the " +
			"keyring, so it is a secret, stored as the cluster's other secrets are. tent takes the cluster's lock " +
			"for the run. After the servers change, Nomad takes no snapshot until its next write; tent then tries " +
			"again for up to 5 minutes. The command needs the cloud's credentials in the environment, " +
			"VULTR_API_KEY for Vultr, and a way to port 4646 of the servers, which spec.access.api allows. tent " +
			"delete cluster deletes the backups with the cluster. Nothing else removes a backup: schedule the " +
			"command with cron or CI, and remove old backups with a lifecycle rule of the bucket, or delete the " +
			"objects.",
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
			svc.OnWarning = warnOnce(cmd.ErrOrStderr())
			stored, err := svc.BackupCreate(cmd.Context(), name)
			return report(err == nil || app.Saved(err), err, func() error {
				return printObject(cmd.OutOrStdout(), opts.output, stored, stored.WriteText)
			})
		},
	}
}

func newBackupListCommand(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list [NAME]",
		Short: "List the backups of a cluster",
		Long: "List the backups of the cluster named by NAME or --name, oldest first. SERVERS says whether a backup " +
			"is of the servers that run now (current) or of servers that tent has replaced since (earlier); tent " +
			"backup restore takes only an earlier one. The command reads only the state store.",
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
			backups, err := svc.Backups(cmd.Context(), name)
			if err != nil {
				return err
			}
			if len(backups) == 0 {
				// A notice that fails to print changes nothing.
				_, _ = io.WriteString(cmd.ErrOrStderr(), "cluster "+name+" has no backups\n")
			}
			return printObject(cmd.OutOrStdout(), opts.output, backups, func(w io.Writer) error {
				rows := make([][]string, len(backups))
				for i, b := range backups {
					servers := "current"
					if b.Earlier {
						servers = "earlier"
					}
					rows[i] = []string{b.Name, b.Time.Format(noticeTime), servers}
				}
				return writeTable(w, []string{"NAME", "TAKEN", "SERVERS"}, rows)
			})
		},
	}
}

func newBackupRestoreCommand(opts *globalOptions) *cobra.Command {
	var from string
	var yes bool
	cmd := &cobra.Command{
		Use:   "restore [NAME]",
		Short: "Put a backup back into a cluster's Nomad",
		Long: "Put a backup back into the Nomad of the cluster named by NAME or --name. This is for a cluster that " +
			"lost its servers. When a server machine is left, delete it in the cloud first. Then run tent update " +
			"cluster --yes, which builds new servers and bootstraps them with the stored secret, and then this " +
			"command. tent restores only a backup of earlier servers, only while the new servers run no job, and " +
			"one backup into servers that it built again; --from names it as tent backup list shows it, and " +
			"latest, the default, takes the newest. Without --yes the command prints what it would restore. With " +
			"--yes it takes the cluster's lock, remembers the restore, sends the backup to Nomad and checks that " +
			"the stored ACL bootstrap secret still works; a run that stops is finished by running the command " +
			"again, and a finished one changes nothing more. Afterwards Nomad holds the jobs, nodes, ACL tokens, " +
			"variables and keyring of the backup. A client of the backup that still runs registers again by " +
			"itself when it reaches the new servers. Nomad refuses a client that registered with the new servers " +
			"before the restore, and lists the backup's nodes as ready for about 5 minutes: then run tent " +
			"validate cluster, and replace the nodes that it reports with tent rolling-update cluster --yes " +
			"--force --nodegroups GROUPS. The command needs the cloud's credentials in the environment, " +
			"VULTR_API_KEY for Vultr, and a way to port 4646 of the servers.",
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
			plan, err := svc.BackupRestore(cmd.Context(), name, from, yes)
			return report(err == nil || app.Saved(err), err, func() error {
				text := plan.WriteText
				if plan.Applied {
					text = plan.WriteApplied
				}
				if err := printObject(cmd.OutOrStdout(), opts.output, plan, text); err != nil {
					return err
				}
				// A notice that fails to print changes nothing.
				switch {
				case plan.Applied:
					_, _ = io.WriteString(cmd.ErrOrStderr(), restoredNotice)
				case !plan.Restored:
					_, _ = io.WriteString(cmd.ErrOrStderr(), "run with --yes to restore it\n")
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&from, "from", app.Latest,
		"the backup to restore: a name that tent backup list shows, or latest")
	cmd.Flags().BoolVar(&yes, "yes", false, "restore the backup; without it, print what would be restored")
	return cmd
}
