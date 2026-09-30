package nodeup_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
	"github.com/ingvarch/tent/internal/nodeup/env"
	"github.com/ingvarch/tent/internal/nodeup/nodeuptest"
)

// Paths and commands of the hostfirewall phase.
const (
	ufwConfPath      = "/etc/ufw/ufw.conf"
	ufwDisable       = "ufw disable"
	ufwEnabled       = "systemctl is-enabled ufw.service"
	ufwOff           = "systemctl disable ufw.service"
	firewalldActive  = "systemctl is-active firewalld.service"
	firewalldEnabled = "systemctl is-enabled firewalld.service"
	firewalldOff     = "systemctl disable --now firewalld.service"
	listTables       = "nft -j list tables"
	loadTent         = "nft -f " + nodeup.FirewallFile
)

// Sources of the sample's rules.
var (
	anywhere    = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	clusterCIDR = []netip.Prefix{netip.MustParsePrefix("10.64.0.0/16")}
	bridges     = []netip.Prefix{netip.MustParsePrefix("172.26.64.0/20"), netip.MustParsePrefix("172.17.0.0/16")}
)

// rule returns a rule of the host firewall that opens the ports first to last, none for ICMP.
func rule(name, protocol string, first, last uint16, from []netip.Prefix) nodeconfig.Rule {
	return nodeconfig.Rule{
		Name: name, Protocol: protocol, Ports: nodeconfig.PortRange{First: first, Last: last}, From: from,
	}
}

// hostFirewall returns a sample host firewall of a node of the role, like the one tent builds: the public rules, the
// rules between nodes that reach the role, and the bridge rules on a node that runs a client. It must match what
// internal/app's HostFirewall builds for each role, which the tests of internal/app pin; the tests of nodeup cannot
// import internal/app.
func hostFirewall(role v1alpha1.Role) nodeconfig.HostFirewall {
	tcp, udp := nodeconfig.ProtocolTCP, nodeconfig.ProtocolUDP
	rules := []nodeconfig.Rule{
		rule("ssh", tcp, 22, 22, anywhere),
		rule("icmp", nodeconfig.ProtocolICMP, 0, 0, anywhere),
	}
	if role.RunsServer() {
		rules = append(rules, rule("api", tcp, 4646, 4646, anywhere))
	}
	rules = append(rules, rule("nomad-http", tcp, 4646, 4646, clusterCIDR))
	if role.RunsServer() {
		rules = append(rules,
			rule("nomad-rpc", tcp, 4647, 4647, clusterCIDR),
			rule("serf", tcp, 4648, 4648, clusterCIDR),
			rule("serf", udp, 4648, 4648, clusterCIDR),
		)
	}
	if role.RunsClient() {
		rules = append(rules,
			rule("dynamic", tcp, 20000, 32000, clusterCIDR),
			rule("dynamic", udp, 20000, 32000, clusterCIDR),
			rule("bridge-http", tcp, 4646, 4646, bridges),
			rule("bridge-dynamic", tcp, 20000, 32000, bridges),
			rule("bridge-dynamic", udp, 20000, 32000, bridges),
		)
	}
	return nodeconfig.HostFirewall{Rules: rules, BlockMetadata: netip.MustParseAddr("169.254.169.254")}
}

// withFirewall returns the combined config as a config of the role, with the role's host firewall.
func withFirewall(t *testing.T, role v1alpha1.Role) *nodeconfig.NodeConfig {
	t.Helper()
	nc := combined(t)
	nc.Role, nc.Firewall = role, hostFirewall(role)
	return rehash(t, nc)
}

// runHostfirewall runs the hostfirewall phase.
func runHostfirewall(t *testing.T, h *nodeup.Host, nc *nodeconfig.NodeConfig) (nodeup.Result, error) {
	t.Helper()
	return runPhase(t, "hostfirewall", h, nc, &nodeuptest.Environment{})
}

