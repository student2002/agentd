// comment_dedupe_test.go 覆盖评论去重逻辑的测试。
package agent_test

import (
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func TestHasDuplicateAgentComment(t *testing.T) {
	comments := []agent.Comment{
		{AuthorType: "agent", AuthorID: "agent-1", CommentType: "question", Content: "What is OpenClaw?"},
		{AuthorType: "member", AuthorID: "member-1", CommentType: "question", Content: "What is OpenClaw?"},
	}

	if !agent.HasDuplicateAgentComment(comments, "agent-1", "question", " What is OpenClaw? ") {
		t.Fatal("expected duplicate agent question comment")
	}
	if agent.HasDuplicateAgentComment(comments, "agent-2", "question", "What is OpenClaw?") {
		t.Fatal("different agent should not be considered duplicate")
	}
	if agent.HasDuplicateAgentComment(comments, "agent-1", "handoff", "What is OpenClaw?") {
		t.Fatal("different comment type should not be considered duplicate")
	}
}
