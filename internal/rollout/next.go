package rollout

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

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
// report. A roll replaces outdated machines and never moves a node to an older Nomad. A shrink removes the machines
// beyond the size of a group, creates none and leaves the Nomad versions alone.
func Next(s State, mode Mode) (Step, error) {
	if mode != Roll && mode != Shrink {
		return Step{}, fmt.Errorf("rollout: unknown mode %d", mode)
	}
	if mode == Roll {
		if _, err := CheckNewNode(s.Nomad, s.Version, false); err != nil {
			return Step{}, err
		}
	}
	groups := sortedGroups(s.Groups)
	next := map[v1alpha1.Role]func(State, Mode, Group) (Step, bool, error){
		v1alpha1.RoleServer:   nextServer,
		v1alpha1.RoleCombined: nextServer,
		v1alpha1.RoleClient:   nextClient,
	}
	for _, g := range groups {
		if next[g.Role] == nil {
			return Step{}, fmt.Errorf("rollout: unknown role %q", g.Role)
		}
	}
	if err := checkDuplicates(s, groups, mode); err != nil {
		return Step{}, err
	}
	if err := checkRoles(s, groups); err != nil {
		return Step{}, err
	}
	for _, g := range groups {
		step, found, err := next[g.Role](s, mode, g)
		if err != nil || found {
			return step, err
		}
	}
	return Step{Action: Done}, nil
}

// sortedGroups returns a copy of the groups: the server and combined groups first, then the client groups, each by
// name.
func sortedGroups(groups []Group) []Group {
	sorted := slices.Clone(groups)
	slices.SortStableFunc(sorted, func(a, b Group) int {
		return cmp.Or(
			compareBool(a.Role == v1alpha1.RoleClient, b.Role == v1alpha1.RoleClient),
			cmp.Compare(a.Name, b.Name),
		)
	})
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

// checkDuplicates refuses a group that lists two machines of one name. A roll tells the user to run update first; a
// shrink is update itself, which deals with such machines before it asks for a step, so it asks to run it again.
func checkDuplicates(s State, groups []Group, mode Mode) error {
	advice := "first"
	if mode == Shrink {
		advice = "again"
	}
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
				return refuse("node group %s: machines %s share the name %s; run tent update cluster %s",
					g.Name, english.And(ids), ms[i].Name, advice)
			}
			i = j
		}
	}
	return nil
}

// RoleLabel is the name of the label that holds the role of a machine, as a refusal tells a user. It must equal
// cloud.LabelRole, which defines the label; internal/rollout cannot import internal/cloud, so a test in internal/app
// checks that the two agree.
const RoleLabel = "tent/role"

// checkRoles refuses a group that lists a machine of another role than the group's: the rules of a group go by its
// role, so a machine that runs a client in a server group would stop without a drain. It names the first such
// machine of the first such group.
func checkRoles(s State, groups []Group) error {
	for _, g := range groups {
		for _, m := range machinesOf(s, g) {
			if m.Role != g.Role {
				created, advice := createdAs(m.Role)
				return refuse("node group %s: node %s was created %s and the group is now %s; tent does not replace "+
					"or remove the nodes of a group whose role changed; %s", g.Name, m.Name, created, g.Role, advice)
			}
		}
	}
	return nil
}

// createdAs says how a refusal names the role a machine was created with, and what to do about it. A group whose
// role changed cannot get its old role back, since tent refuses every change of a stored group's role. A machine with
// no role or an unknown one has a label to fix; the group's role cannot match it.
func createdAs(role v1alpha1.Role) (created, advice string) {
	const ofGroup = "a node group keeps its role, and a cluster moves by creating a new one"
	switch role {
	case v1alpha1.RoleServer:
		return "as a server", ofGroup
	case v1alpha1.RoleCombined:
		return "as a combined node", ofGroup
	case v1alpha1.RoleClient:
		return "as a client", ofGroup
	}
	advice = "give the machine the label " + RoleLabel + " with the role that it runs"
	if role == "" {
		return "without a role", advice
	}
	return fmt.Sprintf("with the role %q", role), advice
}
