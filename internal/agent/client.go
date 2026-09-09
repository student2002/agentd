// Package agent provides the core functionality of the AI agent daemon.
//
// This package implements the full lifecycle of the Agent Daemon, including:
//   - runtime registration and heartbeat maintenance
//   - SSE event listening and response
//   - node claiming and task execution
//   - Git operations and credential management
//   - context construction and tool invocation
//   - RSA encrypted communication
//
// Client is the HTTP client that communicates with the Server and encapsulates
// all API calls.
// The client supports two authentication methods: API Token (permanent) and
// Session Token (7-day validity).
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Client is the HTTP client that communicates with the Teammate Server.
//
// The client encapsulates all REST API interactions with the Server, including:
//   - runtime registration and heartbeat
//   - node claiming and status reporting
//   - Git credential retrieval
//   - comment and log sending
//   - token exchange and refresh
//
// Usage:
//
//	client := NewClient("http://localhost:8080", "tm_xxx_xxx")
//	runtime, _ := client.RegisterRuntime(ctx, workspaceID, agentID, "claude", "1.0.0", pubKey)
type Client struct {
	// BaseURL is the base URL of the Server.
	BaseURL string

	// APIToken is the API Token (tm_ prefix) used for initial authentication.
	APIToken string

	// SessionToken is the exchanged session Token (st_ prefix), valid for 7 days.
	SessionToken string

	// SessionExpiry is the expiry time of the Session Token.
	SessionExpiry time.Time

	// PrivateKeyPEM is the PEM-encoded RSA private key, used to decrypt Git credentials.
	PrivateKeyPEM string

	// HTTP is the underlying HTTP client instance.
	HTTP *http.Client
}

// NewClient creates a new Server communication client.
//
// Parameters:
//   - baseURL: the base URL of the Server (e.g. "http://localhost:8080")
//   - apiToken: the API Token used for initial authentication
//
// Returns:
//   - *Client: the initialized client instance
func NewClient(baseURL, apiToken string) *Client {
	return &Client{
		BaseURL:  baseURL,
		APIToken: apiToken,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// do executes an HTTP request, automatically setting auth headers and JSON
// serialization.
// If body is not nil, it serializes it to JSON and sets the Content-Type header.
func (c *Client) do(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}

	token := c.authToken()
	req.Header.Set("X-API-Key", token)
	req.Header.Set("Content-Type", "application/json")

	return c.HTTP.Do(req)
}

// authToken returns the best available auth token.
// It prefers the session token (if present and not close to expiry), otherwise
// falls back to the API token.
func (c *Client) authToken() string {
	if c.SessionToken != "" && !c.SessionExpiry.IsZero() && time.Now().Before(c.SessionExpiry.Add(-5*time.Minute)) {
		return c.SessionToken
	}
	return c.APIToken
}

// doJSON executes a JSON API request, automatically handling request body
// serialization and response body deserialization.
// If the response status code is >= 300, it returns an error containing the
// status code and response body.
func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, respBody interface{}) error {
	resp, err := c.do(ctx, method, path, reqBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	if respBody != nil {
		return json.NewDecoder(resp.Body).Decode(respBody)
	}
	return nil
}

// ListAgentSkills retrieves the list of skills bound to the current Agent.
func (c *Client) ListAgentSkills(ctx context.Context, workspaceID, agentID string) ([]SkillContext, error) {
	var skills []SkillContext
	path := fmt.Sprintf("/api/workspaces/%s/agents/%s/skills", workspaceID, agentID)
	if err := c.doJSON(ctx, "GET", path, nil, &skills); err != nil {
		return nil, fmt.Errorf("list agent skills: %w", err)
	}
	return skills, nil
}

// AgentMcpServerContext represents the MCP server configuration that can be
// injected into the Agent during execution.
type AgentMcpServerContext struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	URL        string          `json:"url"`
	Type       string          `json:"type"`
	AuthType   string          `json:"auth_type"`
	EnvVars    json.RawMessage `json:"env_vars"`
	Status     string          `json:"status"`
	Enabled    bool            `json:"enabled"`
	AssignedAt string          `json:"assigned_at"`
}

