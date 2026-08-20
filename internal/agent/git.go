// git.go 封装 Git 版本控制操作，为任务执行提供完整的仓库管理能力。
//
// 本文件提供 Agent Daemon 执行任务时所需的 Git 操作，主要包括：
//   - GitManager 结构体：封装工作目录级别的 Git 操作，支持凭据管理和标签追踪
//   - Clone / FetchAndCheckout：仓库克隆和分支检出，处理空仓库和分支不存在等边界情况
//   - ConfigureCredential / CleanupCredential：临时 askpass 脚本管理，安全注入 Git PAT
//   - PushBranch / PushTag / CommitAll：代码提交和推送操作
//   - TagNodeStart / ResetToNode：节点级别的标签创建和状态回退
//   - BranchName / NodeStartTag：分支和标签命名规范生成
//
// 分支命名规范：teammate/task-{taskID}
// 标签命名规范：teammate/task-{taskID}/node-{order}/attempt-{attempt}/start
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

// GitManager 封装 Git 操作，为任务执行提供仓库克隆、分支管理、
// 凭据配置、标签创建和代码重置等功能。
type GitManager struct {
	workDir     string
	env         []string // 额外环境变量（如 GIT_ASKPASS）
	askpassPath string
	clk         clock.Clock
}

// NewGitManager 创建一个新的 Git 管理器。
func NewGitManager(workDir string) *GitManager {
	return &GitManager{workDir: workDir, clk: clock.RealClock{}}
}

// Clone 将远程仓库克隆到工作目录。
// 如果工作目录已包含 Git 仓库则确保远程 origin 可用并 fetch。
// 克隆后确保 master 分支存在：远程没有 master 则从远程默认分支创建一个。
func (g *GitManager) Clone(repoURL, baseBranch string) error {
	if g.IsGitRepo() {
		// 仓库已存在——确保远程 origin 已配置且可 fetch
		return g.ensureRemoteOrigin(repoURL, baseBranch)
	}

	// 确保父目录存在
	if err := os.MkdirAll(g.workDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", g.workDir, err)
	}

	// 先尝试用 master 克隆
	args := []string{"clone", "--branch", baseBranch, repoURL, g.workDir}
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), g.env...)
	if err := cmd.Run(); err == nil {
		g.ensureGitignore()
		return nil
	}
	// 指定分支克隆失败可能是远程没有该分支，也可能是仓库根本不存在 / 无权限。
	// 后者由下面的不带 --branch 克隆捕获——若该次也失败，则原样返回错误。

	// 降级方案：不带 --branch 克隆（远程没有 master 或为空）
	args = []string{"clone", repoURL, g.workDir}
	cmd = exec.Command("git", args...)
	cmd.Env = append(os.Environ(), g.env...)
	if err := cmd.Run(); err != nil {
		// 克隆彻底失败（仓库不存在 / 无访问权限）——直接返回错误，不要降级到
		// initEmptyRepo。否则会在本地建一个空仓库，ConfigureCredential 注入的 PAT
		// 在后续 push origin master 时再次失败，错误被静默吞掉。
		return fmt.Errorf("git clone %s: %w", repoURL, err)
	}

	if !g.IsGitRepo() {
		return g.initEmptyRepo(repoURL, baseBranch)
	}

	// 确保 master 分支存在——缺失时从远程默认分支创建
	g.ensureGitignore()
	return g.ensureMasterBranch(baseBranch)
}

// initEmptyRepo 初始化一个全新的 Git 仓库并推送基础分支，用于远程仓库完全为空的情况。
func (g *GitManager) initEmptyRepo(repoURL, baseBranch string) error {
	// 清理不完整的克隆
	os.RemoveAll(g.workDir)
	if err := os.MkdirAll(g.workDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", g.workDir, err)
	}

	// 初始化新仓库
	cmd := exec.Command("git", "init")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %s: %w", string(out), err)
	}

	// 添加远程 origin
	cmd = exec.Command("git", "remote", "add", "origin", repoURL)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git remote add: %s: %w", string(out), err)
	}

	// 创建初始提交以确保基础分支存在
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

	// 将当前分支重命名为基础分支
	cmd = exec.Command("git", "branch", "-M", baseBranch)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -M %s: %s: %w", baseBranch, string(out), err)
	}

	// 将基础分支推送到 origin
	cmd = exec.Command("git", "push", "-u", "origin", baseBranch)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push origin %s: %s: %w", baseBranch, string(out), err)
	}

	return nil
}

// initBaseBranch 在已克隆但无分支的仓库中创建基础分支。
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

