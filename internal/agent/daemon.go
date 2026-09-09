// Package agent provides the core functionality of the AI agent daemon.
//
// This package implements the full lifecycle of the Agent Daemon, including:
//   - runtime registration and heartbeat maintenance
//   - SSE event listening and response
//   - node claiming and task execution
//   - Git operations and credential management
//   - context construction and tool invocation
//   - RSA encrypted communication
//
// Daemon is the main entry point of the daemon, coordinating the work of all
// submodules.
// TaskExecutor is responsible for the actual task execution flow.
// SSEClient is responsible for real-time communication with the Server.
// Heartbeat is responsible for periodically reporting liveness to the Server.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Daemon manages the full lifecycle of the agent daemon.
//
// Daemon is the main coordinator of the daemon, responsible for:
//   - generating an RSA key pair for credential decryption
//   - registering a runtime with the Server and obtaining a Session Token
//   - starting the SSE client to listen for events
//   - starting the heartbeat to maintain online status
//   - starting the node watcher to poll for claimable tasks
//   - gracefully handling shutdown signals
type Daemon struct {
	cfg           *Config
	client        *Client
	hb            *Heartbeat
	watcher       *NodeWatcher
	exec          *TaskExecutor
	sseClient     *SSEClient
	localState    *LocalStateStore
	localEvents   *LocalEventHub
	localServer   *LocalServer
	profile       string
	configPath    string
	runtimeID     string
	privateKeyPEM string
	publicKeyPEM  string
	stopCh        chan struct{}
	stopOnce      sync.Once
}

type DaemonOptions struct {
	Profile    string
	ConfigPath string
}

// NewDaemon creates a new agent daemon instance.
//
// Parameters:
//   - cfg: daemon configuration, containing server address, Agent ID, workspace ID, etc.
//
// Returns:
//   - *Daemon: the initialized daemon instance
//   - error: returned on creation failure
func NewDaemon(cfg *Config) (*Daemon, error) {
	return NewDaemonWithOptions(cfg, DaemonOptions{})
}

func NewDaemonWithOptions(cfg *Config, opts DaemonOptions) (*Daemon, error) {
	client := NewClient(cfg.Server.URL, cfg.Server.APIToken)
	localState := NewLocalStateStore(LocalStateConfig{
		InstanceID:  cfg.Local.InstanceID,
		Profile:     opts.Profile,
		ServerURL:   cfg.Server.URL,
		WorkspaceID: cfg.Workspace.ID,
		AgentID:     cfg.Agent.ID,
		AgentName:   cfg.Agent.Name,
		Provider:    cfg.Agent.Provider,
	})
	localEvents := NewLocalEventHub()
	logBuffer := NewLogBuffer(2000)
	exec := NewTaskExecutorWithObserver(cfg, client, cfg.Agent.ID, &localExecutionObserver{
		state: localState,
		hub:   localEvents,
	})
	exec.SetLogBuffer(logBuffer)
	var localServer *LocalServer
	if cfg.Local.Enabled {
		localServer = NewLocalServer(LocalServerConfig{
			BindAddr:   cfg.Local.BindAddr,
			LocalToken: cfg.Local.LocalToken,
			Version:    AgentdVersion,
			Executor:   exec,
			LogBuffer:  logBuffer,
		}, localState, localEvents)
	}

	return &Daemon{
		cfg:         cfg,
		client:      client,
		exec:        exec,
		localState:  localState,
		localEvents: localEvents,
		localServer: localServer,
		profile:     opts.Profile,
		configPath:  opts.ConfigPath,
		stopCh:      make(chan struct{}),
	}, nil
}

type localExecutionObserver struct {
	state *LocalStateStore
	hub   *LocalEventHub
}

func (o *localExecutionObserver) OnExecutionStarted(session LocalExecutionSession) {
	o.state.SetExecutionStarted(session)
	o.hub.PublishSnapshot(LocalEventExecutionStarted, o.state.Snapshot())
}

func (o *localExecutionObserver) OnExecutionCompleted(taskID int32, nodeID string) {
	o.state.SetExecutionCompleted(taskID, nodeID)
	o.hub.PublishSnapshot(LocalEventExecutionCompleted, o.state.Snapshot())
}

