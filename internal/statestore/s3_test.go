package statestore_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/s3url/s3urltest"
	"github.com/ingvarch/tent/internal/statestore"
	"github.com/ingvarch/tent/internal/statestore/storetest"
)

// s3URLEnv names the bucket prefix that the tests against a real S3 server use. Its credentials come from the AWS
// environment, such as AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. Without it those tests skip.
const s3URLEnv = "TENT_TEST_S3_URL"

// fakeWriteInterval is the throttling fake's limit, one write per key per interval. It is Cloudflare R2's second,
// scaled down ten times together with the store's wait after a throttled request, so the tests see R2's behaviour
// in a tenth of the time.
const fakeWriteInterval = 100 * time.Millisecond

// The locker suite's timing: the lease lifetime, of which each renewal gets a third, and Acquire's wait between
// tries. Where one write per second reaches a key, a renewal must outlast a few throttled tries and the waits after
// them, and a waiting Acquire must try less often than the limit, or it takes every write and the holder cannot
// unlock. tent's own Acquire waits two seconds.
var (
	lockerOnBucket         = storetest.LockerOptions{TTL: 12 * time.Second, Retry: 2 * time.Second}
	lockerOnFake           = storetest.LockerOptions{TTL: 300 * time.Millisecond}
	lockerOnThrottlingFake = storetest.LockerOptions{TTL: 1500 * time.Millisecond, Retry: 2 * fakeWriteInterval}
)

func TestS3Store(t *testing.T) {
	storetest.Run(t, func(t *testing.T) statestore.Store { return openS3(t, s3TestURL(t)) })
}

func TestS3LeaseLocker(t *testing.T) {
	storetest.RunLocker(t, sharedStoreLockers(func(t *testing.T) statestore.Store {
		return openS3(t, s3TestURL(t))
	}), lockerOnBucket)
}

// sharedStoreLockers returns a LockerSetup whose lockers share one store per subtest. A store keeps nothing of its
// callers, so the lockers stay independent, and the store probes once.
func sharedStoreLockers(open func(t *testing.T) statestore.Store) storetest.LockerSetup {
	return func(t *testing.T, now func() time.Time) (string, func() statestore.Locker) {
		s, layout := open(t), mustLayout(t, "prod")
		return "prod", func() statestore.Locker { return statestore.NewLeaseLocker(s, layout, now) }
	}
}

func TestS3NewLocker(t *testing.T) {
	wantMechanism(t, openS3(t, s3TestURL(t)), statestore.MechanismConditionalPut)
}

func TestS3StoreOnFake(t *testing.T) {
	storetest.Run(t, func(t *testing.T) statestore.Store { return openS3(t, fakeURL(t, newFakeS3(t))) })
}

func TestS3StoreOnFakeWithoutConditionalPuts(t *testing.T) {
	storetest.Run(t, func(t *testing.T) statestore.Store {
		f := newFakeS3(t)
		f.set(func(f *fakeS3) { f.ignoreConditions = true })
		return openS3(t, fakeURL(t, f))
	})
}

func TestS3LeaseLockerOnFake(t *testing.T) {
	storetest.RunLocker(t, sharedStoreLockers(func(t *testing.T) statestore.Store {
		return openS3(t, fakeURL(t, newFakeS3(t)))
	}), lockerOnFake)
}

func TestS3StoreOnThrottlingFake(t *testing.T) {
	storetest.Run(t, func(t *testing.T) statestore.Store { return openS3(t, fakeURL(t, throttlingFake(t))) })
}

func TestS3LeaseLockerOnThrottlingFake(t *testing.T) {
	storetest.RunLocker(t, sharedStoreLockers(func(t *testing.T) statestore.Store {
		return openS3(t, fakeURL(t, throttlingFake(t)))
	}), lockerOnThrottlingFake)
}

// throttlingFake returns a fake that takes one write per fakeWriteInterval to a key, and makes stores wait as long
// after a throttled request.
func throttlingFake(t *testing.T) *fakeS3 {
	t.Helper()
	statestore.SetThrottleWait(t, fakeWriteInterval)
	f := newFakeS3(t)
	f.set(func(f *fakeS3) { f.writeInterval = fakeWriteInterval })
	return f
}

func TestS3NewLockerOnFake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ignore bool
		want   statestore.Mechanism
	}{
		{"conditional puts", false, statestore.MechanismConditionalPut},
		{"no conditional puts", true, statestore.MechanismBestEffort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			f.set(func(f *fakeS3) { f.ignoreConditions = tc.ignore })
			wantMechanism(t, openS3(t, fakeURL(t, f)), tc.want)
		})
	}
}

