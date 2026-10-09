package app

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/rollout"
)

// stopTimeout is how long the cloud may list a machine as running after tent stopped it: Vultr reads a halted instance
// as stopped 4 to 19 s after the call.
const stopTimeout = 2 * time.Minute

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

// stop takes the machine m out of the run's API, powers it off and notes it among the machines being stopped, which
// makes every observation list the machines until the cloud lists it as stopped. A stop that fails leaves the machine
// unnoted, so that a later stop of it is sent.
func (r *rollRun) stop(ctx context.Context, m rollout.Machine) error {
	r.stopping[m.ID] = time.Now()
	err := r.followAPI()
	if err == nil {
		err = r.s.applyNode(ctx, r.kit.nodes, r.kit.cluster, NodeChange{Action: NodeStop, Name: m.Name, ID: m.ID})
	}
	if err != nil {
		delete(r.stopping, m.ID)
		return err
	}
	r.rolled.stopped[m.ID] = true
	return nil
}

// waitStopped waits a poll for the cloud to list the machine, which the run stopped at since, as not running, and fails
// once stopTimeout has passed.
func (r *rollRun) waitStopped(ctx context.Context, step rollout.Step, since time.Time) error {
	if time.Since(since) >= stopTimeout {
		return fmt.Errorf("the cloud still lists node %s (ID %s) as running %s after tent stopped it; "+
			"run tent rolling-update cluster again", step.Machine.Name, step.Machine.ID, stopTimeout)
	}
	return r.sleep(ctx)
}

// apiMachines returns the machines whose Nomad API a roll calls: the machines of the server and combined groups of m
// among listed that joined, run and have a public address, without those in stopping, in the order of their names.
func apiMachines(m *model.Cluster, listed []cloud.Instance, stopping map[string]time.Time) []cloud.Instance {
	machines := slices.DeleteFunc(slices.Clone(listed), func(in cloud.Instance) bool {
		_, stopped := stopping[in.ID]
		return stopped || !isServerMachine(m, in) || !in.Joined || !in.Ready || !in.PublicIP.IsValid()
	})
	slices.SortFunc(machines, compareName)
	return machines
}

// addressesOf returns the addresses of the Nomad API of the machines.
func addressesOf(machines []cloud.Instance) []string {
	addrs := make([]string, len(machines))
	for i, in := range machines {
		addrs[i] = apiAddress(in)
	}
	return addrs
}

// followAPI makes the run's API over apiMachines again when their addresses are not those it was made over. It fails,
// and keeps the API, when there is no such machine.
func (r *rollRun) followAPI() error {
	machines := apiMachines(r.model, r.listed, r.stopping)
	addrs := addressesOf(machines)
	if slices.Equal(addrs, r.apiAt) {
		return nil
	}
	api, err := r.s.rollAPI(r.model.Name, machines, r.kit)
	if err != nil {
		return err
	}
	r.api, r.apiAt = api, addrs
	return nil
}
