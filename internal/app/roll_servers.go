package app

import (
	"context"
	"net/netip"
	"slices"
	"strings"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/rollout"
)

// joinsByVote reports whether a machine of the role has joined once its server votes and autopilot counts it healthy.
// A server's does; the node of a combined machine must register too.
func joinsByVote(role v1alpha1.Role) bool { return role == v1alpha1.RoleServer }

// nodeOfServer returns the name of the node from the name of its server, which is <node name>.<region>.
func nodeOfServer(name string) string {
	node, _, _ := strings.Cut(name, ".")
	return node
}

// serverAt returns the server of servers that runs at the private address addr, and whether there is one.
func serverAt(servers []rollout.Server, addr netip.Addr) (rollout.Server, bool) {
	i := slices.IndexFunc(servers, func(s rollout.Server) bool { return addr.IsValid() && s.Address.Addr() == addr })
	if i < 0 {
		return rollout.Server{}, false
	}
	return servers[i], true
}

// voting returns how many of servers vote.
func voting(servers []rollout.Server) int {
	n := 0
	for _, s := range servers {
		if s.Voter {
			n++
		}
	}
	return n
}

// healthyVoterAt reports whether servers hold a voter at the private address addr that autopilot counts healthy.
func healthyVoterAt(servers []rollout.Server, addr netip.Addr) bool {
	srv, ok := serverAt(servers, addr)
	return ok && srv.Voter && srv.Healthy
}

// showingServer says what the reading shows of the server that a wait for a vote or for a stopped server to go down
// waits on.
func showingServer(step rollout.Step, reading nomadReading) string {
	servers := reading.state().Servers
	if step.Action == rollout.WaitServerDown {
		if healthyVoterAt(servers, step.Machine.PrivateIP) {
			return "autopilot counts it a healthy voter"
		}
		return "autopilot does not count it a healthy voter"
	}
	srv, ok := serverAt(servers, step.Machine.PrivateIP)
	switch {
	case !ok:
		return "the Raft configuration lists no server at its address"
	case !srv.Voter:
		return "its server does not vote yet"
	}
	return "autopilot does not count its server healthy"
}

// stop powers the machine m off and lists the machines at the next observation.
func (r *rollRun) stop(ctx context.Context, m rollout.Machine) error {
	c := NodeChange{Action: NodeStop, Name: m.Name, ID: m.ID}
	if err := r.s.applyNode(ctx, r.kit.nodes, r.kit.cluster, c); err != nil {
		return err
	}
	r.relist = true
	return nil
}
