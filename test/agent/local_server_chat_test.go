package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
	"github.com/teammate/agentd/internal/agent/tool"
)

// fakeChatTool drives ChatManager without spawning real coding tools.
// blocked != nil keeps Execute waiting until the channel closes or the
// context is cancelled, standing in for a long-running tool process.
type fakeChatTool struct {
	blocked chan struct{}
}

func (f *fakeChatTool) Name() string      { return "fake" }
func (f *fakeChatTool) IsInstalled() bool { return true }
func (f *fakeChatTool) Stop() error       { return nil }

func (f *fakeChatTool) Execute(ctx context.Context, workDir, prompt string, options tool.ExecuteOptions, onOutput func(string)) (*tool.ExecutionResult, error) {
	if onOutput != nil {
		onOutput("line-1: " + prompt)
		onOutput("line-2")
	}
	if f.blocked != nil {
		select {
		case <-f.blocked:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &tool.ExecutionResult{
		Output:       "done",
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  15,
		SessionID:    "sess-1",
	}, nil
}

func newChatTestServer(t *testing.T, fake *fakeChatTool) *agent.LocalServer {
	t.Helper()
	cfg := &agent.Config{}
	cfg.Workspace.Root = t.TempDir()
	manager := agent.NewChatManager(cfg, nil, nil, agent.ChatManagerOptions{
		ToolFactory: func(provider, path string) tool.Tool { return fake },
	})
	return agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
		Chat:       manager,
		Registry:   newFakeRegistry("claude-01"),
	})
}

func chatDo(t *testing.T, server *agent.LocalServer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.Header.Set("Authorization", "Bearer lt_test")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func chatState(t *testing.T, server *agent.LocalServer) agent.ChatState {
	t.Helper()
	rec := chatDo(t, server, http.MethodGet, "/api/local/chat/state", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat state: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var state agent.ChatState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("chat state decode: %v", err)
	}
	return state
}

func waitForChatResult(t *testing.T, server *agent.LocalServer) agent.ChatRunResult {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state := chatState(t, server)
		if state.LastResult != nil && state.Active == nil {
			return *state.LastResult
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("chat run did not finish in time")
	return agent.ChatRunResult{}
}

// TestChatEndpointsRequireToken proves every chat route rejects unauthenticated
// requests and enforces its HTTP method.
func TestChatEndpointsRequireToken(t *testing.T) {
	server := newChatTestServer(t, &fakeChatTool{})

	for _, route := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/local/chat/tools"},
		{http.MethodGet, "/api/local/chat/sessions"},
		{http.MethodGet, "/api/local/chat/state"},
		{http.MethodGet, "/api/local/chat/logs/recent"},
		{http.MethodGet, "/api/local/chat/logs/stream"},
		{http.MethodPost, "/api/local/chat/send"},
		{http.MethodPost, "/api/local/chat/stop"},
		{http.MethodPost, "/api/local/chat/reset"},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 without token, got %d", route.method, route.path, rec.Code)
		}
	}

	// GET on a POST-only route → 405 even with a valid token.
	if rec := chatDo(t, server, http.MethodGet, "/api/local/chat/send", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET send: expected 405, got %d", rec.Code)
	}
}

