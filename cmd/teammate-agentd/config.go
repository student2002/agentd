// config.go 提供 Agent Daemon 的配置加载与解析。
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/teammate/agentd/internal/agent"
	"gopkg.in/yaml.v3"
)

var (
	forceOverwrite    bool
	showConfigSecrets bool
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage agent daemon YAML configuration",
	Long:  "Manage the agent daemon config file read by teammate-agentd. The teammate server does not read this YAML file.",
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create a default agent daemon config file",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := resolvedConfigPath()
		if path == "" {
			path = agent.DefaultConfigPath()
		}

		// 覆盖保护：文件已存在且未指定 --force 时拒绝覆盖
		if !forceOverwrite {
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("config file already exists at %s; use --force to overwrite, or use --profile to create a separate config", path)
			}
		}

		cfg := defaultAgentConfig()
		if err := agent.SaveConfig(cfg, path); err != nil {
			return fmt.Errorf("save agentd config: %w", err)
		}
		fmt.Println("Agent daemon config initialized at", path)
		fmt.Println()
		fmt.Println("Required before running teammate-agentd:")
		fmt.Println("  teammate-agentd config set server.api_token <token>   # Agent API token from web UI")
		fmt.Println("  teammate-agentd config set agent.id <uuid>            # Agent UUID from web UI")
		fmt.Println("  teammate-agentd config set workspace.id <uuid>        # Workspace UUID")
		fmt.Println()
		fmt.Println("Tip: use --profile <name> to manage multiple agents:")
		fmt.Println("  teammate-agentd config init --profile claude")
		fmt.Println("  teammate-agentd config init --profile atomcode")
		return nil
	},
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the active agent daemon config path",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := resolvedConfigPath()
		if path == "" {
			path = agent.DefaultConfigPath()
		}
		fmt.Println(path)
		return nil
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get [key]",
	Short: "Display agent daemon configuration",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := agent.LoadConfig(resolvedConfigPath())
		if err != nil {
			return fmt.Errorf("load agentd config: %w", err)
		}
		if len(args) == 0 {
			printConfigSummary(cfg)
			return nil
		}
		value, ok := getConfigValue(cfg, args[0])
		if !ok {
			return fmt.Errorf("unknown config key: %s", args[0])
		}
		fmt.Println(value)
		return nil
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set an agent daemon configuration value",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := resolvedConfigPath()
		key := args[0]
		value := args[1]
		if err := setConfigValueAtPath(path, key, value); err != nil {
			return fmt.Errorf("set agentd config: %w", err)
		}
		fmt.Printf("Set %s = %s\n", key, value)
		return nil
	},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all agent daemon configuration values",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := agent.LoadConfig(resolvedConfigPath())
		if err != nil {
			return fmt.Errorf("load agentd config: %w", err)
		}
		return printConfig(cfg)
	},
}

var configLocalCmd = &cobra.Command{
	Use:   "local",
	Short: "Manage optional local control API settings",
}

var configLocalEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable the optional loopback local control API",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := resolvedConfigPath()
		if err := enableLocalAPIAtPath(path); err != nil {
			return fmt.Errorf("enable local control API: %w", err)
		}
		fmt.Println("Local control API enabled")
		return nil
	},
}

var configLocalDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable the optional local control API while keeping binding credentials",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := resolvedConfigPath()
		if err := disableLocalAPIAtPath(path); err != nil {
			return fmt.Errorf("disable local control API: %w", err)
		}
		fmt.Println("Local control API disabled")
		return nil
	},
}

func init() {
	configInitCmd.Flags().BoolVar(&forceOverwrite, "force", false, "overwrite existing config file")
	configListCmd.Flags().BoolVar(&showConfigSecrets, "show-secrets", false, "show full api tokens in json/yaml output")
	configCmd.AddCommand(configInitCmd)
	configCmd.AddCommand(configPathCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configListCmd)
	configLocalCmd.AddCommand(configLocalEnableCmd)
	configLocalCmd.AddCommand(configLocalDisableCmd)
	configCmd.AddCommand(configLocalCmd)
}

func defaultAgentConfig() *agent.Config {
	cfg := &agent.Config{
		Server: agent.ServerConfig{
			URL:      "http://localhost:8080",
			APIToken: "tm_YOUR_API_TOKEN_HERE",
		},
		Agent: agent.AgentInfo{
			ID:            "YOUR_AGENT_ID",
			Name:          "My Agent",
			ContextWindow: 100000,
			Provider:      "claude",
		},
		Workspace: agent.WorkspaceConfig{
			ID: "YOUR_WORKSPACE_ID",
		},
	}
	cfg.Tools.Claude.Path = "claude"
	return cfg
}

