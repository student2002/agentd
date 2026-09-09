// Package agent_test contains tests for the agent package, covering shell
// escaping, execution context construction, Git operations, and the token
// estimation used by the agent daemon.
package agent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestBranchName verifies that BranchName returns the expected branch name
// format "teammate/task-{taskID}".
func TestBranchName(t *testing.T) {
	got := agent.BranchName(123)
	want := "teammate/task-123"
	if got != want {
		t.Errorf("BranchName = %q, want %q", got, want)
	}
}

// TestNodeStartTag verifies that NodeStartTag returns the expected format
// "teammate/task-{taskID}/node-{order}/attempt-{attempt}/start", used to track
// Git state at the start of each node execution attempt.
func TestNodeStartTag(t *testing.T) {
	got := agent.NodeStartTag(123, 2, 1)
	want := "teammate/task-123/node-2/attempt-1/start"
	if got != want {
		t.Errorf("NodeStartTag = %q, want %q", got, want)
	}
}

// TestCurrentTime verifies that CurrentTime returns a reasonable Unix
// timestamp (non-zero and greater than one billion).
func TestCurrentTime(t *testing.T) {
	gm := agent.NewGitManager(t.TempDir())
	ts := gm.CurrentTime()
	if ts == 0 {
		t.Error("CurrentTime() returned 0, expected a real timestamp")
	}
	if ts < 1000000000 {
		t.Errorf("CurrentTime() = %d, seems too small for a unix timestamp", ts)
	}
}

// --- Expanded Git tests (migrated from internal/agent/git_internal_test.go) ---

// setupTestRepo creates a bare Git repository that acts as a "remote
// repository" and returns its path.
func setupTestRepo(t *testing.T) (remoteDir string, cleanup func()) {
	t.Helper()

	tmpDir := t.TempDir()
	remoteDir = filepath.Join(tmpDir, "remote.git")

	if err := os.MkdirAll(remoteDir, 0755); err != nil {
		t.Fatalf("mkdir remote: %v", err)
	}
	runGit(t, remoteDir, "init", "--bare")
	runGit(t, remoteDir, "config", "receive.denyCurrentBranch", "ignore")

	seedDir := filepath.Join(tmpDir, "seed")
	if err := os.MkdirAll(seedDir, 0755); err != nil {
		t.Fatalf("mkdir seed: %v", err)
	}
	runGit(t, seedDir, "init")
	runGit(t, seedDir, "config", "user.email", "test@test.com")
	runGit(t, seedDir, "config", "user.name", "Test")
	runGit(t, seedDir, "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runGit(t, seedDir, "add", ".")
	runGit(t, seedDir, "commit", "-m", "initial commit")
	runGit(t, seedDir, "remote", "add", "origin", remoteDir)
	runGit(t, seedDir, "push", "-u", "origin", "master")

	cleanup = func() { os.RemoveAll(tmpDir) }
	return remoteDir, cleanup
}

// runGit runs a git command in the specified directory. On test failure it
// terminates the test and includes the git output in the error message.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %s: %v", strings.Join(args, " "), dir, string(out), err)
	}
}

// TestGitManager_IsGitRepo verifies that IsGitRepo returns true for a cloned
// repository and false for a non-existent directory.
func TestGitManager_IsGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	gm := agent.NewGitManager(tmpDir)
	if gm.IsGitRepo() {
		t.Error("empty dir should not be a git repo")
	}

	runGit(t, tmpDir, "init")
	if !gm.IsGitRepo() {
		t.Error("after git init, IsGitRepo should return true")
	}
}

// TestGitManager_Clone verifies that Clone creates a full working copy of the
// remote repository in the managed working directory.
func TestGitManager_Clone(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir := filepath.Join(t.TempDir(), "workspace")
	gm := agent.NewGitManager(workDir)

	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Clone failed: %v", err)
	}
	if !gm.IsGitRepo() {
		t.Error("after Clone, IsGitRepo should return true")
	}
	if _, err := os.Stat(filepath.Join(workDir, "README.md")); err != nil {
		t.Errorf("README.md should exist after clone: %v", err)
	}

	branch, err := gm.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch failed: %v", err)
	}
	if branch != "master" {
		t.Errorf("current branch = %q, want %q", branch, "master")
	}
}

// TestGitManager_Clone_SkipIfAlreadyRepo verifies that clone is skipped if the
// working directory is already a valid Git repository, allowing idempotent
// initialization.
func TestGitManager_Clone_SkipIfAlreadyRepo(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir := filepath.Join(t.TempDir(), "workspace")
	gm := agent.NewGitManager(workDir)

	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("first Clone failed: %v", err)
	}

	markerPath := filepath.Join(workDir, "marker.txt")
	if err := os.WriteFile(markerPath, []byte("test"), 0644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("second Clone failed: %v", err)
	}

	if _, err := os.Stat(markerPath); err != nil {
		t.Error("marker file should still exist after second Clone (should have been skipped)")
	}
}

