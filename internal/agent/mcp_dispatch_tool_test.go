// mcp_dispatch_tool_test.go verifies the dispatch_task_to_machine MCP tool
// branch: required-argument validation, project_id fallback to the execution
// context, and the success path through a fake server (package-internal so
// callMemoryTool is reachable directly).
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func dispatchCfg(serverURL, token string) MCPServerConfig {
	return MCPServerConfig{
		InstanceName: "agent-x",
		AgentID:      "agent-1",
		ServerURL:    serverURL,
		Token:        token,
	}
}

// TestCallDispatchToolMissingArgs verifies each required argument is enforced
// without touching the server.
func TestCallDispatchToolMissingArgs(t *testing.T) {
	cfg := dispatchCfg("", "") // no credentials: validation must fail first
	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"all missing", map[string]interface{}{}},
		{"missing template", map[string]interface{}{"title": "t", "target_machine": "box-b"}},
		{"missing target", map[string]interface{}{"title": "t", "template_id": "tpl"}},
		{"missing title", map[string]interface{}{"template_id": "tpl", "target_machine": "box-b"}},
		{"missing project fallback", map[string]interface{}{"title": "t", "template_id": "tpl", "target_machine": "box-b"}},
	}
	for _, tc := range cases {
		text, isErr := callMemoryTool(context.Background(), nil, cfg, "dispatch_task_to_machine", tc.args)
		if !isErr || text == "" {
			t.Fatalf("%s: expected error text, got %q (isErr=%v)", tc.name, text, isErr)
		}
	}
}

// TestCallDispatchToolProjectFallbackAndSuccess verifies project_id falls back
// to the execution context's project and the success message carries the task
// id from the server.
func TestCallDispatchToolProjectFallbackAndSuccess(t *testing.T) {
	var gotPath string
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/projects/", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"task_id": 7})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := dispatchCfg(srv.URL, "st_token")
	cfg.ProjectID = "proj-current"

	text, isErr := callMemoryTool(context.Background(), nil, cfg, "dispatch_task_to_machine", map[string]interface{}{
		"title": "Fix flaky test", "template_id": "tpl-1", "target_machine": "box-b",
		"description": "run it there",
	})
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if calls.Load() != 1 {
		t.Fatalf("server calls = %d, want 1", calls.Load())
	}
	if gotPath != "/api/projects/proj-current/workflows/tpl-1/runs" {
		t.Fatalf("request path = %q (project fallback failed)", gotPath)
	}
	if text == "" {
		t.Fatal("expected success message")
	}
}
