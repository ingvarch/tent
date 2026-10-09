package vultrfake_test

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// newInfra returns a fake with the SSH key ssh-key-1, the VPC vpc-1 (10.64.0.0/16 in ams) and the firewall group
// firewall-1.
func newInfra(t *testing.T) *vultrfake.Fake {
	t.Helper()
	f := newFake()
	f.AddSSHKey(t, govultr.SSHKey{Name: "k", SSHKey: "ssh-ed25519 AAAA"})
	f.AddVPC(t, govultr.VPC{Region: "ams", V4Subnet: "10.64.0.0", V4SubnetMask: 16})
	f.AddFirewallGroup(t, govultr.FirewallGroup{})
	return f
}

// nodeReq returns the create request of a node as tent sends it: in ams, attached to vpc-1, with firewall-1 and
// ssh-key-1.
func nodeReq(label string, tags ...string) govultr.InstanceCreateReq {
	return govultr.InstanceCreateReq{
		Region: "ams", Plan: "vc2-1c-1gb", OsID: 2284, Label: label, Hostname: label, Tags: tags,
		FirewallGroupID: "firewall-1", AttachVPC: []string{"vpc-1"}, SSHKeys: []string{"ssh-key-1"},
		Backups: "disabled", UserData: "I2Nsb3VkLWNvbmZpZwo=",
	}
}

// mustCreateInstance creates an instance and fails the test on an error.
func mustCreateInstance(t *testing.T, f *vultrfake.Fake, req govultr.InstanceCreateReq) *govultr.Instance {
	t.Helper()
	in, err := f.CreateInstance(t.Context(), &req)
	if err != nil {
		t.Fatalf("CreateInstance %q: %v", req.Label, err)
	}
	return in
}

// mustGetInstance reads an instance and fails the test on an error.
func mustGetInstance(t *testing.T, f *vultrfake.Fake, id string) govultr.Instance {
	t.Helper()
	in, err := f.GetInstance(t.Context(), id)
	if err != nil {
		t.Fatalf("GetInstance %s: %v", id, err)
	}
	return *in
}

// mustListVPCs lists the VPCs of an instance and fails the test on an error.
func mustListVPCs(t *testing.T, f *vultrfake.Fake, id string) []govultr.VPCInfo {
	t.Helper()
	vpcs, err := f.ListInstanceVPCs(t.Context(), id)
	if err != nil {
		t.Fatalf("ListInstanceVPCs %s: %v", id, err)
	}
	return vpcs
}

// state is an instance's status, power_status and server_status.
type state struct{ Status, Power, Server string }

func stateOf(in govultr.Instance) state { return state{in.Status, in.PowerStatus, in.ServerStatus} }

// The states an instance passes through as it boots.
var (
	pending = state{"pending", "stopped", "none"}
	booting = state{"active", "running", "installingbooting"}
	ready   = state{"active", "running", "ok"}
)

func TestCreateInstance(t *testing.T) {
	f := newInfra(t)
	req := nodeReq("prod-servers-0", "tent/cluster=prod", "tent/op=op-1")
	got := mustCreateInstance(t, f, req)
	if got.DefaultPassword == "" {
		t.Error("the create answer holds no default_password")
	}
	want := govultr.Instance{
		ID: "instance-1", Region: "ams", Plan: "vc2-1c-1gb", OsID: 2284, Label: "prod-servers-0",
		Hostname: "prod-servers-0", Tags: []string{"tent/cluster=prod", "tent/op=op-1"}, FirewallGroupID: "firewall-1",
		Status: "pending", PowerStatus: "stopped", ServerStatus: "none", MainIP: "0.0.0.0", DateCreated: date,
		DefaultPassword: got.DefaultPassword,
	}
	if diff := cmp.Diff(&want, got); diff != "" {
		t.Errorf("CreateInstance (-want +got):\n%s", diff)
	}
	sent, ok := f.CreateRequest("instance-1")
	if diff := cmp.Diff(req, sent); !ok || diff != "" {
		t.Errorf("CreateRequest = %v (-want +got):\n%s", ok, diff)
	}
	if got := f.UserData("instance-1"); got != req.UserData {
		t.Errorf("UserData = %q, want %q", got, req.UserData)
	}
	// Only the create answer holds the password.
	want.DefaultPassword = ""
	if diff := cmp.Diff([]govultr.Instance{want}, f.Instances()); diff != "" {
		t.Errorf("Instances (-want +got):\n%s", diff)
	}
	if in := mustGetInstance(t, f, "instance-1"); in.DefaultPassword != "" {
		t.Errorf("GetInstance gives the default_password %q, want none", in.DefaultPassword)
	}
	wantCalls(t, f,
		vultrfake.Call{Name: "CreateInstance", Arg: "prod-servers-0"},
		vultrfake.Call{Name: "GetInstance", Arg: "instance-1"},
	)

	// The fake keeps copies.
	req.Tags[0], req.AttachVPC[0] = "changed", "changed"
	sent.Tags[1], sent.SSHKeys[0] = "changed", "changed"
	f.Instances()[0].Tags[0] = "changed"
	again, _ := f.CreateRequest("instance-1")
	if diff := cmp.Diff(nodeReq("prod-servers-0", "tent/cluster=prod", "tent/op=op-1"), again); diff != "" {
		t.Errorf("changing a request changed the fake's (-want +got):\n%s", diff)
	}
	if tags := f.Instances()[0].Tags; !slices.Equal(tags, []string{"tent/cluster=prod", "tent/op=op-1"}) {
		t.Errorf("changing a request or a result changed the instance's tags: %v", tags)
	}
}

