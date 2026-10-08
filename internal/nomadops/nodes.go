package nomadops

import (
	"context"
	"net/http"
	"net/netip"

	"github.com/hashicorp/nomad/api"
)

const nodesPath = "/v1/nodes"

// Node is a client node as the servers list it. A name can appear more than once: a node that went down stays listed
// until Nomad collects it.
type Node struct {
	ID       string // Nomad's ID of the node
	Name     string
	Status   string // initializing, ready, down or disconnected
	Eligible bool   // the scheduler may place work on the node
	Draining bool   // a drain is under way
	// LastDrain is the node's last drain; the zero value when it was never drained.
	LastDrain LastDrain
	// Address is the host of the HTTP address that the node advertises; invalid when Nomad gives none that parses.
	Address netip.Addr
	Version string // the Nomad version that the client runs
}

// LastDrain is the last drain of a node.
type LastDrain struct {
	Status string            // draining, complete or canceled
	Meta   map[string]string // the meta that the drain was asked with
}

// Is reports whether the node is called name and advertises addr. An invalid addr never matches.
func (n Node) Is(name string, addr netip.Addr) bool {
	return addr.IsValid() && n.Name == name && n.Address == addr
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
			addr, _ := netip.ParseAddr(s.Address) // the invalid Addr when it does not parse
			nodes = append(nodes, Node{ID: s.ID, Name: s.Name, Status: s.Status,
				Eligible: s.SchedulingEligibility == "eligible", Draining: s.Drain, LastDrain: lastDrain(s.LastDrain),
				Address: addr, Version: s.Version})
		}
	}
	return nodes, nil
}

// lastDrain converts Nomad's drain record; nil, which Nomad gives for a node that was never drained, is the zero
// value.
func lastDrain(d *api.DrainMetadata) LastDrain {
	if d == nil {
		return LastDrain{}
	}
	return LastDrain{Status: string(d.Status), Meta: d.Meta}
}
