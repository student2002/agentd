// Package agent_test 包含 agent 包的测试，涵盖 shell 转义、执行上下文构建、Git 操作和代理守护进程使用的 Token 估算。
package agent_test

import (
	"errors"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestParseNodeOrder 验证 ParseNodeOrder 能从节点名称中正确提取数字前缀（例如 "3. 代码实现" → 3），并在无数字前缀时返回 0。
func TestParseNodeOrder(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"numbered", "1. 需求分析", 1},
		{"numbered2", "3. 代码实现", 3},
		{"no number", "review", 0},
		{"empty", "", 0},
		{"double digit", "12. 测试", 12},
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

// TestTaskExecutor_IsRunning 验证新创建的 TaskExecutor 在开始任务执行前 IsRunning() 返回 false。
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

// TestTaskExecutor_CurrentTask_NotRunning 验证在未运行的 executor 上调用 CurrentTask 返回 ok=false，以及零值任务 ID 和 nil 节点。
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
	_ = node // 仅确保它能编译通过
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
