package nomadfake_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/nomadops/nomadfake"
)

// pool is a gossip pool with a member in each status: s1 is alive, s2 failed, s3 left, s4 leaving and s5 alive.
func pool(t *testing.T) (*nomadfake.Fake, nomadops.API) {
	t.Helper()
	f, a := newBootstrappedAPI(t)
	f.SetMembers([]nomadops.Member{
		{Name: "s1.eu", Address: netip.MustParseAddr("10.0.0.1"), Status: "alive"},
		{Name: "s2.eu", Address: netip.MustParseAddr("10.0.0.2"), Status: "failed"},
		{Name: "s3.eu", Address: netip.MustParseAddr("10.0.0.3"), Status: "left"},
		{Name: "s4.eu", Address: netip.MustParseAddr("10.0.0.4"), Status: "leaving"},
		{Name: "s5.eu", Address: netip.MustParseAddr("10.0.0.5"), Status: "alive"},
	})
	return f, a
}

// statuses reads the pool as "<name> <status>" lines, in the order that Members lists them.
func statuses(t *testing.T, a nomadops.API) []string {
	t.Helper()
	members, err := a.Members(t.Context())
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	var out []string
	for _, m := range members {
		out = append(out, m.Name+" "+m.Status)
	}
	return out
}

// TestMembersAreCopies checks that neither SetMembers nor Members shares memory with its caller.
func TestMembersAreCopies(t *testing.T) {
	f, a := newBootstrappedAPI(t)
	set := []nomadops.Member{{Name: "s1.eu", Status: "alive"}}
	f.SetMembers(set)
	set[0].Status = "failed"

	got, err := a.Members(t.Context())
	if err != nil || len(got) != 1 || got[0].Status != "alive" {
		t.Fatalf("Members() = %+v, %v; want s1.eu alive, as it was set", got, err)
	}
	got[0].Status = "failed"

	if again := statuses(t, a); !cmp.Equal(again, []string{"s1.eu alive"}) {
		t.Errorf("Members() after a change of the returned list = %v, want s1.eu alive", again)
	}
}

// TestForceLeaveOfAMemberThatIsDown checks that a failed member and one that left are dropped at once, and the rest of
// the pool stays.
func TestForceLeaveOfAMemberThatIsDown(t *testing.T) {
	for _, name := range []string{"s2.eu", "s3.eu"} {
		t.Run(name, func(t *testing.T) {
			f, a := pool(t)

			if err := a.ForceLeave(t.Context(), name); err != nil {
				t.Fatalf("ForceLeave: %v", err)
			}

			for _, m := range statuses(t, a) {
				if strings.HasPrefix(m, name+" ") {
					t.Errorf("the pool still lists %q", m)
				}
			}
			if got := len(statuses(t, a)); got != 4 {
				t.Errorf("the pool lists %d members, want 4", got)
			}
			wantCalls(t, f, bootstrapCall, nomadfake.Call{Name: "ForceLeave", Arg: name}, nomadfake.Call{Name: "Members"},
				nomadfake.Call{Name: "Members"})
		})
	}
}

// TestForceLeaveOfAnAliveMember checks that a member that is alive shows as leaving at the next read and is gone at the
// read after it.
func TestForceLeaveOfAnAliveMember(t *testing.T) {
	_, a := pool(t)

	if err := a.ForceLeave(t.Context(), "s5.eu"); err != nil {
		t.Fatalf("ForceLeave: %v", err)
	}

	want := []string{"s1.eu alive", "s2.eu failed", "s3.eu left", "s4.eu leaving", "s5.eu leaving"}
	if diff := cmp.Diff(want, statuses(t, a)); diff != "" {
		t.Errorf("first read (-want +got):\n%s", diff)
	}
	want = []string{"s1.eu alive", "s2.eu failed", "s3.eu left", "s4.eu leaving"}
	for i := range 2 {
		if diff := cmp.Diff(want, statuses(t, a)); diff != "" {
			t.Errorf("read %d after it (-want +got):\n%s", i+2, diff)
		}
	}
}

// TestForceLeaveOfALeavingMember checks that a member that the test set to leaving stays, however often it is read.
func TestForceLeaveOfALeavingMember(t *testing.T) {
	_, a := pool(t)

	if err := a.ForceLeave(t.Context(), "s4.eu"); err != nil {
		t.Fatalf("ForceLeave: %v", err)
	}

	for i := range 3 {
		if got := statuses(t, a); len(got) != 5 || got[3] != "s4.eu leaving" {
			t.Errorf("read %d = %v, want s4.eu still leaving", i+1, got)
		}
	}
}

// TestForceLeaveOfAnUnknownMember checks that Nomad's answer 200 for a name that is not in the pool, the node name of a
// member in another region too, is the fake's too, and that the pool stays as it was.
func TestForceLeaveOfAnUnknownMember(t *testing.T) {
	for _, name := range []string{"s9.eu", "s5.us"} {
		t.Run(name, func(t *testing.T) {
			_, a := pool(t)

			if err := a.ForceLeave(t.Context(), name); err != nil {
				t.Fatalf("ForceLeave: %v", err)
			}

			want := []string{"s1.eu alive", "s2.eu failed", "s3.eu left", "s4.eu leaving", "s5.eu alive"}
			for i := range 2 {
				if diff := cmp.Diff(want, statuses(t, a)); diff != "" {
					t.Errorf("read %d (-want +got):\n%s", i+1, diff)
				}
			}
		})
	}
}

// TestForceLeaveRefusesABadNameBeforeAnyCall checks that the fake refuses a name that the client refuses, and logs no
// call.
func TestForceLeaveRefusesABadNameBeforeAnyCall(t *testing.T) {
	f, a := pool(t)

	checkErr(t, a.ForceLeave(t.Context(), ""), "nomadfake: ForceLeave: no member name", false)
	checkErr(t, a.ForceLeave(t.Context(), "s5"), `nomadfake: ForceLeave: member name "s5" has no "."`, false)

	wantCalls(t, f, bootstrapCall)
	if got := len(statuses(t, a)); got != 5 {
		t.Errorf("the pool lists %d members, want 5", got)
	}
}
