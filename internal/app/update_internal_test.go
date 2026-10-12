package app

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/channels"
	"github.com/ingvarch/tent/internal/spec"
	"github.com/ingvarch/tent/internal/statestore"
)

// TestApplyNodeRefusesACreateAndAWait fails a create and a wait, for which applyNode has no user data: it reports the
// change as started and as failed, returns the error, and asks the cloud for no machine.
func TestApplyNodeRefusesACreateAndAWait(t *testing.T) {
	t.Parallel()
	const cause = "node prod-servers-0: no user data"
	for _, tc := range []struct {
		action NodeAction
		want   string
	}{
		{NodeCreate, cause},
		{NodeWait, "wait for node prod-servers-0: " + cause},
	} {
		t.Run(tc.action.String(), func(t *testing.T) {
			var got []string
			s := testService(&got)
			nodes := &recordingNodes{}
			c := NodeChange{Action: tc.action, Name: "prod-servers-0"}

			err := s.applyNode(t.Context(), nodes, "prod", c)

			if err == nil || err.Error() != tc.want {
				t.Errorf("applyNode error = %v\nwant            %s", err, tc.want)
			}
			if len(nodes.creates) != 0 {
				t.Errorf("applyNode asked the cloud for %d machines, want none", len(nodes.creates))
			}
			change := tc.action.String() + " prod-servers-0 "
			if diff := cmp.Diff([]string{change + "started", change + "failed: " + cause}, got); diff != "" {
				t.Errorf("the steps that applyNode reported (-want +got):\n%s", diff)
			}
		})
	}
}

// pinTestSpec is a Cluster of cluster prod that sets the Nomad version v, or none when v is empty.
func pinTestSpec(v string) string {
	doc := "apiVersion: tent/v1alpha1\nkind: Cluster\nmetadata:\n  name: prod\nspec:\n  cloud:\n" +
		"    provider: vultr\n    region: ams\n"
	if v != "" {
		doc += "  nomad:\n    version: \"" + v + "\"\n"
	}
	return doc
}

// pinTestGroup is a node group of the cluster prod.
const pinTestGroup = `apiVersion: tent/v1alpha1
kind: NodeGroup
metadata:
  name: servers
  cluster: prod
spec:
  role: server
  machineType: vc2-2c-4gb
  size: 3
`

// brokenCause is why the spec "kind: Cluster" does not decode.
const brokenCause = "document 1 (Cluster): apiVersion is required"

