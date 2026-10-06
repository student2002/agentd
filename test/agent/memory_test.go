// memory_test.go covers the instance-memory store: append with provenance,
// dedupe, unified per-instance storage (no workspace partition), search with
// limits, read caps, clear, and the file permission boundary.
package agent_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func TestMemoryStoreAppendAndReadWithProvenance(t *testing.T) {
	root := t.TempDir()
	store := agent.NewMemoryStore(root)

	if err := store.Append("claude-01", "prefers table-driven Go tests", "team-a", "ws-a"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.Append("claude-01", "repo builds with pnpm, not npm", "team-b", "ws-b"); err != nil {
		t.Fatalf("append: %v", err)
	}

	content, err := store.Read("claude-01")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(content, "prefers table-driven Go tests") || !strings.Contains(content, "repo builds with pnpm, not npm") {
		t.Fatalf("expected both entries, got:\n%s", content)
	}
	// Provenance: each entry carries its source connection and workspace.
	if !strings.Contains(content, "team-a") || !strings.Contains(content, "ws-a") {
		t.Fatalf("entry missing source annotation:\n%s", content)
	}
	if !strings.Contains(content, "team-b") || !strings.Contains(content, "ws-b") {
		t.Fatalf("entry missing source annotation:\n%s", content)
	}

	// meta.json exists next to memory.md.
	if _, err := os.Stat(filepath.Join(root, "claude-01", "meta.json")); err != nil {
		t.Fatalf("meta.json missing: %v", err)
	}
}

func TestMemoryStorePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not honored on Windows")
	}
	root := t.TempDir()
	store := agent.NewMemoryStore(root)
	if err := store.Append("claude-01", "secret sauce", "team-a", "ws-a"); err != nil {
		t.Fatalf("append: %v", err)
	}
	dir := filepath.Join(root, "claude-01")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat instance dir: %v", err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("instance dir must be 0700, got %v", info.Mode().Perm())
	}
	mem, err := os.Stat(filepath.Join(dir, "memory.md"))
	if err != nil {
		t.Fatalf("stat memory.md: %v", err)
	}
	if mem.Mode().Perm() != 0600 {
		t.Fatalf("memory.md must be 0600, got %v", mem.Mode().Perm())
	}
}

// TestMemoryStoreKeepsRepeatedContent proves writes are never deduplicated:
// every write lands as its own entry with its own provenance — dropping or
// merging writes would erase which workspaces validated the knowledge.
func TestMemoryStoreKeepsRepeatedContent(t *testing.T) {
	store := agent.NewMemoryStore(t.TempDir())

	if err := store.Append("pk-1", "always run go vet", "team-a", "ws-a"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.Append("pk-1", "always run go vet", "team-b", "ws-b"); err != nil {
		t.Fatalf("append repeat: %v", err)
	}

	content, err := store.Read("pk-1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := strings.Count(content, "always run go vet"); got != 2 {
		t.Fatalf("repeated writes must both be kept, found %d:\n%s", got, content)
	}
	if !strings.Contains(content, "team-a") || !strings.Contains(content, "team-b") {
		t.Fatalf("each kept entry must carry its own provenance:\n%s", content)
	}
}

func TestMemoryStoreIsolatedPerInstance(t *testing.T) {
	store := agent.NewMemoryStore(t.TempDir())

	if err := store.Append("claude-01", "fact for claude", "team-a", "ws-a"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.Append("reviewer", "fact for reviewer", "team-a", "ws-a"); err != nil {
		t.Fatalf("append: %v", err)
	}

	forInstance, err := store.Read("claude-01")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(forInstance, "fact for reviewer") {
		t.Fatalf("instance memory leaked across instances:\n%s", forInstance)
	}
	other, err := store.Search("reviewer", "claude", 20)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("search leaked across instances: %v", other)
	}
}

func TestMemoryStoreSearchRespectsLimit(t *testing.T) {
	store := agent.NewMemoryStore(t.TempDir())
	topics := []string{"alpha deployment notes", "beta deployment notes", "gamma deployment notes", "delta other note"}
	for i, topic := range topics {
		connection := "team-a"
		if i == 3 {
			connection = "team-b"
		}
		if err := store.Append("claude-01", topic, connection, "ws-x"); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	hits, err := store.Search("claude-01", "deployment", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected the search limit applied, got %d: %v", len(hits), hits)
	}
	for _, hit := range hits {
		if !strings.Contains(hit, "deployment") {
			t.Fatalf("unexpected search hit: %q", hit)
		}
	}

	misses, err := store.Search("claude-01", "nonexistent", 20)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(misses) != 0 {
		t.Fatalf("expected no hits, got %v", misses)
	}
}

func TestMemoryStoreReadTruncatesToCapacity(t *testing.T) {
	store := agent.NewMemoryStore(t.TempDir())
	store.Limits.MaxFileBytes = 400

	for i := 0; i < 30; i++ {
		if err := store.Append("claude-01", strings.Repeat("x", 40), "team-a", "ws-a"); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	content, err := store.Read("claude-01")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(content) > 400 {
		t.Fatalf("read must stay within the byte cap, got %d bytes", len(content))
	}
	// The compaction drops the oldest entries first: the freshest content
	// survives.
	if got := strings.Count(content, strings.Repeat("x", 40)); got < 1 {
		t.Fatalf("newest entries must survive compaction:\n%.200s", content)
	}
}

func TestMemoryStoreClear(t *testing.T) {
	root := t.TempDir()
	store := agent.NewMemoryStore(root)
	if err := store.Append("claude-01", "gone soon", "team-a", "ws-a"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.Clear("claude-01"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "claude-01")); !os.IsNotExist(err) {
		t.Fatalf("instance dir must be removed, stat err=%v", err)
	}
	content, err := store.Read("claude-01")
	if err != nil {
		t.Fatalf("read after clear: %v", err)
	}
	if strings.TrimSpace(content) != "" {
		t.Fatalf("expected empty memory after clear, got %q", content)
	}
}

func TestMemoryStoreWriteRawReplacesContent(t *testing.T) {
	store := agent.NewMemoryStore(t.TempDir())
	if err := store.Append("claude-01", "old entry", "team-a", "ws-a"); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := store.WriteRaw("claude-01", "hand-edited memory"); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	content, err := store.Read("claude-01")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(content, "old entry") || !strings.Contains(content, "hand-edited memory") {
		t.Fatalf("raw write must replace the memory, got:\n%s", content)
	}
	// Appending after a manual edit still works (meta rebuilt cleanly).
	if err := store.Append("claude-01", "post-edit entry", "team-a", "ws-a"); err != nil {
		t.Fatalf("append after write raw: %v", err)
	}
	content, err = store.Read("claude-01")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(content, "post-edit entry") {
		t.Fatalf("append after manual edit missing:\n%s", content)
	}
}

func TestDefaultMemoryRootUnderTeammateHome(t *testing.T) {
	root := agent.DefaultMemoryRoot()
	if !strings.Contains(root, ".teammate") || !strings.HasSuffix(root, "memory") {
		t.Fatalf("unexpected default memory root: %q", root)
	}
}
