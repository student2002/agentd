// client_agent_header_test.go asserts the X-Agent-ID injection contract:
// every per-agent call carries the header; daemon-domain and daemon-scoped
// calls never do.
package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
)

// headerRecorder captures the X-Agent-ID header value per handled request.
type headerRecorder struct {
	mu      sync.Mutex
	headers map[string]string // request path -> header value ("" = absent)
}

func (r *headerRecorder) record(path string, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headers[path] = req.Header.Get("X-Agent-ID")
}

func (r *headerRecorder) get(path string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.headers[path]
	return v, ok
}

func newRecordingClient(t *testing.T) (*agent.Client, *headerRecorder) {
	t.Helper()
	recorder := &headerRecorder{headers: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		recorder.record(req.URL.Path, req)
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/api/auth/token-exchange" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"session_token": "st_new_session_token",
				"expires_at":    time.Now().Add(24 * time.Hour),
			})
			return
		}
		// List endpoints decode the body into slices; everything else is an
		// object (git-credentials wraps its array: {"credentials":[...]}).
		// /api/tasks/1/nodes must match exactly so per-node action paths
		// (/nodes/n1/claim, ...) keep returning objects.
		if req.URL.Path == "/api/tasks/1/nodes" ||
			strings.HasSuffix(req.URL.Path, "/skills") ||
			strings.HasSuffix(req.URL.Path, "/mcp-servers") ||
			strings.HasSuffix(req.URL.Path, "/in-progress-nodes") ||
			strings.HasSuffix(req.URL.Path, "/projects") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return agent.NewClient(server.URL, "td_test_token"), recorder
}

func TestPerAgentMethodsInjectAgentHeader(t *testing.T) {
	client, recorder := newRecordingClient(t)
	ctx := context.Background()
	const agentID = "agent-uuid-1"
	const wsID = "ws-1"

	calls := []struct {
		name string
		path string
		run  func() error
	}{
		{"ListAgentSkills", "/api/workspaces/ws-1/agents/agent-uuid-1/skills", func() error {
			_, err := client.ListAgentSkills(ctx, wsID, agentID)
			return err
		}},
		{"ListAgentMcpServers", "/api/workspaces/ws-1/agents/agent-uuid-1/execution/mcp-servers", func() error {
			_, err := client.ListAgentMcpServers(ctx, wsID, agentID)
			return err
		}},
		{"GetAgent", "/api/workspaces/ws-1/agents/agent-uuid-1", func() error {
			return client.GetAgent(ctx, wsID, agentID, &struct{}{})
		}},
		{"GetInProgressNodes", "/api/workspaces/ws-1/agents/agent-uuid-1/in-progress-nodes", func() error {
			_, err := client.GetInProgressNodes(ctx, wsID, agentID)
			return err
		}},
		{"ClaimNode", "/api/tasks/1/nodes/n1/claim", func() error {
			_, err := client.ClaimNode(ctx, agentID, 1, "n1")
			return err
		}},
		{"SkipClaim", "/api/tasks/1/nodes/n1/skip-claim", func() error {
			return client.SkipClaim(ctx, agentID, 1, "n1")
		}},
		{"ApproveNode", "/api/tasks/1/nodes/n1/approve", func() error {
			return client.ApproveNode(ctx, agentID, 1, "n1", "ok")
		}},
		{"CompleteNode", "/api/tasks/1/nodes/n1/complete", func() error {
			return client.CompleteNode(ctx, agentID, 1, "n1", "done")
		}},
		{"RejectNode", "/api/tasks/1/nodes/n1/reject", func() error {
			return client.RejectNode(ctx, agentID, 1, "n1", "n0", "bad")
		}},
		{"ManualIntervention", "/api/tasks/1/nodes/n1/manual", func() error {
			return client.ManualIntervention(ctx, agentID, 1, "n1", "help")
		}},
		{"ReportTokenUsage", "/api/tasks/1/token-usage", func() error {
			return client.ReportTokenUsage(ctx, agentID, 1, "n1", agent.TokenUsageRequest{})
		}},
		{"SendMessage", "/api/tasks/1/messages", func() error {
			return client.SendMessage(ctx, agentID, 1, "n1", "log line")
		}},
		{"ReportInterrupt", "/api/tasks/1/nodes/n1/interrupt-ack", func() error {
			return client.ReportInterrupt(ctx, agentID, 1, "n1")
		}},
		{"ReportSummary", "/api/tasks/1/nodes/n1/summary", func() error {
			return client.ReportSummary(ctx, agentID, 1, "n1", "summary")
		}},
		{"ReportGitBranch", "/api/tasks/1/git-branch", func() error {
			return client.ReportGitBranch(ctx, agentID, 1, "task-1")
		}},
		{"PostComment", "/api/tasks/1/comments", func() error {
			return client.PostComment(ctx, agentID, 1, "hi", "agent", agentID)
		}},
		{"PostNodeComment", "/api/tasks/1/comments", func() error {
			return client.PostNodeComment(ctx, agentID, 1, "n1", "", "question", "hi")
		}},
		{"ListTaskNodes", "/api/tasks/1/nodes", func() error {
			_, err := client.ListTaskNodes(ctx, agentID, 1)
			return err
		}},
		{"GetTask", "/api/projects/p1/tasks/1", func() error {
			_, err := client.GetTask(ctx, agentID, "p1", 1)
			return err
		}},
	}

	for _, call := range calls {
		if err := call.run(); err != nil {
			t.Fatalf("%s: unexpected error: %v", call.name, err)
		}
		got, ok := recorder.get(call.path)
		if !ok {
			t.Fatalf("%s: no request recorded for %s", call.name, call.path)
		}
		if got != agentID {
			t.Fatalf("%s: X-Agent-ID = %q, want %q", call.name, got, agentID)
		}
	}
}

