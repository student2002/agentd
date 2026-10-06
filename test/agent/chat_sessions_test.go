package agent_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
	"github.com/teammate/agentd/internal/agent/tool"
)

// fakeSessionListerTool satisfies tool.SessionLister on top of the plain fake.
type fakeSessionListerTool struct {
	fakeChatTool
	sessions []tool.ToolSession
}

func (f *fakeSessionListerTool) ListSessions() ([]tool.ToolSession, error) {
	return f.sessions, nil
}

func newChatSessionsServer(t *testing.T, factory func(provider, path string) tool.Tool) *agent.LocalServer {
	t.Helper()
	cfg := &agent.Config{}
	cfg.Workspace.Root = t.TempDir()
	manager := agent.NewChatManager(cfg, nil, nil, agent.ChatManagerOptions{ToolFactory: factory})
	return agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
		Chat:       manager,
		Registry:   newFakeRegistry("claude-01"),
	})
}

func chatSessionsGet(t *testing.T, server *agent.LocalServer, provider string) (int, agent.ChatSessionsResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/local/chat/sessions?provider="+provider, nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	var result agent.ChatSessionsResult
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatalf("sessions decode: %v", err)
		}
	}
	return rec.Code, result
}

// TestChatSessionsEndpoint proves the sessions endpoint surfaces the
// adapter's SessionLister capability and reports unsupported providers.
func TestChatSessionsEndpoint(t *testing.T) {
	server := newChatSessionsServer(t, func(provider, path string) tool.Tool {
		return &fakeSessionListerTool{sessions: []tool.ToolSession{
			{ID: "a", Title: "Alpha", WorkDir: `D:\a`, UpdatedAt: time.UnixMilli(2000), Turns: 2},
			{ID: "b", Title: "Beta", WorkDir: `D:\b`, UpdatedAt: time.UnixMilli(1000), Turns: 1},
		}}
	})

	code, result := chatSessionsGet(t, server, "claude")
	if code != http.StatusOK || !result.Supported || len(result.Sessions) != 2 {
		t.Fatalf("expected supported list of 2, got %d %+v", code, result)
	}
	if result.Sessions[0].ID != "a" || result.Sessions[0].Turns != 2 || result.Sessions[0].Title != "Alpha" {
		t.Fatalf("unexpected session payload: %+v", result.Sessions)
	}

	if _, result := chatSessionsGet(t, server, "nope"); result.Supported || len(result.Sessions) != 0 {
		t.Fatalf("unknown provider must be unsupported, got %+v", result)
	}

	// Adapter without the capability → supported=false.
	plain := newChatSessionsServer(t, func(provider, path string) tool.Tool { return &fakeChatTool{} })
	if _, result := chatSessionsGet(t, plain, "claude"); result.Supported {
		t.Fatalf("plain adapter must be unsupported, got %+v", result)
	}

	// Unauthenticated → 401.
	req := httptest.NewRequest(http.MethodGet, "/api/local/chat/sessions?provider=claude", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// TestChatSendWithSessionID proves session_id flows through the send endpoint.
func TestChatSendWithSessionID(t *testing.T) {
	server := newChatTestServer(t, &fakeChatTool{})
	body := `{"provider":"claude","prompt":"hi","session_id":"sess-xyz"}`
	rec := chatDo(t, server, http.MethodPost, "/api/local/chat/send", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("send with session_id: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestAtomCodeListSessions proves the atomcode session catalog is enumerated
// from $ATOMCODE_HOME/sessions: scheduled sessions skipped, corrupt files
// ignored, newest first.
func TestAtomCodeListSessions(t *testing.T) {
	home := t.TempDir()
	bucket := filepath.Join(home, "sessions", "hash1")
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	metas := map[string]string{
		"id-old.meta":  `{"id":"id-old","name":"old chat","working_dir":"D:\\proj","updated_at":1000,"turn_count":3}`,
		"id-new.meta":  `{"id":"id-new","name":"new chat","working_dir":"D:\\proj2","updated_at":9000,"turn_count":7}`,
		"id-sched.meta": `{"id":"id-sched","name":"s","working_dir":"x","updated_at":9500,"turn_count":1,"origin":"scheduled"}`,
		"bad.meta":     `{not json`,
	}
	for name, body := range metas {
		if err := os.WriteFile(filepath.Join(bucket, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("ATOMCODE_HOME", home)
	got, err := tool.NewAtomCodeTool("atomcode").ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions (scheduled + corrupt skipped), got %d: %+v", len(got), got)
	}
	if got[0].ID != "id-new" || got[1].ID != "id-old" {
		t.Fatalf("expected newest first, got %+v", got)
	}
	if got[0].Title != "new chat" || got[0].Turns != 7 || !strings.Contains(got[0].WorkDir, "proj2") {
		t.Fatalf("unexpected session fields: %+v", got[0])
	}
}

// TestClaudeListSessions proves claude sessions enumerate from
// $CLAUDE_CONFIG_DIR/projects: top-level jsonl only (subagent transcripts
// excluded), the real cwd read from the transcript, the lossily encoded
// folder name kept as fallback.
func TestClaudeListSessions(t *testing.T) {
	cfgRoot := t.TempDir()
	proj := filepath.Join(cfgRoot, "projects", "d--04project-work----------rpc")
	if err := os.MkdirAll(filepath.Join(proj, "sess-1", "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	// First lines without cwd, then a line carrying the real working dir.
	transcript := strings.Join([]string{
		`{"type":"queue-operation","operation":"enqueue"}`,
		`{"type":"user","cwd":"d:\\04project\\work\\港美股券商\\rpc","message":{"content":"q"}}`,
	}, "\n")
	files := map[string]string{
		filepath.Join(proj, "sess-1.jsonl"):                         transcript,
		filepath.Join(proj, "sess-1", "subagents", "agent-x.jsonl"): "{}",
		filepath.Join(proj, "notes.txt"):                            "x",
		filepath.Join(proj, "sess-nocwd.jsonl"):                     `{"type":"user","message":{"content":"q"}}`,
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	got, err := tool.NewClaudeTool("claude").ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 top-level sessions, got %+v", got)
	}
	byID := map[string]tool.ToolSession{}
	for _, s := range got {
		byID[s.ID] = s
	}
	if cwd := byID["sess-1"].WorkDir; cwd != `d:\04project\work\港美股券商\rpc` {
		t.Fatalf("sess-1 cwd: got %q", cwd)
	}
	if fallback := byID["sess-nocwd"].WorkDir; fallback != "d--04project-work----------rpc" {
		t.Fatalf("sess-nocwd fallback: got %q", fallback)
	}
}

// TestAtomCodeReadSession proves the atomcode transcript loader keeps user and
// non-empty assistant messages in order while skipping system, tool, and
// placeholder entries.
func TestAtomCodeReadSession(t *testing.T) {
	home := t.TempDir()
	bucket := filepath.Join(home, "sessions", "hash1")
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot := `{"version":1,"messages":[` +
		`{"role":"System","text":"system prompt"},` +
		`{"role":"User","text":"first question"},` +
		`{"role":"Assistant","text":""},` +
		`{"role":"Tool","text":"tool output"},` +
		`{"role":"Assistant","text":"first answer"},` +
		`{"role":"User","text":"  "},` +
		`{"role":"Assistant","text":"second answer"}` +
		`],"cache_epoch":0}`
	if err := os.WriteFile(filepath.Join(bucket, "sess-a.snapshot"), []byte(snapshot), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ATOMCODE_HOME", home)
	got, err := tool.NewAtomCodeTool("atomcode").ReadSession("sess-a")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []tool.SessionMessage{
		{Role: "user", Text: "first question"},
		{Role: "assistant", Text: "first answer"},
		{Role: "assistant", Text: "second answer"},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d messages, got %+v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("message %d: got %+v want %+v", i, got[i], want[i])
		}
	}

	if _, err := tool.NewAtomCodeTool("atomcode").ReadSession("../escape"); err == nil {
		t.Fatal("path traversal id must be rejected")
	}
	if _, err := tool.NewAtomCodeTool("atomcode").ReadSession("missing"); err == nil {
		t.Fatal("missing session must error")
	}
}

// TestClaudeReadSession proves the claude jsonl transcript loader keeps user
// and assistant text while skipping tool_result blocks and meta lines.
func TestClaudeReadSession(t *testing.T) {
	cfgRoot := t.TempDir()
	proj := filepath.Join(cfgRoot, "projects", "C--proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"user","message":{"content":"hello claude"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi "},{"type":"text","text":"there"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"raw"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`,
		`{"type":"summary","summary":"meta"}`,
		`not json`,
	}
	if err := os.WriteFile(filepath.Join(proj, "sess-b.jsonl"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", cfgRoot)
	got, err := tool.NewClaudeTool("claude").ReadSession("sess-b")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []tool.SessionMessage{
		{Role: "user", Text: "hello claude"},
		{Role: "assistant", Text: "hi \nthere"},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d messages, got %+v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("message %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// TestChatSessionMessagesEndpoint proves the transcript endpoint surfaces the
// adapter's SessionReader capability and degrades gracefully.
func TestChatSessionMessagesEndpoint(t *testing.T) {
	server := newChatSessionsServer(t, func(provider, path string) tool.Tool {
		return &fakeSessionReaderTool{}
	})

	req := httptest.NewRequest(http.MethodGet, "/api/local/chat/session?provider=claude&id=s1", nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var result agent.ChatSessionMessages
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Supported || len(result.Messages) != 2 || result.Messages[0].Role != "user" {
		t.Fatalf("unexpected transcript: %+v", result)
	}

	// Adapter without the capability → supported=false, empty messages.
	plain := newChatSessionsServer(t, func(provider, path string) tool.Tool { return &fakeChatTool{} })
	req = httptest.NewRequest(http.MethodGet, "/api/local/chat/session?provider=claude&id=s1", nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec = httptest.NewRecorder()
	plain.Handler().ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Supported {
		t.Fatalf("plain adapter must be unsupported: %+v err=%v", result, err)
	}
}

type fakeSessionReaderTool struct {
	fakeChatTool
}

func (f *fakeSessionReaderTool) ReadSession(id string) ([]tool.SessionMessage, error) {
	return []tool.SessionMessage{
		{Role: "user", Text: "q " + id},
		{Role: "assistant", Text: "a"},
	}, nil
}
