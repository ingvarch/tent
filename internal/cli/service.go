package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/buildinfo"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// service opens the state store and returns the use cases over it for cmd.
func (o *globalOptions) service(cmd *cobra.Command, validate v1alpha1.ValidateOptions) (*app.Service, error) {
	u, err := o.requireState()
	if err != nil {
		return nil, err
	}
	open := statestore.Open
	if o.openStore != nil {
		open = o.openStore
	}
	store, err := open(cmd.Context(), u)
	if err != nil {
		return nil, interrupted(cmd.Context(), err)
	}
	o.logger.Debug("opened the state store", "url", store.String())
	stderr := cmd.ErrOrStderr()
	return &app.Service{
		Store:       store,
		Version:     buildinfo.Get().Version,
		LockTimeout: o.lockTimeout,
		Validate:    validate,
		Providers:   o.cloudProviders(),
		Assets:      o.assets,
		Nomad:       o.nomad,
		Channels:    o.channels,
		Log:         o.logger,
		OnWait: func(holder error) {
			// A notice that fails to print changes nothing.
			_, _ = fmt.Fprintf(stderr, "%v; waiting up to %s (--lock-timeout)\n", holder, o.lockTimeout)
		},
		OnWeakLock: func() {
			_, _ = io.WriteString(stderr, "WARNING: this state store does not support conditional writes, so its "+
				"cluster lock is best effort: two tents may change the cluster at once\n")
		},
		OnTakeover: func(previous statestore.Lease) {
			_, _ = fmt.Fprintf(stderr, "WARNING: took over the cluster lock from %s, whose lease had expired or "+
				"whose tent had ended\n", previous)
		},
	}, nil
}

// cloudProviders returns the providers of one command, which log to its logger; nil when tent reaches no cloud. It
// builds each provider once, when the command first needs it, so that the command reaches a cloud through one
// client.
func (o *globalOptions) cloudProviders() func(v1alpha1.Provider) (cloud.Provider, error) {
	if o.providers == nil {
		return nil
	}
	built := map[v1alpha1.Provider]cloud.Provider{}
	return func(name v1alpha1.Provider) (cloud.Provider, error) {
		if p, ok := built[name]; ok {
			return p, nil
		}
		p, err := o.providers(name, o.logger)
		if err != nil {
			return nil, err
		}
		built[name] = p
		return p, nil
	}
}

// interruptedError says that a command was interrupted, in the words of the use cases. It matches the error it
// stands for.
type interruptedError struct{ err error }

func (e *interruptedError) Error() string { return "interrupted" }

func (e *interruptedError) Unwrap() error { return e.err }

// interrupted returns err, or when err comes from the end of ctx, an error that says the command was interrupted.
func interrupted(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil || !errors.Is(err, ctx.Err()) {
		return err
	}
	return &interruptedError{err}
}

// addAllowSingleServer adds --allow-single-server to a command that validates specs.
func addAllowSingleServer(cmd *cobra.Command, allow *bool) {
	cmd.Flags().BoolVar(allow, "allow-single-server", false, "accept a server group of size 1, which has no failover")
}

// writeFunc is a use case that writes objects, or with apply unset only checks them: Create or Replace.
type writeFunc func(svc *app.Service, ctx context.Context, objs spec.Objects, apply bool) ([]app.Change, error)

// writeSpecFile reads the spec file at path and stores its objects with write, as writeObjects does.
func writeSpecFile(cmd *cobra.Command, opts *globalOptions, path string, allowSingle bool, write writeFunc,
	update bool,
) error {
	if path == "" {
		return errors.New("-f needs a file path, or - for standard input")
	}
	if _, err := opts.requireState(); err != nil {
		return err // before it waits for standard input
	}
	objs, err := readSpec(cmd, path)
	if err != nil {
		return err
	}
	return writeObjects(cmd, opts, objs, allowSingle, write, update)
}

