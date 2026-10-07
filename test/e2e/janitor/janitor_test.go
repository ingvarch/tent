package janitor_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/test/e2e/janitor"
	"github.com/ingvarch/tent/test/e2e/janitor/janitortest"
	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

const (
	old   = 4 * time.Hour
	young = time.Hour
	limit = 3 * time.Hour
)

func ago(d time.Duration) time.Time { return now.Add(-d) }

func marker(cluster, kind string) string { return "tent:cluster=" + cluster + ";kind=" + kind }

func instance(id, label string, created time.Time, tags ...string) vultrapi.Instance {
	return vultrapi.Instance{ID: id, Label: label, Tags: tags, Created: created}
}

func find(t *testing.T, api *janitortest.API, olderThan time.Duration) []janitor.Object {
	t.Helper()
	j := janitor.New(api, &bytes.Buffer{})
	objects, err := j.Find(t.Context(), olderThan, now)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	return objects
}

// ids returns "kind:id" of each object, in order.
func ids(objects []janitor.Object) []string {
	var out []string
	for _, o := range objects {
		out = append(out, o.Kind+":"+o.ID)
	}
	return out
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestNewSetsPollingDefaults(t *testing.T) {
	var out bytes.Buffer
	api := &janitortest.API{}
	j := janitor.New(api, &out)
	if j.API != api || j.Out != &out {
		t.Errorf("New did not keep the API and the writer: %+v", j)
	}
	if j.Every != 10*time.Second || j.Timeout != 5*time.Minute {
		t.Errorf("Every = %s, Timeout = %s, want 10s and 5m", j.Every, j.Timeout)
	}
}

func TestObjectString(t *testing.T) {
	zone := time.FixedZone("plus2", 2*60*60)
	o := janitor.Object{
		Kind: janitor.KindVPC, ID: "v1", Name: "tent:cluster=e2e-a;kind=vpc", Cluster: "e2e-a",
		Created: time.Date(2026, 10, 7, 14, 30, 5, 0, zone),
	}
	want := "vpc v1 tent:cluster=e2e-a;kind=vpc cluster=e2e-a created=2026-10-07T12:30:05Z"
	if got := o.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestFindReadsEachKindFromItsTagOrMarker(t *testing.T) {
	created := ago(old)
	firewall := marker("e2e-a", "firewall") + ";role=server"
	api := &janitortest.API{
		Machines: []vultrapi.Instance{instance("i1", "e2e-a-servers-0", created, "tent/cluster=e2e-a", "tent/role=server")},
		Nets:     []vultrapi.VPC{{ID: "v1", Description: marker("e2e-a", "vpc"), Created: created}},
		Groups:   []vultrapi.FirewallGroup{{ID: "f1", Description: firewall, Created: created}},
		Keys:     []vultrapi.SSHKey{{ID: "k1", Name: marker("e2e-a", "ssh"), Created: created}},
	}
	got := find(t, api, limit)
	want := []janitor.Object{
		{Kind: janitor.KindInstance, ID: "i1", Name: "e2e-a-servers-0", Cluster: "e2e-a", Created: created},
		{Kind: janitor.KindFirewall, ID: "f1", Name: firewall, Cluster: "e2e-a", Created: created},
		{Kind: janitor.KindVPC, ID: "v1", Name: marker("e2e-a", "vpc"), Cluster: "e2e-a", Created: created},
		{Kind: janitor.KindSSHKey, ID: "k1", Name: marker("e2e-a", "ssh"), Cluster: "e2e-a", Created: created},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Find =\n%+v\nwant\n%+v", got, want)
	}
}

func TestFindLeavesInstancesThatAreNotE2E(t *testing.T) {
	created := ago(old)
	cases := map[string]vultrapi.Instance{
		"e2e without the dash":                 instance("i9", "x", created, "tent/cluster=e2eprod"),
		"cluster without the prefix":           instance("i1", "prod-servers-0", created, "tent/cluster=prod"),
		"prefix only inside the name":          instance("i2", "x", created, "tent/cluster=my-e2e-a"),
		"no tags":                              instance("i3", "e2e-a-servers-0", created),
		"tags of another tool":                 instance("i4", "e2e-a-servers-0", created, "cluster=e2e-a", "team=e2e-a"),
		"label holds the marker but no tag":    instance("i5", "tent:cluster=e2e-a;kind=x", created),
		"two different tent/cluster tags":      instance("i6", "x", created, "tent/cluster=e2e-a", "tent/cluster=e2e-b"),
		"empty cluster tag":                    instance("i7", "x", created, "tent/cluster="),
		"tag prefix in upper case":             instance("i10", "x", created, "TENT/cluster=e2e-a"),
		"tent/cluster tag with another prefix": instance("i8", "x", created, "xtent/cluster=e2e-a"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got := find(t, &janitortest.API{Machines: []vultrapi.Instance{in}}, 0); len(got) != 0 {
				t.Errorf("Find returned %+v, want nothing", got)
			}
		})
	}
}

func TestFindKeepsInstanceWithTheSameClusterTagTwice(t *testing.T) {
	api := &janitortest.API{Machines: []vultrapi.Instance{
		instance("i1", "x", ago(old), "tent/cluster=e2e-a", "tent/role=server", "tent/cluster=e2e-a"),
	}}
	equal(t, "objects", ids(find(t, api, limit)), []string{"instance:i1"})
}

func TestFindLeavesMarkersThatAreNotE2E(t *testing.T) {
	created := ago(old)
	texts := map[string]string{
		"the account's own VPC":                 "live_vpc",
		"empty":                                 "",
		"no tent prefix":                        "cluster=e2e-a;kind=vpc",
		"another tool's prefix":                 "other:cluster=e2e-a;kind=vpc",
		"e2e without the dash":                  marker("e2eprod", "vpc"),
		"tent prefix not at the start":          "note tent:cluster=e2e-a;kind=vpc",
		"cluster without the prefix":            marker("prod", "vpc"),
		"no cluster field":                      "tent:kind=vpc",
		"nothing after the prefix":              "tent:",
		"empty value":                           "tent:cluster=e2e-a;kind=",
		"empty value of the cluster":            "tent:cluster=;kind=vpc",
		"field without =":                       "tent:cluster=e2e-a;kind",
		"empty field in the middle":             "tent:cluster=e2e-a;;kind=vpc",
		"empty field at the end":                "tent:cluster=e2e-a;kind=vpc;",
		"empty key":                             "tent:cluster=e2e-a;=vpc",
		"two cluster fields":                    "tent:cluster=e2e-a;cluster=e2e-b",
		"two equal cluster fields":              "tent:cluster=e2e-a;kind=vpc;cluster=e2e-a",
		"field name only ends with cluster":     "tent:mycluster=e2e-a;kind=vpc",
		"marker's cluster in a different field": "tent:name=e2e-a;kind=vpc",
	}
	for name, text := range texts {
		t.Run(name, func(t *testing.T) {
			api := &janitortest.API{
				Nets:   []vultrapi.VPC{{ID: "v1", Description: text, Created: created}},
				Groups: []vultrapi.FirewallGroup{{ID: "f1", Description: text, Created: created}},
				Keys:   []vultrapi.SSHKey{{ID: "k1", Name: text, Created: created}},
			}
			if got := find(t, api, 0); len(got) != 0 {
				t.Errorf("Find returned %+v, want nothing", got)
			}
		})
	}
}

func TestFindReadsTheClusterFieldOfAMarkerInAnyPositionWithValuesThatHoldEquals(t *testing.T) {
	api := &janitortest.API{Nets: []vultrapi.VPC{
		{ID: "v1", Description: "tent:kind=vpc;note=a=b;cluster=e2e-a", Created: ago(old)},
	}}
	got := find(t, api, limit)
	if len(got) != 1 || got[0].Cluster != "e2e-a" {
		t.Errorf("Find = %+v, want one object of cluster e2e-a", got)
	}
}

func TestFindLeavesTheAccountsOwnObjectsAmongE2EOnes(t *testing.T) {
	created := ago(old)
	api := &janitortest.API{
		Nets: []vultrapi.VPC{
			{ID: "own1", Description: "live_vpc", Created: ago(24 * 365 * time.Hour)},
			{ID: "own2", Description: "dev_vpc", Created: ago(24 * 365 * time.Hour)},
			{ID: "v1", Description: marker("e2e-a", "vpc"), Created: created},
		},
		Keys: []vultrapi.SSHKey{
			{ID: "main", Name: "main", Created: ago(24 * 365 * time.Hour)},
			{ID: "k1", Name: marker("e2e-a", "ssh"), Created: created},
		},
	}
	equal(t, "objects", ids(find(t, api, limit)), []string{"vpc:v1", "ssh-key:k1"})
}

func TestFindAgeRulePerCluster(t *testing.T) {
	tag := func(c string) string { return "tent/cluster=" + c }
	api := &janitortest.API{
		Machines: []vultrapi.Instance{
			instance("a-i", "a", ago(old), tag("e2e-a")),     // old instance of A
			instance("b-i", "b", ago(young), tag("e2e-b")),   // young cluster B
			instance("c-i", "c", ago(young), tag("e2e-c")),   // C: young instance, old VPC
			instance("d-i", "d", ago(limit), tag("e2e-d")),   // D: exactly at the limit
			instance("e-i", "e", ago(limit+1), tag("e2e-e")), // E: just over the limit
		},
		Nets: []vultrapi.VPC{
			{ID: "a-v", Description: marker("e2e-a", "vpc"), Created: ago(young)},
			{ID: "b-v", Description: marker("e2e-b", "vpc"), Created: ago(young)},
			{ID: "c-v", Description: marker("e2e-c", "vpc"), Created: ago(old)},
		},
	}
	got := find(t, api, limit)
	equal(t, "objects", ids(got), []string{"instance:a-i", "instance:c-i", "instance:e-i", "vpc:a-v", "vpc:c-v"})
}

func TestFindZeroAgeTakesEveryObjectBeforeNow(t *testing.T) {
	api := &janitortest.API{Machines: []vultrapi.Instance{
		instance("i1", "x", ago(time.Nanosecond), "tent/cluster=e2e-a"),
		instance("i2", "x", now, "tent/cluster=e2e-b"),
	}}
	equal(t, "objects", ids(find(t, api, 0)), []string{"instance:i1"})
}

func TestFindSortsByKindThenClusterThenID(t *testing.T) {
	created := ago(old)
	api := &janitortest.API{
		Machines: []vultrapi.Instance{
			instance("i9", "x", created, "tent/cluster=e2e-b"),
			instance("i1", "x", created, "tent/cluster=e2e-b"),
			instance("i5", "x", created, "tent/cluster=e2e-a"),
		},
		Nets: []vultrapi.VPC{
			{ID: "v2", Description: marker("e2e-b", "vpc"), Created: created},
			{ID: "v1", Description: marker("e2e-a", "vpc"), Created: created},
		},
		Groups: []vultrapi.FirewallGroup{
			{ID: "f1", Description: marker("e2e-b", "firewall"), Created: created},
			{ID: "f2", Description: marker("e2e-a", "firewall"), Created: created},
		},
		Keys: []vultrapi.SSHKey{
			{ID: "k1", Name: marker("e2e-b", "ssh"), Created: created},
			{ID: "k2", Name: marker("e2e-a", "ssh"), Created: created},
		},
	}
	equal(t, "objects", ids(find(t, api, limit)), []string{
		"instance:i5", "instance:i1", "instance:i9",
		"firewall:f2", "firewall:f1",
		"vpc:v1", "vpc:v2",
		"ssh-key:k2", "ssh-key:k1",
	})
}

func TestFindSortsOneClusterByID(t *testing.T) {
	created := ago(old)
	api := &janitortest.API{Machines: []vultrapi.Instance{
		instance("i2", "x", created, "tent/cluster=e2e-a"),
		instance("i1", "x", created, "tent/cluster=e2e-a"),
		instance("i3", "x", created, "tent/cluster=e2e-a"),
	}}
	equal(t, "objects", ids(find(t, api, limit)), []string{"instance:i1", "instance:i2", "instance:i3"})
}

func TestFindReturnsTheErrorOfEachList(t *testing.T) {
	boom := errors.New("boom")
	for _, call := range []string{"list-instances", "list-firewalls", "list-vpcs", "list-ssh-keys"} {
		t.Run(call, func(t *testing.T) {
			api := &janitortest.API{Err: map[string]error{call: boom}}
			_, err := janitor.New(api, &bytes.Buffer{}).Find(t.Context(), limit, now)
			if !errors.Is(err, boom) {
				t.Fatalf("Find error = %v, want it to wrap %v", err, boom)
			}
			if kind := strings.TrimPrefix(call, "list-"); !strings.Contains(err.Error(), kind) {
				t.Errorf("Find error %q does not name %q", err, kind)
			}
		})
	}
}

func e2eObject(kind, id string) janitor.Object {
	return janitor.Object{Kind: kind, ID: id, Name: "name-" + id, Cluster: "e2e-a", Created: ago(old)}
}

func sweeper(api *janitortest.API) *janitor.Janitor {
	j := janitor.New(api, api.Writer())
	j.Every = time.Millisecond
	j.Timeout = 5 * time.Second
	return j
}

func fullAccount() (*janitortest.API, []janitor.Object) {
	api := &janitortest.API{
		Machines: []vultrapi.Instance{{ID: "i1"}, {ID: "i2"}},
		Groups:   []vultrapi.FirewallGroup{{ID: "f1"}},
		Nets:     []vultrapi.VPC{{ID: "v1"}},
		Keys:     []vultrapi.SSHKey{{ID: "k1"}},
	}
	objects := []janitor.Object{
		e2eObject(janitor.KindInstance, "i1"), e2eObject(janitor.KindInstance, "i2"),
		e2eObject(janitor.KindFirewall, "f1"), e2eObject(janitor.KindVPC, "v1"), e2eObject(janitor.KindSSHKey, "k1"),
	}
	return api, objects
}

func TestSweepDeletesInstancesThenWaitsThenFirewallsVPCsAndKeys(t *testing.T) {
	api, objects := fullAccount()
	api.Linger = map[string]int{"i1": 2}
	if err := sweeper(api).Sweep(t.Context(), objects); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	equal(t, "calls", api.Calls, []string{
		"delete-instance i1", "delete-instance i2",
		"list-instances", "list-instances", "list-instances",
		"delete-firewall f1", "delete-vpc v1", "delete-ssh-key k1",
	})
}

func TestSweepWaitsOnlyForTheInstancesItDeleted(t *testing.T) {
	api, objects := fullAccount()
	api.Machines = append(api.Machines, vultrapi.Instance{ID: "other"})
	if err := sweeper(api).Sweep(t.Context(), objects); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	equal(t, "calls", api.Calls, []string{
		"delete-instance i1", "delete-instance i2", "list-instances",
		"delete-firewall f1", "delete-vpc v1", "delete-ssh-key k1",
	})
}

func TestSweepWritesEachLineBeforeItsDelete(t *testing.T) {
	api, objects := fullAccount()
	if err := sweeper(api).Sweep(t.Context(), objects); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	line := func(o janitor.Object) string { return "out: delete " + o.String() }
	equal(t, "events", api.Events, []string{
		line(objects[0]), "delete-instance i1",
		line(objects[1]), "delete-instance i2",
		"list-instances",
		line(objects[2]), "delete-firewall f1",
		line(objects[3]), "delete-vpc v1",
		line(objects[4]), "delete-ssh-key k1",
	})
}

func TestSweepDoesNothingForNoObjects(t *testing.T) {
	api := &janitortest.API{}
	if err := sweeper(api).Sweep(t.Context(), nil); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(api.Events) != 0 {
		t.Errorf("Sweep did %q, want nothing", api.Events)
	}
}

func TestSweepSkipsTheWaitWhenNoInstanceIsDeleted(t *testing.T) {
	api, objects := fullAccount()
	if err := sweeper(api).Sweep(t.Context(), objects[2:]); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	equal(t, "calls", api.Calls, []string{"delete-firewall f1", "delete-vpc v1", "delete-ssh-key k1"})
}

func TestSweepReportsFailedDeletesAndGoesOn(t *testing.T) {
	boom := errors.New("boom")
	api, objects := fullAccount()
	api.Err = map[string]error{"delete-instance i1": boom, "delete-vpc v1": boom}
	j := sweeper(api)
	j.Timeout = 30 * time.Millisecond
	err := j.Sweep(t.Context(), objects)
	if !errors.Is(err, boom) {
		t.Fatalf("Sweep error = %v, want it to wrap %v", err, boom)
	}
	for _, o := range []janitor.Object{objects[0], objects[3]} {
		if want := o.String() + ": boom"; !strings.Contains(err.Error(), want) {
			t.Errorf("Sweep error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), objects[1].String()) || strings.Contains(err.Error(), "still listed") {
		t.Errorf("Sweep error %q names more than the two failures", err)
	}
	if len(api.Calls) < 4 {
		t.Fatalf("calls = %q, want at least four", api.Calls)
	}
	if got := api.Calls[len(api.Calls)-1]; got != "delete-ssh-key k1" {
		t.Errorf("last call = %q, want the SSH key delete after the failed VPC", got)
	}
	equal(t, "first calls", api.Calls[:4],
		[]string{"delete-instance i1", "delete-instance i2", "list-instances", "delete-firewall f1"})
}

func TestSweepRetriesAVPCDeleteThatIsRefused(t *testing.T) {
	api, objects := fullAccount()
	api.Err = map[string]error{"delete-vpc v1": errors.New("servers attached")}
	api.Refuse = map[string]int{"delete-vpc v1": 2}
	if err := sweeper(api).Sweep(t.Context(), objects); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	equal(t, "calls", api.Calls, []string{
		"delete-instance i1", "delete-instance i2", "list-instances",
		"delete-firewall f1", "delete-vpc v1", "delete-vpc v1", "delete-vpc v1", "delete-ssh-key k1",
	})
	lines := 0
	for _, e := range api.Events {
		if strings.HasPrefix(e, "out: delete vpc") {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("Sweep wrote the VPC line %d times, want once", lines)
	}
}

func TestSweepReportsTheLastRefusalOfAVPCAfterTheTimeout(t *testing.T) {
	api, objects := fullAccount()
	api.Err = map[string]error{"delete-vpc v1": errors.New("servers attached")}
	api.Refuse = map[string]int{"delete-vpc v1": 1000000}
	j := sweeper(api)
	j.Timeout = 30 * time.Millisecond
	err := j.Sweep(t.Context(), objects)
	if want := objects[3].String() + ": servers attached"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Sweep error = %v, want it to hold %q", err, want)
	}
	if n := strings.Count(err.Error(), "servers attached"); n != 1 {
		t.Errorf("Sweep error names the refusal %d times, want once: %v", n, err)
	}
	if got := api.Calls[len(api.Calls)-1]; got != "delete-ssh-key k1" {
		t.Errorf("last call = %q, want the SSH key delete", got)
	}
}

func TestSweepReportsEveryFailureOfEveryKind(t *testing.T) {
	boom := errors.New("boom")
	api, objects := fullAccount()
	api.Err = map[string]error{"delete-firewall f1": boom, "delete-ssh-key k1": boom}
	err := sweeper(api).Sweep(t.Context(), objects)
	for _, o := range []janitor.Object{objects[2], objects[4]} {
		if want := o.String() + ": boom"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Sweep error %v lacks %q", err, want)
		}
	}
}

func TestSweepFailsWhenInstancesAreStillListedAfterTheTimeout(t *testing.T) {
	api, objects := fullAccount()
	api.Linger = map[string]int{"i2": 100000}
	j := sweeper(api)
	j.Timeout = 30 * time.Millisecond
	err := j.Sweep(t.Context(), objects)
	if err == nil || !strings.Contains(err.Error(), "instances still listed after 30ms: i2") {
		t.Fatalf("Sweep error = %v, want \"instances still listed after 30ms: i2\"", err)
	}
	last := api.Calls[len(api.Calls)-3:]
	equal(t, "calls after the wait", last, []string{"delete-firewall f1", "delete-vpc v1", "delete-ssh-key k1"})
}

func TestSweepNamesEveryInstanceStillListed(t *testing.T) {
	api, objects := fullAccount()
	api.Linger = map[string]int{"i1": 100000, "i2": 100000}
	j := sweeper(api)
	j.Timeout = 20 * time.Millisecond
	err := j.Sweep(t.Context(), objects)
	if err == nil || !strings.Contains(err.Error(), "instances still listed after 20ms: i1, i2") {
		t.Fatalf("Sweep error = %v, want both instances named", err)
	}
}

func TestSweepReportsAListErrorOfTheWaitAndGoesOn(t *testing.T) {
	boom := errors.New("boom")
	api, objects := fullAccount()
	api.Err = map[string]error{"list-instances": boom}
	err := sweeper(api).Sweep(t.Context(), objects)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "instances") {
		t.Fatalf("Sweep error = %v, want a wrapped %v that names the instances", err, boom)
	}
	last := api.Calls[len(api.Calls)-3:]
	equal(t, "calls after the wait", last, []string{"delete-firewall f1", "delete-vpc v1", "delete-ssh-key k1"})
}

func TestSweepStopsWhenTheContextHasEndedWhileInstancesAreListed(t *testing.T) {
	api, objects := fullAccount()
	api.Linger = map[string]int{"i1": 100000}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := sweeper(api).Sweep(ctx, objects)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sweep error = %v, want it to wrap %v", err, context.Canceled)
	}
	if strings.Contains(err.Error(), "still listed") {
		t.Errorf("Sweep error %q reports a timeout", err)
	}
	for _, call := range api.Calls {
		if strings.HasPrefix(call, "delete-firewall") || strings.HasPrefix(call, "delete-vpc") ||
			strings.HasPrefix(call, "delete-ssh-key") {
			t.Errorf("Sweep went on after the context ended: %q", api.Calls)
		}
	}
}

func TestSweepStopsWhenTheContextEndedAndNothingIsListed(t *testing.T) {
	api, objects := fullAccount()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := sweeper(api).Sweep(ctx, objects)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sweep error = %v, want it to wrap %v", err, context.Canceled)
	}
	if slices.Contains(api.Calls, "delete-firewall f1") {
		t.Errorf("Sweep went on after the context ended: %q", api.Calls)
	}
}
