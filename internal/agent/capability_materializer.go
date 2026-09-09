// capability_materializer.go materializes local config files for skills and
// MCP for different coding tools.
package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/teammate/agentd/internal/agent/tool"
)

type CapabilityInjection struct {
	ToolOptions        tool.ExecuteOptions
	PromptCapabilities PromptCapabilities
	SkillCount         int
	MCPServerCount     int
}

type PromptCapabilities struct {
	IncludeSkills bool
	IncludeMCP    bool
}

// MaterializeAgentCapabilities prepares provider-specific local files for
// skills and MCP servers.
// Claude Code and AtomCode use project-local skill files. Tools with no known
// native skill mechanism keep the prompt fallback enabled. All Teammate-generated
// files are refreshed on every execution, so disabled or removed bindings do
// not leak through stale files.
func MaterializeAgentCapabilities(ctx context.Context, client *Client, cfg *Config, workDir, toolName string) (CapabilityInjection, error) {
	injection := CapabilityInjection{
		PromptCapabilities: PromptCapabilities{IncludeSkills: true, IncludeMCP: true},
	}

	if err := ResetGeneratedCapabilities(workDir); err != nil {
		return injection, fmt.Errorf("reset generated capabilities: %w", err)
	}

	skills, err := client.ListAgentSkills(ctx, cfg.Workspace.ID, cfg.Agent.ID)
	if err != nil {
		return injection, fmt.Errorf("list agent skills: %w", err)
	}
	skills = enabledSkills(skills)
	injection.SkillCount = len(skills)

	mcpPath, mcpServers, err := WriteAgentMCPConfig(ctx, client, cfg, workDir)
	if err != nil {
		return injection, fmt.Errorf("write mcp config: %w", err)
	}
	if mcpPath != "" {
		injection.ToolOptions.MCPConfigPath = mcpPath
	}
	injection.MCPServerCount = len(mcpServers)

	switch toolName {
	case "claude":
		if len(skills) > 0 {
			if err := WriteClaudeSkillFiles(workDir, skills); err != nil {
				return injection, fmt.Errorf("write claude skill files: %w", err)
			}
		}
		injection.PromptCapabilities.IncludeSkills = false
		// Keep non-sensitive MCP names/URLs in the prompt for discoverability; secrets stay in the config file.
		injection.PromptCapabilities.IncludeMCP = true
	case "atomcode":
		if len(skills) > 0 {
			if err := WriteAtomCodeSkillFiles(workDir, skills); err != nil {
				return injection, fmt.Errorf("write atomcode skill files: %w", err)
			}
		}
		injection.PromptCapabilities.IncludeSkills = false
		injection.PromptCapabilities.IncludeMCP = true
	case "mimocode":
		if len(skills) > 0 {
			if err := WriteMiMoCodeSkillFiles(workDir, skills); err != nil {
				return injection, fmt.Errorf("write mimocode skill files: %w", err)
			}
		}
		injection.PromptCapabilities.IncludeSkills = true
		injection.PromptCapabilities.IncludeMCP = true
	default:
		injection.PromptCapabilities.IncludeSkills = true
		injection.PromptCapabilities.IncludeMCP = true
	}

	return injection, nil
}

func enabledSkills(skills []SkillContext) []SkillContext {
	enabled := make([]SkillContext, 0, len(skills))
	for _, skill := range skills {
		if skill.Enabled {
			enabled = append(enabled, skill)
		}
	}
	return enabled
}

// ResetGeneratedCapabilities only removes Teammate-owned files. It intentionally
// preserves user-authored provider config files (e.g. AGENTS.md, .atomcode.md)
// and non-teammate skills.
func ResetGeneratedCapabilities(workDir string) error {
	paths := []string{
		filepath.Join(workDir, ".teammate", "capabilities"),
		filepath.Join(workDir, ".teammate", "mcp.json"),
	}
	for _, path := range paths {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	patterns := []string{
		filepath.Join(workDir, ".claude", "skills", "teammate-*"),
		filepath.Join(workDir, ".atomcode", "skills", "teammate-*"),
		filepath.Join(workDir, ".mimocode", "skills", "teammate-*"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("glob generated capabilities: %w", err)
		}
		for _, match := range matches {
			if err := os.RemoveAll(match); err != nil {
				return fmt.Errorf("remove %s: %w", match, err)
			}
		}
	}
	return nil
}

// WriteClaudeSkillFiles writes project-local Claude Code skill files.
func WriteClaudeSkillFiles(workDir string, skills []SkillContext) error {
	baseDir := filepath.Join(workDir, ".claude", "skills")
	if err := ensureGeneratedCapabilityExclude(workDir); err != nil {
		return err
	}
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return fmt.Errorf("mkdir claude skills dir: %w", err)
	}
	usedNames := map[string]int{}
	for _, skill := range skills {
		name := uniqueMCPName("teammate-"+sanitizeMCPName(skill.Name), usedNames)
		dir := filepath.Join(baseDir, name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir skill dir %s: %w", name, err)
		}
		path := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(path, []byte(formatSkill(skill)), 0644); err != nil {
			return fmt.Errorf("write skill %s: %w", name, err)
		}
	}
	return nil
}

