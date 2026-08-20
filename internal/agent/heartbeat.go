// heartbeat.go 管理 Agent Daemon 的周期性心跳发送。
//
// 本文件负责维持代理运行时的在线状态，主要包括：
//   - Heartbeat 结构体：封装心跳循环的配置和生命周期管理
//   - Start / Stop：启动和停止心跳协程，支持优雅退出
//   - 心跳间隔：默认 30 秒，可配置
//
// 心跳通过 HTTP POST 发送到 Server 的 /api/workspaces/{id}/runtimes/{id}/heartbeat 端点。
// 心跳失败时仅记录日志，不中断守护进程运行。
package agent

import (
	"context"
	"log"
	"sync"
	"time"
)

// Heartbeat 管理周期性心跳发送，维持代理运行时的在线状态。
type Heartbeat struct {
	client      *Client
	workspaceID string
	runtimeID   string
	interval    time.Duration
	callbacks   HeartbeatCallbacks
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

type HeartbeatCallbacks struct {
	OnSuccess func(time.Time)
	OnError   func(error)
}

// NewHeartbeat 创建一个新的心跳管理器。
func NewHeartbeat(client *Client, workspaceID, runtimeID string, interval time.Duration) *Heartbeat {
	return NewHeartbeatWithCallbacks(client, workspaceID, runtimeID, interval, HeartbeatCallbacks{})
}

func NewHeartbeatWithCallbacks(client *Client, workspaceID, runtimeID string, interval time.Duration, callbacks HeartbeatCallbacks) *Heartbeat {
	return &Heartbeat{
		client:      client,
		workspaceID: workspaceID,
		runtimeID:   runtimeID,
		interval:    interval,
		callbacks:   callbacks,
		stopCh:      make(chan struct{}),
	}
}

// Start 启动心跳循环。
func (h *Heartbeat) Start() {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ticker := time.NewTicker(h.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := h.client.Heartbeat(context.Background(), h.workspaceID, h.runtimeID); err != nil {
					log.Printf("[heartbeat] failed: %v", err)
					if h.callbacks.OnError != nil {
						h.callbacks.OnError(err)
					}
					continue
				}
				if h.callbacks.OnSuccess != nil {
					h.callbacks.OnSuccess(time.Now().UTC())
				}
			case <-h.stopCh:
				return
			}
		}
	}()
}

// Stop 停止心跳循环并等待协程退出。
func (h *Heartbeat) Stop() {
	close(h.stopCh)
	h.wg.Wait()
}
