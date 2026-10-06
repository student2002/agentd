// tool.go defines the abstract interface for coding tools and the implementation of multiple tool adapters.
//
// This file provides the tool abstraction layer used by the Agent Daemon when executing
// coding tasks, mainly including:
//   - Tool interface: defines four core methods Name / Execute / Stop / IsInstalled
//   - ClaudeTool: adapts Claude Code, supports stream-json output parsing and --resume session recovery
//   - OpenClawTool: adapts the OpenClaw coding tool
//   - OpenCodeTool: adapts the OpenCode coding tool
//   - ExecutionResult: execution result struct, containing output content, exit code, Token usage and session ID
//   - GetTool factory function: returns the corresponding tool adapter based on the provider name
//
// All tool adapters manage the child process tree via internal/agent/process, supporting cross-platform interruption.
// Log output is automatically desensitized for sensitive information.
package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	agentprocess "github.com/teammate/agentd/internal/agent/process"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
	"unicode/utf8"
)

// ExecutionResult contains the result of a coding tool execution.
type ExecutionResult struct {
	Output       string
	ExitCode     int
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	SessionID    string // Claude Code session ID, used for --resume
}

// ExecuteOptions contains the optional runtime configuration passed to the coding tool.
type ExecuteOptions struct {
	MCPConfigPath string
}

// Tool defines the interface for a coding tool; all coding tool adapters must implement this interface.
type Tool interface {
	// Name returns the tool name (e.g. "claude", "openclaw").
	Name() string

	// Execute runs the coding tool in the working directory with the given prompt.
	// onOutput is called for each line of stdout/stderr output.
	Execute(ctx context.Context, workDir, prompt string, options ExecuteOptions, onOutput func(string)) (*ExecutionResult, error)

	// Stop terminates the current execution.
	Stop() error

	// IsInstalled checks whether the tool is available on the system.
	IsInstalled() bool
}

// ToolSession is one historical conversation session of a coding tool,
// enumerated from the tool's own on-disk session store.
type ToolSession struct {
	ID        string
	Title     string
	WorkDir   string
	UpdatedAt time.Time
	Turns     int
}

// SessionLister is an optional adapter capability: adapters whose tool keeps
// a local session store implement it so callers can list and resume past
// conversations. Adapters without one simply omit the interface.
type SessionLister interface {
	ListSessions() ([]ToolSession, error)
}

// SessionMessage is one turn of a tool session's conversation transcript.
type SessionMessage struct {
	Role string // "user" | "assistant"
	Text string
}

// SessionReader is an optional adapter capability to load one historical
// session's conversation transcript from the tool's local store.
type SessionReader interface {
	ReadSession(id string) ([]SessionMessage, error)
}

// sessionReadCaps bound transcript loading: last N messages, each truncated.
const (
	maxSessionMessages     = 100
	maxSessionMessageChars = 8000
)

func validSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return false
	}
	return true
}

func trimSessionMessages(msgs []SessionMessage) []SessionMessage {
	if len(msgs) > maxSessionMessages {
		msgs = msgs[len(msgs)-maxSessionMessages:]
	}
	for i := range msgs {
		if len(msgs[i].Text) > maxSessionMessageChars {
			msgs[i].Text = msgs[i].Text[:maxSessionMessageChars] + "…"
		}
	}
	return msgs
}

func stopCommand(cmd *exec.Cmd, done <-chan struct{}) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	err := agentprocess.TerminateTree(cmd, agentprocess.DefaultTerminateTimeout)
	if done == nil {
		return err
	}

	select {
	case <-done:
	case <-time.After(agentprocess.DefaultTerminateTimeout):
	}
	return err
}

// --- Claude Code Tool ---

// ClaudeTool implements the Tool interface, adapting the Claude Code coding tool.
type ClaudeTool struct {
	path            string
	cmd             *exec.Cmd
	mu              sync.Mutex
	done            chan struct{} // closed when Execute completes
	resumeSessionID string        // if set, the next Execute call will use --resume
	onSessionID     func(string)  // callback when a session_id is captured
}

// NewClaudeTool creates a new Claude Code tool adapter.
func NewClaudeTool(path string) *ClaudeTool {
	return &ClaudeTool{path: path}
}

// Name returns "claude".
func (t *ClaudeTool) Name() string { return "claude" }

// IsInstalled checks whether claude is available.
func (t *ClaudeTool) IsInstalled() bool {
	_, err := exec.LookPath(t.path)
	return err == nil
}

// SetResumeSession sets the Claude session ID to resume on the next Execute call.
func (t *ClaudeTool) SetResumeSession(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resumeSessionID = sessionID
}

