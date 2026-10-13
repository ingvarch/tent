package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// nodeTimeout is how long the create of a node, or the wait for one, may take.
const nodeTimeout = 10 * time.Minute

// Progress is one thing that happened while an update, a delete or a rolling update applied its plan. When Infra is
// set, it is an event of an infrastructure change. When Nomad is set, it is a step that works on Nomad, of an update or
// of a rolling update, and Step and Err say how far it got and why it failed. When Going is set, a delete starts to
// wait until the cloud stops listing that many nodes that it deleted. Otherwise it is a step of the node change Node;
// for NodeScrub and NodeStop, which no plan holds, Node has only the action, the name and the machine's ID, and
// Instance is zero. Err says why a failed step failed. Instance is the machine of a create or a wait that is done, as
// the provider reports it, with its ID and its private address when the cloud gave one; it is the zero Instance for
// the other steps.
type Progress struct {
	Infra    *engine.Event
	Going    int
	Node     NodeChange
	Step     NodeStep
	Err      error
	Instance cloud.Instance
	Nomad    *NomadEvent
}

// NomadAction is what a step of an update or of a rolling update does in Nomad.
type NomadAction int

// Nomad actions.
const (
	// NomadLeader waits for the servers to elect a leader.
	NomadLeader NomadAction = iota + 1
	// NomadBootstrap bootstraps the ACL system.
	NomadBootstrap
	// NomadHealthy waits until the servers are healthy and vote.
	NomadHealthy
	// NomadRegister waits until a node has registered with the servers.
	NomadRegister
	// NomadKeyring waits until Nomad's keyring has an active key, so that intro tokens can be signed.
	NomadKeyring
	// NomadIneligible marks a node ineligible.
	NomadIneligible
	// NomadDrain drains a node.
	NomadDrain
	// NomadDrained waits until a node's drain completes.
	NomadDrained
	// NomadDown waits until Nomad lists a node as down.
	NomadDown
	// NomadPurge purges a node from Nomad.
	NomadPurge
	// NomadVote waits until a new server votes.
	NomadVote
	// NomadStable waits until the servers have been stable for the window in which every node learns of them.
	NomadStable
	// NomadTransfer moves the leadership to another server.
	NomadTransfer
	// NomadServerDown waits until autopilot no longer counts a stopped server as a healthy voter.
	NomadServerDown
	// NomadRemovePeer removes a server from the Raft configuration.
	NomadRemovePeer
	// NomadForceLeave forces a server out of the gossip pool.
	NomadForceLeave
	// NomadSettle waits for a refusal of the decisions of a rolling update or of a scale-down to clear.
	NomadSettle
	// NomadReconcile holds the stop of a server until the leader's next reconcile has passed.
	NomadReconcile
	// NomadJoin waits until a new combined node votes and has registered.
	NomadJoin
)

var nomadActionNames = [...]string{
	NomadLeader: "leader", NomadBootstrap: "bootstrap", NomadHealthy: "healthy", NomadRegister: "register",
	NomadKeyring: "keyring", NomadIneligible: "ineligible", NomadDrain: "drain", NomadDrained: "drained",
	NomadDown: "down", NomadPurge: "purge", NomadVote: "vote", NomadStable: "stable", NomadTransfer: "transfer",
	NomadServerDown: "server-down", NomadRemovePeer: "remove-peer", NomadForceLeave: "force-leave",
	NomadSettle: "settle", NomadReconcile: "reconcile", NomadJoin: "join",
}

// String returns the action's name in lower case, such as leader.
func (a NomadAction) String() string {
	if a < NomadLeader || int(a) >= len(nomadActionNames) {
		return fmt.Sprintf("NomadAction(%d)", int(a))
	}
	return nomadActionNames[a]
}

// MarshalText returns the action's name, as String does.
func (a NomadAction) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// NomadEvent is what a step that works on Nomad works on. Node names the node that a register, vote, join, ineligible,
// drain, drained, down, purge, transfer, server-down, remove-peer or reconcile step works on; for a force-leave step it
// is the member's name in the gossip pool. Address is that node's address, for a down and a purge step. Deadline is the
// deadline of the drain that a drain step starts, and how long a settle or reconcile wait lasts at most. Until is when
// the window of a stable step ends. Leader is the leader's RPC address once a leader wait is done, and for a transfer
// the node name of the server that takes the leadership. Voters is how many healthy servers a healthy wait waits for,
// and once it is done how many of them vote. Reason is the refusal of the decisions that a settle wait waits on.
type NomadEvent struct {
	Action   NomadAction
	Node     string
	Address  string
	Deadline time.Duration
	Until    time.Time
	Leader   string
	Voters   int
	Reason   string
}

