// workspace.go provides the `workspace` subcommands: local management of the
// remote-workspace connections listed in the daemon config. Each connection
// owns one daemon token (td_) issued by that workspace's admin. Edits only the
// YAML; changes take effect after an agentd restart (hot management is the
// local control console's job). A connection is name + token only — its
// instances arrive through desired delivery.
package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/teammate/agentd/internal/agent"
)

var workspaceAddName string

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Manage remote workspace connections in the daemon config",
	Long:  "Manage the workspace connections this daemon serves. Each connection holds one daemon token (td_) from that workspace's web UI; workspace_id/daemon_id are adopted from the server after registration, and agent instances are materialized here on desired delivery.",
}

var workspaceAddCmd = &cobra.Command{
	Use:   "add <td_token>",
	Short: "Add a workspace connection",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		token := strings.TrimSpace(args[0])
		if !strings.HasPrefix(token, "td_") {
			return fmt.Errorf("daemon token must carry the td_ prefix (got %q)", token)
		}
		name := strings.TrimSpace(workspaceAddName)
		if name == "" {
			name = agent.DeriveWorkspaceName(token)
		}

		path := resolvedConfigPath()
		raw, err := loadRawConfigForMutation(path)
		if err != nil {
			return err
		}
		workspaces := rawWorkspacesList(raw)
		for _, entry := range workspaces {
			if entry.Name == name {
				return fmt.Errorf("workspace %q already exists", name)
			}
		}
		workspaces = append(workspaces, agent.WorkspaceEntry{Name: name, Token: token})
		if err := writeRawWorkspaces(path, raw, workspaces); err != nil {
			return err
		}
		fmt.Printf("Workspace %q added (token=%s)\n", name, maskToken(token))
		fmt.Println("Instances created in that workspace's web UI are materialized here after registering.")
		fmt.Println("Restart agentd (or use the local console) for the change to take effect.")
		return nil
	},
}

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List workspace connections",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := agent.LoadGlobalConfig(resolvedConfigPath())
		if err != nil {
			return fmt.Errorf("load agentd config: %w", err)
		}
		if len(cfg.Workspaces) == 0 {
			fmt.Println("No workspaces configured. Add one: teammate-agentd workspace add <td_token> --name team-a")
			return nil
		}
		for _, w := range cfg.Workspaces {
			workspace := w.WorkspaceID
			if workspace == "" {
				workspace = "(pending registration)"
			}
			daemon := w.DaemonID
			if daemon == "" {
				daemon = "(pending registration)"
			}
			fmt.Printf("%s\t%s\t%s\t%s\tagents=%d\n", w.Name, maskToken(w.Token), workspace, daemon, len(w.Agents))
			for _, a := range w.Agents {
				identity := "pending"
				if a.AgentID != "" {
					identity = a.AgentID
				}
				fmt.Printf("  %s\t%s\t%s\t%s\n", a.Name, a.Provider, identity, a.PersonaKey)
			}
		}
		return nil
	},
}

var workspaceRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a workspace connection",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := strings.TrimSpace(args[0])
		path := resolvedConfigPath()
		raw, err := loadRawConfigForMutation(path)
		if err != nil {
			return err
		}
		workspaces := rawWorkspacesList(raw)
		kept := make([]agent.WorkspaceEntry, 0, len(workspaces))
		found := false
		for _, entry := range workspaces {
			if entry.Name == name {
				found = true
				continue
			}
			kept = append(kept, entry)
		}
		if !found {
			return fmt.Errorf("workspace %q not found", name)
		}
		if err := writeRawWorkspaces(path, raw, kept); err != nil {
			return err
		}
		fmt.Printf("Workspace %q removed\n", name)
		fmt.Println("Restart agentd (or use the local console) for the change to take effect.")
		return nil
	},
}

// rawWorkspacesList extracts the workspaces[] entries from the raw YAML map.
func rawWorkspacesList(raw map[string]interface{}) []agent.WorkspaceEntry {
	list, _ := raw["workspaces"].([]interface{})
	workspaces := make([]agent.WorkspaceEntry, 0, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		workspace := agent.WorkspaceEntry{}
		workspace.Name, _ = entry["name"].(string)
		workspace.Token, _ = entry["token"].(string)
		workspace.WorkspaceID, _ = entry["workspace_id"].(string)
		workspace.DaemonID, _ = entry["daemon_id"].(string)
		// The materialized agents (and their agent_ids — the restart
		// recovery path) must survive the round-trip.
		if agentList, ok := entry["agents"].([]interface{}); ok {
			for _, item := range agentList {
				agentRaw, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				materialized := agent.WorkspaceAgent{}
				materialized.Name, _ = agentRaw["name"].(string)
				materialized.Provider, _ = agentRaw["provider"].(string)
				materialized.PersonaKey, _ = agentRaw["persona_key"].(string)
				materialized.AgentID, _ = agentRaw["agent_id"].(string)
				if strings.TrimSpace(materialized.Name) != "" {
					workspace.Agents = append(workspace.Agents, materialized)
				}
			}
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces
}

// writeRawWorkspaces writes the workspaces[] back into the raw map and
// persists it, preserving unrelated keys.
func writeRawWorkspaces(path string, raw map[string]interface{}, workspaces []agent.WorkspaceEntry) error {
	if len(workspaces) == 0 {
		delete(raw, "workspaces")
		return writeRawConfig(path, raw)
	}
	list := make([]interface{}, 0, len(workspaces))
	for _, w := range workspaces {
		entry := map[string]interface{}{"name": w.Name, "token": w.Token}
		if len(w.Agents) > 0 {
			agents := make([]interface{}, 0, len(w.Agents))
			for _, a := range w.Agents {
				agentMap := map[string]interface{}{"name": a.Name, "provider": a.Provider}
				if a.PersonaKey != "" {
					agentMap["persona_key"] = a.PersonaKey
				}
				if a.AgentID != "" {
					agentMap["agent_id"] = a.AgentID
				}
				agents = append(agents, agentMap)
			}
			entry["agents"] = agents
		}
		if w.WorkspaceID != "" {
			entry["workspace_id"] = w.WorkspaceID
		}
		if w.DaemonID != "" {
			entry["daemon_id"] = w.DaemonID
		}
		list = append(list, entry)
	}
	raw["workspaces"] = list
	return writeRawConfig(path, raw)
}

func init() {
	workspaceAddCmd.Flags().StringVar(&workspaceAddName, "name", "", "local alias for the connection (recommended; derived from the token when omitted)")
	workspaceCmd.AddCommand(workspaceAddCmd)
	workspaceCmd.AddCommand(workspaceListCmd)
	workspaceCmd.AddCommand(workspaceRemoveCmd)
}
