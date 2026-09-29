// Package nodeconfig is the contract between tent and tent-node: the NodeConfig that tent puts into a node's user data
// and tent-node reads on the node, and the Nomad agent configuration that tent renders into it.
package nodeconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// Kind is the kind of every NodeConfig. Its apiVersion is v1alpha1.APIVersion.
const Kind = "NodeConfig"

// JoinSeedAndRefresh is the join strategy that gives a new node the private addresses of the servers that exist when
// it is created, and has tent-node keep the list current from the servers' peers.
const JoinSeedAndRefresh = "seed-and-refresh"

// Protocols of the host firewall's rules.
const (
	ProtocolTCP  = "tcp"
	ProtocolUDP  = "udp"
	ProtocolICMP = "icmp"
)

// NodeConfig is everything tent-node needs to turn a fresh machine into a Nomad agent of its node group. Encode and
// Decode convert it to and from the JSON that the node reads.
type NodeConfig struct {
	APIVersion string        `json:"apiVersion"` // v1alpha1.APIVersion
	Kind       string        `json:"kind"`       // Kind
	Cluster    string        `json:"cluster"`
	NodeGroup  string        `json:"nodeGroup"`
	Name       string        `json:"name"` // the node's name, which is also its host name
	Role       v1alpha1.Role `json:"role"`
	Assets     []Asset       `json:"assets,omitempty"` // what the node downloads
	Files      []File        `json:"files,omitempty"`  // what the node writes; Encode writes their content
	Join       Join          `json:"join"`
	System     System        `json:"system,omitzero"`
	Firewall   HostFirewall  `json:"firewall"`
	// SpecHash is empty or SpecHash(nc), the hash of the node group's configuration, which tells a node that is out of
	// date.
	SpecHash string `json:"specHash,omitempty"`
}

// Asset is a file that the node downloads and checks by its sha256, such as the Nomad zip.
type Asset struct {
	Name    string   `json:"name"`    // such as nomad or cni-plugins
	Version string   `json:"version"` // the version of what the file holds
	URLs    []string `json:"urls"`    // where to download it, mirrors in order
	SHA256  string   `json:"sha256"`  // 64 lower-case hex digits
}

// String returns the asset's name, version, sha256 and URLs, such as nomad 2.0.7 sha256 0f1e… from https://….zip. A
// URL shows without its query, which may carry a signature, as https://bucket.example.com/tent-node?[query hidden], and
// without its password. fmt with any verb, slog and encoding/json show the same; Encode writes the URLs as they are.
func (a Asset) String() string {
	shown := make([]string, len(a.URLs))
	for i, raw := range a.URLs {
		u, err := url.Parse(raw)
		switch {
		case err != nil:
			shown[i] = "[a URL that does not parse]"
		case u.RawQuery != "":
			u.RawQuery = "[query hidden]"
			fallthrough
		default:
			shown[i] = u.Redacted()
		}
	}
	return fmt.Sprintf("%s %s sha256 %s from %s", a.Name, a.Version, a.SHA256, strings.Join(shown, ", "))
}

// GoString returns what String does.
func (a Asset) GoString() string { return a.String() }

// Format writes what String returns, whatever the verb, width and flags.
func (a Asset) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, a.String()) }

// LogValue makes slog log what String returns.
func (a Asset) LogValue() slog.Value { return slog.StringValue(a.String()) }

// MarshalJSON writes what String returns as a JSON string.
func (a Asset) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

// File is a file that the node writes. Its content never prints: fmt with any verb, slog and encoding/json show only
// its path and size, such as /etc/nomad.d/00-tent.hcl [1234 bytes], or /etc/nomad.d/01-gossip.hcl [secret, 71 bytes]
// for a secret file. Encode writes the content.
type File struct {
	Path    string // absolute
	Mode    uint32 // permission bits, such as 0o644, which node.json writes as a decimal number, 420
	Owner   string // user:group, such as root:root
	Content []byte // valid UTF-8 text
	PerNode bool   // written for this node only, such as its certificate; a group-level file is alike on every node
	Secret  bool   // holds a key or a token, such as the gossip key; its mode gives only its owner access
}

// String returns the path and the size of the file, such as /etc/nomad.d/00-tent.hcl [1234 bytes], or
// /etc/nomad.d/01-gossip.hcl [secret, 71 bytes] for a secret file. It never returns the content.
func (f File) String() string {
	if f.Secret {
		return fmt.Sprintf("%s [secret, %d bytes]", f.Path, len(f.Content))
	}
	return fmt.Sprintf("%s [%d bytes]", f.Path, len(f.Content))
}

// GoString returns what String does, so that %#v shows no content either.
func (f File) GoString() string { return f.String() }

// Format writes what String returns, whatever the verb, width and flags.
func (f File) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, f.String()) }

// LogValue makes slog log what String returns.
func (f File) LogValue() slog.Value { return slog.StringValue(f.String()) }

