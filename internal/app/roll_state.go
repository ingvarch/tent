package app

import (
	"context"
	"fmt"
	"slices"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// drainMeta is the key of the drain meta that holds the ID of the machine whose node tent drains.
const drainMeta = "tent_machine"

// nomadReading is what one observation reads from Nomad.
type nomadReading struct {
	peers   []nomadops.Peer
	health  nomadops.Health
	members []nomadops.Member
	nodes   []nomadops.Node
}

// readNomad reads the Raft configuration, autopilot's report, the gossip members and the client nodes, in that order,
// and stops at the first read that fails.
func readNomad(ctx context.Context, api nomadops.API) (nomadReading, error) {
	var r nomadReading
	for _, step := range []struct {
		what string
		call func() error
	}{
		{"the Raft configuration", func() (err error) { r.peers, err = api.Peers(ctx); return }},
		{"autopilot's report", func() (err error) { r.health, err = api.Health(ctx); return }},
		{"the gossip members", func() (err error) { r.members, err = api.Members(ctx); return }},
		{"the client nodes", func() (err error) { r.nodes, err = api.Nodes(ctx); return }},
	} {
		if err := step.call(); err != nil {
			return nomadReading{}, fmt.Errorf("read %s: %w", step.what, err)
		}
	}
	return r, nil
}

// state returns what the decisions see: one server per peer of the Raft configuration, with the health, stability and
// version of the autopilot entry of the same Raft ID; the members; and the nodes, each drained for the machine in the
// meta of its last drain once that drain is complete.
func (r nomadReading) state() rollout.Nomad {
	reports := make(map[string]nomadops.ServerHealth, len(r.health.Servers))
	for _, sh := range r.health.Servers {
		reports[sh.ID] = sh
	}
	n := rollout.Nomad{Healthy: r.health.Healthy, FailureTolerance: r.health.FailureTolerance}
	for _, p := range r.peers {
		sh := reports[p.ID] // the zero report when autopilot lacks the server
		n.Servers = append(n.Servers, rollout.Server{
			ID: p.ID, Name: p.Name, Address: p.Address, Voter: p.Voter, Leader: p.Leader,
			Healthy: sh.Healthy, StableSince: sh.StableSince, Version: sh.Version,
		})
	}
	for _, m := range r.members {
		n.Members = append(n.Members, rollout.Member{Name: m.Name, Address: m.Address, Status: m.Status})
	}
	for _, nd := range r.nodes {
		drainedFor := ""
		if nd.LastDrain.Status == "complete" {
			drainedFor = nd.LastDrain.Meta[drainMeta]
		}
		n.Nodes = append(n.Nodes, rollout.Node{
			ID: nd.ID, Name: nd.Name, Address: nd.Address, Status: nd.Status, Eligible: nd.Eligible,
			Draining: nd.Draining, DrainedFor: drainedFor, Version: nd.Version,
		})
	}
	return n
}

// rolloutMachines returns the machines that the cloud listed, as the decisions see them.
func rolloutMachines(listed []cloud.Instance) []rollout.Machine {
	machines := make([]rollout.Machine, 0, len(listed))
	for _, in := range listed {
		machines = append(machines, rollout.Machine{
			ID: in.ID, Name: in.Name, Group: in.Group, Role: in.Role, Zone: in.Zone, SpecHash: in.SpecHash,
			PrivateIP: in.PrivateIP, Ready: in.Ready, Joined: in.Joined, Created: in.Created,
		})
	}
	return machines
}

// rolloutGroups returns the node groups called names, in that order, as the decisions see them: size and zones from
// the model, the spec hash from the builder and the rolling update settings from the specs. The next index of a
// machine name is not set: rollRun.state gives it to the decisions. A setting that a spec leaves out is 0. It fails
// for a name that is not a group of the model, a group whose spec is missing or nil, and a drain
// timeout that does not parse.
func rolloutGroups(m *model.Cluster, specs map[string]*v1alpha1.NodeGroup, b *nodeBuilder, names []string,
) ([]rollout.Group, error) {
	groups := make([]rollout.Group, 0, len(names))
	for _, name := range names {
		i := slices.IndexFunc(m.Groups, func(g model.NodeGroup) bool { return g.Name == name })
		if i < 0 {
			return nil, fmt.Errorf("node group %s: not in the specs", name)
		}
		gs := specs[name]
		if gs == nil {
			return nil, fmt.Errorf("node group %s: no spec", name)
		}
		mg := m.Groups[i]
		g := rollout.Group{
			Name: name, Role: mg.Role, Size: mg.Size, Zones: mg.Zones, SpecHash: b.specHash(name),
		}
		ru := gs.Spec.RollingUpdate
		if ru.MaxSurge != nil {
			g.MaxSurge = *ru.MaxSurge
		}
		if ru.MaxUnavailable != nil {
			g.MaxUnavailable = *ru.MaxUnavailable
		}
		if ru.DrainTimeout != "" {
			drain, err := ru.Drain()
			if err != nil {
				return nil, fmt.Errorf("node group %s: drain timeout: %w", name, err)
			}
			g.DrainTimeout = drain
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// inGroups returns the listed machines that belong to one of the groups.
func inGroups(listed []cloud.Instance, groups []rollout.Group) []cloud.Instance {
	return slices.DeleteFunc(slices.Clone(listed), func(in cloud.Instance) bool {
		return !slices.ContainsFunc(groups, func(g rollout.Group) bool { return g.Name == in.Group })
	})
}

// forcedMachines returns the IDs of the listed machines of the groups that a roll replaces whatever their hash: those
// that carry the replace label, and with force every one of them.
func forcedMachines(listed []cloud.Instance, groups []rollout.Group, force bool) map[string]bool {
	forced := map[string]bool{}
	for _, in := range inGroups(listed, groups) {
		if force || in.Replace {
			forced[in.ID] = true
		}
	}
	return forced
}

// unlabelled returns the listed machines of the groups that lack the replace label.
func unlabelled(listed []cloud.Instance, groups []rollout.Group) []cloud.Instance {
	return slices.DeleteFunc(inGroups(listed, groups), func(in cloud.Instance) bool { return in.Replace })
}
