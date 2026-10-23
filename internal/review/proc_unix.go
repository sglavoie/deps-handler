//go:build unix

package review

import (
	"os/exec"
	"syscall"
)

// ownGroup runs cmd in a process group of its own, and makes cancelling it
// kill the whole group, so processes an agent started do not outlive it.
// cmd must come from exec.CommandContext. One that noTerminal set up leads
// a session, and so a group, of its own already.
func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = !cmd.SysProcAttr.Setsid
	dieWithParent(cmd.SysProcAttr)
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// noTerminal runs cmd in a session of its own, without a controlling
// terminal: what it starts that opens /dev/tty to ask something, as ssh
// does about a host key or a passphrase, fails at once instead of being
// stopped, in a background group, until the review times out.
func noTerminal(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// killGroup kills what is left of the process group of cmd, which has
// been waited for. A group already gone is not an error worth reporting.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
