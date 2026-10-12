package app

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nomadops"
)

// introLifetime is how long after the creation of its machine a client can still register with its intro token.
const introLifetime = nomadops.MaxIntroTTL + nomadops.IntroLeeway

// staleClients returns the clients of the cluster that are older than introLifetime at now and have not joined, by
// name, then ID, whatever the plan does with them. A client is a machine whose role label says so, whatever its group.
// A machine without a creation time is never old.
func staleClients(cluster string, listed []cloud.Instance, now time.Time) []cloud.Instance {
	var stale []cloud.Instance
	for _, in := range listed {
		if in.Cluster == cluster && in.Role == v1alpha1.RoleClient && !in.Joined && !in.Created.IsZero() &&
			now.Sub(in.Created) > introLifetime {
			stale = append(stale, in)
		}
	}
	slices.SortFunc(stale, func(a, b cloud.Instance) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return stale
}

// registered reports whether nodes list a client of the name and private address of the machine in that is not down.
// A down node is one of an earlier machine of the name.
func registered(nodes []nomadops.Node, in cloud.Instance) bool {
	return slices.ContainsFunc(nodes, func(n nomadops.Node) bool {
		return n.Is(in.Name, in.PrivateIP) && n.Status != "down"
	})
}

// askNomadError says that the plan could not ask Nomad whether the stale machines registered, because of cause. It
// names each machine with its ID, since a machine of the same name may have joined.
func askNomadError(stale []cloud.Instance, cause error) error {
	names := make([]string, len(stale))
	for i, in := range stale {
		names[i] = fmt.Sprintf("%s (ID %s)", in.Name, in.ID)
	}
	nodes, its, they := "node", "its", "it"
	if len(names) > 1 {
		nodes, its, they = "nodes", "their", "they"
	}
	return fmt.Errorf("%s %s did not join within %d minutes of %s creation, and tent could not ask Nomad whether %s "+
		"registered: %w", nodes, english.And(names), int(introLifetime.Minutes()), its, they, cause)
}

// couldNotAskNodes is the start of the warning of a read of Nomad's nodes that failed when no node had to be judged
// and no node that joined could go: the plan then lists no purge of a node that a deleted machine left.
const couldNotAskNodes = "tent could not ask Nomad whether a node of a deleted machine is left: "

// joinedAmong returns the machines among servers that carry the joined label.
func joinedAmong(servers []cloud.Instance) []cloud.Instance {
	return slices.DeleteFunc(slices.Clone(servers), func(in cloud.Instance) bool { return !in.Joined })
}

// notRegistered returns the IDs of the stale machines that nodes do not list as registered clients. It returns nil
// when no server that stays has joined: the stale machines that stay are waited for.
func notRegistered(nodes []nomadops.Node, stale, servers []cloud.Instance) map[string]bool {
	if len(joinedAmong(servers)) == 0 {
		return nil
	}
	gone := map[string]bool{}
	for _, in := range stale {
		if !registered(nodes, in) {
			gone[in.ID] = true
		}
	}
	return gone
}

// readNodes reads the client nodes that Nomad lists, with one call, when a plan needs them and no roll-style reading
// has given them: to tell which stale clients registered, through the servers that stay and have joined, and a failure
// then fails the plan; or, when the cluster can answer (the store holds the mark of the ACL bootstrap and a server
// that joined and runs has a public address), to find the nodes that deleted machines left, through those servers. A
// failure of that read comes back as a warning and no error, unless ctx ended. It returns whether it read.
func (s *Service) readNodes(ctx context.Context, m *model.Cluster, access nomadAccess,
	listed, stale, servers []cloud.Instance, marked bool,
) (nodes []nomadops.Node, read bool, warning string, err error) {
	if joined := joinedAmong(servers); len(stale) > 0 && len(joined) > 0 {
		if nodes, err = s.askNodes(ctx, access, joined); err != nil {
			return nil, false, "", askNomadError(stale, err)
		}
		return nodes, true, "", nil
	}
	machines := apiMachines(m, listed, nil, nil)
	if !marked || len(machines) == 0 {
		return nil, false, "", nil
	}
	if nodes, err = s.askNodes(ctx, access, machines); err != nil {
		if ctx.Err() != nil {
			return nil, false, "", err
		}
		return nil, false, couldNotAskNodes + err.Error(), nil
	}
	return nodes, true, "", nil
}

// askNodes reads the client nodes through the API over the machines.
func (s *Service) askNodes(ctx context.Context, access nomadAccess, machines []cloud.Instance,
) ([]nomadops.Node, error) {
	api, err := s.nomadOver(machines, access)
	if err != nil {
		return nil, err
	}
	return api.Nodes(ctx)
}
