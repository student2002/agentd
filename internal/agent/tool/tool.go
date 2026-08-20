// tool.go 定义编码工具的抽象接口和多种工具适配器的实现。
//
// 本文件提供 Agent Daemon 执行编码任务时的工具抽象层，主要包括：
//   - Tool 接口：定义 Name / Execute / Stop / IsInstalled 四个核心方法
//   - ClaudeTool：适配 Claude Code，支持 stream-json 输出解析和 --resume 会话恢复
//   - OpenClawTool：适配 OpenClaw 编码工具
//   - OpenCodeTool：适配 OpenCode 编码工具
//   - ExecutionResult：执行结果结构，包含输出内容、退出码、Token 用量和会话 ID
//   - GetTool 工厂函数：根据 provider 名称返回对应的工具适配器
//
// 所有工具适配器均通过 internal/agent/process 管理子进程树，支持跨平台中断。
// 日志输出自动进行敏感信息脱敏处理。
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

// ExecutionResult 包含编码工具执行的结果。
type ExecutionResult struct {
	Output       string
	ExitCode     int
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	SessionID    string // Claude Code 会话 ID，用于 --resume
}

// ExecuteOptions 包含传递给编码工具的可选运行时配置。
type ExecuteOptions struct {
	MCPConfigPath string
}

