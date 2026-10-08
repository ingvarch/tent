package rollout

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"golang.org/x/mod/semver"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/english"
)

// ErrRefused matches the errors of runs that must not go on.
var ErrRefused = errors.New("refused")

// refusal is an error that matches ErrRefused and says only why.
type refusal string

func (r refusal) Error() string { return string(r) }

func (r refusal) Is(target error) bool { return target == ErrRefused }

func refuse(format string, args ...any) error { return refusal(fmt.Sprintf(format, args...)) }

// Next returns the next step of a run, Done when nothing is left, or an error that matches ErrRefused when the run
// must not go on. It reads only s, so a run that was cut is finished by calling it again on what the cloud and Nomad
// report.
func Next(s State, mode Mode) (Step, error) {
	switch mode {
	case Roll:
	case Shrink:
		return Step{}, errors.New("rollout cannot shrink yet")
	default:
		return Step{}, fmt.Errorf("rollout: unknown mode %d", mode)
	}
	if err := checkVersions(s); err != nil {
		return Step{}, err
	}
	groups := sortedGroups(s.Groups)
	for _, g := range groups {
		if g.Role != v1alpha1.RoleClient {
			return Step{}, errors.New("rollout cannot roll server groups yet")
		}
	}
	if err := checkDuplicates(s, groups); err != nil {
		return Step{}, err
	}
	for _, g := range groups {
		step, found, err := nextClient(s, g)
		if err != nil || found {
			return step, err
		}
	}
	return Step{Action: Done}, nil
}

// sortedGroups returns a copy of the groups ordered by name.
func sortedGroups(groups []Group) []Group {
	sorted := slices.Clone(groups)
	slices.SortStableFunc(sorted, func(a, b Group) int { return cmp.Compare(a.Name, b.Name) })
	return sorted
}

// machinesOf returns the listed machines of the group, ordered by name and then ID.
func machinesOf(s State, g Group) []Machine {
	var ms []Machine
	for _, m := range s.Machines {
		if m.Group == g.Name {
			ms = append(ms, m)
		}
	}
	slices.SortStableFunc(ms, func(a, b Machine) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return ms
}

// checkDuplicates refuses a group that lists two machines of one name.
func checkDuplicates(s State, groups []Group) error {
	for _, g := range groups {
		ms := machinesOf(s, g)
		for i := 0; i < len(ms); {
			j := i + 1
			for j < len(ms) && ms[j].Name == ms[i].Name {
				j++
			}
			if j-i > 1 {
				ids := make([]string, 0, j-i)
				for _, m := range ms[i:j] {
					ids = append(ids, m.ID)
				}
				return refuse("node group %s: machines %s share the name %s; run tent update cluster first",
					g.Name, english.And(ids), ms[i].Name)
			}
			i = j
		}
	}
	return nil
}

// checkVersions refuses a version that is not a version number, and a new node that would run an older Nomad than
// a server or a node that is not down.
func checkVersions(s State) error {
	if err := checkVersion("a new node", s.Version); err != nil {
		return err
	}
	for _, srv := range s.Nomad.Servers {
		if err := checkNotNewer(s.Version, "server "+srv.Name, srv.Version); err != nil {
			return err
		}
	}
	for _, n := range s.Nomad.Nodes {
		if n.Status == nodeDown {
			continue
		}
		if err := checkNotNewer(s.Version, "node "+n.Name, n.Version); err != nil {
			return err
		}
	}
	return nil
}

func checkVersion(owner, version string) error {
	if !semver.IsValid("v" + version) {
		return refuse("the Nomad version of %s is %q, which is not a version number", owner, version)
	}
	return nil
}

func checkNotNewer(pinned, owner, version string) error {
	if err := checkVersion(owner, version); err != nil {
		return err
	}
	if semver.Compare("v"+version, "v"+pinned) > 0 {
		return refuse("tent never moves a node to an older Nomad: the cluster is pinned to %s, and %s runs %s",
			pinned, owner, version)
	}
	return nil
}
