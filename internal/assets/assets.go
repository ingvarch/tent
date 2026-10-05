// Package assets finds the files nodes download, Nomad, the CNI plugins and tent-node, with the sha256 each must
// have. It checks what it reads on the operator's side, so nodes only compare sha256s.
package assets

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Asset is a file a node downloads.
type Asset struct {
	Name    string   // nomad, cni-plugins or tent-node
	Version string   // the version of what the file holds
	URLs    []string // where to download it, mirrors in order
	SHA256  string   // lower-case hex
}

// Options say where the assets come from. The zero value reads the public release sites with http.DefaultClient.
type Options struct {
	Client *http.Client // nil means http.DefaultClient; redirects go only to https unless it has a CheckRedirect

	// The tent-node of a development build of tent, which no release holds: where nodes download it
	// (TENT_NODE_URL) and its sha256 (TENT_NODE_SHA256). tent trusts both as given: nodes check the file against
	// that sha256 alone.
	DevURL    string
	DevSHA256 string

	// Now returns the time at which signatures and the keys that made them are checked; nil means time.Now.
	Now func() time.Time

	// Set by tests only.
	nomadURL string // Nomad's releases; "" means nomadReleases
	tentURL  string // tent's releases; "" means tentReleases
	nomadKey string // the armored key that signs Nomad's SHA256SUMS; "" means HashiCorp's release key
}

// Where the assets come from unless Options say otherwise.
const (
	nomadReleases = "https://releases.hashicorp.com/nomad"
	tentReleases  = "https://github.com/ingvarch/tent/releases/download"
	cniReleases   = "https://github.com/containernetworking/plugins/releases/download"
)

// sha256Hex matches a sha256 as release files write it: 64 lower-case hex digits.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxFileSize bounds what get reads: checksum lists and signatures take a few kilobytes.
const maxFileSize = 1 << 20

// client returns a copy of the client in the options, or of http.DefaultClient, that follows redirects only to https,
// unless the client has a redirect policy of its own.
func (o Options) client() *http.Client {
	c := *cmp.Or(o.Client, http.DefaultClient)
	if c.CheckRedirect == nil {
		c.CheckRedirect = httpsRedirects
	}
	return &c
}

// httpsRedirects follows up to 10 redirects, as http.Client does by default, and only to https: the files are trusted
// because TLS brought them.
func httpsRedirects(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("redirected to %s, which is not https", req.URL)
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// get reads the file at rawURL with one request, which ctx bounds.
func (o Options) get(ctx context.Context, rawURL string) ([]byte, error) {
	return o.getUpTo(ctx, rawURL, maxFileSize)
}

// getUpTo is get for a file of up to limit bytes.
func (o Options) getUpTo(ctx context.Context, rawURL string, limit int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", rawURL, err)
	}
	resp, err := o.client().Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // the error names the URL once
		}
		return nil, fmt.Errorf("get %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: %s", rawURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("read %s: larger than %d bytes", rawURL, limit)
	}
	return data, nil
}

// releaseDir returns the URL of a release's directory, ending in a slash: the version below base, or below def when
// base is empty.
func releaseDir(base, def, version string) string {
	return strings.TrimRight(cmp.Or(base, def), "/") + "/" + version + "/"
}

// sumOf returns the sha256 that data, the checksum file read from fileURL, lists for file. Each line holds a sha256,
// two spaces and a file name, as sha256sum writes them. A malformed line, no line or a second line for file fails.
func sumOf(fileURL string, data []byte, file string) (string, error) {
	var sum string
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		hash, name, ok := strings.Cut(line, "  ")
		if !ok || !sha256Hex.MatchString(hash) || name == "" {
			return "", fmt.Errorf("%s line %d: want a sha256, two spaces and a file name", fileURL, i+1)
		}
		if name != file {
			continue
		}
		if sum != "" {
			return "", fmt.Errorf("%s lists %s twice", fileURL, file)
		}
		sum = hash
	}
	if sum == "" {
		return "", fmt.Errorf("%s has no line for %s", fileURL, file)
	}
	return sum, nil
}
