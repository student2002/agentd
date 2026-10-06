// capability_materializer_test.go covers tests for the capability materialization logic.
package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func TestWriteClaudeSkillFiles(t *testing.T) {
	workDir := t.TempDir()
	gitInfo := filepath.Join(workDir, ".git", "info")
	if err := os.MkdirAll(gitInfo, 0755); err != nil {
		t.Fatalf("mkdir git info: %v", err)
	}

	err := agent.WriteClaudeSkillFiles(workDir, []agent.SkillContext{
		{Name: "Code Review", Description: "Review carefully", PromptTemplate: "Focus on security."},
		{Name: "Code Review", Description: "Second skill", PromptTemplate: "Focus on tests."},
	})
	if err != nil {
		t.Fatalf("WriteClaudeSkillFiles failed: %v", err)
	}

	firstPath := filepath.Join(workDir, ".claude", "skills", "teammate-Code-Review", "SKILL.md")
	data, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("read first skill: %v", err)
	}
	content := string(data)
	for _, want := range []string{"# Code Review", "Review carefully", "Focus on security."} {
		if !strings.Contains(content, want) {
			t.Fatalf("skill file missing %q: %s", want, content)
		}
	}

	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "teammate-Code-Review-2", "SKILL.md")); err != nil {
		t.Fatalf("expected duplicate skill directory with suffix: %v", err)
	}

	exclude, err := os.ReadFile(filepath.Join(gitInfo, "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if !strings.Contains(string(exclude), ".claude/skills/teammate-*/") {
		t.Fatalf("expected generated Claude skills to be excluded, got %q", string(exclude))
	}
}

func TestWriteAtomCodeSkillFilesDoesNotMutateRootInstructions(t *testing.T) {
	workDir := t.TempDir()
	gitInfo := filepath.Join(workDir, ".git", "info")
	if err := os.MkdirAll(gitInfo, 0755); err != nil {
		t.Fatalf("mkdir git info: %v", err)
	}
	agentsPath := filepath.Join(workDir, "AGENTS.md")
	original := "# Project Instructions\n\nKeep this user-authored file intact.\n"
	if err := os.WriteFile(agentsPath, []byte(original), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	err := agent.WriteAtomCodeSkillFiles(workDir, []agent.SkillContext{
		{Name: "Release: Flow", Description: "Ship safely\nwithout leaking", PromptTemplate: "Run tests.", Enabled: true},
	})
	if err != nil {
		t.Fatalf("WriteAtomCodeSkillFiles failed: %v", err)
	}

	after, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(after) != original {
		t.Fatalf("AGENTS.md was mutated: %q", string(after))
	}

	skillPath := filepath.Join(workDir, ".atomcode", "skills", "teammate-Release-Flow", "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read atomcode skill: %v", err)
	}
	content := string(data)
	for _, want := range []string{"---", `name: "Release: Flow"`, "description: \"Ship safely\\nwithout leaking\"", "Run tests."} {
		if !strings.Contains(content, want) {
			t.Fatalf("atomcode skill missing %q: %s", want, content)
		}
	}
}