// NodeStep is how far a node change got.
type NodeStep int

// Node steps. A node change sends NodeStarted, then NodeDone or NodeFailed.
const (
	NodeStarted NodeStep = iota + 1
	NodeDone
	NodeFailed
)

var nodeStepNames = [...]string{NodeStarted: "started", NodeDone: "done", NodeFailed: "failed"}

// String returns the step's name in lower case, such as started.
func (s NodeStep) String() string {
	if s < NodeStarted || int(s) >= len(nodeStepNames) {
		return fmt.Sprintf("NodeStep(%d)", int(s))
	}
	return nodeStepNames[s]
}

// Update brings a cluster to its specs: its infrastructure, such as its network, firewalls and SSH keys, its nodes, and
// Nomad on them. It loads the specs from the store, fills in the defaults, validates them, checks them against the
// cluster's channel, has the provider check them against its live API, and plans the changes. The completed spec, the
// specs with every default as last applied, counts as a change when the stored one is missing or differs. Its Nomad
// version is the one that the spec sets, else the one of the stored completed spec, else the one that the channel
// recommends; a stored one that the channel does not allow, or a stored completed spec that does not decode when its
// version is needed, fails the plan; so does a version in the spec that is more than one minor version above the one of
// the stored completed spec. Each of the cluster's secrets that the store lacks counts as a change too: the
// CA's key and bundle, the gossip key and the ACL bootstrap secret. A stored CA key without its bundle gets a bundle
// signed with it; a bundle without its key, or a stored secret that does not load, fails the plan, since tent never
// replaces a cluster's secrets.
//
// A node that is created or waited for with an operation id boots with the NodeConfig of its group. A plan that does so
// reads the release files that the nodes download, and fails when a node's user data does not fit what a provider
// takes. Every plan reads them, to name the machines that stay and whose spec hash is not their group's, or is missing,
// in Outdated: they are no change, and an update replaces none of them. A plan that creates no node, may remove no
// node that joined and cannot read the release files has no Outdated and no purge and goes on, and Update tells
// OnWarning once that it could not tell which nodes are outdated, unless the plan would write a Nomad version that the
// stored completed spec does not hold: then it fails, because tent pins a version only after it has read the release
// files of that version. A completed spec that is missing or does not decode holds none. A machine of any role without
// the joined label counts as not joined, and is waited for until its node joins and its user data is scrubbed; the wait
// repeats the create only for a machine that the cloud reports not ready. A client that has not joined and is older
// than the lifetime of its intro token, 31 minutes, may never have registered: when a server that stays has joined, the
// plan asks Nomad once, through those servers, about every such client of the cluster, whatever the plan does with it.
// A machine for which Nomad lists no node of its name and private address that is not down never registered. When its
// group is in the specs, a machine of its name whose group is in the specs too stays before it if that machine has
// joined, is not among those that never registered, or is older, and it then goes as a duplicate; otherwise it is
// deleted as not registered, does not count toward its group and frees its name for a create. A plan that cannot ask
// fails, with and without apply, before it changes anything; without a joined server it does not ask, and such a client
// that stays is waited for.
//
// A group with more machines than its size loses the machines that rollout.Surplus names. A machine that never joined
// is deleted. A machine that joined is removed: its node, if it runs a client, is marked ineligible and drained within
// its group's drain timeout, a server leaves the Raft configuration and the gossip pool, and then the machine is
// deleted and its node purged once Nomad lists it down. So are the clients that joined of a node group that the specs
// lack, drained within one hour. A plan of a cluster whose store holds the mark of the ACL bootstrap and that has a
// server which joined, runs and has a public address reads the client nodes of Nomad once, and plans the purge of each
// node that reads down and that no machine has, of a client or combined group or of a client group that the specs
// lack; a node that is not down is left alone. A read that fails is a warning, told once after the first plan, and
// leaves the plan without that purge, unless a stale client has to be judged, which fails the plan, or a node that
// joined may go. A plan that may remove a node that joined reads what a rolling update observes of Nomad instead, needs
// the release files, and asks the rollout decisions about the removals: it fails, with and without apply, before it
// changes anything, when a read fails, when no server runs to ask (unless no removal is among its changes), or when
// the decisions refuse. A plan that deletes a machine that carries the joined label and is no such removal fails the
// same way, and names each machine: one of a node group that the specs lack whose role is not client or that has no
// group label, and machines of one name that joined.
// A plan that creates a node or waits for one with an operation id, or that writes another Nomad version into the
// completed spec than the stored one, of a cluster whose store holds the mark of the ACL bootstrap and that has a
// server which joined, runs and has a public address, holds the Nomad version of the specs against what Nomad
// reports. It uses the reading of a removal when the plan made one, and else reads what a rolling update observes. It
// fails, with and without apply, before it changes anything, when a server or a node that is not down runs a newer
// Nomad than the version of the new node or of the new pin, when the new pin is more than one minor version above, or
// of another major version than, the oldest release that a server or a node that is not down runs, when a version
// that Nomad reports is no version number, and when the read fails. A server that reports no version is not compared.
// A cluster that cannot answer is not checked. When that reading shows a server on an older Nomad than the version of
// the specs, or a server that reports no version, and the plan boots a client, the plan stands and Update tells
// OnWarning once, after its first plan, that the client waits for the servers: the apply stops before it.
// The Nomad step is part of the plan until the store holds the mark of the ACL bootstrap, and also, with the mark, when
// no server or combined machine of the cluster is left: the servers that come are a new Nomad, so it bootstraps again.
// After the bootstrap it is part of the plan when the plan creates or waits for a server or combined node. Without
// apply, or when nothing changes, Update returns the plan and writes nothing.
// A plan fails, with and without apply, when the object of a server or combined group's highest name index holds
// no index.
//
// With apply it takes the cluster's lock and plans again under it. When that plan has changes, it calls OnUpdatePlan
// with it, then OnWarning with each warning about the cluster, such as a Nomad API that the whole internet may reach or
// a Nomad version that the channel has not tested. Then it raises the tent version, writes the missing secrets and the
// completed spec, raises the highest index of the machine names of each server and combined group to the highest among
// the listed machines (a create of such a machine raises it again, before the request), and applies the plan in this
// order: the infrastructure's changes other than its deletes; the deletion of a stale mark of the bootstrap; the waits
// that repeat a create, then the creates, of the server and combined nodes;
// the Nomad step, which waits for a leader, bootstraps the ACL system with the stored secret, waits for healthy servers
// that all vote and for an active key in Nomad's keyring, reads the Raft configuration when the plan has a server or
// combined change, and then, for each server and combined node of the plan in order, waits for a combined node to
// register, checks that its server votes at its private address, and replaces its user data with a stub and labels its
// machine as joined, and last writes the mark;
// then the waits for client nodes, the deletes of the clients that never registered and the creates of the missing
// clients; before the first change that boots a client, the update reads the Raft configuration and autopilot's report
// once. When Nomad cannot be asked, the update stops there, with what it did before standing. When that client would
// run a newer Nomad than a server runs, or a server reports no version yet, the update boots no client: it carries
// out the rest of these changes, the waits for clients without an operation id and the deletes of machines that never
// joined, and then returns the stop, before the removals and the infrastructure's deletes. Each client boots with an
// intro token, registers, and is then scrubbed and labelled as a server is, before the next is made; a wait for a
// client without an operation id asks for no token and calls no cloud until the scrub. Then come the other node
// deletes, which are those of machines that never joined. Each delete of a machine with a private address follows
// Nomad's answer that it has not joined, as a client of its name and private address that is not down or as a server
// of the Raft configuration, else the machine is scrubbed and labelled and the update fails; a machine without a
// private address counts as not joined, and its delete asks Nomad nothing. Then come the removals of the nodes that
// joined and the purges, which the loop of a rolling update carries out in the mode that removes surplus nodes, over
// a fresh list of the machines, one node at a time as the decisions order; the plan's Removed counts the nodes it
// drained and purged and the machines it stopped, also when the loop fails. Last come the infrastructure's deletes,
// so that a firewall group goes only once its nodes are gone.
// Nodes are created one at a time. A create, a wait and each wait of the Nomad step may take 10 minutes, and a scrub 5.
// The first step that fails stops the update, and running it again finishes the job. It returns the plan it applied,
// made under the lock, with the error; the plan says Applied once every step has succeeded, or at once when it has no
// changes.
func (s *Service) Update(ctx context.Context, cluster string, apply bool) (_ UpdatePlan, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return UpdatePlan{}, err
	}
	cache := assetCache{} // both plans of this run find the release files once
	u, err := s.planUpdate(ctx, l, cache)
	if err == nil {
		s.warn(u.readWarnings...)
	}
	switch {
	case err != nil || !apply:
		return u.plan, err
	case !u.plan.HasChanges():
		u.plan.Applied = true
		return u.plan, nil
	}
	err = s.locked(ctx, l, "update", func(ctx context.Context) error {
		var err error
		if u, err = s.planUpdate(ctx, l, cache); err != nil {
			return err
		}
		if u.plan.HasChanges() {
			if err := s.beginUpdate(u); err != nil {
				return err
			}
			if err := s.applyUpdate(ctx, l, &u); err != nil {
				return err
			}
		}
		u.plan.Applied = true
		return nil
	})
	return u.plan, err
}

