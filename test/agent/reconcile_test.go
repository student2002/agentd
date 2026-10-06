// reconcile_test.go covers the desired-agents materialization flow: guarded
// deliveries are skipped without touching the config, passing deliveries are
// materialized onto the owning connection entry (name/provider/agent_id),
// their runtime ensured and identity bound, already-materialized names rebind
// their identity without another config write, and a re-delivered agent_id
// (web delete+recreate) overwrites the persisted one.
package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func newReconciler(t *testing.T, entry agent.WorkspaceEntry) (*agent.Reconciler, *agent.GlobalConfig, *agent.WorkspaceEntry, *reconcileHooks, string) {
	t.Helper()
	cfg := &agent.GlobalConfig{}
	cfg.Server.URL = "http://127.0.0.1:1"
	cfg.Workspaces = []agent.WorkspaceEntry{entry}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := agent.SaveGlobalConfig(cfg, path); err != nil {
		t.Fatalf("save config: %v", err)
	}
	hooks := &reconcileHooks{installed: map[string]bool{"claude": true, "opencode": true}}
	saveFn := func() error {
		return agent.SaveGlobalConfig(cfg, path)
	}
	reconciler := agent.NewReconciler(cfg, &cfg.Workspaces[0], saveFn, agent.ReconcilerHooks{
		ProviderInstalled: hooks.providerInstalled,
		EnsureRuntime:     hooks.ensure,
		ResolveIdentity:   hooks.resolve,
		RemoveRuntime:     hooks.remove,
	})
	return reconciler, cfg, &cfg.Workspaces[0], hooks, path
}

// materializedEntry carries one pre-materialized instance: the state after a
// first delivery round persisted it.
func materializedEntry() agent.WorkspaceEntry {
	return agent.WorkspaceEntry{
		Name:  "team-a",
		Token: "td_test",
		Agents: []agent.WorkspaceAgent{
			{Name: "claude-01", Provider: "claude", AgentID: "uuid-c1", PersonaKey: "persona-old"},
		},
	}
}

type reconcileHooks struct {
	mu        sync.Mutex
	installed map[string]bool
	ensured   []string
	resolved  [][2]string // (name, agentID) pairs
	removed   []string    // names pruned from the local catalog
}

func (h *reconcileHooks) remove(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removed = append(h.removed, name)
}

func (h *reconcileHooks) snapshot() (ensured []string, resolved [][2]string, removed []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.ensured...), append([][2]string{}, h.resolved...), append([]string{}, h.removed...)
}

func (h *reconcileHooks) providerInstalled(provider string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.installed[provider]
}

func (h *reconcileHooks) ensure(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensured = append(h.ensured, name)
}

func (h *reconcileHooks) resolve(name, agentID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resolved = append(h.resolved, [2]string{name, agentID})
}

func TestReconcilerMaterializesDesiredAgent(t *testing.T) {
	reconciler, _, entry, hooks, path := newReconciler(t, materializedEntry())

	reconciler.Reconcile([]agent.DesiredAgent{{AgentID: "uuid-r1", Name: "reviewer", Provider: "opencode", PersonaKey: "persona-r1"}})

	got := entry.Agent("reviewer")
	if got == nil || got.Provider != "opencode" || got.AgentID != "uuid-r1" || got.PersonaKey != "persona-r1" {
		t.Fatalf("delivered agent not materialized on the connection entry: %+v", entry.Agents)
	}
	ensured, resolved, _ := hooks.snapshot()
	if len(ensured) != 1 || ensured[0] != "reviewer" {
		t.Fatalf("expected EnsureRuntime(reviewer), got %v", ensured)
	}
	if len(resolved) != 1 || resolved[0] != [2]string{"reviewer", "uuid-r1"} {
		t.Fatalf("expected ResolveIdentity(reviewer, uuid-r1), got %v", resolved)
	}

	// The YAML on disk must carry the materialized instance on the connection.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	content := string(data)
	for _, want := range []string{"name: reviewer", "provider: opencode", "agent_id: uuid-r1", "persona_key: persona-r1"} {
		if !strings.Contains(content, want) {
			t.Fatalf("delivered agent not persisted (missing %q):\n%s", want, content)
		}
	}
}

// TestReconcilerOverwritesAgentIDOnRedelivery proves the web delete+recreate
// path: the server delivery is authoritative, so a changed agent_id overwrites
// the persisted identity.
func TestReconcilerOverwritesAgentIDOnRedelivery(t *testing.T) {
	reconciler, _, entry, _, path := newReconciler(t, materializedEntry())

	reconciler.Reconcile([]agent.DesiredAgent{{AgentID: "uuid-c2", Name: "claude-01", Provider: "claude", PersonaKey: "persona-new"}})

	got := entry.Agent("claude-01")
	if got == nil || got.AgentID != "uuid-c2" {
		t.Fatalf("expected agent_id overwritten by delivery, got %+v", entry.Agents)
	}
	if got.PersonaKey != "persona-new" {
		t.Fatalf("expected persona_key overwritten by delivery, got %+v", entry.Agents)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "agent_id: uuid-c2") || !strings.Contains(string(data), "persona_key: persona-new") {
		t.Fatalf("overwritten identity not persisted:\n%s", string(data))
	}
}