func TestCreateRequestKeepsPointers(t *testing.T) {
	f := newInfra(t)
	yes := true
	req := nodeReq("n")
	req.EnableIPv6, req.AppVariables = &yes, map[string]string{"k": "v"}
	mustCreateInstance(t, f, req)
	yes = false
	req.AppVariables["k"] = "changed"
	sent, _ := f.CreateRequest("instance-1")
	if sent.EnableIPv6 == nil || !*sent.EnableIPv6 || sent.AppVariables["k"] != "v" {
		t.Errorf("CreateRequest = %+v, want enable_ipv6 true and app_variables k=v as sent", sent)
	}
	*sent.EnableIPv6 = false
	if again, _ := f.CreateRequest("instance-1"); !*again.EnableIPv6 {
		t.Error("changing the returned request changed the fake's")
	}
}

func TestCreateInstanceInvalid(t *testing.T) {
	edit := func(change func(*govultr.InstanceCreateReq)) *govultr.InstanceCreateReq {
		r := nodeReq("n")
		change(&r)
		return &r
	}
	for _, tc := range []struct {
		name string
		req  *govultr.InstanceCreateReq
		msg  string
	}{
		{"no request", nil, "Invalid region."},
		{"no region", edit(func(r *govultr.InstanceCreateReq) { r.Region = "" }), "Invalid region."},
		{"no plan", edit(func(r *govultr.InstanceCreateReq) { r.Plan = "" }), "Invalid plan."},
		{"no os_id", edit(func(r *govultr.InstanceCreateReq) { r.OsID = 0 }), "Invalid os_id."},
		{"an unknown os_id", edit(func(r *govultr.InstanceCreateReq) { r.OsID = 9999 }), "Invalid os_id."},
		{
			"an unknown VPC", edit(func(r *govultr.InstanceCreateReq) { r.AttachVPC = []string{"vpc-1", "vpc-9"} }),
			"Invalid VPC ID.",
		},
		{
			"a VPC in another region", edit(func(r *govultr.InstanceCreateReq) { r.AttachVPC = []string{"vpc-2"} }),
			"The VPC network is not in the instance's region.",
		},
		{
			"an unknown firewall group", edit(func(r *govultr.InstanceCreateReq) { r.FirewallGroupID = "firewall-9" }),
			"Invalid firewall group ID.",
		},
		{
			"an unknown SSH key", edit(func(r *govultr.InstanceCreateReq) { r.SSHKeys = []string{"ssh-key-1", "ssh-key-9"} }),
			"Invalid SSH key ID.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInfra(t)
			f.AddVPC(t, govultr.VPC{Region: "fra", V4Subnet: "10.64.0.0", V4SubnetMask: 16})
			in, err := f.CreateInstance(t.Context(), tc.req)
			wantAPIError(t, err, vultr.ErrInvalid, "vultr: POST /v2/instances: 400 Bad Request: "+tc.msg)
			if in != nil {
				t.Errorf("CreateInstance returned %+v with the error", in)
			}
			if got := f.Instances(); got != nil {
				t.Errorf("the fake holds %v, want no instance", got)
			}
		})
	}
}

func TestCreateInstanceOptional(t *testing.T) {
	f := newFake()
	// Only the region, the plan and the image are required.
	in, err := f.CreateInstance(t.Context(), &govultr.InstanceCreateReq{Region: "ams", Plan: "vc2-1c-1gb", OsID: 2760})
	if err != nil || in.ID != "instance-1" || in.FirewallGroupID != "" || in.Label != "" {
		t.Errorf("CreateInstance = %+v, %v; want instance-1 without a label and a firewall group", in, err)
	}
	if all := f.Instances(); len(all) != 1 {
		t.Errorf("the fake holds %d instances, want 1", len(all))
	}
}

func TestInstanceBoot(t *testing.T) {
	for _, tc := range []struct {
		active, ok int // the boot reads; -1 for the defaults
	}{
		{-1, -1}, {0, 0}, {0, 3}, {2, 2}, {3, 5},
	} {
		t.Run(fmt.Sprintf("%d,%d", tc.active, tc.ok), func(t *testing.T) {
			f := newInfra(t)
			active, ok := 1, 2
			if tc.active >= 0 {
				active, ok = tc.active, tc.ok
				f.SetBootReads(t, active, ok)
			}
			created := mustCreateInstance(t, f, nodeReq("n", "tent/cluster=prod"))
			if got := stateOf(*created); got != pending {
				t.Errorf("the create answer is %+v, want %+v", got, pending)
			}
			// Only GetInstance reads the instance on. ListInstances, the getter and ListInstanceVPCs show it as the next
			// GetInstance will, however often they are called.
			for read := range ok + 2 {
				if vpcs := mustListVPCs(t, f, "instance-1"); (len(vpcs) == 1) != (read >= active) {
					t.Errorf("before read %d: ListInstanceVPCs = %v, want the address listed only after %d reads", read+1,
						vpcs, active)
				}
				_ = f.Instances()
				var listed []govultr.Instance
				for range 3 {
					list, err := f.ListInstances(t.Context(), "tent/cluster=prod")
					if err != nil || len(list) != 1 {
						t.Fatalf("ListInstances = %v, %v; want the instance", list, err)
					}
					listed = append(listed, list[0])
				}
				in := mustGetInstance(t, f, "instance-1")
				want, ip := ready, "198.18.0.1"
				switch {
				case read < active:
					want, ip = pending, "0.0.0.0"
				case read < ok:
					want = booting
				}
				for _, got := range append(listed, in) {
					if stateOf(got) != want || got.MainIP != ip {
						t.Errorf("read %d: %+v with the main_ip %s, want %+v with %s", read+1, stateOf(got), got.MainIP,
							want, ip)
					}
				}
			}
		})
	}
}

