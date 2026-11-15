// mcp_invocation_tools.go implements the caller-side invocation tools of the
// local stdio MCP server: one agent's coding session can list whom it may
// invoke, submit chat / code_review invocations, wait for the result inside
// the current tool turn, read it later, and cancel. Every call carries the
// executing agent's identity from the MCP environment; the server enforces
// the agent:invoke permission on the target.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Invocation await tuning. The await poll blocks the single-threaded stdio
// loop, which is inherent to a synchronous tool turn.
const (
	mcpAwaitDefaultSeconds = 30
	mcpAwaitMaxSeconds     = 120
	mcpAwaitPollInterval   = time.Second
)

// invocationCaller bundles the credentials the caller tools need from the
// MCP execution context.
func invocationCaller(cfg MCPServerConfig) (*Client, error) {
	if cfg.ServerURL == "" || cfg.Token == "" || cfg.AgentID == "" {
		return nil, fmt.Errorf("invocation tools unavailable: no connection credentials in this execution context")
	}
	return NewClient(cfg.ServerURL, cfg.Token), nil
}

// mcpInvocationToolDefs returns the caller-side invocation tool schemas.
func mcpInvocationToolDefs() []map[string]interface{} {
	stringProp := func(description string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": description}
	}
	numberProp := func(description string) map[string]interface{} {
		return map[string]interface{}{"type": "number", "description": description}
	}
	return []map[string]interface{}{
		{
			"name":        "list_invocable_agents",
			"description": "List the agents this agent is allowed to invoke cross-machine (workspace peers with an agent:invoke grant on them). Invoke them with invoke_agent or invoke_code_review.",
			"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			"name":        "invoke_agent",
			"description": "Ask another machine's agent to do something (chat kind): the target agent executes the message with its own tools and returns its final answer. Returns an invocation_id immediately; use await_invocation to wait for the answer within this turn (recommended) and invocation_result to fetch it later.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"target_agent_id":  stringProp("target agent id (from list_invocable_agents)"),
					"message":          stringProp("what the target agent should do; include all context it needs"),
					"deadline_seconds": numberProp("optional queue lifetime in seconds (default 1800)"),
				},
				"required": []string{"target_agent_id", "message"},
			},
		},
		{
			"name":        "invoke_code_review",
			"description": "Ask another machine's agent to review code (code_review kind). The code travels as a Git checkpoint: push your commits first, then pass the project id and the base/head coordinates — the target agent fetches the ref with its own credentials, verifies the snapshot and reviews it. Use conversation_id from a previous round to continue the same review conversation.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"target_agent_id": stringProp("target agent id (from list_invocable_agents)"),
					"project_id":      stringProp("project id the review belongs to"),
					"base_sha":        stringProp("base commit SHA (review base)"),
					"head_ref":        stringProp("branch or ref you pushed the head commit to"),
					"head_sha":        stringProp("head commit SHA to review (must exist on the remote)"),
					"message":         stringProp("review focus and context for the reviewer"),
					"conversation_id": stringProp("optional: continue a previous review conversation (from its earlier result)"),
				},
				"required": []string{"target_agent_id", "project_id", "base_sha", "head_ref", "head_sha", "message"},
			},
		},
		{
			"name":        "await_invocation",
			"description": "Wait for an invocation to finish within this tool turn and return its final answer. Default timeout 30s, maximum 120s; on timeout the current status and latest progress come back and you can call await_invocation or invocation_result again.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"invocation_id":   stringProp("invocation id from invoke_agent / invoke_code_review"),
					"timeout_seconds": numberProp("optional wait cap, 30 by default, 120 at most"),
				},
				"required": []string{"invocation_id"},
			},
		},
		{
			"name":        "invocation_result",
			"description": "Read the current state of an invocation without waiting: status, final answer (when terminal), latest progress and error details.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"invocation_id": stringProp("invocation id from invoke_agent / invoke_code_review"),
				},
				"required": []string{"invocation_id"},
			},
		},
		{
			"name":        "cancel_invocation",
			"description": "Cancel one of your invocations: a queued or claimed one dies immediately; a running one is asked to stop and its worker confirms (status becomes cancelled).",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"invocation_id": stringProp("invocation id to cancel"),
				},
				"required": []string{"invocation_id"},
			},
		},
	}
}

