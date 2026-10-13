package app

import (
	"errors"
	"testing"

	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/rollout"
)

// versionsOn returns what Nomad reports with one server on each of the server versions, named prod-servers-<n>.global,
// and one node of each of the node versions, named prod-workers-<n>, all ready.
func versionsOn(servers []string, nodes ...string) rollout.Nomad {
	var n rollout.Nomad
	for i, v := range servers {
		n.Servers = append(n.Servers, rollout.Server{Name: "prod-servers-" + string(rune('0'+i)) + ".global", Version: v})
	}
	for i, v := range nodes {
		n.Nodes = append(n.Nodes, rollout.Node{Name: "prod-workers-" + string(rune('0'+i)), Status: "ready", Version: v})
	}
	return n
}

// withNodeDown returns n with its node called name read as down.
func withNodeDown(n rollout.Nomad, name string) rollout.Nomad {
	nodes := append([]rollout.Node(nil), n.Nodes...)
	for i := range nodes {
		if nodes[i].Name == name {
			nodes[i].Status = nodeDown
		}
	}
	n.Nodes = nodes
	return n
}

var (
	createWorker2 = NodeChange{Action: NodeCreate, Name: "prod-workers-2", Group: "workers"}
	createWorker3 = NodeChange{Action: NodeCreate, Name: "prod-workers-3", Group: "workers"}
	repeatWorker2 = NodeChange{Action: NodeWait, Name: "prod-workers-2", ID: "7", Op: "op-2"}
	waitWorker1   = NodeChange{Action: NodeWait, Name: "prod-workers-1", ID: "6"}
	deleteWorker0 = NodeChange{Action: NodeDelete, Name: "prod-workers-0", ID: "5", Reason: reasonSurplus}
)

// TestCheckNewNodes checks that a change which boots a node is held against the Nomad versions that a report shows,
// and the text of each refusal.
func TestCheckNewNodes(t *testing.T) {
	t.Parallel()
	const olderThanServer = "update would create node prod-workers-2: tent never moves a node to an older Nomad: " +
		"the cluster's version is 2.0.6, and server prod-servers-0.global runs 2.0.7; " +
		"set spec.nomad.version to 2.0.7 or newer"
	tests := []struct {
		name    string
		changes []NodeChange
		version string
		nomad   rollout.Nomad
		want    string // "" for no error
	}{
		{"nothing is booted", []NodeChange{waitWorker1, deleteWorker0}, "2.0.6", versionsOn([]string{"2.0.7"}, "2.0.7"), ""},
		{"no change", nil, "2.0.6", versionsOn([]string{"2.0.7"}), ""},
		{"a create beside a newer server", []NodeChange{createWorker2}, "2.0.6", versionsOn([]string{"2.0.7"}),
			olderThanServer},
		{"a repeated create beside a newer server", []NodeChange{repeatWorker2}, "2.0.6", versionsOn([]string{"2.0.7"}),
			olderThanServer},
		{"the first boot is named", []NodeChange{waitWorker1, createWorker2, createWorker3}, "2.0.6",
			versionsOn([]string{"2.0.7"}), olderThanServer},
		{"a create beside a newer node", []NodeChange{createWorker2}, "2.0.6", versionsOn([]string{"2.0.6"}, "2.0.7"),
			"update would create node prod-workers-2: tent never moves a node to an older Nomad: " +
				"the cluster's version is 2.0.6, and node prod-workers-0 runs 2.0.7; " +
				"set spec.nomad.version to 2.0.7 or newer"},
		{"a newer node that is down does not count", []NodeChange{createWorker2}, "2.0.6",
			withNodeDown(versionsOn([]string{"2.0.6"}, "2.0.7"), "prod-workers-0"), ""},
		{"the advice names the newest", []NodeChange{createWorker2}, "2.0.6",
			versionsOn([]string{"2.0.7", "2.1.0"}, "2.0.9"),
			"update would create node prod-workers-2: tent never moves a node to an older Nomad: " +
				"the cluster's version is 2.0.6, and server prod-servers-0.global runs 2.0.7; " +
				"set spec.nomad.version to 2.1.0 or newer"},
		{"the same version", []NodeChange{createWorker2}, "2.0.7", versionsOn([]string{"2.0.7"}, "2.0.7"), ""},
		{"an older server and node", []NodeChange{createWorker2}, "2.0.7", versionsOn([]string{"2.0.6"}, "2.0.6"), ""},
		{"a server without a version is not compared", []NodeChange{createWorker2}, "2.0.6",
			versionsOn([]string{""}), ""},
		{"a report that is no version number", []NodeChange{createWorker2}, "2.0.6", versionsOn([]string{"soon"}),
			`update would create node prod-workers-2: the Nomad version of server prod-servers-0.global is "soon", ` +
				"which is not a version number"},
		{"no advice beside a report that is no version number", []NodeChange{createWorker2}, "2.0.6",
			versionsOn([]string{"soon", "2.0.7"}),
			`update would create node prod-workers-2: the Nomad version of server prod-servers-0.global is "soon", ` +
				"which is not a version number"},
		{"a version that is no version number", []NodeChange{createWorker2}, "latest", versionsOn([]string{"2.0.7"}),
			`update would create node prod-workers-2: the Nomad version of a new node is "latest", ` +
				"which is not a version number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkNewNodes(tt.changes, tt.version, tt.nomad)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("checkNewNodes = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("checkNewNodes = %v, want %q", err, tt.want)
			}
			if !errors.Is(err, rollout.ErrRefused) {
				t.Errorf("error %q does not match rollout.ErrRefused", err)
			}
		})
	}
}

