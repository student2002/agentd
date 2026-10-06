// chat.go implements the direct-chat runtime: single-run conversational
// orchestration over the local coding tools, independent of task nodes and
// of HTTP. Callers (local control API, tests, future CLI commands) drive the
// same ChatManager API.
//
// Layering: ChatManager sits above the tool adapter layer (internal/agent/tool)
// and beside TaskExecutor — it shares the adapters and the desensitization
// pipeline but owns no server reporting, no git, and no task state.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/teammate/agentd/internal/agent/tool"
)

// chatProviders lists the providers selectable in direct chat, in UI order.
var chatProviders = []string{"atomcode", "claude", "openclaw", "opencode", "mimocode"}

var chatProviderSet = func() map[string]bool {
	set := make(map[string]bool, len(chatProviders))
	for _, p := range chatProviders {
		set[p] = true
	}
	return set
}()

// ChatNodeID tags direct-chat output lines inside the chat LogBuffer, keeping
// them separate from task execution logs.
const ChatNodeID = "chat"

// Direct-chat lifecycle errors, mapped to HTTP statuses by the local server.
var (
	ErrChatBusy            = errors.New("chat: another run is already active")
	ErrChatUnknownProvider = errors.New("chat: unknown provider")
	ErrChatEmptyPrompt     = errors.New("chat: prompt is empty")
	ErrChatWorkDirInvalid  = errors.New("chat: workdir does not exist or is not a directory")
)

// Chat run statuses.
const (
	ChatRunCompleted = "completed"
	ChatRunFailed    = "failed"
	ChatRunStopped   = "stopped"
)

// ChatToolInfo describes one selectable coding tool.
type ChatToolInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Installed bool   `json:"installed"`
}

// ChatActiveRun describes the run currently in flight.
type ChatActiveRun struct {
	Provider  string    `json:"provider"`
	WorkDir   string    `json:"work_dir"`
	Prompt    string    `json:"prompt"`
	StartedAt time.Time `json:"started_at"`
}

// ChatRunResult is the outcome metadata of one direct-chat turn.
type ChatRunResult struct {
	Status       string    `json:"status"` // completed | failed | stopped
	Provider     string    `json:"provider"`
	Prompt       string    `json:"prompt"`
	StartedAt    time.Time `json:"started_at"`
	DurationMs   int64     `json:"duration_ms"`
	ExitCode     int       `json:"exit_code"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	TotalTokens  int       `json:"total_tokens"`
	SessionID    string    `json:"session_id,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// ChatSessionInfo is the resume memory of the current conversation. A session
// stays valid while provider and workdir are unchanged; ResetSession drops it.
type ChatSessionInfo struct {
	Provider  string `json:"provider"`
	WorkDir   string `json:"work_dir"`
	TurnCount int    `json:"turn_count"`
	SessionID string `json:"session_id,omitempty"` // claude: explicit --resume id
}

// ChatState is the serializable view of the manager used by the local API.
type ChatState struct {
	Active     *ChatActiveRun   `json:"active,omitempty"`
	LastResult *ChatRunResult   `json:"last_result,omitempty"`
	Session    *ChatSessionInfo `json:"session,omitempty"`
}

// ChatSessionSummary is one historical conversation session enumerated from a
// coding tool's own on-disk session store.
type ChatSessionSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	WorkDir   string    `json:"work_dir"`
	UpdatedAt time.Time `json:"updated_at"`
	Turns     int       `json:"turns"`
}

// ChatSessionsResult reports whether the provider supports session listing
// (the adapter implements tool.SessionLister) and its sessions, newest first.
type ChatSessionsResult struct {
	Supported bool                `json:"supported"`
	Sessions  []ChatSessionSummary `json:"sessions"`
}

// ChatObserver receives direct-chat lifecycle notifications. Callbacks must
// not block. It is the integration point for event publishing, CLI frontends,
// and tests.
type ChatObserver interface {
	OnChatStarted(run ChatActiveRun)
	OnChatFinished(result ChatRunResult)
}

