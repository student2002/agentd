// git.go encapsulates Git version control operations, providing full repository
// management capabilities for task execution.
//
// This file provides the Git operations required by the Agent Daemon when
// executing tasks, mainly including:
//   - GitManager struct: encapsulates workdir-level Git operations, supporting
//     credential management and tag tracking
//   - Clone / FetchAndCheckout: repository cloning and branch checkout, handling
//     edge cases such as empty repos and missing branches
//   - ConfigureCredential / CleanupCredential: temporary askpass script
//     management, safely injecting the Git PAT
//   - PushBranch / PushTag / CommitAll: code commit and push operations
//   - TagNodeStart / ResetToNode: node-level tag creation and state rollback
//   - BranchName / NodeStartTag: branch and tag naming convention generation
//
// Branch naming convention: teammate/task-{taskID}
// Tag naming convention:
// teammate/task-{taskID}/node-{order}/attempt-{attempt}/start
package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/teammate/agentd/internal/clock"
)

// GitManager encapsulates Git operations, providing repository cloning, branch
// management, credential configuration, tag creation, and code reset for task
// execution.
type GitManager struct {
	workDir     string
	env         []string // extra environment variables (e.g. GIT_ASKPASS)
	askpassPath string
	clk         clock.Clock
}

// NewGitManager creates a new Git manager.
func NewGitManager(workDir string) *GitManager {
	return &GitManager{workDir: workDir, clk: clock.RealClock{}}
}

// Clone clones the remote repository into the work directory.
// If the work directory already contains a Git repository, it ensures the remote
// origin is available and fetches.
// After cloning it ensures the master branch exists: if the remote has no
// master, one is created from the remote default branch.
func (g *GitManager) Clone(repoURL, baseBranch string) error {
	if g.IsGitRepo() {
		// Repository already exists — ensure the remote origin is configured and fetchable
		return g.ensureRemoteOrigin(repoURL, baseBranch)
	}

	// Ensure the parent directory exists
	if err := os.MkdirAll(g.workDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", g.workDir, err)
	}

	// First try to clone using master
	args := []string{"clone", "--branch", baseBranch, repoURL, g.workDir}
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), g.env...)
	if err := cmd.Run(); err == nil {
		g.ensureGitignore()
		return nil
	}
	// Cloning with a specified branch may fail because the remote lacks that
	// branch, or because the repository does not exist / no permission.
	// The latter is caught by the clone without --branch below — if that also
	// fails, the error is returned as-is.

	// Fallback: clone without --branch (the remote has no master or is empty)
	args = []string{"clone", repoURL, g.workDir}
	cmd = exec.Command("git", args...)
	cmd.Env = append(os.Environ(), g.env...)
	if err := cmd.Run(); err != nil {
		// Clone completely failed (repo does not exist / no access) — return the
		// error directly, do not fall back to initEmptyRepo. Otherwise an empty
		// repo would be created locally, and the PAT injected by
		// ConfigureCredential would fail again on the subsequent push origin
		// master, with the error silently swallowed.
		return fmt.Errorf("git clone %s: %w", repoURL, err)
	}

	if !g.IsGitRepo() {
		return g.initEmptyRepo(repoURL, baseBranch)
	}

	// Ensure the master branch exists — if missing, create it from the remote default branch
	g.ensureGitignore()
	return g.ensureMasterBranch(baseBranch)
}

// initEmptyRepo initializes a brand-new Git repository and pushes the base
// branch, used when the remote repository is completely empty.
func (g *GitManager) initEmptyRepo(repoURL, baseBranch string) error {
	// Clean up the incomplete clone
	os.RemoveAll(g.workDir)
	if err := os.MkdirAll(g.workDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", g.workDir, err)
	}

	// Initialize a new repository
	cmd := exec.Command("git", "init")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %s: %w", string(out), err)
	}

	// Add the remote origin
	cmd = exec.Command("git", "remote", "add", "origin", repoURL)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git remote add: %s: %w", string(out), err)
	}

	// Create an initial commit to ensure the base branch exists
	g.ensureGitignore()
	placeholder := filepath.Join(g.workDir, ".gitkeep")
	if err := os.WriteFile(placeholder, []byte(""), 0644); err != nil {
		return fmt.Errorf("write .gitkeep: %w", err)
	}

	cmd = exec.Command("git", "add", "-A")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %s: %w", string(out), err)
	}

	cmd = exec.Command("git", "commit", "-m", "teammate: initial commit")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %s: %w", string(out), err)
	}

	// Rename the current branch to the base branch
	cmd = exec.Command("git", "branch", "-M", baseBranch)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -M %s: %s: %w", baseBranch, string(out), err)
	}

	// Push the base branch to origin
	cmd = exec.Command("git", "push", "-u", "origin", baseBranch)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push origin %s: %s: %w", baseBranch, string(out), err)
	}

	return nil
}

