package rollout_test

import (
	"errors"
	"testing"

	"github.com/ingvarch/tent/internal/rollout"
)

const (
	notNewer = "tent never moves a node to an older Nomad: the cluster's version is "
	ahead    = "a new node would run Nomad "
)

// TestCheckNewNode runs the version rules of a new node over a report of the servers and the nodes.
func TestCheckNewNode(t *testing.T) {
	t.Parallel()
	server := func(name, version string) rollout.Server { return rollout.Server{Name: name, Version: version} }
	node := func(name, status, version string) rollout.Node {
		return rollout.Node{Name: name, Status: status, Version: version}
	}
	tests := []struct {
		name        string
		nomad       rollout.Nomad
		version     string
		clientOnly  bool
		wantErr     string
		wantUnknown bool
	}{
		{name: "nothing is reported", version: "2.0.7"},
		{name: "servers and nodes of the version", version: "2.0.7",
			nomad: rollout.Nomad{
				Servers: []rollout.Server{server("s0", "2.0.7")},
				Nodes:   []rollout.Node{node("n0", "ready", "2.0.7")},
			}},
		{name: "the version is no version number", version: "latest", wantErr: `the Nomad version of a new node is ` +
			`"latest", which is not a version number`},
		{name: "no version", wantErr: `the Nomad version of a new node is "", which is not a version number`},
		{name: "a server reports no version number", version: "2.0.7",
			nomad:   rollout.Nomad{Servers: []rollout.Server{server("s0", "two")}},
			wantErr: `the Nomad version of server s0 is "two", which is not a version number`},
		{name: "a node reports no version number", version: "2.0.7",
			nomad:   rollout.Nomad{Nodes: []rollout.Node{node("n0", "ready", "x.y")}},
			wantErr: `the Nomad version of node n0 is "x.y", which is not a version number`},
		{name: "a node reports no version", version: "2.0.7",
			nomad:   rollout.Nomad{Nodes: []rollout.Node{node("n0", "ready", "")}},
			wantErr: `the Nomad version of node n0 is "", which is not a version number`},
		{name: "a newer server", version: "2.0.7",
			nomad:   rollout.Nomad{Servers: []rollout.Server{server("s0", "2.1.0")}},
			wantErr: notNewer + "2.0.7, and server s0 runs 2.1.0"},
		{name: "a newer node", version: "2.0.7",
			nomad:   rollout.Nomad{Nodes: []rollout.Node{node("n0", "ready", "2.0.8")}},
			wantErr: notNewer + "2.0.7, and node n0 runs 2.0.8"},
		{name: "a newer node that is down is accepted", version: "2.0.7",
			nomad: rollout.Nomad{Nodes: []rollout.Node{node("n0", "down", "2.0.8")}}},
		{name: "a down node with no version number is accepted", version: "2.0.7",
			nomad: rollout.Nomad{Nodes: []rollout.Node{node("n0", "down", "")}}},
		{name: "a build suffix compares by its number", version: "2.0.7",
			nomad:   rollout.Nomad{Nodes: []rollout.Node{node("n0", "ready", "2.0.8+ent")}},
			wantErr: notNewer + "2.0.7, and node n0 runs 2.0.8+ent"},
		{name: "a build suffix of the same number is not newer", version: "2.0.7",
			nomad: rollout.Nomad{Nodes: []rollout.Node{node("n0", "ready", "2.0.7+ent")}}},
		{name: "an older server is accepted", version: "2.0.7",
			nomad: rollout.Nomad{Servers: []rollout.Server{server("s0", "2.0.6")}}},
		{name: "an older node is accepted", version: "2.0.7", clientOnly: true,
			nomad: rollout.Nomad{Nodes: []rollout.Node{node("n0", "ready", "2.0.6")}}},
		{name: "an older server beside a client", version: "2.0.8", clientOnly: true,
			nomad:   rollout.Nomad{Servers: []rollout.Server{server("s0", "2.0.7")}},
			wantErr: ahead + "2.0.8, newer than the 2.0.7 of server s0; roll the servers first"},
		{name: "the first of two older servers is named", version: "2.0.8", clientOnly: true,
			nomad: rollout.Nomad{Servers: []rollout.Server{
				server("s0", "2.0.8"), server("s1", "2.0.6"), server("s2", "2.0.7"),
			}},
			wantErr: ahead + "2.0.8, newer than the 2.0.6 of server s1; roll the servers first"},
		{name: "a newer node comes before an older server", version: "2.0.7", clientOnly: true,
			nomad: rollout.Nomad{
				Servers: []rollout.Server{server("s0", "2.0.6")},
				Nodes:   []rollout.Node{node("n0", "ready", "2.0.8")},
			},
			wantErr: notNewer + "2.0.7, and node n0 runs 2.0.8"},
		{name: "a newer server comes before an older one", version: "2.0.7", clientOnly: true,
			nomad:   rollout.Nomad{Servers: []rollout.Server{server("s0", "2.0.6"), server("s1", "2.0.9")}},
			wantErr: notNewer + "2.0.7, and server s1 runs 2.0.9"},
		{name: "a server without a version", version: "2.0.7", clientOnly: true,
			nomad:       rollout.Nomad{Servers: []rollout.Server{server("s0", "")}},
			wantUnknown: true},
		{name: "a server without a version is not compared without clientOnly", version: "2.0.7",
			nomad:       rollout.Nomad{Servers: []rollout.Server{server("s0", "")}},
			wantUnknown: true},
		{name: "a server without a version beside an older one", version: "2.0.8", clientOnly: true,
			nomad:   rollout.Nomad{Servers: []rollout.Server{server("s0", ""), server("s1", "2.0.7")}},
			wantErr: ahead + "2.0.8, newer than the 2.0.7 of server s1; roll the servers first"},
		{name: "an older server beside one without a version", version: "2.0.8", clientOnly: true,
			nomad:   rollout.Nomad{Servers: []rollout.Server{server("s1", "2.0.7"), server("s0", "")}},
			wantErr: ahead + "2.0.8, newer than the 2.0.7 of server s1; roll the servers first"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			unknown, err := rollout.CheckNewNode(tt.nomad, tt.version, tt.clientOnly)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckNewNode returned %v, want no error", err)
				}
			} else {
				if !errors.Is(err, rollout.ErrRefused) {
					t.Fatalf("CheckNewNode returned %v, want an error that matches ErrRefused", err)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("CheckNewNode refused with\n%q\nwant\n%q", err.Error(), tt.wantErr)
				}
				return
			}
			if unknown != tt.wantUnknown {
				t.Fatalf("CheckNewNode returned unknown %v, want %v", unknown, tt.wantUnknown)
			}
		})
	}
}
