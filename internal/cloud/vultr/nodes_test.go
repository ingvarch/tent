package vultr_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/engine"
	"github.com/ingvarch/tent/internal/engine/enginetest"
)

// specHash is the spec hash of the nodes that the tests create.
const specHash = "3f9a1c0b7d2e4f68"

// Operation ids of the nodes that the tests create.
const (
	opA = "00000000-0000-4000-8000-00000000000a"
	opB = "00000000-0000-4000-8000-00000000000b"
	opC = "00000000-0000-4000-8000-00000000000c"
)

// userData is the user data of the nodes that the tests create.
var userData = []byte("#cloud-config\n")

// equateAddrs lets cmp compare netip.Addr, whose fields are unexported.
var equateAddrs = cmpopts.EquateComparable(netip.Addr{})

// nodeRequest returns the request for the node name of cluster prod in the node group group with role, in ams, with
// the operation id op.
func nodeRequest(name, group string, role v1alpha1.Role, op string) cloud.CreateRequest {
	return cloud.CreateRequest{
		Cluster: "prod", Group: group, Role: role, Zone: "ams", Name: name, MachineType: "vc2-2c-4gb",
		Image: "ubuntu-24.04", SpecHash: specHash, Op: op, UserData: userData,
	}
}

// serverRequest returns the request for the node prod-servers-0 with the operation id op.
func serverRequest(op string) cloud.CreateRequest {
	return nodeRequest("prod-servers-0", "servers", v1alpha1.RoleServer, op)
}

// newNodesFixture returns a fixture whose fake holds the applied infrastructure of the example cluster with the SSH
// keys keys, and the inventory after the apply. Outside a synctest bubble, Create waits in real time.
func newNodesFixture(t *testing.T, keys ...string) (*fixture, *vultr.Snapshot) {
	t.Helper()
	x := newFixture()
	c := exampleClusterWith(func(c *v1alpha1.Cluster) { c.Spec.SSHKeys = keys })
	tasks, err := x.p.BuildInfra(t.Context(), modelOf(t, c, exampleGroups()))
	if err != nil {
		t.Fatalf("BuildInfra: %v", err)
	}
	x.tasks, x.kinds = tasks, x.p.InfraKinds()
	enginetest.ApplyReplan(t, x.tasks, x.kinds, x.inventory)
	return x, inventory(t, x.p)
}

// seedInfra stores cluster prod's VPC in ams with the network 10.64.0.0/mask and its servers' firewall group in f,
// without SSH keys and without a clients' group. It returns the VPC.
func seedInfra(t *testing.T, f *vultrfake.Fake, mask int) govultr.VPC {
	t.Helper()
	vpc := f.AddVPC(t, govultr.VPC{Region: "ams", Description: vpcMarker, V4Subnet: "10.64.0.0", V4SubnetMask: mask})
	f.AddFirewallGroup(t, govultr.FirewallGroup{Description: serversMarker})
	return vpc
}

// callNames returns the names of calls.
func callNames(calls []vultrfake.Call) []string {
	var names []string
	for _, c := range calls {
		names = append(names, c.Name)
	}
	return names
}

// wantCallsSince checks the calls that reached f after its first n.
func wantCallsSince(t *testing.T, f *vultrfake.Fake, n int, want ...vultrfake.Call) {
	t.Helper()
	if diff := cmp.Diff(want, f.Calls()[n:]); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
}

// createNode creates a node with p, and stops the test when that fails.
func createNode(t *testing.T, p *vultr.Provider, req cloud.CreateRequest) cloud.Instance {
	t.Helper()
	in, err := p.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("Create %s: %v", req.Name, err)
	}
	return in
}

// kept returns what the snapshot s keeps for the key k with get. It stops the test when s keeps nothing for k.
func kept[T any](t *testing.T, get func(engine.Key) (T, bool), k engine.Key) T {
	t.Helper()
	v, ok := get(k)
	if !ok {
		t.Fatalf("the inventory keeps no %s", k)
	}
	return v
}

func TestCreate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, snap := newNodesFixture(t, opsKey, devKey)
		vpc := kept(t, snap.VPC, vpcKey)
		servers := kept(t, snap.FirewallGroup, serversKey)
		clients := kept(t, snap.FirewallGroup, clientsKey)
		ops := kept(t, snap.SSHKey, sshKeyOf(opsFP))
		dev := kept(t, snap.SSHKey, sshKeyOf(devFP))
		before, start := len(x.f.Calls()), time.Now()

		got := createNode(t, x.p, serverRequest(opA))

		want := cloud.Instance{
			ID: "instance-1", Name: "prod-servers-0", Cluster: "prod", Group: "servers", Role: v1alpha1.RoleServer,
			Zone: "ams", SpecHash: specHash, Op: opA, PrivateIP: netip.MustParseAddr("10.64.0.3"),
			PublicIP: netip.MustParseAddr("198.18.0.1"), Ready: true, Created: start.Truncate(time.Second),
		}
		if diff := cmp.Diff(want, got, equateAddrs); diff != "" {
			t.Errorf("Create (-want +got):\n%s", diff)
		}
		req, ok := x.f.CreateRequest(got.ID)
		if !ok {
			t.Fatalf("the fake has no create request of %s", got.ID)
		}
		wantReq := govultr.InstanceCreateReq{
			Region: "ams", Plan: "vc2-2c-4gb", OsID: 2284, Label: "prod-servers-0", Hostname: "prod-servers-0",
			Tags: []string{
				"tent/cluster=prod", "tent/nodegroup=servers", "tent/op=" + opA, "tent/role=server",
				"tent/spec-hash=" + specHash,
			},
			FirewallGroupID: servers.ID, AttachVPC: []string{vpc.ID}, SSHKeys: []string{ops.ID, dev.ID},
			Backups: "disabled", UserData: base64.StdEncoding.EncodeToString(userData),
		}
		if diff := cmp.Diff(wantReq, req); diff != "" {
			t.Errorf("the create request (-want +got):\n%s", diff)
		}
		// The search by operation id, the inventory, the create, then reads 5 s apart: pending, booting, ready.
		wantCallsSince(t, x.f, before, slices.Concat(
			[]vultrfake.Call{{Name: "ListInstances", Arg: "tent/op=" + opA}},
			listCalls,
			[]vultrfake.Call{
				{Name: "ListFirewallRules", Arg: clients.ID}, {Name: "ListFirewallRules", Arg: servers.ID},
				{Name: "CreateInstance", Arg: "prod-servers-0"},
				{Name: "GetInstance", Arg: "instance-1"}, {Name: "GetInstance", Arg: "instance-1"},
				{Name: "GetInstance", Arg: "instance-1"}, {Name: "ListInstanceVPCs", Arg: "instance-1"},
			})...)
		if d := time.Since(start); d != 10*time.Second {
			t.Errorf("Create took %v, want 10s", d)
		}

		// The next node gets the next address.
		next := createNode(t, x.p, nodeRequest("prod-servers-1", "servers", v1alpha1.RoleServer, opB))
		if want := netip.MustParseAddr("10.64.0.4"); next.PrivateIP != want {
			t.Errorf("the second node's private address is %v, want %v", next.PrivateIP, want)
		}
	})
}

