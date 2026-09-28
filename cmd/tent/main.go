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
	"github.com/ingvarch/tent/internal/cli"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
)

// main runs tent; cli.Execute handles Ctrl-C and SIGTERM.
func main() {
	os.Exit(cli.Execute(context.Background(), os.Args[1:], cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr},
		cli.WithProviders(providers(os.Getenv))))
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
