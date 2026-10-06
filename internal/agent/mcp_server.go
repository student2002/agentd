// mcp_server.go implements the local MCP memory server.
//
// `teammate-agentd mcp` runs this server over stdio for one coding-tool
// execution. The executor writes the server into the workDir MCP config with
// the execution context (instance name, connection, workspace) and the
// connection credentials in its environment, so every call is attributed
// automatically — the AI passes no identity arguments. Memory access is
// pull-only: the AI decides when to query or write; nothing is injected into
// prompts. Memory content never reaches the server; the workspace-memory
// proxy calls the existing /api/memories endpoints with the injected
// connection token.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// MCPServerConfig configures one stdio MCP server run.
type MCPServerConfig struct {
	InstanceName string
	PersonaKey   string
	Connection   string
	WorkspaceID  string
	AgentID      string
	ServerURL    string
	Token        string
	MemoryRoot   string // "" → DefaultMemoryRoot()

	// In/Out carry the JSON-RPC stream; Log receives diagnostics (never
	// stdout — that is the protocol channel). All three default to the
	// process stdio.
	In  io.Reader
	Out io.Writer
	Log io.Writer
}

// jsonrpcRequest is one incoming JSON-RPC message. A nil ID means a
// notification, which produces no response; the ID is echoed verbatim
// (numbers, strings).
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RunMCPServer serves the memory tools on the configured streams until the
// input closes or the context is cancelled.
func RunMCPServer(cfg MCPServerConfig) error {
	in := cfg.In
	if in == nil {
		in = os.Stdin
	}
	out := cfg.Out
	if out == nil {
		out = os.Stdout
	}
	logw := cfg.Log
	if logw == nil {
		logw = os.Stderr
	}

	store := NewMemoryStore(cfg.MemoryRoot)
	if cfg.PersonaKey == "" {
		return fmt.Errorf("mcp server requires %s in the environment", MCPCtxPersona)
	}

	reader := bufio.NewReaderSize(in, 4*1024*1024)
	writer := bufio.NewWriter(out)
	ctx := context.Background()
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return nil
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			response, handled := handleMCPMessage(ctx, store, cfg, trimmed, logw)
			if handled {
				data, err := json.Marshal(response)
				if err != nil {
					fmt.Fprintf(logw, "[mcp] marshal response: %v\n", err)
					continue
				}
				if _, err := writer.Write(append(data, '\n')); err != nil {
					return fmt.Errorf("write response: %w", err)
				}
				if err := writer.Flush(); err != nil {
					return fmt.Errorf("flush response: %w", err)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read request: %w", err)
		}
	}
}

// handleMCPMessage dispatches one message. handled=false means the message
// was a notification (no response).
func handleMCPMessage(ctx context.Context, store *MemoryStore, cfg MCPServerConfig, line string, logw io.Writer) (map[string]interface{}, bool) {
	var req jsonrpcRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return mcpError(nil, -32700, "parse error"), true
	}
	if req.ID == nil {
		// Notifications (notifications/initialized etc.) stay unanswered.
		return nil, false
	}
	switch req.Method {
	case "initialize":
		return mcpResult(req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "teammate-memory", "version": AgentdVersion},
		}), true
	case "ping":
		return mcpResult(req.ID, map[string]interface{}{}), true
	case "tools/list":
		return mcpResult(req.ID, map[string]interface{}{"tools": mcpToolDefs()}), true
	case "tools/call":
		var params struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return mcpError(req.ID, -32602, "invalid tools/call params"), true
		}
		text, isErr := callMemoryTool(ctx, store, cfg, params.Name, params.Arguments)
		result := map[string]interface{}{
			"content": []map[string]interface{}{{"type": "text", "text": text}},
		}
		if isErr {
			result["isError"] = true
		}
		return mcpResult(req.ID, result), true
	default:
		return mcpError(req.ID, -32601, fmt.Sprintf("unknown method %q", req.Method)), true
	}
}

func mcpResult(id json.RawMessage, result map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpError(id json.RawMessage, code int, message string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]interface{}{"code": code, "message": message},
	}
}

