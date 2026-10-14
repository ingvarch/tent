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
// workers on the image.
func haCreateArgs(cfg settings, cluster, image, sshPub, runner string) []string {
	return scenarioCreateArgs(cfg, cluster, image, sshPub, runner,
		"--servers", strconv.Itoa(haServers), "--workers", strconv.Itoa(haWorkers))
}

// upgradeNodes is the size of the combined group of the upgrade scenario.
const upgradeNodes = 3

// upgradeCreateArgs are the arguments of tent that create the cluster of the upgrade scenario: a combined group of
// three nodes on the old Nomad version and the image.
func upgradeCreateArgs(cfg settings, cluster, image, sshPub, runner string) []string {
	return scenarioCreateArgs(cfg, cluster, image, sshPub, runner,
		"--combined", "--servers", strconv.Itoa(upgradeNodes), "--nomad-version", cfg.UpgradeFrom)
}

// upgradeTarget takes the output of `tent get clusters <name> -o json`, an array of one cluster, and returns its
// spec.nomad.version, which must differ from the version that the cluster was built on.
func upgradeTarget(clustersJSON []byte, from string) (string, error) {
	var clusters []struct {
		Spec struct {
			Nomad struct {
				Version string
			}
		}
	}
	if err := json.Unmarshal(clustersJSON, &clusters); err != nil {
		return "", fmt.Errorf("read the clusters: %w", err)
	}
	if len(clusters) != 1 {
		return "", fmt.Errorf("got %d clusters, want 1", len(clusters))
	}
	version := clusters[0].Spec.Nomad.Version
	if version == "" {
		return "", errors.New("the cluster has no spec.nomad.version")
	}
	if version == from {
		return "", fmt.Errorf("spec.nomad.version is still %s", from)
	}
	return version, nil
}

// upgradedVersion returns the version that the line of `tent upgrade cluster --yes` says the spec has now: "cluster
// <name>: spec.nomad.version is now <version>; the cluster was on Nomad <old>."
func upgradedVersion(stdout []byte, cluster string) (string, error) {
	prefix := "cluster " + cluster + ": spec.nomad.version is now "
	for line := range strings.Lines(string(stdout)) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}
		if version, _, found := strings.Cut(rest, ";"); found && version != "" {
			return version, nil
		}
	}
	return "", fmt.Errorf("upgrade does not say %q <version>:\n%s", prefix, lastLines(string(stdout), stderrTailLines))
}

// untestedVersionPhrase is what tent's warning says about a Nomad version that its channel has not tested.
const untestedVersionPhrase = "is not tested by this tent"

// untestedVersionWarning returns the line of stderr that warns about an untested Nomad version, or "" when there is
// none.
func untestedVersionWarning(stderr []byte) string {
	for line := range strings.Lines(string(stderr)) {
		if strings.Contains(line, untestedVersionPhrase) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// checkValidVersion checks that the output of tent validate cluster has the line "cluster <name> is valid: ... run
// Nomad <version>".
func checkValidVersion(stdout []byte, cluster, version string) error {
	prefix := "cluster " + cluster + " is valid: "
	suffix := " run Nomad " + version
	for line := range strings.Lines(string(stdout)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) && strings.HasSuffix(line, suffix) {
			return nil
		}
	}
	return fmt.Errorf("validate does not say %q ... %q:\n%s", prefix, suffix, lastLines(string(stdout), stderrTailLines))
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

// registeredProblem is "" when the journal of this boot holds a registration line, Nomad lists the node with id as
// ready and eligible, and the node named after the machine is that node; otherwise it says what is missing.
func registeredProblem(lines []string, nodes []nomadNode, machine, id string) string {
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
	named, err := nodeNamed(nodes, machine)
	switch {
	case err != nil:
		return err.Error()
	case named.ID != id:
		return fmt.Sprintf("the node named %s has the ID %s, want %s", machine, named.ID, id)
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

// storedBackup is the answer of `tent backup create -o json`, as far as the scenario reads it.
type storedBackup struct {
	Name string `json:"name"`
	Size int    `json:"size"`
}

// parseStoredBackup reads the answer of `tent backup create -o json`; a backup without a name or a size is no backup.
func parseStoredBackup(out []byte) (storedBackup, error) {
	var b storedBackup
	if err := json.Unmarshal(out, &b); err != nil {
		return storedBackup{}, fmt.Errorf("read the stored backup: %w", err)
	}
	if b.Name == "" || b.Size <= 0 {
		return storedBackup{}, fmt.Errorf("the stored backup has name %q and size %d", b.Name, b.Size)
	}
	return b, nil
}

// checkBackupList checks that `tent backup list -o json` lists the backup name alone and that it is of servers,
// "current" or "earlier".
func checkBackupList(out []byte, name, servers string) error {
	var backups []struct {
		Name    string `json:"name"`
		Servers string `json:"servers"`
	}
	if err := json.Unmarshal(out, &backups); err != nil {
		return fmt.Errorf("read the backups: %w", err)
	}
	if len(backups) != 1 {
		return fmt.Errorf("%d backups listed, want 1", len(backups))
	}
	if got := backups[0]; got.Name != name || got.Servers != servers {
		return fmt.Errorf("listed backup %s of %s servers, want %s of %s servers", got.Name, got.Servers, name, servers)
	}
	return nil
}

// checkRestored checks that the output of `tent backup restore --yes` reports the restore of backup into cluster.
func checkRestored(out []byte, backup, cluster string) error {
	want := "Restored: backup " + backup + " into cluster " + cluster + "."
	if !strings.Contains(string(out), want) {
		return fmt.Errorf("the output of the restore does not hold %q: %s", want, strings.TrimSpace(string(out)))
	}
	return nil
}

// memberAddresses returns the addresses of the members, sorted.
func memberAddresses(members []agentMember) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.Addr)
	}
	slices.Sort(out)
	return out
}

// nodeAddresses returns the addresses of the nodes, sorted.
func nodeAddresses(nodes []nomadNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Address)
	}
	slices.Sort(out)
	return out
}

// stepTime is how long a part of a step took.
type stepTime struct {
	Step string
	Took time.Duration
}

// backupEvidence is what the backup step leaves to read: the stored backup, the time of each part, the private
// addresses of the servers and workers that Nomad listed before the servers were deleted and at the end, and whether
// the workers had to be rolled with --force.
type backupEvidence struct {
	Backup                       storedBackup
	Times                        []stepTime
	ServersBefore, WorkersBefore []string
	ServersAfter, WorkersAfter   []string
	Forced                       bool
}

// backupRecord writes the evidence as the text of the step's record file.
func backupRecord(e backupEvidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "backup: %s\nsize: %d\n", e.Backup.Name, e.Backup.Size)
	for _, st := range e.Times {
		fmt.Fprintf(&b, "%s: %s\n", st.Step, st.Took)
	}
	for _, l := range []struct {
		label string
		addrs []string
	}{
		{"servers before", e.ServersBefore}, {"workers before", e.WorkersBefore},
		{"servers after", e.ServersAfter}, {"workers after", e.WorkersAfter},
	} {
		fmt.Fprintf(&b, "%s: %s\n", l.label, cmp.Or(strings.Join(l.addrs, " "), "none"))
	}
	forced := "no"
	if e.Forced {
		forced = "yes"
	}
	fmt.Fprintf(&b, "forced roll of the workers: %s\n", forced)
	return b.String()
}

// sleepContext waits for d and returns nil, or returns the context's error as soon as ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
