// mcp_server_test.go covers the local MCP memory server: the JSON-RPC
// handshake, the four memory tools, and the workspace-memory proxy through
// the connection credentials injected via environment variables.
package agent_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// mcpSession drives one MCP server instance over in-memory pipes.
type mcpSession struct {
	t        *testing.T
	store    *agent.MemoryStore
	root     string
	client   *bufio.Reader
	server   *bufio.Writer
	requestW *io.PipeWriter
	serverW  *io.PipeWriter
	done     chan error
}

func newMCPSession(t *testing.T, serverURL, token string) *mcpSession {
	t.Helper()
	root := t.TempDir()
	store := agent.NewMemoryStore(root)
	return newMCPSessionWithStore(t, root, store, serverURL, token)
}

func newMCPSessionWithStore(t *testing.T, root string, store *agent.MemoryStore, serverURL, token string) *mcpSession {
	t.Helper()
	pr, pw := io.Pipe()
	cr, cw := io.Pipe()
	s := &mcpSession{
		t:        t,
		store:    store,
		root:     root,
		client:   bufio.NewReader(cr),
		server:   bufio.NewWriter(pw),
		requestW: pw,
		serverW:  cw,
		done:     make(chan error, 1),
	}
	go func() {
		s.done <- agent.RunMCPServer(agent.MCPServerConfig{
			InstanceName: "claude-01",
			PersonaKey:   "pk-claude-01",
			Connection:   "team-a",
			WorkspaceID:  "ws-a",
			AgentID:      "agent-uuid-a1",
			ServerURL:    serverURL,
			Token:        token,
			MemoryRoot:   root,
			In:           pr,
			Out:          bufio.NewWriter(cw),
			Log:          io.Discard,
		})
	}()
	t.Cleanup(func() {
		// Close both pipe ends the test owns so the server's reads and
		// writes unblock and RunMCPServer returns.
		s.requestW.Close()
		s.serverW.Close()
		<-s.done
	})
	return s
}

func (s *mcpSession) send(request map[string]interface{}) {
	s.t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		s.t.Fatalf("marshal request: %v", err)
	}
	if _, err := s.server.Write(append(data, '\n')); err != nil {
		s.t.Fatalf("write request: %v", err)
	}
	s.server.Flush()
}

func (s *mcpSession) recv() map[string]interface{} {
	s.t.Helper()
	line, err := s.client.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("read response: %v", err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(line, &resp); err != nil {
		s.t.Fatalf("unmarshal response %q: %v", line, err)
	}
	return resp
}

func (s *mcpSession) call(name string, arguments map[string]interface{}) map[string]interface{} {
	s.t.Helper()
	s.send(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      name,
			"arguments": arguments,
		},
	})
	return s.recv()
}

// callIsError reports the isError flag of a tools/call response.
func callIsError(response map[string]interface{}) (bool, bool) {
	result, ok := response["result"].(map[string]interface{})
	if !ok {
		return false, false
	}
	isError, ok := result["isError"].(bool)
	return isError, ok
}

// toolText extracts the concatenated text content of a tools/call response.
func toolText(t *testing.T, response map[string]interface{}) string {
	t.Helper()
	result, ok := response["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("response carries no result: %v", response)
	}
	content, ok := result["content"].([]interface{})
	if !ok {
		t.Fatalf("result carries no content: %v", result)
	}
	var sb strings.Builder
	for _, item := range content {
		part, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if text, ok := part["text"].(string); ok {
			sb.WriteString(text)
		}
	}
	return sb.String()
}

func TestMCPServerInitializeAndListTools(t *testing.T) {
	s := newMCPSession(t, "", "")

	s.send(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]interface{}{}})
	resp := s.recv()
	if resp["id"].(float64) != 1 {
		t.Fatalf("unexpected response id: %v", resp)
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("initialize returned no result: %v", resp)
	}
	info, _ := result["serverInfo"].(map[string]interface{})
	if info == nil || info["name"] != "teammate-memory" {
		t.Fatalf("unexpected server info: %v", result)
	}

	// Notifications carry no id and produce no response.
	s.send(map[string]interface{}{"jsonrpc": "2.0", "method": "notifications/initialized"})

	s.send(map[string]interface{}{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	resp = s.recv()
	result, ok = resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("tools/list returned no result: %v", resp)
	}
	tools, _ := result["tools"].([]interface{})
	names := map[string]bool{}
	for _, item := range tools {
		tool, _ := item.(map[string]interface{})
		if tool == nil {
			continue
		}
		if name, _ := tool["name"].(string); name != "" {
			names[name] = true
		}
	}
	for _, want := range []string{"search_instance_memory", "read_instance_memory", "write_instance_memory", "search_workspace_memory"} {
		if !names[want] {
			t.Fatalf("tool %q missing from tools/list: %v", want, names)
		}
	}
}

