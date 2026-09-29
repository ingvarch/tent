package nodeconfig_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/hcl"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
)

// nodeNames are the names of the nodes that the per-node goldens show, by role.
var nodeNames = map[v1alpha1.Role]string{
	v1alpha1.RoleServer:   "prod-servers-0",
	v1alpha1.RoleClient:   "prod-workers-3",
	v1alpha1.RoleCombined: "prod-core-0",
}

// renderNode returns 10-node.hcl of the node of role in the datacenter ams, with bootstrap_expect 3, and fails the
// test on an error.
func renderNode(t *testing.T, role v1alpha1.Role) nodeconfig.File {
	t.Helper()
	f, err := nodeconfig.RenderNode(nodeNames[role], "ams", role, 3)
	if err != nil {
		t.Fatalf("RenderNode(%s): %v", role, err)
	}
	return f
}

func TestRenderNodeGolden(t *testing.T) {
	for _, role := range v1alpha1.Roles() {
		t.Run(string(role), func(t *testing.T) {
			checkGolden(t, string(role)+"_10-node.hcl.golden", string(renderNode(t, role).Content))
		})
	}
}

// TestPerNodeFiles checks the files that only one node has: 10-node.hcl, 05-join.hcl and 11-instance.hcl are
// per-node files that every local user may read, as none holds a secret.
func TestPerNodeFiles(t *testing.T) {
	files := map[string]nodeconfig.File{}
	for _, role := range v1alpha1.Roles() {
		files["RenderNode("+string(role)+")"] = renderNode(t, role)
		join, err := nodeconfig.RenderJoin(role, seeds)
		if err != nil {
			t.Fatalf("RenderJoin(%s): %v", role, err)
		}
		files["RenderJoin("+string(role)+")"] = join
	}
	instance, err := nodeconfig.RenderInstance("cb676a46-66fd-4dfb-b839-443f2e6c0b60")
	if err != nil {
		t.Fatalf("RenderInstance: %v", err)
	}
	files["RenderInstance"] = instance
	for name, f := range files {
		got := fmt.Sprintf("%s %04o %s per-node=%t secret=%t", f.Path, f.Mode, f.Owner, f.PerNode, f.Secret)
		want := "/etc/nomad.d/10-node.hcl 0644 root:root per-node=true secret=false"
		switch {
		case strings.HasPrefix(name, "RenderJoin"):
			want = "/etc/nomad.d/05-join.hcl 0644 root:root per-node=true secret=false"
		case name == "RenderInstance":
			want = "/etc/nomad.d/11-instance.hcl 0644 root:root per-node=true secret=false"
		}
		if got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
}

// nodeFile is what the tests read back from 10-node.hcl and 11-instance.hcl with HCL1, as Nomad reads them.
type nodeFile struct {
	Name       string `hcl:"name"`
	Datacenter string `hcl:"datacenter"`
	Server     *struct {
		BootstrapExpect int `hcl:"bootstrap_expect"`
	} `hcl:"server"`
	Client *struct {
		Meta map[string]string `hcl:"meta"`
	} `hcl:"client"`
}

// decodeNode reads content with HCL1 and fails the test when it cannot.
func decodeNode(t *testing.T, content []byte) nodeFile {
	t.Helper()
	var got nodeFile
	if err := hcl.Decode(&got, string(content)); err != nil {
		t.Fatalf("HCL1 cannot read the file: %v", err)
	}
	return got
}

// TestRenderNodeReadsBack reads 10-node.hcl back with HCL1: the name and the datacenter as given, and
// bootstrap_expect in a server block on server and combined nodes only.
func TestRenderNodeReadsBack(t *testing.T) {
	for _, tc := range []struct {
		name, datacenter string
		role             v1alpha1.Role
		expect           int
	}{
		{"prod-servers-0", "ams", v1alpha1.RoleServer, 3},
		{"prod-core-2", "fra", v1alpha1.RoleCombined, 1},
		{"prod-workers-17", `dc "one" \ Zürich`, v1alpha1.RoleClient, 0},
	} {
		f, err := nodeconfig.RenderNode(tc.name, tc.datacenter, tc.role, tc.expect)
		if err != nil {
			t.Fatalf("RenderNode(%s): %v", tc.name, err)
		}
		got := decodeNode(t, f.Content)
		if got.Name != tc.name || got.Datacenter != tc.datacenter {
			t.Errorf("RenderNode(%s): name, datacenter = %q, %q; want %q, %q",
				tc.name, got.Name, got.Datacenter, tc.name, tc.datacenter)
		}
		switch {
		case !tc.role.RunsServer() && got.Server != nil:
			t.Errorf("RenderNode(%s): a client has a server block: %+v", tc.name, got.Server)
		case tc.role.RunsServer() && (got.Server == nil || got.Server.BootstrapExpect != tc.expect):
			t.Errorf("RenderNode(%s): server = %+v, want bootstrap_expect = %d", tc.name, got.Server, tc.expect)
		}
		if got.Client != nil {
			t.Errorf("RenderNode(%s): 10-node.hcl has a client block: %+v", tc.name, got.Client)
		}
	}
}

// TestRenderNodeIgnoresBootstrapExpectOnClients checks that a client ignores bootstrap_expect, as the app may give
// every node the number of servers.
func TestRenderNodeIgnoresBootstrapExpectOnClients(t *testing.T) {
	want := renderNode(t, v1alpha1.RoleClient)
	for _, expect := range []int{0, -1, 5} {
		got, err := nodeconfig.RenderNode(nodeNames[v1alpha1.RoleClient], "ams", v1alpha1.RoleClient, expect)
		if err != nil || string(got.Content) != string(want.Content) {
			t.Errorf("RenderNode(client, bootstrap_expect %d) = %s, %v; want the file without it", expect, got, err)
		}
	}
}

func TestRenderNodeErrors(t *testing.T) {
	for _, tc := range []struct {
		name, node, datacenter string
		role                   v1alpha1.Role
		expect                 int
		want                   string
	}{
		{"role", "prod-a-0", "ams", "worker", 3, `10-node.hcl: role "worker" is not server, client or combined`},
		{"no name", "", "ams", v1alpha1.RoleServer, 3, `10-node.hcl: name "" is not a host name: ` +
			`1 to 63 lower-case letters, digits and dashes, starting and ending with a letter or digit`},
		{"name", "Prod_0", "ams", v1alpha1.RoleClient, 0, `10-node.hcl: name "Prod_0" is not a host name: ` +
			`1 to 63 lower-case letters, digits and dashes, starting and ending with a letter or digit`},
		{"no datacenter", "prod-a-0", "", v1alpha1.RoleClient, 0, "10-node.hcl: no datacenter"},
		{"datacenter with a star", "prod-a-0", "ams*", v1alpha1.RoleClient, 0,
			`10-node.hcl: datacenter "ams*" has "*", which Nomad refuses`},
		{"datacenter with a line end", "prod-a-0", "ams\n", v1alpha1.RoleServer, 3,
			"10-node.hcl: datacenter has the control character U+000A"},
		{"datacenter with an interpolation", "prod-a-0", "${dc}", v1alpha1.RoleCombined, 3,
			"10-node.hcl: datacenter has ${, which HCL1 reads as the start of an interpolation"},
		{"no bootstrap_expect", "prod-a-0", "ams", v1alpha1.RoleServer, 0,
			"10-node.hcl: bootstrap_expect 0 is less than 1"},
		{"negative bootstrap_expect", "prod-a-0", "ams", v1alpha1.RoleCombined, -3,
			"10-node.hcl: bootstrap_expect -3 is less than 1"},
		{"the first problem", "prod-a-0", "", "worker", 0, "10-node.hcl: no datacenter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := nodeconfig.RenderNode(tc.node, tc.datacenter, tc.role, tc.expect)
			if got := errText(err); got != tc.want {
				t.Errorf("RenderNode() error = %q, want %q", got, tc.want)
			}
			if f.Path != "" || f.Content != nil {
				t.Errorf("RenderNode() = %v, want no file with the error", f)
			}
		})
	}
}