// ruleset returns the ruleset in the firewall file, and fails t unless only root can read and write it.
func ruleset(t *testing.T, fsys *nodeuptest.FS) string {
	t.Helper()
	e, ok := fsys.Entry(nodeup.FirewallFile)
	if !ok || e.Dir || e.Mode != 0o600 || e.Owner != nodeconfig.Owner {
		t.Fatalf("%s is %+v, want a file with mode 0600 owned by root:root", nodeup.FirewallFile, e)
	}
	return string(e.Data)
}

// tableComment matches the comment of tent's table in a ruleset.
var tableComment = regexp.MustCompile(`(?m)^table inet tent \{\n  comment "(tent-node [0-9a-f]{64})"\n`)

func TestHostfirewall(t *testing.T) {
	for _, role := range []v1alpha1.Role{v1alpha1.RoleServer, v1alpha1.RoleClient, v1alpha1.RoleCombined} {
		t.Run(string(role), func(t *testing.T) {
			h, fsys, r, _ := ubuntuMachine(t)
			res, err := runHostfirewall(t, h, withFirewall(t, role))
			if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
				t.Fatalf("hostfirewall: %+v, %v; want done", res, err)
			}
			// ufw goes last, once tent's table is loaded. ufw disable comes before systemctl disable: at boot ufw
			// does nothing while its conf says no, whatever its unit says.
			want := []string{
				firewalldActive, firewalldEnabled, listTables, loadTent, ufwEnabled, ufwDisable, ufwOff,
			}
			if diff := cmp.Diff(want, r.Commands()); diff != "" {
				t.Errorf("commands (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]string{nodeup.FirewallFile, ufwConfPath}, fsys.Changes()); diff != "" {
				t.Errorf("changes (-want +got):\n%s", diff)
			}
			checkGolden(t, "firewall-"+string(role)+".nft.golden", []byte(ruleset(t, fsys)))
			// cloud-init made /etc/tent; the phase leaves it as it is.
			if dir, _ := fsys.Entry("/etc/tent"); !dir.Dir || dir.Mode != 0o755 {
				t.Errorf("/etc/tent is %+v, want the directory with mode 0755 that cloud-init made", dir)
			}
		})
	}
}

func TestHostfirewallMakesEtcTent(t *testing.T) {
	// As on a machine where tent-node up runs with --config elsewhere.
	h, fsys, _, _ := ubuntuMachine(t)
	if _, err := fsys.Remove("/etc/tent"); err != nil {
		t.Fatal(err)
	}
	changes := len(fsys.Changes())
	if res, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleServer)); err != nil || res.Status != nodeup.Done {
		t.Fatalf("hostfirewall: %+v, %v; want done", res, err)
	}
	if dir, _ := fsys.Entry("/etc/tent"); !dir.Dir || dir.Mode != 0o755 || dir.Owner != nodeconfig.Owner {
		t.Errorf("/etc/tent is %+v, want a directory with mode 0755 owned by root:root", dir)
	}
	if diff := cmp.Diff([]string{"/etc/tent", nodeup.FirewallFile, ufwConfPath}, fsys.Changes()[changes:]); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
}

func TestHostfirewallCommentIsTheRulesetsHash(t *testing.T) {
	h, fsys, _, _ := ubuntuMachine(t)
	if _, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleCombined)); err != nil {
		t.Fatal(err)
	}
	text := ruleset(t, fsys)
	m := tableComment.FindStringSubmatchIndex(text)
	if m == nil {
		t.Fatalf("the ruleset has no table comment tent-node <sha256>:\n%s", text)
	}
	comment := text[m[2]:m[3]]
	// The comment line starts after the table's line and ends with its newline.
	start := strings.Index(text[m[0]:], "\n") + m[0] + 1
	without := text[:start] + text[m[1]:]
	sum := sha256.Sum256([]byte(without))
	if want := "tent-node " + hex.EncodeToString(sum[:]); comment != want {
		t.Errorf("the table's comment is %q, want %q, the sha256 of the ruleset without it", comment, want)
	}
}

