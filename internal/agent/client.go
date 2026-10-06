// Package agent provides the core functionality of the AI agent daemon.
//
// This package implements the full lifecycle of the Agent Daemon, including:
//   - daemon registration, heartbeat maintenance and tool detection reports
//   - SSE event listening and per-instance event routing
//   - node claiming and task execution
//   - Git operations and credential management
//   - context construction and tool invocation
//   - RSA encrypted communication
//
// Client is the HTTP client that communicates with the Server and encapsulates
// all API calls. Authentication is daemon-level: a permanent daemon token
// (td_) that can be exchanged for a 7-day session token (st_). Calls acting
// on behalf of one agent instance carry the X-Agent-ID header; the server
// verifies the instance belongs to this daemon and rewrites the identity.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

// Client is the HTTP client that communicates with the Teammate Server.
//
// The client encapsulates all REST API interactions with the Server, including:
//   - daemon registration, heartbeat and deregistration
//   - node claiming and status reporting (X-Agent-ID scoped)
//   - Git credential retrieval
//   - comment and log sending
//   - token exchange and refresh
type Client struct {
	// BaseURL is the base URL of the Server.
	BaseURL string

	// APIToken is the workspace connection's daemon token (td_ prefix) used
	// for authentication.
	APIToken string

	// SessionToken is the exchanged session token (st_ prefix), valid for 7
	// days.
	SessionToken string

	// SessionExpiry is the expiry time of the Session Token.
	SessionExpiry time.Time

	// PrivateKeyPEM is the daemon-level PEM-encoded RSA private key, used to
	// decrypt Git credentials.
	PrivateKeyPEM string

	// HTTP is the underlying HTTP client instance.
	HTTP *http.Client
}

