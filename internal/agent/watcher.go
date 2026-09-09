// watcher.go implements the node polling listener, which acts as a fallback to
// actively discover claimable nodes when SSE is disconnected.
//
// This file provides the Agent Daemon's passive node discovery mechanism,
// mainly including:
//   - NodeWatcher struct: periodically polls pending nodes across all projects
//     in the workspace
//   - Start / Stop: start and stop the polling goroutine, supporting graceful
//     shutdown
//   - TriggerPoll: triggers a poll immediately, for instant response driven by
//     SSE events
//   - poll: iterates over all projects in the workspace, checking claimable
//     nodes one by one
//   - pollProject: checks a single project's pending nodes, attempts to claim
//     them and dispatches them to the executor
//
// The default polling interval is 60 seconds. Only one node is executed at a
// time (mutual exclusion via executor.IsRunning()). Claiming uses optimistic
// locking (the version field); concurrent claims return 409 Conflict.
package agent

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// NodeWatcher polls available nodes across all projects in the workspace and
// dispatches them to the executor for execution.
// It acts as a fallback when SSE is disconnected, actively polling claimable
// pending nodes.
type NodeWatcher struct {
	client      *Client
	executor    *TaskExecutor
	agentID     string
	workspaceID string
	interval    time.Duration
	pollCh      chan struct{} // used to trigger an immediate poll
	wg          sync.WaitGroup
	paused      atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
}

// NewNodeWatcher creates a new node watcher.
func NewNodeWatcher(client *Client, executor *TaskExecutor, agentID, workspaceID string, interval time.Duration) *NodeWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &NodeWatcher{
		client:      client,
		executor:    executor,
		agentID:     agentID,
		workspaceID: workspaceID,
		interval:    interval,
		pollCh:      make(chan struct{}, 1),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Start starts the node polling loop.
// The initial poll is NOT triggered here — the caller (Daemon) should call
// TriggerPoll() after starting the watcher.
func (w *NodeWatcher) Start() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				w.poll()
			case <-w.pollCh:
				w.poll()
			case <-w.ctx.Done():
				return
			}
		}
	}()
}

// TriggerPoll triggers an immediate poll for claimable nodes, used by the SSE
// event handler when it receives a node:pending event.
func (w *NodeWatcher) TriggerPoll() {
	select {
	case w.pollCh <- struct{}{}:
	default:
		// A poll is already in progress, skip
	}
}

// Pause stops the watcher from claiming new nodes. An in-progress execution
// is NOT affected — pause only gates the polling loop. Orthogonal to
// auto/intervene mode.
func (w *NodeWatcher) Pause() {
	w.paused.Store(true)
	log.Printf("[watcher] paused — will not claim new nodes until resumed")
}

// Resume re-enables polling for new nodes and triggers an immediate poll.
func (w *NodeWatcher) Resume() {
	w.paused.Store(false)
	log.Printf("[watcher] resumed — polling for new nodes")
	w.TriggerPoll()
}

// IsPaused reports whether the watcher is currently paused.
func (w *NodeWatcher) IsPaused() bool {
	return w.paused.Load()
}

// Stop cancels the watcher context and waits for the goroutine to exit.
func (w *NodeWatcher) Stop() {
	w.cancel()
	w.wg.Wait()
}

// poll first recovers in_progress nodes previously claimed but not completed by
// the Agent, then iterates over all projects in the workspace, checking
// claimable pending nodes one by one.
// It exits early on receiving a stop signal or context cancellation.
func (w *NodeWatcher) poll() {
	// Paused mode: do not recover or claim any nodes. An in-progress execution
	// is untouched — pause only gates new work discovery.
	if w.paused.Load() {
		return
	}

	// Recover previously unfinished nodes first (Agent restart scenario)
	w.recoverInProgressNodes()

	projects, err := w.client.ListProjects(w.ctx, w.workspaceID)
	if err != nil {
		if w.ctx.Err() != nil {
			return
		}
		log.Printf("[watcher] failed to list projects: %v", err)
		return
	}

	if len(projects) == 0 {
		return
	}

	for _, project := range projects {
		select {
		case <-w.ctx.Done():
			return
		default:
		}
		w.pollProject(project.ID)
	}
}

