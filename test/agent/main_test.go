package agent_test

import (
	"os"
	"testing"
)

// TestMain pins the git author and committer identity for the whole package.
// GitManager runs git with os.Environ(), so without these variables every
// commit inherits identity from the host's global git config, which is absent
// on clean CI containers and fails with "Author identity unknown".
func TestMain(m *testing.M) {
	for key, value := range map[string]string{
		"GIT_AUTHOR_NAME":     "Test Agent",
		"GIT_AUTHOR_EMAIL":    "test@agent.local",
		"GIT_COMMITTER_NAME":  "Test Agent",
		"GIT_COMMITTER_EMAIL": "test@agent.local",
	} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
