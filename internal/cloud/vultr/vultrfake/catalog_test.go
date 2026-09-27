package vultrfake_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// The default plans, as Vultr listed them on 2026-09-25.
var (
	vc21c1gb = govultr.Plan{
		ID: "vc2-1c-1gb", VCPUCount: 1, RAM: 1024, Disk: 25, DiskCount: 1, Bandwidth: 1024, MonthlyCost: 5,
		Type: "vc2", Locations: []string{"ams"},
	}
	vc21c2gb = govultr.Plan{
		ID: "vc2-1c-2gb", VCPUCount: 1, RAM: 2048, Disk: 55, DiskCount: 1, Bandwidth: 2048, MonthlyCost: 10,
		Type: "vc2", Locations: []string{"ams"},
	}
	vc22c4gb = govultr.Plan{
		ID: "vc2-2c-4gb", VCPUCount: 2, RAM: 4096, Disk: 80, DiskCount: 1, Bandwidth: 3072, MonthlyCost: 20,
		Type: "vc2", Locations: []string{"ams"},
	}
	vhf1c2gb = govultr.Plan{
		ID: "vhf-1c-2gb", VCPUCount: 1, RAM: 2048, Disk: 64, DiskCount: 1, Bandwidth: 2048, MonthlyCost: 12,
		Type: "vhf", Locations: []string{"ams"},
	}
	vhp1c2gb = govultr.Plan{
		ID: "vhp-1c-2gb-amd", VCPUCount: 1, RAM: 2048, Disk: 50, DiskCount: 1, Bandwidth: 3072, MonthlyCost: 12,
		Type: "vhp", Locations: []string{"ams"},
	}
)

func TestDefaultPlans(t *testing.T) {
	f := newFake()
	for _, tc := range []struct {
		planType string
		want     []govultr.Plan
	}{
		{"", []govultr.Plan{vc21c1gb, vc21c2gb, vc22c4gb, vhf1c2gb, vhp1c2gb}},
		{"vc2", []govultr.Plan{vc21c1gb, vc21c2gb, vc22c4gb}},
		{"vhp", []govultr.Plan{vhp1c2gb}},
		{"vcg", nil},
	} {
		got, err := f.ListPlans(t.Context(), tc.planType)
		if err != nil {
			t.Fatalf("ListPlans(%q): %v", tc.planType, err)
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("ListPlans(%q) (-want +got):\n%s", tc.planType, diff)
		}
	}
}

func TestDefaultOS(t *testing.T) {
	want := []govultr.OS{
		{ID: 2284, Name: "Ubuntu 24.04 LTS x64", Arch: "x64", Family: "ubuntu"},
		{ID: 2760, Name: "Ubuntu 26.04 LTS x64", Arch: "x64", Family: "ubuntu"},
		{ID: 2136, Name: "Debian 12 x64 (bookworm)", Arch: "x64", Family: "debian"},
		{ID: 2625, Name: "Debian 13 x64 (trixie)", Arch: "x64", Family: "debian"},
	}
	got, err := newFake().ListOS(t.Context())
	if err != nil {
		t.Fatalf("ListOS: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListOS (-want +got):\n%s", diff)
	}
}

// wantAvailable checks the plans that AvailablePlans returns. Like the client, it returns an empty list, not nil,
// when the region offers none.
func wantAvailable(t *testing.T, f *vultrfake.Fake, region, planType string, want ...string) {
	t.Helper()
	got, err := f.AvailablePlans(t.Context(), region, planType)
	if err != nil {
		t.Fatalf("AvailablePlans(%q, %q): %v", region, planType, err)
	}
	if want == nil {
		want = []string{}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("AvailablePlans(%q, %q) (-want +got):\n%s", region, planType, diff)
	}
}

