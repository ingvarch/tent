package e2e

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ingvarch/tent/test/e2e/janitor/janitortest"
	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

// discardf is a log function that drops what it is given.
func discardf(string, ...any) {}

const (
	cluster2404 = "e2e-ab-2404"
	cluster2604 = "e2e-ab-2604"
)

func TestLeftoversOfFindsEachKindByItsOwnRule(t *testing.T) {
	got := leftoversOf(cluster2404,
		[]vultrapi.Instance{
			{ID: "i-tag", Label: "other", Tags: []string{"tent/cluster=" + cluster2404}, Status: "active",
				PowerStatus: "running", ServerStatus: "ok"},
			{ID: "i-label", Label: cluster2404 + "-workers-0"},
		},
		[]vultrapi.VPC{{ID: "v-1", Description: "tent:cluster=" + cluster2404 + ";kind=vpc"}},
		[]vultrapi.FirewallGroup{{ID: "f-1", Description: "tent:cluster=" + cluster2404 + ";kind=firewall"}},
		[]vultrapi.SSHKey{{ID: "k-1", Name: "tent:cluster=" + cluster2404 + ";kind=ssh-key"}},
	)
	want := []string{
		"instance i-tag other status=active/running/ok",
		"instance i-label " + cluster2404 + "-workers-0 status=//",
		"vpc v-1 tent:cluster=" + cluster2404 + ";kind=vpc",
		"firewall f-1 tent:cluster=" + cluster2404 + ";kind=firewall",
		"ssh-key k-1 tent:cluster=" + cluster2404 + ";kind=ssh-key",
	}
	if !slices.Equal(got, want) {
		t.Errorf("leftoversOf = %q, want %q", got, want)
	}
}

func TestLeftoversOfCatchesAnObjectWithoutTheMarker(t *testing.T) {
	got := leftoversOf(cluster2404, nil,
		[]vultrapi.VPC{{ID: "v-1", Description: "made for " + cluster2404}},
		[]vultrapi.FirewallGroup{{ID: "f-1", Description: cluster2404}},
		[]vultrapi.SSHKey{{ID: "k-1", Name: "key of " + cluster2404}},
	)
	if len(got) != 3 {
		t.Errorf("leftoversOf = %q, want the VPC, the firewall group and the key", got)
	}
}

func TestLeftoversOfLeavesObjectsOfAnotherClusterWithTheSamePrefix(t *testing.T) {
	got := leftoversOf(cluster2404,
		[]vultrapi.Instance{
			{ID: "i-1", Label: cluster2604 + "-servers-0", Tags: []string{"tent/cluster=" + cluster2604}},
			{ID: "i-2", Label: "e2e-ab-24040-servers-0"},
		},
		[]vultrapi.VPC{{ID: "v-1", Description: "tent:cluster=" + cluster2604 + ";kind=vpc"}},
		[]vultrapi.FirewallGroup{{ID: "f-1", Description: "tent:cluster=" + cluster2604 + ";kind=firewall"}},
		[]vultrapi.SSHKey{{ID: "k-1", Name: "tent:cluster=" + cluster2604 + ";kind=ssh-key"}, {ID: "k-2", Name: "main"}},
	)
	if len(got) != 0 {
		t.Errorf("leftoversOf = %q, want nothing of the other clusters", got)
	}
}

func TestLeftoversOfMatchesALabelOnlyWithTheDashAfterTheName(t *testing.T) {
	got := leftoversOf(cluster2404, []vultrapi.Instance{{ID: "i-1", Label: cluster2404}}, nil, nil, nil)
	if len(got) != 0 {
		t.Errorf("leftoversOf = %q, want nothing for a label without the dash", got)
	}
}

