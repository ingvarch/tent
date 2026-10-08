package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// The limits of a rolling update. A wait that has no limit here fails when the run first meets it.
const (
	rollPoll       = 2 * time.Second // between two observations of a wait, and before a step is tried again
	drainGrace     = 5 * time.Minute // how long a wait for a drain lasts beyond the drain's deadline
	downTimeout    = 6 * time.Minute // how long a wait for Nomad to list a node as down lasts
	pendingTimeout = time.Minute     // how long the lists may miss a machine that the run created
	repeatLimit    = 3               // how often a step is tried in a row before the run ends
)

// rollAgain is what the operator does after a wait ran out.
const rollAgain = "run tent rolling-update cluster again to go on waiting"

// stepKey names a step by its action, its group, the machine or the node it acts on; the machine of a create by its
// name, which is all it has yet.
type stepKey struct {
	action               rollout.Action
	group, machine, node string
}

// keyOf returns the key of the step.
func keyOf(step rollout.Step) stepKey {
	machine := step.Machine.ID
	if step.Action == rollout.Create {
		machine = step.Machine.Name
	}
	return stepKey{action: step.Action, group: step.Group, machine: machine, node: step.Node.ID}
}

// seenWait is a wait that the run has met: the step and when the run first met it.
type seenWait struct {
	step  rollout.Step
	since time.Time
}

// pendingMachine is a machine that the run created and that no list has shown yet.
type pendingMachine struct {
	in    cloud.Instance
	since time.Time // when its create returned
}

// openWait is the wait whose start the run has reported and whose end it has not.
type openWait struct {
	key   stepKey
	step  rollout.Step
	event NomadEvent
}

// over reports whether the reading shows what the wait waits for. The decisions may give other steps while it is open,
// and the wait goes on until then.
func (w openWait) over(r *rollRun, reading nomadReading) bool {
	nomad := reading.state()
	nodes := nomad.Nodes
	node := slices.IndexFunc(nodes, func(n rollout.Node) bool { return n.ID == w.step.Node.ID })
	switch w.step.Action {
	case rollout.WaitJoined:
		in, ok := instanceByID(r.machines(), w.step.Machine.ID)
		return !ok || in.Joined
	case rollout.WaitDrained:
		return node < 0 || nodes[node].Status == "down" || nodes[node].DrainedFor == w.step.Machine.ID
	case rollout.WaitNodeDown:
		return node < 0 || nodes[node].Status == "down"
	case rollout.WaitHealthy:
		return !slices.ContainsFunc(nomad.Servers, func(s rollout.Server) bool { return s.Version == "" })
	}
	return true
}

// rollTally holds the IDs of the machines and nodes that a roll created, drained, deleted and purged, so that a step
// tried twice counts once.
type rollTally struct{ created, drained, deleted, purged map[string]bool }

// counts returns how many distinct machines and nodes the tally holds.
func (t rollTally) counts() RollCounts {
	return RollCounts{Created: len(t.created), Drained: len(t.drained), Deleted: len(t.deleted), Purged: len(t.purged)}
}

// rollLoop is what a rolling update remembers from one step to the next.
type rollLoop struct {
	pending  map[string]pendingMachine // by operation id; kept until a list shows the machine
	deleting map[string]time.Time      // the machines whose delete was sent and that the cloud still lists, by ID
	seen     map[stepKey]seenWait      // when the run first met each wait that has not ended
	last     stepKey                   // the step carried out last
	tries    int                       // how often in a row it was carried out
	lastErr  error                     // why the last try failed; nil when it did not
	settled  bool                      // the loop waited a poll since the last try
	relist   bool                      // the next observation lists the machines
	failing  time.Time                 // since when the reads of Nomad fail in a row; zero when they do not
	open     *openWait
	rolled   rollTally
}

// newRollLoop returns the memory of a run that has done nothing.
func newRollLoop() rollLoop {
	return rollLoop{
		pending: map[string]pendingMachine{}, deleting: map[string]time.Time{}, seen: map[stepKey]seenWait{},
		rolled: rollTally{
			created: map[string]bool{}, drained: map[string]bool{}, deleted: map[string]bool{}, purged: map[string]bool{},
		},
	}
}

