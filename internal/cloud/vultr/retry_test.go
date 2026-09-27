package vultr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/engine"
)

// failOnce is a task that plans a create whose first attempt fails with err and whose next ones succeed.
type failOnce struct {
	err      error
	attempts int
}

func (f *failOnce) Key() engine.Key    { return engine.Key{Kind: "test.Object", Name: "one"} }
func (f *failOnce) Deps() []engine.Key { return nil }

func (f *failOnce) Plan(context.Context, *engine.Env) (engine.Change, error) {
	return engine.Change{Action: engine.Create}, nil
}

func (f *failOnce) Apply(context.Context, *engine.Env, engine.Change) error {
	if f.attempts++; f.attempts == 1 {
		return f.err
	}
	return nil
}

func (f *failOnce) Delete(context.Context, *engine.Env, engine.Object) error { return nil }

// applyFailingOnce applies a change whose first attempt fails with err, and returns the events that the engine sends
// for it. The engine's clock is synctest's, so its waits take no time.
func applyFailingOnce(t *testing.T, err error) []engine.Event {
	t.Helper()
	var events []engine.Event
	synctest.Test(t, func(t *testing.T) {
		task := &failOnce{err: err}
		p, perr := engine.NewPlan(t.Context(), []engine.Task{task}, []engine.Kind{{Name: "test.Object", Deleter: task}},
			nil)
		if perr != nil {
			t.Fatalf("NewPlan: %v", perr)
		}
		_ = p.Apply(t.Context(), engine.ApplyOptions{OnEvent: func(e engine.Event) { events = append(events, e) }})
	})
	return events
}

// eventOf returns the first of events of type typ, and whether there is one.
func eventOf(events []engine.Event, typ engine.EventType) (engine.Event, bool) {
	i := slices.IndexFunc(events, func(e engine.Event) bool { return e.Type == typ })
	if i < 0 {
		return engine.Event{}, false
	}
	return events[i], true
}

// wantSameError checks that got matches err and prints as err.
func wantSameError(t *testing.T, got, err error) {
	t.Helper()
	if !errors.Is(got, err) {
		t.Errorf("errors.Is(%v, %v) = false", got, err)
	}
	if got.Error() != err.Error() {
		t.Errorf("the error prints %q, want %q", got, err)
	}
}

func TestMarkRetryable(t *testing.T) {
	throttled := NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusTooManyRequests, "Rate limit exceeded",
		7*time.Second)
	for _, tc := range []struct {
		name       string
		err        error
		idempotent bool
		after      time.Duration // the wait the engine must choose; its first backoff, at most a second, when zero
	}{
		{name: "rate limited", err: throttled, after: 7 * time.Second},
		{name: "rate limited, idempotent", err: throttled, idempotent: true, after: 7 * time.Second},
		{name: "rate limited, wrapped", err: fmt.Errorf("create VPC: %w", throttled), after: 7 * time.Second},
		{
			name: "rate limited without Retry-After",
			err:  NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusTooManyRequests, "Rate limit exceeded", 0),
		},
		{name: "in use", err: NewAPIError(http.MethodDelete, "/v2/vpcs/vpc-1", http.StatusConflict, "busy", 0)},
		{
			name:       "unavailable, idempotent",
			err:        NewAPIError(http.MethodGet, "/v2/vpcs", http.StatusServiceUnavailable, "", 0),
			idempotent: true,
		},
		{
			name:       "no answer, idempotent",
			err:        NewNoAnswerError(http.MethodDelete, "/v2/vpcs/vpc-1", nil),
			idempotent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := markRetryable(tc.err, tc.idempotent)
			wantSameError(t, got, tc.err)
			e, retried := eventOf(applyFailingOnce(t, got), engine.Retrying)
			switch {
			case !retried:
				t.Errorf("the engine did not retry %v", got)
			case tc.after > 0 && e.Wait != tc.after:
				t.Errorf("the engine waited %v before the retry, want %v", e.Wait, tc.after)
			case tc.after == 0 && e.Wait > time.Second:
				t.Errorf("the engine waited %v before the retry, want its first backoff, at most 1s", e.Wait)
			}
		})
	}
}