func TestResetGeneratedCapabilitiesRemovesOnlyTeammateOwnedFiles(t *testing.T) {
	workDir := t.TempDir()
	paths := []string{
		filepath.Join(workDir, ".teammate", "capabilities", "old.md"),
		filepath.Join(workDir, ".teammate", "mcp.json"),
		filepath.Join(workDir, ".claude", "skills", "teammate-old", "SKILL.md"),
		filepath.Join(workDir, ".atomcode", "skills", "teammate-old", "SKILL.md"),
		filepath.Join(workDir, ".mimocode", "skills", "teammate-old.md"),
		filepath.Join(workDir, ".atomcode", "skills", "user-skill", "SKILL.md"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	if err := agent.ResetGeneratedCapabilities(workDir); err != nil {
		t.Fatalf("ResetGeneratedCapabilities failed: %v", err)
	}

	removed := []string{
		filepath.Join(workDir, ".teammate", "capabilities"),
		filepath.Join(workDir, ".teammate", "mcp.json"),
		filepath.Join(workDir, ".claude", "skills", "teammate-old"),
		filepath.Join(workDir, ".atomcode", "skills", "teammate-old"),
		filepath.Join(workDir, ".mimocode", "skills", "teammate-old.md"),
	}
	for _, path := range removed {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed, err=%v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workDir, ".atomcode", "skills", "user-skill", "SKILL.md")); err != nil {
		t.Fatalf("expected user skill to remain: %v", err)
	}
}

func TestMaterializeAgentCapabilitiesFiltersDisabledAndClearsStaleFiles(t *testing.T) {
	workDir := t.TempDir()
	staleSkill := filepath.Join(workDir, ".claude", "skills", "teammate-disabled", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(staleSkill), 0755); err != nil {
		t.Fatalf("mkdir stale skill: %v", err)
	}
	if err := os.WriteFile(staleSkill, []byte("stale"), 0644); err != nil {
		t.Fatalf("write stale skill: %v", err)
	}
	staleMCP := filepath.Join(workDir, ".teammate", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(staleMCP), 0755); err != nil {
		t.Fatalf("mkdir stale mcp: %v", err)
	}
	if err := os.WriteFile(staleMCP, []byte("{}"), 0600); err != nil {
		t.Fatalf("write stale mcp: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/workspaces/ws-1/agents/agent-1/skills", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"skill-1","name":"Enabled Skill","description":"use it","prompt_template":"enabled prompt","enabled":true},
			{"id":"skill-2","name":"Disabled Skill","description":"ignore it","prompt_template":"disabled prompt","enabled":false}
		]`))
	})
	mux.HandleFunc("/api/workspaces/ws-1/agents/agent-1/execution/mcp-servers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"mcp-1","name":"Off","url":"https://mcp.example.test","enabled":false}]`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := agent.NewClient(server.URL, "token")
	cfg := &agent.Config{}
	cfg.Agent.Name = "claude-01"
	run := agent.RunContext{Client: client, ConnName: "team-a", AgentID: "agent-1", WorkspaceID: "ws-1"}
	injection, err := agent.MaterializeAgentCapabilities(context.Background(), run, cfg, workDir, "claude")
	if err != nil {
		t.Fatalf("MaterializeAgentCapabilities failed: %v", err)
	}
	if injection.SkillCount != 1 {
		t.Fatalf("expected 1 enabled skill, got %d", injection.SkillCount)
	}
	// No server-bound MCP servers are enabled, but the teammate memory server
	// is always written so the tool can reach the instance memory.
	if injection.MCPServerCount != 0 {
		t.Fatalf("expected no server-bound MCP servers, got count=%d", injection.MCPServerCount)
	}
	if _, err := os.Stat(staleSkill); !os.IsNotExist(err) {
		t.Fatalf("expected stale disabled skill to be removed, err=%v", err)
	}
	// The stale MCP config was replaced by the memory-server config: the
	// disabled server binding must not leak through.
	rewritten, err := os.ReadFile(filepath.Join(workDir, ".teammate", "mcp.json"))
	if err != nil {
		t.Fatalf("expected the memory MCP config to exist: %v", err)
	}
	if strings.Contains(string(rewritten), "Off") {
		t.Fatalf("stale disabled MCP server leaked:\n%s", rewritten)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "teammate-Enabled-Skill", "SKILL.md")); err != nil {
		t.Fatalf("expected enabled skill file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "teammate-Disabled-Skill", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("expected disabled skill not to be generated, err=%v", err)
	}
}

// TestMaterializeAgentCapabilitiesInjectsMemoryServer proves the agentd
// self-entry in the workDir MCP config: a stdio server launching
// `teammate-agentd mcp` with the execution context and connection credentials
// in its environment.
func TestMaterializeAgentCapabilitiesInjectsMemoryServer(t *testing.T) {
	workDir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workspaces/ws-1/agents/agent-uuid-a1/skills", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/workspaces/ws-1/agents/agent-uuid-a1/execution/mcp-servers", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := agent.NewClient(server.URL, "td_secret_token")
	cfg := &agent.Config{}
	cfg.Agent.Name = "claude-01"
	cfg.Agent.PersonaKey = "pk-claude-01"
	run := agent.RunContext{Client: client, ConnName: "team-a", AgentID: "agent-uuid-a1", WorkspaceID: "ws-1"}

	injection, err := agent.MaterializeAgentCapabilities(context.Background(), run, cfg, workDir, "claude")
	if err != nil {
		t.Fatalf("MaterializeAgentCapabilities failed: %v", err)
	}
	if injection.ToolOptions.MCPConfigPath == "" {
		t.Fatal("expected the MCP config path to be reported")
	}

	data, err := os.ReadFile(filepath.Join(workDir, ".teammate", "mcp.json"))
	if err != nil {
		t.Fatalf("read mcp config: %v", err)
	}
	var config struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("unmarshal mcp config: %v\n%s", err, data)
	}
	entry, ok := config.MCPServers["teammate-memory"]
	if !ok {
		t.Fatalf("teammate-memory entry missing:\n%s", data)
	}
	if entry.Type != "stdio" || entry.Command == "" || len(entry.Args) == 0 || entry.Args[0] != "mcp" {
		t.Fatalf("unexpected memory server entry: %+v", entry)
	}
	wantEnv := map[string]string{
		agent.MCPCtxInstance:   "claude-01",
		agent.MCPCtxPersona:    "pk-claude-01",
		agent.MCPCtxConnection: "team-a",
		agent.MCPCtxWorkspaceID: "ws-1",
		agent.MCPCtxAgentID:    "agent-uuid-a1",
		agent.MCPCtxServerURL:  server.URL,
		agent.MCPCtxToken:      "td_secret_token",
	}
	for key, want := range wantEnv {
		if entry.Env[key] != want {
			t.Fatalf("env %s = %q, want %q", key, entry.Env[key], want)
		}
	}

	// The config file carries the connection token: on Unix it must be
	// 0600. Windows does not honor Unix permission bits, so the check is
	// Unix-only.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(workDir, ".teammate", "mcp.json"))
		if err != nil {
			t.Fatalf("stat mcp config: %v", err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("mcp config must be 0600, got %v", got)
		}
	}
}
