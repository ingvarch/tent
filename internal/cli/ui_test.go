package cli

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
	"github.com/ingvarch/tent/internal/secrettest"
	"github.com/ingvarch/tent/internal/statestore"
)

// The tests of tent ui use real sockets on loopback port 0, so they run outside synctest bubbles, and a cluster that
// was built with the real clock. They wait on channels and never sleep; the time limits only fail a test that hangs.

// uiGuard is how long a test waits for something that must happen before it fails.
const uiGuard = 15 * time.Second

// notifyBuffer is a buffer that tells when something was written to it.
type notifyBuffer struct {
	syncBuffer
	wrote chan struct{}
}

func newNotifyBuffer() *notifyBuffer { return &notifyBuffer{wrote: make(chan struct{}, 1)} }

func (b *notifyBuffer) Write(p []byte) (int, error) {
	n, err := b.syncBuffer.Write(p)
	select {
	case b.wrote <- struct{}{}:
	default:
	}
	return n, err
}

// stubProxy is the proxy factory of the tests: it records the configs it got and serves handler, or what inner
// builds.
type stubProxy struct {
	mu      sync.Mutex
	cfgs    []nomadops.ProxyConfig
	handler http.Handler
	inner   func(nomadops.ProxyConfig) (http.Handler, error) // when set, builds the handler instead
	onBuild func()                                           // runs after the config is recorded
	err     error                                            // when set, the factory fails with it
}

func (p *stubProxy) build(cfg nomadops.ProxyConfig) (http.Handler, error) {
	p.mu.Lock()
	p.cfgs = append(p.cfgs, cfg)
	p.mu.Unlock()
	if p.onBuild != nil {
		p.onBuild()
	}
	switch {
	case p.err != nil:
		return nil, p.err
	case p.inner != nil:
		return p.inner(cfg)
	}
	return p.handler, nil
}

// configs returns the configs that the factory got.
func (p *stubProxy) configs() []nomadops.ProxyConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]nomadops.ProxyConfig(nil), p.cfgs...)
}

// watchedListener is a listener that tells when it is closed, may report another address than its own, and may fail
// to accept.
type watchedListener struct {
	net.Listener
	reportAs  net.Addr
	acceptErr error
	closed    chan struct{}
	once      sync.Once
}

func (l *watchedListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

func (l *watchedListener) Addr() net.Addr {
	if l.reportAs != nil {
		return l.reportAs
	}
	return l.Listener.Addr()
}

func (l *watchedListener) Accept() (net.Conn, error) {
	if l.acceptErr != nil {
		return nil, l.acceptErr
	}
	return l.Listener.Accept()
}

// uiEnv is a built test cluster, the Nomad world and the proxy stub that a run of tent ui works with.
type uiEnv struct {
	t       *testing.T
	s       state
	f       *vultrfake.Fake
	nomad   *nomadfake.Fake
	factory func(nomadops.Config) (nomadops.API, error)
	proxy   *stubProxy
	noProxy bool       // tent gets no proxy factory
	ui      uiSettings // the settings the test replaces
	// watched is the listener that the run opened, when watch was called.
	watched atomic.Pointer[watchedListener]
	// reportAs and acceptErr are what the watched listener does, as watchedListener says.
	reportAs  net.Addr
	acceptErr error
}

// newUIEnv builds the test cluster in a bubble whose clock is first set to the real time, so that the CA of the
// cluster is valid for the real clock that the run of tent ui reads. The bubble ends before the run, which opens
// sockets.
func newUIEnv(t *testing.T) *uiEnv {
	t.Helper()
	exportEnv(t)
	s := withCluster(t)
	var f *vultrfake.Fake
	realNow := time.Now()
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Until(realNow))
		f = buildCluster(t, s)
	})
	nomad, factory := nomadWorld("servers", "workers", 6)
	return &uiEnv{t: t, s: s, f: f, nomad: nomad, factory: factory, proxy: &stubProxy{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "stub "+r.URL.Path)
		}),
	}}
}

// watch makes the run open its listener through a watchedListener, which e.watched holds once the run has opened it.
func (e *uiEnv) watch() {
	e.ui.listen = func(ctx context.Context, address string) (net.Listener, error) {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}
		w := &watchedListener{Listener: ln, reportAs: e.reportAs, acceptErr: e.acceptErr, closed: make(chan struct{})}
		e.watched.Store(w)
		return w, nil
	}
}

// uiRun is tent ui running in another goroutine.
type uiRun struct {
	t        *testing.T
	out, err *notifyBuffer
	cancel   context.CancelFunc
	finished chan struct{}
	code     int // valid once finished is closed
}

// start runs tent ui for the test cluster on a port that the system picks, with more arguments after the defaults.
func (e *uiEnv) start(more ...string) *uiRun {
	e.t.Helper()
	return e.startArgs(append([]string{"ui", "prod", "--state", e.s.url, "--listen", "127.0.0.1:0"}, more...)...)
}