// beginUpdate tells OnUpdatePlan the plan of u, then OnWarning each warning about the cluster. It returns the error
// of OnUpdatePlan.
func (s *Service) beginUpdate(u updateRun) error {
	if s.OnUpdatePlan != nil {
		if err := s.OnUpdatePlan(u.plan); err != nil {
			return err
		}
	}
	s.warn(u.warnings...)
	return nil
}

// updateRun is the plan of an update, with what applying it needs.
type updateRun struct {
	plan UpdatePlan
	nodeKit
	version   string // the Nomad version of the cluster's specs: that of the nodes the run boots
	layout    statestore.Layout
	completed []byte           // the completed spec
	warnings  []string         // about the cluster
	servers   []cloud.Instance // the machines of the server and combined groups that stay, by name
	listed    []cloud.Instance // every machine the cloud listed, for the machine that a wait names by ID
	removal   removalPlan      // what the apply carries out through the rollout decisions
	// readWarnings say what the plan could not read: the release files that tell the outdated machines, or the nodes
	// that deleted machines left; and which client waits for the servers. Update tells them once, after its first plan.
	readWarnings []string
	// staleMark is set when the store holds the mark of a bootstrap that the servers of the plan do not stand behind.
	staleMark bool
}

// loadedCluster is a cluster's stored specs with their defaults, checked against the channel, with the Nomad version
// pinned, and the model that they make.
type loadedCluster struct {
	objs   spec.Objects
	ch     *channels.Channel
	stored []byte // the stored completed spec; nil when the store has none
	// pinned is the Nomad version of the stored completed spec; "" when the store has none or it does not decode.
	pinned string
	m      *model.Cluster
}

