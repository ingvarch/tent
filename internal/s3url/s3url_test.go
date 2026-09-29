package s3url_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/s3url"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want s3url.URL
	}{
		{"s3://tent-state?region=eu-central-1", s3url.URL{Bucket: "tent-state", Region: "eu-central-1"}},
		{"s3://tent-state/?region=eu-central-1", s3url.URL{Bucket: "tent-state", Region: "eu-central-1"}},
		{"s3://tent-state", s3url.URL{Bucket: "tent-state"}},
		{"s3://b/teams/infra/", s3url.URL{Bucket: "b", Prefix: "teams/infra"}},
		{
			"s3://tent-ci/dev?endpoint=https://acct.r2.cloudflarestorage.com&region=auto",
			s3url.URL{Bucket: "tent-ci", Prefix: "dev", Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto"},
		},
		{
			"s3://t/p?endpoint=https%3A%2F%2Fams1.vultrobjects.com%2F&region=ams1",
			s3url.URL{Bucket: "t", Prefix: "p", Endpoint: "https://ams1.vultrobjects.com", Region: "ams1"},
		},
		{
			"s3://t/p?endpoint=http://localhost:7070&region=us-east-1&pathStyle=true",
			s3url.URL{Bucket: "t", Prefix: "p", Endpoint: "http://localhost:7070", Region: "us-east-1", PathStyle: true},
		},
		{"s3://t/p?pathStyle=false", s3url.URL{Bucket: "t", Prefix: "p"}},
	} {
		got, err := s3url.Parse(tc.raw, nil)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.raw, err)
			continue
		}
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("Parse(%q) (-want +got):\n%s", tc.raw, diff)
		}
	}
}

func TestParseFails(t *testing.T) {
	// hidden stands for a secret that a URL holds by mistake: no error shows it.
	const hidden = "s3cret"
	for _, tc := range []struct{ name, raw, want string }{
		{"user and password", "s3://AKIAKEY:" + hidden + "@b/p", "remove the user and password: credentials come " +
			"from the environment"},
		{"a slash in the password", "s3://AKIAKEY:" + hidden + "/x@b/p", "remove the user and password"},
		{"does not parse", "s3://b/p\x7f" + hidden, "invalid control character in URL"},
		{"another scheme", "https://" + hidden + ".example.com/p", "not an s3 URL: s3://bucket[/prefix]"},
		{"no scheme", hidden + "/p", "not an s3 URL: s3://bucket[/prefix]"},
		{"empty", "s3://", "name a bucket: s3://bucket[/prefix]"},
		{"opaque", "s3:" + hidden, "name a bucket: s3://bucket[/prefix]"},
		{"port", "s3://b:9000/p", "a bucket has no port: name the server with endpoint=https://host:port"},
		{"fragment", "s3://b/p#" + hidden, "an s3 URL takes no fragment"},
		{"unknown parameter", "s3://b?token=" + hidden, "unknown query parameter: an s3 URL takes endpoint, region " +
			"and pathStyle"},
		{"parameter case", "s3://b?Region=" + hidden, "unknown query parameter"},
		{"repeated parameter", "s3://b?region=" + hidden + "&region=auto", "region is given more than once"},
		{"empty region", "s3://b?region=", "region is empty"},
		{"endpoint without a scheme", "s3://b?endpoint=" + hidden + ".example.com", "endpoint must be https:// or " +
			"http:// and a host, without a path, query or fragment"},
		{"endpoint scheme", "s3://b?endpoint=ftp://" + hidden, "endpoint must be"},
		{"endpoint without a host", "s3://b?endpoint=https://", "endpoint must be"},
		{"endpoint path", "s3://b?endpoint=https://example.com/" + hidden, "endpoint must be"},
		{"endpoint query", "s3://b?endpoint=https://example.com%3Fx%3D" + hidden, "endpoint must be"},
		{"endpoint escape", "s3://b?endpoint=https://example.com%25zz" + hidden, "endpoint must be"},
		{"empty endpoint", "s3://b?endpoint=", "endpoint must be"},
		{"endpoint with credentials", "s3://b?endpoint=https%3A%2F%2FAKIAKEY%3A" + hidden + "%40example.com",
			"remove the user and password from the endpoint: credentials come from the environment"},
		{"pathStyle", "s3://b?pathStyle=" + hidden, "pathStyle must be true or false"},
		{"bad escape", "s3://b?x=%zz" + hidden, "invalid URL escape"},
		{"semicolon", "s3://b?region=auto;pathStyle=" + hidden, "semicolon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := s3url.Parse(tc.raw, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%q) = %+v, %v; want an error that says %q", tc.raw, u, err, tc.want)
			}
			for _, secret := range []string{hidden, "AKIAKEY"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error %q shows %q", err, secret)
				}
			}
			// An escaped @ is no way out: a prefix check refuses it.
			if strings.Contains(err.Error(), "%40") {
				t.Errorf("the error %q suggests %%40", err)
			}
		})
	}
}

func TestParseChecksThePrefix(t *testing.T) {
	var checked []string
	refuse := errors.New("segment \"x\" is refused")
	check := func(prefix string) error {
		checked = append(checked, prefix)
		if strings.HasSuffix(prefix, "x") {
			return refuse
		}
		return nil
	}
	if _, err := s3url.Parse("s3://b/a/b/", check); err != nil {
		t.Errorf("a prefix the check accepts: %v", err)
	}
	// A URL without a prefix is not checked.
	if _, err := s3url.Parse("s3://b/", check); err != nil {
		t.Errorf("no prefix: %v", err)
	}
	_, err := s3url.Parse("s3://b/a/x?region=auto", check)
	if !errors.Is(err, refuse) || err.Error() != `invalid prefix: segment "x" is refused` {
		t.Errorf("a prefix the check refuses: %v, want %q wrapping the check's error", err,
			`invalid prefix: segment "x" is refused`)
	}
	// The prefix is checked before the query.
	if _, err := s3url.Parse("s3://b/x?nope=1", check); !errors.Is(err, refuse) {
		t.Errorf("a refused prefix and an unknown parameter: %v, want the prefix's error", err)
	}
	if diff := cmp.Diff([]string{"a/b", "a/x", "x"}, checked); diff != "" {
		t.Errorf("prefixes checked (-want +got):\n%s", diff)
	}
}

func TestString(t *testing.T) {
	for _, tc := range []struct {
		u    s3url.URL
		want string
	}{
		{s3url.URL{Bucket: "b", Region: "auto"}, "s3://b"},
		{s3url.URL{Bucket: "b", Prefix: "a/b", PathStyle: true}, "s3://b/a/b"},
		{s3url.URL{Bucket: "b", Prefix: "p", Endpoint: "https://h", Region: "auto"}, "s3://b/p?endpoint=https://h"},
	} {
		if got := tc.u.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.u, got, tc.want)
		}
	}
}
