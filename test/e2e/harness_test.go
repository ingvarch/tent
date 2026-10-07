//go:build e2e

package e2e

import "context"

// sshRun runs script on the machine at ip with the run's SSH key; see the function sshRun.
func (s suiteRun) sshRun(ctx context.Context, ip, script string) (string, error) {
	return sshRun(ctx, s.SSHKey, ip, script)
}
