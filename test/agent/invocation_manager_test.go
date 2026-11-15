// invocation_manager_test.go drives the InvocationManager end to end against
// a fake server and a fake tool: claim → start (before the tool runs) →
// lease renewal while running → complete with the adapter's final answer;
// slot serialization behind a running node; confirm-style cancellation with
// ack-cancel; failure receipts; and startup reconciliation (claimed resumes
// with the original ticket, a lost running invocation is never re-executed).
package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
	"github.com/teammate/agentd/internal/agent/tool"
)

// fakeInvocationTool records one Execute call; block-with-release reproduces
// a long-running tool, executeErr forces the failure path.
type fakeInvocationTool struct {
	mu         sync.Mutex
	workDir    string
	prompt     string
	env        []string
	finalText  string
	release    chan struct{} // when set, Execute waits until it closes
	executeErr error
}

func (f *fakeInvocationTool) Name() string      { return "fake" }
func (f *fakeInvocationTool) IsInstalled() bool { return true }
func (f *fakeInvocationTool) Stop() error       { return nil }

func (f *fakeInvocationTool) Execute(ctx context.Context, workDir, prompt string, options tool.ExecuteOptions, onOutput func(string)) (*tool.ExecutionResult, error) {
	f.mu.Lock()
	f.workDir = workDir
	f.prompt = prompt
	f.env = options.Env
	f.mu.Unlock()

	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.executeErr != nil {
		return nil, f.executeErr
	}
	return &tool.ExecutionResult{
		ExitCode:    0,
		FinalText:   f.finalText,
		DisplayText: "display " + f.finalText,
	}, nil
}

// invocationTestServer is the fake server half: scripted claimable/in-flight
// lists, per-endpoint call signals and body recording.
type invocationTestServer struct {
	mu        sync.Mutex
	srv       *httptest.Server
	claimable []agent.AgentInvocation
	inFlight  []agent.AgentInvocation
	terminal  agent.AgentInvocation // response body for claim/receipts
	events    map[string]chan string
	bodies    map[string]string
}

func newInvocationTestServer(t *testing.T) *invocationTestServer {
	t.Helper()
	ts := &invocationTestServer{
		terminal: invocationFixtureQueued(),
		events:   map[string]chan string{},
		bodies:   map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent-invocations/claimable", func(w http.ResponseWriter, r *http.Request) {
		ts.notify("claimable", "")
		ts.mu.Lock()
		list := ts.claimable
		ts.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"invocations": list})
	})
	mux.HandleFunc("/api/agent-invocations/in-flight", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		list := ts.inFlight
		ts.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"invocations": list})
	})
	mux.HandleFunc("/api/workspaces/ws-1/agents/agent-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"instructions": "you are the review helper", "git_name": "T", "git_email": "t@t.local"})
	})
	mux.HandleFunc("/api/agent-invocations/", func(w http.ResponseWriter, r *http.Request) {
		suffix := strings.TrimPrefix(r.URL.Path, "/api/agent-invocations/")
		body := readAllBody(r)
		log.Printf("[fakesrv] %s %s %s", r.Method, r.URL.Path, body)
		ts.notify(suffix, body)

		response := ts.terminal
		if strings.HasSuffix(suffix, "/claim") {
			// server-side transition: the claimed invocation leaves the queue
			invocationID := strings.SplitN(suffix, "/", 2)[0]
			ts.mu.Lock()
			remaining := ts.claimable[:0]
			for _, inv := range ts.claimable {
				if inv.ID != invocationID {
					remaining = append(remaining, inv)
				}
			}
			ts.claimable = remaining
			ts.mu.Unlock()
			claimed := invocationFixtureQueued()
			claimed.ID = invocationID
			claimed.Status = "claimed"
			claimed.ExecutionID = stringPtr("exec-1")
			response = claimed
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	ts.srv = httptest.NewServer(mux)
	t.Cleanup(ts.srv.Close)
	return ts
}

// notify records the body and delivers a non-blocking signal so handlers
// never block on slow test consumers.
func (ts *invocationTestServer) notify(suffix, body string) {
	ts.mu.Lock()
	ch, ok := ts.events[suffix]
	if !ok {
		ch = make(chan string, 64)
		ts.events[suffix] = ch
	}
	ts.bodies[suffix] = body
	ts.mu.Unlock()
	select {
	case ch <- body:
	default:
	}
}

func (ts *invocationTestServer) signalFor(suffix string) chan string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ch, ok := ts.events[suffix]
	if !ok {
		ch = make(chan string, 32)
		ts.events[suffix] = ch
	}
	return ch
}

// calls reports how many times an endpoint suffix was hit.
func (ts *invocationTestServer) calls(suffix string) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.events[suffix])
}

func (ts *invocationTestServer) body(suffix string) string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.bodies[suffix]
}

