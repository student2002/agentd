// Package agent 提供 AI 代理守护进程的核心功能。
//
// 本包实现了 Agent Daemon 的完整生命周期，包括：
//   - 运行时注册和心跳维持
//   - SSE 事件监听和响应
//   - 节点认领和任务执行
//   - Git 操作和凭据管理
//   - 上下文构建和工具调用
//   - RSA 加密通信
//
// Daemon 是守护进程的主入口，协调所有子模块的工作。
// TaskExecutor 负责实际的任务执行流程。
// SSEClient 负责与 Server 的实时通信。
// Heartbeat 负责定期向 Server 报告存活状态。
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

// Daemon 管理代理守护进程的完整生命周期。
//
// Daemon 是守护进程的主协调器，负责：
//   - 生成 RSA 密钥对用于凭据解密
//   - 向 Server 注册运行时并获取 Session Token
//   - 启动 SSE 客户端监听事件
//   - 启动心跳维持在线状态
//   - 启动节点监听器轮询可认领任务
//   - 优雅处理关闭信号
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

// NewDaemon 创建一个新的代理守护进程实例。
//
// 参数：
//   - cfg: 守护进程配置，包含服务器地址、Agent ID、工作区 ID 等
//
// 返回：
//   - *Daemon: 初始化完成的守护进程实例
//   - error: 创建失败时返回错误
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

// Stop 通知守护进程优雅关闭。
//
// 调用此方法后，所有子模块会在完成当前操作后停止。
// 心跳停止、SSE 连接关闭、任务执行器中断。
func (d *Daemon) Stop() {
	d.stopOnce.Do(func() {
		close(d.stopCh)
	})
}

// Run 启动守护进程并阻塞直到收到关闭信号。
//
// 启动流程：
//  1. 生成 RSA 密钥对（用于 Git 凭据解密）
//  2. 向 Server 注册运行时，获取 runtimeID
//  3. 用 API Token 交换 Session Token（7 天有效期）
//  4. 启动 Session Token 自动刷新（过期前 5 分钟）
//  5. 启动 SSE 客户端，监听实时事件
//  6. 启动心跳（每 30 秒）
//  7. 启动节点监听器（每 60 秒轮询，作为 SSE 降级备份）
//  8. 触发初始全量同步
//  9. 等待 SIGINT/SIGTERM 信号
//
// 错误处理：
//   - RSA 密钥生成失败：继续运行，Git 凭据不可用
//   - 运行时注册失败：继续运行，使用 API Token
//   - Session Token 交换失败：继续使用 API Token
//   - SSE 启动失败：继续运行，依赖 REST 轮询
//
// 返回：
//   - error: 守护进程异常退出时返回错误
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

	// 0. 生成 RSA 密钥对用于凭据解密
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

	// 1. 向服务器注册运行时并启动全部在线组件（Session Token、SSE、心跳）。
	// 注册失败时不阻塞启动：由 retryRuntimeRegistration 每 30 秒重试，
	// 待 Server 恢复后自动补齐组件，无需重启守护进程。
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

	// 5. 启动 NodeWatcher 作为降级方案（60 秒 REST 轮询）
	// 监视器仅在 SSE 断开时主动轮询
	d.watcher = NewNodeWatcher(d.client, d.exec, d.cfg.Agent.ID, d.cfg.Workspace.ID, 60*time.Second)
	if d.localServer != nil {
		d.localServer.SetWatcher(d.watcher)
		d.localServer.SetExecutor(d.exec)
	}
	d.watcher.Start()
	log.Printf("[daemon] node watcher started (60s interval, fallback when SSE disconnected)")

	// 6. 在所有组件启动后触发一次立即的全量同步。
	// 这确保守护进程能发现运行时注册之前就已创建的 pending 节点
	// （例如用户在启动守护进程前创建了任务，或在启动期间错过了 SSE 事件）。
	go d.watcher.TriggerPoll()
	log.Printf("[daemon] triggered initial full sync")

	// 7. 等待关闭信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("[daemon] received signal %v, shutting down...", sig)
	case <-d.stopCh:
		log.Printf("[daemon] stop requested, shutting down...")
	}

	// 7. 优雅关闭
	// 先等待注册重试协程退出，避免它与关闭逻辑并发读写 SSE/心跳组件
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

// runtimeRegisterRetryInterval 是运行时注册失败后的重试间隔。
const runtimeRegisterRetryInterval = 30 * time.Second

// ensureRuntimeComponents 确保守护进程已完成运行时注册，并启动全部在线组件
// （Session Token 交换、SSE 客户端、心跳）。各步骤幂等，可安全重复调用：
//   - 已注册（runtimeID 非空）则跳过注册
//   - 已有 Session Token 则跳过交换
//   - 已创建 SSE 客户端 / 心跳则跳过启动
//
// 只有运行时注册失败会返回错误（触发重试）；Session Token 交换或 SSE 启动失败
// 仅告警并继续，与原有的降级语义一致（回退 API Token / REST 轮询）。
//
// 返回：
//   - error: 运行时注册失败时返回错误
func (d *Daemon) ensureRuntimeComponents() error {
	ctx := context.Background()

	// 1. 向服务器注册运行时（幂等：已注册则跳过）
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

	// 2. 将 API Token 交换为 Session Token（失败仅降级为 API Token，不阻塞后续组件）
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

			// 启动会话令牌刷新器
			d.client.StartSessionTokenRefresher(d.stopCh)
		}
	}

	// 3. 启动 SSE 客户端（主要事件源，幂等）
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

	// 4. 启动心跳（30 秒，幂等）
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

