//go:build !unix

package review

import "os/exec"

// ownGroup leaves cmd as it is: only Unix has process groups to kill.
func ownGroup(*exec.Cmd) {}
