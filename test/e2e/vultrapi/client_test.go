package vultrapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "k3y-s3cret-value"

// newTestClient returns a client that talks to a server running handler, with no waits between tries.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(testKey)
	c.BaseURL = srv.URL + "/v2"
	c.RetryWait = time.Millisecond
	return c
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestNewSetsDefaults(t *testing.T) {
	c := New(testKey)
	if c.BaseURL != "https://api.vultr.com/v2" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.HTTP == nil || c.HTTP.Timeout != 30*time.Second {
		t.Errorf("HTTP = %+v, want a client with a 30 s timeout", c.HTTP)
	}
	if c.RetryWait != time.Second {
		t.Errorf("RetryWait = %v, want 1s", c.RetryWait)
	}
}

func TestRequestsCarryTheBearerKey(t *testing.T) {
	var got string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		writeJSON(w, 200, `{"instances": [], "meta": {"links": {"next": ""}}}`)
	})
	if _, err := c.Instances(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer "+testKey {
		t.Errorf("Authorization = %q", got)
	}
}

func TestListFollowsTheCursorUntilItIsEmpty(t *testing.T) {
	var queries []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Path != "/v2/instances" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			writeJSON(w, 200, `{"instances": [{"id": "a", "date_created": "2026-10-07T10:00:00+00:00"}],
				"meta": {"total": 2, "links": {"next": "page2", "prev": ""}}}`)
		case "page2":
			writeJSON(w, 200, `{"instances": [{"id": "b", "date_created": "2026-10-07T11:00:00+00:00"}],
				"meta": {"total": 2, "links": {"next": "", "prev": "x"}}}`)
		default:
			t.Errorf("unexpected cursor in %q", r.URL.RawQuery)
		}
	})
	got, err := c.Instances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("instances = %+v, want a then b", got)
	}
	want := []string{"per_page=500", "cursor=page2&per_page=500"}
	if len(queries) != 2 || queries[0] != want[0] || queries[1] != want[1] {
		t.Errorf("queries = %q, want %q", queries, want)
	}
}

func TestListFailsWhenACursorComesBack(t *testing.T) {
	for name, next := range map[string]map[string]string{
		"the cursor of the page just read": {"": "c1", "c1": "c1"},
		"an earlier cursor":                {"": "c1", "c1": "c2", "c2": "c1"},
	} {
		t.Run(name, func(t *testing.T) {
			asked := 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				asked++
				following := next[r.URL.Query().Get("cursor")]
				if asked > 10 {
					following = "" // ends a client that follows a loop, so the test fails instead of hanging
				}
				writeJSON(w, 200, `{"instances": [], "meta": {"links": {"next": "`+following+`"}}}`)
			})
			_, err := c.Instances(context.Background())
			want := "vultr: GET /v2/instances: the cursor c1 came back"
			if err == nil || err.Error() != want {
				t.Errorf("Instances error = %v, want %q", err, want)
			}
			if asked > len(next) {
				t.Errorf("asked %d times, want at most %d", asked, len(next))
			}
		})
	}
}

func TestInstanceFields(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, `{"instances": [{"id": "i-1", "label": "e2e-x-s1", "region": "fra", "main_ip": "203.0.113.5",
			"tags": ["tent", "e2e"], "date_created": "2022-10-19T12:15:38+00:00", "os": "ignored",
			"status": "active", "power_status": "stopped", "server_status": "locked"}],
			"meta": {"links": {"next": ""}}}`)
	})
	got, err := c.Instances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Instance{ID: "i-1", Label: "e2e-x-s1", Region: "fra", MainIP: "203.0.113.5", Tags: []string{"tent", "e2e"},
		Created: time.Date(2022, 10, 19, 12, 15, 38, 0, time.UTC)}
	if len(got) != 1 || got[0].ID != want.ID || got[0].Label != want.Label || got[0].Region != want.Region ||
		got[0].MainIP != want.MainIP || strings.Join(got[0].Tags, ",") != "tent,e2e" || !got[0].Created.Equal(want.Created) ||
		got[0].Status != "active" || got[0].PowerStatus != "stopped" || got[0].ServerStatus != "locked" {
		t.Errorf("instances = %+v, want %+v", got, want)
	}
}