func TestSetBootReadsAppliesToNewInstances(t *testing.T) {
	f := newInfra(t)
	mustCreateInstance(t, f, nodeReq("old"))
	f.SetBootReads(t, 0, 0)
	mustCreateInstance(t, f, nodeReq("new"))
	if got := stateOf(mustGetInstance(t, f, "instance-1")); got != pending {
		t.Errorf("the instance created before is %+v, want %+v", got, pending)
	}
	if got := stateOf(mustGetInstance(t, f, "instance-2")); got != ready {
		t.Errorf("the instance created after is %+v, want %+v", got, ready)
	}
}

func TestSetBootReadsMisuse(t *testing.T) {
	f := newInfra(t)
	for _, tc := range []struct {
		active, ok int
		want       string
	}{
		{-1, 2, "vultrfake: SetBootReads: active is -1, want 0 or more"},
		{3, 2, "vultrfake: SetBootReads: ok is 2, want active (3) or more"},
	} {
		tb := &fatalTB{TB: t}
		f.SetBootReads(tb, tc.active, tc.ok)
		wantFatal(t, tb, tc.want)
	}
	// The defaults stand.
	mustCreateInstance(t, f, nodeReq("n"))
	for _, want := range []state{pending, booting, ready} {
		if got := stateOf(mustGetInstance(t, f, "instance-1")); got != want {
			t.Errorf("GetInstance = %+v, want %+v", got, want)
		}
	}
}

func TestInstanceVPCs(t *testing.T) {
	f := newInfra(t)
	// Slow to boot: ListInstanceVPCs shows nothing yet, InstanceVPCs shows the addresses.
	f.SetBootReads(t, 5, 5)
	mustCreateInstance(t, f, nodeReq("first"))
	before := len(f.Calls())

	want := []govultr.VPCInfo{{ID: "vpc-1", IPAddress: "10.64.0.3", MacAddress: "5a:00:04:00:00:01"}}
	if diff := cmp.Diff(want, f.InstanceVPCs("instance-1")); diff != "" {
		t.Errorf("InstanceVPCs instance-1 (-want +got):\n%s", diff)
	}
	if got := f.InstanceVPCs("nope"); got != nil {
		t.Errorf("InstanceVPCs of an unknown id = %v, want nil", got)
	}
	if got := len(f.Calls()); got != before {
		t.Errorf("InstanceVPCs made %d calls, want none", got-before)
	}
	// The caller gets a copy.
	f.InstanceVPCs("instance-1")[0].IPAddress = "changed"
	if diff := cmp.Diff(want, f.InstanceVPCs("instance-1")); diff != "" {
		t.Errorf("InstanceVPCs after a change of the copy (-want +got):\n%s", diff)
	}
}