func TestCreateFirewallGroupOfRole(t *testing.T) {
	for _, tc := range []struct {
		group string
		role  v1alpha1.Role
		want  engine.Key // the firewall group the node joins
	}{
		{"servers", v1alpha1.RoleServer, serversKey},
		{"workers", v1alpha1.RoleClient, clientsKey},
		{"dev", v1alpha1.RoleCombined, serversKey},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x, snap := newNodesFixture(t, opsKey)
				group := kept(t, snap.FirewallGroup, tc.want)
				in := createNode(t, x.p, nodeRequest("prod-"+tc.group+"-0", tc.group, tc.role, opA))
				if in.Group != tc.group || in.Role != tc.role {
					t.Errorf("Create returned the group %q and the role %q, want %q and %q", in.Group, in.Role,
						tc.group, tc.role)
				}
				if req, _ := x.f.CreateRequest(in.ID); req.FirewallGroupID != group.ID {
					t.Errorf("firewall_group_id = %q, want %q (%s)", req.FirewallGroupID, group.ID, tc.want)
				}
			})
		})
	}
}

func TestCreateAdoptsTheInstanceOfItsOp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		first := createNode(t, x.p, serverRequest(opA))
		before := len(x.f.Calls())

		again := createNode(t, x.p, serverRequest(opA))

		if diff := cmp.Diff(first, again, equateAddrs); diff != "" {
			t.Errorf("the second Create (-first +second):\n%s", diff)
		}
		// The search finds the instance: no inventory and no create.
		wantCallsSince(t, x.f, before,
			vultrfake.Call{Name: "ListInstances", Arg: "tent/op=" + opA},
			vultrfake.Call{Name: "GetInstance", Arg: first.ID}, vultrfake.Call{Name: "ListInstanceVPCs", Arg: first.ID})
	})
}

func TestCreateSkipsAnotherClustersInstanceWithItsOp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := vultrfake.New()
		seedInfra(t, f, 16)
		other := f.AddInstance(t, govultr.Instance{
			Label: "staging-servers-0", Tags: []string{"tent/cluster=staging", "tent/op=" + opA},
		})

		got := createNode(t, opProvider(f), serverRequest(opA))

		if got.ID == other.ID {
			t.Errorf("Create adopted the instance %s of cluster staging", other.ID)
		}
		if n := countCalls(f, "CreateInstance"); n != 1 {
			t.Errorf("%d CreateInstance calls, want 1", n)
		}
	})
}

func TestCreateAdoptsTheOldestInstanceOfItsOp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := vultrfake.New()
		vpc := seedInfra(t, f, 16)
		for _, in := range []govultr.Instance{
			{ID: "b-newer", DateCreated: sept25}, {ID: "c-older", DateCreated: sept20},
			{ID: "a-bad-date", DateCreated: "yesterday"}, {ID: "d-older", DateCreated: sept20},
		} {
			f.AddInstance(t, seededServer(in), vpc.ID)
		}
		p, log := newProvider(f)

		got := createNode(t, p, serverRequest(opA))

		// The oldest, then the one with the lowest id; a date that does not parse counts as the newest.
		if got.ID != "c-older" {
			t.Errorf("Create adopted %s, want c-older", got.ID)
		}
		if n := countCalls(f, "CreateInstance"); n != 0 {
			t.Errorf("%d CreateInstance calls, want none", n)
		}
		// The others, in the order of the choice.
		want := []map[string]string{{
			"level": "WARN", "msg": "adopting the oldest of several Vultr instances with one operation id",
			"cluster": "prod", "op": opA, "id": "c-older", "others": "d-older, b-newer, a-bad-date",
		}}
		if diff := cmp.Diff(want, logRecords(t, log)); diff != "" {
			t.Errorf("log (-want +got):\n%s", diff)
		}
	})
}

// seededServer returns in as the node prod-servers-0 of cluster prod in ams, created with the operation id opA.
func seededServer(in govultr.Instance) govultr.Instance {
	in.Hostname, in.Label, in.Region = "prod-servers-0", "prod-servers-0", "ams"
	in.Tags = []string{"tent/cluster=prod", "tent/nodegroup=servers", "tent/op=" + opA, "tent/role=server"}
	return in
}

