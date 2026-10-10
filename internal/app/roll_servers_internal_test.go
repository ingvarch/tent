package app

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// apiModel is a cluster with a server, a combined and a client group.
var apiModel = &model.Cluster{Name: "prod", Groups: []model.NodeGroup{
	{Name: "servers", Role: v1alpha1.RoleServer}, {Name: "all", Role: v1alpha1.RoleCombined},
	{Name: "workers", Role: v1alpha1.RoleClient},
}}

// apiServer is a running server that joined and has the public address 203.0.113.<n>.
func apiServer(id, name string, n byte) cloud.Instance {
	return cloud.Instance{
		ID: id, Name: name, Group: "servers", Role: v1alpha1.RoleServer, Ready: true, Joined: true,
		PublicIP: netip.AddrFrom4([4]byte{203, 0, 113, n}),
	}
}

// TestApiMachinesKeepsTheJoinedRunningServersWithAnAddressByName names the machines whose API a roll calls: those of a
// server or combined group that joined, run, have a public address and are not being stopped, in the order of their
// names.
func TestApiMachinesKeepsTheJoinedRunningServersWithAnAddressByName(t *testing.T) {
	t.Parallel()
	with := func(change func(*cloud.Instance)) cloud.Instance {
		in := apiServer("i-9", "prod-servers-9", 9)
		change(&in)
		return in
	}
	combined := apiServer("i-5", "prod-all-5", 5)
	combined.Group, combined.Role = "all", v1alpha1.RoleCombined
	listed := []cloud.Instance{
		apiServer("i-2", "prod-servers-2", 2),
		with(func(in *cloud.Instance) {
			in.Group, in.Role, in.Name = "workers", v1alpha1.RoleClient, "prod-workers-0"
		}),
		with(func(in *cloud.Instance) { in.Joined = false }),
		with(func(in *cloud.Instance) { in.Ready = false }),
		with(func(in *cloud.Instance) { in.PublicIP = netip.Addr{} }),
		apiServer("i-3", "prod-servers-3", 3),
		apiServer("i-1", "prod-servers-1", 1),
		combined,
	}
	stopping := map[string]time.Time{"i-3": {}}

	got := apiMachines(apiModel, listed, stopping, nil)

	var names []string
	for _, in := range got {
		names = append(names, in.Name)
	}
	if want := []string{"prod-all-5", "prod-servers-1", "prod-servers-2"}; !slices.Equal(names, want) {
		t.Errorf("apiMachines = %v, want %v", names, want)
	}
}

// apiRun returns a run over the model apiModel whose Nomad is a function that notes the address of each server it is
// asked for, and the addresses noted so far.
func apiRun(t *testing.T, listed ...cloud.Instance) (*rollRun, *[]string) {
	t.Helper()
	var steps []string
	var built []string
	s := testService(&steps)
	s.Nomad = func(cfg nomadops.Config) (nomadops.API, error) {
		built = append(built, cfg.Address)
		return &introStub{}, nil
	}
	r := &rollRun{
		s: s, kit: testKit(t, &recordingNodes{}), model: apiModel, listed: listed, rollLoop: newRollLoop(),
	}
	return r, &built
}

