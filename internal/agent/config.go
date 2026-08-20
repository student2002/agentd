// config.go 提供 Agent Daemon 的配置管理功能。
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 表示守护进程的完整配置。
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Agent     AgentInfo       `yaml:"agent"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Tools     ToolsConfig     `yaml:"tools"`
	Git       GitConfig       `yaml:"git"`
	Local     LocalConfig     `yaml:"local"`
	Debug     bool            `yaml:"debug,omitempty"`
}

// ServerConfig 表示服务器连接配置。
type ServerConfig struct {
	URL      string `yaml:"url"`
	APIToken string `yaml:"api_token"`
}

// AgentInfo 表示代理的基本信息。
type AgentInfo struct {
	ID            string `yaml:"id"`
	Name          string `yaml:"name"`
	ContextWindow int    `yaml:"context_window"`
	Provider      string `yaml:"provider"`
}

// WorkspaceConfig 表示工作区配置。
type WorkspaceConfig struct {
	ID   string `yaml:"id"`
	Root string `yaml:"root"`
}

// ToolsConfig 表示编码工具配置。
type ToolsConfig struct {
	Claude   ToolConfig `yaml:"claude"`
	OpenClaw ToolConfig `yaml:"openclaw"`
	OpenCode ToolConfig `yaml:"opencode"`
	AtomCode ToolConfig `yaml:"atomcode"`
	MiMoCode ToolConfig `yaml:"mimocode"`
}

// ToolConfig 表示单个编码工具的配置。
type ToolConfig struct {
	Path string `yaml:"path"`
}

// GitConfig 表示 Git 相关配置。
type GitConfig struct {
	BaseBranch string `yaml:"base_branch"`
}

// LocalConfig 表示本机控制 API 配置。
type LocalConfig struct {
	Enabled    bool   `yaml:"enabled"`
	BindAddr   string `yaml:"bind_addr"`
	LocalToken string `yaml:"local_token"`
	InstanceID string `yaml:"instance_id"`
}

// DefaultConfigPath 返回默认配置文件路径。
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".teammate/config.yaml"
	}
	return filepath.Join(home, ".teammate", "config.yaml")
}

// LoadConfig 从 YAML 文件加载配置并设置运行时默认值。
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found at %s; run 'teammate-agentd config init' to create one", path)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	cfg := configFromRaw(raw)
	applyConfigDefaults(cfg)
	return cfg, nil
}

func configFromRaw(raw map[string]interface{}) *Config {
	cfg := &Config{}

	server := mapValue(raw, "server")
	cfg.Server.URL = stringValue(server, "url")
	cfg.Server.APIToken = stringValue(server, "api_token")

	agent := mapValue(raw, "agent")
	cfg.Agent.ID = stringValue(agent, "id")
	cfg.Agent.Name = stringValue(agent, "name")
	cfg.Agent.ContextWindow = intValue(agent, "context_window")
	cfg.Agent.Provider = stringValue(agent, "provider")

	workspace := mapValue(raw, "workspace")
	cfg.Workspace.ID = stringValue(workspace, "id")
	cfg.Workspace.Root = stringValue(workspace, "root")

	tools := mapValue(raw, "tools")
	cfg.Tools.Claude = toolConfigFromRaw(mapValue(tools, "claude"))
	cfg.Tools.OpenClaw = toolConfigFromRaw(mapValue(tools, "openclaw"))
	cfg.Tools.OpenCode = toolConfigFromRaw(mapValue(tools, "opencode"))
	cfg.Tools.AtomCode = toolConfigFromRaw(mapValue(tools, "atomcode"))
	cfg.Tools.MiMoCode = toolConfigFromRaw(mapValue(tools, "mimocode"))

	git := mapValue(raw, "git")
	cfg.Git.BaseBranch = stringValue(git, "base_branch")

	local := mapValue(raw, "local")
	cfg.Local.Enabled = boolValue(local, "enabled")
	cfg.Local.BindAddr = stringValue(local, "bind_addr")
	cfg.Local.LocalToken = stringValue(local, "local_token")
	cfg.Local.InstanceID = stringValue(local, "instance_id")

	cfg.Debug = boolValue(raw, "debug")

	return cfg
}

func toolConfigFromRaw(raw map[string]interface{}) ToolConfig {
	return ToolConfig{Path: stringValue(raw, "path")}
}

func applyConfigDefaults(cfg *Config) {
	if cfg.Workspace.Root == "" {
		home, _ := os.UserHomeDir()
		cfg.Workspace.Root = filepath.Join(home, ".teammate", "workspaces")
	} else {
		cfg.Workspace.Root = expandHome(cfg.Workspace.Root)
	}
	if cfg.Tools.Claude.Path == "" {
		cfg.Tools.Claude.Path = "claude"
	}
	if cfg.Tools.OpenClaw.Path == "" {
		cfg.Tools.OpenClaw.Path = "openclaw"
	}
	if cfg.Tools.OpenCode.Path == "" {
		cfg.Tools.OpenCode.Path = "opencode"
	}
	if cfg.Tools.AtomCode.Path == "" {
		cfg.Tools.AtomCode.Path = "atomcode"
	}
	if cfg.Tools.MiMoCode.Path == "" {
		cfg.Tools.MiMoCode.Path = "mimocode"
	}
	cfg.Agent.Provider = strings.ToLower(strings.TrimSpace(cfg.Agent.Provider))
	if cfg.Git.BaseBranch == "" {
		cfg.Git.BaseBranch = "master"
	}
	if cfg.Local.BindAddr == "" {
		cfg.Local.BindAddr = "127.0.0.1:17380"
	}
}

func mapValue(raw map[string]interface{}, key string) map[string]interface{} {
	if raw == nil {
		return nil
	}
	value, ok := raw[key]
	if !ok {
		return nil
	}
	typed, ok := value.(map[string]interface{})
	if !ok {
		return nil
	}
	return typed
}

func stringValue(raw map[string]interface{}, key string) string {
	if raw == nil {
		return ""
	}
	value, ok := raw[key].(string)
	if !ok {
		return ""
	}
	return value
}

func intValue(raw map[string]interface{}, key string) int {
	if raw == nil {
		return 0
	}
	switch value := raw[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func boolValue(raw map[string]interface{}, key string) bool {
	if raw == nil {
		return false
	}
	value, ok := raw[key].(bool)
	return ok && value
}

// ValidateConfig 校验 daemon 启动所需的必填配置。
func ValidateConfig(cfg *Config) error {
	if cfg.Agent.Provider == "" {
		return fmt.Errorf("agent.provider is required; supported providers: claude, openclaw, opencode, atomcode, mimocode")
	}
	if !isValidProvider(cfg.Agent.Provider) {
		return fmt.Errorf("invalid agent provider %q; supported providers: claude, openclaw, opencode, atomcode, mimocode", cfg.Agent.Provider)
	}
	if cfg.Local.Enabled {
		if cfg.Local.LocalToken == "" {
			return fmt.Errorf("local.local_token is required when local.enabled=true")
		}
		if cfg.Local.InstanceID == "" {
			return fmt.Errorf("local.instance_id is required when local.enabled=true")
		}
		if err := ValidateLoopbackBindAddr(cfg.Local.BindAddr); err != nil {
			return err
		}
	}
	return nil
}

// SaveConfig 将配置以稀疏 YAML 写入文件。
func SaveConfig(cfg *Config, path string) error {
	if path == "" {
		path = DefaultConfigPath()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	data, err := MarshalConfigYAML(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// MarshalConfigYAML 将配置转换为不包含无意义空字段的 YAML。
func MarshalConfigYAML(cfg *Config) ([]byte, error) {
	return yaml.Marshal(sparseConfigMap(cfg))
}

func MarshalConfigJSON(cfg *Config) ([]byte, error) {
	return json.MarshalIndent(sparseConfigMap(cfg), "", "  ")
}

func sparseConfigMap(cfg *Config) map[string]interface{} {
	out := make(map[string]interface{})

	server := make(map[string]interface{})
	addString(server, "url", cfg.Server.URL)
	addString(server, "api_token", cfg.Server.APIToken)
	addMap(out, "server", server)

	agent := make(map[string]interface{})
	addString(agent, "id", cfg.Agent.ID)
	addString(agent, "name", cfg.Agent.Name)
	if cfg.Agent.ContextWindow != 0 {
		agent["context_window"] = cfg.Agent.ContextWindow
	}
	addString(agent, "provider", cfg.Agent.Provider)
	addMap(out, "agent", agent)

	workspace := make(map[string]interface{})
	addString(workspace, "id", cfg.Workspace.ID)
	addString(workspace, "root", cfg.Workspace.Root)
	addMap(out, "workspace", workspace)

	tools := make(map[string]interface{})
	addTool(tools, "claude", cfg.Tools.Claude, "claude", true)
	addTool(tools, "openclaw", cfg.Tools.OpenClaw, "openclaw", false)
	addTool(tools, "opencode", cfg.Tools.OpenCode, "opencode", false)
	addTool(tools, "atomcode", cfg.Tools.AtomCode, "atomcode", false)
	addTool(tools, "mimocode", cfg.Tools.MiMoCode, "mimocode", false)
	addMap(out, "tools", tools)

	git := make(map[string]interface{})
	if cfg.Git.BaseBranch != "" && cfg.Git.BaseBranch != "master" {
		git["base_branch"] = cfg.Git.BaseBranch
	}
	addMap(out, "git", git)

	local := make(map[string]interface{})
	if cfg.Local.Enabled {
		local["enabled"] = true
	} else if cfg.Local.LocalToken != "" || cfg.Local.InstanceID != "" {
		local["enabled"] = false
	}
	if cfg.Local.BindAddr != "" && cfg.Local.BindAddr != "127.0.0.1:17380" {
		local["bind_addr"] = cfg.Local.BindAddr
	}
	addString(local, "local_token", cfg.Local.LocalToken)
	addString(local, "instance_id", cfg.Local.InstanceID)
	addMap(out, "local", local)

	if cfg.Debug {
		out["debug"] = cfg.Debug
	}
	return out
}

func addTool(parent map[string]interface{}, name string, tool ToolConfig, defaultPath string, includeDefault bool) {
	if tool.Path == "" {
		return
	}
	if tool.Path == defaultPath && !includeDefault {
		return
	}
	toolMap := make(map[string]interface{})
	addString(toolMap, "path", tool.Path)
	addMap(parent, name, toolMap)
}

func addString(parent map[string]interface{}, key, value string) {
	if value != "" {
		parent[key] = value
	}
}

func addMap(parent map[string]interface{}, key string, value map[string]interface{}) {
	if len(value) > 0 {
		parent[key] = value
	}
}

// expandHome 将路径中的 ~/ 前缀展开为用户实际主目录路径。
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func isValidProvider(provider string) bool {
	switch provider {
	case "claude", "openclaw", "opencode", "atomcode", "mimocode":
		return true
	default:
		return false
	}
}
