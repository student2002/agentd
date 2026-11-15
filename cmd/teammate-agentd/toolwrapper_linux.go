//go:build linux

// toolwrapper_linux.go implements the __tool-wrapper entry: the daemon
// re-execs tool commands through this in-binary wrapper so the child arms
// PR_SET_PDEATHSIG itself and verifies parentage before exec'ing, tying the
// whole tool process tree to the daemon's lifetime at the OS level.
package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/spf13/cobra"

	"github.com/teammate/agentd/internal/agent/process"
)

func toolWrapperCommand() *cobra.Command {
	return &cobra.Command{
		Use:    process.ToolWrapperArgName,
		Short:  "internal: exec a tool command under the parent-death signal",
		Hidden: true,
		// Tool argv must pass through untouched: the arguments are the
		// expected parent pid, the resolved tool path (syscall.Exec does not
		// search PATH), the "--" separator, then the tool's own argv.
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 4 || args[2] != "--" {
				return fmt.Errorf("usage: %s <parent-pid> <tool-path> -- <command> [args...]", process.ToolWrapperArgName)
			}
			parentPID, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("parse parent pid %q: %w", args[0], err)
			}

			if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil {
				return fmt.Errorf("set parent-death signal: %w", err)
			}
			// The daemon may have died between its fork() and this prctl: a
			// parent already gone means the signal would never fire and the
			// tool would outlive the daemon, so exit instead of exec'ing.
			if syscall.Getppid() != parentPID {
				return fmt.Errorf("parent %d is gone", parentPID)
			}

			return syscall.Exec(args[1], args[3:], os.Environ())
		},
	}
}
