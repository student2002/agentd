// Package agent_test contains tests for the agent package, covering shell escaping,
// execution context building, Git operations, and token estimation used by the agent daemon.
package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestBuildContext_AllAPIPathsVerified verifies that all 5 context-fetching
// functions call the correct API paths and successfully parse the responses.
func TestBuildContext_AllAPIPathsVerified(t *testing.T) {
	// record which API paths were called
	calledPaths := make(map[string]bool)

	// create a mock HTTP server simulating the backend API
	mux := http.NewServeMux()

	// 1. GET /api/workspaces/{wsId}
	mux.HandleFunc("/api/workspaces/", func(w http.ResponseWriter, r *http.Request) {
		calledPaths["GET /api/workspaces/{wsId}"] = true
		// validate the path format: /api/workspaces/{wsId}
		if r.Method != "GET" {
			t.Errorf("workspace: expected GET, got %s", r.Method)
		}
		json.NewEncoder(w).Encode(agent.WorkspaceContext{
			ID:          "ws-1",
			Name:        "Test Workspace",
			Description: "A test workspace for context injection",
		})
	})

	// 2. GET /api/workspaces/{wsId}/projects/{projectId}
	mux.HandleFunc("/api/workspaces/{wsId}/projects/", func(w http.ResponseWriter, r *http.Request) {
		calledPaths["GET /api/workspaces/{wsId}/projects/{projectId}"] = true
		if r.Method != "GET" {
			t.Errorf("project: expected GET, got %s", r.Method)
		}
		json.NewEncoder(w).Encode(agent.ProjectContext{
			ID:          "proj-1",
			Name:        "Test Project",
			Description: "A test project for context injection",
		})
	})

	// 3. GET /api/memories
	mux.HandleFunc("/api/memories", func(w http.ResponseWriter, r *http.Request) {
		calledPaths["GET /api/memories"] = true
		if r.Method != "GET" {
			t.Errorf("memories: expected GET, got %s", r.Method)
		}
		json.NewEncoder(w).Encode([]agent.SharedMemory{
			{ID: "mem-1", Title: "Architecture Decision", Content: "Use Go for backend", Score: 0.9},
		})
	})

	// 5. GET /api/workspaces/{wsId}/agents/{agentId}/skills
	mux.HandleFunc("/api/workspaces/{wsId}/agents/", func(w http.ResponseWriter, r *http.Request) {
		// this handler matches both the agent info and the skills path
		// check whether it is a skills subpath
		if r.URL.Path == "/api/workspaces/ws-1/agents/agent-1/skills" {
			calledPaths["GET /api/workspaces/{wsId}/agents/{agentId}/skills"] = true
			if r.Method != "GET" {
				t.Errorf("skills: expected GET, got %s", r.Method)
			}
			json.NewEncoder(w).Encode([]agent.SkillContext{
				{ID: "skill-1", Name: "Code Review", Description: "Review code for best practices", PromptTemplate: "Focus on error handling", Enabled: true},
			})
			return
		}
		if r.URL.Path == "/api/workspaces/ws-1/agents/agent-1/execution/mcp-servers" {
			calledPaths["GET /api/workspaces/{wsId}/agents/{agentId}/execution/mcp-servers"] = true
			if r.Method != "GET" {
				t.Errorf("mcp servers: expected GET, got %s", r.Method)
			}
			json.NewEncoder(w).Encode([]agent.AgentMcpServerContext{
				{ID: "mcp-1", Name: "Docs MCP", URL: "https://mcp.example.test/sse", Type: "sse", Status: "connected", Enabled: true},
			})
			return
		}
		// agent info path
		calledPaths["GET /api/workspaces/{wsId}/agents/{agentId}"] = true
		if r.Method != "GET" {
			t.Errorf("agent: expected GET, got %s", r.Method)
		}
		json.NewEncoder(w).Encode(agent.AgentInstructions{
			Instructions: "You are a senior Go developer. Write clean, idiomatic code.",
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	// create a client pointing to the mock server
	client := agent.NewClient(server.URL, "test-token")

	cfg := &agent.Config{
		Workspace: agent.WorkspaceConfig{ID: "ws-1"},
		Agent:     agent.AgentInfo{ID: "agent-1"},
	}

	task := agent.Task{
		ID:          1,
		Title:       "Implement user authentication",
		Description: "Add JWT-based authentication to the API",
		Constraints: "Must follow OWASP guidelines",
		ProjectID:   "proj-1",
		WorkspaceID: "ws-1",
	}

	node := agent.TaskNode{
		ID:          "node-1",
		TaskID:      1,
		Name:        "1. AI Processing",
		Description: "Implement the core authentication logic",
	}

	// build the execution context
	context, err := agent.BuildExecutionContext(client, cfg, task, node, false)
	if err != nil {
		t.Fatalf("BuildExecutionContext failed: %v", err)
	}

	// verify that all 5 API paths were called
	expectedPaths := []string{
		"GET /api/workspaces/{wsId}",
		"GET /api/workspaces/{wsId}/projects/{projectId}",
		"GET /api/memories",
		"GET /api/workspaces/{wsId}/agents/{agentId}",
		"GET /api/workspaces/{wsId}/agents/{agentId}/skills",
		"GET /api/workspaces/{wsId}/agents/{agentId}/execution/mcp-servers",
	}
	for _, path := range expectedPaths {
		if !calledPaths[path] {
			t.Errorf("API path not called: %s", path)
		}
	}

	// verify that the context contains all expected sections
	expectedSections := []string{
		"Workspace Context",
		"Project Context",
		"Shared Memory",
		"Agent Instructions",
		"Skill Context",
		"MCP Servers",
		"Node Description",
		"Task Description",
		"Constraints & Warnings",
	}
	for _, section := range expectedSections {
		if !containsSection(context, section) {
			t.Errorf("context missing section: %s", section)
		}
	}

	// verify specific content from the mock responses
	expectedContents := []string{
		"Test Workspace",
		"A test workspace for context injection",
		"Test Project",
		"A test project for context injection",
		"Architecture Decision",
		"Use Go for backend",
		"You are a senior Go developer",
		"Code Review",
		"Docs MCP",
		"https://mcp.example.test/sse",
		"Implement the core authentication logic",
		"Implement user authentication",
		"Add JWT-based authentication to the API",
		"Must follow OWASP guidelines",
	}
	for _, content := range expectedContents {
		if !containsContent(context, content) {
			t.Errorf("context missing expected content: %q", content)
		}
	}

	// verify that the context is non-empty and not too short (previously only about 187 characters)
	if len(context) < 200 {
		t.Errorf("context is too short (%d chars), expected at least 200", len(context))
	}

	t.Logf("Context length: %d chars", len(context))
	t.Logf("Called API paths: %v", calledPaths)
}

// TestBuildContext_NullStringDeserialization verifies that Task UnmarshalJSON correctly handles
// both plain strings and the sql.NullString format.
func TestBuildContext_NullStringDeserialization(t *testing.T) {
	tests := []struct {
		name     string
		jsonData string
		wantDesc string
		wantCons string
	}{
		{
			name:     "plain string format",
			jsonData: `{"id":1,"title":"Test","description":"hello","constraints":"world","project_id":"p-1"}`,
			wantDesc: "hello",
			wantCons: "world",
		},
		{
			name:     "empty string format",
			jsonData: `{"id":1,"title":"Test","description":"","constraints":"","project_id":"p-1"}`,
			wantDesc: "",
			wantCons: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var task agent.Task
			if err := json.Unmarshal([]byte(tt.jsonData), &task); err != nil {
				t.Fatalf("UnmarshalJSON failed: %v", err)
			}
			if task.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", task.Description, tt.wantDesc)
			}
			if task.Constraints != tt.wantCons {
				t.Errorf("Constraints = %q, want %q", task.Constraints, tt.wantCons)
			}
		})
	}
}

// TestBuildContext_GetTaskAPIPath verifies that GetTask uses the correct API path prefixed with the project ID.
func TestBuildContext_GetTaskAPIPath(t *testing.T) {
	var requestPath string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/projects/", func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		// return a task with sql.NullString fields
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":          1,
			"title":       "Test Task",
			"description": map[string]interface{}{"String": "Task description", "Valid": true},
			"constraints": map[string]interface{}{"String": "Task constraints", "Valid": true},
			"project_id":  "proj-1",
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := agent.NewClient(server.URL, "test-token")

	task, err := client.GetTask(context.Background(), "proj-1", 1)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}

	// verify that the correct API path was called
	expectedPath := "/api/projects/proj-1/tasks/1"
	if requestPath != expectedPath {
		t.Errorf("GetTask called path %q, want %q", requestPath, expectedPath)
	}

	// verify that the task was parsed correctly (including the sql.NullString fields)
	if task.Title != "Test Task" {
		t.Errorf("Task.Title = %q, want %q", task.Title, "Test Task")
	}
	if task.Description != "Task description" {
		t.Errorf("Task.Description = %q, want %q", task.Description, "Task description")
	}
	if task.Constraints != "Task constraints" {
		t.Errorf("Task.Constraints = %q, want %q", task.Constraints, "Task constraints")
	}
}

// TestBuildContext_SSEEventContainsProjectID verifies that the SSE event payload contains the project_id field required by the daemon.
func TestBuildContext_SSEEventContainsProjectID(t *testing.T) {
	// model the SSE event payload format received by the daemon
	payload := map[string]interface{}{
		"task_id":    1,
		"node_id":    "node-uuid-1",
		"project_id": "proj-1",
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal payload: %v", err)
	}

	// parse it the same way daemon.go does
	var parsed struct {
		TaskID    int32  `json:"task_id"`
		NodeID    string `json:"node_id"`
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal payload: %v", err)
	}

	if parsed.ProjectID != "proj-1" {
		t.Errorf("project_id = %q, want %q", parsed.ProjectID, "proj-1")
	}
	if parsed.TaskID != 1 {
		t.Errorf("task_id = %d, want %d", parsed.TaskID, 1)
	}
	if parsed.NodeID != "node-uuid-1" {
		t.Errorf("node_id = %q, want %q", parsed.NodeID, "node-uuid-1")
	}
}

// TestBuildContext_DirectoryPermissions verifies that the node's directory permissions (readonly/full_control)
// are injected into the execution context as a Directory Permissions section; when both are empty no section
// is injected (zero regression).
func TestBuildContext_DirectoryPermissions(t *testing.T) {
	// minimal mock: all fetch endpoints return empty data, focusing on the Directory Permissions section
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := agent.NewClient(server.URL, "test-token")
	cfg := &agent.Config{
		Agent: agent.AgentInfo{ContextWindow: 100000},
	}
	task := agent.Task{ID: 1, Title: "Test Task", ProjectID: "proj-1"}

	t.Run("with directory permissions", func(t *testing.T) {
		node := agent.TaskNode{
			ID:              "node-1",
			Name:            "code",
			ReadonlyDirs:    json.RawMessage(`["/docs","/README.md"]`),
			FullControlDirs: json.RawMessage(`["/src"]`),
		}
		ctx, err := agent.BuildExecutionContext(client, cfg, task, node, false)
		if err != nil {
			t.Fatalf("BuildExecutionContext: %v", err)
		}
		if !containsSection(ctx, "Directory Permissions") {
			t.Errorf("expected Directory Permissions section in context")
		}
		if !containsContent(ctx, "Read-only directories") || !containsContent(ctx, "/docs") {
			t.Errorf("expected readonly dirs (/docs) in context, got:\n%s", ctx)
		}
		if !containsContent(ctx, "Full-control directories") || !containsContent(ctx, "/src") {
			t.Errorf("expected full-control dirs (/src) in context, got:\n%s", ctx)
		}
	})

	t.Run("without directory permissions", func(t *testing.T) {
		node := agent.TaskNode{ID: "node-1", Name: "code"}
		ctx, err := agent.BuildExecutionContext(client, cfg, task, node, false)
		if err != nil {
			t.Fatalf("BuildExecutionContext: %v", err)
		}
		if containsSection(ctx, "Directory Permissions") {
			t.Errorf("expected NO Directory Permissions section when dirs are empty, got:\n%s", ctx)
		}
	})
}

func containsSection(context, sectionName string) bool {
	return containsContent(context, fmt.Sprintf("## %s", sectionName))
}

func containsContent(context, content string) bool {
	return len(context) > 0 && contains(context, content)
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