func mcpToolDefs() []map[string]interface{} {
	stringProp := func(description string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": description}
	}
	return []map[string]interface{}{
		{
			"name":        "search_instance_memory",
			"description": "Search this instance's persistent memory (shared across all workspaces this instance works in) by keyword. Use it to recall earlier decisions, conventions and project knowledge.",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"keyword": stringProp("keyword to look for")},
				"required":   []string{"keyword"},
			},
		},
		{
			"name":        "read_instance_memory",
			"description": "Read this instance's full persistent memory. Prefer search_instance_memory unless a broad review is needed.",
			"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			"name": "write_instance_memory",
			"description": "Append one durable memory entry for this instance (decisions, conventions, project knowledge worth keeping for future tasks). Before writing, call search_instance_memory (or read_instance_memory) and check whether the fact is already recorded — duplicate entries accumulate verbatim, so write only facts that are new or changed. Keep entries short and factual.",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"content": stringProp("the memory to store")},
				"required":   []string{"content"},
			},
		},
		{
			"name":        "search_workspace_memory",
			"description": "Search the current workspace's shared team memories on the Teammate server. Use it for team-wide conventions this workspace maintains.",
			"inputSchema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"keyword": stringProp("keyword to look for")},
				"required":   []string{"keyword"},
			},
		},
	}
}

// callMemoryTool executes one tool call and returns the text content plus an
// error flag.
func callMemoryTool(ctx context.Context, store *MemoryStore, cfg MCPServerConfig, name string, args map[string]interface{}) (string, bool) {
	argString := func(key string) string {
		value, _ := args[key].(string)
		return strings.TrimSpace(value)
	}
	switch name {
	case "search_instance_memory":
		keyword := argString("keyword")
		if keyword == "" {
			return "keyword is required", true
		}
		hits, err := store.Search(cfg.PersonaKey, keyword, store.Limits.SearchLimit)
		if err != nil {
			return fmt.Sprintf("search failed: %v", err), true
		}
		if len(hits) == 0 {
			return "no matching memory entries", false
		}
		return fmt.Sprintf("%d matching entr%s (newest first):\n\n%s", len(hits), pluralY(len(hits)), strings.Join(hits, "\n\n---\n\n")), false

	case "read_instance_memory":
		content, err := store.Read(cfg.PersonaKey)
		if err != nil {
			return fmt.Sprintf("read failed: %v", err), true
		}
		if strings.TrimSpace(content) == "" {
			return "memory is empty", false
		}
		return content, false

	case "write_instance_memory":
		content := argString("content")
		if content == "" {
			return "content is required", true
		}
		if err := store.Append(cfg.PersonaKey, content, cfg.Connection, cfg.WorkspaceID); err != nil {
			return fmt.Sprintf("write failed: %v", err), true
		}
		return "memory stored", false

	case "search_workspace_memory":
		keyword := argString("keyword")
		if keyword == "" {
			return "keyword is required", true
		}
		if cfg.ServerURL == "" || cfg.Token == "" {
			return "workspace memory unavailable: no connection credentials in this execution context", true
		}
		memories, err := NewClient(cfg.ServerURL, cfg.Token).SearchWorkspaceMemories(ctx, keyword, cfg.WorkspaceID)
		if err != nil {
			return fmt.Sprintf("workspace memory search failed: %v", err), true
		}
		if len(memories) == 0 {
			return "no matching workspace memories", false
		}
		var sb strings.Builder
		for i, m := range memories {
			if i > 0 {
				sb.WriteString("\n---\n")
			}
			title := m.Title
			if title == "" {
				title = m.ID
			}
			sb.WriteString(fmt.Sprintf("### %s\n%s", title, m.Content))
		}
		return sb.String(), false

	default:
		return fmt.Sprintf("unknown tool %q", name), true
	}
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// SharedMemory is one workspace-memory search result, the shape the server's
// /api/memories/search endpoint returns.
type SharedMemory struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// SearchWorkspaceMemories queries the workspace's shared team memories
// through the existing server search endpoint.
func (c *Client) SearchWorkspaceMemories(ctx context.Context, keyword, workspaceID string) ([]SharedMemory, error) {
	var memories []SharedMemory
	path := fmt.Sprintf("/api/memories/search?q=%s&workspace_id=%s", url.QueryEscape(keyword), url.QueryEscape(workspaceID))
	if err := c.doJSON(ctx, "GET", path, nil, &memories); err != nil {
		return nil, fmt.Errorf("search workspace memories: %w", err)
	}
	return memories, nil
}
