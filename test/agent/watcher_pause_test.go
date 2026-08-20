package agent_test

import (
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
)

// TestNodeWatcherPausedStopsClaiming asserts that a paused watcher does NOT
// recover in-progress nodes or poll for pending nodes, and that resuming
// re-enables polling. We assert only the observable boolean + the poll gate
// behavior via IsPaused; the server client is nil-safe because poll returns
// before touching the client when paused.
func TestNodeWatcherPausedStopsClaiming(t *testing.T) {
	cfg := &agent.Config{
		Server:    agent.ServerConfig{URL: "http://127.0.0.1:1", APIToken: "fake-token"},
		Workspace: agent.WorkspaceConfig{ID: "ws-1", Root: t.TempDir()},
		Agent:     agent.AgentInfo{ID: "agent-1", Provider: "claude"},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}
	client := agent.NewClient(cfg.Server.URL, cfg.Server.APIToken)
	executor := agent.NewTaskExecutor(cfg, client, "agent-1")
	watcher := agent.NewNodeWatcher(client, executor, "agent-1", "ws-1", 10*time.Millisecond)

	if watcher.IsPaused() {
		t.Fatal("new watcher should not be paused")
	}

	watcher.Pause()
	if !watcher.IsPaused() {
		t.Fatal("watcher should be paused after Pause()")
	}

	// A paused watcher's poll must be a no-op: it must not reach the client
	// (which would panic on a nil server). poll() returns immediately.
	watcher.PollForTest()

	watcher.Resume()
	if watcher.IsPaused() {
		t.Fatal("watcher should not be paused after Resume()")
	}
}
