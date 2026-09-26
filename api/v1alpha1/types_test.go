package v1alpha1

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestClusterJSONRoundTrip(t *testing.T) {
	want := Cluster{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindCluster},
		Metadata: ClusterMeta{Name: "prod"},
		Spec: ClusterSpec{
			Channel: "stable",
			Cloud: Cloud{
				Provider: ProviderVultr,
				Region:   "ams",
				Zones:    []string{"ams"},
				Vultr:    &VultrCloud{},
			},
			Networking: Networking{CIDR: "10.64.0.0/16"},
			Access: Access{
				SSH: []string{"203.0.113.7/32"},
				API: []string{"0.0.0.0/0"},
			},
			SSHKeys: []string{testSSHKey},
			Nomad: ClusterNomad{
				Version:            "2.0.7",
				Region:             "global",
				TLS:                TLS{VerifyHTTPSClient: new(true)},
				ClientIntroduction: ClientIntroductionStrict,
			},
		},
	}
	checkRoundTrip(t, "cluster.json", want)
}

func TestHetznerClusterJSONRoundTrip(t *testing.T) {
	want := Cluster{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindCluster},
		Metadata: ClusterMeta{Name: "prod"},
		Spec: ClusterSpec{
			Cloud: Cloud{
				Provider: ProviderHetzner,
				Region:   "eu-central",
				Zones:    []string{"fsn1", "nbg1", "hel1"},
				Hetzner:  &HetznerCloud{},
			},
			Nomad: ClusterNomad{
				ExtraConfig: ExtraConfig{
					Server: "server {\n  heartbeat_grace = \"30s\"\n}\n",
					Client: "client {\n  gc_max_allocs = 100\n}\n",
				},
			},
		},
	}
	checkRoundTrip(t, "cluster-hetzner.json", want)
}

func TestNodeGroupJSONRoundTrip(t *testing.T) {
	want := NodeGroup{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindNodeGroup},
		Metadata: NodeGroupMeta{Name: "workers", Cluster: "prod"},
		Spec: NodeGroupSpec{
			Role:        RoleClient,
			MachineType: "vc2-2c-4gb",
			Image:       "ubuntu-24.04",
			Size:        3,
			Nomad: NodeGroupNomad{
				NodePool:  "default",
				NodeClass: "general",
				Drivers:   []string{"docker", "exec"},
				Meta:      map[string]string{"team": "platform"},
			},
		},
	}
	checkRoundTrip(t, "nodegroup-workers.json", want)
}

func TestServerNodeGroupJSONRoundTrip(t *testing.T) {
	want := NodeGroup{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindNodeGroup},
		Metadata: NodeGroupMeta{Name: "servers", Cluster: "prod"},
		Spec: NodeGroupSpec{
			Role:        RoleServer,
			MachineType: "vc2-2c-4gb",
			Image:       "ubuntu-24.04",
			Size:        3,
			Zones:       []string{"ams"},
		},
	}
	checkRoundTrip(t, "nodegroup-servers.json", want)
}

func TestZeroValuesAreOmitted(t *testing.T) {
	c := Cluster{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindCluster},
		Metadata: ClusterMeta{Name: "prod"},
		Spec: ClusterSpec{
			Cloud: Cloud{Provider: ProviderVultr, Region: "ams"},
		},
	}
	data := marshal(t, c)
	var doc struct {
		Spec map[string]json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	for _, key := range []string{"networking", "access", "nomad", "sshKeys", "channel"} {
		if v, ok := doc.Spec[key]; ok {
			t.Errorf("spec.%s = %s, want the key omitted", key, v)
		}
	}
}

