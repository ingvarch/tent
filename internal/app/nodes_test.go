package app

import (
	"encoding/hex"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
)

// Machine types and the image of the test groups.
const (
	serverType = "vc2-2c-4gb"
	clientType = "vc2-4c-8gb"
	testImage  = "ubuntu-24.04"
)

// nodeGroup returns a group of the test cluster prod, spread over zones, or over ams when no zones are given.
func nodeGroup(name string, role v1alpha1.Role, size int, zones ...string) model.NodeGroup {
	if len(zones) == 0 {
		zones = []string{"ams"}
	}
	machineType := serverType
	if role == v1alpha1.RoleClient {
		machineType = clientType
	}
	return model.NodeGroup{Name: name, Role: role, MachineType: machineType, Image: testImage, Zones: zones, Size: size}
}

func servers(size int) model.NodeGroup { return nodeGroup("servers", v1alpha1.RoleServer, size) }

func workers(size int, zones ...string) model.NodeGroup {
	return nodeGroup("workers", v1alpha1.RoleClient, size, zones...)
}

// minutes returns the time n minutes after the creation of the oldest test instance.
func minutes(n int) time.Time { return time.Date(2026, 9, 28, 10, n, 0, 0, time.UTC) }

// member returns a ready instance of cluster prod: the node index of group g, in the group's first zone, with id and
// the operation id opFor(id), created at created.
func member(g model.NodeGroup, index int, id string, created time.Time) cloud.Instance {
	return cloud.Instance{
		ID:      id,
		Name:    "prod-" + g.Name + "-" + strconv.Itoa(index),
		Cluster: "prod",
		Group:   g.Name,
		Role:    g.Role,
		Zone:    g.Zones[0],
		Op:      opFor(id),
		Ready:   true,
		Created: created,
	}
}

// opFor returns an operation id made from a test instance's id of at most 6 bytes, such as
// 00000000-0000-4000-8000-000000692d31 for i-1.
func opFor(id string) string {
	h := hex.EncodeToString([]byte(id))
	return "00000000-0000-4000-8000-" + strings.Repeat("0", 12-len(h)) + h
}

// changed returns in after change has changed it.
func changed(in cloud.Instance, change func(*cloud.Instance)) cloud.Instance {
	change(&in)
	return in
}

func notReady(in cloud.Instance) cloud.Instance {
	return changed(in, func(in *cloud.Instance) { in.Ready = false })
}

func inZone(in cloud.Instance, zone string) cloud.Instance {
	return changed(in, func(in *cloud.Instance) { in.Zone = zone })
}

func named(in cloud.Instance, name string) cloud.Instance {
	return changed(in, func(in *cloud.Instance) { in.Name = name })
}

// wantCreate is the create of the node index of group g in zone.
func wantCreate(g model.NodeGroup, index int, zone string) NodeChange {
	return NodeChange{
		Action: NodeCreate, Name: "prod-" + g.Name + "-" + strconv.Itoa(index), Group: g.Name, Role: g.Role,
		Zone: zone, MachineType: g.MachineType, Image: g.Image,
	}
}

// wantWait is the wait for the instance in of group g.
func wantWait(g model.NodeGroup, in cloud.Instance) NodeChange {
	return NodeChange{
		Action: NodeWait, Name: in.Name, Group: in.Group, Role: in.Role, Zone: in.Zone,
		MachineType: g.MachineType, Image: g.Image, ID: in.ID, Op: in.Op,
	}
}

// wantDelete is the delete of the instance in for reason.
func wantDelete(in cloud.Instance, reason string) NodeChange {
	return NodeChange{Action: NodeDelete, Name: in.Name, ID: in.ID, Reason: reason}
}