// run carries out the steps of the rolling update until the decisions have none left, and returns what it did. It
// observes the cloud and Nomad, asks the decisions for a step, carries it out, and observes again; a wait polls every
// rollPoll until the decisions move on or its limit passes. A step that the decisions give again after it was carried
// out is tried again a poll later, and the third try in a row ends the run. It takes no lock.
func (r *rollRun) run(ctx context.Context) (RollCounts, error) {
	r.rollLoop = newRollLoop()
	err := r.loop(ctx)
	r.endWait(err)
	return r.rolled.counts(), err
}

// loop is the passes of run.
func (r *rollRun) loop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		reading, ok, err := r.observe(ctx)
		if err != nil {
			return err
		}
		if !ok {
			if err := r.sleep(ctx); err != nil {
				return err
			}
			continue
		}
		step, err := rollout.Next(r.state(reading), rollout.Roll)
		if err != nil {
			return err
		}
		if r.open != nil && r.open.key != keyOf(step) && r.open.over(r, reading) {
			r.endWait(nil)
		}
		for key, s := range r.seen {
			if key != keyOf(step) && (openWait{step: s.step}).over(r, reading) {
				delete(r.seen, key)
			}
		}
		if step.Action == rollout.Done {
			return nil
		}
		if err := r.refuseRole(step); err != nil {
			return err
		}
		if step.Action.Waits() {
			err = r.poll(ctx, step, reading)
		} else {
			err = r.act(ctx, step)
		}
		if err != nil {
			return err
		}
	}
}

// machines returns the machines of the last list and those that the run created and no list has shown yet.
func (r *rollRun) machines() []cloud.Instance {
	all := slices.Clone(r.listed)
	for _, op := range slices.Sorted(maps.Keys(r.pending)) {
		all = append(all, r.pending[op].in)
	}
	return all
}

// group returns the group of the run called name, and the zero group when the run has none of that name.
func (r *rollRun) group(name string) rollout.Group {
	i := slices.IndexFunc(r.groups, func(g rollout.Group) bool { return g.Name == name })
	if i < 0 {
		return rollout.Group{}
	}
	return r.groups[i]
}

// observe lists the machines when the last step changed them, or a machine that the run created is not listed yet or a
// machine that it deleted still is, and reads Nomad. It returns false, and no error, for reads that no server answered,
// until they have failed for nomadTimeout.
func (r *rollRun) observe(ctx context.Context) (nomadReading, bool, error) {
	if r.relist || len(r.pending) > 0 || len(r.deleting) > 0 {
		if err := r.list(ctx); err != nil {
			return nomadReading{}, false, err
		}
	}
	reading, err := readNomad(ctx, r.api)
	switch {
	case err == nil:
		r.failing = time.Time{}
		return reading, true, nil
	case errors.Is(err, nomadops.ErrNotReady):
		if r.failing.IsZero() {
			r.failing = time.Now()
		}
		if time.Since(r.failing) >= nomadTimeout {
			return nomadReading{}, false, err
		}
		return nomadReading{}, false, nil
	}
	return nomadReading{}, false, err
}

// list reads the machines, drops the pending machines that the cloud shows and the deleted ones that it no longer
// shows, and fails for a pending machine that the lists have missed for pendingTimeout.
func (r *rollRun) list(ctx context.Context) error {
	listed, err := r.kit.nodes.List(ctx, r.kit.cluster)
	if err != nil {
		return err
	}
	r.listed, r.relist = listed, false
	for op, p := range r.pending {
		if _, ok := instanceByID(listed, p.in.ID); ok {
			delete(r.pending, op)
		}
	}
	for id := range r.deleting {
		if _, ok := instanceByID(listed, id); !ok {
			delete(r.deleting, id)
		}
	}
	for _, op := range slices.Sorted(maps.Keys(r.pending)) {
		if p := r.pending[op]; time.Since(p.since) >= pendingTimeout {
			return fmt.Errorf("the cloud does not list node %s (ID %s), which this run created", p.in.Name, p.in.ID)
		}
	}
	return nil
}

