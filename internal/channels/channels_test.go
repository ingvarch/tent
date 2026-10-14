package channels

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

// sha256Hex matches a sha256 as release files write it: 64 lower-case hex digits.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// checkVersion fails the test unless v is a release version without a v, such as 2.0.7.
func checkVersion(t *testing.T, field, v string) {
	t.Helper()
	if semver.Canonical("v"+v) != "v"+v || semver.Prerelease("v"+v) != "" {
		t.Errorf("%s = %q, want a release version such as 2.0.7", field, v)
	}
}

func TestEveryChannelLoads(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no channel is embedded")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			c, err := Load(name)
			if err != nil {
				t.Fatal(err)
			}
			if c.Name != name {
				t.Errorf("name = %q, want %q, the file's name", c.Name, name)
			}
			checkVersion(t, "nomad.minimum", c.Nomad.Minimum)
			checkVersion(t, "nomad.recommended", c.Nomad.Recommended)
			for _, v := range c.Nomad.Tested {
				checkVersion(t, "nomad.tested", v)
				if err := c.Allows(v); err != nil {
					t.Errorf("nomad.tested lists a version that the channel does not allow: %v", err)
				}
			}
			if semver.Compare("v"+c.Nomad.Minimum, "v"+c.Nomad.Recommended) > 0 {
				t.Errorf("nomad.minimum %s is newer than the recommended %s", c.Nomad.Minimum, c.Nomad.Recommended)
			}
			if !c.Tested(c.Nomad.Recommended) {
				t.Errorf("nomad.tested %v lacks the recommended %s", c.Nomad.Tested, c.Nomad.Recommended)
			}
			checkVersion(t, "cni.version", c.CNI.Version)
			for _, arch := range []string{"amd64", "arm64"} {
				if sum := c.CNI.SHA256[arch]; !sha256Hex.MatchString(sum) {
					t.Errorf("cni.sha256.%s = %q, want 64 lower-case hex digits", arch, sum)
				}
			}
		})
	}
}

func TestStableIsEmbedded(t *testing.T) {
	if !slices.Contains(Names(), "stable") {
		t.Errorf("Names() = %v, want stable among them", Names())
	}
}

func TestStableTestsTheVersionsOfTheUpgradeRun(t *testing.T) {
	c, err := Load("stable")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"2.0.6", "2.0.7"} {
		if !c.Tested(v) {
			t.Errorf("stable.nomad.tested = %v, want %s among them", c.Nomad.Tested, v)
		}
	}
}

func TestNamesAreSorted(t *testing.T) {
	if names := Names(); !slices.IsSorted(names) {
		t.Errorf("Names() = %v, want them sorted", names)
	}
}

func TestLoadUnknownChannel(t *testing.T) {
	for _, name := range []string{"beta", "", "stable.yaml", "./stable", "STABLE"} {
		_, err := Load(name)
		want := `unknown channel "` + name + `"; known: stable`
		if err == nil || err.Error() != want {
			t.Errorf("Load(%q) = %v, want %s", name, err, want)
		}
	}
}

func TestDecodeIsStrict(t *testing.T) {
	const good = `name: stable
nomad:
  minimum: 2.0.0
  recommended: 2.0.7
  tested: [2.0.7]
cni:
  version: 1.9.1
  sha256:
    amd64: b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303
    arm64: 56171987d3947707c3563db2f4001bccaf50fd63468611b9f3cbecb1375ee7ec
`
	if _, err := decode("stable", []byte(good)); err != nil {
		t.Fatalf("decode a good channel: %v", err)
	}
	for _, tc := range []struct{ name, data, want string }{
		{"unknown field", good + "images: {}\n", `unknown field "images"`},
		{"unknown nested field", strings.Replace(good, "  recommended:", "  latest: 2.0.7\n  recommended:", 1),
			`unknown field "nomad.latest"`},
		{"key in the wrong case", strings.Replace(good, "name:", "Name:", 1), `unknown field "Name"`},
		{"duplicate key", good + "name: beta\n", `"name" already set`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decode("stable", []byte(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "stable.yaml") {
				t.Errorf("decode = %v, want an error about stable.yaml that says %s", err, tc.want)
			}
		})
	}
}

