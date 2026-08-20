// capability_materializer.go 为不同编码工具物化生成技能与 MCP 的本地配置文件。
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

// MaterializeAgentCapabilities 为技能和 MCP 服务器准备特定提供方的本地文件。
// Claude Code 和 AtomCode 使用项目本地的技能文件。没有已知原生技能机制的
// 工具保持 prompt 回退启用。所有 Teammate 生成的文件在每次执行时都会刷新，
// 因此被禁用或移除的绑定不会通过过期文件泄漏。
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
		// 在 prompt 中保留非敏感的 MCP 名称/URL 以便发现；密钥保留在配置文件中。
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

// ResetGeneratedCapabilities 只删除 Teammate 拥有的文件。它有意保留
// 用户自行编写的提供方配置文件（如 AGENTS.md、.atomcode.md）以及非 teammate 技能。
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

// WriteClaudeSkillFiles 写入项目本地的 Claude Code 技能文件。
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

// WriteAtomCodeSkillFiles 写入项目本地的 AtomCode 技能文件。
// AtomCode 从 .atomcode/skills 加载项目技能；Teammate 只在那里写入 teammate-* 文件，
// 绝不修改根目录的 AGENTS.md 或 .atomcode.md。
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

// WriteMiMoCodeSkillFiles 写入项目本地的 MiMoCode 技能文件。
// MiMoCode 从 .mimocode/ 目录读取项目配置。
// 技能以 markdown 文件形式写入 .mimocode/skills/ 目录。
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
