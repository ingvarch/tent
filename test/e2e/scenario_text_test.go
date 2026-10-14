package e2e

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ingvarch/tent/test/e2e/janitor"
	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

// scenarioCreateArgs are the arguments of tent that create a cluster of the given shape on an image, open to the
// runner alone.
func scenarioCreateArgs(cfg settings, cluster, image, sshPub, runner string, shape ...string) []string {
	args := []string{
		"create", "cluster", cluster,
		"--provider", "vultr", "--region", cfg.Region, "--machine-type", cfg.Plan,
	}
	args = append(args, shape...)
	return append(args,
		"--image", image, "--ssh-key", sshPub,
		"--ssh-access", runner, "--api-access", runner, "--yes")
}

// haServers and haWorkers are the sizes of the node groups of the HA scenario.
const (
	haServers = 3
	haWorkers = 2
)

// haCreateArgs are the arguments of tent that create the cluster of the HA scenario: haServers servers and haWorkers
// workers on the first image.
func haCreateArgs(cfg settings, cluster, sshPub, runner string) []string {
	return scenarioCreateArgs(cfg, cluster, cfg.haImage(), sshPub, runner,
		"--servers", strconv.Itoa(haServers), "--workers", strconv.Itoa(haWorkers))
}

// upgradeCreateArgs are the arguments of tent that create the cluster of the upgrade scenario: a combined group of
// three nodes on the old Nomad version and the last image.
func upgradeCreateArgs(cfg settings, cluster, sshPub, runner string) []string {
	return scenarioCreateArgs(cfg, cluster, cfg.upgradeImage(), sshPub, runner,
		"--combined", "--servers", "3", "--nomad-version", cfg.UpgradeFrom)
}

// withRollChange takes the output of `tent get clusters <name> -o json`, an array of one cluster, and returns that
// cluster with a Nomad extra configuration that changes the spec hash of every server and every worker: the
// servers get a comment and the workers a node meta value e2e_roll. It refuses a cluster that already has an extra
// configuration, which the suite did not write.
func withRollChange(clustersJSON []byte, run string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(clustersJSON))
	dec.UseNumber()
	var clusters []map[string]any
	if err := dec.Decode(&clusters); err != nil {
		return nil, fmt.Errorf("read the clusters: %w", err)
	}
	if len(clusters) != 1 {
		return nil, fmt.Errorf("got %d clusters, want 1", len(clusters))
	}
	spec, ok := clusters[0]["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("the cluster has no spec")
	}
	nomad := map[string]any{}
	if v, found := spec["nomad"]; found {
		if nomad, ok = v.(map[string]any); !ok {
			return nil, errors.New("spec.nomad is not an object")
		}
	}
	if _, found := nomad["extraConfig"]; found {
		return nil, errors.New("spec.nomad.extraConfig is already set")
	}
	nomad["extraConfig"] = map[string]any{
		"server": "# e2e roll " + run + "\n",
		"client": "client {\n  meta {\n    e2e_roll = \"" + run + "\"\n  }\n}\n",
	}
	spec["nomad"] = nomad
	out, err := json.MarshalIndent(clusters[0], "", "  ")
	if err != nil {
		return nil, fmt.Errorf("write the cluster: %w", err)
	}
	return out, nil
}

// registrationPhrase is what a Nomad client logs after each registration with a server.
const registrationPhrase = "node registration complete"

// bootIDScript prints the machine's boot ID.
func bootIDScript() string { return "cat /proc/sys/kernel/random/boot_id" }

// rebootScript reboots the machine two seconds after it returns, so that the SSH session ends cleanly.
func rebootScript() string { return "systemd-run --on-active=2 --unit=e2e-reboot systemctl reboot" }

// registrationScript prints the registration lines of the current boot's Nomad journal, and exits 0 when there is
// none.
func registrationScript() string {
	return "journalctl -b -u nomad --no-pager -o cat | grep '" + registrationPhrase + "' || true"
}

// joinFilePath is the file that tells the agent which servers to join.
const joinFilePath = "/etc/nomad.d/05-join.hcl"

// joinFileScript prints joinFilePath.
func joinFileScript() string { return "cat " + joinFilePath }

