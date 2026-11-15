// client_dispatch_task_test.go verifies the cross-machine dispatch client
// call: it posts to the runs endpoint with the agent header, carries the
// target machine name, and decodes the returned task id.
package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestDispatchTaskToMachine verifies the runs POST: path, agent header,
// target daemon name in the body, and task id decoding.
func TestDispatchTaskToMachine(t *testing.T) {
	var gotPath, gotAgent, gotTarget, gotTitle string
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/projects/p1/workflows/tpl1/runs", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotPath = r.URL.Path
		gotAgent = r.Header.Get("X-Agent-ID")
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotTarget, _ = body["target_daemon_name"].(string)
		gotTitle, _ = body["title"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"task_id": 42, "title": gotTitle})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := agent.NewClient(srv.URL, "st_token")
	taskID, err := client.DispatchTaskToMachine(context.Background(), "agent-1", "p1", "tpl1", "Fix the flaky test", "run on machine b", "box-b")
	if err != nil {
		t.Fatalf("DispatchTaskToMachine: %v", err)
	}
	if taskID != 42 {
		t.Fatalf("task id = %d, want 42", taskID)
	}
	if calls.Load() != 1 {
		t.Fatalf("server calls = %d, want 1", calls.Load())
	}
	if gotPath != "/api/projects/p1/workflows/tpl1/runs" {
		t.Fatalf("request path = %q", gotPath)
	}
	if gotAgent != "agent-1" {
		t.Fatalf("X-Agent-ID = %q, want agent-1", gotAgent)
	}
	if gotTarget != "box-b" {
		t.Fatalf("target_daemon_name = %q, want box-b", gotTarget)
	}
	if gotTitle != "Fix the flaky test" {
		t.Fatalf("title = %q", gotTitle)
	}
}
