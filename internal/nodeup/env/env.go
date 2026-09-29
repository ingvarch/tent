// Package env describes the machine that tent-node runs on as its cloud's metadata service sees it. Each cloud has its
// own Environment.
package env

import (
	"context"
	"net/netip"
)

// Environment is a cloud's metadata service, as the machine sees it.
type Environment interface {
	// Read asks the metadata service about the machine. Each call reads the service anew, so tent-node calls it once
	// per run.
	Read(ctx context.Context) (Instance, error)
}

// Instance is the machine as its cloud describes it. Its JSON leaves out what the cloud did not report.
type Instance struct {
	ID        string     `json:"id,omitempty"`       // the cloud's id of the machine, as its API names it
	Zone      string     `json:"zone,omitempty"`     // in lower case, such as ams
	PrivateIP netip.Addr `json:"privateIP,omitzero"` // the IPv4 address on the cluster's private network
}
