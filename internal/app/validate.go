package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
	"github.com/ingvarch/tent/internal/statestore"
)

// Names of the checks that a validation makes, in the order that a failure list shows them.
const (
	checkMachineMissing    = "machine-missing"
	checkMachineNotRunning = "machine-not-running"
	checkMachineSurplus    = "machine-surplus"
	checkMachineDuplicate  = "machine-duplicate"
	checkMachineUnknown    = "machine-unknown"
	checkNotJoined         = "not-joined"

	checkNomadNotSetUp       = "nomad-not-set-up"
	checkNoLeader            = "nomad-no-leader"
	checkServerNoVote        = "server-no-vote"
	checkServerUnknown       = "server-unknown"
	checkAutopilotUnhealthy  = "autopilot-unhealthy"
	checkServerNotAlive      = "server-not-alive"
	checkServerUnhealthy     = "server-unhealthy"
	checkClientNotRegistered = "client-not-registered"
	checkClientNotReady      = "client-not-ready"
	checkNomadVersion        = "nomad-version"
	checkCertificateExpired  = "certificate-expired"
)

// checkOrder lists the checks in the order that a failure list shows them.
var checkOrder = []string{
	checkMachineMissing, checkMachineNotRunning, checkMachineSurplus, checkMachineDuplicate, checkMachineUnknown,
	checkNotJoined, checkNomadNotSetUp, checkNoLeader, checkServerNoVote, checkServerUnknown, checkAutopilotUnhealthy,
	checkServerNotAlive, checkServerUnhealthy, checkClientNotRegistered, checkClientNotReady, checkNomadVersion,
	checkCertificateExpired,
}

// Failure is one way in which a cluster differs from its specs. Check names what was compared, such as
// machine-missing. Node is the name of the node it concerns, ID the cloud's ID of its machine, and Detail says what
// differs. A failure of the whole cluster has neither a node nor an ID.
type Failure struct {
	Check  string `json:"check"`
	Node   string `json:"node,omitempty"`
	ID     string `json:"id,omitempty"`
	Detail string `json:"detail"`
}

// Validation is what a check of a cluster found: its failures, the warnings about it and who holds its lock. Servers
// and Clients count the machines that stay under the specs by the role of their node group, a combined machine as
// both, and NomadVersion is the Nomad version that the cluster is pinned to.
type Validation struct {
	Cluster      string
	Servers      int
	Clients      int
	NomadVersion string
	Failures     []Failure
	Warnings     []string
	// Lock is the lease of the cluster's lock, or nil when the lock is free. It is the zero lease when the lock is
	// held and its holder cannot be named.
	Lock *statestore.Lease
}

// Valid reports whether the cluster has no failure.
func (v Validation) Valid() bool { return len(v.Failures) == 0 }

// WriteText writes the result for people: for a valid cluster the line "cluster prod is valid: 3 servers and 2
// clients run Nomad 2.0.7"; otherwise a table of the columns NODE and FAILURE, with "-" as the node of a failure of
// the whole cluster, a blank line and the line "cluster prod is not valid: 2 failures". Warnings and the lock are not
// part of it.
func (v Validation) WriteText(w io.Writer) error {
	if v.Valid() {
		return writeText(w, "the result", fmt.Sprintf("cluster %s is valid: %d %s and %d %s run Nomad %s\n",
			v.Cluster, v.Servers, serverNoun(v.Servers), v.Clients, clientNoun(v.Clients), v.NomadVersion))
	}
	nodes := make([]string, len(v.Failures))
	width := len("NODE")
	for i, f := range v.Failures {
		nodes[i] = cmp.Or(f.Node, "-")
		width = max(width, len(nodes[i]))
	}
	var b strings.Builder
	row := func(node, failure string) { fmt.Fprintf(&b, "%-*s  %s\n", width, node, failure) }
	row("NODE", "FAILURE")
	for i, f := range v.Failures {
		row(nodes[i], f.Detail)
	}
	fmt.Fprintf(&b, "\ncluster %s is not valid: %d %s\n", v.Cluster, len(v.Failures), failureNoun(len(v.Failures)))
	return writeText(w, "the result", b.String())
}

// clientNoun returns "client" for one and "clients" for any other count.
func clientNoun(n int) string {
	if n == 1 {
		return "client"
	}
	return "clients"
}

// failureNoun returns "failure" for one and "failures" for any other count.
func failureNoun(n int) string {
	if n == 1 {
		return "failure"
	}
	return "failures"
}

