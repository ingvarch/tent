package vultr_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/engine/enginetest"
	"github.com/ingvarch/tent/internal/model"
)

// vpcDescription returns the description that tent gives cluster prod's VPC when the create carries the operation id
// op.
func vpcDescription(op string) string { return "tent:cluster=prod;kind=vpc;op=" + op }

// prodVPC returns cluster prod's VPC in ams with the network 10.64.0.0/16, as a create with the operation id op
// leaves it.
func prodVPC(op string) govultr.VPC {
	return govultr.VPC{Region: "ams", Description: vpcDescription(op), V4Subnet: "10.64.0.0", V4SubnetMask: 16}
}

// newVPCFixture returns the VPC task of cluster prod in region with the network cidr, and every infrastructure kind,
// on an empty fake. The provider's operation ids are op-1, op-2 and so on.
func newVPCFixture(t *testing.T, region, cidr string) *fixture {
	t.Helper()
	x := newFixture()
	m := model.Cluster{Name: "prod", Region: region, CIDR: netip.MustParsePrefix(cidr)}
	tasks, err := infraTasks(t, x.p, m, "vultr.VPC")
	if err != nil {
		t.Fatalf("BuildInfra: %v", err)
	}
	x.tasks, x.kinds = tasks, x.p.InfraKinds()
	return x
}

// newVPCPruneFixture returns a fixture without tasks, as delete cluster plans, that knows every infrastructure kind.
func newVPCPruneFixture() *fixture {
	x := newFixture()
	x.kinds = x.p.InfraKinds()
	return x
}

// wantVPCs checks the regions, descriptions and subnets of the fake's VPCs, in any order.
func wantVPCs(t *testing.T, f *vultrfake.Fake, want ...govultr.VPC) {
	t.Helper()
	opts := cmp.Options{
		cmpopts.IgnoreFields(govultr.VPC{}, "ID", "DateCreated"),
		cmpopts.SortSlices(func(a, b govultr.VPC) bool { return a.Description < b.Description }),
		cmpopts.EquateEmpty(),
	}
	if diff := cmp.Diff(want, f.VPCs(), opts); diff != "" {
		t.Errorf("VPCs (-want +got):\n%s", diff)
	}
}

func TestVPCTask(t *testing.T) {
	task := newVPCFixture(t, "ams", "10.64.0.0/16").tasks[0]
	if got := task.Key(); got != vpcKey {
		t.Errorf("Key() = %v, want %v", got, vpcKey)
	}
	if deps := task.Deps(); len(deps) != 0 {
		t.Errorf("Deps() = %v, want nothing", deps)
	}
}

func TestVPCTaskApplyReplan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		region string
		cidr   string
		lose   bool // the create's answer is lost
		want   govultr.VPC
	}{
		{name: "a /16 in ams", region: "ams", cidr: "10.64.0.0/16", want: prodVPC("op-1")},
		{
			name: "a /24 in ewr", region: "ewr", cidr: "192.168.8.0/24",
			want: govultr.VPC{
				Region: "ewr", Description: vpcDescription("op-1"), V4Subnet: "192.168.8.0", V4SubnetMask: 24,
			},
		},
		{name: "the create's answer lost", region: "ams", cidr: "10.64.0.0/16", lose: true, want: prodVPC("op-1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newVPCFixture(t, tc.region, tc.cidr)
			if tc.lose {
				x.f.LoseResponse(t, "CreateVPC", 1)
			}
			enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
			wantVPCs(t, x.f, tc.want)
			if got := countCalls(x.f, "CreateVPC"); got != 1 {
				t.Errorf("%d CreateVPC calls, want 1", got)
			}
		})
	}
}

func TestVPCTaskPlanText(t *testing.T) {
	x := newVPCFixture(t, "ams", "10.64.0.0/16")
	p, err := x.plan(t)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var b strings.Builder
	if err := p.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	const want = "+ vultr.VPC/prod\n" +
		"    + cidr: 10.64.0.0/16\n" +
		"    + region: ams\n" +
		"\n" +
		"Plan: 1 to create, 0 to update, 0 to replace, 0 to delete.\n"
	if diff := cmp.Diff(want, b.String()); diff != "" {
		t.Errorf("plan text (-want +got):\n%s", diff)
	}
}

func TestVPCTaskOutputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose bool // the create's answer is lost
	}{
		{name: "created"},
		{name: "adopted after a lost answer", lose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newVPCFixture(t, "ams", "10.64.0.0/16")
			if tc.lose {
				x.f.LoseResponse(t, "CreateVPC", 1)
			}
			id := x.createdID(t)
			if vpcs := x.f.VPCs(); len(vpcs) != 1 || id != vpcs[0].ID {
				t.Errorf("output id = %q, and the fake holds %+v; want the id of its one VPC", id, vpcs)
			}
		})
	}
}

func TestVPCTaskAdoptsAnExistingVPC(t *testing.T) {
	x := newVPCFixture(t, "ams", "10.64.0.0/16")
	seeded := x.f.AddVPC(t, prodVPC("op-earlier")) // an earlier run created it
	x.wantAdopted(t, seeded.ID, "CreateVPC")
	wantVPCs(t, x.f, seeded)
}

