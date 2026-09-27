package vultr_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// Images as Vultr lists them.
var (
	ubuntu2404 = govultr.OS{ID: 2284, Name: "Ubuntu 24.04 LTS x64", Arch: "x64", Family: "ubuntu"}
	ubuntu2604 = govultr.OS{ID: 2760, Name: "Ubuntu 26.04 LTS x64", Arch: "x64", Family: "ubuntu"}
)

// preflightGroups returns the example's node groups, servers and workers, on vc2-2c-4gb and with the image left out,
// so that it takes the default. edit, when not nil, changes their specs. Workers come first in the list, so the
// preflight must sort the groups by name.
func preflightGroups(edit func(servers, workers *v1alpha1.NodeGroupSpec)) []*v1alpha1.NodeGroup {
	servers, workers := nodeGroup("servers", v1alpha1.RoleServer), nodeGroup("workers", v1alpha1.RoleClient)
	servers.Spec.Image, workers.Spec.Image = "", ""
	if edit != nil {
		edit(&servers.Spec, &workers.Spec)
	}
	return []*v1alpha1.NodeGroup{workers, servers}
}

// preflightCalls are the calls of a preflight of a cluster in region.
func preflightCalls(region string) []vultrfake.Call {
	return []vultrfake.Call{{Name: "AvailablePlans", Arg: region}, {Name: "ListPlans"}, {Name: "ListOS"}}
}

func TestValidate(t *testing.T) {
	const (
		machineType = "spec.machineType"
		notInAMS    = `plan "vc2-2c-4gb" is not available in ams now`
		no2404Now   = "Vultr does not offer ubuntu-24.04 (os_id 2284) now"
		no2604Now   = "Vultr does not offer ubuntu-26.04 (os_id 2760) now"
	)
	inXYZ := exampleClusterWith(func(c *v1alpha1.Cluster) { c.Spec.Cloud.Region = "xyz" })
	regionProblem := v1alpha1.FieldError{
		Object: "Cluster prod", Path: "spec.cloud.region", Detail: `Vultr has no region "xyz"`,
	}
	unsupportedImage := v1alpha1.FieldError{
		Object: "NodeGroup servers", Path: "spec.image",
		Detail: `tent supports ubuntu-24.04 and ubuntu-26.04 on Vultr, not "debian-12"`,
	}
	unknownPlan := v1alpha1.FieldError{
		Object: "NodeGroup workers", Path: machineType, Detail: `plan "vc2-9c-9gb" does not exist`,
	}
	for _, tc := range []struct {
		name    string
		fake    func(f *vultrfake.Fake) // changes the fake's catalog; nil keeps the default
		cluster *v1alpha1.Cluster       // nil for the example
		groups  []*v1alpha1.NodeGroup   // nil for preflightGroups(nil)
		want    v1alpha1.Errors
	}{
		{name: "the example with the default image"},
		{
			name: "ubuntu-26.04",
			groups: preflightGroups(func(s, w *v1alpha1.NodeGroupSpec) {
				s.Image, w.Image = "ubuntu-26.04", "ubuntu-26.04"
			}),
		},
		{
			name:    "unknown region",
			cluster: inXYZ,
			want:    v1alpha1.Errors{regionProblem},
		},
		{
			// The availability checks are skipped: xyz can deploy nothing.
			name:    "unknown region, plans and images still checked",
			cluster: inXYZ,
			groups: preflightGroups(func(s, w *v1alpha1.NodeGroupSpec) {
				s.Image, w.MachineType = "debian-12", "vc2-9c-9gb"
			}),
			want: v1alpha1.Errors{regionProblem, unsupportedImage, unknownPlan},
		},
		{
			name:   "unknown plan",
			groups: preflightGroups(func(_, w *v1alpha1.NodeGroupSpec) { w.MachineType = "vc2-9c-9gb" }),
			want:   v1alpha1.Errors{unknownPlan},
		},
		{
			name: "plan not available now",
			fake: func(f *vultrfake.Fake) { f.SetAvailability("ams", "vc2-1c-1gb", "vc2-1c-2gb") },
			want: v1alpha1.Errors{
				{Object: "NodeGroup servers", Path: machineType, Detail: notInAMS},
				{Object: "NodeGroup workers", Path: machineType, Detail: notInAMS},
			},
		},
		{
			name:   "unsupported image",
			groups: preflightGroups(func(s, _ *v1alpha1.NodeGroupSpec) { s.Image = "debian-12" }),
			want:   v1alpha1.Errors{unsupportedImage},
		},
		{
			name: "ubuntu-24.04 not offered now",
			fake: func(f *vultrfake.Fake) { f.SetOS(ubuntu2604) },
			want: v1alpha1.Errors{
				{Object: "NodeGroup servers", Path: "spec.image", Detail: no2404Now},
				{Object: "NodeGroup workers", Path: "spec.image", Detail: no2404Now},
			},
		},
		{
			name:   "ubuntu-26.04 not offered now",
			fake:   func(f *vultrfake.Fake) { f.SetOS(ubuntu2404) },
			groups: preflightGroups(func(_, w *v1alpha1.NodeGroupSpec) { w.Image = "ubuntu-26.04" }),
			want:   v1alpha1.Errors{{Object: "NodeGroup workers", Path: "spec.image", Detail: no2604Now}},
		},
		{
			name: "several problems",
			fake: func(f *vultrfake.Fake) {
				f.SetAvailability("ams", "vc2-1c-1gb")
				f.SetOS(ubuntu2604)
			},
			groups: preflightGroups(func(s, w *v1alpha1.NodeGroupSpec) {
				s.Image, w.MachineType = "debian-12", "vc2-9c-9gb"
			}),
			want: v1alpha1.Errors{
				{Object: "NodeGroup servers", Path: machineType, Detail: notInAMS},
				unsupportedImage,
				unknownPlan,
				{Object: "NodeGroup workers", Path: "spec.image", Detail: no2404Now},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := vultrfake.New()
			if tc.fake != nil {
				tc.fake(f)
			}
			c, groups := tc.cluster, tc.groups
			if c == nil {
				c = exampleCluster()
			}
			if groups == nil {
				groups = preflightGroups(nil)
			}
			p, _ := newProvider(f)
			err := p.Validate(t.Context(), c, groups)

			var got v1alpha1.Errors
			if err != nil && !errors.As(err, &got) {
				t.Fatalf("Validate = %v, want nil or v1alpha1.Errors", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("problems (-want +got):\n%s", diff)
			}
			wantCalls(t, f, preflightCalls(c.Spec.Cloud.Region)...)
		})
	}
}

func TestValidateErrorText(t *testing.T) {
	p, _ := newProvider(vultrfake.New())
	c := exampleClusterWith(func(c *v1alpha1.Cluster) { c.Spec.Cloud.Region = "xyz" })
	groups := preflightGroups(func(_, w *v1alpha1.NodeGroupSpec) { w.MachineType = "vc2-9c-9gb" })
	err := p.Validate(t.Context(), c, groups)
	const want = "Cluster prod: spec.cloud.region: Vultr has no region \"xyz\"\n" +
		"NodeGroup workers: spec.machineType: plan \"vc2-9c-9gb\" does not exist"
	if err == nil || err.Error() != want {
		t.Errorf("Validate = %v, want\n%s", err, want)
	}
}

func TestValidateAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		name, call, path string
		status           int
	}{
		{"availability", "AvailablePlans", "/v2/regions/ams/availability", http.StatusInternalServerError},
		// Only a 400 to the availability call means an unknown region; other ErrInvalid answers are API errors.
		{"availability 405", "AvailablePlans", "/v2/regions/ams/availability", http.StatusMethodNotAllowed},
		{"availability 422", "AvailablePlans", "/v2/regions/ams/availability", http.StatusUnprocessableEntity},
		{"availability 501", "AvailablePlans", "/v2/regions/ams/availability", http.StatusNotImplemented},
		{"plans", "ListPlans", "/v2/plans", http.StatusInternalServerError},
		{"plans invalid", "ListPlans", "/v2/plans", http.StatusBadRequest},
		{"images", "ListOS", "/v2/os", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := vultrfake.New()
			apiErr := vultr.NewAPIError(http.MethodGet, tc.path, tc.status, "Something went wrong", 0)
			f.Fail(t, tc.call, apiErr, 1)
			p, _ := newProvider(f)
			err := p.Validate(t.Context(), exampleCluster(), preflightGroups(nil))

			if want := "preflight of cluster prod: " + apiErr.Error(); err == nil || err.Error() != want {
				t.Errorf("Validate = %v, want %q", err, want)
			}
			var got *vultr.APIError
			if !errors.As(err, &got) || got != apiErr {
				t.Errorf("Validate = %v, want it to wrap the API's error", err)
			}
			var problems v1alpha1.Errors
			if errors.As(err, &problems) {
				t.Errorf("Validate = %v, want an API error, not problems of the specs", err)
			}
		})
	}
}

