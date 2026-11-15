// mcp_invocation_tools_test.go verifies the caller-side invocation MCP tools
// against a fake server: argument validation, the create body shape (kind,
// idempotency key, snapshot coordinates), await polling until the terminal
// answer, timeout reporting, result lookup and cancellation.
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// invocationServer is the fake server for the caller tools: it records the
// create body and drives the invocation status through a scripted sequence.
type invocationServer struct {
	mu         sync.Mutex
	srv        *httptest.Server
	agents     []InvocableAgent
	statuses   []string // GetInvocation returns these in order, then the last
	results    []string
	createBody map[string]interface{}
	created    int
	getCalls   int
	cancelled  int
}

func newInvocationServer(t *testing.T, statuses []string, results []string) *invocationServer {
	t.Helper()
	s := &invocationServer{
		agents:   []InvocableAgent{{ID: "target-1", Name: "beta", Provider: "atomcode"}},
		statuses: statuses,
		results:  results,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent-invocations/invocable-agents", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-ID") != "caller-1" {
			t.Errorf("invocable-agents must carry the caller identity header")
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": s.agents})
	})
	mux.HandleFunc("/api/agent-invocations", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.createBody = body
		s.created++
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "inv-1", "status": "queued", "conversation_id": "conv-1",
		})
	})
	mux.HandleFunc("/api/agent-invocations/inv-1/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.cancelled++
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "inv-1", "status": "cancelled"})
	})
	mux.HandleFunc("/api/agent-invocations/inv-1", func(w http.ResponseWriter, r *http.Request) {
		// Status advances with each lookup so await polling observes progress.
		s.mu.Lock()
		s.getCalls++
		index := s.getCalls - 1
		if index >= len(s.statuses) {
			index = len(s.statuses) - 1
		}
		status := s.statuses[index]
		result := s.results[index]
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "inv-1", "status": status,
			"result_text":      result,
			"progress_stage":   "analyzing",
			"progress_message": "reading files",
		})
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func callerCfg(serverURL string) MCPServerConfig {
	return MCPServerConfig{
		AgentID:     "caller-1",
		WorkspaceID: "ws-1",
		ServerURL:   serverURL,
		Token:       "st_token",
	}
}

// TestCallInvocationToolsListAndCreate verifies list_invocable_agents and the
// create body of both invoke kinds (idempotency key present, snapshot
// coordinates only on code_review).
func TestCallInvocationToolsListAndCreate(t *testing.T) {
	s := newInvocationServer(t, []string{"queued"}, []string{""})
	cfg := callerCfg(s.srv.URL)

	// no credentials → all invocation tools refuse before any HTTP call
	text, isErr, handled := callInvocationTool(context.Background(), callerCfg(""), "invoke_agent",
		map[string]interface{}{"target_agent_id": "t", "message": "m"})
	if !handled || !isErr || !strings.Contains(text, "no connection credentials") {
		t.Fatalf("credential guard: %q handled=%v isErr=%v", text, handled, isErr)
	}

	text, isErr, _ = callInvocationTool(context.Background(), cfg, "list_invocable_agents", map[string]interface{}{})
	if isErr || !strings.Contains(text, "target-1") || !strings.Contains(text, "beta") {
		t.Fatalf("list_invocable_agents: %q isErr=%v", text, isErr)
	}

	text, isErr, _ = callInvocationTool(context.Background(), cfg, "invoke_agent",
		map[string]interface{}{"target_agent_id": "target-1", "message": "do the thing", "deadline_seconds": 60})
	if isErr || !strings.Contains(text, "inv-1") || !strings.Contains(text, "queued") {
		t.Fatalf("invoke_agent: %q isErr=%v", text, isErr)
	}
	s.mu.Lock()
	body := s.createBody
	s.mu.Unlock()
	if body["kind"] != "chat" || body["idempotency_key"] == "" || body["target_agent_id"] != "target-1" {
		t.Fatalf("create body malformed: %+v", body)
	}
	if _, hasBase := body["base_sha"]; hasBase {
		t.Fatalf("chat creation must not carry snapshot fields: %+v", body)
	}

	// missing required args are validated locally
	text, isErr, _ = callInvocationTool(context.Background(), cfg, "invoke_agent", map[string]interface{}{"message": "m"})
	if !isErr || !strings.Contains(text, "target_agent_id") {
		t.Fatalf("missing target: %q isErr=%v", text, isErr)
	}

	// code_review carries the snapshot tuple
	text, isErr, _ = callInvocationTool(context.Background(), cfg, "invoke_code_review",
		map[string]interface{}{
			"target_agent_id": "target-1", "project_id": "p1",
			"base_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "head_ref": "feature/x",
			"head_sha": strings.Repeat("b", 40), "message": "review it",
		})
	if isErr || !strings.Contains(text, "inv-1") {
		t.Fatalf("invoke_code_review: %q isErr=%v", text, isErr)
	}
	s.mu.Lock()
	body = s.createBody
	s.mu.Unlock()
	if body["kind"] != "code_review" || body["project_id"] != "p1" || body["head_ref"] != "feature/x" {
		t.Fatalf("code_review body malformed: %+v", body)
	}
}