func (o *localExecutionObserver) OnExecutionInterrupted(taskID int32, nodeID string) {
	o.state.SetExecutionInterrupted(taskID, nodeID)
	o.hub.PublishSnapshot(LocalEventExecutionInterrupted, o.state.Snapshot())
}

func (o *localExecutionObserver) OnExecutionFailed(taskID int32, nodeID string, err error) {
	o.state.SetExecutionFailed(taskID, nodeID, err)
	o.hub.PublishSnapshot(LocalEventExecutionFailed, o.state.Snapshot())
}

func (o *localExecutionObserver) OnToolStatusChanged(provider string, status string, err error) {
	o.state.SetToolStatus(provider, status, err)
	o.hub.PublishSnapshot(LocalEventToolStatusChanged, o.state.Snapshot())
}

func (d *Daemon) LocalSnapshot() LocalSnapshot {
	return d.localState.Snapshot()
}

// Stop notifies the daemon to shut down gracefully.
//
// After this method is called, all submodules stop after completing their
// current operations.
// The heartbeat stops, the SSE connection closes, and the task executor is
// interrupted.
func (d *Daemon) Stop() {
	d.stopOnce.Do(func() {
		close(d.stopCh)
	})
}

// Run starts the daemon and blocks until a shutdown signal is received.
//
// Startup flow:
//  1. Generate an RSA key pair (for Git credential decryption)
//  2. Register a runtime with the Server to obtain the runtimeID
//  3. Exchange the API Token for a Session Token (7-day validity)
//  4. Start the Session Token auto-refresh (5 minutes before expiry)
//  5. Start the SSE client to listen for real-time events
//  6. Start the heartbeat (every 30 seconds)
//  7. Start the node watcher (polling every 60 seconds, as an SSE fallback)
//  8. Trigger an initial full sync
//  9. Wait for a SIGINT/SIGTERM signal
//
// Error handling:
//   - RSA key generation failure: continue running; Git credentials unavailable
//   - Runtime registration failure: continue running; use the API Token
//   - Session Token exchange failure: continue using the API Token
//   - SSE startup failure: continue running; rely on REST polling
//
// Returns:
//   - error: returned when the daemon exits abnormally
func (d *Daemon) Run() error {
	log.Printf("[daemon] starting with agent=%s workspace=%s", d.cfg.Agent.ID, d.cfg.Workspace.ID)
	if d.cfg.Local.Enabled {
		if d.cfg.Local.LocalToken == "" {
			err := fmt.Errorf("local.local_token is required when local.enabled=true")
			d.localState.SetRuntimeError("local_token_missing", err.Error())
			return err
		}
		if d.cfg.Local.InstanceID == "" {
			err := fmt.Errorf("local.instance_id is required when local.enabled=true")
			d.localState.SetRuntimeError("local_instance_missing", err.Error())
			return err
		}
		if err := d.localServer.Start(); err != nil {
			d.localState.SetRuntimeError("local_server_start_failed", err.Error())
			return err
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := d.localServer.Stop(ctx); err != nil {
				log.Printf("[daemon] WARNING: failed to stop local control API: %v", err)
			}
		}()
		log.Printf("[daemon] local control API started on %s", d.cfg.Local.BindAddr)
	}

	// 0. Generate an RSA key pair for credential decryption
	publicKeyPEM, privateKeyPEM, err := GenerateRSAKeyPair()
	if err != nil {
		log.Printf("[daemon] WARNING: failed to generate RSA key pair: %v", err)
		log.Printf("[daemon] Continuing without RSA key pair — git credentials will not be available")
	} else {
		d.privateKeyPEM = privateKeyPEM
		d.publicKeyPEM = publicKeyPEM
		d.client.PrivateKeyPEM = privateKeyPEM
		log.Printf("[daemon] RSA key pair generated for credential decryption")
	}

	// 1. Register a runtime with the server and start all online components
	// (Session Token, SSE, heartbeat).
	// On registration failure, startup is not blocked: retryRuntimeRegistration
	// retries every 30 seconds; once the Server recovers it automatically fills
	// in the components without needing to restart the daemon.
	var retryWG sync.WaitGroup
	if err := d.ensureRuntimeComponents(); err != nil {
		log.Printf("[daemon] WARNING: failed to register runtime: %v", err)
		d.localState.SetRuntimeError("runtime_register_failed", err.Error())
		d.localEvents.PublishSnapshot("runtime.error", d.localState.Snapshot())
		log.Printf("[daemon] Continuing without runtime registration — will retry every %s", runtimeRegisterRetryInterval)
		retryWG.Add(1)
		go func() {
			defer retryWG.Done()
			d.retryRuntimeRegistration()
		}()
	}

	// 5. Start the NodeWatcher as a fallback (60-second REST polling)
	// The watcher actively polls only when SSE is disconnected
	d.watcher = NewNodeWatcher(d.client, d.exec, d.cfg.Agent.ID, d.cfg.Workspace.ID, 60*time.Second)
	if d.localServer != nil {
		d.localServer.SetWatcher(d.watcher)
		d.localServer.SetExecutor(d.exec)
	}
	d.watcher.Start()
	log.Printf("[daemon] node watcher started (60s interval, fallback when SSE disconnected)")

	// 6. Trigger an immediate full sync after all components have started.
	// This ensures the daemon can discover pending nodes created before runtime
	// registration (e.g. the user created a task before starting the daemon, or
	// an SSE event was missed during startup).
	go d.watcher.TriggerPoll()
	log.Printf("[daemon] triggered initial full sync")

	// 7. Wait for a shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("[daemon] received signal %v, shutting down...", sig)
	case <-d.stopCh:
		log.Printf("[daemon] stop requested, shutting down...")
	}

	// 7. Graceful shutdown
	// First wait for the registration retry goroutine to exit, to avoid it
	// concurrently reading/writing the SSE/heartbeat components alongside the
	// shutdown logic
	retryWG.Wait()
	if d.sseClient != nil {
		d.sseClient.Stop()
		log.Printf("[daemon] SSE client stopped")
	}
	d.watcher.Stop()
	log.Printf("[daemon] watcher stopped")
	if d.hb != nil {
		d.hb.Stop()
		log.Printf("[daemon] heartbeat stopped")
	}
	d.exec.Stop()
	log.Printf("[daemon] executor stopped")

	log.Printf("[daemon] shutdown complete")
	return nil
}