func TestInstanceVPCAddresses(t *testing.T) {
	f := newInfra(t)
	f.SetBootReads(t, 0, 0)
	f.AddVPC(t, govultr.VPC{Region: "ams", V4Subnet: "10.65.0.0", V4SubnetMask: 29}) // vpc-2: .3 to .6
	both := nodeReq("both")
	both.AttachVPC = []string{"vpc-2", "vpc-1"}
	second := nodeReq("second")
	second.AttachVPC = []string{"vpc-2"}
	for _, r := range []govultr.InstanceCreateReq{nodeReq("first"), both, nodeReq("third"), second} {
		mustCreateInstance(t, f, r)
	}
	want := map[string][]govultr.VPCInfo{
		"instance-1": {{ID: "vpc-1", IPAddress: "10.64.0.3", MacAddress: "5a:00:04:00:00:01"}},
		"instance-2": {
			{ID: "vpc-2", IPAddress: "10.65.0.3", MacAddress: "5a:00:04:00:00:02"},
			{ID: "vpc-1", IPAddress: "10.64.0.4", MacAddress: "5a:00:04:00:00:03"},
		},
		"instance-3": {{ID: "vpc-1", IPAddress: "10.64.0.5", MacAddress: "5a:00:04:00:00:04"}},
		"instance-4": {{ID: "vpc-2", IPAddress: "10.65.0.4", MacAddress: "5a:00:04:00:00:05"}},
	}
	for id, w := range want {
		if diff := cmp.Diff(w, mustListVPCs(t, f, id)); diff != "" {
			t.Errorf("ListInstanceVPCs %s (-want +got):\n%s", id, diff)
		}
	}

	// A new instance gets the lowest free address, and a new MAC.
	if err := f.DeleteInstance(t.Context(), "instance-1"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	mustCreateInstance(t, f, nodeReq("fifth"))
	if diff := cmp.Diff([]govultr.VPCInfo{{ID: "vpc-1", IPAddress: "10.64.0.3", MacAddress: "5a:00:04:00:00:06"}},
		mustListVPCs(t, f, "instance-5")); diff != "" {
		t.Errorf("ListInstanceVPCs instance-5 (-want +got):\n%s", diff)
	}

	// vpc-2 has room for two more; after that an instance gets no address in it.
	for range 3 {
		mustCreateInstance(t, f, second)
	}
	var ips []string
	for _, id := range []string{"instance-6", "instance-7", "instance-8"} {
		ips = append(ips, mustListVPCs(t, f, id)[0].IPAddress)
	}
	if diff := cmp.Diff([]string{"10.65.0.5", "10.65.0.6", ""}, ips); diff != "" {
		t.Errorf("the addresses in a full VPC (-want +got):\n%s", diff)
	}
}

func TestInstanceInVPCWithoutSubnet(t *testing.T) {
	f := newInfra(t)
	f.SetBootReads(t, 0, 0)
	f.AddVPC(t, govultr.VPC{Region: "ams"}) // vpc-2
	r := nodeReq("n")
	r.AttachVPC = []string{"vpc-2"}
	mustCreateInstance(t, f, r)
	want := []govultr.VPCInfo{{ID: "vpc-2", MacAddress: "5a:00:04:00:00:01"}}
	if diff := cmp.Diff(want, mustListVPCs(t, f, "instance-1")); diff != "" {
		t.Errorf("ListInstanceVPCs (-want +got):\n%s", diff)
	}
}

func TestListInstancesByTag(t *testing.T) {
	f := newInfra(t)
	for _, tags := range [][]string{
		{"tent/cluster=prod", "tent/op=a"},
		{"tent/cluster=dev"},
		{"tent/op=b", "tent/cluster=prod"},
		{"tent/cluster=production"},
		{"TENT/Cluster=Prod"},
		nil,
	} {
		mustCreateInstance(t, f, nodeReq("n", tags...))
	}
	for _, tc := range []struct {
		tag  string
		want []string
	}{
		{"tent/cluster=prod", []string{"instance-1", "instance-3", "instance-5"}},
		{"TENT/CLUSTER=PROD", []string{"instance-1", "instance-3", "instance-5"}},
		{"tent/op=b", []string{"instance-3"}},
		{"tent/cluster", nil},
		{"cluster=prod", nil},
		{"tent/cluster=pro", nil},
	} {
		list, err := f.ListInstances(t.Context(), tc.tag)
		if err != nil {
			t.Fatalf("ListInstances(%q): %v", tc.tag, err)
		}
		var ids []string
		for _, in := range list {
			ids = append(ids, in.ID)
		}
		if diff := cmp.Diff(tc.want, ids); diff != "" {
			t.Errorf("ListInstances(%q) (-want +got):\n%s", tc.tag, diff)
		}
	}

	// The list is a copy.
	list, _ := f.ListInstances(t.Context(), "tent/op=b")
	list[0].Tags[0], list[0].Label = "changed", "changed"
	if in := f.Instances()[2]; in.Tags[0] != "tent/op=b" || in.Label != "n" {
		t.Errorf("changing a returned list changed the fake: %+v", in)
	}

	// Without a tag the client sends no request, and nor does the fake.
	before := len(f.Calls())
	list, err := f.ListInstances(t.Context(), "")
	if err == nil || err.Error() != `vultr: GET /v2/instances: invalid tag ""` || list != nil {
		t.Errorf(`ListInstances("") = %v, %v; want the error of the client`, list, err)
	}
	if n := len(f.Calls()) - before; n != 0 {
		t.Errorf("ListInstances without a tag logged %d calls, want none", n)
	}
	if last := f.Calls()[before-1]; last != (vultrfake.Call{Name: "ListInstances", Arg: "tent/op=b"}) {
		t.Errorf("the last call is %+v, want ListInstances with its tag", last)
	}
}

func TestLostCreateIsListedByTag(t *testing.T) {
	f := newInfra(t)
	f.LoseResponse(t, "CreateInstance", 1)
	req := nodeReq("prod-servers-0", "tent/cluster=prod", "tent/op=op-1")
	in, err := f.CreateInstance(t.Context(), &req)
	wantAPIError(t, err, vultr.ErrUnavailable, "vultr: POST /v2/instances: vultrfake: the answer was lost")
	if in != nil {
		t.Errorf("CreateInstance returned %+v with the error", in)
	}
	list, err := f.ListInstances(t.Context(), "tent/op=op-1")
	if err != nil || len(list) != 1 || list[0].ID != "instance-1" {
		t.Errorf("ListInstances by the op tag = %v, %v; want instance-1 at once", list, err)
	}
}

func TestUpdateInstance(t *testing.T) {
	f := newInfra(t)
	f.AddFirewallGroup(t, govultr.FirewallGroup{}) // firewall-2
	mustCreateInstance(t, f, nodeReq("n", "a", "b"))
	yes := true
	for _, step := range []struct {
		name                   string
		req                    *govultr.InstanceUpdateReq
		tags                   []string
		label, userData, group string
	}{
		{"nothing", nil, []string{"a", "b"}, "n", "I2Nsb3VkLWNvbmZpZwo=", "firewall-1"},
		{
			"user data, tags null", &govultr.InstanceUpdateReq{UserData: "c3R1Ygo=", DDOSProtection: &yes},
			[]string{"a", "b"}, "n", "c3R1Ygo=", "firewall-1",
		},
		{
			"tags", &govultr.InstanceUpdateReq{Tags: []string{"c"}},
			[]string{"c"}, "n", "c3R1Ygo=", "firewall-1",
		},
		{
			"firewall group and label", &govultr.InstanceUpdateReq{FirewallGroupID: "firewall-2", Label: "m"},
			[]string{"c"}, "m", "c3R1Ygo=", "firewall-2",
		},
		{"no tags", &govultr.InstanceUpdateReq{Tags: []string{}}, []string{}, "m", "c3R1Ygo=", "firewall-2"},
		{
			"tags and user data in one request", &govultr.InstanceUpdateReq{Tags: []string{"e", "f"}, UserData: "bmV3Cg=="},
			[]string{"e", "f"}, "m", "bmV3Cg==", "firewall-2",
		},
	} {
		if err := f.UpdateInstance(t.Context(), "instance-1", step.req); err != nil {
			t.Fatalf("%s: UpdateInstance: %v", step.name, err)
		}
		in := f.Instances()[0]
		if !slices.Equal(in.Tags, step.tags) || in.Label != step.label || in.FirewallGroupID != step.group {
			t.Errorf("%s: tags %q, label %q, firewall group %q; want %q, %q, %q", step.name, in.Tags, in.Label,
				in.FirewallGroupID, step.tags, step.label, step.group)
		}
		if got := f.UserData("instance-1"); got != step.userData {
			t.Errorf("%s: user data %q, want %q", step.name, got, step.userData)
		}
		if in.Hostname != "n" {
			t.Errorf("%s: the hostname is %q, want n", step.name, in.Hostname)
		}
	}

	// The fake keeps a copy of the tags.
	req := &govultr.InstanceUpdateReq{Tags: []string{"d"}}
	if err := f.UpdateInstance(t.Context(), "instance-1", req); err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	req.Tags[0] = "changed"
	if tags := f.Instances()[0].Tags; !slices.Equal(tags, []string{"d"}) {
		t.Errorf("changing the request changed the tags: %v", tags)
	}

	wantAPIError(t, f.UpdateInstance(t.Context(), "instance-1", &govultr.InstanceUpdateReq{FirewallGroupID: "firewall-9",
		Tags: []string{"e"}}), vultr.ErrInvalid, "vultr: PATCH /v2/instances/instance-1: 400 Bad Request: "+
		"Invalid firewall group ID.")
	if in := f.Instances()[0]; in.FirewallGroupID != "firewall-2" || !slices.Equal(in.Tags, []string{"d"}) {
		t.Errorf("a refused update changed the instance: %+v", in)
	}
	wantAPIError(t, f.UpdateInstance(t.Context(), "instance-9", &govultr.InstanceUpdateReq{Label: "l"}),
		vultr.ErrNotFound, "vultr: PATCH /v2/instances/instance-9: 404 Not Found: Invalid instance ID.")
	if last := f.Calls()[len(f.Calls())-1]; last != (vultrfake.Call{Name: "UpdateInstance", Arg: "instance-9"}) {
		t.Errorf("the last call is %+v, want UpdateInstance with the id", last)
	}
}

func TestHaltInstance(t *testing.T) {
	f := newInfra(t)
	mustCreateInstance(t, f, nodeReq("n"))
	for range 2 { // halting twice is harmless
		if err := f.HaltInstance(t.Context(), "instance-1"); err != nil {
			t.Fatalf("HaltInstance: %v", err)
		}
	}
	// A halted instance boots, but stays off.
	for _, want := range []state{pending, {"active", "stopped", "installingbooting"}, {"active", "stopped", "ok"}} {
		if got := stateOf(mustGetInstance(t, f, "instance-1")); got != want {
			t.Errorf("GetInstance = %+v, want %+v", got, want)
		}
	}
	wantAPIError(t, f.HaltInstance(t.Context(), "instance-9"), vultr.ErrNotFound,
		"vultr: POST /v2/instances/instance-9/halt: 404 Not Found: Invalid instance ID.")
	wantCalls(t, f,
		vultrfake.Call{Name: "CreateInstance", Arg: "n"},
		vultrfake.Call{Name: "HaltInstance", Arg: "instance-1"},
		vultrfake.Call{Name: "HaltInstance", Arg: "instance-1"},
		vultrfake.Call{Name: "GetInstance", Arg: "instance-1"},
		vultrfake.Call{Name: "GetInstance", Arg: "instance-1"},
		vultrfake.Call{Name: "GetInstance", Arg: "instance-1"},
		vultrfake.Call{Name: "HaltInstance", Arg: "instance-9"},
	)
}

// stoppedReady is an instance that was halted after it booted.
var stoppedReady = state{"active", "stopped", "ok"}

// bootedInstance creates the instance label, which shows as ready from its first read, and returns its ID.
func bootedInstance(t *testing.T, f *vultrfake.Fake, label string) string {
	t.Helper()
	f.SetBootReads(t, 0, 0)
	return mustCreateInstance(t, f, nodeReq(label, "tent/cluster=prod")).ID
}

func halt(t *testing.T, f *vultrfake.Fake, id string) {
	t.Helper()
	if err := f.HaltInstance(t.Context(), id); err != nil {
		t.Fatalf("HaltInstance %s: %v", id, err)
	}
}

// listedState returns the state that ListInstances shows for the instance id.
func listedState(t *testing.T, f *vultrfake.Fake, id string) state {
	t.Helper()
	list, err := f.ListInstances(t.Context(), "tent/cluster=prod")
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	for _, in := range list {
		if in.ID == id {
			return stateOf(in)
		}
	}
	t.Fatalf("ListInstances lists no %s", id)
	return state{}
}

func TestSetHaltReadsKeepsAHaltedInstanceRunningForNReads(t *testing.T) {
	for _, n := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := newInfra(t)
			id := bootedInstance(t, f, "n")
			f.SetHaltReads(t, n)
			halt(t, f, id)
			// ListInstances and GetInstance both count; Instances and Halted do not.
			for read := range n + 3 {
				want := stoppedReady
				if read < n {
					want = ready
				}
				if got := stateOf(f.Instances()[0]); got != want {
					t.Errorf("before read %d: Instances shows %+v, want %+v", read+1, got, want)
				}
				if !f.Halted(id) {
					t.Errorf("before read %d: Halted = false, want true at once", read+1)
				}
				var got state
				if read%2 == 0 {
					got = listedState(t, f, id)
				} else {
					got = stateOf(mustGetInstance(t, f, id))
				}
				if got != want {
					t.Errorf("read %d = %+v, want %+v", read+1, got, want)
				}
			}
		})
	}
}

