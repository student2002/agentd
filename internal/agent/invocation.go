// invocation.go implements the InvocationManager: the worker that claims
// cross-machine agent invocations targeted at this agent and executes them
// through the shared coding-tool adapters. It sits beside TaskExecutor —
// both acquire the same per-instance ExecutionGate, keeping the "one agent
// instance executes serially" semantics across nodes and invocations.
//
// Lifecycle per invocation: acquire slot → server claim (ticket) → repo
// preparation (code_review) → server start (must succeed before the tool
// process is spawned) → lease renewal + bounded progress → tool run with a
// minimized environment → terminal receipt (complete/fail/ack-cancel) →
// cleanup → slot release. A running invocation is never re-executed after a
// daemon restart: its result is unknowable, the server expires it at lease
// end, and the caller retries with a new key.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/teammate/agentd/internal/agent/tool"
)

// Invocation tuning constants. The lease is 5 minutes server-side; renewal
// runs at half the interval so a couple of failed renewals still stay alive.
// Scan/renew periods are variables so tests can shorten them.
var (
	invocationScanInterval     = 30 * time.Second
	invocationLeaseRenewPeriod = 2 * time.Minute
)

const (
	invocationProgressPeriod   = 2 * time.Second
	invocationProgressMaxBytes = 4096
	invocationHoldGrace        = 30 * time.Second
)

// InvocationManager executes invocations targeted at one agent instance.
type InvocationManager struct {
	cfg         *Config
	client      *Client
	gate        *ExecutionGate
	toolFactory func(provider, path string) tool.Tool
	agentID     string
	workspaceID string
	provider    string

	scanCh   chan struct{}
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	scanInterval     time.Duration
	leaseRenewPeriod time.Duration
	holdGrace        time.Duration

	mu     sync.Mutex
	active *activeInvocation
}

// activeInvocation is the single in-flight invocation this manager owns.
type activeInvocation struct {
	invocationID    string
	executionID     string
	cancel          context.CancelFunc
	t               tool.Tool
	cancelRequested bool
	started         bool
}

// NewInvocationManager creates the manager for one (connection, instance)
// identity. toolFactory may be nil until SetToolFactory injects the shared
// factory; invocations fail with a stable error code while it is missing.
func NewInvocationManager(cfg *Config, client *Client, gate *ExecutionGate, agentID, workspaceID, provider string) *InvocationManager {
	return &InvocationManager{
		cfg:              cfg,
		client:           client,
		gate:             gate,
		agentID:          agentID,
		workspaceID:      workspaceID,
		provider:         provider,
		scanCh:           make(chan struct{}, 1),
		stopCh:           make(chan struct{}),
		scanInterval:     invocationScanInterval,
		leaseRenewPeriod: invocationLeaseRenewPeriod,
		holdGrace:        invocationHoldGrace,
	}
}

// SetTimingForTest overrides the scan, lease-renewal and lost-lease hold
// periods; call it before Start.
func (m *InvocationManager) SetTimingForTest(scan, renew, hold time.Duration) {
	m.scanInterval = scan
	m.leaseRenewPeriod = renew
	m.holdGrace = hold
}

// SetToolFactory injects the shared coding-tool factory (supervisor seam).
func (m *InvocationManager) SetToolFactory(factory func(provider, path string) tool.Tool) {
	m.mu.Lock()
	m.toolFactory = factory
	m.mu.Unlock()
}

// Start launches the scan loop and runs the startup reconciliation.
func (m *InvocationManager) Start() {
	m.wg.Add(1)
	go m.run()
}

// Stop terminates the manager: the scan loop exits and the active tool run is
// cancelled (its receipt path decides between ack-cancel and completion).
func (m *InvocationManager) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
	m.mu.Lock()
	act := m.active
	t := act.t
	m.mu.Unlock()
	if act != nil {
		act.cancel()
		if t != nil {
			_ = t.Stop()
		}
	}
	m.wg.Wait()
}