// SetSessionCallback sets the callback function invoked when a Claude session_id is captured.
func (t *ClaudeTool) SetSessionCallback(cb func(string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onSessionID = cb
}

// Execute runs Claude Code.
func (t *ClaudeTool) Execute(ctx context.Context, workDir, prompt string, options ExecuteOptions, onOutput func(string)) (*ExecutionResult, error) {
	t.mu.Lock()
	t.done = make(chan struct{})
	started := false
	var stopOnce sync.Once

	// Capture and clear the resume session ID while holding the lock
	resumeID := t.resumeSessionID
	t.resumeSessionID = ""

	defer func() {
		if !started {
			// Start() never succeeded — unlock before cleanup to avoid deadlock
			t.mu.Unlock()
		}
		t.mu.Lock()
		t.cmd = nil
		t.mu.Unlock()
		close(t.done)
	}()

	args := []string{
		"--print",
		"--verbose",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if options.MCPConfigPath != "" {
		// Equals form: space-separated "--mcp-config <path>" makes the CLI
		// consume the first prompt word as the config value.
		args = append(args, "--mcp-config="+options.MCPConfigPath)
	}

	t.cmd = exec.Command(t.path, args...)
	t.cmd.Dir = workDir
	agentprocess.PrepareCommand(t.cmd)
	ceilingDir := filepath.Dir(workDir)
	t.cmd.Env = append(os.Environ(),
		"GIT_CEILING_DIRECTORIES="+ceilingDir,
	)

	// The prompt must travel via stdin, never argv: Windows npm shims forward
	// arguments through cmd.exe `%*`, which treats embedded newlines as
	// command separators and truncates the prompt to its first line.
	stdin, _ := t.cmd.StdinPipe()
	stdout, _ := t.cmd.StdoutPipe()
	stderr, _ := t.cmd.StderrPipe()

	if err := t.cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude start: %w", err)
	}
	started = true

	go func() {
		defer stdin.Close()
		if _, err := io.WriteString(stdin, prompt); err != nil {
			log.Printf("[claude] warning: failed to write prompt to stdin: %v", err)
		}
	}()

	t.mu.Unlock() // release the lock after Start() so Stop() can access t.cmd

	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(func() { t.Stop() })
		case <-t.done:
			// Execute completed normally, no Stop needed
		}
	}()

	// Read stdout line by line (stream-json: each line is a JSON object)
	var outputLines []string
	var fullOutput strings.Builder
	var sessionIDMu sync.Mutex
	var capturedSessionID string
	textExtractor := &streamJSONTextExtractor{}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer
		for scanner.Scan() {
			line := scanner.Text()
			fullOutput.WriteString(line)
			fullOutput.WriteByte('\n')
			log.Printf("[tool:stdout] raw line: %s", line[:min(200, len(line))])
			// Extract displayable text from the stream-json line
			displayText := textExtractor.extract(line)
			if displayText != "" {
				sanitized := sanitizeLog(displayText)
				outputLines = append(outputLines, sanitized)
				log.Printf("[tool:stdout] extracted text: %s", sanitized[:min(200, len(sanitized))])
				if onOutput != nil {
					onOutput(sanitized)
				}
			} else {
				log.Printf("[tool:stdout] no text extracted from line")
			}
			// Extract the session_id from the system init event
			if sid := extractSessionID(line); sid != "" {
				sessionIDMu.Lock()
				capturedSessionID = sid
				sessionIDMu.Unlock()
				if t.onSessionID != nil {
					t.onSessionID(sid)
				}
			}
		}
		log.Printf("[tool:stdout] scanner finished, total output lines: %d", len(outputLines))
	}()

	// Read stderr line by line
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer
		for scanner.Scan() {
			line := sanitizeLog(scanner.Text())
			log.Printf("[tool:stderr] %s", line)
			if onOutput != nil {
				onOutput("[stderr] " + line)
			}
		}
	}()

	err := t.cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("claude wait: %w", err)
		}
	}

	result := &ExecutionResult{
		Output:   fullOutput.String(),
		ExitCode: exitCode,
	}
	sessionIDMu.Lock()
	result.SessionID = capturedSessionID
	sessionIDMu.Unlock()

	// Try to parse token usage from the JSON output
	result.InputTokens, result.OutputTokens = extractTokenUsage(result.Output)
	result.TotalTokens = result.InputTokens + result.OutputTokens

	return result, nil
}

// Stop terminates the current Claude Code execution: sends SIGTERM to the process group,
// then waits 5 seconds before SIGKILL.
func (t *ClaudeTool) Stop() error {
	t.mu.Lock()
	cmd := t.cmd
	done := t.done
	t.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	return stopCommand(cmd, done)
}

// --- OpenClaw Tool ---

// OpenClawTool implements the Tool interface, adapting the OpenClaw coding tool.
// OpenClaw uses the command `openclaw agent --message "..."` to run in headless mode.
type OpenClawTool struct {
	path string
	cmd  *exec.Cmd
	mu   sync.Mutex
	done chan struct{} // closed when Execute completes
}

// NewOpenClawTool creates a new OpenClaw tool adapter.
func NewOpenClawTool(path string) *OpenClawTool {
	return &OpenClawTool{path: path}
}

