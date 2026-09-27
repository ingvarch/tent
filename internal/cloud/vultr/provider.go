package vultr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/model"
)

// Engine kinds of the objects of a cluster's infrastructure. They name the objects in plans; the marker kinds, such
// as KindSSHKey, are what a marker says an object is.
const (
	engineKindFirewallGroup = "vultr.FirewallGroup"
	engineKindVPC           = "vultr.VPC"
	engineKindSSHKey        = "vultr.SSHKey"
)

// infraKindOrder lists the engine kinds in the order to delete their objects.
var infraKindOrder = []string{engineKindFirewallGroup, engineKindVPC, engineKindSSHKey}

// Roles of firewall groups, as their markers give them. Combined node groups use the servers' group.
const (
	roleServer = "server"
	roleClient = "client"
)

// firewallGroupNames maps the role of a firewall group to the end of its key's name.
var firewallGroupNames = map[string]string{roleServer: "servers", roleClient: "clients"}

// fingerprintPattern matches the fp of an SSH key's marker: the first 8 lower-case hex digits of the key's
// fingerprint.
var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

// sshKeyKey returns the key of the cluster's SSH key whose marker has the fp fp, such as vultr.SSHKey/prod-0a1b2c3d.
func sshKeyKey(cluster, fp string) engine.Key {
	return engine.Key{Kind: engineKindSSHKey, Name: cluster + "-" + fp}
}

// vpcKey returns the key of the cluster's VPC, such as vultr.VPC/prod.
func vpcKey(cluster string) engine.Key {
	return engine.Key{Kind: engineKindVPC, Name: cluster}
}

// firewallGroupKey returns the key of the cluster's firewall group for role, roleServer or roleClient, such as
// vultr.FirewallGroup/prod-servers.
func firewallGroupKey(cluster, role string) engine.Key {
	return engine.Key{Kind: engineKindFirewallGroup, Name: cluster + "-" + firewallGroupNames[role]}
}

// Provider provisions a cluster's infrastructure on Vultr. It reaches Vultr only through an API.
type Provider struct {
	api  API
	log  *slog.Logger
	opID func() string // returns a new operation id for a create
}

var _ cloud.Provider = (*Provider)(nil)

// ProviderOption changes a default of New.
type ProviderOption func(*Provider)

// WithLogger makes the provider write its warnings to l. The default is slog.Default() at the time of New; a nil l
// keeps it.
func WithLogger(l *slog.Logger) ProviderOption {
	return func(p *Provider) {
		if l != nil {
			p.log = l
		}
	}
}

// withOpIDs makes the provider take its operation ids from next, so that tests know them. The default is newOpID.
func withOpIDs(next func() string) ProviderOption {
	return func(p *Provider) { p.opID = next }
}

// New returns a provider that calls Vultr through api.
func New(api API, opts ...ProviderOption) *Provider {
	p := &Provider{api: api, log: slog.Default(), opID: newOpID}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name returns vultr, the provider's name in specs.
func (p *Provider) Name() string { return string(v1alpha1.ProviderVultr) }

// BuildInfra returns the tasks of the cluster's infrastructure: its SSH keys, its VPC and its firewall groups, in
// that order. Each task has its own operation id. It fails for a cluster on another provider.
func (p *Provider) BuildInfra(_ context.Context, m *model.Cluster) ([]engine.Task, error) {
	if err := onVultr(m); err != nil {
		return nil, err
	}
	tasks, err := p.sshKeyTasks(m)
	if err != nil {
		return nil, err
	}
	tasks = append(tasks, p.vpcTaskOf(m))
	return append(tasks, p.firewallTasks(m)...), nil
}

// onVultr fails for a cluster on another provider.
func onVultr(m *model.Cluster) error {
	if m.Provider != v1alpha1.ProviderVultr {
		return fmt.Errorf("cluster %s runs on %q, not on vultr", m.Name, m.Provider)
	}
	return nil
}

// InfraKinds returns the kinds of the objects that the tasks of BuildInfra manage: firewall groups, VPCs and SSH
// keys, in the order to delete them.
func (p *Provider) InfraKinds() []engine.Kind {
	deleters := map[string]engine.Deleter{
		engineKindFirewallGroup: &firewallTask{api: p.api},
		engineKindVPC:           &vpcTask{api: p.api},
		engineKindSSHKey:        &sshKeyTask{api: p.api},
	}
	kinds := make([]engine.Kind, len(infraKindOrder))
	for i, name := range infraKindOrder {
		kinds[i] = engine.Kind{Name: name, Deleter: deleters[name]}
	}
	return kinds
}

// newOpID returns a random operation id: a lower-case UUID of version 4, such as
// 5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70.
func newOpID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])  // never fails: crypto/rand.Read ends the program instead
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // the variant of RFC 9562
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