// MarshalJSON encodes the result as {"cluster": "prod", "valid": false, "servers": 3, "clients": 2, "nomadVersion":
// "2.0.7", "failures": [{"check": "not-joined", "node": "prod-workers-1", "id": "instance-5", "detail": "..."}],
// "warnings": ["..."], "lock": {...}}. Failures and warnings are lists even when empty, node and id are left out of a
// failure of the whole cluster, and lock is left out when the lock is free. It leaves HTML characters such as < and &
// as they are, so the caller's encoder decides whether to escape them.
func (v Validation) MarshalJSON() ([]byte, error) {
	return marshalJSON("the result", struct {
		Cluster      string            `json:"cluster"`
		Valid        bool              `json:"valid"`
		Servers      int               `json:"servers"`
		Clients      int               `json:"clients"`
		NomadVersion string            `json:"nomadVersion"`
		Failures     []Failure         `json:"failures"`
		Warnings     []string          `json:"warnings"`
		Lock         *statestore.Lease `json:"lock,omitempty"`
	}{
		v.Cluster, v.Valid(), v.Servers, v.Clients, v.NomadVersion, orEmpty(v.Failures), orEmpty(v.Warnings), v.Lock,
	})
}

// ValidateCluster checks a cluster against its specs, in one round. It loads the specs from the store as an update does
// and fails for the same reasons: the specs must be valid, and the channel must allow the Nomad version that the
// cluster is pinned to. It lists the cluster's machines in the cloud, and counts as a failure each change that an
// update would make to the nodes, and each machine that stays and that the cloud does not report as running. Then it
// checks Nomad and the certificates: see checkNomad. It reads the store, the cloud and Nomad and writes nothing, takes
// no lock and reads who holds it: Lock is that holder. A lock whose lease cannot be read is a warning, not a failure. A
// service without a Nomad client fails with an error.
//
// The warnings are those that every change of the cluster gives, the one about a cluster that runs in a single failure
// domain, those about certificates that end within 30 days, and the one about a lock that cannot be read. They are
// returned, and told to OnWarning. They never make the cluster invalid.
func (s *Service) ValidateCluster(ctx context.Context, cluster string) (_ Validation, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return Validation{}, err
	}
	c, err := s.loadCluster(ctx, l)
	if err != nil {
		return Validation{}, err
	}
	p, err := s.provider(c.m.Provider)
	if err != nil {
		return Validation{}, err
	}
	if s.Nomad == nil {
		return Validation{}, errNoNomad
	}
	instances, err := p.Nodes().List(ctx, c.m.Name)
	if err != nil {
		return Validation{}, err
	}
	lease, lockWarning, err := s.lockHolder(ctx, l)
	if err != nil {
		return Validation{}, err
	}
	set := planMachines(c.m, instances)
	found, err := s.checkNomad(ctx, l, c, set, s.now())
	if err != nil {
		return Validation{}, err
	}
	v := Validation{
		Cluster: c.m.Name, NomadVersion: c.objs.Cluster.Spec.Nomad.Version, Failures: set.failures(c.m),
		Warnings: warnings(c.objs.Cluster, c.objs.NodeGroups, c.ch), Lock: lease,
	}
	v.Failures = append(v.Failures, found.failures...)
	sortFailures(v.Failures)
	v.Servers, v.Clients = set.counts(c.m)
	if len(c.m.Zones) == 1 {
		v.Warnings = append(v.Warnings, "cluster "+c.m.Name+" runs in one failure domain, "+c.m.Zones[0]+
			": an outage there takes the whole cluster down")
	}
	v.Warnings = append(v.Warnings, found.warnings...)
	if lockWarning != "" {
		v.Warnings = append(v.Warnings, lockWarning)
	}
	s.warn(v.Warnings...)
	return v, nil
}

// lockHolder returns the lease of the cluster's lock without taking it: nil when the lock is free, and the zero lease
// when it is held and its holder cannot be named. A lease that cannot be read gives no lease and the warning that
// says so.
func (s *Service) lockHolder(ctx context.Context, l statestore.Layout) (*statestore.Lease, string, error) {
	lease, err := statestore.Holder(ctx, s.Store, l)
	switch {
	case ctx.Err() != nil && err != nil:
		return nil, "", err
	case errors.Is(err, statestore.ErrLocked):
		return &statestore.Lease{}, "", nil
	case err != nil:
		return nil, "tent could not " + err.Error(), nil
	}
	return lease, "", nil
}

// machineSet is the node changes that bring a cluster's machines to its specs, and the machines that stay.
type machineSet struct {
	owned   []cloud.Instance          // the machines with the cluster's label, oldest first
	keeps   map[string]cloud.Instance // the machine that keeps each name, as planNodes picks it
	changes []NodeChange              // as planNodes plans them
	stays   []cloud.Instance          // the owned machines that no change deletes, oldest first
}