func TestVPCTaskNetworkChange(t *testing.T) {
	unreadable := prodVPC("op-earlier")
	unreadable.V4Subnet = "10.64.0"
	for _, tc := range []struct {
		name   string
		region string      // of the spec
		cidr   string      // of the spec
		vpc    govultr.VPC // the cluster's VPC in Vultr
		want   string
	}{
		{
			"another network", "ams", "10.65.0.0/16", prodVPC("op-earlier"),
			"the VPC of cluster prod is 10.64.0.0/16 in ams; the spec asks for 10.65.0.0/16 in ams, " +
				"and tent cannot move a cluster's network",
		},
		{
			"a longer prefix", "ams", "10.64.0.0/20", prodVPC("op-earlier"),
			"the VPC of cluster prod is 10.64.0.0/16 in ams; the spec asks for 10.64.0.0/20 in ams, " +
				"and tent cannot move a cluster's network",
		},
		{
			"another region", "ewr", "10.64.0.0/16", prodVPC("op-earlier"),
			"the VPC of cluster prod is 10.64.0.0/16 in ams; the spec asks for 10.64.0.0/16 in ewr, " +
				"and tent cannot move a cluster's network",
		},
		{
			"a subnet that does not parse", "ams", "10.64.0.0/16", unreadable,
			"the VPC of cluster prod is 10.64.0/16 in ams; the spec asks for 10.64.0.0/16 in ams, " +
				"and tent cannot move a cluster's network",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newVPCFixture(t, tc.region, tc.cidr)
			x.f.AddVPC(t, tc.vpc)
			_, err := x.plan(t)
			if want := "plan vultr.VPC/prod: " + tc.want; err == nil || err.Error() != want {
				t.Errorf("NewPlan error = %v, want %q", err, want)
			}
		})
	}
}

func TestVPCTaskLostCreateAndFailedSearch(t *testing.T) {
	x := newVPCFixture(t, "ams", "10.64.0.0/16")
	// The second attempt searches, finds the VPC and does not create it again.
	x.wantSearchAfterLostCreate(t, vultrfake.Call{Name: "CreateVPC", Arg: vpcDescription("op-1")}, "ListVPCs",
		"/v2/vpcs")
	wantVPCs(t, x.f, prodVPC("op-1"))
}

func TestVPCTaskRegionFull(t *testing.T) {
	x := newVPCFixture(t, "ams", "10.64.0.0/16")
	for i := range 5 {
		x.f.AddVPC(t, govultr.VPC{Region: "ams", Description: fmt.Sprintf("another VPC %d", i)})
	}
	events, err := x.applyInBubble(t, func(testing.TB) {})
	// tent did not count the region's VPCs, so Vultr's message states the fact.
	const want = "vultr.VPC/prod: ams may already have 5 VPCs, the most Vultr allows in a region: " +
		"vultr: POST /v2/vpcs: 400 Bad Request: You have reached the maximum number of VPC networks in this region."
	if err == nil || err.Error() != want {
		t.Errorf("apply error = %v, want %q", err, want)
	}
	if !errors.Is(err, vultr.ErrLimitReached) {
		t.Errorf("errors.Is(%v, vultr.ErrLimitReached) = false", err)
	}
	// Vultr does not raise the limit of 5 VPCs per region, so the error does not say how to raise a limit.
	if err != nil && strings.Contains(err.Error(), "Billing, Limits") {
		t.Errorf("apply error = %q, want no hint about account limits", err)
	}
	if n := countEvents(events, engine.Retrying); n != 0 {
		t.Errorf("the engine retried %d times, want never", n)
	}
	if got := len(x.f.VPCs()); got != 5 {
		t.Errorf("the fake holds %d VPCs, want the 5 it had", got)
	}
}

func TestVPCDeleteWhileServersAttached(t *testing.T) {
	x := newVPCPruneFixture()
	vpc := x.f.AddVPC(t, prodVPC("op-earlier"))
	// Vultr refuses the delete for a while after the VPC's servers are gone.
	attached := vultr.NewAPIError(http.MethodDelete, "/v2/vpcs/"+vpc.ID, http.StatusBadRequest,
		"The following servers are attached to this VPC network: 10.64.0.3", 0)
	events := x.applyWithFaults(t, func(tb testing.TB) { x.f.Fail(tb, "DeleteVPC", attached, 2) })
	var retries int
	for _, e := range events {
		if e.Type != engine.Retrying {
			continue
		}
		retries++
		if !errors.Is(e.Err, vultr.ErrInUse) {
			t.Errorf("the engine retried after %v, want an error that matches vultr.ErrInUse", e.Err)
		}
	}
	if retries != 2 {
		t.Errorf("the engine retried %d times, want twice", retries)
	}
	if got := countCalls(x.f, "DeleteVPC"); got != 3 {
		t.Errorf("%d DeleteVPC calls, want 3", got)
	}
	wantVPCs(t, x.f)
}

func TestVPCDeleteOfAGoneVPC(t *testing.T) {
	f := vultrfake.New()
	p, _ := newProvider(f)
	obj := engine.Object{Key: vpcKey, ID: "vpc-9"}
	if err := deleterOf(t, p, "vultr.VPC").Delete(t.Context(), &engine.Env{}, obj); err != nil {
		t.Errorf("Delete of a VPC that is gone: %v, want success", err)
	}
	wantCalls(t, f, vultrfake.Call{Name: "DeleteVPC", Arg: "vpc-9"})
}

func TestVPCPrune(t *testing.T) {
	x := newVPCPruneFixture()
	vpc := x.f.AddVPC(t, prodVPC("op-earlier"))
	other := x.f.AddVPC(t, govultr.VPC{
		Region: "ams", Description: "tent:cluster=staging;kind=vpc;op=op-a", V4Subnet: "10.65.0.0", V4SubnetMask: 16,
	})
	p, err := x.plan(t)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := []engine.PlannedChange{{Key: vpcKey, ID: vpc.ID, Change: engine.Change{Action: engine.Delete}}}
	if diff := cmp.Diff(want, p.Changes()); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	wantVPCs(t, x.f, other)
}
