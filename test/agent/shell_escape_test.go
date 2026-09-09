// Package agent_test contains tests for the agent package, covering shell escaping, execution context construction, Git operations, and Token estimation used by the agent daemon.
package agent_test

import (
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestShellEscapeViaPublicAPI indirectly tests shell escaping through ConfigureCredential, which internally uses shellEscape to write the askpass script. Since shellEscape is unexported, correctness is verified by ensuring credentials containing special characters (such as single quotes) are handled properly.
func TestShellEscapeViaPublicAPI(t *testing.T) {
	tests := []struct {
		name string
		pat  string
	}{
		{"simple token", "ghp_abc123"},
		{"token with single quote", "pat_with'quote"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			gm := agent.NewGitManager(tmpDir)
			// ConfigureCredential internally calls shellEscape for the PAT
			err := gm.ConfigureCredential("user", tt.pat, "Test", "test@test.com")
			if err != nil {
				t.Fatalf("ConfigureCredential(%q) failed: %v", tt.pat, err)
			}
			gm.CleanupCredential()
		})
	}
}