// ListAgentMcpServers retrieves the list of MCP servers bound to the current Agent (via the daemon-only execution endpoint, which returns decrypted env_vars).
func (c *Client) ListAgentMcpServers(ctx context.Context, workspaceID, agentID string) ([]AgentMcpServerContext, error) {
	var servers []AgentMcpServerContext
	path := fmt.Sprintf("/api/workspaces/%s/agents/%s/execution/mcp-servers", workspaceID, agentID)
	if err := c.doJSON(ctx, "GET", path, nil, &servers); err != nil {
		return nil, fmt.Errorf("list agent mcp servers: %w", err)
	}
	return servers, nil
}

// --- Runtime ---

// RegisterRuntimeRequest represents the runtime registration request body,
// corresponding to the server-side registerRuntimeRequest structure.
type RegisterRuntimeRequest struct {
	AgentID          string `json:"agent_id"`
	DaemonID         string `json:"daemon_id"`
	Provider         string `json:"provider"`
	Version          string `json:"version"`
	Status           string `json:"status"`
	SessionTokenHash string `json:"session_token_hash"`
	PublicKey        string `json:"public_key"`
}

// RegisterRuntimeResponse represents the runtime registration response body,
// containing the newly created runtime ID.
type RegisterRuntimeResponse struct {
	ID string `json:"id"`
}

