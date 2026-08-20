// config_test.go 覆盖 Agent Daemon 配置加载的测试。
package agent_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func TestLoadConfigToleratesPartialIncompatibleSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  url: http://localhost:8080
agent:
  id: agent-1
workspace:
  id: ws-1
tools: []
git: ""
`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := agent.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig should tolerate incompatible optional sections: %v", err)
	}

	if cfg.Server.URL != "http://localhost:8080" {
		t.Fatalf("server.url not loaded: %q", cfg.Server.URL)
	}
	if cfg.Agent.ID != "agent-1" {
		t.Fatalf("agent.id not loaded: %q", cfg.Agent.ID)
	}
	if cfg.Workspace.ID != "ws-1" {
		t.Fatalf("workspace.id not loaded: %q", cfg.Workspace.ID)
	}
	if cfg.Tools.Claude.Path != "claude" {
		t.Fatalf("expected default claude path, got %q", cfg.Tools.Claude.Path)
	}
	if cfg.Git.BaseBranch != "master" {
		t.Fatalf("expected default git base branch, got %q", cfg.Git.BaseBranch)
	}
}

func TestLoadConfigDefaultsLocalAPIDisabledWithoutExpandingSavedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`server:
  url: http://localhost:8080
  api_token: tm_test
agent:
  id: agent-1
  name: ac
  provider: claude
workspace:
  id: ws-1
tools:
  claude:
    path: claude
`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := agent.LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Local.Enabled {
		t.Fatal("expected local control API to be disabled by default")
	}
	if cfg.Local.BindAddr != "127.0.0.1:17380" {
		t.Fatalf("unexpected local bind addr: %q", cfg.Local.BindAddr)
	}

	out, err := agent.MarshalConfigYAML(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if bytes.Contains(out, []byte("local:")) {
		t.Fatalf("local defaults should not expand saved YAML:\n%s", string(out))
	}
}

func TestValidateConfigRequiresLocalCredentialsWhenEnabled(t *testing.T) {
	cfg := &agent.Config{}
	cfg.Agent.Provider = "claude"
	cfg.Local.Enabled = true
	cfg.Local.BindAddr = "127.0.0.1:17380"

	err := agent.ValidateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "local.local_token is required") {
		t.Fatalf("expected local token validation error, got %v", err)
	}

	cfg.Local.LocalToken = "lt_test"
	err = agent.ValidateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "local.instance_id is required") {
		t.Fatalf("expected local instance validation error, got %v", err)
	}
}

func TestValidateConfigRejectsNonLoopbackLocalBindAddr(t *testing.T) {
	cfg := &agent.Config{}
	cfg.Agent.Provider = "claude"
	cfg.Local.Enabled = true
	cfg.Local.BindAddr = "0.0.0.0:17380"
	cfg.Local.LocalToken = "lt_test"
	cfg.Local.InstanceID = "instance-1"

	err := agent.ValidateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "local control bind addr must be loopback") {
		t.Fatalf("expected loopback validation error, got %v", err)
	}
}

func TestValidateConfigAcceptsCompleteLocalAPIConfig(t *testing.T) {
	cfg := &agent.Config{}
	cfg.Agent.Provider = "claude"
	cfg.Local.Enabled = true
	cfg.Local.BindAddr = "127.0.0.1:17380"
	cfg.Local.LocalToken = "lt_test"
	cfg.Local.InstanceID = "instance-1"

	if err := agent.ValidateConfig(cfg); err != nil {
		t.Fatalf("expected config to validate: %v", err)
	}
}
