// local_server_test.go 覆盖本地模式 HTTP 服务端的测试。
package agent_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func TestLocalServerSnapshotRequiresToken(t *testing.T) {
	state := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	hub := agent.NewLocalEventHub()
	server := agent.NewLocalServer(agent.LocalServerConfig{
		BindAddr:   "127.0.0.1:0",
		LocalToken: "lt_test",
	}, state, hub)

	req := httptest.NewRequest(http.MethodGet, "/api/local/runtime/snapshot", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/local/runtime/snapshot", nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestLocalServerHealthIsPublic(t *testing.T) {
	state := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	hub := agent.NewLocalEventHub()
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
	}, state, hub)

	req := httptest.NewRequest(http.MethodGet, "/api/local/health", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestLocalServerRejectsNonLoopbackBindAddr(t *testing.T) {
	state := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	hub := agent.NewLocalEventHub()
	server := agent.NewLocalServer(agent.LocalServerConfig{
		BindAddr:   "0.0.0.0:17380",
		LocalToken: "lt_test",
	}, state, hub)

	if err := server.Start(); err == nil {
		t.Fatal("expected non-loopback bind addr to fail")
	}
}

func TestLocalServerRejectsNonGetMethods(t *testing.T) {
	state := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	hub := agent.NewLocalEventHub()
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
	}, state, hub)

	req := httptest.NewRequest(http.MethodPost, "/api/local/runtime/snapshot", nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestLocalServerAllowsLoopbackCorsPreflight(t *testing.T) {
	state := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	hub := agent.NewLocalEventHub()
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
	}, state, hub)

	req := httptest.NewRequest(http.MethodOptions, "/api/local/runtime/snapshot", nil)
	req.Header.Set("Origin", "http://127.0.0.1:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "Authorization")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:3000" {
		t.Fatalf("unexpected allow origin: %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Fatal("expected allow headers")
	}
}

func TestLocalServerRejectsNonLoopbackCorsOrigin(t *testing.T) {
	state := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	hub := agent.NewLocalEventHub()
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
	}, state, hub)

	req := httptest.NewRequest(http.MethodOptions, "/api/local/runtime/snapshot", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected no CORS allow origin, got %q", got)
	}
}

func TestLocalServerLogsRecentRequiresToken(t *testing.T) {
	state := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	hub := agent.NewLocalEventHub()
	lb := agent.NewLogBuffer(10)
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
		LogBuffer:  lb,
	}, state, hub)

	req := httptest.NewRequest(http.MethodGet, "/api/local/logs/recent", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestLocalServerLogsRecentReturnsLines(t *testing.T) {
	state := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	hub := agent.NewLocalEventHub()
	lb := agent.NewLogBuffer(10)
	lb.Append(agent.LogLine{TaskID: 7, NodeID: "n7", Line: "hello"})
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
		LogBuffer:  lb,
	}, state, hub)

	req := httptest.NewRequest(http.MethodGet, "/api/local/logs/recent", nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var got struct {
		Lines []agent.LogLine `json:"lines"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Lines) != 1 || got.Lines[0].Line != "hello" {
		t.Fatalf("unexpected lines: %+v", got.Lines)
	}
}

func TestLocalServerLogsRecentEmptyWithoutBuffer(t *testing.T) {
	state := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	hub := agent.NewLocalEventHub()
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
	}, state, hub)

	req := httptest.NewRequest(http.MethodGet, "/api/local/logs/recent", nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var got struct {
		Lines []agent.LogLine `json:"lines"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Lines) != 0 {
		t.Fatalf("expected empty lines, got %d", len(got.Lines))
	}
}

func TestLocalServerPauseRequiresTokenAndAcceptsPost(t *testing.T) {
	state := agent.NewLocalStateStore(agent.LocalStateConfig{InstanceID: "instance-1"})
	hub := agent.NewLocalEventHub()
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
	}, state, hub)

	// No token → 401
	req := httptest.NewRequest(http.MethodPost, "/api/local/control/pause", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	// With token but no watcher wired → 503 (not 405 — POST is allowed)
	req = httptest.NewRequest(http.MethodPost, "/api/local/control/pause", nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with no watcher, got %d", rec.Code)
	}
}
