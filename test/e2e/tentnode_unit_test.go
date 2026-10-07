package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// builtPair writes a tent and the tent-node next to it, as make build does, and returns the tent's path and the
// tent-node's sha256.
func builtPair(t *testing.T) (tent, sum string) {
	t.Helper()
	dir := t.TempDir()
	tent = filepath.Join(dir, "tent")
	node := []byte("tent-node of this build")
	for path, data := range map[string][]byte{tent: []byte("tent"), filepath.Join(dir, tentNodeFile): node} {
		if err := os.WriteFile(path, data, 0o700); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	h := sha256.Sum256(node)
	return tent, hex.EncodeToString(h[:])
}

func TestCheckTentNodeAcceptsTheTentNodeOfTheSameBuild(t *testing.T) {
	tent, sum := builtPair(t)
	if err := checkTentNode(tent, sum); err != nil {
		t.Errorf("checkTentNode: %v", err)
	}
}

func TestCheckTentNodeRefusesAnUploadOfAnotherBuild(t *testing.T) {
	tent, _ := builtPair(t)
	other := strings.Repeat("0", 64)
	err := checkTentNode(tent, other)
	want := "TENT_NODE_SHA256 is not the sha256 of " + filepath.Join(filepath.Dir(tent), tentNodeFile)
	if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), "make e2e") {
		t.Errorf("error = %v, want one that starts with %q and names make e2e", err, want)
	}
}

func TestCheckTentNodeNeedsTheTentNodeNextToTent(t *testing.T) {
	tent := filepath.Join(t.TempDir(), "tent")
	err := checkTentNode(tent, strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), filepath.Join(filepath.Dir(tent), tentNodeFile)) {
		t.Errorf("error = %v, want one that names the missing tent-node", err)
	}
}

func TestCheckTentNodeNeedsAnAbsoluteTent(t *testing.T) {
	_, sum := builtPair(t)
	err := checkTentNode(filepath.Join("bin", "tent"), sum)
	if err == nil || !strings.Contains(err.Error(), "E2E_TENT") {
		t.Errorf("error = %v, want one about E2E_TENT", err)
	}
}
