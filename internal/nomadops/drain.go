package nomadops

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// DrainRequest asks for the drain of a node.
type DrainRequest struct {
	Deadline time.Duration     // above zero: Nomad stops the allocations that remain after it
	Meta     map[string]string // kept in the node's last drain
}

// Check checks that the deadline is above zero: a deadline of 0 is a drain without a deadline, and a negative one
// stops every allocation at once.
func (r DrainRequest) Check() error {
	if r.Deadline <= 0 {
		return fmt.Errorf("drain: deadline %s is not above zero", r.Deadline)
	}
	return nil
}

// CheckNodeID checks that id is fit for the path of a request: Nomad's IDs are UUIDs, and the Nomad API module puts
// an ID into the path as it is. It accepts ASCII letters, digits and "-".
func CheckNodeID(id string) error { return checkID("node", id) }

// checkID checks an ID of the kind, such as "node", for CheckNodeID and CheckRaftID.
func checkID(kind, id string) error {
	switch {
	case id == "":
		return fmt.Errorf("no %s ID", kind)
	case strings.ContainsFunc(id, func(r rune) bool { return !isIDChar(r) }):
		return fmt.Errorf("%s ID %q has a character other than an ASCII letter, a digit or \"-\"", kind, id)
	}
	return nil
}

// isIDChar reports whether r is an ASCII letter, a digit or "-".
func isIDChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
}

// nodeWritePath returns the path of a write on the node, such as /v1/node/<id>/drain.
func nodeWritePath(id, action string) string { return "/v1/node/" + id + "/" + action }

// nodeGone returns err when it is what Nomad answers for a node that is not in the cluster, and nil otherwise: the
// leader says "node not found", a follower adds "rpc error: ".
func nodeGone(err error) *callError {
	return answeredGone(err, http.StatusInternalServerError, "node not found")
}

// MarkIneligible marks the client node ineligible for new work; a node that is ineligible stays so. It fails with
// ErrGone when the node is not in the cluster. It checks the ID before it sends a request.
func (c *Client) MarkIneligible(ctx context.Context, nodeID string) error {
	if err := CheckNodeID(nodeID); err != nil {
		return fmt.Errorf("nomad: %w", err)
	}
	path := nodeWritePath(nodeID, "eligibility")
	err := c.call(ctx, http.MethodPut, path, func(ctx context.Context) error {
		_, err := c.api.Nodes().ToggleEligibility(nodeID, false, write(ctx))
		return err
	})
	if e := nodeGone(err); e != nil {
		return asGone(e)
	}
	return err
}

// Drain drains the client node: Nomad marks it ineligible, moves its allocations, those of system jobs too, and stops
// the ones that remain after req.Deadline. req.Meta stays in the node's last drain. Asked again while the node drains,
// Nomad moves the deadline to the time of the new request plus req.Deadline. It fails with ErrGone when the node is
// not in the cluster. It checks the ID and req before it sends a request.
func (c *Client) Drain(ctx context.Context, nodeID string, req DrainRequest) error {
	if err := CheckNodeID(nodeID); err != nil {
		return fmt.Errorf("nomad: %w", err)
	}
	if err := req.Check(); err != nil {
		return fmt.Errorf("nomad: %w", err)
	}
	path := nodeWritePath(nodeID, "drain")
	err := c.call(ctx, http.MethodPut, path, func(ctx context.Context) error {
		_, err := c.api.Nodes().UpdateDrainOpts(nodeID, &api.DrainOptions{
			DrainSpec: &api.DrainSpec{Deadline: req.Deadline}, Meta: req.Meta,
		}, write(ctx))
		return err
	})
	if e := nodeGone(err); e != nil {
		return asGone(e)
	}
	return err
}

// Purge removes the client node from Nomad, whether it is up or down. A node that is not in the cluster counts as
// purged. It checks the ID before it sends a request.
func (c *Client) Purge(ctx context.Context, nodeID string) error {
	if err := CheckNodeID(nodeID); err != nil {
		return fmt.Errorf("nomad: %w", err)
	}
	err := c.call(ctx, http.MethodPut, nodeWritePath(nodeID, "purge"), func(ctx context.Context) error {
		_, _, err := c.api.Nodes().Purge(nodeID, query(ctx))
		return err
	})
	if nodeGone(err) != nil {
		return nil
	}
	return err
}
