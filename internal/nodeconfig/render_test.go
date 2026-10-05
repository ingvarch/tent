package nodeconfig_test

import (
	"fmt"
	"maps"
	"net/netip"
	"path"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/hcl"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/secrettest"
)

// hostileMeta is client metadata that HCL1 must read back as it is: quotes, backslashes, what looks like templates or
// directives, dollars without an interpolation, Unicode, an empty value and keys that are not HCL1 identifiers.
var hostileMeta = map[string]string{
	"team":      "platform",
	"quote":     `say "hi"`,
	"backslash": `C:\temp\`,
	"escape":    `\"`,
	"template":  `{{ env "HOME" }}`,
	"directive": "%{ if true }x%{ endif }",
	"dollar":    "$HOME costs $5 {and} $ {more}",
	"unicode":   "Zürich 東京 Ω e\u0301",
	"empty":     "",
	"rack.id":   "r1",
	"1st":       "yes",
}

// agents returns the agent of each role, as the goldens show them.
func agents() map[v1alpha1.Role]nodeconfig.Agent {
	base := nodeconfig.Agent{
		Cluster:            "prod",
		Region:             "global",
		CIDR:               netip.MustParsePrefix("10.64.0.0/16"),
		ClientIntroduction: v1alpha1.ClientIntroductionStrict,
		VerifyHTTPSClient:  true,
		DynamicPorts:       nodeconfig.PortRange{First: 20000, Last: 32000},
		Gossip:             secret.Secret(gossipKey),
		ExtraServer:        "server {\n  raft_multiplier = 2\n}\n",
		ExtraClient:        "client {\n  max_kill_timeout = \"1m\"\n}",
	}
	server, client, combined := base, base, base
	server.Role, server.Group = v1alpha1.RoleServer, "servers"
	client.Role, client.Group = v1alpha1.RoleClient, "workers"
	client.NodePool, client.NodeClass, client.Drivers = "batch", "general", []string{"docker", "exec"}
	client.Meta = hostileMeta
	combined.Role, combined.Group = v1alpha1.RoleCombined, "core"
	combined.ClientIntroduction, combined.VerifyHTTPSClient = v1alpha1.ClientIntroductionWarn, false
	combined.NodePool, combined.Meta = "default", map[string]string{"quote": `say "hi"`, "unicode": "東京"}
	return map[v1alpha1.Role]nodeconfig.Agent{
		v1alpha1.RoleServer: server, v1alpha1.RoleClient: client, v1alpha1.RoleCombined: combined,
	}
}

// render returns the files of the agent of role, and fails the test on an error.
func render(t *testing.T, role v1alpha1.Role) []nodeconfig.File {
	t.Helper()
	files, err := nodeconfig.RenderAgent(agents()[role])
	if err != nil {
		t.Fatalf("RenderAgent(%s): %v", role, err)
	}
	return files
}

// TestRenderAgentGolden checks every file of each role against its golden, testdata/<role>_<file>.golden.
func TestRenderAgentGolden(t *testing.T) {
	for _, role := range v1alpha1.Roles() {
		t.Run(string(role), func(t *testing.T) {
			for _, f := range render(t, role) {
				checkGolden(t, string(role)+"_"+path.Base(f.Path)+".golden", string(f.Content))
			}
		})
	}
}

// TestRenderAgentFiles checks the files of each role: their paths in the order Nomad merges them, their modes and
// owners, and that only the gossip key is secret. A combined agent gets the files of both roles.
func TestRenderAgentFiles(t *testing.T) {
	const (
		tent   = "/etc/nomad.d/00-tent.hcl 0644 root:root"
		gossip = "/etc/nomad.d/01-gossip.hcl 0600 root:root secret"
		server = "/etc/nomad.d/98-user-server.hcl 0600 root:root"
		client = "/etc/nomad.d/99-user-client.hcl 0600 root:root"
	)
	for role, want := range map[v1alpha1.Role][]string{
		v1alpha1.RoleServer:   {tent, gossip, server},
		v1alpha1.RoleClient:   {tent, client},
		v1alpha1.RoleCombined: {tent, gossip, server, client},
	} {
		var got []string
		for _, f := range render(t, role) {
			line := fmt.Sprintf("%s %04o %s", f.Path, f.Mode, f.Owner)
			if f.Secret {
				line += " secret"
			}
			if f.PerNode {
				line += " per-node"
			}
			got = append(got, line)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("RenderAgent(%s) files differ (-want +got):\n%s", role, diff)
		}
	}
}

