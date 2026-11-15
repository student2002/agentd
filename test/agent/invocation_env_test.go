// invocation_env_test.go verifies the minimized environment for invocation
// tool processes: daemon identity/token variables, git credential injection
// variables, SSH agent sockets and cloud credentials are stripped; the user
// runtime (PATH, HOME, locale) is preserved.
package agent_test

import (
	"os"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if ok {
			out[name] = value
		}
	}
	return out
}

func TestBuildMinimalEnv(t *testing.T) {
	t.Setenv("TEAMMATE_MCP_TOKEN", "secret-token")
	t.Setenv("TEAMMATE_MCP_AGENT_ID", "agent-1")
	t.Setenv("TEAMMATE_GIT_PAT", "ghp_leaked")
	t.Setenv("GIT_ASKPASS", "/tmp/askpass.sh")
	t.Setenv("GIT_CEILING_DIRECTORIES", "/tmp")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("AZURE_CLIENT_SECRET", "azure-secret")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/tmp/gcp.json")
	t.Setenv("GITHUB_TOKEN", "ghp_env")
	t.Setenv("GITLAB_TOKEN", "glpat-env")
	t.Setenv("GH_TOKEN", "gh_env")
	t.Setenv("HF_TOKEN", "hf_env")

	env := agent.BuildMinimalEnv()
	values := envMap(env)

	for _, leaked := range []string{
		"TEAMMATE_MCP_TOKEN", "TEAMMATE_MCP_AGENT_ID", "TEAMMATE_GIT_PAT",
		"GIT_ASKPASS", "GIT_CEILING_DIRECTORIES", "SSH_AUTH_SOCK",
		"AWS_SECRET_ACCESS_KEY", "AZURE_CLIENT_SECRET", "GOOGLE_APPLICATION_CREDENTIALS",
		"GITHUB_TOKEN", "GITLAB_TOKEN", "GH_TOKEN", "HF_TOKEN",
	} {
		if _, present := values[leaked]; present {
			t.Fatalf("%s must be stripped from the invocation environment", leaked)
		}
	}
	if _, present := values["PATH"]; !present {
		t.Fatalf("PATH must be preserved for the coding CLI")
	}
	if home, hasHome := os.LookupEnv("HOME"); hasHome && values["HOME"] != home {
		t.Fatalf("HOME must be preserved (CLI logins depend on it)")
	}
}
