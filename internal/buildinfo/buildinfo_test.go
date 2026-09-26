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
