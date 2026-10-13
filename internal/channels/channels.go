// Package channels holds the release channels embedded in tent. A channel sets the Nomad versions a cluster may run,
// recommends one and lists those tested, and pins the CNI plugins.
package channels

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
	sigsjson "sigs.k8s.io/json"
	sigsyaml "sigs.k8s.io/yaml"
)

//go:embed *.yaml
var files embed.FS

// Channel is a release channel.
type Channel struct {
	Name  string `json:"name"`
	Nomad Nomad  `json:"nomad"`
	CNI   CNI    `json:"cni"`
}

// Nomad holds the Nomad versions of a channel, such as 2.0.7. A cluster may run any version from the minimum up to,
// not including, the next major version.
type Nomad struct {
	Minimum     string   `json:"minimum"`     // the oldest version a cluster may run
	Recommended string   `json:"recommended"` // what a new cluster runs
	Tested      []string `json:"tested"`      // the versions tested with this tent
}

// CNI pins the CNI plugins of a channel: a version, such as 1.9.1, and the sha256 of its Linux archive by
// architecture (amd64, arm64).
type CNI struct {
	Version string            `json:"version"`
	SHA256  map[string]string `json:"sha256"`
}

// Names returns the names of the embedded channels, sorted.
func Names() []string {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil // the embedded directory always reads
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	slices.Sort(names)
	return names
}

// Load returns the embedded channel with the given name.
func Load(name string) (*Channel, error) {
	names := Names()
	if !slices.Contains(names, name) {
		return nil, fmt.Errorf("unknown channel %q; known: %s", name, strings.Join(names, ", "))
	}
	data, err := files.ReadFile(name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("read channel file %s.yaml: %w", name, err)
	}
	return decode(name, data)
}

// Allows returns nil when a cluster of the channel may run the Nomad version: a release X.Y.Z, such as 2.0.7, from the
// channel's minimum up to, not including, the next major version. Otherwise it returns a *VersionError that says why.
// It does not check that the version was released.
func (c *Channel) Allows(version string) error {
	v, minimum := "v"+version, "v"+c.Nomad.Minimum
	var problem string
	switch {
	case !isNomadRelease(version):
		problem = "is not a Nomad version such as " + c.Nomad.Recommended
	case semver.Compare(v, minimum) < 0:
		problem = fmt.Sprintf("is older than %s, the oldest Nomad that channel %s allows", c.Nomad.Minimum, c.Name)
	case semver.Major(v) != semver.Major(minimum):
		problem = fmt.Sprintf("is newer than this tent knows; channel %s allows %s.x from %s", c.Name,
			strings.TrimPrefix(semver.Major(minimum), "v"), c.Nomad.Minimum)
	default:
		return nil
	}
	return &VersionError{Version: version, Problem: problem}
}

// VersionError is a Nomad version that a channel does not allow, or that a cluster may not move to, and why.
type VersionError struct {
	Version string // as given
	Problem string // what is wrong with it, such as "is older than 2.0.0, the oldest Nomad that channel stable allows"
}

// Error returns the version, quoted unless it is X.Y.Z, and its problem.
func (e *VersionError) Error() string {
	v := e.Version
	if !isNomadRelease(v) {
		v = strconv.Quote(v)
	}
	return v + " " + e.Problem
}

// isNomadRelease reports whether version is a Nomad release X.Y.Z, such as 2.0.7.
func isNomadRelease(version string) bool {
	v := "v" + version
	return semver.Canonical(v) == v && semver.Prerelease(v) == ""
}

// Compare orders two Nomad releases X.Y.Z as semver does: -1 if a is older than b, 0 if they are the same, +1 if a is
// newer. It does not check that a and b are releases.
func Compare(a, b string) int { return semver.Compare("v"+a, "v"+b) }

// Release returns the release X.Y.Z of version, without its pre-release and build metadata, and "" for a text that is
// no version number.
func Release(version string) string {
	v := "v" + version
	if !semver.IsValid(v) {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSuffix(semver.Canonical(v), semver.Prerelease(v)), "v")
}

// NextMinor returns the first release of the minor version after version's: 2.1.0 for 2.0.7. It returns "" for a
// text that is no release X.Y.Z, and for a minor number too large to add one to.
func NextMinor(version string) string {
	if !isNomadRelease(version) {
		return ""
	}
	major, minor, _ := strings.Cut(strings.TrimPrefix(semver.MajorMinor("v"+version), "v"), ".")
	n, err := strconv.Atoi(minor)
	if err != nil || n == math.MaxInt {
		return ""
	}
	return major + "." + strconv.Itoa(n+1) + ".0"
}

// OlderThanMinimum reports whether version is a release X.Y.Z older than the oldest that the channel allows. A text
// that is no release is not.
func (c *Channel) OlderThanMinimum(version string) bool {
	return isNomadRelease(version) && Compare(version, c.Nomad.Minimum) < 0
}

// CheckMove returns nil when a cluster on the Nomad version from may move to the version to: to is the same or older,
// or newer by at most one minor version of the same major version. Otherwise it returns a *VersionError for to that
// says why. A from or a to that is no release X.Y.Z is not compared: Allows refuses it.
func CheckMove(from, to string) error {
	if !isNomadRelease(from) || !isNomadRelease(to) || Compare(to, from) <= 0 {
		return nil
	}
	var problem string
	switch {
	case semver.Major("v"+to) != semver.Major("v"+from):
		problem = "is of another major version than " + from
	case Compare(to, NextMinor(NextMinor(from))) >= 0:
		problem = "is more than one minor version above " + from
	default:
		return nil
	}
	return &VersionError{Version: to, Problem: problem}
}

// Tested reports whether the channel lists the Nomad version as tested with this tent.
func (c *Channel) Tested(version string) bool { return slices.Contains(c.Nomad.Tested, version) }

// decode reads the file of the named channel strictly: unknown fields, keys in the wrong case and duplicate keys
// fail.
func decode(name string, data []byte) (*Channel, error) {
	j, err := sigsyaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, fmt.Errorf("channel file %s.yaml: %w", name, err)
	}
	var c Channel
	strict, err := sigsjson.UnmarshalStrict(j, &c)
	if err := errors.Join(append(strict, err)...); err != nil {
		return nil, fmt.Errorf("channel file %s.yaml: %w", name, err)
	}
	return &c, nil
}