// TestCallInvocationToolsAwait verifies await_invocation polls until the
// terminal status and returns the final answer; a timeout reports the current
// status instead of an error.
func TestCallInvocationToolsAwait(t *testing.T) {
	s := newInvocationServer(t, []string{"running", "running", "succeeded"}, []string{"", "", "all good"})
	cfg := callerCfg(s.srv.URL)

	text, isErr, _ := callInvocationTool(context.Background(), cfg, "await_invocation",
		map[string]interface{}{"invocation_id": "inv-1"})
	if isErr || !strings.Contains(text, "status succeeded") || !strings.Contains(text, "all good") {
		t.Fatalf("await to terminal: %q isErr=%v", text, isErr)
	}

	// timeout: statuses never terminal within the window
	s2 := newInvocationServer(t, []string{"running"}, []string{""})
	text, isErr, _ = callInvocationTool(context.Background(), callerCfg(s2.srv.URL), "await_invocation",
		map[string]interface{}{"invocation_id": "inv-1", "timeout_seconds": 1})
	if isErr || !strings.Contains(text, "not finished yet") || !strings.Contains(text, "status running") {
		t.Fatalf("await timeout: %q isErr=%v", text, isErr)
	}

}

// TestCallInvocationToolsResultAndCancel verifies result lookup and
// cancellation reporting.
func TestCallInvocationToolsResultAndCancel(t *testing.T) {
	s := newInvocationServer(t, []string{"succeeded"}, []string{"final text here"})
	cfg := callerCfg(s.srv.URL)

	text, isErr, _ := callInvocationTool(context.Background(), cfg, "invocation_result",
		map[string]interface{}{"invocation_id": "inv-1"})
	if isErr || !strings.Contains(text, "status succeeded") || !strings.Contains(text, "final text here") {
		t.Fatalf("invocation_result: %q isErr=%v", text, isErr)
	}

	text, isErr, _ = callInvocationTool(context.Background(), cfg, "cancel_invocation",
		map[string]interface{}{"invocation_id": "inv-1"})
	if isErr || !strings.Contains(text, "cancelled") {
		t.Fatalf("cancel_invocation: %q isErr=%v", text, isErr)
	}
	s.mu.Lock()
	cancelled := s.cancelled
	s.mu.Unlock()
	if cancelled != 1 {
		t.Fatalf("cancel endpoint not called")
	}

	// non-invocation names fall through unhandled
	if _, _, handled := callInvocationTool(context.Background(), cfg, "search_instance_memory", map[string]interface{}{}); handled {
		t.Fatalf("non-invocation tool must fall through")
	}
}