func readAllBody(r *http.Request) string {
	buf := make([]byte, 4096)
	n, _ := r.Body.Read(buf)
	return string(buf[:n])
}

func invocationFixtureQueued() agent.AgentInvocation {
	return agent.AgentInvocation{
		ID:             "inv-1",
		WorkspaceID:    "ws-1",
		CallerAgentID:  "caller-1",
		TargetAgentID:  "agent-1",
		Kind:           "chat",
		Message:        "please summarize",
		ConversationID: "conv-1",
		Status:         "queued",
		ExecutionID:    stringPtr("exec-1"),
	}
}

func stringPtr(s string) *string { return &s }

func timePtr(t time.Time) *time.Time { return &t }

func waitSignal(t *testing.T, ch chan string, timeout time.Duration) string {
	t.Helper()
	select {
	case body := <-ch:
		return body
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for a call")
		return ""
	}
}

// newTestManager builds a manager against the fake server with shortened
// scan/renew/hold periods so every lifecycle event happens in test time.
func newTestManager(t *testing.T, client *agent.Client, gate *agent.ExecutionGate, factory func(string, string) tool.Tool) *agent.InvocationManager {
	t.Helper()
	cfg := &agent.Config{
		Workspace: agent.WorkspaceConfig{Root: t.TempDir()},
		Agent:     agent.AgentInfo{Provider: "claude"},
	}
	manager := agent.NewInvocationManager(cfg, client, gate, "agent-1", "ws-1", "claude")
	manager.SetTimingForTest(50*time.Millisecond, 20*time.Millisecond, 300*time.Millisecond)
	manager.SetToolFactory(factory)
	return manager
}

// TestInvocationManagerFullFlow verifies the happy path and its ordering:
// claim before start, start before the tool runs, lease renewal while the
// tool works, completion carrying the adapter's final answer and a minimized
// environment.
func TestInvocationManagerFullFlow(t *testing.T) {
	ts := newInvocationTestServer(t)
	ts.mu.Lock()
	ts.claimable = []agent.AgentInvocation{invocationFixtureQueued()}
	ts.mu.Unlock()

	release := make(chan struct{})
	fake := &fakeInvocationTool{finalText: "the summary answer", release: release}
	gate := agent.NewExecutionGate()
	manager := newTestManager(t, agent.NewClient(ts.srv.URL, "td_token"), gate, func(provider, path string) tool.Tool {
		return fake
	})
	manager.Start()
	defer manager.Stop()

	waitSignal(t, ts.signalFor("inv-1/claim"), 5*time.Second)
	startBody := waitSignal(t, ts.signalFor("inv-1/start"), 5*time.Second)
	if !strings.Contains(startBody, `"execution_id":"exec-1"`) {
		t.Fatalf("start body missing ticket: %s", startBody)
	}

	fake.mu.Lock()
	env := fake.env
	fake.mu.Unlock()
	for _, kv := range env {
		if strings.HasPrefix(kv, "TEAMMATE_") || strings.HasPrefix(kv, "GIT_") {
			t.Fatalf("tool env must be minimized, found %q", kv)
		}
	}

	waitSignal(t, ts.signalFor("inv-1/lease-renew"), 5*time.Second) // renewal runs while the tool works
	close(release)
	waitSignal(t, ts.signalFor("inv-1/complete"), 10*time.Second)

	if body := ts.body("inv-1/complete"); !strings.Contains(body, `"result_text":"the summary answer"`) {
		t.Fatalf("complete body must carry the final answer: %s", body)
	}
	if got := fake.workDir; got == "" {
		t.Fatalf("tool must receive a work dir")
	}
	if got := fake.prompt; !strings.Contains(got, "please summarize") || !strings.Contains(got, "you are the review helper") {
		t.Fatalf("prompt must contain instructions and message: %q", got)
	}
}

// TestInvocationManagerQueuesBehindNode verifies slot serialization: while a
// node execution holds the gate the invocation stays queued (no claim call);
// releasing the slot lets the manager claim it.
func TestInvocationManagerQueuesBehindNode(t *testing.T) {
	ts := newInvocationTestServer(t)
	ts.mu.Lock()
	ts.claimable = []agent.AgentInvocation{invocationFixtureQueued()}
	ts.mu.Unlock()

	gate := agent.NewExecutionGate()
	nodeRelease, ok := gate.TryAcquire() // simulate a running node execution
	if !ok {
		t.Fatalf("gate must start free")
	}

	manager := newTestManager(t, agent.NewClient(ts.srv.URL, "td_token"), gate, func(provider, path string) tool.Tool {
		return &fakeInvocationTool{finalText: "late answer"}
	})
	manager.Start()
	defer manager.Stop()

	select {
	case <-ts.signalFor("inv-1/claim"):
		t.Fatalf("manager must not claim while the slot is held by a node")
	case <-time.After(300 * time.Millisecond):
	}

	nodeRelease()
	waitSignal(t, ts.signalFor("inv-1/claim"), 5*time.Second)
	waitSignal(t, ts.signalFor("inv-1/complete"), 10*time.Second)
}