// MarshalJSON writes what String returns as a JSON string.
func (f File) MarshalJSON() ([]byte, error) { return json.Marshal(f.String()) }

// Join is how the node finds the Nomad servers.
type Join struct {
	Strategy string       `json:"strategy"` // JoinSeedAndRefresh
	Servers  []netip.Addr `json:"servers,omitempty"`
	// RefreshInterval is how often tent-node asks the servers for their peers, at least a second. node.json writes it
	// as a number of nanoseconds, such as 60000000000 for a minute.
	RefreshInterval time.Duration `json:"refreshInterval"`
}

// System is how the node sets up its operating system.
type System struct {
	Sysctls       map[string]string `json:"sysctls,omitempty"`
	KernelModules []string          `json:"kernelModules,omitempty"`
	Docker        bool              `json:"docker,omitempty"` // install Docker from the distribution's packages
}

// HostFirewall is the firewall on the node itself. Clouds filter only the public interface; the node's own firewall
// filters the traffic from the private network too.
type HostFirewall struct {
	Rules []Rule `json:"rules,omitempty"` // what reaches the node; everything else is dropped
	// BlockMetadata is the address of the cloud's metadata service, which serves the node's user data. Workloads
	// must not reach it.
	BlockMetadata netip.Addr `json:"blockMetadata"`
}

// Rule lets traffic from some networks reach some ports of the node.
type Rule struct {
	Name     string         `json:"name"`           // such as ssh or nomad-rpc
	Protocol string         `json:"protocol"`       // ProtocolTCP, ProtocolUDP or ProtocolICMP
	Ports    PortRange      `json:"ports,omitzero"` // the destination ports; none for ICMP
	From     []netip.Prefix `json:"from"`           // the source networks
}

// PortRange is the ports from First to Last, both included.
type PortRange struct {
	First uint16 `json:"first"`
	Last  uint16 `json:"last"`
}