// NewClient creates a new Server communication client authenticated with the
// daemon token.
func NewClient(baseURL, daemonToken string) *Client {
	return &Client{
		BaseURL:  baseURL,
		APIToken: daemonToken,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// requestOption mutates an outgoing request before it is sent.
type requestOption func(*http.Request)

// withAgentHeader scopes a request to one agent instance via the X-Agent-ID
// header. The server rewrites the caller identity to that instance after
// verifying it was reported by this daemon.
func withAgentHeader(agentID string) requestOption {
	return func(req *http.Request) {
		req.Header.Set("X-Agent-ID", agentID)
	}
}

// do executes an HTTP request, automatically setting auth headers and JSON
// serialization.
// If body is not nil, it serializes it to JSON and sets the Content-Type
// header.
func (c *Client) do(ctx context.Context, method, path string, body interface{}, opts ...requestOption) (*http.Response, error) {
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
	for _, opt := range opts {
		opt(req)
	}

	return c.HTTP.Do(req)
}

// authToken returns the best available auth token.
// It prefers the session token (if present and not close to expiry), otherwise
// falls back to the daemon token.
func (c *Client) authToken() string {
	if c.SessionToken != "" && !c.SessionExpiry.IsZero() && time.Now().Before(c.SessionExpiry.Add(-5*time.Minute)) {
		return c.SessionToken
	}
	return c.APIToken
}

// APIError carries the HTTP status of a failed API call so callers can branch
// on it (e.g. heartbeat 404 meaning the daemon was deleted server-side).
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return "API error " + strconv.Itoa(e.StatusCode) + ": " + e.Body
}

// IsNotFound reports whether the error is an API call that returned 404.
// Callers wrap API errors with %w, so the check must traverse the chain.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsUnauthorized reports whether the error is an API call that returned 401.
// For daemon credentials (td_/st_) a 401 means the token was revoked or its
// owner deleted server-side; there is no token-rotation path, so callers treat
// it as a terminal condition like IsNotFound.
func IsUnauthorized(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized
}

// doJSON executes a JSON API request, automatically handling request body
// serialization and response body deserialization.
// If the response status code is >= 300, it returns an error containing the
// status code and response body.
func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, respBody interface{}, opts ...requestOption) error {
	resp, err := c.do(ctx, method, path, reqBody, opts...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return &APIError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	if respBody != nil {
		return json.NewDecoder(resp.Body).Decode(respBody)
	}
	return nil
}

// ListAgentSkills retrieves the list of skills bound to the agent instance.
func (c *Client) ListAgentSkills(ctx context.Context, workspaceID, agentID string) ([]SkillContext, error) {
	var skills []SkillContext
	path := fmt.Sprintf("/api/workspaces/%s/agents/%s/skills", workspaceID, agentID)
	if err := c.doJSON(ctx, "GET", path, nil, &skills, withAgentHeader(agentID)); err != nil {
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

// ListAgentMcpServers retrieves the list of MCP servers bound to the agent
// instance (via the daemon-only execution endpoint, which returns decrypted
// env_vars).
func (c *Client) ListAgentMcpServers(ctx context.Context, workspaceID, agentID string) ([]AgentMcpServerContext, error) {
	var servers []AgentMcpServerContext
	path := fmt.Sprintf("/api/workspaces/%s/agents/%s/execution/mcp-servers", workspaceID, agentID)
	if err := c.doJSON(ctx, "GET", path, nil, &servers, withAgentHeader(agentID)); err != nil {
		return nil, fmt.Errorf("list agent mcp servers: %w", err)
	}
	return servers, nil
}

// GetAgent retrieves one agent instance's profile (instructions, git
// identity). Scoped to the instance via X-Agent-ID.
func (c *Client) GetAgent(ctx context.Context, workspaceID, agentID string, out interface{}) error {
	path := fmt.Sprintf("/api/workspaces/%s/agents/%s", workspaceID, agentID)
	return c.doJSON(ctx, "GET", path, nil, out, withAgentHeader(agentID))
}

// --- Daemon domain ---

// ProviderInfo is one entry of the tool-detection snapshot reported at
// registration.
type ProviderInfo struct {
	Provider  string `json:"provider"`
	Version   string `json:"version,omitempty"`
	Installed bool   `json:"installed"`
}

// RegisterDaemonReport is the machine-side registration payload: machine
// identity plus the tool-detection snapshot. It carries no instance catalog —
// instances are created server-side and delivered through desired_agents.
type RegisterDaemonReport struct {
	DeviceName string         `json:"device_name"`
	Version    string         `json:"version"`
	PublicKey  string         `json:"public_key"`
	Providers  []ProviderInfo `json:"providers"`
}

// DesiredAgent is one server-owned instance delivered for local
// materialization; AgentID is the server UUID the instance executes as.
type DesiredAgent struct {
	AgentID  string `json:"agent_id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	PersonaKey string `json:"persona_key"`
}

// RegisterDaemonResponse is the registration result: the daemon's own id, the
// workspace the daemon token is bound to, the pending instances to
// materialize, and the heartbeat interval.
type RegisterDaemonResponse struct {
	DaemonID          string         `json:"daemon_id"`
	WorkspaceID       string         `json:"workspace_id"`
	DesiredAgents     []DesiredAgent `json:"desired_agents"`
	HeartbeatInterval int            `json:"heartbeat_interval"`
}

// AgentBusy is one entry of the heartbeat per-agent status payload.
type AgentBusy struct {
	Name string `json:"name"`
	Busy bool   `json:"busy"`
}

// DaemonHeartbeatResponse is the heartbeat result carrying pending instances
// for incremental delivery.
type DaemonHeartbeatResponse struct {
	DesiredAgents     []DesiredAgent `json:"desired_agents"`
	HeartbeatInterval int            `json:"heartbeat_interval"`
}

// RegisterDaemon registers this machine's daemon with its tool-detection
// snapshot. The instance catalog lives server-side; the response carries the
// pending instances to materialize.
func (c *Client) RegisterDaemon(ctx context.Context, report RegisterDaemonReport) (*RegisterDaemonResponse, error) {
	var result RegisterDaemonResponse
	if err := c.doJSON(ctx, "POST", "/api/daemons/register", report, &result); err != nil {
		return nil, fmt.Errorf("register daemon: %w", err)
	}
	return &result, nil
}

// DaemonHeartbeat refreshes the daemon liveness and reports the busy status of
// the instances this daemon has bound identities for. A nil agents slice sends
// an empty list.
func (c *Client) DaemonHeartbeat(ctx context.Context, agents []AgentBusy) (*DaemonHeartbeatResponse, error) {
	body := struct {
		Agents []AgentBusy `json:"agents"`
	}{Agents: agents}
	var result DaemonHeartbeatResponse
	if err := c.doJSON(ctx, "POST", "/api/daemons/heartbeat", body, &result); err != nil {
		return nil, fmt.Errorf("daemon heartbeat: %w", err)
	}
	return &result, nil
}

// DaemonDeregister marks the daemon and all its instances offline (graceful
// shutdown).
func (c *Client) DaemonDeregister(ctx context.Context) error {
	if err := c.doJSON(ctx, "POST", "/api/daemons/deregister", struct{}{}, nil); err != nil {
		return fmt.Errorf("daemon deregister: %w", err)
	}
	return nil
}

// --- Session Token Exchange ---

// ExchangeTokenRequest represents the token exchange request body, used to
// exchange a daemon token for a session token.
type ExchangeTokenRequest struct {
	APIToken string `json:"api_token"`
}

// ExchangeTokenResponse represents the token exchange response body,
// containing the new session token and expiry time.
type ExchangeTokenResponse struct {
	SessionToken string    `json:"session_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ExchangeToken exchanges a daemon token for a session token.
// The session token is used for subsequent API calls and is more secure than
// the daemon token (short-lived).
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
// daemon token.
// On success it updates the client's SessionToken and SessionExpiry fields.
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
// claimed, scoped to the agent instance.
// It first obtains tasks with pending nodes via the board API, then calls the
// node API to get the full node information (including node ID).
func (c *Client) ListPendingNodes(ctx context.Context, agentID, projectID string) ([]TaskNode, error) {
	var result boardResponse
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/projects/%s/board", projectID), nil, &result, withAgentHeader(agentID))
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
		taskNodes, err := c.ListTaskNodes(ctx, agentID, taskID)
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

// ListTaskNodes retrieves all nodes of the specified task, scoped to the agent
// instance.
// Endpoint: GET /api/tasks/{taskId}/nodes
func (c *Client) ListTaskNodes(ctx context.Context, agentID string, taskID int32) ([]TaskNode, error) {
	var result []TaskNode
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/tasks/%d/nodes", taskID), nil, &result, withAgentHeader(agentID))
	return result, err
}

// ListExecutionContextComments retrieves the comment context that should be
// injected when executing the specified node, scoped to the agent instance.
func (c *Client) ListExecutionContextComments(ctx context.Context, agentID string, taskID int32, nodeID string) ([]Comment, error) {
	var result []Comment
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/tasks/%d/comments?node_id=%s&scope=execution_context", taskID, nodeID), nil, &result, withAgentHeader(agentID))
	return result, err
}

// ListNodeComments retrieves comments in the specified node's comment area,
// scoped to the agent instance.
func (c *Client) ListNodeComments(ctx context.Context, agentID string, taskID int32, nodeID string) ([]Comment, error) {
	var result []Comment
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/tasks/%d/comments?node_id=%s", taskID, nodeID), nil, &result, withAgentHeader(agentID))
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

// GetInProgressNodes queries the nodes claimed by the agent instance that are
// not yet completed (in_progress).
// Used to resume unfinished execution after the Agent restarts.
// Endpoint: GET /api/workspaces/{workspaceID}/agents/{agentID}/in-progress-nodes
func (c *Client) GetInProgressNodes(ctx context.Context, workspaceID, agentID string) ([]InProgressNode, error) {
	var result []InProgressNode
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/workspaces/%s/agents/%s/in-progress-nodes", workspaceID, agentID), nil, &result, withAgentHeader(agentID))
	return result, err
}

// ClaimNode claims a pending node, assigning it to the specified agent
// instance.
// Uses optimistic locking for concurrency control; if the node has already
// been claimed by another agent it returns 409 Conflict.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/claim
func (c *Client) ClaimNode(ctx context.Context, agentID string, taskID int32, nodeID string) (*TaskNode, error) {
	var result TaskNode
	err := c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/claim", taskID, nodeID), map[string]string{
		"agent_id": agentID,
	}, &result, withAgentHeader(agentID))
	return &result, err
}

// ApproveNode approves (completes) the current node on behalf of the agent
// instance.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/approve
func (c *Client) ApproveNode(ctx context.Context, agentID string, taskID int32, nodeID, comment string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/approve", taskID, nodeID), map[string]string{
		"operator_id":   agentID,
		"operator_type": "agent",
		"comment":       comment,
	}, nil, withAgentHeader(agentID))
}

// CompleteNode completes a standard node on behalf of the agent instance
// (agent-only call, does not require task:approve permission).
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/complete
func (c *Client) CompleteNode(ctx context.Context, agentID string, taskID int32, nodeID, summary string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/complete", taskID, nodeID), map[string]string{
		"summary": summary,
	}, nil, withAgentHeader(agentID))
}