func ensureGeneratedCapabilityExclude(workDir string) error {
	excludePath := filepath.Join(workDir, ".git", "info", "exclude")
	if _, err := os.Stat(filepath.Dir(excludePath)); err != nil {
		return nil
	}
	content := ""
	if data, err := os.ReadFile(excludePath); err == nil {
		content = string(data)
	}
	entries := []string{".teammate/", ".claude/skills/teammate-*/", ".atomcode/skills/", ".mimocode/skills/"}
	changed := false
	for _, entry := range entries {
		if strings.Contains(content, entry) {
			continue
		}
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += entry + "\n"
		changed = true
	}
	if !changed {
		return nil
	}
	if err := os.WriteFile(excludePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write local git exclude: %w", err)
	}
	return nil
}

func formatSkill(skill SkillContext) string {
	var sb strings.Builder
	name := strings.TrimSpace(skill.Name)
	if name == "" {
		name = "Skill"
	}
	description := strings.TrimSpace(skill.Description)
	sb.WriteString("---\n")
	sb.WriteString("name: ")
	sb.WriteString(formatFrontmatterString(name))
	sb.WriteString("\n")
	if description != "" {
		sb.WriteString("description: ")
		sb.WriteString(formatFrontmatterString(description))
		sb.WriteString("\n")
	}
	sb.WriteString("---\n\n")
	sb.WriteString("# ")
	sb.WriteString(name)
	sb.WriteString("\n\n")
	if description != "" {
		sb.WriteString(description)
		sb.WriteString("\n\n")
	}
	if strings.TrimSpace(skill.PromptTemplate) != "" {
		sb.WriteString(strings.TrimSpace(skill.PromptTemplate))
		sb.WriteString("\n")
	}
	return sb.String()
}

func formatFrontmatterString(value string) string {
	return strconv.Quote(value)
}

// WriteAtomCodeSkillFiles writes project-local AtomCode skill files.
// AtomCode loads project skills from .atomcode/skills; Teammate only writes
// teammate-* files there and never modifies the root AGENTS.md or .atomcode.md.
func WriteAtomCodeSkillFiles(workDir string, skills []SkillContext) error {
	if err := ensureGeneratedCapabilityExclude(workDir); err != nil {
		return err
	}

	baseDir := filepath.Join(workDir, ".atomcode", "skills")
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return fmt.Errorf("mkdir atomcode skills dir: %w", err)
	}
	usedNames := map[string]int{}
	for _, skill := range skills {
		name := uniqueMCPName("teammate-"+sanitizeMCPName(skill.Name), usedNames)
		dir := filepath.Join(baseDir, name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir skill dir %s: %w", name, err)
		}
		path := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(path, []byte(formatSkill(skill)), 0644); err != nil {
			return fmt.Errorf("write skill %s: %w", name, err)
		}
	}
	return nil
}

// WriteMiMoCodeSkillFiles writes project-local MiMoCode skill files.
// MiMoCode reads project config from the .mimocode/ directory.
// Skills are written as markdown files into the .mimocode/skills/ directory.
func WriteMiMoCodeSkillFiles(workDir string, skills []SkillContext) error {
	if err := ensureGeneratedCapabilityExclude(workDir); err != nil {
		return err
	}

	baseDir := filepath.Join(workDir, ".mimocode", "skills")
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return fmt.Errorf("mkdir mimocode skills dir: %w", err)
	}
	usedNames := map[string]int{}
	for _, skill := range skills {
		name := uniqueMCPName("teammate-"+sanitizeMCPName(skill.Name), usedNames)
		path := filepath.Join(baseDir, name+".md")
		if err := os.WriteFile(path, []byte(formatSkill(skill)), 0644); err != nil {
			return fmt.Errorf("write skill %s: %w", name, err)
		}
	}
	return nil
}
