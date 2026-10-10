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
	if err := r.beginStop(m); err != nil {
		return err
	}
	if err := r.callStop(ctx, m); err != nil {
		delete(r.stopping, m.ID)
		return err
	}
	r.rolled.stopped[m.ID] = true
	return nil
}

// stopHeld stops the machine m as stop does, for a stop that the run held. The call gets haltCallTimeout. A call that
// fails while the run's context lives, whether the cloud answered with an error or gave no answer, may still be
// carried out later: the machine stays among those being stopped and out of the run's API, the error is noted in
// heldSent, one warning is given, and the machine is counted once the cloud lists it as stopped. A call that fails
// because the run's context ended leaves the machine unnoted.
func (r *rollRun) stopHeld(ctx context.Context, m rollout.Machine) error {
	if err := r.beginStop(m); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, haltCallTimeout)
	defer cancel()
	err := r.callStop(callCtx, m)
	switch {
	case err == nil:
		r.heldSent[m.ID] = nil
		r.rolled.stopped[m.ID] = true
	case ctx.Err() != nil:
		delete(r.stopping, m.ID)
		return err
	default:
		r.heldSent[m.ID] = err
		r.s.warn(fmt.Sprintf("the stop of node %s (ID %s) may still be carried out; tent waits up to %s for the cloud "+
			"to list it as stopped and removes it from the Raft configuration if Nomad's leader adds it again",
			m.Name, m.ID, stopTimeout))
	}
	return nil
}

// beginStop notes the machine m among those being stopped and takes it out of the run's API. It fails, and notes
// nothing, when the API cannot be made without it.
func (r *rollRun) beginStop(m rollout.Machine) error {
	r.stopping[m.ID] = time.Now()
	if err := r.followAPI(); err != nil {
		delete(r.stopping, m.ID)
		return err
	}
	return nil
}

// callStop asks the cloud to stop the machine m and reports the stop.
func (r *rollRun) callStop(ctx context.Context, m rollout.Machine) error {
	return r.s.applyNode(ctx, r.kit.nodes, r.kit.cluster, NodeChange{Action: NodeStop, Name: m.Name, ID: m.ID})
}

// unpeer takes the machine m out of the run's API, so that no call goes to it once its peer is removed, and notes it
// among the machines without a peer; it does neither for a machine that the last list shows not running or that the
// run has stopped. It reports whether it did. When the API cannot be made without the machine it fails, and the
// machine stays in the API.
func (r *rollRun) unpeer(m rollout.Machine) (bool, error) {
	in, _ := instanceByID(r.listed, m.ID)
	if _, stopped := r.stopping[m.ID]; !in.Ready || stopped {
		return false, nil
	}
	r.unpeered[m.ID] = true
	if err := r.followAPI(); err != nil {
		delete(r.unpeered, m.ID)
		return false, err
	}
	return true, nil
}

// removePeer removes the peer of the server of the step's machine from the Raft configuration. A machine that runs
// leaves the run's API first; one whose call fails is taken back, and the next list makes the API over it again. A
// removal that works is noted in the held stop of the machine.
func (r *rollRun) removePeer(ctx context.Context, step rollout.Step) error {
	m := step.Machine
	left, err := r.unpeer(m)
	if err != nil {
		return err
	}
	err = r.write(NomadEvent{Action: NomadRemovePeer, Node: m.Name}, func() error {
		return r.api.RemovePeer(ctx, step.Server.ID)
	})
	switch {
	case err != nil && left:
		delete(r.unpeered, m.ID)
	case err == nil && r.held[m.ID] != nil:
		r.held[m.ID].noteRemoval()
	}
	return err
}

// dropRejoined takes the machines whose server votes in servers, the Raft configuration, out of the set of those
// without a peer, and has the next observation list, which makes the API over them again.
func (r *rollRun) dropRejoined(servers []rollout.Server) {
	for id := range r.unpeered {
		in, _ := instanceByID(r.listed, id)
		if srv, ok := serverAt(servers, in.PrivateIP); ok && srv.Voter {
			delete(r.unpeered, id)
			r.relist = true
		}
	}
}

// waitStopped waits a poll for the cloud to list the machine, which the run stopped at since, as not running, and fails
// once stopTimeout has passed. The poll of a held stop forgets the step the run carried out last, so that the removal
// of a server that the leader adds again is a first try, and the failure names the error of a held stop whose call
// failed.
func (r *rollRun) waitStopped(ctx context.Context, step rollout.Step, since time.Time) error {
	m := step.Machine
	callErr, held := r.heldSent[m.ID]
	if held {
		r.forgetStep()
	}
	switch {
	case time.Since(since) < stopTimeout:
		return r.sleep(ctx)
	case callErr != nil:
		return fmt.Errorf("stop node %s (ID %s): %w; the cloud still lists it as running after %s; "+
			"run tent rolling-update cluster again", m.Name, m.ID, callErr, stopTimeout)
	}
	return fmt.Errorf("the cloud still lists node %s (ID %s) as running %s after tent stopped it; "+
		"run tent rolling-update cluster again", m.Name, m.ID, stopTimeout)
}

// apiMachines returns the machines whose Nomad API a roll calls: the machines of the server and combined groups of m
// among listed that joined, run and have a public address, without those in stopping and in unpeered, in the order
// of their names.
func apiMachines(m *model.Cluster, listed []cloud.Instance, stopping map[string]time.Time, unpeered map[string]bool,
) []cloud.Instance {
	machines := slices.DeleteFunc(slices.Clone(listed), func(in cloud.Instance) bool {
		_, stopped := stopping[in.ID]
		return stopped || unpeered[in.ID] || !isServerMachine(m, in) || !in.Joined || !in.Ready || !in.PublicIP.IsValid()
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
	machines := apiMachines(r.model, r.listed, r.stopping, r.unpeered)
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
