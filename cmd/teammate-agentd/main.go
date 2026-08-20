// main.go 是 teammate-agentd（Agent 守护进程）的入口。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/teammate/agentd/internal/agent"
)

var (
	cfgPath   string
	profile   string
	outputFmt string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "teammate-agentd",
		Short: "Teammate Agent Daemon - lightweight agent runtime",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := agent.LoadConfig(resolvedConfigPath())
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if err := agent.ValidateConfig(cfg); err != nil {
				return fmt.Errorf("validate config: %w", err)
			}

			d, err := agent.NewDaemonWithOptions(cfg, agent.DaemonOptions{
				Profile:    profile,
				ConfigPath: resolvedConfigPath(),
			})
			if err != nil {
				return fmt.Errorf("init daemon: %w", err)
			}
			return d.Run()
		},
	}

	rootCmd.PersistentFlags().StringVarP(&cfgPath, "config", "c", "", "agent daemon config file path (default: ~/.teammate/config.yaml)")
	rootCmd.PersistentFlags().StringVar(&profile, "profile", "", "agent daemon config profile (loads ~/.teammate/config-{profile}.yaml)")
	rootCmd.PersistentFlags().StringVarP(&outputFmt, "output", "o", "table", "output format for config commands: table, json, yaml")

	rootCmd.AddCommand(configCmd)
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
	if cfgPath != "" {
		return cfgPath
	}
	if profile == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".teammate", "config-"+profile+".yaml")
	}
	return filepath.Join(home, ".teammate", "config-"+profile+".yaml")
}