// TestFollowAPIMakesAnotherAPIOnlyWhenTheAddressesChange keeps the API while the machines it is over stay the same,
// and makes a new one over the new machines when one joins, one is being stopped, or one takes the place of another; a
// stopped server that leaves the list changes nothing.
func TestFollowAPIMakesAnotherAPIOnlyWhenTheAddressesChange(t *testing.T) {
	t.Parallel()
	one, two, three := apiServer("i-1", "prod-servers-1", 1), apiServer("i-2", "prod-servers-2", 2),
		apiServer("i-3", "prod-servers-3", 3)
	r, built := apiRun(t, one, two)
	r.apiAt = addressesOf(apiMachines(apiModel, r.listed, r.stopping, r.unpeered))
	first := &introStub{}
	r.api = first

	if err := r.followAPI(); err != nil || r.api != nomadops.API(first) || len(*built) != 0 {
		t.Fatalf("followAPI = %v, API changed: %v, built %v; want nothing built for the same machines", err,
			r.api != nomadops.API(first), *built)
	}

	r.listed = []cloud.Instance{one, two, three}
	if err := r.followAPI(); err != nil {
		t.Fatalf("followAPI after a server joined: %v", err)
	}
	if want := []string{"203.0.113.1:4646", "203.0.113.2:4646", "203.0.113.3:4646"}; !slices.Equal(*built, want) {
		t.Errorf("the API was made over %v after a server joined, want %v", *built, want)
	}
	if want := *built; !slices.Equal(r.apiAt, want) {
		t.Errorf("the run notes the addresses %v, want %v", r.apiAt, want)
	}
	if _, ok := r.api.(*nomadops.Servers); !ok {
		t.Errorf("the API is %T, want the servers", r.api)
	}

	*built = nil
	r.stopping["i-2"] = time.Time{}
	if err := r.followAPI(); err != nil {
		t.Fatalf("followAPI after a stop: %v", err)
	}
	if want := []string{"203.0.113.1:4646", "203.0.113.3:4646"}; !slices.Equal(*built, want) {
		t.Errorf("the API was made over %v after a stop, want %v", *built, want)
	}

	*built = nil
	r.listed = []cloud.Instance{one, three}
	if err := r.followAPI(); err != nil || len(*built) != 0 {
		t.Errorf("followAPI = %v, built %v; want nothing built: the stopped server left the list and the API", err, *built)
	}

	*built = nil
	r.listed = []cloud.Instance{one, apiServer("i-4", "prod-servers-4", 4)}
	if err := r.followAPI(); err != nil {
		t.Fatalf("followAPI after a server took the place of another: %v", err)
	}
	if want := []string{"203.0.113.1:4646", "203.0.113.4:4646"}; !slices.Equal(*built, want) {
		t.Errorf("the API was made over %v after a server took the place of another, want %v", *built, want)
	}
}

// TestFollowAPIFailsWithoutAServerThatRuns keeps the API and says that no server joined and runs.
func TestFollowAPIFailsWithoutAServerThatRuns(t *testing.T) {
	t.Parallel()
	one := apiServer("i-1", "prod-servers-1", 1)
	r, built := apiRun(t, one)
	r.apiAt = addressesOf([]cloud.Instance{one})
	kept := &introStub{}
	r.api = kept
	r.stopping["i-1"] = time.Time{}

	err := r.followAPI()

	const want = "cluster prod has no server that joined and runs; run tent validate cluster to see what is wrong"
	if err == nil || err.Error() != want {
		t.Errorf("followAPI = %v, want %q", err, want)
	}
	if r.api != nomadops.API(kept) || len(*built) != 0 {
		t.Errorf("the run changed its API to %T and built %v, want it kept", r.api, *built)
	}
}

// TestFollowAPIFailsWhenTheAPICannotBeMade ends with the error of the Nomad that the service makes.
func TestFollowAPIFailsWhenTheAPICannotBeMade(t *testing.T) {
	t.Parallel()
	r, _ := apiRun(t, apiServer("i-1", "prod-servers-1", 1))
	errNomad := errors.New("no client")
	r.s.Nomad = func(nomadops.Config) (nomadops.API, error) { return nil, errNomad }

	if err := r.followAPI(); !errors.Is(err, errNomad) {
		t.Errorf("followAPI = %v, want %v", err, errNomad)
	}
}