// initBaseBranch creates the base branch in an already-cloned repository that
// has no branches.
func (g *GitManager) initBaseBranch(baseBranch string) error {
	cmd := exec.Command("git", "checkout", "-b", baseBranch)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout -b %s: %s: %w", baseBranch, string(out), err)
	}

	cmd = exec.Command("git", "push", "-u", "origin", baseBranch)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push origin %s: %s: %w", baseBranch, string(out), err)
	}

	return nil
}

// ensureRemoteOrigin ensures that an existing repository has the remote origin
// correctly configured and fetchable, and that the master branch exists.
func (g *GitManager) ensureRemoteOrigin(repoURL, baseBranch string) error {
	// Check whether the remote origin exists
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		// No remote origin — add it
		cmd = exec.Command("git", "remote", "add", "origin", repoURL)
		cmd.Dir = g.workDir
		cmd.Env = append(os.Environ(), g.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git remote add origin %s: %s: %w", repoURL, string(out), err)
		}
	} else {
		// Remote origin exists — update it if the URL differs
		currentURL := strings.TrimSpace(string(out))
		if currentURL != repoURL {
			cmd = exec.Command("git", "remote", "set-url", "origin", repoURL)
			cmd.Dir = g.workDir
			cmd.Env = append(os.Environ(), g.env...)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("git remote set-url origin %s: %s: %w", repoURL, string(out), err)
			}
		}
	}

	// Fetch to ensure the remote tracking refs exist
	cmd = exec.Command("git", "fetch", "--tags", "origin")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch origin: %s: %w", string(out), err)
	}

	// Ensure the master branch exists
	return g.ensureMasterBranch(baseBranch)
}

// ensureMasterBranch ensures the master branch exists.
// If the remote has no master, it creates master from the remote default branch
// (origin/HEAD) and pushes it.
func (g *GitManager) ensureMasterBranch(baseBranch string) error {
	// Check whether origin/master already exists
	cmd := exec.Command("git", "rev-parse", "--verify", "origin/"+baseBranch)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if cmd.Run() == nil {
		return nil // master already exists on the remote
	}

	// Find the remote default branch as the starting point
	startPoint := ""
	cmd = exec.Command("git", "rev-parse", "--abbrev-ref", "origin/HEAD")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err == nil {
		ref := strings.TrimSpace(string(out))
		if ref != "" {
			startPoint = ref
		}
	}

	// Fallback: pick the first available remote branch
	if startPoint == "" {
		cmd = exec.Command("git", "branch", "-r")
		cmd.Dir = g.workDir
		cmd.Env = append(os.Environ(), g.env...)
		if out, err := cmd.CombinedOutput(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "origin/HEAD") {
					startPoint = line
					break
				}
			}
		}
	}

	if startPoint == "" {
		// No remote branch at all — this is an empty repo, handled by initBaseBranch
		return g.initBaseBranch(baseBranch)
	}

	// Create master from the remote default branch
	cmd = exec.Command("git", "checkout", "-b", baseBranch, startPoint)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout -b %s %s: %s: %w", baseBranch, startPoint, string(out), err)
	}

	// Push master to the remote
	cmd = exec.Command("git", "push", "-u", "origin", baseBranch)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push origin %s: %s: %w", baseBranch, string(out), err)
	}

	return nil
}