// TestRenderAgentWithoutExtras checks that an agent without extraConfig gets no user files, and that a client without
// a node class or drivers leaves them to Nomad.
func TestRenderAgentWithoutExtras(t *testing.T) {
	a := agents()[v1alpha1.RoleClient]
	a.ExtraServer, a.ExtraClient, a.NodeClass, a.Drivers = "", "", "", nil
	files, err := nodeconfig.RenderAgent(a)
	if err != nil {
		t.Fatalf("RenderAgent: %v", err)
	}
	if len(files) != 1 || files[0].Path != "/etc/nomad.d/00-tent.hcl" {
		t.Fatalf("RenderAgent gave %v, want 00-tent.hcl alone", files)
	}
	for _, s := range []string{"node_class", "driver.allowlist"} {
		if strings.Contains(string(files[0].Content), s) {
			t.Errorf("00-tent.hcl sets %s, which the agent leaves out", s)
		}
	}
}

// nomadConfig is the part of Nomad's agent configuration that the tests read back with HCL1, as Nomad reads it.
type nomadConfig struct {
	Region             string `hcl:"region"`
	LeaveOnTerminate   bool   `hcl:"leave_on_terminate"`
	DisableUpdateCheck bool   `hcl:"disable_update_check"`
	Server             *struct {
		Enabled            bool   `hcl:"enabled"`
		Encrypt            string `hcl:"encrypt"`
		RaftMultiplier     int    `hcl:"raft_multiplier"`
		ClientIntroduction *struct {
			Enforcement string `hcl:"enforcement"`
		} `hcl:"client_introduction"`
	} `hcl:"server"`
	Client *struct {
		Enabled         bool              `hcl:"enabled"`
		NodePool        string            `hcl:"node_pool"`
		NodeClass       string            `hcl:"node_class"`
		MinDynamicPort  int               `hcl:"min_dynamic_port"`
		MaxDynamicPort  int               `hcl:"max_dynamic_port"`
		MaxKillTimeout  string            `hcl:"max_kill_timeout"`
		Options         map[string]string `hcl:"options"`
		Meta            map[string]string `hcl:"meta"`
		DrainOnShutdown *struct {
			Deadline string `hcl:"deadline"`
		} `hcl:"drain_on_shutdown"`
	} `hcl:"client"`
	ACL *struct {
		Enabled bool `hcl:"enabled"`
	} `hcl:"acl"`
	TLS *struct {
		VerifyHTTPSClient bool `hcl:"verify_https_client"`
	} `hcl:"tls"`
	Consul *struct {
		ServerAutoJoin bool `hcl:"server_auto_join"`
		ClientAutoJoin bool `hcl:"client_auto_join"`
	} `hcl:"consul"`
}

// TestRenderAgentReadsBack reads each file with HCL1, as Nomad does, and checks the values that come from the agent:
// hostile metadata comes back as it was given.
func TestRenderAgentReadsBack(t *testing.T) {
	for _, role := range v1alpha1.Roles() {
		t.Run(string(role), func(t *testing.T) {
			a := agents()[role]
			var got nomadConfig
			for _, f := range render(t, role) {
				var one nomadConfig
				if err := hcl.Decode(&one, string(f.Content)); err != nil {
					t.Fatalf("HCL1 cannot read %s: %v", f.Path, err)
				}
				merge(&got, one)
			}
			if got.Region != a.Region {
				t.Errorf("region = %q, want %q", got.Region, a.Region)
			}
			if !got.LeaveOnTerminate {
				t.Error("leave_on_terminate is not set")
			}
			if !got.DisableUpdateCheck {
				t.Error("the update check is not disabled")
			}
			if got.Consul == nil || got.Consul.ServerAutoJoin || got.Consul.ClientAutoJoin {
				t.Errorf("consul = %+v, want both auto-joins off", got.Consul)
			}
			if got.ACL == nil || !got.ACL.Enabled {
				t.Error("ACLs are not enabled")
			}
			if got.TLS == nil || got.TLS.VerifyHTTPSClient != a.VerifyHTTPSClient {
				t.Errorf("tls = %+v, want verify_https_client = %t", got.TLS, a.VerifyHTTPSClient)
			}
			checkServer(t, a, got)
			checkClient(t, a, got)
		})
	}
}

