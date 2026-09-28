package cli

import "testing"

// biggerWorkers is the workers group of the test cluster with 5 nodes.
func biggerWorkers(t *testing.T) string { return replaced(t, workersYAML, "size: 3", "size: 5") }

const replacedWorkers = "cluster prod unchanged\nnode group workers replaced\n"

func TestReplaceFromFile(t *testing.T) {
	s := withCluster(t)
	file := writeFile(t, "prod.yaml", docs(clusterYAML, biggerWorkers(t)))
	wantDone(t, runIn(t, "", "replace", "-f", file, "--state", s.url), replacedWorkers)
	s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML, workersPath: biggerWorkers(t)})

	wantDone(t, runIn(t, "", "replace", "-f", file, "--state", s.url),
		"cluster prod unchanged\nnode group workers unchanged\n")
}

func TestReplaceFromStdin(t *testing.T) {
	s := withCluster(t)
	wantDone(t, runIn(t, docs(clusterYAML, biggerWorkers(t)), "replace", "-f", "-", "--state", s.url), replacedWorkers)
	s.want(t, map[string]string{clusterPath: clusterYAML, serversPath: serversYAML, workersPath: biggerWorkers(t)})
}

func TestReplaceNeedsAFile(t *testing.T) {
	wantError(t, runIn(t, "", "replace", "--state", withCluster(t).url),
		"Error: required flag(s) \"filename\" not set\n")
}

func TestReplaceMissing(t *testing.T) {
	s := withCluster(t)
	web := replaced(t, workersYAML, "name: workers", "name: web")
	wantError(t, runIn(t, web, "replace", "-f", "-", "--state", s.url),
		"Error: node group web of cluster prod not found in "+s.url+"; create it with create -f\n")
	s.want(t, prodObjects)
}

func TestReplaceDecodeError(t *testing.T) {
	s := withCluster(t)
	wantError(t, runIn(t, "kind: NodeGroup\napiVersion: tent/v1alpha2\n", "replace", "-f", "-", "--state", s.url),
		"Error: document 1 (NodeGroup): line 2: unsupported apiVersion \"tent/v1alpha2\", want tent/v1alpha1\n")
	s.want(t, prodObjects)
}

func TestReplaceSingleServer(t *testing.T) {
	s := withCluster(t)
	single := replaced(t, serversYAML, "size: 3", "size: 1")
	wantError(t, runIn(t, single, "replace", "-f", "-", "--state", s.url),
		"Error: invalid spec:\n  NodeGroup servers: spec.size: size 1 needs --allow-single-server\n")
	s.want(t, prodObjects)
	wantDone(t, runIn(t, single, "replace", "-f", "-", "--allow-single-server", "--state", s.url),
		"node group servers replaced\n")
}

// TestReplaceKeepsTheCloud refuses a Cluster on another provider or in another region.
func TestReplaceKeepsTheCloud(t *testing.T) {
	s := withCluster(t)
	hetzner := replaced(t, replaced(t, clusterYAML, "provider: vultr", "provider: hetzner"),
		"region: ams\n    vultr: {}", "region: eu-central\n    zones: [fsn1, nbg1, hel1]\n    hetzner: {}")
	const moves = "; a cluster moves by creating a new one\n"
	wantError(t, runIn(t, hetzner, "replace", "-f", "-", "--state", s.url), "Error: invalid spec:\n"+
		"  Cluster prod: spec.cloud.provider: cannot change from vultr to hetzner"+moves+
		"  Cluster prod: spec.cloud.region: cannot change from ams to eu-central"+moves)
	s.want(t, prodObjects)
}

func TestSpecFileFlagHelp(t *testing.T) {
	for _, name := range []string{"create", "replace"} {
		got := runIn(t, "", name, "--help")
		if !helpHas(got.out, "-f, --filename FILE", "spec FILE, or - for standard input") {
			t.Errorf("tent %s --help has no line for -f FILE:\n%s", name, got.out)
		}
	}
}

func TestEmptySpecFileFlag(t *testing.T) {
	s := withCluster(t)
	for _, name := range []string{"create", "replace"} {
		wantError(t, runIn(t, "", name, "-f", "", "--state", s.url),
			"Error: -f needs a file path, or - for standard input\n")
	}
	s.want(t, prodObjects)
}

// TestBrokenClusterSaysHowToRepairIt refuses every change of a cluster whose stored cluster.yaml does not decode, and
// says how to repair the file.
func TestBrokenClusterSaysHowToRepairIt(t *testing.T) {
	s := withCluster(t)
	s.put(t, clusterPath, "kind: Cluster\n")
	stored := s.objects(t)
	e := newFakeEditor(t, "")
	const broken = "Error: " + clusterPath + ": document 1 (Cluster): apiVersion is required; fix " + clusterPath +
		" in the state store by hand and keep its spec.cloud.provider and spec.cloud.region, since deleting the file " +
		"would leave the cloud objects of the cluster behind\n"
	wantError(t, runIn(t, clusterYAML, "replace", "-f", "-", "--state", s.url), broken)
	wantError(t, runEdit(t, "", "edit", "cluster", "prod", "--state", s.url), broken)
	for _, args := range [][]string{{}, {"--yes"}, {"--yes", "--force"}} {
		wantError(t, runOnCloud(t, append([]string{"delete", "cluster", "prod", "--state", s.url}, args...)...), broken)
	}
	s.want(t, stored)
	e.wantRuns(t)
}
