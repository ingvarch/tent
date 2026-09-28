package vultrfake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vultr/govultr/v3"

	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
)

// pass is a hook that carries every call out as the fake does without one.
func pass(ctx context.Context, _ vultrfake.Call, next func(context.Context) error) error {
	return next(ctx)
}

func TestHookSeesEveryCall(t *testing.T) {
	for _, c := range apiCalls {
		t.Run(c.name, func(t *testing.T) {
			f := newSeeded(t)
			var seen []vultrfake.Call
			f.SetHook(func(ctx context.Context, call vultrfake.Call, next func(context.Context) error) error {
				seen = append(seen, call)
				return next(ctx)
			})
			if err := c.call(t.Context(), f); err != nil {
				t.Errorf("with a hook that carries it out: %v, want success", err)
			}
			if len(seen) != 1 || seen[0].Name != c.name {
				t.Errorf("the hook saw %v, want one %s", seen, c.name)
			}
			if diff := cmp.Diff(f.Calls(), seen); diff != "" {
				t.Errorf("the hook saw the calls (-logged +seen):\n%s", diff)
			}
		})
	}
}

func TestHookDecidesTheOutcome(t *testing.T) {
	f := newFake()
	ctx := t.Context()
	req := &govultr.VPCReq{Region: "ams", Description: "tent:cluster=prod;kind=vpc;op=1"}
	vpc := govultr.VPC{ID: "vpc-1", Region: "ams", Description: req.Description, DateCreated: date}

	// A hook that carries the call out and then fails it loses the answer: the VPC exists, the call returns no value.
	f.SetHook(func(ctx context.Context, _ vultrfake.Call, next func(context.Context) error) error {
		if err := next(ctx); err != nil {
			t.Errorf("next: %v", err)
		}
		return errBoom
	})
	if v, err := f.CreateVPC(ctx, req); !errors.Is(err, errBoom) || v != nil {
		t.Errorf("CreateVPC = %+v, %v; want %v alone", v, err, errBoom)
	}
	if diff := cmp.Diff([]govultr.VPC{vpc}, f.VPCs()); diff != "" {
		t.Errorf("VPCs (-want +got):\n%s", diff)
	}

	// Calling next again after the context ended fails as the client does for an ended context, and reaches nothing.
	cctx, cancel := context.WithCancel(ctx)
	f.SetHook(func(ctx context.Context, _ vultrfake.Call, next func(context.Context) error) error {
		if err := next(ctx); err != nil {
			t.Errorf("next: %v", err)
		}
		cancel()
		return next(ctx)
	})
	_, err := f.CreateVPC(cctx, req)
	wantAPIError(t, err, vultr.ErrUnavailable, "vultr: POST /v2/vpcs: context canceled")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(%v, context.Canceled) = false", err)
	}
	if n := len(f.VPCs()); n != 2 {
		t.Errorf("%d VPCs, want 2: the first next created one", n)
	}

	// Faults apply within next.
	f.SetHook(pass)
	f.Fail(t, "CreateVPC", errBoom, 1)
	if _, err := f.CreateVPC(ctx, req); !errors.Is(err, errBoom) {
		t.Errorf("CreateVPC = %v, want the fault's %v", err, errBoom)
	}

	// Without the hook, the call is the fake's alone.
	f.SetHook(nil)
	if v, err := f.CreateVPC(ctx, req); err != nil || v.ID != "vpc-3" {
		t.Errorf("CreateVPC without the hook = %+v, %v; want vpc-3", v, err)
	}
	create := vultrfake.Call{Name: "CreateVPC", Arg: req.Description}
	wantCalls(t, f, create, create, create, create)
}