func TestValidateKeepsInputs(t *testing.T) {
	p, _ := newProvider(vultrfake.New())
	c, groups := exampleCluster(), preflightGroups(nil)
	if err := p.Validate(t.Context(), c, groups); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if diff := cmp.Diff(exampleCluster(), c); diff != "" {
		t.Errorf("cluster changed (-before +after):\n%s", diff)
	}
	if diff := cmp.Diff(preflightGroups(nil), groups); diff != "" {
		t.Errorf("groups changed (-before +after):\n%s", diff)
	}
}

func TestValidateOtherProvider(t *testing.T) {
	f := vultrfake.New()
	p, _ := newProvider(f)
	c := exampleClusterWith(func(c *v1alpha1.Cluster) {
		c.Spec.Cloud = v1alpha1.Cloud{Provider: v1alpha1.ProviderHetzner, Region: "eu-central", Zones: []string{"fsn1"}}
	})
	err := p.Validate(t.Context(), c, preflightGroups(nil))
	const want = `cluster prod runs on "hetzner", not on vultr`
	if err == nil || err.Error() != want {
		t.Errorf("Validate = %v, want %q", err, want)
	}
	wantCalls(t, f)
}

func TestValidateNoCluster(t *testing.T) {
	f := vultrfake.New()
	p, _ := newProvider(f)
	err := p.Validate(t.Context(), nil, preflightGroups(nil))
	const want = "preflight: no cluster to model"
	if err == nil || err.Error() != want {
		t.Errorf("Validate = %v, want %q", err, want)
	}
	wantCalls(t, f)
}
