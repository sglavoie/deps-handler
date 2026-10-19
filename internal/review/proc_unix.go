//go:build unix

package review

import (
	"os/exec"
	"syscall"
)

// ownGroup runs cmd in a process group of its own, and makes cancelling it
// kill the whole group, so processes an agent started do not outlive it.
// cmd must come from exec.CommandContext.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	dieWithParent(cmd.SysProcAttr)
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// killGroup kills what is left of the process group of cmd, which has
// been waited for. A group already gone is not an error worth reporting.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
