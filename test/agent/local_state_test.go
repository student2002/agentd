// local_state_test.go covers tests for local state management.
package agent_test

import (
	"errors"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
)

func TestLocalStateStoreSnapshotTracksRuntimeAndExecution(t *testing.T) {
	store := agent.NewLocalStateStore(agent.LocalStateConfig{
		InstanceID: "instance-1",
		AgentName:  "ac",
		Provider:   "claude",
		ServerURL:  "http://127.0.0.1:8080",
	})

	store.SetRuntimeRegistered("rt-1", "daemon-1")
	store.SetSSEConnected("event-1")
	store.SetExecutionStarted(agent.LocalExecutionSession{
		TaskID:   12,
		NodeID:   "node-1",
		NodeName: "code",
		Tool:     "claude",
		WorkDir:  "D:\\work",
	})

	snapshot := store.Snapshot()
	if snapshot.Runtime.Status != agent.LocalRuntimeOnline {
		t.Fatalf("expected runtime online, got %s", snapshot.Runtime.Status)
	}
	if !snapshot.Runtime.SSEConnected {
		t.Fatal("expected SSE connected")
	}
	if snapshot.Agent.Status != agent.LocalAgentBusy {
		t.Fatalf("expected agent busy, got %s", snapshot.Agent.Status)
	}
	if snapshot.ExecutionSession.Status != agent.LocalExecutionRunning {
		t.Fatalf("expected session running, got %s", snapshot.ExecutionSession.Status)
	}
}

func TestHeartbeatCallbacksUpdateLocalState(t *testing.T) {
	store := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	store.SetRuntimeRegistered("rt-1", "daemon-1")
	store.SetHeartbeatSuccess(time.Date(2026, 6, 23, 8, 0, 0, 0, time.UTC))
	if store.Snapshot().Runtime.LastHeartbeatAt.IsZero() {
		t.Fatal("expected heartbeat timestamp")
	}

	store.SetHeartbeatError(errors.New("network down"))
	if store.Snapshot().Runtime.LastHeartbeatError == "" {
		t.Fatal("expected heartbeat error")
	}
}

func TestLocalStateStoreTracksExecutionCompletionAndFailure(t *testing.T) {
	store := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1", Provider: "claude"})
	store.SetExecutionStarted(agent.LocalExecutionSession{
		TaskID: 12,
		NodeID: "node-1",
		Tool:   "claude",
	})

	store.SetExecutionCompleted(12, "node-1")
	completed := store.Snapshot()
	if completed.Agent.Status != agent.LocalAgentOnline {
		t.Fatalf("expected agent online after completion, got %s", completed.Agent.Status)
	}
	if completed.ExecutionSession.Status != agent.LocalExecutionCompleted {
		t.Fatalf("expected execution completed, got %s", completed.ExecutionSession.Status)
	}

	store.SetExecutionStarted(agent.LocalExecutionSession{TaskID: 13, NodeID: "node-2", Tool: "claude"})
	store.SetExecutionFailed(13, "node-2", errors.New("tool failed"))
	failed := store.Snapshot()
	if failed.Agent.Status != agent.LocalAgentOnline {
		t.Fatalf("expected agent online after failure, got %s", failed.Agent.Status)
	}
	if failed.ExecutionSession.Status != agent.LocalExecutionFailed {
		t.Fatalf("expected execution failed, got %s", failed.ExecutionSession.Status)
	}
	if failed.LastError.Code != "tool_execution_failed" {
		t.Fatalf("expected tool execution error, got %q", failed.LastError.Code)
	}
}

func TestLocalStateStoreSnapshotDoesNotExposeInternalErrorPointer(t *testing.T) {
	store := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	store.SetRuntimeError("runtime_failed", "boom")

	snapshot := store.Snapshot()
	if snapshot.LastError.At == nil {
		t.Fatal("expected error timestamp")
	}
	changed := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	*snapshot.LastError.At = changed

	got := store.Snapshot()
	if got.LastError.At == nil {
		t.Fatal("expected stored error timestamp")
	}
	if got.LastError.At.Equal(changed) {
		t.Fatal("snapshot mutation leaked back into store")
	}
}

func TestLocalEventHubPublishesSnapshotEvents(t *testing.T) {
	hub := agent.NewLocalEventHub()
	ch, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	hub.Publish(agent.LocalEvent{
		Type:     agent.LocalEventExecutionStarted,
		Snapshot: agent.LocalSnapshot{InstanceID: "instance-1"},
	})

	select {
	case got := <-ch:
		if got.Type != agent.LocalEventExecutionStarted {
			t.Fatalf("unexpected event type: %s", got.Type)
		}
		if got.EventID == "" {
			t.Fatal("expected event id")
		}
		if got.Timestamp.IsZero() {
			t.Fatal("expected timestamp")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestAgentRuntimeInitialLocalSnapshot(t *testing.T) {
	view := &agent.Config{}
	view.Agent.Name = "ac"
	view.Agent.Provider = "claude"
	view.Server.URL = "http://127.0.0.1:8080"
	view.Local.Enabled = true
	view.Local.BindAddr = "127.0.0.1:0"
	view.Local.LocalToken = "lt_test"
	view.Local.InstanceID = "instance-1"

	runtime := agent.NewAgentRuntime(view, "team-a", "ac")
	snapshot := runtime.Snapshot()
	if snapshot.Config.AgentName != "ac" {
		t.Fatalf("unexpected agent name: %s", snapshot.Config.AgentName)
	}
	if snapshot.Runtime.Status != agent.LocalRuntimeOffline {
		t.Fatalf("expected initial runtime offline, got %s", snapshot.Runtime.Status)
	}
	runtime.State().SetAgentIdentity("ws-1", "agent-uuid-1")
	snapshot = runtime.Snapshot()
	if snapshot.Config.WorkspaceID != "ws-1" || snapshot.Config.AgentID != "agent-uuid-1" {
		t.Fatalf("expected identity recorded, got %+v", snapshot.Config)
	}
	// Rebinding replaces the single identity instead of accumulating.
	runtime.State().SetAgentIdentity("ws-1", "agent-uuid-2")
	snapshot = runtime.Snapshot()
	if snapshot.Config.AgentID != "agent-uuid-2" {
		t.Fatalf("expected identity replaced, got %+v", snapshot.Config)
	}
	runtime.State().ClearAgentIdentity()
	snapshot = runtime.Snapshot()
	if snapshot.Config.WorkspaceID != "" || snapshot.Config.AgentID != "" {
		t.Fatalf("expected identity cleared, got %+v", snapshot.Config)
	}
}

func TestAgentRuntimeIdentitiesPendingUntilResolved(t *testing.T) {
	view := &agent.Config{}
	view.Agent.Name = "ac"
	view.Agent.Provider = "claude"

	runtime := agent.NewAgentRuntime(view, "team-a", "ac")
	if got := runtime.Snapshot().Config.AgentID; got != "" {
		t.Fatalf("expected no identity while pending, got %q", got)
	}
	if got := len(runtime.Watchers()); got != 0 {
		t.Fatalf("expected no watcher while pending, got %d", got)
	}
}
