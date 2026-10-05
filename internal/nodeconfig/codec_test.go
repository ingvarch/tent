package nodeconfig_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ingvarch/tent/internal/nodeconfig"
)

// equateNetip compares addresses and prefixes by value; cmp cannot look into their unexported fields.
var equateNetip = cmpopts.EquateComparable(netip.Addr{}, netip.Prefix{})

func TestEncodeGolden(t *testing.T) {
	got, err := nodeconfig.Encode(sample())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	checkGolden(t, "node.json.golden", string(got))
}

// TestEncodeDeterministic checks that one NodeConfig always encodes to the same bytes, whatever the order Go gives a
// map's keys in.
func TestEncodeDeterministic(t *testing.T) {
	c := sample()
	for i := range 20 {
		c.System.Sysctls[strings.Repeat("k", i+1)+".x"] = "1"
	}
	first, err := nodeconfig.Encode(c)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for range 20 {
		again, err := nodeconfig.Encode(c)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if !bytes.Equal(again, first) {
			t.Fatal("Encode gave other bytes for the same NodeConfig")
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for name, edit := range map[string]func(c *nodeconfig.NodeConfig){
		"sample":    func(*nodeconfig.NodeConfig) {},
		"spec hash": func(c *nodeconfig.NodeConfig) { c.SpecHash = nodeconfig.SpecHash(c) },
		"content that JSON escapes": func(c *nodeconfig.NodeConfig) {
			c.Files[0].Content = []byte("<a & b> \"q\" \\ \t\u2028\u00e9\u65e5\x00\x7f\n")
		},
		"nothing optional": func(c *nodeconfig.NodeConfig) {
			c.Assets, c.Files, c.Join.Servers, c.System, c.Firewall.Rules = nil, nil, nil, nodeconfig.System{}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			want := sample()
			edit(want)
			data, err := nodeconfig.Encode(want)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := nodeconfig.Decode(data)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			// cmp shows a file's bytes in a failed test; these are stand-ins, not secrets.
			if diff := cmp.Diff(want, got, equateNetip, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Decode(Encode(c)) differs from c (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEncodeEmptyAsLeftOut checks that an empty list or map encodes as one that is left out, so both give the same
// bytes.
func TestEncodeEmptyAsLeftOut(t *testing.T) {
	left, empty := sample(), sample()
	left.Join.Servers, left.System.KernelModules, left.System.Sysctls = nil, nil, nil
	empty.Join.Servers, empty.System.KernelModules, empty.System.Sysctls = []netip.Addr{}, []string{}, map[string]string{}
	a, err := nodeconfig.Encode(left)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	b, err := nodeconfig.Encode(empty)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("empty lists encode as\n%s\nand left-out lists as\n%s", b, a)
	}
}

func TestEncodeValidates(t *testing.T) {
	c := sample()
	c.Kind = "Node"
	if _, err := nodeconfig.Encode(c); errText(err) != `node config: kind "Node" is not NodeConfig` {
		t.Errorf("Encode() error = %v, want the error of Validate", err)
	}
	if _, err := nodeconfig.Encode(nil); errText(err) != "no node config" {
		t.Errorf("Encode(nil) error = %v, want no node config", err)
	}
}

func TestDecodeStrict(t *testing.T) {
	valid, err := nodeconfig.Encode(sample())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// with replaces the first old in the encoded sample with repl, and fails the test when old is not there.
	with := func(old, repl string) string {
		t.Helper()
		if !strings.Contains(string(valid), old) {
			t.Fatalf("the encoded sample has no %q", old)
		}
		return strings.Replace(string(valid), old, repl, 1)
	}
	for _, tc := range []struct {
		name, data, want string
	}{
		{"unknown field", with(`"kind": "NodeConfig",`, `"kind": "NodeConfig", "instance": "x",`),
			`decode node config: json: unknown field "instance"`},
		{"unknown field of a file", with(`"mode": 420,`, `"mode": 420, "group": "root",`),
			`decode node config: json: unknown field "group"`},
		{"unknown field of an asset", with(`"version": "2.0.7",`, `"version": "2.0.7", "arch": "amd64",`),
			`decode node config: json: unknown field "arch"`},
		{"trailing data", string(valid) + "{}", "decode node config: data after the object"},
		{"a second object", string(valid) + string(valid), "decode node config: data after the object"},
		{"not JSON", "#cloud-config\n", "decode node config: invalid character '#' looking for beginning of value"},
		{"empty", "", "decode node config: EOF"},
		{"an invalid address", with(`"10.64.0.9"`, `"10.64.0.300"`),
			`decode node config: ParseAddr("10.64.0.300"): IPv4 field has value >255`},
		{"null", "null", `node config: apiVersion "" is not tent/v1alpha1`},
		{"invalid", with(`"role": "combined"`, `"role": "worker"`),
			`node config: role "worker" is not server, client or combined`},
		{"no provider", with(`"provider": "vultr",`, ""), `node config: provider "" is not one of vultr, hetzner`},
		{"no region", with("  \"region\": \"global\",\n", ""), `node config: no region`},
		{"malformed region", with(`"region": "global"`, `"region": "Global"`),
			`node config: region "Global" is not lower-case letters, digits and dashes, ` +
				"starting with a letter or digit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := nodeconfig.Decode([]byte(tc.data))
			if got := errText(err); got != tc.want {
				t.Errorf("Decode() error = %q, want %q", got, tc.want)
			}
			if c != nil {
				t.Errorf("Decode() = %v, want nil with the error", c)
			}
		})
	}
	// The text of a type error differs between Go versions.
	c, err := nodeconfig.Decode([]byte(with(`"mode": 420,`, `"mode": "0644",`)))
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) || !strings.HasPrefix(err.Error(), "decode node config: ") || c != nil {
		t.Errorf("Decode() of a string mode = %v, %v; want nil and a wrapped *json.UnmarshalTypeError", c, err)
	}
}
