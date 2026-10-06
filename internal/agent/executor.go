// executor.go implements the task executor, which is responsible for executing claimed
// nodes using coding tools.
//
// This file is the core execution module of the Agent Daemon, containing the following functionality:
//   - Task execution flow orchestration (prepare workspace -> build context -> invoke tool -> report result)
//   - Git workspace management (clone, branch creation, start/end tag)
//   - Coding tool selection and invocation (Claude Code / OpenClaw / OpenCode)
//   - Real-time execution log reporting (via SSE + Redis buffering)
//   - Interrupt handling (receive task:interrupt event, SIGTERM->SIGKILL process group)
//   - Session recovery (resume execution context after restart)
//   - Checkpoint commits (periodically commit current progress)
//   - Disk quota check and Token usage reporting
//
// TaskExecutor is the main struct of the executor, coordinating Git, tool, log and other submodules.
// Execution flow: clone repository -> create feature branch -> tag start -> inject context ->
// invoke coding tool -> report logs in real time -> commit + push -> tag end -> report usage.
package agent

import (
	"context"
	"encoding/json"
	"errors"
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

// ErrInterventionNotHeld is returned by local intervention actions when the
// executor does not hold the requested task/node context (idle, mismatched
// target, or no soft-interrupted takeover). The local control plane maps it
// to 409.
var ErrInterventionNotHeld = errors.New("intervention target not held")

// ErrCodingToolUnavailable is returned when an intervention turn cannot run
// because no coding tool is installed. The local control plane maps it to 503.
var ErrCodingToolUnavailable = errors.New("coding tool unavailable")

// RunContext carries one node's execution identity: the workspace connection
// the node belongs to (alias plus its client), the agent UUID resolved within
// that workspace, and the node's location. Every server call during the
// execution goes through RunContext.Client with X-Agent-ID = RunContext.AgentID.
type RunContext struct {
	Client      *Client // the owning workspace connection's client
	ConnName    string  // the owning connection's local alias
	AgentID     string  // the agent UUID within that workspace
	WorkspaceID string
	ProjectID   string
	TaskID      int32
	NodeID      string
}

// TaskExecutor is responsible for executing claimed nodes using coding tools, and manages
// the Git workspace, session recovery, checkpoint commits, and interrupt handling.
type TaskExecutor struct {
	cfg      *Config
	git      *GitManager
	stopCh   chan struct{}
	observer ExecutionObserver

	// run is the current execution's identity, installed at Execute entry and
	// kept after the execution returns so local intervention turns (soft
	// interrupt, manual takeover, manual completion) keep reporting through
	// the same connection. Nil while no node has ever been executed.
	run *RunContext

	mu          sync.Mutex
	currentTool tool.Tool
	cancelFunc  context.CancelFunc
	currentMu   sync.Mutex
	running     bool
	taskID      int32
	node        TaskNode

	sessionID   string // current Claude Code session ID, used for --resume
	lastWorkDir string // last working directory, used for session invalidation detection

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

	// toolFactory, when non-nil, replaces the real coding-tool selector.
	// Production injection point (supervisor ToolFactory option).
	toolFactory func() tool.Tool

	// logBuffer, when set, captures desensitized output lines for the local
	// control API (recent + SSE streaming). Data source: this agentd's own
	// onOutput only — never cross-workspace/project. Degrades to a no-op when
	// nil (local control disabled).
	logBuffer *LogBuffer

	// onResume, when set, is invoked by Handback after a soft-interrupted
	// takeover is handed back to the agent. It triggers a watcher recovery
	// pass so the still in_progress node is re-entered with the persisted
	// tool session. Wired by the runtime; nil in tests unless injected.
	onResume func()
}

type ExecutionObserver interface {
	OnExecutionStarted(session LocalExecutionSession)
	OnExecutionCompleted(taskID int32, nodeID string)
	OnExecutionInterrupted(taskID int32, nodeID string)
	OnExecutionFailed(taskID int32, nodeID string, err error)
	OnToolStatusChanged(provider string, status string, err error)
}

// NewTaskExecutor creates a new task executor.
//
// Args:
//   - cfg: the machine-level config view (tools, git, workspace root)
//
// Returns:
//   - *TaskExecutor: the initialized executor instance
func NewTaskExecutor(cfg *Config) *TaskExecutor {
	return NewTaskExecutorWithObserver(cfg, nil)
}

func NewTaskExecutorWithObserver(cfg *Config, observer ExecutionObserver) *TaskExecutor {
	return &TaskExecutor{
		cfg:      cfg,
		observer: observer,
		stopCh:   make(chan struct{}),
	}
}

// currentRun returns the active execution identity, or nil when none was ever
// installed.
func (e *TaskExecutor) currentRun() *RunContext {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.run
}

// Execute executes a claimed node, including Git workspace initialization, context building,
// coding tool invocation, and result reporting.
//
// Args:
//   - run: the execution identity (owning connection, agent UUID, workspace, node location)
//   - node: the node information to execute
func (e *TaskExecutor) Execute(run RunContext, node TaskNode) {
	taskID := run.TaskID
	projectID := run.ProjectID
	log.Printf("[executor] starting node %s (%s) for task %d", node.ID, node.Name, taskID)

	e.currentMu.Lock()
	taskChanged := e.taskID != taskID
	e.running = true
	e.taskID = taskID
	e.node = node
	e.currentMu.Unlock()
	e.mu.Lock()
	e.run = &run
	if taskChanged {
		// Bind the session context to this task at entry: a tool session
		// captured for a previous task must never be resumed or reused for
		// this one. Same-task re-entry (watcher recovery, handback) keeps the
		// captured session so the next turn resumes it.
		projectDir := "no-project"
		if run.ProjectID != "" {
			projectDir = run.ProjectID
		}
		e.sessionID = ""
		e.lastWorkDir = workDirPath(e.cfg.Workspace.Root, run.WorkspaceID, run.AgentID, projectDir, taskID)
	}
	e.mu.Unlock()
	e.interrupted.Store(false)
	e.softInterrupted.Store(false)

	defer func() {
		e.currentMu.Lock()
		e.running = false
		e.currentMu.Unlock()
	}()

	// Create a cancellable context for this execution
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.cancelFunc = cancel
	e.mu.Unlock()
	defer cancel()

	// 2. Fetch task details to obtain the projectID and repository information
	task := Task{
		ID:        taskID,
		Title:     node.Name,
		ProjectID: projectID,
	}
	projectRepoURL := ""
	if projectID != "" {
		if fetchedTask, err := run.Client.GetTask(context.Background(), run.AgentID, projectID, taskID); err == nil && fetchedTask != nil {
			task = *fetchedTask
			task.ProjectID = projectID // ensure projectID is set even if the API did not return it
		}
		if project, err := run.Client.GetProject(context.Background(), run.AgentID, run.WorkspaceID, projectID); err == nil && project != nil {
			projectRepoURL = strings.TrimSpace(project.RepoURL)
		} else {
			log.Printf("[executor] warning: failed to fetch project git config: %v", err)
		}
	}

	// Create an isolated working directory: {Root}/{workspaceID}/{agentID}/{projectID}/{taskID}
	// so tasks of different projects and agent instances are isolated from each other
	projectDir := "no-project"
	if task.ProjectID != "" {
		projectDir = task.ProjectID
	}
	workDir := workDirPath(e.cfg.Workspace.Root, run.WorkspaceID, run.AgentID, projectDir, taskID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		log.Printf("[executor] failed to create workdir: %v", err)
		e.notifyExecutionFailed(taskID, node.ID, err)
		e.reportFailure(&run, taskID, node.ID, err)
		return
	}
	e.notifyExecutionStarted(&run, taskID, node, workDir)

	// Check disk quota before execution
	if err := e.checkDiskQuota(workDir); err != nil {
		log.Printf("[executor] disk quota check failed: %v", err)
		e.notifyExecutionFailed(taskID, node.ID, err)
		e.reportFailure(&run, taskID, node.ID, err)
		return
	}

	// 3. Initialize the Git workspace
	e.git = NewGitManager(workDir)
	gitReady := false

	// Whether Git is required: if the project has repo_url configured, or git credentials
	// exist, it is treated as required. This way a clone/credential failure terminates the
	// execution as a fatal error and is reported, rather than silently degrading to running
	// without Git (otherwise it would enter the path of "optional repo clone fails -> log
	// and continue -> later push origin master fails", and the error would be swallowed).
	gitRequired := projectRepoURL != ""
	if task.ProjectID != "" {
		var err error
		gitReady, err = e.initGitWorkspace(&run, workDir, taskID, task.ProjectID, projectRepoURL, gitRequired)
		if err != nil {
			log.Printf("[executor] git workspace initialization failed: %v", err)
			e.notifyExecutionFailed(taskID, node.ID, err)
			e.reportFailure(&run, taskID, node.ID, err)
			return
		}
	}
	// If there is no projectID, work without Git — workDir is a fresh empty directory
	// Clean up Git credentials (askpass script) after execution completes
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
		// Tag the node start — use the node's SortOrder rather than parsing it from the name
		nodeOrder := int(node.SortOrder)
		if nodeOrder <= 0 {
			nodeOrder = ParseNodeOrder(node.Name)
		}
		if err := e.git.TagNodeStart(taskID, nodeOrder, attempt); err != nil {
			log.Printf("[executor] warning: failed to tag node start: %v", err)
		}
		// Report the Git branch name to the Server so the frontend can display it
		branch := BranchName(taskID)
		if err := run.Client.ReportGitBranch(context.Background(), run.AgentID, taskID, branch); err != nil {
			log.Printf("[executor] warning: failed to report git branch: %v", err)
		}
	}

	// 4. Select the coding tool
	t := e.selectTool()
	e.mu.Lock()
	e.currentTool = t
	e.mu.Unlock()
	e.notifyToolStatus(t.Name(), LocalToolConnected, nil)

	// Set up session recovery if available (supported by Claude Code and AtomCode)
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
		// AtomCode sessions are scoped by directory: if the previous turn already ran in this
		// working directory, it can be continued (pass -c), rather than relying on a parsed
		// session_id (AtomCode output contains no parseable session id).
		if e.lastWorkDir == workDir {
			atomTool.SetContinueSession(true)
			log.Printf("[executor] continuing AtomCode session in %s", workDir)
		}
	}

	capabilities, err := MaterializeAgentCapabilities(ctx, run, e.cfg, workDir, t.Name())
	if err != nil {
		log.Printf("[executor] warning: failed to prepare agent capabilities: %v", err)
		capabilities = CapabilityInjection{
			PromptCapabilities: PromptCapabilities{IncludeSkills: true, IncludeMCP: true},
		}
	} else {
		log.Printf("[executor] prepared capabilities for %s: skills=%d mcp_servers=%d", t.Name(), capabilities.SkillCount, capabilities.MCPServerCount)
	}

	// 5. Build the execution context using the context injection layer
	// Use a simplified context when resuming a Claude session (--resume retains the previous reasoning)
	isResume := e.sessionID != "" && e.lastWorkDir == workDir
	prompt, err := e.buildPromptWithClient(run.Client, run.WorkspaceID, run.AgentID, taskID, node, task, isResume, capabilities.PromptCapabilities)
	if err != nil {
		e.notifyExecutionFailed(taskID, node.ID, err)
		e.reportFailure(&run, taskID, node.ID, fmt.Errorf("build prompt: %w", err))
		return
	}

	// 6. Print the full injected context for debugging
	log.Printf("[prompt] ====== Injected Prompt (task=%d, node=%s) ======", taskID, node.Name)
	log.Printf("[prompt]\n%s", prompt)
	log.Printf("[prompt] ====== End Prompt (len=%d) ======", len(prompt))

	// 7. Execute with the coding tool, with periodic checkpoint commits in parallel
	log.Printf("[executor] running %s with prompt len=%d", t.Name(), len(prompt))

	// Start the checkpoint goroutine — periodically commits work to prevent losing uncommitted
	// changes if the tool process crashes
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

	// Start the timeout detection goroutine — on timeout it notifies the frontend without
	// interrupting the Claude process; the user decides whether to abort
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
			warning := fmt.Sprintf("⚠️ Node execution exceeded %v; manual intervention may be required. You can interrupt this task on the task detail page.", timeout)
			if sendErr := run.Client.SendMessageWithType(context.Background(), run.AgentID, taskID, node.ID, "system", warning); sendErr != nil {
				log.Printf("[executor] failed to send timeout warning: %v", sendErr)
			}
		case <-ctx.Done():
			// Execution completed or was interrupted, no timeout notification needed
		}
	}()

	result, err := t.Execute(ctx, workDir, prompt, capabilities.ToolOptions, func(line string) {
		// Real-time output — desensitize and log
		safeLine := desensitizeOutputLine(line)
		log.Printf("[output] %s", safeLine)
		// Capture into the local log buffer (for the local control API to read / SSE push)
		if e.logBuffer != nil {
			e.logBuffer.Append(LogLine{TaskID: taskID, NodeID: node.ID, Line: safeLine})
		}
		// Send the desensitized log to the server
		if sendErr := run.Client.SendMessage(context.Background(), run.AgentID, taskID, node.ID, safeLine); sendErr != nil {
			log.Printf("[executor] failed to send message: %v", sendErr)
		}
	})

	// Cancel the context to stop the checkpoint goroutine, then wait for it to finish
	cancel()
	<-checkpointDone
	<-timeoutDone

	// The tool adapters report a killed/crashed process as a nil error with a
	// non-zero exit code; treat that exactly like a returned error so an
	// interrupted or crashed tool never falls through to the completion path
	// (which would report manual_intervention and leak a local takeover to
	// the server).
	if result != nil && result.ExitCode != 0 && err == nil {
		err = fmt.Errorf("tool exited with code %d", result.ExitCode)
	}

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
		// Commit partial work
		if e.git != nil && e.git.IsGitRepo() {
			e.git.CommitAll(fmt.Sprintf("teammate: partial work for %s (failed)", node.Name))
			_ = e.pushIfNeeded(taskID, node, attempt)
		}
		e.reportFailure(&run, taskID, node.ID, err)
		e.notifyExecutionFailed(taskID, node.ID, err)
		return
	}

	// Capture the session ID from the result (as a backup in case the callback did not fire)
	if result.SessionID != "" {
		e.mu.Lock()
		e.sessionID = result.SessionID
		e.lastWorkDir = workDir
		e.mu.Unlock()
		log.Printf("[executor] captured Claude session ID from result: %s", result.SessionID)
	}

	// Record that this working directory has been executed (sessions are scoped by directory):
	// used by the next node of the same task to decide whether to continue (-c/--resume).
	// Different tasks use different workdirs, so they are naturally isolated and sessions do not leak.
	e.mu.Lock()
	e.lastWorkDir = workDir
	e.mu.Unlock()

	// 7. Commit changes and push to the remote repository
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
			// Tag the node complete — use the node's SortOrder, matching TagNodeStart
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

	// 8. Report Token usage
	if result.TotalTokens > 0 {
		usage := TokenUsageRequest{
			InputTokens:  result.InputTokens,
			OutputTokens: result.OutputTokens,
			TotalTokens:  result.TotalTokens,
		}
		if err := run.Client.ReportTokenUsage(context.Background(), run.AgentID, taskID, node.ID, usage); err != nil {
			log.Printf("[executor] failed to report token usage: %v", err)
		}
	}

	// 9. Check whether the agent needs human input
	if strings.Contains(result.Output, "<needs_input>") {
		log.Printf("[executor] agent requests human input for node %s", node.Name)
		comment := extractNeedsInputComment(result.Output)
		if comment != "" {
			if e.shouldPostNeedsInputComment(context.Background(), &run, taskID, node.ID, comment) {
				if err := run.Client.PostNodeComment(context.Background(), run.AgentID, taskID, node.ID, "", "question", comment); err != nil {
					log.Printf("[executor] failed to post comment: %v", err)
				}
			} else {
				log.Printf("[executor] skipped duplicate needs_input comment for node %s", node.ID)
			}
		}
		if err := run.Client.ManualIntervention(context.Background(), run.AgentID, taskID, node.ID, "Agent requests clarification"); err != nil {
			log.Printf("[executor] failed to set manual intervention: %v", err)
		}
		log.Printf("[executor] node %s set to manual_intervention, waiting for user response", node.Name)
		return
	}

	// 10. Generate the node summary
	summary := e.generateSummary(workDir, t, taskID, node)
	if summary != "" {
		if err := run.Client.ReportSummary(context.Background(), run.AgentID, taskID, node.ID, summary); err != nil {
			log.Printf("[executor] failed to report summary: %v", err)
		}
	}

	// 11. Handle the completion logic according to the node type
	if commitFailed || pushFailed {
		reason := "Git push failed after completion — manual review required"
		if commitFailed {
			reason = "Git commit failed or produced no repository changes — manual review required"
		}
		log.Printf("[executor] git operation failed for node %s, setting manual_intervention", node.Name)
		if err := run.Client.ManualIntervention(context.Background(), run.AgentID, taskID, node.ID, reason); err != nil {
			log.Printf("[executor] failed to set manual intervention: %v", err)
		} else {
			log.Printf("[executor] node %s set to manual_intervention successfully (reason: %s)", node.Name, reason)
		}
		return
	}

	if node.NodeType == "review" {
		// Review node: do not auto-approve. Post the structured review result as a comment.
		reviewComment := fmt.Sprintf("## Review Completed\n\n**Node:** %s\n\n**Recommendation:** Review analysis complete. A human or authorized agent should make the approve/reject decision.\n\n**Summary:** %s", node.Name, summary)
		if err := run.Client.PostNodeComment(context.Background(), run.AgentID, taskID, node.ID, "", "code_review", reviewComment); err != nil {
			log.Printf("[executor] failed to post review comment: %v", err)
		}
		log.Printf("[executor] review node %s completed, waiting for review decision (approve/reject)", node.Name)
	} else {
		// Standard/manual node: auto-complete after execution
		handoffComment := e.buildHandoffComment(taskID, node, summary, gitReady)
		if err := run.Client.CompleteNode(context.Background(), run.AgentID, taskID, node.ID, handoffComment); err != nil {
			log.Printf("[executor] failed to complete node: %v", err)
			comment := fmt.Sprintf("Agent finished execution but failed to mark node completed: %v", err)
			if postErr := run.Client.PostNodeComment(context.Background(), run.AgentID, taskID, node.ID, "", "question", comment); postErr != nil {
				log.Printf("[executor] failed to post completion failure comment: %v", postErr)
			}
			if miErr := run.Client.ManualIntervention(context.Background(), run.AgentID, taskID, node.ID, comment); miErr != nil {
				log.Printf("[executor] failed to set manual intervention after completion failure: %v", miErr)
			}
			return
		}
	}

	log.Printf("[executor] completed node %s for task %d", node.Name, taskID)
	e.notifyExecutionCompleted(taskID, node.ID)
}