func TestMarkRetryableNil(t *testing.T) {
	for _, idempotent := range []bool{false, true} {
		if got := markRetryable(nil, idempotent); got != nil {
			t.Errorf("markRetryable(nil, %t) = %v, want nil", idempotent, got)
		}
	}
}

func TestMarkRetryablePermanent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		idempotent bool
	}{
		{"unavailable, not idempotent", NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusBadGateway, "", 0), false},
		{"no answer, not idempotent", NewNoAnswerError(http.MethodPost, "/v2/vpcs", nil), false},
		{"not found", NewAPIError(http.MethodDelete, "/v2/vpcs/vpc-1", http.StatusNotFound, "", 0), true},
		{"invalid", NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusBadRequest, "Invalid region.", 0), true},
		{"forbidden", NewAPIError(http.MethodGet, "/v2/vpcs", http.StatusUnauthorized, "", 0), true},
		{
			"limit reached",
			NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusBadRequest,
				"You have reached the maximum number of VPC networks in this region.", 0),
			true,
		},
		{"an error of another kind", errors.New("boom"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := markRetryable(tc.err, tc.idempotent)
			wantSameError(t, got, tc.err)
			events := applyFailingOnce(t, got)
			if e, retried := eventOf(events, engine.Retrying); retried {
				t.Errorf("the engine retried after %v", e.Err)
			}
			if e, failed := eventOf(events, engine.Failed); !failed || !errors.Is(e.Err, tc.err) {
				t.Errorf("the change failed: %t, with %v; want it to fail with %v", failed, e.Err, tc.err)
			}
		})
	}
}

func TestWithLimitHint(t *testing.T) {
	const hint = " (an account limit can be raised in the Vultr console under Billing, Limits)"
	limit := NewAPIError(http.MethodPost, "/v2/instances", http.StatusBadRequest,
		"You have reached the maximum number of instances.", 0)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"limit reached", limit},
		{"limit reached, wrapped", fmt.Errorf("create instance prod-workers-0: %w", limit)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withLimitHint(tc.err)
			if want := tc.err.Error() + hint; got.Error() != want {
				t.Errorf("withLimitHint prints %q, want %q", got, want)
			}
			if !errors.Is(got, ErrLimitReached) {
				t.Errorf("errors.Is(%v, ErrLimitReached) = false", got)
			}
			if e := (*APIError)(nil); !errors.As(got, &e) || e != limit {
				t.Errorf("errors.As(%v, *APIError) = %v, want the API error", got, e)
			}
		})
	}
}

func TestWithLimitHintOtherErrors(t *testing.T) {
	if got := withLimitHint(nil); got != nil {
		t.Errorf("withLimitHint(nil) = %v, want nil", got)
	}
	for _, err := range []error{
		errors.New("boom"),
		NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusTooManyRequests, "Rate limit exceeded", 0),
		NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusBadRequest, "Invalid region.", 0),
		// Vultr does not raise the limits of objects on request.
		&objectLimitError{limit: "ams may already have 5 VPCs, the most Vultr allows in a region", err: vpcLimit},
		fmt.Errorf("create VPC: %w", &objectLimitError{limit: "5 VPCs per region", err: vpcLimit}),
	} {
		wantSameError(t, withLimitHint(err), err)
	}
}

// vpcLimit is Vultr's answer to a VPC create in a region that has 5.
var vpcLimit = NewAPIError(http.MethodPost, "/v2/vpcs", http.StatusBadRequest,
	"You have reached the maximum number of VPC networks in this region.", 0)

func TestObjectLimitError(t *testing.T) {
	const limit = "ams may already have 5 VPCs, the most Vultr allows in a region"
	err := &objectLimitError{limit: limit, err: vpcLimit}
	if want := limit + ": " + vpcLimit.Error(); err.Error() != want {
		t.Errorf("the error prints %q, want %q", err, want)
	}
	if !errors.Is(err, ErrLimitReached) {
		t.Errorf("errors.Is(%v, ErrLimitReached) = false", err)
	}
	if e := (*APIError)(nil); !errors.As(err, &e) || e != vpcLimit {
		t.Errorf("errors.As(%v, *APIError) = %v, want Vultr's answer", err, e)
	}
}
