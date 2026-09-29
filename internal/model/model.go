// Package model computes what a cluster's infrastructure should be from its specs, in terms every cloud provider
// understands. It holds the private network, the access rules of the cloud firewalls and the node groups. It also
// holds the network intents between nodes, which are the ports nodes may reach on each other and how they find the
// servers. It holds no Nomad settings. Providers build the infrastructure from it, and the node configuration is
// built from it too.
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
	Intra    []IntraRule       // the rules between nodes: nomad-http, nomad-rpc, serf and dynamic, in that order
	Join     JoinStrategy      // how nodes find the servers
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

// Protocols of the rules.
const (
	ProtocolTCP  = "tcp"
	ProtocolUDP  = "udp"
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

// IntraRule lets traffic from the cluster's private network reach some nodes of the cluster. Clouds such as Vultr and
// Hetzner do not filter private traffic, so the host firewall of each node enforces these rules.
type IntraRule struct {
	Name     string         // nomad-http, nomad-rpc, serf or dynamic; not unique: Name and Protocol identify a rule
	To       Target         // the nodes the rule opens
	Protocol string         // ProtocolTCP or ProtocolUDP
	Ports    PortRange      // the destination ports
	From     []netip.Prefix // the source networks: the cluster CIDR
}

// PortRange is the ports from First to Last, both included. A range of one port has First equal to Last.
type PortRange struct {
	First, Last uint16
}

// JoinStrategy is how Nomad agents find the servers. The zero JoinStrategy is invalid, so a model must say how.
type JoinStrategy int

// Join strategies.
const (
	// JoinSeedAndRefresh gives a new node the private addresses of the servers that exist when it is created. The
	// node then keeps the list current by asking the servers for their peers.
	JoinSeedAndRefresh JoinStrategy = iota + 1
)

var joinNames = [...]string{JoinSeedAndRefresh: "seed-and-refresh"}

// String returns the strategy's name, such as seed-and-refresh.
func (j JoinStrategy) String() string {
	return enumName(joinNames[:], "JoinStrategy", int(j))
}

// Target is the nodes a rule opens. The zero Target is invalid, so a rule must say which nodes it opens.
type Target int

// Targets.
const (
	// AllNodes is every node of the cluster.
	AllNodes Target = iota + 1
	// Servers is the nodes of the server and combined groups, which run Nomad servers.
	Servers
	// Clients is the nodes of the client and combined groups, which run Nomad clients and the workloads.
	Clients
)

var targetNames = [...]string{AllNodes: "all-nodes", Servers: "servers", Clients: "clients"}

// String returns the target's name, such as servers.
func (t Target) String() string {
	return enumName(targetNames[:], "Target", int(t))
}

// Includes reports whether the target's nodes include the nodes of the role: AllNodes includes server, client and
// combined nodes, Servers includes server and combined nodes, and Clients includes client and combined nodes.
func (t Target) Includes(role v1alpha1.Role) bool {
	switch t {
	case AllNodes:
		return role.RunsServer() || role.RunsClient()
	case Servers:
		return role.RunsServer()
	case Clients:
		return role.RunsClient()
	}
	return false
}

// enumName returns names[v], or the type's name with v, such as Target(0), when v is out of range. The value 0 has no
// name.
func enumName(names []string, typ string, v int) string {
	if v < 1 || v >= len(names) {
		return fmt.Sprintf("%s(%d)", typ, v)
	}
	return names[v]
}

// Ports that the rules open.
const (
	SSHPort      = 22
	APIPort      = 4646  // the Nomad HTTP API
	rpcPort      = 4647  // Nomad RPC
	serfPort     = 4648  // Serf gossip between servers
	dynamicFirst = 20000 // the first of Nomad's dynamic ports for workloads
	dynamicLast  = 32000 // the last of them
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
		Intra:    intraRules(cidr),
		Join:     JoinSeedAndRefresh,
		Groups:   nodeGroups(groups),
	}, nil
}

// HasClientGroup reports whether the cluster has a group with the client role, of any size. A combined group does not
// count, though the Clients target includes its nodes: it runs the servers too.
func (c *Cluster) HasClientGroup() bool {
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
		{Name: "ssh", To: AllNodes, Protocol: ProtocolTCP, Port: SSHPort, From: ssh},
		{Name: "icmp", To: AllNodes, Protocol: ProtocolICMP, From: anywhere()},
		{Name: "api", To: Servers, Protocol: ProtocolTCP, Port: APIPort, From: api},
	}
	return slices.DeleteFunc(rules, func(r AccessRule) bool { return len(r.From) == 0 }), nil
}

// DynamicPorts returns the ports that Nomad gives workloads, which the dynamic rules open to the clients.
func DynamicPorts() PortRange { return PortRange{First: dynamicFirst, Last: dynamicLast} }

// intraRules returns the rules that open the nodes to each other over the private network cidr: the Nomad HTTP API
// to every node, RPC and Serf to the servers, and the dynamic ports of workloads to the clients.
func intraRules(cidr netip.Prefix) []IntraRule {
	rule := func(name string, to Target, protocol string, first, last uint16) IntraRule {
		return IntraRule{
			Name:     name,
			To:       to,
			Protocol: protocol,
			Ports:    PortRange{First: first, Last: last},
			From:     []netip.Prefix{cidr},
		}
	}
	return []IntraRule{
		rule("nomad-http", AllNodes, ProtocolTCP, APIPort, APIPort),
		rule("nomad-rpc", Servers, ProtocolTCP, rpcPort, rpcPort),
		rule("serf", Servers, ProtocolTCP, serfPort, serfPort),
		rule("serf", Servers, ProtocolUDP, serfPort, serfPort),
		rule("dynamic", Clients, ProtocolTCP, dynamicFirst, dynamicLast),
		rule("dynamic", Clients, ProtocolUDP, dynamicFirst, dynamicLast),
	}
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
