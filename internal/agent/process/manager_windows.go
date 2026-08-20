//go:build windows

// manager_windows.go 提供 Windows 平台的进程管理实现。
package process

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func prepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

func terminateTree(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	pid := cmd.Process.Pid
	taskkill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	if err := taskkill.Run(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = cmd.Process.Kill()
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	select {
	case <-deadline.C:
		return nil
	default:
		return nil
	}
}