func TestSetHaltReadsApplyToLaterHaltsOnly(t *testing.T) {
	f := newInfra(t)
	early := bootedInstance(t, f, "early")
	late := bootedInstance(t, f, "late")
	halt(t, f, early)
	f.SetHaltReads(t, 2)
	halt(t, f, late)
	if got := stateOf(mustGetInstance(t, f, early)); got != stoppedReady {
		t.Errorf("the instance halted before the setting is %+v, want %+v", got, stoppedReady)
	}
	if got := stateOf(mustGetInstance(t, f, late)); got != ready {
		t.Errorf("the instance halted after the setting is %+v, want %+v", got, ready)
	}
}

func TestSecondHaltDoesNotRestartTheHaltReads(t *testing.T) {
	f := newInfra(t)
	id := bootedInstance(t, f, "n")
	f.SetHaltReads(t, 1)
	halt(t, f, id)
	if got := stateOf(mustGetInstance(t, f, id)); got != ready {
		t.Errorf("first read = %+v, want %+v", got, ready)
	}
	halt(t, f, id)
	if got := stateOf(mustGetInstance(t, f, id)); got != stoppedReady {
		t.Errorf("read after the second halt = %+v, want %+v", got, stoppedReady)
	}
}

func TestSetHaltReadsCountsOnlyTheHaltedInstance(t *testing.T) {
	f := newInfra(t)
	halted := bootedInstance(t, f, "halted")
	running := bootedInstance(t, f, "running")
	f.SetHaltReads(t, 1)
	halt(t, f, halted)
	if got := stateOf(mustGetInstance(t, f, running)); got != ready {
		t.Errorf("the instance that runs reads %+v, want %+v", got, ready)
	}
	if got := stateOf(mustGetInstance(t, f, halted)); got != ready {
		t.Errorf("the first read of the halted one = %+v, want %+v after reads of another instance", got, ready)
	}
	if f.Halted(running) {
		t.Error("Halted of the instance that runs is true")
	}
}

