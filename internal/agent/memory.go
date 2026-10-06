// memory.go implements the per-instance memory store on the local machine.
//
// Each agent instance (identified by its instance name — the same name
// delivered by several workspaces is one brain) owns one unified memory
// directory under the memory root. Storage is NOT partitioned by workspace:
// the memory belongs to the instance persona and evolves across workspaces;
// each entry only carries a provenance annotation (source connection and
// workspace). The memory never leaves the machine — nothing here talks to the
// server. Files are 0600 inside 0700 directories.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Environment variables carrying one execution's memory context into the
// `teammate-agentd mcp` subprocess. The executor injects them into the
// workDir MCP config entry; the MCP server reads them to attribute reads and
// writes without any identity arguments.
const (
	MCPCtxInstance    = "TEAMMATE_MCP_INSTANCE"
	MCPCtxPersona     = "TEAMMATE_MCP_PERSONA"
	MCPCtxConnection  = "TEAMMATE_MCP_CONNECTION"
	MCPCtxWorkspaceID = "TEAMMATE_MCP_WORKSPACE_ID"
	MCPCtxAgentID     = "TEAMMATE_MCP_AGENT_ID"
	MCPCtxServerURL   = "TEAMMATE_MCP_SERVER_URL"
	MCPCtxToken       = "TEAMMATE_MCP_TOKEN"
)

// MemoryLimits bounds one instance's memory. The defaults are adjustable
// tuning values, not contract.
type MemoryLimits struct {
	MaxFileBytes int // memory.md size cap; compaction drops oldest entries
	MaxLines     int // memory.md line cap
	SearchLimit  int // max entries returned by a search
}

func DefaultMemoryLimits() MemoryLimits {
	return MemoryLimits{MaxFileBytes: 64 * 1024, MaxLines: 2000, SearchLimit: 20}
}

