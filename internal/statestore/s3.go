package statestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/ingvarch/tent/internal/s3url"
)

// A server throttles a request when it gets too many: Cloudflare R2 takes one write per second to a key and answers
// more with 429. The store sends a throttled request again after throttleWait plus up to as much again, at most
// throttleAttempts times in all, which rides out a burst of 16 writers to one key on R2.
const throttleAttempts = 30

var throttleWait = time.Second // a variable for tests

// probeDir holds the objects that Capabilities writes to test conditional puts. It starts with '.', so it is never
// an object path and List hides it.
const probeDir = ".tent-probe"

// s3Store keeps objects in an S3 bucket: the path p is the key prefix/p, or p without a prefix.
type s3Store struct {
	client *s3.Client
	bucket string
	prefix string // without a slash at either end; empty for the whole bucket
	url    string // without region and credentials, for messages

	mu   sync.Mutex    // held while probing
	caps *Capabilities // the probe's answer, once there is one; set from the start for Hetzner
}

// newS3Store opens the bucket that rawURL names; Open has checked that the URL parses and has no user.
func newS3Store(ctx context.Context, rawURL string) (*s3Store, error) {
	p, err := s3url.Parse(rawURL, checkPrefix)
	if err != nil {
		return nil, fmt.Errorf("state store URL: %w", err)
	}
	client, err := p.Client(ctx)
	switch {
	case errors.Is(err, s3url.ErrNoRegion):
		return nil, fmt.Errorf("state store URL: %w", err)
	case err != nil:
		return nil, fmt.Errorf("state store: %w", err)
	}
	s := &s3Store{client: client, bucket: p.Bucket, prefix: p.Prefix, url: p.String()}
	// Hetzner Object Storage does not document conditional writes, so tent does not rely on them until end-to-end
	// tests prove them.
	if hetzner(p.Endpoint) {
		s.caps = &Capabilities{}
	}
	return s, nil
}

// checkPrefix checks the prefix of an s3 URL: its segments follow the rules of object paths.
func checkPrefix(prefix string) error {
	if why := badSegments(prefix); why != "" {
		return errors.New(why)
	}
	return nil
}

// hetzner reports whether an endpoint is Hetzner Object Storage.
func hetzner(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "your-objectstorage.com" || strings.HasSuffix(host, ".your-objectstorage.com")
}

// key returns the key of the object at path p, or of the objects below p when p ends with a slash or is empty.
func (s *s3Store) key(p string) string {
	if s.prefix == "" {
		return p
	}
	return s.prefix + "/" + p
}

func (s *s3Store) Get(ctx context.Context, p string) ([]byte, Version, error) {
	out, err := untilAccepted(ctx, func() (*s3.GetObjectOutput, error) {
		return s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(p))})
	})
	if missingObject(err) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = out.Body.Close() }() // nothing to flush after a read
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", err
	}
	v, err := etagVersion(out.ETag)
	if err != nil {
		return nil, "", err
	}
	return data, v, nil
}

func (s *s3Store) Put(ctx context.Context, p string, data []byte, opts PutOptions) (Version, error) {
	if opts.IfNoneMatch || opts.IfMatch != "" {
		caps, err := s.Capabilities(ctx)
		if err != nil {
			return "", err
		}
		if !caps.ConditionalPut {
			return "", fmt.Errorf("%w: the store does not enforce conditional puts", errors.ErrUnsupported)
		}
	}
	return s.put(ctx, s.key(p), data, opts)
}

// put writes an object by its key. The SDK does not retry a conditional put: a retry after a write whose answer was
// lost would find that write and fail with ErrPreconditionFailed, so the winner of a race would believe it lost. A
// throttled put was not carried out, so untilAccepted sends it again.
func (s *s3Store) put(ctx context.Context, key string, data []byte, opts PutOptions) (Version, error) {
	in := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	var once []func(*s3.Options)
	switch {
	case opts.IfNoneMatch:
		in.IfNoneMatch = aws.String("*")
	case opts.IfMatch != "":
		in.IfMatch = aws.String(string(opts.IfMatch))
	}
	if in.IfNoneMatch != nil || in.IfMatch != nil {
		once = append(once, func(o *s3.Options) { o.RetryMaxAttempts = 1 })
	}
	out, err := untilAccepted(ctx, func() (*s3.PutObjectOutput, error) {
		in.Body = bytes.NewReader(data)
		return s.client.PutObject(ctx, in, once...)
	})
	if err != nil {
		return "", putError(err, opts)
	}
	return etagVersion(out.ETag)
}

// putError maps the error of a conditional put that the condition stopped to ErrPreconditionFailed: a 412, any 409
// (a concurrent conditional write won: ConditionalRequestConflict on AWS, ConcurrentModification on Ceph RGW), and a
// 404 on a replace (SeaweedFS answers 412 there). A 501 means the server has no conditional puts.
func putError(err error, opts PutOptions) error {
	if !opts.IfNoneMatch && opts.IfMatch == "" {
		return err
	}
	status := httpStatus(err)
	switch {
	case status == http.StatusPreconditionFailed && opts.IfNoneMatch:
		return fmt.Errorf("%w: the object exists", ErrPreconditionFailed)
	case status == http.StatusPreconditionFailed:
		return fmt.Errorf("%w: the object has changed", ErrPreconditionFailed)
	case status == http.StatusConflict:
		return fmt.Errorf("%w: a concurrent write came first", ErrPreconditionFailed)
	case opts.IfMatch != "" && missingObject(err):
		return fmt.Errorf("%w: the object does not exist", ErrPreconditionFailed)
	case status == http.StatusNotImplemented:
		return fmt.Errorf("%w: the server does not implement conditional puts", errors.ErrUnsupported)
	}
	return err
}

