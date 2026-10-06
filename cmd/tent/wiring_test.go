package main

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/nomadops"
)

// TestNodeAssets reads the development tent-node from TENT_NODE_URL and TENT_NODE_SHA256.
func TestNodeAssets(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	var asked []string
	opts := nodeAssets(env(map[string]string{"TENT_NODE_URL": "https://example.test/tent-node", "TENT_NODE_SHA256": sum},
		&asked))
	if opts.DevURL != "https://example.test/tent-node" || opts.DevSHA256 != sum {
		t.Errorf("nodeAssets = %q, %q, want the URL and the sha256 of the variables", opts.DevURL, opts.DevSHA256)
	}
	if len(asked) != 2 {
		t.Errorf("nodeAssets read the variables %v, want TENT_NODE_URL and TENT_NODE_SHA256", asked)
	}
}

// TestNodeAssetsUnset leaves the development file out when the variables are unset.
func TestNodeAssetsUnset(t *testing.T) {
	opts := nodeAssets(env(nil, new([]string)))
	if opts.DevURL != "" || opts.DevSHA256 != "" {
		t.Errorf("nodeAssets = %q, %q, want empty", opts.DevURL, opts.DevSHA256)
	}
}

// TestNomadServerBadConfig returns a nil interface with the error, not a nil client inside an interface.
func TestNomadServerBadConfig(t *testing.T) {
	api, err := nomadServer(nomadops.Config{})
	if err == nil {
		t.Fatal("nomadServer(Config{}) succeeded, want an error")
	}
	if api != nil {
		t.Errorf("nomadServer(Config{}) returned %T, want a nil interface", api)
	}
}

// TestBinaryServesTheNomadProxy gives tent ui the real proxy: the command goes on to read the state store, where a
// cluster that does not exist stops it, and not to the error of a missing proxy.
func TestBinaryServesTheNomadProxy(t *testing.T) {
	bin := sharedTent(t)
	var stdout, stderr bytes.Buffer
	cmd := tent(t, bin, "ui", "prod", "--state", newStore(t).url, "--listen", "127.0.0.1:0")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()

	if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
		t.Errorf("tent ui: err = %v, want exit code 1", err)
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), "no Nomad proxy is set up") ||
		!strings.Contains(stderr.String(), "prod") {
		t.Errorf("stdout = %q, stderr = %q, want an error about the cluster prod and no word about a proxy",
			stdout.String(), stderr.String())
	}
}

// TestNomadProxyBadConfig returns a nil handler with the error.
func TestNomadProxyBadConfig(t *testing.T) {
	handler, err := nomadops.NewProxy(nomadops.ProxyConfig{})
	if err == nil {
		t.Fatal("NewProxy(ProxyConfig{}) succeeded, want an error")
	}
	if handler != nil {
		t.Errorf("NewProxy(ProxyConfig{}) returned %T, want a nil handler", handler)
	}
}
