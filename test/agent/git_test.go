// Package agent_test 包含 agent 包的测试，涵盖 shell 转义、
// 执行上下文构建、Git 操作以及 agent 守护进程使用的 token 估算。
package agent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestBranchName 验证 BranchName 返回预期的分支名称格式 "teammate/task-{taskID}"。
func TestBranchName(t *testing.T) {
	got := agent.BranchName(123)
	want := "teammate/task-123"
	if got != want {
		t.Errorf("BranchName = %q, want %q", got, want)
	}
}

// TestNodeStartTag 验证 NodeStartTag 返回预期的格式
// "teammate/task-{taskID}/node-{order}/attempt-{attempt}/start"，用于在每个节点执行尝试开始时跟踪 Git 状态。
func TestNodeStartTag(t *testing.T) {
	got := agent.NodeStartTag(123, 2, 1)
	want := "teammate/task-123/node-2/attempt-1/start"
	if got != want {
		t.Errorf("NodeStartTag = %q, want %q", got, want)
	}
}

// TestCurrentTime 验证 CurrentTime 返回一个合理的 Unix 时间戳（非零，大于10亿）。
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

// --- 扩展的 Git 测试（从 internal/agent/git_internal_test.go 迁移而来） ---

// setupTestRepo 创建一个裸 Git 仓库作为"远程仓库"并返回其路径。
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

// runGit 在指定目录执行 git 命令。测试失败时终止测试并在错误消息中包含 git 输出。
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

// TestGitManager_IsGitRepo 验证 IsGitRepo 对克隆的仓库返回 true，对不存在的目录返回 false。
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

// TestGitManager_Clone 验证 Clone 在管理工作目录创建远程仓库的完整工作副本。
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

// TestGitManager_Clone_SkipIfAlreadyRepo 验证如果工作目录已经是有效的 Git 仓库则跳过克隆，
// 允许幂等的初始化。
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

// TestGitManager_FetchAndCheckout 验证 FetchAndCheckout 从基础分支创建并检出新分支。
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

// TestGitManager_FetchAndCheckout_SyncsFromRemote 验证共享同一远程仓库的两个克隆能在 fetch 后看到彼此的提交。
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

// TestGitManager_FetchAndCheckout_SyncsLocalWithRemote 验证本地分支被重置为匹配远程状态（不仅是追加）。
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

// TestGitManager_CommitAll 验证 CommitAll 暂存所有更改并使用给定消息创建提交。
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

// TestGitManager_CommitAllWithResult 报告是否实际创建了一次提交。
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

// TestGitManager_TagNodeStart 验证 TagNodeStart 创建用于跟踪节点执行状态的 Git 标签，且标签可检索。
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

// TestGitManager_ResetToNode 验证 ResetToNode 可以重置工作树到节点的起始标签，丢弃未提交的更改。
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

// TestGitManager_SnapshotBeforeReject 验证 CreateSnapshot 在驳回操作回滚更改前创建当前状态的提交。
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

// TestGitManager_ConfigureCredential 验证 ConfigureCredential 使用正确的 PAT 和用户信息编写 askpass 脚本。
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

	// 验证 git config 已设置
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

// TestGitManager_ConfigureCredential_EmptyPAT 验证 ConfigureCredential 在空 PAT 字符串时返回错误。
func TestGitManager_ConfigureCredential_EmptyPAT(t *testing.T) {
	tmpDir := t.TempDir()
	runGit(t, tmpDir, "init")
	gm := agent.NewGitManager(tmpDir)

	if err := gm.ConfigureCredential("user", "", "", ""); err != nil {
		t.Fatalf("ConfigureCredential with empty PAT should not error: %v", err)
	}
}

// TestGitManager_SetGitConfig 验证 SetGitConfig 将用户名和电子邮件写入仓库的 Git 配置。
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
