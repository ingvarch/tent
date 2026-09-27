package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createdProd is what creating the test cluster prints.
const createdProd = "cluster prod created\nnode group servers created\nnode group workers created\n"

// prodObjects are the objects of the test cluster in the store.
var prodObjects = map[string]string{clusterPath: clusterYAML, serversPath: serversYAML, workersPath: workersYAML}

func TestCreateFromFile(t *testing.T) {
	s := newState(t)
	file := writeFile(t, "prod.yaml", docs(clusterYAML, serversYAML, workersYAML))
	wantDone(t, runIn(t, "", "create", "-f", file, "--state", s.url), createdProd)
	s.want(t, prodObjects)
}

func TestCreateFromStdin(t *testing.T) {
	s := newState(t)
	spec := docs(workersYAML, clusterYAML, serversYAML)
	wantDone(t, runIn(t, spec, "create", "-f", "-", "--state", s.url), createdProd)
	s.want(t, prodObjects)
}

func TestCreatePrintsChangesAsJSONAndYAML(t *testing.T) {
	spec := docs(clusterYAML, serversYAML)
	for _, tc := range []struct {
		format string
		want   string
	}{
		{"json", `[
  {
    "kind": "Cluster",
    "name": "prod",
    "action": "created"
  },
  {
    "kind": "NodeGroup",
    "name": "servers",
    "action": "created"
  }
]
`},
		{"yaml", `- action: created
  kind: Cluster
  name: prod
- action: created
  kind: NodeGroup
  name: servers
`},
	} {
		t.Run(tc.format, func(t *testing.T) {
			s := newState(t)
			wantDone(t, runIn(t, spec, "create", "-f", "-", "-o", tc.format, "--state", s.url), tc.want)
		})
	}
}

func TestCreateDecodeError(t *testing.T) {
	s := newState(t)
	spec := docs(clusterYAML, replaced(t, serversYAML, "size: 3", "sizee: 3"))
	wantError(t, runIn(t, spec, "create", "-f", "-", "--state", s.url),
		"Error: document 2 (NodeGroup): line 19: unknown field \"spec.sizee\"\n")
	s.wantEmpty(t)
}

func TestCreateFromMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := os.ReadFile(missing)
	wantError(t, runIn(t, "", "create", "-f", missing, "--state", newState(t).url),
		"Error: reading the spec: "+err.Error()+"\n")
}

func TestCreateWithoutFileShowsUsage(t *testing.T) {
	got := runIn(t, "", "create")
	if got.code != 0 || got.errOut != "" {
		t.Errorf("exit code = %d, stderr = %q; want 0 and nothing", got.code, got.errOut)
	}
	if want := "Usage:\n  tent create [flags]\n  tent create [command]\n"; !strings.Contains(got.out, want) {
		t.Errorf("stdout\n%s\nwant it to hold\n%s", got.out, want)
	}
}

// testSSHKey is a valid public key.
const testSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILVMgcq7nf63leSBwZNfB40Oi4XwSKWNKchNmRGNCb9k ops@example"

// createProd returns the arguments that generate the test cluster, followed by more.
func createProd(s state, more ...string) []string {
	return append([]string{
		"create", "cluster", "prod", "--provider", "vultr", "--region", "ams", "--machine-type", "vc2-2c-4gb",
		"--state", s.url,
	}, more...)
}

func TestCreateCluster(t *testing.T) {
	s := newState(t)
	wantDone(t, runIn(t, "", createProd(s)...), createdProd)
	s.want(t, prodObjects)
}

func TestCreateClusterNamedByFlag(t *testing.T) {
	s := newState(t)
	args := []string{"create", "cluster", "--name", "prod", "--provider", "vultr", "--region", "ams",
		"--machine-type", "vc2-2c-4gb", "--state", s.url}
	wantDone(t, runIn(t, "", args...), createdProd)
	s.want(t, prodObjects)
}

func TestCreateClusterCombined(t *testing.T) {
	s := newState(t)
	wantDone(t, runIn(t, "", createProd(s, "--combined")...), "cluster prod created\nnode group nodes created\n")
	s.want(t, map[string]string{clusterPath: clusterYAML, "prod/nodegroups/nodes.yaml": `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: nodes
  cluster: prod
spec:
  role: combined
  machineType: vc2-2c-4gb
  size: 3
`})
}