// merge adds the blocks that one file sets to got, as Nomad merges its files.
func merge(got *nomadConfig, one nomadConfig) {
	if one.Region != "" {
		got.Region = one.Region
	}
	if one.LeaveOnTerminate {
		got.LeaveOnTerminate = true
	}
	if one.DisableUpdateCheck {
		got.DisableUpdateCheck = true
	}
	if one.ACL != nil {
		got.ACL = one.ACL
	}
	if one.TLS != nil {
		got.TLS = one.TLS
	}
	if one.Consul != nil {
		got.Consul = one.Consul
	}
	switch {
	case one.Server == nil:
	case got.Server == nil:
		got.Server = one.Server
	case one.Server.Encrypt != "":
		got.Server.Encrypt = one.Server.Encrypt
	default:
		got.Server.RaftMultiplier = one.Server.RaftMultiplier
	}
	switch {
	case one.Client == nil:
	case got.Client == nil:
		got.Client = one.Client
	default:
		got.Client.MaxKillTimeout = one.Client.MaxKillTimeout
	}
}

func checkServer(t *testing.T, a nodeconfig.Agent, got nomadConfig) {
	t.Helper()
	if a.Role == v1alpha1.RoleClient {
		if got.Server != nil {
			t.Errorf("a client has a server block: %+v", got.Server)
		}
		return
	}
	s := got.Server
	switch {
	case s == nil || !s.Enabled:
		t.Fatalf("server = %+v, want enabled", s)
	case s.Encrypt != string(gossipKey):
		t.Error("server.encrypt is not the gossip key")
	case s.ClientIntroduction == nil || s.ClientIntroduction.Enforcement != string(a.ClientIntroduction):
		t.Errorf("server.client_introduction = %+v, want enforcement %q", s.ClientIntroduction, a.ClientIntroduction)
	case s.RaftMultiplier != 2:
		t.Errorf("server.raft_multiplier = %d, want 2 from the extra server configuration", s.RaftMultiplier)
	}
}

func checkClient(t *testing.T, a nodeconfig.Agent, got nomadConfig) {
	t.Helper()
	if a.Role == v1alpha1.RoleServer {
		if got.Client != nil {
			t.Errorf("a server has a client block: %+v", got.Client)
		}
		return
	}
	c := got.Client
	if c == nil || !c.Enabled {
		t.Fatalf("client = %+v, want enabled", c)
	}
	if c.NodePool != a.NodePool || c.NodeClass != a.NodeClass {
		t.Errorf("client node_pool, node_class = %q, %q; want %q, %q", c.NodePool, c.NodeClass, a.NodePool, a.NodeClass)
	}
	if c.MinDynamicPort != 20000 || c.MaxDynamicPort != 32000 {
		t.Errorf("client dynamic ports = %d-%d, want 20000-32000", c.MinDynamicPort, c.MaxDynamicPort)
	}
	if c.MaxKillTimeout != "1m" {
		t.Errorf("client.max_kill_timeout = %q, want 1m from the extra client configuration", c.MaxKillTimeout)
	}
	// A drain on shutdown leaves the node ineligible after every stop of Nomad.
	if c.DrainOnShutdown != nil {
		t.Errorf("client.drain_on_shutdown = %+v, want none", *c.DrainOnShutdown)
	}
	wantMeta := maps.Clone(a.Meta)
	wantMeta["tent_cluster"], wantMeta["tent_nodegroup"] = a.Cluster, a.Group
	if diff := cmp.Diff(wantMeta, c.Meta); diff != "" {
		t.Errorf("client.meta differs (-want +got):\n%s", diff)
	}
	wantOptions := map[string]string{"fingerprint.denylist": "env_aws,env_gce,env_azure,env_digitalocean"}
	if len(a.Drivers) > 0 {
		wantOptions["driver.allowlist"] = strings.Join(a.Drivers, ",")
	}
	if diff := cmp.Diff(wantOptions, c.Options); diff != "" {
		t.Errorf("client.options differ (-want +got):\n%s", diff)
	}
}

