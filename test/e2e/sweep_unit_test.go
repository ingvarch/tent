package e2e

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/test/e2e/janitor/janitortest"
	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

var sweepNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func TestSweepDeletesOnlyClustersOlderThanThreeHours(t *testing.T) {
	api := &janitortest.API{Machines: []vultrapi.Instance{
		{ID: "i-old", Tags: []string{"tent/cluster=e2e-old000-2404"}, Created: sweepNow.Add(-3*time.Hour - time.Minute)},
		{ID: "i-new", Tags: []string{"tent/cluster=e2e-new000-2404"}, Created: sweepNow.Add(-2*time.Hour - 59*time.Minute)},
	}}
	if err := sweepLeftovers(t.Context(), api, &bytes.Buffer{}, sweepNow); err != nil {
		t.Fatalf("sweepLeftovers: %v", err)
	}
	if !slices.Contains(api.Calls, "delete-instance i-old") {
		t.Errorf("calls = %q, want the old instance deleted", api.Calls)
	}
	if slices.Contains(api.Calls, "delete-instance i-new") {
		t.Errorf("calls = %q, the young instance was deleted", api.Calls)
	}
}

func TestSweepDeletesNothingWhenEverythingIsYoung(t *testing.T) {
	created := sweepNow.Add(-time.Hour)
	api := &janitortest.API{
		Machines: []vultrapi.Instance{{ID: "i1", Tags: []string{"tent/cluster=e2e-new000-2404"}, Created: created}},
		Keys:     []vultrapi.SSHKey{{ID: "k1", Name: "tent:cluster=e2e-new000-2404;kind=ssh", Created: created}},
	}
	if err := sweepLeftovers(t.Context(), api, &bytes.Buffer{}, sweepNow); err != nil {
		t.Fatalf("sweepLeftovers: %v", err)
	}
	for _, call := range api.Calls {
		if strings.HasPrefix(call, "delete-") {
			t.Errorf("call %q: a young cluster was touched", call)
		}
	}
}

func TestSweepReportsAFailedDelete(t *testing.T) {
	boom := errors.New("boom")
	api := &janitortest.API{
		Keys: []vultrapi.SSHKey{{
			ID: "k1", Name: "tent:cluster=e2e-old000-2404;kind=ssh", Created: sweepNow.Add(-4 * time.Hour),
		}},
		Err: map[string]error{"delete-ssh-key k1": boom},
	}
	err := sweepLeftovers(t.Context(), api, &bytes.Buffer{}, sweepNow)
	if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "delete leftovers: ") {
		t.Errorf("error = %v, want one wrapping %v after %q", err, boom, "delete leftovers: ")
	}
}

func TestSweepReportsAFailedLookup(t *testing.T) {
	boom := errors.New("boom")
	api := &janitortest.API{Err: map[string]error{"list-vpcs": boom}}
	err := sweepLeftovers(t.Context(), api, &bytes.Buffer{}, sweepNow)
	if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "look for leftovers: ") {
		t.Errorf("error = %v, want one wrapping %v after %q", err, boom, "look for leftovers: ")
	}
}
