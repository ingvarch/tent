// Package rollout decides how nodes are replaced and removed. It holds the names and zones that new nodes get and the
// age order of machines, as pure functions of what the cloud reports. It reaches no cloud, no Nomad and no state
// store.
package rollout