// MemoryEntryMeta is one stored entry's provenance record in meta.json.
type MemoryEntryMeta struct {
	At          time.Time `json:"at"`
	Connection  string    `json:"connection"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
}

type memoryMeta struct {
	Entries []MemoryEntryMeta `json:"entries"`
}

// MemoryStore manages the per-instance memory directories under one root.
type MemoryStore struct {
	root   string
	Limits MemoryLimits
}

// DefaultMemoryRoot returns the machine's memory root: ~/.teammate/memory.
func DefaultMemoryRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".teammate", "memory")
	}
	return filepath.Join(home, ".teammate", "memory")
}

// NewMemoryStore creates a store rooted at the given directory (defaults to
// DefaultMemoryRoot when empty).
func NewMemoryStore(root string) *MemoryStore {
	if root == "" {
		root = DefaultMemoryRoot()
	}
	return &MemoryStore{root: root, Limits: DefaultMemoryLimits()}
}

var unsafeInstanceChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// instanceDir maps an instance name to its memory directory. Instance names
// arrive from server delivery; unsafe path characters are folded to "_".
func (s *MemoryStore) instanceDir(instance string) string {
	name := unsafeInstanceChars.ReplaceAllString(strings.TrimSpace(instance), "_")
	if name == "" {
		name = "_"
	}
	return filepath.Join(s.root, name)
}

func (s *MemoryStore) memoryPath(instance string) string {
	return filepath.Join(s.instanceDir(instance), "memory.md")
}

func (s *MemoryStore) metaPath(instance string) string {
	return filepath.Join(s.instanceDir(instance), "meta.json")
}

// Append stores one memory entry for the archive key, annotated with its
// source connection and workspace. Writes are never deduplicated: every entry
// is a (provenance, content) record. When the file exceeds the size or line
// cap the oldest entries are dropped.
func (s *MemoryStore) Append(key, content, connection, workspaceID string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("memory content is empty")
	}
	dir := s.instanceDir(key)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}

	header := fmt.Sprintf("## [%s] %s (%s)", time.Now().UTC().Format(time.RFC3339), connection, workspaceID)
	block := header + "\n" + content + "\n\n"
	f, err := os.OpenFile(s.memoryPath(key), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open memory file: %w", err)
	}
	if _, err := f.WriteString(block); err != nil {
		f.Close()
		return fmt.Errorf("append memory: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("append memory: %w", err)
	}

	meta, err := s.loadMeta(key)
	if err != nil {
		return err
	}
	meta.Entries = append(meta.Entries, MemoryEntryMeta{
		At:          time.Now().UTC(),
		Connection:  connection,
		WorkspaceID: workspaceID,
	})
	if err := s.saveMeta(key, meta); err != nil {
		return err
	}
	return s.compact(key, meta)
}

// Read returns the instance's full memory, truncated head-first when it
// exceeds the size or line cap.
func (s *MemoryStore) Read(instance string) (string, error) {
	data, err := os.ReadFile(s.memoryPath(instance))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read memory: %w", err)
	}
	return truncateHeadFirst(string(data), s.Limits.MaxFileBytes, s.Limits.MaxLines), nil
}

// Search returns the entries whose text contains the keyword
// (case-insensitive), newest first, up to the search limit.
func (s *MemoryStore) Search(instance, keyword string, limit int) ([]string, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, fmt.Errorf("search keyword is empty")
	}
	if limit <= 0 || limit > s.Limits.SearchLimit {
		limit = s.Limits.SearchLimit
	}
	content, err := s.Read(instance)
	if err != nil {
		return nil, err
	}
	entries := splitMemoryEntries(content)
	needle := strings.ToLower(keyword)
	hits := make([]string, 0, limit)
	for i := len(entries) - 1; i >= 0 && len(hits) < limit; i-- {
		if strings.Contains(strings.ToLower(entries[i]), needle) {
			hits = append(hits, strings.TrimSpace(entries[i]))
		}
	}
	return hits, nil
}

// Clear removes the instance's memory directory.
func (s *MemoryStore) Clear(instance string) error {
	if err := os.RemoveAll(s.instanceDir(instance)); err != nil {
		return fmt.Errorf("clear memory: %w", err)
	}
	return nil
}

// WriteRaw replaces the instance's memory with hand-edited content (the local
// console edit path). The provenance meta is reset: hand edits have no
// automated source annotation.
func (s *MemoryStore) WriteRaw(instance, content string) error {
	dir := s.instanceDir(instance)
	if strings.TrimSpace(content) == "" {
		return s.Clear(instance)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	if err := os.WriteFile(s.memoryPath(instance), []byte(strings.TrimRight(strings.TrimLeft(content, "\n"), "\n")+"\n"), 0600); err != nil {
		return fmt.Errorf("write memory: %w", err)
	}
	meta := memoryMeta{Entries: []MemoryEntryMeta{{At: time.Now().UTC(), Connection: "manual"}}}
	return s.saveMeta(instance, meta)
}

func (s *MemoryStore) loadMeta(instance string) (memoryMeta, error) {
	meta := memoryMeta{Entries: []MemoryEntryMeta{}}
	data, err := os.ReadFile(s.metaPath(instance))
	if err != nil {
		if os.IsNotExist(err) {
			return meta, nil
		}
		return meta, fmt.Errorf("read memory meta: %w", err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		// A corrupted meta never blocks writing memory; it is rebuilt.
		return memoryMeta{Entries: []MemoryEntryMeta{}}, nil
	}
	if meta.Entries == nil {
		meta.Entries = []MemoryEntryMeta{}
	}
	return meta, nil
}

func (s *MemoryStore) saveMeta(instance string, meta memoryMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal memory meta: %w", err)
	}
	if err := os.WriteFile(s.metaPath(instance), data, 0600); err != nil {
		return fmt.Errorf("write memory meta: %w", err)
	}
	return nil
}

// compact drops the oldest entries until the memory file fits both caps and
// rewrites the meta to match.
func (s *MemoryStore) compact(instance string, meta memoryMeta) error {
	data, err := os.ReadFile(s.memoryPath(instance))
	if err != nil {
		return fmt.Errorf("read memory for compaction: %w", err)
	}
	if fitsCaps(string(data), s.Limits.MaxFileBytes, s.Limits.MaxLines) {
		return nil
	}
	entries := splitMemoryEntries(string(data))
	kept := entries
	for len(kept) > 0 {
		body := strings.Join(kept, "") + "\n"
		if fitsCaps(body, s.Limits.MaxFileBytes, s.Limits.MaxLines) {
			break
		}
		kept = kept[1:]
	}
	dropped := len(entries) - len(kept)
	if dropped > 0 && dropped <= len(meta.Entries) {
		meta.Entries = meta.Entries[dropped:]
	}
	body := ""
	if len(kept) > 0 {
		body = strings.Join(kept, "") + "\n"
	}
	if err := os.WriteFile(s.memoryPath(instance), []byte(body), 0600); err != nil {
		return fmt.Errorf("compact memory: %w", err)
	}
	return s.saveMeta(instance, meta)
}

// splitMemoryEntries splits memory content into raw entry blocks on the
// provenance headers. Free text before the first header belongs to the first
// block.
func splitMemoryEntries(content string) []string {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	var entries []string
	current := make([]string, 0, 8)
	for _, line := range lines {
		if strings.HasPrefix(line, "## [") {
			if len(entries) > 0 || len(current) > 0 {
				entries = append(entries, strings.Join(current, "\n"))
			}
			current = current[:0]
		}
		current = append(current, line)
	}
	entries = append(entries, strings.Join(current, "\n"))
	// Drop trailing empties produced by the blank-line separation.
	out := entries[:0]
	for _, entry := range entries {
		if strings.TrimSpace(entry) != "" {
			out = append(out, entry)
		}
	}
	return out
}

func fitsCaps(content string, maxBytes, maxLines int) bool {
	if maxBytes > 0 && len(content) > maxBytes {
		return false
	}
	if maxLines > 0 && strings.Count(content, "\n") > maxLines {
		return false
	}
	return true
}

// truncateHeadFirst keeps the head of the content (oldest entries) within the
// caps.
func truncateHeadFirst(content string, maxBytes, maxLines int) string {
	if fitsCaps(content, maxBytes, maxLines) {
		return content
	}
	lines := strings.Split(content, "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	content = strings.Join(lines, "\n")
	if maxBytes > 0 && len(content) > maxBytes {
		content = content[:maxBytes]
	}
	return content
}
