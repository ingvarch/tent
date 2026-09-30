package nodeup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"slices"
	"time"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup/retry"
)

// assetDir is where tent-node keeps the assets that it downloaded, one file for each, named after the asset. Only
// root reads it.
const assetDir = "/var/lib/tent/assets"

// fetchLimits bound the downloads of fetch.
type fetchLimits struct {
	tries int           // of a URL
	wait  time.Duration // between two tries of a URL
	try   time.Duration // the longest a try takes, its body included
	idle  time.Duration // the longest wait for the answer, and for each byte of its body
	size  int64         // the most bytes an asset has
}

// maxRedirects is the most redirects that a try of a URL follows.
const maxRedirects = 10

// assetLimits are the limits of the downloads of the phases.
var assetLimits = fetchLimits{
	tries: 3, wait: 2 * time.Second, try: 10 * time.Minute, idle: time.Minute, size: 256 << 20,
}

// fetch returns the content of the asset a, which it checks against a's sha256, and reports whether that changed the
// cache. The cache file of an asset holds the one version that the NodeConfig names: a file with a's sha256 serves
// without a request, and any other one, such as an older version, is removed before a is downloaded, checked and
// written in its place. Errors name the asset.
func fetch(ctx context.Context, h *Host, a nodeconfig.Asset) ([]byte, bool, error) {
	return assetLimits.fetch(ctx, h, a)
}

func (l fetchLimits) fetch(ctx context.Context, h *Host, a nodeconfig.Asset) ([]byte, bool, error) {
	if len(a.URLs) == 0 {
		return nil, false, fmt.Errorf("fetch %s %s: the asset has no URLs", a.Name, a.Version)
	}
	data, changed, err := l.cache(ctx, h, a)
	if err != nil {
		return nil, false, fmt.Errorf("fetch %s %s: %w", a.Name, a.Version, err)
	}
	return data, changed, nil
}

// cache returns the content of a from assetDir, or downloads it there.
func (l fetchLimits) cache(ctx context.Context, h *Host, a nodeconfig.Asset) (data []byte, changed bool, err error) {
	for _, dir := range []string{path.Dir(assetDir), assetDir} {
		c, err := h.FS.EnsureDir(dir, 0o700, nodeconfig.Owner)
		if err != nil {
			return nil, false, err
		}
		changed = changed || c
	}
	file := assetDir + "/" + a.Name
	data, err = h.FS.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, false, err
	case sha256Hex(data) == a.SHA256:
		// WriteFile only gives a file that someone changed the mode or owner back.
		c, err := h.FS.WriteFile(file, data, 0o600, nodeconfig.Owner)
		return data, changed || c, err
	default:
		if _, err := h.FS.Remove(file); err != nil {
			return nil, false, err
		}
	}
	h.logger().Info("download", "asset", a.Name, "version", a.Version)
	if data, err = l.download(ctx, h, a); err != nil {
		return nil, false, err
	}
	if _, err := h.FS.WriteFile(file, data, 0o600, nodeconfig.Owner); err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// download gets a from each of its URLs in order until one gives a file with a's sha256. It tries a URL again, l.wait
// after a failure, up to l.tries times, while get says the failure is worth it; other failures move on to the next
// URL at once. The error holds the last failure of each URL. When ctx ends, download stops at once, and the error
// holds the failures so far and ctx's cause.
func (l fetchLimits) download(ctx context.Context, h *Host, a nodeconfig.Asset) ([]byte, error) {
	transport := h.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var errs []error
	for _, raw := range a.URLs {
		shown := nodeconfig.RedactURL(raw)
		var last error // the last failure of raw
		for try := 1; try <= l.tries; try++ {
			if try > 1 && !retry.Sleep(ctx, l.wait) {
				break
			}
			data, again, err := l.get(ctx, transport, raw, a.SHA256)
			if err == nil {
				return data, nil
			}
			if ctx.Err() != nil { // the try stopped with ctx
				break
			}
			h.logger().Warn("download failed", "url", shown, "try", try, "error", err)
			last = fmt.Errorf("GET %s: %w", shown, err)
			if !again {
				break
			}
		}
		if last != nil {
			errs = append(errs, last)
		}
		if ctx.Err() != nil {
			return nil, errors.Join(append(errs, fmt.Errorf("stopped: %w", context.Cause(ctx)))...)
		}
	}
	return nil, errors.Join(errs...)
}

