package vultr

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

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

// Provider provisions a cluster's infrastructure and its machines on Vultr. It reaches Vultr only through an API.
type Provider struct {
	api  API
	log  *slog.Logger
	opID func() string // returns a new operation id for a create
	// pollEvery is the wait between two reads of an instance that is not ready yet, and between two sends of a create
	// that the instance limit refuses.
	pollEvery time.Duration

	mu        sync.Mutex // guards deletedAt
	deletedAt time.Time  // when Vultr last accepted a delete of a machine; zero when it has not
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

// withOpIDs makes the provider take its operation ids from next, so that tests know them. The default is
// cloud.NewOpID.
func withOpIDs(next func() string) ProviderOption {
	return func(p *Provider) { p.opID = next }
}

// pollInterval is how long the provider waits between two reads of an instance that is not ready yet, and between two
// sends of a create that the instance limit refuses.
const pollInterval = 5 * time.Second

// withPollInterval makes the provider wait d between two reads of an instance that is not ready yet, and between two
// sends of a create that the instance limit refuses. The default is pollInterval.
func withPollInterval(d time.Duration) ProviderOption {
	return func(p *Provider) { p.pollEvery = d }
}

// New returns a provider that calls Vultr through api.
func New(api API, opts ...ProviderOption) *Provider {
	p := &Provider{api: api, log: slog.Default(), opID: cloud.NewOpID, pollEvery: pollInterval}
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

// Nodes returns the provider itself: its List, Create, Stop, Delete, MarkJoined and MarkReplace are the machine
// primitives.
func (p *Provider) Nodes() cloud.Nodes { return p }

// Arch returns cloud.ArchAMD64 for every plan without a call: Vultr has no arm64 Cloud Compute plan. Validate has
// checked the plan before.
func (p *Provider) Arch(context.Context, string) (string, error) { return cloud.ArchAMD64, nil }

// InfraKinds returns the kinds of the objects that the tasks of BuildInfra manage: firewall groups, VPCs and SSH
// keys, in the order to delete them.
func (p *Provider) InfraKinds() []engine.Kind {
	deleters := map[string]engine.Deleter{
		engineKindFirewallGroup: &firewallTask{api: p.api, log: p.log},
		engineKindVPC:           &vpcTask{api: p.api},
		engineKindSSHKey:        &sshKeyTask{api: p.api},
	}
	kinds := make([]engine.Kind, len(infraKindOrder))
	for i, name := range infraKindOrder {
		kinds[i] = engine.Kind{Name: name, Deleter: deleters[name]}
	}
	return kinds
}

// limitSettle bounds how long after a delete Vultr may still count the deleted machine against the account's
// instance limit, so how long the provider sends a refused create again. In one run Vultr refused a create that came
// within 11 seconds of a delete; in another the machine that replaced a deleted one was created as fast as any
// (2026-10-06). The bound leaves room for a delete that takes longer.
const limitSettle = 2 * time.Minute

// noteDelete records that Vultr has just accepted the delete of a machine.
func (p *Provider) noteDelete() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deletedAt = time.Now()
}

// limitMayBeStale reports whether less than limitSettle has passed since the last delete that Vultr accepted, so that
// an instance limit error may come from the machine that delete removed and still counted.
func (p *Provider) limitMayBeStale() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.deletedAt.IsZero() && time.Since(p.deletedAt) < limitSettle
}