func TestPlanNodes(t *testing.T) {
	apps := nodeGroup("apps", v1alpha1.RoleClient, 2) // a client group whose name sorts before servers
	nodes, node1 := nodeGroup("nodes", v1alpha1.RoleCombined, 3), nodeGroup("nodes", v1alpha1.RoleCombined, 1)
	app1 := nodeGroup("apps", v1alpha1.RoleClient, 1)
	old := nodeGroup("old", v1alpha1.RoleClient, 2) // a group that is no longer in the spec
	s3, w1, w2 := servers(3), workers(1), workers(2)
	unlabelled := named(changed(member(w1, 0, "i-9", minutes(0)), func(in *cloud.Instance) { in.Group = "" }),
		"prod-workers-1")
	for _, tc := range []struct {
		name      string
		groups    []model.NodeGroup
		instances []cloud.Instance
		want      []NodeChange
	}{
		{
			name:   "an empty cluster: the servers first",
			groups: []model.NodeGroup{apps, s3},
			want: []NodeChange{
				wantCreate(s3, 0, "ams"), wantCreate(s3, 1, "ams"), wantCreate(s3, 2, "ams"),
				wantCreate(apps, 0, "ams"), wantCreate(apps, 1, "ams"),
			},
		},
		{
			name:   "a combined group only",
			groups: []model.NodeGroup{nodes},
			want:   []NodeChange{wantCreate(nodes, 0, "ams"), wantCreate(nodes, 1, "ams"), wantCreate(nodes, 2, "ams")},
		},
		{
			name:   "a combined group goes before clients",
			groups: []model.NodeGroup{app1, node1},
			want:   []NodeChange{wantCreate(node1, 0, "ams"), wantCreate(app1, 0, "ams")},
		},
		{
			name:   "a converged cluster",
			groups: []model.NodeGroup{s3, w2},
			instances: []cloud.Instance{
				member(s3, 0, "i-1", minutes(1)), member(s3, 1, "i-2", minutes(1)), member(s3, 2, "i-3", minutes(1)),
				member(w2, 0, "i-4", minutes(2)), member(w2, 1, "i-5", minutes(2)),
			},
		},
		{
			name:      "scale up over a gap in the indexes",
			groups:    []model.NodeGroup{workers(4)},
			instances: []cloud.Instance{member(w2, 0, "i-1", minutes(1)), member(w2, 2, "i-2", minutes(2))},
			want:      []NodeChange{wantCreate(workers(4), 1, "ams"), wantCreate(workers(4), 3, "ams")},
		},
		{
			name:   "scale down deletes the newest",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(3)), member(w2, 1, "i-2", minutes(1)), member(w2, 2, "i-3", minutes(2)),
			},
			want: []NodeChange{wantDelete(member(w2, 0, "i-1", minutes(3)), "surplus")},
		},
		{
			name:   "scale down takes a node without a creation time for the newest",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", time.Time{}), member(w2, 1, "i-2", minutes(5)), member(w2, 2, "i-3", minutes(1)),
			},
			want: []NodeChange{wantDelete(member(w2, 0, "i-1", time.Time{}), "surplus")},
		},
		{
			name:      "a group of size 0",
			groups:    []model.NodeGroup{workers(0)},
			instances: []cloud.Instance{member(w2, 1, "i-2", minutes(1)), member(w2, 0, "i-1", minutes(2))},
			want: []NodeChange{
				wantDelete(member(w2, 0, "i-1", minutes(2)), "surplus"),
				wantDelete(member(w2, 1, "i-2", minutes(1)), "surplus"),
			},
		},
		{
			name:   "a surplus node that is not ready is deleted, not waited for",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				member(w1, 0, "i-1", minutes(1)), notReady(member(w1, 1, "i-2", minutes(2))),
			},
			want: []NodeChange{wantDelete(member(w1, 1, "i-2", minutes(2)), "surplus")},
		},
		{
			name:   "a removed group",
			groups: []model.NodeGroup{s3},
			instances: []cloud.Instance{
				member(s3, 0, "i-1", minutes(1)), member(s3, 1, "i-2", minutes(1)), member(s3, 2, "i-3", minutes(1)),
				member(old, 1, "i-8", minutes(1)), member(old, 0, "i-7", minutes(1)),
			},
			want: []NodeChange{
				wantDelete(member(old, 0, "i-7", minutes(1)), "not in the spec"),
				wantDelete(member(old, 1, "i-8", minutes(1)), "not in the spec"),
			},
		},
		{
			name:      "an instance without a group label neither counts nor frees its name",
			groups:    []model.NodeGroup{w2},
			instances: []cloud.Instance{member(w2, 0, "i-1", minutes(1)), unlabelled},
			want:      []NodeChange{wantCreate(w2, 2, "ams"), wantDelete(unlabelled, "not in the spec")},
		},
		{
			name:   "duplicates by name: the oldest stays",
			groups: []model.NodeGroup{workers(3)},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(2)), member(w2, 0, "i-2", minutes(1)), // the older stays
				member(w2, 1, "i-3", time.Time{}), member(w2, 1, "i-4", minutes(5)), // no time counts as newest
				member(w2, 2, "i-6", minutes(1)), member(w2, 2, "i-5", minutes(1)), // the lowest ID stays
			},
			want: []NodeChange{
				wantDelete(member(w2, 0, "i-1", minutes(2)), "duplicate"),
				wantDelete(member(w2, 1, "i-3", time.Time{}), "duplicate"),
				wantDelete(member(w2, 2, "i-6", minutes(1)), "duplicate"),
			},
		},
		{
			name:   "nodes that are not ready yet are waited for",
			groups: []model.NodeGroup{s3, w2},
			instances: []cloud.Instance{
				notReady(member(w2, 0, "i-4", minutes(2))),
				member(s3, 0, "i-1", minutes(1)), notReady(member(s3, 1, "i-2", minutes(1))),
			},
			want: []NodeChange{
				wantWait(s3, notReady(member(s3, 1, "i-2", minutes(1)))), wantCreate(s3, 2, "ams"),
				wantWait(w2, notReady(member(w2, 0, "i-4", minutes(2)))), wantCreate(w2, 1, "ams"),
			},
		},
		{
			name:   "a pending server and a missing server: the wait first",
			groups: []model.NodeGroup{s3},
			instances: []cloud.Instance{
				member(s3, 0, "i-1", minutes(1)), notReady(member(s3, 1, "i-2", minutes(1))),
			},
			want: []NodeChange{
				wantWait(s3, notReady(member(s3, 1, "i-2", minutes(1)))), wantCreate(s3, 2, "ams"),
			},
		},
		{
			name:   "a pending client and a missing client: the wait first",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				notReady(member(w2, 0, "i-1", minutes(1))),
			},
			want: []NodeChange{
				wantWait(w2, notReady(member(w2, 0, "i-1", minutes(1)))), wantCreate(w2, 1, "ams"),
			},
		},
		{
			name:   "a pending client does not hold back the servers' creates",
			groups: []model.NodeGroup{apps, s3},
			instances: []cloud.Instance{
				notReady(member(apps, 0, "i-4", minutes(1))), member(s3, 0, "i-1", minutes(1)),
			},
			want: []NodeChange{
				wantCreate(s3, 1, "ams"), wantCreate(s3, 2, "ams"),
				wantWait(apps, notReady(member(apps, 0, "i-4", minutes(1)))), wantCreate(apps, 1, "ams"),
			},
		},
		{
			name:   "a pending server is waited for before the servers' creates, whatever the group names",
			groups: []model.NodeGroup{apps, s3},
			instances: []cloud.Instance{
				notReady(member(apps, 0, "i-4", minutes(1))), notReady(member(s3, 0, "i-1", minutes(1))),
			},
			want: []NodeChange{
				wantWait(s3, notReady(member(s3, 0, "i-1", minutes(1)))),
				wantCreate(s3, 1, "ams"), wantCreate(s3, 2, "ams"),
				wantWait(apps, notReady(member(apps, 0, "i-4", minutes(1)))), wantCreate(apps, 1, "ams"),
			},
		},
		{
			name:   "waits of one role go by node name, not by age",
			groups: []model.NodeGroup{s3},
			instances: []cloud.Instance{
				notReady(member(s3, 2, "i-1", minutes(1))), notReady(member(s3, 0, "i-2", minutes(2))),
			},
			want: []NodeChange{
				wantWait(s3, notReady(member(s3, 0, "i-2", minutes(2)))),
				wantWait(s3, notReady(member(s3, 2, "i-1", minutes(1)))),
				wantCreate(s3, 1, "ams"),
			},
		},
		{
			name:   "combined nodes are ordered with the servers",
			groups: []model.NodeGroup{app1, node1},
			instances: []cloud.Instance{
				notReady(member(app1, 0, "i-4", minutes(1))),
			},
			want: []NodeChange{
				wantCreate(node1, 0, "ams"),
				wantWait(app1, notReady(member(app1, 0, "i-4", minutes(1)))),
			},
		},
		{
			name:   "a node that is not ready and has no operation id counts, and is not waited for",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(1)),
				changed(notReady(member(w2, 1, "i-2", minutes(2))), func(in *cloud.Instance) { in.Op = "" }),
			},
		},
		{
			name:   "a node that is not ready and has a malformed operation id counts, and is not waited for",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(1)),
				changed(notReady(member(w2, 1, "i-2", minutes(2))), func(in *cloud.Instance) { in.Op = "op-i-2" }),
			},
		},
		{
			name:   "the order of changes: server waits and creates, client waits and creates, deletes",
			groups: []model.NodeGroup{apps, s3},
			instances: []cloud.Instance{
				member(old, 0, "i-7", minutes(1)),
				member(apps, 0, "i-5", minutes(2)), member(apps, 0, "i-4", minutes(1)),
				member(s3, 0, "i-1", minutes(1)), notReady(member(s3, 1, "i-2", minutes(1))),
			},
			want: []NodeChange{
				wantWait(s3, notReady(member(s3, 1, "i-2", minutes(1)))), wantCreate(s3, 2, "ams"),
				wantCreate(apps, 1, "ams"),
				wantDelete(member(apps, 0, "i-5", minutes(2)), "duplicate"),
				wantDelete(member(old, 0, "i-7", minutes(1)), "not in the spec"),
			},
		},
		{
			name:   "several zones: the zone with the fewest nodes first, then the one listed first",
			groups: []model.NodeGroup{workers(5, "fsn1", "nbg1", "hel1")},
			instances: []cloud.Instance{
				inZone(member(w2, 0, "i-1", minutes(1)), "nbg1"),
				inZone(member(w2, 1, "i-2", minutes(1)), "nbg1"),
				inZone(member(w2, 2, "i-3", minutes(1)), "fsn1"),
			},
			want: []NodeChange{
				wantCreate(workers(5, "fsn1", "nbg1", "hel1"), 3, "hel1"),
				wantCreate(workers(5, "fsn1", "nbg1", "hel1"), 4, "fsn1"),
			},
		},
		{
			name:   "nodes in a zone that the group no longer lists count and stay",
			groups: []model.NodeGroup{workers(3, "fsn1", "nbg1")},
			instances: []cloud.Instance{
				inZone(member(w2, 0, "i-1", minutes(1)), "hel1"),
				inZone(member(w2, 1, "i-2", minutes(1)), "fsn1"),
			},
			want: []NodeChange{wantCreate(workers(3, "fsn1", "nbg1"), 2, "nbg1")},
		},
		{
			name:   "a node in a zone that the group no longer lists goes as a surplus by its age alone",
			groups: []model.NodeGroup{workers(1, "fsn1")},
			instances: []cloud.Instance{
				inZone(member(w2, 0, "i-1", minutes(1)), "hel1"),
				inZone(member(w2, 1, "i-2", minutes(2)), "fsn1"),
			},
			want: []NodeChange{wantDelete(inZone(member(w2, 1, "i-2", minutes(2)), "fsn1"), "surplus")},
		},
		{
			name:   "instances of another cluster are left alone",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				member(w1, 0, "i-1", minutes(1)),
				changed(member(w1, 1, "i-2", minutes(2)), func(in *cloud.Instance) { in.Cluster = "stage" }),
			},
		},
		{
			name:   "names of another form count but take no index",
			groups: []model.NodeGroup{workers(4)},
			instances: []cloud.Instance{
				named(member(w2, 0, "i-1", minutes(1)), "prod-workers-01"),
				named(member(w2, 1, "i-2", minutes(1)), "web"),
			},
			want: []NodeChange{wantCreate(workers(4), 0, "ams"), wantCreate(workers(4), 1, "ams")},
		},
		{
			name:      "a name that a node of another group holds is not free",
			groups:    []model.NodeGroup{servers(1), w1},
			instances: []cloud.Instance{named(member(servers(1), 0, "i-1", minutes(1)), "prod-workers-0")},
			want:      []NodeChange{wantCreate(w1, 1, "ams")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model.Cluster{Name: "prod", Groups: tc.groups}
			instances := slices.Clone(tc.instances)
			got, _ := planNodes(m, instances)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("planNodes (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.instances, instances, cmpopts.EquateComparable(netip.Addr{})); diff != "" {
				t.Errorf("planNodes changed the instances (-before +after):\n%s", diff)
			}
			slices.Reverse(instances)
			reversed, _ := planNodes(m, instances)
			if diff := cmp.Diff(got, reversed); diff != "" {
				t.Errorf("planNodes of the instances in reverse order (-in order +reversed):\n%s", diff)
			}
		})
	}
}