// ChatManagerOptions carries optional dependencies for NewChatManager.
type ChatManagerOptions struct {
	// ToolFactory creates tool adapters; defaults to tool.GetTool.
	ToolFactory func(provider, path string) tool.Tool
}

// ChatManager orchestrates direct-chat runs. Concurrency contract: at most one
// active run at a time (Chat returns ErrChatBusy otherwise); node execution is
// unaffected because each run owns a dedicated tool instance and workdir.
type ChatManager struct {
	cfg      *Config
	buffer   *LogBuffer
	observer ChatObserver

	toolFactory func(provider, path string) tool.Tool

	mu         sync.Mutex
	active     *chatRun
	session    *ChatSessionInfo
	lastResult *ChatRunResult
}

type chatRun struct {
	info     ChatActiveRun
	cancel   context.CancelFunc
	t        tool.Tool
	stopReq  bool
	stopOnce sync.Once
}

// NewChatManager creates a manager. buffer may be nil — a default 2000-line
// buffer is created; observer may be nil.
func NewChatManager(cfg *Config, buffer *LogBuffer, observer ChatObserver, opts ChatManagerOptions) *ChatManager {
	if buffer == nil {
		buffer = NewLogBuffer(LogBufferDefaultCapacity)
	}
	factory := opts.ToolFactory
	if factory == nil {
		factory = tool.GetTool
	}
	return &ChatManager{
		cfg:         cfg,
		buffer:      buffer,
		observer:    observer,
		toolFactory: factory,
	}
}

// Buffer returns the chat log buffer used for output streaming.
func (m *ChatManager) Buffer() *LogBuffer { return m.buffer }

// Tools returns provider/path/installed triples for every chat provider.
func (m *ChatManager) Tools() []ChatToolInfo {
	out := make([]ChatToolInfo, 0, len(chatProviders))
	for _, name := range chatProviders {
		path := chatToolPath(m.cfg, name)
		out = append(out, ChatToolInfo{
			Name:      name,
			Path:      path,
			Installed: m.toolFactory(name, path).IsInstalled(),
		})
	}
	return out
}

