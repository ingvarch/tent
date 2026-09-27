package vultrfake

import (
	"context"
	"net/http"
	"slices"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
)

// defaultRegion is the one region a new fake knows.
const defaultRegion = "ams"

// defaultPlans returns the smallest plans that Vultr listed on 2026-09-25, each in defaultRegion only.
func defaultPlans() []govultr.Plan {
	plan := func(id, planType string, vcpus, ram, disk, bandwidth int, monthly float32) govultr.Plan {
		return govultr.Plan{
			ID: id, VCPUCount: vcpus, RAM: ram, Disk: disk, DiskCount: 1, Bandwidth: bandwidth, MonthlyCost: monthly,
			Type: planType, Locations: []string{defaultRegion},
		}
	}
	return []govultr.Plan{
		plan("vc2-1c-1gb", "vc2", 1, 1024, 25, 1024, 5),
		plan("vc2-1c-2gb", "vc2", 1, 2048, 55, 2048, 10),
		plan("vc2-2c-4gb", "vc2", 2, 4096, 80, 3072, 20),
		plan("vhf-1c-2gb", "vhf", 1, 2048, 64, 2048, 12),
		plan("vhp-1c-2gb-amd", "vhp", 1, 2048, 50, 3072, 12),
	}
}

// defaultOS returns the images tent supports, as Vultr listed them on 2026-09-25.
func defaultOS() []govultr.OS {
	return []govultr.OS{
		{ID: 2284, Name: "Ubuntu 24.04 LTS x64", Arch: "x64", Family: "ubuntu"},
		{ID: 2760, Name: "Ubuntu 26.04 LTS x64", Arch: "x64", Family: "ubuntu"},
		{ID: 2136, Name: "Debian 12 x64 (bookworm)", Arch: "x64", Family: "debian"},
		{ID: 2625, Name: "Debian 13 x64 (trixie)", Arch: "x64", Family: "debian"},
	}
}

// SetAvailability sets the plans that a region can deploy now; none when planIDs is empty. A region without a call
// of SetAvailability is unknown, except ams, which at first can deploy every default plan.
func (f *Fake) SetAvailability(region string, planIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.available[region] = slices.Clone(planIDs)
}

// SetPlans replaces the plans. It leaves the availability as it is.
func (f *Fake) SetPlans(plans ...govultr.Plan) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans = clonePlans(plans, "")
}

// SetOS replaces the operating system images.
func (f *Fake) SetOS(images ...govultr.OS) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images = slices.Clone(images)
}

// AvailablePlans returns the plans of a type that a region can deploy now; every type's when planType is empty. The
// type of a plan comes from the fake's plans, so with a type it leaves out a plan they do not hold. Like the client,
// it returns an empty list, not nil, when the region offers none. It fails with vultr.ErrInvalid for an unknown
// region.
func (f *Fake) AvailablePlans(ctx context.Context, region, planType string) ([]string, error) {
	if err := vultr.CheckID("GET /v2/regions/{region}/availability", "region", region); err != nil {
		return nil, err
	}
	r := request{
		name: "AvailablePlans", arg: region, method: http.MethodGet, path: "/v2/regions/" + region + "/availability",
	}
	out := []string{}
	err := f.run(ctx, r, func() error {
		ids, ok := f.available[region]
		if !ok {
			// Matches Vultr's answer to an unknown region, checked on 2026-09-27.
			return r.fail(http.StatusBadRequest, "Invalid region.")
		}
		for _, id := range ids {
			i := slices.IndexFunc(f.plans, func(p govultr.Plan) bool { return p.ID == id })
			if planType == "" || (i >= 0 && f.plans[i].Type == planType) {
				out = append(out, id)
			}
		}
		return nil
	})
	return result(out, err)
}

// ListPlans returns every plan of a type; every type's when planType is empty.
func (f *Fake) ListPlans(ctx context.Context, planType string) ([]govultr.Plan, error) {
	var out []govultr.Plan
	r := request{name: "ListPlans", arg: planType, method: http.MethodGet, path: "/v2/plans"}
	err := f.run(ctx, r, func() error {
		out = clonePlans(f.plans, planType)
		return nil
	})
	return result(out, err)
}

// ListOS returns every operating system image.
func (f *Fake) ListOS(ctx context.Context) ([]govultr.OS, error) {
	var out []govultr.OS
	err := f.run(ctx, request{name: "ListOS", method: http.MethodGet, path: "/v2/os"}, func() error {
		out = clone(f.images)
		return nil
	})
	return result(out, err)
}

// clonePlans returns a deep copy of the plans of a type; of every type when planType is empty.
func clonePlans(plans []govultr.Plan, planType string) []govultr.Plan {
	var out []govultr.Plan
	for _, p := range plans {
		if planType == "" || p.Type == planType {
			p.Locations = slices.Clone(p.Locations)
			out = append(out, p)
		}
	}
	return out
}