// initGitWorkspace initializes the Git workspace: clones or pulls the repository, configures
// credentials, and returns whether Git is ready.
//
// Args:
//   - workDir: working directory path
//   - taskID: task ID
//   - projectID: project ID
//
// Returns:
//   - bool: whether the Git workspace is ready
func (e *TaskExecutor) initGitWorkspace(run *RunContext, workDir string, taskID int32, projectID, projectRepoURL string, required bool) (bool, error) {
	creds, err := run.Client.GetGitCredentials(context.Background(), run.AgentID, projectID)
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
			gitName, gitEmail := e.fetchAgentGitIdentity(run)
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
	// The project having git credentials configured (cred != nil) means the project intends to
	// use git — so even if projectRepoURL is empty, promote required to true here, to avoid a
	// silent degradation when cloning the credential's repository fails.
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

	gitName, gitEmail := e.fetchAgentGitIdentity(run)
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
		// required was promoted above based on cred; a clone failure is always treated as a
		// fatal error, to avoid silent degradation followed by the initEmptyRepo "push origin
		// master" path failing again and the error being swallowed.
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
		// Branch verification failure is always fatal — operating on the base branch is dangerous
		if strings.Contains(err.Error(), "branch verification failed") {
			return false, fmt.Errorf("checkout task branch: %w", err)
		}
		log.Printf("[executor] warning: failed to create optional task branch: %v", err)
	}

	log.Printf("[executor] git workspace initialized: cloned %s on branch %s", repoURL, BranchName(taskID))
	return true, nil
}