func TestCreateClusterCombinedTakesItsSizeFromServers(t *testing.T) {
	s := newState(t)
	five := runIn(t, "", createProd(s, "--combined", "--servers", "5", "--dry-run")...)
	wantOK(t, five, docs(clusterYAML, `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: nodes
  cluster: prod
spec:
  role: combined
  machineType: vc2-2c-4gb
  size: 5
`))
}

func TestCreateClusterWorkerMachineType(t *testing.T) {
	s := newState(t)
	wantDone(t, runIn(t, "", createProd(s, "--worker-machine-type", "vc2-4c-8gb")...), createdProd)
	s.want(t, map[string]string{
		clusterPath: clusterYAML, serversPath: serversYAML,
		workersPath: replaced(t, workersYAML, "vc2-2c-4gb", "vc2-4c-8gb"),
	})
}

func TestCreateClusterEveryFlag(t *testing.T) {
	s := newState(t)
	key := writeFile(t, "id_ed25519.pub", testSSHKey+"\n")
	wantOK(t, runIn(t, "", "create", "cluster", "prod", "--state", s.url,
		"--provider", "hetzner", "--region", "eu-central", "--zones", "fsn1,nbg1", "--zones", "hel1",
		"--machine-type", "cx23", "--worker-machine-type", "cx33", "--servers", "5", "--workers", "2",
		"--image", "ubuntu-26.04", "--ssh-key", key, "--ssh-access", "203.0.113.7/32,198.51.100.0/24",
		"--api-access", "203.0.113.7/32", "--api-access", "198.51.100.0/24", "--nomad-version", "2.0.7",
	), createdProd)
	s.want(t, map[string]string{
		clusterPath: `apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: prod
spec:
  cloud:
    provider: hetzner
    region: eu-central
    zones:
      - fsn1
      - nbg1
      - hel1
    hetzner: {}
  access:
    ssh:
      - 203.0.113.7/32
      - 198.51.100.0/24
    api:
      - 203.0.113.7/32
      - 198.51.100.0/24
  sshKeys:
    - ` + testSSHKey + `
  nomad:
    version: 2.0.7
`,
		serversPath: `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: servers
  cluster: prod
spec:
  role: server
  machineType: cx23
  image: ubuntu-26.04
  size: 5
`,
		workersPath: `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: workers
  cluster: prod
spec:
  role: client
  machineType: cx33
  image: ubuntu-26.04
  size: 2
`,
	})
}

func TestCreateClusterDryRun(t *testing.T) {
	s := newState(t)
	want := docs(clusterYAML, serversYAML, workersYAML)
	wantOK(t, runIn(t, "", createProd(s, "--dry-run")...), want)
	wantOK(t, runIn(t, "", createProd(s, "--dry-run", "-o", "yaml")...), want)
	s.wantEmpty(t)
}

// TestCreateClusterDryRunChecksTheSpecs fails as create would, and writes nothing.
func TestCreateClusterDryRunChecksTheSpecs(t *testing.T) {
	s := newState(t)
	wantError(t, runIn(t, "", createProd(s, "--dry-run", "--servers", "2")...),
		"Error: invalid spec:\n  NodeGroup servers: spec.size: must be 1, 3 or 5 for role=server\n")
	s.wantEmpty(t)

	wantDone(t, runIn(t, "", createProd(s)...), createdProd)
	wantError(t, runIn(t, "", createProd(s, "--dry-run", "--nomad-version", "2.0.7")...),
		"Error: cluster prod already exists; change it with edit or replace -f\n")
	s.want(t, prodObjects)
}

// TestCreateClusterDryRunWithoutState checks the specs on their own.
func TestCreateClusterDryRunWithoutState(t *testing.T) {
	s := newState(t)
	wantOK(t, runIn(t, "", createProd(s, "--dry-run", "--state", "")...), prodYAML)
	wantError(t, runIn(t, "", createProd(s, "--dry-run", "--state", "", "--servers", "2")...),
		"Error: invalid spec:\n  NodeGroup servers: spec.size: must be 1, 3 or 5 for role=server\n")
	wantOK(t, runIn(t, "", createProd(s, "--dry-run", "--state", "", "--servers", "1", "--allow-single-server")...),
		replaced(t, prodYAML, "size: 3", "size: 1"))
	s.wantEmpty(t)
}