// RejectNode rejects the current node on behalf of the agent instance, rolling
// it back to the specified target node.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/reject
func (c *Client) RejectNode(ctx context.Context, agentID string, taskID int32, nodeID, targetNodeID, comment string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/reject", taskID, nodeID), map[string]interface{}{
		"operator_id":    agentID,
		"operator_type":  "agent",
		"target_node_id": targetNodeID,
		"comment":        comment,
	}, nil, withAgentHeader(agentID))
}

// ManualIntervention marks the node as requiring manual intervention on
// behalf of the agent instance.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/manual
func (c *Client) ManualIntervention(ctx context.Context, agentID string, taskID int32, nodeID, comment string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/manual", taskID, nodeID), map[string]string{
		"operator_id":   agentID,
		"operator_type": "agent",
		"comment":       comment,
	}, nil, withAgentHeader(agentID))
}

// SkipClaim relinquishes the node's continuation right on behalf of the agent
// instance, allowing other agents to claim subsequent nodes.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/skip-claim
func (c *Client) SkipClaim(ctx context.Context, agentID string, taskID int32, nodeID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/skip-claim", taskID, nodeID), map[string]string{
		"agent_id": agentID,
	}, nil, withAgentHeader(agentID))
}

// --- Token Usage ---

// TokenUsageRequest represents the token usage report request body,
// containing input, output, and total token counts.
type TokenUsageRequest struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ReportTokenUsage reports the token usage of a completed node on behalf of
// the agent instance.
// Endpoint: POST /api/tasks/{taskId}/token-usage
func (c *Client) ReportTokenUsage(ctx context.Context, agentID string, taskID int32, nodeID string, usage TokenUsageRequest) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/token-usage", taskID), map[string]interface{}{
		"task_node_id":  nodeID,
		"agent_id":      agentID,
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
		"total_tokens":  usage.TotalTokens,
	}, nil, withAgentHeader(agentID))
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
// Scoped to the agent instance via X-Agent-ID: the daemon principal has no
// workspace role, but an agent that claimed a node in the project is a
// project member and passes the access check.
func (c *Client) GetGitCredentials(ctx context.Context, agentID, projectID string) ([]GitCredentials, error) {
	var result gitCredentialsResponse
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/projects/%s/git-credentials", projectID), nil, &result, withAgentHeader(agentID))
	if err != nil {
		return nil, err
	}

	creds := make([]GitCredentials, 0, len(result.Credentials))
	for _, entry := range result.Credentials {
		pat := entry.EncryptedPAT
		// If the daemon private key is available, decrypt the PAT
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
// Scoped to the agent instance via X-Agent-ID: workspace-level routes require
// a member role or agent permission, and the daemon principal has neither.
// Endpoint: GET /api/workspaces/{workspaceID}/projects
func (c *Client) ListProjects(ctx context.Context, agentID, workspaceID string) ([]Project, error) {
	var result []Project
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/workspaces/%s/projects", workspaceID), nil, &result, withAgentHeader(agentID))
	return result, err
}

// GetProject retrieves a single project's information, including project-level
// repository configuration. Scoped to the agent instance via X-Agent-ID.
func (c *Client) GetProject(ctx context.Context, agentID, workspaceID, projectID string) (*Project, error) {
	var result Project
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/workspaces/%s/projects/%s", workspaceID, projectID), nil, &result, withAgentHeader(agentID))
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Task Messages ---

// SendMessage sends a task log message on behalf of the agent instance; the
// content is desensitized before upload.
// Endpoint: POST /api/tasks/{taskId}/messages
func (c *Client) SendMessage(ctx context.Context, agentID string, taskID int32, nodeID, content string) error {
	return c.SendMessageWithType(ctx, agentID, taskID, nodeID, "stdout", content)
}

// SendMessageWithType sends a task log message of the specified type on behalf
// of the agent instance; the content is desensitized before upload.
// Endpoint: POST /api/tasks/{taskId}/messages
//
// msgType is one of "stdout", "stderr", "system".
func (c *Client) SendMessageWithType(ctx context.Context, agentID string, taskID int32, nodeID, msgType, content string) error {
	desensitized := DesensitizeLog(content)
	log.Printf("[client:SendMessage] task=%d node=%s type=%s content_len=%d", taskID, nodeID, msgType, len(desensitized))
	err := c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/messages", taskID), map[string]string{
		"node_id": nodeID,
		"type":    msgType,
		"content": desensitized,
	}, nil, withAgentHeader(agentID))
	if err != nil {
		log.Printf("[client:SendMessage] ERROR: %v", err)
	} else {
		log.Printf("[client:SendMessage] success")
	}
	return err
}

// --- Interrupt ---

// ReportInterrupt acknowledges that an interrupt request for a task node has
// been processed, on behalf of the agent instance.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/interrupt-ack
func (c *Client) ReportInterrupt(ctx context.Context, agentID string, taskID int32, nodeID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/interrupt-ack", taskID, nodeID), map[string]string{
		"comment": "interrupt acknowledged by agent",
	}, nil, withAgentHeader(agentID))
}

