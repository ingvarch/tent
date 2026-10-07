package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/test/e2e/janitor"
	"github.com/ingvarch/tent/test/e2e/janitor/janitortest"
	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

const testKey = "test-key-value"

var created = time.Now().Add(-5 * time.Hour).UTC().Truncate(time.Second)

func line(kind, id, name, cluster string) string {
	return fmt.Sprintf("%s %s %s cluster=%s created=%s", kind, id, name, cluster, created.Format(time.RFC3339))
}

// account holds two old E2E clusters, a young one and the maintainer's own VPC.
func account() *janitortest.API {
	young := time.Now().Add(-time.Hour)
	return &janitortest.API{
		Machines: []vultrapi.Instance{
			{ID: "i1", Label: "e2e-a-servers-0", Tags: []string{"tent/cluster=e2e-a"}, Created: created},
			{ID: "i2", Label: "e2e-young-servers-0", Tags: []string{"tent/cluster=e2e-young"}, Created: young},
		},
		Nets: []vultrapi.VPC{
			{ID: "v1", Description: "tent:cluster=e2e-a;kind=vpc", Created: created},
			{ID: "v2", Description: "tent:cluster=e2e-b;kind=vpc", Created: created},
			{ID: "own", Description: "live_vpc", Created: created},
		},
	}
}

type result struct {
	code           int
	stdout, stderr string
	key            string
	asked          bool
}