// sleep waits one poll, or until ctx ends.
func (r *rollRun) sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(rollPoll):
		return nil
	}
}

// rollWait is a wait that the loop polls: how it is reported, how long it lasts, and what an error that ends it says.
type rollWait struct {
	event         NomadEvent
	limit         time.Duration
	subject, verb string // the error says "<subject> did not <verb> within <limit>"
}

// waitOf returns the wait of the step. It fails for a wait that has no limit.
func (r *rollRun) waitOf(step rollout.Step) (rollWait, error) {
	m, n := step.Machine, step.Node
	switch step.Action {
	case rollout.WaitJoined:
		return rollWait{NomadEvent{Action: NomadRegister, Node: m.Name}, nodeTimeout, "node " + m.Name, "join"}, nil
	case rollout.WaitDrained:
		g := r.group(step.Group)
		return rollWait{
			NomadEvent{Action: NomadDrained, Node: n.Name}, g.DrainTimeout + drainGrace, "node " + n.Name, "finish draining",
		}, nil
	case rollout.WaitNodeDown:
		return rollWait{
			NomadEvent{Action: NomadDown, Node: n.Name, Address: n.Address.String()}, downTimeout,
			fmt.Sprintf("node %s (%s)", n.Name, n.Address), "go down",
		}, nil
	case rollout.WaitHealthy:
		return rollWait{
			NomadEvent{Action: NomadHealthy, Voters: step.Voters}, nomadTimeout, "the servers", "become healthy and vote",
		}, nil
	}
	return rollWait{}, fmt.Errorf("no deadline for the wait %s", step)
}

// showing says what the reading of Nomad shows of what the wait step waits for.
func showing(step rollout.Step, reading nomadReading) string {
	if step.Action == rollout.WaitHealthy {
		state := "healthy"
		if !reading.health.Healthy {
			state = "not healthy"
		}
		return fmt.Sprintf("autopilot reports %d voters and the servers %s", reading.health.Voters, state)
	}
	i := slices.IndexFunc(reading.nodes, func(n nomadops.Node) bool {
		if step.Action == rollout.WaitJoined {
			return n.Is(step.Machine.Name, step.Machine.PrivateIP)
		}
		return n.ID == step.Node.ID
	})
	switch {
	case i >= 0 && reading.nodes[i].Draining:
		return "Nomad lists it draining"
	case i >= 0:
		return "Nomad lists it " + reading.nodes[i].Status
	case step.Action == rollout.WaitJoined:
		return "Nomad lists no node of that name at its address"
	}
	return "Nomad does not list it"
}

// poll is one pass of a wait: it reports the wait's start when no wait is open, fails once the wait's limit has passed
// since the run first met it (the wait that ran out becomes the open one, so the failure is reported on it), does what
// the wait for a machine to join does, and then sleeps a poll.
func (r *rollRun) poll(ctx context.Context, step rollout.Step, reading nomadReading) error {
	w, err := r.waitOf(step)
	if err != nil {
		return err
	}
	key := keyOf(step)
	if r.open == nil {
		r.open = &openWait{key: key, step: step, event: w.event}
		r.s.progress(Progress{Step: NodeStarted, Nomad: &w.event})
	}
	first, ok := r.seen[key]
	if !ok {
		first = seenWait{step: step, since: time.Now()}
		r.seen[key] = first
	}
	if time.Since(first.since) >= w.limit {
		r.open = &openWait{key: key, step: step, event: w.event}
		return fmt.Errorf("%s did not %s within %s (%s); %s", w.subject, w.verb, w.limit, showing(step, reading), rollAgain)
	}
	if step.Action == rollout.WaitJoined {
		if err := r.joinPoll(ctx, step, reading); err != nil {
			return err
		}
	}
	return r.sleep(ctx)
}

