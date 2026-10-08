package nomadops

import (
	"context"
	"fmt"
	"net/http"
)

const (
	transferLeadershipPath = "/v1/operator/raft/transfer-leadership"
	removePeerPath         = "/v1/operator/raft/peer"
)

// CheckRaftID checks that id is fit for the query of a request: Raft IDs are UUIDs. It accepts ASCII letters, digits
// and "-".
func CheckRaftID(id string) error { return checkID("Raft", id) }

// raftGone returns err when it is what Nomad answers with status for a Raft ID that is not in the Raft configuration,
// and nil otherwise: the message names the ID that was sent, and a follower adds "rpc error: ".
func raftGone(err error, status int, raftID string) *callError {
	return answeredGone(err, status, fmt.Sprintf("id %q was not found in the Raft configuration", raftID))
}

// TransferLeadership asks the leader to hand the leadership to the server with the Raft ID. A 200 does not show that
// the server leads: Nomad answers it for a server that does not vote, and another server then takes the leadership.
// When the server leads already, Nomad answers 200 and does nothing. It fails with ErrGone when the Raft configuration
// has no server with the ID. It checks the ID before it sends a request.
func (c *Client) TransferLeadership(ctx context.Context, raftID string) error {
	if err := CheckRaftID(raftID); err != nil {
		return fmt.Errorf("nomad: %w", err)
	}
	err := c.call(ctx, http.MethodPut, transferLeadershipPath, func(ctx context.Context) error {
		return c.api.Operator().RaftTransferLeadershipByID(raftID, write(ctx))
	})
	if e := raftGone(err, http.StatusBadRequest, raftID); e != nil {
		return asGone(e)
	}
	return err
}

// RemovePeer removes the server with the Raft ID from the Raft configuration. A server that is not in the
// configuration counts as removed. It checks the ID before it sends a request.
func (c *Client) RemovePeer(ctx context.Context, raftID string) error {
	if err := CheckRaftID(raftID); err != nil {
		return fmt.Errorf("nomad: %w", err)
	}
	err := c.call(ctx, http.MethodDelete, removePeerPath, func(ctx context.Context) error {
		return c.api.Operator().RaftRemovePeerByID(raftID, write(ctx))
	})
	if raftGone(err, http.StatusInternalServerError, raftID) != nil {
		return nil
	}
	return err
}
