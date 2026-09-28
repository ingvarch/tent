package buildinfo

import (
	"runtime"
	"testing"
)

func TestGetWithoutLdflags(t *testing.T) {
	want := Info{
		Version:   "dev",
		Commit:    "unknown",
		Date:      "unknown",
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if got := Get(); got != want {
		t.Errorf("Get() = %#v, want %#v", got, want)
	}
}

func TestRelease(t *testing.T) {
	for version, want := range map[string]string{
		"v0.3.0":                  "v0.3.0",
		"v0.3.0-rc.1":             "v0.3.0-rc.1",
		"v0.3.0-4-gabc1234":       "v0.3.0", // git describe: the tag stands for it
		"v0.3.0-rc.1-4-gabc1234":  "v0.3.0-rc.1",
		"v0.3.0-4-gabc1234-dirty": "v0.3.0",
		"v0.3.0-dirty":            "v0.3.0",
		"v0.3.0-SNAPSHOT-48dde55": "", // GoReleaser's snapshot: a development build
		"dev":                     "",
		"abc1234":                 "",
		"abc1234-dirty":           "",
		"0.3.0":                   "",
		"v0.3":                    "",
		"v0.3.0+b":                "",
		"":                        "",
	} {
		if got := Release(version); got != want {
			t.Errorf("Release(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestIsRelease(t *testing.T) {
	for version, want := range map[string]bool{
		"v0.3.0":                  true,
		"v0.3.0-rc.1":             true,
		"v0.3.0-4-gabc1234":       false, // later commits than the tag
		"v0.3.0-dirty":            false, // changes the tag does not hold
		"v0.3.0-SNAPSHOT-48dde55": false,
		"dev":                     false,
		"abc1234":                 false,
		"":                        false,
	} {
		if got := IsRelease(version); got != want {
			t.Errorf("IsRelease(%q) = %t, want %t", version, got, want)
		}
	}
}

func TestIsVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.3.0":            true,
		"v0.3.0-rc.1":       true,
		"v0.3.0-4-gabc1234": true,
		"0.3.0":             false,
		"v0.3":              false,
		"v0.3.0+b":          false,
		"x":                 false,
		"":                  false,
	} {
		if got := IsVersion(v); got != want {
			t.Errorf("IsVersion(%q) = %t, want %t", v, got, want)
		}
	}
}

func TestInfoString(t *testing.T) {
	i := Info{
		Version:   "1.2.3",
		Commit:    "abc1234",
		Date:      "2026-09-25T12:00:00Z",
		GoVersion: "go1.27.1",
		Platform:  "linux/amd64",
	}
	want := "1.2.3 (commit abc1234, built 2026-09-25T12:00:00Z, go1.27.1 linux/amd64)"
	if got := i.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
