package nodeup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup/env"
)

// FirewallFile is where the hostfirewall phase writes tent's nftables ruleset, which it loads with nft -f.
const FirewallFile = "/etc/tent/firewall.nft"

// The units of the firewalls that images may turn on, which tent's ruleset replaces.
const (
	ufwUnit       = "ufw.service"
	firewalldUnit = "firewalld.service"
)

// ufwConfFile is ufw's configuration, whose ENABLED turns ufw on at boot.
const ufwConfFile = "/etc/ufw/ufw.conf"

// commentPrefix starts the comment of tent's table; the ruleset's sha256 follows it.
const commentPrefix = "tent-node "

// hostfirewall is the phase that owns the host firewall. It turns firewalld off where the image runs it, since its stop
// may touch other tables; loads tent's nftables ruleset from nc's firewall; and turns ufw off last, so that tent's
// rules are in force before ufw's go. The kernel forgets tent's table at a reboot, so the first run after one loads it
// again. nftables.service stays off: its /etc/nftables.conf flushes every table, Docker's too.
func hostfirewall(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (Result, error) {
	return runSteps(ctx, h, nc, disableFirewalld, loadRuleset, disableUFW)
}

// loadRuleset writes tent's ruleset for nc's firewall into FirewallFile, in a directory that it makes when it is
// missing, and loads it when the kernel's table has another comment than the ruleset's.
func loadRuleset(ctx context.Context, h *Host, nc *nodeconfig.NodeConfig) (bool, error) {
	text, sum := renderFirewall(nc.Firewall)
	dirChanged, err := h.FS.EnsureDir(path.Dir(FirewallFile), 0o755, nodeconfig.Owner)
	if err != nil {
		return false, fmt.Errorf("write the host firewall: %w", err)
	}
	written, err := h.FS.WriteFile(FirewallFile, text, 0o600, nodeconfig.Owner)
	if err != nil {
		return false, fmt.Errorf("write the host firewall: %w", err)
	}
	comment, err := loadedComment(ctx, h.Runner)
	if err != nil {
		return false, fmt.Errorf("read the host firewall: %w", err)
	}
	if comment == commentPrefix+sum {
		return dirChanged || written, nil
	}
	if _, err := h.Runner.Run(ctx, "nft", "-f", FirewallFile); err != nil {
		return false, fmt.Errorf("load the host firewall: %w", err)
	}
	return true, nil
}

// disableUFW turns ufw off where its unit exists: apt-get remove takes the program and the unit, and leaves ufw.conf.
// ufw disable runs only while ufw.conf turns ufw on: it also sets the policies of the iptables filter chains to
// accept, which would undo Docker's drop in FORWARD. It leaves tent's table alone. Then it disables the unit, so that
// it does not start at boot.
func disableUFW(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (bool, error) {
	sd := h.systemd()
	state, enabled, err := sd.IsEnabled(ctx, ufwUnit)
	if err != nil {
		return false, fmt.Errorf("disable ufw: %w", err)
	}
	if state == "not-found" {
		return false, nil
	}
	on, err := ufwOn(h.FS)
	if err != nil {
		return false, fmt.Errorf("disable ufw: %w", err)
	}
	if on {
		if _, err := h.Runner.Run(ctx, "ufw", "disable"); err != nil {
			return false, fmt.Errorf("disable ufw: %w", err)
		}
	}
	if enabled {
		if err := sd.Disable(ctx, ufwUnit); err != nil {
			return false, fmt.Errorf("disable ufw: %w", err)
		}
	}
	return on || enabled, nil
}

// ufwOn reports whether ufw.conf turns ufw on at boot, as ufw reads it: the last ENABLED, without quotes, is yes in
// any case. A machine without the file has no ufw.
func ufwOn(fsys FS) (bool, error) {
	data, err := fsys.ReadFile(ufwConfFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	on := false
	for line := range strings.Lines(string(data)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "ENABLED="); ok {
			on = strings.EqualFold(strings.Trim(value, `"'`), "yes")
		}
	}
	return on, nil
}

// disableFirewalld stops firewalld and keeps it from starting at boot, when it is active or enabled.
func disableFirewalld(ctx context.Context, h *Host, _ *nodeconfig.NodeConfig) (bool, error) {
	sd := h.systemd()
	_, active, err := sd.IsActive(ctx, firewalldUnit)
	if err != nil {
		return false, fmt.Errorf("disable firewalld: %w", err)
	}
	_, enabled, err := sd.IsEnabled(ctx, firewalldUnit)
	if err != nil {
		return false, fmt.Errorf("disable firewalld: %w", err)
	}
	if !active && !enabled {
		return false, nil
	}
	if err := sd.DisableNow(ctx, firewalldUnit); err != nil {
		return false, fmt.Errorf("disable firewalld: %w", err)
	}
	return true, nil
}

// loadedComment returns the comment of tent's table in the kernel, as nft -j list tables prints it: empty when the
// table has none or is not there.
func loadedComment(ctx context.Context, r Runner) (string, error) {
	args := []string{"-j", "list", "tables"}
	out, err := r.Run(ctx, "nft", args...)
	if err != nil {
		return "", err
	}
	var listed struct {
		Nftables []struct {
			Table *struct {
				Family  string `json:"family"`
				Name    string `json:"name"`
				Comment string `json:"comment"`
			} `json:"table"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return "", fmt.Errorf("%s: %w", CommandLine("nft", args...), err)
	}
	if listed.Nftables == nil {
		return "", fmt.Errorf("%s: printed no nftables array", CommandLine("nft", args...))
	}
	for _, object := range listed.Nftables {
		if t := object.Table; t != nil && t.Family == "inet" && t.Name == "tent" {
			return t.Comment, nil
		}
	}
	return "", nil
}

// rulesetHead replaces tent's table in one transaction: nft -f makes the table when it is missing, so that the delete
// cannot fail, deletes it and defines it anew. Other tables, such as Docker's and those of Nomad's CNI plugins, stay
// as they are.
const rulesetHead = nodeconfig.NodeHeader + "table inet tent\ndelete table inet tent\ntable inet tent {\n"

// inputChain drops what reaches the node, except replies, loopback, IPv6 neighbour discovery, DHCP replies and the
// traffic of the rules, whose lines replace %s.
const inputChain = `  chain input {
    type filter hook input priority filter; policy drop;
    ct state established,related accept
    ct state invalid drop
    iif "lo" accept
    icmpv6 type { nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept
    udp sport 67 udp dport 68 accept
%s  }
`

// metadataChains keep workloads from the metadata service at %[2]s, of the family %[1]s: only packets with the mark
// %[3]s, which tent-node's own socket carries, leave the node for it, and no forwarded packet reaches it. The drops
// count their packets. A drop in any base chain is final, so forward drops nothing else: Docker's and the CNI
// plugins' chains filter the rest.
const metadataChains = `  chain forward {
    type filter hook forward priority filter; policy accept;
    %[1]s daddr %[2]s counter drop
  }
  chain output {
    type filter hook output priority filter; policy accept;
    %[1]s daddr %[2]s meta mark %[3]s accept
    %[1]s daddr %[2]s counter drop
  }
}
`

// renderFirewall returns tent's nftables ruleset for fw, and sum, the sha256 in hex of the ruleset without the comment
// of tent's table. The comment is commentPrefix and sum.
func renderFirewall(fw nodeconfig.HostFirewall) (text []byte, sum string) {
	var rules strings.Builder
	for _, r := range fw.Rules {
		rules.WriteString(ruleLines(r))
	}
	body := fmt.Sprintf(inputChain, rules.String()) +
		fmt.Sprintf(metadataChains, family(fw.BlockMetadata), fw.BlockMetadata, fmt.Sprintf("0x%08x", env.MetadataMark))
	sum = sha256Hex([]byte(rulesetHead + body))
	return []byte(rulesetHead + `  comment "` + commentPrefix + sum + "\"\n" + body), sum
}

// ruleLines returns the lines of the input chain that accept the traffic of r: one for each address family of its
// sources, IPv4 first.
func ruleLines(r nodeconfig.Rule) string {
	var b strings.Builder
	for _, fam := range []string{"ip", "ip6"} {
		from := outermost(slices.DeleteFunc(slices.Clone(r.From), func(p netip.Prefix) bool {
			return family(p.Addr()) != fam
		}))
		if len(from) == 0 {
			continue
		}
		sources := make([]string, len(from))
		for i, p := range from {
			sources[i] = p.String()
		}
		fmt.Fprintf(&b, "    %s saddr { %s } %s accept comment \"%s\"\n", fam, strings.Join(sources, ", "),
			match(r.Protocol, r.Ports, fam), r.Name)
	}
	return b.String()
}

// family returns the nftables family of a, ip or ip6.
func family(a netip.Addr) string {
	if a.Is4() {
		return "ip"
	}
	return "ip6"
}

// match returns the match of the protocol and the ports: the destination ports for TCP and UDP, and ICMP of the
// family fam, without ports.
func match(protocol string, ports nodeconfig.PortRange, fam string) string {
	switch {
	case protocol == nodeconfig.ProtocolICMP && fam == "ip":
		return "meta l4proto icmp"
	case protocol == nodeconfig.ProtocolICMP:
		return "meta l4proto icmpv6"
	case ports.First == ports.Last:
		return protocol + " dport " + strconv.Itoa(int(ports.First))
	}
	return fmt.Sprintf("%s dport %d-%d", protocol, ports.First, ports.Last)
}

// outermost returns the prefixes that no other one contains, each once, in their order: nft refuses a set whose
// elements overlap.
func outermost(prefixes []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for i, p := range prefixes {
		inner := slices.ContainsFunc(prefixes, func(q netip.Prefix) bool {
			return q.Bits() < p.Bits() && q.Contains(p.Addr())
		})
		if !inner && !slices.Contains(prefixes[:i], p) {
			out = append(out, p)
		}
	}
	return out
}
