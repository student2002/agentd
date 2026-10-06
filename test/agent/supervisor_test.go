// supervisor_test.go covers the supervisor's connection management and a
// dual-workspace end-to-end smoke run against a fake teammate server: two
// connections (two tokens, two SSE streams, two pending nodes) each walk
// register → desired delivery → identity binding → watcher claim → tool
// execute → complete, all per-agent calls carrying the X-Agent-ID of that
// workspace's agent UUID. The same instance name on both connections gets two
// independent runtimes and may execute in both workspaces at the same time.
// Heartbeats must report exactly each connection's delivered set, delivered
// identities must persist for restart recovery, and a connection removal must
// leave the sibling working.
package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teammate/agentd/internal/agent"
	"github.com/teammate/agentd/internal/agent/tool"
)

// fakeWorkspace is one workspace served by the fake teammate server.
type fakeWorkspace struct {
	alias       string
	token       string
	workspaceID string
	daemonID    string
	// uuids maps instance names to this workspace's agent UUIDs (server side).
	uuids map[string]string
	// desired overrides the derived desired set: nil = deliver every uuid
	// (fresh pending rows); an empty map = nothing pending (the incremental
	// state once every row is online).
	desired map[string]string
	// rejectClaimer makes the claim endpoint answer 409 for these agent UUIDs,
	// pinning which instance wins a node when several could (the optimistic
	// lock is the server's; this only narrows the race deterministically).
	rejectClaimer map[string]bool

	mu        sync.Mutex
	pending   map[int32]bool   // taskID → claimable
	claims    map[int32]string // taskID → X-Agent-ID that claimed
	completes map[int32]string // taskID → X-Agent-ID that completed
	busy      [][]string       // agent name sets received at heartbeat
	events    chan string
}

func newFakeWorkspace(alias, token, workspaceID, daemonID string, uuids map[string]string, pending ...int32) *fakeWorkspace {
	ws := &fakeWorkspace{
		alias:       alias,
		token:       token,
		workspaceID: workspaceID,
		daemonID:    daemonID,
		uuids:       uuids,
		pending:     map[int32]bool{},
		claims:      map[int32]string{},
		completes:   map[int32]string{},
		events:      make(chan string, 8),
	}
	for _, id := range pending {
		ws.pending[id] = true
	}
	return ws
}

func (w *fakeWorkspace) pushEvent(t *testing.T, taskID int32, agentUUID string) {
	t.Helper()
	select {
	case w.events <- fmt.Sprintf("event: node:pending\ndata: {\"agent_id\":%q,\"task_id\":%d}\n\n", agentUUID, taskID):
	default:
		t.Logf("workspace %s event channel full; watcher polling will pick the node up", w.alias)
	}
}

// fakeTeammate implements just enough of the server API for the daemon smoke
// run, serving one http.Workspace per configured entry. The workspace is
// resolved from the X-API-Key daemon token (or the session token exchanged
// from it).
type fakeTeammate struct {
	mu         sync.Mutex
	workspaces []*fakeWorkspace
	sessions   map[string]string // session token → daemon token
	sessionSeq int
	dereg      map[string]bool // workspaceID → deregistered
}

func newFakeTeammate(workspaces ...*fakeWorkspace) *fakeTeammate {
	return &fakeTeammate{workspaces: workspaces, sessions: map[string]string{}, dereg: map[string]bool{}}
}

func (f *fakeTeammate) byToken(token string) *fakeWorkspace {
	f.mu.Lock()
	if mapped, ok := f.sessions[token]; ok {
		token = mapped
	}
	f.mu.Unlock()
	for _, ws := range f.workspaces {
		if ws.token == token {
			return ws
		}
	}
	return nil
}

func (f *fakeTeammate) byWorkspaceID(id string) *fakeWorkspace {
	for _, ws := range f.workspaces {
		if ws.workspaceID == id {
			return ws
		}
	}
	return nil
}