// TestCreateRefusesAnotherNodeWithItsOp checks that Create does not adopt an instance with the request's operation id
// that is another node: the caller gave one operation id to two nodes.
func TestCreateRefusesAnotherNodeWithItsOp(t *testing.T) {
	const refused = "create node prod-servers-0 of cluster prod: instance inst with the operation id " + opA +
		" is another node: %s; each node needs its own operation id"
	for _, tc := range []struct {
		name string
		edit func(in *govultr.Instance)
		want string // what differs; "" when Create adopts the instance
	}{
		{"name", func(in *govultr.Instance) { in.Hostname = "prod-servers-1" }, "name prod-servers-1 (want prod-servers-0)"},
		{"name without a hostname", func(in *govultr.Instance) { in.Hostname, in.Label = "", "prod-servers-1" },
			"name prod-servers-1 (want prod-servers-0)"},
		{"group", func(in *govultr.Instance) { in.Tags[1] = "tent/nodegroup=dev" }, "group dev (want servers)"},
		{"role", func(in *govultr.Instance) { in.Tags[3] = "tent/role=combined" }, "role combined (want server)"},
		{"zone", func(in *govultr.Instance) { in.Region = "ewr" }, "zone ewr (want ams)"},
		{"several", func(in *govultr.Instance) { in.Hostname, in.Region = "prod-dev-0", "ewr" },
			"name prod-dev-0 (want prod-servers-0), zone ewr (want ams)"},
		// The label can be changed in the console; the hostname only by a reinstall.
		{"a label changed in the console", func(in *govultr.Instance) { in.Label = "db primary" }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := vultrfake.New()
				vpc := seedInfra(t, f, 16)
				in := seededServer(govultr.Instance{ID: "inst"})
				tc.edit(&in)
				f.AddInstance(t, in, vpc.ID)

				got, err := opProvider(f).Create(t.Context(), serverRequest(opA))

				if tc.want == "" {
					if err != nil || got.ID != "inst" {
						t.Errorf("Create = %+v, %v; want the instance inst", got, err)
					}
					return
				}
				if want := fmt.Sprintf(refused, tc.want); errText(err) != want {
					t.Errorf("Create = %v, want %q", err, want)
				}
				// Neither a create nor a wait.
				if diff := cmp.Diff([]string{"ListInstances"}, callNames(f.Calls())); diff != "" {
					t.Errorf("calls (-want +got):\n%s", diff)
				}
			})
		})
	}
}

func TestCreateLostAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		before := len(x.f.Calls())
		x.f.LoseResponse(t, "CreateInstance", 1)

		got := createNode(t, x.p, serverRequest(opA))

		// The search after the create finds the instance.
		if got.ID != "instance-1" || !got.Ready || got.PrivateIP != netip.MustParseAddr("10.64.0.3") {
			t.Errorf("Create = %+v, want instance-1 ready at 10.64.0.3", got)
		}
		names := callNames(x.f.Calls()[before:])
		i := slices.Index(names, "CreateInstance")
		if i < 0 || i+1 == len(names) || names[i+1] != "ListInstances" {
			t.Errorf("calls %v, want a search right after the create", names)
		}
		if n := countCalls(x.f, "CreateInstance"); n != 1 {
			t.Errorf("%d CreateInstance calls, want 1", n)
		}
	})
}

func TestCreateLostAnswerNotListedYet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		// No answer, and the search lists nothing: here Vultr did not create the instance at all.
		x.f.Fail(t, "CreateInstance", vultr.NewNoAnswerError(http.MethodPost, "/v2/instances", nil), 1)

		_, err := x.p.Create(t.Context(), serverRequest(opA))

		const want = "create node prod-servers-0 of cluster prod: vultr: POST /v2/instances: no answer; " +
			"no instance with the operation id " + opA + " is listed yet"
		if errText(err) != want {
			t.Errorf("Create = %v, want %q", err, want)
		}
		if !errors.Is(err, vultr.ErrUnavailable) {
			t.Errorf("errors.Is(%v, vultr.ErrUnavailable) = false", err)
		}
		// Called again with the same operation id, it searches, then creates the instance.
		createNode(t, x.p, serverRequest(opA))
		if n := len(x.f.Instances()); n != 1 {
			t.Errorf("%d instances, want 1", n)
		}
	})
}

// afterCreate is a fake that runs hook after each CreateInstance call, so that a test can set faults for the calls
// that follow the create.
type afterCreate struct {
	*vultrfake.Fake
	hook func()
}

func (a *afterCreate) CreateInstance(ctx context.Context, req *govultr.InstanceCreateReq) (*govultr.Instance,
	error) {
	in, err := a.Fake.CreateInstance(ctx, req)
	a.hook()
	return in, err
}

func TestCreateLostAnswerSearchFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		p := opProvider(&afterCreate{Fake: x.f, hook: func() { x.f.Throttle(t, "ListInstances", 0, 1) }})
		x.f.LoseResponse(t, "CreateInstance", 1)

		_, err := p.Create(t.Context(), serverRequest(opA))

		const want = "create node prod-servers-0 of cluster prod: vultr: POST /v2/instances: " +
			"vultrfake: the answer was lost; search by operation id: vultr: GET /v2/instances: " +
			"429 Too Many Requests: Rate limit exceeded"
		if errText(err) != want {
			t.Errorf("Create = %v, want %q", err, want)
		}
		// The outcome of the create is unknown, and the search was throttled.
		for _, class := range []error{vultr.ErrUnavailable, vultr.ErrRateLimited} {
			if !errors.Is(err, class) {
				t.Errorf("errors.Is(%v, %v) = false", err, class)
			}
		}
		// Called again with the same operation id, it finds the instance and does not create another.
		if got := createNode(t, p, serverRequest(opA)); got.ID != "instance-1" {
			t.Errorf("the second Create returned %s, want instance-1", got.ID)
		}
		if n := countCalls(x.f, "CreateInstance"); n != 1 {
			t.Errorf("%d CreateInstance calls, want 1", n)
		}
	})
}

