// Package agent 提供 AI 代理守护进程的核心功能。
//
// 本包实现了 Agent Daemon 的完整生命周期，包括：
//   - 运行时注册和心跳维持
//   - SSE 事件监听和响应
//   - 节点认领和任务执行
//   - Git 操作和凭据管理
//   - 上下文构建和工具调用
//   - RSA 加密通信
//
// Client 是与 Server 通信的 HTTP 客户端，封装了所有 API 调用。
// 客户端支持两种认证方式：API Token（永久）和 Session Token（7 天有效期）。
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

// Client 是与 Teammate Server 通信的 HTTP 客户端。
//
// 客户端封装了所有与 Server 的 REST API 交互，包括：
//   - 运行时注册和心跳
//   - 节点认领和状态上报
//   - Git 凭据拉取
//   - 评论和日志发送
//   - Token 交换和刷新
//
// 使用方式：
//
//	client := NewClient("http://localhost:8080", "tm_xxx_xxx")
//	runtime, _ := client.RegisterRuntime(ctx, workspaceID, agentID, "claude", "1.0.0", pubKey)
type Client struct {
	// BaseURL 是 Server 的基础 URL 地址。
	BaseURL string

	// APIToken 是用于初始认证的 API Token（tm_ 前缀）。
	APIToken string

	// SessionToken 是交换后的会话 Token（st_ 前缀），7 天有效期。
	SessionToken string

	// SessionExpiry 是 Session Token 的过期时间。
	SessionExpiry time.Time

	// PrivateKeyPEM 是 RSA 私钥的 PEM 编码，用于解密 Git 凭据。
	PrivateKeyPEM string

	// HTTP 是底层的 HTTP 客户端实例。
	HTTP *http.Client
}