func TestHostfirewallSecondRunChangesNothing(t *testing.T) {
	h, fsys, r, _ := ubuntuMachine(t)
	nc := withFirewall(t, v1alpha1.RoleCombined)
	if _, err := runHostfirewall(t, h, nc); err != nil {
		t.Fatal(err)
	}
	changes, commands := len(fsys.Changes()), len(r.Commands())
	res, err := runHostfirewall(t, h, nc)
	if err != nil || res != (nodeup.Result{Status: nodeup.Unchanged}) {
		t.Errorf("the second run: %+v, %v; want unchanged", res, err)
	}
	if got := fsys.Changes()[changes:]; len(got) != 0 {
		t.Errorf("the second run changed %q, want nothing", got)
	}
	want := []string{firewalldActive, firewalldEnabled, listTables, ufwEnabled}
	if diff := cmp.Diff(want, r.Commands()[commands:]); diff != "" {
		t.Errorf("the second run's commands, which must only read (-want +got):\n%s", diff)
	}
}

func TestHostfirewallAfterAReboot(t *testing.T) {
	h, fsys, r, m := ubuntuMachine(t)
	nc := withFirewall(t, v1alpha1.RoleClient)
	if _, err := runHostfirewall(t, h, nc); err != nil {
		t.Fatal(err)
	}
	m.Reboot()
	changes, commands := len(fsys.Changes()), len(r.Commands())
	// The kernel lost the table, so the phase loads it again; ufw stays off.
	res, err := runHostfirewall(t, h, nc)
	if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
		t.Errorf("after a reboot: %+v, %v; want done", res, err)
	}
	want := []string{firewalldActive, firewalldEnabled, listTables, loadTent, ufwEnabled}
	if diff := cmp.Diff(want, r.Commands()[commands:]); diff != "" {
		t.Errorf("commands after a reboot (-want +got):\n%s", diff)
	}
	if got := fsys.Changes()[changes:]; len(got) != 0 {
		t.Errorf("after a reboot the phase changed %q, want nothing", got)
	}
	if res, err := runHostfirewall(t, h, nc); err != nil || res.Status != nodeup.Unchanged {
		t.Errorf("the run after that: %+v, %v; want unchanged", res, err)
	}
}

func TestHostfirewallReloadsAChangedRuleset(t *testing.T) {
	h, fsys, r, _ := ubuntuMachine(t)
	if _, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleServer)); err != nil {
		t.Fatal(err)
	}
	before := ruleset(t, fsys)
	changes, commands := len(fsys.Changes()), len(r.Commands())
	nc := withFirewall(t, v1alpha1.RoleServer)
	nc.Firewall.Rules[0].Ports = nodeconfig.PortRange{First: 2222, Last: 2222}
	rehash(t, nc)
	res, err := runHostfirewall(t, h, nc)
	if err != nil || res != (nodeup.Result{Status: nodeup.Done}) {
		t.Errorf("a changed rule: %+v, %v; want done", res, err)
	}
	if diff := cmp.Diff([]string{nodeup.FirewallFile}, fsys.Changes()[changes:]); diff != "" {
		t.Errorf("changes (-want +got):\n%s", diff)
	}
	if n := countOf(r.Commands()[commands:], loadTent); n != 1 {
		t.Errorf("%s ran %d times, want once", loadTent, n)
	}
	after := ruleset(t, fsys)
	if !strings.Contains(after, " tcp dport 2222 accept comment \"ssh\"\n") {
		t.Errorf("the ruleset does not open 2222 to ssh:\n%s", after)
	}
	if tableComment.FindStringSubmatch(before)[1] == tableComment.FindStringSubmatch(after)[1] {
		t.Error("the table's comment did not change with the rule")
	}
}

