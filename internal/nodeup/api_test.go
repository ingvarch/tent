package nodeup_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/nodeup"
)

// TestAPIFollowsNoRedirect checks that a call to a Nomad agent takes a redirect as the answer: the call's headers
// would go with it.
func TestAPIFollowsNoRedirect(t *testing.T) {
	h, fsys, _, _ := ubuntu(t)
	srv := agentAnswering(t, h, fsys, v1alpha1.RoleClient, "/v1/agent/health",
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/v1/agent/health?type=client&again", http.StatusFound)
		})
	err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleClient, 0)
	want := "check the Nomad agent: GET /v1/agent/health?type=client answered 302 Found"
	if !strings.HasPrefix(errText(err), want) {
		t.Errorf("CheckHealth = %q, want %q and a quote", errText(err), want)
	}
	if got := srv.Requests(); len(got) != 1 {
		t.Errorf("the agent saw %q, want one request", got)
	}
}

// TestAPIBoundsTheAnswer checks that an answer of 1 MiB is taken, and a longer one fails the call.
func TestAPIBoundsTheAnswer(t *testing.T) {
	for size, want := range map[int]string{
		1 << 20:   "",
		1<<20 + 1: "check the Nomad agent: GET /v1/agent/health?type=client: the answer is over 1048576 bytes",
	} {
		h, fsys, _, _ := ubuntu(t)
		agentAnswering(t, h, fsys, v1alpha1.RoleClient, "/v1/agent/health", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), size))
		})
		if err := nodeup.CheckHealth(t.Context(), h, v1alpha1.RoleClient, 0); errText(err) != want {
			t.Errorf("an answer of %d bytes: %q, want %q", size, errText(err), want)
		}
	}
}