func TestEmptyListsAndMapsSurviveARoundTrip(t *testing.T) {
	// An explicit empty list or map means something: a missing api list defaults to 0.0.0.0/0 and an empty one is
	// an error, and ssh: [] closes SSH. So encoding keeps an empty value and omits only nil.
	tests := []struct {
		kind  string
		path  string // the field's keys, joined by dots
		empty string // an object with the field set to an empty value
		field func(t *testing.T, data []byte) (obj, field any)
	}{
		{
			KindCluster, "spec.sshKeys", `{"spec":{"sshKeys":[]}}`,
			fieldOf(func(c *Cluster) any { return c.Spec.SSHKeys }),
		},
		{
			KindCluster, "spec.cloud.zones", `{"spec":{"cloud":{"zones":[]}}}`,
			fieldOf(func(c *Cluster) any { return c.Spec.Cloud.Zones }),
		},
		{
			KindCluster, "spec.access.ssh", `{"spec":{"access":{"ssh":[]}}}`,
			fieldOf(func(c *Cluster) any { return c.Spec.Access.SSH }),
		},
		{
			KindCluster, "spec.access.api", `{"spec":{"access":{"api":[]}}}`,
			fieldOf(func(c *Cluster) any { return c.Spec.Access.API }),
		},
		{
			KindNodeGroup, "spec.zones", `{"spec":{"zones":[]}}`,
			fieldOf(func(g *NodeGroup) any { return g.Spec.Zones }),
		},
		{
			KindNodeGroup, "spec.nomad.drivers", `{"spec":{"nomad":{"drivers":[]}}}`,
			fieldOf(func(g *NodeGroup) any { return g.Spec.Nomad.Drivers }),
		},
		{
			KindNodeGroup, "spec.nomad.meta", `{"spec":{"nomad":{"meta":{}}}}`,
			fieldOf(func(g *NodeGroup) any { return g.Spec.Nomad.Meta }),
		},
	}
	for _, tt := range tests {
		t.Run(tt.kind+" "+tt.path, func(t *testing.T) {
			obj, field := tt.field(t, []byte(tt.empty))
			if !isEmptyNonNil(field) {
				t.Fatalf("decoded %s = %#v, want an empty non-nil value", tt.path, field)
			}
			out := marshal(t, obj)
			if _, again := tt.field(t, out); !isEmptyNonNil(again) {
				t.Errorf("decoded %s = %#v from %s, want an empty non-nil value", tt.path, again, out)
			}
			zero, _ := tt.field(t, []byte(`{}`))
			if out := marshal(t, zero); hasKey(t, out, tt.path) {
				t.Errorf("encoded %s, want a nil %s omitted", out, tt.path)
			}
		})
	}
}

func TestFalseVerifyHTTPSClientIsKept(t *testing.T) {
	// A plain bool with omitempty would drop false, and the default would turn verification back on.
	off := decodeStrict[Cluster](t, []byte(`{"spec": {"nomad": {"tls": {"verifyHTTPSClient": false}}}}`))
	out := marshal(t, off)
	if !bytes.Contains(out, []byte(`"tls":{"verifyHTTPSClient":false}`)) {
		t.Errorf("encoded %s, want \"verifyHTTPSClient\":false kept", out)
	}
}

func TestEnumLists(t *testing.T) {
	if diff := cmp.Diff([]Role{"server", "client", "combined"}, Roles()); diff != "" {
		t.Errorf("Roles() mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]Provider{"vultr", "hetzner"}, Providers()); diff != "" {
		t.Errorf("Providers() mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]ClientIntroduction{"strict", "warn", "none"}, ClientIntroductions()); diff != "" {
		t.Errorf("ClientIntroductions() mismatch (-want +got):\n%s", diff)
	}
}

// checkRoundTrip decodes testdata/<file> strictly, compares it with want, and checks that encoding it gives the file
// back byte for byte. encoding/json matches keys case-insensitively, so only the bytes catch a misspelled tag such as
// "nodeclass".
func checkRoundTrip[T any](t *testing.T, file string, want T) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	got := decodeStrict[T](t, data)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("decoding %s (-want +got):\n%s", file, diff)
	}
	out, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if diff := cmp.Diff(string(data), string(out)+"\n"); diff != "" {
		t.Errorf("encoding differs from %s (-file +encoded):\n%s", file, diff)
	}
}

// fieldOf returns a function that decodes JSON into a T strictly and returns the T and the field get picks.
func fieldOf[T any](get func(*T) any) func(*testing.T, []byte) (obj, field any) {
	return func(t *testing.T, data []byte) (any, any) {
		t.Helper()
		v := decodeStrict[T](t, data)
		return v, get(&v)
	}
}

// isEmptyNonNil reports whether v is a slice or map that is empty but not nil.
func isEmptyNonNil(v any) bool {
	rv := reflect.ValueOf(v)
	return (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map) && !rv.IsNil() && rv.Len() == 0
}

// hasKey reports whether the JSON object data has a value at path, keys joined by dots.
func hasKey(t *testing.T, data []byte, path string) bool {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	for key := range strings.SplitSeq(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if v, ok = m[key]; !ok {
			return false
		}
	}
	return true
}

// marshal encodes v as JSON and fails the test on an error.
func marshal(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// decodeStrict decodes JSON into a T and fails the test on unknown fields.
func decodeStrict[T any](t *testing.T, data []byte) T {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var v T
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return v
}