func TestLeftoversNowListsTheFourKinds(t *testing.T) {
	api := &janitortest.API{
		Machines: []vultrapi.Instance{{ID: "i-1", Tags: []string{"tent/cluster=" + cluster2404}}},
		Nets:     []vultrapi.VPC{{ID: "v-1", Description: cluster2404}},
		Groups:   []vultrapi.FirewallGroup{{ID: "f-1", Description: cluster2404}},
		Keys:     []vultrapi.SSHKey{{ID: "k-1", Name: cluster2404}},
	}
	got, err := leftoversNow(t.Context(), api, cluster2404)
	if err != nil {
		t.Fatalf("leftoversNow: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("leftoversNow = %q, want one object of each kind", got)
	}
	wantCalls := []string{"list-instances", "list-vpcs", "list-firewalls", "list-ssh-keys"}
	if !slices.Equal(api.Calls, wantCalls) {
		t.Errorf("calls = %q, want %q", api.Calls, wantCalls)
	}
}

func TestLeftoversNowNamesTheKindWhoseListFailed(t *testing.T) {
	for call, kind := range map[string]string{
		"list-instances": "instances", "list-vpcs": "VPCs", "list-firewalls": "firewall groups",
		"list-ssh-keys": "SSH keys",
	} {
		t.Run(call, func(t *testing.T) {
			boom := errors.New("boom")
			api := &janitortest.API{Err: map[string]error{call: boom}}
			_, err := leftoversNow(t.Context(), api, cluster2404)
			if !errors.Is(err, boom) || !strings.Contains(err.Error(), "list "+kind) {
				t.Errorf("leftoversNow = %v, want %q and the cause", err, "list "+kind)
			}
		})
	}
}

func TestWaitNoLeftoversPassesWhenTheObjectsDisappearInTime(t *testing.T) {
	api := &janitortest.API{
		Machines: []vultrapi.Instance{{ID: "i-1", Tags: []string{"tent/cluster=" + cluster2404}}},
		Linger:   map[string]int{},
	}
	api.Linger["i-1"] = 2
	if err := api.DeleteInstance(t.Context(), "i-1"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	var seen []string
	logf := func(format string, args ...any) { seen = append(seen, fmt.Sprintf(format, args...)) }
	if err := waitNoLeftovers(t.Context(), api, cluster2404, time.Millisecond, time.Minute, logf); err != nil {
		t.Errorf("waitNoLeftovers: %v", err)
	}
	// The two polls that still listed the instance are logged, the last one that found nothing is not.
	if len(seen) != 2 || !strings.Contains(seen[0], "instance i-1 ") || !strings.Contains(seen[0], "still listed") {
		t.Errorf("logged %q, want the two polls that listed instance i-1", seen)
	}
}

func TestWaitNoLeftoversListsWhatIsLeftAtTheEnd(t *testing.T) {
	api := &janitortest.API{Nets: []vultrapi.VPC{{ID: "v-9", Description: "tent:cluster=" + cluster2404}}}
	err := waitNoLeftovers(t.Context(), api, cluster2404, time.Millisecond, 20*time.Millisecond, discardf)
	if err == nil || !strings.Contains(err.Error(), "vpc v-9 ") {
		t.Errorf("waitNoLeftovers = %v, want the VPC named", err)
	}
}

func TestWaitNoLeftoversLogsAPollWhoseListFailed(t *testing.T) {
	api := &janitortest.API{Err: map[string]error{"list-vpcs": errors.New("boom")}}
	var seen []string
	logf := func(format string, args ...any) { seen = append(seen, fmt.Sprintf(format, args...)) }
	_ = waitNoLeftovers(t.Context(), api, cluster2404, time.Millisecond, 20*time.Millisecond, logf)
	if len(seen) == 0 || !strings.Contains(seen[0], "list VPCs: boom") || !strings.Contains(seen[0], "took") {
		t.Errorf("logged %q, want each failed poll with its error and the time its lists took", seen)
	}
}

func TestWaitNoLeftoversReportsAFailedList(t *testing.T) {
	api := &janitortest.API{Err: map[string]error{"list-vpcs": errors.New("boom")}}
	err := waitNoLeftovers(t.Context(), api, cluster2404, time.Millisecond, 20*time.Millisecond, discardf)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("waitNoLeftovers = %v, want the list error", err)
	}
}
