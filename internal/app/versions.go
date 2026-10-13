package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/rollout"
)

// reported is a Nomad version that a server, or a node that is not down, reports, and who reports it.
type reported struct{ who, version string }

// release returns the release X.Y.Z of a reported version, without its pre-release and build metadata; it returns ""
// for a version that is no version number.
func (r reported) release() string {
	return channels.Release(r.version)
}

// reports returns the versions that the servers and the nodes that are not down report.
func reports(n rollout.Nomad) []reported {
	var out []reported
	for _, s := range n.Servers {
		out = append(out, reported{"server " + s.Name, s.Version})
	}
	for _, node := range n.Nodes {
		if node.Status != nodeDown {
			out = append(out, reported{"node " + node.Name, node.Version})
		}
	}
	return out
}

// extreme returns the report whose release the order puts last, or false when no report is a version number: a server
// that reports none, as one does for a moment after it joins, is passed over. The first of equal reports wins.
func extreme(n rollout.Nomad, order func(a, b string) int) (reported, bool) {
	var best reported
	for _, r := range reports(n) {
		if rel := r.release(); rel != "" && (best.release() == "" || order(rel, best.release()) > 0) {
			best = r
		}
	}
	return best, best.release() != ""
}

// createPhrase starts the refusal of the plan that boots the node of c.
func createPhrase(c NodeChange) string { return "update would create node " + c.Name }

// pinPhrase starts the refusal of the plan that writes the Nomad version to into the completed spec of cluster in
// place of from, which is empty when the store holds none.
func pinPhrase(cluster, from, to string) string {
	label := "the Nomad version of " + clusterLabel(cluster)
	switch {
	case from == "":
		return fmt.Sprintf("update would set %s to %s", label, to)
	case lowersPin(from, to):
		return fmt.Sprintf("update would lower %s from %s to %s", label, from, to)
	}
	return fmt.Sprintf("update would change %s from %s to %s", label, from, to)
}

// lowersPin reports whether the plan writes an older Nomad version than the stored one.
func lowersPin(from, to string) bool { return from != "" && channels.Compare(to, from) < 0 }

// newerAdvice returns the advice for a version that is older than a server or a node reports, or "" when none is:
// the newest release that nomad reports. A word follows it when the plan lowers a pin. It gives none when a report is
// no version number.
func newerAdvice(n rollout.Nomad, version string, lowers bool) string {
	if slices.ContainsFunc(reports(n), func(r reported) bool { return r.version != "" && r.release() == "" }) {
		return "" // the refusal may name a report that is no version number, which no version in the spec answers
	}
	newest, ok := extreme(n, channels.Compare)
	if !ok || channels.Release(version) == "" || channels.Compare(newest.release(), version) <= 0 {
		return ""
	}
	if lowers {
		return "; set spec.nomad.version to " + newest.release() + " again and finish the roll with " +
			"tent rolling-update cluster"
	}
	return "; set spec.nomad.version to " + newest.release() + " or newer"
}

// checkNewNodes fails when a node that the changes boot would run an older Nomad than a server, or a node that is
// not down, reports in nomad, or when a version of that report is no version number: see rollout.CheckNewNode.
func checkNewNodes(changes []NodeChange, version string, nomad rollout.Nomad) error {
	i := slices.IndexFunc(changes, bootsMachine)
	if i < 0 {
		return nil
	}
	if _, err := rollout.CheckNewNode(nomad, version, false); err != nil {
		return fmt.Errorf("%s: %w%s", createPhrase(changes[i]), err, newerAdvice(nomad, version, false))
	}
	return nil
}