// joinPoll does what joinCheck says to the machine of the wait for a node to join: it repeats the machine's create,
// scrubs it, or does nothing.
func (r *rollRun) joinPoll(ctx context.Context, step rollout.Step, reading nomadReading) error {
	in, _ := instanceByID(r.machines(), step.Machine.ID)
	what, err := joinCheck(in, reading.nodes, r.s.now())
	if err != nil {
		return err
	}
	switch what {
	case joinCreate:
		return r.repeatCreate(ctx, in)
	case joinScrub:
		return r.scrub(ctx, in)
	}
	return nil
}

// repeatCreate repeats the create of the machine in with its operation id, which finds the machine and waits until it
// is ready, or makes it when the cloud has none with that id; a machine that it makes counts as created.
func (r *rollRun) repeatCreate(ctx context.Context, in cloud.Instance) error {
	g, _ := findGroup(r.model, in.Group)
	c, _ := waitFor(g, in)
	c.SpecHash = r.group(in.Group).SpecHash
	made, err := r.s.bootClient(ctx, r.kit, r, c)
	if err != nil {
		return err
	}
	if made.ID != in.ID {
		r.pending[in.Op] = pendingMachine{in: made, since: time.Now()}
		r.rolled.created[made.ID] = true
	}
	r.relist = true
	return nil
}

// scrub reports the wait for the machine's node as done, then replaces the machine's user data with the stub and labels
// it as joined, also on the copy of a machine that no list has shown yet.
func (r *rollRun) scrub(ctx context.Context, in cloud.Instance) error {
	r.endWait(nil)
	if err := r.s.markJoined(ctx, r.kit, in); err != nil {
		return err
	}
	for op, p := range r.pending {
		if p.in.ID == in.ID {
			p.in.Joined = true
			r.pending[op] = p
		}
	}
	r.relist = true
	return nil
}

// endWait reports the open wait, if there is one, as failed with err, or as done when err is nil.
func (r *rollRun) endWait(err error) {
	w := r.open
	if w == nil {
		return
	}
	r.open = nil
	if err != nil {
		r.s.progress(Progress{Step: NodeFailed, Err: err, Nomad: &w.event})
		return
	}
	r.s.progress(Progress{Step: NodeDone, Nomad: &w.event})
}

// act carries out the step that is no wait. The step on a machine whose delete was sent is that delete, which the cloud
// took and still lists: it is not sent again, and the loop lists at every poll until the cloud stops listing the
// machine. A step that the decisions give right after it was carried out waits a poll before it is tried again; the
// third try in a row ends the run. A write that fails because the node is gone or no server answered, and a create
// that failed before it sent anything because no server answered, wait a poll and count as a try; any other failure
// ends the run.
func (r *rollRun) act(ctx context.Context, step rollout.Step) error {
	key := keyOf(step)
	if since, sent := r.deleting[step.Machine.ID]; sent {
		return r.waitGone(ctx, step, since)
	}
	if key == r.last && !r.settled {
		r.settled = true
		return r.sleep(ctx)
	}
	if key == r.last && r.tries >= repeatLimit {
		if r.lastErr != nil {
			return fmt.Errorf("%s: %w", step, r.lastErr)
		}
		return fmt.Errorf("%s had no effect after %d tries", step, repeatLimit)
	}
	r.settled, r.lastErr = false, nil
	if key == r.last {
		r.tries++
	} else {
		r.last, r.tries = key, 1
	}
	err := r.carry(ctx, step)
	if err == nil {
		return nil
	}
	if !tryAgain(step, err) {
		return fmt.Errorf("%s: %w", step, err)
	}
	r.lastErr, r.settled = err, true
	return r.sleep(ctx)
}

// tryAgain reports whether the failure err of the step is one that a try a poll later may get past.
func tryAgain(step rollout.Step, err error) bool {
	switch step.Action {
	case rollout.Create:
		return errors.Is(err, errNotSent) && errors.Is(err, nomadops.ErrNotReady)
	case rollout.Delete:
		return false
	}
	return errors.Is(err, nomadops.ErrGone) || errors.Is(err, nomadops.ErrNotReady)
}

