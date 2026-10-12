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

// TestNomadFilesVerify proves that the embedded files of both versions are HashiCorp's: the signature verifies.
func TestNomadFilesVerify(t *testing.T) {
	opts := assets.Options{Client: &http.Client{Transport: assetstest.New()}, Now: assetstest.Now}
	for _, tc := range []struct{ version, sha256 string }{
		{assetstest.NomadVersion, assetstest.NomadSHA256},
		{assetstest.OlderNomadVersion, assetstest.OlderNomadSHA256},
	} {
		a, err := assets.Nomad(t.Context(), opts, tc.version, "amd64")
		if err != nil {
			t.Fatalf("Nomad %s: %v", tc.version, err)
		}
		if a.SHA256 != tc.sha256 {
			t.Errorf("Nomad %s: sha256 = %s, want %s", tc.version, a.SHA256, tc.sha256)
		}
	}
}

func TestNomadVersionWithoutFilesIsNotFound(t *testing.T) {
	const base = "https://releases.hashicorp.com/nomad/2.0.99/nomad_2.0.99_SHA256SUMS"
	for _, url := range []string{base, base + ".sig"} {
		if code, _ := get(t, assetstest.New(), url); code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", url, code)
		}
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
