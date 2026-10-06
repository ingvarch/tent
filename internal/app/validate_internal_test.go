package app

import (
	"net/netip"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/model"
)

// validateModel is the cluster prod with three servers and two workers.
func validateModel() *model.Cluster {
	return &model.Cluster{
		Name: "prod", Provider: v1alpha1.ProviderVultr, Region: "ams", Zones: []string{"ams"},
		Groups: []model.NodeGroup{
			{Name: "servers", Role: v1alpha1.RoleServer, Zones: []string{"ams"}, Size: 3},
			{Name: "workers", Role: v1alpha1.RoleClient, Zones: []string{"ams"}, Size: 2},
		},
	}
}

// machine returns a ready machine of the cluster prod that has joined, with the ID instance-<n>, the name and group
// given, created n minutes after a fixed time.
func machine(n int, name, group string, role v1alpha1.Role) cloud.Instance {
	return cloud.Instance{
		ID: "instance-" + strconv.Itoa(n), Name: name, Cluster: "prod", Group: group, Role: role, Zone: "ams",
		PrivateIP: netip.MustParseAddr("10.64.0." + strconv.Itoa(n+2)), Ready: true, Joined: true,
		Created: time.Date(2026, 10, 6, 10, n, 0, 0, time.UTC),
	}
}

// fullCluster returns the machines of validateModel, instance-1 to instance-5.
func fullCluster() []cloud.Instance {
	return []cloud.Instance{
		machine(1, "prod-servers-0", "servers", v1alpha1.RoleServer),
		machine(2, "prod-servers-1", "servers", v1alpha1.RoleServer),
		machine(3, "prod-servers-2", "servers", v1alpha1.RoleServer),
		machine(4, "prod-workers-0", "workers", v1alpha1.RoleClient),
		machine(5, "prod-workers-1", "workers", v1alpha1.RoleClient),
	}
}

// foreign returns a machine of the cluster staging with the group label workers, stopped and not joined.
func foreign() cloud.Instance {
	in := machine(8, "prod-workers-9", "workers", v1alpha1.RoleClient)
	in.Cluster, in.Ready, in.Joined = "staging", false, false
	return in
}

// with returns the machines of fullCluster, changed by change, which may drop a machine by returning false.
func with(change func(cloud.Instance) (cloud.Instance, bool), extra ...cloud.Instance) []cloud.Instance {
	var out []cloud.Instance
	for _, in := range fullCluster() {
		if in, keep := change(in); keep {
			out = append(out, in)
		}
	}
	return append(out, extra...)
}

