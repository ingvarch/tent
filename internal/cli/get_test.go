package cli

import (
	"os"
	"strings"
	"testing"
)

// prodYAML is the test cluster as one spec file.
var prodYAML = docs(clusterYAML, serversYAML, workersYAML)

func TestGet(t *testing.T) {
	s := withCluster(t)
	for _, args := range [][]string{
		{"get", "prod"},
		{"get", "--name", "prod"},
		{"get", "prod", "-o", "yaml"},
	} {
		wantOK(t, runIn(t, "", append(args, "--state", s.url)...), prodYAML)
	}
	t.Setenv(envCluster, "prod")
	wantOK(t, runIn(t, "", "get", "--state", s.url), prodYAML)
}

// TestGetRoundTrip prints what create -f read.
func TestGetRoundTrip(t *testing.T) {
	s := newState(t)
	spec := docs(replaced(t, clusterYAML, "    vultr: {}\n", "    vultr: {}\n  nomad:\n    version: 2.0.7\n"),
		serversYAML, replaced(t, workersYAML, "size: 3\n", "size: 3\n  nomad:\n    drivers:\n      - docker\n"))
	wantDone(t, runIn(t, spec, "create", "-f", "-", "--state", s.url), createdProd)
	wantOK(t, runIn(t, "", "get", "prod", "--state", s.url), spec)
}

// The test cluster with the defaults filled in.
const (
	fullClusterYAML = `apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: prod
spec:
  channel: stable
  cloud:
    provider: vultr
    region: ams
    zones:
      - ams
    vultr: {}
  networking:
    cidr: 10.64.0.0/16
  access:
    api:
      - 0.0.0.0/0
  nomad:
    region: global
    tls:
      verifyHTTPSClient: true
    clientIntroduction: strict
`
	fullServersYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: servers
  cluster: prod
spec:
  role: server
  machineType: vc2-2c-4gb
  image: ubuntu-24.04
  size: 3
  zones:
    - ams
`
	fullWorkersYAML = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: workers
  cluster: prod
spec:
  role: client
  machineType: vc2-2c-4gb
  image: ubuntu-24.04
  size: 3
  zones:
    - ams
  nomad:
    nodePool: default
  rollingUpdate:
    maxSurge: 1
    maxUnavailable: 0
    drainTimeout: 1h
`
)

func TestGetFull(t *testing.T) {
	s := withCluster(t)
	wantOK(t, runIn(t, "", "get", "prod", "--full", "--state", s.url),
		docs(fullClusterYAML, fullServersYAML, fullWorkersYAML))
	wantOK(t, runIn(t, "", "get", "clusters", "--full", "-o", "yaml", "--state", s.url), fullClusterYAML)
	wantOK(t, runIn(t, "", "get", "nodegroups", "--name", "prod", "--full", "-o", "yaml", "--state", s.url),
		docs(fullServersYAML, fullWorkersYAML))
	s.want(t, prodObjects) // the store keeps the user specs
}

func TestGetJSON(t *testing.T) {
	s := newState(t)
	s.put(t, clusterPath, clusterYAML+"  nomad:\n    extraConfig:\n      server: a <b> & c\n")
	s.put(t, serversPath, serversYAML)
	wantOK(t, runIn(t, "", "get", "prod", "-o", "json", "--state", s.url), `[
  {
    "apiVersion": "tent/v1alpha1",
    "kind": "Cluster",
    "metadata": {
      "name": "prod"
    },
    "spec": {
      "cloud": {
        "provider": "vultr",
        "region": "ams",
        "vultr": {}
      },
      "nomad": {
        "extraConfig": {
          "server": "a <b> & c"
        }
      }
    }
  },
  {
    "apiVersion": "tent/v1alpha1",
    "kind": "NodeGroup",
    "metadata": {
      "name": "servers",
      "cluster": "prod"
    },
    "spec": {
      "role": "server",
      "machineType": "vc2-2c-4gb",
      "size": 3
    }
  }
]
`)
}

// TestGetTakesOneName says how to list several clusters. A mistyped subcommand is TestMistypedSubcommands.
func TestGetTakesOneName(t *testing.T) {
	s := withTwoClusters(t)
	wantError(t, runIn(t, "", "get", "prod", "dev", "--state", s.url),
		"Error: get takes one NAME; list several clusters with get clusters prod dev\n")
}