// TestChatSendLifecycle proves a send streams output lines into the chat
// buffer and surfaces token usage, session id, and session continuity in the
// state endpoint.
func TestChatSendLifecycle(t *testing.T) {
	server := newChatTestServer(t, &fakeChatTool{})

	rec := chatDo(t, server, http.MethodPost, "/api/local/chat/send",
		`{"provider":"atomcode","prompt":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("send: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	result := waitForChatResult(t, server)
	if result.Status != agent.ChatRunCompleted {
		t.Fatalf("expected completed, got %q (error %q)", result.Status, result.Error)
	}
	if result.InputTokens != 10 || result.OutputTokens != 5 || result.SessionID != "sess-1" {
		t.Fatalf("unexpected result metadata: %+v", result)
	}

	state := chatState(t, server)
	if state.Session == nil || state.Session.TurnCount != 1 || state.Session.Provider != "atomcode" {
		t.Fatalf("expected session with turn_count 1 for atomcode, got %+v", state.Session)
	}

	rec = chatDo(t, server, http.MethodGet, "/api/local/chat/logs/recent", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "line-1: hello") {
		t.Fatalf("chat logs missing streamed line: %d %s", rec.Code, rec.Body.String())
	}

	// A finished run frees the slot for the next send.
	rec = chatDo(t, server, http.MethodPost, "/api/local/chat/send",
		`{"provider":"atomcode","prompt":"again"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second send: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	second := waitForChatResult(t, server)
	if second.Status != agent.ChatRunCompleted {
		t.Fatalf("second run: expected completed, got %q", second.Status)
	}
	if state := chatState(t, server); state.Session.TurnCount != 2 {
		t.Fatalf("expected turn_count 2 after second run, got %d", state.Session.TurnCount)
	}

	// Reset drops the session memory.
	if rec := chatDo(t, server, http.MethodPost, "/api/local/chat/reset", ""); rec.Code != http.StatusOK {
		t.Fatalf("reset: expected 200, got %d", rec.Code)
	}
	if state := chatState(t, server); state.Session != nil {
		t.Fatalf("expected nil session after reset, got %+v", state.Session)
	}
}

// TestChatSendValidation proves provider/prompt validation and the busy guard.
func TestChatSendValidation(t *testing.T) {
	server := newChatTestServer(t, &fakeChatTool{})

	if rec := chatDo(t, server, http.MethodPost, "/api/local/chat/send", `{"provider":"nope","prompt":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider: expected 400, got %d", rec.Code)
	}
	if rec := chatDo(t, server, http.MethodPost, "/api/local/chat/send", `{"provider":"claude","prompt":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt: expected 400, got %d", rec.Code)
	}
	if rec := chatDo(t, server, http.MethodPost, "/api/local/chat/send", `{"provider":"claude","prompt":"x","workdir":"Z:/definitely/not/here"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing workdir: expected 400, got %d", rec.Code)
	}

	// Blocked run occupies the single slot.
	blocked := &fakeChatTool{blocked: make(chan struct{})}
	busyServer := newChatTestServer(t, blocked)
	if rec := chatDo(t, busyServer, http.MethodPost, "/api/local/chat/send", `{"provider":"claude","prompt":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("blocked send: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := chatDo(t, busyServer, http.MethodPost, "/api/local/chat/send", `{"provider":"claude","prompt":"y"}`); rec.Code != http.StatusConflict {
		t.Fatalf("busy send: expected 409, got %d", rec.Code)
	}
	close(blocked.blocked)
	waitForChatResult(t, busyServer)
}

// TestChatStop proves stop transitions the active run to "stopped".
func TestChatStop(t *testing.T) {
	blocked := &fakeChatTool{blocked: make(chan struct{})}
	server := newChatTestServer(t, blocked)

	if rec := chatDo(t, server, http.MethodPost, "/api/local/chat/send", `{"provider":"opencode","prompt":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("send: expected 200, got %d", rec.Code)
	}
	if rec := chatDo(t, server, http.MethodPost, "/api/local/chat/stop", ""); rec.Code != http.StatusOK {
		t.Fatalf("stop: expected 200, got %d", rec.Code)
	}

	result := waitForChatResult(t, server)
	if result.Status != agent.ChatRunStopped {
		t.Fatalf("expected stopped, got %q", result.Status)
	}
}

// TestChatToolsList proves the tools endpoint lists providers with install
// state from the injected factory.
func TestChatToolsList(t *testing.T) {
	installed := map[string]bool{"atomcode": true, "claude": false, "openclaw": true, "opencode": true, "mimocode": true}
	cfg := &agent.Config{}
	cfg.Workspace.Root = t.TempDir()
	manager := agent.NewChatManager(cfg, nil, nil, agent.ChatManagerOptions{
		ToolFactory: func(provider, path string) tool.Tool {
			return &stubInstallTool{installed: installed[provider]}
		},
	})
	server := agent.NewLocalServer(agent.LocalServerConfig{LocalToken: "lt_test", Chat: manager, Registry: newFakeRegistry("claude-01")})

	rec := chatDo(t, server, http.MethodGet, "/api/local/chat/tools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("tools: expected 200, got %d", rec.Code)
	}
	var payload struct {
		Tools []agent.ChatToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("tools decode: %v", err)
	}
	if len(payload.Tools) != 5 {
		t.Fatalf("expected 5 tools, got %d", len(payload.Tools))
	}
	byName := map[string]agent.ChatToolInfo{}
	for _, ti := range payload.Tools {
		byName[ti.Name] = ti
	}
	if !byName["atomcode"].Installed || byName["claude"].Installed {
		t.Fatalf("unexpected install states: %+v", payload.Tools)
	}
}

type stubInstallTool struct {
	installed bool
}

func (s *stubInstallTool) Name() string { return "stub" }
func (s *stubInstallTool) IsInstalled() bool {
	return s.installed
}
func (s *stubInstallTool) Stop() error { return nil }
func (s *stubInstallTool) Execute(ctx context.Context, workDir, prompt string, options tool.ExecuteOptions, onOutput func(string)) (*tool.ExecutionResult, error) {
	return &tool.ExecutionResult{}, nil
}

// TestChatEndpointUnavailableWithoutManager proves the chat routes degrade to
// 503 when no ChatManager is configured.
func TestChatEndpointUnavailableWithoutManager(t *testing.T) {
	server := agent.NewLocalServer(agent.LocalServerConfig{LocalToken: "lt_test", Registry: newFakeRegistry("claude-01")})

	if rec := chatDo(t, server, http.MethodGet, "/api/local/chat/tools", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("tools without manager: expected 503, got %d", rec.Code)
	}
	if rec := chatDo(t, server, http.MethodPost, "/api/local/chat/send", `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("send without manager: expected 503, got %d", rec.Code)
	}
}
