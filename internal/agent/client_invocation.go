// client_invocation.go implements the agentd client surface for cross-machine
// agent invocations. All endpoints carry the agent identity header; the server
// verifies the reporting agent is the invocation's target before any state
// change, and every receipt after claim must present the execution ticket.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// AgentInvocation mirrors the server's invocation record (snake_case JSON).
type AgentInvocation struct {
	ID                   string          `json:"id"`
	WorkspaceID          string          `json:"workspace_id"`
	CallerAgentID        string          `json:"caller_agent_id"`
	TargetAgentID        string          `json:"target_agent_id"`
	Kind                 string          `json:"kind"`
	ProjectID            *string         `json:"project_id,omitempty"`
	BaseSHA              *string         `json:"base_sha,omitempty"`
	HeadRef              *string         `json:"head_ref,omitempty"`
	HeadSHA              *string         `json:"head_sha,omitempty"`
	Message              string          `json:"message"`
	IdempotencyKey       string          `json:"idempotency_key"`
	ConversationID       string          `json:"conversation_id"`
	PreviousInvocationID *string         `json:"previous_invocation_id,omitempty"`
	Status               string          `json:"status"`
	ExecutionID          *string         `json:"execution_id,omitempty"`
	ClaimedByDaemonID    *string         `json:"claimed_by_daemon_id,omitempty"`
	LeaseUntil           *time.Time      `json:"lease_until,omitempty"`
	DeadlineAt           time.Time       `json:"deadline_at"`
	ResultText           *string         `json:"result_text,omitempty"`
	ResultMetadata       json.RawMessage `json:"result_metadata,omitempty"`
	ProgressStage        string          `json:"progress_stage,omitempty"`
	ProgressMessage      string          `json:"progress_message,omitempty"`
	LastProgressAt       *time.Time      `json:"last_progress_at,omitempty"`
	ErrorCode            string          `json:"error_code,omitempty"`
	ErrorMessage         string          `json:"error_message,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
	StartedAt            *time.Time      `json:"started_at,omitempty"`
	CompletedAt          *time.Time      `json:"completed_at,omitempty"`
}

// Invocation statuses mirrored from the server (kept as literals here to avoid
// a dependency on the server module).
const (
	InvocationStatusQueued          = "queued"
	InvocationStatusClaimed         = "claimed"
	InvocationStatusRunning         = "running"
	InvocationStatusCancelRequested = "cancel_requested"
	InvocationStatusSucceeded       = "succeeded"
	InvocationStatusFailed          = "failed"
	InvocationStatusCancelled       = "cancelled"
	InvocationStatusExpired         = "expired"
)

// Invocation kinds mirrored from the server.
const (
	InvocationKindChat       = "chat"
	InvocationKindCodeReview = "code_review"
)

func (c *Client) listInvocations(ctx context.Context, path, agentID string) ([]AgentInvocation, error) {
	var result struct {
		Invocations []AgentInvocation `json:"invocations"`
	}
	if err := c.doJSON(ctx, "GET", path, nil, &result, withAgentHeader(agentID)); err != nil {
		return nil, err
	}
	return result.Invocations, nil
}

// ListClaimableInvocations returns the queued invocations targeted at the
// agent; the SQL-backed discovery path when SSE wake-ups are lost.
func (c *Client) ListClaimableInvocations(ctx context.Context, agentID string) ([]AgentInvocation, error) {
	return c.listInvocations(ctx, "/api/agent-invocations/claimable", agentID)
}

// ListInFlightInvocations returns the invocations this daemon claimed for the
// agent that are not yet terminal; the source for startup reconciliation.
func (c *Client) ListInFlightInvocations(ctx context.Context, agentID string) ([]AgentInvocation, error) {
	return c.listInvocations(ctx, "/api/agent-invocations/in-flight", agentID)
}

// GetInvocation fetches one invocation by ID (e.g. to read a previous round's
// result text for a degraded continuation).
func (c *Client) GetInvocation(ctx context.Context, agentID, invocationID string) (AgentInvocation, error) {
	var inv AgentInvocation
	if err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/agent-invocations/%s", invocationID), nil, &inv, withAgentHeader(agentID)); err != nil {
		return AgentInvocation{}, err
	}
	return inv, nil
}

// ClaimInvocation transitions a queued invocation to claimed and returns it
// with the execution ticket. 409 (another claimant or past deadline) surfaces
// as *APIError via IsConflict.
func (c *Client) ClaimInvocation(ctx context.Context, agentID, invocationID string) (AgentInvocation, error) {
	var inv AgentInvocation
	if err := c.doJSON(ctx, "POST", fmt.Sprintf("/api/agent-invocations/%s/claim", invocationID), nil, &inv, withAgentHeader(agentID)); err != nil {
		return AgentInvocation{}, err
	}
	return inv, nil
}

func (c *Client) invocationTicket(ctx context.Context, method, path, agentID string, body map[string]interface{}) (AgentInvocation, error) {
	var inv AgentInvocation
	if err := c.doJSON(ctx, method, path, body, &inv, withAgentHeader(agentID)); err != nil {
		return AgentInvocation{}, err
	}
	return inv, nil
}

// StartInvocation transitions claimed → running under the ticket; the server
// must confirm it before the tool process is spawned.
func (c *Client) StartInvocation(ctx context.Context, agentID, invocationID, executionID string) (AgentInvocation, error) {
	return c.invocationTicket(ctx, "POST", fmt.Sprintf("/api/agent-invocations/%s/start", invocationID), agentID,
		map[string]interface{}{"execution_id": executionID})
}

// RenewInvocationLease extends the claim lease while the invocation executes.
func (c *Client) RenewInvocationLease(ctx context.Context, agentID, invocationID, executionID string) (AgentInvocation, error) {
	return c.invocationTicket(ctx, "POST", fmt.Sprintf("/api/agent-invocations/%s/lease-renew", invocationID), agentID,
		map[string]interface{}{"execution_id": executionID})
}

// UpdateInvocationProgress reports a bounded, already-sanitized progress
// stage/message; the server rate-limits its storage, not the reporting.
func (c *Client) UpdateInvocationProgress(ctx context.Context, agentID, invocationID, executionID, stage, message string) (AgentInvocation, error) {
	return c.invocationTicket(ctx, "POST", fmt.Sprintf("/api/agent-invocations/%s/progress", invocationID), agentID,
		map[string]interface{}{"execution_id": executionID, "stage": stage, "message": message})
}

// CompleteInvocation records the final answer text under the ticket.
func (c *Client) CompleteInvocation(ctx context.Context, agentID, invocationID, executionID, resultText string, metadata map[string]interface{}) (AgentInvocation, error) {
	body := map[string]interface{}{"execution_id": executionID, "result_text": resultText}
	if len(metadata) > 0 {
		body["result_metadata"] = metadata
	}
	return c.invocationTicket(ctx, "POST", fmt.Sprintf("/api/agent-invocations/%s/complete", invocationID), agentID, body)
}

// FailInvocation records a terminal failure with a stable error code and a
// sanitized message under the ticket.
func (c *Client) FailInvocation(ctx context.Context, agentID, invocationID, executionID, errorCode, errorMessage string) (AgentInvocation, error) {
	return c.invocationTicket(ctx, "POST", fmt.Sprintf("/api/agent-invocations/%s/fail", invocationID), agentID,
		map[string]interface{}{"execution_id": executionID, "error_code": errorCode, "error_message": errorMessage})
}

// AckCancelInvocation confirms the worker stopped the process tree of a
// cancel_requested invocation; 409 means a competing terminal state won.
func (c *Client) AckCancelInvocation(ctx context.Context, agentID, invocationID, executionID string) (AgentInvocation, error) {
	return c.invocationTicket(ctx, "POST", fmt.Sprintf("/api/agent-invocations/%s/ack-cancel", invocationID), agentID,
		map[string]interface{}{"execution_id": executionID})
}

// ---------------------------------------------------------------------------
// Caller face: the invoking agent's tools use these endpoints (the same
// agent identity header; the server enforces agent:invoke on the target).
// ---------------------------------------------------------------------------

// InvocableAgent is one entry of the invocable-agents listing.
type InvocableAgent struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// CreateInvocationRequest is the body of POST /api/agent-invocations.
type CreateInvocationRequest struct {
	TargetAgentID   string `json:"target_agent_id"`
	Kind            string `json:"kind"`
	Message         string `json:"message"`
	IdempotencyKey  string `json:"idempotency_key"`
	ProjectID       string `json:"project_id,omitempty"`
	BaseSHA         string `json:"base_sha,omitempty"`
	HeadRef         string `json:"head_ref,omitempty"`
	HeadSHA         string `json:"head_sha,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
	DeadlineSeconds int    `json:"deadline_seconds,omitempty"`
}

