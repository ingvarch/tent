// Package buildinfo reports the version, commit and build date that the linker injects, and which release a version
// counts as.
package buildinfo

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/mod/semver"
)

// Set with -ldflags "-X github.com/ingvarch/tent/internal/buildinfo.version=..." (see the Makefile and
// .goreleaser.yaml).
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the build information of the running binary.
func Get() Info {
	return Info{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String formats the info the way "tent version" prints it.
func (i Info) String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s %s)", i.Version, i.Commit, i.Date, i.GoVersion, i.Platform)
}

// describeSuffix matches what git describe appends to the tag of an untagged commit: -4-gabc1234.
var describeSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+$`)

// Release returns the release a tent version counts as, or "" for a development build. A release (v0.3.0) and a
// pre-release (v0.3.0-rc.1) count as themselves, and git describe output (v0.3.0-4-gabc1234, maybe -dirty) as its
// tag: semver would read it as a pre-release older than the tag. A snapshot build, such as v0.3.0-SNAPSHOT-abc1234,
// carries the last tag and may hold later commits, so it is a development build, and so is anything else.
func Release(version string) string {
	v := describeSuffix.ReplaceAllString(strings.TrimSuffix(version, "-dirty"), "")
	if strings.Contains(v, "-SNAPSHOT") || !IsVersion(v) {
		return ""
	}
	return v
}

// IsRelease reports whether version is exactly a release or a pre-release, such as v0.3.0 or v0.3.0-rc.1: not git
// describe output, a dirty or a snapshot build.
func IsRelease(version string) bool { return version != "" && Release(version) == version }

// IsVersion reports whether v is vX.Y.Z or vX.Y.Z-pre.
func IsVersion(v string) bool { return semver.IsValid(v) && semver.Canonical(v) == v }
