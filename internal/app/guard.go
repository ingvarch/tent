package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/nomadops"
)

// noDrain says why an update keeps the nodes that joined Nomad.
const noDrain = "tent cannot drain a node or remove a server yet, so update deletes only nodes that never joined"

// refuseJoinedDeletes fails when the node changes delete a machine that carries the joined label, among the machines
// that the cloud listed. The error names each such machine, with its ID and the reason of its delete; for a duplicate
// the reason names the machine that stays. It says what to do for each kind of delete.
func refuseJoinedDeletes(changes []NodeChange, listed []cloud.Instance) error {
	var names, dups []string
	kept := 0 // the machines whose delete is not that of a duplicate
	for _, c := range changes {
		if c.Action != NodeDelete {
			continue
		}
		if in, ok := instanceByID(listed, c.ID); !ok || !in.Joined {
			continue
		}
		reason := c.Reason
		if c.Reason == reasonDuplicate {
			reason += " of ID " + stayerOf(changes, listed, c.ID).ID
			dups = append(dups, c.Name)
		} else {
			kept++
		}
		names = append(names, fmt.Sprintf("%s (ID %s, %s)", c.Name, c.ID, reason))
	}
	if len(names) == 0 {
		return nil
	}
	var todo []string
	switch kept {
	case 0:
	case 1:
		todo = append(todo, "keep this node in the specs")
	default:
		todo = append(todo, "keep these nodes in the specs")
	}
	switch len(dups) {
	case 0:
	case 1:
		todo = append(todo, "remove one of the two machines called "+dups[0]+" from Nomad and delete it in the cloud")
	default:
		todo = append(todo, "of the machines that share a name, remove one from Nomad and delete it in the cloud")
	}
	nodes := "a node that joined"
	if len(names) > 1 {
		nodes = "nodes that joined"
	}
	return fmt.Errorf("update would delete %s Nomad: %s; %s; %s, or delete the whole cluster with tent delete cluster",
		nodes, english.And(names), noDrain, strings.Join(todo, ", and "))
}

// stayerOf returns the machine that stays in the place of the duplicate with the ID id: the listed machine of its
// cluster and name that the changes do not delete as a duplicate or as outside the spec. A machine that stays under
// its name may still go as surplus or as not registered.
func stayerOf(changes []NodeChange, listed []cloud.Instance, id string) cloud.Instance {
	dup, _ := instanceByID(listed, id)
	i := slices.IndexFunc(listed, func(in cloud.Instance) bool {
		return in.Cluster == dup.Cluster && in.Name == dup.Name && !slices.ContainsFunc(changes, func(c NodeChange) bool {
			return c.Action == NodeDelete && c.ID == in.ID && (c.Reason == reasonDuplicate || c.Reason == reasonNotInSpec)
		})
	})
	if i < 0 {
		return cloud.Instance{}
	}
	return listed[i]
}

// applyDelete carries out the delete c of a node, unless its machine has joined Nomad. A machine that joined is
// scrubbed and labelled, so that the next plan refuses its delete, and the step fails. When Nomad cannot be asked,
// nothing is deleted.
func (a *applier) applyDelete(ctx context.Context, c NodeChange) error {
	err := a.guardDelete(ctx, c)
	if err != nil {
		a.s.progress(Progress{Node: c, Step: NodeStarted})
		a.s.progress(Progress{Node: c, Step: NodeFailed, Err: err})
		return err
	}
	return a.s.applyNode(ctx, a.u.nodes, a.u.cluster, c)
}

// guardDelete fails when the machine of the delete c has joined Nomad, after it has scrubbed and labelled the machine.
func (a *applier) guardDelete(ctx context.Context, c NodeChange) error {
	in := a.listedMachine(c)
	joinedAs, err := a.hasJoined(ctx, in)
	switch {
	case err != nil:
		return fmt.Errorf("delete node %s (%s): ask Nomad whether the node joined: %w", c.Name, c.ID, err)
	case joinedAs == "":
		return nil
	}
	if err := a.markJoined(ctx, in); err != nil {
		return fmt.Errorf("delete node %s (%s): the node has joined Nomad (%s): %w", c.Name, c.ID, joinedAs, err)
	}
	return fmt.Errorf("delete node %s (%s): the node has joined Nomad (%s); %s", c.Name, c.ID, joinedAs, noDrain)
}

// hasJoined asks Nomad whether the machine in has joined, whatever its role: whether a client of its name at its
// private address is registered and not down, or the Raft configuration lists a server at that address, voter or
// not. It returns what the machine joined as, such as "a registered client at 10.64.0.9", or "" when it has not
// joined. A machine without a private address has not.
func (a *applier) hasJoined(ctx context.Context, in cloud.Instance) (string, error) {
	if !in.PrivateIP.IsValid() {
		return "", nil
	}
	api, err := a.nomadAPI()
	if err != nil {
		return "", err
	}
	nodes, err := api.Nodes(ctx)
	if err != nil {
		return "", err
	}
	if registered(nodes, in) {
		return "a registered client at " + in.PrivateIP.String(), nil
	}
	peers, err := api.Peers(ctx)
	if err != nil {
		return "", err
	}
	if p, ok := nomadops.FindPeer(peers, in.PrivateIP); ok {
		return "a server at " + p.Address.String(), nil
	}
	return "", nil
}