func TestSetHaltReadsDoNotShowAnInstanceThatIsStillPendingAsRunning(t *testing.T) {
	f := newInfra(t)
	id := mustCreateInstance(t, f, nodeReq("n", "tent/cluster=prod")).ID
	f.SetHaltReads(t, 5)
	halt(t, f, id)
	if got := stateOf(mustGetInstance(t, f, id)); got != pending {
		t.Errorf("the first read of the instance halted while pending = %+v, want %+v", got, pending)
	}
}

func TestSetHaltReadsMisuse(t *testing.T) {
	f := newInfra(t)
	tb := &fatalTB{TB: t}
	f.SetHaltReads(tb, -1)
	wantFatal(t, tb, "vultrfake: SetHaltReads: n is -1, want 0 or more")
	// The default stands: a halt shows at once.
	id := bootedInstance(t, f, "n")
	halt(t, f, id)
	if got := stateOf(mustGetInstance(t, f, id)); got != stoppedReady {
		t.Errorf("GetInstance after a halt = %+v, want %+v", got, stoppedReady)
	}
}

func TestHalted(t *testing.T) {
	f := newInfra(t)
	id := bootedInstance(t, f, "n")
	if f.Halted(id) {
		t.Error("Halted of a new instance is true")
	}
	halt(t, f, id)
	if !f.Halted(id) {
		t.Error("Halted after a halt is false")
	}
	if f.Halted("instance-9") {
		t.Error("Halted of an unknown instance is true")
	}
	if err := f.DeleteInstance(t.Context(), id); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if f.Halted(id) {
		t.Error("Halted of a deleted instance is true")
	}
}