// answerWithoutID is a fake whose CreateInstance answers with answer and no error. When create is set, the fake
// creates the instance first.
type answerWithoutID struct {
	*vultrfake.Fake
	create bool
	answer *govultr.Instance
}

func (a *answerWithoutID) CreateInstance(ctx context.Context, req *govultr.InstanceCreateReq) (*govultr.Instance,
	error) {
	if a.create {
		if _, err := a.Fake.CreateInstance(ctx, req); err != nil {
			return nil, err
		}
	}
	return a.answer, nil
}

// TestCreateAnswerWithoutID checks that a create answer without an instance id counts as an answer that tent cannot
// read: Create searches by the operation id, as after a lost answer.
func TestCreateAnswerWithoutID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create bool // whether Vultr created the instance
		answer *govultr.Instance
		want   string // the error's text; "" when Create adopts instance-1
	}{
		{"no id", true, &govultr.Instance{Status: "pending"}, ""},
		{"no instance", true, nil, ""},
		{"no id, and nothing created", false, &govultr.Instance{},
			"create node prod-servers-0 of cluster prod: vultr: POST /v2/instances: the answer holds no instance id; " +
				"no instance with the operation id " + opA + " is listed yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				x, _ := newNodesFixture(t, opsKey)
				p := opProvider(&answerWithoutID{Fake: x.f, create: tc.create, answer: tc.answer})

				got, err := p.Create(t.Context(), serverRequest(opA))

				if tc.want != "" {
					if errText(err) != tc.want {
						t.Errorf("Create = %v, want %q", err, tc.want)
					}
					if !errors.Is(err, vultr.ErrUnavailable) {
						t.Errorf("errors.Is(%v, vultr.ErrUnavailable) = false", err)
					}
					return
				}
				if err != nil || got.ID != "instance-1" || !got.Ready {
					t.Errorf("Create = %+v, %v; want instance-1 ready", got, err)
				}
				if n := searches(x.f, opA); n != 2 {
					t.Errorf("%d searches by operation id, want 2: before and after the create", n)
				}
			})
		})
	}
}

// unspecifiedAddress is a fake whose ListInstanceVPCs gives the address 0.0.0.0 in every VPC in its first n answers
// that list a VPC.
type unspecifiedAddress struct {
	*vultrfake.Fake
	n int
}

func (u *unspecifiedAddress) ListInstanceVPCs(ctx context.Context, id string) ([]govultr.VPCInfo, error) {
	vpcs, err := u.Fake.ListInstanceVPCs(ctx, id)
	if len(vpcs) > 0 && u.n > 0 {
		u.n--
		for i := range vpcs {
			vpcs[i].IPAddress = "0.0.0.0"
		}
	}
	return vpcs, err
}

// TestCreateWaitsForAnAddress checks that the address 0.0.0.0 in the VPC counts as none: Create waits on.
func TestCreateWaitsForAnAddress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		start := time.Now()

		got := createNode(t, opProvider(&unspecifiedAddress{Fake: x.f, n: 2}), serverRequest(opA))

		// Ready on the third read at 10 s; the address comes on the fifth, at 20 s.
		if want := netip.MustParseAddr("10.64.0.3"); got.PrivateIP != want {
			t.Errorf("the private address is %v, want %v", got.PrivateIP, want)
		}
		if n := countCalls(x.f, "ListInstanceVPCs"); n != 3 {
			t.Errorf("%d ListInstanceVPCs calls, want 3", n)
		}
		if d := time.Since(start); d != 20*time.Second {
			t.Errorf("Create took %v, want 20s", d)
		}
	})
}

func TestCreateWaitsUntilReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		p, _ := newProvider(x.f, vultr.WithPollInterval(time.Second))
		x.f.SetBootReads(t, 2, 4)
		start := time.Now()

		got := createNode(t, p, serverRequest(opA))

		// Pending twice, booting twice, then ready: five reads a second apart, and the VPCs once it is ready.
		if !got.Ready {
			t.Error("Create returned an instance that is not ready")
		}
		if n := countCalls(x.f, "GetInstance"); n != 5 {
			t.Errorf("%d GetInstance calls, want 5", n)
		}
		if n := countCalls(x.f, "ListInstanceVPCs"); n != 1 {
			t.Errorf("%d ListInstanceVPCs calls, want 1", n)
		}
		if d := time.Since(start); d != 4*time.Second {
			t.Errorf("Create took %v, want 4s", d)
		}
	})
}

func TestCreateWaitEndsWithTheContext(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mask      int // the size of the VPC's network
		bootReads int // the reads after which the instance is ready
		vpcReads  int // how many times Create reads the instance's VPCs
	}{
		{"never ready", 16, 1000, 0},
		// Ready from the second read at 5 s on, but a VPC this small gives out no address.
		{"no address in the VPC", 30, 1, 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := vultrfake.New()
				seedInfra(t, f, tc.mask)
				f.SetBootReads(t, tc.bootReads, tc.bootReads)
				ctx, cancel := context.WithTimeout(t.Context(), 58*time.Second)
				defer cancel()

				_, err := opProvider(f).Create(ctx, serverRequest(opA))

				const want = "create node prod-servers-0 of cluster prod: wait for instance instance-1: " +
					"context deadline exceeded"
				if errText(err) != want {
					t.Errorf("Create = %v, want %q", err, want)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false", err)
				}
				if n := countCalls(f, "ListInstanceVPCs"); n != tc.vpcReads {
					t.Errorf("%d ListInstanceVPCs calls, want %d", n, tc.vpcReads)
				}
			})
		})
	}
}

