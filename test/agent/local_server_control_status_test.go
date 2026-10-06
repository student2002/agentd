package agent_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestLocalControlRejectionStatusCodes proves soft-interrupt and intervene map
// "target not held" rejections (idle instance, mismatched task/node) to 409,
// aligned with handback/complete, instead of a blanket 500.
func TestLocalControlRejectionStatusCodes(t *testing.T) {
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
		Registry:   newFakeRegistry("team-a", "claude-01"),
	})

	post := func(route string) int {
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer lt_test")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	base := "/api/local/connections/team-a/agents/claude-01/control"
	if code := post(base + "/soft-interrupt"); code != http.StatusConflict {
		t.Fatalf("idle soft-interrupt: expected 409, got %d", code)
	}
	if code := post(base + "/intervene"); code != http.StatusConflict {
		t.Fatalf("idle intervene: expected 409, got %d", code)
	}
}