var (
	hostNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	sha256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ownerPattern    = regexp.MustCompile(`^[a-z_][a-z0-9_-]*:[a-z_][a-z0-9_-]*$`)
	sysctlPattern   = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)+$`)
	modulePattern   = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

// Validate checks that tent-node can act on the NodeConfig: its header, names and role, the form of every asset, file,
// join setting, system setting and firewall rule, and that a spec hash, when there is one, is SpecHash(nc). It returns
// the first problem it finds.
func (nc *NodeConfig) Validate() error {
	if nc == nil {
		return errors.New("no node config")
	}
	for _, check := range []func() error{
		nc.checkHeader, nc.checkAssets, nc.checkFiles, nc.Join.check, nc.System.check, nc.Firewall.check,
		nc.checkSpecHash,
	} {
		if err := check(); err != nil {
			return fmt.Errorf("node config: %w", err)
		}
	}
	return nil
}

func (nc *NodeConfig) checkHeader() error {
	switch {
	case nc.APIVersion != v1alpha1.APIVersion:
		return fmt.Errorf("apiVersion %q is not %s", nc.APIVersion, v1alpha1.APIVersion)
	case nc.Kind != Kind:
		return fmt.Errorf("kind %q is not %s", nc.Kind, Kind)
	}
	if err := v1alpha1.ValidateName(v1alpha1.KindCluster, nc.Cluster); err != nil {
		return err
	}
	if err := v1alpha1.ValidateName(v1alpha1.KindNodeGroup, nc.NodeGroup); err != nil {
		return err
	}
	if err := checkHostName(nc.Name); err != nil {
		return err
	}
	return checkRole(nc.Role)
}

// checkHostName checks that a node's name is a host name, as it becomes the node's host name.
func checkHostName(name string) error {
	if !hostNamePattern.MatchString(name) {
		return fmt.Errorf("name %q is not a host name: 1 to 63 lower-case letters, digits and dashes, starting and "+
			"ending with a letter or digit", name)
	}
	return nil
}

// checkRole checks that r is server, client or combined.
func checkRole(r v1alpha1.Role) error {
	if !slices.Contains(v1alpha1.Roles(), r) {
		return fmt.Errorf("role %q is not server, client or combined", r)
	}
	return nil
}

func (nc *NodeConfig) checkAssets() error {
	named := make(map[string]bool, len(nc.Assets))
	for i, a := range nc.Assets {
		if a.Name == "" {
			return fmt.Errorf("assets[%d]: no name", i)
		}
		if named[a.Name] {
			return fmt.Errorf("two assets are named %s", a.Name)
		}
		named[a.Name] = true
		if err := a.check(); err != nil {
			return fmt.Errorf("asset %s: %w", a.Name, err)
		}
	}
	return nil
}

func (a Asset) check() error {
	if a.Version == "" {
		return errors.New("no version")
	}
	if len(a.URLs) == 0 {
		return errors.New("no URLs")
	}
	for i, raw := range a.URLs {
		// The error leaves the URL out: a presigned URL carries a signature.
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("URLs[%d] is not an absolute http or https URL", i)
		}
	}
	if !sha256Pattern.MatchString(a.SHA256) {
		return fmt.Errorf("sha256 %q is not 64 lower-case hex digits", a.SHA256)
	}
	return nil
}

func (nc *NodeConfig) checkFiles() error {
	paths := make(map[string]bool, len(nc.Files))
	for i, f := range nc.Files {
		if !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || f.Path == "/" {
			return fmt.Errorf("files[%d]: path %q is not absolute and clean", i, f.Path)
		}
		if paths[f.Path] {
			return fmt.Errorf("two files have the path %s", f.Path)
		}
		paths[f.Path] = true
		if err := f.check(); err != nil {
			return fmt.Errorf("file %s: %w", f.Path, err)
		}
	}
	return nil
}

func (f File) check() error {
	switch {
	case f.Mode == 0:
		return errors.New("no mode")
	case f.Mode&^0o777 != 0:
		return fmt.Errorf("mode %#o has bits beyond the permissions 0777", f.Mode)
	case f.Secret && f.Mode&0o077 != 0:
		return fmt.Errorf("mode %#o gives other users access to a secret file", f.Mode)
	case !ownerPattern.MatchString(f.Owner):
		return fmt.Errorf("owner %q is not user:group", f.Owner)
	case !utf8.Valid(f.Content):
		return errors.New("content is not valid UTF-8")
	}
	return nil
}

func (j Join) check() error {
	if j.Strategy != JoinSeedAndRefresh {
		return fmt.Errorf("join strategy %q is not %s", j.Strategy, JoinSeedAndRefresh)
	}
	for i, a := range j.Servers {
		switch {
		case !a.IsValid():
			return fmt.Errorf("join servers[%d]: not an address", i)
		case slices.Contains(j.Servers[:i], a):
			return fmt.Errorf("join servers[%d]: %s is repeated", i, a)
		}
	}
	if j.RefreshInterval < time.Second {
		return fmt.Errorf("join refresh interval %s is less than 1s", j.RefreshInterval)
	}
	return nil
}

func (s System) check() error {
	for _, key := range slices.Sorted(maps.Keys(s.Sysctls)) {
		value := s.Sysctls[key]
		switch {
		case !sysctlPattern.MatchString(key):
			return fmt.Errorf("sysctl %q is not a sysctl key", key)
		case value == "":
			return fmt.Errorf("sysctl %s: the value is empty", key)
		case strings.ContainsFunc(value, unicode.IsControl) || !utf8.ValidString(value):
			return fmt.Errorf("sysctl %s: the value has a control character or invalid UTF-8", key)
		}
	}
	for i, m := range s.KernelModules {
		switch {
		case !modulePattern.MatchString(m):
			return fmt.Errorf("kernel module %q is not a module name", m)
		case slices.Contains(s.KernelModules[:i], m):
			return fmt.Errorf("kernel module %s is repeated", m)
		}
	}
	return nil
}

func (fw HostFirewall) check() error {
	for i, r := range fw.Rules {
		if r.Name == "" {
			return fmt.Errorf("firewall rules[%d]: no name", i)
		}
		if err := r.check(); err != nil {
			// Rules share names, such as serf over tcp and over udp.
			return fmt.Errorf("firewall rule %s/%s: %w", r.Name, r.Protocol, err)
		}
	}
	if !fw.BlockMetadata.IsValid() {
		return errors.New("firewall: no metadata address to block")
	}
	return nil
}

func (r Rule) check() error {
	switch r.Protocol {
	case ProtocolTCP, ProtocolUDP:
		if err := r.Ports.check(); err != nil {
			return err
		}
	case ProtocolICMP:
		if r.Ports != (PortRange{}) {
			return errors.New("ICMP has no ports")
		}
	default:
		return fmt.Errorf("protocol %q is not tcp, udp or icmp", r.Protocol)
	}
	if len(r.From) == 0 {
		return errors.New("no sources")
	}
	for i, p := range r.From {
		if err := checkNetwork(p); err != nil {
			return fmt.Errorf("from[%d] %w", i, err)
		}
	}
	return nil
}

// check checks that the range holds at least one port and no port 0.
func (p PortRange) check() error {
	if p.First == 0 || p.First > p.Last {
		return fmt.Errorf("ports %d-%d are not a range of ports from 1 to 65535", p.First, p.Last)
	}
	return nil
}

// checkNetwork checks that p is a network: a valid prefix with no bits set after its length.
func checkNetwork(p netip.Prefix) error {
	switch {
	case !p.IsValid():
		return errors.New("is not a prefix")
	case p != p.Masked():
		return fmt.Errorf("%s has bits set after /%d", p, p.Bits())
	}
	return nil
}