// --- Task Details ---

// GetTask retrieves task details by task ID on behalf of the agent instance.
// Endpoint: GET /api/projects/{projectID}/tasks/{taskID}
func (c *Client) GetTask(ctx context.Context, agentID, projectID string, taskID int32) (*Task, error) {
	var result Task
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/projects/%s/tasks/%d", projectID, taskID), nil, &result, withAgentHeader(agentID))
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ReportSummary updates the summary information of a completed node on behalf
// of the agent instance.
// Endpoint: POST /api/tasks/{taskId}/nodes/{nodeId}/summary
func (c *Client) ReportSummary(ctx context.Context, agentID string, taskID int32, nodeID, summary string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/summary", taskID, nodeID), map[string]string{
		"summary": summary,
	}, nil, withAgentHeader(agentID))
}

// ReportGitBranch reports the task's Git branch name after the Git workspace
// is initialized successfully, on behalf of the agent instance.
// Endpoint: PUT /api/tasks/{taskId}/git-branch
func (c *Client) ReportGitBranch(ctx context.Context, agentID string, taskID int32, gitBranch string) error {
	return c.doJSON(ctx, "PUT", fmt.Sprintf("/api/tasks/%d/git-branch", taskID), map[string]string{
		"git_branch": gitBranch,
	}, nil, withAgentHeader(agentID))
}

// PostComment posts a comment on a task on behalf of the agent instance.
// Endpoint: POST /api/tasks/{taskId}/comments
func (c *Client) PostComment(ctx context.Context, agentID string, taskID int32, content, authorType, authorID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/comments", taskID), map[string]string{
		"content": content,
	}, nil, withAgentHeader(agentID))
}

// PostNodeComment posts a comment in the specified node's comment area on
// behalf of the agent instance.
func (c *Client) PostNodeComment(ctx context.Context, agentID string, taskID int32, nodeID, sourceNodeID, commentType, content string) error {
	body := map[string]string{
		"node_id":      nodeID,
		"content":      content,
		"comment_type": commentType,
	}
	if sourceNodeID != "" {
		body["source_node_id"] = sourceNodeID
	}
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/comments", taskID), body, nil, withAgentHeader(agentID))
}