// ensureRemoteOrigin 确保已有仓库的远程 origin 配置正确、可 fetch，且 master 分支存在。
func (g *GitManager) ensureRemoteOrigin(repoURL, baseBranch string) error {
	// 检查远程 origin 是否存在
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		// 没有远程 origin——添加它
		cmd = exec.Command("git", "remote", "add", "origin", repoURL)
		cmd.Dir = g.workDir
		cmd.Env = append(os.Environ(), g.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git remote add origin %s: %s: %w", repoURL, string(out), err)
		}
	} else {
		// 远程 origin 存在——如果 URL 不同则更新
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

	// Fetch 以确保远程跟踪引用存在
	cmd = exec.Command("git", "fetch", "--tags", "origin")
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch origin: %s: %w", string(out), err)
	}

	// 确保 master 分支存在
	return g.ensureMasterBranch(baseBranch)
}

// ensureMasterBranch 确保 master 分支存在。
// 如果远程没有 master，则从远程默认分支（origin/HEAD）创建 master 并推送。
func (g *GitManager) ensureMasterBranch(baseBranch string) error {
	// 检查 origin/master 是否已存在
	cmd := exec.Command("git", "rev-parse", "--verify", "origin/"+baseBranch)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if cmd.Run() == nil {
		return nil // master 已存在于远程
	}

	// 查找远程默认分支作为起始点
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

	// 降级方案：选择第一个可用的远程分支
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
		// 完全没有远程分支——这是一个空仓库，由 initBaseBranch 处理
		return g.initBaseBranch(baseBranch)
	}

	// 从远程默认分支创建 master
	cmd = exec.Command("git", "checkout", "-b", baseBranch, startPoint)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout -b %s %s: %s: %w", baseBranch, startPoint, string(out), err)
	}

	// 将 master 推送到远程
	cmd = exec.Command("git", "push", "-u", "origin", baseBranch)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push origin %s: %s: %w", baseBranch, string(out), err)
	}

	return nil
}

// FetchAndCheckout 从远程拉取并检出（或创建）任务分支。
// 用于工作目录已包含 Git 仓库的情况。
func (g *GitManager) FetchAndCheckout(taskID int32, baseBranch string) error {
	branch := BranchName(taskID)
	remoteBranch := fmt.Sprintf("origin/%s", branch)

	// 从 origin 获取所有标签和分支
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

	// 检查远程任务分支是否已存在
	remoteBranchExists := false
	checkRemoteCmd := exec.Command("git", "rev-parse", "--verify", remoteBranch)
	checkRemoteCmd.Dir = g.workDir
	checkRemoteCmd.Env = append(os.Environ(), g.env...)
	if checkRemoteCmd.Run() == nil {
		remoteBranchExists = true
	}

	// 检查任务分支是否已存在于本地
	localBranchExists := false
	checkLocalCmd := exec.Command("git", "rev-parse", "--verify", branch)
	checkLocalCmd.Dir = g.workDir
	checkLocalCmd.Env = append(os.Environ(), g.env...)
	if checkLocalCmd.Run() == nil {
		localBranchExists = true
	}

	if localBranchExists {
		// 分支已存在于本地，检出并与远程同步
		cmd := exec.Command("git", "checkout", branch)
		cmd.Dir = g.workDir
		cmd.Env = append(os.Environ(), g.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git checkout %s: %s: %w", branch, string(out), err)
		}

		// 如果远程分支存在，将本地快进到与远程一致
		if remoteBranchExists {
			cmd = exec.Command("git", "merge", "--ff-only", remoteBranch)
			cmd.Dir = g.workDir
			cmd.Env = append(os.Environ(), g.env...)
			if _, err := cmd.CombinedOutput(); err != nil {
				// ff-only 失败（已分叉），重置到远程以确保一致
				cmd = exec.Command("git", "reset", "--hard", remoteBranch)
				cmd.Dir = g.workDir
				cmd.Env = append(os.Environ(), g.env...)
				if out, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("git reset --hard %s: %s: %w", remoteBranch, string(out), err)
				}
			}
		}
	} else if remoteBranchExists {
		// 远程分支存在但本地没有——从远程任务分支检出
		cmd := exec.Command("git", "checkout", "-b", branch, remoteBranch)
		cmd.Dir = g.workDir
		cmd.Env = append(os.Environ(), g.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git checkout -b %s %s: %s: %w", branch, remoteBranch, string(out), err)
		}
	} else {
		// 本地和远程任务分支都不存在——从 master 创建
		cmd := exec.Command("git", "checkout", "-b", branch, fmt.Sprintf("origin/%s", baseBranch))
		cmd.Dir = g.workDir
		cmd.Env = append(os.Environ(), g.env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git checkout -b %s origin/%s: %s: %w", branch, baseBranch, string(out), err)
		}
	}

	// 验证我们处于任务分支——绝不允许在基础分支上操作
	currentBranch, err := g.CurrentBranch()
	if err != nil {
		return fmt.Errorf("verify current branch: %w", err)
	}
	if currentBranch != branch {
		return fmt.Errorf("branch verification failed: expected %s but on %s — refusing to operate on base branch", branch, currentBranch)
	}

	return nil
}