// callInvocationTool executes one caller-side invocation tool; handled=false
// means the name is not an invocation tool.
func callInvocationTool(ctx context.Context, cfg MCPServerConfig, name string, args map[string]interface{}) (string, bool, bool) {
	handled := func() bool {
		switch name {
		case "list_invocable_agents", "invoke_agent", "invoke_code_review",
			"await_invocation", "invocation_result", "cancel_invocation":
			return true
		}
		return false
	}()
	if !handled {
		return "", false, false
	}

	client, err := invocationCaller(cfg)
	if err != nil {
		return err.Error(), true, true
	}
	argString := func(key string) string {
		value, _ := args[key].(string)
		return strings.TrimSpace(value)
	}
	argNumber := func(key string) int {
		switch v := args[key].(type) {
		case float64:
			return int(v)
		case string:
			var out int
			fmt.Sscanf(strings.TrimSpace(v), "%d", &out)
			return out
		}
		return 0
	}

	switch name {
	case "list_invocable_agents":
		agents, err := client.ListInvocableAgents(ctx, cfg.AgentID)
		if err != nil {
			return fmt.Sprintf("list invocable agents failed: %v", err), true, true
		}
		if len(agents) == 0 {
			return "no agents are granted for invocation; ask a workspace admin for an agent:invoke grant on a target", false, true
		}
		var sb strings.Builder
		sb.WriteString("agents you can invoke:\n")
		for _, a := range agents {
			sb.WriteString(fmt.Sprintf("- %s (%s, provider %s)\n", a.ID, a.Name, a.Provider))
		}
		return sb.String(), false, true

	case "invoke_agent", "invoke_code_review":
		req := CreateInvocationRequest{
			TargetAgentID:   argString("target_agent_id"),
			Message:         argString("message"),
			IdempotencyKey:  uuid.NewString(),
			DeadlineSeconds: argNumber("deadline_seconds"),
			ConversationID:  argString("conversation_id"),
		}
		if name == "invoke_code_review" {
			req.Kind = InvocationKindCodeReview
			req.ProjectID = argString("project_id")
			req.BaseSHA = argString("base_sha")
			req.HeadRef = argString("head_ref")
			req.HeadSHA = argString("head_sha")
		} else {
			req.Kind = InvocationKindChat
		}
		if req.TargetAgentID == "" || req.Message == "" {
			return "target_agent_id and message are required", true, true
		}
		if name == "invoke_code_review" && (req.ProjectID == "" || req.BaseSHA == "" || req.HeadRef == "" || req.HeadSHA == "") {
			return "project_id, base_sha, head_ref and head_sha are required for a code review", true, true
		}
		inv, err := client.CreateInvocation(ctx, cfg.AgentID, req)
		if err != nil {
			return fmt.Sprintf("invoke failed: %v", err), true, true
		}
		kindHint := "use await_invocation to wait for the answer"
		if name == "invoke_code_review" {
			kindHint = "the reviewer fetches the snapshot and posts findings; use await_invocation or invocation_result"
		}
		return fmt.Sprintf("invocation %s queued for %s (%s, conversation %s); %s",
			inv.ID, req.TargetAgentID, inv.Status, inv.ConversationID, kindHint), false, true

	case "await_invocation":
		invocationID := argString("invocation_id")
		if invocationID == "" {
			return "invocation_id is required", true, true
		}
		timeout := argNumber("timeout_seconds")
		if timeout <= 0 {
			timeout = mcpAwaitDefaultSeconds
		}
		if timeout > mcpAwaitMaxSeconds {
			timeout = mcpAwaitMaxSeconds
		}
		deadline := time.Now().Add(time.Duration(timeout) * time.Second)
		for {
			inv, err := client.GetInvocation(ctx, cfg.AgentID, invocationID)
			if err != nil {
				return fmt.Sprintf("await failed: %v", err), true, true
			}
			if isInvocationTerminal(inv.Status) {
				return formatInvocationOutcome(inv), false, true
			}
			if !time.Now().Before(deadline) {
				return fmt.Sprintf("not finished yet (status %s, progress: %s %s); call await_invocation again or check invocation_result later",
					inv.Status, inv.ProgressStage, inv.ProgressMessage), false, true
			}
			time.Sleep(mcpAwaitPollInterval)
		}

	case "invocation_result":
		invocationID := argString("invocation_id")
		if invocationID == "" {
			return "invocation_id is required", true, true
		}
		inv, err := client.GetInvocation(ctx, cfg.AgentID, invocationID)
		if err != nil {
			return fmt.Sprintf("lookup failed: %v", err), true, true
		}
		return formatInvocationOutcome(inv), false, true

	case "cancel_invocation":
		invocationID := argString("invocation_id")
		if invocationID == "" {
			return "invocation_id is required", true, true
		}
		inv, err := client.CancelInvocation(ctx, cfg.AgentID, invocationID)
		if err != nil {
			return fmt.Sprintf("cancel failed: %v", err), true, true
		}
		return fmt.Sprintf("invocation %s is now %s", inv.ID, inv.Status), false, true
	}

	return fmt.Sprintf("unknown tool %q", name), true, false
}

// isInvocationTerminal reports whether the invocation reached a final state.
func isInvocationTerminal(status string) bool {
	switch status {
	case InvocationStatusSucceeded, InvocationStatusFailed,
		InvocationStatusCancelled, InvocationStatusExpired:
		return true
	}
	return false
}

// formatInvocationOutcome renders the invocation state for the model: the
// final answer on success, the error details on failure, progress otherwise.
func formatInvocationOutcome(inv AgentInvocation) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("invocation %s: status %s", inv.ID, inv.Status))
	if inv.ProgressStage != "" || inv.ProgressMessage != "" {
		sb.WriteString(fmt.Sprintf(" (progress: %s %s)", inv.ProgressStage, inv.ProgressMessage))
	}
	sb.WriteString("\n")
	if inv.Status == InvocationStatusSucceeded && inv.ResultText != nil {
		sb.WriteString("\nFinal answer:\n" + *inv.ResultText + "\n")
	}
	if inv.Status == InvocationStatusFailed && inv.ErrorMessage != "" {
		sb.WriteString(fmt.Sprintf("\nFailed: %s (%s)\n", inv.ErrorCode, inv.ErrorMessage))
	}
	if inv.MetadataFlag() {
		sb.WriteString("\n(the result metadata is truncated)\n")
	}
	return sb.String()
}

// MetadataFlag reports whether the result metadata carries the truncated flag.
func (i AgentInvocation) MetadataFlag() bool {
	if len(i.ResultMetadata) == 0 {
		return false
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(i.ResultMetadata, &meta); err != nil {
		return false
	}
	truncated, _ := meta["truncated"].(bool)
	return truncated
}