// TestListFollowsTheAPIAndForgetsStoppedMachinesTheCloudShowsNotRunning makes the API over the servers of the new
// list, and drops the stopped machines that the list shows not ready or no more, and keeps those it shows running.
func TestListFollowsTheAPIAndForgetsStoppedMachinesTheCloudShowsNotRunning(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		running, off, other := apiServer("i-1", "prod-servers-1", 1), apiServer("i-2", "prod-servers-2", 2),
			apiServer("i-4", "prod-servers-4", 4)
		off.Ready = false
		r, built := apiRun(t)
		r.kit.nodes = listsNodes{listed: []cloud.Instance{running, off, other}}
		for _, id := range []string{"i-1", "i-2", "i-3"} {
			r.stopping[id] = time.Now()
		}

		if err := r.list(t.Context()); err != nil {
			t.Fatalf("list: %v", err)
		}

		if got := slices.Sorted(maps.Keys(r.stopping)); !slices.Equal(got, []string{"i-1"}) {
			t.Errorf("the run is still stopping %v, want only the machine that the list shows running", got)
		}
		if want := []string{"203.0.113.4:4646"}; !slices.Equal(*built, want) {
			t.Errorf("the API was made over %v, want over the one server that is not being stopped", *built)
		}
	})
}

// TestObserveListsWhileAMachineIsBeingStopped lists at every observation until the cloud shows the machine not
// running, whatever the decisions gave.
func TestObserveListsWhileAMachineIsBeingStopped(t *testing.T) {
	t.Parallel()
	fail := false
	r := &rollRun{
		api: readsNomad{fail: &fail}, rollLoop: newRollLoop(), kit: nodeKit{nodes: listsNodes{}}, model: apiModel,
	}
	r.stopping["i-1"] = time.Time{}

	if _, ok, err := r.observe(t.Context()); !ok || err != nil || !r.listing {
		t.Fatalf("observe = %v, %v, listing %v; want an observation that listed", ok, err, r.listing)
	}
	if len(r.stopping) != 0 {
		t.Fatalf("the run is still stopping %v after a list without the machine", r.stopping)
	}
	if _, ok, err := r.observe(t.Context()); !ok || err != nil || r.listing {
		t.Errorf("observe = %v, %v, listing %v; want an observation that did not list", ok, err, r.listing)
	}
}

// TestActWaitsForAStoppedMachineAndEndsAtTheLimit does not send a stop again for a machine that the run stopped: it
// waits a poll, without counting a try, and ends the run once the cloud has listed the machine as running for two
// minutes since the stop. Any other step of the machine is carried out.
func TestActWaitsForAStoppedMachineAndEndsAtTheLimit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var steps []string
		nodes := &stoppingNodes{}
		node := &marksIneligible{}
		r := &rollRun{
			s: testService(&steps), kit: nodeKit{cluster: "prod", nodes: nodes}, rollLoop: newRollLoop(), api: node,
		}
		r.stopping["i-1"] = time.Now()
		machine := rollout.Machine{ID: "i-1", Name: "prod-servers-1"}
		stop := rollout.Step{Action: rollout.Stop, Group: "servers", Machine: machine}
		start := time.Now()

		for time.Since(start) < stopTimeout {
			if err := r.act(t.Context(), stop); err != nil {
				t.Fatalf("act after %v: %v", time.Since(start), err)
			}
		}

		if len(nodes.stopped) != 0 || r.tries != 0 || r.last != (stepKey{}) {
			t.Errorf("act sent %d stops and tried %d times of %v, want it to wait without a try", len(nodes.stopped),
				r.tries, r.last)
		}
		if got := time.Since(start); got != stopTimeout {
			t.Errorf("act waited %v, want a poll each time up to %v", got, stopTimeout)
		}
		err := r.act(t.Context(), stop)
		want := "the cloud still lists node prod-servers-1 (ID i-1) as running 2m0s after tent stopped it; " +
			"run tent rolling-update cluster again"
		if err == nil || err.Error() != want {
			t.Errorf("act at the limit = %v, want %q", err, want)
		}

		other := rollout.Step{
			Action: rollout.MarkIneligible, Group: "servers", Machine: machine, Node: rollout.Node{ID: "n-1"},
		}
		if err := r.act(t.Context(), other); err != nil || len(node.marked) != 1 {
			t.Errorf("act of another step = %v with %d nodes marked, want it carried out", err, len(node.marked))
		}
	})
}

