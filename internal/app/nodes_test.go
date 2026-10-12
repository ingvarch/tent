package app

import (
	"context"
	"encoding/hex"
	"errors"
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
	"github.com/ingvarch/tent/internal/rollout"
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

// member returns a ready instance of cluster prod that has joined: the node index of group g, in the group's first
// zone, with id, the operation id opFor(id) and the private address addressFor(id), created at created.
func member(g model.NodeGroup, index int, id string, created time.Time) cloud.Instance {
	return cloud.Instance{
		ID:        id,
		Name:      "prod-" + g.Name + "-" + strconv.Itoa(index),
		Cluster:   "prod",
		Group:     g.Name,
		Role:      g.Role,
		Zone:      g.Zones[0],
		Op:        opFor(id),
		Ready:     true,
		Joined:    true,
		Created:   created,
		PrivateIP: addressFor(id),
	}
}

// addressFor returns a private address made from the number in a test instance's id, such as 10.64.0.1 for i-1.
func addressFor(id string) netip.Addr {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "i-"))
	return netip.AddrFrom4([4]byte{10, 64, byte(n >> 8), byte(n)})
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

// notReady returns in as the cloud shows it before it has booted: not ready and not joined.
func notReady(in cloud.Instance) cloud.Instance {
	return changed(in, func(in *cloud.Instance) { in.Ready, in.Joined = false, false })
}

// labelled returns in with the role label role, as another group's machine or a change by hand could have it.
func labelled(in cloud.Instance, role v1alpha1.Role) cloud.Instance {
	return changed(in, func(in *cloud.Instance) { in.Role = role })
}

// unjoined returns in without the joined label.
func unjoined(in cloud.Instance) cloud.Instance {
	return changed(in, func(in *cloud.Instance) { in.Joined = false })
}