// TestCheckPinMove checks the two rules that hold a new pin against what Nomad reports, and the text of each refusal.
func TestCheckPinMove(t *testing.T) {
	t.Parallel()
	const lowerBesideServer = "update would lower the Nomad version of cluster prod from 2.0.7 to 2.0.6: " +
		"tent never moves a node to an older Nomad: the cluster's version is 2.0.6, and server " +
		"prod-servers-3.global runs 2.0.7; set spec.nomad.version to 2.0.7 again and finish the roll with " +
		"tent rolling-update cluster"
	tests := []struct {
		name     string
		from, to string
		nomad    rollout.Nomad
		want     string // "" for no error
	}{
		{"the pin does not move", "2.0.7", "2.0.7", versionsOn([]string{"2.0.9"}), ""},
		{"nothing is reported", "2.0.6", "2.0.7", rollout.Nomad{}, ""},
		{"a pin goes down beside a server", "2.0.7", "2.0.6",
			versionsOn([]string{"2.0.6", "2.0.6", "2.0.6", "2.0.7"}), lowerBesideServer},
		{"a pin goes down beside a node", "2.0.7", "2.0.6", versionsOn([]string{"2.0.6"}, "2.0.7"),
			"update would lower the Nomad version of cluster prod from 2.0.7 to 2.0.6: tent never moves a node to an " +
				"older Nomad: the cluster's version is 2.0.6, and node prod-workers-0 runs 2.0.7; " +
				"set spec.nomad.version to 2.0.7 again and finish the roll with tent rolling-update cluster"},
		{"a pin goes down beside a node that is down", "2.0.7", "2.0.6",
			withNodeDown(versionsOn([]string{"2.0.6"}, "2.0.7"), "prod-workers-0"), ""},
		{"a pin goes down where no node is newer", "2.0.7", "2.0.6", versionsOn([]string{"2.0.6"}, "2.0.6"), ""},
		{"a pin goes up but stays below a node", "2.0.5", "2.0.6", versionsOn([]string{"2.0.7"}),
			"update would change the Nomad version of cluster prod from 2.0.5 to 2.0.6: tent never moves a node to an " +
				"older Nomad: the cluster's version is 2.0.6, and server prod-servers-0.global runs 2.0.7; " +
				"set spec.nomad.version to 2.0.7 or newer"},
		{"a pin without a stored one stays below a server", "", "2.0.6", versionsOn([]string{"2.0.7"}),
			"update would set the Nomad version of cluster prod to 2.0.6: tent never moves a node to an older Nomad: " +
				"the cluster's version is 2.0.6, and server prod-servers-0.global runs 2.0.7; " +
				"set spec.nomad.version to 2.0.7 or newer"},
		{"a report that is no version number", "2.0.6", "2.0.7", versionsOn([]string{"soon"}),
			`update would change the Nomad version of cluster prod from 2.0.6 to 2.0.7: the Nomad version of server ` +
				`prod-servers-0.global is "soon", which is not a version number`},
		{"one minor version above the oldest server", "2.0.7", "2.1.3", versionsOn([]string{"2.0.7", "2.0.7"}), ""},
		{"the next patch", "2.0.6", "2.0.7", versionsOn([]string{"2.0.6"}, "2.0.6"), ""},
		{"two minor versions above the oldest server", "2.1.0", "2.2.0", versionsOn([]string{"2.1.0", "2.0.7"}),
			"update would change the Nomad version of cluster prod from 2.1.0 to 2.2.0: 2.2.0 is more than one minor " +
				"version above 2.0.7, which server prod-servers-1.global still runs; set spec.nomad.version to " +
				"2.1.0 again, roll every node with tent rolling-update cluster, and move then"},
		{"the first of two oldest servers is named", "2.1.0", "2.2.0", versionsOn([]string{"2.1.0", "2.0.7", "2.0.7"}),
			"update would change the Nomad version of cluster prod from 2.1.0 to 2.2.0: 2.2.0 is more than one minor " +
				"version above 2.0.7, which server prod-servers-1.global still runs; set spec.nomad.version to " +
				"2.1.0 again, roll every node with tent rolling-update cluster, and move then"},
		{"two minor versions above the oldest node", "2.1.0", "2.2.0", versionsOn([]string{"2.1.0"}, "2.1.0", "2.0.7"),
			"update would change the Nomad version of cluster prod from 2.1.0 to 2.2.0: 2.2.0 is more than one minor " +
				"version above 2.0.7, which node prod-workers-1 still runs; set spec.nomad.version to " +
				"2.1.0 again, roll every node with tent rolling-update cluster, and move then"},
		{"an oldest node that is down does not count", "2.1.0", "2.2.0",
			withNodeDown(versionsOn([]string{"2.1.0"}, "2.1.0", "2.0.7"), "prod-workers-1"), ""},
		{"an oldest server without a version does not count", "2.1.0", "2.2.0", versionsOn([]string{"2.1.0", ""}), ""},
		{"a build of a release counts as the release", "2.1.0", "2.2.0", versionsOn([]string{"2.1.0", "2.0.7+ent"}),
			"update would change the Nomad version of cluster prod from 2.1.0 to 2.2.0: 2.2.0 is more than one minor " +
				"version above 2.0.7, which server prod-servers-1.global still runs; set spec.nomad.version to " +
				"2.1.0 again, roll every node with tent rolling-update cluster, and move then"},
		{"a pre-release counts as its release", "2.1.0", "2.2.0", versionsOn([]string{"2.1.0", "2.0.7-dev"}),
			"update would change the Nomad version of cluster prod from 2.1.0 to 2.2.0: 2.2.0 is more than one minor " +
				"version above 2.0.7, which server prod-servers-1.global still runs; set spec.nomad.version to " +
				"2.1.0 again, roll every node with tent rolling-update cluster, and move then"},
		{"another major version", "2.1.0", "3.0.0", versionsOn([]string{"2.1.0"}),
			"update would change the Nomad version of cluster prod from 2.1.0 to 3.0.0: 3.0.0 is of another major " +
				"version than 2.1.0, which server prod-servers-0.global still runs; set spec.nomad.version to " +
				"2.1.0 again, roll every node with tent rolling-update cluster, and move then"},
		{"too far above the oldest with no stored pin", "", "2.2.0", versionsOn([]string{"2.0.7"}),
			"update would set the Nomad version of cluster prod to 2.2.0: 2.2.0 is more than one minor version above " +
				"2.0.7, which server prod-servers-0.global still runs; set spec.nomad.version to a version below " +
				"2.2.0, roll every node with tent rolling-update cluster, and move then"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkPinMove("prod", tt.from, tt.to, tt.nomad)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("checkPinMove = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("checkPinMove = %v\nwant %q", err, tt.want)
			}
		})
	}
}

