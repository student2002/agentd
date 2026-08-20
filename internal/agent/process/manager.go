// Package process 提供用于管理工具进程的跨平台辅助函数。
package process

import (
	"os/exec"
	"time"
)

const DefaultTerminateTimeout = 5 * time.Second

// PrepareCommand 在 Start 之前应用平台特定的进程属性。
func PrepareCommand(cmd *exec.Cmd) {
	prepareCommand(cmd)
}

// TerminateTree 终止 cmd 及其子进程。
func TerminateTree(cmd *exec.Cmd, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultTerminateTimeout
	}
	return terminateTree(cmd, timeout)
}
