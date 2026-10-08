package nomadops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/hashicorp/nomad/api"
)

const healthPath = "/v1/operator/autopilot/health"

// Health is autopilot's view of the servers.
type Health struct {
	Healthy bool // every server is healthy
	Voters  int  // how many servers vote in Raft
	// FailureTolerance is how many voters the cluster can lose, by autopilot.
	FailureTolerance int
	// Servers are the report's servers, in the report's order, which Nomad changes between calls: find a server by
	// its address or name.
	Servers []ServerHealth
}

// ServerHealth is autopilot's view of one server.
type ServerHealth struct {
	ID   string // the Raft ID
	Name string // as Nomad names it, <name>.<region>
	// Address is the server's Raft address; invalid when Nomad gives none that parses.
	Address netip.AddrPort
	Serf    string // Serf's status of the server, as Nomad gives it, such as alive, left or failed
	Healthy bool   // autopilot counts the server healthy
	Voter   bool   // the server votes in Raft
	Leader  bool   // the server leads
	Version string // the Nomad version the server runs
	// StableSince is when autopilot last saw the server's health change; Nomad gives whole seconds. The zero time
	// when the report has none.
	StableSince time.Time
}

// Health returns autopilot's view of the servers. An unhealthy cluster is a Health, not an error, although Nomad
// answers it with 429. A 429 that is no such report, one that names no server, voter or leader, is an error.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var reply *api.OperatorHealthReply
	err := c.call(ctx, http.MethodGet, healthPath, func(ctx context.Context) error {
		var err error
		reply, _, err = c.api.Operator().AutopilotServerHealth(query(ctx))
		var answer api.UnexpectedResponseError
		if errors.As(err, &answer) && answer.StatusCode() == http.StatusTooManyRequests {
			var unhealthy api.OperatorHealthReply
			if json.Unmarshal([]byte(answer.Body()), &unhealthy) == nil && isReport(&unhealthy) {
				reply, err = &unhealthy, nil
			}
		}
		return err
	})
	if err != nil {
		return Health{}, err
	}
	h := Health{Healthy: reply.Healthy, Voters: len(reply.Voters), FailureTolerance: reply.FailureTolerance}
	for _, sv := range reply.Servers {
		addr, _ := netip.ParseAddrPort(sv.Address) // the invalid AddrPort when it does not parse
		h.Servers = append(h.Servers, ServerHealth{ID: sv.ID, Name: sv.Name, Address: addr, Serf: sv.SerfStatus,
			Healthy: sv.Healthy, Voter: sv.Voter, Leader: sv.Leader, Version: sv.Version, StableSince: sv.StableSince})
	}
	return h, nil
}

// isReport reports whether r is autopilot's report: it names servers, voters or a leader.
func isReport(r *api.OperatorHealthReply) bool {
	return len(r.Servers) > 0 || len(r.Voters) > 0 || r.Leader != ""
}