func wantMechanism(t *testing.T, s statestore.Store, want statestore.Mechanism) {
	t.Helper()
	_, got, err := statestore.NewLocker(t.Context(), s, mustLayout(t, "prod"), nil)
	if err != nil || got != want {
		t.Errorf("NewLocker = %q, %v; want %q", got, err, want)
	}
}

func TestOpenS3(t *testing.T) {
	s3urltest.IsolateAWS(t)
	for _, tc := range []struct{ name, url, want string }{
		{"bucket", "s3://tent-state?region=eu-central-1", "s3://tent-state"},
		{"trailing slash", "s3://tent-state/?region=eu-central-1", "s3://tent-state"},
		{"prefix", "s3://tent-state/teams/infra?region=eu-central-1", "s3://tent-state/teams/infra"},
		{"prefix with a trailing slash", "s3://tent-state/infra/?region=eu-central-1", "s3://tent-state/infra"},
		{
			"endpoint", "s3://tent-ci/ci?endpoint=https://acct.r2.cloudflarestorage.com&region=auto",
			"s3://tent-ci/ci?endpoint=https://acct.r2.cloudflarestorage.com",
		},
		{
			"endpoint with a slash", "s3://tent/prod?region=fsn1&endpoint=https://fsn1.your-objectstorage.com/",
			"s3://tent/prod?endpoint=https://fsn1.your-objectstorage.com",
		},
		{
			"escaped endpoint", "s3://tent/prod?endpoint=https%3A%2F%2Fams1.vultrobjects.com&region=ams1",
			"s3://tent/prod?endpoint=https://ams1.vultrobjects.com",
		},
		{
			"path style", "s3://tent/dev?endpoint=http://localhost:7070&region=us-east-1&pathStyle=true",
			"s3://tent/dev?endpoint=http://localhost:7070",
		},
		{"virtual-host style", "s3://tent/dev?region=us-east-1&pathStyle=false", "s3://tent/dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := statestore.Open(t.Context(), tc.url)
			if err != nil {
				t.Fatalf("Open(%q): %v", tc.url, err)
			}
			if got := s.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOpenS3RegionFromTheEnvironment(t *testing.T) {
	s3urltest.IsolateAWS(t)
	t.Setenv("AWS_REGION", "eu-central-1")
	if _, err := statestore.Open(t.Context(), "s3://tent-state/prod"); err != nil {
		t.Errorf("Open without a region in the URL but with AWS_REGION: %v", err)
	}
}

func TestOpenS3Rejects(t *testing.T) {
	s3urltest.IsolateAWS(t)
	testRejects(t, []rejectCase{
		{"empty", "s3://", "name a bucket"},
		{"no bucket", "s3:///state?region=auto", "name a bucket"},
		{"opaque", "s3:tent-state", "name a bucket"},
		{"port", "s3://tent-state:9000/p?region=auto", "no port"},
		{"fragment", "s3://tent-state/p?region=auto#top", "fragment"},
		{"empty segment", "s3://tent-state//p?region=auto", "invalid prefix"},
		{"dot segment", "s3://tent-state/a/../b?region=auto", "invalid prefix"},
		{"reserved name", "s3://tent-state/.tent?region=auto", "invalid prefix"},
		{"space", "s3://tent-state/my%20state?region=auto", "invalid prefix"},
		{"unknown parameter", "s3://tent-state?region=auto&profile=x", "unknown query parameter"},
		{"parameter case", "s3://tent-state?Region=auto", "unknown query parameter"},
		{"repeated parameter", "s3://tent-state?region=auto&region=eu", "region is given more than once"},
		{"empty region", "s3://tent-state?region=", "region is empty"},
		{"no region", "s3://tent-state/p", "no region"},
		{"endpoint without a scheme", "s3://t?region=fsn1&endpoint=fsn1.your-objectstorage.com", "endpoint must"},
		{"endpoint scheme", "s3://t?region=auto&endpoint=ftp://example.com", "endpoint must"},
		{"endpoint without a host", "s3://t?region=auto&endpoint=https://", "endpoint must"},
		{"endpoint path", "s3://t?region=auto&endpoint=https://example.com/tent-state", "endpoint must"},
		{"endpoint query", "s3://t?region=auto&endpoint=https://example.com?x=1", "endpoint must"},
		{"empty endpoint", "s3://t?region=auto&endpoint=", "endpoint must"},
		{
			"endpoint with credentials", "s3://t?region=auto&endpoint=https%3A%2F%2Fkey%3Asecret%40example.com",
			"credentials come from the environment",
		},
		{"pathStyle", "s3://t?region=auto&pathStyle=yes", "pathStyle must be true or false"},
		{"bad escape", "s3://t?region=auto&x=%zz", "invalid URL escape"},
		{"semicolon", "s3://t?region=auto;pathStyle=true", "semicolon"},
	})
}

func TestS3ListHidesInternalNames(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ignorePrefix bool
	}{
		{"server lists the prefix", false},
		{"server lists everything", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			f.set(func(f *fakeS3) { f.ignoreListPrefix = tc.ignorePrefix })
			s := openS3(t, fakeURL(t, f))
			// Objects of the store, names that are not paths, what a failed probe leaves, and objects outside the
			// prefix.
			f.put("state/a/b", "state/z", "state/.tent-probe/x", "state/.hidden", "state/bad name", "state/a//c",
				"state/a/", "state/é", "state", "stateful/x", "other/state/y", "a")
			for _, tc := range []struct {
				prefix string
				want   []string
			}{
				{"", []string{"a/b", "z"}},
				{"a", []string{"a/b"}},
				{"a/", []string{"a/b"}},
				{"b", nil},
			} {
				got, err := s.List(t.Context(), tc.prefix)
				if err != nil {
					t.Fatalf("List(%q): %v", tc.prefix, err)
				}
				if !slices.Equal(got, tc.want) {
					t.Errorf("List(%q) = %q, want %q", tc.prefix, got, tc.want)
				}
			}
		})
	}
}

func TestS3ConditionalPutErrors(t *testing.T) {
	plain := func(statestore.Version) statestore.PutOptions { return statestore.PutOptions{} }
	create := func(statestore.Version) statestore.PutOptions { return statestore.PutOptions{IfNoneMatch: true} }
	replace := func(v statestore.Version) statestore.PutOptions { return statestore.PutOptions{IfMatch: v} }
	for _, tc := range []struct {
		name   string
		opts   func(statestore.Version) statestore.PutOptions
		answer fakeError
		want   error // nil: neither ErrPreconditionFailed nor errors.ErrUnsupported
	}{
		{"412 on a create", create, fakeError{412, "PreconditionFailed"}, statestore.ErrPreconditionFailed},
		{"412 on a replace", replace, fakeError{412, "PreconditionFailed"}, statestore.ErrPreconditionFailed},
		{"409 of AWS S3", create, fakeError{409, "ConditionalRequestConflict"}, statestore.ErrPreconditionFailed},
		{"409 of Ceph RGW", replace, fakeError{409, "ConcurrentModification"}, statestore.ErrPreconditionFailed},
		{"409 of another kind", replace, fakeError{409, "OperationAborted"}, statestore.ErrPreconditionFailed},
		{"404 on a replace", replace, fakeError{404, "NoSuchKey"}, statestore.ErrPreconditionFailed},
		{"501 on a create", create, fakeError{501, "NotImplemented"}, errors.ErrUnsupported},
		{"501 on a replace", replace, fakeError{501, "NotImplemented"}, errors.ErrUnsupported},
		{"404 on a create", create, fakeError{404, "NoSuchKey"}, nil},
		{"missing bucket on a replace", replace, fakeError{404, "NoSuchBucket"}, nil},
		{"access denied on a replace", replace, fakeError{403, "AccessDenied"}, nil},
		{"412 on a plain put", plain, fakeError{412, "PreconditionFailed"}, nil},
		{"409 on a plain put", plain, fakeError{409, "OperationAborted"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			s := openS3(t, fakeURL(t, f))
			v := mustPutS3(t, s, "a", "one")
			wantConditionalPut(t, s, true)
			f.set(func(f *fakeS3) { f.intercept = answerPuts("state/a", tc.answer) })
			_, err := s.Put(t.Context(), "a", []byte("two"), tc.opts(v))
			switch {
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("Put error = %v, want %v", err, tc.want)
			case tc.want == nil && (err == nil || errors.Is(err, statestore.ErrPreconditionFailed) ||
				errors.Is(err, errors.ErrUnsupported)):
				t.Errorf("Put error = %v, want another error", err)
			}
		})
	}
}

// TestS3ConditionalPutsAreNotRetried answers a conditional put that the server carried out with a server error, as
// when the response is lost. A retry would find its own write and report ErrPreconditionFailed, a lost race.
func TestS3ConditionalPutsAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool // IfMatch; otherwise IfNoneMatch
	}{
		{"create", false},
		{"replace", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			s := openS3(t, fakeURL(t, f))
			wantConditionalPut(t, s, true)
			opts := statestore.PutOptions{IfNoneMatch: !tc.replace}
			if tc.replace {
				opts.IfMatch = mustPutS3(t, s, "a", "one")
			}
			before := f.count(http.MethodPut, "state/a")
			f.set(func(f *fakeS3) {
				f.intercept = func(r *http.Request, key string, body []byte) *fakeError {
					if r.Method != http.MethodPut || key != "state/a" {
						return nil
					}
					f.objects["state/a"] = body // the write happens, and its answer is lost
					return &fakeError{http.StatusInternalServerError, "InternalError"}
				}
			})
			_, err := s.Put(t.Context(), "a", []byte("two"), opts)
			if err == nil || errors.Is(err, statestore.ErrPreconditionFailed) {
				t.Errorf("Put error = %v, want the server error", err)
			}
			if n := f.count(http.MethodPut, "state/a") - before; n != 1 {
				t.Errorf("the conditional put was sent %d times, want once", n)
			}
		})
	}
}

