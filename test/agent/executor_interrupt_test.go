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
		Server:    agent.ServerConfig{URL: "http://127.0.0.1:1", APIToken: "fake-token"},
		Agent:     agent.AgentInfo{ID: "agent-1", Name: "Agent One", Provider: "claude"},
		Workspace: agent.WorkspaceConfig{ID: "ws-1", Root: t.TempDir()},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}

	client := agent.NewClient(cfg.Server.URL, cfg.Server.APIToken)
	observer := &recordingExecutionObserver{}
	executor := agent.NewTaskExecutorWithObserver(cfg, client, "agent-1", observer)
	executor.SetToolFactoryForTest(func() agent.TestTool {
		return &fakeToolAdapter{tool: newFakeTool("claude")}
	})

	node := agent.TaskNode{ID: "node-1", Name: "code", SortOrder: 1, NodeType: "standard"}

	go executor.Execute(12, node, "project-1")
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