// ConfigureCredential 配置 Git 凭据助手，使用临时 askpass 脚本处理认证。
func (g *GitManager) ConfigureCredential(username, pat, gitName, gitEmail string) error {
	if pat == "" {
		return nil
	}

	scriptPath, env, err := createAskPass(username, pat)
	if err != nil {
		return err
	}

	g.env = append(g.env, env...)

	// 保存脚本路径以备清理
	g.askpassPath = scriptPath

	// 设置 git 身份——代理必须配置 git_name/git_email
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

// SetGitConfig 在本地仓库中设置 Git 配置项。
func (g *GitManager) SetGitConfig(key, value string) error {
	cmd := exec.Command("git", "config", key, value)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config %s %s: %s: %w", key, value, string(out), err)
	}
	return nil
}

// CleanupCredential 删除临时 askpass 脚本。
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

// PushBranch 将当前分支推送到远程仓库。
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

// TagNodeStart 创建标记节点开始的标签，attempt 参数用于防止节点重新执行时的标签冲突。
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

// TagNodeComplete 创建标记节点完成的标签，与 TagNodeStart 对应，标记节点执行结束。
// attempt 参数用于防止节点重新执行时的标签冲突。
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

// PushTag 将标签推送到远程仓库。
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

// CreateTag 创建本地 Git 标签。
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

// tagExists 检查指定的 Git 标签是否存在于本地仓库中。
//
// 参数:
//   - tag: 要检查的标签名称
//
// 返回:
//   - bool: 标签存在返回 true，否则返回 false
func (g *GitManager) tagExists(tag string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", tag)
	cmd.Dir = g.workDir
	return cmd.Run() == nil
}

// ResetToNode 将工作树重置到指定节点开始时的状态，attempt 参数与 TagNodeStart 中使用的一致。
func (g *GitManager) ResetToNode(taskID int32, nodeOrder, attempt int) error {
	tag := NodeStartTag(taskID, nodeOrder, attempt)

	// 验证标签存在
	if !g.tagExists(tag) {
		return fmt.Errorf("tag %s does not exist", tag)
	}

	// 执行重置
	cmd := exec.Command("git", "reset", "--hard", tag)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git reset --hard %s: %s: %w", tag, string(out), err)
	}
	return nil
}

// SnapshotBeforeReject 在拒绝节点前创建快照标签。
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

// CommitAll 暂存所有变更并提交。
// 提交成功或无变更时返回 nil，真正的失败（如缺少 Git 身份、钩子、权限等）返回错误。
func (g *GitManager) CommitAll(message string) error {
	_, err := g.CommitAllWithResult(message)
	return err
}

// CommitAllWithResult 暂存所有变更并提交，返回是否实际创建了新提交。
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
	// 强制英文输出，避免 locale 导致 "nothing to commit" 检查失败
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

// HeadCommit 返回当前 HEAD 提交哈希。
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

// ensureGitignore 确保本地 Git exclude 包含 .teammate/ 排除规则，
// 防止框架注入的工具脚本被提交到仓库，同时不污染用户工作树。
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

// IsGitRepo 检查工作目录是否为 Git 仓库，仅检查 workDir 内的 .git 目录。
func (g *GitManager) IsGitRepo() bool {
	gitDir := filepath.Join(g.workDir, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return false
	}
	return info.IsDir() || info.Mode().IsRegular() // .git 可以是目录或文件（worktree）
}

// CurrentBranch 返回当前 Git 分支名称。
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

// BranchName 根据任务 ID 生成任务分支名称，格式为 "teammate/task-{taskID}"。
func BranchName(taskID int32) string {
	return fmt.Sprintf("teammate/task-%d", taskID)
}

// NodeStartTag 生成节点开始标签，格式为 "teammate/task-{taskID}/node-{order}/attempt-{attempt}/start"。
func NodeStartTag(taskID int32, nodeOrder, attempt int) string {
	return fmt.Sprintf("%s/node-%d/attempt-%d/start", BranchName(taskID), nodeOrder, attempt)
}

// NodeCompleteTag 生成节点完成标签，格式为 "teammate/task-{taskID}/node-{order}/attempt-{attempt}/complete"。
// 与 NodeStartTag 对应，标记节点执行结束的提交。
func NodeCompleteTag(taskID int32, nodeOrder, attempt int) string {
	return fmt.Sprintf("%s/node-%d/attempt-%d/complete", BranchName(taskID), nodeOrder, attempt)
}

// CurrentTime 返回用于标签的 Unix 时间戳。
func (g *GitManager) CurrentTime() int64 {
	return g.clk.Now().Unix()
}