// withoutOp returns in without an operation id.
func withoutOp(in cloud.Instance) cloud.Instance {
	return changed(in, func(in *cloud.Instance) { in.Op = "" })
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

// wantJoinWait is the wait for the instance in of group g without an operation id: it calls no cloud and only waits
// until the node joins.
func wantJoinWait(g model.NodeGroup, in cloud.Instance) NodeChange {
	w := wantWait(g, in)
	w.Op = ""
	return w
}

// wantDelete is the delete of the instance in for reason.
func wantDelete(in cloud.Instance, reason string) NodeChange {
	return NodeChange{Action: NodeDelete, Name: in.Name, ID: in.ID, Reason: reason}
}

// TestDefaultDrainIsTheDefaultOfTheAPI keeps the drain of a node that no spec of the cluster describes at the default
// drain timeout of the API.
func TestDefaultDrainIsTheDefaultOfTheAPI(t *testing.T) {
	t.Parallel()
	want, err := time.ParseDuration(v1alpha1.DefaultDrainTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if defaultDrain != want {
		t.Errorf("defaultDrain = %v, want %v", defaultDrain, want)
	}
}

// wantRemoval is the delete of the joined instance in for reason with what comes before it: the drain, when it is not
// empty, and the server's leaving.
func wantRemoval(in cloud.Instance, reason, drain string, server bool) NodeChange {
	c := wantDelete(in, reason)
	c.Drain, c.Server = drain, server
	return c
}

// wantRemovalOf is the removal of the joined instance in for reason as the zero view plans it: with the drain of
// defaultDrain when the machine runs a client, and with the leaving of its server when it runs a server.
func wantRemovalOf(in cloud.Instance, reason string) NodeChange {
	drain := ""
	if in.Role.RunsClient() {
		drain = defaultDrain.String()
	}
	return wantRemoval(in, reason, drain, in.Role.RunsServer())
}

func TestPlanNodes(t *testing.T) {
	t.Parallel()
	apps := nodeGroup("apps", v1alpha1.RoleClient, 2) // a client group whose name sorts before servers
	nodes, node1 := nodeGroup("nodes", v1alpha1.RoleCombined, 3), nodeGroup("nodes", v1alpha1.RoleCombined, 1)
	app1 := nodeGroup("apps", v1alpha1.RoleClient, 1)
	old := nodeGroup("old", v1alpha1.RoleClient, 2) // a group that is no longer in the spec
	s3, w1, w2, w3 := servers(3), workers(1), workers(2), workers(3)
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
			want: []NodeChange{wantRemovalOf(member(w2, 0, "i-1", minutes(3)), "surplus")},
		},
		{
			name:   "scale down takes a node without a creation time for the newest",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", time.Time{}), member(w2, 1, "i-2", minutes(5)), member(w2, 2, "i-3", minutes(1)),
			},
			want: []NodeChange{wantRemovalOf(member(w2, 0, "i-1", time.Time{}), "surplus")},
		},
		{
			name:      "a group of size 0",
			groups:    []model.NodeGroup{workers(0)},
			instances: []cloud.Instance{member(w2, 1, "i-2", minutes(1)), member(w2, 0, "i-1", minutes(2))},
			want: []NodeChange{
				wantRemovalOf(member(w2, 0, "i-1", minutes(2)), "surplus"),
				wantRemovalOf(member(w2, 1, "i-2", minutes(1)), "surplus"),
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
				wantRemovalOf(member(old, 0, "i-7", minutes(1)), "not in the spec"),
				wantRemovalOf(member(old, 1, "i-8", minutes(1)), "not in the spec"),
			},
		},
		{
			name:      "an instance without a group label neither counts nor frees its name",
			groups:    []model.NodeGroup{w2},
			instances: []cloud.Instance{member(w2, 0, "i-1", minutes(1)), unlabelled},
			want:      []NodeChange{wantCreate(w2, 2, "ams"), wantDelete(unlabelled, "not in the spec")},
		},
		{
			name:      "an instance without a group label does not make a node of its name a duplicate",
			groups:    []model.NodeGroup{w1},
			instances: []cloud.Instance{named(unlabelled, "prod-workers-0"), member(w1, 0, "i-1", minutes(1))},
			want:      []NodeChange{wantDelete(named(unlabelled, "prod-workers-0"), "not in the spec")},
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
			name:   "duplicates by name: a joined one stays before an older one that has not joined",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				unjoined(member(w1, 0, "i-1", minutes(1))), member(w1, 0, "i-2", minutes(2)),
			},
			want: []NodeChange{wantDelete(member(w1, 0, "i-1", minutes(1)), "duplicate")},
		},
		{
			name:   "duplicates by name: two joined ones keep the older, two that have not joined keep the older",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(1)), member(w2, 0, "i-2", minutes(2)),
				unjoined(member(w2, 1, "i-3", minutes(3))), unjoined(member(w2, 1, "i-4", minutes(4))),
			},
			want: []NodeChange{
				wantJoinWait(w2, unjoined(member(w2, 1, "i-3", minutes(3)))),
				wantDelete(member(w2, 0, "i-2", minutes(2)), "duplicate"),
				wantDelete(member(w2, 1, "i-4", minutes(4)), "duplicate"),
			},
		},
		{
			name:   "a surplus node that has not joined goes before a newer one that did",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				unjoined(member(w1, 0, "i-1", minutes(1))), member(w1, 1, "i-2", minutes(2)),
			},
			want: []NodeChange{wantDelete(unjoined(member(w1, 0, "i-1", minutes(1))), "surplus")},
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
			name:   "a ready server that has not joined is waited for without an operation id",
			groups: []model.NodeGroup{s3},
			instances: []cloud.Instance{
				member(s3, 0, "i-1", minutes(1)), unjoined(member(s3, 1, "i-2", minutes(1))), member(s3, 2, "i-3", minutes(1)),
			},
			want: []NodeChange{wantJoinWait(s3, unjoined(member(s3, 1, "i-2", minutes(1))))},
		},
		{
			name:   "a server that is not ready keeps its operation id",
			groups: []model.NodeGroup{s3},
			instances: []cloud.Instance{
				member(s3, 0, "i-1", minutes(1)), member(s3, 1, "i-2", minutes(1)),
				notReady(member(s3, 2, "i-3", minutes(1))),
			},
			want: []NodeChange{wantWait(s3, notReady(member(s3, 2, "i-3", minutes(1))))},
		},
		{
			name:   "a server that is not ready and has no valid operation id is waited for without one",
			groups: []model.NodeGroup{servers(2)},
			instances: []cloud.Instance{
				withoutOp(notReady(member(servers(2), 0, "i-1", minutes(1)))),
				changed(notReady(member(servers(2), 1, "i-2", minutes(1))), func(in *cloud.Instance) { in.Op = "op-i-2" }),
			},
			want: []NodeChange{
				wantJoinWait(servers(2), withoutOp(notReady(member(servers(2), 0, "i-1", minutes(1))))),
				wantJoinWait(servers(2), changed(notReady(member(servers(2), 1, "i-2", minutes(1))),
					func(in *cloud.Instance) { in.Op = "op-i-2" })),
			},
		},
		{
			name:   "a joined server that the cloud reports not ready is not waited for",
			groups: []model.NodeGroup{servers(2)},
			instances: []cloud.Instance{
				member(servers(2), 0, "i-1", minutes(1)),
				changed(member(servers(2), 1, "i-2", minutes(1)), func(in *cloud.Instance) { in.Ready = false }),
			},
		},
		{
			name:   "a ready combined node that has not joined is waited for without an operation id",
			groups: []model.NodeGroup{node1},
			instances: []cloud.Instance{
				unjoined(member(node1, 0, "i-1", minutes(1))),
			},
			want: []NodeChange{wantJoinWait(node1, unjoined(member(node1, 0, "i-1", minutes(1))))},
		},
		{
			name:   "waits of servers go by name, with and without an operation id",
			groups: []model.NodeGroup{s3},
			instances: []cloud.Instance{
				unjoined(member(s3, 2, "i-1", minutes(1))), notReady(member(s3, 0, "i-2", minutes(2))),
				unjoined(member(s3, 1, "i-3", minutes(3))),
			},
			want: []NodeChange{
				wantWait(s3, notReady(member(s3, 0, "i-2", minutes(2)))),
				wantJoinWait(s3, unjoined(member(s3, 1, "i-3", minutes(3)))),
				wantJoinWait(s3, unjoined(member(s3, 2, "i-1", minutes(1)))),
			},
		},
		{
			name:   "a ready client that has not joined is waited for without an operation id",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(1)), unjoined(member(w2, 1, "i-2", minutes(1))),
			},
			want: []NodeChange{wantJoinWait(w2, unjoined(member(w2, 1, "i-2", minutes(1))))},
		},
		{
			name:   "a client that is not ready keeps its operation id",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(1)), notReady(member(w2, 1, "i-2", minutes(1))),
			},
			want: []NodeChange{wantWait(w2, notReady(member(w2, 1, "i-2", minutes(1))))},
		},
		{
			name:   "a joined client that the cloud reports not ready is not waited for",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(1)),
				changed(member(w2, 1, "i-2", minutes(1)), func(in *cloud.Instance) { in.Ready = false }),
			},
		},
		{
			name:   "waits of clients go by name, with and without an operation id",
			groups: []model.NodeGroup{w3},
			instances: []cloud.Instance{
				unjoined(member(w3, 2, "i-1", minutes(1))), notReady(member(w3, 0, "i-2", minutes(2))),
				unjoined(member(w3, 1, "i-3", minutes(3))),
			},
			want: []NodeChange{
				wantWait(w3, notReady(member(w3, 0, "i-2", minutes(2)))),
				wantJoinWait(w3, unjoined(member(w3, 1, "i-3", minutes(3)))),
				wantJoinWait(w3, unjoined(member(w3, 2, "i-1", minutes(1)))),
			},
		},
		{
			name:   "a ready machine labelled client in a server group is waited for without an operation id",
			groups: []model.NodeGroup{s3},
			instances: []cloud.Instance{
				member(s3, 0, "i-1", minutes(1)), member(s3, 1, "i-2", minutes(1)),
				labelled(unjoined(member(s3, 2, "i-3", minutes(1))), v1alpha1.RoleClient),
			},
			want: []NodeChange{wantJoinWait(s3, labelled(unjoined(member(s3, 2, "i-3", minutes(1))), v1alpha1.RoleClient))},
		},
		{
			name:   "a ready machine labelled server in a client group is waited for without an operation id",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(1)),
				labelled(unjoined(member(w2, 1, "i-2", minutes(1))), v1alpha1.RoleServer),
			},
			want: []NodeChange{wantJoinWait(w2, labelled(unjoined(member(w2, 1, "i-2", minutes(1))), v1alpha1.RoleServer))},
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
				wantCreate(s3, 3, "ams"),
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
			name:   "a client that is not ready and has no operation id is waited for without one",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(1)),
				withoutOp(notReady(member(w2, 1, "i-2", minutes(2)))),
			},
			want: []NodeChange{wantJoinWait(w2, withoutOp(notReady(member(w2, 1, "i-2", minutes(2)))))},
		},
		{
			name:   "a client that is not ready and has a malformed operation id is waited for without one",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				member(w2, 0, "i-1", minutes(1)),
				changed(notReady(member(w2, 1, "i-2", minutes(2))), func(in *cloud.Instance) { in.Op = "op-i-2" }),
			},
			want: []NodeChange{wantJoinWait(w2, changed(notReady(member(w2, 1, "i-2", minutes(2))),
				func(in *cloud.Instance) { in.Op = "op-i-2" }))},
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
				wantRemovalOf(member(old, 0, "i-7", minutes(1)), "not in the spec"),
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
			name:   "a zone that the group no longer lists gives a surplus node no priority: the newest goes",
			groups: []model.NodeGroup{workers(1, "fsn1")},
			instances: []cloud.Instance{
				inZone(member(w2, 0, "i-1", minutes(1)), "hel1"),
				inZone(member(w2, 1, "i-2", minutes(2)), "fsn1"),
			},
			want: []NodeChange{wantRemovalOf(inZone(member(w2, 1, "i-2", minutes(2)), "fsn1"), "surplus")},
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
			got, _ := planNodes(m, instances, nil, nil, shrinkView{})
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("planNodes (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.instances, instances, cmpopts.EquateComparable(netip.Addr{})); diff != "" {
				t.Errorf("planNodes changed the instances (-before +after):\n%s", diff)
			}
			slices.Reverse(instances)
			reversed, _ := planNodes(m, instances, nil, nil, shrinkView{})
			if diff := cmp.Diff(got, reversed); diff != "" {
				t.Errorf("planNodes of the instances in reverse order (-in order +reversed):\n%s", diff)
			}
		})
	}
}

