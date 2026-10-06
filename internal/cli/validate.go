package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
)

// validateInterval is how long a wait sleeps between two rounds of checks.
const validateInterval = 10 * time.Second

func newValidateCommand(opts *globalOptions) *cobra.Command {
	cmd := groupCommand("validate", "Check a cluster against its specs")
	cmd.AddCommand(newValidateClusterCommand(opts))
	return cmd
}

func newValidateClusterCommand(opts *globalOptions) *cobra.Command {
	var wait time.Duration
	var allowSingle bool
	cmd := &cobra.Command{
		Use:   "cluster [NAME]",
		Short: "Check a cluster against its specs",
		Long: "Check the cluster named by NAME or --name against its specs in the state store, and change nothing. " +
			"It checks that each node group has its machines, that the cloud reports them as running, that each has " +
			"joined Nomad, that Nomad has a leader, that the servers vote, are alive and healthy, that the clients " +
			"are registered, ready and eligible, that every node runs the pinned Nomad version, and that no " +
			"certificate has ended. It prints what differs, and warns about an open spec.access.api, a combined node " +
			"group, a cluster in one failure domain and certificates that end within 30 days. " +
			"The command exits with 0 when the cluster is valid, with 2 when it is not, and with 1 when tent could " +
			"not check it: for a spec or a stored secret that does not load, or a cloud that does not answer. " +
			"With --wait it checks every 10 seconds until the cluster is valid or the duration has passed, and " +
			"prints the last result. It needs the cloud's credentials in the environment, VULTR_API_KEY for Vultr, " +
			"and a way to port 4646 of the servers, which spec.access.api allows. It never takes the cluster's " +
			"lock; it tells who holds it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := opts.clusterArg(args)
			if err != nil {
				return err
			}
			if wait < 0 {
				return fmt.Errorf("invalid --wait %s: must not be negative", wait)
			}
			svc, err := opts.service(cmd, v1alpha1.ValidateOptions{AllowSingleServer: allowSingle})
			if err != nil {
				return err
			}
			svc.OnWarning = warnOnce(cmd.ErrOrStderr())
			v, err := validateUntil(cmd, svc, name, wait)
			if err != nil {
				return err
			}
			if err := reportValidation(cmd, opts, v); err != nil {
				return err
			}
			if !v.Valid() {
				return errNotValid
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 0, "check every 10 seconds until the cluster is valid or this long has passed")
	addAllowSingleServer(cmd, &allowSingle)
	return cmd
}

// validateUntil validates the cluster in rounds: one, and while the result is not valid and wait has not passed, one
// more after a sleep of validateInterval, or of the time that is left when that is less. The first sleep is announced
// once on stderr. It returns the result of the last round. A round that could not check ends the wait with that
// round's error and no result, and so does ctx when it ends during a sleep, with the error of an interrupted command.
func validateUntil(cmd *cobra.Command, svc *app.Service, cluster string, wait time.Duration) (app.Validation, error) {
	ctx := cmd.Context()
	deadline := time.Now().Add(wait)
	for announced := false; ; announced = true {
		v, err := svc.ValidateCluster(ctx, cluster)
		if err != nil {
			return app.Validation{}, err
		}
		left := time.Until(deadline)
		if v.Valid() || left <= 0 {
			return v, nil
		}
		if !announced {
			// A notice that fails to print changes nothing.
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cluster %s is not valid yet (%s); checking every %s for up to %s\n",
				cluster, countFailures(len(v.Failures)), validateInterval, wait)
		}
		if err := sleep(ctx, min(validateInterval, left)); err != nil {
			return app.Validation{}, err
		}
	}
}

// sleep waits for d, or returns the error that says the command was interrupted when ctx ends first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return interrupted(ctx, ctx.Err())
	}
}

// countFailures returns "1 failure" or "3 failures".
func countFailures(n int) string {
	if n == 1 {
		return "1 failure"
	}
	return fmt.Sprintf("%d failures", n)
}

// reportValidation tells on stderr who holds the cluster's lock, then prints the result v in the output format.
func reportValidation(cmd *cobra.Command, opts *globalOptions, v app.Validation) error {
	if v.Lock != nil {
		// A notice that fails to print changes nothing.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cluster %s is locked by %s\n", v.Cluster, v.Lock)
	}
	return printObject(cmd.OutOrStdout(), opts.output, v, v.WriteText)
}
