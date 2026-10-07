package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// tentNodeFile is the tent-node for linux/amd64 that make build writes next to tent.
const tentNodeFile = "tent-node_linux_amd64"

// checkTentNode checks that the tent-node next to tent has the sha256 sum. It reads no version. A node refuses a
// tent-node whose version is not its tent's, so the check relies on make build writing both binaries with one
// version; a tent built any other way passes it and fails on the nodes.
func checkTentNode(tent, sum string) error {
	if !filepath.IsAbs(tent) {
		return fmt.Errorf("E2E_TENT is %q, want an absolute path", tent)
	}
	node := filepath.Join(filepath.Dir(tent), tentNodeFile)
	data, err := os.ReadFile(node)
	if err != nil {
		return fmt.Errorf("read the tent-node built with %s: %w", tent, err)
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != sum {
		return errors.New("TENT_NODE_SHA256 is not the sha256 of " + node + ", the tent-node built with " + tent +
			": run the suite with make e2e, which uploads the tent-node of its build")
	}
	return nil
}
