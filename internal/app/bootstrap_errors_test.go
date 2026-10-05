package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// errPermanent is an error of a Nomad call that no other server answers differently.
var errPermanent = errors.New("test: permission denied")

// hintText is the part of the error of a leader wait that names the access setting.
const hintText = "check spec.access.api"

// seedServer adds a ready server of the test cluster to f that has no private address, and has the public address
// public unless that is empty. It copies a server that a first build makes in a fake of its own.
func seedServer(t *testing.T, f *vultrfake.Fake, name, public string) {
	t.Helper()
	svc, from, _ := newRelease(t)
	mustUpdate(t, svc)
	in := from.Instances()[0]
	in.ID, in.Hostname, in.Label, in.MainIP = "", name, name, public
	f.AddInstance(t, in)
}

// TestUpdateNamesAFailedNomadCall fails the bootstrap and an intro token, with an error that the call returns and with
// the end of the run's context. The error of the run names the call and keeps the cause; the progress of a failed step
// shows the cause alone.
func TestUpdateNamesAFailedNomadCall(t *testing.T) {
	for _, tc := range []struct{ call, prefix string }{
		{"Bootstrap", "bootstrap the ACL system: "},
		{"IntroToken", "intro token for node prod-workers-0: "},
	} {
		t.Run(tc.call+" fails", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _, w := newRelease(t)
				w.Fail(t, tc.call, errPermanent)
				progress := recordProgress(svc)

				_, err := svc.Update(t.Context(), "prod", true)

				wantError(t, err, tc.prefix+errPermanent.Error())
				if tc.call == "Bootstrap" {
					const want = "nomad failed bootstrap: test: permission denied"
					if last := (*progress)[len(*progress)-1]; last != want {
						t.Errorf("the last progress line is %q, want %q", last, want)
					}
				}
				wantLockFree(t, svc.Store)
			})
		})
		t.Run(tc.call+" meets the end of the context", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, _, w := newRelease(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
					if c.Name == tc.call {
						cancel()
					}
					return next(ctx)
				})

				_, err := svc.Update(ctx, "prod", true)

				cause := app.Stands(err)
				if cause == nil || !strings.HasPrefix(cause.Error(), tc.prefix) || !errors.Is(err, context.Canceled) {
					t.Errorf("Update = %v, which stands for %v, want an error that starts with %q and matches "+
						"context.Canceled", err, cause, tc.prefix)
				}
				wantLockFree(t, svc.Store)
			})
		})
	}
}

// TestUpdateNeedsServerAddresses fails the changes that need an address of a server that it does not have, and names
// the servers, with the text that tells to run the command again.
func TestUpdateNeedsServerAddresses(t *testing.T) {
	oneServer := edit(t, serversYAML, "size: 3", "size: 1")
	t.Run("a server joins servers that have no private address", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, _ := newRelease(t)
			seedServer(t, f, "prod-servers-0", "198.18.9.9")

			_, err := svc.Update(t.Context(), "prod", true)

			wantError(t, err, "node prod-servers-1: no server of cluster prod has a private address yet "+
				"(prod-servers-0); run the command again")
			wantLockFree(t, svc.Store)
		})
	})
	t.Run("a client finds no server with a private address", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, _ := newRelease(t, keyedClusterYAML, oneServer, workersYAML)
			seedServer(t, f, "prod-servers-0", "198.18.9.9")

			_, err := svc.Update(t.Context(), "prod", true)

			wantError(t, err, "node prod-workers-0: no server of cluster prod has a private address yet "+
				"(prod-servers-0); run the command again")
		})
	})
	t.Run("no server has a public address", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, f, w := newRelease(t, keyedClusterYAML, oneServer, workersYAML)
			seedServer(t, f, "prod-servers-0", "")

			_, err := svc.Update(t.Context(), "prod", true)

			wantError(t, err, "cluster prod: no server has a public address")
			if n := len(w.Log()); n != 0 {
				t.Errorf("%d Nomad calls were made, want none", n)
			}
		})
	})
}

// TestUpdateHintsOnlyAtTheDeadline adds the hint about the access setting to the wait for a leader that ran out of
// time, and to no other failure.
func TestUpdateHintsOnlyAtTheDeadline(t *testing.T) {
	t.Run("a permanent error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := newRelease(t)
			w.Fail(t, "Leader", errPermanent)

			_, err := svc.Update(t.Context(), "prod", true)

			if err == nil || !errors.Is(err, errPermanent) || strings.Contains(err.Error(), hintText) {
				t.Errorf("Update = %v, want an error that matches %v and has no hint", err, errPermanent)
			}
		})
	})
	t.Run("an ended context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, _, w := newRelease(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			w.SetHook(func(ctx context.Context, c nomadfake.Call, next func(context.Context) error) error {
				if c.Name == "Leader" {
					cancel()
				}
				return next(ctx)
			})

			_, err := svc.Update(ctx, "prod", true)

			if err == nil || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), hintText) {
				t.Errorf("Update = %v, want an error that matches context.Canceled and has no hint", err)
			}
		})
	})
}

// TestUpdateAsksForTheNodePoolOfTheGroup asks for the intro token of a client in the node pool of its group.
func TestUpdateAsksForTheNodePoolOfTheGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		batch := workersYAML + "  nomad:\n    nodePool: batch\n"
		svc, _, w := newRelease(t, keyedClusterYAML, serversYAML, batch)

		mustUpdate(t, svc)

		for _, name := range []string{"prod-workers-0", "prod-workers-1"} {
			want := "nomad IntroToken " + name + " batch 30m0s (prod-servers-0)"
			found := false
			for _, line := range nomadLines(w) {
				found = found || line == want
			}
			if !found {
				t.Errorf("no Nomad call %q in\n%s", want, strings.Join(nomadLines(w), "\n"))
			}
		}
	})
}