// FetchAndCheckout fetches from the remote and checks out (or creates) the task
// branch.
// Used when the work directory already contains a Git repository.
func (g *GitManager) FetchAndCheckout(taskID int32, baseBranch string) error {
	branch := BranchName(taskID)
	remoteBranch := fmt.Sprintf("origin/%s", branch)

	// Fetch all tags and branches from origin
	cmds := [][]string{
		{"fetch", "--tags", "origin"},
	}

	for _, args := range cmds {
		cmd := exec.Command("git", args...)
		cmd.Dir = g.workDir
		cmd.Env = append(os.Environ(), g.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), string(out), err)
		}
	}

	// Check whether the remote task branch already exists
	remoteBranchExists := false
	checkRemoteCmd := exec.Command("git", "rev-parse", "--verify", remoteBranch)
	checkRemoteCmd.Dir = g.workDir
	checkRemoteCmd.Env = append(os.Environ(), g.env...)
	if checkRemoteCmd.Run() == nil {
		remoteBranchExists = true
	}

	// Check whether the task branch already exists locally
	localBranchExists := false
	checkLocalCmd := exec.Command("git", "rev-parse", "--verify", branch)
	checkLocalCmd.Dir = g.workDir
	checkLocalCmd.Env = append(os.Environ(), g.env...)
	if checkLocalCmd.Run() == nil {
		localBranchExists = true
	}

	if localBranchExists {
		// Branch already exists locally; check it out and sync with the remote
		cmd := exec.Command("git", "checkout", branch)
		cmd.Dir = g.workDir
		cmd.Env = append(os.Environ(), g.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git checkout %s: %s: %w", branch, string(out), err)
		}

		// If the remote branch exists, fast-forward the local branch to match the remote
		if remoteBranchExists {
			cmd = exec.Command("git", "merge", "--ff-only", remoteBranch)
			cmd.Dir = g.workDir
			cmd.Env = append(os.Environ(), g.env...)
			if _, err := cmd.CombinedOutput(); err != nil {
				// ff-only failed (already diverged); reset to the remote to ensure consistency
				cmd = exec.Command("git", "reset", "--hard", remoteBranch)
				cmd.Dir = g.workDir
				cmd.Env = append(os.Environ(), g.env...)
				if out, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("git reset --hard %s: %s: %w", remoteBranch, string(out), err)
				}
			}
		}
	} else if remoteBranchExists {
		// Remote branch exists but not locally — check out from the remote task branch
		cmd := exec.Command("git", "checkout", "-b", branch, remoteBranch)
		cmd.Dir = g.workDir
		cmd.Env = append(os.Environ(), g.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git checkout -b %s %s: %s: %w", branch, remoteBranch, string(out), err)
		}
	} else {
		// Neither local nor remote task branch exists — create from master
		cmd := exec.Command("git", "checkout", "-b", branch, fmt.Sprintf("origin/%s", baseBranch))
		cmd.Dir = g.workDir
		cmd.Env = append(os.Environ(), g.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git checkout -b %s origin/%s: %s: %w", branch, baseBranch, string(out), err)
		}
	}

	// Verify that we are on the task branch — never allow operating on the base branch
	currentBranch, err := g.CurrentBranch()
	if err != nil {
		return fmt.Errorf("verify current branch: %w", err)
	}
	if currentBranch != branch {
		return fmt.Errorf("branch verification failed: expected %s but on %s — refusing to operate on base branch", branch, currentBranch)
	}

	return nil
}

// ConfigureCredential configures the Git credential helper, using a temporary
// askpass script to handle authentication.
func (g *GitManager) ConfigureCredential(username, pat, gitName, gitEmail string) error {
	if pat == "" {
		return nil
	}

	scriptPath, env, err := createAskPass(username, pat)
	if err != nil {
		return err
	}

	g.env = append(g.env, env...)

	// Save the script path for later cleanup
	g.askpassPath = scriptPath

	// Set the git identity — the agent must configure git_name/git_email
	if gitEmail == "" {
		return fmt.Errorf("agent git_email is required for git operations")
	}
	_ = g.SetGitConfig("user.email", gitEmail)

	if gitName == "" {
		return fmt.Errorf("agent git_name is required for git operations")
	}
	_ = g.SetGitConfig("user.name", gitName)

	return nil
}