func TestHostfirewallUFW(t *testing.T) {
	// conf gives ufw.conf the content.
	conf := func(content string) func(*testing.T, *nodeuptest.FS, *nodeuptest.Runner) {
		return func(t *testing.T, fsys *nodeuptest.FS, _ *nodeuptest.Runner) {
			fsys.AddFile(t, ufwConfPath, []byte(content), 0o644, nodeconfig.Owner)
		}
	}
	all := []string{ufwEnabled, ufwDisable, ufwOff}
	cases := []struct {
		name  string
		setup func(t *testing.T, fsys *nodeuptest.FS, r *nodeuptest.Runner)
		ufw   []string // the commands after tent's table is loaded
	}{
		// ufw reads the setting without quotes and in any case, and the last one counts.
		{"quoted", conf("ENABLED=\"yes\"\n"), all},
		{"upper case", conf("ENABLED=YES\n"), all},
		{"the last setting says no", conf("ENABLED=yes\nENABLED=no\n"), []string{ufwEnabled, ufwOff}},
		{"commented out", conf("#ENABLED=yes\n"), []string{ufwEnabled, ufwOff}},
		// ufw disable would set the filter policies to accept, and undo Docker's drop in FORWARD.
		{"conf says no, unit enabled", func(t *testing.T, fsys *nodeuptest.FS, _ *nodeuptest.Runner) {
			conf, _ := fsys.Entry(ufwConfPath)
			fsys.AddFile(t, ufwConfPath, []byte(strings.Replace(string(conf.Data), "ENABLED=yes", "ENABLED=no", 1)),
				0o644, nodeconfig.Owner)
		}, []string{ufwEnabled, ufwOff}},
		{"no ufw", func(t *testing.T, fsys *nodeuptest.FS, _ *nodeuptest.Runner) {
			for _, p := range []string{ufwConfPath, "/usr/lib/systemd/system/ufw.service"} {
				if _, err := fsys.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
		}, []string{ufwEnabled}},
		{"conf says yes, unit disabled", func(t *testing.T, _ *nodeuptest.FS, r *nodeuptest.Runner) {
			if err := (nodeup.Systemd{Runner: r}).Disable(t.Context(), "ufw.service"); err != nil {
				t.Fatal(err)
			}
		}, []string{ufwEnabled, ufwDisable}},
		// Without the conf, ufw does nothing at boot, whatever its unit says.
		{"no conf, unit enabled", func(t *testing.T, fsys *nodeuptest.FS, _ *nodeuptest.Runner) {
			if _, err := fsys.Remove(ufwConfPath); err != nil {
				t.Fatal(err)
			}
		}, []string{ufwEnabled, ufwOff}},
		// apt-get remove leaves the conf, which still says yes, and takes the program and the unit.
		{"removed, not purged", func(t *testing.T, fsys *nodeuptest.FS, _ *nodeuptest.Runner) {
			if _, err := fsys.Remove("/usr/lib/systemd/system/ufw.service"); err != nil {
				t.Fatal(err)
			}
		}, []string{ufwEnabled}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fsys, r, _ := ubuntuMachine(t)
			c.setup(t, fsys, r)
			commands := len(r.Commands())
			res, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleServer))
			if err != nil || res.Status != nodeup.Done {
				t.Fatalf("hostfirewall: %+v, %v; want done", res, err)
			}
			want := append([]string{firewalldActive, firewalldEnabled, listTables, loadTent}, c.ufw...)
			if diff := cmp.Diff(want, r.Commands()[commands:]); diff != "" {
				t.Errorf("commands (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHostfirewallLoadsTentsTableBeforeUFWGoes(t *testing.T) {
	h, fsys, r, _ := ubuntuMachine(t)
	var listed string // tent's table when ufw disable ran
	r.On(ufwDisable, func(ctx context.Context) ([]byte, error) {
		out, err := r.Run(ctx, "nft", "-j", "list", "tables")
		listed = fmt.Sprint(string(out), err)
		return nil, nil
	})
	if _, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleCombined)); err != nil {
		t.Fatal(err)
	}
	comment := tableComment.FindStringSubmatch(ruleset(t, fsys))[1]
	if !strings.Contains(listed, `"`+comment+`"`) {
		t.Errorf("when ufw disable ran, nft listed %q, want tent's table with the comment %q", listed, comment)
	}
}

func TestHostfirewallDisablesFirewalld(t *testing.T) {
	t.Run("installed", func(t *testing.T) {
		h, _, r, m := ubuntuMachine(t)
		m.InstallFirewalld(t)
		nc := withFirewall(t, v1alpha1.RoleServer)
		if _, err := runHostfirewall(t, h, nc); err != nil {
			t.Fatal(err)
		}
		if n := countOf(r.Commands(), firewalldOff); n != 1 {
			t.Errorf("%s ran %d times, want once", firewalldOff, n)
		}
		commands := len(r.Commands())
		if res, err := runHostfirewall(t, h, nc); err != nil || res.Status != nodeup.Unchanged {
			t.Errorf("the second run: %+v, %v; want unchanged", res, err)
		}
		if slices.Contains(r.Commands()[commands:], firewalldOff) {
			t.Errorf("the second run disabled firewalld again: %q", r.Commands()[commands:])
		}
	})
	// Either state is enough.
	cases := map[string]map[string]nodeuptest.Answer{
		"active only":  {firewalldEnabled: nodeuptest.ExitOutput(1, "disabled\n", "")},
		"enabled only": {firewalldActive: nodeuptest.ExitOutput(3, "inactive\n", "")},
	}
	for name, answers := range cases {
		t.Run(name, func(t *testing.T) {
			h, _, r, m := ubuntuMachine(t)
			m.InstallFirewalld(t)
			for command, answer := range answers {
				r.On(command, answer)
			}
			if _, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleServer)); err != nil {
				t.Fatal(err)
			}
			if n := countOf(r.Commands(), firewalldOff); n != 1 {
				t.Errorf("%s ran %d times, want once", firewalldOff, n)
			}
		})
	}
}

