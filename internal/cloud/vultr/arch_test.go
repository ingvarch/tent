package vultr_test

import (
	"context"
	"testing"

	"github.com/ingvarch/tent/internal/cloud"
)

func TestProviderArch(t *testing.T) {
	x := newFixture()
	var p cloud.Provider = x.p
	const want = "amd64"
	for _, plan := range []string{"vc2-2c-4gb", "vhf-1c-1gb", "no-such-plan"} {
		got, err := p.Arch(context.Background(), plan)
		if err != nil {
			t.Fatalf("Arch(%q): %v", plan, err)
		}
		if got != want {
			t.Errorf("Arch(%q) = %q, want %q", plan, got, want)
		}
	}
	if calls := x.f.Calls(); len(calls) != 0 {
		t.Errorf("Arch made the Vultr calls %v, want none", calls)
	}
}
