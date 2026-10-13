package app_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/internal/app"
)

// dueUpgrade is a plan of a cluster on Nomad 2.0.6 whose channel recommends 2.0.7, before a write.
func dueUpgrade() app.UpgradePlan {
	return app.UpgradePlan{
		Cluster: "prod", Channel: "stable", Recommended: "2.0.7",
		Pinned: "2.0.6", Current: "2.0.6", Target: "2.0.7",
	}
}

// appliedUpgrade is dueUpgrade after the write.
func appliedUpgrade() app.UpgradePlan {
	p := dueUpgrade()
	p.Applied = true
	return p
}

func TestUpgradePlanWriteTextGolden(t *testing.T) {
	t.Parallel()
	checkGolden(t, "upgrade_plan.golden", planText(t, dueUpgrade()))
}

func TestUpgradePlanAppliedWriteTextGolden(t *testing.T) {
	t.Parallel()
	checkGolden(t, "upgrade_plan_applied.golden", planText(t, appliedUpgrade()))
}

func TestUpgradePlanJSONGolden(t *testing.T) {
	t.Parallel()
	checkGolden(t, "upgrade_plan.json.golden", encodeJSON(t, dueUpgrade(), "  "))
}

func TestUpgradePlanAppliedJSONGolden(t *testing.T) {
	t.Parallel()
	checkGolden(t, "upgrade_plan_applied.json.golden", encodeJSON(t, appliedUpgrade(), "  "))
}

