package app

import (
	"testing"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/rollout"
)

// TestRolloutRoleLabelIsTheCloudRoleLabel checks that the label that a role refusal names is the one the cloud sets.
func TestRolloutRoleLabelIsTheCloudRoleLabel(t *testing.T) {
	t.Parallel()
	if rollout.RoleLabel != cloud.LabelRole {
		t.Errorf("rollout.RoleLabel = %q, want cloud.LabelRole %q", rollout.RoleLabel, cloud.LabelRole)
	}
}