func (f *fakeTeammate) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/daemons/register", func(w http.ResponseWriter, r *http.Request) {
		ws := f.byToken(r.Header.Get("X-API-Key"))
		if ws == nil {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		// The registration report carries machine information only.
		if _, hasAgents := body["agents"]; hasAgents {
			t.Errorf("register report must not carry an agents field: %s", body["agents"])
		}
		ws.mu.Lock()
		desiredSet := ws.desired
		if desiredSet == nil {
			desiredSet = ws.uuids
		}
		ws.mu.Unlock()
		desired := make([]map[string]string, 0, len(desiredSet))
		for name, id := range desiredSet {
			desired = append(desired, map[string]string{"agent_id": id, "name": name, "provider": "claude", "persona_key": "pk-" + name})
		}
		writeJSON(w, map[string]interface{}{
			"daemon_id":          ws.daemonID,
			"workspace_id":       ws.workspaceID,
			"desired_agents":     desired,
			"heartbeat_interval": 1,
		})
	})
	mux.HandleFunc("/api/daemons/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		ws := f.byToken(r.Header.Get("X-API-Key"))
		if ws == nil {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		var body struct {
			Agents []struct {
				Name string `json:"name"`
				Busy bool   `json:"busy"`
			} `json:"agents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		names := make([]string, 0, len(body.Agents))
		for _, a := range body.Agents {
			names = append(names, a.Name)
		}
		ws.mu.Lock()
		ws.busy = append(ws.busy, names)
		// Declarative full set: every instance of the daemon is delivered on
		// every heartbeat, independent of the reported status.
		desiredSet := ws.desired
		if desiredSet == nil {
			desiredSet = ws.uuids
		}
		ws.mu.Unlock()
		desired := make([]map[string]string, 0, len(desiredSet))
		for name, id := range desiredSet {
			desired = append(desired, map[string]string{"agent_id": id, "name": name, "provider": "claude", "persona_key": "pk-" + name})
		}
		writeJSON(w, map[string]interface{}{
			"desired_agents":     desired,
			"heartbeat_interval": 1,
		})
	})
	mux.HandleFunc("/api/daemons/deregister", func(w http.ResponseWriter, r *http.Request) {
		ws := f.byToken(r.Header.Get("X-API-Key"))
		if ws == nil {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.dereg[ws.workspaceID] = true
		f.mu.Unlock()
		writeJSON(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/api/auth/token-exchange", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			APIToken string `json:"api_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		ws := f.byToken(body.APIToken)
		if ws == nil {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		// Each exchange mints a distinct session token so concurrent
		// connections never share a principal.
		f.mu.Lock()
		f.sessionSeq++
		session := fmt.Sprintf("st_%s_%d", ws.workspaceID, f.sessionSeq)
		f.sessions[session] = ws.token
		f.mu.Unlock()
		writeJSON(w, map[string]interface{}{
			"session_token": session,
			"expires_at":    time.Now().Add(24 * time.Hour),
		})
	})

	// Daemon SSE stream per workspace: replays pushed node:pending events.
	mux.HandleFunc("/api/workspaces/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		// /api/workspaces/{ws}/...
		if len(parts) < 4 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		ws := f.byWorkspaceID(parts[3])
		if ws == nil {
			http.Error(w, "unknown workspace", http.StatusNotFound)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/daemons/"+ws.daemonID+"/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": connected\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case data := <-ws.events:
					fmt.Fprint(w, data)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case strings.HasSuffix(r.URL.Path, "/projects"):
			writeJSON(w, []map[string]interface{}{{"id": "p1", "name": "proj-" + ws.alias}})
		case strings.HasSuffix(r.URL.Path, "/projects/p1"):
			writeJSON(w, map[string]interface{}{"id": "p1", "name": "proj-" + ws.alias})
		case strings.HasSuffix(r.URL.Path, "/in-progress-nodes"):
			writeJSON(w, []map[string]interface{}{})
		default:
			writeJSON(w, map[string]interface{}{})
		}
	})
	mux.HandleFunc("/api/projects/p1/board", func(w http.ResponseWriter, r *http.Request) {
		ws := f.byToken(r.Header.Get("X-API-Key"))
		if ws == nil {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		ws.mu.Lock()
		tasks := make([]map[string]interface{}, 0, len(ws.pending))
		for taskID := range ws.pending {
			tasks = append(tasks, map[string]interface{}{"id": taskID})
		}
		ws.mu.Unlock()
		writeJSON(w, map[string]interface{}{
			"columns": []map[string]interface{}{{"key": "pending", "tasks": tasks}},
		})
	})
	mux.HandleFunc("/api/tasks/", func(w http.ResponseWriter, r *http.Request) {
		// /api/tasks/{id}/... — node listing, claim and complete per workspace.
		parts := strings.Split(r.URL.Path, "/")
		var taskID int32
		fmt.Sscanf(parts[3], "%d", &taskID)
		ws := f.byToken(r.Header.Get("X-API-Key"))
		if ws == nil {
			http.Error(w, "unknown token", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/nodes") && r.Method == http.MethodGet:
			ws.mu.Lock()
			claimable := ws.pending[taskID]
			ws.mu.Unlock()
			if !claimable {
				writeJSON(w, []map[string]interface{}{})
				return
			}
			writeJSON(w, []map[string]interface{}{
				{"id": nodeID(taskID), "task_id": taskID, "name": "1. code", "node_type": "standard", "status": "pending", "assignee_type": "agent", "sort_order": 1},
			})
		case strings.HasSuffix(r.URL.Path, "/claim"):
			ws.mu.Lock()
			if ws.rejectClaimer[r.Header.Get("X-Agent-ID")] {
				ws.mu.Unlock()
				http.Error(w, "node reserved", http.StatusConflict)
				return
			}
			if !ws.pending[taskID] {
				ws.mu.Unlock()
				// Optimistic lock, mirroring the server: only one claimer wins.
				http.Error(w, "node already claimed", http.StatusConflict)
				return
			}
			ws.pending[taskID] = false
			ws.claims[taskID] = r.Header.Get("X-Agent-ID")
			ws.mu.Unlock()
			writeJSON(w, map[string]interface{}{
				"id": nodeID(taskID), "task_id": taskID, "name": "1. code", "node_type": "standard", "status": "in_progress", "assignee_type": "agent", "sort_order": 1,
			})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			ws.mu.Lock()
			ws.completes[taskID] = r.Header.Get("X-Agent-ID")
			ws.mu.Unlock()
			writeJSON(w, map[string]string{"status": "ok"})
		default:
			writeJSON(w, map[string]string{"status": "ok"})
		}
	})

	// Everything else the executor/context builder touches returns empty.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/tasks/") &&
			(strings.Contains(r.URL.Path, "/messages") ||
				strings.Contains(r.URL.Path, "/token-usage") ||
				strings.Contains(r.URL.Path, "/comments") ||
				strings.Contains(r.URL.Path, "/summary") ||
				strings.Contains(r.URL.Path, "/git-branch")) {
			writeJSON(w, map[string]string{"status": "ok"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/projects/p1/tasks/") {
			var taskID int32
			fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/api/projects/p1/tasks/"), "%d", &taskID)
			writeJSON(w, map[string]interface{}{"id": taskID, "title": fmt.Sprintf("Task %d", taskID), "project_id": "p1"})
			return
		}
		writeJSON(w, map[string]interface{}{})
	})

	return mux
}

func nodeID(taskID int32) string { return fmt.Sprintf("n%d", taskID) }

func writeJSON(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// smokeTool succeeds immediately with a small token usage so the executor
// walks the full completion path.
type smokeTool struct{}

func (smokeTool) Name() string { return "claude" }
func (smokeTool) IsInstalled() bool {
	return true
}
func (smokeTool) Stop() error { return nil }
func (smokeTool) Execute(_ context.Context, _ string, _ string, _ tool.ExecuteOptions, onOutput func(string)) (*tool.ExecutionResult, error) {
	if onOutput != nil {
		onOutput("fake output line")
	}
	return &tool.ExecutionResult{Output: "done", InputTokens: 3, OutputTokens: 4, TotalTokens: 7}, nil
}

// gateTool blocks every Execute call until its gate channel is closed, so
// tests can observe in-flight executions overlapping in time. The started
// counter covers every Execute call, including the summary-generation turns.
type gateTool struct {
	gate    chan struct{}
	started atomic.Int32
}

func (g *gateTool) Name() string      { return "claude" }
func (g *gateTool) IsInstalled() bool { return true }
func (g *gateTool) Stop() error       { return nil }
func (g *gateTool) Execute(_ context.Context, _ string, _ string, _ tool.ExecuteOptions, onOutput func(string)) (*tool.ExecutionResult, error) {
	if onOutput != nil {
		onOutput("blocked execution")
	}
	g.started.Add(1)
	<-g.gate
	return &tool.ExecutionResult{Output: "done", InputTokens: 3, OutputTokens: 4, TotalTokens: 7}, nil
}

// TestSupervisorDualWorkspaceParallelExecution runs one daemon against two
// workspaces that both delivered an instance named claude-01: each
// (connection, instance) pair gets its own runtime, and the two claude-01
// executions — one per workspace — must run at the same time instead of
// queueing behind each other. Identities stay per-workspace, heartbeats
// report each connection's delivered set only, and removing one connection
// leaves the other fully working.
func TestSupervisorDualWorkspaceParallelExecution(t *testing.T) {
	teamA := newFakeWorkspace("team-a", "td_team_a", "ws-a", "dm-a",
		map[string]string{"claude-01": "agent-uuid-a1", "reviewer": "agent-uuid-a2"}, 1)
	// Pin task 1 to claude-01 so the same-name parallel execution across the
	// two workspaces is observable deterministically.
	teamA.rejectClaimer = map[string]bool{"agent-uuid-a2": true}
	teamB := newFakeWorkspace("team-b", "td_team_b", "ws-b", "dm-b",
		map[string]string{"claude-01": "agent-uuid-b1"}, 2)
	fake := newFakeTeammate(teamA, teamB)
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()

	path := writeDualWorkspaceConfig(t, server.URL)
	cfg, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	gate := make(chan struct{})
	blocked := &gateTool{gate: gate}
	var releaseGate sync.Once
	release := func() { releaseGate.Do(func() { close(gate) }) }
	sup := agent.NewSupervisor(cfg, path, agent.SupervisorOptions{
		ToolFactory: func(provider, path string) tool.Tool { return blocked },
	})
	// Guarantee teardown on every exit path (including t.Fatalf): Run leaks
	// the SSE streams otherwise and the test server never closes.
	defer func() {
		release()
		sup.Stop()
	}()

	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(context.Background()) }()

	waitFor := func(desc string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s", desc)
	}

	// Both workspaces deliver claude-01; both watchers claim their own node
	// and both executions must be in flight at the same time — the two
	// runtimes do not queue behind each other.
	teamB.pushEvent(t, 2, "agent-uuid-b1")
	waitFor("both claude-01 executions in flight", func() bool {
		return blocked.started.Load() >= 2
	})
	teamA.mu.Lock()
	aClaim := teamA.claims[1]
	teamA.mu.Unlock()
	teamB.mu.Lock()
	bClaim := teamB.claims[2]
	teamB.mu.Unlock()
	if aClaim != "agent-uuid-a1" {
		t.Fatalf("team-a claude-01 must claim with its own UUID, got %q", aClaim)
	}
	if bClaim != "agent-uuid-b1" {
		t.Fatalf("team-b claude-01 must claim with its own UUID, got %q", bClaim)
	}

	// The instance list exposes one row per (connection, instance).
	agents := sup.Agents()
	byKey := map[[2]string]agent.LocalAgentInfo{}
	for _, a := range agents {
		byKey[[2]string{a.Connection, a.Name}] = a
	}
	if len(agents) != 3 {
		t.Fatalf("expected 3 (connection, instance) rows, got %+v", agents)
	}
	if byKey[[2]string{"team-a", "claude-01"}].Status != "busy" || byKey[[2]string{"team-b", "claude-01"}].Status != "busy" {
		t.Fatalf("both claude-01 rows must be busy in parallel: %+v", agents)
	}

	// Release the executions and wait for both completions with consistent
	// identities.
	release()
	waitFor("team-a task 1 complete", func() bool {
		teamA.mu.Lock()
		defer teamA.mu.Unlock()
		return teamA.completes[1] == "agent-uuid-a1"
	})
	waitFor("team-b task 2 complete", func() bool {
		teamB.mu.Lock()
		defer teamB.mu.Unlock()
		return teamB.completes[2] == "agent-uuid-b1"
	})

	// Each connection's heartbeat busy list is exactly its delivered set:
	// team-a carries claude-01+reviewer, team-b only claude-01 — the local
	// catalog never leaks undelivered names into a heartbeat.
	waitFor("team-a heartbeat busy set", func() bool {
		teamA.mu.Lock()
		defer teamA.mu.Unlock()
		for _, names := range teamA.busy {
			if nameSetEqual(names, []string{"claude-01", "reviewer"}) {
				return true
			}
		}
		return false
	})
	waitFor("team-b heartbeat busy set", func() bool {
		teamB.mu.Lock()
		defer teamB.mu.Unlock()
		for _, names := range teamB.busy {
			if nameSetEqual(names, []string{"claude-01"}) {
				return true
			}
		}
		return false
	})

	// The adopted workspace/daemon ids and the delivered identities (the
	// restart recovery path) must persist to the connection entries.
	reloaded, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	entryA, entryB := reloaded.Workspace("team-a"), reloaded.Workspace("team-b")
	if entryA == nil || entryA.WorkspaceID != "ws-a" || entryA.DaemonID != "dm-a" {
		t.Fatalf("team-a ids not adopted: %+v", entryA)
	}
	if entryB == nil || entryB.WorkspaceID != "ws-b" || entryB.DaemonID != "dm-b" {
		t.Fatalf("team-b ids not adopted: %+v", entryB)
	}
	if got := entryA.Agent("claude-01"); got == nil || got.AgentID != "agent-uuid-a1" || got.PersonaKey != "pk-claude-01" {
		t.Fatalf("team-a claude-01 identity not persisted: %+v", entryA.Agents)
	}
	if got := entryA.Agent("reviewer"); got == nil || got.AgentID != "agent-uuid-a2" || got.PersonaKey != "pk-reviewer" {
		t.Fatalf("team-a reviewer identity not persisted: %+v", entryA.Agents)
	}
	if got := entryB.Agent("claude-01"); got == nil || got.AgentID != "agent-uuid-b1" || got.PersonaKey != "pk-claude-01" {
		t.Fatalf("team-b claude-01 identity not persisted: %+v", entryB.Agents)
	}

	// Removing team-a must not disturb team-b: its runtime survives, team-a's
	// runtimes disappear, and a new pending node for team-b is still claimed
	// through the surviving connection.
	if err := sup.RemoveConnection("team-a"); err != nil {
		t.Fatalf("remove connection: %v", err)
	}
	if _, ok := sup.Runtime("team-a", "claude-01"); ok {
		t.Fatal("team-a/claude-01 runtime must be gone after connection removal")
	}
	if _, ok := sup.Runtime("team-b", "claude-01"); !ok {
		t.Fatal("team-b/claude-01 runtime must survive team-a removal")
	}
	reloaded, err = agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("reload after removal: %v", err)
	}
	if reloaded.Workspace("team-a") != nil {
		t.Fatal("team-a entry still present after removal")
	}
	if reloaded.Workspace("team-b") == nil {
		t.Fatal("team-b entry disappeared after removing team-a")
	}

	teamB.mu.Lock()
	teamB.pending[3] = true
	teamB.mu.Unlock()
	teamB.pushEvent(t, 3, "agent-uuid-b1")
	waitFor("team-b task 3 claim after team-a removal", func() bool {
		teamB.mu.Lock()
		defer teamB.mu.Unlock()
		return teamB.claims[3] == "agent-uuid-b1"
	})
	waitFor("team-b task 3 complete", func() bool {
		teamB.mu.Lock()
		defer teamB.mu.Unlock()
		return teamB.completes[3] == "agent-uuid-b1"
	})

	// Server-side deletion reconciles through the full set: drop reviewer
	// from team-a's declared set... (team-a was removed above, so exercise
	// the mechanism on team-b): remove claude-01 from team-b's uuids and
	// wait — the next heartbeat delivers a set without it, agentd prunes the
	// local entry and the runtime disappears.
	teamB.mu.Lock()
	teamB.uuids = map[string]string{}
	teamB.desired = map[string]string{}
	teamB.mu.Unlock()
	waitFor("team-b claude-01 pruned after server-side deletion", func() bool {
		_, ok := sup.Runtime("team-b", "claude-01")
		return !ok
	})
	reloaded, err = agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("reload after prune: %v", err)
	}
	if entryB := reloaded.Workspace("team-b"); entryB == nil || entryB.Agent("claude-01") != nil {
		t.Fatalf("pruned instance still materialized: %+v", entryB.Agents)
	}

	// Graceful shutdown: Stop must deregister the surviving daemon.
	sup.Stop()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
	fake.mu.Lock()
	deregB := fake.dereg["ws-b"]
	fake.mu.Unlock()
	if !deregB {
		t.Fatal("expected team-b deregister on shutdown")
	}
}

func nameSetEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	set := map[string]bool{}
	for _, name := range got {
		set[name] = true
	}
	for _, name := range want {
		if !set[name] {
			return false
		}
	}
	return true
}

// writeDualWorkspaceConfig writes a fresh machine: both connections start with
// empty materialized sets; desired delivery fills them in.
func writeDualWorkspaceConfig(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := strings.Join([]string{
		"server:",
		"  url: " + serverURL,
		"workspaces:",
		"  - name: team-a",
		"    token: td_team_a",
		"  - name: team-b",
		"    token: td_team_b",
		// The local control server is on by default; keep this test hermetic
		// (no fixed-port binding).
		"local:",
		"  enabled: false",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestSupervisorAgentsListReflectsRuntimeState(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Server.URL = "http://127.0.0.1:1"
	cfg.Workspaces = []agent.WorkspaceEntry{{
		Name:  "team-a",
		Token: "td_test",
		Agents: []agent.WorkspaceAgent{
			{Name: "claude-01", Provider: "claude"},
			// A dormant entry: materialized but never bound.
			{Name: "paused-one", Provider: "claude"},
		},
	}}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := agent.SaveGlobalConfig(cfg, path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	sup := agent.NewSupervisor(cfg, path, agent.SupervisorOptions{})
	sup.EnsureRuntimeForTest("team-a", "claude-01")
	sup.ResolveIdentityForTest("team-a", "claude-01", "uuid-1", "ws-1")

	agents := sup.Agents()
	if len(agents) != 2 {
		t.Fatalf("expected both connection entries listed, got %+v", agents)
	}
	byName := map[string]agent.LocalAgentInfo{}
	for _, a := range agents {
		if a.Connection != "team-a" {
			t.Fatalf("unexpected connection on row: %+v", a)
		}
		byName[a.Name] = a
	}
	if byName["claude-01"].Status != "online" || byName["claude-01"].AgentID != "uuid-1" {
		t.Fatalf("claude-01 should be online with its identity: %+v", byName["claude-01"])
	}
	if byName["paused-one"].Status != "pending" {
		t.Fatalf("unbound entry must stay pending: %+v", byName["paused-one"])
	}
	if _, ok := sup.Runtime("team-a", "paused-one"); ok {
		t.Fatal("entry without a runtime must not resolve")
	}
}

func TestSupervisorConnectionManagement(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Server.URL = "http://127.0.0.1:1"
	cfg.Local.Enabled = false
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := agent.SaveGlobalConfig(cfg, path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	sup := agent.NewSupervisor(cfg, path, agent.SupervisorOptions{})

	// Add persists the entry and exposes it through Connections().
	if err := sup.AddConnection("team-a", "td_new"); err != nil {
		t.Fatalf("add connection: %v", err)
	}
	reloaded, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if entry := reloaded.Workspace("team-a"); entry == nil || entry.Token != "td_new" {
		t.Fatalf("connection not persisted: %+v", reloaded.Workspaces)
	}
	conns := sup.Connections()
	if len(conns) != 1 || conns[0].Name != "team-a" || conns[0].TokenMasked == "" {
		t.Fatalf("unexpected connections: %+v", conns)
	}

	// Duplicate name is rejected; a non-td token is rejected.
	if err := sup.AddConnection("team-a", "td_other"); err == nil {
		t.Fatal("expected duplicate connection error")
	}
	if err := sup.AddConnection("team-b", "nope"); err == nil {
		t.Fatal("expected invalid token error")
	}

	// Remove deletes the entry; removing an unknown connection fails.
	if err := sup.RemoveConnection("team-a"); err != nil {
		t.Fatalf("remove connection: %v", err)
	}
	reloaded, err = agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("reload after removal: %v", err)
	}
	if reloaded.Workspace("team-a") != nil {
		t.Fatal("team-a entry still present after removal")
	}
	if err := sup.RemoveConnection("team-a"); err == nil {
		t.Fatal("expected unknown connection error")
	}
}

// TestSupervisorRestartRebindsPersistedIdentities proves the restart path:
// the full set still declares the online instance, but the local entry is
// already materialized with the same agent_id/persona — the reconciler makes
// no config change and only rebinds identity, so a fresh process restores the
// watcher from the persisted entry and goes back to claiming with the same
// agent UUID.
func TestSupervisorRestartRebindsPersistedIdentities(t *testing.T) {
	teamA := newFakeWorkspace("team-a", "td_team_a", "ws-a", "dm-a",
		map[string]string{"claude-01": "agent-uuid-a1"}, 1)
	fake := newFakeTeammate(teamA)
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()

	path := writeRestartConfig(t, server.URL)
	cfg, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	sup := agent.NewSupervisor(cfg, path, agent.SupervisorOptions{
		ToolFactory: func(provider, path string) tool.Tool { return smokeTool{} },
	})
	defer sup.Stop()

	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(context.Background()) }()

	// The persisted identity alone must drive claim+complete with the same
	// UUID a fresh delivery would have carried.
	waitFor := func(desc string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s", desc)
	}
	waitFor("team-a task 1 claim+complete", func() bool {
		teamA.mu.Lock()
		defer teamA.mu.Unlock()
		return teamA.claims[1] == "agent-uuid-a1" && teamA.completes[1] == "agent-uuid-a1"
	})

	// The heartbeat busy list is rebuilt from the restored identities.
	waitFor("team-a heartbeat busy set", func() bool {
		teamA.mu.Lock()
		defer teamA.mu.Unlock()
		for _, names := range teamA.busy {
			if nameSetEqual(names, []string{"claude-01"}) {
				return true
			}
		}
		return false
	})

	// The runtime is online again under the persisted identity.
	agents := sup.Agents()
	if len(agents) != 1 || agents[0].Name != "claude-01" || agents[0].Status != "online" {
		t.Fatalf("claude-01 not back online after restart: %+v", agents)
	}
	if agents[0].AgentID != "agent-uuid-a1" {
		t.Fatalf("persisted identity not rebound: %+v", agents[0])
	}

	sup.Stop()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
}

// TestSupervisorSkipsUninstalledProviderAtStartup proves the non-fatal
// startup policy: an instance whose provider is not installed per the probe
// snapshot gets no runtime, while the daemon still starts, materializes
// delivered instances and executes work.
func TestSupervisorSkipsUninstalledProviderAtStartup(t *testing.T) {
	teamA := newFakeWorkspace("team-a", "td_team_a", "ws-a", "dm-a",
		map[string]string{"claude-01": "agent-uuid-a1"}, 1)
	fake := newFakeTeammate(teamA)
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()

	path := writeUninstalledProviderConfig(t, server.URL)
	cfg, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	sup := agent.NewSupervisor(cfg, path, agent.SupervisorOptions{
		ToolFactory: func(provider, path string) tool.Tool {
			if provider == "claude" {
				return smokeTool{}
			}
			return &fakeProbeTool{installed: false}
		},
	})
	defer sup.Stop()

	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(context.Background()) }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		teamA.mu.Lock()
		claimed := teamA.claims[1] != "" && teamA.completes[1] != ""
		teamA.mu.Unlock()
		if claimed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	teamA.mu.Lock()
	claimed := teamA.claims[1] == "agent-uuid-a1" && teamA.completes[1] == "agent-uuid-a1"
	teamA.mu.Unlock()
	if !claimed {
		t.Fatal("timeout waiting for claude-01 claim+complete with the installed provider")
	}

	// The uninstalled-provider entry stays catalogued but gets no runtime.
	if _, ok := sup.Runtime("team-a", "coder"); ok {
		t.Fatal("uninstalled provider must not get a runtime")
	}
	if _, ok := sup.Runtime("team-a", "claude-01"); !ok {
		t.Fatal("installed provider must get a runtime")
	}

	sup.Stop()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
}

// writeRestartConfig writes the machine state after a first run: the instance
// is materialized on the connection entry with its persisted agent_id.
func writeRestartConfig(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := strings.Join([]string{
		"server:",
		"  url: " + serverURL,
		"workspaces:",
		"  - name: team-a",
		"    token: td_team_a",
		"    workspace_id: ws-a",
		"    daemon_id: dm-a",
		"    agents:",
		"      - name: claude-01",
		"        provider: claude",
		"        persona_key: pk-claude-01",
		"        agent_id: agent-uuid-a1",
		"local:",
		"  enabled: false",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// writeUninstalledProviderConfig writes a connection entry mixing an
// installed provider (claude) with an uninstalled one (opencode).
func writeUninstalledProviderConfig(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := strings.Join([]string{
		"server:",
		"  url: " + serverURL,
		"workspaces:",
		"  - name: team-a",
		"    token: td_team_a",
		"    agents:",
		"      - name: claude-01",
		"        provider: claude",
		"        persona_key: pk-claude-01",
		"      - name: coder",
		"        provider: opencode",
		"        persona_key: pk-coder",
		"local:",
		"  enabled: false",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
