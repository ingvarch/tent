package vultr_test

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
)

const opID = "5f0c2a9e-8d1b-4c7e-9f3a-2b6d8e1c4a70"

// allLabels has every canonical label, each with a value of the form tent writes.
func allLabels() cloud.Labels {
	return cloud.Labels{
		cloud.LabelCluster:   "prod",
		cloud.LabelNodeGroup: "workers",
		cloud.LabelRole:      "client",
		cloud.LabelSpecHash:  "3f9a1c0b7d2e4f68",
		cloud.LabelSlot:      "2",
		cloud.LabelOp:        opID,
		cloud.LabelLockFor:   "prod",
		cloud.LabelE2E:       "true",
		cloud.LabelJoined:    "true",
		cloud.LabelE2ERun:    "run-7k2m9x",
	}
}

// manyLabels returns n labels tent/k00, tent/k01, ...
func manyLabels(n int) cloud.Labels {
	l := cloud.Labels{}
	for i := range n {
		l[fmt.Sprintf("tent/k%02d", i)] = "v"
	}
	return l
}

// errText returns the error's message, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestEncodeTags(t *testing.T) {
	got, err := vultr.EncodeTags(allLabels())
	if err != nil {
		t.Fatalf("EncodeTags: %v", err)
	}
	want := []string{
		"tent/cluster=prod",
		"tent/e2e-run=run-7k2m9x",
		"tent/e2e=true",
		"tent/joined=true",
		"tent/lock-for=prod",
		"tent/nodegroup=workers",
		"tent/op=" + opID,
		"tent/role=client",
		"tent/slot=2",
		"tent/spec-hash=3f9a1c0b7d2e4f68",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("EncodeTags (-want +got):\n%s", diff)
	}
}

func TestEncodeTagsNone(t *testing.T) {
	got, err := vultr.EncodeTags(nil)
	if err != nil || len(got) != 0 {
		t.Errorf("EncodeTags(nil) = %q, %v; want no tags", got, err)
	}
}

func TestTagsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels cloud.Labels
	}{
		{"canonical labels", allLabels()},
		{"a value with =", cloud.Labels{cloud.LabelCluster: "prod", "tent/note": "a=b"}},
		{"50 labels", manyLabels(50)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tags, err := vultr.EncodeTags(tc.labels)
			if err != nil {
				t.Fatalf("EncodeTags: %v", err)
			}
			got, err := vultr.DecodeTags(tags)
			if err != nil {
				t.Fatalf("DecodeTags(%q): %v", tags, err)
			}
			if diff := cmp.Diff(tc.labels, got); diff != "" {
				t.Errorf("DecodeTags(EncodeTags(l)) (-want +got):\n%s", diff)
			}
		})
	}
}

func TestEncodeTagsLimit(t *testing.T) {
	if tags, err := vultr.EncodeTags(manyLabels(50)); err != nil || len(tags) != 50 {
		t.Errorf("EncodeTags of 50 labels = %d tags, %v; want 50 tags", len(tags), err)
	}
	const want = "51 labels: an instance takes at most 50 tags"
	if tags, err := vultr.EncodeTags(manyLabels(51)); errText(err) != want || tags != nil {
		t.Errorf("EncodeTags of 51 labels = %q, %v; want the error %q", tags, err, want)
	}
}

func TestEncodeTagsErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels cloud.Labels
		want   string
	}{
		{"no prefix", cloud.Labels{"cluster": "prod"}, `label "cluster=prod": no "tent/" prefix`},
		{"empty key", cloud.Labels{"": "prod"}, `label "=prod": no "tent/" prefix`},
		{"prefix only", cloud.Labels{"tent/": "x"}, `label "tent/=x": no name after "tent/"`},
		{"= in the key", cloud.Labels{"tent/a=b": "c"}, `label "tent/a=b=c": "=" in key`},
		{"empty value", cloud.Labels{cloud.LabelCluster: ""}, `label "tent/cluster=": empty value`},
		{"upper case key", cloud.Labels{"tent/Cluster": "prod"}, `label "tent/Cluster=prod": upper case`},
		{"upper case prefix", cloud.Labels{"TENT/cluster": "prod"}, `label "TENT/cluster=prod": upper case`},
		{"upper case value", cloud.Labels{cloud.LabelCluster: "Prod"}, `label "tent/cluster=Prod": upper case`},
		{"non-ASCII upper case", cloud.Labels{cloud.LabelCluster: "prÜd"}, `label "tent/cluster=prÜd": upper case`},
		{"first bad label by key", cloud.Labels{"tent/b": "", "tent/a": "X"}, `label "tent/a=X": upper case`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tags, err := vultr.EncodeTags(tc.labels)
			if errText(err) != tc.want || tags != nil {
				t.Errorf("EncodeTags(%q) = %q, %v; want the error %q", tc.labels, tags, err, tc.want)
			}
		})
	}
}