func (s *s3Store) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := s.keys(ctx, s.key(prefix))
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, k := range keys {
		// Hides keys outside the prefix, the probe's objects and names that are not paths.
		if p, ok := strings.CutPrefix(k, s.key("")); ok && strings.HasPrefix(p, prefix) && badPath(p) == "" {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	return paths, nil
}

// keys returns the keys that start with prefix, as the server lists them.
func (s *s3Store) keys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := untilAccepted(ctx, func() (*s3.ListObjectsV2Output, error) { return pages.NextPage(ctx) })
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}

func (s *s3Store) Delete(ctx context.Context, p string) error { return s.deleteKey(ctx, s.key(p)) }

// deleteKey deletes an object by its key. Most servers answer 204 for a missing object, some 404.
func (s *s3Store) deleteKey(ctx context.Context, key string) error {
	_, err := untilAccepted(ctx, func() (*s3.DeleteObjectOutput, error) {
		return s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	})
	if err != nil && !missingObject(err) {
		return err
	}
	return nil
}

// Capabilities probes the server on the first call that gets an answer, and keeps the answer.
func (s *s3Store) Capabilities(ctx context.Context) (Capabilities, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.caps == nil {
		enforced, err := s.probe(ctx)
		if err != nil {
			return Capabilities{}, fmt.Errorf("test conditional puts in %s: %w", s.url, err)
		}
		s.caps = &Capabilities{ConditionalPut: enforced}
	}
	return *s.caps, nil
}

// probe reports whether the server enforces If-None-Match: it creates an object below probeDir twice, and the
// second create must fail. Some servers ignore the header, and some reject it. It deletes the object again.
func (s *s3Store) probe(ctx context.Context) (enforced bool, err error) {
	key := s.key(probeDir + "/" + rand.Text())
	defer func() {
		// Also when ctx has ended: List hides a leftover, but it would stay forever.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonTimeout)
		defer cancel()
		if derr := s.deleteKey(dctx, key); derr != nil && err == nil {
			err = fmt.Errorf("delete the probe object: %w", derr)
		}
	}()
	createOnly := PutOptions{IfNoneMatch: true}
	if _, err = s.put(ctx, key, nil, createOnly); err != nil {
		if !errors.Is(err, ErrPreconditionFailed) && !headerRejected(err) {
			return false, err
		}
		// A plain put tells a server without the header from one that refuses every write.
		_, err = s.put(ctx, key, nil, PutOptions{})
		return false, err
	}
	_, err = s.put(ctx, key, nil, createOnly)
	switch {
	case errors.Is(err, ErrPreconditionFailed):
		return true, nil
	case err == nil || headerRejected(err):
		return false, nil
	}
	return false, err
}

// untilAccepted calls send, which sends one request, again while the server throttles it. A throttled request was
// not carried out, so sending it again is safe, a conditional put too.
func untilAccepted[T any](ctx context.Context, send func() (T, error)) (T, error) {
	for attempt := 1; ; attempt++ {
		out, err := send()
		if !throttled(err) || attempt == throttleAttempts {
			return out, err
		}
		if werr := sleep(ctx, throttleWait+mathrand.N(throttleWait)); werr != nil {
			return out, fmt.Errorf("%w; the server throttled the request: %w", werr, err)
		}
	}
}

// throttled reports whether err says the server did not carry out a request because it got too many: 429, or
// SlowDown (AWS S3 and Ceph RGW answer 503 SlowDown).
func throttled(err error) bool {
	code := errorCode(err)
	return httpStatus(err) == http.StatusTooManyRequests || code == "TooManyRequests" || code == "SlowDown"
}

// headerRejected reports whether a conditional put failed because the server does not take its header.
func headerRejected(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) || httpStatus(err) == http.StatusBadRequest
}

// etagVersion returns the version of an ETag. Servers quote ETags in some answers and not in others; the version
// is always quoted, as If-Match wants it.
func etagVersion(etag *string) (Version, error) {
	e := aws.ToString(etag)
	switch {
	case e == "":
		return "", errors.New("the server sent no ETag")
	case strings.HasPrefix(e, `"`):
		return Version(e), nil
	}
	return Version(`"` + e + `"`), nil
}

// missingObject reports whether err says the object does not exist: a 404, but not for a missing bucket.
func missingObject(err error) bool {
	return httpStatus(err) == http.StatusNotFound && errorCode(err) != "NoSuchBucket"
}

// httpStatus returns the HTTP status of the answer that err reports, or 0.
func httpStatus(err error) int {
	if e, ok := errors.AsType[interface {
		error
		HTTPStatusCode() int
	}](err); ok {
		return e.HTTPStatusCode()
	}
	return 0
}

// errorCode returns the S3 error code that err reports, or "".
func errorCode(err error) string {
	if e, ok := errors.AsType[smithy.APIError](err); ok {
		return e.ErrorCode()
	}
	return ""
}

func (s *s3Store) String() string { return s.url }