// TestPlanNodesSurplusFollowsTheOrderOfTheDecisions checks which machines of a group above its size are deleted: the
// ones that rollout.Surplus names, whether a reading of Nomad is given or the joined label is taken at its word.
func TestPlanNodesSurplusFollowsTheOrderOfTheDecisions(t *testing.T) {
	t.Parallel()
	w1, w2, s2 := workers(1), workers(2), servers(2)
	wide := workers(2, "ams", "fra")
	inFra := func(in cloud.Instance) cloud.Instance {
		return changed(in, func(in *cloud.Instance) { in.Zone = "fra" })
	}
	withHash := func(in cloud.Instance, hash string) cloud.Instance {
		return changed(in, func(in *cloud.Instance) { in.SpecHash = hash })
	}
	noAddress := func(in cloud.Instance) cloud.Instance {
		return changed(in, func(in *cloud.Instance) { in.PrivateIP = netip.Addr{} })
	}
	stopped := func(in cloud.Instance) cloud.Instance {
		return changed(in, func(in *cloud.Instance) { in.Ready = false })
	}
	c1, c2 := nodeGroup("nodes", v1alpha1.RoleCombined, 1), nodeGroup("nodes", v1alpha1.RoleCombined, 2)
	w1a, w1b := member(w1, 0, "i-1", minutes(1)), member(w1, 1, "i-2", minutes(2))
	readNode := func(in cloud.Instance, eligible bool) rollout.Node {
		return rollout.Node{
			ID: "n-" + in.ID, Name: in.Name, Address: in.PrivateIP, Status: "ready", Eligible: eligible,
		}
	}
	for _, tc := range []struct {
		name        string
		groups      []model.NodeGroup
		instances   []cloud.Instance
		view        shrinkView
		want        []NodeChange
		wantServers []string
	}{
		{
			name:   "a client that stays and has not joined is still waited for",
			groups: []model.NodeGroup{w2},
			instances: []cloud.Instance{
				unjoined(member(w2, 0, "i-1", minutes(1))), unjoined(member(w2, 1, "i-2", minutes(2))),
				member(w2, 2, "i-3", minutes(3)),
			},
			want: []NodeChange{
				wantJoinWait(w2, unjoined(member(w2, 0, "i-1", minutes(1)))),
				wantDelete(member(w2, 1, "i-2", minutes(2)), "surplus"),
			},
		},
		{
			name:   "a server that has not joined goes before newer ones that did, and the others stay",
			groups: []model.NodeGroup{s2},
			instances: []cloud.Instance{
				unjoined(member(s2, 0, "i-1", minutes(1))), member(s2, 1, "i-2", minutes(2)),
				member(s2, 2, "i-3", minutes(3)),
			},
			want:        []NodeChange{wantDelete(unjoined(member(s2, 0, "i-1", minutes(1))), "surplus")},
			wantServers: []string{"i-2", "i-3"},
		},
		{
			name:   "a joined server that does not run goes before running ones",
			groups: []model.NodeGroup{s2},
			instances: []cloud.Instance{
				stopped(member(s2, 0, "i-1", minutes(1))), member(s2, 1, "i-2", minutes(2)),
				member(s2, 2, "i-3", minutes(3)),
			},
			want:        []NodeChange{wantRemovalOf(stopped(member(s2, 0, "i-1", minutes(1))), "surplus")},
			wantServers: []string{"i-2", "i-3"},
		},
		{
			name:   "a joined client that does not run goes before running ones",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				stopped(member(w1, 0, "i-1", minutes(1))), member(w1, 1, "i-2", minutes(2)),
			},
			want: []NodeChange{wantRemovalOf(stopped(member(w1, 0, "i-1", minutes(1))), "surplus")},
		},
		{
			name:   "of servers that have not joined the newest goes, with or without a private address",
			groups: []model.NodeGroup{s2},
			instances: []cloud.Instance{
				member(s2, 0, "i-1", minutes(1)), noAddress(unjoined(member(s2, 2, "i-2", minutes(2)))),
				unjoined(member(s2, 1, "i-3", minutes(3))),
			},
			want: []NodeChange{
				wantJoinWait(s2, noAddress(unjoined(member(s2, 2, "i-2", minutes(2))))),
				wantDelete(unjoined(member(s2, 1, "i-3", minutes(3))), "surplus"),
			},
			wantServers: []string{"i-1", "i-2"},
		},
		{
			name:   "a combined node that has not joined goes before a newer one that did",
			groups: []model.NodeGroup{c1},
			instances: []cloud.Instance{
				unjoined(member(c1, 0, "i-1", minutes(1))), member(c1, 1, "i-2", minutes(2)),
			},
			want:        []NodeChange{wantDelete(unjoined(member(c1, 0, "i-1", minutes(1))), "surplus")},
			wantServers: []string{"i-2"},
		},
		{
			name:   "a joined combined node that does not run goes before running ones",
			groups: []model.NodeGroup{c2},
			instances: []cloud.Instance{
				stopped(member(c2, 0, "i-1", minutes(1))), member(c2, 1, "i-2", minutes(2)),
				member(c2, 2, "i-3", minutes(3)),
			},
			want:        []NodeChange{wantRemovalOf(stopped(member(c2, 0, "i-1", minutes(1))), "surplus")},
			wantServers: []string{"i-2", "i-3"},
		},
		{
			name:   "a server without a private address goes before ones that have one",
			groups: []model.NodeGroup{servers(1)},
			instances: []cloud.Instance{
				noAddress(member(servers(1), 0, "i-1", minutes(1))), member(servers(1), 1, "i-2", minutes(2)),
			},
			want: []NodeChange{wantRemovalOf(noAddress(member(servers(1), 0, "i-1", minutes(1))), "surplus")},
		},
		{
			name:   "the zone with the most machines loses one before a newer machine elsewhere",
			groups: []model.NodeGroup{wide},
			instances: []cloud.Instance{
				member(wide, 0, "i-1", minutes(1)), member(wide, 1, "i-2", minutes(2)),
				inFra(member(wide, 2, "i-3", minutes(3))),
			},
			want: []NodeChange{wantRemovalOf(member(wide, 1, "i-2", minutes(2)), "surplus")},
		},
		{
			name:   "a joined client without a private address goes before a machine that stays",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				noAddress(member(w1, 0, "i-1", minutes(1))), member(w1, 1, "i-2", minutes(2)),
			},
			want: []NodeChange{wantRemovalOf(noAddress(member(w1, 0, "i-1", minutes(1))), "surplus")},
		},
		{
			name:      "without a reading no machine is outdated, whatever its hash",
			groups:    []model.NodeGroup{w1},
			instances: []cloud.Instance{withHash(w1a, "x"), withHash(w1b, "")},
			want:      []NodeChange{wantRemovalOf(withHash(w1b, ""), "surplus")},
		},
		{
			name:      "an outdated machine goes first by the hash of its group",
			groups:    []model.NodeGroup{w1},
			instances: []cloud.Instance{withHash(w1a, "old"), withHash(w1b, "new")},
			view:      shrinkView{hash: func(string) string { return "new" }},
			want:      []NodeChange{wantRemovalOf(withHash(w1a, "old"), "surplus")},
		},
		{
			name:      "a machine to replace goes first",
			groups:    []model.NodeGroup{w1},
			instances: []cloud.Instance{withHash(w1a, "h"), withHash(w1b, "h")},
			view:      shrinkView{hash: func(string) string { return "h" }, forced: map[string]bool{"i-1": true}},
			want:      []NodeChange{wantRemovalOf(withHash(w1a, "h"), "surplus")},
		},
		{
			name:      "the reading of Nomad is used: the machine of an ineligible node goes first",
			groups:    []model.NodeGroup{w1},
			instances: []cloud.Instance{w1a, w1b},
			view: shrinkView{read: true, nomad: rollout.Nomad{
				Nodes: []rollout.Node{readNode(w1a, false), readNode(w1b, true)},
			}},
			want: []NodeChange{wantRemovalOf(w1a, "surplus")},
		},
		{
			name:      "the reading of Nomad is used: without nodes a stopped client goes no sooner than a running one",
			groups:    []model.NodeGroup{w1},
			instances: []cloud.Instance{stopped(w1a), w1b},
			view:      shrinkView{read: true},
			want:      []NodeChange{wantRemovalOf(w1b, "surplus")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &model.Cluster{Name: "prod", Groups: tc.groups}
			got, servers := planNodes(m, slices.Clone(tc.instances), nil, nil, tc.view)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("the changes (-want +got):\n%s", diff)
			}
			if tc.wantServers == nil {
				return
			}
			var ids []string
			for _, in := range servers {
				ids = append(ids, in.ID)
			}
			if diff := cmp.Diff(tc.wantServers, ids); diff != "" {
				t.Errorf("the servers that stay (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPlanNodesRemovalsOfJoinedNodes gives a delete of a machine that joined what comes before it: the drain, for a
// client and a combined node, and the leaving of the server, for a server and a combined node. A machine of a group
// that the specs lack gets it only when it is a client, no other machine of its name joined, and its group label
// is set. A machine that never joined, a duplicate and any other machine that joined get none.
func TestPlanNodesRemovalsOfJoinedNodes(t *testing.T) {
	t.Parallel()
	w1, s1, c1 := workers(1), servers(1), nodeGroup("nodes", v1alpha1.RoleCombined, 1)
	old := nodeGroup("old", v1alpha1.RoleClient, 2) // a group that is no longer in the spec
	oldServers := nodeGroup("db", v1alpha1.RoleServer, 2)
	unlabelled := func(in cloud.Instance) cloud.Instance {
		return changed(in, func(in *cloud.Instance) { in.Group = "" })
	}
	halfHour := func(group string) time.Duration {
		if group == "workers" {
			return 30 * time.Minute
		}
		return 2 * time.Hour
	}
	for _, tc := range []struct {
		name      string
		groups    []model.NodeGroup
		instances []cloud.Instance
		view      shrinkView
		want      []NodeChange
	}{
		{
			name:      "a joined client drains within the deadline of its group",
			groups:    []model.NodeGroup{w1},
			instances: []cloud.Instance{member(w1, 0, "i-1", minutes(1)), member(w1, 1, "i-2", minutes(2))},
			view:      shrinkView{drain: halfHour},
			want:      []NodeChange{wantRemoval(member(w1, 1, "i-2", minutes(2)), "surplus", "30m0s", false)},
		},
		{
			name:      "without a deadline in the view a joined client drains for the default of the API",
			groups:    []model.NodeGroup{w1},
			instances: []cloud.Instance{member(w1, 0, "i-1", minutes(1)), member(w1, 1, "i-2", minutes(2))},
			want:      []NodeChange{wantRemoval(member(w1, 1, "i-2", minutes(2)), "surplus", "1h0m0s", false)},
		},
		{
			name:      "a joined server leaves the cluster and does not drain",
			groups:    []model.NodeGroup{s1},
			instances: []cloud.Instance{member(s1, 0, "i-1", minutes(1)), member(s1, 1, "i-2", minutes(2))},
			view:      shrinkView{drain: halfHour},
			want:      []NodeChange{wantRemoval(member(s1, 1, "i-2", minutes(2)), "surplus", "", true)},
		},
		{
			name:      "a joined combined node drains and leaves the cluster",
			groups:    []model.NodeGroup{c1},
			instances: []cloud.Instance{member(c1, 0, "i-1", minutes(1)), member(c1, 1, "i-2", minutes(2))},
			view:      shrinkView{drain: halfHour},
			want:      []NodeChange{wantRemoval(member(c1, 1, "i-2", minutes(2)), "surplus", "2h0m0s", true)},
		},
		{
			name:   "a client that never joined is deleted as before, and so is a server",
			groups: []model.NodeGroup{w1, s1},
			instances: []cloud.Instance{
				member(w1, 0, "i-1", minutes(1)), unjoined(member(w1, 1, "i-2", minutes(2))),
				member(s1, 0, "i-3", minutes(1)), unjoined(member(s1, 1, "i-4", minutes(2))),
			},
			want: []NodeChange{
				wantDelete(unjoined(member(s1, 1, "i-4", minutes(2))), "surplus"),
				wantDelete(unjoined(member(w1, 1, "i-2", minutes(2))), "surplus"),
			},
		},
		{
			name:      "a joined client of a group that the specs lack drains for the default of the API",
			groups:    []model.NodeGroup{w1},
			instances: []cloud.Instance{member(w1, 0, "i-1", minutes(1)), member(old, 0, "i-7", minutes(1))},
			view:      shrinkView{drain: halfHour},
			want:      []NodeChange{wantRemoval(member(old, 0, "i-7", minutes(1)), "not in the spec", "1h0m0s", false)},
		},
		{
			name:   "of two machines of a removed group with one name, the one that joined drains",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				member(w1, 0, "i-1", minutes(1)),
				unjoined(member(old, 0, "i-7", minutes(1))), member(old, 0, "i-8", minutes(2)),
			},
			want: []NodeChange{
				wantDelete(unjoined(member(old, 0, "i-7", minutes(1))), "not in the spec"),
				wantRemoval(member(old, 0, "i-8", minutes(2)), "not in the spec", "1h0m0s", false),
			},
		},
		{
			name:   "two machines of a removed group with one name that both joined get no drain",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				member(w1, 0, "i-1", minutes(1)), member(old, 0, "i-7", minutes(1)), member(old, 0, "i-8", minutes(2)),
				member(old, 1, "i-9", minutes(3)),
			},
			want: []NodeChange{
				wantDelete(member(old, 0, "i-7", minutes(1)), "not in the spec"),
				wantDelete(member(old, 0, "i-8", minutes(2)), "not in the spec"),
				wantRemoval(member(old, 1, "i-9", minutes(3)), "not in the spec", "1h0m0s", false),
			},
		},
		{
			name:   "a joined machine of a removed server group gets nothing, and neither does one without a role",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				member(w1, 0, "i-1", minutes(1)), member(oldServers, 0, "i-7", minutes(1)),
				labelled(member(old, 1, "i-8", minutes(1)), ""),
			},
			want: []NodeChange{
				wantDelete(member(oldServers, 0, "i-7", minutes(1)), "not in the spec"),
				wantDelete(labelled(member(old, 1, "i-8", minutes(1)), ""), "not in the spec"),
			},
		},
		{
			name:   "a joined client without a group label gets nothing",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				member(w1, 0, "i-1", minutes(1)), unlabelled(member(old, 0, "i-7", minutes(1))),
			},
			want: []NodeChange{wantDelete(unlabelled(member(old, 0, "i-7", minutes(1))), "not in the spec")},
		},
		{
			name:   "a joined duplicate gets nothing",
			groups: []model.NodeGroup{w1},
			instances: []cloud.Instance{
				member(w1, 0, "i-1", minutes(1)), member(w1, 0, "i-2", minutes(2)),
			},
			want: []NodeChange{wantDelete(member(w1, 0, "i-2", minutes(2)), "duplicate")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &model.Cluster{Name: "prod", Groups: tc.groups}
			got, _ := planNodes(m, slices.Clone(tc.instances), nil, nil, tc.view)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("the changes (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPlanNodesTakesADrainingNodeBeforeOneThatNeverJoined reads Nomad's nodes for the surplus. A group of size 2 has a
// joined machine, a joined one whose node drains and one that never joined: the one that drains goes, with a drain,
// and the one that never joined stays and is waited for.
func TestPlanNodesTakesADrainingNodeBeforeOneThatNeverJoined(t *testing.T) {
	t.Parallel()
	w2 := workers(2)
	stays, drains := member(w2, 0, "i-1", minutes(1)), member(w2, 1, "i-2", minutes(2))
	fresh := unjoined(member(w2, 2, "i-3", minutes(3)))
	view := shrinkView{read: true, nomad: rollout.Nomad{Nodes: []rollout.Node{
		{ID: "n-1", Name: stays.Name, Address: stays.PrivateIP, Status: "ready", Eligible: true},
		{ID: "n-2", Name: drains.Name, Address: drains.PrivateIP, Status: "ready", Draining: true},
	}}}
	m := &model.Cluster{Name: "prod", Groups: []model.NodeGroup{w2}}
	got, _ := planNodes(m, []cloud.Instance{stays, drains, fresh}, nil, nil, view)
	want := []NodeChange{wantJoinWait(w2, fresh), wantRemoval(drains, "surplus", "1h0m0s", false)}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the changes (-want +got):\n%s", diff)
	}
}

// TestPlanNodesOrdersTheDeletes puts the deletes in the order of the run: the machines that never joined by name, then
// the removals of the server and combined groups by group, in the order in which each group loses its servers, then
// the removals of the client groups by group and name. The groups web and web-2 tell the order of the groups from the
// order of the names: prod-web-2-0 sorts before prod-web-9.
func TestPlanNodesOrdersTheDeletes(t *testing.T) {
	t.Parallel()
	s1, c1 := servers(1), nodeGroup("nodes", v1alpha1.RoleCombined, 1)
	a1, w1 := nodeGroup("apps", v1alpha1.RoleClient, 1), workers(1)
	web, web2 := nodeGroup("web", v1alpha1.RoleClient, 0), nodeGroup("web-2", v1alpha1.RoleClient, 0)
	old := nodeGroup("old", v1alpha1.RoleClient, 1)
	instances := []cloud.Instance{
		member(s1, 0, "i-1", minutes(1)), member(s1, 1, "i-2", minutes(2)), member(s1, 2, "i-3", minutes(3)),
		member(c1, 0, "i-4", minutes(1)), member(c1, 1, "i-5", minutes(2)),
		member(a1, 0, "i-6", minutes(1)), member(a1, 1, "i-7", minutes(2)),
		member(w1, 0, "i-8", minutes(1)), member(w1, 1, "i-9", minutes(2)), member(w1, 2, "i-10", minutes(3)),
		unjoined(member(w1, 3, "i-13", minutes(4))),
		unjoined(member(old, 0, "i-11", minutes(1))), member(old, 1, "i-12", minutes(2)),
		member(web, 9, "i-14", minutes(1)), member(web2, 0, "i-15", minutes(1)),
	}
	want := []NodeChange{
		wantDelete(unjoined(member(old, 0, "i-11", minutes(1))), "not in the spec"),
		wantDelete(unjoined(member(w1, 3, "i-13", minutes(4))), "surplus"),
		wantRemoval(member(c1, 1, "i-5", minutes(2)), "surplus", "1h0m0s", true),
		wantRemoval(member(s1, 2, "i-3", minutes(3)), "surplus", "", true),
		wantRemoval(member(s1, 1, "i-2", minutes(2)), "surplus", "", true),
		wantRemoval(member(a1, 1, "i-7", minutes(2)), "surplus", "1h0m0s", false),
		wantRemoval(member(old, 1, "i-12", minutes(2)), "not in the spec", "1h0m0s", false),
		wantRemoval(member(web, 9, "i-14", minutes(1)), "not in the spec", "1h0m0s", false),
		wantRemoval(member(web2, 0, "i-15", minutes(1)), "not in the spec", "1h0m0s", false),
		wantRemoval(member(w1, 1, "i-9", minutes(2)), "surplus", "1h0m0s", false),
		wantRemoval(member(w1, 2, "i-10", minutes(3)), "surplus", "1h0m0s", false),
	}
	m := &model.Cluster{Name: "prod", Groups: []model.NodeGroup{s1, c1, a1, w1}}
	got, _ := planNodes(m, slices.Clone(instances), nil, nil, shrinkView{})
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the changes (-want +got):\n%s", diff)
	}
	slices.Reverse(instances)
	reversed, _ := planNodes(m, instances, nil, nil, shrinkView{})
	if diff := cmp.Diff(want, reversed); diff != "" {
		t.Errorf("the changes of the machines in reverse order (-in order +reversed):\n%s", diff)
	}
}

// TestPlanNodesServerNames names a new machine of a server or combined group above every name that the group has
// had: above the listed machines and above the index that tent remembers, one higher for each further create. A client
// group still takes the lowest free name.
func TestPlanNodesServerNames(t *testing.T) {
	t.Parallel()
	s3, s5 := servers(3), servers(5)
	nodes := nodeGroup("nodes", v1alpha1.RoleCombined, 2)
	w3 := workers(3)
	for _, tc := range []struct {
		name      string
		groups    []model.NodeGroup
		instances []cloud.Instance
		next      map[string]int
		want      []NodeChange
	}{
		{
			name:      "a gap in the names is not filled",
			groups:    []model.NodeGroup{s3},
			instances: []cloud.Instance{member(s3, 0, "i-1", minutes(1)), member(s3, 2, "i-2", minutes(2))},
			want:      []NodeChange{wantCreate(s3, 3, "ams")},
		},
		{
			name:      "the remembered index holds the names of servers that are gone",
			groups:    []model.NodeGroup{s3},
			instances: []cloud.Instance{member(s3, 0, "i-1", minutes(1))},
			next:      map[string]int{"servers": 7},
			want:      []NodeChange{wantCreate(s3, 7, "ams"), wantCreate(s3, 8, "ams")},
		},
		{
			name:      "a listed machine above the remembered index wins",
			groups:    []model.NodeGroup{s3},
			instances: []cloud.Instance{member(s3, 5, "i-1", minutes(1)), member(s3, 0, "i-2", minutes(2))},
			next:      map[string]int{"servers": 2},
			want:      []NodeChange{wantCreate(s3, 6, "ams")},
		},
		{
			name:   "the index of another group does not count",
			groups: []model.NodeGroup{s3, nodes},
			next:   map[string]int{"nodes": 4},
			want: []NodeChange{
				wantCreate(nodes, 4, "ams"), wantCreate(nodes, 5, "ams"),
				wantCreate(s3, 0, "ams"), wantCreate(s3, 1, "ams"), wantCreate(s3, 2, "ams"),
			},
		},
		{
			name:   "a machine that is not to stay still holds its index",
			groups: []model.NodeGroup{servers(2)},
			instances: []cloud.Instance{
				member(s3, 0, "i-1", minutes(1)),
				changed(member(s3, 3, "i-2", minutes(2)), func(in *cloud.Instance) { in.Group = "gone" }),
			},
			want: []NodeChange{
				wantCreate(servers(2), 4, "ams"),
				wantDelete(changed(member(s3, 3, "i-2", minutes(2)), func(in *cloud.Instance) { in.Group = "gone" }),
					reasonNotInSpec),
			},
		},
		{
			name:      "a growth from one server to five",
			groups:    []model.NodeGroup{s5},
			instances: []cloud.Instance{member(s5, 0, "i-1", minutes(1))},
			next:      map[string]int{"servers": 4},
			want: []NodeChange{
				wantCreate(s5, 4, "ams"), wantCreate(s5, 5, "ams"), wantCreate(s5, 6, "ams"), wantCreate(s5, 7, "ams"),
			},
		},
		{
			name:      "a client group takes the lowest free name whatever the map says",
			groups:    []model.NodeGroup{w3},
			instances: []cloud.Instance{member(w3, 1, "i-1", minutes(1))},
			next:      map[string]int{"workers": 9},
			want:      []NodeChange{wantCreate(w3, 0, "ams"), wantCreate(w3, 2, "ams")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model.Cluster{Name: "prod", Groups: tc.groups}
			got, _ := planNodes(m, slices.Clone(tc.instances), nil, tc.next, shrinkView{})
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("planNodes (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPlanNodesUnregistered plans the delete of the machines that never registered, with the reason not registered,
// between the waits for clients and their creates: such a machine neither counts toward its group nor holds its name.
func TestPlanNodesUnregistered(t *testing.T) {
	t.Parallel()
	s2, w3 := servers(2), workers(3)
	fra := workers(2, "ams", "fra")
	w0, w1, w2 := unjoined(member(w3, 0, "i-4", minutes(4))), unjoined(member(w3, 1, "i-5", minutes(5))),
		unjoined(member(w3, 2, "i-6", minutes(6)))
	s0, s1 := member(s2, 0, "i-1", minutes(1)), unjoined(member(s2, 1, "i-2", minutes(2)))
	gone := unjoined(member(workers(1), 0, "i-9", minutes(9)))
	gone.Group = "gone"
	twin := changed(w1, func(in *cloud.Instance) { in.ID, in.Created = "i-7", minutes(7) })
	for _, tc := range []struct {
		name         string
		groups       []model.NodeGroup
		instances    []cloud.Instance
		unregistered map[string]bool
		want         []NodeChange
	}{
		{
			name:         "the machine frees its name and its place",
			groups:       []model.NodeGroup{workers(2)},
			instances:    []cloud.Instance{member(workers(2), 0, "i-4", minutes(4)), w1},
			unregistered: map[string]bool{"i-5": true},
			want:         []NodeChange{wantDelete(w1, reasonNotRegistered), wantCreate(workers(2), 1, "ams")},
		},
		{
			name:         "the delete follows the waits and precedes the creates, and the other deletes come last",
			groups:       []model.NodeGroup{s2, w3},
			instances:    []cloud.Instance{s0, s1, w0, w1, gone},
			unregistered: map[string]bool{"i-5": true},
			want: []NodeChange{
				wantJoinWait(s2, s1), wantJoinWait(w3, w0), wantDelete(w1, reasonNotRegistered),
				wantCreate(w3, 1, "ams"), wantCreate(w3, 2, "ams"), wantDelete(gone, reasonNotInSpec),
			},
		},
		{
			name:         "two machines, by name",
			groups:       []model.NodeGroup{w3},
			instances:    []cloud.Instance{w0, w1, w2},
			unregistered: map[string]bool{"i-6": true, "i-4": true},
			want: []NodeChange{
				wantJoinWait(w3, w1), wantDelete(w0, reasonNotRegistered), wantDelete(w2, reasonNotRegistered),
				wantCreate(w3, 0, "ams"), wantCreate(w3, 2, "ams"),
			},
		},
		{
			name:         "the zone count leaves it out",
			groups:       []model.NodeGroup{fra},
			instances:    []cloud.Instance{member(fra, 0, "i-4", minutes(4)), member(fra, 1, "i-5", minutes(5))},
			unregistered: map[string]bool{"i-5": true},
			want: []NodeChange{
				wantDelete(member(fra, 1, "i-5", minutes(5)), reasonNotRegistered), wantCreate(fra, 1, "fra"),
			},
		},
		{
			name:         "of two machines that never registered the older goes as not registered, the younger as a duplicate",
			groups:       []model.NodeGroup{workers(2)},
			instances:    []cloud.Instance{member(workers(2), 0, "i-4", minutes(4)), w1, twin},
			unregistered: map[string]bool{"i-5": true, "i-7": true},
			want: []NodeChange{
				wantDelete(w1, reasonNotRegistered), wantCreate(workers(2), 1, "ams"),
				wantDelete(twin, reasonDuplicate),
			},
		},
		{
			name:         "a twin that registered stays, and the older machine that never did goes as a duplicate",
			groups:       []model.NodeGroup{workers(2)},
			instances:    []cloud.Instance{member(workers(2), 0, "i-4", minutes(4)), w1, twin},
			unregistered: map[string]bool{"i-5": true},
			want:         []NodeChange{wantJoinWait(workers(2), twin), wantDelete(w1, reasonDuplicate)},
		},
		{
			name:   "a twin of a joined machine goes as a duplicate and the name stays taken",
			groups: []model.NodeGroup{w3},
			instances: []cloud.Instance{
				member(w3, 0, "i-4", minutes(4)), member(w3, 1, "i-5", minutes(5)),
				twin,
			},
			unregistered: map[string]bool{"i-7": true},
			want:         []NodeChange{wantCreate(w3, 2, "ams"), wantDelete(twin, reasonDuplicate)},
		},
		{
			name:         "an ID that no machine has changes nothing",
			groups:       []model.NodeGroup{workers(2)},
			instances:    []cloud.Instance{member(workers(2), 0, "i-4", minutes(4)), w1},
			unregistered: map[string]bool{"i-99": true},
			want:         []NodeChange{wantJoinWait(workers(2), w1)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model.Cluster{Name: "prod", Groups: tc.groups}
			instances := slices.Clone(tc.instances)
			got, _ := planNodes(m, instances, tc.unregistered, nil, shrinkView{})
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("planNodes (-want +got):\n%s", diff)
			}
			slices.Reverse(instances)
			reversed, _ := planNodes(m, instances, tc.unregistered, nil, shrinkView{})
			if diff := cmp.Diff(got, reversed); diff != "" {
				t.Errorf("planNodes of the instances in reverse order (-in order +reversed):\n%s", diff)
			}
		})
	}
}

// TestPlanNodesServers returns the machines of the server and combined groups that stay, by name, and leaves out the
// duplicates, the surplus, the machines of client groups and those of another cluster.
func TestPlanNodesServers(t *testing.T) {
	t.Parallel()
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
		{"a joined duplicate stays before an older one", []model.NodeGroup{servers(1)}, []cloud.Instance{
			unjoined(s0), changed(s0, func(in *cloud.Instance) { in.ID, in.Created = "i-9", minutes(9) }),
		}, []cloud.Instance{changed(s0, func(in *cloud.Instance) { in.ID, in.Created = "i-9", minutes(9) })}},
		{"a node of another group goes", []model.NodeGroup{servers(1)},
			[]cloud.Instance{s0, changed(s1, func(in *cloud.Instance) { in.Group = "gone" })}, []cloud.Instance{s0}},
		{"another cluster's node is not ours", []model.NodeGroup{servers(1)},
			[]cloud.Instance{s0, changed(s1, func(in *cloud.Instance) { in.Cluster = "dev" })}, []cloud.Instance{s0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model.Cluster{Name: "prod", Groups: tc.groups}
			_, got := planNodes(m, slices.Clone(tc.instances), nil, nil, shrinkView{})
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateComparable(netip.Addr{})); diff != "" {
				t.Errorf("the servers (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNodeActionString(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action NodeAction
		want   string
	}{
		{NodeCreate, "create"},
		{NodeWait, "wait"},
		{NodeDelete, "delete"},
		{NodeScrub, "scrub"},
		{NodeStop, "stop"},
		{0, "NodeAction(0)"},
		{NodeStop + 1, "NodeAction(6)"},
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

// stoppingNodes is a cloud that records the machines it is asked to stop and answers with err.
type stoppingNodes struct {
	cloud.Nodes
	stopped []cloud.Instance
	err     error
}

func (n *stoppingNodes) Stop(_ context.Context, in cloud.Instance) error {
	n.stopped = append(n.stopped, in)
	return n.err
}

// TestChangeNodeStopsTheMachineAndFailsWithTheCloudsError sends a stop to the cloud with the machine's ID, name and
// cluster, and returns the cloud's error as it is.
func TestChangeNodeStopsTheMachineAndFailsWithTheCloudsError(t *testing.T) {
	t.Parallel()
	errCloud := errors.New("the cloud refused")
	for _, tc := range []struct {
		name     string
		cloudErr error
	}{{"the cloud stops it", nil}, {"the cloud refuses", errCloud}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nodes := &stoppingNodes{err: tc.cloudErr}
			c := NodeChange{Action: NodeStop, Name: "prod-servers-0", ID: "i-1"}

			in, err := changeNode(t.Context(), nodes, "prod", c, nil)

			if !errors.Is(err, tc.cloudErr) {
				t.Errorf("changeNode error = %v, want %v", err, tc.cloudErr)
			}
			want := []cloud.Instance{{ID: "i-1", Name: "prod-servers-0", Cluster: "prod"}}
			if !slices.Equal(nodes.stopped, want) {
				t.Errorf("the cloud was asked to stop %+v, want %+v", nodes.stopped, want)
			}
			if in.ID != "" {
				t.Errorf("changeNode returned the machine %+v, want none", in)
			}
		})
	}
}