// loadCluster loads a cluster's specs from the store, fills in the defaults, validates them and checks them against
// the cluster's channel, pins the Nomad version and makes the model, as Update says.
func (s *Service) loadCluster(ctx context.Context, l statestore.Layout) (loadedCluster, error) {
	objs, err := s.get(ctx, l, true)
	if err != nil {
		return loadedCluster{}, err
	}
	ch, err := checkCluster(objs.Cluster, objs.NodeGroups, s.Validate, s.channel)
	if err != nil {
		return loadedCluster{}, err
	}
	stored, err := s.readCompleted(ctx, l)
	if err != nil {
		return loadedCluster{}, err
	}
	pinned, err := pinVersion(objs.Cluster, ch, l, stored)
	if err != nil {
		return loadedCluster{}, err
	}
	m, err := model.New(objs.Cluster, objs.NodeGroups)
	if err != nil {
		return loadedCluster{}, err
	}
	return loadedCluster{objs: objs, ch: ch, stored: stored, pinned: pinned, m: m}, nil
}

// planUpdate loads and checks a cluster's specs, as Update says, and plans the changes that bring the cloud to them and
// the report of the outdated machines. The release files that it finds go into cache.
func (s *Service) planUpdate(ctx context.Context, l statestore.Layout, cache assetCache) (updateRun, error) {
	c, err := s.loadCluster(ctx, l)
	if err != nil {
		return updateRun{}, err
	}
	objs, ch, stored, m := c.objs, c.ch, c.stored, c.m
	p, err := s.provider(m.Provider)
	if err != nil {
		return updateRun{}, err
	}
	if err := p.Validate(ctx, objs.Cluster, objs.NodeGroups); err != nil {
		return updateRun{}, err
	}
	completed, err := spec.Encode(objs)
	if err != nil {
		return updateRun{}, fmt.Errorf("encode the completed spec of %s: %w", clusterLabel(m.Name), err)
	}
	secrets, err := s.planSecrets(ctx, l)
	if err != nil {
		return updateRun{}, err
	}
	names, err := s.readNames(ctx, l, serverGroups(m, nil))
	if err != nil {
		return updateRun{}, err
	}
	marked, err := s.bootstrapped(ctx, l)
	if err != nil {
		return updateRun{}, err
	}
	getBuilder := sync.OnceValues(func() (*nodeBuilder, error) {
		return s.planBuilder(ctx, p, m, objs, ch, secrets, cache)
	})
	in := planInput{
		access: nomadAccess{cluster: m.Name, region: objs.Cluster.Spec.Nomad.Region, secrets: secrets}, next: names.nexts(),
		marked: marked, specs: groupSpecs(objs.NodeGroups), version: objs.Cluster.Spec.Nomad.Version, builder: getBuilder,
	}
	plan, found, err := s.planChanges(ctx, p, m, in)
	if err != nil {
		return updateRun{}, err
	}
	if err := refuseJoinedDeletes(plan.Nodes, found.listed); err != nil {
		return updateRun{}, err
	}
	plan.Secrets, plan.Completed = relativePaths(l, secrets.writes), !bytes.Equal(stored, completed)
	plan.Nomad = planNomad(m, plan.Nodes, found.servers, marked)
	u := updateRun{
		plan: plan, version: objs.Cluster.Spec.Nomad.Version, layout: l, completed: completed,
		warnings: s.updateWarnings(objs.Cluster, objs.NodeGroups, ch), servers: found.servers, listed: found.listed,
		removal:   found.removal,
		staleMark: marked && plan.Nomad != nil && plan.Nomad.Bootstrap,
		nodeKit: nodeKit{
			cluster: m.Name, region: objs.Cluster.Spec.Nomad.Region, nodes: p.Nodes(), secrets: secrets, names: names,
		},
	}
	if found.warning != "" {
		u.readWarnings = append(u.readWarnings, found.warning)
	}
	changes := changesNodes(plan.Nodes)
	builder, err := getBuilder()
	switch {
	case err != nil && (changes || ctx.Err() != nil):
		return updateRun{}, err
	case err != nil && objs.Cluster.Spec.Nomad.Version != c.pinned:
		return updateRun{}, fmt.Errorf("%s: tent pins Nomad %s only after it has read the release files that the nodes "+
			"download: %w", clusterLabel(m.Name), objs.Cluster.Spec.Nomad.Version, err)
	case err != nil:
		u.readWarnings = append(u.readWarnings, couldNotTellOutdated+err.Error())
		return u, nil
	}
	waits, err := s.checkVersions(ctx, c, in, plan, found)
	if err != nil {
		return updateRun{}, err
	}
	if waits != "" {
		u.readWarnings = append(u.readWarnings, waits)
	}
	u.builder = builder
	if changes {
		if err := u.prepareNodes(m, s.now()); err != nil {
			return updateRun{}, err
		}
	}
	u.plan.Outdated = outdatedNodes(m, builder, staying(found.listed, plan.Nodes))
	return u, nil
}