func TestMachineFailures(t *testing.T) {
	unchanged := func(in cloud.Instance) (cloud.Instance, bool) { return in, true }
	noGroup := machine(8, "prod-stray-0", "", v1alpha1.RoleClient)
	stoppedSurplus := machine(6, "prod-workers-2", "workers", v1alpha1.RoleClient)
	stoppedSurplus.Ready, stoppedSurplus.Joined = false, false
	for _, tc := range []struct {
		name      string
		instances []cloud.Instance
		want      []Failure
	}{
		{"a full cluster", fullCluster(), nil},
		{
			"a twin of a surplus machine",
			with(unchanged,
				machine(6, "prod-workers-2", "workers", v1alpha1.RoleClient),
				machine(9, "prod-workers-2", "workers", v1alpha1.RoleClient)),
			[]Failure{
				{
					Check: "machine-surplus", Node: "prod-workers-2", ID: "instance-6",
					Detail: "machine ID instance-6 is one more than the size of node group workers, 2",
				},
				{
					Check: "machine-duplicate", Node: "prod-workers-2", ID: "instance-9",
					Detail: "machine ID instance-9 has the name of machine ID instance-6",
				},
			},
		},
		{"a machine of another cluster is not counted", with(unchanged, foreign()), nil},
		{
			"a missing node",
			with(func(in cloud.Instance) (cloud.Instance, bool) { return in, in.ID != "instance-5" }),
			[]Failure{{
				Check: "machine-missing", Node: "prod-workers-1",
				Detail: "no machine: node group workers has 1 of its 2",
			}},
		},
		{
			"a missing node beside a twin",
			with(func(in cloud.Instance) (cloud.Instance, bool) { return in, in.ID != "instance-5" },
				machine(9, "prod-workers-0", "workers", v1alpha1.RoleClient)),
			[]Failure{
				{Check: "machine-missing", Node: "prod-workers-1", Detail: "no machine: node group workers has 1 of its 2"},
				{
					Check: "machine-duplicate", Node: "prod-workers-0", ID: "instance-9",
					Detail: "machine ID instance-9 has the name of machine ID instance-4",
				},
			},
		},
		{
			"two missing nodes",
			with(func(in cloud.Instance) (cloud.Instance, bool) { return in, in.Group != "workers" }),
			[]Failure{
				{Check: "machine-missing", Node: "prod-workers-0", Detail: "no machine: node group workers has 0 of its 2"},
				{Check: "machine-missing", Node: "prod-workers-1", Detail: "no machine: node group workers has 0 of its 2"},
			},
		},
		{
			"a stopped machine",
			with(func(in cloud.Instance) (cloud.Instance, bool) {
				in.Ready = in.ID != "instance-4"
				return in, true
			}),
			[]Failure{{
				Check: "machine-not-running", Node: "prod-workers-0", ID: "instance-4",
				Detail: "the cloud reports its machine (ID instance-4) as not running",
			}},
		},
		{
			"two stopped machines come by name, not by age",
			with(func(in cloud.Instance) (cloud.Instance, bool) {
				switch in.ID {
				case "instance-4":
					in.Name, in.Ready = "prod-workers-1", false
				case "instance-5":
					in.Name, in.Ready = "prod-workers-0", false
				}
				return in, true
			}),
			[]Failure{
				{
					Check: "machine-not-running", Node: "prod-workers-0", ID: "instance-5",
					Detail: "the cloud reports its machine (ID instance-5) as not running",
				},
				{
					Check: "machine-not-running", Node: "prod-workers-1", ID: "instance-4",
					Detail: "the cloud reports its machine (ID instance-4) as not running",
				},
			},
		},
		{
			"a surplus machine",
			with(unchanged, machine(6, "prod-workers-2", "workers", v1alpha1.RoleClient)),
			[]Failure{{
				Check: "machine-surplus", Node: "prod-workers-2", ID: "instance-6",
				Detail: "machine ID instance-6 is one more than the size of node group workers, 2",
			}},
		},
		{
			"a surplus machine that is stopped and has not joined shows once",
			with(unchanged, stoppedSurplus),
			[]Failure{{
				Check: "machine-surplus", Node: "prod-workers-2", ID: "instance-6",
				Detail: "machine ID instance-6 is one more than the size of node group workers, 2",
			}},
		},
		{
			"a twin",
			with(unchanged, machine(9, "prod-workers-0", "workers", v1alpha1.RoleClient)),
			[]Failure{{
				Check: "machine-duplicate", Node: "prod-workers-0", ID: "instance-9",
				Detail: "machine ID instance-9 has the name of machine ID instance-4",
			}},
		},
		{
			"a machine of an unknown group",
			with(unchanged, machine(7, "prod-old-0", "old", v1alpha1.RoleClient)),
			[]Failure{{
				Check: "machine-unknown", Node: "prod-old-0", ID: "instance-7",
				Detail: "machine ID instance-7 is of node group old, which the specs do not have",
			}},
		},
		{
			"a machine without a group label",
			with(unchanged, noGroup),
			[]Failure{{
				Check: "machine-unknown", Node: "prod-stray-0", ID: "instance-8",
				Detail: "machine ID instance-8 has no node group label",
			}},
		},
		{
			"a machine that has not joined",
			with(func(in cloud.Instance) (cloud.Instance, bool) {
				in.Joined = in.ID != "instance-5"
				return in, true
			}),
			[]Failure{{
				Check: "not-joined", Node: "prod-workers-1", ID: "instance-5",
				Detail: "machine ID instance-5 has not joined Nomad: it carries no tent/joined label",
			}},
		},
		{
			"a machine that is stopped and has not joined shows both",
			with(func(in cloud.Instance) (cloud.Instance, bool) {
				if in.ID == "instance-1" {
					in.Ready, in.Joined = false, false
				}
				return in, true
			}),
			[]Failure{
				{
					Check: "machine-not-running", Node: "prod-servers-0", ID: "instance-1",
					Detail: "the cloud reports its machine (ID instance-1) as not running",
				},
				{
					Check: "not-joined", Node: "prod-servers-0", ID: "instance-1",
					Detail: "machine ID instance-1 has not joined Nomad: it carries no tent/joined label",
				},
			},
		},
		{
			"every kind, in the order of the checks",
			with(func(in cloud.Instance) (cloud.Instance, bool) {
				switch in.ID {
				case "instance-5":
					return in, false
				case "instance-4":
					in.Ready = false
				case "instance-3":
					in.Joined = false
				}
				return in, true
			},
				machine(6, "prod-servers-3", "servers", v1alpha1.RoleServer),
				machine(7, "prod-old-0", "old", v1alpha1.RoleClient),
				machine(9, "prod-servers-0", "servers", v1alpha1.RoleServer)),
			[]Failure{
				{Check: "machine-missing", Node: "prod-workers-1", Detail: "no machine: node group workers has 1 of its 2"},
				{
					Check: "machine-not-running", Node: "prod-workers-0", ID: "instance-4",
					Detail: "the cloud reports its machine (ID instance-4) as not running",
				},
				{
					Check: "machine-surplus", Node: "prod-servers-3", ID: "instance-6",
					Detail: "machine ID instance-6 is one more than the size of node group servers, 3",
				},
				{
					Check: "machine-duplicate", Node: "prod-servers-0", ID: "instance-9",
					Detail: "machine ID instance-9 has the name of machine ID instance-1",
				},
				{
					Check: "machine-unknown", Node: "prod-old-0", ID: "instance-7",
					Detail: "machine ID instance-7 is of node group old, which the specs do not have",
				},
				{
					Check: "not-joined", Node: "prod-servers-2", ID: "instance-3",
					Detail: "machine ID instance-3 has not joined Nomad: it carries no tent/joined label",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := planMachines(validateModel(), tc.instances).failures(validateModel())
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("the failures (-want +got):\n%s", diff)
			}
			reversed := slices.Clone(tc.instances)
			slices.Reverse(reversed)
			again := planMachines(validateModel(), reversed).failures(validateModel())
			if diff := cmp.Diff(tc.want, again); diff != "" {
				t.Errorf("the failures of the reversed list (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMachineCountsFollowTheNodeGroup(t *testing.T) {
	m := validateModel()
	stray := machine(4, "prod-workers-0", "workers", v1alpha1.RoleCombined) // the label is not the group's role
	noLabel := machine(1, "prod-servers-0", "servers", "")
	oldLabel := machine(2, "prod-servers-1", "servers", v1alpha1.RoleClient)
	instances := slices.Concat(
		[]cloud.Instance{noLabel, oldLabel, machine(3, "prod-servers-2", "servers", v1alpha1.RoleServer), stray},
		[]cloud.Instance{machine(5, "prod-workers-1", "workers", v1alpha1.RoleClient), foreign()},
	)

	servers, clients := planMachines(m, instances).counts(m)

	if servers != 3 || clients != 2 {
		t.Errorf("%d servers and %d clients, want 3 and 2", servers, clients)
	}
}

func TestMachineCountsLeaveOutTheMachinesThatAnUpdateDeletes(t *testing.T) {
	m := validateModel()
	instances := append(fullCluster(),
		machine(6, "prod-workers-2", "workers", v1alpha1.RoleClient), // one more than the group's size
		machine(9, "prod-servers-0", "servers", v1alpha1.RoleServer)) // a twin of instance-1

	servers, clients := planMachines(m, instances).counts(m)

	if servers != 3 || clients != 2 {
		t.Errorf("%d servers and %d clients, want 3 and 2", servers, clients)
	}
}
