package app

import (
	"fmt"
	"io"
)

// UpgradePlan is what an upgrade of a cluster's Nomad version finds and does.
type UpgradePlan struct {
	Cluster     string
	Channel     string // the cluster's release channel
	Recommended string // the Nomad version that the channel recommends
	Spec        string // spec.nomad.version of the stored cluster.yaml; "" when the spec leaves it out
	Pinned      string // the Nomad version of the stored completed spec; "" when the store has none
	Current     string // the cluster's version: Spec, else Pinned; "" for a cluster without a version yet
	Target      string // the version that the upgrade writes into spec.nomad.version; "" when nothing is to upgrade
	Applied     bool   // the upgrade wrote Target, or was asked to and had nothing to write
}

// Due reports whether the plan has a version to write.
func (p UpgradePlan) Due() bool { return p.Target != "" }

// WriteText writes the plan for people. A plan with a version to write is two lines, such as "cluster prod: upgrade
// Nomad from 2.0.6 to 2.0.7, which channel stable recommends." and the commands that follow; once applied, it is
// "cluster prod: spec.nomad.version is now 2.0.7; the cluster was on Nomad 2.0.6." and the commands that follow. A
// plan without a version to write says why in one line, such as "cluster prod: nothing to upgrade: its Nomad version
// is 2.0.7, which channel stable recommends.", and adds a line when the spec sets a version and the pin is another
// one.
func (p UpgradePlan) WriteText(w io.Writer) error {
	return writeText(w, "the plan", p.text())
}

// text returns the plan's text for people.
func (p UpgradePlan) text() string {
	const next = "tent update cluster, then tent rolling-update cluster."
	prefix := "cluster " + p.Cluster + ": "
	switch {
	case p.Due() && p.Applied:
		return fmt.Sprintf("%sspec.nomad.version is now %s; the cluster was on Nomad %s.\nNext: %s\n",
			prefix, p.Target, p.Current, next)
	case p.Due():
		return fmt.Sprintf("%supgrade Nomad from %s to %s, which channel %s recommends.\nAfter the upgrade: %s\n",
			prefix, p.Current, p.Target, p.Channel, next)
	}
	prefix += "nothing to upgrade: "
	switch p.Current {
	case "":
		return fmt.Sprintf("%sit has no Nomad version yet; its first tent update cluster takes %s.\n",
			prefix, p.Recommended)
	case p.Recommended:
		return fmt.Sprintf("%sits Nomad version is %s, which channel %s recommends.\n",
			prefix, p.Current, p.Channel) + p.unappliedLine()
	default:
		return fmt.Sprintf("%sits Nomad version is %s, newer than the %s that channel %s recommends.\n",
			prefix, p.Current, p.Recommended, p.Channel) + p.unappliedLine()
	}
}

// unappliedLine returns the line that says tent update cluster has not applied the spec's version, or nothing when
// the spec or the pin is missing, or they agree.
func (p UpgradePlan) unappliedLine() string {
	if p.Spec == "" || p.Pinned == "" || p.Spec == p.Pinned {
		return ""
	}
	return fmt.Sprintf("tent update cluster has not applied Nomad %s yet: the cluster is pinned to %s.\n",
		p.Spec, p.Pinned)
}

// MarshalJSON encodes the plan as {"cluster": "prod", "channel": "stable", "recommended": "2.0.7", "spec": "2.0.6",
// "pinned": "2.0.6", "current": "2.0.6", "target": "2.0.7", "applied": true}. Spec, pinned, current and target are
// left out when empty, and applied unless it is true. It leaves HTML characters such as < and & as they are, so the
// caller's encoder decides whether to escape them.
func (p UpgradePlan) MarshalJSON() ([]byte, error) {
	return marshalJSON("the plan", struct {
		Cluster     string `json:"cluster"`
		Channel     string `json:"channel"`
		Recommended string `json:"recommended"`
		Spec        string `json:"spec,omitempty"`
		Pinned      string `json:"pinned,omitempty"`
		Current     string `json:"current,omitempty"`
		Target      string `json:"target,omitempty"`
		Applied     bool   `json:"applied,omitempty"`
	}{p.Cluster, p.Channel, p.Recommended, p.Spec, p.Pinned, p.Current, p.Target, p.Applied})
}