func TestRenderAgentErrors(t *testing.T) {
	type agent = nodeconfig.Agent
	for _, tc := range []struct {
		name string
		role v1alpha1.Role
		edit func(a *agent)
		want string
	}{
		{"role", v1alpha1.RoleServer, func(a *agent) { a.Role = "worker" },
			`nomad agent: role "worker" is not server, client or combined`},
		{"no cluster", v1alpha1.RoleServer, func(a *agent) { a.Cluster = "" }, "nomad agent: no cluster"},
		{"no group", v1alpha1.RoleClient, func(a *agent) { a.Group = "" }, "nomad agent: no node group"},
		{"cluster", v1alpha1.RoleClient, func(a *agent) { a.Cluster = "a\nb" },
			"nomad agent: cluster has the control character U+000A"},
		{"no region", v1alpha1.RoleServer, func(a *agent) { a.Region = "" }, "nomad agent: no region"},
		{"region", v1alpha1.RoleServer, func(a *agent) { a.Region = "${var.region}" },
			"nomad agent: region has ${, which HCL1 reads as the start of an interpolation"},
		{"no CIDR", v1alpha1.RoleServer, func(a *agent) { a.CIDR = netip.Prefix{} },
			"nomad agent: CIDR is not a prefix"},
		{"CIDR with host bits", v1alpha1.RoleServer, func(a *agent) { a.CIDR = netip.MustParsePrefix("10.64.0.1/16") },
			"nomad agent: CIDR 10.64.0.1/16 has bits set after /16"},
		{"client introduction", v1alpha1.RoleServer, func(a *agent) { a.ClientIntroduction = "loose" },
			`nomad agent: client introduction "loose" is not strict, warn or none`},
		{"no gossip key", v1alpha1.RoleCombined, func(a *agent) { a.Gossip = nil }, "nomad agent: no gossip key"},
		{"gossip key", v1alpha1.RoleServer, func(a *agent) { a.Gossip = secret.Secret("abc\ndef") },
			"nomad agent: gossip key has the control character U+000A"},
		{"node pool", v1alpha1.RoleClient, func(a *agent) { a.NodePool = "${pool}" },
			"nomad agent: node pool has ${, which HCL1 reads as the start of an interpolation"},
		{"node class", v1alpha1.RoleCombined, func(a *agent) { a.NodeClass = "gpu\x00" },
			"nomad agent: node class has the control character U+0000"},
		{"empty driver", v1alpha1.RoleClient, func(a *agent) { a.Drivers = []string{"docker", ""} },
			`nomad agent: driver "" is empty or has a comma or white space`},
		{"driver with a comma", v1alpha1.RoleClient, func(a *agent) { a.Drivers = []string{"docker,raw_exec"} },
			`nomad agent: driver "docker,raw_exec" is empty or has a comma or white space`},
		{"driver with a space", v1alpha1.RoleClient, func(a *agent) { a.Drivers = []string{"raw exec"} },
			`nomad agent: driver "raw exec" is empty or has a comma or white space`},
		{"driver", v1alpha1.RoleClient, func(a *agent) { a.Drivers = []string{"exec\ue123"} },
			"nomad agent: driver \"exec\\ue123\" has U+E123, which HCL1 reserves"},
		{"empty meta key", v1alpha1.RoleClient, func(a *agent) { a.Meta = map[string]string{"": "x"} },
			"nomad agent: meta has an empty key"},
		{"tent meta key", v1alpha1.RoleCombined, func(a *agent) { a.Meta = map[string]string{"tent_cluster": "x"} },
			`nomad agent: meta key "tent_cluster" starts with tent_, which tent keeps for its own keys`},
		{"meta key", v1alpha1.RoleClient, func(a *agent) { a.Meta = map[string]string{"a\tb": "x"} },
			`nomad agent: meta key "a\tb" has the control character U+0009`},
		{"meta value", v1alpha1.RoleClient, func(a *agent) { a.Meta = map[string]string{"id": "${node.unique.id}"} },
			`nomad agent: meta "id": the value has ${, which HCL1 reads as the start of an interpolation`},
		{"meta value with a line end", v1alpha1.RoleClient,
			func(a *agent) { a.Meta = map[string]string{"a": "ok", "b": "two\nlines"} },
			`nomad agent: meta "b": the value has the control character U+000A`},
		{"no dynamic ports", v1alpha1.RoleClient, func(a *agent) { a.DynamicPorts = nodeconfig.PortRange{} },
			"nomad agent: dynamic ports 0-0 are not a range of ports from 1 to 65535"},
		{"reversed dynamic ports", v1alpha1.RoleCombined,
			func(a *agent) { a.DynamicPorts = nodeconfig.PortRange{First: 32000, Last: 20000} },
			"nomad agent: dynamic ports 32000-20000 are not a range of ports from 1 to 65535"},
		{"extra server configuration", v1alpha1.RoleServer, func(a *agent) { a.ExtraServer = "server {\xff}" },
			"nomad agent: spec.nomad.extraConfig.server is not valid UTF-8"},
		{"extra client configuration", v1alpha1.RoleCombined, func(a *agent) { a.ExtraClient = "\xff" },
			"nomad agent: spec.nomad.extraConfig.client is not valid UTF-8"},

		// The first problem in the order of the fields, whatever kind it is.
		{"region before the gossip key", v1alpha1.RoleServer, func(a *agent) { a.Region, a.Gossip = "${r}", nil },
			"nomad agent: region has ${, which HCL1 reads as the start of an interpolation"},
		{"cluster before the client introduction", v1alpha1.RoleCombined,
			func(a *agent) { a.Cluster, a.ClientIntroduction = "a\nb", "loose" },
			"nomad agent: cluster has the control character U+000A"},
		{"node pool before the drivers", v1alpha1.RoleClient,
			func(a *agent) { a.NodePool, a.Drivers = "${p}", []string{""} },
			"nomad agent: node pool has ${, which HCL1 reads as the start of an interpolation"},
		{"meta before the dynamic ports", v1alpha1.RoleClient,
			func(a *agent) { a.Meta, a.DynamicPorts = map[string]string{"x": "${y}"}, nodeconfig.PortRange{} },
			`nomad agent: meta "x": the value has ${, which HCL1 reads as the start of an interpolation`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := agents()[tc.role]
			tc.edit(&a)
			files, err := nodeconfig.RenderAgent(a)
			if got := errText(err); got != tc.want {
				t.Errorf("RenderAgent() error = %q, want %q", got, tc.want)
			}
			if files != nil {
				t.Errorf("RenderAgent() = %v, want no files with the error", files)
			}
			if secrettest.Shows(errText(err), a.Gossip) {
				t.Error("the error shows the gossip key")
			}
		})
	}
}

