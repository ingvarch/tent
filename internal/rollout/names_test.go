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

func TestNodeIndex(t *testing.T) {
	tests := []struct {
		name  string
		node  string
		index int
		ok    bool
	}{
		{"the first index", "prod-servers-0", 0, true},
		{"a larger index", "prod-servers-12", 12, true},
		{"the highest index", "prod-servers-2147483647", 2147483647, true},
		{"an index above the highest", "prod-servers-2147483648", 0, false},
		{"an index of 2 to the power of 32", "prod-servers-4294967296", 0, false},
		{"an index far above the highest", "prod-servers-99999999999999999999", 0, false},
		{"a letter", "prod-servers-x", 0, false},
		{"no index", "prod-servers-", 0, false},
		{"a minus sign", "prod-servers--9", 0, false},
		{"a plus sign", "prod-servers-+9", 0, false},
		{"a suffix", "prod-servers-9-b", 0, false},
		{"another cluster", "other-servers-9", 0, false},
		{"another group", "prod-workers-9", 0, false},
		{"no cluster", "servers-9", 0, false},
		{"no dash before the index", "prod-servers9", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index, ok := rollout.NodeIndex("prod", "servers", tt.node)
			if index != tt.index || ok != tt.ok {
				t.Errorf("NodeIndex(%q) = %d, %t, want %d, %t", tt.node, index, ok, tt.index, tt.ok)
			}
		})
	}
}

func TestGroupOfNode(t *testing.T) {
	tests := []struct {
		name, cluster, node string
		group               string
		ok                  bool
	}{
		{"a node of a group", "prod", "prod-workers-3", "workers", true},
		{"an index of several digits", "prod", "prod-workers-120", "workers", true},
		{"a group with dashes", "prod", "prod-web-workers-3", "web-workers", true},
		{"a group whose name ends in digits", "prod", "prod-pool-2-7", "pool-2", true},
		{"a cluster with dashes", "my-prod", "my-prod-workers-3", "workers", true},
		{"the name of another cluster", "prod", "dev-workers-3", "", false},
		{"a cluster whose name only starts the same", "prod", "prod2-workers-3", "", false},
		{"no index", "prod", "prod-workers", "", false},
		{"an empty index", "prod", "prod-workers-", "", false},
		{"no group", "prod", "prod--3", "", false},
		{"a suffix after the index", "prod", "prod-workers-3-x", "", false},
		{"a sign in the index", "prod", "prod-workers-+3", "", false},
		{"the cluster alone", "prod", "prod", "", false},
		{"empty", "prod", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group, ok := rollout.GroupOfNode(tt.cluster, tt.node)
			if group != tt.group || ok != tt.ok {
				t.Errorf("GroupOfNode(%q, %q) = %q, %t, want %q, %t", tt.cluster, tt.node, group, ok, tt.group, tt.ok)
			}
		})
	}
}

func TestGroupOfNodeIsTheReverseOfNodeName(t *testing.T) {
	for _, group := range []string{"workers", "web-workers", "pool-2"} {
		got, ok := rollout.GroupOfNode("prod", rollout.NodeName("prod", group, 12))
		if got != group || !ok {
			t.Errorf("GroupOfNode(NodeName(%q)) = %q, %t, want the group", group, got, ok)
		}
	}
}
