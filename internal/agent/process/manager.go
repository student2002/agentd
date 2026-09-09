// Package process provides cross-platform helpers for managing tool processes.
package process

import (
	"os/exec"
	"time"
)

const DefaultTerminateTimeout = 5 * time.Second

// PrepareCommand applies platform-specific process attributes before Start.
func PrepareCommand(cmd *exec.Cmd) {
	prepareCommand(cmd)
}

// TerminateTree terminates cmd and its child processes.
func TerminateTree(cmd *exec.Cmd, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultTerminateTimeout
	}
	return terminateTree(cmd, timeout)
}
