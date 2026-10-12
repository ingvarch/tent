package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/nomadops"
)

// refuseJoinedDeletes fails when the node changes delete a machine that carries the joined label and is no removal,
// among the machines that the cloud listed: a machine of a group that the specs lack whose role is not client or that
// has no group label, and machines of one name that joined. A removal, which drains the node or makes the server
// leave, is no such delete. The error names each such machine, with its ID and the reason of its delete; for a
// duplicate the reason names the machine that stays. It gives the advice for each cause.
func refuseJoinedDeletes(changes []NodeChange, listed []cloud.Instance) error {
	var names, twins []string // twins are the names that machines of one name that joined share
	var groups []string       // the groups that the specs lack and that have a node to delete
	unlabelled := 0           // the machines to delete that have no group label
	for _, c := range changes {
		in, ok := instanceByID(listed, c.ID)
		if c.Action != NodeDelete || !ok || !in.Joined || isRemoval(c) {
			continue
		}
		reason := c.Reason
		switch {
		case c.Reason == reasonDuplicate:
			reason += " of ID " + stayerOf(changes, listed, c.ID).ID
			twins = append(twins, c.Name)
		case in.Role == v1alpha1.RoleClient && in.Group != "":
			twins = append(twins, c.Name)
		case in.Group == "":
			unlabelled++
		default:
			groups = append(groups, in.Group)
		}
		names = append(names, fmt.Sprintf("%s (ID %s, %s)", c.Name, c.ID, reason))
	}
	if len(names) == 0 {
		return nil
	}
	var causes []string
	if keep := keepAdvice(groups, unlabelled); keep != "" {
		causes = append(causes, "tent removes a whole node group only when its nodes are clients: "+keep)
	}
	if len(twins) > 0 {
		causes = append(causes, "tent does not choose between machines of one name that joined: "+twinAdvice(twins))
	}
	nodes := "a node that joined"
	if len(names) > 1 {
		nodes = "nodes that joined"
	}
	return fmt.Errorf("update would delete %s Nomad: %s; %s, or delete the whole cluster with tent delete cluster",
		nodes, english.And(names), strings.Join(causes, "; "))
}

// keepAdvice says which node groups to keep in the specs: the groups, and the machines without a group label, of which
// there are unlabelled. It is empty for none.
func keepAdvice(groups []string, unlabelled int) string {
	groups = slices.Compact(slices.Sorted(slices.Values(groups)))
	var keep []string
	switch len(groups) {
	case 0:
	case 1:
		keep = append(keep, "keep node group "+groups[0]+" in the specs")
	default:
		keep = append(keep, "keep node groups "+english.And(groups)+" in the specs")
	}
	switch unlabelled {
	case 0:
	case 1:
		keep = append(keep, "keep it in a node group of the specs")
	default:
		keep = append(keep, "keep them in a node group of the specs")
	}
	return strings.Join(keep, ", and ")
}

// twinAdvice says what to do about machines of the names, each of which more than one machine that joined has.
func twinAdvice(names []string) string {
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	if len(names) > 1 {
		return "of the machines that share a name, remove one from Nomad and delete it in the cloud"
	}
	return "remove one of the two machines called " + names[0] + " from Nomad and delete it in the cloud"
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
// scrubbed and labelled, so that the next plan removes it as a node that joined or refuses its delete, and the step
// fails. When Nomad cannot be asked, nothing is deleted.
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
	if err := a.s.markJoined(ctx, a.u.nodeKit, in); err != nil {
		return fmt.Errorf("delete node %s (%s): the node has joined Nomad (%s): %w", c.Name, c.ID, joinedAs, err)
	}
	return fmt.Errorf("delete node %s (%s): the node has joined Nomad (%s); run %s again, which removes it as a node "+
		"that joined or says why it cannot", c.Name, c.ID, joinedAs, updateCommand)
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