func TestUpgradePlanWriteText(t *testing.T) {
	t.Parallel()
	const hasNoUpgrade = "cluster prod: nothing to upgrade: "
	for _, tc := range []struct {
		name string
		plan app.UpgradePlan
		want string
	}{
		{
			"the cluster is on the recommended version",
			app.UpgradePlan{
				Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Pinned: "2.0.7", Current: "2.0.7",
			},
			hasNoUpgrade + "its Nomad version is 2.0.7, which channel stable recommends.\n",
		},
		{
			"the cluster is on a newer version than the recommended one",
			app.UpgradePlan{
				Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: "2.0.8", Pinned: "2.0.8", Current: "2.0.8",
			},
			hasNoUpgrade + "its Nomad version is 2.0.8, newer than the 2.0.7 that channel stable recommends.\n",
		},
		{
			"the cluster has no version yet",
			app.UpgradePlan{Cluster: "prod", Channel: "stable", Recommended: "2.0.7"},
			hasNoUpgrade + "it has no Nomad version yet; its first tent update cluster takes 2.0.7.\n",
		},
		{
			"the spec and the pin differ",
			app.UpgradePlan{
				Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: "2.0.7", Pinned: "2.0.6", Current: "2.0.7",
			},
			hasNoUpgrade + "its Nomad version is 2.0.7, which channel stable recommends.\n" +
				"tent update cluster has not applied Nomad 2.0.7 yet: the cluster is pinned to 2.0.6.\n",
		},
		{
			"the spec and the pin differ, and the version is newer than the recommended one",
			app.UpgradePlan{
				Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: "2.0.8", Pinned: "2.0.6", Current: "2.0.8",
			},
			hasNoUpgrade + "its Nomad version is 2.0.8, newer than the 2.0.7 that channel stable recommends.\n" +
				"tent update cluster has not applied Nomad 2.0.8 yet: the cluster is pinned to 2.0.6.\n",
		},
		{
			"the spec equals the pin",
			app.UpgradePlan{
				Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: "2.0.7", Pinned: "2.0.7", Current: "2.0.7",
			},
			hasNoUpgrade + "its Nomad version is 2.0.7, which channel stable recommends.\n",
		},
		{
			"a spec without a pin",
			app.UpgradePlan{Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: "2.0.7", Current: "2.0.7"},
			hasNoUpgrade + "its Nomad version is 2.0.7, which channel stable recommends.\n",
		},
		{
			"a write that was asked for and had nothing to write",
			app.UpgradePlan{
				Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Pinned: "2.0.7", Current: "2.0.7", Applied: true,
			},
			hasNoUpgrade + "its Nomad version is 2.0.7, which channel stable recommends.\n",
		},
		{
			"an upgrade is due while the spec and the pin differ",
			app.UpgradePlan{
				Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: "2.0.6", Pinned: "2.0.5", Current: "2.0.6",
				Target: "2.0.7",
			},
			"cluster prod: upgrade Nomad from 2.0.6 to 2.0.7, which channel stable recommends.\n" +
				"After the upgrade: tent update cluster, then tent rolling-update cluster.\n",
		},
		{
			"an applied upgrade while the spec and the pin differed",
			app.UpgradePlan{
				Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: "2.0.6", Pinned: "2.0.5", Current: "2.0.6",
				Target: "2.0.7", Applied: true,
			},
			"cluster prod: spec.nomad.version is now 2.0.7; the cluster was on Nomad 2.0.6.\n" +
				"Next: tent update cluster, then tent rolling-update cluster.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, planText(t, tc.plan)); diff != "" {
				t.Errorf("WriteText (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpgradePlanWriteTextWriteError(t *testing.T) {
	t.Parallel()
	errBroken := errors.New("broken pipe")
	for _, tc := range []struct {
		name string
		plan app.UpgradePlan
	}{
		{"an upgrade is due", dueUpgrade()},
		{"applied", appliedUpgrade()},
		{"nothing to upgrade", app.UpgradePlan{Cluster: "prod", Channel: "stable", Recommended: "2.0.7"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &failWriter{err: errBroken}
			err := tc.plan.WriteText(w)
			if got, want := err, "writing the plan: broken pipe"; got == nil || got.Error() != want {
				t.Errorf("WriteText error = %v, want %q", got, want)
			}
			if !errors.Is(err, errBroken) {
				t.Errorf("WriteText error %v does not wrap %v", err, errBroken)
			}
			if w.writes != 1 {
				t.Errorf("WriteText wrote %d times, want 1: it stops at the first error", w.writes)
			}
		})
	}
}

func TestUpgradePlanDue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		plan app.UpgradePlan
		want bool
	}{
		{"a target", dueUpgrade(), true},
		{"a target that is written", appliedUpgrade(), true},
		{"no target", app.UpgradePlan{Cluster: "prod", Recommended: "2.0.7", Current: "2.0.7"}, false},
		{"no target, applied", app.UpgradePlan{Cluster: "prod", Recommended: "2.0.7", Applied: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.Due(); got != tc.want {
				t.Errorf("Due() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUpgradePlanJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		plan app.UpgradePlan
		want string
	}{
		{
			"every key",
			app.UpgradePlan{
				Cluster: "prod", Channel: "stable", Recommended: "2.0.7", Spec: "2.0.6", Pinned: "2.0.5",
				Current: "2.0.6", Target: "2.0.7", Applied: true,
			},
			`{"cluster":"prod","channel":"stable","recommended":"2.0.7","spec":"2.0.6","pinned":"2.0.5",` +
				`"current":"2.0.6","target":"2.0.7","applied":true}` + "\n",
		},
		{
			"nothing to upgrade on a cluster without a version",
			app.UpgradePlan{Cluster: "prod", Channel: "stable", Recommended: "2.0.7"},
			`{"cluster":"prod","channel":"stable","recommended":"2.0.7"}` + "\n",
		},
		{
			"an upgrade that is due",
			dueUpgrade(),
			`{"cluster":"prod","channel":"stable","recommended":"2.0.7","pinned":"2.0.6","current":"2.0.6",` +
				`"target":"2.0.7"}` + "\n",
		},
		{
			"an empty plan keeps cluster, channel and recommended",
			app.UpgradePlan{},
			`{"cluster":"","channel":"","recommended":""}` + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodeJSON(t, tc.plan, ""); got != tc.want {
				t.Errorf("JSON = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUpgradePlanJSONLeavesEscapingToTheCaller checks that the plan's JSON escapes HTML characters such as < and &
// only when the caller's encoder does.
func TestUpgradePlanJSONLeavesEscapingToTheCaller(t *testing.T) {
	t.Parallel()
	p := app.UpgradePlan{Cluster: "prod", Channel: "<beta>&more", Recommended: "2.0.7"}
	want := `{"cluster":"prod","channel":"<beta>&more","recommended":"2.0.7"}` + "\n"
	if got := encodeJSON(t, p, ""); got != want {
		t.Errorf("JSON without HTML escaping = %q, want %q", got, want)
	}
	escaped, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `\u003cbeta\u003e\u0026more`; !strings.Contains(string(escaped), want) {
		t.Errorf("json.Marshal = %s, want it to hold %s", escaped, want)
	}
}

// TestUpgradePlanYAML checks that the CLI's YAML encoder, which goes through the plan's JSON, writes the same keys.
func TestUpgradePlanYAML(t *testing.T) {
	t.Parallel()
	got, err := yaml.Marshal(appliedUpgrade())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := "applied: true\nchannel: stable\ncluster: prod\ncurrent: 2.0.6\npinned: 2.0.6\nrecommended: 2.0.7\n" +
		"target: 2.0.7\n"
	if diff := cmp.Diff(want, string(got)); diff != "" {
		t.Errorf("YAML (-want +got):\n%s", diff)
	}
}