// pinTestLayout returns the layout of cluster prod.
func pinTestLayout(t *testing.T) statestore.Layout {
	t.Helper()
	l, err := statestore.NewLayout("prod")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// pinTestChannel is a channel that allows Nomad from 2.0.0 and recommends 2.0.7.
func pinTestChannel() *channels.Channel {
	return &channels.Channel{Name: "stable", Nomad: channels.Nomad{Minimum: "2.0.0", Recommended: "2.0.7"}}
}

// TestStoredPin reads the Nomad version of a stored completed spec: none for no spec, the version for a spec with
// one, and an error that names the path for a spec that does not decode or holds no Cluster.
func TestStoredPin(t *testing.T) {
	t.Parallel()
	l := pinTestLayout(t)
	for _, tc := range []struct {
		name    string
		stored  []byte
		want    string
		wantErr string
	}{
		{"no spec", nil, "", ""},
		{"a version", []byte(pinTestSpec("2.0.6")), "2.0.6", ""},
		{"no version", []byte(pinTestSpec("")), "", ""},
		{"broken", []byte("kind: Cluster\n"), "", "prod/cluster.completed.yaml: " + brokenCause},
		{"node groups only", []byte(pinTestGroup), "", "prod/cluster.completed.yaml: holds no Cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := storedPin(l, tc.stored)
			if got != tc.want || (err == nil) != (tc.wantErr == "") || err != nil && err.Error() != tc.wantErr {
				t.Errorf("storedPin = %q, %v\nwant        %q, %q", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestMoveProblem says why a move of the Nomad version is refused and what to do, and nothing for an allowed move.
func TestMoveProblem(t *testing.T) {
	t.Parallel()
	const pinnedTo = ", the Nomad version that the cluster is pinned to"
	for _, tc := range []struct {
		pinned, to, why, advice string
	}{
		{"2.0.7", "2.2.0", "2.2.0 is more than one minor version above 2.0.7" + pinnedTo, "move to 2.1 first"},
		{"2.9.0", "2.11.0", "2.11.0 is more than one minor version above 2.9.0" + pinnedTo, "move to 2.10 first"},
		{"1.11.0", "2.0.7", "2.0.7 is of another major version than 1.11.0" + pinnedTo, ""},
		{"2.0.7", "2.1.5", "", ""},
		{"2.0.7", "2.0.6", "", ""},
		{"", "2.2.0", "", ""},
	} {
		why, advice := moveProblem(tc.pinned, tc.to)
		if why != tc.why || advice != tc.advice {
			t.Errorf("moveProblem(%q, %q) = %q, %q\nwant %q, %q", tc.pinned, tc.to, why, advice, tc.why, tc.advice)
		}
	}
}

// TestPinVersion sets the Nomad version of a cluster that leaves it out to the pin, or else to the channel's
// recommended one, holds a version that the spec sets to the pin, and returns the pin.
func TestPinVersion(t *testing.T) {
	t.Parallel()
	l := pinTestLayout(t)
	const (
		hint = "; set spec.nomad.version to the Nomad version the cluster was built with, or fix " +
			"prod/cluster.completed.yaml in the state store by hand"
		broken = "prod/cluster.completed.yaml: " + brokenCause
	)
	for _, tc := range []struct {
		name       string
		version    string // the spec's
		stored     []byte
		wantUsed   string // the spec's version afterwards
		wantPinned string
		wantErr    string
	}{
		{"nothing set or pinned", "", nil, "2.0.7", "", ""},
		{"only pinned", "", []byte(pinTestSpec("2.0.6")), "2.0.6", "2.0.6", ""},
		{"a spec that sets the pin", "2.0.6", []byte(pinTestSpec("2.0.6")), "2.0.6", "2.0.6", ""},
		{"a spec with a version and no pin", "2.2.0", nil, "2.2.0", "", ""},
		{"a spec with a version older than the pin", "2.0.5", []byte(pinTestSpec("2.0.6")), "2.0.5", "2.0.6", ""},
		{"a spec with the next minor version", "2.1.3", []byte(pinTestSpec("2.0.6")), "2.1.3", "2.0.6", ""},
		{"a spec two minor versions above", "2.2.0", []byte(pinTestSpec("2.0.6")), "", "", "cluster prod: " +
			"spec.nomad.version: 2.2.0 is more than one minor version above 2.0.6, the Nomad version that the " +
			"cluster is pinned to (prod/cluster.completed.yaml); move to 2.1 first"},
		{"a spec of another major version", "2.0.7", []byte(pinTestSpec("1.11.0")), "", "", "cluster prod: " +
			"spec.nomad.version: 2.0.7 is of another major version than 1.11.0, the Nomad version that the " +
			"cluster is pinned to (prod/cluster.completed.yaml)"},
		{"a spec with a version and a broken pin", "2.2.0", []byte("kind: Cluster\n"), "2.2.0", "", ""},
		{"a broken pin", "", []byte("kind: Cluster\n"), "", "", broken + hint},
		{"a pin without a Cluster", "", []byte(pinTestGroup), "", "",
			"prod/cluster.completed.yaml: holds no Cluster" + hint},
		{"a pin that the channel does not allow", "", []byte(pinTestSpec("1.11.0")), "", "", "cluster prod is " +
			"pinned to Nomad 1.11.0 (prod/cluster.completed.yaml), which is older than 2.0.0, the oldest Nomad " +
			"that channel stable allows; run tent upgrade cluster, or set spec.nomad.version to a version that " +
			"the channel allows"},
		{"a pin of a major version that the channel does not know", "", []byte(pinTestSpec("3.0.0")), "", "", "cluster " +
			"prod is pinned to Nomad 3.0.0 (prod/cluster.completed.yaml), which is newer than this tent knows; " +
			"channel stable allows 2.x from 2.0.0; set spec.nomad.version to a version that the channel allows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objs, err := spec.Decode([]byte(pinTestSpec(tc.version)))
			if err != nil {
				t.Fatal(err)
			}
			c := objs.Cluster
			pinned, err := pinVersion(c, pinTestChannel(), l, tc.stored)
			if pinned != tc.wantPinned || (err == nil) != (tc.wantErr == "") || err != nil && err.Error() != tc.wantErr {
				t.Errorf("pinVersion = %q, %v\nwant         %q, %q", pinned, err, tc.wantPinned, tc.wantErr)
			}
			if err == nil && c.Spec.Nomad.Version != tc.wantUsed {
				t.Errorf("the cluster's Nomad version is %q, want %q", c.Spec.Nomad.Version, tc.wantUsed)
			}
		})
	}
}

// TestLoadClusterKeepsThePin loads a cluster with the Nomad version of its stored completed spec, "" when there is
// none or the stored one does not decode, whether the spec sets a version or not.
func TestLoadClusterKeepsThePin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		version   string
		completed string // the stored completed spec; empty for none
		want      string
		wantUsed  string
	}{
		{"no completed spec", "", "", "", "2.0.7"},
		{"only pinned", "", pinTestSpec("2.0.6"), "2.0.6", "2.0.6"},
		{"a spec older than the pin", "2.0.5", pinTestSpec("2.0.6"), "2.0.6", "2.0.5"},
		{"a spec above the pin", "2.1.0", pinTestSpec("2.0.6"), "2.0.6", "2.1.0"},
		{"a completed spec that does not decode", "2.1.0", "kind: Cluster\n", "", "2.1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, l := secretsTestService(t)
			put := func(p, doc string) {
				t.Helper()
				if _, err := s.Store.Put(t.Context(), p, []byte(doc), statestore.PutOptions{}); err != nil {
					t.Fatalf("put %s: %v", p, err)
				}
			}
			put(l.ClusterSpec(), pinTestSpec(tc.version))
			group, err := l.NodeGroup("servers")
			if err != nil {
				t.Fatal(err)
			}
			put(group, pinTestGroup)
			if tc.completed != "" {
				put(l.Completed(), tc.completed)
			}
			c, err := s.loadCluster(t.Context(), l)
			if err != nil {
				t.Fatalf("loadCluster: %v", err)
			}
			if c.pinned != tc.want {
				t.Errorf("the pin is %q, want %q", c.pinned, tc.want)
			}
			if got := c.objs.Cluster.Spec.Nomad.Version; got != tc.wantUsed {
				t.Errorf("the cluster's Nomad version is %q, want %q", got, tc.wantUsed)
			}
		})
	}
}
