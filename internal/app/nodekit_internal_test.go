package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
)

// recordingNodes is a cloud that records the creates and the scrubs it is asked for. A test that uses it calls no
// other method.
type recordingNodes struct {
	cloud.Nodes
	creates   []cloud.CreateRequest
	createErr error
	joined    []cloud.Instance
	joinErr   error
}

func (n *recordingNodes) Create(_ context.Context, req cloud.CreateRequest) (cloud.Instance, error) {
	n.creates = append(n.creates, req)
	if n.createErr != nil {
		return cloud.Instance{}, n.createErr
	}
	return cloud.Instance{ID: "id-1", Name: req.Name}, nil
}

func (n *recordingNodes) MarkJoined(_ context.Context, in cloud.Instance) error {
	n.joined = append(n.joined, in)
	return n.joinErr
}

// introStub is a Nomad API that issues intro tokens and records what it was asked for.
type introStub struct {
	nomadops.API
	requests []nomadops.IntroRequest
	err      error
}

func (s *introStub) IntroToken(_ context.Context, req nomadops.IntroRequest) (pki.Secret, error) {
	s.requests = append(s.requests, req)
	if s.err != nil {
		return nil, s.err
	}
	return pki.Secret("token"), nil
}

// joinStub is a joinPoint with fixed answers; it records the seed calls.
type joinStub struct {
	addrs    []netip.Addr
	seedErr  error
	api      nomadops.API
	apiErr   error
	seedFor  []string
	asClient []bool
}

func (j *joinStub) seed(name string, client bool) ([]netip.Addr, error) {
	j.seedFor, j.asClient = append(j.seedFor, name), append(j.asClient, client)
	return j.addrs, j.seedErr
}

func (j *joinStub) nomadAPI() (nomadops.API, error) { return j.api, j.apiErr }

// testKit returns a nodeKit for the cluster prod that creates its machines in nodes, with the templates of the test
// groups.
func testKit(t *testing.T, nodes cloud.Nodes) nodeKit {
	t.Helper()
	ca := testCA(t)
	tmpls := templates(t, pki.NewGossipKey(), ca.Bundle(), nodeClusterYAML, nodeServersYAML, nodeWorkersYAML)
	return nodeKit{
		cluster: "prod", region: "global", nodes: nodes, secrets: clusterSecrets{ca: ca},
		builder: &nodeBuilder{templates: tmpls, pools: map[string]string{"workers": "batch"}, servers: 1},
	}
}

// clientCreate is the create of a client node of the group workers.
func clientCreate() NodeChange {
	return NodeChange{
		Action: NodeCreate, Name: "prod-workers-0", Group: "workers", Role: v1alpha1.RoleClient, Zone: "ams",
		MachineType: "vc2-4c-8gb", Image: "ubuntu-24.04", SpecHash: "hash",
	}
}

// testService returns a service that tells the steps it reports to steps, as lines, and whose clock stands at testNow.
func testService(steps *[]string) *Service {
	return &Service{
		Now: func() time.Time { return testNow },
		OnProgress: func(p Progress) {
			line := p.Node.Action.String() + " " + p.Node.Name + " " + p.Step.String()
			if p.Err != nil {
				line += ": " + p.Err.Error()
			}
			*steps = append(*steps, line)
		},
	}
}

