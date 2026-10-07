// Package janitortest has a fake of the Vultr API for the tests of the janitor and its command.
package janitortest

import (
	"context"
	"slices"
	"strings"

	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

// API is an in-memory Vultr account. Deletes remove the object. Calls records the API calls in order, such as
// "list-instances" and "delete-vpc v1"; Events records the same calls and the lines written to Writer, as
// "out: <line>".
type API struct {
	Machines []vultrapi.Instance
	Nets     []vultrapi.VPC
	Groups   []vultrapi.FirewallGroup
	Keys     []vultrapi.SSHKey

	// Err maps a call, as recorded in Calls, to the error that call returns. The call is still recorded.
	Err map[string]error
	// Refuse maps a call to the number of times it returns its Err before it succeeds. A call in Refuse
	// ignores the plain Err once its count is used up.
	Refuse map[string]int
	// Linger maps an instance ID to the number of lists that still show the instance after its delete.
	Linger map[string]int

	Calls  []string
	Events []string

	dying map[string]int
}

// Writer returns a writer that records each write, without its final newline, in Events.
func (a *API) Writer() *Writer { return &Writer{api: a} }

// Writer records what is written to it in the Events of its API.
type Writer struct{ api *API }

// Write records p as one event.
func (w *Writer) Write(p []byte) (int, error) {
	w.api.Events = append(w.api.Events, "out: "+strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

func (a *API) record(call string) error {
	a.Calls = append(a.Calls, call)
	a.Events = append(a.Events, call)
	if left, ok := a.Refuse[call]; ok {
		if left == 0 {
			return nil
		}
		a.Refuse[call] = left - 1
	}
	return a.Err[call]
}

// Instances lists the instances.
func (a *API) Instances(context.Context) ([]vultrapi.Instance, error) {
	if err := a.record("list-instances"); err != nil {
		return nil, err
	}
	var listed []vultrapi.Instance
	for _, in := range slices.Clone(a.Machines) {
		left, isDying := a.dying[in.ID]
		if isDying && left == 0 {
			a.Machines = slices.DeleteFunc(a.Machines, func(i vultrapi.Instance) bool { return i.ID == in.ID })
			delete(a.dying, in.ID)
			continue
		}
		if isDying {
			a.dying[in.ID]--
		}
		listed = append(listed, in)
	}
	return listed, nil
}

// VPCs lists the VPCs.
func (a *API) VPCs(context.Context) ([]vultrapi.VPC, error) {
	if err := a.record("list-vpcs"); err != nil {
		return nil, err
	}
	return slices.Clone(a.Nets), nil
}

// FirewallGroups lists the firewall groups.
func (a *API) FirewallGroups(context.Context) ([]vultrapi.FirewallGroup, error) {
	if err := a.record("list-firewalls"); err != nil {
		return nil, err
	}
	return slices.Clone(a.Groups), nil
}

// SSHKeys lists the SSH keys.
func (a *API) SSHKeys(context.Context) ([]vultrapi.SSHKey, error) {
	if err := a.record("list-ssh-keys"); err != nil {
		return nil, err
	}
	return slices.Clone(a.Keys), nil
}

// DeleteInstance deletes an instance; it stays listed for Linger[id] more lists.
func (a *API) DeleteInstance(_ context.Context, id string) error {
	if err := a.record("delete-instance " + id); err != nil {
		return err
	}
	if n := a.Linger[id]; n > 0 {
		if a.dying == nil {
			a.dying = map[string]int{}
		}
		a.dying[id] = n
		return nil
	}
	a.Machines = slices.DeleteFunc(a.Machines, func(i vultrapi.Instance) bool { return i.ID == id })
	return nil
}

// DeleteVPC deletes a VPC.
func (a *API) DeleteVPC(_ context.Context, id string) error {
	if err := a.record("delete-vpc " + id); err != nil {
		return err
	}
	a.Nets = slices.DeleteFunc(a.Nets, func(v vultrapi.VPC) bool { return v.ID == id })
	return nil
}

// DeleteFirewallGroup deletes a firewall group.
func (a *API) DeleteFirewallGroup(_ context.Context, id string) error {
	if err := a.record("delete-firewall " + id); err != nil {
		return err
	}
	a.Groups = slices.DeleteFunc(a.Groups, func(g vultrapi.FirewallGroup) bool { return g.ID == id })
	return nil
}

// DeleteSSHKey deletes an SSH key.
func (a *API) DeleteSSHKey(_ context.Context, id string) error {
	if err := a.record("delete-ssh-key " + id); err != nil {
		return err
	}
	a.Keys = slices.DeleteFunc(a.Keys, func(k vultrapi.SSHKey) bool { return k.ID == id })
	return nil
}