// startArgs runs tent with args.
func (e *uiEnv) startArgs(args ...string) *uiRun {
	e.t.Helper()
	ctx, cancel := context.WithCancel(e.t.Context())
	r := &uiRun{t: e.t, out: newNotifyBuffer(), err: newNotifyBuffer(), cancel: cancel, finished: make(chan struct{})}
	opts := &globalOptions{providers: onVultr(e.f), assets: testAssets(), nomad: e.factory, ui: e.ui}
	if !e.noProxy {
		opts.nomadProxy = e.proxy.build
	}
	go func() {
		defer close(r.finished)
		root := newRootCommand(Streams{In: strings.NewReader(""), Out: r.out, Err: r.err}, opts)
		r.code = execute(ctx, root, args, r.err)
	}()
	e.t.Cleanup(func() {
		cancel()
		select {
		case <-r.finished:
		case <-time.After(uiGuard):
			e.t.Error("tent ui did not end after its context ended")
		}
	})
	return r
}

// exited waits for tent to end by itself and returns what it did.
func (r *uiRun) exited() result {
	r.t.Helper()
	select {
	case <-r.finished:
	case <-time.After(uiGuard):
		r.t.Fatalf("tent ui did not end; stdout %q, stderr %q", r.out.String(), r.err.String())
	}
	return result{r.code, r.out.String(), r.err.String()}
}

// stop ends tent as Ctrl-C does and returns what it did.
func (r *uiRun) stop() result {
	r.t.Helper()
	r.cancel()
	return r.exited()
}

// uiURLPattern finds the URL of the UI in what tent ui printed.
var uiURLPattern = regexp.MustCompile(`http://[^/\s"]+/ui/`)

// ready waits until tent ui has printed its lines and returns the UI's URL.
func (r *uiRun) ready() string {
	r.t.Helper()
	timeout := time.After(uiGuard)
	for !strings.Contains(r.err.String(), "press Ctrl-C to stop") {
		select {
		case <-r.err.wrote:
		case <-r.finished:
			r.t.Fatalf("tent ui ended with %d before it printed its lines; stdout %q, stderr %q", r.code,
				r.out.String(), r.err.String())
		case <-timeout:
			r.t.Fatalf("tent ui printed no lines; stdout %q, stderr %q", r.out.String(), r.err.String())
		}
	}
	url := uiURLPattern.FindString(r.out.String())
	if url == "" {
		r.t.Fatalf("stdout %q holds no URL of the UI", r.out.String())
	}
	return url
}

// uiAnswer is what a request to the port got.
type uiAnswer struct {
	status int
	body   string
	err    error
}

// uiGet gets url with the Host header host, or the URL's own when host is empty, over a connection of its own.
func uiGet(t *testing.T, url, host string) uiAnswer {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*uiGuard) // longer than any wait for the answer
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return uiAnswer{err: err}
	}
	if host != "" {
		req.Host = host
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return uiAnswer{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return uiAnswer{status: resp.StatusCode, body: string(body), err: err}
}

// hostOf returns the host and port of a URL that uiURLPattern found.
func hostOf(url string) string {
	return strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/ui/")
}

// wantRefused fails the test when something still accepts connections at address.
func wantRefused(t *testing.T, address string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, uiGuard)
	if err == nil {
		_ = conn.Close()
		t.Errorf("%s still accepts connections", address)
	}
}

// wantNoAccess fails the test unless the run asked Nomad for no token and called the proxy factory not once.
func (e *uiEnv) wantNoAccess() {
	e.t.Helper()
	if calls := e.nomad.Calls(); len(calls) != 0 {
		e.t.Errorf("Nomad got the calls %v", calls)
	}
	if issued := e.nomad.Issued(); len(issued) != 0 {
		e.t.Errorf("Nomad issued %v", issued)
	}
	if cfgs := e.proxy.configs(); len(cfgs) != 0 {
		e.t.Errorf("the proxy factory got %d configs", len(cfgs))
	}
}

// wantUILines fails the test unless the result holds exactly the two lines of tent ui for the test cluster, with the
// same port in both, and returns the URL and the end of the session that they say.
func wantUILines(t *testing.T, got result) (url string, ends time.Time) {
	t.Helper()
	out := regexp.MustCompile(`^Nomad UI of cluster prod: (http://127\.0\.0\.1:(\d+)/ui/)\n$`).FindStringSubmatch(got.out)
	notice := regexp.MustCompile(`^the Nomad CLI works through it with NOMAD_ADDR=http://127\.0\.0\.1:(\d+); ` +
		`press Ctrl-C to stop; the session ends at (\d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC)\n$`).FindStringSubmatch(got.errOut)
	if out == nil || notice == nil {
		t.Fatalf("stdout %q and stderr %q do not hold the two lines of tent ui", got.out, got.errOut)
	}
	if out[2] == "0" || out[2] != notice[1] {
		t.Errorf("the URL has the port %s and NOMAD_ADDR the port %s, want one real port", out[2], notice[1])
	}
	ends, err := time.Parse(noticeTime, notice[2])
	if err != nil {
		t.Fatal(err)
	}
	return out[1], ends
}

