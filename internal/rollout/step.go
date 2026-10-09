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
	// TransferLeadership moves the Raft leadership to a server.
	TransferLeadership
	// Stop stops a machine.
	Stop
	// WaitServerDown waits until autopilot no longer counts a server as a healthy voter.
	WaitServerDown
	// RemovePeer removes a server from the Raft configuration.
	RemovePeer
	// ForceLeave forces a member out of the gossip pool.
	ForceLeave
	// WaitHealthy waits until the servers are healthy and a number of them vote.
	WaitHealthy
	// WaitStable waits until a time, so that every node has refreshed its list of servers.
	WaitStable
)

// Step is one thing that a run does next. The WaitStable and WaitHealthy of a server's removal name the server's
// machine as their Machine.
type Step struct {
	Action   Action
	Group    string
	Machine  Machine       // the machine it acts on; for Create the new one's name, group, role and zone
	Node     Node          // the client node it acts on
	Server   Server        // RemovePeer: the peer; TransferLeadership: the new leader
	Member   Member        // ForceLeave
	Voters   int           // WaitHealthy
	Deadline time.Duration // Drain
	Until    time.Time     // WaitStable
}

// Waits reports whether the action polls until something happens.
func (a Action) Waits() bool {
	switch a {
	case WaitJoined, WaitDrained, WaitNodeDown, WaitServerDown, WaitHealthy, WaitStable:
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
	case TransferLeadership:
		return fmt.Sprintf("move the leadership from %s to %s", m.Name, nodeOfServer(s.Server.Name))
	case Stop:
		return fmt.Sprintf("stop node %s (ID %s)", m.Name, m.ID)
	case WaitServerDown:
		return fmt.Sprintf("wait until autopilot no longer counts %s as a healthy voter", m.Name)
	case RemovePeer:
		return fmt.Sprintf("remove %s from the Raft configuration", m.Name)
	case ForceLeave:
		return fmt.Sprintf("force %s out of the gossip pool", s.Member.Name)
	case WaitHealthy:
		return fmt.Sprintf("wait until %d healthy servers vote", s.Voters)
	case WaitStable:
		return fmt.Sprintf("wait until %s for the servers to be stable", s.Until.UTC().Format(time.TimeOnly))
	default:
		return fmt.Sprintf("unknown action %d", s.Action)
	}
}