func TestRenderInstanceGolden(t *testing.T) {
	got, err := nodeconfig.RenderInstance("cb676a46-66fd-4dfb-b839-443f2e6c0b60")
	if err != nil {
		t.Fatalf("RenderInstance: %v", err)
	}
	checkGolden(t, "11-instance.hcl.golden", string(got.Content))
}

// TestRenderInstanceReadsBack reads 11-instance.hcl back with HCL1: the instance id comes back as it was given, as
// the client's tent_instance_id meta, and nothing else is set.
func TestRenderInstanceReadsBack(t *testing.T) {
	for _, id := range []string{"cb676a46-66fd-4dfb-b839-443f2e6c0b60", "108042816", `i-"quoted"\`, "{{ id }} $x %{y}"} {
		f, err := nodeconfig.RenderInstance(id)
		if err != nil {
			t.Fatalf("RenderInstance(%q): %v", id, err)
		}
		got := decodeNode(t, f.Content)
		want := map[string]string{"tent_instance_id": id}
		if got.Client == nil || fmt.Sprint(got.Client.Meta) != fmt.Sprint(want) {
			t.Errorf("RenderInstance(%q): client = %+v, want meta %v", id, got.Client, want)
		}
		if got.Name != "" || got.Datacenter != "" || got.Server != nil {
			t.Errorf("RenderInstance(%q) sets more than the client meta: %+v", id, got)
		}
	}
}

func TestRenderInstanceErrors(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"", "11-instance.hcl: no instance id"},
		{"id\n", "11-instance.hcl: instance id has the control character U+000A"},
		{"${id}", "11-instance.hcl: instance id has ${, which HCL1 reads as the start of an interpolation"},
		{"\xff", "11-instance.hcl: instance id is not valid UTF-8"},
	} {
		f, err := nodeconfig.RenderInstance(tc.id)
		if got := errText(err); got != tc.want {
			t.Errorf("RenderInstance(%q) error = %q, want %q", tc.id, got, tc.want)
		}
		if f.Path != "" || f.Content != nil {
			t.Errorf("RenderInstance(%q) = %v, want no file with the error", tc.id, f)
		}
	}
}