// planMachines plans the node changes for the machines instances of the cluster m, as an update does without any
// machine that never registered.
func planMachines(m *model.Cluster, instances []cloud.Instance) machineSet {
	changes, _ := planNodes(m, instances, nil)
	set := machineSet{changes: changes}
	set.owned = slices.DeleteFunc(slices.Clone(instances), func(in cloud.Instance) bool { return in.Cluster != m.Name })
	slices.SortStableFunc(set.owned, compareAge)
	set.keeps = keepers(m, set.owned, nil)
	gone := map[string]bool{}
	for _, c := range changes {
		if c.Action == NodeDelete {
			gone[c.ID] = true
		}
	}
	for _, in := range set.owned {
		if !gone[in.ID] {
			set.stays = append(set.stays, in)
		}
	}
	return set
}

// counts returns the numbers of servers and clients among the machines that stay, by the role of each machine's node
// group in m: a machine of a combined group counts as both.
func (set machineSet) counts(m *model.Cluster) (servers, clients int) {
	return len(set.servers(m)), len(set.clients(m))
}

// failures returns the failures of the set, which is of the cluster m: the node changes that an update would make, and
// the machines that stay and that are not running or have not joined. They come in the order of checkOrder, then by
// node name, then by ID, whatever the order of the instances.
func (set machineSet) failures(m *model.Cluster) []Failure {
	have := map[string]int{} // the machines that stay, by node group
	for _, in := range set.stays {
		have[in.Group]++
	}
	var fs []Failure
	for _, c := range set.changes {
		switch c.Action {
		case NodeCreate:
			fs = append(fs, Failure{
				Check: checkMachineMissing, Node: c.Name,
				Detail: fmt.Sprintf("no machine: node group %s has %d of its %d", c.Group, have[c.Group], groupSize(m, c.Group)),
			})
		case NodeDelete:
			in, _ := instanceByID(set.owned, c.ID)
			fs = append(fs, deleteFailure(m, c, in, set.keeps[c.Name]))
		}
	}
	for _, in := range set.stays {
		if !in.Ready {
			fs = append(fs, Failure{
				Check: checkMachineNotRunning, Node: in.Name, ID: in.ID,
				Detail: "the cloud reports its machine (ID " + in.ID + ") as not running",
			})
		}
		if !in.Joined {
			fs = append(fs, Failure{
				Check: checkNotJoined, Node: in.Name, ID: in.ID,
				Detail: "machine ID " + in.ID + " has not joined Nomad: it carries no " + cloud.LabelJoined + " label",
			})
		}
	}
	sortFailures(fs)
	return fs
}

// sortFailures puts the failures in the order of checkOrder, then by node name, then by ID.
func sortFailures(fs []Failure) {
	slices.SortStableFunc(fs, func(a, b Failure) int {
		return cmp.Or(cmp.Compare(slices.Index(checkOrder, a.Check), slices.Index(checkOrder, b.Check)),
			strings.Compare(a.Node, b.Node), strings.Compare(a.ID, b.ID))
	})
}

// deleteFailure returns the failure of the delete c of the machine in. keeper is the machine that keeps the name, for a
// duplicate.
func deleteFailure(m *model.Cluster, c NodeChange, in, keeper cloud.Instance) Failure {
	f := Failure{Node: c.Name, ID: c.ID}
	id := "machine ID " + c.ID
	switch c.Reason {
	case reasonSurplus:
		f.Check = checkMachineSurplus
		f.Detail = fmt.Sprintf("%s is one more than the size of node group %s, %d", id, in.Group, groupSize(m, in.Group))
	case reasonDuplicate:
		f.Check = checkMachineDuplicate
		f.Detail = id + " has the name of machine ID " + keeper.ID
	default:
		f.Check = checkMachineUnknown
		f.Detail = id + " is of node group " + in.Group + ", which the specs do not have"
		if in.Group == "" {
			f.Detail = id + " has no node group label"
		}
	}
	return f
}

// groupSize returns the size of the node group called name in m; 0 for a group that m does not have.
func groupSize(m *model.Cluster, name string) int {
	g, _ := findGroup(m, name)
	return g.Size
}

// findGroup returns the node group of m called name, and false when m has none.
func findGroup(m *model.Cluster, name string) (model.NodeGroup, bool) {
	i := slices.IndexFunc(m.Groups, func(g model.NodeGroup) bool { return g.Name == name })
	if i < 0 {
		return model.NodeGroup{}, false
	}
	return m.Groups[i], true
}
