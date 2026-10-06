// config.go provides config management for the Agent Daemon.
//
// The daemon runs one process per machine managing agent instances scoped to
// workspace connections: the merge key of an instance is (connection, name),
// so each instance belongs to exactly one connection entry and carries
// exactly one server identity. The on-disk config is a single GlobalConfig
// keyed by the machine: one WorkspaceEntry per remote workspace (its own td_
// token plus its materialized instance list). Per-agent components
// (executor, context builder, capability materializer) keep consuming the
// legacy *Config shape, produced by viewForAgent so they stay agnostic of
// this file's schema; workspace identity is carried by the execution
// context, never by the config view.
package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// Config is the per-agent view consumed by the executor, context builder,
// capability materializer, MCP config writer and git manager. It carries no
// credentials and no workspace identity: server auth is connection-level (td_
// token held by the per-workspace Client) and the workspace/agent identity is
// attached at execution time via RunContext.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Agent     AgentInfo       `yaml:"agent"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Tools     ToolsConfig     `yaml:"tools"`
	Git       GitConfig       `yaml:"git"`
	Local     LocalConfig     `yaml:"local"`
	Debug     bool            `yaml:"debug,omitempty"`
}

// ServerConfig represents the server connection configuration.
type ServerConfig struct {
	URL string `yaml:"url"`
}

// AgentInfo represents the basic information of one agent instance. The
// server UUID is per-connection state held by the runtime, not by this view.
type AgentInfo struct {
	Name          string `yaml:"name"`
	ContextWindow int    `yaml:"context_window"`
	Provider      string `yaml:"provider"`
	PersonaKey    string `yaml:"persona_key,omitempty"`
}

// WorkspaceConfig carries the machine-level workspace layout consumed by the
// executor and git manager. Root is the machine workspace root; per-task
// directories are built as {root}/{workspaceID}/{agentID}/{projectID}/{taskID}
// with the workspace/agent IDs taken from the execution context.
type WorkspaceConfig struct {
	Root string `yaml:"root"`
}

// ToolsConfig represents the coding tools configuration.
type ToolsConfig struct {
	Claude   ToolConfig `yaml:"claude"`
	OpenClaw ToolConfig `yaml:"openclaw"`
	OpenCode ToolConfig `yaml:"opencode"`
	AtomCode ToolConfig `yaml:"atomcode"`
	MiMoCode ToolConfig `yaml:"mimocode"`
}

// ToolConfig represents the configuration of a single coding tool.
type ToolConfig struct {
	Path string `yaml:"path"`
}

// GitConfig represents the Git-related configuration.
type GitConfig struct {
	BaseBranch string `yaml:"base_branch"`
}

// LocalConfig represents the local control API configuration.
type LocalConfig struct {
	Enabled    bool   `yaml:"enabled"`
	BindAddr   string `yaml:"bind_addr"`
	LocalToken string `yaml:"local_token"`
	InstanceID string `yaml:"instance_id"`
}

// WorkspaceAgent is one materialized instance on a workspace connection:
// delivered by the server, identified by that workspace's agent UUID.
type WorkspaceAgent struct {
	Name       string `yaml:"name" json:"name"`
	Provider   string `yaml:"provider" json:"provider"`
	PersonaKey string `yaml:"persona_key,omitempty" json:"persona_key,omitempty"`
	AgentID    string `yaml:"agent_id,omitempty" json:"agent_id,omitempty"`
}

// WorkspaceEntry is one remote-workspace connection in the local config. Each
// entry owns its daemon token (td_) issued by that workspace's admin; the
// daemon registers, streams events and heartbeats once per entry. Agents is
// the connection's full materialized instance set — empty means nothing has
// been delivered yet, not "every instance". WorkspaceID and DaemonID are
// adopted from the register response and written back; they are never
// hand-edited. Each agent's AgentID is the server UUID bound from desired
// delivery: delivery is incremental (rows leave desired once online), so
// restarts restore identity from here, and a web-side delete+recreate
// re-delivers a new UUID that overwrites the field.
type WorkspaceEntry struct {
	Name        string           `yaml:"name" json:"name"`
	Token       string           `yaml:"token" json:"token"`
	Agents      []WorkspaceAgent `yaml:"agents,omitempty" json:"agents,omitempty"`
	WorkspaceID string           `yaml:"workspace_id,omitempty" json:"workspace_id,omitempty"`
	DaemonID    string           `yaml:"daemon_id,omitempty" json:"daemon_id,omitempty"`
}

