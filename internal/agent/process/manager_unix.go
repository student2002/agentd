//go:build !windows

// manager_unix.go provides the Unix process management implementation.
package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func prepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateTree(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	time.Sleep(timeout)
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
