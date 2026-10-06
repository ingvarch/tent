// Command tent-operator gives the operator access to the Nomad API of a cluster that tent built. It reads the
// cluster's CA and ACL bootstrap secret from the state store, issues an operator certificate and writes the files that
// the Nomad CLI needs into a new directory. It prints the lines that set the Nomad variables for fish or sh, and says
// on stderr how to set NOMAD_TOKEN from the token file, so no secret reaches the terminal. README.md says how to use
// it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/shellenv"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// Exit codes of the tool.
const (
	exitError = 1 // the cluster's secrets cannot be read or the files cannot be written
	exitUsage = 2 // the command line is wrong
)

// The environment variables that tent reads for the same settings as -state and -name.
const (
	envState   = "TENT_STATE"
	envCluster = "TENT_CLUSTER"
)

// The files that the tool writes into the directory.
const (
	caFile    = "ca.pem"
	certFile  = "cli.pem"
	keyFile   = "cli-key.pem"
	tokenFile = "token"
)

// Modes of what the tool writes: the key and the token are secrets.
const (
	dirMode  = 0o700
	fileMode = 0o600
)

// options are what the command line says.
type options struct {
	state   string        // the state store URL
	cluster string        // the cluster's name
	dir     string        // the new directory for the files
	addr    string        // the address of the Nomad API, https://host:port
	ttl     time.Duration // how long the operator certificate works
	shell   string        // shellenv.Sh or shellenv.Fish
}

// usageError is a wrong command line: the tool exits with exitUsage.
type usageError string

func (e usageError) Error() string { return string(e) }

// main runs the tool. Ctrl-C and SIGTERM cancel the reads from the store.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

// run runs the tool with args and returns its exit code. Only the lines for the shell go to stdout; the rest goes to
// stderr. getenv reads the environment.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	o, err := parseArgs(args, stderr, getenv)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err == nil {
		err = operate(ctx, o, stdout, stderr)
	}
	if err == nil {
		return 0
	}
	// Nowhere to report a failed write to stderr; the exit code still says the tool failed.
	_, _ = fmt.Fprintf(stderr, "Error: %s\n", err)
	if _, ok := errors.AsType[usageError](err); ok {
		return exitUsage
	}
	return exitError
}

// parseArgs reads the flags. The defaults of -state and -name stay empty, as the usage would print the URL; they
// come from the environment after the parse. It returns flag.ErrHelp once it has written the usage for -h.
func parseArgs(args []string, stderr io.Writer, getenv func(string) string) (options, error) {
	fs := flag.NewFlagSet("tent-operator", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.state, "state", "", "the state store `URL`: file:///abs/path or s3://bucket[/prefix] "+
		"(default $"+envState+")")
	fs.StringVar(&o.cluster, "name", "", "the cluster's `name` (default $"+envCluster+")")
	fs.StringVar(&o.dir, "dir", "", "the new `directory` for the files; it must not exist")
	fs.StringVar(&o.addr, "addr", "", "the `address` of the Nomad API, such as https://203.0.113.5:4646")
	fs.DurationVar(&o.ttl, "ttl", time.Hour, "how long the operator certificate works")
	fs.StringVar(&o.shell, "shell", "", "print the lines for fish or sh (default fish when $SHELL is fish, sh "+
		"otherwise)")
	// run writes the errors of the flag package once, as it writes every error.
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fs.SetOutput(stderr)
		writeUsage(fs)
		return options{}, err
	case err != nil:
		return options{}, usageError(err.Error() + "; run tent-operator -h for the flags")
	case fs.NArg() > 0:
		return options{}, usageError("tent-operator takes no arguments, only flags")
	}
	if o.state == "" {
		o.state = getenv(envState)
	}
	if o.cluster == "" {
		o.cluster = getenv(envCluster)
	}
	if o.shell == "" {
		o.shell = shellenv.Login(getenv("SHELL"))
	}
	if err := o.check(); err != nil {
		return o, err
	}
	// The printed paths must keep working after a cd.
	dir, err := filepath.Abs(o.dir)
	if err != nil {
		return o, fmt.Errorf("-dir: %w", err)
	}
	o.dir = dir
	return o, nil
}

// check checks that the flags are set and valid.
func (o options) check() error {
	for _, f := range []struct{ name, value, env string }{
		{"state", o.state, " or $" + envState}, {"name", o.cluster, " or $" + envCluster}, {"dir", o.dir, ""},
		{"addr", o.addr, ""},
	} {
		if f.value == "" {
			return usageError("-" + f.name + f.env + " is required")
		}
	}
	if u, err := url.Parse(o.addr); err != nil || u.Scheme != "https" || u.Host == "" {
		return usageError("-addr: want an https address with a host, such as https://203.0.113.5:4646")
	}
	if o.ttl <= 0 {
		return usageError(fmt.Sprintf("-ttl %s: want a duration above zero", o.ttl))
	}
	if !shellenv.Valid(o.shell) {
		return usageError(fmt.Sprintf("-shell %q: want fish or sh", o.shell))
	}
	return nil
}

