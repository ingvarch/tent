package nodeconfig

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// hashFormat is the version of the canonical form that SpecHash hashes. It is part of the form, so a new form gives
// every config a new hash.
const hashFormat = 1

// canon is the canonical form of a node group's configuration. It is built field by field: the JSON forms of File and
// Asset leave out the content and show URLs, which the hash must not follow.
type canon struct {
	Format   int          `json:"format"`
	Files    []canonFile  `json:"files"`
	Assets   []canonAsset `json:"assets"`
	System   System       `json:"system"`
	Firewall HostFirewall `json:"firewall"`
}

type canonFile struct {
	Path    string `json:"path"`
	Mode    uint32 `json:"mode"`
	Owner   string `json:"owner"`
	Content string `json:"content"`
}

type canonAsset struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// SpecHash returns the hash of the node group's configuration, 16 lower-case hex digits: the start of the sha256 of
// a canonical JSON of
//   - the group-level files that are not secret, sorted by path, with their path, mode, owner and content;
//   - the assets, sorted by name, with their name, version and sha256;
//   - the system settings;
//   - the host firewall, its rules sorted by name, protocol, ports and sources, and the sources of each rule sorted.
//
// Every node of a group has the same hash, and a node whose hash differs from its group's is out of date. The node's
// name, its own files, its join settings, the secret files, where the assets come from, the provider, which a
// cluster never changes, and the region, which 00-tent.hcl carries, leave the hash as it is, so neither a new mirror
// nor a new server marks a node out of date.
// The order of the files, the assets, the rules and their sources leaves it as it is too. For nil it returns "".
func SpecHash(nc *NodeConfig) string {
	if nc == nil {
		return ""
	}
	c := canon{Format: hashFormat, Files: []canonFile{}, Assets: []canonAsset{}, System: nc.System,
		Firewall: canonFirewall(nc.Firewall)}
	for _, f := range nc.Files {
		if !f.PerNode && !f.Secret {
			c.Files = append(c.Files, canonFile{Path: f.Path, Mode: f.Mode, Owner: f.Owner, Content: string(f.Content)})
		}
	}
	// Sorting by every field puts even the files of one path, or the assets of one name, in one order.
	slices.SortFunc(c.Files, func(a, b canonFile) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), cmp.Compare(a.Mode, b.Mode), strings.Compare(a.Owner, b.Owner),
			strings.Compare(a.Content, b.Content))
	})
	for _, a := range nc.Assets {
		c.Assets = append(c.Assets, canonAsset{Name: a.Name, Version: a.Version, SHA256: a.SHA256})
	}
	slices.SortFunc(c.Assets, func(a, b canonAsset) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Version, b.Version),
			strings.Compare(a.SHA256, b.SHA256))
	})
	data, _ := json.Marshal(c) // never fails: the form holds strings, numbers, a map with string keys and netip values
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// canonFirewall returns a copy of fw with its rules sorted by name, protocol, ports and sources, and the sources of
// each rule sorted by address and length.
func canonFirewall(fw HostFirewall) HostFirewall {
	rules := make([]Rule, len(fw.Rules))
	for i, r := range fw.Rules {
		r.From = slices.SortedFunc(slices.Values(r.From), netip.Prefix.Compare)
		rules[i] = r
	}
	slices.SortFunc(rules, func(a, b Rule) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Protocol, b.Protocol),
			cmp.Compare(a.Ports.First, b.Ports.First), cmp.Compare(a.Ports.Last, b.Ports.Last),
			slices.CompareFunc(a.From, b.From, netip.Prefix.Compare))
	})
	fw.Rules = rules
	return fw
}

// checkSpecHash checks that a stored spec hash is the hash of the configuration.
func (nc *NodeConfig) checkSpecHash() error {
	if nc.SpecHash == "" {
		return nil
	}
	if want := SpecHash(nc); nc.SpecHash != want {
		return fmt.Errorf("spec hash %q is not the hash of the configuration, %s", nc.SpecHash, want)
	}
	return nil
}