// TestUIServesTheProxyAndStopsOnCtrlC prints the URL and the notice, serves the proxy's handler on the printed port,
// and on Ctrl-C ends with code 0 and closes the port. The session ends a day from now.
func TestUIServesTheProxyAndStopsOnCtrlC(t *testing.T) {
	e := newUIEnv(t)
	r := e.start()
	base := r.ready()

	ans := uiGet(t, strings.TrimSuffix(base, "ui/")+"v1/jobs", "")
	got := r.stop()

	if ans.err != nil || ans.status != http.StatusOK || ans.body != "stub /v1/jobs" {
		t.Errorf("a request got %+v, want 200 and the proxy's answer", ans)
	}
	if got.code != 0 {
		t.Errorf("exit code %d, want 0\n%s", got.code, got.errOut)
	}
	url, ends := wantUILines(t, got)
	if url != base {
		t.Errorf("the printed URL is %s, want %s", url, base)
	}
	if left := time.Until(ends); left > 24*time.Hour || left < 24*time.Hour-time.Minute {
		t.Errorf("the session ends in %s, want about 24h", left)
	}
	wantRefused(t, hostOf(base))
}

// TestUIGivesTheProxyItsConfig builds the proxy with every server, the region, the CA, a certificate pair, a token
// that is not the bootstrap secret, the listener's real address and the command's logger; Nomad made one token of
// 24h for the purpose ui, and the state store is as it was.
func TestUIGivesTheProxyItsConfig(t *testing.T) {
	e := newUIEnv(t)
	before := e.s.objects(t)
	r := e.start()
	base := r.ready()

	r.stop()

	cfgs := e.proxy.configs()
	if len(cfgs) != 1 {
		t.Fatalf("the proxy factory got %d configs, want one", len(cfgs))
	}
	cfg := cfgs[0]
	if len(cfg.Servers) != 3 || cfg.Servers[0] != tokenServer(t, e.nomad) {
		t.Errorf("Servers = %v, want the three servers, the one that made the token first", cfg.Servers)
	}
	for _, s := range cfg.Servers {
		if !strings.HasSuffix(s, ":4646") {
			t.Errorf("server %s is no API address", s)
		}
	}
	if cfg.Region != "global" {
		t.Errorf("Region = %q, want global", cfg.Region)
	}
	if diff := cmp.Diff(before["prod/pki/ca-bundle.pem"], string(cfg.CA)); diff != "" {
		t.Errorf("CA (-want +got):\n%s", diff)
	}
	if _, err := tls.X509KeyPair(cfg.Cert.Cert, cfg.Cert.Key); err != nil {
		t.Errorf("Cert and its key are no pair: %v", err)
	}
	if len(cfg.Token) != 36 || string(cfg.Token) == before["prod/secrets/acl-bootstrap-token"] {
		t.Errorf("Token has %d bytes or is the bootstrap secret, want a new UUID", len(cfg.Token))
	}
	if cfg.Listen != hostOf(base) {
		t.Errorf("Listen = %q, want the listener's address %q", cfg.Listen, hostOf(base))
	}
	if cfg.Log == nil {
		t.Error("Log is nil, want the command's logger")
	}
	owner, host := statestore.LocalHolder()
	if issued := e.nomad.Issued(); len(issued) != 1 || issued[0].TTL != 24*time.Hour ||
		issued[0].Name != "tent ui "+owner+"@"+host {
		t.Errorf("Nomad issued %+v, want one token of 24h named for ui", issued)
	}
	e.s.want(t, before)
}

// TestUIListensOnLocalhostAndPrintsTheRealAddress accepts the name localhost with port 0 and prints the address that
// the system gave, not the name.
func TestUIListensOnLocalhostAndPrintsTheRealAddress(t *testing.T) {
	e := newUIEnv(t)
	r := e.startArgs("ui", "prod", "--state", e.s.url, "--listen", "localhost:0")
	base := r.ready()

	r.stop()

	if !regexp.MustCompile(`^http://(127\.0\.0\.1|\[::1\]):[1-9]\d*/ui/$`).MatchString(base) {
		t.Errorf("the URL is %s, want a loopback address with a real port", base)
	}
	if cfgs := e.proxy.configs(); len(cfgs) != 1 || cfgs[0].Listen != hostOf(base) {
		t.Errorf("the proxy got %+v, want Listen %s", cfgs, hostOf(base))
	}
}