var bootIDForm = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// parseBootID reads the answer of bootIDScript.
func parseBootID(out string) (string, error) {
	id := strings.TrimSpace(out)
	if !bootIDForm.MatchString(id) {
		return "", fmt.Errorf("read the boot ID from %q", id)
	}
	return id, nil
}

// registrationLines returns the lines of out that report a registration.
func registrationLines(out string) []string {
	return slices.DeleteFunc(nonEmptyLines(out), func(line string) bool {
		return !strings.Contains(line, registrationPhrase)
	})
}

// nomadNode is a node as /v1/nodes lists it.
type nomadNode struct {
	ID                    string
	Name                  string
	Status                string
	SchedulingEligibility string
	Version               string
	Address               string
}

// listNodes returns the nodes /v1/nodes lists.
func listNodes(ctx context.Context, n *nomadAPI) ([]nomadNode, error) {
	var nodes []nomadNode
	if err := n.get(ctx, "/v1/nodes", &nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// nodeMeta returns the meta values of the node with this ID.
func nodeMeta(ctx context.Context, n *nomadAPI, id string) (map[string]string, error) {
	var node struct{ Meta map[string]string }
	if err := n.get(ctx, "/v1/node/"+url.PathEscape(id), &node); err != nil {
		return nil, err
	}
	return node.Meta, nil
}

// agentMember is a member of the servers' gossip pool, as /v1/agent/members lists it.
type agentMember struct {
	Name   string
	Addr   string
	Status string
	Tags   map[string]string
}

// listMembers returns the members that /v1/agent/members lists.
func listMembers(ctx context.Context, n *nomadAPI) ([]agentMember, error) {
	var answer struct{ Members []agentMember }
	if err := n.get(ctx, "/v1/agent/members", &answer); err != nil {
		return nil, err
	}
	return answer.Members, nil
}

// runningAllocations counts the running allocations of job.
func runningAllocations(ctx context.Context, n *nomadAPI, job string) (int, error) {
	var allocs []jobAllocation
	if err := n.get(ctx, "/v1/job/"+url.PathEscape(job)+"/allocations", &allocs); err != nil {
		return 0, err
	}
	return len(slices.DeleteFunc(allocs, func(a jobAllocation) bool { return a.ClientStatus != "running" })), nil
}

// checkVersions checks a cluster of combined nodes after a roll: each machine has a node and an alive member, and
// they are all on version; every node is ready and eligible. A server member is named <machine label>.<region>.
// Members of other states are left out of the checks and listed apart in the error. The error names every mismatch.
func checkVersions(nodes []nomadNode, members []agentMember, machines []vultrapi.Instance, version string) error {
	if len(machines) == 0 {
		return errors.New("no machine to check")
	}
	labels := make([]string, 0, len(machines))
	for _, m := range machines {
		labels = append(labels, m.Label)
	}
	var problems []string
	var seen []named
	for _, node := range nodes {
		seen = append(seen, named{node.Name, node.Name})
		if node.Version != version {
			problems = append(problems, fmt.Sprintf("node %s: version %s, want %s", node.Name, node.Version, version))
		}
		if node.Status != "ready" {
			problems = append(problems, fmt.Sprintf("node %s: status %s, want ready", node.Name, node.Status))
		}
		if node.SchedulingEligibility != "eligible" {
			problems = append(problems,
				fmt.Sprintf("node %s: scheduling %s, want eligible", node.Name, node.SchedulingEligibility))
		}
	}
	problems = append(problems, matchProblems("node", seen, labels)...)
	var other []string
	seen = nil
	for _, m := range members {
		if m.Status != "alive" {
			other = append(other, fmt.Sprintf("%s (%s)", m.Name, m.Status))
			continue
		}
		label, _, hasRegion := strings.Cut(m.Name, ".")
		if !hasRegion {
			problems = append(problems, fmt.Sprintf("member %s has no region in its name", m.Name))
		}
		seen = append(seen, named{m.Name, label})
		if build := cmp.Or(m.Tags["build"], "none"); build != version {
			problems = append(problems, fmt.Sprintf("member %s: build %s, want %s", m.Name, build, version))
		}
	}
	problems = append(problems, matchProblems("alive member", seen, labels)...)
	if len(problems) == 0 {
		return nil
	}
	msg := strings.Join(problems, "; ")
	if len(other) > 0 {
		msg += "; other members: " + strings.Join(other, ", ")
	}
	return errors.New(msg)
}

// named is something of a machine that Nomad lists: the name shown, and the label of the machine it belongs to.
type named struct{ shown, label string }

// matchProblems reports what keeps the things of kind from being one for each machine label: a count that differs,
// a thing of no machine, a machine without a thing.
func matchProblems(kind string, things []named, labels []string) []string {
	var problems []string
	if len(things) != len(labels) {
		problems = append(problems, fmt.Sprintf("%d %ss for %d machines", len(things), kind, len(labels)))
	}
	have := map[string]bool{}
	for _, th := range things {
		have[th.label] = true
		if !slices.Contains(labels, th.label) {
			problems = append(problems, fmt.Sprintf("%s %s is of no current machine", kind, th.shown))
		}
	}
	for _, label := range labels {
		if !have[label] {
			problems = append(problems, fmt.Sprintf("machine %s has no %s", label, kind))
		}
	}
	return problems
}

// checkRolled fails when a machine of before is still listed in after, and names those machines. An empty before
// fails: the machines to roll were never recorded.
func checkRolled(before, after []vultrapi.Instance) error {
	if len(before) == 0 {
		return errors.New("no machine was recorded before the roll")
	}
	listed := listedIDs(after)
	var left []string
	for _, in := range before {
		if listed[in.ID] {
			left = append(left, in.Label)
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("machines of before the roll are still listed: %s", strings.Join(left, ", "))
	}
	return nil
}

// checkKept fails when a machine of kept is no longer listed in after, and names those machines. An empty kept
// fails: the machines to keep were never recorded.
func checkKept(kept, after []vultrapi.Instance) error {
	if len(kept) == 0 {
		return errors.New("no machine was recorded to be kept")
	}
	listed := listedIDs(after)
	var gone []string
	for _, in := range kept {
		if !listed[in.ID] {
			gone = append(gone, in.Label)
		}
	}
	if len(gone) > 0 {
		return fmt.Errorf("machines that should stay are no longer listed: %s", strings.Join(gone, ", "))
	}
	return nil
}

func listedIDs(instances []vultrapi.Instance) map[string]bool {
	ids := make(map[string]bool, len(instances))
	for _, in := range instances {
		ids[in.ID] = true
	}
	return ids
}

// checkCounts checks that cluster has the given numbers of server and client machines among instances.
func checkCounts(instances []vultrapi.Instance, cluster string, servers, workers int) error {
	var problems []string
	if got := len(machinesOf(instances, cluster, "server")); got != servers {
		problems = append(problems, fmt.Sprintf("%d servers, want %d", got, servers))
	}
	if got := len(machinesOf(instances, cluster, "client")); got != workers {
		problems = append(problems, fmt.Sprintf("%d workers, want %d", got, workers))
	}
	if len(problems) > 0 {
		return fmt.Errorf("cluster %s has %s", cluster, strings.Join(problems, "; "))
	}
	return nil
}

// serversToDelete returns the server machines of an e2e cluster: the ones that carry both its cluster tag and the
// server role tag. It refuses a name that is not e2e- followed by more, and a cluster that has no server machine.
func serversToDelete(instances []vultrapi.Instance, cluster string) ([]vultrapi.Instance, error) {
	if !strings.HasPrefix(cluster, janitor.ClusterPrefix) || len(cluster) == len(janitor.ClusterPrefix) {
		return nil, fmt.Errorf("%q is not the name of a cluster of this suite", cluster)
	}
	servers := machinesOf(instances, cluster, "server")
	if len(servers) == 0 {
		return nil, fmt.Errorf("cluster %s has no server machine", cluster)
	}
	return servers, nil
}

// nomadPort is the port of a server's HTTP API.
const nomadPort = "4646"

// serverAddresses returns the API address, "https://<ip>:4646", of each machine of a role in a cluster that has a
// public address.
func serverAddresses(instances []vultrapi.Instance, cluster, role string) []string {
	var out []string
	for _, m := range machinesOf(instances, cluster, role) {
		if m.MainIP != "" && m.MainIP != "0.0.0.0" {
			out = append(out, "https://"+net.JoinHostPort(m.MainIP, nomadPort))
		}
	}
	return out
}

// lowestLabel returns the machine whose label sorts first.
func lowestLabel(machines []vultrapi.Instance) (vultrapi.Instance, error) {
	if len(machines) == 0 {
		return vultrapi.Instance{}, errors.New("no machine to choose from")
	}
	return slices.MinFunc(machines, func(a, b vultrapi.Instance) int { return strings.Compare(a.Label, b.Label) }), nil
}

// nodeNamed returns the one node that has name; tent names a node after its machine.
func nodeNamed(nodes []nomadNode, name string) (nomadNode, error) {
	var found []nomadNode
	for _, n := range nodes {
		if n.Name == name {
			found = append(found, n)
		}
	}
	if len(found) != 1 {
		return nomadNode{}, fmt.Errorf("%d nodes are named %s, want 1", len(found), name)
	}
	return found[0], nil
}

// rollMetaKey is the node meta key that the roll change gives every worker.
const rollMetaKey = "e2e_roll"

// checkRollMeta checks that there are want nodes and that each of them, by name, has the run's value in its meta.
func checkRollMeta(metas map[string]map[string]string, run string, want int) error {
	var problems []string
	if len(metas) != want {
		problems = append(problems, fmt.Sprintf("%d nodes, want %d", len(metas), want))
	}
	for _, name := range slices.Sorted(maps.Keys(metas)) {
		if got := metas[name][rollMetaKey]; got != run {
			problems = append(problems, fmt.Sprintf("node %s has %s %q, want %q", name, rollMetaKey, got, run))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// bootProblem is "" when the answer of bootIDScript shows a boot ID other than before; otherwise it says why not.
func bootProblem(out string, err error, before string) string {
	if err != nil {
		return err.Error()
	}
	id, err := parseBootID(out)
	switch {
	case err != nil:
		return err.Error()
	case id == before:
		return "the machine still has the boot ID " + before
	}
	return ""
}

// registeredProblem is "" when the journal of this boot holds a registration line and Nomad lists the node with id as
// ready and eligible; otherwise it says what is missing.
func registeredProblem(lines []string, nodes []nomadNode, id string) string {
	if len(lines) == 0 {
		return "the journal of this boot holds no registration"
	}
	i := slices.IndexFunc(nodes, func(n nomadNode) bool { return n.ID == id })
	switch {
	case i < 0:
		return "Nomad does not list the node " + id
	case nodes[i].Status != "ready":
		return "the node " + id + " is " + nodes[i].Status
	case nodes[i].SchedulingEligibility != "eligible":
		return "the node " + id + " is " + nodes[i].SchedulingEligibility
	}
	return ""
}

// restartEvidence is what the restart of a client leaves to read.
type restartEvidence struct {
	Machine, NodeID        string
	BootBefore, BootAfter  string
	BootWait, RegisterWait time.Duration
	Registrations          []string
	JoinFile               string
}

// restartRecord writes the evidence as the text of the step's record file.
func restartRecord(e restartEvidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "machine: %s\nnode ID: %s\nboot ID before: %s\nboot ID after: %s\n", e.Machine, e.NodeID,
		e.BootBefore, e.BootAfter)
	fmt.Fprintf(&b, "wait for the new boot: %s\nwait for the registration: %s\n", e.BootWait, e.RegisterWait)
	b.WriteString("registration lines of this boot:\n")
	for _, line := range e.Registrations {
		b.WriteString(line + "\n")
	}
	b.WriteString(joinFilePath + ":\n" + e.JoinFile)
	return b.String()
}

// allocationsProblem is "" when running is the wanted number of running allocations of job.
func allocationsProblem(running int, job string, want int) string {
	if running == want {
		return ""
	}
	return fmt.Sprintf("%d running allocations of %s, want %d", running, job, want)
}