// retryRuntimeRegistration 周期性地重试运行时注册，直到成功或守护进程关闭。
// 用于 Server 尚未就绪时启动守护进程的场景：Server 恢复后守护进程无需重启，
// 即可自动补齐注册、Session Token、SSE 与心跳（由 ensureRuntimeComponents 幂等保证）。
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

// handleSSEEvent 处理从 Server 接收到的 SSE 事件。
//
// 支持的事件类型：
//   - node:pending: 有待认领的节点，触发轮询
//   - node:continuation_invite: 续约权邀请，自动认领并执行
//   - task:interrupt: 任务中断请求，停止当前执行
//   - sync:required: 缓冲过期，执行全量同步
//   - mention:trigger: 有 @提及，触发轮询
//   - node:timeout: 节点超时，中断执行器
//   - node:reject_rollback: 驳回回退，执行 git reset
//
// 参数：
//   - eventType: 事件类型字符串
//   - data: 事件数据（JSON 格式）
func (d *Daemon) handleSSEEvent(eventType string, data json.RawMessage) {
	log.Printf("[daemon] SSE event: %s", eventType)

	switch eventType {
	case "node:pending":
		// 触发监视器检查可认领的节点
		log.Printf("[daemon] received node:pending, triggering watcher poll")
		go d.watcher.TriggerPoll()

	case "node:continuation_invite":
		// 如果已配置则自动认领
		log.Printf("[daemon] received node:continuation_invite")
		go d.handleContinuationInvite(data)

	case "task:interrupt":
		// 停止当前执行
		log.Printf("[daemon] received task:interrupt")
		go d.handleInterrupt(data)

	case "sync:required":
		// 执行完整 REST 同步
		log.Printf("[daemon] received sync:required, triggering full sync")
		go d.watcher.TriggerPoll()

	case "mention:trigger":
		// 代理在评论中被提及
		log.Printf("[daemon] received mention:trigger: %s", string(data))
		go d.handleMentionTrigger(data)

	case "node:timeout":
		// 超时事件——中断正在运行的执行器以停止工具进程
		log.Printf("[daemon] received node:timeout, interrupting executor")
		go d.handleNodeTimeout(data)

	case "node:reject_rollback":
		// 驳回回退：执行 git reset --hard 到目标节点的基线
		log.Printf("[daemon] received node:reject_rollback")
		go d.handleRejectRollback(data)

	default:
		log.Printf("[daemon] unhandled SSE event type: %s", eventType)
	}
}

// handleNodeTimeout 处理节点超时事件。
//
// 当节点执行时间超过 timeout_minutes 时，Server 发送此事件。
// 守护进程需要中断当前执行器，并将节点状态设为 manual_intervention。
//
// 参数：
//   - data: JSON 格式的事件数据，包含 task_id 和 node_id
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

// handleContinuationInvite 处理续约邀请事件。
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

// handleMentionTrigger 处理 mention:trigger 事件。
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

// handleInterrupt 处理 task:interrupt 事件。
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

	// 如果 payload 中没有 node_id，尝试从当前任务获取
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

// handleRejectRollback 处理 node:reject_rollback 事件。
// 它执行 git reset --hard 到目标节点的代码基线标签。
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

	// 首先尝试从当前执行中获取 git 管理器
	gitMgr := d.exec.GetGitManager()
	taskID, _, ok := d.exec.CurrentTask()

	if ok && taskID == payload.TaskID && gitMgr != nil {
		// 当前正在执行此任务——使用活动的 git 管理器
		log.Printf("[daemon] performing git rollback for active task %d to node order %d", payload.TaskID, payload.TargetOrder)
	} else {
		// 当前未在执行——尝试定位隔离的工作目录
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

	// 回退前创建快照
	if _, err := gitMgr.SnapshotBeforeReject(payload.TaskID); err != nil {
		log.Printf("[daemon] WARNING: failed to create pre-rollback snapshot: %v", err)
	}

	// 重置到目标节点的开始标签
	attempt := payload.TargetAttempt
	if attempt <= 0 {
		attempt = 1
	}
	if err := gitMgr.ResetToNode(payload.TaskID, int(payload.TargetOrder), attempt); err != nil {
		log.Printf("[daemon] ERROR: failed to git reset to node order %d: %v", payload.TargetOrder, err)

		// 降级方案：尝试之前的可用标签
		if fallbackErr := d.fallbackRollback(gitMgr, payload.TaskID, int(payload.TargetOrder)); fallbackErr != nil {
			log.Printf("[daemon] ERROR: fallback rollback also failed: %v", fallbackErr)
			d.client.ManualIntervention(context.Background(), d.cfg.Agent.ID, payload.TaskID, payload.TargetNodeID,
				fmt.Sprintf("Git rollback failed: %v", err))
			return
		}
	}

	log.Printf("[daemon] successfully rolled back task %d to node order %d baseline", payload.TaskID, payload.TargetOrder)

	// 回退后，触发监视器接管目标节点
	go d.watcher.TriggerPoll()
}

// fallbackRollback 在正常回退失败时，逐级向前查找可用的 start tag 进行回退。
//
// 从 targetOrder-1 开始向前遍历每个节点顺序，对每个节点尝试最多 5 次 attempt，
// 找到第一个存在的 start tag 后执行 git reset 回退到该位置。
//
// 参数:
//   - gitMgr: Git 管理器实例
//   - taskID: 任务 ID
//   - targetOrder: 目标节点的顺序号
//
// 返回:
//   - 成功返回 nil，所有 tag 都不存在则返回错误
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