// TestPlanNodesServers returns the machines of the server and combined groups that stay, by name, and leaves out the
// duplicates, the surplus, the machines of client groups and those of another cluster.
func TestPlanNodesServers(t *testing.T) {
	combined := nodeGroup("all", v1alpha1.RoleCombined, 1)
	s3 := servers(3)
	s0, s1, s2 := member(s3, 0, "i-1", minutes(1)), member(s3, 1, "i-2", minutes(2)), member(s3, 2, "i-3", minutes(3))
	for _, tc := range []struct {
		name      string
		groups    []model.NodeGroup
		instances []cloud.Instance
		want      []cloud.Instance
	}{
		{"none exist", []model.NodeGroup{s3, workers(2)}, nil, nil},
		{"in the order of their names", []model.NodeGroup{s3}, []cloud.Instance{s2, s0, s1}, []cloud.Instance{s0, s1, s2}},
		{"not ready ones too", []model.NodeGroup{s3}, []cloud.Instance{s0, notReady(s1)},
			[]cloud.Instance{s0, notReady(s1)}},
		{"clients are no servers", []model.NodeGroup{servers(1), workers(2)},
			[]cloud.Instance{member(workers(2), 0, "i-4", minutes(4)), s0}, []cloud.Instance{s0}},
		{"combined nodes are servers", []model.NodeGroup{combined},
			[]cloud.Instance{member(combined, 0, "i-5", minutes(5))}, []cloud.Instance{member(combined, 0, "i-5", minutes(5))}},
		{"a surplus node goes", []model.NodeGroup{servers(1)}, []cloud.Instance{s1, s0}, []cloud.Instance{s0}},
		{"a duplicate goes", []model.NodeGroup{servers(1)}, []cloud.Instance{s0, changed(s0, func(in *cloud.Instance) {
			in.ID, in.Created = "i-9", minutes(9)
		})}, []cloud.Instance{s0}},
		{"a node of another group goes", []model.NodeGroup{servers(1)},
			[]cloud.Instance{s0, changed(s1, func(in *cloud.Instance) { in.Group = "gone" })}, []cloud.Instance{s0}},
		{"another cluster's node is not ours", []model.NodeGroup{servers(1)},
			[]cloud.Instance{s0, changed(s1, func(in *cloud.Instance) { in.Cluster = "dev" })}, []cloud.Instance{s0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := planNodes(&model.Cluster{Name: "prod", Groups: tc.groups}, slices.Clone(tc.instances))
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateComparable(netip.Addr{})); diff != "" {
				t.Errorf("the servers (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNodeActionString(t *testing.T) {
	for _, tc := range []struct {
		action NodeAction
		want   string
	}{
		{NodeCreate, "create"},
		{NodeWait, "wait"},
		{NodeDelete, "delete"},
		{0, "NodeAction(0)"},
		{NodeDelete + 1, "NodeAction(4)"},
	} {
		if got := tc.action.String(); got != tc.want {
			t.Errorf("NodeAction(%d).String() = %q, want %q", int(tc.action), got, tc.want)
		}
		text, err := tc.action.MarshalText()
		if err != nil || string(text) != tc.want {
			t.Errorf("NodeAction(%d).MarshalText() = %q, %v; want %q", int(tc.action), text, err, tc.want)
		}
	}
}