// Name returns "openclaw".
func (t *OpenClawTool) Name() string { return "openclaw" }

// IsInstalled checks whether openclaw is available.
func (t *OpenClawTool) IsInstalled() bool {
	_, err := exec.LookPath(t.path)
	return err == nil
}

// Execute runs OpenClaw.
// Uses `openclaw agent --message "..." --thinking high` for headless mode execution.
func (t *OpenClawTool) Execute(ctx context.Context, workDir, prompt string, options ExecuteOptions, onOutput func(string)) (*ExecutionResult, error) {
	t.mu.Lock()
	t.done = make(chan struct{})
	started := false
	var stopOnce sync.Once

	defer func() {
		if !started {
			t.mu.Unlock()
		}
		t.mu.Lock()
		t.cmd = nil
		t.mu.Unlock()
		close(t.done)
	}()

	if options.MCPConfigPath != "" {
		prompt += fmt.Sprintf("\n\nMCP server configuration is available at %s. Use the MCP servers described there when the tool supports MCP configuration files.", options.MCPConfigPath)
	}
	args := []string{"agent", "--message", prompt, "--thinking", "high"}
	t.cmd = exec.Command(t.path, args...)
	t.cmd.Dir = workDir
	agentprocess.PrepareCommand(t.cmd)
	ceilingDir := filepath.Dir(workDir)
	t.cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+ceilingDir)

	stdout, _ := t.cmd.StdoutPipe()
	stderr, _ := t.cmd.StderrPipe()

	if err := t.cmd.Start(); err != nil {
		return nil, fmt.Errorf("openclaw start: %w", err)
	}
	started = true
	t.mu.Unlock() // release the lock after Start() so Stop() can access t.cmd

	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(func() { t.Stop() })
		case <-t.done:
			// Execute completed normally, no Stop needed
		}
	}()

	var fullOutput strings.Builder
	go func() {
		scanLines(stdout, func(line string) {
			fullOutput.WriteString(line)
			fullOutput.WriteByte('\n')
			sanitized := sanitizeLog(line)
			if onOutput != nil {
				onOutput(sanitized)
			}
		})
	}()

	go func() {
		scanLines(stderr, func(line string) {
			line = sanitizeLog(line)
			log.Printf("[tool:stderr:openclaw] %s", line)
			if onOutput != nil {
				onOutput("[stderr] " + line)
			}
		})
	}()

	err := t.cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("openclaw wait: %w", err)
		}
	}

	result := &ExecutionResult{
		Output:   fullOutput.String(),
		ExitCode: exitCode,
	}

	result.InputTokens, result.OutputTokens = extractTokenUsage(result.Output)
	result.TotalTokens = result.InputTokens + result.OutputTokens

	return result, nil
}

// Stop terminates the current OpenClaw execution.
func (t *OpenClawTool) Stop() error {
	t.mu.Lock()
	cmd := t.cmd
	done := t.done
	t.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	return stopCommand(cmd, done)
}

// --- OpenCode Tool ---

// OpenCodeTool implements the Tool interface, adapting the OpenCode coding tool.
type OpenCodeTool struct {
	path string
	cmd  *exec.Cmd
	mu   sync.Mutex
	done chan struct{} // closed when Execute completes
}

// NewOpenCodeTool creates a new OpenCode tool adapter.
func NewOpenCodeTool(path string) *OpenCodeTool {
	return &OpenCodeTool{path: path}
}

// Name returns "opencode".
func (t *OpenCodeTool) Name() string { return "opencode" }

// IsInstalled checks whether opencode is available.
func (t *OpenCodeTool) IsInstalled() bool {
	_, err := exec.LookPath(t.path)
	return err == nil
}

// Execute runs OpenCode.
func (t *OpenCodeTool) Execute(ctx context.Context, workDir, prompt string, options ExecuteOptions, onOutput func(string)) (*ExecutionResult, error) {
	t.mu.Lock()
	t.done = make(chan struct{})
	started := false
	var stopOnce sync.Once

	defer func() {
		if !started {
			t.mu.Unlock()
		}
		t.mu.Lock()
		t.cmd = nil
		t.mu.Unlock()
		close(t.done)
	}()

	if options.MCPConfigPath != "" {
		prompt += fmt.Sprintf("\n\nMCP server configuration is available at %s. Use the MCP servers described there when the tool supports MCP configuration files.", options.MCPConfigPath)
	}
	t.cmd = exec.Command(t.path, "--prompt", prompt)
	t.cmd.Dir = workDir
	agentprocess.PrepareCommand(t.cmd)
	ceilingDir := filepath.Dir(workDir)
	t.cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+ceilingDir)

	stdout, _ := t.cmd.StdoutPipe()
	stderr, _ := t.cmd.StderrPipe()

	if err := t.cmd.Start(); err != nil {
		return nil, fmt.Errorf("opencode start: %w", err)
	}
	started = true
	t.mu.Unlock() // release the lock after Start() so Stop() can access t.cmd

	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(func() { t.Stop() })
		case <-t.done:
			// Execute completed normally, no Stop needed
		}
	}()

	var fullOutput strings.Builder
	go func() {
		scanLines(stdout, func(line string) {
			fullOutput.WriteString(line)
			fullOutput.WriteByte('\n')
			sanitized := sanitizeLog(line)
			if onOutput != nil {
				onOutput(sanitized)
			}
		})
	}()

	go func() {
		scanLines(stderr, func(line string) {
			line = sanitizeLog(line)
			log.Printf("[tool:stderr:opencode] %s", line)
			if onOutput != nil {
				onOutput("[stderr] " + line)
			}
		})
	}()

	err := t.cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("opencode wait: %w", err)
		}
	}

	result := &ExecutionResult{
		Output:   fullOutput.String(),
		ExitCode: exitCode,
	}

	result.InputTokens, result.OutputTokens = extractTokenUsage(result.Output)
	result.TotalTokens = result.InputTokens + result.OutputTokens

	return result, nil
}

