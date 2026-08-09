//go:build unix

package bigoci_test

import "syscall"

// The kill harness needs two things only Unix can give it: a process group of
// the child's own, and a signal aimed at that group. They live behind these
// two functions so the rest of the harness — and the e2e files that share its
// helpers — still type-check on Windows, where the suite never runs but the
// repository's cross-platform check must not rot.

// processGroupAttr returns the attributes that put the child in a fresh
// process group, so a signal aimed at the group reaches the transfer and
// nothing else — least of all the test process, which shares the terminal.
func processGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to the whole process group pid leads.
func signalGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
