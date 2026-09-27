package vultr

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/engine"
)

// Errors of the calls in the tests of createWithOp.
var (
	errLost      = NewNoAnswerError(http.MethodPost, "/v2/vpcs", nil)
	errThrottled = NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusTooManyRequests, "Rate limit exceeded",
		7*time.Second)
	errListFailed = NewAPIError(http.MethodGet, "/v2/vpcs", http.StatusInternalServerError, "Internal error", 0)
)

// findResult is what a find call gives in the tests of createWithOp.
type findResult int

const (
	notListed findResult = iota // the object is not listed
	listed                      // the object is listed
	listFails                   // the list fails with errListFailed
)

// IDs that the object gets in the tests of createWithOp: from a create's answer, and from a search.
const (
	createdID = "obj-created"
	foundID   = "obj-found"
)

// opCloud stands in for the cloud in the tests of createWithOp. Each find and create call takes the next of its
// results, and calls logs them.
type opCloud struct {
	t       *testing.T
	finds   []findResult
	creates []error // the error of each create call; nil creates the object
	calls   []string
}

func (c *opCloud) find(context.Context) (string, bool, error) {
	c.calls = append(c.calls, "find")
	if len(c.finds) == 0 {
		c.t.Fatal("createWithOp searched more often than the test expects")
	}
	r := c.finds[0]
	c.finds = c.finds[1:]
	switch r {
	case listed:
		return foundID, true, nil
	case listFails:
		return "", false, errListFailed
	}
	return "", false, nil
}

func (c *opCloud) create(context.Context) (string, error) {
	c.calls = append(c.calls, "create")
	if len(c.creates) == 0 {
		c.t.Fatal("createWithOp created more often than the test expects")
	}
	err := c.creates[0]
	c.creates = c.creates[1:]
	if err != nil {
		return "", err
	}
	return createdID, nil
}

// takeCalls returns the calls logged since the last takeCalls, such as "create find".
func (c *opCloud) takeCalls() string {
	s := strings.Join(c.calls, " ")
	c.calls = nil
	return s
}

// opAttempt is one attempt of createWithOp and what it must do.
type opAttempt struct {
	calls string // the calls it makes, such as "create find"
	id    string // the id it returns; empty when it fails
	// fail is what the error of a failing attempt matches; the engine must retry the error.
	fail error
	wait time.Duration // the wait the engine must choose before the retry, when it is set
	msg  string        // what the error prints, when it is set
}

