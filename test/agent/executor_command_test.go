// executor_command_test.go verifies the command execution branch: argv stays
// verbatim (no shell), PATH and workspace-relative programs both run,
// workspace escapes are rejected, failures carry execution error codes, and
// interrupt kills the whole process tree.
package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/teammate/agentd/internal/agent"
)

// buildCommandHelper compiles a small helper program used to exercise the
// command branch. It relies only on the Go toolchain that is already running
// the tests, never on globally installed tools.
func buildCommandHelper(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	name := "command-helper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(binDir, name)

	srcDir := filepath.Join(binDir, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("mkdir helper src: %v", err)
	}
	src := filepath.Join(srcDir, "main.go")
	code := `package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case "echo":
		for _, a := range args[1:] {
			fmt.Println(a)
		}
	case "fail":
		os.Exit(3)
	case "stream":
		fmt.Println("out-1")
		fmt.Fprintln(os.Stderr, "err-1")
		fmt.Println("out-2")
	case "hang":
		seconds := 10
		if len(args) > 1 {
			fmt.Sscanf(args[1], "%d", &seconds)
		}
		time.Sleep(time.Duration(seconds) * time.Second)
	case "spawn":
		fmt.Println("spawned")
		child := exec.Command(os.Args[0], "pulse", os.Args[2])
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		_ = child.Run()
	case "pulse":
		for i := 0; i < 300; i++ {
			f, err := os.OpenFile(os.Args[2], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err == nil {
				f.WriteString("x")
				f.Close()
			}
			time.Sleep(200 * time.Millisecond)
		}
	case "env":
		fmt.Println(os.Getenv(args[1]))
	case "bigutf8":
		for i := 0; i < 50; i++ {
			line := make([]rune, 700)
			for j := range line {
				line[j] = '中'
			}
			fmt.Println(string(line))
		}
	}
}
`
	if err := os.WriteFile(src, []byte(code), 0644); err != nil {
		t.Fatalf("write helper source: %v", err)
	}
	build := exec.Command("go", "build", "-o", bin, src)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}
	return bin
}

// commandTestEnv wires a receipt server and an executor whose task workspace
// pre-seeded with the helper binary, mirroring the executor's isolated
// per-task workdir layout.
func commandTestEnv(t *testing.T) (*agent.TaskExecutor, *agent.Client, *receiptRecorder, string, agent.RunContext) {
	t.Helper()
	t.Setenv("TEAMMATE_DISK_QUOTA_GB", "100")
	cfg := &agent.Config{
		Server:    agent.ServerConfig{URL: "http://127.0.0.1:1"},
		Agent:     agent.AgentInfo{Name: "Agent One", Provider: "claude"},
		Workspace: agent.WorkspaceConfig{Root: t.TempDir()},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}
	client, rec := newReceiptServer(t)
	executor := agent.NewTaskExecutorWithObserver(cfg, &recordingExecutionObserver{})

	workDir := filepath.Join(cfg.Workspace.Root, "ws-1", "agent-1", "proj-1", "12")
	run := agent.RunContext{
		Client:      client,
		AgentID:     "agent-1",
		WorkspaceID: "ws-1",
		ProjectID:   "proj-1",
		TaskID:      12,
		NodeID:      "n-recv",
	}
	return executor, client, rec, workDir, run
}

// seedHelper places the helper binary inside the task workspace under the
// given relative name so a command ticket can invoke it workspace-relatively.
func seedHelper(t *testing.T, workDir, name string) {
	t.Helper()
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	data, err := os.ReadFile(buildCommandHelper(t))
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, name), data, 0755); err != nil {
		t.Fatalf("seed helper: %v", err)
	}
}

func commandNode(spec *agent.CommandSpec, timeoutMinutes int32) agent.TaskNode {
	return agent.TaskNode{
		ID:             "n-recv",
		TaskID:         12,
		Name:           "run tests",
		Status:         "running",
		ExecutionID:    "exec-recv",
		ExecutionKind:  agent.ExecutionKindCommand,
		Command:        spec,
		TimeoutMinutes: timeoutMinutes,
	}
}

func receiptMap(t *testing.T, raw interface{}) map[string]interface{} {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal receipt: %v", err)
	}
	return m
}