func TestVPCFields(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/vpcs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(w, 200, `{"vpcs": [{"id": "v-1", "description": "tent:x", "region": "fra",
			"date_created": "2022-10-19T12:15:38+00:00", "v4_subnet": "10.0.0.0"}], "meta": {"links": {"next": ""}}}`)
	})
	got, err := c.VPCs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "v-1" || got[0].Description != "tent:x" || got[0].Region != "fra" ||
		!got[0].Created.Equal(time.Date(2022, 10, 19, 12, 15, 38, 0, time.UTC)) {
		t.Errorf("vpcs = %+v", got)
	}
}

func TestFirewallGroupFields(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/firewalls" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(w, 200, `{"firewall_groups": [{"id": "f-1", "description": "tent:y",
			"date_created": "2022-10-19T12:15:38+00:00", "rule_count": 3}], "meta": {"links": {"next": ""}}}`)
	})
	got, err := c.FirewallGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "f-1" || got[0].Description != "tent:y" ||
		!got[0].Created.Equal(time.Date(2022, 10, 19, 12, 15, 38, 0, time.UTC)) {
		t.Errorf("firewall groups = %+v", got)
	}
}

func TestSSHKeyFields(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/ssh-keys" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(w, 200, `{"ssh_keys": [{"id": "s-1", "name": "tent:z", "ssh_key": "ssh-ed25519 AAAA",
			"date_created": "2022-10-19T12:15:38+00:00"}], "meta": {"links": {"next": ""}}}`)
	})
	got, err := c.SSHKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "s-1" || got[0].Name != "tent:z" ||
		!got[0].Created.Equal(time.Date(2022, 10, 19, 12, 15, 38, 0, time.UTC)) {
		t.Errorf("ssh keys = %+v", got)
	}
}

func TestUnparsableDateNamesTheObject(t *testing.T) {
	cases := map[string]struct {
		body string
		call func(*Client, context.Context) error
		want string
	}{
		"instance": {`{"instances": [{"id": "i-9", "date_created": "yesterday"}]}`,
			func(c *Client, ctx context.Context) error { _, err := c.Instances(ctx); return err }, "instance i-9"},
		"vpc": {`{"vpcs": [{"id": "v-9", "date_created": "yesterday"}]}`,
			func(c *Client, ctx context.Context) error { _, err := c.VPCs(ctx); return err }, "vpc v-9"},
		"firewall": {`{"firewall_groups": [{"id": "f-9", "date_created": ""}]}`,
			func(c *Client, ctx context.Context) error { _, err := c.FirewallGroups(ctx); return err },
			"firewall group f-9"},
		"ssh key": {`{"ssh_keys": [{"id": "s-9", "date_created": "1"}]}`,
			func(c *Client, ctx context.Context) error { _, err := c.SSHKeys(ctx); return err }, "ssh key s-9"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, tc.body) })
			err := tc.call(c, context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `{"instances": [`) })
	_, err := c.Instances(context.Background())
	if err == nil || !strings.Contains(err.Error(), "GET /v2/instances") {
		t.Errorf("err = %v, want a decode error that names the call", err)
	}
}

func TestListWithoutItsKeyIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, `{"meta": {"links": {"next": ""}}}`)
	})
	_, err := c.Instances(context.Background())
	if err == nil || !strings.Contains(err.Error(), `no "instances"`) {
		t.Errorf("err = %v, want an error about the missing \"instances\"", err)
	}
}

func TestRetriesTransientStatusesThenSucceeds(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) < 3 {
					writeJSON(w, status, `{"error": "slow down", "status": 0}`)
					return
				}
				writeJSON(w, 200, `{"instances": [{"id": "ok", "date_created": "2026-10-07T10:00:00+00:00"}]}`)
			})
			got, err := c.Instances(context.Background())
			if err != nil || len(got) != 1 || got[0].ID != "ok" {
				t.Fatalf("got %+v, %v", got, err)
			}
			if calls.Load() != 3 {
				t.Errorf("calls = %d, want 3", calls.Load())
			}
		})
	}
}

func TestGivesUpAfterFiveTriesWithTheLastError(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 503, fmt.Sprintf(`{"error": "try %d", "status": 503}`, calls.Add(1)))
	})
	_, err := c.Instances(context.Background())
	if err == nil || err.Error() != "vultr: GET /v2/instances: 503: try 5" {
		t.Errorf("err = %v", err)
	}
	if calls.Load() != 5 {
		t.Errorf("calls = %d, want 5", calls.Load())
	}
}

func TestWaitsDoubleBetweenTries(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 429, `{}`) })
	c.RetryWait = 3 * time.Second
	var waits []time.Duration
	c.wait = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	if _, err := c.Instances(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	want := []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Errorf("waits = %v, want %v", waits, want)
	}
}

func TestOnRetryHearsOfEveryRetry(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			writeJSON(w, 429, `{"error": "slow down"}`)
			return
		}
		writeJSON(w, 200, `{"instances": [], "meta": {"links": {"next": ""}}}`)
	})
	var heard []string
	c.OnRetry = func(err error, wait time.Duration) { heard = append(heard, fmt.Sprintf("%v; %s", err, wait)) }
	if _, err := c.Instances(context.Background()); err != nil {
		t.Fatalf("Instances: %v", err)
	}
	want := []string{
		"vultr: GET /v2/instances: 429: slow down; 1ms",
		"vultr: GET /v2/instances: 429: slow down; 2ms",
	}
	if fmt.Sprint(heard) != fmt.Sprint(want) {
		t.Errorf("OnRetry heard %q, want %q", heard, want)
	}
}

func TestContextEndsARetryWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			time.AfterFunc(50*time.Millisecond, cancel)
		}
		writeJSON(w, 503, `{"error": "down"}`)
	})
	c.RetryWait = time.Hour
	done := make(chan error, 1)
	go func() { _, err := c.Instances(ctx); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || err == nil || !strings.Contains(err.Error(), "503: down") {
			t.Errorf("err = %v, want context.Canceled and the status that caused the retry", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the retry wait ignored the context")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestErrorTextHasMethodPathStatusAndBodyError(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, 403, `{"error": "Unauthorized IP address", "status": 403}`)
	})
	_, err := c.Instances(context.Background())
	if err == nil || err.Error() != "vultr: GET /v2/instances: 403: Unauthorized IP address" {
		t.Errorf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, a 403 must not be retried", calls.Load())
	}
	if err != nil && strings.Contains(err.Error(), testKey) {
		t.Error("the error holds the key")
	}
}

func TestErrorTextFallsBackToTheStatusText(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 403, `not json`) })
	_, err := c.VPCs(context.Background())
	if err == nil || err.Error() != "vultr: GET /v2/vpcs: 403: Forbidden" {
		t.Errorf("err = %v", err)
	}
}

func TestTransportErrorDoesNotHoldTheKey(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c := New(testKey)
	c.BaseURL = srv.URL + "/v2"
	srv.Close()
	_, err := c.Instances(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Error("the error holds the key")
	}
	if !strings.Contains(err.Error(), "GET /v2/instances") {
		t.Errorf("err = %v, want it to name the call", err)
	}
}

func TestDeleteCallsTheRightPath(t *testing.T) {
	cases := map[string]struct {
		path string
		call func(*Client, context.Context, string) error
	}{
		"instance": {"/v2/instances/id%2F1", (*Client).DeleteInstance},
		"vpc":      {"/v2/vpcs/id%2F1", (*Client).DeleteVPC},
		"firewall": {"/v2/firewalls/id%2F1", (*Client).DeleteFirewallGroup},
		"ssh key":  {"/v2/ssh-keys/id%2F1", (*Client).DeleteSSHKey},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var method, path string
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.EscapedPath()
				w.WriteHeader(204)
			})
			if err := tc.call(c, context.Background(), "id/1"); err != nil {
				t.Fatal(err)
			}
			if method != "DELETE" || path != tc.path {
				t.Errorf("request = %s %s, want DELETE %s", method, path, tc.path)
			}
		})
	}
}

func TestDeleteOfAMissingObjectIsDone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 404, `{"error": "gone"}`) })
	if err := c.DeleteInstance(context.Background(), "x"); err != nil {
		t.Errorf("err = %v, want nil on 404", err)
	}
}

func TestDeleteReportsOtherFailures(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 403, `{"error": "no rights"}`) })
	err := c.DeleteVPC(context.Background(), "v-1")
	if err == nil || err.Error() != "vultr: DELETE /v2/vpcs/v-1: 403: no rights" {
		t.Errorf("err = %v", err)
	}
}

func TestDeleteRetriesTransientStatuses(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writeJSON(w, 429, `{"error": "slow"}`)
			return
		}
		w.WriteHeader(204)
	})
	if err := c.DeleteSSHKey(context.Background(), "s-1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}