// TestUIPrintsOneObjectWithJSONAndYAML prints the keys cluster, url and expires once on stdout, in whole seconds of
// UTC, and still tells the notice on stderr.
func TestUIPrintsOneObjectWithJSONAndYAML(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			e := newUIEnv(t)
			r := e.start("-o", format)
			base := r.ready()

			got := r.stop()

			var obj map[string]any
			if err := yaml.Unmarshal([]byte(got.out), &obj); err != nil {
				t.Fatalf("stdout is not %s: %v\n%s", format, err, got.out)
			}
			if diff := cmp.Diff([]string{"cluster", "expires", "url"}, slices.Sorted(maps.Keys(obj))); diff != "" {
				t.Errorf("the keys (-want +got):\n%s", diff)
			}
			if obj["cluster"] != "prod" || obj["url"] != base {
				t.Errorf("cluster and url are %v and %v, want prod and %s", obj["cluster"], obj["url"], base)
			}
			expires, _ := obj["expires"].(string)
			ends, err := time.Parse(time.RFC3339, expires)
			if err != nil || !strings.HasSuffix(expires, "Z") || ends.Nanosecond() != 0 {
				t.Errorf("expires is %q, want RFC 3339 in UTC in whole seconds (%v)", expires, err)
			}
			notice := "the Nomad CLI works through it with NOMAD_ADDR=" + strings.TrimSuffix(base, "/ui/") + "; "
			if !strings.Contains(got.errOut, notice) {
				t.Errorf("stderr = %q, want the notice", got.errOut)
			}
		})
	}
}

// TestUISessionEndsWhenItsTimeComes stops like Ctrl-C when the end of the session comes, with exit code 0, the line
// that says so after the two lines, and the port closed.
func TestUISessionEndsWhenItsTimeComes(t *testing.T) {
	e := newUIEnv(t)
	var late atomic.Bool
	fire := make(chan time.Time)
	e.ui.now = func() time.Time {
		if late.Load() {
			return time.Now().Add(48 * time.Hour)
		}
		return time.Now()
	}
	e.ui.after = func(time.Duration) <-chan time.Time { return fire }
	r := e.start()
	base := r.ready()
	if ans := uiGet(t, base, ""); ans.err != nil || ans.status != http.StatusOK {
		t.Errorf("a request before the end got %+v, want 200", ans)
	}

	late.Store(true)
	close(fire)
	got := r.exited()

	if got.code != 0 {
		t.Errorf("exit code %d, want 0\n%s", got.code, got.errOut)
	}
	const line = "the session of tent ui ended: its certificate and token expired; run it again\n"
	if !strings.HasSuffix(got.errOut, "UTC\n"+line) {
		t.Errorf("stderr = %q, want the notice and then %q", got.errOut, line)
	}
	wantRefused(t, hostOf(base))
}

// TestUICtrlCSaysNothingMoreThanTheTwoLines leaves stderr as the two lines were: the line about the session's end is
// for the end alone.
func TestUICtrlCSaysNothingMoreThanTheTwoLines(t *testing.T) {
	e := newUIEnv(t)
	r := e.start()
	r.ready()

	got := r.stop()

	if strings.Contains(got.errOut, "ended") {
		t.Errorf("stderr = %q, want no line about the session's end", got.errOut)
	}
}

// pastToken is a Nomad API whose tokens ended a minute ago.
type pastToken struct{ nomadops.API }

func (a pastToken) CreateToken(ctx context.Context, req nomadops.TokenRequest) (nomadops.Token, error) {
	tok, err := a.API.CreateToken(ctx, req)
	tok.Expires = time.Now().Add(-time.Minute)
	return tok, err
}

// TestUISessionEndsByItsOwnTimer ends by the timer that the command sets when no test replaces it: the token's end
// has gone, so the session ends at once.
func TestUISessionEndsByItsOwnTimer(t *testing.T) {
	e := newUIEnv(t)
	inner := e.factory
	e.factory = func(cfg nomadops.Config) (nomadops.API, error) {
		api, err := inner(cfg)
		return pastToken{api}, err
	}
	r := e.start()

	got := r.exited()

	const line = "the session of tent ui ended: its certificate and token expired; run it again\n"
	if got.code != 0 || !strings.HasSuffix(got.errOut, "UTC\n"+line) {
		t.Errorf("exit code %d, stderr %q, want 0 and the line of the session's end", got.code, got.errOut)
	}
}