func TestListPendingNodesInjectsAgentHeader(t *testing.T) {
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/projects/p1/board" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"columns": []map[string]interface{}{
					{"key": "pending", "tasks": []map[string]interface{}{{"id": 1}}},
				},
			})
			return
		}
		if r.Header.Get("X-Agent-ID") != "agent-uuid-1" {
			t.Errorf("path %s: X-Agent-ID = %q, want agent-uuid-1", r.URL.Path, r.Header.Get("X-Agent-ID"))
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer board.Close()
	bc := agent.NewClient(board.URL, "td_test_token")

	if _, err := bc.ListPendingNodes(context.Background(), "agent-uuid-1", "p1"); err != nil {
		t.Fatalf("ListPendingNodes: %v", err)
	}
}

func TestDaemonDomainMethodsDoNotInjectAgentHeader(t *testing.T) {
	client, recorder := newRecordingClient(t)
	ctx := context.Background()

	if _, err := client.RegisterDaemon(ctx, agent.RegisterDaemonReport{}); err != nil {
		t.Fatalf("RegisterDaemon: %v", err)
	}
	if _, err := client.DaemonHeartbeat(ctx, nil); err != nil {
		t.Fatalf("DaemonHeartbeat: %v", err)
	}
	if err := client.DaemonDeregister(ctx); err != nil {
		t.Fatalf("DaemonDeregister: %v", err)
	}

	for _, path := range []string{
		"/api/daemons/register",
		"/api/daemons/heartbeat",
		"/api/daemons/deregister",
	} {
		if got, ok := recorder.get(path); ok && got != "" {
			t.Fatalf("%s: X-Agent-ID unexpectedly set to %q", path, got)
		} else if !ok {
			t.Fatalf("%s: no request recorded", path)
		}
	}
}

// TestWorkspaceDomainMethodsInjectAgentHeader covers the workspace-level reads
// (project listing/detail, git credentials): the daemon principal carries no
// workspace role, so these calls go through the effective-agent identity.
func TestWorkspaceDomainMethodsInjectAgentHeader(t *testing.T) {
	client, recorder := newRecordingClient(t)
	ctx := context.Background()

	if _, err := client.ListProjects(ctx, "agent-uuid-1", "ws-1"); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if _, err := client.GetProject(ctx, "agent-uuid-1", "ws-1", "p1"); err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if _, err := client.GetGitCredentials(ctx, "agent-uuid-1", "p1"); err != nil {
		t.Fatalf("GetGitCredentials: %v", err)
	}

	for _, path := range []string{
		"/api/workspaces/ws-1/projects",
		"/api/workspaces/ws-1/projects/p1",
		"/api/projects/p1/git-credentials",
	} {
		got, ok := recorder.get(path)
		if !ok {
			t.Fatalf("%s: no request recorded", path)
		}
		if got != "agent-uuid-1" {
			t.Fatalf("%s: X-Agent-ID = %q, want agent-uuid-1", path, got)
		}
	}
}

func TestDaemonTokenAuthAndSessionExchange(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-API-Key")
		if r.URL.Path == "/api/auth/token-exchange" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"session_token": "st_test_session",
				"expires_at":    time.Now().Add(24 * time.Hour),
			})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := agent.NewClient(server.URL, "td_test_token")
	if _, _, err := client.ExchangeToken(context.Background(), "td_test_token"); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if err := client.RefreshSessionToken(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if client.SessionToken != "st_test_session" {
		t.Fatalf("session token = %q", client.SessionToken)
	}
	if err := client.DaemonDeregister(context.Background()); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	if gotAuth != "st_test_session" {
		t.Fatalf("after refresh, X-API-Key = %q, want the session token", gotAuth)
	}
}

func TestIsNotFoundDetects404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon not found", http.StatusNotFound)
	}))
	defer server.Close()

	client := agent.NewClient(server.URL, "td_test_token")
	_, err := client.DaemonHeartbeat(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error from 404")
	}
	if !agent.IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
}