// TestS3ThrottledRequestsAreRetried answers a request twice with a throttling error, which says the server did not
// carry it out, as Cloudflare R2 does with more than one write per second to a key.
func TestS3ThrottledRequestsAreRetried(t *testing.T) {
	tooMany := fakeError{http.StatusTooManyRequests, "TooManyRequests"}
	slowDown := fakeError{http.StatusServiceUnavailable, "SlowDown"}
	put := func(opts statestore.PutOptions) func(ctx context.Context, s statestore.Store) error {
		return func(ctx context.Context, s statestore.Store) error {
			_, err := s.Put(ctx, "a", []byte("new"), opts)
			return err
		}
	}
	get := func(ctx context.Context, s statestore.Store) error { _, _, err := s.Get(ctx, "a"); return err }
	list := func(ctx context.Context, s statestore.Store) error { _, err := s.List(ctx, ""); return err }
	del := func(ctx context.Context, s statestore.Store) error { return s.Delete(ctx, "a") }
	for _, tc := range []struct {
		name   string
		method string
		key    string // "" for List
		answer fakeError
		exists bool
		call   func(ctx context.Context, s statestore.Store) error
		want   error // nil: success
	}{
		{"create", http.MethodPut, "state/a", tooMany, false, put(statestore.PutOptions{IfNoneMatch: true}), nil},
		{
			"create of an existing object", http.MethodPut, "state/a", tooMany, true,
			put(statestore.PutOptions{IfNoneMatch: true}), statestore.ErrPreconditionFailed,
		},
		{"replace", http.MethodPut, "state/a", slowDown, true, put(statestore.PutOptions{IfMatch: etagOfOld}), nil},
		{"plain put", http.MethodPut, "state/a", tooMany, false, put(statestore.PutOptions{}), nil},
		{"get", http.MethodGet, "state/a", tooMany, true, get, nil},
		{"list", http.MethodGet, "", tooMany, true, list, nil},
		{"delete", http.MethodDelete, "state/a", tooMany, true, del, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statestore.SetThrottleWait(t, time.Millisecond)
			f := newFakeS3(t)
			s := openS3(t, fakeURL(t, f))
			wantConditionalPut(t, s, true)
			if tc.exists {
				mustPutS3(t, s, "a", "old")
			}
			throttled := 0
			f.set(func(f *fakeS3) {
				f.intercept = func(r *http.Request, key string, _ []byte) *fakeError {
					if r.Method != tc.method || key != tc.key || throttled == 2 {
						return nil
					}
					throttled++
					return &tc.answer
				}
			})
			before := f.count(tc.method, tc.key)
			err := tc.call(t.Context(), s)
			if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
			if n := f.count(tc.method, tc.key) - before; n != 3 {
				t.Errorf("the request was sent %d times, want 3: twice throttled, then carried out", n)
			}
		})
	}
}

