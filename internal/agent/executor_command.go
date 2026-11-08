// executor_command.go implements the command execution branch of TaskExecutor.
//
// A command ticket runs one deterministic program invocation (program + argv,
// never a shell) inside the same run lifecycle as agent tasks: no coding tool,
// no git commit/push/tag, no checkpoint. stdout/stderr stream into the regular
// real-time log channel, and the receipt keeps only the tail of each stream —
// the full output lives in the task log. Retry and routing remain server
// decisions driven by the receipt's error code.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agentprocess "github.com/teammate/agentd/internal/agent/process"
)

// commandTailLimit caps the stdout/stderr tail kept in the completion receipt.
const commandTailLimit = 64 * 1024

// commandWaitDelay bounds how long Wait waits for the output pipes to close
// after the process exits — the safety net for child processes that survive a
// failed tree termination and keep the pipes open.
const commandWaitDelay = 3 * time.Second

// executeCommand runs one command ticket to completion and reports the
// receipt. It never commits to Git and never selects the coding tool.
func (e *TaskExecutor) executeCommand(ctx context.Context, run *RunContext, node TaskNode, workDir string) {
	spec := node.Command
	if spec == nil {
		e.failCommand(run, node, "command_start_failed", "command ticket carries no command spec")
		return
	}

	program, err := resolveCommandProgram(workDir, spec.Program)
	if err != nil {
		e.failCommand(run, node, "command_start_failed", err.Error())
		return
	}
	dir, err := resolveCommandWorkDir(workDir, spec.WorkDir)
	if err != nil {
		e.failCommand(run, node, "command_start_failed", err.Error())
		return
	}

	runCtx := ctx
	if node.TimeoutMinutes > 0 {
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeout(ctx, time.Duration(node.TimeoutMinutes)*time.Minute)
		defer cancelTimeout()
	}

	cmd := exec.CommandContext(runCtx, program, spec.Args...)
	cmd.Dir = dir
	env := os.Environ()
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdoutTail := newOutputTail()
	stderrTail := newOutputTail()
	stdoutWriter := newLineReportWriter(func(line string) {
		e.reportCommandOutput(run, node, "stdout", line, stdoutTail)
	})
	stderrWriter := newLineReportWriter(func(line string) {
		e.reportCommandOutput(run, node, "stderr", line, stderrTail)
	})
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

	// Context cancellation (timeout or interrupt) must kill the whole process
	// tree: the default cancellation only kills the direct child, while
	// children spawned by the command keep the pipes open and the machine
	// dirty. exec invokes Cancel at most once per execution.
	cmd.Cancel = func() error {
		return agentprocess.TerminateTree(cmd, agentprocess.DefaultTerminateTimeout)
	}
	cmd.WaitDelay = commandWaitDelay

	agentprocess.PrepareCommand(cmd)
	log.Printf("[executor] running command %s %v in %s", spec.Program, spec.Args, dir)
	if err := cmd.Start(); err != nil {
		e.failCommand(run, node, "command_start_failed", fmt.Sprintf("start %s: %v", spec.Program, err))
		return
	}

	waitErr := cmd.Wait()
	// A trailing chunk without a newline is still real output.
	stdoutWriter.flush()
	stderrWriter.flush()

	// Interrupt owns the interrupt reporting path; SoftInterrupt keeps the
	// node in_progress while a human works locally. Neither may leak a
	// receipt here.
	if e.interrupted.Load() || e.softInterrupted.Load() {
		return
	}

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		e.failCommand(run, node, "command_timeout", fmt.Sprintf("command %s exceeded the %d minute timeout", spec.Program, node.TimeoutMinutes))
	case waitErr == nil || errors.Is(waitErr, exec.ErrWaitDelay):
		if exitCode == 0 {
			summary := fmt.Sprintf("Command %s exited with code 0", spec.Program)
			if err := run.Client.CompleteNode(context.Background(), run.AgentID, run.TaskID, node.ID, node.ExecutionID, summary, commandReceiptOutput(0, stdoutTail, stderrTail)); err != nil {
				log.Printf("[executor] failed to complete command node: %v", err)
			}
			e.notifyExecutionCompleted(run.TaskID, node.ID)
			return
		}
		e.failCommand(run, node, "command_exit_nonzero", fmt.Sprintf("program %s exited with code %d", spec.Program, exitCode))
	case errors.As(waitErr, &exitErr):
		e.failCommand(run, node, "command_exit_nonzero", fmt.Sprintf("program %s exited with code %d", spec.Program, exitErr.ExitCode()))
	default:
		e.failCommand(run, node, "command_failed", waitErr.Error())
	}
}

