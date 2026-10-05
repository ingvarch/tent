package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/buildinfo"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/spec"
)

// joinRefresh is how often tent-node asks the servers for their peers.
const joinRefresh = time.Minute

// metadataAddr is the address of the metadata service on Vultr and Hetzner, which serves a node's user data.
var metadataAddr = netip.AddrFrom4([4]byte{169, 254, 169, 254})

// publicRules are the host firewall's rules for traffic from the internet, the ports of the model's access rules. The
// host opens them to every source: the cloud firewall filters the sources by spec.access, so a change of access leaves
// the nodes as they are. The model leaves out an access rule without sources, so the host cannot take them from it.
var publicRules = []struct {
	name     string
	to       model.Target
	protocol string
	port     uint16 // 0 for ICMP
}{
	{"ssh", model.AllNodes, nodeconfig.ProtocolTCP, model.SSHPort},
	{"icmp", model.AllNodes, nodeconfig.ProtocolICMP, 0},
	{"api", model.Servers, nodeconfig.ProtocolTCP, model.APIPort},
}

// The bridges where workloads run on a node that runs a client.
var (
	nomadBridge  = netip.MustParsePrefix("172.26.64.0/20") // Nomad's default bridge_network_subnet
	dockerBridge = netip.MustParsePrefix("172.17.0.0/16")  // Docker's default bridge
)

// bridgeRules are the host firewall's rules for workloads on the bridges. A workload that calls the local agent's HTTP
// API, or a task with host networking on a dynamic port, at the node's address reaches the node itself.
var bridgeRules = []struct {
	name     string
	protocol string
	ports    model.PortRange
}{
	{"bridge-http", nodeconfig.ProtocolTCP, model.PortRange{First: model.APIPort, Last: model.APIPort}},
	{"bridge-dynamic", nodeconfig.ProtocolTCP, model.DynamicPorts()},
	{"bridge-dynamic", nodeconfig.ProtocolUDP, model.DynamicPorts()},
}

// nodeAssets are the files that nodes download.
type nodeAssets struct {
	nomad, cni, tentNode nodeconfig.Asset
}

// forRole returns the files that a node of the role downloads, in the order nomad, cni-plugins, tent-node. Only a node
// that runs a client downloads the CNI plugins: a server runs no workloads, so a new CNI release leaves it as it is.
func (a nodeAssets) forRole(role v1alpha1.Role) []nodeconfig.Asset {
	if role.RunsClient() {
		return []nodeconfig.Asset{a.nomad, a.cni, a.tentNode}
	}
	return []nodeconfig.Asset{a.nomad, a.tentNode}
}

// resolveAssets returns the files that the nodes download for linux on arch: Nomad of nomadVersion, whose sha256
// comes from its signed SHA256SUMS; the CNI plugins that the channel pins; and the tent-node of tent of tentVersion.
// It reads the release files with one request each.
func resolveAssets(ctx context.Context, opts assets.Options, ch *channels.Channel, nomadVersion, tentVersion,
	arch string,
) (nodeAssets, error) {
	nomad, err := assets.Nomad(ctx, opts, nomadVersion, arch)
	if err != nil {
		return nodeAssets{}, fmt.Errorf("find Nomad %s: %w", nomadVersion, err)
	}
	cni, err := assets.CNI(ch, arch)
	if err != nil {
		return nodeAssets{}, fmt.Errorf("find the CNI plugins: %w", err)
	}
	tentNode, err := assets.TentNode(ctx, opts, tentVersion, arch)
	if err != nil {
		return nodeAssets{}, fmt.Errorf("find tent-node: %w", err)
	}
	return nodeAssets{
		nomad: nodeconfig.Asset(nomad), cni: nodeconfig.Asset(cni), tentNode: nodeconfig.Asset(tentNode),
	}, nil
}

// devVariablesWarning returns the warning about TENT_NODE_URL or TENT_NODE_SHA256 set for a release build of tent of
// version, which ignores them, or "" when there is nothing to warn about. It names the variables, never their values.
func devVariablesWarning(version string, opts assets.Options) string {
	if !buildinfo.IsRelease(version) {
		return ""
	}
	var set []string
	if opts.DevURL != "" {
		set = append(set, "TENT_NODE_URL")
	}
	if opts.DevSHA256 != "" {
		set = append(set, "TENT_NODE_SHA256")
	}
	verb, pronoun := "is", "it"
	switch len(set) {
	case 0:
		return ""
	case 2:
		verb, pronoun = "are", "them"
	}
	return fmt.Sprintf("%s %s set, but tent %s is a release build and ignores %s: its nodes download the tent-node "+
		"of release %s", english.And(set), verb, version, pronoun, version)
}

