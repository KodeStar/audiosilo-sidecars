//go:build !unix

package agent

import "os/exec"

// setProcGroup is a no-op on platforms without POSIX process groups (Windows). The
// timeout still kills the immediate child via killProcGroup.
func setProcGroup(_ *exec.Cmd) {}

// killProcGroup kills the immediate child process. Grandchildren are not reaped on
// these platforms, which is an accepted limitation.
func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