// TestUISessionEndsAtTheNextClockCheck ends within one check after the wall clock passes the end, while the wait of
// the whole time has not fired: a timer does not count the time that the machine sleeps.
func TestUISessionEndsAtTheNextClockCheck(t *testing.T) {
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	var wall atomic.Pointer[time.Time]
	setWall := func(w time.Time) { wall.Store(&w) }
	setWall(until.Add(-24 * time.Hour))
	waits := make(chan time.Duration, 4)
	fire := make(chan time.Time)
	clock := uiClock{
		now: func() time.Time { return *wall.Load() },
		after: func(d time.Duration) <-chan time.Time {
			waits <- d
			return fire
		},
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		ended bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		srv := newUIServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
		ended, err := serveUntil(t.Context(), srv, ln, until, clock, time.Second)
		done <- result{ended, err}
	}()

	select {
	case d := <-waits:
		if d != time.Minute {
			t.Fatalf("the first wait is %v, want one minute", d)
		}
	case <-time.After(uiGuard):
		t.Fatal("the session did not wait for its clock")
	}
	setWall(until.Add(time.Second))
	select {
	case fire <- time.Time{}:
	case <-time.After(uiGuard):
		t.Fatal("the session did not wait on its timer")
	}
	var got result
	select {
	case got = <-done:
	case <-time.After(uiGuard):
		t.Fatal("the session did not end at the next check of the clock")
	}

	if !got.ended || got.err != nil {
		t.Errorf("ended = %v, err = %v, want an end by the clock and no error", got.ended, got.err)
	}
}

// TestUISessionEndFollowsTheClock ends at the time it is given and not before, and at once for a time that has gone.
func TestUISessionEndFollowsTheClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		until := time.Now().Add(time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		end := sessionEnds(ctx, until, uiClock{})

		time.Sleep(time.Hour - time.Nanosecond)
		synctest.Wait()
		select {
		case <-end:
			t.Fatal("the session ended before its time")
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case <-end:
		default:
			t.Fatal("the session did not end at its time")
		}
		select {
		case <-sessionEnds(ctx, until.Add(-time.Minute), uiClock{}):
		case <-time.After(time.Second):
			t.Error("a session whose time has gone did not end at once")
		}
	})
}

// TestUILoopbackListen accepts the loopback addresses and refuses every other host, including another name, a mapped
// address, a zone, an empty host and anything that is not host:port with a port number.
func TestUILoopbackListen(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:4646", "127.0.0.1:0", "127.1.2.3:80", "[::1]:4646", "[::1]:0", "localhost:4646",
		"LocalHost:0", "127.0.0.1:65535"} {
		if err := checkLoopbackListen(ok); err != nil {
			t.Errorf("checkLoopbackListen(%q) = %v, want nil", ok, err)
		}
	}
	for _, tc := range []struct{ listen, want string }{
		{"0.0.0.0:4646", "--listen 0.0.0.0:4646: tent ui listens on a loopback address only"},
		{"192.0.2.1:80", "--listen 192.0.2.1:80: tent ui listens on a loopback address only"},
		{"[::]:4646", "--listen [::]:4646: tent ui listens on a loopback address only"},
		{":4646", "--listen :4646: tent ui listens on a loopback address only"},
		{"example.com:80", "--listen example.com:80: tent ui listens on a loopback address only"},
		{"localhost.:80", "--listen localhost.:80: tent ui listens on a loopback address only"},
		{"127.1:80", "--listen 127.1:80: tent ui listens on a loopback address only"},
		{"[::ffff:127.0.0.1]:80", "--listen [::ffff:127.0.0.1]:80: tent ui listens on a loopback address only"},
		{"[fe80::1%lo0]:80", "--listen [fe80::1%lo0]:80: tent ui listens on a loopback address only"},
		{"[::1%lo0]:80", "--listen [::1%lo0]:80: tent ui listens on a loopback address only"},
		{"nonsense", "--listen nonsense: want host:port, such as 127.0.0.1:4646"},
		{"", "--listen : want host:port, such as 127.0.0.1:4646"},
		{"127.0.0.1", "--listen 127.0.0.1: want host:port, such as 127.0.0.1:4646"},
		{"127.0.0.1:http", "--listen 127.0.0.1:http: the port must be a number from 0 to 65535"},
		{"127.0.0.1:65536", "--listen 127.0.0.1:65536: the port must be a number from 0 to 65535"},
		{"127.0.0.1:-1", "--listen 127.0.0.1:-1: the port must be a number from 0 to 65535"},
		{"127.0.0.1:", "--listen 127.0.0.1:: the port must be a number from 0 to 65535"},
	} {
		err := checkLoopbackListen(tc.listen)
		if err == nil || err.Error() != tc.want {
			t.Errorf("checkLoopbackListen(%q) = %v, want %q", tc.listen, err, tc.want)
		}
	}
}

