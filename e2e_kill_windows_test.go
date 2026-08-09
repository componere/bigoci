//go:build windows

package bigoci_test

import (
	"errors"
	"syscall"
)

// The kill harness drives Unix process groups and signals, which Windows does
// not have. These stubs exist so the e2e files type-check under GOOS=windows —
// the repository's cross-platform gate — while the harness itself never runs
// there: the suite needs Docker-hosted registries and a POSIX kill either way.

// processGroupAttr returns no attributes: Windows has no Setpgid, and nothing
// on this platform ever starts the helper child.
func processGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// signalGroup reports that process-group signalling does not exist here, so a
// row that somehow reached it fails loudly instead of killing nothing.
func signalGroup(int, syscall.Signal) error {
	return errors.New("the kill harness needs unix process groups, which windows does not have")
}
