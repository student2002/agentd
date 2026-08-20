package agent_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/teammate/agentd/internal/agent"
)

func TestSessionStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := agent.NewSessionStore(dir)

	if _, err := store.Load(); err == nil {
		t.Fatal("expected error loading absent session, got nil")
	}

	if err := store.Save("sess-123", "claude"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SessionID != "sess-123" || got.Tool != "claude" {
		t.Fatalf("unexpected session %+v", got)
	}

	if err := store.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("expected error after Delete, got nil")
	}

	info, _ := os.Stat(filepath.Join(dir, ".teammate-session"))
	if info != nil {
		t.Fatal("session file should not exist after Delete")
	}
}

func TestSessionStoreFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0600 file modes are a POSIX concept; Windows has no per-file owner/group/other permission bits")
	}
	dir := t.TempDir()
	store := agent.NewSessionStore(dir)
	if err := store.Save("sess", "claude"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, ".teammate-session"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("expected 0600, got %v", info.Mode().Perm())
	}
}
