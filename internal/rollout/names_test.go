package rollout_test

import (
	"slices"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/rollout"
)

func TestNodeName(t *testing.T) {
	if got, want := rollout.NodeName("prod", "workers", 12), "prod-workers-12"; got != want {
		t.Errorf("NodeName = %q, want %q", got, want)
	}
}

func TestNodeOfServer(t *testing.T) {
	tests := []struct{ server, want string }{
		{"prod-servers-0.global", "prod-servers-0"},
		{"prod-servers-0.eu.west", "prod-servers-0"},
		{"prod-servers-0", "prod-servers-0"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := rollout.NodeOfServer(tc.server); got != tc.want {
			t.Errorf("NodeOfServer(%q) = %q, want %q", tc.server, got, tc.want)
		}
	}
}

func TestFreeName(t *testing.T) {
	tests := []struct {
		name  string
		taken map[string]bool
		want  string
	}{
		{"nothing taken", nil, "prod-workers-0"},
		{"the lowest free index past taken names", map[string]bool{"prod-workers-0": true, "prod-workers-1": true},
			"prod-workers-2"},
		{"a gap is filled", map[string]bool{"prod-workers-0": true, "prod-workers-2": true}, "prod-workers-1"},
		{"a name that maps to false is free", map[string]bool{"prod-workers-0": false}, "prod-workers-0"},
		{"names of other groups and clusters do not count",
			map[string]bool{"prod-servers-0": true, "dev-workers-0": true}, "prod-workers-0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rollout.FreeName("prod", "workers", tt.taken); got != tt.want {
				t.Errorf("FreeName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFreeNameDoesNotChangeTaken(t *testing.T) {
	taken := map[string]bool{"prod-workers-0": true}
	rollout.FreeName("prod", "workers", taken)
	if len(taken) != 1 || !taken["prod-workers-0"] {
		t.Errorf("taken = %v, want it unchanged", taken)
	}
}

func TestLeastUsedZone(t *testing.T) {
	tests := []struct {
		name    string
		zones   []string
		perZone map[string]int
		want    string
	}{
		{"the zone with the fewest", []string{"ams", "fra", "lon"}, map[string]int{"ams": 2, "fra": 1, "lon": 2}, "fra"},
		{"the first listed on a tie", []string{"ams", "fra", "lon"}, map[string]int{"ams": 1, "fra": 1, "lon": 1}, "ams"},
		{"a zone without machines counts zero", []string{"ams", "fra"}, map[string]int{"ams": 1}, "fra"},
		{"the first of equals, not of all", []string{"ams", "fra", "lon"}, map[string]int{"ams": 3, "fra": 1, "lon": 1},
			"fra"},
		{"a zone that is not listed does not count", []string{"ams"}, map[string]int{"ams": 2, "old": 0}, "ams"},
		{"no zones", nil, map[string]int{"ams": 1}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rollout.LeastUsedZone(tt.zones, tt.perZone); got != tt.want {
				t.Errorf("LeastUsedZone = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompareCreated(t *testing.T) {
	early := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	tests := []struct {
		name string
		a, b time.Time
		want int
	}{
		{"earlier first", early, late, -1},
		{"later last", late, early, 1},
		{"equal", early, early, 0},
		{"unknown counts as the latest", time.Time{}, early, 1},
		{"known before unknown", early, time.Time{}, -1},
		{"two unknown are equal", time.Time{}, time.Time{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rollout.CompareCreated(tt.a, tt.b); got != tt.want {
				t.Errorf("CompareCreated = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCompareCreatedSortsUnknownLast(t *testing.T) {
	early := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	times := []time.Time{{}, early.Add(time.Hour), early}
	slices.SortFunc(times, rollout.CompareCreated)
	if !times[0].Equal(early) || !times[1].Equal(early.Add(time.Hour)) || !times[2].IsZero() {
		t.Errorf("sorted = %v, want early, late, unknown", times)
	}
}
