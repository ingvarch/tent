package nomadops

import (
	"context"
	"net/http"
	"net/netip"

	"github.com/hashicorp/nomad/api"
)

const peersPath = "/v1/operator/raft/configuration"

// Peer is a server in the Raft configuration.
type Peer struct {
	ID string // the Raft ID, a UUID
	// Name is the server's name in Nomad, such as prod-servers-0.global; "(unknown)" when Nomad knows no server at
	// the address.
	Name    string
	Address netip.AddrPort // the Raft address; invalid when the one that Nomad gives does not parse
	Voter   bool           // the server has a vote
	Leader  bool           // the server leads
}

// FindPeer returns the peer at addr, whatever its port.
func FindPeer(peers []Peer, addr netip.Addr) (Peer, bool) {
	if !addr.IsValid() {
		return Peer{}, false
	}
	for _, p := range peers {
		if p.Address.Addr() == addr {
			return p, true
		}
	}
	return Peer{}, false
}

// Peers returns the servers of the Raft configuration, in the order that Nomad lists them. Unlike the autopilot
// report, the configuration changes as soon as a server joins or is removed. Nomad answers only a management token.
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	var conf *api.RaftConfiguration
	err := c.call(ctx, http.MethodGet, peersPath, func(ctx context.Context) error {
		var err error
		conf, err = c.api.Operator().RaftGetConfiguration(query(ctx))
		return err
	})
	if err != nil {
		return nil, err
	}
	peers := make([]Peer, 0, len(conf.Servers))
	for _, s := range conf.Servers {
		if s != nil {
			addr, _ := netip.ParseAddrPort(s.Address) // the invalid AddrPort when it does not parse
			peers = append(peers, Peer{ID: s.ID, Name: s.Node, Address: addr, Voter: s.Voter, Leader: s.Leader})
		}
	}
	return peers, nil
}
