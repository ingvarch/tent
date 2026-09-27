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

// writeSpecFile reads the spec file at path and stores its objects with write.
func writeSpecFile(cmd *cobra.Command, opts *globalOptions, path string, allowSingle bool, write writeFunc) error {
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
	return writeObjects(cmd, opts, objs, allowSingle, write)
}

// writeObjects stores objs with write and prints the changes.
func writeObjects(cmd *cobra.Command, opts *globalOptions, objs spec.Objects, allowSingle bool,
	write writeFunc,
) error {
	svc, err := opts.service(cmd, v1alpha1.ValidateOptions{AllowSingleServer: allowSingle})
	if err != nil {
		return err
	}
	svc.OnOpenAPI = warnOpenAPI(cmd.ErrOrStderr())
	changes, err := write(svc, cmd.Context(), objs, true)
	return report(changes != nil, err, func() error { return printChanges(cmd.OutOrStdout(), opts.output, changes) })
}

// warnOpenAPI returns an OnOpenAPI that warns on w that the whole internet may reach the Nomad API.
func warnOpenAPI(w io.Writer) func() {
	return func() {
		_, _ = io.WriteString(w, "WARNING: spec.access.api lets the whole internet reach the Nomad API (port 4646); "+
			"mTLS and ACLs protect it; narrow it with --api-access or spec.access.api\n")
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

// printChanges writes what a command did to each object: one line each, such as "node group workers created".
func printChanges(w io.Writer, format string, changes []app.Change) error {
	list := make([]changeOutput, len(changes))
	for i, c := range changes {
		list[i] = changeOutput(c)
	}
	return printObject(w, format, list, func(w io.Writer) error {
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