// ListInvocableAgents returns the workspace agents the caller may invoke.
func (c *Client) ListInvocableAgents(ctx context.Context, agentID string) ([]InvocableAgent, error) {
	var result struct {
		Agents []InvocableAgent `json:"agents"`
	}
	// The caller identity header carries the workspace; no query param needed.
	if err := c.doJSON(ctx, "GET", "/api/agent-invocations/invocable-agents", nil, &result, withAgentHeader(agentID)); err != nil {
		return nil, err
	}
	return result.Agents, nil
}

// CreateInvocation submits a chat or code_review invocation to the target
// agent. The server answers 202 with the queued record; an identical
// idempotency-key retry replays the original record.
func (c *Client) CreateInvocation(ctx context.Context, callerAgentID string, req CreateInvocationRequest) (AgentInvocation, error) {
	var inv AgentInvocation
	if err := c.doJSON(ctx, "POST", "/api/agent-invocations", req, &inv, withAgentHeader(callerAgentID)); err != nil {
		return AgentInvocation{}, err
	}
	return inv, nil
}

// CancelInvocation applies a caller-side cancel: queued/claimed die at once,
// a running invocation is asked to stop and the worker confirms.
func (c *Client) CancelInvocation(ctx context.Context, agentID, invocationID string) (AgentInvocation, error) {
	var inv AgentInvocation
	if err := c.doJSON(ctx, "POST", fmt.Sprintf("/api/agent-invocations/%s/cancel", invocationID), nil, &inv, withAgentHeader(agentID)); err != nil {
		return AgentInvocation{}, err
	}
	return inv, nil
}
