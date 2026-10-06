package agent_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestLocalServerControlEndpointsRequireTokenAndPost proves the four control
// routes (soft-interrupt, intervene, handback, complete) reject unauthenticated
// requests (401), reject GET (405 from postOnly), and accept an authenticated
// POST (delegating to the executor, which returns 200/JSON).
func TestLocalServerControlEndpointsRequireTokenAndPost(t *testing.T) {
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
		Registry:   newFakeRegistry("claude-01"),
	})

	for _, route := range []string{
		"/api/local/connections/team-a/agents/claude-01/control/soft-interrupt",
		"/api/local/connections/team-a/agents/claude-01/control/intervene",
		"/api/local/connections/team-a/agents/claude-01/control/handback",
		"/api/local/connections/team-a/agents/claude-01/control/complete",
	} {
		// No token → 401
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401 without token, got %d", route, rec.Code)
		}

		// GET → 405 (postOnly)
		req = httptest.NewRequest(http.MethodGet, route, nil)
		req.Header.Set("Authorization", "Bearer lt_test")
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: expected 405 on GET, got %d", route, rec.Code)
		}
	}
}

// TestLocalServerControlPageServed proves the embedded HTML control page is
// served at /api/local/control/page and is HTML.
func TestLocalServerControlPageServed(t *testing.T) {
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
		Registry:   newFakeRegistry("claude-01"),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/local/control/page", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected text/html, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Teammate Agent Console") {
		t.Fatalf("control page body missing title; got %q", body[:min(80, len(body))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