// TriggerScan wakes the scan loop (SSE invocation:ready, slot release).
func (m *InvocationManager) TriggerScan() {
	select {
	case m.scanCh <- struct{}{}:
	default:
	}
}

// HandleCancelRequested stops the active tool run for a cancel_requested
// invocation; the receipt path then confirms via ack-cancel.
func (m *InvocationManager) HandleCancelRequested(invocationID, executionID string) {
	m.mu.Lock()
	act := m.active
	m.mu.Unlock()
	if act == nil || act.invocationID != invocationID {
		return
	}
	if executionID != "" && act.executionID != executionID {
		return // stale event for an earlier ticket
	}
	m.mu.Lock()
	act.cancelRequested = true
	t := act.t
	m.mu.Unlock()
	log.Printf("[invocation=%s] cancel requested, stopping tool process", invocationID)
	act.cancel()
	if t != nil {
		_ = t.Stop()
	}
}

// HandleCancelled aborts a claimed-but-not-started invocation whose server
// state moved to cancelled (repo preparation releases the slot).
func (m *InvocationManager) HandleCancelled(invocationID string) {
	m.mu.Lock()
	act := m.active
	m.mu.Unlock()
	if act == nil || act.invocationID != invocationID {
		return
	}
	m.mu.Lock()
	wasStarted := act.started
	if !wasStarted {
		act.cancelRequested = true
	}
	t := act.t
	m.mu.Unlock()
	if wasStarted {
		return // running calls are handled via cancel_requested, not cancelled
	}
	log.Printf("[invocation=%s] cancelled while preparing, aborting", invocationID)
	act.cancel()
	if t != nil {
		_ = t.Stop()
	}
}

func (m *InvocationManager) run() {
	defer m.wg.Done()
	m.reconcileInFlight()

	ticker := time.NewTicker(m.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-m.scanCh:
		case <-ticker.C:
			// Periodic housekeeping rides on the fallback tick.
			m.pruneExpiredConversations()
		}
		m.claimNext()
	}
}

// runCtx derives a context that dies with the manager.
func (m *InvocationManager) runCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-m.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// claimNext discovers claimable work and, when the shared slot is free,
// claims and executes the oldest invocation. Blocking on Acquire queues the
// invocation behind a running node execution (nodes go first).
func (m *InvocationManager) claimNext() {
	ctx, cancel := m.runCtx()
	defer cancel()

	invocations, err := m.client.ListClaimableInvocations(ctx, m.agentID)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[invocation] failed to list claimable: %v", err)
		}
		return
	}
	if len(invocations) == 0 {
		return
	}

	release, err := m.gate.Acquire(ctx)
	if err != nil {
		return // stopping
	}

	// The slot is held before the server claim, so a claimed invocation never
	// queues behind another local execution; a 409 here means another
	// claimant won or the deadline passed and the scan will find new work.
	claimed, err := m.client.ClaimInvocation(ctx, m.agentID, invocations[0].ID)
	if err != nil {
		release()
		if !IsConflict(err) && ctx.Err() == nil {
			log.Printf("[invocation] claim rejected: %v", err)
		}
		return
	}
	m.execute(ctx, claimed, release)
}

