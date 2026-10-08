package app

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestApplyNodeRefusesACreateAndAWait fails a create and a wait, for which applyNode has no user data: it reports the
// change as started and as failed, returns the error, and asks the cloud for no machine.
func TestApplyNodeRefusesACreateAndAWait(t *testing.T) {
	const cause = "node prod-servers-0: no user data"
	for _, tc := range []struct {
		action NodeAction
		want   string
	}{
		{NodeCreate, cause},
		{NodeWait, "wait for node prod-servers-0: " + cause},
	} {
		t.Run(tc.action.String(), func(t *testing.T) {
			var got []string
			s := testService(&got)
			nodes := &recordingNodes{}
			c := NodeChange{Action: tc.action, Name: "prod-servers-0"}

			err := s.applyNode(t.Context(), nodes, "prod", c)

			if err == nil || err.Error() != tc.want {
				t.Errorf("applyNode error = %v\nwant            %s", err, tc.want)
			}
			if len(nodes.creates) != 0 {
				t.Errorf("applyNode asked the cloud for %d machines, want none", len(nodes.creates))
			}
			change := tc.action.String() + " prod-servers-0 "
			if diff := cmp.Diff([]string{change + "started", change + "failed: " + cause}, got); diff != "" {
				t.Errorf("the steps that applyNode reported (-want +got):\n%s", diff)
			}
		})
	}
}