// etagOfOld is the version of the object "old" in the fake.
var etagOfOld = statestore.Version(etagOf([]byte("old")))

func TestS3ThrottlingGivesUp(t *testing.T) {
	statestore.SetThrottleWait(t, time.Millisecond)
	f := newFakeS3(t)
	s := openS3(t, fakeURL(t, f))
	wantConditionalPut(t, s, true)
	f.set(func(f *fakeS3) {
		f.intercept = answerPuts("state/a", fakeError{http.StatusTooManyRequests, "TooManyRequests"})
	})
	_, err := s.Put(t.Context(), "a", []byte("x"), statestore.PutOptions{IfNoneMatch: true})
	if err == nil || errors.Is(err, statestore.ErrPreconditionFailed) || !strings.Contains(err.Error(), "429") {
		t.Errorf("Put error = %v, want the throttling error", err)
	}
	if n := f.count(http.MethodPut, "state/a"); n != statestore.ThrottleAttempts {
		t.Errorf("the put was sent %d times, want %d", n, statestore.ThrottleAttempts)
	}
}

func TestS3ThrottleWaitEndsWithTheContext(t *testing.T) {
	f := newFakeS3(t)
	s := openS3(t, fakeURL(t, f))
	f.set(func(f *fakeS3) {
		f.intercept = answerPuts("state/a", fakeError{http.StatusTooManyRequests, "TooManyRequests"})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Put(ctx, "a", []byte("x"), statestore.PutOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Put error = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("Put returned %v after its context ended, want at once", d)
	}
}

func TestS3AddressingStyle(t *testing.T) {
	for _, tc := range []struct {
		pathStyle string
		host      string // before the port
	}{
		{"true", "localhost"},
		{"false", fakeBucket + ".localhost"},
	} {
		t.Run("pathStyle="+tc.pathStyle, func(t *testing.T) {
			if _, err := net.LookupHost(tc.host); err != nil {
				t.Skipf("%s does not resolve here: %v", tc.host, err)
			}
			f := newFakeS3(t)
			s3urltest.IsolateAWS(t)
			endpoint := strings.Replace(f.url, "127.0.0.1", "localhost", 1) // an IP address is always path-style
			s := openS3(t, "s3://"+fakeBucket+"/state?endpoint="+endpoint+"&region=us-east-1&pathStyle="+tc.pathStyle)
			mustPutS3(t, s, "a", "x")
			_, port, _ := strings.Cut(strings.TrimPrefix(f.url, "http://"), ":")
			if got, want := f.hostLog(), []string{tc.host + ":" + port}; !slices.Equal(got, want) {
				t.Errorf("the requests went to %q, want %q", got, want)
			}
			if got, want := f.keys(), []string{"state/a"}; !slices.Equal(got, want) {
				t.Errorf("the bucket holds %q, want %q", got, want)
			}
		})
	}
}

// TestS3WritesNothingToStderr uses a server whose answers make the AWS SDK log: a Date header that does not parse.
// The fake also refuses GET requests that ask for checksums, which the SDK asks for by default.
func TestS3WritesNothingToStderr(t *testing.T) {
	f := newFakeS3(t)
	f.set(func(f *fakeS3) { f.badDate = true })
	u := fakeURL(t, f)
	out := captureStderr(t, func() {
		s := openS3(t, u)
		v := mustPutS3(t, s, "a", "one")
		if _, err := s.Put(t.Context(), "a", []byte("two"), statestore.PutOptions{IfMatch: v}); err != nil {
			t.Errorf("Put: %v", err)
		}
		if _, _, err := s.Get(t.Context(), "a"); err != nil {
			t.Errorf("Get: %v", err)
		}
		if _, err := s.List(t.Context(), ""); err != nil {
			t.Errorf("List: %v", err)
		}
		if err := s.Delete(t.Context(), "a"); err != nil {
			t.Errorf("Delete: %v", err)
		}
	})
	if out != "" {
		t.Errorf("the store wrote to stderr:\n%s", out)
	}
}

// captureStderr runs fn with os.Stderr going to a pipe, and returns what fn wrote there.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r) // ends when w closes
		read <- string(data)
	}()
	stderr := os.Stderr
	os.Stderr = w
	func() {
		defer func() { os.Stderr = stderr }()
		fn()
	}()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return <-read
}

