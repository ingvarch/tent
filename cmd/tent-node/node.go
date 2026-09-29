package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/env"
	"github.com/ingvarch/tent/internal/nodeup/env/vultr"
)

// install installs the systemd units that run tent-node with the node config, and starts them: tent-node.service
// runs up, and install waits until it has.
func install(ctx context.Context, args []string, _, stderr io.Writer, d deps) error {
	config, err := configFlag("install", args, stderr)
	if err != nil {
		return err
	}
	h, nc, err := load(d, stderr, config)
	if err != nil {
		return err
	}
	binary, err := d.executable()
	if err != nil {
		return fmt.Errorf("find the tent-node binary: %w", err)
	}
	return nodeup.Install(ctx, h, nc, config, binary)
}

// up sets the machine up as the node config says, with the metadata service of the config's cloud.
func up(ctx context.Context, args []string, _, stderr io.Writer, d deps) error {
	config, err := configFlag("up", args, stderr)
	if err != nil {
		return err
	}
	h, nc, err := load(d, stderr, config)
	if err != nil {
		return err
	}
	e, err := d.environment(nc.Provider)
	if err != nil {
		return err
	}
	_, err = nodeup.Up(ctx, h, nc, nodeup.Phases(e))
	return err
}

// refreshJoin is not built yet: it leaves the servers that the node joins as they are.
func refreshJoin(_ context.Context, args []string, _, stderr io.Writer, _ deps) error {
	if _, err := configFlag("refresh-join", args, stderr); err != nil {
		return err
	}
	logger(stderr).Info("refresh-join is not built yet: the servers that the node joins stay as they are")
	return nil
}

// environment returns the metadata service of the cloud p.
func environment(p v1alpha1.Provider) (env.Environment, error) {
	if p == v1alpha1.ProviderVultr {
		return vultr.New(), nil
	}
	return nil, fmt.Errorf("tent-node does not support provider %s yet", p)
}

// configFlag parses the arguments of the command name, which takes only --config, and returns the config's path. It
// returns flag.ErrHelp once it has written the usage for -h.
func configFlag(name string, args []string, stderr io.Writer) (string, error) {
	fs := flag.NewFlagSet("tent-node "+name, flag.ContinueOnError)
	config := fs.String("config", nodeconfig.ConfigPath, "the node config's `path`")
	// The flag package writes errors too; finish writes them once, as it does every error.
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fs.SetOutput(stderr)
		fs.Usage()
		return "", err
	case err != nil:
		return "", usageError(fmt.Sprintf("%s; run %s -h for its flags", err, fs.Name()))
	case fs.NArg() > 0:
		return "", usageError(fs.Name() + " takes no arguments, only flags")
	}
	if err := nodeup.CheckPath(*config); err != nil {
		return "", usageError("--config: " + err.Error())
	}
	return *config, nil
}

// load returns the machine, which logs to stderr, and the node config at path on it.
func load(d deps, stderr io.Writer, path string) (*nodeup.Host, *nodeconfig.NodeConfig, error) {
	h, err := d.host(logger(stderr))
	if err != nil {
		return nil, nil, err
	}
	data, err := h.FS.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read the node config: %w", err)
	}
	nc, err := nodeconfig.Decode(data)
	if err != nil {
		return nil, nil, err
	}
	return h, nc, nil
}

// logger returns the logger of the commands, which writes text to stderr: under systemd, into the journal.
func logger(stderr io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(stderr, nil)) }
