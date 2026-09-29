package nomadops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/hashicorp/nomad/api"
)

const healthPath = "/v1/operator/autopilot/health"

// Health is autopilot's view of the servers.
type Health struct {
	Healthy bool // every server is healthy
	Voters  int  // how many servers vote in Raft
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
	return Health{Healthy: reply.Healthy, Voters: len(reply.Voters)}, nil
}

// isReport reports whether r is autopilot's report: it names servers, voters or a leader.
func isReport(r *api.OperatorHealthReply) bool {
	return len(r.Servers) > 0 || len(r.Voters) > 0 || r.Leader != ""
}
