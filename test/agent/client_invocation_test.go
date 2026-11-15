// client_invocation_test.go verifies the invocation client surface against a
// fake server: paths, agent identity headers, ticket bodies, and error
// classification for the conflict-driven control flow.
package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func invocationFixture() agent.AgentInvocation {
	return agent.AgentInvocation{
		ID:            "inv-1",
		WorkspaceID:   "ws-1",
		CallerAgentID: "caller-1",
		TargetAgentID: "target-1",
		Kind:          "chat",
		Message:       "hello",
		Status:        "queued",
	}
}

func writeInvocation(t *testing.T, w http.ResponseWriter, inv agent.AgentInvocation) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(inv); err != nil {
		t.Fatalf("encode invocation: %v", err)
	}
}

// TestInvocationClientMethods verifies every client method hits its path with
// the agent header and the documented body shape.
func TestInvocationClientMethods(t *testing.T) {
	var gotAgent, gotMethod, gotPath, gotBody string
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.Header.Get("X-Agent-ID")
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		writeInvocation(t, w, invocationFixture())
	}
	mux.HandleFunc("/api/agent-invocations/claimable", handler)
	mux.HandleFunc("/api/agent-invocations/in-flight", handler)
	mux.HandleFunc("/api/agent-invocations/inv-1", handler)
	mux.HandleFunc("/api/agent-invocations/inv-1/claim", handler)
	mux.HandleFunc("/api/agent-invocations/inv-1/start", handler)
	mux.HandleFunc("/api/agent-invocations/inv-1/lease-renew", handler)
	mux.HandleFunc("/api/agent-invocations/inv-1/progress", handler)
	mux.HandleFunc("/api/agent-invocations/inv-1/complete", handler)
	mux.HandleFunc("/api/agent-invocations/inv-1/fail", handler)
	mux.HandleFunc("/api/agent-invocations/inv-1/ack-cancel", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := agent.NewClient(srv.URL, "td_token")
	ctx := context.Background()

	type check struct {
		name         string
		call         func() error
		wantMethod   string
		wantPath     string
		wantBodyPart string
	}
	checks := []check{
		{"claimable", func() error { _, err := client.ListClaimableInvocations(ctx, "agent-1"); return err },
			"GET", "/api/agent-invocations/claimable", ""},
		{"in-flight", func() error { _, err := client.ListInFlightInvocations(ctx, "agent-1"); return err },
			"GET", "/api/agent-invocations/in-flight", ""},
		{"get", func() error { _, err := client.GetInvocation(ctx, "agent-1", "inv-1"); return err },
			"GET", "/api/agent-invocations/inv-1", ""},
		{"claim", func() error { _, err := client.ClaimInvocation(ctx, "agent-1", "inv-1"); return err },
			"POST", "/api/agent-invocations/inv-1/claim", ""},
		{"start", func() error { _, err := client.StartInvocation(ctx, "agent-1", "inv-1", "exec-1"); return err },
			"POST", "/api/agent-invocations/inv-1/start", `"execution_id":"exec-1"`},
		{"lease-renew", func() error { _, err := client.RenewInvocationLease(ctx, "agent-1", "inv-1", "exec-1"); return err },
			"POST", "/api/agent-invocations/inv-1/lease-renew", `"execution_id":"exec-1"`},
		{"progress", func() error {
			_, err := client.UpdateInvocationProgress(ctx, "agent-1", "inv-1", "exec-1", "analyzing", "reading")
			return err
		},
			"POST", "/api/agent-invocations/inv-1/progress", `"stage":"analyzing"`},
		{"complete", func() error {
			_, err := client.CompleteInvocation(ctx, "agent-1", "inv-1", "exec-1", "done", map[string]interface{}{"resumed": true})
			return err
		},
			"POST", "/api/agent-invocations/inv-1/complete", `"result_text":"done"`},
		{"fail", func() error {
			_, err := client.FailInvocation(ctx, "agent-1", "inv-1", "exec-1", "boom", "bad")
			return err
		},
			"POST", "/api/agent-invocations/inv-1/fail", `"error_code":"boom"`},
		{"ack-cancel", func() error { _, err := client.AckCancelInvocation(ctx, "agent-1", "inv-1", "exec-1"); return err },
			"POST", "/api/agent-invocations/inv-1/ack-cancel", `"execution_id":"exec-1"`},
	}
	for _, c := range checks {
		if err := c.call(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if gotAgent != "agent-1" {
			t.Fatalf("%s: X-Agent-ID = %q, want agent-1", c.name, gotAgent)
		}
		if gotMethod != c.wantMethod || gotPath != c.wantPath {
			t.Fatalf("%s: %s %s, want %s %s", c.name, gotMethod, gotPath, c.wantMethod, c.wantPath)
		}
		if c.wantBodyPart != "" && !strings.Contains(gotBody, c.wantBodyPart) {
			t.Fatalf("%s: body %s missing %s", c.name, gotBody, c.wantBodyPart)
		}
	}
}

// TestInvocationClientConflict verifies that a 409 receipt rejection is
// surfaced as an API error IsConflict recognizes (the CAS control signal).
func TestInvocationClientConflict(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent-invocations/inv-1/complete", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"conflict","message":"invocation is already terminal"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := agent.NewClient(srv.URL, "td_token")
	_, err := client.CompleteInvocation(context.Background(), "agent-1", "inv-1", "exec-1", "late", nil)
	if err == nil {
		t.Fatalf("expected an error on 409")
	}
	if !agent.IsConflict(err) {
		t.Fatalf("409 must classify as conflict: %v", err)
	}
}
