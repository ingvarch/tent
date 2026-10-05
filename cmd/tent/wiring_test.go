package main

import (
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