func TestS3ProbeDeletesItsObjectWhenCanceled(t *testing.T) {
	f := newFakeS3(t)
	s := openS3(t, fakeURL(t, f))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The caller gives up while the second create is on its way.
	f.set(func(f *fakeS3) {
		creates := answerCreate(2, fakeError{http.StatusInternalServerError, "InternalError"})
		f.intercept = func(r *http.Request, key string, body []byte) *fakeError {
			e := creates(r, key, body)
			if e != nil {
				cancel()
			}
			return e
		}
	})
	if caps, err := s.Capabilities(ctx); err == nil {
		t.Errorf("Capabilities = %+v, want an error", caps)
	}
	if keys := f.keys(); len(keys) != 0 {
		t.Errorf("the probe left %q", keys)
	}
}

// offlineClient is an HTTP client for stores on real hosts: it sends nothing, fails every request, and counts them.
type offlineClient struct{ calls atomic.Int32 }

func (c *offlineClient) Do(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	// A host that is not found, which the SDK does not retry.
	why := "offlineClient: a test tried to send " + r.Method + " here"
	return nil, &net.DNSError{Err: why, Name: r.URL.Hostname(), IsNotFound: true}
}

// TestS3HetznerTakesNoConditionalPuts checks that a store on Hetzner Object Storage reports no conditional puts
// without asking the server, and refuses them.
func TestS3HetznerTakesNoConditionalPuts(t *testing.T) {
	s3urltest.IsolateAWS(t)
	for _, tc := range []struct{ name, endpoint string }{
		{"location", "https://fsn1.your-objectstorage.com"},
		{"domain", "https://your-objectstorage.com"},
		{"upper case and port", "https://NBG1.Your-ObjectStorage.COM:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openS3(t, "s3://tent-state/prod?region=fsn1&endpoint="+tc.endpoint)
			offline := &offlineClient{}
			statestore.SetS3HTTPClient(s, offline) // the host is real: nothing may reach it
			caps, err := s.Capabilities(t.Context())
			if err != nil || caps.ConditionalPut {
				t.Errorf("Capabilities = %+v, %v; want no conditional puts", caps, err)
			}
			_, err = s.Put(t.Context(), "a", []byte("x"), statestore.PutOptions{IfNoneMatch: true})
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("create-only Put: error = %v, want errors.ErrUnsupported", err)
			}
			if n := offline.calls.Load(); n != 0 {
				t.Errorf("the store sent %d requests, want none", n)
			}
		})
	}
	// Other domains that end in the same letters are probed.
	t.Run("look-alike domain", func(t *testing.T) {
		s := openS3(t, "s3://tent-state/prod?region=auto&endpoint=https://xyour-objectstorage.com")
		offline := &offlineClient{}
		statestore.SetS3HTTPClient(s, offline) // the host may be real: nothing may reach it
		if _, err := s.Capabilities(t.Context()); err == nil || offline.calls.Load() == 0 {
			t.Errorf("Capabilities: error = %v after %d requests, want a probe", err, offline.calls.Load())
		}
	})
	t.Run("look-alike subdomain", func(t *testing.T) {
		const host = "your-objectstorage.com.localhost"
		if _, err := net.LookupHost(host); err != nil {
			t.Skipf("%s does not resolve here: %v", host, err)
		}
		f := newFakeS3(t)
		endpoint := strings.Replace(f.url, "127.0.0.1", host, 1)
		s := openS3(t, "s3://"+fakeBucket+"/state?endpoint="+endpoint+"&region=us-east-1&pathStyle=true")
		wantConditionalPut(t, s, true)
	})
}

