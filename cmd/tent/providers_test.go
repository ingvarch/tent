package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cli"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/statestore"
)

// fakeKey stands for an API key; it is not one.
const fakeKey = "not-a-real-key-0123456789"

// env returns a getenv that reads vars, and records the names it was asked for.
func env(vars map[string]string, asked *[]string) func(string) string {
	return func(name string) string {
		*asked = append(*asked, name)
		return vars[name]
	}
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestProvidersVultr(t *testing.T) {
	var asked []string
	p, err := providers(env(map[string]string{"VULTR_API_KEY": fakeKey}, &asked))(v1alpha1.ProviderVultr, discard)
	if err != nil {
		t.Fatalf("providers(vultr) = %v", err)
	}
	if p.Name() != "vultr" {
		t.Errorf("the provider is %s, want vultr", p.Name())
	}
	if len(asked) != 1 || asked[0] != "VULTR_API_KEY" {
		t.Errorf("read the variables %v, want VULTR_API_KEY", asked)
	}
}

// TestProvidersVultrWithoutKey fails with an error that is not the one of an unsupported provider: the cloud may hold
// objects of the cluster.
func TestProvidersVultrWithoutKey(t *testing.T) {
	var asked []string
	_, err := providers(env(nil, &asked))(v1alpha1.ProviderVultr, discard)
	if err == nil || err.Error() != "VULTR_API_KEY is not set" {
		t.Errorf("providers(vultr) = %v, want VULTR_API_KEY is not set", err)
	}
	if errors.Is(err, cloud.ErrUnsupportedProvider) {
		t.Errorf("errors.Is(%v, ErrUnsupportedProvider) = true, want false", err)
	}
}

// TestProvidersVultrBadKey names the variable, and never the key.
func TestProvidersVultrBadKey(t *testing.T) {
	var asked []string
	key := fakeKey + " \n"
	_, err := providers(env(map[string]string{"VULTR_API_KEY": key}, &asked))(v1alpha1.ProviderVultr, discard)
	if err == nil || !strings.HasPrefix(err.Error(), "VULTR_API_KEY: ") || strings.Contains(err.Error(), fakeKey) {
		t.Errorf("providers(vultr) = %v, want an error about VULTR_API_KEY without the key", err)
	}
}

// TestProvidersUnsupported fails for a provider that tent knows and cannot manage yet, without reading any
// credentials.
func TestProvidersUnsupported(t *testing.T) {
	var asked []string
	_, err := providers(env(map[string]string{"VULTR_API_KEY": fakeKey}, &asked))(v1alpha1.ProviderHetzner, discard)
	if want := "tent cannot manage clusters on hetzner yet"; err == nil || err.Error() != want {
		t.Errorf("providers(hetzner) = %v, want %s", err, want)
	}
	if !errors.Is(err, cloud.ErrUnsupportedProvider) {
		t.Errorf("errors.Is(%v, ErrUnsupportedProvider) = false", err)
	}
	if len(asked) != 0 {
		t.Errorf("providers(hetzner) read the variables %v, want none", asked)
	}
}

// TestProvidersUnknown fails for a name that tent does not know with an error that is not the one of an unsupported
// provider: a broken spec must not make delete cluster delete the state alone.
func TestProvidersUnknown(t *testing.T) {
	for _, name := range []v1alpha1.Provider{"aws", "", "vulr", "Vultr"} {
		var asked []string
		_, err := providers(env(map[string]string{"VULTR_API_KEY": fakeKey}, &asked))(name, discard)
		if want := `unknown cloud provider "` + string(name) + `"`; err == nil || err.Error() != want {
			t.Errorf("providers(%q) = %v, want %s", name, err, want)
		}
		if errors.Is(err, cloud.ErrUnsupportedProvider) {
			t.Errorf("errors.Is(%v, ErrUnsupportedProvider) = true, want false", err)
		}
		if len(asked) != 0 {
			t.Errorf("providers(%q) read the variables %v, want none", name, asked)
		}
	}
}

// TestDeleteClusterOnAnUnknownProvider refuses to delete a cluster whose stored spec names a provider that tent does
// not know, and keeps its state: the cloud may hold objects of the cluster.
func TestDeleteClusterOnAnUnknownProvider(t *testing.T) {
	newInProcess(t) // keeps the user's config file away
	s := newStore(t)
	const spec = "apiVersion: tent/v1alpha1\nkind: Cluster\nmetadata:\n  name: prod\nspec:\n  cloud:\n" +
		"    provider: vultr-eu\n    region: ams\n"
	if _, err := s.open(t).Put(t.Context(), "prod/cluster.yaml", []byte(spec), statestore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	before := s.objects(t)
	for _, yes := range []string{"--yes=false", "--yes"} {
		var out, errOut bytes.Buffer
		code := cli.Execute(t.Context(), []string{"delete", "cluster", "prod", yes, "--state", s.url},
			cli.Streams{In: strings.NewReader(""), Out: &out, Err: &errOut},
			cli.WithProviders(providers(env(map[string]string{"VULTR_API_KEY": fakeKey}, new([]string)))))
		wantResult(t, result{code, out.String(), errOut.String()}, 1, "",
			"Error: unknown cloud provider \"vultr-eu\"\n")
	}
	s.wantUntouched(t, before)
}
