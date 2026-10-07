// mcp.go provides the `mcp` subcommand: the local MCP memory server run over
// stdio. The coding tool launches it from the workDir MCP config; the
// execution context (instance name, connection, workspace) and the connection
// credentials arrive through the environment variables the executor injects.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/teammate/agentd/internal/agent"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run the local MCP memory server on stdio",
	Long:  "Run the local MCP memory server for one coding-tool execution. Launched by the coding tool through the MCP config written into the workDir; the instance context and connection credentials come from the TEAMMATE_MCP_* environment variables.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		instance := os.Getenv(agent.MCPCtxInstance)
		if instance == "" {
			return fmt.Errorf("%s is required (this command is launched by the coding tool from the workDir MCP config)", agent.MCPCtxInstance)
		}
		return agent.RunMCPServer(agent.MCPServerConfig{
			InstanceName: instance,
			PersonaKey:   os.Getenv(agent.MCPCtxPersona),
			Connection:   os.Getenv(agent.MCPCtxConnection),
			WorkspaceID:  os.Getenv(agent.MCPCtxWorkspaceID),
			AgentID:      os.Getenv(agent.MCPCtxAgentID),
			ServerURL:    os.Getenv(agent.MCPCtxServerURL),
			Token:        os.Getenv(agent.MCPCtxToken),
		})
	},
}

func init() {
	rootCmd.AddCommand(mcpCmd)
}
