// context.go implements the node execution context construction logic.
//
// This file is responsible for combining multiple information sources by
// priority into the execution prompt for coding tools, mainly including:
//   - ContextSection definition: a context section with priority and truncatable flag
//   - BuildExecutionContext: builds the full context by priority from high to low,
//     intelligently truncating when it exceeds 80% of the window
//   - Context sources: constraints/warnings, node description, task description,
//     prior node results, agent instructions, skill context, shared memory,
//     project context, workspace context
//   - estimateCharToTokenRatio: estimates the character-to-token conversion ratio
//     based on the Chinese/English ratio
//   - nullString compatibility: handles both server-side sql.NullString and plain
//     string JSON formats
//
// Context injection follows descending priority: the smaller the value, the
// higher the priority, and the more it is preserved when the window is insufficient.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// ContextSection represents a section of the execution context, injected into
// the coding tool's prompt after being sorted by priority.
// The smaller the priority value, the more important it is, and the more it is
// preserved when the context window is insufficient.
type ContextSection struct {
	Name           string
	Content        string
	Priority       int  // priority; the smaller the value, the higher the priority (less likely to be truncated)
	NonTruncatable bool // if true, this section is never truncated
}

// Task represents task information obtained from the API, used to build the
// execution context.
// It contains the task's basic information, description, constraints, and the
// IDs of the project and workspace it belongs to.
type Task struct {
	ID          int32  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Constraints string `json:"constraints"`
	ProjectID   string `json:"project_id"`
	WorkspaceID string `json:"workspace_id"`
}

// UnmarshalJSON performs custom JSON deserialization for Task, compatible with
// the sql.NullString format.
// The sql.NullString fields returned by the API have the form
// {"String":"...","Valid":true}, which standard string deserialization cannot
// handle.
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

// nullString is compatible with both plain string and sql.NullString JSON
// formats.
// The server may return either format; this type is used to handle them uniformly.
type nullString struct {
	Valid  bool
	StrVal string
}

// UnmarshalJSON implements custom JSON deserialization, first trying the plain
// string format and falling back to the sql.NullString format.
func (ns *nullString) UnmarshalJSON(data []byte) error {
	// Try the plain string format
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		ns.StrVal = s
		ns.Valid = s != ""
		return nil
	}
	// Try the sql.NullString format {"String":"...","Valid":true}
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

// String returns the valid value of nullString, or an empty string if invalid.
func (ns *nullString) String() string {
	if ns.Valid {
		return ns.StrVal
	}
	return ""
}

// WorkspaceContext represents workspace-level context information, containing
// ID, name, and description.
type WorkspaceContext struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ProjectContext represents project-level context information, containing ID,
// name, and description.
type ProjectContext struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	RepoURL     string `json:"repo_url"`
}

// SharedMemory represents a shared memory entry, used for cross-task knowledge
// transfer.
// It contains the memory ID, title, content, and relevance score.
type SharedMemory struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// AgentInstructions represents the agent's identity instructions, containing
// behavior guidance and Git identity information.
// The instructions are injected into the execution context to guide the agent's
// behavior.
type AgentInstructions struct {
	Instructions string `json:"instructions"`
	GitName      string `json:"git_name"`
	GitEmail     string `json:"git_email"`
}

// SkillContext represents skill information, used to inject into the execution
// context.
// It contains the skill's ID, name, description, and prompt template.
type SkillContext struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	PromptTemplate string `json:"prompt_template"`
	Enabled        bool   `json:"enabled"`
	Category       string `json:"category,omitempty"`
	AssignedAt     string `json:"assigned_at,omitempty"`
}

// UnmarshalJSON treats a missing enabled field as enabled, to be compatible with
// older API responses and lightweight test fixtures. An explicit enabled=false
// still disables the skill.
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

