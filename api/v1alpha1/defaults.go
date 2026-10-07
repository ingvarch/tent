package v1alpha1

import "slices"

// Defaults of fields the operator leaves empty.
const (
	DefaultChannel     = "stable"       // spec.channel
	DefaultCIDR        = "10.64.0.0/16" // spec.networking.cidr
	DefaultAPISource   = "0.0.0.0/0"    // spec.access.api
	DefaultNomadRegion = "global"       // spec.nomad.region
	DefaultImage       = "ubuntu-24.04" // NodeGroup spec.image
	DefaultNodePool    = "default"      // NodeGroup spec.nomad.nodePool

	DefaultMaxSurge       = 1    // NodeGroup spec.rollingUpdate.maxSurge of a client group
	DefaultMaxUnavailable = 0    // NodeGroup spec.rollingUpdate.maxUnavailable of a client group
	DefaultDrainTimeout   = "1h" // NodeGroup spec.rollingUpdate.drainTimeout of a client or combined group
)

// SetDefaults fills the empty fields of a cluster and its node groups in place. groups must be all node groups of
// the cluster. Calling it twice changes nothing more. It knows only static rules; defaults that need the provider's
// API come from the provider.
func SetDefaults(c *Cluster, groups []*NodeGroup) {
	if c == nil {
		return
	}
	setClusterDefaults(&c.Spec, groups)
	for _, g := range groups {
		if g != nil {
			setGroupDefaults(&g.Spec, c.Spec.Cloud.Zones)
		}
	}
}

func setClusterDefaults(s *ClusterSpec, groups []*NodeGroup) {
	setIfEmpty(&s.Channel, DefaultChannel)
	// Vultr has no zones, so its only zone is the region.
	if len(s.Cloud.Zones) == 0 && s.Cloud.Provider == ProviderVultr {
		s.Cloud.Zones = []string{s.Cloud.Region}
	}
	setIfEmpty(&s.Networking.CIDR, DefaultCIDR)
	// Only a left-out list gets the default: an explicit empty one stays for Validate to reject.
	if s.Access.API == nil {
		s.Access.API = []string{DefaultAPISource}
	}
	setIfEmpty(&s.Nomad.Region, DefaultNomadRegion)
	setIfNil(&s.Nomad.TLS.VerifyHTTPSClient, true)
	setIfEmpty(&s.Nomad.ClientIntroduction, defaultClientIntroduction(groups))
}

// defaultClientIntroduction is warn when a group is combined, because its client registers before intro tokens
// exist, and strict otherwise.
func defaultClientIntroduction(groups []*NodeGroup) ClientIntroduction {
	for _, g := range groups {
		if g != nil && g.Spec.Role == RoleCombined {
			return ClientIntroductionWarn
		}
	}
	return ClientIntroductionStrict
}

func setGroupDefaults(s *NodeGroupSpec, clusterZones []string) {
	setIfEmpty(&s.Image, DefaultImage)
	if len(s.Zones) == 0 {
		s.Zones = slices.Clone(clusterZones)
	}
	if s.Role.RunsClient() {
		setIfEmpty(&s.Nomad.NodePool, DefaultNodePool)
		setIfEmpty(&s.RollingUpdate.DrainTimeout, DefaultDrainTimeout)
	}
	if s.Role == RoleClient {
		setIfNil(&s.RollingUpdate.MaxSurge, DefaultMaxSurge)
		setIfNil(&s.RollingUpdate.MaxUnavailable, DefaultMaxUnavailable)
	}
}

// setIfEmpty sets *v to def when *v is the zero value.
func setIfEmpty[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

// setIfNil points *v at a copy of def when *v is nil, so that a set 0 stays.
func setIfNil[T any](v **T, def T) {
	if *v == nil {
		*v = &def
	}
}
