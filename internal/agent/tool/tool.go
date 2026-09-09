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
		args = append(args, "--mcp-config", options.MCPConfigPath)
	}
	args = append(args, prompt)

	t.cmd = exec.Command(t.path, args...)
	t.cmd.Dir = workDir
	agentprocess.PrepareCommand(t.cmd)
	ceilingDir := filepath.Dir(workDir)
	t.cmd.Env = append(os.Environ(),
		"GIT_CEILING_DIRECTORIES="+ceilingDir,
	)

	stdout, _ := t.cmd.StdoutPipe()
	stderr, _ := t.cmd.StderrPipe()

	if err := t.cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude start: %w", err)
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

	// Read stdout line by line (stream-json: each line is a JSON object)
	var outputLines []string
	var fullOutput strings.Builder
	var sessionIDMu sync.Mutex
	var capturedSessionID string
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer
		for scanner.Scan() {
			line := scanner.Text()
			fullOutput.WriteString(line)
			fullOutput.WriteByte('\n')
			log.Printf("[tool:stdout] raw line: %s", line[:min(200, len(line))])
			// Extract displayable text from the stream-json line
			displayText := extractStreamJSONText(line)
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
}

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
	if continueSession {
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

	go func() {
		scanLines(stderr, func(line string) {
			line = sanitizeLog(line)
			log.Printf("[tool:stderr:atomcode] %s", line)
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

// extractStreamJSONText parses a single line of Claude stream-json output and returns the displayable text.
// Each line is a JSON object containing a "type" field that identifies the event type.
func extractStreamJSONText(line string) string {
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
		return strings.Join(texts, "\n")

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
			return delta.Text
		}
		return ""

	case "result":
		// Final result: {"type":"result","result":"...","usage":{...}}
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

// extractTokenUsage extracts Token usage information from the output.
func extractTokenUsage(output string) (int, int) {
	// Try to parse the JSON output to obtain token usage
	var result struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	// Try to parse as a single JSON object
	if err := json.Unmarshal([]byte(output), &result); err == nil {
		return result.Usage.InputTokens, result.Usage.OutputTokens
	}

	// Try to find a JSON line containing usage information
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.Contains(line, "usage") {
			if err := json.Unmarshal([]byte(line), &result); err == nil {
				if result.Usage.InputTokens > 0 || result.Usage.OutputTokens > 0 {
					return result.Usage.InputTokens, result.Usage.OutputTokens
				}
			}
		}
	}

	return 0, 0
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