func getConfigValue(cfg *agent.Config, key string) (string, bool) {
	switch key {
	case "server.url":
		return cfg.Server.URL, true
	case "server.api_token":
		return maskToken(cfg.Server.APIToken), true
	case "agent.id":
		return cfg.Agent.ID, true
	case "agent.name":
		return cfg.Agent.Name, true
	case "agent.context_window":
		return fmt.Sprintf("%d", cfg.Agent.ContextWindow), true
	case "agent.provider":
		return cfg.Agent.Provider, true
	case "workspace.id":
		return cfg.Workspace.ID, true
	case "workspace.root":
		return cfg.Workspace.Root, true
	case "git.base_branch":
		return cfg.Git.BaseBranch, true
	case "local.enabled":
		return fmt.Sprintf("%t", cfg.Local.Enabled), true
	case "local.bind_addr":
		return cfg.Local.BindAddr, true
	case "local.local_token":
		return maskToken(cfg.Local.LocalToken), true
	case "local.instance_id":
		return cfg.Local.InstanceID, true
	case "debug":
		return fmt.Sprintf("%t", cfg.Debug), true
	default:
		if toolName, field, ok := parseToolKey(key); ok {
			tool, ok := getToolConfig(cfg, toolName)
			if !ok {
				return "", false
			}
			switch field {
			case "path":
				return tool.Path, true
			}
		}
		return "", false
	}
}

func setConfigValueAtPath(path, key, value string) error {
	if path == "" {
		path = agent.DefaultConfigPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("config file not found at %s; run 'teammate-agentd config init' to create one", path)
		}
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]interface{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	typedValue, err := parseConfigSetValue(key, value)
	if err != nil {
		return err
	}
	if err := setRawConfigValue(raw, key, typedValue); err != nil {
		return err
	}

	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, out, 0600)
}

func parseConfigSetValue(key, value string) (interface{}, error) {
	switch key {
	case "agent.context_window":
		var parsed int
		if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed <= 0 {
			return nil, fmt.Errorf("agent.context_window must be a positive integer")
		}
		return parsed, nil
	case "debug", "local.enabled":
		return parseBool(value), nil
	case "agent.provider":
		return strings.ToLower(strings.TrimSpace(value)), nil
	default:
		if toolName, _, ok := parseToolKey(key); ok {
			if !isKnownTool(toolName) {
				return nil, fmt.Errorf("unknown tool: %s", toolName)
			}
			return value, nil
		}
		if _, ok := knownScalarConfigKeys()[key]; ok {
			return value, nil
		}
		return nil, fmt.Errorf("unknown config key: %s", key)
	}
}

func knownScalarConfigKeys() map[string]struct{} {
	return map[string]struct{}{
		"server.url":        {},
		"server.api_token":  {},
		"agent.id":          {},
		"agent.name":        {},
		"workspace.id":      {},
		"workspace.root":    {},
		"git.base_branch":   {},
		"local.bind_addr":   {},
		"local.local_token": {},
		"local.instance_id": {},
	}
}

func enableLocalAPIAtPath(path string) error {
	raw, err := loadRawConfigForMutation(path)
	if err != nil {
		return err
	}
	local, ok := raw["local"].(map[string]interface{})
	if !ok {
		local = make(map[string]interface{})
		raw["local"] = local
	}
	local["enabled"] = true
	if rawStringValue(local, "bind_addr") == "" {
		local["bind_addr"] = "127.0.0.1:17380"
	}
	if rawStringValue(local, "instance_id") == "" {
		local["instance_id"] = uuid.NewString()
	}
	if rawStringValue(local, "local_token") == "" {
		token, err := agent.GenerateLocalToken()
		if err != nil {
			return err
		}
		local["local_token"] = token
	}
	return writeRawConfig(path, raw)
}

func disableLocalAPIAtPath(path string) error {
	raw, err := loadRawConfigForMutation(path)
	if err != nil {
		return err
	}
	local, ok := raw["local"].(map[string]interface{})
	if !ok {
		local = make(map[string]interface{})
		raw["local"] = local
	}
	local["enabled"] = false
	return writeRawConfig(path, raw)
}

