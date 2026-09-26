package statestore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

// ErrTentTooOld means a cluster needs a newer tent than the running one.
var ErrTentTooOld = errors.New("tent is too old for the cluster")

// describeSuffix matches what git describe appends to the tag of an untagged commit: -4-gabc1234.
var describeSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+$`)

// CheckVersion fails when the cluster was written by a newer tent than the running one. The running version is a
// release (v0.3.0), a pre-release (v0.3.0-rc.1) or git describe output (v0.3.0-4-gabc1234, which counts as v0.3.0);
// anything else, snapshot builds included, is a development build, which passes. A cluster without a recorded
// version passes.
func CheckVersion(ctx context.Context, s Store, l Layout, running string) error {
	release := releaseOf(running)
	if release == "" {
		return nil
	}
	stored, err := readVersion(ctx, s, l)
	if err != nil {
		return err
	}
	if semver.Compare(release, stored) < 0 {
		return tooOldError(fmt.Sprintf("cluster %s needs tent %s or newer; this is %s", l.Cluster(), stored, running))
	}
	return nil
}

// RaiseVersion records the running version as the cluster's minimum, if it is newer than the stored one. It reads
// and then writes, so callers hold the cluster's lock. A development build writes nothing.
func RaiseVersion(ctx context.Context, s Store, l Layout, running string) error {
	release := releaseOf(running)
	if release == "" {
		return nil
	}
	stored, err := readVersion(ctx, s, l)
	if err != nil {
		return err
	}
	if semver.Compare(release, stored) <= 0 {
		return nil
	}
	_, err = s.Put(ctx, l.TentVersion(), []byte(release+"\n"), PutOptions{})
	return err
}

// releaseOf returns the version a running tent counts as, or "" for a development build. Semver would read git
// describe output as a pre-release older than its tag, so the tag stands for it. A snapshot build, such as
// v0.3.0-SNAPSHOT-abc1234, carries the last tag and may hold later commits, so it is a development build.
func releaseOf(running string) string {
	v := describeSuffix.ReplaceAllString(strings.TrimSuffix(running, "-dirty"), "")
	if strings.Contains(v, "-SNAPSHOT") || !isVersion(v) {
		return ""
	}
	return v
}

// readVersion returns the cluster's minimum tent version, or "" when none is recorded. Semver orders "" before every
// version.
func readVersion(ctx context.Context, s Store, l Layout) (string, error) {
	data, _, err := s.Get(ctx, l.TentVersion())
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	v := strings.TrimSuffix(string(data), "\n")
	if !isVersion(v) {
		return "", fmt.Errorf("%s holds %q, not a tent version", l.TentVersion(), v)
	}
	return v, nil
}

// isVersion reports whether v is vX.Y.Z or vX.Y.Z-pre.
func isVersion(v string) bool { return semver.IsValid(v) && semver.Canonical(v) == v }

// tooOldError is ErrTentTooOld with a message that names both versions.
type tooOldError string

func (e tooOldError) Error() string { return string(e) }

func (tooOldError) Unwrap() error { return ErrTentTooOld }
