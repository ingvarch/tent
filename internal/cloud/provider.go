package cloud

import (
	"context"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/model"
)

// Provider is a cloud that tent provisions clusters on. The core reaches a cloud only through it. It has the methods
// that checking specs, building or deleting a cluster's infrastructure and managing its machines need; the methods
// for server discovery, user data and capabilities join it with the code that first uses them.
type Provider interface {
	// Name returns the provider's name as specs give it, such as vultr.
	Name() string
	// Validate checks specs that passed v1alpha1.Validate against the cloud's live API: for example, that the region
	// exists, that the machine types can be deployed there and that the images exist.
	Validate(ctx context.Context, c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup) error
	// BuildInfra returns the engine tasks that make the cluster's infrastructure, such as its network, firewalls and
	// SSH keys, match the model. Nodes are not part of it.
	BuildInfra(ctx context.Context, m *model.Cluster) ([]engine.Task, error)
	// InfraKinds returns every kind of object that the infrastructure tasks can manage, in the order to delete them,
	// whether or not the model asks for one, so the engine can delete an object the specs no longer ask for.
	InfraKinds() []engine.Kind
	// Inventory lists every object the cluster owns in the cloud. Tasks read it through the provider's own snapshot
	// type.
	Inventory(ctx context.Context, cluster string) (engine.Snapshot, error)
	// Nodes returns the primitives that list, create, stop and delete machines and scrub their user data.
	Nodes() Nodes
}
