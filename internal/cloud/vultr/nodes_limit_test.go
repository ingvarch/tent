package vultr_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// instanceLimit is Vultr's answer to a create that the account's instance limit refuses.
func instanceLimit() *vultr.APIError {
	return vultr.NewAPIError(http.MethodPost, "/v2/instances", 400,
		"Server add failed: You have reached the maximum number of active instances for this account.", 0)
}

// deletedNode creates and deletes the node prod-servers-0 with x's provider, and returns when the delete was done.
func deletedNode(t *testing.T, x *fixture) time.Time {
	t.Helper()
	in := createNode(t, x.p, serverRequest(opA))
	if err := x.p.Delete(t.Context(), in); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	return time.Now()
}

// replacement is the create of the node that takes the place of the one that deletedNode deleted.
func replacement() cloud.CreateRequest { return serverRequest(opB) }

func TestCreateSendsAgainWhileTheLimitStillCountsADeletedNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		deletedNode(t, x)
		creates := countCalls(x.f, "CreateInstance")
		x.f.Fail(t, "CreateInstance", instanceLimit(), 2)

		in := createNode(t, x.p, replacement())

		if n := countCalls(x.f, "CreateInstance") - creates; n != 3 {
			t.Errorf("%d CreateInstance calls, want 3: two refused and one accepted", n)
		}
		if diff := cmp.Diff([]string{in.ID}, instanceIDs(x.f)); diff != "" {
			t.Errorf("instances (-want +got):\n%s", diff)
		}
		// The first refusal is logged once, so that a create that stands still has a reason.
		want := []map[string]string{{
			"level": "INFO", "msg": "the instance limit refused a create right after a delete; sending it again",
			"node": "prod-servers-0",
		}}
		if diff := cmp.Diff(want, logRecords(t, x.log)); diff != "" {
			t.Errorf("log (-want +got):\n%s", diff)
		}
	})
}

func TestCreateWaitsThePollIntervalBetweenSends(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		var sentAt []time.Duration
		var start time.Time
		p := opProvider(&onCreate{Fake: x.f, hook: func() { sentAt = append(sentAt, time.Since(start)) }})
		in := createNode(t, p, serverRequest(opA))
		if err := p.Delete(t.Context(), in); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		x.f.Fail(t, "CreateInstance", instanceLimit(), 1)
		sentAt, start = nil, time.Now()

		createNode(t, p, replacement())

		if len(sentAt) != 2 || sentAt[0] != 0 || sentAt[1] != 5*time.Second {
			t.Errorf("the POSTs went out after %v, want 0 and 5s, the provider's poll interval", sentAt)
		}
	})
}

// onCreate is a fake that runs hook before each CreateInstance call.
type onCreate struct {
	*vultrfake.Fake
	hook func()
}

func (a *onCreate) CreateInstance(ctx context.Context, r *govultr.InstanceCreateReq) (*govultr.Instance, error) {
	a.hook()
	return a.Fake.CreateInstance(ctx, r)
}

func TestCreateReturnsTheLimitAtOnceWithoutADelete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		x.f.Fail(t, "CreateInstance", instanceLimit(), 1)
		start := time.Now()

		_, err := x.p.Create(t.Context(), serverRequest(opA))

		wantLimit(t, err)
		if n := countCalls(x.f, "CreateInstance"); n != 1 {
			t.Errorf("%d CreateInstance calls, want 1", n)
		}
		if got := time.Since(start); got != 0 {
			t.Errorf("Create waited %v, want no wait", got)
		}
	})
}

func TestCreateReturnsTheLimitAtOnceAfterAnOldDelete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		deletedNode(t, x)
		time.Sleep(vultr.LimitSettle)
		creates := countCalls(x.f, "CreateInstance")
		x.f.Fail(t, "CreateInstance", instanceLimit(), 1)
		start := time.Now()

		_, err := x.p.Create(t.Context(), replacement())

		wantLimit(t, err)
		if n := countCalls(x.f, "CreateInstance") - creates; n != 1 {
			t.Errorf("%d CreateInstance calls, want 1", n)
		}
		if got := time.Since(start); got != 0 {
			t.Errorf("Create waited %v, want no wait", got)
		}
	})
}

