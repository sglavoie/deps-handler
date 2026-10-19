//go:build unix && !linux && !freebsd

package review

import "syscall"

// dieWithParent does nothing: only Linux and FreeBSD can have the kernel
// kill a child when its parent dies.
func dieWithParent(*syscall.SysProcAttr) {}