// TestUIRefusesAnAddressBeforeAnyCall stops at once for an address that is not loopback or not host:port, before it
// opens a listener, asks Nomad or builds the proxy.
func TestUIRefusesAnAddressBeforeAnyCall(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{"0.0.0.0:4646", "Error: --listen 0.0.0.0:4646: tent ui listens on a loopback address only\n"},
		{"192.0.2.1:80", "Error: --listen 192.0.2.1:80: tent ui listens on a loopback address only\n"},
		{"nonsense", "Error: --listen nonsense: want host:port, such as 127.0.0.1:4646\n"},
	} {
		t.Run(tc.listen, func(t *testing.T) {
			e := newUIEnv(t)
			e.ui.listen = func(context.Context, string) (net.Listener, error) {
				t.Error("tent ui opened a listener")
				return nil, errors.New("not expected")
			}
			e.noProxy = true // the check of --listen comes before the one for the proxy
			r := e.startArgs("ui", "prod", "--state", e.s.url, "--listen", tc.listen)

			got := r.exited()

			wantError(t, got, tc.want)
			e.wantNoAccess()
		})
	}
}

// TestUIChecksTheAddressBeforeTheName refuses a bad address with no cluster name given, before it says that a name
// is missing.
func TestUIChecksTheAddressBeforeTheName(t *testing.T) {
	e := newUIEnv(t)
	r := e.startArgs("ui", "--state", e.s.url, "--listen", "0.0.0.0:4646")

	got := r.exited()

	wantError(t, got, "Error: --listen 0.0.0.0:4646: tent ui listens on a loopback address only\n")
}

// TestUIPrintsTheEndInWholeSeconds cuts the fraction of a second off the end of the session, which is the token's when
// that ends before the certificate.
func TestUIPrintsTheEndInWholeSeconds(t *testing.T) {
	e := newUIEnv(t)
	inner := e.factory
	e.factory = func(cfg nomadops.Config) (nomadops.API, error) {
		api, err := inner(cfg)
		return earlyToken{api}, err
	}
	r := e.start("-o", "json")
	r.ready()

	got := r.stop()

	var obj struct{ Expires string }
	if err := json.Unmarshal([]byte(got.out), &obj); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, got.out)
	}
	ends, err := time.Parse(time.RFC3339, obj.Expires)
	if err != nil || ends.Nanosecond() != 0 || strings.Contains(obj.Expires, ".") {
		t.Errorf("expires is %q, want RFC 3339 in whole seconds (%v)", obj.Expires, err)
	}
	if want := "the session ends at " + ends.UTC().Format(noticeTime) + "\n"; !strings.Contains(got.errOut, want) {
		t.Errorf("stderr = %q, want %q", got.errOut, want)
	}
}

// TestUIRefusesAListenerThatIsNotLoopback closes a listener whose real address is not loopback, as localhost may be
// resolved to another, and fails before it asks Nomad for anything.
func TestUIRefusesAListenerThatIsNotLoopback(t *testing.T) {
	for name, addr := range map[string]net.Addr{
		"another address": &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 4646},
		"no TCP address":  &net.UnixAddr{Name: "ui.sock", Net: "unix"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUIEnv(t)
			e.watch()
			e.reportAs = addr
			r := e.startArgs("ui", "prod", "--state", e.s.url, "--listen", "localhost:0")

			got := r.exited()

			wantError(t, got, "Error: --listen localhost:0: tent ui listens on a loopback address only\n")
			select {
			case <-e.watched.Load().closed:
			default:
				t.Error("the listener was not closed")
			}
			e.wantNoAccess()
		})
	}
}

// TestUIFailsOnABusyPortWithoutAToken says what the system said, and the way out, and asks Nomad for nothing.
func TestUIFailsOnABusyPortWithoutAToken(t *testing.T) {
	e := newUIEnv(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	address := busy.Addr().String()
	_, again := net.Listen("tcp", address)
	opErr, ok := errors.AsType[*net.OpError](again)
	if !ok {
		t.Fatalf("a second listen on %s gave %v, want an *net.OpError", address, again)
	}
	r := e.startArgs("ui", "prod", "--state", e.s.url, "--listen", address)

	got := r.exited()

	wantError(t, got, "Error: listen on "+address+": "+opErr.Err.Error()+"; pick another address with --listen\n")
	e.wantNoAccess()
}

// TestUIClosesTheListenerWhenAccessFails leaves no open port when the operator's access cannot be made: here the
// cluster has no secrets yet.
func TestUIClosesTheListenerWhenAccessFails(t *testing.T) {
	e := newUIEnv(t)
	e.s = withCluster(t) // specs only
	e.watch()
	r := e.start()

	got := r.exited()

	if got.code != 1 || !strings.Contains(got.errOut, "run tent update cluster --yes first") || got.out != "" {
		t.Errorf("exit code %d, stdout %q, stderr %q, want an error that says to run update", got.code, got.out, got.errOut)
	}
	wantRefused(t, e.watched.Load().Listener.Addr().String())
	if cfgs := e.proxy.configs(); len(cfgs) != 0 {
		t.Errorf("the proxy factory got %d configs", len(cfgs))
	}
}

// TestUIClosesTheListenerWhenTheProxyFails ends with the factory's error and no open port.
func TestUIClosesTheListenerWhenTheProxyFails(t *testing.T) {
	e := newUIEnv(t)
	e.proxy.err = errors.New("no good")
	e.watch()
	r := e.start()

	got := r.exited()

	wantError(t, got, "Error: set up the proxy: no good\n")
	wantRefused(t, e.watched.Load().Listener.Addr().String())
}

// TestUIEndsAsInterruptedBeforeTheServerStarts says interrupted, with exit code 1, prints no line and leaves no open
// port when the command's context ends after the proxy is built and before the server starts.
func TestUIEndsAsInterruptedBeforeTheServerStarts(t *testing.T) {
	e := newUIEnv(t)
	e.watch()
	started := make(chan *uiRun, 1)
	e.proxy.onBuild = func() { (<-started).cancel() }
	r := e.start()
	started <- r

	got := r.exited()

	wantError(t, got, "Error: interrupted\n")
	wantRefused(t, e.watched.Load().Listener.Addr().String())
}

// TestUIFailsWithoutAProxy fails with the sentence of a missing Nomad factory's kind, before it opens a listener or
// asks Nomad for a token.
func TestUIFailsWithoutAProxy(t *testing.T) {
	e := newUIEnv(t)
	e.noProxy = true
	e.ui.listen = func(context.Context, string) (net.Listener, error) {
		t.Error("tent ui opened a listener")
		return nil, errors.New("not expected")
	}
	r := e.start()

	got := r.exited()

	wantError(t, got, "Error: no Nomad proxy is set up\n")
	e.wantNoAccess()
}

// TestUIOptionGivesTheProxyFactory builds the proxy with the factory that WithNomadProxy gives.
func TestUIOptionGivesTheProxyFactory(t *testing.T) {
	e := newUIEnv(t)
	var out, errOut syncBuffer
	proxy := func(nomadops.ProxyConfig) (http.Handler, error) { return nil, errors.New("no good") }

	code := executeTest(t.Context(), t, []string{"ui", "prod", "--state", e.s.url, "--listen", "127.0.0.1:0"},
		Streams{In: strings.NewReader(""), Out: &out, Err: &errOut},
		WithProviders(onVultr(e.f)), WithAssets(testAssets()), WithNomad(e.factory), WithNomadProxy(proxy))

	wantError(t, result{code, out.String(), errOut.String()}, "Error: set up the proxy: no good\n")
}

// TestUIFailsWithoutAClusterName tells how to name the cluster, before it opens a listener.
func TestUIFailsWithoutAClusterName(t *testing.T) {
	e := newUIEnv(t)
	e.ui.listen = func(context.Context, string) (net.Listener, error) {
		t.Error("tent ui opened a listener")
		return nil, errors.New("not expected")
	}
	r := e.startArgs("ui", "--state", e.s.url)

	got := r.exited()

	const want = "Error: no cluster name: give NAME or set --name"
	if got.code != 1 || got.out != "" || !strings.HasPrefix(got.errOut, want) {
		t.Errorf("exit code %d, stdout %q, stderr %q, want the error for a missing name", got.code, got.out, got.errOut)
	}
	e.wantNoAccess()
}

// TestUIFailsWhenTheServerFails ends with exit code 1 and the error of the server, after the two lines, and closes the
// listener.
func TestUIFailsWhenTheServerFails(t *testing.T) {
	e := newUIEnv(t)
	e.acceptErr = errors.New("accept failed")
	e.watch()
	r := e.start()

	got := r.exited()

	address := e.watched.Load().Listener.Addr().String()
	if got.code != 1 || !strings.HasSuffix(got.errOut, "UTC\nError: serve on "+address+": accept failed\n") {
		t.Errorf("exit code %d, stderr %q, want the notice and then the server's error", got.code, got.errOut)
	}
	select {
	case <-e.watched.Load().closed:
	default:
		t.Error("the listener was not closed")
	}
}

// TestUIFinishesAnOpenRequestAtTheEnd stops accepting at once and still answers a request that is open, then ends with
// code 0.
func TestUIFinishesAnOpenRequestAtTheEnd(t *testing.T) {
	e := newUIEnv(t)
	started, release := make(chan struct{}), make(chan struct{})
	e.proxy.handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	e.watch()
	r := e.start()
	base := r.ready()
	answer := make(chan uiAnswer, 1)
	go func() { answer <- uiGet(t, base, "") }()
	<-started

	r.cancel()
	select {
	case <-e.watched.Load().closed:
	case <-time.After(uiGuard):
		t.Fatal("the listener was not closed at the end")
	}
	close(release)

	if got := <-answer; got.err != nil || got.status != http.StatusOK || got.body != "done" {
		t.Errorf("the open request got %+v, want 200 and its whole answer", got)
	}
	if got := r.exited(); got.code != 0 {
		t.Errorf("exit code %d, want 0\n%s", got.code, got.errOut)
	}
}

// TestUIClosesWhatStaysOpenAfterTheLimit ends with code 0 after the limit, when a request does not finish, and closes
// its connection.
func TestUIClosesWhatStaysOpenAfterTheLimit(t *testing.T) {
	e := newUIEnv(t)
	e.ui.grace = 50 * time.Millisecond
	started, ended := make(chan struct{}), make(chan struct{})
	e.proxy.handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(ended)
	})
	r := e.start()
	base := r.ready()
	answer := make(chan uiAnswer, 1)
	go func() { answer <- uiGet(t, base, "") }()
	<-started

	r.cancel()

	select {
	case <-ended: // within the limit that the test set, which is far below the real one
	case <-time.After(uiGuard / 5):
		t.Error("the open request was not closed within the limit")
	}
	if got := r.exited(); got.code != 0 {
		t.Errorf("exit code %d, want 0\n%s", got.code, got.errOut)
	}
	if got := <-answer; got.err == nil && got.status == http.StatusOK {
		t.Errorf("the open request got %+v, want its connection closed", got)
	}
}