// TestCheckPinMoveRefusalsAreRefusals checks that the refusal of a pin that goes down matches rollout.ErrRefused.
func TestCheckPinMoveRefusalsAreRefusals(t *testing.T) {
	t.Parallel()
	err := checkPinMove("prod", "2.0.7", "2.0.6", versionsOn([]string{"2.0.7"}))
	if !errors.Is(err, rollout.ErrRefused) {
		t.Errorf("error %v does not match rollout.ErrRefused", err)
	}
}

// TestExtremePassesOverReportsThatAreNoVersion checks that neither order picks a server without a version or a
// version that is no number.
func TestExtremePassesOverReportsThatAreNoVersion(t *testing.T) {
	t.Parallel()
	nomad := versionsOn([]string{"2.0.7", "", "soon"}, "2.0.8")
	oldest, ok := extreme(nomad, func(a, b string) int { return channels.Compare(b, a) })
	if !ok || oldest.who != "server prod-servers-0.global" {
		t.Errorf("the oldest is %+v, %t; want server prod-servers-0.global", oldest, ok)
	}
	newest, ok := extreme(nomad, channels.Compare)
	if !ok || newest.who != "node prod-workers-0" {
		t.Errorf("the newest is %+v, %t; want node prod-workers-0", newest, ok)
	}
	if got, ok := extreme(versionsOn([]string{"", "soon"}), channels.Compare); ok {
		t.Errorf("extreme = %+v, true; want false when no report is a version number", got)
	}
}
