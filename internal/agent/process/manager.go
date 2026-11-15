// Package process provides cross-platform helpers for managing tool processes.
// Tool process trees are bound to the daemon lifetime: on Linux the child
// re-execs through an in-binary wrapper that arms the parent-death signal and
// verifies parentage (closing the fork/prctl race); on Windows the tree is
// assigned to a KILL_ON_JOB_CLOSE Job Object owned by the daemon process.
package process

import (
	"os/exec"
	"time"
)

const (
	DefaultTerminateTimeout = 5 * time.Second
	// ToolWrapperArgName is the hidden daemon subcommand that re-execs tool
	// commands under the parent-death signal (Linux only).
	ToolWrapperArgName = "__tool-wrapper"
)

// PrepareCommand applies platform-specific process attributes before Start.
// On Linux it also rewrites the command to run through the daemon's own
// __tool-wrapper entry so the child arms PDEATHSIG itself before exec'ing.
func PrepareCommand(cmd *exec.Cmd) {
	prepareCommand(cmd)
}

// AttachAfterStart binds a started process tree to the daemon lifetime.
// It must be called right after cmd.Start() succeeds. The returned release
// function detaches the OS-level binding and must run only after the process
// tree has been reaped; on Windows releasing closes the Job Object handle.
// On platforms where the binding is established at Start it is a no-op.
func AttachAfterStart(cmd *exec.Cmd) (release func()) {
	return attachAfterStart(cmd)
}

// TerminateTree terminates cmd and its child processes.
func TerminateTree(cmd *exec.Cmd, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultTerminateTimeout
	}
	return terminateTree(cmd, timeout)
}
