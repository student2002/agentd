// registration_sse_test.go covers the daemon registration payload, the
// daemon-level heartbeat behavior, the daemon SSE stream URL, and the
// supervisor's agent_id event routing.
package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
)

func TestRegistrarSubmitsMachineOnlyReport(t *testing.T) {
	var gotBody map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemons/register" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(agent.RegisterDaemonResponse{
			DaemonID: "daemon-1",
			DesiredAgents: []agent.DesiredAgent{
				{AgentID: "agent-uuid-1", Name: "claude-01", Provider: "claude"},
			},
			HeartbeatInterval: 30,
		})
	}))
	defer server.Close()

	registrar := agent.NewRegistrar(agent.NewClient(server.URL, "td_test"), func() agent.RegisterDaemonReport {
		return agent.RegisterDaemonReport{
			DeviceName: "dev-box-01",
			Version:    agent.AgentdVersion,
			PublicKey:  "-----BEGIN PUBLIC KEY-----",
			Providers:  []agent.ProviderInfo{{Provider: "claude", Installed: true}},
		}
	})

	resp, err := registrar.Register(context.Background())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.DaemonID != "daemon-1" {
		t.Fatalf("daemon id = %q", resp.DaemonID)
	}
	if len(resp.DesiredAgents) != 1 || resp.DesiredAgents[0].AgentID != "agent-uuid-1" {
		t.Fatalf("unexpected desired set: %+v", resp.DesiredAgents)
	}
	// The report carries machine information only — no instance catalog.
	if _, hasAgents := gotBody["agents"]; hasAgents {
		t.Fatalf("register report must not carry an agents field: %s", gotBody["agents"])
	}
	if string(gotBody["device_name"]) != `"dev-box-01"` || string(gotBody["providers"]) == "" {
		t.Fatalf("unexpected report body: %s", gotBody)
	}
	if string(gotBody["version"]) != `"`+agent.AgentdVersion+`"` {
		t.Fatalf("report version = %s", gotBody["version"])
	}
}

