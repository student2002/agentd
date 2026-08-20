// watcher.go 实现节点轮询监听器，在 SSE 断连时作为降级方案主动发现可认领节点。
//
// 本文件提供 Agent Daemon 的被动节点发现机制，主要包括：
//   - NodeWatcher 结构体：定期轮询工作区中所有项目的 pending 节点
//   - Start / Stop：启动和停止轮询协程，支持优雅退出
//   - TriggerPoll：立即触发一次轮询，用于 SSE 事件驱动的即时响应
//   - poll：遍历工作区所有项目，逐个检查可认领节点
//   - pollProject：检查单个项目的 pending 节点，尝试认领并分派给执行器
//
// 轮询间隔默认 60 秒。单次只执行一个节点（executor.IsRunning() 互斥检查），
// 认领使用乐观锁（version 字段），并发认领返回 409 Conflict。
package agent

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// NodeWatcher 轮询工作区中所有项目的可用节点，并分派给执行器执行。
// 在 SSE 断连时作为降级方案，主动轮询可认领的 pending 节点。
type NodeWatcher struct {
	client      *Client
	executor    *TaskExecutor
	agentID     string
	workspaceID string
	interval    time.Duration
	pollCh      chan struct{} // 用于触发立即轮询
	wg          sync.WaitGroup
	paused      atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
}

// NewNodeWatcher 创建一个新的节点监听器。
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

// Start 启动节点轮询循环。
// 初始轮询不会在此触发——调用方（Daemon）应在启动监听器后调用 TriggerPoll()。
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

// TriggerPoll 立即触发一次可认领节点的轮询，用于 SSE 事件处理器收到 node:pending 事件时。
func (w *NodeWatcher) TriggerPoll() {
	select {
	case w.pollCh <- struct{}{}:
	default:
		// 已有轮询在进行，跳过
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

// Stop 取消监听器上下文并等待协程退出。
func (w *NodeWatcher) Stop() {
	w.cancel()
	w.wg.Wait()
}

// poll 先恢复 Agent 之前认领但未完成的 in_progress 节点，
// 然后遍历工作区中所有项目，逐个检查可认领的 pending 节点。
// 在收到停止信号或上下文取消时提前退出。
func (w *NodeWatcher) poll() {
	// Paused mode: do not recover or claim any nodes. An in-progress execution
	// is untouched — pause only gates new work discovery.
	if w.paused.Load() {
		return
	}

	// 优先恢复之前未完成的节点（Agent 重启场景）
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

// recoverInProgressNodes 查询当前 Agent 之前认领但未完成（in_progress）的节点，
// 如果执行器空闲则恢复执行第一个此类节点。
// 用于 Agent 重启后自动恢复被中断的任务。
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

	// 恢复第一个 in_progress 节点
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
		ReadonlyDirs:    node.ReadonlyDirs,    // 恢复执行时保持目录权限
		FullControlDirs: node.FullControlDirs, // 恢复执行时保持目录权限
	}, node.ProjectID)
}

// pollProject 检查单个项目中的 pending 节点，尝试认领并分派给执行器执行。
// 工作流节点严格有序，仅认领第一个 pending 节点（sort_order 最小），
// 前序节点未完成时后序节点不可认领。仅处理非人类分配的节点。
// 单次只执行一个节点（executor.IsRunning() 互斥）。
//
// 参数:
//   - projectID: 要检查的项目 ID
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

		// 工作流有序：只认领第一个 pending 且非人类分配的节点
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
				// 不允许自审——跳过此节点，以便其他代理认领
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
