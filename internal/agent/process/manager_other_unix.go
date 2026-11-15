//go:build !windows && !linux

// manager_other_unix.go provides stubs for Unix platforms without the Linux
// parent-death wrapper (e.g. macOS development builds).
package process

import "os/exec"

func prepareLinuxWrapper(cmd *exec.Cmd) {}

func attachAfterStart(cmd *exec.Cmd) func() {
	return func() {}
}
