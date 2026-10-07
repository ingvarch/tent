package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
)

// stderrTailLines is how many lines of tent's stderr a failure message quotes.
const stderrTailLines = 20

// lastLines returns the last n lines of text, without the blank lines at its end and without a final newline.
func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// tentFailure describes a tent command that did not exit with 0: the step, the exit code, the log and the last
// lines of stderr.
func tentFailure(step string, res tentResult, log string) string {
	head := fmt.Sprintf("tent %s exited with code %d", step, res.Code)
	if res.Code == -1 {
		head = fmt.Sprintf("tent %s was ended by a signal or by its context", step)
	}
	stderr := lastLines(string(res.Stderr), stderrTailLines)
	if stderr == "" {
		stderr = "(empty)"
	}
	return fmt.Sprintf("%s\nlog: %s\nlast %d lines of stderr:\n%s", head, log, stderrTailLines, stderr)
}

// logPath is the log that run writes for a step of a cluster.
func (r tentRunner) logPath(cluster, step string) string {
	return filepath.Join(r.Dir, cluster+"-"+step+".log")
}

// storeURL is the --state URL of a cluster's store in this run.
func (r tentRunner) storeURL(cluster string) string {
	return stateURL(filepath.Join(r.Dir, "state-"+cluster))
}

// deleteCommand is the command line that deletes a cluster of this run, for a run that keeps its clusters.
func (r tentRunner) deleteCommand(cluster string) string {
	return fmt.Sprintf("%s delete cluster %s --yes --state %s", r.Bin, cluster, r.storeURL(cluster))
}

// createArgs are the arguments of tent that create a cluster of one server and one worker, open to the runner alone.
func createArgs(cfg settings, cluster, image, sshPub, runner string) []string {
	return []string{
		"create", "cluster", cluster,
		"--provider", "vultr", "--region", cfg.Region, "--machine-type", cfg.Plan,
		"--servers", "1", "--allow-single-server", "--workers", "1",
		"--image", image, "--ssh-key", sshPub,
		"--ssh-access", runner, "--api-access", runner, "--yes",
	}
}

// validateWait is how long the validate steps let tent wait for a valid cluster.
const validateWait = "5m"

// validateArgs are the arguments of tent that check a cluster of this suite, which has one server, until it is valid.
func validateArgs(cluster string) []string {
	return []string{"validate", "cluster", cluster, "--wait", validateWait, "--allow-single-server"}
}

// deleteInCleanup tells whether the cleanup deletes the cluster: when no delete ran, or when a signal ended the
// delete before it finished. A delete that failed on its own is not repeated.
func deleteInCleanup(ran, done, interrupted bool) bool {
	return !done && (!ran || interrupted)
}