// updateWarnings returns the warnings about the cluster c that an update tells before its first change: those of every
// change, and that of development variables that a release build ignores.
func (s *Service) updateWarnings(c *v1alpha1.Cluster, groups []*v1alpha1.NodeGroup, ch *channels.Channel) []string {
	w := warnings(c, groups, ch)
	if dev := devVariablesWarning(s.Version, s.Assets); dev != "" {
		w = append(w, dev)
	}
	return w
}

// bootstrapped reports whether the store holds the mark of the cluster's ACL bootstrap.
func (s *Service) bootstrapped(ctx context.Context, l statestore.Layout) (bool, error) {
	_, _, err := s.Store.Get(ctx, l.NomadBootstrapped())
	switch {
	case errors.Is(err, statestore.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// readCompleted returns the cluster's stored completed spec, or nil when the store lacks it.
func (s *Service) readCompleted(ctx context.Context, l statestore.Layout) ([]byte, error) {
	data, _, err := s.Store.Get(ctx, l.Completed())
	if errors.Is(err, statestore.ErrNotFound) {
		return nil, nil
	}
	return data, err
}

// pinVersion sets the Nomad version of the cluster c, which has its defaults and the channel ch, when its spec leaves
// it out: to the one pinned in stored, the cluster's stored completed spec, or else to the one that ch recommends. A
// pinned version that ch does not allow is an error, and so is a stored completed spec that does not decode or holds
// no Cluster: tent does not move a cluster to another Nomad version by itself. A version that the spec sets must not
// be more than one minor version above the pinned one. pinVersion returns the pinned version, "" when there is none;
// a version that the spec sets is checked against it only when the completed spec decodes.
func pinVersion(c *v1alpha1.Cluster, ch *channels.Channel, l statestore.Layout, stored []byte) (string, error) {
	n := &c.Spec.Nomad
	pinned, err := storedPin(l, stored)
	if n.Version != "" {
		if err := moveError(c.Metadata.Name, l, pinned, n.Version); err != nil {
			return "", err
		}
		return pinned, nil
	}
	if err != nil {
		return "", unreadablePin(l, err)
	}
	if pinned == "" {
		n.Version = ch.Nomad.Recommended
		return "", nil
	}
	if err := pinOutsideChannel(c.Metadata.Name, l, ch, pinned); err != nil {
		return "", err
	}
	n.Version = pinned
	return pinned, nil
}

// moveError returns the error of a cluster pinned to the Nomad version pinned whose spec sets the version to, when that
// is more than one minor version above the pin or of another major version, and nil otherwise.
func moveError(cluster string, l statestore.Layout, pinned, to string) error {
	why, advice := moveProblem(pinned, to)
	if why == "" {
		return nil
	}
	why += " (" + l.Completed() + ")"
	if advice != "" {
		why += "; " + advice
	}
	return fmt.Errorf("%s: spec.nomad.version: %s", clusterLabel(cluster), why)
}

// unreadablePin returns the error of a stored completed spec that does not decode, with what to do about it.
func unreadablePin(l statestore.Layout, err error) error {
	return fmt.Errorf("%w; set spec.nomad.version to the Nomad version the cluster was built with, or fix "+
		"%s in the state store by hand", err, l.Completed())
}

// pinOutsideChannel returns the error of a cluster pinned to the Nomad version pinned when the channel ch does not
// allow it, and nil otherwise.
func pinOutsideChannel(cluster string, l statestore.Layout, ch *channels.Channel, pinned string) error {
	// Allows returns nil or a *VersionError.
	v, ok := errors.AsType[*channels.VersionError](ch.Allows(pinned))
	if !ok {
		return nil
	}
	advice := "set spec.nomad.version to a version that the channel allows"
	if ch.OlderThanMinimum(pinned) {
		advice = "run tent upgrade cluster, or " + advice
	}
	return fmt.Errorf("%s is pinned to Nomad %s (%s), which %s; %s", clusterLabel(cluster), pinned, l.Completed(),
		v.Problem, advice)
}

// storedPin returns the Nomad version that the cluster's stored completed spec holds, "" when stored is nil. A
// completed spec that does not decode, or holds no Cluster, is an error that names its path.
func storedPin(l statestore.Layout, stored []byte) (string, error) {
	if stored == nil {
		return "", nil
	}
	objs, err := spec.Decode(stored)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s: %w", l.Completed(), err)
	case objs.Cluster == nil:
		return "", fmt.Errorf("%s: holds no %s", l.Completed(), v1alpha1.KindCluster)
	}
	return objs.Cluster.Spec.Nomad.Version, nil
}

// moveProblem says why a cluster pinned to the Nomad version pinned may not take the version to: it is more than one
// minor version above, or of another major version. It also says what to do, unless the version is of another major
// version. Both are empty when the move is allowed, and when pinned is empty.
func moveProblem(pinned, to string) (why, advice string) {
	v, ok := errors.AsType[*channels.VersionError](channels.CheckMove(pinned, to))
	if !ok {
		return "", ""
	}
	why = v.Error() + ", the Nomad version that the cluster is pinned to"
	if sameMajor(pinned, to) {
		advice = "move to " + strings.TrimSuffix(channels.NextMinor(pinned), ".0") + " first"
	}
	return why, advice
}

// sameMajor reports whether the Nomad versions a and b have the same major version.
func sameMajor(a, b string) bool {
	major, _, _ := strings.Cut(a, ".")
	return strings.HasPrefix(b, major+".")
}

// provider returns the cloud provider called name.
func (s *Service) provider(name v1alpha1.Provider) (cloud.Provider, error) {
	if s.Providers == nil {
		return nil, errors.New("no cloud providers are set up")
	}
	return s.Providers(name)
}

// machines are the machines of a cluster that an update plans over, and what the plan read of Nomad about them.
type machines struct {
	servers []cloud.Instance // those of the server and combined groups that stay, by name, as planNodes says
	listed  []cloud.Instance // all that the cloud listed, for the machine that a wait names by ID
	removal removalPlan      // what the apply carries out through the rollout decisions
	reading *nomadReading    // what the plan read of Nomad when that was what a roll observes, else nil
	warning string           // why the plan lacks the purge of the nodes that deleted machines left, or ""
}

// planChanges plans the changes that bring the cloud to the model m: those of the infrastructure, from a fresh
// inventory, and those of the nodes, from a fresh list of the machines, which it reads Nomad about as planNodeChanges
// says. It also returns the machines it planned over.
func (s *Service) planChanges(ctx context.Context, p cloud.Provider, m *model.Cluster, in planInput,
) (UpdatePlan, machines, error) {
	snap, err := p.Inventory(ctx, m.Name)
	if err != nil {
		return UpdatePlan{}, machines{}, err
	}
	tasks, err := p.BuildInfra(ctx, m)
	if err != nil {
		return UpdatePlan{}, machines{}, err
	}
	infra, err := engine.NewPlan(ctx, tasks, p.InfraKinds(), snap)
	if err != nil {
		return UpdatePlan{}, machines{}, err
	}
	instances, err := p.Nodes().List(ctx, m.Name)
	if err != nil {
		return UpdatePlan{}, machines{}, err
	}
	nodes, err := s.planNodeChanges(ctx, m, instances, in)
	if err != nil {
		return UpdatePlan{}, machines{}, err
	}
	found := machines{
		servers: nodes.servers, listed: instances, removal: nodes.removal, reading: nodes.reading, warning: nodes.warning,
	}
	return UpdatePlan{Infra: infra, Nodes: nodes.changes, Purges: nodes.purges}, found, nil
}

// applyUpdate applies the plan of u in the order that Update gives.
func (s *Service) applyUpdate(ctx context.Context, l statestore.Layout, u *updateRun) error {
	if u.needsNomad() && s.Nomad == nil {
		return errNoNomad
	}
	if err := statestore.RaiseVersion(ctx, s.Store, l, s.Version); err != nil {
		return err
	}
	if err := s.writeSecrets(ctx, u.secrets.writes); err != nil {
		return err
	}
	if u.plan.Completed {
		if _, err := s.Store.Put(ctx, l.Completed(), u.completed, statestore.PutOptions{}); err != nil {
			return fmt.Errorf("write the completed spec: %w", err)
		}
	}
	if err := u.names.raiseToListed(ctx, u.listed); err != nil {
		return err
	}
	opts := s.applyOptions()
	if err := u.plan.Infra.ApplyTaskChanges(ctx, opts); err != nil {
		return fmt.Errorf("apply the infrastructure: %w", err)
	}
	if u.staleMark {
		path := l.NomadBootstrapped()
		if err := s.Store.Delete(ctx, path); err != nil {
			return fmt.Errorf("delete %s: %w", path, err)
		}
	}
	a := &applier{s: s, u: *u, known: slices.Clone(u.servers)}
	err := a.run(ctx)
	u.plan.Removed = a.removed
	if err != nil {
		return err
	}
	if err := u.plan.Infra.ApplyDeletes(ctx, opts); err != nil {
		return fmt.Errorf("delete infrastructure objects: %w", err)
	}
	return nil
}

// applyOptions returns the options that apply an infrastructure plan and report the engine's events as progress.
func (s *Service) applyOptions() engine.ApplyOptions {
	return engine.ApplyOptions{OnEvent: func(e engine.Event) { s.progress(Progress{Infra: &e}) }}
}

// applyNode carries out the node change c of the cluster, which is a delete or a stop, and reports its steps, as
// applyNodeWith does. It has no user data to boot a machine with, so a create or a wait fails.
func (s *Service) applyNode(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange) error {
	_, err := s.applyNodeWith(ctx, nodes, cluster, c, nil)
	return err
}

// applyNodeWith carries out the node change c of the cluster and reports its steps: a failed one with its error, which
// applyNodeWith returns naming the node for a wait. A create or a wait boots the machine with the user data that
// prepare returns; its error fails the change, and so does a nil prepare. It returns the machine of a create or a wait.
func (s *Service) applyNodeWith(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange,
	prepare func(context.Context) (cloud.UserData, error),
) (cloud.Instance, error) {
	s.progress(Progress{Node: c, Step: NodeStarted})
	in, err := changeNode(ctx, nodes, cluster, c, prepare)
	if err != nil {
		s.progress(Progress{Node: c, Step: NodeFailed, Err: err})
		if c.Action == NodeWait {
			return cloud.Instance{}, fmt.Errorf("wait for node %s: %w", c.Name, err)
		}
		return cloud.Instance{}, err
	}
	s.progress(Progress{Node: c, Step: NodeDone, Instance: in})
	return in, nil
}

// changeNode carries out the node change c of the cluster: a create with its operation id, or with a new one when it
// has none; a wait, which repeats the create of the machine with its operation id; a delete; or a stop. It returns the
// machine of a create or a wait.
func changeNode(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange,
	prepare func(context.Context) (cloud.UserData, error),
) (cloud.Instance, error) {
	switch c.Action {
	case NodeCreate:
		op := c.Op
		if op == "" {
			op = cloud.NewOpID()
		}
		return createNode(ctx, nodes, cluster, c, op, prepare)
	case NodeWait:
		return createNode(ctx, nodes, cluster, c, c.Op, prepare)
	case NodeDelete:
		return cloud.Instance{}, nodes.Delete(ctx, cloud.Instance{ID: c.ID, Name: c.Name, Cluster: cluster})
	case NodeStop:
		return cloud.Instance{}, nodes.Stop(ctx, cloud.Instance{ID: c.ID, Name: c.Name, Cluster: cluster})
	}
	return cloud.Instance{}, fmt.Errorf("node %s: unknown change %s", c.Name, c.Action)
}

// createNode creates the machine of the node change c with the operation id op and the user data that prepare
// returns, or finds the one that an earlier call with op created, waits until it is ready and returns it. It gives the
// call nodeTimeout, which the preparation does not use. Without prepare it fails before it calls the cloud: a machine
// without user data would never join the cluster. A failure of the preparation matches errNotSent and keeps its text.
func createNode(ctx context.Context, nodes cloud.Nodes, cluster string, c NodeChange, op string,
	prepare func(context.Context) (cloud.UserData, error),
) (cloud.Instance, error) {
	if prepare == nil {
		return cloud.Instance{}, fmt.Errorf("node %s: no user data", c.Name)
	}
	userData, err := prepare(ctx)
	if err != nil {
		return cloud.Instance{}, notSentError{err}
	}
	ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	return nodes.Create(ctx, cloud.CreateRequest{
		Cluster: cluster, Group: c.Group, Role: c.Role, Zone: c.Zone, Name: c.Name, MachineType: c.MachineType,
		Image: c.Image, SpecHash: c.SpecHash, Op: op, UserData: userData,
	})
}

// progress tells OnProgress about p, when it is set.
func (s *Service) progress(p Progress) {
	if s.OnProgress != nil {
		s.OnProgress(p)
	}
}
