package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/statestore"
)

// Environment variables that set what --state and --name set.
const (
	envState   = "TENT_STATE"
	envCluster = "TENT_CLUSTER"
)

// Output formats accepted by -o.
const (
	outputTable = "table"
	outputYAML  = "yaml"
	outputJSON  = "json"
)

// Log formats accepted by --log-format.
const (
	logFormatText = "text"
	logFormatJSON = "json"
)

// globalOptions are the settings every command receives. A value the command line leaves unset comes from
// TENT_STATE or TENT_CLUSTER, then the config file, then the default.
type globalOptions struct {
	state       string // state store URL, empty when unset
	cluster     string // cluster name, empty when unset
	clusterFlag bool   // the cluster name is from --name on the command line
	output      string
	logFormat   string
	verbosity   int
	lockTimeout time.Duration
	configPath  string
	logger      *slog.Logger
	// openStore opens the state store at a URL; statestore.Open when nil.
	openStore func(ctx context.Context, url string) (statestore.Store, error)
	// providers returns the cloud provider that a cluster's spec names; nil when tent reaches no cloud.
	providers Providers
	// assets says where the files that nodes download are found.
	assets assets.Options
	// nomad returns the API of one Nomad server; nil when tent reaches no Nomad.
	nomad func(nomadops.Config) (nomadops.API, error)
	// channels returns the release channel called name; the channels embedded in tent when nil.
	channels func(name string) (*channels.Channel, error)
}

// addFlags adds the global flags to cmd and its subcommands.
func (o *globalOptions) addFlags(cmd *cobra.Command) {
	f := cmd.PersistentFlags()
	f.StringVar(&o.state, "state", "", "state store URL: file:///abs/path or s3://bucket[/prefix] (env "+envState+")")
	f.StringVar(&o.cluster, "name", "", "cluster name (env "+envCluster+")")
	f.StringVarP(&o.output, "output", "o", outputTable, "output format: table, yaml or json")
	f.CountVarP(&o.verbosity, "verbose", "v", "log more: -v for info, -vv for debug")
	f.StringVar(&o.logFormat, "log-format", logFormatText, "log format: text or json")
	f.DurationVar(&o.lockTimeout, "lock-timeout", 5*time.Minute, "how long to wait for the cluster lock")
}

// resolve fills in what cmd's command line leaves unset, checks the values and creates the logger on cmd's stderr.
func (o *globalOptions) resolve(cmd *cobra.Command) error {
	path, homeErr := configPath()
	var file fileConfig
	if homeErr == nil {
		var err error
		if file, err = readConfig(path); err != nil {
			return err
		}
	}
	o.configPath = path

	flags := cmd.Flags()
	fill := func(name string, value *string, fallbacks ...string) {
		if !flags.Changed(name) {
			*value = cmp.Or(append(fallbacks, *value)...) // *value holds the flag's default
		}
	}
	fill("state", &o.state, os.Getenv(envState), file.state)
	fill("name", &o.cluster, os.Getenv(envCluster), file.cluster)
	o.clusterFlag = flags.Changed("name")
	fill("output", &o.output, file.output)
	fill("log-format", &o.logFormat, file.logFormat)

	if err := validateOutput(o.output); err != nil {
		return err
	}
	if err := validateLogFormat(o.logFormat); err != nil {
		return err
	}
	if o.lockTimeout < 0 {
		return fmt.Errorf("invalid --lock-timeout %s: must not be negative", o.lockTimeout)
	}
	o.logger = newLogger(cmd.ErrOrStderr(), o.logFormat, o.verbosity)
	if homeErr != nil {
		o.logger.Debug("no config file", "reason", homeErr)
	}
	return nil
}

// requireState returns the state store URL, or an error that says how to set one.
func (o *globalOptions) requireState() (string, error) {
	if o.state == "" {
		return "", o.unsetError("no state store", "set", "--state", envState, "state")
	}
	return o.state, nil
}

// requireCluster returns the cluster name, or an error that says how to set one.
func (o *globalOptions) requireCluster() (string, error) {
	if o.cluster == "" {
		return "", o.unsetError("no cluster name", "set", "--name", envCluster, "cluster")
	}
	return o.cluster, nil
}

// clusterArg returns the cluster named by a command's optional NAME argument, or else by --name. A NAME and a --name
// on the command line must agree.
func (o *globalOptions) clusterArg(args []string) (string, error) {
	switch {
	case len(args) == 0 && o.cluster == "":
		return "", o.unsetError("no cluster name", "give NAME or set", "--name", envCluster, "cluster")
	case len(args) == 0:
		return o.cluster, nil
	case o.clusterFlag && o.cluster != args[0]:
		return "", fmt.Errorf("NAME %s and --name %s differ; give one", args[0], o.cluster)
	}
	return args[0], nil
}

// unsetError says that a value is missing and how to give it: how, such as "set", then the flag, the variable and
// the config key.
func (o *globalOptions) unsetError(what, how, flag, env, key string) error {
	if o.configPath == "" {
		return fmt.Errorf("%s: %s %s or %s", what, how, flag, env)
	}
	return fmt.Errorf("%s: %s %s, %s or %q in %s", what, how, flag, env, key, o.configPath)
}

// newLogger returns a logger that writes to w in format. It logs warnings and errors, info with -v, and debug with
// -vv.
func newLogger(w io.Writer, format string, verbosity int) *slog.Logger {
	opts := &slog.HandlerOptions{Level: logLevel(verbosity)}
	if format == logFormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func logLevel(verbosity int) slog.Level {
	switch {
	case verbosity >= 2:
		return slog.LevelDebug
	case verbosity == 1:
		return slog.LevelInfo
	default:
		return slog.LevelWarn
	}
}

func validateOutput(format string) error {
	switch format {
	case outputTable, outputYAML, outputJSON:
		return nil
	}
	return invalidOutputError(format)
}

func invalidOutputError(format string) error {
	return fmt.Errorf("invalid output format %q: want table, yaml or json", format)
}

func validateLogFormat(format string) error {
	switch format {
	case logFormatText, logFormatJSON:
		return nil
	}
	return fmt.Errorf("invalid log format %q: want text or json", format)
}
