package statestore_test

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeBucket   = "tent-test"
	fakePageSize = 2 // keys per List page, small so that every List pages
)

// fakeS3 is an in-memory S3 server with one bucket, for tests without a real bucket. Like AWS S3, it enforces
// If-None-Match and If-Match atomically and answers a replace of a missing object with 404. It takes path-style
// requests, and virtual-host ones to tent-test.localhost. Like Ceph RGW, it sends no checksums, and it refuses a GET
// that asks for them.
type fakeS3 struct {
	url string // http://127.0.0.1:port

	mu      sync.Mutex
	objects map[string][]byte // by key
	// ignoreConditions writes conditional puts unconditionally, as a server without conditional puts does.
	ignoreConditions bool
	// putETag turns the quoted ETag of a written object into the ETag header of the PUT response.
	putETag func(etag string) string
	// getETag does the same for GET responses.
	getETag func(etag string) string
	// writeInterval, when set, lets one write a key per interval and answers faster writes with 429, as Cloudflare R2
	// does with one write per second. Every write it lets through counts, also one that fails its condition.
	writeInterval time.Duration
	lastWrite     map[string]time.Time // by key
	// badDate sends a Date header that does not parse.
	badDate bool
	// ignoreListPrefix lists every key, whatever prefix the request asks for.
	ignoreListPrefix bool
	// intercept answers a request instead of the bucket when it returns an error. It runs with the lock held, so it
	// may change objects. The key is "" when the request names none.
	intercept func(r *http.Request, key string, body []byte) *fakeError
	requests  []string // method and key of every request
	hosts     []string // Host header of every request
}

// fakeError is an S3 error response.
type fakeError struct {
	status int
	code   string
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, lastWrite: map[string]time.Time{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// set changes the fake's behaviour while the server may be serving.
func (f *fakeS3) set(change func(f *fakeS3)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// keys returns the keys of the stored objects, sorted.
func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// put stores objects directly, as another client would.
func (f *fakeS3) put(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		f.objects[k] = []byte(k)
	}
}

// count returns how many requests with this method went to the key.
func (f *fakeS3) count(method, key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r == method+" "+key {
			n++
		}
	}
	return n
}

// log returns the method and key of every request so far.
func (f *fakeS3) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// hostLog returns the Host header of every request so far.
func (f *fakeS3) hostLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hosts)
}

// route returns the key a request names, path-style or virtual-host, and whether it names the bucket at all.
func route(r *http.Request) (key string, inBucket bool) {
	host, _, _ := strings.Cut(r.Host, ":")
	if host == fakeBucket+".localhost" {
		return strings.TrimPrefix(r.URL.Path, "/"), true
	}
	if r.URL.Path == "/"+fakeBucket {
		return "", true
	}
	return strings.CutPrefix(r.URL.Path, "/"+fakeBucket+"/")
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fakeError{http.StatusBadRequest, "IncompleteBody"}.write(w)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key, inBucket := route(r)
	f.requests = append(f.requests, r.Method+" "+key)
	f.hosts = append(f.hosts, r.Host)
	if f.badDate {
		w.Header().Set("Date", "not a date")
	}
	if f.intercept != nil {
		if e := f.intercept(r, key, body); e != nil {
			e.write(w)
			return
		}
	}
	isWrite := r.Method == http.MethodPut || r.Method == http.MethodDelete
	switch {
	case !inBucket:
		fakeError{http.StatusNotFound, "NoSuchBucket"}.write(w)
	case r.Method == http.MethodGet && key == "" && r.URL.Query().Get("list-type") == "2":
		f.list(w, r)
	case key == "":
		fakeError{http.StatusBadRequest, "InvalidRequest"}.write(w)
	case isWrite && !f.admitWrite(key):
		fakeError{http.StatusTooManyRequests, "TooManyRequests"}.write(w)
	case r.Method == http.MethodPut:
		f.write(w, r, key, body)
	case r.Method == http.MethodGet && r.Header.Get("X-Amz-Checksum-Mode") != "":
		fakeError{http.StatusBadRequest, "InvalidRequest"}.write(w)
	case r.Method == http.MethodGet:
		f.read(w, key)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		fakeError{http.StatusNotImplemented, "NotImplemented"}.write(w)
	}
}