// groupSpecs indexes the specs in groups by the group's name and skips nil ones, as model.New does.
func groupSpecs(groups []*v1alpha1.NodeGroup) map[string]*v1alpha1.NodeGroup {
	specs := make(map[string]*v1alpha1.NodeGroup, len(groups))
	for _, g := range groups {
		if g != nil {
			specs[g.Metadata.Name] = g
		}
	}
	return specs
}

// groupTemplates returns the NodeConfig of each node group of the model m, by the group's name: what every node of the
// group has, with the group's spec hash. c and groups are the specs of m with their defaults, as the completed spec
// holds them. A template has the cluster's provider, the Nomad agent configuration that the specs describe, the CA
// bundle, the assets that its role downloads, which downloads holds for each group by its name, the join strategy,
// the system settings and the host firewall; nodeConfig adds what one node has.
// An empty CA bundle is an error, as is an empty gossip key for a group that runs servers.
func groupTemplates(m *model.Cluster, c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup,
	downloads map[string]nodeAssets, gossip pki.Secret, caBundle []byte,
) (map[string]nodeconfig.NodeConfig, error) {
	n := c.Spec.Nomad
	if len(caBundle) == 0 {
		return nil, fmt.Errorf("%s: no CA bundle", clusterLabel(m.Name))
	}
	if n.TLS.VerifyHTTPSClient == nil {
		return nil, fmt.Errorf("%s: spec.nomad.tls.verifyHTTPSClient is not set", clusterLabel(m.Name))
	}
	strategy, err := joinStrategy(m.Join)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", clusterLabel(m.Name), err)
	}
	specs := groupSpecs(groups)
	tmpls := make(map[string]nodeconfig.NodeConfig, len(m.Groups))
	for _, g := range m.Groups {
		gs, ok := specs[g.Name]
		if !ok {
			return nil, fmt.Errorf("node group %s: no spec", g.Name)
		}
		gn := gs.Spec.Nomad
		groupAssets, ok := downloads[g.Name]
		if !ok {
			return nil, fmt.Errorf("node group %s: no assets", g.Name)
		}
		files, err := nodeconfig.RenderAgent(nodeconfig.Agent{
			Role: g.Role, Cluster: m.Name, Group: g.Name, Region: n.Region, CIDR: m.CIDR,
			ClientIntroduction: n.ClientIntroduction, Gossip: gossip, VerifyHTTPSClient: *n.TLS.VerifyHTTPSClient,
			NodePool: gn.NodePool, NodeClass: gn.NodeClass, Drivers: gn.Drivers, Meta: gn.Meta,
			DynamicPorts: nodeconfig.PortRange(model.DynamicPorts()),
			ExtraServer:  n.ExtraConfig.Server, ExtraClient: n.ExtraConfig.Client,
		})
		if err != nil {
			return nil, fmt.Errorf("node group %s: %w", g.Name, err)
		}
		// The CA bundle holds no secret; it is in the spec hash, so a new CA marks every node out of date.
		files = append(files,
			nodeconfig.File{Path: nodeconfig.CAFile, Mode: 0o644, Owner: nodeconfig.Owner, Content: caBundle})
		// The unit is group-level too: a change of it marks every node of the group out of date.
		files = append(files, nodeconfig.RenderNomadService())
		tmpl := nodeconfig.NodeConfig{
			APIVersion: v1alpha1.APIVersion,
			Kind:       nodeconfig.Kind,
			Cluster:    m.Name,
			Provider:   m.Provider,
			NodeGroup:  g.Name,
			Role:       g.Role,
			Region:     n.Region,
			Assets:     groupAssets.forRole(g.Role),
			Files:      files,
			Join:       nodeconfig.Join{Strategy: strategy, RefreshInterval: joinRefresh},
			System:     nodeSystem(g.Role, gn.Drivers),
			Firewall:   hostFirewall(m.Intra, g.Role),
		}
		tmpl.SpecHash = nodeconfig.SpecHash(&tmpl)
		tmpls[g.Name] = tmpl
	}
	return tmpls, nil
}

// joinStrategy returns the NodeConfig name of the model's join strategy.
func joinStrategy(j model.JoinStrategy) (string, error) {
	if j != model.JoinSeedAndRefresh {
		return "", fmt.Errorf("join strategy %s is not one that nodes know", j)
	}
	return nodeconfig.JoinSeedAndRefresh, nil
}