func TestDeleteInstance(t *testing.T) {
	f := newInfra(t)
	mustCreateInstance(t, f, nodeReq("a"))
	mustCreateInstance(t, f, nodeReq("b"))
	ctx := t.Context()
	if err := f.DeleteInstance(ctx, "instance-1"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if got := f.Instances(); len(got) != 1 || got[0].ID != "instance-2" {
		t.Errorf("Instances = %v, want instance-2 only", got)
	}
	const gone = "404 Not Found: Invalid instance ID."
	wantAPIError(t, f.DeleteInstance(ctx, "instance-1"), vultr.ErrNotFound,
		"vultr: DELETE /v2/instances/instance-1: "+gone)
	in, err := f.GetInstance(ctx, "instance-1")
	wantAPIError(t, err, vultr.ErrNotFound, "vultr: GET /v2/instances/instance-1: "+gone)
	if in != nil {
		t.Errorf("GetInstance returned %+v with the error", in)
	}
	vpcs, err := f.ListInstanceVPCs(ctx, "instance-1")
	wantAPIError(t, err, vultr.ErrNotFound, "vultr: GET /v2/instances/instance-1/vpcs: "+gone)
	if vpcs != nil {
		t.Errorf("ListInstanceVPCs returned %v with the error", vpcs)
	}
	if got := f.UserData("instance-1"); got != "" {
		t.Errorf("UserData of a deleted instance = %q, want none", got)
	}
	if _, ok := f.CreateRequest("instance-1"); ok {
		t.Error("CreateRequest of a deleted instance is there")
	}
	wantCalls(t, f,
		vultrfake.Call{Name: "CreateInstance", Arg: "a"},
		vultrfake.Call{Name: "CreateInstance", Arg: "b"},
		vultrfake.Call{Name: "DeleteInstance", Arg: "instance-1"},
		vultrfake.Call{Name: "DeleteInstance", Arg: "instance-1"},
		vultrfake.Call{Name: "GetInstance", Arg: "instance-1"},
		vultrfake.Call{Name: "ListInstanceVPCs", Arg: "instance-1"},
	)
}

func TestDeleteVPCWhileAttached(t *testing.T) {
	f := newInfra(t)
	f.AddVPC(t, govultr.VPC{Region: "ams", V4Subnet: "10.65.0.0", V4SubnetMask: 24}) // vpc-2
	other := nodeReq("other")
	other.AttachVPC = []string{"vpc-2"}
	mustCreateInstance(t, f, nodeReq("a")) // attached at once, though its address is listed only once it is active
	mustCreateInstance(t, f, other)
	mustCreateInstance(t, f, nodeReq("b"))
	ctx := t.Context()
	const attached = "vultr: DELETE /v2/vpcs/vpc-1: 400 Bad Request: The following servers are attached to this VPC " +
		"network: "
	err := f.DeleteVPC(ctx, "vpc-1")
	wantAPIError(t, err, vultr.ErrInUse, attached+"10.64.0.3, 10.64.0.4")
	if err := f.DeleteInstance(ctx, "instance-1"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	wantAPIError(t, f.DeleteVPC(ctx, "vpc-1"), vultr.ErrInUse, attached+"10.64.0.4")
	if got := len(f.VPCs()); got != 2 {
		t.Errorf("the fake holds %d VPCs, want 2", got)
	}
	// Unlike Vultr, which refuses for some seconds more, the fake deletes the VPC as soon as its instances are gone.
	if err := f.DeleteInstance(ctx, "instance-3"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if err := f.DeleteVPC(ctx, "vpc-1"); err != nil {
		t.Errorf("DeleteVPC after its instances are gone: %v", err)
	}
	wantAPIError(t, f.DeleteVPC(ctx, "vpc-9"), vultr.ErrNotFound,
		"vultr: DELETE /v2/vpcs/vpc-9: 404 Not Found: Invalid VPC ID.")
}

func TestDeleteFirewallGroupInUse(t *testing.T) {
	f := newInfra(t)
	f.AddFirewallGroup(t, govultr.FirewallGroup{}) // firewall-2
	mustCreateInstance(t, f, nodeReq("a"))
	b := nodeReq("b")
	b.FirewallGroupID = "firewall-2"
	mustCreateInstance(t, f, b)
	// Vultr deletes a group that instances use, and they lose it.
	if err := f.DeleteFirewallGroup(t.Context(), "firewall-1"); err != nil {
		t.Fatalf("DeleteFirewallGroup: %v", err)
	}
	var groups []string
	for _, in := range f.Instances() {
		groups = append(groups, in.FirewallGroupID)
	}
	if diff := cmp.Diff([]string{"", "firewall-2"}, groups); diff != "" {
		t.Errorf("the instances' firewall groups (-want +got):\n%s", diff)
	}
	if got := f.FirewallGroups(); len(got) != 1 || got[0].ID != "firewall-2" {
		t.Errorf("FirewallGroups = %v, want firewall-2 only", got)
	}
}

func TestAddInstance(t *testing.T) {
	f := newInfra(t)
	const old = "2026-01-02T03:04:05+00:00"
	a := f.AddInstance(t, govultr.Instance{Label: "a", Tags: []string{"tent/cluster=prod"}}, "vpc-1")
	b := f.AddInstance(t, govultr.Instance{
		ID: uuid, Label: "b", Status: "active", PowerStatus: "stopped", ServerStatus: "installingbooting",
		MainIP: "198.51.100.7", DateCreated: old,
	})
	// A seeded instance is ready unless the test gives its states, and it stays as seeded however often it is read.
	wantA := govultr.Instance{
		ID: "instance-1", Label: "a", Tags: []string{"tent/cluster=prod"}, Status: "active", PowerStatus: "running",
		ServerStatus: "ok", DateCreated: date,
	}
	wantB := govultr.Instance{
		ID: uuid, Label: "b", Status: "active", PowerStatus: "stopped", ServerStatus: "installingbooting",
		MainIP: "198.51.100.7", DateCreated: old,
	}
	if diff := cmp.Diff([]govultr.Instance{wantA, wantB}, []govultr.Instance{a, b}); diff != "" {
		t.Errorf("AddInstance (-want +got):\n%s", diff)
	}
	wantCalls(t, f) // seeding is no call
	for range 3 {
		if diff := cmp.Diff(wantB, mustGetInstance(t, f, uuid)); diff != "" {
			t.Errorf("GetInstance (-want +got):\n%s", diff)
		}
	}
	// Its VPC address is listed at once.
	want := []govultr.VPCInfo{{ID: "vpc-1", IPAddress: "10.64.0.3", MacAddress: "5a:00:04:00:00:01"}}
	if diff := cmp.Diff(want, mustListVPCs(t, f, "instance-1")); diff != "" {
		t.Errorf("ListInstanceVPCs (-want +got):\n%s", diff)
	}
	if _, ok := f.CreateRequest("instance-1"); ok {
		t.Error("a seeded instance has a create request")
	}
	// Created instances skip the seeded ids and addresses.
	f.SetBootReads(t, 0, 0)
	if in := mustCreateInstance(t, f, nodeReq("c")); in.ID != "instance-2" {
		t.Errorf("the created instance's id is %q, want instance-2", in.ID)
	}
	if vpcs := mustListVPCs(t, f, "instance-2"); len(vpcs) != 1 || vpcs[0].IPAddress != "10.64.0.4" {
		t.Errorf("ListInstanceVPCs instance-2 = %v, want 10.64.0.4", vpcs)
	}
}

func TestAddInstanceMisuse(t *testing.T) {
	f := newInfra(t)
	f.AddInstance(t, govultr.Instance{ID: "taken"})
	for _, tc := range []struct {
		name string
		seed func(testing.TB)
		want string
	}{
		{
			"a bad id", func(tb testing.TB) { f.AddInstance(tb, govultr.Instance{ID: "a/b"}) },
			`vultrfake: AddInstance: the id "a/b" holds more than ASCII letters, digits and "-"`,
		},
		{
			"a taken id", func(tb testing.TB) { f.AddInstance(tb, govultr.Instance{ID: "taken"}) },
			`vultrfake: AddInstance: the id "taken" is taken`,
		},
		{
			"a missing VPC", func(tb testing.TB) { f.AddInstance(tb, govultr.Instance{ID: "free"}, "vpc-1", "vpc-9") },
			`vultrfake: AddInstance: no VPC "vpc-9"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := &fatalTB{TB: t}
			tc.seed(tb)
			wantFatal(t, tb, tc.want)
		})
	}
	// Nothing was stored, and the id of the refused instance is free.
	if got := f.Instances(); len(got) != 1 {
		t.Errorf("Instances = %v, want the one seeded before", got)
	}
	if in := f.AddInstance(t, govultr.Instance{ID: "free"}); in.ID != "free" {
		t.Errorf("AddInstance = %+v, want the id free", in)
	}
}

func TestSetInstanceTagsAndUserData(t *testing.T) {
	f := newInfra(t)
	f.AddInstance(t, govultr.Instance{ID: "seeded", Tags: []string{"a", "b"}})
	f.SetBootReads(t, 0, 0)
	created := mustCreateInstance(t, f, nodeReq("n", "t"))
	f.SetInstanceUserData(t, "seeded", "c2VlZA==")
	f.SetInstanceUserData(t, created.ID, "Y3JlYXRlZA==")
	f.SetInstanceTags(t, "seeded", "b", "x")
	f.SetInstanceTags(t, created.ID)
	tags := map[string][]string{}
	for _, in := range f.Instances() {
		tags[in.ID] = in.Tags
	}
	if diff := cmp.Diff(map[string][]string{"seeded": {"b", "x"}, created.ID: nil}, tags); diff != "" {
		t.Errorf("the tags (-want +got):\n%s", diff)
	}
	if got := f.UserData("seeded"); got != "c2VlZA==" {
		t.Errorf("the user data of the seeded instance is %q, want c2VlZA==", got)
	}
	if got := f.UserData(created.ID); got != "Y3JlYXRlZA==" {
		t.Errorf("the user data of the created instance is %q, want Y3JlYXRlZA==", got)
	}
	if _, ok := f.CreateRequest(created.ID); !ok {
		t.Error("the create request of the created instance is gone")
	}
	wantCalls(t, f, vultrfake.Call{Name: "CreateInstance", Arg: "n"}) // no call of its own
	for _, tc := range []struct {
		name string
		seed func(testing.TB)
		want string
	}{
		{"tags of an unknown instance", func(tb testing.TB) { f.SetInstanceTags(tb, "none", "x") },
			`vultrfake: SetInstanceTags: no instance "none"`},
		{"user data of an unknown instance", func(tb testing.TB) { f.SetInstanceUserData(tb, "none", "x") },
			`vultrfake: SetInstanceUserData: no instance "none"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := &fatalTB{TB: t}
			tc.seed(tb)
			wantFatal(t, tb, tc.want)
		})
	}
}

func TestInstanceGetters(t *testing.T) {
	f := newInfra(t)
	if in, data := f.Instances(), f.UserData("instance-1"); in != nil || data != "" {
		t.Errorf("a new fake has %v and the user data %q; want nothing", in, data)
	}
	mustCreateInstance(t, f, nodeReq("n", "t"))
	// The getters make no call: no fault touches them, and they do not read the instance on.
	f.Fail(t, "GetInstance", errBoom, 1)
	for range 3 {
		if in := f.Instances(); len(in) != 1 || stateOf(in[0]) != pending {
			t.Errorf("Instances = %v, want the pending instance", in)
		}
		_ = f.UserData("instance-1")
		_, _ = f.CreateRequest("instance-1")
		_ = f.Halted("instance-1")
	}
	f.Instances()[0].Tags[0] = "changed"
	if tags := f.Instances()[0].Tags; tags[0] != "t" {
		t.Errorf("changing the result of Instances changed the fake: %v", tags)
	}
	wantCalls(t, f, vultrfake.Call{Name: "CreateInstance", Arg: "n"})
	if _, err := f.GetInstance(t.Context(), "instance-1"); !errors.Is(err, errBoom) {
		t.Errorf("GetInstance = %v, want the fault that the getters left", err)
	}
	if got := stateOf(mustGetInstance(t, f, "instance-1")); got != pending {
		t.Errorf("the first read that the fake carried out gives %+v, want %+v", got, pending)
	}
}

func TestInstancesConcurrentUse(t *testing.T) {
	const workers, rounds = 10, 5
	f := newInfra(t)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			ctx := t.Context()
			for i := range rounds {
				tag := fmt.Sprintf("tent/op=%d-%d", w, i)
				req := nodeReq("n", "tent/cluster=prod", tag)
				in, err := f.CreateInstance(ctx, &req)
				if err != nil {
					t.Errorf("CreateInstance: %v", err)
					return
				}
				if _, err := f.GetInstance(ctx, in.ID); err != nil {
					t.Errorf("GetInstance: %v", err)
				}
				if list, err := f.ListInstances(ctx, tag); err != nil || len(list) != 1 {
					t.Errorf("ListInstances(%q) = %v, %v; want one instance", tag, list, err)
				}
				if err := f.UpdateInstance(ctx, in.ID, &govultr.InstanceUpdateReq{UserData: "c3R1Ygo="}); err != nil {
					t.Errorf("UpdateInstance: %v", err)
				}
				if _, err := f.ListInstanceVPCs(ctx, in.ID); err != nil {
					t.Errorf("ListInstanceVPCs: %v", err)
				}
				_ = f.Instances()
			}
		})
	}
	wg.Wait()

	const n = workers * rounds
	all, err := f.ListInstances(t.Context(), "tent/cluster=prod")
	if err != nil || len(all) != n {
		t.Fatalf("ListInstances = %d instances, %v; want %d", len(all), err, n)
	}
	ids, ips, macs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, in := range all {
		ids[in.ID] = true
		for _, v := range mustListVPCs(t, f, in.ID) {
			ips[v.IPAddress], macs[v.MacAddress] = true, true
		}
	}
	if len(ids) != n || len(ips) != n || len(macs) != n {
		t.Errorf("%d distinct ids, %d addresses and %d MACs; want %d of each", len(ids), len(ips), len(macs), n)
	}
}