func loadRawConfigForMutation(path string) (map[string]interface{}, error) {
	if path == "" {
		path = agent.DefaultConfigPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found at %s; run 'teammate-agentd config init' to create one", path)
		}
		return nil, err
	}
	var raw map[string]interface{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}
	return raw, nil
}

func writeRawConfig(path string, raw map[string]interface{}) error {
	if path == "" {
		path = agent.DefaultConfigPath()
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, data, 0600)
}

func rawStringValue(raw map[string]interface{}, key string) string {
	value, ok := raw[key].(string)
	if !ok {
		return ""
	}
	return value
}

func setRawConfigValue(raw map[string]interface{}, key string, value interface{}) error {
	parts := strings.Split(key, ".")
	if len(parts) == 0 {
		return fmt.Errorf("unknown config key: %s", key)
	}

	current := raw
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]interface{})
		if !ok {
			next = make(map[string]interface{})
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
	return nil
}

func parseToolKey(key string) (toolName, field string, ok bool) {
	parts := strings.Split(key, ".")
	if len(parts) != 3 || parts[0] != "tools" {
		return "", "", false
	}
	if parts[2] != "path" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func isKnownTool(name string) bool {
	switch name {
	case "claude", "openclaw", "opencode", "atomcode", "mimocode":
		return true
	default:
		return false
	}
}

func getToolConfig(cfg *agent.Config, name string) (agent.ToolConfig, bool) {
	switch name {
	case "claude":
		return cfg.Tools.Claude, true
	case "openclaw":
		return cfg.Tools.OpenClaw, true
	case "opencode":
		return cfg.Tools.OpenCode, true
	case "atomcode":
		return cfg.Tools.AtomCode, true
	case "mimocode":
		return cfg.Tools.MiMoCode, true
	default:
		return agent.ToolConfig{}, false
	}
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on", "enabled":
		return true
	default:
		return false
	}
}

func printConfig(cfg *agent.Config) error {
	switch strings.ToLower(outputFmt) {
	case "json", "yaml", "yml":
		data, err := marshalConfigForOutput(cfg, outputFmt, showConfigSecrets)
		if err != nil {
			return err
		}
		fmt.Println(string(data))
	default:
		printConfigSummary(cfg)
	}
	return nil
}

func maskedConfig(cfg *agent.Config) *agent.Config {
	copy := *cfg
	copy.Server.APIToken = maskToken(cfg.Server.APIToken)
	copy.Local.LocalToken = maskToken(cfg.Local.LocalToken)
	return &copy
}

func marshalConfigForOutput(cfg *agent.Config, format string, showSecrets bool) ([]byte, error) {
	outputCfg := cfg
	if !showSecrets {
		outputCfg = maskedConfig(cfg)
	}
	switch strings.ToLower(format) {
	case "json":
		return agent.MarshalConfigJSON(outputCfg)
	case "yaml", "yml":
		return agent.MarshalConfigYAML(outputCfg)
	default:
		return nil, fmt.Errorf("unsupported output format: %s", format)
	}
}

func printConfigSummary(cfg *agent.Config) {
	fmt.Printf("Server URL:       %s\n", cfg.Server.URL)
	fmt.Printf("API Token:        %s\n", maskToken(cfg.Server.APIToken))
	fmt.Printf("Agent ID:         %s\n", cfg.Agent.ID)
	fmt.Printf("Agent Name:       %s\n", cfg.Agent.Name)
	fmt.Printf("Agent Provider:   %s\n", cfg.Agent.Provider)
	fmt.Printf("Context Window:   %d\n", cfg.Agent.ContextWindow)
	fmt.Printf("Workspace ID:     %s\n", cfg.Workspace.ID)
	fmt.Printf("Workspace Root:   %s\n", cfg.Workspace.Root)
	fmt.Printf("Git Base Branch:  %s\n", cfg.Git.BaseBranch)
	fmt.Printf("Local API:        %v\n", cfg.Local.Enabled)
	fmt.Printf("Local Bind Addr:  %s\n", cfg.Local.BindAddr)
	fmt.Printf("Local Token:      %s\n", maskToken(cfg.Local.LocalToken))
	fmt.Printf("Local Instance:   %s\n", cfg.Local.InstanceID)
	fmt.Printf("Debug:            %v\n", cfg.Debug)
}

func maskToken(token string) string {
	if len(token) <= 10 {
		return token
	}
	return token[:6] + "..." + token[len(token)-4:]
}