// marksIneligible is a Nomad API that records the nodes it was asked to mark ineligible.
type marksIneligible struct {
	nomadops.API
	marked []string
}

func (m *marksIneligible) MarkIneligible(_ context.Context, nodeID string) error {
	m.marked = append(m.marked, nodeID)
	return nil
}

// apiRecordingStop is a cloud whose Stop notes the addresses that the run's API was over when it was asked.
type apiRecordingStop struct {
	cloud.Nodes
	r   *rollRun
	at  [][]string
	err error
}

func (n *apiRecordingStop) Stop(context.Context, cloud.Instance) error {
	n.at = append(n.at, slices.Clone(n.r.apiAt))
	return n.err
}

// TestStopLeavesTheAPIBeforeItSendsTheStopAndCountsOnlyAStopThatSucceeds takes the machine out of the API and notes it
// among the stopped ones before the cloud is asked, counts it once, and forgets it when the cloud refuses.
func TestStopLeavesTheAPIBeforeItSendsTheStopAndCountsOnlyAStopThatSucceeds(t *testing.T) {
	t.Parallel()
	one, two := apiServer("i-1", "prod-servers-1", 1), apiServer("i-2", "prod-servers-2", 2)
	errCloud := errors.New("the cloud refused")
	for _, tc := range []struct {
		name     string
		cloudErr error
		stopping []string
		counted  int
	}{{"the cloud stops it", nil, []string{"i-1"}, 1}, {"the cloud refuses", errCloud, nil, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := apiRun(t, one, two)
			nodes := &apiRecordingStop{r: r, err: tc.cloudErr}
			r.kit.nodes = nodes
			r.apiAt = addressesOf(apiMachines(apiModel, r.listed, r.stopping, r.unpeered))

			err := r.stop(t.Context(), rollout.Machine{ID: "i-1", Name: "prod-servers-1"})

			if !errors.Is(err, tc.cloudErr) {
				t.Errorf("stop error = %v, want %v", err, tc.cloudErr)
			}
			if diff := cmp.Diff([][]string{{"203.0.113.2:4646"}}, nodes.at); diff != "" {
				t.Errorf("the addresses of the API when the cloud was asked (-want +got):\n%s", diff)
			}
			if got := slices.Sorted(maps.Keys(r.stopping)); !slices.Equal(got, tc.stopping) {
				t.Errorf("the run is stopping %v, want %v", got, tc.stopping)
			}
			if got := r.rolled.counts().Stopped; got != tc.counted {
				t.Errorf("the roll counts %d stopped machines, want %d", got, tc.counted)
			}
		})
	}
}

// TestRollTallyCountsEachStoppedMachineOnce counts the distinct machines that the roll stopped.
func TestRollTallyCountsEachStoppedMachineOnce(t *testing.T) {
	t.Parallel()
	tally := newRollLoop().rolled
	tally.stopped["i-1"], tally.stopped["i-1"], tally.stopped["i-2"] = true, true, true

	if got := tally.counts(); got != (RollCounts{Stopped: 2}) {
		t.Errorf("counts = %+v, want two stopped machines and nothing else", got)
	}
}

// TestApiMachinesLeavesOutTheMachinesWithoutAPeer keeps the other machines, in the order of their names, when some are
// in the set of the machines that the run knows to have no peer.
func TestApiMachinesLeavesOutTheMachinesWithoutAPeer(t *testing.T) {
	t.Parallel()
	listed := []cloud.Instance{
		apiServer("i-3", "prod-servers-3", 3), apiServer("i-1", "prod-servers-1", 1), apiServer("i-2", "prod-servers-2", 2),
	}

	got := apiMachines(apiModel, listed, nil, map[string]bool{"i-1": true, "i-9": true})

	var names []string
	for _, in := range got {
		names = append(names, in.Name)
	}
	if want := []string{"prod-servers-2", "prod-servers-3"}; !slices.Equal(names, want) {
		t.Errorf("apiMachines = %v, want %v", names, want)
	}
}

