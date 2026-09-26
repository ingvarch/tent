// Package buildinfo reports the version, commit and build date that the linker injects.
package buildinfo

import (
	"fmt"
	"runtime"
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