// TestUIWaitsFiveSecondsForOpenRequests waits 5 seconds at the end, unless a test sets another limit.
func TestUIWaitsFiveSecondsForOpenRequests(t *testing.T) {
	if got := (uiSettings{}).shutdownGrace(); got != 5*time.Second {
		t.Errorf("the default limit is %s, want 5s", got)
	}
	if got := (uiSettings{grace: time.Millisecond}).shutdownGrace(); got != time.Millisecond {
		t.Errorf("the limit that a test set is %s, want 1ms", got)
	}
}

// TestUIServerLimits gives the server a limit for the headers of a request and none that would cut a stream, and
// sends the server's own error lines to the log as warnings.
func TestUIServerLimits(t *testing.T) {
	var logged strings.Builder
	srv := newUIServer(http.NotFoundHandler(), slog.New(slog.NewTextHandler(&logged, nil)))

	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 || srv.IdleTimeout != 0 {
		t.Errorf("ReadTimeout, WriteTimeout and IdleTimeout are %s, %s and %s, want none",
			srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
	if srv.Handler == nil {
		t.Error("the server has no handler")
	}
	srv.ErrorLog.Print("accept failed")
	if got := logged.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "accept failed") {
		t.Errorf("the server's own error line was logged as %q, want a warning with its text", got)
	}
}

// TestUIHidesTheSecretsAndLogsRequestLines runs with -v, and with -vv, and -o json against the real proxy: no stream
// shows the token, the key or the bootstrap secret, and the proxy's request lines show in the command's log, with the
// path and the status and nothing of the headers.
func TestUIHidesTheSecretsAndLogsRequestLines(t *testing.T) {
	for _, verbose := range []string{"-v", "-vv"} {
		t.Run(verbose, func(t *testing.T) {
			e := newUIEnv(t)
			e.proxy.inner = nomadops.NewProxy
			r := e.start(verbose, "-o", "json")
			base := r.ready()

			ans := uiGet(t, strings.TrimSuffix(base, "ui/")+"v1/jobs", "evil.test")
			got := r.stop()

			if ans.err != nil || ans.status != http.StatusForbidden {
				t.Fatalf("a request for another host got %+v, want 403", ans)
			}
			for _, want := range []string{`msg="proxy request"`, "method=GET", "path=/v1/jobs", "status=403"} {
				if !strings.Contains(got.errOut, want) {
					t.Errorf("stderr = %q, want %s", got.errOut, want)
				}
			}
			cfg := e.proxy.configs()[0]
			secrets := map[string][]byte{
				"token": cfg.Token.Bytes(), "key": cfg.Cert.Key.Bytes(),
				"bootstrap secret": []byte(e.s.objects(t)["prod/secrets/acl-bootstrap-token"]),
			}
			secrettest.CheckHidden(t, map[string]string{"stdout": got.out, "stderr": got.errOut}, secrets, "")
		})
	}
}

// TestUIHelpSaysWhatTheCommandDoes names in the long help the risk of the port, NOMAD_ADDR, the token's end and what
// the command needs.
func TestUIHelpSaysWhatTheCommandDoes(t *testing.T) {
	got := runIn(t, "", "ui", "--help")

	for _, want := range []string{
		"management token", "loopback address only", "NOMAD_ADDR", "24 hours", "is not deleted", "VULTR_API_KEY", "4646",
	} {
		if !strings.Contains(got.out, want) {
			t.Errorf("the help of tent ui lacks %q:\n%s", want, got.out)
		}
	}
}