// writeUsage writes how to run the tool.
func writeUsage(fs *flag.FlagSet) {
	_, _ = fmt.Fprint(fs.Output(), `Usage: tent-operator [flags]

Reads the CA and the ACL bootstrap secret of a cluster from the state store, issues an operator certificate and
writes ca.pem, cli.pem, cli-key.pem and token into the new directory -dir (mode 0700, the files 0600). It prints the
lines that set NOMAD_ADDR, NOMAD_CACERT, NOMAD_CLIENT_CERT, NOMAD_CLIENT_KEY and NOMAD_TLS_SERVER_NAME for fish or
sh, and says on stderr how to set NOMAD_TOKEN from the token file.

Flags:
`)
	fs.PrintDefaults()
}

// operate reads the cluster's secrets, issues the operator certificate and writes the files and the lines. It
// creates the directory only after everything is read and issued, and removes it when a write fails.
func operate(ctx context.Context, o options, stdout, stderr io.Writer) error {
	store, err := statestore.Open(ctx, o.state)
	if err != nil {
		return err
	}
	l, err := statestore.NewLayout(o.cluster)
	if err != nil {
		return err
	}
	region, err := nomadRegion(ctx, store, l)
	if err != nil {
		return err
	}
	ca, token, err := loadSecrets(ctx, store, l)
	if err != nil {
		return err
	}
	cert, err := ca.IssueOperator(region, o.ttl, time.Now())
	if err != nil {
		return err
	}
	if err := writeFiles(o.dir, map[string][]byte{
		caFile: ca.Bundle(), certFile: cert.Cert, keyFile: cert.Key.Bytes(), tokenFile: token.Bytes(),
	}); err != nil {
		return err
	}
	return printLines(o, region, stdout, stderr)
}

// read returns an object of the store, and an error that names the object and says what to do when it is missing.
func read(ctx context.Context, store statestore.Store, path string) ([]byte, error) {
	data, _, err := store.Get(ctx, path)
	if errors.Is(err, statestore.ErrNotFound) {
		return nil, fmt.Errorf("%s has no %s: run tent update cluster --yes first", store, path)
	}
	return data, err
}

// nomadRegion returns the Nomad region of the cluster's completed spec.
func nomadRegion(ctx context.Context, store statestore.Store, l statestore.Layout) (string, error) {
	data, err := read(ctx, store, l.Completed())
	if err != nil {
		return "", err
	}
	objs, err := spec.Decode(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", l.Completed(), err)
	}
	if objs.Cluster == nil || objs.Cluster.Spec.Nomad.Region == "" {
		return "", fmt.Errorf("%s has no cluster with a Nomad region", l.Completed())
	}
	return objs.Cluster.Spec.Nomad.Region, nil
}

// loadSecrets returns the cluster's CA and its ACL bootstrap secret. The errors name the objects and never show them.
func loadSecrets(ctx context.Context, store statestore.Store, l statestore.Layout) (*pki.CA, pki.Secret, error) {
	key, err := read(ctx, store, l.CAKey())
	if err != nil {
		return nil, nil, err
	}
	bundle, err := read(ctx, store, l.CABundle())
	if err != nil {
		return nil, nil, err
	}
	ca, err := pki.LoadCA(bundle, pki.Secret(key))
	if err != nil {
		return nil, nil, fmt.Errorf("the CA of %s: %w", l.Cluster(), err)
	}
	token, err := read(ctx, store, l.ACLBootstrapSecret())
	if err != nil {
		return nil, nil, err
	}
	if err := pki.CheckBootstrapSecret(pki.Secret(token)); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", l.ACLBootstrapSecret(), err)
	}
	return ca, pki.Secret(token), nil
}

// writeFiles makes the directory, which must not exist, and writes the files into it. When a write fails it removes
// the directory it made.
func writeFiles(dir string, files map[string][]byte) error {
	if err := os.Mkdir(dir, dirMode); err != nil {
		return fmt.Errorf("make the directory: %w", err)
	}
	for name, data := range files {
		if err := writeFile(filepath.Join(dir, name), data); err != nil {
			_ = os.RemoveAll(dir) // only what this call made
			return err
		}
	}
	return nil
}

// writeFile writes a new file with the owner-only mode.
func writeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("make the file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// printLines writes the lines that set the Nomad variables to stdout and the hint for NOMAD_TOKEN to stderr.
func printLines(o options, region string, stdout, stderr io.Writer) error {
	lines := [][2]string{
		{"NOMAD_ADDR", o.addr},
		{"NOMAD_CACERT", filepath.Join(o.dir, caFile)},
		{"NOMAD_CLIENT_CERT", filepath.Join(o.dir, certFile)},
		{"NOMAD_CLIENT_KEY", filepath.Join(o.dir, keyFile)},
		{"NOMAD_TLS_SERVER_NAME", "server." + region + ".nomad"},
	}
	for _, kv := range lines {
		if _, err := fmt.Fprintln(stdout, shellenv.ExportLine(o.shell, kv[0], kv[1])); err != nil {
			return err
		}
	}
	set := shellenv.FileLine(o.shell, "NOMAD_TOKEN", filepath.Join(o.dir, tokenFile))
	_, err := fmt.Fprintf(stderr, "wrote %s; the certificate works for %s\nto set NOMAD_TOKEN: %s\n", o.dir, o.ttl, set)
	return err
}
