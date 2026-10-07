// Package vultrapi is a small client of the Vultr API for the E2E suite and its janitor. It reads the lists the
// suite needs and deletes objects; the key goes only into the Authorization header and never into an error.
package vultrapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	defaultBaseURL = "https://api.vultr.com/v2"
	maxTries       = 5
	pageSize       = "500"
	maxBody        = 16 << 20
)

// Client calls the Vultr API with one key; make one with New. Tests point BaseURL at a server and shorten RetryWait.
type Client struct {
	Key     string
	BaseURL string
	HTTP    *http.Client
	// RetryWait is the wait before the second try of a call; it doubles before each later try.
	RetryWait time.Duration
	// OnRetry, when set, hears of each failed try that is tried again: its error and the wait before the next.
	OnRetry func(err error, wait time.Duration)

	wait func(context.Context, time.Duration) error
}

// New returns a client of the Vultr API with a 30 s timeout per request and a first retry wait of one second.
func New(key string) *Client {
	return &Client{
		Key:       key,
		BaseURL:   defaultBaseURL,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		RetryWait: time.Second,
		wait:      sleep,
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Instance is a Vultr instance. Status, PowerStatus and ServerStatus are Vultr's three states of it.
type Instance struct {
	ID           string
	Label        string
	Region       string
	MainIP       string
	Tags         []string
	Created      time.Time
	Status       string
	PowerStatus  string
	ServerStatus string
}

// VPC is a Vultr VPC.
type VPC struct {
	ID          string
	Description string
	Region      string
	Created     time.Time
}

// FirewallGroup is a Vultr firewall group.
type FirewallGroup struct {
	ID          string
	Description string
	Created     time.Time
}

// SSHKey is a Vultr SSH key.
type SSHKey struct {
	ID      string
	Name    string
	Created time.Time
}

// Instances lists every instance of the account.
func (c *Client) Instances(ctx context.Context) ([]Instance, error) {
	type wire struct {
		ID           string   `json:"id"`
		Label        string   `json:"label"`
		Region       string   `json:"region"`
		MainIP       string   `json:"main_ip"`
		Tags         []string `json:"tags"`
		Created      string   `json:"date_created"`
		Status       string   `json:"status"`
		PowerStatus  string   `json:"power_status"`
		ServerStatus string   `json:"server_status"`
	}
	return listAll(ctx, c, "/instances", "instances", func(w wire) (Instance, error) {
		created, err := parseCreated("instance", w.ID, w.Created)
		return Instance{
			ID: w.ID, Label: w.Label, Region: w.Region, MainIP: w.MainIP, Tags: w.Tags, Created: created,
			Status: w.Status, PowerStatus: w.PowerStatus, ServerStatus: w.ServerStatus,
		}, err
	})
}

// VPCs lists every VPC of the account.
func (c *Client) VPCs(ctx context.Context) ([]VPC, error) {
	type wire struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		Region      string `json:"region"`
		Created     string `json:"date_created"`
	}
	return listAll(ctx, c, "/vpcs", "vpcs", func(w wire) (VPC, error) {
		created, err := parseCreated("vpc", w.ID, w.Created)
		return VPC{ID: w.ID, Description: w.Description, Region: w.Region, Created: created}, err
	})
}

// FirewallGroups lists every firewall group of the account.
func (c *Client) FirewallGroups(ctx context.Context) ([]FirewallGroup, error) {
	type wire struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		Created     string `json:"date_created"`
	}
	return listAll(ctx, c, "/firewalls", "firewall_groups", func(w wire) (FirewallGroup, error) {
		created, err := parseCreated("firewall group", w.ID, w.Created)
		return FirewallGroup{ID: w.ID, Description: w.Description, Created: created}, err
	})
}

// SSHKeys lists every SSH key of the account.
func (c *Client) SSHKeys(ctx context.Context) ([]SSHKey, error) {
	type wire struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Created string `json:"date_created"`
	}
	return listAll(ctx, c, "/ssh-keys", "ssh_keys", func(w wire) (SSHKey, error) {
		created, err := parseCreated("ssh key", w.ID, w.Created)
		return SSHKey{ID: w.ID, Name: w.Name, Created: created}, err
	})
}

