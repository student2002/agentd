package agent_test

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/teammate/agentd/internal/agent/tool"
)

// fakeTool is a controllable tool.Tool for executor tests. It blocks on Execute
// until either finish() is called or the context cancels. In intervention mode
// (set via SetInterventionMode) it returns synchronously so intervention-turn
// tests can run without the started/finish channels.
type fakeTool struct {
	mu          sync.Mutex
	name        string
	startedCh   chan struct{}
	finishCh    chan struct{}
	stopped     bool
	outputLines []string

	output          string
	calls           int
	lastPrompt      string
	interventionMode atomic.Bool
}

func newFakeTool(name string) *fakeTool {
	return &fakeTool{
		name:      name,
		startedCh: make(chan struct{}),
		finishCh:  make(chan struct{}),
	}
}

func (t *fakeTool) Name() string      { return t.name }
func (t *fakeTool) IsInstalled() bool { return true }

func (t *fakeTool) Execute(ctx context.Context, _ string, prompt string, _ tool.ExecuteOptions, onOutput func(string)) (*tool.ExecutionResult, error) {
	t.mu.Lock()
	t.calls++
	t.lastPrompt = prompt
	output := t.output
	intervention := t.interventionMode.Load()
	lines := t.outputLines
	t.mu.Unlock()

	if intervention {
		if onOutput != nil && output != "" {
			onOutput(output)
		}
		return &tool.ExecutionResult{Output: output}, nil
	}

	t.mu.Lock()
	close(t.startedCh)
	t.mu.Unlock()

	for _, line := range lines {
		if onOutput != nil {
			onOutput(line)
		}
	}

	select {
	case <-t.finishCh:
		return &tool.ExecutionResult{Output: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *fakeTool) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	return nil
}

// SetInterventionMode makes Execute return synchronously with the configured output.
func (t *fakeTool) SetInterventionMode(b bool) { t.interventionMode.Store(b) }
func (t *fakeTool) SetOutput(s string)         { t.mu.Lock(); t.output = s; t.mu.Unlock() }
func (t *fakeTool) callCount() int             { t.mu.Lock(); defer t.mu.Unlock(); return t.calls }
func (t *fakeTool) lastPromptValue() string    { t.mu.Lock(); defer t.mu.Unlock(); return t.lastPrompt }

func (t *fakeTool) finish()      { close(t.finishCh) }
func (t *fakeTool) waitStarted() { <-t.startedCh }
func (t *fakeTool) FinishCh() <-chan struct{} { return t.finishCh }
func (t *fakeTool) isStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

func sleep() { time.Sleep(10 * time.Millisecond) }

// fakeToolAdapter bridges the test's fakeTool to the executor's TestTool seam.
type fakeToolAdapter struct {
	tool *fakeTool
}

func (a *fakeToolAdapter) Name() string { return a.tool.Name() }

func (a *fakeToolAdapter) Execute(ctx context.Context, workDir, prompt string, onOutput func(string)) (*tool.ExecutionResult, error) {
	return a.tool.Execute(ctx, workDir, prompt, tool.ExecuteOptions{}, onOutput)
}

func (a *fakeToolAdapter) Stop() error       { return a.tool.Stop() }
func (a *fakeToolAdapter) IsInstalled() bool { return a.tool.IsInstalled() }