// TestGitManager_FetchAndCheckout verifies that FetchAndCheckout creates and
// checks out a new branch from the base branch.
func TestGitManager_FetchAndCheckout(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir := filepath.Join(t.TempDir(), "workspace")
	gm := agent.NewGitManager(workDir)
	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	if err := gm.FetchAndCheckout(456, "master"); err != nil {
		t.Fatalf("FetchAndCheckout failed: %v", err)
	}

	branch, err := gm.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch failed: %v", err)
	}
	wantBranch := "teammate/task-456"
	if branch != wantBranch {
		t.Errorf("current branch = %q, want %q", branch, wantBranch)
	}

	if err := gm.FetchAndCheckout(456, "master"); err != nil {
		t.Fatalf("second FetchAndCheckout failed: %v", err)
	}
}

// TestGitManager_FetchAndCheckout_SyncsFromRemote verifies that two clones
// sharing the same remote repository can see each other's commits after a
// fetch.
func TestGitManager_FetchAndCheckout_SyncsFromRemote(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir1 := filepath.Join(t.TempDir(), "workspace1")
	gm1 := agent.NewGitManager(workDir1)
	if err := gm1.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Node1 Clone failed: %v", err)
	}
	if err := gm1.FetchAndCheckout(789, "master"); err != nil {
		t.Fatalf("Node1 FetchAndCheckout failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir1, "node1-work.txt"), []byte("node1 was here"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gm1.CommitAll("node1: add work"); err != nil {
		t.Fatalf("Node1 CommitAll failed: %v", err)
	}
	if err := gm1.PushBranch(789); err != nil {
		t.Fatalf("Node1 PushBranch failed: %v", err)
	}

	workDir2 := filepath.Join(t.TempDir(), "workspace2")
	gm2 := agent.NewGitManager(workDir2)
	if err := gm2.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Node2 Clone failed: %v", err)
	}
	if err := gm2.FetchAndCheckout(789, "master"); err != nil {
		t.Fatalf("Node2 FetchAndCheckout failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workDir2, "node1-work.txt"))
	if err != nil {
		t.Fatalf("Node2 should see node1-work.txt: %v", err)
	}
	if string(data) != "node1 was here" {
		t.Errorf("node1-work.txt content = %q, want %q", string(data), "node1 was here")
	}
}

// TestGitManager_FetchAndCheckout_SyncsLocalWithRemote verifies that the local
// branch is reset to match the remote state (not merely appended).
func TestGitManager_FetchAndCheckout_SyncsLocalWithRemote(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir := filepath.Join(t.TempDir(), "workspace")
	gm := agent.NewGitManager(workDir)
	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Clone failed: %v", err)
	}
	if err := gm.FetchAndCheckout(999, "master"); err != nil {
		t.Fatalf("FetchAndCheckout failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "initial.txt"), []byte("initial"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gm.CommitAll("initial work"); err != nil {
		t.Fatalf("CommitAll failed: %v", err)
	}
	if err := gm.PushBranch(999); err != nil {
		t.Fatalf("PushBranch failed: %v", err)
	}

	otherDir := filepath.Join(t.TempDir(), "other")
	gmOther := agent.NewGitManager(otherDir)
	if err := gmOther.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Other Clone failed: %v", err)
	}
	if err := gmOther.FetchAndCheckout(999, "master"); err != nil {
		t.Fatalf("Other FetchAndCheckout failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "remote-update.txt"), []byte("from other agent"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gmOther.CommitAll("other agent work"); err != nil {
		t.Fatalf("Other CommitAll failed: %v", err)
	}
	if err := gmOther.PushBranch(999); err != nil {
		t.Fatalf("Other PushBranch failed: %v", err)
	}

	if err := gm.FetchAndCheckout(999, "master"); err != nil {
		t.Fatalf("FetchAndCheckout sync failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "remote-update.txt"))
	if err != nil {
		t.Fatalf("should see remote-update.txt after sync: %v", err)
	}
	if string(data) != "from other agent" {
		t.Errorf("remote-update.txt content = %q, want %q", string(data), "from other agent")
	}
}

// TestGitManager_CommitAll verifies that CommitAll stages all changes and
// creates a commit with the given message.
func TestGitManager_CommitAll(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir := filepath.Join(t.TempDir(), "workspace")
	gm := agent.NewGitManager(workDir)
	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workDir, "newfile.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gm.CommitAll("test: add newfile"); err != nil {
		t.Fatalf("CommitAll failed: %v", err)
	}

	cmd := exec.Command("git", "log", "--oneline", "-1")
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	if !strings.Contains(string(out), "test: add newfile") {
		t.Errorf("commit message not found in log: %s", string(out))
	}
}

// TestGitManager_CommitAllWithResult reports whether a commit was actually
// created.
func TestGitManager_CommitAllWithResult(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir := filepath.Join(t.TempDir(), "workspace")
	gm := agent.NewGitManager(workDir)
	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	committed, err := gm.CommitAllWithResult("test: no changes")
	if err != nil {
		t.Fatalf("CommitAllWithResult no changes failed: %v", err)
	}
	if committed {
		t.Fatal("expected no commit when workspace has no changes")
	}

	if err := os.WriteFile(filepath.Join(workDir, "result.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	committed, err = gm.CommitAllWithResult("test: add result")
	if err != nil {
		t.Fatalf("CommitAllWithResult with changes failed: %v", err)
	}
	if !committed {
		t.Fatal("expected commit when workspace has changes")
	}
}

// TestGitManager_TagNodeStart verifies that TagNodeStart creates a Git tag used
// to track node execution state and that the tag can be retrieved.
func TestGitManager_TagNodeStart(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir := filepath.Join(t.TempDir(), "workspace")
	gm := agent.NewGitManager(workDir)
	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	if err := gm.TagNodeStart(789, 1, 1); err != nil {
		t.Fatalf("TagNodeStart failed: %v", err)
	}

	cmd := exec.Command("git", "tag", "-l", "teammate/task-789/node-1/attempt-1/start")
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git tag list failed: %v", err)
	}
	got := strings.TrimSpace(string(out))
	want := "teammate/task-789/node-1/attempt-1/start"
	if got != want {
		t.Errorf("tag = %q, want %q", got, want)
	}
}

// TestGitManager_ResetToNode verifies that ResetToNode can reset the working
// tree to the node's starting tag, discarding uncommitted changes.
func TestGitManager_ResetToNode(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir := filepath.Join(t.TempDir(), "workspace")
	gm := agent.NewGitManager(workDir)
	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Clone failed: %v", err)
	}
	if err := gm.TagNodeStart(9001, 1, 1); err != nil {
		t.Fatalf("TagNodeStart failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "changed.txt"), []byte("changed"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := gm.CommitAll("test: make change"); err != nil {
		t.Fatalf("CommitAll failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workDir, "changed.txt")); err != nil {
		t.Fatalf("changed.txt should exist before reset: %v", err)
	}

	if err := gm.ResetToNode(9001, 1, 1); err != nil {
		t.Fatalf("ResetToNode failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workDir, "changed.txt")); err == nil {
		t.Error("changed.txt should not exist after reset")
	}
}

// TestGitManager_SnapshotBeforeReject verifies that CreateSnapshot commits the
// current state before a reject operation rolls back changes.
func TestGitManager_SnapshotBeforeReject(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir := filepath.Join(t.TempDir(), "workspace")
	gm := agent.NewGitManager(workDir)
	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	tag, err := gm.SnapshotBeforeReject(42)
	if err != nil {
		t.Fatalf("SnapshotBeforeReject failed: %v", err)
	}
	if !strings.HasPrefix(tag, "teammate/task-42/before-reject-") {
		t.Errorf("tag = %q, want prefix teammate/task-42/before-reject-", tag)
	}
	if strings.HasSuffix(tag, "-0") {
		t.Errorf("tag ends with -0, currentTime() bug: tag = %q", tag)
	}
}

// TestGitManager_ConfigureCredential verifies that ConfigureCredential writes
// an askpass script with the correct PAT and user information.
func TestGitManager_ConfigureCredential(t *testing.T) {
	remoteDir, cleanup := setupTestRepo(t)
	defer cleanup()

	workDir := filepath.Join(t.TempDir(), "workspace")
	gm := agent.NewGitManager(workDir)
	if err := gm.Clone(remoteDir, "master"); err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	if err := gm.ConfigureCredential("testuser", "testpat123", "Test Agent", "test@agent.local"); err != nil {
		t.Fatalf("ConfigureCredential failed: %v", err)
	}

	// Verify that git config has been set
	cmd := exec.Command("git", "config", "user.name")
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git config user.name failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "Test Agent" {
		t.Errorf("user.name = %q, want %q", strings.TrimSpace(string(out)), "Test Agent")
	}

	gm.CleanupCredential()
}

// TestGitManager_ConfigureCredential_EmptyPAT verifies that ConfigureCredential
// returns an error when given an empty PAT string.
func TestGitManager_ConfigureCredential_EmptyPAT(t *testing.T) {
	tmpDir := t.TempDir()
	runGit(t, tmpDir, "init")
	gm := agent.NewGitManager(tmpDir)

	if err := gm.ConfigureCredential("user", "", "", ""); err != nil {
		t.Fatalf("ConfigureCredential with empty PAT should not error: %v", err)
	}
}

// TestGitManager_SetGitConfig verifies that SetGitConfig writes username and
// email to the repository's Git configuration.
func TestGitManager_SetGitConfig(t *testing.T) {
	tmpDir := t.TempDir()
	runGit(t, tmpDir, "init")
	gm := agent.NewGitManager(tmpDir)

	if err := gm.SetGitConfig("user.name", "TestUser"); err != nil {
		t.Fatalf("SetGitConfig failed: %v", err)
	}

	cmd := exec.Command("git", "config", "user.name")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git config user.name failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "TestUser" {
		t.Errorf("user.name = %q, want %q", strings.TrimSpace(string(out)), "TestUser")
	}
}
