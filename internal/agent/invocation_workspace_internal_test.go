// invocation_workspace_internal_test.go verifies the code_review workspace
// preparation against a real local Git remote: clone/fetch of the head ref,
// snapshot verification (head SHA match, base ancestry), detached checkout,
// previous-round cleanup, conversation path confinement, and conversation
// metadata persistence.
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// reviewTestRemote builds a bare remote with two commits: base on the default
// branch and head on feature/x, returning the remote path and both SHAs.
func reviewTestRemote(t *testing.T) (remote, baseSHA, headSHA string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	remotePath := filepath.Join(root, "remote.git")

	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t.local",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t.local")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	git(work, "init", "-b", "main")
	os.WriteFile(filepath.Join(work, "file.txt"), []byte("base content\n"), 0o644)
	git(work, "add", "-A")
	git(work, "commit", "-m", "base")
	baseSHA = git(work, "rev-parse", "HEAD")

	git(work, "checkout", "-b", "feature/x")
	os.WriteFile(filepath.Join(work, "feature.txt"), []byte("feature change\n"), 0o644)
	git(work, "add", "-A")
	git(work, "commit", "-m", "head")
	headSHA = git(work, "rev-parse", "HEAD")

	git(root, "clone", "--bare", work, remotePath)
	return remotePath, baseSHA, headSHA
}

func reviewTestManager(t *testing.T, client *Client, root string) *InvocationManager {
	t.Helper()
	cfg := &Config{
		Workspace: WorkspaceConfig{Root: root},
		Agent:     AgentInfo{Provider: "claude"},
	}
	return NewInvocationManager(cfg, client, NewExecutionGate(), "agent-1", "ws-1", "claude")
}

func reviewInvocation(conversation, remote, baseSHA, headSHA string) AgentInvocation {
	base := baseSHA
	head := headSHA
	project := "proj-1"
	return AgentInvocation{
		ID:             "inv-review",
		WorkspaceID:    "ws-1",
		CallerAgentID:  "caller-1",
		TargetAgentID:  "agent-1",
		Kind:           InvocationKindCodeReview,
		ProjectID:      &project,
		BaseSHA:        &base,
		HeadRef:        stringPtr("feature/x"),
		HeadSHA:        &head,
		Message:        "review the feature",
		ConversationID: conversation,
		Status:         "claimed",
		ExecutionID:    stringPtr("exec-1"),
	}
}

// stubReviewServer serves the project and credentials the preparation needs;
// the git remote is a local path, so no credential material is consulted.
func stubReviewServer(t *testing.T, repoURL string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/workspaces/ws-1/agents/agent-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instructions":"review identity","git_name":"T","git_email":"t@t.local"}`))
	})
	mux.HandleFunc("/api/workspaces/ws-1/projects/proj-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"demo","repo_url":"` + filepath.ToSlash(repoURL) + `"}`))
	})
	mux.HandleFunc("/api/projects/proj-1/git-credentials", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credentials":[{"agent_id":"agent-1","username":"u","pat":"p"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "td_token")
}

