// config_test.go covers tests of the config loading logic in config.go.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func TestSaveDefaultConfigWritesSparseYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := agent.SaveGlobalConfig(defaultGlobalConfig(), path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(data)
	for _, unwanted := range []string{
		"local:",
		"openclaw:",
		"opencode:",
		"atomcode:",
		"mimocode:",
		"base_branch: master",
		"max_concurrent_executions",
		"path: \"\"",
		"agents:",
		"workspaces:",
		"daemon:",
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("default config wrote unwanted %q into sparse YAML:\n%s", unwanted, got)
		}
	}
	for _, want := range []string{"url: http://127.0.0.1:8080", "workspace_root:", "claude:", "path: claude"} {
		if !strings.Contains(got, want) {
			t.Fatalf("default config missing %q, got:\n%s", want, got)
		}
	}
}

func TestConfigLocalEnableGeneratesLocalBindingCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := agent.SaveGlobalConfig(defaultGlobalConfig(), path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config before enable: %v", err)
	}
	if strings.Contains(string(before), "local:") {
		t.Fatalf("default config should not enable local API:\n%s", string(before))
	}

	if err := enableLocalAPIAtPath(path); err != nil {
		t.Fatalf("enable local API: %v", err)
	}

	cfg, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("load enabled config: %v", err)
	}
	if !cfg.Local.Enabled {
		t.Fatal("expected local API to be enabled")
	}
	if cfg.Local.BindAddr != "127.0.0.1:17380" {
		t.Fatalf("unexpected bind addr: %q", cfg.Local.BindAddr)
	}
	if cfg.Local.InstanceID == "" {
		t.Fatal("expected instance_id to be generated")
	}
	if !strings.HasPrefix(cfg.Local.LocalToken, "lt_") {
		t.Fatalf("expected local token with lt_ prefix, got %q", cfg.Local.LocalToken)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read enabled config: %v", err)
	}
	got := string(data)
	for _, want := range []string{"local:", "enabled: true", "instance_id:", "local_token: lt_"} {
		if !strings.Contains(got, want) {
			t.Fatalf("enabled config missing %q:\n%s", want, got)
		}
	}
}

func TestConfigLocalEnablePreservesSparseHandWrittenYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := strings.Join([]string{
		"server:",
		"  url: http://127.0.0.1:8080",
		"workspaces:",
		"  - name: team-a",
		"    token: td_token",
		"tools:",
		"  claude:",
		"    path: claude",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := enableLocalAPIAtPath(path); err != nil {
		t.Fatalf("enable local API: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(data)
	for _, unwanted := range []string{
		"openclaw:",
		"opencode:",
		"atomcode:",
		"mimocode:",
		"base_branch: master",
		"path: \"\"",
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("config local enable wrote unwanted %q into sparse YAML:\n%s", unwanted, got)
		}
	}
	for _, want := range []string{"local:", "enabled: true", "instance_id:", "local_token: lt_"} {
		if !strings.Contains(got, want) {
			t.Fatalf("enabled config missing %q:\n%s", want, got)
		}
	}
}

func TestConfigLocalDisableKeepsBindingCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := agent.SaveGlobalConfig(defaultGlobalConfig(), path); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if err := enableLocalAPIAtPath(path); err != nil {
		t.Fatalf("enable local API: %v", err)
	}
	enabled, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("load enabled config: %v", err)
	}

	if err := disableLocalAPIAtPath(path); err != nil {
		t.Fatalf("disable local API: %v", err)
	}

	disabled, err := agent.LoadGlobalConfig(path)
	if err != nil {
		t.Fatalf("load disabled config: %v", err)
	}
	if disabled.Local.Enabled {
		t.Fatal("expected local API to be disabled")
	}
	if disabled.Local.InstanceID != enabled.Local.InstanceID {
		t.Fatalf("expected instance id to be preserved, got %q want %q", disabled.Local.InstanceID, enabled.Local.InstanceID)
	}
	if disabled.Local.LocalToken != enabled.Local.LocalToken {
		t.Fatalf("expected local token to be preserved")
	}
}

func TestConfigSetPreservesSparseHandWrittenYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := strings.Join([]string{
		"server:",
		"  url: http://127.0.0.1:8080",
		"workspaces:",
		"  - name: team-a",
		"    token: td_token",
		"tools:",
		"  claude:",
		"    path: claude",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := setConfigValueAtPath(path, "name", "dev-box-01"); err != nil {
		t.Fatalf("set config: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(data)
	for _, unwanted := range []string{
		"openclaw:",
		"opencode:",
		"atomcode:",
		"mimocode:",
		"base_branch: master",
		"path: \"\"",
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("config set wrote unwanted %q into sparse YAML:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "name: dev-box-01") {
		t.Fatalf("expected updated name, got:\n%s", got)
	}
	if !strings.Contains(got, "token: td_token") {
		t.Fatalf("expected existing workspace token to remain, got:\n%s", got)
	}
}

func TestConfigSetCanPatchIncompatiblePartialYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	partial := strings.Join([]string{
		"server:",
		"  url: http://127.0.0.1:8080",
		"tools: []",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(partial), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := setConfigValueAtPath(path, "tools.claude.path", "claude"); err != nil {
		t.Fatalf("set config: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "tools:") || !strings.Contains(got, "claude:") || !strings.Contains(got, "path: claude") {
		t.Fatalf("expected tools.claude.path to be materialized, got:\n%s", got)
	}
}

func TestConfigSetRejectsRemovedDaemonKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := agent.SaveGlobalConfig(defaultGlobalConfig(), path); err != nil {
		t.Fatalf("save config: %v", err)
	}
	for _, key := range []string{"agent.id", "agent.provider", "server.api_token", "agents", "daemon.token", "daemon.workspace_id", "daemon.name", "workspaces"} {
		if err := setConfigValueAtPath(path, key, "x"); err == nil {
			t.Fatalf("expected set %q to be rejected", key)
		}
	}
}

func TestConfigListYAMLMasksSecretsByDefault(t *testing.T) {
	cfg := defaultGlobalConfig()
	cfg.Workspaces = append(cfg.Workspaces, agent.WorkspaceEntry{
		Name:  "team-a",
		Token: "td_1234567890abcdef1234567890abcdef",
	})
	cfg.Local.Enabled = true
	cfg.Local.LocalToken = "lt_1234567890abcdef1234567890abcdef"
	cfg.Local.InstanceID = "instance-1"

	out, err := marshalConfigForOutput(cfg, "yaml", false)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	got := string(out)
	if strings.Contains(got, cfg.Workspaces[0].Token) || strings.Contains(got, cfg.Local.LocalToken) {
		t.Fatalf("expected secrets to be masked, got:\n%s", got)
	}
	if !strings.Contains(got, "td_123...cdef") || !strings.Contains(got, "lt_123...cdef") {
		t.Fatalf("expected masked tokens, got:\n%s", got)
	}
}

func TestConfigListJSONCanShowSecretsExplicitly(t *testing.T) {
	cfg := defaultGlobalConfig()
	cfg.Workspaces = append(cfg.Workspaces, agent.WorkspaceEntry{
		Name:  "team-a",
		Token: "td_1234567890abcdef1234567890abcdef",
	})
	cfg.Local.Enabled = true
	cfg.Local.LocalToken = "lt_1234567890abcdef1234567890abcdef"
	cfg.Local.InstanceID = "instance-1"

	out, err := marshalConfigForOutput(cfg, "json", true)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, cfg.Workspaces[0].Token) || !strings.Contains(got, cfg.Local.LocalToken) {
		t.Fatalf("expected explicit secret output, got:\n%s", got)
	}
}
