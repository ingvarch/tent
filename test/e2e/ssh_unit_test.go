package e2e

import (
	"reflect"
	"testing"
)

func TestSSHArgsRunTheScriptFromStdinAsRootWithoutHostKeyChecks(t *testing.T) {
	want := []string{
		"-F", "/dev/null",
		"-i", "/r/ssh/id_ed25519",
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"root@203.0.113.9", "sh", "-s",
	}
	if got := sshArgs("/r/ssh/id_ed25519", "203.0.113.9"); !reflect.DeepEqual(got, want) {
		t.Errorf("sshArgs = %q, want %q", got, want)
	}
}
