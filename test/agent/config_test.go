// config_test.go covers tests for the daemon GlobalConfig: loading the
// per-workspace instance schema, defaults, validation, and persistence.
package agent_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func writeGlobalConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// validGlobalConfigYAML is the instance-per-workspace schema: each connection
// entry carries its own materialized agent objects (name/provider/agent_id).
func validGlobalConfigYAML() string {
	return strings.Join([]string{
		"server:",
		"  url: http://127.0.0.1:8080",
		"name: dev-machine",
		"workspaces:",
		"  - name: team-a",
		"    token: td_test_token",
		"    agents:",
		"      - name: claude-01",
		"        provider: claude",
		"        persona_key: shared.brain-1",
		"        agent_id: agent-uuid-a1",
		"      - name: reviewer",
		"        provider: claude",
		"  - name: team-b",
		"    token: td_other_token",
		"    agents:",
		"      - name: claude-01",
		"        provider: atomcode",
		"        agent_id: agent-uuid-b1",
		"",
	}, "\n")
}

func TestLoadGlobalConfigParsesWorkspaceAgents(t *testing.T) {
	cfg, err := agent.LoadGlobalConfig(writeGlobalConfig(t, validGlobalConfigYAML()))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Workspaces) != 2 {
		t.Fatalf("expected 2 workspaces, got %d", len(cfg.Workspaces))
	}
	teamA := cfg.Workspace("team-a")
	if len(teamA.Agents) != 2 {
		t.Fatalf("expected 2 agents on team-a, got %+v", teamA.Agents)
	}
	first := teamA.Agent("claude-01")
	if first == nil || first.Provider != "claude" || first.AgentID != "agent-uuid-a1" {
		t.Fatalf("claude-01 not parsed: %+v", first)
	}
	if first.PersonaKey != "shared.brain-1" {
		t.Fatalf("persona_key not parsed: %+v", first)
	}
	second := teamA.Agent("reviewer")
	if second == nil || second.AgentID != "" {
		t.Fatalf("reviewer must parse with an empty agent_id (materialized, not yet bound): %+v", second)
	}
	teamB := cfg.Workspace("team-b")
	bAgent := teamB.Agent("claude-01")
	if bAgent == nil || bAgent.Provider != "atomcode" || bAgent.AgentID != "agent-uuid-b1" {
		t.Fatalf("team-b claude-01 not parsed: %+v", bAgent)
	}
	// The same instance name may exist on several connections: uniqueness is
	// only required within one connection.
	if teamB.Agent("reviewer") != nil {
		t.Fatal("reviewer must not leak onto team-b")
	}
}

// TestLoadGlobalConfigIgnoresLegacyAgentShapes proves the legacy string-list
// agents entries and the identities map are no longer parsed: no backward
// compatibility, affected instances re-arrive through desired delivery.
func TestLoadGlobalConfigIgnoresLegacyAgentShapes(t *testing.T) {
	path := writeGlobalConfig(t, strings.Join([]string{
		"server:",
		"  url: http://127.0.0.1:8080",
		"workspaces:",
		"  - name: team-a",
		"    token: td_test",
		"    agents: [claude-01, reviewer]",
		"    identities:",
		"      claude-01: agent-uuid-old",
		"",
	}, "\n"))

	cfg, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	entry := cfg.Workspace("team-a")
	if len(entry.Agents) != 0 {
		t.Fatalf("legacy string agents must not parse, got %+v", entry.Agents)
	}
}

func TestDeriveWorkspaceName(t *testing.T) {
	// td_{id8hex}_{40hex}: the alias comes from the first 8 chars of segment 3.
	if got := agent.DeriveWorkspaceName("td_abcd1234_" + strings.Repeat("f", 40)); got != "ws-ffffffff" {
		t.Fatalf("unexpected derived name %q", got)
	}
	if got := agent.DeriveWorkspaceName("td_short"); !strings.HasPrefix(got, "ws-") {
		t.Fatalf("expected fallback derived name, got %q", got)
	}
}

func TestValidateGlobalConfigRequiresWorkspaceAndToken(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	if err := agent.ValidateGlobalConfig(cfg); err == nil || !strings.Contains(err.Error(), "at least one workspace") {
		t.Fatalf("expected workspace validation error, got %v", err)
	}

	cfg.Workspaces = []agent.WorkspaceEntry{{Name: "team-a", Token: "not-a-td-token"}}
	if err := agent.ValidateGlobalConfig(cfg); err == nil || !strings.Contains(err.Error(), "td_") {
		t.Fatalf("expected token prefix validation error, got %v", err)
	}
}

func TestValidateGlobalConfigRejectsDuplicateWorkspaceNames(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Workspaces = []agent.WorkspaceEntry{
		{Name: "team-a", Token: "td_test"},
		{Name: "team-a", Token: "td_test"},
	}
	err := agent.ValidateGlobalConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate workspace name") {
		t.Fatalf("expected duplicate workspace validation error, got %v", err)
	}
}

func TestValidateGlobalConfigRejectsDuplicateAgentNameWithinConnection(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Workspaces = []agent.WorkspaceEntry{{
		Name:  "team-a",
		Token: "td_test",
		Agents: []agent.WorkspaceAgent{
			{Name: "claude-01", Provider: "claude"},
			{Name: "claude-01", Provider: "atomcode"},
		},
	}}
	err := agent.ValidateGlobalConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate agent name") {
		t.Fatalf("expected duplicate agent name error, got %v", err)
	}
}