func TestCreateFails(t *testing.T) {
	// The calls of the search by operation id and of the inventory of the seeded infrastructure.
	search := []string{"ListInstances", "ListSSHKeys", "ListVPCs", "ListFirewallGroups", "ListInstances"}
	for _, tc := range []struct {
		name  string
		mask  int // the size of the seeded VPC's network; 0 for no infrastructure
		edit  func(r *cloud.CreateRequest)
		want  string   // the error's text
		calls []string // the calls Create sends
	}{
		{"invalid request", 16, func(r *cloud.CreateRequest) { r.Op = "" }, "create request: no operation id", nil},
		{"operation id not a UUID", 16, func(r *cloud.CreateRequest) { r.Op = "op-1" },
			`create request: operation id "op-1" is not one that NewOpID makes (a lower-case UUID of version 4)`, nil},
		{"unsupported image", 16, func(r *cloud.CreateRequest) { r.Image = "debian-12" },
			`create node prod-servers-0 of cluster prod: tent supports ubuntu-24.04 and ubuntu-26.04 on Vultr, ` +
				`not "debian-12"`, nil},
		{"label in upper case", 16, func(r *cloud.CreateRequest) { r.Group = "Servers" },
			`create node prod-servers-0 of cluster prod: label "tent/nodegroup=Servers": upper case`, nil},
		{"no VPC", 0, func(*cloud.CreateRequest) {},
			"create node prod-servers-0 of cluster prod: cluster prod has no VPC; apply its infrastructure first",
			search},
		{"no firewall group for the role", 16, func(r *cloud.CreateRequest) {
			*r = nodeRequest("prod-workers-0", "workers", v1alpha1.RoleClient, opA)
		}, "create node prod-workers-0 of cluster prod: cluster prod has no firewall group prod-clients; apply its " +
			"infrastructure first", append(search, "ListFirewallRules")},
		{"zone outside the VPC's region", 16, func(r *cloud.CreateRequest) { r.Zone = "ewr" },
			`create node prod-servers-0 of cluster prod: zone "ewr": the VPC of cluster prod is in ams`,
			append(search, "ListFirewallRules")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := vultrfake.New()
			if tc.mask > 0 {
				seedInfra(t, f, tc.mask)
			}
			req := serverRequest(opA)
			tc.edit(&req)

			_, err := opProvider(f).Create(t.Context(), req)

			if errText(err) != tc.want {
				t.Errorf("Create = %v, want %q", err, tc.want)
			}
			if diff := cmp.Diff(tc.calls, callNames(f.Calls())); diff != "" {
				t.Errorf("calls (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateCallFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(tb testing.TB, f *vultrfake.Fake)
		want  string // the error's text after "create node prod-servers-0 of cluster prod: "
		class error
	}{
		{"search", func(tb testing.TB, f *vultrfake.Fake) { f.Throttle(tb, "ListInstances", 0, 1) },
			"search by operation id: vultr: GET /v2/instances: 429 Too Many Requests: Rate limit exceeded",
			vultr.ErrRateLimited},
		{"inventory", func(tb testing.TB, f *vultrfake.Fake) {
			f.Fail(tb, "ListVPCs", vultr.NewAPIError(http.MethodGet, "/v2/vpcs", 500, "Internal error", 0), 1)
		}, "inventory: vultr: GET /v2/vpcs: 500 Internal Server Error: Internal error",
			vultr.ErrUnavailable},
		{"instance limit", func(tb testing.TB, f *vultrfake.Fake) {
			f.Fail(tb, "CreateInstance", vultr.NewAPIError(http.MethodPost, "/v2/instances", 400,
				"You have reached the maximum number of instances allowed on your account", 0), 1)
		}, "vultr: POST /v2/instances: 400 Bad Request: You have reached the maximum number of instances allowed " +
			"on your account (an account limit can be raised in the Vultr console under Billing, Limits)",
			vultr.ErrLimitReached},
		{"create refused", func(tb testing.TB, f *vultrfake.Fake) {
			f.Fail(tb, "CreateInstance",
				vultr.NewAPIError(http.MethodPost, "/v2/instances", 400, "Invalid plan.", 0), 1)
		}, "vultr: POST /v2/instances: 400 Bad Request: Invalid plan.", vultr.ErrInvalid},
		{"wait", func(tb testing.TB, f *vultrfake.Fake) {
			f.Fail(tb, "GetInstance", vultr.NewAPIError(http.MethodGet, "/v2/instances/instance-1", 404,
				"Invalid instance ID.", 0), 1)
		}, "wait for instance instance-1: vultr: GET /v2/instances/instance-1: 404 Not Found: Invalid instance ID.",
			vultr.ErrNotFound},
		{"address", func(tb testing.TB, f *vultrfake.Fake) {
			f.SetBootReads(tb, 0, 0)
			f.Fail(tb, "ListInstanceVPCs", vultr.NewAPIError(http.MethodGet, "/v2/instances/instance-1/vpcs", 500,
				"Internal error", 0), 1)
		}, "wait for instance instance-1: vultr: GET /v2/instances/instance-1/vpcs: 500 Internal Server Error: " +
			"Internal error", vultr.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := vultrfake.New()
			seedInfra(t, f, 16)
			tc.fault(t, f)

			_, err := opProvider(f).Create(t.Context(), serverRequest(opA))

			if want := "create node prod-servers-0 of cluster prod: " + tc.want; errText(err) != want {
				t.Errorf("Create = %v, want %q", err, want)
			}
			if !errors.Is(err, tc.class) {
				t.Errorf("errors.Is(%v, %v) = false", err, tc.class)
			}
			// A create that got an answer is not followed by a search.
			if n := searches(f, opA); n != 1 {
				t.Errorf("%d searches by operation id, want 1", n)
			}
		})
	}
}

// searches returns how many calls that reached f listed the instances with the tag of the operation id op.
func searches(f *vultrfake.Fake, op string) int {
	n := 0
	for _, c := range f.Calls() {
		if c == (vultrfake.Call{Name: "ListInstances", Arg: "tent/op=" + op}) {
			n++
		}
	}
	return n
}

func TestList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		p, log := newProvider(x.f)
		worker := createNode(t, p, nodeRequest("prod-workers-0", "workers", v1alpha1.RoleClient, opA))
		server := createNode(t, p, serverRequest(opB))
		// A node of cluster prod that is not booted, without a VPC, a spec hash or a date that parses, with a tag of
		// its own.
		x.f.AddInstance(t, govultr.Instance{
			ID: "booting", Label: "prod-servers-1", Region: "ams", MainIP: "0.0.0.0", DateCreated: "yesterday",
			Status: "pending", PowerStatus: "stopped", ServerStatus: "none",
			Tags: []string{"tent/cluster=prod", "tent/nodegroup=servers", "tent/op=" + opC, "tent/role=server", "web"},
		})
		// Another cluster's instance, and one with a tag in upper case, which the tag filter lists too.
		x.f.AddInstance(t, govultr.Instance{ID: "staging", Label: "staging-servers-0",
			Tags: []string{"tent/cluster=staging"}})
		x.f.AddInstance(t, govultr.Instance{ID: "upper", Label: "prod-servers-2", Tags: []string{"tent/cluster=PROD"}})

		got, err := p.List(t.Context(), "prod")
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		booting := cloud.Instance{
			ID: "booting", Name: "prod-servers-1", Cluster: "prod", Group: "servers", Role: v1alpha1.RoleServer,
			Zone: "ams", Op: opC,
		}
		// By name, as Create returned them.
		if diff := cmp.Diff([]cloud.Instance{server, booting, worker}, got, equateAddrs); diff != "" {
			t.Errorf("List (-want +got):\n%s", diff)
		}
		want := []map[string]string{{
			"level": "WARN", "msg": "skipping a Vultr instance whose tags do not mark it as the cluster's",
			"cluster": "prod", "id": "upper", "reason": `tag "tent/cluster=PROD": upper case`,
		}}
		if diff := cmp.Diff(want, logRecords(t, log)); diff != "" {
			t.Errorf("log (-want +got):\n%s", diff)
		}

		// A cluster without nodes has none.
		if got, err := p.List(t.Context(), "dev"); err != nil || len(got) != 0 {
			t.Errorf("List dev = %+v, %v; want none", got, err)
		}
	})
}

func TestListFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster string
		fault   func(tb testing.TB, f *vultrfake.Fake)
		want    string // the error's text
	}{
		{"no cluster", "", func(testing.TB, *vultrfake.Fake) {},
			`list the nodes of cluster : label "tent/cluster=": empty value`},
		{"list", "prod", func(tb testing.TB, f *vultrfake.Fake) { f.Throttle(tb, "ListInstances", 0, 1) },
			"list the nodes of cluster prod: vultr: GET /v2/instances: 429 Too Many Requests: Rate limit exceeded"},
		{"VPCs", "prod", func(tb testing.TB, f *vultrfake.Fake) {
			f.Fail(tb, "ListInstanceVPCs", vultr.NewAPIError(http.MethodGet, "/v2/instances/node/vpcs", 500,
				"Internal error", 0), 1)
		}, "list the nodes of cluster prod: instance node: vultr: GET /v2/instances/node/vpcs: " +
			"500 Internal Server Error: Internal error"},
		{"the read after VPCs not found", "prod", func(tb testing.TB, f *vultrfake.Fake) {
			f.Fail(tb, "ListInstanceVPCs", vultr.NewAPIError(http.MethodGet, "/v2/instances/node/vpcs", 404,
				"Invalid instance ID.", 0), 1)
			f.Fail(tb, "GetInstance", vultr.NewAPIError(http.MethodGet, "/v2/instances/node", 500,
				"Internal error", 0), 1)
		}, "list the nodes of cluster prod: instance node: vultr: GET /v2/instances/node: " +
			"500 Internal Server Error: Internal error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := vultrfake.New()
			f.AddInstance(t, govultr.Instance{ID: "node", Label: "prod-servers-0", Tags: []string{"tent/cluster=prod"}})
			tc.fault(t, f)

			got, err := opProvider(f).List(t.Context(), tc.cluster)

			if errText(err) != tc.want {
				t.Errorf("List = %+v, %v; want the error %q", got, err, tc.want)
			}
		})
	}
}

// afterList is a fake that runs hook after each ListInstances call, so that a test can change what the calls that
// follow the list see.
type afterList struct {
	*vultrfake.Fake
	hook func()
}

func (a *afterList) ListInstances(ctx context.Context, tag string) ([]govultr.Instance, error) {
	list, err := a.Fake.ListInstances(ctx, tag)
	a.hook()
	return list, err
}