func TestGetMissingCluster(t *testing.T) {
	s := withCluster(t)
	notFound := "Error: cluster dev not found in " + s.url + "\n"
	wantError(t, runIn(t, "", "get", "dev", "--state", s.url), notFound)
	wantError(t, runIn(t, "", "get", "nodegroups", "--name", "dev", "--state", s.url), notFound)
	wantError(t, runIn(t, "", "get", "clusters", "prod", "dev", "--state", s.url), notFound)
}

func TestGetWithoutStateOrName(t *testing.T) {
	path := configFile(t)
	wantError(t, runIn(t, "", "get", "prod"),
		"Error: no state store: set --state, TENT_STATE or \"state\" in "+path+"\n")
	wantError(t, runIn(t, "", "get", "--state", newState(t).url),
		"Error: no cluster name: give NAME or set --name, TENT_CLUSTER or \"cluster\" in "+path+"\n")
	wantError(t, runIn(t, "", "get", "clusters"),
		"Error: no state store: set --state, TENT_STATE or \"state\" in "+path+"\n")
}

// withTwoClusters returns a store with the test cluster, which has one more client group, and a cluster dev.
func withTwoClusters(t *testing.T) state {
	t.Helper()
	s := withCluster(t)
	s.put(t, "prod/nodegroups/batch.yaml", replaced(t, replaced(t, workersYAML, "name: workers", "name: batch"),
		"size: 3", "size: 2"))
	s.put(t, "dev/cluster.yaml", devYAML)
	s.put(t, "dev/nodegroups/nodes.yaml", `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: nodes
  cluster: dev
spec:
  role: combined
  machineType: cx23
  size: 1
  zones:
    - fsn1
`)
	return s
}

const devYAML = `apiVersion: tent/v1alpha1
kind: Cluster
metadata:
  name: dev
spec:
  cloud:
    provider: hetzner
    region: eu-central
    zones:
      - fsn1
      - nbg1
    hetzner: {}
  nomad:
    version: 2.0.7
`

func TestGetClusters(t *testing.T) {
	s := withTwoClusters(t)
	const header = "NAME   PROVIDER   REGION       NOMAD   SERVERS   WORKERS\n"
	const dev = "dev    hetzner    eu-central   2.0.7   1         0\n"
	const prod = "prod   vultr      ams          -       3         5\n"
	wantOK(t, runIn(t, "", "get", "clusters", "--state", s.url), header+dev+prod)
	wantOK(t, runIn(t, "", "get", "cluster", "--state", s.url), header+dev+prod)
	wantOK(t, runIn(t, "", "get", "clusters", "prod", "--state", s.url),
		"NAME   PROVIDER   REGION   NOMAD   SERVERS   WORKERS\nprod   vultr      ams      -       3         5\n")
	wantOK(t, runIn(t, "", "get", "clusters", "-o", "yaml", "--state", s.url), docs(devYAML, clusterYAML))
	wantOK(t, runIn(t, "", "get", "clusters", "prod", "-o", "json", "--state", s.url), `[
  {
    "apiVersion": "tent/v1alpha1",
    "kind": "Cluster",
    "metadata": {
      "name": "prod"
    },
    "spec": {
      "cloud": {
        "provider": "vultr",
        "region": "ams",
        "vultr": {}
      }
    }
  }
]
`)
}