// nodeSystem returns how a node of the role sets up its operating system. A node that runs a client loads the kernel
// module of bridge networking and passes bridged traffic through the firewall, as Nomad's bridge networking needs. It
// installs Docker, and loads the overlay module of Docker's storage, unless the group's drivers leave the docker
// driver out; an empty list keeps all of Nomad's built-in drivers, Docker among them. A server runs no workloads and
// needs none of it.
func nodeSystem(role v1alpha1.Role, drivers []string) nodeconfig.System {
	if !role.RunsClient() {
		return nodeconfig.System{}
	}
	s := nodeconfig.System{
		Sysctls: map[string]string{
			"net.bridge.bridge-nf-call-arptables": "1",
			"net.bridge.bridge-nf-call-ip6tables": "1",
			"net.bridge.bridge-nf-call-iptables":  "1",
		},
		KernelModules: []string{"br_netfilter"},
		Docker:        len(drivers) == 0 || slices.Contains(drivers, "docker"),
	}
	if s.Docker {
		s.KernelModules = append(s.KernelModules, "overlay")
	}
	return s
}

// hostFirewall returns the host firewall of a node of the role: the public rules, then the rules between nodes, each
// when it reaches the role, then the bridge rules on a node that runs a client, and the metadata service blocked for
// workloads.
func hostFirewall(intra []model.IntraRule, role v1alpha1.Role) nodeconfig.HostFirewall {
	var rules []nodeconfig.Rule
	for _, r := range publicRules {
		if r.to.Includes(role) {
			rules = append(rules, nodeconfig.Rule{
				Name: r.name, Protocol: r.protocol, Ports: nodeconfig.PortRange{First: r.port, Last: r.port},
				From: []netip.Prefix{
					netip.PrefixFrom(netip.IPv4Unspecified(), 0), netip.PrefixFrom(netip.IPv6Unspecified(), 0),
				},
			})
		}
	}
	for _, r := range intra {
		if r.To.Includes(role) {
			rules = append(rules, nodeconfig.Rule{
				Name: r.Name, Protocol: r.Protocol, Ports: nodeconfig.PortRange(r.Ports), From: slices.Clone(r.From),
			})
		}
	}
	if role.RunsClient() {
		for _, r := range bridgeRules {
			rules = append(rules, nodeconfig.Rule{
				Name: r.name, Protocol: r.protocol, Ports: nodeconfig.PortRange(r.ports),
				From: []netip.Prefix{nomadBridge, dockerBridge},
			})
		}
	}
	return nodeconfig.HostFirewall{Rules: rules, BlockMetadata: metadataAddr}
}

// NewNode is a node that tent is about to create, with everything that its NodeConfig is made from.
type NewNode struct {
	// Specs are the cluster's specs with their defaults and the Nomad version pinned, as the completed spec holds them.
	Specs spec.Objects
	// Channel is the cluster's release channel, which pins the CNI plugins.
	Channel *channels.Channel
	// Assets say where Nomad and tent-node are found. The node downloads the tent-node of tent of TentVersion, and
	// everything for linux on Arch.
	Assets      assets.Options
	TentVersion string
	Arch        string
	// The cluster's gossip key, a secret, and its CA bundle, which holds certificates alone.
	Gossip   pki.Secret
	CABundle []byte
	// The node: its group's name; its name, which is also its host name; its zone; its certificate and key; the
	// servers it joins first; and an intro token, which only a node that runs a client takes.
	Group string
	Name  string
	Zone  string
	Cert  pki.Certificate
	Seed  []netip.Addr
	Intro pki.Secret
}

// NodeConfigOf returns the NodeConfig of the new node n: the template of its group, with the assets that the
// channel, the pinned Nomad version and the tent version give, and the node's own parts. The specs give the number of
// servers. It sends no request when the specs lack the version or the group. Its errors are those of its steps: the
// specs' and the template's name the cluster, when there is one, or the group; the node's parts' name the node; the
// assets' name the asset and the URL they read.
// update must build a node's config with the same steps, or through this function, so that what the tools that check
// tent-node give a machine is what tent gives a node.
func NodeConfigOf(ctx context.Context, n NewNode) (*nodeconfig.NodeConfig, error) {
	c, groups := n.Specs.Cluster, n.Specs.NodeGroups
	m, err := model.New(c, groups)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(m.Groups, func(g model.NodeGroup) bool { return g.Name == n.Group }) {
		return nil, fmt.Errorf("node group %s: not in the specs", n.Group)
	}
	archs := make(map[string]string, len(m.Groups))
	for _, g := range m.Groups {
		archs[g.Name] = n.Arch
	}
	b, err := newNodeBuilder(ctx, builderInput{
		Specs: n.Specs, Channel: n.Channel, Assets: n.Assets, TentVersion: n.TentVersion, Archs: archs,
		Gossip: n.Gossip, CABundle: n.CABundle,
	}, nil)
	if err != nil {
		return nil, err
	}
	return b.node(n.Group, n.Name, n.Zone, n.Cert, n.Seed, n.Intro)
}