// setupExistingRepo handles the case where the working directory already contains a Git
// repository: it pulls from the remote and checks out the task branch.
//
// Args:
//   - taskID: task ID
//
// Returns:
//   - bool: whether the setup succeeded
func (e *TaskExecutor) setupExistingRepo(taskID int32, required bool) (bool, error) {
	baseBranch := e.cfg.Git.BaseBranch
	if err := e.git.FetchAndCheckout(taskID, baseBranch); err != nil {
		if required {
			return false, fmt.Errorf("fetch/checkout required git repository: %w", err)
		}
		// Branch verification failure is always fatal — operating on the base branch is dangerous
		if strings.Contains(err.Error(), "branch verification failed") {
			return false, fmt.Errorf("fetch/checkout git repository: %w", err)
		}
		log.Printf("[executor] warning: failed to fetch/checkout optional task branch: %v", err)
	}
	return true, nil
}

// fetchAgentGitIdentity fetches the agent's Git username and email from the
// server through the execution's connection. Used to configure git config
// user.name and user.email.
//
// Returns:
//   - gitName: Git username
//   - gitEmail: Git email
func (e *TaskExecutor) fetchAgentGitIdentity(run *RunContext) (gitName, gitEmail string) {
	var agent struct {
		GitName  string `json:"git_name"`
		GitEmail string `json:"git_email"`
	}
	if err := run.Client.GetAgent(context.Background(), run.WorkspaceID, run.AgentID, &agent); err != nil {
		log.Printf("[executor] warning: failed to fetch agent git identity: %v", err)
		return "", ""
	}
	return agent.GitName, agent.GitEmail
}

