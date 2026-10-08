package nomadops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/hashicorp/nomad/api"
)

const (
	membersPath    = "/v1/agent/members"
	forceLeavePath = "/v1/agent/force-leave"
)

// Member is a server in the gossip pool, as the server that answers sees it.
type Member struct {
	Name    string     // <node name>.<region>, the name that ForceLeave takes
	Address netip.Addr // the member's Serf address; invalid when Nomad gives none that parses
	Status  string     // alive, leaving, left or failed
}

// CheckMemberName checks that name is fit for ForceLeave: Nomad names a member <node name>.<region>, and answers 200
// to a name without its region while it changes nothing. It accepts a name that holds a ".".
func CheckMemberName(name string) error {
	switch {
	case name == "":
		return errors.New("no member name")
	case !strings.Contains(name, "."):
		return fmt.Errorf("member name %q has no \".\"", name)
	}
	return nil
}

// Members returns the servers of the gossip pool as the server that answers sees it, in the order that it lists them.
// Clients are not in the pool.
func (c *Client) Members(ctx context.Context) ([]Member, error) {
	var answer *api.ServerMembers
	err := c.call(ctx, http.MethodGet, membersPath, func(ctx context.Context) error {
		var err error
		answer, err = c.api.Agent().MembersOpts(query(ctx))
		return err
	})
	if err != nil {
		return nil, err
	}
	if answer == nil { // a null answer
		return []Member{}, nil
	}
	members := make([]Member, 0, len(answer.Members))
	for _, m := range answer.Members {
		if m != nil {
			addr, _ := netip.ParseAddr(m.Addr) // the invalid Addr when it does not parse
			members = append(members, Member{Name: m.Name, Address: addr, Status: m.Status})
		}
	}
	return members, nil
}

// ForceLeave forces the member called name, <node name>.<region>, out of the gossip pool and prunes it, so that Nomad
// forgets it. Nomad answers 200 to any name, a member that is not there included. The server tells the other servers
// by gossip. It checks the name before it sends a request.
func (c *Client) ForceLeave(ctx context.Context, name string) error {
	if err := CheckMemberName(name); err != nil {
		return fmt.Errorf("nomad: %w", err)
	}
	endpoint := forceLeavePath + "?" + url.Values{"node": {name}, "prune": {"1"}}.Encode()
	return c.call(ctx, http.MethodPut, forceLeavePath, func(ctx context.Context) error {
		// Not Agent().ForceLeaveWithOptions, which takes no options of the request and so no context.
		_, err := c.api.Raw().Write(endpoint, nil, nil, write(ctx))
		return err
	})
}