// reportCommandOutput sends one desensitized output line into the regular log
// channel (server message + local log buffer) and the receipt tail.
func (e *TaskExecutor) reportCommandOutput(run *RunContext, node TaskNode, stream, line string, tail *outputTail) {
	line = desensitizeOutputLine(line)
	log.Printf("[command:%s] %s", stream, line)
	tail.write([]byte(line + "\n"))
	if e.logBuffer != nil {
		e.logBuffer.Append(LogLine{TaskID: run.TaskID, NodeID: node.ID, Line: line})
	}
	if err := run.Client.SendMessageWithType(context.Background(), run.AgentID, run.TaskID, node.ID, stream, line); err != nil {
		log.Printf("[executor] failed to send command %s log: %v", stream, err)
	}
}

// failCommand reports a command failure through FailNode with an execution
// error code; retry and routing remain server decisions.
func (e *TaskExecutor) failCommand(run *RunContext, node TaskNode, code, message string) {
	err := fmt.Errorf("%s: %s", code, message)
	log.Printf("[executor] command node %s failed: %v", node.ID, err)
	e.notifyExecutionFailed(run.TaskID, node.ID, err)
	if reportErr := run.Client.FailNode(context.Background(), run.AgentID, run.TaskID, node.ID, node.ExecutionID, code, message); reportErr != nil {
		log.Printf("[executor] failed to report command failure: %v", reportErr)
	}
}

// lineReportWriter splits a command output stream into lines and reports each
// complete line through the configured callback; a trailing chunk without a
// newline stays buffered until flush.
type lineReportWriter struct {
	mu     sync.Mutex
	buf    []byte
	report func(line string)
}

func newLineReportWriter(report func(line string)) *lineReportWriter {
	return &lineReportWriter{report: report}
}

func (w *lineReportWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]
		w.report(strings.TrimSuffix(line, "\r"))
	}
	return len(p), nil
}

// flush reports any buffered trailing chunk that lacks a newline.
func (w *lineReportWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return
	}
	line := string(w.buf)
	w.buf = w.buf[:0]
	w.report(strings.TrimSuffix(line, "\r"))
}

// commandReceiptOutput builds the structured success receipt: exit code plus
// the tail of each output stream. The full output lives in the task log.
func commandReceiptOutput(exitCode int, stdoutTail, stderrTail *outputTail) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"exit_code":   exitCode,
		"stdout_tail": stdoutTail.String(),
		"stderr_tail": stderrTail.String(),
	})
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

// isMachineAbsolutePath reports whether p references a machine location by
// itself: a volume-qualified path, or a path rooted at a drive/filesystem
// root. Volume-less rooted paths (e.g. "/abs" on Windows) still resolve
// outside any task workspace, so they count as absolute too.
func isMachineAbsolutePath(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	if filepath.VolumeName(p) != "" {
		return true
	}
	return strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\")
}

// resolveCommandProgram validates the program of a command ticket: a PATH
// name, or a relative executable inside the task workspace. Absolute paths and
// paths resolving outside the workspace are rejected — the command must never
// become a shell-driven entry point to arbitrary machine locations.
func resolveCommandProgram(workDir, program string) (string, error) {
	if program == "" {
		return "", fmt.Errorf("command spec has no program")
	}
	if isMachineAbsolutePath(program) {
		return "", fmt.Errorf("command program must be a PATH name or a task-workspace-relative path, got absolute path %q", program)
	}
	if strings.ContainsRune(program, '/') || strings.ContainsRune(program, '\\') {
		resolved := filepath.Join(workDir, filepath.FromSlash(program))
		if !containedInWorkspace(workDir, resolved) {
			return "", fmt.Errorf("command program %q resolves outside the task workspace", program)
		}
		return resolved, nil
	}
	return program, nil
}

// resolveCommandWorkDir validates the command's working directory: empty means
// the task repository root; anything else must stay inside the task workspace.
func resolveCommandWorkDir(taskWorkDir, specWorkDir string) (string, error) {
	if specWorkDir == "" {
		return taskWorkDir, nil
	}
	if isMachineAbsolutePath(specWorkDir) {
		return "", fmt.Errorf("command workdir must be a task-workspace-relative path, got absolute path %q", specWorkDir)
	}
	resolved := filepath.Join(taskWorkDir, filepath.FromSlash(specWorkDir))
	if !containedInWorkspace(taskWorkDir, resolved) {
		return "", fmt.Errorf("command workdir %q resolves outside the task workspace", specWorkDir)
	}
	return resolved, nil
}

// containedInWorkspace reports whether target lies inside base (or is base).
func containedInWorkspace(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// outputTail keeps the last commandTailLimit bytes of one output stream.
// String drops leading bytes that would split a UTF-8 sequence, so the
// receipt never carries a broken rune.
type outputTail struct {
	mu  sync.Mutex
	buf []byte
}

func newOutputTail() *outputTail {
	return &outputTail{buf: make([]byte, 0, commandTailLimit)}
}

func (t *outputTail) write(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > commandTailLimit {
		t.buf = t.buf[len(t.buf)-commandTailLimit:]
	}
}

func (t *outputTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buf
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r != utf8.RuneError || size > 1 {
			break
		}
		b = b[1:]
	}
	return string(b)
}