// TestCommandTicketRunsArgvVerbatim proves argv boundaries survive: an
// argument full of shell metacharacters reaches the program as one literal
// string, and the workspace-relative program runs.
func TestCommandTicketRunsArgvVerbatim(t *testing.T) {
	executor, _, rec, workDir, run := commandTestEnv(t)
	seedHelper(t, workDir, "helper.exe")

	weird := `a b & | ; < > $() "quote"`
	spec := &agent.CommandSpec{
		Program: "./helper.exe",
		Args:    []string{"echo", weird},
	}
	executor.Execute(run, commandNode(spec, 0))

	fails, completes, manuals := rec.snapshot()
	if len(fails) != 0 || len(manuals) != 0 {
		t.Fatalf("expected success receipts only, got fails=%d manuals=%d", len(fails), len(manuals))
	}
	if len(completes) != 1 {
		t.Fatalf("expected exactly one /complete receipt, got %d", len(completes))
	}
	output := receiptMap(t, completes[0]["output"])
	if code, ok := output["exit_code"].(float64); !ok || code != 0 {
		t.Fatalf("exit_code = %#v, want 0", output["exit_code"])
	}
	stdout, _ := output["stdout_tail"].(string)
	if stdout != weird+"\n" {
		t.Fatalf("stdout_tail = %q, want the verbatim argument %q (shell must not interpret it)", stdout, weird)
	}
	if stderr, _ := output["stderr_tail"].(string); stderr != "" {
		t.Fatalf("stderr_tail = %q, want empty", stderr)
	}
}

// TestCommandTicketPATHProgram proves a program resolved from PATH runs.
func TestCommandTicketPATHProgram(t *testing.T) {
	executor, _, rec, _, run := commandTestEnv(t)

	spec := &agent.CommandSpec{Program: "go", Args: []string{"version"}}
	executor.Execute(run, commandNode(spec, 0))

	fails, completes, _ := rec.snapshot()
	if len(fails) != 0 {
		t.Fatalf("unexpected /fail receipts: %#v", fails)
	}
	if len(completes) != 1 {
		t.Fatalf("expected exactly one /complete receipt, got %d", len(completes))
	}
	output := receiptMap(t, completes[0]["output"])
	if code, ok := output["exit_code"].(float64); !ok || code != 0 {
		t.Fatalf("exit_code = %#v, want 0", output["exit_code"])
	}
	if stdout, _ := output["stdout_tail"].(string); len(stdout) == 0 {
		t.Error("stdout_tail should carry the go version output")
	}
}

// TestCommandTicketStreamsToLogChannel proves stdout/stderr are split into
// their typed log messages and land in the receipt tails.
func TestCommandTicketStreamsToLogChannel(t *testing.T) {
	executor, _, rec, workDir, run := commandTestEnv(t)
	seedHelper(t, workDir, "helper.exe")

	spec := &agent.CommandSpec{Program: "./helper.exe", Args: []string{"stream"}}
	executor.Execute(run, commandNode(spec, 0))

	if !rec.waitForMessage("out-1", 2*time.Second) {
		t.Fatal("stdout line out-1 never reached the log channel")
	}
	if !rec.waitForMessage("err-1", 2*time.Second) {
		t.Fatal("stderr line err-1 never reached the log channel")
	}
	var stdoutType, stderrType string
	for _, m := range rec.snapshotMessages() {
		switch m["content"] {
		case "out-1", "out-2":
			stdoutType, _ = m["type"].(string)
		case "err-1":
			stderrType, _ = m["type"].(string)
		}
	}
	if stdoutType != "stdout" || stderrType != "stderr" {
		t.Fatalf("stream types = %q/%q, want stdout/stderr", stdoutType, stderrType)
	}

	_, completes, _ := rec.snapshot()
	output := receiptMap(t, completes[0]["output"])
	if stdout, _ := output["stdout_tail"].(string); stdout != "out-1\nout-2\n" {
		t.Errorf("stdout_tail = %q, want %q", stdout, "out-1\nout-2\n")
	}
	if stderr, _ := output["stderr_tail"].(string); stderr != "err-1\n" {
		t.Errorf("stderr_tail = %q, want %q", stderr, "err-1\n")
	}
}

