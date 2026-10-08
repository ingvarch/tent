// Package rollout decides how nodes are replaced and removed: Next returns the next step of a run from what the cloud
// and Nomad report. It also holds the names and zones that new nodes get and the age order of machines. It reaches no
// cloud, no Nomad and no state store.
package rollout
