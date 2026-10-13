package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ingvarch/tent/internal/channels"
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

// checkVersions holds the Nomad version of the specs against what Nomad reports, for the changes of a plan: a plan
// that boots a node needs a version that is not older than the Nomad of a server or of a node that is not down, and a
// plan that writes another version into the completed spec than the stored one needs the rules of checkPinMove. It
// reads what the plan read already (found), else what a roll observes. A cluster that cannot answer, because the store
// holds no mark of its bootstrap or no server runs with a public address, gets no check. A read that fails ends the
// plan.
func (s *Service) checkVersions(ctx context.Context, c loadedCluster, in planInput, plan UpdatePlan, found machines,
) error {
	to := c.objs.Cluster.Spec.Nomad.Version
	boot := slices.IndexFunc(plan.Nodes, bootsMachine)
	moves := to != c.pinned
	if !in.marked || boot < 0 && !moves {
		return nil
	}
	reading := found.reading
	if reading == nil {
		read, err := readForRemoval(ctx, c.m, found.listed, plan.Nodes, s.nomadAsker(in.access))
		switch {
		case errors.Is(err, errNoServerToAsk):
			return nil
		case err != nil:
			what := pinPhrase(c.m.Name, c.pinned, to)
			if boot >= 0 {
				what = createPhrase(plan.Nodes[boot])
			}
			return fmt.Errorf("%s, and tent could not ask Nomad which versions run: %w", what, err)
		}
		reading = &read
	}
	nomad := reading.state()
	if err := checkNewNodes(plan.Nodes, to, nomad); err != nil {
		return err
	}
	return checkPinMove(c.m.Name, c.pinned, to, nomad)
}