// checkPinMove fails when the plan would write the Nomad version to into the completed spec of cluster in place of
// from, which is empty when the store holds none, while nomad reports a server, or a node that is not down, on a newer
// Nomad than to, or on a release that is more than one minor version older than to.
func checkPinMove(cluster, from, to string, nomad rollout.Nomad) error {
	if from == to {
		return nil
	}
	what := pinPhrase(cluster, from, to)
	if _, err := rollout.CheckNewNode(nomad, to, false); err != nil {
		return fmt.Errorf("%s: %w%s", what, err, newerAdvice(nomad, to, lowersPin(from, to)))
	}
	oldest, ok := extreme(nomad, func(a, b string) int { return channels.Compare(b, a) })
	if !ok {
		return nil
	}
	err := channels.CheckMove(oldest.release(), to)
	if err == nil {
		return nil
	}
	back := "a version below " + channels.NextMinor(channels.NextMinor(oldest.release()))
	if from != "" {
		back = from + " again"
	}
	return fmt.Errorf("%s: %w, which %s still runs; set spec.nomad.version to %s, roll every node with "+
		"tent rolling-update cluster, and move then", what, err, oldest.who, back)
}

// couldNotAskVersions starts the cause of a read of the versions that failed.
const couldNotAskVersions = "tent could not ask Nomad which versions run: "

// versionCheck is the check of the versions that an apply makes when its plan could not read Nomad.
type versionCheck func(ctx context.Context) error

// checkVersions holds the Nomad version of the specs against what Nomad reports, for the changes of a plan: a plan
// that boots a node needs a version that is not older than the Nomad of a server or of a node that is not down, and a
// plan that writes another version into the completed spec than the stored one needs the rules of checkPinMove. It
// reads what the plan read already (found), else what a roll observes. A cluster that cannot answer, because the store
// holds no mark of its bootstrap or no server runs with a public address, gets no check. A read that fails ends the
// plan. When no server answers, the error carries the hint for spec.access.api, unless the plan has an
// infrastructure change other than a delete: then the plan stands with a warning, and owed is the check that the
// apply makes once it has applied those changes. The warning is otherwise empty unless the plan boots a client that
// clientAhead says waits for the servers.
func (s *Service) checkVersions(ctx context.Context, c loadedCluster, in planInput, plan UpdatePlan, found machines,
) (warning string, owed versionCheck, err error) {
	to := c.objs.Cluster.Spec.Nomad.Version
	if !in.marked || !slices.ContainsFunc(plan.Nodes, bootsMachine) && to == c.pinned {
		return "", nil, nil
	}
	reading := found.reading
	if reading == nil {
		read, err := s.readVersions(ctx, c, in, plan.Nodes, found.listed)
		switch {
		case errors.Is(err, errNoServerToAsk):
			return "", nil, nil
		case errors.Is(err, nomadops.ErrNotReady) && infraRunsFirst(plan):
			warning = couldNotAskVersions + err.Error() +
				"; update asks again once it has applied the infrastructure's changes, before it writes the " +
				"completed spec or creates a node"
			owed = func(ctx context.Context) error { return s.askVersions(ctx, c, in, plan.Nodes, found.listed) }
			return warning, owed, nil
		case err != nil:
			return "", nil, cannotRead(c, plan.Nodes, err)
		}
		reading = &read
	}
	nomad := reading.state()
	if err := holdVersions(c, plan.Nodes, nomad); err != nil {
		return "", nil, err
	}
	if node, why := clientAhead(plan.Nodes, to, nomad); why != nil {
		return waitWarning(node, why), nil, nil
	}
	return "", nil, nil
}

// infraRunsFirst reports whether the plan has an infrastructure change other than a delete: the changes that an apply
// which owes the check of the versions carries out before that check.
func infraRunsFirst(p UpdatePlan) bool {
	if p.Infra == nil {
		return false
	}
	s := p.Infra.Summary()
	return s.Create+s.Update+s.Replace > 0
}

// askVersions is the check of the versions that an apply owes: it reads what a roll observes and holds the specs'
// version against it, as checkVersions does. A read that fails, and a refusal, are its errors.
func (s *Service) askVersions(ctx context.Context, c loadedCluster, in planInput, nodes []NodeChange,
	listed []cloud.Instance,
) error {
	read, err := s.readVersions(ctx, c, in, nodes, listed)
	if err != nil {
		return cannotRead(c, nodes, err)
	}
	return holdVersions(c, nodes, read.state())
}

// readVersions reads what a roll observes of Nomad, through the servers among the listed machines.
func (s *Service) readVersions(ctx context.Context, c loadedCluster, in planInput, nodes []NodeChange,
	listed []cloud.Instance,
) (nomadReading, error) {
	return readForRemoval(ctx, c.m, listed, nodes, s.nomadAsker(in.access))
}

// holdVersions fails when the plan's nodes or its pin break a rule of checkNewNodes or checkPinMove against nomad.
func holdVersions(c loadedCluster, nodes []NodeChange, nomad rollout.Nomad) error {
	to := c.objs.Cluster.Spec.Nomad.Version
	if err := checkNewNodes(nodes, to, nomad); err != nil {
		return err
	}
	return checkPinMove(c.m.Name, c.pinned, to, nomad)
}

// cannotRead is the error of a plan, or of an apply that owes the check, when the read of Nomad failed with err. It
// carries the hint for spec.access.api when no server answered.
func cannotRead(c loadedCluster, nodes []NodeChange, err error) error {
	what := pinPhrase(c.m.Name, c.pinned, c.objs.Cluster.Spec.Nomad.Version)
	if i := slices.IndexFunc(nodes, bootsMachine); i >= 0 {
		what = createPhrase(nodes[i])
	}
	err = fmt.Errorf("%s, and %s%w", what, couldNotAskVersions, err)
	if errors.Is(err, nomadops.ErrNotReady) {
		return withReachHint(err)
	}
	return err
}

// errServerWithoutVersion is why a client waits for the servers when one of them reports no Nomad version yet.
var errServerWithoutVersion = errors.New("a server reports no Nomad version yet")

// clientAhead returns the first client that the changes boot and why it waits: the refusal of rollout.CheckNewNode
// when a server reports an older Nomad than version, or a server or a node that is not down a newer one, and
// errServerWithoutVersion when a server reports no version. It returns "" and nil when no client has to wait. A
// server or combined node is never held back.
func clientAhead(changes []NodeChange, version string, nomad rollout.Nomad) (node string, why error) {
	i := slices.IndexFunc(changes, func(c NodeChange) bool { return bootsMachine(c) && !isServerChange(c) })
	if i < 0 {
		return "", nil
	}
	unknown, err := rollout.CheckNewNode(nomad, version, true)
	if err == nil && unknown {
		err = errServerWithoutVersion
	}
	if err != nil {
		return changes[i].Name, err
	}
	return "", nil
}

// aheadAdvice splits why, the refusal that clientAhead returns, into its reason and the commands that follow it; the
// advice is "" when no command helps.
func aheadAdvice(why error) (reason, advice string) {
	if errors.Is(why, errServerWithoutVersion) {
		return why.Error(), "run tent update cluster again"
	}
	if reason, ok := strings.CutSuffix(why.Error(), "; roll the servers first"); ok {
		return reason, "run tent rolling-update cluster, then tent update cluster again"
	}
	return why.Error(), ""
}

// waitWarning tells that the plan creates the client node, which waits for the servers, and that the apply stops
// before it.
func waitWarning(node string, why error) string {
	reason, advice := aheadAdvice(why)
	msg := fmt.Sprintf("node %s waits for the servers: %s; this update stops before it", node, reason)
	if advice != "" {
		msg += ": " + advice
	}
	return msg
}

// stopsBefore starts the error of an apply that stops before the client node.
func stopsBefore(node string) string { return "update stops before node " + node }

// cannotAsk is the error of an apply that stops before the client node because Nomad did not answer with err.
func cannotAsk(node string, err error) error {
	return fmt.Errorf("%s: tent could not ask Nomad which versions its servers run: %w", stopsBefore(node), err)
}

// stopError is the error of an apply that stops before the client node because of why.
func stopError(node string, why error) error {
	reason, advice := aheadAdvice(why)
	msg := stopsBefore(node) + ": " + reason
	if advice != "" {
		msg += "; " + advice
	}
	return errors.New(msg)
}