// runtimeRegisterRetryInterval is the retry interval after runtime registration
// failure.
const runtimeRegisterRetryInterval = 30 * time.Second

// ensureRuntimeComponents ensures the daemon has completed runtime registration
// and started all online components (Session Token exchange, SSE client,
// heartbeat). Each step is idempotent and safe to call repeatedly:
//   - already registered (runtimeID non-empty): skip registration
//   - already has a Session Token: skip exchange
//   - SSE client / heartbeat already created: skip starting
//
// Only runtime registration failure returns an error (triggering a retry);
// Session Token exchange or SSE startup failure only warns and continues,
// consistent with the original degraded semantics (falling back to API Token /
// REST polling).
//
// Returns:
//   - error: returned on runtime registration failure
func (d *Daemon) ensureRuntimeComponents() error {
	ctx := context.Background()

	// 1. Register a runtime with the server (idempotent: skip if already registered)
	if d.runtimeID == "" {
		provider := d.cfg.Agent.Provider
		if provider == "" {
			provider = "claude"
		}
		runtime, err := d.client.RegisterRuntime(ctx, d.cfg.Workspace.ID, d.cfg.Agent.ID, provider, AgentdVersion, d.publicKeyPEM)
		if err != nil {
			return fmt.Errorf("register runtime: %w", err)
		}
		d.runtimeID = runtime.ID
		d.localState.SetRuntimeRegistered(runtime.ID, d.cfg.Local.InstanceID)
		d.localEvents.PublishSnapshot("runtime.registered", d.localState.Snapshot())
		log.Printf("[daemon] registered as runtime %s", runtime.ID)
	}

	// 2. Exchange the API Token for a Session Token (on failure, only degrade to
	// API Token; do not block subsequent components)
	if d.client.SessionToken == "" {
		sessionToken, expiresAt, err := d.client.ExchangeToken(ctx, d.cfg.Server.APIToken)
		if err != nil {
			log.Printf("[daemon] WARNING: failed to exchange session token: %v", err)
			d.localState.SetLastError("session_token_exchange_failed", err)
			d.localEvents.PublishSnapshot("error.changed", d.localState.Snapshot())
			log.Printf("[daemon] Continuing with API token...")
		} else {
			d.client.SessionToken = sessionToken
			d.client.SessionExpiry = expiresAt
			log.Printf("[daemon] session token obtained, expires at %v", expiresAt)

			// Start the session token refresher
			d.client.StartSessionTokenRefresher(d.stopCh)
		}
	}

	// 3. Start the SSE client (the primary event source, idempotent)
	if d.sseClient == nil {
		d.sseClient = NewSSEClientWithCallbacks(
			d.cfg.Server.URL,
			d.cfg.Workspace.ID,
			d.runtimeID,
			d.client.authToken,
			d.handleSSEEvent,
			SSECallbacks{
				OnConnected: func(lastEventID string) {
					d.localState.SetSSEConnected(lastEventID)
					d.localEvents.PublishSnapshot("sse.connected", d.localState.Snapshot())
				},
				OnDisconnected: func(err error) {
					d.localState.SetSSEDisconnected(err)
					d.localEvents.PublishSnapshot("sse.disconnected", d.localState.Snapshot())
				},
			},
		)
		if err := d.sseClient.Start(); err != nil {
			log.Printf("[daemon] WARNING: failed to start SSE client: %v", err)
		} else {
			log.Printf("[daemon] SSE client started")
		}
	}

	// 4. Start the heartbeat (30 seconds, idempotent)
	if d.hb == nil {
		d.hb = NewHeartbeatWithCallbacks(d.client, d.cfg.Workspace.ID, d.runtimeID, 30*time.Second, HeartbeatCallbacks{
			OnSuccess: func(at time.Time) {
				d.localState.SetHeartbeatSuccess(at)
				d.localEvents.PublishSnapshot("runtime.heartbeat", d.localState.Snapshot())
			},
			OnError: func(err error) {
				d.localState.SetHeartbeatError(err)
				d.localEvents.PublishSnapshot("runtime.error", d.localState.Snapshot())
			},
		})
		d.hb.Start()
		log.Printf("[daemon] heartbeat started (30s interval)")
	}

	return nil
}

