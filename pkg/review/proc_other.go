//go:build !unix

package review

import "os/exec"

// ownGroup leaves cmd as it is: only Unix has process groups to kill.
func ownGroup(*exec.Cmd) {}

// noTerminal leaves cmd as it is: only Unix has controlling terminals.
func noTerminal(*exec.Cmd) {}

// killGroup does nothing: only Unix has process groups to kill.
func killGroup(*exec.Cmd) {}
