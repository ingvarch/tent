package nomadops

import (
	"context"
	"net/http"

	"github.com/hashicorp/nomad/api"
)

const nodesPath = "/v1/nodes"

// Node is a client node as the servers list it. A name can appear more than once: a node that went down stays listed
// until Nomad collects it.
type Node struct {
	Name     string
	Status   string // initializing, ready, down or disconnected
	Eligible bool   // the scheduler may place work on the node
}

// Ready reports whether the node is ready and eligible for work.
func (n Node) Ready() bool { return n.Status == "ready" && n.Eligible }

// Nodes returns the client nodes that registered with the cluster, in the order that the server lists them.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var stubs []*api.NodeListStub
	err := c.call(ctx, http.MethodGet, nodesPath, func(ctx context.Context) error {
		// Not Nodes().List, whose sort panics on a null in the list.
		_, err := c.api.Raw().Query(nodesPath, &stubs, query(ctx))
		return err
	})
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(stubs))
	for _, s := range stubs {
		if s != nil {
			nodes = append(nodes, Node{Name: s.Name, Status: s.Status, Eligible: s.SchedulingEligibility == "eligible"})
		}
	}
	return nodes, nil
}
