//go:build linux

// manager_linux.go rewrites tool commands to run through the daemon's own
// __tool-wrapper entry. The wrapper arms PR_SET_PDEATHSIG in the child and
// verifies the parent's identity before exec'ing, so a tool process tree
// cannot outlive a daemon that dies between fork and the child's prctl call
// (the race that a parent-side-only Pdeathsig cannot close).
package process

import (
	"log"
	"os"
	"os/exec"
	"strconv"
)

func prepareLinuxWrapper(cmd *exec.Cmd) {
	exe, err := os.Executable()
	if err != nil {
		// No wrapper possible: the child still runs, but its lifetime is no
		// longer tied to the daemon by the parent-death signal.
		log.Printf("[process] resolve executable for tool wrapper: %v", err)
		return
	}
	origPath, origArgs := cmd.Path, cmd.Args
	if len(origArgs) == 0 {
		origArgs = []string{origPath}
	}
	// The resolved path must travel separately from the argv: the wrapper
	// execs origPath directly (syscall.Exec does not search PATH), while
	// origArgs[0] stays the tool's own argv[0].
	cmd.Path = exe
	cmd.Args = append([]string{exe, ToolWrapperArgName, strconv.Itoa(os.Getpid()), origPath, "--"}, origArgs...)
}

func attachAfterStart(cmd *exec.Cmd) func() {
	// The parent-death signal is armed inside the child by the wrapper; there
	// is no post-start OS handle to hold on Linux.
	return func() {}
}
