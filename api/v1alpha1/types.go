package v1alpha1

// APIVersion is the apiVersion of every object in this package.
const APIVersion = "tent/v1alpha1"

// Kinds of the objects in this package.
const (
	KindCluster   = "Cluster"
	KindNodeGroup = "NodeGroup"
)

// TypeMeta says which kind and API version an object is.
type TypeMeta struct {
	// APIVersion is always tent/v1alpha1.
	APIVersion string `json:"apiVersion"`
	// Kind is Cluster or NodeGroup.
	Kind string `json:"kind"`
}

// ClusterMeta names a cluster.
type ClusterMeta struct {
	// Name of the cluster: 2 to 20 characters, [a-z][a-z0-9-]{0,18}[a-z0-9], and not a name Windows reserves, such
	// as con or com1. It prefixes every resource name.
	Name string `json:"name"`
}

// NodeGroupMeta names a node group and its cluster.
type NodeGroupMeta struct {
	// Name of the node group: 2 to 20 characters, [a-z][a-z0-9-]{0,18}[a-z0-9], and not a name Windows reserves,
	// such as con or com1.
	Name string `json:"name"`
	// Cluster is the name of the cluster the group belongs to.
	Cluster string `json:"cluster"`
}

// Cluster is a Nomad cluster on one cloud provider.
type Cluster struct {
	TypeMeta
	// Metadata names the cluster.
	Metadata ClusterMeta `json:"metadata"`
	// Spec is what the operator wants the cluster to be.
	Spec ClusterSpec `json:"spec"`
}

// ClusterSpec is what the operator wants the cluster to be.
type ClusterSpec struct {
	// Channel names the release channel, which sets the Nomad versions that the cluster may run and the version of the
	// CNI plugins. Defaults to stable.
	Channel string `json:"channel,omitempty"`
	// Cloud says where the cluster runs.
	Cloud Cloud `json:"cloud"`
	// Networking is the cluster's private network.
	Networking Networking `json:"networking,omitzero"`
	// Access says who may reach the nodes from the internet.
	Access Access `json:"access,omitzero"`
	// SSHKeys are public keys installed on every node, for diagnostics only.
	SSHKeys []string `json:"sshKeys,omitzero"`
	// Nomad configures the Nomad agents of the whole cluster.
	Nomad ClusterNomad `json:"nomad,omitzero"`
}

// Provider is a cloud provider.
type Provider string

// Supported providers.
const (
	ProviderVultr   Provider = "vultr"
	ProviderHetzner Provider = "hetzner"
)

// Providers lists the supported providers.
func Providers() []Provider { return []Provider{ProviderVultr, ProviderHetzner} }

// Cloud says where the cluster runs.
type Cloud struct {
	// Provider is vultr or hetzner.
	Provider Provider `json:"provider"`
	// Region is a Vultr region (ams) or a Hetzner network zone (eu-central).
	Region string `json:"region"`
	// Zones are the failure domains. Vultr has none: left out, zones is [region], and any other value is an error. On
	// Hetzner they are locations such as fsn1 and must be given.
	Zones []string `json:"zones,omitzero"`
	// Vultr holds Vultr settings. Set it only when the provider is vultr.
	Vultr *VultrCloud `json:"vultr,omitempty"`
	// Hetzner holds Hetzner settings. Set it only when the provider is hetzner.
	Hetzner *HetznerCloud `json:"hetzner,omitempty"`
}

// VultrCloud holds Vultr settings of a cluster. There are none yet.
type VultrCloud struct{}

// HetznerCloud holds Hetzner settings of a cluster. There are none yet.
type HetznerCloud struct{}

// Networking is the cluster's private network.
type Networking struct {
	// CIDR of the private network, a private IPv4 prefix. Defaults to 10.64.0.0/16.
	CIDR string `json:"cidr,omitempty"`
}

// Access says who may reach the nodes from the internet.
type Access struct {
	// SSH lists the CIDRs allowed to reach port 22. Empty closes SSH on the cloud firewall.
	SSH []string `json:"ssh,omitzero"`
	// API lists the CIDRs allowed to reach the Nomad API on port 4646. Left out, it is 0.0.0.0/0: mTLS and ACLs protect
	// the API, and tent warns while it is open to everyone. An empty list is an error, because tent needs the API.
	API []string `json:"api,omitzero"`
}