// TestCommandTicketRejectsUnsafeSpecs proves absolute program paths, program
// paths escaping the workspace, and escaping workdirs are rejected before any
// process starts.
func TestCommandTicketRejectsUnsafeSpecs(t *testing.T) {
	cases := []struct {
		name    string
		spec    *agent.CommandSpec
		wantMsg string
	}{
		{"absolute program", &agent.CommandSpec{Program: "/abs/evil"}, "absolute path"},
		{"escaping program", &agent.CommandSpec{Program: "../outside/helper"}, "outside the task workspace"},
		{"absolute workdir", &agent.CommandSpec{Program: "go", WorkDir: "/tmp"}, "absolute path"},
		{"escaping workdir", &agent.CommandSpec{Program: "go", WorkDir: "../outside"}, "outside the task workspace"},
		{"empty program", &agent.CommandSpec{}, "no program"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor, _, rec, _, run := commandTestEnv(t)
			executor.Execute(run, commandNode(tc.spec, 0))

			fails, completes, _ := rec.snapshot()
			if len(completes) != 0 {
				t.Fatalf("rejected spec must not complete, got %d completes", len(completes))
			}
			if len(fails) != 1 {
				t.Fatalf("expected exactly one /fail receipt, got %d", len(fails))
			}
			if fails[0]["error_code"] != "command_start_failed" {
				t.Errorf("error_code = %v, want command_start_failed", fails[0]["error_code"])
			}
			if msg, _ := fails[0]["error_message"].(string); !contains(msg, tc.wantMsg) {
				t.Errorf("error_message = %q, want it to mention %q", msg, tc.wantMsg)
			}
		})
	}
}

// TestCommandTicketProgramNotFound proves a program name that resolves to
// nothing reports a start failure.
func TestCommandTicketProgramNotFound(t *testing.T) {
	executor, _, rec, _, run := commandTestEnv(t)

	spec := &agent.CommandSpec{Program: "definitely-not-a-real-command-xyz"}
	executor.Execute(run, commandNode(spec, 0))

	fails, completes, _ := rec.snapshot()
	if len(completes) != 0 || len(fails) != 1 {
		t.Fatalf("expected one /fail and no /complete, got fails=%d completes=%d", len(fails), len(completes))
	}
	if fails[0]["error_code"] != "command_start_failed" {
		t.Errorf("error_code = %v, want command_start_failed", fails[0]["error_code"])
	}
}

// TestCommandTicketExitNonzero proves a non-zero exit reports
// command_exit_nonzero with the code in the message and no completion.
func TestCommandTicketExitNonzero(t *testing.T) {
	executor, _, rec, workDir, run := commandTestEnv(t)
	seedHelper(t, workDir, "helper.exe")

	spec := &agent.CommandSpec{Program: "./helper.exe", Args: []string{"fail"}}
	executor.Execute(run, commandNode(spec, 0))

	fails, completes, _ := rec.snapshot()
	if len(completes) != 0 {
		t.Fatalf("non-zero exit must not complete, got %d completes", len(completes))
	}
	if len(fails) != 1 {
		t.Fatalf("expected exactly one /fail receipt, got %d", len(fails))
	}
	if fails[0]["error_code"] != "command_exit_nonzero" {
		t.Errorf("error_code = %v, want command_exit_nonzero", fails[0]["error_code"])
	}
	if msg, _ := fails[0]["error_message"].(string); !contains(msg, "exited with code 3") {
		t.Errorf("error_message = %q, want it to carry the exit code", msg)
	}
}

// TestInterruptKillsCommandProcessTree proves interrupting a running command
// kills the entire process tree: the helper spawns its own child that keeps
// appending to a heartbeat file, so a surviving grandchild would keep the file
// growing after the interrupt. The execution must also return without
// reporting any receipt.
func TestInterruptKillsCommandProcessTree(t *testing.T) {
	executor, _, rec, workDir, run := commandTestEnv(t)
	seedHelper(t, workDir, "helper.exe")

	marker := filepath.Join(t.TempDir(), "pulse")
	spec := &agent.CommandSpec{Program: "./helper.exe", Args: []string{"spawn", marker}}
	done := make(chan struct{})
	go func() {
		executor.Execute(run, commandNode(spec, 0))
		close(done)
	}()
	executor.WaitRunningForTest(t)
	if !rec.waitForMessage("spawned", 10*time.Second) {
		t.Fatal("helper never announced the spawned child process")
	}
	time.Sleep(time.Second) // let the grandchild accumulate heartbeats

	if err := executor.Interrupt(12, "n-recv"); err != nil {
		t.Fatalf("Interrupt returned error: %v", err)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Execute did not return after interrupt — the process tree was not terminated")
	}

	fileSize := func() int64 {
		info, err := os.Stat(marker)
		if err != nil {
			t.Fatalf("heartbeat file missing: %v", err)
		}
		return info.Size()
	}
	time.Sleep(2 * time.Second)
	sizeAfterKill := fileSize()
	time.Sleep(1 * time.Second)
	if fileSize() != sizeAfterKill {
		t.Fatal("grandchild process is still alive after interrupt — only the direct child was killed")
	}
	fails, completes, _ := rec.snapshot()
	if len(fails) != 0 || len(completes) != 0 {
		t.Fatalf("interrupted command reported receipts: fails=%d completes=%d", len(fails), len(completes))
	}
}

