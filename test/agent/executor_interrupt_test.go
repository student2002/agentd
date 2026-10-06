package agent_test

import (
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestInterruptDoesNotReportManualIntervention reproduces the bug where
// Interrupt's cancel causes Execute's err branch to call reportFailure
// (client.ManualIntervention) on top of Interrupt's own ReportInterrupt.
// After the fix, an interrupted execution must NOT be reported as a failure.
func TestInterruptDoesNotReportManualIntervention(t *testing.T) {
	t.Setenv("TEAMMATE_DISK_QUOTA_GB", "100")
	cfg := &agent.Config{
		Server:    agent.ServerConfig{URL: "http://127.0.0.1:1"},
		Agent:     agent.AgentInfo{Name: "Agent One", Provider: "claude"},
		Workspace: agent.WorkspaceConfig{Root: t.TempDir()},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}

	client := agent.NewClient(cfg.Server.URL, "td_fake_token")
	observer := &recordingExecutionObserver{}
	executor := agent.NewTaskExecutorWithObserver(cfg, observer)
	executor.SetToolFactoryForTest(func() agent.TestTool {
		return &fakeToolAdapter{tool: newFakeTool("claude")}
	})

	node := agent.TaskNode{ID: "node-1", Name: "code", SortOrder: 1, NodeType: "standard"}

	go executor.Execute(agent.RunContext{
		Client:      client,
		AgentID:     "agent-1",
		WorkspaceID: "ws-1",
		ProjectID:   "project-1",
		TaskID:      12,
		NodeID:      "node-1",
	}, node)
	executor.WaitRunningForTest(t)

	if err := executor.Interrupt(12, "node-1"); err != nil {
		t.Fatalf("Interrupt returned error: %v", err)
	}

	if observer.failure != nil {
		t.Fatalf("interrupted execution was reported as failure (manual_intervention misfire): %v", observer.failure)
	}
	if observer.failedTaskID != 0 || observer.failedNodeID != "" {
		t.Fatalf("interrupted execution reported OnExecutionFailed: task=%d node=%s", observer.failedTaskID, observer.failedNodeID)
	}
}