// pushIfNeeded pushes the current branch and the node start tag to the remote repository.
// A branch push failure returns an error; a tag push failure only logs.
//
// Args:
//   - taskID: task ID
//   - node: node information
//   - attempt: attempt count
//
// Returns:
//   - error: returns an error when the branch push fails
func (e *TaskExecutor) pushIfNeeded(taskID int32, node TaskNode, attempt int) error {
	if e.git == nil || !e.git.IsGitRepo() {
		return nil
	}

	// Push the branch
	if err := e.git.PushBranch(taskID); err != nil {
		return fmt.Errorf("failed to push branch: %w", err)
	}

	// Push the node start tag — use the node's SortOrder
	nodeOrder := int(node.SortOrder)
	if nodeOrder <= 0 {
		nodeOrder = ParseNodeOrder(node.Name)
	}
	startTag := NodeStartTag(taskID, nodeOrder, attempt)
	if err := e.git.PushTag(startTag); err != nil {
		log.Printf("[executor] warning: failed to push tag %s: %v", startTag, err)
	}
	// The complete tag only exists on runs that reached the completion phase;
	// push it only when present locally
	completeTag := NodeCompleteTag(taskID, nodeOrder, attempt)
	if e.git.tagExists(completeTag) {
		if err := e.git.PushTag(completeTag); err != nil {
			log.Printf("[executor] warning: failed to push tag %s: %v", completeTag, err)
		}
	}
	return nil
}