// TestCommandTicketTimeoutReportsTimeout proves the ticket's timeout deadline
// terminates the command and reports command_timeout. The protocol's timeout
// unit is whole minutes, so this test waits out a one-minute floor.
func TestCommandTicketTimeoutReportsTimeout(t *testing.T) {
	executor, _, rec, workDir, run := commandTestEnv(t)
	seedHelper(t, workDir, "helper.exe")

	spec := &agent.CommandSpec{Program: "./helper.exe", Args: []string{"hang", "600"}}
	executor.Execute(run, commandNode(spec, 1))

	fails, completes, _ := rec.snapshot()
	if len(completes) != 0 {
		t.Fatalf("timed-out command must not complete, got %d completes", len(completes))
	}
	if len(fails) != 1 {
		t.Fatalf("expected exactly one /fail receipt, got %d", len(fails))
	}
	if fails[0]["error_code"] != "command_timeout" {
		t.Errorf("error_code = %v, want command_timeout", fails[0]["error_code"])
	}
}

// TestCommandTicketEnvOverrides proves ticket env entries override the
// process environment's same-named variables.
func TestCommandTicketEnvOverrides(t *testing.T) {
	executor, _, rec, workDir, run := commandTestEnv(t)
	seedHelper(t, workDir, "helper.exe")
	t.Setenv("TEAMMATE_TEST_INJECT", "from-process-env")

	spec := &agent.CommandSpec{
		Program: "./helper.exe",
		Args:    []string{"env", "TEAMMATE_TEST_INJECT"},
		Env:     map[string]string{"TEAMMATE_TEST_INJECT": "from-ticket"},
	}
	executor.Execute(run, commandNode(spec, 0))

	fails, completes, _ := rec.snapshot()
	if len(fails) != 0 || len(completes) != 1 {
		t.Fatalf("expected one complete and no fails, got fails=%d completes=%d", len(fails), len(completes))
	}
	stdout, _ := receiptMap(t, completes[0]["output"])["stdout_tail"].(string)
	if stdout != "from-ticket\n" {
		t.Fatalf("stdout_tail = %q, want the ticket env value to override the process env", stdout)
	}
}

// TestCommandTicketTailCapsAndKeepsUTF8 proves the receipt tail stays within
// the 64 KiB cap and never carries a broken UTF-8 sequence even when the cap
// cuts inside a multi-byte rune.
func TestCommandTicketTailCapsAndKeepsUTF8(t *testing.T) {
	executor, _, rec, workDir, run := commandTestEnv(t)
	seedHelper(t, workDir, "helper.exe")

	spec := &agent.CommandSpec{Program: "./helper.exe", Args: []string{"bigutf8"}}
	executor.Execute(run, commandNode(spec, 0))

	fails, completes, _ := rec.snapshot()
	if len(fails) != 0 || len(completes) != 1 {
		t.Fatalf("expected one complete and no fails, got fails=%d completes=%d", len(fails), len(completes))
	}
	stdout, _ := receiptMap(t, completes[0]["output"])["stdout_tail"].(string)
	if len(stdout) > 64*1024 {
		t.Fatalf("stdout_tail = %d bytes, exceeds the 64 KiB cap", len(stdout))
	}
	if !utf8.ValidString(stdout) {
		t.Fatal("stdout_tail carries a broken UTF-8 sequence")
	}
	if !contains(stdout, "中") {
		t.Fatal("stdout_tail lost the CJK content entirely")
	}
}

