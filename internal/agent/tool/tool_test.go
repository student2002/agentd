package tool

import "testing"

// The same reply arrives multiple times in Claude's stream-json: as a
// complete assistant message and as the final result (and, in delta-capable
// streams, as text deltas first). The extractor must emit it exactly once.
func TestStreamJSONTextExtractorDeduplicatesAssistantAndResult(t *testing.T) {
	x := &streamJSONTextExtractor{}
	assistant := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello world"}]}}`
	result := `{"type":"result","result":"hello world"}`

	if got := x.extract(assistant); got != "hello world" {
		t.Fatalf("assistant text: got %q", got)
	}
	if got := x.extract(result); got != "" {
		t.Fatalf("result duplicated the assistant text: got %q", got)
	}
}

func TestStreamJSONTextExtractorPrefersDeltas(t *testing.T) {
	x := &streamJSONTextExtractor{}
	d1 := `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}`
	d2 := `{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}`
	assistant := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`
	result := `{"type":"result","result":"hello"}`

	if got := x.extract(d1); got != "hel" {
		t.Fatalf("delta 1: got %q", got)
	}
	if got := x.extract(d2); got != "lo" {
		t.Fatalf("delta 2: got %q", got)
	}
	if got := x.extract(assistant); got != "" {
		t.Fatalf("assistant duplicated the deltas: got %q", got)
	}
	if got := x.extract(result); got != "" {
		t.Fatalf("result duplicated the deltas: got %q", got)
	}
}

func TestStreamJSONTextExtractorResultFallback(t *testing.T) {
	x := &streamJSONTextExtractor{}
	result := `{"type":"result","result":"only source"}`

	if got := x.extract(result); got != "only source" {
		t.Fatalf("result fallback: got %q", got)
	}
}

func TestStreamJSONTextExtractorIgnoresSystemAndUnknown(t *testing.T) {
	x := &streamJSONTextExtractor{}
	if got := x.extract(`{"type":"system","subtype":"init","session_id":"s"}`); got != "" {
		t.Fatalf("system: got %q", got)
	}
	if got := x.extract(`{"type":"whatever"}`); got != "" {
		t.Fatalf("unknown: got %q", got)
	}
}