// SetGitConfig sets a Git config item in the local repository.
func (g *GitManager) SetGitConfig(key, value string) error {
	cmd := exec.Command("git", "config", key, value)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config %s %s: %s: %w", key, value, string(out), err)
	}
	return nil
}

// CleanupCredential deletes the temporary askpass script.
func (g *GitManager) CleanupCredential() {
	if g.askpassPath != "" {
		os.Remove(g.askpassPath)
		g.askpassPath = ""
	}
}

func createAskPass(username, pat string) (string, []string, error) {
	ext := ".sh"
	content := "#!/bin/sh\ncase \"$1\" in *sername*|*Username*|*username*) printf '%s' \"$TEAMMATE_GIT_USERNAME\";; *) printf '%s' \"$TEAMMATE_GIT_PAT\";; esac\n"
	if runtime.GOOS == "windows" {
		ext = ".cmd"
		content = "@echo off\r\npowershell -NoProfile -ExecutionPolicy Bypass -Command \"if ($args[0] -match 'sername') { [Console]::Write($env:TEAMMATE_GIT_USERNAME) } else { [Console]::Write($env:TEAMMATE_GIT_PAT) }\" -- %*\r\n"
	}

	tmpFile, err := os.CreateTemp("", "teammate-askpass-*"+ext)
	if err != nil {
		return "", nil, fmt.Errorf("create temp askpass script: %w", err)
	}
	scriptPath := tmpFile.Name()
	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		os.Remove(scriptPath)
		return "", nil, fmt.Errorf("write askpass script: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(scriptPath)
		return "", nil, fmt.Errorf("close askpass script: %w", err)
	}
	if err := os.Chmod(scriptPath, 0700); err != nil {
		os.Remove(scriptPath)
		return "", nil, fmt.Errorf("chmod askpass script: %w", err)
	}

	env := []string{
		fmt.Sprintf("GIT_ASKPASS=%s", scriptPath),
		"GIT_TERMINAL_PROMPT=0",
		fmt.Sprintf("TEAMMATE_GIT_USERNAME=%s", username),
		fmt.Sprintf("TEAMMATE_GIT_PAT=%s", pat),
	}
	return scriptPath, env, nil
}

// PushBranch pushes the current branch to the remote repository.
func (g *GitManager) PushBranch(taskID int32) error {
	cmd := exec.Command("git", "push", "-u", "origin", BranchName(taskID))
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push %s: %s: %w", BranchName(taskID), string(out), err)
	}
	return nil
}

// TagNodeStart creates a tag marking the start of a node; the attempt parameter
// prevents tag conflicts when a node is re-executed.
func (g *GitManager) TagNodeStart(taskID int32, nodeOrder, attempt int) error {
	tag := NodeStartTag(taskID, nodeOrder, attempt)
	cmd := exec.Command("git", "tag", tag)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git tag %s: %s: %w", tag, string(out), err)
	}
	return nil
}

// TagNodeComplete creates a tag marking the completion of a node, corresponding
// to TagNodeStart and marking the end of node execution.
// The attempt parameter prevents tag conflicts when a node is re-executed.
func (g *GitManager) TagNodeComplete(taskID int32, nodeOrder, attempt int) error {
	tag := NodeCompleteTag(taskID, nodeOrder, attempt)
	cmd := exec.Command("git", "tag", tag)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git tag %s: %s: %w", tag, string(out), err)
	}
	return nil
}

// PushTag pushes a tag to the remote repository.
func (g *GitManager) PushTag(tag string) error {
	cmd := exec.Command("git", "push", "origin", tag)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push tag %s: %s: %w", tag, string(out), err)
	}
	return nil
}

// CreateTag creates a local Git tag.
func (g *GitManager) CreateTag(tag string) error {
	cmd := exec.Command("git", "tag", tag)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git tag %s: %s: %w", tag, string(out), err)
	}
	return nil
}

// tagExists checks whether the specified Git tag exists in the local repository.
//
// Parameters:
//   - tag: the name of the tag to check
//
// Returns:
//   - bool: true if the tag exists, false otherwise
func (g *GitManager) tagExists(tag string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", tag)
	cmd.Dir = g.workDir
	return cmd.Run() == nil
}

