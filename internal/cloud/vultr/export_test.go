package vultr

import (
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/engine"
)

// WithOpIDs makes the provider take its operation ids from next, for tests.
var WithOpIDs = withOpIDs

// WithPollInterval sets how long the provider waits between two reads of an instance that is not ready, for tests.
var WithPollInterval = withPollInterval

// Snapshot is the type of the snapshots that Inventory returns, for tests.
type Snapshot = snapshot

// SSHKey returns the SSH key that the snapshot keeps for k, for tests.
func (s *snapshot) SSHKey(k engine.Key) (govultr.SSHKey, bool) { return s.sshKey(k) }

// VPC returns the VPC that the snapshot keeps for k, for tests.
func (s *snapshot) VPC(k engine.Key) (govultr.VPC, bool) { return s.vpc(k) }

// FirewallGroup returns the firewall group that the snapshot keeps for k, for tests.
func (s *snapshot) FirewallGroup(k engine.Key) (govultr.FirewallGroup, bool) {
	return s.firewallGroup(k)
}

// FirewallRules returns the rules of a firewall group that the snapshot keeps, for tests.
func (s *snapshot) FirewallRules(groupID string) []govultr.FirewallRule {
	return s.firewallRules(groupID)
}