// NewClient 创建一个新的 Server 通信客户端。
//
// 参数：
//   - baseURL: Server 的基础 URL 地址（如 "http://localhost:8080"）
//   - apiToken: 用于初始认证的 API Token
//
// 返回：
//   - *Client: 初始化完成的客户端实例
func NewClient(baseURL, apiToken string) *Client {
	return &Client{
		BaseURL:  baseURL,
		APIToken: apiToken,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// do 执行一个 HTTP 请求，自动设置认证头和 JSON 序列化。
// 如果 body 不为 nil，则将其序列化为 JSON 并设置 Content-Type 头。
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

// authToken 返回当前最佳的认证令牌。
// 优先使用会话令牌（如果存在且未接近过期），否则回退到 API 令牌。
func (c *Client) authToken() string {
	if c.SessionToken != "" && !c.SessionExpiry.IsZero() && time.Now().Before(c.SessionExpiry.Add(-5*time.Minute)) {
		return c.SessionToken
	}
	return c.APIToken
}

// doJSON 执行一个 JSON API 请求，自动处理请求体序列化和响应体反序列化。
// 如果响应状态码 >= 300，返回包含状态码和响应体的错误。
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

// ListAgentSkills 获取当前 Agent 绑定的技能列表。
func (c *Client) ListAgentSkills(ctx context.Context, workspaceID, agentID string) ([]SkillContext, error) {
	var skills []SkillContext
	path := fmt.Sprintf("/api/workspaces/%s/agents/%s/skills", workspaceID, agentID)
	if err := c.doJSON(ctx, "GET", path, nil, &skills); err != nil {
		return nil, fmt.Errorf("list agent skills: %w", err)
	}
	return skills, nil
}

// AgentMcpServerContext 表示执行期可注入给 Agent 的 MCP 服务器配置。
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

// ListAgentMcpServers 获取当前 Agent 绑定的 MCP 服务器列表（通过 daemon-only 执行端点，返回解密后的 env_vars）。
func (c *Client) ListAgentMcpServers(ctx context.Context, workspaceID, agentID string) ([]AgentMcpServerContext, error) {
	var servers []AgentMcpServerContext
	path := fmt.Sprintf("/api/workspaces/%s/agents/%s/execution/mcp-servers", workspaceID, agentID)
	if err := c.doJSON(ctx, "GET", path, nil, &servers); err != nil {
		return nil, fmt.Errorf("list agent mcp servers: %w", err)
	}
	return servers, nil
}

// --- Runtime ---

// RegisterRuntimeRequest 表示运行时注册请求体，与服务端 registerRuntimeRequest 结构对应。
type RegisterRuntimeRequest struct {
	AgentID          string `json:"agent_id"`
	DaemonID         string `json:"daemon_id"`
	Provider         string `json:"provider"`
	Version          string `json:"version"`
	Status           string `json:"status"`
	SessionTokenHash string `json:"session_token_hash"`
	PublicKey        string `json:"public_key"`
}

// RegisterRuntimeResponse 表示运行时注册响应体，包含新创建的运行时 ID。
type RegisterRuntimeResponse struct {
	ID string `json:"id"`
}

// RegisterRuntime 将守护进程注册为代理的运行时实例。
// 注册成功后返回运行时 ID，用于后续的心跳和事件接收。
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - workspaceID: 工作区 ID
//   - agentID: 代理 ID
//   - provider: 编码工具提供者（如 "claude"）
//   - toolVersion: 编码工具版本号
//   - publicKeyPEM: RSA 公钥的 PEM 编码，用于服务端加密 Git 凭据
//
// 返回：
//   - *RegisterRuntimeResponse: 注册响应，包含运行时 ID
//   - error: 注册失败时返回错误
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

// ExchangeTokenRequest 表示令牌交换请求体，用于将 API 令牌交换为会话令牌。
type ExchangeTokenRequest struct {
	APIToken string `json:"api_token"`
}

// ExchangeTokenResponse 表示令牌交换响应体，包含新的会话令牌和过期时间。
type ExchangeTokenResponse struct {
	SessionToken string    `json:"session_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ExchangeToken 将 API 令牌交换为会话令牌。
// 会话令牌用于后续的 API 调用，比 API 令牌更安全（短期有效）。
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - apiToken: 用于交换的 API 令牌
//
// 返回：
//   - sessionToken: 新的会话令牌
//   - expiresAt: 会话令牌的过期时间
//   - error: 交换失败时返回错误
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

// RefreshSessionToken 尝试用 API 令牌重新交换新的会话令牌。
// 交换成功后更新客户端的 SessionToken 和 SessionExpiry 字段。
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//
// 返回：
//   - error: 交换失败时返回错误
func (c *Client) RefreshSessionToken(ctx context.Context) error {
	token, expiresAt, err := c.ExchangeToken(ctx, c.APIToken)
	if err != nil {
		return fmt.Errorf("refresh session token: %w", err)
	}
	c.SessionToken = token
	c.SessionExpiry = expiresAt
	return nil
}

// StartSessionTokenRefresher 启动后台协程，在会话令牌过期前自动刷新。
// 刷新时机为过期前 5 分钟，失败后 30 秒重试一次。
//
// 参数：
//   - stopCh: 停止信号通道，关闭时协程退出
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

			// 在过期前 5 分钟刷新
			refreshAt := c.SessionExpiry.Add(-5 * time.Minute)
			waitDuration := time.Until(refreshAt)
			if waitDuration < 0 {
				waitDuration = 0
			}

			select {
			case <-time.After(waitDuration):
				if err := c.RefreshSessionToken(context.Background()); err != nil {
					// 失败后 30 秒重试
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

// Heartbeat 向 Server 发送心跳，维持运行时的在线状态。
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - workspaceID: 工作区 ID
//   - runtimeID: 运行时 ID
//
// 返回：
//   - error: 发送失败时返回错误
func (c *Client) Heartbeat(ctx context.Context, workspaceID, runtimeID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/workspaces/%s/runtimes/%s/heartbeat", workspaceID, runtimeID), nil, nil)
}

// --- Node Operations ---

// TaskNode 表示从 API 获取的任务节点信息，包含节点状态、类型和执行信息。
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
	ReadonlyDirs    json.RawMessage `json:"readonly_dirs"`     // 只读目录（JSON 数组），模板节点配置
	FullControlDirs json.RawMessage `json:"full_control_dirs"` // 完全控制目录（JSON 数组），模板节点配置
}

// Comment 表示任务或节点评论，用于执行上下文和节点交接。
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

// UnmarshalJSON 实现 TaskNode 的自定义 JSON 反序列化。
// 服务端返回的 sql.NullString 字段格式为 {"String":"...","Valid":true}，
// 标准字符串反序列化无法处理，需要特殊处理。
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

// boardResponse 表示看板 API 的响应格式，包含按状态分组的任务列。
type boardResponse struct {
	Columns []boardColumn `json:"columns"`
}

// boardColumn 表示看板的一列，包含列标识、标签和任务列表。
type boardColumn struct {
	Key   string            `json:"key"`
	Label string            `json:"label"`
	Tasks []boardColumnTask `json:"tasks"`
}

// boardColumnTask 表示看板列中的一个任务条目，包含任务基本信息和当前节点状态。
type boardColumnTask struct {
	ID                int32       `json:"id"`
	Title             string      `json:"title"`
	Priority          string      `json:"priority"`
	Type              string      `json:"type"`
	CurrentNodeName   string      `json:"current_node_name"`
	CurrentNodeStatus string      `json:"current_node_status"`
	AssigneeID        interface{} `json:"assignee_id"`
}

// ListPendingNodes 返回项目中可认领的 pending 节点列表。
// 先通过看板 API 获取有 pending 节点的任务，再调用节点 API 获取完整的节点信息（包括节点 ID）。
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - projectID: 项目 ID
//
// 返回：
//   - []TaskNode: 可认领的 pending 节点列表
//   - error: 查询失败时返回错误
func (c *Client) ListPendingNodes(ctx context.Context, projectID string) ([]TaskNode, error) {
	var result boardResponse
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/projects/%s/board", projectID), nil, &result)
	if err != nil {
		return nil, err
	}

	// 从 pending 列收集任务 ID（review 节点现已合并到同一列中）
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

	// 为每个 pending 任务获取完整节点详情以得到节点 ID
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

// ListTaskNodes 获取指定任务的所有节点。
// 接口：GET /api/tasks/{taskId}/nodes
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - taskID: 任务 ID
//
// 返回：
//   - []TaskNode: 任务节点列表
//   - error: 查询失败时返回错误
func (c *Client) ListTaskNodes(ctx context.Context, taskID int32) ([]TaskNode, error) {
	var result []TaskNode
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/tasks/%d/nodes", taskID), nil, &result)
	return result, err
}

// ListExecutionContextComments 获取执行指定节点时应注入的评论上下文。
func (c *Client) ListExecutionContextComments(ctx context.Context, taskID int32, nodeID string) ([]Comment, error) {
	var result []Comment
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/tasks/%d/comments?node_id=%s&scope=execution_context", taskID, nodeID), nil, &result)
	return result, err
}

// ListNodeComments 获取指定节点评论区中的评论。
func (c *Client) ListNodeComments(ctx context.Context, taskID int32, nodeID string) ([]Comment, error) {
	var result []Comment
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/tasks/%d/comments?node_id=%s", taskID, nodeID), nil, &result)
	return result, err
}

// InProgressNode 表示 Agent 认领但未完成的节点，用于重启后恢复执行。
type InProgressNode struct {
	ID              string          `json:"id"`
	TaskID          int32           `json:"task_id"`
	Name            string          `json:"name"`
	SortOrder       int32           `json:"sort_order"`
	NodeType        string          `json:"node_type"`
	Status          string          `json:"status"`
	ReadonlyDirs    json.RawMessage `json:"readonly_dirs"`     // 只读目录（JSON 数组）
	FullControlDirs json.RawMessage `json:"full_control_dirs"` // 完全控制目录（JSON 数组）
	ProjectID       string          `json:"project_id"`
}

// GetInProgressNodes 查询当前 Agent 认领但未完成（in_progress）的节点。
// 用于 Agent 重启后恢复未完成的执行。
// 接口：GET /api/workspaces/{workspaceID}/agents/{agentID}/in-progress-nodes
//
// 参数：
//   - ctx: 上下文
//   - workspaceID: 工作区 ID
//   - agentID: 代理 ID
//
// 返回：
//   - []InProgressNode: in_progress 节点列表
//   - error: 查询失败时返回错误
func (c *Client) GetInProgressNodes(ctx context.Context, workspaceID, agentID string) ([]InProgressNode, error) {
	var result []InProgressNode
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/workspaces/%s/agents/%s/in-progress-nodes", workspaceID, agentID), nil, &result)
	return result, err
}

// ClaimNode 认领一个 pending 节点，将其分配给指定代理。
// 使用乐观锁实现并发控制，如果节点已被其他代理认领则返回 409 Conflict。
// 接口：POST /api/tasks/{taskId}/nodes/{nodeId}/claim
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - agentID: 要认领节点的代理 ID
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//
// 返回：
//   - *TaskNode: 认领成功后的节点信息
//   - error: 认领失败时返回错误（如 409 Conflict）
func (c *Client) ClaimNode(ctx context.Context, agentID string, taskID int32, nodeID string) (*TaskNode, error) {
	var result TaskNode
	err := c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/claim", taskID, nodeID), map[string]string{
		"agent_id": agentID,
	}, &result)
	return &result, err
}

// ApproveNode 审批通过（完成）当前节点。
// 接口：POST /api/tasks/{taskId}/nodes/{nodeId}/approve
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - agentID: 审批者代理 ID
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//   - comment: 审批意见
//
// 返回：
//   - error: 审批失败时返回错误
func (c *Client) ApproveNode(ctx context.Context, agentID string, taskID int32, nodeID, comment string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/approve", taskID, nodeID), map[string]string{
		"operator_id":   agentID,
		"operator_type": "agent",
		"comment":       comment,
	}, nil)
}

// CompleteNode 完成一个标准节点（仅代理调用，不需要 task:approve 权限）。
// 接口：POST /api/tasks/{taskId}/nodes/{nodeId}/complete
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - agentID: 执行代理 ID
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//   - summary: 节点执行摘要
//
// 返回：
//   - error: 完成失败时返回错误
func (c *Client) CompleteNode(ctx context.Context, agentID string, taskID int32, nodeID, summary string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/complete", taskID, nodeID), map[string]string{
		"summary": summary,
	}, nil)
}

// RejectNode 驳回当前节点，将其回退到指定的目标节点。
// 接口：POST /api/tasks/{taskId}/nodes/{nodeId}/reject
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - agentID: 驳回者代理 ID
//   - taskID: 任务 ID
//   - nodeID: 被驳回的节点 ID
//   - targetNodeID: 回退目标节点 ID
//   - comment: 驳回意见
//
// 返回：
//   - error: 驳回失败时返回错误
func (c *Client) RejectNode(ctx context.Context, agentID string, taskID int32, nodeID, targetNodeID, comment string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/reject", taskID, nodeID), map[string]interface{}{
		"operator_id":    agentID,
		"operator_type":  "agent",
		"target_node_id": targetNodeID,
		"comment":        comment,
	}, nil)
}

// ManualIntervention 将节点标记为需要人工干预。
// 接口：POST /api/tasks/{taskId}/nodes/{nodeId}/manual
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - agentID: 操作者代理 ID
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//   - comment: 干预原因说明
//
// 返回：
//   - error: 操作失败时返回错误
func (c *Client) ManualIntervention(ctx context.Context, agentID string, taskID int32, nodeID, comment string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/manual", taskID, nodeID), map[string]string{
		"operator_id":   agentID,
		"operator_type": "agent",
		"comment":       comment,
	}, nil)
}

// SkipClaim 放弃节点的续约权，允许其他代理认领后续节点。
// 接口：POST /api/tasks/{taskId}/nodes/{nodeId}/skip-claim
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - agentID: 放弃续约权的代理 ID
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//
// 返回：
//   - error: 操作失败时返回错误
func (c *Client) SkipClaim(ctx context.Context, agentID string, taskID int32, nodeID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/skip-claim", taskID, nodeID), map[string]string{
		"agent_id": agentID,
	}, nil)
}

// --- Token Usage ---

// TokenUsageRequest 表示 Token 用量上报请求体，包含输入、输出和总 Token 数。
type TokenUsageRequest struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ReportTokenUsage 上报已完成节点的 Token 用量。
// 接口：POST /api/tasks/{taskId}/token-usage
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//   - agentID: 执行代理 ID
//   - usage: Token 用量信息
//
// 返回：
//   - error: 上报失败时返回错误
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

// GitCredentials 表示解密后的 Git 凭据，包含仓库 URL、用户名和个人访问令牌。
type GitCredentials struct {
	RepoURL  string `json:"repo_url"`
	Username string `json:"username"`
	PAT      string `json:"pat"` // 解密后的个人访问令牌（RSA 解密后）
}

// gitCredentialsResponse 表示 Git 凭据 API 的响应格式。
type gitCredentialsResponse struct {
	Credentials []gitCredentialEntry `json:"credentials"`
}

// gitCredentialEntry 表示服务端返回的单个凭据条目，包含加密的个人访问令牌。
type gitCredentialEntry struct {
	RepoURL      string `json:"repo_url"`
	Username     string `json:"username"`
	EncryptedPAT string `json:"encrypted_pat"`
}

// GetGitCredentials 获取并解密项目的 Git 凭据。
// 返回凭据列表（每个配置的 repo_url 一个）。
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - projectID: 项目 ID
//
// 返回：
//   - []GitCredentials: 解密后的凭据列表
//   - error: 获取或解密失败时返回错误
func (c *Client) GetGitCredentials(ctx context.Context, projectID string) ([]GitCredentials, error) {
	var result gitCredentialsResponse
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/projects/%s/git-credentials", projectID), nil, &result)
	if err != nil {
		return nil, err
	}

	creds := make([]GitCredentials, 0, len(result.Credentials))
	for _, entry := range result.Credentials {
		pat := entry.EncryptedPAT
		// 如果有私钥，则解密 PAT
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

// Project 表示从 API 获取的项目信息，包含项目 ID 和名称。
type Project struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	RepoURL     string `json:"repo_url"`
}

// ListProjects 获取工作区中的所有项目。
// 接口：GET /api/workspaces/{workspaceID}/projects
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - workspaceID: 工作区 ID
//
// 返回：
//   - []Project: 项目列表
//   - error: 查询失败时返回错误
func (c *Client) ListProjects(ctx context.Context, workspaceID string) ([]Project, error) {
	var result []Project
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/workspaces/%s/projects", workspaceID), nil, &result)
	return result, err
}

// GetProject 获取单个项目信息，包括项目级仓库配置。
func (c *Client) GetProject(ctx context.Context, workspaceID, projectID string) (*Project, error) {
	var result Project
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/workspaces/%s/projects/%s", workspaceID, projectID), nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Task Messages ---

// SendMessage 发送任务日志消息，内容在上传前会进行脱敏处理。
// 接口：POST /api/tasks/{taskId}/messages
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//   - content: 日志内容
//
// 返回：
//   - error: 发送失败时返回错误
func (c *Client) SendMessage(ctx context.Context, taskID int32, nodeID, content string) error {
	return c.SendMessageWithType(ctx, taskID, nodeID, "stdout", content)
}

// SendMessageWithType 发送指定类型的任务日志消息，内容在上传前会进行脱敏处理。
// 接口：POST /api/tasks/{taskId}/messages
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//   - msgType: 消息类型 ("stdout"、"stderr"、"system")
//   - content: 日志内容
//
// 返回：
//   - error: 发送失败时返回错误
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

// ReportInterrupt 确认已处理任务节点的中断请求。
// 接口：POST /api/tasks/{taskId}/nodes/{nodeId}/interrupt-ack
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//
// 返回：
//   - error: 确认失败时返回错误
func (c *Client) ReportInterrupt(ctx context.Context, taskID int32, nodeID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/interrupt-ack", taskID, nodeID), map[string]string{
		"comment": "interrupt acknowledged by agent",
	}, nil)
}

// --- Task Details ---

// GetTask 根据任务 ID 获取任务详情。
// 接口：GET /api/projects/{projectID}/tasks/{taskID}
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - projectID: 项目 ID
//   - taskID: 任务 ID
//
// 返回：
//   - *Task: 任务详情
//   - error: 查询失败时返回错误
func (c *Client) GetTask(ctx context.Context, projectID string, taskID int32) (*Task, error) {
	var result Task
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/api/projects/%s/tasks/%d", projectID, taskID), nil, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ReportSummary 更新已完成节点的摘要信息。
// 接口：POST /api/tasks/{taskId}/nodes/{nodeId}/summary
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - taskID: 任务 ID
//   - nodeID: 节点 ID
//   - summary: 节点执行摘要
//
// 返回：
//   - error: 更新失败时返回错误
func (c *Client) ReportSummary(ctx context.Context, taskID int32, nodeID, summary string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/nodes/%s/summary", taskID, nodeID), map[string]string{
		"summary": summary,
	}, nil)
}

// ReportGitBranch 在 Git 工作区初始化成功后，上报任务的 Git 分支名称。
// 接口：PUT /api/tasks/{taskId}/git-branch
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - taskID: 任务 ID
//   - gitBranch: Git 分支名称
//
// 返回：
//   - error: 上报失败时返回错误
func (c *Client) ReportGitBranch(ctx context.Context, taskID int32, gitBranch string) error {
	return c.doJSON(ctx, "PUT", fmt.Sprintf("/api/tasks/%d/git-branch", taskID), map[string]string{
		"git_branch": gitBranch,
	}, nil)
}

// PostComment 在任务上发布评论。
// 接口：POST /api/tasks/{taskId}/comments
//
// 参数：
//   - ctx: 上下文，用于控制请求超时和取消
//   - taskID: 任务 ID
//   - content: 评论内容
//   - authorType: 作者类型（"agent" 或 "human"）
//   - authorID: 作者 ID
//
// 返回：
//   - error: 发布失败时返回错误
func (c *Client) PostComment(ctx context.Context, taskID int32, content, authorType, authorID string) error {
	return c.doJSON(ctx, "POST", fmt.Sprintf("/api/tasks/%d/comments", taskID), map[string]string{
		"content": content,
	}, nil)
}

// PostNodeComment 在指定节点评论区发布评论。
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