// Stop terminates the current OpenCode execution.
func (t *OpenCodeTool) Stop() error {
	t.mu.Lock()
	cmd := t.cmd
	done := t.done
	t.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	return stopCommand(cmd, done)
}

// --- AtomCode Tool ---

// AtomCodeTool implements the Tool interface, adapting the AtomCode coding tool.
// AtomCode is a terminal AI coding agent written in Rust, supporting headless mode (-p)
// and session recovery (--continue).
// CLI: atomcode -p "prompt" [--continue] [-C workdir] [--model model]
type AtomCodeTool struct {
	path            string
	cmd             *exec.Cmd
	mu              sync.Mutex
	done            chan struct{} // closed when Execute completes
	continueSession bool          // next Execute uses --continue to resume the session
	resumeSessionID string        // if set, the next Execute call uses --resume <id>
}

// atomcodeResumeHint matches the stderr hint atomcode prints after a run:
// "To resume this session, run: atomcode -p "…" --resume <uuid>".
var atomcodeResumeHint = regexp.MustCompile(`--resume ([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// NewAtomCodeTool creates a new AtomCode tool adapter.
func NewAtomCodeTool(path string) *AtomCodeTool {
	return &AtomCodeTool{path: path}
}

// Name returns "atomcode".
func (t *AtomCodeTool) Name() string { return "atomcode" }

// IsInstalled checks whether atomcode is available.
func (t *AtomCodeTool) IsInstalled() bool {
	_, err := exec.LookPath(t.path)
	return err == nil
}

// SetContinueSession sets the next Execute call to use --continue to resume the session.
func (t *AtomCodeTool) SetContinueSession(continueSession bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.continueSession = continueSession
}

// SetResumeSession resumes the given atomcode session id on the next Execute call.
func (t *AtomCodeTool) SetResumeSession(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resumeSessionID = sessionID
}

// Execute runs AtomCode.
// Uses `atomcode -p "prompt"` for headless mode execution.
// In AtomCode headless mode, bash calls are auto-approved; other tools that require
// approval are rejected.
func (t *AtomCodeTool) Execute(ctx context.Context, workDir, prompt string, options ExecuteOptions, onOutput func(string)) (*ExecutionResult, error) {
	t.mu.Lock()
	t.done = make(chan struct{})
	started := false
	var stopOnce sync.Once

	continueSession := t.continueSession
	t.continueSession = false
	resumeID := t.resumeSessionID
	t.resumeSessionID = ""

	defer func() {
		if !started {
			t.mu.Unlock()
		}
		t.mu.Lock()
		t.cmd = nil
		t.mu.Unlock()
		close(t.done)
	}()

	if options.MCPConfigPath != "" {
		prompt += fmt.Sprintf("\n\nMCP server configuration is available at %s. Use the MCP servers described there when the tool supports MCP configuration files.", options.MCPConfigPath)
	}

	args := []string{"-p", prompt}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	} else if continueSession {
		args = append([]string{"--continue"}, args...)
	}
	t.cmd = exec.Command(t.path, args...)
	t.cmd.Dir = workDir
	agentprocess.PrepareCommand(t.cmd)
	ceilingDir := filepath.Dir(workDir)
	t.cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+ceilingDir)

	stdout, _ := t.cmd.StdoutPipe()
	stderr, _ := t.cmd.StderrPipe()

	if err := t.cmd.Start(); err != nil {
		return nil, fmt.Errorf("atomcode start: %w", err)
	}
	started = true
	t.mu.Unlock() // release the lock after Start() so Stop() can access t.cmd

	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(func() { t.Stop() })
		case <-t.done:
			// Execute completed normally, no Stop needed
		}
	}()

	var fullOutput strings.Builder
	go func() {
		scanLines(stdout, func(line string) {
			fullOutput.WriteString(line)
			fullOutput.WriteByte('\n')
			sanitized := sanitizeLog(line)
			if onOutput != nil {
				onOutput(sanitized)
			}
		})
	}()

	var sessionIDMu sync.Mutex
	var capturedSessionID string
	go func() {
		scanLines(stderr, func(line string) {
			line = sanitizeLog(line)
			log.Printf("[tool:stderr:atomcode] %s", line)
			if m := atomcodeResumeHint.FindStringSubmatch(line); m != nil {
				sessionIDMu.Lock()
				capturedSessionID = m[1]
				sessionIDMu.Unlock()
				// The resume hint is adapter metadata, not transcript content.
				return
			}
			if strings.TrimSpace(line) == "" {
				return
			}
			if onOutput != nil {
				onOutput("[stderr] " + line)
			}
		})
	}()

	err := t.cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("atomcode wait: %w", err)
		}
	}

	result := &ExecutionResult{
		Output:   fullOutput.String(),
		ExitCode: exitCode,
	}
	sessionIDMu.Lock()
	result.SessionID = capturedSessionID
	sessionIDMu.Unlock()

	result.InputTokens, result.OutputTokens = extractTokenUsage(result.Output)
	result.TotalTokens = result.InputTokens + result.OutputTokens

	return result, nil
}

// Stop terminates the current AtomCode execution.
func (t *AtomCodeTool) Stop() error {
	t.mu.Lock()
	cmd := t.cmd
	done := t.done
	t.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	return stopCommand(cmd, done)
}

// --- MiMoCode Tool ---

// MiMoCodeTool implements the Tool interface, adapting the MiMoCode coding tool.
// MiMoCode is a memory-enabled AI coding agent forked from OpenCode; its CLI is compatible
// with OpenCode.
// CLI: mimocode --prompt "prompt" (headless mode)
type MiMoCodeTool struct {
	path string
	cmd  *exec.Cmd
	mu   sync.Mutex
	done chan struct{} // closed when Execute completes
}

// NewMiMoCodeTool creates a new MiMoCode tool adapter.
func NewMiMoCodeTool(path string) *MiMoCodeTool {
	return &MiMoCodeTool{path: path}
}

// Name returns "mimocode".
func (t *MiMoCodeTool) Name() string { return "mimocode" }

// IsInstalled checks whether mimocode is available.
func (t *MiMoCodeTool) IsInstalled() bool {
	_, err := exec.LookPath(t.path)
	return err == nil
}

// Execute runs MiMoCode.
// MiMoCode is based on an OpenCode fork and uses the --prompt argument for headless mode execution.
func (t *MiMoCodeTool) Execute(ctx context.Context, workDir, prompt string, options ExecuteOptions, onOutput func(string)) (*ExecutionResult, error) {
	t.mu.Lock()
	t.done = make(chan struct{})
	started := false
	var stopOnce sync.Once

	defer func() {
		if !started {
			t.mu.Unlock()
		}
		t.mu.Lock()
		t.cmd = nil
		t.mu.Unlock()
		close(t.done)
	}()

	if options.MCPConfigPath != "" {
		prompt += fmt.Sprintf("\n\nMCP server configuration is available at %s. Use the MCP servers described there when the tool supports MCP configuration files.", options.MCPConfigPath)
	}
	t.cmd = exec.Command(t.path, "--prompt", prompt)
	t.cmd.Dir = workDir
	agentprocess.PrepareCommand(t.cmd)
	ceilingDir := filepath.Dir(workDir)
	t.cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+ceilingDir)

	stdout, _ := t.cmd.StdoutPipe()
	stderr, _ := t.cmd.StderrPipe()

	if err := t.cmd.Start(); err != nil {
		return nil, fmt.Errorf("mimocode start: %w", err)
	}
	started = true
	t.mu.Unlock() // release the lock after Start() so Stop() can access t.cmd

	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(func() { t.Stop() })
		case <-t.done:
			// Execute completed normally, no Stop needed
		}
	}()

	var fullOutput strings.Builder
	go func() {
		scanLines(stdout, func(line string) {
			fullOutput.WriteString(line)
			fullOutput.WriteByte('\n')
			sanitized := sanitizeLog(line)
			if onOutput != nil {
				onOutput(sanitized)
			}
		})
	}()

	go func() {
		scanLines(stderr, func(line string) {
			line = sanitizeLog(line)
			log.Printf("[tool:stderr:mimocode] %s", line)
			if onOutput != nil {
				onOutput("[stderr] " + line)
			}
		})
	}()

	err := t.cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("mimocode wait: %w", err)
		}
	}

	result := &ExecutionResult{
		Output:   fullOutput.String(),
		ExitCode: exitCode,
	}

	result.InputTokens, result.OutputTokens = extractTokenUsage(result.Output)
	result.TotalTokens = result.InputTokens + result.OutputTokens

	return result, nil
}

// Stop terminates the current MiMoCode execution.
func (t *MiMoCodeTool) Stop() error {
	t.mu.Lock()
	cmd := t.cmd
	done := t.done
	t.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	return stopCommand(cmd, done)
}

// --- Stream JSON Text Extraction ---

// streamJSONTextExtractor extracts displayable text from Claude stream-json
// lines, deduplicating the redundant text sources in the protocol: the same
// reply can arrive as incremental text deltas, as a complete assistant
// message, and as the final result. Preference order: live deltas, then the
// assistant message, and the result only as a fallback when nothing else
// carried the text. One instance per Execute run.
type streamJSONTextExtractor struct {
	sawDeltas bool
	emitted   bool
}

// extract parses a single stream-json line and returns its displayable text.
func (x *streamJSONTextExtractor) extract(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return ""
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return line // not valid JSON, return as-is
	}

	// Extract the type
	var eventType string
	if t, ok := obj["type"]; ok {
		_ = json.Unmarshal(t, &eventType)
	}

	switch eventType {
	case "assistant":
		// Assistant message: {"type":"assistant","message":{"content":[{"type":"text","text":"..."}]}}
		var msg struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if m, ok := obj["message"]; ok {
			_ = json.Unmarshal(m, &msg)
		}
		var texts []string
		for _, block := range msg.Content {
			if block.Type == "text" && block.Text != "" {
				texts = append(texts, block.Text)
			}
		}
		text := strings.Join(texts, "\n")
		if x.sawDeltas {
			// Deltas already streamed this text as it arrived.
			return ""
		}
		if text != "" {
			x.emitted = true
		}
		return text

	case "content_block_start":
		// A new content block starts
		var cb struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if c, ok := obj["content_block"]; ok {
			_ = json.Unmarshal(c, &cb)
		}
		if cb.Type == "text" && cb.Text != "" {
			x.sawDeltas = true
			x.emitted = true
			return cb.Text
		}
		return ""

	case "content_block_delta":
		// Incremental text delta
		var delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if d, ok := obj["delta"]; ok {
			_ = json.Unmarshal(d, &delta)
		}
		if delta.Type == "text_delta" && delta.Text != "" {
			x.sawDeltas = true
			x.emitted = true
			return delta.Text
		}
		return ""

	case "result":
		// Final result: {"type":"result","result":"...","usage":{...}}.
		// The result text duplicates the final assistant reply; emit it only
		// when no other source carried the text.
		if x.emitted {
			return ""
		}
		var resultStr string
		if r, ok := obj["result"]; ok {
			_ = json.Unmarshal(r, &resultStr)
		}
		return resultStr

	case "system":
		// System message (init, etc.) — skip displaying text
		return ""

	default:
		// Unknown event type — skip
		return ""
	}
}

// extractSessionID extracts the Claude Code session ID from the system init event.
// Returns an empty string if the line is not a system init event or has no session_id.
func extractSessionID(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return ""
	}
	var eventType string
	if t, ok := obj["type"]; ok {
		_ = json.Unmarshal(t, &eventType)
	}
	if eventType != "system" {
		return ""
	}
	var subtype string
	if s, ok := obj["subtype"]; ok {
		_ = json.Unmarshal(s, &subtype)
	}
	if subtype != "init" {
		return ""
	}
	var sessionID string
	if sid, ok := obj["session_id"]; ok {
		_ = json.Unmarshal(sid, &sessionID)
	}
	return sessionID
}

// --- Log Sanitization ---

// sensitivePatterns matches common key formats in logs, used for desensitization.
var sensitivePatterns = regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,}|ghp_[a-zA-Z0-9]{20,}|tm_[a-zA-Z0-9]{20,}|Bearer\s+\S+|api_key[=:]\s*\S+)`)

