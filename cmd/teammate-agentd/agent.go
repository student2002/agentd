// agent.go provides the `agent list` subcommand: a read-only view of the
// instances materialized into the daemon config from desired delivery,
// grouped by workspace connection. Instances are created by workspace admins
// in the web UI (daemon group); there is no local creation or removal path.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/teammate/agentd/internal/agent"
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Inspect the agent instances materialized in the daemon config",
	Long:  "Inspect the agent instances materialized in the daemon config, one row per (connection, instance). Instances are created by workspace admins in the web UI (daemon group) and materialized here on desired delivery; there is no local instance management.",
}

var agentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List agent instances grouped by workspace connection",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := agent.LoadGlobalConfig(resolvedConfigPath())
		if err != nil {
			return fmt.Errorf("load agentd config: %w", err)
		}
		type row struct {
			Connection string `json:"connection"`
			Name       string `json:"name"`
			Provider   string `json:"provider"`
			PersonaKey string `json:"persona_key,omitempty"`
			AgentID    string `json:"agent_id,omitempty"`
		}
		rows := make([]row, 0)
		for _, w := range cfg.Workspaces {
			for _, a := range w.Agents {
				rows = append(rows, row{Connection: w.Name, Name: a.Name, Provider: a.Provider, PersonaKey: a.PersonaKey, AgentID: a.AgentID})
			}
		}
		if strings.ToLower(outputFmt) == "json" {
			data, err := json.MarshalIndent(rows, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}
		if len(rows) == 0 {
			fmt.Println("No instances materialized yet. Create them in the workspace web UI (daemon group); agentd materializes them after registering.")
			return nil
		}
		current := ""
		for _, r := range rows {
			if r.Connection != current {
				current = r.Connection
				fmt.Printf("[%s]\n", current)
			}
			identity := "(pending delivery)"
			if r.AgentID != "" {
				identity = shortIdentity(r.AgentID)
			}
			persona := "-"
			if r.PersonaKey != "" {
				persona = shortIdentity(r.PersonaKey)
			}
			fmt.Printf("  %s\t%s\t%s\tmem=%s\n", r.Name, r.Provider, identity, persona)
		}
		return nil
	},
}

func shortIdentity(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func init() {
	agentCmd.AddCommand(agentListCmd)
}
