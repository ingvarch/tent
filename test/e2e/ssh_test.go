package e2e

// sshArgs returns the arguments of ssh that run a script from stdin as root on ip, with the private key at key
// alone: the user's ssh config and the keys of an ssh agent stay out. It checks no host key, because every run's
// machines are new.
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
		"root@" + ip, "sh", "-s",
	}
}
