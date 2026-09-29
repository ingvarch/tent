// Package s3urltest keeps the developer's AWS configuration out of the tests that make S3 clients. It imports testing,
// so only tests import it.
package s3urltest

import (
	"path/filepath"
	"testing"
)

// The made-up keys that IsolateAWS gives the AWS SDK. Tests check that nothing prints them.
const (
	KeyID  = "AKIATENTS3URLTEST"
	Secret = "tent-s3urltest-secret-access-key-0123456789"
)

// IsolateAWS keeps the developer's AWS configuration out of a test: no shared files, no region, endpoint or retry
// settings, and no credentials from the metadata service, a container or a web identity. The keys are KeyID and
// Secret, without a session token.
func IsolateAWS(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "missing")
	for _, kv := range [][2]string{
		{"AWS_CONFIG_FILE", missing}, {"AWS_SHARED_CREDENTIALS_FILE", missing}, {"AWS_PROFILE", ""},
		{"AWS_REGION", ""}, {"AWS_DEFAULT_REGION", ""}, {"AWS_ENDPOINT_URL", ""}, {"AWS_ENDPOINT_URL_S3", ""},
		{"AWS_MAX_ATTEMPTS", ""}, {"AWS_RETRY_MODE", ""}, {"AWS_EC2_METADATA_DISABLED", "true"},
		{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", ""}, {"AWS_CONTAINER_CREDENTIALS_FULL_URI", ""},
		{"AWS_WEB_IDENTITY_TOKEN_FILE", ""}, {"AWS_ROLE_ARN", ""},
		{"AWS_ACCESS_KEY_ID", KeyID}, {"AWS_SECRET_ACCESS_KEY", Secret}, {"AWS_SESSION_TOKEN", ""},
	} {
		t.Setenv(kv[0], kv[1])
	}
}
