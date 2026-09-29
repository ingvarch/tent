package nodeconfig

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/ingvarch/tent/api/v1alpha1"
)

// Ports that Nomad agents join the servers on.
const (
	rpcPort  = 4647 // the servers' RPC port, which clients join
	serfPort = 4648 // the servers' gossip port, which servers join
)

// RenderJoin returns 05-join.hcl, the servers that a node joins when Nomad starts, in the order given:
//   - a server joins their gossip port, 4648, in server { server_join { retry_join } };
//   - a client joins their RPC port, 4647, in client { server_join { retry_join } };
//   - a combined node joins as a server does: Nomad gives its client the RPC address of the server in the same
//     agent, and the client learns the other servers from it.
//
// It never sets retry_join in the server block itself, which Nomad 2.1 removes. An IPv6 address is written in
// brackets, and no servers give an empty list, which Nomad ignores. Every address must be valid and, with its zone,
// an HCL1 string.
func RenderJoin(role v1alpha1.Role, servers []netip.Addr) (File, error) {
	content, err := renderJoin(role, servers)
	if err != nil {
		return File{}, fmt.Errorf("05-join.hcl: %w", err)
	}
	return perNodeFile(joinPath, content), nil
}

func renderJoin(role v1alpha1.Role, servers []netip.Addr) ([]byte, error) {
	if err := checkRole(role); err != nil {
		return nil, err
	}
	block, port := "server", uint16(serfPort)
	if !role.RunsServer() {
		block, port = "client", rpcPort
	}
	var p problems
	addrs := make([]string, len(servers))
	for i, a := range servers {
		if !a.IsValid() {
			p.add(fmt.Errorf("servers[%d]: not an address", i))
		}
		addrs[i] = p.quote(fmt.Sprintf("servers[%d]", i), netip.AddrPortFrom(a, port).String())
	}
	if p.err != nil {
		return nil, p.err
	}
	return []byte(nodeHeader + block + " {\n  server_join {\n    retry_join = [" + strings.Join(addrs, ", ") +
		"]\n  }\n}\n"), nil
}
