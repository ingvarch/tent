//go:build !unix

package nodeup

import "os/exec"

// stopGroup leaves cmd as it is: when its context ends, exec kills the program alone.
func stopGroup(*exec.Cmd) (afterWait func()) { return func() {} }