// writeObjects stores objs with write and prints the changes. With update set, it then updates the cluster of objs
// as update cluster --yes does.
func writeObjects(cmd *cobra.Command, opts *globalOptions, objs spec.Objects, allowSingle bool,
	write writeFunc, update bool,
) error {
	svc, err := opts.service(cmd, v1alpha1.ValidateOptions{AllowSingleServer: allowSingle})
	if err != nil {
		return err
	}
	svc.OnWarning = warnOnce(cmd.ErrOrStderr())
	changes, err := write(svc, cmd.Context(), objs, true)
	if update && opts.output != outputTable {
		return updateCreated(cmd, opts, svc, clusterOf(objs), changes, err)
	}
	err = report(changes != nil, err, func() error { return printChanges(cmd.OutOrStdout(), opts.output, changes) })
	if err != nil || !update {
		return err
	}
	return applyUpdateText(cmd, opts, svc, clusterOf(objs))
}

// createOutput is what create --yes prints with -o json and -o yaml: the changes of the create, and the plan that the
// update applied, which is left out when the update failed.
type createOutput struct {
	Changes []changeOutput  `json:"changes"`
	Update  *app.UpdatePlan `json:"update,omitempty"`
}

// updateCreated updates the cluster after a create that made changes and ended with err, and prints both as one
// document: the create's changes and the plan that the update applied.
func updateCreated(cmd *cobra.Command, opts *globalOptions, svc *app.Service, cluster string, changes []app.Change,
	err error,
) error {
	out := createOutput{Changes: changeOutputs(changes)}
	if err == nil {
		var plan app.UpdatePlan
		plan, err = applyUpdate(cmd, opts, svc, cluster)
		if err == nil || app.Saved(err) {
			out.Update = &plan
		}
	}
	return report(changes != nil, err, func() error { return printObject(cmd.OutOrStdout(), opts.output, out, nil) })
}

// clusterOf returns the name of the cluster that objs belong to: the Cluster's, or else its node groups'.
func clusterOf(objs spec.Objects) string {
	if objs.Cluster != nil {
		return objs.Cluster.Metadata.Name
	}
	for _, g := range objs.NodeGroups {
		if g != nil {
			return g.Metadata.Cluster
		}
	}
	return ""
}

// warnOnce returns an OnWarning that prints each warning on w once, so that a command that stores specs and then
// updates the cluster warns once.
func warnOnce(w io.Writer) func(string) {
	warned := map[string]bool{}
	return func(warning string) {
		if !warned[warning] {
			warned[warning] = true
			_, _ = io.WriteString(w, "WARNING: "+warning+"\n")
		}
	}
}

// report prints what a use case did and returns the use case's error. It prints nothing for a use case that failed
// without doing anything, and prints what one did that returned an error with it, such as a change that was saved
// although its lock was lost.
func report(did bool, err error, show func() error) error {
	if err != nil && !did {
		return err
	}
	return errors.Join(err, show())
}

// readSpec reads and decodes the spec file at path, or standard input when path is "-".
func readSpec(cmd *cobra.Command, path string) (spec.Objects, error) {
	var (
		data []byte
		err  error
	)
	if path == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return spec.Objects{}, fmt.Errorf("reading the spec: %w", err)
	}
	return spec.Decode(data)
}

// changeOutput is one change as -o json and -o yaml print it.
type changeOutput struct {
	Kind   string     `json:"kind"`
	Name   string     `json:"name"`
	Action app.Action `json:"action"`
}

// changeOutputs returns the changes as -o json and -o yaml print them.
func changeOutputs(changes []app.Change) []changeOutput {
	list := make([]changeOutput, len(changes))
	for i, c := range changes {
		list[i] = changeOutput(c)
	}
	return list
}

// printChanges writes what a command did to each object: one line each, such as "node group workers created".
func printChanges(w io.Writer, format string, changes []app.Change) error {
	return printObject(w, format, changeOutputs(changes), func(w io.Writer) error {
		for _, c := range changes {
			if _, err := fmt.Fprintf(w, "%s %s %s\n", kindWord(c.Kind), c.Name, c.Action); err != nil {
				return fmt.Errorf("writing the changes: %w", err)
			}
		}
		return nil
	})
}

// kindWord names a kind in a sentence: cluster or node group.
func kindWord(kind string) string {
	if kind == v1alpha1.KindNodeGroup {
		return "node group"
	}
	return "cluster"
}
