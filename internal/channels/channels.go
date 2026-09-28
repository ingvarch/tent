// Package channels holds the release channels embedded in tent. A channel sets the Nomad versions a cluster may run,
// recommends one and lists those tested, and pins the CNI plugins.
package channels

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
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

// VersionError is a Nomad version that a channel does not allow, and why.
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