// assetKey names the files that one architecture's nodes download for one Nomad and one tent.
type assetKey struct{ nomadVersion, tentVersion, arch string }

// assetCache keeps the files that nodeBuilders found, so that builders of one run read each release's files once.
type assetCache map[assetKey]nodeAssets

// builderInput is what a nodeBuilder is made from.
type builderInput struct {
	// Specs are the cluster's specs with their defaults and the Nomad version pinned, as the completed spec holds them.
	Specs spec.Objects
	// Channel is the cluster's release channel, which pins the CNI plugins.
	Channel *channels.Channel
	// Assets say where Nomad and tent-node are found; the nodes download the tent-node of tent of TentVersion.
	Assets      assets.Options
	TentVersion string
	// Archs gives the CPU architecture of each node group's machines, by the group's name.
	Archs map[string]string
	// The cluster's gossip key, a secret, and its CA bundle, which holds certificates alone.
	Gossip   pki.Secret
	CABundle []byte
}

// nodeBuilder makes the NodeConfig of the nodes of one cluster from the template of each group.
type nodeBuilder struct {
	templates map[string]nodeconfig.NodeConfig
	pools     map[string]string
	// servers is the number of Nomad servers that the specs give, which every server waits for before it bootstraps.
	servers int
}

// newNodeBuilder returns a builder for the cluster of in. It reads the release files of each architecture once, or
// takes them from cache, which may be nil. A group without an architecture is an error before any request.
func newNodeBuilder(ctx context.Context, in builderInput, cache assetCache) (*nodeBuilder, error) {
	c, groups := in.Specs.Cluster, in.Specs.NodeGroups
	m, err := model.New(c, groups)
	if err != nil {
		return nil, err
	}
	version := c.Spec.Nomad.Version
	if version == "" {
		return nil, fmt.Errorf("%s: spec.nomad.version is not set", clusterLabel(m.Name))
	}
	for _, g := range m.Groups {
		if in.Archs[g.Name] == "" {
			return nil, fmt.Errorf("node group %s: no architecture", g.Name)
		}
	}
	if cache == nil {
		cache = assetCache{}
	}
	downloads := make(map[string]nodeAssets, len(m.Groups))
	for _, g := range m.Groups {
		key := assetKey{nomadVersion: version, tentVersion: in.TentVersion, arch: in.Archs[g.Name]}
		found, ok := cache[key]
		if !ok {
			found, err = resolveAssets(ctx, in.Assets, in.Channel, version, in.TentVersion, key.arch)
			if err != nil {
				return nil, err
			}
			cache[key] = found
		}
		downloads[g.Name] = found
	}
	tmpls, err := groupTemplates(m, c, groups, downloads, in.Gossip, in.CABundle)
	if err != nil {
		return nil, err
	}
	b := &nodeBuilder{templates: tmpls, pools: make(map[string]string, len(groups))}
	for name, g := range groupSpecs(groups) {
		b.pools[name] = g.Spec.Nomad.NodePool
	}
	b.servers = clusterServers(m)
	return b, nil
}

// node returns the NodeConfig of the node called name in zone of group, as nodeConfig makes it, with the number of
// servers of the builder. A group that the builder lacks is an error.
func (b *nodeBuilder) node(group, name, zone string, cert pki.Certificate, seed []netip.Addr, intro pki.Secret,
) (*nodeconfig.NodeConfig, error) {
	tmpl, ok := b.templates[group]
	if !ok {
		return nil, fmt.Errorf("node group %s: not in the specs", group)
	}
	return nodeConfig(tmpl, name, zone, b.servers, cert, seed, intro)
}

// specHash returns the spec hash of the nodes of group, "" for a group that the builder lacks.
func (b *nodeBuilder) specHash(group string) string { return b.templates[group].SpecHash }

// role returns the role of the nodes of group, "" for a group that the builder lacks.
func (b *nodeBuilder) role(group string) v1alpha1.Role { return b.templates[group].Role }

// nodePool returns the Nomad node pool of the nodes of group, "" for a group that the builder lacks.
func (b *nodeBuilder) nodePool(group string) string { return b.pools[group] }