func TestHostfirewallLoadsWhenTheTableHasAnotherComment(t *testing.T) {
	// tables answers as nft -j list tables does, with Docker's table, a table tent of another family, and then more.
	tables := func(more string) nodeuptest.Answer {
		return nodeuptest.Output(`{"nftables": [{"metainfo": {"version": "1.0.9", "release_name": "Old Doc Yak #3", ` +
			`"json_schema_version": 1}}, {"table": {"family": "ip", "name": "filter", "handle": 1}}, ` +
			`{"table": {"family": "ip", "name": "tent", "handle": 2, "comment": "x"}}` + more + `]}` + "\n")
	}
	cases := map[string]nodeuptest.Answer{
		"no comment":      tables(`, {"table": {"family": "inet", "name": "tent", "handle": 3}}`),
		"another comment": tables(`, {"table": {"family": "inet", "name": "tent", "handle": 3, "comment": "tent-node 00"}}`),
		"no table":        tables(""),
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			h, _, r, _ := ubuntuMachine(t)
			r.On(listTables, answer)
			res, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleServer))
			if err != nil || res.Status != nodeup.Done {
				t.Errorf("hostfirewall: %+v, %v; want done", res, err)
			}
			if n := countOf(r.Commands(), loadTent); n != 1 {
				t.Errorf("%s ran %d times, want once", loadTent, n)
			}
		})
	}
}