func TestDecodeTags(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []string
		want cloud.Labels
	}{
		{
			"as Vultr returns them",
			[]string{"tent/cluster=prod", "team=web", "tent/op=" + opID},
			cloud.Labels{cloud.LabelCluster: "prod", cloud.LabelOp: opID},
		},
		{"no tags", nil, cloud.Labels{}},
		{
			"only foreign tags",
			[]string{
				"", "web", "Team=Web", "tent", "tentative=yes", "tent-ü", "tent:cluster=prod", "x/tent/cluster=prod",
			},
			cloud.Labels{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vultr.DecodeTags(tc.tags)
			if err != nil {
				t.Fatalf("DecodeTags(%q): %v", tc.tags, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("DecodeTags(%q) (-want +got):\n%s", tc.tags, diff)
			}
		})
	}
}

func TestDecodeTagsErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []string
		want string
	}{
		{"no =", []string{"tent/cluster"}, `tag "tent/cluster": no "="`},
		{"prefix only", []string{"tent/=prod"}, `tag "tent/=prod": no name after "tent/"`},
		{"empty value", []string{"tent/cluster="}, `tag "tent/cluster=": empty value`},
		{"upper case key", []string{"tent/Cluster=prod"}, `tag "tent/Cluster=prod": upper case`},
		{"upper case prefix", []string{"Tent/cluster=prod"}, `tag "Tent/cluster=prod": upper case`},
		{"upper case value", []string{"tent/cluster=Prod"}, `tag "tent/cluster=Prod": upper case`},
		{
			"same tag twice", []string{"tent/cluster=prod", "tent/cluster=prod"},
			`tag "tent/cluster=prod": key given twice`,
		},
		{
			"key twice", []string{"tent/cluster=prod", "team=web", "tent/cluster=dev"},
			`tag "tent/cluster=dev": key given twice`,
		},
		{"first bad tag", []string{"tent/role=", "tent/cluster=Prod"}, `tag "tent/role=": empty value`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := vultr.DecodeTags(tc.tags)
			if errText(err) != tc.want || got != nil {
				t.Errorf("DecodeTags(%q) = %q, %v; want the error %q", tc.tags, got, err, tc.want)
			}
		})
	}
}

// validMarkers are markers of every kind with their text form.
var validMarkers = []struct {
	name   string
	marker vultr.Marker
	text   string
}{
	{"vpc", vultr.Marker{Cluster: "prod", Kind: vultr.KindVPC}, "tent:cluster=prod;kind=vpc"},
	{
		"firewall group",
		vultr.Marker{Cluster: "prod", Kind: vultr.KindFirewall, Role: "server"},
		"tent:cluster=prod;kind=firewall;role=server",
	},
	{
		"ssh key",
		vultr.Marker{Cluster: "prod", Kind: vultr.KindSSHKey, Fingerprint: "3f9a1c0b"},
		"tent:cluster=prod;kind=ssh-key;fp=3f9a1c0b",
	},
	{
		"fingerprint with colons",
		vultr.Marker{Cluster: "prod", Kind: vultr.KindSSHKey, Fingerprint: "b7:2f:30"},
		"tent:cluster=prod;kind=ssh-key;fp=b7:2f:30",
	},
	{
		"load balancer",
		vultr.Marker{Cluster: "prod", Kind: vultr.KindLoadBalancer, Name: "api"},
		"tent:cluster=prod;kind=lb;name=api",
	},
	{
		"with an operation id",
		vultr.Marker{Cluster: "prod", Kind: vultr.KindVPC, Op: opID},
		"tent:cluster=prod;kind=vpc;op=" + opID,
	},
	{
		"every field",
		vultr.Marker{
			Cluster: "prod", Kind: vultr.KindFirewall, Role: "server", Name: "api", Fingerprint: "3f9a1c0b", Op: opID,
		},
		"tent:cluster=prod;kind=firewall;role=server;name=api;fp=3f9a1c0b;op=" + opID,
	},
}

func TestMarkerString(t *testing.T) {
	for _, tc := range validMarkers {
		if got := tc.marker.String(); got != tc.text {
			t.Errorf("%s: String() = %q, want %q", tc.name, got, tc.text)
		}
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	for _, tc := range validMarkers {
		if err := tc.marker.Validate(); err != nil {
			t.Errorf("%s: Validate: %v", tc.name, err)
		}
		got, ok, err := vultr.ParseMarker(tc.marker.String())
		if err != nil || !ok {
			t.Errorf("%s: ParseMarker(%q) = %v, %v; want ok", tc.name, tc.marker.String(), ok, err)
			continue
		}
		if diff := cmp.Diff(tc.marker, got); diff != "" {
			t.Errorf("%s: ParseMarker(String()) (-want +got):\n%s", tc.name, diff)
		}
	}
}

func TestParseMarkerAnyOrder(t *testing.T) {
	const text = "tent:op=" + opID + ";kind=vpc;cluster=prod"
	got, ok, err := vultr.ParseMarker(text)
	if err != nil || !ok {
		t.Fatalf("ParseMarker(%q) = %v, %v; want ok", text, ok, err)
	}
	want := vultr.Marker{Cluster: "prod", Kind: vultr.KindVPC, Op: opID}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseMarker(%q) (-want +got):\n%s", text, diff)
	}
}