func TestCreateReturnsTheLimitThatHoldsOnceTheWindowHasPassed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		deleted := deletedNode(t, x)
		x.f.Fail(t, "CreateInstance", instanceLimit(), 1000)

		_, err := x.p.Create(t.Context(), replacement())

		wantLimit(t, err)
		if got := time.Since(deleted); got < vultr.LimitSettle || got >= vultr.LimitSettle+5*time.Second {
			t.Errorf("Create gave up %v after the delete, want %v and less than one poll interval more", got,
				vultr.LimitSettle)
		}
	})
}

func TestCreateAfterTheDeleteOfAGoneNodeOpensNoWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		gone := cloud.Instance{ID: "instance-404", Name: "prod-servers-9"}
		if err := x.p.Delete(t.Context(), gone); err != nil {
			t.Fatalf("Delete of a gone node: %v", err)
		}
		x.f.Fail(t, "CreateInstance", instanceLimit(), 1)

		_, err := x.p.Create(t.Context(), serverRequest(opA))

		wantLimit(t, err)
		if n := countCalls(x.f, "CreateInstance"); n != 1 {
			t.Errorf("%d CreateInstance calls, want 1", n)
		}
	})
}

func TestCreateEndsWithItsContextWhileItWaitsForTheLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		deletedNode(t, x)
		creates := countCalls(x.f, "CreateInstance")
		x.f.Fail(t, "CreateInstance", instanceLimit(), 1000)
		ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
		defer cancel()
		start := time.Now()

		_, err := x.p.Create(ctx, replacement())

		const want = "create node prod-servers-0 of cluster prod: wait to send the create again: " +
			"context deadline exceeded"
		if errText(err) != want {
			t.Errorf("Create = %v, want %q", err, want)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false", err)
		}
		if got := time.Since(start); got != 12*time.Second {
			t.Errorf("Create returned after %v, want 12s, when the context ended", got)
		}
		if n := countCalls(x.f, "CreateInstance") - creates; n != 3 {
			t.Errorf("%d CreateInstance calls, want 3 (at 0, 5 and 10 seconds)", n)
		}
	})
}

func TestCreateReturnsAnotherErrorOfTheCreateAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		deletedNode(t, x)
		creates := countCalls(x.f, "CreateInstance")
		x.f.Fail(t, "CreateInstance", vultr.NewAPIError(http.MethodPost, "/v2/instances", 400, "Invalid plan.", 0), 1)
		start := time.Now()

		_, err := x.p.Create(t.Context(), replacement())

		if !errors.Is(err, vultr.ErrInvalid) {
			t.Errorf("Create = %v, want ErrInvalid", err)
		}
		if n := countCalls(x.f, "CreateInstance") - creates; n != 1 {
			t.Errorf("%d CreateInstance calls, want 1", n)
		}
		if got := time.Since(start); got != 0 {
			t.Errorf("Create waited %v, want no wait", got)
		}
	})
}

// TestCreateAndDeleteRunTogether runs two deletes beside a create that the limit refuses. It fails only under the race
// detector, which make check runs: the provider guards the time of its last delete. What the create returns depends on
// the order of the goroutines, so it is not checked.
func TestCreateAndDeleteRunTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x, _ := newNodesFixture(t, opsKey)
		first := createNode(t, x.p, nodeRequest("prod-servers-0", "servers", v1alpha1.RoleServer, opA))
		second := createNode(t, x.p, nodeRequest("prod-servers-1", "servers", v1alpha1.RoleServer, opB))
		x.f.Fail(t, "CreateInstance", instanceLimit(), 4)
		third := nodeRequest("prod-servers-2", "servers", v1alpha1.RoleServer, opC)
		var wg sync.WaitGroup

		for _, in := range []cloud.Instance{first, second} {
			wg.Go(func() {
				if err := x.p.Delete(t.Context(), in); err != nil {
					t.Errorf("Delete %s: %v", in.Name, err)
				}
			})
		}
		wg.Go(func() { _, _ = x.p.Create(t.Context(), third) })
		wg.Wait()
	})
}

// wantLimit checks that err is the limit error with Vultr's answer and the hint.
func wantLimit(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, vultr.ErrLimitReached) {
		t.Fatalf("Create = %v, want ErrLimitReached", err)
	}
	for _, part := range []string{"maximum number of active instances", "an account limit can be raised"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("the error %q lacks %q", err, part)
		}
	}
}
