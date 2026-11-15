//go:build !linux

// toolwrapper_other.go stubs the tool wrapper on platforms that bind tool
// trees to the daemon by other means (Windows Job Object).
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/teammate/agentd/internal/agent/process"
)

func toolWrapperCommand() *cobra.Command {
	return &cobra.Command{
		Use:    process.ToolWrapperArgName,
		Short:  "internal: not supported on this platform",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("%s is only available on linux", process.ToolWrapperArgName)
		},
	}
}