// nodeConfig returns the NodeConfig of the node called name in zone, from the template of its group: the template with
// the node's name, its 10-node.hcl, its certificate and key, and the seed of servers to join. A node that runs a
// client gets its intro token too, when intro holds one; a server takes none. bootstrapExpect is the number of servers,
// which a client ignores. The node's own parts leave the spec hash as it is. An empty certificate or key, and a config
// that does not validate, are errors.
func nodeConfig(tmpl nodeconfig.NodeConfig, name, zone string, bootstrapExpect int, cert pki.Certificate,
	seed []netip.Addr, intro pki.Secret,
) (*nodeconfig.NodeConfig, error) {
	nc, err := withNode(tmpl, name, zone, bootstrapExpect, cert, seed, intro)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", name, err)
	}
	return nc, nil
}

// withNode returns nc, a copy of the template, with the parts of the node called name, as nodeConfig says. Its errors
// do not name the node; nodeConfig adds the name.
func withNode(nc nodeconfig.NodeConfig, name, zone string, bootstrapExpect int, cert pki.Certificate,
	seed []netip.Addr, intro pki.Secret,
) (*nodeconfig.NodeConfig, error) {
	if len(cert.Cert) == 0 || len(cert.Key) == 0 {
		return nil, errors.New("no certificate or key")
	}
	if len(intro) > 0 && !nc.Role.RunsClient() {
		return nil, errors.New("a server takes no intro token")
	}
	node, err := nodeconfig.RenderNode(name, zone, nc.Role, bootstrapExpect)
	if err != nil {
		return nil, err
	}
	// A copy, so that nodes of one template share none of their own files.
	nc.Files = append(slices.Clone(nc.Files), node,
		nodeconfig.File{Path: nodeconfig.CertFile, Mode: 0o644, Owner: nodeconfig.Owner, Content: cert.Cert,
			PerNode: true},
		nodeconfig.File{Path: nodeconfig.KeyFile, Mode: 0o600, Owner: nodeconfig.Owner, Content: cert.Key,
			PerNode: true, Secret: true},
	)
	if len(intro) > 0 {
		nc.Files = append(nc.Files, nodeconfig.File{Path: nodeconfig.IntroTokenFile, Mode: 0o600,
			Owner: nodeconfig.Owner, Content: intro, PerNode: true, Secret: true})
	}
	nc.Name, nc.Join.Servers = name, slices.Clone(seed)
	if err := nc.Validate(); err != nil {
		return nil, err
	}
	return &nc, nil
}

// standInIntroTokenSize is the size of the intro token that the checks of a plan use: about three times that of a real
// token, which is under 1 KiB for a node name of 30 characters.
const standInIntroTokenSize = 2048

// standInIntroToken returns text of standInIntroTokenSize bytes that is shaped like an intro token, three base64url
// parts, and that gzip shrinks no more than a real one. It is the same every time.
func standInIntroToken() pki.Secret {
	raw := make([]byte, standInIntroTokenSize)
	_, _ = rand.NewChaCha8([32]byte{}).Read(raw) // never fails
	text := base64.RawURLEncoding.EncodeToString(raw)[:standInIntroTokenSize-2]
	third := len(text) / 3
	return pki.Secret(text[:third] + "." + text[third:2*third] + "." + text[2*third:])
}

// assetOptions returns the options that find the release files that nodes download: the service's, with its clock
// when they have none.
func (s *Service) assetOptions() assets.Options {
	opts := s.Assets
	if opts.Now == nil {
		opts.Now = s.now
	}
	return opts
}

// planBuilder returns the builder of the NodeConfig of the nodes of the cluster m, whose specs are objs and whose
// channel is ch, with the run's secrets. It asks the provider p for the architecture of each node group's machines and
// reads the release files, or takes them from cache.
func (s *Service) planBuilder(ctx context.Context, p cloud.Provider, m *model.Cluster, objs spec.Objects,
	ch *channels.Channel, secrets clusterSecrets, cache assetCache,
) (*nodeBuilder, error) {
	archs := make(map[string]string, len(m.Groups))
	byType := map[string]string{}
	for _, g := range m.Groups {
		arch, ok := byType[g.MachineType]
		if !ok {
			var err error
			if arch, err = p.Arch(ctx, g.MachineType); err != nil {
				return nil, fmt.Errorf("architecture of %s: %w", g.MachineType, err)
			}
			byType[g.MachineType] = arch
		}
		archs[g.Name] = arch
	}
	return newNodeBuilder(ctx, builderInput{
		Specs: objs, Channel: ch, Assets: s.assetOptions(), TentVersion: s.Version, Archs: archs,
		Gossip: secrets.gossip, CABundle: secrets.ca.Bundle(),
	}, cache)
}