// execute drives one claimed invocation to a terminal receipt.
func (m *InvocationManager) execute(parent context.Context, inv AgentInvocation, release func()) {
	defer release()
	if inv.ExecutionID == nil || *inv.ExecutionID == "" {
		log.Printf("[invocation=%s] claim returned no execution ticket, skipping", inv.ID)
		return
	}
	executionID := *inv.ExecutionID

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Register the active slot before any preparation so cancel events can
	// abort the whole flow, including claimed-not-started repo preparation.
	act := &activeInvocation{
		invocationID: inv.ID,
		executionID:  executionID,
		cancel:       cancel,
	}
	m.mu.Lock()
	m.active = act
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.active = nil
		m.mu.Unlock()
	}()

	// Lease renewal keeps the server from expiring long executions; a
	// renewal rejection (stale ticket, expired) means the server has moved
	// on, so the local run is cancelled.
	renewStop := make(chan struct{})
	defer close(renewStop)
	go m.renewLoop(ctx, renewStop, inv.ID, executionID)

	workDir, review, err := m.prepareWorkspace(ctx, inv)
	if err != nil {
		m.mu.Lock()
		cancelRequested := act.cancelRequested
		m.mu.Unlock()
		if ctx.Err() != nil || cancelRequested {
			m.abortUnstarted(act, inv, executionID)
			return
		}
		log.Printf("[invocation=%s] workspace preparation failed: %v", inv.ID, err)
		m.fail(ctx, inv, executionID, "workspace_prepare_failed", DesensitizeLog(err.Error()))
		return
	}
	// Every exit from here cleans the transient directory (chat); the review
	// workspace is deliberately kept for the conversation's next round.
	defer m.cleanupWorkspace(inv, workDir, review)

	if _, err := m.client.StartInvocation(context.Background(), m.agentID, inv.ID, executionID); err != nil {
		if IsConflict(err) {
			// cancelled or expired while claimed/preparing: the server already
			// reached a terminal state, no receipt is needed.
			m.abortUnstarted(act, inv, executionID)
			return
		}
		if ctx.Err() != nil {
			m.abortUnstarted(act, inv, executionID)
			return
		}
		m.fail(ctx, inv, executionID, "start_failed", DesensitizeLog(err.Error()))
		return
	}
	m.mu.Lock()
	act.started = true
	m.mu.Unlock()

	m.reportProgress(inv.ID, executionID, "analyzing", "")

	prompt, err := m.buildPrompt(ctx, inv, review)
	if err != nil {
		m.mu.Lock()
		cancelRequested := act.cancelRequested
		m.mu.Unlock()
		if ctx.Err() != nil || cancelRequested {
			m.stopAndAck(ctx, inv, executionID)
			return
		}
		m.fail(ctx, inv, executionID, "context_build_failed", DesensitizeLog(err.Error()))
		return
	}

	toolPath := ToolPathFor(m.cfg.Tools, m.provider)
	m.mu.Lock()
	factory := m.toolFactory
	m.mu.Unlock()
	var t tool.Tool
	if factory != nil {
		// Test/override seam: the supervisor may inject a shared factory.
		t = factory(m.provider, toolPath)
	} else {
		// Production selection: the same adapter the task executor picks.
		t = tool.GetTool(m.provider, toolPath)
	}
	if !t.IsInstalled() {
		m.fail(ctx, inv, executionID, "tool_unavailable", "provider "+m.provider+" is not installed")
		return
	}
	// act.t is read by the cancel paths (SSE events, lease loss, Stop) — the
	// write stays inside m.mu so those readers never race the swap.
	m.mu.Lock()
	act.t = t
	m.mu.Unlock()

	applyInvocationSessionResume(m.provider, t, review)

	var lastProgress time.Time
	var progressMu sync.Mutex
	result, execErr := t.Execute(ctx, workDir, prompt, tool.ExecuteOptions{Env: BuildMinimalEnv()}, func(line string) {
		progressMu.Lock()
		if time.Since(lastProgress) < invocationProgressPeriod {
			progressMu.Unlock()
			return
		}
		lastProgress = time.Now()
		progressMu.Unlock()
		m.reportProgress(inv.ID, executionID, "working", truncateRunes(line, invocationProgressMaxBytes))
	})

	m.mu.Lock()
	cancelRequested := act.cancelRequested
	m.mu.Unlock()

	if cancelRequested {
		m.stopAndAck(ctx, inv, executionID)
	} else if execErr != nil {
		m.fail(ctx, inv, executionID, "tool_failed", DesensitizeLog(execErr.Error()))
	} else if result.ExitCode != 0 {
		m.fail(ctx, inv, executionID, "tool_exit_nonzero", fmt.Sprintf("tool exited with code %d", result.ExitCode))
	} else {
		m.complete(ctx, inv, executionID, result, review)
	}

}