// admitWrite reports whether the write limit lets a write to the key through now, and counts it if so.
func (f *fakeS3) admitWrite(key string) bool {
	if f.writeInterval == 0 {
		return true
	}
	now := time.Now()
	if last, ok := f.lastWrite[key]; ok && now.Sub(last) < f.writeInterval {
		return false
	}
	f.lastWrite[key] = now
	return true
}

func (f *fakeS3) write(w http.ResponseWriter, r *http.Request, key string, body []byte) {
	old, exists := f.objects[key]
	ifMatch := r.Header.Get("If-Match")
	if !f.ignoreConditions {
		switch {
		case r.Header.Get("If-None-Match") == "*" && exists:
			fakeError{http.StatusPreconditionFailed, "PreconditionFailed"}.write(w)
			return
		case ifMatch != "" && !exists:
			fakeError{http.StatusNotFound, "NoSuchKey"}.write(w)
			return
		case ifMatch != "" && ifMatch != etagOf(old):
			fakeError{http.StatusPreconditionFailed, "PreconditionFailed"}.write(w)
			return
		}
	}
	f.objects[key] = body
	etag := etagOf(body)
	if f.putETag != nil {
		etag = f.putETag(etag)
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) read(w http.ResponseWriter, key string) {
	data, ok := f.objects[key]
	if !ok {
		fakeError{http.StatusNotFound, "NoSuchKey"}.write(w)
		return
	}
	etag := etagOf(data)
	if f.getETag != nil {
		etag = f.getETag(etag)
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	_, _ = w.Write(data) // the client sees a short body
}

// listResult is the ListObjectsV2 response.
type listResult struct {
	XMLName               xml.Name    `xml:"ListBucketResult"`
	Xmlns                 string      `xml:"xmlns,attr"`
	Name                  string      `xml:"Name"`
	Prefix                string      `xml:"Prefix"`
	KeyCount              int         `xml:"KeyCount"`
	MaxKeys               int         `xml:"MaxKeys"`
	IsTruncated           bool        `xml:"IsTruncated"`
	Contents              []listEntry `xml:"Contents"`
	NextContinuationToken string      `xml:"NextContinuationToken,omitempty"`
}

type listEntry struct {
	Key  string `xml:"Key"`
	ETag string `xml:"ETag"`
	Size int    `xml:"Size"`
}

// list answers ListObjectsV2 in pages of fakePageSize keys. The continuation token is the last key of a page.
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, after := q.Get("prefix"), q.Get("continuation-token")
	if f.ignoreListPrefix {
		prefix = ""
	}
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	res := listResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: fakeBucket, Prefix: prefix, MaxKeys: fakePageSize,
	}
	if len(keys) > fakePageSize {
		keys = keys[:fakePageSize]
		res.IsTruncated, res.NextContinuationToken = true, keys[len(keys)-1]
	}
	for _, k := range keys {
		res.Contents = append(res.Contents, listEntry{Key: k, ETag: etagOf(f.objects[k]), Size: len(f.objects[k])})
	}
	res.KeyCount = len(res.Contents)
	writeXML(w, http.StatusOK, res)
}

func (e fakeError) write(w http.ResponseWriter) {
	writeXML(w, e.status, struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: e.code, Message: e.code + " from the fake"})
}

func writeXML(w http.ResponseWriter, status int, v any) {
	data, err := xml.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write(append([]byte(xml.Header), data...)) // the client sees a short body
}

// etagOf returns the quoted MD5 sum of data, the ETag S3 gives an object written in one PUT.
func etagOf(data []byte) string {
	sum := md5.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}
