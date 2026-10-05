package nodeup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"path"
	"slices"

	"github.com/ingvarch/tent/internal/nodeconfig"
)

// peersFile keeps the addresses of the servers that last answered, so that a refresh and the next boot ask them
// before the seed: a replaced server group leaves the seed dead.
const peersFile = "/var/lib/tent/peers.json"

// join writes 05-join.hcl: from the live set of the servers that answer, when one does, and from the seed and the
// last known set otherwise, so that Nomad finds the current servers at its next start.
func join(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (Result, error) {
	servers := knownServers(h, nc)
	answered := false
	ask, err := hasTLSFiles(h)
	if err != nil {
		return Result{}, err
	}
	if ask && len(servers) > 0 {
		if peers, _ := askServers(ctx, h, nc, servers); peers != nil {
			servers, answered = peers, true
		}
	}
	changed, err := writeJoin(h, nc, servers)
	if err != nil {
		return Result{}, err
	}
	if answered {
		c, err := storePeers(h, servers)
		changed = changed || c
		if err != nil {
			return Result{}, err
		}
	}
	if changed {
		return Result{Status: Done}, nil
	}
	return Result{Status: Unchanged}, nil
}

// RefreshJoin keeps 05-join.hcl current while the node runs, at each run of tent-node-join.timer: it asks for the
// servers' peers, as the join phase does, and when one answers, writes 05-join.hcl for them and keeps them in the
// peers file. On a role that runs a server it asks the node's own agent first: the cluster's first server knows no
// other server, and its agent knows the peers. Without the node's TLS files, a server to ask or an answer it changes
// nothing, and the next refresh tries again. When ctx ends before an answer, it stops asking and fails with ctx's
// cause. It never restarts Nomad: Nomad reads retry_join only at start, and needs the file for nothing else.
func RefreshJoin(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) error {
	known := knownServers(h, nc)
	ask, err := hasTLSFiles(h)
	if err != nil {
		return fmt.Errorf("refresh 05-join.hcl: %w", err)
	}
	if !ask {
		h.logger().Info("no TLS files yet; 05-join.hcl stays until the next refresh")
		return nil
	}
	servers := known
	if nc.Role.RunsServer() {
		servers = append([]netip.Addr{localAgent}, known...)
	}
	if len(servers) == 0 {
		h.logger().Info("no server is known; 05-join.hcl stays until the next refresh")
		return nil
	}
	peers, asked := askServers(ctx, h, nc, servers)
	switch {
	case peers != nil:
	case ctx.Err() != nil:
		return fmt.Errorf("refresh 05-join.hcl: %w", context.Cause(ctx))
	case !asked: // askServers has warned why
		return nil
	default:
		h.logger().Info("no server answered; 05-join.hcl stays until the next refresh", "asked", len(servers))
		return nil
	}
	changed, err := writeJoin(h, nc, peers)
	if err != nil {
		return fmt.Errorf("refresh 05-join.hcl: %w", err)
	}
	if _, err := storePeers(h, peers); err != nil {
		return fmt.Errorf("refresh 05-join.hcl: %w", err)
	}
	if changed {
		h.logger().Info("05-join.hcl joins the servers that answered", "known", len(known), "servers", len(peers),
			"peers", peers)
	}
	return nil
}

// writeJoin writes 05-join.hcl for the servers, and reports whether that changed the file or its directory.
func writeJoin(h *Host, nc *nodeconfig.NodeConfig, servers []netip.Addr) (bool, error) {
	f, err := nodeconfig.RenderJoin(nc.Role, servers)
	if err != nil {
		return false, err
	}
	changed, err := h.FS.EnsureDir(path.Dir(f.Path), 0o755, f.Owner)
	if err != nil {
		return false, err
	}
	c, err := h.FS.WriteFile(f.Path, f.Content, fs.FileMode(f.Mode), f.Owner)
	return changed || c, err
}

// knownServers returns the last known servers and then the seed, deduplicated. A peers file that is missing or
// corrupt leaves the seed.
func knownServers(h *Host, nc *nodeconfig.NodeConfig) []netip.Addr {
	var servers []netip.Addr
	data, err := h.FS.ReadFile(peersFile)
	if err == nil {
		var peers []string
		if err := json.Unmarshal(data, &peers); err == nil {
			for _, p := range peers {
				a, err := netip.ParseAddr(p)
				if err != nil {
					h.logger().Warn("the peers file has an entry that is not an address; it is left out",
						"file", peersFile, "entry", excerpt([]byte(p)))
					continue
				}
				servers = append(servers, a)
			}
		} else {
			h.logger().Warn("the peers file is corrupt; the seed is used", "file", peersFile)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		h.logger().Warn("the peers file cannot be read; the seed is used", "file", peersFile)
	}
	servers = append(servers, nc.Join.Servers...)
	seen := make(map[netip.Addr]bool, len(servers))
	return slices.DeleteFunc(servers, func(a netip.Addr) bool {
		if seen[a] {
			return true
		}
		seen[a] = true
		return false
	})
}

// hasTLSFiles reports whether the node's TLS files are written yet: on the first boot join runs before the phase
// that writes them, and then only the seed is rendered.
func hasTLSFiles(h *Host) (bool, error) {
	for _, p := range []string{nodeconfig.CAFile, nodeconfig.CertFile, nodeconfig.KeyFile} {
		_, err := h.FS.Stat(p)
		switch {
		case err == nil:
		case errors.Is(err, fs.ErrNotExist):
			return false, nil
		default:
			return false, fmt.Errorf("stat %s: %w", p, err)
		}
	}
	return true, nil
}

// askServers asks each of the servers for its peers, first answer wins, and returns the answer, or nil when none
// answers. The answer's addresses, without their ports, are sorted, so that a same set in another order changes
// nothing. asked reports whether it could ask: without an mTLS client it warns and asks nobody. When ctx ends, it
// stops asking.
func askServers(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig, servers []netip.Addr) (
	peers []netip.Addr, asked bool,
) {
	client, err := newAPIClient(h, "server."+nc.Region+".nomad")
	if err != nil {
		h.logger().Warn("no mTLS client of the Nomad API; the known servers stay", "error", err)
		return nil, false
	}
	defer client.close()
	for _, s := range servers {
		url := "https://" + netip.AddrPortFrom(s, apiPort).String() + "/v1/status/peers?stale"
		code, body, err := client.get(ctx, url)
		if err != nil && ctx.Err() != nil {
			return nil, true
		}
		if err != nil || code != http.StatusOK {
			attrs := []any{"server", s, "status", code}
			if err != nil {
				attrs = append(attrs, "error", withoutURL(err))
			}
			h.logger().Warn("a server did not answer", attrs...)
			continue
		}
		var list []string
		if err := json.Unmarshal(body, &list); err != nil {
			h.logger().Warn("a server's peers do not parse", "server", s, "error", err)
			continue
		}
		answer, err := peerAddrs(list)
		if err != nil {
			h.logger().Warn("a server's peers do not parse", "server", s, "error", err)
			continue
		}
		// A server answers [] before the bootstrap; an empty set would replace the known servers with none, so it is no
		// answer.
		if len(answer) == 0 {
			h.logger().Warn("a server answered with no peers", "server", s)
			continue
		}
		slices.SortFunc(answer, netip.Addr.Compare)
		return answer, true
	}
	return nil, true
}

// peerAddrs parses the addresses of a peers answer, such as ["10.0.0.5:4647"], without their ports.
func peerAddrs(list []string) ([]netip.Addr, error) {
	peers := make([]netip.Addr, 0, len(list))
	seen := make(map[netip.Addr]bool, len(list))
	for _, p := range list {
		ap, err := netip.ParseAddrPort(p)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address with a port", p)
		}
		if !seen[ap.Addr()] {
			seen[ap.Addr()] = true
			peers = append(peers, ap.Addr())
		}
	}
	return peers, nil
}

// storePeers writes the servers that answered into the peers file and reports whether it changed.
func storePeers(h *Host, servers []netip.Addr) (bool, error) {
	names := make([]string, len(servers))
	for i, s := range servers {
		names[i] = s.String()
	}
	data, err := json.Marshal(names)
	if err != nil {
		return false, fmt.Errorf("write the peers: %w", err)
	}
	changed, err := h.FS.EnsureDir(path.Dir(peersFile), 0o700, nodeconfig.Owner)
	if err != nil {
		return false, err
	}
	c, err := h.FS.WriteFile(peersFile, data, 0o600, nodeconfig.Owner)
	if err != nil {
		return changed, err
	}
	return changed || c, nil
}