// Agent returns the connection's materialized instance with the given name,
// or nil when absent.
func (e *WorkspaceEntry) Agent(name string) *WorkspaceAgent {
	for i := range e.Agents {
		if e.Agents[i].Name == name {
			return &e.Agents[i]
		}
	}
	return nil
}

// GlobalConfig is the machine-level daemon configuration.
type GlobalConfig struct {
	Server struct {
		URL string
	}
	Name          string
	Workspaces    []WorkspaceEntry
	WorkspaceRoot string
	Tools         ToolsConfig
	Git           GitConfig
	Local         LocalConfig
	Debug         bool
}

// SupportedProviders lists the coding-tool providers an agent instance may use.
var SupportedProviders = []string{"claude", "openclaw", "opencode", "atomcode", "mimocode"}

// DefaultConfigPath returns the default config file path.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".teammate/config.yaml"
	}
	return filepath.Join(home, ".teammate", "config.yaml")
}

// DefaultWorkspaceRoot returns the default workspace root directory.
func DefaultWorkspaceRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".teammate", "workspaces")
	}
	return filepath.Join(home, ".teammate", "workspaces")
}

// LoadGlobalConfig loads the daemon config from a YAML file and applies
// runtime defaults.
func LoadGlobalConfig(path string) (*GlobalConfig, error) {
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

	cfg := globalConfigFromRaw(raw)
	applyGlobalConfigDefaults(cfg)
	return cfg, nil
}

func globalConfigFromRaw(raw map[string]interface{}) *GlobalConfig {
	cfg := &GlobalConfig{}

	server := mapValue(raw, "server")
	cfg.Server.URL = stringValue(server, "url")

	cfg.Name = stringValue(raw, "name")

	if workspaceList, ok := raw["workspaces"].([]interface{}); ok {
		for _, item := range workspaceList {
			entry, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			cfg.Workspaces = append(cfg.Workspaces, workspaceEntryFromRaw(entry))
		}
	}

	cfg.WorkspaceRoot = stringValue(raw, "workspace_root")

	cfg.Tools = toolsConfigFromRaw(mapValue(raw, "tools"))

	git := mapValue(raw, "git")
	cfg.Git.BaseBranch = stringValue(git, "base_branch")

	local := mapValue(raw, "local")
	// The local control frontend is on by default; the local section only
	// needs to appear in the config to disable it or override the defaults.
	cfg.Local.Enabled = true
	if v, ok := local["enabled"].(bool); ok {
		cfg.Local.Enabled = v
	}
	cfg.Local.BindAddr = stringValue(local, "bind_addr")
	cfg.Local.LocalToken = stringValue(local, "local_token")
	cfg.Local.InstanceID = stringValue(local, "instance_id")

	cfg.Debug = boolValue(raw, "debug")

	return cfg
}

func workspaceEntryFromRaw(entry map[string]interface{}) WorkspaceEntry {
	workspace := WorkspaceEntry{
		Name:        strings.TrimSpace(stringValue(entry, "name")),
		Token:       strings.TrimSpace(stringValue(entry, "token")),
		WorkspaceID: strings.TrimSpace(stringValue(entry, "workspace_id")),
		DaemonID:    strings.TrimSpace(stringValue(entry, "daemon_id")),
	}
	// Agents parse as object entries only; the legacy string-list shape is
	// not recognized (affected instances re-arrive through desired delivery).
	if agentList, ok := entry["agents"].([]interface{}); ok {
		for _, item := range agentList {
			agent, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			materialized := WorkspaceAgent{
				Name:       strings.TrimSpace(stringValue(agent, "name")),
				Provider:   strings.ToLower(strings.TrimSpace(stringValue(agent, "provider"))),
				PersonaKey: strings.TrimSpace(stringValue(agent, "persona_key")),
				AgentID:    strings.TrimSpace(stringValue(agent, "agent_id")),
			}
			if materialized.Name != "" {
				workspace.Agents = append(workspace.Agents, materialized)
			}
		}
	}
	return workspace
}

func toolsConfigFromRaw(tools map[string]interface{}) ToolsConfig {
	return ToolsConfig{
		Claude:   toolConfigFromRaw(mapValue(tools, "claude")),
		OpenClaw: toolConfigFromRaw(mapValue(tools, "openclaw")),
		OpenCode: toolConfigFromRaw(mapValue(tools, "opencode")),
		AtomCode: toolConfigFromRaw(mapValue(tools, "atomcode")),
		MiMoCode: toolConfigFromRaw(mapValue(tools, "mimocode")),
	}
}

func toolConfigFromRaw(raw map[string]interface{}) ToolConfig {
	return ToolConfig{Path: stringValue(raw, "path")}
}