// removesPeers is a Nomad API of one server that notes the removals of peers it is asked for, with its address.
type removesPeers struct {
	nomadops.API
	addr  string
	calls *[]string
	err   error
}

func (p removesPeers) RemovePeer(_ context.Context, raftID string) error {
	*p.calls = append(*p.calls, p.addr+" "+raftID)
	return p.err
}

// removeRun returns a run over the servers one and two whose API reaches removesPeers stubs that answer err, with the
// API made, and the addresses that the stubs were asked to remove a peer at and the addresses built after that.
func removeRun(t *testing.T, err error, one, two cloud.Instance) (*rollRun, *[]string, *[]string) {
	t.Helper()
	r, built := apiRun(t, one, two)
	var calls []string
	r.s.Nomad = func(cfg nomadops.Config) (nomadops.API, error) {
		*built = append(*built, cfg.Address)
		return removesPeers{addr: cfg.Address, calls: &calls, err: err}, nil
	}
	if e := r.followAPI(); e != nil {
		t.Fatalf("followAPI: %v", e)
	}
	*built = nil
	return r, &calls, built
}

// removePeerOf is the step that removes the peer of the server of the machine i-1.
var removePeerOf = rollout.Step{
	Action: rollout.RemovePeer, Group: "servers", Machine: rollout.Machine{ID: "i-1", Name: "prod-servers-1"},
	Server: rollout.Server{ID: "raft-1"},
}

// TestCarryRemovePeerOfARunningMachineLeavesTheAPIBeforeTheCallIsSent makes the API again without the machine and notes
// it as having no peer, so that the call goes to the other server and not to the machine, which is the first by name.
func TestCarryRemovePeerOfARunningMachineLeavesTheAPIBeforeTheCallIsSent(t *testing.T) {
	t.Parallel()
	one, two := apiServer("i-1", "prod-servers-1", 1), apiServer("i-2", "prod-servers-2", 2)
	r, calls, built := removeRun(t, nil, one, two)

	if err := r.carry(t.Context(), removePeerOf); err != nil {
		t.Fatalf("carry: %v", err)
	}

	if want := []string{"203.0.113.2:4646 raft-1"}; !slices.Equal(*calls, want) {
		t.Errorf("the removals were sent as %v, want %v", *calls, want)
	}
	if want := []string{"203.0.113.2:4646"}; !slices.Equal(*built, want) || !slices.Equal(r.apiAt, want) {
		t.Errorf("the API was made over %v and is over %v, want over %v only", *built, r.apiAt, want)
	}
	if got := slices.Sorted(maps.Keys(r.unpeered)); !slices.Equal(got, []string{"i-1"}) {
		t.Errorf("the run knows %v to have no peer, want i-1", got)
	}
}

// TestCarryRemovePeerSendsNoCallWhenTheAPICannotLeaveTheMachine returns the error of the API, sends nothing and leaves
// the machine out of the set of those without a peer.
func TestCarryRemovePeerSendsNoCallWhenTheAPICannotLeaveTheMachine(t *testing.T) {
	t.Parallel()
	r, calls, _ := removeRun(t, nil, apiServer("i-1", "prod-servers-1", 1), apiServer("i-2", "prod-servers-2", 2))
	errNomad := errors.New("no client")
	r.s.Nomad = func(nomadops.Config) (nomadops.API, error) { return nil, errNomad }

	if err := r.carry(t.Context(), removePeerOf); !errors.Is(err, errNomad) {
		t.Errorf("carry = %v, want %v", err, errNomad)
	}

	if len(*calls) != 0 {
		t.Errorf("the removals were sent as %v, want none", *calls)
	}
	if len(r.unpeered) != 0 {
		t.Errorf("the run knows %v to have no peer, want none", slices.Sorted(maps.Keys(r.unpeered)))
	}
}

