// context.go 实现节点执行上下文的构建逻辑。
//
// 本文件负责将多个信息源按优先级组合成编码工具的执行 prompt，主要包括：
//   - ContextSection 定义：带优先级和可截断标志的上下文区段
//   - BuildExecutionContext：按优先级从高到低构建完整上下文，超过 80% 窗口时智能截断
//   - 上下文来源：约束/警告、节点描述、任务描述、前序节点结果、代理指令、
//     技能上下文、共享记忆、项目上下文、工作区上下文
//   - estimateCharToTokenRatio：根据中英文比例估算字符与 Token 的转换比率
//   - nullString 兼容：处理服务端 sql.NullString 与纯字符串两种 JSON 格式
//
// 上下文注入遵循优先级降序：数值越小优先级越高，在窗口不足时优先保留。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// ContextSection 表示执行上下文的一个区段，按优先级排序后注入到编码工具的 prompt 中。
// 优先级数值越小越重要，在上下文窗口不足时会被保留。
type ContextSection struct {
	Name           string
	Content        string
	Priority       int  // 优先级，数值越小优先级越高（越不容易被截断）
	NonTruncatable bool // 如果为 true，该区段绝不截断
}

// Task 表示从 API 获取的任务信息，用于构建执行上下文。
// 包含任务的基本信息、描述、约束条件以及所属项目和工作区的 ID。
type Task struct {
	ID          int32  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Constraints string `json:"constraints"`
	ProjectID   string `json:"project_id"`
	WorkspaceID string `json:"workspace_id"`
}

// UnmarshalJSON 自定义 Task 的 JSON 反序列化，兼容 sql.NullString 格式。
// API 返回的 sql.NullString 字段格式为 {"String":"...","Valid":true}，
// 标准字符串反序列化无法处理。
func (t *Task) UnmarshalJSON(data []byte) error {
	type Alias Task
	aux := &struct {
		Description nullString `json:"description"`
		Constraints nullString `json:"constraints"`
		*Alias
	}{
		Alias: (*Alias)(t),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	t.Description = aux.Description.String()
	t.Constraints = aux.Constraints.String()
	return nil
}

// nullString 兼容纯字符串和 sql.NullString 两种 JSON 格式。
// 服务端可能返回任一格式，此类型用于统一处理。
type nullString struct {
	Valid  bool
	StrVal string
}

// UnmarshalJSON 实现自定义 JSON 反序列化，优先尝试纯字符串格式，回退到 sql.NullString 格式。
func (ns *nullString) UnmarshalJSON(data []byte) error {
	// 尝试纯字符串格式
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		ns.StrVal = s
		ns.Valid = s != ""
		return nil
	}
	// 尝试 sql.NullString 格式 {"String":"...","Valid":true}
	var nss struct {
		String string `json:"String"`
		Valid  bool   `json:"Valid"`
	}
	if err := json.Unmarshal(data, &nss); err != nil {
		return err
	}
	ns.StrVal = nss.String
	ns.Valid = nss.Valid
	return nil
}

// String 返回 nullString 的有效值，如果无效则返回空字符串。
func (ns *nullString) String() string {
	if ns.Valid {
		return ns.StrVal
	}
	return ""
}

// WorkspaceContext 表示工作区级别的上下文信息，包含 ID、名称和描述。
type WorkspaceContext struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ProjectContext 表示项目级别的上下文信息，包含 ID、名称和描述。
type ProjectContext struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	RepoURL     string `json:"repo_url"`
}

// SharedMemory 表示一条共享记忆条目，用于跨任务的知识传递。
// 包含记忆 ID、标题、内容和相关性得分。
type SharedMemory struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// AgentInstructions 表示代理的身份指令，包含行为指引和 Git 身份信息。
// 指令会注入到执行上下文中，指导代理的行为模式。
type AgentInstructions struct {
	Instructions string `json:"instructions"`
	GitName      string `json:"git_name"`
	GitEmail     string `json:"git_email"`
}

