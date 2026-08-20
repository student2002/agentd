// mcp_config_test.go 覆盖 MCP 配置文件生成的测试。
package agent_test

import (
	"encoding/json"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func TestBuildMCPToolConfig(t *testing.T) {
	config, enabled := agent.BuildMCPToolConfig([]agent.AgentMcpServerContext{
		{
			ID:      "mcp-1",
			Name:    "Docs MCP!",
			URL:     "https://mcp.example.test/sse",
			Type:    "",
			EnvVars: json.RawMessage(`{"TOKEN":"secret","COUNT":3}`),
			Enabled: true,
		},
		{
			ID:      "mcp-2",
			Name:    "Disabled MCP",
			URL:     "https://disabled.example.test",
			Enabled: false,
		},
		{
			ID:      "mcp-3",
			Name:    "Docs MCP!",
			URL:     "https://mcp2.example.test/api",
			Type:    "http",
			Enabled: true,
		},
	})

	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled MCP servers, got %d", len(enabled))
	}
	if len(config.MCPServers) != 2 {
		t.Fatalf("expected 2 MCP config entries, got %d", len(config.MCPServers))
	}

	first, ok := config.MCPServers["Docs-MCP"]
	if !ok {
		t.Fatalf("expected sanitized MCP name Docs-MCP, got %#v", config.MCPServers)
	}
	if first.Type != "sse" {
		t.Fatalf("expected inferred sse transport, got %q", first.Type)
	}
	if first.Env["TOKEN"] != "secret" || first.Env["COUNT"] != "3" {
		t.Fatalf("expected env values to be preserved and stringified, got %#v", first.Env)
	}

	second, ok := config.MCPServers["Docs-MCP-2"]
	if !ok {
		t.Fatalf("expected duplicate MCP name to be suffixed, got %#v", config.MCPServers)
	}
	if second.Type != "http" {
		t.Fatalf("expected explicit http transport, got %q", second.Type)
	}
}
