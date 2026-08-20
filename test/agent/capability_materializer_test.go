// capability_materializer_test.go 覆盖能力物化逻辑的测试。
package agent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	cfg := &agent.Config{Workspace: agent.WorkspaceConfig{ID: "ws-1"}, Agent: agent.AgentInfo{ID: "agent-1"}}
	injection, err := agent.MaterializeAgentCapabilities(context.Background(), client, cfg, workDir, "claude")
	if err != nil {
		t.Fatalf("MaterializeAgentCapabilities failed: %v", err)
	}
	if injection.SkillCount != 1 {
		t.Fatalf("expected 1 enabled skill, got %d", injection.SkillCount)
	}
	if injection.MCPServerCount != 0 || injection.ToolOptions.MCPConfigPath != "" {
		t.Fatalf("expected no enabled MCP config, got count=%d path=%q", injection.MCPServerCount, injection.ToolOptions.MCPConfigPath)
	}
	if _, err := os.Stat(staleSkill); !os.IsNotExist(err) {
		t.Fatalf("expected stale disabled skill to be removed, err=%v", err)
	}
	if _, err := os.Stat(staleMCP); !os.IsNotExist(err) {
		t.Fatalf("expected stale MCP config to be removed, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "teammate-Enabled-Skill", "SKILL.md")); err != nil {
		t.Fatalf("expected enabled skill file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "teammate-Disabled-Skill", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("expected disabled skill not to be generated, err=%v", err)
	}
}