func TestHostfirewallFails(t *testing.T) {
	// exec.ErrNotFound's text differs between systems, such as $PATH or %PATH%.
	noNftErr := &exec.Error{Name: "nft", Err: exec.ErrNotFound}
	noNft := func(context.Context) ([]byte, error) { return nil, noNftErr }
	noBus := nodeuptest.Exit(1, "Failed to connect to bus: No such file or directory")
	denied := nodeuptest.Exit(1, "Failed to disable unit: Access denied")
	cases := []struct {
		name    string
		answers map[string]nodeuptest.Answer
		loaded  bool // whether the failure comes once tent's table is loaded
		want    string
	}{
		{"nft missing", map[string]nodeuptest.Answer{listTables: noNft}, false,
			"read the host firewall: nft -j list tables: " + noNftErr.Error()},
		{"nft cannot list", map[string]nodeuptest.Answer{listTables: nodeuptest.Exit(1, "netlink: Error: cache "+
			"initialization failed: Operation not permitted")}, false, "read the host firewall: nft -j list tables: " +
			"exit status 1: netlink: Error: cache initialization failed: Operation not permitted"},
		{"not JSON", map[string]nodeuptest.Answer{listTables: nodeuptest.Output("table inet tent\n")}, false,
			"read the host firewall: nft -j list tables: invalid character 'a' in literal true (expecting 'r')"},
		{"JSON without nftables", map[string]nodeuptest.Answer{listTables: nodeuptest.Output(`{"tables": []}`)},
			false, "read the host firewall: nft -j list tables: printed no nftables array"},
		{"load fails", map[string]nodeuptest.Answer{loadTent: nodeuptest.Exit(1, "/etc/tent/firewall.nft:9:5-8: "+
			"Error: syntax error")}, false, "load the host firewall: nft -f /etc/tent/firewall.nft: exit status 1: " +
			"/etc/tent/firewall.nft:9:5-8: Error: syntax error"},
		{"no state of firewalld", map[string]nodeuptest.Answer{firewalldActive: noBus}, false,
			"disable firewalld: systemctl is-active firewalld.service: exit status 1: Failed to connect to bus: No " +
				"such file or directory"},
		{"no enablement of firewalld", map[string]nodeuptest.Answer{firewalldEnabled: noBus}, false,
			"disable firewalld: systemctl is-enabled firewalld.service: exit status 1: Failed to connect to bus: No " +
				"such file or directory"},
		{"systemctl disable --now firewalld fails", map[string]nodeuptest.Answer{
			firewalldActive: nodeuptest.Output("active\n"), firewalldOff: denied,
		}, false, "disable firewalld: systemctl disable --now firewalld.service: exit status 1: Failed to disable " +
			"unit: Access denied"},
		{"no enablement of ufw", map[string]nodeuptest.Answer{ufwEnabled: noBus}, true,
			"disable ufw: systemctl is-enabled ufw.service: exit status 1: Failed to connect to bus: No such file or " +
				"directory"},
		{"ufw disable fails", map[string]nodeuptest.Answer{
			ufwDisable: nodeuptest.Exit(1, "ERROR: problem running ufw-init"),
		}, true, "disable ufw: ufw disable: exit status 1: ERROR: problem running ufw-init"},
		{"systemctl disable ufw fails", map[string]nodeuptest.Answer{ufwOff: denied}, true,
			"disable ufw: systemctl disable ufw.service: exit status 1: Failed to disable unit: Access denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, r, _ := ubuntuMachine(t)
			for command, answer := range c.answers {
				r.On(command, answer)
			}
			res, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleServer))
			if errText(err) != c.want || res != (nodeup.Result{}) {
				t.Errorf("hostfirewall: %+v, %q; want no result and %q", res, errText(err), c.want)
			}
			// Without tent's table, ufw stays on.
			if !c.loaded && slices.Contains(r.Commands(), ufwDisable) {
				t.Errorf("ufw was turned off although the phase failed before tent's table was loaded: %q",
					r.Commands())
			}
		})
	}
	t.Run("ufw.conf cannot be read", func(t *testing.T) {
		h, fsys, _, _ := ubuntuMachine(t)
		if _, err := fsys.Remove(ufwConfPath); err != nil {
			t.Fatal(err)
		}
		fsys.AddDir(t, ufwConfPath, 0o755, nodeconfig.Owner)
		_, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleServer))
		if want := "disable ufw: read " + ufwConfPath + ": is a directory"; errText(err) != want {
			t.Errorf("hostfirewall: %q, want %q", errText(err), want)
		}
	})
	t.Run("/etc/tent cannot be made", func(t *testing.T) {
		h, fsys, r, _ := ubuntuMachine(t)
		fsys.Fail("/etc/tent", fs.ErrPermission)
		_, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleServer))
		if want := "write the host firewall: make directory /etc/tent: permission denied"; errText(err) != want {
			t.Errorf("hostfirewall: %q, want %q", errText(err), want)
		}
		if slices.Contains(r.Commands(), loadTent) || slices.Contains(r.Commands(), ufwDisable) {
			t.Errorf("the phase loaded a ruleset without its directory, or turned ufw off: %q", r.Commands())
		}
	})
	t.Run("the ruleset cannot be written", func(t *testing.T) {
		h, fsys, r, _ := ubuntuMachine(t)
		fsys.Fail(nodeup.FirewallFile, fs.ErrPermission)
		_, err := runHostfirewall(t, h, withFirewall(t, v1alpha1.RoleServer))
		if want := "write the host firewall: write " + nodeup.FirewallFile + ": permission denied"; errText(
			err) != want {
			t.Errorf("hostfirewall: %q, want %q", errText(err), want)
		}
		if slices.Contains(r.Commands(), loadTent) || slices.Contains(r.Commands(), ufwDisable) {
			t.Errorf("the phase loaded a ruleset it could not write, or turned ufw off: %q", r.Commands())
		}
	})
}

