package env_test

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/ingvarch/tent/internal/nodeup/env"
)

// TestMetadataMark checks that the mark shares no bit with the marks that others set on a node's packets: CNI's
// portmap, kube-proxy, Tailscale and Calico. Their rules test their own bits, so a shared bit would let them act on
// tent-node's packets.
func TestMetadataMark(t *testing.T) {
	const others = 0x2000 | 0x4000 | 0x8000 | 0xff0000 | 0xffff0000
	if env.MetadataMark == 0 || env.MetadataMark&others != 0 {
		t.Errorf("MetadataMark = %#x, want a mark without the bits %#x", env.MetadataMark, others)
	}
}

// TestInstanceJSON checks the JSON of an Instance, as the status file shows it: a part the cloud did not report is
// left out.
func TestInstanceJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   env.Instance
		want string
	}{
		{"every part", env.Instance{
			ID: "0b8f5c3e-1d2a-4c6e-9f7b-2a4d6e8c0f13", Zone: "ams", PrivateIP: netip.MustParseAddr("10.64.0.3"),
		}, `{"id":"0b8f5c3e-1d2a-4c6e-9f7b-2a4d6e8c0f13","zone":"ams","privateIP":"10.64.0.3"}`},
		{"nothing", env.Instance{}, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal() = %s, want %s", got, tc.want)
			}
		})
	}
}