// TestParseMarkerForeign checks texts that are not tent's: the prefix is exact, since tent writes it in lower case
// and compares it on the client.
func TestParseMarkerForeign(t *testing.T) {
	for _, text := range []string{
		"",
		"web servers",
		"tent",
		"tent/cluster=prod",
		"Tent:cluster=prod;kind=vpc",
		" tent:cluster=prod;kind=vpc",
		"prod vpc tent:cluster=prod;kind=vpc",
	} {
		got, ok, err := vultr.ParseMarker(text)
		if ok || err != nil || got != (vultr.Marker{}) {
			t.Errorf("ParseMarker(%q) = %+v, %v, %v; want not ours", text, got, ok, err)
		}
	}
}

func TestParseMarkerErrors(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"tent:", `marker "tent:": no cluster`},
		{"tent:kind=vpc", `marker "tent:kind=vpc": no cluster`},
		{"tent:cluster=prod", `marker "tent:cluster=prod": no kind`},
		{"tent:cluster=prod;;kind=vpc", `marker "tent:cluster=prod;;kind=vpc": empty field`},
		{"tent:cluster=prod;kind=vpc;", `marker "tent:cluster=prod;kind=vpc;": empty field`},
		{"tent:cluster=prod;vpc", `marker "tent:cluster=prod;vpc": no "=" in "vpc"`},
		{"tent:cluster=prod;kind=vpc;zone=ams", `marker "tent:cluster=prod;kind=vpc;zone=ams": unknown field "zone"`},
		{"tent:Cluster=prod;kind=vpc", `marker "tent:Cluster=prod;kind=vpc": unknown field "Cluster"`},
		{
			"tent:cluster=prod;kind=vpc;cluster=prod",
			`marker "tent:cluster=prod;kind=vpc;cluster=prod": cluster given twice`,
		},
		{"tent:cluster=prod;kind=vpc;role=", `marker "tent:cluster=prod;kind=vpc;role=": empty role`},
		{"tent:cluster=prod;kind=lb;name=a=b", `marker "tent:cluster=prod;kind=lb;name=a=b": "=" in name`},
		{"tent:cluster=Prod;kind=vpc", `marker "tent:cluster=Prod;kind=vpc": upper case in cluster`},
		{"tent:cluster=prod;kind=VPC", `marker "tent:cluster=prod;kind=VPC": upper case in kind`},
		{
			"tent:cluster=prod;kind=ssh-key;fp=3F9A1C0B",
			`marker "tent:cluster=prod;kind=ssh-key;fp=3F9A1C0B": upper case in fp`,
		},
	} {
		got, ok, err := vultr.ParseMarker(tc.text)
		if errText(err) != tc.want || !ok || got != (vultr.Marker{}) {
			t.Errorf("ParseMarker(%q) = %+v, %v, %v; want ours with the error %q", tc.text, got, ok, err, tc.want)
		}
	}
}

func TestMarkerValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker vultr.Marker
		want   string
	}{
		{"empty", vultr.Marker{}, `marker "tent:": no cluster`},
		{"no cluster", vultr.Marker{Kind: vultr.KindVPC}, `marker "tent:kind=vpc": no cluster`},
		{"no kind", vultr.Marker{Cluster: "prod"}, `marker "tent:cluster=prod": no kind`},
		{
			"; in a value",
			vultr.Marker{Cluster: "prod", Kind: vultr.KindFirewall, Role: "a;b"},
			`marker "tent:cluster=prod;kind=firewall;role=a;b": ";" in role`,
		},
		{
			"= in a value",
			vultr.Marker{Cluster: "prod", Kind: vultr.KindLoadBalancer, Name: "a=b"},
			`marker "tent:cluster=prod;kind=lb;name=a=b": "=" in name`,
		},
		{
			"upper case",
			vultr.Marker{Cluster: "prod", Kind: vultr.KindVPC, Op: "5F0C2A9E"},
			`marker "tent:cluster=prod;kind=vpc;op=5F0C2A9E": upper case in op`,
		},
		{
			"upper case in the cluster",
			vultr.Marker{Cluster: "Prod", Kind: vultr.KindVPC},
			`marker "tent:cluster=Prod;kind=vpc": upper case in cluster`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errText(tc.marker.Validate()); got != tc.want {
				t.Errorf("Validate() = %q, want %q", got, tc.want)
			}
		})
	}
}