// recoverInProgressNodes queries the in_progress nodes previously claimed but
// not completed by the current Agent, and recovers the first such node if the
// executor is idle.
// Used to automatically resume interrupted tasks after an Agent restart.
func (w *NodeWatcher) recoverInProgressNodes() {
	if w.executor.IsRunning() {
		return
	}

	nodes, err := w.client.GetInProgressNodes(w.ctx, w.workspaceID, w.agentID)
	if err != nil {
		if w.ctx.Err() != nil {
			return
		}
		log.Printf("[watcher] failed to get in-progress nodes: %v", err)
		return
	}

	if len(nodes) == 0 {
		return
	}

	// Recover the first in_progress node
	node := nodes[0]
	if w.executor.IsRunning() {
		return
	}

	log.Printf("[watcher] recovering in-progress node %s (%s) for task %d", node.ID, node.Name, node.TaskID)
	go w.executor.Execute(node.TaskID, TaskNode{
		ID:              node.ID,
		TaskID:          node.TaskID,
		Name:            node.Name,
		SortOrder:       node.SortOrder,
		Status:          node.Status,
		ReadonlyDirs:    node.ReadonlyDirs,    // preserve directory permissions when resuming
		FullControlDirs: node.FullControlDirs, // preserve directory permissions when resuming
	}, node.ProjectID)
}

// pollProject checks the pending nodes in a single project, attempts to claim
// them and dispatches them to the executor for execution.
// Workflow nodes are strictly ordered; only the first pending node (smallest
// sort_order) is claimed, and subsequent nodes cannot be claimed until their
// predecessors are completed. Only non-human-assigned nodes are processed.
// Only one node is executed at a time (executor.IsRunning() mutex).
//
// Parameters:
//   - projectID: the project ID to check
func (w *NodeWatcher) pollProject(projectID string) {
	if w.executor.IsRunning() {
		return
	}

	tasks, err := w.client.ListPendingNodes(w.ctx, projectID)
	if err != nil {
		if w.ctx.Err() != nil {
			return
		}
		log.Printf("[watcher] failed to list pending nodes for project %s: %v", projectID, err)
		return
	}

	for _, task := range tasks {
		if w.executor.IsRunning() {
			break
		}

		select {
		case <-w.ctx.Done():
			return
		default:
		}

		nodes, err := w.client.ListTaskNodes(w.ctx, task.TaskID)
		if err != nil {
			if w.ctx.Err() != nil {
				return
			}
			log.Printf("[watcher] failed to list nodes for task %d: %v", task.TaskID, err)
			continue
		}

		// Workflow is ordered: only claim the first pending, non-human-assigned node
		var firstPending *TaskNode
		for i := range nodes {
			if nodes[i].Status == "pending" && nodes[i].AssigneeType != "human" {
				firstPending = &nodes[i]
				break
			}
		}
		if firstPending == nil {
			continue
		}

		if w.executor.IsRunning() {
			break
		}

		select {
		case <-w.ctx.Done():
			return
		default:
		}

		claimed, err := w.client.ClaimNode(w.ctx, w.agentID, task.TaskID, firstPending.ID)
		if err != nil {
			if w.ctx.Err() != nil {
				return
			}
			errMsg := err.Error()
			if strings.Contains(errMsg, "self-review") {
				// Self-review not allowed — skip this node so other agents can claim it
				if skipErr := w.client.SkipClaim(w.ctx, w.agentID, task.TaskID, firstPending.ID); skipErr != nil {
					log.Printf("[watcher] failed to skip-claim node %s: %v", firstPending.ID, skipErr)
				} else {
					log.Printf("[watcher] skipped self-review node %s", firstPending.ID)
				}
			} else {
				log.Printf("[watcher] failed to claim node %s: %v", firstPending.ID, err)
			}
			continue
		}

		log.Printf("[watcher] claimed node %s (%s) for task %d", claimed.ID, claimed.Name, task.TaskID)
		go w.executor.Execute(task.TaskID, *claimed, projectID)
	}
}

// PollForTest runs one poll cycle synchronously. Test-only seam.
func (w *NodeWatcher) PollForTest() {
	w.poll()
}
