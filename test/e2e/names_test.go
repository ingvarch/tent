package e2e

import (
	"fmt"
	"io"
	"net/netip"
	"strings"
)

const runIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

const runIDLength = 6

// newRunID reads a run id of 6 characters of [a-z0-9] from r.
func newRunID(r io.Reader) (string, error) {
	raw := make([]byte, runIDLength)
	if _, err := io.ReadFull(r, raw); err != nil {
		return "", fmt.Errorf("read the run id: %w", err)
	}
	id := make([]byte, runIDLength)
	for i, b := range raw {
		id[i] = runIDAlphabet[int(b)%len(runIDAlphabet)]
	}
	return string(id), nil
}

// clusterName names the cluster of one image in a run: e2e-<run>-<the digits of the image>.
func clusterName(run, image string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, image)
	return "e2e-" + run + "-" + digits
}

// runnerCIDR turns the address of the machine that runs the suite into a CIDR of one address.
func runnerCIDR(addr string) (string, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(addr))
	if err != nil {
		return "", fmt.Errorf("the runner address %q: %w", addr, err)
	}
	ip = ip.Unmap()
	return netip.PrefixFrom(ip, ip.BitLen()).String(), nil
}
