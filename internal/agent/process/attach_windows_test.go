//go:build windows

// attach_windows_test.go verifies the KILL_ON_JOB_CLOSE binding with a real
// process tree built from the test binary itself (helper-process pattern):
// the daemon's child spawns a grandchild only after the job assignment, so
// the grandchild inherits the membership; releasing the job handle terminates
// both, which is exactly the daemon-death behaviour.
package process

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	jobTestRoleEnv      = "JOB_TEST_ROLE"
	jobTestAssignedEnv  = "JOB_TEST_ASSIGNED_FILE"
	jobTestPIDFileEnv   = "JOB_TEST_PID_FILE"
	jobTestWaitDuration = 10 * time.Minute
)

func TestJobObjectKillsTreeOnRelease(t *testing.T) {
	switch os.Getenv(jobTestRoleEnv) {
	case "child":
		runJobTestChild()
		return
	case "grandchild":
		runJobTestGrandchild()
		return
	}

	tmp := t.TempDir()
	assignedFile := filepath.Join(tmp, "assigned")
	pidFile := filepath.Join(tmp, "grandchild.pid")

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestJobObjectKillsTreeOnRelease", "-test.v=false")
	cmd.Env = append(os.Environ(),
		jobTestRoleEnv+"=child",
		jobTestAssignedEnv+"="+assignedFile,
		jobTestPIDFileEnv+"="+pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	release := AttachAfterStart(cmd)
	// Signal the child to spawn the grandchild only now: anything it creates
	// after the assignment inherits the job membership.
	if err := os.WriteFile(assignedFile, []byte("1"), 0o644); err != nil {
		t.Fatalf("write assignment signal: %v", err)
	}

	grandchild := waitForGrandchildPID(t, pidFile)
	if !processAlive(grandchild) {
		t.Fatalf("grandchild %d died before release", grandchild)
	}

	release()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(grandchild) {
			return // the job took the whole tree down with it
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild %d survived the job release", grandchild)
}

// waitForGrandchildPID polls the pid file the child writes once its own child
// (the grandchild under job membership) is up.
func waitForGrandchildPID(t *testing.T, path string) uint32 {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var pid uint32
			if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err == nil && pid != 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild pid file %s never appeared", path)
	return 0
}

func processAlive(pid uint32) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false
	}
	// WAIT_TIMEOUT means the wait expired without the object being signalled,
	// i.e. the process is still running; WAIT_OBJECT_0 means it exited.
	return event == uint32(windows.WAIT_TIMEOUT)
}

// runJobTestChild waits for the assignment signal, spawns the grandchild and
// reports its pid, then blocks until the tree is killed.
func runJobTestChild() {
	for {
		if _, err := os.Stat(os.Getenv(jobTestAssignedEnv)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	grandchild := exec.Command(exe, "-test.run=TestJobObjectKillsTreeOnRelease", "-test.v=false")
	grandchild.Env = append(os.Environ(), jobTestRoleEnv+"=grandchild", jobTestPIDFileEnv+"="+os.Getenv(jobTestPIDFileEnv))
	if err := grandchild.Start(); err != nil {
		return
	}
	_ = os.WriteFile(os.Getenv(jobTestPIDFileEnv), []byte(fmt.Sprint(grandchild.Process.Pid)), 0o644)
	_ = grandchild.Wait() // returns when the job release (or nothing else) kills it
}

// runJobTestGrandchild records its pid and waits to be killed by the job.
func runJobTestGrandchild() {
	_ = os.WriteFile(os.Getenv(jobTestPIDFileEnv), []byte(fmt.Sprint(os.Getpid())), 0o644)
	time.Sleep(jobTestWaitDuration)
}