func TestHostfirewallRuleset(t *testing.T) {
	// ruleOf runs the phase with the firewall fw and returns the ruleset it wrote.
	ruleOf := func(t *testing.T, fw nodeconfig.HostFirewall) string {
		t.Helper()
		h, fsys, _, _ := ubuntuMachine(t)
		nc := combined(t)
		nc.Firewall = fw
		if _, err := runHostfirewall(t, h, rehash(t, nc)); err != nil {
			t.Fatal(err)
		}
		return ruleset(t, fsys)
	}
	metadata := netip.MustParseAddr("169.254.169.254")
	mark := fmt.Sprintf("0x%08x", env.MetadataMark)

	t.Run("the metadata service", func(t *testing.T) {
		text := ruleOf(t, nodeconfig.HostFirewall{BlockMetadata: metadata})
		// Only tent-node's own socket carries the mark.
		want := "    ip daddr 169.254.169.254 meta mark " + mark + " accept\n" +
			"    ip daddr 169.254.169.254 counter drop\n"
		if mark != "0x00000747" || !strings.Contains(text, want) {
			t.Errorf("the output chain does not let only the mark %s reach the metadata service:\n%s", mark, text)
		}
		// The forward chain drops it too, and both count what they drop.
		if n := strings.Count(text, "    ip daddr 169.254.169.254 counter drop\n"); n != 2 {
			t.Errorf("%d chains count and drop packets to the metadata service, want 2:\n%s", n, text)
		}
	})
	t.Run("an IPv6 metadata service", func(t *testing.T) {
		text := ruleOf(t, nodeconfig.HostFirewall{BlockMetadata: netip.MustParseAddr("fd00:ec2::254")})
		for _, want := range []string{
			"    ip6 daddr fd00:ec2::254 counter drop\n", "    ip6 daddr fd00:ec2::254 meta mark " + mark + " accept\n",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the ruleset has no line %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, "ip daddr") {
			t.Errorf("the ruleset matches an IPv4 metadata address:\n%s", text)
		}
	})
	t.Run("overlapping prefixes", func(t *testing.T) {
		from := []netip.Prefix{
			netip.MustParsePrefix("10.64.0.0/16"), netip.MustParsePrefix("10.0.0.0/8"),
			netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("10.64.1.0/24"),
			netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("fd00:1::/32"),
			netip.MustParsePrefix("fd00::/8"),
		}
		text := ruleOf(t, nodeconfig.HostFirewall{
			Rules: []nodeconfig.Rule{rule("web", nodeconfig.ProtocolTCP, 8080, 8081, from)}, BlockMetadata: metadata,
		})
		for _, want := range []string{
			"    ip saddr { 10.0.0.0/8, 192.168.0.0/16 } tcp dport 8080-8081 accept comment \"web\"\n",
			"    ip6 saddr { fd00::/8 } tcp dport 8080-8081 accept comment \"web\"\n",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the ruleset has no line %q:\n%s", want, text)
			}
		}
	})
	t.Run("one family", func(t *testing.T) {
		text := ruleOf(t, nodeconfig.HostFirewall{
			Rules: []nodeconfig.Rule{
				rule("v4", nodeconfig.ProtocolUDP, 53, 53, clusterCIDR),
				rule("v6", nodeconfig.ProtocolICMP, 0, 0, []netip.Prefix{netip.MustParsePrefix("fd00::/8")}),
			},
			BlockMetadata: metadata,
		})
		want := "    ip saddr { 10.64.0.0/16 } udp dport 53 accept comment \"v4\"\n" +
			"    ip6 saddr { fd00::/8 } meta l4proto icmpv6 accept comment \"v6\"\n  }\n"
		if !strings.Contains(text, want) {
			t.Errorf("the ruleset has no lines\n%s\nin\n%s", want, text)
		}
	})
}
