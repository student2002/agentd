// invocation_context.go builds the prompt context of one invocation. Unlike
// BuildExecutionContext it never injects task/node/comment/skill/MCP context
// and never touches credential material: the invoked session sees the target
// agent's own instructions, the caller's message, and — for code_review — the
// verified snapshot coordinates only.
package agent

import (
	"context"
	"fmt"
	"strings"
)

// fetchAgentInstructionsWithError loads the target agent's instructions;
// an empty identity fails the invocation instead of executing silently.
// AgentInstructions is shared with the task execution context (context.go).
func fetchAgentInstructionsWithError(client *Client, workspaceID, agentID string) (AgentInstructions, error) {
	var agent AgentInstructions
	if err := client.GetAgent(context.Background(), workspaceID, agentID, &agent); err != nil {
		return AgentInstructions{}, fmt.Errorf("load agent instructions: %w", err)
	}
	if strings.TrimSpace(agent.Instructions) == "" {
		return AgentInstructions{}, fmt.Errorf("agent instructions are empty; refusing to execute an invocation without an identity")
	}
	return agent, nil
}

// buildPrompt assembles the full prompt for one invocation.
func (m *InvocationManager) buildPrompt(ctx context.Context, inv AgentInvocation, ws *invocationWorkspace) (string, error) {
	identity, err := fetchAgentInstructionsWithError(m.client, m.workspaceID, m.agentID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("You are handling a cross-machine invocation. Another machine's agent asked you to complete exactly this request. Return your final answer as plain text; do not start work that was not requested, do not create tasks, and do not call other agents.\n\n")

	if identity.Instructions != "" {
		b.WriteString("## Your Instructions\n\n")
		b.WriteString(strings.TrimSpace(identity.Instructions))
		b.WriteString("\n\n")
	}

	b.WriteString("## Request\n\n")
	b.WriteString(inv.Message)
	b.WriteString("\n")

	if inv.Kind == InvocationKindCodeReview {
		section, err := m.buildReviewContext(ctx, inv, ws)
		if err != nil {
			return "", err
		}
		b.WriteString(section)
	}
	return b.String(), nil
}

// buildReviewContext describes the verified snapshot coordinates. The review
// workspace path is the only filesystem location the session needs; nothing
// about the caller's machine, credentials, or task state is included.
func (m *InvocationManager) buildReviewContext(ctx context.Context, inv AgentInvocation, ws *invocationWorkspace) (string, error) {
	project, err := m.client.GetProject(ctx, m.agentID, m.workspaceID, *inv.ProjectID)
	if err != nil {
		return "", fmt.Errorf("load project summary: %w", err)
	}

	var b strings.Builder
	b.WriteString("\n## Code Review Request\n\n")
	b.WriteString(fmt.Sprintf("Project: %s\n", project.Name))
	b.WriteString(fmt.Sprintf("Review workspace: %s\n", ws.dir))
	b.WriteString(fmt.Sprintf("Snapshot: review the changes between %s and %s (the workspace is already checked out at %s).\n", *inv.BaseSHA, *inv.HeadSHA, *inv.HeadSHA))
	b.WriteString("You may read the full repository and run tests or builds locally. Do not push, do not modify remote branches, and do not commit.\n")
	b.WriteString("Deliver your review findings as your final answer: issues found, their severity, and concrete suggestions.\n")

	if inv.PreviousInvocationID != nil && *inv.PreviousInvocationID != "" {
		if ws != nil && ws.resumed && ws.meta != nil && ws.meta.SessionID != "" {
			b.WriteString("\nThis is a follow-up round of the same review conversation; your previous session has been resumed, so the earlier context is already available.\n")
		} else if previous, err := m.client.GetInvocation(ctx, m.agentID, *inv.PreviousInvocationID); err == nil && previous.ResultText != nil && strings.TrimSpace(*previous.ResultText) != "" {
			b.WriteString("\n## Previous Review Round\n\n")
			b.WriteString("This conversation continues an earlier round whose session could not be resumed. Its conclusion follows; review the new snapshot on top of it.\n\n")
			b.WriteString(strings.TrimSpace(*previous.ResultText))
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}

// applyInvocationSessionResume wires the conversation's provider session onto
// a fresh tool instance when the local mapping says a real resume is possible.
func applyInvocationSessionResume(provider string, t interface{}, ws *invocationWorkspace) {
	if ws == nil || ws.meta == nil || ws.meta.SessionID == "" || !ws.resumed {
		return
	}
	switch provider {
	case "claude":
		if ct, ok := t.(interface{ SetResumeSession(string) }); ok {
			ct.SetResumeSession(ws.meta.SessionID)
		}
	case "atomcode":
		if at, ok := t.(interface {
			SetResumeSession(string)
			SetContinueSession(bool)
		}); ok {
			at.SetResumeSession(ws.meta.SessionID)
		}
	}
}
