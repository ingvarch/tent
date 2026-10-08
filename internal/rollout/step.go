package rollout

import (
	"fmt"
	"time"
)

// Action is the kind of a step.
type Action int

const (
	// Done ends the run.
	Done Action = iota + 1
	// Create creates a node without waiting for its join.
	Create
	// WaitJoined waits until a machine has joined and carries its label.
	WaitJoined
	// MarkIneligible marks a client node ineligible.
	MarkIneligible
	// Drain drains a client node with a deadline and the machine's ID in the drain's meta.
	Drain
	// WaitDrained waits until a node's drain completes.
	WaitDrained
	// Delete deletes a machine.
	Delete
	// WaitNodeDown waits until Nomad lists a node as down.
	WaitNodeDown
	// Purge purges a node from Nomad.
	Purge
)

// Step is one thing that a run does next.
type Step struct {
	Action   Action
	Group    string
	Machine  Machine       // the machine it acts on; for Create the new one's name, group, role and zone
	Node     Node          // the client node it acts on
	Deadline time.Duration // Drain
}

// Waits reports whether the action polls until something happens.
func (a Action) Waits() bool {
	switch a {
	case WaitJoined, WaitDrained, WaitNodeDown:
		return true
	default:
		return false
	}
}

// String says what the step does.
func (s Step) String() string {
	m, n := s.Machine, s.Node
	switch s.Action {
	case Done:
		return "done"
	case Create:
		return fmt.Sprintf("create node %s (%s of %s, %s)", m.Name, m.Role, m.Group, m.Zone)
	case WaitJoined:
		return fmt.Sprintf("wait until node %s joins", m.Name)
	case MarkIneligible:
		return fmt.Sprintf("mark node %s ineligible", n.Name)
	case Drain:
		return fmt.Sprintf("drain node %s within %s", n.Name, s.Deadline)
	case WaitDrained:
		return fmt.Sprintf("wait until node %s is drained", n.Name)
	case Delete:
		return fmt.Sprintf("delete node %s (ID %s)", m.Name, m.ID)
	case WaitNodeDown:
		return fmt.Sprintf("wait until Nomad lists node %s at %s as down", n.Name, n.Address)
	case Purge:
		return fmt.Sprintf("purge node %s at %s from Nomad", n.Name, n.Address)
	default:
		return fmt.Sprintf("unknown action %d", s.Action)
	}
}
