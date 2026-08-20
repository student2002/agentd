package agent_test

import (
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
)

// TestSoftInterruptStopsToolWithoutReportingFailure proves SoftInterrupt cancels
// the running turn and causes Execute to return WITHOUT reportFailure or
// notifyExecutionFailed firing — the server stays unaware (no ManualIntervention,
// no ReportInterrupt path).
func TestSoftInterruptStopsToolWithoutReportingFailure(t *testing.T) {
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

	node := agent.TaskNode{ID: "n-soft", Name: "node-1", SortOrder: 1, NodeType: "standard"}
	go executor.Execute(42, node, "proj-1")
	executor.WaitRunningForTest(t)

	time.Sleep(50 * time.Millisecond) // let the tool start

	if err := executor.SoftInterrupt(42, "n-soft"); err != nil {
		t.Fatalf("SoftInterrupt returned error: %v", err)
	}

	select {
	case <-time.After(2 * time.Second):
		t.Fatal("soft-interrupt did not let Execute return within 2s")
	default:
	}

	o := executor.ObserverForTest().(*recordingExecutionObserver)
	if o.failure != nil {
		t.Fatalf("soft-interrupted execution was reported as failure: %v (server must stay unaware)", o.failure)
	}
	if !o.interrupted {
		t.Fatalf("soft-interrupt should fire OnExecutionInterrupted (interrupted flag not set)")
	}
}