func TestDaemonHeartbeatSendsBusyAndConsumesDesired(t *testing.T) {
	var mu sync.Mutex
	var lastAgents []agent.AgentBusy
	beats := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemons/heartbeat" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body struct {
			Agents []agent.AgentBusy `json:"agents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		lastAgents = body.Agents
		beats++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"desired_agents":     []map[string]string{{"name": "new-agent", "provider": "claude"}},
			"heartbeat_interval": 7,
		})
	}))
	defer server.Close()

	var desiredMu sync.Mutex
	var desiredCount int
	hb := agent.NewDaemonHeartbeat(agent.NewClient(server.URL, "td_test"), 10*time.Millisecond,
		func() []agent.AgentBusy {
			return []agent.AgentBusy{{Name: "claude-01", Busy: true}}
		},
		func(resp *agent.DaemonHeartbeatResponse) {
			if len(resp.DesiredAgents) > 0 {
				desiredMu.Lock()
				desiredCount++
				desiredMu.Unlock()
			}
		},
		agent.HeartbeatCallbacks{},
	)
	hb.Start()
	defer hb.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		beatsNow := beats
		agents := lastAgents
		mu.Unlock()
		if beatsNow > 0 && len(agents) == 1 && agents[0].Busy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	finalAgents := lastAgents
	mu.Unlock()
	if len(finalAgents) != 1 || !finalAgents[0].Busy || finalAgents[0].Name != "claude-01" {
		t.Fatalf("heartbeat body missing busy status: %+v", finalAgents)
	}
	desiredMu.Lock()
	gotDesired := desiredCount
	desiredMu.Unlock()
	if gotDesired == 0 {
		t.Fatal("expected desired agents callback")
	}
}

func TestDaemonHeartbeatStopsOn404(t *testing.T) {
	var mu sync.Mutex
	beats := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		beats++
		mu.Unlock()
		http.Error(w, "daemon not found", http.StatusNotFound)
	}))
	defer server.Close()

	hb := agent.NewDaemonHeartbeat(agent.NewClient(server.URL, "td_test"), 10*time.Millisecond,
		nil, nil, agent.HeartbeatCallbacks{})
	hb.Start()

	// The loop must stop by itself after the first 404; give it time to prove
	// it is not looping forever.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	final := beats
	mu.Unlock()
	if final == 0 {
		t.Fatal("expected at least one beat")
	}
	if final > 3 {
		t.Fatalf("heartbeat kept running after 404: %d beats", final)
	}
}

func TestDaemonSSEClientUsesDaemonStreamURL(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": connected\n\n"))
		<-r.Context().Done()
	}))
	defer server.Close()

	client := agent.NewSSEClient(server.URL, "ws-1", "daemon-1", func() string { return "td_test" }, func(string, json.RawMessage) {})
	if err := client.Start(); err != nil {
		t.Fatalf("start sse: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(paths)
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	client.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("SSE client never connected")
	}
	want := "/api/workspaces/ws-1/daemons/daemon-1/events"
	if paths[0] != want {
		t.Fatalf("SSE path = %q, want %q", paths[0], want)
	}
}

// TestSupervisorRoutesEventsByAgentID asserts the daemon-stream dispatch
// within one connection's identity namespace: agent_id-addressed events reach
// their (connection, instance) runtime, ids from another workspace are
// dropped, and events without an id broadcast to the connection's
// materialized set.
func TestSupervisorRoutesEventsByAgentID(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Server.URL = "http://127.0.0.1:1"
	cfg.Workspaces = []agent.WorkspaceEntry{
		{
			Name:  "team-a",
			Token: "td_test",
			Agents: []agent.WorkspaceAgent{
				{Name: "claude-01", Provider: "claude", AgentID: "uuid-a1"},
				{Name: "reviewer", Provider: "claude", AgentID: "uuid-a2"},
			},
		},
		{
			Name:  "team-b",
			Token: "td_other",
			Agents: []agent.WorkspaceAgent{
				{Name: "claude-01", Provider: "claude", AgentID: "uuid-b1"},
			},
		},
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := agent.SaveGlobalConfig(cfg, path); err != nil {
		t.Fatalf("save config: %v", err)
	}
	sup := agent.NewSupervisor(cfg, path, agent.SupervisorOptions{})

	sup.EnsureRuntimeForTest("team-a", "claude-01")
	sup.EnsureRuntimeForTest("team-a", "reviewer")
	sup.EnsureRuntimeForTest("team-b", "claude-01")
	sup.ResolveIdentityForTest("team-a", "claude-01", "uuid-a1", "ws-a")
	sup.ResolveIdentityForTest("team-a", "reviewer", "uuid-a2", "ws-a")
	sup.ResolveIdentityForTest("team-b", "claude-01", "uuid-b1", "ws-b")

	if _, ok := sup.Runtime("team-a", "claude-01"); !ok {
		t.Fatal("team-a/claude-01 runtime missing")
	}
	if _, ok := sup.Runtime("team-b", "claude-01"); !ok {
		t.Fatal("team-b/claude-01 runtime missing")
	}
	if _, ok := sup.Runtime("team-a", "reviewer"); !ok {
		t.Fatal("team-a/reviewer runtime missing")
	}

	// node:pending for uuid-a1 routes through team-a's connection only.
	sup.HandleDaemonEventForTest("team-a", "node:pending", json.RawMessage(`{"agent_id":"uuid-a1","task_id":1}`))

	// An agent_id belonging to another workspace's connection is dropped.
	sup.HandleDaemonEventForTest("team-a", "node:pending", json.RawMessage(`{"agent_id":"uuid-b1","task_id":2}`))

	// Unknown agent_id must be dropped without panicking.
	sup.HandleDaemonEventForTest("team-a", "node:pending", json.RawMessage(`{"agent_id":"uuid-unknown"}`))

	// Broadcast event reaches the connection's materialized runtimes
	// (sync:required carries no agent_id).
	sup.HandleDaemonEventForTest("team-a", "sync:required", json.RawMessage(`{"reason":"buffer_expired"}`))

	// The flat list carries one row per (connection, instance) with each
	// row's single identity.
	agents := sup.Agents()
	byKey := map[[2]string]agent.LocalAgentInfo{}
	for _, a := range agents {
		byKey[[2]string{a.Connection, a.Name}] = a
	}
	if got := len(agents); got != 3 {
		t.Fatalf("expected 3 (connection, instance) rows, got %d: %+v", got, agents)
	}
	if byKey[[2]string{"team-a", "claude-01"}].AgentID != "uuid-a1" || byKey[[2]string{"team-b", "claude-01"}].AgentID != "uuid-b1" {
		t.Fatalf("claude-01 identities not per-connection: %+v", agents)
	}
	if byKey[[2]string{"team-a", "claude-01"}].Status != "online" {
		t.Fatalf("team-a/claude-01 not online: %+v", byKey[[2]string{"team-a", "claude-01"}])
	}
	if byKey[[2]string{"team-a", "reviewer"}].AgentID != "uuid-a2" {
		t.Fatalf("reviewer identity missing: %+v", byKey[[2]string{"team-a", "reviewer"}])
	}
}

// TestSupervisorRebuildsRuntimeOnPersonaChange proves the persona-sync path:
// when the entry's persona_key is overwritten server-side (backfill) while a
// runtime already exists, the next delivery rebuilds the runtime so its view
// carries the new memory identity.
func TestSupervisorRebuildsRuntimeOnPersonaChange(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Server.URL = "http://127.0.0.1:1"
	cfg.Local.Enabled = false
	cfg.Workspaces = []agent.WorkspaceEntry{{
		Name:  "team-a",
		Token: "td_test",
		Agents: []agent.WorkspaceAgent{
			{Name: "claude-01", Provider: "claude", AgentID: "uuid-a1", PersonaKey: "persona-old"},
		},
	}}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := agent.SaveGlobalConfig(cfg, path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	sup := agent.NewSupervisor(cfg, path, agent.SupervisorOptions{})
	sup.EnsureRuntimeForTest("team-a", "claude-01")
	before, ok := sup.Runtime("team-a", "claude-01")
	if !ok || before.PersonaKey() != "persona-old" {
		t.Fatalf("runtime should carry the old persona: %+v", before)
	}

	// server-side backfill overwrites the entry persona
	entry := cfg.Workspace("team-a")
	entry.Agent("claude-01").PersonaKey = "persona-new"
	sup.EnsureRuntimeForTest("team-a", "claude-01")

	after, ok := sup.Runtime("team-a", "claude-01")
	if !ok {
		t.Fatal("runtime missing after persona rebuild")
	}
	if after.PersonaKey() != "persona-new" {
		t.Fatalf("rebuilt runtime must carry the new persona, got %q", after.PersonaKey())
	}
}
