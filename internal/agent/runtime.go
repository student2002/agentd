// runtime.go implements one (connection, instance) runtime.
//
// AgentRuntime owns everything scoped to one materialized instance of one
// workspace connection: local state/event hub/log buffer, the single-flight
// executor, and the instance's single server identity (the agent UUID that
// workspace assigned). The identity carries the connection's client and its
// node watcher. Until the identity is bound the instance stays pending — no
// watcher, no claiming. An instance executes serially within its connection:
// IsRunning gates the single-flight executor; the same instance name on
// another connection is a different runtime and never queues behind this one.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/teammate/agentd/internal/agent/tool"
)

// runtimeIdentity is the instance's single server identity: the agent UUID the
// owning workspace assigned, the connection's client, and the node watcher
// scoped to that workspace.
type runtimeIdentity struct {
	agentUUID   string
	workspaceID string
	client      *Client
	watcher     *NodeWatcher
}

// connectionHealth tracks the owning connection's SSE/heartbeat state for the
// snapshot.
type connectionHealth struct {
	sseConnected  bool
	heartbeatOK   bool
	heartbeatErr  string
	daemonID      string
	lastHeartbeat time.Time
}

// AgentRuntime manages one materialized instance of one connection: its
// identity, executor and local control state.
type AgentRuntime struct {
	connName string
	name     string
	view     *Config

	identity *runtimeIdentity
	health   connectionHealth

	exec        *TaskExecutor
	localState  *LocalStateStore
	localEvents *LocalEventHub
	logBuffer   *LogBuffer

	mu       sync.Mutex
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewAgentRuntime creates a runtime for the named instance of one connection.
// view is the machine-level Config view (provider from the instance itself);
// the identity (and its watcher) is attached by BindIdentity when the server
// delivery arrives.
func NewAgentRuntime(view *Config, connName, name string) *AgentRuntime {
	localState := NewLocalStateStore(LocalStateConfig{
		ServerURL: view.Server.URL,
		AgentName: name,
		Provider:  view.Agent.Provider,
	})
	localEvents := NewLocalEventHub()
	logBuffer := NewLogBuffer(LogBufferDefaultCapacity)
	exec := NewTaskExecutorWithObserver(view, &localExecutionObserver{
		state: localState,
		hub:   localEvents,
	})
	exec.SetLogBuffer(logBuffer)
	r := &AgentRuntime{
		connName:    connName,
		name:        name,
		view:        view,
		exec:        exec,
		localState:  localState,
		localEvents: localEvents,
		logBuffer:   logBuffer,
		stopCh:      make(chan struct{}),
	}
	exec.SetResumeCallback(r.triggerPoll)
	return r
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

// Name returns the instance name (the local identity reported at heartbeat).
func (r *AgentRuntime) Name() string { return r.name }

// Provider returns the instance provider.
func (r *AgentRuntime) Provider() string { return r.view.Agent.Provider }

// PersonaKey returns the memory identity carried by this runtime's view —
// the persona injected into the execution's memory MCP env. A mismatch with
// the entry's persona means the runtime predates a persona overwrite and
// must be rebuilt.
func (r *AgentRuntime) PersonaKey() string { return r.view.Agent.PersonaKey }

// Busy reports whether this instance's executor is running a node.
func (r *AgentRuntime) Busy() bool { return r.exec.IsRunning() }

// State exposes the local snapshot store (local control API).
func (r *AgentRuntime) State() *LocalStateStore { return r.localState }

// Hub exposes the local event hub (local control API).
func (r *AgentRuntime) Hub() *LocalEventHub { return r.localEvents }

// Buffer exposes the execution log buffer (local control API).
func (r *AgentRuntime) Buffer() *LogBuffer { return r.logBuffer }

// Executor exposes the executor (local control API).
func (r *AgentRuntime) Executor() *TaskExecutor { return r.exec }

// SetToolFactory replaces the executor's coding-tool selector (supervisor
// ToolFactory option).
func (r *AgentRuntime) SetToolFactory(factory func() tool.Tool) {
	r.exec.SetToolFactory(factory)
}

// Watchers exposes the instance's node watcher (local control API); empty
// while the identity is not bound yet.
func (r *AgentRuntime) Watchers() []*NodeWatcher {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.identity == nil || r.identity.watcher == nil {
		return nil
	}
	return []*NodeWatcher{r.identity.watcher}
}

// Snapshot returns the local control snapshot.
func (r *AgentRuntime) Snapshot() LocalSnapshot { return r.localState.Snapshot() }

// BindIdentity records the agent UUID the owning workspace resolved for this
// instance and starts its watcher, which was blocked until now.
// Re-registration returning the same UUID is a no-op; a changed UUID (the
// instance was deleted web-side and re-created) rebuilds the watcher. Ignored
// after the runtime has been stopped.
func (r *AgentRuntime) BindIdentity(agentUUID string, client *Client, workspaceID string) {
	select {
	case <-r.stopCh:
		return
	default:
	}
	r.mu.Lock()
	if r.identity != nil && r.identity.agentUUID == agentUUID {
		r.mu.Unlock()
		return
	}
	if r.identity != nil && r.identity.watcher != nil {
		r.identity.watcher.Stop()
	}
	identity := &runtimeIdentity{
		agentUUID:   agentUUID,
		workspaceID: workspaceID,
		client:      client,
	}
	identity.watcher = NewNodeWatcher(client, r.exec, r.connName, agentUUID, workspaceID, 60*time.Second)
	r.identity = identity
	watcher := identity.watcher
	r.mu.Unlock()

	r.localState.SetAgentIdentity(workspaceID, agentUUID)
	r.localEvents.PublishSnapshot("agent.registered", r.localState.Snapshot())
	log.Printf("[agent=%s] connection %s resolved identity agent_id=%s workspace=%s", r.name, r.connName, agentUUID, workspaceID)

	watcher.Start()
	log.Printf("[agent=%s] node watcher started for connection %s (60s interval, fallback when SSE disconnected)", r.name, r.connName)
	go watcher.TriggerPoll()
}

// SetSSE mirrors the connection's SSE state into the snapshot.
func (r *AgentRuntime) SetSSE(connected bool, lastEventID string) {
	r.mu.Lock()
	r.health.sseConnected = connected
	r.mu.Unlock()
	if connected {
		r.localState.SetSSEConnected(lastEventID)
	} else {
		r.localState.SetSSEDisconnected(nil)
	}
}

// SetHeartbeat mirrors the connection's heartbeat result into the snapshot.
func (r *AgentRuntime) SetHeartbeat(err error) {
	r.mu.Lock()
	if err == nil {
		r.health.heartbeatOK = true
		r.health.heartbeatErr = ""
		r.health.lastHeartbeat = time.Now().UTC()
	} else {
		r.health.heartbeatOK = false
		r.health.heartbeatErr = err.Error()
	}
	r.mu.Unlock()
	if err == nil {
		r.localState.SetHeartbeatSuccess(time.Now().UTC())
	} else {
		r.localState.SetHeartbeatError(err)
	}
}

// HandleEvent handles one daemon-stream event routed to this (connection,
// instance) runtime through its owning connection. Events use the bound
// identity (claiming and reporting use its client/UUID) and are dropped while
// the identity is not bound.
//
// Supported event types:
//   - node:ready: a node became ready for claiming, triggers polling
//   - node:continuation_invite: a continuation-right invitation, auto-claimed and executed
//   - task:interrupt: a task interrupt request, stops the current execution
//   - sync:required: the buffer is stale, perform a full sync
//   - mention:trigger: an @mention, triggers polling
//   - node:timeout: a node timed out, interrupts the executor
func (r *AgentRuntime) HandleEvent(eventType string, data json.RawMessage) {
	identity := r.identitySnapshot()
	if identity == nil {
		log.Printf("[agent=%s] SSE event %s dropped: identity not resolved", r.name, eventType)
		return
	}
	log.Printf("[agent=%s] SSE event: %s (connection %s)", r.name, eventType, r.connName)

	switch eventType {
	case "node:ready":
		log.Printf("[agent=%s] received node:ready, triggering watcher poll", r.name)
		go r.triggerPoll()

	case "node:continuation_invite":
		log.Printf("[agent=%s] received node:continuation_invite", r.name)
		go r.handleContinuationInvite(identity, data)

	case "task:interrupt":
		log.Printf("[agent=%s] received task:interrupt", r.name)
		go r.handleInterrupt(data)

	case "sync:required":
		log.Printf("[agent=%s] received sync:required, triggering full sync", r.name)
		go r.triggerPoll()

	case "mention:trigger":
		log.Printf("[agent=%s] received mention:trigger: %s", r.name, string(data))
		go r.handleMentionTrigger(data)

	case "node:timeout":
		log.Printf("[agent=%s] received node:timeout, interrupting executor", r.name)
		go r.handleNodeTimeout(data)

	default:
		log.Printf("[agent=%s] unhandled SSE event type: %s", r.name, eventType)
	}
}

// identitySnapshot copies the bound identity for use outside the lock.
func (r *AgentRuntime) identitySnapshot() *runtimeIdentity {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.identity == nil {
		return nil
	}
	copied := *r.identity
	return &copied
}

func (r *AgentRuntime) triggerPoll() {
	identity := r.identitySnapshot()
	if identity == nil || identity.watcher == nil {
		return
	}
	identity.watcher.TriggerPoll()
}

// StopWatchers stops the instance's watcher (no new claims) without touching
// an in-flight execution.
func (r *AgentRuntime) StopWatchers() {
	r.mu.Lock()
	identity := r.identity
	r.mu.Unlock()
	if identity == nil || identity.watcher == nil {
		return
	}
	identity.watcher.Stop()
	log.Printf("[agent=%s] watcher stopped", r.name)
}

// Stop tears the runtime down: stop the watcher, then stop the executor.
// The supervisor interrupts in-flight executions before calling Stop.
func (r *AgentRuntime) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
	r.StopWatchers()
	r.exec.Stop()
	log.Printf("[agent=%s] executor stopped", r.name)
}

// InterruptRunning interrupts the in-flight execution, if any, reporting the
// interrupt to the server.
func (r *AgentRuntime) InterruptRunning() {
	taskID, node, ok := r.exec.CurrentTask()
	if !ok {
		return
	}
	if err := r.exec.Interrupt(taskID, node.ID); err != nil {
		log.Printf("[agent=%s] failed to interrupt task %d: %v", r.name, taskID, err)
	}
}

// handleNodeTimeout handles the node timeout event.
//
// When a node's execution time exceeds timeout_minutes, the Server sends this
// event. The runtime interrupts the current executor so the node lands in
// manual_intervention.
func (r *AgentRuntime) handleNodeTimeout(data json.RawMessage) {
	var payload struct {
		TaskID int32  `json:"task_id"`
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[agent=%s] failed to parse node:timeout: %v", r.name, err)
		return
	}

	log.Printf("[agent=%s] handling node:timeout for task %d node %s", r.name, payload.TaskID, payload.NodeID)
	if err := r.exec.Interrupt(payload.TaskID, payload.NodeID); err != nil {
		log.Printf("[agent=%s] failed to interrupt executor on timeout: %v", r.name, err)
	}
}

// handleContinuationInvite handles the continuation invitation event on the
// bound identity.
func (r *AgentRuntime) handleContinuationInvite(identity *runtimeIdentity, data json.RawMessage) {
	var payload struct {
		TaskID    int32  `json:"task_id"`
		NodeID    string `json:"node_id"`
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[agent=%s] failed to parse continuation_invite: %v", r.name, err)
		return
	}

	log.Printf("[agent=%s] auto-claiming continuation node %s for task %d", r.name, payload.NodeID, payload.TaskID)
	claimed, err := identity.client.ClaimNode(context.Background(), identity.agentUUID, payload.TaskID, payload.NodeID)
	if err != nil {
		log.Printf("[agent=%s] failed to claim continuation node: %v", r.name, err)
		return
	}

	log.Printf("[agent=%s] claimed continuation node %s (%s) for task %d", r.name, claimed.ID, claimed.Name, payload.TaskID)
	go r.exec.Execute(RunContext{
		Client:      identity.client,
		ConnName:    r.connName,
		AgentID:     identity.agentUUID,
		WorkspaceID: identity.workspaceID,
		ProjectID:   payload.ProjectID,
		TaskID:      payload.TaskID,
		NodeID:      claimed.ID,
	}, *claimed)
}

// handleMentionTrigger handles the mention:trigger event.
func (r *AgentRuntime) handleMentionTrigger(data json.RawMessage) {
	var payload struct {
		TaskID    int32  `json:"task_id"`
		CommentID string `json:"comment_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[agent=%s] failed to parse mention:trigger: %v", r.name, err)
		return
	}

	log.Printf("[agent=%s] handling mention in task %d, comment %s — triggering watcher poll", r.name, payload.TaskID, payload.CommentID)
	r.triggerPoll()
}

// handleInterrupt handles the task:interrupt event.
func (r *AgentRuntime) handleInterrupt(data json.RawMessage) {
	var payload struct {
		TaskID    int32  `json:"task_id"`
		NodeOrder int32  `json:"node_order"`
		NodeID    string `json:"node_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[agent=%s] failed to parse task:interrupt: %v", r.name, err)
		return
	}

	// If node_id is not in the payload, try to get it from the current task
	nodeID := payload.NodeID
	if nodeID == "" {
		if taskID, node, ok := r.exec.CurrentTask(); ok && taskID == payload.TaskID {
			nodeID = node.ID
		}
	}

	if err := r.exec.Interrupt(payload.TaskID, nodeID); err != nil {
		log.Printf("[agent=%s] failed to interrupt task %d: %v", r.name, payload.TaskID, err)
	}
}

// workDirPath builds the isolated per-task working directory:
// {root}/{workspaceID}/{agentID}/{projectID}/{taskID}. The single source for
// the layout, shared by the executor and session-context rebinding.
func workDirPath(root, workspaceID, agentID, projectID string, taskID int32) string {
	if projectID == "" {
		projectID = "no-project"
	}
	return filepath.Join(root, workspaceID, agentID, projectID, fmt.Sprintf("%d", taskID))
}