// TestChangeNodeCreatesWithTheOperationIdOfTheChange checks that a create with an operation id creates with it, that
// a create without one gets a new valid id each time, and that a wait repeats its create with its id.
func TestChangeNodeCreatesWithTheOperationIdOfTheChange(t *testing.T) {
	t.Parallel()
	const op = "4f6a2d5e-8c3b-4d1e-9a7f-0b2c3d4e5f60"
	prepare := func(context.Context) (cloud.UserData, error) { return cloud.UserData("data"), nil }
	create := func(c NodeChange) (string, error) {
		nodes := &recordingNodes{}
		if _, err := changeNode(t.Context(), nodes, "prod", c, prepare); err != nil {
			return "", err
		}
		if len(nodes.creates) != 1 {
			return "", fmt.Errorf("asked the cloud for %d machines, want 1", len(nodes.creates))
		}
		return nodes.creates[0].Op, nil
	}

	withOp, err := create(NodeChange{Action: NodeCreate, Name: "prod-workers-0", Op: op})
	if err != nil || withOp != op {
		t.Errorf("a create with the operation id %s created with %q (error %v)", op, withOp, err)
	}
	wait, err := create(NodeChange{Action: NodeWait, Name: "prod-workers-0", Op: op})
	if err != nil || wait != op {
		t.Errorf("a wait with the operation id %s created with %q (error %v)", op, wait, err)
	}
	first, err1 := create(NodeChange{Action: NodeCreate, Name: "prod-workers-0"})
	second, err2 := create(NodeChange{Action: NodeCreate, Name: "prod-workers-0"})
	if err1 != nil || err2 != nil {
		t.Fatalf("a create without an operation id failed: %v, %v", err1, err2)
	}
	if !cloud.ValidOpID(first) || !cloud.ValidOpID(second) || first == second {
		t.Errorf("creates without an operation id used %q and %q, want two different valid ids", first, second)
	}
}