// TestCarryRemovePeerTakesTheMachineBackWhenTheCallFails keeps the error of the call and the machine out of the set of
// those without a peer, so that the next list makes the API over it again.
func TestCarryRemovePeerTakesTheMachineBackWhenTheCallFails(t *testing.T) {
	t.Parallel()
	errCall := errors.New("the server refused")
	r, calls, built := removeRun(t, errCall, apiServer("i-1", "prod-servers-1", 1), apiServer("i-2", "prod-servers-2", 2))
	r.kit.nodes = listsNodes{listed: r.listed}

	if err := r.carry(t.Context(), removePeerOf); !errors.Is(err, errCall) {
		t.Errorf("carry = %v, want %v", err, errCall)
	}

	if len(*calls) != 1 {
		t.Errorf("the removals were sent as %v, want one", *calls)
	}
	if len(r.unpeered) != 0 {
		t.Errorf("the run knows %v to have no peer, want none", slices.Sorted(maps.Keys(r.unpeered)))
	}
	*built = nil
	if err := r.list(t.Context()); err != nil {
		t.Fatalf("list: %v", err)
	}
	if want := []string{"203.0.113.1:4646", "203.0.113.2:4646"}; !slices.Equal(*built, want) {
		t.Errorf("the API was made over %v at the next list, want %v", *built, want)
	}
}

// TestCarryRemovePeerKeepsAStoppedMachineAmongThoseWithoutAPeerWhenTheCallFails leaves the set as it is when the call
// for a machine that the list shows stopped fails: the machine left the API when it stopped.
func TestCarryRemovePeerKeepsAStoppedMachineAmongThoseWithoutAPeerWhenTheCallFails(t *testing.T) {
	t.Parallel()
	errCall := errors.New("the server refused")
	one := apiServer("i-1", "prod-servers-1", 1)
	one.Ready = false
	r, _, _ := removeRun(t, errCall, one, apiServer("i-2", "prod-servers-2", 2))
	r.unpeered["i-1"] = true

	if err := r.carry(t.Context(), removePeerOf); !errors.Is(err, errCall) {
		t.Errorf("carry = %v, want %v", err, errCall)
	}

	if !r.unpeered["i-1"] {
		t.Error("the stopped machine is not among those without a peer any more")
	}
}

// TestCarryRemovePeerOfAStoppedMachineChangesNeitherTheSetNorTheAPI sends the call over the API as it is when the list
// shows the machine stopped, and when the run stopped it and the list still shows it running.
func TestCarryRemovePeerOfAStoppedMachineChangesNeitherTheSetNorTheAPI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(one *cloud.Instance, stopping map[string]time.Time)
	}{
		{"the list shows it stopped", func(one *cloud.Instance, _ map[string]time.Time) { one.Ready = false }},
		{"the run stopped it", func(_ *cloud.Instance, stopping map[string]time.Time) { stopping["i-1"] = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			one, two := apiServer("i-1", "prod-servers-1", 1), apiServer("i-2", "prod-servers-2", 2)
			stopping := map[string]time.Time{}
			tc.setup(&one, stopping)
			r, calls, built := removeRun(t, nil, one, two)
			maps.Copy(r.stopping, stopping)
			if err := r.followAPI(); err != nil {
				t.Fatalf("followAPI: %v", err)
			}
			*built = nil

			if err := r.carry(t.Context(), removePeerOf); err != nil {
				t.Fatalf("carry: %v", err)
			}

			if want := []string{"203.0.113.2:4646 raft-1"}; !slices.Equal(*calls, want) {
				t.Errorf("the removals were sent as %v, want %v", *calls, want)
			}
			if len(*built) != 0 || len(r.unpeered) != 0 {
				t.Errorf("the API was made over %v and the run knows %v to have no peer, want neither", *built,
					slices.Sorted(maps.Keys(r.unpeered)))
			}
		})
	}
}