func TestCreateWithOp(t *testing.T) {
	const lostNotListed = "vultr: POST /v2/vpcs: no answer; no object with its operation id is listed yet"
	for _, tc := range []struct {
		name     string
		finds    []findResult
		creates  []error
		attempts []opAttempt
	}{
		{
			name:     "created",
			creates:  []error{nil},
			attempts: []opAttempt{{calls: "create", id: createdID}},
		},
		{
			name:     "a lost answer, listed at once",
			finds:    []findResult{listed},
			creates:  []error{errLost},
			attempts: []opAttempt{{calls: "create find", id: foundID}},
		},
		{
			name:    "a lost answer, listed on the next attempt",
			finds:   []findResult{notListed, listed},
			creates: []error{errLost},
			attempts: []opAttempt{
				{calls: "create find", fail: errLost, msg: lostNotListed},
				{calls: "find", id: foundID},
			},
		},
		{
			// Each create after the first follows a search that found nothing.
			name:    "a lost answer, not listed twice",
			finds:   []findResult{notListed, notListed, notListed, listed},
			creates: []error{errLost, errLost},
			attempts: []opAttempt{
				{calls: "create find", fail: errLost},
				{calls: "find create find", fail: errLost},
				{calls: "find", id: foundID},
			},
		},
		{
			// A create after an empty search searches once more: the lost copy may be listed by then.
			name:    "a lost answer, not listed, then created",
			finds:   []findResult{notListed, notListed, notListed},
			creates: []error{errLost, nil},
			attempts: []opAttempt{
				{calls: "create find", fail: errLost},
				{calls: "find create find", id: createdID},
			},
		},
		{
			name:    "a lost answer, not listed, then created and listed",
			finds:   []findResult{notListed, notListed, listed},
			creates: []error{errLost, nil},
			attempts: []opAttempt{
				{calls: "create find", fail: errLost},
				{calls: "find create find", id: foundID},
			},
		},
		{
			// The create succeeded, so a failed search after it returns the created id.
			name:    "a lost answer, not listed, then created, and the search after it fails",
			finds:   []findResult{notListed, notListed, listFails},
			creates: []error{errLost, nil},
			attempts: []opAttempt{
				{calls: "create find", fail: errLost},
				{calls: "find create find", id: createdID},
			},
		},
		{
			name:    "the search after a lost answer fails",
			finds:   []findResult{listFails, listed},
			creates: []error{errLost},
			attempts: []opAttempt{
				{
					calls: "create find", fail: errListFailed,
					msg: "search by operation id after a create without an answer: " + errListFailed.Error(),
				},
				{calls: "find", id: foundID},
			},
		},
		{
			name:    "the search of a later attempt fails",
			finds:   []findResult{notListed, listFails, listed},
			creates: []error{errLost},
			attempts: []opAttempt{
				{calls: "create find", fail: errLost},
				{calls: "find", fail: errListFailed},
				{calls: "find", id: foundID},
			},
		},
		{
			// Vultr did not carry out a throttled create, so the next attempt creates without a search.
			name:    "rate limited",
			creates: []error{errThrottled, nil},
			attempts: []opAttempt{
				{calls: "create", fail: errThrottled, wait: 7 * time.Second},
				{calls: "create", id: createdID},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &opCloud{t: t, finds: tc.finds, creates: tc.creates}
			var s opState
			for i, a := range tc.attempts {
				id, err := createWithOp(t.Context(), &s, c.find, c.create)
				if got := c.takeCalls(); got != a.calls {
					t.Errorf("attempt %d made the calls %q, want %q", i+1, got, a.calls)
				}
				if id != a.id {
					t.Errorf("attempt %d returned the id %q, want %q", i+1, id, a.id)
				}
				checkOpError(t, i+1, err, a)
			}
		})
	}
}

// checkOpError checks the error of attempt n of createWithOp against a.
func checkOpError(t *testing.T, n int, err error, a opAttempt) {
	t.Helper()
	if a.fail == nil {
		if err != nil {
			t.Errorf("attempt %d failed with %v, want success", n, err)
		}
		return
	}
	if !errors.Is(err, a.fail) {
		t.Errorf("attempt %d failed with %v, want an error that matches %v", n, err, a.fail)
		return
	}
	if a.msg != "" && err.Error() != a.msg {
		t.Errorf("attempt %d failed with %q, want %q", n, err, a.msg)
	}
	e, retried := eventOf(applyFailingOnce(t, err), engine.Retrying)
	switch {
	case !retried:
		t.Errorf("the engine does not retry the error of attempt %d: %v", n, err)
	case a.wait > 0 && e.Wait != a.wait:
		t.Errorf("the engine waits %v after attempt %d, want %v", e.Wait, n, a.wait)
	}
}

// TestCreateWithOpReturnsTheKeptCopy checks a create that follows a search that found nothing, while the copy of a
// lost create was not listed yet: the id returned is that of the copy that the inventory keeps, the older one.
func TestCreateWithOpReturnsTheKeptCopy(t *testing.T) {
	const marker = "tent:cluster=prod;kind=ssh-key;fp=0a1b2c3d;op=op-1"
	lost := govultr.SSHKey{ID: "key-lost", Name: marker, DateCreated: "2026-09-27T10:00:00+00:00"}
	created := govultr.SSHKey{ID: "key-created", Name: marker, DateCreated: "2026-09-27T10:00:05+00:00"}
	var keys []govultr.SSHKey // what the list shows
	list := func(context.Context) ([]govultr.SSHKey, error) { return slices.Clone(keys), nil }
	creates := 0
	create := func(context.Context) (string, error) {
		if creates++; creates == 1 {
			return "", errLost // Vultr created lost, but does not list it yet
		}
		keys = []govultr.SSHKey{created, lost}
		return created.ID, nil
	}
	find := opFinder(list, sshKeyType, "prod", "op-1")
	var s opState
	if _, err := createWithOp(t.Context(), &s, find, create); !errors.Is(err, errLost) {
		t.Fatalf("the first attempt failed with %v, want %v", err, errLost)
	}
	id, err := createWithOp(t.Context(), &s, find, create)
	if err != nil || id != lost.ID {
		t.Errorf("the second attempt = %q, %v; want %q", id, err, lost.ID)
	}
}