// TestGetClustersOfEmptyStore says on stderr that there are none, so that a mistyped --state shows.
func TestGetClustersOfEmptyStore(t *testing.T) {
	s := newState(t)
	none := "no clusters in " + s.url + "\n"
	check := func() {
		t.Helper()
		wantResult(t, runIn(t, "", "get", "clusters", "--state", s.url), 0,
			"NAME   PROVIDER   REGION   NOMAD   SERVERS   WORKERS\n", none)
		wantResult(t, runIn(t, "", "get", "clusters", "-o", "yaml", "--state", s.url), 0, "", none)
		wantResult(t, runIn(t, "", "get", "clusters", "-o", "json", "--state", s.url), 0, "[]\n", none)
	}
	check() // the directory is missing
	s.wantEmpty(t)
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestGetNodeGroups(t *testing.T) {
	s := withTwoClusters(t)
	wantOK(t, runIn(t, "", "get", "nodegroups", "--name", "prod", "--state", s.url), ""+
		"NAME      ROLE     MACHINE-TYPE   SIZE   ZONES\n"+
		"batch     client   vc2-2c-4gb     2      -\n"+
		"servers   server   vc2-2c-4gb     3      -\n"+
		"workers   client   vc2-2c-4gb     3      -\n")
	wantOK(t, runIn(t, "", "get", "nodegroup", "--name", "dev", "--full", "--state", s.url), ""+
		"NAME    ROLE       MACHINE-TYPE   SIZE   ZONES\n"+
		"nodes   combined   cx23           1      fsn1\n")
	wantOK(t, runIn(t, "", "get", "nodegroups", "--name", "prod", "--full", "--state", s.url), ""+
		"NAME      ROLE     MACHINE-TYPE   SIZE   ZONES\n"+
		"batch     client   vc2-2c-4gb     2      ams\n"+
		"servers   server   vc2-2c-4gb     3      ams\n"+
		"workers   client   vc2-2c-4gb     3      ams\n")
}

func TestGetNodeGroupsYAMLAndJSON(t *testing.T) {
	s := withCluster(t)
	wantOK(t, runIn(t, "", "get", "nodegroups", "--name", "prod", "-o", "yaml", "--state", s.url),
		docs(serversYAML, workersYAML))
	s.put(t, workersPath, replaced(t, workersYAML, "size: 3\n", "size: 3\n  zones:\n    - ams\n"))
	wantOK(t, runIn(t, "", "get", "nodegroups", "--name", "prod", "-o", "json", "--state", s.url), `[
  {
    "apiVersion": "tent/v1alpha1",
    "kind": "NodeGroup",
    "metadata": {
      "name": "servers",
      "cluster": "prod"
    },
    "spec": {
      "role": "server",
      "machineType": "vc2-2c-4gb",
      "size": 3
    }
  },
  {
    "apiVersion": "tent/v1alpha1",
    "kind": "NodeGroup",
    "metadata": {
      "name": "workers",
      "cluster": "prod"
    },
    "spec": {
      "role": "client",
      "machineType": "vc2-2c-4gb",
      "size": 3,
      "zones": [
        "ams"
      ]
    }
  }
]
`)
}

// TestGetJSONRoundTrip feeds what get -o json prints to create -f and replace -f.
func TestGetJSONRoundTrip(t *testing.T) {
	s := withCluster(t)
	j := runIn(t, "", "get", "prod", "-o", "json", "--state", s.url).out
	wantResult(t, runIn(t, j, "replace", "-f", "-", "--state", s.url), 0,
		"cluster prod unchanged\nnode group servers unchanged\nnode group workers unchanged\n", openAPIWarning)
	other := newState(t)
	wantResult(t, runIn(t, j, "create", "-f", "-", "--state", other.url), 0, createdProd, openAPIWarning)
	other.want(t, prodObjects)
}

func TestGetNamedNodeGroups(t *testing.T) {
	s := withTwoClusters(t)
	wantOK(t, runIn(t, "", "get", "nodegroups", "workers", "batch", "--name", "prod", "--state", s.url), ""+
		"NAME      ROLE     MACHINE-TYPE   SIZE   ZONES\n"+
		"workers   client   vc2-2c-4gb     3      -\n"+
		"batch     client   vc2-2c-4gb     2      -\n")
	wantOK(t, runIn(t, "", "get", "nodegroups", "workers", "-o", "yaml", "--name", "prod", "--state", s.url),
		workersYAML)
	wantError(t, runIn(t, "", "get", "nodegroups", "web", "--name", "prod", "--state", s.url),
		"Error: node group web of cluster prod not found in "+s.url+"\n")
}

func TestGetHelpNamesTheSubcommandNames(t *testing.T) {
	got := runIn(t, "", "get", "--help")
	const want = "For a cluster named cluster, clusters, nodegroup or nodegroups, use --name."
	if got.code != 0 || !strings.Contains(got.out, want) {
		t.Errorf("exit code %d, help\n%s\nwant it to hold %q", got.code, got.out, want)
	}
}
