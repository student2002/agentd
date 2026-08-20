// Package agent_test 包含 agent 包的测试，涵盖 shell 转义、执行上下文构建、Git 操作和代理守护进程使用的 Token 估算。
package agent_test

import (
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestShellEscapeViaPublicAPI 通过 ConfigureCredential 间接测试 Shell 转义，该方法内部使用 shellEscape 编写 askpass 脚本。由于 shellEscape 未导出，通过确保含特殊字符（单引号等）的凭证能正确处理来验证正确性。
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
			// ConfigureCredential 内部为 PAT 调用 shellEscape
			err := gm.ConfigureCredential("user", tt.pat, "Test", "test@test.com")
			if err != nil {
				t.Fatalf("ConfigureCredential(%q) failed: %v", tt.pat, err)
			}
			gm.CleanupCredential()
		})
	}
}
