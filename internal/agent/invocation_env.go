// invocation_env.go builds the minimized environment for invocation tool
// processes: the user environment minus variables that would leak the
// daemon's credentials or the host's cloud credentials into the invoked
// session. HOME/USERPROFILE are deliberately kept so the coding CLIs can
// reach their local logins — an accepted residual exposure for trusted
// caller/target pairs.
package agent

import (
	"os"
	"strings"
)

// deniedEnvPrefixes and deniedEnvNames list the variables stripped from the
// invocation tool environment: the daemon's own identity/token variables
// (TEAMMATE_*), the git credential injection variables (GIT_*), and the
// common cloud provider credential variables.
var (
	deniedEnvPrefixes = []string{
		"TEAMMATE_",
		"GIT_",
		"AWS_",
		"AZURE_",
		"CLOUDSDK_",
		"GOOGLE_APPLICATION_",
	}
	deniedEnvNames = map[string]bool{
		"SSH_AUTH_SOCK": true,
		"SSH_AGENT_PID": true,
		"GH_TOKEN":      true,
		"GITHUB_TOKEN":  true,
		"GITLAB_TOKEN":  true,
		"HF_TOKEN":      true,
	}
)

// BuildMinimalEnv returns os.Environ() with the credential-bearing variables
// removed.
func BuildMinimalEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if deniedEnvNames[name] {
			continue
		}
		denied := false
		for _, prefix := range deniedEnvPrefixes {
			if strings.HasPrefix(name, prefix) {
				denied = true
				break
			}
		}
		if !denied {
			env = append(env, kv)
		}
	}
	return env
}