// DeleteInstance deletes an instance. An instance that is already gone counts as deleted.
func (c *Client) DeleteInstance(ctx context.Context, id string) error {
	return c.delete(ctx, "/instances/"+url.PathEscape(id))
}

// DeleteVPC deletes a VPC. A VPC that is already gone counts as deleted.
func (c *Client) DeleteVPC(ctx context.Context, id string) error {
	return c.delete(ctx, "/vpcs/"+url.PathEscape(id))
}

// DeleteFirewallGroup deletes a firewall group. A group that is already gone counts as deleted.
func (c *Client) DeleteFirewallGroup(ctx context.Context, id string) error {
	return c.delete(ctx, "/firewalls/"+url.PathEscape(id))
}

// DeleteSSHKey deletes an SSH key. A key that is already gone counts as deleted.
func (c *Client) DeleteSSHKey(ctx context.Context, id string) error {
	return c.delete(ctx, "/ssh-keys/"+url.PathEscape(id))
}

// parseCreated reads the creation date of an object; Vultr writes RFC 3339 with an offset.
func parseCreated(kind, id, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("vultr: %s %s: creation date %q: %w", kind, id, value, err)
	}
	return t, nil
}

// listAll reads every page of a list and turns each element of the array under key into a T. It fails when a
// cursor comes back that it has already followed.
func listAll[W, T any](ctx context.Context, c *Client, path, key string, convert func(W) (T, error)) ([]T, error) {
	var out []T
	cursor := ""
	read := map[string]bool{}
	for {
		q := url.Values{"per_page": {pageSize}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		body, err := c.call(ctx, http.MethodGet, path, q, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var meta struct {
			Meta struct {
				Links struct {
					Next string `json:"next"`
				} `json:"links"`
			} `json:"meta"`
		}
		var lists map[string]json.RawMessage
		if err := json.Unmarshal(body, &meta); err != nil {
			return nil, fmt.Errorf("vultr: GET /v2%s: decode: %w", path, err)
		}
		if err := json.Unmarshal(body, &lists); err != nil {
			return nil, fmt.Errorf("vultr: GET /v2%s: decode: %w", path, err)
		}
		raw, ok := lists[key]
		if !ok {
			return nil, fmt.Errorf("vultr: GET /v2%s: the answer has no %q", path, key)
		}
		var items []W
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("vultr: GET /v2%s: decode %s: %w", path, key, err)
		}
		for _, w := range items {
			v, err := convert(w)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		cursor = meta.Meta.Links.Next
		if cursor == "" {
			return out, nil
		}
		if read[cursor] {
			return nil, fmt.Errorf("vultr: GET /v2%s: the cursor %s came back", path, cursor)
		}
		read[cursor] = true
	}
}

func (c *Client) delete(ctx context.Context, path string) error {
	_, err := c.call(ctx, http.MethodDelete, path, nil, http.StatusNoContent, http.StatusNotFound)
	return err
}

// call sends a request and returns the body of a response whose status is in ok. It tries again on 429 and 5xx, up
// to maxTries in all, and returns the last error.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, ok ...int) ([]byte, error) {
	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	label := method + " /v2" + path
	wait := c.RetryWait
	for try := 1; ; try++ {
		status, body, err := c.send(ctx, method, target, label)
		if err != nil {
			return nil, err
		}
		for _, s := range ok {
			if status == s {
				return body, nil
			}
		}
		failure := statusError(label, status, body)
		if (status != http.StatusTooManyRequests && status < 500) || try == maxTries {
			return nil, failure
		}
		if c.OnRetry != nil {
			c.OnRetry(failure, wait)
		}
		if err := c.wait(ctx, wait); err != nil {
			return nil, fmt.Errorf("%w; waiting to try again: %w", failure, err)
		}
		wait *= 2
	}
}

func (c *Client) send(ctx context.Context, method, target, label string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("vultr: %s: %w", label, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("vultr: %s: %w", label, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, fmt.Errorf("vultr: %s: read body: %w", label, err)
	}
	return resp.StatusCode, body, nil
}

// statusError reads Vultr's error body, {"error": "...", "status": N}, and falls back to the status text.
func statusError(label string, status int, body []byte) error {
	var parsed struct {
		Error string `json:"error"`
	}
	text := http.StatusText(status)
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
		text = parsed.Error
	}
	return fmt.Errorf("vultr: %s: %d: %s", label, status, text)
}