// runContextWorkspace resolves the workspace of the current execution, or ""
// when none was ever installed (local-only seeds).
func runContextWorkspace(run *RunContext) string {
	if run == nil {
		return ""
	}
	return run.WorkspaceID
}

// Stop terminates the executor and closes the stop signal channel.
func (e *TaskExecutor) Stop() {
	close(e.stopCh)
}

// Interrupt handles the task:interrupt event: forcibly stops the current execution and
// commits an interrupt snapshot.
// Flow: cancel context -> stop the tool process -> force a Git commit -> push the interrupt
// tag -> report the interrupt confirmation.
//
// Args:
//   - taskID: the task ID to interrupt
//   - nodeID: the node ID to interrupt
//
// Returns:
//   - error: returns an error when interrupt handling fails
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

	// Duplicate control events (e.g. a redelivered interrupt) must not run
	// the interrupt path twice: the second commit/tag/ack would fail and
	// double-report.
	if e.interrupted.Swap(true) {
		log.Printf("[executor] interrupt ignored: task %d node %s already interrupted", taskID, nodeID)
		return nil
	}

	log.Printf("[executor] interrupting task %d node %s", taskID, nodeID)
	// The interrupted flag (set by the Swap above) makes Execute's err branch
	// skip reportFailure (ManualIntervention), partial commit, and
	// notifyExecutionFailed; Interrupt owns the interrupt reporting path.
	e.notifyExecutionInterrupted(taskID, nodeID)

	// 1. Cancel the execution context
	e.mu.Lock()
	if e.cancelFunc != nil {
		e.cancelFunc()
	}
	e.mu.Unlock()

	// 2. Stop the tool process via the Stop() method
	e.mu.Lock()
	t := e.currentTool
	e.mu.Unlock()
	if t != nil {
		if err := t.Stop(); err != nil {
			log.Printf("[executor] tool Stop() failed: %v", err)
		}
	}

	// 3. Force a git commit using the tag interrupted/<node-id>
	if e.git != nil && e.git.IsGitRepo() {
		nodeOrder := int(currentNode.SortOrder)
		if nodeOrder <= 0 {
			nodeOrder = ParseNodeOrder(currentNode.Name)
		}
		interruptMsg := fmt.Sprintf("chore: interrupted by admin [%d node-%d]", taskID, nodeOrder)
		e.git.CommitAll(interruptMsg)

		// Create the interrupt tag locally first, then push it
		tag := fmt.Sprintf("%s/node-%d-interrupted-%d", BranchName(taskID), nodeOrder, time.Now().Unix())
		if err := e.git.CreateTag(tag); err != nil {
			log.Printf("[executor] warning: failed to create local interrupt tag: %v", err)
		} else if err := e.git.PushTag(tag); err != nil {
			log.Printf("[executor] warning: failed to push interrupt tag: %v", err)
		}

		// Push the interrupted work to the remote
		if err := e.git.PushBranch(taskID); err != nil {
			log.Printf("[executor] warning: failed to push interrupted branch: %v", err)
		}
	}

	// 4. Report the interrupt completion to the server
	if run := e.currentRun(); run != nil {
		if err := run.Client.ReportInterrupt(context.Background(), run.AgentID, taskID, nodeID); err != nil {
			log.Printf("[executor] failed to report interrupt: %v", err)
		}
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
		log.Printf("[executor] soft-interrupt rejected: not running task %d node %s", taskID, nodeID)
		return fmt.Errorf("%w: not running task %d node %s", ErrInterventionNotHeld, taskID, nodeID)
	}

	log.Printf("[executor] soft-interrupting task %d node %s (server stays unaware)", taskID, nodeID)
	e.softInterrupted.Store(true)

	// Local takeover moves the session to the intervening state; the server
	// is not notified and the node stays in_progress.
	e.mu.Lock()
	workDir := e.lastWorkDir
	sessionID := e.sessionID
	e.mu.Unlock()
	if e.observer != nil {
		e.observer.OnExecutionStarted(LocalExecutionSession{
			WorkspaceID:          runContextWorkspace(e.currentRun()),
			TaskID:               taskID,
			NodeID:               nodeID,
			NodeName:             currentNode.Name,
			ToolSessionIDPresent: sessionID != "",
			WorkDir:              workDir,
			Status:               LocalExecutionIntervening,
		})
	}

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