// estimateCharToTokenRatio estimates the character-to-token ratio based on the
// content language.
// Pure English is about 4 chars/token, pure Chinese is about 1.5 chars/token,
// and mixed content is linearly interpolated.
func estimateCharToTokenRatio(content string) float64 {
	cjkCount := 0
	totalRunes := 0
	for _, r := range content {
		totalRunes++
		if (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
			(r >= 0x3400 && r <= 0x4DBF) || // CJK Extension A
			(r >= 0x3040 && r <= 0x309F) || // Hiragana
			(r >= 0x30A0 && r <= 0x30FF) || // Katakana
			(r >= 0xAC00 && r <= 0xD7AF) { // Hangul Syllables
			cjkCount++
		}
	}
	if totalRunes == 0 {
		return 4.0
	}
	cjkRatio := float64(cjkCount) / float64(totalRunes)
	// Linear interpolation: pure English 4.0 chars/token, pure Chinese 1.5 chars/token
	return 4.0 - cjkRatio*(4.0-1.5)
}

// BuildExecutionContext builds the full execution context of a node in priority
// order.
// Priority (high to low): constraints/warnings -> node description ->
// task description -> agent instructions -> skill context -> shared memory ->
// project context -> workspace context.
// When it exceeds 80% of the context window, truncation starts from the lowest
// priority.
// When isResume is true, only essential sections are included, because --resume
// preserves the previous context.
func BuildExecutionContext(client *Client, cfg *Config, task Task, node TaskNode, isResume bool) (string, error) {
	return BuildExecutionContextWithCapabilities(client, cfg, task, node, isResume, PromptCapabilities{IncludeSkills: true, IncludeMCP: true})
}

func BuildExecutionContextWithCapabilities(client *Client, cfg *Config, task Task, node TaskNode, isResume bool, caps PromptCapabilities) (string, error) {
	sections := buildContextSections(client, cfg, task, node, isResume, caps)

	// Estimate the token count (roughly: 1 token ≈ 4 chars)
	maxTokens := cfg.Agent.ContextWindow
	if maxTokens <= 0 {
		maxTokens = 100000
	}
	contextLimit := int(float64(maxTokens) * 0.8)
	// Estimate the char/token ratio based on the content language
	var allContent strings.Builder
	for _, sec := range sections {
		allContent.WriteString(sec.Content)
	}
	charPerToken := estimateCharToTokenRatio(allContent.String())
	charLimit := int(float64(contextLimit) * charPerToken)

	var sb strings.Builder
	// Build from highest priority to lowest priority; stop when the limit is reached
	totalChars := 0
	truncated := false

	// First pass: include all non-truncatable sections (these sections must never be trimmed)
	for _, sec := range sections {
		if sec.Content == "" || !sec.NonTruncatable {
			continue
		}
		sectionText := formatSection(sec)
		sb.WriteString(sectionText)
		totalChars += len(sectionText)
	}

	// Second pass: add truncatable sections within the remaining budget
	for _, sec := range sections {
		if sec.Content == "" || sec.NonTruncatable {
			continue // already included in the first pass
		}

		sectionText := formatSection(sec)
		sectionLen := len(sectionText)

		if totalChars+sectionLen > charLimit {
			// Try to fit a truncated version
			remaining := charLimit - totalChars
			if remaining > 200 {
				// Include a truncated version
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

	// 1. Constraints/warnings (highest priority, non-truncatable: critical for safety)
	sections = append(sections, ContextSection{
		Name:           "Constraints & Warnings",
		Content:        task.Constraints,
		Priority:       1,
		NonTruncatable: true,
	})

	// 2. Directory permissions (safety constraint, non-truncatable; injected only
	// when the template node configures read-only/full-control directories)
	if dirCtx := buildDirectoryPermissions(node); dirCtx != "" {
		sections = append(sections, ContextSection{
			Name:           "Directory Permissions",
			Content:        dirCtx,
			Priority:       1,
			NonTruncatable: true,
		})
	}

	// 3. Node description (critical for understanding what to do)
	nodeDesc := node.Description
	if nodeDesc == "" {
		nodeDesc = node.Name
	}
	sections = append(sections, ContextSection{
		Name:     "Node Description",
		Content:  nodeDesc,
		Priority: 2,
	})

	// When resuming a session, --resume preserves the previous reasoning context.
	// Node comment context is still injected because user replies and upstream
	// handoffs are both delivered via comments.
	if isResume {
		commentCtx := fetchExecutionComments(client, task.ID, node.ID)
		sections = append(sections, ContextSection{
			Name:           "Node Comments",
			Content:        commentCtx,
			Priority:       4,
			NonTruncatable: true,
		})

		// 5. Agent instructions (non-truncatable: critical for agent identity)
		agentCtx := fetchAgentInstructions(client, cfg)
		sections = append(sections, ContextSection{
			Name:           "Agent Instructions",
			Content:        agentCtx,
			Priority:       5,
			NonTruncatable: true,
		})
		return sections
	}

	// 3. Task description (critical for understanding the overall goal)
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

	// 4. Agent instructions (non-truncatable: critical for agent identity)
	agentCtx := fetchAgentInstructions(client, cfg)
	sections = append(sections, ContextSection{
		Name:           "Agent Instructions",
		Content:        agentCtx,
		Priority:       5,
		NonTruncatable: true,
	})

	// 6. Skill context
	if caps.IncludeSkills {
		skillCtx := fetchSkillContext(client, cfg)
		sections = append(sections, ContextSection{
			Name:     "Skill Context",
			Content:  skillCtx,
			Priority: 6,
		})
	}

	// 7. MCP server context
	if caps.IncludeMCP {
		mcpCtx := fetchMCPContext(client, cfg)
		sections = append(sections, ContextSection{
			Name:     "MCP Servers",
			Content:  mcpCtx,
			Priority: 7,
		})
	}

	// 8. Shared memory (Top-K relevant)
	memCtx := fetchSharedMemory(client, task)
	sections = append(sections, ContextSection{
		Name:     "Shared Memory",
		Content:  memCtx,
		Priority: 8,
	})

	// 9. Project description
	projCtx := fetchProjectContext(client, cfg, task.ProjectID)
	sections = append(sections, ContextSection{
		Name:     "Project Context",
		Content:  projCtx,
		Priority: 9,
	})

	// 10. Workspace description (lowest priority, truncated first)
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

// buildDirectoryPermissions generates prompt content based on the node's
// configured directory permissions.
// The directory permissions come from the workflow template node's
// readonly_dirs / full_control_dirs (JSON arrays).
// It returns non-empty content only when at least one type of directory is
// configured; if both are empty it returns an empty string (not injected, to
// preserve zero regression).
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

// parseJSONStringArray parses a JSON array (e.g. ["/docs","/README.md"]) into a
// string slice.
// Empty values (nil / "null" / empty array) return nil.
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

// fetchExecutionComments retrieves the comment context required for executing
// the current node.
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

// fetchWorkspaceContext retrieves workspace-level context information,
// returning an empty string on failure.
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

// fetchProjectContext retrieves project-level context information, returning an
// empty string on failure.
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

// fetchSharedMemory retrieves the Top-K related shared memories, fetching only
// verified or high-confidence memories to prevent low-quality content pollution.
func fetchSharedMemory(client *Client, task Task) string {
	if task.WorkspaceID == "" {
		return ""
	}
	var memories []SharedMemory
	// Fetch only verified or high-confidence memories to prevent low-quality content pollution
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

// fetchAgentInstructions retrieves the agent's identity instructions,
// returning an empty string on failure.
func fetchAgentInstructions(client *Client, cfg *Config) string {
	var agent AgentInstructions
	err := client.doJSON(context.Background(), "GET", fmt.Sprintf("/api/workspaces/%s/agents/%s", cfg.Workspace.ID, cfg.Agent.ID), nil, &agent)
	if err != nil {
		log.Printf("[context] failed to fetch agent instructions: %v", err)
		return ""
	}
	return agent.Instructions
}

// fetchSkillContext retrieves the agent's skill context, returning an empty
// string on failure.
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

// fetchMCPContext retrieves the MCP server context bound to the agent, avoiding
// injecting sensitive env values.
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