func TestListSkipsAnInstanceDeletedAfterTheList(t *testing.T) {
	f := vultrfake.New()
	vpc := seedInfra(t, f, 16)
	tags := []string{"tent/cluster=prod", "tent/nodegroup=servers", "tent/role=server"}
	f.AddInstance(t, govultr.Instance{ID: "kept", Label: "prod-servers-0", Tags: tags}, vpc.ID)
	f.AddInstance(t, govultr.Instance{ID: "gone", Label: "prod-servers-1", Tags: tags}, vpc.ID)
	p := opProvider(&afterList{Fake: f, hook: func() {
		if err := f.DeleteInstance(t.Context(), "gone"); err != nil {
			t.Errorf("DeleteInstance: %v", err)
		}
	}})

	got, err := p.List(t.Context(), "prod")

	if err != nil {
		t.Fatalf("List: %v, want the instance that is left", err)
	}
	if len(got) != 1 || got[0].ID != "kept" {
		t.Errorf("List = %+v, want the instance kept only", got)
	}
	// A 404 on the addresses alone does not skip an instance: the read of the instance confirms it is gone.
	wantCallsSince(t, f, 1, vultrfake.Call{Name: "DeleteInstance", Arg: "gone"},
		vultrfake.Call{Name: "ListInstanceVPCs", Arg: "kept"}, vultrfake.Call{Name: "ListInstanceVPCs", Arg: "gone"},
		vultrfake.Call{Name: "GetInstance", Arg: "gone"})
}

// TestListKeepsAnInstanceWithoutAddresses checks that an instance whose addresses are not found, as a pending
// instance's may be, stays in the list without a private address. Skipping it would let a quick rerun create a
// second node with its name.
func TestListKeepsAnInstanceWithoutAddresses(t *testing.T) {
	f := vultrfake.New()
	vpc := seedInfra(t, f, 16)
	f.AddInstance(t, seededServer(govultr.Instance{ID: "pending"}), vpc.ID)
	f.Fail(t, "ListInstanceVPCs", vultr.NewAPIError(http.MethodGet, "/v2/instances/pending/vpcs", 404,
		"Not found", 0), 1)

	got, err := opProvider(f).List(t.Context(), "prod")

	if err != nil || len(got) != 1 || got[0].ID != "pending" {
		t.Fatalf("List = %+v, %v; want the instance pending", got, err)
	}
	if got[0].PrivateIP.IsValid() {
		t.Errorf("the private address is %v, want none", got[0].PrivateIP)
	}
	wantCalls(t, f, vultrfake.Call{Name: "ListInstances", Arg: "tent/cluster=prod"},
		vultrfake.Call{Name: "ListInstanceVPCs", Arg: "pending"}, vultrfake.Call{Name: "GetInstance", Arg: "pending"})
}

func TestListUnspecifiedAddress(t *testing.T) {
	f := vultrfake.New()
	vpc := seedInfra(t, f, 16)
	f.AddInstance(t, seededServer(govultr.Instance{ID: "node"}), vpc.ID)

	got, err := opProvider(&unspecifiedAddress{Fake: f, n: 1}).List(t.Context(), "prod")

	if err != nil || len(got) != 1 {
		t.Fatalf("List = %+v, %v; want the node", got, err)
	}
	if got[0].PrivateIP.IsValid() {
		t.Errorf("the private address is %v, want none: 0.0.0.0 is no address", got[0].PrivateIP)
	}
}

// TestListNamesNodesByHostname checks that a node's name is its hostname, which only a reinstall changes and which is
// its Nomad node name, and its label only when it has no hostname: the label can be changed in the console.
func TestListNamesNodesByHostname(t *testing.T) {
	f := vultrfake.New()
	tags := []string{"tent/cluster=prod"}
	f.AddInstance(t, govultr.Instance{ID: "a", Hostname: "prod-servers-0", Label: "db primary", Tags: tags})
	f.AddInstance(t, govultr.Instance{ID: "b", Label: "prod-servers-1", Tags: tags})

	got, err := opProvider(f).List(t.Context(), "prod")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var names []string
	for _, in := range got {
		names = append(names, in.ID+" "+in.Name)
	}
	if diff := cmp.Diff([]string{"a prod-servers-0", "b prod-servers-1"}, names); diff != "" {
		t.Errorf("names (-want +got):\n%s", diff)
	}
}

func TestProviderNodes(t *testing.T) {
	p, _ := newProvider(vultrfake.New())
	if got := p.Nodes(); got != cloud.Nodes(p) {
		t.Errorf("Nodes() = %v, want the provider", got)
	}
}

// instanceOf returns the instance with id that f holds, as a read shows it now. It stops the test when f holds none.
func instanceOf(t *testing.T, f *vultrfake.Fake, id string) govultr.Instance {
	t.Helper()
	instances := f.Instances()
	i := slices.IndexFunc(instances, func(in govultr.Instance) bool { return in.ID == id })
	if i < 0 {
		t.Fatalf("the fake holds no instance %s", id)
	}
	return instances[i]
}

// instanceIDs returns the ids of the instances that f holds, in creation order.
func instanceIDs(f *vultrfake.Fake) []string {
	var ids []string
	for _, in := range f.Instances() {
		ids = append(ids, in.ID)
	}
	return ids
}

func TestStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		in := createNode(t, x.p, serverRequest(opA))
		before := len(x.f.Calls())

		if err := x.p.Stop(t.Context(), in); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		// A hard power-off: Vultr has no graceful shutdown.
		wantCallsSince(t, x.f, before, vultrfake.Call{Name: "HaltInstance", Arg: in.ID})
		if got := instanceOf(t, x.f, in.ID).PowerStatus; got != "stopped" {
			t.Errorf("power_status = %q, want stopped", got)
		}
	})
}

