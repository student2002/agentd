// atomcode_mcp_test.go verifies the AtomCode .mcp.json bridge against real
// Git repositories: the bridged file is git-ignored (never appears in status,
// so it cannot be committed), removed on cleanup, a foreign .mcp.json aborts
// the run untouched, the bridge's own crash leftover is rewritten, linked
// worktrees resolve their own exclude file, and a non-repo workdir aborts.
package tool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs one git command in dir, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.local",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// gitRepoWithCommit creates a repo with one committed file so the worktree is
// clean before the bridge runs.
func gitRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("demo"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	gitIn(t, dir, "add", "README.md")
	gitIn(t, dir, "commit", "-m", "init")
	return dir
}

func gitStatus(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitIn(t, dir, "status", "--porcelain"))
}

func mcpConfigFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	// The materialized config names the manager MCP server with THIS process's
	// own executable — exactly what a real daemon writes (the ownership
	// fingerprint the leftover check looks for).
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	content := fmt.Sprintf(`{"mcpServers":{%q:{"type":"stdio","command":%q,"args":["mcp"],"env":{"TEAMMATE_MCP_TOKEN":"td_secret"}}}}`,
		managedMCPServerName, exe)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path, content
}

func TestBridgeAtomCodeMCPConfigIgnoresAndCleansUp(t *testing.T) {
	workDir := gitRepoWithCommit(t)
	configPath, content := mcpConfigFixture(t)

	cleanup, err := bridgeAtomCodeMCPConfig(workDir, configPath)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}

	bridged, err := os.ReadFile(filepath.Join(workDir, ".mcp.json"))
	if err != nil {
		t.Fatalf("bridged file missing: %v", err)
	}
	if string(bridged) != content {
		t.Fatalf("bridged content mismatch: %q", bridged)
	}
	if status := gitStatus(t, workDir); status != "" {
		t.Fatalf("bridged .mcp.json leaked into git status: %q", status)
	}

	cleanup()
	if _, err := os.Stat(filepath.Join(workDir, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("cleanup left .mcp.json behind: %v", err)
	}
	if status := gitStatus(t, workDir); status != "" {
		t.Fatalf("post-cleanup git status dirty: %q", status)
	}
}

func TestBridgeRejectsForeignMCPConfig(t *testing.T) {
	workDir := gitRepoWithCommit(t)
	configPath, _ := mcpConfigFixture(t)

	foreign := `{"mcpServers":{"project-owned":{"command":"other.exe"}}}`
	if err := os.WriteFile(filepath.Join(workDir, ".mcp.json"), []byte(foreign), 0o644); err != nil {
		t.Fatalf("seed foreign config: %v", err)
	}

	if _, err := bridgeAtomCodeMCPConfig(workDir, configPath); err == nil {
		t.Fatalf("bridge over a foreign .mcp.json must fail")
	}
	kept, err := os.ReadFile(filepath.Join(workDir, ".mcp.json"))
	if err != nil || string(kept) != foreign {
		t.Fatalf("foreign config altered: %v %q", err, kept)
	}
}

func TestBridgeRewritesOwnCrashLeftover(t *testing.T) {
	workDir := gitRepoWithCommit(t)
	configPath, content := mcpConfigFixture(t)

	cleanup, err := bridgeAtomCodeMCPConfig(workDir, configPath)
	if err != nil {
		t.Fatalf("first bridge: %v", err)
	}
	_ = cleanup // 模拟崩溃残留:不走清理,文件留在工作区

	cleanup2, err := bridgeAtomCodeMCPConfig(workDir, configPath)
	if err != nil {
		t.Fatalf("re-bridge over identical leftover: %v", err)
	}
	cleanup2()
	if strings.TrimSpace(gitStatus(t, workDir)) != "" {
		t.Fatalf("git status dirty after re-bridge")
	}
	_ = content
}

func TestBridgeWorktreeUsesOwnExcludeFile(t *testing.T) {
	main := gitRepoWithCommit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, main, "worktree", "add", wt, "-b", "wt-branch")

	configPath, _ := mcpConfigFixture(t)
	cleanup, err := bridgeAtomCodeMCPConfig(wt, configPath)
	if err != nil {
		t.Fatalf("bridge in worktree: %v", err)
	}
	defer cleanup()
	if strings.TrimSpace(gitStatus(t, wt)) != "" {
		t.Fatalf("worktree status dirty: %q", gitStatus(t, wt))
	}
}

func TestBridgeAbortsOutsideGitRepo(t *testing.T) {
	workDir := t.TempDir()
	configPath, _ := mcpConfigFixture(t)

	cleanup, err := bridgeAtomCodeMCPConfig(workDir, configPath)
	if err == nil {
		cleanup()
		t.Fatalf("bridge outside a git repo must fail, not launch")
	}
	if _, err := os.Stat(filepath.Join(workDir, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("failed bridge left .mcp.json behind: %v", err)
	}
}

// TestManagedLeftoverFingerprint pins the ownership rule: the manager entry
// naming our own executable is a crashed leftover (rewritable even when the
// token rotated); anything else is the project's file and must not be
// touched.
func TestManagedLeftoverFingerprint(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve executable: %v", err)
	}
	exeName := filepath.Base(exe)

	ours := fmt.Sprintf(`{"mcpServers":{%q:{"command":%q,"env":{"TEAMMATE_MCP_TOKEN":"td_rotated_after_restart"}}}}`,
		managedMCPServerName, exe)
	if !managedLeftover([]byte(ours), exeName) {
		t.Fatalf("token-rotated leftover must be recognized as ours")
	}

	disguised := fmt.Sprintf(`{"mcpServers":{%q:{"command":"C:\somewhere\else\agentd.exe"}}}`,
		managedMCPServerName)
	if managedLeftover([]byte(disguised), exeName) {
		t.Fatalf("same manager key but a foreign command must not be treated as ours")
	}

	foreign := `{"mcpServers":{"project-own":{"command":"tool.exe"}}}`
	if managedLeftover([]byte(foreign), exeName) {
		t.Fatalf("project-owned config must not be treated as ours")
	}

	if managedLeftover([]byte("not-json"), exeName) {
		t.Fatalf("unparsable content must fail closed")
	}
}
