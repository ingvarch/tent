//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// SSHRun runs script with sh on the machine at ip as root, over ssh with the run's key, and returns its output. A
// failure holds ssh's own error text.
func (s suiteRun) SSHRun(ctx context.Context, ip, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(s.SSHKey, ip)...)
	cmd.WaitDelay = killGrace
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("ssh root@%s: %w: %s", ip, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