func TestCreateWithOpPermanentErrors(t *testing.T) {
	invalid := NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusBadRequest, "Invalid region.", 0)
	limit := NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusBadRequest,
		"You have reached the maximum number of VPC networks in this region.", 0)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"invalid", invalid, invalid.Error()},
		{"limit reached", limit, limit.Error() + " (" + limitHint + ")"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &opCloud{t: t, creates: []error{tc.err}}
			id, err := createWithOp(t.Context(), &opState{}, c.find, c.create)
			if got := c.takeCalls(); got != "create" {
				t.Errorf("createWithOp made the calls %q, want %q", got, "create")
			}
			if id != "" || !errors.Is(err, tc.err) || err.Error() != tc.want {
				t.Fatalf("createWithOp = %q, %v; want the error %q", id, err, tc.want)
			}
			if e, retried := eventOf(applyFailingOnce(t, err), engine.Retrying); retried {
				t.Errorf("the engine retried %v", e.Err)
			}
		})
	}
}

func TestFindByOp(t *testing.T) {
	const marker = "tent:cluster=prod;kind=ssh-key;fp=0a1b2c3d;op=op-1"
	keys := []govultr.SSHKey{
		{ID: "other-op", Name: "tent:cluster=prod;kind=ssh-key;fp=0a1b2c3d;op=op-2"},
		{ID: "other-cluster", Name: "tent:cluster=staging;kind=ssh-key;fp=0a1b2c3d;op=op-1"},
		{ID: "other-kind", Name: "tent:cluster=prod;kind=vpc;op=op-1"},
		{ID: "no-marker", Name: "op=op-1"},
		// The inventory skips a marker without an 8-digit fp, so the search does too.
		{
			ID: "short-fp", Name: "tent:cluster=prod;kind=ssh-key;fp=0a1b;op=op-1",
			DateCreated: "2026-09-01T10:00:00+00:00",
		},
		{ID: "newer", Name: marker, DateCreated: "2026-09-27T10:00:00+00:00"},
		{ID: "older", Name: marker, DateCreated: "2026-09-20T10:00:00+00:00"},
	}
	// Of two copies, the one the inventory keeps.
	if got, ok := findByOp(keys, sshKeyType, "prod", "op-1"); !ok || got.ID != "older" {
		t.Errorf("findByOp(op-1) = %q, %t; want older", got.ID, ok)
	}
	if got, ok := findByOp(keys, sshKeyType, "prod", "op-3"); ok {
		t.Errorf("findByOp(op-3) = %q, want none", got.ID)
	}
}

func TestDeleted(t *testing.T) {
	for _, err := range []error{nil, NewAPIError(http.MethodDelete, "/v2/vpcs/vpc-1", http.StatusNotFound, "", 0)} {
		if got := deleted(err); got != nil {
			t.Errorf("deleted(%v) = %v, want nil", err, got)
		}
	}
	unavailable := NewAPIError(http.MethodDelete, "/v2/vpcs/vpc-1", http.StatusServiceUnavailable, "", 0)
	got := deleted(unavailable)
	wantSameError(t, got, unavailable)
	if _, retried := eventOf(applyFailingOnce(t, got), engine.Retrying); !retried {
		t.Errorf("the engine does not retry %v", got)
	}
	forbidden := NewAPIError(http.MethodDelete, "/v2/vpcs/vpc-1", http.StatusForbidden, "", 0)
	got = deleted(forbidden)
	wantSameError(t, got, forbidden)
	if e, retried := eventOf(applyFailingOnce(t, got), engine.Retrying); retried {
		t.Errorf("the engine retried %v", e.Err)
	}
}
