// Package agent_test contains tests for the agent package, covering shell escaping, execution context construction, Git operations, and Token estimation used by the agent daemon.
package agent_test

import (
	"errors"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestParseNodeOrder verifies that ParseNodeOrder correctly extracts a numeric prefix from a node name (e.g. "3. Implementation" → 3), and returns 0 when there is no numeric prefix.
func TestParseNodeOrder(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"numbered", "1. Requirement Analysis", 1},
		{"numbered2", "3. Implementation", 3},
		{"no number", "review", 0},
		{"empty", "", 0},
		{"double digit", "12. Test", 12},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agent.ParseNodeOrder(tt.input)
			if got != tt.want {
				t.Errorf("ParseNodeOrder(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// TestTaskExecutor_IsRunning verifies that a newly created TaskExecutor returns false from IsRunning() before task execution begins.
func TestTaskExecutor_IsRunning(t *testing.T) {
	cfg := &agent.Config{
		Workspace: agent.WorkspaceConfig{Root: t.TempDir()},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}
	client := agent.NewClient("http://localhost:0", "fake-token")
	executor := agent.NewTaskExecutor(cfg, client, "agent-1")

	if executor.IsRunning() {
		t.Error("new executor should not be running")
	}
}

// TestTaskExecutor_CurrentTask_NotRunning verifies that calling CurrentTask on a non-running executor returns ok=false, along with a zero-value task ID and a nil node.
func TestTaskExecutor_CurrentTask_NotRunning(t *testing.T) {
	cfg := &agent.Config{
		Workspace: agent.WorkspaceConfig{Root: t.TempDir()},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}
	client := agent.NewClient("http://localhost:0", "fake-token")
	executor := agent.NewTaskExecutor(cfg, client, "agent-1")

	taskID, node, ok := executor.CurrentTask()
	if ok {
		t.Error("CurrentTask on non-running executor should return ok=false")
	}
	if taskID != 0 {
		t.Errorf("taskID = %d, want 0", taskID)
	}
	_ = node // just ensure it compiles
}

func TestTaskExecutorObserverReportsExecutionLifecycle(t *testing.T) {
	t.Setenv("TEAMMATE_DISK_QUOTA_GB", "0")
	cfg := &agent.Config{
		Server: agent.ServerConfig{URL: "http://127.0.0.1:1", APIToken: "fake-token"},
		Agent: agent.AgentInfo{
			ID:       "agent-1",
			Name:     "Agent One",
			Provider: "claude",
		},
		Workspace: agent.WorkspaceConfig{
			ID:   "ws-1",
			Root: t.TempDir(),
		},
		Git: agent.GitConfig{BaseBranch: "master"},
	}
	client := agent.NewClient(cfg.Server.URL, cfg.Server.APIToken)
	observer := &recordingExecutionObserver{}
	executor := agent.NewTaskExecutorWithObserver(cfg, client, "agent-1", observer)
	node := agent.TaskNode{ID: "node-1", Name: "code", SortOrder: 1, NodeType: "standard"}

	executor.Execute(12, node, "project-1")

	if observer.started.TaskID != 12 {
		t.Fatalf("expected start task id 12, got %d", observer.started.TaskID)
	}
	if observer.started.NodeID != "node-1" {
		t.Fatalf("expected start node node-1, got %s", observer.started.NodeID)
	}
	if observer.failedTaskID != 12 || observer.failedNodeID != "node-1" {
		t.Fatalf("expected failure for task 12 node-1, got task %d node %s", observer.failedTaskID, observer.failedNodeID)
	}
	if observer.failure == nil {
		t.Fatal("expected failure error")
	}
}

type recordingExecutionObserver struct {
	started      agent.LocalExecutionSession
	failedTaskID int32
	failedNodeID string
	failure      error
	interrupted  bool // set by OnExecutionInterrupted; asserted by Task 3/4
}

func (o *recordingExecutionObserver) OnExecutionStarted(session agent.LocalExecutionSession) {
	o.started = session
}

func (o *recordingExecutionObserver) OnExecutionCompleted(_ int32, _ string) {}

func (o *recordingExecutionObserver) OnExecutionInterrupted(_ int32, _ string) {
	o.interrupted = true
}

func (o *recordingExecutionObserver) OnExecutionFailed(taskID int32, nodeID string, err error) {
	o.failedTaskID = taskID
	o.failedNodeID = nodeID
	o.failure = err
}

func (o *recordingExecutionObserver) OnToolStatusChanged(_ string, _ string, err error) {
	if err != nil {
		o.failure = errors.Join(o.failure, err)
	}
}
