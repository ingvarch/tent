package app

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
)

// TestIntroLifetime is the intro token's TTL and the minute that a server still accepts it after its expiry.
func TestIntroLifetime(t *testing.T) {
	if introLifetime != 31*time.Minute {
		t.Errorf("introLifetime = %s, want 31m0s", introLifetime)
	}
}

// TestStaleClients picks the listed machines of the cluster that have not joined and are older than the lifetime of
// an intro token, by the machine's role label and its creation time, whatever the plan does with them.
func TestStaleClients(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	client := func(id string, age time.Duration) cloud.Instance {
		return cloud.Instance{
			ID: id, Name: "prod-workers-" + id, Cluster: "prod", Group: "workers", Role: v1alpha1.RoleClient,
			Ready: true, Created: now.Add(-age),
		}
	}
	old := client("0", 40*time.Minute)
	for _, tc := range []struct {
		name   string
		listed []cloud.Instance
		want   []cloud.Instance
	}{
		{name: "no machines"},
		{name: "an old client", listed: []cloud.Instance{old}, want: []cloud.Instance{old}},
		{
			name:   "a second after the lifetime of the token",
			listed: []cloud.Instance{client("1", introLifetime+time.Second)},
			want:   []cloud.Instance{client("1", introLifetime+time.Second)},
		},
		{name: "the whole lifetime of the token", listed: []cloud.Instance{client("1", introLifetime)}},
		{name: "a young client", listed: []cloud.Instance{client("1", 20*time.Minute)}},
		{
			name:   "a client without a creation time",
			listed: []cloud.Instance{changed(old, func(in *cloud.Instance) { in.Created = time.Time{} })},
		},
		{
			name:   "a client that joined",
			listed: []cloud.Instance{changed(old, func(in *cloud.Instance) { in.Joined = true })},
		},
		{
			name:   "a client that is not ready",
			listed: []cloud.Instance{changed(old, func(in *cloud.Instance) { in.Ready = false })},
			want:   []cloud.Instance{changed(old, func(in *cloud.Instance) { in.Ready = false })},
		},
		{name: "an old server", listed: []cloud.Instance{labelled(old, v1alpha1.RoleServer)}},
		{name: "an old combined node", listed: []cloud.Instance{labelled(old, v1alpha1.RoleCombined)}},
		{
			name:   "a machine of a server group that the label calls a client",
			listed: []cloud.Instance{changed(old, func(in *cloud.Instance) { in.Group = "servers" })},
			want:   []cloud.Instance{changed(old, func(in *cloud.Instance) { in.Group = "servers" })},
		},
		{
			name:   "a machine of a group that the specs do not have",
			listed: []cloud.Instance{changed(old, func(in *cloud.Instance) { in.Group = "gone" })},
			want:   []cloud.Instance{changed(old, func(in *cloud.Instance) { in.Group = "gone" })},
		},
		{
			name:   "a machine of another cluster",
			listed: []cloud.Instance{changed(old, func(in *cloud.Instance) { in.Cluster = "other" })},
		},
		{
			name:   "the old ones among the young, by name",
			listed: []cloud.Instance{client("3", 2*time.Hour), client("2", time.Minute), client("1", time.Hour)},
			want:   []cloud.Instance{client("1", time.Hour), client("3", 2*time.Hour)},
		},
		{
			name: "two old machines of one name, by ID",
			listed: []cloud.Instance{
				changed(client("7", time.Hour), func(in *cloud.Instance) { in.Name = "prod-workers-1" }),
				changed(client("5", time.Hour), func(in *cloud.Instance) { in.Name = "prod-workers-1" }),
			},
			want: []cloud.Instance{
				changed(client("5", time.Hour), func(in *cloud.Instance) { in.Name = "prod-workers-1" }),
				changed(client("7", time.Hour), func(in *cloud.Instance) { in.Name = "prod-workers-1" }),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := staleClients("prod", tc.listed, now)
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateComparable(netip.Addr{}, netip.AddrPort{})); diff != "" {
				t.Errorf("staleClients (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRegistered answers whether Nomad lists a client of the machine's name at its private address that is not down.
func TestRegistered(t *testing.T) {
	addr := netip.MustParseAddr("10.64.0.9")
	machine := cloud.Instance{ID: "instance-5", Name: "prod-workers-1", PrivateIP: addr}
	node := func(change func(*nomadops.Node)) nomadops.Node {
		n := nomadops.Node{Name: "prod-workers-1", Status: "ready", Eligible: true, Address: addr}
		if change != nil {
			change(&n)
		}
		return n
	}
	for _, tc := range []struct {
		name    string
		nodes   []nomadops.Node
		machine cloud.Instance
		want    bool
	}{
		{name: "no nodes", machine: machine},
		{name: "a ready node", nodes: []nomadops.Node{node(nil)}, machine: machine, want: true},
		{
			name:    "a node that is not eligible",
			nodes:   []nomadops.Node{node(func(n *nomadops.Node) { n.Eligible = false })},
			machine: machine, want: true,
		},
		{
			name:    "a node that is initializing",
			nodes:   []nomadops.Node{node(func(n *nomadops.Node) { n.Status = "initializing" })},
			machine: machine, want: true,
		},
		{
			name:    "a node that is disconnected",
			nodes:   []nomadops.Node{node(func(n *nomadops.Node) { n.Status = "disconnected" })},
			machine: machine, want: true,
		},
		{
			name:    "a node that is down",
			nodes:   []nomadops.Node{node(func(n *nomadops.Node) { n.Status = "down" })},
			machine: machine,
		},
		{
			name: "a node that is down beside a ready one of the same name and address",
			nodes: []nomadops.Node{
				node(func(n *nomadops.Node) { n.Status = "down" }), node(nil),
			},
			machine: machine, want: true,
		},
		{
			name:    "a node of the name at another address",
			nodes:   []nomadops.Node{node(func(n *nomadops.Node) { n.Address = netip.MustParseAddr("10.64.0.10") })},
			machine: machine,
		},
		{
			name:    "a node at the address under another name",
			nodes:   []nomadops.Node{node(func(n *nomadops.Node) { n.Name = "prod-workers-2" })},
			machine: machine,
		},
		{
			name:    "a machine without a private address",
			nodes:   []nomadops.Node{node(func(n *nomadops.Node) { n.Address = netip.Addr{} })},
			machine: changed(machine, func(in *cloud.Instance) { in.PrivateIP = netip.Addr{} }),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := registered(tc.nodes, tc.machine); got != tc.want {
				t.Errorf("registered = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAskNomadError names the machines that the plan could not ask Nomad about, in the singular and the plural.
func TestAskNomadError(t *testing.T) {
	cause := errors.New("boom")
	one := cloud.Instance{ID: "instance-5", Name: "prod-workers-1"}
	two := cloud.Instance{ID: "instance-4", Name: "prod-workers-0"}
	twin := cloud.Instance{ID: "instance-6", Name: "prod-workers-1"}
	for _, tc := range []struct {
		name  string
		stale []cloud.Instance
		want  string
	}{
		{
			"one machine", []cloud.Instance{one},
			"node prod-workers-1 (ID instance-5) did not join within 31 minutes of its creation, and tent could not " +
				"ask Nomad whether it registered: boom",
		},
		{
			"two machines", []cloud.Instance{two, one},
			"nodes prod-workers-0 (ID instance-4) and prod-workers-1 (ID instance-5) did not join within 31 minutes " +
				"of their creation, and tent could not ask Nomad whether they registered: boom",
		},
		{
			"two machines of one name", []cloud.Instance{one, twin},
			"nodes prod-workers-1 (ID instance-5) and prod-workers-1 (ID instance-6) did not join within 31 minutes " +
				"of their creation, and tent could not ask Nomad whether they registered: boom",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := askNomadError(tc.stale, cause)
			if err == nil || err.Error() != tc.want || !errors.Is(err, cause) {
				t.Errorf("askNomadError = %v, want %q wrapping its cause", err, tc.want)
			}
		})
	}
}
