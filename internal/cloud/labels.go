// Package cloud holds what tent's core and its cloud providers share without depending on any one cloud, such as the
// canonical labels that mark the objects tent owns.
package cloud

// LabelPrefix starts the key of every canonical label.
const LabelPrefix = "tent/"

// Canonical labels. Each provider stores them in its own way: Hetzner as native labels, Vultr as instance tags.
const (
	// LabelCluster holds the cluster name. It is the only signal that an object belongs to a cluster.
	LabelCluster = "tent/cluster"
	// LabelNodeGroup holds the node group name of a machine or a placement group.
	LabelNodeGroup = "tent/nodegroup"
	// LabelRole holds the Nomad role of a machine: server, client or combined.
	LabelRole = "tent/role"
	// LabelSpecHash holds the hash of the node configuration a machine was created with, 16 lower-case hex
	// characters.
	LabelSpecHash = "tent/spec-hash"
	// LabelSlot holds the slot of a Hetzner Nomad server, 0 to 6.
	LabelSlot = "tent/slot"
	// LabelOp holds the operation id, a lower-case UUID, of the call that created the object.
	LabelOp = "tent/op"
	// LabelLockFor holds the cluster name on the Hetzner lock firewall, which has no LabelCluster.
	LabelLockFor = "tent/lock-for"
	// LabelJoined is "true" on a machine whose node has joined its cluster. Where the cloud lets user data change, the
	// user data holds no secrets any more.
	LabelJoined = "tent/joined"
	// LabelReplace is "true" on a machine that a forced rolling update replaces, whatever its spec hash.
	LabelReplace = "tent/replace"
)

// Labels maps canonical label keys to their values.
type Labels map[string]string