func runWith(t *testing.T, api *janitortest.API, env map[string]string, args ...string) result {
	t.Helper()
	var res result
	var stdout, stderr bytes.Buffer
	newAPI := func(key string) janitor.API {
		res.key, res.asked = key, true
		return api
	}
	res.code = run(t.Context(), args, func(k string) string { return env[k] }, &stdout, &stderr, newAPI)
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

func withKey() map[string]string { return map[string]string{"VULTR_API_KEY": testKey} }

func deletes(api *janitortest.API) []string {
	return slices.DeleteFunc(slices.Clone(api.Calls), func(c string) bool { return strings.HasPrefix(c, "list-") })
}

func TestRunRefusesWithoutAKey(t *testing.T) {
	for name, env := range map[string]map[string]string{"unset": {}, "empty": {"VULTR_API_KEY": ""}} {
		t.Run(name, func(t *testing.T) {
			res := runWith(t, account(), env, "--yes")
			if res.code != 1 || res.stderr != "Error: VULTR_API_KEY is not set\n" || res.stdout != "" || res.asked {
				t.Errorf("got %+v, want exit 1 and only the error", res)
			}
		})
	}
}

func TestRunRefusesANegativeAge(t *testing.T) {
	api := account()
	res := runWith(t, api, withKey(), "--older-than", "-1s", "--yes")
	if res.code != 1 || res.stderr != "Error: --older-than must not be negative\n" || res.stdout != "" {
		t.Errorf("got %+v, want exit 1 and the error", res)
	}
	if len(api.Calls) != 0 {
		t.Errorf("run called the API: %q", api.Calls)
	}
}

func TestRunRejectsAnUnknownFlag(t *testing.T) {
	api := account()
	res := runWith(t, api, withKey(), "--nope")
	if res.code != 2 || !strings.Contains(res.stderr, "nope") || len(api.Calls) != 0 {
		t.Errorf("got %+v and calls %q, want exit 2, the flag named and no call", res, api.Calls)
	}
}

func TestRunHelpExitsWithZero(t *testing.T) {
	res := runWith(t, account(), withKey(), "-h")
	if res.code != 0 || !strings.Contains(res.stderr, "older-than") || res.asked {
		t.Errorf("got %+v, want exit 0 and the usage", res)
	}
}

func TestRunPassesTheKeyToTheAPI(t *testing.T) {
	res := runWith(t, account(), withKey())
	if res.key != testKey {
		t.Errorf("newAPI got key %q, want %q", res.key, testKey)
	}
}

func TestRunWithoutYesListsAndDeletesNothing(t *testing.T) {
	api := account()
	res := runWith(t, api, withKey())
	want := strings.Join([]string{
		line("instance", "i1", "e2e-a-servers-0", "e2e-a"),
		line("vpc", "v1", "tent:cluster=e2e-a;kind=vpc", "e2e-a"),
		line("vpc", "v2", "tent:cluster=e2e-b;kind=vpc", "e2e-b"),
		"would delete 3 objects of 2 clusters older than 3h0m0s; run again with --yes",
	}, "\n") + "\n"
	if res.code != 0 || res.stdout != want || res.stderr != "" {
		t.Errorf("got exit %d, stdout\n%q\nstderr %q\nwant stdout\n%q", res.code, res.stdout, res.stderr, want)
	}
	if got := deletes(api); len(got) != 0 {
		t.Errorf("run deleted without --yes: %q", got)
	}
}

func TestRunSaysWhenNothingIsOlder(t *testing.T) {
	for _, args := range [][]string{{"--older-than", "100h"}, {"--older-than", "100h", "--yes"}} {
		api := account()
		res := runWith(t, api, withKey(), args...)
		if res.code != 0 || res.stdout != "nothing older than 100h0m0s\n" || res.stderr != "" {
			t.Errorf("%v: got %+v, want \"nothing older than 100h0m0s\"", args, res)
		}
		if got := deletes(api); len(got) != 0 {
			t.Errorf("%v: run deleted %q", args, got)
		}
	}
}

func TestRunTakesTheAgeFromTheFlag(t *testing.T) {
	res := runWith(t, account(), withKey(), "--older-than=30m")
	if !strings.Contains(res.stdout, "e2e-young") ||
		!strings.HasSuffix(res.stdout, "would delete 4 objects of 3 clusters older than 30m0s; run again with --yes\n") {
		t.Errorf("stdout = %q, want the young cluster too and a count of 4 objects of 3 clusters", res.stdout)
	}
}

func TestRunAcceptsAZeroAge(t *testing.T) {
	res := runWith(t, account(), withKey(), "--older-than", "0")
	if res.code != 0 || !strings.Contains(res.stdout, "older than 0s; run again with --yes") {
		t.Errorf("got %+v, want exit 0 with the age 0s", res)
	}
}

func TestRunWithYesDeletesAndCounts(t *testing.T) {
	api := account()
	res := runWith(t, api, withKey(), "--yes")
	want := strings.Join([]string{
		"delete " + line("instance", "i1", "e2e-a-servers-0", "e2e-a"),
		"delete " + line("vpc", "v1", "tent:cluster=e2e-a;kind=vpc", "e2e-a"),
		"delete " + line("vpc", "v2", "tent:cluster=e2e-b;kind=vpc", "e2e-b"),
		"deleted 3 objects of 2 clusters",
	}, "\n") + "\n"
	if res.code != 0 || res.stdout != want || res.stderr != "" {
		t.Errorf("got exit %d, stdout\n%q\nstderr %q\nwant stdout\n%q", res.code, res.stdout, res.stderr, want)
	}
	wantCalls := []string{"delete-instance i1", "delete-vpc v1", "delete-vpc v2"}
	if got := deletes(api); !slices.Equal(got, wantCalls) {
		t.Errorf("deletes = %q, want %q", got, wantCalls)
	}
}

func TestRunWithYesReportsFailures(t *testing.T) {
	boom := errors.New("boom")
	api := account()
	api.Err = map[string]error{"delete-instance i1": boom}
	res := runWith(t, api, withKey(), "--yes")
	wantErr := "Error: " + line("instance", "i1", "e2e-a-servers-0", "e2e-a") + ": boom\n"
	if res.code != 1 || res.stderr != wantErr {
		t.Errorf("got exit %d, stderr %q, want exit 1 and %q", res.code, res.stderr, wantErr)
	}
	if strings.Contains(res.stdout, "deleted ") {
		t.Errorf("stdout %q claims success", res.stdout)
	}
	if got := deletes(api); !slices.Contains(got, "delete-vpc v2") {
		t.Errorf("the sweep did not go on: %q", got)
	}
}

func TestRunReportsAListError(t *testing.T) {
	api := account()
	api.Err = map[string]error{"list-vpcs": errors.New("boom")}
	res := runWith(t, api, withKey(), "--yes")
	if res.code != 1 || res.stderr != "Error: listing vpcs: boom\n" || res.stdout != "" {
		t.Errorf("got %+v, want exit 1 and the list error", res)
	}
	if got := deletes(api); len(got) != 0 {
		t.Errorf("run deleted after a list error: %q", got)
	}
}
