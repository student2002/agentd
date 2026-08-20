// session_store.go persists the Claude Code session id to disk so an agentd
// restart can resume the same session for an in-progress node. The file is
// JSON, mode 0600, located at {workDir}/.teammate-session.
//
// Data source / permission boundary: the workDir is per-{agent,workspace,
// project,task}; the file holds only the session id + tool name. It degrades
// to "no session" (Load returns an error, callers treat resumption as off) on
// any read/parse failure. It cannot leak cross-workspace data because it is
// scoped to one task's workDir.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const sessionFileName = ".teammate-session"

// PersistedSession is the on-disk shape of the session pointer.
type PersistedSession struct {
	SessionID string    `json:"session_id"`
	Tool      string    `json:"tool"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SessionStore reads and writes the session pointer file inside a workDir.
type SessionStore struct {
	dir string
}

func NewSessionStore(workDir string) *SessionStore {
	return &SessionStore{dir: workDir}
}

func (s *SessionStore) path() string { return filepath.Join(s.dir, sessionFileName) }

func (s *SessionStore) Save(sessionID, tool string) error {
	if sessionID == "" {
		return nil
	}
	doc := PersistedSession{
		SessionID: sessionID,
		Tool:      tool,
		UpdatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	if err := os.WriteFile(s.path(), data, 0600); err != nil {
		return fmt.Errorf("write session file: %w", err)
	}
	return nil
}

func (s *SessionStore) Load() (*PersistedSession, error) {
	data, err := os.ReadFile(s.path())
	if err != nil {
		return nil, fmt.Errorf("read session file: %w", err)
	}
	var doc PersistedSession
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse session file: %w", err)
	}
	if doc.SessionID == "" {
		return nil, fmt.Errorf("session file has empty session_id")
	}
	return &doc, nil
}

func (s *SessionStore) Delete() error {
	err := os.Remove(s.path())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove session file: %w", err)
	}
	return nil
}
