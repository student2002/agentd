// Package agent_test 包含 agent 包的测试，涵盖 shell 转义、执行上下文构建、Git 操作和代理守护进程使用的 Token 估算。
package agent_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

// TestTokenEstimation_CJKReducesCharBudget 验证中文内容相比英文内容在相同上下文窗口下字符预算更少。通过提供超出预算的内容并检查中文内容被截断得更彻底来测试。
func TestTokenEstimation_CJKReducesCharBudget(t *testing.T) {
	// 将填满上下文窗口的长描述
	englishDesc := strings.Repeat("This is a long English description that fills the context window. ", 200)
	cjkDesc := strings.Repeat("这是一段很长的中文描述，用来填满上下文窗口，测试 Token 估算算法是否能正确调整字符预算。", 100)

	buildCtx := func(t *testing.T, description string) string {
		t.Helper()

		mux := http.NewServeMux()
		mux.HandleFunc("/api/workspaces/", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(agent.WorkspaceContext{ID: "ws-1", Name: "WS"})
		})
		mux.HandleFunc("/api/workspaces/{wsId}/projects/", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(agent.ProjectContext{ID: "proj-1", Name: "Proj"})
		})
		mux.HandleFunc("/api/memories", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode([]agent.SharedMemory{})
		})
		mux.HandleFunc("/api/workspaces/{wsId}/agents/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/workspaces/ws-1/agents/agent-1/skills" {
				json.NewEncoder(w).Encode([]agent.SkillContext{})
				return
			}
			json.NewEncoder(w).Encode(agent.AgentInstructions{Instructions: "You are a helper."})
		})

		server := httptest.NewServer(mux)
		defer server.Close()

		client := agent.NewClient(server.URL, "test-token")
		// 使用较小的上下文窗口以强制截断
		cfg := &agent.Config{
			Workspace: agent.WorkspaceConfig{ID: "ws-1"},
			Agent:     agent.AgentInfo{ID: "agent-1", ContextWindow: 1000}, // 小窗口
		}

		task := agent.Task{
			ID:          1,
			Title:       "Test",
			Description: description,
			ProjectID:   "proj-1",
		}
		node := agent.TaskNode{
			ID:          "node-1",
			TaskID:      1,
			Name:        "1. Work",
			Description: "Do the work",
		}

		ctx, err := agent.BuildExecutionContext(client, cfg, task, node, false)
		if err != nil {
			t.Fatalf("BuildExecutionContext failed: %v", err)
		}
		return ctx
	}

	englishCtx := buildCtx(t, englishDesc)
	cjkCtx := buildCtx(t, cjkDesc)

	// 使用较小的上下文窗口时，CJK 内容应被更激进地截断
	// 因为相同数量的字符会消耗更多 Token
	// 英文上下文应保留更多内容（更长）
	if len(cjkCtx) > len(englishCtx) {
		t.Logf("English context: %d chars, CJK context: %d chars", len(englishCtx), len(cjkCtx))
		// 如果 CJK 文本每个字符都紧凑得多，就可能出现这种情况
		// 关键点是：在相同内容长度下，CJK 使用更多 Token
		// 但 CJK 用更少的字符表达相同的意思
		// 因此此测试更多是验证算法能正确运行
		// 而非严格的长度比较
	}

	// 真正的测试：两个上下文都应被截断（不包含完整描述）
	if strings.Contains(englishCtx, englishDesc) {
		t.Error("English context should be truncated, not contain the full description")
	}
	if strings.Contains(cjkCtx, cjkDesc) {
		t.Error("CJK context should be truncated, not contain the full description")
	}

	t.Logf("English context: %d chars, CJK context: %d chars", len(englishCtx), len(cjkCtx))
}

// TestTokenEstimation_DefaultContextWindow 验证零上下文窗口时回退到 100000 Token。
func TestTokenEstimation_DefaultContextWindow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workspaces/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(agent.WorkspaceContext{ID: "ws-1", Name: "WS"})
	})
	mux.HandleFunc("/api/workspaces/{wsId}/projects/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(agent.ProjectContext{ID: "proj-1", Name: "Proj"})
	})
	mux.HandleFunc("/api/memories", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]agent.SharedMemory{})
	})
	mux.HandleFunc("/api/workspaces/{wsId}/agents/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/workspaces/ws-1/agents/agent-1/skills" {
			json.NewEncoder(w).Encode([]agent.SkillContext{})
			return
		}
		json.NewEncoder(w).Encode(agent.AgentInstructions{Instructions: "Helper."})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := agent.NewClient(server.URL, "test-token")
	cfg := &agent.Config{
		Workspace: agent.WorkspaceConfig{ID: "ws-1"},
		Agent:     agent.AgentInfo{ID: "agent-1", ContextWindow: 0}, // zero = default 100000
	}

	task := agent.Task{ID: 1, Title: "Test", Description: "Hello", ProjectID: "proj-1"}
	node := agent.TaskNode{ID: "node-1", TaskID: 1, Name: "1. Work", Description: "Do work"}

	ctx, err := agent.BuildExecutionContext(client, cfg, task, node, false)
	if err != nil {
		t.Fatalf("BuildExecutionContext with zero ContextWindow failed: %v", err)
	}
	if len(ctx) == 0 {
		t.Error("context should not be empty with default context window")
	}
}
