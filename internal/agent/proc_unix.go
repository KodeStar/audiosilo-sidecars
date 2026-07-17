//go:build unix

package agent

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the child in its own process group so a timeout can kill the
// whole tree (an agent CLI spawns helper subprocesses) in one signal.
func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcGroup sends SIGKILL to the child's whole process group (negative pid). It
// falls back to killing the immediate process if the group signal fails.
func killProcGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