// Tool 定义了编码工具的接口，所有编码工具适配器必须实现此接口。
type Tool interface {
	// Name 返回工具名称（如 "claude"、"openclaw"）。
	Name() string

	// Execute 在工作目录中使用给定的 prompt 运行编码工具。
	// onOutput 在每行 stdout/stderr 输出时被调用。
	Execute(ctx context.Context, workDir, prompt string, options ExecuteOptions, onOutput func(string)) (*ExecutionResult, error)

	// Stop 终止当前执行。
	Stop() error

	// IsInstalled 检查工具是否在系统上可用。
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

// ClaudeTool 实现了 Tool 接口，适配 Claude Code 编码工具。
type ClaudeTool struct {
	path            string
	cmd             *exec.Cmd
	mu              sync.Mutex
	done            chan struct{} // Execute 完成时关闭
	resumeSessionID string        // 如果设置，下次 Execute 调用将使用 --resume
	onSessionID     func(string)  // 捕获到 session_id 时的回调
}

// NewClaudeTool 创建一个新的 Claude Code 工具适配器。
func NewClaudeTool(path string) *ClaudeTool {
	return &ClaudeTool{path: path}
}

// Name 返回 "claude"。
func (t *ClaudeTool) Name() string { return "claude" }

// IsInstalled 检查 claude 是否可用。
func (t *ClaudeTool) IsInstalled() bool {
	_, err := exec.LookPath(t.path)
	return err == nil
}

// SetResumeSession 设置下次 Execute 调用时恢复的 Claude 会话 ID。
func (t *ClaudeTool) SetResumeSession(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resumeSessionID = sessionID
}

// SetSessionCallback 设置当捕获到 Claude session_id 时的回调函数。
func (t *ClaudeTool) SetSessionCallback(cb func(string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onSessionID = cb
}

// Execute 运行 Claude Code。
func (t *ClaudeTool) Execute(ctx context.Context, workDir, prompt string, options ExecuteOptions, onOutput func(string)) (*ExecutionResult, error) {
	t.mu.Lock()
	t.done = make(chan struct{})
	started := false
	var stopOnce sync.Once

	// 持锁时捕获并清除恢复会话 ID
	resumeID := t.resumeSessionID
	t.resumeSessionID = ""

	defer func() {
		if !started {
			// Start() 从未成功——在清理前解锁以避免死锁
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
	t.mu.Unlock() // 在 Start() 后释放锁，使 Stop() 能访问 t.cmd

	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(func() { t.Stop() })
		case <-t.done:
			// Execute 正常完成，无需 Stop
		}
	}()

	// 逐行读取 stdout（stream-json：每行是一个 JSON 对象）
	var outputLines []string
	var fullOutput strings.Builder
	var sessionIDMu sync.Mutex
	var capturedSessionID string
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB 缓冲区
		for scanner.Scan() {
			line := scanner.Text()
			fullOutput.WriteString(line)
			fullOutput.WriteByte('\n')
			log.Printf("[tool:stdout] raw line: %s", line[:min(200, len(line))])
			// 从 stream-json 行提取可显示的文本
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
			// 从系统初始化事件中提取 session_id
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

	// 逐行读取 stderr
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB 缓冲区
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

	// 尝试从 JSON 输出解析 token 用量
	result.InputTokens, result.OutputTokens = extractTokenUsage(result.Output)
	result.TotalTokens = result.InputTokens + result.OutputTokens

	return result, nil
}

// Stop 终止 Claude Code 当前执行，向进程组发送 SIGTERM 后等待 5 秒再 SIGKILL。
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

// OpenClawTool 实现了 Tool 接口，适配 OpenClaw 编码工具。
// OpenClaw 使用 openclaw agent --message "..." 命令执行无头模式。
type OpenClawTool struct {
	path string
	cmd  *exec.Cmd
	mu   sync.Mutex
	done chan struct{} // Execute 完成时关闭
}

// NewOpenClawTool 创建一个新的 OpenClaw 工具适配器。
func NewOpenClawTool(path string) *OpenClawTool {
	return &OpenClawTool{path: path}
}

// Name 返回 "openclaw"。
func (t *OpenClawTool) Name() string { return "openclaw" }

// IsInstalled 检查 openclaw 是否可用。
func (t *OpenClawTool) IsInstalled() bool {
	_, err := exec.LookPath(t.path)
	return err == nil
}

// Execute 运行 OpenClaw。
// 使用 openclaw agent --message "..." --thinking high 进行无头模式执行。
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
	t.mu.Unlock() // 在 Start() 后释放锁，使 Stop() 能访问 t.cmd

	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(func() { t.Stop() })
		case <-t.done:
			// Execute 正常完成，无需 Stop
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

// Stop 终止 OpenClaw 当前执行。
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

// OpenCodeTool 实现了 Tool 接口，适配 OpenCode 编码工具。
type OpenCodeTool struct {
	path string
	cmd  *exec.Cmd
	mu   sync.Mutex
	done chan struct{} // Execute 完成时关闭
}

// NewOpenCodeTool 创建一个新的 OpenCode 工具适配器。
func NewOpenCodeTool(path string) *OpenCodeTool {
	return &OpenCodeTool{path: path}
}

// Name 返回 "opencode"。
func (t *OpenCodeTool) Name() string { return "opencode" }

// IsInstalled 检查 opencode 是否可用。
func (t *OpenCodeTool) IsInstalled() bool {
	_, err := exec.LookPath(t.path)
	return err == nil
}

// Execute 运行 OpenCode。
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
	t.mu.Unlock() // 在 Start() 后释放锁，使 Stop() 能访问 t.cmd

	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(func() { t.Stop() })
		case <-t.done:
			// Execute 正常完成，无需 Stop
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

// Stop 终止 OpenCode 当前执行。
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

// AtomCodeTool 实现了 Tool 接口，适配 AtomCode 编码工具。
// AtomCode 是 Rust 编写的终端 AI 编码代理，支持无头模式 (-p) 和会话恢复 (--continue)。
// CLI: atomcode -p "prompt" [--continue] [-C workdir] [--model model]
type AtomCodeTool struct {
	path            string
	cmd             *exec.Cmd
	mu              sync.Mutex
	done            chan struct{} // Execute 完成时关闭
	continueSession bool          // 下次 Execute 使用 --continue 恢复会话
}

// NewAtomCodeTool 创建一个新的 AtomCode 工具适配器。
func NewAtomCodeTool(path string) *AtomCodeTool {
	return &AtomCodeTool{path: path}
}

// Name 返回 "atomcode"。
func (t *AtomCodeTool) Name() string { return "atomcode" }

// IsInstalled 检查 atomcode 是否可用。
func (t *AtomCodeTool) IsInstalled() bool {
	_, err := exec.LookPath(t.path)
	return err == nil
}

// SetContinueSession 设置下次 Execute 调用时使用 --continue 恢复会话。
func (t *AtomCodeTool) SetContinueSession(continueSession bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.continueSession = continueSession
}

// Execute 运行 AtomCode。
// 使用 atomcode -p "prompt" 进行无头模式执行。
// AtomCode 无头模式下自动批准 bash 调用，其他工具需要批准的被拒绝。
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
	t.mu.Unlock() // 在 Start() 后释放锁，使 Stop() 能访问 t.cmd

	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(func() { t.Stop() })
		case <-t.done:
			// Execute 正常完成，无需 Stop
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

// Stop 终止 AtomCode 当前执行。
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

// MiMoCodeTool 实现了 Tool 接口，适配 MiMoCode 编码工具。
// MiMoCode 是基于 OpenCode fork 的带记忆 AI 编码代理，CLI 接口与 OpenCode 兼容。
// CLI: mimocode --prompt "prompt" (无头模式)
type MiMoCodeTool struct {
	path string
	cmd  *exec.Cmd
	mu   sync.Mutex
	done chan struct{} // Execute 完成时关闭
}

// NewMiMoCodeTool 创建一个新的 MiMoCode 工具适配器。
func NewMiMoCodeTool(path string) *MiMoCodeTool {
	return &MiMoCodeTool{path: path}
}

// Name 返回 "mimocode"。
func (t *MiMoCodeTool) Name() string { return "mimocode" }

// IsInstalled 检查 mimocode 是否可用。
func (t *MiMoCodeTool) IsInstalled() bool {
	_, err := exec.LookPath(t.path)
	return err == nil
}

// Execute 运行 MiMoCode。
// MiMoCode 基于 OpenCode fork，使用 --prompt 参数进行无头模式执行。
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
	t.mu.Unlock() // 在 Start() 后释放锁，使 Stop() 能访问 t.cmd

	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(func() { t.Stop() })
		case <-t.done:
			// Execute 正常完成，无需 Stop
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

// Stop 终止 MiMoCode 当前执行。
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

// extractStreamJSONText 解析 Claude stream-json 输出的单行数据，返回可显示的文本内容。
// 每行是一个 JSON 对象，包含 "type" 字段标识事件类型。
func extractStreamJSONText(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return ""
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return line // 不是有效的 JSON，原样返回
	}

	// 提取类型
	var eventType string
	if t, ok := obj["type"]; ok {
		_ = json.Unmarshal(t, &eventType)
	}

	switch eventType {
	case "assistant":
		// Assistant 消息：{"type":"assistant","message":{"content":[{"type":"text","text":"..."}]}}
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
		// 新的内容块开始
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
		// 增量文本 delta
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
		// 最终结果：{"type":"result","result":"...","usage":{...}}
		var resultStr string
		if r, ok := obj["result"]; ok {
			_ = json.Unmarshal(r, &resultStr)
		}
		return resultStr

	case "system":
		// 系统消息（init 等）——跳过显示文本
		return ""

	default:
		// 未知事件类型——跳过
		return ""
	}
}

// extractSessionID 从系统初始化事件中提取 Claude Code 会话 ID。
// 如果行不是系统初始化事件或没有 session_id，则返回空字符串。
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

// sensitivePatterns 匹配日志中的常见密钥格式，用于脱敏处理。
var sensitivePatterns = regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,}|ghp_[a-zA-Z0-9]{20,}|tm_[a-zA-Z0-9]{20,}|Bearer\s+\S+|api_key[=:]\s*\S+)`)

// sanitizeLog 对日志行进行脱敏处理。
func sanitizeLog(line string) string {
	return sensitivePatterns.ReplaceAllString(line, "***REDACTED***")
}

// --- Encoding Normalization ---

// decodeLine 将工具 stdout/stderr 的一行文本规范化为 UTF-8。
//
// 背景：中文 Windows 控制台默认代码页为 GBK(936)。AtomCode/OpenClaw/OpenCode 等
// 原生程序直接以 GBK 字节输出到管道，Go 的 bufio.Scanner.Text() 仅做字节→string
// 的拷贝而不做编码转换，非法 UTF-8 字节原样进入 string。这些字节随后通过
// PostNodeComment / <needs_input> 写入数据库的 UTF-8 文本列，前端按 UTF-8 渲染即
// 出现乱码（部分字节还可能在某个中间环节被强制 UTF-8 解码而变成 U+FFFD，造成不可
// 逆损坏）。Claude Code 走 stream-json，输出本身就是 UTF-8，不受影响。
//
// 策略：若该行已是合法 UTF-8，原样返回（零开销快路径）；否则按 GBK 解码为 UTF-8。
// 解码仍失败则退回到 utf8.RuneError 的 ToValidUTF8 清洗，避免遗留非法字节。
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
	// GBK 解码失败或仍含非法字节：用替换字符清洗，保证下游接收的是合法 UTF-8。
	return strings.ToValidUTF8(line, "�")
}

// scanLines 从 reader 逐行读取，每行经 decodeLine 规范化为 UTF-8 后通过 fn 回调。
// 抽取自各工具适配器中重复的 bufio.Scanner 读取逻辑，统一处理编码兜底。
func scanLines(reader io.Reader, fn func(line string)) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB 缓冲区
	for scanner.Scan() {
		fn(decodeLine(scanner.Text()))
	}
}

// --- Token Usage Extraction ---

// extractTokenUsage 从输出中提取 Token 用量信息。
func extractTokenUsage(output string) (int, int) {
	// 尝试解析 JSON 输出以获取 token 用量
	var result struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}

	// 尝试作为单个 JSON 对象解析
	if err := json.Unmarshal([]byte(output), &result); err == nil {
		return result.Usage.InputTokens, result.Usage.OutputTokens
	}

	// 尝试查找包含用量信息的 JSON 行
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

// GetTool 根据提供者名称返回对应的编码工具适配器。
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
		return NewClaudeTool(path) // 默认使用 Claude
	}
}
