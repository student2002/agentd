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

	node := agent.TaskNode{ID: "n-soft", Name: "node-1", SortOrder: 1, NodeType: "standard"}
	go executor.Execute(agent.RunContext{
		Client:      client,
		AgentID:     "agent-1",
		WorkspaceID: "ws-1",
		ProjectID:   "proj-1",
		TaskID:      42,
		NodeID:      "n-soft",
	}, node)
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
	if o.interrupted {
		t.Fatal("soft-interrupt fired OnExecutionInterrupted (that path is reserved for server interrupts)")
	}
	if o.started.Status != agent.LocalExecutionIntervening {
		t.Fatalf("soft-interrupt should move the session to intervening, got %q", o.started.Status)
	}
}

// TestInterventionAfterSoftInterrupt proves the local takeover flow stays
// usable after SoftInterrupt: the execution goroutine has returned (not
// running), yet intervene and handback must still accept the node, and
// handback must fire the resume callback so the watcher recovers the node.
func TestInterventionAfterSoftInterrupt(t *testing.T) {
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
	fake := newFakeTool("claude")
	executor.SetToolFactoryForTest(func() agent.TestTool {
		return &fakeToolAdapter{tool: fake}
	})
	resumed := make(chan struct{}, 1)
	executor.SetResumeCallback(func() { resumed <- struct{}{} })

	node := agent.TaskNode{ID: "n-takeover", Name: "node-1", SortOrder: 1, NodeType: "standard"}
	go executor.Execute(agent.RunContext{
		Client:      client,
		AgentID:     "agent-1",
		WorkspaceID: "ws-1",
		ProjectID:   "proj-1",
		TaskID:      42,
		NodeID:      "n-takeover",
	}, node)
	executor.WaitRunningForTest(t)
	time.Sleep(50 * time.Millisecond) // let the tool start

	if err := executor.SoftInterrupt(42, "n-takeover"); err != nil {
		t.Fatalf("SoftInterrupt returned error: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for executor.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if executor.IsRunning() {
		t.Fatal("Execute did not return after soft-interrupt")
	}

	// Takeover held: intervene and handback must both accept the node.
	fake.SetInterventionMode(true)
	if _, err := executor.ExecuteInterventionTurn(42, "n-takeover", "human message"); err != nil {
		t.Fatalf("intervene after soft-interrupt rejected: %v", err)
	}
	if !executor.Handback(42, "n-takeover") {
		t.Fatal("handback after soft-interrupt rejected")
	}
	select {
	case <-resumed:
	case <-time.After(2 * time.Second):
		t.Fatal("handback did not fire the resume callback")
	}
}

