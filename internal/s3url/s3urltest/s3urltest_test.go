package s3urltest_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"

	"github.com/ingvarch/tent/internal/s3url/s3urltest"
)

// writeFile writes text into a new file in dir and returns its path.
func writeFile(t *testing.T, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// setenv sets each variable of env for the test.
func setenv(t *testing.T, env map[string]string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestIsolateAWSHidesTheDevelopersConfiguration(t *testing.T) {
	dir := t.TempDir()
	profile := "region = eu-west-1\nendpoint_url = https://developer.example\nmax_attempts = 9\nretry_mode = adaptive\n"
	keys := "aws_access_key_id = AKIADEVELOPER\naws_secret_access_key = developer-secret\n"
	setenv(t, map[string]string{
		"AWS_CONFIG_FILE":             writeFile(t, dir, "config", "[default]\n"+profile+"[profile dev]\n"+profile),
		"AWS_SHARED_CREDENTIALS_FILE": writeFile(t, dir, "credentials", "[default]\n"+keys+"[dev]\n"+keys),
		"AWS_PROFILE":                 "dev",
		"AWS_REGION":                  "eu-west-1", "AWS_DEFAULT_REGION": "eu-west-2",
		"AWS_ENDPOINT_URL": "https://developer.example", "AWS_ENDPOINT_URL_S3": "https://s3.developer.example",
		"AWS_MAX_ATTEMPTS": "9", "AWS_RETRY_MODE": "adaptive",
		"AWS_ACCESS_KEY_ID": "AKIADEVELOPER", "AWS_SECRET_ACCESS_KEY": "developer-secret",
		"AWS_SESSION_TOKEN": "developer-token",
	})
	s3urltest.IsolateAWS(t)

	cfg, err := config.LoadDefaultConfig(t.Context(), config.WithLogger(logging.Nop{}))
	if err != nil {
		t.Fatalf("LoadDefaultConfig: %v", err)
	}
	if cfg.Region != "" || cfg.BaseEndpoint != nil || cfg.RetryMaxAttempts != 0 || cfg.RetryMode != "" {
		t.Errorf("the configuration has the region %q, the endpoint %q, %d attempts and the retry mode %q; want none",
			cfg.Region, aws.ToString(cfg.BaseEndpoint), cfg.RetryMaxAttempts, cfg.RetryMode)
	}
	if o := s3.NewFromConfig(cfg).Options(); o.BaseEndpoint != nil {
		t.Errorf("the S3 client has the endpoint %q, want none", *o.BaseEndpoint)
	}
	creds, err := cfg.Credentials.Retrieve(t.Context())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if creds.AccessKeyID != s3urltest.KeyID || creds.SecretAccessKey != s3urltest.Secret || creds.SessionToken != "" {
		t.Errorf("the credentials have the key id %q, a session token %t; want %q, the made-up secret and no token",
			creds.AccessKeyID, creds.SessionToken != "", s3urltest.KeyID)
	}
}

// recorder records the method and path of every request, and refuses each.
type recorder struct {
	mu       sync.Mutex
	requests []string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req.Method+" "+req.URL.Path)
	w.WriteHeader(http.StatusForbidden)
}

func (r *recorder) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

func TestIsolateAWSLeavesNoOtherCredentials(t *testing.T) {
	// Without the keys, the SDK would read the shared files, or ask a container's endpoint, a web identity or the
	// metadata service. Here the files hold made-up keys, and each of the others is a server that records what it is
	// asked.
	var rec recorder
	srv := httptest.NewServer(&rec)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	keys := "[default]\naws_access_key_id = AKIADEVELOPER\naws_secret_access_key = developer-secret\n"
	setenv(t, map[string]string{
		"AWS_CONFIG_FILE":                    writeFile(t, dir, "config", keys),
		"AWS_SHARED_CREDENTIALS_FILE":        writeFile(t, dir, "credentials", keys),
		"AWS_CONTAINER_CREDENTIALS_FULL_URI": srv.URL + "/container",
		"AWS_WEB_IDENTITY_TOKEN_FILE":        writeFile(t, dir, "token", "developer-web-identity-token"),
		"AWS_ROLE_ARN":                       "arn:aws:iam::123456789012:role/developer",
		"AWS_ENDPOINT_URL_STS":               srv.URL + "/sts",
		"AWS_EC2_METADATA_DISABLED":          "false",
		"AWS_EC2_METADATA_SERVICE_ENDPOINT":  srv.URL + "/imds",
	})
	s3urltest.IsolateAWS(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	// A region lets the SDK ask for a web identity's credentials.
	cfg, err := config.LoadDefaultConfig(t.Context(), config.WithRegion("auto"), config.WithLogger(logging.Nop{}))
	var creds aws.Credentials
	if err == nil {
		creds, err = cfg.Credentials.Retrieve(t.Context())
	}
	if err == nil {
		t.Errorf("the SDK found credentials with the key id %q without the keys, want none", creds.AccessKeyID)
	}
	if got := rec.log(); len(got) != 0 {
		t.Errorf("the SDK asked for credentials with %q, want no request", got)
	}
}
