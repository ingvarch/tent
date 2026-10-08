package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
)

// scrubTimeout is how long the scrub of one node may take.
const scrubTimeout = 5 * time.Minute

// nodeKit is what creating the nodes of a cluster needs: the cloud's primitives, the cluster's secrets, its Nomad
// region and the builder of the nodes' NodeConfig.
type nodeKit struct {
	cluster string
	region  string // the Nomad region
	nodes   cloud.Nodes
	secrets clusterSecrets
	// builder makes the NodeConfig of the nodes that the run creates, or waits for with an operation id; it is nil when
	// the run has none.
	builder *nodeBuilder
}

// joinPoint is where a new node finds the servers that it joins, and the API that issues its intro token.
type joinPoint interface {
	// seed returns the private addresses of the servers that the node called name joins; client is true for a node
	// that runs no server.
	seed(name string, client bool) ([]netip.Addr, error)
	// nomadAPI returns the API over the servers.
	nomadAPI() (nomadops.API, error)
}

// userData returns the user data of the node that the change c creates or waits for: the NodeConfig of its group with
// the node's name, a certificate issued at now, the seed and the intro token.
func (k nodeKit) userData(c NodeChange, now time.Time, seed []netip.Addr, intro pki.Secret) (cloud.UserData, error) {
	cert, err := k.secrets.ca.IssueNode(k.builder.role(c.Group), k.region, now)
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", c.Name, err)
	}
	nc, err := k.builder.node(c.Group, c.Name, c.Zone, cert, seed, intro)
	if err != nil {
		return nil, err
	}
	data, err := nodeconfig.UserData(nc)
	if err != nil {
		return nil, err
	}
	return cloud.UserData(data), nil
}

// nomad returns what the run needs to call the cluster's servers.
func (k nodeKit) nomad() nomadAccess {
	return nomadAccess{cluster: k.cluster, region: k.region, secrets: k.secrets}
}

// bootClient creates the client node of c, or repeats its create with c's operation id, with an intro token and the
// seed of j, and returns its machine.
func (s *Service) bootClient(ctx context.Context, k nodeKit, j joinPoint, c NodeChange) (cloud.Instance, error) {
	return s.applyNodeWith(ctx, k.nodes, k.cluster, c, func(ctx context.Context) (cloud.UserData, error) {
		api, err := j.nomadAPI()
		if err != nil {
			return nil, err
		}
		seed, err := j.seed(c.Name, true)
		if err != nil {
			return nil, err
		}
		intro, err := api.IntroToken(ctx, nomadops.IntroRequest{
			NodeName: c.Name, NodePool: k.builder.nodePool(c.Group), TTL: nomadops.MaxIntroTTL,
		})
		if err != nil {
			return nil, fmt.Errorf("intro token for node %s: %w", c.Name, err)
		}
		return k.userData(c, s.now(), seed, intro)
	})
}

// markJoined replaces the user data of the machine in with the stub and labels the machine as joined, and reports the
// scrub as a step. A failure stops the run with the provider's error; the next run plans the wait again.
func (s *Service) markJoined(ctx context.Context, k nodeKit, in cloud.Instance) error {
	step := NodeChange{Action: NodeScrub, Name: in.Name, ID: in.ID}
	s.progress(Progress{Node: step, Step: NodeStarted})
	ctx, cancel := context.WithTimeout(ctx, scrubTimeout)
	defer cancel()
	if err := k.nodes.MarkJoined(ctx, in); err != nil {
		s.progress(Progress{Node: step, Step: NodeFailed, Err: err})
		return err
	}
	s.progress(Progress{Node: step, Step: NodeDone})
	return nil
}

// errNotSent matches a failure that came before the cloud got any request to create a machine.
var errNotSent = errors.New("not sent")

// notSentError is a failure before the create request, with its cause's text.
type notSentError struct{ err error }

func (e notSentError) Error() string { return e.err.Error() }
func (e notSentError) Unwrap() error { return e.err }

// Is makes the error match errNotSent as well as its cause.
func (e notSentError) Is(target error) bool { return target == errNotSent }
