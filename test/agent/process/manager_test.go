// Package process_test 包含 process 包的测试，验证进程树终止功能。
package process_test

import (
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent/process"
)

func TestTerminateTreeStopsProcess(t *testing.T) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-Command", "Start-Sleep -Seconds 60")
	} else {
		cmd = exec.Command("sh", "-c", "sleep 60")
	}

	process.PrepareCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}

	if err := process.TerminateTree(cmd, 2*time.Second); err != nil {
		t.Fatalf("terminate tree: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after TerminateTree")
	}
}
