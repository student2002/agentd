// executor.go 实现任务执行器，负责使用编码工具执行已认领的节点。
//
// 本文件是 Agent Daemon 中最核心的执行模块，包含以下功能：
//   - 任务执行流程编排（准备工作区 → 构建上下文 → 调用工具 → 上报结果）
//   - Git 工作区管理（克隆、分支创建、start/end tag）
//   - 编码工具选择与调用（Claude Code / OpenClaw / OpenCode）
//   - 执行日志实时上报（通过 SSE + Redis 缓冲）
//   - 中断处理（接收 task:interrupt 事件，SIGTERM→SIGKILL 进程组）
//   - 会话恢复（重启后恢复执行上下文）
//   - 检查点提交（定期 commit 当前进度）
//   - 磁盘配额检查和 Token 用量上报
//
// TaskExecutor 是执行器的主结构体，协调 Git、工具、日志等子模块。
// 执行流程：克隆仓库 → 创建特性分支 → 打 start tag → 注入上下文 →
// 调用编码工具 → 实时上报日志 → commit + push → 打 end tag → 上报用量。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/teammate/agentd/internal/agent/tool"
)

// TaskExecutor 负责使用编码工具执行已认领的节点，管理 Git 工作区、
// 会话恢复、检查点提交和中断处理。
type TaskExecutor struct {
	cfg      *Config
	client   *Client
	agentID  string
	git      *GitManager
	stopCh   chan struct{}
	observer ExecutionObserver

	mu          sync.Mutex
	currentTool tool.Tool
	cancelFunc  context.CancelFunc
	currentMu   sync.Mutex
	running     bool
	taskID      int32
	node        TaskNode

	sessionID   string // 当前 Claude Code 会话 ID，用于 --resume
	lastWorkDir string // 上次工作目录，用于会话失效检测

	// nodeProjectID is the projectID of the running node, captured at Execute
	// entry. Intervention turns reuse it for completeness; the turn makes no
	// server call, so it is informational — but capturing it keeps the local
	// intervention state symmetric with the autonomous Execute path.
	nodeProjectID string

	// interrupted is set by Interrupt before cancelling the execution context.
	// It tells Execute's err branch to skip reportFailure (ManualIntervention),
	// partial commit, and notifyExecutionFailed — Interrupt owns interrupt
	// reporting (ReportInterrupt + interrupted git tag). Without this gate the
	// err branch double-reports an interrupted node as manual_intervention.
	interrupted atomic.Bool

	// softInterrupted is set by SoftInterrupt (server-unaware local takeover)
	// before cancelling the execution context. Like interrupted, it makes the
	// Execute err-branch skip reportFailure/ManualIntervention, partial commit,
	// and notifyExecutionFailed — but unlike interrupted it does NOT create an
	// interrupted git tag, push, or ReportInterrupt. The node stays in_progress
	// on the server while a human works locally.
	softInterrupted atomic.Bool

	// toolFactoryForTest, when non-nil, replaces the real coding-tool selector
	// with a fake tool. Test-only seam; never set in production.
	toolFactoryForTest func() TestTool

	// logBuffer, when set, captures desensitized output lines for the local
	// control API (recent + SSE streaming). Data source: this agentd's own
	// onOutput only — never cross-workspace/project. Degrades to a no-op when
	// nil (local control disabled).
	logBuffer *LogBuffer
}

type ExecutionObserver interface {
	OnExecutionStarted(session LocalExecutionSession)
	OnExecutionCompleted(taskID int32, nodeID string)
	OnExecutionInterrupted(taskID int32, nodeID string)
	OnExecutionFailed(taskID int32, nodeID string, err error)
	OnToolStatusChanged(provider string, status string, err error)
}

// NewTaskExecutor 创建一个新的任务执行器。
//
// 参数：
//   - cfg: 守护进程配置
//   - client: Server 通信客户端
//   - agentID: 代理 ID
//
// 返回：
//   - *TaskExecutor: 初始化完成的执行器实例
func NewTaskExecutor(cfg *Config, client *Client, agentID string) *TaskExecutor {
	return NewTaskExecutorWithObserver(cfg, client, agentID, nil)
}

func NewTaskExecutorWithObserver(cfg *Config, client *Client, agentID string, observer ExecutionObserver) *TaskExecutor {
	return &TaskExecutor{
		cfg:      cfg,
		client:   client,
		agentID:  agentID,
		observer: observer,
		stopCh:   make(chan struct{}),
	}
}

