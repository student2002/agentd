// sse_client.go 实现 SSE（Server-Sent Events）客户端，用于接收 Server 的实时事件推送。
//
// 本文件提供 Agent Daemon 与 Server 之间的长连接事件通道，主要包括：
//   - SSEClient 结构体：封装 SSE 连接管理，支持断线重连和指数退避
//   - Start / Stop：启动和停止 SSE 连接，优雅取消进行中的 HTTP 请求
//   - run：主循环，管理连接生命周期和自动重连
//   - connect：建立 SSE 连接，设置 Last-Event-ID 用于事件回放
//   - readStream：解析 SSE 事件流，提取 id、event、data 字段
//   - dispatchEvent：将解析后的事件分发给注册的回调函数
//
// 支持的 SSE 事件类型：node:pending、node:continuation_invite、task:interrupt 等。
// 断线重连时通过 Last-Event-ID 头实现事件回放补偿。
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SSEClient 连接到服务器的 SSE 端点，接收实时事件推送，支持断线重连和事件回放。
type SSEClient struct {
	serverURL   string
	workspaceID string
	runtimeID   string
	tokenFn     func() string // 回调函数，返回最新的认证令牌
	onEvent     func(eventType string, data json.RawMessage)
	callbacks   SSECallbacks
	stopCh      chan struct{}
	wg          sync.WaitGroup

	mu             sync.Mutex
	connected      bool
	lastEventID    string
	backoffSeconds int

	cancelFn context.CancelFunc // 取消当前 SSE HTTP 请求的上下文
	cancelMu sync.Mutex
}

type SSECallbacks struct {
	OnConnected    func(lastEventID string)
	OnDisconnected func(error)
}

// NewSSEClient 创建一个新的 SSE 客户端。
// tokenFn 是回调函数，返回当前最佳的认证令牌（会话令牌或 API 令牌）。
func NewSSEClient(serverURL, workspaceID, runtimeID string, tokenFn func() string, onEvent func(string, json.RawMessage)) *SSEClient {
	return NewSSEClientWithCallbacks(serverURL, workspaceID, runtimeID, tokenFn, onEvent, SSECallbacks{})
}

func NewSSEClientWithCallbacks(serverURL, workspaceID, runtimeID string, tokenFn func() string, onEvent func(string, json.RawMessage), callbacks SSECallbacks) *SSEClient {
	return &SSEClient{
		serverURL:      serverURL,
		workspaceID:    workspaceID,
		runtimeID:      runtimeID,
		tokenFn:        tokenFn,
		onEvent:        onEvent,
		callbacks:      callbacks,
		stopCh:         make(chan struct{}),
		backoffSeconds: 1,
	}
}

// Start 连接到 SSE 端点并开始接收事件。
func (c *SSEClient) Start() error {
	c.wg.Add(1)
	go c.run()
	return nil
}

// Stop 断开 SSE 客户端连接并取消正在进行的 HTTP 请求。
func (c *SSEClient) Stop() {
	close(c.stopCh)
	// 取消所有进行中的 HTTP 请求，使 readStream 解除阻塞
	c.cancelMu.Lock()
	if c.cancelFn != nil {
		c.cancelFn()
	}
	c.cancelMu.Unlock()
	c.wg.Wait()
}

// IsConnected 返回 SSE 客户端当前是否已连接。
func (c *SSEClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// setConnected 设置 SSE 客户端的连接状态标志。
//
// 参数:
//   - v: 连接状态，true 表示已连接，false 表示已断开
func (c *SSEClient) setConnected(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = v
}

// run 是 SSE 客户端的主循环协程，管理连接生命周期和自动重连。
// 连接失败时使用指数退避策略重试，最大退避间隔 30 秒。
// 收到停止信号时优雅退出。
func (c *SSEClient) run() {
	defer c.wg.Done()

	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		err := c.connect()
		if err != nil {
			log.Printf("[sse] connection error: %v", err)
		}

		c.setConnected(false)
		if c.callbacks.OnDisconnected != nil {
			c.callbacks.OnDisconnected(err)
		}

		// 检查是否应在重连前停止
		select {
		case <-c.stopCh:
			return
		default:
		}

		// 指数退避
		wait := time.Duration(c.backoffSeconds) * time.Second
		log.Printf("[sse] reconnecting in %v...", wait)

		select {
		case <-time.After(wait):
		case <-c.stopCh:
			return
		}

		c.mu.Lock()
		c.backoffSeconds *= 2
		if c.backoffSeconds > 30 {
			c.backoffSeconds = 30
		}
		c.mu.Unlock()
	}
}

