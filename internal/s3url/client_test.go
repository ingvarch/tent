package s3url_test

import (
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/s3url"
	"github.com/ingvarch/tent/internal/s3url/s3urltest"
)

// server records the method and path of every request, and answers each with 200.
type server struct {
	mu       sync.Mutex
	requests []string
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	w.WriteHeader(http.StatusOK)
}

func TestClientReachesTheEndpoint(t *testing.T) {
	s3urltest.IsolateAWS(t)
	var s server
	srv := httptest.NewServer(&s)
	t.Cleanup(srv.Close)
	u, err := s3url.Parse("s3://tent-dev/p?endpoint="+srv.URL+"&region=auto&pathStyle=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := u.Client(t.Context())
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if _, err := c.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String(u.Bucket), Key: aws.String("p/key"),
	}); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if diff := cmp.Diff([]string{"HEAD /tent-dev/p/key"}, s.requests); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestClientAsksForChecksumsOnlyWhenRequired(t *testing.T) {
	// A download asks for no checksum: some servers send none, and a presigned URL would carry the request.
	s3urltest.IsolateAWS(t)
	u, err := s3url.Parse("s3://b/p?endpoint=https://example.com&region=auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := u.Client(t.Context())
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	req, err := s3.NewPresignClient(c).PresignGetObject(t.Context(),
		&s3.GetObjectInput{Bucket: aws.String("b"), Key: aws.String("p/key")})
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}
	q, err := url.Parse(req.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires", "X-Amz-Signature",
		"X-Amz-SignedHeaders", "x-id"}
	if diff := cmp.Diff(want, slices.Sorted(maps.Keys(q.Query()))); diff != "" {
		t.Errorf("the presigned URL's query parameters (-want +got):\n%s", diff)
	}
}

func TestClientNeedsARegion(t *testing.T) {
	s3urltest.IsolateAWS(t)
	u, err := s3url.Parse("s3://b/p?endpoint=https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = u.Client(t.Context())
	want := "no region: add region=… (Cloudflare R2 takes auto) or set AWS_REGION"
	if !errors.Is(err, s3url.ErrNoRegion) || err.Error() != want {
		t.Errorf("Client without a region = %v, want ErrNoRegion, %q", err, want)
	}
	t.Setenv("AWS_REGION", "eu-central-1")
	if _, err := u.Client(t.Context()); err != nil {
		t.Errorf("Client with AWS_REGION: %v", err)
	}
}