// SkillContext 表示技能信息，用于注入到执行上下文中。
// 包含技能的 ID、名称、描述和提示模板。
type SkillContext struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	PromptTemplate string `json:"prompt_template"`
	Enabled        bool   `json:"enabled"`
	Category       string `json:"category,omitempty"`
	AssignedAt     string `json:"assigned_at,omitempty"`
}

// UnmarshalJSON 将缺失的 enabled 字段视为启用，以兼容旧版 API
// 响应和轻量级测试夹具。显式 enabled=false 仍会禁用技能。
func (s *SkillContext) UnmarshalJSON(data []byte) error {
	type skillContextAlias SkillContext
	aux := struct {
		Enabled *bool `json:"enabled"`
		*skillContextAlias
	}{
		skillContextAlias: (*skillContextAlias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.Enabled == nil {
		s.Enabled = true
	} else {
		s.Enabled = *aux.Enabled
	}
	return nil
}

// estimateCharToTokenRatio 根据内容语言估算字符与 Token 的比例。
// 纯英文约 4 字符/Token，纯中文约 1.5 字符/Token，混合内容线性插值。
func estimateCharToTokenRatio(content string) float64 {
	cjkCount := 0
	totalRunes := 0
	for _, r := range content {
		totalRunes++
		if (r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一汉字
			(r >= 0x3400 && r <= 0x4DBF) || // CJK 扩展 A
			(r >= 0x3040 && r <= 0x309F) || // 平假名
			(r >= 0x30A0 && r <= 0x30FF) || // 片假名
			(r >= 0xAC00 && r <= 0xD7AF) { // 韩语音节
			cjkCount++
		}
	}
	if totalRunes == 0 {
		return 4.0
	}
	cjkRatio := float64(cjkCount) / float64(totalRunes)
	// 线性插值：纯英文 4.0 字符/Token，纯中文 1.5 字符/Token
	return 4.0 - cjkRatio*(4.0-1.5)
}

// BuildExecutionContext 按优先级顺序构建节点的完整执行上下文。
// 优先级（从高到低）：约束/警告 → 节点描述 → 任务描述 →
// 代理指令 → 技能上下文 → 共享记忆 → 项目上下文 → 工作区上下文。
// 当超过上下文窗口的 80% 时，从低优先级开始截断。
// isResume 为 true 时只包含必要区段，因为 --resume 保留了之前的上下文。
func BuildExecutionContext(client *Client, cfg *Config, task Task, node TaskNode, isResume bool) (string, error) {
	return BuildExecutionContextWithCapabilities(client, cfg, task, node, isResume, PromptCapabilities{IncludeSkills: true, IncludeMCP: true})
}

func BuildExecutionContextWithCapabilities(client *Client, cfg *Config, task Task, node TaskNode, isResume bool, caps PromptCapabilities) (string, error) {
	sections := buildContextSections(client, cfg, task, node, isResume, caps)

	// 估算 Token 数量（粗略：1 Token ≈ 4 字符）
	maxTokens := cfg.Agent.ContextWindow
	if maxTokens <= 0 {
		maxTokens = 100000
	}
	contextLimit := int(float64(maxTokens) * 0.8)
	// 根据内容语言估算字符/Token 比例
	var allContent strings.Builder
	for _, sec := range sections {
		allContent.WriteString(sec.Content)
	}
	charPerToken := estimateCharToTokenRatio(allContent.String())
	charLimit := int(float64(contextLimit) * charPerToken)

	var sb strings.Builder
	// 从最高优先级到最低优先级构建，达到限制时停止
	totalChars := 0
	truncated := false

	// 第一轮：包含所有不可截断的区段（这些区段绝不能被裁剪）
	for _, sec := range sections {
		if sec.Content == "" || !sec.NonTruncatable {
			continue
		}
		sectionText := formatSection(sec)
		sb.WriteString(sectionText)
		totalChars += len(sectionText)
	}

	// 第二轮：在剩余预算内添加可截断区段
	for _, sec := range sections {
		if sec.Content == "" || sec.NonTruncatable {
			continue // 已在第一轮中包含
		}

		sectionText := formatSection(sec)
		sectionLen := len(sectionText)

		if totalChars+sectionLen > charLimit {
			// 尝试放入截断版本
			remaining := charLimit - totalChars
			if remaining > 200 {
				// 包含截断版本
				truncatedContent := sec.Content
				if len(truncatedContent) > remaining-100 {
					truncatedContent = truncatedContent[:remaining-100] + "\n[...truncated...]"
				}
				sb.WriteString(formatSection(ContextSection{
					Name:     sec.Name,
					Content:  truncatedContent,
					Priority: sec.Priority,
				}))
				totalChars += remaining
			}
			truncated = true
			break
		}

		sb.WriteString(sectionText)
		totalChars += sectionLen
	}

	if truncated {
		log.Printf("[context] context truncated to ~%d tokens, some lower-priority sections omitted", totalChars/4)
	}

	return sb.String(), nil
}

func buildContextSections(client *Client, cfg *Config, task Task, node TaskNode, isResume bool, caps PromptCapabilities) []ContextSection {
	sections := make([]ContextSection, 0, 8)

	// 1. 约束/警告（最高优先级，不可截断：对安全至关重要）
	sections = append(sections, ContextSection{
		Name:           "Constraints & Warnings",
		Content:        task.Constraints,
		Priority:       1,
		NonTruncatable: true,
	})

	// 2. 目录权限（安全约束，不可截断；仅当模板节点配置了只读/完全控制目录时注入）
	if dirCtx := buildDirectoryPermissions(node); dirCtx != "" {
		sections = append(sections, ContextSection{
			Name:           "Directory Permissions",
			Content:        dirCtx,
			Priority:       1,
			NonTruncatable: true,
		})
	}

	// 3. 节点描述（对了解要做什么至关重要）
	nodeDesc := node.Description
	if nodeDesc == "" {
		nodeDesc = node.Name
	}
	sections = append(sections, ContextSection{
		Name:     "Node Description",
		Content:  nodeDesc,
		Priority: 2,
	})

	// 恢复会话时，--resume 保留了之前的推理上下文。
	// 仍然注入节点评论上下文，因为用户回复和上游 handoff 都通过评论传递。
	if isResume {
		commentCtx := fetchExecutionComments(client, task.ID, node.ID)
		sections = append(sections, ContextSection{
			Name:           "Node Comments",
			Content:        commentCtx,
			Priority:       4,
			NonTruncatable: true,
		})

		// 5. 代理指令（不可截断：对代理身份至关重要）
		agentCtx := fetchAgentInstructions(client, cfg)
		sections = append(sections, ContextSection{
			Name:           "Agent Instructions",
			Content:        agentCtx,
			Priority:       5,
			NonTruncatable: true,
		})
		return sections
	}

	// 3. 任务描述（对理解整体目标至关重要）
	taskDesc := task.Title
	if task.Description != "" {
		taskDesc += "\n\n" + task.Description
	}
	sections = append(sections, ContextSection{
		Name:     "Task Description",
		Content:  taskDesc,
		Priority: 3,
	})

	commentCtx := fetchExecutionComments(client, task.ID, node.ID)
	sections = append(sections, ContextSection{
		Name:           "Node Comments",
		Content:        commentCtx,
		Priority:       4,
		NonTruncatable: true,
	})

	// 4. 代理指令（不可截断：对代理身份至关重要）
	agentCtx := fetchAgentInstructions(client, cfg)
	sections = append(sections, ContextSection{
		Name:           "Agent Instructions",
		Content:        agentCtx,
		Priority:       5,
		NonTruncatable: true,
	})

	// 6. 技能上下文
	if caps.IncludeSkills {
		skillCtx := fetchSkillContext(client, cfg)
		sections = append(sections, ContextSection{
			Name:     "Skill Context",
			Content:  skillCtx,
			Priority: 6,
		})
	}

	// 7. MCP 服务器上下文
	if caps.IncludeMCP {
		mcpCtx := fetchMCPContext(client, cfg)
		sections = append(sections, ContextSection{
			Name:     "MCP Servers",
			Content:  mcpCtx,
			Priority: 7,
		})
	}

	// 8. 共享记忆（Top-K 相关）
	memCtx := fetchSharedMemory(client, task)
	sections = append(sections, ContextSection{
		Name:     "Shared Memory",
		Content:  memCtx,
		Priority: 8,
	})

	// 9. 项目描述
	projCtx := fetchProjectContext(client, cfg, task.ProjectID)
	sections = append(sections, ContextSection{
		Name:     "Project Context",
		Content:  projCtx,
		Priority: 9,
	})

	// 10. 工作区描述（最低优先级，最先被截断）
	wsCtx := fetchWorkspaceContext(client, cfg)
	sections = append(sections, ContextSection{
		Name:     "Workspace Context",
		Content:  wsCtx,
		Priority: 10,
	})

	return sections
}

func formatSection(sec ContextSection) string {
	if sec.Content == "" {
		return ""
	}
	return fmt.Sprintf("## %s\n%s\n\n", sec.Name, sec.Content)
}

// buildDirectoryPermissions 根据节点配置的目录权限生成提示词内容。
// 目录权限来自工作流模板节点的 readonly_dirs / full_control_dirs（JSON 数组）。
// 仅当至少配置了一类目录时返回非空内容；两者皆空返回空字符串（不注入，保持零回归）。
func buildDirectoryPermissions(node TaskNode) string {
	var lines []string
	if dirs := parseJSONStringArray(node.ReadonlyDirs); len(dirs) > 0 {
		lines = append(lines, "Read-only directories (DO NOT modify — no create/edit/delete):\n"+strings.Join(dirs, ", "))
	}
	if dirs := parseJSONStringArray(node.FullControlDirs); len(dirs) > 0 {
		lines = append(lines, "Full-control directories (you may freely modify):\n"+strings.Join(dirs, ", "))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// parseJSONStringArray 将 JSON 数组（如 ["/docs","/README.md"]）解析为字符串切片。
// 空值（nil / "null" / 空数组）返回 nil。
func parseJSONStringArray(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var dirs []string
	if err := json.Unmarshal(raw, &dirs); err != nil {
		return nil
	}
	return dirs
}

// fetchExecutionComments 获取当前节点执行所需的评论上下文。
func fetchExecutionComments(client *Client, taskID int32, nodeID string) string {
	if taskID == 0 || nodeID == "" {
		return ""
	}
	comments, err := client.ListExecutionContextComments(context.Background(), taskID, nodeID)
	if err != nil {
		log.Printf("[context] failed to fetch execution comments: %v", err)
		return ""
	}
	if len(comments) == 0 {
		return ""
	}

	var sb strings.Builder
	for i, c := range comments {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		scope := "task"
		if c.NodeID != nil && *c.NodeID == nodeID {
			scope = "current_node"
		}
		if c.SourceNodeID != nil {
			sb.WriteString(fmt.Sprintf("[%s][%s][from_node:%s] %s", scope, c.CommentType, *c.SourceNodeID, c.Content))
		} else {
			sb.WriteString(fmt.Sprintf("[%s][%s] %s", scope, c.CommentType, c.Content))
		}
	}
	return sb.String()
}

// fetchWorkspaceContext 获取工作区级别的上下文信息，失败时返回空字符串。
func fetchWorkspaceContext(client *Client, cfg *Config) string {
	var ws WorkspaceContext
	err := client.doJSON(context.Background(), "GET", fmt.Sprintf("/api/workspaces/%s", cfg.Workspace.ID), nil, &ws)
	if err != nil {
		log.Printf("[context] failed to fetch workspace context: %v", err)
		return ""
	}
	if ws.Description == "" {
		return ws.Name
	}
	return ws.Name + "\n" + ws.Description
}

// fetchProjectContext 获取项目级别的上下文信息，失败时返回空字符串。
func fetchProjectContext(client *Client, cfg *Config, projectID string) string {
	if projectID == "" {
		return ""
	}
	var proj ProjectContext
	err := client.doJSON(context.Background(), "GET", fmt.Sprintf("/api/workspaces/%s/projects/%s", cfg.Workspace.ID, projectID), nil, &proj)
	if err != nil {
		log.Printf("[context] failed to fetch project context: %v", err)
		return ""
	}
	if proj.Description == "" {
		return proj.Name
	}
	return proj.Name + "\n" + proj.Description
}

// fetchSharedMemory 获取 Top-K 条相关共享记忆，仅获取已验证或高置信度的记忆以防止低质量内容污染。
func fetchSharedMemory(client *Client, task Task) string {
	if task.WorkspaceID == "" {
		return ""
	}
	var memories []SharedMemory
	// 只获取已验证或高置信度的记忆以防止低质量内容污染
	err := client.doJSON(context.Background(), "GET", fmt.Sprintf("/api/memories?limit=5&verified=true&min_confidence=0.7"), nil, &memories)
	if err != nil {
		log.Printf("[context] failed to fetch shared memory: %v", err)
		return ""
	}
	if len(memories) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, m := range memories {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString(fmt.Sprintf("### %s\n%s", m.Title, m.Content))
	}
	return sb.String()
}

// fetchAgentInstructions 获取代理的身份指令，失败时返回空字符串。
func fetchAgentInstructions(client *Client, cfg *Config) string {
	var agent AgentInstructions
	err := client.doJSON(context.Background(), "GET", fmt.Sprintf("/api/workspaces/%s/agents/%s", cfg.Workspace.ID, cfg.Agent.ID), nil, &agent)
	if err != nil {
		log.Printf("[context] failed to fetch agent instructions: %v", err)
		return ""
	}
	return agent.Instructions
}

// fetchSkillContext 获取代理的技能上下文，失败时返回空字符串。
func fetchSkillContext(client *Client, cfg *Config) string {
	skills, err := client.ListAgentSkills(context.Background(), cfg.Workspace.ID, cfg.Agent.ID)
	if err != nil {
		log.Printf("[context] failed to fetch skill context: %v", err)
		return ""
	}
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, s := range skills {
		if !s.Enabled {
			continue
		}
		sb.WriteString(fmt.Sprintf("### %s\n%s\n", s.Name, s.Description))
		if s.PromptTemplate != "" {
			sb.WriteString(s.PromptTemplate + "\n")
		}
	}
	return sb.String()
}

// fetchMCPContext 获取代理绑定的 MCP 服务器上下文，避免注入敏感 env 值。
func fetchMCPContext(client *Client, cfg *Config) string {
	servers, err := client.ListAgentMcpServers(context.Background(), cfg.Workspace.ID, cfg.Agent.ID)
	if err != nil {
		log.Printf("[context] failed to fetch mcp context: %v", err)
		return ""
	}
	if len(servers) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, server := range servers {
		if !server.Enabled {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString(fmt.Sprintf("### %s\n", server.Name))
		sb.WriteString(fmt.Sprintf("- URL: %s\n", server.URL))
		if server.Type != "" {
			sb.WriteString(fmt.Sprintf("- Type: %s\n", server.Type))
		}
		if server.Status != "" {
			sb.WriteString(fmt.Sprintf("- Status: %s\n", server.Status))
		}
	}
	return sb.String()
}
