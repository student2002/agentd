package agent_test

import (
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func TestLogBufferAppendAndRecent(t *testing.T) {
	lb := agent.NewLogBuffer(3)
	lb.Append(agent.LogLine{TaskID: 1, NodeID: "n1", Line: "a"})
	lb.Append(agent.LogLine{TaskID: 1, NodeID: "n1", Line: "b"})
	lb.Append(agent.LogLine{TaskID: 1, NodeID: "n1", Line: "c"})
	lb.Append(agent.LogLine{TaskID: 1, NodeID: "n1", Line: "d"}) // evicts "a"

	recent := lb.Recent(10)
	if len(recent) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(recent))
	}
	if recent[0].Line != "b" || recent[2].Line != "d" {
		t.Fatalf("unexpected lines: %v", recent)
	}
	if recent[0].Seq >= recent[1].Seq || recent[1].Seq >= recent[2].Seq {
		t.Fatalf("seq not increasing: %v", recent)
	}
}

func TestLogBufferSince(t *testing.T) {
	lb := agent.NewLogBuffer(100)
	lb.Append(agent.LogLine{Line: "a"})
	lb.Append(agent.LogLine{Line: "b"})
	mid := lb.Recent(10)[1].Seq // seq of "b"; Since returns lines with Seq > mid
	lb.Append(agent.LogLine{Line: "c"})
	lb.Append(agent.LogLine{Line: "d"})

	got := lb.Since(mid)
	if len(got) != 2 {
		t.Fatalf("expected 2 lines after seq %d, got %d", mid, len(got))
	}
	if got[0].Line != "c" || got[1].Line != "d" {
		t.Fatalf("unexpected lines: %v", got)
	}
}

func TestLogBufferSubscribeReceivesAppends(t *testing.T) {
	lb := agent.NewLogBuffer(10)
	ch, unsubscribe := lb.Subscribe()
	defer unsubscribe()

	lb.Append(agent.LogLine{Line: "hello"})
	select {
	case line := <-ch:
		if line.Line != "hello" {
			t.Fatalf("expected hello, got %s", line.Line)
		}
	default:
		t.Fatal("did not receive appended line on subscribe channel")
	}
}

func TestLogBufferRecentEmpty(t *testing.T) {
	lb := agent.NewLogBuffer(10)
	if got := lb.Recent(5); len(got) != 0 {
		t.Fatalf("expected empty, got %d", len(got))
	}
	if got := lb.Since(0); len(got) != 0 {
		t.Fatalf("expected empty Since, got %d", len(got))
	}
}
