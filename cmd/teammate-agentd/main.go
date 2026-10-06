// main.go is the entry point of teammate-agentd (the machine daemon managing
// local agent instances).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/teammate/agentd/internal/agent"
)

var (
	cfgPath   string
	outputFmt string
)

var rootCmd = &cobra.Command{
	Use:   "teammate-agentd",
	Short: "Teammate Agent Daemon - one daemon managing local agent instances",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := agent.LoadGlobalConfig(resolvedConfigPath())
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		// The local control frontend runs by default; generate its
		// credentials on first start so the supervisor sees them.
		if err := cfg.EnsureLocalCredentials(resolvedConfigPath()); err != nil {
			return err
		}
		if err := agent.ValidateGlobalConfig(cfg); err != nil {
			return fmt.Errorf("validate config: %w", err)
		}

		supervisor := agent.NewSupervisor(cfg, resolvedConfigPath(), agent.SupervisorOptions{})
		return supervisor.Run(context.Background())
	},
}

func main() {
	rootCmd.PersistentFlags().StringVarP(&cfgPath, "config", "c", "", "daemon config file path (default: ~/.teammate/config.yaml)")
	rootCmd.PersistentFlags().StringVarP(&outputFmt, "output", "o", "table", "output format for config commands: table, json, yaml")

	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(daemonCmd)
	rootCmd.AddCommand(agentCmd)
	rootCmd.AddCommand(workspaceCmd)
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("teammate-agentd v" + agent.AgentdVersion)
		},
	})

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func resolvedConfigPath() string {
	return cfgPath
}