// TestClaimNodeParsesCommandTicket proves the claim response deserializes a
// command execution ticket (kind, spec, execution ID) end to end over JSON.
func TestClaimNodeParsesCommandTicket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "n-cmd",
			"task_id": 7,
			"name": "run tests",
			"status": "running",
			"execution_id": "exec-cmd-1",
			"execution_kind": "command",
			"command": {
				"program": "./scripts/test.sh",
				"args": ["-run", "TestX"],
				"workdir": "sub/dir",
				"env": {"GOFLAGS": "-count=1"}
			},
			"timeout_minutes": 15,
			"readonly_dirs": ["docs"],
			"full_control_dirs": ["src"]
		}`))
	}))
	defer server.Close()

	claimed, err := agent.NewClient(server.URL, "td_test").ClaimNode(context.Background(), "agent-1", 7, "n-cmd")
	if err != nil {
		t.Fatalf("ClaimNode: %v", err)
	}
	if claimed.ExecutionID != "exec-cmd-1" || claimed.ExecutionKind != agent.ExecutionKindCommand {
		t.Fatalf("execution identity = %q/%q, want exec-cmd-1/command", claimed.ExecutionID, claimed.ExecutionKind)
	}
	if claimed.Command == nil {
		t.Fatal("command spec not deserialized")
	}
	cmd := claimed.Command
	if cmd.Program != "./scripts/test.sh" {
		t.Errorf("program = %q", cmd.Program)
	}
	if len(cmd.Args) != 2 || cmd.Args[0] != "-run" || cmd.Args[1] != "TestX" {
		t.Errorf("args = %#v", cmd.Args)
	}
	if cmd.WorkDir != "sub/dir" {
		t.Errorf("workdir = %q", cmd.WorkDir)
	}
	if cmd.Env["GOFLAGS"] != "-count=1" {
		t.Errorf("env = %#v", cmd.Env)
	}
	if claimed.TimeoutMinutes != 15 {
		t.Errorf("timeout_minutes = %d, want 15", claimed.TimeoutMinutes)
	}
}

// TestRecoveryReportsInterruptedCommand proves a command ticket found
// in-progress after a daemon restart is reported as interrupted through
// /fail — never blindly re-executed.
func TestRecoveryReportsInterruptedCommand(t *testing.T) {
	t.Setenv("TEAMMATE_DISK_QUOTA_GB", "100")
	var failCalls []map[string]interface{}
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workspaces/ws-1/agents/agent-1/in-progress-nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"id": "n-cmd",
			"task_id": 7,
			"name": "deploy",
			"status": "running",
			"execution_id": "exec-cmd-1",
			"execution_kind": "command",
			"command": {"program": "./deploy.sh"},
			"project_id": "proj-1"
		}]`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/fail") {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			failCalls = append(failCalls, body)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := agent.NewClient(server.URL, "td_test")
	executor := agent.NewTaskExecutor(&agent.Config{
		Workspace: agent.WorkspaceConfig{Root: t.TempDir()},
		Git:       agent.GitConfig{BaseBranch: "master"},
	})
	watcher := agent.NewNodeWatcher(client, executor, "conn", "agent-1", "ws-1", time.Hour)
	watcher.PollForTest()

	if executor.IsRunning() {
		t.Fatal("command ticket must not be re-executed from the recovery path")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(failCalls) != 1 {
		t.Fatalf("expected exactly one /fail receipt, got %d", len(failCalls))
	}
	if failCalls[0]["execution_id"] != "exec-cmd-1" {
		t.Errorf("execution_id = %v, want exec-cmd-1", failCalls[0]["execution_id"])
	}
	if failCalls[0]["error_code"] != "command_interrupted" {
		t.Errorf("error_code = %v, want command_interrupted", failCalls[0]["error_code"])
	}
}

// TestSoftInterruptRejectsCommandTicket proves a local takeover is refused for
// command executions — there is no tool session to hand to a human, and the
// handback path must never re-run a command.
func TestSoftInterruptRejectsCommandTicket(t *testing.T) {
	executor := agent.NewTaskExecutorWithObserver(nil, &recordingExecutionObserver{})
	executor.SeedRunningForTest(nil, 7, agent.TaskNode{
		ID:            "n-cmd",
		Name:          "deploy",
		ExecutionID:   "exec-cmd-1",
		ExecutionKind: agent.ExecutionKindCommand,
	}, t.TempDir())

	err := executor.SoftInterrupt(7, "n-cmd")
	if err == nil {
		t.Fatal("SoftInterrupt must reject command tickets")
	}
	if !errors.Is(err, agent.ErrInterventionNotHeld) {
		t.Fatalf("error = %v, want it to wrap ErrInterventionNotHeld", err)
	}
}