// RegisterRuntime registers the daemon as a runtime instance of the agent.
// On success it returns the runtime ID, used for subsequent heartbeats and
// event reception.
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - workspaceID: workspace ID
//   - agentID: agent ID
//   - provider: coding tool provider (e.g. "claude")
//   - toolVersion: coding tool version
//   - publicKeyPEM: PEM-encoded RSA public key, used by the server to encrypt Git credentials
//
// Returns:
//   - *RegisterRuntimeResponse: registration response, containing the runtime ID
//   - error: returned on registration failure
func (c *Client) RegisterRuntime(ctx context.Context, workspaceID, agentID, provider, toolVersion, publicKeyPEM string) (*RegisterRuntimeResponse, error) {
	var result RegisterRuntimeResponse
	err := c.doJSON(ctx, "POST", fmt.Sprintf("/api/workspaces/%s/runtimes", workspaceID), RegisterRuntimeRequest{
		AgentID:   agentID,
		Provider:  provider,
		Version:   toolVersion,
		Status:    "online",
		PublicKey: publicKeyPEM,
	}, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Session Token Exchange ---

// ExchangeTokenRequest represents the token exchange request body, used to
// exchange an API token for a session token.
type ExchangeTokenRequest struct {
	APIToken string `json:"api_token"`
}

// ExchangeTokenResponse represents the token exchange response body,
// containing the new session token and expiry time.
type ExchangeTokenResponse struct {
	SessionToken string    `json:"session_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ExchangeToken exchanges an API token for a session token.
// The session token is used for subsequent API calls and is more secure than
// the API token (short-lived).
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - apiToken: the API token to exchange
//
// Returns:
//   - sessionToken: the new session token
//   - expiresAt: the expiry time of the session token
//   - error: returned on exchange failure
func (c *Client) ExchangeToken(ctx context.Context, apiToken string) (sessionToken string, expiresAt time.Time, err error) {
	var result ExchangeTokenResponse
	err = c.doJSON(ctx, "POST", "/api/auth/token-exchange", ExchangeTokenRequest{
		APIToken: apiToken,
	}, &result)
	if err != nil {
		return "", time.Time{}, err
	}
	return result.SessionToken, result.ExpiresAt, nil
}

// RefreshSessionToken attempts to re-exchange a new session token using the
// API token.
// On success it updates the client's SessionToken and SessionExpiry fields.
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//
// Returns:
//   - error: returned on exchange failure
func (c *Client) RefreshSessionToken(ctx context.Context) error {
	token, expiresAt, err := c.ExchangeToken(ctx, c.APIToken)
	if err != nil {
		return fmt.Errorf("refresh session token: %w", err)
	}
	c.SessionToken = token
	c.SessionExpiry = expiresAt
	return nil
}

// StartSessionTokenRefresher starts a background goroutine that automatically
// refreshes the session token before it expires.
// Refresh happens 5 minutes before expiry; on failure it retries once after 30
// seconds.
//
// Parameters:
//   - stopCh: stop signal channel; closing it makes the goroutine exit
func (c *Client) StartSessionTokenRefresher(stopCh <-chan struct{}) {
	go func() {
		for {
			if c.SessionExpiry.IsZero() {
				select {
				case <-time.After(30 * time.Second):
					continue
				case <-stopCh:
					return
				}
			}

			// Refresh 5 minutes before expiry
			refreshAt := c.SessionExpiry.Add(-5 * time.Minute)
			waitDuration := time.Until(refreshAt)
			if waitDuration < 0 {
				waitDuration = 0
			}

			select {
			case <-time.After(waitDuration):
				if err := c.RefreshSessionToken(context.Background()); err != nil {
					// Retry 30 seconds after failure
					select {
					case <-time.After(30 * time.Second):
						_ = c.RefreshSessionToken(context.Background())
					case <-stopCh:
						return
					}
				}
			case <-stopCh:
				return
			}
		}
	}()
}

// Heartbeat sends a heartbeat to the Server to keep the runtime online.
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - workspaceID: workspace ID
//   - runtimeID: runtime ID
//
// Returns:
//   - error: returned on send failure
func (c *Client) Heartbeat(ctx context.Context, workspaceID, runtimeID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/workspaces/%s/runtimes/%s/heartbeat", workspaceID, runtimeID), nil, nil)
}

// --- Node Operations ---

// TaskNode represents task node information obtained from the API, including
// node status, type, and execution information.
type TaskNode struct {
	ID              string          `json:"id"`
	TaskID          int32           `json:"task_id"`
	Name            string          `json:"name"`
	NodeType        string          `json:"node_type"`
	Status          string          `json:"status"`
	AssigneeType    string          `json:"assignee_type"`
	SortOrder       int32           `json:"sort_order"`
	RejectCount     int32           `json:"reject_count"`
	Description     string          `json:"description"`
	Summary         string          `json:"summary"`
	ReadonlyDirs    json.RawMessage `json:"readonly_dirs"`     // read-only directories (JSON array), template node config
	FullControlDirs json.RawMessage `json:"full_control_dirs"` // full-control directories (JSON array), template node config
}

// Comment represents a comment on a task or node, used for execution context
// and node handoff.
type Comment struct {
	ID           string  `json:"id"`
	TaskID       int32   `json:"task_id"`
	NodeID       *string `json:"node_id"`
	SourceNodeID *string `json:"source_node_id"`
	AuthorType   string  `json:"author_type"`
	AuthorID     string  `json:"author_id"`
	Content      string  `json:"content"`
	CommentType  string  `json:"comment_type"`
}

// UnmarshalJSON implements custom JSON deserialization for TaskNode.
// The server returns sql.NullString fields in the form {"String":"...","Valid":true},
// which standard string deserialization cannot handle, so special handling is needed.
func (n *TaskNode) UnmarshalJSON(data []byte) error {
	type Alias TaskNode
	aux := &struct {
		Description nullString `json:"description"`
		Summary     nullString `json:"summary"`
		*Alias
	}{
		Alias: (*Alias)(n),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	n.Description = aux.Description.String()
	n.Summary = aux.Summary.String()
	return nil
}

// boardResponse represents the response format of the board API, containing
// task columns grouped by status.
type boardResponse struct {
	Columns []boardColumn `json:"columns"`
}

// boardColumn represents a board column, containing a column key, label, and
// task list.
type boardColumn struct {
	Key   string            `json:"key"`
	Label string            `json:"label"`
	Tasks []boardColumnTask `json:"tasks"`
}

// boardColumnTask represents a task entry in a board column, containing basic
// task information and the current node status.
type boardColumnTask struct {
	ID                int32       `json:"id"`
	Title             string      `json:"title"`
	Priority          string      `json:"priority"`
	Type              string      `json:"type"`
	CurrentNodeName   string      `json:"current_node_name"`
	CurrentNodeStatus string      `json:"current_node_status"`
	AssigneeID        interface{} `json:"assignee_id"`
}

// ListPendingNodes returns the list of pending nodes in the project that can be
// claimed.
// It first obtains tasks with pending nodes via the board API, then calls the
// node API to get the full node information (including node ID).
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - projectID: project ID
//
// Returns:
//   - []TaskNode: the list of pending nodes that can be claimed
//   - error: returned on query failure
func (c *Client) ListPendingNodes(ctx context.Context, projectID string) ([]TaskNode, error) {
	var result boardResponse
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/projects/%s/board", projectID), nil, &result)
	if err != nil {
		return nil, err
	}

	// Collect task IDs from the pending column (review nodes are now merged into the same column)
	var pendingTaskIDs []int32
	for _, col := range result.Columns {
		if col.Key == "pending" {
			for _, t := range col.Tasks {
				pendingTaskIDs = append(pendingTaskIDs, t.ID)
			}
		}
	}

	if len(pendingTaskIDs) == 0 {
		return nil, nil
	}

	// For each pending task, fetch full node details to obtain the node ID
	var nodes []TaskNode
	for _, taskID := range pendingTaskIDs {
		select {
		case <-ctx.Done():
			return nodes, ctx.Err()
		default:
		}
		taskNodes, err := c.ListTaskNodes(ctx, taskID)
		if err != nil {
			continue
		}
		for _, n := range taskNodes {
			if n.Status == "pending" {
				nodes = append(nodes, n)
			}
		}
	}
	return nodes, nil
}

// ListTaskNodes retrieves all nodes of the specified task.
// Endpoint: GET /api/tasks/{taskId}/nodes
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - taskID: task ID
//
// Returns:
//   - []TaskNode: the task node list
//   - error: returned on query failure
func (c *Client) ListTaskNodes(ctx context.Context, taskID int32) ([]TaskNode, error) {
	var result []TaskNode
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/tasks/%d/nodes", taskID), nil, &result)
	return result, err
}

// ListExecutionContextComments retrieves the comment context that should be
// injected when executing the specified node.
func (c *Client) ListExecutionContextComments(ctx context.Context, taskID int32, nodeID string) ([]Comment, error) {
	var result []Comment
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/tasks/%d/comments?node_id=%s&scope=execution_context", taskID, nodeID), nil, &result)
	return result, err
}

// ListNodeComments retrieves comments in the specified node's comment area.
func (c *Client) ListNodeComments(ctx context.Context, taskID int32, nodeID string) ([]Comment, error) {
	var result []Comment
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/tasks/%d/comments?node_id=%s", taskID, nodeID), nil, &result)
	return result, err
}

// InProgressNode represents a node claimed by the Agent but not yet completed,
// used to resume execution after a restart.
type InProgressNode struct {
	ID              string          `json:"id"`
	TaskID          int32           `json:"task_id"`
	Name            string          `json:"name"`
	SortOrder       int32           `json:"sort_order"`
	NodeType        string          `json:"node_type"`
	Status          string          `json:"status"`
	ReadonlyDirs    json.RawMessage `json:"readonly_dirs"`     // read-only directories (JSON array)
	FullControlDirs json.RawMessage `json:"full_control_dirs"` // full-control directories (JSON array)
	ProjectID       string          `json:"project_id"`
}

// GetInProgressNodes queries the nodes claimed by the current Agent that are
// not yet completed (in_progress).
// Used to resume unfinished execution after the Agent restarts.
// Endpoint: GET /api/workspaces/{workspaceID}/agents/{agentID}/in-progress-nodes
//
// Parameters:
//   - ctx: context
//   - workspaceID: workspace ID
//   - agentID: agent ID
//
// Returns:
//   - []InProgressNode: the in_progress node list
//   - error: returned on query failure
func (c *Client) GetInProgressNodes(ctx context.Context, workspaceID, agentID string) ([]InProgressNode, error) {
	var result []InProgressNode
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/workspaces/%s/agents/%s/in-progress-nodes", workspaceID, agentID), nil, &result)
	return result, err
}

// ClaimNode claims a pending node, assigning it to the specified agent.
// Uses optimistic locking for concurrency control; if the node has already
// been claimed by another agent it returns 409 Conflict.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/claim
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - agentID: the ID of the agent claiming the node
//   - taskID: task ID
//   - nodeID: node ID
//
// Returns:
//   - *TaskNode: the node information after a successful claim
//   - error: returned on claim failure (e.g. 409 Conflict)
func (c *Client) ClaimNode(ctx context.Context, agentID string, taskID int32, nodeID string) (*TaskNode, error) {
	var result TaskNode
	err := c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/claim", taskID, nodeID), map[string]string{
		"agent_id": agentID,
	}, &result)
	return &result, err
}

// ApproveNode approves (completes) the current node.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/approve
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - agentID: the approver agent ID
//   - taskID: task ID
//   - nodeID: node ID
//   - comment: approval comment
//
// Returns:
//   - error: returned on approval failure
func (c *Client) ApproveNode(ctx context.Context, agentID string, taskID int32, nodeID, comment string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/approve", taskID, nodeID), map[string]string{
		"operator_id":   agentID,
		"operator_type": "agent",
		"comment":       comment,
	}, nil)
}

// CompleteNode completes a standard node (agent-only call, does not require
// task:approve permission).
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/complete
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - agentID: the executing agent ID
//   - taskID: task ID
//   - nodeID: node ID
//   - summary: node execution summary
//
// Returns:
//   - error: returned on completion failure
func (c *Client) CompleteNode(ctx context.Context, agentID string, taskID int32, nodeID, summary string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/complete", taskID, nodeID), map[string]string{
		"summary": summary,
	}, nil)
}

// RejectNode rejects the current node, rolling it back to the specified target
// node.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/reject
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - agentID: the rejecter agent ID
//   - taskID: task ID
//   - nodeID: the ID of the rejected node
//   - targetNodeID: the rollback target node ID
//   - comment: rejection comment
//
// Returns:
//   - error: returned on rejection failure
func (c *Client) RejectNode(ctx context.Context, agentID string, taskID int32, nodeID, targetNodeID, comment string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/reject", taskID, nodeID), map[string]interface{}{
		"operator_id":    agentID,
		"operator_type":  "agent",
		"target_node_id": targetNodeID,
		"comment":        comment,
	}, nil)
}

// ManualIntervention marks the node as requiring manual intervention.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/manual
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - agentID: the operator agent ID
//   - taskID: task ID
//   - nodeID: node ID
//   - comment: explanation of the intervention reason
//
// Returns:
//   - error: returned on operation failure
func (c *Client) ManualIntervention(ctx context.Context, agentID string, taskID int32, nodeID, comment string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/manual", taskID, nodeID), map[string]string{
		"operator_id":   agentID,
		"operator_type": "agent",
		"comment":       comment,
	}, nil)
}

// SkipClaim relinquishes the node's continuation right, allowing other agents
// to claim subsequent nodes.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/skip-claim
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - agentID: the ID of the agent relinquishing the continuation right
//   - taskID: task ID
//   - nodeID: node ID
//
// Returns:
//   - error: returned on operation failure
func (c *Client) SkipClaim(ctx context.Context, agentID string, taskID int32, nodeID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/skip-claim", taskID, nodeID), map[string]string{
		"agent_id": agentID,
	}, nil)
}

// --- Token Usage ---

// TokenUsageRequest represents the token usage report request body,
// containing input, output, and total token counts.
type TokenUsageRequest struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ReportTokenUsage reports the token usage of a completed node.
// Endpoint: POST /api/tasks/{taskId}/token-usage
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - taskID: task ID
//   - nodeID: node ID
//   - agentID: the executing agent ID
//   - usage: token usage information
//
// Returns:
//   - error: returned on report failure
func (c *Client) ReportTokenUsage(ctx context.Context, taskID int32, nodeID, agentID string, usage TokenUsageRequest) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/token-usage", taskID), map[string]interface{}{
		"task_node_id":  nodeID,
		"agent_id":      agentID,
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
		"total_tokens":  usage.TotalTokens,
	}, nil)
}

// --- Git Credentials ---

// GitCredentials represents the decrypted Git credentials, containing the
// repository URL, username, and personal access token.
type GitCredentials struct {
	RepoURL  string `json:"repo_url"`
	Username string `json:"username"`
	PAT      string `json:"pat"` // decrypted personal access token (after RSA decryption)
}

// gitCredentialsResponse represents the response format of the Git credentials API.
type gitCredentialsResponse struct {
	Credentials []gitCredentialEntry `json:"credentials"`
}

// gitCredentialEntry represents a single credential entry returned by the
// server, containing the encrypted personal access token.
type gitCredentialEntry struct {
	RepoURL      string `json:"repo_url"`
	Username     string `json:"username"`
	EncryptedPAT string `json:"encrypted_pat"`
}

// GetGitCredentials retrieves and decrypts the project's Git credentials.
// Returns a list of credentials (one per configured repo_url).
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - projectID: project ID
//
// Returns:
//   - []GitCredentials: the decrypted credential list
//   - error: returned on retrieval or decryption failure
func (c *Client) GetGitCredentials(ctx context.Context, projectID string) ([]GitCredentials, error) {
	var result gitCredentialsResponse
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/projects/%s/git-credentials", projectID), nil, &result)
	if err != nil {
		return nil, err
	}

	creds := make([]GitCredentials, 0, len(result.Credentials))
	for _, entry := range result.Credentials {
		pat := entry.EncryptedPAT
		// If a private key is available, decrypt the PAT
		if c.PrivateKeyPEM != "" && entry.EncryptedPAT != "" {
			decrypted, err := DecryptWithPrivateKey(c.PrivateKeyPEM, entry.EncryptedPAT)
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt PAT: %w", err)
			}
			pat = decrypted
		}

		creds = append(creds, GitCredentials{
			RepoURL:  entry.RepoURL,
			Username: entry.Username,
			PAT:      pat,
		})
	}

	return creds, nil
}

// --- Projects ---

// Project represents project information obtained from the API, containing the
// project ID and name.
type Project struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	RepoURL     string `json:"repo_url"`
}

// ListProjects retrieves all projects in the workspace.
// Endpoint: GET /api/workspaces/{workspaceID}/projects
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - workspaceID: workspace ID
//
// Returns:
//   - []Project: the project list
//   - error: returned on query failure
func (c *Client) ListProjects(ctx context.Context, workspaceID string) ([]Project, error) {
	var result []Project
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/workspaces/%s/projects", workspaceID), nil, &result)
	return result, err
}

// GetProject retrieves a single project's information, including project-level
// repository configuration.
func (c *Client) GetProject(ctx context.Context, workspaceID, projectID string) (*Project, error) {
	var result Project
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/workspaces/%s/projects/%s", workspaceID, projectID), nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Task Messages ---

// SendMessage sends a task log message; the content is desensitized before
// upload.
// Endpoint: POST /api/tasks/{taskId}/messages
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - taskID: task ID
//   - nodeID: node ID
//   - content: log content
//
// Returns:
//   - error: returned on send failure
func (c *Client) SendMessage(ctx context.Context, taskID int32, nodeID, content string) error {
	return c.SendMessageWithType(ctx, taskID, nodeID, "stdout", content)
}

// SendMessageWithType sends a task log message of the specified type; the
// content is desensitized before upload.
// Endpoint: POST /api/tasks/{taskId}/messages
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - taskID: task ID
//   - nodeID: node ID
//   - msgType: message type ("stdout", "stderr", "system")
//   - content: log content
//
// Returns:
//   - error: returned on send failure
func (c *Client) SendMessageWithType(ctx context.Context, taskID int32, nodeID, msgType, content string) error {
	desensitized := DesensitizeLog(content)
	log.Printf("[client:SendMessage] task=%d node=%s type=%s content_len=%d", taskID, nodeID, msgType, len(desensitized))
	err := c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/messages", taskID), map[string]string{
		"node_id": nodeID,
		"type":    msgType,
		"content": desensitized,
	}, nil)
	if err != nil {
		log.Printf("[client:SendMessage] ERROR: %v", err)
	} else {
		log.Printf("[client:SendMessage] success")
	}
	return err
}

// --- Interrupt ---

// ReportInterrupt acknowledges that an interrupt request for a task node has
// been processed.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/interrupt-ack
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - taskID: task ID
//   - nodeID: node ID
//
// Returns:
//   - error: returned on acknowledgment failure
func (c *Client) ReportInterrupt(ctx context.Context, taskID int32, nodeID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/interrupt-ack", taskID, nodeID), map[string]string{
		"comment": "interrupt acknowledged by agent",
	}, nil)
}

// --- Task Details ---

// GetTask retrieves task details by task ID.
// Endpoint: GET /api/projects/{projectID}/tasks/{taskID}
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - projectID: project ID
//   - taskID: task ID
//
// Returns:
//   - *Task: task details
//   - error: returned on query failure
func (c *Client) GetTask(ctx context.Context, projectID string, taskID int32) (*Task, error) {
	var result Task
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/projects/%s/tasks/%d", projectID, taskID), nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ReportSummary updates the summary information of a completed node.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/summary
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - taskID: task ID
//   - nodeID: node ID
//   - summary: node execution summary
//
// Returns:
//   - error: returned on update failure
func (c *Client) ReportSummary(ctx context.Context, taskID int32, nodeID, summary string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/summary", taskID, nodeID), map[string]string{
		"summary": summary,
	}, nil)
}

// ReportGitBranch reports the task's Git branch name after the Git workspace
// is initialized successfully.
// Endpoint: PUT /api/tasks/{taskId}/git-branch
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - taskID: task ID
//   - gitBranch: Git branch name
//
// Returns:
//   - error: returned on report failure
func (c *Client) ReportGitBranch(ctx context.Context, taskID int32, gitBranch string) error {
	return c.doJSON(ctx, "PUT", fmt.Sprintf("/api/tasks/%d/git-branch", taskID), map[string]string{
		"git_branch": gitBranch,
	}, nil)
}

// PostComment posts a comment on a task.
// Endpoint: POST /api/tasks/{taskId}/comments
//
// Parameters:
//   - ctx: context, used to control request timeout and cancellation
//   - taskID: task ID
//   - content: comment content
//   - authorType: author type ("agent" or "human")
//   - authorID: author ID
//
// Returns:
//   - error: returned on post failure
func (c *Client) PostComment(ctx context.Context, taskID int32, content, authorType, authorID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/comments", taskID), map[string]string{
		"content": content,
	}, nil)
}

// PostNodeComment posts a comment in the specified node's comment area.
func (c *Client) PostNodeComment(ctx context.Context, taskID int32, nodeID, sourceNodeID, commentType, content string) error {
	body := map[string]string{
		"node_id":      nodeID,
		"content":      content,
		"comment_type": commentType,
	}
	if sourceNodeID != "" {
		body["source_node_id"] = sourceNodeID
	}
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/comments", taskID), body, nil)
}