func applyGlobalConfigDefaults(cfg *GlobalConfig) {
	if cfg.WorkspaceRoot == "" {
		cfg.WorkspaceRoot = DefaultWorkspaceRoot()
	} else {
		cfg.WorkspaceRoot = expandHome(cfg.WorkspaceRoot)
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
	if cfg.Git.BaseBranch == "" {
		cfg.Git.BaseBranch = "master"
	}
	if cfg.Local.BindAddr == "" {
		cfg.Local.BindAddr = "127.0.0.1:17380"
	}
	for i := range cfg.Workspaces {
		cfg.Workspaces[i].Name = strings.TrimSpace(cfg.Workspaces[i].Name)
		cfg.Workspaces[i].Token = strings.TrimSpace(cfg.Workspaces[i].Token)
		for j := range cfg.Workspaces[i].Agents {
			cfg.Workspaces[i].Agents[j].Name = strings.TrimSpace(cfg.Workspaces[i].Agents[j].Name)
			cfg.Workspaces[i].Agents[j].Provider = strings.ToLower(strings.TrimSpace(cfg.Workspaces[i].Agents[j].Provider))
			cfg.Workspaces[i].Agents[j].PersonaKey = strings.TrimSpace(cfg.Workspaces[i].Agents[j].PersonaKey)
			cfg.Workspaces[i].Agents[j].AgentID = strings.TrimSpace(cfg.Workspaces[i].Agents[j].AgentID)
		}
	}
}

// Workspace returns the workspace connection entry with the given name, or nil
// when absent.
func (g *GlobalConfig) Workspace(name string) *WorkspaceEntry {
	for i := range g.Workspaces {
		if g.Workspaces[i].Name == name {
			return &g.Workspaces[i]
		}
	}
	return nil
}

// DeriveWorkspaceName builds a fallback local alias for a workspace token:
// ws-{first 8 chars of the token's third underscore-separated segment}. The
// CLI asks for an explicit name; this only backs API-driven additions that
// omit one.
func DeriveWorkspaceName(token string) string {
	segments := strings.Split(token, "_")
	if len(segments) >= 3 && segments[2] != "" {
		suffix := segments[2]
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		return "ws-" + suffix
	}
	return "ws-" + uuid.NewString()[:8]
}

// MachineView builds a Config view for machine-level components (chat, local
// control) that only need the tool paths and the workspace root.
func (g *GlobalConfig) MachineView() *Config {
	view := &Config{
		Server:    ServerConfig{URL: g.Server.URL},
		Workspace: WorkspaceConfig{Root: g.WorkspaceRoot},
		Tools:     g.Tools,
		Git:       g.Git,
		Local:     g.Local,
		Debug:     g.Debug,
	}
	return view
}

// viewForAgent builds the per-agent Config view for one materialized instance
// of a connection. The provider comes from the instance itself; everything
// else (server/tools/git/local) is machine-level. The view carries no
// credentials and no workspace identity; the server UUID is per-connection
// state held by the runtime, not by the view.
func (g *GlobalConfig) viewForAgent(entry *WorkspaceEntry, a WorkspaceAgent) *Config {
	return &Config{
		Server: ServerConfig{URL: g.Server.URL},
		Agent: AgentInfo{
			Name:          a.Name,
			Provider:      a.Provider,
			PersonaKey:    a.PersonaKey,
			ContextWindow: 100000,
		},
		Workspace: WorkspaceConfig{Root: g.WorkspaceRoot},
		Tools:     g.Tools,
		Git:       g.Git,
		Local:     g.Local,
		Debug:     g.Debug,
	}
}

// ValidateGlobalConfig validates the required config needed to start the
// daemon.
func ValidateGlobalConfig(cfg *GlobalConfig) error {
	if len(cfg.Workspaces) == 0 {
		return fmt.Errorf("at least one workspace entry is required; run 'teammate-agentd workspace add <td_token> --name <alias>'")
	}
	// A connection's agents list may be empty: a fresh machine starts with no
	// delivered instances and materializes them from desired delivery after
	// registering.
	seenWorkspaces := make(map[string]struct{}, len(cfg.Workspaces))
	for _, w := range cfg.Workspaces {
		if w.Name == "" {
			return fmt.Errorf("workspace name is required for every workspaces[] entry")
		}
		if _, dup := seenWorkspaces[w.Name]; dup {
			return fmt.Errorf("duplicate workspace name %q; names must be unique within the daemon", w.Name)
		}
		seenWorkspaces[w.Name] = struct{}{}
		if !strings.HasPrefix(w.Token, "td_") {
			return fmt.Errorf("workspace %q requires a daemon token with the td_ prefix from the web UI", w.Name)
		}
		seenAgents := make(map[string]struct{}, len(w.Agents))
		for _, a := range w.Agents {
			if a.Name == "" {
				return fmt.Errorf("agent name is required for every agents[] entry in workspace %q", w.Name)
			}
			if _, dup := seenAgents[a.Name]; dup {
				return fmt.Errorf("duplicate agent name %q in workspace %q; names must be unique within one connection", a.Name, w.Name)
			}
			seenAgents[a.Name] = struct{}{}
			if !isValidProvider(a.Provider) {
				return fmt.Errorf("invalid provider %q for agent %q in workspace %q; supported providers: %s", a.Provider, a.Name, w.Name, strings.Join(SupportedProviders, ", "))
			}
		}
	}
	if cfg.Local.Enabled {
		// local_token / instance_id are generated at startup when unset
		// (EnsureLocalCredentials); only the bind address is validated here.
		if err := ValidateLoopbackBindAddr(cfg.Local.BindAddr); err != nil {
			return err
		}
	}
	return nil
}

// EnsureLocalCredentials generates the local control token and instance id
// when the local server is enabled but they are unset, persisting them so
// they stay stable across restarts (the control-page URL and EventSource
// streams embed the token).
func (g *GlobalConfig) EnsureLocalCredentials(path string) error {
	if !g.Local.Enabled {
		return nil
	}
	if path == "" {
		path = DefaultConfigPath()
	}
	filled := false
	if g.Local.LocalToken == "" {
		token, err := GenerateLocalToken()
		if err != nil {
			return fmt.Errorf("generate local token: %w", err)
		}
		g.Local.LocalToken = token
		filled = true
	}
	if g.Local.InstanceID == "" {
		g.Local.InstanceID = "inst-" + uuid.NewString()[:8]
		filled = true
	}
	if !filled {
		return nil
	}
	if err := SaveGlobalConfig(g, path); err != nil {
		return fmt.Errorf("persist generated local credentials: %w", err)
	}
	log.Printf("[agentd] generated local control credentials (local.local_token / local.instance_id) and persisted them to %s", path)
	return nil
}

// SaveGlobalConfig writes the daemon config to a file as sparse YAML.
func SaveGlobalConfig(cfg *GlobalConfig, path string) error {
	if path == "" {
		path = DefaultConfigPath()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := MarshalGlobalConfigYAML(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// MarshalGlobalConfigYAML converts the daemon config to YAML without
// meaningless empty fields.
func MarshalGlobalConfigYAML(cfg *GlobalConfig) ([]byte, error) {
	return yaml.Marshal(sparseGlobalConfigMap(cfg))
}

// MarshalGlobalConfigJSON converts the daemon config to indented JSON.
func MarshalGlobalConfigJSON(cfg *GlobalConfig) ([]byte, error) {
	return json.MarshalIndent(sparseGlobalConfigMap(cfg), "", "  ")
}

func sparseGlobalConfigMap(cfg *GlobalConfig) map[string]interface{} {
	out := make(map[string]interface{})

	server := make(map[string]interface{})
	addString(server, "url", cfg.Server.URL)
	addMap(out, "server", server)

	addString(out, "name", cfg.Name)

	if len(cfg.Workspaces) > 0 {
		workspaces := make([]interface{}, 0, len(cfg.Workspaces))
		for _, w := range cfg.Workspaces {
			entry := make(map[string]interface{})
			entry["name"] = w.Name
			entry["token"] = w.Token
			if len(w.Agents) > 0 {
				agents := make([]interface{}, 0, len(w.Agents))
				for _, a := range w.Agents {
					agent := make(map[string]interface{})
					agent["name"] = a.Name
					agent["provider"] = a.Provider
					addString(agent, "persona_key", a.PersonaKey)
					addString(agent, "agent_id", a.AgentID)
					agents = append(agents, agent)
				}
				entry["agents"] = agents
			}
			addString(entry, "workspace_id", w.WorkspaceID)
			addString(entry, "daemon_id", w.DaemonID)
			workspaces = append(workspaces, entry)
		}
		out["workspaces"] = workspaces
	}

	addString(out, "workspace_root", cfg.WorkspaceRoot)

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
	// enabled defaults to true when absent, so only an explicit opt-out is
	// meaningful on disk.
	if !cfg.Local.Enabled {
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

func boolValue(raw map[string]interface{}, key string) bool {
	if raw == nil {
		return false
	}
	value, ok := raw[key].(bool)
	return ok && value
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

// expandHome expands the ~/ prefix in a path to the user's actual home
// directory path.
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