// TestMCPServerMemoryLifecycle proves the cross-workspace property with one
// store: entries written through one session's context are read back through
// another session bound to the same instance name but a different connection.
func TestMCPServerMemoryLifecycle(t *testing.T) {
	root := t.TempDir()
	store := agent.NewMemoryStore(root)
	s1 := newMCPSessionWithStore(t, root, store, "", "")
	s2 := newMCPSessionWithStore(t, root, store, "", "")

	result := s1.call("write_instance_memory", map[string]interface{}{"content": "the deploy pipeline needs pnpm"})
	if isError, _ := callIsError(result); isError {
		t.Fatalf("write failed: %v", result)
	}

	// Read from the second session (same instance name): unified storage.
	read := toolText(t, s2.call("read_instance_memory", map[string]interface{}{}))
	if !strings.Contains(read, "the deploy pipeline needs pnpm") {
		t.Fatalf("memory not visible across sessions of the same instance: %q", read)
	}
	// The entry carries the writing connection's provenance.
	if !strings.Contains(read, "team-a") {
		t.Fatalf("entry missing provenance: %q", read)
	}

	hits := toolText(t, s2.call("search_instance_memory", map[string]interface{}{"keyword": "pnpm"}))
	if !strings.Contains(hits, "the deploy pipeline needs pnpm") {
		t.Fatalf("search did not find the entry: %q", hits)
	}
	miss := toolText(t, s2.call("search_instance_memory", map[string]interface{}{"keyword": "docker"}))
	if strings.Contains(miss, "the deploy pipeline needs pnpm") {
		t.Fatalf("search returned an unrelated entry: %q", miss)
	}

	// The persisted store on disk matches the MCP view (keyed by persona).
	disk, err := store.Read("pk-claude-01")
	if err != nil {
		t.Fatalf("disk read: %v", err)
	}
	if !strings.Contains(disk, "the deploy pipeline needs pnpm") {
		t.Fatalf("disk memory missing entry:\n%s", disk)
	}
}

func TestMCPServerWriteRejectsEmptyContent(t *testing.T) {
	s := newMCPSession(t, "", "")

	result := s.call("write_instance_memory", map[string]interface{}{"content": "   "})
	if isError, _ := callIsError(result); !isError {
		t.Fatalf("empty content must be rejected: %v", result)
	}
}

// TestMCPServerRequiresPersonaKey proves the memory identity is mandatory:
// the memory archive is keyed by persona, never by instance name.
func TestMCPServerRequiresPersonaKey(t *testing.T) {
	err := agent.RunMCPServer(agent.MCPServerConfig{
		InstanceName: "claude-01",
		MemoryRoot:   t.TempDir(),
		In:           strings.NewReader(""),
		Log:          io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), agent.MCPCtxPersona) {
		t.Fatalf("expected %s requirement error, got %v", agent.MCPCtxPersona, err)
	}
}

func TestMCPServerWorkspaceMemoryProxy(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("q")
		gotAuth = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"m-1","title":"Release flow","content":"tag before merge","score":0.9}]`))
	}))
	defer server.Close()

	s := newMCPSession(t, server.URL, "td_team_a")
	text := toolText(t, s.call("search_workspace_memory", map[string]interface{}{"keyword": "release"}))

	if !strings.Contains(text, "Release flow") || !strings.Contains(text, "tag before merge") {
		t.Fatalf("workspace memory not proxied: %q", text)
	}
	if gotPath != "/api/memories/search" {
		t.Fatalf("unexpected proxy path: %q", gotPath)
	}
	if gotQuery != "release" {
		t.Fatalf("keyword not forwarded: %q", gotQuery)
	}
	if gotAuth != "td_team_a" {
		t.Fatalf("connection credentials not used, auth=%q", gotAuth)
	}
}

func TestMCPServerWorkspaceMemoryWithoutCredentials(t *testing.T) {
	s := newMCPSession(t, "", "")
	result := s.call("search_workspace_memory", map[string]interface{}{"keyword": "anything"})
	if isError, _ := callIsError(result); !isError {
		t.Fatalf("workspace search without credentials must surface an error: %v", result)
	}
}

// TestMemoryEnvNames pins the environment contract between the executor's
// MCP config injection and the `teammate-agentd mcp` subcommand.
func TestMemoryEnvNames(t *testing.T) {
	for _, env := range []string{agent.MCPCtxInstance, agent.MCPCtxConnection, agent.MCPCtxWorkspaceID, agent.MCPCtxAgentID, agent.MCPCtxServerURL, agent.MCPCtxToken} {
		if !strings.HasPrefix(env, "TEAMMATE_MCP_") {
			t.Fatalf("unexpected env name %q", env)
		}
	}
	if agent.MCPCtxInstance != "TEAMMATE_MCP_INSTANCE" || agent.MCPCtxToken != "TEAMMATE_MCP_TOKEN" {
		t.Fatalf("env contract changed: %q / %q", agent.MCPCtxInstance, agent.MCPCtxToken)
	}
}