func TestDelete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		in := createNode(t, x.p, serverRequest(opA))
		other := createNode(t, x.p, nodeRequest("prod-servers-1", "servers", v1alpha1.RoleServer, opB))
		before := len(x.f.Calls())

		if err := x.p.Delete(t.Context(), in); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		wantCallsSince(t, x.f, before, vultrfake.Call{Name: "DeleteInstance", Arg: in.ID})
		if diff := cmp.Diff([]string{other.ID}, instanceIDs(x.f)); diff != "" {
			t.Errorf("instances (-want +got):\n%s", diff)
		}
	})
}

// scrubbedUserData is the user data that ScrubUserData leaves on a node: a cloud-config without modules.
const scrubbedUserData = "#cloud-config\n# tent removed this node's user data after the node joined the cluster\n"

// recordUpdates is a fake that records the request of each UpdateInstance call.
type recordUpdates struct {
	*vultrfake.Fake
	reqs []govultr.InstanceUpdateReq
}

func (a *recordUpdates) UpdateInstance(ctx context.Context, id string, req *govultr.InstanceUpdateReq) error {
	a.reqs = append(a.reqs, *req)
	return a.Fake.UpdateInstance(ctx, id, req)
}

func TestScrubUserData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		in := createNode(t, x.p, serverRequest(opA))
		tags := instanceOf(t, x.f, in.ID).Tags
		rec := &recordUpdates{Fake: x.f}
		before := len(x.f.Calls())

		if err := opProvider(rec).ScrubUserData(t.Context(), in); err != nil {
			t.Fatalf("ScrubUserData: %v", err)
		}

		wantCallsSince(t, x.f, before, vultrfake.Call{Name: "UpdateInstance", Arg: in.ID})
		// The user data, base64 as Create sends it, and nothing else. Tags stays nil, which govultr sends as null
		// and Vultr takes as "keep the tags".
		want := []govultr.InstanceUpdateReq{{UserData: base64.StdEncoding.EncodeToString([]byte(scrubbedUserData))}}
		if diff := cmp.Diff(want, rec.reqs); diff != "" {
			t.Errorf("update requests (-want +got):\n%s", diff)
		}
		got, err := base64.StdEncoding.DecodeString(x.f.UserData(in.ID))
		if err != nil || string(got) != scrubbedUserData {
			t.Errorf("the user data decodes to %q (%v), want %q", got, err, scrubbedUserData)
		}
		if diff := cmp.Diff(tags, instanceOf(t, x.f, in.ID).Tags); diff != "" {
			t.Errorf("tags (-before +after):\n%s", diff)
		}
	})
}

// nodeCalls are the calls of cloud.Nodes that act on one machine.
var nodeCalls = []struct {
	name   string // the method of cloud.Nodes
	call   func(cloud.Nodes, context.Context, cloud.Instance) error
	api    string // the vultr.API method it calls
	method string // the HTTP method of that call
	path   string // its path for the instance node
	want   string // the start of its error, before the client's
}{
	{"Stop", cloud.Nodes.Stop, "HaltInstance", http.MethodPost, "/v2/instances/node/halt",
		"stop node prod-servers-0 (node): "},
	{"Delete", cloud.Nodes.Delete, "DeleteInstance", http.MethodDelete, "/v2/instances/node",
		"delete node prod-servers-0 (node): "},
	{"ScrubUserData", cloud.Nodes.ScrubUserData, "UpdateInstance", http.MethodPatch, "/v2/instances/node",
		"scrub the user data of node prod-servers-0 (node): "},
}

// seededNode stores the node prod-servers-0 of cluster prod with the id node in f, and returns it as List does.
func seededNode(t *testing.T, f *vultrfake.Fake) cloud.Instance {
	t.Helper()
	f.AddInstance(t, govultr.Instance{ID: "node", Label: "prod-servers-0", Tags: []string{"tent/cluster=prod"}})
	nodes, err := opProvider(f).List(t.Context(), "prod")
	if err != nil || len(nodes) != 1 {
		t.Fatalf("List = %+v, %v; want the node", nodes, err)
	}
	return nodes[0]
}

func TestNodeCallsOfAGoneInstance(t *testing.T) {
	for _, nc := range nodeCalls {
		t.Run(nc.name, func(t *testing.T) {
			f := vultrfake.New()
			in := seededNode(t, f)
			if err := f.DeleteInstance(t.Context(), in.ID); err != nil {
				t.Fatalf("DeleteInstance: %v", err)
			}
			before := len(f.Calls())

			if err := nc.call(opProvider(f), t.Context(), in); err != nil {
				t.Errorf("%s of an instance that is gone: %v, want success", nc.name, err)
			}
			wantCallsSince(t, f, before, vultrfake.Call{Name: nc.api, Arg: in.ID})
		})
	}
}

func TestNodeCallsFail(t *testing.T) {
	for _, nc := range nodeCalls {
		t.Run(nc.name, func(t *testing.T) {
			f := vultrfake.New()
			in := seededNode(t, f)
			// No answer that the transport's retries could get: the caller may call again.
			f.Fail(t, nc.api, vultr.NewAPIError(nc.method, nc.path, http.StatusServiceUnavailable, "Try again later",
				0), 1)

			err := nc.call(opProvider(f), t.Context(), in)

			want := nc.want + "vultr: " + nc.method + " " + nc.path + ": 503 Service Unavailable: Try again later"
			if errText(err) != want {
				t.Errorf("%s = %v, want %q", nc.name, err, want)
			}
			if !errors.Is(err, vultr.ErrUnavailable) {
				t.Errorf("errors.Is(%v, vultr.ErrUnavailable) = false", err)
			}
		})
	}
}
