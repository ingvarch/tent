// Command tent provisions and operates HashiCorp Nomad clusters.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/cli"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/nomadops"
)

// main runs tent; cli.Execute handles Ctrl-C and SIGTERM.
func main() {
	os.Exit(cli.Execute(context.Background(), os.Args[1:], cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr},
		cli.WithProviders(providers(os.Getenv)), cli.WithAssets(nodeAssets(os.Getenv)), cli.WithNomad(nomadServer),
		cli.WithNomadProxy(nomadops.NewProxy)))
}

// nodeAssets returns where nodes find the tent-node of a development build: TENT_NODE_URL and its sha256 in
// TENT_NODE_SHA256. A release build ignores them.
func nodeAssets(getenv func(string) string) assets.Options {
	return assets.Options{DevURL: getenv("TENT_NODE_URL"), DevSHA256: getenv("TENT_NODE_SHA256")}
}

// nomadServer returns the API of the Nomad server that cfg names. It returns a nil API with the error, so that the
// caller's nil check holds.
func nomadServer(cfg nomadops.Config) (nomadops.API, error) {
	client, err := nomadops.New(cfg)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// providers returns the clouds that tent manages clusters on: Vultr, with the API key in VULTR_API_KEY. It reads the
// key with getenv only when a cluster on Vultr needs it, and never puts the key into an error. Another provider that
// the API lists fails with cloud.UnsupportedProvider, and a name that it does not list fails with another error.
func providers(getenv func(string) string) cli.Providers {
	return func(name v1alpha1.Provider, log *slog.Logger) (cloud.Provider, error) {
		switch {
		case !slices.Contains(v1alpha1.Providers(), name):
			return nil, fmt.Errorf("unknown cloud provider %q", name)
		case name != v1alpha1.ProviderVultr:
			return nil, cloud.UnsupportedProvider(name)
		}
		key := getenv("VULTR_API_KEY")
		if key == "" {
			return nil, errors.New("VULTR_API_KEY is not set")
		}
		client, err := vultr.NewClient(key)
		if err != nil {
			return nil, fmt.Errorf("VULTR_API_KEY: %w", err)
		}
		return vultr.New(client, vultr.WithLogger(log)), nil
	}
}
