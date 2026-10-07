package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// sshTimeout is how long one run of ssh may take.
var sshTimeout = time.Minute

// sshRun runs script with sh on the machine at ip as root, over ssh with the private key at key, and returns its
// output. It ends ssh after sshTimeout. A failure holds ssh's own error text.
func sshRun(ctx context.Context, key, ip, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(key, ip)...)
	cmd.WaitDelay = killGrace
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("ssh root@%s: %w: %s", ip, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// sshArgs returns the arguments of ssh that run a script from stdin as root on ip, with the private key at key
// alone: the user's ssh config and the keys of an ssh agent stay out. It checks no host key, because every run's
// machines are new. ServerAlive options end a connection that has gone silent.
func sshArgs(key, ip string) []string {
	return []string{
		"-F", "/dev/null",
		"-i", key,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"root@" + ip, "sh", "-s",
	}
}
