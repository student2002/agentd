package agent_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
)

// TestExecuteRebindsSessionContextOnTaskChange proves the executor binds the
// session context (lastWorkDir) to the incoming task at Execute entry: an
// intervention turn fired right after a task switch runs in the NEW task's
// workdir, never the previous task's directory.
func TestExecuteRebindsSessionContextOnTaskChange(t *testing.T) {
	t.Setenv("TEAMMATE_DISK_QUOTA_GB", "100")
	root := t.TempDir()
	cfg := &agent.Config{
		Server:    agent.ServerConfig{URL: "http://127.0.0.1:1"},
		Agent:     agent.AgentInfo{Name: "Agent One", Provider: "claude"},
		Workspace: agent.WorkspaceConfig{Root: root},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}
	client := agent.NewClient(cfg.Server.URL, "td_fake_token")
	executor := agent.NewTaskExecutorWithObserver(cfg, &recordingExecutionObserver{})
	fake := newFakeTool("claude")
	executor.SetToolFactoryForTest(func() agent.TestTool {
		return &fakeToolAdapter{tool: fake}
	})

	// Prime stale state as if a previous task (48) had just finished with its
	// workdir installed on the executor.
	node48 := agent.TaskNode{ID: "n-48", Name: "old-node", SortOrder: 1, NodeType: "standard"}
	executor.SeedRunningForTest(&agent.RunContext{
		Client: client, AgentID: "agent-1", WorkspaceID: "ws-1", ProjectID: "proj-1", TaskID: 48, NodeID: "n-48",
	}, 48, node48, filepath.Join(root, "ws-1", "agent-1", "proj-1", "48"))

	// Start task 58 and take it over before its tool captures any session.
	node58 := agent.TaskNode{ID: "n-58", Name: "new-node", SortOrder: 1, NodeType: "standard"}
	go executor.Execute(agent.RunContext{
		Client: client, AgentID: "agent-1", WorkspaceID: "ws-1", ProjectID: "proj-1", TaskID: 58, NodeID: "n-58",
	}, node58)
	fake.waitStarted()

	if err := executor.SoftInterrupt(58, "n-58"); err != nil {
		t.Fatalf("SoftInterrupt: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for executor.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if executor.IsRunning() {
		t.Fatal("Execute did not return after soft-interrupt")
	}

	fake.SetInterventionMode(true)
	if _, err := executor.ExecuteInterventionTurn(58, "n-58", "human message"); err != nil {
		t.Fatalf("intervene after task switch rejected: %v", err)
	}
	want := filepath.Join(root, "ws-1", "agent-1", "proj-1", "58")
	if got := fake.lastWorkDirValue(); got != want {
		t.Fatalf("intervention workdir = %q, want %q (previous task's context must not leak)", got, want)
	}
}
