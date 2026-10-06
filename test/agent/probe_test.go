// probe_test.go covers the tool-detection snapshot.
package agent_test

import (
	"context"
	"testing"

	"github.com/teammate/agentd/internal/agent"
	"github.com/teammate/agentd/internal/agent/tool"
)

// fakeProbeTool reports a fixed install state.
type fakeProbeTool struct {
	installed bool
}

func (f *fakeProbeTool) Name() string { return "fake" }
func (f *fakeProbeTool) IsInstalled() bool {
	return f.installed
}
func (f *fakeProbeTool) Stop() error { return nil }
func (f *fakeProbeTool) Execute(_ context.Context, _ string, _ string, _ tool.ExecuteOptions, _ func(string)) (*tool.ExecutionResult, error) {
	return &tool.ExecutionResult{}, nil
}

func newProbeManagerWithInstall(installed map[string]bool) *agent.ProbeManager {
	probe := agent.NewProbeManager(agent.ToolsConfig{})
	probe.SetFactory(func(provider, path string) tool.Tool {
		return &fakeProbeTool{installed: installed[provider]}
	})
	return probe
}

func TestProbeSnapshotCoversAllProviders(t *testing.T) {
	probe := newProbeManagerWithInstall(map[string]bool{"claude": true})

	snapshot := probe.Snapshot()
	if len(snapshot) != len(agent.SupportedProviders) {
		t.Fatalf("expected %d providers, got %d", len(agent.SupportedProviders), len(snapshot))
	}
	byProvider := map[string]bool{}
	for _, info := range snapshot {
		byProvider[info.Provider] = info.Installed
	}
	if !byProvider["claude"] {
		t.Fatal("claude should be installed")
	}
	if byProvider["opencode"] {
		t.Fatal("opencode should be uninstalled")
	}

	last := probe.LastSnapshot()
	if len(last) != len(snapshot) {
		t.Fatalf("LastSnapshot = %d entries, want %d", len(last), len(snapshot))
	}
}