func TestTested(t *testing.T) {
	c := &Channel{Nomad: Nomad{Minimum: "2.0.0", Recommended: "2.0.7", Tested: []string{"2.0.6", "2.0.7"}}}
	for v, want := range map[string]bool{"2.0.6": true, "2.0.7": true, "2.0.8": false, "v2.0.7": false, "": false} {
		if got := c.Tested(v); got != want {
			t.Errorf("Tested(%q) = %t, want %t", v, got, want)
		}
	}
}

func TestAllows(t *testing.T) {
	c := &Channel{Name: "stable", Nomad: Nomad{Minimum: "2.0.0", Recommended: "2.0.7", Tested: []string{"2.0.7"}}}
	const (
		older = " is older than 2.0.0, the oldest Nomad that channel stable allows"
		newer = " is newer than this tent knows; channel stable allows 2.x from 2.0.0"
		form  = " is not a Nomad version such as 2.0.7"
	)
	for _, tc := range []struct{ version, want string }{
		{"2.0.0", ""},
		{"2.0.7", ""},
		{"2.0.8", ""},
		{"2.13.0", ""},
		{"2.99.99", ""},
		{"1.11.0", "1.11.0" + older},
		{"0.0.1", "0.0.1" + older},
		{"3.0.0", "3.0.0" + newer},
		{"10.1.2", "10.1.2" + newer},
		{"2.0", `"2.0"` + form},
		{"2", `"2"` + form},
		{"v2.0.7", `"v2.0.7"` + form},
		{"2.0.7-beta.1", `"2.0.7-beta.1"` + form},
		{"2.0.7+ent", `"2.0.7+ent"` + form},
		{"02.0.7", `"02.0.7"` + form},
		{"2.0.07", `"2.0.07"` + form},
		{" 2.0.7", `" 2.0.7"` + form},
		{"", `""` + form},
	} {
		err := c.Allows(tc.version)
		if got, want := fmt.Sprint(err), cmp.Or(tc.want, "<nil>"); got != want {
			t.Errorf("Allows(%q) = %s, want %s", tc.version, got, want)
		}
		if err == nil {
			continue
		}
		// The problem is the error without the version, for messages that name the version already.
		v, ok := errors.AsType[*VersionError](err)
		if !ok || v.Version != tc.version {
			t.Errorf("Allows(%q) = %#v, want a *VersionError of the version", tc.version, err)
			continue
		}
		if head := strings.TrimSuffix(tc.want, " "+v.Problem); head != tc.version && head != strconv.Quote(tc.version) {
			t.Errorf("Allows(%q): the problem %q is not the error without the version", tc.version, v.Problem)
		}
	}
}

