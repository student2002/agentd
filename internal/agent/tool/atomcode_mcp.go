// atomcode_mcp.go bridges the materialized MCP config (workDir/.teammate/
// mcp.json) to the project-root .mcp.json that AtomCode loads natively. The
// bridged file carries the daemon token, so it must never enter Git history:
// the ignore is verified with git check-ignore before every launch and the
// file is removed after the run.
package tool

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// managedMCPServerName is the manager MCP server entry the materializer
// writes into every bridged config; its presence identifies the file as ours.
const managedMCPServerName = "teammate-memory"

// managedLeftover reports whether an existing .mcp.json is a crashed run's
// leftover of our own: it carries the manager MCP server entry and names an
// executable of ours. Token rotation or a rebuilt binary may change the
// details, so byte equality would misclassify our own leftover as foreign.
func managedLeftover(data []byte, exeName string) bool {
	var cfg struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false
	}
	srv, ok := cfg.MCPServers[managedMCPServerName]
	if !ok || srv.Command == "" {
		return false
	}
	return filepath.Base(srv.Command) == exeName
}

// bridgeAtomCodeMCPConfig copies configPath to <workDir>/.mcp.json, excludes
// it from Git and verifies the exclusion actually applies. A pre-existing
// .mcp.json that is not our own leftover aborts the run: no merge/restore
// logic, a foreign file is the project's own and stays untouched. Returns the
// cleanup func removing the bridged file.
func bridgeAtomCodeMCPConfig(workDir, configPath string) (func(), error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read mcp config: %w", err)
	}
	target := filepath.Join(workDir, ".mcp.json")
	if existing, err := os.ReadFile(target); err == nil {
		exe, exeErr := os.Executable()
		if exeErr != nil {
			return nil, fmt.Errorf("resolve own executable: %w", exeErr)
		}
		if !managedLeftover(existing, filepath.Base(exe)) {
			return nil, fmt.Errorf("project already has a .mcp.json with different content; refusing to overwrite — delete %s to let the daemon manage it", target)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read existing .mcp.json: %w", err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return nil, fmt.Errorf("write .mcp.json: %w", err)
	}
	cleanup := func() { _ = os.Remove(target) }
	if err := ignoreGitPath(workDir, ".mcp.json"); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

// ignoreGitPath adds path (relative to workDir) to the repository's local
// info/exclude — resolved through rev-parse --git-path so linked worktrees
// hit their own exclude file — and verifies with git check-ignore. A failed
// verification is an error, never best-effort: the path stays untracked-safe
// or the caller must not proceed.
func ignoreGitPath(workDir, path string) error {
	out, err := exec.Command("git", "-C", workDir, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return fmt.Errorf("resolve git exclude path: %w", err)
	}
	excludePath := strings.TrimSpace(string(out))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(workDir, excludePath)
	}
	if err := appendExcludeEntry(excludePath, path); err != nil {
		return err
	}
	verify := exec.Command("git", "-C", workDir, "check-ignore", "-q", "--", path)
	if err := verify.Run(); err != nil {
		return fmt.Errorf("verify git ignores %s: %w", path, err)
	}
	return nil
}

// appendExcludeEntry appends one entry to the exclude file unless present.
func appendExcludeEntry(excludePath, entry string) error {
	content := ""
	if data, err := os.ReadFile(excludePath); err == nil {
		content = string(data)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read git exclude %s: %w", excludePath, err)
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.WriteFile(excludePath, []byte(content+entry+"\n"), 0o644); err != nil {
		return fmt.Errorf("write git exclude %s: %w", excludePath, err)
	}
	return nil
}
