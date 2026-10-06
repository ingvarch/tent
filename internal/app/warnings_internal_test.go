package app

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/channels"
)

// TestWarnings returns the warnings about a cluster in a fixed order: the open API, the combined group, the Nomad
// version that the channel has not tested.
func TestWarnings(t *testing.T) {
	ch := &channels.Channel{Name: "stable", Nomad: channels.Nomad{Tested: []string{"2.0.7"}}}
	cluster := func(api, version string) *v1alpha1.Cluster {
		c := &v1alpha1.Cluster{}
		c.Spec.Access.API = []string{api}
		c.Spec.Nomad.Version = version
		return c
	}
	group := func(role v1alpha1.Role) []*v1alpha1.NodeGroup {
		g := &v1alpha1.NodeGroup{}
		g.Metadata.Name = "nodes"
		g.Spec.Role = role
		return []*v1alpha1.NodeGroup{nil, g}
	}
	const untested = "Nomad 2.0.6 is not tested by this tent; channel stable tests 2.0.7"
	for _, tc := range []struct {
		name   string
		c      *v1alpha1.Cluster
		groups []*v1alpha1.NodeGroup
		want   []string
	}{
		{"neither", cluster("203.0.113.0/24", ""), group(v1alpha1.RoleServer), nil},
		{"open", cluster("0.0.0.0/0", ""), group(v1alpha1.RoleServer), []string{openAPIWarning}},
		{"combined", cluster("203.0.113.0/24", ""), group(v1alpha1.RoleCombined),
			[]string{combinedWarning("nodes")}},
		{"both", cluster("0.0.0.0/0", ""), group(v1alpha1.RoleCombined),
			[]string{openAPIWarning, combinedWarning("nodes")}},
		{"all three", cluster("::/0", "2.0.6"), group(v1alpha1.RoleCombined),
			[]string{openAPIWarning, combinedWarning("nodes"), untested}},
		{"untested alone", cluster("203.0.113.0/24", "2.0.6"), group(v1alpha1.RoleClient), []string{untested}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, warnings(tc.c, tc.groups, ch)); diff != "" {
				t.Errorf("warnings (-want +got):\n%s", diff)
			}
		})
	}
}