// TestReviewWorkspacePrepareAndReuse verifies the first round creates the
// conversation workspace at the pinned snapshot and a second round with a
// new head re-checks out inside the same directory.
func TestReviewWorkspacePrepareAndReuse(t *testing.T) {
	remote, baseSHA, headSHA := reviewTestRemote(t)
	root := t.TempDir()
	client := stubReviewServer(t, remote)
	manager := reviewTestManager(t, client, root)

	inv := reviewInvocation("conv-abc", remote, baseSHA, headSHA)
	dir, ws, err := manager.prepareWorkspace(context.Background(), inv)
	if err != nil {
		t.Fatalf("prepare first round: %v", err)
	}
	if ws == nil {
		t.Fatalf("code_review must return workspace state")
	}
	if !strings.HasSuffix(filepath.ToSlash(dir), "/ws-1/agent-1/reviews/conv-abc") {
		t.Fatalf("review dir %s is not {root}/{workspace}/{agent}/reviews/{conversation}", dir)
	}
	head, err := ws.git.HeadCommit()
	if err != nil || head != headSHA {
		t.Fatalf("checked-out head = %s (%v), want %s", head, err, headSHA)
	}
	if meta := loadConversationMeta(dir); meta == nil || meta.Provider != "claude" {
		t.Fatalf("conversation meta missing after first round: %+v", loadConversationMeta(dir))
	}

	// simulate a previous round's session + local dirt, then re-review with a
	// new head: the workspace must be cleaned and re-pinned, meta preserved
	meta := loadConversationMeta(dir)
	meta.SessionID = "provider-session-1"
	meta.LastResultText = "previous findings"
	saveConversationMeta(dir, meta)
	os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("leftover"), 0o644)

	// advance the remote: a new commit on feature/x
	newHead := advanceRemote(t, remote, headSHA)
	inv2 := reviewInvocation("conv-abc", remote, baseSHA, newHead)
	dir2, ws2, err := manager.prepareWorkspace(context.Background(), inv2)
	if err != nil {
		t.Fatalf("prepare second round: %v", err)
	}
	if dir2 != dir {
		t.Fatalf("second round must reuse the conversation workspace")
	}
	head2, err := ws2.git.HeadCommit()
	if err != nil || head2 != newHead {
		t.Fatalf("second-round head = %s (%v), want %s", head2, err, newHead)
	}
	if _, err := os.Stat(filepath.Join(dir2, "dirty.txt")); !os.IsNotExist(err) {
		t.Fatalf("previous round's untracked files must be wiped")
	}
	meta2 := loadConversationMeta(dir2)
	if meta2 == nil || meta2.SessionID != "provider-session-1" {
		t.Fatalf("session mapping must survive the second round: %+v", meta2)
	}
}

// TestReviewWorkspaceRejectsForgedConversation verifies the path confinement:
// a conversation id that escapes the reviews root is refused before any
// filesystem work happens.
func TestReviewWorkspaceRejectsForgedConversation(t *testing.T) {
	remote, baseSHA, headSHA := reviewTestRemote(t)
	root := t.TempDir()
	client := stubReviewServer(t, remote)
	manager := reviewTestManager(t, client, root)

	for _, forged := range []string{"..", "../evil", "a/b"} {
		inv := reviewInvocation(forged, remote, baseSHA, headSHA)
		if _, _, err := manager.prepareWorkspace(context.Background(), inv); err == nil {
			t.Fatalf("forged conversation %q must be rejected", forged)
		}
	}
}

// TestReviewWorkspaceRejectsForeignSnapshot verifies the snapshot integrity
// gates: a head SHA that does not match the fetched ref, and an unreachable
// base, both fail the preparation.
func TestReviewWorkspaceRejectsForeignSnapshot(t *testing.T) {
	remote, baseSHA, headSHA := reviewTestRemote(t)
	root := t.TempDir()
	client := stubReviewServer(t, remote)
	manager := reviewTestManager(t, client, root)

	foreign := strings.Repeat("a", 40)
	inv := reviewInvocation("conv-snap", remote, baseSHA, foreign)
	if _, _, err := manager.prepareWorkspace(context.Background(), inv); err == nil {
		t.Fatalf("head SHA mismatch must fail the preparation")
	}

	inv = reviewInvocation("conv-snap2", remote, foreign, headSHA)
	if _, _, err := manager.prepareWorkspace(context.Background(), inv); err == nil {
		t.Fatalf("unreachable base must fail the preparation")
	}
}

// advanceRemote pushes one more commit onto feature/x and returns its SHA.
func advanceRemote(t *testing.T, remote, parent string) string {
	t.Helper()
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t.local",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t.local")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "clone", "--branch", "feature/x", remote, "work")
	work := filepath.Join(root, "work")
	os.WriteFile(filepath.Join(work, "more.txt"), []byte("second round\n"), 0o644)
	git(work, "add", "-A")
	git(work, "commit", "-m", "second")
	git(work, "push", "origin", "feature/x")
	return git(work, "rev-parse", "HEAD")
}

func stringPtr(s string) *string { return &s }
