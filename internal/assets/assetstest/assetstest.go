// Package assetstest serves release files to tests without a network: HashiCorp's signed SHA256SUMS of Nomad 2.0.7
// and 2.0.6, and the checksums of tent's releases. It imports only the standard library, so any test can use it.
package assetstest

import (
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// What the files hold, and the build of tent that tests pass as a development build.
const (
	// NomadVersion is the newer Nomad whose SHA256SUMS the package serves.
	NomadVersion = "2.0.7"
	// NomadSHA256 is the sum of the linux amd64 zip in the signed SHA256SUMS.
	NomadSHA256 = "4c9b8a0850d6fd9caadbbab09b3e6fdf8b77aa777729543c70c61b85acca68c1"
	// OlderNomadVersion is the older Nomad whose SHA256SUMS the package serves.
	OlderNomadVersion = "2.0.6"
	// OlderNomadSHA256 is the sum of its linux amd64 zip in the signed SHA256SUMS.
	OlderNomadSHA256 = "c6da734081a41d51cadf15ec880d5eab0313093a29d2ee0984ca5ae1cdf867ec"
	// TentNodeSHA256 is the made-up sum that every release's checksums.txt gives for both tent-node files.
	TentNodeSHA256 = "3333333333333333333333333333333333333333333333333333333333333333"
	// DevURL and DevSHA256 are the tent-node of a development build.
	DevURL    = "https://tent-dev.s3.example.com/tent-node_linux_amd64?X-Amz-Signature=0123"
	DevSHA256 = "4444444444444444444444444444444444444444444444444444444444444444"
)

const tentRelease = "https://github.com/ingvarch/tent/releases/download/"

var (
	//go:embed testdata/nomad_2.0.7_SHA256SUMS
	nomadSums string
	//go:embed testdata/nomad_2.0.7_SHA256SUMS.sig
	nomadSig string
	//go:embed testdata/nomad_2.0.6_SHA256SUMS
	olderNomadSums string
	//go:embed testdata/nomad_2.0.6_SHA256SUMS.sig
	olderNomadSig string
)

// Now is a time at which the signature of the Nomad files verifies with the embedded key.
func Now() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }

// Sites is an http.RoundTripper that answers by full URL. Any other URL is 404.
type Sites struct {
	mu    sync.Mutex
	files map[string]string
	urls  []string
}

// New returns Sites that serve the Nomad files of both versions and the checksums of every release of tent.
func New() *Sites {
	files := map[string]string{}
	for _, f := range []struct{ version, sums, sig string }{
		{NomadVersion, nomadSums, nomadSig},
		{OlderNomadVersion, olderNomadSums, olderNomadSig},
	} {
		name := "https://releases.hashicorp.com/nomad/" + f.version + "/nomad_" + f.version + "_SHA256SUMS"
		files[name], files[name+".sig"] = f.sums, f.sig
	}
	return &Sites{files: files}
}

// URLs returns the URLs asked for, in order.
func (s *Sites) URLs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.urls...)
}

// NomadSums returns the signed SHA256SUMS of NomadVersion and its signature as served.
func NomadSums() (sums, sig string) { return nomadSums, nomadSig }

// RoundTrip answers the request from the files.
func (s *Sites) RoundTrip(r *http.Request) (*http.Response, error) {
	url := r.URL.String()
	s.mu.Lock()
	s.urls = append(s.urls, url)
	body, ok := s.files[url]
	s.mu.Unlock()
	if !ok && strings.HasPrefix(url, tentRelease) && strings.HasSuffix(url, "/checksums.txt") {
		body, ok = fmt.Sprintf("%[1]s  tent-node_linux_amd64\n%[1]s  tent-node_linux_arm64\n", TentNodeSHA256), true
	}
	code := http.StatusOK
	if !ok {
		code = http.StatusNotFound
	}
	return &http.Response{
		StatusCode: code,
		Status:     fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}