// interventionTargetMatches reports whether the executor currently holds the
// context for the given node — either mid-execution or after a soft-interrupted
// local takeover — so local intervention turns (intervene, handback, manual
// complete) may operate on it. Returns the held node when it matches.
func (e *TaskExecutor) interventionTargetMatches(taskID int32, nodeID string) (TaskNode, bool) {
	e.currentMu.Lock()
	running := e.running
	currentTaskID := e.taskID
	currentNode := e.node
	e.currentMu.Unlock()
	if !(running || e.softInterrupted.Load()) || currentTaskID != taskID || currentNode.ID != nodeID {
		return TaskNode{}, false
	}
	return currentNode, true
}

// IsInterventionAllowed reports whether a human can take over the currently
// held node locally. Intervention requires an execution context (running, or
// soft-interrupted takeover) and a tool session id to be present (so the turn
// can resume the same session).
func (e *TaskExecutor) IsInterventionAllowed() bool {
	e.currentMu.Lock()
	running := e.running
	e.currentMu.Unlock()
	if !running && !e.softInterrupted.Load() {
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
	currentNode, ok := e.interventionTargetMatches(taskID, nodeID)
	if !ok {
		return "", fmt.Errorf("%w: not holding task %d node %s", ErrInterventionNotHeld, taskID, nodeID)
	}

	e.mu.Lock()
	workDir := e.lastWorkDir
	sessionID := e.sessionID
	e.mu.Unlock()
	run := e.currentRun()

	t := e.selectTool()
	if t == nil {
		return "", fmt.Errorf("intervention turn: %w", ErrCodingToolUnavailable)
	}

	if claudeTool, ok := t.(*tool.ClaudeTool); ok && sessionID != "" {
		claudeTool.SetResumeSession(sessionID)
	} else if atomTool, ok := t.(*tool.AtomCodeTool); ok && sessionID != "" {
		atomTool.SetContinueSession(true)
	}

	if e.observer != nil {
		e.observer.OnExecutionStarted(LocalExecutionSession{
			WorkspaceID:          runContextWorkspace(run),
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
		if run != nil {
			if sendErr := run.Client.SendMessage(context.Background(), run.AgentID, taskID, nodeID, safeLine); sendErr != nil {
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
	currentNode, ok := e.interventionTargetMatches(taskID, nodeID)
	if !ok {
		return false
	}

	e.mu.Lock()
	workDir := e.lastWorkDir
	e.mu.Unlock()
	if e.observer != nil {
		e.observer.OnExecutionStarted(LocalExecutionSession{
			WorkspaceID:          runContextWorkspace(e.currentRun()),
			TaskID:               taskID,
			NodeID:               nodeID,
			NodeName:             currentNode.Name,
			ToolSessionIDPresent: true,
			WorkDir:              workDir,
			Status:               LocalExecutionRunning,
		})
	}

	// After a soft-interrupted takeover the execution goroutine has already
	// returned; clear the takeover flag and trigger a recovery pass so the
	// watcher re-enters Execute for the still in_progress node with the
	// persisted tool session.
	if !e.IsRunning() {
		e.softInterrupted.Store(false)
		if e.onResume != nil {
			go e.onResume()
		}
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
	currentNode, ok := e.interventionTargetMatches(taskID, nodeID)
	if !ok {
		return false
	}
	run := e.currentRun()
	if run == nil {
		return false
	}

	comment := fmt.Sprintf("Node %s completed manually by the local operator.", currentNode.Name)
	if err := run.Client.CompleteNode(context.Background(), run.AgentID, taskID, nodeID, comment); err != nil {
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

func (e *TaskExecutor) notifyExecutionStarted(run *RunContext, taskID int32, node TaskNode, workDir string) {
	if e.observer == nil {
		return
	}
	e.mu.Lock()
	sessionPresent := e.sessionID != ""
	e.mu.Unlock()
	e.observer.OnExecutionStarted(LocalExecutionSession{
		WorkspaceID:          run.WorkspaceID,
		TaskID:               taskID,
		NodeID:               node.ID,
		NodeName:             node.Name,
		Tool:                 e.cfg.Agent.Provider,
		ToolSessionIDPresent: sessionPresent,
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

// IsRunning returns whether the executor is currently running a task.
func (e *TaskExecutor) IsRunning() bool {
	e.currentMu.Lock()
	defer e.currentMu.Unlock()
	return e.running
}

// IsSoftInterrupted reports whether the executor holds a locally taken-over
// node (soft-interrupted, server still unaware). While true the watcher must
// neither recover the node nor claim new work: the human owns the executor
// until Handback clears the flag.
func (e *TaskExecutor) IsSoftInterrupted() bool {
	return e.softInterrupted.Load()
}

// CurrentTask returns the currently running task ID and node information.
func (e *TaskExecutor) CurrentTask() (taskID int32, node TaskNode, ok bool) {
	e.currentMu.Lock()
	defer e.currentMu.Unlock()
	if !e.running {
		return 0, TaskNode{}, false
	}
	return e.taskID, e.node, true
}

// GetGitManager returns the current Git manager, or nil if unavailable.
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

// SetToolFactory replaces the real coding-tool selector with the given
// factory. Production injection point (supervisor options).
func (e *TaskExecutor) SetToolFactory(factory func() tool.Tool) {
	e.mu.Lock()
	e.toolFactory = factory
	e.mu.Unlock()
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

// SetResumeCallback installs the callback Handback invokes after handing a
// soft-interrupted takeover back to the agent. The runtime wires it to a
// watcher recovery trigger.
func (e *TaskExecutor) SetResumeCallback(fn func()) {
	e.mu.Lock()
	e.onResume = fn
	e.mu.Unlock()
}

// SeedRunningForTest primes the executor as if Execute had started a node with
// the given identity and then been soft-interrupted, so intervention methods
// can be exercised without a full Execute run. Test-only.
func (e *TaskExecutor) SeedRunningForTest(run *RunContext, taskID int32, node TaskNode, workDir string) {
	e.currentMu.Lock()
	e.running = true
	e.taskID = taskID
	e.node = node
	e.currentMu.Unlock()
	e.mu.Lock()
	e.sessionID = ""
	e.lastWorkDir = workDir
	e.run = run
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

// selectTool selects the appropriate coding tool based on the configuration.
// selectTool selects the coding tool based on AgentInfo.Provider.
// Provider is a required field, validated at configuration load time.
//
// Returns:
//   - tool.Tool: the selected coding tool adapter instance
func (e *TaskExecutor) selectTool() tool.Tool {
	if e.toolFactory != nil {
		return e.toolFactory()
	}
	if e.toolFactoryForTest != nil {
		return testToolAdapter{inner: e.toolFactoryForTest()}
	}
	provider := e.cfg.Agent.Provider
	path := e.toolPath(provider)
	return tool.GetTool(provider, path)
}

// toolPath returns the executable path for the specified provider.
// Default values are handled by each tool's constructor; here it only reads from the configuration.
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

func (e *TaskExecutor) shouldPostNeedsInputComment(ctx context.Context, run *RunContext, taskID int32, nodeID, content string) bool {
	comments, err := run.Client.ListNodeComments(ctx, run.AgentID, taskID, nodeID)
	if err != nil {
		log.Printf("[executor] warning: failed to list node comments before needs_input dedupe: %v", err)
		return true
	}
	return !HasDuplicateAgentComment(comments, run.AgentID, "question", content)
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
	sb.WriteString("## Node Handoff\n\n")
	sb.WriteString(fmt.Sprintf("- Source Node: %s (%s)\n", node.Name, node.ID))
	sb.WriteString(fmt.Sprintf("- Executor: %s\n", e.cfg.Agent.Name))
	if gitReady {
		sb.WriteString(fmt.Sprintf("- Git Branch: %s\n", BranchName(taskID)))
	}
	if summary != "" {
		sb.WriteString("\n### Execution Summary\n")
		sb.WriteString(summary)
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n### Execution Summary\nCurrent node has completed, but no additional summary was generated.\n")
	}
	sb.WriteString("\n### Notes for Next Node\nPlease continue execution based on the current repository state and this handoff information. Code state is subject to the Git working directory, and non-code context is subject to the current node's comment area.")
	return sb.String()
}

// buildPromptWithClient builds the execution context prompt using the context
// injection layer, scoped to the execution's connection and identity.
func (e *TaskExecutor) buildPromptWithClient(client *Client, workspaceID, agentID string, taskID int32, node TaskNode, task Task, isResume bool, caps PromptCapabilities) (string, error) {
	context, err := BuildExecutionContextWithCapabilities(client, workspaceID, agentID, e.cfg, task, node, isResume, caps)
	if err != nil {
		return "", fmt.Errorf("build execution context: %w", err)
	}

	// Append collaboration tool instructions
	context += "\n## Collaboration Tools\n"
	context += "You can use the following tool to communicate with your team:\n"
	context += "- Request human input: output `<needs_input>your question here</needs_input>` to pause the task and let Teammate post the question once.\n"

	// Append output requirements
	context += "\n## Output Requirements\n"
	context += "- All task results MUST be written to files in the working directory.\n"
	context += "- Do not just output results to the terminal — they will not be saved.\n"
	context += "- Create appropriate files (e.g., markdown, code, config) for your deliverables.\n"
	context += "- Your changes will be automatically committed to the git repository.\n"

	return context, nil
}

// reportFailure reports the execution failure as a manual_intervention status, requesting
// human intervention.
//
// Args:
//   - taskID: task ID
//   - nodeID: node ID
//   - err: the error that caused the failure
func (e *TaskExecutor) reportFailure(run *RunContext, taskID int32, nodeID string, err error) {
	comment := fmt.Sprintf("Execution failed: %v\nManual intervention required.", err)
	if reportErr := run.Client.ManualIntervention(context.Background(), run.AgentID, taskID, nodeID, comment); reportErr != nil {
		log.Printf("[executor] failed to report manual intervention: %v", reportErr)
	}
}

// ParseNodeOrder extracts the node sequence number from the node name (e.g. "1. Requirements Analysis" returns 1).
func ParseNodeOrder(name string) int {
	order := 0
	fmt.Sscanf(name, "%d.", &order)
	return order
}

// generateSummary generates a work summary for the completed node,
// reusing the same coding tool session to retain the full context of the execution process.
func (e *TaskExecutor) generateSummary(workDir string, t tool.Tool, taskID int32, node TaskNode) string {
	// Set up session recovery so the summary call continues the same session
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
	// Cleanup: extract text from stream-json if necessary
	summary = cleanSummaryOutput(summary)
	if len(summary) > 500 {
		summary = summary[:497] + "..."
	}
	return summary
}

// cleanSummaryOutput extracts plain text from output that may contain JSON formatting.
func cleanSummaryOutput(output string) string {
	// If the output looks like stream-json, try to extract text from "result" type lines
	var texts []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			// Plain text line
			if line != "" {
				texts = append(texts, line)
			}
			continue
		}
		// Try to parse as JSON and extract text
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

// needsInputRegex is used to match the content inside <needs_input> tags.
var needsInputRegex = regexp.MustCompile(`(?s)<needs_input>(.*?)</needs_input>`)

// extractNeedsInputComment extracts the comment content from a <needs_input>...</needs_input> block.
func extractNeedsInputComment(output string) string {
	matches := needsInputRegex.FindStringSubmatch(output)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	// If there is no closing tag, try to get the text after <needs_input>
	if idx := strings.Index(output, "<needs_input>"); idx >= 0 {
		remaining := strings.TrimSpace(output[idx+len("<needs_input>"):])
		// Take at most 500 characters
		if len(remaining) > 500 {
			remaining = remaining[:500]
		}
		return remaining
	}
	return ""
}

// checkDiskQuota checks whether the disk usage of the working directory exceeds the quota limit.
// The default quota is 10GB, configurable via the TEAMMATE_DISK_QUOTA_GB environment variable.
// A warning is issued at 80% of the quota; an error is returned when the quota is exceeded.
//
// Args:
//   - workDir: the working directory path to check
//
// Returns:
//   - error: returns an error when the disk quota is exceeded
func (e *TaskExecutor) checkDiskQuota(workDir string) error {
	maxSizeGB := int64(10) // default 10GB
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