// retryRuntimeRegistration periodically retries runtime registration until it
// succeeds or the daemon shuts down.
// Used for the scenario where the daemon is started while the Server is not yet
// ready: once the Server recovers, the daemon automatically fills in the
// registration, Session Token, SSE, and heartbeat without restarting (guaranteed
// idempotent by ensureRuntimeComponents).
func (d *Daemon) retryRuntimeRegistration() {
	ticker := time.NewTicker(runtimeRegisterRetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			if err := d.ensureRuntimeComponents(); err != nil {
				log.Printf("[daemon] WARNING: runtime registration retry failed: %v", err)
				continue
			}
			log.Printf("[daemon] runtime registered after retry, online components started")
			return
		}
	}
}

// handleSSEEvent handles SSE events received from the Server.
//
// Supported event types:
//   - node:pending: a node pending claim, triggers polling
//   - node:continuation_invite: a continuation-right invitation, auto-claimed and executed
//   - task:interrupt: a task interrupt request, stops the current execution
//   - sync:required: the buffer is stale, perform a full sync
//   - mention:trigger: an @mention, triggers polling
//   - node:timeout: a node timed out, interrupts the executor
//   - node:reject_rollback: a rejection rollback, performs a git reset
//
// Parameters:
//   - eventType: the event type string
//   - data: the event data (JSON format)
func (d *Daemon) handleSSEEvent(eventType string, data json.RawMessage) {
	log.Printf("[daemon] SSE event: %s", eventType)

	switch eventType {
	case "node:pending":
		// Trigger the watcher to check for claimable nodes
		log.Printf("[daemon] received node:pending, triggering watcher poll")
		go d.watcher.TriggerPoll()

	case "node:continuation_invite":
		// Auto-claim if configured
		log.Printf("[daemon] received node:continuation_invite")
		go d.handleContinuationInvite(data)

	case "task:interrupt":
		// Stop the current execution
		log.Printf("[daemon] received task:interrupt")
		go d.handleInterrupt(data)

	case "sync:required":
		// Perform a full REST sync
		log.Printf("[daemon] received sync:required, triggering full sync")
		go d.watcher.TriggerPoll()

	case "mention:trigger":
		// The agent was mentioned in a comment
		log.Printf("[daemon] received mention:trigger: %s", string(data))
		go d.handleMentionTrigger(data)

	case "node:timeout":
		// Timeout event — interrupt the running executor to stop the tool process
		log.Printf("[daemon] received node:timeout, interrupting executor")
		go d.handleNodeTimeout(data)

	case "node:reject_rollback":
		// Rejection rollback: perform a git reset --hard to the target node's baseline
		log.Printf("[daemon] received node:reject_rollback")
		go d.handleRejectRollback(data)

	default:
		log.Printf("[daemon] unhandled SSE event type: %s", eventType)
	}
}

