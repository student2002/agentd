// local_state_test.go 覆盖本地状态管理的测试。
package agent_test

import (
	"errors"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
)

func TestLocalStateStoreSnapshotTracksRuntimeAndExecution(t *testing.T) {
	store := agent.NewLocalStateStore(agent.LocalStateConfig{
		InstanceID:  "instance-1",
		AgentID:     "agent-1",
		AgentName:   "ac",
		WorkspaceID: "ws-1",
		Provider:    "claude",
		ServerURL:   "http://localhost:8080",
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

func TestDaemonInitialLocalSnapshot(t *testing.T) {
	cfg := &agent.Config{}
	cfg.Agent.ID = "agent-1"
	cfg.Agent.Name = "ac"
	cfg.Agent.Provider = "claude"
	cfg.Workspace.ID = "ws-1"
	cfg.Server.URL = "http://localhost:8080"
	cfg.Local.Enabled = true
	cfg.Local.BindAddr = "127.0.0.1:0"
	cfg.Local.LocalToken = "lt_test"
	cfg.Local.InstanceID = "instance-1"

	daemon, err := agent.NewDaemon(cfg)
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	snapshot := daemon.LocalSnapshot()
	if snapshot.InstanceID != "instance-1" {
		t.Fatalf("unexpected instance id: %s", snapshot.InstanceID)
	}
	if snapshot.Runtime.Status != agent.LocalRuntimeOffline {
		t.Fatalf("expected initial runtime offline, got %s", snapshot.Runtime.Status)
	}
}

func TestDaemonStopIsIdempotent(t *testing.T) {
	cfg := &agent.Config{}
	cfg.Agent.ID = "agent-1"
	cfg.Agent.Name = "ac"
	cfg.Agent.Provider = "claude"
	cfg.Workspace.ID = "ws-1"
	cfg.Server.URL = "http://localhost:8080"

	daemon, err := agent.NewDaemon(cfg)
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}

	daemon.Stop()
	daemon.Stop()
}