func TestS3MissingObjectOrBucket(t *testing.T) {
	f := newFakeS3(t)
	s := openS3(t, fakeURL(t, f))
	mustPutS3(t, s, "a", "one")

	// Some servers answer a delete of a missing object with 404.
	f.set(func(f *fakeS3) { f.intercept = answerAll(http.MethodDelete, fakeError{404, "NoSuchKey"}) })
	if err := s.Delete(t.Context(), "a"); err != nil {
		t.Errorf("Delete answered with NoSuchKey: %v", err)
	}
	// A missing bucket is a broken store, not a missing object.
	f.set(func(f *fakeS3) { f.intercept = answerAll("", fakeError{404, "NoSuchBucket"}) })
	if _, _, err := s.Get(t.Context(), "a"); err == nil || errors.Is(err, statestore.ErrNotFound) {
		t.Errorf("Get from a missing bucket: error = %v, want one that is not ErrNotFound", err)
	}
	if err := s.Delete(t.Context(), "a"); err == nil {
		t.Error("Delete from a missing bucket succeeded")
	}
}

func TestS3Capabilities(t *testing.T) {
	rejecting := func(e fakeError) func(f *fakeS3) {
		return func(f *fakeS3) { f.intercept = answerConditional(e) }
	}
	for _, tc := range []struct {
		name  string
		setup func(f *fakeS3)
		want  bool
	}{
		{"enforced", func(*fakeS3) {}, true},
		{"ignored", func(f *fakeS3) { f.ignoreConditions = true }, false},
		{"not implemented", rejecting(fakeError{501, "NotImplemented"}), false},
		{"header rejected", rejecting(fakeError{400, "InvalidArgument"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			f.set(tc.setup)
			s := openS3(t, fakeURL(t, f))
			wantConditionalPut(t, s, tc.want)
			if keys := f.keys(); len(keys) != 0 {
				t.Errorf("the probe left %q", keys)
			}
			for _, r := range f.log() {
				if !strings.HasPrefix(r, http.MethodPut+" state/.tent-probe/") &&
					!strings.HasPrefix(r, http.MethodDelete+" state/.tent-probe/") {
					t.Errorf("the probe sent %s, want only puts and deletes below state/.tent-probe/", r)
				}
			}
			n := len(f.log())
			wantConditionalPut(t, s, tc.want)
			if again := len(f.log()); again != n {
				t.Errorf("the second call sent %d requests, want none: the answer is kept", again-n)
			}
			if tc.want {
				return
			}
			for _, opts := range []statestore.PutOptions{{IfNoneMatch: true}, {IfMatch: `"x"`}} {
				if _, err := s.Put(t.Context(), "a", []byte("x"), opts); !errors.Is(err, errors.ErrUnsupported) {
					t.Errorf("Put with %+v: error = %v, want errors.ErrUnsupported", opts, err)
				}
			}
			if n := f.count(http.MethodPut, "state/a"); n != 0 {
				t.Errorf("rejected conditional puts sent %d puts", n)
			}
		})
	}
}

func TestS3CapabilitiesErrors(t *testing.T) {
	denied := fakeError{403, "AccessDenied"}
	for _, tc := range []struct {
		name      string
		intercept func(r *http.Request, key string, body []byte) *fakeError
	}{
		{"puts denied", answerAll(http.MethodPut, denied)},
		{"header rejected and plain puts denied", func(r *http.Request, key string, body []byte) *fakeError {
			if e := answerConditional(fakeError{400, "InvalidArgument"})(r, key, body); e != nil {
				return e
			}
			return answerAll(http.MethodPut, denied)(r, key, body)
		}},
		{"server error on the first create", answerCreate(1, fakeError{500, "InternalError"})},
		{"server error on the second create", answerCreate(2, fakeError{500, "InternalError"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			f.set(func(f *fakeS3) { f.intercept = tc.intercept })
			s := openS3(t, fakeURL(t, f))
			caps, err := s.Capabilities(t.Context())
			want := "test conditional puts in " + s.String() + ": "
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("Capabilities = %+v, %v; want an error that contains %q", caps, err, want)
			}
			// A failed probe is not kept: the next call probes again.
			f.set(func(f *fakeS3) { f.intercept = nil })
			wantConditionalPut(t, s, true)
		})
	}
}

func TestS3VersionsIgnoreETagQuoting(t *testing.T) {
	unquote := func(etag string) string { return strings.Trim(etag, `"`) }
	for _, tc := range []struct {
		name     string
		put, get func(string) string
	}{
		{"unquoted after a put", unquote, nil},
		{"unquoted after a get", nil, unquote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3(t)
			f.set(func(f *fakeS3) { f.putETag, f.getETag = tc.put, tc.get })
			s := openS3(t, fakeURL(t, f))
			v := mustPutS3(t, s, "a", "one")
			_, got, err := s.Get(t.Context(), "a")
			if err != nil {
				t.Fatal(err)
			}
			if got != v {
				t.Errorf("Get version = %q, Put returned %q", got, v)
			}
			// The fake compares quoted ETags, as S3 does.
			if _, err := s.Put(t.Context(), "a", []byte("two"), statestore.PutOptions{IfMatch: v}); err != nil {
				t.Errorf("Put with the version of Put: %v", err)
			}
			if _, err := s.Put(t.Context(), "a", []byte("three"), statestore.PutOptions{IfMatch: got}); err == nil {
				t.Error("Put with a stale version succeeded")
			}
		})
	}
}

