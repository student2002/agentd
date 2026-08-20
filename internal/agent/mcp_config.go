// mcp_config.go 生成 MCP 服务器配置文件并管理其生命周期。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type MCPToolConfig struct {
	MCPServers map[string]MCPServerToolConfig `json:"mcpServers"`
}

type MCPServerToolConfig struct {
	Type string            `json:"type,omitempty"`
	URL  string            `json:"url,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
}

var invalidMCPNameChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// WriteAgentMCPConfig 获取代理已启用的 MCP 绑定并写入本地工具配置文件。
// 文件以 0600 权限写入，因为环境变量可能包含凭据。
func WriteAgentMCPConfig(ctx context.Context, client *Client, cfg *Config, workDir string) (string, []AgentMcpServerContext, error) {
	if client == nil || cfg == nil || cfg.Workspace.ID == "" || cfg.Agent.ID == "" {
		return "", nil, nil
	}

	servers, err := client.ListAgentMcpServers(ctx, cfg.Workspace.ID, cfg.Agent.ID)
	if err != nil {
		return "", nil, err
	}
	config, enabled := BuildMCPToolConfig(servers)
	dir := filepath.Join(workDir, ".teammate")
	path := filepath.Join(dir, "mcp.json")
	if len(config.MCPServers) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", enabled, fmt.Errorf("remove stale mcp config: %w", err)
		}
		return "", enabled, nil
	}

	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", enabled, fmt.Errorf("mkdir mcp config dir: %w", err)
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", enabled, fmt.Errorf("marshal mcp config: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", enabled, fmt.Errorf("write mcp config: %w", err)
	}
	// 将 .teammate/mcp.json 加入 git exclude，避免提交到仓库（best-effort，不阻断）
	_ = appendToGitExclude(workDir, ".teammate/mcp.json")
	return path, enabled, nil
}

func BuildMCPToolConfig(servers []AgentMcpServerContext) (MCPToolConfig, []AgentMcpServerContext) {
	cfg := MCPToolConfig{MCPServers: map[string]MCPServerToolConfig{}}
	enabled := make([]AgentMcpServerContext, 0, len(servers))
	usedNames := map[string]int{}

	for _, server := range servers {
		if !server.Enabled || strings.TrimSpace(server.URL) == "" {
			continue
		}
		enabled = append(enabled, server)
		name := uniqueMCPName(sanitizeMCPName(server.Name), usedNames)
		env := parseMCPEnv(server.EnvVars)
		entry := MCPServerToolConfig{
			Type: inferMCPTransport(server),
			URL:  strings.TrimSpace(server.URL),
			Env:  env,
		}
		cfg.MCPServers[name] = entry
	}
	return cfg, enabled
}

func sanitizeMCPName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "mcp-server"
	}
	name = invalidMCPNameChars.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		return "mcp-server"
	}
	return name
}

func uniqueMCPName(name string, used map[string]int) string {
	count := used[name]
	used[name] = count + 1
	if count == 0 {
		return name
	}
	return fmt.Sprintf("%s-%d", name, count+1)
}

func parseMCPEnv(raw json.RawMessage) map[string]string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err == nil {
		if len(values) == 0 {
			return nil
		}
		return values
	}
	var anyValues map[string]any
	if err := json.Unmarshal(raw, &anyValues); err != nil {
		return nil
	}
	out := make(map[string]string, len(anyValues))
	for key, value := range anyValues {
		switch v := value.(type) {
		case string:
			out[key] = v
		case nil:
			continue
		default:
			out[key] = fmt.Sprint(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func inferMCPTransport(server AgentMcpServerContext) string {
	t := strings.ToLower(strings.TrimSpace(server.Type))
	if t == "sse" || t == "http" || t == "streamable_http" {
		return t
	}
	url := strings.ToLower(strings.TrimSpace(server.URL))
	if strings.Contains(url, "/sse") || strings.HasSuffix(url, "sse") {
		return "sse"
	}
	return "http"
}

// appendToGitExclude 将指定模式添加到仓库的 .git/info/exclude 文件中。
// 如果模式已存在，则不重复添加。
// 如果 .git/info/exclude 文件不存在或无法写入，则静默忽略——这是一个
// best-effort 操作，失败不应阻断 MCP config 生成。
//
// 参数：
//   - workDir: 工作目录（仓库根目录）
//   - pattern: 要排除的文件模式（如 ".teammate/mcp.json"）
func appendToGitExclude(workDir, pattern string) error {
	gitDir := filepath.Join(workDir, ".git")
	infoDir := filepath.Join(gitDir, "info")
	excludeFile := filepath.Join(infoDir, "exclude")

	// .git 可能是 worktree 文件（内容为 "gitdir: ..."），不是目录
	gitStat, err := os.Stat(gitDir)
	if err != nil {
		return nil // 非 Git 仓库，静默忽略
	}
	if !gitStat.IsDir() {
		return nil // git worktree 场景，info/exclude 不可写
	}
	if _, err := os.Stat(infoDir); os.IsNotExist(err) {
		return nil // info/ 不存在，跳过
	}

	data, err := os.ReadFile(excludeFile)
	if err != nil {
		return nil // exclude 文件不可读，静默忽略
	}

	patternLine := pattern + "\n"
	if strings.Contains(string(data), patternLine) {
		return nil // 已存在，不重复添加
	}

	f, err := os.OpenFile(excludeFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil // 无法写入，静默忽略
	}
	defer f.Close()

	if _, err := f.WriteString(patternLine); err != nil {
		return nil // 写入失败，静默忽略
	}
	return nil
}