// handleNodeTimeout handles the node timeout event.
//
// When a node's execution time exceeds timeout_minutes, the Server sends this
// event.
// The daemon needs to interrupt the current executor and set the node status to
// manual_intervention.
//
// Parameters:
//   - data: JSON-format event data, containing task_id and node_id
func (d *Daemon) handleNodeTimeout(data json.RawMessage) {
	var payload struct {
		TaskID int32  `json:"task_id"`
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[daemon] failed to parse node:timeout: %v", err)
		return
	}

	log.Printf("[daemon] handling node:timeout for task %d node %s", payload.TaskID, payload.NodeID)
	if err := d.exec.Interrupt(payload.TaskID, payload.NodeID); err != nil {
		log.Printf("[daemon] failed to interrupt executor on timeout: %v", err)
	}
}

// handleContinuationInvite handles the continuation invitation event.
func (d *Daemon) handleContinuationInvite(data json.RawMessage) {
	var payload struct {
		TaskID    int32  `json:"task_id"`
		NodeID    string `json:"node_id"`
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[daemon] failed to parse continuation_invite: %v", err)
		return
	}

	log.Printf("[daemon] auto-claiming continuation node %s for task %d", payload.NodeID, payload.TaskID)
	claimed, err := d.client.ClaimNode(context.Background(), d.cfg.Agent.ID, payload.TaskID, payload.NodeID)
	if err != nil {
		log.Printf("[daemon] failed to claim continuation node: %v", err)
		return
	}

	log.Printf("[daemon] claimed continuation node %s (%s) for task %d", claimed.ID, claimed.Name, payload.TaskID)
	go d.exec.Execute(payload.TaskID, *claimed, payload.ProjectID)
}

// handleMentionTrigger handles the mention:trigger event.
func (d *Daemon) handleMentionTrigger(data json.RawMessage) {
	var payload struct {
		TaskID    int32  `json:"task_id"`
		CommentID string `json:"comment_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[daemon] failed to parse mention:trigger: %v", err)
		return
	}

	log.Printf("[daemon] handling mention in task %d, comment %s — triggering watcher poll", payload.TaskID, payload.CommentID)
	go d.watcher.TriggerPoll()
}

// handleInterrupt handles the task:interrupt event.
func (d *Daemon) handleInterrupt(data json.RawMessage) {
	var payload struct {
		TaskID    int32  `json:"task_id"`
		NodeOrder int    `json:"node_order"`
		NodeID    string `json:"node_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[daemon] failed to parse task:interrupt: %v", err)
		return
	}

	// If node_id is not in the payload, try to get it from the current task
	nodeID := payload.NodeID
	if nodeID == "" {
		if taskID, node, ok := d.exec.CurrentTask(); ok && taskID == payload.TaskID {
			nodeID = node.ID
		}
	}

	if err := d.exec.Interrupt(payload.TaskID, nodeID); err != nil {
		log.Printf("[daemon] failed to interrupt task %d: %v", payload.TaskID, err)
	}
}

