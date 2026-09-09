// mcp_config.go generates the MCP server config file and manages its lifecycle.
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

// WriteAgentMCPConfig fetches the agent's enabled MCP bindings and writes them to
// the local tool config file.
// The file is written with 0600 permissions because the environment variables
// may contain credentials.
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
	// Add .teammate/mcp.json to git exclude to avoid committing it to the repo (best-effort, non-blocking)
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

// appendToGitExclude adds the specified pattern to the repository's
// .git/info/exclude file.
// If the pattern already exists, it is not added again.
// If the .git/info/exclude file does not exist or cannot be written, it is
// silently ignored — this is a best-effort operation, and failure should not
// block MCP config generation.
//
// Parameters:
//   - workDir: the work directory (repository root)
//   - pattern: the file pattern to exclude (e.g. ".teammate/mcp.json")
func appendToGitExclude(workDir, pattern string) error {
	gitDir := filepath.Join(workDir, ".git")
	infoDir := filepath.Join(gitDir, "info")
	excludeFile := filepath.Join(infoDir, "exclude")

	// .git may be a worktree file (content "gitdir: ..."), not a directory
	gitStat, err := os.Stat(gitDir)
	if err != nil {
		return nil // not a Git repository, silently ignore
	}
	if !gitStat.IsDir() {
		return nil // git worktree scenario, info/exclude is not writable
	}
	if _, err := os.Stat(infoDir); os.IsNotExist(err) {
		return nil // info/ does not exist, skip
	}

	data, err := os.ReadFile(excludeFile)
	if err != nil {
		return nil // exclude file not readable, silently ignore
	}

	patternLine := pattern + "\n"
	if strings.Contains(string(data), patternLine) {
		return nil // already exists, do not add again
	}

	f, err := os.OpenFile(excludeFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil // not writable, silently ignore
	}
	defer f.Close()

	if _, err := f.WriteString(patternLine); err != nil {
		return nil // write failed, silently ignore
	}
	return nil
}