// renewLoop extends the claim lease until the run finishes. A definitive
// rejection (unknown ticket, conflict, auth) or three consecutive transient
// failures means the server no longer acknowledges this execution: the local
// run is cancelled so it cannot keep producing side effects whose receipts
// would only be rejected.
func (m *InvocationManager) renewLoop(ctx context.Context, stop <-chan struct{}, invocationID, executionID string) {
	ticker := time.NewTicker(m.leaseRenewPeriod)
	defer ticker.Stop()
	transientFailures := 0
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		_, err := m.client.RenewInvocationLease(context.Background(), m.agentID, invocationID, executionID)
		if err == nil {
			transientFailures = 0
			continue
		}
		if !leaseRejectionTransient(err) {
			log.Printf("[invocation=%s] lease revoked by server (%v), stopping local run", invocationID, err)
			m.HandleCancelRequested(invocationID, executionID)
			return
		}
		transientFailures++
		log.Printf("[invocation=%s] lease renewal failed (%d/3): %v", invocationID, transientFailures, err)
		if transientFailures >= 3 {
			log.Printf("[invocation=%s] lease renewal keeps failing, stopping local run", invocationID)
			m.HandleCancelRequested(invocationID, executionID)
			return
		}
	}
}

// leaseRejectionTransient reports whether a renewal error may recover on its
// own (transport failure, server restart, overload): only those are retried.
// A reply the server itself produced that rejects the ticket is final.
func leaseRejectionTransient(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.StatusCode >= 500 || apiErr.StatusCode == 429
}

// abortUnstarted releases a claimed invocation that never reached running:
// either the server already cancelled it (no receipt needed) or the local
// flow was stopped before start (ack-cancel records the cancelled state).
func (m *InvocationManager) abortUnstarted(act *activeInvocation, inv AgentInvocation, executionID string) {
	m.mu.Lock()
	cancelRequested := act.cancelRequested
	started := act.started
	m.mu.Unlock()
	if cancelRequested && !started {
		if _, err := m.client.AckCancelInvocation(context.Background(), m.agentID, inv.ID, executionID); err != nil && !IsConflict(err) {
			log.Printf("[invocation=%s] ack-cancel failed: %v", inv.ID, err)
		}
	}
}

// stopAndAck records the worker's confirmation of a cancel_requested stop;
// a 409 means the run actually finished first and the completion CAS won.
func (m *InvocationManager) stopAndAck(ctx context.Context, inv AgentInvocation, executionID string) {
	_, err := m.client.AckCancelInvocation(context.Background(), m.agentID, inv.ID, executionID)
	if err != nil && !IsConflict(err) {
		log.Printf("[invocation=%s] ack-cancel failed: %v", inv.ID, err)
	}
}

func (m *InvocationManager) fail(ctx context.Context, inv AgentInvocation, executionID, code, message string) {
	_, err := m.client.FailInvocation(context.Background(), m.agentID, inv.ID, executionID, code, message)
	if err != nil {
		log.Printf("[invocation=%s] fail receipt rejected: %v", inv.ID, err)
	}
}

// complete records the final answer: the adapter's FinalText when it can
// identify one, otherwise the sanitized display transcript.
func (m *InvocationManager) complete(ctx context.Context, inv AgentInvocation, executionID string, result *tool.ExecutionResult, review *invocationWorkspace) {
	finalText := strings.TrimSpace(result.FinalText)
	if finalText == "" {
		finalText = strings.TrimSpace(result.DisplayText)
	}
	if finalText == "" {
		// A successful run with no extractable text is still a success with
		// an empty answer; never fall back to the raw protocol output.
		finalText = ""
	}
	metadata := map[string]interface{}{}
	if review != nil {
		if review.resumed {
			metadata["resumed"] = true
		}
		if result.SessionID != "" && review.meta != nil {
			review.meta.SessionID = result.SessionID
		}
	}
	if _, err := m.client.CompleteInvocation(context.Background(), m.agentID, inv.ID, executionID, finalText, metadata); err != nil {
		log.Printf("[invocation=%s] complete receipt rejected: %v", inv.ID, err)
		return
	}
	if review != nil && review.meta != nil {
		review.meta.LastResultText = finalText
		saveConversationMeta(review.dir, review.meta)
	}
}

