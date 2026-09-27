package spec

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite testdata/example.encoded.yaml")

// clusterDoc is a short Cluster document of eight lines.
const clusterDoc = `apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: prod
spec:
  cloud:
    provider: vultr
    region: ams
`

// groupDoc is a short NodeGroup document of nine lines that ends inside spec.
const groupDoc = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: workers
  cluster: prod
spec:
  role: client
  machineType: vc2-2c-4gb
  size: 3
`

// example is what testdata/example.yaml holds.
func example() Objects {
	return Objects{
		Cluster: &v1alpha1.Cluster{
			TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindCluster},
			Metadata: v1alpha1.ClusterMeta{Name: "prod"},
			Spec: v1alpha1.ClusterSpec{
				Channel: "stable",
				Cloud: v1alpha1.Cloud{
					Provider: v1alpha1.ProviderVultr,
					Region:   "ams",
					Zones:    []string{"ams"},
					Vultr:    &v1alpha1.VultrCloud{},
				},
				Networking: v1alpha1.Networking{CIDR: "10.64.0.0/16"},
				Access: v1alpha1.Access{
					SSH: []string{"203.0.113.7/32"},
					API: []string{"0.0.0.0/0"},
				},
				SSHKeys: []string{
					"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILVMgcq7nf63leSBwZNfB40Oi4XwSKWNKchNmRGNCb9k ops@example",
				},
				Nomad: v1alpha1.ClusterNomad{
					Version:            "2.0.7",
					Region:             "global",
					TLS:                v1alpha1.TLS{VerifyHTTPSClient: new(true)},
					ClientIntroduction: v1alpha1.ClientIntroductionStrict,
					ExtraConfig: v1alpha1.ExtraConfig{
						Server: "server {\n  heartbeat_grace = \"30s\"\n}\n",
						Client: "client {\n  gc_max_allocs = 100\n}\n",
					},
				},
			},
		},
		NodeGroups: []*v1alpha1.NodeGroup{
			{
				TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindNodeGroup},
				Metadata: v1alpha1.NodeGroupMeta{Name: "servers", Cluster: "prod"},
				Spec: v1alpha1.NodeGroupSpec{
					Role:        v1alpha1.RoleServer,
					MachineType: "vc2-2c-4gb",
					Image:       "ubuntu-24.04",
					Size:        3,
					Zones:       []string{"ams"},
				},
			},
			{
				TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindNodeGroup},
				Metadata: v1alpha1.NodeGroupMeta{Name: "workers", Cluster: "prod"},
				Spec: v1alpha1.NodeGroupSpec{
					Role:        v1alpha1.RoleClient,
					MachineType: "vc2-2c-4gb",
					Image:       "ubuntu-24.04",
					Size:        3,
					Zones:       []string{"ams"},
					Nomad: v1alpha1.NodeGroupNomad{
						NodePool:  "default",
						NodeClass: "general",
						Drivers:   []string{"docker", "exec"},
						Meta:      map[string]string{"team": "platform"},
					},
				},
			},
		},
	}
}

func TestDecodeTheExample(t *testing.T) {
	got := decode(t, readFile(t, "example.yaml"))
	if diff := cmp.Diff(example(), got); diff != "" {
		t.Errorf("Decode(example.yaml) (-want +got):\n%s", diff)
	}
}

func TestDecodeRejects(t *testing.T) {
	// Lines 1-8 are the cluster, line 9 is ---, lines 10-18 are the node group, and line 19 is the first one added.
	group := clusterDoc + "---\n" + groupDoc
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown field", group + "  sizee: 3\n", `document 2 (NodeGroup): line 19: unknown field "spec.sizee"`},
		{
			"every unknown field, by line",
			group + "  sizee: 3\n  foo: 1\n",
			`document 2 (NodeGroup): line 19: unknown field "spec.sizee"` + "\n" +
				`document 2 (NodeGroup): line 20: unknown field "spec.foo"`,
		},
		{
			"lower-case key",
			group + "  nomad:\n    nodeclass: general\n",
			`document 2 (NodeGroup): line 20: unknown field "spec.nomad.nodeclass"`,
		},
		{
			"upper-case key",
			clusterDoc + "---\n" + strings.Replace(groupDoc, "size:", "Size:", 1),
			`document 2 (NodeGroup): line 18: unknown field "spec.Size"`,
		},
		// Line 19 of the file: the line of the second key, not a line of some re-encoded copy.
		{"duplicate key", group + "  size: 5\n", `document 2: line 19: duplicate key "size"`},
		// 1 and 0x1 differ as text but are the same integer, and the same key in JSON.
		{
			"keys with one value",
			group + "  nomad:\n    meta: {1: x, 0x1: y}\n",
			`document 2: line 20: duplicate key "0x1"`,
		},
		{
			"true and True",
			group + "  nomad:\n    meta:\n      true: x\n      True: y\n",
			`document 2: line 22: duplicate key "True"`,
		},
		// The key *k stands for a, so the second key repeats the first.
		{
			"alias keys",
			group + "  nomad:\n    meta:\n      &k a: x\n      *k : y\n",
			`document 2: line 22: duplicate key "a"`,
		},
		{"null key", group + "  nomad:\n    meta: {~: x}\n", "document 2: line 20: a key must not be null"},
		// A null value is almost always a forgotten entry, and the JSON Schema rejects it too.
		{"no value", clusterDoc + "    vultr:\n", "document 1: line 9: spec.cloud.vultr: must not be null"},
		{"null value", clusterDoc + "  channel: null\n", "document 1: line 9: spec.channel: must not be null"},
		{
			"null map value",
			group + "  nomad:\n    meta: {team: ~}\n",
			"document 2: line 20: spec.nomad.meta.team: must not be null",
		},
		{
			"null list item",
			clusterDoc + "    zones: [~]\n",
			"document 1: line 9: spec.cloud.zones[0]: must not be null",
		},
		{
			"wrong type",
			clusterDoc + "---\n" + strings.Replace(groupDoc, "size: 3", "size: three", 1),
			"document 2 (NodeGroup): line 18: spec.size: must be a whole number, not a string",
		},
		{
			"fraction for a whole number",
			clusterDoc + "---\n" + strings.Replace(groupDoc, "size: 3", "size: 3.5", 1),
			"document 2 (NodeGroup): line 18: spec.size: must be a whole number, not 3.5",
		},
		{
			"yes for a bool",
			clusterDoc + "  nomad:\n    tls: {verifyHTTPSClient: yes}\n",
			"document 1 (Cluster): line 10: spec.nomad.tls.verifyHTTPSClient: must be true or false, not a string",
		},
		{
			"missing kind",
			clusterDoc + "---\n" + strings.Replace(groupDoc, "kind: NodeGroup\n", "", 1),
			"document 2: kind is required",
		},
		{
			"kind of the wrong type",
			clusterDoc + "---\n" + strings.Replace(groupDoc, "kind: NodeGroup", "kind: [NodeGroup]", 1),
			"document 2: line 11: kind: must be a string, not a list",
		},
		{
			"unknown kind",
			clusterDoc + "---\n" + strings.Replace(groupDoc, "kind: NodeGroup", "kind: Deployment", 1),
			`document 2: line 11: unknown kind "Deployment", want Cluster or NodeGroup`,
		},
		{
			"missing apiVersion",
			clusterDoc + "---\n" + strings.Replace(groupDoc, "apiVersion: tent/v1alpha1\n", "", 1),
			"document 2 (NodeGroup): apiVersion is required",
		},
		{
			"other API version",
			clusterDoc + "---\n" + strings.Replace(groupDoc, "tent/v1alpha1", "tent/v1beta1", 1),
			`document 2 (NodeGroup): line 10: unsupported apiVersion "tent/v1beta1", want tent/v1alpha1`,
		},
		{
			"second cluster",
			group + "---\n" + clusterDoc,
			"document 3 (Cluster): a second Cluster, the first is document 1",
		},
		{"not a mapping", clusterDoc + "---\nplain\n", "document 2: not a mapping"},
		{"list of other things", clusterDoc + "---\n- a\n- b\n", "document 2, item 1: not a mapping"},
		{"list of lists", "- [a]\n", "document 1, item 1: not a mapping"},
		{
			"unknown field in a list",
			"- " + indent(clusterDoc) + "- " + indent(groupDoc+"  sizee: 3\n"),
			`document 1, item 2 (NodeGroup): line 18: unknown field "spec.sizee"`,
		},
		{
			"duplicate key in a list",
			"- " + indent(clusterDoc+"  channel: stable\n  channel: beta\n"),
			`document 1, item 1: line 10: duplicate key "channel"`,
		},
		{
			"second cluster in a list",
			"- " + indent(clusterDoc) + "- " + indent(groupDoc) + "---\n- " + indent(clusterDoc),
			"document 2, item 1 (Cluster): a second Cluster, the first is document 1, item 1",
		},
		{
			"invalid YAML",
			clusterDoc + "---\nkind: [\n",
			"document 2: yaml: line 10: did not find expected node content",
		},
		{
			"empty documents count",
			clusterDoc + "---\n---\n" + groupDoc + "  sizee: 3\n",
			`document 3 (NodeGroup): line 20: unknown field "spec.sizee"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("Decode() error = nil, want %q", tt.want)
			}
			if got := err.Error(); got != tt.want {
				t.Errorf("Decode() error\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestDecodeKeepsEmptyMappings(t *testing.T) {
	o := decode(t, []byte(clusterDoc+"    vultr: {}\n"))
	if o.Cluster == nil || o.Cluster.Spec.Cloud.Vultr == nil {
		t.Errorf("Decode() = %+v, want spec.cloud.vultr set", o.Cluster)
	}
}

func TestDecodeKeepsAnyString(t *testing.T) {
	// Decode re-encodes each document for sigs.k8s.io/yaml, and the copy must hold every string the file holds.
	in := clusterDoc + `  sshKeys: ["  a\nb\n", "\na\n"]
  nomad:
    extraConfig:
      server: "\tserver {}\n"
`
	o := decode(t, []byte(in))
	if o.Cluster == nil {
		t.Fatalf("Decode() = %+v, want a cluster", o)
	}
	if diff := cmp.Diff([]string{"  a\nb\n", "\na\n"}, o.Cluster.Spec.SSHKeys); diff != "" {
		t.Errorf("spec.sshKeys (-want +got):\n%s", diff)
	}
	if got, want := o.Cluster.Spec.Nomad.ExtraConfig.Server, "\tserver {}\n"; got != want {
		t.Errorf("spec.nomad.extraConfig.server = %q, want %q", got, want)
	}
}

func TestDecodeUsesYAML12Scalars(t *testing.T) {
	// Plain yes, no, on and off are strings in YAML 1.2, which yaml.v3 follows, and bools in YAML 1.1.
	in := groupDoc + `  nomad:
    nodePool: off
    nodeClass: on
    meta: {enabled: yes, on: no}
`
	o := decode(t, []byte(in))
	if len(o.NodeGroups) != 1 {
		t.Fatalf("Decode() = %+v, want one node group", o)
	}
	want := v1alpha1.NodeGroupNomad{
		NodePool:  "off",
		NodeClass: "on",
		Meta:      map[string]string{"enabled": "yes", "on": "no"},
	}
	if diff := cmp.Diff(want, o.NodeGroups[0].Spec.Nomad); diff != "" {
		t.Fatalf("decoded spec.nomad (-want +got):\n%s", diff)
	}

	out := encode(t, o)
	// Quoted, so that YAML 1.1 readers get strings too.
	for _, line := range []string{`nodePool: "off"`, `nodeClass: "on"`, `enabled: "yes"`, `"on": "no"`} {
		if !strings.Contains(string(out), line) {
			t.Errorf("Encode() wrote\n%s\nwant a line %s", out, line)
		}
	}
	if diff := cmp.Diff(o, decode(t, out)); diff != "" {
		t.Errorf("Decode(Encode(o)) (-want +got):\n%s", diff)
	}
}

// indent turns a document into a list item that follows "- ".
func indent(doc string) string {
	return strings.ReplaceAll(strings.TrimSuffix(doc, "\n"), "\n", "\n  ") + "\n"
}

// TestDecodeLists reads a document that is a list of objects, as tent get -o json prints them, as their documents.
func TestDecodeLists(t *testing.T) {
	want := decode(t, []byte(clusterDoc+"---\n"+groupDoc))
	j := encodeJSONList(t, want.Cluster, want.NodeGroups[0])
	for name, in := range map[string]string{
		"JSON":           j,
		"YAML":           "- " + indent(clusterDoc) + "- " + indent(groupDoc),
		"list and a doc": "- " + indent(clusterDoc) + "---\n" + groupDoc,
		"empty list":     "[]\n---\n" + clusterDoc + "---\n" + groupDoc,
	} {
		t.Run(name, func(t *testing.T) {
			if diff := cmp.Diff(want, decode(t, []byte(in))); diff != "" {
				t.Errorf("Decode (-want +got):\n%s", diff)
			}
		})
	}
}

func encodeJSONList(t *testing.T, objs ...any) string {
	t.Helper()
	data, err := json.MarshalIndent(objs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func TestDecodeSkipsEmptyDocuments(t *testing.T) {
	in := "---\n" + clusterDoc + "---\n---\n" + groupDoc + "---\n"
	o := decode(t, []byte(in))
	if o.Cluster == nil || len(o.NodeGroups) != 1 {
		t.Errorf("Decode() = %+v, want the cluster and one node group", o)
	}
}

func TestDecodeNodeGroupsOnly(t *testing.T) {
	in := groupDoc + "---\n" + strings.Replace(groupDoc, "name: workers", "name: batch", 1)
	o := decode(t, []byte(in))
	if o.Cluster != nil {
		t.Errorf("Cluster = %+v, want nil", o.Cluster)
	}
	if got := names(o.NodeGroups); !slices.Equal(got, []string{"workers", "batch"}) {
		t.Errorf("node groups = %v, want [workers batch]", got)
	}
}

func TestEmptyAPIListSurvivesYAML(t *testing.T) {
	// A missing list defaults to 0.0.0.0/0 and an empty one is a validation error, so the two must stay apart.
	isEmptyList := func(api []string) bool { return api != nil && len(api) == 0 }
	o := decode(t, []byte(clusterDoc+"  access: {api: []}\n"))
	if o.Cluster == nil {
		t.Fatalf("Decode() = %+v, want a cluster", o)
	}
	if !isEmptyList(o.Cluster.Spec.Access.API) {
		t.Fatalf("decoded spec.access.api = %#v, want an empty non-nil list", o.Cluster.Spec.Access.API)
	}
	out := encode(t, o)
	if again := decode(t, out); again.Cluster == nil || !isEmptyList(again.Cluster.Spec.Access.API) {
		t.Errorf("Decode(Encode(o)) = %+v from\n%s\nwant spec.access.api an empty non-nil list", again.Cluster, out)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	in := example()
	slices.Reverse(in.NodeGroups)
	out := encode(t, in)

	if got := names(in.NodeGroups); !slices.Equal(got, []string{"workers", "servers"}) {
		t.Errorf("Encode() reordered its input to %v", got)
	}
	if !bytes.HasPrefix(out, []byte("apiVersion: tent/v1alpha1\nkind: Cluster\n")) {
		t.Errorf("Encode() wrote\n%s\nwant the Cluster first", out)
	}
	// Decode keeps the file order, so the groups come back sorted by name, as in example().
	if diff := cmp.Diff(example(), decode(t, out)); diff != "" {
		t.Errorf("Decode(Encode(example)) (-want +got):\n%s", diff)
	}
}

func TestEncodeNothing(t *testing.T) {
	if out := encode(t, Objects{}); len(out) != 0 {
		t.Errorf("Encode(Objects{}) = %q, want nothing", out)
	}
}

func TestEncodeRoundTripsAnyString(t *testing.T) {
	// yaml.v3 picks a literal block for some strings that it cannot read back, such as one that starts with a tab.
	for _, s := range []string{
		"\tserver {}\n", "\t\n", "  a\nb\n", "\na\n", "\n", " a\n", "a\n\tb\n", "a\nb", "a\n\n", "a \nb\n",
		"", " ", "yes", "1:20", "123", "- a", "# a", "a: b", `"a"`, "'a'",
		"<<", // yaml.v3 writes it plain, and reads it back as a merge key
		// JSON writes these raw, and YAML must escape them.
		"a\x7fb", "a\u0085b", "a\u0080b", "a\ufffeb",
	} {
		t.Run(fmt.Sprintf("%q", s), func(t *testing.T) {
			o := example()
			o.Cluster.Spec.SSHKeys = []string{s, s}
			o.Cluster.Spec.Nomad.ExtraConfig.Server = s
			o.NodeGroups[1].Spec.Nomad.Drivers = []string{s}
			o.NodeGroups[1].Spec.Nomad.Meta = map[string]string{s: s}
			out := encode(t, o)
			if diff := cmp.Diff(o, decode(t, out)); diff != "" {
				t.Errorf("Decode(Encode(o)) (-want +got) from\n%s\n%s", out, diff)
			}
		})
	}
}

func TestEncodeSkipsNilNodeGroups(t *testing.T) {
	o := example()
	o.NodeGroups = []*v1alpha1.NodeGroup{nil, o.NodeGroups[1], nil, o.NodeGroups[0]}
	if diff := cmp.Diff(example(), decode(t, encode(t, o))); diff != "" {
		t.Errorf("Decode(Encode(o)) (-want +got):\n%s", diff)
	}
}

func TestEncodeIsStable(t *testing.T) {
	// The M0 exit criterion: a spec survives create, get -o yaml and replace unchanged.
	data := readFile(t, "example.encoded.yaml")
	if diff := cmp.Diff(string(data), string(encode(t, decode(t, data)))); diff != "" {
		t.Errorf("Encode(Decode(example.encoded.yaml)) (-file +encoded):\n%s", diff)
	}
}

func TestEncodeKeepsEmptyListsAndMaps(t *testing.T) {
	// An explicit empty list or map means something (ssh: [] closes SSH), so it must survive get -o yaml and replace
	// as it is: written as [] or {} and read back as empty, not as left out.
	hetznerDoc := strings.Replace(clusterDoc, "provider: vultr\n    region: ams",
		"provider: hetzner\n    region: eu-central", 1)
	serverDoc := strings.Replace(groupDoc, "role: client", "role: server", 1)
	tests := []struct{ name, yaml string }{
		{"access.ssh", clusterDoc + "  access: {ssh: []}\n"},
		{"sshKeys", clusterDoc + "  sshKeys: []\n"},
		{"Hetzner cloud.zones", hetznerDoc + "    zones: []\n"},
		{"group zones", groupDoc + "  zones: []\n"},
		{"server group nomad", serverDoc + "  nomad: {drivers: [], meta: {}}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := decode(t, []byte(tt.yaml))
			out := encode(t, first)
			again := decode(t, out)
			if diff := cmp.Diff(first, again); diff != "" {
				t.Errorf("Decode(Encode(o)) (-want +got) from\n%s\n%s", out, diff)
			}
			if diff := cmp.Diff(string(out), string(encode(t, again))); diff != "" {
				t.Errorf("Encode(Decode(Encode(o))) differs from Encode(o) (-first +second):\n%s", diff)
			}
		})
	}
}

func TestEncodeGolden(t *testing.T) {
	const golden = "example.encoded.yaml"
	out := encode(t, example())
	if *update {
		if err := os.WriteFile(filepath.Join("testdata", golden), out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if diff := cmp.Diff(string(readFile(t, golden)), string(out)); diff != "" {
		t.Errorf("Encode(example) differs from %s (-file +encoded):\n%s", golden, diff)
	}
}

func decode(t *testing.T, data []byte) Objects {
	t.Helper()
	o, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode(%s): %v", data, err)
	}
	return o
}

func encode(t *testing.T, o Objects) []byte {
	t.Helper()
	out, err := Encode(o)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return out
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func names(groups []*v1alpha1.NodeGroup) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g.Metadata.Name)
	}
	return out
}