// ClientIntroduction is how strictly Nomad servers require intro tokens from new clients.
type ClientIntroduction string

// Client introduction modes.
const (
	ClientIntroductionStrict ClientIntroduction = "strict"
	ClientIntroductionWarn   ClientIntroduction = "warn"
	ClientIntroductionNone   ClientIntroduction = "none"
)

// ClientIntroductions lists the client introduction modes.
func ClientIntroductions() []ClientIntroduction {
	return []ClientIntroduction{ClientIntroductionStrict, ClientIntroductionWarn, ClientIntroductionNone}
}

// ClusterNomad configures the Nomad agents of the whole cluster.
type ClusterNomad struct {
	// Version of Nomad, for example 2.0.7. Empty means the version that the cluster was first built with; a new cluster
	// gets the one that the channel recommends.
	Version string `json:"version,omitempty"`
	// Region is the Nomad region. Defaults to global.
	Region string `json:"region,omitempty"`
	// TLS configures mTLS on the Nomad API.
	TLS TLS `json:"tls,omitzero"`
	// ClientIntroduction is strict, warn or none. Defaults to strict, or to warn when the cluster has a combined group;
	// strict is an error with a combined group, whose client registers before intro tokens exist.
	ClientIntroduction ClientIntroduction `json:"clientIntroduction,omitempty"`
	// ExtraConfig is HCL added to the agents as 99-user.hcl. It is not validated or supported.
	ExtraConfig ExtraConfig `json:"extraConfig,omitzero"`
}

// TLS configures mTLS on the Nomad API.
type TLS struct {
	// VerifyHTTPSClient requires client certificates on the HTTP API. Defaults to true; tent ui keeps the browser
	// working either way.
	VerifyHTTPSClient *bool `json:"verifyHTTPSClient,omitempty"`
}

// ExtraConfig is HCL added to the agents as 99-user.hcl.
type ExtraConfig struct {
	// Server is added to server and combined nodes.
	Server string `json:"server,omitempty"`
	// Client is added to client and combined nodes.
	Client string `json:"client,omitempty"`
}

// NodeGroup is a set of identical nodes in one cluster.
type NodeGroup struct {
	TypeMeta
	// Metadata names the node group and its cluster.
	Metadata NodeGroupMeta `json:"metadata"`
	// Spec is what the operator wants the node group to be.
	Spec NodeGroupSpec `json:"spec"`
}

// Role is what the nodes of a group run.
type Role string

// Node group roles.
const (
	// RoleServer nodes run Nomad servers only.
	RoleServer Role = "server"
	// RoleClient nodes run Nomad clients and the workloads.
	RoleClient Role = "client"
	// RoleCombined nodes run a server and a client in one agent, for dev and small clusters.
	RoleCombined Role = "combined"
)

// Roles lists the node group roles.
func Roles() []Role { return []Role{RoleServer, RoleClient, RoleCombined} }

// NodeGroupSpec is what the operator wants the node group to be.
type NodeGroupSpec struct {
	// Role is server, client or combined.
	Role Role `json:"role"`
	// MachineType is the provider's plan or server type, for example vc2-2c-4gb on Vultr or cx23 on Hetzner.
	MachineType string `json:"machineType"`
	// Image is the operating system image by name. Defaults to ubuntu-24.04.
	Image string `json:"image,omitempty"`
	// Size is the number of nodes. A server or combined group has 1, 3 or 5; a client group has 0 or more.
	Size int `json:"size"`
	// Zones the nodes are spread over, a subset of the cluster zones. Defaults to all cluster zones.
	Zones []string `json:"zones,omitzero"`
	// Nomad configures the Nomad clients of the group. Server groups leave it empty.
	Nomad NodeGroupNomad `json:"nomad,omitzero"`
}

// NodeGroupNomad configures the Nomad clients of a group.
type NodeGroupNomad struct {
	// NodePool of the clients. Defaults to the node pool named default for client and combined groups.
	NodePool string `json:"nodePool,omitempty"`
	// NodeClass of the clients.
	NodeClass string `json:"nodeClass,omitempty"`
	// Drivers are the task drivers the clients enable, for example docker and exec.
	Drivers []string `json:"drivers,omitzero"`
	// Meta is client metadata that jobs can use in constraints.
	Meta map[string]string `json:"meta,omitzero"`
}
