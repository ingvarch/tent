package rollout_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/rollout"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

// workersGroup is the client group workers: size 3, new hash, zones ams and fra, a drain deadline of an hour.
func workersGroup(surge, unavailable int) rollout.Group {
	return rollout.Group{
		Name: "workers", Role: v1alpha1.RoleClient, Size: 3, Zones: []string{"ams", "fra"}, SpecHash: newHash,
		MaxSurge: surge, MaxUnavailable: unavailable, DrainTimeout: time.Hour,
	}
}

// outdatedWorkers is a cluster of one server and three outdated workers.
func outdatedWorkers(surge, unavailable int) *world {
	w := newWorld(curVersion)
	w.addServer(curVersion)
	w.addGroup(workersGroup(surge, unavailable))
	w.addClients("workers", 3, oldHash, oldVersion)
	return w
}

type scenario struct {
	name  string
	mode  rollout.Mode
	build func() *world
}

var scenarios = []scenario{
	{"clients_surge1", rollout.Roll, func() *world { return outdatedWorkers(1, 0).arm() }},
	{"clients_surge2", rollout.Roll, func() *world { return outdatedWorkers(2, 0).arm() }},
	{"clients_unavailable1", rollout.Roll, func() *world { return outdatedWorkers(0, 1).arm() }},
	{"refuse_client_newer", rollout.Roll, func() *world {
		w := outdatedWorkers(1, 0)
		w.version = "2.0.8"
		return w.arm()
	}},
	{"refuse_downgrade", rollout.Roll, func() *world {
		w := newWorld(oldVersion)
		w.addServer(oldVersion)
		w.addGroup(workersGroup(1, 0))
		w.addClients("workers", 3, oldHash, curVersion)
		return w.arm()
	}},
	{"refuse_stuck", rollout.Roll, func() *world {
		w := newWorld(curVersion)
		w.addServer(curVersion)
		w.addGroup(workersGroup(1, 0))
		w.addClients("workers", 1, newHash, curVersion)
		w.addClients("workers", 2, oldHash, oldVersion)
		w.makeIneligible("prod-workers-0")
		return w.arm()
	}},
	{"refuse_duplicates", rollout.Roll, func() *world {
		w := outdatedWorkers(1, 0)
		w.duplicate("prod-workers-1")
		return w.arm()
	}},
}

// describe is the comment that starts a golden file: the mode, the groups and the machines.
func (w *world) describe(mode rollout.Mode) []string {
	modes := map[rollout.Mode]string{rollout.Roll: "roll", rollout.Shrink: "shrink"}
	lines := []string{
		"mode " + modes[mode],
		fmt.Sprintf("a new node runs Nomad %s", w.version),
	}
	for _, g := range w.groups {
		lines = append(lines, fmt.Sprintf("group %s: %s, size %d, zones %s, maxSurge %d, maxUnavailable %d, drain %s",
			g.Name, g.Role, g.Size, strings.Join(g.Zones, " and "), g.MaxSurge, g.MaxUnavailable, g.DrainTimeout))
	}
	for _, m := range w.machines {
		line := fmt.Sprintf("machine %s (%s) in %s runs Nomad %s", m.Name, m.ID, m.Zone, m.version)
		if g, ok := w.group(m.Group); ok {
			if m.SpecHash == g.SpecHash {
				line += ", up to date"
			} else {
				line += ", outdated"
			}
		}
		if i := w.nodeIndexByOwner(m.ID); i >= 0 && !w.nodes[i].Eligible {
			line += ", its node is ineligible"
		}
		lines = append(lines, line)
	}
	for _, srv := range w.servers {
		if srv.leader {
			lines = append(lines, "leader "+w.machines[w.machineIndex(srv.machine)].Name)
		}
	}
	return lines
}

// transcript is the text of a golden file: the description as comments, then one line per step.
func transcript(description, lines []string) string {
	var b strings.Builder
	for _, l := range description {
		b.WriteString("# " + l + "\n")
	}
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

func TestGolden(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			w := sc.build()
			description := w.describe(sc.mode)
			res, err := w.run(sc.mode, rollout.Next, false)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			got := transcript(description, res.lines)
			path := filepath.Join("testdata", sc.name+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(string(want), got); diff != "" {
				t.Errorf("transcript differs from %s (-want +got):\n%s", path, diff)
			}
		})
	}
}

// A run that ends with done leaves every group at its size with up to date machines only.
func TestRollsEndUpToDate(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			w := sc.build()
			res, err := w.run(sc.mode, rollout.Next, false)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res.refused {
				return
			}
			for _, g := range w.groups {
				if got := w.countOf(g.Name); got != g.Size {
					t.Errorf("group %s has %d machines, want %d", g.Name, got, g.Size)
				}
				for _, m := range w.machines {
					if m.Group == g.Name && m.SpecHash != g.SpecHash {
						t.Errorf("machine %s still has the hash %q", m.Name, m.SpecHash)
					}
				}
			}
			for _, n := range w.nodes {
				if w.machineIndex(n.owner) < 0 {
					t.Errorf("node %s of a deleted machine is still listed", n.Name)
				}
			}
		})
	}
}

// resumeProblem runs the scenario in full, then finishes a new run from the world before each of its decisions, and
// returns the first difference between the two: other lines or another world. newDecider makes the decision function
// of one run. "" means that every resumed run ended as the full run did.
func resumeProblem(sc scenario, newDecider func() decider) (string, error) {
	full := sc.build()
	res, err := full.run(sc.mode, newDecider(), true)
	if err != nil {
		return "", fmt.Errorf("full run: %w", err)
	}
	wantWorld := full.summary()
	for i, snap := range res.snapshots {
		rest, err := snap.world.run(sc.mode, newDecider(), false)
		if err != nil {
			return "", fmt.Errorf("run resumed at decision %d: %w", i, err)
		}
		if diff := cmp.Diff(res.lines[snap.line:], rest.lines); diff != "" {
			return fmt.Sprintf("run resumed at decision %d prints other lines (-full +resumed):\n%s", i, diff), nil
		}
		if diff := cmp.Diff(wantWorld, snap.world.summary()); diff != "" {
			return fmt.Sprintf("run resumed at decision %d ends in another world (-full +resumed):\n%s", i, diff), nil
		}
	}
	return "", nil
}

// A run cut after any step, or inside a wait, is finished by a new run with the rest of the same steps, in a world
// that ends as the full run's does.
func TestResumeFromEveryState(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			problem, err := resumeProblem(sc, func() decider { return rollout.Next })
			if err != nil {
				t.Fatal(err)
			}
			if problem != "" {
				t.Error(problem)
			}
		})
	}
}