// TestRenderAgentIgnoresOtherRoles checks that a server ignores the client settings and a client the server ones:
// the app may give every group the cluster's gossip key.
func TestRenderAgentIgnoresOtherRoles(t *testing.T) {
	server := agents()[v1alpha1.RoleServer]
	server.Meta, server.Drivers = map[string]string{"x": "${y}"}, []string{","}
	server.DynamicPorts, server.ExtraClient = nodeconfig.PortRange{}, "\xff"
	if _, err := nodeconfig.RenderAgent(server); err != nil {
		t.Errorf("RenderAgent(server with bad client settings) = %v, want no error", err)
	}
	client := agents()[v1alpha1.RoleClient]
	client.Gossip, client.ClientIntroduction = secret.Secret("a\nb"), "loose"
	files, err := nodeconfig.RenderAgent(client)
	if err != nil {
		t.Fatalf("RenderAgent(client with bad server settings) = %v, want no error", err)
	}
	for _, f := range files {
		if secrettest.Shows(string(f.Content), client.Gossip) {
			t.Errorf("%s of a client shows the gossip key", f.Path)
		}
	}
}

// TestAgentNeverPrintsGossip checks that no way of printing or logging an agent shows its gossip key.
func TestAgentNeverPrintsGossip(t *testing.T) {
	secrets := map[string][]byte{"the gossip key": gossipKey}
	secrettest.CheckHidden(t, secrettest.Printed(t, agents()[v1alpha1.RoleServer]), secrets, "[secret, 44 bytes]")
}
