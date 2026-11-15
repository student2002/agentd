// invocation_workspace.go owns the working directories and persistent
// conversation state of invocation execution: transient directories for chat,
// persistent review workspaces for code_review (one per conversation, pinned
// to the reviewed Git snapshot), and the local conversation metadata that
// lets Claude/AtomCode resume their real sessions across rounds.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// execGit builds a git command bound to ctx so cancellation (e.g. an
// invocation:cancelled event) kills an in-flight clone/fetch.
func execGit(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", args...)
}

// conversationRetention keeps review workspaces and their session mappings
// for 30 days, matching the server's invocation retention.
const conversationRetention = 30 * 24 * time.Hour

// invocationWorkspace carries the persistent state of one code_review
// execution; nil for chat kind.
type invocationWorkspace struct {
	dir     string // the review workspace (also the tool workDir)
	meta    *conversationMeta
	resumed bool        // this round actually resumed the provider session
	git     *GitManager // askpass-scoped manager; credential cleaned after prep
}

// conversationMeta is the local session mapping for one review conversation:
// provider, provider session reference, and the snapshot the last round
// reviewed. It never leaves this machine — the server stores no session ids.
type conversationMeta struct {
	Provider       string    `json:"provider"`
	SessionID      string    `json:"session_id,omitempty"`
	LastHeadSHA    string    `json:"last_head_sha,omitempty"`
	LastResultText string    `json:"last_result_text,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func conversationMetaPath(reviewDir string) string {
	return filepath.Join(reviewDir, ".teammate", "conversation.json")
}

func loadConversationMeta(reviewDir string) *conversationMeta {
	data, err := os.ReadFile(conversationMetaPath(reviewDir))
	if err != nil {
		return nil
	}
	var meta conversationMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil
	}
	return &meta
}

func saveConversationMeta(reviewDir string, meta *conversationMeta) {
	if meta == nil {
		return
	}
	meta.UpdatedAt = time.Now().UTC()
	path := conversationMetaPath(reviewDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("[invocation] create meta dir: %v", err)
		return
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		log.Printf("[invocation] marshal conversation meta: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Printf("[invocation] save conversation meta: %v", err)
	}
}

// prepareWorkspace returns the tool working directory for the invocation:
// a transient directory for chat, the conversation's persistent review
// workspace with the requested snapshot checked out for code_review.
func (m *InvocationManager) prepareWorkspace(ctx context.Context, inv AgentInvocation) (string, *invocationWorkspace, error) {
	if inv.Kind != InvocationKindCodeReview {
		dir, err := m.invocationTempDir(inv.ID)
		return dir, nil, err
	}
	return m.prepareReviewWorkspace(ctx, inv)
}

// prepareReviewWorkspace materializes the conversation's review workspace:
// clone on first use, then fetch the head ref with the target agent's own
// project credentials, verify the snapshot (head SHA matches, base is an
// ancestor), and check it out detached. Previous local modifications and
// untracked files from earlier rounds are wiped before the checkout.
func (m *InvocationManager) prepareReviewWorkspace(ctx context.Context, inv AgentInvocation) (string, *invocationWorkspace, error) {
	if inv.ProjectID == nil || inv.BaseSHA == nil || inv.HeadRef == nil || inv.HeadSHA == nil {
		return "", nil, fmt.Errorf("code_review invocation is missing project or snapshot fields")
	}
	projectID := *inv.ProjectID

	reviewDir, err := m.reviewWorkspaceDir(inv)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(reviewDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create review workspace: %w", err)
	}

	project, err := m.client.GetProject(ctx, m.agentID, m.workspaceID, projectID)
	if err != nil {
		return "", nil, fmt.Errorf("load project: %w", err)
	}
	if strings.TrimSpace(project.RepoURL) == "" {
		return "", nil, fmt.Errorf("project %s has no repository configured", projectID)
	}

	credentials, err := m.client.GetGitCredentials(ctx, m.agentID, projectID)
	if err != nil {
		return "", nil, fmt.Errorf("load git credentials: %w", err)
	}
	if len(credentials) == 0 {
		return "", nil, fmt.Errorf("no git credentials configured for project %s", projectID)
	}
	identity, err := fetchAgentInstructionsWithError(m.client, m.workspaceID, m.agentID)
	if err != nil {
		return "", nil, err
	}

	git := NewGitManager(reviewDir)
	// The askpass credential lives only for this preparation; the tool
	// process never sees it.
	if err := git.ConfigureCredential(credentials[0].Username, credentials[0].PAT, identity.GitName, identity.GitEmail); err != nil {
		return "", nil, err
	}
	defer git.CleanupCredential()

	meta := loadConversationMeta(reviewDir)

	// Clone on first use; Clone only ensures the remote when the repository
	// already exists.
	if err := git.CloneForReview(ctx, project.RepoURL); err != nil {
		return "", nil, fmt.Errorf("clone repository: %w", err)
	}

	// Wipe the previous round's local state before switching snapshots.
	if err := git.DiscardLocalChanges(ctx); err != nil {
		return "", nil, fmt.Errorf("reset review workspace: %w", err)
	}

	log.Printf("[invocation=%s] preparing repo: fetching %s", inv.ID, *inv.HeadRef)
	if err := git.FetchRef(ctx, *inv.HeadRef); err != nil {
		return "", nil, fmt.Errorf("fetch head ref %s: %w", *inv.HeadRef, err)
	}
	if err := git.VerifySnapshot(ctx, *inv.BaseSHA, *inv.HeadSHA); err != nil {
		return "", nil, err
	}
	if err := git.CheckoutDetached(ctx, *inv.HeadSHA); err != nil {
		return "", nil, fmt.Errorf("checkout snapshot %s: %w", *inv.HeadSHA, err)
	}

	resumed := false
	if meta == nil {
		meta = &conversationMeta{Provider: m.provider}
	} else {
		resumed = meta.Provider == m.provider && meta.SessionID != ""
	}
	meta.Provider = m.provider
	meta.LastHeadSHA = *inv.HeadSHA
	// Persist immediately: the mapping must exist even if the daemon
	// dies mid-round, so later rounds keep their resume reference.
	saveConversationMeta(reviewDir, meta)

	return reviewDir, &invocationWorkspace{dir: reviewDir, meta: meta, resumed: resumed, git: git}, nil
}

// reviewWorkspaceDir builds and validates the conversation's review path:
// {workspace_root}/{workspace}/{agent}/reviews/{conversation}. The absolute
// path must stay inside the reviews root so a forged conversation id cannot
// point the wipe/checkout machinery at an arbitrary directory.
func (m *InvocationManager) reviewWorkspaceDir(inv AgentInvocation) (string, error) {
	root := m.cfg.Workspace.Root
	if root == "" {
		root = filepath.Join(os.TempDir(), "teammate-invocations")
	}
	reviewsRoot := filepath.Join(root, m.workspaceID, m.agentID, "reviews")
	dir := filepath.Join(reviewsRoot, inv.ConversationID)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve review dir: %w", err)
	}
	absRoot, err := filepath.Abs(reviewsRoot)
	if err != nil {
		return "", fmt.Errorf("resolve reviews root: %w", err)
	}
	if absDir == absRoot || !strings.HasPrefix(absDir, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("conversation path escapes the reviews root")
	}
	if filepath.Base(absDir) != inv.ConversationID {
		return "", fmt.Errorf("conversation id must be a plain directory name")
	}
	return absDir, nil
}

// cleanupWorkspace removes the transient chat directory; the review workspace
// is kept for the conversation's next round.
func (m *InvocationManager) cleanupWorkspace(inv AgentInvocation, workDir string, ws *invocationWorkspace) {
	if ws == nil {
		if err := os.RemoveAll(workDir); err != nil {
			log.Printf("[invocation=%s] cleanup temp dir: %v", inv.ID, err)
		}
	}
}

// pruneExpiredConversations removes review workspaces and session mappings
// past the retention window. Called opportunistically from the scan loop.
func (m *InvocationManager) pruneExpiredConversations() {
	root := m.cfg.Workspace.Root
	if root == "" {
		return
	}
	reviewsRoot := filepath.Join(root, m.workspaceID, m.agentID, "reviews")
	entries, err := os.ReadDir(reviewsRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(reviewsRoot, entry.Name())
		meta := loadConversationMeta(dir)
		cutoff := time.Now().UTC().Add(-conversationRetention)
		stale := meta == nil || meta.UpdatedAt.Before(cutoff)
		if !stale {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("[invocation] prune conversation %s: %v", entry.Name(), err)
		}
	}
}

// --- review-scoped Git helpers on GitManager ---

// DiscardLocalChanges wipes previous rounds' modifications and untracked
// files so the next snapshot checkout starts clean.
func (g *GitManager) DiscardLocalChanges(ctx context.Context) error {
	if err := g.git(ctx, "reset", "--hard"); err != nil {
		return err
	}
	return g.git(ctx, "clean", "-fd")
}

// FetchRef validates the ref with git check-ref-format and fetches it into
// FETCH_HEAD.
func (g *GitManager) FetchRef(ctx context.Context, ref string) error {
	// --allow-onelevel: server-side validation accepts plain branch names
	// like "main"; without it check-ref-format rejects every one-level ref.
	if err := g.git(ctx, "check-ref-format", "--allow-onelevel", ref); err != nil {
		return fmt.Errorf("invalid head ref %q: %w", ref, err)
	}
	return g.git(ctx, "fetch", "--force", "origin", "--", ref)
}

// VerifySnapshot checks the fetched snapshot's integrity before any model
// sees it: the head commit exists and equals the requested SHA, and the base
// commit is reachable and an ancestor of the head.
func (g *GitManager) VerifySnapshot(ctx context.Context, baseSHA, headSHA string) error {
	fetched, err := g.RevParse(ctx, "FETCH_HEAD")
	if err != nil {
		return fmt.Errorf("resolve fetched ref: %w", err)
	}
	if !strings.EqualFold(fetched, headSHA) {
		return fmt.Errorf("head ref no longer points at %s (fetched %s); the caller must re-push and retry with a new key", headSHA, fetched)
	}
	if _, err := g.RevParse(ctx, baseSHA+"^{commit}"); err != nil {
		return fmt.Errorf("base commit %s is not reachable in the fetched history", baseSHA)
	}
	if err := g.git(ctx, "merge-base", "--is-ancestor", baseSHA, headSHA); err != nil {
		return fmt.Errorf("base commit %s is not an ancestor of head %s", baseSHA, headSHA)
	}
	return nil
}

// CheckoutDetached checks out the snapshot commit without a local branch.
func (g *GitManager) CheckoutDetached(ctx context.Context, sha string) error {
	return g.git(ctx, "checkout", "--detach", sha)
}

// RevParse resolves one revision to its commit SHA.
func (g *GitManager) RevParse(ctx context.Context, rev string) (string, error) {
	cmd := execGit(ctx, "rev-parse", rev)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *GitManager) git(ctx context.Context, args ...string) error {
	cmd := execGit(ctx, args...)
	cmd.Dir = g.workDir
	cmd.Env = append(os.Environ(), g.env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, DesensitizeLog(string(out)))
	}
	return nil
}
