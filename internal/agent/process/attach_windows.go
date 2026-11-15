//go:build windows

// attach_windows.go binds started process trees to a KILL_ON_JOB_CLOSE Job
// Object owned by the daemon process: when the daemon dies without reaping
// its children (crash, kill, power loss), the OS closes the handle and the
// kernel terminates every process in the job, including grandchildren.
package process

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

func attachAfterStart(cmd *exec.Cmd) (release func()) {
	noop := func() {}
	if cmd == nil || cmd.Process == nil {
		return noop
	}

	var job windows.Handle
	var assignErr error
	// os.Process.WithHandle exposes the real child handle: assigning through
	// it avoids the PID-reuse window an OpenProcess(by pid) re-open has, and
	// the handle stays owned by os.Process (never closed here).
	err := cmd.Process.WithHandle(func(handle uintptr) {
		job, assignErr = windows.CreateJobObject(nil, nil)
		if assignErr != nil {
			return
		}
		limitInfo := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		limitInfo.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, assignErr = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&limitInfo)), uint32(unsafe.Sizeof(limitInfo))); assignErr != nil {
			return
		}
		assignErr = windows.AssignProcessToJobObject(job, windows.Handle(handle))
	})
	if err != nil || assignErr != nil || job == 0 {
		if job != 0 {
			_ = windows.CloseHandle(job)
		}
		return noop
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			// Closing the last job handle terminates the whole tree; this is
			// the intended daemon-death behaviour, so the release path never
			// calls TerminateJobObject explicitly.
			_ = windows.CloseHandle(job)
		})
	}
}