// connect 建立到 Server SSE 端点的 HTTP 长连接。
// 设置 Last-Event-ID 头用于断线重连时的事件回放补偿。
// 连接成功后重置退避计数器并进入事件流读取循环。
//
// 返回:
//   - error: 连接失败或事件流读取异常时返回错误
func (c *SSEClient) connect() error {
	url := fmt.Sprintf("%s/api/workspaces/%s/runtimes/%s/events", c.serverURL, c.workspaceID, c.runtimeID)

	// 创建可取消的上下文，使 Stop() 能中止 HTTP 请求
	ctx, cancel := context.WithCancel(context.Background())
	c.cancelMu.Lock()
	c.cancelFn = cancel
	c.cancelMu.Unlock()
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-API-Key", c.tokenFn())

	// 发送 Last-Event-ID 以在重连时进行事件回放
	c.mu.Lock()
	if c.lastEventID != "" {
		req.Header.Set("Last-Event-ID", c.lastEventID)
	}
	c.mu.Unlock()

	// 为 SSE 长连接使用无超时的客户端
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	c.setConnected(true)
	if c.callbacks.OnConnected != nil {
		c.callbacks.OnConnected(c.LastEventID())
	}
	// 连接成功后重置退避
	c.mu.Lock()
	c.backoffSeconds = 1
	c.mu.Unlock()

	log.Printf("[sse] connected to %s", url)

	return c.readStream(resp)
}

func (c *SSEClient) LastEventID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastEventID
}

// readStream 逐行解析 SSE 事件流，提取 id、event、data 字段。
// 遵循 SSE 规范：空行分隔事件，冒号开头的行为注释，data: 后的首空格自动去除。
// 解析完成后调用 dispatchEvent 分发事件。
//
// 参数:
//   - resp: SSE HTTP 响应对象
//
// 返回:
//   - error: 扫描器读取异常时返回错误
func (c *SSEClient) readStream(resp *http.Response) error {
	scanner := bufio.NewScanner(resp.Body)
	var eventType string
	var dataBuilder strings.Builder

	for scanner.Scan() {
		select {
		case <-c.stopCh:
			return nil
		default:
		}

		line := scanner.Text()

		if line == "" {
			// 空行 = 事件结束
			if dataBuilder.Len() > 0 {
				c.dispatchEvent(eventType, dataBuilder.String())
				dataBuilder.Reset()
				eventType = ""
			}
			continue
		}

		if strings.HasPrefix(line, "id:") {
			id := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			c.mu.Lock()
			c.lastEventID = id
			c.mu.Unlock()
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		if strings.HasPrefix(line, "data:") {
			dataLine := strings.TrimPrefix(line, "data:")
			// SSE 规范："data:" 后的单个空格（如有）会被移除
			if len(dataLine) > 0 && dataLine[0] == ' ' {
				dataLine = dataLine[1:]
			}
			if dataBuilder.Len() > 0 {
				dataBuilder.WriteString("\n")
			}
			dataBuilder.WriteString(dataLine)
			continue
		}

		// 以 ':' 开头的行是注释，忽略
	}

	return scanner.Err()
}

// dispatchEvent 将解析后的 SSE 事件分发给注册的回调函数。
// 如果事件类型为空，默认设置为 "message"。
//
// 参数:
//   - eventType: 事件类型（如 "node:pending"、"task:interrupt"）
//   - data: 事件数据的原始 JSON 字符串
func (c *SSEClient) dispatchEvent(eventType, data string) {
	if eventType == "" {
		eventType = "message"
	}

	raw := json.RawMessage(data)
	if c.onEvent != nil {
		c.onEvent(eventType, raw)
	}
}