func TestS3VersionsAreNeverEmpty(t *testing.T) {
	f := newFakeS3(t)
	none := func(string) string { return "" }
	f.set(func(f *fakeS3) { f.putETag = none })
	s := openS3(t, fakeURL(t, f))
	if v, err := s.Put(t.Context(), "a", []byte("one"), statestore.PutOptions{}); err == nil {
		t.Errorf("Put without an ETag in the answer = %q, want an error", v)
	}
	f.set(func(f *fakeS3) { f.putETag, f.getETag = nil, none })
	mustPutS3(t, s, "a", "one")
	if _, v, err := s.Get(t.Context(), "a"); err == nil {
		t.Errorf("Get without an ETag in the answer = %q, want an error", v)
	}
}

func TestPurgeS3(t *testing.T) {
	f := newFakeS3(t)
	s := openS3(t, fakeURL(t, f))
	mustPutS3(t, s, "a/b", "x")
	f.put("state/.tent-probe/x", "state/bad name", "stateful/x", "state")
	if err := statestore.PurgeS3(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if got, want := f.keys(), []string{"state", "stateful/x"}; !slices.Equal(got, want) {
		t.Errorf("after PurgeS3 the bucket holds %q, want %q", got, want)
	}
	whole := openS3(t, "s3://"+fakeBucket+"?endpoint="+f.url+"&region=us-east-1&pathStyle=true")
	if err := statestore.PurgeS3(t.Context(), whole); err == nil {
		t.Error("PurgeS3 of a whole bucket succeeded")
	}
}

// answerPuts answers every put of the key with e.
func answerPuts(key string, e fakeError) func(r *http.Request, key string, body []byte) *fakeError {
	want := key
	return func(r *http.Request, key string, _ []byte) *fakeError {
		if r.Method == http.MethodPut && key == want {
			return &e
		}
		return nil
	}
}

// answerAll answers every request with the method, or every request when method is empty, with e.
func answerAll(method string, e fakeError) func(r *http.Request, key string, body []byte) *fakeError {
	return func(r *http.Request, _ string, _ []byte) *fakeError {
		if method == "" || r.Method == method {
			return &e
		}
		return nil
	}
}

// answerConditional answers every conditional put with e.
func answerConditional(e fakeError) func(r *http.Request, key string, body []byte) *fakeError {
	return func(r *http.Request, _ string, _ []byte) *fakeError {
		if r.Method == http.MethodPut && (r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Match") != "") {
			return &e
		}
		return nil
	}
}

// answerCreate answers the nth create-only put with e.
func answerCreate(n int, e fakeError) func(r *http.Request, key string, body []byte) *fakeError {
	creates := 0
	return func(r *http.Request, _ string, _ []byte) *fakeError {
		if r.Method != http.MethodPut || r.Header.Get("If-None-Match") == "" {
			return nil
		}
		if creates++; creates == n {
			return &e
		}
		return nil
	}
}

func wantConditionalPut(t *testing.T, s statestore.Store, want bool) {
	t.Helper()
	caps, err := s.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.ConditionalPut != want {
		t.Errorf("ConditionalPut = %v, want %v", caps.ConditionalPut, want)
	}
}

func mustPutS3(t *testing.T, s statestore.Store, p, data string) statestore.Version {
	t.Helper()
	v, err := s.Put(t.Context(), p, []byte(data), statestore.PutOptions{})
	if err != nil {
		t.Fatalf("Put(%q): %v", p, err)
	}
	return v
}

func openS3(t *testing.T, rawURL string) statestore.Store {
	t.Helper()
	s, err := statestore.Open(t.Context(), rawURL)
	if err != nil {
		t.Fatalf("Open(%q): %v", rawURL, err)
	}
	return s
}

// fakeURL keeps the AWS environment out of the test and returns the URL of the prefix "state" in the fake's bucket.
func fakeURL(t *testing.T, f *fakeS3) string {
	t.Helper()
	s3urltest.IsolateAWS(t)
	return "s3://" + fakeBucket + "/state?endpoint=" + f.url + "&region=us-east-1&pathStyle=true"
}

// s3TestURL returns the URL of a new, empty prefix below the one in TENT_TEST_S3_URL, and deletes everything below
// it when the test ends, because CI runs share one bucket. It skips the test when TENT_TEST_S3_URL is not set.
func s3TestURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv(s3URLEnv)
	if base == "" {
		t.Skipf("set %s to an s3:// URL, with credentials in the AWS environment, to test against a bucket", s3URLEnv)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s: %v", s3URLEnv, err)
	}
	dirs := []string{u.Path}
	if run := os.Getenv("GITHUB_RUN_ID"); run != "" {
		dirs = append(dirs, "run-"+run)
	}
	u.Path = path.Join(append(dirs, segmentOf(t.Name())+"-"+rand.Text()[:8])...)
	raw := u.String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s, err := statestore.Open(ctx, raw)
		if err == nil {
			err = statestore.PurgeS3(ctx, s)
		}
		if err != nil {
			t.Errorf("delete the test's objects: %v", err)
		}
	})
	return raw
}

// segmentOf turns a test name into one path segment.
func segmentOf(name string) string {
	var b strings.Builder
	for _, c := range []byte(name) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