func TestCheckMove(t *testing.T) {
	const (
		tooFar = " is more than one minor version above "
		major  = " is of another major version than "
	)
	for _, tc := range []struct{ name, from, to, want string }{
		{"the same version", "2.0.7", "2.0.7", ""},
		{"a newer patch", "2.0.6", "2.0.7", ""},
		{"a patch that skips versions", "2.0.1", "2.0.9", ""},
		{"a double-digit patch", "2.0.9", "2.0.10", ""},
		{"the next minor version", "2.0.7", "2.1.0", ""},
		{"the next minor version, any patch", "2.0.6", "2.1.3", ""},
		{"the next minor version past nine", "2.9.0", "2.10.0", ""},
		{"two minor versions up", "2.0.7", "2.2.0", "2.2.0" + tooFar + "2.0.7"},
		{"two minor versions up, from a later patch", "2.0.7", "2.2.9", "2.2.9" + tooFar + "2.0.7"},
		{"the minor version after the next past nine", "2.9.0", "2.11.0", "2.11.0" + tooFar + "2.9.0"},
		{"many minor versions up", "2.0.7", "2.13.0", "2.13.0" + tooFar + "2.0.7"},
		{"an older patch", "2.0.7", "2.0.6", ""},
		{"an older minor version", "2.3.0", "2.1.4", ""},
		{"an older major version", "2.0.7", "1.11.3", ""},
		{"a newer major version", "2.0.7", "3.0.0", "3.0.0" + major + "2.0.7"},
		{"a newer major version, minor 0 after minor 0", "2.9.9", "3.0.0", "3.0.0" + major + "2.9.9"},
		{"a far newer major version", "2.0.7", "10.0.0", "10.0.0" + major + "2.0.7"},
		{"from is no release: empty", "", "2.2.0", ""},
		{"from is no release: two numbers", "2.0", "2.2.0", ""},
		{"from is no release: with a v", "v2.0.7", "2.2.0", ""},
		{"from is no release: a prerelease", "2.0.7-rc.1", "2.2.0", ""},
		{"from is no release: text", "banana", "2.2.0", ""},
		{"to is no release: empty", "2.0.7", "", ""},
		{"to is no release: two numbers", "2.0.7", "2.0", ""},
		{"to is no release: with a v", "2.0.7", "v2.2.0", ""},
		{"to is no release: a prerelease", "2.0.7", "2.2.0-rc.1", ""},
		{"to is no release: text", "2.0.7", "banana", ""},
		{"to is no release: build metadata", "2.0.7", "2.5.0+ent", ""},
		{"to is no release: a prerelease of another major version", "2.0.7", "3.0.0-rc.1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckMove(tc.from, tc.to)
			if got, want := fmt.Sprint(err), cmp.Or(tc.want, "<nil>"); got != want {
				t.Errorf("CheckMove(%q, %q) = %s, want %s", tc.from, tc.to, got, want)
			}
			if err == nil {
				return
			}
			if v, ok := errors.AsType[*VersionError](err); !ok || v.Version != tc.to {
				t.Errorf("CheckMove(%q, %q) = %#v, want a *VersionError of %q", tc.from, tc.to, err, tc.to)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"2.0.6", "2.0.7", -1},
		{"2.0.7", "2.0.7", 0},
		{"2.0.8", "2.0.7", 1},
		{"2.0.9", "2.0.10", -1},
		{"2.0.10", "2.0.9", 1},
		{"2.9.0", "2.10.0", -1},
		{"1.11.3", "2.0.0", -1},
		{"3.0.0", "2.99.99", 1},
	} {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestRelease(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"2.0.7", "2.0.7"},
		{"2.0.7-rc1", "2.0.7"},
		{"2.0.7+ent", "2.0.7"},
		{"2.0.7-rc1+ent", "2.0.7"},
		{"2.0", "2.0.0"},
		{"", ""},
		{"banana", ""},
		{"v2.0.7", ""},
	} {
		if got := Release(tc.version); got != tc.want {
			t.Errorf("Release(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

func TestNextMinor(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"2.0.7", "2.1.0"},
		{"2.0.0", "2.1.0"},
		{"2.9.3", "2.10.0"},
		{"2.10.4", "2.11.0"},
		{"1.11.3", "1.12.0"},
		{"2.99999999999999999999.0", ""},
		{"2.9223372036854775807.0", ""},
		{"2.0", ""},
		{"v2.0.7", ""},
		{"2.0.7-rc.1", ""},
		{"banana", ""},
		{"", ""},
	} {
		if got := NextMinor(tc.version); got != tc.want {
			t.Errorf("NextMinor(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

func TestOlderThanMinimum(t *testing.T) {
	c := &Channel{Name: "stable", Nomad: Nomad{Minimum: "2.0.3", Recommended: "2.0.7", Tested: []string{"2.0.7"}}}
	for version, want := range map[string]bool{
		"2.0.2":      true,
		"1.11.3":     true,
		"2.0.3":      false,
		"2.0.4":      false,
		"2.1.0":      false,
		"3.0.0":      false,
		"2.0":        false,
		"v2.0.2":     false,
		"2.0.2-rc.1": false,
		"banana":     false,
		"":           false,
	} {
		if got := c.OlderThanMinimum(version); got != want {
			t.Errorf("OlderThanMinimum(%q) = %t, want %t", version, got, want)
		}
	}
}