// TestReconcilerSkipsDeliveryWithoutPersonaKey proves the delivery contract:
// persona_key is the memory identity and must be non-empty; blank deliveries
// (legacy rows) stay unmaterialized.
func TestReconcilerSkipsDeliveryWithoutPersonaKey(t *testing.T) {
	reconciler, _, entry, hooks, _ := newReconciler(t, materializedEntry())

	reconciler.Reconcile([]agent.DesiredAgent{{AgentID: "uuid-p1", Name: "legacy", Provider: "claude"}})

	if entry.Agent("legacy") != nil {
		t.Fatalf("delivery without persona_key materialized: %+v", entry.Agents)
	}
	ensured, _, _ := hooks.snapshot()
	if len(ensured) != 0 {
		t.Fatalf("expected no runtime for persona-less delivery, got %v", ensured)
	}
}

// TestReconcilerPrunesAbsentFromFullSet proves the declarative full-set
// reconciliation: instances present locally but missing from the delivered
// full set (deleted web-side) are removed from the entry, persisted, and
// their runtime is torn down.
func TestReconcilerPrunesAbsentFromFullSet(t *testing.T) {
	reconciler, _, entry, hooks, path := newReconciler(t, agent.WorkspaceEntry{
		Name:  "team-a",
		Token: "td_test",
		Agents: []agent.WorkspaceAgent{
			{Name: "claude-01", Provider: "claude", AgentID: "uuid-c1", PersonaKey: "pk-1"},
			{Name: "doomed", Provider: "claude", AgentID: "uuid-d1", PersonaKey: "pk-doom"},
		},
	})

	reconciler.Reconcile([]agent.DesiredAgent{{AgentID: "uuid-c1", Name: "claude-01", Provider: "claude", PersonaKey: "pk-1"}})

	if entry.Agent("doomed") != nil {
		t.Fatalf("absent instance must be pruned from the entry: %+v", entry.Agents)
	}
	if entry.Agent("claude-01") == nil {
		t.Fatal("present instance must survive")
	}
	_, _, removed := hooks.snapshot()
	if len(removed) != 1 || removed[0] != "doomed" {
		t.Fatalf("expected RemoveRuntime(doomed), got %v", removed)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "doomed") {
		t.Fatalf("pruned instance still persisted:\n%s", string(data))
	}
}

// TestReconcilerRebindsIdentityWithoutConfigWrite proves the restart path:
// for an already-materialized name with the same agent_id the config stays
// untouched but the runtime is ensured and the identity rebound on every
// delivery.
func TestReconcilerRebindsIdentityWithoutConfigWrite(t *testing.T) {
	reconciler, _, entry, hooks, path := newReconciler(t, materializedEntry())

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	beforeAgents := len(entry.Agents)
	reconciler.Reconcile([]agent.DesiredAgent{{AgentID: "uuid-c1", Name: "claude-01", Provider: "claude", PersonaKey: "persona-old"}})
	reconciler.Reconcile([]agent.DesiredAgent{{AgentID: "uuid-c1", Name: "claude-01", Provider: "claude", PersonaKey: "persona-old"}})

	if len(entry.Agents) != beforeAgents {
		t.Fatalf("materialized name written again: %+v", entry.Agents)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read config: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("config rewritten for an unchanged delivery:\n%s", string(after))
	}
	ensured, resolved, _ := hooks.snapshot()
	if len(ensured) != 2 || ensured[0] != "claude-01" || ensured[1] != "claude-01" {
		t.Fatalf("expected EnsureRuntime on every delivery, got %v", ensured)
	}
	if len(resolved) != 2 || resolved[0] != [2]string{"claude-01", "uuid-c1"} {
		t.Fatalf("expected identity rebound on every delivery, got %v", resolved)
	}
}

func TestReconcilerSkipsDeliveryWithoutAgentID(t *testing.T) {
	reconciler, _, entry, hooks, _ := newReconciler(t, materializedEntry())

	reconciler.Reconcile([]agent.DesiredAgent{{Name: "legacy", Provider: "claude"}})

	if entry.Agent("legacy") != nil {
		t.Fatalf("delivery without agent_id materialized: %+v", entry.Agents)
	}
	ensured, _, _ := hooks.snapshot()
	if len(ensured) != 0 {
		t.Fatalf("expected no runtime for agent_id-less delivery, got %v", ensured)
	}
}

func TestReconcilerSkipsInvalidProvider(t *testing.T) {
	reconciler, _, entry, hooks, _ := newReconciler(t, materializedEntry())

	reconciler.Reconcile([]agent.DesiredAgent{{AgentID: "uuid-bad", Name: "bad", Provider: "curl"}})

	if entry.Agent("bad") != nil {
		t.Fatalf("invalid provider materialized: %+v", entry.Agents)
	}
	ensured, _, _ := hooks.snapshot()
	if len(ensured) != 0 {
		t.Fatalf("expected no runtime for invalid provider, got %v", ensured)
	}
}

func TestReconcilerSkipsUninstalledProvider(t *testing.T) {
	reconciler, _, entry, hooks, _ := newReconciler(t, materializedEntry())
	hooks.mu.Lock()
	hooks.installed["opencode"] = false
	hooks.mu.Unlock()

	reconciler.Reconcile([]agent.DesiredAgent{{AgentID: "uuid-r1", Name: "reviewer", Provider: "opencode"}})

	if entry.Agent("reviewer") != nil {
		t.Fatalf("uninstalled provider materialized: %+v", entry.Agents)
	}
	ensured, _, _ := hooks.snapshot()
	if len(ensured) != 0 {
		t.Fatalf("expected no runtime for uninstalled provider, got %v", ensured)
	}
}
