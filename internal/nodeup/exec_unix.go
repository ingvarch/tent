//go:build unix

package nodeup

import (
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
)

// stopGroup makes cmd start in a process group of its own and send SIGTERM to the whole group when its context ends.
// Call the function it returns once cmd.Wait has returned: when Cancel sent SIGTERM, it sends SIGKILL to what is left
// of the group.
func stopGroup(cmd *exec.Cmd) (afterWait func()) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stopped atomic.Bool
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		stopped.Store(err == nil)
		return err
	}
	return func() {
		// A group's id stays taken while a process of the group lives, so this kill can reach another group only if
		// the kernel reused the id between the group's end and the end of the wait: a short time, at most WaitDelay.
		if stopped.Load() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
}