// TestCreateClusterDryRunJSON prints what get prints once the cluster is created.
func TestCreateClusterDryRunJSON(t *testing.T) {
	s := newState(t)
	dry := runIn(t, "", createProd(s, "--dry-run", "-o", "json")...)
	s.wantEmpty(t)
	wantDone(t, runIn(t, "", createProd(s)...), createdProd)
	wantOK(t, dry, runIn(t, "", "get", "prod", "-o", "json", "--state", s.url).out)
}

func TestCreateClusterMissingFlags(t *testing.T) {
	s := newState(t)
	wantError(t, runIn(t, "", "create", "cluster", "prod", "--state", s.url),
		"Error: required flag(s) \"machine-type\", \"provider\", \"region\" not set\n")
	wantError(t, runIn(t, "", "create", "cluster", "prod", "dev", "--state", s.url),
		"Error: accepts at most 1 arg(s), received 2\n")
	s.wantEmpty(t)
}

func TestCreateClusterWithoutName(t *testing.T) {
	path := configFile(t)
	wantError(t, runIn(t, "", "create", "cluster", "--provider", "vultr", "--region", "ams",
		"--machine-type", "vc2-2c-4gb", "--dry-run"),
		"Error: no cluster name: give NAME or set --name, TENT_CLUSTER or \"cluster\" in "+path+"\n")
}

func TestCreateClusterCombinedTakesNoWorkers(t *testing.T) {
	s := newState(t)
	const refused = "Error: --combined makes one group of --servers nodes; it takes no --workers or " +
		"--worker-machine-type\n"
	wantError(t, runIn(t, "", createProd(s, "--combined", "--workers", "2")...), refused)
	wantError(t, runIn(t, "", createProd(s, "--combined", "--worker-machine-type", "vc2-4c-8gb")...), refused)
	s.wantEmpty(t)
}

func TestCreateClusterHelp(t *testing.T) {
	got := runIn(t, "", "create", "cluster", "--help")
	if got.code != 0 {
		t.Fatalf("exit code = %d, stderr %q", got.code, got.errOut)
	}
	for _, flag := range []struct{ name, usage string }{
		{"--provider PROVIDER", "cloud PROVIDER: vultr or hetzner; the spec gets its empty block, vultr: {} or " +
			"hetzner: {} (required)"},
		{"--region REGION", "REGION: a Vultr region, such as ams, or a Hetzner network zone, such as eu-central " +
			"(required)"},
		{"--machine-type TYPE", "machine TYPE of every node, such as vc2-2c-4gb or cx23 (required)"},
		{"--worker-machine-type TYPE", "machine TYPE of the workers, if not --machine-type"},
		{"--zones LOCATIONS", "Hetzner LOCATIONS to spread the nodes over, such as fsn1,nbg1,hel1"},
		{"--image IMAGE", "operating system IMAGE of every node (default ubuntu-24.04)"},
		{"--ssh-key PATH", "public SSH key file at PATH to install on every node; repeat for more keys"},
		{"--ssh-access CIDR", "CIDR that may reach SSH; repeat or separate with commas (default none)"},
		{"--api-access CIDR", "CIDR that may reach the Nomad API; repeat or separate with commas (default " +
			"0.0.0.0/0)"},
		{"--nomad-version VERSION", "Nomad VERSION, such as 2.0.7 (default: the channel's recommended one)"},
	} {
		if !helpHas(got.out, flag.name, flag.usage) {
			t.Errorf("help has no line for %s: %s\n%s", flag.name, flag.usage, got.out)
		}
	}
}

// helpHas reports whether help has a line for the flag with its value's name and usage.
func helpHas(help, flag, usage string) bool {
	for line := range strings.Lines(help) {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, flag+" "); ok && strings.TrimSpace(rest) == usage {
			return true
		}
	}
	return false
}

