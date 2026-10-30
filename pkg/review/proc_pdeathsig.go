//go:build linux || freebsd

package review

import "syscall"

// dieWithParent has the kernel kill the agent should deps-handler die
// without the chance to kill its group, as on SIGKILL: in a group of its
// own, the agent gets no signal meant for deps-handler's terminal or job.
// The processes the agent started are not covered, but lose their parent.
func dieWithParent(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }
