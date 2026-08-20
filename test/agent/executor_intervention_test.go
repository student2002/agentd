package agent_test

import (
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
)

// TestExecuteInterventionTurnRunsOneTurnWithMessage proves the intervention turn
// runs the fake tool once with the human's message and returns the tool's
// output, without touching the server (no client wired).
func TestExecuteInterventionTurnRunsOneTurnWithMessage(t *testing.T) {
	t.Setenv("TEAMMATE_DISK_QUOTA_GB", "0")

	ft := newFakeTool("claude")
	ft.SetInterventionMode(true)
	ft.SetOutput("did the thing")
	executor := agent.NewTaskExecutorWithObserver(nil, nil, "agent-1", &recordingExecutionObserver{})
	executor.SetToolFactoryForTest(func() agent.TestTool { return &fakeToolAdapter{tool: ft} })
	executor.SetGitManagerForTest(nil)

	// Seed the executor as if Execute() had already started a node and captured
	// its workDir + projectID, then soft-interrupted to hand control to a human.
	executor.SeedRunningForTest(7, agent.TaskNode{ID: "n-int", Name: "node-2", SortOrder: 2}, "proj-7", t.TempDir())

	out, err := executor.ExecuteInterventionTurn(7, "n-int", "please add a test")
	if err != nil {
		t.Fatalf("ExecuteInterventionTurn: %v", err)
	}
	if out != "did the thing" {
		t.Fatalf("expected tool output, got %q", out)
	}
	if ft.callCount() != 1 {
		t.Fatalf("expected exactly one tool Execute call, got %d", ft.callCount())
	}
	if ft.lastPromptValue() != "please add a test" {
		t.Fatalf("expected human message as prompt, got %q", ft.lastPromptValue())
	}
}

// TestCompleteManuallyReturnsFalseWhenNotRunning proves manual complete is
// rejected when no node matches — the human cannot complete a node the
// executor is not tracking.
func TestCompleteManuallyReturnsFalseWhenNotRunning(t *testing.T) {
	executor := agent.NewTaskExecutorWithObserver(nil, nil, "agent-1", nil)
	if ok := executor.CompleteManually(7, "n-none"); ok {
		t.Fatal("expected CompleteManually to return false when not running the node")
	}
}

var _ = time.Second