// sanitizeLog desensitizes a log line.
func sanitizeLog(line string) string {
	return sensitivePatterns.ReplaceAllString(line, "***REDACTED***")
}

// --- Encoding Normalization ---

// decodeLine normalizes a single line of tool stdout/stderr text to UTF-8.
//
// Background: the default code page of the Chinese Windows console is GBK(936). Native
// programs such as AtomCode/OpenClaw/OpenCode write GBK bytes directly to the pipe; Go's
// bufio.Scanner.Text() only copies bytes into a string without any encoding conversion, so
// invalid UTF-8 bytes enter the string as-is. These bytes are then written via
// PostNodeComment / <needs_input> into UTF-8 text columns in the database; the frontend
// renders them as UTF-8 and garbled text appears (some bytes may also be force-decoded to
// UTF-8 at some intermediate stage and become U+FFFD, causing irreversible corruption).
// Claude Code uses stream-json, and its output is already UTF-8, so it is unaffected.
//
// Strategy: if the line is already valid UTF-8, return it as-is (zero-cost fast path);
// otherwise decode it from GBK to UTF-8. If decoding still fails, fall back to ToValidUTF8
// cleansing with utf8.RuneError to avoid leaving invalid bytes behind.
func decodeLine(line string) string {
	if utf8.ValidString(line) {
		return line
	}
	decoded, err := io.ReadAll(transform.NewReader(strings.NewReader(line), simplifiedchinese.GBK.NewDecoder()))
	if err == nil {
		ds := string(decoded)
		if utf8.ValidString(ds) {
			return ds
		}
	}
	// GBK decoding failed or still contains invalid bytes: cleanse with replacement
	// characters to guarantee the downstream receives valid UTF-8.
	return strings.ToValidUTF8(line, "�")
}