func TestDefaultAvailability(t *testing.T) {
	f := newFake()
	wantAvailable(t, f, "ams", "", "vc2-1c-1gb", "vc2-1c-2gb", "vc2-2c-4gb", "vhf-1c-2gb", "vhp-1c-2gb-amd")
	wantAvailable(t, f, "ams", "vc2", "vc2-1c-1gb", "vc2-1c-2gb", "vc2-2c-4gb")
	wantAvailable(t, f, "ams", "vhf", "vhf-1c-2gb")
	plans, err := f.AvailablePlans(t.Context(), "fra", "vc2")
	wantAPIError(t, err, vultr.ErrInvalid, "vultr: GET /v2/regions/fra/availability: 400 Bad Request: Invalid region.")
	if plans != nil {
		t.Errorf("AvailablePlans returned %v with the error", plans)
	}
	wantCalls(t, f,
		vultrfake.Call{Name: "AvailablePlans", Arg: "ams"},
		vultrfake.Call{Name: "AvailablePlans", Arg: "ams"},
		vultrfake.Call{Name: "AvailablePlans", Arg: "ams"},
		vultrfake.Call{Name: "AvailablePlans", Arg: "fra"},
	)
}

func TestSetAvailability(t *testing.T) {
	f := newFake()
	f.SetAvailability("ams") // sold out
	wantAvailable(t, f, "ams", "vc2")
	// The type of a plan comes from the fake's plans, so a plan it does not list has none.
	f.SetAvailability("fra", "vc2-1c-1gb", "vhf-1c-2gb", "vc2-9c-99gb")
	wantAvailable(t, f, "fra", "vc2", "vc2-1c-1gb")
	wantAvailable(t, f, "fra", "", "vc2-1c-1gb", "vhf-1c-2gb", "vc2-9c-99gb")
	wantAvailable(t, f, "fra", "vhp")
}

func TestSetPlansAndOS(t *testing.T) {
	f := newFake()
	plans := []govultr.Plan{{ID: "vc2-4c-8gb", Type: "vc2", MonthlyCost: 40, Locations: []string{"fra"}}}
	images := []govultr.OS{{ID: 1, Name: "Custom"}}
	f.SetPlans(plans...)
	f.SetOS(images...)
	plans[0].Locations[0] = "changed"
	images[0].Name = "changed"

	gotPlans, err := f.ListPlans(t.Context(), "vc2")
	want := []govultr.Plan{{ID: "vc2-4c-8gb", Type: "vc2", MonthlyCost: 40, Locations: []string{"fra"}}}
	if diff := cmp.Diff(want, gotPlans); err != nil || diff != "" {
		t.Errorf("ListPlans = %v; (-want +got):\n%s", err, diff)
	}
	gotOS, err := f.ListOS(t.Context())
	if diff := cmp.Diff([]govultr.OS{{ID: 1, Name: "Custom"}}, gotOS); err != nil || diff != "" {
		t.Errorf("ListOS = %v; (-want +got):\n%s", err, diff)
	}
	// SetPlans leaves the availability as it was.
	wantAvailable(t, f, "ams", "", "vc2-1c-1gb", "vc2-1c-2gb", "vc2-2c-4gb", "vhf-1c-2gb", "vhp-1c-2gb-amd")
}

func TestCatalogReturnsCopies(t *testing.T) {
	f := newFake()
	plans, err := f.ListPlans(t.Context(), "vc2")
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	plans[0].Locations[0] = "changed"
	plans[1].ID = "changed"
	available, err := f.AvailablePlans(t.Context(), "ams", "vc2")
	if err != nil {
		t.Fatalf("AvailablePlans: %v", err)
	}
	available[0] = "changed"
	images, err := f.ListOS(t.Context())
	if err != nil {
		t.Fatalf("ListOS: %v", err)
	}
	images[0].Name = "changed"

	g := newFake()
	for _, call := range []struct {
		name string
		got  func(*vultrfake.Fake) (any, error)
	}{
		{"ListPlans", func(f *vultrfake.Fake) (any, error) { return f.ListPlans(t.Context(), "") }},
		{"AvailablePlans", func(f *vultrfake.Fake) (any, error) { return f.AvailablePlans(t.Context(), "ams", "") }},
		{"ListOS", func(f *vultrfake.Fake) (any, error) { return f.ListOS(t.Context()) }},
	} {
		got, err := call.got(f)
		want, werr := call.got(g)
		if diff := cmp.Diff(want, got); err != nil || werr != nil || diff != "" {
			t.Errorf("%s after changing a returned value: %v, %v; (-want +got):\n%s", call.name, err, werr, diff)
		}
	}
}