func TestValidateGlobalConfigRejectsInvalidAgentFields(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Workspaces = []agent.WorkspaceEntry{{
		Name:   "team-a",
		Token:  "td_test",
		Agents: []agent.WorkspaceAgent{{Name: "", Provider: "claude"}},
	}}
	if err := agent.ValidateGlobalConfig(cfg); err == nil || !strings.Contains(err.Error(), "agent name is required") {
		t.Fatalf("expected missing-name error, got %v", err)
	}

	cfg.Workspaces[0].Agents = []agent.WorkspaceAgent{{Name: "a", Provider: ""}}
	if err := agent.ValidateGlobalConfig(cfg); err == nil || !strings.Contains(err.Error(), "invalid provider") {
		t.Fatalf("expected provider validation error, got %v", err)
	}
}

// TestValidateGlobalConfigAllowsEmptyAgentList proves a fresh connection with
// nothing delivered yet validates: instances arrive through desired delivery.
func TestValidateGlobalConfigAllowsEmptyAgentList(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Workspaces = []agent.WorkspaceEntry{{Name: "team-a", Token: "td_test"}}
	if err := agent.ValidateGlobalConfig(cfg); err != nil {
		t.Fatalf("empty agents list must validate: %v", err)
	}
}

// TestSaveGlobalConfigRoundTripsWorkspaceAgents covers the persisted materialized
// set: agents with their agent_id survive a save/load cycle — the agent_id is
// the restart recovery path.
func TestSaveGlobalConfigRoundTripsWorkspaceAgents(t *testing.T) {
	path := writeGlobalConfig(t, validGlobalConfigYAML())
	cfg, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	cfg.Workspaces[0].WorkspaceID = "ws-adopted"
	cfg.Workspaces[0].DaemonID = "dm-adopted"
	if err := agent.SaveGlobalConfig(cfg, path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	reloaded, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	teamA := reloaded.Workspace("team-a")
	if teamA.WorkspaceID != "ws-adopted" || teamA.DaemonID != "dm-adopted" {
		t.Fatalf("expected adopted ids persisted, got %+v", teamA)
	}
	if got := teamA.Agent("claude-01"); got == nil || got.AgentID != "agent-uuid-a1" {
		t.Fatalf("expected agent_id persisted, got %+v", teamA.Agents)
	}
	teamB := reloaded.Workspace("team-b")
	if got := teamB.Agent("claude-01"); got == nil || got.AgentID != "agent-uuid-b1" {
		t.Fatalf("expected team-b agent_id persisted, got %+v", teamB.Agents)
	}

	out, err := agent.MarshalGlobalConfigYAML(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(out, []byte("local:")) {
		t.Fatalf("local defaults should not expand saved YAML:\n%s", string(out))
	}
	if !bytes.Contains(out, []byte("agent_id: agent-uuid-a1")) {
		t.Fatalf("expected agent_id in YAML:\n%s", string(out))
	}
	if !bytes.Contains(out, []byte("persona_key: shared.brain-1")) {
		t.Fatalf("expected persona_key in YAML:\n%s", string(out))
	}
}

// TestSaveGlobalConfigWritesSparseAgentLists proves an empty materialized set
// stays absent on disk.
func TestSaveGlobalConfigWritesSparseAgentLists(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Workspaces = []agent.WorkspaceEntry{{Name: "team-a", Token: "td_test"}}
	out, err := agent.MarshalGlobalConfigYAML(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "agents:") {
		t.Fatalf("empty agents list must not be written:\n%s", string(out))
	}
}

func TestLocalCredentialsAreGeneratedAndPersistedOnFirstStart(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Workspaces = []agent.WorkspaceEntry{{Name: "team-a", Token: "td_test"}}
	cfg.Local.Enabled = true
	cfg.Local.BindAddr = "127.0.0.1:17380"

	// Missing credentials validate: they are generated at startup, not required up front.
	if err := agent.ValidateGlobalConfig(cfg); err != nil {
		t.Fatalf("expected config to validate: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := agent.SaveGlobalConfig(cfg, path); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if err := cfg.EnsureLocalCredentials(path); err != nil {
		t.Fatalf("ensure credentials: %v", err)
	}
	if !strings.HasPrefix(cfg.Local.LocalToken, "lt_") {
		t.Fatalf("local token not generated: %q", cfg.Local.LocalToken)
	}
	if !strings.HasPrefix(cfg.Local.InstanceID, "inst-") {
		t.Fatalf("instance id not generated: %q", cfg.Local.InstanceID)
	}

	reloaded, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.Local.LocalToken != cfg.Local.LocalToken || reloaded.Local.InstanceID != cfg.Local.InstanceID {
		t.Fatalf("generated credentials not persisted: %+v", reloaded.Local)
	}
	if !reloaded.Local.Enabled {
		t.Fatal("local control should stay enabled after reload")
	}
}

func TestValidateGlobalConfigRejectsNonLoopbackLocalBindAddr(t *testing.T) {
	cfg := &agent.GlobalConfig{}
	cfg.Workspaces = []agent.WorkspaceEntry{{Name: "team-a", Token: "td_test"}}
	cfg.Local.Enabled = true
	cfg.Local.BindAddr = "0.0.0.0:17380"
	cfg.Local.LocalToken = "lt_test"
	cfg.Local.InstanceID = "instance-1"

	err := agent.ValidateGlobalConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "local control bind addr must be loopback") {
		t.Fatalf("expected loopback validation error, got %v", err)
	}
}
