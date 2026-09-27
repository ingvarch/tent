// Package model computes what a cluster's infrastructure should be from its specs, in terms every cloud provider
// understands: the private network, the access rules of the cloud firewalls and the node groups. It holds no Nomad
// settings; providers build the infrastructure from it.
package model

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// Cluster is what a cluster's infrastructure should be, with every default filled in.
type Cluster struct {
	Name     string            // the cluster name, the prefix of every resource name
	Provider v1alpha1.Provider // the cloud the cluster runs on
	Region   string            // a Vultr region such as ams, or a Hetzner network zone such as eu-central
	Zones    []string          // the failure domains, in the spec's order; on Vultr only the region
	CIDR     netip.Prefix      // the private network
	SSHKeys  []string          // public keys installed on every node, in the spec's order
	Access   []AccessRule      // who may reach the nodes from the internet: ssh, icmp and api, in that order
	Groups   []NodeGroup       // sorted by name
}

// NodeGroup is a set of identical machines.
type NodeGroup struct {
	Name        string
	Role        v1alpha1.Role // server, client or combined; combined nodes count as servers
	MachineType string        // the provider's plan or server type
	Image       string        // the operating system image by name, such as ubuntu-24.04
	Zones       []string      // the zones the machines are spread over, a subset of the cluster's zones
	Size        int           // the number of machines
}

// Protocols of access rules.
const (
	ProtocolTCP  = "tcp"
	ProtocolICMP = "icmp"
)

// AccessRule lets traffic from the internet reach some nodes of the cluster. Cloud firewalls enforce these rules on
// the public interface; they drop all other traffic from the internet.
type AccessRule struct {
	Name     string         // ssh, icmp or api
	To       Target         // the nodes the rule opens
	Protocol string         // ProtocolTCP or ProtocolICMP
	Port     uint16         // the destination port; 0 for ICMP
	From     []netip.Prefix // the source networks, never empty: IPv4 first, then by address and length, no repeats
}

// Target is the nodes an access rule opens. The zero Target is invalid, so a rule must say which nodes it opens.
type Target int

// Targets.
const (
	// AllNodes is every node of the cluster.
	AllNodes Target = iota + 1
	// Servers is the nodes of the server and combined groups, which run Nomad servers.
	Servers
)

var targetNames = [...]string{AllNodes: "all-nodes", Servers: "servers"}

// String returns the target's name, such as servers.
func (t Target) String() string {
	if t < AllNodes || int(t) >= len(targetNames) {
		return fmt.Sprintf("Target(%d)", int(t))
	}
	return targetNames[t]
}

// Ports that access rules open.
const (
	sshPort = 22
	apiPort = 4646 // the Nomad HTTP API
)

// New computes the model of a cluster from its specs: the Cluster and all its node groups. It fills in the defaults
// on copies, so the specs stay as they are, and skips nil groups. The model shares no memory with the specs. The
// specs should have passed v1alpha1.Validate; New does not check them again, but a value it cannot parse, such as a
// malformed CIDR, is an error.
func New(c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) (*Cluster, error) {
	if c == nil {
		return nil, errors.New("no cluster to model")
	}
	c, groups = withDefaults(c, groups)
	name := c.Metadata.Name
	s := &c.Spec
	cidr, err := netip.ParsePrefix(s.Networking.CIDR)
	if err != nil {
		return nil, fmt.Errorf("cluster %s: spec.networking.cidr: %w", name, err)
	}
	access, err := accessRules(s.Access)
	if err != nil {
		return nil, fmt.Errorf("cluster %s: %w", name, err)
	}
	return &Cluster{
		Name:     name,
		Provider: s.Cloud.Provider,
		Region:   s.Cloud.Region,
		Zones:    slices.Clone(s.Cloud.Zones),
		CIDR:     cidr,
		SSHKeys:  slices.Clone(s.SSHKeys),
		Access:   access,
		Groups:   nodeGroups(groups),
	}, nil
}

// HasClients reports whether the cluster has a group with the client role, of any size. Combined groups count as
// servers, not as clients.
func (c *Cluster) HasClients() bool {
	return slices.ContainsFunc(c.Groups, func(g NodeGroup) bool { return g.Role == v1alpha1.RoleClient })
}

// withDefaults returns copies of the specs, without nil groups, with their defaults filled in. The copies share
// slices, maps and pointers with the specs; SetDefaults gives only empty fields new values, so the specs stay as they
// are.
func withDefaults(c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) (*v1alpha1.Cluster, []*v1alpha1.NodeGroup) {
	cc := *c
	gs := make([]*v1alpha1.NodeGroup, 0, len(groups))
	for _, g := range groups {
		if g != nil {
			gc := *g
			gs = append(gs, &gc)
		}
	}
	v1alpha1.SetDefaults(&cc, gs)
	return &cc, gs
}

// accessRules returns the rules that open the nodes to the internet. A rule without sources opens nothing, so it is
// left out.
func accessRules(a v1alpha1.Access) ([]AccessRule, error) {
	ssh, err := sources("spec.access.ssh", a.SSH)
	if err != nil {
		return nil, err
	}
	api, err := sources("spec.access.api", a.API)
	if err != nil {
		return nil, err
	}
	rules := []AccessRule{
		{Name: "ssh", To: AllNodes, Protocol: ProtocolTCP, Port: sshPort, From: ssh},
		{Name: "icmp", To: AllNodes, Protocol: ProtocolICMP, From: anywhere()},
		{Name: "api", To: Servers, Protocol: ProtocolTCP, Port: apiPort, From: api},
	}
	return slices.DeleteFunc(rules, func(r AccessRule) bool { return len(r.From) == 0 }), nil
}

// sources parses the CIDRs at path in the spec and sorts them: IPv4 first, then by address and length. It drops
// repeats.
func sources(path string, cidrs []string) ([]netip.Prefix, error) {
	ps := make([]netip.Prefix, 0, len(cidrs))
	for i, cidr := range cidrs {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", path, i, err)
		}
		ps = append(ps, p)
	}
	slices.SortFunc(ps, netip.Prefix.Compare)
	return slices.Compact(ps), nil
}

// anywhere is every IPv4 and IPv6 address.
func anywhere() []netip.Prefix {
	return []netip.Prefix{
		netip.PrefixFrom(netip.IPv4Unspecified(), 0),
		netip.PrefixFrom(netip.IPv6Unspecified(), 0),
	}
}

// nodeGroups returns the model of each group, sorted by name.
func nodeGroups(groups []*v1alpha1.NodeGroup) []NodeGroup {
	out := make([]NodeGroup, len(groups))
	for i, g := range groups {
		out[i] = NodeGroup{
			Name:        g.Metadata.Name,
			Role:        g.Spec.Role,
			MachineType: g.Spec.MachineType,
			Image:       g.Spec.Image,
			Zones:       slices.Clone(g.Spec.Zones),
			Size:        g.Spec.Size,
		}
	}
	slices.SortStableFunc(out, func(a, b NodeGroup) int { return strings.Compare(a.Name, b.Name) })
	return out
}