// Chat starts one conversational run in the background and returns its
// descriptor. workDir may be empty — a per-provider scratch directory under
// the workspace root (or the system temp dir) is created on demand.
// resumeSessionID, when non-empty, resumes that tool session for exactly
// this run; continuation afterwards follows the manager's session memory.
func (m *ChatManager) Chat(provider, prompt, workDir, resumeSessionID string) (*ChatActiveRun, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	resumeSessionID = strings.TrimSpace(resumeSessionID)
	if strings.TrimSpace(prompt) == "" {
		return nil, ErrChatEmptyPrompt
	}
	if !chatProviderSet[provider] {
		return nil, fmt.Errorf("%w: %s", ErrChatUnknownProvider, provider)
	}
	workDir, err := m.resolveWorkDir(provider, workDir)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.active != nil {
		m.mu.Unlock()
		return nil, ErrChatBusy
	}
	// Session continuation follows the current conversation thread: sending
	// to a different provider than the remembered session invalidates it.
	if m.session != nil && m.session.Provider != provider {
		m.session = nil
	}
	t := m.toolFactory(provider, chatToolPath(m.cfg, provider))
	if resumeSessionID != "" {
		applyResumeSession(provider, resumeSessionID, t)
	} else {
		m.applyResumeLocked(provider, workDir, t)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &chatRun{
		info: ChatActiveRun{
			Provider:  provider,
			WorkDir:   workDir,
			Prompt:    prompt,
			StartedAt: time.Now().UTC(),
		},
		cancel: cancel,
		t:      t,
	}
	m.active = run
	m.mu.Unlock()

	go m.run(ctx, run)

	info := run.info
	return &info, nil
}

// Stop terminates the active run, if any. Idempotent.
func (m *ChatManager) Stop() error {
	m.mu.Lock()
	run := m.active
	m.mu.Unlock()
	if run == nil {
		return nil
	}
	run.stopOnce.Do(func() {
		m.mu.Lock()
		run.stopReq = true
		m.mu.Unlock()
		run.cancel()
		_ = run.t.Stop()
	})
	return nil
}

// ResetSession drops the resume memory so the next run starts a fresh session.
func (m *ChatManager) ResetSession() {
	m.mu.Lock()
	m.session = nil
	m.mu.Unlock()
}

// State returns a copy of the current manager state.
func (m *ChatManager) State() ChatState {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := ChatState{LastResult: m.lastResult, Session: m.session}
	if m.active != nil {
		info := m.active.info
		state.Active = &info
	}
	return state
}

// ChatMessage is one turn of a loaded session transcript.
type ChatMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// ChatSessionMessages reports whether the provider supports transcript
// loading and the session's messages, oldest first.
type ChatSessionMessages struct {
	Supported bool         `json:"supported"`
	Messages  []ChatMessage `json:"messages"`
}

// Sessions enumerates the provider's own historical sessions through the
// adapter's optional tool.SessionLister capability. Providers whose adapter
// keeps no local session store report Supported=false.
func (m *ChatManager) Sessions(provider string) ChatSessionsResult {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !chatProviderSet[provider] {
		return ChatSessionsResult{Sessions: []ChatSessionSummary{}}
	}
	t := m.toolFactory(provider, chatToolPath(m.cfg, provider))
	lister, ok := t.(tool.SessionLister)
	if !ok {
		return ChatSessionsResult{Sessions: []ChatSessionSummary{}}
	}
	result := ChatSessionsResult{Supported: true, Sessions: []ChatSessionSummary{}}
	sessions, err := lister.ListSessions()
	if err != nil {
		log.Printf("[chat] list %s sessions: %v", provider, err)
		return result
	}
	for _, s := range sessions {
		result.Sessions = append(result.Sessions, ChatSessionSummary{
			ID:        s.ID,
			Title:     s.Title,
			WorkDir:   s.WorkDir,
			UpdatedAt: s.UpdatedAt,
			Turns:     s.Turns,
		})
	}
	return result
}

// SessionMessages loads one historical session's transcript through the
// adapter's optional tool.SessionReader capability. A read failure degrades
// to an empty transcript (supported, zero messages) — the session stays
// resumable by id regardless.
func (m *ChatManager) SessionMessages(provider, id string) ChatSessionMessages {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !chatProviderSet[provider] {
		return ChatSessionMessages{Messages: []ChatMessage{}}
	}
	t := m.toolFactory(provider, chatToolPath(m.cfg, provider))
	reader, ok := t.(tool.SessionReader)
	if !ok {
		return ChatSessionMessages{Messages: []ChatMessage{}}
	}
	result := ChatSessionMessages{Supported: true, Messages: []ChatMessage{}}
	msgs, err := reader.ReadSession(strings.TrimSpace(id))
	if err != nil {
		log.Printf("[chat] read %s session %s: %v", provider, id, err)
		return result
	}
	for _, msg := range msgs {
		result.Messages = append(result.Messages, ChatMessage{Role: msg.Role, Text: msg.Text})
	}
	return result
}

// run executes one turn on a background goroutine and records the outcome.
func (m *ChatManager) run(ctx context.Context, run *chatRun) {
	if m.observer != nil {
		m.observer.OnChatStarted(run.info)
	}

	result, err := run.t.Execute(ctx, run.info.WorkDir, run.info.Prompt, tool.ExecuteOptions{}, func(line string) {
		m.buffer.Append(LogLine{
			NodeID: ChatNodeID,
			Line:   desensitizeOutputLine(line),
			Ts:     time.Now().UTC(),
		})
	})
	duration := time.Since(run.info.StartedAt)

	m.mu.Lock()
	res := ChatRunResult{
		Provider:   run.info.Provider,
		Prompt:     run.info.Prompt,
		StartedAt:  run.info.StartedAt,
		DurationMs: duration.Milliseconds(),
	}
	switch {
	case run.stopReq:
		res.Status = ChatRunStopped
	case err != nil:
		res.Status = ChatRunFailed
		res.Error = err.Error()
	default:
		res.Status = ChatRunCompleted
	}
	if result != nil {
		res.ExitCode = result.ExitCode
		res.InputTokens = result.InputTokens
		res.OutputTokens = result.OutputTokens
		res.TotalTokens = result.TotalTokens
		res.SessionID = result.SessionID
	}
	if res.Status == ChatRunCompleted {
		turns := 0
		if m.session != nil && m.session.Provider == res.Provider && m.session.WorkDir == run.info.WorkDir {
			turns = m.session.TurnCount
		}
		m.session = &ChatSessionInfo{
			Provider:  res.Provider,
			WorkDir:   run.info.WorkDir,
			TurnCount: turns + 1,
			SessionID: res.SessionID,
		}
	} else if res.SessionID != "" && m.session != nil {
		// A failed claude run may still have advanced the session id.
		m.session.SessionID = res.SessionID
	}
	m.lastResult = &res
	m.active = nil
	m.mu.Unlock()

	if m.observer != nil {
		m.observer.OnChatFinished(res)
	}
}

// applyResumeLocked wires the session-resume strategy onto a fresh tool
// instance. This switch is the single extension point when an adapter gains
// resume support. Caller must hold m.mu.
func (m *ChatManager) applyResumeLocked(provider, workDir string, t tool.Tool) {
	if m.session == nil || m.session.Provider != provider || m.session.WorkDir != workDir {
		return
	}
	switch provider {
	case "claude":
		if ct, ok := t.(*tool.ClaudeTool); ok && m.session.SessionID != "" {
			ct.SetResumeSession(m.session.SessionID)
		}
	case "atomcode":
		if at, ok := t.(*tool.AtomCodeTool); ok {
			// An explicitly captured session id beats --continue, which
			// resumes whatever ran last in the workdir.
			if m.session.SessionID != "" {
				at.SetResumeSession(m.session.SessionID)
			} else {
				at.SetContinueSession(true)
			}
		}
	}
}

// applyResumeSession wires a user-picked session id onto a fresh tool
// instance for a one-shot resume.
func applyResumeSession(provider, sessionID string, t tool.Tool) {
	switch provider {
	case "claude":
		if ct, ok := t.(*tool.ClaudeTool); ok {
			ct.SetResumeSession(sessionID)
		}
	case "atomcode":
		if at, ok := t.(*tool.AtomCodeTool); ok {
			at.SetResumeSession(sessionID)
		}
	}
}

// resolveWorkDir validates or creates the run's working directory.
func (m *ChatManager) resolveWorkDir(provider, workDir string) (string, error) {
	if strings.TrimSpace(workDir) == "" {
		root := ""
		if m.cfg != nil {
			root = m.cfg.Workspace.Root
		}
		if root == "" {
			root = filepath.Join(os.TempDir(), "teammate-chat")
		}
		dir := filepath.Join(root, "chat", provider)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("chat: create scratch workdir: %w", err)
		}
		return dir, nil
	}
	info, err := os.Stat(workDir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrChatWorkDirInvalid, workDir)
	}
	return filepath.Clean(workDir), nil
}

// chatToolPath resolves a provider to its configured executable path.
func chatToolPath(cfg *Config, provider string) string {
	if cfg == nil {
		return ""
	}
	switch strings.ToLower(provider) {
	case "claude":
		return cfg.Tools.Claude.Path
	case "openclaw":
		return cfg.Tools.OpenClaw.Path
	case "opencode":
		return cfg.Tools.OpenCode.Path
	case "atomcode":
		return cfg.Tools.AtomCode.Path
	case "mimocode":
		return cfg.Tools.MiMoCode.Path
	default:
		return ""
	}
}
