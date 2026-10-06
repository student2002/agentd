// local_server_test.go covers tests for the local-mode HTTP server: auth,
// per-(connection, instance) routing, the agents list with its connection
// filter, and the connection management endpoints.
package agent_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// testAgentRoute builds one instance-scoped route under the connection prefix.
func testAgentRoute(conn, name, suffix string) string {
	return "/api/local/connections/" + conn + "/agents/" + name + suffix
}

func newTestLocalServer(registry agent.RuntimeRegistry) *agent.LocalServer {
	return agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
		Registry:   registry,
	})
}

func TestLocalServerSnapshotRequiresToken(t *testing.T) {
	server := newTestLocalServer(newFakeRegistry("team-a", "claude-01"))

	req := httptest.NewRequest(http.MethodGet, testAgentRoute("team-a", "claude-01", "/runtime/snapshot"), nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, testAgentRoute("team-a", "claude-01", "/runtime/snapshot"), nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestLocalServerHealthIsPublic(t *testing.T) {
	server := newTestLocalServer(newFakeRegistry("team-a", "claude-01"))

	req := httptest.NewRequest(http.MethodGet, "/api/local/health", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got struct {
		Status string                 `json:"status"`
		Agents []agent.LocalAgentInfo `json:"agents"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "ok" || len(got.Agents) != 1 || got.Agents[0].Name != "claude-01" {
		t.Fatalf("unexpected health payload: %+v", got)
	}
}

func TestLocalServerRejectsNonLoopbackBindAddr(t *testing.T) {
	server := agent.NewLocalServer(agent.LocalServerConfig{
		BindAddr:   "0.0.0.0:17380",
		LocalToken: "lt_test",
		Registry:   newFakeRegistry("team-a", "claude-01"),
	})

	if err := server.Start(); err == nil {
		t.Fatal("expected non-loopback bind addr to fail")
	}
}

func TestLocalServerRejectsNonGetMethods(t *testing.T) {
	server := newTestLocalServer(newFakeRegistry("team-a", "claude-01"))

	req := httptest.NewRequest(http.MethodPost, testAgentRoute("team-a", "claude-01", "/runtime/snapshot"), nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestLocalServerAllowsLoopbackCorsPreflight(t *testing.T) {
	server := newTestLocalServer(newFakeRegistry("team-a", "claude-01"))

	req := httptest.NewRequest(http.MethodOptions, testAgentRoute("team-a", "claude-01", "/runtime/snapshot"), nil)
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
	server := newTestLocalServer(newFakeRegistry("team-a", "claude-01"))

	req := httptest.NewRequest(http.MethodOptions, testAgentRoute("team-a", "claude-01", "/runtime/snapshot"), nil)
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

// TestLocalServerAgentsListGroupsByConnection proves the flat agent list
// carries the connection dimension and the ?connection= filter scopes it.
func TestLocalServerAgentsListGroupsByConnection(t *testing.T) {
	registry := newFakeRegistry("team-a", "claude-01", "reviewer")
	registry.add("team-b", "claude-01")
	server := newTestLocalServer(registry)

	do := func(path string) []agent.LocalAgentInfo {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer lt_test")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d", path, rec.Code)
		}
		var got struct {
			Agents []agent.LocalAgentInfo `json:"agents"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got.Agents
	}

	all := do("/api/local/agents")
	if len(all) != 3 {
		t.Fatalf("expected 3 (connection, instance) rows, got %+v", all)
	}
	byKey := map[[2]string]agent.LocalAgentInfo{}
	for _, a := range all {
		byKey[[2]string{a.Connection, a.Name}] = a
	}
	if _, ok := byKey[[2]string{"team-a", "claude-01"}]; !ok {
		t.Fatalf("team-a/claude-01 missing: %+v", all)
	}
	if _, ok := byKey[[2]string{"team-b", "claude-01"}]; !ok {
		t.Fatalf("team-b/claude-01 missing: %+v", all)
	}

	scoped := do("/api/local/agents?connection=team-b")
	if len(scoped) != 1 || scoped[0].Connection != "team-b" || scoped[0].Name != "claude-01" {
		t.Fatalf("connection filter did not scope the list: %+v", scoped)
	}
}

// TestLocalServerInstanceRoutesRequireConnection proves the per-instance
// endpoints are addressed by (connection, name): a wrong connection yields 404
// even when the instance name exists on another connection.
func TestLocalServerInstanceRoutesRequireConnection(t *testing.T) {
	registry := newFakeRegistry("team-a", "claude-01")
	registry.add("team-b", "claude-01")
	server := newTestLocalServer(registry)

	req := httptest.NewRequest(http.MethodGet, testAgentRoute("team-a", "claude-01", "/runtime/snapshot"), nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for team-a/claude-01, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, testAgentRoute("team-c", "claude-01", "/runtime/snapshot"), nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown connection, got %d", rec.Code)
	}

	// The legacy connection-less route is gone.
	req = httptest.NewRequest(http.MethodGet, "/api/local/agents/claude-01/runtime/snapshot", nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for the legacy connection-less route, got %d", rec.Code)
	}
}

func TestLocalServerLogsRecentRequiresTokenAndReturnsLines(t *testing.T) {
	registry := newFakeRegistry("team-a", "claude-01")
	server := newTestLocalServer(registry)

	route := testAgentRoute("team-a", "claude-01", "/logs/recent")
	req := httptest.NewRequest(http.MethodGet, route, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	handle, _ := registry.Runtime("team-a", "claude-01")
	handle.Buffer().Append(agent.LogLine{TaskID: 7, NodeID: "n7", Line: "hello"})

	req = httptest.NewRequest(http.MethodGet, route, nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec = httptest.NewRecorder()
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

// TestLocalServerLogsRecentLimit verifies the ?limit= query parameter: unset
// or invalid values keep the historical default of 500, explicit values are
// honored up to the buffer capacity, and oversized values clamp to capacity.
func TestLocalServerLogsRecentLimit(t *testing.T) {
	registry := newFakeRegistry("team-a", "claude-01")
	server := newTestLocalServer(registry)

	handle, _ := registry.Runtime("team-a", "claude-01")
	buf := handle.Buffer()
	if buf.Capacity() != agent.LogBufferDefaultCapacity {
		t.Fatalf("capacity = %d, want %d", buf.Capacity(), agent.LogBufferDefaultCapacity)
	}
	total := agent.LogBufferDefaultCapacity + 500 // exceeds capacity; ring keeps the last 2000
	for i := 0; i < total; i++ {
		buf.Append(agent.LogLine{TaskID: 1, NodeID: "n", Line: fmt.Sprintf("line-%d", i)})
	}

	get := func(query string) int {
		route := testAgentRoute("team-a", "claude-01", "/logs/recent") + query
		req := httptest.NewRequest(http.MethodGet, route, nil)
		req.Header.Set("Authorization", "Bearer lt_test")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", query, rec.Code)
		}
		var got struct {
			Lines []agent.LogLine `json:"lines"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return len(got.Lines)
	}

	if n := get(""); n != 500 {
		t.Fatalf("no limit: got %d lines, want 500", n)
	}
	if n := get("?limit=bogus"); n != 500 {
		t.Fatalf("invalid limit: got %d lines, want 500", n)
	}
	if n := get("?limit=0"); n != 500 {
		t.Fatalf("zero limit: got %d lines, want 500", n)
	}
	if n := get("?limit=600"); n != 600 {
		t.Fatalf("limit=600: got %d lines, want 600", n)
	}
	if n := get("?limit=99999"); n != agent.LogBufferDefaultCapacity {
		t.Fatalf("oversized limit: got %d lines, want %d", n, agent.LogBufferDefaultCapacity)
	}
}

func TestLocalServerPauseRequiresTokenAndWatcher(t *testing.T) {
	server := newTestLocalServer(newFakeRegistry("team-a", "claude-01"))

	// No token → 401
	req := httptest.NewRequest(http.MethodPost, testAgentRoute("team-a", "claude-01", "/control/pause"), nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	// With token but pending watcher → 503 (not 405 — POST is allowed)
	req = httptest.NewRequest(http.MethodPost, testAgentRoute("team-a", "claude-01", "/control/pause"), nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with no watcher, got %d", rec.Code)
	}
}

// TestLocalServerHasNoInstanceManagementEndpoints proves the local creation
// and removal paths are gone: the catalog is server-owned and materialized
// from desired delivery only.
func TestLocalServerHasNoInstanceManagementEndpoints(t *testing.T) {
	server := newTestLocalServer(newFakeRegistry("team-a", "claude-01"))
	auth := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer lt_test")
		return req
	}

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/local/agents/reviewer"},
		{http.MethodDelete, "/api/local/agents/claude-01"},
		{http.MethodPost, testAgentRoute("team-a", "claude-01", "/enable")},
		{http.MethodPost, testAgentRoute("team-a", "claude-01", "/disable")},
		// The explicit enabled-set endpoint is gone: the entry's agents list is
		// the server-delivered full set.
		{http.MethodPut, "/api/local/connections/team-a/agents"},
	} {
		req := auth(httptest.NewRequest(tc.method, tc.path, nil))
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: expected 404, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

// TestLocalServerMemoryEndpoints covers the console's per-instance memory
// management: read, hand-edit and clear, all behind the local token. The
// memory is addressed by instance name only — it is the instance's unified
// memory, not a per-connection view.
func TestLocalServerMemoryEndpoints(t *testing.T) {
	server := agent.NewLocalServer(agent.LocalServerConfig{
		LocalToken: "lt_test",
		Registry:   newFakeRegistry("team-a", "claude-01"),
		Memory:     agent.NewMemoryStore(t.TempDir()),
	})
	auth := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer lt_test")
		return req
	}

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req := auth(httptest.NewRequest(method, path, reader))
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec
	}

	// No token → 401.
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/local/memory/claude-01", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	// GET on a memory-less instance answers empty content.
	rec = do(http.MethodGet, "/api/local/memory/claude-01", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get memory: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var payload struct {
		Instance string `json:"instance"`
		Content  string `json:"content"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode memory: %v", err)
	}
	if payload.Instance != "claude-01" || payload.Content != "" {
		t.Fatalf("unexpected memory payload: %+v", payload)
	}

	// PUT replaces the memory.
	rec = do(http.MethodPut, "/api/local/memory/claude-01", `{"content":"hand-edited memory"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put memory: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodGet, "/api/local/memory/claude-01", "")
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode memory: %v", err)
	}
	if !strings.Contains(payload.Content, "hand-edited memory") {
		t.Fatalf("memory edit not persisted: %+v", payload)
	}

	// DELETE clears it.
	rec = do(http.MethodDelete, "/api/local/memory/claude-01", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete memory: expected 200, got %d", rec.Code)
	}
	rec = do(http.MethodGet, "/api/local/memory/claude-01", "")
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode memory: %v", err)
	}
	if payload.Content != "" {
		t.Fatalf("memory must be cleared, got %q", payload.Content)
	}

	// Other methods → 405.
	rec = do(http.MethodPost, "/api/local/memory/claude-01", "{}")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on POST, got %d", rec.Code)
	}
}

// TestLocalServerMemoryEndpointsUnavailableWithoutStore proves a server
// without a memory store answers 503 instead of touching a default path.
func TestLocalServerMemoryEndpointsUnavailableWithoutStore(t *testing.T) {
	server := newTestLocalServer(newFakeRegistry("team-a", "claude-01"))
	req := httptest.NewRequest(http.MethodGet, "/api/local/memory/claude-01", nil)
	req.Header.Set("Authorization", "Bearer lt_test")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without a memory store, got %d", rec.Code)
	}
}

func TestLocalServerConnections(t *testing.T) {
	registry := newFakeRegistry("team-a", "claude-01")
	server := newTestLocalServer(registry)
	auth := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer lt_test")
		return req
	}

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req := auth(httptest.NewRequest(method, path, reader))
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		return rec
	}

	// GET on an empty registry lists no connections.
	rec := do(http.MethodGet, "/api/local/connections", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list connections: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var list struct {
		Connections []agent.ConnectionInfo `json:"connections"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode connections: %v", err)
	}
	if len(list.Connections) != 0 {
		t.Fatalf("expected no connections, got %+v", list.Connections)
	}

	// POST adds a connection (token + alias). Connections carry no agent
	// selection: instances arrive through desired delivery.
	rec = do(http.MethodPost, "/api/local/connections", `{"name":"team-a","token":"td_new"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add connection: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode added connections: %v", err)
	}
	if len(list.Connections) != 1 || list.Connections[0].Name != "team-a" || list.Connections[0].TokenMasked == "" {
		t.Fatalf("connection not added: %+v", list.Connections)
	}

	// DELETE removes the connection.
	rec = do(http.MethodDelete, "/api/local/connections/team-a", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete connection: expected 200, got %d", rec.Code)
	}
	if got := len(registry.Connections()); got != 0 {
		t.Fatalf("expected connection removed, got %d", got)
	}
	rec = do(http.MethodDelete, "/api/local/connections/team-a", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown connection: expected 404, got %d", rec.Code)
	}

	// POST with a non-td token → 400.
	rec = do(http.MethodPost, "/api/local/connections", `{"name":"bad","token":"nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid token: expected 400, got %d", rec.Code)
	}

	// Duplicate name → 409.
	rec = do(http.MethodPost, "/api/local/connections", `{"name":"team-a","token":"td_x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add team-a: expected 200, got %d", rec.Code)
	}
	rec = do(http.MethodPost, "/api/local/connections", `{"name":"team-a","token":"td_y"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate connection: expected 409, got %d", rec.Code)
	}
}
