package statestore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/ingvarch/tent/internal/buildinfo"
)

// ErrTentTooOld means a cluster needs a newer tent than the running one.
var ErrTentTooOld = errors.New("tent is too old for the cluster")

// CheckVersion fails when the cluster was written by a newer tent than the running one. The running version is a
// release (v0.3.0), a pre-release (v0.3.0-rc.1) or git describe output (v0.3.0-4-gabc1234, which counts as v0.3.0);
// anything else, snapshot builds included, is a development build, which passes. A cluster without a recorded
// version passes.
func CheckVersion(ctx context.Context, s Store, l Layout, running string) error {
	release := buildinfo.Release(running)
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
	release := buildinfo.Release(running)
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
	if !buildinfo.IsVersion(v) {
		return "", fmt.Errorf("%s holds %q, not a tent version", l.TentVersion(), v)
	}
	return v, nil
}

// tooOldError is ErrTentTooOld with a message that names both versions.
type tooOldError string

func (e tooOldError) Error() string { return string(e) }

func (tooOldError) Unwrap() error { return ErrTentTooOld }