// handleRejectRollback handles the node:reject_rollback event.
// It performs a git reset --hard to the target node's code baseline tag.
func (d *Daemon) handleRejectRollback(data json.RawMessage) {
	var payload struct {
		TaskID        int32  `json:"task_id"`
		TargetNodeID  string `json:"target_node_id"`
		TargetOrder   int32  `json:"target_order"`
		RejectedNode  string `json:"rejected_node"`
		ProjectID     string `json:"project_id"`
		TargetAttempt int    `json:"target_attempt"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[daemon] failed to parse node:reject_rollback: %v", err)
		return
	}

	// First try to get the git manager from the current execution
	gitMgr := d.exec.GetGitManager()
	taskID, _, ok := d.exec.CurrentTask()

	if ok && taskID == payload.TaskID && gitMgr != nil {
		// Currently executing this task — use the active git manager
		log.Printf("[daemon] performing git rollback for active task %d to node order %d", payload.TaskID, payload.TargetOrder)
	} else {
		// Not currently executing — try to locate the isolated workdir
		log.Printf("[daemon] not currently executing task %d, attempting to locate isolated workdir for rollback", payload.TaskID)

		if payload.ProjectID == "" {
			log.Printf("[daemon] cannot locate workdir without project_id for rollback")
			return
		}

		workDir := filepath.Join(d.cfg.Workspace.Root, d.cfg.Agent.ID, d.cfg.Workspace.ID, payload.ProjectID, fmt.Sprintf("%d", payload.TaskID))
		candidateGit := NewGitManager(workDir)
		if !candidateGit.IsGitRepo() {
			log.Printf("[daemon] no git repo found at %s for rollback", workDir)
			return
		}
		gitMgr = candidateGit
		log.Printf("[daemon] located git repo at %s for rollback", workDir)
	}

	// Create a snapshot before rollback
	if _, err := gitMgr.SnapshotBeforeReject(payload.TaskID); err != nil {
		log.Printf("[daemon] WARNING: failed to create pre-rollback snapshot: %v", err)
	}

	// Reset to the target node's start tag
	attempt := payload.TargetAttempt
	if attempt <= 0 {
		attempt = 1
	}
	if err := gitMgr.ResetToNode(payload.TaskID, int(payload.TargetOrder), attempt); err != nil {
		log.Printf("[daemon] ERROR: failed to git reset to node order %d: %v", payload.TargetOrder, err)

		// Fallback: try previous available tags
		if fallbackErr := d.fallbackRollback(gitMgr, payload.TaskID, int(payload.TargetOrder)); fallbackErr != nil {
			log.Printf("[daemon] ERROR: fallback rollback also failed: %v", fallbackErr)
			d.client.ManualIntervention(context.Background(), d.cfg.Agent.ID, payload.TaskID, payload.TargetNodeID,
				fmt.Sprintf("Git rollback failed: %v", err))
			return
		}
	}

	log.Printf("[daemon] successfully rolled back task %d to node order %d baseline", payload.TaskID, payload.TargetOrder)

	// After rollback, trigger the watcher to take over the target node
	go d.watcher.TriggerPoll()
}

// fallbackRollback, when the normal rollback fails, walks backward level by
// level looking for an available start tag to roll back to.
//
// Starting from targetOrder-1, it walks backward through each node order; for
// each node it tries up to 5 attempts, and after finding the first existing
// start tag it performs a git reset to that position.
//
// Parameters:
//   - gitMgr: the Git manager instance
//   - taskID: task ID
//   - targetOrder: the order number of the target node
//
// Returns:
//   - nil on success; an error if no tag exists
func (d *Daemon) fallbackRollback(gitMgr *GitManager, taskID int32, targetOrder int) error {
	for order := targetOrder - 1; order >= 1; order-- {
		for attempt := 1; attempt <= 5; attempt++ {
			tag := NodeStartTag(taskID, order, attempt)
			if gitMgr.tagExists(tag) {
				log.Printf("[daemon] fallback: using tag %s instead", tag)
				return gitMgr.ResetToNode(taskID, order, attempt)
			}
		}
	}
	return fmt.Errorf("no fallback tag found")
}
