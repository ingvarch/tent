package assetstest_test

import (
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/assets/assetstest"
)

func get(t *testing.T, s *assetstest.Sites, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestNomadFilesVerify(t *testing.T) {
	opts := assets.Options{Client: &http.Client{Transport: assetstest.New()}, Now: assetstest.Now}
	a, err := assets.Nomad(t.Context(), opts, assetstest.NomadVersion, "amd64")
	if err != nil {
		t.Fatalf("Nomad: %v", err)
	}
	if a.SHA256 != assetstest.NomadSHA256 {
		t.Errorf("sha256 = %s, want %s", a.SHA256, assetstest.NomadSHA256)
	}
}

func TestTentNodeChecksums(t *testing.T) {
	opts := assets.Options{Client: &http.Client{Transport: assetstest.New()}, Now: assetstest.Now}
	for _, arch := range []string{"amd64", "arm64"} {
		a, err := assets.TentNode(t.Context(), opts, "v0.3.0", arch)
		if err != nil {
			t.Fatalf("TentNode %s: %v", arch, err)
		}
		if a.SHA256 != assetstest.TentNodeSHA256 {
			t.Errorf("%s: sha256 = %s, want %s", arch, a.SHA256, assetstest.TentNodeSHA256)
		}
	}
}

func TestUnknownURLIsNotFound(t *testing.T) {
	for _, url := range []string{
		"https://example.com/nothing",
		"https://example.com/checksums.txt",
		"https://github.com/ingvarch/tent/releases/download/v0.3.0/tent-node_linux_amd64",
	} {
		if code, _ := get(t, assetstest.New(), url); code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", url, code)
		}
	}
}

func TestURLsAreRecordedInOrder(t *testing.T) {
	s := assetstest.New()
	const first, second = "https://example.com/a", "https://example.com/b"
	get(t, s, first)
	get(t, s, second)
	if want := []string{first, second}; !slices.Equal(s.URLs(), want) {
		t.Errorf("URLs = %v, want %v", s.URLs(), want)
	}
}