// waitGone waits a poll for the cloud to stop listing the machine whose delete was sent at since, and fails once
// goneTimeout has passed.
func (r *rollRun) waitGone(ctx context.Context, step rollout.Step, since time.Time) error {
	if time.Since(since) >= goneTimeout {
		return fmt.Errorf("the cloud still lists node %s (ID %s) %s after its delete", step.Machine.Name, step.Machine.ID,
			goneTimeout)
	}
	return r.sleep(ctx)
}

// carry sends the step to the cloud or to Nomad and reports it.
func (r *rollRun) carry(ctx context.Context, step rollout.Step) error {
	n := step.Node
	switch step.Action {
	case rollout.Create:
		return r.create(ctx, step)
	case rollout.MarkIneligible:
		return r.write(NomadEvent{Action: NomadIneligible, Node: n.Name}, func() error {
			return r.api.MarkIneligible(ctx, n.ID)
		})
	case rollout.Drain:
		req := nomadops.DrainRequest{Deadline: step.Deadline, Meta: map[string]string{drainMeta: step.Machine.ID}}
		return r.write(NomadEvent{Action: NomadDrain, Node: n.Name, Deadline: step.Deadline}, func() error {
			return counted(r.rolled.drained, n.ID, r.api.Drain(ctx, n.ID, req))
		})
	case rollout.Delete:
		return r.remove(ctx, step.Machine)
	case rollout.Purge:
		return r.write(NomadEvent{Action: NomadPurge, Node: n.Name, Address: n.Address.String()}, func() error {
			return counted(r.rolled.purged, n.ID, r.api.Purge(ctx, n.ID))
		})
	}
	return fmt.Errorf("no way to carry out the step %s", step)
}

// write reports the Nomad step that event describes as started, calls call and reports the step as done or failed.
func (r *rollRun) write(event NomadEvent, call func() error) error {
	return r.s.reportNomad(event, func() (NomadEvent, error) { return event, call() })
}

// counted puts id into set when err is nil, and returns err.
func counted(set map[string]bool, id string, err error) error {
	if err == nil {
		set[id] = true
	}
	return err
}

// createChange returns the create of the machine that the step names, with a new operation id. It carries the group's
// spec hash, so that the new machine is not outdated.
func (r *rollRun) createChange(step rollout.Step) NodeChange {
	m := step.Machine
	g, _ := findGroup(r.model, m.Group)
	return NodeChange{
		Action: NodeCreate, Name: m.Name, Group: m.Group, Role: m.Role, Zone: m.Zone, MachineType: g.MachineType,
		Image: g.Image, SpecHash: r.group(m.Group).SpecHash, Op: cloud.NewOpID(),
	}
}

// create creates the machine of the step and keeps it among the pending machines until a list shows it.
func (r *rollRun) create(ctx context.Context, step rollout.Step) error {
	c := r.createChange(step)
	in, err := r.s.bootClient(ctx, r.kit, r, c)
	if err != nil {
		return err
	}
	r.pending[c.Op] = pendingMachine{in: in, since: time.Now()}
	r.rolled.created[in.ID] = true
	return nil
}

// remove deletes the machine m, and from then on lists the machines at every observation until the cloud stops listing
// it.
func (r *rollRun) remove(ctx context.Context, m rollout.Machine) error {
	c := NodeChange{Action: NodeDelete, Name: m.Name, ID: m.ID}
	if err := r.s.applyNode(ctx, r.kit.nodes, r.kit.cluster, c); err != nil {
		return err
	}
	r.deleting[m.ID] = time.Now()
	r.rolled.deleted[m.ID] = true
	return nil
}

// seed returns the private addresses of the listed machines of the server and combined groups other than the node
// called name, by name.
func (r *rollRun) seed(name string, client bool) ([]netip.Addr, error) {
	servers := slices.DeleteFunc(slices.Clone(r.listed), func(in cloud.Instance) bool {
		return !isServerMachine(r.model, in)
	})
	slices.SortFunc(servers, compareName)
	return seedOf(r.kit.cluster, servers, name, client)
}

// nomadAPI returns the API over the servers that joined.
func (r *rollRun) nomadAPI() (nomadops.API, error) { return r.api, nil }
