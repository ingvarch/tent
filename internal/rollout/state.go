package rollout

import (
	"net/netip"
	"time"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// Mode is what a run does with the groups of its state.
type Mode int

const (
	// Roll replaces outdated nodes.
	Roll Mode = iota + 1
	// Shrink removes the nodes beyond a group's size.
	Shrink
)

// State is what the decisions see of a cluster at one moment.
type State struct {
	Cluster  string          // the cluster's name, the prefix of node names
	Groups   []Group         // the groups that the run handles
	Machines []Machine       // every machine of the cluster that the cloud lists
	Nomad    Nomad           // what Nomad reports
	Version  string          // the Nomad version that a new node runs, such as 2.0.7
	Forced   map[string]bool // machines to replace whatever their hash, by ID
	Refresh  time.Duration   // how often a node refreshes its list of servers
	Now      time.Time
}

// Group is a node group with its completed spec.
type Group struct {
	Name           string
	Role           v1alpha1.Role
	Size           int
	Zones          []string      // where its new nodes go
	SpecHash       string        // the hash that its nodes carry when they are up to date
	MaxSurge       int           // client groups: how many machines beyond Size may exist
	MaxUnavailable int           // client groups: how many below Size may be unavailable
	DrainTimeout   time.Duration // client and combined groups: Nomad's drain deadline
}

// Machine is a machine of the cluster as the cloud lists it.
type Machine struct {
	ID, Name, Group string
	Role            v1alpha1.Role
	Zone            string
	SpecHash        string
	PrivateIP       netip.Addr // invalid until the cloud reports one
	Ready           bool       // the cloud reports it running
	Joined          bool       // it carries tent/joined=true
	Created         time.Time  // zero when the cloud gives none
}

// Nomad is what Nomad reports about the cluster.
type Nomad struct {
	Healthy          bool     // autopilot reports every server healthy
	FailureTolerance int      // how many voters the cluster can lose, by autopilot
	Servers          []Server // the Raft configuration, each with autopilot's view of it
	Members          []Member // the servers' gossip pool
	Nodes            []Node   // the client nodes; a name can appear twice
}

// Server is a server of the Raft configuration.
type Server struct {
	ID          string         // the Raft ID
	Name        string         // <node name>.<region>
	Address     netip.AddrPort // the Raft address: the private address, port 4647
	Voter       bool
	Leader      bool
	Healthy     bool      // autopilot counts it healthy; false when its report lacks the server
	StableSince time.Time // when autopilot last saw its health change; a new leader resets it
	Version     string    // the Nomad version it runs; empty when the report lacks the server
}

// Member is a server in the gossip pool.
type Member struct {
	Name    string // <node name>.<region>
	Address netip.Addr
	Status  string // alive, leaving, left or failed
}

// Node is a client node as Nomad lists it.
type Node struct {
	ID         string
	Name       string
	Address    netip.Addr
	Status     string // initializing, ready, down or disconnected
	Eligible   bool
	Draining   bool   // a drain is under way
	DrainedFor string // the machine ID in the meta of the node's last drain, once that drain completed
	Version    string
}
