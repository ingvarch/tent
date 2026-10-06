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

// unregistered returns the IDs of the stale machines that Nomad does not list as registered clients. It asks only
// when a server that stays has joined: through those servers, with one call of Nodes. Without one it returns nil, and
// those of the machines that stay are waited for.
func (s *Service) unregistered(ctx context.Context, access nomadAccess, stale, servers []cloud.Instance,
) (map[string]bool, error) {
	joined := slices.DeleteFunc(slices.Clone(servers), func(in cloud.Instance) bool { return !in.Joined })
	if len(stale) == 0 || len(joined) == 0 {
		return nil, nil
	}
	api, err := s.nomadOver(joined, access)
	if err != nil {
		return nil, askNomadError(stale, err)
	}
	nodes, err := api.Nodes(ctx)
	if err != nil {
		return nil, askNomadError(stale, err)
	}
	gone := map[string]bool{}
	for _, in := range stale {
		if !registered(nodes, in) {
			gone[in.ID] = true
		}
	}
	return gone, nil
}