func (m *InvocationManager) reportProgress(invocationID, executionID, stage, message string) {
	if _, err := m.client.UpdateInvocationProgress(context.Background(), m.agentID, invocationID, executionID, stage, message); err != nil {
		log.Printf("[invocation=%s] progress rejected: %v", invocationID, err)
	}
}

// reconcileInFlight runs at manager start: claimed-not-started invocations
// resume with their original ticket; running/cancel_requested invocations are
// never re-executed (their result is unknowable) — the slot stays occupied
// until their lease lapses so no new work starts on top of the unknown state.
func (m *InvocationManager) reconcileInFlight() {
	ctx, cancel := m.runCtx()
	defer cancel()

	invocations, err := m.client.ListInFlightInvocations(ctx, m.agentID)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[invocation] in-flight reconciliation failed: %v", err)
		}
		return
	}
	// Claimed invocations resume first: the original ticket's lease is still
	// running out, so the resume must win the slot before the holds below
	// claim it for their expiry wait.
	for _, inv := range invocations {
		if inv.Status != InvocationStatusClaimed {
			continue
		}
		if inv.ExecutionID == nil || *inv.ExecutionID == "" {
			continue
		}
		log.Printf("[invocation=%s] resuming claimed invocation with original ticket", inv.ID)
		release, err := m.gate.Acquire(ctx)
		if err != nil {
			return
		}
		m.execute(ctx, inv, release)
	}
	// Lost running invocations are never re-executed (their result is
	// unknowable): the slot stays occupied until the lease lapses so no new
	// work starts on top of the unknown state.
	for _, inv := range invocations {
		if inv.Status != InvocationStatusRunning && inv.Status != InvocationStatusCancelRequested {
			continue
		}
		hold := m.holdGrace
		if inv.LeaseUntil != nil {
			remaining := time.Until(*inv.LeaseUntil)
			if remaining > hold {
				hold = remaining
			}
		}
		log.Printf("[invocation=%s] found a lost running invocation, holding the slot for %s without re-executing", inv.ID, hold)
		go func(leaseID string, hold time.Duration) {
			// The hold outlives the reconciliation pass (which returns
			// immediately), so it gets its own manager-lifetime context.
			holdCtx, cancelHold := m.runCtx()
			defer cancelHold()
			release, err := m.gate.Acquire(holdCtx)
			if err != nil {
				return
			}
			select {
			case <-time.After(hold):
			case <-holdCtx.Done():
			case <-m.stopCh:
			}
			release()
			m.TriggerScan()
		}(inv.ID, hold)
	}
}

func truncateRunes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for len(cut) > 0 && !isUTF8Boundary(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func isUTF8Boundary(s string) bool {
	// utf8.ValidString is true for the truncated prefix only when the cut
	// lands on a rune boundary.
	return utf8.ValidString(s)
}

// invocationTempDir is the transient working directory of a chat invocation.
// The id must be a single path segment (server-generated UUID); anything else
// is rejected so the mkdir cannot be steered out of the root.
func (m *InvocationManager) invocationTempDir(invocationID string) (string, error) {
	if invocationID == "" || strings.ContainsAny(invocationID, "/\\") || invocationID == "." || invocationID == ".." {
		return "", fmt.Errorf("invalid invocation id %q", invocationID)
	}
	root := m.cfg.Workspace.Root
	if root == "" {
		root = filepath.Join(os.TempDir(), "teammate-invocations")
	}
	dir := filepath.Join(root, "invocations", invocationID)
	_ = os.MkdirAll(dir, 0o755)
	return dir, nil
}