// get downloads the URL raw once through transport, following up to maxRedirects redirects, and checks that the body
// has the sha256 sum. again reports whether a failure is worth another try: a failed connection, a limit of l's on
// time, or a status that retry.Status takes. No error shows a URL: a redirect's may carry a signature too.
func (l fetchLimits) get(ctx context.Context, transport http.RoundTripper, raw, sum string) (
	data []byte, again bool, err error,
) {
	tryCtx, cancelTry := context.WithTimeoutCause(ctx, l.try, fmt.Errorf("no whole answer within %v", l.try))
	defer cancelTry()
	idleCtx, cancel := context.WithCancelCause(tryCtx)
	defer cancel(nil)
	// limit returns the cause of a limit that stopped the try, or err.
	limit := func(err error) error {
		if idleCtx.Err() != nil && ctx.Err() == nil {
			return context.Cause(idleCtx)
		}
		return err
	}
	resp, again, err := l.answer(idleCtx, cancel, transport, raw)
	if err != nil {
		return nil, again, limit(err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode != http.StatusOK:
		return nil, retry.Status(resp.StatusCode), fmt.Errorf("answered %s", resp.Status)
	case resp.ContentLength > l.size:
		return nil, false, fmt.Errorf("the answer has %d bytes, more than %d", resp.ContentLength, l.size)
	}
	body := &idleReader{r: resp.Body, idle: l.idle,
		timer: time.AfterFunc(l.idle, func() { cancel(fmt.Errorf("no data for %v", l.idle)) })}
	defer body.timer.Stop()
	var buf bytes.Buffer
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(&buf, hash), io.LimitReader(body, l.size+1))
	switch {
	case err != nil:
		return nil, true, limit(fmt.Errorf("read the answer: %w", err))
	case n > l.size:
		return nil, false, fmt.Errorf("the answer has more than %d bytes", l.size)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != sum {
		return nil, false, fmt.Errorf("the sha256 is %s, not %s", got, sum)
	}
	return buf.Bytes(), false, nil
}

// answer returns the answer to a GET of raw through transport, after up to maxRedirects redirects, each of which
// answers within l.idle, or cancel ends ctx. It follows the redirects itself, as net/http's client quotes a Location
// in its errors, and a Location may carry a signature. A URL's user and password go as basic authentication, as
// net/http's client sends them. again reports whether a failure is worth another try: only a failed request is. A
// redirect whose Location is missing or does not parse fails, as does one more than maxRedirects.
func (l fetchLimits) answer(ctx context.Context, cancel context.CancelCauseFunc, transport http.RoundTripper,
	raw string,
) (resp *http.Response, again bool, err error) {
	for redirects := 0; ; redirects++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, false, withoutURL(err)
		}
		if user := req.URL.User; user != nil {
			password, _ := user.Password()
			req.SetBasicAuth(user.Username(), password)
		}
		// The file as it is: a transport that asks for gzip decodes an answer that a server marks gzip-encoded,
		// as some mark a .tgz.
		req.Header.Set("Accept-Encoding", "identity")
		timer := time.AfterFunc(l.idle, func() { cancel(fmt.Errorf("no answer within %v", l.idle)) })
		resp, err := transport.RoundTrip(req)
		timer.Stop()
		if err != nil {
			return nil, true, err
		}
		if !slices.Contains(redirectStatuses, resp.StatusCode) {
			return resp, false, nil
		}
		_ = resp.Body.Close() // unread: the connection may go
		location := resp.Header.Get("Location")
		next, err := req.URL.Parse(location)
		switch {
		case location == "":
			return nil, false, fmt.Errorf("answered %s without a Location", resp.Status)
		case err != nil:
			return nil, false, fmt.Errorf("answered %s with a Location that does not parse", resp.Status)
		case redirects == maxRedirects:
			return nil, false, fmt.Errorf("more than %d redirects", maxRedirects)
		}
		raw = next.String()
	}
}

// redirectStatuses are the statuses of the redirects that answer follows, as net/http's client does.
var redirectStatuses = []int{
	http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect,
	http.StatusPermanentRedirect,
}

// idleReader reads r, and pushes the timer back to idle whenever a byte arrives.
type idleReader struct {
	r     io.Reader
	idle  time.Duration
	timer *time.Timer
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	return n, err
}

// withoutURL returns the error that err wraps when it is an error of net/url, whose text shows the URL.
func withoutURL(err error) error {
	if u, ok := errors.AsType[*url.Error](err); ok {
		return u.Err
	}
	return err
}

// sha256Hex returns the sha256 of data in hex.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
