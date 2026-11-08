// executor_receipt_test.go verifies the execution receipts the executor sends
// for agent tickets: success goes to /complete with the ticket's execution ID,
// summary and structured output; ordinary execution failures go to /fail with
// the same execution ID; interrupted runs report neither.
package agent_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
)

// receiptRecorder captures the execution receipts posted by the executor.
type receiptRecorder struct {
	mu        sync.Mutex
	fails     []map[string]interface{}
	completes []map[string]interface{}
	manuals   []map[string]interface{}
	messages  []map[string]interface{}
}

func (r *receiptRecorder) snapshot() (fails, completes, manuals []map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fails, r.completes, r.manuals
}

// snapshotMessages returns the log messages posted so far.
func (r *receiptRecorder) snapshotMessages() []map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.messages
}

// waitForMessage blocks until a message with the given content appears or the
// timeout elapses.
func (r *receiptRecorder) waitForMessage(content string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range r.snapshotMessages() {
			if m["content"] == content {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// newReceiptServer builds a client pointing at a fake server that answers the
// read endpoints Execute needs (task, project, git credentials, comments,
// agent profile, skills, MCP servers, workspace) with minimal payloads and
// records every /complete, /fail and /manual receipt body.
func newReceiptServer(t *testing.T) (*agent.Client, *receiptRecorder) {
	return newReceiptServerWithCompleteStatus(t, http.StatusOK)
}

// newReceiptServerWithCompleteStatus behaves like newReceiptServer but answers
// /complete with the given HTTP status.
func newReceiptServerWithCompleteStatus(t *testing.T, completeStatus int) (*agent.Client, *receiptRecorder) {
	t.Helper()
	rec := &receiptRecorder{}
	mux := http.NewServeMux()

	writeMinimal := func(w http.ResponseWriter, value interface{}) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}

	mux.HandleFunc("/api/projects/proj-1/tasks/12", func(w http.ResponseWriter, r *http.Request) {
		writeMinimal(w, map[string]interface{}{"id": 12, "title": "Task", "project_id": "proj-1"})
	})
	mux.HandleFunc("/api/projects/proj-1/git-credentials", func(w http.ResponseWriter, r *http.Request) {
		writeMinimal(w, map[string]interface{}{"credentials": []interface{}{}})
	})
	mux.HandleFunc("/api/tasks/12/comments", func(w http.ResponseWriter, r *http.Request) {
		writeMinimal(w, []interface{}{})
	})
	mux.HandleFunc("/api/workspaces/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces/ws-1":
			writeMinimal(w, map[string]interface{}{"id": "ws-1", "name": "WS"})
		case "/api/workspaces/ws-1/projects/proj-1":
			writeMinimal(w, map[string]interface{}{"id": "proj-1", "name": "P", "repo_url": ""})
		case "/api/workspaces/ws-1/agents/agent-1/skills",
			"/api/workspaces/ws-1/agents/agent-1/execution/mcp-servers":
			writeMinimal(w, []interface{}{})
		default:
			writeMinimal(w, map[string]interface{}{"instructions": "Be helpful."})
		}
	})
	mux.HandleFunc("/api/tasks/12/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.mu.Lock()
			rec.messages = append(rec.messages, body)
			rec.mu.Unlock()
		}
		writeMinimal(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeMinimal(w, map[string]string{})
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/complete"):
			rec.completes = append(rec.completes, body)
			rec.mu.Unlock()
			if completeStatus != http.StatusOK {
				http.Error(w, "superseded execution", completeStatus)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/fail"):
			rec.fails = append(rec.fails, body)
			rec.mu.Unlock()
		case strings.HasSuffix(r.URL.Path, "/manual"):
			rec.manuals = append(rec.manuals, body)
			rec.mu.Unlock()
		default:
			rec.mu.Unlock()
		}
		writeMinimal(w, map[string]string{"status": "ok"})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return agent.NewClient(server.URL, "td_receipt_token"), rec
}

func receiptNode() agent.TaskNode {
	return agent.TaskNode{
		ID:            "n-recv",
		TaskID:        12,
		Name:          "code",
		Status:        "running",
		ExecutionID:   "exec-recv",
		ExecutionKind: agent.ExecutionKindAgent,
		Instruction:   "do the work",
		Input:         json.RawMessage(`{"title":"bug"}`),
	}
}

func receiptRunContext(client *agent.Client) agent.RunContext {
	return agent.RunContext{
		Client:      client,
		AgentID:     "agent-1",
		WorkspaceID: "ws-1",
		ProjectID:   "proj-1",
		TaskID:      12,
		NodeID:      "n-recv",
	}
}

// TestExecuteReportsCompleteReceiptWithExecutionID proves a successful agent
// execution completes the node through /complete, echoing the ticket's
// execution ID and carrying a text summary plus structured output — and never
// touches /fail or /manual.
func TestExecuteReportsCompleteReceiptWithExecutionID(t *testing.T) {
	t.Setenv("TEAMMATE_DISK_QUOTA_GB", "100")
	cfg := &agent.Config{
		Server:    agent.ServerConfig{URL: "http://127.0.0.1:1"},
		Agent:     agent.AgentInfo{Name: "Agent One", Provider: "claude"},
		Workspace: agent.WorkspaceConfig{Root: t.TempDir()},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}
	client, rec := newReceiptServer(t)
	executor := agent.NewTaskExecutorWithObserver(cfg, &recordingExecutionObserver{})
	ft := newFakeTool("claude")
	ft.SetInterventionMode(true)
	ft.SetOutput("did the thing")
	executor.SetToolFactoryForTest(func() agent.TestTool {
		return &fakeToolAdapter{tool: ft}
	})

	executor.Execute(receiptRunContext(client), receiptNode())

	fails, completes, manuals := rec.snapshot()
	if len(completes) != 1 {
		t.Fatalf("expected exactly one /complete receipt, got %d", len(completes))
	}
	body := completes[0]
	if body["execution_id"] != "exec-recv" {
		t.Errorf("complete execution_id = %v, want exec-recv", body["execution_id"])
	}
	summary, _ := body["summary"].(string)
	if summary == "" {
		t.Error("complete receipt must carry a non-empty summary")
	}
	output, ok := body["output"].(map[string]interface{})
	if !ok || len(output) != 0 {
		t.Errorf("complete output = %#v, want an empty object (no git repo in this fixture)", body["output"])
	}
	if len(fails) != 0 || len(manuals) != 0 {
		t.Errorf("successful run must not report /fail or /manual, got fails=%d manuals=%d", len(fails), len(manuals))
	}
}

// TestCompleteReceipt409NotRetriedNorEscalated proves a 409 rejection of the
// completion receipt (stale execution superseded server-side) triggers neither
// a retry of /complete nor a comment/manual-intervention escalation — the
// superseded run's business is done.
func TestCompleteReceipt409NotRetriedNorEscalated(t *testing.T) {
	t.Setenv("TEAMMATE_DISK_QUOTA_GB", "100")
	cfg := &agent.Config{
		Server:    agent.ServerConfig{URL: "http://127.0.0.1:1"},
		Agent:     agent.AgentInfo{Name: "Agent One", Provider: "claude"},
		Workspace: agent.WorkspaceConfig{Root: t.TempDir()},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}
	client, rec := newReceiptServerWithCompleteStatus(t, http.StatusConflict)
	executor := agent.NewTaskExecutorWithObserver(cfg, &recordingExecutionObserver{})
	ft := newFakeTool("claude")
	ft.SetInterventionMode(true)
	ft.SetOutput("done")
	executor.SetToolFactoryForTest(func() agent.TestTool {
		return &fakeToolAdapter{tool: ft}
	})

	executor.Execute(receiptRunContext(client), receiptNode())

	fails, completes, manuals := rec.snapshot()
	if len(completes) != 1 {
		t.Fatalf("expected exactly one /complete attempt, got %d (409 must not be retried)", len(completes))
	}
	if len(manuals) != 0 {
		t.Fatalf("409 rejection must not escalate to manual intervention, got %d", len(manuals))
	}
	if len(fails) != 0 {
		t.Fatalf("409 rejection must not report /fail, got %d", len(fails))
	}
}

// TestExecuteReportsFailReceiptOnToolError proves an ordinary tool failure is
// reported through /fail with the ticket's execution ID and an error code —
// the server alone decides retry and routing, so the daemon sends no
// manual-intervention report for it.
func TestExecuteReportsFailReceiptOnToolError(t *testing.T) {
	t.Setenv("TEAMMATE_DISK_QUOTA_GB", "100")
	cfg := &agent.Config{
		Server:    agent.ServerConfig{URL: "http://127.0.0.1:1"},
		Agent:     agent.AgentInfo{Name: "Agent One", Provider: "claude"},
		Workspace: agent.WorkspaceConfig{Root: t.TempDir()},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}
	client, rec := newReceiptServer(t)
	executor := agent.NewTaskExecutorWithObserver(cfg, &recordingExecutionObserver{})
	ft := newFakeTool("claude")
	ft.SetError(errors.New("tool crashed"))
	executor.SetToolFactoryForTest(func() agent.TestTool {
		return &fakeToolAdapter{tool: ft}
	})

	executor.Execute(receiptRunContext(client), receiptNode())

	fails, completes, manuals := rec.snapshot()
	if len(fails) != 1 {
		t.Fatalf("expected exactly one /fail receipt, got %d", len(fails))
	}
	body := fails[0]
	if body["execution_id"] != "exec-recv" {
		t.Errorf("fail execution_id = %v, want exec-recv", body["execution_id"])
	}
	if body["error_code"] != "agent_execution_failed" {
		t.Errorf("error_code = %v, want agent_execution_failed", body["error_code"])
	}
	if msg, _ := body["error_message"].(string); msg == "" {
		t.Error("error_message must not be empty")
	}
	if len(completes) != 0 || len(manuals) != 0 {
		t.Errorf("failed run must not report /complete or /manual, got completes=%d manuals=%d", len(completes), len(manuals))
	}
}

// TestInterruptReportsNoExecutionReceipt proves a server-interrupted execution
// reports neither /fail nor /complete: Interrupt owns the interrupt reporting
// path, and the Execute err-branch must stay silent.
func TestInterruptReportsNoExecutionReceipt(t *testing.T) {
	t.Setenv("TEAMMATE_DISK_QUOTA_GB", "100")
	cfg := &agent.Config{
		Server:    agent.ServerConfig{URL: "http://127.0.0.1:1"},
		Agent:     agent.AgentInfo{Name: "Agent One", Provider: "claude"},
		Workspace: agent.WorkspaceConfig{Root: t.TempDir()},
		Git:       agent.GitConfig{BaseBranch: "master"},
	}
	client, rec := newReceiptServer(t)
	executor := agent.NewTaskExecutorWithObserver(cfg, &recordingExecutionObserver{})
	ft := newFakeTool("claude")
	executor.SetToolFactoryForTest(func() agent.TestTool {
		return &fakeToolAdapter{tool: ft}
	})

	go executor.Execute(receiptRunContext(client), receiptNode())
	executor.WaitRunningForTest(t)
	ft.waitStarted()

	if err := executor.Interrupt(12, "n-recv"); err != nil {
		t.Fatalf("Interrupt returned error: %v", err)
	}

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		fails, completes, _ := rec.snapshot()
		if len(fails) != 0 || len(completes) != 0 {
			t.Fatalf("interrupted run reported execution receipts: fails=%d completes=%d", len(fails), len(completes))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