func TestCreateClusterInvalid(t *testing.T) {
	s := newState(t)
	args := []string{"create", "cluster", "prod", "--provider", "aws", "--region", "ams", "--machine-type", "m7i",
		"--servers", "2", "--state", s.url}
	wantError(t, runIn(t, "", args...), `Error: invalid spec:
  Cluster prod: spec.cloud.provider: must be one of vultr, hetzner
  NodeGroup servers: spec.size: must be 1, 3 or 5 for role=server
`)
	s.wantEmpty(t)
}

func TestCreateClusterSingleServer(t *testing.T) {
	s := newState(t)
	wantError(t, runIn(t, "", createProd(s, "--servers", "1")...),
		"Error: invalid spec:\n  NodeGroup servers: spec.size: size 1 needs --allow-single-server\n")
	s.wantEmpty(t)
	wantDone(t, runIn(t, "", createProd(s, "--servers", "1", "--allow-single-server")...), createdProd)
	s.want(t, map[string]string{
		clusterPath: clusterYAML, serversPath: replaced(t, serversYAML, "size: 3", "size: 1"), workersPath: workersYAML,
	})
}

func TestCreateClusterTwice(t *testing.T) {
	s := newState(t)
	wantDone(t, runIn(t, "", createProd(s)...), createdProd)
	wantDone(t, runIn(t, "", createProd(s)...),
		"cluster prod unchanged\nnode group servers unchanged\nnode group workers unchanged\n")

	const exists = " already exists; change it with edit or replace -f"
	wantError(t, runIn(t, "", createProd(s, "--nomad-version", "2.0.7")...), "Error: cluster prod"+exists+"\n")
	wantError(t, runIn(t, "", createProd(s, "--nomad-version", "2.0.7", "--workers", "5")...),
		"Error: cluster prod"+exists+"\n  node group workers of cluster prod"+exists+"\n")
	s.want(t, prodObjects)
}

func TestCreateClusterSSHKeyFiles(t *testing.T) {
	s := newState(t)
	missing := filepath.Join(t.TempDir(), "nope.pub")
	_, err := os.ReadFile(missing)
	wantError(t, runIn(t, "", createProd(s, "--ssh-key", missing)...), "Error: reading --ssh-key: "+err.Error()+"\n")

	private := writeFile(t, "id_ed25519", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk=\n"+
		"-----END OPENSSH PRIVATE KEY-----\n")
	wantError(t, runIn(t, "", createProd(s, "--ssh-key", private, "--dry-run")...),
		"Error: --ssh-key "+private+" is a private key: give the public key, the .pub file\n")
	s.wantEmpty(t)
}

// TestCreateWithoutStateReadsNoSpec fails before it reads standard input, where a person may be typing.
func TestCreateWithoutStateReadsNoSpec(t *testing.T) {
	path := configFile(t)
	wantError(t, runIn(t, "not: [yaml", "create", "-f", "-"),
		"Error: no state store: set --state, TENT_STATE or \"state\" in "+path+"\n")
}

func TestCreateNodeGroupWithoutCluster(t *testing.T) {
	s := newState(t)
	wantError(t, runIn(t, replaced(t, workersYAML, "  cluster: prod\n", ""), "create", "-f", "-", "--state", s.url),
		"Error: invalid spec:\n  NodeGroup workers: metadata.cluster: required\n")
	s.wantEmpty(t)
}

func TestCreateClusterSSHKeyFileOfTwoKeys(t *testing.T) {
	s := newState(t)
	two := writeFile(t, "keys.pub", testSSHKey+"\n\n"+testSSHKey+"2\n")
	wantError(t, runIn(t, "", createProd(s, "--ssh-key", two)...),
		"Error: --ssh-key "+two+" holds 2 keys: give one key per file\n")
	s.wantEmpty(t)

	// A key with a Windows line ending is one key.
	crlf := writeFile(t, "id_ed25519.pub", testSSHKey+"\r\n")
	wantResult(t, runIn(t, "", createProd(s, "--ssh-key", crlf, "--dry-run")...), 0,
		replaced(t, prodYAML, "    vultr: {}\n", "    vultr: {}\n  sshKeys:\n    - "+testSSHKey+"\n"), "")
}