// TestBootClientCreatesTheNodeWithAnIntroToken checks the intro token request, which names the node, its pool and the
// longest lifetime, the seed request of a client, and the create that follows with the change's operation id.
func TestBootClientCreatesTheNodeWithAnIntroToken(t *testing.T) {
	t.Parallel()
	var steps []string
	nodes := &recordingNodes{}
	api := &introStub{}
	join := &joinStub{addrs: []netip.Addr{netip.MustParseAddr("10.10.0.5")}, api: api}
	c := clientCreate()
	c.Op = "4f6a2d5e-8c3b-4d1e-9a7f-0b2c3d4e5f60"

	in, err := testService(&steps).bootClient(t.Context(), testKit(t, nodes), join, c)

	if err != nil {
		t.Fatalf("bootClient: %v", err)
	}
	if in.Name != c.Name || in.ID != "id-1" {
		t.Errorf("bootClient returned the machine %+v, want the one that the cloud created", in)
	}
	want := []nomadops.IntroRequest{{NodeName: c.Name, NodePool: "batch", TTL: nomadops.MaxIntroTTL}}
	if diff := cmp.Diff(want, api.requests); diff != "" {
		t.Errorf("the intro token requests (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{c.Name}, join.seedFor); diff != "" || !join.asClient[0] {
		t.Errorf("the seed was asked for (-want +got):\n%s\nas a client: %v", diff, join.asClient)
	}
	if len(nodes.creates) != 1 {
		t.Fatalf("bootClient asked the cloud for %d machines, want 1", len(nodes.creates))
	}
	got := nodes.creates[0]
	if got.Cluster != "prod" || got.Op != c.Op || got.Name != c.Name || got.Group != "workers" ||
		got.SpecHash != "hash" || len(got.UserData) == 0 {
		t.Errorf("the create request is %+v, want the kit's cluster, the change's operation id, name, group and spec "+
			"hash, and user data", got)
	}
	if diff := cmp.Diff([]string{"create prod-workers-0 started", "create prod-workers-0 done"}, steps); diff != "" {
		t.Errorf("the steps (-want +got):\n%s", diff)
	}
}

// TestBootClientFailsBeforeTheCloudIsAsked checks that a failure of the seed, of the API, of the intro token or of the
// user data is reported as a failed create with its own text, calls no cloud, and matches errNotSent and its cause.
func TestBootClientFailsBeforeTheCloudIsAsked(t *testing.T) {
	t.Parallel()
	seedErr := errors.New("no server has a private address")
	apiErr := fmt.Errorf("no server answered: %w", nomadops.ErrNotReady)
	tokenErr := errors.New("token refused")
	for _, tc := range []struct {
		name  string
		join  *joinStub
		group string
		cause error  // nil for a failure of the user data, whose text the test does not pin
		text  string // the text of the error
	}{
		{"seed", &joinStub{seedErr: seedErr, api: &introStub{}}, "workers", seedErr, seedErr.Error()},
		{"api", &joinStub{apiErr: apiErr}, "workers", apiErr, apiErr.Error()},
		{"intro token", &joinStub{api: &introStub{err: tokenErr}}, "workers", tokenErr,
			"intro token for node prod-workers-0: token refused"},
		{"user data", &joinStub{api: &introStub{}}, "gone", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var steps []string
			nodes := &recordingNodes{}
			c := clientCreate()
			c.Group = tc.group

			_, err := testService(&steps).bootClient(t.Context(), testKit(t, nodes), tc.join, c)

			if err == nil || !errors.Is(err, errNotSent) {
				t.Fatalf("bootClient error = %v, want one that matches errNotSent", err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Errorf("bootClient error = %v, want one that matches its cause %v", err, tc.cause)
			}
			if got, want := errors.Is(err, nomadops.ErrNotReady), tc.name == "api"; got != want {
				t.Errorf("bootClient error = %v matches ErrNotReady: %v, want %v", err, got, want)
			}
			if tc.text != "" && err.Error() != tc.text {
				t.Errorf("bootClient error text = %q, want %q", err, tc.text)
			}
			if len(nodes.creates) != 0 {
				t.Errorf("bootClient asked the cloud for %d machines, want none", len(nodes.creates))
			}
			wantSteps := []string{"create prod-workers-0 started", "create prod-workers-0 failed: " + err.Error()}
			if diff := cmp.Diff(wantSteps, steps); diff != "" {
				t.Errorf("the steps (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBootClientFailedCreateDoesNotMatchErrNotSent checks that a failure of the cloud's create does not match
// errNotSent, since the request may have reached the cloud, and keeps its text.
func TestBootClientFailedCreateDoesNotMatchErrNotSent(t *testing.T) {
	t.Parallel()
	var steps []string
	boom := errors.New("the cloud refused")
	nodes := &recordingNodes{createErr: boom}
	join := &joinStub{api: &introStub{}}

	_, err := testService(&steps).bootClient(t.Context(), testKit(t, nodes), join, clientCreate())

	if !errors.Is(err, boom) || errors.Is(err, errNotSent) || err.Error() != boom.Error() {
		t.Errorf("bootClient error = %v, want the cloud's error alone, not matching errNotSent", err)
	}
	if len(nodes.creates) != 1 {
		t.Errorf("bootClient asked the cloud for %d machines, want 1", len(nodes.creates))
	}
}

// TestMarkJoinedScrubsTheMachineAndReportsIt checks that the machine is marked, the scrub is reported as started and
// done, and a failure is reported and returned as it is.
func TestMarkJoinedScrubsTheMachineAndReportsIt(t *testing.T) {
	t.Parallel()
	in := cloud.Instance{ID: "id-1", Name: "prod-workers-0"}
	boom := errors.New("the cloud refused the label")
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"scrubbed", nil, []string{"scrub prod-workers-0 started", "scrub prod-workers-0 done"}},
		{"refused", boom, []string{"scrub prod-workers-0 started", "scrub prod-workers-0 failed: " + boom.Error()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var steps []string
			nodes := &recordingNodes{joinErr: tc.err}

			err := testService(&steps).markJoined(t.Context(), testKit(t, nodes), in)

			if !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
				t.Errorf("markJoined error = %v, want %v", err, tc.err)
			}
			if diff := cmp.Diff([]cloud.Instance{in}, nodes.joined, cmpopts.EquateComparable(netip.Addr{})); diff != "" {
				t.Errorf("the machines marked as joined (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.want, steps); diff != "" {
				t.Errorf("the steps (-want +got):\n%s", diff)
			}
		})
	}
}