// Execute 执行一个已认领的节点，包括 Git 工作区初始化、上下文构建、编码工具调用和结果上报。
//
// 参数：
//   - taskID: 任务 ID
//   - node: 要执行的节点信息
//   - projectID: 项目 ID
func (e *TaskExecutor) Execute(taskID int32, node TaskNode, projectID string) {
	log.Printf("[executor] starting node %s (%s) for task %d", node.ID, node.Name, taskID)

	e.currentMu.Lock()
	e.running = true
	e.taskID = taskID
	e.node = node
	e.currentMu.Unlock()
	e.interrupted.Store(false)
	e.softInterrupted.Store(false)

	defer func() {
		e.currentMu.Lock()
		e.running = false
		e.currentMu.Unlock()
	}()

	// 为本次执行创建可取消的上下文
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.cancelFunc = cancel
	e.mu.Unlock()
	defer cancel()

	// 2. 获取任务详情以获取 projectID 和仓库信息
	task := Task{
		ID:        taskID,
		Title:     node.Name,
		ProjectID: projectID,
	}
	projectRepoURL := ""
	if projectID != "" {
		if fetchedTask, err := e.client.GetTask(context.Background(), projectID, taskID); err == nil && fetchedTask != nil {
			task = *fetchedTask
			task.ProjectID = projectID // 即使 API 未返回也确保 projectID 已设置
		}
		if project, err := e.client.GetProject(context.Background(), e.cfg.Workspace.ID, projectID); err == nil && project != nil {
			projectRepoURL = strings.TrimSpace(project.RepoURL)
		} else {
			log.Printf("[executor] warning: failed to fetch project git config: %v", err)
		}
	}

	// 1. 创建隔离的工作目录：{Root}/{workspaceID}/{projectID}/{taskID}/{agentID}
	// 确保不同项目和代理的任务相互隔离
	projectDir := "no-project"
	if task.ProjectID != "" {
		projectDir = task.ProjectID
	}
	workDir := filepath.Join(e.cfg.Workspace.Root, e.agentID, e.cfg.Workspace.ID, projectDir, fmt.Sprintf("%d", taskID))
	if err := os.MkdirAll(workDir, 0755); err != nil {
		log.Printf("[executor] failed to create workdir: %v", err)
		e.notifyExecutionFailed(taskID, node.ID, err)
		e.reportFailure(taskID, node.ID, err)
		return
	}
	e.notifyExecutionStarted(taskID, node, workDir)

	// Capture the node's projectID so intervention turns (which reuse the
	// executor's captured state without re-running Execute) have it available.
	e.mu.Lock()
	e.nodeProjectID = projectID
	e.mu.Unlock()

	// 执行前检查磁盘配额
	if err := e.checkDiskQuota(workDir); err != nil {
		log.Printf("[executor] disk quota check failed: %v", err)
		e.notifyExecutionFailed(taskID, node.ID, err)
		e.reportFailure(taskID, node.ID, err)
		return
	}

	// 3. 初始化 Git 工作区
	e.git = NewGitManager(workDir)
	gitReady := false

	// Git 是否为必需：项目配了 repo_url，或存在 git 凭据时，都视为必需。
	// 这样克隆/凭据失败会作为致命错误终止执行并上报，而不是静默降级为无 Git 继续执行
	// （否则会进入"克隆可选仓库失败→log 后继续→后续 push origin master 失败"的链路，错误被吞掉）。
	gitRequired := projectRepoURL != ""
	if task.ProjectID != "" {
		var err error
		gitReady, err = e.initGitWorkspace(workDir, taskID, task.ProjectID, projectRepoURL, gitRequired)
		if err != nil {
			log.Printf("[executor] git workspace initialization failed: %v", err)
			e.notifyExecutionFailed(taskID, node.ID, err)
			e.reportFailure(taskID, node.ID, err)
			return
		}
	}
	// 如果没有 projectID，则在无 Git 的情况下工作——workDir 是一个全新的空目录

	// 执行完成后清理 Git 凭据（askpass 脚本）
	if e.git != nil {
		defer e.git.CleanupCredential()
	}

	attempt := int(node.RejectCount) + 1

	gitStartHead := ""
	if gitReady && e.git != nil && e.git.IsGitRepo() {
		if head, err := e.git.HeadCommit(); err == nil {
			gitStartHead = head
		} else {
			log.Printf("[executor] warning: failed to read git HEAD before execution: %v", err)
		}
	}

	if gitReady {
		// 标记节点开始——使用节点的 SortOrder，而非从名称解析
		nodeOrder := int(node.SortOrder)
		if nodeOrder <= 0 {
			nodeOrder = ParseNodeOrder(node.Name)
		}
		if err := e.git.TagNodeStart(taskID, nodeOrder, attempt); err != nil {
			log.Printf("[executor] warning: failed to tag node start: %v", err)
		}
		// 向 Server 上报 Git 分支名称，以便前端显示
		branch := BranchName(taskID)
		if err := e.client.ReportGitBranch(context.Background(), taskID, branch); err != nil {
			log.Printf("[executor] warning: failed to report git branch: %v", err)
		}
	}

	// 4. 选择编码工具
	t := e.selectTool()
	e.mu.Lock()
	e.currentTool = t
	e.mu.Unlock()
	e.notifyToolStatus(t.Name(), LocalToolConnected, nil)

	// 如果可用，设置会话恢复（Claude Code 和 AtomCode 支持）
	if claudeTool, ok := t.(*tool.ClaudeTool); ok {
		if e.sessionID != "" && e.lastWorkDir == workDir {
			claudeTool.SetResumeSession(e.sessionID)
			log.Printf("[executor] resuming Claude session %s", e.sessionID)
		}
		claudeTool.SetSessionCallback(func(sid string) {
			e.mu.Lock()
			e.sessionID = sid
			e.lastWorkDir = workDir
			e.mu.Unlock()
			log.Printf("[executor] captured Claude session ID: %s", sid)
		})
	} else if atomTool, ok := t.(*tool.AtomCodeTool); ok {
		// AtomCode 会话按目录作用域：上一轮已在本工作目录执行过即可续接（传 -c），
		// 而非依赖解析出的 session_id（AtomCode 输出不含可解析的 session id）。
		if e.lastWorkDir == workDir {
			atomTool.SetContinueSession(true)
			log.Printf("[executor] continuing AtomCode session in %s", workDir)
		}
	}

	capabilities, err := MaterializeAgentCapabilities(ctx, e.client, e.cfg, workDir, t.Name())
	if err != nil {
		log.Printf("[executor] warning: failed to prepare agent capabilities: %v", err)
		capabilities = CapabilityInjection{
			PromptCapabilities: PromptCapabilities{IncludeSkills: true, IncludeMCP: true},
		}
	} else {
		log.Printf("[executor] prepared capabilities for %s: skills=%d mcp_servers=%d", t.Name(), capabilities.SkillCount, capabilities.MCPServerCount)
	}

	// 5. 使用上下文注入层级构建执行上下文
	// 恢复 Claude 会话时使用简化上下文（--resume 保留了之前的推理）
	isResume := e.sessionID != "" && e.lastWorkDir == workDir
	prompt, err := e.buildPromptWithClient(taskID, node, task, isResume, capabilities.PromptCapabilities)
	if err != nil {
		e.notifyExecutionFailed(taskID, node.ID, err)
		e.reportFailure(taskID, node.ID, fmt.Errorf("build prompt: %w", err))
		return
	}

	// 6. 打印完整注入上下文，方便调试
	log.Printf("[prompt] ====== Injected Prompt (task=%d, node=%s) ======", taskID, node.Name)
	log.Printf("[prompt]\n%s", prompt)
	log.Printf("[prompt] ====== End Prompt (len=%d) ======", len(prompt))

	// 7. 使用编码工具执行，同时进行定期检查点提交
	log.Printf("[executor] running %s with prompt len=%d", t.Name(), len(prompt))

	// 启动检查点协程——定期提交工作，防止工具进程崩溃时丢失未提交的更改
	checkpointDone := make(chan struct{})
	go func() {
		defer close(checkpointDone)
		checkpointInterval := 3 * time.Minute
		if envInterval := os.Getenv("TEAMMATE_CHECKPOINT_INTERVAL"); envInterval != "" {
			if d, err := time.ParseDuration(envInterval); err == nil {
				checkpointInterval = d
			}
		}
		ticker := time.NewTicker(checkpointInterval)
		defer ticker.Stop()

		checkpointCount := 0
		for {
			select {
			case <-ticker.C:
				if e.git != nil && e.git.IsGitRepo() {
					checkpointCount++
					commitMsg := fmt.Sprintf("teammate: checkpoint %d for %s", checkpointCount, node.Name)
					if err := e.git.CommitAll(commitMsg); err != nil {
						log.Printf("[executor] checkpoint commit failed: %v", err)
					} else {
						log.Printf("[executor] checkpoint %d committed for node %s", checkpointCount, node.Name)
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// 启动超时检测协程——超时后通知前端，不中断 Claude 进程，由用户决定是否中止
	timeoutDone := make(chan struct{})
	go func() {
		defer close(timeoutDone)
		timeout := 30 * time.Minute
		if envTimeout := os.Getenv("TEAMMATE_EXECUTION_TIMEOUT"); envTimeout != "" {
			if d, err := time.ParseDuration(envTimeout); err == nil {
				timeout = d
			}
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case <-timer.C:
			log.Printf("[executor] node %s execution exceeded %v, notifying user", node.Name, timeout)
			warning := fmt.Sprintf("⚠️ 节点执行已超过 %v，可能需要人工介入。您可以在任务详情页中断此任务。", timeout)
			if sendErr := e.client.SendMessageWithType(context.Background(), taskID, node.ID, "system", warning); sendErr != nil {
				log.Printf("[executor] failed to send timeout warning: %v", sendErr)
			}
		case <-ctx.Done():
			// 执行已完成或被中断，无需超时通知
		}
	}()

	result, err := t.Execute(ctx, workDir, prompt, capabilities.ToolOptions, func(line string) {
		// 实时输出——脱敏并记录日志
		safeLine := desensitizeOutputLine(line)
		log.Printf("[output] %s", safeLine)
		// 捕获到本地日志缓冲区（供 local control API 读取/SSE 推送）
		if e.logBuffer != nil {
			e.logBuffer.Append(LogLine{TaskID: taskID, NodeID: node.ID, Line: safeLine})
		}
		// 将脱敏后的日志发送到服务器
		if sendErr := e.client.SendMessage(context.Background(), taskID, node.ID, safeLine); sendErr != nil {
			log.Printf("[executor] failed to send message: %v", sendErr)
		}
	})

	// 取消上下文以停止检查点协程，然后等待其完成
	cancel()
	<-checkpointDone
	<-timeoutDone

	if err != nil {
		log.Printf("[executor] tool execution failed: %v", err)
		if e.interrupted.Load() || e.softInterrupted.Load() {
			// Interrupt (server-aware) already reported via ReportInterrupt + tag.
			// SoftInterrupt (server-unaware) intentionally keeps the node
			// in_progress while a human takes over locally. In both cases the
			// err-branch must NOT call reportFailure/ManualIntervention, partial
			// commit, or notifyExecutionFailed — that would double-report or
			// leak the local intervention to the server.
			return
		}
		e.notifyToolStatus(t.Name(), LocalToolDisconnected, err)
		// 提交部分工作成果
		if e.git != nil && e.git.IsGitRepo() {
			e.git.CommitAll(fmt.Sprintf("teammate: partial work for %s (failed)", node.Name))
			_ = e.pushIfNeeded(taskID, node, attempt)
		}
		e.reportFailure(taskID, node.ID, err)
		e.notifyExecutionFailed(taskID, node.ID, err)
		return
	}

	// 从结果中捕获会话 ID（作为回调未触发时的备份）
	if result.SessionID != "" {
		e.mu.Lock()
		e.sessionID = result.SessionID
		e.lastWorkDir = workDir
		e.mu.Unlock()
		log.Printf("[executor] captured Claude session ID from result: %s", result.SessionID)
	}

	// 记录本工作目录已执行过（会话按目录作用域）：供同任务下一节点判断是否续接（-c/--resume）。
	// 不同任务使用不同 workdir，因此天然隔离、不会串会话。
	e.mu.Lock()
	e.lastWorkDir = workDir
	e.mu.Unlock()

	// 7. 提交更改并推送到远程仓库
	pushFailed := false
	commitFailed := false
	gitChanged := false
	if gitReady && e.git != nil && e.git.IsGitRepo() {
		commitMsg := fmt.Sprintf("teammate: completed %s\n\nNode: %s\nTask: %d", node.Name, node.Name, taskID)
		committed, err := e.git.CommitAllWithResult(commitMsg)
		if err != nil {
			log.Printf("[executor] ERROR: git commit failed: %v", err)
			commitFailed = true
		} else if committed {
			gitChanged = true
		} else if gitStartHead != "" {
			if head, headErr := e.git.HeadCommit(); headErr == nil && head != gitStartHead {
				gitChanged = true
			}
		}
		if !commitFailed && !gitChanged {
			log.Printf("[executor] ERROR: no git changes produced for completed node %s", node.Name)
			commitFailed = true
		}
		if !commitFailed {
			// 标记节点完成——使用节点的 SortOrder，与 TagNodeStart 对应
			nodeOrder := int(node.SortOrder)
			if nodeOrder <= 0 {
				nodeOrder = ParseNodeOrder(node.Name)
			}
			if err := e.git.TagNodeComplete(taskID, nodeOrder, attempt); err != nil {
				log.Printf("[executor] warning: failed to tag node complete: %v", err)
			}
			if err := e.pushIfNeeded(taskID, node, attempt); err != nil {
				log.Printf("[executor] ERROR: failed to push after completion: %v", err)
				pushFailed = true
			}
		}
	}

	// 8. 上报 Token 用量
	if result.TotalTokens > 0 {
		usage := TokenUsageRequest{
			InputTokens:  result.InputTokens,
			OutputTokens: result.OutputTokens,
			TotalTokens:  result.TotalTokens,
		}
		if err := e.client.ReportTokenUsage(context.Background(), taskID, node.ID, e.agentID, usage); err != nil {
			log.Printf("[executor] failed to report token usage: %v", err)
		}
	}

	// 9. 检查代理是否需要人工输入
	if strings.Contains(result.Output, "<needs_input>") {
		log.Printf("[executor] agent requests human input for node %s", node.Name)
		comment := extractNeedsInputComment(result.Output)
		if comment != "" {
			if e.shouldPostNeedsInputComment(context.Background(), taskID, node.ID, comment) {
				if err := e.client.PostNodeComment(context.Background(), taskID, node.ID, "", "question", comment); err != nil {
					log.Printf("[executor] failed to post comment: %v", err)
				}
			} else {
				log.Printf("[executor] skipped duplicate needs_input comment for node %s", node.ID)
			}
		}
		if err := e.client.ManualIntervention(context.Background(), e.agentID, taskID, node.ID, "Agent requests clarification"); err != nil {
			log.Printf("[executor] failed to set manual intervention: %v", err)
		}
		log.Printf("[executor] node %s set to manual_intervention, waiting for user response", node.Name)
		return
	}

	// 10. 生成节点摘要
	summary := e.generateSummary(workDir, t, taskID, node)
	if summary != "" {
		if err := e.client.ReportSummary(context.Background(), taskID, node.ID, summary); err != nil {
			log.Printf("[executor] failed to report summary: %v", err)
		}
	}

	// 11. 根据节点类型处理完成逻辑
	if commitFailed || pushFailed {
		reason := "Git push failed after completion — manual review required"
		if commitFailed {
			reason = "Git commit failed or produced no repository changes — manual review required"
		}
		log.Printf("[executor] git operation failed for node %s, setting manual_intervention", node.Name)
		if err := e.client.ManualIntervention(context.Background(), e.agentID, taskID, node.ID, reason); err != nil {
			log.Printf("[executor] failed to set manual intervention: %v", err)
		} else {
			log.Printf("[executor] node %s set to manual_intervention successfully (reason: %s)", node.Name, reason)
		}
		return
	}

	if node.NodeType == "review" {
		// Review 节点：不要自动批准。将结构化的审查结果作为评论发布。
		reviewComment := fmt.Sprintf("## Review Completed\n\n**Node:** %s\n\n**Recommendation:** Review analysis complete. A human or authorized agent should make the approve/reject decision.\n\n**Summary:** %s", node.Name, summary)
		if err := e.client.PostNodeComment(context.Background(), taskID, node.ID, "", "code_review", reviewComment); err != nil {
			log.Printf("[executor] failed to post review comment: %v", err)
		}
		log.Printf("[executor] review node %s completed, waiting for review decision (approve/reject)", node.Name)
	} else {
		// 标准/手动节点：执行后自动完成
		handoffComment := e.buildHandoffComment(taskID, node, summary, gitReady)
		if err := e.client.CompleteNode(context.Background(), e.agentID, taskID, node.ID, handoffComment); err != nil {
			log.Printf("[executor] failed to complete node: %v", err)
			comment := fmt.Sprintf("Agent finished execution but failed to mark node completed: %v", err)
			if postErr := e.client.PostNodeComment(context.Background(), taskID, node.ID, "", "question", comment); postErr != nil {
				log.Printf("[executor] failed to post completion failure comment: %v", postErr)
			}
			if miErr := e.client.ManualIntervention(context.Background(), e.agentID, taskID, node.ID, comment); miErr != nil {
				log.Printf("[executor] failed to set manual intervention after completion failure: %v", miErr)
			}
			return
		}
	}

	log.Printf("[executor] completed node %s for task %d", node.Name, taskID)
	e.notifyExecutionCompleted(taskID, node.ID)
}

// initGitWorkspace 初始化 Git 工作区：克隆或拉取仓库、配置凭据，返回 Git 是否就绪。
//
// 参数：
//   - workDir: 工作目录路径
//   - taskID: 任务 ID
//   - projectID: 项目 ID
//
// 返回：
//   - bool: Git 工作区是否就绪
func (e *TaskExecutor) initGitWorkspace(workDir string, taskID int32, projectID, projectRepoURL string, required bool) (bool, error) {
	creds, err := e.client.GetGitCredentials(context.Background(), projectID)
	if err != nil {
		if e.git.IsGitRepo() {
			log.Printf("[executor] git credentials unavailable, using existing repository: %v", err)
			return e.setupExistingRepo(taskID, required)
		}
		if required {
			return false, fmt.Errorf("project git is required but credentials are unavailable: %w", err)
		}
		log.Printf("[executor] git credentials unavailable and git is optional: %v", err)
		return false, nil
	}

	var cred *GitCredentials
	for i := range creds {
		if creds[i].RepoURL != "" && (projectRepoURL == "" || creds[i].RepoURL == projectRepoURL) {
			cred = &creds[i]
			break
		}
	}
	if cred == nil {
		for i := range creds {
			if creds[i].RepoURL != "" {
				cred = &creds[i]
				break
			}
		}
	}

	if e.git.IsGitRepo() {
		if cred != nil {
			gitName, gitEmail := e.fetchAgentGitIdentity()
			if err := e.git.ConfigureCredential(cred.Username, cred.PAT, gitName, gitEmail); err != nil {
				e.git.CleanupCredential()
				if required {
					return false, fmt.Errorf("configure git credentials: %w", err)
				}
				log.Printf("[executor] failed to configure optional git credentials: %v", err)
				return false, nil
			}
		}
		return e.setupExistingRepo(taskID, required)
	}

	repoURL := projectRepoURL
	if repoURL == "" && cred != nil {
		repoURL = cred.RepoURL
	}
	// 项目配了 git 凭据（cred != nil）即说明项目想用 git——此时即使 projectRepoURL 为空，
	// 也把 required 提升为 true，避免凭据对应的仓库克隆失败时被静默降级。
	if cred != nil && !required {
		required = true
	}
	if repoURL == "" {
		if required {
			return false, fmt.Errorf("project git is required but no repo_url is configured")
		}
		log.Printf("[executor] no repo_url configured, proceeding without git")
		return false, nil
	}
	if cred == nil {
		if required {
			return false, fmt.Errorf("project git is required but no credential matches repo %s", repoURL)
		}
		log.Printf("[executor] no git credential for repo %s, proceeding without git", repoURL)
		return false, nil
	}

	gitName, gitEmail := e.fetchAgentGitIdentity()
	if err := e.git.ConfigureCredential(cred.Username, cred.PAT, gitName, gitEmail); err != nil {
		e.git.CleanupCredential()
		if required {
			return false, fmt.Errorf("configure git credentials before clone: %w", err)
		}
		log.Printf("[executor] failed to configure optional git credentials before clone: %v", err)
		return false, nil
	}

	baseBranch := e.cfg.Git.BaseBranch
	if err := e.git.Clone(repoURL, baseBranch); err != nil {
		e.git.CleanupCredential()
		// required 已在上方根据 cred 提升；克隆失败一律视为致命错误，避免静默降级后
		// 走到 initEmptyRepo 的 "push origin master" 又失败的链路，错误被吞掉。
		if required {
			return false, fmt.Errorf("clone repository %s: %w", repoURL, err)
		}
		log.Printf("[executor] failed to clone optional repository %s: %v", repoURL, err)
		return false, nil
	}

	if err := e.git.FetchAndCheckout(taskID, baseBranch); err != nil {
		if required {
			return false, fmt.Errorf("checkout task branch: %w", err)
		}
		// 分支校验失败始终是致命的——在基础分支上操作是危险的
		if strings.Contains(err.Error(), "branch verification failed") {
			return false, fmt.Errorf("checkout task branch: %w", err)
		}
		log.Printf("[executor] warning: failed to create optional task branch: %v", err)
	}

	log.Printf("[executor] git workspace initialized: cloned %s on branch %s", repoURL, BranchName(taskID))
	return true, nil
}

// setupExistingRepo 处理工作目录已包含 Git 仓库的情况，从远程拉取并检出任务分支。
//
// 参数：
//   - taskID: 任务 ID
//
// 返回：
//   - bool: 是否成功设置
func (e *TaskExecutor) setupExistingRepo(taskID int32, required bool) (bool, error) {
	baseBranch := e.cfg.Git.BaseBranch
	if err := e.git.FetchAndCheckout(taskID, baseBranch); err != nil {
		if required {
			return false, fmt.Errorf("fetch/checkout required git repository: %w", err)
		}
		// 分支校验失败始终是致命的——在基础分支上操作是危险的
		if strings.Contains(err.Error(), "branch verification failed") {
			return false, fmt.Errorf("fetch/checkout git repository: %w", err)
		}
		log.Printf("[executor] warning: failed to fetch/checkout optional task branch: %v", err)
	}
	return true, nil
}

// fetchAgentGitIdentity 从服务器获取代理的 Git 用户名和邮箱。
// 用于配置 git config user.name 和 user.email。
//
// 返回：
//   - gitName: Git 用户名
//   - gitEmail: Git 邮箱
func (e *TaskExecutor) fetchAgentGitIdentity() (gitName, gitEmail string) {
	var agent struct {
		GitName  string `json:"git_name"`
		GitEmail string `json:"git_email"`
	}
	if err := e.client.doJSON(context.Background(), "GET", fmt.Sprintf("/api/workspaces/%s/agents/%s", e.cfg.Workspace.ID, e.agentID), nil, &agent); err != nil {
		log.Printf("[executor] warning: failed to fetch agent git identity: %v", err)
		return "", ""
	}
	return agent.GitName, agent.GitEmail
}

// pushIfNeeded 将当前分支和节点开始标签推送到远程仓库。
// 分支推送失败会返回错误，标签推送失败仅记录日志。
//
// 参数：
//   - taskID: 任务 ID
//   - node: 节点信息
//   - attempt: 尝试次数
//
// 返回：
//   - error: 分支推送失败时返回错误
func (e *TaskExecutor) pushIfNeeded(taskID int32, node TaskNode, attempt int) error {
	if e.git == nil || !e.git.IsGitRepo() {
		return nil
	}

	// 推送分支
	if err := e.git.PushBranch(taskID); err != nil {
		return fmt.Errorf("failed to push branch: %w", err)
	}

	// 推送节点开始标签——使用节点的 SortOrder
	nodeOrder := int(node.SortOrder)
	if nodeOrder <= 0 {
		nodeOrder = ParseNodeOrder(node.Name)
	}
	startTag := NodeStartTag(taskID, nodeOrder, attempt)
	if err := e.git.PushTag(startTag); err != nil {
		log.Printf("[executor] warning: failed to push tag %s: %v", startTag, err)
	}
	// 推送节点完成标签（若存在）——完成阶段已创建
	completeTag := NodeCompleteTag(taskID, nodeOrder, attempt)
	if err := e.git.PushTag(completeTag); err != nil {
		log.Printf("[executor] warning: failed to push tag %s: %v", completeTag, err)
	}
	return nil
}

// Stop 终止执行器，关闭停止信号通道。
func (e *TaskExecutor) Stop() {
	close(e.stopCh)
}

// Interrupt 处理 task:interrupt 事件，强制停止当前执行并提交中断快照。
// 流程：取消上下文 → 停止工具进程 → 强制 Git 提交 → 推送中断标签 → 上报中断确认。
//
// 参数：
//   - taskID: 要中断的任务 ID
//   - nodeID: 要中断的节点 ID
//
// 返回：
//   - error: 中断处理失败时返回错误
func (e *TaskExecutor) Interrupt(taskID int32, nodeID string) error {
	e.currentMu.Lock()
	running := e.running
	currentTaskID := e.taskID
	currentNode := e.node
	e.currentMu.Unlock()

	if !running || currentTaskID != taskID {
		log.Printf("[executor] interrupt ignored: not running task %d", taskID)
		return nil
	}

	log.Printf("[executor] interrupting task %d node %s", taskID, nodeID)
	// Mark interrupted so Execute's err branch (triggered by our cancel) does
	// NOT also call reportFailure (ManualIntervention) + partial commit +
	// notifyExecutionFailed. Interrupt owns the interrupt reporting path.
	e.interrupted.Store(true)
	e.notifyExecutionInterrupted(taskID, nodeID)

	// 1. 取消执行上下文
	e.mu.Lock()
	if e.cancelFunc != nil {
		e.cancelFunc()
	}
	e.mu.Unlock()

	// 2. 通过 Stop() 方法停止工具进程
	e.mu.Lock()
	t := e.currentTool
	e.mu.Unlock()
	if t != nil {
		if err := t.Stop(); err != nil {
			log.Printf("[executor] tool Stop() failed: %v", err)
		}
	}

	// 3. 使用标签 interrupted/<node-id> 强制提交 git
	if e.git != nil && e.git.IsGitRepo() {
		nodeOrder := int(currentNode.SortOrder)
		if nodeOrder <= 0 {
			nodeOrder = ParseNodeOrder(currentNode.Name)
		}
		interruptMsg := fmt.Sprintf("chore: interrupted by admin [%d node-%d]", taskID, nodeOrder)
		e.git.CommitAll(interruptMsg)

		// 先在本地创建中断标签，再推送
		tag := fmt.Sprintf("%s/node-%d-interrupted-%d", BranchName(taskID), nodeOrder, time.Now().Unix())
		if err := e.git.CreateTag(tag); err != nil {
			log.Printf("[executor] warning: failed to create local interrupt tag: %v", err)
		} else if err := e.git.PushTag(tag); err != nil {
			log.Printf("[executor] warning: failed to push interrupt tag: %v", err)
		}

		// 将中断的工作推送到远程
		if err := e.git.PushBranch(taskID); err != nil {
			log.Printf("[executor] warning: failed to push interrupted branch: %v", err)
		}
	}

	// 4. 向服务器上报中断完成
	if err := e.client.ReportInterrupt(context.Background(), taskID, nodeID); err != nil {
		log.Printf("[executor] failed to report interrupt: %v", err)
	}

	log.Printf("[executor] interrupt completed for task %d node %s", taskID, nodeID)
	return nil
}

// SoftInterrupt stops the currently running tool turn WITHOUT notifying the
// server. It cancels the execution context and stops the tool process so the
// human can take over locally, but it deliberately does NOT create the
// interrupted git tag, does NOT PushBranch, and does NOT ReportInterrupt. The
// node stays in_progress on the server; the err-branch is gated by
// softInterrupted so neither reportFailure (ManualIntervention) nor
// notifyExecutionFailed fires.
//
// Data source / permission boundary: this is a local-only control that reuses
// the executor's own in-memory state (taskID, node, cancelFunc, currentTool).
// It never reads or writes server state and cannot leak cross-workspace data.
func (e *TaskExecutor) SoftInterrupt(taskID int32, nodeID string) error {
	e.currentMu.Lock()
	running := e.running
	currentTaskID := e.taskID
	currentNode := e.node
	e.currentMu.Unlock()

	if !running || currentTaskID != taskID || currentNode.ID != nodeID {
		log.Printf("[executor] soft-interrupt ignored: not running task %d node %s", taskID, nodeID)
		return nil
	}

	log.Printf("[executor] soft-interrupting task %d node %s (server stays unaware)", taskID, nodeID)
	e.notifyExecutionInterrupted(taskID, nodeID)
	e.softInterrupted.Store(true)

	// 1. Cancel the execution context so the tool's Execute returns ctx.Err().
	e.mu.Lock()
	if e.cancelFunc != nil {
		e.cancelFunc()
	}
	e.mu.Unlock()

	// 2. Stop the tool process (headless tools return nil once the process is gone).
	e.mu.Lock()
	t := e.currentTool
	e.mu.Unlock()
	if t != nil {
		if err := t.Stop(); err != nil {
			log.Printf("[executor] soft-interrupt tool Stop() failed: %v", err)
		}
	}

	log.Printf("[executor] soft-interrupt completed for task %d node %s", taskID, nodeID)
	return nil
}

// IsInterventionAllowed reports whether a human can take over the currently
// running node locally. Intervention requires the node to be running and a
// tool session id to be present (so the turn can resume the same session).
func (e *TaskExecutor) IsInterventionAllowed() bool {
	e.currentMu.Lock()
	running := e.running
	e.currentMu.Unlock()
	if !running {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessionID != "" && e.lastWorkDir != ""
}

// ExecuteInterventionTurn runs ONE coding-tool turn in the node's workDir using
// the human's message as the prompt, resuming the captured Claude session when
// available. The server is NOT notified — the node stays in_progress. Returns
// the tool's stdout output.
//
// Data source / permission boundary: reuses the executor's own captured
// {workDir, sessionID, node} state. No server calls are made during the turn;
// the only server interaction is the existing SendMessage stream the onOutput
// closure already appends to (kept so the team sees the human's working
// output). It cannot leak cross-workspace data: workDir is scoped to one task.
func (e *TaskExecutor) ExecuteInterventionTurn(taskID int32, nodeID, message string) (string, error) {
	e.currentMu.Lock()
	running := e.running
	currentTaskID := e.taskID
	currentNode := e.node
	e.currentMu.Unlock()

	if !running || currentTaskID != taskID || currentNode.ID != nodeID {
		return "", fmt.Errorf("intervention turn rejected: not running task %d node %s", taskID, nodeID)
	}

	e.mu.Lock()
	workDir := e.lastWorkDir
	sessionID := e.sessionID
	projectID := e.nodeProjectID
	e.mu.Unlock()

	t := e.selectTool()
	if t == nil {
		return "", fmt.Errorf("intervention turn rejected: coding tool unavailable")
	}

	if claudeTool, ok := t.(*tool.ClaudeTool); ok && sessionID != "" {
		claudeTool.SetResumeSession(sessionID)
	} else if atomTool, ok := t.(*tool.AtomCodeTool); ok && sessionID != "" {
		atomTool.SetContinueSession(true)
	}

	if e.observer != nil {
		e.observer.OnExecutionStarted(LocalExecutionSession{
			TaskID:               taskID,
			NodeID:               nodeID,
			NodeName:             currentNode.Name,
			Tool:                 t.Name(),
			ToolSessionIDPresent: sessionID != "",
			WorkDir:              workDir,
			Status:               LocalExecutionIntervening,
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result, err := t.Execute(ctx, workDir, message, tool.ExecuteOptions{}, func(line string) {
		safeLine := desensitizeOutputLine(line)
		log.Printf("[intervention-output] %s", safeLine)
		if e.client != nil {
			if sendErr := e.client.SendMessage(context.Background(), taskID, nodeID, safeLine); sendErr != nil {
				log.Printf("[intervention] failed to send message: %v", sendErr)
			}
		}
		if e.logBuffer != nil {
			e.logBuffer.Append(LogLine{TaskID: taskID, NodeID: nodeID, Line: safeLine})
		}
	})
	if err != nil {
		return "", fmt.Errorf("intervention turn: %w", err)
	}

	// A new session id may have been captured; persist it for restart-resume.
	if result.SessionID != "" {
		e.mu.Lock()
		e.sessionID = result.SessionID
		e.mu.Unlock()
		if store := NewSessionStore(workDir); store != nil {
			_ = store.Save(result.SessionID, t.Name())
		}
	}
	_ = projectID // captured for completeness; the turn makes no server call

	return result.Output, nil
}

// Handback returns control of the node to the agent: it flips the local session
// status back to running so the watcher's next poll picks the node up and
// re-enters the Execute completion path for the same node using the persisted
// session id. The server is NOT notified — the node stays in_progress. It
// returns false if the node is no longer running (already completed/expired).
//
// Data source / permission boundary: reuses the executor's own in-memory state.
// No server calls are made; it cannot leak cross-workspace data.
func (e *TaskExecutor) Handback(taskID int32, nodeID string) bool {
	e.currentMu.Lock()
	running := e.running
	currentTaskID := e.taskID
	currentNode := e.node
	e.currentMu.Unlock()

	if !running || currentTaskID != taskID || currentNode.ID != nodeID {
		return false
	}

	if e.observer != nil {
		e.observer.OnExecutionStarted(LocalExecutionSession{
			TaskID:               taskID,
			NodeID:               nodeID,
			NodeName:             currentNode.Name,
			Tool:                 "",
			ToolSessionIDPresent: true,
			WorkDir:              e.lastWorkDir,
			Status:               LocalExecutionRunning,
		})
	}
	return true
}

// CompleteManually lets the human mark the node complete directly, calling the
// server's CompleteNode with a short summary. This is the only intervention
// method that touches server node state — and it uses the same API the
// autonomous path uses, so the server sees a normal completion. Returns false
// if the node is not currently running (nothing to complete).
//
// Data source / permission boundary: reuses the executor's captured task/node
// identity. The single server call (CompleteNode) uses the executor's own
// agentID; no cross-workspace data is involved.
func (e *TaskExecutor) CompleteManually(taskID int32, nodeID string) bool {
	e.currentMu.Lock()
	running := e.running
	currentTaskID := e.taskID
	currentNode := e.node
	e.currentMu.Unlock()

	if !running || currentTaskID != taskID || currentNode.ID != nodeID {
		return false
	}
	if e.client == nil {
		return false
	}

	comment := fmt.Sprintf("Node %s completed manually by the local operator.", currentNode.Name)
	if err := e.client.CompleteNode(context.Background(), e.agentID, taskID, nodeID, comment); err != nil {
		log.Printf("[intervention] manual CompleteNode failed: %v", err)
		return false
	}

	e.mu.Lock()
	workDir := e.lastWorkDir
	e.mu.Unlock()
	if workDir != "" {
		_ = NewSessionStore(workDir).Delete()
	}

	e.notifyExecutionCompleted(taskID, nodeID)
	return true
}

func (e *TaskExecutor) notifyExecutionStarted(taskID int32, node TaskNode, workDir string) {
	if e.observer == nil {
		return
	}
	e.observer.OnExecutionStarted(LocalExecutionSession{
		TaskID:               taskID,
		NodeID:               node.ID,
		NodeName:             node.Name,
		Tool:                 e.cfg.Agent.Provider,
		ToolSessionIDPresent: e.sessionID != "",
		WorkDir:              workDir,
	})
}

func (e *TaskExecutor) notifyExecutionCompleted(taskID int32, nodeID string) {
	if e.observer != nil {
		e.observer.OnExecutionCompleted(taskID, nodeID)
	}
}

func (e *TaskExecutor) notifyExecutionInterrupted(taskID int32, nodeID string) {
	if e.observer != nil {
		e.observer.OnExecutionInterrupted(taskID, nodeID)
	}
}

func (e *TaskExecutor) notifyExecutionFailed(taskID int32, nodeID string, err error) {
	if e.observer != nil {
		e.observer.OnExecutionFailed(taskID, nodeID, err)
	}
}

func (e *TaskExecutor) notifyToolStatus(provider string, status string, err error) {
	if e.observer != nil {
		e.observer.OnToolStatusChanged(provider, status, err)
	}
}

// IsRunning 返回执行器当前是否正在运行任务。
func (e *TaskExecutor) IsRunning() bool {
	e.currentMu.Lock()
	defer e.currentMu.Unlock()
	return e.running
}

// CurrentTask 返回当前正在运行的任务 ID 和节点信息。
func (e *TaskExecutor) CurrentTask() (taskID int32, node TaskNode, ok bool) {
	e.currentMu.Lock()
	defer e.currentMu.Unlock()
	if !e.running {
		return 0, TaskNode{}, false
	}
	return e.taskID, e.node, true
}

// GetGitManager 返回当前的 Git 管理器，如果不可用则返回 nil。
func (e *TaskExecutor) GetGitManager() *GitManager {
	e.currentMu.Lock()
	defer e.currentMu.Unlock()
	return e.git
}

// TestTool is the minimal interface tests use to replace the real coding tool.
// It is a test-only seam; production code never constructs TestTool values.
type TestTool interface {
	Name() string
	Execute(ctx context.Context, workDir, prompt string, onOutput func(string)) (*tool.ExecutionResult, error)
	Stop() error
	IsInstalled() bool
}

// SetToolFactoryForTest replaces the real tool selector with a fake for tests.
// Test-only seam; does not change production behavior when unset.
func (e *TaskExecutor) SetToolFactoryForTest(factory func() TestTool) {
	e.toolFactoryForTest = factory
}

// SetGitManagerForTest injects (or nils) the git manager without touching disk.
func (e *TaskExecutor) SetGitManagerForTest(g *GitManager) {
	e.git = g
}

// ObserverForTest returns the configured observer so tests can assert on it.
func (e *TaskExecutor) ObserverForTest() ExecutionObserver {
	return e.observer
}

// SetLogBuffer attaches a LogBuffer for capturing execution output lines served
// by the local control API. Optional; when unset, output is not buffered locally.
func (e *TaskExecutor) SetLogBuffer(lb *LogBuffer) {
	e.mu.Lock()
	e.logBuffer = lb
	e.mu.Unlock()
}

// SeedRunningForTest primes the executor as if Execute had started a node and
// then been soft-interrupted, so intervention methods can be exercised without
// a full Execute run. Test-only.
func (e *TaskExecutor) SeedRunningForTest(taskID int32, node TaskNode, projectID, workDir string) {
	e.currentMu.Lock()
	e.running = true
	e.taskID = taskID
	e.node = node
	e.currentMu.Unlock()
	e.mu.Lock()
	e.sessionID = ""
	e.lastWorkDir = workDir
	e.nodeProjectID = projectID
	e.mu.Unlock()
}

// WaitRunningForTest blocks until the executor reports it is running a task.
// Test-only seam.
func (e *TaskExecutor) WaitRunningForTest(t interface{ Fatalf(string, ...any) }) {
	for i := 0; i < 1000; i++ {
		if e.IsRunning() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("executor never started running")
}

// testToolAdapter wraps a TestTool so it satisfies tool.Tool for the executor.
type testToolAdapter struct {
	inner TestTool
}

func (a testToolAdapter) Name() string { return a.inner.Name() }

func (a testToolAdapter) Execute(ctx context.Context, workDir, prompt string, _ tool.ExecuteOptions, onOutput func(string)) (*tool.ExecutionResult, error) {
	return a.inner.Execute(ctx, workDir, prompt, onOutput)
}

func (a testToolAdapter) Stop() error       { return a.inner.Stop() }
func (a testToolAdapter) IsInstalled() bool { return a.inner.IsInstalled() }

// selectTool 根据配置选择合适的编码工具。
// selectTool 根据 AgentInfo.Provider 选择编码工具。
// Provider 为必填字段，在配置加载时已校验。
//
// 返回:
//   - tool.Tool: 选中的编码工具适配器实例
func (e *TaskExecutor) selectTool() tool.Tool {
	if e.toolFactoryForTest != nil {
		return testToolAdapter{inner: e.toolFactoryForTest()}
	}
	provider := e.cfg.Agent.Provider
	path := e.toolPath(provider)
	return tool.GetTool(provider, path)
}

// toolPath 返回指定 provider 的可执行文件路径。
// 默认值由各工具构造函数处理，此处仅从配置中读取。
func (e *TaskExecutor) toolPath(provider string) string {
	switch strings.ToLower(provider) {
	case "claude":
		return e.cfg.Tools.Claude.Path
	case "openclaw":
		return e.cfg.Tools.OpenClaw.Path
	case "opencode":
		return e.cfg.Tools.OpenCode.Path
	case "atomcode":
		return e.cfg.Tools.AtomCode.Path
	case "mimocode":
		return e.cfg.Tools.MiMoCode.Path
	default:
		return ""
	}
}

func (e *TaskExecutor) shouldPostNeedsInputComment(ctx context.Context, taskID int32, nodeID, content string) bool {
	comments, err := e.client.ListNodeComments(ctx, taskID, nodeID)
	if err != nil {
		log.Printf("[executor] warning: failed to list node comments before needs_input dedupe: %v", err)
		return true
	}
	return !HasDuplicateAgentComment(comments, e.agentID, "question", content)
}

func HasDuplicateAgentComment(comments []Comment, agentID, commentType, content string) bool {
	want := strings.TrimSpace(content)
	for _, c := range comments {
		if c.AuthorType != "agent" || c.AuthorID != agentID {
			continue
		}
		if c.CommentType != commentType {
			continue
		}
		if strings.TrimSpace(c.Content) == want {
			return true
		}
	}
	return false
}

func (e *TaskExecutor) buildHandoffComment(taskID int32, node TaskNode, summary string, gitReady bool) string {
	var sb strings.Builder
	sb.WriteString("## 节点交接\n\n")
	sb.WriteString(fmt.Sprintf("- 来源节点：%s (%s)\n", node.Name, node.ID))
	sb.WriteString(fmt.Sprintf("- 执行者：%s\n", e.cfg.Agent.Name))
	if gitReady {
		sb.WriteString(fmt.Sprintf("- Git 分支：%s\n", BranchName(taskID)))
	}
	if summary != "" {
		sb.WriteString("\n### 执行摘要\n")
		sb.WriteString(summary)
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n### 执行摘要\n当前节点已完成，但未生成额外摘要。\n")
	}
	sb.WriteString("\n### 下个节点注意事项\n请基于当前仓库状态和本交接信息继续执行；代码状态以 Git 工作区为准，非代码上下文以当前节点评论区为准。")
	return sb.String()
}

// buildPromptWithClient 使用上下文注入层级构建执行上下文 prompt。
func (e *TaskExecutor) buildPromptWithClient(taskID int32, node TaskNode, task Task, isResume bool, caps PromptCapabilities) (string, error) {
	context, err := BuildExecutionContextWithCapabilities(e.client, e.cfg, task, node, isResume, caps)
	if err != nil {
		return "", fmt.Errorf("build execution context: %w", err)
	}

	// 追加协作工具说明
	context += "\n## Collaboration Tools\n"
	context += "You can use the following tool to communicate with your team:\n"
	context += "- Request human input: output `<needs_input>your question here</needs_input>` to pause the task and let Teammate post the question once.\n"

	// 追加输出要求
	context += "\n## Output Requirements\n"
	context += "- All task results MUST be written to files in the working directory.\n"
	context += "- Do not just output results to the terminal — they will not be saved.\n"
	context += "- Create appropriate files (e.g., markdown, code, config) for your deliverables.\n"
	context += "- Your changes will be automatically committed to the git repository.\n"

	return context, nil
}

// reportFailure 将执行失败信息上报为 manual_intervention 状态，请求人工介入处理。
//
// 参数:
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//   - err: 导致失败的错误
func (e *TaskExecutor) reportFailure(taskID int32, nodeID string, err error) {
	comment := fmt.Sprintf("Execution failed: %v\nManual intervention required.", err)
	if reportErr := e.client.ManualIntervention(context.Background(), e.agentID, taskID, nodeID, comment); reportErr != nil {
		log.Printf("[executor] failed to report manual intervention: %v", reportErr)
	}
}

// ParseNodeOrder 从节点名称中提取节点序号（例如 "1. 需求分析" 返回 1）。
func ParseNodeOrder(name string) int {
	order := 0
	fmt.Sscanf(name, "%d.", &order)
	return order
}

// generateSummary 为已完成的节点生成工作摘要，
// 复用同一编码工具会话以保留执行过程的完整上下文。
func (e *TaskExecutor) generateSummary(workDir string, t tool.Tool, taskID int32, node TaskNode) string {
	// 设置会话恢复，使摘要调用延续同一个会话
	if claudeTool, ok := t.(*tool.ClaudeTool); ok {
		e.mu.Lock()
		sid := e.sessionID
		e.mu.Unlock()
		if sid != "" {
			claudeTool.SetResumeSession(sid)
		}
	} else if atomTool, ok := t.(*tool.AtomCodeTool); ok {
		e.mu.Lock()
		sid := e.sessionID
		e.mu.Unlock()
		if sid != "" {
			atomTool.SetContinueSession(true)
		}
	}

	summaryPrompt := fmt.Sprintf(
		"Summarize the work you completed for node \"%s\" in Chinese (3-5 sentences). Focus on what was done, not how. Provide ONLY the summary text, no additional formatting.",
		node.Name,
	)

	log.Printf("[prompt] ====== Summary Prompt (node=%s) ======", node.Name)
	log.Printf("[prompt]\n%s", summaryPrompt)
	log.Printf("[prompt] ====== End Summary Prompt (len=%d) ======", len(summaryPrompt))

	log.Printf("[executor] generating summary for node %s", node.Name)
	sumCtx, sumCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer sumCancel()
	result, err := t.Execute(sumCtx, workDir, summaryPrompt, tool.ExecuteOptions{}, func(line string) {
		log.Printf("[summary] %s", desensitizeOutputLine(line))
	})
	if err != nil {
		log.Printf("[executor] summary generation failed: %v", err)
		return ""
	}

	summary := strings.TrimSpace(result.Output)
	// 清理：必要时从 stream-json 中提取文本
	summary = cleanSummaryOutput(summary)
	if len(summary) > 500 {
		summary = summary[:497] + "..."
	}
	return summary
}

// cleanSummaryOutput 从可能包含 JSON 格式的输出中提取纯文本。
func cleanSummaryOutput(output string) string {
	// 如果输出看起来像 stream-json，尝试从 "result" 类型行提取文本
	var texts []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			// 纯文本行
			if line != "" {
				texts = append(texts, line)
			}
			continue
		}
		// 尝试解析为 JSON 并提取文本
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			texts = append(texts, line)
			continue
		}
		var eventType string
		if t, ok := obj["type"]; ok {
			_ = json.Unmarshal(t, &eventType)
		}
		if eventType == "result" {
			var resultStr string
			if r, ok := obj["result"]; ok {
				_ = json.Unmarshal(r, &resultStr)
				if resultStr != "" {
					texts = append(texts, resultStr)
				}
			}
		}
	}
	return strings.Join(texts, "\n")
}

// needsInputRegex 用于匹配 <needs_input> 标签中的内容。
var needsInputRegex = regexp.MustCompile(`(?s)<needs_input>(.*?)</needs_input>`)

// extractNeedsInputComment 从 <needs_input>...</needs_input> 块中提取评论内容。
func extractNeedsInputComment(output string) string {
	matches := needsInputRegex.FindStringSubmatch(output)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	// 如果没有闭合标签，尝试获取 <needs_input> 之后的文本
	if idx := strings.Index(output, "<needs_input>"); idx >= 0 {
		remaining := strings.TrimSpace(output[idx+len("<needs_input>"):])
		// 最多取 500 个字符
		if len(remaining) > 500 {
			remaining = remaining[:500]
		}
		return remaining
	}
	return ""
}

// checkDiskQuota 检查工作目录的磁盘使用量是否超过配额限制。
// 默认配额为 10GB，可通过 TEAMMATE_DISK_QUOTA_GB 环境变量配置。
// 达到 80% 配额时发出警告，超过配额时返回错误。
//
// 参数:
//   - workDir: 要检查的工作目录路径
//
// 返回:
//   - error: 磁盘配额超限时返回错误
func (e *TaskExecutor) checkDiskQuota(workDir string) error {
	maxSizeGB := int64(10) // 默认 10GB
	if maxStr := os.Getenv("TEAMMATE_DISK_QUOTA_GB"); maxStr != "" {
		if max, err := strconv.ParseInt(maxStr, 10, 64); err == nil {
			maxSizeGB = max
		}
	}

	size, err := getDirSize(workDir)
	if err != nil {
		return fmt.Errorf("get directory size: %w", err)
	}

	sizeGB := size / (1024 * 1024 * 1024)
	if sizeGB >= maxSizeGB {
		return fmt.Errorf("disk quota exceeded: current %dGB, max %dGB", sizeGB, maxSizeGB)
	}

	if sizeGB >= maxSizeGB*80/100 {
		log.Printf("[executor] WARNING: disk usage %dGB approaching quota %dGB", sizeGB, maxSizeGB)
	}

	return nil
}

func getDirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return err
	})
	return size, err
}
