package statestore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ingvarch/tent/internal/statestore"
)

type openCase struct {
	name string
	url  string
	dir  string // where objects land; empty for directories the test cannot write
	want string // String()
}

func TestOpenFileURL(t *testing.T) {
	dir := t.TempDir()
	tests := []openCase{
		{"directory", fileURL(dir), dir, fileURL(dir)},
		{"trailing slash", fileURL(dir) + "/", dir, fileURL(dir)},
		{"dot segments", fileURL(dir) + "/x/../y", filepath.Join(dir, "y"), fileURL(filepath.Join(dir, "y"))},
		{"escaped space", fileURL(dir) + "/my%20state", filepath.Join(dir, "my state"),
			fileURL(filepath.Join(dir, "my state"))},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests,
			openCase{"drive", "file:///C:/state", "", "file:///C:/state"},
			openCase{"lower-case drive", "file:///c:/x/state", "", "file:///c:/x/state"},
		)
	} else {
		tests = append(tests,
			openCase{"absolute", "file:///srv/tent", "", "file:///srv/tent"},
			openCase{"one slash", "file:/srv/tent", "", "file:///srv/tent"},
		)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := statestore.Open(t.Context(), tc.url)
			if err != nil {
				t.Fatalf("Open(%q): %v", tc.url, err)
			}
			if got := s.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			if tc.dir == "" {
				return
			}
			if _, err := s.Put(t.Context(), "probe", nil, statestore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(tc.dir, filepath.FromSlash("probe"))); err != nil {
				t.Errorf("the object is not in %s: %v", tc.dir, err)
			}
		})
	}
}

type rejectCase struct {
	name string
	url  string
	want string // part of the error
}

func TestOpenRejects(t *testing.T) {
	tests := []rejectCase{
		{"host", "file://state/dir", "file:///"},
		{"localhost", "file://localhost/srv/state", "file:///"},
		{"user", "file://igor@/srv/state", "environment"},
		{"relative", "file:state", "not absolute"},
		{"relative with dot", "file:./state", "not absolute"},
		{"no path", "file://", "not absolute"},
		{"query", "file:///srv/state?x=1", "query"},
		{"fragment", "file:///srv/state#top", "fragment"},
		{"no scheme", "/srv/state", "file:///"},
		{"empty", "", "file:///"},
		{"https", "https://example.com/state", `unsupported scheme "https"`},
		{"bad escape", "file:///srv/%zz", "invalid URL escape"},
		{"at sign", "file:///srv/a@b", "write an @ in a path as %40"},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests,
			rejectCase{"no drive", "file:///srv/state", "not absolute"},
			rejectCase{"drive-relative", "file:///C:state", "not absolute"},
			rejectCase{"drive root", "file:///C:/", "name a directory"},
		)
	} else {
		tests = append(tests,
			rejectCase{"disk root", "file:///", "name a directory"},
			rejectCase{"disk root with dots", "file:///srv/..", "name a directory"},
		)
	}
	testRejects(t, tests)
}

// testRejects checks that Open fails on every URL with an error that says why.
func testRejects(t *testing.T, tests []rejectCase) {
	t.Helper()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := statestore.Open(t.Context(), tc.url)
			if err == nil {
				t.Fatalf("Open(%q) = %v, want an error", tc.url, s)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Open(%q) error = %q, want it to contain %q", tc.url, err, tc.want)
			}
		})
	}
}

func TestOpenHidesCredentials(t *testing.T) {
	for _, u := range []string{
		"s3://AKIAKEY:s3cr%zzt@bucket/prefix", // does not parse
		"s3://AKIAKEY:s3cret@bucket/prefix",
		"https://AKIAKEY:s3cret@example.com/state",
		"file://AKIAKEY:s3cret@/srv/state",
		"file://AKIAKEY@/srv/state",
		// A slash in the secret ends the authority, so the URL has no user and the secret lands elsewhere.
		"s3://AKIAKEY:s3cr/def+ghi@bucket/prefix",
		"s3://AKIAKEY:123/def+ghi@bucket/prefix",
		"AKIAKEY:s3cret@bucket",
		// Secrets in other parts of the URL.
		"s3://bucket/prefix?secret=s3cret",
		"s3://bucket/prefix?AKIAKEY=s3cret",
		"s3://bucket/prefix?region=auto&pathStyle=s3cret",
		"s3://bucket/prefix?region=auto&endpoint=s3cret",
		"s3://bucket/prefix?region=auto&endpoint=https://example.com/s3cret",
		"s3://bucket/prefix?region=auto&endpoint=https://example.com%zzs3cret",
		"s3://bucket/prefix?region=s3cret&region=auto",
		"s3://bucket/prefix?region=auto&endpoint=https%3A%2F%2FAKIAKEY%3As3cret%40example.com",
		"s3://bucket/prefix?region=auto&endpoint=https%3A%2F%2FAKIAKEY%40example.com",
		"file:///srv/state?token=s3cret",
		"file:///srv/state#s3cret",
	} {
		_, err := statestore.Open(t.Context(), u)
		if err == nil {
			t.Errorf("Open(%q) succeeded", u)
			continue
		}
		for _, secret := range []string{"AKIAKEY", "s3cr", "def+ghi"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Open(%q) error %q shows %q", u, err, secret)
			}
		}
	}
}

func TestOpenCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := statestore.Open(ctx, fileURL(t.TempDir())); !errors.Is(err, context.Canceled) {
		t.Errorf("Open error = %v, want context.Canceled", err)
	}
}