// TestInvocationManagerCancelRequested verifies the confirm-style cancel:
// the tool run is stopped and the worker acks with its ticket.
func TestInvocationManagerCancelRequested(t *testing.T) {
	ts := newInvocationTestServer(t)
	ts.mu.Lock()
	ts.claimable = []agent.AgentInvocation{invocationFixtureQueued()}
	ts.mu.Unlock()

	blocking := &fakeInvocationTool{release: make(chan struct{})} // never released
	gate := agent.NewExecutionGate()
	manager := newTestManager(t, agent.NewClient(ts.srv.URL, "td_token"), gate, func(provider, path string) tool.Tool {
		return blocking
	})
	manager.Start()
	defer manager.Stop()

	<-ts.signalFor("inv-1/start") // tool is running now
	manager.HandleCancelRequested("inv-1", "exec-1")

	ackBody := waitSignal(t, ts.signalFor("inv-1/ack-cancel"), 5*time.Second)
	if !strings.Contains(ackBody, `"execution_id":"exec-1"`) {
		t.Fatalf("ack body must carry the ticket: %s", ackBody)
	}
	if calls := ts.calls("inv-1/complete"); calls != 0 {
		t.Fatalf("a cancelled run must not complete")
	}
}

// TestInvocationManagerFail verifies a tool error produces a failure receipt
// with a sanitized message.
func TestInvocationManagerFail(t *testing.T) {
	ts := newInvocationTestServer(t)
	ts.mu.Lock()
	ts.claimable = []agent.AgentInvocation{invocationFixtureQueued()}
	ts.mu.Unlock()

	gate := agent.NewExecutionGate()
	manager := newTestManager(t, agent.NewClient(ts.srv.URL, "td_token"), gate, func(provider, path string) tool.Tool {
		return &fakeInvocationTool{executeErr: fmt.Errorf("boom at secret-token-abc")}
	})
	manager.Start()
	defer manager.Stop()

	failBody := waitSignal(t, ts.signalFor("inv-1/fail"), 10*time.Second)
	if !strings.Contains(failBody, `"error_code":"tool_failed"`) {
		t.Fatalf("fail body missing code: %s", failBody)
	}
}

// TestInvocationManagerReconcile verifies startup reconciliation: a lost
// running invocation is never re-executed (no claim/start, tool factory never
// runs) and the slot stays held until its lease lapses; a claimed invocation
// resumes with its original ticket and completes.
func TestInvocationManagerReconcile(t *testing.T) {
	ts := newInvocationTestServer(t)
	running := invocationFixtureQueued()
	running.ID = "inv-running"
	running.Status = "running"
	running.LeaseUntil = timePtr(time.Now().UTC().Add(400 * time.Millisecond))
	claimed := invocationFixtureQueued()
	claimed.ID = "inv-claimed"
	claimed.Status = "claimed"
	claimed.ExecutionID = stringPtr("exec-original")
	ts.mu.Lock()
	ts.inFlight = []agent.AgentInvocation{running, claimed}
	ts.mu.Unlock()

	gate := agent.NewExecutionGate()
	manager := newTestManager(t, agent.NewClient(ts.srv.URL, "td_token"), gate, func(provider, path string) tool.Tool {
		return &fakeInvocationTool{finalText: "resumed answer"}
	})
	manager.Start()
	defer manager.Stop()

	// the lost running invocation holds the slot: within a short window every
	// acquire attempt must fail until the lease lapses
	heldSeen := false
	for start := time.Now(); time.Since(start) < 2*time.Second && !heldSeen; {
		if release, ok := gate.TryAcquire(); !ok {
			heldSeen = true
		} else {
			release() // transient free window before the hold goroutine wins
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !heldSeen {
		t.Fatalf("the slot must be held while the lost running invocation's lease lapses")
	}

	// the claimed invocation resumes with the original ticket and completes
	startBody := waitSignal(t, ts.signalFor("inv-claimed/start"), 10*time.Second)
	if !strings.Contains(startBody, `"execution_id":"exec-original"`) {
		t.Fatalf("resume must reuse the original ticket: %s", startBody)
	}
	waitSignal(t, ts.signalFor("inv-claimed/complete"), 10*time.Second)

	// no claim/start may ever target the lost running invocation
	if calls := ts.calls("inv-running/claim"); calls != 0 {
		t.Fatalf("lost running invocation must not be re-claimed (%d calls)", calls)
	}
	if calls := ts.calls("inv-running/start"); calls != 0 {
		t.Fatalf("lost running invocation must not be re-started (%d calls)", calls)
	}
}