// ResetToNode resets the working tree to the state at the start of the specified
// node; the attempt parameter matches the one used in TagNodeStart.
func (g *GitManager) ResetToNode(taskID int32, nodeOrder, attempt int) error {
	tag := NodeStartTag(taskID, nodeOrder, attempt)

	// Verify the tag exists
	if !g.tagExists(tag) {
		return fmt.Errorf("tag %s does not exist", tag)
	}

	// Perform the reset
	cmd := exec.Command("git", "reset", "--hard", tag)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git reset --hard %s: %s: %w", tag, string(out), err)
	}
	return nil
}

// SnapshotBeforeReject creates a snapshot tag before rejecting a node.
func (g *GitManager) SnapshotBeforeReject(taskID int32) (string, error) {
	tag := fmt.Sprintf("%s/before-reject-%d", BranchName(taskID), g.CurrentTime())
	cmd := exec.Command("git", "tag", tag)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git tag %s: %s: %w", tag, string(out), err)
	}
	return tag, nil
}

// CommitAll stages all changes and commits.
// Returns nil on success or when there are no changes; genuine failures (such
// as missing Git identity, hooks, permissions) return an error.
func (g *GitManager) CommitAll(message string) error {
	_, err := g.CommitAllWithResult(message)
	return err
}

// CommitAllWithResult stages all changes and commits, returning whether a new
// commit was actually created.
func (g *GitManager) CommitAllWithResult(message string) (bool, error) {
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git add -A: %s: %w", string(out), err)
	}

	cmd = exec.Command("git", "commit", "-m", message)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	// Force English output to avoid the "nothing to commit" check failing due to locale
	cmd.Env = append(cmd.Env, "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := string(out)
		if strings.Contains(output, "nothing to commit") || strings.Contains(output, "no changes added to commit") {
			return false, nil
		}
		return false, fmt.Errorf("git commit: %s: %w", output, err)
	}
	return true, nil
}

// HeadCommit returns the current HEAD commit hash.
func (g *GitManager) HeadCommit() (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %s: %w", string(out), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ensureGitignore ensures the local Git exclude contains a .teammate/ exclusion
// rule, preventing framework-injected tool scripts from being committed to the
// repository without polluting the user's working tree.
func (g *GitManager) ensureGitignore() {
	excludePath := filepath.Join(g.workDir, ".git", "info", "exclude")
	content := ""
	if data, err := os.ReadFile(excludePath); err == nil {
		content = string(data)
	}
	if strings.Contains(content, ".teammate/") {
		return
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += ".teammate/\n"
	_ = os.WriteFile(excludePath, []byte(content), 0644)
}

// IsGitRepo checks whether the work directory is a Git repository, checking only
// the .git directory inside workDir.
func (g *GitManager) IsGitRepo() bool {
	gitDir := filepath.Join(g.workDir, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return false
	}
	return info.IsDir() || info.Mode().IsRegular() // .git can be a directory or a file (worktree)
}

// CurrentBranch returns the current Git branch name.
func (g *GitManager) CurrentBranch() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %s: %w", string(out), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// BranchName generates the task branch name from the task ID, in the format
// "teammate/task-{taskID}".
func BranchName(taskID int32) string {
	return fmt.Sprintf("teammate/task-%d", taskID)
}

// NodeStartTag generates the node start tag, in the format
// "teammate/task-{taskID}/node-{order}/attempt-{attempt}/start".
func NodeStartTag(taskID int32, nodeOrder, attempt int) string {
	return fmt.Sprintf("%s/node-%d/attempt-%d/start", BranchName(taskID), nodeOrder, attempt)
}

// NodeCompleteTag generates the node complete tag, in the format
// "teammate/task-{taskID}/node-{order}/attempt-{attempt}/complete".
// Corresponding to NodeStartTag, it marks the commit at the end of node
// execution.
func NodeCompleteTag(taskID int32, nodeOrder, attempt int) string {
	return fmt.Sprintf("%s/node-%d/attempt-%d/complete", BranchName(taskID), nodeOrder, attempt)
}

// CurrentTime returns the Unix timestamp used for tags.
func (g *GitManager) CurrentTime() int64 {
	return g.clk.Now().Unix()
}