// scanLines reads from the reader line by line; each line is normalized to UTF-8 via
// decodeLine and then passed through the fn callback.
// Extracted from the duplicated bufio.Scanner reading logic across tool adapters to unify
// the encoding fallback handling.
func scanLines(reader io.Reader, fn func(line string)) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer
	for scanner.Scan() {
		fn(decodeLine(scanner.Text()))
	}
}

// --- Token Usage Extraction ---

// scanJSONObjects returns every complete top-level JSON object in s, recovered
// by brace matching. Stream-json output is line-framed in principle, but some
// gateways emit objects with embedded newlines or concatenate objects without
// line breaks, so line-based splitting cannot be relied upon.
func scanJSONObjects(s string) []string {
	var objs []string
	depth, start := 0, -1
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case !inStr && c == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case !inStr && c == '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					objs = append(objs, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return objs
}

// extractTokenUsage extracts Token usage information from the output.
// Parsing is tolerant: the last object carrying top-level usage (e.g. the
// stream-json "result" event) wins; when no such object exists, per-message
// usage from "assistant" events is summed as a fallback, so successful runs
// still report usage when the gateway omits the closing result event.
func extractTokenUsage(output string) (int, int) {
	type usageJSON struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}

	resultIn, resultOut := 0, 0
	hasResult := false
	fallbackIn, fallbackOut := 0, 0

	for _, obj := range scanJSONObjects(output) {
		var event struct {
			Type    string `json:"type"`
			Usage   *usageJSON `json:"usage"`
			Message *struct {
				Usage *usageJSON `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(obj), &event); err != nil {
			continue
		}
		if event.Usage != nil && (event.Usage.InputTokens > 0 || event.Usage.OutputTokens > 0) {
			resultIn, resultOut = event.Usage.InputTokens, event.Usage.OutputTokens
			hasResult = true
			continue
		}
		if event.Type == "assistant" && event.Message != nil && event.Message.Usage != nil {
			fallbackIn += event.Message.Usage.InputTokens
			fallbackOut += event.Message.Usage.OutputTokens
		}
	}

	if hasResult {
		return resultIn, resultOut
	}
	return fallbackIn, fallbackOut
}

// --- Session stores (SessionLister implementations) ---

// maxListSessions caps the enumerated history per tool.
const maxListSessions = 50

// atomcodeSessionMeta is the subset of atomcode's `<id>.meta` catalog file
// needed for listing. Parsing is deliberately lenient: unknown fields are
// ignored, unreadable files are skipped, so a newer atomcode format stays
// listable until a breaking change.
type atomcodeSessionMeta struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	WorkingDir string `json:"working_dir"`
	UpdatedAt  int64  `json:"updated_at"` // epoch milliseconds
	TurnCount  int    `json:"turn_count"`
	Origin     string `json:"origin"`
}

// ListSessions enumerates atomcode sessions from $ATOMCODE_HOME/sessions
// (default ~/.atomcode/sessions), newest first. Scheduled-run sessions are
// hidden, matching atomcode's own resume picker.
func (t *AtomCodeTool) ListSessions() ([]ToolSession, error) {
	root, err := atomcodeSessionsRoot()
	if err != nil {
		return nil, err
	}
	return listAtomCodeSessions(root)
}

func atomcodeSessionsRoot() (string, error) {
	if home := os.Getenv("ATOMCODE_HOME"); home != "" {
		return filepath.Join(home, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".atomcode", "sessions"), nil
}

func listAtomCodeSessions(root string) ([]ToolSession, error) {
	buckets, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []ToolSession
	for _, bucket := range buckets {
		if !bucket.IsDir() {
			continue
		}
		metas, err := filepath.Glob(filepath.Join(root, bucket.Name(), "*.meta"))
		if err != nil {
			continue
		}
		for _, metaPath := range metas {
			data, err := os.ReadFile(metaPath)
			if err != nil {
				continue
			}
			var meta atomcodeSessionMeta
			if err := json.Unmarshal(data, &meta); err != nil {
				continue
			}
			if meta.ID == "" || meta.Origin == "scheduled" {
				continue
			}
			sessions = append(sessions, ToolSession{
				ID:        meta.ID,
				Title:     meta.Name,
				WorkDir:   meta.WorkingDir,
				UpdatedAt: time.UnixMilli(meta.UpdatedAt).UTC(),
				Turns:     meta.TurnCount,
			})
		}
	}
	sortSessions(sessions)
	return trimSessions(sessions), nil
}

// ListSessions enumerates claude sessions from $CLAUDE_CONFIG_DIR/projects
// (default ~/.claude/projects), newest first. Claude keeps no separate
// catalog: one `<session-id>.jsonl` per session; the real working directory
// is read from the transcript's first cwd field. Titles are unavailable
// without replaying full messages, so they stay empty.
func (t *ClaudeTool) ListSessions() ([]ToolSession, error) {
	root, err := claudeProjectsRoot()
	if err != nil {
		return nil, err
	}
	return listClaudeSessions(root)
}

func claudeProjectsRoot() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

func listClaudeSessions(root string) ([]ToolSession, error) {
	projects, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []ToolSession
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, project.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			// Subdirectories hold subagent transcripts, not top-level sessions.
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			sessions = append(sessions, ToolSession{
				ID:        strings.TrimSuffix(f.Name(), ".jsonl"),
				WorkDir:   claudeSessionCwd(filepath.Join(root, project.Name(), f.Name()), project.Name()),
				UpdatedAt: info.ModTime().UTC(),
			})
		}
	}
	sortSessions(sessions)
	return trimSessions(sessions), nil
}

// claudeSessionCwd reads the session's real working directory from the first
// transcript line carrying a cwd field. Claude Code encodes project folder
// names lossily (every non-alphanumeric character becomes '-'), so the folder
// name is only a fallback identifier, never a decoded path.
func claudeSessionCwd(path, fallback string) string {
	file, err := os.Open(path)
	if err != nil {
		return fallback
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for i := 0; scanner.Scan() && i < 50; i++ {
		var line struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err == nil && line.Cwd != "" {
			return line.Cwd
		}
	}
	return fallback
}

func sortSessions(sessions []ToolSession) {
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt) })
}

func trimSessions(sessions []ToolSession) []ToolSession {
	if len(sessions) > maxListSessions {
		return sessions[:maxListSessions]
	}
	return sessions
}

// ReadSession loads an atomcode session transcript from its `<id>.snapshot`
// file next to the `.meta` catalog. System and Tool messages and empty
// assistant placeholders (tool-call turns) are skipped.
func (t *AtomCodeTool) ReadSession(id string) ([]SessionMessage, error) {
	if !validSessionID(id) {
		return nil, fmt.Errorf("atomcode: invalid session id")
	}
	root, err := atomcodeSessionsRoot()
	if err != nil {
		return nil, err
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", id+".snapshot"))
	if len(matches) == 0 {
		return nil, fmt.Errorf("atomcode: session %s not found", id)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		return nil, err
	}
	var snap struct {
		Messages []struct {
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("atomcode: parse snapshot: %w", err)
	}
	out := []SessionMessage{}
	for _, m := range snap.Messages {
		switch m.Role {
		case "User":
			if strings.TrimSpace(m.Text) != "" {
				out = append(out, SessionMessage{Role: "user", Text: m.Text})
			}
		case "Assistant":
			if strings.TrimSpace(m.Text) != "" {
				out = append(out, SessionMessage{Role: "assistant", Text: m.Text})
			}
		}
	}
	return trimSessionMessages(out), nil
}

// ReadSession loads a claude session transcript from its `<id>.jsonl` file.
// Only user and assistant text is kept; tool results and meta lines are
// skipped.
func (t *ClaudeTool) ReadSession(id string) ([]SessionMessage, error) {
	if !validSessionID(id) {
		return nil, fmt.Errorf("claude: invalid session id")
	}
	root, err := claudeProjectsRoot()
	if err != nil {
		return nil, err
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", id+".jsonl"))
	if len(matches) == 0 {
		return nil, fmt.Errorf("claude: session %s not found", id)
	}
	file, err := os.Open(matches[0])
	if err != nil {
		return nil, err
	}
	defer file.Close()

	out := []SessionMessage{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		var line struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil || line.Message == nil {
			continue
		}
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}
		text := claudeContentText(msg.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		switch line.Type {
		case "user":
			out = append(out, SessionMessage{Role: "user", Text: text})
		case "assistant":
			out = append(out, SessionMessage{Role: "assistant", Text: text})
		}
	}
	return trimSessionMessages(out), nil
}

// claudeContentText extracts displayable text from a claude message content
// field, which is either a plain string or an array of typed blocks.
func claudeContentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(content, &asString); err == nil {
		return asString
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// --- Tool Factory ---

// GetTool returns the coding tool adapter corresponding to the provider name.
func GetTool(provider string, path string) Tool {
	switch strings.ToLower(provider) {
	case "claude":
		return NewClaudeTool(path)
	case "openclaw":
		return NewOpenClawTool(path)
	case "opencode":
		return NewOpenCodeTool(path)
	case "atomcode":
		return NewAtomCodeTool(path)
	case "mimocode":
		return NewMiMoCodeTool(path)
	default:
		return NewClaudeTool(path) // use Claude by default
	}
}
